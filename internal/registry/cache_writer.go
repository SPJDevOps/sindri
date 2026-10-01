package registry

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

// errCacheWriteAborted is what the staging goroutine sees when the writer is
// abandoned before the upstream body finished.
var errCacheWriteAborted = errors.New("cache write aborted")

// CacheWriter stages upstream bytes as they stream past a client, so a large
// first pull is not serialised behind the write to storage. Nothing is
// committed until Commit verifies the digest; an abandoned or mismatched
// stream leaves no content-addressed key behind.
//
// Staging runs in its own goroutine behind a pipe because cas.Upload.Append
// consumes a reader, while the proxy produces bytes one client read at a time.
type CacheWriter struct {
	pw     *io.PipeWriter
	done   chan error
	commit func(context.Context, model.Digest, int64) (storage.BlobInfo, error)
	abort  func(context.Context)
	closed bool
}

// NewCacheWriter opens a staged upload for upstream bytes of unknown length.
func (r *Registry) NewCacheWriter(ctx context.Context) (*CacheWriter, error) {
	upload, err := r.cas.NewUpload(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening cache upload: %w", err)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := upload.Append(ctx, pr)
		// Unblock a writer still pushing if staging failed first.
		_ = pr.CloseWithError(err)
		done <- err
	}()
	return &CacheWriter{
		pw:     pw,
		done:   done,
		commit: upload.Commit,
		abort: func(ctx context.Context) {
			_ = upload.Abort(context.WithoutCancel(ctx))
		},
	}, nil
}

// Write stages p. A failed write means the cache copy is lost; the caller may
// keep serving its client and Abort.
func (w *CacheWriter) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

// Commit finishes staging and promotes the bytes only if they hash to
// expected and, when size is non-negative, have that length.
func (w *CacheWriter) Commit(ctx context.Context, expected model.Digest, size int64) (storage.BlobInfo, error) {
	if w.closed {
		return storage.BlobInfo{}, fmt.Errorf("committing cache write: %w", errCacheWriteAborted)
	}
	w.closed = true
	_ = w.pw.Close()
	if err := <-w.done; err != nil {
		w.abort(ctx)
		return storage.BlobInfo{}, fmt.Errorf("staging cached bytes: %w", err)
	}
	return w.commit(ctx, expected, size)
}

// Abort discards whatever was staged. Calling it after Commit is a no-op.
func (w *CacheWriter) Abort(ctx context.Context) {
	if w.closed {
		return
	}
	w.closed = true
	_ = w.pw.CloseWithError(errCacheWriteAborted)
	<-w.done
	w.abort(ctx)
}
