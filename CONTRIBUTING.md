# Contributing to Sindri

Thank you for looking. Sindri is small on purpose, and these notes keep it
that way.

## Before you write code

- **Read [CLAUDE.md](CLAUDE.md).** It is the working agreement for every
  contributor, human or not: the locked technical choices, the code
  conventions, and the security rules.
- **Read ARCHITECTURE.md section 2** before you touch a read path. The five
  invariants are why the code looks the way it does.
- **Read [roadmap.md](roadmap.md)** to see where your change fits.
- **Write an ADR first** when a change reverses a decision in `docs/adr/`,
  adds a dependency, or would make a future contributor ask "why?". Copy the
  format and keep it to a page. The rejected alternatives are the part that
  saves the next person's time.

## Setting up

You need Go (the version in `go.mod`), Docker, and `make`.

```bash
make tools              # golangci-lint v2, sqlc, goreleaser, pinned versions
make up                 # Postgres and MinIO on high local ports
make build              # ./bin/sindri
make help               # everything else
```

golangci-lint v2.13.2 cannot read the standard library of Go 1.27.2 and later.
CI pins Go 1.27.1. If your Go is newer, run lint as
`GOTOOLCHAIN=go1.27.1 make lint`.

## Tests

```bash
make test               # unit tests, race detector, no Docker
make test-integration   # real Postgres and MinIO through testcontainers
make lint               # gofmt, go vet, golangci-lint (integration tag included)
make ci                 # what CI runs
```

- **Test first.** A behaviour change starts with a failing test.
- **Names state the behaviour:** `TestResolve_HostedMemberWinsOverProxy`, not
  `TestResolve2`.
- **Tests are table-driven**, with named cases.
- **Integration tests use real dependencies.** Get Postgres from
  `internal/testsupport`; `MigratedDB(t)` gives each test its own database on
  one shared container. No database fakes.
- **One suite per interface.** Every implementation of an interface passes the
  same suite. The storage suite in `internal/storage/storagetest` runs against
  the filesystem, S3, and memstore backends.
- **Safety tests get a mutation check.** A test that guards an invariant, an
  authorization rule, or garbage collection safety is shown to fail with its
  guard removed before it counts as a guard. Record that in the PR.
- **Never delete or relax an invariant guard test** (named for I2, I3, or I5)
  without an ADR.

Each milestone ends with gate commands: `scripts/m8-gc-gate.sh` against the
Compose stack, and `scripts/m8-quickstart-gate.sh` in a clean Docker daemon.
Run the ones your change touches.

## Commits and pull requests

- **One logical change per commit**, as conventional commits:
  `type(scope): what changed (Mn)`. For example,
  `feat(gc): mark and sweep (M8a)` or `fix(oci): map unknown blobs to
  MANIFEST_BLOB_UNKNOWN`.
- **The PR description** says what changed, why, and which gate commands you
  ran, with their results.
- **`make ci` is green** before you ask for review.
- **Security issues go through [SECURITY.md](SECURITY.md)**, never a public
  issue or PR.

## Generated code

Database queries live in `internal/db/queries/*.sql`. `make generate` turns
them into Go with sqlc. Commit the generated files; CI fails when they are
stale. Migrations go in `internal/db/migrations`. They must stay backward
compatible for one release, so a rollback never needs a database restore.

## Licence

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
