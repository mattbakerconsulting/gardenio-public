package gardenhub

import (
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Wishes as goats (gd-ODxztYntyd, step 2.1 of plan gd-EGpbDpqsec). web/goats.js is pure under node: a 16 x 12 sprite of five
// colours (gd-KMCkhEZPfZ: coat, shade, horns and hooves, eye, beard) and four frames (grazing head down, walking, and gd-BuWCnuLYQv's
// chewing, the grazing frame with its jaw a px lower, and the walk's second step, legs gathered) in three coats,
// drawn in device px at a whole number of them per sprite px, 1 CSS px at Plan and the zoom but never under 2 at Bead, ringed
// so it reads on every ground by day and at night (the night rim);
// a goat stands in the path under the plot of its wish's newest kin on the map, the goats of one path each
// with an even stretch of it, and wanders only inside its stretch, in whole steps, facing the way it walks, never off the
// path's line and never as tall as the path at the Plan level, chewing every other step while it grazes and walking a two-step; at most 12 drawn, the newest, the rest counted by plot. On the
// page's side, fed a farm document and drawn: nothing below the Plan level; drawn in device px with its rim under it and the
// world transform put back; reduced motion grazes still at home; the page is woken for the next step, not every frame; a plot's
// sign says "+N wishes" for its goats past the cap. Ghost or solid (gd-2cPnm71cxT, step 2.3): an untended wish's goat is drawn
// whole at GHOST alpha from a sprite of its own (one per look, kept), a tended one solid; at() finds the goat under a board
// point, and card() names the wish, who asked and whether it is tended, with its thread in the Commons. The pen (gd-kVKuWqIK1v,
// step 3.1; homestead-world-design.md 28.2): WORLD.layout fills each project's Pen cell of its home strip (byte for byte the
// same world for the same document), a cell per open wish oldest first, each once in world.index as wish:<id>, out when a plot
// of its kin is on the map; no wish moves a region; at= and resolve read <project>/pen and <project>/wish:<id>. A kinless goat grazes in its cell, looking up and turning, every one drawn (past MAX); an out goat's
// cell shows its open gate, which at() finds and card() says is out; the fence is drawn from the Project level in; the pen's
// sign counts the wishes past its cap. Skipped when node is not installed.
func TestGoatsUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var on = {}, timers = []; global.document = { addEventListener: function (k, f) { (on[k] = on[k] || []).push(f); } };
global.setTimeout = function (f, ms) { timers.push({ f: f, ms: ms }); return timers.length; }; global.clearTimeout = function () {};
function fire(k, d) { (on[k] || []).forEach(function (f) { f({ detail: d }); }); }
var W = require(process.argv[1]), G = require(process.argv[2]), A = require(process.argv[3]), BI = require(process.argv[4]), C = require(process.argv[5]), assert = require("assert"), T = W.TILE;
// the sprite: 16 x 12, four frames, rows of the letters a to f; three coats, each a colour per letter by day and by night, each
// run painted the colour at its letter's place
assert.deepStrictEqual([G.SP.w, G.SP.h, G.SP.f.length], [16, 12, 4]);
G.SP.f.forEach(function (fr) { assert.strictEqual(fr.length, 12); fr.forEach(function (r) { assert.ok(r.length === 16 && /^[a-f.]+$/.test(r), r); }); });
// every frame has its coat, shade, horns, an eye and a beard, and four hooves on the bottom row; grazing and chewing the eye is
// in the bottom half (the head down by the hooves), walking in the top (up)
function eyeRow(fr) { for (var y = 0; y < fr.length; y++) if (fr[y].indexOf("d") >= 0) return y; return -1; }
G.SP.f.forEach(function (fr, f) { var all = fr.join(""); "abcde".split("").forEach(function (l) { assert.ok(all.indexOf(l) >= 0, "frame " + f + " has " + l); }); assert.strictEqual(fr[fr.length - 1].replace(/[^c]/g, "").length, 4, "frame " + f + ": four hooves"); });
assert.ok(eyeRow(G.SP.f[0]) >= 6 && eyeRow(G.SP.f[1]) >= 0 && eyeRow(G.SP.f[1]) < 6, "grazing: the head down by the hooves; walking: up");
// chewing is grazing with the jaw a px lower: the same goat down to the muzzle's row, the beard a row further down; the walk's
// second step is the first's head and body on other legs: the same rows above the knees, the hooves elsewhere
function diff(a, b) { var out = []; a.forEach(function (r, y) { for (var x = 0; x < r.length; x++) if (r[x] !== b[y][x]) out.push([x, y]); }); return out; }
assert.ok(diff(G.SP.f[0], G.SP.f[2]).length > 0 && diff(G.SP.f[0], G.SP.f[2]).every(function (q) { return q[0] >= 12 && q[1] >= 10; }), "chewing: only the jaw moves");
assert.strictEqual(G.SP.f[2][11].indexOf("e"), G.SP.f[0][10].indexOf("e"), "the beard a row lower");
assert.ok(diff(G.SP.f[1], G.SP.f[3]).length > 0 && diff(G.SP.f[1], G.SP.f[3]).every(function (q) { return q[1] >= 10; }), "the second step: only the legs move");
assert.deepStrictEqual(Object.keys(G.COATS), ["white", "brown", "black"]);
Object.keys(G.COATS).forEach(function (k) { var c = G.COATS[k]; assert.ok(c.day.length === c.night.length, k); G.SP.runs.forEach(function (rs) { rs.forEach(function (r) { assert.ok(r.i === G.INKS.indexOf(r.c) && r.i < c.day.length, k + ": " + r.c); }); }); });
// mirrored, a goat walking left is the same goat
G.SP.runs.forEach(function (rs, f) { assert.deepStrictEqual(G.SP.left[f].map(function (r) { return [G.SP.w - r.x - r.w, r.y, r.w, r.c, r.i]; }), rs.map(function (r) { return [r.x, r.y, r.w, r.c, r.i]; })); });
// ringed so it reads: the coat 3:1 off its ring, and the coat or its ring 3:1 off every grass, soil and sand tone, by day and at
// night; the white goat in the dark ring, the dark goats in the pale rims (the night rim at night)
function lum(h) { var c = [1, 3, 5].map(function (i) { var v = parseInt(h.substr(i, 2), 16) / 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }); return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]; }
function ratio(x, y) { var a = lum(x), b = lum(y); return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05); }
[false, true].forEach(function (night) {
  var pal = A.palette(night);
  Object.keys(G.COATS).forEach(function (k) {
    var c = G.COATS[k], cs = night ? c.night : c.day, rim = G.rimOf(c, night);
    assert.strictEqual(rim, k === "white" ? G.INK : night ? G.RIM : G.DAY_RIM, k + " night " + night);
    assert.ok(ratio(cs[0], rim) >= 3, k + " off its ring, night " + night + ": " + ratio(cs[0], rim).toFixed(2) + ":1");
    assert.ok(ratio(cs[2], cs[0]) >= 3, k + "'s horns off its coat, night " + night);
    "ghdstuPN".split("").forEach(function (g) { var r = Math.max(ratio(cs[0], pal[g]), ratio(rim, pal[g])); assert.ok(r >= 3, k + " on " + g + ", night " + night + ": " + r.toFixed(2) + ":1"); });
  });
});
// device px per sprite px, whole, never under 1 CSS px: 1 at the Plan level (a goat 16 x 12); at the Bead level the zoom,
// never under 2, so a Bead goat is bigger than a Plan one and grows as the camera comes in
assert.deepStrictEqual([G.pxOf(0.49, 1, 2), G.pxOf(0.49, 2, 2), G.pxOf(0.49, 3, 2), G.pxOf(0.91, 1, 2)], [1, 2, 3, 1], "Plan");
assert.deepStrictEqual([1, 1.5, 2, 3, 4].map(function (z) { return G.pxOf(z, 1, 3); }), [2, 2, 2, 3, 4], "Bead at DPR 1");
[1, 2, 3].forEach(function (d) {
  [0.45, 0.6, 0.99].forEach(function (z) { assert.ok(G.pxOf(z, d, 2) >= G.PX * d, "never under PX at Plan"); });
  [1, 1.5, 2, 3, 4].forEach(function (z) { assert.ok(G.pxOf(z, d, 3) > G.pxOf(0.91, d, 2), "Bead " + z + "x over Plan at DPR " + d); });
});
// a coat by id, the same every time, and all three in a herd
assert.strictEqual(G.coatOf("gd-wish1"), G.coatOf("gd-wish1"));
var herd = {}; for (var i = 0; i < 30; i++) herd[G.coatOf("gd-w" + i)] = 1;
assert.deepStrictEqual(Object.keys(herd).sort(), ["black", "brown", "white"]);

