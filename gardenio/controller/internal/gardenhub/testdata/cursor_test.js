/* The cursor world's pure model (gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md section 10.1), run by
   TestCursorUnderNode:  node cursor_test.js <web dir>
   Two worlds: a hand-built layout in the spec's geometry (square 32-tile plots at a 34 pitch, 3x3 beds of 9x9 at offsets 2/12/22,
   block-major tasks, the 5-wide lattice), where coordinates are checked against rects exactly; and WORLD.layout() over the
   fixture farm and the 10x farm, where the tree's invariants, the links and the walks are checked. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var W = require(path.join(process.argv[2], "world.js")), C = require(path.join(process.argv[2], "cursor.js")), B = require(path.join(process.argv[2], "beacons.js")), A = require(path.join(process.argv[2], "art.js"));
var gen = require(path.join(__dirname, "world_tenx.js")), fixture = require(path.join(__dirname, "farm_fixture.json"));
var NOW = 1792001000000, report = {};
function last(a) { return a[a.length - 1]; }
function ids(p) { return p.join(" > "); }

/* ---------- a layout in the spec's geometry, with 2b.1's field names (gd-zjATSCW3s1): biome.cell.k, bed.at/other/n, bed.crate
   and plot.crate with at, cell.i ---------- */
var OFF = [2, 12, 22];
function task(id, st, extra) { var t = { id: id, title: "Task " + id, type: "task", state: st || "open", updated: 1792000000 }; if (extra) Object.keys(extra).forEach(function (k) { t[k] = extra[k]; }); return t; }
/* spec: {key, name, k, plans: [{id, slot, phases: [task count, ...], other: n, more: {phase index: n}, needs}], meadow, orchard, nursery, overflow} */
function synth(projects) {
  var world = { biomes: [], index: {}, bbox: null };
  projects.forEach(function (sp) {
    var cx = (sp.k % 5) * 300, cy = Math.floor(sp.k / 5) * 300, key = sp.key;
    var b = { id: "project:" + key, kind: "biome", key: key, name: sp.name || key, cell: { k: sp.k, c: sp.k % 5, r: Math.floor(sp.k / 5) }, needs: 0, plots: [], orchard: [], meadow: [], nursery: [], overflow: null, counts: {} };
    sp.plans.forEach(function (pl) {
      var s = W.SPIRAL[pl.slot], px = cx + s.c * 34 - 16, py = cy + s.r * 34 - 16, phases = [], beds = [];
      pl.phases.forEach(function (n, j) {
        var tl = []; for (var i = 0; i < n; i++) tl.push(task(pl.id + "-" + j + "-" + i, i % 2 ? "in_progress" : "open"));
        phases.push({ id: pl.id + "-ph" + j, title: "Phase " + (j + 1) + " of " + pl.id, status: j < (pl.closed || 0) ? "closed" : "open", counts: { open: n, closed: 2, blocked: j === 1 ? 1 : 0 }, tasks: tl, more: (pl.more || {})[j] || 0 });
      });
      var unph = { id: "", other: true, title: "Other", counts: { open: pl.other || 0 }, tasks: [] };
      for (var u = 0; u < (pl.other || 0); u++) unph.tasks.push(task(pl.id + "-o" + u));
      var rows = phases.slice(); if (unph.tasks.length) rows.push(unph);
      var plot = { id: pl.id, kind: "plot", slot: pl.slot, x: px, y: py, w: 32, h: 32, project: key, needs: pl.needs || 0,
        plan: { id: pl.id, title: pl.title || "Plan " + pl.id, project: key, counts: { open: 10, closed: 4 }, phases: phases, unphased: unph } };
      var room = rows.length > 9 ? 8 : 9;
      rows.slice(0, room).forEach(function (ph, i) {
        var bx = px + OFF[i % 3], by = py + OFF[Math.floor(i / 3)], bed = { id: ph.other ? pl.id + "/other" : ph.id, other: !!ph.other, title: ph.title, status: ph.status || "", n: i + 1, at: { c: i % 3, r: Math.floor(i / 3) }, x: bx, y: by, w: 9, h: 9, cells: [], stubble: 2 };
        ph.tasks.forEach(function (tk, j) { var at = C.blockCell(j); bed.cells.push({ id: tk.id, kind: "task", i: j, x: bx + at.c, y: by + at.r, w: 1, h: 1, task: tk, plot: pl.id, phase: ph.id }); world.index[tk.id] = bed.cells[j]; });
        if (ph.more) { var ci = ph.tasks.length, ca = C.blockCell(ci); bed.crate = { id: bed.id + "/more", kind: "crate", n: ph.more, i: ci, at: ca, x: bx + ca.c, y: by + ca.r, w: 1, h: 1 }; }
        beds.push(bed);
      });
      if (rows.length > 9) plot.crate = { id: pl.id + "/phases", kind: "phases", n: rows.length - 8, at: { c: 2, r: 2 }, x: px + 22, y: py + 22, w: 9, h: 9 };
      plot.beds = beds; b.plots.push(plot); b.needs += plot.needs; world.index[plot.id] = plot;
    });
    var top = cy - 2 * 34 - 16;
    var hs = W.farmstead({ x: cx - 16, y: cy - 16, w: 32, h: 32 }, { escalation: 7, mailbox: 8, pen: 15, growing: 16 }), field = W.slotsOf(hs.cells.growing, "growing");
    /* the farmstead (29.1, 29.2) in slot 0, the centre of the plots, at 29.2's widths (29.10): its zones, a pen, the loose beads (the Meadow's) in the Growing field's slots */
    b.home = { id: "home:" + key, kind: "home", project: key, x: hs.x, y: hs.y, w: hs.w, h: hs.h, cells: hs.cells, bay: hs.bay, keep: hs.keep, counts: {}, zones: {} };
    W.HOME.cells.forEach(function (c) { var r = hs.cells[c[0]]; b.home.zones[c[0]] = c[0] === "pen" ? (b.pen = W.penOf({ open: [] }, key, r)) : c[0] === "escalation" ? W.escOf({ open: sp.escalations || [] }, key, r) : { id: b.home.id + "/" + c[0], kind: "zone", zone: c[0], project: key, x: r.x, y: r.y, w: r.w, h: r.h }; });
    ["meadow", "orchard", "nursery"].forEach(function (band, bi) {
      for (var i = 0; i < (sp[band] || 0); i++) {
        var id = key + "-" + band + i, y = band === "nursery" ? cy + 3 * 34 : top - 6 + bi * 3, it = { id: id, kind: { meadow: "flower", orchard: "tree", nursery: "seed" }[band], x: cx - 100 + i * 2, y: y, w: 1, h: 1, project: key };
        if (band === "meadow") { it.x = field[i >> 1].x + (i & 1); it.y = field[i >> 1].y; it.slot = i >> 1; b.home.zones.growing.halves = i + 1; }   // two to a pair slot (28.12)
        it[band === "orchard" ? "plan" : band === "nursery" ? "seed" : "task"] = { id: id, title: band + " bead " + i, state: "open", updated: 1792000000 };
        b[band].push(it); world.index[id] = it;
      }
    });
    if (sp.overflow) b.overflow = { id: "overflow:" + key, kind: "overflow", n: sp.overflow, ids: ["late-a", "late-b"].slice(0, sp.overflow), x: cx, y: cy + 4 * 34, w: 32, h: 32, project: key };
    var used = W.union(null, b.home); b.plots.forEach(function (p) { used = W.union(used, p); }); b.orchard.concat(b.meadow, b.nursery).forEach(function (it) { used = W.union(used, it); });
    if (b.overflow) used = W.union(used, b.overflow);
    b.used = used; b.x = used.x; b.y = used.y; b.w = used.w; b.h = used.h;
    b.home.zones.escalation.items.forEach(function (it) { world.index[it.id] = it; });
    world.biomes.push(b); world.index[b.id] = b;
    world.bbox = W.union(world.bbox, W.tiles(used));
  });
  return world;
}
var full9 = function (n) { var a = []; for (var i = 0; i < n; i++) a.push(9); return a; };
var S = synth([
  { key: "alpha", k: 2, needs: 0, meadow: 3, orchard: 2, nursery: 2, overflow: 2, escalations: [
    { id: "e2", title: "Roll the chart", kind: "escalation", state: "approved", human: false, waits_on: "the deployer", since: NOW / 1000 - 600, project: "alpha", family: "deploy" },
    { id: "e1", title: "Merge the fix", kind: "escalation", state: "pending", human: true, since: NOW / 1000 - 7200, project: "alpha", family: "merge" }], plans: [
    { id: "a0", slot: 1, phases: [5, 12, 3, 9, 4, 60, 2], other: 2, more: { 5: 7 }, needs: 1, closed: 1 },
    { id: "a1", slot: 6, phases: [9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9] },   // 11 phases: eight beds and a crate
    { id: "a2", slot: 8, phases: [1, 1, 1] },
    { id: "a5", slot: 2, phases: [4, 4, 4, 4, 4, 4] },
    { id: "a8", slot: 5, phases: [2] }] },
  { key: "beta", k: 3, plans: [{ id: "b0", slot: 3, phases: [3, 3, 3, 3, 3, 3] }, { id: "b4", slot: 1, phases: [6, 6, 6] }, { id: "b7", slot: 4, phases: [1] }] },
  { key: "", name: "Commons", k: 1, meadow: 4, plans: [] },
  { key: "gamma", k: 7, plans: [{ id: "g0", slot: 1, phases: [8] }] }]);
