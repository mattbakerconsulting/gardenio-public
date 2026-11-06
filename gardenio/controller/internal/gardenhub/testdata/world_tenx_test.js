/* Layout golden and stability at 10x (homestead-world-design.md sections 3.5, 6, 9; cursor-grid spec 5 and 10.2), run by
   TestWorldTenXLayoutUnderNode:
     node world_tenx_test.js <web dir> <golden path> [-update]
   Pins: the 10x lattice (every project's cell and a digest of every rectangle), determinism under shuffling, what a memo
   keeps still when beads change (coordinates always; pixels unless a region changes size), the cull bounds a viewport can
   hold, and the data-time layout budget. */
"use strict";
var fs = require("fs"), path = require("path"), assert = require("assert");
var W = require(path.join(process.argv[2], "world.js")), gen = require(path.join(__dirname, "world_tenx.js"));
var goldenPath = process.argv[3], update = process.argv[4] === "-update";

var base = gen.tenx();

/* Every placed rectangle by a stable id, in tiles: biome used/foot/sign/bands/home, plots, beds and their crates, task cells, band
   items, overflow. */
function snapOne(b, out) {
  var put = function (id, r) { out[id] = r.x + "," + r.y + "," + r.w + "," + r.h; };
  put(b.id + "#used", b.used); put(b.id + "#foot", b.foot); put(b.id + "#sign", b.sign);
  ["orchard", "nursery"].forEach(function (k) { put(b.id + "#band:" + k, b.bands[k]); }); put(b.id + "#home", b.home);
  Object.keys(b.home.zones).forEach(function (z) { put(b.home.zones[z].id, b.home.zones[z]); });
  b.plots.forEach(function (p) {
    put("plot:" + p.id, p); if (p.crate) put("phases:" + p.crate.id, p.crate);
    p.beds.forEach(function (bd) { put("bed:" + p.id + "/" + bd.id, bd); if (bd.crate) put("crate:" + bd.crate.id, bd.crate); bd.cells.forEach(function (c) { put("task:" + c.id, c); }); });
  });
  b.orchard.concat(b.meadow, b.nursery, b.pen.items).forEach(function (it) { put(it.kind + ":" + it.id, it); });
  if (b.overflow) put("overflow:" + b.key, b.overflow);
  return out;
}
function snap(w) { var out = {}; w.biomes.forEach(function (b) { snapOne(b, out); }); return out; }
/* ids present in both snapshots whose rectangle differs */
function moved(a, b) { return Object.keys(a).filter(function (k) { return b[k] !== undefined && b[k] !== a[k]; }).sort(); }
function fnv(s) { var h = 2166136261; for (var i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619) >>> 0; } return ("0000000" + h.toString(16)).slice(-8); }
function digest(w) { var s = snap(w); return fnv(Object.keys(s).sort().map(function (k) { return k + "=" + s[k]; }).join("\n")); }
function counts(w) {
  var c = { biomes: w.biomes.length, plots: 0, tasks: 0, items: 0, overflow: 0 };
  w.biomes.forEach(function (b) { c.plots += b.plots.length; b.plots.forEach(function (p) { p.beds.forEach(function (bd) { c.tasks += bd.cells.length; }); }); c.items += b.orchard.length + b.meadow.length + b.nursery.length; if (b.overflow) c.overflow += b.overflow.n; });
  return c;
}
function shuffle(a, r) { a = a.slice(); for (var i = a.length - 1; i > 0; i--) { var j = Math.floor(r() * (i + 1)), t = a[i]; a[i] = a[j]; a[j] = t; } return a; }
function byKey(w) { var m = {}; w.biomes.forEach(function (b) { m[b.key] = b; }); return m; }
/* The grid coordinates N-D9 promises: every project's lattice index and every plot's {c, r}. */
function coords(w) { var out = {}; w.biomes.forEach(function (b) { out[b.id] = "k" + b.cell.k; b.plots.forEach(function (p) { out[p.id] = p.at.c + "," + p.at.r; }); }); return out; }
function changed(a, b) { return Object.keys(a).filter(function (k) { return b[k] !== undefined && b[k] !== a[k]; }).sort(); }
/* A project moves as a whole or not at all (N-D9: pixels shift by whole column widths, nothing re-flows): every plot of a project
   that is in both worlds moved by one delta, and so did every rectangle of a project whose region kept its size. */
