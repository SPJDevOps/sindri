# Sindri

A lightweight, cloud-native, multi-protocol artifact repository.

One Go binary. Starts with a single Docker Compose file, grows to a Kubernetes
installation without switching products. Artifact **identity** is the core idea, not file
storage: an artifact is not copied between repositories as it matures, its trust status
changes.

Sindri answers the question CI/CD and Kubernetes actually ask. Not "does this image
exist?" but **"is exactly this artifact approved for this environment?"**

## Status

Pre-v1, under active construction. See [roadmap.md](roadmap.md) for the milestone plan
and [ARCHITECTURE.md](ARCHITECTURE.md) for the design.

Through milestone M8, `sindri serve` boots from one YAML file (or none), applies its
embedded migrations to Postgres, stores digest-verified blobs on a filesystem or any
S3-compatible bucket, and serves two protocols:

- **`/v2`** — the OCI distribution API. `docker push` and `docker pull` work, and the
  upstream distribution-spec push, pull, and content-management workflows pass.
- **`/pypi`** — PyPI. `twine upload` and `pip install` work. See
  [using Sindri as a pip index](#using-sindri-as-a-pip-index).

Repositories are hosted, proxy (pypi.org, Docker Hub, ghcr.io), or virtual. Artifacts
carry lifecycle state and evidence, and `/api/v1/decisions` answers whether a digest is
approved for an environment. Repositories, stages, and access are declared in the
configuration file and reconciled on startup. Every request authenticates, with an
OpenID Connect token, a machine token for CI, or the bootstrap token. There is no
anonymous access.

A background worker evicts stale proxy cache entries and abandoned uploads. `sindri gc`
reclaims blobs nothing references, safely beside pushes in flight, and `/api/v1/audit`
reads the promotion ledger.

A read-only web UI at `/ui/` shows repositories, packages, tags, and each artifact's
digest, owners, provenance, evidence, and lifecycle state per environment, limited to
what the signed-in person may read. Releases ship as binaries for Linux and macOS and as
a multi-architecture image.

## What v1 will do

- **Protocols:** OCI (Docker, Helm charts) and PyPI
- **Repository types:** hosted, proxy, and virtual, cleanly separated
- **Lifecycle:** stages and promotion that change trust status instead of copying bytes
- **Evidence:** SBOMs and signatures attached to an artifact identity
- **Config as code:** one short YAML file for a single team, reconciled on startup with
  no separate apply step; enveloped manifests and org policy when you have many teams
- **Auth:** OpenID Connect login with role-based access control
- **Storage:** filesystem or any S3-compatible object store, a config choice only

## Quickstart

With nothing but Docker, [docs/quickstart.md](docs/quickstart.md) runs Sindri, Postgres,
and a development identity provider from one Compose file:

```bash
curl -fsSLO https://raw.githubusercontent.com/SPJDevOps/sindri/main/deploy/quickstart/compose.yaml
export SINDRI_TOKEN=$(openssl rand -hex 16)
docker compose up -d --wait
echo "$SINDRI_TOKEN" | docker login localhost:8080 -u admin --password-stdin
docker pull alpine:3.20 && docker tag alpine:3.20 localhost:8080/demo/app:1.0
docker push localhost:8080/demo/app:1.0
open http://localhost:8080/ui/
```

The quickstart is verified on every milestone in a fresh Docker daemon by
`scripts/m8-quickstart-gate.sh`, which runs the document's own commands.

To run from source instead, with Go and Docker installed:

```bash
make up    # Postgres on 55432, MinIO on 59000, Dex on 5556
make run   # builds, then serves deploy/compose/sindri.yaml on :8080
open http://localhost:8080/ui/
```

The browser signs in through the development Dex as an example administrator, the same
as in the quickstart. The server listens on loopback only, because its bootstrap token
`sindri-dev-token` is public, and it warns while that default is in use. `docker login`
takes the token as its password, at `localhost:8080` on Linux; Docker Desktop's daemon runs
in a VM, so there use `host.docker.internal:8080` and list it under `insecure-registries`.
`./bin/sindri serve` with no configuration file also works, but without `auth.oidc` the UI
can only explain how to enable sign-in. Set `auth: ${SINDRI_TOKEN}` and point `auth.oidc` at
your own identity provider for anything but local development.

## Configuration as code

One file declares a whole single-team setup, and `serve` reconciles it on startup and
on `SIGHUP`, with no apply step. See [`testdata/config/sindri.yaml`](testdata/config/sindri.yaml):

```yaml
repos:
  internal-py: { protocol: pypi, read: [team:dev], write: [team:dev] }   # hosted
  pypi:        { upstream: pypi, read: [authenticated] }                 # proxy, a preset
  python:      { members: [internal-py, pypi], read: [team:dev] }        # virtual

lifecycle:
  stages: [dev, prod]
  gates:
    prod: [sbom, signature]
```

```bash
./bin/sindri config check -f sindri.yaml              # static: no database, no server
./bin/sindri config show --expanded -f sindri.yaml    # the long form the server acts on
./bin/sindri serve --config sindri.yaml
```

Many teams write enveloped manifests instead (`kind: Repository`, `OrgPolicy`,
`Lifecycle`) and converge them from CI with `sindri apply -f manifests/`. `sindri diff`
reports drift and exits 1 when there is any. See ARCHITECTURE.md section 8 and
[ADR 17](docs/adr/0017-config-kinds-namespaces-and-reconciliation.md).

Garbage collection: `sindri gc --dry-run` lists what nothing references any more, such as
an image whose tag moved and that was never promoted, and `sindri gc` deletes it. It talks
to a running server with an administrator's `$SINDRI_TOKEN`. Set `worker.gc.interval` to
run it on a schedule. See [ADR 19](docs/adr/0019-garbage-collection-roots-write-intent-and-sweep.md).

Identity: configure `auth.oidc` and `auth.admins`, and people use their identity
provider's ID token as the password for docker, pip, and twine. CI uses machine tokens
from `POST /api/v1/tokens`. See [ADR 18](docs/adr/0018-authentication-and-rbac-in-v1.md).

## Using Sindri as a pip index

Sindri is usable as a private Python index today, for both publishing and installing.
Starting from the Quickstart above, declare a repository once, in the configuration
file or through the API. PyPI repositories are never auto-created, so a typo in a
`--repository-url` fails loudly instead of silently creating a second index:

```bash
curl -fsS -X POST localhost:8080/api/v1/repositories \
  -H 'Authorization: Bearer sindri-dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"name":"internal","type":"hosted","protocol":"pypi"}'
```

Publish with twine, and install with pip:

```bash
python -m twine upload \
  --repository-url http://127.0.0.1:8080/pypi/internal/upload \
  -u token -p "$SINDRI_TOKEN" dist/*

pip install --index-url "http://token:$SINDRI_TOKEN@127.0.0.1:8080/pypi/internal/simple/" \
  --trusted-host 127.0.0.1:8080 your-package
```

Or run [`scripts/pypi-twine-gate.sh`](scripts/pypi-twine-gate.sh), which does all of the
above against a checked-in sample package.

To avoid repeating the URLs, `~/.pypirc`:

```ini
[distutils]
index-servers = sindri

[sindri]
repository = http://127.0.0.1:8080/pypi/internal/upload
username = token
password = <your token>
```

Worth knowing before you point real work at it:

- **Every request authenticates**, the index and downloads included. The username is
  ignored; the password is the token. Uploading needs the write role on the repository.
- **Use one virtual index, not `--extra-index-url`.** With two indexes, pip chooses by
  version and not by trust. A virtual repository listing your hosted repository before a
  pypi.org proxy never serves a name you publish from the proxy, whatever version the
  proxy offers. That is the dependency-confusion defence of invariant I5.
- **A published file is immutable.** Re-uploading identical bytes is a no-op; different
  bytes under a filename already published is a 409, because consumers pin the hash the
  index advertised. Bump the version to republish. See
  [ADR 13](docs/adr/0013-pypi-write-time-validation-and-immutable-files.md).
- **Wheels and sdists only** (`.whl`, `.tar.gz`, `.tgz`), and an upload must be
  self-consistent: the filename, the packaged metadata, and twine's form fields have to
  agree on project and version. Build tools satisfy that; a hand-rolled `curl -F` may not.
- **No yanking.** Removing a file is a `DELETE` on its management API path,
  `/api/v1/repositories/{repo}/packages/{name}/versions/{version}/files/{filename}`.

## Why

The original pitch is in [pitch.md](pitch.md). In short: Nexus and Artifactory have grown
heavy, and Harbor is strong on OCI but was not designed to be multi-protocol. Small and
mid-sized teams end up stitching together Harbor, Verdaccio, and a homegrown PyPI server
because nothing sits in the middle. That gap is the target.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as
[SECURITY.md](SECURITY.md) describes, never in a public issue.

## License

Apache License 2.0. See [LICENSE](LICENSE).
