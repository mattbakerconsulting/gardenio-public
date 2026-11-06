/* The release truck's pure part (web/truck.js, gd-yOOVahJMCh, step 7 of plan gd-UZKZ9H25UD), run by TestTruckUnderNode:
     node truck_test.js <web dir>
   TRUCK.norm reads the farm document's releases blob in both shapes the plan fixes (step 5's latest/deployments/recent/shipping
   and releases-design.md 3.5's newest/targets/recent tags), keyed like the biomes, shipping from the status when the blob does
   not say, a project with no tag none; bayOf is the 6 x 4 tile bay (the truck S 3, 72 x 48 world px, standing centred on its floor; gd-Ir8jxl4c57) under the Barn's keep (28.10), clear of
   the sign, the steward's tile and the Pen; place gives one truck per project on the map with release:<tag> (release:<key>/<tag> on a
   clash) and a road off its biome's east edge, past the Pen (28.10), on the lanes (29.3): west out of the bay, north up the
   west lane, east along the north lane, its middle on a lane tile the whole way out and back; load is a crate per plan, most PRs first, then the unplanned, at most MAX_CRATES;
   depart drives a loading truck out when its release goes live (never on the first document) and names the targets; pose
   drives out along that path, is away, drives back the same way reversed and parks, and under reduced motion stands still, its board up while it loads or parks (gd-rSz81Me1Pe);
   tagFit keeps the tail of whole parts that fits; shippedOf reads FarmPlan.shipped, the blob's map or a produce row and names
   the targets running that release or a newer one, or FarmPlan.boarded's open release ("on the next release (open)"); signWords and card say it in words, "No deployments yet" when nothing runs
   it. Then the page side under a recording canvas: nothing below the Plan level, the truck in view with its crates, the
   plate and the tag, true while it drives, world.index's release:<tag>, the signs' words; and ART's crate, shipped stamp and
   legend glyph. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var on = {};
global.document = { addEventListener: function (k, f) { (on[k] = on[k] || []).push(f); } };
function fire(k, d) { (on[k] || []).forEach(function (f) { f({ detail: d }); }); }
var WORLD = require(path.join(process.argv[2], "world.js")), ART = require(path.join(process.argv[2], "art.js")), TRUCK = require(path.join(process.argv[2], "truck.js"));
var farm = require(path.join(__dirname, "farm_fixture.json")), fx = require(path.join(__dirname, "releases_fixture.json"));
var T = WORLD.TILE, report = {};

/* ---------- 1. norm: both shapes ---------- */
var a = TRUCK.norm(fx.shipping).gardenio;
assert.deepStrictEqual([a.tag, a.status, a.shipping, a.count, a.unplanned, a.total, a.id], ["2026.299.2", "built", true, 14, 2, 3, "gd-RelFix0002"]);
assert.deepStrictEqual(a.plans.map(function (p) { return p.id; }), ["gd-planA", "gd-planP", "gd-planB"], "most PRs first");
assert.deepStrictEqual(a.targets, [{ name: "prod", tag: "2026.299.1", since: 1791750800, revision: "411" }]);
var b = TRUCK.norm({ projects: [{ project: "Gardenio", newest: { tag: "v1.2.0", status: "live", cut_at: "2026-10-26T05:00:00Z", plans: { "gd-x": 2, "gd-y": 5 } },
  targets: [{ name: "staging" }, { name: "prod", until: "2026-10-27T00:00:00Z" }], recent: ["v1.2.0", "v1.1.0"] }] }).gardenio;
assert.ok(b, "an array of projects, keyed by WORLD.projectKey");
assert.deepStrictEqual([b.tag, b.shipping, b.count, b.cut, b.total], ["v1.2.0", false, 7, Date.parse("2026-10-26T05:00:00Z") / 1000, 2], "count from the plans; a time in RFC 3339");
assert.deepStrictEqual(b.plans.map(function (p) { return p.id; }), ["gd-y", "gd-x"]);
assert.deepStrictEqual(b.targets.map(function (d) { return d.name + "=" + d.tag; }), ["staging=v1.2.0"], "a closed row (until) runs nothing; a row with no tag runs the live release");
assert.deepStrictEqual(Object.keys(TRUCK.norm({ a: { latest: { status: "cut" } }, b: {} })), [], "no tag, no truck");
assert.strictEqual(TRUCK.norm({ x: { latest: { tag: "1", status: "cut" } } }).x.shipping, true, "cut is shipping");
assert.strictEqual(TRUCK.norm({ x: { latest: { tag: "1", status: "superseded" } } }).x.shipping, false);
assert.deepStrictEqual(TRUCK.norm(null), {});