/* alpha's strip: two letters, and two growing beads, of which a0-0-1 is a plan's task and so is not drawn twice (28.2) */
var home = W.homeFill(S, { decision: [{ id: "d1", title: "Answer me", type: "decision", project: "alpha" }, { id: "d2", title: "Approve me", type: "escalation", project: "alpha" }],
  work: [{ id: "a0-0-1", title: "Growing in the field", state: "in_progress", project: "alpha" }, { id: "w1", title: "Loose and growing", state: "in_progress", project: "alpha", updated: 1792000000 }] }, {});
var T = C.tree(S, home, { now: NOW });

/* ---------- 1. the tree ---------- */
function checkTree(t, world, label) {
  assert.deepStrictEqual(t.dropped, [], label + ": nothing dropped");
  var seen = {};
  Object.keys(t.nodes).forEach(function (id) {
    var n = t.nodes[id];
    if (n.cells) {
      var inGrid = {};
      n.cells.forEach(function (row, r) { row.forEach(function (cid, c) { if (!cid) return; var k = t.nodes[cid]; assert.ok(k, label + ": cell names a node " + cid); assert.strictEqual(k.parent, id, label + ": parent " + cid); assert.strictEqual(k.r, r); if (!k.span) assert.strictEqual(k.c, c, label + ": coordinate " + cid); inGrid[cid] = 1; }); });
      assert.deepStrictEqual(Object.keys(inGrid).sort(), n.kids.slice().sort(), label + ": kids are the occupied cells of " + id);
      n.kids.forEach(function (cid, i) { if (i) { var a = t.nodes[n.kids[i - 1]], b = t.nodes[cid]; assert.ok(a.r < b.r || (a.r === b.r && a.c < b.c), label + ": reading order in " + id); } });
      n.kids.forEach(function (cid) { assert.ok(!seen[cid], label + ": in exactly one grid: " + cid); seen[cid] = 1; });
    }
  });
  Object.keys(t.nodes).forEach(function (id) { if (id !== "world") assert.ok(seen[id], label + ": every node sits in a grid: " + id); });
  /* every placed Thing of the layout is a node */
  world.biomes.forEach(function (b) {
    assert.ok(t.nodes[b.id]);
    b.plots.forEach(function (p) { assert.ok(t.nodes[p.id], label + ": plot " + p.id); p.beds.forEach(function (bd, j) { var bid = bd.id || p.id + "/other"; if (t.nodes[p.id + "/phases"] && j >= 8) return; assert.ok(t.nodes[bid], label + ": bed " + bid); bd.cells.forEach(function (c) { assert.ok(t.nodes[c.id], label + ": task " + c.id); }); }); });
    b.orchard.concat(b.meadow, b.nursery).forEach(function (it) { assert.ok(t.nodes[it.id], label + ": band item " + it.id); });
  });
  /* wherever the layout states a coordinate (2b.1: biome.cell, plot.at, bed.at, cell.at, the crates' at), the tree agrees */
  var agree = function (id, at, dr) { if (!at) return; var n = t.nodes[id]; assert.deepStrictEqual([n.c, n.r], [at.c, at.r + (dr || 0)], label + ": the layout's coordinate for " + id); stated++; };
  var stated = 0;
  world.biomes.forEach(function (b) {
    agree(b.id, b.cell);
    b.plots.forEach(function (p) {
      agree(p.id, p.at, C.ROW.plots); if (p.crate) agree(p.crate.id, p.crate.at);
      p.beds.forEach(function (bd) { agree(bd.id, bd.at); if (bd.crate) agree(bd.crate.id, bd.crate.at); bd.cells.forEach(function (c) { agree(c.id, c.at); }); });
    });
  });
  report[label + "_stated"] = stated;
  return Object.keys(seen).length;
}
checkTree(T, S, "spec");
assert.deepStrictEqual(T.root.cells[0], [null, "project:", "project:alpha", "project:beta", null], "lattice: each biome at its memo'd cell");
assert.strictEqual(T.root.cells[1][2], "project:gamma", "cell 7 is (2, 1)");
/* the home cell (29.6): every biome's, a stop even when empty; its zones laid out as the courtyard's quarters, an empty one no
   stop (N-D3); the Escalations cell beside the Mailbox holds the project's escalations, the Mailbox its decisions (28.13) */
assert.deepStrictEqual(T.nodes["home:alpha"].kids, ["home:alpha/escalation", "home:alpha/mailbox", "home:alpha/growing"], "alpha's zones in reading order: the empty Barn and Pen are no stops");
assert.deepStrictEqual(["escalation", "mailbox", "growing"].map(function (z) { var n = T.nodes["home:alpha/" + z]; return [n.c, n.r]; }), [[2, 0], [3, 0], [3, 1]], "the north-east quarter's two cells on the top row, the Growing field under them");
assert.strictEqual(T.nodes["home:alpha/escalation"].title, "Escalations");
assert.strictEqual(T.nodes["home:alpha/mailbox"].status, "1 decision waiting", "the ledger's escalation d2 is not a letter");
assert.ok(!T.nodes.d2, "and is no node: the farm's escalations list is the cell's");
assert.strictEqual(T.nodes["home:alpha/escalation"].status, "2 escalations · 1 needs you");
assert.deepStrictEqual(T.nodes["home:alpha/escalation"].kids, ["esc:e1", "esc:e2"], "ESCKIND's order: the one waiting on you first");
assert.deepStrictEqual([T.nodes["esc:e1"].kind, T.nodes["esc:e1"].open, T.nodes["esc:e1"].c, T.nodes["esc:e1"].r, T.nodes["esc:e2"].c], ["escalation", "e1", 0, 0, 1], "opens its bead; slots in reading order");
assert.strictEqual(T.nodes["esc:e1"].status, "waiting on you · since 2 h");
assert.strictEqual(T.nodes["esc:e2"].status, "approved, running · since 10 min");
assert.strictEqual(T.nodes["home:alpha/growing"].status, "4 growing");
assert.strictEqual(T.nodes["home:alpha"].status, "2 need you · 1 letter · 2 escalations · 4 growing", "the need words first");
assert.strictEqual(T.nodes["home:alpha"].needs, 2, "the decision and the pending escalation");
assert.deepStrictEqual(T.nodes["home:alpha/growing"].kids, ["alpha-meadow0", "alpha-meadow1", "alpha-meadow2", "w1"], "the loose beads, then the growing ones, in slot order");
assert.deepStrictEqual([T.nodes.w1.c, T.nodes.w1.r], [3, 0], "a plant's coordinate is its slot's place");
assert.strictEqual(T.nodes.w1.status, "growing · 16m"); assert.strictEqual(T.nodes.d1.status, "decision waiting on you");
assert.strictEqual(T.nodes["a0-0-1"].parent, "a0-ph0", "a plan's task growing on the ledger stays on its plot");
assert.deepStrictEqual(T.nodes["home:beta"].kids, [], "beta's strip holds nothing");
assert.strictEqual(T.nodes["home:beta"].status, "No letters · nobody in the field · nothing growing", "and says so");
/* coordinates match the rects exactly in the spec geometry */
var al = T.nodes["project:alpha"];
assert.deepStrictEqual([T.nodes["home:alpha"].c, T.nodes["home:alpha"].r, T.nodes["home:alpha"].span], [3, C.ROW.plots + 2, false], "slot 0, the home cell, is the centre of the 7x5 (29.6)");
assert.deepStrictEqual([T.nodes.a0.c, T.nodes.a0.r], [4, C.ROW.plots + 2], "slot 1 is east of it");
assert.deepStrictEqual([T.nodes.a1.c, T.nodes.a1.r], [2, C.ROW.plots + 1], "slot 6 is up-left of it (ring 1 runs clockwise from the east, 29.1)");
Object.keys(T.nodes).forEach(function (id) {
  var n = T.nodes[id];
  if (!n.cells || n.kind === "world" || n.kind === "band" || n.kind === "zone" || n.kind === "home") return;
  var kids = n.kids.map(function (k) { return T.nodes[k]; }).filter(function (k) { return !k.span; });
  kids.forEach(function (a) { kids.forEach(function (b) {
    if (a.r === b.r && a.c < b.c) assert.ok(a.rect.x < b.rect.x, "column order is x order in " + id);
    if (a.c === b.c && a.r < b.r) assert.ok(a.rect.y < b.rect.y, "row order is y order in " + id);
  }); });
});
var a0 = S.index.a0, bed0 = a0.beds[0];
a0.beds.forEach(function (bd, i) { var n = T.nodes[bd.id]; assert.deepStrictEqual([n.c, n.r], [i % 3, Math.floor(i / 3)]); assert.strictEqual(n.ordinal, i + 1); });
assert.strictEqual(T.nodes["a0/other"].kind, "bed", "Other is the bed after the phases"); assert.strictEqual(T.nodes["a0/other"].open, "a0", "Other has no bead of its own: it opens the plan");
assert.deepStrictEqual([T.nodes["a0/other"].c, T.nodes["a0/other"].r], [1, 2], "phases 1-7 then Other in cell 8");
bed0.cells.forEach(function (c, j) { var n = T.nodes[c.id], at = C.blockCell(j); assert.deepStrictEqual([n.c, n.r], [at.c, at.r]); assert.deepStrictEqual([c.x - bed0.x, c.y - bed0.y], [n.c, n.r], "task coordinate is its tile in the bed"); });
assert.deepStrictEqual([0, 8, 9, 26, 27, 80].map(function (i) { var b = C.blockCell(i); return [b.c, b.r]; }), [[0, 0], [2, 2], [3, 0], [8, 2], [0, 3], [8, 8]], "block-major order");
var big = T.nodes["a0-ph5"];
assert.strictEqual(big.kids.length, 61, "60 tasks and a crate fit in a 9x9 bed");
assert.strictEqual(T.nodes["a0-ph5/more"].kind, "crate"); assert.strictEqual(T.nodes["a0-ph5/more"].open, "a0-ph5", "the crate opens the phase");
assert.strictEqual(T.nodes["a1/phases"].kind, "crate"); assert.deepStrictEqual([T.nodes["a1/phases"].c, T.nodes["a1/phases"].r], [2, 2], "past 9 phases: the crate is cell 9");
assert.strictEqual(T.nodes.a1.kids.length, 9); assert.strictEqual(T.nodes["a1/phases"].title, "+3 phases");
["orchard", "nursery"].forEach(function (k) { var b = T.nodes["band:alpha/" + k]; assert.ok(b.span); assert.strictEqual(b.r, C.ROW[k]); assert.strictEqual(al.cells[C.ROW[k]].filter(function (x) { return x === b.id; }).length, al.cols, "a band spans the grid"); });
assert.strictEqual(T.nodes["overflow:alpha"].r, C.ROW.crate); assert.strictEqual(T.nodes["overflow:alpha"].drawer, "alpha");
assert.ok(!T.nodes["band:beta/orchard"], "a band with no items is not a cell"); assert.ok(T.nodes["home:beta"] && !T.nodes["home:beta"].span, "a home cell is, always, at the centre (H-D5)");
/* status lines (section 6.4): zero parts are left out */
assert.strictEqual(al.status, "7 plans · 1 needs you");
assert.strictEqual(T.nodes.a0.status, "phase 2 of 7 · 4/14 steps · 1 needs you", "the current phase is the first not closed");
assert.strictEqual(T.nodes["a0-ph1"].status, "2/15 steps · 1 blocked");
assert.strictEqual(T.nodes["a0-0-1"].status, "growing · 16m");
assert.strictEqual(T.nodes["project:beta"].status, "3 plans");

