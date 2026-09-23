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

Through milestone M1, `sindri serve` boots: it reads one YAML file, applies its embedded
migrations to Postgres, serves `/healthz`, `/readyz`, and `/metrics`, and stores digest
verified blobs on a filesystem or any S3-compatible bucket. There is no protocol adapter
yet, so there is nothing to `docker push` at; that is M3.

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

## Why

The original pitch is in [pitch.md](pitch.md). In short: Nexus and Artifactory have grown
heavy, and Harbor is strong on OCI but was not designed to be multi-protocol. Small and
mid-sized teams end up stitching together Harbor, Verdaccio, and a homegrown PyPI server
because nothing sits in the middle. That gap is the target.

## License

Apache License 2.0. See [LICENSE](LICENSE).
