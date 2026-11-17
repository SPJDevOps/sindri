package auth_test

import (
	"crypto/sha256"
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
	"github.com/spj/sindri/internal/config"
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
	}), true, "https://docs.example/"))
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
	tests := []struct{ name, next string }{
		{"Absolute", "https://evil.example.com/"},
		{"SchemeRelative", "//evil.example.com/"},
		{"Backslash", "/\\evil.example.com"},
		// Browsers strip tabs and newlines from a URL before resolving it,
		// so "/<tab>/evil" becomes "//evil" in the Location header.
		{"TabBetweenSlashes", "/%09/evil.example.com"},
		{"NewlineBetweenSlashes", "/%0A/evil.example.com"},
		{"CarriageReturnBetweenSlashes", "/%0D/evil.example.com"},
		{"TabBeforeBackslash", "/%09%5Cevil.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login?next="+tc.next, nil))
			if got := pendingNext(t, rec); got != "/auth/whoami" {
				t.Fatalf("next=%q was kept for after login as %q", tc.next, got)
			}
		})
	}
}

// pendingNext decodes the login cookie and returns where the callback will
// send the browser.
func pendingNext(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name != "sindri_login" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(c.Value)
		if err != nil {
			t.Fatalf("decoding the login cookie: %v", err)
		}
		var pl struct {
			Next string `json:"r"`
		}
		if err := json.Unmarshal(raw, &pl); err != nil {
			t.Fatalf("parsing the login cookie: %v", err)
		}
		return pl.Next
	}
	t.Fatalf("login set no login cookie (status %d)", rec.Code)
	return ""
}

func TestGuardUI_IsReadOnly(t *testing.T) {
	chain := auth.NewChain("boot", nil, nil, nil)
	h := auth.GuardUI(chain, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), true, "https://docs.example/")

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

// Without an identity provider there is no /auth/login to send a browser to.
// The guard explains instead, and never answers with a Basic challenge: a
// browser would cache Basic credentials and attach them to cross-site
// requests, including POSTs to the API and the adapters.
func TestGuardUI_NoLoginConfiguredExplainsInsteadOfRedirecting(t *testing.T) {
	chain := auth.NewChain("boot", nil, nil, nil)
	h := auth.GuardUI(chain, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), false, "https://docs.example/")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous UI visit without login: %d, want 401", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("redirected to %s, which does not exist without auth.oidc", loc)
	}
	if strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatal("the UI asked the browser for Basic credentials")
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "auth.oidc") {
		t.Fatalf("the explanation does not say how to enable sign-in: %q", rec.Body.String())
	}

	// A token still works, for scripts and for testing.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/", nil)
	req.Header.Set("Authorization", "Bearer boot")
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, req)
	if ok.Code != http.StatusOK {
		t.Fatalf("UI visit with a token: %d", ok.Code)
	}
}

