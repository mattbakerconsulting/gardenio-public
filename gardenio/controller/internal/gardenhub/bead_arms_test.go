package gardenhub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

// testArms is a run from the renderer's own library (its first, second and third charges, as the Change arms control posts
// them), so the tests never spell an emoji.
func testArms(t *testing.T) (canonical string, a heraldry.Arms) {
	t.Helper()
	lib := heraldry.Library()
	canonical = lib[0].Emoji + lib[1].Emoji + lib[2].Emoji
	a, err := heraldry.Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	return canonical, a
}

func armsMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func armsGet(t *testing.T, mux http.Handler, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// The bead document carries an epic's arms as the run, how many emoji it holds and its plain-words name, and says which
// types may bear them; a task carries neither, even one whose row holds arms (R6), arms the renderer cannot read are left out
// rather than drawn broken, and a bead that stored the six-token form before arms became a run reads as its run.
func TestBeadDocArms(t *testing.T) {
	canonical, a := testArms(t)
	lib := heraldry.Library()
	src := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{
		"gd-ArmsEpic01": {ID: "gd-ArmsEpic01", Title: "A plan", Type: "epic", Status: "open", Heraldry: canonical, Labels: []string{"project:gardenio"}},
		"gd-ArmsTask01": {ID: "gd-ArmsTask01", Title: "A step", Type: "task", Status: "open", Heraldry: canonical},
		"gd-ArmsProj01": {ID: "gd-ArmsProj01", Title: "A project", Type: "project", Status: "open", Heraldry: "mauve nonsense"},
		"gd-ArmsOld001": {ID: "gd-ArmsOld001", Title: "An old plan", Type: "epic", Status: "open", Heraldry: "blue " + lib[0].Emoji + " . " + lib[1].Emoji + " . " + lib[2].Emoji},
	}}
	d, err := readBead(context.Background(), src, "gd-ArmsEpic01")
	if err != nil {
		t.Fatal(err)
	}
	if d.Arms == nil || *d.Arms != (beadArms{Canonical: canonical, N: 3, Name: a.PlainName()}) || !d.ArmsOK {
		t.Fatalf("epic arms = %+v ok=%v, want %q / 3 / %q", d.Arms, d.ArmsOK, canonical, a.PlainName())
	}
	if !strings.HasPrefix(d.Arms.Name, "arms: ") || strings.Contains(d.Arms.Name, lib0(t)) {
		t.Errorf("the name is words, never the emoji: %q", d.Arms.Name)
	}
	d, _ = readBead(context.Background(), src, "gd-ArmsOld001")
	if d.Arms == nil || *d.Arms != (beadArms{Canonical: canonical, N: 3, Name: a.PlainName()}) {
		t.Errorf("the six-token form reads as its run, so the control starts from the run: %+v", d.Arms)
	}
	d, _ = readBead(context.Background(), src, "gd-ArmsTask01")
	if d.Arms != nil || d.ArmsOK {
		t.Errorf("a task bears no arms and may not: %+v ok=%v", d.Arms, d.ArmsOK)
	}
	d, _ = readBead(context.Background(), src, "gd-ArmsProj01")
	if d.Arms != nil || !d.ArmsOK {
		t.Errorf("unreadable arms are left out, and a project may still bear arms: %+v ok=%v", d.Arms, d.ArmsOK)
	}
	body, _ := json.Marshal(d)
	if strings.Contains(string(body), `"arms"`) {
		t.Errorf("no arms, no key: %s", body)
	}
}

func lib0(t *testing.T) string { t.Helper(); return heraldry.Library()[0].Emoji }

// GET /garden/arms.svg draws arms through the renderer's cache with its key as the ETag, answers a matching
// If-None-Match with 304, and refuses what is not arms or not a theme.
func TestArmsSVGRoute(t *testing.T) {
	canonical, a := testArms(t)
	mux := armsMux(&Handler{})
	rec := armsGet(t, mux, "/garden/arms.svg?theme=night&a="+url.QueryEscape(canonical))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("svg: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `aria-label="`+a.PlainName()+`"`) {
		t.Errorf("the picture is named in plain words")
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("an SVG opened as a document runs nothing: %q", rec.Header().Get("Content-Security-Policy"))
	}
	want := `"` + heraldry.Key(a, heraldry.FormatSVG, 0, heraldry.Night) + `"`
	if got := rec.Header().Get("ETag"); got != want {
		t.Errorf("ETag %s, want %s", got, want)
	}
	if rec := armsGet(t, mux, "/garden/arms.svg?theme=night&a="+url.QueryEscape(canonical), "If-None-Match", want); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", rec.Code)
	}
	for _, q := range []string{"a=" + url.QueryEscape(canonical) + "&theme=dusk", "a=blue+.+.+.+.+.", "a=nonsense", ""} {
		if rec := armsGet(t, mux, "/garden/arms.svg?"+q); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", q, rec.Code)
		}
	}
}

