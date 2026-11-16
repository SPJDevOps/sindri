//go:build integration

package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/spj/sindri/internal/gc"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
	"github.com/spj/sindri/internal/testsupport"
	"github.com/spj/sindri/internal/worker"
)

func TestWorkerLanes_GCCompletesWhileAScanJobIsRunning(t *testing.T) {
	q := queue.New(testsupport.MigratedDB(t))
	ctx := t.Context()

	scanStarted := make(chan struct{})
	releaseScan := make(chan struct{})
	var once sync.Once
	handlers := map[string]worker.Handler{
		scanjob.JobKind: func(ctx context.Context, _ queue.Job) (json.RawMessage, error) {
			once.Do(func() { close(scanStarted) })
			select {
			case <-releaseScan:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return json.RawMessage(`{}`), nil
		},
		gc.JobKind: func(context.Context, queue.Job) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}
	scanJob, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: scanjob.JobKind, CreatedBy: "t"})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	for _, lane := range workerLanes(worker.Options{Queue: q, Owner: "w", Handlers: handlers, Poll: 20 * time.Millisecond}) {
		wg.Go(func() { _ = worker.Run(runCtx, lane) })
	}

	select {
	case <-scanStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the scan job never started")
	}
	gcJob, _, err := q.Enqueue(ctx, queue.EnqueueParams{Kind: gc.JobKind, CreatedBy: "t"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := q.Get(ctx, gcJob.ID)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gc job state = %s while the scan runs, want succeeded", j.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	j, err := q.Get(ctx, scanJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != "running" {
		t.Errorf("scan job state = %s, want still running", j.State)
	}
	close(releaseScan)
}
