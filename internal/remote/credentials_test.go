package remote

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spj/sindri/internal/registry"
)

func TestApplyCredentials_TokenSendsBearerOnUpstreamOriginOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		cred       Credential
		sameOrigin bool
		wantAuth   string
	}{
		{name: "TokenIsBearerOnTheUpstreamOrigin", cred: Credential{Token: "npm_tok3n"}, sameOrigin: true, wantAuth: "Bearer npm_tok3n"},
		{name: "TokenIsWithheldFromAnotherOrigin", cred: Credential{Token: "npm_tok3n"}, sameOrigin: false, wantAuth: ""},
		{name: "PasswordIsBasicOnTheUpstreamOrigin", cred: Credential{Username: "u", Password: "p"}, sameOrigin: true, wantAuth: "Basic dTpw"},
		{name: "PasswordIsWithheldFromAnotherOrigin", cred: Credential{Username: "u", Password: "p"}, sameOrigin: false, wantAuth: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(Options{Registry: &registry.Registry{}, Credentials: StaticCredentials{"mirror": tc.cred}})
			target := "https://registry.example.com/pkg"
			if !tc.sameOrigin {
				target = "https://elsewhere.example.net/pkg"
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			rem := registry.Remote{UpstreamURL: "https://registry.example.com", CredentialsRef: "mirror"}
			if err := c.applyCredentials(t.Context(), req, rem, false); err != nil {
				t.Fatalf("applyCredentials: %v", err)
			}
			if got := req.Header.Get("Authorization"); got != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", got, tc.wantAuth)
			}
		})
	}
}

func TestRedirect_DropsBearerAcrossOrigins(t *testing.T) {
	t.Parallel()
	var offsiteAuth atomic.Value
	offsite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsiteAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(offsite.Close)
	var onsiteAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, offsite.URL+"/file", http.StatusFound)
		case "/here":
			http.Redirect(w, r, "/file", http.StatusFound)
		default:
			onsiteAuth.Store(r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(upstream.Close)

	c := New(Options{Registry: &registry.Registry{}, Credentials: StaticCredentials{"tok": {Token: "npm_tok3n"}}})
	rem := registry.Remote{UpstreamURL: upstream.URL, CredentialsRef: "tok"}
	get := func(path string) {
		t.Helper()
		resp, err := c.do(t.Context(), upstreamRequest{method: http.MethodGet, url: upstream.URL + path, remote: rem})
		if err != nil {
			t.Fatalf("do %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}

	get("/away")
	if got, _ := offsiteAuth.Load().(string); got != "" {
		t.Fatalf("a redirect off the origin carried Authorization %q", got)
	}
	get("/here")
	if got, _ := onsiteAuth.Load().(string); got != "Bearer npm_tok3n" {
		t.Fatalf("a same-origin redirect lost the token: Authorization = %q", got)
	}
}

func TestDo_OCIRequestNeverCarriesATokenCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		cred      Credential
		oci       bool
		wantErr   bool
		wantAuth  string
		wantReach bool
	}{
		{name: "OCIRefusesATokenBeforeSending", cred: Credential{Token: "s3cret-tok"}, oci: true, wantErr: true},
		{name: "OCIStillSendsBasic", cred: Credential{Username: "u", Password: "p"}, oci: true, wantAuth: "Basic dTpw", wantReach: true},
		{name: "NonOCISendsTheToken", cred: Credential{Token: "s3cret-tok"}, oci: false, wantAuth: "Bearer s3cret-tok", wantReach: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reached atomic.Bool
			var auth atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Store(true)
				auth.Store(r.Header.Get("Authorization"))
			}))
			t.Cleanup(srv.Close)
			c := New(Options{Registry: &registry.Registry{}, Credentials: StaticCredentials{"mirror": tc.cred}})
			ur := upstreamRequest{method: http.MethodGet, url: srv.URL + "/v2/x", remote: registry.Remote{UpstreamURL: srv.URL, CredentialsRef: "mirror"}}
			if tc.oci {
				ur.tokenKey = "host|mirror|x"
			}
			resp, err := c.do(t.Context(), ur)
			if err == nil {
				_ = resp.Body.Close()
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("do error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !strings.Contains(err.Error(), `"mirror"`) || strings.Contains(err.Error(), "s3cret-tok") {
					t.Fatalf("error must name the reference and never the secret: %v", err)
				}
			}
			if reached.Load() != tc.wantReach {
				t.Fatalf("upstream reached = %v, want %v", reached.Load(), tc.wantReach)
			}
			if got, _ := auth.Load().(string); got != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", got, tc.wantAuth)
			}
		})
	}
}
