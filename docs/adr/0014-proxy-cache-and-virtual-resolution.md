# 14. Proxy cache and virtual resolution

- **Status:** Accepted
- **Date:** 2026-09-30
- **Relates to:** [ARCHITECTURE.md](../../ARCHITECTURE.md) sections 2 (I4, I5) and 4,
  [ADR 3](0003-content-addressable-store-with-placement.md),
  [ADR 12](0012-pypi-hosted-adapter.md)

## Context

M5 adds proxy and virtual repositories. Four questions land together: how an OCI
path that nests an image name inside a Sindri repository is routed, how
upstream failures interact with the cache, how streaming a large first pull
stays correct under content-addressing, and where upstream credentials live.

## Decision

**OCI longest-prefix routing.** On `/v2/...`, choose the longest existing
repository name that is a prefix of the path (equal, or followed by `/`). The
remainder is the image name stored as `packages.normalized_name`. No remainder
keeps the M3 shape: the repository name is the image and tags hang off package
`_`. Writes to a matched proxy or virtual repository are rejected; unknown
write paths still auto-create a hosted repository for the full name.

**Negative cache is separate from positive.** `remote_cache.status` is `hit` or
`miss`. Only an upstream 404 writes `miss`. A 5xx, timeout, or truncated body
leaves the row unchanged and never replaces a previous hit. Fresh `miss` skips
upstream; expired `hit` refetches and falls back to the stale hit when upstream
is down. An upstream 401/403 that survives the bearer challenge is *denied*:
resolution treats it as not found (Docker Hub answers unknown names this way),
but it is never negatively cached, so a corrected credential works at once.

**Mutable names go through the TTL; immutable ones do not.** An OCI digest
reference, once placed, is served locally forever. An OCI tag and a PyPI
project index are mutable upstream, so they are served locally only while
their `remote_cache` row is fresh. A placed tag is not a reason to skip the
TTL, or `latest` would be pinned to its first pull for good.

**A proxy index is the upstream's index.** A proxied PyPI project page lists
every file the upstream index lists, with placed files taking precedence on
a filename collision (their hash is one Sindri computed). Placed files are the
subset fetched so far, never the whole project; a placed file the upstream
later drops stays listed, because the cache keeps serving it.

**Stream while staging, commit after verifying.** An uncached blob or package
file streams to the client as upstream sends it, while a `registry.CacheWriter`
stages the same bytes (a pipe into `cas.Upload.Append`). At upstream EOF the
bytes are checked against the digest and length upstream promised; only then is
the staged upload committed and placed. Memory use is constant in the file
size. When verification fails after headers are sent, the adapter cuts the
connection with `panic(http.ErrAbortHandler)`, so no client mistakes a bad body
for a complete one; OCI and pip clients verify digests themselves as well. A
client that disconnects early abandons the staged copy. The upstream timeout
covers response headers and then each gap between body reads, so a slow but
steady large layer is not cut off. A HEAD on an uncached blob is an upstream
HEAD and caches nothing; a ranged GET on an uncached blob caches the whole blob
first, because ranges are served from storage.

Metadata documents (PyPI simple JSON, OCI manifests) are small and read into
memory under a 32 MiB bound that fails rather than truncates. PyPI index JSON is
stored as a blob referenced by `remote_cache.blob_digest` so a fresh hit can
rebuild the index without re-contacting upstream.

**Manifest children before their bytes.** A proxied manifest records its
`manifest_children` edges when it is placed, which needs a `blobs` row per
child before the child is fetched; an index names platform manifests that may
never be pulled. The placeholder row takes its size from the descriptor and is
inserted with `ON CONFLICT DO NOTHING`, so it never rewrites the size of a blob
that already exists. A placeholder has no bytes behind it: M8's garbage
collector must treat an unplaced `blobs` row as absent, never as something to
delete from storage.

**Docker Hub's `library/`.** A Docker client adds `library/` to single-segment
names only when talking to Docker Hub itself. A proxy whose upstream host is a
Docker Hub host adds it for single-segment images, so `…/dockerhub/alpine` and
`…/dockerhub/library/alpine` both work. They cache as two image names.

**Credentials by reference, and only to the configured origin.**
`remotes.credentials_ref` names a secret looked up through an injected
`Credentials` interface. The secret is never stored in Postgres and never
logged. It is sent only to the scheme and host of `upstream_url`: an upstream
index may link files on any host, and a credential must not follow those links.
A bearer challenge's realm comes from the upstream and receives the credential
only over https or on the upstream's own origin. Bearer tokens are cached per
registry host and image, the scope a pull token covers. Nothing supplies secrets
yet, so the API rejects a `credentials_ref` until M7;
[ADR 15](0015-upstream-credentials-source-deferred-to-m7.md) decides the source.

**Upstream failure is a 502.** An upstream that is down, or whose bytes fail
verification before headers are sent, is reported as 502 (OCI code `UNKNOWN`),
not as an internal error or a client error.

**I4 at validate time.** `reconcile.ValidateRepository` rejects lifecycle
stages on proxy and virtual repositories. The management API calls it on
create; M7's reconciler will call the same function.

## Consequences

- Cached bytes are placed on the proxy member, never on the virtual repository.
- `TestDependencyConfusion_HostedNameIsNeverServedFromProxy` permanently guards
  the name-claim rule.
- Eager background mirroring and TTL eviction remain M8.

## Rejected

- **Serving by digest from any repository that holds the blob.** Breaks I2.
- **Treating 5xx like 404 in the negative cache.** Either hammers a healthy
  upstream after a transient outage or hides a package that appeared minutes
  ago.
- **Storing upstream passwords in `remotes`.** Secrets belong outside the
  placement schema.
- **Merging versions across virtual members.** First hit wins; scoring would
  reintroduce dependency confusion.
- **Buffering upstream files in memory before serving.** Simple, but memory
  grows with layer size and concurrency, and a first pull waits for the whole
  download. The first M5 draft did this under a 512 MiB cap, which also broke
  every larger layer.
- **Holding back the final chunk until verification.** It would let a failed
  digest end as a clean error status instead of a cut connection, but headers
  are already sent by then, so the status cannot change anyway.
- **Automatic `library/` for every upstream.** Only Docker Hub has the
  convention; elsewhere a single-segment name is a real name.
