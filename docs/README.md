# Sindri

Sindri is an artifact repository for OCI images, Python packages, and npm packages,
shipped as one Go binary. An artifact keeps one identity as it matures: it is not copied
from repository to repository, its approval for each environment changes. CI and
Kubernetes can ask whether exactly this artifact is approved for this environment.

These pages are built from the `docs/` directory of
[the repository](https://github.com/SPJDevOps/sindri), so they are versioned and reviewed
with the code they describe.

- [Quickstart](quickstart.md): a registry with a web UI on one machine, in about two
  minutes.
- [Configuration reference](configuration.md): every key `sindri.yaml` accepts, in short
  and long form.
- [Upgrading](upgrading.md): what happens on startup, the behaviour changes in each
  release, and how to roll back.
- [Architecture decision records](adr/index.md): why Sindri is built the way it is, and
  what was rejected.

The design is in
[ARCHITECTURE.md](https://github.com/SPJDevOps/sindri/blob/main/ARCHITECTURE.md), and
what comes next is in
[roadmap.md](https://github.com/SPJDevOps/sindri/blob/main/roadmap.md).
