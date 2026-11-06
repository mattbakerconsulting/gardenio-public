/* The gardeners' transitions under node, run by TestTransitionsUnderNode (transitions_test.go, which says what it covers):
     node transitions_test.js <web dir>
   It prints a JSON report on its last line, as nodeScript reads. */
var path = require("path"), web = function (f) { return path.join(process.argv[2], f); };
var W = require(web("world.js")), A = require(web("art.js")), assert = require("assert");
global.ART = A; var M = require(web("transitions.js")), LN = require(web("lanes.js"));
function plan(id, project, n) {
  var tasks = []; for (var i = 0; i < n; i++) tasks.push({ id: id + "-t" + i, title: "T" + i, type: "task", state: "in_progress", updated: 1 });
  return { id: id, title: "Plan " + id, project: project, created: 1, activity: 1, counts: { in_progress: n },
    phases: [{ id: id + "-p1", title: "Phase 1", counts: { in_progress: n }, tasks: tasks }], unphased: { id: "", counts: {}, tasks: [] } };
}
var fill = []; for (var fi = 0; fi < 5; fi++) { fill.push(plan("pf" + fi, "gardenio", 1)); fill[fi].created = 2 + fi; }
var pbp = plan("pb", "gardenio", 3); pbp.created = 9;   // pa in slot 1, east of the farmstead; pb in slot 7, north of it (29.1)
var doc = { plans: [plan("pa", "gardenio", 8)].concat(fill, [pbp]), seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } };
var w = W.layout(doc, null, { now: 1, readonly: true });
function g(id, state, bucket, task, plot) { return { id: id, agent: "name-" + id, role: "crew", project: "gardenio", state: state, bucket: bucket, task: task, plot: plot ? { plan: plot, phase: "" } : null, since: 1, href: "/garden?bead=" + id }; }
function st(list) { return W.gardenerState(w, list); }
var T0 = 1792000000000, S = T0 / 1000, all = { x: -1e6, y: -1e6, w: 2e6, h: 2e6 };
function run(a, b, exits, o) { o = Object.assign({ level: 3, view: all, home: {}, t: T0, nowS: S }, o || {}); return M.plan(st(a), st(b), exits, o); }
function one(r, kind) { assert.strictEqual(r.list.length, 1, JSON.stringify(r.list)); assert.strictEqual(r.list[0].kind, kind); return r.list[0]; }
var w1 = g("w1", "working", "alive", "pa-t3", "pa"), bio = w.index["project:gardenio"], bk = bio.home.keep.barn, barn = M.door(st([w1]), "w1");
assert.deepStrictEqual([barn, M.home(st([w1]), "w1")], [W.barnDoor(w, "project:gardenio"), barn].map(function (d) { return { x: d.x, y: d.y }; }), "its own project's barn door, also its home tile while out (28.4)");
function inStrip(p, b) { var h = b.home; return p.x >= h.x * 16 && p.x + 16 <= (h.x + h.w) * 16 && p.y >= h.y * 16 && p.y + 24 <= (h.y + h.h) * 16; }
function wayLen(pts) { var n = 0; for (var i = 1; i < pts.length; i++) n += Math.hypot(pts[i].x - pts[i - 1].x, pts[i].y - pts[i - 1].y); return n; }
// what a way (figure top lefts) crosses under the figure's feet, in order: the farmstead, a lane, the plot gap on the farm's
// side of plot p (only for a plot outside the three middle rows), plot p; anything else is named with its tile (29.3)
function legs(b, p, pts) {
  var q = W.SPIRAL[p.slot], gap = { x: b.foot.x, y: q.r > 0 ? p.y - 3 : p.y + p.h, w: b.foot.w, h: 3 }, out = [];
  var ins = function (r, x, y) { return x >= r.x && x < r.x + r.w && y >= r.y && y < r.y + r.h; };
  var put = function (fx, fy) {
    var x = Math.floor((fx + 8) / 16), y = Math.floor((fy + 23) / 16);
    var c = ins(p, x, y) ? "plot" : ins(b.home, x, y) ? "farm" : b.lanes.some(function (l) { return ins(l, x, y); }) ? "lane" : Math.abs(q.r) > 1 && ins(gap, x, y) ? "gap" : "grass@" + x + "," + y;
    if (out[out.length - 1] !== c) out.push(c);
  };
  for (var i = 1; i < pts.length; i++) for (var k = 0; k < 64; k++) put(pts[i - 1].x + (pts[i].x - pts[i - 1].x) * k / 64, pts[i - 1].y + (pts[i].y - pts[i - 1].y) * k / 64);
  put(pts[pts.length - 1].x, pts[pts.length - 1].y);
  return out;
}

