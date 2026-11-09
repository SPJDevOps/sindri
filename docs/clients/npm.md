# Using Sindri as an npm registry

<!-- commands checked by: scripts/npm-gate.sh -->

Sindri serves npm at `/npm/{repo}/`, for publishing and installing, with npm, pnpm, and
yarn. Declare the repositories in the configuration file. npm packuments carry absolute
tarball URLs, so a server with an npm repository must be told its own address with
`publicUrl`, and refuses to load without it:

```yaml
publicUrl: http://localhost:8080   # the origin your clients use

repos:
  internal-js: { protocol: npm }                       # hosted
  npm:         { upstream: npm }                       # proxy of registry.npmjs.org
  javascript:  { members: [internal-js, npm] }         # virtual
```

Point a project at a repository with a `.npmrc`, and keep the token out of the project:
put it in your user-level `~/.npmrc`, under the repository's own path. A host-level key
makes `npm publish` fail with `ENEEDAUTH`, and pnpm refuses to expand `${SINDRI_TOKEN}`
in a credential from a project `.npmrc`. To publish to `internal-js`, the project's
`.npmrc` names the registry:

```ini
registry=http://localhost:8080/npm/internal-js/
```

and `~/.npmrc` carries the token:

```ini
//localhost:8080/npm/internal-js/:_authToken=${SINDRI_TOKEN}
```

```bash
npm publish --access public
```

To install through the virtual repository, name it instead, and add its token line:

```ini
registry=http://localhost:8080/npm/javascript/
//localhost:8080/npm/javascript/:_authToken=${SINDRI_TOKEN}
```

```bash
npm install
```

`SINDRI_TOKEN` is the bootstrap token, or any token with the right role; the real
deployment uses an identity provider's ID token or a machine token, the same as for pip.
pnpm reads the same `.npmrc`. `pnpm import` turns npm's `package-lock.json` into a
lockfile, and `pnpm install --frozen-lockfile` installs from it. Yarn 1 reads it too:

```bash
pnpm import
pnpm install --frozen-lockfile
yarn import
yarn install --frozen-lockfile
```

Yarn 4 reads `.yarnrc.yml` instead, and allows plain `http` only for hosts you list:

```yaml
npmRegistryServer: "http://localhost:8080/npm/javascript/"
unsafeHttpWhitelist:
  - localhost
npmAlwaysAuth: true
npmAuthToken: "${SINDRI_TOKEN}"
```

Yarn 4 keeps its default age gate (`npmMinimalAgeGate`, 1440 minutes): it holds back a
version published less than a day ago, judging by the `time` map in the packument. Sindri
serves that map, with each hosted version's real publish time and, through a proxy,
npmjs's own, so the gate works as it does against npmjs and needs no setting.

If your team publishes an internal package and installs it within a day, list your scope
as pre-approved. Yarn then skips the age gate for those packages only, and keeps it for
everything else:

```yaml
npmPreapprovedPackages:
  - "@acme/*"
```

Or run [`scripts/npm-gate.sh`](https://github.com/SPJDevOps/sindri/blob/main/scripts/npm-gate.sh), which does all of the above against
the Compose stack.

Worth knowing before you point real work at it:

- **Every request authenticates**, packuments and tarballs included.
  Publishing needs the write role on the repository.
- **Use one virtual repository, not two registries.** A virtual repository listing your
  hosted repository before the npmjs proxy never serves a name you publish from the proxy,
  whatever version the proxy offers, and a hosted `@scope/name` claims the whole scope.
  That is the dependency-confusion defence of invariant I5. See
  [ADR 24](../adr/0024-npm-packument-and-dist-tags.md).
- **A published version is immutable.** Publishing the same version again is refused, and
  so is publishing a version you deleted, even with identical bytes. Bump the version.
- **Files upstream drops disappear.** Once npmjs stops listing a file, a proxy answers 404
  for it at once, in a lockfile install too. It serves again if npmjs relists it, and the
  row is removed after `worker.retention.proxyStaleAfter`. OCI is outside this: a digest
  manifest and its layer blobs keep serving from placement after upstream drops them. A
  whole-package 404 moves a virtual repository's walk on, so with `[private-proxy, npm]` a
  package removed from the private upstream can resolve from the public proxy; only a hosted
  claim protects a private name.
- **An upstream that refuses Sindri's credential is a 502**, not a 404, and fails a
  virtual repository's walk; a cached copy still serves. See
  [upgrading](../upgrading.md).
