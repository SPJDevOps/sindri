package cas_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/memstore"
)

func TestPut_StoresBytesUnderTheirRealDigest(t *testing.T) {
	blobs := memstore.New()
	store := cas.New(blobs)
	payload := []byte("verified on the way in")

	info, err := store.Put(t.Context(), bytes.NewReader(payload), digestOf(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.Digest != digestOf(payload) {
		t.Fatalf("Put digest = %v, want %v", info.Digest, digestOf(payload))
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("Put size = %d, want %d", info.Size, len(payload))
	}

	rc, err := store.Get(t.Context(), info.Digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip = %q, want %q", got, payload)
	}
}

// A client that claims a digest the bytes do not have is the case this whole
// package exists to refuse. See ARCHITECTURE.md section 6 and CLAUDE.md's
// second security rule.
func TestPut_RejectsAClaimedDigestThatDoesNotMatchTheBytes(t *testing.T) {
	blobs := memstore.New()
	store := cas.New(blobs)

	payload := []byte("these are the bytes")
	claimed := digestOf([]byte("but this is the digest of something else"))

	_, err := store.Put(t.Context(), bytes.NewReader(payload), claimed, int64(len(payload)))
	if !errors.Is(err, cas.ErrDigestMismatch) {
		t.Fatalf("Put error = %v, want ErrDigestMismatch", err)
	}

	if _, err := store.Stat(t.Context(), claimed); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the claimed digest is readable after a mismatch: %v", err)
	}
	if _, err := store.Stat(t.Context(), digestOf(payload)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a rejected upload was committed under its real digest: %v", err)
	}
	if blobs.Len() != 0 {
		t.Fatalf("storage holds %d blobs after a rejected upload, want 0", blobs.Len())
	}
	if blobs.Uploads() != 0 {
		t.Fatalf("storage holds %d staged uploads after a rejected upload, want 0", blobs.Uploads())
	}
}

func TestPut_RejectsATruncatedUpload(t *testing.T) {
	blobs := memstore.New()
	store := cas.New(blobs)
	payload := []byte("shorter than claimed")

	_, err := store.Put(t.Context(), bytes.NewReader(payload), model.Digest{}, int64(len(payload))+64)
	if !errors.Is(err, cas.ErrSizeMismatch) {
		t.Fatalf("Put error = %v, want ErrSizeMismatch", err)
	}
	if blobs.Len() != 0 {
		t.Fatalf("storage holds %d blobs after a truncated upload, want 0", blobs.Len())
	}
	if blobs.Uploads() != 0 {
		t.Fatalf("storage holds %d staged uploads after a truncated upload, want 0", blobs.Uploads())
	}
}

// An adapter that only learns the digest at the end of the stream still gets a
// digest computed from the bytes, never from a claim.
func TestPut_WithoutAClaimReportsTheComputedDigest(t *testing.T) {
	store := cas.New(memstore.New())
	payload := []byte("unclaimed")

	info, err := store.Put(t.Context(), bytes.NewReader(payload), model.Digest{}, -1)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if info.Digest != digestOf(payload) {
		t.Fatalf("Put digest = %v, want %v", info.Digest, digestOf(payload))
	}
}

// Invariant I1: one blob per digest, however many times it arrives.
func TestPut_DeduplicatesAndDrainsTheSecondUpload(t *testing.T) {
	blobs := memstore.New()
	store := cas.New(blobs)
	payload := []byte("pushed twice, stored once")

	for i := range 2 {
		r := &countingReader{Reader: bytes.NewReader(payload)}
		if _, err := store.Put(t.Context(), r, digestOf(payload), int64(len(payload))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		// The body must still be consumed on the deduplicated path, or the
		// client is left writing into a connection nobody is reading.
		if r.n != len(payload) {
			t.Fatalf("Put %d read %d bytes of the body, want %d", i, r.n, len(payload))
		}
	}

	if blobs.Len() != 1 {
		t.Fatalf("storage holds %d blobs, want 1", blobs.Len())
	}
}

func TestPutBytes_HashesWhatItWrites(t *testing.T) {
	blobs := memstore.New()
	store := cas.New(blobs)
	payload := []byte(`{"schemaVersion":2}`)

	info, err := store.PutBytes(t.Context(), payload)
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if info.Digest != digestOf(payload) {
		t.Fatalf("PutBytes digest = %v, want %v", info.Digest, digestOf(payload))
	}

	if _, err := store.PutBytes(t.Context(), payload); err != nil {
		t.Fatalf("second PutBytes: %v", err)
	}
	if blobs.Len() != 1 {
		t.Fatalf("storage holds %d blobs, want 1", blobs.Len())
	}
}

func TestUpload_AppendsAcrossCallsAndCommitsOnce(t *testing.T) {
	blobs := memstore.New()
	store := cas.New(blobs)
	chunks := []string{"chunked ", "like an ", "OCI push"}
	payload := []byte(strings.Join(chunks, ""))

	upload, err := store.NewUpload(t.Context())
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}

	var offset int64
	for _, chunk := range chunks {
		offset, err = upload.Append(t.Context(), strings.NewReader(chunk))
		if err != nil {
			t.Fatalf("Append(%q): %v", chunk, err)
		}
	}
	if offset != int64(len(payload)) {
		t.Fatalf("offset = %d, want %d", offset, len(payload))
	}
	if upload.Digest() != digestOf(payload) {
		t.Fatalf("running digest = %v, want %v", upload.Digest(), digestOf(payload))
	}

	info, err := upload.Commit(t.Context(), digestOf(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if info.Digest != digestOf(payload) {
		t.Fatalf("committed digest = %v, want %v", info.Digest, digestOf(payload))
	}

	if _, err := upload.Append(t.Context(), strings.NewReader("after the commit")); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("Append after Commit = %v, want ErrUploadNotFound", err)
	}
	if _, err := upload.Commit(t.Context(), digestOf(payload), int64(len(payload))); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("second Commit = %v, want ErrUploadNotFound", err)
	}
	if err := upload.Abort(t.Context()); err != nil {
		t.Fatalf("Abort after Commit = %v, want nil", err)
	}
}

func TestUpload_AbortDiscardsStagedBytes(t *testing.T) {
	blobs := memstore.New()
	store := cas.New(blobs)
	payload := []byte("abandoned")

	upload, err := store.NewUpload(t.Context())
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}
	if _, err := upload.Append(t.Context(), bytes.NewReader(payload)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := upload.Abort(t.Context()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := upload.Abort(t.Context()); err != nil {
		t.Fatalf("second Abort: %v, want nil", err)
	}

	if blobs.Len() != 0 || blobs.Uploads() != 0 {
		t.Fatalf("after abort: %d blobs, %d uploads, want 0 and 0", blobs.Len(), blobs.Uploads())
	}
}

func TestStore_RejectsTheZeroDigestOnEveryReadPath(t *testing.T) {
	store := cas.New(memstore.New())

	if _, err := store.Get(t.Context(), model.Digest{}); !errors.Is(err, storage.ErrInvalidDigest) {
		t.Fatalf("Get(zero) = %v, want ErrInvalidDigest", err)
	}
	if _, err := store.GetRange(t.Context(), model.Digest{}, 0, 1); !errors.Is(err, storage.ErrInvalidDigest) {
		t.Fatalf("GetRange(zero) = %v, want ErrInvalidDigest", err)
	}
	if _, err := store.Stat(t.Context(), model.Digest{}); !errors.Is(err, storage.ErrInvalidDigest) {
		t.Fatalf("Stat(zero) = %v, want ErrInvalidDigest", err)
	}
	if err := store.Delete(t.Context(), model.Digest{}); !errors.Is(err, storage.ErrInvalidDigest) {
		t.Fatalf("Delete(zero) = %v, want ErrInvalidDigest", err)
	}
}

func TestResumeUpload_AppendsAcrossCalls(t *testing.T) {
	store := cas.New(memstore.New())
	payload := []byte("hello chunked world")

	upload, err := store.NewUpload(t.Context())
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}
	id := upload.ID()

	if _, err := upload.Append(t.Context(), bytes.NewReader(payload[:5])); err != nil {
		t.Fatalf("Append head: %v", err)
	}

	resumed, err := store.ResumeUpload(id)
	if err != nil {
		t.Fatalf("ResumeUpload: %v", err)
	}
	if resumed.Size() != 5 {
		t.Fatalf("resumed size = %d, want 5", resumed.Size())
	}
	if _, err := resumed.Append(t.Context(), bytes.NewReader(payload[5:])); err != nil {
		t.Fatalf("Append tail: %v", err)
	}

	info, err := resumed.Commit(t.Context(), digestOf(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if info.Digest != digestOf(payload) {
		t.Fatalf("digest = %v, want %v", info.Digest, digestOf(payload))
	}
	if _, err := store.ResumeUpload(id); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("ResumeUpload after commit = %v, want ErrUploadNotFound", err)
	}
}

func TestResumeUpload_UnknownID(t *testing.T) {
	store := cas.New(memstore.New())
	if _, err := store.ResumeUpload(storage.UploadID("0123456789abcdef0123456789abcdef")); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("ResumeUpload unknown = %v, want ErrUploadNotFound", err)
	}
}

type countingReader struct {
	io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += n
	return n, err //nolint:wrapcheck // a pass-through test reader
}

func digestOf(payload []byte) model.Digest {
	d := model.NewDigester(model.AlgorithmSHA256)
	_, _ = d.Write(payload)
	return d.Digest()
}
