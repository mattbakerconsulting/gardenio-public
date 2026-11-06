/* The courtyard's cells sized by what they hold (homestead-world-design.md 29.10, X-D13, as 29.11 amends them; plan gd-PJB3D7glwF step 12
   gd-nOkEhUBDk4), run by TestHomeCellsUnderNode (page_test.go): node home_cells_test.js <web dir>. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert"), web = process.argv[2], W = require(path.join(web, "world.js")), L = require(path.join(web, "lanes.js"));
function plan(id, project, created) { return { id: id, title: "Plan " + id, project: project, created: created || 1, activity: 1, counts: {}, phases: [], unphased: { id: "", counts: {}, tasks: [] } }; }
function escs(n, project) { var o = []; for (var i = 0; i < n; i++) o.push({ id: project + "-e" + (i < 10 ? "0" : "") + i, state: "pending", human: true, since: i, project: project, family: "merge" }); return o; }
function wishes(n, project) { var o = []; for (var i = 0; i < n; i++) o.push({ id: project + "-w" + i, title: "W" + i, project: project, opened: i }); return o; }
function lay(extra, prev) { var d = { plans: [plan("zp", "zoo")] }; Object.keys(extra || {}).forEach(function (k) { d[k] = extra[k]; }); return W.layout(d, null, { now: 1, readonly: true, prev: prev }); }
function wd(w, key) { var c = w.index["project:" + (key || "zoo")].home.cells; return { escalation: c.escalation.w, mailbox: c.mailbox.w, pen: c.pen.w, growing: c.growing.w, barn: c.barn.w }; }
var R = { x: 0, y: 0, w: 32, h: 32 }, i;

// an empty farm: every cell at its minimum, the Barn the rest (22), the north arm on column 22, the south on 23, 15 tiles of grass
var w0 = lay(), h0 = w0.index["project:zoo"].home, c0 = h0.cells;
assert.deepStrictEqual(wd(w0), { escalation: 3, mailbox: 5, pen: 8, growing: 8, barn: 22 }, "an empty farm at its minimums");
assert.deepStrictEqual([h0.cross.xn - h0.x, h0.cross.xs - h0.x, h0.cross.y - h0.y, c0.growing.x - 1 - (c0.pen.x + c0.pen.w)], [22, 23, 15, 15], "the arms on 22 and 23, row 15; 15 tiles of grass");
assert.deepStrictEqual([c0.escalation.x - h0.x, c0.mailbox.x - h0.x, h0.sign.x - h0.x, h0.keep.barn, h0.bay.x - h0.x], [23, 27, 12, { x: h0.x, y: h0.y, w: 7, h: 5 }, 0], "the asks east of the north arm; the sign, the keep and the bay fixed");

// each cell's ladder: one column a step, from its minimum to its maximum, no width that adds no column
var cols = function (zone, w) {
  var o = {}; o[zone] = w; var c = W.farmstead({ x: 0, y: 0, w: W.homeW(o), h: 32 }, o).cells[zone];
  if (zone === "pen") return W.penOf({ open: [] }, "", c).cols;
  var xs = {}; W.slotsOf(c, zone).forEach(function (r) { xs[r.x] = 1; }); return Object.keys(xs).length;
};
var LADDER = { escalation: [3, 5, 6, 7, 8, 9, 10, 11], mailbox: [5, 6, 7, 8, 9], pen: [8, 11, 14, 17, 20, 23], growing: [8, 10, 12, 14, 16, 18, 20, 22] };
Object.keys(LADDER).forEach(function (z) {
  var l = W.ladderOf(z);
  assert.deepStrictEqual(l, LADDER[z], z + "'s ladder");
  l.forEach(function (w, k) { if (k) assert.strictEqual(cols(z, w), cols(z, l[k - 1]) + 1, z + " " + w + " adds one column"); });
  for (var w = l[0]; w <= l[l.length - 1]; w++) if (l.indexOf(w) < 0) assert.strictEqual(cols(z, w), cols(z, w - 1), z + " " + w + " adds none, so is no rung");
});
var caps = function (z, M) { return W.ladderOf(z).map(function (w) { return W.capAt(z, w, M); }); };
assert.deepStrictEqual([caps("mailbox"), caps("pen"), caps("growing")], [[18, 27, 38, 49, 60], [14, 21, 28, 35, 42, 49], [21, 28, 35, 42, 49, 56, 63, 70]], "the Mailbox less its keep, 7 goats a Pen column, 7 pairs a field column (rows 32 px apart)");
[5, 6, 7, 8, 9].forEach(function (M) { assert.deepStrictEqual(caps("escalation", M), [11, 22, 33, 44, 55, 66, 77, 88], "11 an Escalations column at Mailbox " + M + ": the sign never meets them (29.11)"); });
var sw = W.farmstead({ x: 0, y: 0, w: W.homeW({ escalation: 11, mailbox: 9 }), h: 32 }, { escalation: 11, mailbox: 9 }), sc = sw.cells.escalation;
assert.ok(!W.hit(sw.sign, sc) && sc.x + sc.w === sw.cells.mailbox.x - 1, "at the widest the sign is clear of the Escalations; the asks' path between them");
assert.deepStrictEqual([sw.w, sw.cells.barn.w, sw.cross.xn], [44, 22, 22], "the two asks' maxima widen the farm to 44 and leave the Barn its 22");

// the counts: the Escalations' total (by_project folded to the key), the Mailbox's letters less them, the Pen's wishes and
// open_more, the field's max(stands + loose / 2, loose) in pair slots (two loose beads to one), a gardener's stand counted as gardenerSpots places it (fieldHome)
var hc = W.homeCounts({ escalations: { open: escs(2, "zoo"), by_project: { Zoo: 3, zoo: 2 } }, home: { zoo: { letters: 9, field: 4 } }, wishes: { open: wishes(3, "zoo"), open_more: { zoo: 2 } } }, "zoo", 1);
assert.deepStrictEqual(hc, { escalation: 5, mailbox: 4, pen: 5, growing: 4 }, "each cell's count from the farm document alone");
assert.strictEqual(W.homeCounts({ home: { zoo: { field: 5 } } }, "zoo", 4, 1).growing, 7, "a tended loose bead takes a whole slot: 4 stands and 6 halves");
assert.strictEqual(W.homeCounts({ home: { zoo: { field: 1 } } }, "zoo", 6).growing, 7, "the stands and the loose beads");
var gs = [{ id: "g1", project: "zoo", role: "crew" }, { id: "g2", project: "zoo", role: "steward" }, { id: "g3", project: "zoo", role: "thread" }, { id: "g4", role: "crew" }];
var wg = lay({ gardeners: gs }), st = W.gardenerSpots(wg, gs, 3), has = function (id) { return !!wg.index[id]; };
assert.deepStrictEqual([st.fields["project:zoo"], st.fields["project:"], gs.map(function (g) { return W.fieldHome(g, has); })], [["g1"], ["g3", "g4"], ["project:zoo", null, "project:", "project:"]], "one rule: a steward at its sign, a thread agent and the projectless in the Commons");

// full at its maximum, then one more: the Escalations' crate, the field's badge, the Pen's and the Mailbox's "+N"
var we = lay({ escalations: { open: escs(88, "zoo") }, home: { zoo: { letters: 88 } } }), ze = we.index["home:zoo/escalation"];
assert.deepStrictEqual([wd(we).escalation, ze.items.length, ze.crate, ze.more], [11, 88, null, 0], "88 fill the widest cell, no crate");
we = lay({ escalations: { open: escs(89, "zoo") }, home: { zoo: { letters: 89 } } }); ze = we.index["home:zoo/escalation"];
assert.deepStrictEqual([wd(we).escalation, ze.items.length, ze.crate.n, ze.crate.slot, ze.more], [11, 87, 2, 87, 2], "the 89th: 87 and a +2 crate in the last slot");
var stand = function (n) { var o = []; for (var k = 0; k < n; k++) o.push({ id: "gz" + k, project: "zoo", role: "crew", state: "idle", bucket: "idle" }); return o; };
var rows = function (n, pre, kind) { var o = []; for (var k = 0; k < n; k++) o.push(kind === "agent" ? { id: "st:" + pre + k, g: { id: pre + k, project: "zoo" } } : { id: pre + k, project: "zoo", type: "decision" }); return o; };
var wf = lay({ gardeners: stand(35), home: { zoo: { field: 70 } } }), ff = W.homeFill(wf, { agent: rows(35, "gz", "agent"), work: rows(70, "lw") }, {})["project:zoo"].growing;
assert.deepStrictEqual([wd(wf).growing, ff.items.length, ff.more, !!ff.badge], [22, 105, 0, false], "35 stands and 70 loose beads, two to a slot, fill the widest field");
wf = lay({ gardeners: stand(36), home: { zoo: { field: 70 } } }); ff = W.homeFill(wf, { agent: rows(36, "gz", "agent"), work: rows(70, "lw") }, {})["project:zoo"].growing;
assert.deepStrictEqual([wd(wf).growing, ff.badge.slot, ff.badge.n, ff.items.length], [22, 69, 4, 102], "the 36th stand: the badge in the last slot, the four loose beads it leaves no half for behind it");
var wp = lay({ wishes: { open: wishes(49, "zoo") } }), zp = wp.index["home:zoo/pen"];
assert.deepStrictEqual([wd(wp).pen, zp.items.length, zp.more], [23, 49, 0], "49 goats fill the widest pen");
zp = lay({ wishes: { open: wishes(49, "zoo"), open_more: { zoo: 1 } } }).index["home:zoo/pen"];
assert.deepStrictEqual([zp.w, zp.items.length, zp.more], [23, 49, 1], "one more: the pen's +1");
var wm = lay({ home: { zoo: { letters: 60 } } }), fm = W.homeFill(wm, { decision: rows(60, "d") }, {})["project:zoo"].mailbox;
assert.deepStrictEqual([wd(wm).mailbox, fm.items.length, fm.more], [9, 60, 0], "60 letters fill the widest Mailbox");
fm = W.homeFill(lay({ home: { zoo: { letters: 61 } } }), { decision: rows(61, "d") }, {})["project:zoo"].mailbox;
assert.deepStrictEqual([fm.items.length, fm.more], [60, 1], "the 61st: +1 not shown");

// no south floor rule since 29.11: a full Pen and a full field each keep their own width, and the farm widens to hold them
var wb = lay({ wishes: { open: wishes(49, "zoo") }, home: { zoo: { field: 80 } } });
assert.deepStrictEqual([wd(wb).pen, wd(wb).growing, wb.index["home:zoo"].w], [23, 22, 48], "both full: 23 + 22, the farm 48 wide");
assert.deepStrictEqual([W.homeWidths({ pen: 49, growing: 25 }), W.homeWidths({ pen: 49, growing: 15 })], [{ mailbox: 5, escalation: 3, pen: 23, growing: 10 }, { mailbox: 5, escalation: 3, pen: 23, growing: 8 }], "a full pen and a small field: neither squeezed");
assert.deepStrictEqual(W.homeWidths({ escalation: 40, mailbox: 45, pen: 28, growing: 34 }), { mailbox: 8, escalation: 7, pen: 14, growing: 12 }, "29.2's counts give its widths (its Pen's 15 is no rung: 14 holds the same 28; the field's 34 pairs fit 12 tiles at 32 px rows)");
assert.strictEqual(W.homeW({ mailbox: 8, escalation: 7, pen: 14, growing: 12 }), 40, "and a farm 40 wide");

// the hysteresis: 11, 12, 11, 10, 9 Escalations, each layout given the last as prev, are 1, 2, 2, 2, 1 columns; a reload sizes afresh
var prev = null, seq = [11, 12, 11, 10, 9].map(function (n) { prev = lay({ escalations: { open: escs(n, "zoo") }, home: { zoo: { letters: n } } }, prev); return wd(prev).escalation; });
assert.deepStrictEqual(seq, [3, 5, 5, 5, 3], "grows at once, shrinks only once n + HOME.slack fits");
assert.strictEqual(wd(lay({ escalations: { open: escs(11, "zoo") }, home: { zoo: { letters: 11 } } })).escalation, 3, "no prev: the narrowest that holds 11");
assert.strictEqual(W.homeWidths({ pen: 13 }, { pen: 11 }).pen, 11, "13 goats in an 11-tile pen of 21: 13 + 2 still needs 21");

// one count change that keeps the farm 32 wide moves only that farm's cells: every plot, lane, region and sign, and every other
// biome, are deep-equal (a count that widens it is farm_grows_test.js's)
var two = function (n) { return W.layout({ plans: [plan("a1", "alpha", 1), plan("b1", "beta", 2)], wishes: { open: wishes(n, "alpha") } }, null, { now: 1, readonly: true }); };
var strip = function (b) { var o = JSON.parse(JSON.stringify({ region: b.region, foot: b.foot, used: b.used, lanes: b.lanes, sign: b.sign, cell: b.cell, plots: b.plots.map(function (p) { return [p.id, p.x, p.y, p.w, p.h]; }), home: [b.home.x, b.home.y, b.home.w, b.home.h] })); return o; };
var ta = two(0), tb = two(30);
assert.deepStrictEqual(tb.biomes.map(strip), ta.biomes.map(strip), "no plot, lane, region or sign moves");
assert.deepStrictEqual([tb.index["project:beta"].home.cells, tb.bbox, tb.lattice], [ta.index["project:beta"].home.cells, ta.bbox, ta.lattice], "nor the other farm, nor the world");
tb.biomes.forEach(function (b) { assert.deepStrictEqual(W.shape(b.centre.x, b.centre.y, b.grid, !!b.overflow, b.counts.graves).foot, b.foot, "shape() is the counts' none: " + b.id); assert.deepStrictEqual(W.slotRect(b.centre, 0), { x: b.home.x, y: b.home.y, w: 32, h: 32 }); });
assert.deepStrictEqual([wd(ta, "alpha").pen, wd(tb, "alpha").pen, wd(tb, "alpha").growing, tb.index["home:alpha"].w], [8, 17, 8, 32], "alpha's Pen grows east into its grass");

// the walk out from a field stand when the arms differ: north by the east-west arm to the north arm, south down the south arm
var seven = []; for (i = 1; i <= 7; i++) seven.push(plan("p" + i, "zoo", i));
var ww = W.layout({ plans: seven }, null, { now: 1, readonly: true }), bw = ww.index["project:zoo"], hw = bw.home, gw = hw.cells.growing, X = hw.cross;
var from = { x: (gw.x + 2) * 16 - 8, y: (gw.y + 2) * 16 - 24 }, px = function (tx) { return Math.round((tx + 0.5) * 16 - 8); }, py = function (ty) { return Math.round((ty + 0.5) * 16 - 24); };
var spot = function (p) { return { x: (p.x + 5) * 16 - 8, y: (p.y + 5) * 16 - 24 }; }, north = ww.index.p7, south = ww.index.p3;
assert.deepStrictEqual([W.SPIRAL[north.slot], W.SPIRAL[south.slot], X.xn !== X.xs], [{ c: 0, r: -1 }, { c: 0, r: 1 }, true], "a plot due north and one due south; the arms apart");
var wn = L.walkLanes(ww, bw.id, from, north.id, spot(north)), ws = L.walkLanes(ww, bw.id, from, south.id, spot(south));
assert.deepStrictEqual(wn.slice(0, 4), [from, { x: from.x, y: py(X.y) }, { x: px(X.xn), y: py(X.y) }, { x: px(X.xn), y: wn[3].y }], "north: up to the east-west arm, along it to the north arm, up it");
assert.ok(wn[3].y < (hw.y) * 16, "out of the north gate");
assert.deepStrictEqual(ws.slice(0, 3), [from, { x: px(X.xs), y: from.y }, { x: px(X.xs), y: ws[2].y }], "south: along its row to the south arm, down it");
assert.ok(ws[2].y > (hw.y + hw.h) * 16 - 24, "out of the south gate");

console.log(JSON.stringify({ ok: true }));