// the ring: the middle of the 2-tile path round a box, one tile out, from its bottom-left corner round
assert.deepStrictEqual(G.ring({ x: 0, y: 0, w: 32, h: 32 }).map(function (p) { return [p.x / T, p.y / T]; }), [[-1, 33], [33, 33], [33, -1], [-1, -1], [-1, 33]]);
var pA = { id: "gd-planA", kind: "plot", project: "gardenio", x: 100, y: 40, w: 32, h: 32 }, pB = { id: "gd-planB", kind: "plot", project: "gardenio", x: 134, y: 40, w: 32, h: 32 };
var pC = { id: "gd-planC", kind: "plot", project: "acme", x: 400, y: 40, w: 32, h: 32 }, pD = { id: "gd-planD", kind: "plot", project: "gardenio", x: 168, y: 74, w: 32, h: 32 };
var world = { index: { "gd-planA": pA, "gd-planB": pB, "gd-planC": pC, "gd-planD": pD, "gd-A1": { id: "gd-A1", kind: "bed" }, "wish:gd-w2": { id: "wish:gd-w2", kind: "goat", x: -11, y: 10, w: 3, h: 2 } } };
var tA = G.tourOf([pA]);
assert.strictEqual(tA.len, 4 * 34 * T);
// a plot's gutters, each with a mouth on the path at either end, the way in pointing into the plot
assert.deepStrictEqual(tA.mouths.map(function (m) { return m.axis; }).sort().join(""), "hhhhvvvv");
tA.mouths.forEach(function (m) { var x = m.x + m.dx * 3 * T, y = m.y + m.dy * 3 * T; assert.ok(x > pA.x * T && x < (pA.x + 32) * T && y > pA.y * T && y < (pA.y + 32) * T, "in from " + JSON.stringify(m)); });
// two plots side by side: one ring round both, and only the mouths on it (not the two on the path between them)
var tAB = G.tourOf([pA, pB]);
assert.deepStrictEqual(tAB.box, { x: (pA.x - 1) * T, y: (pA.y - 1) * T, w: 68 * T, h: 34 * T });
assert.strictEqual(tAB.mouths.length, 12, "4 across the top, 4 along the bottom, 2 at each end");

// place: the goat of a wish laps every kin plot on the map in its newest kin's project; one with none stands in its pen cell
var wishes = [
  { id: "gd-w1", title: "a pen for the goats", asker: "oidc:matt@example.com", href: "/garden/c/bead/gd-w1", tending: true, arms: [{ canonical: "🧭🔥📦", n: 3, name: "arms: compass, fire, package" }, { canonical: "📦🔥🧭", n: 3, name: "arms: package, fire, compass" }], plots: [{ plan: "gd-gone", phase: "" }, { plan: "gd-A1", phase: "" }, { plan: "gd-planB", phase: "gd-B1" }, { plan: "gd-planC" }, { plan: "gd-planA", phase: "" }, { plan: "gd-planB" }] },
  { id: "gd-w2", title: "no kin yet" },
  { id: "gd-w3", title: "on A", plots: [{ plan: "gd-planA", phase: "gd-A1" }] },
  { id: "gd-w4", title: "on A too", plots: [{ plan: "gd-planA", phase: "" }] },
  { id: "" }
];
var gs = G.place(wishes, world);
assert.deepStrictEqual(gs.map(function (g) { return g.id + "@" + g.plot + ":" + g.plots.join("+"); }), ["gd-w1@gd-planB:gd-planB+gd-planA", "gd-w2@pen:", "gd-w3@gd-planA:gd-planA", "gd-w4@gd-planA:gd-planA"], "in order; acme's plan C is another project's");
assert.deepStrictEqual([gs[0].title, gs[0].asker, gs[0].href, gs[0].tending, gs[1].tending], ["a pen for the goats", "oidc:matt@example.com", "/garden/c/bead/gd-w1", true, false]);
assert.deepStrictEqual([gs[0].tour.box, gs[1].tour, gs[1].pen, gs[1].cell, gs[2].tour.len], [tAB.box, undefined, true, { x: -9.5 * T, y: 11 * T, w: 3 * T, h: 2 * T }, tA.len]);
assert.deepStrictEqual(G.place([{ id: "gd-w9", title: "past the pen's cap" }], world), [], "a wish in no pen cell and on no plot is not drawn (the pen's sign counts it)");
assert.ok(gs[2].tour === gs[3].tour && Math.abs(Math.abs(gs[3].off - gs[2].off) - tA.len / 2) <= 1, "two goats of A half the ring apart");
assert.deepStrictEqual([gs[2].adv, gs[2].jit], [G.ADV, G.JIT]);
assert.deepStrictEqual(G.place(null, world), []);

