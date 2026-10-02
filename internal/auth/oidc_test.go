package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/auth/oidctest"
)

func newOIDC(t *testing.T, idp *oidctest.Provider) *auth.OIDC {
	t.Helper()
	o, err := auth.NewOIDC(t.Context(), auth.OIDCConfig{
		Issuer:     idp.Issuer(),
		ClientID:   oidctest.ClientID,
		HTTPClient: idp.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return o
}

func TestOIDC_AcceptsAValidTokenAndReadsItsGroups(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetUser(oidctest.User{Subject: "u1", Email: "ada@example.com", Groups: []string{"team:payments", "team:platform"}})
	p, err := newOIDC(t, idp).Verify(t.Context(), idp.Token("u1"))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.Subject != "user:ada@example.com" || p.Kind != auth.KindUser {
		t.Fatalf("principal = %+v", p)
	}
	if len(p.Groups) != 2 || p.Groups[0] != "team:payments" {
		t.Fatalf("groups = %v", p.Groups)
	}
}

func TestOIDC_Rejects(t *testing.T) {
	idp := oidctest.New(t)
	other := oidctest.New(t) // a different provider, with a different key
	o := newOIDC(t, idp)

	tests := []struct {
		name  string
		token string
	}{
		{"WrongAudience", idp.Mint(oidctest.Claims{Subject: "u", Audience: "some-other-app"})},
		{"Expired", idp.Mint(oidctest.Claims{Subject: "u", Expiry: time.Now().Add(-time.Minute)})},
		{"UnknownIssuer", idp.Mint(oidctest.Claims{Subject: "u", Issuer: "https://evil.example.com"})},
		{"SignedByAnotherProvider", other.Mint(oidctest.Claims{Subject: "u", Issuer: idp.Issuer()})},
		{"Tampered", tamper(idp.Mint(oidctest.Claims{Subject: "u", Groups: []string{"team:x"}}))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := o.Verify(t.Context(), tc.token); !errors.Is(err, auth.ErrUnauthorized) {
				t.Fatalf("want ErrUnauthorized, got %v", err)
			}
		})
	}
}

// tamper rewrites the payload to claim an administrator group, keeping the
// original signature.
func tamper(token string) string {
	parts := strings.Split(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		panic(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		panic(err)
	}
	claims["groups"] = []string{"team:platform-admins"}
	raw, err = json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString(raw)
	return strings.Join(parts, ".")
}

// The key set is cached, and fetched again when a token names a key the
// cache has not seen: a provider's key rotation needs no restart.
func TestOIDC_RefetchesJWKSOnKeyRotation(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetUser(oidctest.User{Subject: "u1", Email: "ada@example.com"})
	o := newOIDC(t, idp)

	for range 3 {
		if _, err := o.Verify(t.Context(), idp.Token("u1")); err != nil {
			t.Fatal(err)
		}
	}
	if got := idp.JWKSFetches.Load(); got != 1 {
		t.Fatalf("JWKS fetched %d times for one key, want 1 (cached)", got)
	}

	idp.RotateKey()
	if _, err := o.Verify(t.Context(), idp.Token("u1")); err != nil {
		t.Fatalf("token signed with the rotated key: %v", err)
	}
	if got := idp.JWKSFetches.Load(); got != 2 {
		t.Fatalf("JWKS fetched %d times after rotation, want 2", got)
	}
}

// Discovery failing at boot does not stop the server starting; the verifier
// refuses identity-provider tokens until the provider answers.
func TestOIDC_UnreachableProviderRefusesWithoutFailingConstruction(t *testing.T) {
	o, err := auth.NewOIDC(t.Context(), auth.OIDCConfig{Issuer: "http://127.0.0.1:1", ClientID: "sindri"})
	if err == nil || !errors.Is(err, auth.ErrProviderUnavailable) || o == nil {
		t.Fatalf("NewOIDC = %v, %v", o, err)
	}
	if _, err := o.Verify(t.Context(), "a.b.c"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("Verify with no provider: %v", err)
	}
}

// The browser flow: login redirects to the provider with PKCE, the callback
// exchanges the code, verifies the ID token and its nonce, and sets the
// session cookie; whoami then reads the caller's groups from it.
func TestSession_LoginSetsCookieAndWhoamiReadsIt(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetUser(oidctest.User{Subject: "u1", Email: "ada@example.com", Groups: []string{"team:platform"}})
	idp.LoginAs("u1")
	o := newOIDC(t, idp)
	chain := auth.NewChain("boot", nil, o, []string{"team:platform"})

	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	mux := http.NewServeMux()
	mux.Handle("/auth/", auth.NewSessions(auth.SessionConfig{
		ClientID:    oidctest.ClientID,
		RedirectURL: srv.URL + "/auth/callback",
	}, o, chain).Handler())
	mux.Handle("/ui/", auth.GuardUI(chain, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFromContext(r.Context())
		_, _ = io.WriteString(w, "hello "+p.Subject)
	})))
	handler = mux

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	get := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Visiting the UI signed out goes through the provider and lands back
	// on the page asked for.
	status, body := get("/ui/repositories")
	if status != http.StatusOK || body != "hello user:ada@example.com" {
		t.Fatalf("UI after login: %d %q", status, body)
	}

	status, body = get("/auth/whoami")
	if status != http.StatusOK {
		t.Fatalf("whoami: %d %s", status, body)
	}
	var who struct {
		Subject string   `json:"subject"`
		Groups  []string `json:"groups"`
		Admin   bool     `json:"admin"`
	}
	if err := json.Unmarshal([]byte(body), &who); err != nil {
		t.Fatal(err)
	}
	if who.Subject != "user:ada@example.com" || !who.Admin || len(who.Groups) != 1 {
		t.Fatalf("whoami = %+v", who)
	}

	var session *http.Cookie
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range jar.Cookies(base) {
		if c.Name == auth.SessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie was set")
	}

	// The cookie alone is not a credential for anything that changes state,
	// nor for the API or the adapters, which use Chain.
	post := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ui/x", nil)
	post.AddCookie(session)
	if _, err := (auth.SessionAuthenticator{Chain: chain}).Authenticate(t.Context(), post); err == nil {
		t.Fatal("a session cookie authenticated a POST")
	}
	apiReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/repositories", nil)
	apiReq.AddCookie(session)
	if _, err := chain.Authenticate(t.Context(), apiReq); err == nil {
		t.Fatal("the API authenticator accepted a session cookie")
	}
}

