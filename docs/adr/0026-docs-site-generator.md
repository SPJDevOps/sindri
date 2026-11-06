# 26. The docs site is built by mdBook from plain markdown, and lychee checks its links

- **Status:** Accepted
- **Date:** 2026-11-06
- **Relates to:** [ADR 2](0002-go-single-binary.md),
  [ADR 22](0022-post-v1-direction-polyglot-teams-first.md),
  [ADR 25](0025-what-the-announcement-requires.md), roadmap M10

## Context

M10 asks for a docs site built from `docs/` and published on every release. The pages that
exist (quickstart, configuration reference, upgrading, and the ADRs) are plain markdown
that GitHub renders today, and the quickstart's commands are read out of the page by its
gate script. Whatever builds the site must not make those pages worse to read in the
repository, or harder for a script to parse.

The generator is build tooling. ADR 2's single-binary promise is about what an operator
runs, so a docs toolchain does not break it. It does add a dependency to CI and to a
contributor's machine, and that cost should stay small.

## Decision

**mdBook builds the site.** `docs/book.toml` and `docs/SUMMARY.md` are the only
generator-specific files. Every page stays a plain markdown file with relative `.md` links,
so it reads the same on GitHub as on the site. mdBook rewrites those links to `.html` and
adds search, a sidebar, and a print view.

**lychee checks every link in the built site,** anchors included, offline. `make docs`
builds the site into `./site` and fails on a broken internal link or a missing anchor.
Links to other sites are checked by `make docs-links-external`, which is not part of the
gate: a gate that fails because someone else's server is down teaches people to ignore it.

**Both are pinned binaries, checked against a sha256 before use.**
`scripts/install-docs-tools.sh` downloads them, and `make tools` and CI both run it. mdBook
publishes no checksums, so its sums were computed from the release assets when the version
was pinned. A version bump updates the version and every sum together.

**A link out of `docs/` points at GitHub,** at `main`. The site only contains `docs/`, so a
relative link to `ARCHITECTURE.md` or `deploy/` would break on it. Pointing at `main` means
a page for an old release can link to a newer file. That is acceptable while one version
of the docs is published.

**The release workflow publishes the site to GitHub Pages** after the release job
succeeds, so the site never describes a version that failed to publish. CI runs
`make docs` on every pull request.

**A directory's index page is `index.md`,** not `README.md`. mdBook renames a `README.md`
chapter to `index.html` but does not rewrite links to it. The root page, `docs/README.md`,
is the exception: nothing links to it by name, and GitHub shows it for the directory.

## Consequences

- A new page is not on the site until it is in `SUMMARY.md`. mdBook ignores a file it is
  not told about, and the docs tests fail on a page missing from `SUMMARY.md`.
- One version of the docs is published: the most recent release tag, pre-releases
  excluded. A patch tagged on an older release line would publish that line's docs over
  the newer ones; while there is one release line that cannot happen. Docs for older
  versions are in the repository at their tag.
- The `github-pages` environment GitHub creates deploys only from the default branch, so
  it must be given a rule for `v*` tags when Pages is enabled.
- The site looks plain. It works for reference documentation; a marketing site is out of
  scope for M10 anyway.

## Rejected

- **Hugo.** A Go tool, and well themed. But every page needs front matter, and relative
  `.md` links only resolve with render hooks or `ref` shortcodes. Either way `docs/` would
  read worse on GitHub, and the quickstart gate would have to skip the extra syntax.
- **MkDocs with Material.** The most polished result, and common for infrastructure
  projects. It needs a Python toolchain in CI and on contributors' machines, and Material
  went into maintenance mode in 2025, with its authors moving to a successor.
- **Docusaurus, VitePress, and other JavaScript generators.** They need a Node toolchain
  and a `package.json` with a lockfile to keep current. That is a lot of supply chain for
  four pages and the ADRs.
- **A GitHub wiki.** Already rejected by the roadmap: docs live in the repository, so they
  are versioned and reviewed with the code.
- **GitHub's own markdown rendering, with no site.** It costs nothing, but has no search,
  no navigation across pages, and nothing checks the links.
- **Versioned docs (one site per release).** mdBook has no built-in versioning, and
  nobody needs it yet. Revisit this when two supported release lines exist at once.
