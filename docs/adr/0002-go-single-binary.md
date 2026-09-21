# 2. Go, as a single binary with no runtime dependencies

- **Status:** Accepted
- **Date:** 2026-09-21

## Context

The product promise is "starts with one Compose file, grows to a Kubernetes
installation, one binary, not two products". The competition's weight is the thing we are
selling against, so operational lightness is a feature and not an implementation detail.

## Decision

Go, latest stable, compiled to a single static binary. Migrations, web assets, and
templates are embedded with `embed.FS`. One binary with several operating modes, never a
second product for "small" versus "large".

Concretely this rules out a Node build step for the UI, which is why the UI is Go
templates plus htmx.

## Consequences

- Deployment is copying one file. Upgrading is replacing one file.
- Strong concurrency primitives for parallel uploads and background jobs.
- Natural fit with the Kubernetes and OCI ecosystem, whose libraries are Go.
- The UI is less ergonomic to build than a React application would be. Accepted: the v1 UI
  is read-only browsing, which templates handle well.

## Rejected

- **Rust.** Comparable single-binary story and better raw throughput, but the OCI, Helm,
  and Kubernetes client ecosystem is Go, and we would hand-roll what we could import.
- **Java or Kotlin.** What Nexus and Artifactory chose. A JVM to operate and tune is
  precisely the weight we are positioning against.
- **Separate frontend build.** Breaks the single-binary promise the whole product rests on.
