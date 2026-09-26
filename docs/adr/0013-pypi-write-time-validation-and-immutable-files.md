# 13. PyPI uploads validate at write time, and published files are immutable

- **Status:** Accepted
- **Date:** 2026-09-26
- **Relates to:** [ADR 12, PyPI hosted adapter](0012-pypi-hosted-adapter.md),
  [ADR 3, content-addressable store with placement](0003-content-addressable-store-with-placement.md)

## Context

A review of the M4 adapter found the upload path accepted whatever the client
asserted. The `name` and `version` form fields were trusted, the wheel's own
`METADATA` was only a fallback for missing fields, the filename was stored
verbatim, and a second upload of a filename replaced the first through
`UpsertFile`'s `ON CONFLICT DO UPDATE`.

Each of those is fine in isolation and wrong together. pip derives the project
and version from the *filename*, not from the index entry, so an upload that
declares `name=demo&version=9.9.9` while shipping `evil-1.0.whl` produces an
index nobody can install correctly. Replacing bytes under an existing filename
breaks the consumer who recorded that file's hash, which is the entire promise
of a supply-chain registry. And the `files` unique constraint is
`(version_id, filename)`, so one filename could exist under two versions while
the download path (`/pypi/{repo}/packages/{project}/{filename}`) has no version
segment to disambiguate with.

## Decision

**The artifact is the authority; the form fields say where it goes.** Upload
extracts the wheel `METADATA` or sdist `PKG-INFO`, parses the project and
version out of the filename, and rejects the upload unless all three agree. The
project is compared after PEP 503 normalization and the version after PEP 427
escaping, so `Demo.Package` matching `demo_package` and `1.0.0+local` matching
`1.0.0_local` are agreement, while `1.0.0` against `9.9.9` is not. Placement
uses the metadata's version when the artifact carries one.

**Filenames are validated, not sanitized.** A filename must be at most 256
bytes of `[A-Za-z0-9._+-]`, must not start with a dot or contain `..`, and must
end in `.whl`, `.tar.gz`, or `.tgz`. `mime/multipart` already applies
`filepath.Base`, so a separator never reaches the handler over HTTP; the check
exists because the download path serves the filename as one path segment and
quotes it into `Content-Disposition`, and a name that cannot survive that round
trip must not enter the database.

**A published file is immutable.** `PublishParams.Immutable` places the file
with `InsertFileImmutable`, whose `DO UPDATE` refreshes only derived columns and
fires only when the stored digest already equals the incoming one. Re-uploading
identical bytes is therefore idempotent (twine retries, CI re-runs), and
re-uploading different bytes returns no row, which surfaces as
`registry.ErrFileExists` and HTTP 409. The same publish transaction also
rejects a filename already placed under another version of the package, which
is the constraint the schema cannot express because `files` has no
`package_id`. `GetFileByFilename` additionally orders and limits to one row, so
even pre-existing data cannot make a download non-deterministic.

**The upload streams, and the adapter hashes once.** The part is not read into
memory. It is hashed where the multipart reader left it (memory budget or a
temporary file), the client's `sha256_digest` is compared against that hash, and
the same reader is handed to `cas.Put` as `ExpectDigest`. The digest passed into
the CAS is always one Sindri computed over the bytes it received, never a client
claim, because that digest is also the deduplication key: `cas.Put` short
circuits on `Stat` when the expected digest is already stored.

**Adapter path markers are reserved repository name segments.**
`validateRepoName` rejects `blobs`, `manifests`, `tags`, `uploads`, `simple`,
`packages`, and `upload` as name segments. Repository names may contain
slashes, so without this `/pypi/team/packages/simple/foo/` names either the
simple index of `team/packages` or a download from `team`, and a project
legitimately called `packages` collides with the download route. With the
reservation, the PyPI dispatcher takes the leftmost marker segment and is
unambiguous.

## Consequences

- Twine and `python -m build` uploads are unaffected: both derive `name` and
  `version` from the same metadata the artifact carries.
- A rejected re-upload leaves its bytes in the CAS unreferenced, because the
  CAS write precedes the placement transaction. That is GC's problem (M8), and
  the alternative is a check-then-write race.
- `scripts/pypi-twine-gate.sh` stamps a unique `1.0.0.postYYYYMMDDHHMMSS`
  version per run, because a wheel is not byte-reproducible and the gate must
  stay re-runnable. It also asserts the 409.
- Existing repositories whose names contain a reserved segment keep working for
  reads but can no longer be created. No deployment has one: the rule lands in
  the same release as the PyPI adapter.
- M5 inherits the rule that placement never trusts a client-declared name, which
  is what the name-claim rule needs to be enforceable.

## Rejected

- **Sanitizing instead of rejecting.** Quietly rewriting a filename or picking
  one of two disagreeing versions leaves the index describing something other
  than what was uploaded. A 400 tells the publisher which of their two sources
  is wrong.
- **A unique index on `(package_id, filename)`.** `files` references a version,
  not a package, so this needs a denormalized column and a backfill. The
  in-transaction guard buys the same property now; the column can come with the
  schema change M6 already brings.
- **Trusting `sha256_digest` as the CAS expected digest.** It is the cheapest
  path (dedup without hashing) and it makes a client's claim decide what gets
  stored. Hashing the received bytes costs a local read of a file already on
  disk.
- **Buffering the upload to keep one hash pass.** 512 MiB per concurrent upload
  in exchange for avoiding a local re-read of a temporary file.
- **Allowing a re-upload to replace bytes when the uploader is the owner.**
  Ownership is not the issue; a consumer who already resolved that filename is.
- **Rejecting reserved segments in each adapter.** Two adapters, two lists, and
  a third protocol arrives to find neither. The name is validated once, where it
  is created.
