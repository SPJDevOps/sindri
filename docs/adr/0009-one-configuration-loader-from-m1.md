# 9. One configuration loader, built in M1

- **Status:** Accepted
- **Date:** 2026-09-21
- **Relates to:** [ADR 7, A configuration surface that scales down](0007-config-surface-short-and-long-form.md),
  which this implements

## Context

M1 needs three settings: a listen address, a storage location, and a database URL. M7 adds
`repos:`, `lifecycle:`, and `defaults:` to the same file, with short and long forms, a
reconciler, and `config check`.

The tempting M1 move is a twelve-line loader: read the file, `yaml.Unmarshal` into a
struct, done. The cost lands in M7, which arrives to find a throwaway format in production
and ships a second one beside it. roadmap.md names this the most likely self-inflicted
wound in the plan, and it is the one failure this decision exists to prevent.

## Decision

`internal/config` is built in M1 as the loader for the life of the project, with four
properties that are cheap now and expensive to retrofit.

**Unknown keys are an error, at every level.** yaml.v3's `KnownFields` reaches only the
top-level document, so a section with its own `UnmarshalYAML` would silently ignore a
typo and keep its default. Every section therefore decodes through one `strictDecode`
helper that checks each key against the struct's yaml tags. A mistyped `pth:` fails at
load instead of quietly writing blobs somewhere else.

**Environment substitution walks parsed scalar nodes, never the raw text.** `${VAR}` and
`${VAR:-default}` are expanded after parsing, on string scalars only; `$$` is a literal
dollar. An unset variable with no default is an error rather than an empty string.

**Every section accepts a short form that desugars totally.** `listen: :8080`,
`storage: ./data`, `storage: s3://bucket/prefix`, `database: postgres://...` and
`log: debug` each expand into the mapping form during loading. Nothing downstream can ask
which spelling was written. A golden test loads the same setup in both spellings and
asserts the resulting trees are identical.

**Durations carry units.** `readHeaderTimeout: 30s` is valid, `3600` is rejected.

## Consequences

- M7 extends this file by adding fields and sugar rules, not by replacing a parser.
- The golden test that keeps two spellings honest exists from M1, which is when it is
  cheap to write, rather than from M7, which is when it is needed.
- Errors name the file, the line, and the field, because the tree is decoded through
  `yaml.Node` rather than straight into a struct. That is the foundation `config check`
  needs in M7.
- A password or bucket name containing `#` or `:` survives substitution intact, which a
  text-level expansion would have corrupted or silently truncated.
- More code than M1 strictly requires: roughly 400 lines instead of 50, and one reflection
  helper. Accepted deliberately.
- The default database URL carries the development credential from the Compose file, so
  `sindri serve` works after `make up` with no configuration at all. It is annotated for
  `gosec` with that justification and must never reach a deployment manifest.

## Rejected

- **A minimal M1 loader, rewritten in M7.** The wound this ADR exists to avoid. By M7 the
  throwaway format is in running deployments, and replacing it is a migration rather than
  a refactor.
- **Expanding `${VAR}` over the raw file before parsing.** Shorter, and how most tools do
  it. A value containing YAML punctuation then changes the document's structure, and every
  error loses its line number.
- **Environment variables as an override layer** (`SINDRI_LISTEN` beating the file). It is
  a second configuration system with its own precedence rules, which ADR 7 rules out.
  `${VAR:-default}` inside the file covers the same need, visibly.
- **Allowing an unset `${VAR}` to expand to an empty string.** How a storage path silently
  becomes the working directory.
