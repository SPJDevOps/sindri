# Sindri Roadmap to v1

This is the build plan. It turns [pitch.md](pitch.md) into ordered milestones, each small
enough for one focused working session and each ending in a gate you can **run** rather
than eyeball.

Design lives in [ARCHITECTURE.md](ARCHITECTURE.md). Conventions live in
[CLAUDE.md](CLAUDE.md). Decisions live in [docs/adr/](docs/adr/).

---

## How to use this document

**One milestone per session, one branch per milestone.** Start a session by naming the
milestone: "implement M3 from roadmap.md". Do not start M4 in the same branch as M3.

**The gate is the definition of done.** Every milestone ends with commands. If the
commands do not pass, the milestone is not finished, regardless of how complete the code
looks.

**Milestones are ordered by dependency, not by preference.** M3 needs M2's spine. M5 needs
two working adapters to have anything to resolve between. Do not reorder without reading
what the later gate actually requires.

**Vertical slices.** Each milestone from M1 onward leaves the system runnable and
demonstrable. There is no milestone whose output is only internal plumbing you cannot
exercise.

### Definition of done, every milestone

- [ ] The milestone's gate commands pass locally
- [ ] `make ci` is green, and CI is green on the pull request
- [ ] New decisions are written up as an ADR in `docs/adr/`
- [ ] `ARCHITECTURE.md` updated if the schema or a component boundary changed
- [ ] `CLAUDE.md` updated if a convention changed
- [ ] This file's milestone checklist is ticked

---

## Scope of v1

| Area | In v1 | Deferred |
|---|---|---|
| Protocols | OCI, PyPI | npm, Maven, Helm |
| Repository types | hosted, proxy, virtual | — |
| Storage | filesystem, S3-compatible | GCS, Azure |
| Deployment | single binary, Docker Compose | `--api-only`/`--worker-only`, Helm chart |
| Identity | subjects, ownership, provenance | — |
| Lifecycle | stages, promotion, gates | time-based auto-promotion |
| Evidence | attach and list SBOMs, signatures | running scanners, verifying signatures |
| Config | YAML kinds, reconciler, drift report | Git-polling operator, webhooks |
| Auth | OIDC login, basic RBAC | SAML, LDAP, fine-grained per-path ACLs |
| UI | read-only browse | administration in the browser |

**Why these deferrals.** npm, Maven, and Helm are deferred because the point of the shared
core is that a third adapter is cheap; two adapters prove the abstraction and a fifth only
proves it again at four times the cost. The split serve modes are deferred because the
queue-table design in M8 is what makes them trivial, and shipping them earlier means
maintaining a scaling story nobody is using yet. Scanning is deferred because attaching
and gating on evidence is the hard, differentiating half; running Trivy is an integration
anyone can add later.

---

## Milestones

### M0 — Repository bootstrap ✅

**Goal.** Empty folder to green CI, with the design written down before the first line of
product code.

- [x] `git init`, `main` branch, Go module
- [x] `cmd/sindri` skeleton with `version` and `help`
- [x] `Makefile`: `build`, `test`, `lint`, `fmt`, `cover`, `tidy`, `up`, `down`, `ci`
- [x] `.golangci.yml` with a security-leaning linter set
- [x] GitHub Actions: lint, unit tests, cross-platform build
- [x] Apache 2.0 licence, README
- [x] `roadmap.md`, `ARCHITECTURE.md`, `CLAUDE.md`, first ADRs
- [x] Compose file with Postgres and MinIO for local development

**Gate.** Verified on 2026-09-21 with Go 1.27.1, Docker 29.8, Compose v5.5.1.
```bash
make ci                # gofmt, go vet, golangci-lint, race tests, build
./bin/sindri version   # prints build metadata
make up                # Postgres 18 and MinIO
docker compose -f deploy/compose/docker-compose.yml ps   # both healthy
make down
```

**Local ports.** Host ports default high so they do not collide with anything you already
run: Postgres on **55432**, the MinIO S3 API on **59000**, its console on **59001**.
Override with `SINDRI_PG_PORT`, `SINDRI_S3_PORT`, `SINDRI_S3_CONSOLE_PORT`. The MinIO
bucket `sindri-blobs` is created automatically by the `minio-init` one-shot service.

**Install the linter** with `make tools`. Sindri pins golangci-lint v2, whose config
format is not compatible with v1. If you already have v1 on your `PATH` it will reject
`.golangci.yml`, so install v2 into a separate `GOBIN` if you need to keep v1 for another
project.

**Before anything else:** the module path is currently `github.com/spj/sindri`. If the
repository will live somewhere else, change it now, while it costs one command:
```bash
go mod edit -module github.com/YOUR_ORG/sindri
grep -rl 'github.com/spj/sindri' --include='*.go' --include='Makefile' --include='*.yml' . \
  | xargs sed -i '' 's|github.com/spj/sindri|github.com/YOUR_ORG/sindri|g'
```

---

### M1 — Storage, CAS, and a process that boots ✅

**Goal.** `sindri serve` runs, connects to Postgres, and can store and retrieve a verified
blob on two different backends.

**Scope.**
- [x] Command tree with `cobra`; `serve` reads `sindri.yaml` with `${VAR:-default}`
  environment substitution
- [x] `storage.BlobStore` interface per ARCHITECTURE.md section 6
- [x] `fsstore`: content-addressed directory fan-out (`ab/cd/abcd...`) to avoid
  million-entry directories
- [x] `s3store` on `aws-sdk-go-v2`, tested against MinIO, path-style addressing
  configurable
- [x] `cas`: streaming digest computation, staged uploads, digest verification before
  commit, dedup on put