/* ---------- 2. the bay, the trucks ---------- */
var world = WORLD.layout(farm, null, { readonly: true }), bio = world.index["project:gardenio"], bay = TRUCK.bayOf(bio), st = WORLD.stewardSpot(bio);
function apartRows(r, q) { return r.y + r.h <= q.y || q.y + q.h <= r.y; }
function apart(r, q) { return r.x + r.w <= q.x || q.x + q.w <= r.x || r.y + r.h <= q.y || q.y + q.h <= r.y; }
assert.deepStrictEqual([TRUCK.S, TRUCK.K, TRUCK.TW, TRUCK.TH, bay.w, bay.h], [3, 1.5, 72, 48, 6, 4], "a 72 x 48 world px truck in the 6 x 4 tile bay (gd-Ir8jxl4c57)");
assert.ok(TRUCK.TW < bay.w * T && TRUCK.TH < bay.h * T, "the box fits inside the bay");
var barn = bio.home.cells.barn;
assert.deepStrictEqual([bay.x, bay.y], [barn.x, bio.home.keep.barn.y + bio.home.keep.barn.h], "under the Barn's keep, at the cell's west edge (28.10, 28.12)");
assert.ok(bay.x >= bio.home.x && bay.y >= bio.home.y && bay.x + bay.w <= bio.home.x + bio.home.w && bay.y + bay.h <= bio.home.y + bio.home.h, "inside the farmstead");
assert.ok(apart(bay, bio.sign) && apart(bay, bio.home.cells.pen), "clear of the sign and the Pen");
assert.ok(apart(bay, { x: st.col, y: st.row - 1, w: 1, h: 2 }), "clear of the steward");
var rels = TRUCK.norm(fx.shipping), trucks = TRUCK.place(rels, world), tr = trucks[0];
assert.strictEqual(trucks.length, 1);
assert.deepStrictEqual([tr.id, tr.key, tr.x, tr.y, tr.dir, tr.crates.length], ["release:2026.299.2", "gardenio", bay.x * T + 12, (bay.y + bay.h) * T - TRUCK.TH, 1, 4], "parked centred on its bay's floor, facing east");
assert.ok(tr.road >= (bio.x + bio.w) * T, "the road ends off its biome's east edge (28.10)");
assert.ok(tr.road >= (bio.home.cells.pen.x + bio.home.cells.pen.w) * T, "east past the Pen");
/* the drive on the lanes (29.3, X-D11): the truck's middle at a moment, tiles; on a lane tile, or in the bay's row between the
   bay and the west lane, or past the foot's east edge, off the region */
