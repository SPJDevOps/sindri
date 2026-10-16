//go:build integration

package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/gc"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/reconcile"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

// routeCase is one request against one /api/v1 route, and who it must refuse.
type routeCase struct {
	path string // under /api/v1, query included
	body string
	// as must be refused with status. tok-a (read on repo-a) proves a write
	// route refuses a reader with 403; bob (no access to repo-a) proves a read
	// route hides repo-a with 404; alice proves an admin route refuses a
	// non-admin with 403. An empty as means any authenticated caller may use
	// the route, and then bob's answer must not name repo-a.
	as     string
	status int
}

// Every route the management API registers must appear in this table with
// the refusal it owes. A new route without a row fails here, which is the
// point: a route that skips auth.Authorize is a security bug, not an omission
// (security rule 7, ADR 18). The handler is built with every optional part
// set, so no route is missing from the walk because its feature was off.
func TestAPI_EveryRouteHasAnAuthorizationCase(t *testing.T) {
	e := newRouteEnv(t)
	d := e.subject
	stages := `{"stages":[{"name":"dev","gates":[]},{"name":"prod","gates":[]}]}`
	change := `{"repository":"repo-a","stage":"dev","reason":"route walk"}`
	file := "/repositories/repo-a/packages/demo/versions/1.0/files/"

	cases := map[string]routeCase{
		"GET /repositories/":       {path: "/repositories"},
		"POST /repositories/":      {path: "/repositories", body: `{}`, as: "alice", status: http.StatusForbidden},
		"GET /repositories/{name}": {path: "/repositories/repo-a", as: "bob", status: http.StatusNotFound},
		// repo-c exists so that the admin's control request has something to
		// delete without removing what the other cases read.
		"DELETE /repositories/{name}":                                                     {path: "/repositories/repo-c", as: "alice", status: http.StatusForbidden},
		"GET /repositories/{repo}/packages":                                               {path: "/repositories/repo-a/packages", as: "bob", status: http.StatusNotFound},
		"GET /repositories/{repo}/packages/{name}/versions":                               {path: "/repositories/repo-a/packages/demo/versions", as: "bob", status: http.StatusNotFound},
		"GET /repositories/{repo}/packages/{name}/versions/{version}/files/{filename}":    {path: file + "demo.txt", as: "bob", status: http.StatusNotFound},
		"PUT /repositories/{repo}/packages/{name}/versions/{version}/files/{filename}":    {path: file + "new.txt", body: "new", as: "tok-a", status: http.StatusForbidden},
		"DELETE /repositories/{repo}/packages/{name}/versions/{version}/files/{filename}": {path: file + "doomed.txt", as: "tok-a", status: http.StatusForbidden},

		"GET /lifecycle/stages": {path: "/lifecycle/stages"},
		"PUT /lifecycle/stages": {path: "/lifecycle/stages", body: stages, as: "alice", status: http.StatusForbidden},

		"GET /subjects/{digest}/":              {path: "/subjects/" + d, as: "bob", status: http.StatusNotFound},
		"POST /subjects/{digest}/evidence":     {path: "/subjects/" + d + "/evidence?type=sbom", body: `{}`, as: "tok-a", status: http.StatusForbidden},
		"GET /subjects/{digest}/evidence":      {path: "/subjects/" + d + "/evidence", as: "bob", status: http.StatusNotFound},
		"GET /subjects/{digest}/evidence/{id}": {path: "/subjects/" + d + "/evidence/" + e.evidenceID, as: "bob", status: http.StatusNotFound},
		"POST /subjects/{digest}/promote":      {path: "/subjects/" + d + "/promote", body: change, as: "tok-a", status: http.StatusForbidden},
		"POST /subjects/{digest}/revoke":       {path: "/subjects/" + d + "/revoke", body: change, as: "tok-a", status: http.StatusForbidden},
		"GET /subjects/{digest}/events":        {path: "/subjects/" + d + "/events", as: "bob", status: http.StatusNotFound},

		// Admission controllers ask this question with whatever credential
		// they hold, so it is open to any authenticated caller (ADR 16).
		"GET /decisions": {path: "/decisions?digest=" + d + "&environment=dev"},
		"GET /audit":     {path: "/audit?repository=repo-a", as: "bob", status: http.StatusNotFound},

		"POST /config/diff":  {path: "/config/diff", body: `{}`, as: "alice", status: http.StatusForbidden},
		"POST /config/apply": {path: "/config/apply", body: `{}`, as: "alice", status: http.StatusForbidden},

		"GET /tokens":         {path: "/tokens", as: "alice", status: http.StatusForbidden},
		"POST /tokens":        {path: "/tokens", body: `{}`, as: "alice", status: http.StatusForbidden},
		"DELETE /tokens/{id}": {path: "/tokens/1", as: "alice", status: http.StatusForbidden},

		"GET /gc/candidates": {path: "/gc/candidates", as: "alice", status: http.StatusForbidden},
		"POST /gc":           {path: "/gc", body: `{}`, as: "alice", status: http.StatusForbidden},
		"GET /jobs/{id}":     {path: "/jobs/1", as: "alice", status: http.StatusForbidden},
	}

	var routes []string
	if err := chi.Walk(e.routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+route)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("no routes walked")
	}
	slices.Sort(routes) // the admin's control requests change state; keep the order fixed

	for _, route := range routes {
		c, ok := cases[route]
		if !ok {
			t.Errorf("route %s has no authorization case; add one to this table", route)
			continue
		}
		delete(cases, route)
		method, _, _ := strings.Cut(route, " ")

		// The case must exercise the route it is filed under, or a typo in
		// its path would test some other route and pass.
		rctx := chi.NewRouteContext()
		target, _, _ := strings.Cut(c.path, "?")
		// Walk reports a subrouter's index route as "/repositories/", and
		// Match reports the same route without the trailing slash.
		if !e.routes.Match(rctx, method, target) || method+" "+rctx.RoutePattern() != strings.TrimSuffix(route, "/") {
			t.Errorf("%s: case path %s routes to %q", route, c.path, rctx.RoutePattern())
			continue
		}

		if status, _ := e.do(t, "", method, c.path, c.body); status != http.StatusUnauthorized {
			t.Errorf("%s without a credential: %d, want 401", route, status)
		}
		if c.as == "" {
			if status, body := e.do(t, "bob", method, c.path, c.body); status == http.StatusUnauthorized || strings.Contains(body, "repo-a") {
				t.Errorf("%s as bob: %d, and must not name repo-a:\n%s", route, status, body)
			}
			continue
		}
		if status, body := e.do(t, c.as, method, c.path, c.body); status != c.status {
			t.Errorf("%s as %s: %d, want %d:\n%s", route, c.as, status, c.status, body)
		}
		// The same request from an administrator is not refused the same way,
		// so the refusal above came from authorization and not from a
		// malformed request.
		if status, body := e.do(t, "admin", method, c.path, c.body); status == c.status || status == http.StatusUnauthorized {
			t.Errorf("%s as admin: %d, the same as the refusal; the case does not isolate authorization:\n%s", route, status, body)
		}
	}
	for route := range cases {
		t.Errorf("case %s matches no registered route; remove it", route)
	}
}

