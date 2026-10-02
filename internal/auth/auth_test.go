package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/model"
)

// fakeVerifier stands in for the OIDC and machine-token verifiers.
type fakeVerifier map[string]auth.Principal

func (f fakeVerifier) Verify(_ context.Context, raw string) (auth.Principal, error) {
	p, ok := f[raw]
	if !ok {
		return auth.Principal{}, auth.ErrUnauthorized
	}
	return p, nil
}

const jwtShaped = "aaa.bbb.ccc"

func newChain(admins ...string) *auth.Chain {
	return auth.NewChain("boot-token",
		fakeVerifier{"sindri_ci": {Subject: "token:ci", Kind: auth.KindMachine, Token: &auth.TokenScope{Role: auth.RoleRead, Repository: "docker"}}},
		fakeVerifier{jwtShaped: {Subject: "user:ada", Kind: auth.KindUser, Groups: []string{"team:platform"}}},
		admins)
}

func request(t *testing.T, set func(*http.Request)) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	if set != nil {
		set(req)
	}
	return req
}

// Every credential arrives as a Bearer token or a Basic password, and is
// routed by its shape.
func TestChain_RoutesCredentialsByShape(t *testing.T) {
	tests := []struct {
		name    string
		set     func(*http.Request)
		subject string
	}{
		{"BootstrapBearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer boot-token") }, "bootstrap"},
		{"BootstrapAsBasicPassword", func(r *http.Request) { r.SetBasicAuth("anything", "boot-token") }, "bootstrap"},
		{"MachineTokenAsBasicPassword", func(r *http.Request) { r.SetBasicAuth("ci", "sindri_ci") }, "token:ci"},
		{"IdentityProviderTokenBearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+jwtShaped) }, "user:ada"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newChain().Authenticate(t.Context(), request(t, tc.set))
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if p.Subject != tc.subject {
				t.Fatalf("Subject = %q, want %q", p.Subject, tc.subject)
			}
		})
	}
}

func TestChain_RejectsWhatItDoesNotKnow(t *testing.T) {
	tests := []struct {
		name string
		set  func(*http.Request)
	}{
		{"Anonymous", nil},
		{"WrongBootstrap", func(r *http.Request) { r.Header.Set("Authorization", "Bearer boot-tokenX") }},
		{"UnknownMachineToken", func(r *http.Request) { r.SetBasicAuth("ci", "sindri_other") }},
		{"UnknownJWT", func(r *http.Request) { r.Header.Set("Authorization", "Bearer x.y.z") }},
		{"EmptyBearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }},
		{"BasicWithoutPassword", func(r *http.Request) { r.SetBasicAuth("boot-token", "") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newChain().Authenticate(t.Context(), request(t, tc.set)); !errors.Is(err, auth.ErrUnauthorized) {
				t.Fatalf("want ErrUnauthorized, got %v", err)
			}
		})
	}
}

func TestChain_EmptyBootstrapFailsClosed(t *testing.T) {
	c := auth.NewChain("", nil, nil, nil)
	req := request(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") })
	if _, err := c.Authenticate(t.Context(), req); err == nil {
		t.Fatal("an empty bootstrap token authenticated a request")
	}
}

// Administrator groups are read from the token on each request, and a reload
// that changes them takes effect at once.
func TestChain_AdminGroupsAreLive(t *testing.T) {
	c := newChain()
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+jwtShaped) }
	p, err := c.Authenticate(t.Context(), request(t, bearer))
	if err != nil || p.Admin {
		t.Fatalf("before: %+v, %v", p, err)
	}
	c.SetAdmins([]string{"team:platform"})
	p, err = c.Authenticate(t.Context(), request(t, bearer))
	if err != nil || !p.Admin {
		t.Fatalf("after SetAdmins: %+v, %v", p, err)
	}
}

