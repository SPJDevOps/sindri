package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	// SessionCookie holds a browser's ID token. It is honoured only by the
	// read-only UI and /auth/whoami, never by the API or the adapters, so a
	// cross-site request cannot borrow it to change anything.
	SessionCookie = "sindri_session"
	loginCookie   = "sindri_login"
	loginLifetime = 10 * time.Minute
)

// SessionConfig configures the browser login flow.
type SessionConfig struct {
	ClientID     string
	ClientSecret string
	// RedirectURL is this server's /auth/callback as the browser reaches it.
	RedirectURL string
	Scopes      []string
}

// Sessions serves /auth: the OIDC authorization code flow with PKCE, logout,
// and whoami.
type Sessions struct {
	cfg   SessionConfig
	oidc  *OIDC
	chain *Chain
	// secure marks cookies Secure when the redirect URL is https.
	secure bool
}

// NewSessions wires the browser flow onto a verifier and the chain that
// decides administrator status.
func NewSessions(cfg SessionConfig, o *OIDC, chain *Chain) *Sessions {
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return &Sessions{cfg: cfg, oidc: o, chain: chain, secure: strings.HasPrefix(cfg.RedirectURL, "https://")}
}

// Handler returns the /auth routes. Mount it at /auth.
func (s *Sessions) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("GET /callback", s.callback)
	mux.HandleFunc("POST /logout", s.logout)
	mux.Handle("GET /whoami", Middleware(SessionAuthenticator{Chain: s.chain}, denyJSON)(http.HandlerFunc(whoami)))
	return http.StripPrefix("/auth", mux)
}

func (s *Sessions) oauth(provider *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.cfg.ClientID,
		ClientSecret: s.cfg.ClientSecret,
		RedirectURL:  s.cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       s.cfg.Scopes,
	}
}

// cookie builds every cookie this flow sets: HttpOnly, SameSite=Lax, and
// Secure whenever the server is reached over https.
func (s *Sessions) cookie(name, value, path string, maxAge int) *http.Cookie {
	// G124: Secure is false only when redirectUrl is plain http, which
	// config validation allows for a loopback host alone: a development
	// browser on localhost would otherwise never send the cookie back.
	return &http.Cookie{ //nolint:gosec // see above
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

type pendingLogin struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Next     string `json:"r"`
}

func (s *Sessions) login(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RedirectURL == "" {
		http.Error(w, "browser login needs auth.oidc.redirectUrl", http.StatusNotFound)
		return
	}
	provider, err := s.oidc.Provider()
	if err != nil {
		http.Error(w, "identity provider unavailable", http.StatusServiceUnavailable)
		return
	}
	pl := pendingLogin{
		State:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		Nonce:    randomString(),
		Next:     safeNext(r.URL.Query().Get("next")),
	}
	raw, err := json.Marshal(pl)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, s.cookie(loginCookie, base64.RawURLEncoding.EncodeToString(raw), "/auth/", int(loginLifetime/time.Second)))
	target := s.oauth(provider).AuthCodeURL(pl.State, oauth2.S256ChallengeOption(pl.Verifier), oidc.Nonce(pl.Nonce))
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Sessions) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(loginCookie)
	if err != nil {
		http.Error(w, "login expired; start again at /auth/login", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	var pl pendingLogin
	if err != nil || json.Unmarshal(raw, &pl) != nil || pl.State == "" {
		http.Error(w, "login expired; start again at /auth/login", http.StatusBadRequest)
		return
	}
	// The state ties this callback to the browser that started the login,
	// which is the CSRF defence of the authorization code flow.
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(pl.State)) != 1 {
		http.Error(w, "login state mismatch", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, s.cookie(loginCookie, "", "/auth/", -1))

	provider, err := s.oidc.Provider()
	if err != nil {
		http.Error(w, "identity provider unavailable", http.StatusServiceUnavailable)
		return
	}
	tok, err := s.oauth(provider).Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(pl.Verifier))
	if err != nil {
		http.Error(w, "the identity provider refused the login", http.StatusUnauthorized)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		http.Error(w, "the identity provider returned no ID token", http.StatusUnauthorized)
		return
	}
	verifier, err := s.oidc.discover()
	if err != nil {
		http.Error(w, "identity provider unavailable", http.StatusServiceUnavailable)
		return
	}
	idTok, err := verifier.Verify(r.Context(), rawID)
	if err != nil || subtle.ConstantTimeCompare([]byte(idTok.Nonce), []byte(pl.Nonce)) != 1 {
		http.Error(w, "the identity provider returned an invalid ID token", http.StatusUnauthorized)
		return
	}

	// The cookie lives as long as the token in it; Verify refuses it after
	// that anyway.
	http.SetCookie(w, s.cookie(SessionCookie, rawID, "/", max(1, int(time.Until(idTok.Expiry)/time.Second))))
	http.Redirect(w, r, pl.Next, http.StatusFound)
}