var lane = function (n) { return bio.lanes.filter(function (l) { return l.name === n; })[0]; }, inR = function (r, p) { return p.x >= r.x && p.x < r.x + r.w && p.y >= r.y && p.y < r.y + r.h; };
var mid = function (q) { return { x: (q.x + TRUCK.TW / 2) / T, y: (q.y + TRUCK.TH / 2) / T }; }, pull = { x: lane("west").x, y: bay.y, w: bay.x + bay.w - lane("west").x, h: bay.h };
var where = function (q) { var m = mid(q); return m.x >= bio.x + bio.w ? "off" : bio.lanes.some(function (l) { return inR(l, m); }) ? "lane" : inR(pull, m) ? "bay" : "grass"; };
assert.deepStrictEqual(tr.path[0], { x: tr.x, y: tr.y }, "it sets off from the bay");
assert.deepStrictEqual(tr.path.slice(1).map(where), ["lane", "lane", "off"], "west onto the west lane, north to the crossroads, east off the region: " + JSON.stringify(tr.path));
assert.deepStrictEqual([mid(tr.path[1]).x, mid(tr.path[2]).x, mid(tr.path[2]).y, mid(tr.path[3]).y], [lane("west").x + 1.5, lane("west").x + 1.5, lane("north").y + 1.5, lane("north").y + 1.5], "down the middle of the west lane, then of the north lane");
assert.ok(tr.path[1].x < tr.x && tr.path[2].y < tr.y && tr.path[3].x > tr.path[2].x, "west, north, then east");
for (var k = 0; k <= 200; k++) assert.notStrictEqual(where(TRUCK.on(tr.path, k / 200)), "grass", "on the lanes the whole way: " + k);
assert.deepStrictEqual(TRUCK.place({ nobody: a }, world), [], "a project not on the map has no truck");
var clash = TRUCK.place({ gardenio: a, music: Object.assign({}, a, { key: "music" }) }, { biomes: [bio, Object.assign({}, bio, { key: "music" })], bbox: world.bbox });
assert.deepStrictEqual(clash.map(function (t) { return t.id; }), ["release:2026.299.2", "release:music/2026.299.2"], "a tag two projects share");

/* ---------- 3. load ---------- */
assert.deepStrictEqual(TRUCK.load(a).map(function (c) { return c.id + ":" + c.count; }), ["gd-planA:6", "gd-planP:4", "gd-planB:2", ":2"], "a crate per plan, then the unplanned");
var many = { plans: [], unplanned: 3 };
for (var i = 0; i < 12; i++) many.plans.push({ id: "p" + i, count: 1 });
assert.strictEqual(TRUCK.load(many).length, TRUCK.MAX_CRATES);
assert.deepStrictEqual(TRUCK.load({ plans: [], unplanned: 0 }), []);

/* ---------- 4. depart and pose ---------- */
var live = TRUCK.norm(fx.live);
var d0 = TRUCK.depart(null, live, 1000);
assert.deepStrictEqual([d0.gone, d0.state.gardenio.at], [[], -Infinity], "the first document drives nothing out");
var d1 = TRUCK.depart(null, rels, 1000), d2 = TRUCK.depart(d1.state, live, 5000);
assert.deepStrictEqual(d2.gone, [{ key: "gardenio", project: "gardenio", tag: "2026.299.2", targets: ["prod", "staging"] }], "loading, then live: it drives out");
assert.deepStrictEqual([d2.state.gardenio.at, d2.state.gardenio.cargo], [5000, 4]);
var d3 = TRUCK.depart(d2.state, live, 9000);
assert.deepStrictEqual([d3.gone, d3.state.gardenio.at], [[], 5000], "live, then live: nothing more");
assert.deepStrictEqual(TRUCK.depart(d1.state, rels, 9000).gone, [], "still loading");
var lt = TRUCK.place(live, world)[0], s2 = d2.state.gardenio, t0 = 5000, D = TRUCK.DRIVE, AW = TRUCK.AWAY, BK = TRUCK.BACK;
var pp = function (t, still) { return TRUCK.pose(lt, s2, t, still); };
assert.deepStrictEqual([pp(t0).phase, pp(t0).x, pp(t0).crates], ["out", lt.x, 4], "it sets off from the bay with its crates");
var ks = [0.04, 0.2, 0.75, 0.999].map(function (k) { return pp(t0 + D * k); });
assert.ok(ks[0].x < lt.x && ks[0].y === lt.y && ks[0].dir === -1, "it pulls west out of the bay: " + JSON.stringify(ks[0]));
assert.ok(ks[1].y < lt.y, "then north up the west lane: " + JSON.stringify(ks[1]));
assert.ok(ks[2].x > lt.x && ks[3].x > ks[2].x && ks[2].y === ks[3].y && ks[3].dir === 1, "then east along the north lane: " + JSON.stringify(ks.slice(2)));
for (k = 0; k <= 100; k++) { var q = pp(t0 + D * k / 100); assert.notStrictEqual(where(q), "grass", "out on the lanes: " + JSON.stringify(q)); q = pp(t0 + D + AW + BK * k / 100); assert.notStrictEqual(where(q), "grass", "and home the same way: " + JSON.stringify(q)); }
assert.deepStrictEqual([pp(t0 + D + 1).phase, pp(t0 + D + AW + 1).phase, pp(t0 + D + AW + 1).dir, pp(t0 + D + AW + 1).crates], ["away", "back", -lt.dir, 0], "away, then back the other way, empty");
assert.deepStrictEqual([pp(t0 + D + AW + BK).phase, pp(t0 + D + AW + BK).x, pp(t0 + D + AW + BK).y, pp(t0 + D + AW + BK).sign], ["park", lt.x, lt.y, true], "parked at the bay, its board up");
assert.strictEqual(pp(t0 + D + AW + BK * 0.999).dir, 1, "backing in east, as it stands");
assert.deepStrictEqual([pp(t0 + D / 2).sign, pp(t0 + D + AW + 1).sign], [false, false], "no board on the road");
assert.deepStrictEqual([pp(t0 + 10, true).phase, pp(t0 + 10, true).x, pp(t0 + 10, true).y, pp(t0 + 10, true).sign], ["park", lt.x, lt.y, true], "reduced motion: still at its bay, with its board");
assert.deepStrictEqual([TRUCK.pose(tr, d1.state.gardenio, 2000, true).phase, TRUCK.pose(tr, d1.state.gardenio, 2000, true).sign, TRUCK.pose(tr, d1.state.gardenio, 2000, false).crates], ["load", true, 4], "loading: its crates and its board");

