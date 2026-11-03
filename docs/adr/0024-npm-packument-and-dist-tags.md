# 24. The npm packument is derived, dist-tags are pointers, and virtual repositories do not merge

- **Status:** Accepted
- **Date:** 2026-11-02
- **Relates to:** [ADR 13](0013-pypi-write-time-validation-and-immutable-files.md),
  [ADR 14](0014-proxy-cache-and-virtual-resolution.md),
  [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md),
  [ADR 18](0018-authentication-and-rbac-in-v1.md),
  [ADR 19](0019-garbage-collection-roots-write-intent-and-sweep.md),
  roadmap M9

## Context

npm is the third adapter. Its per-package metadata document, the packument, changes on
every publish, and its dist-tags move. OCI and PyPI have nothing like it. The core was also
built around the first two protocols in ways npm breaks: package names fold PEP 503 style,
and download links are root-relative.

Client behaviour below was captured from npm 10.9.9 on Node 22 against a logging server.
pnpm and yarn were not captured; the M9 gate covers them.

## Decision

**The packument is derived from rows on each read, and never stored.** It cannot drift from
what is placed, and `dist.*` is always Sindri's. One response serves both the full and the
abbreviated `Accept` types; npm accepted it for both.

**A version's manifest comes from the tarball, not the request.** At publish Sindri reads
`package/package.json` from the tarball and keeps it in `files.metadata`, with the computed
`integrity` and `shasum`. The publish body's `versions` entry only has to agree on `name`
and `version`; if it disagrees, the publish gets a 400. Every other client field is ignored,
so what the packument says is what the tarball holds, and manifest confusion has nowhere to
start. Sindri parses tarballs with its own strict grammar, which accepts the forms npm pack
and tar-stream write and refuses the rest, because a second parser that disagrees with an
installer is how manifest confusion returns. A filename with a colon, or ending in a dot or a
space, is refused at publish, since Windows reads it differently, and so is any other spelling
of `package.json` at depth one or less, in any case or as the NTFS short name `PACKAG~1.JSO`.

The manifest is stored in jsonb, so publish also refuses nesting past 64 levels, a number
longer than 1000 bytes, and an exponent past ±64. jsonb writes out every digit on read, so
`1e1000` would be small to publish and large in every packument after it.

**Integrity is checked at write time, and client values never reach a reader.** A body
carries one version and one attachment. Sindri hashes the decoded bytes before `cas.Put`. A
declared `dist.integrity` or `dist.shasum` that disagrees is refused and nothing is stored
or placed. A correct one is discarded like any other client value. The blob key stays
`sha256`. The filename is built from name and version, because the client's attachment key
contains a slash (`@acme/ui-1.0.0.tgz`) and its `dist.tarball` is its own guess. The body is
streamed under PyPI's 512 MiB bound.

**A published version is immutable, even for identical bytes.** `InsertFileImmutable`
treats identical bytes as a no-op, which is right for PyPI but not here, so the publish
transaction refuses any version that exists. The answer is a 409 with an `error` field,
which npm prints as `E409`. `registry.Publish` refuses an npm publish without
`ExclusiveVersion`, so no caller can opt out.

**Dist-tags are pointers to versions, outside the identity model.**
`dist_tags(package_id, tag, version_id)` cascades from both parents. A dist-tag is not a
placement and not a GC root: the `files` row already is both, and I2 and GC do not change.
It is not a subject and not a lifecycle event, so promotion (I3) does not see it. The
subject is the tarball's digest, with a new kind `npm-tarball`.

**Tag moves are recorded.** Moving `latest` changes what a plain install gets, so each
change appends to `dist_tag_events` (repository, package, tag, from and to version, actor,
time). The table is append-only by trigger, and holds no foreign key to the package or
version, so it outlives a delete. A package always has a `latest`: the first publish sets
it whatever `--tag` says, `npm dist-tag rm latest` is refused, and if `latest`'s version is
deleted, the packument reports the highest non-prerelease version, or the highest prerelease
if that is all there is, until someone sets a tag. `DeleteRepository` writes no events: its
tags cascade away unrecorded, and the repository's earlier events remain.

