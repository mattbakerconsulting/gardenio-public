# Vendored for the page

## three.js, for the splash

`three.module.min.js` is three.js, served by `splash.go` at `/garden/splash/three.module.min.js` for `web/splash.js`. It is
not fetched from a CDN and not built at build time: it is committed here and embedded with go:embed.

| | |
|---|---|
| Library | three.js |
| Version | 0.186.1 (r186) |
| License | MIT (below) |
| Source | https://registry.npmjs.org/three/-/three-0.186.1.tgz |
| Source sha256 | `8cd068708ea44f2c73c944b1cead2ba2f0d5c15c8fc194e5700f4e4f4a033fe7` |
| Bundled with | esbuild 0.28.2: `build/three.module.js` (which imports `build/three.core.js`) as one minified ES module, `--bundle --minify --format=esm --target=es2020 --legal-comments=inline` |
| File sha256 | `ed11a90814a27be663f430328985511ad86e03fd56a610d2347eea6f3139e705` |

three.js stopped shipping a single-file build (the module build imports `three.core.js`), and r186 dropped the `.min.js`
files, so the one file is a bundle of the published module build, not a file from the tarball. esbuild is deterministic,
so the same tarball gives the same bytes.

`make vendor-three` (`scripts/vendor-three.sh`) re-downloads the tarball, checks the source hash, bundles it and checks the
file hash. To upgrade, change the version and the three pins in that script and in this table together, run it, and
re-shoot the splash goldens. `TestSplashThreeVendored` fails when the file and the hash recorded here disagree.

### License

The MIT License

Copyright © 2010-2026 three.js authors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.

## xterm.js, for the Glass

`xterm.bundle.js` (which defines `window.Terminal`) and `xterm.css` are xterm.js, the Glass's terminal (`web/glass.js`).
`page.go` embeds them and the asset table serves them hashed and gzipped from `/garden/assets/` as written, since they
are minified upstream (`withVendored`, gd-BC3yZxTQ65). beads-viewer's agent page loads the same files from its
`/assets/static/` by their old names (`gardenhub.XtermFS`). They moved here from `cmd/beads-viewer/templates/static/`,
where they had been committed since before the repo's first public snapshot; no build fetches them.

| | |
|---|---|
| Library | xterm.js (MIT, its header in both files) |
| `xterm.bundle.js` sha256 | `f0fa9bf9b4c0e57030891fdc05b0a23761f7491153ac306e88381ef471673ba8` |
| `xterm.css` sha256 | `832f3f2c603b43ad4351ff04970150cc7a873014276db126a6065c6dd81e4872` |

`TestXtermBundleSize` (beads-viewer) caps the bundle gzipped.