- [x] `db`: `pgx/v5` pool, `goose` migrations embedded via `embed.FS`, applied on startup
  under an advisory lock
- [x] `/healthz`, `/readyz` (readiness checks the database, and storage where the backend
  can be probed), `/metrics`, structured `slog` logging with a request id
- [x] Graceful shutdown on SIGTERM

Sindri's own default listen address is `:8080`, overridable, and the default development
database URL is `postgres://sindri:sindri@127.0.0.1:55432/sindri?sslmode=disable` to match
the Compose stack.

**Out of scope.** Any protocol adapter. Any notion of a repository.

**One config file, for the whole project's life.** M1 needs only `listen`, `storage`, and
`database`, but this is the same file and the same loader that M7 extends with `repos:`,
`lifecycle:`, and `defaults:`. Build it as a section-based schema with an unknown-key
error from the start. The alternative is that M7 arrives to find a throwaway format in
place and Sindri ships with two configuration systems, which is the most likely
self-inflicted wound in this roadmap. See ARCHITECTURE.md section 8.

**Watch out for.** Write staged bytes to a staging key and only rename to the
content-addressed key after the digest matches. A partially uploaded blob committed under
its claimed digest is corruption that every later integrity check will trust.

**Gate.** Verified on 2026-09-21 with Go 1.27.1, Docker 29.8, golangci-lint v2.13.2.
```bash
make up
make test                      # unit tests pass
make test-integration          # storage conformance suite passes on BOTH backends
./bin/sindri serve &           # starts, applies migrations
curl -fsS localhost:8080/healthz && curl -fsS localhost:8080/readyz
curl -fsS localhost:8080/metrics | head
```
The conformance suite is one table-driven test run twice, once per backend, via
testcontainers. Two suites that drifted apart would defeat the purpose.

It is `TestBlobStore_Conformance` in `internal/storage/conformance_integration_test.go`,
over a suite that lives in `internal/storage/storagetest`. The in-memory store that stands
in for a backend in higher-level tests runs the same suite, so a fake cannot drift from
the thing it fakes.

**What M1 decided.** Staged uploads became part of `storage.BlobStore` rather than a local
temporary file inside `cas` ([ADR 8](docs/adr/0008-staged-uploads-belong-to-the-blobstore.md)),
and the configuration loader was built in full rather than as a throwaway
([ADR 9](docs/adr/0009-one-configuration-loader-from-m1.md)). `cas` does not touch
Postgres: dedup is a `Stat` against the backend, and the `blobs` table stays in M2 where
this document puts it. The first migration is a documented baseline whose job is to prove
that embedded migrations apply on startup.

Two things changed outside the milestone's scope because they were broken: MinIO's images
are pinned rather than floating, and `make lint` now covers the `integration` build tag.
That pin has since moved twice as upstream closed anonymous pulls, first to `quay.io` and
then to `bitnamilegacy/minio` ([ADR 11](docs/adr/0011-minio-test-image-source.md)).

The `integration` job in `.github/workflows/ci.yml` is enabled from here.

---

### M2 — Domain spine and management API ✅

**Goal.** Repositories, packages, versions, and files are real rows, manageable over HTTP.

**Scope.**
- [x] Migrations for the placement layer: `repositories`, `repository_members`, `remotes`,
  `packages`, `versions`, `files`, `blobs`, `tags`, `manifest_children`
- [x] `sqlc` wired into `make generate`, typed queries in `internal/db`
- [x] `internal/registry`: publish, resolve, list, delete, enforcing invariant I1
- [x] `/api/v1` with repository CRUD, package and version listing, cursor pagination
- [x] One error model: stable machine-readable code, human message, request id
- [x] Static bearer-token auth as an explicit placeholder for M7, and a middleware seam that
  M7 fills rather than replaces
- [x] Placement check helper that every read path must call, per invariant I2

**Out of scope.** Protocols, proxying, lifecycle.

**Watch out for.** Introduce the placement check now, as the only way to reach a blob, even
though with one repository type it looks like pointless indirection. Adding it after two
adapters exist means auditing every handler instead of writing one function.

**Gate.** Verified on 2026-09-24 with Go 1.27.1, Docker 29.1, golangci-lint v2.13.2.
```bash
make generate
make test
make test-integration
make ci
```
An end-to-end test that, over HTTP only: creates a hosted repository, uploads a blob,
lists it, reads it back byte-identical, fails to read it via a *different* repository's
path with 404, and deletes it. That fourth assertion is invariant I2 and it is the
important one. See `TestAPI_PlacementGatesCrossRepoReads`.

---

### M3 — OCI adapter, hosted ✅

**Goal.** `docker push` and `docker pull` work against Sindri.

**Scope.**
- [x] `GET /v2/` version check
- [x] Blob upload: monolithic `POST`, chunked `PATCH`/`PUT` with correct `Range` and
  `Location` handling, `DELETE` of an upload session
- [x] Blob `GET`/`HEAD` with range support
- [x] Manifest `GET`/`HEAD`/`PUT`/`DELETE`, including image indexes and multi-arch
- [x] Tag listing with pagination and `Link` headers
- [x] `manifest_children` populated on every manifest push, covering index-to-manifest and
  manifest-to-layer edges
- [x] OCI-shaped error bodies with the specification's error codes
- [x] Struct types from `opencontainers/image-spec`; no hand-rolled manifest JSON

**Out of scope.** Proxying Docker Hub, referrers API, cross-repository blob mounts.

**Watch out for.** Chunked upload is where naive implementations fail conformance: offsets
must be tracked server-side, and the `Range` response header is inclusive of both bounds,
which is off by one from every Go slice you will write next to it.

