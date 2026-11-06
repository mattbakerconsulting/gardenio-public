package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
)

// Bundles (Lands workbench-glass spec WB-9; step 5.10, gd-UFvVkR7Mwx).

type fakeBundles struct {
	asked []string
	docs  map[string]*BundleDoc
	err   error
}

func (f *fakeBundles) Bundle(_ context.Context, id string) (*BundleDoc, error) {
	f.asked = append(f.asked, id)
	if f.err != nil {
		return nil, f.err
	}
	if d, ok := f.docs[id]; ok {
		return d, nil
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not a bundle"}
}

// GET /garden/bundle/{id} sends the reader's bundle, a repeated or idless document dropped, the documents capped (the rest
// counted) and each one's content cut, at a line break, past its own cap and the bundle's; not there, not visible or not a
// bundle is a 404, a daemon failure a 502, and no reader a 503.
func TestBundleViewSendsTheDocs(t *testing.T) {
	long := strings.Repeat("a line of a long document\n", bundleContentMax/20)
	d := &BundleDoc{ID: "gd-b", Title: "Bundle", Docs: []BundlePart{
		{ID: "gd-d1", Format: "md", Content: "# One"},
		{ID: "gd-d1", Format: "md", Content: "again"},
		{ID: "", Format: "md"},
		{ID: "gd-d2", Format: "code", Content: long},
	}}
	for i := 0; i < bundleDocsMax; i++ {
		d.Docs = append(d.Docs, BundlePart{ID: "gd-x" + strings.Repeat("x", i), Content: strings.Repeat("é", bundleContentMax/2)})
	}
	f := &fakeBundles{docs: map[string]*BundleDoc{"gd-b": d}}
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Bundles: f}

	code, doc := getQueue(t, h, "/garden/bundle/gd-b")
	if code != http.StatusOK || doc["id"] != "gd-b" || doc["title"] != "Bundle" {
		t.Fatalf("%d %v", code, doc)
	}
	docs := doc["docs"].([]any)
	if len(docs) != bundleDocsMax || doc["more"] != float64(2) {
		t.Fatalf("%d docs, more %v; want the cap %d and 2 counted", len(docs), doc["more"], bundleDocsMax)
	}
	first, second := docs[0].(map[string]any), docs[1].(map[string]any)
	if first["id"] != "gd-d1" || first["content"] != "# One" || first["cut"] != nil || second["id"] != "gd-d2" {
		t.Errorf("first two %v, %v: the repeat and the idless one go", first, second)
	}
	c := second["content"].(string)
	if second["cut"] != true || len(c) > bundleContentMax || !strings.HasSuffix(c, "document") {
		t.Errorf("a long document is cut at a line break under %d bytes: cut %v, %d bytes, ends %q", bundleContentMax, second["cut"], len(c), c[len(c)-10:])
	}
	total := 0
	for _, x := range docs {
		s, _ := x.(map[string]any)["content"].(string)
		if !utf8.ValidString(s) {
			t.Fatalf("content cut inside a rune: %q", s[len(s)-4:])
		}
		total += len(s)
	}
	if total > bundleTotalMax {
		t.Errorf("%d bytes of content sent, over the bundle's %d", total, bundleTotalMax)
	}
	if last := docs[len(docs)-1].(map[string]any); last["cut"] != true || last["content"] != nil {
		t.Errorf("past the bundle's cap a document is sent empty and cut: %v", last)
	}

	if code, _ := getQueue(t, h, "/garden/bundle/gd-nothere"); code != http.StatusNotFound {
		t.Errorf("not a bundle: %d, want 404", code)
	}
	forbidden := &Handler{Hub: New(newFake(), "/v", nil), Bundles: &fakeBundles{err: &beadsapi.APIError{StatusCode: http.StatusForbidden}}}
	if code, _ := getQueue(t, forbidden, "/garden/bundle/gd-b"); code != http.StatusNotFound {
		t.Errorf("not visible: %d, want 404", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), Bundles: &fakeBundles{err: errors.New("down")}}, "/garden/bundle/gd-b"); code != http.StatusBadGateway {
		t.Errorf("a reader error: %d, want 502", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil)}, "/garden/bundle/gd-b"); code != http.StatusServiceUnavailable {
		t.Errorf("no reader: %d, want 503", code)
	}
	if strings.Join(f.asked, " ") != "gd-b gd-nothere" {
		t.Errorf("reader asked %q", f.asked)
	}
}

