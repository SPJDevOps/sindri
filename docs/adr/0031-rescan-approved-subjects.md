# 31. Re-scanning approved subjects, and what a new finding does

- **Status:** Accepted
- **Date:** 2026-11-12
- **Relates to:** [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md),
  [ADR 23](0023-scanning-as-a-queue-job-and-the-open-core-line.md),
  [ADR 29](0029-what-a-scan-covers.md), [ADR 30](0030-severity-gate.md), roadmap M11b

## Context

A scan describes the vulnerabilities known on the day it ran. An approved subject keeps
running long after that, and a CVE published next month is invisible until something
scans it again. M11b closes that gap without changing what approval means.

## Decision

1. **What is re-scanned.** Every subject with a current `approve` row in the
   `lifecycle_current` view, for any stage. A schedule (job kind `rescan_approved`) runs
   every `scanning.rescanInterval` and enqueues one ordinary `scan` job per approved
   subject, reusing the scan job's dedupe key `scan:<digest>`, so a scan already queued
   or running is not doubled.
2. **Which repository the re-scan reads from.** A scan job carries a repository and reads
   only blobs that repository places (I2). The re-scan uses the subject's hosted owner
   repository (from `subject_owners`), the lowest-named when there are several. If that
   repository no longer places the subject, the scan job skips, as it already does, and
   is never a failure.
3. **The ledger is untouched.** A re-scan only adds evidence rows. It never writes
   `lifecycle_events`, never revokes, and never changes a decision already made. The
   next promotion's severity gate reads the newest Sindri scan, so a new finding blocks
   future promotions only.
4. **A new finding.** A vulnerability ID present in the newest Sindri scan for a platform
   and absent from the previous Sindri scan of the same subject and platform. The worker
   computes it when it attaches the scan and writes the sorted IDs as
   `new_vulnerabilities` in the scan evidence metadata. A first scan has no previous one
   and carries no `new_vulnerabilities` field. IDs are compared, not packages.
5. **Findings carry package detail.** Each entry of the metadata's `vulnerabilities` list
   gains `package`, `installed`, and `fixed` (Trivy's PkgName, InstalledVersion,
   FixedVersion; `fixed` is empty when there is no fix). Entries are one per (id,
   package, installed version); `severity_counts` keep their ADR 30 meaning. Older scans
   without these fields stay valid.
6. **The metric.** `sindri_approved_findings{severity}` is a gauge read at scrape time:
   the number of currently approved subjects whose newest Sindri scan (any platform) has
   at least one finding of that severity. The labels CRITICAL, HIGH, MEDIUM, LOW, and
   UNKNOWN are always present.
7. **On-demand re-scan.** `POST /api/v1/subjects/{digest}/scan` enqueues a scan of one
   subject. It needs write on a hosted owner repository of the subject (otherwise 404,
   like every subject route), answers 202 with the job, and answers 409 when scanning is
   disabled. It is the way to re-scan an npm version, which npm will not re-publish
   (`docs/scanning.md` promises this).
8. **Config.** `scanning.rescanInterval`, a duration, default `24h`. `0` disables the
   schedule; the on-demand route still works. Values below `1m` and negative values are
   refused.

## Consequences

- A re-scan costs one Trivy run per approved subject per interval. Cost follows what is
  approved, not the registry's whole history.
- An approved subject with a new CRITICAL finding stays approved. The gauge, the new
  finding list, and the UI show it; a person decides what to do.
- Scan evidence grows by one row per subject per interval. Retention of evidence rows is
  a separate question and is not decided here.
- Metadata grows by the package fields; the gate still reads only `id` and `severity`.

## Alternatives rejected

- **Auto-revoke on a new finding.** It rewrites what was approved and could take
  production down on a CVE that has no fix yet. Revoking stays a person's call.
- **A counter of new findings.** It resets on restart and is awkward to alert on. A gauge
  answers "is something in production vulnerable right now".
- **Re-scanning every subject.** Cost grows with history, and an unapproved artifact is
  not running anywhere.
- **Diffing by package instead of ID.** A package version bump that keeps the same CVE
  would look new, and one CVE reported against two packages would look like two.
- **Putting the interval under `worker:`.** It only exists when scanning is on, so it
  belongs beside the other scanning settings.
