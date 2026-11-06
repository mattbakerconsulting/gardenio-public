package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ListBeadsFiltered gives the bead view's fake the agent list the harvest block reads: its beads matching every type,
// status and field the query names.
func (f *fakeBeads) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listQ = q
	f.ctxs = append(f.ctxs, ctx)
	if f.failList {
		return nil, errors.New("daemon down")
	}
	in := func(v string, set []string) bool { return len(set) == 0 || slices.Contains(set, v) }
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		ok := in(b.Type, q.Types) && in(b.Status, q.Statuses)
		for k, v := range q.Fields {
			ok = ok && b.Fields[k] == v
		}
		if ok {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

// harvestStep adds gd-HarvestSt1 to the bead view's reader only (like stressBead, it is on no plot): a step closed 49
// hours after it was filed, with two UX grades (a re-grade), a report, a follow-up and a decision among its children, and
// two crews that claimed it (a follow-through crew whose wrap-up the controller synthesized when it reaped it), plus a
// crew of another step. Times count back from now so the goldens' frozen clock reads the same ages on every run.
func harvestStep(beads *fakeBeads, now time.Time) {
	t0 := now.Add(-72 * time.Hour)
	add := func(b *beadsapi.BeadDetail) { beads.beads[b.ID] = b }
	add(&beadsapi.BeadDetail{ID: "gd-HarvestSt1", Title: "2b.5b Plot rollup stamp", Type: "task", Status: "closed", Assignee: "crew-b",
		Labels: []string{"project:gardenio"}, Description: "Stamp each plot with its rollup once the phase closes.",
		CreatedAt: t0, UpdatedAt: t0.Add(49 * time.Hour), ClosedAt: t0.Add(49 * time.Hour).UTC().Format(time.RFC3339),
		Fields: map[string]string{"mr_url": "https://github.com/acme/garden/pull/825", "mr_state": "merged", "ci_state": "success",
			"tests_touched": "3", "goldens_touched": "0", "review_rounds": "2", "close_reason": "merged"}})
	add(&beadsapi.BeadDetail{ID: "gd-HarvestEv1", Title: "UX grade 4/5: cursorphase", Type: "evidence", Status: "closed",
		CreatedBy: "crew-a", CreatedAt: t0.Add(20 * time.Hour),
		Fields: map[string]string{"kind": "ux", "step": "gd-HarvestSt1", "subject": "cursorphase", "score": "4", "of": "5", "percent": "80",
			"urls": "https://garden.example/image/coop/sessions/images/a1.png\nnot a url\n"}})
	add(&beadsapi.BeadDetail{ID: "gd-HarvestEv2", Title: "UX grade 5/5: cursorphase, bead view", Type: "evidence", Status: "closed",
		CreatedBy: "crew-b", CreatedAt: t0.Add(47 * time.Hour),
		Fields: map[string]string{"kind": "ux", "step": "gd-HarvestSt1", "subject": "cursorphase, bead view", "score": "5", "of": "5", "percent": "100",
			"urls": "https://garden.example/image/coop/sessions/images/b1.png\nhttps://garden.example/image/coop/sessions/images/b2.png\n" +
				"https://garden.example/image/coop/sessions/images/b3.png\nhttps://garden.example/image/coop/sessions/images/b4.png",
			"pr": "https://github.com/acme/garden/pull/825"}})
	add(&beadsapi.BeadDetail{ID: "gd-HarvestRp1", Title: "Rollup stamp build notes", Type: "report", Status: "closed"})
	add(&beadsapi.BeadDetail{ID: "gd-HarvestFu1", Title: "Stamp spacing on a narrow plot", Type: "bug", Status: "open"})
	add(&beadsapi.BeadDetail{ID: "gd-HarvestDc1", Title: "Which stamp glyph", Type: "decision", Status: "closed"})
	for _, c := range []string{"gd-HarvestEv1", "gd-HarvestEv2", "gd-HarvestRp1", "gd-HarvestFu1", "gd-HarvestDc1"} {
		beads.deps = append(beads.deps, beadsapi.Dependency{BeadID: c, DependsOnID: "gd-HarvestSt1", Type: "parent-child"})
	}
	add(&beadsapi.BeadDetail{ID: "gd-HarvestAg1", Title: "crew-a", Type: "agent", Status: "closed", CreatedAt: t0,
		Fields: map[string]string{"task_id": "gd-HarvestSt1", "agent": "crew-a", "total_cost_usd": "0.19",
			"wrapup": `{"tldr":"Stamp lands on every plot whose phase closed; PR #825 merged after two rounds","accomplishments":"PR #825"}`}})
	add(&beadsapi.BeadDetail{ID: "gd-HarvestAg2", Title: "crew-b", Type: "agent", Status: "closed", CreatedAt: t0.Add(30 * time.Hour),
		Fields: map[string]string{"task_id": "gd-HarvestSt1", "agent": "crew-b", "live_cost_usd": "0.10",
			"wrapup": `{"tldr":"stale: reaped","accomplishments":"x","custom":{"synthesized":"true"}}`}})
	add(&beadsapi.BeadDetail{ID: "gd-HarvestAg3", Title: "other", Type: "agent", Status: "closed", CreatedAt: t0,
		Fields: map[string]string{"task_id": "gd-else", "total_cost_usd": "9"}})
}

func harvestFixture() (*fakeBeads, time.Time) {
	now := time.Unix(1_790_000_000, 0)
	f := beadFixture()
	harvestStep(f, now)
	return f, now.Add(-72 * time.Hour)
}

// A closed step's document carries its harvest (spec §6.1): every grade newest first, the MR with its forge number, the
// merge-time signals with a recorded zero kept, produce and follow-ups split from the other children, and the cost summed
// over every crew that claimed it, with the newest real wrap-up.
func TestBeadViewHarvest(t *testing.T) {
	f, t0 := harvestFixture()
	doc, err := readBead(context.Background(), f, "gd-HarvestSt1")
	if err != nil {
		t.Fatal(err)
	}
	h := doc.Harvest
	if h == nil {
		t.Fatal("no harvest block on a closed step")
	}
	if q := f.listQ; q.Fields["task_id"] != "gd-HarvestSt1" || !reflect.DeepEqual(q.Types, []string{"agent"}) || !reflect.DeepEqual(q.Statuses, produceStatuses) {
		t.Errorf("crew list query = %+v, want agent beads of every status with task_id=gd-HarvestSt1", q)
	}
	if h.Base != HarvestSheaf || h.Lead != 49*3600 || h.Closed != t0.Unix()+49*3600 {
		t.Errorf("base %q lead %d closed %d", h.Base, h.Lead, h.Closed)
	}
	if h.MR == nil || *h.MR != (harvestMR{URL: "https://github.com/acme/garden/pull/825", Number: "#825", State: "merged"}) {
		t.Errorf("mr = %+v", h.MR)
	}
	if h.CI != "success" || h.Tests == nil || *h.Tests != 3 || h.Goldens == nil || *h.Goldens != 0 || h.Rounds == nil || *h.Rounds != 2 {
		t.Errorf("ci %q tests %v goldens %v rounds %v: want success, 3, a recorded 0, 2", h.CI, h.Tests, h.Goldens, h.Rounds)
	}
	var ids []string
	for _, g := range h.Grades {
		ids = append(ids, g.ID)
	}
	if !reflect.DeepEqual(ids, []string{"gd-HarvestEv2", "gd-HarvestEv1"}) {
		t.Errorf("grades %v, want newest first", ids)
	}
	g := h.Grades[0]
	if g.Score != 5 || g.Of != 5 || g.Percent != 100 || g.By != "crew-b" || g.At != t0.Unix()+47*3600 || g.Subject != "cursorphase, bead view" ||
		g.PR != "https://github.com/acme/garden/pull/825" || len(g.URLs) != 4 {
		t.Errorf("newest grade = %+v", g)
	}
	if u := h.Grades[1].URLs; !reflect.DeepEqual(u, []string{"https://garden.example/image/coop/sessions/images/a1.png"}) {
		t.Errorf("older grade's urls = %v, want only the link", u)
	}
	if len(h.Produce) != 1 || h.Produce[0].ID != "gd-HarvestRp1" || len(h.FollowUps) != 1 || h.FollowUps[0].ID != "gd-HarvestFu1" || h.FollowUps[0].Status != "open" {
		t.Errorf("produce %+v follow-ups %+v", h.Produce, h.FollowUps)
	}
	if h.Crews != 2 || math.Abs(h.CostUSD-0.29) > 1e-9 {
		t.Errorf("cost $%v across %d crews, want $0.29 across 2 (the other step's crew left out)", h.CostUSD, h.Crews)
	}
	if !strings.HasPrefix(h.TLDR, "Stamp lands on every plot") || h.TLDRBy != "crew-a" {
		t.Errorf("tldr %q by %q: want the newest wrap-up that is not a reap's", h.TLDR, h.TLDRBy)
	}
	if len(h.Partial) != 0 {
		t.Errorf("partial = %v on a full read", h.Partial)
	}

	// over HTTP, and the crew read rides the request's context like the rest
	f.ctxs = nil
	req := httptest.NewRequest("GET", "/garden/bead/gd-HarvestSt1", nil)
	req = req.WithContext(context.WithValue(req.Context(), viewerKey{}, "me"))
	rec := httptest.NewRecorder()
	beadMux(f).ServeHTTP(rec, req)
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); rec.Code != 200 || err != nil || d["harvest"] == nil {
		t.Fatalf("GET: %d %v, harvest %v", rec.Code, err, d["harvest"])
	}
	if b, _ := json.Marshal(d["harvest"].(map[string]any)["mr"]); string(b) != `{"number":"#825","state":"merged","url":"https://github.com/acme/garden/pull/825"}` {
		t.Errorf("mr json = %s", b)
	}
	for _, c := range f.ctxs {
		if c.Value(viewerKey{}) != "me" {
			t.Fatal("a daemon read did not carry the request context")
		}
	}
}

