package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeBeads is a BeadReader over an in-memory bead set with dependency rows read both ways.
type fakeBeads struct {
	mu       sync.Mutex
	beads    map[string]*beadsapi.BeadDetail
	comments map[string][]beadsapi.Comment
	deps     []beadsapi.Dependency // BeadID depends on DependsOnID
	failCmt  bool
	failList bool                    // the agent list (bead_harvest_test.go) fails
	listQ    beadsapi.ListBeadsQuery // the last list query
	ctxs     []context.Context
}

// put adds or replaces b, as the one daemon in production holds what the Commons writes (cardWorldRig).
func (f *fakeBeads) put(b *beadsapi.BeadDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beads[b.ID] = b
}

func (f *fakeBeads) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxs = append(f.ctxs, ctx)
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("getting bead %s: %w", id, &beadsapi.APIError{StatusCode: 404, Message: "not found"})
}

func (f *fakeBeads) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	if f.failCmt {
		return nil, errors.New("daemon down")
	}
	return f.comments[id], nil
}

func (f *fakeBeads) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	var out []beadsapi.Dependency
	for _, d := range f.deps {
		if d.BeadID == id {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeBeads) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	var out []beadsapi.Dependency
	for _, d := range f.deps {
		if d.DependsOnID == id {
			out = append(out, d)
		}
	}
	return out, nil
}

func beadFixture() *fakeBeads {
	t0 := time.Unix(1_790_000_000, 0)
	return &fakeBeads{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-task": {ID: "gd-task", Title: "In-garden bead view", Type: "task", Status: "in_progress", Priority: 0, Assignee: "crew-1",
				Labels: []string{"project:gardenio", "ux"}, Description: "Line one.\n\nLine two <b>not html</b>.",
				Fields:    map[string]string{"mr_url": "https://github.com/x/y/pull/1", "empty": "  ", "user_bead_id": "gd-user"},
				CreatedBy: "owner@example.com", OperatedByDisplay: "Owner", CreatedAt: t0, UpdatedAt: t0.Add(time.Hour)},
			"gd-epic":  {ID: "gd-epic", Title: "Self-contained garden", Type: "epic", Status: "open"},
			"gd-inv":   {ID: "gd-inv", Title: "Inventory every link", Type: "task", Status: "closed", Labels: []string{"project:kbeads"}},
			"gd-child": {ID: "gd-child", Title: "A child step", Type: "task", Status: "open"},
			"gd-next":  {ID: "gd-next", Title: "Waits on this", Type: "bug", Status: "open"},
		},
		comments: map[string][]beadsapi.Comment{"gd-task": {
			{Author: "a", Text: "first", CreatedAt: t0},
			{Author: "b", OperatedByDisplay: "Bee", Text: "second\nline", CreatedAt: t0.Add(time.Minute)},
		}},
		deps: []beadsapi.Dependency{
			{BeadID: "gd-task", DependsOnID: "gd-epic", Type: "parent-child"},
			{BeadID: "gd-task", DependsOnID: "gd-inv", Type: "blocks"},
			{BeadID: "gd-task", DependsOnID: "gd-hidden", Type: "related"},
			{BeadID: "gd-child", DependsOnID: "gd-task", Type: "parent-child"},
			{BeadID: "gd-next", DependsOnID: "gd-task", Type: "blocks"},
			{BeadID: "gd-next", DependsOnID: "gd-task", Type: "blocks"}, // a duplicate row is listed once
		},
	}
}

func beadMux(r BeadReader) *http.ServeMux {
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Beads: r}).Register(mux)
	return mux
}