function rigid(w0, w1, label) {
  var b1 = byKey(w1);
  w0.biomes.forEach(function (b) {
    var o = b1[b.key]; if (!o) return;
    var dx = o.region.x - b.region.x, dy = o.region.y - b.region.y, same = o.region.w === b.region.w && o.region.h === b.region.h;
    var deltas = {};
    b.plots.forEach(function (p) { var q = w1.index[p.id]; if (q && q.kind === "plot") deltas[(q.x - p.x) + "," + (q.y - p.y)] = 1; });
    assert.ok(Object.keys(deltas).length <= 1, label + ": the plots of " + b.key + " move together: " + Object.keys(deltas).join(" "));
    if (same) {
      var s0 = {}, s1 = {}; snapOne(b, s0); snapOne(o, s1);
      Object.keys(s0).forEach(function (k) {
        if (s1[k] === undefined) return;
        var a = s0[k].split(",").map(Number), c = s1[k].split(",").map(Number);
        assert.ok(c[0] - a[0] === dx && c[1] - a[1] === dy && c[2] === a[2] && c[3] === a[3], label + ": " + k + " moved with its project " + b.key);
      });
    }
  });
}
/* A copy of a plan under a new id, with fresh phase and task ids, so it is a different plan and not a duplicate. */
function fresh(plan, tag) {
  var p = gen.clone(plan); p.id = tag;
  p.phases.forEach(function (ph, j) { ph.id = tag + "-h" + j; ph.tasks.forEach(function (t, k) { t.id = tag + "-h" + j + "-t" + k; }); });
  return p;
}
function recount(plan) {
  var pc = { in_progress: 0, review: 0, blocked: 0, open: 0, deferred: 0, closed: 0 };
  plan.phases.forEach(function (ph) { var c = { in_progress: 0, review: 0, blocked: 0, open: 0, deferred: 0, closed: ph.counts.closed }; ph.tasks.forEach(function (t) { c[t.state]++; }); c.open += ph.more || 0; ph.counts = c; Object.keys(c).forEach(function (k) { pc[k] += c[k]; }); });
  plan.counts = pc;
}

/* ---------- 1. the 10x golden: 50 projects in the lattice (cursor-grid spec 5.1), the Commons cell 0, then the projects oldest
   first from cell 1; one project over the plot cap ---------- */
var t0 = Date.now(), cold = W.layout(base, null, { now: 1 }), coldMs = Date.now() - t0;
var golden = { projects: {}, lattice: { cols: cold.lattice.cols, rows: cold.lattice.rows }, counts: counts(cold), bbox: cold.bbox, digest: digest(cold) };
cold.biomes.forEach(function (b) { golden.projects[b.key || "~"] = "k" + b.cell.k + " @" + b.centre.x + "," + b.centre.y; });
assert.strictEqual(cold.biomes.length, 51, "50 projects plus the Commons");
assert.deepStrictEqual([cold.biomes[0].key, cold.biomes[0].cell.k], ["", 0], "the Commons is cell 0, at the origin");
assert.deepStrictEqual(cold.biomes.map(function (b) { return b.cell.k; }), cold.biomes.map(function (b, i) { return i; }), "projects take cells 1..50 with no holes");
var ages = cold.biomes.slice(1).map(function (b) { var a = Infinity; base.plans.concat(base.seeds).forEach(function (it) { if (W.projectKey(it.project) === b.key && it.created > 0 && it.created < a) a = it.created; }); return a; });
for (var ai = 1; ai < ages.length; ai++) assert.ok(ages[ai - 1] <= ages[ai], "oldest first: cell " + (ai + 1));
assert.deepStrictEqual([cold.lattice.cols, cold.lattice.rows], [5, 11], "51 cells, five wide");
assert.notStrictEqual(golden.projects.gardenio, undefined);
assert.strictEqual(golden.counts.plots, 358); assert.strictEqual(golden.counts.overflow, 2);   // 34 plots a biome: slot 0 is its farmstead (29.1)
/* every region sits inside its lattice cell and keeps GAP tiles clear of every other */
var gapOK = function (a, b) { return a.x + a.w + W.GAP <= b.x || b.x + b.w + W.GAP <= a.x || a.y + a.h + W.GAP <= b.y || b.y + b.h + W.GAP <= a.y; };
var regions = cold.biomes.map(function (b) { return b.region; });
for (var ri = 0; ri < regions.length; ri++) for (var rj = ri + 1; rj < regions.length; rj++) assert.ok(gapOK(regions[ri], regions[rj]), "regions keep their gap: " + ri + " " + rj);
cold.biomes.forEach(function (b) {
  var r = cold.lattice.cells[b.cell.k].rect;
  assert.ok(b.region.x >= r.x && b.region.y >= r.y && b.region.x + b.region.w <= r.x + r.w && b.region.y + b.region.h <= r.y + r.h, "a region lies inside its cell: " + b.key);
  assert.deepStrictEqual(b.foot, b.region, "the region is the foot: " + b.key);
});
var cells = {}; cold.biomes.forEach(function (b) { cells[b.cell.k] = b.key; });
/* every plot's beds sit at the 3x3 offsets (2, 12, 22) without overlap, every cell inside its bed at its block-major place, and
   every bed's cursor stops (tasks, then a crate) fit the 81 cells */