// the ground: the ring, or a gutter of a kin plot; never a bed
function beds(p) { var o = []; W.BED.off.forEach(function (x) { W.BED.off.forEach(function (y) { o.push({ x: (p.x + x) * T, y: (p.y + y) * T, w: W.BED.size * T, h: W.BED.size * T }); }); }); return o; }
var allBeds = [pA, pB, pC, pD].reduce(function (a, p) { return a.concat(beds(p)); }, []);
function hit(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }
function box(p, sz) { return { x: p.x - sz.w / 2, y: p.y - sz.h / 2, w: sz.w, h: sz.h }; }
function onRing(t, p) { for (var i = 1; i < t.pts.length; i++) { var a = t.pts[i - 1], b = t.pts[i]; if (p.x >= Math.min(a.x, b.x) && p.x <= Math.max(a.x, b.x) && p.y >= Math.min(a.y, b.y) && p.y <= Math.max(a.y, b.y)) return true; } return false; }
function inGutter(g, p) {
  return g.plots.some(function (id) {
    var q = world.index[id], lines = G.freeLines(32).slice(1, -1);
    return lines.some(function (l) { return (p.x === (q.x + l) * T && p.y > (q.y - 1) * T && p.y < (q.y + 33) * T) || (p.y === (q.y + l) * T && p.x > (q.x - 1) * T && p.x < (q.x + 33) * T); });
  });
}
// the goat's size at the bottom of the Plan level (0.49x at DPR 1, its outline in), and its body at the Bead level from 2x
var planSz = { w: (16 * 1 + 2) / 0.49, h: (12 * 1 + 2) / 0.49 }, beadBody = { w: 16, h: 12 };
function walk(g, ms, every, o, each) {
  var seen = { 0: 0, 1: 0, 2: 0, 3: 0 }, walks = 0, flips = {}, last = null, gut = 0;
  for (var tt = 0; tt < ms; tt += every) {
    var p = G.wander(g, tt, o);
    assert.strictEqual(p.x, Math.round(p.x), "whole world px"); assert.strictEqual(p.y, Math.round(p.y));
    var ring = onRing(g.tour, p), gt = inGutter(g, p);
    assert.ok(ring || gt, g.id + " on the ring or in a gutter at " + tt + ": " + p.x + "," + p.y);
    if (gt && !ring) gut++;
    assert.ok(p.next > 0 && p.next <= 2 * G.PER, "next " + p.next);
    if (p.walking) { walks++; assert.ok(p.f === 1 || p.f === 3, "walking, head up, at " + tt); if (last && p.x !== last.x) assert.strictEqual(p.dir, p.x > last.x ? 1 : -1, "facing the way it walks at " + tt); }
    if (last) assert.ok(Math.abs(p.x - last.x) + Math.abs(p.y - last.y) <= 32, "a step at a time at " + tt + ": " + JSON.stringify([last, p]));
    var q = G.wander(g, tt + p.next - 1, o);
    if (p.next > 1) assert.deepStrictEqual([q.x, q.y, q.f, q.dir], [p.x, p.y, p.f, p.dir], "unchanged until next, at " + tt);
    if (!p.walking && p.f !== 1) assert.strictEqual(p.f, Math.floor(((tt + g.phase) % G.PER) / G.STEP) % 2 ? 2 : 0, "grazing, chewing every other step, at " + tt);
    if (each) each(p, tt);
    seen[p.f]++; flips[p.dir] = 1; last = p;
  }
  return { seen: seen, walks: walks, flips: flips, gut: gut };
}
// at the Plan level (no gutter fits): round the ring, never on a bed, mostly grazing, now and then walking or looking up, turning
var g = gs[2], plan = { fit: null, size: planSz };
var w1 = walk(g, 600000, 100, plan, function (p) { allBeds.forEach(function (b) { assert.ok(!hit(box(p, planSz), b), "on a bed: " + JSON.stringify([p, b])); }); });
assert.ok(w1.seen[0] + w1.seen[2] > w1.seen[1] + w1.seen[3] && w1.seen[1] > 0 && w1.walks > 0, "mostly grazing, sometimes walking or looking up");
assert.ok(w1.seen[2] > 0 && Math.abs(w1.seen[0] - w1.seen[2]) < w1.seen[0] / 4 && w1.seen[3] > 0, "it chews and walks a two-step");
// the walk is a two-step: one step in stride, the next gathered, by turns
var steps = [];
for (tt = 0; tt < 120000 && steps.length < 8; tt += 20) { var sp = G.wander(g, tt, plan); if (sp.walking && (!steps.length || steps[steps.length - 1].t + G.STEP <= tt)) steps.push({ t: tt, f: sp.f }); else if (!sp.walking) steps = []; }
assert.strictEqual(steps.length, 8, "a walk of eight steps");
steps.forEach(function (st, i) { if (i) assert.notStrictEqual(st.f, steps[i - 1].f, "by turns: " + JSON.stringify(steps)); });
assert.deepStrictEqual(Object.keys(w1.flips).sort(), ["-1", "1"], "it turns");
assert.strictEqual(w1.gut, 0, "no gutter at the Plan level");
// at the Bead level from 1.5x its body fits a gutter: now and then it turns up one, its body never on a bed
var w2 = walk(g, 600000, 100, { fit: { v: true, h: true }, size: beadBody }, function (p) { allBeds.forEach(function (b) { assert.ok(!hit(box(p, beadBody), b), "on a bed: " + JSON.stringify([p, b])); }); });
assert.ok(w2.gut > 0, "up a gutter");
// only the gutters it fits: across y but not across x, it never stands in one across x
walk(g, 300000, 100, { fit: { v: false, h: true }, size: beadBody }, function (p) { if (!onRing(g.tour, p)) assert.ok(G.freeLines(32).some(function (l) { return p.y === (pA.y + l) * T; }), "in a gutter across x: " + JSON.stringify(p)); });
// at the Plan level a goat in its ring is no taller than the 2-tile path it walks, at every Plan zoom and DPR
[1, 2, 3].forEach(function (d) {
  [0.45, 0.6, 0.99].forEach(function (z) { var n = G.pxOf(z, d, 2), r = Math.max(1, Math.round(d), Math.round(n / 4)), h = (G.SP.h * n + 2 * r) / (z * d); assert.ok(h <= 2 * T, "z " + z + " DPR " + d + ": " + h.toFixed(1) + " world px"); });
});
// it laps: a goat round A and B passes under both
var under = {};
walk(gs[0], 1200000, 1000, plan, function (p) { if (p.y === (pA.y + 33) * T) under[p.x < pB.x * T ? "A" : "B"] = 1; });
assert.deepStrictEqual(Object.keys(under).sort(), ["A", "B"], "under A and under B in one lap");
// a goat with no kin grazes in its pen cell: never walking, its middle at the cell's, looking up now and then and turning
var pv = { f: {}, dir: {} };
for (tt = 0; tt < 600000; tt += 500) { var pp = G.wander(gs[1], tt, plan); assert.deepStrictEqual([pp.x, pp.y, pp.walking], [gs[1].cell.x, gs[1].cell.y, false]); assert.ok(pp.next > 0 && pp.next <= G.PER); pv.f[pp.f] = 1; pv.dir[pp.dir] = 1; }
assert.deepStrictEqual([Object.keys(pv.f).sort(), Object.keys(pv.dir).sort()], [["0", "1", "2"], ["-1", "1"]], "it chews, looks up and turns");
assert.deepStrictEqual([G.wander(gs[1], 1234, true).f, G.wander(gs[1], 1234, true).next, G.wander(gs[1], 1234, true).x], [0, Infinity, gs[1].cell.x], "reduced motion: still, head down");
// two goats of one tour do not walk in step
var ph = 0; for (tt = 0; tt < 120000; tt += 250) if (G.wander(gs[2], tt, plan).walking !== G.wander(gs[3], tt, plan).walking) ph++;
assert.ok(ph > 0, "out of step");
// a crowd round one ring keeps its distance: each goat 2 KEEP from the next, whatever it does, so two Plan goats never
// overlap, even round a corner
var crowd = []; for (i = 0; i < 25; i++) crowd.push({ id: "gd-c" + i, plots: [{ plan: "gd-planA" }] });
var cg = G.place(crowd, world);
assert.ok(cg[0].adv < G.ADV && cg[0].adv > 0, "a crowd moves on less: " + cg[0].adv);
for (tt = 0; tt < 240000; tt += 700) {
  var ps = cg.map(function (x) { return G.wander(x, tt, plan); });
  ps.forEach(function (p, j) { var q = ps[(j + 1) % ps.length]; assert.ok(Math.abs(p.x - q.x) + Math.abs(p.y - q.y) >= 2 * G.KEEP - 2 && !hit(box(p, planSz), box(q, planSz)), "too close at " + tt + ": " + JSON.stringify([p, q])); });
}
var packed = []; for (i = 0; i < 80; i++) packed.push({ id: "gd-p" + i, plots: [{ plan: "gd-planA" }] });
assert.deepStrictEqual([G.place(packed, world)[0].adv, G.wander(G.place(packed, world)[0], 5000, plan).next], [0, Infinity], "too packed to move: still");
// two spells on one spot (a spot moved on to the same clear point): no way to walk, nothing breaks
var at5 = G.pointAt(tA, 5), sp5 = { s: 5, x: at5.x, y: at5.y }, r0 = G.routeOf(tA, sp5, sp5);
assert.deepStrictEqual([r0.len, r0.dur, G.along(r0.pts, 0)], [0, 0, { x: at5.x, y: at5.y, dir: 0 }]);
// reduced motion: grazing still at its place round the ring, beside the plot
var st = G.wander(g, 1234, true);
assert.deepStrictEqual([st.x, st.y, st.f, st.next], [G.pointAt(g.tour, g.off).x, G.pointAt(g.tour, g.off).y, 0, Infinity]);
assert.deepStrictEqual(G.wander(g, 99999, { still: true }), G.wander(g, 1, { still: true }));
// never under a gardener or its birds: keepOuts holds each standing figure and the ground its birds fly out over
var lv = { spots: {
  "gd-fence": { plot: "gd-planA", col: pA.x + 3, row: pA.y + 31, x: (pA.x + 3) * T, y: (pA.y + 31) * T - 8, shown: true, cell: "" },
  "gd-cell": { plot: "gd-planA", col: pA.x + 14, row: pA.y + 12, x: (pA.x + 14) * T, y: (pA.y + 12) * T - 8, shown: true, cell: "gd-A1t" },
  "gd-hidden": { plot: "gd-planA", shown: false }, "gd-home": { home: true } } };
