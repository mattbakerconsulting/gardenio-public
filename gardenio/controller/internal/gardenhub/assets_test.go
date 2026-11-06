package gardenhub

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The page's scripts and stylesheets are its own assets (P1.2): each URL the page names is served once a year with its
// content, gzipped for a browser that takes it, and revalidates to a 304; the page itself keeps its per-snapshot ETag and
// no-cache. A data change then re-sends only the shell.
func TestPageAssetsAreImmutable(t *testing.T) {
	mux, rec := servedPage(t)
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("page Cache-Control %q, want private, no-cache", cc)
	}
	// the lazy bundles (P1.3) are named for web/lazy.js on the body, data-<bundle>-js, not loaded by a tag
	srcs := regexp.MustCompile(`(?:src|href|data-[a-z]+-js)="/v(/garden/assets/[^"]+)"`).FindAllStringSubmatch(rec.Body.String(), -1)
	// page-node.js is the workbench's page's (node.html, TestNodeAddressGetsTheWorkbenchPage), page-nudge.js its live
	// head's nudge box, named on its body for an operator, and page-wake.js its agent head's Wake / Restart, named for a
	// viewer who may restart gardeners (TestWakePageFlag); page-nodebead.js is named on it too (node.html)
	for _, n := range []string{"page-node.js", "page-nudge.js", "page-wake.js", "page-nodebead.js"} {
		srcs = append(srcs, []string{"", assetsPath + pageAssets.byName[n].file})
	}
	// xterm.js is named only on a page that offers the Glass (TestPageOffersGlassToWatchersOnly)
	for _, n := range []string{"xterm.bundle.js", "xterm.css"} {
		srcs = append(srcs, []string{"", assetsPath + pageAssets.byName[n].file})
	}
	if len(srcs) != len(pageAssets.byName) {
		t.Errorf("the page loads %d assets, the table holds %d", len(srcs), len(pageAssets.byName))
	}
	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, m := range srcs {
		path := m[1]
		a := pageAssets.byFile[strings.TrimPrefix(path, assetsPath)]
		if a == nil {
			t.Fatalf("%s is not a page asset", path)
		}
		plain := get(path)
		if plain.Code != 200 || plain.Body.String() != a.body {
			t.Fatalf("%s: %d, %d bytes, want the %d bytes of %s", path, plain.Code, plain.Body.Len(), len(a.body), a.name)
		}
		if cc := plain.Header().Get("Cache-Control"); cc != assetCacheControl {
			t.Errorf("%s: Cache-Control %q", path, cc)
		}
		if ct := plain.Header().Get("Content-Type"); strings.HasSuffix(a.name, ".css") != strings.HasPrefix(ct, "text/css") {
			t.Errorf("%s: Content-Type %q", path, ct)
		}
		gz := get(path, "Accept-Encoding", "gzip, deflate, br")
		if gz.Header().Get("Content-Encoding") != "gzip" || gz.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("%s: not gzipped for a browser that takes it: %v", path, gz.Header())
		}
		zr, err := gzip.NewReader(gz.Body)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := io.ReadAll(zr); string(b) != a.body {
			t.Errorf("%s: the gzip does not unpack to the asset", path)
		}
		if nm := get(path, "If-None-Match", plain.Header().Get("ETag")); nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
			t.Errorf("%s: revalidation answered %d with %d bytes, want a bodyless 304", path, nm.Code, nm.Body.Len())
		}
	}

	// a page from the previous build names page-farm.js under its old hash: it gets today's, not cached under that URL
	stale := get(assetsPath + "page-farm.000000000000.js")
	if stale.Code != 200 || stale.Body.String() != pageAssets.byName["page-farm.js"].body || stale.Header().Get("Cache-Control") != "no-cache" || stale.Header().Get("ETag") != "" {
		t.Errorf("an old hash of page-farm.js: %d, Cache-Control %q, ETag %q", stale.Code, stale.Header().Get("Cache-Control"), stale.Header().Get("ETag"))
	}
	// a script inside the bundle has no URL of its own
	for _, p := range []string{"nope.js", "nope.0123456789ab.js", "garden.html", "art.js", "art.0123456789ab.js", "garden.css", "page-farm.js.map"} {
		if got := get(assetsPath + p).Code; got != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, got)
		}
	}
	if got := get(assetsPath+"page-farm.js", "Accept-Encoding", "gzip;q=0").Header().Get("Content-Encoding"); got != "" {
		t.Errorf("gzip;q=0 still got Content-Encoding %q", got)
	}
}