/* ---------- 2. move (section 3.2) ---------- */
function walk(t, p, dirs) { var out = [], col; dirs.forEach(function (d) { var m = C.move(t, p, d, col); out.push(last(m.path) + (m.bumped ? "!" : "")); p = m.path; col = m.col; }); return out; }
var pa0 = C.pathFor(T, "a0");
assert.deepStrictEqual(pa0, ["project:alpha", "a0"]);
/* alpha's row 2 up: a1 (2,3) a2 (4,3); home (3,4) between a8 (2,4) and a0 (4,4); a5 (4,5). Right from a1 skips the fallow slot
   above the farm to a2, the home cell is a stop like a plot (29.6), and the bands (spanning rows) are never a sideways stop */
assert.deepStrictEqual(walk(T, ["project:alpha", "a1"], ["right", "right", "down", "left", "left", "left"]), ["a2", "a2!", "a0", "home:alpha", "a8", "a8!"]);
assert.deepStrictEqual(walk(C.tree(S, home, { now: NOW }), ["project:alpha", "a1"], ["right", "right", "down", "left", "left", "left"]), ["a2", "a2!", "a0", "home:alpha", "a8", "a8!"], "the same farm gives the same walk");
/* up from a top-row plot lands on the band above; down from it returns to the column the cursor came from */
var up = C.move(T, ["project:alpha", "a2"], "up"); assert.strictEqual(last(up.path), "band:alpha/orchard"); assert.strictEqual(up.col, 4);
assert.strictEqual(C.move(T, up.path, "up", up.col).bumped, true, "the Orchard is the top row: a second Up bumps (no home row above it, 29.6)");
var bTop = T.nodes["project:beta"].kids.filter(function (id) { return T.nodes[id].kind === "plot"; }).sort(function (a, b) { return T.nodes[a].r - T.nodes[b].r; })[0];
assert.strictEqual(C.move(T, ["project:beta", bTop], "up").bumped, true, "no Orchard or Graveyard: Up from the top plot row bumps");
/* the arrows onto the home cell and back (29.6, X-D10): Left from the plot east of the farm is the farm, and so on round it */
var XW = synth([{ key: "cross", k: 2, plans: [{ id: "xe", slot: 1, phases: [1] }, { id: "xs", slot: 3, phases: [1] }, { id: "xw", slot: 5, phases: [1] }, { id: "xn", slot: 7, phases: [1] }] }]), TX = C.tree(XW, null, { now: NOW });
checkTree(TX, XW, "cross");
[["xe", "left", "right"], ["xw", "right", "left"], ["xs", "up", "down"], ["xn", "down", "up"]].forEach(function (w) {
  assert.deepStrictEqual(walk(TX, ["project:cross", w[0]], [w[1], w[2]]), ["home:cross", w[0]], w[1] + " from " + w[0] + " is the home cell, and " + w[2] + " comes back");
});
assert.strictEqual(last(C.move(T, up.path, "down", up.col).path), "a2", "back down the column it came from");
assert.strictEqual(last(C.move(T, ["project:alpha", "band:alpha/orchard"], "down", 2).path), "a1");
assert.strictEqual(last(C.move(T, ["project:alpha", "a5"], "down").path), "band:alpha/nursery");
assert.strictEqual(last(C.move(T, ["project:alpha", "band:alpha/nursery"], "down").path), "overflow:alpha");
assert.strictEqual(C.move(T, ["project:alpha", "overflow:alpha"], "down").bumped, true, "a bump at the edge");
/* a dense bed is one cell per press, along block-major rows */
var pb = ["project:alpha", "a0", "a0-ph5", bed0 && "a0-5-0"];
assert.deepStrictEqual(walk(T, pb, ["right", "right", "right", "right"]), ["a0-5-1", "a0-5-2", "a0-5-9", "a0-5-10"], "one cell a press, across a block edge");
assert.deepStrictEqual(walk(T, pb, ["down", "down", "down"]), ["a0-5-3", "a0-5-6", "a0-5-27"]);
assert.strictEqual(C.move(T, pb, "left").bumped, true); assert.strictEqual(C.move(T, pb, "up").bumped, true);
/* skipping empties: a 5-task bed (blocks of 3x3) */
var p5 = ["project:alpha", "a0", "a0-ph0", "a0-0-4"];
assert.deepStrictEqual(walk(T, p5, ["up", "left", "left", "right"]), ["a0-0-1", "a0-0-0", "a0-0-0!", "a0-0-1"]);
assert.strictEqual(C.move(T, ["project:alpha", "a0", "a0-ph0", "a0-0-3"], "down").bumped, true, "nothing below task 4 of 5 (task 5 is up-right of it)");
assert.strictEqual(last(C.move(T, ["project:alpha", "a0", "a0-ph0", "a0-0-3"], "right").path), "a0-0-4");
assert.strictEqual(last(C.move(T, ["project:alpha", "a0", "a0-ph0", "a0-0-2"], "down").path), "a0-0-4", "down from (2,0) skips to the nearest occupied below, (1,1)");
/* depth 0: the lattice, skipping the empty cell 4; down from beta (3,0) goes to the nearest below, gamma (2,1) */
assert.deepStrictEqual(walk(T, ["project:"], ["right", "right", "right", "down", "left"]), ["project:alpha", "project:beta", "project:beta!", "project:gamma", "project:"]);

/* ---------- 3. jump (section 3.3) ---------- */
function J(p, d) { var j = C.jump(T, p, d); return (j.bumped ? "!" : "") + ids(j.path); }
/* the spec's example: task (2,1) of phase 4 -> task (2,1) of phase 5 */
assert.strictEqual(J(["project:alpha", "a1", "a1-ph3", T.nodes["a1-ph3"].cells[1][2]], "right"), ids(["project:alpha", "a1", "a1-ph4", T.nodes["a1-ph4"].cells[1][2]]), "same coordinate in the next bed");
assert.strictEqual(T.nodes["a1-ph4"].cells[1][2], "a1-4-5");
/* phase 5 of a0 holds 4 tasks, so (2,1) is empty there: the nearest occupied (Manhattan, reading order), (2,0) */
assert.strictEqual(J(["project:alpha", "a0", "a0-ph3", T.nodes["a0-ph3"].cells[1][2]], "right"), ids(["project:alpha", "a0", "a0-ph4", "a0-4-2"]));
assert.strictEqual(J(["project:alpha", "a0", "a0-ph0", "a0-0-0"], "right"), ids(["project:alpha", "a0", "a0-ph1", "a0-1-0"]));
assert.strictEqual(J(["project:alpha", "a0", "a0-ph1", "a0-1-11"], "left"), ids(["project:alpha", "a0", "a0-ph0", "a0-0-2"]), "(5,0) is outside the smaller bed: clamped to its used bounds, (2,0)");
/* bubbling: phase 3 (right column) of a1 (2,3) -> the next plot to the right in its row, a2 (4,3); a8's one bed -> past the home
   cell, which is no plot, to a0 */
