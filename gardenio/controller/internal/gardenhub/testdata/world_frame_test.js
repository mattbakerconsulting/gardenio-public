/* The frame budget at 10x (homestead-world-design.md section 6.6; cursor-grid spec 10.3), run by TestWorldFrameBudgetUnderNode:
     node world_frame_test.js <web dir> [-v]
   garden.js boots under world_dom_mock.js with the 10x document, then the cursor is walked the way a person walks it: 40 moves
   at each depth (the world, the densest project, a plan, a phase), sibling jumps, ten descends and ten ascends, plus a mouse
   drag that pans and comes back, and a wheel that zooms about the pointer (the free camera, cursor-grid spec revision 3), small
   notches and then one across a level edge, which takes the cursor out with it. A move that does
   not scroll the framing costs at most 3 ms of script at the median and bakes no chunk; a descend settles in at most 64 frames. Each frame is one input event, the tasks it queues (a cursor move's signs run in a task of their own) and the render it queues. What is measured is the page's own
   JavaScript per frame (culling, hot-link and label reconciliation, the draw calls issued) and the counts that bound the
   raster work (draw calls, hot nodes, labels, chunk bakes). Layout must never run inside a frame. */
"use strict";
var path = require("path"), assert = require("assert");
var M = require(path.join(__dirname, "world_dom_mock.js")), gen = require(path.join(__dirname, "world_tenx.js"));
var webDir = process.argv[2], verbose = process.argv.indexOf("-v") > 0;

/* Budgets. A frame is 16.7 ms; the page's JavaScript gets a quarter of it at the median so the browser keeps the rest for
   raster and compositing, and a generous p95 for garbage collection on a loaded CI runner. Draw calls bound the raster
   side: a few thousand fillRect/drawImage per frame is well inside what a 2D canvas paints in a few ms. */
/* bake_ms: a native chunk bake (wild grass, biome ground, fences and plot beds for 32x32 tiles, blitted from the tile sprite
   cache) is the largest single unit of work a frame does; BAKE_PER_FRAME of them are allowed per frame. Before the sprite cache
   one bake was ~100k fills and 6-12 ms here; a bake now is ~1-2k drawImage. "bakes" counts the chunk bitmaps;
   the small sprite canvases are counted apart, as "sprites", since each is painted once per session. */
/* still_ms: a move inside the framing at the world still repaints it, 1.2-1.6 ms on a quiet core; at 2 ms a neighbour on the
   same core failed it on CPU time alone, so 3 ms, which a still move doing twice the work still misses (gd-WsKtzYxI5c). */
/* draws: the birds sweep at the Plan level over the densest project, with the plot it aims at centred, is ~8600 calls (the
   farmstead's live part among its plots); 8000 had passed only where the old project framing left that plot (gd-vtkyDBES19). */
var BUDGET = { median_ms: 4, p95_ms: 12, frame_ms: 16, bake_ms: 24, draws: 9000, hots: 300, bakes_per_frame: 8, bakes_per_level: 64, settle_frames: 64, farmdoc_ms: 1000, still_ms: 3 };

/* The clock is the main thread's CPU time, not the wall (gd-WsKtzYxI5c). A busy CI runner shares its cores with the other
   test packages and may cap the pod with a CFS quota, so the wall charges a frame for every slice the script sat preempted or
   throttled: medians twice the quiet ones, single frames of 100+ ms with no work in them. The thread's CPU time counts only
   the work the page's script did, garbage collection on that thread included, which is what the budgets bound; a real
   regression is as slow on it as on the wall. Not the process's: V8's compiler and GC helper threads add ~400 ms to the first
   farm document that the page's thread never waits for. On Linux the thread's figure is only brought up to date at a
   scheduler tick (1 ms steps, too coarse for a 2 ms frame), but reading the process's first does that (thread_group_cputime
   updates the running task), which makes it exact to the microsecond. A node before 23.9 has no threadCpuUsage: the process's. */
