# Sindri UI design

The rules behind `static/sindri.css`. The stylesheet is one file under 30 KB, and
`web/contrast_test.go` fails when it grows past that, when it styles a class no
template uses, when a colour token is missing from this page, or when the contrast
table below disagrees with the stylesheet.

The look is fixed: hairline borders, 2px corners, no shadows, no gradients, one
steel-blue accent. `reference.html` shows every component in every state; a test
renders it and compares it with `internal/ui/testdata/reference.golden.html`. It has
no route.

## Colour: one meaning each

A colour token never gets a second meaning. If a new thing needs a colour, it needs
a new token.

| Token | Meaning |
|---|---|
| `--bg` | Page ground. |
| `--surface` | Cards, rows, tables, callouts. |
| `--surface-2` | Hover, code, and command blocks. |
| `--border` | Hairlines between rows and around panels. |
| `--border-strong` | Borders of controls and neutral pills. |
| `--text` | Body text and headings. |
| `--text-muted` | Secondary text, table headers, neutral pills. |
| `--text-faint` | Hints, meta lines, and "none" states. Still 4.5:1. |
| `--accent` | Links and the current item. The only interactive colour. |
| `--accent-hover` | A link under the pointer. |
| `--upstream` | The warm colour. Content crossed a trust boundary: it came from an upstream. Nothing else uses it. |
| `--approved` | Approved in a lifecycle stage; a healthy status dot; a scan with no findings. |
| `--revoked` | Revoked in a lifecycle stage; a failing status dot. |
| `--sev-critical` | Critical severity. Its pill is also bold. |
| `--sev-high` | High severity. |
| `--sev-medium` | Medium severity. |
| `--sev-low` | Low severity. |
| `--sev-unknown` | Unknown severity. Its pill has a dashed border. |
| `--new` | A finding that the last scan found and the one before did not. |
| `--warn` | A status dot that needs attention, such as a delivery waiting for a retry. A fill only, never text. |

Green is approved. Red is revoked or critical. Colour is never the only signal: every
pill carries text, every status dot sits beside a label, and a sortable header shows
its direction as an arrow glyph.

## Type

| Font | Where |
|---|---|
| Geist | All running text, labels, buttons. |
| Geist Mono | Anything a person may copy: digests, names, versions, commands, numbers in a stat tile. |
| Tektur | The wordmark and page and section titles. Nowhere else. |

Scale (`--text-*`): `xs` 11px (pills, table byline), `sm` 12px (hints, dense tables,
mono inline), `md` 13px (buttons, tile names), `base` 14px (body), `lg` 16px
(wordmark), `xl` 18px (section titles), `2xl` 24px (stat values, mono page titles),
`3xl` 30px (page titles), `4xl` 48px (empty-state figure).

## Space and size

Spacing is a 4px scale, `--space-N` being N times 4px: `1` 4, `2` 8, `3` 12, `4` 16,
`5` 20, `6` 24, `8` 32, `10` 40, `12` 48, `16` 64. Nothing else is used for padding,
gaps, or margins, apart from the 1px hairlines and the 2px and 6px optical nudges
inside pills and breadcrumbs.

| Token | Value | For |
|---|---|---|
| `--row-h` | 36px | Row lists, table rows, status line. |
| `--row-h-dense` | 28px | Dense table rows, tabs, pager and load-more buttons. |
| `--target` | 24px | The smallest thing a pointer or finger must hit. |

## Components

Each is shown, in every state below, on the reference page.

| Component | Classes | States |
|---|---|---|
| Pill | `.pill` | neutral; `.approve`; `.revoke`; `.none` (dashed); `.upstream`; `.new`; `.sev-critical`, `.sev-high`, `.sev-medium`, `.sev-low`, `.sev-unknown` (dashed), `.sev-none` (no findings) |
| Row list | `.rows` `.row` `.primary` `.secondary` | with secondary; link; `.empty` |
| Data table | `.table-wrap` `table` | sortable header with `aria-sort` and `.sort` link (unsorted, ascending, descending); 36px rows; `.dense` at 28px |
| Facts list | `.facts` `dt` `dd` | short value; long value that wraps; link; hint |
| Callout | `.callout` | note; `.upstream`; `.revoke` |
| Tabs | `.tabs` | default; hover; `aria-current` on the current link |
| Filter bar | `.filters` `.filter` | link; `.active` (a span) |
| Command block | `.cmd` `pre` `.copy` | the copy button is hidden unless `html.js-enhanced` is set |
| Stat tile | `.stats` `.stat` `.stat-label` `.stat-value` | neutral; `.approve`; `.revoke`; `.upstream`; link (`a.stat`) |
| Empty state | `.empty-state`; or `.row.empty` | full (also the 404 and 500 pages, the latter with a request id in `.cmd.error-id`); inline |
| Pagination | `.pager` `.where` `.off`; `.more` | both links; first page (`.off`); load more; load more fetching (`.htmx-request`) |
| Status dot | `.dot` | `.ok`; `.warn`; `.bad`; `.none`, always beside a text label |

