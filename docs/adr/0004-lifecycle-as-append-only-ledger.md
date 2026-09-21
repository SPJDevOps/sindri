# 4. Lifecycle as an append-only event ledger

- **Status:** Accepted
- **Date:** 2026-09-21

## Context

The differentiating idea is that an artifact is not copied between repositories as it
matures; its trust status changes. Conventional repositories model maturity as
`docker-dev` → `docker-staging` → `docker-prod`, copying bytes at each hop, which makes
"is this the artifact we tested?" a question of faith rather than of fact.

We also need an audit trail. "Who promoted this to production, when, and why" must be
answerable months later, and audit retention is a planned commercial feature.

## Decision

Lifecycle state is an append-only table, `lifecycle_events`, keyed by subject digest.
Promotion and revocation each append one row. No row is ever updated or deleted.

Current state is a database view selecting the latest event per
`(subject_digest, repo_id, stage_id)`, with the key columns indexed because the admission
decision endpoint sits on the hot path of every deployment in the organisation.

Promotion applies only to hosted repositories (invariant I4).

## Consequences

- Promotion is one `INSERT`. Nothing is copied, and an integration test asserts the blob
  count in storage is unchanged across a full promotion sequence.
- The audit trail is a byproduct rather than a second system, and audit retention becomes
  a query.
- Revocation preserves history instead of erasing it.
- Reads cost a view lookup rather than a column read. Accepted, and mitigated by indexes.
- The table grows without bound and will eventually need partitioning by time. Acceptable
  at v1 scale and a known follow-up.

## Rejected

- **A mutable status column.** Faster to read and much easier to get wrong. It destroys
  the audit trail, which is the feature. Explicitly guarded against in `CLAUDE.md`,
  because "the view feels slow" is exactly how it would get introduced.
- **Copying between repositories per stage.** The industry norm, and the thing we are
  arguing against: it multiplies storage and severs the identity link that lets you prove
  the artifact in production is the one that passed the tests.
