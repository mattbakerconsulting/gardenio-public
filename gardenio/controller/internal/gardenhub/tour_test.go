package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tour.js ranks and frames the chore tour (gd-S2Cn2dTsP0), so its contract runs under node on a real layout: the weeds and
// what waits on you are one list, heaviest first and at most five; a bead with two weeds is one stop naming both; an
// unlinked PR and anything off the map are left out; each stop offers only what applies, suggested first; a ping is
// addressed and says why; and each stop is a cursor jump, framed in the part of the board the card leaves free.
func TestTourUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]); global.WORLD = W; global.WEEDS = require(process.argv[2]); var B = require(process.argv[3]), TOUR = require(process.argv[4]), C = require(process.argv[5]), assert = require("assert");
var now = 1792000000, H = 3600, D = 86400;
var task = function (id, state, upd, extra) { var t = { id: id, title: "Step " + id, type: "task", state: state, priority: 2, project: "gardenio", updated: now - upd }; for (var k in extra || {}) t[k] = extra[k]; return t; };
var counts = function (n) { return { in_progress: n, review: 0, blocked: 0, open: 0, deferred: 0, closed: 0 }; };
var none = { id: "", title: "", status: "", counts: counts(0), tasks: [] };
var plan = function (id, project, phases, extra) {
  var p = { id: id, title: "Plan " + id, state: "building", status: "in_progress", project: project, priority: 2, created: now - 30 * D, updated: now - H, activity: now - H,
    phases: phases, unphased: none, counts: counts(1), open_decisions: 0, open_escalations: 0, needs_human: 0 };
  for (var k in extra || {}) p[k] = extra[k];
  return p;
};
var phase = function (id, tasks) { return { id: id, title: "Phase " + id, status: "open", counts: counts(tasks.length), tasks: tasks }; };
var doc = {
  plans: [
    plan("gd-P1", "gardenio", [phase("gd-ph1", [task("gd-t1", "in_progress", 13 * H, { assignee: "mo" }), task("gd-t2", "open", H, { project: "" }), task("gd-t3", "in_progress", H)])]),
    plan("gd-P2", "", [], { owner: "ann", unphased: { id: "", title: "", status: "", counts: counts(1), tasks: [task("gd-t4", "in_progress", 20 * H, { project: "" })] } }),
    plan("gd-P3", "gardenio", [phase("gd-ph3", [task("gd-t5", "open", H)])], { open_decisions: 1, needs_human: 1,
      waiting: [{ id: "gd-d1", title: "Pick a name", kind: "decision", state: "open", human: true, since: now - 3 * D }] })
  ],
  seeds: [{ id: "gd-S1", title: "Old draft", state: "frosted", status: "open", project: "gardenio", operator: "oidc:zed@example.com", priority: 2, created: now - 20 * D, updated: now - 8 * D, phases: 1, tasks: 0 }],
  piles: { orphan_mr_total: 2, orphan_mrs: [
    { url: "https://github.com/o/r/pull/77", iid: "77", title: "Fix the gate latch", repo: "o/r", why: "bead_closed", bead: "gd-gone", assignee: "flo", project: "gardenio", since: now - 2 * D },
    { url: "https://github.com/o/r/pull/78", iid: "78", title: "Stray", repo: "o/r", why: "no_bead", project: "gardenio", since: now - 9 * D }] },
  meta: { schema: 1, truncated: false, row_cap: 2500, rows: 12 }
};
var world = W.layout(doc, null, { now: now * 1000 }), ix = WEEDS.index(WEEDS.lint(doc, now), world);
var asks = [{ id: "gd-d1", title: "Pick a name", type: "decision", project: "gardenio", u: now - 3 * D }, { id: "gd-d9", title: "Loose ask", type: "decision", project: "", u: now - 5 * D }];
var o = { doc: doc, ix: ix, targets: B.targets(world, asks, W.TILE), world: world, asks: asks, now: now, me: "",
  at: function (id) { return W.atFor(world, null, id); } };
var S = TOUR.stops(o);

