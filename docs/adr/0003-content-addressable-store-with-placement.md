# 3. A content-addressable store with placement metadata

- **Status:** Accepted
- **Date:** 2026-09-21

## Context

Sindri is multi-protocol. Five protocols could mean five storage layouts and five
implementations each of lifecycle, promotion, policy, and scanning. That is exactly the
accumulated complexity we criticise Nexus for, and it would be self-inflicted.

We also need deduplication: the same base-image layer appears in hundreds of images, and
the same blob can arrive both through a proxy and through a hosted push.

## Decision

One content-addressable store keyed by `sha256`. A blob is stored exactly once per digest.
Repository membership is **placement metadata** in Postgres, not a second copy.

Lifecycle, promotion, policy, and evidence live once in the core, above the CAS and below
the protocol adapters. Adapters translate their wire format to core operations and own no
storage layout.

Because deduplication makes every blob physically reachable regardless of who pushed it,
**every read must check placement before touching storage** (invariant I2).

## Consequences

- A third protocol adapter is genuinely cheap, which is the claim the architecture exists
  to support.
- Large storage savings on layers and wheels.
- Deletion cannot be a file delete. It is unlinking a placement, and reclamation is the
  garbage collector's reachability pass.
- The placement check is a hard security boundary. Skipping it in one handler is a
  cross-tenant data leak, so it is a single shared function and a named invariant test.

## Rejected

- **Per-protocol storage trees.** Simplest to start, but duplicates the core five times
  and makes cross-protocol lifecycle impossible.
- **Reference counting for reclamation.** Cheap reads, but a drifted counter either leaks
  storage permanently or deletes live data, and it cannot be recomputed after the fact.
  Reachability from tags and lifecycle states over `manifest_children` is recomputable,
  which is worth the scan cost.