var maxStops = 0, crates = 0;
cold.biomes.forEach(function (b) { b.plots.forEach(function (p) {
  assert.deepStrictEqual(p.at, W.slotAt(p.slot));
  p.beds.forEach(function (bd, i) {
    assert.deepStrictEqual([bd.x - p.x, bd.y - p.y, bd.w, bd.h, bd.n], [W.BED.off[i % 3], W.BED.off[Math.floor(i / 3)], 9, 9, i + 1], "bed at its offset: " + bd.id);
    bd.cells.forEach(function (c, j) { assert.ok(c.i === j && c.x === bd.x + W.cellAt(j).c && c.y === bd.y + W.cellAt(j).r, "block-major cell: " + c.id); });
    var stops = bd.cells.length + (bd.crate ? 1 : 0); maxStops = Math.max(maxStops, stops); if (bd.crate) crates++;
    assert.ok(stops + bd.stubble <= W.BED.cap, "a bed holds 81 cells: " + bd.id);
  });
  assert.ok(p.beds.length + (p.crate ? 1 : 0) <= 9, "at most nine beds: " + p.id);
}); });
assert.ok(maxStops > 9, "the fixture spills a bed past its first block: " + maxStops + " (the 60-task bed and its crate are pinned in TestWorldLayoutUnderNode)");
var pretty = JSON.stringify(golden, null, 2) + "\n";
if (update) fs.writeFileSync(goldenPath, pretty);
var want = fs.existsSync(goldenPath) ? fs.readFileSync(goldenPath, "utf8") : "";
assert.strictEqual(pretty, want, "10x layout drifted from " + path.basename(goldenPath) + "; if the change is intended, regenerate with -update-world-golden and note it in homestead-world-design.md");

/* ---------- 2. determinism: a shuffled document gives a byte-identical map, cold and from a memo ---------- */
var r = gen.rng(99), mixed = gen.clone(base);
mixed.plans = shuffle(mixed.plans, r); mixed.seeds = shuffle(mixed.seeds, r); mixed.closed_plans.shown = shuffle(mixed.closed_plans.shown, r); mixed.meadow.shown = shuffle(mixed.meadow.shown, r);
assert.strictEqual(digest(W.layout(mixed, null, { now: 1 })), golden.digest, "shuffled input, same map");
assert.strictEqual(digest(W.layout(mixed, cold.memo, { now: 2 })), golden.digest, "a memo of the same world changes nothing");
assert.strictEqual(digest(W.layout(base, JSON.parse(JSON.stringify(cold.memo)), { now: 3 })), golden.digest, "a memo round-tripped through JSON is the same memo");

/* ---------- 3. stability with a memo: what a live browser keeps still when beads change ---------- */
var before = snap(cold), memo = cold.memo, bk = byKey(cold);
function after(doc, now) { return snap(W.layout(doc, JSON.parse(JSON.stringify(memo)), { now: now || 10 })); }