var ko = G.keepOuts(lv, world);
assert.strictEqual(ko.length, 4, "two figures, two birds' ways");
function covers(b, p) { return p.x >= b.x && p.x <= b.x + b.w && p.y >= b.y && p.y <= b.y + b.h; }
[-1, 1].forEach(function (s) { assert.ok(covers(ko[3], { x: (pA.x + 3) * T + 8 + s * BI.RUN / 2, y: (pA.y + 31) * T - 4 - 1.5 * BI.RUN }), "the fence gardener's birds' way up off its head, left and right"); });
assert.deepStrictEqual(ko[2], { x: (pA.x + 3) * T, y: (pA.y + 31) * T - 8, w: 16, h: 24 }, "its figure");
assert.ok(covers(ko[1], { x: (pA.x + 14) * T + 8, y: (pA.y + 12) * T - 4 - BI.RUN }), "the cell gardener's birds' way up");
assert.deepStrictEqual(G.keepOuts(null, world), []);
var kept = { fit: { v: true, h: true }, size: planSz, keep: ko }, n0 = 0;
walk(g, 600000, 100, kept, function (p) { if (!p.walking) ko.forEach(function (b) { assert.ok(!hit(box(p, planSz), b), "grazing under a gardener or its birds: " + JSON.stringify([p, b])); }); else n0++; });
assert.ok(n0 > 0);

// admit: the tours in view, the first 12 in order, the rest counted by plot
var many = []; for (i = 0; i < 16; i++) many.push({ id: "gd-m" + i, plots: [{ plan: i < 10 ? "gd-planA" : "gd-planB" }] });
var mg = G.place(many, world), all = G.admit(mg, null);
assert.strictEqual(G.MAX, 12);
assert.deepStrictEqual([all.shown.length, all.more], [12, { "gd-planB": 4 }], "12 drawn, the newest; B's other four counted");
assert.deepStrictEqual(all.shown.map(function (x) { return x.id; }), mg.slice(0, 12).map(function (x) { return x.id; }));
var vA = { x: pA.x * T, y: pA.y * T, w: 20 * T, h: 40 * T };
assert.deepStrictEqual([G.admit(mg, vA).shown.length, G.admit(mg, vA).more], [10, {}], "only A's goats in A's view");
assert.deepStrictEqual(G.admit(mg, { x: 0, y: 0, w: 10, h: 10 }).shown, [], "none off the view");
assert.strictEqual(G.admit(mg, null, 3).shown.length, 3);
// a project's open wishes, the ones the farm's cap left out too
assert.deepStrictEqual(G.countOf({ open: [{ id: "a", project: "Gardenio" }, { id: "b", project: "gardenio" }, { id: "c" }, { id: "d", project: "acme" }], open_more: { gardenio: 3 } }), { gardenio: 5, acme: 1 });
assert.deepStrictEqual(G.countOf(null), {});

/* ---------- the page's side, fed a farm document and drawn ---------- */
var rects = [], sets = [], m = { a: 0.98, b: 0, c: 0, d: 0.98, e: 40, f: 20 };
var ctx = { fillStyle: "", globalAlpha: 1, getTransform: function () { return m; }, setTransform: function () { sets.push([].slice.call(arguments)); }, fillRect: function (x, y, w, h) { rects.push({ x: x, y: y, w: w, h: h, c: this.fillStyle, a: this.globalAlpha }); } };
function o(x) { var r = { level: 2, z: 0.49, night: false, view: null, world: world, gs: null, reduce: false }; for (var k in x) r[k] = x[k]; return r; }
var woke = 0; G.onWake(function () { woke++; });
G.pin(1792000000000);
G.draw(ctx, o());
assert.deepStrictEqual([G.list().length, rects.length], [0, 0], "no farm document: no goats");
fire("farmdoc", { wishes: { open: wishes.slice(0, 4) } });
assert.strictEqual(G.draw(ctx, o({ level: 1 })), false);
assert.deepStrictEqual([G.list().length, rects.length], [0, 0], "World and Project: no goats");
assert.strictEqual(G.draw(ctx, o()), false, "a goat never asks for every frame");
assert.deepStrictEqual(G.list().map(function (x) { return x.id; }), ["gd-w1", "gd-w2", "gd-w3", "gd-w4"]);
assert.deepStrictEqual(sets, [[1, 0, 0, 1, 0, 0], [0.98, 0, 0, 0.98, 40, 20]], "drawn in device px, the world transform put back");
// two device px per sprite px at 0.49x and DPR 2, each goat over its ring two device px round, in whole sprite px
function span(rs, k, d) { return Math.max.apply(null, rs.map(function (r) { return r[k] + r[d]; })) - Math.min.apply(null, rs.map(function (r) { return r[k]; })); }
var one = rects.slice(0, G.SP.runs[G.list()[0].f].length * 2), ring = one.slice(0, one.length / 2), body = one.slice(one.length / 2);
var c0 = G.COATS[G.coatOf("gd-w1")];
assert.ok(ring.every(function (r) { return r.c === G.rimOf(c0, false); }) && body.every(function (r) { return c0.day.indexOf(r.c) >= 0; }), "the ring under the goat");
assert.ok(body.every(function (r) { return r.w % 2 === 0 && r.h === 2; }), "whole sprite px");
assert.deepStrictEqual([span(body, "x", "w") <= 32, span(ring, "y", "h") - span(body, "y", "h"), span(ring, "x", "w") - span(body, "x", "w")], [true, 4, 4], "ringed two device px round");
var l0 = G.list()[0];
// ghost or solid: gd-w1 is tended, the rest are not; with no canvas to draw a sprite on (node), a ghost is its fills at GHOST
// alpha, and the alpha is put back
assert.deepStrictEqual(G.list().map(function (x) { return x.ghost; }), [false, true, true, true]);
var per = G.list().map(function (x) { return G.SP.runs[x.f].length * 2; }), alphas = [], at0 = 0;
per.forEach(function (n) { alphas.push(rects.slice(at0, at0 + n).map(function (r) { return r.a; })); at0 += n; });
assert.deepStrictEqual(alphas.map(function (a) { return a.every(function (v) { return v === a[0]; }) ? a[0] : "mixed"; }), [1, G.GHOST, G.GHOST, G.GHOST], "a ghost at GHOST alpha, a tended goat solid");
assert.ok(G.GHOST > 0.3 && G.GHOST < 0.7, "translucent, never gone");
assert.strictEqual(ctx.globalAlpha, 1, "the alpha put back");
// at(): the goat under a board point (CSS px: device px over DPR 2), missed by up to HIT_PAD; the last drawn is on top
var b0 = { x: Math.min.apply(null, ring.map(function (r) { return r.x; })) / 2, y: Math.min.apply(null, ring.map(function (r) { return r.y; })) / 2, w: span(ring, "x", "w") / 2, h: span(ring, "y", "h") / 2 };
var hit0 = G.at(b0.x + b0.w / 2, b0.y + b0.h / 2);
assert.deepStrictEqual([hit0.id, hit0.title, hit0.asker, hit0.href, hit0.tending], ["gd-w1", "a pen for the goats", "oidc:matt@example.com", "/garden/c/bead/gd-w1", true]);
assert.ok(Math.abs(hit0.x + hit0.w / 2 - l0.x) < 1 && Math.abs(hit0.y + hit0.h / 2 - l0.y) < 1 && hit0.w > 0, "its box, world px, round its spot");
assert.strictEqual(G.at(b0.x - G.HIT_PAD + 0.5, b0.y + 1).id, "gd-w1", "missed by under HIT_PAD");
assert.strictEqual(G.at(-99999, -99999), null, "nothing there");
assert.strictEqual(G.at(b0.x - 12, b0.y + 1, 0), null); assert.strictEqual(G.at(b0.x - 12, b0.y + 1, G.TAP_PAD).id, "gd-w1", "a finger's pad");
// card(): the wish, who asked, tended or not (in words, not alpha alone), and its thread in the Commons
var ago = function () { return "3 days"; };
var c1 = G.card(G.place(wishes, world)[0], "/v", ago), c2 = G.card({ id: "gd-w5", title: "five", asker: "", href: "/garden/c/bead/gd-w5", tending: false, kin: 3, project: "gardenio", opened: 1 }, "", ago), c3 = G.card(G.place(wishes, world)[1], "", null);
assert.strictEqual(c1.title, "a pen for the goats");
assert.deepStrictEqual(c1.lines.slice(0, 2).map(function (l) { return l[1]; }), ["Asked by matt", "Tended: a gardener is at work on it"]);
assert.deepStrictEqual(c1.lines[3], ["why", [{ text: "Thread in the Commons", href: "/v/garden/c/bead/gd-w1", cls: "gchat", id: "gd-w1" }, { text: "Zoom to it on the map", href: "/v/garden?at=gardenio/gd-planB", cls: "ggoto", id: "gd-planB" }]], "the gardeners' Chat link: CHAT.open on its id; and the jump to plan B's plot, which it grazes by (gd-xlRpUpTjPZ)");
assert.deepStrictEqual([c2.lines[3][1][1], c3.lines[3][1][1]], [{ text: "Zoom to it on the map", href: "/garden?at=gardenio/wish%3Agd-w5", cls: "ggoto", id: "wish:gd-w5" }, { text: "Zoom to it on the map", href: "/garden?at=~/wish%3Agd-w2", cls: "ggoto", id: "wish:gd-w2" }], "in the pen: the jump to its cell there");
assert.deepStrictEqual([G.goTo({ id: "gd-x", grazes: "gd-planB", pkey: "" }), G.goTo({ id: "gd-x", grazes: "pen", plot: "gd-planB" })], [{ id: "gd-planB", at: "~/gd-planB" }, { id: "wish:gd-x", at: "~/wish%3Agd-x" }], "the loose beads' project is ~; a goat hit in the pen goes to its cell in its project's pen");
assert.deepStrictEqual(c2.lines.slice(0, 3).map(function (l) { return l[1]; }), ["Nobody is named as asking", "Untended: none of its 3 steps is in progress", "wish · gardenio · opened 3 days ago"]);
assert.strictEqual(c3.lines[1][1], "Untended: nothing has come of it yet");
assert.ok(/^Wish: a pen for the goats, asked by matt\. Tended/.test(c1.tip), c1.tip);
// its kin's line arms (heraldry 5.10) ride the card, drawn before the title, and the tip names them; a goat without has none
assert.deepStrictEqual([c1.arms.map(function (a) { return a.canonical; }), c2.arms, c3.arms], [["🧭🔥📦", "📦🔥🧭"], [], []]);
assert.ok(/\. Arms: compass, fire, package; arms: package, fire, compass\.$/.test(c1.tip) && !/rms/.test(c3.tip), c1.tip);
assert.strictEqual(hit0.arms.length, 2, "the hit goat carries its arms to the tip");
// the herder's verdict (gd-RkkGXAz643): a line after whether it is tended, in words, with what the herder does about it and when it
// judged it; "agent woken" and "asked" only when it did (herder_acted); none before its first pass
var herded = function (v, acted, checked) { return G.card({ id: "gd-h", title: "h", href: "/garden/c/bead/gd-h", kin: 1, herder: v, acted: acted, checked: checked }, "", ago); };
assert.deepStrictEqual([herded("fulfilled", false, 9), herded("in-progress", false, 9), herded("unfulfilled", true, 9), herded("unclear", true, 9), herded("unfulfilled", false, 0), herded("unclear", false, 9)].map(function (c) { return c.lines[2]; }),
  [["rsn", "Herder: fulfilled, closing · checked 3 days ago"], ["who", "Herder: in progress · checked 3 days ago"], ["rsn rsn-parked", "Herder: unfulfilled, agent woken · checked 3 days ago"],
   ["rsn rsn-parked", "Herder: unclear, asked · checked 3 days ago"], ["rsn rsn-parked", "Herder: unfulfilled"], ["rsn rsn-parked", "Herder: unclear · checked 3 days ago"]]);