/* ---------- 5. words ---------- */
assert.deepStrictEqual(["2026.299.2", "2026.299.12", "v1.20.3", "123456789"].map(function (s) { return TRUCK.tagFit(s, TRUCK.PLATE.w - 4); }), ["299.2", "12", "20.3", "6789"]);
assert.ok(TRUCK.textW("299.2") <= TRUCK.PLATE.w - 4 && TRUCK.textW("2026.299.2") > TRUCK.PLATE.w - 4);
var plan = { id: "gd-planA", project: "gardenio" };
assert.deepStrictEqual(TRUCK.shippedOf(plan, rels), { tag: "2026.299.1", targets: ["prod"] }, "the blob's map; prod runs it");
assert.deepStrictEqual(TRUCK.shippedOf(plan, live), { tag: "2026.299.2", targets: ["prod", "staging"] });
assert.deepStrictEqual(TRUCK.shippedOf({ id: "q", project: "gardenio", shipped: "2026.298.2" }, live), { tag: "2026.298.2", targets: ["prod", "staging"] }, "a newer release runs it too");
assert.deepStrictEqual(TRUCK.shippedOf({ id: "q", project: "gardenio", shipped: "2026.299.2" }, rels), { tag: "2026.299.2", targets: [] }, "built, not deployed: no targets");
assert.deepStrictEqual(TRUCK.shippedOf({ id: "q", project: "gardenio", produce: [{ state: "merged", release: "2026.298.2", updated: 2 }, { state: "merged", release: "", updated: 9 }, { state: "opened", release: "x", updated: 10 }] }, live).tag, "2026.298.2", "a produce row's release");
assert.strictEqual(TRUCK.shippedOf({ id: "q", project: "gardenio" }, rels), null);
assert.deepStrictEqual(TRUCK.shippedOf({ id: "q", project: "gardenio", boarded: true }, rels), { tag: "", open: true, targets: [] }, "on the open release: boarded, not shipped");
assert.deepStrictEqual([TRUCK.signWords(a), TRUCK.signWords(live.gardenio)], ["3 releases, 1 shipping", "3 releases"]);
var ago = function (s) { return Math.round((1792010000 - s) / 3600) + " hours"; };
var c = TRUCK.card(tr, "/b", ago), cl = TRUCK.card(lt, "/b", ago);
assert.strictEqual(c.title, "Release 2026.299.2");
assert.deepStrictEqual(c.lines.slice(0, 3).map(function (l) { return l[1]; }), ["Loading: Built, not deployed yet · cut 2 hours ago", "14 PRs from 3 plans, 2 unplanned", "prod runs 2026.299.1 since 72 hours ago"]);
assert.deepStrictEqual(cl.lines[2][1], "prod runs it since 1 hours ago · staging runs it since 2 hours ago");
assert.strictEqual(c.href, "/b/garden?release=gd-RelFix0002", "the release's changelog, inside /garden");
assert.deepStrictEqual(c.lines[3][1].map(function (k) { return k.text; }), ["Open the changelog"]);
assert.ok(/^Release 2026\.299\.2 of gardenio\. /.test(c.tip));
var none = TRUCK.place(TRUCK.norm({ gardenio: { latest: { tag: "v9", status: "cut", count: 1 } } }), world)[0];
assert.strictEqual(TRUCK.card(none, "", ago).lines[2][1], "No deployments yet", "an empty deployments list");
assert.strictEqual(TRUCK.card(none, "", ago).href, "/garden?at=gardenio/release%3Av9", "no bead id: the truck itself, never out of /garden");

