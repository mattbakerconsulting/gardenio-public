package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Search (Lands workbench-glass spec WB-5; step 5.7, gd-Oqh2suV8ap).

type fakeSearch struct {
	asked []string
	doc   func(q string) *SearchDoc
	err   error
}

func (f *fakeSearch) Search(_ context.Context, q string) (*SearchDoc, error) {
	f.asked = append(f.asked, q)
	if f.err != nil {
		return nil, f.err
	}
	return f.doc(q), nil
}

// GET /garden/search asks the reader for the query as typed, whitespace collapsed and capped, never for an empty one; it
// drops a repeated hit (the exact id one leads), clips the snippets and caps the hits.
func TestSearchViewAsksTheReader(t *testing.T) {
	f := &fakeSearch{doc: func(q string) *SearchDoc {
		d := &SearchDoc{Mode: SearchSemantic, Hits: []SearchHit{
			{ID: "gd-exact", Title: "the id", Exact: true},
			{ID: "gd-one", Title: "one", Text: strings.Repeat("word ", 200)},
			{ID: "gd-exact", Title: "the id again"},
			{ID: "", Title: "no id"},
		}}
		for i := 0; i < searchHitsMax; i++ {
			d.Hits = append(d.Hits, SearchHit{ID: "gd-" + strings.Repeat("x", i+1)})
		}
		return d
	}}
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Search: f}

	_, doc := getQueue(t, h, "/garden/search?q=+edge%0A%20idp+")
	if doc["q"] != "edge idp" || doc["mode"] != "semantic" {
		t.Errorf("q %v mode %v", doc["q"], doc["mode"])
	}
	hits := doc["hits"].([]any)
	if len(hits) != searchHitsMax {
		t.Fatalf("%d hits, want the cap %d", len(hits), searchHitsMax)
	}
	first, second := hits[0].(map[string]any), hits[1].(map[string]any)
	if first["id"] != "gd-exact" || first["exact"] != true || second["id"] != "gd-one" {
		t.Errorf("first two hits %v, %v: the exact id leads, its repeat and the idless hit go", first, second)
	}
	if txt := second["text"].(string); len([]rune(txt)) > searchTextMax || !strings.HasSuffix(txt, "…") {
		t.Errorf("a long snippet is clipped to %d runes: %d", searchTextMax, len([]rune(txt)))
	}

	_, doc = getQueue(t, h, "/garden/search?q="+strings.Repeat("a", searchQueryMax+50))
	if n := len([]rune(doc["q"].(string))); n != searchQueryMax {
		t.Errorf("a long query is cut to %d runes: %d", searchQueryMax, n)
	}

	_, doc = getQueue(t, h, "/garden/search?q=%20%20")
	if doc["q"] != "" || doc["mode"] != nil {
		t.Errorf("an empty query: q %v mode %v", doc["q"], doc["mode"])
	}
	if hits, ok := doc["hits"].([]any); !ok || len(hits) != 0 {
		t.Errorf("an empty query's hits %#v, want []", doc["hits"])
	}
	if len(f.asked) != 2 || f.asked[0] != "edge idp" {
		t.Errorf("reader asked %q: once per non-empty query", f.asked)
	}

	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}, "/garden/search?q=x"); code != http.StatusServiceUnavailable {
		t.Errorf("no reader: %d, want 503", code)
	}
	if code, _ := getQueue(t, &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Search: &fakeSearch{err: errors.New("down")}}, "/garden/search?q=x"); code != http.StatusBadGateway {
		t.Errorf("a reader error: %d, want 502", code)
	}
}

// The page inlines search.js after the Workbench it registers with, and the '/' jump and the map's keys say it searches.
func TestPageCarriesSearch(t *testing.T) {
	if i, j := strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "search.js"}}`); j < 0 || j < i {
		t.Fatalf("search.js must be inlined after workbench.js: %d, %d", i, j)
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	body := inlined(t, rec.Body.String())
	for _, want := range []string{"var SEARCH = ", "W.register('q',", `aria-label="Jump to a project, plan or bead, or search every bead"`,
		"or searches every bead (so does Control K)", "find:function(q){lift();openJump(q);}", "benchOpen('q',e.id)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// search.js's pure half runs under node: the query's address, a hit's words, the line over the hits, the empty line and
// the Cmd-K chord.
func TestSearchLogicUnderNode(t *testing.T) {
	script := `
