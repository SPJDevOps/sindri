//go:build integration

package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/proto/oci"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
	"github.com/spj/sindri/internal/resolve"
	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

// TestLifecycle_PromotionStory is the M6 gate. Over HTTP only it walks the
// whole story: push, evidence, a promotion refused by its gate, a promotion
// that passes, an admission decision, and a revocation that keeps the
// approval in the ledger. Throughout, promotion moves no bytes (invariant I3):
// the only blobs that ever appear are the evidence documents themselves.
func TestLifecycle_PromotionStory(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")

	// 1. Push an image; it becomes a subject with an owner and provenance.
	img := pushTestImage(t, env.h, "demo", "1.0", "story", map[string]string{
		ocispec.AnnotationSource:   "https://git.example.com/team/demo",
		ocispec.AnnotationRevision: "4b825dc",
	}, nil)

	var subject struct {
		Digest     string   `json:"digest"`
		Kind       string   `json:"kind"`
		Owners     []string `json:"owners"`
		Provenance []struct {
			SourceType string `json:"source_type"`
			SourceRef  string `json:"source_ref"`
			BuildRef   string `json:"build_ref"`
		} `json:"provenance"`
	}
	env.getJSON(t, "/api/v1/subjects/"+img, &subject)
	if subject.Kind != "oci-manifest" {
		t.Errorf("kind = %q, want oci-manifest", subject.Kind)
	}
	if !slices.Equal(subject.Owners, []string{"repository:demo"}) {
		t.Errorf("owners = %v, want [repository:demo]", subject.Owners)
	}
	if len(subject.Provenance) != 1 ||
		subject.Provenance[0].SourceType != "oci-push" ||
		subject.Provenance[0].SourceRef != "https://git.example.com/team/demo" ||
		subject.Provenance[0].BuildRef != "4b825dc" {
		t.Fatalf("provenance = %+v", subject.Provenance)
	}

	env.setStages(t, `{"stages":[
		{"name":"dev","gates":[]},
		{"name":"integration","gates":[{"evidence":"sbom"}]},
		{"name":"prod","gates":[{"evidence":"sbom"},{"evidence":"signature"}]}]}`)

	base := env.blobCounts(t)

	// 2. Attach an SBOM: exactly one new blob, the document.
	env.attach(t, img, "sbom", "application/spdx+json", `{"spdxVersion":"SPDX-2.3"}`)
	afterSBOM := env.blobCounts(t)
	afterSBOM.assertDelta(t, base, 1, "attaching an SBOM")

	// 3. Promotion to prod needs SBOM and signature: refused, naming the gap.
	refused := env.do(t, http.MethodPost, "/api/v1/subjects/"+img+"/promote",
		`{"repository":"demo","stage":"prod","reason":"release 1.0"}`)
	if refused.StatusCode != http.StatusConflict {
		t.Fatalf("promote without signature: %d %s, want 409", refused.StatusCode, refused.Body)
	}
	var gate api.ErrorBody
	if err := json.Unmarshal([]byte(refused.Body), &gate); err != nil {
		t.Fatalf("decoding gate error: %v", err)
	}
	if gate.Code != "GATE_FAILED" || !slices.Equal(gate.Missing, []string{"signature"}) {
		t.Fatalf("gate error = %+v, want GATE_FAILED missing [signature]", gate)
	}
	env.blobCounts(t).assertDelta(t, afterSBOM, 0, "a refused promotion")

	// 4. Attach a signature, then promote.
	env.attach(t, img, "signature", "application/vnd.dev.cosign.simplesigning.v1+json", `{"critical":{}}`)
	afterSig := env.blobCounts(t)
	afterSig.assertDelta(t, afterSBOM, 1, "attaching a signature")

	promoted := env.do(t, http.MethodPost, "/api/v1/subjects/"+img+"/promote",
		`{"repository":"demo","stage":"prod","reason":"release 1.0"}`)
	if promoted.StatusCode != http.StatusCreated {
		t.Fatalf("promote: %d %s, want 201", promoted.StatusCode, promoted.Body)
	}
	env.blobCounts(t).assertDelta(t, afterSig, 0, "a promotion")

	// 5. The admission decision: approved for this digest, denied for another.
	if got := env.decision(t, img, "prod"); got.Decision != "approved" {
		t.Fatalf("decision after promote = %+v, want approved", got)
	}
	unrelated := digestOf([]byte("never pushed"))
	if got := env.decision(t, unrelated, "prod"); got.Decision != "denied" {
		t.Fatalf("decision for unrelated digest = %+v, want denied", got)
	}
	if got := env.decision(t, img, "integration"); got.Decision != "denied" {
		t.Fatalf("prod approval leaked into integration: %+v", got)
	}

	// 6. Revoke: denied again, and the approval is still in the ledger.
	revoked := env.do(t, http.MethodPost, "/api/v1/subjects/"+img+"/revoke",
		`{"repository":"demo","stage":"prod","reason":"CVE-2026-0001"}`)
	if revoked.StatusCode != http.StatusCreated {
		t.Fatalf("revoke: %d %s, want 201", revoked.StatusCode, revoked.Body)
	}
	if got := env.decision(t, img, "prod"); got.Decision != "denied" || got.Reason != "not_approved" {
		t.Fatalf("decision after revoke = %+v, want denied/not_approved", got)
	}
	var events struct {
		Items []struct {
			Action string `json:"action"`
			Stage  string `json:"stage"`
			Reason string `json:"reason"`
		} `json:"items"`
	}
	env.getJSON(t, "/api/v1/subjects/"+img+"/events", &events)
	if len(events.Items) != 2 ||
		events.Items[0].Action != "approve" || events.Items[0].Reason != "release 1.0" ||
		events.Items[1].Action != "revoke" || events.Items[1].Reason != "CVE-2026-0001" {
		t.Fatalf("ledger = %+v, want [approve, revoke]", events.Items)
	}

	// 7. Invariant I3: promotion, revocation, and decisions moved no bytes.
	env.blobCounts(t).assertDelta(t, afterSig, 0, "promote, decide, and revoke")
}

