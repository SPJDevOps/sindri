# 33. What is open stays open, paid work is added beside it, and the CNCF is a goal

- **Status:** Accepted
- **Date:** 2026-11-16
- **Supersedes:** the open-core line in
  [ADR 23](0023-scanning-as-a-queue-job-and-the-open-core-line.md), ARCHITECTURE.md
  section 10 as it stood, and
  [pitch.md](https://github.com/SPJDevOps/sindri/blob/main/pitch.md) section 6 as it stood
- **Relates to:** [ADR 4](0004-lifecycle-as-append-only-ledger.md),
  [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md),
  [ADR 18](0018-authentication-and-rbac-in-v1.md),
  [ADR 22](0022-post-v1-direction-polyglot-teams-first.md), roadmap "After v1"

## Context

The pitch planned an open core: what one team needs is free, and what an organisation
needs to govern many teams is paid. Its paid column listed SSO, audit logging with
retention, high availability and multi-region replication, the advanced policy engine,
and managed hosting. ADR 23 moved scanning and the severity gate to the free side and
kept the rest.

Three things have changed since.

- The code went another way. OIDC sign-in, role-based access, the audit ledger and its
  API, and scanning all shipped in the open build, and high availability is M13 on the
  open roadmap. The documents still called some of these paid.
- Joining the CNCF is now a goal. The CNCF takes a project whose whole code is under
  Apache 2.0, holds its name, and expects decisions to be made on technical merit. A
  vendor may still sell products around such a project.
- Sign-in behind a paywall is the complaint this audience raises most about the
  alternatives. Having it open is a reason to choose Sindri.

## Decision

**This repository is Apache 2.0 and stays Apache 2.0.** It will not be relicensed, and
no contributor agreement that would permit a relicence will be introduced.

**Nothing that is open today will move behind payment.** That covers every protocol
adapter, both storage backends, OIDC sign-in and role-based access, the audit ledger and
its API, scanning and the severity gate, lifecycle and evidence, and configuration as
code. A feature that ships open stays open.

**Sign-in and high availability are not what Sindri charges for.** OIDC is built in.
SAML and LDAP are no longer reserved as commercial; native support is not planned, and
an OIDC bridge such as Dex or Keycloak is the expected route. M13, several replicas and
the split serve modes, stays on the open roadmap.

**Paid work is additive and clearly separate.** The maintainer intends to make a living
from this, so there will be paid products. They add something new beside the open code
and never take from it. The first candidates serve regulated organisations: advanced
auditing and compliance, for example reports mapped to the EU Cyber Resilience Act,
NIS2, or DORA, retention with guarantees, and signed audit exports. Delivery into
air-gapped networks, managed hosting, and support are others. Sindri keeps the complete
record either way: the ledger, the audit API, evidence, scan results, and exports of
them.

**What is not decided here.** Whether per-CVE waivers, licence policy, organisation-wide
rules, and multi-region replication are open or paid is decided when each is built, in
its own record. So is the form a paid product takes: a separate program, an add-on, or
a hosted service.

**The CNCF Sandbox is a goal, not a commitment.** An application comes once Sindri has
production users, and is decided then, with the funding situation known. A donation
cannot be undone: the name moves to the Linux Foundation, and decisions about
contributions must be made on technical merit alone. Nothing in this record is a promise
to donate. The promises above hold whether or not it happens.

## Consequences

- "Open core" as the pitch drew it no longer describes the project. ARCHITECTURE.md
  section 10, the roadmap's paid layer, and pitch.md section 6 are rewritten with this
  record. ADR 4 and ADR 16 call audit retention a planned commercial feature; that
  remains possible as an addition, and the ledger and its API stay open.
- The public promise is small enough to keep: Apache 2.0, and nothing taken away. A
  paid product can be added later without breaking it.
- Every new feature needs a conscious choice before it ships, because shipping it open
  is permanent.
- A Sandbox application would need work not yet done: sign-off on commits (DCO), a
  governance document, a code of conduct, and a check of the CNCF's current
  requirements. Commercial products should have their own names from the start, so that
  a donation of the name "Sindri" does not affect them.

## Rejected

- **Keep the open core as the pitch drew it.** It charges for sign-in and availability,
  which a small team needs as much as a large one, and the code already ships both.
- **Promise that there will never be a paid tier or an enterprise edition.** An earlier
  draft of this record said so. It is a promise about an unknown future made by someone
  who has to live from the work, and breaking it later would cost more trust than never
  making it.
- **Commit to the CNCF now.** The foundation brings credibility and no funding, and the
  step is irreversible. It is better decided when there are users and the income is
  known.
- **A source-available licence for some directories.** It is the move that produced
  OpenTofu and OpenSearch, and it would end the CNCF option.
- **Say nothing about the business model.** The announcement will be asked how the
  project is funded and whether it will be relicensed. An answer written down before the
  question is asked is more believable than one given afterwards.
