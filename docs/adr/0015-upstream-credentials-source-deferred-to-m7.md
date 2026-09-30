# 15. Upstream credentials: source decided, wiring deferred to M7

- **Status:** Accepted
- **Date:** 2026-09-30
- **Relates to:** [ADR 7](0007-config-surface-short-and-long-form.md),
  [ADR 9](0009-one-configuration-loader-from-m1.md),
  [ADR 14](0014-proxy-cache-and-virtual-resolution.md), roadmap M5 and M7

## Context

A proxy repository may need credentials for its upstream: a private registry, a
paid mirror, or an authenticated Docker Hub account to lift rate limits. M5
stores a `remotes.credentials_ref` and applies credentials only to the
upstream's own origin (ADR 14), behind a `remote.Credentials` interface. But
nothing supplies secrets to that interface in the running binary. A reference
was accepted when the repository was created and then failed every pull.

Two things make completing this in M5 a mistake:

1. **Reads are anonymous until M7.** A proxy holding credentials for a private
   upstream would serve that upstream's content to anyone who can reach Sindri.
   Credentialed proxies are only safe once RBAC can restrict who reads them.
2. **M7 designs the configuration that references credentials.** Credentials
   are named from repository definitions, which arrive with `repos:`. Designing
   the secret section alone now risks a shape the short form must then work
   around, which is the "two syntaxes" failure ADR 7 exists to prevent.

## Decision

**Source: a `credentials:` section of `sindri.yaml`,** in the one configuration
file and its one loader (ADR 9). Values use the loader's existing `${VAR}`
substitution, so the secret itself lives in the environment or a mounted
secret, never in the file:

```yaml
credentials:
  fancysecret:
    username: ${FANCY_USER}
    password: ${FANCY_TOKEN}
```

A repository names an entry by key, as `credentials_ref` does today. The
config-backed implementation of `remote.Credentials` is built from this section
and passed to `remote.New` in `serve`.

**Rules that hold whatever M7 adds:**

- A secret is never written to Postgres. `remotes` holds only the reference.
- A secret is never logged, and `sindri config show --expanded` redacts it.
- Credentials are sent only to the scheme and host of `upstream_url`, and to a
  bearer-token realm only over https or on that same origin (ADR 14).
- A reference that names no entry is a validation error when the repository is
  created or reconciled, never a failure at pull time.
- A proxy with credentials must not be anonymously readable. M7 decides whether
  that is a validation rule or a default role binding, but it is not optional.

**Until M7:** `reconcile.ValidateRepository` rejects any `credentials_ref` with
`ErrCredentialsUnsupported`, and the management API returns 400 naming this
ADR. M7 replaces the blanket rejection with a lookup against `credentials:`.
Public upstreams (Docker Hub anonymous, quay.io, ghcr.io public images,
pypi.org) are unaffected.

## Consequences

- M5 ships proxies for public upstreams only. Its roadmap item says so.
- The credential-scoping behaviour stays under test
  (`TestProxyPyPI_CredentialsStayOnUpstreamOrigin` creates its repository below
  the API), so M7 builds on verified code rather than rewriting it.
- M7's scope gains the `credentials:` section, the config-backed
  `remote.Credentials`, reference validation, and the read-access rule above.

## Rejected

- **Wire it in M5 with the `credentials:` section.** Works mechanically, but
  republishes private upstreams to anonymous readers until M7, and fixes a
  configuration shape before the rest of the config surface is designed.
- **Environment variables by naming convention**
  (`SINDRI_CRED_FANCYSECRET_PASSWORD`). Needs no schema, but it is a second,
  implicit configuration channel that `config check` cannot see and
  `config show` cannot print, which CLAUDE.md rules out.
- **Secrets in the `remotes` table.** Puts credentials in every database
  backup and every read replica; ADR 14 already rejected it.
- **External secret managers** (Vault, cloud KMS). Out of scope for v1; the
  `remote.Credentials` interface is where one would plug in later.
- **Leave `credentials_ref` accepted and fix it in M7.** An accepted setting
  that silently breaks every pull is worse than a clear refusal.