// absent -> on a plot (it started) with no field stand drawn (past the field's "+N"): a truck from the barn door to its spot;
// the static sprite waits for it, and it has no link. It drives the lanes (29.3, X-D11): down into the bay, west out of it onto the west lane, along the lanes and in at its column
var r = run([], [g("w1", "spawning", "alive", "pa-t3", "pa")]), tr = one(r, "ride"), spot = st([w1])[3].spots.w1;
assert.deepStrictEqual(tr.from, barn); assert.strictEqual(tr.to.x, spot.x); assert.strictEqual(tr.to.y, spot.y);
assert.strictEqual(tr.moving, true); assert.strictEqual(tr.t0, T0); assert.deepStrictEqual(r.changed, ["w1"]);
assert.deepStrictEqual([tr.pts[0], tr.pts[tr.pts.length - 1]], [barn, { x: spot.x, y: spot.y }], "from the barn door to its spot");
var bay = bio.home.bay, wl = bio.lanes.filter(function (l) { return l.name === "west"; })[0];
assert.deepStrictEqual(tr.pts.slice(1, 3), [{ x: barn.x, y: (bay.y + bay.h / 2) * 16 - 24 }, { x: (wl.x + 1.5) * 16 - 8, y: (bay.y + bay.h / 2) * 16 - 24 }], "into the bay, then west out of it onto the west lane's middle");
assert.deepStrictEqual(legs(bio, w.index.pa, tr.pts), ["farm", "lane", "plot"], "on the lanes from the farm to plot A, which a lane runs beside: " + JSON.stringify(tr.pts));
assert.strictEqual(tr.pts[tr.pts.length - 2].x, spot.x, "it turns in at its spot's column");
var d = wayLen(tr.pts);
assert.strictEqual(M.SPEED, 100, "a leisurely 100 world px per s"); assert.deepStrictEqual(M.RIDE, [2500, 7000]);
assert.strictEqual(tr.dur, Math.max(2500, Math.min(7000, d / 100 * 1000)), "the way's length / 100 px per s, clamped to 2.5-7 s");
var mid = M.at(tr, T0 + tr.dur / 2);
assert.strictEqual(mid.done, false); assert.strictEqual(mid.hide, true);
assert.deepStrictEqual(mid.ops.map(function (o) { return o.s; }), ["truck"]);
assert.strictEqual(mid.ops[0].x, Math.round(M.along(tr.pts, 0.5).x) - 4, "halfway along the lanes, the truck under the figure's feet");
assert.strictEqual(mid.ops[0].dir, M.heading(tr.pts, 0.5) < 0 ? "l" : "r");
assert.strictEqual(M.at(tr, T0 + tr.dur * 0.02).ops[0].dir, "l", "pulling west out of the bay, it faces west");
var ridePts = tr.pts;
assert.deepStrictEqual(M.at(tr, T0 + Math.ceil(tr.dur)), { done: true, hide: false, ops: [] }, "landed: the static sprite takes over");   // ceil: T0 + a fractional dur rounds below it
// at home -> on a plot (it went to work): the same ride
one(run([g("w1", "idle", "idle", "pa-t3", "pa")], [w1]), "ride");
// on plot A -> plot B (its task moved): it walks there along the paths, and it changed (a transition running for it is cut)
var w1b = g("w1", "working", "alive", "pb-t0", "pb"), sa = st([w1]), sb = st([w1b]), pa = w.index.pa, pb = w.index.pb;
r = run([w1], [w1b]); tr = one(r, "walkto"); assert.deepStrictEqual(r.changed, ["w1"]);
var a3 = sa[3].spots.w1, b3 = sb[3].spots.w1, la = sa[3].plots.pa.lane, lb = sb[3].plots.pb.lane;
assert.ok(pb.x < pa.x && pb.y < pa.y, "the fixture's plot B is up and to the left of A: " + JSON.stringify([pa.x, pa.y, pb.x, pb.y]));
var gap = (W.SLOT.dx - W.SLOT.w) / 2;
assert.deepStrictEqual(la, { y: 16 * (pa.y + W.SLOT.h + gap) - 24, l: 16 * (pa.x - gap) - 8, r: 16 * (pa.x + W.SLOT.w + gap) - 8, project: "gardenio" }, "the lane: the middle of the 3-tile paths under and beside the plot");
assert.strictEqual(la.l, lb.r, "two neighbours share the path between them");
assert.deepStrictEqual(tr.pts, [{ x: a3.x, y: a3.y }, { x: a3.x, y: la.y }, { x: la.l, y: la.y }, { x: la.l, y: lb.y }, { x: b3.x, y: lb.y }, { x: b3.x, y: b3.y }],
  "off the fence, along the path under A, up the path on A's west side (B is west), along the path under B, onto B's fence");
