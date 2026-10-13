# 23. Scanning runs as a queue job, and scanning is open source

- **Status:** Accepted
- **Date:** 2026-10-13
- **Relates to:** [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md),
  [ADR 20](0020-queue-table-and-background-worker.md),
  [ADR 22](0022-post-v1-direction-polyglot-teams-first.md),
  [pitch.md](../../pitch.md) section 6, roadmap M11

## Context

v1 stores evidence (`sbom`, `signature`, `scan`, `attestation`) and gates
promotion on its presence (ADR 16). It never reads the documents. CI can run a
scanner and attach the report, but nobody can enforce "no critical
vulnerabilities", only "a scan exists". And nothing notices when a new CVE is
published against something already approved for prod.

The polyglot team ADR 22 targets wants SBOMs and scans for images, wheels, and
npm tarballs in one place, without editing every pipeline, and wants to hear
about new CVEs in what is deployed. Harbor already scans images with Trivy, so
scanning decides whether this team switches.

There was also a contradiction to settle. pitch.md section 6 puts the advanced
policy engine, with the example "no prod promotion without an SBOM", in the paid
tier. v1 already ships that exact gate as open source.

## Decision

**Sindri runs the scanner itself, as an optional job on the queue table.**

- A `scanning:` section in the one configuration loader, off by default, names
  the `trivy` executable and an optional mirrored vulnerability database, for
  air-gapped installs.
- A hosted publish enqueues a `scan` job. The worker runs Trivy and attaches a
  CycloneDX `sbom` and a `scan` result to the subject as evidence, through the
  existing write path. Trivy covers OCI images, Python, and npm with one tool.
- A daily job re-scans every subject currently approved for any stage. A new
  finding adds evidence and shows in the API, the UI, and a metric. It never
  rewrites the ledger.
- Gates get a second rule kind, `{"scan": {"max_severity": "high"}}`. The
  decoder keeps rejecting rules it cannot evaluate.
- The binary and the default image stay free of dependencies. Trivy is an
  external executable, and a second release image includes it.

**The open-core line.** Free, as everything one team needs: scanning, SBOM
generation, the severity gate, and every presence gate, including "no prod
without an SBOM". Paid, as governance across many teams: per-CVE waivers with
expiry, licence policy, org-wide rules teams cannot override, and compliance
reports. This keeps the pitch's rule ("one team free, many teams paid") and
moves its example to the free side, where v1 already put it. pitch.md's table is
updated in M11.

## Consequences

- The worker gains a job that can run for minutes and fetch a vulnerability
  database. Leases (ADR 20) already handle long jobs. Air-gapped installs need
  the mirror setting from the start, not as an afterthought.
- A scan result is a claim about a point in time. The decision endpoint answers
  from the ledger and the current gates. Re-scans add evidence, and the ledger
  stays the record of what was approved and when.
- Once a deployment splits the worker from the API, scanning moves with the
  worker and needs no further design.

## Rejected

- **Ingest only.** Sindri would parse CycloneDX, SPDX, SARIF, and Trivy JSON that
  CI attaches, and never run a scanner. It is the least work, but every team
  must wire up every pipeline, and nothing re-scans when a new CVE appears. That
  misses the audience.
- **A separate scanner service.** A `sindri-scanner` process would take jobs over
  the API and post evidence back. The isolation is clean, but it is a second
  deployable and breaks the one-binary promise. The queue table already makes
  running the worker apart a configuration change later.
- **Trivy as a Go library inside the binary.** The dependency tree is large, it
  ties Sindri's release cadence to Trivy's, and it makes the scanner impossible
  to swap or upgrade on its own.
- **Scanning in the paid tier.** A small team would not choose Sindri over
  Harbor without it, and the free tier exists to win exactly that team.
