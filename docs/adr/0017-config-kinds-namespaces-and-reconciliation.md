# 17. Config kinds, namespaces, and reconciliation

- **Status:** Accepted
- **Date:** 2026-10-02
- **Relates to:** [ADR 5](0005-configuration-as-code.md), [ADR 7](0007-config-surface-short-and-long-form.md),
  [ADR 9](0009-one-configuration-loader-from-m1.md), [ADR 15](0015-upstream-credentials-source-deferred-to-m7.md),
  [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md), roadmap M7

## Context

ADR 7 fixed the shape of the configuration surface: one loader, a short form
that desugars totally into a long form, and one reconciler with two triggers.
M7 builds it, and has to settle what ADR 7 left open:

1. What a team namespace is, and how a reference crosses one.
2. How the long form spells a member reference, given that repository names
   contain slashes.
3. Which kinds exist, and where `lifecycle:` and `defaults:` go.
4. What the reconciler may delete, and who owns a row when both triggers run.
5. What "lifecycle configuration on a proxy" (I4) means at the repository level.

## Decision

**A namespace prefixes the name.** `metadata.namespace: payments` with
`metadata.name: python` is the repository `payments/python`, on the wire and in
the database. A document with no namespace is organisation-level and keeps its
bare name. The namespace is then visible in every URL that reaches the
repository, and two teams cannot collide. `org` is reserved, and an
organisation repository may not be named `org/...`.

**References are type-qualified and resolve locally first.** A long-form
member is `ref: <type>/<ref>`, for example `hosted/internal-py`, so the expanded
form shows every member's type (I5 made visible). The type is checked against
the target. The ref after it resolves as follows (`reconcile.ResolveRef`):

- A ref without a slash is local: inside `payments`, `internal` is
  `payments/internal`, never an organisation repository.
- `org/<name>` reaches the organisation.
- Any other ref with a slash is tried locally, then taken as a full name,
  which is how `search/index` reaches another team.

Every cross-boundary reference is therefore visibly one. The short form writes
bare names and gets the type prefix from inference. A member outside the file
must be written already qualified.

**Three kinds.** `Repository`, `OrgPolicy`, and `Lifecycle`. The short
`lifecycle:` section desugars into a `Lifecycle` document, so the long form can
express everything the short form can, which is what "total" requires.
`defaults:` is sugar only: it is applied while desugaring and is absent from
the long form. `config show --expanded` therefore prints every default written
out. Durations print in their shortest exact form (`30m`, not `30m0s`).

**Repositories own their access lists and stage restrictions.** `access.read`
and `access.write` list identity-provider groups, or `authenticated`.
`lifecycle.stages` restricts which stages a hosted repository promotes into, and
promotion enforces it (`NOT_PROMOTABLE`). It is never accepted and ignored. On a
proxy or virtual repository it is the I4 error. Both live in
`repositories.config` (jsonb) beside `namespace` and `source`, so authorizing a
pull reads nothing beyond the repository row the adapter already fetches.

**One row, one owner.** `managed_by` separates API rows from config rows. The
reconciler refuses, and never overrides, a name the API created. Within config
rows, `source` records the trigger: `file` (serve --config) or `apply`. Each
trigger refuses the other's rows (`MANAGED_ELSEWHERE`). A deployment uses one
trigger per namespace. The type and protocol of an existing repository are
immutable (`IMMUTABLE_FIELD`), because changing them is a delete and a
re-create, which the reconciler never does implicitly.

**Pruning is opt-in.** A config-managed repository in scope that desired state
no longer lists is reported as orphaned. `sindri apply --prune` deletes it.
`serve --config` never deletes. A repository with ledger history still cannot
be deleted (`HAS_HISTORY`, ADR 16). Scope is the namespaces the applied
manifests speak for. A namespace absent from an apply is never emptied by it.

**One transaction.** Validation against the database, the organisation policy,
the stages (through `lifecycle.SetStagesIn`, the writer the API also uses), and
the repositories commit together or not at all. `diff` is the same reconcile,
rolled back, so the two cannot disagree.

**Locked repositories and team scope.** An `OrgPolicy` lists mandatory
repositories. `hybrid` and `centralized` require them to exist, and `locked`
marks the row. A namespace administrator, named in `OrgPolicy.namespaces` or
holding a namespace-scoped admin token, may apply only that namespace. Any
document that targets a locked or organisation repository is refused
(`LOCKED`). Only a global administrator changes a locked repository.
`centralized` refuses namespaced documents outright.

**Credentials (ADR 15, completed).** The `credentials:` section of sindri.yaml
backs `remote.Credentials` through an atomically swapped store. An unknown
reference fails `config check` (when the file carries the section) and every
reconcile. `config show` redacts secrets. A reload installs new credentials
before reconciling and restores the old ones if the reconcile fails.

## Consequences

- One sindri.yaml with fifteen lines brings up a working two-protocol setup on
  boot. The gate script shows it, with a docker push into a repository nobody
  created by hand.
- The golden test (`TestManifests_ShortAndLongFormsAreIdentical`) compares
  trees. The integration test
  (`TestReconcile_ShortAndLongFormsProduceIdenticalDatabaseState`) compares
  database state, excluding only the `source` key, which records the trigger
  rather than the state. Both were checked to fail when the desugarer drops a
  field.
- Teams that want to move a repository between triggers must delete and
  re-create it, or edit `source` by hand. That is rare enough to leave
  explicit.
- A team that removes its last repository leaves its namespace out of the
  apply, so nothing is pruned. Orphans are still reported by a diff of any
  manifest that names the namespace.

## Rejected

- **Namespace as ownership metadata only, names global.** It avoids changing
  URLs, but `team/x` would then be ambiguous between a team reference and an
  organisation repository whose name contains a slash, and two teams could
  fight over one name.
- **Bare member names in the long form.** Shorter, but the expanded form would
  then hide the hosted/proxy split that I5 depends on.
- **Pruning by default.** Pure GitOps convergence, but a bad merge would delete
  a repository's packages. For a registry, the asymmetry favours reporting.
- **Letting both triggers manage the same rows.** Simple until they disagree.
  Then a SIGHUP silently reverts what CI applied, which is the invisible fight
  ADR 5 rules out.
- **Storing the last applied manifests to compute diffs.** A second source of
  truth. Diffing against the live rows also reports drift made behind the
  reconciler's back, which is what `diff` is for.
- **`defaults:` as a long-form kind.** It would be a template mechanism by
  another name, the road to `extends:` that ADR 7 closed.
