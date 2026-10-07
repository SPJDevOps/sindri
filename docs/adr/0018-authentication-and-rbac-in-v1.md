# 18. Authentication and RBAC in v1

- **Status:** Accepted
- **Date:** 2026-10-02
- **Relates to:** [ADR 5](0005-configuration-as-code.md), [ADR 10](0010-oci-hosted-adapter-placement.md),
  [ADR 12](0012-pypi-hosted-adapter.md), [ADR 15](0015-upstream-credentials-source-deferred-to-m7.md),
  roadmap M7
- **Supersedes:** the anonymous `/v2` of ADR 10 and the anonymous index of ADR 12

## Context

Until M7 only `/api/v1` checked anything, and it checked one shared token.
`/v2` and `/pypi` were anonymous. M7 must deliver OIDC, group-to-role mapping,
RBAC on every route, and machine tokens. Two constraints shape the design:

- **docker, pip, and twine are not browsers.** They speak HTTP Basic, sent
  after a challenge. The authorization code flow alone cannot serve them.
- **Membership must be live (ADR 5).** A group change at the identity provider
  must take effect without anything in Sindri being re-applied, and must never
  be cached in the database.

## Decision

**One credential channel.** Every credential arrives as a Bearer token or as the
password of HTTP Basic. The username is ignored. The credential's shape picks
its verifier:

| Credential | Shape | Verified against |
|---|---|---|
| Machine token | `sindri_` + 32 random bytes | SHA-256 lookup in `machine_tokens` |
| Identity-provider token | a JWT | the provider's JWKS: signature, issuer, audience (client id), expiry |
| Bootstrap token | anything else | `auth.static_token`, constant-time compare |

**People use an ID token as the password.** `docker login -u me -p <id token>`
and an index URL carrying the token are how humans authenticate CLI clients.
Groups and the username come from configurable claims of the token presented
with each request. Membership is therefore live to within one token's lifetime,
and the database holds no group data at all. The integration test changes a
group at a stub provider and sees access granted and withdrawn with no
database write. Administrators are a provider group (`auth.admins`).

**Machine tokens for CI** are scoped to one repository, one namespace, or
(admin only) the server, with one role: `read`, `write`, or `admin`. Only a
hash is stored. The token is shown once, and revoking it keeps the row so the
ledger's `token:<name>` actors stay meaningful.

**The bootstrap token** stays as the administrator credential before a
provider exists and the break-glass path after. Running with the development
default logs a warning.

**No anonymous access in v1.** Only `/healthz`, `/readyz`, `/metrics`, and
`/auth/*` are open. That makes ADR 15's rule hold by construction: a proxy
holding credentials is never anonymously readable. A configuration that grants
`anonymous` or `*` is refused at write time. `authenticated` is the widest
grant.

**One authorization function.** `auth.Authorize(principal, repository, role)`
is pure and reads only the repository row, which now carries its access lists.
The download path stays a lookup. Write implies read. Admin implies
everything. Pushing to an unknown OCI name creates a repository and therefore
needs admin. A caller who may not read a repository is told it does not exist,
so names are not disclosed. A caller who may read but not write gets 403. Each
surface keeps its own error shape: OCI `UNAUTHORIZED` and `DENIED`, PyPI plain
text, management API `FORBIDDEN`.

**Virtual repositories answer only from readable members.** Read on a virtual
repository lets a caller address it. Each member that answers must be readable
too, or a team could republish another team's private repository by wrapping
it in a virtual one. A name claimed by an unreadable hosted member is a miss.
It never falls through to a proxy, so invariant I5 is unaffected.

**Browsers** use the authorization code flow with PKCE, state, and nonce at
`/auth/login`. The verified ID token becomes the `sindri_session` cookie:
HttpOnly, SameSite=Lax, and Secure over https. Plain http is accepted only on
loopback. The cookie is honoured only on GET and HEAD, and only by the
read-only UI guard and `/auth/whoami`. The API and the adapters never read it,
so a cross-site request cannot borrow a session to change anything.

**Provider outages do not take the registry down.** Discovery is lazy and
retried with backoff. Until it succeeds, provider tokens are refused, and
machine and bootstrap tokens keep working.

## Consequences

- `pip install` and `docker pull` need credentials. The gate scripts show the
  spelling: `docker login`, and `https://token:<secret>@host/pypi/<repo>/simple/`.
- A revoked group membership stops working when the user's current token
  expires. Short-lived ID tokens are the operator's lever, and the ADR states
  the bound honestly rather than claiming immediacy.
- API-created repositories and repositories from before M7 grant nothing
  until access is added, so only administrators reach them. That fails
  closed.
- Subjects are authorized through their owning repositories: reading
  evidence needs read on a repository that published the digest.

## Rejected

- **Groups from the userinfo endpoint on every request.** Truly live, but
  every pull would depend on the provider's availability and latency, and the
  download path would stop being a lookup.
- **Personal access tokens minted at browser login.** The token would carry
  the groups of the moment it was minted, which is cached membership under
  another name (ADR 5).
- **The Docker token-service protocol (`/v2/token`).** It adds a second
  credential exchange and a signing key to operate, for no gain over Basic on
  a TLS-terminated endpoint.
- **An `anonymous` grant.** Useful for public mirrors, but it needs a
  validator rule to keep credentialed proxies private and a test per adapter.
  It can be added later without a format change.
- **Session cookies on the API.** That would need CSRF tokens on every
  mutating route. Bearer tokens for scripts and cookies only for the read-only
  UI avoid the problem entirely.