type routeEnv struct {
	h          http.Handler // the whole server, as clients reach it
	routes     chi.Routes   // the /api/v1 router alone, for Walk and Match
	subject    string       // a manifest digest owned by repo-a
	evidenceID string       // evidence attached to subject
}

// newRouteEnv builds the management API with every optional part set, and
// gives repo-a (team:a) a manifest, files, evidence, and a dev approval.
// repo-b belongs to team:b; repo-c is empty and exists to be deleted.
func newRouteEnv(t *testing.T) *routeEnv {
	t.Helper()
	database := testsupport.MigratedDB(t)
	blobs := memstore.New()
	casStore := cas.New(blobs, cas.WithReserver(db.NewBlobReserver(database, "memory")))
	reg := registry.New(database, casStore, "memory")
	lc := lifecycle.New(database)
	collector, err := gc.New(gc.Options{Database: database, CAS: casStore})
	if err != nil {
		t.Fatalf("gc.New: %v", err)
	}
	noCredentials := func() map[string]struct{} { return nil }
	handler := api.Handler(api.Options{
		Registry:    reg,
		Lifecycle:   lc,
		Evidence:    evidence.New(database, casStore, "memory"),
		Auth:        testPrincipals,
		RequestID:   server.RequestIDFromContext,
		Credentials: noCredentials,
		Reconciler:  reconcile.New(database, reg, noCredentials),
		Tokens:      auth.NewTokenService(database),
		GC:          collector,
		Queue:       queue.New(database),
	})
	routes, ok := handler.(chi.Routes)
	if !ok {
		t.Fatalf("api.Handler returned %T, not a chi router", handler)
	}
	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: database,
		Blobs:    blobs,
		API:      handler,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	e := &routeEnv{h: srv.Handler(), routes: routes}

	for name, group := range map[string]string{"repo-a": "team:a", "repo-b": "team:b", "repo-c": "team:b"} {
		if _, err := reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
			Name: name, Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI,
			Access: model.Access{Read: []string{group}, Write: []string{group}},
		}); err != nil {
			t.Fatalf("CreateRepository %s: %v", name, err)
		}
	}
	for _, name := range []string{"demo.txt", "doomed.txt"} {
		if _, err := reg.Publish(t.Context(), registry.PublishParams{
			Repo: "repo-a", Package: "demo", Version: "1.0", Filename: name,
			Body: strings.NewReader(name), Size: int64(len(name)),
		}); err != nil {
			t.Fatalf("Publish %s: %v", name, err)
		}
	}
	if _, err := lc.SetStages(t.Context(), []lifecycle.StageSpec{{Name: "dev"}, {Name: "prod"}}); err != nil {
		t.Fatalf("SetStages: %v", err)
	}
	e.subject = (&auditEnv{reg: reg}).push(t, "repo-a", "1.0").String()

	status, body := e.do(t, "admin", http.MethodPost, "/subjects/"+e.subject+"/evidence?type=sbom", `{"bomFormat":"CycloneDX"}`)
	if status != http.StatusCreated {
		t.Fatalf("attaching evidence: %d %s", status, body)
	}
	_, rest, _ := strings.Cut(body, `"id":"`)
	e.evidenceID, _, _ = strings.Cut(rest, `"`)
	if e.evidenceID == "" {
		t.Fatalf("no evidence id in %s", body)
	}
	if status, body := e.do(t, "admin", http.MethodPost, "/subjects/"+e.subject+"/promote",
		`{"repository":"repo-a","stage":"dev","reason":"fixture"}`); status != http.StatusCreated {
		t.Fatalf("promoting: %d %s", status, body)
	}
	return e
}

// do sends one request under /api/v1 as token's principal; an empty token
// sends no credential at all.
func (e *routeEnv) do(t *testing.T, token, method, path, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, "/api/v1"+path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestDecisions_ListsOnlyApprovalsTheCallerMayRead(t *testing.T) {
	e := newRouteEnv(t)
	path := "/decisions?digest=" + e.subject + "&environment=dev"
	for _, tc := range []struct {
		name, token string
		wantRepoA   bool
	}{
		{name: "OutsiderGetsTheAnswerButNoApprovals", token: "bob"},
		{name: "ReaderSeesTheApproval", token: "alice", wantRepoA: true},
		{name: "AdminSeesTheApproval", token: "admin", wantRepoA: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := e.do(t, tc.token, http.MethodGet, path, "")
			if status != http.StatusOK || !strings.Contains(body, `"decision":"approved"`) {
				t.Fatalf("GET %s as %s: %d %s, want 200 approved", path, tc.token, status, body)
			}
			if got := strings.Contains(body, `"repository":"repo-a"`); got != tc.wantRepoA {
				t.Fatalf("approval from repo-a listed: %t, want %t:\n%s", got, tc.wantRepoA, body)
			}
		})
	}
}
