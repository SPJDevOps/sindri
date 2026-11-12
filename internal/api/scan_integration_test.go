//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spj/sindri/internal/api"
	"github.com/spj/sindri/internal/scanjob"
)

type scanJob struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	CreatedBy string `json:"created_by"`
}

type rawResponse struct {
	status   int
	body     string
	location string
}

// doRaw is do plus the Location header.
func (e *routeEnv) doRaw(t *testing.T, token, method, path, _ string) rawResponse {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, "/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rawResponse{status: rec.Code, body: rec.Body.String(), location: rec.Header().Get("Location")}
}

// unplaceFromRepoA removes every placement of the subject from repo-a, as
// DeleteManifest does; the subject_owners row stays until GC.
func (e *routeEnv) unplaceFromRepoA(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM tags WHERE target_digest = $1`,
		`DELETE FROM repository_blobs WHERE digest = $1`,
	} {
		if _, err := e.database.Pool().Exec(t.Context(), q, e.subject); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// addUnplacingOwner creates a hosted repository named name that team:a can
// write and records it as an owner of the subject without placing it.
func (e *routeEnv) addUnplacingOwner(t *testing.T, name string) {
	t.Helper()
	ctx := t.Context()
	if _, err := e.database.Pool().Exec(ctx,
		`INSERT INTO repositories (name, type, protocol, managed_by, config)
		 VALUES ($1, 'hosted', 'oci', 'api', '{"access":{"read":["team:a"],"write":["team:a"]}}'::jsonb)`, name); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	if _, err := e.database.Pool().Exec(ctx,
		`INSERT INTO subject_owners (digest, owner_ref) VALUES ($1, $2)`, e.subject, "repository:"+name); err != nil {
		t.Fatalf("owning: %v", err)
	}
}

func (e *routeEnv) scanArgsRepository(t *testing.T, jobID int64) string {
	t.Helper()
	var args string
	if err := e.database.Pool().QueryRow(t.Context(), `SELECT args::text FROM jobs WHERE id = $1`, jobID).Scan(&args); err != nil {
		t.Fatalf("reading job args: %v", err)
	}
	var got scanjob.Args
	if err := json.Unmarshal([]byte(args), &got); err != nil {
		t.Fatalf("decoding args %s: %v", args, err)
	}
	return got.Repository
}

func TestScanSubject_OwnerThatNoLongerPlacesTheSubjectIsSkipped(t *testing.T) {
	for _, as := range []string{"alice", "admin"} {
		t.Run(as, func(t *testing.T) {
			e := newRouteEnv(t)
			e.addUnplacingOwner(t, "repo-0") // sorts before repo-a
			status, body := e.do(t, as, http.MethodPost, "/subjects/"+e.subject+"/scan", "")
			if status != http.StatusAccepted {
				t.Fatalf("scan: %d %s, want 202", status, body)
			}
			var job scanJob
			if err := json.Unmarshal([]byte(body), &job); err != nil {
				t.Fatalf("decoding %s: %v", body, err)
			}
			if got := e.scanArgsRepository(t, job.ID); got != "repo-a" {
				t.Errorf("job repository = %q, want repo-a, the lowest owner that places the subject", got)
			}
		})
	}
}

func TestScanSubject_NoPlacingHostedOwnerAnswers409(t *testing.T) {
	for _, as := range []string{"alice", "admin"} {
		t.Run(as, func(t *testing.T) {
			e := newRouteEnv(t)
			e.unplaceFromRepoA(t)
			status, body := e.do(t, as, http.MethodPost, "/subjects/"+e.subject+"/scan", "")
			if status != http.StatusConflict {
				t.Fatalf("scan: %d %s, want 409", status, body)
			}
			var eb api.ErrorBody
			if err := json.Unmarshal([]byte(body), &eb); err != nil || eb.Code != "NO_HOSTED_REPOSITORY" {
				t.Errorf("body = %s (%v), want code NO_HOSTED_REPOSITORY", body, err)
			}
		})
	}
}

func TestScanSubject_NonAdminGetsNoJobInternalsAndNoLocation(t *testing.T) {
	tests := []struct {
		name     string
		as       string
		internal bool
	}{
		{name: "WriterSeesIdKindStateOnly", as: "alice", internal: false},
		{name: "AdminSeesTheFullJobAndLocation", as: "admin", internal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newRouteEnv(t)
			rec := e.doRaw(t, tt.as, http.MethodPost, "/subjects/"+e.subject+"/scan", "")
			if rec.status != http.StatusAccepted {
				t.Fatalf("scan: %d %s, want 202", rec.status, rec.body)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(rec.body), &fields); err != nil {
				t.Fatalf("decoding %s: %v", rec.body, err)
			}
			if tt.internal {
				if _, ok := fields["attempts"]; !ok {
					t.Errorf("admin body = %s, want the full job", rec.body)
				}
				if rec.location == "" {
					t.Error("admin response has no Location")
				}
				return
			}
			if len(fields) != 3 {
				t.Errorf("body = %s, want exactly id, kind, state", rec.body)
			}
			for _, k := range []string{"id", "kind", "state"} {
				if _, ok := fields[k]; !ok {
					t.Errorf("body = %s, missing %s", rec.body, k)
				}
			}
			if rec.location != "" {
				t.Errorf("Location = %q, want none for a non-admin", rec.location)
			}
		})
	}
}

func TestScanSubject_QueuesAScanJob(t *testing.T) {
	e := newRouteEnv(t)
	status, body := e.do(t, "admin", http.MethodPost, "/subjects/"+e.subject+"/scan", "")
	if status != http.StatusAccepted {
		t.Fatalf("scan: %d %s, want 202", status, body)
	}
	var job scanJob
	if err := json.Unmarshal([]byte(body), &job); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if job.Kind != scanjob.JobKind || job.State != "queued" || job.CreatedBy != scanjob.Actor {
		t.Errorf("job = %+v, want a queued %s job by %s", job, scanjob.JobKind, scanjob.Actor)
	}
	// The job is the one the jobs endpoint shows, and it names the owner.
	var args string
	if err := e.database.Pool().QueryRow(t.Context(), `SELECT args::text FROM jobs WHERE id = $1`, job.ID).Scan(&args); err != nil {
		t.Fatalf("reading job args: %v", err)
	}
	var got scanjob.Args
	if err := json.Unmarshal([]byte(args), &got); err != nil {
		t.Fatalf("decoding args %s: %v", args, err)
	}
	if got.Digest != e.subject || got.Repository != "repo-a" || got.Kind == "" {
		t.Errorf("args = %+v, want digest %s in repo-a", got, e.subject)
	}
}

func TestScanSubject_SecondCallWhileQueuedReturnsTheSameJob(t *testing.T) {
	e := newRouteEnv(t)
	var ids [2]int64
	for i := range ids {
		status, body := e.do(t, "alice", http.MethodPost, "/subjects/"+e.subject+"/scan", "")
		if status != http.StatusAccepted {
			t.Fatalf("scan %d: %d %s, want 202", i, status, body)
		}
		var job scanJob
		if err := json.Unmarshal([]byte(body), &job); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
		ids[i] = job.ID
	}
	if ids[0] != ids[1] {
		t.Errorf("job ids = %v, want the same job twice", ids)
	}
}

func TestScanSubject_ScanningOffAnswers409(t *testing.T) {
	e := newRouteEnvScanning(t, false)
	status, body := e.do(t, "alice", http.MethodPost, "/subjects/"+e.subject+"/scan", "")
	if status != http.StatusConflict {
		t.Fatalf("scan with scanning off: %d %s, want 409", status, body)
	}
	var eb api.ErrorBody
	if err := json.Unmarshal([]byte(body), &eb); err != nil || eb.Code != "SCANNING_DISABLED" {
		t.Errorf("body = %s (%v), want code SCANNING_DISABLED", body, err)
	}
}

func TestScanSubject_CallersWithoutWriteAreRefused(t *testing.T) {
	e := newRouteEnv(t)
	tests := []struct {
		name   string
		as     string
		status int
	}{
		{name: "ReaderOfTheOwnerIsToldItMayNotChangeIt", as: "tok-a", status: http.StatusForbidden},
		{name: "CallerWithNoAccessSeesNoSubject", as: "bob", status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if status, body := e.do(t, tt.as, http.MethodPost, "/subjects/"+e.subject+"/scan", ""); status != tt.status {
				t.Errorf("scan as %s: %d %s, want %d", tt.as, status, body, tt.status)
			}
		})
	}
}