// TestLifecycle_EventsAreAppendOnly: the ledger refuses UPDATE, DELETE, and
// TRUNCATE in the database itself, so no code path can rewrite history.
func TestLifecycle_EventsAreAppendOnly(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")
	img := pushTestImage(t, env.h, "demo", "1.0", "ledger", nil, nil)
	env.setStages(t, `{"stages":[{"name":"dev","gates":[]}]}`)
	env.mustStatus(t, http.MethodPost, "/api/v1/subjects/"+img+"/promote",
		`{"repository":"demo","stage":"dev"}`, http.StatusCreated)

	tests := []struct {
		name string
		sql  string
	}{
		{name: "Update", sql: `UPDATE lifecycle_events SET action = 'revoke'`},
		{name: "Delete", sql: `DELETE FROM lifecycle_events`},
		{name: "Truncate", sql: `TRUNCATE lifecycle_events`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.database.Pool().Exec(t.Context(), tc.sql); err == nil {
				t.Fatalf("%s succeeded on the append-only ledger", tc.sql)
			}
		})
	}
	if got := env.decision(t, img, "dev"); got.Decision != "approved" {
		t.Fatalf("ledger changed: decision = %+v", got)
	}
}

func TestLifecycle_PromoteRequiresPlacementInRepository(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "team-a", "oci")
	createRepo(t, env.h, "team-b", "oci")
	img := pushTestImage(t, env.h, "team-a", "1.0", "placed", nil, nil)
	env.setStages(t, `{"stages":[{"name":"dev","gates":[]}]}`)

	res := env.do(t, http.MethodPost, "/api/v1/subjects/"+img+"/promote", `{"repository":"team-b","stage":"dev"}`)
	if res.StatusCode != http.StatusNotFound || !strings.Contains(res.Body, "NOT_PLACED") {
		t.Fatalf("promote in a repository that does not hold it: %d %s, want 404 NOT_PLACED", res.StatusCode, res.Body)
	}
}

