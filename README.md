# Sindri

One registry for container images, Python packages, and npm packages, in a single Go
binary.

Polyglot teams tend to end up running Harbor for images, Verdaccio for npm, and devpi for
Python, each with its own users, its own backups, and its own idea of what is safe to
deploy. Sindri replaces all three with one server, one configuration file, and one sign-in.

| If you run | Sindri gives you | Move over with |
|---|---|---|
| Harbor | An OCI registry, Helm charts as OCI artifacts, and a proxy for Docker Hub and ghcr.io | [Migrating from Harbor](https://spjdevops.github.io/sindri/migrate/harbor.html) |
| Verdaccio | An npm registry for npm, pnpm, and yarn, with a proxy for npmjs | [Migrating from Verdaccio](https://spjdevops.github.io/sindri/migrate/verdaccio.html) |
| devpi | A private Python index for twine and pip, with a proxy for pypi.org | [Migrating from devpi](https://spjdevops.github.io/sindri/migrate/devpi.html) |

## The idea

An artifact has one identity, its digest. It is not copied from a `dev` repository to a
`prod` repository as it matures: its approval for each environment changes, and evidence
such as SBOMs and signatures attaches to that identity. So CI and Kubernetes can ask the
question they actually care about. Not "does this image exist?", but **"is exactly this
artifact approved for this environment?"**

## Try it in two minutes

You need Docker. This starts Sindri, Postgres, and a development identity provider, then
pushes an image and opens the web UI:

<!-- readme:start -->
```bash
curl -fsSLO https://raw.githubusercontent.com/SPJDevOps/sindri/main/deploy/quickstart/compose.yaml
export SINDRI_TOKEN=$(openssl rand -hex 16)
docker compose up -d --wait
echo "$SINDRI_TOKEN" | docker login localhost:8080 -u admin --password-stdin
docker pull alpine:3.20
docker tag alpine:3.20 localhost:8080/demo/app:1.0
docker push localhost:8080/demo/app:1.0
open http://localhost:8080/ui/
```
<!-- readme:end -->

On Linux, use `xdg-open` instead of `open`. This setup signs in anyone who opens the
browser, so it is for trying Sindri only. The
[quickstart](https://spjdevops.github.io/sindri/quickstart.html) goes further and
publishes an npm package.

## What you get

- **Protocols:** OCI (Docker images and Helm charts), PyPI, and npm.
- **Repositories:** hosted, proxy, and virtual, kept deliberately separate. A proxy can
  never serve a name you publish, which is the defence against dependency confusion.
- **Lifecycle:** stages and promotion that change an artifact's trust status without
  copying its bytes, with gates on evidence and an append-only audit ledger.
- **Scanning:** with the `-trivy` image and scanning enabled, every hosted publish is
  scanned, and promotion is refused above a severity you set. Works air-gapped with a
  mirror of Trivy's databases. See
  [Scanning and the severity gate](https://spjdevops.github.io/sindri/scanning.html).
- **Configuration as code:** one YAML file for one team, reconciled on startup, and
  manifests applied from CI when you have many teams.
- **Access:** OpenID Connect sign-in, machine tokens for CI, and role-based access. There
  is no anonymous access.
- **Storage:** a filesystem or any S3-compatible bucket.
- **A read-only web UI** showing each artifact's digest, provenance, evidence, and state,
  limited to what the signed-in person may read.

A whole single-team setup looks like this:

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

## Documentation

The full documentation is at **<https://spjdevops.github.io/sindri/>**.

- **Use it:** [pip and twine](https://spjdevops.github.io/sindri/clients/pip.html),
  [npm, pnpm, and yarn](https://spjdevops.github.io/sindri/clients/npm.html),
  [cosign and oras](https://spjdevops.github.io/sindri/clients/referrers.html), and the
  [configuration reference](https://spjdevops.github.io/sindri/configuration.html)
- **Deploy it:** [Docker Compose](https://spjdevops.github.io/sindri/deploy/compose.html),
  [Kubernetes with Helm](https://spjdevops.github.io/sindri/deploy/helm.html), and
  [TLS behind a reverse proxy](https://spjdevops.github.io/sindri/deploy/reverse-proxy.html)
- **Run it:** [backups, alerts, and sizing](https://spjdevops.github.io/sindri/operations.html),
  and [upgrading](https://spjdevops.github.io/sindri/upgrading.html)
- **Why it is built this way:** [ARCHITECTURE.md](ARCHITECTURE.md), the
  [decision records](https://spjdevops.github.io/sindri/adr/index.html), and the
  [roadmap](roadmap.md)

## Not there yet

So that nothing is silently missing:

- **Several replicas.** Sindri runs as one replica, because upload sessions live in memory.
- **Other formats.** Maven, and Helm's own repository format. Helm charts as OCI artifacts
  work today.
- **OCI:** cross-repository blob mounts.
- **Storage:** native Google Cloud Storage and Azure Blob Storage.
- **Auth:** SAML, LDAP, and per-path access rules.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md), which also covers running from source. Report
vulnerabilities privately as [SECURITY.md](SECURITY.md) describes, never in a public issue.

## License

Apache License 2.0. See [LICENSE](LICENSE).
