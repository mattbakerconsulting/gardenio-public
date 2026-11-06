package gardenhub

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/js"
	"github.com/tdewolff/parse/v2"
	jsparse "github.com/tdewolff/parse/v2/js"
)

// The /garden page's scripts and stylesheets are served as static assets (P1.2, gd-jrIxewKgjF), not inlined: the page
// is a small data shell whose ETag moves with every snapshot version, and inlined it re-sent ~470 KB gzipped of JS and
// CSS on every data change. Each asset's URL carries a hash of its content (art.<hash>.js), so a deploy that changes a
// file changes its URL, and the browser keeps every other file for a year without asking again.

// assetsPath is where the page's assets are served, below the viewer's base path.
const assetsPath = "/garden/assets/"

// assetCacheControl is a hashed asset's caching: its URL changes with its content, so it never needs revalidating.
const assetCacheControl = "public, max-age=31536000, immutable"

// pageAsset is one script or stylesheet the page loads.
type pageAsset struct {
	name  string // its source name, art.js
	file  string // its hashed name, art.<hash>.js
	ctype string
	src   string // the text it was built from, as written: what the tests that read a script's text read
	body  string // the bytes it is served, src minified
	// vendored: served as written, minified upstream (withVendored)
	vendored bool

	gzOnce sync.Once
	gz     []byte
}

// gzipped compresses the asset once, on its first request that accepts gzip.
func (a *pageAsset) gzipped() []byte {
	a.gzOnce.Do(func() {
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		_, _ = zw.Write([]byte(a.body))
		_ = zw.Close()
		a.gz = b.Bytes()
	})
	return a.gz
}

// assetTable holds the page's assets by source name and by hashed name.
type assetTable struct {
	byName map[string]*pageAsset
	byFile map[string]*pageAsset
	broken map[string]error // the bundles that failed to build, by name
}

// newAssetTable builds each source into its served asset.
func newAssetTable(srcs map[string]string) *assetTable {
	t := &assetTable{byName: map[string]*pageAsset{}, byFile: map[string]*pageAsset{}}
	for name, src := range srcs {
		a := buildAsset(name, src)
		t.byName[name] = a
		t.byFile[a.file] = a
	}
	return t
}

// buildAsset is the one place a source file becomes the bytes the page is served, and the hash in its name is taken
// from those bytes: the first 12 hex digits of their SHA-256, between the stem and the extension. The served bytes are
// the source minified (gd-s75uSI2rpW), so the URL moves with what is sent; the files in web/ stay as written, for the
// node tests that run them and the Go tests that read them.
func buildAsset(name, src string) *pageAsset {
	return hashedAsset(name, src, minified(assetType(name), src))
}

// hashedAsset is the asset name serves as body, under a name hashed from it.
func hashedAsset(name, src, body string) *pageAsset {
	ext := path.Ext(name)
	a := &pageAsset{name: name, ctype: assetType(name), src: src, body: body}
	sum := sha256.Sum256([]byte(a.body))
	a.file = strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:])[:12] + ext
	return a
}

// assetType is the Content-Type of a source by its extension: a stylesheet or a script.
func assetType(name string) string {
	if path.Ext(name) == ".css" {
		return "text/css; charset=utf-8"
	}
	return "text/javascript; charset=utf-8"
}

// withVendored adds files served as they are, hashed and gzipped like the rest: a vendored library its upstream already
// minified (web/vendor/xterm.bundle.js), which the minifier would only parse again at every start.
func (t *assetTable) withVendored(srcs map[string]string) *assetTable {
	for name, src := range srcs {
		a := hashedAsset(name, src, src)
		a.vendored = true
		t.byName[name] = a
		t.byFile[a.file] = a
	}
	return t
}

// assetMinifier strips the page's comments and whitespace and shortens a function's local names; a top-level name, the
// ART or BEAD another script calls, is global and kept.
var assetMinifier = func() *minify.M {
	m := minify.New()
	m.Add("text/css", &css.Minifier{})
	m.Add("text/javascript", &js.Minifier{})
	return m
}()