function clock() { var u = process.cpuUsage(); if (process.threadCpuUsage) u = process.threadCpuUsage(); return (u.user + u.system) / 1000; }
/* The CPU clock still runs slow on a shared core (gd-idxhLFHgIq): a neighbour on the other hyperthread, or one thrashing the
   cache, makes the same work take more CPU time, up to twice the quiet figure, steadily over a whole sweep, so no attempt of
   three escapes it. So the budgets are stated for a quiet core and scaled by how slow this one runs right now: before every
   frame the script times a fixed workload of its own (sort 1024 objects by a fresh key and read them back; no
   page code, so a regression in the page never moves it) and divides the frame's time by max(1, median of the last 15 of
   those / CALIB_MS). CALIB_MS is that workload on a quiet core of the pool (Xeon 8259CL, node 24): its fastest of 2000 runs
   on a busy pod, and the busy median 0.41 over the walk's own busy/quiet ratio (1.7). On a busy pod the still move then reads
   1.8-2.3 ms (3.0-3.9 raw), and render doing 1.5 ms more work fails it at 3.4-3.7. The counts are never scaled, nor is a
   time on a core faster than the reference. */
var CALIB_MS = 0.25, CALIB_N = 1024, CALIB_WINDOW = 15, calibPool = [], calibSeed = 1, calibSink = 0, calibs = [];
for (var ci = 0; ci < CALIB_N; ci++) calibPool.push({ k: 0, v: ci, s: "c" + (ci % 97) });
function calibrate() {
  var t = clock(), i, s = 0;
  for (i = 0; i < CALIB_N; i++) { calibSeed = (calibSeed * 1103515245 + 12345) & 0x7fffffff; calibPool[i].k = calibSeed & 0xffff; }
  calibPool.sort(function (a, b) { return a.k - b.k; });
  for (i = 0; i < CALIB_N; i++) s += calibPool[i].s.length + (calibPool[i].v & 7);
  calibSink += s; calibs.push(clock() - t); if (calibs.length > CALIB_WINDOW) calibs.shift();
}
function slowdown() { return Math.max(1, pct(calibs, 0.5) / CALIB_MS); }
for (ci = 0; ci < 300; ci++) calibrate();   // warm the JIT on the workload before its first figure counts
var h = M.boot(webDir, { w: 1280, h: 720, reduce: true });
h.flushAll();
var W = h.W, layouts = 0, realLayout = W.layout;
W.layout = function () { layouts++; return realLayout.apply(this, arguments); };   // garden.js calls through the WORLD object, so this counts its layouts

/* 60 gardeners spread over the plots (homestead-agents-as-people.md section 7.2 rule 6): people are drawn on the dynamic
   layer at Plan and Bead, from the sprite cache, so they must fit the same budget. Every phase lists its closed steps, the
   newest 12 a plan carrying a harvest, so the harvest cells (harvest spec 4) and their hots are inside it too (spec 12). */
for (ci = 0; ci < CALIB_WINDOW; ci++) calibrate();
var doc = gen.withGardeners(gen.withClosed(gen.tenx()), 60), t0 = clock();
h.farmdoc(doc); h.flushAll();
var farmdocMs = (clock() - t0) / slowdown();
assert.strictEqual(layouts, 1, "one layout per farm document");
var world = realLayout(doc, null, { now: 1 }), dense = world.biomes.slice().sort(function (a, b) { return b.plots.length - a.plots.length; })[0];

function pct(a, p) { var s = a.slice().sort(function (x, y) { return x - y; }); return s[Math.min(s.length - 1, Math.floor(p * s.length))]; }
/* One frame: an input event, the tasks it queues and the render it queues; input_ms is the input task alone. */
/* the grid's links (gridcells in its rows) and the gardeners' beside it */
function hotCount() { var n = h.ghots.children.length; h.hots.children.forEach(function (r) { n += r.children.length; }); return n; }
function camera() { return h.hots.style.transform; }
/* ms and input_ms are scaled to a quiet core (slowdown); cpu_ms is the frame's own CPU time. */
function frame(fn) {
  calibrate(); h.reset(); var before = layouts, cam0 = camera(), t = clock();
  fn(); var input = clock() - t, ntasks = h.tasks(), rafs = h.flush();
  var cpu = clock() - t, slow = slowdown(), s = h.stats;
  return { ms: cpu / slow, cpu_ms: cpu, slow: slow, input_ms: input / slow, tasks: ntasks, draws: s.main.fillRect + s.main.drawImage, bakes: s.canvases, sprites: s.sprites, offdraws: s.off.fillRect + s.off.drawImage, hots: hotCount(), lbls: h.lbls.children.length, rafs: rafs, layouts: layouts - before, level: h.level(), still: camera() === cam0 };
}
var report = {}, LABEL_MAX = [24, 64, 64, 40], PHASE_MAX = [0, 0, 60, 60];   // phase rows have their own budget (gd-kIUS2y1z7I)
/* Even on the CPU clock a shared runner is noisy (a garbage-collection pause is one slow frame at a random index; a neighbour
   on the same core slows every frame for a while), so a sweep runs up to three times, stopping at the first attempt that fits
   every time budget (median, p95, the slowest frame and, for a walk, the moves inside the framing). Each time figure is judged
   at its best attempt: noise only ever adds time, so the least of three is the closest to the work, and a regression is slow on
   all three. The counts (draws, hots, labels, bakes, layouts) are exact, so they are judged at their worst over every attempt.
   A sweep whose input leaves state the next attempt would start from (the wheel's part-way gesture) passes opts.reset, run
   before each retry to put that state back (gd-7leNovxZB4). */
