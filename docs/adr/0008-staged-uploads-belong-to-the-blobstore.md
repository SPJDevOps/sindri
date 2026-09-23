# 8. Staged uploads belong to the BlobStore

- **Status:** Accepted
- **Date:** 2026-09-21
- **Relates to:** [ADR 3, A content-addressable store with placement](0003-content-addressable-store-with-placement.md)

## Context

A blob's storage key is the hash of its bytes, which the server may only learn after it
has received all of them. That leaves a window between "bytes are arriving" and "the
digest is verified", and where those bytes live during that window decides whether an
unverified payload can ever occupy a content-addressed key.

ARCHITECTURE.md section 6 originally gave `BlobStore` five methods, all keyed by digest:
`Put`, `Get`, `GetRange`, `Stat`, `Delete`. There was no way to express "write these bytes
somewhere that is not yet a content address", so the staging step had nowhere to live
inside the storage abstraction.

Two further facts shaped the decision. OCI chunked push (M3) appends to one upload across
several HTTP requests, so staging state outlives a single request. And S3 can assemble a
multipart upload and copy it server side, so a commit need not move bytes through Sindri's
process at all.

## Decision

`BlobStore` gains four methods, and staging becomes a storage concern:

```go
CreateUpload(ctx) (UploadID, error)
Append(ctx, id UploadID, r io.Reader) (offset int64, err error)
CommitUpload(ctx, id UploadID, digest Digest) error
AbortUpload(ctx, id UploadID) error
```

`internal/cas` is the only caller. It hashes what it appended, compares that against the
digest the client claimed, and calls `CommitUpload` only on a match; a mismatch calls
`AbortUpload`. `Put` remains for payloads already in memory and already hashed, such as
manifests and evidence documents.

The filesystem backend stages to `staging/<id>` and commits with `os.Rename`, which is
atomic within a filesystem. The S3 backend accumulates multipart parts under a staging key
and commits with `CompleteMultipartUpload` followed by a server-side `CopyObject`; an
upload too small to have flushed a part is written straight to its content-addressed key
at commit, which is safe for the same reason `Put` is.

## Consequences

- No unverified byte is ever reachable at a content-addressed key, on either backend. The
  conformance suite asserts it, and `internal/cas` asserts that a rejected upload leaves
  neither a blob nor a staging object behind.
- Committing a multi-gigabyte layer moves no bytes through Sindri on either backend: a
  rename on a filesystem, a server-side copy on S3.
- The interface is four methods wider, and every future backend implements them. That is
  the cost, and the shared conformance suite is what keeps it from becoming four methods
  that behave differently per backend.
- M3's chunked upload has a home: `Append` already returns a running offset, which is what
  the OCI `Range` response header reports.
- **Known limit.** S3 upload sessions are held in process memory, so an upload cannot be
  resumed by a different instance. This is invisible in v1, where `sindri serve` is one
  process, and it must be revisited when the post-v1 split serve modes arrive. The fix is
  known: recover the multipart upload by listing in-progress uploads for the staging key.
  That is deliberately not built before something needs it.

## Rejected

- **Staging to a local temporary file inside `cas`.** The simplest option, and the one
  that keeps the interface at five methods. Rejected because every S3 upload would be
  written to local disk and then re-uploaded, doubling I/O and requiring local disk sized
  for the largest artifact; and because an upload that spans several requests would have
  no correct home on a host that is not guaranteed to be the same one next time.
- **Putting bytes at the content-addressed key and deleting on a mismatch.** A window in
  which a partially uploaded blob is readable under a digest that is not its own. That is
  a supply-chain vulnerability, not a race to be narrowed.
- **A separate `StagingStore` interface.** Keeps `BlobStore` unchanged, but then `cas`
  branches on whether a backend supports staging, and a backend without it silently takes
  a different, weaker write path. One interface, one conformance suite, no branch.
