//go:build integration

package resolve_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// npmTarball is what npm pack would produce for name@version.
func npmTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for path, body := range map[string]string{
		"package/package.json": fmt.Sprintf(`{"name":%q,"version":%q,"main":"index.js"}`, name, version),
		"package/index.js":     "module.exports = 1\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// publishNPM publishes name@version through the adapter, the only way a hosted
// npm row gets the manifest its derived packument is built from.
func (e *testEnv) publishNPM(t *testing.T, repo, name, version string) []byte {
	t.Helper()
	tgz := npmTarball(t, name, version)
	body, err := json.Marshal(map[string]any{
		"_id": name, "name": name,
		"dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{
			"name": name, "version": version,
			"dist": map[string]string{"integrity": sri512(tgz), "shasum": shasum(tgz), "tarball": "http://client.guess/x.tgz"},
		}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream",
			"data":         base64.StdEncoding.EncodeToString(tgz),
			"length":       len(tgz),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, http.MethodPut, "/npm/"+repo+"/"+strings.ReplaceAll(name, "/", "%2f"), body, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish %s@%s to %s = %d %s", name, version, repo, rec.Code, rec.Body.String())
	}
	return tgz
}

func (e *testEnv) createHostedNPM(t *testing.T, name string) {
	t.Helper()
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeHosted, Protocol: model.ProtocolNPM,
	}); err != nil {
		t.Fatalf("create hosted %s: %v", name, err)
	}
}

func (e *testEnv) createVirtualNPM(t *testing.T, name string, members ...string) {
	t.Helper()
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeVirtual, Protocol: model.ProtocolNPM, Members: members,
	}); err != nil {
		t.Fatalf("create virtual %s: %v", name, err)
	}
}

// npmUpstream is a proxy repository with a stub upstream of its own, so a test can
// tell which member was asked.
type npmUpstream struct {
	*npmStub
	url string
}

func (e *testEnv) createProxyNPM(t *testing.T, name string, ttl int32) npmUpstream {
	t.Helper()
	stub := newNPMStub()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	if _, err := e.reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeProxy, Protocol: model.ProtocolNPM,
		UpstreamURL: srv.URL, TTLSeconds: ttl, NegativeTTL: 300,
	}); err != nil {
		t.Fatalf("create proxy %s: %v", name, err)
	}
	return npmUpstream{npmStub: stub, url: srv.URL}
}

// offer serves name at the versions given, each with a tarball that says which
// upstream it came from, and returns the bytes of the last version.
func (u npmUpstream) offer(t *testing.T, name string, marker string, versions ...string) []byte {
	t.Helper()
	base := name[strings.LastIndex(name, "/")+1:]
	dist := map[string]map[string]any{}
	var tgz []byte
	for _, v := range versions {
		tgz = []byte(marker + " tarball of " + name + "@" + v)
		// Under the name, since spellings of one name are separate packages upstream.
		uri := "/tarballs/" + strings.ReplaceAll(name, "/", "_") + "/" + base + "-" + v + ".tgz"
		u.setTarball(uri, tgz)
		dist[v] = map[string]any{"tarball": u.url + uri, "integrity": sri512(tgz)}
	}
	u.setDocument("/"+strings.ReplaceAll(name, "/", "%2f"), npmDoc(t, name, dist,
		map[string]string{"latest": versions[len(versions)-1]}))
	return tgz
}

func (e *testEnv) wantNPM(t *testing.T, path string, want int) []byte {
	t.Helper()
	rec := e.do(t, http.MethodGet, path, nil, "")
	if rec.Code != want {
		t.Fatalf("GET %s = %d %s, want %d", path, rec.Code, rec.Body.String(), want)
	}
	return rec.Body.Bytes()
}

// npmVersionsIn decodes a packument into its versions and each one's dist.tarball.
func npmVersionsIn(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var doc struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decoding packument %s: %v", body, err)
	}
	out := make(map[string]string, len(doc.Versions))
	for v, entry := range doc.Versions {
		out[v] = entry.Dist.Tarball
	}
	return out
}

