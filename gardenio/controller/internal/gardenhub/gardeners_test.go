package gardenhub

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// Gardeners on their plots (gardenio/docs/homestead-agents-as-people.md sections 4-6) are pure in world.js, so the
// placement contract runs under node: who is at home (idle, no plot, a plot not in the world) and who stands on a plot
// (alive or stuck), need-first order, on an empty tile of its field facing what it tends: beside its task's cell (uncapped;
// two on one task on the nearest two empty tiles), else beside its phase's or the growing bed (capped per level, the rest a
// "+N" badge), never on a task cell, a crate, stubble (a gone step's stub too) or a stake, and on the bottom fence only as the overflow of a full field, the counts the signs read, the look (pose, role group, hat from the biome palette index),
// the outline's Gardeners group, and plans.js keeping gardeners[] across patches.
func TestGardenersUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), P = require(process.argv[2]), A = require(process.argv[3]), assert = require("assert");
function plan(id, project, n) {
  var tasks = []; for (var i = 0; i < n; i++) tasks.push({ id: id + "-t" + i, title: "T" + i, type: "task", state: "in_progress", updated: 1 });
  return { id: id, title: "Plan " + id, project: project, created: 1, activity: 1, counts: { in_progress: n },
    phases: [{ id: id + "-p1", title: "Phase 1", counts: { in_progress: n }, tasks: tasks }], unphased: { id: "", counts: {}, tasks: [] } };
}
var doc = { plans: [plan("pa", "gardenio", 8), plan("pb", "music", 1)], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } };
var w = W.layout(doc, null, { now: 1, readonly: true }), pa = w.index.pa, pb = w.index.pb;
var at = function (plot) { return { plan: plot, phase: "" }; };
function g(id, state, bucket, task, plot, since, extra) { var o = { id: id, agent: "name-" + id, role: "crew", project: "gardenio", state: state, bucket: bucket, task: task, plot: plot ? at(plot) : null, since: since || 0, href: "/garden?bead=" + id }; if (extra) Object.keys(extra).forEach(function (k) { o[k] = extra[k]; }); return o; }
var gs = [
  g("w1", "working", "alive", "pa-t3", "pa", 50), g("w2", "working", "alive", "pa-t3", "pa", 40),    // two on one task: side by side
  g("w3", "working", "alive", "pa", "pa", 30),                                                    // the plan itself: the gate
  g("ask", "yielding", "alive", "pa-t0", "pa", 90), g("down", "failed", "stuck", "pa-t7", "pa", 99),
  g("st", "spawning", "alive", "pa-t5", "pa", 10), g("w4", "working", "alive", "pa-t13", "pa", 20),   // a task past the bed: the gate
  g("idle", "idle", "idle", "pa-t1", "pa", 5),                                                    // idle with a plot: at home
  g("seed", "working", "alive", "s1", null, 5), g("gone", "working", "alive", "nope", "nope", 5),  // no plot; a plot not in the world
  g("vis", "working", "alive", "pb-t0", "pb", 5, { project: "zeta", role: "gardener,plan-gardener" })
];
var s3 = W.gardenerSpots(w, gs, 3), s2 = W.gardenerSpots(w, gs, 2), s1 = W.gardenerSpots(w, gs, 1);
// home: idle, no plot, a plot not in the world; in need order
assert.deepStrictEqual(s3.home, ["gone", "seed", "idle"]);
["idle", "seed", "gone"].forEach(function (id) { assert.deepStrictEqual([s3.spots[id].home, s3.spots[id].field.home], ["project:gardenio", "project:gardenio"], "at home in its own project's field (28.4, 28.12)"); });
assert.deepStrictEqual([s3.fields["project:gardenio"].length, ["gone", "seed", "idle", "w1", "down"].every(function (id) { return s3.fields["project:gardenio"].indexOf(id) >= 0; }), s3.fields["project:zeta"]], [10, true, undefined], "every one a stand, at home or out");
assert.deepStrictEqual(s3.fields["project:"], ["vis"], "zeta has no biome: its gardener stands in the Commons's field (28.3)");
// need first: down, ask, then working by since (st is spawning: the rest)
assert.deepStrictEqual(s3.plots.pa.ids, ["down", "ask", "w4", "w3", "w2", "w1", "st"]);
// on an empty tile of its field, facing what it tends (gd-HB0xs0CTbk, the owner's call in Slack thread gd-FikFgWvLMr; it
// replaces standing on the task's cell, gd-lXIyn1hqFl). The bed starts at x + 2, y + 2 and fills block-major (cursor-grid spec
// 5.4), so t0 is at (2, 2), t3 at (2, 3), t4 at (3, 3), t5 at (4, 3), t7 at (3, 4), and its stake is at (1, 1). Those on a task
// cell take their tiles first, in need order: down beside t7 in its row ((4, 4) would put its head over t5), ask up and right
// of t0 (left of it, its head would cover the stake's pips), w2 left of t3 in the gutter, w1 the nearest tile after that
// (right, past t4 and t5), st right of t5 past w1. Then w4 (its task past the bed) and w3 (the plan) tend the growing bed
// nearest the gate: the nearest free tiles beside its used 3 x 3.
var spot = function (sp, id) { var s = sp.spots[id], p = w.index[s.plot]; return [s.col - p.x, s.row - p.y, s.face, s.cell, s.fence]; };
assert.deepStrictEqual(spot(s3, "down"), [5, 4, "l", "pa-t7", false]);
assert.deepStrictEqual(spot(s3, "ask"), [3, 1, "l", "pa-t0", false]);
assert.deepStrictEqual(spot(s3, "w2"), [1, 3, "r", "pa-t3", false]);
assert.deepStrictEqual(spot(s3, "w1"), [5, 3, "l", "pa-t3", false], "two on one task: the nearest two empty tiles");
assert.deepStrictEqual(spot(s3, "st"), [6, 3, "l", "pa-t5", false], "spawning on its task, not capped");
assert.deepStrictEqual(spot(s3, "w4"), [5, 2, "l", "", false], "its task is not on the plot: beside the growing bed");
assert.deepStrictEqual(spot(s3, "w3"), [6, 2, "l", "", false], "the plan itself: the same bed, the next free tile");
// sound(sp, p, gs): every gardener shown on plot p stands on its own tile, never on a mark (a task cell, a crate, stubble, a
// stake); in the field it is inside the fence and beside what it tends (not in its columns), facing it; on the fence (the
// overflow) its feet are on the fence row. Feet on the row it stands on: the 24 px sprite's top 8 px above it.
function sound(sp, p, list, what) {
  var marks = W.plotMarks(p), seen = {}, byId = {};
  list.forEach(function (x) { byId[x.id] = x; });
  assert.ok(Object.keys(marks).length > 0, "the plot has marks");
  sp.plots[p.id].shown.forEach(function (id) {
    var s = sp.spots[id], k = s.col + ":" + s.row, c = s.cell ? w.index[s.cell] : null;
    var t = c ? { x0: c.x, x1: c.x } : W.bedBox(p, byId[id].task), face = t.x0 > s.col ? "r" : t.x1 < s.col ? "l" : s.fence && "r";   // on the fence right under it: as drawn
    assert.ok(!marks[k], what + ": " + id + " stands on a mark at " + k);
    assert.ok(!seen[k], what + ": " + id + " shares its tile " + k); seen[k] = 1;
    assert.deepStrictEqual([s.x, s.y], [16 * s.col, 16 * s.row - 8], what + ": " + id + "'s sprite");
    assert.strictEqual(s.face, face, what + ": " + id + " faces what it tends");
    if (s.fence) { assert.strictEqual(s.row, p.y + W.SLOT.h - 1, what + ": " + id + " on the fence row"); return; }
    assert.ok(s.col > p.x && s.col < p.x + W.SLOT.w - 1 && s.row > p.y && s.row < p.y + W.SLOT.h - 1, what + ": " + id + " inside the fence");
    assert.ok(s.col < t.x0 || s.col > t.x1, what + ": " + id + " beside what it tends, not over or under it");
  });
}
sound(s3, pa, gs, "the fixture"); sound(s3, w.index.pb, gs, "the visitor");
assert.ok(s3.plots.pa.shown.every(function (id) { return !s3.spots[id].fence; }), "a field with room: nobody on the fence");
// a harvest cell's badges sit on its right side, so the tile right of it is a last resort (gd-USSMfRaAol): one gardener on t4
// (3, 3), between t3 and t5, takes (5, 3), right of t5, while t5 grows; once t5 is a harvest cell it takes (1, 3) and faces right
var one = [g("solo", "working", "alive", "pa-t4", "pa", 5)];
assert.deepStrictEqual(spot(W.gardenerSpots(w, one, 3), "solo"), [5, 3, "l", "pa-t4", false], "beside t4, right first");
var hdoc = JSON.parse(JSON.stringify(doc)); Object.assign(hdoc.plans[0].phases[0].tasks[5], { state: "closed", harvest: { base: "sheaf", grade: { score: 4, of: 5 } } });
var hw = W.layout(hdoc, null, { now: 1, readonly: true }), hs = W.gardenerSpots(hw, one, 3).spots.solo;
assert.deepStrictEqual([hs.col - hw.index.pa.x, hs.row - hw.index.pa.y, hs.face], [1, 3, "r"], "never right of a harvest cell when a tile is free");
// a step that left its bed keeps its cell as a stub (bed.gone, drawn like stubble): nobody stands on it. pg-t0 is at cell 0 (2, 2)
// and the step gone from cell 1 is at (3, 2), right of it; left of pg-t0 its head would cover the stake, so without the stub
// marked the gardener would stand on it. The stub's tile comes from the cell, not from W.plotMarks, which sound() trusts.
var wg = W.layout({ plans: [plan("pg", "gardenio", 1)], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } }, null,
  { now: 1, readonly: true, cells: { pg: { "pg-p1": { "pg-t0": 0, "pg-left": 1 } } } }), pg = wg.index.pg;
