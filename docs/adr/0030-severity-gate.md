# 30. The severity gate: a ceiling, read from worker metadata

- **Status:** Accepted
- **Date:** 2026-11-11
- **Relates to:** [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md),
  [ADR 23](0023-scanning-as-a-queue-job-and-the-open-core-line.md),
  [ADR 29](0029-what-a-scan-covers.md), roadmap M11a

## Context

ADR 16 gates promotion on the presence of evidence. ADR 29 decided which scans a
severity rule may believe (Sindri's own, marked `producer: sindri`) and left the rule
itself open: what the number means, and how the gate reads a scan without doing
unbounded work inside the promotion transaction.

## Decision

**The rule is a ceiling.** `{"scan": {"max_severity": "high"}}` means HIGH is the
highest severity allowed: the gate refuses CRITICAL findings only. `medium` refuses HIGH
and CRITICAL, `low` refuses MEDIUM and above, and `critical` allows every named severity.
Valid values are `critical`, `high`, `medium`, `low`, lowercase. The order is CRITICAL >
HIGH > MEDIUM > LOW.

**UNKNOWN is refused by every ceiling.** Trivy reports UNKNOWN when it cannot rate a
finding. Passing it would let an unrated finding through, so the gate fails closed, as
ADR 29 does for a missing scan. `critical` therefore refuses exactly the UNKNOWN
findings.

**The gate reads metadata, not blobs.** The worker writes `vulnerabilities` into the
`scan` evidence metadata: a list of `{id, severity}`, de-duplicated by (id, severity)
and sorted by severity descending, then id. It is always present on a Sindri scan, an
empty list when clean. The gate runs inside the promotion transaction and reads only
that jsonb, so it opens no blob and holds no object-store connection under a lock, and
the 409 can name the CVE ids.

**Which scans.** The newest Sindri scan per platform is read (an index has one per
platform, and every platform must pass). A newer clean scan supersedes an older failing
one. A scan without `producer: sindri` never counts, whatever it says.

**Every platform of an index.** The worker attaches an index's platforms one after
another, so a failed job or a promotion mid-job can leave scans for only some. The worker
therefore writes `platforms` (the job's full platform list, sorted) into the metadata of
each scan of an index child; a single-manifest subject has no `platform` and no
`platforms`. The gate takes the `platforms` list of the newest Sindri scan as the required
set and refuses, with "scan incomplete: no Sindri scan for platform X", when any of them
lacks a scan. An index-child scan without `platforms` refuses like one without
`vulnerabilities`: scan again.

**Refusals.** No Sindri scan for the subject refuses. A Sindri scan whose metadata has no
`vulnerabilities` key refuses and asks for a re-scan; no released build has scans,
because scanning landed after v1.2.0, so this only guards against a partial upgrade.

**The ledger.** An approve event records the ids of the evidence each gate read, in a
nullable `evidence_ids uuid[]` column, which ADR 29 requires. A presence rule records the
evidence that satisfied it, a scan rule the scans it evaluated. The column is additive,
so one version back ignores it.

## Consequences

- A rule is stored as `{"scan":{"max_severity":"high"}}` beside `{"evidence":"sbom"}`;
  exactly one of the two keys is set.
- Metadata grows with the finding count. A scan with thousands of findings stores a few
  hundred kilobytes of jsonb; that is the price of reading no blob.
- A later, worse scan does not revoke an approval (ADR 23). The ledger shows what was
  evaluated.

## Alternatives rejected

- **Threshold semantics** (`max_severity: high` refuses HIGH and above). It reads like a
  limit but fails on the value it names; users write `high` meaning "up to high".
- **Reading the report blob inside the promotion transaction.** It holds a transaction
  open across object-store I/O and parses megabytes under load.
- **UNKNOWN passing.** An unrated finding would be waved through by exactly the rule
  meant to catch what was not assessed.
- **Counts only (`severity_counts`).** They cannot name a CVE, so the refusal could not
  tell the operator what to fix.
