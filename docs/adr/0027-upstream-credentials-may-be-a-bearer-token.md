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

**Both are sent only to the upstream's own origin,** exactly as in ADR 14 (whose one
exception is an OCI proxy's username and password reaching its token realm over https). The redirect
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
repository, every OCI request to that upstream fails with an error naming the credential
reference, and the token is sent neither to the registry nor to its token realm.

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

`scripts/migrate-verdaccio-gate.sh` runs a Verdaccio 6.11.1 container (pinned by
digest) that requires a login for `@acme/*`, and a Sindri proxy whose upstream
is that Verdaccio with `token:` set to a token made by `npm token create`.
Verdaccio's own log shows Sindri's reads of the private packument and tarball
under the token's account, and the gate fails if the proxy cannot read the
package. The Bearer path works end to end.

The npm proxy's strict tarball rules (ADR 24) were checked against what
Verdaccio really writes:

- **Scoped packages pass.** Verdaccio's tarball links are
  `/@acme/legacy/-/legacy-1.0.0.tgz`: no `%2f`, a basename ending `.tgz`, and under
  the upstream's base path. None of those rules refused them.
- **One rule does refuse a real configuration: the same-host rule.** A Verdaccio
  that advertises another public URL (`VERDACCIO_PUBLIC_URL`, `url_prefix`) than
  the one Sindri reaches it at writes tarball links on that other host, and the
  proxy answers 502 for each, logging `tarball URL is on another host`.
  Packuments still serve. It is not relaxed: the credential goes with the
  tarball request, and a link that names another host is exactly the hostile
  packument the rule exists for. The migration guide tells the operator to point
  `upstream.url` at the public URL, and the gate covers the refusal.
- **A cross-origin redirect was not exercised.** Verdaccio serves tarballs
  directly. A Verdaccio with an S3 storage plugin that redirects to the bucket
  is unverified, as are GitHub Packages and registries that redirect tarballs
  to a CDN. By the redirect rules above the token is not sent there, so such an
  upstream may need its tarballs served without a redirect.

The same gate also found, outside Sindri, that npm 10.9.9's
`replace-registry-host=always` doubles the repository path in Sindri's own
tarball links, and that a named host must be written without its port. Both are
in the guide.

## Consequences

- `remote.StaticCredentials` and `config.CredentialStore.Lookup` change shape, so out-of-tree
  code implementing `remote.Credentials` must be updated. Sindri has no such callers.
- `reconcile.ValidateOptions.Credentials`, `reconcile.New`, and `api.Options.Credentials`
  carry `map[string]model.CredentialKind` instead of a set of names.
- Existing configuration files are unaffected: `username` and `password` still mean Basic.