assert.deepStrictEqual(tr.from, tr.pts[0]); assert.deepStrictEqual(tr.to, tr.pts[5]);
var len = 0; for (var i = 1; i < tr.pts.length; i++) len += Math.hypot(tr.pts[i].x - tr.pts[i - 1].x, tr.pts[i].y - tr.pts[i - 1].y);
assert.deepStrictEqual(M.WALK, [1000, 7000]); assert.strictEqual(tr.dur, Math.max(1000, Math.min(7000, len / 100 * 1000)), "the route's length at 100 px per s, clamped to 1-7 s");
assert.strictEqual(tr.moving, true, "it has no link until it lands");
f = M.at(tr, T0 + tr.dur / 2); assert.strictEqual(f.hide, true, "it stands in for its sprite on B"); assert.deepStrictEqual(f.ops.map(function (o) { return o.s; }), ["walk"]);
var p = M.along(tr.pts, 0.5); assert.strictEqual(f.ops[0].x, Math.round(p.x)); assert.strictEqual(f.ops[0].y, Math.round(p.y));
f = M.at(tr, T0 + 1); assert.strictEqual(f.ops[0].x, a3.x, "it steps straight down off the fence first"); assert.ok(f.ops[0].y >= a3.y);
assert.deepStrictEqual(M.at(tr, T0 + Math.ceil(tr.dur)), { done: true, hide: false, ops: [] }, "landed: its sprite on B takes over");
// another tile of the same plot: a short walk across the field
tr = one(run([w1], [g("w1", "working", "alive", "pa-t5", "pa")]), "walkto"); var a5 = st([g("w1", "working", "alive", "pa-t5", "pa")])[3].spots.w1;
assert.notStrictEqual(a5.x, a3.x, "t5 has another column"); assert.deepStrictEqual(tr.pts, [{ x: a3.x, y: a3.y }, { x: a5.x, y: a5.y }]);
assert.strictEqual(tr.dur, Math.max(1000, Math.hypot(a5.x - a3.x, a5.y - a3.y) * 10), "at least a second");
// another row in the same column (gd-HB0xs0CTbk: a spot in the field moves down as well as across): a walk too, and the figure
// faces its task there (where() carries the spot's facing for the transitions' poses)
var w6 = g("w1", "working", "alive", "pa-t6", "pa"), a6 = st([w6])[3].spots.w1;
assert.strictEqual(a6.x, a3.x, "t6 is under t3, so the tile beside it is under w1's"); assert.notStrictEqual(a6.y, a3.y);
tr = one(run([w1], [w6]), "walkto"); assert.deepStrictEqual(tr.pts, [{ x: a3.x, y: a3.y }, { x: a6.x, y: a6.y }]);
assert.strictEqual(M.where(st([w1]), 3, "w1").face, a3.face); assert.ok(a3.face === "l" || a3.face === "r");
// down on A, then on B: no walk (a scarecrow does not walk); off the padded viewport: a cut
assert.deepStrictEqual(run([g("w1", "failed", "stuck", "pa-t3", "pa")], [g("w1", "failed", "stuck", "pb-t0", "pb")]).list, []);
assert.deepStrictEqual(run([w1], [w1b], [], { view: { x: 1e5, y: 1e5, w: 100, h: 100 } }).list, [], "a walk out of view: a cut");
// the route in a bigger world: a plot straight above uses the path on A's east side; another project's plot, straight
var pcp = plan("pc", "gardenio", 2); pcp.created = 10;   // slot 8, north-east of the farmstead: straight above pa
var w2 = W.layout({ plans: doc.plans.concat([pcp, plan("pz", "zeta", 2)]), seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } }, null, { now: 1, readonly: true });
var at2 = function (task, plot) { return M.where(W.gardenerState(w2, [g("w1", "working", "alive", task, plot)]), 3, "w1"); };
var A2 = at2("pa-t3", "pa"), C2 = at2("pc-t0", "pc"), Z2 = at2("pz-t0", "pz");
assert.strictEqual(w2.index.pc.x, w2.index.pa.x, "pc is in pa's column"); assert.ok(w2.index.pc.y < w2.index.pa.y, "above it");
assert.deepStrictEqual(M.route(A2, C2).map(function (q) { return q.x; }), [A2.x, A2.x, A2.lane.r, A2.lane.r, C2.x, C2.x], "up the east path");
assert.deepStrictEqual(M.route(A2, Z2), [{ x: A2.x, y: A2.y }, { x: Z2.x, y: Z2.y }], "another project's paths do not line up: straight");
var hz = M.route(at2("pb-t0", "pb"), A2); assert.strictEqual(hz.length, 6);
// nothing changed: nothing at all
r = run([w1], [w1]); assert.deepStrictEqual(r.list, []); assert.deepStrictEqual(r.changed, []);

