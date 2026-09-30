# Working agreement for Sindri

Read this first. It records decisions that are **already made**, so that sessions spend
their effort on the problem rather than re-deciding the stack.

- **What we are building and why:** [pitch.md](pitch.md)
- **How it is designed:** [ARCHITECTURE.md](ARCHITECTURE.md) — the source of truth for
  schema, package boundaries, and the five invariants
- **What to build next:** [roadmap.md](roadmap.md) — work one milestone at a time
- **Why a decision was made:** [docs/adr/](docs/adr/)

## Starting a session

1. Read the milestone in `roadmap.md`. Build only that milestone.
2. Work on a branch named for it, for example `m3-oci-adapter`.
3. Read the invariants in `ARCHITECTURE.md` section 2 before touching a read path.
4. Finish with the milestone's gate commands actually run, not described.

## Locked technical choices

Do not substitute these without an ADR.

| Concern | Choice | Why |
|---|---|---|
| Language | Go, latest stable | Single binary, no runtime deps, Kubernetes-adjacent |
| Router | `chi` | Stdlib `ServeMux` cannot express OCI paths whose name segment contains slashes mid-path |
| Database | Postgres via `pgx/v5` | Only supported store; jsonb and views carry real weight here |
| Queries | `sqlc` | Typed, reviewable SQL; no ORM |
| Migrations | `goose`, embedded via `embed.FS` | Upgrade is replacing one binary |
| Object storage | `aws-sdk-go-v2` | S3-compatible covers MinIO, Ceph, and the clouds |
| CLI | `cobra` | Subcommand tree; config through `yaml.v3`, **not** viper |
| Logging | `log/slog` | Stdlib, structured, no dependency |
| Metrics | `prometheus/client_golang` | Expected by the target audience |
| OCI types | `opencontainers/image-spec` | Never hand-roll manifest JSON |
| Auth | `coreos/go-oidc` + `x/oauth2` | Correct token and JWKS handling |
| Tests | `testcontainers-go` | Real Postgres and MinIO, not fakes |
| UI | Go templates + htmx, embedded | A Node build would break the single-binary promise |

## Code conventions

**Dependency direction is inward, always.** `proto/*` and `api` depend on `registry`,
`lifecycle`, `auth`. Those depend on `cas`, `db`, `model`. `cas` depends on `storage`.
Nothing in `storage`, `cas`, or `model` may import an adapter. This one-way rule is what
makes a third protocol cheap, so a cycle is a design error, not a lint warning.

**Context first.** Every function doing I/O takes `ctx context.Context` first. No
`context.Background()` below `main` and tests.

**Errors wrap.** `fmt.Errorf("reading manifest %s: %w", digest, err)`. Compare with
`errors.Is` and `errors.As`, never `==`. Sentinel errors in the package that owns the
concept.

**No naked returns**, no `interface{}`/`any` where a type works, no panics outside
`main` and impossible-state assertions. One sanctioned exception: an adapter whose
streamed response fails after headers are sent calls `panic(http.ErrAbortHandler)`,
net/http's way to cut the connection so the client cannot mistake a bad body for a
complete one. The server's recoverer passes that value through (ADR 14).

**Digests are a type**, not a `string`. Parse and validate at the edge so that internal
code cannot receive a malformed digest.

**Table-driven tests** with named cases. Test names state the behaviour:
`TestResolve_HostedMemberWinsOverProxy`.

**Every backend passes the same suite.** When there are two implementations of an
interface, there is one test suite parameterised over them, not two suites that drift. The
storage suite lives in `internal/storage/storagetest` and is run per backend by
`internal/storage/conformance_integration_test.go`. Test fakes run it too, so a fake cannot
behave differently from the thing it fakes.

**Integration tests get their dependencies from `internal/testsupport`**, which is built
only under the `integration` tag and starts real Postgres and real MinIO through
testcontainers. Container images are pinned there; MinIO comes from
`bitnamilegacy/minio` because neither Docker Hub nor `quay.io` serves MinIO's own
images to anonymous pulls any more. See ADR 11 before changing that pin.

## Configuration rules

There is **one** configuration file and one loader, extended by section over time: M1 adds
server settings, M7 adds `repos:`, `lifecycle:`, and `defaults:`. Never a second
configuration system, and never a separate simplified file for small deployments.

The short form desugars into the long form **one-way and totally**. Loading produces long
form; nothing downstream may branch on which spelling was written. A golden test asserts
both spellings of one setup produce identical internal trees, and that test is the only
thing keeping two spellings from becoming two parsers.

Sugar may make hosted, proxy, and virtual repositories cheap to declare. It may never make
them invisible: that separation is the dependency-confusion defence. See
ARCHITECTURE.md section 8 and ADR 7.

## Security rules

This is a registry. Supply-chain security is the product, so these are not style points.

1. **Never serve bytes by digest alone.** Check placement first, every time. See invariant
   I2. This is the easiest severe bug to introduce here, because deduplication makes every
   tenant's blobs physically present for every other tenant.
2. **Never trust a client-supplied digest.** Hash the bytes, compare, and only then commit
   to the content-addressed key.
3. **Never let a proxy shadow a hosted name.** See the name-claim rule. The regression test
   guarding it does not get deleted or relaxed.
4. **Never log credentials, tokens, or full request bodies.** Log a request id.
5. **Validate at write time**, not on the download path.
6. `gosec` findings are bugs. Fix them or annotate with a written justification.

## Operational rules

- Background work goes through a **queue table**, even while the worker runs in the same
  process. That is what keeps the post-v1 split serve modes a config change.
- Protocol adapters translate errors to their own specified shapes at the edge. An OCI
  client needs the OCI error body; a generic JSON error breaks real clients.
- Migrations apply on startup and must be backward compatible for one version, so a
  rollback does not need a database restore.

## Commands

```bash
make help              # list targets
make build             # ./bin/sindri
make test              # unit tests, race detector
make test-integration  # needs Docker
make lint              # gofmt check, go vet, golangci-lint
make up / make down    # local Postgres and MinIO
make ci                # what CI runs
make tools             # install golangci-lint v2
```

## Local development ports

Compose binds high host ports so it never fights whatever else you are running.

| Service | Host port | Override |
|---|---|---|
| Postgres | 55432 | `SINDRI_PG_PORT` |
| MinIO S3 API | 59000 | `SINDRI_S3_PORT` |
| MinIO console | 59001 | `SINDRI_S3_CONSOLE_PORT` |

Credentials are `sindri` / `sindri` for Postgres and `sindri` /
`sindri-dev-secret` for MinIO, which creates the `sindri-blobs` bucket on startup. These
are development-only values and must never appear in a deployment manifest.

`make lint` runs with the `integration` build tag set in `.golangci.yml`, so the suites
that touch real infrastructure are linted with everything else.

**golangci-lint must be v2.** The v2 configuration format is incompatible with v1, so a v1
binary on your `PATH` will reject `.golangci.yml`. Install v2 into its own `GOBIN` if you
need v1 for another project.

## Writing an ADR

Any decision a future session might reopen gets one. Copy the numbering, keep it to a
page, and state what was rejected and why. Rejected alternatives are the part that saves
the next session's time.