**Gate.** Verified on 2026-09-24 with Go 1.27.1, Docker 29.8, golangci-lint v2.13.2.
Upstream distribution-spec conformance (`OCI_TEST_PUSH/PULL/CONTENT_MANAGEMENT`)
passed 60/60 (20 skipped: referrers and related). Docker Desktop needs
`host.docker.internal:8080` in `insecure-registries` for `docker push`; use
[`scripts/oci-docker-gate.sh`](scripts/oci-docker-gate.sh). Round-trip is also
covered by `TestOCI_PushPullAndPlacementGate`.
```bash
# create demo/app via /api/v1 (see scripts/oci-docker-gate.sh), then:
docker build -t host.docker.internal:8080/demo/app:1.0 testdata/demo
docker push  host.docker.internal:8080/demo/app:1.0
docker rmi   host.docker.internal:8080/demo/app:1.0
docker pull  host.docker.internal:8080/demo/app:1.0          # round-trips

# Upstream conformance, push + pull + content management workflows
./scripts/oci-conformance.sh
```
Check in the conformance invocation as a script so the next session does not reconstruct
it.

---

### M4 — PyPI adapter, hosted ✅

**Goal.** `twine upload` and `pip install` work against Sindri.

**Scope.**
- [x] PEP 503 HTML simple index, both the root listing and per-project pages
- [x] PEP 691 JSON index with correct content negotiation
- [x] PEP 503 name normalization, enforced as the uniqueness key
- [x] Upload via twine's multipart `POST`, including the `:action` field and hash fields
- [x] File download with correct content type and `Content-Length`
- [x] Metadata extraction from wheel `METADATA` and sdist `PKG-INFO`
- [x] `data-requires-python` and hash fragments on index links, since pip relies on both

**Out of scope.** Proxying pypi.org, which is M5. Yanking. Per-file GPG signatures.

**Watch out for.** Normalize on write and on read, and store the normalized form as the
key. Accepting `Foo.Bar` and `foo_bar` as two projects is the bug that makes the
name-claim rule in M5 unenforceable, and by then it is a data migration.

**Gate.** Verified on 2026-09-26. Create `internal` via `/api/v1` (see
[`scripts/pypi-twine-gate.sh`](scripts/pypi-twine-gate.sh)); PyPI repositories
are not auto-created (ADR 12). Round-trip is also covered by
`TestPyPI_UploadInstallAndPlacementGate`.
```bash
# create internal via /api/v1 (see scripts/pypi-twine-gate.sh), then:
python -m twine upload --repository-url http://localhost:8080/pypi/internal/upload \
  -u token -p "$SINDRI_TOKEN" dist/*

python -m venv /tmp/v && /tmp/v/bin/pip install \
  --index-url http://localhost:8080/pypi/internal/simple/ demo-package
/tmp/v/bin/python -c 'import demo_package; print(demo_package.__version__)'

curl -fsS -H 'Accept: application/vnd.pypi.simple.v1+json' \
  http://localhost:8080/pypi/internal/simple/demo-package/ | jq .
```
Or run the checked-in gate script:
```bash
./scripts/pypi-twine-gate.sh
```

**What M4 decided.** Hosted PyPI uses `files` placement (no `repository_blobs`),
does not auto-create repositories, keeps simple/download anonymous, and accepts
Basic on upload without validating identity until M7
([ADR 12](docs/adr/0012-pypi-hosted-adapter.md)). Downloads are served at
`/pypi/{repo}/packages/{project}/{filename}`.

An upload must prove what it is before it is placed: the wheel `METADATA` or
sdist `PKG-INFO`, the filename, and the form fields must agree on project and
version, and a claimed `sha256_digest` is checked against the received bytes.
A published file is immutable, so a re-upload of identical bytes is idempotent
and one with different bytes is 409. Adapter path markers (`simple`, `packages`,
`upload`, `blobs`, `manifests`, `tags`, `uploads`) are reserved repository name
segments, which is what makes a slash-containing repository name unambiguous to
route ([ADR 13](docs/adr/0013-pypi-write-time-validation-and-immutable-files.md)).
The gate script stamps a fresh version per run, because immutability plus
non-reproducible wheels would otherwise make it single-use.

The index describes itself and never the request: download links are
root-relative, so no `Host` header reaches a link. Index URLs are canonical
(a 301 onto the trailing-slash, normalized form), the root listing walks every
page rather than stopping at the first, content negotiation follows `q=` values,
and the JSON API declares api-version 1.1 (it carries PEP 700's `versions` and
`size`) while the HTML API carries a PEP 629 meta tag saying 1.0.

---

### M5 — Proxy and virtual repositories ✅

**Goal.** The three repository types work together, and dependency confusion is
structurally impossible.

**Scope.**
- [x] `remotes` configuration: upstream URL, positive and negative TTL. A credentials
  reference is stored and applied only to the upstream's own origin, but nothing supplies
  secrets yet, so the API rejects one: M5 proxies public upstreams only. The secret source
  is decided and lands in M7 ([ADR 15](docs/adr/0015-upstream-credentials-source-deferred-to-m7.md))
- [x] On-miss upstream fetch that streams to the client while caching into the CAS, so a
  large first pull is not serialised behind the write
- [x] Negative caching, upstream timeouts, and graceful behaviour when upstream is down but
  content is cached
- [x] `internal/resolve`: ordered member walk, first hit wins, invariant I5
- [x] The **name-claim rule** from ARCHITECTURE.md section 4
- [x] Reconciler-time rejection of lifecycle configuration on a proxy repository, invariant I4
- [x] Proxy support for both adapters: pypi.org and Docker Hub

**Out of scope.** Eager background mirroring. Cache eviction policy beyond TTL.