function times(fs) { return fs.map(function (f) { return f.ms; }); }
function stills(fs) { return fs.filter(function (f) { return f.still; }); }
function fits(fs, walk) {
  var ms = times(fs), still = stills(fs);
  return pct(ms, 0.5) <= BUDGET.median_ms && pct(ms, 0.95) <= BUDGET.p95_ms && Math.max.apply(null, ms) <= BUDGET.bake_ms + BUDGET.frame_ms &&
    (!walk || !still.length || pct(times(still), 0.5) <= BUDGET.still_ms);
}
function sweep(name, n, fn, opts) {
  opts = opts || {}; var runs = [];
  for (var attempt = 0; attempt < 3; attempt++) {
    if (attempt && opts.reset) opts.reset();
    var run = []; for (var i = 0; i < n; i++) run.push(frame(function () { fn(i); }));
    runs.push(run);
    if (fits(run, opts.walk)) break;
  }
  var all = [].concat.apply([], runs), fs = runs[0];
  function least(f) { return +Math.min.apply(null, runs.map(f)).toFixed(2); }
  function most(k) { return Math.max.apply(null, all.map(function (f) { return f[k]; })); }
  function sum(fs, k) { return fs.reduce(function (a, f) { return a + f[k]; }, 0); }
  var r = {
    frames: n, attempts: runs.length, level: fs[0].level, median_ms: least(function (fs) { return pct(times(fs), 0.5); }), p95_ms: least(function (fs) { return pct(times(fs), 0.95); }),
    max_ms: least(function (fs) { return Math.max.apply(null, times(fs)); }), max_draws: most("draws"), max_hots: most("hots"), max_lbls: most("lbls"),
    bakes: sum(fs, "bakes"), max_bakes: most("bakes"), sprites: sum(fs, "sprites"), max_offdraws: most("offdraws"), layouts: sum(all, "layouts"), zoom: h.zoom(),
    input_median_ms: least(function (fs) { return pct(fs.map(function (f) { return f.input_ms; }), 0.5); }), tasks: sum(fs, "tasks"),
    cpu_median_ms: least(function (fs) { return pct(fs.map(function (f) { return f.cpu_ms; }), 0.5); }), slow: +pct(all.map(function (f) { return f.slow; }), 0.5).toFixed(2) };
  var still = stills(all);   // moves inside the framing: no camera, only the ring and the links
  if (still.length) { r.still = stills(fs).length; r.still_median_ms = least(function (fs) { return pct(times(stills(fs)), 0.5); }); r.still_bakes = sum(still, "bakes"); }
  report[name] = r;
  if (verbose) console.error(name, JSON.stringify(r));
  assert.strictEqual(r.layouts, 0, name + ": layout ran inside a frame");
  assert.ok(r.median_ms <= BUDGET.median_ms, name + ": median " + r.median_ms + " ms over the " + BUDGET.median_ms + " ms budget");
  assert.ok(r.p95_ms <= BUDGET.p95_ms, name + ": p95 " + r.p95_ms + " ms over the " + BUDGET.p95_ms + " ms budget");
  assert.ok(r.max_ms <= BUDGET.bake_ms + BUDGET.frame_ms, name + ": slowest frame " + r.max_ms + " ms, budget " + (BUDGET.bake_ms + BUDGET.frame_ms));
  assert.ok(r.max_draws <= BUDGET.draws, name + ": " + r.max_draws + " draw calls in a frame, budget " + BUDGET.draws);
  assert.ok(r.max_hots <= BUDGET.hots + 3, name + ": " + r.max_hots + " hot nodes, cap " + BUDGET.hots);
  assert.ok(r.max_bakes <= (opts.bakes || BUDGET.bakes_per_frame), name + ": " + r.max_bakes + " bitmaps baked in one frame");
  all.forEach(function (f) { assert.ok(f.lbls <= LABEL_MAX[f.level] + PHASE_MAX[f.level] + 3, name + ": " + f.lbls + " labels at level " + f.level); });
  if (opts.walk && r.still) {
    assert.ok(r.still_median_ms <= BUDGET.still_ms, name + ": a move inside the framing took " + r.still_median_ms + " ms at the median, budget " + BUDGET.still_ms);
    assert.strictEqual(r.still_bakes, 0, name + ": a move inside the framing baked a chunk");
  }
  return r;
}
/* A fly-to: a jump cut, then the frames that bake the native chunks under the new view one per frame until the render
   stops asking for more (the settle). Every one of those frames has to fit the frame budget on its own. */
