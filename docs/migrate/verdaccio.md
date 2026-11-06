# Migrating from Verdaccio

<!-- commands checked by: scripts/migrate-verdaccio-gate.sh -->

You can move a team from Verdaccio to Sindri without a flag day. Sindri proxies
your Verdaccio, hosts new publishes itself, and serves both through one
repository, so clients change one line and Verdaccio can be switched off once
nothing reads it. This page shows each step, and the script that checks it runs
the commands below with the substitutions listed in
[What this was checked against](#what-this-was-checked-against).

The method uses three npm repositories:

- a **proxy**, `verdaccio`, whose upstream is your Verdaccio, so everything you
  have already published stays installable;
- a **hosted** repository, `internal-js`, where new versions are published; and
- a **virtual** repository, `javascript`, over both, which is the only address
  clients use.

The virtual repository lists the hosted repository first, and that order is a
rule, not a preference. A name that hosted has ever held is **claimed**: the
proxy is never asked for it, so a package published to `internal-js` can never
be replaced by one of the same name from Verdaccio or from npmjs. For npm the
claim covers the whole scope, so one published `@acme/*` package claims every
`@acme/*` name. That has a consequence for the order of the steps below, which
[Move the history before you switch anything](#3-move-the-history-before-you-switch-anything)
explains. See [ADR 24](../adr/0024-npm-packument-and-dist-tags.md).

You need a Sindri server with an npm repository's `publicUrl` set (see
[Configuration](../configuration.md)), npm 10 or later, and a Verdaccio 5 or 6
that the Sindri server can reach over HTTP or HTTPS. These steps were checked
with Verdaccio 6.11 and npm 10.9. Examples write the two servers as
`https://verdaccio.example.com` and `https://sindri.example.com`.

## 1. Give Sindri a token on Verdaccio

Sindri reads your packages as a Verdaccio user, so make that user. A dedicated
account such as `sindri-proxy` keeps Sindri's reads apart from people's in
Verdaccio's log. It must be allowed to read every package you want to migrate,
which means it must be named in, or match, each `access:` rule in Verdaccio's
configuration. Sign in as that user with `npm login`, then ask Verdaccio for a
token:

<!-- migrate:token -->
```bash
npm token create --registry https://verdaccio.example.com/
```
<!-- /migrate:token -->

npm prints `Created publish token <token>` after it asks for the account's
password. Keep the token in your secret store and give it to the Sindri server
as `VERDACCIO_TOKEN`. Verdaccio's `npm token create` makes a token that works
as a Bearer token, which is what Sindri sends.

## 2. Declare the repositories

Add this to the Sindri configuration, next to your `listen`, `storage`,
`database`, and `auth` settings:

<!-- migrate:config -->
```yaml
publicUrl: https://sindri.example.com

credentials:
  verdaccio:
    token: ${VERDACCIO_TOKEN}

defaults:
  hosted:  { read: [authenticated], write: [team:platform] }
  proxy:   { read: [authenticated] }
  virtual: { read: [authenticated] }

repos:
  verdaccio:   { protocol: npm, upstream: { url: "https://verdaccio.example.com", ttl: 10m, negativeTtl: 1m, credentials: verdaccio } }
  internal-js: { protocol: npm }
  npm:         { upstream: npm }
  javascript:  { members: [internal-js, verdaccio, npm] }
```
<!-- /migrate:config -->

`token:` makes Sindri send `Authorization: Bearer <token>` to the upstream, and
only to the upstream's own origin: a tarball link in a Verdaccio document that
names another host is refused, and never receives the token. See
[ADR 27](../adr/0027-upstream-credentials-may-be-a-bearer-token.md) and the
[`credentials` reference](../configuration.md#credentials).

The `npm` member is the public registry, last, so a package Verdaccio does not
hold still installs. Leave it out if your Verdaccio is the only source you want.
Replace `team:platform` with the group that publishes.

The `upstream.url` must be the address Verdaccio writes into its tarball links.
Sindri fetches a tarball only from the upstream's own host and under its base
path, because the token goes with the request. A Verdaccio that advertises a
different public URL (its `url_prefix`, or `VERDACCIO_PUBLIC_URL`) than the one
Sindri reaches it at has its packuments served but every tarball refused with a
502, and the log says `tarball URL is on another host`. Point `upstream.url` at
the public one.

## 3. Move the history before you switch anything

Do this before any client reads from Sindri and before the first publish to
`internal-js`. Publishing `@acme/legacy` to `internal-js` claims the whole `@acme`
scope. From then on, through `javascript`, a Verdaccio-only version of
`@acme/legacy`, and every other `@acme/*` package that is not yet in `internal-js`,
is not found. The import below is itself a series of publishes, so the scope is
claimed from the first one, and a client that read `javascript` meanwhile would
find unimported packages missing. Until you switch the clients in step 4 they keep
reading Verdaccio, and nothing looks different to them while this runs.

Importing is a pack from Verdaccio and a publish to Sindri, once per version, for
every package of the scope. The client needs the Verdaccio token for this step
only:

<!-- migrate:import-npmrc -->
```ini
//verdaccio.example.com/:_authToken=${VERDACCIO_TOKEN}
```
<!-- /migrate:import-npmrc -->

First list what there is. This script asks Verdaccio's search for every package of
the scope, then for each one's document (it asks for 250 hits at a time, and goes on by what
comes back, so a server that caps the page lower is still read to the end; it fails if
the count falls short of the total the search reports), and prints one `name version`
line per version, oldest first by semantic-version order, so `1.10.0` follows
`1.2.0`. It uses only Node's own `fetch`. Save it as `inventory.mjs`:

<!-- migrate:inventory -->
```javascript
const url = process.env.VERDACCIO_URL.replace(/\/$/, "");
const scope = process.env.SCOPE;
const headers = { Authorization: `Bearer ${process.env.VERDACCIO_TOKEN}` };

async function get(path) {
  const res = await fetch(url + path, { headers });
  if (!res.ok) throw new Error(`${path}: ${res.status}`);
  return res.json();
}

const split = (v) => {
  const [core, ...pre] = v.split("+")[0].split("-");
  return [core.split(".").map(Number), pre.join("-")];
};
const numeric = (id) => /^\d+$/.test(id);
function comparePre(a, b) {
  if (a === b) return 0;
  if (a === "") return 1;
  if (b === "") return -1;
  const x = a.split("."), y = b.split(".");
  for (let i = 0; i < Math.min(x.length, y.length); i++) {
    if (x[i] === y[i]) continue;
    if (numeric(x[i]) && numeric(y[i])) return x[i] - y[i];
    if (numeric(x[i]) !== numeric(y[i])) return numeric(x[i]) ? -1 : 1;
    return x[i] < y[i] ? -1 : 1;
  }
  return x.length - y.length;
}
function compare(a, b) {
  const [ca, pa] = split(a), [cb, pb] = split(b);
  for (let i = 0; i < 3; i++) if (ca[i] !== cb[i]) return ca[i] - cb[i];
  return comparePre(pa, pb);
}

const size = Number(process.env.PAGE_SIZE || 250);
const found = new Set();
let total;
for (let from = 0; ; ) {
  const page = await get(`/-/v1/search?text=${encodeURIComponent(scope)}&size=${size}&from=${from}`);
  if (typeof page.total === "number") total = page.total;
  const before = found.size;
  for (const hit of page.objects) found.add(hit.package.name);
  // The server may return fewer than asked, so go on by what came back.
  if (page.objects.length === 0 || found.size === before) break;
  from += page.objects.length;
}
if (total !== undefined && found.size < total) {
  throw new Error(`search listed ${found.size} packages but reports ${total}`);
}
const names = [...found].filter((n) => n.startsWith(scope + "/")).sort();
for (const name of names) {
  const doc = await get("/" + name.replace("/", "%2f"));
  if (process.argv[2] === "tags") {
    for (const [tag, version] of Object.entries(doc["dist-tags"])) console.log(name, tag, version);
  } else {
    for (const version of Object.keys(doc.versions).sort(compare)) console.log(name, version);
  }
}
```
<!-- /migrate:inventory -->

Run it once for each scope you own, and read the list before going on:

<!-- migrate:inventory-run -->
```bash
VERDACCIO_URL=https://verdaccio.example.com SCOPE=@acme node inventory.mjs > inventory.txt
VERDACCIO_URL=https://verdaccio.example.com SCOPE=@acme node inventory.mjs tags > tags.txt
```
<!-- /migrate:inventory-run -->

`inventory.txt` holds the versions and `tags.txt` holds Verdaccio's `dist-tags`, one
`name tag version` line each. The account that made the token must be able to read
every package in them; a package it cannot read is not in the list.

The import goes through the list in order. A version already on Sindri is skipped,
so an interrupted import can be run again. Every version is published under a
throwaway tag, `sindri-import`, because `npm publish` otherwise moves `latest` to
whatever was published last, and the history goes in oldest first. When a package
has all its versions in, its real tags are copied from `tags.txt` and the throwaway
is removed. If `sindri-import` is already one of your tags, pick another name in
both places:

<!-- migrate:import -->
```bash
while read -r name version; do
  if [ -n "$(npm view "${name}@${version}" version --registry https://sindri.example.com/npm/internal-js/ </dev/null 2>/dev/null)" ]; then
    continue
  fi
  tarball="$(npm pack "${name}@${version}" --silent --registry https://verdaccio.example.com/ </dev/null)"
  npm publish "./${tarball}" --access public --tag sindri-import \
    --registry https://sindri.example.com/npm/internal-js/ </dev/null
done < inventory.txt
```
<!-- /migrate:import -->

<!-- migrate:import-tags -->
```bash
for name in $(cut -d' ' -f1 inventory.txt | sort -u); do
  npm dist-tag ls "${name}" --registry https://sindri.example.com/npm/internal-js/ </dev/null 2>/dev/null |
    grep -q '^sindri-import:' || continue
  awk -v n="${name}" '$1 == n { print $2, $3 }' tags.txt | while read -r tag version; do
    npm dist-tag add "${name}@${version}" "${tag}" --registry https://sindri.example.com/npm/internal-js/ </dev/null
  done
  npm dist-tag rm "${name}" sindri-import --registry https://sindri.example.com/npm/internal-js/ </dev/null
done
```
<!-- /migrate:import-tags -->

Only a package that still has `sindri-import` is touched, so running this again
does not move a tag you have since set by publishing. Check that the tags match
what Verdaccio had; this prints each package and its tags:

<!-- migrate:verify-tags -->
```bash
for name in $(cut -d' ' -f1 inventory.txt | sort -u); do
  echo "${name}"
  npm dist-tag ls "${name}" --registry https://sindri.example.com/npm/internal-js/ </dev/null
done
```
<!-- /migrate:verify-tags -->

Remove the Verdaccio line from `~/.npmrc` afterwards.

A package first published to Verdaccio after your inventory is not in
`internal-js`. Through `javascript` it is not found, because the scope is
claimed. Run the inventory, the import, and the tag copy again before you switch the
clients: the second run imports only what is new.

What the import does not carry over:

- **Publish times.** An imported version has Sindri's publish time, the moment
  you imported it, not Verdaccio's. That matters to a client with a minimum
  release age, such as yarn 4's `npmMinimalAgeGate`: an imported version counts as
  new until the age has passed.
- **Deprecations.** `npm deprecate` marks live in the document, not in the
  tarball, so a deprecated version arrives not deprecated. Apply them again with
  `npm deprecate "<name>@<version>" "<message>" --registry
  https://sindri.example.com/npm/internal-js/`.
- **Anything that is not in the tarball or a dist-tag:** Verdaccio's access
  rules, its list of owners, and the account that published each version.
  Sindri's own repositories and tokens decide who may read and publish.

## 4. Switch the clients

The history is in Sindri, so clients can change. A client needs the registry, and
a token for it. Put the registry in the
project's `.npmrc`:

<!-- migrate:project-npmrc -->
```ini
registry=https://sindri.example.com/npm/javascript/
replace-registry-host=verdaccio.example.com
```
<!-- /migrate:project-npmrc -->

Put the token in the user's `~/.npmrc`, at path level, as shown. A key for the
host alone (`//sindri.example.com/:_authToken`) is not enough, because Sindri's
repositories live under `/npm/<name>/`. `${SINDRI_TOKEN}` is npm's own
environment reference, so the file holds no secret:

<!-- migrate:user-npmrc -->
```ini
//sindri.example.com/npm/javascript/:_authToken=${SINDRI_TOKEN}
//sindri.example.com/npm/internal-js/:_authToken=${SINDRI_TOKEN}
```
<!-- /migrate:user-npmrc -->

Scopes need nothing more. A scope is the first part of the name, and the
virtual repository resolves `@acme/legacy` as it resolves `left-pad`. A
`@acme:registry=` line that points a scope at Verdaccio must be removed, or that
scope bypasses Sindri. The Verdaccio token line goes too, once you have done
the import in step 3: a client that has no Verdaccio credential cannot go around
Sindri to reach it.

The second line of the project file is the lockfile setting. The next section
explains it.

## 5. Lockfiles keep the old host

`package-lock.json` records the URL each tarball was fetched from, in
`resolved`. For the public registry npm rewrites that URL to whatever
`registry` is set to. For any other host, and a Verdaccio is another host, it
keeps the old URL: `npm ci` goes straight to Verdaccio, past Sindri, and works
only because your clients can still reach it. A client with no Verdaccio
credential fails instead, with a 401 from Verdaccio.

Two fixes work with npm 10.9, and they are meant to be used together:

- `replace-registry-host=<host>`, as in the project file above, makes npm
  rewrite the tarball links on that one host to the configured registry at
  install time. Write the hostname alone, without a port: npm 10.9
  compares it with the link's hostname, so `verdaccio.example.com:4873` matches
  nothing and the install still goes to Verdaccio. The existing lockfile then installs through Sindri with the same versions and
  integrity hashes. The file itself is not changed, and still names Verdaccio.
- Regenerating the lockfile writes Sindri's URLs into it. npm resolves again
  when it has no lockfile, so versions may move within their ranges:

<!-- migrate:lockfile -->
```bash
rm package-lock.json
npm install
```
<!-- /migrate:lockfile -->

Use the first to cut over today, and the second as you next update each
project's dependencies; then drop the setting. `npm ci` is the install command
the first fix is for:

<!-- migrate:install -->
```bash
npm ci
```
<!-- /migrate:install -->

Do not use `replace-registry-host=always`, which the npm documentation offers
for this. It also rewrites the links Sindri writes, and because Sindri's
repositories live under `/npm/<name>/`, npm doubles that path
(`/npm/javascript/npm/javascript/...`) and the install fails with a 404 for
any version npm resolves fresh, such as one you publish after the switch.

## 6. Publish new versions to Sindri

Publishing goes to the hosted repository, not the virtual one. Point the
package's own `.npmrc` at it:

<!-- migrate:publish-npmrc -->
```ini
registry=https://sindri.example.com/npm/internal-js/
```
<!-- /migrate:publish-npmrc -->

<!-- migrate:publish -->
```bash
npm publish --access public
```
<!-- /migrate:publish -->

Clients that install through `javascript` see the new version beside the
history, because both are in `internal-js`.

## 7. Switch Verdaccio off

When nothing reads Verdaccio but Sindri, stop publishing to it, remove
`verdaccio` from the `javascript` members, and then stop Verdaccio. Do not
remove the proxy before the history is imported (step 3): until a package is
imported, the proxy is what keeps its versions installable. Once it is gone, regenerate any lockfile that still names
Verdaccio, and drop `replace-registry-host`.

## What this was checked against

The commands above run against a real Verdaccio 6.11.1 container that requires a
login for `@acme/*`, with npm 10.9.9 on Node 22. Sindri authenticates to it with
a `token:` credential made by `npm token create`; the check fails if Sindri
cannot read Verdaccio's private package.

- Scoped packages (`@acme/legacy`) work with Verdaccio's tarball links, which
  are `/@acme/legacy/-/legacy-1.0.0.tgz` and so pass the proxy's strict tarball
  rules.
- A client with no Verdaccio credential installs the lockfile's tarball through
  Sindri, and Verdaccio's log shows the tarball was read by the Sindri account.
- A hosted name is never served from the Verdaccio proxy, even when the proxy is
  listed first.

Not checked here: a Verdaccio that stores tarballs in S3 and redirects to them,
GitHub Packages, and pnpm and yarn against this layout. A redirect that leaves
the upstream's origin loses the token by design.

The check runs the page's configuration, client files, and commands out of the
page itself, with these substitutions: the example hosts `verdaccio.example.com`
and `sindri.example.com` become the hosts the check runs on, and
`replace-registry-host` names Verdaccio's host without a port. Two commands are
changed. The lockfile command's `rm package-lock.json` becomes `rm -f`, because
that copy of the project has no lockfile left to remove. The check's own
publishes to Verdaccio and its comparisons of tags are its own commands, and not
the page's.

The history import runs with two or more versions published out of order, a
version that sorts differently as text and as a number (`1.10.0` against
`1.2.0`), and a prerelease. After the import, the check compares every package's
dist-tags on Sindri with Verdaccio's, and asserts that `sindri-import` is gone and
that a deprecation was not carried over.
