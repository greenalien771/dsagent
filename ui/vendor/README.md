# Vendored front-end libraries

These files are served locally by dsagent's embedded HTTP server (`//go:embed ui/*`
in `main.go` bundles this whole directory into the binary). No CDN / network access
is required at runtime.

| File | Library | Version | Upstream artifact | License |
|---|---|---|---|---|
| `marked.umd.js` | [marked](https://github.com/markedjs/marked) | 18.1.1 | `marked@18.1.1/lib/marked.umd.js` | MIT (`LICENSE-marked.txt`) |
| `purify.min.js` | [DOMPurify](https://github.com/cure53/DOMPurify) | 3.4.17 | `dompurify@3.4.17/dist/purify.min.js` | Apache-2.0 / MPL-2.0 (`LICENSE-DOMPurify.txt`) |
| `highlight.min.js` | [highlight.js](https://github.com/highlightjs/highlight.js) | 11.12.0 | `highlightjs/cdn-release@11.12.0/build/highlight.min.js` | BSD-3-Clause (`LICENSE-highlight.js.txt`) |
| `highlight-github-dark.css` | highlight.js theme, "GitHub Dark" | 11.12.0 | `highlightjs/cdn-release@11.12.0/build/styles/github-dark.min.css` | BSD-3-Clause (`LICENSE-highlight.js.txt`) |

The theme is the one file that is **renamed** locally: upstream ships
`github-dark.min.css`, and `ui/index.html` links it as
`vendor/highlight-github-dark.css`. Keep that name when updating, or the stylesheet
silently stops loading.

## Provenance

The source archives for marked 18.1.1 and highlight.js 11.12.0 ship TypeScript
sources rather than prebuilt browser bundles, so the official prebuilt artifacts for
those exact versions were taken from jsDelivr (the package/CDN artifacts published
by the projects themselves, at the `npm` coordinate listed above). `purify.min.js`
is the `dist/purify.min.js` documented for the official DOMPurify 3.4.17 release.

## SHA-256

```
b82b9334406d8bdd16b6236c08de1cb65b2a859873ca9503d9cc0b292ca3c12a  marked.umd.js
cd37898f9782adf54820c413191b5691803f0f137c799d6edcfd38523b4c2c31  purify.min.js
8ab71eb09c51f501e5e25157d9cff100e46cc29bcbfc744d0b746d451fca7f53  highlight.min.js
ee2461da81ff8bd1f4418691ab06cc93d5c9c7a77f244ae3d6e65188127b576f  highlight-github-dark.css
```

Verify from this directory:

```sh
certutil -hashfile marked.umd.js SHA256     # Windows
shasum -a 256 marked.umd.js                 # macOS / Linux
```

The three `LICENSE-*.txt` files are byte-for-byte copies of the license files
published at those versions, and are not edited by hand. Their hashes, if you ever
need to prove provenance:

```
8e3a3f82f59a60958f56ca08f445647c32a4733dc7ca6c2c46f6eb898471ab9c  LICENSE-marked.txt
cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30  LICENSE-DOMPurify.txt
6c081431591d9df696c82dc598fe1423765b8a299b200ed00b281afd0f64c490  LICENSE-highlight.js.txt
```

## Updating

1. Replace the file(s) above with the new release artifacts, keeping the local
   filename - `ui/index.html` hard-codes all four paths.
2. Replace the matching `LICENSE-*.txt` with the new release's license file, and
   add or drop a file if the license set changed.
3. Update the version column here and the version comment at the top of
   `ui/index.html`.
4. Recompute the SHA-256 block above.
5. Rebuild. On Windows the running `dsagent.exe` is locked, so stop dsagent first,
   or prove the build elsewhere with `go build -o "%TEMP%\check.exe" .`.