/* 3a. every task changes state, title, assignee, priority, MR and time; every plan's counts, activity and needs change: nothing moves */
var churn = gen.clone(base);
churn.plans.forEach(function (p, i) {
  p.title = "renamed " + i; p.activity += 5000; p.updated += 5000; p.priority = (p.priority + 1) % 4; p.open_decisions = i % 3; p.open_escalations = i % 5 === 0 ? 1 : 0; p.state = i % 2 ? "building" : "review";
  p.phases.forEach(function (ph, j) { ph.title = "phase " + j; ph.status = "in_progress"; ph.tasks.forEach(function (t, k) { t.state = ["open", "in_progress", "review", "blocked", "deferred"][(k + i) % 5]; t.title = "moved on " + k; t.assignee = "someone"; t.priority = 0; t.updated += 100; t.type = k % 2 ? "bug" : "decision"; t.mr = { url: "https://x/y", state: "merged" }; }); });
  recount(p);
});
churn.seeds.forEach(function (s) { s.title = "x"; s.tasks += 3; s.phases += 1; });   // a draft that turns ready is staked out on a plot (TestWorldLayoutStaked)
churn.meadow.shown.forEach(function (t) { t.state = "blocked"; t.title = "y"; });
var a3 = after(churn);
assert.deepStrictEqual(moved(before, a3), [], "state, title and count changes move nothing");
assert.deepStrictEqual(Object.keys(a3).sort(), Object.keys(before).sort(), "and place the same things");

/* 3b. a new project, and three plans on it: no project's index and no plot's {c, r} changes, every project moves as a whole
   (or not at all), and the newcomer takes the lowest free cell */
var cbefore = coords(cold);
var grown = gen.clone(base);
for (var g = 0; g < 3; g++) { var np = fresh(base.plans[g], "gd-NEW" + g); np.project = "Newcomer"; np.created = 1; grown.plans.push(np); }
grown.seeds.push({ id: "gd-NEWSEED", title: "n", state: "frosted", status: "open", project: "Newcomer", priority: 2, created: 1, updated: 1, phases: 1, tasks: 0 });
var wg = W.layout(grown, JSON.parse(JSON.stringify(memo)), { now: 10 });
assert.deepStrictEqual(changed(cbefore, coords(wg)), [], "a new project changes no coordinate");
rigid(cold, wg, "a new project");
assert.ok(byKey(wg).newcomer && byKey(wg).newcomer.plots.length === 3 && !cells[byKey(wg).newcomer.cell.k], "the newcomer is placed on a free cell");
assert.strictEqual(byKey(wg).newcomer.cell.k, 51, "the lowest free index, although it is the oldest project");
wg.biomes.forEach(function (b) { if (b.key !== "newcomer") assert.ok(gapOK(b.region, byKey(wg).newcomer.region), "the newcomer keeps its gap from " + b.key); });

/* 3c. a plan added to every project: no coordinate changes and every project moves as a whole. The 36-plan project gains a
   third overflow plan. */
var added = gen.clone(base), addTo = {};
cold.biomes.forEach(function (b) { if (b.key) addTo[b.key] = b.name; });
Object.keys(addTo).forEach(function (k, i) { var np = fresh(base.plans[i], "gd-ADD" + i); np.project = addTo[k]; np.created = 1; added.plans.push(np); });
var wa = W.layout(added, JSON.parse(JSON.stringify(memo)), { now: 10 }), aa = snap(wa);
assert.deepStrictEqual(changed(cbefore, coords(wa)), [], "a plan added anywhere changes no coordinate");
rigid(cold, wa, "a plan added");
var big = cold.biomes.filter(function (b) { return b.plots.length === W.SLOT_CAP; })[0];
assert.strictEqual(byKey(wa)[big.key].overflow.n, 3, "the full biome counts the newcomer in its overflow marker");
Object.keys(addTo).forEach(function (k, i) { assert.ok(aa["plot:gd-ADD" + i] || byKey(wa)[k].overflow.ids.indexOf("gd-ADD" + i) >= 0, "placed " + k); });

/* 3d. 20 plans close (spread across projects). Slots are packed (decision gd-VfOCGDpxg2, cursor-grid spec 12.2): no project
   changes index; in every project the survivors keep their order and fill slots 1..n with no hole, each plot stepping back one
   spiral slot for every plan ahead of it that closed; a project that lost no plan moves as a whole or not at all. Each closed
   plan keeps its memo entry, and a plan created later takes the next slot after the survivors rather than shifting anyone */
