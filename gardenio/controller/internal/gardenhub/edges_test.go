package gardenhub

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// A blocker in another project names its project on the task (blocked_by_project, gd-CNhEh30Ykf), so a client can draw the
// cross-project edge even when the document does not carry the blocker. Same-project blockers are left out of the map.
func TestFarmBlockedByProject(t *testing.T) {
	src := fixtureWorld()
	far := fb("gd-K1", "task", "open", "", 100)
	far.Labels = []string{"project:kbeads"}
	bare := fb("gd-N1", "task", "open", "", 90)
	bare.Labels = nil
	src.add(far, bare)
	src.deps["gd-A2a"] = append(src.deps["gd-A2a"], beadsapi.Dependency{BeadID: "gd-A2a", DependsOnID: "gd-K1", Type: "blocks"}, beadsapi.Dependency{BeadID: "gd-A2a", DependsOnID: "gd-N1", Type: "blocks"})
	f := newTestFarm(src)
	_ = f.refresh(context.Background())
	a := findTask(findPlan(decodeSnap(t, f), "gd-planA").Phases[1].Tasks, "gd-A2a")
	if a == nil || strings.Join(a.BlockedBy, ",") != "gd-A1a,gd-K1,gd-N1" {
		t.Fatalf("blocked_by wrong: %+v", a)
	}
	if len(a.BlockedByProject) != 2 || a.BlockedByProject["gd-K1"] != "kbeads" || a.BlockedByProject["gd-N1"] != "" {
		t.Fatalf("blocked_by_project = %#v, want gd-K1 kbeads and gd-N1 unlabelled only", a.BlockedByProject)
	}
	if _, ok := a.BlockedByProject["gd-A1a"]; ok {
		t.Fatal("a same-project blocker must not be named")
	}
	if b := findTask(findPlan(decodeSnap(t, f), "gd-planA").Phases[1].Tasks, "gd-A2b"); b == nil || b.BlockedByProject != nil {
		t.Fatalf("a task blocked only within its project carries no map: %+v", b)
	}
	raw, _ := json.Marshal(a)
	if !strings.Contains(string(raw), `"blocked_by_project":{"gd-K1":"kbeads","gd-N1":""}`) {
		t.Fatalf("wire shape: %s", raw)
	}
}

// edges.js (gd-CNhEh30Ykf) keeps the cross-project edges pure, so its contract runs under node on a real layout: which edges
// cross regions (a step's region is its plan's; a blocker the document lacks takes its blocked_by_project), the square route
// between two rects, merging and the cap per level, lanes for channels that share a gap, the words on cards and links, and
// the draw (fillRect only, culled to the view).
func TestEdgesLogicUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), E = require(process.argv[2]), assert = require("assert");
var now = 1792000000, D = 86400;
var counts = function (n) { return { in_progress: 0, review: 0, blocked: 0, open: n, deferred: 0, closed: 0 }; };
var task = function (id, project, extra) { var t = { id: id, title: "Step " + id, type: "task", state: "open", priority: 2, project: project, updated: now }; for (var k in extra || {}) t[k] = extra[k]; return t; };
var plan = function (id, project, tasks) { return { id: id, title: "Plan " + id, state: "building", status: "in_progress", project: project, priority: 2, created: now - D, updated: now, activity: now,
  phases: [{ id: id + "-ph", title: "Phase", status: "open", counts: counts(tasks.length), tasks: tasks }], unphased: { id: "", title: "Other", status: "", counts: counts(0), tasks: [] }, counts: counts(tasks.length), open_decisions: 0, open_escalations: 0, needs_human: 0 }; };
var doc = { plans: [
  // gd-g1 waits on a kbeads step on the map, a gardenio step (same region: no edge), a coop bead the document lacks, and a bead
  // in a project with no region; gd-g3 is labelled kbeads but grows on a gardenio plot, so its gardenio blocker is no edge
  plan("gd-G1", "gardenio", [task("gd-g1", "gardenio", { blocked_by: ["gd-k1", "gd-g2", "gd-x9", "gd-z1"], blocked_by_project: { "gd-k1": "kbeads", "gd-x9": "Coop", "gd-z1": "zeta" } }),
    task("gd-g2", "gardenio"), task("gd-g3", "kbeads", { blocked_by: ["gd-g2"], blocked_by_project: { "gd-g2": "gardenio" } })]),
  plan("gd-K1P", "kbeads", [task("gd-k1", "kbeads"), task("gd-k2", "kbeads", { blocked_by: ["gd-g2", "gd-u1"] })]),
  plan("gd-C1P", "coop", [task("gd-c1", "coop")])
], seeds: [], meadow: { total: 0, shown: [] }, closed_plans: { total: 0, shown: [] } };
var world = W.layout(doc, null, { now: now * 1000 }), ix = E.index(doc, world);
var pairs = ix.edges.map(function (e) { return e.task + ">" + e.blocker + "@" + e.to + (e.drawn ? "" : "(not drawn)"); });
assert.deepStrictEqual(pairs, ["gd-g1>gd-k1@kbeads", "gd-g1>gd-x9@coop", "gd-g1>gd-z1@zeta(not drawn)", "gd-k2>gd-g2@gardenio"], "cross-region edges only: " + pairs);
assert.strictEqual(ix.edges[1].project, "coop", "a region is named as its biome is");
assert.strictEqual(ix.edges[2].project, "zeta", "a project with no region keeps its label");
assert.strictEqual(ix.edges[1].blockerTitle, "", "a blocker the document lacks has no title");
assert.deepStrictEqual(E.index(null, null).edges, []);