// GET /garden/arms/charge/{cp} serves a library charge's artwork and nothing else.
func TestArmsChargeRoute(t *testing.T) {
	mux := armsMux(&Handler{})
	c := heraldry.Library()[0]
	rec := armsGet(t, mux, "/garden/arms/charge/"+c.Codepoints)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(rec.Body.String(), "\x89PNG") {
		t.Fatalf("charge: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, cp := range []string{"zzzz", "..%2Fheraldry.go", "1f600-0000"} {
		if rec := armsGet(t, mux, "/garden/arms/charge/"+cp); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", cp, rec.Code)
		}
	}
}

// GET /garden/arms/library is the renderer's library: every charge with its name and codepoints, and nothing of the shield
// (no places, no colours: the control is one row, heraldry 5.5b), with an ETag the browser revalidates with.
func TestArmsLibraryRoute(t *testing.T) {
	mux := armsMux(&Handler{})
	rec := armsGet(t, mux, "/garden/arms/library")
	if rec.Code != http.StatusOK {
		t.Fatalf("library: %d", rec.Code)
	}
	var lib armsLibrary
	if err := json.Unmarshal(rec.Body.Bytes(), &lib); err != nil {
		t.Fatal(err)
	}
	if len(lib.Charges) != len(heraldry.Library()) {
		t.Fatalf("%d charges", len(lib.Charges))
	}
	for _, c := range lib.Charges {
		if c.E == "" || c.N == "" || c.C == "" {
			t.Fatalf("a charge without its emoji, name or codepoints: %+v", c)
		}
	}
	if b := rec.Body.String(); strings.Contains(b, `"places"`) || strings.Contains(b, `"tinctures"`) {
		t.Errorf("the library still carries the shield's places or colours: %.200s", b)
	}
	if rec := armsGet(t, mux, "/garden/arms/library", "If-None-Match", rec.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", rec.Code)
	}
}

