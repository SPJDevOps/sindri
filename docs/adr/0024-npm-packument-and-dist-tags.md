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
Upstream `dist.integrity` is verified when a tarball is cached. (Amended in M9 when the
proxy was built; the paragraphs below say what "as upstream sent them" came to.)

**A proxy asks for abbreviated documents only, and that costs three things.** Sindri sends
`Accept: application/vnd.npm.install-v1+json`, so the cached document has no `time` map and
no readme. `npm view` therefore shows less. `npm install --before` and pnpm's release-age
setting both read `time`, so neither works through a proxy. Fetching full documents would
fix that and make every cached packument several times larger; this is not done. It narrows
"as upstream sent them" to "as upstream sent the abbreviated form".

**The packument is parsed with exact keys and its `name` must match the request.** Go's
struct decoding is case-insensitive and would read a `DIST` that npm ignores. A version that
cannot be served safely (a bad version string or filename, a malformed entry) is left out
with any dist-tag naming it. Two versions claiming one filename are a 502, because choosing
between their hashes would depend on map order. Sindri also reads `dist.integrity` as the
strongest of sha512, sha384, and sha256, falling back to sha1 from the SRI string or
`dist.shasum`; the blob key stays sha256.

**A tarball is fetched only from the upstream's own host and base path.** `dist.tarball` is
author-supplied, and the fetch carries the upstream's credential. The scheme and host are
forced to the upstream's, userinfo and fragment are dropped, and the resolved path must stay
under the upstream's base path on a segment boundary. Dot segments and encoded separators
are refused in decoded or encoded form, to any depth of encoding, rather than cleaned. A
redirect that leaves the first request's origin is refused, and credentials stop at any such
redirect for every protocol: net/http alone kept `Authorization` for the same hostname on
another port. A redirect to the same origin is followed, because registries move tarballs on
their own host, but credentials stop at any hop outside the base path or with an unsafe path.
So an open redirect on a shared host cannot carry them to another tenant's path, and a
tarball that really needs them there fails rather than authenticates.

**A version with neither `integrity` nor `shasum` is listed, and its tarball is a 502.**
Sindri will not cache bytes it cannot check. The same answer covers a tarball URL off the
upstream.

**An uncached proxy stream declares no length.** The hash is known only after the last byte,
so a declared `Content-Length` would frame the response as complete before an integrity
abort could land. Chunked framing makes the abort visible to npm. A placed tarball declares
its length. This refines ADR 14, which still holds for PyPI and OCI: both declare a length
on an unverified stream and rely on the client checking the digest. A placed tarball is served
as it was cached even if upstream later changes that version's hash, and the client then
fails with `EINTEGRITY`.

**In a virtual repository the first member with the package owns its tarballs.** For a
proxy member, "has the package" means its packument resolves, and that is checked before any
placed copy is read. A tarball the package's document does not list, or lists and upstream
lacks, is a 404 when it is not already placed, and a later member is not asked, including
from outside a nested virtual. Otherwise a second proxy could supply a tarball for a
version the first member's document never offered.

`FetchNPMTarball` fetches the packument before it looks for a placed copy, so the check is
of the file as well as the package: a version upstream has dropped is flagged by the refresh
and answers 404, directly and through a virtual (SIN-54, below).

**An upstream 5xx, or a document that fails to parse, fails the walk with a 502.** A 404 or a
negative cache entry is a miss and the walk moves on. A 401/403 is a refusal and fails it
too, below. A stale document still
answers from its own member during an outage, so an outage cannot change which member a
name resolves to, and an outage on a proxy does not touch a claimed name, since the proxy is
not contacted. Only hosted members claim: a proxy's cached `packages` rows look like hosted
rows, and `claimOwner` is what keeps them out. Names through a virtual use `validReadName`,
ASCII only, and a hosted member compares exactly while the claim compares in lowercase. So
`JSONStream` against a hosted `jsonstream` is claimed and then a 404.

**A 401 or 403 from a proxy member fails the walk with a 502.** SIN-55 decided on 2026-11-04
that a refusal fails the walk on all three protocols (ADR 14 has the rule table and the OCI
exceptions): the broken thing is Sindri's own upstream credential, not the package's
presence. The risk it closes is real. With members `[private-proxy, public-proxy]`, a broken
credential on the first would otherwise make a private name resolve from the public proxy for
as long as the credential stays broken. A refusal is never negatively cached, so that would
not have been bounded by the negative TTL either. A stale private document keeps answering
from its own member, as it does in an outage, because a refusal is also unavailable. A claim
by a hosted member still holds, so this affects only names that live upstream on a private
registry.