// route: region to region, out of the facing side, along the gap's middle, into the facing side
assert.deepStrictEqual(E.route({ x: 0, y: 0, w: 10, h: 10 }, { x: 30, y: 20, w: 10, h: 10 }), { axis: "h", pts: [[10, 5], [20, 5], [20, 25], [30, 25]], sides: ["y", "y"] });
assert.deepStrictEqual(E.route({ x: 30, y: 0, w: 10, h: 10 }, { x: 0, y: 2, w: 10, h: 10 }).pts, [[30, 5], [20, 5], [20, 7], [10, 7]], "right to left");
assert.deepStrictEqual(E.route({ x: 0, y: 0, w: 10, h: 10 }, { x: 4, y: 40, w: 10, h: 10 }), { axis: "v", pts: [[5, 10], [5, 25], [9, 25], [9, 40]], sides: ["x", "x"] });
// a plot inside its region steps into the gutter beside it (toward the other end), runs along it out of the region, turns in
// the gap between the regions, and comes into the other plot the same way
var sr = { x: 0, y: 0, w: 100, h: 100 }, dr = { x: 200, y: 0, w: 100, h: 100 };
assert.deepStrictEqual(E.route({ x: 10, y: 10, w: 32, h: 32 }, { x: 210, y: 50, w: 32, h: 32 }, sr, dr), { axis: "h", pts: [[26, 42], [26, 43], [150, 43], [150, 49], [226, 49], [226, 50]], sides: ["x", "x"] });
assert.deepStrictEqual(E.route({ x: 10, y: 10, w: 32, h: 32 }, dr, sr, dr).pts, [[26, 42], [26, 43], [150, 43], [150, 50], [200, 50]], "a plot to a region");
assert.deepStrictEqual(E.route({ x: 10, y: 10, w: 32, h: 32 }, { x: 50, y: 250, w: 32, h: 32 }, sr, { x: 0, y: 200, w: 100, h: 100 }).pts, [[42, 26], [43, 26], [43, 150], [49, 150], [49, 266], [50, 266]], "stacked regions: out of the side into the column gutter");
assert.strictEqual(E.route({ x: 0, y: 0, w: 10, h: 10 }, { x: 15, y: 30, w: 10, h: 10 }).axis, "v", "diagonal: the wider gap");
assert.strictEqual(E.route({ x: 0, y: 0, w: 10, h: 10 }, { x: 5, y: 5, w: 10, h: 10 }), null, "overlapping rects have no route");

// channels: per level, merged by their two ends, the two directions of one gap in separate lanes
var l0 = ix.levels[0].shown.map(function (c) { return c.id; }).sort(), l1 = ix.levels[1].shown.map(function (c) { return c.id + "#" + c.n; }).sort();
assert.deepStrictEqual(l0, ["project:coop>project:gardenio", "project:gardenio>project:kbeads", "project:kbeads>project:gardenio"], "World: region to region");
assert.deepStrictEqual(l1, ["gd-G1>gd-K1P#1", "gd-K1P>gd-G1#1", "project:coop>gd-G1#1"], "below the World: plot to plot, or a region for a blocker off the map");
ix.levels.forEach(function (lv) {
  lv.shown.forEach(function (c) {
    var p = c.route.pts, inside = function (r, q) { return q[0] >= r.x && q[0] <= r.x + r.w && q[1] >= r.y && q[1] <= r.y + r.h; };
    assert.ok(inside(c.src, p[0]) && inside(c.dst, p[p.length - 1]), c.id + ": the channel starts on its blocker and ends on the step that waits");
  });
});
// below the World level no run crosses a plot: only the two short steps off the ends touch their own plots
var plots = []; world.biomes.forEach(function (b) { plots = plots.concat(b.plots); });
ix.levels[1].shown.forEach(function (c) {
  var p = c.route.pts;
  for (var j = 1; j < p.length; j++) {
    var r = { x: Math.min(p[j][0], p[j - 1][0]), y: Math.min(p[j][1], p[j - 1][1]) }; r.w = Math.abs(p[j][0] - p[j - 1][0]); r.h = Math.abs(p[j][1] - p[j - 1][1]);
    plots.forEach(function (q) { assert.ok(!(r.x < q.x + q.w && q.x < r.x + r.w && r.y < q.y + q.h && q.y < r.y + r.h), c.id + " run " + j + " crosses plot " + q.id); });
  }
});
// the cap: 60 steps each waiting on its own region's twin merge into 1 channel per pair, cut at MAX_CHANNELS
var many = { plans: [] };
for (var i = 0; i < 60; i++) many.plans.push(plan("gd-M" + i, "p" + i, [task("gd-m" + i, "p" + i, { blocked_by: ["gd-m" + ((i + 1) % 60)], blocked_by_project: {} })]));
many.plans.forEach(function (p, i) { p.phases[0].tasks[0].blocked_by_project["gd-m" + ((i + 1) % 60)] = "p" + ((i + 1) % 60); });
var mw = W.layout(many, null, { now: now * 1000 }), mix = E.index(many, mw);
assert.strictEqual(mix.edges.length, 60);
assert.strictEqual(mix.levels[0].shown.length, E.MAX_CHANNELS, "at most MAX_CHANNELS drawn");
assert.strictEqual(mix.levels[0].dropped, 60 - E.MAX_CHANNELS, "the rest are counted");
// no two channels share a stretch of one line in one lane
var taken = [];
mix.levels.concat(ix.levels).forEach(function (lv, li) {
  lv.shown.forEach(function (c) {
    var p = c.route.pts;
    for (var j = 1; j < p.length; j++) {
      var hz = p[j][1] === p[j - 1][1], q = hz ? 0 : 1, run = { lv: li, line: (hz ? "h" : "v") + p[j][1 - q], lane: c.lane, a: Math.min(p[j][q], p[j - 1][q]), b: Math.max(p[j][q], p[j - 1][q]), id: c.id };
      taken.forEach(function (t) { assert.ok(t.lv !== run.lv || t.id === run.id || t.line !== run.line || t.lane !== run.lane || t.b <= run.a || run.b <= t.a, run.id + " and " + t.id + " share " + run.line + " in lane " + run.lane); });
      taken.push(run);
    }
  });
});

