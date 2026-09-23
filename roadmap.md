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
now come from `quay.io` and are pinned, since Docker Hub no longer serves them to
anonymous pulls, and `make lint` now covers the `integration` build tag.

The `integration` job in `.github/workflows/ci.yml` is enabled from here.

---

### M2 — Domain spine and management API

**Goal.** Repositories, packages, versions, and files are real rows, manageable over HTTP.

**Scope.**
- Migrations for the placement layer: `repositories`, `repository_members`, `remotes`,
  `packages`, `versions`, `files`, `blobs`, `tags`, `manifest_children`
- `sqlc` wired into `make generate`, typed queries in `internal/db`
- `internal/registry`: publish, resolve, list, delete, enforcing invariant I1
- `/api/v1` with repository CRUD, package and version listing, cursor pagination
- One error model: stable machine-readable code, human message, request id
- Static bearer-token auth as an explicit placeholder for M7, and a middleware seam that
  M7 fills rather than replaces
- Placement check helper that every read path must call, per invariant I2

**Out of scope.** Protocols, proxying, lifecycle.

**Watch out for.** Introduce the placement check now, as the only way to reach a blob, even
though with one repository type it looks like pointless indirection. Adding it after two
adapters exist means auditing every handler instead of writing one function.

**Gate.**
```bash
make test-integration
```
An end-to-end test that, over HTTP only: creates a hosted repository, uploads a blob,
lists it, reads it back byte-identical, fails to read it via a *different* repository's
path with 404, and deletes it. That fourth assertion is invariant I2 and it is the
important one.

---

### M3 — OCI adapter, hosted

**Goal.** `docker push` and `docker pull` work against Sindri.

**Scope.**
- `GET /v2/` version check
- Blob upload: monolithic `POST`, chunked `PATCH`/`PUT` with correct `Range` and
  `Location` handling, `DELETE` of an upload session
- Blob `GET`/`HEAD` with range support
- Manifest `GET`/`HEAD`/`PUT`/`DELETE`, including image indexes and multi-arch
- Tag listing with pagination and `Link` headers
- `manifest_children` populated on every manifest push, covering index-to-manifest and
  manifest-to-layer edges
- OCI-shaped error bodies with the specification's error codes
- Struct types from `opencontainers/image-spec`; no hand-rolled manifest JSON

**Out of scope.** Proxying Docker Hub, referrers API, cross-repository blob mounts.

**Watch out for.** Chunked upload is where naive implementations fail conformance: offsets
must be tracked server-side, and the `Range` response header is inclusive of both bounds,
which is off by one from every Go slice you will write next to it.

**Gate.**
```bash
docker build -t localhost:8080/demo/app:1.0 testdata/demo
docker push  localhost:8080/demo/app:1.0
docker rmi   localhost:8080/demo/app:1.0
docker pull  localhost:8080/demo/app:1.0          # round-trips

# Upstream conformance, push + pull + content management workflows
docker run --rm -e OCI_ROOT_URL=http://host.docker.internal:8080 \
  -e OCI_NAMESPACE=conformance/test \
  -e OCI_TEST_PUSH=1 -e OCI_TEST_PULL=1 -e OCI_TEST_CONTENT_MANAGEMENT=1 \
  ghcr.io/opencontainers/distribution-spec/conformance:latest
```
Check in the conformance invocation as a script so the next session does not reconstruct
it.

---

### M4 — PyPI adapter, hosted

**Goal.** `twine upload` and `pip install` work against Sindri.

**Scope.**
- PEP 503 HTML simple index, both the root listing and per-project pages
- PEP 691 JSON index with correct content negotiation
- PEP 503 name normalization, enforced as the uniqueness key
- Upload via twine's multipart `POST`, including the `:action` field and hash fields
- File download with correct content type and `Content-Length`
- Metadata extraction from wheel `METADATA` and sdist `PKG-INFO`
- `data-requires-python` and hash fragments on index links, since pip relies on both

**Out of scope.** Proxying pypi.org, which is M5. Yanking. Per-file GPG signatures.

