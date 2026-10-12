//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/testsupport"
	"github.com/spj/sindri/internal/worker"
)

const gcConfig = `
worker:
  gc:
    grace: 1s
# --- desired
repos:
  docker: { protocol: oci }
lifecycle:
  stages: [dev]
`

func fastWorker(h *serveHooks) { h.build.WorkerPoll = 20 * time.Millisecond }

// manifestDigest reads the digest the registry stored for a tag.
func (s *stack) manifestDigest(name, tag string) string {
	s.t.Helper()
	r := s.request(http.MethodHead, "/v2/"+name+"/manifests/"+tag, nil, bearer(bootstrapToken))
	if r.status != http.StatusOK {
		s.t.Fatalf("HEAD %s:%s: %d", name, tag, r.status)
	}
	return r.header.Get("Docker-Content-Digest")
}

func (s *stack) push(name, tag, seed string) string {
	s.t.Helper()
	if status, body := s.pushImage(name, tag, seed, bearer(bootstrapToken)); status != http.StatusCreated {
		s.t.Fatalf("push %s:%s: %d %s", name, tag, status, body)
	}
	return s.manifestDigest(name, tag)
}

func (s *stack) untag(name, tag string) {
	s.t.Helper()
	if r := s.request(http.MethodDelete, "/v2/"+name+"/manifests/"+tag, nil, bearer(bootstrapToken)); r.status != http.StatusAccepted {
		s.t.Fatalf("DELETE %s:%s: %d %s", name, tag, r.status, r.body)
	}
}

func (s *stack) job(id int64) queue.Job {
	s.t.Helper()
	r := s.request(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", id), nil, bearer(bootstrapToken))
	if r.status != http.StatusOK {
		s.t.Fatalf("GET job %d: %d %s", id, r.status, r.body)
	}
	var j struct {
		ID     int64           `json:"id"`
		State  string          `json:"state"`
		Error  string          `json:"last_error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(r.body, &j); err != nil {
		s.t.Fatal(err)
	}
	return queue.Job{ID: j.ID, State: queue.State(j.State), LastError: j.Error, Result: j.Result}
}

func (s *stack) startGC() int64 {
	s.t.Helper()
	r := s.request(http.MethodPost, "/api/v1/gc", nil, bearer(bootstrapToken))
	if r.status != http.StatusAccepted {
		s.t.Fatalf("POST /gc: %d %s", r.status, r.body)
	}
	var j struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.body, &j); err != nil {
		s.t.Fatal(err)
	}
	return j.ID
}

// The M8a gate, through the real wiring: a collection requested over the API
// and run by the worker inside serve deletes the untagged, unpromoted image
// and keeps the tagged one and the promoted one, which still pull.
func TestServe_GCCollectsUntaggedKeepsTaggedAndPromoted(t *testing.T) {
	s := startServe(t, testsupport.DatabaseURL(t), gcConfig, fastWorker)
	const image = "docker/demo/app"

	tagged := s.push(image, "1.0", "tagged")
	promoted := s.push(image, "2.0", "promoted")
	r := s.request(http.MethodPost, "/api/v1/subjects/"+promoted+"/promote",
		strings.NewReader(`{"repository":"docker","stage":"dev","reason":"gate"}`), bearer(bootstrapToken))
	if r.status != http.StatusCreated {
		t.Fatalf("promote: %d %s", r.status, r.body)
	}
	s.untag(image, "2.0")
	doomed := s.push(image, "tmp", "doomed")
	s.untag(image, "tmp")
	doomedLayer := sha([]byte("layer for doomed"))
	keptLayer := sha([]byte("layer for tagged"))

	time.Sleep(1200 * time.Millisecond) // past the 1s grace period

	dry := s.request(http.MethodGet, "/api/v1/gc/candidates", nil, bearer(bootstrapToken))
	for _, want := range []string{doomed, doomedLayer} {
		if !bytes.Contains(dry.body, []byte(want)) {
			t.Fatalf("dry run does not list %s: %s", want, dry.body)
		}
	}
	for _, kept := range []string{tagged, promoted, keptLayer} {
		if bytes.Contains(dry.body, []byte(kept)) {
			t.Fatalf("dry run lists %s, which is still referenced: %s", kept, dry.body)
		}
	}

	id := s.startGC()
	eventually(t, "the collection to finish", func() bool { return s.job(id).Done() })
	if j := s.job(id); j.State != queue.StateSucceeded {
		t.Fatalf("collection = %+v", j)
	}

	for ref, want := range map[string]int{tagged: http.StatusOK, promoted: http.StatusOK, doomed: http.StatusNotFound} {
		if r := s.request(http.MethodGet, "/v2/"+image+"/manifests/"+ref, nil, bearer(bootstrapToken)); r.status != want {
			t.Errorf("GET manifest %s: %d, want %d", ref, r.status, want)
		}
	}
	for layer, want := range map[string]int{keptLayer: http.StatusOK, doomedLayer: http.StatusNotFound} {
		if r := s.request(http.MethodGet, "/v2/"+image+"/blobs/"+layer, nil, bearer(bootstrapToken)); r.status != want {
			t.Errorf("GET blob %s: %d, want %d", layer, r.status, want)
		}
	}
}

func TestServe_WorkerDisabledLeavesJobsQueued(t *testing.T) {
	s := startServe(t, testsupport.DatabaseURL(t), "worker: false\n", fastWorker)
	id := s.startGC()
	time.Sleep(300 * time.Millisecond)
	if j := s.job(id); j.State != queue.StateQueued {
		t.Fatalf("job = %+v; with the worker off it must wait for a worker elsewhere", j)
	}
}

// Shutdown order: the worker hands its job back through the database, so it
// must stop before the pool closes. A job left running would wait out its
// lease before anything retried it.
func TestServe_WorkerStopsBeforeTheDatabaseCloses(t *testing.T) {
	url := testsupport.DatabaseURL(t)
	running := make(chan struct{})
	s := startServe(t, url, "", func(h *serveHooks) {
		fastWorker(h)
		h.build.ExtraHandlers = map[string]worker.Handler{
			// A job that takes a moment to wind down once cancelled, as a
			// collection finishing its current blob does. Without the
			// pause, cancellation reaches the worker before serve's
			// deferred cleanup runs and the order is never tested.
			"hold": func(ctx context.Context, _ queue.Job) (json.RawMessage, error) {
				close(running)
				<-ctx.Done()
				time.Sleep(500 * time.Millisecond)
				return nil, ctx.Err()
			},
		}
	})

	database, err := db.Open(t.Context(), testsupport.DBOptions(url))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(database.Close)
	q := queue.New(database)
	job, _, err := q.Enqueue(t.Context(), queue.EnqueueParams{Kind: "hold", CreatedBy: "test"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never started the job")
	}

	s.stop()

	got, err := q.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != queue.StateQueued || got.Attempts != 0 {
		t.Fatalf("job after shutdown = %+v, want it handed back", got)
	}
}
