# Migrating from Verdaccio

<!-- commands checked by: scripts/migrate-verdaccio-gate.sh -->

You can move a team from Verdaccio to Sindri without a flag day. Sindri proxies
your Verdaccio, hosts new publishes itself, and serves both through one
repository, so clients change one line and Verdaccio can be switched off once
nothing reads it. This page shows each step, and the script that checks it runs
the commands below as written.

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
[Move the history before you publish](#5-move-the-history-before-you-publish)
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

## 3. Switch the clients

A client needs the registry, and a token for it. Put the registry in the
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
step 5: a client that has no Verdaccio credential cannot go around Sindri to
reach it.

The second line of the project file is the lockfile setting. The next section
explains it.

## 4. Lockfiles keep the old host

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

## 5. Move the history before you publish

Publishing `@acme/legacy` to `internal-js` claims the whole `@acme` scope. From
then on, through `javascript`, a Verdaccio-only version of `@acme/legacy`, and
every other `@acme/*` package that is not yet in `internal-js`, is not found. So
import what you keep, for every package of a scope, before the first publish
to it, or at least before clients upgrade.

Importing is a pack from Verdaccio and a publish to Sindri, once per version.
The client needs the Verdaccio token for this step only:

<!-- migrate:import-npmrc -->
```ini
//verdaccio.example.com/:_authToken=${VERDACCIO_TOKEN}
```
<!-- /migrate:import-npmrc -->

<!-- migrate:import -->
```bash
npm pack @acme/legacy@1.0.0 --registry https://verdaccio.example.com/
npm publish ./acme-legacy-1.0.0.tgz --access public --registry https://sindri.example.com/npm/internal-js/
```
<!-- /migrate:import -->

Remove the Verdaccio line from `~/.npmrc` afterwards. Each imported version has
Sindri's publish time, which matters to a client with a minimum release age,
such as yarn 4's `npmMinimalAgeGate`.

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
remove the proxy before the history is imported: the proxy is what keeps those
versions installable. Once it is gone, regenerate any lockfile that still names
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