// Without a provider the page says what to write, with every field the
// loader requires, and links the configured documentation. It must work
// before sign-in, so it may load nothing and its policy admits nothing but its
// own stylesheet.
func TestGuardUI_NoLoginConfiguredShowsTheOIDCBlockAndTheDocsLink(t *testing.T) {
	chain := auth.NewChain("boot", nil, nil, nil)
	h := auth.GuardUI(chain, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), false, "https://docs.internal.example/sindri/?a=1&b=2")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"auth:\n", "  oidc:\n", "    issuer: https://", "    clientId: ", "    clientSecret: ${", "    redirectUrl: https://", "/auth/callback",
		`href="https://docs.internal.example/sindri/?a=1&amp;b=2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
	for _, banned := range []string{"<script", "<link", "<img", " src=", "@import", "url("} {
		if strings.Contains(body, banned) {
			t.Errorf("the page loads or runs something (%q)", banned)
		}
	}
	// The block must be one the loader accepts as written, once the operator
	// has set the variables it names.
	t.Setenv("SINDRI_TOKEN", "a-token")
	t.Setenv("SINDRI_OIDC_CLIENT_SECRET", "a-secret")
	pre := body[strings.Index(body, "<pre>")+len("<pre>") : strings.Index(body, "</pre>")]
	cfg, err := config.Parse([]byte(pre), "signin-example.yaml")
	if err != nil {
		t.Fatalf("the example block does not load: %v\n%s", err, pre)
	}
	if cfg.Auth.OIDC == nil || cfg.Auth.OIDC.RedirectURL == "" || cfg.Auth.OIDC.ClientID == "" || cfg.Auth.OIDC.Issuer == "" {
		t.Errorf("the example block leaves out a field browser sign-in needs: %+v", cfg.Auth.OIDC)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.HasPrefix(csp, "default-src 'none'") || strings.Contains(csp, "unsafe") || strings.Contains(csp, "script-src") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	// The one inline stylesheet is admitted by its hash, so it must be the
	// hashed one.
	style := strings.TrimSuffix(strings.TrimPrefix(body[strings.Index(body, "<style>"):strings.Index(body, "</style>")], "<style>"), "")
	sum := sha256.Sum256([]byte(style))
	if want := "style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
		t.Errorf("Content-Security-Policy = %q, want it to contain %q", csp, want)
	}
}

// An htmx request is XHR, which follows a 302 silently to the identity
// provider and hands htmx a page it cannot swap. The guard answers 401 with
// HX-Redirect instead, which htmx follows by navigating the whole page.
func TestGuardUI_HtmxRequestWithoutASessionGetsAnHXRedirect(t *testing.T) {
	chain := auth.NewChain("boot", nil, nil, nil)
	h := auth.GuardUI(chain, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), true, "https://docs.example/")

	tests := []struct {
		name    string
		target  string
		current string
		want    string
	}{
		{"CurrentPageWithItsQuery", "/ui/r/team/app?cursor=web", "https://sindri.example.com/ui/r/team/app/-/package?name=web&x=1", "/auth/login?next=%2Fui%2Fr%2Fteam%2Fapp%2F-%2Fpackage%3Fname%3Dweb%26x%3D1"},
		{"NoCurrentURLFallsBackToTheRequest", "/ui/r/team/app?cursor=web", "", "/auth/login?next=%2Fui%2Fr%2Fteam%2Fapp%3Fcursor%3Dweb"},
		{"UnparseableCurrentURLFallsBackToTheRequest", "/ui/?cursor=a", "http://[::1", "/auth/login?next=%2Fui%2F%3Fcursor%3Da"},
		{"CurrentURLWithoutAPathFallsBackToTheRequest", "/ui/?cursor=a", "https://evil.example", "/auth/login?next=%2Fui%2F%3Fcursor%3Da"},
		{"SchemeRelativePathIsNotKept", "/ui/?cursor=a", "https://sindri.example.com//evil.example/x", "/auth/login?next=%2Fui%2F%3Fcursor%3Da"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.target, nil)
			req.Header.Set("HX-Request", "true")
			if tc.current != "" {
				req.Header.Set("HX-Current-URL", tc.current)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("HX-Redirect"); got != tc.want {
				t.Errorf("HX-Redirect = %q, want %q", got, tc.want)
			}
			if rec.Header().Get("Location") != "" {
				t.Errorf("Location = %q: a redirect status would be followed by XHR", rec.Header().Get("Location"))
			}
		})
	}
}

// A browser navigating to a page still gets the plain redirect.
func TestGuardUI_FullPageRequestWithoutASessionStillRedirects(t *testing.T) {
	chain := auth.NewChain("boot", nil, nil, nil)
	h := auth.GuardUI(chain, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), true, "https://docs.example/")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/r/team/app?x=1", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?next=%2Fui%2Fr%2Fteam%2Fapp%3Fx%3D1" {
		t.Fatalf("full page visit: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("HX-Redirect") != "" {
		t.Errorf("a full-page request got HX-Redirect %q", rec.Header().Get("HX-Redirect"))
	}
}

// The UI's sign-out control posts through htmx, which follows HX-Redirect
// rather than a 3xx.
func TestSession_LogoutFromTheUIRedirectsWithHtmx(t *testing.T) {
	idp := oidctest.New(t)
	o := newOIDC(t, idp)
	chain := auth.NewChain("boot", nil, o, nil)
	h := auth.NewSessions(auth.SessionConfig{ClientID: oidctest.ClientID, RedirectURL: "http://localhost/auth/callback"}, o, chain).Handler()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("HX-Redirect") != "/ui/" {
		t.Fatalf("logout from the UI: %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), auth.SessionCookie+"=;") {
		t.Fatalf("logout did not clear the session: %q", rec.Header().Get("Set-Cookie"))
	}
}

// docs/configuration.md states this default for auth.oidc.scopes, and
// TestReference_StatedDefaultsAreChecked names this test as what keeps it true.
func TestSession_LoginRequestsOpenIDProfileAndEmailByDefault(t *testing.T) {
	idp := oidctest.New(t)
	o := newOIDC(t, idp)
	h := auth.NewSessions(auth.SessionConfig{ClientID: oidctest.ClientID, RedirectURL: "http://127.0.0.1/auth/callback"}, o,
		auth.NewChain("boot", nil, o, nil)).Handler()

	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("login: %d", login.Code)
	}
	loc, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := loc.Query().Get("scope"), "openid profile email"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
}