// The page inlines bundle.js after the Workbench and the bead view it builds markdown with.
func TestPageCarriesBundles(t *testing.T) {
	if i, j := strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "bundle.js"}}`); j < 0 || j < i {
		t.Fatalf("bundle.js must be inlined after workbench.js: %d, %d", i, j)
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	for _, want := range []string{"var BUNDLE = ", "W.register('bundle',", "api.mdInto = function"} {
		if !strings.Contains(inlined(t, rec.Body.String()), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// bundle.js's pure half runs under node: a document's format read into the renderer it gets and its words, a diff's lines
// and counts, a csv's rows (quotes, escaped quotes, line breaks inside quotes, ragged rows, an unclosed quote), the line
// under the title and a pull request's words.
func TestBundleLogicUnderNode(t *testing.T) {
	script := `
var S = require(process.argv[1]), assert = require("assert");
assert.strictEqual(S.url("gd-a b"), "/garden/bundle/gd-a%20b");
[["md", "md"], ["Markdown", "md"], [" report ", "md"], ["diff", "diff"], ["patch", "diff"], ["csv", "csv"], ["code", "code"],
 ["image", "image"], ["screenshot", "image"], ["link", "link"], ["html", "html"], ["", ""], ["weird", ""]].forEach(function (c) {
  assert.strictEqual(S.kindOf({ format: c[0] }), c[1], c[0]);
});
assert.strictEqual(S.kindOf(null), "");
assert.strictEqual(S.formatWord({ format: "code", language: "go" }), "code · go");
assert.strictEqual(S.formatWord({ format: "image" }), "screenshot");
assert.strictEqual(S.formatWord({ format: "csv", language: "x" }), "table", "a language only for code and diffs");
assert.strictEqual(S.formatWord({}), "text");

var diff = "diff --git a/x b/x\nindex 1..2\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n ctx\n-old\n+new\n+more\n--- not a header inside a hunk\ndiff --git a/y b/y\n@@ -0,0 +1 @@\n+y\n";
var L = S.diffLines(diff);
assert.deepStrictEqual(L.map(function (l) { return l.k; }), ["file", "file", "file", "file", "hunk", "ctx", "del", "add", "add", "del", "file", "hunk", "add"]);
assert.deepStrictEqual(S.diffCounts(L), { files: 2, add: 3, del: 2 });
assert.deepStrictEqual(S.diffCounts(S.diffLines("--- a\n+++ b\n@@ -1 +1 @@\n-a\n+b")), { files: 1, add: 1, del: 1 }, "a plain unified diff counts by +++");
assert.deepStrictEqual(S.diffCounts(S.diffLines("just text")), { files: 0, add: 0, del: 0 });

assert.deepStrictEqual(S.csvRows('a,b,c\n1,"two, quoted","say ""hi"""\r\n3,"line\nbreak"\n4'), [["a", "b", "c"], ["1", "two, quoted", 'say "hi"'], ["3", "line\nbreak"], ["4"]]);
assert.deepStrictEqual(S.csvRows("a,b\n"), [["a", "b"]], "a closing line break makes no row");
assert.deepStrictEqual(S.csvRows("a,,\n"), [["a", "", ""]], "empty fields kept");
assert.deepStrictEqual(S.csvRows("  "), []);
assert.strictEqual(S.csvRows('a,"open\nnever closed'), null, "an unclosed quote is not a table");
assert.ok(S.CSV_ROWS >= 100);