// Only a closed work bead has a harvest; a failed crew read says so instead of a $0 cost.
func TestBeadViewHarvestAbsentAndPartial(t *testing.T) {
	f, _ := harvestFixture()
	for _, id := range []string{"gd-task", "gd-HarvestDc1", "gd-HarvestEv1"} { // open work, a closed decision, an evidence bead
		doc, err := readBead(context.Background(), f, id)
		if err != nil {
			t.Fatal(err)
		}
		if doc.Harvest != nil {
			t.Errorf("%s has a harvest block: %+v", id, doc.Harvest)
		}
	}
	f.failList = true
	doc, err := readBead(context.Background(), f, "gd-HarvestSt1")
	if err != nil {
		t.Fatal(err)
	}
	if h := doc.Harvest; h == nil || !reflect.DeepEqual(h.Partial, []string{"crews"}) || h.Crews != 0 || h.CostUSD != 0 || len(h.Grades) != 2 {
		t.Errorf("crew read down: %+v, want the rest with partial [crews]", h)
	}
}

// The base word follows the cell's rule (spec §4.1), first match wins.
func TestHarvestBase(t *testing.T) {
	for _, c := range []struct {
		reason, mr string
		yield      bool
		want       string
	}{
		{"Superseded by gd-x", "merged", true, HarvestTrampled},
		{"wontfix", "", false, HarvestTrampled},
		{"", "closed", false, HarvestTrampled},
		{"", "closed", true, HarvestSheaf},
		{"", "merged", false, HarvestSheaf},
		{"done", "", true, HarvestSheaf},
		{"done", "opened", false, HarvestBare},
		{"", "", false, HarvestBare},
	} {
		if got := harvestBase(c.reason, c.mr, c.yield); got != c.want {
			t.Errorf("harvestBase(%q, %q, %v) = %q, want %q", c.reason, c.mr, c.yield, got, c.want)
		}
	}
	for u, want := range map[string]string{
		"https://github.com/o/r/pull/825":             "#825",
		"https://gitlab.com/g/p/-/merge_requests/12/": "!12",
		"https://example.com/nothing":                 "",
	} {
		if got := mrNumber(u, ""); got != want {
			t.Errorf("mrNumber(%q) = %q, want %q", u, got, want)
		}
	}
}