assert.ok(/\. Herder: fulfilled, closing\.$/.test(herded("fulfilled", false, 9).tip) && !/Herder/.test(c1.tip) && c1.lines[2][0] === "who", "the tip says it too; a goat it has not judged has no line");
assert.deepStrictEqual([herded("judged", true, 9).lines[2][0], G.herderWords("judged", true), G.herderWords("in-progress", true)], ["who", "", "in progress"], "a verdict it does not know is none; nobody is woken on one in progress");
var hp = G.place([{ id: "gd-hp", title: "herded", herder_verdict: "unclear", herder_checked_at: 7, herder_acted: true }], { index: { "wish:gd-hp": { kind: "goat", x: 0, y: 0, w: 2, h: 2 } } })[0];
assert.deepStrictEqual([hp.herder, hp.checked, hp.acted], ["unclear", 7, true], "place carries the farm's herder_* to the goat");
assert.ok(Math.abs((Math.min.apply(null, ring.map(function (r) { return r.x; })) + span(ring, "x", "w") / 2) - (m.a * l0.x + m.e)) <= 2 * 2 + 2, "centred on its spot");
// with a canvas to draw on (the page), a ghost is its own sprite drawn whole at GHOST alpha, so its ring never shows through its
// coat; the sprite is kept, one per look
var made = 0, imgs = [], sctx = { fillRect: function () {} };
global.document.createElement = function (t) { made++; return { tag: t, getContext: function () { return sctx; } }; };
ctx.drawImage = function (img, x, y) { imgs.push({ img: img, x: x, y: y, a: this.globalAlpha }); };
rects = []; G.draw(ctx, o());
assert.deepStrictEqual(imgs.map(function (d) { return d.a; }), [G.GHOST, G.GHOST, G.GHOST], "the three ghosts drawn whole");
assert.ok(imgs.every(function (d) { return d.img.width === 16 * 2 + 4 && d.img.height === 12 * 2 + 4; }), "a sprite its outline's size");
assert.strictEqual(rects.length, G.SP.runs[G.list()[0].f].length * 2, "only the tended goat by fills");
var made1 = made; G.draw(ctx, o()); assert.strictEqual(made, made1, "a sprite is kept");
delete global.document.createElement; delete ctx.drawImage;
// at night the dark goats in the pale night rim
rects = []; G.draw(ctx, o({ night: true }));
var coats = G.list().map(function (x) { return G.coatOf(x.id); });
assert.strictEqual(rects.some(function (r) { return r.c === G.RIM; }), coats.some(function (c) { return c !== "white"; }), "a dark goat ringed in the night rim");
assert.strictEqual(rects.some(function (r) { return r.c === G.INK; }), coats.indexOf("white") >= 0, "the white goat in the dark one");
assert.ok(!rects.some(function (r) { return r.c === G.DAY_RIM; }), "no day rim at night");
// the page is woken for the next step: one timer, the soonest goat's change
timers = []; G.pin(1792000100000); G.draw(ctx, o());
assert.strictEqual(timers.length, 1, "one timer");
var wo2 = { fit: null, keep: [], size: { w: (16 * 2 + 4) / 0.98, h: (12 * 2 + 4) / 0.98 } };
var soonest = Math.min.apply(null, G.place(wishes.slice(0, 4), world).map(function (x) { return G.wander(x, 1792000100000, wo2).next; }));
assert.deepStrictEqual(G.list().map(function (x) { return [x.x, x.y]; }), G.place(wishes.slice(0, 4), world).map(function (x) { var p = G.wander(x, 1792000100000, wo2); return [p.x, p.y]; }), "where wander says, at the Plan level no gutter");
assert.strictEqual(timers[0].ms, Math.max(16, soonest));
timers[0].f(); assert.strictEqual(woke, 1, "the timer wakes the page");
// at the Bead level, zoomed in to 2x at DPR 2: four device px per sprite px
m = { a: 4, b: 0, c: 0, d: 4, e: 40, f: 20 }; rects = []; G.draw(ctx, o({ level: 3, z: 2 }));
var bodies = rects.filter(function (r) { return [G.RIM, G.DAY_RIM, G.INK].indexOf(r.c) < 0; });
assert.ok(bodies.length > 0 && bodies.every(function (r) { return r.h === 4 && r.w % 4 === 0; }), "a Bead goat four device px per sprite px");
m = { a: 0.98, b: 0, c: 0, d: 0.98, e: 40, f: 20 };
// reduced motion: grazing still at home, and nothing to wake for
timers = []; G.draw(ctx, o({ reduce: true }));
assert.deepStrictEqual(G.list().map(function (x) { return [x.x, x.y, x.f]; }), G.place(wishes.slice(0, 4), world).map(function (x) { var p = G.wander(x, 0, true); return [p.x, p.y, 0]; }));
assert.strictEqual(timers.length, 0, "a still goat wakes nothing");
G.motion(true); G.pin(1792000200000); G.draw(ctx, o({ reduce: true })); assert.ok(timers.length === 1, "the goldens can force it to walk"); G.motion(false);
// with the gardeners: none grazes under one (a figure standing on the ring's spot of a still goat moves it on)
var still0 = G.list()[2], gsF = { 2: { spots: { "gd-g": { plot: "gd-planA", x: still0.x - 8, y: still0.y - 12, shown: true, cell: "" } } } };
G.draw(ctx, o({ reduce: true, gs: gsF }));
assert.ok(G.list()[2].x !== still0.x || G.list()[2].y !== still0.y, "moved on from under the gardener");
G.keepOuts(gsF[2], world).forEach(function (b) { G.list().forEach(function (p) { assert.ok(!hit(box(p, { w: 36 / 0.98, h: 28 / 0.98 }), b), "under a gardener: " + p.id); }); });
// off the view: none drawn
rects = []; G.draw(ctx, o({ view: { x: -99999, y: -99999, w: 10, h: 10 } }));   // (the pen goat stands by the origin)
assert.deepStrictEqual([G.list().length, rects.length], [0, 0]);
// the cap: 16 wishes, 12 drawn; plan B's sign says "+4 wishes" at Plan and Bead, not at Project, nor on A's
var tree = { nodes: { "gd-planA": { kind: "plot", id: "gd-planA" }, "gd-planB": { kind: "plot", id: "gd-planB" }, "project:gardenio": { kind: "biome", key: "gardenio" } } };
function signed(id, lv) { return G.sign(tree.nodes[id], "phase 1 of 3", lv); }
assert.strictEqual(signed("gd-planB", 2), "phase 1 of 3", "nothing past the cap yet");
fire("farmdoc", { wishes: { open: many } });
G.draw(ctx, o());
assert.strictEqual(G.list().length, 12);
assert.strictEqual(G.stale(), true, "the signs are re-read");
assert.deepStrictEqual([signed("gd-planB", 2), signed("gd-planB", 3), signed("gd-planB", 1), signed("gd-planA", 2), signed("project:gardenio", 2)],
  ["phase 1 of 3 · +4 wishes", "phase 1 of 3 · +4 wishes", "phase 1 of 3", "phase 1 of 3", "phase 1 of 3"]);