**Watch out for.** Normalize on write and on read, and store the normalized form as the
key. Accepting `Foo.Bar` and `foo_bar` as two projects is the bug that makes the
name-claim rule in M5 unenforceable, and by then it is a data migration.

**Gate.**
```bash
python -m twine upload --repository-url http://localhost:8080/pypi/internal/upload \
  -u token -p "$SINDRI_TOKEN" dist/*

python -m venv /tmp/v && /tmp/v/bin/pip install \
  --index-url http://localhost:8080/pypi/internal/simple/ demo-package
/tmp/v/bin/python -c 'import demo_package; print(demo_package.__version__)'

curl -fsS -H 'Accept: application/vnd.pypi.simple.v1+json' \
  http://localhost:8080/pypi/internal/simple/demo-package/ | jq .
```

---

### M5 — Proxy and virtual repositories

**Goal.** The three repository types work together, and dependency confusion is
structurally impossible.

**Scope.**
- `remotes` configuration: upstream URL, credentials reference, positive and negative TTL
- On-miss upstream fetch that streams to the client while caching into the CAS, so a
  large first pull is not serialised behind the write
- Negative caching, upstream timeouts, and graceful behaviour when upstream is down but
  content is cached
- `internal/resolve`: ordered member walk, first hit wins, invariant I5
- The **name-claim rule** from ARCHITECTURE.md section 4
- Reconciler-time rejection of lifecycle configuration on a proxy repository, invariant I4
- Proxy support for both adapters: pypi.org and Docker Hub

**Out of scope.** Eager background mirroring. Cache eviction policy beyond TTL.

**Watch out for.** Upstream failure must not poison the cache, and a 404 must be cached
separately from a 503. Treating them alike either hammers a healthy upstream or hides a
package that appeared five minutes ago.

**Gate.**
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
while "simplifying tests".

---

### M6 — Identity, lifecycle, and evidence

**Goal.** The differentiating feature. Artifacts carry trust status, and promotion moves
no bytes.

**Scope.**
- Migrations for the identity layer: `subjects`, `subject_owners`, `provenance`,
  `evidence`, `lifecycle_stages`, `promotion_gates`, `lifecycle_events`
- `subjects` rows created on publish, with ownership and provenance capture from
  push-time metadata and headers
- Stage definitions, ordered, organisation-configurable
- `POST /api/v1/subjects/{digest}/promote` writing one append-only event
- Gate evaluation before promotion: for example, no promotion to `prod` without an
  attached SBOM and signature
- Evidence attach and list; evidence bodies stored as blobs in the CAS like anything else
- Revocation as an event, never a delete
- `GET /api/v1/decisions?digest=&environment=` returning an approval answer from the
  current-state view, on one indexed query
- Invariant I4 enforced on the promotion path as well as at reconcile time

**Out of scope.** Running scanners. Cryptographic signature verification, which is
post-v1; v1 stores and gates on evidence presence and type.

**Watch out for.** Resist adding an `UPDATE` to `lifecycle_events` when a "current status"
query feels slow. The view plus an index is the answer. A mutable status column destroys
the audit trail that makes this feature worth building.

**Gate.**
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

---

### M7 — Config as code, reconciler, OIDC and RBAC

**Goal.** Governance as code, in a form a one-person team will actually write. Real
authentication. Guardrails teams cannot route around.

Read ARCHITECTURE.md section 8 in full before starting. This milestone is where the
project either stays light for small teams or acquires the ceremony it was built to avoid.

**Scope, configuration.**
- Extend `sindri.yaml` from M1 with `repos:`, `lifecycle:`, and `defaults:` sections
- **Short form** with type inference: `members` means virtual, `upstream` means proxy,
  neither means hosted, both is a named error
- Virtual protocol inferred from members, with all members required to share one protocol
- Named upstream presets: `pypi`, `dockerhub`, `ghcr`, `npm`, `maven-central`, pinned in
  the binary, each overridable by an explicit object
- **Long form** `Repository` and `OrgPolicy` kinds, multi-document manifest directories
- **One-way, total desugaring** into the long form; nothing downstream may branch on which
  form was written
