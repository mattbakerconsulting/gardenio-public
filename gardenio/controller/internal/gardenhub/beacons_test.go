package gardenhub

import (
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// beacons.js is pure maths plus a small renderer, so its contract runs under node (gd-gtr7qBCn9z). runBeaconsNode runs one
// part of it: beaconsPrelude, then script, with beacons.js, world.js and the fixture farm as process.argv[1..3].
func runBeaconsNode(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	out, err := exec.Command(node, "-e", beaconsPrelude+script, abs(filepath.Join("web", "beacons.js")), abs(filepath.Join("web", "world.js")), abs(filepath.Join("testdata", "farm_fixture.json"))).CombinedOutput()
	if err != nil {
		t.Fatalf("node beacons test failed: %v\n%s", err, out)
	}
}

const beaconsPrelude = `
var B = require(process.argv[1]), W = require(process.argv[2]), assert = require("assert");
var T = 16, vp = { w: 1280, h: 720 }, cam = { cx: 0, cy: 0, z: 1 };
function tg(id, x, y, n, g) { return { id: id, kind: "plan", name: "Plan " + id, count: n || 1, glyph: g || "?", rect: { x: x, y: y, w: 64, h: 48 } }; }
function inside(a, box) { return a.x - a.size / 2 >= 0 && a.y - a.size / 2 >= 0 && a.x + a.size / 2 <= box.w && a.y + a.size / 2 <= box.h; }
function sq(a, pad) { var h = a.size / 2 + (pad || 0); return { x: a.x - h, y: a.y - h, w: 2 * h, h: 2 * h }; }
`

// What needs you becomes a target: a plan waiting on a human; for an ask no plan carries, its project's Mailbox, or its
// project's Escalations cell when the ask is an escalation.
func TestBeaconsTargetsUnderNode(t *testing.T) {
	runBeaconsNode(t, `
// targets: plans that wait on a human, with ! when one of their human asks is an escalation; for the rest, each project's
// Mailbox, the Commons's for an ask with no project or whose project has no biome (28.6)
var world = { biomes: [{ name: "gardenio", plots: [
  { id: "p1", x: 10, y: 4, w: 7, h: 5, needs: 2, plan: { title: "Beacons", waiting: [{ id: "d1", kind: "decision", human: true }, { id: "e1", kind: "escalation", human: true }] } },
  { id: "p2", x: 30, y: 4, w: 7, h: 5, needs: 0, plan: { title: "Quiet", waiting: [{ id: "e2", kind: "escalation", human: false }] } },
  { id: "p3", x: 50, y: 4, w: 7, h: 5, needs: 1, plan: { title: "Older doc", open_decisions: 1 } }] }] };
var mbox = function (key, x) { return { id: "project:" + key, key: key, name: key || "Commons", home: { id: "home:" + key, cells: { mailbox: { x: x, y: -8, w: 10, h: 12 } } } }; };
world.index = { "project:": mbox("", -12), "project:gardenio": mbox("gardenio", 20) };
var ts = B.targets(world, [{ id: "d1", type: "decision", project: "gardenio" }, { id: "e2", type: "escalation", project: "gardenio" }, { id: "d9", type: "decision", project: "Gardenio" },
  { id: "d8", type: "decision", project: "nowhere" }, { id: "e9", type: "escalation", project: "" }], T);
assert.deepStrictEqual(ts.map(function (t) { return [t.id, t.kind, t.count, t.glyph, t.name, t.asks]; }), [["p1", "plan", 2, "!", "Beacons", undefined], ["p3", "plan", 1, "?", "Older doc", undefined],
  ["home:gardenio/mailbox", "mailbox", 1, "?", "the Mailbox in gardenio", ["d9"]], ["home:/mailbox", "mailbox", 1, "?", "the Mailbox in the Commons", ["d8"]]],
  "d1 and e2 are carried by plans (e2 waits on its executor, not you), so gardenio's Mailbox beacons only d9; d8's project has no biome, so the Commons's has it; e9 is the Escalations cell's (28.13), not a letter");
assert.deepStrictEqual(ts[0].rect, { x: 160, y: 64, w: 112, h: 80 }, "plot tiles to world px");
assert.deepStrictEqual(ts[2].rect, { x: 320, y: -128, w: 160, h: 192 }, "the Mailbox cell to world px");
assert.strictEqual(B.targets(world, [{ id: "d1", type: "decision" }], T).length, 2, "no Mailbox beacon when every ask is in a plan");
assert.deepStrictEqual(B.targets(null, [], T), [], "no world, no asks");
// a home strip's Escalations (28.13): one ! for its escalations waiting on you that no plan carries, each its cell for the N
// walk; an escalation is never the Mailbox's
var hw = W.layout({ escalations: { open: [{ id: "e1", project: "gardenio", state: "pending", human: true, since: 1 }, { id: "e5", project: "gardenio", state: "pending", human: true, since: 2 },
  { id: "e6", project: "gardenio", state: "approved", since: 3 }] }, plans: [] }, null, { now: 1, readonly: true });
hw.biomes[0].plots = world.biomes[0].plots;
var et = B.targets(hw, [{ id: "e5", type: "escalation", project: "gardenio" }], T), ez = hw.index["home:gardenio/escalation"];
assert.deepStrictEqual(et.map(function (t) { return [t.id, t.kind, t.count, t.glyph, t.asks]; }), [["p1", "plan", 2, "!", undefined], ["p3", "plan", 1, "?", undefined], ["home:gardenio/escalation", "escalation", 1, "!", ["esc:e5"]]],
  "e1 is p1's and e6 waits on its executor, so the cell's arrow is e5's alone; no Mailbox arrow for an escalation");
assert.deepStrictEqual(et[2].rect, { x: ez.x * T, y: ez.y * T, w: ez.w * T, h: ez.h * T });
`)
}

// An on-screen target has no arrow, an off-screen one has an arrow on the edge pointing at it, arrows that would touch merge
// with a summed count, arrows keep off the map's controls, and every arrow says in words how many, which way and what.
func TestBeaconsLayoutUnderNode(t *testing.T) {
	runBeaconsNode(t, `
// on screen: no arrow; just off the east edge: one arrow on the right edge, pointing east
assert.deepStrictEqual(B.layout([tg("a", 100, 100)], cam, vp), [], "a target in view has no arrow");
var east = B.layout([tg("a", 2000, -24)], cam, vp);
assert.strictEqual(east.length, 1);
var a = east[0];
assert.strictEqual(a.dir, "east"); assert.ok(Math.abs(a.angle) < 5, "points east: " + a.angle);
assert.strictEqual(a.x, vp.w - B.SIZE / 2 - B.MARGIN, "on the right edge"); assert.ok(Math.abs(a.y - vp.h / 2) < 2, "level with the target");
assert.strictEqual(a.key, "plan:a"); assert.strictEqual(a.count, 1); assert.strictEqual(a.glyph, "?");
assert.strictEqual(B.label(a), "1 needs you to the east: Plan a. Fly to Plan a.");
// a sliver at the edge is not "seen"; most of the target is
assert.strictEqual(B.layout([tg("s", 640 - 10, 0)], cam, vp).length, 1, "10 px showing is off-screen");
assert.strictEqual(B.layout([tg("s", 640 - 40, 0)], cam, vp).length, 0, "40 px showing is on screen");
// a target smaller than MIN_SEEN all in view is on screen, though its edges round: the Commons's Mailbox at the phone's World level
assert.strictEqual(B.onScreen({ x: -416, y: -448, w: 160, h: 192 }, { cx: 3233.5, cy: 1142.1953125, z: 0.04 }, { w: 390, h: 844 }, 104), true, "a 6.4 px target in view");
// under the header is not on screen
assert.strictEqual(B.layout([tg("h", 0, -360)], cam, vp, { top: 60 }).length, 1, "a plot hidden by the header gets an arrow");

// each compass direction, and the angle points from the arrow at the target
[[3000, 0, "east"], [3000, 3000, "south-east"], [0, 3000, "south"], [-3000, 3000, "south-west"], [-3000, 0, "west"], [-3000, -3000, "north-west"], [0, -3000, "north"], [3000, -3000, "north-east"]].forEach(function (c) {
  var g = B.layout([tg("c", c[0] - 32, c[1] - 24)], { cx: 0, cy: 0, z: 0.5 }, { w: 800, h: 800 });
  assert.strictEqual(g.length, 1); assert.strictEqual(g[0].dir, c[2], c.join(","));
});

// several in one direction merge, nearest first, with the summed count and ! if any is an escalation
var two = B.layout([tg("far", 6000, 0, 2), tg("near", 3000, 10, 1, "!"), tg("west", -3000, 0, 3)], cam, vp);
assert.strictEqual(two.length, 2, "east pair merges, west stays apart");
var e = two.filter(function (g) { return g.dir === "east"; })[0];
assert.deepStrictEqual(e.targets.map(function (t) { return t.id; }), ["near", "far"], "nearest first: the one it flies to");
assert.strictEqual(e.count, 3); assert.strictEqual(e.glyph, "!"); assert.strictEqual(e.key, "plan:near");
assert.strictEqual(B.label(e), "3 need you to the east: Plan near and Plan far (2). Fly to Plan near.");
var many = B.layout([1, 2, 3, 4, 5].map(function (i) { return tg("m" + i, 3000 * i, 0); }), cam, vp)[0];
assert.ok(/: Plan m1, Plan m2, Plan m3 and 2 more\. Fly to Plan m1\.$/.test(B.label(many)), B.label(many));
// clockwise from the top-left, so Tab walks round the edge
var ring = B.layout([tg("s", -32, 3000), tg("n", -32, -3000), tg("w", -3000, -24), tg("e", 3000, -24)], cam, vp);
assert.deepStrictEqual(ring.map(function (g) { return g.dir; }), ["north", "east", "south", "west"]);

// perimeter maths round-trips
var box = B.edgeBox(vp, 0, 36, []);
[0, 100, 1000, 1500, 2500, 3200].forEach(function (t) { var p = B.atT(box, t); assert.ok(Math.abs(B.toT(box, p.x, p.y) - t) < 1e-6, "t " + t); });

// keep off the controls: an arrow that would sit on the camera group slides along the edge, clear of it
var camRect = { x: 900, y: 8, w: 372, h: 40 };
var up = B.layout([tg("u", 1000, -3000)], { cx: 1050, cy: 0, z: 1 }, vp, { blocks: [camRect] });
assert.strictEqual(up.length, 1);
assert.ok(!W.hit(sq(up[0], B.GAP - 0.01), camRect), "the arrow keeps off the camera buttons: " + JSON.stringify(up[0]));
// a full-width bar at the bottom (the phone tab bar) lifts the bottom edge
var bar = { x: 0, y: 650, w: 1280, h: 70 }, down = B.layout([tg("d", -32, 3000)], cam, vp, { blocks: [bar] })[0];
assert.strictEqual(down.dir, "south"); assert.ok(down.y + down.size / 2 <= bar.y - B.MARGIN, "above the tab bar: " + down.y);
// a bar stacked on it (the title bar, then another bar above it) lifts it again while the map above keeps room for an arrow
// (gd-GIhQagxxTN); one that would leave less stays a block, which the arrows slide off: never on the dock's heading
var pv = { w: 390, h: 844 }, tabs = { x: 0, y: 775, w: 390, h: 69 }, barRow = { x: 0, y: 700, w: 390, h: 75 }, dock = { x: 8, y: 580, w: 374, h: 117 };
assert.strictEqual(B.edgeBox(pv, 100, 44, [tabs, barRow, dock]).y1, dock.y - 22 - B.MARGIN, "portrait: the edge runs along the dock's top");
assert.strictEqual(B.edgeBox(pv, 100, 44, [dock, tabs]).y1, tabs.y - 22 - B.MARGIN, "a bar not stacked on the edge lifts nothing");
var lv = { w: 647, h: 340 }, lbar = { x: 0, y: 280, w: 647, h: 60 }, ldock = { x: 8, y: 130, w: 570, h: 147 };
assert.strictEqual(B.edgeBox(lv, 56, 44, [lbar, ldock]).y1, lbar.y - 22 - B.MARGIN, "landscape: too little map above the dock to lift to it");
B.layout([tg("w", -3000, 0), tg("sw", -3000, 3000), tg("s", 0, 3000)], { cx: 0, cy: 0, z: 1 }, lv, { size: 44, top: 56, blocks: [lbar, ldock] }).forEach(function (g) {
  assert.ok(!W.hit(sq(g, B.GAP - 0.01), ldock), "landscape: the arrow keeps off the upper bar: " + JSON.stringify([g.x, g.y, g.dir]));
});
// a block hugging the right edge (the Escalations cell's items at a phone's edge, gd-WzYgtbwMT7) slides the arrow along that
// edge, past its nearer end, and off it
var cell = { x: 338, y: 335, w: 52, h: 210 }, side = B.layout([tg("e", 300, -124)], { cx: 0, cy: 0, z: 1 }, pv, { size: 44, top: 100, blocks: [cell] })[0];
assert.ok(W.hit(sq(B.layout([tg("e", 300, -124)], { cx: 0, cy: 0, z: 1 }, pv, { size: 44, top: 100 })[0]), cell), "unblocked, it would sit on the cell");
assert.strictEqual(side.x, pv.w - 22 - B.MARGIN, "still on the right edge"); assert.strictEqual(side.dir, "east");
assert.ok(!W.hit(sq(side, B.GAP - 0.01), cell) && side.y < cell.y + cell.h / 2, "above the cell, its nearer end: " + JSON.stringify([side.x, side.y]));
// every edge covered (an open phone drawer): no arrows at all
assert.deepStrictEqual(B.layout([tg("x", 3000, 0)], cam, vp, { blocks: [{ x: 0, y: 0, w: 1280, h: 720 }] }), []);
// a board too small for an arrow
assert.deepStrictEqual(B.layout([tg("x", 3000, 0)], cam, { w: 60, h: 60 }), []);
`)
}

// Arrows never overlap each other or leave the board, and an arrow over a framed Thing insets the cursor's camera.
func TestBeaconsFuzzUnderNode(t *testing.T) {
	runBeaconsNode(t, `
// fuzz: on a phone and a desktop, with the real controls' rects, arrows stay on the board, off the controls, never overlap
var seed = 7; function rnd() { seed = (seed * 1103515245 + 12345) % 2147483648; return seed / 2147483648; }
[[{ w: 390, h: 844 }, 44, 100, [{ x: 338, y: 330, w: 44, h: 300 }, { x: 286, y: 694, w: 96, h: 66 }, { x: 0, y: 775, w: 390, h: 69 }]],
 [{ w: 1280, h: 720 }, 36, 52, [{ x: 760, y: 60, w: 512, h: 32 }, { x: 8, y: 60, w: 90, h: 36 }, { x: 8, y: 676, w: 140, h: 36 }, { x: 1112, y: 602, w: 160, h: 110 }]]].forEach(function (c) {
  for (var run = 0; run < 60; run++) {
    var list = [];
    for (var i = 0; i < 12; i++) list.push(tg("f" + i, (rnd() - 0.5) * 20000, (rnd() - 0.5) * 20000, 1 + Math.floor(rnd() * 3), rnd() < 0.3 ? "!" : "?"));
    var gs = B.layout(list, { cx: 0, cy: 0, z: 0.2 + rnd() }, c[0], { size: c[1], top: c[2], blocks: c[3] }), n = 0;
    gs.forEach(function (g, k) {
      n += g.targets.length;
      assert.strictEqual(g.size, c[1]);
      assert.ok(inside(g, c[0]) && g.y - g.size / 2 >= c[2], "on the board, under the header: " + JSON.stringify([g.x, g.y]));
      c[3].forEach(function (r) { if (r.w < c[0].w * 0.6) assert.ok(!W.hit(sq(g, B.GAP - 0.01), r), "off control " + JSON.stringify(r) + " at " + [g.x, g.y]); });
      gs.forEach(function (h, j) { if (j > k) assert.ok(!W.hit(sq(g), sq(h)), "arrows overlap: " + [g.x, g.y, h.x, h.y]); });
      assert.strictEqual(g.count, g.targets.reduce(function (s, t) { return s + t.count; }, 0));
    });
    assert.ok(n <= list.length);
  }
});

// inset: an arrow over a framed Thing claims its nearest side out to its far edge and a GAP (the cursor's camera fits inside
// it, gd-SWjiYouIQt); an arrow on the grass between Things claims nothing
var ib = { w: 400, h: 800 }, ic = { cx: 200, cy: 400, z: 1 }, ia = function (x, y) { return { x: x, y: y, size: 44 }; };
assert.deepStrictEqual(B.inset([ia(36, 400), ia(364, 300), ia(200, 764)], ic, ib, [{ x: 0, y: 0, w: 400, h: 800 }]), { top: 0, right: 62, bottom: 62, left: 62 });
assert.deepStrictEqual(B.inset([ia(36, 400), ia(200, 120)], ic, ib, [{ x: 0, y: 0, w: 400, h: 800 }]), { top: 146, right: 0, bottom: 0, left: 62 }, "a top arrow under the header claims down to its far edge");
assert.strictEqual(B.inset([ia(36, 400)], ic, ib, [{ x: 100, y: 0, w: 300, h: 800 }, { x: 0, y: 0, w: 400, h: 300 }]), null, "on the grass between Things: no band");
assert.strictEqual(B.inset([ia(36, 400)], { cx: 200, cy: 400, z: 2 }, ib, [{ x: 140, y: 300, w: 120, h: 200 }]), null, "the rect is in world px, scaled by the camera");
`)
}

// On the fixture farm every plot that needs you is a target, and plotOf finds the plot an ask is on.
func TestBeaconsFarmUnderNode(t *testing.T) {
	runBeaconsNode(t, `
// a laid-out fixture farm: every plot that needs you is a target, at its plot
var fx = JSON.parse(require("fs").readFileSync(process.argv[3], "utf8")), lw = W.layout(fx, null, {});
var need = []; lw.biomes.forEach(function (b) { b.plots.forEach(function (p) { if (p.needs > 0) need.push(p.id); }); });
assert.ok(need.length > 0, "the fixture farm has a plan that needs you");
assert.deepStrictEqual(B.targets(lw, [], T).map(function (t) { return t.id; }), need);

// plotOf: the Mailbox's "Open on the map" (gd-kNV6Eos4ON). An ask a plan's waiting list carries is that plan's plot; a staked
// plot's or a Nursery pot's plan decision is that plot or pot; a wait past the list's cap is found through the bed holding it;
// an ask no plan carries has no plot
var po = { biomes: [{ name: "gardenio", plots: [
  { id: "pl1", kind: "plot", plan: { title: "Releases", waiting: [{ id: "e1" }] } },
  { id: "pl2", kind: "plot", staked: true, seed: { title: "Staked", decision: { id: "d2" } }, plan: { title: "Staked out at length", short_title: "staked short", waiting: [] } }],
  nursery: [{ id: "pot3", kind: "seed", seed: { title: "Draft", decision: { id: "d3" } } }] }],
  index: { pl1: { id: "pl1", kind: "plot", plan: { title: "Releases" } }, t9: { id: "t9", kind: "task", plot: "pl1" } } };
assert.deepStrictEqual(B.plotOf(po, "e1"), { id: "pl1", kind: "plot", title: "Releases", short: "Releases" });
assert.deepStrictEqual(B.plotOf(po, "d2"), { id: "pl2", kind: "plot", title: "Staked out at length", short: "staked short" }, "a short title names it short");
assert.deepStrictEqual(B.plotOf(po, "d3"), { id: "pot3", kind: "seed", title: "Draft", short: "Draft" });
assert.deepStrictEqual(B.plotOf(po, "t9"), { id: "pl1", kind: "plot", title: "Releases", short: "Releases" }, "past the waiting cap: the plot whose bed holds it");
assert.strictEqual(B.plotOf(po, "d9"), null); assert.strictEqual(B.plotOf(po, ""), null); assert.strictEqual(B.plotOf(null, "e1"), null);
var waited = 0;
lw.biomes.forEach(function (b) { b.plots.forEach(function (p) { ((p.plan && p.plan.waiting) || []).forEach(function (x) {
  waited++; assert.strictEqual(B.plotOf(lw, x.id).id, p.id, x.id + " is on " + p.id);
  assert.ok(W.atFor(lw, "plot", p.id), "a plot the Mailbox points at has an at= address");
}); }); });
assert.ok(waited > 0, "the fixture farm has a plan with an open ask");

// keepOff (gd-WzYgtbwMT7): a cell holding items in view is one board rect from its top-left corner to its last item, clipped to
// the board under the header; a cell with none, or out of view, is none
var fe = JSON.parse(JSON.stringify(fx));
fe.escalations = { open: [1, 2, 3, 4, 5, 6, 7].map(function (i) { return { id: "gd-Esc" + i, kind: "escalation", state: "pending", human: true, title: "Esc " + i, project: "gardenio" }; }), total: 7, by_project: { gardenio: 7 } };
lw = W.layout(fe, null, {});
var gz = lw.biomes.filter(function (b) { return b.key === "gardenio"; })[0].home.zones, ez = gz.escalation;
assert.strictEqual(ez.items.length, 7, "gardenio's Escalations cell holds the seven");
var pc = { w: 390, h: 844 }, ecam = { cx: ez.x * T, cy: (ez.y + ez.h / 2) * T, z: 2 }, ko = B.keepOff(lw, ecam, pc, T, 100);
var corner = function (r) { return { x: (r.x * T - ecam.cx) * ecam.z + pc.w / 2, y: (r.y * T - ecam.cy) * ecam.z + pc.h / 2, w: r.w * T * ecam.z, h: r.h * T * ecam.z }; };
var er = ko.filter(function (r) { return W.hit(r, corner(ez.items[0])); });
assert.strictEqual(er.length, 1, "one rect holds the cell's items: " + JSON.stringify(ko));
ez.items.forEach(function (it) { var q = corner(it); assert.ok(q.x >= er[0].x - 1e-6 && q.y >= er[0].y - 1e-6 && q.x + q.w <= er[0].x + er[0].w + 1e-6 && q.y + q.h <= er[0].y + er[0].h + 1e-6, it.id + " inside"); });
var top = corner(ez);
assert.strictEqual(er[0].x, Math.max(0, top.x), "from the cell's left (its sign)"); assert.strictEqual(er[0].x + er[0].w <= pc.w, true, "clipped to the board");
assert.ok(er[0].y >= 100, "under the header");
ko.forEach(function (r) { assert.ok(r.w > 0 && r.h > 0); });
assert.ok(!B.keepOff(lw, ecam, pc, T, 0).some(function (r) { return W.hit(r, corner(gz.barn)) && !W.hit(r, corner(ez)) && !W.hit(r, corner(gz.pen)); }), "the Barn holds no items: no rect of its own");
assert.deepStrictEqual(B.keepOff(lw, { cx: -1e6, cy: -1e6, z: 1 }, pc, T, 0), [], "out of view: none");
assert.deepStrictEqual(B.keepOff(null, ecam, pc, T, 0), []);
`)
}

// The beacons are wired into the page: the script loads before garden.js, the arrows have their own group on the board,
// garden.js re-lays them on every camera change and data change and activating one is a cursor jump (cursor-grid spec section
// 8; the camera follows with the shared flyTo, so reduced motion is a jump cut), coarse pointers get 44 px arrows, and the
// cursor's framing keeps off the arrows' edge bands (fitClear, gd-SWjiYouIQt).
func TestBeaconsWired(t *testing.T) {
	_, rec := servedPage(t)
	body := inlined(t, rec.Body.String())
	if i, j := strings.Index(body, "var BEACONS = (function"), strings.Index(body, "var SECS=['decision','work','agent']"); i < 0 || j < 0 || i > j {
		t.Error("beacons.js must load before garden.js")
	}
	if !strings.Contains(body, `<div class="bcns" id="bcns" role="group" aria-label="Off the screen and waiting on you"></div>`) {
		t.Error("page missing the beacons group")
	}
	for _, want := range []string{
		"syncBeacons();placeLabels();scheduleURL();", // the arrows first: the board keeps off them (web/signfit.js)
		"BEACONS.layout(bcnTg,c,vp,{size:coarse?BEACONS.SIZE_COARSE:BEACONS.SIZE,",
		"BEACONS.keepOff(world,c,vp,T,bcnAt.top)", // the farmstead cells' items (gd-WzYgtbwMT7)
		"BEACONS.keepOff(world,c,vp,T,bcnAt.top)", // the farmstead cells' items keep the arrows off too (gd-WzYgtbwMT7)
		"BEACONS.render(bcnEl,bcnNow=bcnLayout(cam),goBeacon);",
		"var b=BEACONS.inset(bcnLayout(WORLD.clampCam({cx:f.cx,cy:f.cy,z:f.z},worldBox)),f,vp,things);",
		"var f=fitClear(nav.path,function(v){return nav.frame(v);})",
		"function goBeacon(t,keyed){cursorTo(t.id,keyed);}",
		"refreshSigns();buildOutline();syncBeacons();",
		"refreshSigns();syncBeacons();buildOutline();",
	} {
		if !strings.Contains(pageAppJS, want) {
			t.Errorf("garden.js missing %s", want)
		}
	}
	for _, want := range []string{".bcns{position:absolute;", "@media (pointer:coarse){.bcn{width:44px;height:44px}", ".bcn:focus-visible{"} {
		if !strings.Contains(pageCSS, want) {
			t.Errorf("garden.css missing %s", want)
		}
	}
	if !strings.Contains(pageBeaconsJS, "SIZE = 36, SIZE_COARSE = 44") {
		t.Error("beacons.js: the coarse arrow must be 44 px")
	}
}

// The beacons in a real browser: Chromium under playwright-core opens the goldens' world at the Bead level on plan P, checks
// each arrow is on the board, clear of the controls and of the other arrows, sized 36 px (44 px on a touch phone) and
// worded, then clicks, presses Enter on and taps one, and checks the fly (a jump cut under reduced motion, animated
// otherwise) and the focus hand-off. What is asserted lives in testdata/beacons_browser.js.
func TestBeaconsUnderChromium(t *testing.T) {
	underChromium(t, "beacons_browser.js", func() *httptest.Server {
		return cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	})
}

// The Mailbox's way to the map in a real browser (gd-kNV6Eos4ON): a row whose ask a plan carries names the plan and opens
// its plot, on desktop and phone. What is asserted lives in testdata/mailmap_browser.js.
func TestMailMapUnderChromium(t *testing.T) {
	underChromium(t, "mailmap_browser.js", func() *httptest.Server {
		// the Mailbox holds plan A's decision (its waiting list carries it) and the staked releases plan's own "Run it?",
		// beside the goldens' d-1, which no plan carries
		a2f := fb("gd-A2f", "decision", "open", "gd-A2", 950)
		a2f.Title = "Where does the chat live?"
		rel := releasesPlan()
		return cardWorld(t, func() time.Time { return goldenNow }, append(goldenPorch(), a2f, rel[len(rel)-1])...)
	}, os.Getenv("GARDENHUB_MAILMAP_SHOTS"))
}

// A strip's letters are on the map in a real browser (HW 28.2, gd-2vAMq1HHI9): with 60 loose asks in gardenio, more than its
// Mailbox cell holds, GARDEN.known says onMap for each letter the strip places, and GARDEN.fly reaches it, and for none past
// the cell's "+N". What is asserted lives in testdata/homeknown_browser.js.
func TestStripKnownUnderChromium(t *testing.T) {
	underChromium(t, "homeknown_browser.js", func() *httptest.Server {
		asks := goldenPorch()
		for i := 0; i < 60; i++ {
			asks = append(asks, fb(fmt.Sprintf("gd-loose%02d", i), "decision", "open", "", 900))
		}
		return cardWorld(t, func() time.Time { return goldenNow }, asks...)
	})
}
