# 5. Configuration as code with a reconciler

- **Status:** Accepted
- **Date:** 2026-09-21

## Context

Two demands that pull against each other: teams want to create and shape their own
repositories without filing tickets, and organisations want guardrails that teams cannot
route around. Click-driven administration satisfies neither, because permission changes
become invisible and unreviewable.

## Decision

Two levels of declarative YAML. `OrgPolicy` sets binding guardrails including mandatory
and `locked: true` repositories. `Repository` lets teams define their own, including
virtual repositories with an explicit member order.

Manifests live in Git. A reconciler converges the database onto them and reports drift.
`sindri apply -f` and `sindri diff -f` are the interface, so existing CI drives it.

Two rules follow:

1. **Validation happens at write time, not per request.** The download path must stay a
   lookup, and a misconfiguration should fail in a pull request rather than three weeks
   later on a developer's machine.
2. **Structure is static, membership is live.** Manifests name groups such as
   `team:payments`; who belongs to that group is resolved against the identity provider at
   request time and never cached in the database.

## Consequences

- Every permission change goes through code review.
- Configuration is reviewable, diffable, and revertible.
- Repositories created through the API are marked as API-managed and distinguished from
  config-managed ones, so the reconciler and the API cannot fight over the same row.
- Some administration requires a commit. That is the intent, not a limitation.

## Rejected

- **A Git-polling operator in v1.** More moving parts and a second failure mode. A
  directory-based `apply` that CI calls covers the same ground; polling can come later
  without changing the manifest format.
- **Caching group membership in the database.** A revoked team membership would keep
  granting access until someone re-applied, which recreates the invisible-admin problem
  this decision exists to remove.