- `defaults:` keyed by repository type. No `extends:`, per ADR 7
- Bare member names resolve locally; a prefix is required to cross a namespace boundary
- `sindri config check`: static validation, no database, no server, errors naming file,
  line, and field
- `sindri config show --expanded`: prints the desugared long form

**Scope, reconciliation.**
- One reconciler, two triggers: `serve --config` converges on boot and on `SIGHUP`, and
  `sindri apply -f <dir>` converges from CI against a running server
- `sindri diff -f <dir>` reports drift without changing anything
- Config-managed and API-managed repositories distinguished so the two never fight over a
  row
- `OrgPolicy` modes `centralized`, `delegated`, `hybrid`; an absent `OrgPolicy` means
  `delegated` with no mandatory repositories
- `locked: true` repositories: referenceable, never overridable
- Invariant I4 enforced here: lifecycle configuration on a proxy repository is rejected at
  reconcile time

**Scope, identity.**
- OIDC authorization code flow, token validation, JWKS caching with refresh
- Group-to-role mapping; group membership resolved live at request time, never stored
- RBAC enforced on every route: both adapters, the management API, and the UI
- Machine tokens for CI, scoped to a repository and a role

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

**Gate.**
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

---

### M8 — Read-only UI, background worker, release

**Goal.** Sindri is something a stranger can run, see, and operate.

**Scope.**
- Embedded read-only UI: repository list, package and version browse, artifact detail with
  digest, owner, provenance, evidence, and per-environment lifecycle status. Go templates
  plus htmx, no Node build
- Worker on a queue table, with the API and worker kept separable so the post-v1 split
  modes are configuration rather than a rewrite
- Garbage collection: reachability from tags and lifecycle states over
  `manifest_children`, with a dry-run mode and a grace period for in-flight uploads
- Retention jobs: proxy cache TTL eviction, upload session cleanup
- `GET /api/v1/audit` over `lifecycle_events`
- `goreleaser` for multi-architecture binaries and a container image, `docker-compose.yml`
  that runs Sindri itself, quickstart documentation
- `SECURITY.md`, `CONTRIBUTING.md`

**Out of scope.** Administration in the UI. Dashboards. Metrics UI.

**Watch out for.** Garbage collection deleting a blob that an in-flight push already
referenced is the classic registry data-loss bug. Use a grace period on blob age and take
the reachability snapshot inside one transaction.

**Gate.**
```bash
# Quickstart, exactly as documented, on a machine with only Docker
docker compose up -d
docker push localhost:8080/demo/app:1.0
open http://localhost:8080/ui/

# Garbage collection safety
./bin/sindri gc --dry-run            # lists an untagged, unpromoted blob
./bin/sindri gc                      # deletes it
# a promoted blob and a tagged blob both survive; asserted in an integration test

goreleaser release --snapshot --clean  # binaries and image build
```
The quickstart gate means what it says: run it in a clean VM or a fresh container, with no
Go toolchain and no warm caches. Quickstarts that only work on the author's machine are
the most common first impression a project gets wrong.

---

## v1 release checklist

- [ ] `docker push`/`pull` round-trip passes; distribution-spec conformance green for
      push, pull, and content management
- [ ] `twine upload` and `pip install` round-trip passes
- [ ] `TestDependencyConfusion_HostedNameIsNeverServedFromProxy` passes
- [ ] Storage conformance suite green on filesystem **and** S3
- [ ] Promotion asserted to copy zero bytes
- [ ] Invariant I2 asserted: no blob reachable through a repository it is not placed in
- [ ] RBAC enforced on every route, with an anonymous-access test per adapter
- [ ] Quickstart verified on a clean machine with only Docker
- [ ] A single-team setup works from one short-form `sindri.yaml` with no apply step
- [ ] Short form and long form asserted to produce identical database state
- [ ] Upgrade path documented: replace the binary, migrations self-apply
- [ ] Every deferred item listed as deferred, so nothing is silently missing
- [ ] `SECURITY.md` published with a disclosure contact

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
