package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeGCServer answers the gc and jobs routes the way the server does, so the
// command's behaviour is tested without a database. The real routes are
// covered end to end by TestServe_GCCollectsUntaggedKeepsTaggedAndPromoted.
func fakeGCServer(t *testing.T, finalState string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/gc/candidates", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"items":[{"digest":"sha256:aaaa","size":1024,"last_seen_at":"2026-10-01T00:00:00Z"}],"next_cursor":"sha256:aaaa"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"digest":"sha256:bbbb","size":2048,"last_seen_at":"2026-10-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("POST /api/v1/gc", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v1/jobs/7")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":7,"kind":"gc","state":"queued"}`))
	})
	mux.HandleFunc("GET /api/v1/jobs/7", func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) < 3 {
			_, _ = w.Write([]byte(`{"id":7,"kind":"gc","state":"running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":7,"kind":"gc","state":"` + finalState + `","last_error":"storage unavailable",` +
			`"result":{"candidates":2,"deleted":2,"bytes_freed":3072,"skipped_recent":0,"skipped_referenced":0,"skipped_locked":0,"errors":0}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls
}

func runGC(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("SINDRI_TOKEN", "secret")
	cmd := newGCCmd()
	cmd.SetArgs(append([]string{"--server", server, "--poll", "1ms"}, args...))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestGCCommand_DryRunPrintsEveryCandidatePage(t *testing.T) {
	srv, polls := fakeGCServer(t, "succeeded")
	out, err := runGC(t, srv.URL, "--dry-run")
	if err != nil {
		t.Fatalf("gc --dry-run: %v\n%s", err, out)
	}
	for _, want := range []string{"sha256:aaaa", "sha256:bbbb", "2 blobs, 3.0 KiB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if polls.Load() != 0 {
		t.Fatal("a dry run started a collection")
	}
}

func TestGCCommand_WaitsForTheJobAndPrintsItsResult(t *testing.T) {
	srv, polls := fakeGCServer(t, "succeeded")
	out, err := runGC(t, srv.URL)
	if err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	if polls.Load() < 3 {
		t.Fatalf("gc returned after %d polls, before the job finished", polls.Load())
	}
	if !strings.Contains(out, "deleted 2 blobs") {
		t.Fatalf("output lacks the result:\n%s", out)
	}
}

func TestGCCommand_FailsWhenTheJobFails(t *testing.T) {
	srv, _ := fakeGCServer(t, "failed")
	out, err := runGC(t, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "storage unavailable") {
		t.Fatalf("gc = %v, want the job's error\n%s", err, out)
	}
}

func TestGCCommand_ReportsARefusal(t *testing.T) {
	srv, _ := fakeGCServer(t, "succeeded")
	t.Setenv("SINDRI_TOKEN", "wrong")
	cmd := newGCCmd()
	cmd.SetArgs([]string{"--server", srv.URL, "--dry-run"})
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("gc with a bad token = %v, want the 401 reported", err)
	}
}