**Watch out for.** Upstream failure must not poison the cache, and a 404 must be cached
separately from a 503. Treating them alike either hammers a healthy upstream or hides a
package that appeared five minutes ago.

**Gate.** Verified on 2026-09-30. Decisions in
[ADR 14](docs/adr/0014-proxy-cache-and-virtual-resolution.md).
```bash
make test-integration
```
Integration tests that assert:
1. `pip install` through `proxy/pypi-public` succeeds against a stubbed upstream
2. The second install serves from cache with **zero** upstream requests, asserted on the
   stub's request count
3. A virtual repository with `[hosted/internal, proxy/pypi-public]` serves the hosted
   package when both members offer the same name
4. `TestDependencyConfusion_HostedNameIsNeverServedFromProxy`: upstream offers version
   `99.0.0` of a name the hosted member claims at `1.0.0`; resolution returns the hosted
   artifact and never contacts upstream
5. Applying a manifest that puts a lifecycle stage on a proxy repository fails validation

Test 4 is a permanent regression guard. It is named explicitly so that nobody deletes it
while "simplifying tests". Its OCI half is
`TestDependencyConfusion_OCIHostedImageIsNeverServedFromProxy`, guarded the same way.

The five items above were not enough on their own: the first M5 draft passed them while
hiding every uncached version of a proxied PyPI project, pinning proxied OCI tags forever,
and buffering whole layers in memory. The integration suite in `internal/resolve` also
covers, and each test was checked to fail with its bug reintroduced:
- refresh after TTL expiry (`TestProxyOCI_TagRefreshesAfterTTL`,
  `TestProxyPyPI_IndexListsUpstreamFilesAfterOneIsCached`)
- a 5xx never replacing a hit, and stale content served while upstream is down
  (`TestProxy_StaleIndexServedWhenUpstreamDown`)
- streaming, verification, HEAD and Range (`TestProxyOCI_BlobStreamsBeforeUpstreamFinishes`,
  `TestProxyOCI_BadBlobAbortsAndIsNotCached`, `TestProxy_BadDigestNotPlaced`,
  `TestProxyOCI_HeadAndRangeOnUncachedBlob`)
- the Docker Hub auth shape and credential scoping (`TestProxyOCI_BearerChallenge`,
  `TestProxyPyPI_CredentialsStayOnUpstreamOrigin`)

A stub cannot prove the real upstreams work, so the gate also includes a live check against
Docker Hub and pypi.org, with `make up`, `make build`, and `./bin/sindri serve` running:
```bash
curl -fsS -X POST localhost:8080/api/v1/repositories -H "Authorization: Bearer $SINDRI_TOKEN" \
  -d '{"name":"dockerhub","type":"proxy","protocol":"oci","upstream_url":"https://registry-1.docker.io","ttl_seconds":3600,"negative_ttl_seconds":300}'
curl -fsS -X POST localhost:8080/api/v1/repositories -H "Authorization: Bearer $SINDRI_TOKEN" \
  -d '{"name":"pypi-public","type":"proxy","protocol":"pypi","upstream_url":"https://pypi.org/simple","ttl_seconds":3600,"negative_ttl_seconds":300}'
docker pull host.docker.internal:8080/dockerhub/alpine:latest          # library/ added
docker pull host.docker.internal:8080/dockerhub/library/alpine:latest
pip install --index-url http://localhost:8080/pypi/pypi-public/simple/ requests
pip install --index-url http://localhost:8080/pypi/pypi-public/simple/ 'requests==2.31.0'  # other versions still listed
```

---

### M6 — Identity, lifecycle, and evidence ✅

**Goal.** The differentiating feature. Artifacts carry trust status, and promotion moves
no bytes.

**Scope.**
- [x] Migrations for the identity layer: `subjects`, `subject_owners`, `provenance`,
  `evidence`, `lifecycle_stages`, `promotion_gates`, `lifecycle_events`
- [x] `subjects` rows created on publish, with ownership and provenance capture from
  push-time metadata and headers
- [x] Stage definitions, ordered, organisation-configurable
- [x] `POST /api/v1/subjects/{digest}/promote` writing one append-only event
- [x] Gate evaluation before promotion: for example, no promotion to `prod` without an
  attached SBOM and signature
- [x] Evidence attach and list; evidence bodies stored as blobs in the CAS like anything else
- [x] Revocation as an event, never a delete
- [x] `GET /api/v1/decisions?digest=&environment=` returning an approval answer from the
  current-state view, on one indexed query
- [x] Invariant I4 enforced on the promotion path as well as at reconcile time

**Out of scope.** Running scanners. Cryptographic signature verification, which is
post-v1; v1 stores and gates on evidence presence and type.

**Watch out for.** Resist adding an `UPDATE` to `lifecycle_events` when a "current status"
query feels slow. The view plus an index is the answer. A mutable status column destroys
the audit trail that makes this feature worth building.

**Gate.** Verified on 2026-10-01 with Go 1.27.1, golangci-lint v2.13.2. Also smoke-tested
against `./bin/sindri serve` on the Compose stack.
```bash
make test-integration
```
An integration test that walks the whole story:
1. Push an image, confirm a `subjects` row with owner and provenance
2. Attach an SBOM as evidence
3. Attempt promotion to `prod`, which requires SBOM **and** signature: expect refusal
   naming the missing gate
4. Attach a signature, promote to `prod`: expect success
5. `GET /api/v1/decisions` for that digest and `prod`: approved; for an unrelated digest:
   denied
6. Revoke, then query again: denied, and the original approval event still present
7. **Assert the blob count in storage is unchanged across all of the above.** Invariant I3
   is only real if a test enforces it.