func getBead(t *testing.T, mux *http.ServeMux, id string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden/bead/"+id, nil))
	var doc map[string]any
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("bead doc is not JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, doc
}

// GET /garden/bead/{id} is what the in-garden bead view reads: the bead, its fields, both directions of its links with
// the linked beads' titles, and its newest comments. It replaces the beads-viewer bead page for /garden (gd-BdhucNqv0i).
func TestBeadViewDocument(t *testing.T) {
	f := beadFixture()
	rec, d := getBead(t, beadMux(f), "gd-task")
	if rec.Code != 200 {
		t.Fatalf("GET /garden/bead/gd-task: %d %s", rec.Code, rec.Body.String())
	}
	if !current(rec.Header()) {
		t.Errorf("Cache-Control = %q, want it revalidated (the panel shows the bead as it is now)", rec.Header().Get("Cache-Control"))
	}
	// The Workbench split (gd-7jlAbyk8Fy): the kbeads reads, the encode, then the whole.
	if st := rec.Header().Get("Server-Timing"); !regexp.MustCompile(`^kbeads;dur=[\d.]+, encode;dur=[\d.]+, total;dur=[\d.]+$`).MatchString(st) {
		t.Errorf("Server-Timing = %q", st)
	}
	for k, want := range map[string]any{
		"id": "gd-task", "title": "In-garden bead view", "type": "task", "status": "in_progress", "priority": 0.0, "assignee": "crew-1",
		"project": "gardenio", "description": "Line one.\n\nLine two <b>not html</b>.", "created_by": "owner@example.com", "operated_by": "Owner",
		"updated": float64(1_790_003_600), "comments_total": 2.0,
	} {
		if d[k] != want {
			t.Errorf("%s = %#v, want %#v", k, d[k], want)
		}
	}
	// fields: sorted, blank ones dropped
	fields, _ := json.Marshal(d["fields"])
	if string(fields) != `[{"k":"mr_url","v":"https://github.com/x/y/pull/1"},{"k":"user_bead_id","v":"gd-user"}]` {
		t.Errorf("fields = %s", fields)
	}
	// links, each hydrated with the other bead's title (and project); a bead the reader cannot return keeps only its id
	var typed beadDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &typed); err != nil {
		t.Fatal(err)
	}
	lists := map[string][]beadRel{"parents": typed.Parents, "blocked_by": typed.BlockedBy, "children": typed.Children, "blocks": typed.Blocks, "related": typed.Related}
	rel := func(k string) string { b, _ := json.Marshal(lists[k]); return string(b) }
	for k, want := range map[string]string{
		"parents":    `[{"id":"gd-epic","title":"Self-contained garden","type":"epic","status":"open"}]`,
		"blocked_by": `[{"id":"gd-inv","title":"Inventory every link","type":"task","status":"closed","project":"kbeads"}]`, // its project, for "in kbeads"
		"children":   `[{"id":"gd-child","title":"A child step","type":"task","status":"open"}]`,
		"blocks":     `[{"id":"gd-next","title":"Waits on this","type":"bug","status":"open"}]`,
		"related":    `[{"id":"gd-hidden","rel":"related"}]`,
	} {
		if got := rel(k); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	// comments: display name over author, line breaks kept
	cm, _ := json.Marshal(typed.Comments)
	if string(cm) != `[{"author":"a","text":"first","at":1790000000},{"author":"Bee","text":"second\nline","at":1790000060}]` {
		t.Errorf("comments = %s", cm)
	}
	if _, ok := d["partial"]; ok {
		t.Errorf("partial set on a full answer: %v", d["partial"])
	}
	// every daemon read carries the request's context, so the viewer's own bearer (and its visibility cut) goes with it
	f.ctxs = nil
	req := httptest.NewRequest("GET", "/garden/bead/gd-task", nil)
	req = req.WithContext(context.WithValue(req.Context(), viewerKey{}, "me"))
	beadMux(f).ServeHTTP(httptest.NewRecorder(), req)
	if len(f.ctxs) == 0 {
		t.Fatal("no daemon read")
	}
	for _, c := range f.ctxs {
		if c.Value(viewerKey{}) != "me" {
			t.Fatal("a daemon read did not carry the request context")
		}
	}
}

type viewerKey struct{}

func TestBeadViewDegradesAndRefuses(t *testing.T) {
	f := beadFixture()
	f.failCmt = true
	rec, d := getBead(t, beadMux(f), "gd-task")
	if rec.Code != 200 {
		t.Fatalf("comments down must not fail the bead: %d", rec.Code)
	}
	if p, _ := json.Marshal(d["partial"]); string(p) != `["comments"]` {
		t.Errorf("partial = %s, want [\"comments\"]", p)
	}
	if rec, _ = getBead(t, beadMux(f), "gd-nope"); rec.Code != 404 {
		t.Errorf("unknown bead: %d, want 404", rec.Code)
	}
	if rec, _ = getBead(t, beadMux(nil), "gd-task"); rec.Code != 503 {
		t.Errorf("no reader: %d, want 503", rec.Code)
	}
	if rec, _ = getBead(t, beadMux(f), strings.Repeat("x", 201)); rec.Code != 400 {
		t.Errorf("an absurd id: %d, want 400", rec.Code)
	}
}

// A refetch on a move (gd-HprlA40a7z): the document's ETag makes an unchanged one a bodyless 304, and a move that only
// added comments reads ?after=N, the comments after the first N with the titles of the ids they name; fewer than N (some
// deleted) sends none, so the panel fetches the whole bead. A bead the viewer cannot see is a 404 either way.
func TestBeadViewTagAndNewComments(t *testing.T) {
	f := beadFixture()
	f.beads["gd-Mention001"] = &beadsapi.BeadDetail{ID: "gd-Mention001", Title: "Named in a comment", Type: "task", Status: "open"}
	mux := beadMux(f)
	rec, _ := getBead(t, mux, "gd-task")
	tag := rec.Header().Get("ETag")
	if tag == "" || rec.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("ETag %q, Cache-Control %q: want a tag, revalidated", tag, rec.Header().Get("Cache-Control"))
	}
	again := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/garden/bead/gd-task", nil)
		req.Header.Set("If-None-Match", tag)
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := again(); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("unchanged: %d with %d bytes, want a bodyless 304", rec.Code, rec.Body.Len())
	}
	f.comments["gd-task"] = append(f.comments["gd-task"], beadsapi.Comment{Author: "c", Text: "See gd-Mention001 and gd-Gone000000.", CreatedAt: time.Unix(1_790_000_600, 0)})
	if rec := again(); rec.Code != 200 {
		t.Errorf("a comment added: %d, want the new document", rec.Code)
	}

	read := func(id, after string) (int, beadCommentsDoc) {
		rec, _ := getBead(t, mux, id+"?after="+after)
		var d beadCommentsDoc
		if rec.Code == 200 {
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, d
	}
	code, d := read("gd-task", "2")
	if code != 200 || d.Total != 3 || len(d.Comments) != 1 || d.Comments[0].Text != "See gd-Mention001 and gd-Gone000000." || d.Comments[0].At != 1_790_000_600 {
		t.Errorf("after 2: %d %+v, want the one new comment of 3", code, d)
	}
	if d.Mentions["gd-Mention001"] != "Named in a comment" || len(d.Unseen) != 1 || d.Unseen[0] != "gd-Gone000000" {
		t.Errorf("mentions %v, unseen %v: the new comment's ids are titled", d.Mentions, d.Unseen)
	}
	if code, d = read("gd-task", "3"); code != 200 || d.Total != 3 || len(d.Comments) != 0 {
		t.Errorf("after 3: %d %+v, want none new", code, d)
	}
	if code, d = read("gd-task", "9"); code != 200 || d.Total != 3 || len(d.Comments) != 0 || d.Comments == nil {
		t.Errorf("after 9: %d %+v, want none (the panel then fetches the whole bead)", code, d)
	}
	if code, _ = read("gd-nope", "0"); code != 404 {
		t.Errorf("unknown bead: %d, want 404", code)
	}
	f.failCmt = true
	if code, _ = read("gd-task", "2"); code != 502 {
		t.Errorf("comments down: %d, want 502", code)
	}
}