var closing = {}, gone = gen.clone(base), lost = {};
for (var c = 0; c < 20; c++) closing[base.plans[c * 17].id] = 1;
gone.plans = gone.plans.filter(function (p) { return !closing[p.id]; });
var wc = W.layout(gone, JSON.parse(JSON.stringify(memo)), { now: 10 }), ac = snap(wc);
changed(cbefore, coords(wc)).forEach(function (k) { assert.ok(k.indexOf("project:") !== 0, "closing plans changes no project's index: " + k); });
cold.biomes.forEach(function (b) {
  var o = byKey(wc)[b.key], ahead = 0, kept = [];
  b.plots.forEach(function (p) {
    if (closing[p.id]) { ahead++; return; }
    kept.push(p.id); assert.strictEqual(wc.index[p.id].slot, p.slot - ahead, "a plot steps back one slot per closed plan ahead of it: " + p.id);
  });
  if (ahead) lost[b.key] = 1;
  assert.deepStrictEqual(o.plots.slice(0, kept.length).map(function (p) { return p.id; }), kept, "the survivors keep their order: " + b.key);
  o.plots.forEach(function (p, i) { assert.strictEqual(p.slot, i + 1, "no fallow hole: " + b.key); });
});
assert.ok(Object.keys(lost).length > 5, "the closes spread across projects");
rigid({ biomes: cold.biomes.filter(function (b) { return !lost[b.key]; }) }, wc, "closing plans elsewhere");
/* three of the full project's 36 plans close, so its two overflowed plans take the next slots and the marker goes */
var bigId = cold.biomes.filter(function (b) { return b.plots.length === W.SLOT_CAP; })[0].id;
assert.ok(!wc.index["overflow:" + bigId.slice(8)] && !byKey(wc)[bigId.slice(8)].overflow && ac["overflow:" + bigId.slice(8)] === undefined, "the overflow marker is gone once the plans fit");
Object.keys(closing).forEach(function (pid) { assert.ok(ac["plot:" + pid] === undefined && wc.memo.slots[pid] && wc.memo.slots[pid].t === 1, "a closed plan is gone but remembered: " + pid); });
var refill = gen.clone(gone), first = base.plans[0], rp = fresh(first, "gd-REFILL"); rp.created = 9e9; refill.plans.push(rp);
var wr = W.layout(refill, JSON.parse(JSON.stringify(wc.memo)), { now: 11 });
assert.deepStrictEqual(changed(coords(wc), coords(wr)), [], "a new plan after the closes changes no coordinate");
assert.strictEqual(wr.index["gd-REFILL"].slot, byKey(wr)[W.projectKey(first.project)].plots.length, "the newest plan takes the next slot after the survivors");

/* 3e. tasks close (leave the listed tasks): only the later cells of the same bed slide one cell back in block-major order; the
   bed, plot, every other plot and every band item stay. This bed-local reflow is the harvest (section 12: closed tasks become
   stubble). */
var harvest = gen.clone(base), touched = {};
harvest.plans.forEach(function (p, i) { if (i % 12) return; var ph = p.phases[0]; if (ph.tasks.length < 3) return; ph.tasks.splice(1, 1); ph.counts.closed++; touched[p.id] = ph.id; recount(p); });
var ah = after(harvest), mh = moved(before, ah);
assert.ok(mh.length > 0, "a closed task frees its tile");
mh.forEach(function (k) { assert.ok(k.indexOf("task:") === 0, "only task cells move: " + k); var t = base.plans.filter(function (p) { return touched[p.id]; }).some(function (p) { return p.phases[0].tasks.some(function (x) { return "task:" + x.id === k; }); }); assert.ok(t, "a moved cell is in a touched bed: " + k); });
Object.keys(touched).forEach(function (pid) { assert.strictEqual(ah["plot:" + pid], before["plot:" + pid]); assert.strictEqual(ah["bed:" + pid + "/" + touched[pid]], before["bed:" + pid + "/" + touched[pid]]); });

/* 3f. band items (pots, flowers, trees) have no memo (section 3.3): removing one may move a neighbour that had probed past
   its cell, but only inside the same band of the same biome; plots and cells never move for it */
