# Operating Sindri

<!-- commands checked by: scripts/deploy-compose-gate.sh -->

## Backup and restore

Sindri keeps state in two places, and a backup needs both. Postgres holds what exists:
repositories, tags, placements, and the lifecycle ledger. The blob store holds the bytes.
Neither is any use without the other.

**Back up Postgres first, then the blobs.** The order decides which kind of mismatch a
restore can have:

- *Bytes without rows* are harmless. Garbage collection removes them eventually.
- *Rows without bytes* are the failure: Sindri lists an artifact it cannot serve.

Taking the dump first means every row in it refers to bytes already written, so the later
copy includes them. Don't run garbage collection between the two steps, because it could
delete a blob the dump still refers to.

On the [Compose deployment](deploy/compose.md):

<!-- ops:backup -->
```bash
docker compose exec -T postgres pg_dump -U sindri -Fc sindri > sindri.dump
docker run --rm -v sindri_blobs:/data:ro -v "$PWD":/backup alpine:3.20 \
  tar czf /backup/blobs.tar.gz -C /data .
```
<!-- /ops:backup -->

To restore, onto the same machine or a new one with the same `compose.yaml`, `sindri.yaml`,
and `.env`:

<!-- ops:restore -->
```bash
docker compose stop sindri
docker compose up -d --wait postgres
docker compose exec -T postgres pg_restore -U sindri -d sindri --clean --if-exists --no-owner < sindri.dump
docker run --rm -v sindri_blobs:/data -v "$PWD":/backup alpine:3.20 \
  tar xzf /backup/blobs.tar.gz -C /data
docker compose up -d --wait sindri
```
<!-- /ops:restore -->

Anything pushed after the backup is gone after a restore. Clients get a "not found" and
push it again.

With managed Postgres or S3, the same order applies with your provider's tools: take the
database snapshot first, then copy the bucket. Bucket versioning protects the bytes
without a copy, as long as old versions outlive your oldest database snapshot.

Back up `.env` or your Secrets separately. The token and passwords are in neither backup.

## Garbage collection and retention

The worker runs inside `serve` and does two different jobs, configured under
[`worker`](configuration.md#worker).

**Retention** is on by default and runs hourly. It discards abandoned uploads after
`retention.uploadMaxIdle` and evicts stale proxy cache entries after
`retention.proxyStaleAfter`.

**Garbage collection deletes blobs nothing refers to, and is off until you schedule it.**
"Untagged and unpromoted is garbage" is a policy to adopt on purpose, so try it first:

- Run `sindri gc --dry-run` to list what a collection would delete, and check the list.
  Without `--dry-run` the command really deletes.
- Then set `worker.gc.interval` to schedule it.

Blobs seen within `worker.gc.grace` (24 hours by default) are always kept, which protects
uploads in progress.

## Metrics and alerts

`GET /metrics` serves Prometheus metrics with no authentication, so keep it off the public
internet. Sindri's own series:

| Metric | Meaning |
|---|---|
| `sindri_http_requests_total` | Requests by `method`, matched `route`, and `status` |
| `sindri_http_request_duration_seconds` | Request duration by `method` and `route` |
| `sindri_queue_jobs` | Background jobs by `state`: `queued`, `running`, `failed` |
| `sindri_build_info` | The running `version` and `commit`, always 1 |

A starting set of alerts, assuming the scrape job is named `sindri`. Tune the thresholds to
your traffic:

<!-- ops:alerts -->
```yaml
groups:
  - name: sindri
    rules:
      - alert: SindriDown
        expr: up{job="sindri"} == 0
        for: 5m
        labels: {severity: critical}
      # The database or the blob store is unreachable. Needs something calling
      # /readyz, which the Compose healthcheck and the chart's probes both do.
      - alert: SindriNotReady
        expr: sum(rate(sindri_http_requests_total{route="/readyz",status="503"}[5m])) > 0
        for: 5m
        labels: {severity: critical}
      # Storage failures during a request surface here as 5xx responses.
      - alert: SindriHighErrorRate
        expr: |
          sum(rate(sindri_http_requests_total{status=~"5.."}[5m]))
            / sum(rate(sindri_http_requests_total[5m])) > 0.05
        for: 10m
        labels: {severity: warning}
      - alert: SindriQueueBacklog
        expr: sindri_queue_jobs{state="queued"} > 100
        for: 30m
        labels: {severity: warning}
      # Failed jobs are kept for 30 days, so alert on new ones.
      - alert: SindriJobsFailed
        expr: delta(sindri_queue_jobs{state="failed"}[1h]) > 0
        labels: {severity: warning}
```
<!-- /ops:alerts -->

## Sizing

No load test has been published yet, so these say what grows rather than giving numbers.
Measure your own deployment against them.

- **Blob storage** grows with unique content: identical layers and files are stored once
  however many repositories hold them. Plan for your published artifacts plus everything
  your proxies cache.
- **Postgres** holds metadata only and stays small compared to the blobs. It grows with
  the number of artifacts and the lifecycle ledger, which is append-only.
  `database.maxConns` defaults to 10.
- **Memory** follows concurrency, more than stored data. Blobs are streamed rather than
  held in memory, but an npm proxy holds each upstream package document in memory while it
  serves it, so a very large package costs more.
- **Temporary space:** npm publishes are spooled to the temp directory while they are
  validated, so allow more than your largest package. The chart's `tmp.sizeLimit` sets
  this on Kubernetes.
- **CPU** goes mostly to hashing, because every uploaded byte is digested.

One replica is the supported size for now, so scale it up rather than out.

## Upgrading

See [Upgrading](upgrading.md). Take a backup first.
