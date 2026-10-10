# Sindri Architecture

This document is the single source of truth for Sindri's design. When code and this
document disagree, one of them is a bug. Decisions with a rationale worth preserving live
in [docs/adr/](docs/adr/).

The implementation sequence is in [roadmap.md](roadmap.md). The product rationale is in
[pitch.md](pitch.md).

---

## 1. The organising idea

Classic repositories are file-first. They ask "where is the file?" Sindri is
identity-first. It asks "what is this artifact, where did it come from, who owns it, what
evidence backs it, and how far has it been trusted?"

The practical consequence is that **an artifact is never copied between repositories as
it matures.** Its trust status changes. One digest, one set of bytes, many lifecycle
states.

```
                    subject
                 sha256:abc123
                       |
          +------------+------------+
          v            v            v
        DEV       INTEGRATION      PROD
      approved     approved       denied
```

So the question Sindri answers for CI/CD and for a Kubernetes admission controller is not
"does this image exist?" but **"is exactly this digest approved for this environment?"**
That is a single indexed query, not a directory convention.

---

## 2. The five invariants

Everything below follows from these. A change that breaks one of them needs an ADR.

### I1. One blob, many placements

A blob exists exactly once per `sha256` digest in the content-addressable store.
Repository membership is metadata, never a second copy. A layer pulled through a proxy
and also pushed to a hosted repository is stored once with two placement rows.

### I2. Placement gates every read

Never serve bytes by digest alone. Every read resolves
`(caller, repository, digest) -> allowed?` before touching storage.

This is the invariant most easily broken by accident. Deduplication means a digest one
tenant pushed is physically present for everyone, so an endpoint that trusts the digest
in the URL is a cross-tenant data leak. The OCI specification's blob routes are scoped by
repository name for exactly this reason. Honour that scope even though the CAS does not
need it.

### I3. Promotion changes state, never location

Lifecycle is an append-only ledger keyed by subject digest. Promoting writes one row.
Nothing is moved, copied, retagged, or re-uploaded. Current state is a view over the
latest event per `(digest, repository, stage)`.

Append-only gives the audit trail for free, makes "who promoted this and why" answerable
forever, and turns the paid audit-retention feature into a query rather than a rewrite.

### I4. Only hosted repositories promote

Proxy content is external and stays external. Promotion, ownership, and evidence apply to
artifacts that are ours. The reconciler rejects lifecycle configuration on a proxy
repository at write time, not at request time.

### I5. Virtual repositories resolve in explicit, hosted-first order

A virtual repository holds an ordered member list. Resolution walks it and the first hit
wins. There is no scoring, no "newest version across members", no implicit ordering.

Combined with the name-claim rule in section 4, this is the dependency-confusion defence:
a package name claimed by a hosted member can never be served from a proxy member, no
matter what version the upstream offers.

---

## 3. Component map

```
              +-----------------------------------------------+
              |             Protocol Adapters                 |
              |   OCI  |  PyPI  |  (npm, Maven, Helm: post-v1)|
              +--------------------+--------------------------+
                                   |
              +--------------------v--------------------------+
              |  Resolution: hosted | proxy | virtual         |
              |  ordering, name claims, upstream cache        |
              +--------------------+--------------------------+
                                   |
        +--------------------------+--------------------------+
        |                          |                          |
+-------v--------+     +-----------v----------+    +----------v---------+
|  Core Registry  |     |  Identity/Lifecycle  |    |  Auth: OIDC, RBAC  |
|  publish,       |     |  subjects, evidence, |    |                    |
|  resolve, list  |     |  stages, promotion   |    |                    |
+-------+--------+     +-----------+----------+    +--------------------+
        |                          |
+-------v--------------------------v--------------+
|  CAS: sha256 -> blob, staged uploads, dedup      |
+-------+------------------------------+----------+
        |                              |
+-------v---------+          +---------v---------+
|  BlobStore      |          |  Metadata: Postgres|
|  filesystem, S3 |          |                    |
+-----------------+          +--------------------+
```

Lifecycle, promotion, policy, and evidence exist **once** in the core, not once per
protocol. That is the whole reason for the shared CAS. If a protocol adapter ever needs
its own promotion logic, the abstraction is wrong.