assert.strictEqual(J(["project:alpha", "a1", "a1-ph2"], "right"), ids(["project:alpha", "a2", "a2-ph2"]), "a bed at its plot's edge bubbles to the next plot, onto the same bed coordinate");
assert.strictEqual(J(["project:alpha", "a8", "a8-ph0"], "right"), ids(["project:alpha", "a0", "a0-ph0"]), "a sibling jump steps over the farm");
/* across projects: alpha's rightmost plot a2 -> beta's plot at the same slot coordinate or the nearest */
var j1 = C.jump(T, ["project:alpha", "a2", "a2-ph2"], "right");
assert.deepStrictEqual(j1.path.slice(0, 1), ["project:beta"], "past the rightmost plot: into the next project in the lattice");
assert.strictEqual(T.nodes[j1.path[1]].kind, "plot"); assert.strictEqual(j1.path.length, 3);
assert.strictEqual(J(["project:beta", "b4", "b4-ph2", "b4-2-0"], "right"), "!" + ids(["project:beta", "b4", "b4-ph2", "b4-2-0"]), "beta is the east edge of the lattice: the step bubbles to the world and bumps");
assert.strictEqual(C.jump(T, ["project:alpha"], "right").bumped, true, "depth 0 has no sibling grid");
var j2 = C.jump(T, ["project:alpha", "a1", "a1-ph0", "a1-0-0"], "down");
assert.strictEqual(j2.path[2], "a1-ph3", "Shift+Down: the bed below, same task coordinate"); assert.strictEqual(j2.path[3], "a1-3-0");

/* ---------- 4. descend and ascend (section 3.2, N-D4) ---------- */
assert.deepStrictEqual(C.descend(T, ["project:alpha"], {}, []).path, ["project:alpha", "a1"], "no memory, nothing needs you: the first plot in reading order");
assert.deepStrictEqual(C.descend(T, ["project:alpha"], {}, ["b0", "a5"]).path, ["project:alpha", "a5"], "else the first child that needs you, in beacon order");
assert.deepStrictEqual(C.descend(T, ["project:alpha"], { "project:alpha": "a8" }, ["a5"]).path, ["project:alpha", "a8"], "memory wins");
assert.deepStrictEqual(C.descend(T, ["project:alpha"], { "project:alpha": "gone" }, []).path, ["project:alpha", "a1"], "a stale memory is ignored");
assert.deepStrictEqual(C.descend(T, ["project:alpha", "a0"], {}, ["a0-3-2"]).path, ["project:alpha", "a0", "a0-ph3"], "a need inside a bed picks the bed");
assert.deepStrictEqual(C.descend(T, ["project:alpha", "home:alpha"], {}, ["w1"]).path, ["project:alpha", "home:alpha", "home:alpha/growing"], "into a home cell: the zone that holds what needs you");
assert.deepStrictEqual(C.descend(T, ["project:alpha", "home:alpha"], {}, []).path, ["project:alpha", "home:alpha", "home:alpha/escalation"], "else the first in reading order: the top row west to east, the Barn empty");
var leaf = C.descend(T, ["project:alpha", "a0", "a0-ph0", "a0-0-0"], {}, []);
assert.strictEqual(leaf.open, "a0-0-0"); assert.strictEqual(leaf.bumped, false); assert.strictEqual(leaf.path.length, 4, "a leaf opens its bead and the cursor stays");
assert.strictEqual(C.descend(T, ["project:alpha", "a1", "a1/phases"], {}, []).open, "a1", "the phases crate opens the plan");
assert.strictEqual(C.descend(T, ["project:alpha", "overflow:alpha"], {}, []).drawer, "alpha", "the plans crate opens the Plans drawer");
assert.strictEqual(C.descend(T, ["project:beta", "home:beta"], {}, []).bumped, true, "Enter on an empty strip bumps (28.6)");
assert.deepStrictEqual(C.ascend(T, ["project:alpha", "a0"]), { path: ["project:alpha"], bumped: false });
assert.strictEqual(C.ascend(T, ["project:alpha"]).bumped, true);

/* ---------- 5. frame (section 4.1) ---------- */
var D = { w: 1280, h: 800 }, P = { w: 390, h: 520, coarse: true }, X = { w: 1920, h: 1080 };
function onScreen(f, r, vp) { return { x: (r.x * 16 - f.cx) * f.z + vp.w / 2, y: (r.y * 16 - f.cy) * f.z + vp.h / 2, w: r.w * 16 * f.z, h: r.h * 16 * f.z }; }
var five = ["project:alpha", "a0", "a0-ph0", "a0-0-0"], f5 = C.frame(T, five, P);
assert.deepStrictEqual(f5.bounds, W.tiles({ x: bed0.x, y: bed0.y, w: 3, h: 3 }), "a 5-task phase frames one block");
assert.strictEqual(f5.z, 4, "and frames it at z 4 on a phone"); assert.strictEqual(f5.window, null);
var full = ["project:alpha", "a0", "a0-ph5", "a0-5-0"], ff = C.frame(T, full, P);
assert.strictEqual(ff.z, 2, "a full 9x9 bed fits a 390 px phone whole at z 2"); assert.strictEqual(ff.window, null);
assert.strictEqual(C.frame(T, full, D).z, 4); assert.strictEqual(C.frame(T, full, X).z, 4);
[D, P, X].forEach(function (vp) {
  [five, full, ["project:alpha", "a0", "a0-ph0"], ["project:alpha", "a0"], ["project:alpha"], ["project:alpha", "home:alpha", "home:alpha/mailbox"]].forEach(function (p) {
    var f = C.frame(T, p, vp), b = onScreen(f, { x: f.bounds.x / 16, y: f.bounds.y / 16, w: f.bounds.w / 16, h: f.bounds.h / 16 }, vp);
    if (f.z >= 1) assert.ok(W.SNAPS.indexOf(f.z) >= 0, "the zoom rests on a snap: " + f.z);
    if (!f.window) assert.ok(b.x >= -0.5 && b.y >= -0.5 && b.x + b.w <= vp.w + 0.5 && b.y + b.h <= vp.h + 0.5, "the used bounds are in view: " + ids(p) + " " + JSON.stringify(b));
  });
});
var fp = C.frame(T, ["project:alpha", "a0"], P), plotPx = 32 * 16 * fp.z;
assert.ok(!fp.window && plotPx >= 32, "the whole project fits a phone, plots at least 32 px: " + plotPx.toFixed(1));
/* insets: the framing centres in the board minus the HUD */
var fi = C.frame(T, five, { w: 1280, h: 800, top: 100 }), bi = onScreen(fi, { x: bed0.x, y: bed0.y, w: 3, h: 3 }, D);
assert.ok(Math.abs(bi.y + bi.h / 2 - (100 + 350)) < 0.5, "centred under the header inset");
/* the window: a coarse 200 px board cannot hold a 9x9 bed at 32 px cells, so it frames whole blocks and scrolls only at its edge */
var tiny = { w: 200, h: 200, coarse: true }, w0 = C.frame(T, full, tiny);
assert.ok(w0.window, "a window"); assert.strictEqual(w0.z, 2); assert.deepStrictEqual([w0.window.c0, w0.window.r0, w0.window.cols, w0.window.rows], [0, 0, 3, 3]);
var p1 = ["project:alpha", "a0", "a0-ph5", "a0-5-4"], w1 = C.frame(T, p1, tiny, { prev: w0.window });
assert.deepStrictEqual(w1, w0, "a move inside the window moves no camera");
var p2 = ["project:alpha", "a0", "a0-ph5", "a0-5-9"], w2 = C.frame(T, p2, tiny, { prev: w0.window });
assert.deepStrictEqual([w2.window.c0, w2.window.r0], [3, 0], "leaving the window scrolls it by one block"); assert.ok(w2.cx > w0.cx);
var mid = { w: 300, h: 300, coarse: true }, m0 = C.frame(T, full, mid);
assert.deepStrictEqual([m0.window.cols, m0.window.rows], [6, 6], "two blocks a side");
var mp = ["project:alpha", "a0", "a0-ph5", "a0-5-10"], m1 = C.frame(T, mp, mid, { prev: m0.window });
assert.deepStrictEqual(m1.window, m0.window, "inside the window, no scroll");
var m2 = C.frame(T, ["project:alpha", "a0", "a0-ph5", "a0-5-18"], mid, { prev: m0.window });
assert.strictEqual(m2.window.c0, 3, "within one block of the far edge (and room beyond): scroll a block");
/* a beacon is one more inset (gd-SWjiYouIQt): on a phone the plan's beds fill the board edge to edge, so the Mailbox's arrow on
   the left edge would sit on them; its band, fed back as a left inset (garden.js fitClear), keeps every bed clear of it */
