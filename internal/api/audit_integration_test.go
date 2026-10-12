//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/gc"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

// principals maps a bearer token to who it is, so a test can act as a team
// member, a scoped machine token, or an administrator without an identity
// provider. The authorization under test is the real auth.Authorize.
type principals map[string]auth.Principal

func (p principals) Authenticate(_ context.Context, r *http.Request) (auth.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return auth.Principal{}, auth.ErrUnauthorized
	}
	who, ok := p[token]
	if !ok {
		return auth.Principal{}, errors.New("unknown test token")
	}
	return who, nil
}

var testPrincipals = principals{
	"admin": {Subject: "user:admin", Kind: auth.KindUser, Admin: true},
	"alice": {Subject: "user:alice", Kind: auth.KindUser, Groups: []string{"team:a"}},
	"bob":   {Subject: "user:bob", Kind: auth.KindUser, Groups: []string{"team:b"}},
	"tok-a": {Subject: "token:ci-a", Kind: auth.KindMachine, Token: &auth.TokenScope{Repository: "repo-a", Role: auth.RoleRead}},
}

type auditEnv struct {
	h   http.Handler
	db  *db.DB
	reg *registry.Registry
	lc  *lifecycle.Service
}

func newAuditEnv(t *testing.T) *auditEnv {
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
	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: database,
		Blobs:    blobs,
		API: api.Handler(api.Options{
			Registry:  reg,
			Lifecycle: lc,
			Evidence:  evidence.New(database, casStore, "memory"),
			Auth:      testPrincipals,
			RequestID: server.RequestIDFromContext,
			GC:        collector,
			Queue:     queue.New(database),
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	e := &auditEnv{h: srv.Handler(), db: database, reg: reg, lc: lc}

	for name, group := range map[string]string{"repo-a": "team:a", "repo-b": "team:b"} {
		if _, err := reg.CreateRepository(t.Context(), registry.CreateRepositoryParams{
			Name: name, Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI,
			Access: model.Access{Read: []string{group}, Write: []string{group}},
		}); err != nil {
			t.Fatalf("CreateRepository %s: %v", name, err)
		}
	}
	if _, err := lc.SetStages(t.Context(), []lifecycle.StageSpec{{Name: "dev"}, {Name: "prod"}}); err != nil {
		t.Fatalf("SetStages: %v", err)
	}
	return e
}

// push places a manifest under tag in repo and returns its digest.
func (e *auditEnv) push(t *testing.T, repo, tag string) model.Digest {
	t.Helper()
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Annotations: map[string]string{"seed": repo + tag}}
	man.SchemaVersion = 2
	body, _ := json.Marshal(man)
	placed, err := e.reg.PutManifest(t.Context(), repo, "app", tag, ocispec.MediaTypeImageManifest, body, model.Provenance{})
	if err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	return placed.Digest
}

func (e *auditEnv) promote(t *testing.T, d model.Digest, repo, stage, actor string) {
	t.Helper()
	if _, err := e.lc.Promote(t.Context(), lifecycle.ChangeParams{
		Digest: d, Repository: repo, Stage: stage, Actor: actor, Reason: "test",
	}); err != nil {
		t.Fatalf("Promote: %v", err)
	}
}

func (e *auditEnv) get(t *testing.T, token, path string) (int, string) {
	t.Helper()
	return e.do(t, token, http.MethodGet, path)
}

func (e *auditEnv) do(t *testing.T, token, method, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

type eventPage struct {
	Items []struct {
		ID         int64  `json:"id"`
		Digest     string `json:"digest"`
		Repository string `json:"repository"`
		Stage      string `json:"stage"`
		Action     string `json:"action"`
		Actor      string `json:"actor"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func (e *auditEnv) audit(t *testing.T, token, query string) eventPage {
	t.Helper()
	code, body := e.get(t, token, "/api/v1/audit"+query)
	if code != http.StatusOK {
		t.Fatalf("GET /audit%s as %s: %d %s", query, token, code, body)
	}
	var page eventPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decoding audit: %v", err)
	}
	return page
}

func repositoriesIn(p eventPage) []string {
	var out []string
	for _, ev := range p.Items {
		if !slices.Contains(out, ev.Repository) {
			out = append(out, ev.Repository)
		}
	}
	slices.Sort(out)
	return out
}

func TestAudit_NonAdminSeesOnlyEventsOfReadableRepositories(t *testing.T) {
	e := newAuditEnv(t)
	e.promote(t, e.push(t, "repo-a", "1"), "repo-a", "dev", "user:alice")
	e.promote(t, e.push(t, "repo-b", "1"), "repo-b", "dev", "user:bob")

	tests := []struct {
		token string
		want  []string
	}{
		{"admin", []string{"repo-a", "repo-b"}},
		{"alice", []string{"repo-a"}},
		{"bob", []string{"repo-b"}},
	}
	for _, tc := range tests {
		t.Run(tc.token, func(t *testing.T) {
			if got := repositoriesIn(e.audit(t, tc.token, "")); !slices.Equal(got, tc.want) {
				t.Fatalf("%s sees events in %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

func TestAudit_RepoScopedTokenSeesOnlyItsRepository(t *testing.T) {
	e := newAuditEnv(t)
	e.promote(t, e.push(t, "repo-a", "1"), "repo-a", "dev", "user:alice")
	e.promote(t, e.push(t, "repo-b", "1"), "repo-b", "dev", "user:bob")

	if got := repositoriesIn(e.audit(t, "tok-a", "")); !slices.Equal(got, []string{"repo-a"}) {
		t.Fatalf("a token scoped to repo-a sees events in %v", got)
	}
}

func TestAudit_RepositoryFilterOnUnreadableRepoIs404(t *testing.T) {
	e := newAuditEnv(t)
	e.promote(t, e.push(t, "repo-b", "1"), "repo-b", "dev", "user:bob")

	if code, body := e.get(t, "alice", "/api/v1/audit?repository=repo-b"); code != http.StatusNotFound {
		t.Fatalf("alice filtering on repo-b: %d %s, want 404 so the name is not disclosed", code, body)
	}
	if got := e.audit(t, "bob", "?repository=repo-b"); len(got.Items) != 1 {
		t.Fatalf("bob filtering on his own repository saw %d events, want 1", len(got.Items))
	}
}

func TestAudit_PaginatesNewestFirstWithStableCursor(t *testing.T) {
	e := newAuditEnv(t)
	d := e.push(t, "repo-a", "1")
	for i := range 25 {
		e.promote(t, d, "repo-a", "dev", fmt.Sprintf("user:%d", i))
	}

	var seen []int64
	cursor := ""
	for page := 0; ; page++ {
		q := "?limit=10"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		got := e.audit(t, "alice", q)
		for _, ev := range got.Items {
			seen = append(seen, ev.ID)
		}
		// Events appended mid-paging are newer than every cursor, so they
		// must not appear, and must not shift what is still to come.
		if page == 0 {
			e.promote(t, d, "repo-a", "dev", "user:late")
		}
		if got.NextCursor == "" {
			break
		}
		cursor = got.NextCursor
	}

	if len(seen) != 25 {
		t.Fatalf("paged through %d events, want the 25 that existed when paging began", len(seen))
	}
	if !slices.IsSortedFunc(seen, func(a, b int64) int { return int(b - a) }) {
		t.Fatalf("events not newest first: %v", seen)
	}
	if len(slices.Compact(slices.Clone(seen))) != len(seen) {
		t.Fatalf("an event appeared twice: %v", seen)
	}
}

func TestAudit_Filters(t *testing.T) {
	e := newAuditEnv(t)
	one := e.push(t, "repo-a", "1")
	two := e.push(t, "repo-a", "2")
	e.promote(t, one, "repo-a", "dev", "user:alice")
	e.promote(t, two, "repo-a", "prod", "user:carol")
	if _, err := e.lc.Revoke(t.Context(), lifecycle.ChangeParams{
		Digest: two, Repository: "repo-a", Stage: "prod", Actor: "user:carol", Reason: "test",
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"Digest", "?digest=" + two.String(), 2},
		{"Stage", "?stage=dev", 1},
		{"Actor", "?actor=user:carol", 2},
		{"Action", "?action=revoke", 1},
		{"Since", "?since=2000-01-01T00:00:00Z", 3},
		{"Until", "?until=2000-01-01T00:00:00Z", 0},
		{"Combined", "?actor=user:carol&action=approve", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.audit(t, "admin", tc.query); len(got.Items) != tc.want {
				t.Fatalf("audit%s returned %d events, want %d", tc.query, len(got.Items), tc.want)
			}
		})
	}
}

func TestAudit_RejectsMalformedCursorAndTimes(t *testing.T) {
	e := newAuditEnv(t)
	for _, q := range []string{
		"?cursor=abc", "?cursor=-4", "?since=yesterday", "?until=2026-13-01",
		"?digest=nope", "?action=delete", "?limit=0", "?limit=100000",
	} {
		if code, body := e.get(t, "admin", "/api/v1/audit"+q); code != http.StatusBadRequest {
			t.Errorf("audit%s: %d %s, want 400", q, code, body)
		}
	}
}

// Reading a subject through one owner does not entitle the caller to learn
// which other repositories it was promoted in.
func TestEvents_HidesEventsInUnreadableRepositories(t *testing.T) {
	e := newAuditEnv(t)
	d := e.push(t, "repo-a", "1")
	// The same manifest, pushed to repo-b as well: one subject, two owners.
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Annotations: map[string]string{"seed": "repo-a1"}}
	man.SchemaVersion = 2
	body, _ := json.Marshal(man)
	if _, err := e.reg.PutManifest(t.Context(), "repo-b", "app", "1", ocispec.MediaTypeImageManifest, body, model.Provenance{}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	e.promote(t, d, "repo-a", "dev", "user:alice")
	e.promote(t, d, "repo-b", "prod", "user:bob")

	code, raw := e.get(t, "alice", "/api/v1/subjects/"+d.String()+"/events")
	if code != http.StatusOK {
		t.Fatalf("events as alice: %d %s", code, raw)
	}
	var page eventPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatalf("decoding events: %v", err)
	}
	if got := repositoriesIn(page); !slices.Equal(got, []string{"repo-a"}) {
		t.Fatalf("alice sees events in %v, want only repo-a", got)
	}

	code, raw = e.get(t, "alice", "/api/v1/subjects/"+d.String())
	if code != http.StatusOK {
		t.Fatalf("subject as alice: %d %s", code, raw)
	}
	if strings.Contains(raw, "repo-b") {
		t.Fatalf("the subject view names repo-b to a caller who cannot read it: %s", raw)
	}
}

func TestAPI_GCAndJobsRequireAdmin(t *testing.T) {
	e := newAuditEnv(t)
	for _, token := range []string{"alice", "tok-a"} {
		for _, req := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/gc/candidates"},
			{http.MethodPost, "/api/v1/gc"},
			{http.MethodGet, "/api/v1/jobs/1"},
		} {
			if code, body := e.do(t, token, req.method, req.path); code != http.StatusForbidden {
				t.Errorf("%s %s as %s: %d %s, want 403", req.method, req.path, token, code, body)
			}
		}
	}
}

func TestAPI_GCEnqueueIsIdempotentWhileActive(t *testing.T) {
	e := newAuditEnv(t)
	start := func() (int64, string) {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/gc", nil)
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST /gc: %d %s", rec.Code, rec.Body)
		}
		var job struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatalf("decoding job: %v", err)
		}
		return job.ID, rec.Header().Get("Location")
	}

	first, loc := start()
	second, _ := start()
	if first != second {
		t.Fatalf("a second POST /gc while one is queued made job %d, want %d", second, first)
	}
	if loc != fmt.Sprintf("/api/v1/jobs/%d", first) {
		t.Fatalf("Location = %q", loc)
	}
}

func TestAPI_JobStatusReportsResult(t *testing.T) {
	e := newAuditEnv(t)
	q := queue.New(e.db)
	job, _, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: gc.JobKind, CreatedBy: "test"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, _, _ := q.Claim(t.Context(), "w", []string{gc.JobKind}, time.Minute)
	if err := q.Complete(t.Context(), claimed.ID, "w", json.RawMessage(`{"deleted":2}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	code, body := e.get(t, "admin", fmt.Sprintf("/api/v1/jobs/%d", job.ID))
	if code != http.StatusOK {
		t.Fatalf("GET job: %d %s", code, body)
	}
	var got struct {
		State  string `json:"state"`
		Result struct {
			Deleted int `json:"deleted"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding job: %v", err)
	}
	if got.State != "succeeded" || got.Result.Deleted != 2 {
		t.Fatalf("job = %s", body)
	}
	if code, _ := e.get(t, "admin", "/api/v1/jobs/999999"); code != http.StatusNotFound {
		t.Fatalf("unknown job: %d, want 404", code)
	}
}

func TestAPI_GCCandidatesListsWhatACollectionWouldDelete(t *testing.T) {
	e := newAuditEnv(t)
	d := e.push(t, "repo-a", "tmp")
	if err := e.reg.DeleteManifest(t.Context(), "repo-a", "app", "tmp"); err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if _, err := e.db.Pool().Exec(t.Context(), `UPDATE blobs SET last_seen_at = now() - interval '2 days'`); err != nil {
		t.Fatalf("aging: %v", err)
	}

	code, body := e.get(t, "admin", "/api/v1/gc/candidates")
	if code != http.StatusOK || !strings.Contains(body, d.String()) {
		t.Fatalf("GET /gc/candidates: %d %s, want it to list %s", code, body, d)
	}
}