// alive -> stuck on its plot: a burst, the scarecrow rising in its place, then the flight back and a puff at the barn
tr = one(run([w1], [g("w1", "failed", "stuck", "pa-t3", "pa")]), "burst");
assert.strictEqual(tr.stuck, true); assert.strictEqual(tr.moving, false, "the scarecrow is its sprite: it keeps its link");
assert.ok(tr.arc >= 2500 && tr.arc <= 4500, "the flight is 2.5 to 4.5 s"); assert.strictEqual(tr.dur, M.BURST + tr.arc + M.PUFF);
var f = M.at(tr, T0 + 0.5 * M.BURST);
assert.strictEqual(f.hide, true); assert.strictEqual(f.ops[0].s, "pose"); assert.strictEqual(f.ops[0].pose, "down");
assert.strictEqual(f.ops[0].face, spot.face, "the scarecrow faces the task its spot faces");
var parts = f.ops.filter(function (o) { return o.s === "part"; });
assert.strictEqual(parts.length, 8, "eight particles"); assert.deepStrictEqual(parts.map(function (o) { return o.c; }).sort(), ["H", "H", "H", "H", "S", "S", "S", "S"], "in hat and shirt colours");
f = M.at(tr, T0 + M.BURST + 0.6 * tr.arc);
assert.strictEqual(f.hide, false, "the scarecrow is up"); assert.deepStrictEqual(f.ops.map(function (o) { return o.s; }), ["fly"]);
var lx = spot.x + (barn.x - spot.x) * 0.6, ly = spot.y + (barn.y - spot.y) * 0.6;
assert.strictEqual(f.ops[0].x, Math.round(lx)); assert.ok(f.ops[0].y < ly - 20, "it arcs over the straight line");
assert.strictEqual(M.at(tr, T0 + M.BURST + tr.arc + 10).ops[0].s, "puff", "and puffs out at the barn");
// stuck -> alive: the scarecrow drops and the person stands up, in place
tr = one(run([g("w1", "failed", "stuck", "pa-t3", "pa")], [w1]), "stand");
assert.strictEqual(M.at(tr, T0 + 10).ops[0].pose, "down"); assert.strictEqual(M.at(tr, T0 + tr.dur - 10).ops[0].pose, "work"); assert.strictEqual(M.at(tr, T0).hide, true);
assert.strictEqual(M.at(tr, T0 + tr.dur - 10).ops[0].face, spot.face, "standing up facing its task");
// on a plot -> home (idle), no stand drawn: the truck back to its barn's door; it has no barn slot until it lands
tr = one(run([w1], [g("w1", "idle", "idle", "pa-t3", "pa")]), "rideback");
assert.deepStrictEqual(tr.to, barn); assert.strictEqual(tr.moving, true); assert.strictEqual(M.at(tr, T0 + 1).hide, false);
assert.deepStrictEqual(tr.pts, ridePts.slice().reverse(), "home the way it came");