### Package layout

```
cmd/sindri/              entrypoint and command tree
internal/
  config/                YAML kinds (Repository, OrgPolicy), loading, validation
  storage/               BlobStore interface
    fsstore/             filesystem backend
    s3store/             S3-compatible backend
    memstore/            in-memory backend, tests only, not configurable
    storagetest/         the one conformance suite every backend runs
  cas/                   digest computation, staged uploads, dedup
  db/                    pgx pool, migrations/, sqlc-generated queries
  model/                 domain types shared across packages, imports nothing
  server/                HTTP server, middleware, /healthz, /readyz, /metrics
  registry/              core artifact service: publish, resolve, list, delete
  lifecycle/             stages, promotion, gate evaluation
  evidence/              attach and list evidence
  auth/                  tokens, OIDC, RBAC middleware
  api/                   management REST API
  proto/
    oci/                 OCI distribution spec handlers
    pypi/                PEP 503/691 index and upload
  remote/                proxy upstream fetch and caching
  resolve/               virtual repository ordered resolution
  reconcile/             YAML to database convergence
  queue/                 the jobs table: enqueue, claim under a lease, complete
  gc/                    garbage collection: mark and sweep (ADR 19)
  worker/                runs queued jobs and schedules recurring ones (ADR 20)
web/                     Go templates and static assets, embedded
deploy/compose/          local and single-node deployment
docs/adr/                architecture decision records
```

Dependency direction is strictly inward. `proto/*` and `api` depend on `registry`,
`lifecycle`, and `auth`. Those depend on `cas`, `db`, and `model`. `cas` depends on
`storage`. Nothing in `storage`, `cas`, or `model` imports an adapter. Keeping this
one-way is what makes a third protocol cheap later.

---

## 4. Repository types

| Type | Owns bytes | Promotable | Resolution |
|---|---|---|---|
| **hosted** | yes | yes | direct |
| **proxy** | caches upstream bytes | never | direct, with upstream fetch on miss |
| **virtual** | no | no | ordered walk over members |

**Hosted** repositories hold our own artifacts. They are the only place ownership,
provenance, evidence, and lifecycle apply.

**Proxy** repositories mirror an upstream such as pypi.org or Docker Hub. On a miss they
fetch, stream to the client, and cache into the CAS. They carry a positive TTL for
metadata freshness and a negative TTL so that a missing package does not hammer upstream.
Cached bytes are indistinguishable from any other blob in storage, but their placement row
marks them as proxied, which is what keeps invariant I4 enforceable.

**Virtual** repositories are the developer-facing view. One URL, an explicit ordered list
of members, hosted first.

**OCI path routing.** An OCI repository name and an image name share one URL path. On
`/v2/...`, Sindri chooses the longest existing repository name that is a prefix of the
path; the remainder is the image name within that repository (see ADR 14). PyPI needs no
equivalent rule because `/simple/`, `/packages/`, and `/upload` already mark where the
repository name ends.

### The name-claim rule

When a virtual repository resolves a package name:

1. Walk members in order.
2. If a **hosted** member has ever published that normalized name, that name is
   **claimed**. Resolution stops at the hosted member. A proxy member is not consulted,
   even if the hosted member lacks the requested version.
3. Only an unclaimed name falls through to a proxy member.

Step 2 is the dependency-confusion defence, and the failure mode it prevents is subtle:
without it, an attacker publishes `our-internal-lib` version `99.0.0` to pypi.org, a
developer asks for the newest version, and the proxy member wins. With it, the request
fails loudly instead of installing hostile code. There is a regression test named for this
behaviour; it is not optional and it does not get relaxed for convenience.

---

## 5. Data model

Postgres. Two layers: identity, keyed by digest, and placement, keyed by repository and
name.

### Identity layer