// web/workbench.js says the harvest in words (spec §4.2, §6.2): the badges in order with nothing for an unlit one, the signals
// no badge says with a recorded zero said and an unrecorded one left out, lead time and cost (an unread cost is never $0), and pictures
// only through the image proxy.
func TestHarvestLogicUnderNode(t *testing.T) {
	script := `
var V = require(process.argv[1]), assert = require("assert");
var h = { base: "sheaf", lead: 49 * 3600, mr: { url: "u", number: "#825", state: "merged" }, ci: "success", tests: 3, goldens: 2, rounds: 2,
  grades: [{ id: "e2", kind: "ux", score: 5, of: 5, urls: ["a", "b"] }, { id: "e1", kind: "ux", score: 4, of: 5, urls: ["c"] }], cost_usd: 0.29, crews: 2 };
function words(x) { return V.badges(x).map(function (b) { return b.k + ":" + b.w; }); }
assert.deepStrictEqual(words(h), ["crate:merged #825", "rosette:graded 5/5 gates", "photo:4 pictures", "vial:3 tests", "lantern:checks green", "seal:merged after 2 rounds"]);
assert.deepStrictEqual(words({ mr: { state: "opened" }, ci: "failed", tests: 0, rounds: 1 }), ["seal:merged first time"], "an open MR, failed checks and no tests light nothing");
assert.deepStrictEqual(words({ grades: [{ kind: "other", score: 1, of: 1 }] }), [], "only a ux grade is the rosette");
assert.deepStrictEqual(words({ goldens: 1 }), ["photo:1 picture"]);
assert.deepStrictEqual(words(null), []);
assert.deepStrictEqual(V.signals(h), ["2 goldens touched"], "what a lit badge says is not said again");
assert.deepStrictEqual(V.signals({ ci: "failed", tests: 0, goldens: 0 }), ["checks failed", "no tests touched", "no goldens touched"], "a recorded zero is said");
assert.deepStrictEqual(V.signals({ ci: "running" }), ["checks running"]);
assert.deepStrictEqual(V.signals({}), [], "nothing recorded, nothing said");
assert.deepStrictEqual(V.metrics(h), ["closed in 2d", "$0.29 across 2 crews"]);
assert.deepStrictEqual(V.metrics({ lead: 1800, cost_usd: 0.1, crews: 1 }), ["closed in 30m", "$0.10 across 1 crew"]);
assert.deepStrictEqual(V.metrics({ lead: 5 * 3600, partial: ["crews"] }), ["closed in 5h", "cost not read"], "an unread cost is not $0");
assert.deepStrictEqual(V.metrics({}), []);
assert.strictEqual(V.baseWord("sheaf"), "harvested");
assert.strictEqual(V.baseWord("trampled"), "abandoned");
assert.strictEqual(V.baseWord("bare"), "closed");
assert.strictEqual(V.picSrc("/v", "https://garden.example/image/coop/sessions/images/a%20b.png"), "/v/image/coop/sessions/images/a%20b.png");
assert.strictEqual(V.picSrc("", "https://garden.example/v/image/k.png"), "/image/k.png", "the uploader's base path is the page's");
assert.strictEqual(V.picSrc("", "https://example.com/pic.png"), "", "not the proxy: a plain link");
assert.strictEqual(V.picSrc("/v", "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/images/2026-10-25/c258478a-after-barn-mid.png"), "/v/image/coop/sessions/images/2026-10-25/c258478a-after-barn-mid.png", "the uploader's raw storage location is the same key through the proxy");
assert.strictEqual(V.picSrc("", "https://gardenio-sessions.s3.us-west-2.amazonaws.com/coop/sessions/images/2026-10-25/a%20b.png"), "/image/coop/sessions/images/2026-10-25/a%20b.png", "a regional bucket host too");
assert.strictEqual(V.picSrc("", "https://gardenio-sessions.s3.amazonaws.com/coop/sessions/audit/x.png"), "", "a bucket key outside the uploader's images layout stays a plain link");
assert.strictEqual(V.picSrc("", "https://evil.example/s3.amazonaws.com/images/x.png"), "", "only a bucket host");
assert.strictEqual(V.picSrc("", "https://garden.example/image/"), "", "no key");
assert.strictEqual(V.picSrc("", "javascript:alert(1)//image/x"), "");
assert.strictEqual(V.picSrc("", "not a url"), "");
`
	runNodeLogic(t, "workbench.js", script)
}

