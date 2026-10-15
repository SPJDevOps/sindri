# 16. Lifecycle, gates, and evidence in v1

- **Status:** Accepted
- **Date:** 2026-10-01
- **Relates to:** [ARCHITECTURE.md](../../ARCHITECTURE.md) sections 2 (I3, I4) and 5,
  [ADR 4](0004-lifecycle-as-append-only-ledger.md), roadmap M6 and M7

## Context

M6 implements the identity layer that [ADR 4](0004-lifecycle-as-append-only-ledger.md)
designed. Seven questions were open:

1. Where do stage definitions come from before M7 brings `lifecycle:` to the config file?
2. Who owns an artifact while pushes are still anonymous?
3. What can a gate express?
4. Does stage order mean anything?
5. What does the decision endpoint answer for an environment it does not know?
6. How is the ledger protected from rewrites?
7. What does provenance promise?

## Decision

**Stages and gates come from the management API until M7.**
`PUT /api/v1/lifecycle/stages` replaces the ordered stage set and the gates of each
stage in one transaction. `lifecycle.ValidateStages` needs no database, so M7's
reconciler can reject a bad `lifecycle:` section at `config check` time. It then
applies the section through the same `SetStages` call. One validator and one writer
serve both sources. A stage that has ledger history cannot be removed
(`STAGE_IN_USE`). It can be reordered and its gates can change.

**The owner is the repository.** A subject published into hosted repository `demo` is
owned by `repository:demo`. Nothing else about the publisher is verified today: OCI push
is anonymous, and PyPI accepts any Basic username (ADR 12). M7 adds user and team owners
alongside this one, not instead of it.

**A gate is one rule: evidence of a type must be present.** It is stored as
`{"evidence": "sbom"}` in `promotion_gates.rule`. The decoder rejects unknown keys, so
a rule this build cannot evaluate is refused when it is written. It is never accepted
and then ignored. The evidence types are `sbom`, `signature`, `scan`, and
`attestation`. v1 stores the documents and checks that each type is present. It does
not parse them or verify signatures (roadmap scope). A refused promotion returns 409
`GATE_FAILED` with a `missing` list, in the order the stage declares its gates.

**Stage order is for display only.** Positions order the stages in the API and the UI.
Promotion to `prod` does not require a current approval at `integration`. A
"must pass integration first" rule would be a second rule kind. It is left out until
someone needs it, rather than being built into stage order where nobody can opt out.

**Decisions fail closed.** `GET /api/v1/decisions` always answers a well-formed question
with 200 and either `approved` or `denied`. An unknown environment is `denied` with
reason `unknown_environment`, not 404. An admission controller with a typo in its
environment name then blocks deployment instead of reading a 404 as "no policy". A
digest is approved for a stage when any hosted repository has a current `approve` for
it at that stage. The approving repositories are listed in the response, but only
those the caller may read. The answer is for any authenticated caller, since an
admission controller asks with whatever credential it holds; an approval names a
repository, an actor, and a reason, which ADR 18 does not disclose to someone who
cannot read that repository. (Amended in v1.0.1, when the route-walk test found
the full list going to every caller.)

**The database enforces the ledger.**
- A trigger refuses `UPDATE`, `DELETE`, and `TRUNCATE` on `lifecycle_events`.
- The ledger's foreign keys to `subjects`, `repositories`, and `lifecycle_stages` do not
  cascade. Deleting a repository that has promotion history therefore fails, and the
  API maps that failure to 409 `HAS_HISTORY`. Without this, `ON DELETE CASCADE` from
  `repositories` would erase the audit trail as a side effect of a cleanup.
- Current state is the view `lifecycle_current`. It is a `DISTINCT ON` over
  `lifecycle_events_current_idx`. A test checks with `EXPLAIN` that the decision query
  uses that index.

**Provenance is a recorded claim.** Each publish path records a `source_type`:
`oci-push`, `pypi-upload`, or `api-publish`.
- OCI push takes `source_ref` and `build_ref` from the
  `org.opencontainers.image.source` and `org.opencontainers.image.revision` annotations.
  The `X-Sindri-Source-Ref` and `X-Sindri-Build-Ref` headers override them.
- The other paths read the headers only.

Nothing verifies these claims. A repeat publish of the same bytes with the same claim
does not add a row.

**Subjects exist only for hosted content.** `recordSubject` runs inside the hosted
publish transaction. It does nothing for any other repository type. The proxy cache
paths never call it, and `TestSubjects_ProxyCacheCreatesNoSubject` guards this. A subject
outlives its placements: deleting a file or manifest removes the placement but leaves the
subject, its evidence, and its ledger. The bytes have the same identity wherever they
are deployed.

## Consequences

- Promotion and revocation each append one row. `TestLifecycle_PromotionStory` asserts
  that they change neither the stored blob count nor the `blobs` table. Attaching
  evidence adds exactly one blob per new document, which is the document itself.
- Evidence blobs have no repository placement. They are read only through
  `/subjects/{digest}/evidence/{id}`, which is scoped by subject.
- Garbage collection (M8) must treat `subjects` and `evidence.blob_digest` as roots. The
  foreign keys to `blobs` make a mistake here fail loudly rather than corrupt anything.
  *Refined by [ADR 19](0019-garbage-collection-roots-write-intent-and-sweep.md): a
  subject is a root only when it has ledger history or evidence, so an untagged,
  unpromoted image is collected.*
- Gate configuration changes are not audited. Only the promotions they allowed are.
  Auditing policy changes is a follow-up that fits the commercial audit-retention
  feature.
- A repository with promotion history can never be deleted through the API. An
  operator who really needs to delete one has to make that decision in SQL, which is
  deliberate.

## Rejected

- **A mutable status column**, for the reasons ADR 4 gives.
- **Cascading deletes from repositories into the ledger.** This is the default
  everywhere else in the schema, and here it would silently destroy history.
- **Strict stage sequencing.** It can be expressed later as an explicit rule kind. Built
  into stage order, it would be policy that nobody chose.
- **The unverified caller name as owner.** A Basic username or a header looks richer
  but records unchecked claims as ownership. Ownership is what RBAC will key on in M7.
- **A `lifecycle:` config section in M6.** That would pull part of M7's loader and
  golden-test work forward and leave M7 to finish it. The API path and the reconciler
  path share the same validator and writer instead.
- **404 for an unknown environment.** An admission controller has to map every status
  code to allow or deny, and a 404 is the easiest one to get wrong.