func TestLifecycle_RefusesUnknownSubjectStageAndRepository(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")
	img := pushTestImage(t, env.h, "demo", "1.0", "refusals", nil, nil)
	env.setStages(t, `{"stages":[{"name":"dev","gates":[]}]}`)

	tests := []struct {
		name   string
		path   string
		body   string
		status int
		code   string
	}{
		{name: "UnknownSubject", path: "/api/v1/subjects/" + digestOf([]byte("nope")) + "/promote",
			body: `{"repository":"demo","stage":"dev"}`, status: http.StatusNotFound, code: "SUBJECT_NOT_FOUND"},
		{name: "UnknownStage", path: "/api/v1/subjects/" + img + "/promote",
			body: `{"repository":"demo","stage":"prod"}`, status: http.StatusBadRequest, code: "UNKNOWN_STAGE"},
		{name: "UnknownRepository", path: "/api/v1/subjects/" + img + "/promote",
			body: `{"repository":"ghost","stage":"dev"}`, status: http.StatusNotFound, code: "NOT_FOUND"},
		{name: "MalformedDigest", path: "/api/v1/subjects/sha256:xyz/promote",
			body: `{"repository":"demo","stage":"dev"}`, status: http.StatusBadRequest, code: "INVALID_DIGEST"},
		{name: "RevokeWithoutApproval", path: "/api/v1/subjects/" + img + "/revoke",
			body: `{"repository":"demo","stage":"dev"}`, status: http.StatusConflict, code: "NOT_APPROVED"},
		{name: "UnknownEvidenceType", path: "/api/v1/subjects/" + img + "/evidence?type=vibes",
			body: `{}`, status: http.StatusBadRequest, code: "INVALID_ARGUMENT"},
		{name: "EmptyEvidence", path: "/api/v1/subjects/" + img + "/evidence?type=sbom",
			body: ``, status: http.StatusBadRequest, code: "INVALID_ARGUMENT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := env.do(t, http.MethodPost, tc.path, tc.body)
			var body api.ErrorBody
			_ = json.Unmarshal([]byte(res.Body), &body)
			if res.StatusCode != tc.status || body.Code != tc.code {
				t.Fatalf("%d %s, want %d %s", res.StatusCode, res.Body, tc.status, tc.code)
			}
		})
	}
}

func TestLifecycle_StageWithHistoryCannotBeRemoved(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")
	img := pushTestImage(t, env.h, "demo", "1.0", "history", nil, nil)
	env.setStages(t, `{"stages":[{"name":"dev","gates":[]},{"name":"prod","gates":[]}]}`)
	env.mustStatus(t, http.MethodPost, "/api/v1/subjects/"+img+"/promote",
		`{"repository":"demo","stage":"dev"}`, http.StatusCreated)

	res := env.do(t, http.MethodPut, "/api/v1/lifecycle/stages", `{"stages":[{"name":"prod","gates":[]}]}`)
	if res.StatusCode != http.StatusConflict || !strings.Contains(res.Body, "STAGE_IN_USE") {
		t.Fatalf("removing a stage with history: %d %s, want 409 STAGE_IN_USE", res.StatusCode, res.Body)
	}

	// Reordering and regating a stage with history is fine; removing an unused one too.
	env.setStages(t, `{"stages":[{"name":"dev","gates":[{"evidence":"scan"}]}]}`)
	var stages struct {
		Stages []struct {
			Name  string `json:"name"`
			Gates []struct {
				Evidence string `json:"evidence"`
			} `json:"gates"`
		} `json:"stages"`
	}
	env.getJSON(t, "/api/v1/lifecycle/stages", &stages)
	if len(stages.Stages) != 1 || stages.Stages[0].Name != "dev" ||
		len(stages.Stages[0].Gates) != 1 || stages.Stages[0].Gates[0].Evidence != "scan" {
		t.Fatalf("stages = %+v", stages.Stages)
	}
}

