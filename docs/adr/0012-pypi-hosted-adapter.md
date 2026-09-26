# 12. PyPI hosted adapter: placement, auth, and download paths

- **Status:** Accepted
- **Date:** 2026-09-26
- **Relates to:** [ADR 6, OCI and PyPI first](0006-oci-and-pypi-first.md),
  [ADR 10, OCI hosted adapter placement](0010-oci-hosted-adapter-placement.md)
- **Amended by:** [ADR 13, write-time validation and immutable files](0013-pypi-write-time-validation-and-immutable-files.md),
  which sets what an upload must prove before it is placed

## Context

M4 adds the hosted PyPI adapter under `/pypi`. Placement for file protocols
already exists as `files` rows via `registry.Publish`. Several questions land
together: whether to auto-create repositories on upload (as OCI does), how to
treat twine's mandatory Basic credentials before M7 auth, where pip downloads
files from, and how repository names that contain slashes are routed.

## Decision

**Placement is the `files` row.** A twine upload is `registry.Publish`: bytes
land in the CAS, and a `files` row places them. `CheckPlacement` already unions
`files`, so invariant I2 needs no `repository_blobs` for PyPI. Wheel and sdist
fields such as `requires_python` live in `files.metadata` jsonb.

**No auto-create.** A PyPI repository must exist via `/api/v1` (or later the
reconciler) before upload. Twine always targets an explicit repository URL;
creating the row on first POST would hide a misconfigured URL. This differs
from OCI, where docker and distribution-spec conformance push to undeclared
names.

**Auth mirrors ADR 10 for M4.** Simple index and package downloads are
anonymous so the `pip install --index-url` gate needs no credentials. Upload
accepts HTTP Basic because twine requires a username and password, but does
not validate identity against the static bearer. M7 wires the existing
`auth.Authenticator` seam onto protocol adapters; inventing Basic-token
validation now would be throwaway work.

**Download path.** Files are served at
`/pypi/{repo}/packages/{project}/{filename}`, where `{project}` is the PEP 503
normalized name. Simple-index links are absolute to that path and carry
`#sha256={hex}` fragments plus `data-requires-python` when known.

**Slash-safe dispatcher.** Repository names may contain `/` (same
`validateRepoName` as the management API). The adapter mounts at `/pypi` as a
plain handler and splits on `/simple/`, `/upload`, and `/packages/`, matching
the OCI mid-path approach.

## Consequences

- Cross-repository package GETs still 404: placement is per repository.
- Operators create a hosted `protocol: pypi` repository before the first twine
  upload; the gate script does this via `/api/v1`.
- Local `pip install` against a development binary needs no credentials.
  Production auth remains an explicit M7 deliverable.
- ARCHITECTURE.md section 7 lists the `/packages/` download path alongside
  `simple` and `upload`.

## Rejected

- **Auto-create on first upload.** Hides typos in `--repository-url` and
  differs from the operator expectation that a private index is declared.
- **Validating Basic against the static bearer in M4.** Twine always sends
  credentials; checking them against the management-API token would invent a
  second auth mapping that M7 replaces.
- **Requiring credentials on simple/download.** Breaks the roadmap gate and
  every anonymous `pip install --index-url` workflow used in development.
- **Serving downloads only under `/simple/...`.** Pip follows absolute file
  URLs; a dedicated `/packages/` path keeps the index HTML small and makes
  content-type and Content-Length handling obvious.
- **Chi `{repo}` single-segment routes.** Would reject valid repository names
  that contain slashes.