**What M6 decided.** Stages and gates are set through `PUT /api/v1/lifecycle/stages` until
M7's reconciler writes the same rows through the same call. The owner is the hosted
repository (`repository:<name>`), the only owner verifiable while pushes are anonymous.
A gate is "evidence of this type is present"; stage order is for display only. An
unknown environment is denied, not 404. The ledger is append-only in the database itself
(a trigger), and its foreign keys do not cascade, so a repository with promotion history
cannot be deleted ([ADR 16](docs/adr/0016-lifecycle-gates-and-evidence-in-v1.md)).

The gate is `TestLifecycle_PromotionStory` in `internal/api`. Step 7 is asserted in its
honest form: attaching evidence necessarily stores the document, so each attach must add
exactly one blob, and the refused promotion, the promotion, the decisions, and the
revocation must add zero, counted both in storage and in the `blobs` table. The
surrounding suite covers append-only enforcement, placement on the promotion path,
stage removal with history, repository deletion with history, the decision query's use of
the index, and, in `internal/resolve`, that proxy-cached content gets no subject and a
proxy repository cannot promote. Those two and the gate test were each checked to fail
with their bug reintroduced.

---

### M7 — Config as code, reconciler, OIDC and RBAC ✅

**Goal.** Governance as code, in a form a one-person team will actually write. Real
authentication. Guardrails teams cannot route around.

Read ARCHITECTURE.md section 8 in full before starting. This milestone is where the
project either stays light for small teams or acquires the ceremony it was built to avoid.

**Scope, configuration.**
- [x] Extend `sindri.yaml` from M1 with `repos:`, `lifecycle:`, and `defaults:` sections
- [x] **Short form** with type inference: `members` means virtual, `upstream` means proxy,
    neither means hosted, both is a named error
- [x] Virtual protocol inferred from members, with all members required to share one protocol
- [x] Named upstream presets: `pypi`, `dockerhub`, `ghcr`, `npm`, `maven-central`, pinned in
    the binary, each overridable by an explicit object
- [x] **Long form** `Repository` and `OrgPolicy` kinds, multi-document manifest directories
- [x] **One-way, total desugaring** into the long form; nothing downstream may branch on which
    form was written
- [x] `defaults:` keyed by repository type. No `extends:`, per ADR 7
- [x] Bare member names resolve locally; a prefix is required to cross a namespace boundary
- [x] `sindri config check`: static validation, no database, no server, errors naming file,
    line, and field