// a plot outside the three middle rows (ring 2: slot 9, north-west of the farm's corner): along the west lane to the plot gap
// below it, along that gap to the spot's column and up; the walk home the same way
var ring2 = []; for (var ri = 0; ri < 9; ri++) { ring2.push(plan("r" + ri, "gardenio", 1)); ring2[ri].created = 20 + ri; }
var w3 = W.layout({ plans: doc.plans.concat(ring2), seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } }, null, { now: 1, readonly: true });
var far = w3.biomes[0].plots.filter(function (p) { return Math.abs(W.SPIRAL[p.slot].r) === 2; })[0], b3 = w3.index["project:gardenio"];
assert.ok(far, "the fixture fills ring 2");
var gf = g("w9", "working", "alive", far.id + "-t0", far.id), s3 = W.gardenerState(w3, [gf]), sp9 = s3[3].spots.w9;
tr = one(M.plan(W.gardenerState(w3, []), s3, [], { level: 3, view: all, t: T0, nowS: S }), "ride");
assert.deepStrictEqual(legs(b3, far, tr.pts), ["farm", "lane", "gap", "plot"], "the lanes until the last plot gap: " + JSON.stringify(tr.pts));
assert.deepStrictEqual(legs(b3, far, LN.walkLanes(w3, "project:gardenio", W.homeTile(w3, gf), far.id, sp9)), ["farm", "lane", "gap", "plot"], "walking, too");
// between biomes (a gardener whose home is another project's farm) the ride goes straight, as before
assert.strictEqual(LN.rideLanes(w2, "project:gardenio", barn, "pz", Z2), null, "plot pz is zeta's, not gardenio's: no lane way");

