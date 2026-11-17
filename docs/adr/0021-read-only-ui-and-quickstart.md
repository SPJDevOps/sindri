# 21. A read-only UI, and a quickstart with a development identity provider

- **Status:** Accepted
- **Date:** 2026-10-12
- **Relates to:** [ADR 2](0002-go-single-binary.md),
  [ADR 18](0018-authentication-and-rbac-in-v1.md), roadmap M8b

## Context

v1 promises a read-only web UI: repositories, packages, and artifact detail
with digest, owner, provenance, evidence, and per-environment lifecycle status.
The stack was already decided: Go templates and htmx, embedded, because a Node
build would break the single-binary promise. Four questions were left open:

- how pages address repositories whose names contain slashes
- what stops a page from leaking another team's data
- how a browser signs in when no identity provider is configured
- how a stranger gets a working UI in the quickstart, given that v1 has no
  anonymous access

## Decision

**Server-rendered pages from the API's services.** `internal/ui` renders with
`html/template` from `registry`, `lifecycle`, and `evidence`, at the same layer
as `api`. htmx is used only for "load more". `web/` embeds the templates, CSS,
htmx, and fonts, so nothing loads from a CDN and the UI works air-gapped.
`web/VENDOR.md` records each vendored file's checksum, and a test enforces it.

**Routing.** Repository names contain slashes, so a page within a repository
follows a `/-/` separator:

- `/ui/r/team/app`
- `/ui/r/team/app/-/package?name=web`
- `/ui/r/team/app/-/artifact?digest=…`

A lone `-` is now a reserved name segment, so the split is never ambiguous.
Assets are served under a hash of their contents, `/ui/static/{hash}/…`, and
cached as immutable.

**Authorization is the API's, page by page.**

- Every repository shown passes `auth.Authorize`.
- A repository the caller cannot read gets the same 404 as one that does not
  exist.
- A virtual repository lists only members the caller can read.
- An artifact page is scoped to a repository and checks placement first
  (invariant I2).
- Owners and lifecycle state from other repositories are shown only when the
  caller can read those repositories.
- Evidence appears as metadata only. Its documents stay behind the API, which
  authorizes each subject.

A test walks the router, so a route added without an authorization case
fails it.

**A strict Content-Security-Policy.** The policy is `default-src 'none'`,
with `'self'` for script, style, font, and connect. Forms, framing, and base
URLs are not allowed. A test fails any template with inline script, inline
style, or an event-handler attribute, so nothing tempts a later change to
loosen the policy.

*Amended 2026-11-17 (SIN-98).* The "sign-in is not configured" page is the one
exception. `internal/auth` serves it before anyone has signed in, and the UI's
assets sit behind sign-in, so it cannot load `sindri.css`. It carries a single
inline stylesheet, admitted by its sha256 hash in that page's own policy
(`default-src 'none'`, no script, no forms, no framing), and a test recomputes
the hash. The alternative, an unauthenticated stylesheet route, was rejected:
it would be an anonymous path, which ADR 18 refuses.

*Amended 2026-11-17 (SIN-96).* The UI policy's `form-action` is now `'self'`
instead of `'none'`, so the repository list's filter can be a plain
`<form method="get" action="/ui/">`. Every form in a template must be a GET
with the method written out, and a test fails any that is not. This is safe
because the UI still has no write path: the session cookie is honoured only on
GET and HEAD, so a form that posted would be answered 405 and do nothing, and
a GET form can only reach the same read-only pages a link reaches. The sign-in
help page's policy in `internal/auth` stays at `form-action 'none'`. The
alternative, links and htmx only, was rejected: the text box would not work
without JavaScript, and the filter state would not be a URL a browser can
submit by itself. Global search (SIN-101) reuses the same form-and-GET
pattern and needs no further change to the policy.

**Sign-in.**

- Browsers sign in through ADR 18's authorization code flow. The session
  cookie is honoured only on GET and HEAD, and the UI's only forms are GET forms
  (the filter), which cannot change anything.
- Sign-out is an htmx POST to `/auth/logout`, which answers with
  `HX-Redirect`.
- Without `auth.oidc`, the guard answers a browser with a page explaining how
  to configure it. It never sends a Basic challenge: a browser caches Basic
  credentials and attaches them to cross-site requests, and every write
  route accepts Basic, because that is what docker, pip, and twine send.
- `GET /` redirects to `/ui/`.

**The quickstart runs Dex.** `deploy/quickstart/compose.yaml` runs Postgres,
Dex, and Sindri:

- Dex's mock connector signs in one example user in the `authors` group, and
  `authors` administer the server.
- Sindri shares Dex's network namespace, so `http://localhost:5556` is the
  issuer for both the browser and the server. Sindri needs no configuration
  field for that.
- `docker login` uses the bootstrap token from `SINDRI_TOKEN`.
- The file says, in capitals, that it is for development only.

**Release.**

- goreleaser builds linux and darwin binaries for amd64 and arm64.
- It also builds a multi-architecture image on pinned distroless
  `static:nonroot`, with no shell and user 65532. The image copies binaries
  goreleaser already built and has no RUN step.
- `sindri probe URL` exists so Compose and Kubernetes have a healthcheck in an
  image without curl.

The gate runs `docs/quickstart.md`, read from the document itself, in a fresh
`docker:dind` daemon.

## Consequences

- The UI adds about 220 KB of assets to the binary, mostly fonts.
- Every page costs a few queries per repository it shows. A registry has tens
  or hundreds of repositories, so the home page filters and groups them by
  namespace, pages them 200 at a time, and reads their figures (packages,
  last publish) in one aggregate query for the page, never one per row. Other
  listings page by 50.
- Signing out from the UI ends the Sindri session but not the identity
  provider's own. A provider with a live session signs the user straight back
  in, which is normal for OIDC relying parties.
- The quickstart's Compose file and the document can never drift apart: the
  gate diffs them.

## Rejected

- **A single-page app.** It would need a Node build, which the single-binary
  rule forbids, and a second rendering of authorization decisions the server
  already makes.
- **Basic-auth fallback in the browser when OIDC is absent.** This is the
  CSRF hole described above. An operator without a provider uses the API with
  a token.
- **An anonymous read-only UI.** ADR 18 refuses anonymous access everywhere,
  and a UI-only exception would be the one place a misconfiguration exposes
  names.
- **`issuerUrl` and `discoveryUrl` configuration** to let Sindri reach Dex
  under a different host than the browser does. Sharing the network
  namespace solves the quickstart without a configuration field that only
  the quickstart needs.
- **Google Fonts or a CDN for htmx.** That would add a third-party origin to
  the policy, and a request leaving every viewer's browser.
- **Serving evidence documents from the UI.** They can be large and hold
  sensitive detail. The API already authorizes them per subject.
