//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
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

func TestScanSubject_QueuesAScanJob(t *testing.T) {
	e := newRouteEnv(t)
	status, body := e.do(t, "alice", http.MethodPost, "/subjects/"+e.subject+"/scan", "")
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