func TestLifecycle_InvalidStageDefinitionsAreRejected(t *testing.T) {
	env := newIdentityEnv(t)
	tests := []struct {
		name string
		body string
	}{
		{name: "DuplicateStage", body: `{"stages":[{"name":"dev"},{"name":"dev"}]}`},
		{name: "UppercaseName", body: `{"stages":[{"name":"Prod"}]}`},
		{name: "EmptyName", body: `{"stages":[{"name":""}]}`},
		{name: "UnknownEvidence", body: `{"stages":[{"name":"prod","gates":[{"evidence":"vibes"}]}]}`},
		{name: "DuplicateGate", body: `{"stages":[{"name":"prod","gates":[{"evidence":"sbom"},{"evidence":"sbom"}]}]}`},
		{name: "UnknownRuleKey", body: `{"stages":[{"name":"prod","gates":[{"stage":"integration"}]}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := env.do(t, http.MethodPut, "/api/v1/lifecycle/stages", tc.body)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("%d %s, want 400", res.StatusCode, res.Body)
			}
		})
	}
}

func TestAPI_RepositoryWithLedgerHistoryCannotBeDeleted(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")
	createRepo(t, env.h, "scratch", "oci")
	img := pushTestImage(t, env.h, "demo", "1.0", "keep", nil, nil)
	env.setStages(t, `{"stages":[{"name":"dev","gates":[]}]}`)
	env.mustStatus(t, http.MethodPost, "/api/v1/subjects/"+img+"/promote",
		`{"repository":"demo","stage":"dev"}`, http.StatusCreated)

	res := env.do(t, http.MethodDelete, "/api/v1/repositories/demo", "")
	if res.StatusCode != http.StatusConflict || !strings.Contains(res.Body, "HAS_HISTORY") {
		t.Fatalf("deleting a repository with ledger history: %d %s, want 409 HAS_HISTORY", res.StatusCode, res.Body)
	}
	env.mustStatus(t, http.MethodDelete, "/api/v1/repositories/scratch", "", http.StatusNoContent)
}

func TestDecisions_UnknownEnvironmentIsDenied(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")
	img := pushTestImage(t, env.h, "demo", "1.0", "env", nil, nil)

	got := env.decision(t, img, "prdo")
	if got.Decision != "denied" || got.Reason != "unknown_environment" {
		t.Fatalf("decision = %+v, want denied/unknown_environment", got)
	}
	if res := env.do(t, http.MethodGet, "/api/v1/decisions?digest=nope&environment=prod", ""); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed digest: %d, want 400", res.StatusCode)
	}
}

// TestDecisions_UsesIndex: the decision query is on the hot path of every
// deployment, so it must be answerable from lifecycle_events_current_idx.
// Sequential scans are disabled so a tiny test table cannot hide a plan that
// would scan the ledger in production.
func TestDecisions_UsesIndex(t *testing.T) {
	env := newIdentityEnv(t)
	ctx := t.Context()
	conn, err := env.database.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("SET: %v", err)
	}
	rows, err := conn.Query(ctx, `EXPLAIN
		SELECT c.repo_id FROM lifecycle_current c
		WHERE c.subject_digest = 'sha256:00' AND c.stage_id = '00000000-0000-0000-0000-000000000000' AND c.action = 'approve'`)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !strings.Contains(plan.String(), "lifecycle_events_current_idx") {
		t.Fatalf("decision query does not use lifecycle_events_current_idx:\n%s", plan.String())
	}
}

func TestEvidence_ReadBackThroughSubjectOnly(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")
	img := pushTestImage(t, env.h, "demo", "1.0", "read", nil, nil)
	other := pushTestImage(t, env.h, "demo", "2.0", "other", nil, nil)

	doc := `{"bomFormat":"CycloneDX"}`
	id := env.attach(t, img, "sbom", "application/vnd.cyclonedx+json", doc)

	got := env.do(t, http.MethodGet, "/api/v1/subjects/"+img+"/evidence/"+id, "")
	if got.StatusCode != http.StatusOK || got.Body != doc {
		t.Fatalf("read evidence: %d %q", got.StatusCode, got.Body)
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/vnd.cyclonedx+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	// The same evidence id under another subject is not found.
	if cross := env.do(t, http.MethodGet, "/api/v1/subjects/"+other+"/evidence/"+id, ""); cross.StatusCode != http.StatusNotFound {
		t.Fatalf("evidence read via another subject: %d, want 404", cross.StatusCode)
	}

	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"items"`
	}
	env.getJSON(t, "/api/v1/subjects/"+img+"/evidence", &list)
	if len(list.Items) != 1 || list.Items[0].ID != id || list.Items[0].Type != "sbom" {
		t.Fatalf("evidence list = %+v", list.Items)
	}
}

// TestSubjects_HeadersOverrideAnnotations: a pipeline that states its build
// reference explicitly wins over what the image happens to be annotated with.
func TestSubjects_HeadersOverrideAnnotations(t *testing.T) {
	env := newIdentityEnv(t)
	createRepo(t, env.h, "demo", "oci")
	img := pushTestImage(t, env.h, "demo", "1.0", "override",
		map[string]string{ocispec.AnnotationRevision: "from-annotation"},
		map[string]string{"X-Sindri-Build-Ref": "ci/run/7"})

	var subject struct {
		Provenance []struct {
			BuildRef string `json:"build_ref"`
		} `json:"provenance"`
	}
	env.getJSON(t, "/api/v1/subjects/"+img, &subject)
	if len(subject.Provenance) != 1 || subject.Provenance[0].BuildRef != "ci/run/7" {
		t.Fatalf("provenance = %+v, want build_ref ci/run/7", subject.Provenance)
	}
}

type identityEnv struct {
	h        http.Handler
	blobs    *memstore.Store
	database *db.DB
}

func newIdentityEnv(t *testing.T) *identityEnv {
	t.Helper()
	url := testsupport.StartPostgres(t)
	opts := testsupport.DBOptions(url)
	if err := db.Migrate(t.Context(), opts, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	database, err := db.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(database.Close)

	blobs := memstore.New()
	casStore := cas.New(blobs)
	reg := registry.New(database, casStore, "memory")
	resolver := resolve.New(reg, remote.New(remote.Options{Registry: reg}))

	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: database,
		Blobs:    blobs,
		API: api.Handler(api.Options{
			Registry:  reg,
			Lifecycle: lifecycle.New(database),
			Evidence:  evidence.New(database, casStore, "memory"),
			Auth:      auth.NewStaticBearer(testToken),
			RequestID: server.RequestIDFromContext,
		}),
		OCI:    oci.Handler(oci.Options{Resolver: resolver, CAS: casStore}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return &identityEnv{h: srv.Handler(), blobs: blobs, database: database}
}

func (e *identityEnv) do(t *testing.T, method, path, body string) httpResult {
	t.Helper()
	var b []byte
	if body != "" || method == http.MethodPost {
		b = []byte(body)
	}
	return mustDo(t, e.h, method, path, b, "application/json")
}

func (e *identityEnv) mustStatus(t *testing.T, method, path, body string, status int) {
	t.Helper()
	if res := e.do(t, method, path, body); res.StatusCode != status {
		t.Fatalf("%s %s: %d %s, want %d", method, path, res.StatusCode, res.Body, status)
	}
}

func (e *identityEnv) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	res := e.do(t, http.MethodGet, path, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, res.StatusCode, res.Body)
	}
	if err := json.Unmarshal([]byte(res.Body), out); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

func (e *identityEnv) setStages(t *testing.T, body string) {
	t.Helper()
	e.mustStatus(t, http.MethodPut, "/api/v1/lifecycle/stages", body, http.StatusOK)
}

// attach posts an evidence document and returns its id.
func (e *identityEnv) attach(t *testing.T, subject, evType, mediaType, doc string) string {
	t.Helper()
	res := mustDo(t, e.h, http.MethodPost, "/api/v1/subjects/"+subject+"/evidence?type="+evType, []byte(doc), mediaType)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("attach %s: %d %s", evType, res.StatusCode, res.Body)
	}
	var ev struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.Body), &ev); err != nil {
		t.Fatalf("decoding evidence: %v", err)
	}
	return ev.ID
}

