# 22. After v1, serve polyglot app teams first

- **Status:** Accepted
- **Date:** 2026-10-13
- **Relates to:** [pitch.md](../../pitch.md) sections 1, 6, and 7,
  [ADR 6](0006-oci-and-pypi-first.md), roadmap "After v1"

## Context

v1 shipped with OCI and PyPI, hosted, proxy, and virtual repositories, lifecycle
and evidence, configuration as code, OIDC, and a read-only UI. The list of
possible next steps is long: a docs site, a better README, npm and Maven, CVE
scanning and SBOMs, a Helm chart, the admission webhook, split serve modes.
Built in arbitrary order, each one is useful but none of them makes Sindri the
obvious choice for anyone.

The pitch already names the audience in general terms: small and mid-sized
teams, won through open-source adoption before enterprise. Within that audience
there are at least three different first users, and each one orders the work
differently.

## Decision

**The first audience is the polyglot app team.** These teams run Python services
and a JavaScript frontend, and today they stitch together Harbor, Verdaccio, and
a homegrown PyPI server. The next phase is done when such a team can retire all
three. This is the gap pitch.md section 1 names.

**The order is v1.0.1, M9 npm, M10 adoptable, M11 scan to evidence.**

- v1.0.1 closes what the v1 checklist pass found: no route-walk authorization
  test on `/api/v1`, Helm over OCI verified only by hand, and a flaky
  testcontainers reaper.
- npm comes before the docs site. Without npm this audience cannot drop
  Verdaccio, so docs would sell something they cannot fully use yet. The README
  is enough for early adopters until then.
- M10 makes Sindri adoptable by someone who has never seen it: a docs site, a
  generated configuration reference, migration guides from Verdaccio, devpi, and
  Harbor, and an operations guide. Migration means a Sindri proxy in front of the
  old registry, not an import tool.
- M11 adds scanning, because Harbor already scans with Trivy and a team will not
  replace it with something that does less. [ADR 23](0023-scanning-as-a-queue-job-and-the-open-core-line.md)
  decides how.

**No public announcement before M10.** First impressions come from the docs, so
the docs come first.

**Docs live in the repository**, published as a site, not in a GitHub wiki. A
wiki is not versioned with the code and is not reviewed in pull requests.

## Consequences

- The Helm chart, split serve modes, and the admission webhook move behind M11.
  A polyglot team usually runs a VM or Compose, so a Helm chart for Sindri does
  not change whether they can adopt it.
- Maven waits. It is a third protocol for a different audience.
- The admission webhook, the clearest demonstration of the thesis, waits for the
  Kubernetes milestone. The decision endpoint gets production use first, which
  is what the v1 roadmap wanted anyway.

## Rejected

- **Kubernetes platform teams first.** They would put the Helm chart, HA, and the
  admission webhook first. That competes with Harbor on Harbor's home ground,
  where Harbor is mature and multi-protocol support matters least.
- **Security-first teams first.** They would put scanning, SBOMs, signature
  verification, and the webhook first. That is where the pitch expects revenue,
  but selling the paid layer before there are users of the free one reverses the
  open-core strategy in pitch.md section 7.
- **Docs first, then npm.** It is better if an announcement is near, and none is
  planned. It would polish the onboarding for a product the target team cannot
  yet fully switch to.
- **npm and docs in one milestone.** It makes one coherent launch, but it would be
  the largest milestone yet, and CLAUDE.md works one milestone at a time.