/* the board over the cab: where it runs or its status, and since when; the door's PR count */
var NOW = 1792010000 * 1000;
assert.deepStrictEqual([TRUCK.board(a, NOW), TRUCK.board(live.gardenio, NOW)], ["BUILT 2H", "PROD+1 1H"], "loading: its status since cut; live: its first target, +1 more, since it went live");
assert.strictEqual(live.gardenio.live, 1792004600, "live since its earliest target's since when the blob has no live_since");
assert.strictEqual(TRUCK.norm({ x: { latest: { tag: "1", status: "live", live_since: 1792009000, built_at: 5 } } }).x.live, 1792009000);
assert.strictEqual(TRUCK.board(TRUCK.norm({ x: { latest: { tag: "1", status: "built", cut_at: 1, built_at: 1792009000 } } }).x, NOW), "BUILT 16M", "built: since it was built");
assert.strictEqual(TRUCK.board(TRUCK.norm({ x: { latest: { tag: "1", status: "live" } } }).x, NOW), "LIVE", "live with no target or time: the word alone");
assert.strictEqual(TRUCK.board(TRUCK.norm({ x: { latest: { tag: "1", status: "superseded", cut_at: 1 } } }).x, NOW), "SUPERSEDED", "superseded: no age");
var far = TRUCK.board(TRUCK.norm({ x: { latest: { tag: "1", status: "live" }, deployments: [{ target: "box-a-very-long-target-name", tag: "1", since: 1791000000 }] } }).x, NOW);
assert.ok(TRUCK.textW(far) <= TRUCK.BOARD && / 11D$/.test(far) && /^BOX-A/.test(far), "a long target is cut to the board, its age kept: " + far);
assert.deepStrictEqual([30, 3600, 3600 * 47, 86400 * 9].map(TRUCK.age), ["1M", "1H", "47H", "9D"]);
Object.keys(ART.DIGITS).forEach(function (d) { assert.strictEqual(TRUCK.FONT[d], ART.DIGITS[d].map(function (r) { return parseInt(r.replace(/k/g, "1").replace(/\./g, "0"), 2); }).join(""), "the digit " + d + " as ART.DIGITS draws it"); });
assert.ok("ABCDEFGHIJKLMNOPQRSTUVWXYZ+-".split("").every(function (ch) { return /^[0-7]{5}$/.test(TRUCK.FONT[ch]); }), "every capital for a target's name");