type decisionResult struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func (e *identityEnv) decision(t *testing.T, digest, environment string) decisionResult {
	t.Helper()
	var d decisionResult
	e.getJSON(t, "/api/v1/decisions?digest="+digest+"&environment="+environment, &d)
	return d
}

// blobCount is the number of blobs in storage and in the blobs table. Both
// are counted because invariant I3 is about bytes at rest, and a row without
// bytes (or bytes without a row) would each be a different bug.
type blobCount struct {
	stored, rows int
}

func (e *identityEnv) blobCounts(t *testing.T) blobCount {
	t.Helper()
	var rows int
	if err := e.database.Pool().QueryRow(t.Context(), "SELECT count(*) FROM blobs").Scan(&rows); err != nil {
		t.Fatalf("counting blobs: %v", err)
	}
	return blobCount{stored: e.blobs.Len(), rows: rows}
}

func (c blobCount) assertDelta(t *testing.T, before blobCount, want int, what string) {
	t.Helper()
	if c.stored-before.stored != want || c.rows-before.rows != want {
		t.Fatalf("%s changed blob counts by (stored %+d, rows %+d), want %+d",
			what, c.stored-before.stored, c.rows-before.rows, want)
	}
}

// pushTestImage pushes a one-layer image through /v2 and returns its manifest
// digest. seed makes the layer, and so the digest, unique per call.
func pushTestImage(t *testing.T, h http.Handler, repo, tag, seed string, annotations, headers map[string]string) string {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := []byte("layer for " + seed)
	for _, blob := range [][]byte{config, layer} {
		res := mustDo(t, h, http.MethodPost, "/v2/"+repo+"/blobs/uploads/?digest="+digestOf(blob), blob, "application/octet-stream")
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("blob upload: %d %s", res.StatusCode, res.Body)
		}
	}
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.Digest(digestOf(config)),
			Size:      int64(len(config)),
		},
		Layers: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    digest.Digest(digestOf(layer)),
			Size:      int64(len(layer)),
		}},
		Annotations: annotations,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/v2/"+repo+"/manifests/"+tag, bytes.NewReader(manifest))
	req.Header.Set("Content-Type", ocispec.MediaTypeImageManifest)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("manifest push: %d %s", rec.Code, rec.Body.String())
	}
	return digestOf(manifest)
}

func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