// fixtureImage stands in for the viewer's /image/ proxy (cmd/beads-viewer) in the browser fixture: a PNG, a sky over a field
// with a band whose shade comes from the key, so a grade's thumbnails are pictures and tell apart. It is 160x120, or the
// WxH a key names (p3-day-1280x800.png), for a report's screenshots at their real sizes. A key whose file name starts
// "missing-" is a 404, the picture the store no longer has (the rubric's F6, ux_fixture_test.go).
func fixtureImage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if strings.HasPrefix(path.Base(key), "missing-") {
		http.NotFound(w, r)
		return
	}
	var h uint8
	for i := 0; i < len(key); i++ {
		h = h*31 + key[i]
	}
	iw, ih := 160, 120
	if m := fixtureImageSize.FindStringSubmatch(key); m != nil {
		iw, ih = atoiOr(m[1], iw), atoiOr(m[2], ih)
	}
	img := image.NewRGBA(image.Rect(0, 0, iw, ih))
	for y := 0; y < ih; y++ {
		for x := 0; x < iw; x++ {
			c := color.RGBA{0xbf, 0xe1, 0xee, 0xff} // sky
			switch {
			case y > ih*7/12:
				c = color.RGBA{0x5f, 0xae, 0x3e, 0xff} // field
			case y > ih/3 && x > iw/8 && x < iw*7/8:
				c = color.RGBA{h, 0x60, 0xff - h, 0xff} // a panel, its shade the key's
			}
			img.Set(x, y, c)
		}
	}
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, img)
}

var fixtureImageSize = regexp.MustCompile(`(\d{2,4})x(\d{2,4})\.png$`)

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

// The Harvest section in a real Chromium (spec §8 step 3): first on a closed step, above Description, and absent on an
// open one, desktop and phone, light and dark. What is asserted lives in testdata/harvest_browser.js.
func TestHarvestUnderChromium(t *testing.T) {
	underChromium(t, "harvest_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
