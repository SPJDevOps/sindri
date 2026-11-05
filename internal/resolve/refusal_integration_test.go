//go:build integration

package resolve_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
)

// countedUpstream is a stub upstream that counts the requests it receives.
type countedUpstream struct {
	url  string
	hits atomic.Int32
}

func (e *testEnv) countedUpstream(t *testing.T, handler http.HandlerFunc) *countedUpstream {
	t.Helper()
	u := &countedUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	u.url = srv.URL
	return u
}

func (e *testEnv) createProxyAt(t *testing.T, name string, protocol model.Protocol, url, credentialsRef string) {
	t.Helper()
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeProxy, Protocol: protocol,
		UpstreamURL: url, CredentialsRef: credentialsRef, TTLSeconds: 3600, NegativeTTL: 300,
	}); err != nil {
		t.Fatalf("create proxy %s: %v", name, err)
	}
}

func (e *testEnv) createVirtualOf(t *testing.T, name string, protocol model.Protocol, members ...string) {
	t.Helper()
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeVirtual, Protocol: protocol, Members: members,
	}); err != nil {
		t.Fatalf("create virtual %s: %v", name, err)
	}
}

func (e *testEnv) cacheRowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.Pool().QueryRow(t.Context(), `SELECT count(*) FROM remote_cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var refusalCredentials = remote.StaticCredentials{"cred": {"user", "pass"}}

// bearerChallenge answers 401 with a challenge naming this stub's token endpoint.
func bearerChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token",service="stub",scope="repository:library/alpine:pull"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// A refusal of Sindri's own credential is a failure to read the member, not
// evidence the name is absent. Moving on would let a later, public member
// answer for a name the first member exists to serve (dependency confusion).
func TestVirtual_RefusalFailsTheWalk(t *testing.T) {
	cases := []struct {
		name        string
		protocol    model.Protocol
		path        string
		credentials string
		refuse      http.HandlerFunc
	}{
		{
			name: "NPM", protocol: model.ProtocolNPM, path: "/npm/both/foo",
			refuse: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		},
		{
			name: "PyPI", protocol: model.ProtocolPyPI, path: "/pypi/both/simple/foo/",
			refuse: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		},
		{
			name: "OCIBasic", protocol: model.ProtocolOCI, path: "/v2/both/library/alpine/manifests/latest",
			refuse: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				w.WriteHeader(http.StatusUnauthorized)
			},
		},
		{
			name: "OCICredentialedTokenEndpoint", protocol: model.ProtocolOCI, path: "/v2/both/library/alpine/manifests/latest",
			credentials: "cred",
			refuse: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				bearerChallenge(w, r)
			},
		},
		{
			// A login that still gets a token but has lost access to the image
			// must not fall through to a later member. Docker Hub answers this
			// way for unknown names too, so the cost is documented in ADR 14.
			name: "OCICredentialedTokenThenUnauthorized", protocol: model.ProtocolOCI, path: "/v2/both/library/alpine/manifests/latest",
			credentials: "cred",
			refuse: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"token":"tok","expires_in":300}`))
					return
				}
				bearerChallenge(w, r)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, http.NotFound, withCredentials(refusalCredentials))
			first := e.countedUpstream(t, tc.refuse)
			second := e.countedUpstream(t, http.NotFound)
			e.createProxyAt(t, "first", tc.protocol, first.url, tc.credentials)
			e.createProxyAt(t, "second", tc.protocol, second.url, "")
			e.createVirtualOf(t, "both", tc.protocol, "first", "second")

			rec := e.do(t, http.MethodGet, tc.path, nil, "application/vnd.pypi.simple.v1+json")
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("GET %s = %d %s, want 502", tc.path, rec.Code, rec.Body.String())
			}
			if n := second.hits.Load(); n != 0 {
				t.Fatalf("the second member got %d requests after the first refused", n)
			}
			if n := e.cacheRowCount(t); n != 0 {
				t.Fatalf("a refusal wrote %d cache rows", n)
			}
		})
	}
}