assert.strictEqual(G.stale(), false);
fire("farmdoc", { wishes: { open: many.concat([{ id: "gd-y", plots: [{ plan: "gd-planA" }] }]) } }); G.draw(ctx, o());
assert.strictEqual(signed("gd-planA", 2), "phase 1 of 3 · +1 wish");
// the kinless goats stand in their project's pen, every one it holds drawn (no MAX there); its sign counts the wishes past its
// capacity, no plot's count them. The pen sign is read off a pen node (the cursor's from plan gd-LurZOR44M1 step 5)
var kinless = []; for (i = 0; i < 32; i++) kinless.push({ id: "gd-k" + i, title: "kinless " + i, opened: 1000 - i });   // newest first, as the farm lists them
var kw = W.layout({ plans: [], wishes: { open: kinless, open_more: { "": 2 } } }, null, { now: 1 }), kpen = kw.index["home:/pen"];
var ktree = { nodes: { "home:/pen": { id: "home:/pen", kind: "pen", status: "32 open wishes" } } };
fire("farmdoc", { wishes: { open: kinless, open_more: { "": 2 } } }); G.draw(ctx, o({ world: kw }));
assert.ok(G.list().length === kpen.items.length && kpen.items.length === 32 && kpen.cols * kpen.rows === 35 && G.list().every(function (x) { return x.pen && x.plot === "pen"; }), "the pen's goats: " + G.list().length);
var kcell = {}; kpen.items.forEach(function (it) { kcell[it.wish.id] = [(it.x + it.w / 2) * T, (it.y + it.h / 2) * T]; });
assert.deepStrictEqual(G.list().map(function (x) { return [x.x, x.y]; }), G.list().map(function (x) { return kcell[x.id]; }), "each in its cell");
assert.deepStrictEqual([kpen.items.slice(0, 3).map(function (it) { return it.wish.id; }), kpen.more], [["gd-k31", "gd-k30", "gd-k29"], 2], "a 17-tile pen holds all 32 and open_more's 2 (29.10), planted oldest first; open_more counted");
assert.strictEqual(G.stale(), true, "the signs are re-read");
function signedT(id, lv) { return G.sign(ktree.nodes[id], ktree.nodes[id].status, lv); }
assert.deepStrictEqual([signedT("home:/pen", 2), signedT("home:/pen", 3)], ["32 open wishes · +2 not in the pen", "32 open wishes · +2 not in the pen"]);
G.draw(ctx, o({ world: kw, level: 1 })); assert.strictEqual(signedT("home:/pen", 1), "32 open wishes", "below the Plan level the pen's sign says only how many");
// and how many the herder judged fulfilled and is about to close (FarmWishes.fulfilled, gd-RkkGXAz643), alone when none are past its cap
fire("farmdoc", { wishes: { open: kinless, open_more: { "": 2 }, fulfilled: 3 } }); G.draw(ctx, o({ world: kw }));
assert.strictEqual(G.stale(), true, "the signs are re-read");
assert.strictEqual(signedT("home:/pen", 2), "32 open wishes · +2 not in the pen · 3 fulfilled, closing");
var fw = W.layout({ plans: [], wishes: { open: kinless.slice(0, 3), fulfilled: 1 } }, null, { now: 1 });
fire("farmdoc", { wishes: { open: kinless.slice(0, 3), fulfilled: 1 } }); G.draw(ctx, o({ world: fw }));
assert.strictEqual(signedT("home:/pen", 2), "32 open wishes · 1 fulfilled, closing");