```sql
subjects(digest PK FK, size, kind, created_at)  -- oci-manifest | oci-index | pypi-dist | file
subject_owners(digest FK, owner_ref, PRIMARY KEY (digest, owner_ref))
provenance(id PK, digest FK, source_type, source_ref, build_ref, recorded_at,
           UNIQUE NULLS NOT DISTINCT (digest, source_type, source_ref, build_ref))

evidence(
  id PK, subject_digest FK, type,          -- sbom | signature | scan | attestation
  media_type, blob_digest FK, metadata jsonb,
  created_at, created_by
)

lifecycle_stages(id PK, name UNIQUE, position UNIQUE DEFERRABLE)
promotion_gates(id PK, stage_id FK, position, rule jsonb)   -- {"evidence": "sbom"}

lifecycle_events(                          -- APPEND ONLY, never updated or deleted
  id PK, subject_digest FK, repo_id FK, stage_id FK,   -- FKs do not cascade
  action,                                  -- approve | revoke
  actor, reason, at
)
lifecycle_current                          -- VIEW: latest event per (digest, stage, repo)
```

A subject is created inside the publish transaction of a **hosted** repository, owned by
`repository:<name>`, with the publisher's provenance claim recorded unverified. Proxy
caching never creates one (I4). A subject outlives its placements. See ADR 16.

`lifecycle_events` is the ledger of invariant I3. A trigger refuses `UPDATE`, `DELETE`, and
`TRUNCATE`, and its foreign keys do not cascade, so a repository or stage with history
cannot be deleted out from under it. Current state is the view `lifecycle_current`, a
`DISTINCT ON` over `lifecycle_events_current_idx (subject_digest, stage_id, repo_id, at
DESC, id DESC)`: the admission-decision endpoint is on the hot path for every deployment
in the organisation, and a test asserts its query uses that index.

Evidence documents are blobs in the CAS with no repository placement; they are read only
through the subject they are attached to. Gates check that evidence of a type is present;
v1 does not parse or verify the documents.

### Placement and protocol layer

```sql
blobs(digest PK, size, storage_backend, created_at,
      last_seen_at)                        -- write intent; GC grace (ADR 19)

repositories(
  id PK, name UNIQUE, type,                -- hosted | proxy | virtual
  protocol,                                -- oci | pypi
  config jsonb, locked bool, managed_by,   -- config | api
  created_at
)  -- config: {namespace, source: file|apply, access: {read, write}, lifecycleStages}
repository_members(virtual_repo_id FK, member_repo_id FK, position,
                   PRIMARY KEY (virtual_repo_id, member_repo_id))
remotes(repo_id PK FK, upstream_url, credentials_ref,
        ttl_seconds, negative_ttl_seconds)

packages(id PK, repo_id FK, normalized_name, display_name,
         UNIQUE (repo_id, normalized_name))
versions(id PK, package_id FK, version, UNIQUE (package_id, version))
files(id PK, version_id FK, blob_digest FK, filename, media_type, metadata jsonb)

tags(repo_id FK, package_id FK, name, target_digest FK,
     PRIMARY KEY (repo_id, package_id, name))     -- mutable, OCI
manifest_children(parent_digest FK, child_digest FK,
                  PRIMARY KEY (parent_digest, child_digest))
repository_blobs(repo_id FK, digest FK,
                 PRIMARY KEY (repo_id, digest))   -- OCI blob membership (I2)
```

`repositories.config` carries what authorization and the reconciler read on
the request path. That covers the owning team's namespace, which reconciler
trigger owns the row, the groups granted read and write, and the stages a
hosted repository may promote into. A pull then costs one row lookup, and that
row already holds its access list (ADR 17, ADR 18).

```sql
org_policy(id = 1, spec jsonb, updated_at)           -- the last applied OrgPolicy
machine_tokens(id PK, name, token_hash UNIQUE,       -- SHA-256 only, never the token
               repo_id FK NULL, namespace NULL,      -- at most one scope
               role,                                 -- read | write | admin
               created_by, created_at, expires_at, revoked_at)
```

No table holds group membership. Groups are read from the caller's token on
every request (ADR 5).

`normalized_name` is the protocol's canonical form, lowercased with separators folded for
PyPI per PEP 503. Uniqueness is on the normalized form so that `Foo.Bar` and `foo-bar`
cannot both be claimed.

