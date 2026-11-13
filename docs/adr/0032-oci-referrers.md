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
   facts read from the bytes alone, never from the `Content-Type` a client sent, and the
   first push writes the row for good: it is shared by every repository that places the
   digest, so a later push must not be able to change it. An image manifest and an index
   may both carry a subject. The subject does not have to exist: the specification
   allows a referrer to arrive first.
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
6. **A referrer lives as long as its subject, in a repository that holds both.** The
   garbage collector treats a referrer as reachable when its subject is reachable and
   some repository places both of them. It is collected with its subject, or after the
   grace period once it has been deleted from that repository. A referrer whose subject
   never arrives is collected like any untagged manifest.
7. **A referrer that is not an image is not scanned on publish.** A signature or an SBOM
   is a document, and a Trivy run on it is noise. The test is the manifest itself: it
   names a subject, it is not an index, its config is not an image config, and none of
   its layers has an image layer media type. Anything that looks like an image in either
   respect is scanned like any other, so a `subject` field and an odd config type are
   not a way around the scan. A manifest that skips the scan has no Sindri scan, so a
   severity gate refuses it (ADR 30); the on-demand scan route still works.
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
- The sweep holds a referrer for as long as some repository places both it and its
  subject, the same conservative rule as an edge from a dead parent, so a referrer goes
  one pass after its subject. One race stays open: a subject pushed at the moment its
  long-orphaned referrer is being swept loses that referrer. The client that pushes an
  image pushes its signature afterwards, which writes it again.
- cosign's older signature format uses an image config, so such a signature is still
  scanned, as it was before this ADR. The newer formats are not.
- A referrer pushed into repository B for a subject that lives only in repository A is
  not kept alive by A: it needs a tag of its own, or it is collected after the grace
  period. Nothing B pushes can pin bytes to A's image, and whether B's referrer survives
  says nothing about what A holds.
- oras labels a file as an image layer unless told otherwise, so an attachment made with
  its defaults is scanned. The docs show the layer media type that avoids it.
- `referrers` as a reserved segment stops `serve --config` on a file that declares such
  a repository. The upgrade notes say to rename it first.
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
- **Keeping a referrer alive through its subject alone, wherever either is placed.** The
  first draft did this. A referrer deleted from its repository then stayed in storage
  for as long as the image lived, anyone with write on any repository could pin bytes to
  another team's image, and the referrer's survival told its pusher whether a guessed
  digest existed elsewhere. A review found it.
- **Skipping the scan for every manifest with a subject, or for every one without an
  image config.** An image pushed with a `subject` field, or with that and a renamed
  config type, would then never be scanned, which breaks the claim that every hosted
  image is. Reviews of the first two drafts found these.
- **Updating the row on a later push.** A push into one repository, with a misleading
  `Content-Type`, could then change what another repository's list says.
