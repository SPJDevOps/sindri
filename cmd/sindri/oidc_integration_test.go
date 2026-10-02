//go:build integration

package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/spj/sindri/internal/auth/oidctest"
	"github.com/spj/sindri/internal/testsupport"
)

// startWithIdP runs serve with a stub identity provider configured and the
// RBAC tests' repositories declared, and pushes one image as the bootstrap
// administrator so there is something to pull.
func startWithIdP(t *testing.T) (*stack, *oidctest.Provider) {
	t.Helper()
	idp := oidctest.New(t)
	authLines := fmt.Sprintf("  admins: [team:sindri-admins]\n  oidc:\n    issuer: %s\n    clientId: %s\n",
		idp.Issuer(), oidctest.ClientID)
	s := startServeAuth(t, testsupport.StartPostgres(t), authLines, rbacRepos, func(h *serveHooks) {
		h.build.OIDCClient = idp.Client()
	})
	if status, body := s.pushImage("docker/demo/app", "1.0", "oidc", bearer(bootstrapToken)); status != http.StatusCreated {
		t.Fatalf("setup push: %d %s", status, body)
	}
	return s, idp
}

// uploadStatus posts an upload form to a PyPI repository. Authorization is
// decided before the form is read, so an empty form is enough to tell a
// refusal (403) from an accepted caller (400, the form is incomplete).
func (s *stack) uploadStatus(repo string, cred func(*http.Request)) int {
	s.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField(":action", "file_upload"); err != nil {
		s.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		s.t.Fatal(err)
	}
	r := s.request(http.MethodPost, "/pypi/"+repo+"/upload", &body, func(req *http.Request) {
		req.Header.Set("Content-Type", mw.FormDataContentType())
		cred(req)
	})
	return r.status
}

// Gate test 7: a user in team:payments can pull and cannot push, on both
// adapters, authenticated by an identity-provider token used as the password
// exactly as `docker login` and pip send it. A user in team:platform, which
// the same file grants write, can push.
func TestRBAC_TeamPaymentsCanPullCannotPush(t *testing.T) {
	s, idp := startWithIdP(t)
	idp.SetUser(oidctest.User{Subject: "p1", Email: "pat@example.com", Groups: []string{"team:payments"}})
	idp.SetUser(oidctest.User{Subject: "o1", Email: "ola@example.com", Groups: []string{"team:platform"}})
	payments := basic("pat@example.com", idp.Token("p1"))
	platform := basic("ola@example.com", idp.Token("o1"))

	if r := s.request(http.MethodGet, "/v2/docker/demo/app/manifests/1.0", nil, payments); r.status != http.StatusOK {
		t.Fatalf("team:payments docker pull: %d %s", r.status, r.body)
	}
	if status, body := s.pushImage("docker/demo/app", "2.0", "payments", payments); status != http.StatusForbidden || ociCode(t, []byte(body)) != "DENIED" {
		t.Fatalf("team:payments docker push: %d %s, want 403 DENIED", status, body)
	}
	if r := s.request(http.MethodGet, "/pypi/internal-py/simple/", nil, payments); r.status != http.StatusOK {
		t.Fatalf("team:payments pip index: %d %s", r.status, r.body)
	}
	if got := s.uploadStatus("internal-py", payments); got != http.StatusForbidden {
		t.Fatalf("team:payments twine upload: %d, want 403", got)
	}

	if status, body := s.pushImage("docker/demo/app", "2.0", "platform", platform); status != http.StatusCreated {
		t.Fatalf("team:platform docker push: %d %s", status, body)
	}
	if got := s.uploadStatus("internal-py", platform); got != http.StatusBadRequest {
		t.Fatalf("team:platform twine upload of an empty form: %d, want 400 (authorized, then refused as incomplete)", got)
	}

	// Neither team can reach a repository granted to neither: the name is not
	// even disclosed.
	if r := s.request(http.MethodGet, "/v2/secret/app/tags/list", nil, payments); r.status != http.StatusNotFound {
		t.Fatalf("team:payments on an ungranted repository: %d, want 404", r.status)
	}
	// And a user is never an administrator by being signed in.
	if r := s.request(http.MethodPost, "/api/v1/repositories", bytes.NewReader([]byte(`{"name":"x","protocol":"oci"}`)), bearer(idp.Token("o1"))); r.status != http.StatusForbidden {
		t.Fatalf("team:platform creating a repository: %d, want 403", r.status)
	}
}

// Gate test 9: adding a user to a group at the identity provider grants
// access with no re-apply, and removing them takes it away again. Nothing in
// Sindri's database changes along the way: membership is read from each
// request's token and never stored (ADR 5).
func TestRBAC_GroupChangeAtIdPGrantsAccessWithoutReapply(t *testing.T) {
	s, idp := startWithIdP(t)
	idp.SetUser(oidctest.User{Subject: "n1", Email: "new@example.com"})
	pull := func(token string) int {
		return s.request(http.MethodGet, "/v2/docker/demo/app/manifests/1.0", nil, basic("new@example.com", token)).status
	}
	before := s.dbFingerprint()

	stale := idp.Token("n1")
	if got := pull(stale); got != http.StatusNotFound {
		t.Fatalf("before joining: %d, want 404", got)
	}

	idp.SetUser(oidctest.User{Subject: "n1", Email: "new@example.com", Groups: []string{"team:payments"}})
	if got := pull(idp.Token("n1")); got != http.StatusOK {
		t.Fatalf("after joining team:payments at the provider: %d, want 200", got)
	}
	// The token minted before the change still says what it said: Sindri
	// read nothing anywhere but the token.
	if got := pull(stale); got != http.StatusNotFound {
		t.Fatalf("the pre-change token: %d, want 404", got)
	}

	idp.SetUser(oidctest.User{Subject: "n1", Email: "new@example.com"})
	if got := pull(idp.Token("n1")); got != http.StatusNotFound {
		t.Fatalf("after leaving the group: %d, want 404", got)
	}

	if after := s.dbFingerprint(); after != before {
		t.Fatalf("group changes wrote to the database:\nbefore %s\nafter  %s", before, after)
	}
}

// Administrators are a group at the provider too, read the same way.
func TestRBAC_AdminGroupFromProvider(t *testing.T) {
	s, idp := startWithIdP(t)
	idp.SetUser(oidctest.User{Subject: "a1", Email: "root@example.com", Groups: []string{"team:sindri-admins"}})
	r := s.request(http.MethodPost, "/api/v1/repositories", bytes.NewReader([]byte(`{"name":"made-by-admin","protocol":"oci"}`)), bearer(idp.Token("a1")))
	if r.status != http.StatusCreated {
		t.Fatalf("admin group creating a repository: %d %s", r.status, r.body)
	}
}

// dbFingerprint summarises every table a membership change could plausibly
// be written to.
func (s *stack) dbFingerprint() string {
	s.t.Helper()
	r := s.request(http.MethodGet, "/api/v1/repositories?limit=200", nil, bearer(bootstrapToken))
	t := s.request(http.MethodGet, "/api/v1/tokens", nil, bearer(bootstrapToken))
	return string(r.body) + string(t.body)
}
