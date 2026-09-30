package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/storage"
)

// errClosedEarly is recorded when a consumer closes the body before upstream
// finished, which abandons the cache copy.
var errClosedEarly = errors.New("upstream body closed before EOF")

// cachingBody streams an upstream response to its consumer while a
// CacheWriter stages the same bytes. The consumer sees each byte as soon as
// upstream sends it; the cache commits only after EOF, and only when the bytes
// hash to the digest upstream promised.
//
// Verification is independent of caching. If staging fails the consumer still
// gets its bytes, and the next request fetches again. If verification fails
// the final Read returns an error, so the adapter can abort the response
// rather than finish it cleanly; OCI and pip clients also verify digests
// themselves, which covers bytes already sent.
type cachingBody struct {
	ctx         context.Context
	log         *slog.Logger
	body        io.ReadCloser
	cancel      context.CancelFunc
	idle        *time.Timer
	idleTimeout time.Duration
	digester    *model.Digester
	expected    model.Digest // zero when upstream promised none
	size        int64        // negative when upstream did not say
	cache       *registry.CacheWriter
	place       func(context.Context, storage.BlobInfo) error
	err         error // sticky: io.EOF after a clean finish
}

func (b *cachingBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.body.Read(p)
	if n > 0 {
		b.idle.Reset(b.idleTimeout)
		_, _ = b.digester.Write(p[:n])
		if b.cache != nil {
			if _, werr := b.cache.Write(p[:n]); werr != nil {
				b.log.WarnContext(b.ctx, "proxy cache staging failed; serving uncached", slog.Any("error", werr))
				b.abandon()
			}
		}
	}
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		b.err = b.finish()
		return n, b.err
	default:
		b.err = fmt.Errorf("%w: reading upstream body: %w", ErrUpstreamUnavailable, err)
		b.abandon()
		return n, b.err
	}
}

// finish verifies what upstream sent and, when it checks out, commits and
// places the cached copy. It returns io.EOF on success.
func (b *cachingBody) finish() error {
	actual := b.digester.Digest()
	if b.size >= 0 && b.digester.Size() != b.size {
		b.abandon()
		return fmt.Errorf("%w: upstream sent %d bytes, promised %d", ErrUpstreamUnavailable, b.digester.Size(), b.size)
	}
	if b.expected.Valid() && actual != b.expected {
		b.abandon()
		return fmt.Errorf("%w: upstream bytes hash to %s, want %s", ErrUpstreamUnavailable, actual, b.expected)
	}
	if b.cache == nil {
		return io.EOF
	}
	cache := b.cache
	b.cache = nil
	// The consumer may hang up right after the last byte; the cache copy is
	// verified and worth keeping regardless.
	ctx := context.WithoutCancel(b.ctx)
	info, err := cache.Commit(ctx, actual, b.digester.Size())
	if err != nil {
		b.log.WarnContext(ctx, "proxy cache commit failed", slog.String("digest", actual.String()), slog.Any("error", err))
		return io.EOF
	}
	if err := b.place(ctx, info); err != nil {
		b.log.WarnContext(ctx, "proxy cache placement failed", slog.String("digest", actual.String()), slog.Any("error", err))
	}
	return io.EOF
}

func (b *cachingBody) abandon() {
	if b.cache != nil {
		b.cache.Abort(b.ctx)
		b.cache = nil
	}
}

// Close releases the upstream connection. Closing before EOF discards the
// staged copy: a partial body is never committed.
func (b *cachingBody) Close() error {
	if b.err == nil {
		b.err = errClosedEarly
		b.abandon()
	}
	b.idle.Stop()
	b.cancel()
	if err := b.body.Close(); err != nil {
		return fmt.Errorf("closing upstream body: %w", err)
	}
	return nil
}