// words: "Blocked by <id> in <project>" on the step, its plot and the link names; "Blocks" on the blocker's side
var href = function (b) { return "/garden?bead=" + b.id; };
assert.deepStrictEqual(E.lines(ix, { kind: "task", id: "gd-g1" }, href), [
  ["edge", "Blocked by gd-k1 in kbeads", "/garden?bead=gd-k1", "Step gd-k1"], ["edge", "Blocked by gd-x9 in coop", "/garden?bead=gd-x9", "gd-x9"],
  ["edge", "Blocked by gd-z1 in zeta", "/garden?bead=gd-z1", "gd-z1"]]);
assert.deepStrictEqual(E.lines(ix, { kind: "task", id: "gd-g2" }, null), [["edge", "Blocks gd-k2 in kbeads"]]);
assert.deepStrictEqual(E.lines(ix, { kind: "plot", id: "gd-G1" }, null, 2), [["edge", "Blocked by gd-k1 in kbeads: Step gd-g1 waits on it"], ["edge", "Blocked by gd-x9 in coop: Step gd-g1 waits on it"], ["edge", "+2 more edges to other projects"]], "capped, then counted");
assert.deepStrictEqual(E.lines(ix, { kind: "plot", id: "gd-K1P" }, null), [["edge", "Blocked by gd-g2 in gardenio: Step gd-k2 waits on it"], ["edge", "Blocks gd-g1 in gardenio: it waits on Step gd-k1"]]);
assert.deepStrictEqual(E.lines(ix, { kind: "biome", id: "project:gardenio" }, href), [["edge", "Irrigation channels: 1 step here waits on beads in kbeads, coop and zeta; 1 bead here holds up steps in kbeads."]]);
assert.deepStrictEqual(E.lines(ix, { kind: "biome", id: "project:coop" }, href), [["edge", "Irrigation channels: 1 bead here holds up steps in gardenio."]]);
assert.deepStrictEqual(E.lines(ix, { kind: "gardener", id: "gd-g1" }, href), []);
assert.deepStrictEqual(E.lines(null, { kind: "task", id: "gd-g1" }, href), []);
assert.strictEqual(E.aria(ix, { kind: "task", id: "gd-g1" }), " Blocked by gd-k1 in kbeads. Blocked by gd-x9 in coop. Blocked by gd-z1 in zeta.");
assert.strictEqual(E.aria(ix, { kind: "plot", id: "gd-G1" }), " Blocked by gd-k1 in kbeads. Blocked by gd-x9 in coop. Blocked by gd-z1 in zeta. And 1 more edge to other projects.");
assert.strictEqual(E.aria(ix, { kind: "plotsign", id: "gd-K1P" }), " Blocked by gd-g2 in gardenio. Blocks gd-g1 in gardenio.");
assert.strictEqual(E.aria(ix, { kind: "sign", id: "project:kbeads" }), " Irrigation channels: 1 step here waits on beads in gardenio; 1 bead here holds up steps in gardenio.");
assert.strictEqual(E.aria(ix, { kind: "task", id: "gd-c1" }), "");

// a scoped viewer's document (gd-U2pbNv3lLC): a foreign blocker is a project name only, so its edge is named and links nowhere;
// "" is "another project"; the kept blocker is an ordinary edge. It is drawn as a bridge (gd-DgEhq38EnY), below.
var sdoc = { plans: [plan("gd-S1", "gardenio", [task("gd-s1", "gardenio", { blocked_by: ["gd-s2"], blocked_by_foreign: ["", "kbeads"] }), task("gd-s2", "gardenio"),
  task("gd-s3", "gardenio", { blocked_by_foreign: ["kbeads"] })])], seeds: [], meadow: { total: 0, shown: [] }, closed_plans: { total: 0, shown: [] } };
var sw = W.layout(sdoc, null, { now: now * 1000 }), six = E.index(sdoc, sw);
assert.deepStrictEqual(six.edges.map(function (e) { return e.task + ">" + (e.blocker || "-") + "@" + e.project + (e.drawn ? "" : "(not drawn)"); }),
  ["gd-s1>-@another project", "gd-s1>-@kbeads", "gd-s3>-@kbeads"], "foreign blockers: named, drawn as bridges");
