//go:build integration

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/auth/oidctest"
	"github.com/spj/sindri/internal/testsupport"
)

const uiRepos = `
# --- desired
repos:
  team-a: { protocol: oci, read: [team:a], write: [team:a] }
  team-b: { protocol: oci, read: [team:b], write: [team:b] }
`

// freeAddr reserves a loopback port and releases it, so the configuration can
// name the server's own URL before it starts.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// browser is a cookie-keeping HTTP client, which is what the authorization
// code flow needs to come back to the page it started from.
func browser(t *testing.T, follow bool) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	if !follow {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

func visit(t *testing.T, c *http.Client, method, url string, set func(*http.Request)) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if set != nil {
		set(req)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

// The UI end to end through the real wiring: a person signs in through the
// identity provider, sees only the repositories their groups may read, cannot
// open another team's pages, and cannot change anything.
func TestServe_UIBrowserSignInShowsOnlyReadableRepos(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetUser(oidctest.User{Subject: "u-a", Email: "alice@example.com", Groups: []string{"team:a"}})
	idp.LoginAs("u-a")
	addr := freeAddr(t)
	authLines := fmt.Sprintf("  oidc:\n    issuer: %s\n    clientId: %s\n    redirectUrl: http://%s/auth/callback\n",
		idp.Issuer(), oidctest.ClientID, addr)
	s := startServeOn(t, addr, testsupport.DatabaseURL(t), authLines, uiRepos, func(h *serveHooks) {
		h.build.OIDCClient = idp.Client()
	})
	if status, body := s.pushImage("team-b/app", "1.0", "ui", bearer(bootstrapToken)); status != http.StatusCreated {
		t.Fatalf("setup push: %d %s", status, body)
	}

	t.Run("SignedOutIsSentToSignIn", func(t *testing.T) {
		status, _, hdr := visit(t, browser(t, false), http.MethodGet, s.base+"/ui/", nil)
		if status != http.StatusFound || !strings.HasPrefix(hdr.Get("Location"), "/auth/login?next=") {
			t.Fatalf("signed-out visit: %d %q, want a redirect to /auth/login", status, hdr.Get("Location"))
		}
	})

	alice := browser(t, true)
	t.Run("SignedInSeesOnlyHerRepositories", func(t *testing.T) {
		status, body, _ := visit(t, alice, http.MethodGet, s.base+"/", nil)
		if status != http.StatusOK {
			t.Fatalf("after sign-in: %d %s", status, body)
		}
		if !strings.Contains(body, `href="/ui/r/team-a"`) || !strings.Contains(body, "user:alice@example.com") {
			t.Fatalf("alice's home page lacks team-a or her name:\n%s", body)
		}
		if strings.Contains(body, "team-b") {
			t.Fatal("alice's home page names team-b")
		}
		if status, _, _ := visit(t, alice, http.MethodGet, s.base+"/ui/r/team-b/-/package?name=app", nil); status != http.StatusNotFound {
			t.Fatalf("alice opening team-b: %d, want 404", status)
		}
	})

	t.Run("NothingChangesThroughTheUI", func(t *testing.T) {
		// With a token the guard admits the caller and refuses the method.
		status, _, _ := visit(t, browser(t, false), http.MethodPost, s.base+"/ui/", bearer(bootstrapToken))
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("POST /ui/ with a token: %d, want 405", status)
		}
		// With only the session cookie, nothing but GET and HEAD is even
		// authenticated, so a cross-site POST borrows nothing.
		status, _, _ = visit(t, alice, http.MethodPost, s.base+"/ui/", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("POST /ui/ with the session cookie: %d, want 401", status)
		}
		status, _, _ = visit(t, alice, http.MethodGet, s.base+"/api/v1/repositories", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("the API with the session cookie: %d, want 401", status)
		}
	})

	t.Run("AssetsLoadForASignedInBrowser", func(t *testing.T) {
		_, body, _ := visit(t, alice, http.MethodGet, s.base+"/ui/", nil)
		start := strings.Index(body, `href="/ui/static/`)
		if start < 0 {
			t.Fatal("the page links no stylesheet")
		}
		href := body[start+len(`href="`):]
		href = href[:strings.IndexByte(href, '"')]
		status, css, hdr := visit(t, alice, http.MethodGet, s.base+href, nil)
		if status != http.StatusOK || !strings.Contains(css, "--accent") || !strings.HasPrefix(hdr.Get("Content-Type"), "text/css") {
			t.Fatalf("stylesheet %s: %d %q", href, status, hdr.Get("Content-Type"))
		}
	})

	t.Run("SignOutClearsTheSession", func(t *testing.T) {
		status, _, hdr := visit(t, alice, http.MethodPost, s.base+"/auth/logout", func(r *http.Request) {
			r.Header.Set("HX-Request", "true")
		})
		if status != http.StatusNoContent || hdr.Get("HX-Redirect") != "/ui/" {
			t.Fatalf("sign out: %d %q", status, hdr.Get("HX-Redirect"))
		}
		for _, c := range alice.Jar.Cookies(mustURL(t, s.base)) {
			if c.Name == auth.SessionCookie {
				t.Fatal("the session cookie survived sign-out")
			}
		}
	})
}

// Without an identity provider the UI explains how to enable sign-in rather
// than redirecting to a login page that does not exist or asking the browser
// for Basic credentials.
func TestServe_UIWithoutSignInExplains(t *testing.T) {
	s := startServe(t, testsupport.DatabaseURL(t), uiRepos, nil)
	status, body, hdr := visit(t, browser(t, false), http.MethodGet, s.base+"/ui/", nil)
	if status != http.StatusUnauthorized || !strings.Contains(body, "auth.oidc") {
		t.Fatalf("UI without sign-in: %d %s", status, body)
	}
	if strings.HasPrefix(hdr.Get("WWW-Authenticate"), "Basic") {
		t.Fatal("the UI asked the browser for Basic credentials")
	}
	// The bootstrap token still opens it, for an operator with curl.
	if status, _, _ := visit(t, browser(t, false), http.MethodGet, s.base+"/ui/", bearer(bootstrapToken)); status != http.StatusOK {
		t.Fatalf("UI with the bootstrap token: %d", status)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