// A change to one source changes its URL and only its URL.
func TestAssetHashFollowsContent(t *testing.T) {
	a := newAssetTable(map[string]string{"a.js": "var A = 1;", "b.css": "b{}"})
	b := newAssetTable(map[string]string{"a.js": "var A = 2;", "b.css": "b{}"})
	url := func(tb *assetTable, name string) string {
		u, err := tb.url(name)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if url(a, "a.js") == url(b, "a.js") {
		t.Error("a.js kept its URL across a change")
	}
	if url(a, "b.css") != url(b, "b.css") {
		t.Error("b.css changed URL without changing")
	}
	if !regexp.MustCompile(`^/garden/assets/a\.[0-9a-f]{12}\.js$`).MatchString(url(a, "a.js")) {
		t.Errorf("url %q", url(a, "a.js"))
	}
	if _, err := a.url("nope.js"); err == nil {
		t.Error("an unknown asset has a URL")
	}
	for in, want := range map[string]string{"art.0123456789ab.js": "art.js", "art.js": "art.js", "garden.css": "garden.css", "x": "x"} {
		if got := unhashed(in); got != want {
			t.Errorf("unhashed(%q) = %q, want %q", in, got, want)
		}
	}
}

// The page is served minified (gd-s75uSI2rpW): every asset is smaller than its source, still parses as a script, declares
// no name more often than its source does (newDecls), and still declares the top-level names the other bundles call (var
// ART, var BEAD, ...), which are global and never renamed.
// The hash is the served bytes', so a change to a comment alone keeps the URL. The parse needs node; without it only the
// sizes are checked.
func TestPageAssetsAreMinified(t *testing.T) {
	node, _ := exec.LookPath("node")
	topVar := regexp.MustCompile(`(?m)^var ([A-Za-z_$][\w$]*) =`)
	var src, body int
	for name, a := range pageAssets.byName {
		if a.vendored {
			if a.body != a.src {
				t.Errorf("%s is vendored, minified upstream, but not served as written", name)
			}
			continue
		}
		src, body = src+len(a.src), body+len(a.body)
		if len(a.body) >= len(a.src)*9/10 {
			t.Errorf("%s is served at %d bytes from a %d-byte source: not minified", name, len(a.body), len(a.src))
		}
		if strings.HasSuffix(name, ".css") {
			continue
		}
		if names := newDecls(a.src); len(names) > 0 {
			t.Errorf("%s: the minifier would make %s a local where the source assigns an outer one, so it is served as written; keep the assignment off the next var statement (as `var to=signFr=...` in garden.js's frameCursor)", name, strings.Join(names, ", "))
			continue
		}
		for _, m := range topVar.FindAllStringSubmatch(a.src, -1) {
			if !regexp.MustCompile(`(?:\bvar |,)` + regexp.QuoteMeta(m[1]) + `=`).MatchString(a.body) {
				t.Errorf("%s: minified, it no longer declares var %s", name, m[1])
			}
		}
		if node == "" {
			continue
		}
		cmd := exec.Command(node, "-e", `new (require("vm").Script)(require("fs").readFileSync(0, "utf8"), { filename: process.argv[1] })`, name)
		cmd.Stdin = strings.NewReader(a.body)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s does not parse minified: %v\n%s", name, err, out)
		}
	}
	t.Logf("page assets: %d bytes as written, %d served", src, body)

	a := newAssetTable(map[string]string{"a.js": "var A = 1; // one", "b.css": "/* b */\nb { color: red; }"})
	b := newAssetTable(map[string]string{"a.js": "var A = 1; // the first", "b.css": "b{color:red}"})
	for _, n := range []string{"a.js", "b.css"} {
		if a.byName[n].file != b.byName[n].file {
			t.Errorf("%s changed URL with only its comments and spacing: %s, %s", n, a.byName[n].file, b.byName[n].file)
		}
	}
}

// The minifier folds an assignment into the var statement beside it even when the name is an enclosing function's
// (newDecls): the outer name is then never written. Both orders are caught, a name the function shadows on purpose is
// not, and a script caught is served as written.
func TestMinifiedRefusesANewLocal(t *testing.T) {
	for src, want := range map[string]string{
		"(function(){function f(t){var a=t+1,b=2;g=a;return b}var g=null;f(1)})()":      "g",
		"(function(){function f(t){g=t;var a=t+1;return a}var g=null;f(1)})()":          "g",
		"(function(){function f(t){var a=t+1,g=a;return g}var g=null;f(1)})()":          "",
		"(function(){function f(t){var a=t+1,b=g=a;return b}var g=null;f(1)})()":        "",
		"(function(){function f(t){var a=t+1,b=2;if(a)g=a;return b}var g=null;f(1)})()": "",
	} {
		if got := strings.Join(newDecls(src), ","); got != want {
			t.Errorf("newDecls(%s) = %q, want %q", src, got, want)
		}
		if served := minified("text/javascript; charset=utf-8", src); (served == src) != (want != "") {
			t.Errorf("minified(%s) = %s", src, served)
		}
	}
}