assert.deepStrictEqual(pg.beds[0].gone, [1], "the fixture has a gone cell");
var gg = [g("gt", "working", "alive", "pg-t0", "pg", 5)], gsp = W.gardenerSpots(wg, gg, 3).spots.gt;
assert.ok(gsp.shown && !gsp.fence, "beside its task in the field");
assert.notDeepStrictEqual([gsp.col - pg.x, gsp.row - pg.y], [3, 2], "the gardener does not stand on the gone step's stub");
assert.ok(gsp.col !== pg.x + 2, "beside pg-t0, not over or under it");
// the cap counts those not on a task cell only: everyone on a task is drawn at Plan and Bead; nothing drawn at Project, but counted
assert.deepStrictEqual(s3.plots.pa.shown, ["down", "ask", "w4", "w3", "w2", "w1", "st"]); assert.strictEqual(s3.plots.pa.more, 0);
assert.deepStrictEqual(s2.plots.pa.shown, s3.plots.pa.shown); assert.strictEqual(s2.plots.pa.more, 0);
s2.plots.pa.shown.forEach(function (id) { assert.deepStrictEqual(s2.spots[id], s3.spots[id], "a gardener shown at both levels stands in one place: " + id); });
assert.deepStrictEqual(s1.plots.pa.shown, []); assert.strictEqual(s1.plots.pa.more, 7);
assert.deepStrictEqual(s3.plots.pa.counts, { all: 7, work: 5, ask: 1, down: 1 });
assert.deepStrictEqual(s3.biomes["project:gardenio"], { all: 7, work: 5, ask: 1, down: 1 });
assert.deepStrictEqual(s3.biomes["project:music"], { all: 1, work: 1, ask: 0, down: 0 }, "a visitor counts where it works");
// the cap and "+N": six beside the bed at Bead (three at Plan), the rest behind the badge, at the gate when nobody is on the fence
var many = []; for (var i = 0; i < 32; i++) many.push(g("m" + i, "working", "alive", "pa-t13", "pa", i));
var full = W.gardenerSpots(w, many, 3), few = W.gardenerSpots(w, many, 2); W.GARDENER_CAP[3] = 32;
var wide = W.gardenerSpots(w, many, 3); W.GARDENER_CAP[3] = 6;
assert.strictEqual(full.plots.pa.shown.length, 6); assert.strictEqual(full.plots.pa.more, 26); assert.strictEqual(full.plots.pa.moreX / 16 - pa.x, 1, "the badge at the gate");
assert.strictEqual(full.plots.pa.moreY, 16 * (pa.y + W.SLOT.h - 1), "on the fence row");
assert.strictEqual(few.plots.pa.shown.length, 3); assert.strictEqual(few.plots.pa.more, 29);
sound(full, pa, many, "six beside the bed"); sound(wide, pa, many, "thirty-two beside the bed");
assert.strictEqual(wide.plots.pa.shown.length, 32); assert.strictEqual(wide.plots.pa.more, 0);
assert.ok(wide.plots.pa.shown.every(function (id) { return !wide.spots[id].fence; }), "thirty-two fit in the field");
// a phase's gardener tends its own bed; with no step in progress the nearest bed with steps; a plot without beds has no bed box
var two = plan("pc", "gardenio", 2); two.phases.push({ id: "pc-p2", title: "Phase 2", counts: { open: 1 }, tasks: [{ id: "pc-u0", title: "U0", type: "task", state: "open", updated: 1 }] });
var w2 = W.layout({ plans: [two], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } }, null, { now: 1, readonly: true }), pc = w2.index.pc;
var b2 = pc.beds[1];
assert.deepStrictEqual(W.bedBox(pc, "pc-p2"), { x0: b2.x, x1: b2.x + 2, y0: b2.y, y1: b2.y + 2 }, "its phase's bed, the used 3 x 3");
assert.deepStrictEqual(W.bedBox(pc, "pc"), { x0: pc.beds[0].x, x1: pc.beds[0].x + 2, y0: pc.beds[0].y, y1: pc.beds[0].y + 2 }, "the growing bed");
assert.strictEqual(W.bedBox({ x: 0, y: 0, beds: [] }, "x"), null);
// thirty-two on one task: never capped, each on the nearest empty tile beside t4, nearest first, never on a mark
var crowd = []; for (i = 0; i < 32; i++) crowd.push(g("c" + i, "working", "alive", "pa-t4", "pa", i));
var cs = W.gardenerSpots(w, crowd, 2);
assert.strictEqual(cs.plots.pa.shown.length, 32); assert.strictEqual(cs.plots.pa.more, 0);
sound(cs, pa, crowd, "a crowd on t4");
var reach = function (id) { var s = cs.spots[id]; return Math.abs(s.col - (pa.x + 3)) + Math.abs(s.row - (pa.y + 3)); };
cs.plots.pa.shown.forEach(function (id, k) { if (k) assert.ok(reach(id) >= reach(cs.plots.pa.shown[k - 1]) - 1, "nearest first: " + id); });
// a full field falls back to the fence: t3's tenders fill every tile not a mark and not in t3's column (900 inside the fence,
// 9 marks, 27 more in column 2), then the fence from the gate (the overflow), facing t3 on its left; two on the plan find no
// tile beside the bed either and stand on the fence after them, under the cap; the rest are behind the "+N"
var packed = []; for (i = 0; i < 900 - 9 - 27 + 6; i++) packed.push(g("f" + i, "working", "alive", "pa-t3", "pa", i));
packed.push(g("fp0", "working", "alive", "pa", "pa", 5000), g("fp1", "working", "alive", "pa", "pa", 5001));
var fs = W.gardenerSpots(w, packed, 3), fence = fs.plots.pa.shown.filter(function (id) { return fs.spots[id].fence; });
sound(fs, pa, packed, "a full field");
assert.strictEqual(fs.plots.pa.shown.length, packed.length); assert.strictEqual(fs.plots.pa.more, 0);
assert.deepStrictEqual(fence, ["f864", "f865", "f866", "f867", "f868", "f869", "fp0", "fp1"], "only the overflow on the fence");
assert.deepStrictEqual(fence.map(function (id) { return fs.spots[id].col - pa.x; }), [1, 2, 3, 4, 5, 6, 7, 8], "from the gate, the nearest free fence column");
assert.deepStrictEqual(fence.map(function (id) { return fs.spots[id].face; }), ["r", "r", "l", "l", "l", "l", "l", "l"], "facing t3 (column 2) and the bed from the fence");
packed.push(g("fx", "working", "alive", "pa", "pa", 6000)); for (i = 0; i < 30; i++) packed.push(g("fy" + i, "working", "alive", "pa-t3", "pa", 7000 + i));
var over = W.gardenerSpots(w, packed, 3);
assert.strictEqual(over.plots.pa.more, 9, "the fence full with 24 more of t3's: their last six and the plan's three behind the badge");
assert.strictEqual(over.plots.pa.moreX / 16 - pa.x, W.SLOT.w - 2, "the badge on the last fence column when the fence is full");
// no world, no gardeners
assert.deepStrictEqual(W.gardenerSpots(null, gs, 3).home.length, gs.length); assert.deepStrictEqual(W.gardenerSpots(w, null, 3).home, []);
// the look (5.2, gd-wJVEevAYPP): pose from the state, group from the first role token with one, its own shirt by its id, its plan's hat
assert.deepStrictEqual(W.gardenerLook(gs[0]), { pose: "work", group: "crew", shirt: W.h32("w1", 3) % 8, hat: W.h32("pa", 2) % 8 });
assert.notStrictEqual(W.gardenerLook(gs[0]).hat, W.gardenerLook(g("x", "working", "alive", "pb-t0", "pb")).hat, "two plans, two hats");
assert.strictEqual(W.gardenerLook(g("x", "working", "alive", "pa-t3", "pa")).hat, W.gardenerLook(gs[0]).hat, "one plan, one hat for everyone on it");
assert.ok(W.gardenerLook(g("x", "working", "alive", "pa-t3", "pa")).shirt !== W.gardenerLook(g("y", "working", "alive", "pa-t3", "pa")).shirt || W.h32("x", 3) % 8 === W.h32("y", 3) % 8, "each its own shirt");
assert.strictEqual(W.gardenerLook({ id: "x", role: "crew", project: "gardenio" }).hat, 8, "no plot: the straw hat, whatever the project");
assert.strictEqual(W.gardenerLook({ id: "x", role: "steward", project: "gardenio" }).hat, w.biomes.filter(function (b) { return b.key === "gardenio"; })[0].palette, "a steward's hat is its land's biome");
assert.strictEqual(W.gardenerLook({ id: "x", role: "crew" }).shirt, W.gardenerLook({ id: "x", role: "thread", plot: { plan: "pa" } }).shirt, "the shirt index is by id alone");
assert.deepStrictEqual(A.gardenerWear("crew", 3), A.GD_WEAR[3]); assert.deepStrictEqual(A.gardenerWear("other", 7), A.GD_WEAR[7]); assert.deepStrictEqual(A.gardenerWear("thread", 3), A.GD_SHIRTS.thread); assert.deepStrictEqual(A.gardenerWear("steward", 3), A.GD_SHIRTS.steward);
assert.strictEqual(A.GD_WEAR.length, 8); assert.strictEqual(A.gardenerPal(false, "crew", 0, 5).S, A.GD_WEAR[5][0]); assert.notStrictEqual(A.gardenerPal(false, "crew", 0, 5).S, A.gardenerPal(false, "crew", 0, 0).S, "the palette is per shirt");
assert.strictEqual(W.gardenerLook(g("x", "yielding", "alive")).pose, "ask"); assert.strictEqual(W.gardenerLook(g("x", "rate_limited", "stuck")).pose, "down");
assert.strictEqual(W.gardenerLook(g("x", "spawning", "alive")).pose, "stand"); assert.strictEqual(W.gardenerLook(g("x", "idle", "idle")).pose, "stand");
assert.strictEqual(W.gardenerLook(gs[10]).group, "gardener", "a comma list: the first token with a group");
[["crew", "crew"], ["plan-captain", "gardener"], ["release-captain", "gardener"], ["steward", "steward"], ["cicd", "infra"], ["infra", "infra"], ["reviewer", "other"], ["thread", "thread"], ["dm", "thread"], ["crew,thread", "crew"], ["", "other"], ["weird,cicd", "infra"]].forEach(function (r) {
  assert.strictEqual(W.gardenerLook({ role: r[0] }).group, r[1], r[0]);
});
assert.strictEqual(W.gardenerLook({ role: "steward", project: "" }).hat, 8, "a steward with no project: the straw hat"); assert.strictEqual(W.gardenerLook({ role: "steward", project: " GardenIO " }).hat, W.gardenerLook({ role: "steward", project: "gardenio" }).hat);
assert.strictEqual(A.GD_HATS.length, 9); assert.strictEqual(A.GD_HATS[4], "#fffaf0", "no green hat on green grass");
assert.deepStrictEqual([W.homeFor(gs[0]), W.homeFor({ role: "crew" }), W.homeFor({ role: "thread", project: "gardenio" })], ["project:gardenio", "project:", "project:"], "its project's barn; the Commons's without one and for a thread agent (H-D10)");
// the steward (Lands 9.8, section 9.2): its own role group, at home at its land's homestead (the biome's sign), never on a plot
// or at the barn; one figure per land from the cost row's steward, a live run of it merged in
var gb = w.index["project:gardenio"], spot = W.stewardSpot(gb);
assert.strictEqual(W.homeFor({ role: "steward", project: " GardenIO " }), "project:gardenio"); assert.strictEqual(W.homeFor({ role: "steward" }), "project:", "no project: the Commons's barn");
assert.strictEqual(W.homeFor({ role: "crew,steward", project: "gardenio" }), "project:gardenio"); assert.strictEqual(W.gardenerLook({ role: "steward" }).group, "steward");
assert.deepStrictEqual(spot, { col: gb.sign.x + gb.sign.w, row: gb.sign.y + gb.sign.h - 1 }, "right of the sign's plate, feet on its posts' row");
var run = g("stw-run", "working", "alive", "pa-t2", "pa", 70, { role: "steward", agent: "gardenio-steward-ab1", chat: "/garden/c/bead/stw-run" });
var withRun = gs.concat([run]), sr3 = W.gardenerSpots(w, withRun, 3);
assert.deepStrictEqual(sr3.spots["stw-run"], { land: "project:gardenio" }); assert.ok(sr3.home.indexOf("stw-run") < 0 && sr3.plots.pa.ids.indexOf("stw-run") < 0, "neither at the barn nor on its task's plot");
var memory = { kind: "in_flight", text: "Plan X: crew on step 2" }, parked = { kind: "parked", text: "Waiting on you: approve plan X (d-9)", href: "/garden?bead=d-9" };
var cost = { projects: [{ project: "gardenio", steward: { id: "trg-1", href: "/garden?bead=trg-1", agent: "gardenio-steward", max_runs_per_day: 24, runs_today: 3, capped: false, reason: memory } },
  { project: "music", steward: { id: "trg-2", href: "/garden?bead=trg-2", agent: "music-steward", max_runs_per_day: 24, runs_today: 24, capped: true, reason: { kind: "sleeping", text: "Parked: the purse is empty" } } },
  { project: "nowhere", steward: { id: "trg-3", href: "/garden?bead=trg-3", agent: "nowhere-steward", reason: null } }, { project: "zeta" }] };