function fly(name, fn) {
  var f = frame(fn), settle = [f.ms], bakes = f.bakes, sprites = f.sprites, offdraws = f.offdraws;
  while (h.pending() && settle.length < 500) { var g = frame(function () {}); settle.push(g.ms); bakes += g.bakes; sprites += g.sprites; offdraws = Math.max(offdraws, g.offdraws); assert.ok(g.bakes <= BUDGET.bakes_per_frame, name + ": " + g.bakes + " bakes in a settle frame"); }
  report[name] = { ms: +f.ms.toFixed(2), draws: f.draws, bakes: bakes, sprites: sprites, max_offdraws: offdraws, settle_frames: settle.length, settle_p90_ms: +pct(settle, 0.9).toFixed(2), settle_max_ms: +Math.max.apply(null, settle).toFixed(2), level: f.level, hots: f.hots, lbls: f.lbls, zoom: h.zoom() };
  if (verbose) console.error(name, JSON.stringify(report[name]));
  assert.strictEqual(f.layouts, 0, name + ": layout ran on a fly-to");
  assert.ok(settle.length <= BUDGET.settle_frames, name + ": " + settle.length + " frames to settle, budget " + BUDGET.settle_frames);
  /* every settle frame does the same unit of work (one chunk bake), so the p90 is the measure once there are frames enough
     for one; the max only catches a frame that is wrong by a multiple, not a garbage-collection pause or CPU steal on a shared
     runner. A cursor move that bakes nothing settles in one frame, judged by the max alone (and the descends as a group). */
  if (settle.length >= 5) assert.ok(report[name].settle_p90_ms <= BUDGET.bake_ms, name + ": settle frames p90 " + report[name].settle_p90_ms + " ms, budget " + BUDGET.bake_ms);
  assert.ok(report[name].settle_max_ms <= BUDGET.bake_ms * 3, name + ": a settle frame took " + report[name].settle_max_ms + " ms, budget " + BUDGET.bake_ms * 3);
  return f;
}
function key(k, shift) { return function () { h.key(k, shift); }; }
function depth() { return JSON.parse(h.store["garden.cursor.v1"]).path.length - 1; }
var ARROWS = ["ArrowRight", "ArrowDown", "ArrowRight", "ArrowUp", "ArrowLeft", "ArrowDown", "ArrowLeft", "ArrowUp"];
/* The steady title bar (gd-yozHl8UqIs) follows a move in the input task itself, before the signs' own task (gd-fsuxCGGbZr) runs:
   the first arrow that moves the cursor leaves the signs queued and the bar's last link already on the cursor's Thing. */