// Each bundle is the sources its garden.html block names, each once, in that order, each closed off from the next; every
// page script and stylesheet in web/ is in exactly one bundle but sky.js, which runs in <head> before first paint, and
// the splash's own files. The bundles are few: each is a request on a cold load (B3), page-wall.js, page-glass.js, page-land.js and page-panels.js
// only for the viewers who use them (web/lazy.js).
func TestPageBundlesJoinSourcesInOrder(t *testing.T) {
	pageHTML := pageHTML + nodeHTML // and node.html's: the workbench's page, its lazy nudge box, Wake and bead view
	blocks := bundleBlock.FindAllStringSubmatch(pageHTML, -1)
	if len(blocks) < 2 || len(blocks) > 12 {
		t.Errorf("garden.html and node.html have %d bundles, want a handful", len(blocks))
	}
	in := map[string]string{}
	for _, b := range blocks {
		name := b[1]
		got := pageAssets.byName[name]
		if got == nil {
			t.Fatalf("%s is not a page asset", name)
		}
		_, block, _ := strings.Cut(pageHTML, `{{define "`+name+`"}}`)
		block, _, _ = strings.Cut(block, "{{end}}")
		end := "\n;\n"
		if strings.HasSuffix(name, ".css") {
			end = "\n"
		}
		parts, err := bundleNames(name, block, pageSources)
		if err != nil {
			t.Fatal(err)
		}
		at := 0
		for _, part := range parts {
			if prev, ok := in[part]; ok && !strings.Contains(nodeHTML, `{{define "`+name+`"}}`) { // once per page
				t.Errorf("%s is in %s and %s", part, prev, name)
			}
			in[part] = name
			src := pageSources[part] + end
			i := strings.Index(got.src[at:], src)
			if i != 0 {
				t.Fatalf("%s does not carry %s right after the parts before it", name, part)
			}
			at += len(src)
		}
		if at != len(got.src) {
			t.Errorf("%s has %d bytes after its last part", name, len(got.src)-at)
		}
	}
	files, _ := filepath.Glob(filepath.Join("web", "*.js"))
	more, _ := filepath.Glob(filepath.Join("web", "*.css"))
	sheets, _ := filepath.Glob(filepath.Join("web", "css", "*.css"))
	for _, f := range append(append(files, more...), sheets...) {
		name := filepath.ToSlash(strings.TrimPrefix(f, "web"+string(filepath.Separator)))
		if name == "sky.js" || name == "almanac.js" || name == "splash.js" || name == "splash.css" {
			continue
		}
		if in[name] == "" {
			t.Errorf("web/%s is in none of garden.html's bundles", name)
		}
	}
	if in["tunes.js"] == "" || in["tunes.css"] == "" {
		t.Error("the music player is in none of the bundles")
	}

	if got, err := bundle(`{{define "x.js"}}{{asset "a.js"}}
{{asset "b.js"}}{{end}}{{asset "c.js"}}`, "x.js", map[string]string{"a.js": "a() // no newline", "b.js": "(b)()", "c.js": "c"}); err != nil || got != "a() // no newline\n;\n(b)()\n;\n" {
		t.Errorf("bundle = %q, %v", got, err)
	}
	if got, err := bundle(`{{define "x.css"}}{{asset "a.css"}}{{asset "b.css"}}{{end}}`, "x.css", map[string]string{"a.css": "a{}", "b.css": "b{}"}); err != nil || got != "a{}\nb{}\n" {
		t.Errorf("a stylesheet bundle = %q, %v: a stray ; would drop the rule after it", got, err)
	}
	// a glob part is its matches in name order, wherever it stands in the block
	if got, err := bundle(`{{define "x.css"}}{{asset "a.css"}}{{asset "css/*.css"}}{{end}}`, "x.css", map[string]string{"a.css": "a{}", "css/20-c.css": "c{}", "css/10-b.css": "b{}", "d.css": "d{}"}); err != nil || got != "a{}\nb{}\nc{}\n" {
		t.Errorf("a glob bundle = %q, %v", got, err)
	}
	// a bad block is an error, and its bundle's URL is that error, so every page render fails
	for _, tmpl := range []string{`{{define "x.js"}}{{asset "a.js"}}`, `{{define "x.js"}}{{asset "nope.js"}}{{end}}`, `{{define "x.js"}}{{asset "css/*.js"}}{{end}}`} {
		tb := newAssetTable(map[string]string{}).withBundles(tmpl, map[string]string{"a.js": "a"})
		if _, err := tb.url("x.js"); err == nil {
			t.Errorf("the bundle of %q has a URL", tmpl)
		}
	}
	if _, err := bundle(`{{asset "a.js"}}`, "x.js", map[string]string{"a.js": "a"}); err == nil {
		t.Error("a bundle with no block built")
	}
}

// The page is a shell (P1.2): what a data change re-sends is the markup and the lists, not the scripts. On the test farm
// it was 467 KB gzipped with everything inlined; anything near the old size means a script or stylesheet went back in.
// The log line is the repeat-visit measure lands-perf-baseline.md quotes.
func TestPageShellIsSmall(t *testing.T) {
	mux, _ := servedPage(t)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	page := get("/garden")
	const budget = 32 << 10
	if page.Header().Get("Content-Encoding") != "gzip" || page.Body.Len() > budget {
		t.Fatalf("the page shell is %d bytes gzipped (%q), want at most %d", page.Body.Len(), page.Header().Get("Content-Encoding"), budget)
	}
	first := page.Body.Len()
	for _, a := range pageAssets.byFile {
		first += get(assetsPath + a.file).Body.Len()
	}
	t.Logf("first visit %d requests, %d B gzipped; a repeat visit after a data change 1 request, %d B", len(pageAssets.byName)+1, first, page.Body.Len())
}
