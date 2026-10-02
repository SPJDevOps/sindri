// Package oidctest is a stub OpenID Connect provider for tests: discovery,
// a JWKS, an authorization endpoint that approves at once, and a token
// endpoint, all served from one httptest.Server whose URL is the issuer.
//
// Group membership lives in the stub and can change mid-test, which is what
// lets a test prove that Sindri reads groups from each token rather than
// remembering them.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ClientID is the audience the stub issues tokens for.
const ClientID = "sindri"

// Provider is the stub.
type Provider struct {
	t      testing.TB
	server *httptest.Server

	mu     sync.Mutex
	key    *rsa.PrivateKey
	kid    string
	keyGen int
	users  map[string]User // by subject
	codes  map[string]pendingCode
	// LoginAs is who /authorize signs in.
	loginAs string

	// JWKSFetches counts requests for the key set.
	JWKSFetches atomic.Int64
}

// User is an account at the stub provider.
type User struct {
	Subject string
	Email   string
	Groups  []string
}

type pendingCode struct {
	subject   string
	challenge string
	nonce     string
}

// New starts a stub provider that stops when the test ends.
func New(t testing.TB) *Provider {
	t.Helper()
	p := &Provider{t: t, users: map[string]User{}, codes: map[string]pendingCode{}}
	p.rotate()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

// Issuer is the provider's issuer URL.
func (p *Provider) Issuer() string { return p.server.URL }

// Client returns an HTTP client for the stub.
func (p *Provider) Client() *http.Client { return p.server.Client() }

// SetUser creates or replaces an account. Changing Groups here is a change
// at the identity provider.
func (p *Provider) SetUser(u User) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users[u.Subject] = u
}

// LoginAs makes the authorization endpoint sign in subject.
func (p *Provider) LoginAs(subject string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loginAs = subject
}

// RotateKey replaces the signing key. Tokens minted afterwards carry a key id
// the verifier has not seen.
func (p *Provider) RotateKey() { p.rotate() }

func (p *Provider) rotate() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		p.t.Fatalf("oidctest: generating key: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.key = key
	p.keyGen++
	p.kid = fmt.Sprintf("key-%d", p.keyGen)
}

// Token mints an ID token for subject as the account stands now.
func (p *Provider) Token(subject string) string {
	p.mu.Lock()
	u, ok := p.users[subject]
	p.mu.Unlock()
	if !ok {
		p.t.Fatalf("oidctest: no user %q", subject)
	}
	return p.Mint(Claims{Subject: u.Subject, Email: u.Email, Groups: u.Groups})
}

// Claims are what Mint signs. Zero fields take sensible defaults.
type Claims struct {
	Subject  string
	Email    string
	Groups   []string
	Audience string
	Issuer   string
	Expiry   time.Time
	Nonce    string
}

// Mint signs arbitrary claims, for tests of what the verifier rejects.
func (p *Provider) Mint(c Claims) string {
	if c.Audience == "" {
		c.Audience = ClientID
	}
	if c.Issuer == "" {
		c.Issuer = p.Issuer()
	}
	if c.Expiry.IsZero() {
		c.Expiry = time.Now().Add(time.Hour)
	}
	body := map[string]any{
		"iss":    c.Issuer,
		"sub":    c.Subject,
		"aud":    c.Audience,
		"exp":    c.Expiry.Unix(),
		"iat":    time.Now().Add(-time.Minute).Unix(),
		"email":  c.Email,
		"groups": c.Groups,
	}
	if c.Nonce != "" {
		body["nonce"] = c.Nonce
	}
	p.mu.Lock()
	key, kid := p.key, p.kid
	p.mu.Unlock()
	return sign(p.t, key, kid, body)
}

func sign(t testing.TB, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("oidctest: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signing := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("oidctest: signing: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	iss := p.Issuer()
	writeJSON(w, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/authorize",
		"token_endpoint":                        iss + "/token",
		"jwks_uri":                              iss + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	p.JWKSFetches.Add(1)
	p.mu.Lock()
	pub, kid := p.key.PublicKey, p.kid
	p.mu.Unlock()
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"alg": "RS256",
		"use": "sig",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// authorize approves at once as LoginAs and redirects back with a code.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ClientID || q.Get("response_type") != "code" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "PKCE required", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	subject := p.loginAs
	code := fmt.Sprintf("code-%d", len(p.codes)+1)
	p.codes[code] = pendingCode{subject: subject, challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	p.mu.Unlock()

	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	// G710: a stub provider redirects wherever the client under test asked;
	// that is the behaviour being stubbed, in tests only.
	http.Redirect(w, r, back.String(), http.StatusFound) //nolint:gosec // see above
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	pc, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	u := p.users[pc.subject]
	p.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != pc.challenge {
		http.Error(w, `{"error":"invalid_grant","error_description":"PKCE verifier mismatch"}`, http.StatusBadRequest)
		return
	}
	idToken := p.Mint(Claims{Subject: u.Subject, Email: u.Email, Groups: u.Groups, Nonce: pc.nonce})
	writeJSON(w, map[string]any{
		"access_token": "opaque-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