function barFollows(name) {
  function at() { var p = JSON.parse(h.store["garden.cursor.v1"] || '{"path":[]}').path; return p[p.length - 1]; }
  for (var i = 0, was = at(); i < ARROWS.length; i++) {   // every way, Left too: a walk may end on the last bed of a one-row plan
    h.key(ARROWS[i]);
    if (at() !== was) {
      var links = h.bar.children.filter(function (c) { return c.dataset.hot; });
      assert.ok(h.pending() > 0 && links.length && links[links.length - 1].dataset.hot === at(), name + ": the title bar reads the cursor's Thing before its signs' task runs: " + JSON.stringify(links.map(function (c) { return c.dataset.hot; })) + " at " + at());
      h.flushAll(); return;
    }
    h.flushAll();
  }
  assert.fail(name + ": no arrow moved the cursor");
}
/* 40 cursor moves in the current grid (a bump at an edge is a move too), then 12 sibling jumps where there is a parent grid */
function walk(name, jumps) {
  barFollows(name);
  var r = sweep(name + "_walk", 40, function (i) { h.key(ARROWS[i % ARROWS.length]); }, { walk: true });
  assert.ok(r.tasks > 0, name + "_walk: a cursor move refreshes its signs in a task of its own (gd-fsuxCGGbZr)");
  /* a sibling jump frames another grid: a fly (a cut under reduced motion, then the settle that bakes the chunks there) */
  if (jumps) for (var j = 0; j < 12; j++) fly(name + "_jump" + j, key(ARROWS[j % 4], true));
}
/* A mouse drag pans (cursor-grid spec revision 3): 20 moves right, then 20 back, so the camera ends where the drag's first
   move left it. */
function drag(name) {
  h.pointer("pointerdown", 1, 640, 360); h.pointer("pointermove", 1, 648, 360);   // past the 5 px click threshold: an 8 px pan
  var c0 = global.window.GARDEN.cam().cam;
  sweep(name + "_drag", 40, function (i) { h.pointer("pointermove", 1, 648 + (i < 20 ? i + 1 : 39 - i) * 2, 360); });
  var c1 = global.window.GARDEN.cam().cam;
  h.pointer("pointerup", 1, 648, 360); h.flushAll();
  assert.ok(c1.z === c0.z && Math.abs(c1.cx - c0.cx) < 1e-6 && Math.abs(c1.cy - c0.cy) < 1e-6, name + ": a drag there and back pans the camera back: " + JSON.stringify([c0, c1]));
}
/* The wheel zooms about the pointer (revision 3): 14 small notches out (a frame crossing a level draws from another mip, whose
   chunks under the view are baked on that frame: up to the LRU of 64, one flat fill each at the coarse mips), which a retry
   wheels back in, and then one wheel across the Bead edge that takes the cursor out one depth; Enter goes back in. */
function wheelZoom(name) {
  var d0 = depth(), z0 = h.zoom();
  sweep(name + "_wheel", 14, function () { h.wheel(8, 700, 300); }, { bakes: BUDGET.bakes_per_level, reset: function () {
    for (var k = 0; k < 14; k++) h.wheel(-8, 700, 300);
    h.flushAll();
    assert.ok(Math.abs(h.zoom() / z0 - 1) < 1e-9, name + ": wheeling the notches back puts the zoom back");
  } });
  assert.strictEqual(depth(), d0, name + ": small notches inside the level move no cursor");
  fly(name + "_wheel_out", function () { h.wheel(Math.ceil(Math.log2(h.zoom() / 0.8) / 0.003), 700, 300); });
  assert.strictEqual(depth(), d0 - 1, name + ": a wheel across the Bead edge ascends once");
  fly(name + "_wheel_in", key("Enter"));
}

/* Into the densest project, its fullest plan and that plan's fullest phase, one descend at a time, each measured as a fly. */
var full = dense.plots.slice().sort(function (a, b) { return b.beds.length - a.beds.length; })[0];
var bed = full.beds.slice().sort(function (a, b) { return b.cells.length - a.cells.length; })[0];
function into(tag) {
  fly(tag + "_top", key("0"));
  assert.strictEqual(depth(), 0, "0 is depth 0");
  fly(tag + "_to_project", function () { global.window.GARDEN.fly(dense.id); });
  fly(tag + "_in_project", key("Enter"));
  assert.strictEqual(depth(), 1, "Enter on a project goes into its plots");
  fly(tag + "_to_plan", function () { global.window.GARDEN.fly(full.id); });
  fly(tag + "_in_plan", key("Enter"));
  assert.strictEqual(depth(), 2, "Enter on a plan goes into its phases");
  fly(tag + "_to_phase", function () { global.window.GARDEN.fly(bed.cells[0].id); });
  assert.strictEqual(depth(), 3, "a step is at depth 3");
}