**An npm packument is bounded at 128 MiB, and each read parses all of it.** PyPI pages and
OCI manifests keep 32 MiB; a packument lists every version and real ones are large
(`renovate` is 36.6 MB, `next` 25.7 MB, measured 2026-11-03). Over the bound is a 502 that
neither poisons the cache nor displaces a good stale copy, and the reason is logged with the
repository and package name, never the URL, headers or body. The cost is that every read,
cached or not, loads and parses the whole document, and there is no request coalescing, so
concurrent cold reads of one package each fetch it.

**A file upstream has removed stops serving at once.** Every refresh that gets an answer
from upstream tells the registry which files the package still lists: a 200 document names
the tarball filenames it lists, and a 404 names none. A placed file the answer does not list
gets `files.upstream_gone_at`, and a file it lists has the flag cleared, so a file upstream
lists again serves again from the next refresh. A flagged file is invisible to the
adapter-facing reads (the tarball and file reads, the file listing) and to the placement
check, so neither a direct read nor a virtual walk can reach it, and that holds for a
version pulled as malicious as much as for any other. `FetchNPMTarball` and the PyPI file
read fetch the document before they serve a placed copy, so a stale entry refreshes on a
lockfile-only install that never reads the packument. Only an answer sets the flag: a 5xx, a
401/403 (`ErrUpstreamDenied` is an outage here) or a document that does not parse says
nothing about the files, and the placed copy still serves. Retention removes a flagged row,
and the version and package rows it empties, once it has been flagged longer than
`worker.retention.proxyStaleAfter`; the collector reclaims the bytes later. Hosted rows are
never flagged. A document whose versions were dropped as unservable (`Dropped`) lists only
the versions the parser kept, so a placed file of a dropped version is flagged, which is the
safe direction. PyPI walks the same way: a proxy member whose index resolves owns the
answer for its project's files, so a file it no longer lists is a 404 and a later member
listing the same filename is not asked. A member whose project answers 404 still moves the
walk on.

Rejected: keeping a placed file serving until retention collects it, as this ADR first
did. It is the more available choice, but it serves a version upstream pulled for a reason,
including malware, for as long as the cache entry is renewed, and a virtual and a direct
read disagreed about one file. The accepted cost is that a pinned install breaks after a
left-pad style unpublish, until upstream lists the file again.

**A hosted package row survives the deletion of its last file.** The claim is the `packages`
row, so deleting a file removes the version when emptied and keeps the package, hidden from
listings and absent on reads. A virtual still answers 404 for the name and does not ask a
proxy. Publishing to it reuses the row. Deleting the repository releases the claim, but
`DeleteRepository` refuses a repository that has promotion events, so such a repository's
claim has no release path today. An accidental publish of `@types/foo` there blocks
`@types/*` through that virtual until its members are restructured. It fails closed. This
covers PyPI as well as npm. Rows lost to deletes made before this fix cannot be recovered,
so such a name stays unclaimed until it is published again.

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

### Checked against installers (SIN-51)

`scripts/npm-tarball-differential.sh` installs each tarball shape the parser accepts from a
local file with the npm on PATH (10.9.9 in the recorded run) and pnpm 10.34.9 and yarn 1.22.22,
which the script fetches at those exact versions. Installs use `--ignore-scripts` in temp
directories. The installed `package.json` is compared with what `inspectManifest` read. The
harness is `internal/proto/npm/tarball_differential_test.go`, behind the `differential`
build tag. Each tarball holds a manifest, placeholder files, and, for most shapes, a
second `package.json` variant (the decoy) in the place a misreading parser would take as
the manifest, with a different name, version and description, so a disagreement shows which
was read. The test fails if a shape's parser outcome differs from the expected one, if a
client errors outside a named allowlist, or if no negative control shows a client
installing the decoy.

The recorded run had 59 candidate shapes (excluding controls): 40 accepted and compared, 19
refused and so skipped. Of the 120 client rows, 119 agree, none disagree, and one is a
client error: yarn 1.22.22 cannot extract a tarball with 7 stray zero bytes after the end
marker ("Unexpected end of data"). Sindri accepts that tarball. yarn reads no manifest from it,
so it is not manifest confusion; it is awaiting its own ticket. Every agree row installed the
name, version and description the parser read, not the decoy's.