// The bounds keep one panel from pulling a whole epic: long text is cut, the newest comments and the first links are kept.
func TestBeadViewBounds(t *testing.T) {
	f := beadFixture()
	b := f.beads["gd-task"]
	b.Description = strings.Repeat("é", beadDescMax+50)
	b.Fields = map[string]string{}
	for i := 0; i < beadFieldsMax+5; i++ {
		b.Fields[fmt.Sprintf("f%03d", i)] = strings.Repeat("v", beadFieldMax+9)
	}
	var cs []beadsapi.Comment
	for i := 0; i < beadCommentsMax+7; i++ {
		cs = append(cs, beadsapi.Comment{Author: "a", Text: fmt.Sprintf("c%d", i)})
	}
	f.comments["gd-task"] = cs
	for i := 0; i < beadRelMax+3; i++ {
		f.deps = append(f.deps, beadsapi.Dependency{BeadID: fmt.Sprintf("gd-k%d", i), DependsOnID: "gd-task", Type: "parent-child"})
	}
	doc, err := readBead(context.Background(), f, "gd-task")
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(doc.Description)); n != beadDescMax || !strings.HasSuffix(doc.Description, "…") {
		t.Errorf("description %d runes, want %d ending in an ellipsis", n, beadDescMax)
	}
	if len(doc.Fields) != beadFieldsMax || doc.FieldsMore != 5 || len([]rune(doc.Fields[0].V)) != beadFieldMax {
		t.Errorf("fields %d (+%d more), first %d runes", len(doc.Fields), doc.FieldsMore, len([]rune(doc.Fields[0].V)))
	}
	if len(doc.Comments) != beadCommentsMax || doc.CommentsAll != beadCommentsMax+7 || doc.Comments[len(doc.Comments)-1].Text != fmt.Sprintf("c%d", beadCommentsMax+6) {
		t.Errorf("comments %d of %d, last %q: want the newest %d", len(doc.Comments), doc.CommentsAll, doc.Comments[len(doc.Comments)-1].Text, beadCommentsMax)
	}
	n := len(doc.Parents) + len(doc.Children) + len(doc.BlockedBy) + len(doc.Blocks) + len(doc.Related)
	if n != beadRelMax || n+doc.RelMore != 5+beadRelMax+3 {
		t.Errorf("links %d listed + %d not = %d; want the cap %d listed and all %d counted", n, doc.RelMore, n+doc.RelMore, beadRelMax, 5+beadRelMax+3)
	}
}

