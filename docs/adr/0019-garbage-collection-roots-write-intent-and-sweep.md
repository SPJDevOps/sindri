# 19. Garbage collection: roots, write intent, and the sweep

- **Status:** Accepted
- **Date:** 2026-10-11
- **Relates to:** [ADR 3](0003-content-addressable-store-with-placement.md),
  [ADR 8](0008-staged-uploads-belong-to-the-blobstore.md),
  [ADR 13](0013-pypi-write-time-validation-and-immutable-files.md),
  [ADR 14](0014-proxy-cache-and-virtual-resolution.md),
  [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md), roadmap M8a
- **Refines:** ADR 16's "subjects are roots", ADR 14's placeholder rule

## Context

Deduplication means one blob can be placed in many repositories, so deleting
a placement never deletes bytes (ADR 3). Something has to reclaim bytes that
nothing references any more, without ever reclaiming bytes a push in flight
is about to depend on. That is the classic registry data-loss bug, and two
facts made it concrete here:

- **Bytes land before metadata.** Every write path stores bytes in the CAS and
  only then commits the placement transaction. A collector that deletes the
  bytes in between loses them, and the placement commits pointing at nothing.
- **Deduplication skips writes.** `cas.Put` and `cas.PutBytes` do not write
  bytes the store already holds. If the collector deletes them after that
  check, the skipped write never happens.

A grace period on blob age does not close either race on its own:
`blobs.created_at` was first-seen time, so an old blob pushed again looked
old.

## Decision

**Roots.** A blob is live when a root reaches it, directly or through
`manifest_children`:

- `tags.target_digest`
- `files.blob_digest`
- `remote_cache.blob_digest`, every hit, regardless of age
- `evidence.blob_digest`, and `evidence.subject_digest`
- every subject with a row in `lifecycle_events`

`repository_blobs` is not a root: a layer uploaded and never named by a
manifest is garbage. A subject with no ledger history and no evidence is not
a root either, so an untagged, unpromoted manifest is collected along with
its `subjects`, `subject_owners`, and `provenance` rows. This refines ADR 16,
which made every subject a root and so would have kept every image ever
pushed.

**Write intent.** `blobs.last_seen_at` is touched before any byte is written
or relied on. `cas` calls a `Reserver`, implemented in package `db` so that
`cas` stays below the database:

- `PutBytes` reserves before its dedup `Stat`.
- `Upload.Commit` reserves after the digest verifies and before
  `CommitUpload`.
- `Put` with a claimed digest touches before its dedup `Stat`. A touch only
  refreshes an existing row, so an unverified claim never creates one.

Each call commits on its own. Every placement's `UpsertBlob` also touches.
`gc.New` refuses a `cas.Store` without a reserver.

**Mark.** One recursive query in a `REPEATABLE READ READ ONLY` transaction
lists blobs that are unreachable and last seen before the grace period
(default 24h). The dry run is the mark alone.

**Sweep.** One `READ COMMITTED` transaction per candidate:

1. Lock the blob row with `FOR UPDATE SKIP LOCKED`. A row someone else holds
   is in use, so it is skipped.
2. Re-check the grace period against the row as it is now.
3. Lock the subject row, if any, so a promotion or evidence attach blocks.
4. Re-check every inbound reference.
5. Delete the dependent rows and the blob row.
6. Delete the bytes, still holding the lock.
7. Commit.

A writer whose intent committed first is seen by the re-check. A writer that
arrives during the sweep blocks on the lock. When it gets the row, the row
and the bytes are gone, so it writes both again. Parents go first, and the
sweep repeats passes until one deletes nothing.

**Placeholders.** ADR 14's placeholder rows have no bytes. They are collected
like anything else, and deleting a blob that has no bytes is a success.

**Proxy content** stays a root while its cache entry exists, so the collector
never reads TTLs. Retention evicts a hit once it is older than the
repository's `ttl` plus `worker.retention.proxyStaleAfter`, together with the
proxy tag or files it justified. After that the collector reclaims the bytes
through ordinary reachability. `ttl: 0` means fresh forever and is never
evicted. The stale allowance is what still serves a cached copy through an
upstream outage.

**Interfaces.** `sindri gc` talks HTTP to a running server, as `apply` does.
`GET /api/v1/gc/candidates` is the dry run, `POST /api/v1/gc` queues a
collection, and `GET /api/v1/jobs/{id}` reports on it. All three are
administrator-only. Scheduled collection is off by default
(`worker.gc.interval: 0`), because "untagged is garbage" is a policy to adopt
after a dry run, not one to inherit by upgrading.

## Consequences

- No unreferenced bytes are kept forever, and no referenced bytes can be
  deleted by a race. Each guard has a test that was run with the guard
  removed and failed: the reservation order, the dedup touch, the sweep's
  re-check, the grace re-check, and deleting bytes before the commit.
- A failed commit after the bytes were deleted leaves a row without bytes,
  which is the state a placeholder is always in. The next run collects it.
- A hosted manifest whose layer was collected before it arrived gets
  `MANIFEST_BLOB_UNKNOWN` and is pushed again. Before this ADR that case was a
  500.
- Rows from before the upgrade count as seen at the upgrade, so nothing is
  collectable until one grace period after it.
- **Known limit.** Bytes written by an M7 binary whose placement transaction
  then failed have no row, so the collector cannot see them. That is a
  storage leak, never a safety problem. Reclaiming them needs a storage
  listing, which is deferred until someone needs it.
- **Mixed versions.** An M7 binary writing beside an M8 collector writes
  bytes without an intent and is not safe. Upgrade every server before
  running `sindri gc`.

## Rejected

- **Reference counts.** Every placement and edge write would become a counter
  update. A missed decrement leaks and an extra one deletes live data, and
  reachability is still needed to audit the counts.
- **Committing, then deleting bytes.** Between the commit and the delete, a
  writer's dedup check finds the bytes and skips the write, and then they are
  deleted. The test for it fails with exactly that loss.
- **A grace period alone.** It protects new blobs, not a push that deduplicates
  against an old one.
- **`repository_blobs` as a root.** Nothing would ever be collected from a
  hosted repository, because every upload is placed.
- **TTL evaluation in the collector.** It would delete the cached copy that
  the stale fallback serves during an upstream outage, and it would put
  remote semantics into a component that otherwise knows only reachability.
- **A `BlobStore` listing to sweep orphaned bytes.** It would widen every
  backend and the conformance suite to recover a pre-upgrade leak the write
  intent now prevents.