// exits: finished walks home with its tldr; interrupted and parked burst, with no scarecrow left
var ex = function (kind, at) { return [{ id: "w1", agent: "name-w1", kind: kind, tldr: "Merged MR 3286", plot: { plan: "pa", phase: "" }, at: at || S }]; };
tr = one(run([w1], [], ex("finished")), "walk");
assert.strictEqual(tr.tldr, "Merged MR 3286"); assert.deepStrictEqual(tr.to, barn);
assert.deepStrictEqual(legs(bio, w.index.pa, tr.pts), ["plot", "lane", "farm"], "out of plot A at its column, the lanes, in at a gate: " + JSON.stringify(tr.pts));
var gt = tr.pts[tr.pts.length - 3], cr = bio.home.cross;
assert.ok((gt.x + 8) / 16 === cr.x + 0.5 || (gt.y + 24) / 16 === cr.y + 0.5, "the gate is the end of a cross arm: " + JSON.stringify(gt));
assert.deepStrictEqual(tr.pts, LN.walkLanes(w, "project:gardenio", barn, "pa", spot).reverse(), "home the way a walk out goes, reversed");
assert.strictEqual(tr.dur, Math.max(2500, Math.min(7000, wayLen(tr.pts) * 10)));
assert.ok(inStrip(tr.to, bio) && inStrip(tr.to, { home: bio.home.cells.barn }), "a walk from its plot ends inside its project's home strip, at its barn");
f = M.at(tr, T0 + tr.dur / 2); assert.deepStrictEqual(f.ops.map(function (o) { return o.s; }), ["walk"]);
assert.notStrictEqual(M.at(tr, T0).ops[0].f, M.at(tr, T0 + M.STEP).ops[0].f, "two walking frames");
["interrupted", "parked"].forEach(function (k) { var b = one(run([w1], [], ex(k)), "burst"); assert.strictEqual(b.stuck, false); assert.strictEqual(M.at(b, T0 + 10).hide, false); });
assert.deepStrictEqual(run([w1], [], []).list, [], "an id dropped with no exit (past the cap) only leaves");
assert.deepStrictEqual(run([w1], [], ex("finished", S - 11)).list, [], "an exit more than 10 s old does not animate");
// a home gardener's exit: a puff where its figure stood at its field stand (WORLD.homeFill's), its home tile
var idle = g("i1", "idle", "idle", "", null);
W.homeFill(w, { agent: W.gardenerField(st([idle]), null, "", 0) }, {});
var fi = w.index[W.FIELD + "i1"]; tr = one(run([idle], [], [{ id: "i1", kind: "interrupted", at: S }]), "puff"); assert.deepStrictEqual(tr.from, { x: (fi.x + fi.w - 1) * 16, y: fi.y * 16 });
W.homeFill(w, {}, {});
assert.deepStrictEqual(run([idle], [], [{ id: "i1", kind: "interrupted", at: S }]).list, [], "no figure drawn: nothing");
// absent -> home: it just appears at its stand
assert.deepStrictEqual(run([], [idle]).list, []);

