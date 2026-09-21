# 6. OCI and PyPI first, other protocols after v1

- **Status:** Accepted
- **Date:** 2026-09-21

## Context

The pitch names five protocols: OCI, npm, PyPI, Maven, and Helm. Building all five before
a first release would roughly double the time to something usable, and would prove the
same architectural point five times instead of once.

## Decision

v1 ships two adapters: **OCI** and **PyPI**.

OCI is the anchor, because Harbor is the honest comparison and OCI is where Harbor is
strong. PyPI is the second adapter because it is structurally unlike OCI: a simple index
with immutable files, rather than manifests referencing layers. Two dissimilar protocols
over one core is a real test of the abstraction.

npm, Maven, and Helm follow after v1, npm first because it introduces mutable metadata
documents that neither v1 adapter exercises.

## Consequences

- A usable release arrives in a reasonable time.
- The shared core is validated against two genuinely different shapes, so the claim that a
  third adapter is cheap is tested rather than asserted.
- Helm is nearly free later, since Helm charts are already OCI artifacts.
- Teams needing npm or Maven cannot adopt Sindri at v1. Accepted, and stated plainly in
  the roadmap rather than left as an implied promise.

## Rejected

- **OCI only.** Would not test the multi-protocol core at all, and multi-protocol is the
  entire differentiator against Harbor. A second adapter arriving later would likely find
  the abstraction wrong, after the core had hardened around one protocol.
- **All five in v1.** Roughly doubles the work for diminishing architectural information.