// Every Ledger, Porch and Mailbox title links into the garden, not to the bead page, and the page carries the panel the
// link opens and the script that opens it.
func TestPageLinksBeadsIntoTheGarden(t *testing.T) {
	f := newFake()
	f.set([]*beadsapi.BeadDetail{bead("gd-w1", "bug", 5)}, nil)
	h := New(f, "/v", nil)
	_ = h.refresh(context.Background())
	mux := http.NewServeMux()
	(&Handler{Hub: h, BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden?bead=gd-w1", nil))
	body := inlined(t, rec.Body.String())
	for _, want := range []string{
		`<a class="t" href="/v/garden?bead=gd-w1" title="gd-w1">`,
		`<section class="bv" id="bead" role="dialog" aria-labelledby="bv-title" hidden>`, `id="bv-title" tabindex="-1"`, `id="bv-x"`, `aria-label="Close the bead view"`,
		`var BEAD = (function`, `'/garden/bead/' + encodeURIComponent(id)`, `window.GARDEN={`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(body, `href="/v/gd-w1"`) {
		t.Error("an item still links to the beads-viewer bead page")
	}
	if got := beadHref("/v", "gd a/b"); got != "/v/garden?bead=gd+a%2Fb" {
		t.Errorf("beadHref escapes the id: %q", got)
	}
}

// workbench.js's bead view, pure half, runs under node: the ?bead= parameter is read and written without disturbing the camera's at= and
// z=, a link opens a bead only when it is this garden's own /garden?bead=, the panel's link lists come out in order, free
// text's URLs and bead ids are cut out as links (gd-6DquTUojWU), and a plan's phase list numbers its phases as the plot does.
func TestBeadViewLogicUnderNode(t *testing.T) {
	script := `
var V = require(process.argv[1]), assert = require("assert");
assert.strictEqual(V.param("?at=gardenio&bead=gd-abc&z=2", "bead"), "gd-abc");
assert.strictEqual(V.param("?bead=gd%2Fx+y", "bead"), "gd/x y", "decoded");
assert.strictEqual(V.param("?at=x", "bead"), null);
assert.strictEqual(V.param("", "bead"), null);
assert.strictEqual(V.param("?bead", "bead"), "");
assert.strictEqual(V.withBead("?at=gardenio%2Fgd-1&z=2", "gd-x"), "?at=gardenio%2Fgd-1&z=2&bead=gd-x", "at and z kept as written");
assert.strictEqual(V.withBead("?bead=old&at=yard", "gd-new"), "?at=yard&bead=gd-new", "one bead=, replaced");
assert.strictEqual(V.withBead("?bead=old&at=yard", ""), "?at=yard", "dropped on close");
assert.strictEqual(V.withBead("?bead=old", ""), "", "nothing left");
assert.strictEqual(V.withBead("", "a b/c"), "?bead=a%20b%2Fc");
var O = "https://gardenio.example";
assert.strictEqual(V.linkId("", O + "/garden?bead=gd-abc", O), "gd-abc");
assert.strictEqual(V.linkId("/v", "/v/garden?at=yard&bead=gd-abc", O), "gd-abc", "relative, other params");
assert.strictEqual(V.linkId("", O + "/gd-abc", O), "", "the viewer's bead page is not ours");
assert.strictEqual(V.linkId("", O + "/garden?at=yard", O), "", "a camera link opens no bead");
assert.strictEqual(V.linkId("", "https://evil.example/garden?bead=gd-abc", O), "", "another origin");
assert.strictEqual(V.linkId("/v", O + "/garden?bead=gd-abc", O), "", "outside the base path");
assert.strictEqual(V.linkId("", O + "/garden/bead/gd-abc", O), "", "the JSON route is not a link");
assert.strictEqual(V.linkId("", "https://github.com/x/y/pull/1", O), "");
var d = { related: [{ id: "r" }], parents: [{ id: "p" }], blocks: [], children: [{ id: "c1" }, { id: "c2" }] };
assert.deepStrictEqual(V.sections(d).map(function (s) { return s.title + ":" + s.list.length; }), ["Part of:1", "Children:2", "Related:1"]);
assert.deepStrictEqual(V.sections(null), []);
// what the view lists above (a plan's phases, a bundle's documents) is not listed again under Children (gd-cNbP4dtM8E)
assert.deepStrictEqual(V.sections(d, { c1: true }).map(function (s) { return s.title + ":" + s.list.map(function (b) { return b.id; }).join(","); }), ["Part of:p", "Other children:c2", "Related:r"]);
assert.deepStrictEqual(V.sections(d, { c1: true, c2: true, p: true, r: true }).map(function (s) { return s.title; }), ["Part of", "Related"], "only children are dropped, and none left drops the list");
assert.deepStrictEqual(Object.keys(V.shownIds({ phases: [{ id: "e1", title: "One", counts: {} }, { title: "No id", counts: {} }] }, { items: [{ parts: [{ sm: "1" }, { bead: "d1", t: "Doc" }] }] })).sort(), ["d1", "e1"]);
assert.deepStrictEqual(V.shownIds(null, null), {});
assert.deepStrictEqual(V.shownIds(null, null, { steps: [{ id: "s1", n: 1 }, { id: "s2", n: 2 }] }), { s1: true, s2: true }, "a staked plan's steps are listed in its Steps section");
assert.strictEqual(V.mrURL({ fields: [{ k: "mr_url", v: "https://github.com/x/y/pull/9" }] }), "https://github.com/x/y/pull/9");
assert.strictEqual(V.mrURL({ fields: [{ k: "mr_url", v: "javascript:alert(1)" }] }), "", "only http(s)");
assert.strictEqual(V.mrURL({}), "");
// a plan's phase list (cursor-grid spec 5.3, 6.4): numbered as the beds, the first not closed current, Other last and never
// current, the ones past the plot's eight in the crate
function ph(id, closed, open, status) { return { id: id, title: "P " + id, status: status || "", counts: { closed: closed, open: open }, tasks: [] }; }
var ps = V.phases({ phases: [ph("a", 3, 0), ph("b", 1, 2), ph("c", 0, 4)], unphased: { id: "", title: "Other", counts: { open: 1 }, tasks: [] } });
assert.deepStrictEqual(ps.map(function (p) { return [p.n, p.id, p.title, p.done + "/" + p.total, p.closed, p.current, p.other, p.crated]; }),
  [[1, "a", "P a", "3/3", true, false, false, false], [2, "b", "P b", "1/3", false, true, false, false], [3, "c", "P c", "0/4", false, false, false, false], [4, "", "Other", "0/1", false, false, true, false]]);
assert.deepStrictEqual(ps.map(V.phaseNote), ["3/3 steps · closed", "1/3 steps · current", "0/4 steps", "0/1 steps"]);
assert.strictEqual(V.phases({ phases: [ph("a", 0, 0, "closed"), ph("b", 0, 0)] })[1].current, true, "a closed status closes an empty phase; the next is current");
assert.strictEqual(V.phases({ phases: [ph("a", 0, 0)], unphased: { counts: {}, tasks: [] } }).length, 1, "an empty Other is no bed");
var big = []; for (var i = 0; i < 10; i++) big.push(ph("x" + i, 0, 1));
var bp = V.phases({ phases: big });
assert.deepStrictEqual(bp.map(function (p) { return p.crated; }), [false, false, false, false, false, false, false, false, true, true], "past nine beds: eight on the plot, the rest in the crate");
assert.strictEqual(V.phaseNote(bp[9]), "0/1 steps · in the crate");
assert.deepStrictEqual(V.phases(null), []);
assert.strictEqual(V.stateWord("in_progress"), "growing");
assert.strictEqual(V.stateWord("weird"), "weird");
function kinds(t) { return V.runs(t).map(function (r) { return r.k + ":" + r.s; }); }
assert.deepStrictEqual(kinds("see gd-AbCdEfGhIj."), ["text:see ", "bead:gd-AbCdEfGhIj", "text:."]);
assert.deepStrictEqual(kinds("ka-0000001089 and gd-6DquTUojWU"), ["bead:ka-0000001089", "text: and ", "bead:gd-6DquTUojWU"]);
assert.deepStrictEqual(kinds("re-introduced, xgd-AbCdEfGhIj, gd-AbCdEfGhIjk, gd-short1"), ["text:re-introduced, xgd-AbCdEfGhIj, gd-AbCdEfGhIjk, gd-short1"], "words and near-ids stay text");
assert.deepStrictEqual(kinds("branch agent/x/gd-AbCdEfGhIj; gd-AbCdEfGhIj/y, task gd-AbCdEfGhIj"), ["text:branch agent/x/gd-AbCdEfGhIj; gd-AbCdEfGhIj/y, task ", "bead:gd-AbCdEfGhIj"], "an id glued to a '/' is the path's");
assert.deepStrictEqual(kinds("MR https://h.example/a/gd-AbCdEfGhIj/b, done"), ["text:MR ", "url:https://h.example/a/gd-AbCdEfGhIj/b", "text:, done"], "an id inside a URL is the URL's");
assert.deepStrictEqual(kinds("(see https://en.example/wiki/A_(b)) or <https://x.example/y>."), ["text:(see ", "url:https://en.example/wiki/A_(b)", "text:) or <", "url:https://x.example/y", "text:>."], "brackets it did not open stay text");
assert.deepStrictEqual(kinds('"https://q.example/p" https:// http://'), ['text:"', "url:https://q.example/p", 'text:" https:// http://'], "quotes end it; a bare scheme is text");
assert.deepStrictEqual(kinds("javascript:alert(1) data:text/html,x"), ["text:javascript:alert(1) data:text/html,x"], "only http(s) links");
assert.deepStrictEqual(kinds(""), []);
assert.deepStrictEqual(kinds(null), []);
assert.strictEqual(V.runs("x https://gitlab.example.com/g/c/-/merge_requests/4242/diffs?commit_id=0123 y")[1].label, "gitlab.example.com/…/4242/diffs", "host and last two segments, no query");
assert.strictEqual(V.urlLabel("https://www.github.com/x/y"), "github.com/x/y");
assert.strictEqual(V.urlLabel("https://github.com/"), "github.com");
assert.strictEqual(V.urlLabel("https://h.example/a/b/caf%C3%A9"), "h.example/…/b/café", "decoded");
assert.strictEqual(V.urlLabel("https://h.example/a/" + "x".repeat(40)), "h.example/a/" + "x".repeat(23) + "…", "a long segment is capped");
`
	runNodeLogic(t, "workbench.js", script)
}

// A plan waiting to be approved (plan_state ready, or blocked on an open decision: asked) sends each child step's body,
// cut at beadStepBodyMax runes, so the bead view reads the whole plan from one request; a building plan, a draft and a
// plain epic send titles only.
func TestBeadViewStepBodies(t *testing.T) {
	long := strings.Repeat("步", beadStepBodyMax+20)
	f := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{
		"gd-s1":   {ID: "gd-s1", Title: "1. Read model", Type: "task", Status: "open", Description: "  Steps and summary.\n\nOn seeds.  "},
		"gd-s2":   {ID: "gd-s2", Title: "2. Stakes", Type: "task", Status: "open", Description: long},
		"gd-ask":  {ID: "gd-ask", Title: "Plan - run it?", Type: "decision", Status: "open"},
		"gd-done": {ID: "gd-done", Title: "Plan - run it?", Type: "decision", Status: "closed"},
	}}
	for _, c := range []struct {
		id, typ, state, blocker string
		plan, bodies            bool
	}{
		{"gd-ready", "epic", "ready", "", true, true},
		{"gd-asked", "epic", "draft", "gd-ask", true, true},
		{"gd-draft", "epic", "draft", "gd-done", true, false}, // its decision was answered
		{"gd-build", "epic", "building", "", true, false},
		{"gd-plain", "epic", "ready", "", false, false}, // not a plan
	} {
		b := &beadsapi.BeadDetail{ID: c.id, Title: c.id, Type: c.typ, Status: "open", Fields: map[string]string{"plan_state": c.state}}
		if c.plan {
			b.Labels = []string{"plan", "project:gardenio"}
		}
		f.beads[c.id] = b
		f.deps = append(f.deps,
			beadsapi.Dependency{BeadID: "gd-s1", DependsOnID: c.id, Type: "parent-child"},
			beadsapi.Dependency{BeadID: "gd-s2", DependsOnID: c.id, Type: "parent-child"})
		if c.blocker != "" {
			f.deps = append(f.deps, beadsapi.Dependency{BeadID: c.id, DependsOnID: c.blocker, Type: "blocks"})
		}
	}
	mux := beadMux(f)
	for _, id := range []string{"gd-ready", "gd-asked", "gd-draft", "gd-build", "gd-plain"} {
		rec, _ := getBead(t, mux, id)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", id, rec.Code, rec.Body.String())
		}
		var d beadDoc
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		if len(d.Children) != 2 {
			t.Fatalf("%s children = %+v, want the two steps", id, d.Children)
		}
		bodies := map[string]string{}
		for _, ch := range d.Children {
			bodies[ch.ID] = ch.Body
		}
		ready := id == "gd-ready" || id == "gd-asked"
		if !ready {
			if bodies["gd-s1"] != "" || bodies["gd-s2"] != "" {
				t.Errorf("%s sent step bodies %v, want titles only", id, bodies)
			}
			continue
		}
		if bodies["gd-s1"] != "Steps and summary.\n\nOn seeds." {
			t.Errorf("%s gd-s1 body = %q, want the description trimmed, its line breaks kept", id, bodies["gd-s1"])
		}
		if r := []rune(bodies["gd-s2"]); len(r) != beadStepBodyMax || r[len(r)-1] != '…' {
			t.Errorf("%s gd-s2 body is %d runes, want %d ending in an ellipsis", id, len(r), beadStepBodyMax)
		}
	}
}