assert.deepStrictEqual(E.lines(six, { kind: "task", id: "gd-s1" }, href), [["edge", "Blocked by a bead in another project"], ["edge", "Blocked by a bead in kbeads"]], "no link, no id");
assert.deepStrictEqual(E.lines(six, { kind: "plot", id: "gd-S1" }, null), [["edge", "Blocked by a bead in another project: Step gd-s1 waits on it"], ["edge", "Blocked by a bead in kbeads: Step gd-s1 waits on it"], ["edge", "Blocked by a bead in kbeads: Step gd-s3 waits on it"]]);
assert.strictEqual(E.aria(six, { kind: "task", id: "gd-s3" }), " Blocked by a bead in kbeads.");
assert.deepStrictEqual(E.lines(six, { kind: "biome", id: "project:gardenio" }, href), [["edge", "Irrigation channels: 2 steps here wait on beads in another project and kbeads."]]);
assert.deepStrictEqual(six.holds, {}, "a foreign edge holds up nothing the viewer has");
// bridges: one per rect that waits (gd-s1 and gd-s3 share plan gd-S1's plot; at the World level the region), named for every
// project it waits on, out of the region by a side nothing lies beyond (the empty Commons takes no room west of the only region), STUB tiles past
// it: below the World the nearest (east: the plot is slot 1, east of the farmstead, 29.1), at the World level south, away from
// the region's board
var sb = function (lv) { return six.levels[lv].shown.map(function (c) { return c.id + "#" + c.n + ":" + E.nameOf(c) + "@" + c.side + (c.free ? "" : "(blocked)"); }); };
assert.deepStrictEqual(sb(0), ["bridge>project:gardenio#3:another project and kbeads@bottom"], "World: one bridge off the region");
assert.deepStrictEqual(sb(1), ["bridge>gd-S1#3:another project and kbeads@right"], "below the World: one bridge off the plot");
var sf = sw.index["project:gardenio"].foot, sp = sw.index["gd-S1"];
six.levels.forEach(function (lv, li) {
  var c = lv.shown[0], p = c.route.pts, inside = function (r, q) { return q[0] >= r.x && q[0] <= r.x + r.w && q[1] >= r.y && q[1] <= r.y + r.h; };
  assert.ok(inside(c.dst, p[p.length - 1]), "level " + li + ": the bridge ends on what waits");
  var east = c.side === "right";
  assert.deepStrictEqual([p[0][east ? 0 : 1], c.edge], east ? [sf.x + sf.w + E.STUB, sf.x + sf.w] : [sf.y + sf.h + E.STUB, sf.y + sf.h], "level " + li + ": it starts STUB tiles past the foot's edge");
  assert.ok(p.every(function (q, i) { return !i || q[0] === p[i - 1][0] || q[1] === p[i - 1][1]; }), "level " + li + ": square runs");
});
assert.deepStrictEqual(six.levels[1].shown[0].route.pts, [[sf.x + sf.w + E.STUB, sp.y + sp.h + E.GUTTER], [sp.x + sp.w / 2, sp.y + sp.h + E.GUTTER], [sp.x + sp.w / 2, sp.y + sp.h]], "along the gutter south of the plot, into its foot, straight out of the region");
assert.strictEqual(E.nameOf({ names: ["a", "b", "c", "d"] }), "a, b +2", "a long list is cut");
// outward: the nearest free side; a side another region lies beyond is not free; with none free, the nearest of all
var ow = { biomes: [], index: {} }, mk = function (id, f) { var b = { id: "project:" + id, foot: f }; ow.biomes.push(b); ow.index[b.id] = b; return b; };
mk("a", { x: 0, y: 0, w: 100, h: 100 });
assert.strictEqual(E.outward(ow, "a", { x: 30, y: 10, w: 32, h: 32 }).side, "top", "nearest side");
assert.strictEqual(E.outward(ow, "a", { x: 60, y: 50, w: 32, h: 32 }).side, "right");
assert.strictEqual(E.outward(ow, "a", { x: 50, y: 10, w: 0, h: 0 }).side, "bottom", "from the middle at the World level: south first, whatever is nearer");
mk("b", { x: 116, y: 0, w: 100, h: 100 });
var o2 = E.outward(ow, "a", { x: 60, y: 50, w: 32, h: 32 });
assert.deepStrictEqual([o2.side, o2.free, o2.at], ["bottom", true, [93, 104]], "a neighbour to the right: the nearest free side, on the plot's gutter");
mk("c", { x: 0, y: -116, w: 100, h: 100 }); mk("d", { x: 0, y: 116, w: 100, h: 100 }); mk("e", { x: -116, y: 0, w: 100, h: 100 });
var o3 = E.outward(ow, "a", { x: 60, y: 50, w: 32, h: 32 });
assert.deepStrictEqual([o3.side, o3.free, o3.at[0]], ["right", false, 104], "boxed in: the nearest side, STUB tiles out, short of the gap's middle");