func (s *Sessions) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.cookie(SessionCookie, "", "/", -1))
	// The UI's sign-out control posts through htmx, which follows this
	// header rather than a redirect status.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/ui/")
	}
	w.WriteHeader(http.StatusNoContent)
}

type whoamiResponse struct {
	Subject string   `json:"subject"`
	Kind    Kind     `json:"kind"`
	Groups  []string `json:"groups"`
	Admin   bool     `json:"admin"`
}

func whoami(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	groups := p.Groups
	if groups == nil {
		groups = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(whoamiResponse{Subject: p.Subject, Kind: p.Kind, Groups: groups, Admin: p.Admin})
}

func denyJSON(w http.ResponseWriter, _ *http.Request, _ error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="sindri"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"code":"UNAUTHORIZED","message":"sign in at /auth/login"}` + "\n"))
}

// SessionAuthenticator accepts what Chain accepts, and also the session
// cookie on safe methods. Only the read-only UI and /auth/whoami use it.
type SessionAuthenticator struct {
	Chain *Chain
}

// Authenticate implements Authenticator.
func (s SessionAuthenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	if _, ok := Credential(r); ok {
		return s.Chain.Authenticate(ctx, r)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return Principal{}, fmt.Errorf("%w: a session cookie is accepted only on GET and HEAD", ErrUnauthorized)
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return Principal{}, ErrUnauthorized
	}
	if !looksLikeJWT(c.Value) {
		return Principal{}, errors.New("malformed session cookie")
	}
	return s.Chain.Verify(ctx, c.Value)
}

// GuardUI wraps the read-only UI: every page needs a signed-in principal, by
// session cookie or token, and only GET and HEAD reach it.
//
// login says whether browser sign-in (auth.oidc with a redirect URL) is
// configured. Without it there is no /auth/login to send a browser to, so a
// signed-out visit gets a page saying how to enable it, linking docsURL. It never gets a Basic
// challenge: a browser caches Basic credentials and attaches them to
// cross-site requests, and every write route accepts Basic, because that is
// what docker, pip, and twine send (ADR 21).
func GuardUI(chain *Chain, ui http.Handler, login bool, docsURL string) http.Handler {
	help, err := signInHelpPage(docsURL)
	if err != nil {
		help = "Sign-in is not configured: set auth.oidc in sindri.yaml.\n"
	}
	readOnly := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "the UI is read-only", http.StatusMethodNotAllowed)
			return
		}
		ui.ServeHTTP(w, r)
	})
	return Middleware(SessionAuthenticator{Chain: chain}, func(w http.ResponseWriter, r *http.Request, _ error) {
		switch {
		case r.Method == http.MethodGet && login && r.Header.Get("HX-Request") == "true":
			// A 302 would be followed silently by XHR, all the way to the
			// identity provider, and the partial would break. htmx acts on
			// HX-Redirect whatever the status, and navigates the whole page.
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("HX-Redirect", loginURL(htmxNext(r)))
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && login:
			// G710: the target is the fixed /auth/login path; the request URI
			// travels only as an escaped query value, which login re-checks.
			http.Redirect(w, r, loginURL(r.URL.RequestURI()), http.StatusFound) //nolint:gosec // see above
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Security-Policy", signInHelpCSP)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, help)
		default:
			denyJSON(w, r, nil)
		}
	})(readOnly)
}

func loginURL(next string) string { return "/auth/login?next=" + url.QueryEscape(next) }

// htmxNext is where to send a browser back to after it signs in again: the
// page the htmx request was made from, which HX-Current-URL names, else the
// request's own URI. Only a path and query survive, and safeNext keeps them
// on this server.
func htmxNext(r *http.Request) string {
	if cur, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && cur.Path != "" {
		if next := cur.EscapedPath(); safeNext(next) == next {
			if cur.RawQuery != "" {
				next += "?" + cur.RawQuery
			}
			return next
		}
	}
	return r.URL.RequestURI()
}

// signInHelp is the page a signed-out visitor gets when no browser login is
// configured. It is served before sign-in, and the UI's assets sit behind
// sign-in, so it carries its own small stylesheet. The Content-Security-Policy
// admits that one stylesheet by hash and nothing else: no script, no image, no
// font, no other origin.
var signInHelp = template.Must(template.New("signin").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Sign-in is not configured · Sindri</title>
<style>` + signInStyle + `</style></head>
<body><main>
<h1>Sign-in is not configured</h1>
<p>The Sindri web interface signs people in through an OpenID Connect identity provider, and this server has none.
Set <code>auth.oidc</code> in <code>sindri.yaml</code> and restart:</p>
<pre>` + signInExample + `</pre>
<ul>
<li><code>issuer</code>, <code>clientId</code>, and <code>clientSecret</code> come from the client you register with the provider.</li>
<li><code>redirectUrl</code> is this server&rsquo;s own address followed by <code>/auth/callback</code>, and it must be registered with the provider as written. It is what turns sign-in on.</li>
<li><code>admins</code> lists the provider groups whose members may open the System pages.</li>
<li>If <code>auth</code> is a single token today, keep it as <code>static_token</code>, as above.</li>
</ul>
<p>docker, pip, npm, twine, and the management API keep working with machine tokens and the bootstrap token.</p>
<p><a href="{{.DocsURL}}">Read the documentation</a></p>
</main></body></html>
`))

