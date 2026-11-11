# 29. What a scan covers, and which scans a severity gate trusts

- **Status:** Accepted
- **Date:** 2026-11-10
- **Relates to:** [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md),
  [ADR 19](0019-garbage-collection-roots-write-intent-and-sweep.md),
  [ADR 20](0020-queue-table-and-background-worker.md),
  [ADR 23](0023-scanning-as-a-queue-job-and-the-open-core-line.md),
  [ADR 25](0025-what-the-announcement-requires.md), roadmap M11a

## Context

ADR 23 decided that Sindri runs Trivy as a queue job and attaches the result as evidence.
ADR 25 left one thing open: what each protocol's scan covers, because a wheel or an npm
tarball declares dependency ranges and its scan goes less deep than an image's. The
severity gate in the next sprint needs a second answer: whose scans it may believe.
Evidence attached by a client is a claim by the client, and a gate that reads it can be
satisfied by anyone who can publish.

## Decision

**What Trivy is pointed at, per subject kind.**

- `oci-manifest`: the worker writes the manifest, config, and layers into an OCI image
  layout in a temp directory and runs `trivy image --input <layout>`. The evidence claims
  the OS packages and language packages installed in the image, at their pinned versions.
  It claims nothing about what is not in the layers.
- `oci-index`: each child image manifest is scanned the same way, and the index subject
  gets one `sbom` and `scan` pair per platform, with `platform` in the metadata. Children
  pushed on their own are subjects too and have their own scans. Only a BuildKit
  attestation (annotated as one and with platform `unknown/unknown`) is skipped; the label
  alone proves nothing. A nested index or a child of unknown media type fails the whole job
  with no evidence, as does any blob the publishing repository does not hold (the job names
  that repository, and the scanner checks placement before reading each blob). Shared layers
  are staged once per job and hard-linked into each platform's layout. Two distinct children with the same platform
  (os, architecture, variant, os.version, os.features) fail the job with no evidence, so a
  gate reading the newest scan per platform cannot be shown the benign one only.
- `pypi-dist` (wheel or sdist) and `npm-tarball`: the worker unpacks the archive into a
  temp directory, with a size cap and path traversal refused, and runs `trivy rootfs`. The
  evidence claims the package's own name and version, and anything pinned inside the
  archive: a bundled `node_modules`, `npm-shrinkwrap.json`, vendored code, a `dist-info`
  or `egg-info`. It does **not** claim the declared dependency ranges, which are never
  resolved. An sdist without `egg-info` metadata may report nothing about itself.
- `file`: not scanned. The job succeeds and logs the unsupported kind.

Cached proxy content is never scanned, because it has no subject (ADR 16).

**The evidence.** One Trivy run per target produces the JSON report (`scan`, media type
`application/vnd.aquasec.trivy.report+json`), and `trivy convert` turns that report into
CycloneDX (`sbom`, `application/vnd.cyclonedx+json`). Both are written through
`evidence.Attach` and so through `cas`, with its write intent recorded first (ADR 19).
Metadata keys: `producer`, `scanner`, `scanner_version`, `db_updated_at`,
`severity_counts` (CRITICAL, HIGH, MEDIUM, LOW, UNKNOWN), `coverage` (`image` or
`package`), and `platform` for an index child.

**Trust.** Severity rules read only evidence whose metadata says `producer: sindri`. Only
the worker writes that key. The API always stores `{}` as metadata and cannot be made to
set it, so a client cannot forge a clean scan. Client-attached scans still satisfy
presence gates, which ADR 16 does not change.

**Gate semantics, recorded now for the severity gate.** The rule reads the newest Sindri
scan per platform. If there is none, because the job is queued, failed after its retries,
or the subject was published before scanning was on, the gate refuses. The ledger records
which evidence ids were evaluated. A scan is a point-in-time claim, and later evidence
never rewrites a decision (ADR 23).

**Failures.** A failing scan retries through the queue's backoff, with the same maximum
attempts as other jobs. A subject whose scan job is exhausted has no Sindri scan, so a
severity gate refuses it.

## Consequences

- A clean wheel or tarball scan means its own version and its pinned contents have no known
  findings. It does not mean what `pip` or `npm` will install alongside it is clean.
  Teams that need that scan their lockfile in CI and attach the result for presence gates.
- The announcement wording has to match these limits (ADR 25), and the scanning docs page
  (SIN-72) states them.
- Turning scanning on does not scan what was published earlier. Those subjects are refused
  by a severity gate until they are scanned.
- The scan job must pull every layer of an image into a temp directory, so disk use is
  bounded by image size, and archives are bounded by the size cap.

## Alternatives rejected

- **Resolving dependency ranges.** The result would depend on the hour of the scan, so the
  same bytes could pass today and fail tomorrow, and it cannot be reproduced.
- **Trusting client-attached scans for severity.** They are forgeable by anyone who can
  publish.
- **SARIF as the scan format.** It carries severity poorly and differently per tool, and
  the gate needs to read it.
- **Scanning an index as one image.** Trivy picks one platform and the rest go unscanned.
- **Scanning proxy content.** It has no subject to attach evidence to (ADR 16).
- **Generating the SBOM with a second tool.** Two runs could disagree about what the
  artifact contains.