func TestSession_CallbackRejectsAForeignState(t *testing.T) {
	idp := oidctest.New(t)
	o := newOIDC(t, idp)
	h := auth.NewSessions(auth.SessionConfig{ClientID: oidctest.ClientID, RedirectURL: "http://127.0.0.1/auth/callback"}, o,
		auth.NewChain("boot", nil, o, nil)).Handler()

	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("login: %d", login.Code)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/callback?code=x&state=forged", nil)
	for _, c := range login.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state: %d, want 400", rec.Code)
	}
}

// /auth/login never redirects off this server after signing in.
func TestSession_LoginRefusesOpenRedirects(t *testing.T) {
	idp := oidctest.New(t)
	o := newOIDC(t, idp)
	h := auth.NewSessions(auth.SessionConfig{ClientID: oidctest.ClientID, RedirectURL: "http://127.0.0.1/auth/callback"}, o,
		auth.NewChain("boot", nil, o, nil)).Handler()
	for _, next := range []string{"https://evil.example.com/", "//evil.example.com/", "/\\evil.example.com"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login?next="+next, nil))
		for _, c := range rec.Result().Cookies() {
			if strings.Contains(c.Value, "evil") {
				t.Fatalf("next=%q was kept for after login", next)
			}
		}
	}
}

func TestGuardUI_IsReadOnly(t *testing.T) {
	chain := auth.NewChain("boot", nil, nil, nil)
	h := auth.GuardUI(chain, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	anon := httptest.NewRecorder()
	h.ServeHTTP(anon, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/", nil))
	if anon.Code != http.StatusFound || !strings.HasPrefix(anon.Header().Get("Location"), "/auth/login") {
		t.Fatalf("anonymous UI visit: %d %s", anon.Code, anon.Header().Get("Location"))
	}

	post := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ui/", nil)
	post.Header.Set("Authorization", "Bearer boot")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, post)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to the UI: %d, want 405", rec.Code)
	}
}
