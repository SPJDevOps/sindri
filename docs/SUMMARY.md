# Summary

[Sindri](README.md)

# Using Sindri

- [Quickstart](quickstart.md)
- [Configuration reference](configuration.md)
- [Upgrading](upgrading.md)

# Decisions

- [Architecture decision records](adr/index.md)
  - [1. Record architecture decisions](adr/0001-record-architecture-decisions.md)
  - [2. Go, as a single binary with no runtime dependencies](adr/0002-go-single-binary.md)
  - [3. A content-addressable store with placement metadata](adr/0003-content-addressable-store-with-placement.md)
  - [4. Lifecycle as an append-only event ledger](adr/0004-lifecycle-as-append-only-ledger.md)
  - [5. Configuration as code with a reconciler](adr/0005-configuration-as-code.md)
  - [6. OCI and PyPI first, other protocols after v1](adr/0006-oci-and-pypi-first.md)
  - [7. A configuration surface that scales down: short form, long form, one reconciler](adr/0007-config-surface-short-and-long-form.md)
  - [8. Staged uploads belong to the BlobStore](adr/0008-staged-uploads-belong-to-the-blobstore.md)
  - [9. One configuration loader, built in M1](adr/0009-one-configuration-loader-from-m1.md)
  - [10. OCI hosted adapter placement and anonymous /v2](adr/0010-oci-hosted-adapter-placement.md)
  - [11. The MinIO test image comes from Bitnami's archive](adr/0011-minio-test-image-source.md)
  - [12. PyPI hosted adapter: placement, auth, and download paths](adr/0012-pypi-hosted-adapter.md)
  - [13. PyPI write-time validation, immutable files, and what the index says](adr/0013-pypi-write-time-validation-and-immutable-files.md)
  - [14. Proxy cache and virtual resolution](adr/0014-proxy-cache-and-virtual-resolution.md)
  - [15. Upstream credentials: source decided, wiring deferred to M7](adr/0015-upstream-credentials-source-deferred-to-m7.md)
  - [16. Lifecycle, gates, and evidence in v1](adr/0016-lifecycle-gates-and-evidence-in-v1.md)
  - [17. Config kinds, namespaces, and reconciliation](adr/0017-config-kinds-namespaces-and-reconciliation.md)
  - [18. Authentication and RBAC in v1](adr/0018-authentication-and-rbac-in-v1.md)
  - [19. Garbage collection: roots, write intent, and the sweep](adr/0019-garbage-collection-roots-write-intent-and-sweep.md)
  - [20. A queue table and a background worker](adr/0020-queue-table-and-background-worker.md)
  - [21. A read-only UI, and a quickstart with a development identity provider](adr/0021-read-only-ui-and-quickstart.md)
  - [22. After v1, serve polyglot app teams first](adr/0022-post-v1-direction-polyglot-teams-first.md)
  - [23. Scanning runs as a queue job, and scanning is open source](adr/0023-scanning-as-a-queue-job-and-the-open-core-line.md)
  - [24. The npm packument is derived, dist-tags are pointers, and virtual repositories do not merge](adr/0024-npm-packument-and-dist-tags.md)
  - [25. The announcement requires M10, a Helm chart, and the first half of M11](adr/0025-what-the-announcement-requires.md)
  - [26. The docs site is built by mdBook from plain markdown, and lychee checks its links](adr/0026-docs-site-generator.md)
  - [27. Upstream credentials may be a Bearer token](adr/0027-upstream-credentials-may-be-a-bearer-token.md)
