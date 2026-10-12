# 20. A queue table and a background worker

- **Status:** Accepted
- **Date:** 2026-10-11
- **Relates to:** [ADR 8](0008-staged-uploads-belong-to-the-blobstore.md),
  [ADR 19](0019-garbage-collection-roots-write-intent-and-sweep.md),
  ARCHITECTURE.md section 9, roadmap M8a

## Context

M8a brings the first background work: garbage collection, evicting stale proxy
cache entries, and cleaning up abandoned uploads. ARCHITECTURE.md section 9
already required that background work go through a queue table, even while
`sindri serve` runs everything in one process. That is what keeps the post-v1
`--api-only` and `--worker-only` modes a configuration change. What remained
open was the shape of the table, how claims stay safe, and where the clock
comes from.

## Decision

**One `jobs` table, claimed with `FOR UPDATE SKIP LOCKED`.** Any number of
workers claim concurrently and never share a job. A claim takes a lease. The
worker renews it every third of its length, and completing, failing, or
releasing a job checks that the caller still holds the lease. A worker that
dies holds its job only until the lease runs out. After that the next poll
requeues it and counts the attempt. A failed attempt is retried with
exponential backoff, capped at an hour, until `max_attempts` is reached.

**Dedupe keys.** A partial unique index allows one queued or running job per
`dedupe_key`. Enqueueing an active key returns the existing job instead of
creating another. This one mechanism covers three cases:

- `POST /api/v1/gc` twice returns one job.
- A scheduled collection and one asked for by hand never overlap.
- Two workers' schedulers racing each other insert one row between them.

**Schedules.** Each worker enqueues a kind when nothing of that kind is
active and nothing succeeded within the interval. Retention runs hourly by
default. Collection is not scheduled unless `worker.gc.interval` is set.

**The database is the clock.** Every time the queue compares or records comes
from Postgres's `now()`, so workers on different hosts agree on when a lease
ran out. Tests move rows with SQL instead of faking a clock.

**`kind` carries no CHECK constraint**, and a worker claims only the kinds it
has handlers for. A binary that was rolled back leaves newer kinds queued
instead of failing them or refusing to start.

**Wiring.** `worker.Run` takes a queue and handlers, nothing else, and stops
when its context ends. A job still running at that point is handed back
without counting the attempt. `serve` starts the worker beside the HTTP
server when `worker.enabled` is true (the default), and stops it before the
database pool closes. A test fails if that order is reversed. With the worker
off, jobs wait for a worker elsewhere.

**One sanctioned exception: upload sessions in process memory.** An OCI
chunked upload's session lives in the memory of the process that opened it
(ADR 8). A job claimed by another process could not reach it, so expiring
idle sessions runs on a ticker inside each serving process. The durable half,
staging state in storage, goes through the queue: the `purge_uploads` job calls
`BlobStore.PurgeUploads`. That method also removes state a crashed process
left behind, which no session knows about.

## Consequences

- The split serve modes stay a flag. The API and the worker share no memory,
  only the table.
- A crashed worker delays its job by at most one lease, two minutes by
  default, and never runs it twice at once.
- Finished jobs are deleted after 30 days by the purge job, so the table
  does not grow without limit.
- `BlobStore` gains `PurgeUploads`, which every backend implements and the
  conformance suite checks, including after a restart. On S3 it lists
  multipart uploads without a prefix and filters by key on the client,
  because MinIO returns nothing for a prefix that is not a whole object key.
  It needs `s3:ListBucketMultipartUploads`. A bucket lifecycle rule
  `AbortIncompleteMultipartUpload` is a sensible second safety net.

## Rejected

- **A goroutine and a ticker per task.** This is the simplest option, and it
  is what the upload session exception does. For everything else it would tie
  background work to the serving process, which is the coupling section 9
  exists to prevent.
- **An external queue (Redis, NATS, SQS).** It would add a second piece of
  stateful infrastructure for a workload measured in jobs per hour.
  Postgres already gives transactions, row locks, and a clock.
- **Advisory locks instead of leases.** A session lock dies with its
  connection, which ties a job to the database connection that claimed it.
  It also leaves no record of who held a job and when.
- **Application clocks.** With two workers on two hosts, clock skew would
  decide when a lease ran out.