var pruned = gen.clone(base), drop = {};
pruned.seeds = pruned.seeds.filter(function (s, i) { if (i % 3) return true; drop[s.project] = 1; return false; });
var ap = after(pruned), mp = moved(before, ap);
mp.forEach(function (k) { assert.ok(k.indexOf("seed:") === 0, "pruning seeds moves only pots: " + k); });
var movedPots = {}; mp.forEach(function (k) { var sid = k.slice(5); base.seeds.forEach(function (s) { if (s.id === sid) movedPots[s.project] = 1; }); });
Object.keys(movedPots).forEach(function (p) { assert.ok(drop[p], "a pot moved in a biome that lost no seed: " + p); });

/* 3g. a memo read from an empty or truncated document is never written; a truncated document still lays out */
var trunc = gen.clone(base); trunc.meta.truncated = true;
var wt = W.layout(trunc, JSON.parse(JSON.stringify(memo)), { now: 10, readonly: true });
assert.strictEqual(wt.memoChanged, false); assert.strictEqual(digest(wt), golden.digest, "readonly gives the same map");
assert.strictEqual(JSON.stringify(wt.memo), JSON.stringify(memo), "and leaves the memo as read");

/* 3h. the memo stays bounded at 10x: every plan slot and project domain remembered, under the caps */
assert.ok(Object.keys(memo.slots).length === 358 && Object.keys(memo.cells).length === 50, "memo holds every plot and project (the Commons is always cell 1)");
var fat = JSON.parse(JSON.stringify(memo)); for (var f = 0; f < 5000; f++) fat.slots["old" + f] = { s: f % 35, t: 0 };
var wf = W.layout(base, fat, { now: 40 * 86400000 });
assert.ok(Object.keys(wf.memo.slots).length <= 4096 && !wf.memo.slots.old1 && wf.memo.slots[base.plans[0].id], "stale entries pruned first, live ones kept");

/* ---------- 4. cull bounds: what one viewport can hold at 10x, per level (section 6) ---------- */
var VP = { w: 1920, h: 1080 }, dense = cold.biomes.slice().sort(function (a, b) { return b.plots.length - a.plots.length; })[0];
var worst = { 0: 0, 1: 0, 2: 0, 3: 0 }, plotsAt = { 0: 0, 1: 0, 2: 0, 3: 0 };
cold.biomes.forEach(function (b) {
  for (var lv = 0; lv < 4; lv++) {
    var cam = W.fit(W.tiles(b.used), VP, { level: lv, pad: 0.15 }), v = W.visible(cold, W.viewRect(cam, VP, 0.25), lv);
    var n = v.biomes.length * 2 + v.plots.length * 2 + v.items.length + v.tasks.length + v.overflow.length;
    worst[lv] = Math.max(worst[lv], n); plotsAt[lv] = Math.max(plotsAt[lv], v.plots.length);
  }
});
/* packed (section 16): a Plan-level view over the 35-plot biome also meets its neighbours, so up to about two biomes' plots show;
   at Bead the view is still inside one biome */
assert.ok(plotsAt[3] <= 35, "a viewport never sees more than one biome's plots at Bead: " + plotsAt[3]);
assert.ok(plotsAt[2] <= 80, "a Plan-level viewport sees at most about two biomes' plots in a packed world: " + plotsAt[2]);
assert.ok(worst[3] <= 6000, "visible things at Bead on a 1920x1080 view, padded 25%: " + worst[3]);
var all = W.visible(cold, cold.bbox, 3);
assert.strictEqual(all.tasks.length, golden.counts.tasks, "the whole world is reachable");

/* ---------- 5. the data-time budget: layout at 10x runs on a farm event, never per frame ---------- */
var best = coldMs, warm = 1e9;
for (var i = 0; i < 5; i++) { var s = Date.now(); W.layout(base, null, { now: 1 }); best = Math.min(best, Date.now() - s); s = Date.now(); W.layout(base, memo, { now: 2 }); warm = Math.min(warm, Date.now() - s); }
assert.ok(best < 400, "cold layout at 10x took " + best + " ms (budget 400)");
assert.ok(warm < 200, "memo'd layout at 10x took " + warm + " ms (budget 200)");

process.stdout.write(JSON.stringify({ layout_cold_ms: best, layout_memo_ms: warm, counts: golden.counts, visible_worst: worst, plots_worst: plotsAt, memo_bytes: JSON.stringify(memo).length, doc_bytes: JSON.stringify(base).length, dense: dense.key }) + "\n");
