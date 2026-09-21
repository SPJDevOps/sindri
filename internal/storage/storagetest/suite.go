// Package storagetest holds the conformance suite that every BlobStore
// implementation must pass.
//
// There is one suite, run once per backend. Two suites that drifted apart would
// defeat the point: "choosing a backend is configuration, never a code path"
// (ARCHITECTURE.md section 6) is only true while one set of assertions holds
// for all of them.
package storagetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage"
)

// NewStoreFunc returns a store backed by state private to one test.
type NewStoreFunc func(t *testing.T) storage.BlobStore

// partSizeCrossing is larger than the S3 backend's multipart part size, so that
// the suite exercises the multipart path rather than only single puts.
const partSizeCrossing = 10 << 20

// RunSuite runs every conformance case against the store newStore returns.
func RunSuite(t *testing.T, newStore NewStoreFunc) {
	t.Helper()

	cases := []struct {
		name string
		run  func(t *testing.T, s storage.BlobStore)
	}{
		{"PutGetRoundTripsBytesIdentically", testPutGetRoundTrip},
		{"PutEmptyBlobRoundTrips", testPutEmptyBlob},
		{"PutIsIdempotentForTheSameDigest", testPutDedup},
		{"StatReportsSizeAndDigest", testStat},
		{"StatOnAbsentBlobIsErrNotFound", testStatNotFound},
		{"GetOnAbsentBlobIsErrNotFound", testGetNotFound},
		{"GetRangeReadsFromOffset", testGetRangeOffsets},
		{"GetRangeOutsideBlobIsErrInvalidRange", testGetRangeInvalid},
		{"DeleteRemovesBlobAndIsIdempotent", testDelete},
		{"StagedUploadCommitsUnderItsDigest", testStagedUploadCommit},
		{"AppendReturnsARunningOffset", testAppendOffsets},
		{"AbortedUploadLeavesNothingBehind", testAbortUpload},
		{"UnknownUploadIsErrUploadNotFound", testUnknownUpload},
		{"ConcurrentPutsOfOneDigestAllSucceed", testConcurrentPut},
		{"LargeBlobRoundTripsThroughStaging", testLargeStagedBlob},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newStore(t))
		})
	}
}