// drawing: fillRect only, a gate per channel, culled to the view; lanes keep two channels of one gap apart
function Ctx() { this.calls = []; this.fillStyle = ""; }
Ctx.prototype.fillRect = function (x, y, w, h) { assert.ok(isFinite(x + y + w + h) && w > 0 && h > 0, "bad rect " + [x, y, w, h]); this.calls.push([this.fillStyle, x, y, w, h]); };
[[0, 0.08], [1, 0.3], [2, 0.7], [3, 2]].forEach(function (lz) {
  var c = new Ctx(), n = E.draw(c, ix, { level: lz[0], z: lz[1], T: 16, night: false });
  assert.strictEqual(n, c.calls.length, "the count it returns is its fillRects");
  var gates = c.calls.filter(function (k) { return k[0] === "#9a6630"; }).length;
  assert.strictEqual(gates, ix.levels[lz[0] ? 1 : 0].shown.length, "one gate per channel at level " + lz[0]);
  assert.ok(c.calls.some(function (k) { return k[0] === "#3a8fd6"; }), "water");
});
var nc = new Ctx(); E.draw(nc, ix, { level: 0, z: 0.08, T: 16, night: true });
assert.ok(nc.calls.some(function (k) { return k[0] === "#5ab0ff"; }) && !nc.calls.some(function (k) { return k[0] === "#3a8fd6"; }), "night water");
var far = new Ctx(); assert.strictEqual(E.draw(far, ix, { level: 3, z: 2, T: 16, night: false, view: { x: 1e6, y: 1e6, w: 100, h: 100 } }), 0, "nothing in view, nothing drawn");
var all = new Ctx(), some = new Ctx(), c0 = ix.levels[1].shown[0], p0 = E.lanePts(c0, 16, 3);
assert.strictEqual(E.width(2, 3, 16), E.LINE_PX / 2, "on screen LINE_PX");
assert.strictEqual(E.width(0.13, 1, 16), 1.6 * 16, "below the World level it fits the gutter between plots");
assert.strictEqual(E.width(0.05, 0, 16), E.LINE_PX / 0.05, "the World level is not capped");
E.draw(all, ix, { level: 3, z: 2, T: 16 }); E.draw(some, ix, { level: 3, z: 2, T: 16, view: { x: p0[1][0] - 40, y: p0[1][1] - 40, w: 80, h: 80 } });
assert.ok(some.calls.length > 0 && some.calls.length < all.calls.length / 2, "a small view draws only its runs and ripples: " + some.calls.length + " of " + all.calls.length);
var two = ix.levels[0].shown.filter(function (c) { return c.id.indexOf("kbeads") >= 0; }).map(function (c) { return E.lanePts(c, 16, 75); });
assert.ok(Math.abs(two[0][1][1] - two[1][1][1]) >= 75, "the two directions of one gap lie a width apart");
assert.deepStrictEqual(ix.levels[0].shown[0].route.pts[0], (function (u) { return [u.x + u.w / 2, u.y + u.h / 2]; })(world.index["project:coop"].used), "at the World level a channel starts in the middle of its region");
assert.strictEqual(E.draw(new Ctx(), null, { level: 0, z: 1, T: 16 }), 0);
// a bridge: its channel, the gate at the plot, a deck at the far end; on a free side at least four widths past the foot
[[1, 0.3], [3, 2]].forEach(function (lz) {
  var c = new Ctx(), n = E.draw(c, six, { level: lz[0], z: lz[1], T: 16, night: false }), W = E.width(lz[1], lz[0], 16);
  assert.strictEqual(n, c.calls.length, "the count it returns is its fillRects");
  var planks = c.calls.filter(function (k) { return k[0] === "#9a6630"; });
  assert.strictEqual(planks.length, 2, "level " + lz[0] + ": a gate and a deck");
  var water = c.calls.filter(function (k) { return k[0] === "#3a8fd6"; }), deckAt = planks[1];
  var east = (sf.x + sf.w) * 16, far = Math.max.apply(null, water.map(function (k) { return k[1] + k[3]; }));
  var south = (sf.y + sf.h) * 16, low = Math.max.apply(null, water.map(function (k) { return k[2] + k[4]; }));
  assert.ok(far >= east + 4 * W - W / 2 || low >= south + 4 * W - W / 2, "level " + lz[0] + ": the water runs four widths past the region: " + far + " vs " + (east + 4 * W) + ", " + low + " vs " + (south + 4 * W));
  var downs = six.levels[lz[0] ? 1 : 0].shown[0].side === "bottom";   // the side the bridge leaves by (sb above): its deck lies past it, across the water
  assert.ok(downs ? deckAt[2] > south && deckAt[3] > W : deckAt[1] > east && deckAt[4] > W, "level " + lz[0] + ": the deck is outside the region, wider than the channel");
});
var w0 = new Ctx(), W0 = E.width(0.05, 0, 16); E.draw(w0, six, { level: 0, z: 0.05, T: 16 });
assert.ok(Math.max.apply(null, w0.calls.filter(function (k) { return k[0] === "#3a8fd6"; }).map(function (k) { return k[2] + k[4]; })) >= (sf.y + sf.h) * 16 + 3.5 * W0, "World: the water runs four widths south of the region");
var named = []; function NCtx() { Ctx.call(this); } NCtx.prototype = Object.create(Ctx.prototype); NCtx.prototype.drawImage = function (img, x, y, w, h) { named.push([img, x, y, w, h]); };
var boxes = E.drawNames(new NCtx(), six, { level: 3, z: 2, T: 16, night: false, dpr: 2 });
assert.strictEqual(named.length, 0, "off the page a name is not drawn (no canvas to bake it on)");
assert.strictEqual(boxes.length, 1, "a box per bridge");
assert.ok(boxes[0].x > (sf.x + sf.w) * 16 && boxes[0].h > 0, "beyond the far end, east of the region: " + JSON.stringify(boxes[0]));
assert.deepStrictEqual(E.drawNames(new Ctx(), ix, { level: 1, z: 0.3, T: 16 }), [], "no bridge, no name");
assert.deepStrictEqual(E.drawNames(new Ctx(), null, { level: 1, z: 0.3, T: 16 }), []);
assert.strictEqual(E.safeView(null, { cx: 0, cy: 0, z: 1 }, { w: 800, h: 600 }), null, "no safe view before the signs' first refresh");
assert.deepStrictEqual(E.safeView({ w: 800, h: 600, top: 40, right: 100, bottom: 60 }, { cx: 500, cy: 300, z: 2 }, { w: 800, h: 600 }), { x: 300, y: 170, w: 350, h: 250 },
  "the map the HUD leaves, in world px at the camera");