// Docker Hub and quay.io answer 401 for a name they do not have even to a valid
// token, and ghcr.io answers 403 at the token endpoint to an anonymous caller
// asking for an unknown name. Those are misses, or a virtual such as
// [dockerhub, ghcr] could never reach its second member.
func TestVirtual_HiddenNameMovesOn(t *testing.T) {
	hub := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"token":"tok","expires_in":300}`))
			return
		}
		bearerChallenge(w, r)
	}
	cases := []struct {
		name        string
		credentials string
		hidden      http.HandlerFunc
	}{
		{name: "HubStyleTokenThenUnauthorized", hidden: hub},
		{
			name: "AnonymousForbiddenWithABearerToken",
			hidden: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"token":"tok","expires_in":300}`))
					return
				}
				if r.Header.Get("Authorization") == "" {
					bearerChallenge(w, r)
					return
				}
				w.WriteHeader(http.StatusForbidden)
			},
		},
		{
			name: "GhcrStyleAnonymousTokenForbidden",
			hidden: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				bearerChallenge(w, r)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &ociStub{}
			stub.set(newTestImage(t, []byte("layer"), nil))
			e := newEnv(t, http.NotFound, withCredentials(refusalCredentials))
			first := e.countedUpstream(t, tc.hidden)
			second := e.countedUpstream(t, stub.ServeHTTP)
			e.createProxyAt(t, "first", model.ProtocolOCI, first.url, tc.credentials)
			e.createProxyAt(t, "second", model.ProtocolOCI, second.url, "")
			e.createVirtualOf(t, "both", model.ProtocolOCI, "first", "second")

			rec := e.do(t, http.MethodGet, "/v2/both/library/alpine/manifests/latest", nil, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("manifest = %d %s, want 200 from the second member", rec.Code, rec.Body.String())
			}
			if second.hits.Load() == 0 {
				t.Fatal("the second member was never asked")
			}
			var negative int
			if err := e.db.Pool().QueryRow(t.Context(),
				`SELECT count(*) FROM remote_cache WHERE status = 'miss'`).Scan(&negative); err != nil {
				t.Fatal(err)
			}
			if negative != 0 {
				t.Fatalf("a hidden name wrote %d negative cache rows", negative)
			}
		})
	}
}

// An upstream that starts refusing after Sindri has a copy is treated like one
// that is down: the copy still answers, and the cache keeps its hit.
func TestProxy_RefusalServesAStaleCopy(t *testing.T) {
	t.Run("NPM", func(t *testing.T) {
		e := newNPMProxy(t, npmProxyConfig{ttl: 1, negativeTTL: 300})
		e.serveOneVersion(t, "/foo", "foo")
		e.mustGet(t, "foo")
		waitPastTTL()
		e.stub.setStatus(http.StatusForbidden)
		if body := e.mustGet(t, "foo"); !strings.Contains(string(body), "foo-1.0.0.tgz") {
			t.Fatalf("stale packument = %s", body)
		}
	})

	t.Run("PyPI", func(t *testing.T) {
		var refuse atomic.Bool
		e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
			if refuse.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"name":"cached-pkg","files":[{"filename":"cached_pkg-1.0.0-py3-none-any.whl","url":"/f/x","hashes":{"sha256":"` + shaHex(nil) + `"}}]}`))
		})
		e.createProxyAt(t, "stale", model.ProtocolPyPI, e.upstream.URL+"/simple", "")
		const path = "/pypi/stale/simple/cached-pkg/"
		if rec := e.do(t, http.MethodGet, path, nil, "application/vnd.pypi.simple.v1+json"); rec.Code != http.StatusOK {
			t.Fatalf("warm index = %d %s", rec.Code, rec.Body.String())
		}
		if _, err := e.db.Pool().Exec(t.Context(), `UPDATE remote_cache SET fetched_at = fetched_at - interval '2 hours'`); err != nil {
			t.Fatal(err)
		}
		refuse.Store(true)
		rec := e.do(t, http.MethodGet, path, nil, "application/vnd.pypi.simple.v1+json")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cached_pkg-1.0.0") {
			t.Fatalf("stale index = %d %s, want the cached copy", rec.Code, rec.Body.String())
		}
	})

	t.Run("OCI", func(t *testing.T) {
		stub := &ociStub{}
		stub.set(newTestImage(t, []byte("layer"), nil))
		var refuse atomic.Bool
		e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
			if refuse.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			stub.ServeHTTP(w, r)
		})
		e.createProxyAt(t, "hub", model.ProtocolOCI, e.upstream.URL, "")
		const path = "/v2/hub/library/alpine/manifests/latest"
		if rec := e.do(t, http.MethodGet, path, nil, ""); rec.Code != http.StatusOK {
			t.Fatalf("warm manifest = %d %s", rec.Code, rec.Body.String())
		}
		if _, err := e.db.Pool().Exec(t.Context(), `UPDATE remote_cache SET fetched_at = fetched_at - interval '2 hours'`); err != nil {
			t.Fatal(err)
		}
		refuse.Store(true)
		if rec := e.do(t, http.MethodGet, path, nil, ""); rec.Code != http.StatusOK {
			t.Fatalf("stale manifest = %d %s, want the cached copy", rec.Code, rec.Body.String())
		}
	})

	t.Run("OCICredentialedTokenThenUnauthorized", func(t *testing.T) {
		stub := &ociStub{}
		stub.set(newTestImage(t, []byte("layer"), nil))
		var refuse atomic.Bool
		e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/token":
				_, _ = w.Write([]byte(`{"token":"tok","expires_in":300}`))
			case refuse.Load() || r.Header.Get("Authorization") != "Bearer tok":
				bearerChallenge(w, r)
			default:
				stub.ServeHTTP(w, r)
			}
		}, withCredentials(refusalCredentials))
		e.createProxyAt(t, "hub", model.ProtocolOCI, e.upstream.URL, "cred")
		const path = "/v2/hub/library/alpine/manifests/latest"
		if rec := e.do(t, http.MethodGet, path, nil, ""); rec.Code != http.StatusOK {
			t.Fatalf("warm manifest = %d %s", rec.Code, rec.Body.String())
		}
		if _, err := e.db.Pool().Exec(t.Context(), `UPDATE remote_cache SET fetched_at = fetched_at - interval '2 hours'`); err != nil {
			t.Fatal(err)
		}
		refuse.Store(true)
		if rec := e.do(t, http.MethodGet, path, nil, ""); rec.Code != http.StatusOK {
			t.Fatalf("stale manifest = %d %s, want the cached copy", rec.Code, rec.Body.String())
		}
	})
}

// A token fetched with one proxy's login must not answer for a proxy on the same
// upstream and image that has no login: that would read another tenant's image.
func TestProxyOCI_ATokenIsNeverSharedAcrossCredentials(t *testing.T) {
	stub := &ociStub{}
	stub.set(newTestImage(t, []byte("layer"), nil))
	e := newEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if _, _, ok := r.BasicAuth(); ok {
				_, _ = w.Write([]byte(`{"token":"private","expires_in":300}`))
				return
			}
			_, _ = w.Write([]byte(`{"token":"public","expires_in":300}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer private" {
			bearerChallenge(w, r)
			return
		}
		stub.ServeHTTP(w, r)
	}, withCredentials(refusalCredentials))
	e.createProxyAt(t, "authed", model.ProtocolOCI, e.upstream.URL, "cred")
	e.createProxyAt(t, "anon", model.ProtocolOCI, e.upstream.URL, "")

	if rec := e.do(t, http.MethodGet, "/v2/authed/library/alpine/manifests/latest", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("credentialed pull = %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodGet, "/v2/anon/library/alpine/manifests/latest", nil, ""); rec.Code == http.StatusOK {
		t.Fatalf("the anonymous proxy was served with the credentialed proxy's token: %s", rec.Body.String())
	}
}