A repository name may contain slashes, so that `team/internal` reads naturally in a
`docker pull` or an index URL. It may not contain a segment an adapter splits paths on
(`blobs`, `manifests`, `tags`, `uploads`, `simple`, `packages`, `upload`), because
`/pypi/team/packages/simple/foo/` would otherwise name either a repository or a download.
The rule lives in one validator rather than in each adapter. See ADR 13.

`files.metadata` holds what an adapter needs to render an index entry it cannot derive from
the placement rows: for PyPI that is `name`, `version`, `requires_python`, and `summary`,
read from the wheel's `METADATA` or the sdist's `PKG-INFO` at upload. It is adapter-owned
and read only by the adapter that wrote it, so a proxy caching the same project writes the
same keys rather than inventing a second spelling.

A `files` row is immutable once placed: a re-publish of identical bytes is a no-op, and one
with different bytes is refused. Consumers pin the hash an index advertised, so the bytes
behind a filename are not something a later push may change. See ADR 13.

`repository_blobs` records that a digest was uploaded (or written as a manifest) into a
repository. It is what makes a layer readable under invariant I2 after a blob push and
before any tag exists. See ADR 10.

`manifest_children` records manifest-to-blob and index-to-manifest edges at push time.
Garbage collection is then a reachability query from tags and lifecycle states, which is
far safer than reference counting: a refcount that drifts either leaks storage forever or
deletes live data, and it cannot be recomputed after the fact.

### Garbage collection

The roots are tags, PyPI files, proxy cache hits, evidence, and every subject
with ledger history. Anything they do not reach through `manifest_children`,
and that was last seen before the grace period, is a candidate. That includes
an untagged, unpromoted image and a layer no manifest names. Placement in
`repository_blobs` is not a root.

Every writer touches `blobs.last_seen_at` before it writes bytes or relies on
bytes it found. The sweep deletes one candidate per transaction, and for each
one it:

1. locks the row
2. re-checks every reference and the grace period
3. deletes the rows, then the bytes, then commits

A push racing the collector either commits its intent first and is seen, or
blocks on the lock and writes the bytes again afterwards. See ADR 19.

### Background work

```sql
jobs(id PK, kind, args jsonb, state,       -- queued | running | succeeded | failed
     dedupe_key,                           -- one active job per key
     attempts, max_attempts, run_after,
     lease_owner, lease_expires_at,        -- a dead worker's lease runs out
     created_by, created_at, started_at, finished_at, last_error, result jsonb)
```

Jobs are claimed with `FOR UPDATE SKIP LOCKED` under a lease, and every time
the queue uses comes from the database clock. See ADR 20.

---

## 6. Storage

```go
// BlobStore is the only thing that touches bytes at rest.
type BlobStore interface {
	Put(ctx context.Context, digest model.Digest, r io.Reader, size int64) error
	Get(ctx context.Context, digest model.Digest) (io.ReadCloser, error)
	GetRange(ctx context.Context, digest model.Digest, off, length int64) (io.ReadCloser, error)
	Stat(ctx context.Context, digest model.Digest) (BlobInfo, error)
	Delete(ctx context.Context, digest model.Digest) error

	// Staged writes. Bytes land under a staging key and reach their content
	// addressed key only through CommitUpload, after the caller has verified
	// the digest. See ADR 8.
	CreateUpload(ctx context.Context) (UploadID, error)
	Append(ctx context.Context, id UploadID, r io.Reader) (offset int64, err error)
	CommitUpload(ctx context.Context, id UploadID, digest model.Digest) error
	AbortUpload(ctx context.Context, id UploadID) error

	// Discards staged uploads idle since before olderThan, including what a
	// crashed process left behind. See ADR 20.
	PurgeUploads(ctx context.Context, olderThan time.Time) (int, error)
}
```

A digest is `model.Digest`, a struct with unexported fields, never a string. It is parsed
at the edge, so no internal code can be handed a malformed one and the zero value is
invalid rather than an empty but usable key.

Two implementations, filesystem and S3-compatible. Choosing between them is configuration,
never a code path. Both pass the same conformance suite, which is what makes that claim
true rather than aspirational.

`GetRange` exists because OCI clients resume interrupted layer pulls and because the UI
should not stream a gigabyte to render a manifest.

