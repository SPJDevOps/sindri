# 11. The MinIO test image comes from Bitnami's archive

- **Status:** Accepted
- **Date:** 2026-09-25
- **Relates to:** [ADR 3, A content-addressable store with placement](0003-content-addressable-store-with-placement.md),
  [ADR 8, Staged uploads belong to the BlobStore](0008-staged-uploads-belong-to-the-blobstore.md)

## Context

`testcontainers-go` starts a real MinIO for the storage conformance suite, the
CAS suite, and `make up`. M1 moved that image to `quay.io` because MinIO's Docker
Hub repository had stopped serving anonymous pulls.

`quay.io` has since closed too. Both `quay.io/minio/minio` and Docker Hub's
`minio/minio` now answer an anonymous manifest request with `401`, for the pinned
tag and for `latest` alike, and quay's API refuses unauthenticated listing. CI
failed on `main` before the M3 pull request ran:

```
starting MinIO: run minio: generic container: create container:
Error response from daemon: unauthorized: access to the requested resource is not authorized
```

Only machines with a warm image cache were unaffected, which is why the failure
looked local to CI and why `make up` was quietly broken for a fresh clone.

The suite needs a server that implements what `s3store` actually calls: the full
multipart sequence (`CreateMultipartUpload`, `UploadPart`, `CompleteMultipartUpload`,
`AbortMultipartUpload`) plus `CopyObject`, `HeadBucket`, `HeadObject`, and the
object verbs.

## Decision

**The image is `bitnamilegacy/minio`, pinned to `2025.7.23-debian-12-r5`.** It is
the same MinIO server, it pulls anonymously, and it publishes `linux/amd64` and
`linux/arm64`, so Apple Silicon and CI runners agree. `make up` takes
`bitnamilegacy/minio-client` for the one-shot bucket create.

**`internal/testsupport` drives the container directly instead of through
testcontainers' minio module.** That module hardcodes `--cmd server /data`, which
the Bitnami entrypoint does not accept and whose data directory its non-root user
cannot write. The module gave us three lines: an exposed port, two environment
variables, and a wait strategy. Inlining them costs less than fighting it.

The pin stays a pin: a test failure remains a change in Sindri, never a change
upstream.

## Consequences

- Bitnami froze this archive on 2025-08-19 and does not support it. That is
  tolerable precisely because the reference is pinned — an unmaintained image
  cannot regress under a fixed tag, only disappear.
- If the archive is pruned, the fallback is mirroring the pinned image into a
  registry this project controls. Nothing about the suite changes in that case;
  only the constant in `internal/testsupport/minio.go` and the compose pins move.
- `make up` and the integration suite still need no credentials, so a clone still
  runs `make test-integration` with nothing but Docker.
- The compose volume moved from `/data` to `/bitnami/minio/data`. A developer
  with an existing `sindri-dev_minio-data` volume gets an empty bucket once and
  `minio-init` recreates it.

## Rejected

- **Authenticating to MinIO's registry in CI.** Keeps the exact image, but breaks
  clone-and-`make test-integration` for everyone without credentials, and the
  restriction that closed anonymous access is commercial, so a free account may
  not grant the pull at all.
- **Mirroring the pinned image into GHCR now.** Works, and stays the fallback,
  but it makes this project responsible for redistributing and retaining someone
  else's binary before that cost has to be paid.
- **Swapping MinIO for SeaweedFS, LocalStack, or a mock such as s3mock.** All
  pull freely. A mock is out on principle: CLAUDE.md says real Postgres and real
  MinIO, not fakes, and the storage suite's whole job is to catch what a fake
  would paper over. Replacing MinIO with another real server is a larger change
  than the failure justifies, and would be reconsidered on its own merits rather
  than under a broken build.
- **Unpinning to `latest`.** Makes every future upstream change a Sindri test
  failure, which is the thing the pin exists to prevent.
