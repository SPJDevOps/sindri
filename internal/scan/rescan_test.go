package scan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/queue"
	"github.com/spj/sindri/internal/scanjob"
)

type fakeRescanLister struct {
	rows []db.ListApprovedSubjectsForRescanRow
	err  error
}

func (f fakeRescanLister) ListApprovedSubjectsForRescan(context.Context) ([]db.ListApprovedSubjectsForRescanRow, error) {
	return f.rows, f.err
}

type fakeRescanQueue struct {
	got     []queue.EnqueueParams
	active  map[string]bool
	failFor string
}

func (f *fakeRescanQueue) Enqueue(_ context.Context, p queue.EnqueueParams) (queue.Job, bool, error) {
	if p.DedupeKey == f.failFor {
		return queue.Job{}, false, errors.New("queue is down")
	}
	f.got = append(f.got, p)
	return queue.Job{Kind: p.Kind}, !f.active[p.DedupeKey], nil
}

func TestRescanner_Handle(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	rows := []db.ListApprovedSubjectsForRescanRow{
		{Digest: good, Kind: "image", Repository: "team-a"},
		{Digest: "not-a-digest", Kind: "image", Repository: "team-a"},
		{Digest: other, Kind: "wheel", Repository: "py"},
	}
	tests := []struct {
		name     string
		lister   fakeRescanLister
		queue    *fakeRescanQueue
		want     RescanResult
		wantJobs int
		wantErr  string
	}{
		{name: "EnqueuesOneScanPerSubjectAndSkipsMalformedDigests", lister: fakeRescanLister{rows: rows}, queue: &fakeRescanQueue{}, want: RescanResult{Subjects: 3, Enqueued: 2}, wantJobs: 2},
		{name: "AnAlreadyActiveScanIsNotCountedOrAnError", lister: fakeRescanLister{rows: rows}, queue: &fakeRescanQueue{active: map[string]bool{scanjob.DedupeKey(good): true}}, want: RescanResult{Subjects: 3, Enqueued: 1}, wantJobs: 2},
		{name: "NoApprovedSubjectsEnqueuesNothing", lister: fakeRescanLister{}, queue: &fakeRescanQueue{}, want: RescanResult{}},
		{name: "AnEnqueueErrorFailsTheJob", lister: fakeRescanLister{rows: rows}, queue: &fakeRescanQueue{failFor: scanjob.DedupeKey(other)}, wantErr: "queue is down"},
		{name: "AListErrorFailsTheJob", lister: fakeRescanLister{err: errors.New("db is down")}, queue: &fakeRescanQueue{}, wantErr: "db is down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRescanner(tt.lister, tt.queue, slog.New(slog.NewTextHandler(io.Discard, nil)))
			out, err := r.Handle(t.Context(), queue.Job{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Handle error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			var got RescanResult
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("result %s: %v", out, err)
			}
			if got != tt.want {
				t.Errorf("result = %+v, want %+v", got, tt.want)
			}
			if len(tt.queue.got) != tt.wantJobs {
				t.Fatalf("enqueued %d jobs, want %d", len(tt.queue.got), tt.wantJobs)
			}
			for _, p := range tt.queue.got {
				var a scanjob.Args
				if err := json.Unmarshal(p.Args, &a); err != nil {
					t.Fatalf("args %s: %v", p.Args, err)
				}
				if p.Kind != scanjob.JobKind || p.CreatedBy != scanjob.Actor || p.DedupeKey != scanjob.DedupeKey(a.Digest) || a.Repository == "" || a.Kind == "" {
					t.Errorf("unexpected job %+v args %+v", p, a)
				}
			}
		})
	}
}