var bp = [[100, 50], [100, 200]], nb = E.nameBox({ side: "top" }, bp, 4, { w: 40, h: 10 }, 0), nb1 = E.nameBox({ side: "top" }, bp, 4, { w: 40, h: 10 }, 1);
assert.ok(Math.abs(nb.x - (100 + 4 * 1.2 + 4 * 0.6)) < 1e-9 && nb.y === 45, "off the top a name sits beside the channel's tip, clear of the deck: " + JSON.stringify(nb));
var nh = E.nameBox({ side: "right" }, [[300, 50], [100, 50]], 4, { w: 40, h: 10 }, 0);
assert.ok(Math.abs(nh.x - (300 + 2 + 2.4)) < 1e-9 && nh.y === 45, "off a side, beyond the far end in line with the channel: " + JSON.stringify(nh));
assert.ok(nb1.y + nb1.h <= nb.y, "and steps further out past one already there");
assert.deepStrictEqual(E.nameBox({ side: "top" }, bp, 4, { w: 40, h: 10 }, 0, { x: 0, y: 60, w: 500, h: 500 }).y, 60, "under the header it slides back along its channel onto the map");
assert.deepStrictEqual(E.nameBox({ side: "top" }, bp, 4, { w: 40, h: 10 }, 0, { x: 0, y: 300, w: 500, h: 500 }).y, 190, "but no further than the channel's last bend");
assert.deepStrictEqual(E.nameBox({ side: "bottom" }, [[100, 300], [100, 200]], 4, { w: 40, h: 10 }, 0, { x: 0, y: 0, w: 500, h: 280 }).y, 270, "and over the Commons the same, upward");
assert.deepStrictEqual(E.nameBox({ side: "bottom" }, [[100, 300], [100, 200]], 4, { w: 40, h: 10 }, 0, { x: 0, y: 0, w: 120, h: 500 }).x, 100 - 4 * 1.2 - 4 * 0.6 - 40, "past safe's east edge, beside the tip's west side");
var lg = new Ctx(); E.drawLegend(lg, false); assert.ok(lg.calls.length >= 4 && lg.calls.every(function (k) { return k[1] >= 0 && k[2] >= 0 && k[1] + k[3] <= 24 && k[2] + k[4] <= 28; }), "the legend swatch fits its 24 x 28 canvas");
`
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	out, err := exec.Command(node, "-e", script, abs(filepath.Join("web", "world.js")), abs(filepath.Join("web", "edges.js"))).CombinedOutput()
	if err != nil {
		t.Fatalf("node edges logic test failed: %v\n%s", err, out)
	}
}

// Water channels inside a field (gd-Y4gXDBblIG; thin and seam-routed since gd-osWVpshPrj): on the fixture farm's plan A,
// gd-A2a waits on gd-A1a (the bed before) and gd-A2b on gd-A2a (the next cell). Every channel is square, runs from its
// blocker's centre to the waiting cell's, bends only on the seams between tiles (so it never crosses another cell), stays on
// its plot, and is drawn as merged thin runs with no gate; the comets step a sixteenth of a tile with the frame toward the
// step that waits. Then the routes, shared runs and the cap.
func TestFieldChannelsUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), E = require(process.argv[2]), assert = require("assert"), doc = JSON.parse(require("fs").readFileSync(process.argv[3]));
var world = W.layout(doc, null, { now: 1792010000 * 1000 }), ix = E.index(doc, world), f = ix.fields["gd-planA"], plot = world.index["gd-planA"];
assert.deepStrictEqual(Object.keys(ix.fields), ["gd-planA"], "only plan A has blockers on its own plot");
assert.deepStrictEqual(f.shown.map(function (c) { return c.blocker + ">" + c.task; }), ["gd-A1a>gd-A2a", "gd-A2a>gd-A2b"]);
var half = function (v) { return Math.abs(v * 2 - Math.round(v * 2)) < 1e-9; }, centre = function (v) { return Math.abs(v - Math.floor(v) - 0.5) < 1e-9; }, seam = function (v) { return Math.abs(v - Math.round(v)) < 1e-9; };
f.shown.forEach(function (c) {
  var p = c.pts, a = world.index[c.blocker], b = world.index[c.task];
  assert.deepStrictEqual([p[0], p[p.length - 1]], [[a.x + 0.5, a.y + 0.5], [b.x + 0.5, b.y + 0.5]], c.task + ": from the blocker's centre to the waiting cell's: " + JSON.stringify(p));
  p.forEach(function (q, i) {
    if (i) assert.ok(q[0] === p[i - 1][0] || q[1] === p[i - 1][1], c.task + ": square, never diagonal");
    if (i && i < p.length - 1) assert.ok(seam(q[0]) || seam(q[1]), c.task + ": bends on a seam between tiles: " + q);
    assert.ok(q[0] > plot.x && q[0] < plot.x + plot.w && q[1] > plot.y && q[1] < plot.y + plot.h, c.task + ": on its plot");
  });
  for (var i = 1; i < p.length; i++) {   // no stretch crosses a cell other than the two ends: between seams, or inside a or b
    var inA = function (q) { return q[0] > a.x && q[0] < a.x + 1 && q[1] > a.y && q[1] < a.y + 1; }, inB = function (q) { return q[0] > b.x && q[0] < b.x + 1 && q[1] > b.y && q[1] < b.y + 1; };
    var m = [(p[i][0] + p[i - 1][0]) / 2, (p[i][1] + p[i - 1][1]) / 2], onSeam = p[i][0] === p[i - 1][0] ? seam(p[i][0]) : seam(p[i][1]);
    assert.ok(onSeam || inA(m) || inB(m) || (p.length === 2), c.task + ": a stretch through a cell that is not an end: " + JSON.stringify([p[i - 1], p[i]]));
  }
  assert.deepStrictEqual(p, E.fieldRoute(a, b, plot).pts, c.task + ": fieldRoute's path");
});
f.runs.forEach(function (r) { assert.ok((centre(r.at) || seam(r.at)) && half(r.a) && half(r.b) && r.a <= r.b && Math.abs(r.d) === 1, "a run on a centre line or a seam: " + JSON.stringify(r)); });
assert.strictEqual(E.FIELD_W, 1 / 8);
// drawn: fillRect only, thin (2 px of water in a 1 px bank at 16 px a tile), no gate, water then comets stepping a sixteenth per frame
function Ctx() { this.calls = []; this.fillStyle = ""; }
Ctx.prototype.fillRect = function (x, y, w, h) { assert.ok(isFinite(x + y + w + h) && w > 0 && h > 0, "bad rect " + [x, y, w, h]); this.calls.push([this.fillStyle, x, y, w, h]); };
var c0 = new Ctx(), n0 = E.drawField(c0, ix, "gd-planA", { T: 16, night: false, frame: 0 }), c1 = new Ctx(); E.drawField(c1, ix, "gd-planA", { T: 16, night: false, frame: 1 });
assert.strictEqual(n0, c0.calls.length, "the count it returns is its fillRects");
assert.ok(!c0.calls.some(function (k) { return k[0] === "#9a6630"; }), "no gate");
c0.calls.filter(function (k) { return k[0] === "#2a1a14"; }).forEach(function (k) { assert.ok(Math.min(k[3], k[4]) <= 4 + 1e-9, "a bank 4 px across: " + k); });
c0.calls.filter(function (k) { return k[0] === "#3a8fd6"; }).forEach(function (k) { assert.ok(Math.min(k[3], k[4]) <= 2 + 1e-9, "2 px of water: " + k); });
var east = f.runs.filter(function (r) { return r.h && r.d > 0; })[0];
assert.ok(east, "a run east, as gd-A1a's water runs");
var heads = function (c) { return c.calls.filter(function (k) { return k[0] === "#bfe6ff" && Math.abs(k[2] - (east.at * 16 - 1)) < 1e-9 && k[1] > east.a * 16 + 1 && k[1] + k[3] < east.b * 16 - 1; }).map(function (k) { return k[1]; }).sort(function (a, b) { return a - b; }); }, r0 = heads(c0), r1 = heads(c1);
assert.ok(r0.length && r1.length, "comet heads on it");
assert.ok(r1.some(function (x) { return Math.abs(x - r0[0] - 1) < 1e-9; }), "a frame steps them a px east: " + r0 + " / " + r1);
var nc = new Ctx(); E.drawField(nc, ix, "gd-planA", { T: 16, night: true });
assert.ok(nc.calls.some(function (k) { return k[0] === "#5ab0ff"; }) && nc.calls.some(function (k) { return k[0] === "#1e3a5a"; }), "night water in a blue halo");
assert.strictEqual(E.drawField(new Ctx(), ix, "gd-planB", { T: 16 }), 0, "a plot with none draws nothing");
assert.strictEqual(E.drawField(new Ctx(), null, "gd-planA", { T: 16 }), 0);
// the routes: centre to centre for neighbours; one row or column by the seam nearer the plot's middle; else the row seam on
// b's side, then the column seam on a's side of b when b's row is not beside it
var pl = { x: -1, y: -1, w: 8, h: 8 };
assert.deepStrictEqual(E.fieldRoute({ x: 0, y: 0 }, { x: 1, y: 0 }, pl).pts, [[0.5, 0.5], [1.5, 0.5]], "neighbours");
assert.deepStrictEqual(E.fieldRoute({ x: 0, y: 0 }, { x: 3, y: 0 }, pl).pts, [[0.5, 0.5], [0.5, 1], [3.5, 1], [3.5, 0.5]], "one row, by the seam below");
assert.deepStrictEqual(E.fieldRoute({ x: 0, y: 5 }, { x: 3, y: 5 }, pl).pts, [[0.5, 5.5], [0.5, 5], [3.5, 5], [3.5, 5.5]], "one row, by the seam above");
assert.deepStrictEqual(E.fieldRoute({ x: 2, y: 4 }, { x: 2, y: 1 }, pl).pts, [[2.5, 4.5], [3, 4.5], [3, 1.5], [2.5, 1.5]], "one column, by the seam at its right");
assert.deepStrictEqual(E.fieldRoute({ x: 1, y: 3 }, { x: 4, y: 2 }, pl).pts, [[1.5, 3.5], [1.5, 3], [4.5, 3], [4.5, 2.5]], "b's row beside the seam: in from below");
assert.deepStrictEqual(E.fieldRoute({ x: 0, y: 0 }, { x: 3, y: 2 }, pl).pts, [[0.5, 0.5], [0.5, 1], [3, 1], [3, 2.5], [3.5, 2.5]], "else round by the column seam on a's side of b");
// shared stretches merge: two steps in one row waiting on the same blocker draw its run once; past FIELD_MAX they are counted
var task = function (id, extra) { var t = { id: id, title: id, type: "task", state: "open", priority: 2, project: "p", updated: 1792000000, created: 1792000000 + parseInt(id.slice(4), 10) }; for (var k in extra || {}) t[k] = extra[k]; return t; };
var ts = [task("gd-t0")]; for (var i = 1; i <= 20; i++) ts.push(task("gd-t" + i, { blocked_by: ["gd-t0"] }));
var many = { plans: [{ id: "gd-P", title: "P", state: "building", status: "in_progress", project: "p", priority: 2, created: 1791900000, updated: 1792000000, activity: 1792000000,
  phases: [{ id: "gd-P1", title: "Phase", status: "open", counts: { open: 21 }, tasks: ts }], unphased: { id: "", title: "Other", status: "", counts: {}, tasks: [] }, counts: { open: 21 } }], seeds: [], meadow: { total: 0, shown: [] }, closed_plans: { total: 0, shown: [] } };
var mx = E.index(many, W.layout(many, null, { now: 1792010000 * 1000 })).fields["gd-P"];
assert.strictEqual(mx.shown.length, E.FIELD_MAX, "at most FIELD_MAX a plot");
assert.strictEqual(mx.dropped, 21 - 1 - E.FIELD_MAX, "the rest are counted");
var units = 0; mx.shown.forEach(function (c) { for (var j = 1; j < c.pts.length; j++) units += Math.abs(c.pts[j][0] - c.pts[j - 1][0]) + Math.abs(c.pts[j][1] - c.pts[j - 1][1]); });
var drawn = mx.runs.reduce(function (s, r) { return s + r.b - r.a; }, 0);
assert.ok(drawn < units, "shared stretches are drawn once: " + drawn + " of " + units + " tiles");
var cover = {}; mx.runs.forEach(function (r) { for (var u = r.a; u < r.b; u += 0.5) { var k = (r.h ? "h" : "v") + r.at + ":" + u; assert.ok(!cover[k], "no stretch drawn twice: " + k); cover[k] = 1; } });
`
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	out, err := exec.Command(node, "-e", script, abs(filepath.Join("web", "world.js")), abs(filepath.Join("web", "edges.js")), abs(filepath.Join("testdata", "farm_fixture.json"))).CombinedOutput()
	if err != nil {
		t.Fatalf("node field channels test failed: %v\n%s", err, out)
	}
}

