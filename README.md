# Sindri

A lightweight, cloud-native, multi-protocol artifact repository.

One Go binary. Starts with a single Docker Compose file, grows to a Kubernetes
installation without switching products. Artifact **identity** is the core idea, not file
storage: an artifact is not copied between repositories as it matures, its trust status
changes.

Sindri answers the question CI/CD and Kubernetes actually ask. Not "does this image
exist?" but **"is exactly this artifact approved for this environment?"**

## Status

v1. See [roadmap.md](roadmap.md) for how it was built and what comes next, and
[ARCHITECTURE.md](ARCHITECTURE.md) for the design.

`sindri serve` boots from one YAML file (or none), applies its
embedded migrations to Postgres, stores digest-verified blobs on a filesystem or any
S3-compatible bucket, and serves two protocols:

- **`/v2`** — the OCI distribution API. `docker push` and `docker pull` work, and the
  upstream distribution-spec push, pull, and content-management workflows pass.
- **`/pypi`** — PyPI. `twine upload` and `pip install` work. See
  [using Sindri as a pip index](#using-sindri-as-a-pip-index).
- **`/npm`** — npm. `npm publish` and `npm install` work, and so do pnpm and yarn. See
  [using Sindri as an npm registry](#using-sindri-as-an-npm-registry).

Repositories are hosted, proxy (pypi.org, npmjs.org, Docker Hub, ghcr.io), or virtual. Artifacts
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

## What v1 does

- **Protocols:** OCI (Docker, Helm charts), PyPI, and npm
- **Repository types:** hosted, proxy, and virtual, cleanly separated
- **Lifecycle:** stages and promotion that change trust status instead of copying bytes
- **Evidence:** SBOMs and signatures attached to an artifact identity
- **Config as code:** one short YAML file for a single team, reconciled on startup with
  no separate apply step; enveloped manifests and org policy when you have many teams
- **Auth:** OpenID Connect login with role-based access control
- **Storage:** filesystem or any S3-compatible object store, a config choice only

## Not in v1

Deliberately deferred, so nothing is silently missing. The reasons are in
[roadmap.md](roadmap.md#scope-of-v1).

- **Protocols:** Maven, and Helm's own repository format (Helm charts as OCI
  artifacts work today)
- **OCI:** the referrers API and cross-repository blob mounts
- **PyPI:** yanking; removing a file is a management API call
- **Storage:** Google Cloud Storage and Azure Blob Storage
- **Deployment:** separate `--api-only` and `--worker-only` modes, and a Helm chart
- **Lifecycle:** time-based auto-promotion
- **Evidence:** running scanners and verifying signatures; v1 attaches, lists, and
  gates on evidence produced elsewhere
- **Config:** a Git-polling operator and webhooks
- **Auth:** SAML, LDAP, and fine-grained per-path ACLs
- **UI:** administration in the browser; the UI is read-only
- **npm:** upstreams that need a token (SIN-53), and real publish times in packuments
  (SIN-60)

## Quickstart

With nothing but Docker, [docs/quickstart.md](docs/quickstart.md) runs Sindri, Postgres,
and a development identity provider from one Compose file. The commands below need only
Docker; the quickstart document also publishes an npm package, a step that needs Node.js
and is skipped without it:

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
`scripts/m8-quickstart-gate.sh`, which runs the document's own commands. Upgrading
later is replacing the image; see [docs/upgrading.md](docs/upgrading.md).

To run from source instead, with Go and Docker installed:

```bash
make up    # Postgres on 55432, MinIO on 59000, Dex on 5556
make run   # builds, then serves deploy/compose/sindri.yaml on 127.0.0.1:8080
```

In another terminal, push an image and open the UI. The registry address depends on
where the Docker daemon runs:

```bash
# Docker Desktop (macOS, Windows): the daemon runs in a VM, and its localhost is not
# your machine. List host.docker.internal:8080 under insecure-registries in Docker
# Desktop's Docker Engine settings once.
REGISTRY=host.docker.internal:8080
# Linux, with the daemon on this machine, use this line instead:
# REGISTRY=localhost:8080

echo sindri-dev-token | docker login "$REGISTRY" -u dev --password-stdin
docker pull alpine:3.20 && docker tag alpine:3.20 "$REGISTRY/demo/app:1.0"
docker push "$REGISTRY/demo/app:1.0"
open http://localhost:8080/ui/
```

The browser signs in through the development Dex as an example administrator, the same
as in the quickstart. The server listens on loopback only, because its bootstrap token
`sindri-dev-token` is public, and it warns while that default is in use; at startup it
also logs the address a Docker Desktop daemon must push to. `./bin/sindri serve` with no
configuration file also works, but without `auth.oidc` the UI can only explain how to
enable sign-in. Set `auth: ${SINDRI_TOKEN}` and point `auth.oidc` at your own identity
provider for anything but local development.

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

Every key the file accepts is in the [configuration reference](docs/configuration.md).

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
- **A published file is immutable.** Re-uploading identical bytes is a no-op while the
  file exists; different bytes under a filename already published is a 409, because
  consumers pin the hash the index advertised. A deleted file's name stays taken too: it
  is refused even with identical bytes. Bump the version to republish. See
  [ADR 13](docs/adr/0013-pypi-write-time-validation-and-immutable-files.md).
- **Wheels and sdists only** (`.whl`, `.tar.gz`, `.tgz`), and an upload must be
  self-consistent: the filename, the packaged metadata, and twine's form fields have to
  agree on project and version. Build tools satisfy that; a hand-rolled `curl -F` may not.
- **No yanking.** Removing a file is a `DELETE` on its management API path,
  `/api/v1/repositories/{repo}/packages/{name}/versions/{version}/files/{filename}`.

## Using Sindri as an npm registry

Sindri serves npm at `/npm/{repo}/`, for publishing and installing, with npm, pnpm, and
yarn. Declare the repositories in the configuration file. npm packuments carry absolute
tarball URLs, so a server with an npm repository must be told its own address with
`publicUrl`, and refuses to load without it:

```yaml
publicUrl: http://localhost:8080   # the origin your clients use

repos:
  internal-js: { protocol: npm }                       # hosted
  npm:         { upstream: npm }                       # proxy of registry.npmjs.org
  javascript:  { members: [internal-js, npm] }         # virtual
```

Point a project at a repository with a `.npmrc`, and keep the token out of the project:
put it in your user-level `~/.npmrc`, under the repository's own path. A host-level key
makes `npm publish` fail with `ENEEDAUTH`, and pnpm refuses to expand `${SINDRI_TOKEN}`
in a credential from a project `.npmrc`. To publish to `internal-js`, the project's
`.npmrc` names the registry:

```ini
registry=http://localhost:8080/npm/internal-js/
```

and `~/.npmrc` carries the token:

```ini
//localhost:8080/npm/internal-js/:_authToken=${SINDRI_TOKEN}
```

```bash
npm publish --access public
```

To install through the virtual repository, name it instead, and add its token line:

```ini
registry=http://localhost:8080/npm/javascript/
//localhost:8080/npm/javascript/:_authToken=${SINDRI_TOKEN}
```

```bash
npm install
```

`SINDRI_TOKEN` is the bootstrap token, or any token with the right role; the real
deployment uses an identity provider's ID token or a machine token, the same as for pip.
pnpm reads the same `.npmrc`. `pnpm import` turns npm's `package-lock.json` into a
lockfile, and `pnpm install --frozen-lockfile` installs from it. Yarn 1 reads it too:

```bash
pnpm import
pnpm install --frozen-lockfile
yarn import
yarn install --frozen-lockfile
```

Yarn 4 reads `.yarnrc.yml` instead, and allows plain `http` only for hosts you list:

```yaml
npmRegistryServer: "http://localhost:8080/npm/javascript/"
unsafeHttpWhitelist:
  - localhost
npmAlwaysAuth: true
npmAuthToken: "${SINDRI_TOKEN}"
npmMinimalAgeGate: 0
```

**`npmMinimalAgeGate: 0` turns off yarn 4's age safeguard, and yarn 4 needs it for now.**
Yarn 4.18 refuses to install a package younger than a day by default, and decides the age
from the `time` map in the packument. Sindri does not serve that map yet, so yarn 4
refuses every package Sindri serves with a quarantine error (`YN0016`). Setting the gate
to `0` installs them, and also means yarn will no longer hold back a freshly published
package from you. Set it only in the projects that install from Sindri. Real publish
times are tracked as SIN-60.

Or run [`scripts/npm-gate.sh`](scripts/npm-gate.sh), which does all of the above against
the Compose stack.

Worth knowing before you point real work at it:

- **Every request authenticates**, packuments and tarballs included.
  Publishing needs the write role on the repository.
- **Use one virtual repository, not two registries.** A virtual repository listing your
  hosted repository before the npmjs proxy never serves a name you publish from the proxy,
  whatever version the proxy offers, and a hosted `@scope/name` claims the whole scope.
  That is the dependency-confusion defence of invariant I5. See
  [ADR 24](docs/adr/0024-npm-packument-and-dist-tags.md).
- **A published version is immutable.** Publishing the same version again is refused, and
  so is publishing a version you deleted, even with identical bytes. Bump the version.
- **Files upstream drops disappear.** Once npmjs stops listing a file, a proxy answers 404
  for it at once, in a lockfile install too. It serves again if npmjs relists it, and the
  row is removed after `worker.retention.proxyStaleAfter`.
- **An upstream that refuses Sindri's credential is a 502**, not a 404, and fails a
  virtual repository's walk; a cached copy still serves. See
  [upgrading](docs/upgrading.md).
- **Token-authenticated private npm upstreams are not supported yet** (SIN-53). The
  `npm` preset reaches the public registry anonymously.

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