var ph = { w: 390, h: 844, top: 96, right: 56, bottom: 120, coarse: true }, pp = ["project:alpha", "a0", "a0-ph0"], bo = { size: B.SIZE_COARSE, top: ph.top };
var mb = [{ id: "home:/mailbox", kind: "mailbox", name: "the Mailbox in the Commons", count: 1, glyph: "?", rect: W.tiles(T.nodes["home:"].rect) }];
function clear(f, arrows) { return T.nodes.a0.kids.every(function (id) { var r = onScreen(f, T.nodes[id].rect, ph); return arrows.every(function (a) { var h = a.size / 2; return a.x + h <= r.x || a.x - h >= r.x + r.w || a.y + h <= r.y || a.y - h >= r.y + r.h; }); }); }
var beds = T.nodes.a0.kids.map(function (id) { return W.tiles(T.nodes[id].rect); });
var pf = C.frame(T, pp, ph), pa = B.layout(mb, pf, ph, bo), band = B.inset(pa, pf, ph, beds);
assert.strictEqual(pa.length, 1); assert.strictEqual(pa[0].dir, "west", "the Commons's strip is west of the plan");
assert.ok(!clear(pf, pa), "without the band the arrow sits on a bed");
assert.deepStrictEqual(band, { top: 0, right: 0, bottom: 0, left: B.MARGIN + B.SIZE_COARSE + B.GAP }, "the band runs to the arrow's far side and a gap");
var pf2 = C.frame(T, pp, Object.assign({}, ph, { left: band.left, zmin: Math.min(pf.z, W.levelRange(W.lodFor(pf.z))[0]) })), pa2 = B.layout(mb, pf2, ph, bo);
assert.strictEqual(W.lodFor(pf2.z), W.lodFor(pf.z), "the refit keeps the level of detail");
assert.ok(clear(pf2, pa2), "with it no bed is under the arrow");
assert.ok(onScreen(pf2, T.nodes["a0-ph0"].rect, ph).x >= band.left, "the cursor's bed starts right of the band");
assert.strictEqual(B.inset(pa2, pf2, ph, beds), null, "the refit needs no second band");
assert.strictEqual(B.inset([], pf, ph, beds), null, "no arrow, no band");
var zf = C.frame(T, pp, Object.assign({}, ph, { left: 120 })).z;
assert.ok(zf < 0.5 && C.frame(T, pp, Object.assign({}, ph, { left: 120, zmin: 0.5 })).z === 0.5, "zmin: the fit does not rest below it (" + zf + ")");
/* the cursor in the farmstead (gd-bkePy06Ont): a home cell's zones are not a lattice, so a view that cannot hold the farmstead at
   z 1 frames the cursor's own zone, fitted, and its 8 field stands (every running agent, 28.12) stay in view at 390, 820 and
   1280; 1280 holds the whole farmstead at z 1. The stands are in the Growing field, in the columns they are drawn in, before the
   loose work; the Barn holds no figure, so the release truck is its only item, with its words last (28.10, 28.12, H-D16) */
var SY = JSON.parse(JSON.stringify(S)), figs = [], wk = [], ltr = [];
for (var fi = 0; fi < 8; fi++) figs.push({ id: W.FIELD + "agent" + fi, kind: "stand", title: "agent " + fi, state: "working", project: "alpha", task: "", plot: fi === 7 ? "a0" : "", where: fi === 7 ? "On Plan a0" : undefined });
for (fi = 0; fi < 5; fi++) wk.push({ id: "gw" + fi, title: "growing " + fi, state: "in_progress", project: "alpha" });
for (fi = 0; fi < 3; fi++) ltr.push({ id: "gl" + fi, title: "letter " + fi, type: "decision", project: "alpha" });
var REL = { tag: "2026.305.3", id: "gd-rel1", status: "cut", shipping: true, targets: [] };
var truck = function (rel) { return [{ id: "release:" + rel.tag, key: "alpha", tag: rel.tag, rel: rel, bay: SY.biomes[0].home.bay }]; };
var ya = SY.biomes[0]; ya.pen = ya.home.zones.pen = W.penOf({ open: [{ id: "wish1", title: "A wish", project: "alpha", opened: NOW / 1000 - 60 }] }, "alpha", ya.home.cells.pen);
var homeY = W.homeFill(SY, { decision: ltr, work: wk, agent: figs }, {}), TY = C.tree(SY, homeY, { now: NOW, trucks: truck(REL) });
checkTree(TY, SY, "barn");
/* the arrows inside the home cell move as the courtyard is drawn (29.6): Right from the Barn is the Escalations, then the
   Mailbox; Down from either the Growing field, and Up from it the Mailbox; Down from the Barn the Pen, Right from it the field.
   With no Mailbox, Up from the field is the Escalations, not the Barn */
var hz = function (z) { return ["project:alpha", "home:alpha", "home:alpha/" + z]; }, zw = function (t, z, dirs) { return walk(t, hz(z), dirs).map(function (id) { return id.replace("home:alpha/", ""); }); };
assert.deepStrictEqual(zw(TY, "barn", ["right", "down", "up", "left", "left", "down", "right", "right", "left", "up"]), ["escalation", "growing", "mailbox", "escalation", "barn", "pen", "growing", "growing!", "pen", "barn"]);
var TN = C.tree(SY, W.homeFill(SY, { decision: [], work: wk, agent: figs }, {}), { now: NOW });
assert.ok(!TN.nodes["home:alpha/mailbox"], "no letters: no Mailbox stop (N-D3)");
assert.deepStrictEqual([zw(TN, "growing", ["up"]), zw(TN, "escalation", ["down"])], [["escalation"], ["growing"]], "the field is under the north-east quarter");
assert.ok(!TN.nodes["home:alpha/barn"], "no truck: no Barn stop, its gardeners stand in the field (28.12)");
var BZ = ["project:alpha", "home:alpha", "home:alpha/barn"], GZ = ["project:alpha", "home:alpha", "home:alpha/growing"], bz = TY.nodes[BZ[2]];
var gz = TY.nodes["home:alpha/growing"];
var nm = SY.biomes[0].meadow.length, s7 = TY.nodes[W.FIELD + "agent7"];
var gwAt = function (id) { return TY.nodes[id].c + 14 * TY.nodes[id].r; };   // a column per tile: the loose beads two to a pair slot
assert.deepStrictEqual([gz.cols, gz.rows, gwAt("gw0"), gwAt("gw4")], [14, 7, nm & 1 ? nm : 2 * (nm / 2 + 8), 2 * (Math.ceil(nm / 2) + 8) + (nm & 1 ? 3 : 4)], "the 16-tile field's grid: a column per tile, 14 x 7 (28.12, 29.2), the stands before the work, which takes the Meadow's spare half first");
assert.deepStrictEqual([s7.c + 14 * s7.r, s7.status], [2 * (Math.ceil(nm / 2) + 7), "On Plan a0 · working"], "the stand of one out on a plot, after the Meadow's flowers");
assert.deepStrictEqual(bz.kids, ["release:2026.305.3"], "the truck is the Barn's only item");
assert.strictEqual(C.descend(TY, BZ.concat("release:2026.305.3"), {}, []).open, "release:2026.305.3", "Enter opens it");
assert.strictEqual(bz.status, "release 2026.305.3 loading", "the Barn's line is the truck's words");
assert.strictEqual(TY.nodes["home:alpha"].status, "4 need you · 3 letters · 2 escalations · 8 working · " + (nm + 5) + " growing · 1 open wish", "the home cell's counts the stands apart and does not name the truck");
assert.strictEqual(gz.status, "8 working · " + (nm + 5) + " growing", "the field's line: its stands, then its loose beads (28.12)");
assert.strictEqual(C.atFor(TY, GZ.concat(W.FIELD + "agent7")), "alpha/growing", "a field figure's at= is its field's, as WORLD.atFor says (28.12)");
assert.deepStrictEqual(C.pathFor(TY, "alpha/growing"), GZ, "and it reads back to the field");
assert.strictEqual(C.tree(SY, homeY, { now: NOW, trucks: truck(Object.assign({}, REL, { status: "live", shipping: false, targets: [{ name: "prod", tag: REL.tag }] })) }).nodes[BZ[2]].status, "live on prod");
assert.strictEqual(C.tree(SY, homeY, { now: NOW }).nodes["release:2026.305.3"], undefined, "no release: no truck");
var phone = { w: 390, h: 844, top: 96, right: 56, bottom: 120, coarse: true }, midT = { w: 820, h: 1180, top: 60, coarse: true }, deskT = { w: 1280, h: 800, top: 60 };
[phone, midT, deskT].forEach(function (vp) {
  [BZ, ["project:alpha", "home:alpha", "home:alpha/mailbox"], ["project:alpha", "home:alpha", "home:alpha/growing"], GZ.concat(W.FIELD + "agent5"), GZ.concat(W.FIELD + "agent7"), BZ.concat("release:2026.305.3")].forEach(function (p) {
    var f = C.frame(TY, p, vp), r = C.screenRect(TY.nodes[last(p)].rect, f, vp);
    assert.ok(C.within(r, C.insets(vp)), vp.w + " px: the cursor's " + last(p) + " is in view: " + JSON.stringify(r));
  });
});
var fb = C.frame(TY, BZ, phone);
assert.deepStrictEqual(fb.window, { grid: "home:alpha", c0: 0, r0: 0, cols: 1, rows: 1 }, "a phone frames the Barn, not a window of the farmstead's zones");
assert.strictEqual(fb.z, 1, "fitted: the 15-tile Barn in 334 px (29.2)");
var fd = C.frame(TY, BZ, deskT); assert.ok(fd.window === null && fd.z >= 1, "1280 px frames the whole farmstead at z " + fd.z);
assert.deepStrictEqual([0, 1, 2, 7].map(function (i) { var n = TY.nodes[W.FIELD + "agent" + i]; return [n.c, n.r]; }), [0, 1, 2, 7].map(function (i) { var k = Math.ceil(nm / 2) + i; return [2 * (k % 7), Math.floor(k / 7)]; }), "a stand's column is the west tile of the pair slot it is drawn in, after the Meadow's flowers");
/* and everywhere: the cursor's own Thing is in view at every framing, so a band's seed past a window's edge, or a project past
   the world's, is panned in (each was off the screen) */