/* ---------- the pen (3.1, gd-kVKuWqIK1v; homestead-world-design.md 28.2): WORLD.layout and the pen drawn ---------- */
var pdoc = function (open, more) { return { plans: [{ id: "gd-pP", project: "gardenio", created: 1, title: "Plan P", phases: [] }], wishes: { open: open, open_more: more || {} } }; };
var pws = [{ id: "gd-n1", title: "newest", tending: true, opened: 3, plots: [{ plan: "gd-pP" }], project: "gardenio" }, { id: "gd-n2", title: "second", opened: 1, project: "GardenIO" }, { id: "gd-n3", title: "third", tending: true, opened: 2, plots: [{ plan: "gd-gone" }], project: "gardenio" }];
var lw = W.layout(pdoc(pws), null, { now: 1 }), pen = lw.index["project:gardenio"].pen, cell = lw.index["project:gardenio"].home.cells.pen;
// the Pen cell of the project's farmstead (its south-west quarter, 29.2), its fence the cell, a goat per wish oldest first
// (gd-wDdun2AVwq) inside it, each once in the index
assert.deepStrictEqual([pen.id, pen.kind, pen.x, pen.y, pen.w, pen.h, pen.cols, pen.rows, pen.more], ["home:gardenio/pen", "pen", cell.x, cell.y, cell.w, cell.h, 2, 7, 0], "three goats: the 8-tile pen, its minimum (29.10)");
assert.deepStrictEqual(pen.items.map(function (it) { return [it.id, it.i, it.out]; }), [["wish:gd-n2", 0, false], ["wish:gd-n3", 1, false], ["wish:gd-n1", 2, true]], "oldest first; out: grazing by a plot on the map");
// a new wish takes the next cell and moves no goat; a closed one frees its cell and the goats after it step back one
var penOfDoc = function (d) { return W.layout(d, null, { now: 1 }).index["home:gardenio/pen"]; };
var grown = penOfDoc(pdoc(pws.concat([{ id: "gd-n0", title: "newer", opened: 4, project: "gardenio" }])));
assert.deepStrictEqual(grown.items.map(function (it) { return [it.id, it.x, it.y]; }).slice(0, 3), pen.items.map(function (it) { return [it.id, it.x, it.y]; }), "a new wish moves no goat");
assert.deepStrictEqual([grown.items[3].id, grown.items[3].i], ["wish:gd-n0", 3], "and takes the next cell");
assert.deepStrictEqual(penOfDoc(pdoc(pws.filter(function (w) { return w.id !== "gd-n3"; }))).items.map(function (it) { return [it.id, it.i]; }), [["wish:gd-n2", 0], ["wish:gd-n1", 1]], "a closed wish frees its cell; the goats after it step back");
pen.items.forEach(function (it, j) {
  assert.ok(it.x > pen.x && it.x + it.w < pen.x + pen.w && it.y > pen.y && it.y + it.h < pen.y + pen.h, "inside the fence");
  assert.ok(j % pen.cols === 0 || it.x === pen.items[j - 1].x + W.PEN.dx, "in reading order");
  assert.strictEqual(lw.index[it.id], it);
});
assert.strictEqual(lw.index["home:gardenio/pen"], pen);
// the region is fixed by shape (H-D4): the wishes move no region, only the pen's fence (29.10); each project's pen holds only its own goats
var many2 = function (n, tag, pj) { var a = []; for (var q = 0; q < n; q++) a.push({ id: "gd-" + tag + q, project: pj || "gardenio" }); return a; };
var noPen = W.layout(pdoc([]), null, { now: 1 });
assert.deepStrictEqual(W.layout(pdoc(many2(30, "a")), null, { now: 1 }).lattice, noPen.lattice, "no wish moves a region");
assert.strictEqual(JSON.stringify(W.layout(pdoc(pws), null, { now: 1 })), JSON.stringify(lw), "the same document, byte for byte the same world");
assert.deepStrictEqual([W.penOf({ open: many2(30, "c") }, "gardenio", cell).items.length, W.penOf({ open: many2(30, "c") }, "gardenio", cell).more, W.penOf(null, "gardenio", cell).items.length, W.penOf({ open: [{ id: "" }] }, "", cell).items.length], [14, 16, 0, 0], "the 8-tile pen holds 2 x 7, the rest in more");
var two = W.layout(pdoc(pws.concat(many2(2, "z", "zed"))), null, { now: 1 });
assert.deepStrictEqual([two.index["home:gardenio/pen"].items.length, two.index["home:zed/pen"].items.map(function (it) { return it.id; })], [3, ["wish:gd-z0", "wish:gd-z1"]], "two projects, two pens, each its own goats");
// at= and resolve (28.7): <project>/pen and <project>/wish:<id>; a legacy yard link still lands
assert.deepStrictEqual([W.atFor(lw, null, "home:gardenio/pen"), W.atFor(lw, null, "wish:gd-n2")], ["gardenio/pen", "gardenio/wish%3Agd-n2"]);
assert.deepStrictEqual([W.resolve(lw, "gardenio/pen").kind, W.resolve(lw, "gardenio/wish%3Agd-n2").id, W.resolve(lw, "gardenio/wish%3Agd-n2").level, W.resolve(lw, "yard/wish%3Agd-n2").id, W.resolve(lw, "yard/wish%3Agd-n2").moved], ["pen", "wish:gd-n2", 3, "wish:gd-n2", "yard"]);
// the jump index (3.3, gd-NbWjIdLTnd): each pen and its goats, so a phone reaches them from Go
var pidx = W.searchIndex(lw), kinds = function (q) { return W.search(pidx, q).map(function (e) { return [e.kind, e.id]; }); };
assert.deepStrictEqual(pidx.filter(function (e) { return e.kind === "pen" || e.kind === "goat"; }).map(function (e) { return [e.kind, e.id, e.bead || "", e.title, e.note]; }),
  [["pen", "home:gardenio/pen", "", "gardenio pen", "3 open wishes"], ["goat", "wish:gd-n2", "gd-n2", "second", "untended"], ["goat", "wish:gd-n3", "gd-n3", "third", "tended"], ["goat", "wish:gd-n1", "gd-n1", "newest", "out grazing by its plan"]]);
assert.deepStrictEqual([kinds("pen")[0], kinds("goats")[0], kinds("open wishes")[0]], [["pen", "home:gardenio/pen"], ["pen", "home:gardenio/pen"], ["pen", "home:gardenio/pen"]], "by its name or what it holds");
assert.deepStrictEqual([kinds("GD-N2")[0], kinds("second")[0]], [["goat", "wish:gd-n2"], ["goat", "wish:gd-n2"]], "a goat by its wish's bead id or its title");
assert.ok(!W.searchIndex(noPen).some(function (e) { return e.kind === "pen" || e.kind === "goat"; }), "no goat, no row");
// drawn: the fence from the Project level in, the goats and the out goat's open gate at Plan and Bead; the gate names its wish
var fenced = 0; ctx.drawImage = undefined;
rects = []; fire("farmdoc", { wishes: { open: pws } }); G.draw(ctx, o({ world: lw, level: 1 })); fenced = rects.length;
assert.ok(fenced > 0 && G.list().length === 0, "the fence at Project, no goat");
rects = []; G.draw(ctx, o({ world: lw, level: 0 })); assert.strictEqual(rects.length, 0, "nothing at World");
rects = []; G.draw(ctx, o({ world: lw }));
assert.deepStrictEqual(G.list().map(function (x) { return [x.id, x.plot, x.pen, x.out]; }), [["gd-n1", "gd-pP", false, false], ["gd-n2", "pen", true, false], ["gd-n3", "pen", true, false], ["gd-n1", "pen", false, true]],
  "gd-n1 grazing by plan P, the penned goats, then gd-n1's open gate in the pen, last (on top)");
var gt = G.list()[3], gh = G.at(gt.css.x + gt.css.w / 2, gt.css.y + gt.css.h / 2);
assert.deepStrictEqual([gh.id, gh.out, gh.plot, gh.grazes, gh.pkey], ["gd-n1", true, "pen", "gd-pP", "gardenio"], "the gate's hit: in the pen, out, grazing by plan P");
var ghc = G.card(gh, "", function () { return "1 day"; });
assert.ok(/out grazing by its plan$/.test(ghc.lines[2][1]), "its card says it is out");
assert.deepStrictEqual(ghc.lines[3][1][1], { text: "Zoom to it on the map", href: "/garden?at=gardenio/gd-pP", cls: "ggoto", id: "gd-pP" }, "and takes the cursor to plan P, where it grazes (gd-xlRpUpTjPZ)");
assert.deepStrictEqual([G.grazes("gd-n1", lw), G.grazes("gd-n2", lw), G.grazes("gd-n3", lw), G.grazes("gd-none", lw), G.grazes("gd-n1", null)], ["gd-pP", "pen", "pen", "", ""], "grazes: Enter on an out goat's cell follows it to plan P");
assert.ok(rects.some(function (r) { return r.c === A.palette(false).c; }), "the gate in the fence's wood");
var pg = W.layout({ plans: [], wishes: { open: pws } }, null, { now: 1 });
assert.ok(pg.index["home:gardenio/pen"].items.every(function (it) { return !it.out; }), "with no plot on the map nothing is out");
rects = []; G.draw(ctx, o({ world: lw, view: { x: -99999, y: -99999, w: 10, h: 10 } })); assert.deepStrictEqual([rects.length, G.list().length], [0, 0], "off the view: no fence, no goat");
// below the Plan level nothing is counted
G.draw(ctx, o({ level: 1 })); assert.strictEqual(signed("gd-planB", 2), "phase 1 of 3");
// at the Project level a project's sign says its open wishes; a plot's, and the World's, nothing
fire("farmdoc", { wishes: { open: [{ id: "gd-x1", project: "gardenio" }, { id: "gd-x2", project: "gardenio" }, { id: "gd-x3", project: "acme" }], open_more: { gardenio: 1 } } });
assert.strictEqual(G.stale(), true, "the signs are re-read");
tree.nodes["project:acme"] = { kind: "biome", key: "acme" }; tree.nodes["project:music"] = { kind: "biome", key: "music" };
assert.deepStrictEqual([signed("project:gardenio", 1), signed("project:acme", 1), signed("project:music", 1), signed("project:gardenio", 0), signed("gd-planA", 1)],
  ["phase 1 of 3 · 3 wishes", "phase 1 of 3 · 1 wish", "phase 1 of 3", "phase 1 of 3", "phase 1 of 3"]);
