# 27. Upstream credentials may be a Bearer token

- **Status:** Accepted
- **Amends:** [ADR 15](0015-upstream-credentials-source-deferred-to-m7.md)
- **Date:** 2026-11-06
- **Relates to:** [ADR 14](0014-proxy-cache-and-virtual-resolution.md),
  [ADR 24](0024-npm-packument-and-dist-tags.md), roadmap M10 (SIN-53)

## Context

ADR 15 gave every credential a username and a password, and the proxy sent them as
HTTP Basic. A private npm registry such as Verdaccio, or a hosted one, commonly issues a
token that the client sends as `Authorization: Bearer <token>`. With Basic only, a team
moving from such a registry could not point a Sindri proxy at it, and the migration guide
had nothing to tell them.

## Decision

**A credential is either `username` and `password`, or `token`.** Setting both, or
neither, is a validation error when the configuration loads. `token` is redacted by
`config show` like the other secrets. Loading keeps the one shape ADR 7 asks for: there
is no second spelling.

```yaml
credentials:
  npm-private:
    token: ${NPM_TOKEN}
```

**A token is sent as `Authorization: Bearer <token>`, and a password as Basic,** from the
same place in `internal/remote`. `remote.Credentials.Lookup` returns a typed
`remote.Credential` instead of a username and password, so no caller can ignore the kind.

**Both are sent only to the upstream's own origin,** exactly as in ADR 14. The redirect
rules that strip `Authorization` when a redirect leaves the origin already cover a Bearer
header, and a test now proves it. An upstream document that names another host (an npm
tarball URL) never receives the token.

**A token credential on an OCI proxy is refused when the repository is written.** An OCI
registry authenticates a client by exchanging Basic credentials at its token realm for a
short-lived Bearer token. A static token has no defined meaning in that exchange, and
sending one to a realm would be guessing. The error names the credential and says OCI
needs username and password. To enforce this, `Credentials.Names()` becomes a map from name
to kind (`basic` or `token`). A kind is not a secret, so the rule that the reconciler sees
no secrets still holds. If a reload changes a credential's kind under a stored OCI
repository, the token request fails and the token is not sent.

## Rejected

- **A `scheme:` field** (`scheme: bearer` beside `password`). It lets an operator write a
  Bearer token into a field called `password`, and makes `basic` the default that most
  people never write. `token` says what the value is, and "exactly one of" is checkable.
- **An npm-only token,** a different section or a protocol flag. Credentials are looked up
  by name and sent by one function; a per-protocol token would fork that, and PyPI
  indexes behind a token proxy would need the same thing again. OCI is the one protocol
  where the kind does not apply, and that is a validation rule, not a second mechanism.
- **Sending a token to an OCI token realm as the password.** Registries differ on whether
  that works, and a silent success on one and a 401 on another is worse than a clear
  refusal.

## Tried against a real registry

Filled in by the Verdaccio migration gate. It runs a Verdaccio proxy with a `token:`
credential end to end and records here which of the strict tarball rules, if any, refused
something real. GitHub Packages and registries that redirect tarballs to a CDN are
unverified.

## Consequences

- `remote.StaticCredentials` and `config.CredentialStore.Lookup` change shape, so out-of-tree
  code implementing `remote.Credentials` must be updated. Sindri has no such callers.
- `reconcile.ValidateOptions.Credentials`, `reconcile.New`, and `api.Options.Credentials`
  carry `map[string]model.CredentialKind` instead of a set of names.
- Existing configuration files are unaffected: `username` and `password` still mean Basic.