/* warm the JIT on an unmeasured walk, down to a step and back, so the first measured sweep is not paying for compilation or for
   the sprites each level paints once a session */
h.key("0"); h.flushAll(); for (var wu = 0; wu < 30; wu++) { h.key(ARROWS[wu % ARROWS.length]); h.flushAll(); }
var gplot = doc.gardeners.filter(function (g) { return g.plot && dense.plots.some(function (p) { return p.id === g.plot.plan; }); })[0];
assert.ok(gplot, "a gardener works in the densest project");
[dense.id, full.id, bed.cells[0].id, gplot.id].forEach(function (id) { global.window.GARDEN.fly(id); h.flushAll(); h.key("Enter"); h.flushAll(); });
for (wu = 0; wu < 30; wu++) { h.key(ARROWS[wu % ARROWS.length]); h.flushAll(); }
fly("world_top", key("0")); walk("world"); drag("world");
into("down");
walk("phase", true); drag("phase"); wheelZoom("phase");
fly("phase_out", key("Escape")); assert.strictEqual(depth(), 2, "Escape comes out to the plan");
walk("plan", true);
fly("plan_out", key("Escape")); assert.strictEqual(depth(), 1, "and out to the project");
walk("project", true); drag("project");
/* ten descends and ten ascends between the project and a plan, a phase and a step: every one settles within the budget */
var steps = [];
for (var d = 0; d < 10; d++) { steps.push(fly("descend" + d, key("Enter")).ms); steps.push(fly("ascend" + d, key("Escape")).ms); }
assert.strictEqual(depth(), 1, "the descends and ascends pair up");
report.descend_p90_ms = +pct(steps, 0.9).toFixed(2);
assert.ok(report.descend_p90_ms <= BUDGET.bake_ms, "descends and ascends: p90 " + report.descend_p90_ms + " ms, budget " + BUDGET.bake_ms);
/* the gardeners stand at their plots: their links are beside the grid inside a plot where one works */
fly("gardeners", function () { global.window.GARDEN.fly(gplot.id); });
var ghots = h.ghots.children.filter(function (c) { return c._h && c._h.kind === "gardener"; }).length;
assert.ok(ghots > 0, "gardener links inside a plot where one works");
report.gardener_hots = ghots;

/* A fresh farm document mid-session (a stream patch): data-time work, bounded, and the camera stays put */
var z = h.zoom(), lv = h.level(), doc2 = gen.clone(doc); doc2.plans[0].phases[0].tasks[0].state = "blocked"; doc2.gardeners[0].state = "yielding";
for (ci = 0; ci < CALIB_WINDOW; ci++) calibrate();
var t1 = clock(); h.farmdoc(doc2); var rafs = h.flushAll(); var patchMs = (clock() - t1) / slowdown();
assert.ok(h.zoom() === z && h.level() === lv && rafs >= 1, "a farm patch re-renders without moving the camera");
assert.ok(farmdocMs <= BUDGET.farmdoc_ms && patchMs <= BUDGET.farmdoc_ms, "farm document handling " + farmdocMs.toFixed(0) + "/" + patchMs.toFixed(0) + " ms, budget " + BUDGET.farmdoc_ms);
assert.strictEqual(layouts, 2, "one layout per farm document, none per frame");
layouts = 0;

/* 12 concurrent transitions over the 60 gardeners (homestead-agents-as-people.md section 7.2 rule 6): at the Plan level over the
   densest biome, live patches turn working gardeners there down, six a patch (a patch of more than eight is a cut), until
   twelve bursts run (the cap); every frame of them must fit the same budget, and once they are over the render stops asking
   for frames (rule 5). The mock's reduced motion is on, so the test hook lets ANIM animate anyway; its clock is pinned. */
fly("transitions_project", function () { global.window.GARDEN.fly(dense.id); }); fly("transitions_in", key("Enter"));
/* The wheel in toward the plot nearest the middle of the working gardeners there, until the Plan level, with most of them in
   view. */