// GET /garden/arms/register lists the project's register as kd heraldry check reads it, as the viewer, with each bearer's
// short title and the plain-words name; a failed read is a 502 and a missing project a 400.
func TestArmsRegisterRoute(t *testing.T) {
	canonical, a := testArms(t)
	src := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{
		"gd-ArmsEpic01": {ID: "gd-ArmsEpic01", Title: "A long plan title", ShortTitle: "A plan", Type: "epic", Status: "open", Heraldry: canonical},
		"gd-ArmsEpic02": {ID: "gd-ArmsEpic02", Title: "No arms", Type: "epic", Status: "open"},
	}}
	mux := armsMux(&Handler{Beads: src})
	rec := armsGet(t, mux, "/garden/arms/register?project=gardenio")
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	if q := src.listQ; q.HeraldryProject != "gardenio" || len(q.Labels) != 1 || q.Labels[0] != "project:gardenio" || q.Limit != armsRegisterLimit {
		t.Errorf("query %+v", q)
	}
	var got struct {
		Project string       `json:"project"`
		Bearers []armsBearer `json:"bearers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Bearers) != 1 || got.Bearers[0] != (armsBearer{ID: "gd-ArmsEpic01", Title: "A plan", Arms: canonical, Name: a.PlainName()}) {
		t.Errorf("bearers %+v", got.Bearers)
	}
	if rec := armsGet(t, mux, "/garden/arms/register"); rec.Code != http.StatusBadRequest {
		t.Errorf("no project: %d", rec.Code)
	}
	src.failList = true
	if rec := armsGet(t, mux, "/garden/arms/register?project=gardenio"); rec.Code != http.StatusBadGateway {
		t.Errorf("daemon down: %d", rec.Code)
	}
	if rec := armsGet(t, armsMux(&Handler{}), "/garden/arms/register?project=gardenio"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no reader: %d", rec.Code)
	}
}

// The page inlines arms.js before workbench.js, which paints its section.
func TestPageCarriesArmsJS(t *testing.T) {
	i, j := strings.Index(pageHTML, `{{asset "arms.js"}}`), strings.Index(pageHTML, `{{asset "workbench.js"}}`)
	if i < 0 || j < 0 || i > j {
		t.Fatalf("arms.js must be inlined before workbench.js: %d, %d", i, j)
	}
	if !strings.Contains(pageWorkbenchJS, "ARMS.section(x)") {
		t.Error("the bead view no longer paints the Heraldry section")
	}
}

// arms.js's pure half runs under node: a run read as its charges (the longest library emoji first, a selector no
// difference), the register's keys and what it says about a draft row, the count and the plain-words name, the daemon's
// refusal said in words, the library search, the picture's width and the URLs. The library is the renderer's whole one, so
// a sequence whose first part is a charge of its own is read as it is stored.
func TestArmsLogicUnderNode(t *testing.T) {
	lib := heraldry.Library()
	charges := make([]armsLibraryCharge, 0, len(lib))
	for _, c := range lib {
		charges = append(charges, armsLibraryCharge{E: c.Emoji, N: c.Name, C: c.Codepoints})
	}
	libJSON, _ := json.Marshal(map[string]any{"charges": charges})
	// a charge that begins another charge (a ZWJ sequence on it, say), if the library has one: the run holds the longer
	long, short := -1, -1
	for i, c := range lib {
		for j, d := range lib {
			if i != j && len(d.Emoji) > len(c.Emoji) && strings.HasPrefix(strings.ReplaceAll(d.Emoji, "️", ""), strings.ReplaceAll(c.Emoji, "️", "")) {
				long, short = j, i
				break
			}
		}
		if long >= 0 {
			break
		}
	}
	runErr, _ := json.Marshal("invalid arms: arms are three to five emoji. " + heraldry.Help)
	stray := ""
	for _, e := range []string{"\u2705", "\U0001F600", "\U0001FAE8", "\u274C"} { // an emoji the library lacks
		if _, ok := heraldry.Lookup(e); !ok {
			stray = e
			break
		}
	}
	_, missing := heraldry.ParseRun(lib[0].Emoji + stray + lib[1].Emoji)
	if stray == "" || missing == nil {
		t.Fatalf("no emoji outside the library to refuse (%q)", stray)
	}
	missingErr, _ := json.Marshal(missing.Error())
	taken, _ := json.Marshal("the arms " + lib[0].Emoji + lib[1].Emoji + lib[2].Emoji + " are already borne by gd-Other (Plan B) in project gardenio; arms are unique in a project, so choose other emoji or another order")
	n := func(i int) string { b, _ := json.Marshal(lib[i].Name); return string(b) }
	script := `
var A = require(process.argv[1]), assert = require("assert");
var lib = A.index(` + string(libJSON) + `), L = lib.charges;
var e0 = L[0].e, e1 = L[1].e, e2 = L[2].e, e3 = L[3].e, e4 = L[4].e, e5 = L[5].e;

/* a run read as its charges, in order, spaces and a presentation selector no difference */
assert.deepStrictEqual(A.split(lib, e0 + e1 + e2), [e0, e1, e2]);
assert.deepStrictEqual(A.split(lib, " " + e0 + " " + e1 + "️" + e2 + " "), [e0, e1, e2]);
assert.deepStrictEqual(A.split(lib, ""), []);
assert.strictEqual(A.split(lib, e0 + "x" + e1), null, "a letter is no charge");
var long = ` + strconv.Itoa(long) + `, short = ` + strconv.Itoa(short) + `;
if (long >= 0) {
  assert.deepStrictEqual(A.split(lib, L[long].e + e0), [L[long].e, e0], "the longer charge, not its first part and a stray");
  assert.deepStrictEqual(A.split(lib, L[short].e + e0), [L[short].e, e0]);
}
L.forEach(function (c) { assert.deepStrictEqual(A.split(lib, c.e), [c.e], "every charge reads as itself: " + c.n); });
assert.strictEqual(A.run([e0, e1, e2]), e0 + e1 + e2);

/* the register: taken on the same run, a look-alike on the same emoji in another order; never itself */
assert.strictEqual(A.set([e1, e0 + "️", e2]), A.set([e0, e2, e1]), "the set forgets the order");
var reg = [
  { id: "gd-self", arms: e0 + e1 + e2 },
  { id: "gd-same", arms: e0 + e1 + e3 },
  { id: "gd-moved", arms: e3 + e0 + e1 },
  { id: "gd-other", arms: e0 + e1 + e4 },
  { id: "gd-broken", arms: "blue" }];
var c = A.collisions(lib, reg, "gd-self", [e0, e1, e3]);
assert.strictEqual(c.taken.id, "gd-same");
assert.deepStrictEqual(c.otherOrder.map(function (b) { return b.id; }), ["gd-moved"]);
assert.strictEqual(A.collisions(lib, reg, "gd-self", [e0, e1, e2]).taken, null, "the bead's own arms are no clash");
assert.strictEqual(A.collisions(lib, reg, "gd-same", [e0, e1, e3]).taken, null, "nor are the arms the bead already bears");
assert.deepStrictEqual(A.collisions(lib, reg, "gd-x", []), { taken: null, otherOrder: [] });
assert.strictEqual(A.collisions(lib, null, "gd-x", [e0, e1, e2]).taken, null);

/* the count in words, and the name as heraldry.PlainName says it */
assert.strictEqual(A.tally(0), "No emoji yet: pick three to five below, in order.");
assert.strictEqual(A.tally(1), "Pick two more: arms are three to five emoji.");
assert.strictEqual(A.tally(2), "Pick one more: arms are three to five emoji.");
assert.strictEqual(A.tally(3), "Three emoji. You may add up to two more.");
assert.strictEqual(A.tally(4), "Four emoji. You may add one more.");
assert.strictEqual(A.tally(5), "Five emoji, the most arms hold. Remove one to add another.");
assert.strictEqual(A.plainName(lib, [e0, e1, e2]), "arms: " + ` + n(0) + ` + ", " + ` + n(1) + ` + ", " + ` + n(2) + `);
assert.strictEqual(A.sentence("arms: x"), "Arms: x");

/* the daemon's refusals in words: the help line (which spells its example in emoji) dropped, each emoji named */
var pict = /\p{Extended_Pictographic}/u;
assert.strictEqual(A.words(lib, ` + string(runErr) + `), "Arms are three to five emoji.");
var w = A.words(lib, ` + string(missingErr) + `);
assert.strictEqual(w, "An emoji is not in the library.", w);
w = A.words(lib, ` + string(taken) + `);
assert.strictEqual(w, "The arms " + ` + n(0) + ` + ", " + ` + n(1) + ` + ", " + ` + n(2) + ` + " are already borne by gd-Other (Plan B) in project gardenio; arms are unique in a project, so choose other emoji or another order.");
assert.strictEqual(A.words(lib, e5 + "️ is used twice"), A.sentence(L[5].n) + " is used twice.");
assert.strictEqual(A.words(lib, "That is too long."), "That is too long.");
L.forEach(function (c) { assert.ok(!pict.test(A.words(lib, c.e + " is used twice")), "no emoji left: " + c.n); });

/* the search: a word of the name starting with each word asked, any case, in library order, capped */
var fruit = [{ e: "x1", n: "red apple" }, { e: "x2", n: "green apple" }, { e: "x3", n: "pineapple" }];
assert.deepStrictEqual(A.search(fruit, "APPLE", 0).list.map(function (c) { return c.n; }), ["red apple", "green apple"], "not pineapple");
assert.deepStrictEqual(A.search(fruit, "pine", 0).list.map(function (c) { return c.n; }), ["pineapple"]);
assert.deepStrictEqual(A.search([{ n: "musical keyboard" }, { n: "old key" }, { n: "key" }, { n: "monkey" }], "key", 0).list.map(function (c) { return c.n; }), ["key", "old key", "musical keyboard"], "the name itself, then whole words");
assert.deepStrictEqual(A.search(fruit, " apple  re ", 0).list.map(function (c) { return c.n; }), ["red apple"]);
var s = A.search(fruit, "apple", 1);
assert.deepStrictEqual([s.list.length, s.total], [1, 2]);
assert.strictEqual(A.search(fruit, "", 0).total, 3, "no words, every charge");
assert.strictEqual(A.charge(lib, e1 + "️").n, ` + n(1) + `);
assert.strictEqual(A.charge(lib, "nope"), null);

/* the picture's width (heraldry.Width) and the URLs */
assert.deepStrictEqual([A.width(3, 64), A.width(5, 64), A.width(3, 24), A.width(1, 64), A.width(0, 64)], [` +
		strconv.Itoa(heraldry.Width(3, 64)) + `, ` + strconv.Itoa(heraldry.Width(5, 64)) + `, ` + strconv.Itoa(heraldry.Width(3, 24)) + `, ` + strconv.Itoa(heraldry.Width(1, 64)) + `, 0]);
assert.strictEqual(A.svgSrc("/v", e0 + e1 + e2, true), "/v/garden/arms.svg?a=" + encodeURIComponent(e0 + e1 + e2) + "&theme=night");
assert.strictEqual(A.chargeSrc("", { c: "1f525" }), "/garden/arms/charge/1f525");
assert.deepStrictEqual([A.MIN, A.MAX], [` + strconv.Itoa(heraldry.MinRun) + `, ` + strconv.Itoa(heraldry.MaxRun) + `]);
`
	runNodeLogic(t, "arms.js", script)
}