func TestMiddleware_StoresPrincipalOrDenies(t *testing.T) {
	var denied error
	var got auth.Principal
	h := auth.Middleware(newChain(), func(w http.ResponseWriter, _ *http.Request, err error) {
		denied = err
		w.WriteHeader(http.StatusUnauthorized)
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = auth.PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer boot-token") }))
	if rec.Code != http.StatusNoContent || got.Subject != "bootstrap" || !got.Admin {
		t.Fatalf("authenticated: %d %+v", rec.Code, got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, nil))
	if rec.Code != http.StatusUnauthorized || !errors.Is(denied, auth.ErrUnauthorized) {
		t.Fatalf("anonymous: %d %v", rec.Code, denied)
	}
}

func TestAuthorize(t *testing.T) {
	docker := model.Repository{ID: "1", Name: "docker", Access: model.Access{
		Read: []string{"team:payments"}, Write: []string{"team:platform"},
	}}
	open := model.Repository{ID: "2", Name: "pypi", Access: model.Access{Read: []string{auth.Authenticated}}}
	team := model.Repository{ID: "3", Name: "payments/internal", Namespace: "payments"}
	bare := model.Repository{ID: "4", Name: "legacy"}

	user := func(groups ...string) auth.Principal {
		return auth.Principal{Subject: "user:x", Kind: auth.KindUser, Groups: groups}
	}
	token := func(role auth.Role, repo, ns string) auth.Principal {
		return auth.Principal{Subject: "token:t", Kind: auth.KindMachine, Token: &auth.TokenScope{Role: role, Repository: repo, Namespace: ns}}
	}

	tests := []struct {
		name string
		p    auth.Principal
		repo model.Repository
		need auth.Role
		want bool
	}{
		{"ReaderGroupCanPull", user("team:payments"), docker, auth.RoleRead, true},
		{"ReaderGroupCannotPush", user("team:payments"), docker, auth.RoleWrite, false},
		{"WriterGroupCanPull", user("team:platform"), docker, auth.RoleRead, true},
		{"WriterGroupCanPush", user("team:platform"), docker, auth.RoleWrite, true},
		{"StrangerCannotPull", user("team:search"), docker, auth.RoleRead, false},
		{"AuthenticatedGrantCoversAnyUser", user(), open, auth.RoleRead, true},
		{"NoGrantsMeansAdminsOnly", user("team:platform"), bare, auth.RoleRead, false},
		{"AdminReadsAnything", auth.Principal{Subject: "bootstrap", Kind: auth.KindBootstrap, Admin: true}, bare, auth.RoleWrite, true},
		{"UserNeverAdministersARepository", user("team:platform"), docker, auth.RoleAdmin, false},
		{"RepoTokenReadsItsRepository", token(auth.RoleRead, "docker", ""), docker, auth.RoleRead, true},
		{"RepoTokenCannotWriteWithReadRole", token(auth.RoleRead, "docker", ""), docker, auth.RoleWrite, false},
		{"RepoTokenStaysInItsRepository", token(auth.RoleWrite, "docker", ""), open, auth.RoleRead, false},
		{"RepoTokenIgnoresAuthenticatedGrant", token(auth.RoleRead, "docker", ""), open, auth.RoleRead, false},
		{"NamespaceTokenCoversItsNamespace", token(auth.RoleWrite, "", "payments"), team, auth.RoleWrite, true},
		{"NamespaceTokenStaysInItsNamespace", token(auth.RoleWrite, "", "payments"), docker, auth.RoleRead, false},
		{"RepoTokenCannotCreateWhatDoesNotExist", token(auth.RoleWrite, "new", ""), model.Repository{Name: "new"}, auth.RoleWrite, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := auth.Authorize(tc.p, tc.repo, tc.need)
			if (err == nil) != tc.want {
				t.Fatalf("Authorize = %v, want allowed=%v", err, tc.want)
			}
			if err != nil && !errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("refusal does not wrap ErrForbidden: %v", err)
			}
		})
	}
}

func TestAuthorizeGlobal(t *testing.T) {
	if err := auth.AuthorizeGlobal(auth.Principal{Kind: auth.KindUser, Groups: []string{"x"}}, "create repositories"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("user: %v", err)
	}
	global := auth.Principal{Kind: auth.KindMachine, Token: &auth.TokenScope{Role: auth.RoleAdmin}}
	if err := auth.AuthorizeGlobal(global, "create repositories"); err != nil {
		t.Fatalf("global admin token: %v", err)
	}
	ns := auth.Principal{Kind: auth.KindMachine, Token: &auth.TokenScope{Role: auth.RoleAdmin, Namespace: "payments"}}
	if err := auth.AuthorizeGlobal(ns, "create repositories"); err == nil {
		t.Fatal("a namespace admin token administered the server")
	}
}
