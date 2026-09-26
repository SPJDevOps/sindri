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

Through milestone M4, `sindri serve` boots from one YAML file (or none), applies its
embedded migrations to Postgres, stores digest-verified blobs on a filesystem or any
S3-compatible bucket, and serves two protocols:

- **`/v2`** — the OCI distribution API. `docker push` and `docker pull` work, and the
  upstream distribution-spec push, pull, and content-management workflows pass.
- **`/pypi`** — hosted PyPI. `twine upload` and `pip install` work. See
  [using Sindri as a pip index](#using-sindri-as-a-pip-index).

Alongside them: `/api/v1` for repository, package, and version management, plus
`/healthz`, `/readyz`, and `/metrics`.

Not there yet: proxy and virtual repositories (M5), lifecycle and evidence (M6), and real
authentication (M7). **Authentication today is a placeholder:** the management API takes
one static bearer token, and the protocol adapters do not check identity at all. Run it on
a trusted network and not on the open internet.

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

Not yet available. It arrives with milestone M8 and will be verified from a clean
machine with nothing installed but Docker.

To run what exists today from source, with Go and Docker installed:

```bash
make up            # Postgres on 55432, MinIO on 59000
make build
./bin/sindri serve # no configuration file needed; defaults match the Compose stack
curl -fsS localhost:8080/readyz
```

## Using Sindri as a pip index

Sindri is usable as a private Python index today, for both publishing and installing.
Starting from the Quickstart above, declare a repository once — PyPI repositories are
never auto-created, so a typo in a `--repository-url` fails loudly instead of silently
creating a second index:

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
  -u token -p anything dist/*

pip install --index-url http://127.0.0.1:8080/pypi/internal/simple/ \
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
password = anything
```

Worth knowing before you point real work at it:

- **Any credentials are accepted on upload.** Twine requires a username and password, so
  Sindri requires the header and ignores it until M7. The simple index and downloads are
  anonymous. See [ADR 12](docs/adr/0012-pypi-hosted-adapter.md).
- **Only hosted repositories exist.** There is no pypi.org proxying until M5, so a package
  whose dependencies are not in your repository needs an `--extra-index-url`. That is
  exactly the dependency-confusion shape M5 closes: with two indexes, pip chooses by
  version and not by trust.
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

## License

Apache License 2.0. See [LICENSE](LICENSE).
