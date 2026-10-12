# Vendored assets

Everything the UI loads ships inside the binary: no CDN, no Google Fonts, no
request leaves the browser for anything but this server. That is what lets the
UI work air-gapped and keeps its Content-Security-Policy at `'self'`.

`TestVendoredAssets_MatchRecordedChecksums` fails when a file below changes
without this table changing with it. Update both together, and only from the
named upstream.

| File | Upstream | Licence | sha256 |
|---|---|---|---|
| `static/htmx.min.js` | htmx.org 2.0.11, `dist/htmx.min.js` from npm | Zero-Clause BSD (`static/LICENSE-htmx.txt`) | `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717` |
| `static/fonts/Geist-Variable.woff2` | geist 1.7.2 on npm, `dist/fonts/geist-sans/` | SIL OFL 1.1 (`static/fonts/LICENSE-Geist.txt`) | `a369fcf5628ea2aa4e1b9e2ec6a5b3624e365bda588e1f0f2f12b564f728fbb8` |
| `static/fonts/GeistMono-Variable.woff2` | geist 1.7.2 on npm, `dist/fonts/geist-mono/` | SIL OFL 1.1 (`static/fonts/LICENSE-Geist.txt`) | `fba8f577f38a2bbcbe818efa6348dd58f36303a10b8737c42fefad275be563ab` |
| `static/fonts/Tektur-Latin.woff2` | Google Fonts, Tektur v6, Latin subset, weights 400 to 700 | SIL OFL 1.1 (`static/fonts/OFL-Tektur.txt`) | `468f3e60237cb450abf4ab64f96dab0de0aee61a0339226d35899add6a1ad2ab` |

The look mirrors the maintainer's other products: Geist for text, Geist Mono
for digests, Tektur for the wordmark and headings, and the colour tokens in
`static/sindri.css`.