var working = doc2.gardeners.filter(function (g) { return g.state === "working" && g.plot && world.index[g.plot.plan] && dense.plots.indexOf(world.index[g.plot.plan]) >= 0; });
var gx = 0, gy = 0; working.forEach(function (g) { var p = world.index[g.plot.plan]; gx += p.x + p.w / 2; gy += p.y + p.h / 2; });
gx /= working.length; gy /= working.length;
var aimPlot = dense.plots.slice().sort(function (a, b) { return Math.hypot(a.x + a.w / 2 - gx, a.y + a.h / 2 - gy) - Math.hypot(b.x + b.w / 2 - gx, b.y + b.h / 2 - gy); })[0];
global.window.GARDEN.look({ on: [aimPlot.id] }); h.flushAll();   // centred first: the view the wheel reaches is the plot's, wherever the project's framing left it
var tf = /translate\(([-\d.]+)px,([-\d.]+)px\) scale\(([-\d.]+)\)/.exec(h.hots.style.transform), T = W.TILE || 16;
var sx = +tf[1] + (aimPlot.x + aimPlot.w / 2) * T * +tf[3], sy = +tf[2] + (aimPlot.y + aimPlot.h / 2) * T * +tf[3];
for (var wz = 0; wz < 39 && h.level() < 2; wz++) { h.wheel(-30, sx, sy); h.flushAll(); }
assert.strictEqual(h.level(), 2, "the wheel reaches the Plan level over the densest project");
var A = global.ANIM, at = 1792000000000, cur = doc2, turned = {}, cands =cur.gardeners.filter(function (g) { return g.state === "working" && g.plot && dense.plots.some(function (p) { return p.id === g.plot.plan; }); });
A.motion(true); A.pin(at); h.farmlive("open"); A.pin(at += A.CATCH_UP + 1);
for (var c = 0; c < cands.length && A.list().length < A.MAX_RUN; c += 6) {
  var next = gen.clone(cur), ids = cands.slice(c, c + 6).map(function (g) { turned[g.id] = 1; return g.id; });
  next.gardeners.forEach(function (g) { if (ids.indexOf(g.id) >= 0) { g.state = "failed"; g.bucket = "stuck"; } });
  h.farmlive("patch", []); h.farmdoc(next); cur = next; h.flush();
}
assert.strictEqual(A.list().length, A.MAX_RUN, "twelve transitions run at once (" + cands.length + " candidates)");
/* every turned gardener bursts; placement is need first (WORLD.gardenerSpots), so one going down on a plot can take the column
   of a working sibling there, who walks to the next one (World Phase 3, gd-HLvOwOxdWz): that walk is the only other kind */
A.list().forEach(function (t) {
  if (turned[t.id]) assert.strictEqual(t.kind, "burst", t.id + " went down and should burst, not " + t.kind);
  else assert.strictEqual(t.kind, "walkto", t.id + " was not turned down, so it can only walk to another column, not " + t.kind);
});
assert.ok(A.list().filter(function (t) { return t.kind === "burst"; }).length >= A.MAX_RUN - 2, "the cap is filled by bursts, a sibling's walk or two aside");
A.pin(at += A.BURST + 16); h.flush();   // unmeasured: the first frame past the particles bakes the flying figures' sprites, once a session
sweep("transitions", 30, function () { A.pin(at += 16); });
assert.ok(A.list().length > 0, "still running at the end of the sweep");
A.pin(at += 5000); h.flushAll();
assert.strictEqual(A.list().length, 0, "every transition ended"); assert.strictEqual(h.pending(), 0, "and the render stopped asking for frames");
A.motion(false); A.pin();

/* Birds (gd-pLFocqmYyf): the 60 gardeners at 4 tool calls a second each, the activity stream's cap per gardener, at the same Plan
   level framing. A frame of 16 ms carries about four calls; the gardeners off the view lift nothing, the rest fill the 24-bird cap
   and the flocks of three, and every frame must fit the same budget. Each call's plate perches on its gardener's head at once
   (gd-CykqEHeEpX); once the calls stop the birds are gone in 2 s, the plates stay, and every frame must fit it too; LINGER ms
   after the calls they are gone and the render stops asking for frames. */
