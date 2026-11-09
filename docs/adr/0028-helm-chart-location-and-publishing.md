# 28. The Helm chart lives in this repository and is published to ghcr as an OCI artifact

- **Status:** Accepted
- **Date:** 2026-11-09
- **Relates to:** [ADR 9](0009-one-configuration-loader-from-m1.md),
  [ADR 25](0025-what-the-announcement-requires.md), roadmap M10

## Context

ADR 25 moved a one-replica Helm chart into M10, and left its location and publishing to an
ADR. Three things constrain it. Upload sessions live in process memory
(`internal/cas/cas.go`), so a chunked push breaks if two pods serve one upload. There is
one configuration file and one loader, and a chart is an easy place to grow a second
format by accident. And the guide's install commands must be the ones a gate runs, so the
chart and the guide have to change together.

## Decision

**The chart is at `charts/sindri`,** reviewed and tested with the code it deploys.

**The release workflow publishes it to `oci://ghcr.io/spjdevops/charts/sindri`,** after the
release job succeeds, using the registry and token that already publish the image.

**The chart's version and `appVersion` are the release tag.** `Chart.yaml` in the
repository says `0.0.0-dev`, and the workflow sets both when it packages. A chart version
therefore names exactly one Sindri version, and there is no chart release without a Sindri
release.

**One replica, with no value to change it.** The Deployment uses `strategy: Recreate`,
because a rolling update runs two pods for a moment and an upload in progress could land
on either.

**The `config` value is `sindri.yaml`.** The chart writes that block into a ConfigMap
unchanged and the one loader reads it, so every key in the configuration reference works
and the chart adds no format of its own. Secrets stay out of it: `envFrom` names
operator-created Secrets, and the file refers to them with `${VAR}`. The chart contains no
credential, not even a development one.

**The operator brings Postgres, the blob store, the identity provider, and TLS.** The
chart bundles none of them. A PersistentVolumeClaim is available for the filesystem
backend, and is off by default.

**The optional Ingress lists Sindri's public paths** rather than routing `/`. `/metrics`,
`/healthz`, and `/readyz` share the main listener and carry no authentication, so routing
everything would publish them. They remain reachable inside the cluster through the
Service.

## Consequences

- The chart can't be patched without a Sindri release. A chart-only fix ships as a patch
  release of Sindri.
- `helm install` needs Helm 3.8 or later, the first release that can pull from an OCI
  registry by default.
- The `charts` package on ghcr is private when first pushed and has to be made public by
  hand, once.
- A rollout takes the server down briefly. One replica has downtime on any restart, and
  `Recreate` makes that explicit rather than racing uploads.
- Several replicas stay a separate decision (SIN-71), and it will have to move upload
  sessions out of process memory first.

## Alternatives rejected

- **A classic Helm repository on GitHub Pages.** It would allow `helm repo add`, but it
  shares the Pages deployment with the docs site, needs an `index.yaml` kept in step, and
  adds a second publishing path beside the registry that already holds the image.
- **A separate charts repository.** The chart could version independently, which nobody
  has asked for, and the guide's commands could drift from the chart they describe.
- **Bundling Postgres and MinIO as subcharts.** That makes the first install shorter and
  makes this project responsible for operating a database in someone's cluster. The
  quickstart already covers trying Sindri with nothing installed.
- **Typed values translated into `sindri.yaml`.** Each setting would exist twice, and the
  template would be a second configuration parser, which CLAUDE.md rules out.
- **A `replicaCount` value that defaults to 1.** A value invites being changed, and the
  failure it causes, a chunked push that breaks intermittently, would be hard to diagnose.
