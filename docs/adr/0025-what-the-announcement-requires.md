# 25. The announcement requires M10, a Helm chart, and the first half of M11

- **Status:** Accepted
- **Date:** 2026-11-05
- **Amends:** [ADR 22](0022-post-v1-direction-polyglot-teams-first.md) (the Helm chart's
  place, and when the announcement happens)
- **Relates to:** [ADR 23](0023-scanning-as-a-queue-job-and-the-open-core-line.md),
  roadmap "After v1"

## Context

ADR 22 allowed a public announcement once M10 was done, and put the Helm chart behind M11
because a polyglot team usually runs a VM or Compose. Planning the sprints after v1.1.0
asked what Sindri must honestly offer on the day it is announced. Security is the product,
so docs alone were not enough to say so.

## Decision

**The announcement follows v1.3.0 and requires:**

- OCI, PyPI, and npm, with the other formats promised for later.
- The docs of M10.
- Deployment guides for Docker Compose and for Kubernetes through a Helm chart.
- This claim, and its limits: with scanning enabled, every hosted publish is scanned, and
  promotion is refused above a severity. Scanning stays off by default, as ADR 23 decided.
  Cached proxy content is not scanned, because it has no subject. The claim's wording
  follows what the M11a ADR decides each protocol's scan covers, since a wheel or an npm
  tarball declares ranges and its scan goes less deep than an image's.

**A hardening release, v1.1.1, comes before M10.** It fixes what a first adopter or a
security reader would hit first, such as yarn 4 refusing every package on its default
settings (SIN-60).

**The Helm chart moves into M10, alone.** It runs one replica, because upload sessions live
in process memory (`internal/cas/cas.go`) and a chunked push would break across replicas.
Several replicas, split serve modes, and the admission webhook stay in Later. ADR 22's
audience does not change: this adds a way to deploy, not a new first user.

**M11 splits.** M11a (v1.3.0) is the config section, scan on publish, the severity gate, and
the Trivy image, which is what makes the claim true without extra installs. M11b (v1.4.0,
after the announcement) is the daily re-scan, the vulnerability UI, and the referrers API.

## Consequences

- The announcement is later than ADR 22 implied, by the two M11a sprints.
- At announcement, a CVE published after an artifact was scanned is not flagged until M11b.
  The announcement must not imply otherwise, and ADR 22's reasoning that Harbor users expect
  scanning is only partly met until then.
- Kubernetes users get a chart with no high availability at first, and the guide says so.
- The M10 gate gains a `kind` cluster.

## Rejected

- **Announcing at M10, as ADR 22 allowed.** It is sooner, but it invites security-minded
  readers before Sindri can enforce anything about vulnerabilities.
- **All of M11 first.** The re-scan and the UI strengthen the claim but are not needed to
  make it, and the referrers API is independent of it.
- **More protocols first.** Each is a milestone of its own, and these three cover the
  audience ADR 22 chose.
- **A multi-replica chart from the start.** It needs shared upload sessions and an audit of
  per-process state, which is work no first adopter on one team has asked for.
- **Leaving the chart in Later and documenting Compose only.** That was ADR 22's position.
  The project owner wants Kubernetes covered on announcement day.