var S = require(process.argv[1]), assert = require("assert");
assert.strictEqual(S.url("edge idp"), "/garden/search?q=edge%20idp");
assert.strictEqual(S.url("a&b=c"), "/garden/search?q=a%26b%3Dc");

var now = 100000;
assert.deepStrictEqual(S.line({ id: "gd-a", type: "task", status: "in_progress", project: "gardenio", priority: 1, updated: now - 7200 }, now),
  ["task", "in progress", "gardenio", "P1", "changed 2h ago"]);
assert.deepStrictEqual(S.line({ id: "gd-p", type: "epic", plan: true, status: "open", priority: 0, exact: true }, now),
  ["the id you typed", "plan", "open", "P0"]);
assert.deepStrictEqual(S.line({ id: "gd-g", type: "agent", status: "closed", priority: 2, updated: now - 30 }, now), ["agent", "closed", "changed 1m ago"]);
assert.deepStrictEqual(S.line({ id: "gd-x", status: "hooked_up" }, now), ["hooked up"]);

assert.strictEqual(S.mode({ mode: "semantic", hits: [{ id: "gd-a" }] }), "Closest in meaning first.");
assert.strictEqual(S.mode({ mode: "lexical", hits: [{ id: "gd-a" }] }), "Word matches.");
assert.strictEqual(S.mode({ mode: "lexical", hits: [], note: "Semantic search is off." }), "Semantic search is off.");
assert.strictEqual(S.mode({ mode: "semantic", hits: [] }), "");
assert.strictEqual(S.mode(null), "");
assert.strictEqual(S.empty("edge"), "No bead matches “edge”.");

assert.ok(S.isFind({ key: "k", metaKey: true }) && S.isFind({ key: "K", ctrlKey: true }));
assert.ok(!S.isFind({ key: "k" }) && !S.isFind({ key: "k", ctrlKey: true, shiftKey: true }) && !S.isFind({ key: "k", metaKey: true, altKey: true }));
assert.ok(!S.isFind({ key: "j", ctrlKey: true }) && !S.isFind(null));
`
	runNodeLogic(t, "search.js", script)
}

// fixtureSearch is the card world's search reader (card_browser_test.go): "edge" finds plan A and two face fixtures closest
// in meaning first, a face fixture's id finds it exactly, "words" finds one by its words with semantic search off, and
// anything else finds nothing.
func fixtureSearch(now time.Time) SearchReader {
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	return &fakeSearch{doc: func(q string) *SearchDoc {
		switch strings.ToLower(q) {
		case "edge":
			return &SearchDoc{Mode: SearchSemantic, Hits: []SearchHit{
				{ID: "gd-planA", Title: "Roll the edge IdP out", Type: "epic", Plan: true, Status: "in_progress", Project: "gardenio", Priority: 1,
					Text: "Roll the edge IdP out. Second line.", Updated: ago(3 * time.Hour)},
				{ID: "gd-FaceWork01", Title: "Typed faces for the bead panel", ShortTitle: "Typed faces", Type: "task", Status: "in_progress", Project: "gardenio",
					Priority: 2, Text: "Every bead type gets its own face in the panel, with the generic fields table as the fallback.", Updated: ago(20 * time.Minute)},
				{ID: "gd-FaceDecide1", Title: "Which IdP goes first on the edge?", Type: "decision", Status: "open", Project: "gardenio", Priority: 2,
					Updated: ago(26 * time.Hour)},
			}}
		case "gd-facework01":
			return &SearchDoc{Mode: SearchSemantic, Hits: []SearchHit{
				{ID: "gd-FaceWork01", Title: "Typed faces for the bead panel", Type: "task", Status: "in_progress", Project: "gardenio", Priority: 2, Exact: true},
			}}
		case "words":
			return &SearchDoc{Mode: SearchLexical, Note: "Semantic search is off on the beads daemon, so these are word matches only.", Hits: []SearchHit{
				{ID: "gd-FaceMsg0001", Title: "Faces are up for review", Type: "message", Status: "open", Project: "gardenio", Priority: 2},
			}}
		}
		return &SearchDoc{Mode: SearchSemantic}
	}}
}

// The search panel in a real Chromium: Ctrl-K opens the '/' jump, its last row opens the panel, the panel's field searches
// again in place and keeps the focus, Ctrl-K with the panel open goes back to the field, a hit opens its bead and Back
// returns, and on a phone the Go button's jump leads there too. What is asserted lives in testdata/search_browser.js.
func TestSearchUnderChromium(t *testing.T) {
	underChromium(t, "search_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