**`npm deprecate` writes one field.** The client GETs the packument with `?write=true` and
PUTs the whole document back without `_attachments`, sending `deprecated: ""` to clear. A
PUT with no attachments therefore reads only `versions[*].deprecated`, into a new nullable
`versions.deprecated`. Only `""` clears: a listed version whose field is absent or null stays
as it is, so a client holding a stale document cannot erase a message someone added since. An
unknown version is refused, a message with a control character is a 400, and everything else
in the body, `dist-tags` and `dist` included, is ignored. It is not recorded, because it cannot
change which bytes an install resolves.

**npm names key exactly.** `normalizePackageName` folds `@acme/ui` to `acmeui` and merges
`foo.bar` with `foo_bar`. Those are different npm packages, so one could take over the
other's claim. Normalization becomes per protocol. The npm key is the full name, scope
included. Hosted publishes refuse uppercase. Claim lookups compare in lowercase, so a hosted
`jsonstream` also claims a legacy upstream `JSONStream`; that over-claims in the safe
direction.

**A hosted scope is claimed by its rows.** A hosted repository holding any `@acme/*` name
claims every `@acme/*` name. The name-claim rule in ARCHITECTURE.md section 4 otherwise
applies unchanged, and `claimOwner` and `walkMembers` already have the right shape.

**A virtual repository does not merge packuments.** The first member that has the package
supplies the whole document, as I5 and PyPI's `ListPyPIFiles` already work. A claimed name
never contacts a proxy. Merging is what the roadmap's warning describes, and removing it
removes the bug rather than guarding against it. This overrides the roadmap's "merging
packuments across members".

**Proxy packuments are cached as upstream sent them and rewritten per read.** They live
under the existing `ttl`. Each read rewrites `dist.tarball` to the repository the client
asked for, including a virtual one, so installs stay behind `auth.Authorize` and placement.
Upstream `dist.integrity` is verified when a tarball is cached.

**`dist.tarball` is absolute, from a new top-level `publicUrl`.** ADR 13's root-relative
links do not work for npm. With a clean cache, install failed with `ENOENT` on
`/npm/team/internal/rel/-/rel-1.0.0.tgz`; with a warm cache it succeeded and wrote a
`file:../../..` path into `package-lock.json`. The Host header stays rejected for ADR 13's
reason. `config check` requires `publicUrl` when any npm repository exists, with the https
rule `auth.oidc.redirectUrl` has. It must be the origin in the client's `registry=`: npm
sent no token to a tarball on another host, which Sindri would answer with a 401. A reload
reports that a changed `publicUrl` needs a restart.

**Routes** live under `/npm/{repository}/` and are parsed from `r.URL.EscapedPath()`, since
clients send `@acme%2fui` and a decoded path loses the boundary. `-` is already a reserved
segment and `@` is illegal in a repository name, so neither can occur in one. The leftmost
`-` segment splits the path:

| Path after the repository | Meaning |
|---|---|
| `{name}`, `@scope%2fname`, `@scope/name` | packument GET, publish or deprecate PUT |
| `{name}/-/{file}.tgz`, `@scope/name/-/{file}.tgz` | tarball |
| `-/package/{name}/dist-tags[/{tag}]` | dist-tag GET, PUT, DELETE |
| `-/whoami` | caller's subject |
| `-/v1/login` | constant 404 |
| `-/user/org.couchdb.user:{name}` | login |

**`npm login` verifies a credential, and issues nothing.** Plain `npm login` POSTs to
`-/v1/login`. On a 404 it prompts for a username and password and PUTs them to `-/user/...`,
then stores the `token` from the response as `_authToken`. Sindri takes the password as an
existing OIDC, machine, or bootstrap token, checks it with `Chain.Verify`, and returns that
same token. The username is ignored, as `auth.Credential` already ignores it for Basic. A
failure is a 401, and the answer does not vary with the repository, so it cannot probe for
names.