[[T, "synth"], [tf0(), "fixture"]].forEach(function (e) {
  Object.keys(e[0].nodes).forEach(function (id) {
    if (id === "world" || !e[0].nodes[id].rect) return;
    var p = C.pathFor(e[0], id);
    [D, P, X, phone].forEach(function (vp) { var f = C.frame(e[0], p, vp), r = C.screenRect(e[0].nodes[id].rect, f, vp); assert.ok(C.within(r, C.insets(vp)), e[1] + " " + vp.w + " px: " + id + " is in view: " + JSON.stringify(r)); });
  });
});
function tf0() { return C.tree(W.layout(fixture, null, { now: NOW, readonly: true }), null, { now: NOW }); }
report.frame = { phone_project_plot_px: +plotPx.toFixed(1), five_z: f5.z, full_phone_z: ff.z, phone_barn_z: fb.z };

/* ---------- 6. signs (section 6) ---------- */
/* the cursor's Thing has no label (gd-zDdiA3HBh5: its lineage, title and status line are the title bar's); its children a short
   label where they are wide enough, clear of each other, else a glyph; the rest of the grid glyph only */
[D, P, X, phone].forEach(function (vp) {
  [five, full, ["project:alpha", "a0", "a0-ph0"], ["project:alpha", "a0"], ["project:alpha"], ["project:alpha", "home:alpha"], ["project:alpha", "home:alpha", "home:alpha/mailbox"], ["project:alpha", "band:alpha/orchard"]].forEach(function (p) {
    var s = C.signs(T, p, vp), view = C.insets(vp);
    assert.deepStrictEqual(s.filter(function (x) { return x.tier !== "child" && x.tier !== "glyph"; }), [], "only child labels and glyphs: " + ids(p) + " " + vp.w);
    assert.ok(!s.some(function (x) { return x.id === last(p); }), "no sign for the cursor's own Thing: " + ids(p));
    var kids = s.filter(function (x) { return x.tier === "child"; });
    kids.forEach(function (k) { assert.ok(C.within(k.box, view), "a child label is in the view"); assert.ok(k.text.length <= 10, k.text); });
    kids.forEach(function (k, i) { kids.forEach(function (o, j) { if (i < j) assert.ok(!C.overlap(k.box, o.box), "child labels never overlap"); }); });
    var node = T.nodes[last(p)], g = C.gridOf(T, p);
    if (node.rect) { var nr = C.screenRect(node.rect, C.frame(T, p, vp), vp), inner = { x: nr.x + 2, y: nr.y + 2, w: nr.w - 4, h: nr.h - 4 }; kids.forEach(function (k) { assert.ok(C.within(k.box, inner), "a child label stays inside the cursor's Thing, clear of its ring (gd-dS4KEs0ohA): " + k.id + " " + vp.w); }); }
    s.forEach(function (x) { if (x.tier === "child") assert.strictEqual(T.nodes[x.id].parent, node.id, "child labels are the Thing's children"); });
    s.forEach(function (x) { if (T.nodes[x.id].parent === g.id && x.id !== node.id) assert.strictEqual(x.tier, "glyph", "siblings are glyph only at " + vp.w); });
    assert.ok(kids.length <= 35);
    if (node.kind === "bed") assert.strictEqual(kids.length, 0, "tasks are glyph only");
    assert.deepStrictEqual(C.signs(T, p, vp), s, "the same path and viewport give the same signs");
    assert.deepStrictEqual(C.signs(T, p, vp, C.frame(T, p, vp)), s, "placed against the framing, whatever camera is mid-ease");
  });
});
/* a child at the Thing's foot shorter than its label: the label is lifted inside the Thing, off its ring, not hung under it; one
   wider than the Thing is a glyph (gd-dS4KEs0ohA) */
(function () {
  var band = function (id, title, r) { return { id: id, kind: "band", title: title, parent: "P", rect: r, kids: [] }; };
  var ft = { root: { id: "world", kids: ["P"] }, nodes: { P: { id: "P", kind: "biome", parent: "world", rect: { x: 0, y: 0, w: 10, h: 10 }, cells: [1], kids: ["top", "foot"] },
    top: band("top", "Orchard", { x: 0, y: 0, w: 10, h: 1 }), foot: band("foot", "Nursery", { x: 0, y: 9.5, w: 10, h: 0.5 }) } };
  var fvp = { w: 1000, h: 1000 }, fcam = { cx: 0, cy: 0, z: 1 }, by = function (s, id) { return s.filter(function (x) { return x.id === id; })[0]; };
  var fs = C.signs(ft, ["P"], fvp, fcam), nr = C.screenRect(ft.nodes.P.rect, fcam, fvp), foot = by(fs, "foot");
  assert.strictEqual(foot.tier, "child", "the foot child keeps its label");
  assert.strictEqual(foot.box.y + foot.box.h, nr.y + nr.h - 2, "lifted to sit 2 px inside the Thing's foot");
  assert.strictEqual(by(fs, "top").box.y, nr.y + 2, "a child with room stays at its own top-left");
  ft.nodes.P.rect.w = 4; ft.nodes.top.rect.w = ft.nodes.foot.rect.w = 4; ft.nodes.top.title = "Greenhouse";
  assert.strictEqual(by(C.signs(ft, ["P"], fvp, fcam), "top").tier, "glyph", "a label wider than the Thing: a glyph");
})();
/* a zone of the home cell is in view at its framing, phone, mid and desktop (gd-lnc0xuboeg) */
[phone, midT, deskT].forEach(function (vp) {
  ["home:alpha/barn", "home:alpha/growing", "home:alpha/mailbox"].forEach(function (id) {
    var p = ["project:alpha", "home:alpha", id], fr = C.frame(TY, p, vp);
    assert.ok(C.within(C.screenRect(TY.nodes[id].rect, fr, vp), C.insets(vp)), vp.w + " px: " + id + " is in view");
  });
});
var vw = { x: 0, y: 0, w: 1280, h: 800 };
/* the World's header plates (gd-zceqmx40pD): every neighbouring project named */
var pj = { x: 200, y: 200, w: 300, h: 200 }, ab = { x: 0, y: 150, w: 1280, h: 48 };
assert.deepStrictEqual(C.plateBox(pj, 150, 60, 20, vw, [], []), { x: 200, y: 176, w: 150, h: 20, brief: false }, "nothing in the way: over the project, flush left");
assert.strictEqual(C.plateBox(pj, 150, 60, 20, vw, [ab], []).y, 202, "a sign over it: inside its top edge");
assert.strictEqual(C.plateBox(pj, 150, 60, 20, vw, [], [ab]).y, 202, "a project over it: inside its top edge");
assert.deepStrictEqual(C.plateBox({ x: 100, y: 200, w: 300, h: 200 }, 200, 60, 20, { x: 0, y: 0, w: 250, h: 800 }, [], []), { x: 100, y: 176, w: 60, h: 20, brief: true }, "the whole plate fits nowhere flush: its name alone");
assert.deepStrictEqual(C.plateBox({ x: -100, y: 300, w: 200, h: 100 }, 150, 120, 20, vw, [], []), { x: 4, y: 276, w: 150, h: 20, brief: false }, "off the map's edge: pushed into it, whole");
assert.strictEqual(C.plateBox(pj, 150, 60, 20, vw, [vw], []), null, "no room anywhere: none");
var hA = { id: "a", r: { x: 100, y: 300, w: 200, h: 150 }, w: 150, wt: 60, h: 20 }, hB = { id: "b", r: { x: 400, y: 300, w: 200, h: 150 }, w: 150, wt: 60, h: 20 }, pC = { id: "c", r: { x: 700, y: 300, w: 200, h: 150 } };
var ws0 = C.worldSigns([hA, hB], [hA, hB, pC], vw, []);
assert.deepStrictEqual([ws0.a.y, ws0.b.y], [276, 276], "a plate over each project");
assert.strictEqual(C.worldSigns([hA, hB], [hA, hB, pC], vw, [{ x: 50, y: 250, w: 300, h: 40 }]).a.y, 302, "a sign over a project: its plate inside its top edge");
var a0 = T.nodes.a0;
/* gatePlot is the plot whose arms the page draws at every level when the cursor is inside it, so a short map framing a plan at
   Project art still shows them (gd-rA31ZIfHdv); only arms the farm named */
