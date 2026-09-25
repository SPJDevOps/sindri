# 10. OCI hosted adapter placement and anonymous /v2

- **Status:** Accepted
- **Date:** 2026-09-24
- **Relates to:** [ADR 3, A content-addressable store with placement](0003-content-addressable-store-with-placement.md),
  [ADR 8, Staged uploads belong to the BlobStore](0008-staged-uploads-belong-to-the-blobstore.md)

## Context

M3 adds the OCI distribution-spec adapter under `/v2`. Two questions land at the
same time: how a layer digest becomes readable under invariant I2 before any tag
exists, and whether `/v2` reuses the management API's static bearer for M3.

A docker push uploads blobs first and only then puts a manifest that tags them.
Between those steps clients HEAD and GET the blob. `CheckPlacement` that only
looks at `files` and `tags` cannot admit those digests. Walking `manifest_children`
from tags also fails, because there is no tag yet.

## Decision

**`repository_blobs(repo_id, digest)`** records that a digest was uploaded or
written as a manifest into a repository. `CheckPlacement` unions this table with
`files` and `tags`. `manifest_children` is still written on every manifest put so
garbage collection can walk the graph later; it is not the read gate for M3.

**`/v2` is unauthenticated in M3.** The management API keeps the static bearer.
M7 wires the existing `auth.Authenticator` seam onto protocol adapters; inventing
a second token scheme for docker login now would be throwaway work.

**Hosted OCI repositories are created on first write when absent**, matching every
other OCI registry's push UX and what distribution-spec conformance expects for
dynamically named mount destinations. Explicit `/api/v1` create remains valid.

Chunked upload sessions stay in process memory on `cas.Store` (same v1 limit ADR 8
recorded for S3 multipart state). `ResumeUpload` recovers the live digester across
HTTP requests on one `sindri serve` process.

Cross-repository blob mounts remain unimplemented: a `?mount=` POST falls back to
a fresh upload session (`202`), as the distribution specification allows.

## Consequences

- Cross-repository blob GETs still 404: placement is per repository.
- Content-management DELETE unlinks `repository_blobs` (or a tag); bytes remain
  for the future GC pass.
- Local docker and upstream conformance run without credentials against a
  development binary. Production auth is an explicit M7 deliverable.

## Rejected

- **Placing every uploaded blob as a `files` row under a synthetic package.**
  Works with the existing `CheckPlacement`, but pollutes the management API's
  package listing with OCI plumbing and confuses the PyPI naming model.
- **Requiring a tag before any blob is readable.** Breaks the push sequence and
  distribution-spec conformance.
- **Docker login with the static bearer in M3.** Possible, but conformance and
  the gate scripts then need credential plumbing that M7 will replace anyway.
- **Implementing cross-repository mounts in M3.** Out of scope; the spec's
  fallback-to-upload behaviour is enough for conformance.