// denyMember makes one repository unreadable for the rest of the test or subtest.
func (e *testEnv) denyMember(t *testing.T, denied string) {
	t.Helper()
	e.resolver.SetMemberAccess(func(_ context.Context, m model.Repository) bool { return m.Name != denied })
	t.Cleanup(func() { e.resolver.SetMemberAccess(nil) })
}

// TestDependencyConfusion_HostedNpmScopeIsNeverServedFromProxy is the permanent
// I5 regression guard for npm, where one published @acme/* package claims the whole
// scope. Upstream offers a hostile version of everything a hosted member claims,
// under every spelling a client can use. Resolution answers from the hosted member
// or fails with a 404, and never contacts upstream.
// ARCHITECTURE.md section 4 / invariant I5 / ADR 24.
func TestDependencyConfusion_HostedNpmScopeIsNeverServedFromProxy(t *testing.T) {
	e := newEnv(t, http.NotFound)
	const hosted, proxy = "internal", "npmjs"
	e.createHostedNPM(t, hosted)
	up := e.createProxyNPM(t, proxy, 3600)
	uiTarball := e.publishNPM(t, hosted, "@acme/ui", "1.0.0")
	e.publishNPM(t, hosted, "jsonstream", "1.0.0")
	e.publishNPM(t, hosted, "@kit/ui", "1.0.0")

	// Each is a name the proxy would happily serve if it were ever asked.
	for _, name := range []string{"@acme/ui", "@acme/new", "@ACME/new", "@ACME/ui", "JSONStream", "@Kit/new", "@Kit/ui"} {
		up.offer(t, name, "hostile", "99.0.0")
	}
	up.offer(t, "@public/lib", "public", "1.0.0")

	e.createVirtualNPM(t, "hosted-first", hosted, proxy)
	e.createVirtualNPM(t, "proxy-first", proxy, hosted)
	e.createVirtualNPM(t, "hosted-only", hosted)
	e.createVirtualNPM(t, "nested-hosted-last", proxy, "hosted-only")
	e.createVirtualNPM(t, "nested-whole", "proxy-first")
	virtuals := []string{"hosted-first", "proxy-first", "nested-hosted-last", "nested-whole"}

	// noContact fails the test if body makes any request of the upstream.
	noContact := func(t *testing.T, body func()) {
		t.Helper()
		before := up.count()
		body()
		if got := up.count(); got != before {
			t.Fatalf("dependency confusion contacted upstream: before=%d after=%d", before, got)
		}
	}

	refused := []struct {
		name     string
		spelling string
		file     string
	}{
		{"NewNameInScope", "@acme%2fnew", "new-99.0.0.tgz"},
		{"NewNameInScopeUpperEscape", "@acme%2Fnew", "new-99.0.0.tgz"},
		{"NewNameInScopeEscapedAt", "%40acme%2fnew", "new-99.0.0.tgz"},
		{"NewNameInScopePlainSlash", "@acme/new", "new-99.0.0.tgz"},
		{"NewNameInScopeEscapedAtPlainSlash", "%40acme/new", "new-99.0.0.tgz"},
		{"UppercaseScope", "@ACME/new", "new-99.0.0.tgz"},
		{"UppercaseScopeEscaped", "@ACME%2Fnew", "new-99.0.0.tgz"},
		{"UppercaseScopeOfAHostedName", "@ACME/ui", "ui-99.0.0.tgz"},
		{"LegacyCaseOfAHostedName", "JSONStream", "JSONStream-99.0.0.tgz"},
		{"KelvinSignInScope", "@%E2%84%AAit%2fnew", "new-99.0.0.tgz"},
		{"KelvinSignInScopePlainSlash", "@%E2%84%AAit/new", "new-99.0.0.tgz"},
		{"KelvinSignOfAHostedName", "@%E2%84%AAit/ui", "ui-99.0.0.tgz"},
	}

	for _, virtual := range virtuals {
		t.Run(virtual, func(t *testing.T) {
			t.Run("ExactNameListsOnlyHostedVersions", func(t *testing.T) {
				noContact(t, func() {
					versions := npmVersionsIn(t, e.wantNPM(t, "/npm/"+virtual+"/@acme%2fui", http.StatusOK))
					want := npmPublicURL + "/npm/" + virtual + "/@acme/ui/-/ui-1.0.0.tgz"
					if len(versions) != 1 || versions["1.0.0"] != want {
						t.Fatalf("versions = %v, want only 1.0.0 at %s", versions, want)
					}
					if got := e.wantNPM(t, "/npm/"+virtual+"/@acme/ui/-/ui-1.0.0.tgz", http.StatusOK); !bytes.Equal(got, uiTarball) {
						t.Fatalf("tarball = %q, want the hosted bytes", got)
					}
					// The version only upstream has fails loudly.
					e.wantNPM(t, "/npm/"+virtual+"/@acme/ui/-/ui-99.0.0.tgz", http.StatusNotFound)
					e.wantNPM(t, "/npm/"+virtual+"/-/package/@acme%2fui/dist-tags", http.StatusOK)
				})
			})
			for _, tc := range refused {
				t.Run(tc.name, func(t *testing.T) {
					noContact(t, func() {
						e.wantNPM(t, "/npm/"+virtual+"/"+tc.spelling, http.StatusNotFound)
						e.wantNPM(t, "/npm/"+virtual+"/"+tc.spelling+"/-/"+tc.file, http.StatusNotFound)
						e.wantNPM(t, "/npm/"+virtual+"/-/package/"+strings.ReplaceAll(tc.spelling, "/", "%2f")+"/dist-tags", http.StatusNotFound)
					})
				})
			}
			t.Run("OwnerTheCallerCannotReadDoesNotFallThrough", func(t *testing.T) {
				e.denyMember(t, hosted)
				noContact(t, func() {
					for _, path := range []string{
						"@acme%2fui", "@acme/ui/-/ui-1.0.0.tgz", "@acme/ui/-/ui-99.0.0.tgz",
						"@acme%2fnew", "@acme/new/-/new-99.0.0.tgz",
						"jsonstream", "JSONStream", "JSONStream/-/JSONStream-99.0.0.tgz",
					} {
						e.wantNPM(t, "/npm/"+virtual+"/"+path, http.StatusNotFound)
					}
				})
			})
		})
	}

	// The control that gives every "no contact" above its meaning: this fixture's
	// proxy does answer, for a name nobody hosted has claimed.
	t.Run("UnclaimedNameStillReachesTheProxy", func(t *testing.T) {
		for _, virtual := range virtuals {
			body := e.wantNPM(t, "/npm/"+virtual+"/@public%2flib", http.StatusOK)
			if v := npmVersionsIn(t, body); v["1.0.0"] != npmPublicURL+"/npm/"+virtual+"/@public/lib/-/lib-1.0.0.tgz" {
				t.Fatalf("through %s: versions = %v", virtual, v)
			}
		}
		if up.countOf("/@public%2flib") == 0 {
			t.Fatal("the proxy was never asked for an unclaimed name")
		}
	})

	// A claim outlives the name's last file. The owner removing their only release
	// must not let the attacker's version appear, for that name or for its scope.
	t.Run("AfterTheLastFileIsDeleted", func(t *testing.T) {
		e.publishNPM(t, hosted, "@gone/only", "1.0.0")
		e.publishNPM(t, hosted, "gone-lib", "1.0.0")
		for _, name := range []string{"@gone/only", "@gone/other", "gone-lib"} {
			up.offer(t, name, "hostile", "99.0.0")
		}
		for _, file := range []string{
			"packages/@gone%2fonly/versions/1.0.0/files/only-1.0.0.tgz",
			"packages/gone-lib/versions/1.0.0/files/gone-lib-1.0.0.tgz",
		} {
			if rec := e.do(t, http.MethodDelete, "/api/v1/repositories/"+hosted+"/"+file, nil, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE %s = %d %s", file, rec.Code, rec.Body.String())
			}
		}
		cases := []struct{ name, spelling, file string }{
			{"ExactScopedName", "@gone%2fonly", "only-99.0.0.tgz"},
			{"DeletedFile", "@gone%2fonly", "only-1.0.0.tgz"},
			{"AnotherNameInTheScope", "@gone%2fother", "other-99.0.0.tgz"},
			{"ExactUnscopedName", "gone-lib", "gone-lib-99.0.0.tgz"},
		}
		for _, virtual := range virtuals {
			for _, tc := range cases {
				t.Run(virtual+"/"+tc.name, func(t *testing.T) {
					noContact(t, func() {
						e.wantNPM(t, "/npm/"+virtual+"/"+tc.spelling, http.StatusNotFound)
						e.wantNPM(t, "/npm/"+virtual+"/"+tc.spelling+"/-/"+tc.file, http.StatusNotFound)
					})
				})
			}
		}
	})

	// Only a hosted member claims. A proxy caching a package writes the same rows a
	// publish does, and if those were asked, the first package a proxy cached in a
	// scope would hide the rest of that scope from every later member.
	t.Run("AProxyMembersCachedRowsMakeNoClaim", func(t *testing.T) {
		first := e.createProxyNPM(t, "cache-first", 3600)
		second := e.createProxyNPM(t, "cache-second", 3600)
		cached := first.offer(t, "@shared/one", "first", "1.0.0")
		other := second.offer(t, "@shared/two", "second", "1.0.0")
		e.createVirtualNPM(t, "two-proxies", "cache-first", "cache-second")

		if got := e.wantNPM(t, "/npm/two-proxies/@shared/one/-/one-1.0.0.tgz", http.StatusOK); !bytes.Equal(got, cached) {
			t.Fatalf("download = %q", got)
		}
		claimed, err := e.reg.PackageClaimed(t.Context(), "cache-first", "@shared/two")
		if err != nil {
			t.Fatal(err)
		}
		if !claimed {
			t.Fatal("the proxy's rows must be there for this test to mean anything: nothing was cached")
		}

		e.wantNPM(t, "/npm/two-proxies/@shared%2ftwo", http.StatusOK)
		if got := e.wantNPM(t, "/npm/two-proxies/@shared/two/-/two-1.0.0.tgz", http.StatusOK); !bytes.Equal(got, other) {
			t.Fatalf("download = %q, want the later member's", got)
		}
	})
}