assert.strictEqual(G.stale(), false);
// the lanes (homestead-world-design.md 29.3, X-D11): plot la east of the farmstead has the east lane on its west and the north
// and south lanes above and below it, so its goats' ring runs on those lanes' verges (the plot's edge tiles beside them) and on
// the grass gap to its east; la and lb (south-east of the farm) together make a ring whose east side crosses the south lane.
// No grazing spot, moving or reduced, at Plan or at Bead, is on a lane tile
function lplan(id, created) { return { id: id, title: id, project: "gardenio", created: created, activity: 1, counts: { in_progress: 1 }, phases: [{ id: id + "-p1", title: "P", counts: {}, tasks: [] }], unphased: { id: "", counts: {}, tasks: [] } }; }
var nw = W.layout({ plans: [lplan("la", 1), lplan("lb", 2)], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } }, null, { now: 1, readonly: true });
var nb = nw.index["project:gardenio"], la = nw.index.la, lb = nw.index.lb;
assert.deepStrictEqual([W.SPIRAL[la.slot], W.SPIRAL[lb.slot]], [{ c: 1, r: 0 }, { c: 1, r: 1 }], "la east of the farm, lb south-east");
var lg = G.place([{ id: "gd-l1", title: "on la", plots: [{ plan: "la" }] }, { id: "gd-l2", title: "on la", plots: [{ plan: "la" }] }, { id: "gd-l3", title: "on both", plots: [{ plan: "la" }, { plan: "lb" }] }], nw);
assert.deepStrictEqual(lg[0].tour.lanes, nb.lanes, "a tour knows its biome's lanes");
assert.deepStrictEqual(G.sides(la, nb.lanes), { x0: (la.x + 0.5) * T, x1: (la.x + la.w + 1) * T, y0: (la.y + 0.5) * T, y1: (la.y + la.h - 0.5) * T }, "three sides on the verges, the east one in the gap");
assert.deepStrictEqual(G.sides(la, []), { x0: (la.x - 1) * T, x1: (la.x + la.w + 1) * T, y0: (la.y - 1) * T, y1: (la.y + la.h + 1) * T }, "no lanes: the path round it");
assert.ok(lg[0].tour.mouths.length > 0 && lg[0].tour.mouths.every(function (m) { return !G.onLane(nb.lanes, m); }), "its gutters' mouths on the verges");
var crossed = false; for (var cy = lg[2].tour.box.y; cy < lg[2].tour.box.y + lg[2].tour.box.h; cy += 8) if (G.onLane(nb.lanes, { x: lg[2].tour.pts[1].x, y: cy })) crossed = true;
assert.ok(crossed, "la and lb's ring crosses the south lane on its east side");
var verge = 0, graze = 0;
[{ fit: null, size: planSz }, { fit: { v: true, h: true }, size: beadBody }].forEach(function (lo) {
  lg.forEach(function (gg) {
    for (var lt = 0; lt < 900000; lt += 250) {
      var lp = G.wander(gg, lt, lo);
      if (lp.walking) continue;
      graze++;
      assert.ok(!G.onLane(nb.lanes, lp), gg.id + " grazes on a lane at " + lt + ": " + JSON.stringify(lp));
      if (lp.x === (la.x + 0.5) * T || lp.y === (la.y + 0.5) * T || lp.y === (la.y + la.h - 0.5) * T) verge++;
    }
    assert.ok(!G.onLane(nb.lanes, G.wander(gg, 5000, true)), gg.id + ": reduced motion, off the lanes too");
  });
});
assert.ok(verge > graze / 4, "the goats graze the verges beside the lanes: " + verge + " of " + graze);
`
	var args []string
	for _, f := range []string{"world.js", "goats.js", "art.js", "birds.js", "cursor.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node goats test failed: %v\n%s", err, out)
	}
}

// goats.js loads after world.js, which it reads, and before garden.js, which draws the goats before the birds and the
// gardeners, wakes for a goat's next step, asks the signs for "+N wishes", names a goat's wish in the steward's tip on hover
// and opens its thread on a click.
func TestGoatsJSOnThePage(t *testing.T) {
	at := func(s string) int { return strings.Index(pageHTML, `{{asset "`+s+`"}}`) }
	g := at("goats.js")
	if w := at("world.js"); w < 0 || g < 0 || w > g {
		t.Errorf("WorldJS at %d, goats.js at %d: goats.js must load after it", w, g)
	}
	if a := at("garden.js"); g > a {
		t.Errorf("goats.js at %d, garden.js at %d: goats.js must load first", g, a)
	}
	d, b := strings.Index(pageSceneJS, "GOATS.draw("), strings.Index(pageSceneJS, "BIRDS.draw(")
	if d < 0 || b < 0 || d > b {
		t.Error("scene.js does not draw the goats before the birds")
	}
	for _, s := range []string{"GOATS.sign(", "GOATS.onWake(", "GOATS.stale()", "GOATS.wire(board,cv,", "goats.click(e,q[0],q[1])", "CHAT.open(g.id)", "go:goats.go}", "if(goats.enter(nav.node()))return;", "tapped:tapWas,world:function(){return world;},to:cursorTo,into:cursorInto,focus:focusCursor,"} {
		if !strings.Contains(pageAppJS, s) {
			t.Errorf("garden.js does not call %s", s)
		}
	}
	for _, s := range []string{"grazes(n.open, env.world())", "env.to(id, false) : env.into(id)", "e.stopImmediatePropagation(); env.tapped();"} {
		if !strings.Contains(pageGoatsJS, s) {
			t.Errorf("goats.js wire does not hold %s (the goat jump, gd-xlRpUpTjPZ)", s)
		}
	}
	if !strings.Contains(pageStipJS, "GOATS.card(h.g, s.base, s.ago)") || !strings.Contains(pageGoatsJS, "STIP.goat(g, false)") {
		t.Error("a goat's hover does not show its card in the steward's tip")
	}
	if !strings.Contains(pageStipJS, "a.classList.contains('ggoto') && env().go && env().go(a.dataset.id)") {
		t.Error("the card's \"Zoom to it on the map\" is not a cursor jump (stip.js env().go, gd-xlRpUpTjPZ)")
	}
}

// Ghost and solid, hover and click in a real browser (gd-2cPnm71cxT): at the Plan level round plan A a mouse over the tended
// wish's goat shows the tip naming the wish, who asked and that it is tended, and off it the tip goes; a click asks the Commons
// for its thread and, with none, opens the wish's bead view; on a phone the first tap names it and the second asks for the
// thread. The fixture's untended wish is a ghost in its project's pen, framed on the Pen cell (gd-pvPzGCltmS), beside the open
// gate of the tended wish, which is out grazing by plan A (gd-kVKuWqIK1v). What is asserted lives in
// testdata/goats_browser.js.
func TestGoatsUnderChromium(t *testing.T) {
	underChromium(t, "goats_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