// one list, heaviest first: the Commons's Mailbox's ask (5 days), the plan's (3 days), the orphaned PR (weight 3, 2 days), the Commons
// plan (two tidiness weeds, 30 days old) and the wilted step; the fresher weeds and the unlinked PR are not on the tour
assert.deepStrictEqual(S.map(function (s) { return s.key; }), ["ask:home:/mailbox", "ask:gd-P3", "weed:gd-gone", "weed:gd-P2", "weed:gd-t4"]);
assert.strictEqual(S.length, TOUR.STOPS);
assert.ok(S.every(function (s, i) { return i === 0 || S[i - 1].score >= s.score; }), "heaviest first");
assert.ok(S.every(function (s) { return s.at && W.resolve(world, s.at).found; }), "every stop is on the map: " + JSON.stringify(S.map(function (s) { return s.at; })));
// a cursor walk (cursor grid spec 8): every stop is a cursor jump to where it is, the Commons's ask to its Mailbox (its home
// cell while the Mailbox holds nothing, 28.6)
var tree = C.tree(world, null, { now: now * 1000 });
assert.deepStrictEqual(S.map(function (s) { return s.cur; }), ["~/mailbox"].concat(S.slice(1).map(function (s) { return s.at; })));
assert.ok(S.every(function (s) { return C.pathFor(tree, s.cur); }), "every stop has a cursor path: " + JSON.stringify(S.map(function (s) { return [s.cur, C.pathFor(tree, s.cur)]; })));

var y = S[0];
assert.ok(y.mail === "home:/mailbox" && y.at === "~/mailbox" && y.answer === "" && y.bead === "" && y.title === "The Mailbox in the Commons", JSON.stringify(y));
assert.deepStrictEqual(y.whys, [["Waiting on you", "A decision or escalation that no plan carries waits on you; the oldest, “Loose ask”, for 5 days."]], "gd-d1 is the plan's, so the Commons's Mailbox counts gd-d9 only");
assert.deepStrictEqual(y.actions, ["answer"]);
var p3 = S[1];
assert.strictEqual(p3.answer, "gd-d1", "a plan's stop answers its oldest ask"); assert.strictEqual(p3.bead, "gd-P3");
assert.strictEqual(p3.whys[0][1], "A decision or escalation waits on you; the oldest, “Pick a name”, for 3 days.");

var orphan = S[2];
assert.strictEqual(orphan.bead, "gd-gone", "an orphaned PR's stop acts on its closed bead");
assert.strictEqual(orphan.at, "gardenio", "a closed bead is not on the map: the stop is its project");
assert.strictEqual(orphan.who, "flo"); assert.strictEqual(orphan.state, "closed");
assert.deepStrictEqual(orphan.actions, ["ping"], "a closed bead only takes a ping");
assert.strictEqual(TOUR.pingText(orphan), "@flo the chore tour in /garden stopped at gd-gone. Orphaned PR: PR #77 in o/r is still open, but its bead gd-gone is closed. Merge or close the PR, or reopen the bead. Could you see to it?");

var p2 = S[3];
assert.deepStrictEqual(p2.whys.map(function (w) { return w[0]; }), ["Plan without phases", "No project label"], "one stop names both of the bead's weeds");
assert.strictEqual(p2.at, "~/gd-P2"); assert.strictEqual(p2.who, "ann"); assert.strictEqual(p2.whoWord, "Owner");
assert.deepStrictEqual(p2.actions, ["ping", "adopt", "defer", "close"]);
assert.ok(/^@ann the chore tour in \/garden stopped at gd-P2\. Plan without phases: .* No project label: .* Could you tend it: post an update, hand it on, or say if it should be deferred or closed\?$/.test(TOUR.pingText(p2)), TOUR.pingText(p2));

var t4 = S[4];
assert.strictEqual(t4.rule, "wilt"); assert.strictEqual(t4.who, ""); assert.strictEqual(t4.whoWord, "Gardener");
assert.deepStrictEqual(t4.actions, ["adopt", "defer", "close"], "nobody to ping: adopt it");

// what applies: never ping or adopt your own; an ask is answered
assert.deepStrictEqual(TOUR.actions({ kind: "weed", rule: "wilt", who: "mo", state: "in_progress" }, "mo"), ["defer", "close"]);
assert.deepStrictEqual(TOUR.actions({ kind: "weed", rule: "wilt", who: "mo", state: "in_progress" }, "ann"), ["ping", "adopt", "defer", "close"]);
assert.deepStrictEqual(TOUR.actions({ kind: "weed", rule: "frosted", who: "zed", state: "open" }, ""), ["ping", "close", "defer"]);
assert.deepStrictEqual(TOUR.actions({ kind: "weed", rule: "unlabelled", who: "", state: "open" }, ""), ["adopt", "defer", "close"]);
assert.deepStrictEqual(TOUR.actions({ kind: "ask" }, ""), ["answer"]);
// a read-only viewer (gd-GuZ3tbnTFI) only looks: an ask is looked at, a weed offers nothing to do but is still a stop
assert.deepStrictEqual(TOUR.actions({ kind: "ask" }, "", true), ["look"]);
assert.deepStrictEqual(TOUR.actions({ kind: "weed", rule: "wilt", who: "mo", state: "in_progress" }, "ann", true), []);
var RO = TOUR.stops(Object.assign({}, o, { ro: true }));
assert.strictEqual(RO.length, S.length, "the same number of stops");
assert.ok(RO.every(function (s) { return s.kind === "ask" ? s.actions.join() === "look" : s.actions.length === 0; }), JSON.stringify(RO.map(function (s) { return [s.key, s.actions]; })));