/* ---------- 6. the page's side ---------- */
function rec() {
  var c = { ops: [], fillStyle: "", globalAlpha: 1, fillRect: function (x, y, w, h) { c.ops.push({ c: c.fillStyle, x: x, y: y, w: w, h: h }); } };
  return c;
}
var view = { x: -1e5, y: -1e5, w: 2e5, h: 2e5 };
fire("farmdoc", Object.assign({}, farm, { releases: fx.shipping }));
var cx = rec();
assert.strictEqual(TRUCK.draw(cx, { level: 1, night: false, view: view, world: world, reduce: false }), false);
assert.strictEqual(cx.ops.length, 0, "nothing at the Project level");
assert.strictEqual(world.index["release:2026.299.2"], undefined, "not indexed until placed");
var hs = TRUCK.hots(world);
assert.deepStrictEqual(hs.map(function (h) { return [h.kind, h.id, h.lv, h.rect.x, h.rect.y]; }), [["truck", "release:2026.299.2", 12, bay.x, bay.y]], "a link at the Plan and Bead levels");
var ix = world.index["release:2026.299.2"];
assert.deepStrictEqual([ix.kind, ix.project, ix.x, ix.y, ix.w, ix.h], ["truck", "gardenio", bay.x, bay.y, bay.w, bay.h]);
var hit = WORLD.resolve(world, "gardenio/release:2026.299.2");
assert.ok(hit && hit.found && hit.kind === "truck" && hit.level === 2, "?at= finds it at the Plan level: " + JSON.stringify(hit));
TRUCK.pin(NOW);   // the board's age reads the page's clock
cx = rec();
assert.strictEqual(TRUCK.draw(cx, { level: 2, night: false, view: view, world: world, reduce: true }), false, "standing still");
var pal = ART.palette(false), K = TRUCK.K, inBox = cx.ops.filter(function (o) { return o.x >= tr.x && o.x + o.w <= tr.x + TRUCK.TW && o.y >= tr.y - TRUCK.SIGN.dy * K && o.y + o.h <= tr.y + TRUCK.TH; });
assert.strictEqual(inBox.length, cx.ops.length, "every fill inside the truck's box");
assert.ok(cx.ops.some(function (o) { return o.c === "#d94a3a"; }), "the red truck");
assert.strictEqual(cx.ops.filter(function (o) { return o.c === pal.B && o.w === 3 * K && o.h === K; }).length, 4, "four crates on its bed, K px per crate px");
var crates = cx.ops.filter(function (o) { return o.c === pal.k && o.w === TRUCK.CRATE * K && o.h === TRUCK.CRATE * K; });
assert.deepStrictEqual(crates.map(function (o) { return o.y - tr.y; }), [9 * K, 9 * K, 9 * K, 9 * K], "a row of 5 x 5 art px crates standing on the bed's line");
var plateOp = cx.ops.filter(function (o) { return o.c === pal.G; })[0];
assert.deepStrictEqual([plateOp.w, plateOp.h], [(TRUCK.PLATE.w - 2) * K, (TRUCK.PLATE.h - 2) * K], "the tag's plate, K px per art px");
assert.ok(cx.ops.some(function (o) { return o.c === pal.k && o.w === K && o.h === K && o.y >= tr.y + TRUCK.PLATE.y * K; }), "its letters at K px per letter px");
assert.strictEqual(cx.ops.filter(function (o) { return o.c === pal.G; }).length, 1, "loading: the tag's gold plate, the board paper");
var l = TRUCK.list();
assert.deepStrictEqual(l.map(function (d) { return [d.id, d.phase, d.crates, d.board, d.door]; }), [["release:2026.299.2", "load", 4, "BUILT 2H", "14"]], "its board and its door's count");
var door = cx.ops.filter(function (o) { return o.c === pal.j && o.w === K && o.h === K; });
assert.ok(door.length && door.every(function (o) { return o.y >= tr.y + TRUCK.DOOR.y * K && o.y < tr.y + (TRUCK.DOOR.y + 5) * K; }), "the PR count in paper on the cab's door");
var boardOp = cx.ops.filter(function (o) { return o.c === pal.j && o.h === (TRUCK.SIGN.h - 2) * K; })[0];
assert.ok(boardOp && boardOp.y === tr.y - (TRUCK.SIGN.dy - 1) * K, "a paper board over the cab while it loads");
assert.strictEqual(TRUCK.release("gd-RelFix0002").tag, "2026.299.2", "the newest release by its bead");
assert.strictEqual(TRUCK.release("gd-nope"), null);
cx = rec();
TRUCK.draw(cx, { level: 2, night: false, view: { x: tr.x + 1e4, y: tr.y, w: 10, h: 10 }, world: world, reduce: true });
assert.strictEqual(cx.ops.length, 0, "out of view, not drawn");
/* the release goes live: a toast, then a drive out at the Plan level, and the plate under reduced motion */
var toasts = [];
TRUCK.pin(1000);
TRUCK.wire({ toast: function (m) { toasts.push(m); } });
fire("farmdoc", Object.assign({}, farm, { releases: fx.live }));
assert.deepStrictEqual(toasts, ["Released 2026.299.2 to prod, staging (gardenio)"]);
cx = rec();
assert.strictEqual(TRUCK.draw(cx, { level: 2, night: false, view: view, world: world, reduce: false }), true, "it drives: the page asks for the next frame");
assert.strictEqual(TRUCK.list()[0].phase, "out");
TRUCK.pin(1000 + TRUCK.DRIVE / 2);
cx = rec();
TRUCK.draw(cx, { level: 3, night: true, view: view, world: world, reduce: false });
assert.ok(TRUCK.list()[0].x !== tr.x, "half way along");
cx = rec();
TRUCK.pin(NOW);
TRUCK.draw(cx, { level: 2, night: false, view: view, world: world, reduce: true });
assert.deepStrictEqual([TRUCK.list()[0].phase, TRUCK.list()[0].sign, TRUCK.list()[0].board], ["park", true, "PROD+1 1H"], "reduced motion: still, its board says where it runs");
assert.strictEqual(cx.ops.filter(function (o) { return o.c === pal.G; }).length, 2, "a live release's board is gold");
/* the signs */
var tree = { nodes: { "project:gardenio": { kind: "biome", key: "gardenio" }, "gd-planA": { kind: "plot", ref: { plan: { id: "gd-planA", project: "gardenio" } } } } };
var nd = tree.nodes;
assert.strictEqual(TRUCK.line(nd["project:gardenio"], 1, 40), "3 releases");
assert.strictEqual(TRUCK.line(nd["gd-planA"], 2, 40), "shipped in 2026.299.2 · prod, staging");
assert.strictEqual(TRUCK.line(nd["gd-planA"], 2, 28), "shipped in 2026.299.2", "past a phone's cap the targets go");
assert.strictEqual(TRUCK.line(nd["gd-planA"], 2, 18), "shipped in 299.2", "then the tag keeps its tail");
assert.strictEqual(TRUCK.line(nd["gd-planA"], 0, 40), "", "nothing at the World level");
assert.strictEqual(TRUCK.line(nd["project:gardenio"], 2, 40), "", "a project's count only at the Project level");
var boarded = { kind: "plot", ref: { plan: { id: "gd-planQ", project: "gardenio", boarded: true } } };
assert.deepStrictEqual([TRUCK.line(boarded, 2, 40), TRUCK.line(boarded, 2, 18)], ["on the next release (open)", "next release"], "merged onto the open release");
assert.strictEqual(TRUCK.stale(), false);
assert.ok(TRUCK.shipped({ id: "gd-planA", project: "gardenio" }) && !TRUCK.shipped({ id: "gd-planZ", project: "gardenio" }));
assert.ok(!TRUCK.shipped({ id: "gd-planQ", project: "gardenio", boarded: true }), "no shipped stamp before the cut");
TRUCK.feed({ gardenio: { latest: { tag: "2026.300.1", status: "cut", count: 2 }, recent: [] } });
assert.strictEqual(TRUCK.stale(), true, "a new release: the signs say it again");
TRUCK.feed(null);
assert.strictEqual(TRUCK.hots(world)[0].id, "release:2026.299.2", "feed(null): back to the document's");