// The page wires edges.js before garden.js, and garden.js keeps small hooks: the index per farm document, the channels in the
// world layer (under the signs and plots), the hot links' names and a legend row. The info card that listed each edge is gone
// (cursor-grid spec N-D14); its lines are the bead view's "Across projects" section, through GARDEN.known (build step 6).
func TestEdgesWiredIntoGarden(t *testing.T) {
	if i, j := strings.Index(pageHTML, `{{asset "edges.js"}}`), strings.Index(pageHTML, `{{asset "garden.js"}}`); i < 0 || i > j {
		t.Fatal("edges.js must be embedded before garden.js")
	}
	for _, want := range []string{
		"edgeIx=EDGES.index(doc,w);",
		"edges=function(){EDGES.draw(ctx,edgeIx,{level:level,z:cam.z,T:T,night:night,view:WORLD.viewRect(cam,vp,0)});};",
		"if(level===0)edges();",
		"if(level>=3)EDGES.drawField(ctx,edgeIx,p.id,{T:T,night:night,frame:f});",  // under the plants at Bead
		"if(level===2)EDGES.drawField(ctx,edgeIx,p.id,{T:T,night:night,frame:f});", // over the flat plot at Plan
		"EDGES.drawNames(ctx,edgeIx,{level:level,z:cam.z,T:T,night:night,dpr:vp.dpr,view:WORLD.viewRect(cam,vp,0),safe:op.safe});",
		"safe:EDGES.safeView(signView,cam,vp),",
		"WEEDS.aria(weedIx,h)+EDGES.aria(edgeIx,h)",
		"if(r[0]==='channel')EDGES.drawLegend(x,false);", // the key in the day palette (gd-8YEjli6gpl)
		"o.edges=EDGES.lines(edgeIx,wh,beadHref);",
	} {
		if !strings.Contains(pageAppJS+pageSceneJS, want) {
			t.Errorf("garden.js and scene.js missing %s", want)
		}
	}
}