| Shape | npm | pnpm | yarn |
|---|---|---|---|
| npm pack, pax path | agree | agree | agree |
| npm pack, ustar prefix | agree | agree | agree |
| tar-stream form | agree | agree | agree |
| npm form with no owner | agree | agree | agree |
| NUL typeflag | agree | agree | agree |
| directories with and without a slash | agree | agree | agree |
| manifest after other files | agree | agree | agree |
| extra zero blocks | agree | agree | agree |
| zero bytes short of a block | agree | agree | client error (awaiting ticket) |
| manifest split over prefix and name | agree | agree | agree |
| manifest named by a pax path over a short name | agree | agree | agree |
| pax with every allowed key | agree | agree | agree |
| pax with no path | agree | agree | agree |
| pax size repeating the header's | agree | agree | agree |
| pax path over a name ending in a slash | agree | agree | agree |
| pax path over a name cut after a parent segment | agree | agree | agree |
| pax path over a name cut inside a character | agree | agree | agree |
| ustar prefix carrying a decoy | agree | agree | agree |
| decoy manifest as a plain second file | agree | agree | agree |
| decoy manifest one directory down | agree | agree | agree |
| decoy manifest two directories down | agree | agree | agree |
| decoy manifest under the NTFS short-name lookalike | agree | agree | agree |
| pax path replacing PaxHeader | agree | agree | agree |
| pax path replacing package/cut-short/ | agree | agree | agree |
| pax path replacing package/cut-short. | agree | agree | agree |
| pax path replacing package/cut-short | agree | agree | agree |
| pax path replacing package/c: | agree | agree | agree |
| pax path replacing package/foo:bar.js | agree | agree | agree |
| pax path replacing package/nested/package.json | agree | agree | agree |
| pax path replacing package/nested/c:/package.json | agree | agree | agree |
| pax path replacing package/c:/nested/package.json | agree | agree | agree |
| pax path replacing //server/share/package/nested/package.json | agree | agree | agree |
| pax path replacing //?/c:/package/nested/package.json | agree | agree | agree |
| pax path replacing ab:/package/nested/package.json | agree | agree | agree |
| pax path replacing package/1:/package.json | agree | agree | agree |
| pax path replacing package/package.jso | agree | agree | agree |
| pax path replacing package/package.json.js | agree | agree | agree |
| pax path replacing package/nested/PACKAG~1.JSO | agree | agree | agree |
| pax path replacing package/PACKAG~1.JSON | agree | agree | agree |
| pax path replacing package/README~1.JSO | agree | agree | agree |

Negative controls check that the comparison can see a disagreement. They are shapes the
parser refuses, because an installer takes the decoy, sent to the clients anyway:

| Negative control (refused by the parser, sent anyway) | npm | pnpm | yarn |
|---|---|---|---|
| second manifest | DISAGREE | DISAGREE | DISAGREE |
| manifest under another top directory | DISAGREE | DISAGREE | DISAGREE |
| manifest in upper case | DISAGREE | agree | DISAGREE |

Not tested: Windows name handling. Drive roots, 8.3 short names, trailing dots and spaces,
UNC roots and backslashes cannot be exercised on this platform, so those refusals rest on
reasoning and Unix reproductions only. The "upper case" control also depends on the
filesystem: this run was on macOS, whose default filesystem folds case.

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
- A name claim now survives deleting a package's last file, as section 4 says "ever
  published" means. A deleted version number is now refused too (SIN-52): deleting a
  hosted file records its version in `retired_files` in the same transaction, and a
  publish checks that table after its exclusive insert, so the insert's wait for an
  in-flight delete makes the tombstone visible. The refusal is `ErrVersionExists`, the
  same 409 as a live version, and it applies to identical bytes as well: this is a
  behaviour change, because republishing a deleted version used to succeed. Versions
  deleted before migration 00009 are not remembered and can still be published again.
  Only hosted npm and PyPI repositories retire anything: a proxy
  and a hosted OCI-protocol repository do not.
- A proxy cannot see `time` or the readme, so `npm install --before` and pnpm's release-age
  setting do not work through one.
- A proxy takes only tarballs on its own host under its base path, with no off-origin
  redirects. Registries that write `%2f` into a tarball URL, or redirect downloads to a CDN,
  get a 502 or are left out. Bearer credentials and private upstreams are SIN-53.
- A refused credential on a proxy member fails the walk with a 502 rather than letting it
  fall to the next member (SIN-55).

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
- **Fetching full packuments from the upstream.** It would restore `time` and the readme at
  several times the cache size for every package, and the abbreviated form is what installs
  use.
- **Declaring a length on a proxied tarball and aborting on a bad hash.** The length frames
  the body as complete, and net/http then computes the end of the response from it, so the
  abort cannot be told from a finished download.
- **Cleaning a `dist.tarball` path.** Normalizing dot segments and encodings is the
  behaviour a hostile author depends on. Refusing the spellings leaves nothing to agree on.
- **Refusing a same-origin redirect outside the base path.** It would break registries that
  move tarballs on their own host. Stopping the credentials closes the same risk.
- **Failing the walk on a refusal for npm only.** Smaller, since npm resolves through its own
  closures, but it would leave the protocols different. SIN-55 chose all three, with Docker
  Hub, quay.io and ghcr.io's refusals of unknown names kept as misses (ADR 14).
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