// One warning per refusal of Sindri's credential, naming the repository and the
// status and nothing from the request. A name an upstream merely hides is an
// ordinary miss and logs nothing.
func TestRefusal_IsLoggedOnceAndAHiddenNameIsNot(t *testing.T) {
	cases := []struct {
		name, path string
		protocol   model.Protocol
		handler    http.HandlerFunc
		credential string
		wantStatus int
		wantWarns  int
	}{
		{
			name: "NPMRefusal", path: "/npm/up/foo", protocol: model.ProtocolNPM, wantStatus: http.StatusBadGateway, wantWarns: 1,
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		},
		{
			name: "OCIBasicRefusal", path: "/v2/up/library/alpine/manifests/latest", protocol: model.ProtocolOCI, wantStatus: http.StatusBadGateway, wantWarns: 1,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Basic realm="r"`)
				w.WriteHeader(http.StatusUnauthorized)
			},
		},
		{
			name: "OCIHiddenName", path: "/v2/up/library/alpine/manifests/latest", protocol: model.ProtocolOCI, wantStatus: http.StatusNotFound, wantWarns: 0,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"token":"tok","expires_in":300}`))
					return
				}
				bearerChallenge(w, r)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &logBuffer{}
			e := newEnv(t, tc.handler, func(o *remote.Options) { o.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			e.createProxyAt(t, "up", tc.protocol, e.upstream.URL, tc.credential)
			if rec := e.do(t, http.MethodGet, tc.path, nil, ""); rec.Code != tc.wantStatus {
				t.Fatalf("GET %s = %d %s, want %d", tc.path, rec.Code, rec.Body.String(), tc.wantStatus)
			}
			got := logs.String()
			if n := strings.Count(got, "upstream refused"); n != tc.wantWarns {
				t.Fatalf("%d refusal warnings, want %d:\n%s", n, tc.wantWarns, got)
			}
			if tc.wantWarns > 0 {
				if !strings.Contains(got, "repository=up") || !strings.Contains(got, "status=") || strings.Contains(got, e.upstream.URL) {
					t.Fatalf("refusal log should carry repository and status, not the URL:\n%s", got)
				}
			}
			if strings.Count(got, "level=WARN") != tc.wantWarns {
				t.Fatalf("unexpected extra warnings:\n%s", got)
			}
		})
	}
}