var Bd = global.BIRDS, CLS = ["read", "edit", "test", "git", "beads", "web", "shell", "deploy"], call = 0, tagMax = 0;
var TOOL = { read: "Read", edit: "Edit", test: "go_test", git: "git_push", beads: "kd_show", web: "WebFetch", shell: "other", deploy: "helm_upgrade" };   // each bird carries its tool word (4k)
/* and most calls a caption on its plate (gd-TmYAAOBQEH): one in eight has none (a Bash call with no description), the rest one of
   CAPS distinct ones, two lines' worth, so the bake cache churns as it would on a busy farm */
var CAPS = 300;
function capOf(i) { return i % 8 === 7 ? "" : "Step " + (i % CAPS) + ": read the plan, claim its next task and check the team news"; }
Bd.motion(true); Bd.pin(at); h.farmlive("open"); Bd.pin(at += Bd.QUIET + 1);
function calls(n) { for (var q = 0; q < n; q++, call++) h.activity({ id: cur.gardeners[call % cur.gardeners.length].id, doing: CLS[call % CLS.length], tool: TOOL[CLS[call % CLS.length]], at: 1, dur: 0, cap: capOf(call) }); }
sweep("birds", 60, function () { Bd.pin(at += 16); calls(4); tagMax = Math.max(tagMax, Bd.tags().length); });
var flying = Bd.list().length;
assert.ok(flying >= 12 && flying <= Bd.MAX, flying + " birds at the end of the sweep, cap " + Bd.MAX);
report.birds_live = flying; report.birds_tags_max = tagMax;
assert.ok(tagMax > 0 && tagMax <= Bd.MAX, tagMax + " plates at most in a frame, one a gardener");
var perchMax = 0, gone = Math.max.apply(null, Bd.list().map(function (b) { return b.t0 + b.dur; }));
Bd.pin(at = gone); h.flushAll();
assert.strictEqual(Bd.list().length, 0, "every bird gone");
sweep("birds_linger", 18, function () { Bd.pin(at += 16); perchMax = Math.max(perchMax, Bd.tags().length); });   // the plates, their birds gone
assert.ok(perchMax > 0 && Bd.perched().length <= Bd.MAX, perchMax + " plates perched in a frame, " + Bd.perched().length + " held, cap " + Bd.MAX);
report.birds_perched_max = perchMax;
Bd.pin(at += Bd.LINGER); h.activity({ id: "nobody", doing: "read", tool: "Read", at: 1, dur: 0 }); h.flushAll();   // the plates' wake is a timed timer, which the mock never fires: an event for a gardener not on the map asks for the render the page's own wake would
assert.strictEqual(Bd.perched().length, 0, "every plate gone"); assert.strictEqual(h.pending(), 0, "and the render stopped asking for frames");
/* The same load as a page framing the whole farm hears it under the stream budget (gd-iAq4va9XDt): each gardener's calls fold
   over 1 s, so one event a second each, a finish whose n is the 4 calls started in it, about one a frame. The busy gardeners in
   view still fill the cap with flocks. */
h.farmlive("open"); Bd.pin(at += Bd.QUIET + 1);
function folds(n) { for (var q = 0; q < n; q++, call++) h.activity({ id: cur.gardeners[call % cur.gardeners.length].id, doing: CLS[call % CLS.length], tool: TOOL[CLS[call % CLS.length]], at: 1, dur: 50, n: 4, cap: capOf(call) }); }
sweep("birds_folded", 60, function () { Bd.pin(at += 16); folds(1); });
flying = Bd.list().length;
assert.ok(flying >= 12 && flying <= Bd.MAX, flying + " birds at the end of the folded sweep, cap " + Bd.MAX);
report.birds_folded_live = flying;
Bd.pin(at += 2200 + Bd.LINGER); h.flushAll();
assert.strictEqual(Bd.list().length, 0, "every folded bird gone"); assert.strictEqual(h.pending(), 0, "and the render stopped asking for frames");
Bd.motion(false); Bd.pin();

report.farmdoc_ms = +farmdocMs.toFixed(1); report.patch_ms = +patchMs.toFixed(1); report.dense = dense.key + " (" + dense.plots.length + " plots)"; report.budget = BUDGET; report.clock = process.threadCpuUsage ? "thread cpu" : "process cpu"; report.calib_ms = +pct(calibs, 0.5).toFixed(3);
process.stdout.write(JSON.stringify(report) + "\n");