func testPutGetRoundTrip(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := []byte("the bytes we actually received")
	digest := digestOf(payload)

	if err := s.Put(ctx, digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got := mustGet(t, ctx, s, digest); !bytes.Equal(got, payload) {
		t.Fatalf("Get returned %q, want %q", got, payload)
	}
}

func testPutEmptyBlob(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	digest := digestOf(nil)

	if err := s.Put(ctx, digest, bytes.NewReader(nil), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got := mustGet(t, ctx, s, digest); len(got) != 0 {
		t.Fatalf("Get returned %d bytes, want 0", len(got))
	}

	info, err := s.Stat(ctx, digest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 0 {
		t.Fatalf("Stat size = %d, want 0", info.Size)
	}
}

func testPutDedup(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := []byte("stored exactly once per digest")
	digest := digestOf(payload)

	for i := range 3 {
		if err := s.Put(ctx, digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	info, err := s.Stat(ctx, digest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("Stat size = %d, want %d", info.Size, len(payload))
	}
	if got := mustGet(t, ctx, s, digest); !bytes.Equal(got, payload) {
		t.Fatalf("Get after repeated Put returned %q", got)
	}
}

func testStat(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := bytes.Repeat([]byte("s"), 4096)
	digest := digestOf(payload)

	if err := s.Put(ctx, digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	info, err := s.Stat(ctx, digest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Digest != digest {
		t.Fatalf("Stat digest = %v, want %v", info.Digest, digest)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("Stat size = %d, want %d", info.Size, len(payload))
	}
}

func testStatNotFound(t *testing.T, s storage.BlobStore) {
	_, err := s.Stat(t.Context(), digestOf([]byte("never stored")))
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat error = %v, want ErrNotFound", err)
	}
}

func testGetNotFound(t *testing.T, s storage.BlobStore) {
	rc, err := s.Get(t.Context(), digestOf([]byte("also never stored")))
	if err == nil {
		_ = rc.Close()
		t.Fatal("Get on an absent blob succeeded")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func testGetRangeOffsets(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := []byte("0123456789abcdef")
	digest := digestOf(payload)

	if err := s.Put(ctx, digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	tests := []struct {
		name   string
		off    int64
		length int64
		want   string
	}{
		{name: "FromStart", off: 0, length: 4, want: "0123"},
		{name: "MidBlob", off: 6, length: 4, want: "6789"},
		{name: "ToEndWithNegativeLength", off: 10, length: -1, want: "abcdef"},
		{name: "LengthPastEndIsTruncated", off: 12, length: 1000, want: "cdef"},
		{name: "WholeBlob", off: 0, length: int64(len(payload)), want: string(payload)},
		{name: "ZeroLength", off: 4, length: 0, want: ""},
		{name: "LastByte", off: int64(len(payload)) - 1, length: 1, want: "f"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mustGetRange(t, ctx, s, digest, tc.off, tc.length)
			if string(got) != tc.want {
				t.Fatalf("GetRange(%d, %d) = %q, want %q", tc.off, tc.length, got, tc.want)
			}
		})
	}
}

func testGetRangeInvalid(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := []byte("sixteen bytes!!!")
	digest := digestOf(payload)

	if err := s.Put(ctx, digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	tests := []struct {
		name   string
		off    int64
		length int64
		absent bool
		want   error
	}{
		{name: "NegativeOffset", off: -1, length: 4, want: storage.ErrInvalidRange},
		{name: "OffsetAtEnd", off: int64(len(payload)), length: 1, want: storage.ErrInvalidRange},
		{name: "OffsetPastEnd", off: int64(len(payload)) + 10, length: 1, want: storage.ErrInvalidRange},
		{name: "AbsentBlob", off: 0, length: 1, absent: true, want: storage.ErrNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := digest
			if tc.absent {
				d = digestOf([]byte("absent"))
			}
			rc, err := s.GetRange(ctx, d, tc.off, tc.length)
			if err == nil {
				_ = rc.Close()
				t.Fatalf("GetRange(%d, %d) succeeded, want %v", tc.off, tc.length, tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("GetRange(%d, %d) error = %v, want %v", tc.off, tc.length, err, tc.want)
			}
		})
	}
}

func testDelete(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := []byte("deleted twice")
	digest := digestOf(payload)

	if err := s.Put(ctx, digest, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(ctx, digest); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, digest); err != nil {
		t.Fatalf("second Delete: %v, want nil", err)
	}
}

func testStagedUploadCommit(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	chunks := [][]byte{[]byte("staged "), []byte("in three "), []byte("writes")}
	payload := bytes.Join(chunks, nil)
	digest := digestOf(payload)

	id, err := s.CreateUpload(ctx)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	for i, chunk := range chunks {
		if _, err := s.Append(ctx, id, bytes.NewReader(chunk)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	if _, err := s.Stat(ctx, digest); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("blob is readable before CommitUpload: %v", err)
	}

	if err := s.CommitUpload(ctx, id, digest); err != nil {
		t.Fatalf("CommitUpload: %v", err)
	}
	if got := mustGet(t, ctx, s, digest); !bytes.Equal(got, payload) {
		t.Fatalf("committed blob = %q, want %q", got, payload)
	}
	if err := s.AbortUpload(ctx, id); err != nil {
		t.Fatalf("AbortUpload after commit: %v, want nil", err)
	}
}

func testAppendOffsets(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()

	id, err := s.CreateUpload(ctx)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	t.Cleanup(func() { _ = s.AbortUpload(context.WithoutCancel(ctx), id) })

	var want int64
	for _, chunk := range []string{"", "hello", " ", "world", ""} {
		want += int64(len(chunk))
		got, err := s.Append(ctx, id, bytes.NewReader([]byte(chunk)))
		if err != nil {
			t.Fatalf("Append(%q): %v", chunk, err)
		}
		if got != want {
			t.Fatalf("Append(%q) offset = %d, want %d", chunk, got, want)
		}
	}
}

func testAbortUpload(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := []byte("never committed")
	digest := digestOf(payload)

	id, err := s.CreateUpload(ctx)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, err := s.Append(ctx, id, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.AbortUpload(ctx, id); err != nil {
		t.Fatalf("AbortUpload: %v", err)
	}

	if _, err := s.Stat(ctx, digest); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat after abort = %v, want ErrNotFound", err)
	}
	if err := s.AbortUpload(ctx, id); err != nil {
		t.Fatalf("second AbortUpload: %v, want nil", err)
	}
	if _, err := s.Append(ctx, id, bytes.NewReader(payload)); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("Append after abort = %v, want ErrUploadNotFound", err)
	}
}

func testUnknownUpload(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	const unknown = storage.UploadID("0123456789abcdef0123456789abcdef")

	if _, err := s.Append(ctx, unknown, bytes.NewReader([]byte("x"))); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("Append = %v, want ErrUploadNotFound", err)
	}
	if err := s.CommitUpload(ctx, unknown, digestOf([]byte("x"))); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("CommitUpload = %v, want ErrUploadNotFound", err)
	}
	if err := s.AbortUpload(ctx, unknown); err != nil {
		t.Fatalf("AbortUpload on an unknown id = %v, want nil", err)
	}
}

func testConcurrentPut(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := bytes.Repeat([]byte("concurrent"), 512)
	digest := digestOf(payload)

	const writers = 8
	errs := make([]error, writers)

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			errs[i] = s.Put(ctx, digest, bytes.NewReader(payload), int64(len(payload)))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put %d: %v", i, err)
		}
	}
	if got := mustGet(t, ctx, s, digest); !bytes.Equal(got, payload) {
		t.Fatalf("blob is corrupt after concurrent writes: %d bytes", len(got))
	}
}

func testLargeStagedBlob(t *testing.T, s storage.BlobStore) {
	ctx := t.Context()
	payload := pseudoRandom(partSizeCrossing)
	digest := digestOf(payload)

	id, err := s.CreateUpload(ctx)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	// Two appends, the first past the multipart part size and the second far
	// below it, which is the shape S3 rejects if parts are flushed naively:
	// every part but the last must be at least 5 MiB.
	split := partSizeCrossing - 128
	if _, err := s.Append(ctx, id, bytes.NewReader(payload[:split])); err != nil {
		t.Fatalf("Append head: %v", err)
	}
	offset, err := s.Append(ctx, id, bytes.NewReader(payload[split:]))
	if err != nil {
		t.Fatalf("Append tail: %v", err)
	}
	if offset != int64(len(payload)) {
		t.Fatalf("offset = %d, want %d", offset, len(payload))
	}

	if err := s.CommitUpload(ctx, id, digest); err != nil {
		t.Fatalf("CommitUpload: %v", err)
	}

	info, err := s.Stat(ctx, digest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("Stat size = %d, want %d", info.Size, len(payload))
	}

	got := mustGetRange(t, ctx, s, digest, int64(split), -1)
	if !bytes.Equal(got, payload[split:]) {
		t.Fatalf("tail range is not byte identical: %d bytes", len(got))
	}
}

func digestOf(payload []byte) model.Digest {
	d := model.NewDigester(model.AlgorithmSHA256)
	_, _ = d.Write(payload)
	return d.Digest()
}

func mustGet(t *testing.T, ctx context.Context, s storage.BlobStore, digest model.Digest) []byte {
	t.Helper()
	rc, err := s.Get(ctx, digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return readAll(t, rc)
}

func mustGetRange(t *testing.T, ctx context.Context, s storage.BlobStore, digest model.Digest, off, length int64) []byte {
	t.Helper()
	rc, err := s.GetRange(ctx, digest, off, length)
	if err != nil {
		t.Fatalf("GetRange(%d, %d): %v", off, length, err)
	}
	return readAll(t, rc)
}

func readAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer func() {
		if cerr := rc.Close(); cerr != nil {
			t.Errorf("closing blob: %v", cerr)
		}
	}()

	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading blob: %v", err)
	}
	return b
}

// pseudoRandom builds incompressible but reproducible test data.
func pseudoRandom(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(1)) //nolint:gosec // test fixture, not a secret
	_, _ = r.Read(b)
	return b
}
