# Upgrading Sindri

<!-- commands checked by: scripts/upgrade-gate.sh -->

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

1. Back up Postgres. Migrations are written to be safe, but a backup is what
   makes any mistake recoverable.
2. Replace the binary, or change the image tag:

   <!-- upgrade:steps -->
   ```bash
   # Compose quickstart
   SINDRI_IMAGE=ghcr.io/spjdevops/sindri:<version> docker compose up -d

   # Binary
   tar -xzf sindri_<version>_linux_amd64.tar.gz sindri
   install -m 0755 sindri /usr/local/bin/sindri
   systemctl restart sindri
   ```
   <!-- /upgrade:steps -->

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

## Behaviour changes in 1.1.1

- **npm packuments carry real publish times.** Migration 00011 adds
  `versions.published_at`, set to the insert time for every new version. Yarn 4 now
  installs on its default settings, so you can remove `npmMinimalAgeGate: 0` from
  `.yarnrc.yml`. Read the next two items first.
- **Yarn 4 on defaults now holds back anything published in the last 24 hours, internal
  versions included.** Its default `npmMinimalAgeGate` is one day, and it now has a real
  time to compare. A version your team publishes will not install for a day. To exempt an
  internal scope, add it to `.yarnrc.yml`:

  ```yaml
  npmPreapprovedPackages: ["@acme/*"]
  ```

  The glob form is the one that matches on yarn 4.18.1; a bare `"@acme"` does not.
- **There is no backfill, and removing `npmMinimalAgeGate: 0` breaks versions published
  before the upgrade.** Such a version has no recorded time and is left out of `time`,
  because Sindri does not invent one. Yarn 4 treats a version it cannot date as
  quarantined, so once you remove the setting it will not install, now or later, unless
  its package is pre-approved as above. Pre-approve the affected scopes before you remove
  `npmMinimalAgeGate: 0`. The other way out is to set the time yourself in Postgres
  (`UPDATE versions SET published_at = ... WHERE ...`, on the `versions` table). That is
  an operator asserting a publish time Sindri never observed, so it is your choice to
  make, and Sindri does not suggest it.
- **A proxy downloads whole upstream documents.** The proxy now asks npmjs for the full
  document instead of the abbreviated one, so upstream traffic, parsing, the response a
  client gets, and the cache all grow. A cached document gains `time` only when it
  refreshes, and a repository with `ttl: 0` never refreshes (a cached hit is fresh
  forever), so a document cached before the upgrade never gains `time` there. To fix
  that, set a non-zero `ttl` on the proxy and reload (or run `sindri apply`). The `ttl` is
  read on every request and an entry's age counts from when it was fetched, so an old
  entry is stale at once and the next read refreshes it. This is from reading the code,
  not a run of an upgraded instance.
  That remedy covers config-managed repositories only. The API cannot change the `ttl` of
  a repository it created (it can create and delete, not update), so the way out for an
  API-created proxy is to delete it and create it again with a non-zero `ttl`, which
  needs an admin and is refused for a repository that has promotion events. Raising `ttl`
  from 0 also makes the repository's old entries, and the cached files they justified,
  evictable once they are older than `ttl` plus the stale allowance
  (`worker.retention.proxyStaleAfter`), where at `ttl: 0` nothing was ever evicted.
  See [ADR 24](adr/0024-npm-packument-and-dist-tags.md).
- **A proxy backs off for 30 seconds after its upstream stops answering.** Since 1.1.0 a
  placed npm or PyPI file read past the `ttl` contacts upstream before it serves, so a
  blackholed upstream made each such read wait the full timeout. After one refresh fails
  because the connection failed or no response headers arrived within the timeout, that
  repository's documents are not refreshed for 30 seconds: placed files serve from the
  cache and anything else answers 502 at once. A file upstream removes during that window
  keeps serving until a read after the window ends refreshes the document. A 5xx does not
  start the window: it is a fast answer, so it fails that one request with a 502, and a
  stale copy still serves, as before. Nor do 401, 403, 429, an oversize or unparsable
  document, or a body that stalls after the headers. The window is not configurable, and
  is held in memory per process. A placed file whose document is still fresh is also
  served without loading the document.
- **Two pushes are now refused.** Publishing an npm tarball
  that has a partial block after its end-of-archive marker is refused (SIN-59). npm and pnpm
  install such a tarball, but yarn 1 cannot extract it. An OCI manifest whose
  `org.opencontainers.image.source` or `org.opencontainers.image.revision` annotation
  holds a NUL byte or invalid UTF-8 is a 400 (SIN-63), as a bad provenance header already
  was.

## Behaviour changes in 1.2

1.2 adds no migration and changes no setting, so upgrading from 1.1.1 means replacing the
image or binary and nothing else. It adds the Helm chart
([ADR 28](adr/0028-helm-chart-location-and-publishing.md)), the `sindri_queue_jobs`
metric, and a `token:` form for upstream credentials
([ADR 27](adr/0027-upstream-credentials-may-be-a-bearer-token.md)). Four things behave
differently:

- **A large `n` on an OCI tags list is capped, not refused.** Above 200 it was a 400. It
  is now treated as 200, and the response carries a `Link` header for the next page, as
  Distribution does. A zero or negative `n` is still refused, and the management API's
  page limit is unchanged (SIN-79).
- **OCI manifest reads honour every `Accept` line.** Only the first line was read, so a
  Docker daemon, which sends one line per media type, could get a 404 through a proxy of
  a Distribution registry (SIN-78).
- **A PyPI proxy resolves relative file links against the project page,** as PEP 503
  says, not against the upstream's base. This is what makes a devpi upstream work. The
  cached copy of an index page now stores its links absolute (SIN-77).
- **An OCI proxy refuses a `token:` credential.** Configuration validation rejects one,
  and none is sent on an OCI request. `token:` is new in 1.2, so no existing
  configuration is affected (SIN-53).

## Behaviour changes in 1.3

- **Remove scan gates before rolling back to 1.2.x.** A stored `scan` gate rule stops a
  1.2.x binary from starting under `serve --config`, because that parser does not know
  the `scan` key and refuses it. Delete the `scan:` gates from the configuration, apply
  it, and only then start the older binary.

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

Rolling back from 1.1.1 to 1.1.0 needs no configuration change. A 1.1.0 server serves no
`time` for a hosted package, and a proxy goes back to the abbreviated document as its cache
entries refresh, so yarn 4 refuses every version again unless `npmMinimalAgeGate: 0` is
back in `.yarnrc.yml`. Restore it before you roll back. The `published_at` column stays,
unused.

Rolling back further than one release is not supported without restoring the
backup taken before the upgrade.

## Pinning

The quickstart uses `ghcr.io/spjdevops/sindri:latest` so that it works first
time. For anything you depend on, pin a version tag, or a digest, and upgrade
on purpose.

Each release also publishes an image that includes Trivy, for vulnerability
scanning: the same tag with a `-trivy` suffix (`<version>-trivy`, `latest-trivy`).
The default image stays free of it.