// past the cap of five: the long-frosted seed is sixth; its owner is named without the oidc: scheme
var six = TOUR.stops(Object.assign({}, o, { targets: [] })).filter(function (s) { return s.bead === "gd-S1"; })[0];
assert.ok(six && six.who === "zed@example.com" && six.whoWord === "Owner", JSON.stringify(six));
assert.deepStrictEqual(six.actions, ["ping", "close", "defer"]);

// fewer than five things to tend: all of them; nothing: an empty tour
var quiet = { doc: { plans: [doc.plans[0]] }, now: now, me: "", asks: [] };
quiet.world = W.layout(quiet.doc, null, { now: now * 1000 }); quiet.ix = WEEDS.index(WEEDS.lint(quiet.doc, now), quiet.world);
quiet.targets = B.targets(quiet.world, [], W.TILE); quiet.at = function (id) { return W.atFor(quiet.world, null, id); };
assert.deepStrictEqual(TOUR.stops(quiet).map(function (s) { return s.key; }), ["weed:gd-t1", "weed:gd-t2"]);
assert.deepStrictEqual(TOUR.stops({ doc: {}, ix: null, targets: [], world: null, asks: [], now: now, at: function () { return null; } }), []);

// framing: the free strip is the larger side of the card, under the header and above the tab bar
var desk = { w: 1280, h: 800 }, card = { x: 420, y: 560, w: 440, h: 232 };
assert.deepStrictEqual(TOUR.free(desk, 60, card, 800), { x: 0, y: 60, w: 1280, h: 500 }, "a desktop frames above the card");
assert.deepStrictEqual(TOUR.free({ w: 390, h: 844 }, 100, { x: 0, y: 100, w: 390, h: 280 }, 775), { x: 0, y: 380, w: 390, h: 395 }, "a phone frames below the sheet, above the tab bar");
assert.deepStrictEqual(TOUR.free(desk, 60, null, 800), { x: 0, y: 60, w: 1280, h: 740 }, "no card: the board under the header");
// the free strip is the inset the cursor's camera frames in: a stop's grid lands inside it
var f = TOUR.free(desk, 60, card, 800), ins = TOUR.inset(desk, f);
assert.deepStrictEqual(ins, { top: 60, right: 0, bottom: 240, left: 0 });
assert.deepStrictEqual(TOUR.inset({ w: 390, h: 844 }, TOUR.free({ w: 390, h: 844 }, 100, { x: 0, y: 100, w: 390, h: 280 }, 775)), { top: 380, right: 0, bottom: 69, left: 0 });
var vp = { w: desk.w, h: desk.h, top: ins.top, right: ins.right, bottom: ins.bottom, left: ins.left };
S.forEach(function (s) {
  var p = C.pathFor(tree, s.cur), cam = C.frame(tree, p, vp), b = W.tiles(tree.nodes[p[p.length - 1]].rect);
  var s0 = W.toScreen(cam, desk, b.x, b.y), s1 = W.toScreen(cam, desk, b.x + b.w, b.y + b.h);
  assert.ok(s0.x >= f.x - 1 && s1.x <= f.x + f.w + 1 && s0.y >= f.y - 1 && s1.y <= f.y + f.h + 1, s.key + ": the stop is framed in the free strip: " + JSON.stringify([s0, s1, f]));
});
console.log("tour node checks passed");
`
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	out, err := exec.Command(node, "-e", script, abs(filepath.Join("web", "world.js")), abs(filepath.Join("web", "weeds.js")), abs(filepath.Join("web", "beacons.js")), abs(filepath.Join("web", "tour.js")), abs(filepath.Join("web", "cursor.js"))).CombinedOutput()
	if err != nil {
		t.Fatalf("node tour test failed: %v\n%s", err, out)
	}
}

// The tour is wired into the page: the script loads after weeds.js and beacons.js and before garden.js, Tend sits in
// the camera's Menu and controls the card, the card sits beside the board (so a drag or a map key never starts in
// it), garden.js mounts it as a cursor walk (a stop is a cursor jump; the card is an inset of the cursor's camera) with the
// shared post, and both the info card and the beacons keep off it.
func TestTourWired(t *testing.T) {
	_, rec := servedPage(t)
	body := inlined(t, rec.Body.String())
	b, tr, app := strings.Index(body, "var BEACONS = (function"), strings.Index(body, "var TOUR = (function"), strings.Index(body, "var SECS=['decision','work','agent']")
	if b < 0 || tr < 0 || app < 0 || b > tr || tr > app {
		t.Error("tour.js must load after beacons.js and before garden.js")
	}
	for _, want := range []string{
		`<button class="btn mi" id="cam-tend" type="button" aria-expanded="false" aria-controls="tour"`,
		`<section class="tour" id="tour" role="dialog" aria-labelledby="tour-n tour-h" hidden></section>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	if i, j := strings.Index(pageHTML, `id="tour"`), strings.Index(pageHTML, `<nav class="outline"`); i < j {
		t.Error("the tour card must sit outside the board, after it")
	}
	for _, want := range []string{
		"TOUR.mount(tourEl,document.getElementById('cam-tend'),{",
		"TOUR.stops({doc:farmDoc,ix:weedIx,targets:bcnTg,world:world,",
		"show:function(s){bcnAt=null;if(!s||!(cursorTo(s.cur)||cursorTo(s.at)))frameCursor('to');}",
		"t=tourEl.hidden?null:TOUR.measure(board,vp,hdrBottom());",
		"post:post,mail:function(zid){paneTo('mail',zid);}",
	} {
		if !strings.Contains(pageAppJS, want) {
			t.Errorf("garden.js missing %s", want)
		}
	}
	if !strings.Contains(pageTourJS, "return inset(vp, free(vp, top, ") {
		t.Error("tour.js must hand the cursor's camera the free rect as an inset")
	}
	if strings.Contains(pageTourJS, "tour-ring") || strings.Contains(pageCSS, ".tour-ring") || strings.Contains(pageAppJS, "flyTo(c,320)") {
		t.Error("the tour must not ring or fly on its own: the cursor's ring and camera do (cursor grid spec 8)")
	}
	if !strings.Contains(pageBeaconsJS, `".cam,.dbtns,.dw,.tabbar,.tour,.curbar"`) {
		t.Error("beacons must keep off the tour card and the cursor bar")
	}
	for _, want := range []string{".tour{position:absolute;", "@media (pointer:coarse){.tour .btn.small,.tour-open{min-height:44px;"} {
		if !strings.Contains(pageCSS, want) {
			t.Errorf("garden.css missing %s", want)
		}
	}
}