Neither login route can authorize through `auth.Authorize`, because the client sends no
credential header. They expose no repository data, the credential simply travels in the
body, and each gets a named case in the npm route-walk test. They are the only exception to
security rule 7. The body holds a secret and is never logged (rule 4).

**Migration `00008` is additive.** It widens the `protocol` and `kind` CHECK constraints and
adds the two tables and the column. An older binary ignores all of it, and the rollback path
of running the previous binary does not run Down. Down, if run, keeps repositories, packages,
versions, and files, and restores the old constraints as `NOT VALID`, so it succeeds with npm
rows present. It does drop `dist_tags`, `dist_tag_events`, and `versions.deprecated`.

**The preset stays `npm`.** It is what `presets.go` and ARCHITECTURE.md already say, and it
matches `pypi`. The roadmap's `npmjs` changes.

## Consequences

- A third protocol did need a schema change, and it also needed per-protocol normalization
  in the core. The third adapter is still mostly an adapter, but "cheap" has these limits.
- Sindri gets its first origin setting, used only by npm. `testdata/config/sindri.yaml`
  has to carry it, and a wrong value breaks installs or pins a bad host into lockfiles.
- An OIDC token stored by `npm login` expires with the token, so people re-run login, and
  CI uses machine tokens. Issuing machine tokens stays admin-only.
- A fork of one `@types/*` package in a hosted repository blocks all of `@types/*` through
  any virtual repository containing it. It fails closed with a 404; the fix is a separate
  hosted repository or a new name.
- A scope has no claim until its first publish. Nothing can depend on an internal package
  before it exists, so this is accepted.
- Two proxies in one virtual repository do not combine versions; the first with the package
  wins. A team that needs otherwise can reorder the members.
- `npm view` shows no readme, because the packument omits it.
- The login path carries the username npm prompted for, so a token typed there by mistake
  would reach the access log, which records the path. The password travels in the body and
  stays out of it.
- Tag moves are recorded and OCI tag moves still are not. Bringing OCI in line is separate
  work.
- npm inherits a gap that already exists for PyPI: `Registry.DeleteFile` removes an emptied
  package row, and `PackageExistsInRepo` reads that row, so deleting a package's last file
  releases its name claim, although section 4 says "ever published". It also lets a deleted
  version number be published again. This needs its own fix, and that fix must cover npm.

## Rejected

- **Storing the packument.** A second source of truth, rewritten on every publish, that can
  disagree with placement.
- **Serving the manifest from the publish body.** npm's own manifest-confusion problem: the
  metadata promises one set of dependencies and the tarball holds another.
- **`archive/tar` plus per-shape checks.** Go and node-tar disagree on too many header shapes
  to list: review reproduced more than a dozen where Sindri read one `package.json` and npm
  installed another, and each fix only exposed the next.
- **Reusing `tags` for dist-tags.** It targets a digest, and a row counts as a placement and
  a GC root, so a dist-tag could keep a deleted version's tarball readable.
- **Recording tag moves in `lifecycle_events`.** Its actions are approve and revoke, every
  row needs a stage, and it is keyed by digest. A tag move is none of those.
- **Merging packuments per version.** A hostile `99.0.0` can appear under a name it should
  never reach. ADR 14 already rejected merging, and I5 forbids it.
- **Declaring scopes in config.** It closes the first-publish gap at the cost of a field
  that can drift from what is published.
- **The Host header for `dist.tarball`.** Client input reflected into a document that
  lockfiles then pin.
- **npm's web login.** It needs a pending-login table, an anonymous polling endpoint, and a
  token minted for a user. A minted token cannot carry groups, which ADR 18 says are never
  stored.
- **Minting a token at the login route.** Same objection, and a machine token cannot stand
  in for a person.
- **Treating an identical re-publish as idempotent, as PyPI does.** The roadmap refuses
  it, and a publish also moves a dist-tag, so a silent no-op would leave it unclear whether
  the tag moved.