`code.command` on the notification log is the older inline form of the command
block. A later ticket moves it onto `.cmd`.

## Header, footer, and mark

The header is one 48px row (`--space-12`): the mark and wordmark on the left; Repositories,
System (administrators only), the signed-in name as plain text, and Sign out on the
right. It lists only pages that exist. Below 720px the links give way to a native
`<details class="menu">` holding the same links, which needs no script. Both are always
in the markup, and the stylesheet shows one. The first focusable element is the
skip-to-content link, which targets `<main id="main">`.

The footer carries the version, the read-only note, and a Docs link to `ui.docsUrl`.

The mark is a squared, Tektur-style S in white on `--accent` (the light value, so it is
the same in both themes), in a square with 2px corners. The S is a path, because a
favicon cannot load a web font. `static/favicon.svg` and `static/favicon.png` (32x32) come
from one set of points in `web/genfavicon`; run `go run ./web/genfavicon` from the
repository root after changing it. The page header shows the SVG as an image.

## Contrast

Every foreground and background pair below is at least 4.5:1 (WCAG AA) in both
themes. `TestDesignContrast_TableMatchesTokensAndPasses` recomputes each ratio from
`sindri.css`, rounds to one decimal place, and fails when this table disagrees.
Pills sit on `--surface`, and on `--surface-2` when a tile or row is hovered.

| Pair | Light | Dark |
|---|---|---|
| `--text` on `--bg` | 17.0 | 16.1 |
| `--text` on `--surface` | 17.7 | 15.1 |
| `--text` on `--surface-2` | 16.1 | 13.8 |
| `--text-muted` on `--bg` | 5.9 | 7.2 |
| `--text-muted` on `--surface` | 6.1 | 6.8 |
| `--text-muted` on `--surface-2` | 5.6 | 6.2 |
| `--text-faint` on `--bg` | 5.0 | 5.9 |
| `--text-faint` on `--surface` | 5.2 | 5.5 |
| `--text-faint` on `--surface-2` | 4.7 | 5.1 |
| `--accent` on `--bg` | 6.4 | 7.1 |
| `--accent` on `--surface` | 6.6 | 6.7 |
| `--accent` on `--surface-2` | 6.0 | 6.1 |
| `--accent-hover` on `--surface` | 8.2 | 7.8 |
| `--accent-hover` on `--surface-2` | 7.4 | 7.2 |
| `--upstream` on `--surface` | 5.4 | 7.7 |
| `--upstream` on `--surface-2` | 4.9 | 7.1 |
| `--approved` on `--surface` | 5.0 | 7.2 |
| `--approved` on `--surface-2` | 4.6 | 6.6 |
| `--revoked` on `--surface` | 6.5 | 5.6 |
| `--revoked` on `--surface-2` | 5.9 | 5.2 |
| `--sev-critical` on `--surface` | 8.0 | 7.4 |
| `--sev-critical` on `--surface-2` | 7.3 | 6.8 |
| `--sev-high` on `--surface` | 6.5 | 5.6 |
| `--sev-high` on `--surface-2` | 5.9 | 5.2 |
| `--sev-medium` on `--surface` | 5.7 | 7.9 |
| `--sev-medium` on `--surface-2` | 5.2 | 7.3 |
| `--sev-low` on `--surface` | 5.4 | 8.4 |
| `--sev-low` on `--surface-2` | 4.9 | 7.7 |
| `--sev-unknown` on `--surface` | 5.8 | 6.8 |
| `--sev-unknown` on `--surface-2` | 5.3 | 6.2 |
| `--new` on `--surface` | 7.1 | 8.0 |
| `--new` on `--surface-2` | 6.5 | 7.4 |

Fills that carry no text, such as status dots, need 3:1 against what they sit on (WCAG 1.4.11).
`TestDesignContrast_FillsMatchTokensAndPass` checks this table the same way.

| Fill | Light | Dark |
|---|---|---|
| `--warn` fill on `--surface` | 3.7 | 11.5 |
| `--warn` fill on `--surface-2` | 3.4 | 10.5 |