// with its field stand drawn (WORLD.homeFill's: garden.js places the new stands before it plans, 28.12): it walks out of its
// stand to its plot along the lanes, back to it when it goes idle, home to it with its tldr when it finishes, and its
// fly-back aims at it
W.homeFill(w, { agent: W.gardenerField(st([w1]), null, "", 0) }, {});
var sw = W.homeTile(w, w1), fs = { x: sw.x, y: sw.y }, fz = { home: bio.home.cells.growing };
assert.ok(sw.drawn && inStrip(fs, fz), "w1's stand is in its project's Growing field: " + JSON.stringify(sw));
tr = one(run([], [g("w1", "spawning", "alive", "pa-t3", "pa")]), "walkout");
assert.deepStrictEqual(tr.from, fs, "a spawn walks from its stand"); assert.deepStrictEqual([tr.to.x, tr.to.y], [spot.x, spot.y], "to its spot");
assert.deepStrictEqual(tr.pts, LN.walkLanes(w, "project:gardenio", fs, "pa", spot), "out of the farm by a gate, along the lanes, in at its column");
assert.deepStrictEqual(legs(bio, w.index.pa, tr.pts), ["farm", "lane", "plot"], "on the lanes from the farm to plot A: " + JSON.stringify(tr.pts));
assert.strictEqual(tr.dur, Math.max(2500, Math.min(7000, wayLen(tr.pts) * 10)));
assert.strictEqual(tr.moving, true, "it has no link until it lands");
f = M.at(tr, T0 + tr.dur / 2); assert.strictEqual(f.hide, true, "its sprite on the plot waits for it"); assert.deepStrictEqual(f.ops.map(function (o) { return o.s; }), ["walk"]);
assert.deepStrictEqual(M.at(tr, T0).ops[0], { s: "walk", x: fs.x, y: fs.y, f: 0 }, "it steps off its stand");
one(run([g("w1", "idle", "idle", "pa-t3", "pa")], [w1]), "walkout");
tr = one(run([w1], [g("w1", "idle", "idle", "pa-t3", "pa")]), "walk");
assert.deepStrictEqual(tr.to, fs, "gone idle: it walks back to its stand"); assert.deepStrictEqual(tr.pts, LN.walkLanes(w, "project:gardenio", fs, "pa", spot).reverse(), "the way it came");
assert.strictEqual(tr.tldr, undefined, "no tldr: it did not finish"); assert.strictEqual(M.at(tr, T0 + 1).hide, false);
tr = one(run([w1], [], ex("finished")), "walk");
assert.deepStrictEqual(tr.to, fs, "finished: home to its stand, where its tldr label lands"); assert.strictEqual(tr.tldr, "Merged MR 3286");
assert.deepStrictEqual(legs(bio, w.index.pa, tr.pts), ["plot", "lane", "farm"]);
tr = one(run([w1], [g("w1", "failed", "stuck", "pa-t3", "pa")]), "burst"); assert.deepStrictEqual(tr.to, fs, "down: the fly-back aims at its stand");
tr = one(run([w1], [], ex("interrupted")), "burst"); assert.deepStrictEqual(tr.to, fs, "interrupted: the same");
// to a plot of another biome: a truck leaves the barn's door (the truck is the Barn's), picks it up at its stand and drives it
// there, straight; back the same way, dropping it at its stand on the way to the barn
var gz = g("w1", "working", "alive", "pz-t0", "pz"), z0 = W.gardenerState(w2, [gz]), zi = W.gardenerState(w2, [g("w1", "idle", "idle", "pz-t0", "pz")]);
W.homeFill(w2, { agent: W.gardenerField(z0, null, "", 0) }, {});
var sz = W.homeTile(w2, gz), dz = M.door(z0, "w1"), Zs = { x: Z2.x, y: Z2.y };
assert.ok(sz.drawn && sz.biome === "project:gardenio", "its stand is in its own project's field, not zeta's");
tr = one(M.plan(W.gardenerState(w2, []), z0, [], { level: 3, view: all, t: T0, nowS: S }), "ride");
assert.deepStrictEqual(tr.from, dz, "the truck leaves the Barn's door"); assert.deepStrictEqual(tr.pts, [dz, { x: sz.x, y: sz.y }, Zs], "to its stand, then to its spot");
assert.strictEqual(M.at(tr, T0 + 1).hide, true);
tr = one(M.plan(z0, zi, [], { level: 3, view: all, t: T0, nowS: S }), "rideback");
assert.deepStrictEqual(tr.to, dz); assert.deepStrictEqual(tr.pts, [Zs, { x: sz.x, y: sz.y }, dz], "back by its stand to the barn");
W.homeFill(w, {}, {}); W.homeFill(w2, {}, {});

// rules 2 and 3: not below the Plan level, not off the padded viewport, not in a patch of more than eight
assert.deepStrictEqual(run([], [w1], [], { level: 1 }).list, []);
assert.strictEqual(run([], [w1], [], { level: 2 }).list.length, 1, "at Plan");
assert.deepStrictEqual(run([], [w1], [], { view: { x: 1e5, y: 1e5, w: 100, h: 100 } }).list, [], "off screen: a cut");
var nine = []; for (var i = 0; i < 9; i++) nine.push(g("n" + i, "working", "alive", "pa-t" + (i % 8), "pa"));
W.GARDENER_CAP[3] = 16;
assert.deepStrictEqual(run([], nine).list, [], "nine at once (a fleet restart): the end state");
assert.strictEqual(run([], nine.slice(0, 8)).list.length, 8);
W.GARDENER_CAP[3] = 6;
// one behind "+N" was never drawn there: no ride to it. The cap holds those not on a task cell (the task is the plan); one on
// its own task is always drawn (homestead-agents-as-people.md 4.2), so the seventh of those rides in
var seven = []; for (i = 0; i < 7; i++) seven.push(g("f" + i, "working", "alive", "pa", "pa")); assert.strictEqual(run(seven.slice(0, 6), seven).list.length, 0);
assert.strictEqual(run(nine.slice(0, 6), nine.slice(0, 7)).list.length, 1, "the seventh on its task: drawn, so it rides in");
console.log(JSON.stringify({ ok: true }));