var ss = W.stewardState(w, cost, gs, W.gardenerState(w, gs));
assert.deepStrictEqual(ss.list.map(function (e) { return e.id; }), ["steward:gardenio", "steward:music"], "one per land on the map with a steward; none for a land without a biome");
var sg = ss.byId["steward:gardenio"];
assert.deepStrictEqual([sg.x, sg.y, sg.look.group, sg.look.pose], [spot.col * W.TILE, W.TILE * spot.row - 8, "steward", "stand"]);
assert.deepStrictEqual([sg.g.agent, sg.g.href, sg.g.reason, sg.g.run, W.stewardWord(sg.g)], ["gardenio-steward", "/garden?bead=trg-1", memory, "", "Minding the land"]);
assert.strictEqual(ss.byId["steward:music"].look.pose, "sleep", "parked at the wall: asleep");
assert.deepStrictEqual(ss.hots.map(function (h) { return h.kind + ":" + h.id + ":" + h.lv; }), ["steward:gardenio:12", "steward:music:12"], "a link at Plan and Bead; no bubble without a letter");
var sc = W.stewardCard(ss.byId["steward:music"].g, "/b", function () { return "4m"; });
assert.deepStrictEqual(sc.lines.slice(0, 4).map(function (l) { return l[1]; }), ["Parked: the purse is empty", "Today's 24 runs are used; it wakes again at 00:00 UTC", "steward · music", "Asleep"]);
assert.ok(/^Asleep · Parked: the purse is empty · steward of music/.test(sc.tip), sc.tip);
assert.strictEqual(W.gardenerTip({ kind: "steward", g: sg.g }, null, w, "/b", function () { return "4m"; }).text, W.stewardCard(sg.g, "/b", function () { return "4m"; }).tip);
// a live run: at work, its thread and bead, the memory line kept under it; a parked memory line: the bubble to its letter
var sl = W.stewardState(w, cost, withRun, W.gardenerState(w, withRun)).byId["steward:gardenio"];
assert.deepStrictEqual([sl.look.pose, sl.g.agent, sl.g.href, sl.g.chat, sl.g.run, sl.g.reason, sl.g.note], ["work", "gardenio-steward-ab1", "/garden?bead=stw-run", "/garden/c/bead/stw-run", "stw-run", null, memory]);
var rl = W.stewardCard(sl.g, "/b", function () { return "4m"; }).lines;
assert.deepStrictEqual(rl[0], ["rsn rsn-in_flight", "Plan X: crew on step 2"]); assert.strictEqual(rl[rl.length - 2][1], "At work · 4m");
assert.deepStrictEqual(rl[rl.length - 1][1].map(function (k) { return k.text; }), ["Talk to the steward", "Open bead", "Thread in the Commons"]);
assert.deepStrictEqual(sc.lines[sc.lines.length - 1][1][0], { text: "Talk to the steward", href: "/b/garden/c/s/music", cls: "gsteward" }, "the steward view first, with no run live too (gd-LisW87zrnf)");
cost.projects[0].steward.reason = parked;
var sp2 = W.stewardState(w, cost, gs, null), pk = sp2.byId["steward:gardenio"];
assert.deepStrictEqual([pk.look.pose, W.stewardWord(pk.g)], ["ask", "Waiting on you"]);
assert.deepStrictEqual(sp2.hots.filter(function (h) { return h.kind === "gask"; }).map(function (h) { return h.id; }), ["steward:gardenio"], "the bubble's button to the letter");
assert.deepStrictEqual(W.stewardState(w, null, withRun, null).list.map(function (e) { return e.g.agent; }), ["gardenio-steward-ab1"], "no cost rows: a live run still stands at its homestead");
assert.deepStrictEqual(W.stewardState(w, null, gs, null).list, []);
var kidsOf = function (o) { return [].concat.apply([], Object.keys(o).map(function (h) { return o[h].children; })); };   // every project's Gardeners group (28.7)
var ow = kidsOf(W.gardenerOutline(w, withRun, W.gardenerSpots(w, withRun, 2), sr3)).filter(function (e) { return e.id === "stw-run"; })[0];
assert.deepStrictEqual([ow.hot, ow.level, ow.plot], ["steward:gardenio", 2, "At the homestead of " + gb.name]);
// the card's links: the bead, and the thread in the Commons named for where it goes, never the retired "Chat" (gd-2Y9GQqK0Ok)
var links = function (gd) { return W.gardenerCard(gd, { 3: s3 }, w, "/b", function () { return "4m"; }).lines.filter(function (l) { return l[0] === "why"; })[0][1]; };
assert.deepStrictEqual(links(g("t1", "working", "alive", "", null, 5, { chat: "/garden/c/bead/t1" })),
  [{ text: "Open bead", href: "/b/garden?bead=t1" }, { text: "Thread in the Commons", href: "/b/garden/c/bead/t1", cls: "gchat", id: "t1" }]);