**Staged uploads.** An upload writes to a staging key under a session id, hashing as it
goes. On completion the computed digest is compared against the client's claim; only on a
match is the blob committed to its final content-addressed key, by rename on a filesystem
and by server-side copy on S3, so a commit moves no bytes through the process. A mismatch
discards the staging object. Never write unverified bytes to a content-addressed key, and
never trust a client-supplied digest.

`internal/cas` is the only caller of the staging methods and the only place that decides a
blob's digest. `Put` stays for payloads that are already in memory and already hashed,
such as manifests and evidence documents, where a staging round trip buys nothing.

---

## 7. HTTP surface

| Prefix | Purpose |
|---|---|
| `/v2/...` | OCI distribution API |
| `/pypi/{repo}/simple/...` | PyPI index, PEP 503 and 691 |
| `/pypi/{repo}/packages/...` | PyPI file download |
| `/pypi/{repo}/upload` | PyPI upload, twine's POST |
| `/api/v1/...` | management API |
| `/api/v1/lifecycle/stages` | ordered stages and their gates |
| `/api/v1/subjects/{digest}/...` | identity, evidence, promote, revoke, ledger |
| `/api/v1/decisions?digest=&environment=` | "is this digest approved for this environment" |
| `/api/v1/config/diff`, `/api/v1/config/apply` | the server half of `sindri diff` and `sindri apply` |
| `/api/v1/tokens` | machine tokens for CI: issue, list, revoke |
| `/api/v1/audit` | the lifecycle ledger, newest first, filtered to readable repositories |
| `/api/v1/gc`, `/api/v1/gc/candidates`, `/api/v1/jobs/{id}` | garbage collection and its dry run, administrators only |
| `/auth/login`, `/auth/callback`, `/auth/logout`, `/auth/whoami` | browser sign-in through the identity provider |
| `/ui/...` | read-only web interface |
| `/healthz`, `/readyz` | liveness and readiness |
| `/metrics` | Prometheus |

`/healthz` checks nothing: a liveness probe that fails during a database outage gets the
process killed for someone else's problem. `/readyz` checks the database, and the blob
store when the backend offers a cheap probe. Both report which check failed, never why:
the cause goes to the log, tied to the same request id.

### Authentication and authorization

Every route except `/healthz`, `/readyz`, `/metrics`, and `/auth/*` needs a
credential. There is no anonymous pull in v1. A credential arrives as a Bearer
token or as the password of HTTP Basic, which is what docker, pip, and twine
send. Its shape picks the verifier:

- a `sindri_` machine token, looked up by hash
- an identity-provider JWT, verified against the provider's cached JWKS
- the bootstrap token

Users' groups come from their token on each request and are never stored.

`auth.Authorize(principal, repository, role)` is the one authorization
function, and every surface calls it: both adapters, the management API, and
the UI guard. It is pure and reads only the repository row. Write implies read
and admin implies everything. A caller who cannot read a repository is told it
does not exist. A virtual repository answers only from members the caller can
read, and a name claimed by an unreadable hosted member is a miss, never a
proxy fall-through (I5). The browser session cookie is honoured only on GET and
HEAD, by the read-only UI and `/auth/whoami`. See ADR 18.

Routing uses `chi`. The stdlib `ServeMux` cannot express the OCI routes, because an OCI
repository name legitimately contains slashes in the middle of a path
(`/v2/team/service/blobs/sha256:...`), and stdlib wildcards only span path segments at the
end of a pattern.

Validation happens at write time in the reconciler, not per request. Downloads and
installs are the hot path and must stay a lookup. A misconfiguration should fail in a pull
request, not three weeks later on a developer's machine.

---

## 8. Configuration as code

Two levels of concern. Teams get autonomy, the organisation keeps guardrails. But a
single team running Docker Compose has no organisation to govern, and asking them to
write enveloped manifests and run a separate apply step is friction with nothing bought
for it.

So the configuration surface scales the same way the deployment does: one file for one
team, a directory of manifests for many, with one schema and one loader underneath.

### 8.1 One file, two forms

There is a **short form** and a **long form**. The short form desugars into the long
form during loading. The reconciler, the validator, and everything downstream see only
the long form.

This is the same arrangement Compose itself uses for `ports: ["80:80"]` versus its long
object form, and it works for the same reason: one representation to implement against,
two ways to write it.