/* ---------- 7. ART: the crate, the shipped stamp, the legend ---------- */
var k = rec();
ART.drawCrate(k, pal, 10, 20);
assert.deepStrictEqual(k.ops[0], { c: pal.k, x: 10, y: 20, w: 5, h: 5 }, "a 5 x 5 crate");
k = rec();
ART.drawShippedMark(k, pal, 100, 200, 63, 1);
assert.ok(k.ops.every(function (o) { return o.x >= 102 && o.x + o.w <= 111 && o.y >= 199 && o.y + o.h <= 208; }), "the stamp: 9 x 9 on the top fence at the left, its top row only above it");
assert.ok(k.ops.some(function (o) { return o.c === pal.b; }), "with its crate");
k = rec();
ART.drawShippedMark(k, pal, 100, 200, 64, 1);
assert.ok(k.ops.every(function (o) { return o.x >= 104 && o.x + o.w <= 122 && o.y >= 198 && o.y + o.h <= 216; }), "2x from a 64 px plot, clear of the cursor's ring above the fence");
k = rec();
ART.drawLegend(k, "release", false);
assert.ok(k.ops.length > 0 && k.ops.every(function (o) { return o.x >= 0 && o.x + o.w <= 24 && o.y >= 0 && o.y + o.h <= 28; }), "the legend glyph fits its 24 x 28");
/* ---------- 8. the release panel's pure part (web/release.js): the changelog by plan, then step ---------- */
var RELEASE = require(path.join(process.argv[2], "release.js"));
var rows = [
  { pr: 1, title: "a", url: "https://github.com/o/r/pull/1", plan: "gd-p1", plan_title: "Plan one", step: "gd-s1", step_title: "Step one" },
  { pr: 2, title: "b", url: "https://github.com/o/r/pull/2" },
  { pr: 3, title: "c", url: "https://github.com/o/r/pull/3", plan: "gd-p2", plan_title: "Plan two", plan_status: "closed", step: "gd-s3" },
  { pr: 4, title: "d", url: "https://github.com/o/r/pull/4", plan: "gd-p1", step: "gd-s2", step_title: "Step two" },
  { pr: 5, title: "e", url: "https://github.com/o/r/pull/5", plan: "gd-p1", step: "gd-s1" },
  { pr: 6, title: "f", url: "https://github.com/o/r/pull/6", plan: "gd-p3", plan_title: "Plan three", task: "gd-t6" }
];
var gr = RELEASE.group(rows);
assert.deepStrictEqual(gr.map(function (g) { return [g.id, g.title, g.n]; }), [["gd-p2", "Plan two", 1], ["gd-p1", "Plan one", 3], ["gd-p3", "Plan three", 1], ["", "", 1]], "the plan it finished first, then most PRs, the unplanned last");
assert.deepStrictEqual(gr[1].steps.map(function (s) { return [s.id, s.title, s.rows.map(function (r) { return r.pr; })]; }), [["gd-s1", "Step one", [1, 5]], ["gd-s2", "Step two", [4]]], "steps in the order met, a step's PRs together");
assert.strictEqual(gr[2].steps[0].id, "gd-t6", "no step: its task");
assert.deepStrictEqual(RELEASE.group(null), []);
assert.strictEqual(RELEASE.summary({ contents: rows }, 0), "6 PRs from 3 plans");
assert.strictEqual(RELEASE.summary({ contents: rows }, 8), "8 PRs from 3 plans (2 with no PR number, not listed)", "the record's own count wins (nit b)");
assert.strictEqual(RELEASE.summary({ contents: [rows[1]] }, 0), "1 PR");
assert.deepStrictEqual(RELEASE.facts({ status: "live", cut_at: "2026-10-31T12:19:07Z", deployments: [{ target: "prod", since: "2026-10-31T13:00:00Z" }, { target: "old", since: "2026-10-30T00:00:00Z", until: "2026-10-31T13:00:00Z" }] }),
  ["Live", "cut 2026-10-31 12:19Z", "runs on prod since 2026-10-31 13:00Z"]);
assert.deepStrictEqual(RELEASE.facts({ status: "odd", deployments: [] }), ["Cut, not built or deployed yet", "No deployments yet"], "an unknown status reads as cut");
assert.strictEqual(RELEASE.url("gd-a/b"), "/api/releases/gd-a%2Fb");
assert.strictEqual(RELEASE.stamp("nope"), "");

report.bay = bay; report.tags = TRUCK.tagFit("2026.299.2", TRUCK.PLATE.w - 4);
report.ok = true;
console.log(JSON.stringify(report));