// The chore tour in a real Chromium (gd-S2Cn2dTsP0), on the weeds' farm: Tend starts a cursor walk of five stops, heaviest
// first; at each the cursor is on the stop and framed in the map the card leaves free (above the card on a desktop, below the
// sheet on a phone), and the card names why it needs tending. Ping, adopt, defer and close post to the viewer's endpoints (stood in for
// here, and recorded), close and defer only on a second tap; a refusal is said on the card in the viewer's own words (its JSON answer); the last stop ends on a summary
// and Done or Escape hands focus back to Tend with the cursor left on the last stop. What is asserted lives in testdata/tour_browser.js.
func TestTourUnderChromium(t *testing.T) {
	underChromium(t, "tour_browser.js", func() *httptest.Server {
		return weedsWorld(t, weedsNow, func(mux *http.ServeMux) {
			mux.HandleFunc("POST /b/{id}/{verb}", func(w http.ResponseWriter, r *http.Request) {
				if r.PathValue("id") == "gd-orph" {
					// The viewer's refusal as its write routes answer a JSON caller (respondAction, gd-jgVYg6kH5K). A post that
					// did not ask for JSON gets the HTML banner, which the tour no longer reads, so the card would say less.
					if r.Header.Get("Accept") != "application/json" {
						w.Header().Set("Content-Type", "text/html")
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`<p role="alert">You may not comment on gd-orph.</p>`))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"ok":false,"bead":"gd-orph","error":"You may not comment on gd-orph.","status":403}`))
					return
				}
				_ = r.ParseForm()
				w.Header().Set("X-Tour-Seen", r.PathValue("verb")+":"+r.PostForm.Get("text"))
				w.WriteHeader(http.StatusOK)
			})
		})
	}, weedsNowArg())
}