// minified is src minified as the media type of ctype. A source the minifier cannot parse, or a script it would
// miscompile (newDecls), is served as written: the page still works, only bigger, and TestPageAssetsAreMinified fails on
// it.
func minified(ctype, src string) string {
	mt, _, _ := strings.Cut(ctype, ";")
	out, err := assetMinifier.String(mt, src)
	if err != nil {
		return src
	}
	if mt == "text/javascript" && len(newDecls(src)) > 0 {
		return src
	}
	return out
}

// keepNamesMinifier is assetMinifier's JS pass with the names kept, so its output can be held up against the source.
var keepNamesMinifier = func() *minify.M {
	m := minify.New()
	m.Add("text/javascript", &js.Minifier{KeepVarNames: true})
	return m
}()

// newDecls is the names the JS minifier declares more often than src does: a miscompile. tdewolff/minify (v2.24.19
// through master f562a918) folds an assignment next to a var statement into it, `var a=1,b=2;x=a` to `var a=1,b=2,x=a`,
// without asking whether x is that function's own: an x declared in an enclosing function becomes a new local, and the
// outer x is never written. garden.js's frameCursor lost signFr that way (gd-s75uSI2rpW), and its signs went up against
// the moving camera. The minifier's other rewrites move a declaration or drop one, never add one, so a name declared
// more often after it than before is that fold. A source that does not parse has none; minified reports its error.
func newDecls(src string) []string {
	out, err := keepNamesMinifier.String("text/javascript", src)
	if err != nil {
		return nil
	}
	before, after := declCounts(src), declCounts(out)
	var names []string
	for name, n := range after {
		if n > before[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// declCounts is how many scopes of the script src declare each name: its vars, lets, consts, functions, classes and
// parameters.
func declCounts(src string) map[string]int {
	ast, err := jsparse.Parse(parse.NewInputString(src), jsparse.Options{})
	if err != nil {
		return nil
	}
	counts := map[string]int{}
	jsparse.Walk(scopeCounter(counts), ast)
	return counts
}

// scopeCounter counts the names each block's scope declares; a function's body is a block, and its scope holds the
// function's parameters and vars.
type scopeCounter map[string]int

func (c scopeCounter) Enter(n jsparse.INode) jsparse.IVisitor {
	if b, ok := n.(*jsparse.BlockStmt); ok {
		for _, v := range b.Scope.Declared {
			c[string(v.Data)]++
		}
	}
	return c
}

func (c scopeCounter) Exit(jsparse.INode) {}

// bundleBlock is a bundle's block in the page template: {{define "page-farm.js"}}, then one {{asset "x"}} per part.
var (
	bundleBlock = regexp.MustCompile(`\{\{define "([^"]+)"\}\}`)
	bundlePart  = regexp.MustCompile(`\{\{asset "([^"]+)"\}\}`)
)

// bundle joins the sources a page template's {{define "<name>"}} block names, in the order it names them, into one. The
// block is never executed: it keeps the order where it always was, in garden.html, one line per file. Each bundle is its
// own <script>, so its own task in the browser (a cold load's long tasks, rubric section 7, B5), and its own request (B3):
// a handful of bundles, cut where the page's parts are. A script that throws while loading stops the ones after it in its
// bundle, where its own <script> tag would not have. A script part ends with a newline and a semicolon, so a // comment
// or a missing semicolon at the end of one file cannot run on into the next; a stylesheet part ends with a newline.
func bundle(tmpl, name string, srcs map[string]string) (string, error) {
	open := `{{define "` + name + `"}}`
	i := strings.Index(tmpl, open)
	if i < 0 {
		return "", fmt.Errorf("gardenhub: no {{define %q}} block in the page template", name)
	}
	block, _, ok := strings.Cut(tmpl[i+len(open):], "{{end}}")
	if !ok {
		return "", fmt.Errorf("gardenhub: the %s block has no {{end}}", name)
	}
	end := "\n;\n"
	if path.Ext(name) == ".css" {
		end = "\n"
	}
	names, err := bundleNames(name, block, srcs)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, n := range names {
		b.WriteString(srcs[n])
		b.WriteString(end)
	}
	return b.String(), nil
}

// bundleNames is the sources a bundle's block names, in order. A part with a * is a glob (path.Match) over the source
// names, its matches in name order: {{asset "css/*.css"}} is every stylesheet in web/css, so a feature's styles are a file
// of their own that another feature's change does not touch (gd-MMuj3Rj6tn). A name that is no source, or a glob that
// matches none, is an error.
func bundleNames(name, block string, srcs map[string]string) ([]string, error) {
	var names []string
	for _, m := range bundlePart.FindAllStringSubmatch(block, -1) {
		if !strings.Contains(m[1], "*") {
			if _, ok := srcs[m[1]]; !ok {
				return nil, fmt.Errorf("gardenhub: the %s block names %s, which is not a page source", name, m[1])
			}
			names = append(names, m[1])
			continue
		}
		var matched []string
		for n := range srcs {
			if ok, _ := path.Match(m[1], n); ok {
				matched = append(matched, n)
			}
		}
		if len(matched) == 0 {
			return nil, fmt.Errorf("gardenhub: the %s block's %s matches no page source", name, m[1])
		}
		sort.Strings(matched)
		names = append(names, matched...)
	}
	return names, nil
}

// withBundles adds the bundle of each {{define}} block in tmpl to the table. One that fails to build gets no URL: the
// page's {{asset}} for it reports the build's error, so every page render fails with it, as an unknown name does.
func (t *assetTable) withBundles(tmpl string, srcs map[string]string) *assetTable {
	for _, m := range bundleBlock.FindAllStringSubmatch(tmpl, -1) {
		name := m[1]
		src, err := bundle(tmpl, name, srcs)
		if err != nil {
			if t.broken == nil {
				t.broken = map[string]error{}
			}
			t.broken[name] = err
			continue
		}
		a := buildAsset(name, src)
		t.byName[name] = a
		t.byFile[a.file] = a
	}
	return t
}

// url is the asset's path below the base path; the page template calls it as {{asset "art.js"}}. An unknown name is an
// error, which the template reports as a render error, so a typo in garden.html fails every page test rather than one
// browser.
func (t *assetTable) url(name string) (string, error) {
	if err := t.broken[name]; err != nil {
		return "", err
	}
	a, ok := t.byName[name]
	if !ok {
		return "", fmt.Errorf("gardenhub: no page asset %s", name)
	}
	return assetsPath + a.file, nil
}

// serve answers GET /garden/assets/{file}. The current hash is cached for a year. A known source with another hash (a
// page rendered by the previous build, during a rollout) gets the current file without the long cache, so it is never
// kept under a URL whose content it is not; anything else is a 404.
func (t *assetTable) serve(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	a, current := t.byFile[file]
	if !current {
		a = t.byName[unhashed(file)]
		if a == nil {
			http.NotFound(w, r)
			return
		}
	}
	hd := w.Header()
	hd.Set("Content-Type", a.ctype)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Vary", "Accept-Encoding")
	if !current {
		hd.Set("Cache-Control", "no-cache")
	} else {
		etag := `"` + a.file + `"`
		hd.Set("Cache-Control", assetCacheControl)
		hd.Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		hd.Set("Content-Encoding", "gzip")
		_, _ = w.Write(a.gzipped())
		return
	}
	_, _ = w.Write([]byte(a.body))
}

// unhashed strips the hash from a hashed name: art.0123456789ab.js -> art.js. A name without one comes back as it is.
func unhashed(file string) string {
	ext := path.Ext(file)
	stem := strings.TrimSuffix(file, ext)
	if i := strings.LastIndexByte(stem, '.'); i > 0 {
		return stem[:i] + ext
	}
	return file
}

// acceptsGzip reports whether an Accept-Encoding header admits gzip and does not refuse it with q=0.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		return strings.ReplaceAll(strings.TrimSpace(params), " ", "") != "q=0"
	}
	return false
}
