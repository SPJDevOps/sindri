package remote

import (
	"net/http"
	"net/http/httptest"
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
			if err := c.applyCredentials(t.Context(), req, rem); err != nil {
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
