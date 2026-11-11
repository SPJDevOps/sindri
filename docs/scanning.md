# Scanning and the severity gate

<!-- commands checked by: scripts/m11-scan-gate.sh -->

With scanning enabled, every hosted publish is scanned, and promotion is refused above a
severity. This page turns that on, shows what a refusal looks like, and explains how to
read the scan evidence behind it. It ends with how to run scanning on a server that
cannot reach the internet.

The commands assume the setup from the [quickstart](quickstart.md): a server at
`localhost:8080` and the bootstrap token in `SINDRI_TOKEN`.

## Enable scanning

Scanning is off by default. It needs Trivy, which the `-trivy` release image ships. Use
`ghcr.io/spjdevops/sindri:<version>-trivy` (or `latest-trivy`) in place of the plain
image, and add this to `sindri.yaml`:

```yaml
scanning:
  enabled: true
```

For Docker Compose, set `SINDRI_VERSION` in `.env` to the `-trivy` tag and keep Trivy's
cache on a volume. The [Compose guide](deploy/compose.md#scanning) shows the volume. For
Kubernetes, set `image.tag` to the `-trivy` tag and `config.scanning.enabled`. The
[Helm guide](deploy/helm.md#scanning) shows the values.

Each hosted publish then queues a scan job. The worker unpacks the image or package,
runs Trivy, and attaches two documents to the artifact as evidence: a CycloneDX `sbom`
and a Trivy `scan` report. The queue is the same one the garbage collector uses (see
[ADR 20](adr/0020-queue-table-and-background-worker.md)), so a scan that fails is retried
with backoff. Every key under `scanning` is in the
[configuration reference](configuration.md#scanning).

Turning scanning on does not scan what was published earlier. Those artifacts have no
scan until something queues one, and what does depends on the protocol:

- **OCI:** pushing the same manifest again queues a scan.
- **PyPI:** uploading the identical file again queues a scan. A file that was deleted
  cannot be uploaded again, because a deleted filename is never reused.
- **npm:** nothing. Publishing a version again is refused, so an existing version cannot
  be scanned later. A new version is a new artifact and gets its own scan.

A command to scan what is already published arrives with M11b (SIN-33).

## Refuse promotion above a severity

A `scan` gate sets a severity ceiling for a stage. Declare it next to the stages:

```yaml
lifecycle:
  stages: [dev, prod]
  gates:
    prod:
      - scan: {maxSeverity: high}
```

`maxSeverity` is a ceiling: `critical`, `high`, `medium`, or `low`. The ceiling
`high` allows findings rated high and lower, and refuses a critical one. The ceiling
`low` refuses everything rated medium or above. A finding that Trivy does not rate
(`UNKNOWN`) is refused at every ceiling, because the gate cannot tell it is safe.

The gate reads the newest scan that Sindri itself made, for each platform of the
subject. For an image index, every platform in the index must have a scan. A scan you
attach yourself, with `POST /subjects/{digest}/evidence`, still satisfies a presence gate
such as `sbom`, but the severity gate ignores it. Only the worker can mark evidence as
its own, so nobody who can publish can produce a clean scan.
[ADR 29](adr/0029-what-a-scan-covers.md) and
[ADR 30](adr/0030-severity-gate.md) give the reasoning.

To see it work, publish an old image and try to promote it. `alpine:3.10` carries a
critical finding in Trivy's database as this page was written:

<!-- scan:push -->
```bash
echo "$SINDRI_TOKEN" | docker login localhost:8080 -u admin --password-stdin
docker pull alpine:3.10
docker tag alpine:3.10 localhost:8080/scan/old:1
docker push localhost:8080/scan/old:1
DIGEST=$(curl -fsSI -u "admin:$SINDRI_TOKEN" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  localhost:8080/v2/scan/old/manifests/1 \
  | tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest" {print $2}')
echo "$DIGEST"
```
<!-- /scan:push -->

The scan runs in the background, so wait for the `scan` evidence to appear before you
promote. This polls for up to two minutes, longer than a small image needs once the
database is cached:

<!-- scan:wait -->
```bash
for _ in $(seq 1 60); do
  n=$(curl -fsS -H "Authorization: Bearer $SINDRI_TOKEN" \
    "localhost:8080/api/v1/subjects/$DIGEST/evidence" \
    | jq '[.items[] | select(.type == "scan")] | length')
  [ "$n" -gt 0 ] && break
  sleep 2
done
echo "scans: $n"
```
<!-- /scan:wait -->

A subject with no scan yet is refused, the same as one with a bad scan. Now promote:

<!-- scan:promote -->
```bash
curl -sS -X POST -H "Authorization: Bearer $SINDRI_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"repository":"scan","stage":"prod","reason":"try the severity gate"}' \
  "localhost:8080/api/v1/subjects/$DIGEST/promote" | jq .
```
<!-- /scan:promote -->

The server answers `409 Conflict` with code `GATE_FAILED`. The `violations` list names
each rule that failed, the platform when the subject has one, and the findings above the
ceiling:

```json
{
  "code": "GATE_FAILED",
  "message": "promotion to prod refused: scan.max_severity=high: findings above the ceiling: CVE-2021-36159 CRITICAL",
  "request_id": "c8b7cda5d42367d73f26d5e00778564b",
  "violations": [
    {
      "rule": "scan.max_severity=high",
      "findings": [{"id": "CVE-2021-36159", "severity": "CRITICAL"}]
    }
  ]
}
```

The message names at most ten findings and then counts the rest; `violations` lists them
all. A subject with a platform that has findings also carries `"platform": "linux/arm64"`
(or whichever platform it is) on that violation. When the gate refuses for lack of
evidence instead, the violation has a `reason` and no `findings`:

| `reason` | Cause |
|---|---|
| `no Sindri scan for sha256:...` | The scan is queued, failed after its retries, or the artifact predates scanning. |
| `scan incomplete: no Sindri scan for platform linux/arm64` | One platform of an index has no scan: the job is still running, or it failed part way. |
| `scan incomplete: no Sindri scan for the index child without a platform` | The same, for an index child that declares no platform. |
| `the Sindri scan <id> lists no vulnerabilities; it needs a new scan, ...` | A scan written before the gate existed. Pushing it again (OCI) or uploading it again (PyPI) queues a new scan; npm has no way to until M11b. |
| `the Sindri scan <id> does not list the platforms of its index; it needs a new scan, ...` | A scan of an index child written before the gate existed. The same applies. |

A refusal writes no event to the ledger. A promotion that passes records the ids of the
scan and presence evidence it evaluated, in `evidence_ids` on the event, so an audit can
show exactly which scan the decision rested on. Read them with
`GET /api/v1/subjects/{digest}/events` or `GET /api/v1/audit`. A later scan never rewrites
a decision that was already made.

## Read the evidence

Evidence is listed per artifact. Each item carries the metadata the worker wrote:

<!-- scan:evidence -->
```bash
curl -fsS -H "Authorization: Bearer $SINDRI_TOKEN" \
  "localhost:8080/api/v1/subjects/$DIGEST/evidence" \
  | jq '.items[] | select(.type == "scan") | {id, media_type, created_at, metadata}'
```
<!-- /scan:evidence -->

```json
{
  "id": "3670f7f3-ea62-491e-8398-4fe4cb6b44a8",
  "media_type": "application/vnd.aquasec.trivy.report+json",
  "created_at": "2026-11-11T09:41:07Z",
  "metadata": {
    "producer": "sindri",
    "scanner": "trivy",
    "scanner_version": "0.76.2",
    "db_updated_at": "2026-11-10T18:34:12.5Z",
    "severity_counts": {"CRITICAL": 1, "HIGH": 0, "MEDIUM": 0, "LOW": 0, "UNKNOWN": 0},
    "vulnerabilities": [{"id": "CVE-2021-36159", "severity": "CRITICAL"}],
    "coverage": "image"
  }
}
```

| Key | Meaning |
|---|---|
| `producer` | `sindri` on evidence the worker wrote. The severity gate reads only this. |
| `scanner`, `scanner_version` | The scanner and its version. |
| `db_updated_at` | When the vulnerability database the scan used was built. See [Keep the database fresh](#keep-the-database-fresh). |
| `severity_counts` | Findings per severity. |
| `vulnerabilities` | Each finding as `id` and `severity`, worst first. On `scan` rows only; the `sbom` row omits it. |
| `coverage` | `image` for an OCI image, `package` for a wheel, sdist, or npm tarball. |
| `platform` | The platform of an index child, such as `linux/arm64`. Absent on a single image. |
| `platforms` | On an index child's scan, every platform the job scanned. The gate requires a scan for each. |

The full report and the SBOM are the evidence documents themselves.
`GET /api/v1/subjects/{digest}/evidence/{id}` returns the bytes. The web UI shows the
same evidence on the artifact's page. Each Sindri `scan` row there also shows its findings
per severity, worst first, and its platform, or "no findings". A scan a client attached
shows no summary, because the page only repeats what Sindri itself found. The list of
individual CVEs is in the API for now; a vulnerability view in the UI comes with M11b
(SIN-35).

Two Prometheus series describe the worker (see [Operating Sindri](operations.md#metrics-and-alerts)):
`sindri_scans_total{result}` counts scan job attempts as `succeeded`, `failed`, or
`skipped`, and `sindri_scan_duration_seconds` times them. Both exist only while scanning
is on. `sindri_queue_jobs{state="failed"}` rises when a scan has used up its retries.
That artifact then has no Sindri scan and a severity gate refuses it.

## Size the cache

Trivy keeps its vulnerability database in `scanning.cacheDir`. The `-trivy` image sets
that to `/var/cache/sindri/trivy`. Scans also unpack each image or package into the same
volume, so size it for two things:

- **The databases.** The vulnerability database is about 130 MiB compressed and the Java
  database about 950 MiB compressed, both larger once unpacked. Trivy downloads the Java
  database only when a scan finds a jar.
- **The largest thing you scan.** Each scan unpacks the whole image. A volume smaller
  than the largest image fails that scan, and the job is retried. For wheels and
  tarballs, `scanning.maxUnpackedBytes` (1 GiB by default) caps one archive.

Keep the cache on a volume that survives a restart, or Trivy downloads the database again
each time. Compose and Helm both give the cache a place; their guides show it.
`scanning.timeout` (ten minutes by default) cancels a scan that runs too long.

## What scanning does not cover

- **It is off by default.** Nothing is scanned until you set `scanning.enabled`.
- **Hosted repositories only.** Content cached from an upstream through a proxy is never
  scanned. It has no subject of its own, and a proxy is a mirror of someone else's
  artifacts (ADR 16, ADR 23).
- **Coverage differs by protocol.** An image scan covers the OS and language packages
  installed in its layers. A wheel or npm tarball scan covers the package's own name and
  version and anything pinned inside the archive, such as a bundled `node_modules`. It
  does **not** resolve the dependency ranges the package declares, so a clean scan of a
  wheel does not mean what `pip install` fetches beside it is clean. Teams that need that
  scan their lockfile in CI and attach the result for presence gates. Generic files are
  not scanned. [ADR 29](adr/0029-what-a-scan-covers.md) has the detail.
- **Only Sindri's scans count.** The severity gate ignores evidence a client attached.
- **A missing scan refuses.** A queued job, a failed job, or an artifact published before
  scanning was on has no Sindri scan, and the gate refuses it.
- **A scan is a point in time.** A vulnerability published tomorrow does not change
  today's scan or a decision made on it. Pushing an OCI image or uploading a PyPI file
  again scans it with a newer database. npm cannot do this until the re-scan command
  arrives with M11b (SIN-33).

## Scan without internet access

Trivy fetches two databases from `ghcr.io/aquasecurity`: `trivy-db` for
vulnerabilities and `trivy-java-db` for the Maven index it uses to identify jars. Both
are ordinary OCI artifacts. An air-gapped install copies them into a registry it can
reach and points Sindri at the copy.

### Mirror the databases

Run this on a machine that can reach both `ghcr.io` and your mirror. It uses
[`oras`](https://oras.land) (the commands were run with oras 1.3.0) and any registry that
accepts OCI artifacts. For a first try, this starts a throwaway one:

<!-- scan:mirror-registry -->
```bash
docker run -d --name trivy-mirror -p 127.0.0.1:5001:5000 registry:3
export MIRROR=localhost:5001
```
<!-- /scan:mirror-registry -->

The mirror listens on port 5001 because macOS uses 5000 for AirPlay Receiver.

For a real mirror, set `MIRROR` to your registry's host, and log in with
`oras login "$MIRROR"` if it needs credentials. Then copy both databases. `oras cp` copies
the manifest and layers as they are, and the tag you give is the tag Trivy asks for:

<!-- scan:mirror -->
```bash
oras cp --to-plain-http ghcr.io/aquasecurity/trivy-db:2 "$MIRROR/trivy/trivy-db:2"
oras cp --to-plain-http ghcr.io/aquasecurity/trivy-java-db:1 "$MIRROR/trivy/trivy-java-db:1"
```
<!-- /scan:mirror -->

Drop `--to-plain-http` for a mirror that serves HTTPS. Trivy reaches a mirror over HTTPS
unless its host is `localhost`, so a real mirror needs TLS. If its certificate comes from
a private CA, set `SSL_CERT_FILE` in the server's environment to a bundle that holds the
CA; Sindri passes it on to Trivy.

Point Sindri at the copies. Give each as a full reference with its tag:

```yaml
scanning:
  enabled: true
  dbRepository: mirror.example.com/trivy/trivy-db:2
  javaDbRepository: mirror.example.com/trivy/trivy-java-db:1
```

With these set, Trivy asks only the mirror. It no longer falls back to the public
registries. Sindri passes the values to Trivy as `--db-repository` and
`--java-db-repository`, and Trivy reads any registry credentials from the server's Docker config.

### Keep the database fresh

Aquasec publishes a new vulnerability database regularly. The mirror holds the
copy you made, so it ages until you copy again. Run the two `oras cp` commands on a
schedule from a machine that sees both registries. A cron entry or a Kubernetes
`CronJob` that runs every six hours is enough. Sindri needs no restart: on its next scan Trivy
checks whether its cached database is due for an update, and if so downloads from the mirror.

Every scan records `db_updated_at`, the build time of the database it used, so a stale
mirror is visible on the artifacts it scanned. This prints the age in days of the newest
scan of one artifact:

<!-- scan:staleness -->
```bash
curl -fsS -H "Authorization: Bearer $SINDRI_TOKEN" \
  "localhost:8080/api/v1/subjects/$DIGEST/evidence" \
  | jq -r '[.items[] | select(.type == "scan")] | sort_by(.created_at) | last
           | .metadata.db_updated_at
           | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601
           | ((now - .) / 86400 | floor)
           | "database is \(.) days old"'
```
<!-- /scan:staleness -->

Alert on that age from your own tooling. A scan made against a month-old database is
still a valid scan, and the gate does not look at its age. It will pass an artifact whose
newest vulnerabilities the database does not know yet.

### Or pre-seed the cache

If you cannot run a registry, fill the cache directly and tell Trivy never to download.
On a machine with internet access, use the same Trivy version as the `-trivy` image
(0.76.2 at the time of writing; the version is in `scanner_version` on any scan):

<!-- scan:preseed -->
```bash
mkdir -p trivy-cache
docker run --rm -v "$PWD/trivy-cache:/cache" aquasec/trivy:0.76.2 \
  image --download-db-only --cache-dir /cache
docker run --rm -v "$PWD/trivy-cache:/cache" aquasec/trivy:0.76.2 \
  image --download-java-db-only --cache-dir /cache
tar -C trivy-cache -czf trivy-cache.tar.gz .
```
<!-- /scan:preseed -->

Carry `trivy-cache.tar.gz` across the gap and unpack it into the cache directory the
server uses (`/var/cache/sindri/trivy` in the `-trivy` image). The server accepts only a
cache directory with mode `0700`, owned by the user it runs as, which is uid 65532 in the
release image. With Docker, this fills a named volume that way:

<!-- scan:unpack -->
```bash
docker volume create sindri-trivy-cache
docker run --rm -v sindri-trivy-cache:/cache -v "$PWD:/in:ro" alpine:3.20 sh -c \
  'tar -xzf /in/trivy-cache.tar.gz -C /cache && chown -R 65532:65532 /cache && chmod 0700 /cache'
```
<!-- /scan:unpack -->

Mount that volume at `/var/cache/sindri/trivy`. Then set:

```yaml
scanning:
  enabled: true
  skipDBUpdate: true
```

`skipDBUpdate` cannot be combined with `dbRepository` or `javaDbRepository`, and the
server refuses to start with both. Refresh the cache by repeating the steps. The Helm
chart's scan cache is an `emptyDir`, which is empty again when the pod is replaced, so
use the mirror there.

### Images with jars fail closed

Trivy loads the Java database only when a scan finds a jar. An air-gapped server with no
Java database does not skip those files: the scan **fails**, retries, and runs out of
attempts. That artifact has no Sindri scan, and a severity gate refuses it. This is the
intended direction of failure, because a scan that quietly skipped the jars could report
an image clean that is not.

It happens in two ways. With `skipDBUpdate`, the Java database is missing from the cache.
With a mirror, `javaDbRepository` is unset, so Trivy tries the public registries and
cannot reach them. Either way the fix is to supply the Java database. Watch for it in
`sindri_queue_jobs{state="failed"}` and in `sindri_scans_total{result="failed"}`.

## Clean up

If you started the throwaway mirror above:

<!-- scan:cleanup -->
```bash
docker rm -f trivy-mirror
```
<!-- /scan:cleanup -->
