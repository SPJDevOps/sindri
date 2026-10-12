package cas_test

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/memstore"
)

func TestStore_ExpireIdleAbortsOnlyIdleSessions(t *testing.T) {
	blobs := memstore.New()
	store := cas.New(blobs)
	ctx := t.Context()

	idle, err := store.NewUpload(ctx)
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}
	if _, err := idle.Append(ctx, bytes.NewReader([]byte("abandoned"))); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if n := store.ExpireIdle(ctx, time.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("ExpireIdle with an old cutoff expired %d sessions", n)
	}
	if n := store.ExpireIdle(ctx, time.Now().Add(time.Minute)); n != 1 {
		t.Fatalf("ExpireIdle expired %d sessions, want 1", n)
	}
	if _, err := store.ResumeUpload(idle.ID()); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("ResumeUpload after expiry = %v, want ErrUploadNotFound", err)
	}
	if blobs.Uploads() != 0 {
		t.Fatalf("storage still holds %d staged uploads after expiry", blobs.Uploads())
	}
}

// blockingReader parks an Append until released, so a test can expire while
// the upload is mid-write.
type blockingReader struct {
	started chan struct{}
	release chan struct{}
	done    bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	close(r.started)
	<-r.release
	r.done = true
	return copy(p, "late bytes"), nil
}

func TestStore_ExpireIdleSkipsAnAppendInProgress(t *testing.T) {
	store := cas.New(memstore.New())
	ctx := t.Context()
	up, err := store.NewUpload(ctx)
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}

	r := &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
	appended := make(chan error, 1)
	go func() {
		_, err := up.Append(ctx, r)
		appended <- err
	}()
	<-r.started

	if n := store.ExpireIdle(ctx, time.Now().Add(time.Minute)); n != 0 {
		t.Fatalf("ExpireIdle expired %d sessions while one was appending", n)
	}
	close(r.release)
	if err := <-appended; err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.ResumeUpload(up.ID()); err != nil {
		t.Fatalf("ResumeUpload after an append that survived expiry: %v", err)
	}
}
