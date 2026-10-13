# Security policy

Sindri is a registry: supply-chain security is the product, so a security
report is the most useful thing anyone can send.

## Reporting a vulnerability

Please **do not open a public issue**. Report privately through GitHub:
[Security → Report a vulnerability](https://github.com/SPJDevOps/sindri/security/advisories/new)
on this repository. Reports there are visible only to the maintainers.

Include what you can:

- the version (`sindri version`) and the storage backend
- the steps to reproduce, or a proof of concept
- what an attacker gains, and what they need first

We aim to acknowledge a report within **3 working days** and to agree on a fix
and a disclosure date within **14 days**. We credit reporters in the advisory
unless you ask us not to.

## Supported versions

Sindri is pre-1.0. Fixes land on `main` and in the next release. There are no
backports to earlier releases until 1.0.

## What we especially want to hear about

These are the properties the design exists to protect (ARCHITECTURE.md
section 2):

- **Cross-repository reads (invariant I2).** Any way to read a blob,
  manifest, file, evidence document, or UI page through a repository it is not
  placed in, or that the caller may not read. Deduplication means the bytes
  are physically present for everyone, so this is the most serious class.
- **Dependency confusion (invariant I5).** Any way a name claimed by a hosted
  repository is served from a proxy member of a virtual repository.
- **Unverified content.** Any way bytes reach a content-addressed key under a
  digest they do not hash to.
- **Garbage collection data loss.** Any way the collector deletes a blob that
  a tag, file, cache entry, evidence, ledger event, or in-flight push still
  needs.
- **Authentication and authorization bypass.** Any route that skips
  `auth.Authorize`, any way an expired or revoked credential keeps working,
  or any way the browser session cookie authorizes a write.
- **Ledger tampering.** Any way to change or delete a `lifecycle_events` row.

## Out of scope

- The quickstart's Compose file. It runs a development identity provider
  that signs in anyone as an administrator, and it says so.
- Denial of service through very large uploads to an authorized write
  credential. Size limits are an operator's configuration.
- Findings that need an administrator's credential to exploit.