A complete single-team setup:

```yaml
# sindri.yaml
listen:   :8080
storage:  ./data                          # or s3://sindri-blobs
database: postgres://sindri@localhost:55432/sindri

repos:
  internal-py:  { protocol: pypi }                 # hosted
  pypi:         { upstream: pypi }                 # proxy, from a preset
  python:       { members: [internal-py, pypi] }   # virtual, order is precedence

  internal-oci: { protocol: oci }
  dockerhub:    { upstream: dockerhub }
  docker:       { members: [internal-oci, dockerhub] }

lifecycle:
  stages: [dev, integration, prod]
  gates:
    prod: [sbom, signature]
```

The same `python` repository in long form, which is what the loader produces and what a
multi-team installation writes directly:

```yaml
kind: Repository
metadata:
  name: python
spec:
  type: virtual
  protocol: pypi
  members:
    - ref: hosted/internal-py
    - ref: proxy/pypi
```

And the organisation level, which exists only when there is an organisation to govern:

```yaml
kind: OrgPolicy
spec:
  mode: hybrid                 # centralized | delegated | hybrid
  mandatoryRepos:
    - name: approved-base-images
      locked: true             # teams reference it, they cannot override it
```

An absent `OrgPolicy` means `mode: delegated` with no mandatory repositories. A single
team therefore never writes one, and adopting governance later is adding a file rather
than migrating a format.

The long form has three kinds: `Repository`, `OrgPolicy`, and `Lifecycle`. The short
`lifecycle:` section desugars into the last. A team's repositories carry
`metadata.namespace`, which prefixes the name: `namespace: payments`, `name: python` is
the repository `payments/python`. `OrgPolicy.spec.namespaces` names who may apply each
namespace. They may change only that namespace, and never a `locked` repository
(ADR 17).

### 8.2 Inference rules

Repository type is inferred from which key is present, so `type:` is never required:

| Keys present | Type |
|---|---|
| `members` | virtual |
| `upstream` | proxy |
| neither | hosted |
| both | error, reported as a conflict |

A virtual repository's protocol is inferred from its members, and **all members must
share one protocol**. That is not a shortcut, it is a real constraint: one URL serves one
wire protocol, so a virtual repository mixing PyPI and OCI members could not be served at
all.

Bare member names resolve within the same file. A prefix (`org/`, another team's
namespace) is required to reach across a boundary, which makes every cross-boundary
reference visibly one. In the long form a member is `ref: <type>/<ref>`, so the
expanded form names every member's type. Inside namespace `payments`,
`hosted/internal` is `payments/internal`, `hosted/org/base` is the organisation's
`base`, and `hosted/search/index` is team `search`'s `index`.

`upstream` accepts a named preset (`pypi`, `dockerhub`, `ghcr`, `npm`, `maven-central`)
or an explicit object. Presets carry the upstream URL and sensible TTLs so that nobody
hand-types the pypi.org simple index. Preset expansions are visible through
`sindri config show --expanded`, and are pinned in the binary so an upstream URL change is
a release, not a surprise.

### 8.3 What keeps two forms from becoming two systems

Desugaring must be **one-way and total**. The short form has no representational power the
long form lacks, nothing downstream branches on which form was written, and a golden test
asserts that the short and long spellings of the same setup produce identical internal
trees. Without that discipline two syntaxes become two parsers that drift, which is a real
failure mode of configuration surfaces that grow sugar after the fact.

Two commands make the sugar honest rather than magic:

- `sindri config check` validates statically, with no database and no server, so it runs
  in CI before anything is deployed. Errors name the file, the line, and the field.
- `sindri config show --expanded` prints the desugared long form. This is how a reader
  sees exactly what the short form did, how a team learns the explicit form, and the first
  thing to reach for when the resolution order is not what someone expected.

### 8.4 Two triggers, one reconciler

The reconciler is the same in both deployments. Only what invokes it differs.

```
Compose:     sindri serve --config sindri.yaml    # reconciles on boot and on SIGHUP
Enterprise:  sindri apply -f manifests/           # reconciles from CI, against a server
```