var a0plan = a0.ref.plan;
assert.strictEqual(C.gatePlot(T, ["project:alpha", "a0", "a0-ph0"]), null, "no arms: none");
a0plan.heraldry = "blue \u{1F9ED} . \u{1F525} . \u{1F4E6}";
assert.strictEqual(C.gatePlot(T, ["project:alpha", "a0", "a0-ph0"]), null, "arms the farm could not name: none");
a0plan.heraldry_name = "arms on blue: compass top-left, fire centre, package bottom-right";
assert.strictEqual(C.gatePlot(T, ["project:alpha", "a0", "a0-ph0"]), a0, "at the Plan depth: the cursor's plot");
assert.strictEqual(C.gatePlot(T, five), a0, "at the Bead depth: the step's plot");
assert.strictEqual(C.gatePlot(T, ["project:alpha", "a0"]), null, "on the plot itself: none");
assert.strictEqual(C.gatePlot(T, []), null, "no cursor: none");
delete a0plan.heraldry; delete a0plan.heraldry_name;
var plan = C.signs(T, ["project:alpha", "a0"], X), beds = plan.filter(function (x) { return T.nodes[x.id].parent === "a0"; });
assert.strictEqual(beds.length, 8, "a plan's beds are all signed"); assert.ok(beds.every(function (x) { return x.glyph === String(T.nodes[x.id].ordinal); }), "a bed's glyph is its ordinal");
var bbeds = C.signs(T, ["project:beta", "b0"], X).filter(function (x) { return T.nodes[x.id].parent === "b0"; });
assert.ok(bbeds.length === 6 && bbeds.every(function (x) { return x.tier === "child" && x.text.length <= 10; }), "in a compact project at 1920 px every bed is wide enough for a short label");
assert.ok(beds.every(function (x) { return x.tier === "glyph"; }), "alpha's tall grid frames beds under 56 px: glyph only");
assert.ok(beds.every(function (x) { return x.pips >= 0 && x.pips <= 4; }), "progress pips in quarters");
var proj = C.signs(T, ["project:alpha", "a0"], D).filter(function (x) { return x.id === "a1"; })[0];
assert.strictEqual(proj.tier, "glyph", "a sibling plot is glyph only"); assert.strictEqual(proj.glyph, "", "no need, no mark");
assert.strictEqual(C.signs(T, ["project:alpha", "a1"], D).filter(function (x) { return x.id === "a0"; })[0].glyph, "!1", "a need mark");
/* fitGlyph: a cell's glyph stays inside the cell on screen (gd-G2rS3GKYve) */
assert.deepStrictEqual(C.fitGlyph("!50", 100, 200), { g: "!50", x: 102 }, "a wide cell: the whole glyph 2 px in");
assert.deepStrictEqual(C.fitGlyph("!50", 100, 130), { g: "!50", x: 102 }, "an exact fit keeps the count");
assert.deepStrictEqual(C.fitGlyph("!50", 100, 129), { g: "!", x: 102 }, "one px short: the mark alone");
assert.deepStrictEqual(C.fitGlyph("+12", 100, 120), { g: "+", x: 102 }, "a crate's count drops to its +");
assert.deepStrictEqual(C.fitGlyph("!1", 100, 108), { g: "!", x: 96 }, "narrower than the mark: flush with the east edge");
[["!50", 0, 9], ["!50", 0, 30], ["!1", 50, 70], ["+3", 7, 300]].forEach(function (c) {
  var f = C.fitGlyph(c[0], c[1], c[2]);
  assert.ok(f.x + C.glyphW(f.g) <= c[2], "never past the east edge: " + c.join(" "));
});
/* statusOf: the title bar's status line (§6.4), whole, the need words first */
assert.deepStrictEqual(C.statusOf({ status: "phase 1 of 3 · 4/9 steps · 2 need you · 3 gardeners" }), { status: "2 need you · phase 1 of 3 · 4/9 steps · 3 gardeners", icons: null }, "the need words lead");
assert.deepStrictEqual(C.statusOf({ status: "growing · @mo · 3m" }), { status: "growing · @mo · 3m", icons: null }, "no need words: as it is");
assert.deepStrictEqual(C.statusOf({ status: "" }), { status: "", icons: null }, "none: empty");
assert.deepStrictEqual(C.statusOf(null), { status: "", icons: null }, "no Thing: empty");
var longS = "3 plans · 1 needs you · 2 gardeners · 4 weeds · a status line far past any label's forty characters, whole";
assert.strictEqual(C.statusOf({ status: longS }).status, "1 needs you · 3 plans · 2 gardeners · 4 weeds · a status line far past any label's forty characters, whole", "never cut");

/* ---------- 6b. a harvest cell's status line and badges (harvest spec 5) ---------- */
var SH = JSON.parse(JSON.stringify(S)), hbed = SH.biomes[0].plots[0].beds[0], hn = hbed.cells.length, DAY = 86400;
function closedCell(id, i, h, who) { var at = C.blockCell(i); return { id: id, kind: "task", i: i, x: hbed.x + at.c, y: hbed.y + at.r, w: 1, h: 1, task: task(id, "closed", { assignee: who, harvest: h }), plot: "a0", phase: hbed.id }; }
hbed.cells.push(closedCell("h-full", hn, { closed: NOW / 1000 - 2 * DAY, base: "sheaf", mr: "#825", ux: [5, 5], pics: 9, tests: 3, ci: "success", rounds: 2 }, "mo"));
hbed.cells.push(closedCell("h-bare", hn + 1, { closed: NOW / 1000 - 3 * 3600, base: "sheaf", mr: "#9", ci: "failed" }));
hbed.cells.push(closedCell("h-plain", hn + 2, undefined, "jo"));
var TH = C.tree(SH, home, { now: NOW });
assert.strictEqual(TH.nodes["h-full"].status, "harvested · @mo · 2d ago · merged #825 · graded 5/5 gates · 3 tests · checks green · 9 pictures · merged after 2 rounds",
  "the status line: the base word, who, when, then every badge's words in priority order");
assert.strictEqual(TH.nodes["h-bare"].status, "harvested · 3h ago · checks failed · merged #9", "zero parts are left out (no who, no grade, tests, pictures or rounds); checks that did not go green say so");
assert.strictEqual(TH.nodes["h-plain"].status, "closed · @jo · 16m", "a closed row without harvest keeps its plain status");
assert.strictEqual(TH.nodes["h-plain"].icons, null);
var hf = C.statusOf(TH.nodes["h-full"]);
assert.strictEqual(hf.status, "harvested · @mo · 2d ago", "the bar's status is the lead; the badge words go with their icons");
assert.deepStrictEqual(hf.icons.map(function (b) { return b.k; }), ["crate", "rosette", "vial", "lantern", "photo", "seal"], "every badge, in priority order, none dropped");
assert.deepStrictEqual(C.statusOf(TH.nodes["h-bare"]), { status: "harvested · 3h ago · checks failed", icons: [{ k: "crate", w: "merged #9" }] }, "one badge");
assert.deepStrictEqual(C.statusOf(TH.nodes["h-plain"]), { status: "closed · @jo · 16m", icons: null }, "a plain closed row has no badges");

/* ---------- 7. follow (section 3.4) ---------- */
var S2 = JSON.parse(JSON.stringify(S));
/* a0-0-1 closes: a0-0-2 slides into its cell (the bed is re-laid in order) */
var bd = S2.index.a0.beds[0]; bd.cells.splice(1, 1); bd.cells.forEach(function (c, j) { var at = C.blockCell(j); c.i = j; c.x = bd.x + at.c; c.y = bd.y + at.r; });
S2.biomes[0].plots[0].beds[0] = bd;
var T2 = C.tree(S2, home, { now: NOW });
assert.deepStrictEqual(C.follow(T2, T, ["project:alpha", "a0", "a0-ph0", "a0-0-3"]), ["project:alpha", "a0", "a0-ph0", "a0-0-3"], "by id, though its cell moved");
assert.deepStrictEqual([T2.nodes["a0-0-3"].c, T2.nodes["a0-0-3"].r], [2, 0], "(it did move)");
assert.deepStrictEqual(C.follow(T2, T, ["project:alpha", "a0", "a0-ph0", "a0-0-1"]), ["project:alpha", "a0", "a0-ph0", "a0-0-2"], "gone: the cell now at the same coordinate");
var S3 = JSON.parse(JSON.stringify(S)); S3.biomes[0].plots[0].beds[0].cells = S3.biomes[0].plots[0].beds[0].cells.slice(0, 2);
var T3 = C.tree(S3, home, { now: NOW });
assert.deepStrictEqual(C.follow(T3, T, ["project:alpha", "a0", "a0-ph0", "a0-0-4"]), ["project:alpha", "a0", "a0-ph0", "a0-0-1"], "an empty coordinate: the nearest occupied");
var S4 = JSON.parse(JSON.stringify(S)); S4.biomes[0].plots[0].beds[0].cells = [];
var T4 = C.tree(S4, home, { now: NOW });
assert.deepStrictEqual(C.follow(T4, T, ["project:alpha", "a0", "a0-ph0", "a0-0-4"]), ["project:alpha", "a0", "a0-ph0"], "an empty grid: ascend");
var S5 = JSON.parse(JSON.stringify(S)); S5.biomes[1].plots.splice(0, 1);
var T5 = C.tree(S5, home, { now: NOW });
var f5p = C.follow(T5, T, ["project:beta", "b0", "b0-ph1", "b0-1-0"]);
assert.strictEqual(f5p[0], "project:beta"); assert.strictEqual(T5.nodes[f5p[1]].kind, "plot"); assert.strictEqual(f5p.length, 4, "a completed plan: the nearest plot, back down to the same depth");
/* the camera does not re-frame on a data change: follow keeps the path, so the page's framing (a function of the path) stays */
assert.deepStrictEqual(C.follow(T2, T, five), five);