assert.deepStrictEqual(links(gs[0]).map(function (k) { return k.text; }), ["Open bead"], "no thread, no thread link");
// the plan's bead view lists who stands on its plot (cursor-grid spec 6.4): need first, each in words and what it tends; without
// gardeners[], its growing steps' assignees by name
var st = W.gardenerState(w, gs), who = W.plotWho(st, pa);
assert.deepStrictEqual(who.map(function (x) { return x.id; }), ["down", "ask", "w4", "w3", "w2", "w1", "st"], "everyone on the plot, past the sprite cap too");
assert.deepStrictEqual(who[1], { id: "ask", name: "name-ask", word: "Waiting on a human", task: "" });
assert.strictEqual(W.plotWho(null, pb).map(function (x) { return x.name; }).length, 0, "no assignees, nobody");
pa.beds[0].cells[0].task.assignee = "flo"; pa.beds[0].cells[1].task.assignee = "flo"; pa.beds[0].cells[2].task.assignee = "rae";
assert.deepStrictEqual(W.plotWho(null, pa), [{ name: "flo" }, { name: "rae" }], "each assignee once");
// the outline's Gardeners groups (28.7): one per project, in its home cell, need first then by name; hots the page has at the entry's level
var og = W.gardenerOutline(w, gs, s2, s3), o = { children: kidsOf(og) }, byId = {}; o.children.forEach(function (e) { byId[e.id] = e; });
assert.strictEqual(o.children.length, gs.length, "everyone, once: " + Object.keys(og));
Object.keys(og).forEach(function (h) { assert.deepStrictEqual([og[h].kind, og[h].hot, og[h].n], ["gardeners", "gardeners:" + h.slice(5), og[h].children.length]); assert.ok(w.index[h] && w.index[h].kind === "home", "under a home cell: " + h); });
assert.deepStrictEqual(og["home:" + pa.project].children.slice(0, 2).map(function (e) { return e.id; }), ["down", "ask"]);
assert.deepStrictEqual([byId.down.hot, byId.down.level, byId.down.plot], ["gardener:down", 2, "Plan pa"]);
assert.deepStrictEqual([byId.w2.hot, byId.w2.level, byId.w2.rect], ["gardener:w2", 2, { x: pa.x + 1, y: pa.y + 2, w: 1, h: 2 }], "on its task: at Plan too, the rect over its sprite on its tile");
assert.deepStrictEqual([byId.st.hot, byId.st.level], ["gardener:st", 2], "on its task: never behind the badge");
var om = kidsOf(W.gardenerOutline(w, many, W.gardenerSpots(w, many, 2), full)), obm = {}; om.forEach(function (e) { obm[e.id] = e; });
assert.deepStrictEqual([obm.m4.hot, obm.m4.level], ["gardener:m4", 3], "one beside the bed past the Plan cap: shown at Bead only");
assert.deepStrictEqual([obm.m6.hot, obm.m6.level], ["gmore:pa@3", 3], "past both caps: the badge stands in");
assert.deepStrictEqual([byId.idle.hot, byId.idle.barn, byId.idle.plot], ["bead:field:idle", true, "In the field of gardenio"], "at home: its field figure's link, and the field it stands in (28.12)");
// where a gardener stands, in the Porch's words (28.12): its field, and its plot too when it is out; a stand's own words
var gst = W.gardenerState(w, gs);
assert.deepStrictEqual([W.fieldWhere(gst, gs.filter(function (x) { return x.id === "down"; })[0]), W.fieldWhere(gst, gs.filter(function (x) { return x.id === "idle"; })[0]), W.fieldWhere(null, gs[0])],
  ["In the field of gardenio and on Plan pa", "In the field of gardenio", ""]);