// Irrigation channels in a real browser (gd-CNhEh30Ykf): the card fixture farm with one cross-project edge, plan A's bug
// gd-A2c (gardenio) waiting on gd-kbeadsa1a (kbeads). What is asserted lives in testdata/edges_browser.js;
// GARDENHUB_SHOTS=<dir> saves its screenshots.
func TestEdgesUnderChromium(t *testing.T) {
	underChromium(t, "edges_browser.js", func() *httptest.Server {
		return cardWorldAs(t, time.Now, "", func(src *fakeFarmSrc) {
			src.deps["gd-A2c"] = []beadsapi.Dependency{{BeadID: "gd-A2c", DependsOnID: "gd-kbeadsa1a", Type: "blocks"}}
		})
	})
}

// A scoped viewer's named bridge in a real browser (gd-DgEhq38EnY): the card fixture farm cut to gardenio, coop and music
// (kbeads is not theirs), with plan A's bug gd-A2c waiting on gd-kbeadsa1a, which the cut names only as "kbeads". What is
// asserted lives in testdata/bridge_browser.js; GARDENHUB_SHOTS=<dir> saves its screenshots.
func TestBridgeUnderChromium(t *testing.T) {
	underChromium(t, "bridge_browser.js", func() *httptest.Server {
		return cardWorldCut(t, time.Now, "", []string{"gardenio", "coop", "music"}, func(src *fakeFarmSrc) {
			src.deps["gd-A2c"] = []beadsapi.Dependency{{BeadID: "gd-A2c", DependsOnID: "gd-kbeadsa1a", Type: "blocks"}}
		})
	})
}