// newTwoProxies is a virtual repository, "both", over proxies "first" and "second",
// and "nested", a virtual repository holding "both" with "second" after it.
func newTwoProxies(t *testing.T) (*testEnv, npmUpstream, npmUpstream) {
	t.Helper()
	e := newEnv(t, http.NotFound)
	first := e.createProxyNPM(t, "first", 3600)
	second := e.createProxyNPM(t, "second", 3600)
	e.createVirtualNPM(t, "both", "first", "second")
	e.createVirtualNPM(t, "first-only", "first")
	e.createVirtualNPM(t, "nested", "first-only", "second")
	return e, first, second
}

func TestVirtualNPM_FirstMemberWithThePackageSuppliesItInMemberOrder(t *testing.T) {
	e, first, second := newTwoProxies(t)
	e.createVirtualNPM(t, "reversed", "second", "first")
	fromFirst := first.offer(t, "foo", "first", "1.0.0")
	fromSecond := second.offer(t, "foo", "second", "1.0.0", "2.0.0")
	onlySecond := second.offer(t, "bar", "second", "1.0.0")

	for _, tc := range []struct {
		name, virtual string
		versions      int
		file          string
		want          []byte
	}{
		{"FirstMemberWins", "both", 1, "foo-1.0.0.tgz", fromFirst},
		{"NestedFirstMemberWins", "nested", 1, "foo-1.0.0.tgz", fromFirst},
		{"OrderIsTheConfiguredOne", "reversed", 2, "foo-2.0.0.tgz", fromSecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No merging: the document is one member's, whole.
			if v := npmVersionsIn(t, e.wantNPM(t, "/npm/"+tc.virtual+"/foo", http.StatusOK)); len(v) != tc.versions {
				t.Fatalf("versions = %v, want %d", v, tc.versions)
			}
			if got := e.wantNPM(t, "/npm/"+tc.virtual+"/foo/-/"+tc.file, http.StatusOK); !bytes.Equal(got, tc.want) {
				t.Fatalf("download = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("AMissMovesToTheNextMember", func(t *testing.T) {
		for _, virtual := range []string{"both", "nested"} {
			e.wantNPM(t, "/npm/"+virtual+"/bar", http.StatusOK)
			if got := e.wantNPM(t, "/npm/"+virtual+"/bar/-/bar-1.0.0.tgz", http.StatusOK); !bytes.Equal(got, onlySecond) {
				t.Fatalf("through %s: download = %q", virtual, got)
			}
		}
	})
	t.Run("NoMemberHasItIs404", func(t *testing.T) {
		e.wantNPM(t, "/npm/both/absent", http.StatusNotFound)
		e.wantNPM(t, "/npm/both/absent/-/absent-1.0.0.tgz", http.StatusNotFound)
		e.wantNPM(t, "/npm/both/-/package/absent/dist-tags", http.StatusNotFound)
	})
}

// A version must not come from a different proxy than the packument that listed it.
func TestVirtualNPM_TheFirstMemberWithThePackageOwnsItsTarballs(t *testing.T) {
	for _, virtual := range []string{"both", "nested"} {
		t.Run(virtual, func(t *testing.T) {
			e, first, second := newTwoProxies(t)
			first.offer(t, "foo", "first", "1.0.0")
			second.offer(t, "foo", "second", "1.0.0", "2.0.0")
			// Listed by the first member, and gone from its upstream's storage.
			first.setDocument("/listed", npmDoc(t, "listed", map[string]map[string]any{
				"1.0.0": {"tarball": first.url + "/tarballs/listed-1.0.0.tgz", "integrity": sri512([]byte("x"))},
			}, map[string]string{"latest": "1.0.0"}))
			second.offer(t, "listed", "second", "1.0.0")

			e.wantNPM(t, "/npm/"+virtual+"/foo/-/foo-2.0.0.tgz", http.StatusNotFound)
			e.wantNPM(t, "/npm/"+virtual+"/listed/-/listed-1.0.0.tgz", http.StatusNotFound)
			if n := second.count(); n != 0 {
				t.Fatalf("the second member got %d requests", n)
			}
		})
	}
}

func TestVirtualNPM_AnUpstreamFailureFailsTheWalk(t *testing.T) {
	cases := []struct {
		name    string
		virtual string
		arrange func(t *testing.T, first, second npmUpstream)
		path    string
		// wantSecondAsked is false when the walk must stop before the second member.
		wantSecondAsked bool
	}{
		{
			name: "FirstMemberPackument", virtual: "both", path: "foo",
			arrange: func(_ *testing.T, first, _ npmUpstream) { first.setStatus(http.StatusServiceUnavailable) },
		},
		{
			name: "FirstMemberTarballOfAnUnknownPackage", virtual: "both", path: "foo/-/foo-1.0.0.tgz",
			arrange: func(_ *testing.T, first, _ npmUpstream) { first.setStatus(http.StatusInternalServerError) },
		},
		{
			name: "NestedFirstMember", virtual: "nested", path: "foo",
			arrange: func(_ *testing.T, first, _ npmUpstream) { first.setStatus(http.StatusBadGateway) },
		},
		{
			name: "NestedFirstMemberTarball", virtual: "nested", path: "foo/-/foo-1.0.0.tgz",
			arrange: func(_ *testing.T, first, _ npmUpstream) { first.setStatus(http.StatusBadGateway) },
		},
		{
			name: "TarballFailingOnTheOwner", virtual: "both", path: "listed/-/listed-1.0.0.tgz",
			arrange: func(t *testing.T, first, second npmUpstream) {
				t.Helper()
				first.setDocument("/listed", npmDoc(t, "listed", map[string]map[string]any{
					"1.0.0": {"tarball": first.url + "/tarballs/listed-1.0.0.tgz", "integrity": sri512([]byte("x"))},
				}, map[string]string{"latest": "1.0.0"}))
				first.configure(func(s *npmStub) { s.tarballStatus = http.StatusServiceUnavailable })
				second.offer(t, "listed", "second", "1.0.0")
			},
		},
		{
			name: "LaterMemberAfterAMiss", virtual: "both", path: "absent", wantSecondAsked: true,
			arrange: func(_ *testing.T, _, second npmUpstream) { second.setStatus(http.StatusServiceUnavailable) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, first, second := newTwoProxies(t)
			second.offer(t, "foo", "second", "1.0.0")
			tc.arrange(t, first, second)

			e.wantNPM(t, "/npm/"+tc.virtual+"/"+tc.path, http.StatusBadGateway)
			if asked := second.count() != 0; asked != tc.wantSecondAsked {
				t.Fatalf("second member asked = %v, want %v", asked, tc.wantSecondAsked)
			}
		})
	}

	// A member that only refused or lacks the name says nothing about availability.
	t.Run("ARefusalIsAMiss", func(t *testing.T) {
		e, first, second := newTwoProxies(t)
		first.setStatus(http.StatusForbidden)
		second.offer(t, "foo", "second", "1.0.0")
		e.wantNPM(t, "/npm/both/foo", http.StatusOK)
	})

	// An outage serves what the member already has rather than changing the answer.
	t.Run("AStaleDocumentStillAnswersFromTheSameMember", func(t *testing.T) {
		e := newEnv(t, http.NotFound)
		first := e.createProxyNPM(t, "first", 1)
		second := e.createProxyNPM(t, "second", 3600)
		e.createVirtualNPM(t, "both", "first", "second")
		first.offer(t, "foo", "first", "1.0.0")
		second.offer(t, "foo", "second", "1.0.0", "2.0.0")
		e.wantNPM(t, "/npm/both/foo", http.StatusOK)

		waitPastTTL()
		first.setStatus(http.StatusServiceUnavailable)
		if v := npmVersionsIn(t, e.wantNPM(t, "/npm/both/foo", http.StatusOK)); len(v) != 1 {
			t.Fatalf("versions = %v, want the first member's one", v)
		}
		if n := second.count(); n != 0 {
			t.Fatalf("the second member got %d requests", n)
		}
	})

	// A claimed name never needs the proxy, so an outage there cannot touch it.
	t.Run("AnOutageDoesNotTouchAClaimedName", func(t *testing.T) {
		e := newEnv(t, http.NotFound)
		e.createHostedNPM(t, "internal")
		up := e.createProxyNPM(t, "npmjs", 3600)
		e.createVirtualNPM(t, "all", "npmjs", "internal")
		tgz := e.publishNPM(t, "internal", "@acme/ui", "1.0.0")
		up.setStatus(http.StatusServiceUnavailable)

		e.wantNPM(t, "/npm/all/@acme%2fui", http.StatusOK)
		if got := e.wantNPM(t, "/npm/all/@acme/ui/-/ui-1.0.0.tgz", http.StatusOK); !bytes.Equal(got, tgz) {
			t.Fatalf("download = %q", got)
		}
		if n := up.count(); n != 0 {
			t.Fatalf("a claimed name made %d upstream requests", n)
		}
	})
}

// dist.tarball names the repository the client asked for, whichever member answered,
// so an install stays behind that repository's authorization.
func TestVirtualNPM_TarballURLNamesTheRepositoryTheClientAskedFor(t *testing.T) {
	e := newEnv(t, http.NotFound)
	e.createHostedNPM(t, "internal")
	up := e.createProxyNPM(t, "npmjs", 3600)
	e.createVirtualNPM(t, "team/inner", "internal", "npmjs")
	e.createVirtualNPM(t, "outer", "team/inner")
	hostedBytes := e.publishNPM(t, "internal", "@acme/ui", "1.0.0")
	publicBytes := up.offer(t, "@public/lib", "public", "1.0.0")

	for _, tc := range []struct {
		name, pkg, escaped, file string
		want                     []byte
	}{
		{"Hosted", "@acme/ui", "@acme%2fui", "ui-1.0.0.tgz", hostedBytes},
		{"Proxy", "@public/lib", "@public%2flib", "lib-1.0.0.tgz", publicBytes},
	} {
		for _, virtual := range []string{"team/inner", "outer"} {
			t.Run(tc.name+"/"+virtual, func(t *testing.T) {
				body := e.wantNPM(t, "/npm/"+virtual+"/"+tc.escaped, http.StatusOK)
				want := npmPublicURL + "/npm/" + virtual + "/" + tc.pkg + "/-/" + tc.file
				if got := npmVersionsIn(t, body)["1.0.0"]; got != want {
					t.Fatalf("dist.tarball = %q, want %q", got, want)
				}
				for _, leak := range []string{"/npm/internal/", "/npm/npmjs/", up.url} {
					if bytes.Contains(body, []byte(leak)) {
						t.Fatalf("packument names %s: %s", leak, body)
					}
				}
				if got := e.wantNPM(t, "/npm/"+virtual+"/"+tc.pkg+"/-/"+tc.file, http.StatusOK); !bytes.Equal(got, tc.want) {
					t.Fatalf("download = %q", got)
				}
			})
		}
	}
}

func TestVirtualNPM_AnUnreadableMemberIsSkipped(t *testing.T) {
	for _, virtual := range []string{"both", "nested"} {
		t.Run(virtual, func(t *testing.T) {
			e, first, second := newTwoProxies(t)
			first.offer(t, "foo", "first", "1.0.0")
			fromSecond := second.offer(t, "foo", "second", "1.0.0")
			e.denyMember(t, "first")

			e.wantNPM(t, "/npm/"+virtual+"/foo", http.StatusOK)
			if got := e.wantNPM(t, "/npm/"+virtual+"/foo/-/foo-1.0.0.tgz", http.StatusOK); !bytes.Equal(got, fromSecond) {
				t.Fatalf("download = %q", got)
			}
			if n := first.count(); n != 0 {
				t.Fatalf("an unreadable member's upstream got %d requests", n)
			}
		})
	}
}

// Through a virtual repository the walk decides which member answers, and that
// member's placement still decides whether it may: deduplication makes the bytes of
// a repository outside the walk physically present (I2).
func TestVirtualNPM_TarballIsReadThroughTheMembersPlacement(t *testing.T) {
	e := newEnv(t, http.NotFound)
	e.createHostedNPM(t, "outside")
	up := e.createProxyNPM(t, "npmjs", 3600)
	e.createVirtualNPM(t, "all", "npmjs")
	e.publishNPM(t, "outside", "@acme/ui", "1.0.0")

	e.wantNPM(t, "/npm/all/@acme%2fui", http.StatusNotFound)
	e.wantNPM(t, "/npm/all/@acme/ui/-/ui-1.0.0.tgz", http.StatusNotFound)

	// A proxy member caches into its own placement, once.
	tgz := up.offer(t, "foo", "public", "1.0.0")
	if got := e.wantNPM(t, "/npm/all/foo/-/foo-1.0.0.tgz", http.StatusOK); !bytes.Equal(got, tgz) {
		t.Fatalf("download = %q", got)
	}
	if _, rc, err := e.reg.GetNPMTarball(t.Context(), "npmjs", "foo", "foo-1.0.0.tgz"); err != nil {
		t.Fatalf("the proxy member holds no placement: %v", err)
	} else if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.reg.GetNPMTarball(t.Context(), "all", "foo", "foo-1.0.0.tgz"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("placement on the virtual repository itself: %v, want ErrNotFound", err)
	}
	before := up.count()
	if got := e.wantNPM(t, "/npm/all/foo/-/foo-1.0.0.tgz", http.StatusOK); !bytes.Equal(got, tgz) {
		t.Fatalf("cached download = %q", got)
	}
	if up.count() != before {
		t.Fatal("a placed tarball contacted upstream")
	}
}

func TestVirtualNPM_DistTagsComeFromTheMemberThatAnswers(t *testing.T) {
	e := newEnv(t, http.NotFound)
	e.createHostedNPM(t, "internal")
	up := e.createProxyNPM(t, "npmjs", 3600)
	e.createVirtualNPM(t, "all", "internal", "npmjs")
	e.publishNPM(t, "internal", "@acme/ui", "1.0.0")
	up.offer(t, "@public/lib", "public", "1.0.0", "2.0.0")

	for path, want := range map[string]string{"@acme%2fui": "1.0.0", "@public%2flib": "2.0.0"} {
		var tags map[string]string
		if err := json.Unmarshal(e.wantNPM(t, "/npm/all/-/package/"+path+"/dist-tags", http.StatusOK), &tags); err != nil {
			t.Fatal(err)
		}
		if tags["latest"] != want {
			t.Fatalf("%s: dist-tags = %v, want latest %s", path, tags, want)
		}
	}
}