assert.deepStrictEqual([W.standLine({ id: "a", agent: "rae" }, "Dig the bed"), W.standLine({ id: "a" }, ""), W.standWord("")], ["rae — working: Dig the bed", "a — resting", "resting"]);
o.children.forEach(function (e) { if (!e.barn) assert.ok(e.rect && e.rect.w > 0 && e.rect.h > 0, "a rect to fly to: " + e.id); });
// a thread agent (5b, gd-1kFVOmUjiK): its own shirt; at home its outline entry, card, tip and barn link say the thread it answers;
// one with no thread, or a crew with one, stands in its field ("In the field of ..."); on a plot it is that plot's, as anyone is
var th = g("th", "working", "alive", "", null, 7, { role: "thread", chat: "/garden/c/bead/th", thread_title: "a pen for the goats" });
var thw = [th, g("thn", "working", "alive", "", null, 7, { role: "thread" }), g("cr", "working", "alive", "", null, 7, { chat: "/garden/c/bead/cr", thread_title: "x" }),
  g("thp", "working", "alive", "pa-t2", "pa", 7, { role: "thread", chat: "/garden/c/bead/thp", thread_title: "y" })];
var tst = W.gardenerState(w, thw), tb = {};
kidsOf(W.gardenerOutline(w, thw, tst[2], tst[3])).forEach(function (e) { tb[e.id] = e; });
assert.deepStrictEqual([tb.th.plot, tb.thn.plot, tb.cr.plot, tb.thp.plot], ["In the Commons: a pen for the goats", "In the field", "In the field of gardenio", "Plan pa"], "this world has no Commons, so a thread agent with no thread has no field to name");
assert.deepStrictEqual([W.isThread(th), W.isThread({ role: "crew,dm" }), W.isThread({ role: "threads" })], [true, true, false]);
var tc = W.gardenerCard(th, tst, w, "", function () { return "4m"; });
assert.ok(tc.lines.some(function (l) { return l[1] === "In the Commons: a pen for the goats"; }), "the card: " + JSON.stringify(tc.lines));
assert.ok(tc.lines.some(function (l) { return l[0] === "why" && l[1].some(function (k) { return k.cls === "gchat" && k.href === "/garden/c/bead/th"; }); }), "and the link to its thread");
assert.ok(/ · in the Commons: a pen for the goats · /.test(tc.tip), "the tip: " + tc.tip);
assert.ok(!/Commons/.test(W.gardenerCard(thw[3], tst, w, "", function () { return "4m"; }).tip), "on a plot it tends its step");
var hi = W.gardenerField(tst, "", "", 42).filter(function (it) { return it.id === W.FIELD + "th"; })[0];
assert.deepStrictEqual([hi.where, hi.aria, hi.plant], ["In the Commons: a pen for the goats", "name-th, at work in the Commons on a pen for the goats: watch in the Glass", null], "its field stand's link, leading with its thread and saying where it goes (H-D21); its thread head is no plant");
assert.strictEqual(hi.lookNow().group, "thread", "the field figure wears its look, with its doing pose (lookOf)");
// plans.js keeps gardeners[]: the snapshot, a patch's keyed set, and a document from before the field
var d = P.normalize({ v: 1, plans: [], gardeners: [gs[0], gs[1]] });
assert.strictEqual(d.gardeners.length, 2); assert.deepStrictEqual(P.normalize({ v: 1 }).gardeners, []);
var d2 = P.applyPatch(d, { v: 2, prev: 1, gardeners: { up: [g("w1", "yielding", "alive", "pa-t3", "pa", 60), gs[2]], rm: ["w2"] } });
assert.deepStrictEqual(d2.gardeners.map(function (x) { return x.id + ":" + x.state; }).sort(), ["w1:yielding", "w3:working"]);
assert.strictEqual(P.applyPatch(d2, { v: 3, prev: 2 }).gardeners, d2.gardeners, "a patch without gardeners keeps them");
// every pose draws, with and without the sprite cache, in both schemes; the legend rows draw the real sprite
var fills = 0, blits = 0, ctx = { fillRect: function () { fills++; }, drawImage: function () { blits++; }, set fillStyle(v) { if (!/^#[0-9a-f]{6}$/i.test(v)) throw new Error("bad colour " + v); } };
var sp = A.spriteCache(function () { return { getContext: function () { return ctx; } }; });
["work", "stand", "ask", "down"].forEach(function (pose) { ["crew", "gardener", "infra", "other"].forEach(function (grp) { [0, 4, 8].forEach(function (hat) { [false, true].forEach(function (n) {
  A.drawGardener(ctx, { pose: pose, group: grp, hat: hat }, 32, 48, 1, n, sp); A.drawGardener(ctx, { pose: pose, group: grp, hat: hat }, 32, 48, 0, n);
}); }); }); });
assert.ok(blits > 0 && fills > 0); assert.ok(sp.size() <= 5 * 4 * 3 * 2, "one bitmap per pose, frame, group, hat and scheme: " + sp.size());
// facing left (gd-HB0xs0CTbk): the figure mirrored in place about its 16 px middle, its own bitmap in the cache as the truck's
// TRUCK.l; "r" and no facing are the figure as drawn
var n0 = sp.size(), wk = { pose: "work", group: "crew", hat: 0 };
A.drawGardener(ctx, wk, 32, 48, 1, false, sp, "l"); assert.strictEqual(sp.size(), n0 + 1, "one more bitmap: the left-facing one");
A.drawGardener(ctx, wk, 32, 48, 1, false, sp, "l"); A.drawGardener(ctx, wk, 32, 48, 1, false, sp, "r"); assert.strictEqual(sp.size(), n0 + 1, "cached; r is the figure as drawn");
var px = function (face) { var out = [], st = "", rc = { set fillStyle(v) { st = v; }, fillRect: function (x, y, w, h) { for (var i = 0; i < w; i++) for (var j = 0; j < h; j++) out.push(st + "@" + (face === "l" ? 2 * 40 - 1 - (x + i) : x + i) + "," + (y + j)); } }; A.drawGardener(rc, wk, 32, 48, 1, false, null, face); return out.sort(); };
assert.deepStrictEqual(px("l"), px("r"), "every pixel of the left-facing figure is the right-facing one's, mirrored about x + 8");
assert.notDeepStrictEqual(px("r"), px("r").map(function (k) { var a = k.split("@"), xy = a[1].split(","); return a[0] + "@" + (79 - +xy[0]) + "," + xy[1]; }).sort(), "the figure is not symmetric, so the mirror shows");
["work", "stand", "down", "sleep", "wilt", "ask"].forEach(function (pose) { [false, true].forEach(function (n) { A.drawGardener(ctx, { pose: pose, group: "steward", hat: 2 }, 32, 48, 1, n, sp); }); });
A.drawStewards(ctx, [{ id: "s", x: 0, y: 0, look: { pose: "work", group: "steward", hat: 0 } }], 1, false, sp);
assert.ok(A.GD_SHIRTS.steward && A.GDS_MAPS.stand && A.GDS_MAPS.stand.join("") !== A.GD_MAPS.stand.join(""), "the steward has its own sprite");
// the doing poses (4c): two frames each that differ, drawn at both frames; think is the stand under its bubble
["read", "test", "build", "git", "beads", "deploy", "web", "delegate", "waiting"].forEach(function (p) {
  assert.ok(A.GD_MAPS[p + "0"] && A.GD_MAPS[p + "1"] && A.GD_MAPS[p + "0"].join("") !== A.GD_MAPS[p + "1"].join(""), p + " has two frames");
  assert.ok(A.GD_MAPS[p + "0"].length === 26 && A.GD_MAPS[p + "0"].every(function (r) { return r.length === 22; }), p + " is padded to 22 x 26");
  [0, 1].forEach(function (f) { var b0 = blits; A.drawGardener(ctx, { pose: p, group: "crew", hat: 1 }, 32, 48, f, false, sp); assert.strictEqual(blits, b0 + 1); });
});
// 4f (gd-1EMXaKkRPI): told apart at Bead zoom, one CSS px a pixel. Every prop stays in the padding (columns 2 to 19, rows 2 to
// 23), so its outline and rim fit and nothing rises over the hat; any two of the hoe and the doing poses differ by 40 or more
// pixels of silhouette in the still frame (Plan, reduced motion), where 4c's crate and clipboard differed by 2; and each prop's
// colour is in both its frames and 10 or more (CIE76) from every shirt and every ground, by day and by night.
var DO = ["work", "read", "test", "build", "git", "beads", "deploy", "web", "delegate", "waiting"];
DO.forEach(function (p) { [0, 1].forEach(function (f) { A.GD_MAPS[p + f].forEach(function (r, y) { for (var x = 0; x < r.length; x++) if (!/[.k@]/.test(r[x])) assert.ok(x >= 2 && x <= 19 && y >= 2 && y <= 23, p + f + " at " + x + "," + y); }); }); });
function sil(m) { return m.map(function (r) { return r.replace(/[^.]/g, "#"); }).join(""); }
DO.forEach(function (p, i) { DO.slice(i + 1).forEach(function (q) {
  var a = sil(A.GD_MAPS[p + "0"]), b = sil(A.GD_MAPS[q + "0"]), n = 0; for (var k = 0; k < a.length; k++) if (a[k] !== b[k]) n++;
  assert.ok(n >= 40, p + " and " + q + " differ by " + n + " px of silhouette");
}); });
function lab(h) {
  var c = [1, 3, 5].map(function (i) { var v = parseInt(h.substr(i, 2), 16) / 255; return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); });
  var f = function (t) { return t > 0.008856 ? Math.cbrt(t) : 7.787 * t + 16 / 116; };
  var X = f((c[0] * 0.4124 + c[1] * 0.3576 + c[2] * 0.1805) / 0.95047), Y = f(c[0] * 0.2126 + c[1] * 0.7152 + c[2] * 0.0722), Z = f((c[0] * 0.0193 + c[1] * 0.1192 + c[2] * 0.9505) / 1.08883);
  return [116 * Y - 16, 500 * (X - Y), 200 * (Y - Z)];
}
function dE(x, y) { var a = lab(x), b = lab(y); return Math.sqrt(Math.pow(a[0] - b[0], 2) + Math.pow(a[1] - b[1], 2) + Math.pow(a[2] - b[2], 2)); }
Object.keys(A.DOING_PROP).forEach(function (p) {
  var c = A.DOING_PROP[p];
  [0, 1].forEach(function (f) { assert.ok(A.GD_MAPS[p + f].join("").indexOf(c) >= 0, p + f + " shows its prop colour " + c); });
  [false, true].forEach(function (n) {
    var near = [];
    for (var si = 0; si < A.GD_WEAR.length; si++) near.push(["shirt " + si, A.gardenerPal(n, "crew", 0, si).S]);
    ["thread", "steward"].forEach(function (grp) { var gp = A.gardenerPal(n, grp, 0); near.push(["shirt " + grp, gp.S]); });
    for (var i = 0; i < 8; i++) { var bp = A.biomePalette(n, i); ["g", "h", "d"].forEach(function (k) { near.push([A.BIOME_PALS[i].name + " " + k, bp[k]]); }); }
    var col = A.gardenerPal(n, "crew", 0)[c];
    near.forEach(function (o) { var d = dE(col, o[1]); assert.ok(d >= 10, p + "'s " + c + " " + col + " on " + o[0] + " " + o[1] + (n ? " at night" : "") + ": dE " + d.toFixed(1)); });
  });
});
// a thread agent's pink shirt (5b) and the steward's raspberry tell apart from each other and from every gardener's own shirt, by day and at night
[false, true].forEach(function (n) {
  var d = dE(A.gardenerPal(n, "thread", 0).S, A.gardenerPal(n, "steward", 0).S); assert.ok(d >= 15, "shirt thread by steward" + (n ? " at night" : "") + ": dE " + d.toFixed(1));
  ["thread", "steward"].forEach(function (grp) { for (var si = 0; si < A.GD_WEAR.length; si++) { var e = dE(A.gardenerPal(n, grp, 0).S, A.gardenerPal(n, "crew", 0, si).S); assert.ok(e >= 15, "shirt " + grp + " by shirt " + si + (n ? " at night" : "") + ": dE " + e.toFixed(1)); } });
});
var f1 = fills; A.drawGardener(ctx, { pose: "think", group: "crew", hat: 1 }, 32, 48, 0, false); assert.ok(fills - f1 > 100, "think draws a person and its bubble");
A.drawGardener(ctx, { pose: "read", group: "steward", hat: 0 }, 32, 48, 1, false, sp);   // a steward has no doing poses: it stands
["do-read", "do-test", "do-build", "do-git", "do-beads", "do-deploy", "do-web", "do-delegate", "do-think", "do-waiting"].forEach(function (k) { var f0 = fills; A.drawLegend(ctx, k, false); assert.ok(fills - f0 > 100, "legend " + k + " draws a person"); });
["working", "idle", "crashed", "ask", "steward"].forEach(function (k) { var f0 = fills; A.drawLegend(ctx, k, false); assert.ok(fills - f0 > 100, "legend " + k + " draws a person"); });
// every map has its rim: each painted pixel on the silhouette's edge is the rim colour, so it clears 3:1 on any ground
[A.GD_MAPS, A.GDS_MAPS].forEach(function (maps) { Object.keys(maps).forEach(function (k) {
  var m = maps[k];
  for (var y = 0; y < m.length; y++) for (var x = 0; x < m[y].length; x++) {
    var c = m[y][x]; if (c === ".") continue;
    var edge = y === 0 || x === 0 || y === m.length - 1 || x === m[y].length - 1 || m[y - 1][x] === "." || m[y + 1][x] === "." || m[y][x - 1] === "." || m[y][x + 1] === ".";
    if (edge) assert.strictEqual(c, "@", k + " at " + x + "," + y + " is " + c);
  }
}); });
`
	var args []string
	for _, f := range []string{"world.js", "plans.js", "art.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node gardeners test failed: %v\n%s", err, out)
	}
}

// A live gardener with no plot whose task (else tends) is drawn as a Meadow wildflower, Nursery pot, Orchard tree, headstone or
// pen goat stands beside it, not at the barn (gd-6FCoWelnx2; homestead-agents-as-people.md 4.1): the placement, caps, counts,
// outline, card, ghost and tip words. testdata/stand_test.js holds the assertions.
func TestStandBesideUnderNode(t *testing.T) {
	if r := nodeScript(t, "stand_test.js"); r["ok"] != true {
		t.Fatalf("stand report: %v", r)
	}
}

// A field stand in words (28.12, gd-xx7tjsBkXO): a project with one working gardener out on a plot has the stand in its
// outline under its Growing field ("<agent> — working: <task>"), nothing under its Barn, and the '/' jump board finds the
// agent by name under its project. Skipped when node is not installed.
func TestStandWordsUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), C = require(process.argv[2]), N = require(process.argv[3]), assert = require("assert");
var tasks = [0, 1, 2].map(function (i) { return { id: "pa-t" + i, title: "Dig bed " + i, type: "task", state: "in_progress", updated: 1 }; });
var doc = { plans: [{ id: "pa", title: "Plan pa", project: "gardenio", created: 1, activity: 1, counts: { in_progress: 3 },
    phases: [{ id: "pa-p1", title: "Phase 1", counts: { in_progress: 3 }, tasks: tasks }], unphased: { id: "", counts: {}, tasks: [] } }],
  seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] },
  gardeners: [{ id: "gd-rae", agent: "rae", role: "crew", project: "gardenio", state: "working", bucket: "alive", task: "pa-t1", task_title: "Dig bed 1", plot: { plan: "pa", phase: "" }, since: 1, href: "/garden?bead=gd-rae" }] };
var w = W.layout(doc, null, { now: 1, readonly: true }), st = W.gardenerState(w, doc.gardeners), g = doc.gardeners[0];
var fill = N.homeSpec(w, { agent: W.gardenerField(st, "", "", 0) }, {}, { "project:gardenio": { n: 1, home: 0, land: 0 } });
var t = C.tree(w, fill, { now: 1, gardeners: st }), rows = N.outline(t, W.gardenerOutline(w, doc.gardeners, st[2], st[3]));
var find = function (list, id) { return list.filter(function (r) { return r.id === id; })[0]; };
var home = find(find(rows, "project:gardenio").children, "home:gardenio");
var field = find(home.children, "home:gardenio/growing"), barn = find(home.children, "home:gardenio/barn");
assert.deepStrictEqual(field.children.map(function (r) { return [r.id, r.title]; }), [[W.FIELD + "gd-rae", "rae — working: Dig bed 1"]], "the stand under its Growing field");
assert.ok(barn && barn.children.length === 0, "the Barn is still a stop for its account, and holds nothing: " + JSON.stringify(barn));
assert.strictEqual(field.sub, "1 working", "the field counts its stand");
var gr = find(home.children, "gardeners:gardenio").children[0];
assert.deepStrictEqual([gr.id, gr.plot], ["gd-rae", "Plan pa"], "the Gardeners group still lists it, by its plot");
assert.strictEqual(W.fieldWhere(st, g), "In the field of gardenio and on Plan pa", "the Porch's place words");
// the jump board: by agent name and by bead id, under its project, saying what it is doing; the home row counts it working
var idx = W.searchIndex(w, st), hit = W.search(idx, "rae")[0];
assert.deepStrictEqual([hit.kind, hit.id, hit.bead, hit.project, hit.note], ["stand", W.FIELD + "gd-rae", "gd-rae", "gardenio", "working: Dig bed 1"]);
assert.strictEqual(W.search(idx, "gd-rae")[0].id, W.FIELD + "gd-rae", "its bead id finds it too");
assert.ok(!W.searchIndex(w).some(function (e) { return e.kind === "stand"; }), "no gardener state: no stands");
assert.strictEqual(idx.filter(function (e) { return e.id === "home:gardenio"; })[0].note, "1 working", "the home row: N working, never at the barn");
assert.strictEqual(W.homeWords({ letters: 2, working: 3, field: 4, barn: 5, wishes: 1 }), "2 letters · 3 working · 4 growing · 1 open wish", "homeWords has no barn count");
// one at home with no task rests in its field
var idle = { id: "gd-ida", agent: "ida", role: "crew", project: "gardenio", state: "idle", bucket: "idle", plot: null, since: 1, href: "/garden?bead=gd-ida" };
var st2 = W.gardenerState(w, [idle]), it = W.gardenerField(st2, "", "", 0)[0];
assert.deepStrictEqual([it.line, W.fieldWhere(st2, idle), W.search(W.searchIndex(w, st2), "ida")[0].note], ["ida — resting", "In the field of gardenio", "resting"]);
`
	var args []string
	for _, f := range []string{"world.js", "cursor.js", "curnav.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	if out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("node stand words test failed: %v\n%s", err, out)
	}
}

// A gardener's silhouette must reach 3:1 (WCAG 1.4.11, non-text) against every ground it can stand on, in both schemes
// (homestead-agents-as-people.md section 5.3): the eight biome grounds and the wild grass (g, h, d), the fence (c, b, B)
// and the soil (s, t, u). The silhouette is the better of the k outline and the 1 px rim outside it (cream by day, the
// emissive gold at night). The hat and shirt colours alone do not reach it, which is why the rim exists. Skipped when node
// is not installed (the palettes live in art.js).
func TestGardenerSpriteContrast(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	art, err := filepath.Abs(filepath.Join("web", "art.js"))
	if err != nil {
		t.Fatal(err)
	}
	script := `
var A = require(process.argv[1]), out = {};
[false, true].forEach(function (n) {
  var base = A.palette(n), gp = A.gardenerPal(n, "crew", 0), grounds = {};
  ["g", "h", "d", "c", "b", "B", "s", "t", "u"].forEach(function (k) { grounds["wild " + k] = base[k]; });
  for (var i = 0; i < 8; i++) { var bp = A.biomePalette(n, i); ["g", "h", "d"].forEach(function (k) { grounds[A.BIOME_PALS[i].name + " " + k] = bp[k]; }); }
  out[n ? "dark" : "light"] = { outline: gp.k, rim: gp["@"], grounds: grounds };
});
process.stdout.write(JSON.stringify(out));
`
	raw, err := exec.Command(node, "-e", script, art).CombinedOutput()
	if err != nil {
		t.Fatalf("node palette dump failed: %v\n%s", err, raw)
	}
	var schemes map[string]struct {
		Outline, Rim string
		Grounds      map[string]string
	}
	if err := json.Unmarshal(raw, &schemes); err != nil {
		t.Fatalf("palette dump: %v\n%s", err, raw)
	}
	if len(schemes) != 2 {
		t.Fatalf("want light and dark, got %v", schemes)
	}
	for name, s := range schemes {
		if len(s.Grounds) != 9+8*3 {
			t.Fatalf("%s: %d grounds", name, len(s.Grounds))
		}
		worst, at := 99.0, ""
		for g, c := range s.Grounds {
			r := max(contrast(s.Outline, c), contrast(s.Rim, c))
			if r < worst {
				worst, at = r, g
			}
			if r < 3 {
				t.Errorf("%s: a gardener on %s %s reads at %.2f:1 (outline %s, rim %s), want >= 3", name, g, c, r, s.Outline, s.Rim)
			}
		}
		t.Logf("%s: worst silhouette contrast %.2f:1 on %s", name, worst, at)
	}
}
