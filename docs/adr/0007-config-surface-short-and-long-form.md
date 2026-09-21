# 7. A configuration surface that scales down: short form, long form, one reconciler

- **Status:** Accepted
- **Date:** 2026-09-21
- **Relates to:** [ADR 5, Configuration as code](0005-configuration-as-code.md), which this
  extends rather than supersedes

## Context

ADR 5 settled that configuration is declarative YAML in Git, converged by a reconciler.
It described that configuration in one shape: enveloped `kind`/`metadata`/`spec`
documents, applied with `sindri apply -f`.

That shape was designed for the organisation with many teams, and it fits that case well.
It fits the opposite end badly. A single team running Docker Compose has no organisation
to govern, one file rather than a directory, and one process rather than a cluster.
Handing them an `OrgPolicy` document, a `kind` envelope around every repository, and a
separate apply step is ceremony with nothing bought for it.

That matters more here than it would elsewhere. "Starts with one Compose file" is the
product's central promise, and small and mid-sized teams are the first adoption target.
A configuration surface that feels like Kubernetes to a two-person team contradicts the
pitch at the exact moment of first impression.

Two observations shaped the decision. First, the Kubernetes feel comes mostly from the
*workflow*, the separate apply step against a reconciler, rather than from the envelope.
Second, the envelope and the workflow are independent, so they can be fixed separately.

## Decision

Three parts.

### A short form that desugars into the long form

One schema, one loader, two spellings. The short form is a flat `sindri.yaml` with a
`repos:` map; repository type is inferred from which key is present (`members` means
virtual, `upstream` means proxy, neither means hosted, both is an error). A virtual
repository's protocol is inferred from its members, which must all share one protocol.
Upstreams accept a named preset. The long form is the enveloped document set from ADR 5.

**Desugaring is one-way and total.** The short form has no representational power the long
form lacks. Loading produces long form, and nothing downstream may branch on which
spelling was written.

`OrgPolicy` becomes optional: absent means `mode: delegated` with no mandatory
repositories. Adopting governance is adding a file, never migrating a format.

### One reconciler, two triggers

`sindri serve --config sindri.yaml` reconciles on boot and on `SIGHUP`. `sindri apply -f
<dir>` reconciles from CI against a running server. The reconciler is identical; only the
invocation differs. This removes the apply step entirely for single-node installations and
is the larger half of making the configuration feel native to Compose.

### Two commands that keep the sugar honest

`sindri config check` validates statically, with no database and no server. `sindri config
show --expanded` prints the desugared long form.

## Consequences

- A complete two-protocol setup with lifecycle stages is about fifteen lines, and needs no
  apply step. That is the Compose-scale experience the pitch promises.
- The enterprise path is unchanged from ADR 5. Nothing is taken away.
- `config show --expanded` makes the inference inspectable, so the short form is
  discoverable sugar rather than magic, and it is the natural first tool when resolution
  order surprises someone.
- `config check` runs in CI with no infrastructure, which suits teams whose entire
  deployment is a Compose file.
- Two spellings are more surface to maintain and to document. Accepted, and bounded by the
  one-way desugaring rule plus a golden test asserting both spellings of one setup produce
  identical internal trees. Without that test this decision becomes two parsers that
  drift, so the test is part of the decision, not an implementation detail.
- Type inference means a reader cannot see a repository's type without knowing the
  inference table. Mitigated by keeping explicit `type:` valid, and by
  `config show --expanded`.

## Rejected

- **Only the enveloped long form.** Correct for the enterprise, actively hostile to the
  first adoption target, and contradicts the product's own pitch.
- **Only a flat short form.** Cannot express multi-team governance, and would have to grow
  an envelope later under pressure, which is how configuration formats acquire two
  incompatible generations.
- **A separate simplified config file for Compose users.** The obvious shortcut and the
  worst outcome: two schemas, two validators, two documentation sets, and a translation
  layer that silently drifts. One schema with two spellings costs a desugaring pass and a
  golden test.
- **Collapsing a virtual repository and its members into one entry**, such as
  `python: {hosted: ..., proxy: ...}`. The most tempting piece of sugar available and
  firmly rejected. The proxy/hosted/virtual separation is the dependency-confusion defence
  of invariant I5. Hiding it in configuration teaches users to stop reasoning about it, and
  the guarantee then erodes socially even while the code still enforces it. Sugar may make
  the three types cheap to declare; it may not make them invisible.
- **`extends:` for repository templates.** Per-type `defaults:` plus YAML anchors cover the
  real cases. Transitive template inheritance is a well-known road to unreadable
  configuration. Revisit with a new ADR if per-type defaults prove insufficient.
- **A Git-polling operator to remove the apply step.** Reconcile-on-boot achieves the same
  thing for the single-node case with no extra moving parts. Polling remains available
  later without a format change, as ADR 5 already noted.
