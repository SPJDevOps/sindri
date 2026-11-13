# 32. The OCI referrers API: a view over placed manifests

- **Status:** Accepted
- **Date:** 2026-11-13
- **Relates to:** [ADR 13](0013-pypi-write-time-validation-and-immutable-files.md),
  [ADR 14](0014-proxy-cache-and-virtual-resolution.md),
  [ADR 16](0016-lifecycle-gates-and-evidence-in-v1.md),
  [ADR 19](0019-garbage-collection-roots-write-intent-and-sweep.md),
  [ADR 29](0029-what-a-scan-covers.md), roadmap M11b

## Context

cosign, oras, Notation, and Trivy attach a signature or an SBOM to an image by pushing a
second manifest whose `subject` field names the image, and they find it again through
`GET /v2/<name>/referrers/<digest>`. Sindri stored such manifests and ignored the field,
so nothing attached this way could be found, and the 20 conformance specs for it have
been skipped since M3. Sindri also has its own evidence model (ADR 16), which gates
promotion. This ADR says how the two relate.

## Decision

1. **A referrer is a manifest with a `subject`, and nothing more.** On a hosted manifest
   push, Sindri records the manifest's subject digest, media type, artifact type, size,
   and annotations in `manifest_subjects`, keyed by the manifest's own digest. These are
   facts read from the bytes, so they are the same in every repository. An image
   manifest and an index may both carry a subject. The subject does not have to exist:
   the specification allows a referrer to arrive first.
2. **Referrers are never evidence.** A referrer is not copied into the `evidence` table,
   and no gate reads `manifest_subjects`. A `signature` or `sbom` gate is satisfied only
   by evidence attached through `/api/v1`, as before. The referrers API is for standard
   tooling to find what standard tooling pushed.
3. **Placement decides what is listed (I2).** The listing for a repository holds the
   referrers that are placed in that repository (`repository_blobs`), and no others. The
   same subject digest in two repositories has two separate lists. Deleting a referrer
   manifest by digest removes its placement, and with it the entry. As with blobs,
   placement is per Sindri repository, not per image name inside it.
4. **The response.** `GET /v2/<name>/referrers/<digest>` needs read on the repository,
   like every other read. It answers 200 with an OCI image index, empty when nothing
   refers to the digest or the digest is unknown, and 400 for a malformed digest. Each
   descriptor carries the referrer's `artifactType` (for an image manifest without one,
   its config media type) and annotations. `?artifactType=` filters the list and sets
   `OCI-Filters-Applied: artifactType`. Long lists are paged with a `Link` header. A
   manifest push with a subject answers with `OCI-Subject`, which tells a client that it
   need not maintain the fallback tag.
5. **Hosted, proxy, virtual.** A hosted repository answers from its own placements. A
   proxy answers with an empty index and contacts no upstream. A virtual repository
   answers from the first hosted member, in member order, that the caller may read and
   that places the subject; proxy members are passed over; if there is none, the index
   is empty. Lists are never merged across members (I5).
6. **A referrer lives as long as its subject.** The garbage collector treats a referrer
   as reachable when its subject is reachable, and collects it with its subject
   otherwise. A referrer whose subject never arrives is collected after the grace
   period, like any untagged manifest.
7. **A referrer is not scanned on publish.** It is a signature or a document, not an
   image, and a Trivy run on it is noise. It has no Sindri scan, so a severity gate
   refuses it (ADR 30); the on-demand scan route still works.
8. **No backfill.** Manifests with a subject that were pushed before this release are
   stored but not listed. Pushing them again lists them.
9. **`referrers` is a reserved repository name segment** for new repositories, with the
   other adapter markers (ADR 13). The route is recognised only as the last two path
   segments, `/referrers/<digest>`, so an existing repository or image with that segment
   in its name keeps working.

## Consequences

- A cosign signature pushed to Sindri is discoverable by cosign, and still does not
  satisfy a `signature` gate. The docs say so. Reading signatures for the gate belongs
  with signature verification (M14).
- A client that used the fallback tag scheme against an older Sindri finds its older
  signatures only through those tags until they are pushed again. The upgrade notes say
  so.
- An untagged signature is no longer collected a day after it was pushed.
- A referrer pushed into repository B for a subject that lives only in repository A stays
  alive while A's subject does. It is listed only in B, costs B's owner nothing they
  could not get with a tag, and tells B nothing about A.
- The schema gains one table, additive, so a rollback to 1.4 needs no restore.

## Alternatives rejected

- **Mirroring referrers into evidence.** A push by anyone with write on a repository
  would then satisfy a `signature` gate without the signature being checked, and two
  write paths would feed one ledger input. Evidence stays an explicit act.
- **Deriving the list by reading manifest bodies on each request.** It reads every
  untagged manifest in the repository per request, and validation and parsing belong at
  write time.
- **Backfilling on upgrade.** It needs a job that reads every hosted manifest. The
  project has no adopters with signatures to lose yet, and a re-push repairs it.
- **Proxying the upstream's referrers.** Proxy content has no subject, owner, or
  evidence (I4), and an upstream's referrers list is mutable in a way the cache has no
  rule for. Empty is honest and can be widened later.
- **Rooting every referrer in the collector.** A signature for an image that has been
  collected would live forever.
- **Scanning referrers.** See decision 7.