assert.strictEqual(S.summary({ docs: [{}, {}], more: 1, kind: "plan_review", status: "closed" }), "3 documents · plan review · closed");
assert.strictEqual(S.summary({ docs: [{}] }), "1 document");
assert.strictEqual(S.summary({}), "0 documents");
assert.strictEqual(S.mrWord({ number: "12", state: "merged" }), "!12 · merged");
assert.strictEqual(S.mrWord({ number: "#7" }), "#7");
assert.strictEqual(S.mrWord({ state: "opened" }), "the pull request · opened");
assert.strictEqual(S.mrWord(null), "");
`
	runNodeLogic(t, "bundle.js", script)
}

// fixtureBundles is the card world's bundle reader (card_browser_test.go): the face fixture's bundle (gd-FaceBundle1,
// bead_face_test.go, whose face links here) with one document of each format a bundle shows, filed against the face epic
// and produced by the face fixture's pull request; a bundle whose documents the daemon did not answer; and one holding an
// html page that tries to run a script.
func fixtureBundles(_ time.Time) BundleReader {
	return &fakeBundles{docs: map[string]*BundleDoc{
		"gd-FaceBundle1": {ID: "gd-FaceBundle1", Title: "Workbench narrative", Kind: "plan_narrative", Status: "open",
			Description: "What step 5.3 built, for the plan's readers.",
			Parent:      &BundleRef{ID: "gd-FaceEpic01", Title: "5. Workbench: beads, plans cockpit, bundles"},
			MR:          &BundleMR{URL: "https://github.com/acme/garden/pull/57", Number: "57", State: "merged", Title: "feat(gardenhub): typed faces"},
			Docs: []BundlePart{
				{ID: "gd-FaceBndDoc2", Title: "The face block", Format: "md", Content: "# The face block\n\nEvery bead type gets a **face** in the panel; " +
					"any other type keeps the generic fields table.\n\n- a decision: its options and the one chosen\n- an escalation: the ask and who approves it\n\n" +
					"```go\ntype beadFace struct {\n\tKind string\n}\n```"},
				{ID: "gd-FaceBndDoc1", Title: "Why typed faces", Format: "md", Content: "A face says what the bead is in words. See gd-FaceWork01."},
				{ID: "gd-FaceBndDoc4", Title: "The change", Format: "diff", Language: "go", Content: "diff --git a/bead_face.go b/bead_face.go\n" +
					"--- a/bead_face.go\n+++ b/bead_face.go\n@@ -1,3 +1,4 @@\n package gardenhub\n-// faces\n+// typed faces\n+// one per kind\n"},
				{ID: "gd-FaceBndDoc5", Title: "Faces graded", Format: "csv", Content: "Face,Phone,Desktop\nwork,pass,pass\ndecision,pass,pass\n\"report, markdown\",pass,pass\n"},
				{ID: "gd-FaceBndDoc6", Title: "The bead panel on a phone", Format: "image", URL: "/image/coop/sessions/images/bundle-face-320x200.png"},
				{ID: "gd-FaceBndDoc7", Title: "The pull request", Format: "link", URL: "https://github.com/acme/garden/pull/57"},
				{ID: "gd-FaceBndDoc3", Title: "What is left for 5.4", Format: "text", Content: "Inline edit, comments, lifecycle."},
			}},
		"gd-BundleDown1": {ID: "gd-BundleDown1", Title: "A bundle the daemon half answered", Partial: []string{"docs"}},
		"gd-BundleHtml1": {ID: "gd-BundleHtml1", Title: "A web page", Docs: []BundlePart{
			{ID: "gd-BndHtmlDoc1", Title: "Page", Format: "html", Content: `<p id="p">framed</p><script>parent.document.body.dataset.pwned = "1"; document.getElementById("p").textContent = "ran"</script>`},
		}},
	}}
}

// The bundle panel in a real Chromium: a bundle's face opens it, every format renders, the contents rail goes to a
// document on a desktop and is not there on a phone, its references open their beads here, an html page runs no script,
// and a daemon failure or an unknown bundle says so. What is asserted lives in testdata/bundle_browser.js.
func TestBundleUnderChromium(t *testing.T) {
	underChromium(t, "bundle_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