Reconcile-on-boot is what removes the apply step for a single-node install, and it is the
larger half of making this feel native to Compose rather than borrowed from Kubernetes.
The config file is read-only to the process, so it mounts read-only into a container.

Repositories created through the management API are marked API-managed and are
distinguished from config-managed ones, so the reconciler and the API never fight over the
same row. Among config-managed rows, each records which trigger owns it, and neither
trigger changes the other's.

A reconcile is one transaction: policy, stages, and repositories commit together.
`sindri diff` is the same reconcile, rolled back. A config-managed repository that the
configuration no longer lists is reported as orphaned. Only `sindri apply --prune` deletes
it, so removing a line from a manifest never silently deletes packages. A repository's
type and protocol are immutable. Changing either is a deliberate delete and re-create.

### 8.5 Power without inheritance chains

Environment substitution with defaults, because one file should work across dev and prod:

```yaml
storage: s3://${SINDRI_BUCKET:-sindri-blobs}
```

A `defaults:` block keyed by repository type, which covers most repetition:

```yaml
defaults:
  proxy:
    ttl: 1h
    negativeTtl: 5m
  hosted:
    read: [team:platform]
```

Durations are Go duration strings, parsed with `time.ParseDuration`, so `1h` and `5m` are
valid and a bare `3600` is **rejected** rather than assumed to be seconds. Guessing a unit
for the user is how a five-minute negative cache becomes a one-hour one.

Deliberately **no `extends:`**. Per-type defaults plus YAML's own anchors and aliases cover
the real cases, and transitive template inheritance is how configuration languages become
unreadable. If per-type defaults turn out to be insufficient, that needs an ADR and not a
convenience feature.

### 8.6 The properties that must survive the sugar

**Manifests live in Git.** A reconciler converges the database onto them and reports
drift, so every permission change goes through code review instead of an invisible admin
click.

**Validation happens at write time**, not per request. The download path stays a lookup,
and a misconfiguration fails in a pull request rather than three weeks later on a
developer's machine.

**Structure is static, membership is live.** The manifest names groups such as
`team:payments`; who belongs to that group is resolved against the identity provider at
request time. Configuration is never re-applied because somebody joined a team.

**The three repository types stay visible.** Sugar makes declaring hosted, proxy, and
virtual repositories cheap. It must never make them disappear into a single entry, however
tempting `python: {hosted: ..., proxy: ...}` looks. The proxy/hosted/virtual separation
*is* the dependency-confusion defence of invariant I5, and a configuration surface that
hides it teaches users to stop reasoning about it. The guarantee then erodes socially even
while the code still enforces it.

---

## 9. Operations

**Modes.** One binary. `sindri serve` runs everything in one process, which is the Compose
story. `--api-only` and `--worker-only` split the same binary for Kubernetes and are
post-v1, but no v1 design may assume that API and worker share a process. Background work
goes through a queue table from the start.

**Worker.** `serve` runs a worker unless `worker: false`. It runs garbage
collection when one is requested, or on `worker.gc.interval` when that is set;
it is off by default. Every `worker.retention.interval` it evicts stale proxy
cache entries and purges abandoned uploads. The only background work outside
the queue is expiring upload sessions held in a process's memory, which no
other process could reach. See ADR 20.

**Observability.** Structured logs via `log/slog`, Prometheus metrics, a request id on
every log line and response.

**Errors.** One error model across the management API. Protocol adapters translate to
their own specified error shapes at the edge, because an OCI client expects the OCI error
body and a broken shape breaks real clients.

**Migrations** are embedded in the binary and applied on startup, so upgrading is
replacing one binary.

---

## 10. Open Core boundary

Drawn deliberately, and drawn here so that the free and paid split never leaks into the
core's structure: everything a single team needs to work productively is open source under
Apache 2.0. Everything an organisation needs to govern many teams is commercial.

Open: all protocol adapters, both storage backends, basic RBAC, full single-node
operation, lifecycle and promotion, evidence.

Commercial: SSO beyond basic OIDC (SAML, LDAP), audit logging with retention guarantees,
multi-region replication, the advanced policy engine, managed hosting.

No commercial feature may require a different core. If one does, the boundary is in the
wrong place.
