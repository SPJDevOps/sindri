# Upgrading Sindri

An upgrade is replacing one binary or one image. There is no separate migrate
step and no upgrade tool.

## What happens on startup

`sindri serve` applies every outstanding database migration before it accepts a
request. The migrations are embedded in the binary, so the schema a binary
needs always ships with it. They run under a Postgres advisory lock: when
several instances start at once, one migrates and the others wait, so a rolling
restart is safe.

The blob store needs nothing. Blobs are content-addressed bytes whose layout
does not change between releases.

## Upgrading

### Default listen security change

Sindri now defaults to a loopback-only listener (`127.0.0.1:8080`) for local development.
If you run with the built-in development bootstrap token, the server refuses to start on a
public address such as `:8080`, `0.0.0.0:8080`, or `[::]:8080`. This is a safety check,
not a change in how a configured production listener works: operators who want a network
listener must set their own static token or use a stronger identity-provider setup.

1. Back up Postgres. Migrations are written to be safe, but a backup is what
   makes any mistake recoverable.
2. Replace the binary, or change the image tag:

   ```bash
   # Compose quickstart
   SINDRI_IMAGE=ghcr.io/spjdevops/sindri:<version> docker compose up -d

   # Binary
   tar -xzf sindri_<version>_linux_amd64.tar.gz sindri
   install -m 0755 sindri /usr/local/bin/sindri
   systemctl restart sindri
   ```

3. Check the log for `database schema is up to date`. Its `applied` field
   counts the migrations this start ran.

Upgrade one release at a time. Each release's migrations are only guaranteed
to be compatible with the release before it.

## Behaviour changes in 1.1

Read these before upgrading a server that other people depend on.

- **An upstream that refuses Sindri's credential is a 502.** A proxy whose
  upstream answers 401 or 403 now gives 502 on every protocol, and fails a
  virtual repository's walk instead of being skipped. A copy already cached
  still serves. On OCI, only an anonymous proxy reads a bearer-carrying
  401/403, or a refusal from an anonymous token endpoint, as "no such name". A
  proxy with credentials always fails the walk. In a virtual repository such as
  `[hosted, dockerhub-with-login, ghcr]`, an image that exists only on ghcr now
  gets a 502 unless the logged-in proxy is last, or runs anonymously. See
  [ADR 14](adr/0014-proxy-cache-and-virtual-resolution.md).
- **A deleted file's name stays taken.** Deleting an npm version or a PyPI
  filename from a hosted repository means it cannot be published again, even
  with identical bytes: the publish answers 409. Publish a new version. Hosted
  OCI-protocol files are unaffected, and files deleted before the upgrade are
  not remembered. See
  [ADR 13](adr/0013-pypi-write-time-validation-and-immutable-files.md) and
  [ADR 24](adr/0024-npm-packument-and-dist-tags.md).
- **A file upstream drops answers 404 at once.** Once a proxy sees that the
  upstream no longer lists a cached file, reads answer 404 straight away,
  directly and through a virtual repository, lockfile installs included. The
  file serves again if upstream lists it again. Its row is removed after
  `worker.retention.proxyStaleAfter`, 7 days by default. A placed file read past its
  cache TTL now waits for the upstream's answer before it serves, so an upstream that
  accepts connections and never answers can hold each such read for the upstream timeout,
  30 seconds by default.
  This covers npm and PyPI. On OCI, a digest manifest and its layer blobs keep serving
  from placement after upstream drops them, so an image pulled upstream as malicious keeps
  serving by digest until its tag is evicted and the collector reclaims it. A whole-package
  404 moves a virtual repository's walk on, so with `[private-proxy, npm]` a package removed
  from the private upstream can resolve from the public proxy; only a hosted claim protects
  a private name.
- **A malformed provenance header is a 400.** A provenance header with invalid
  UTF-8 or a NUL byte is rejected.
- **npm is new.** An npm repository needs `publicUrl`, and a configuration that
  declares one without it no longer loads. See the
  [configuration reference](configuration.md).

## Rolling back

Every migration stays backward compatible for one release, so the previous
binary runs against the newer schema. To roll back one release, start the
previous binary or image. It finds no migrations to apply, leaves the newer
schema in place, and serves. A rollback does not need a database restore.

Rolling back from 1.1 to 1.0.x needs the configuration file changed first. The 1.0.x
binary does not know `publicUrl` or `protocol: npm`, so it refuses to start on a file that
has either. Remove `publicUrl` and the npm repositories before starting it. While rolled
back, files an upstream dropped serve again and deleted names are no longer enforced as
taken.

Rolling back further than one release is not supported without restoring the
backup taken before the upgrade.

## Pinning

The quickstart uses `ghcr.io/spjdevops/sindri:latest` so that it works first
time. For anything you depend on, pin a version tag, or a digest, and upgrade
on purpose.