/* ---------- 8. links (section 3.5) ---------- */
function roundTrip(t, world, label) {
  var n = 0;
  Object.keys(t.nodes).forEach(function (id) {
    if (id === "world") return;
    var p = C.pathFor(t, id), at = C.atFor(t, p);
    assert.deepStrictEqual(C.pathFor(t, at), p, label + ": at round trip " + id + " " + at);
    var k = t.nodes[id].kind;
    if (k === "biome" || k === "plot" || k === "task" || k === "flower" || k === "tree" || k === "seed" || (k === "bed" && !t.nodes[id].ref.other && t.nodes[id].ref.id)) {
      var r = W.resolve(world, at); assert.ok(r && r.found && r.id === id, label + ": WORLD.resolve reads it back " + at);
    }
    n++;
  });
  return n;
}
roundTrip(T, S, "spec");
assert.deepStrictEqual(C.pathFor(T, "alpha/a0-0-1"), ["project:alpha", "a0", "a0-ph0", "a0-0-1"]);
assert.deepStrictEqual(C.pathFor(T, W.queryGet("?bead=x&at=alpha/a0-ph1&z=3", "at")), ["project:alpha", "a0", "a0-ph1"], "an old link with z= lands on its Thing");
assert.deepStrictEqual(C.pathFor(T, "alpha/gd-gone"), ["project:alpha"], "a gone id falls back to its project");
assert.deepStrictEqual(C.pathFor(T, "late-b"), ["project:alpha", "overflow:alpha"], "a plan past the plot cap finds its crate");
assert.deepStrictEqual(C.pathFor(T, "d1"), ["project:alpha", "home:alpha", "home:alpha/mailbox", "d1"], "a letter");
var EZ = ["project:alpha", "home:alpha", "home:alpha/escalation"];
assert.deepStrictEqual(C.pathFor(T, "e1"), EZ.concat("esc:e1"), "an escalation's bead id finds its cell");
assert.deepStrictEqual(C.pathFor(T, "alpha/escalation"), EZ, "the zone's word (28.7)");
assert.strictEqual(C.atFor(T, EZ), "alpha/escalation");
assert.strictEqual(C.atFor(T, EZ.concat("esc:e1")), "alpha/esc%3Ae1");
assert.deepStrictEqual(C.pathFor(T, "alpha/esc%3Ae1"), EZ.concat("esc:e1"), "and its at= reads back");
assert.deepStrictEqual(C.move(T, ["project:alpha", "home:alpha", "home:alpha/mailbox"], "left").path, EZ, "Left from the Mailbox reaches the Escalations (29.6)");
assert.deepStrictEqual(C.move(T, EZ, "down").path, ["project:alpha", "home:alpha", "home:alpha/growing"], "and Down from them the Growing field");
assert.strictEqual(C.descend(T, EZ.concat("esc:e1"), {}, []).open, "e1", "Enter on an escalation opens its bead");
assert.deepStrictEqual(C.pathFor(T, "a0-0-1"), ["project:alpha", "a0", "a0-ph0", "a0-0-1"], "a bead both growing on the ledger and planted in a plan is the plan's");
assert.strictEqual(C.atFor(T, ["project:"]), "~"); assert.strictEqual(C.atFor(T, ["project:", "home:"]), "~/home");
assert.strictEqual(C.atFor(T, ["project:alpha", "home:alpha", "home:alpha/mailbox"]), "alpha/mailbox", "a zone: its word (28.7)");
assert.deepStrictEqual(C.pathFor(T, "beta/mailbox"), ["project:beta", "home:beta"], "an empty zone's link: its home cell");
assert.deepStrictEqual(C.pathFor(T, "beta/home"), ["project:beta", "home:beta"]);
assert.strictEqual(C.pathFor(T, "nowhere/x"), null);

/* ---------- 9. the fixture farm and the 10x farm through WORLD.layout ---------- */
var wf = W.layout(fixture, null, { now: NOW, readonly: true }), tf = C.tree(wf, null, { now: NOW, gardeners: W.gardenerState(wf, fixture.gardeners) });
checkTree(tf, wf, "fixture"); roundTrip(tf, wf, "fixture");
/* legacy yard links (H-D12): the Commons's home cell, its pen, the thing named wherever it is, else the Commons's home */
var GOAT = ["project:gardenio", "home:gardenio", "home:gardenio/pen", "wish:gd-wish2"];
assert.deepStrictEqual(C.pathFor(tf, "yard"), ["project:", "home:"], "?at=yard: the Commons's home cell");
assert.deepStrictEqual(C.pathFor(tf, "yard/pen"), tf.nodes["home:/pen"] ? ["project:", "home:", "home:/pen"] : ["project:", "home:"], "?at=yard/pen: the Commons's pen, else its home");
assert.deepStrictEqual(C.pathFor(tf, "yard/" + encodeURIComponent("wish:gd-wish2")), GOAT, "?at=yard/wish:<id>: the goat, in its own project's pen");
assert.deepStrictEqual(C.pathFor(tf, "gd-wish2"), GOAT, "a wish's own id finds its goat");
assert.deepStrictEqual(C.pathFor(tf, "yard/wish:gd-gone"), ["project:", "home:"], "a gone goat: the Commons's home");
assert.strictEqual(C.atFor(tf, GOAT), "gardenio/wish%3Agd-wish2", "a goat's at= (28.7)");
var doc = gen.tenx(), w10 = W.layout(doc, null, { now: NOW, readonly: true });
var t0 = Date.now(), t10 = C.tree(w10, null, { now: NOW }), treeMs = Date.now() - t0;
var nodes = checkTree(t10, w10, "10x");
t0 = Date.now(); var trips = roundTrip(t10, w10, "10x"), tripMs = Date.now() - t0;
/* walks: from every project, descend to a task by memory-less descends, then 40 moves and a sibling jump each way; never off the tree */
var walks = 0, bumps = 0, dirs = ["right", "down", "left", "up"];
t0 = Date.now();
t10.root.kids.forEach(function (pid) {
  var p = [pid];
  for (var d = 0; d < 3; d++) { var r = C.descend(t10, p, {}, []); if (r.bumped || r.open || r.drawer) break; p = r.path; }
  for (var i = 0; i < 40; i++) {
    var m = C.move(t10, p, dirs[(i >> 2) % 4]); if (m.bumped) bumps++; p = m.path; walks++;
    assert.ok(t10.nodes[last(p)] && C.gridOf(t10, p).cells, "the walk stays on the tree");
  }
  dirs.forEach(function (dd) { var j = C.jump(t10, p, dd); assert.strictEqual(j.path.length, p.length, "a jump keeps the depth"); assert.ok(j.path.every(function (id, k) { return !k || t10.nodes[id].parent === j.path[k - 1]; }), "a jump lands on a real path"); });
});
var walkMs = Date.now() - t0;
/* Up from every project's top plot row (the home cell among the plots, 29.6): the nearest band above that holds anything, the
   Graveyard, else the Orchard; with both empty, a bump */
var ups = 0, bandUps = 0;
t10.root.kids.forEach(function (pid) {
  var bn = t10.nodes[pid], plots = bn.kids.filter(function (id) { return t10.nodes[id].kind === "plot" || t10.nodes[id].kind === "home"; });
  if (!plots.length) return;
  var r0 = Math.min.apply(null, plots.map(function (id) { return t10.nodes[id].r; })), want = [C.ROW.graveyard, C.ROW.orchard].map(function (r) { return bn.cells[r][0]; }).filter(Boolean)[0];
  plots.filter(function (id) { return t10.nodes[id].r === r0; }).forEach(function (id) {
    var m = C.move(t10, [pid, id], "up");
    if (want) { assert.strictEqual(last(m.path), want, "Up from " + id); bandUps++; } else assert.ok(m.bumped, "no band above " + id + ": a bump");
    ups++;
  });
});
assert.ok(bandUps > 0 && bandUps < ups, "some projects' top rows reach a band, some bump: " + bandUps + " of " + ups);
report.tenx = { nodes: nodes, tree_ms: treeMs, round_trips: trips, round_trip_ms: tripMs, moves: walks, bumps: bumps, walk_ms: walkMs, ups: ups, band_ups: bandUps };
console.log(JSON.stringify(report));