- [x] `sindri config show --expanded`: prints the desugared long form
- [x] `credentials:` section for proxy upstream secrets, with `${VAR}` substitution, per
    [ADR 15](docs/adr/0015-upstream-credentials-source-deferred-to-m7.md): a config-backed
    `remote.Credentials` wired into `serve`, an unknown reference rejected at reconcile time
    (replacing M5's blanket rejection), secrets redacted by `config show`, and no anonymous
    read access to a proxy that holds credentials

**Scope, reconciliation.**
- [x] One reconciler, two triggers: `serve --config` converges on boot and on `SIGHUP`, and
    `sindri apply -f <dir>` converges from CI against a running server
- [x] `sindri diff -f <dir>` reports drift without changing anything
- [x] Config-managed and API-managed repositories distinguished so the two never fight over a
    row
- [x] `OrgPolicy` modes `centralized`, `delegated`, `hybrid`; an absent `OrgPolicy` means
    `delegated` with no mandatory repositories
- [x] `locked: true` repositories: referenceable, never overridable
- [x] Invariant I4 enforced here: lifecycle configuration on a proxy repository is rejected at
    reconcile time

**Scope, identity.**
- [x] OIDC authorization code flow, token validation, JWKS caching with refresh
- [x] Group-to-role mapping; group membership resolved live at request time, never stored
- [x] RBAC enforced on every route: both adapters, the management API, and the UI
- [x] Machine tokens for CI, scoped to a repository and a role

**Out of scope.** Git polling; v1 reconciles on boot or from CI, and an operator can come
later without changing the format. SAML and LDAP are commercial.

**Watch out for.**

*Two syntaxes becoming two parsers.* The guard is a golden test asserting that the short
and long spellings of the same setup produce byte-identical internal trees. Without it
they drift, and then every feature costs twice.

*Sugar that hides the three repository types.* Do not add `python: {hosted: ..., proxy:
...}`, however much shorter it looks. That separation is the dependency-confusion defence
of invariant I5, and hiding it teaches people to stop reasoning about it.

*Validation drifting onto the request path.* It belongs at reconcile time. Downloads and
installs must stay a lookup.

*Caching group membership.* A revoked team membership would keep granting access until
something re-applied, which recreates exactly the invisible-admin problem
config-as-code exists to remove.

**Gate.** Verified on 2026-10-02 with Go 1.27.1, golangci-lint v2.13.2, by
`scripts/m7-config-gate.sh` against the Compose stack, which includes a real
`docker login`, push, and pull. The twine and pip gate (`scripts/pypi-twine-gate.sh`)
was re-run with credentials.
```bash
# Short form: a complete setup in one file, no apply step
./bin/sindri config check  -f testdata/config/sindri.yaml
./bin/sindri config show --expanded -f testdata/config/sindri.yaml
./bin/sindri serve --config testdata/config/sindri.yaml &   # reconciles on boot
docker push localhost:8080/docker/demo/app:1.0              # repos exist, unprompted
kill -HUP $!                                                # re-reconciles

# Long form: a manifest directory from CI
./bin/sindri apply -f testdata/manifests/    # converges, reports what changed
./bin/sindri diff  -f testdata/manifests/    # clean on the second run

make test-integration
```
Integration tests, against a stub OIDC provider, that assert:
1. The short-form file and the equivalent manifest directory produce **identical**
   database state, which is the desugaring guarantee made executable
2. `config check` rejects a repository with both `members` and `upstream`, and rejects a
   virtual repository whose members disagree on protocol, each with a message naming the
   file and field
3. `serve --config` creates repositories on boot with no apply step, and `SIGHUP`
   re-reconciles
4. Re-applying is a no-op, and manual drift is reported
5. A team manifest that overrides a `locked: true` org repository is **rejected**
6. Lifecycle configuration on a proxy repository is rejected, invariant I4
7. A user in `team:payments` can pull and cannot push
8. An anonymous `docker pull` and an anonymous `pip install` are both refused
9. Adding a user to a group in the identity provider grants access with **no** re-apply

Test 1 is the one that keeps the two forms honest for the life of the project.

The tests that cover each gate item:

| # | Test |
|---|---|
| 1 | `TestReconcile_ShortAndLongFormsProduceIdenticalDatabaseState` (`internal/reconcile`) and the tree-level `TestManifests_ShortAndLongFormsAreIdentical` (`internal/config`) |
| 2 | `TestDesugar_Rejects/MembersAndUpstream` and `.../VirtualMembersDisagreeOnProtocol`, plus `TestValidateSet_Rejects/VirtualMixesProtocols` for the long form |
| 3 | `TestServe_ReconcilesOnBootAndOnSIGHUP` (`cmd/sindri`) |
| 4 | `TestReconcile_SecondRunIsNoOp`, `TestReconcile_DiffReportsManualDrift` |
| 5 | `TestReconcile_TeamCannotOverrideLockedOrgRepository` |
| 6 | `TestReconcile_RejectsLifecycleOnProxy` |
| 7 | `TestRBAC_TeamPaymentsCanPullCannotPush` (`cmd/sindri`, stub OIDC provider) |
| 8 | `TestRBAC_AnonymousPullAndInstallRefused` |
| 9 | `TestRBAC_GroupChangeAtIdPGrantsAccessWithoutReapply` |

Tests 1, 8, and 9 were each checked to fail with their bug reintroduced:

- **Test 1:** a desugarer that drops `defaults.proxy.ttl`.
- **Test 8:** an OCI adapter that lets an anonymous GET through.
- **Test 9:** an OIDC verifier that caches groups by user.

**What M7 decided.** The configuration decisions are in
[ADR 17](docs/adr/0017-config-kinds-namespaces-and-reconciliation.md) and the identity
decisions in [ADR 18](docs/adr/0018-authentication-and-rbac-in-v1.md).

Configuration:
- A namespace prefixes the repository name.
- Long-form members are type-qualified (`hosted/internal-py`), and a bare ref never
  leaves its namespace.
- There are three kinds: `Repository`, `OrgPolicy`, and `Lifecycle`. `defaults:` is
  short-form sugar only.
- A config row records which trigger owns it, and neither trigger changes the other's.
  The gate therefore runs the long form against a second, empty database.
- Pruning needs `--prune`, and `serve --config` never deletes.

Identity:
- People use an identity-provider ID token as the Basic password.
- Groups are read from each request's token and stored nowhere.
- Machine tokens are scoped to a repository, a namespace, or the server.
- There is no anonymous access at all.
- A virtual repository answers only from members the caller can read.

Two things this milestone could not finish:
- The UI is guarded (`auth.GuardUI`, `server.Options.UI`) but not built; that is M8.
- Owners beyond `repository:<name>` (ADR 16) were not added, since authorization works
  through the owning repository.

---

### M8a — Background worker, garbage collection, retention, audit ✅

**Goal.** Sindri reclaims what nobody references and never what somebody does. M8 was split
in two because this half can lose data and deserves a gate of its own.

**Scope.**
- [x] A `jobs` queue table and a worker, with the API and the worker linked only through the
  database, so the post-v1 split modes are configuration rather than a rewrite
- [x] Write intent: every writer touches a `blobs` row (`last_seen_at`) before writing or
  relying on bytes, which closes the in-flight push race and ADR 13's orphaned bytes
- [x] Garbage collection: roots are tags, PyPI files, proxy cache hits, evidence, and subjects
  with lifecycle events or evidence; reachability over `manifest_children`; a grace period on
  `last_seen_at`; a per-candidate sweep that re-checks under a row lock and deletes bytes
  before it commits
- [x] `sindri gc --dry-run` and `sindri gc` against a running server (`/api/v1/gc`,
  `/api/v1/jobs/{id}`), admin only
- [x] Retention: stale proxy cache eviction (`ttl` plus `proxyStaleAfter`), upload session
  cleanup in storage (`BlobStore.PurgeUploads`) and in process memory
- [x] `GET /api/v1/audit` over `lifecycle_events`, filtered to readable repositories, and the
  same filter applied to `/subjects/{digest}/events`
- [x] `worker:` section in the one configuration loader

**Out of scope.** The UI and release work, which are M8b. A storage listing to find bytes
written by pre-M8 binaries without a row: a documented leak, never a safety problem.

**Watch out for.** Garbage collection deleting a blob that an in-flight push already
referenced is the classic registry data-loss bug. The grace period alone does not prevent
it; the write intent, the row lock, and the bytes-before-commit order do. Each has a test
that was checked to fail with its guard removed.

**Gate.** Verified on 2026-10-11 with Go 1.27.2 (lint under Go 1.27.1, the toolchain
`make tools` builds golangci-lint v2.13.2 with), Docker 29.8, against the Compose stack.
```bash
make generate && git diff --exit-code internal/db
make ci && make test-integration
make up && make build && ./scripts/m8-gc-gate.sh
# gc --dry-run lists an untagged, unpromoted image (manifest, config, layer);
# gc deletes it; a tagged and a promoted image both survive, pull by digest,
# and docker pull still works; a second dry run lists nothing
```

The tests that guard each part, and the bug each was shown to catch when it was put
back in (★):

| Concern | Test | ★ Reintroduced bug |
|---|---|---|
| In-flight push | `TestGC_InFlightPushSurvivesConcurrentCollection` | reserve after `CommitUpload`: row kept, bytes lost |
| Dedup write | `TestGC_PushThatDedupsAgainstACollectedBlobRewritesItsBytes` | no touch before the dedup `Stat`; bytes deleted after commit |
| Sweep re-check | `TestGC_SweepSkipsABlobReferencedAfterTheMarkSnapshot` | re-check removed |
| Grace re-check | `TestGC_SweepRechecksTheGracePeriod` | sweep ignores `last_seen_at` |
| Byte delete order | `TestGC_FailedByteDeleteRollsBackRows` | bytes deleted after the commit |
| Roots | `TestGC_PromotedUntaggedManifestSurvives`, `TestGC_EvidenceBlobAndItsSubjectSurvive` | ledger, evidence dropped from the roots |
| Closure | `TestGC_LayersOfATaggedIndexSurviveThroughEveryLevel`, `TestGCMark_MatchesReferenceReachabilityOnRandomGraphs` | non-recursive walk |
| Queue | `TestQueue_ClaimIsExclusiveUnderConcurrency`, `TestQueue_SingletonEnqueueReturnsTheActiveJob` | no `SKIP LOCKED`; dedupe key ignored |
| Worker | `TestWorker_LostLeaseCancelsTheHandler`, `TestServe_WorkerStopsBeforeTheDatabaseCloses` | lost lease ignored; pool closed before the worker |
| Retention | `TestEvictProxyCache_HostedRepositoriesAreNeverTouched` | proxy-type filter removed |
| Audit | `TestAudit_NonAdminSeesOnlyEventsOfReadableRepositories`, `TestEvents_HidesEventsInUnreadableRepositories` | repository filter removed, on `/audit`, `/events`, and subject states |
| OCI edge | `TestOCI_PushPullAndPlacementGate` (dangling layer) | `MANIFEST_BLOB_UNKNOWN` mapping removed: a 500 |
| Config | `TestLoad_ShortAndLongFormsAreIdentical` (now with `worker:`) | `worker: false` desugars differently |

The storage conformance suite gained four `PurgeUploads` cases, run on the filesystem,
S3 (MinIO), and memstore backends, including purging what a crashed process left behind.
Two mutations first survived and the tests were strengthened until they did not: a grace
mutation that broke the query rather than the behaviour, and a shutdown-order mutation the
first version of the worker test could not see.

**What M8a decided.** [ADR 19](docs/adr/0019-garbage-collection-roots-write-intent-and-sweep.md)
and [ADR 20](docs/adr/0020-queue-table-and-background-worker.md).

- Subjects are roots only with ledger history or evidence (refining ADR 16).
- Every proxy cache hit is a root. Retention, not the collector, decides when a cached
  copy goes: after `ttl` plus `proxyStaleAfter`, and never with `ttl: 0`.
- Scheduled collection is off by default.
- Audit is open to any caller, filtered per event. That filter now also applies to
  `/subjects/{digest}/events` and the subject view, which used to name repositories the
  caller could not read.
- Upload sessions in process memory expire on a ticker, the one background task outside
  the queue.

Two things this milestone left:

- Bytes written by a pre-M8 binary whose placement failed have no row and are never
  collected. That is a storage leak, and reclaiming it would need a storage listing.
- Running an M7 binary beside an M8 collector is unsafe; upgrade every server first.

---

### M8b — Read-only UI and release ✅

**Goal.** Sindri is something a stranger can run, see, and operate.

**Scope.**
- [x] Embedded read-only UI: repository list, package and version browse, artifact detail with
  digest, owner, provenance, evidence, and per-environment lifecycle status. Go templates
  plus htmx, no Node build. Every route authorizes; artifact pages check placement (I2)
- [x] `goreleaser` for multi-architecture binaries and a container image, a Compose file that
  runs Sindri itself with a development identity provider, quickstart documentation
- [x] `SECURITY.md`, `CONTRIBUTING.md`

**Out of scope.** Administration in the UI. Dashboards. Metrics UI.

**Gate.** Verified on 2026-10-12 with Go 1.27.2 (lint under 1.27.1), goreleaser v2.18.3,
Docker 29.8.
```bash
make ci && make test-integration
goreleaser release --snapshot --clean                  # 4 archives, linux/amd64 and arm64 images
docker run --rm ghcr.io/spjdevops/sindri:<snapshot>-arm64 version   # ldflags stamped, user 65532
./scripts/m8-quickstart-gate.sh        # docs/quickstart.md, verbatim, in a fresh docker:dind
```
The quickstart gate means what it says: run it in a clean VM or a fresh container, with no
Go toolchain and no warm caches. Quickstarts that only work on the author's machine are
the most common first impression a project gets wrong.

The gate starts a `docker:dind` daemon with no images and loads only the Sindri image built
from the checkout, since no release exists yet. It then:

- reads the Compose file and the commands out of `docs/quickstart.md`
- fails if that Compose file differs from `deploy/quickstart/compose.yaml`
- runs the commands as written
- signs in through Dex the way a browser does, and checks that the signed-in page lists
  the pushed image

The UI was also checked by eye: screenshots in light and dark mode, at desktop and phone
width, from the golden renders with the real stylesheet and fonts. That check found a
filler cell in the repository grid and a digest title set in the display font. Both were
fixed.

The tests that guard the UI, each shown to catch its bug when it was put back in (★):

| Concern | Test | ★ Reintroduced bug |
|---|---|---|
| List | `TestUI_RepositoryListShowsOnlyReadable` | unreadable repositories listed |
| Pages | `TestUI_UnreadableRepositoryPagesAre404` | repository authorization removed |
| I2 | `TestUI_ArtifactPageRequiresPlacementInTheRepository` | placement check removed |
| Other teams | `TestUI_ArtifactPageHidesOwnersAndStatesInUnreadableRepositories`, `TestUI_VirtualRepositoryListsOnlyReadableMembers` | foreign states and members shown |
| New routes | `TestUI_EveryRouteHasAnAuthorizationCase` | a route added without a case |
| CSP | `TestTemplates_NoInlineScriptOrStyleOrHandlers` | an inline `style=` |
| Vendored files | `TestVendoredAssets_MatchRecordedChecksums` | a checksum that does not match |

End to end, `TestServe_UIBrowserSignInShowsOnlyReadableRepos` signs in through the stub
identity provider and checks that the UI never changes anything, with a token or with the
session cookie. `TestServe_UIWithoutSignInExplains` covers a server without OIDC.

**What M8b decided.** [ADR 21](docs/adr/0021-read-only-ui-and-quickstart.md).

- The UI is rendered on the server from the API's services. Pages within a repository
  follow `/-/`, and `-` is now a reserved name segment.
- Evidence is shown as metadata only.
- Without OIDC the UI explains how to configure it, and never sends a Basic challenge.
- The quickstart runs Dex with a mock connector, sharing a network namespace with Sindri.
- Releases ship on distroless `nonroot`, with `sindri probe` as the healthcheck.

---

## v1 release checklist

Run on 2026-10-13 against `03fb446` (`make test`, `make test-integration`, `make lint`, each
gate script below, and `goreleaser release --snapshot --clean`).

- [x] `docker push`/`pull` round-trip passes; distribution-spec conformance green for
      push, pull, and content management (`scripts/oci-docker-gate.sh`;
      `scripts/oci-conformance.sh` 60/60, 20 skipped: referrers and related)
- [x] `twine upload` and `pip install` round-trip passes (`scripts/pypi-twine-gate.sh`)
- [x] `TestDependencyConfusion_HostedNameIsNeverServedFromProxy` passes, and its OCI
      twin `TestDependencyConfusion_OCIHostedImageIsNeverServedFromProxy`
- [x] Storage conformance suite green on filesystem **and** S3
      (`TestBlobStore_Conformance/Filesystem`, `/S3`)
- [x] Promotion asserted to copy zero bytes (`TestLifecycle_PromotionStory`)
- [x] Invariant I2 asserted: no blob reachable through a repository it is not placed in
      (`TestAPI_PlacementGatesCrossRepoReads`, `TestOCI_PushPullAndPlacementGate`,
      `TestPyPI_UploadInstallAndPlacementGate`,
      `TestUI_ArtifactPageRequiresPlacementInTheRepository`)
- [x] RBAC enforced on every route, with an anonymous-access test per adapter
      (`TestRBAC_AnonymousPullAndInstallRefused`, `TestUI_EveryRouteHasAnAuthorizationCase`)
- [x] Quickstart verified on a clean machine with only Docker
- [x] A single-team setup works from one short-form `sindri.yaml` with no apply step
      (`scripts/m7-config-gate.sh`, `TestServe_ReconcilesOnBootAndOnSIGHUP`)
- [x] Short form and long form asserted to produce identical database state
      (`TestReconcile_ShortAndLongFormsProduceIdenticalDatabaseState`)
- [x] Upgrade path documented: replace the binary, migrations self-apply
      ([docs/upgrading.md](docs/upgrading.md))
- [x] Every deferred item listed as deferred, so nothing is silently missing
      ([README, Not in v1](README.md#not-in-v1))
- [x] `SECURITY.md` published with a disclosure contact (private vulnerability
      reporting enabled)
- [x] Repository and the `ghcr.io/spjdevops/sindri` package made public, and private
      vulnerability reporting enabled, at the `v1.0.0` tag
- [x] `v1.0.0` tagged, and the release workflow green; `ghcr.io/spjdevops/sindri:1.0.0`
      pulls anonymously for linux/amd64 and linux/arm64

---

## After v1

Kept here so post-v1 work continues rather than restarts.

**Protocols.** npm first, as it exercises mutable metadata documents in a way OCI and PyPI
do not. Then Maven, then Helm, which is mostly OCI already.

**Scale.** `--api-only` and `--worker-only`, a Helm chart, horizontal pod autoscaling, and
queue-driven worker scaling. The M8 queue table is the prerequisite.

**Security depth.** Scanner integration producing evidence rows, real signature
verification, the advanced policy engine, and a Kubernetes admission webhook calling the
M6 decisions endpoint. That webhook is the clearest demonstration of the whole thesis, and
it is deliberately post-v1: it is worth building once the decision endpoint has been in
production use.

**Commercial layer.** SAML and LDAP, audit retention, multi-region replication, managed
hosting.

---

## Testing strategy

| Layer | What | When |
|---|---|---|
| Unit | Pure logic beside the code, no Docker | every `make test` |
| Integration | Real Postgres and MinIO via testcontainers, `integration` tag | `make test-integration`, CI |
| Conformance | Upstream distribution-spec suite | M3 onward, CI |
| Client round-trip | Real `docker`, `pip`, `twine` | each adapter's gate |
| Invariant guards | Named tests for I2, I3, I5 | permanent, never deleted |

The invariant guards deserve their own row because they are the tests most likely to be
removed by someone who does not know why they exist. Each carries a comment pointing at
its ARCHITECTURE.md invariant.
