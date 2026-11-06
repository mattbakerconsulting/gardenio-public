package gardenhub

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"sort"
	"strings"

	"gardenio/controller/internal/tunes"
)

// The /garden document is a small data shell: garden.html, the three pre-rendered lists and the viewer's flags. Its
// scripts and stylesheets are same-origin static assets with a content hash in their URLs (assets.go, P1.2), cached for a
// year, so a data change re-sends only the shell; still no external requests and no web fonts. The page sources live in
// web/ and are embedded, not hand-escaped into a Go raw string: garden.html is the template and node.html the Commons v2
// workbench's, the page's styles are web/css/*.css, one file per feature joined in name order, and each script and
// stylesheet says in its header comment what it is. The teensy music player on the HUD
// is internal/tunes (its CSS and script, served the same way; the playlist is fetched in the browser on the first press).
// The three lists are pre-rendered from the in-memory snapshot; the script attaches the live stream and paints the farm
// scene from those same list items.
//
// A new script or stylesheet needs no line in this file: dropped into web/ it is a page source by its file name, and
// naming it in a bundle block of garden.html or node.html puts it on the page; a stylesheet in web/css is on it already,
// at the place its name sorts to (gd-MMuj3Rj6tn).
//
//go:embed web/garden.html web/node.html web/*.js web/*.css web/css/*.css
var webFS embed.FS

// webFile is one file of webFS by its name in web/. Every caller names a file the go:embed above matches, so the read
// cannot fail.
func webFile(name string) string {
	b, _ := fs.ReadFile(webFS, "web/"+name)
	return string(b)
}

var pageHTML, nodeHTML = webFile("garden.html"), webFile("node.html")

// xtermFS is xterm.js, the Glass's terminal (web/vendor/VENDOR.md).
//
//go:embed web/vendor/xterm.bundle.js web/vendor/xterm.css
var xtermFS embed.FS

// XtermFS holds xterm.bundle.js and xterm.css at its root, for beads-viewer's agent page, which loads them from its
// /assets/static/. The Glass loads the same files hashed from /garden/assets/. Sub of a compile-time embedded directory
// cannot fail.
var XtermFS, _ = fs.Sub(xtermFS, "web/vendor")

// pageXtermJS and pageXtermCSS are the two files the go:embed above names, so their reads cannot fail; an empty one
// would fail TestXtermBundleSize and TestPageAssetsAreImmutable.
var pageXtermJS, pageXtermCSS = xtermFile("xterm.bundle.js"), xtermFile("xterm.css")

func xtermFile(name string) string {
	b, _ := fs.ReadFile(xtermFS, "web/vendor/"+name)
	return string(b)
}

// pageSources are the page's scripts and stylesheets by source name, their path below web/: every web/*.js, web/*.css
// and web/css/*.css, and the music player's two.
var pageSources = func() map[string]string {
	srcs := map[string]string{"tunes.css": tunes.CSS, "tunes.js": tunes.JS}
	for _, glob := range []string{"web/*.js", "web/*.css", "web/css/*.css"} {
		files, _ := fs.Glob(webFS, glob)
		for _, f := range files {
			name := strings.TrimPrefix(f, "web/")
			srcs[name] = webFile(name)
		}
	}
	return srcs
}()

// pageAssets are what garden.html and node.html load: sky.js (in <head>, before first paint) by its own name, and one bundle for each
// of its {{define}} blocks, page.css and the page-*.js scripts, each its sources joined in the order the block lists them
// (bundle, assets.go).
var pageAssets = newAssetTable(map[string]string{"sky.js": pageSources["sky.js"]}).withBundles(pageHTML, pageSources).withBundles(nodeHTML, pageSources).
	withVendored(map[string]string{"xterm.bundle.js": pageXtermJS, "xterm.css": pageXtermCSS})

var pageTmpl = template.Must(template.New("garden").Funcs(template.FuncMap{"asset": pageAssets.url}).Parse(pageHTML))

// nodeTmpl is the Commons v2 workbench's page (web/node.html, plan gd-lRaCxsiLvC step 4): what a /garden/c/n/{id} address
// gets in place of the farm's page.
var nodeTmpl = template.Must(template.New("node").Funcs(template.FuncMap{"asset": pageAssets.url}).Parse(nodeHTML))

// assetsTag identifies the embedded page sources. It is folded into the page
// ETag: the snapshot version restarts with the process, so without it a browser
// holding a cached page from before a deploy could get a 304 for the old markup
// (and the old asset URLs).
var assetsTag = func() string {
	names := make([]string, 0, len(pageSources))
	for name := range pageSources {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, s := range []string{pageHTML, nodeHTML, pageXtermJS, pageXtermCSS, EmptyPrompt} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	for _, name := range names {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write([]byte(pageSources[name]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}()
