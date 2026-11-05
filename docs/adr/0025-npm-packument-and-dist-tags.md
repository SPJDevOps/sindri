# 25. npm packuments are derived views, dist-tags are package pointers, and virtual repos follow the name-claim rule

- **Status:** Accepted
- **Date:** 2026-10-19
- **Relates to:** [ADR 14](0014-proxy-cache-and-virtual-resolution.md), [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md), [ADR 17](0017-config-kinds-namespaces-and-reconciliation.md), [roadmap.md](../../roadmap.md) M9

## Context

The v1 roadmap places npm immediately after PyPI because npm is the one remaining protocol whose metadata is mutable in a way the earlier adapters are not. OCI and PyPI both map cleanly onto Sindri's identity model:

- OCI uses content-addressed manifests and indexes as subjects, with placement rows and append-only lifecycle history.
- PyPI exposes a simple index and JSON API built from immutable files and package metadata, and a publish is mostly a new version placement rather than a mutation of an existing document.

npm is different. A package's full metadata document, the packument, changes on every publish, and every tag move changes the package's visible identity (`latest`, `beta`, `next`, etc.) without changing the tarball or its digest. If Sindri treats the packument as a stored artifact like an OCI manifest or a blob, it creates a second class of mutable content that bypasses the subject/placement model and makes tag moves semantically ambiguous.

This is the design decision that must be fixed before the first npm code lands. It is not just a storage detail. It answers three questions:

- Is the packument stored or derived?
- Do dist-tags belong in the lifecycle ledger or in package metadata?
- How does a virtual repository build a merged packument without violating dependency confusion protections?

## Decision

### Packuments are derived, not stored

A packument is a projection over package rows, version rows, file rows, and tag rows. It is rebuilt on every read from the namespace and package tables, just as PyPI's simple index is a derived view of the rows, not a separately stored document.

The authoritative rows remain:

- `packages`: one row per repository + normalized package name
- `versions`: one row per package version
- `files`: one row per file in a version, pointing at the stored blob digest
- `tags` / `dist-tags`: the current package-level pointer map from tag name to version

A packument is therefore not a content-addressed subject and does not get its own blob row. It is not published as a stable artifact, because a single npm package may have a new packument for every publish and dist-tag update. A mutable, re-synthesized document is a view, not a subject.

### Dist-tags are package metadata pointers, not subjects or lifecycle states

`dist-tags` are not registry subjects and they do not participate in the subject ledger. A tag name like `latest` points at a version. Moving it from one version to another is a metadata mutation to the package, not a promotion of an artifact and not a state change of a subject digest.

We record tag moves as append-only package metadata events, separate from `lifecycle_events`:

- `npm_tag_events` (or the equivalent package metadata ledger): repo_id, package_id, tag, previous_version, new_version, actor, created_at
- the current tag state is the latest event per `(repo_id, package_id, tag)`

The event is auditable and supports rollback and debugging, but it does not become a subject lifecycle promotion. `latest` is not a trust state. A subject still has only the lifecycle states defined for trust and proof, and the tag pointer is orthogonal to those states.

This keeps the identity model honest:

- artifact identity stays in subjects and their placements
- package-level metadata stays in package/version/tag rows
- tag moves are a package history concern, not a subject trust concern

### Virtual repositories synthesize the packument from the first matching member

A virtual repository builds its packument by the same ordered member walk used for resolution, with the existing hosted-first name-claim rule:

1. Walk the virtual repository's members in order.
2. If a hosted member has already claimed the package name, that hosted package wins and the virtual packument is built from that package alone.
3. Only a package name not already claimed falls through to a proxy member.
4. If the first matching member is a proxy, the virtual packument is synthesized from that proxy's packument only.

The virtual packument is therefore a single synthetic document for the selected package, not a union of all versions from all members. This is crucial. A union would break the name-claim rule by letting a proxy shadow a hosted name, and it would make the virtual repository's view non-deterministic depending on which member published a package in which order.

A virtual repository never says: "show me every package that any member has." It says: "show me the package as resolved by the first member that can satisfy the name claim." That is the same rule that prevents dependency confusion.

## Consequences

- npm packuments stay cheap to compute and easy to version: they are derived from the rows, not stored as mutable documents.
- tag moves are recorded in a package metadata ledger rather than in `lifecycle_events`, preserving audit history without collapsing package metadata and trust state into one stream.
- the virtual repository rule remains consistent with invariant I5: a hosted member's claim blocks proxy shadowing even when the proxy has a newer version available.
- the npm adapter can share the same package-level indexing model as PyPI without inventing a second, mutable artifact class.

## Rejected

- **Store a packument blob as a mutable artifact.** Rejected because the packument changes on every publish and tag move, which makes it both a write-heavy mutable object and a poor fit for the existing subject model. It duplicates state that is already representable as rows, and it is the first place a stale document would be silently trusted.
- **Treat dist-tags as lifecycle states.** Rejected because a tag move is not a promotion, revocation, or evidence event and does not change the trust status of a subject. It changes a package pointer. Keeping it in the lifecycle ledger would blur the boundary between trust and metadata and make audits harder to reason about.
- **Merge virtual packuments across all members.** Rejected because it violates the name-claim rule and reintroduces the dependency-confusion failure mode by letting a proxy answer for a name a hosted member already owns.
- **Persist a synthetic virtual packument in the database.** Rejected because it creates a second source of truth that must be invalidated on every tag change or publish. The rule is already clear and cheap to compute at read time, so a persisted virtual view adds maintenance cost without adding safety.

## Notes

This decision is the npm-specific expression of the same core rule used elsewhere in Sindri: metadata is derived from the rows, trust is in the ledger, and repository resolution is explicit and ordered. The npm adapter should be built around that design, not around a mutable packument object that looks like a distributed registry artifact but is actually just a different view over the same package data.