const signInExample = `auth:
  static_token: ${SINDRI_TOKEN}
  admins: [platform-admins]
  oidc:
    issuer: https://idp.example.com/realms/main
    clientId: sindri
    clientSecret: ${SINDRI_OIDC_CLIENT_SECRET}
    redirectUrl: https://sindri.example.com/auth/callback`

const signInStyle = `
:root { color-scheme: light dark; }
body { margin: 0; font: 14px/1.6 system-ui, sans-serif; background: Canvas; color: CanvasText; }
main { max-width: 42rem; margin: 0 auto; padding: 48px 24px; }
h1 { font-size: 24px; margin: 0 0 16px; }
pre { overflow-x: auto; padding: 12px; border: 1px solid GrayText; border-radius: 2px; }
code, pre { font-family: ui-monospace, Menlo, monospace; font-size: 13px; }
`

// signInHelpCSP admits the page's one stylesheet by its hash.
var signInHelpCSP = func() string {
	sum := sha256.Sum256([]byte(signInStyle))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

// signInHelpPage renders the page for one documentation address.
func signInHelpPage(docsURL string) (string, error) {
	var b strings.Builder
	if err := signInHelp.Execute(&b, struct{ DocsURL string }{docsURL}); err != nil {
		return "", fmt.Errorf("rendering the sign-in help: %w", err)
	}
	return b.String(), nil
}

// safeNext keeps the post-login redirect on this server: a path, never a
// scheme-relative or absolute URL that would make /auth/login an open
// redirect.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/auth/whoami"
	}
	return next
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}
