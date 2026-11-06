package gardenhub

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// Every running gardener stands in its project's Growing field, and the barn is still the account of every running agent
// (28.12, plan gd-ryynhxINtp; 5a, gd-JnmuZLs0kD). Pure in world.js, porch.js and art.js, so it runs under node:
// gardenerSpots gives every gardener, on a plot or at home, a field stand in need order (a steward stays at its homestead)
// and nothing a ghost; gardenerField lists those out first, then those at home, each with its plant and its plot, the rest
// past cap behind a "+N" badge, the Mine scope and a still-moving gardener left out; PORCH.groups counts those at a homestead
// apart from the barn, and barnWords says everyone in words; ART.drawHome draws every stand opaque in the Growing field.
func TestFieldStandsUnderNode(t *testing.T) {
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
var w = W.layout(doc, null, { now: 1, readonly: true });
function g(id, state, bucket, task, plot, since, extra) { var o = { id: id, agent: "name-" + id, role: "crew", project: "gardenio", state: state, bucket: bucket, task: task, plot: plot ? { plan: plot, phase: "" } : null, since: since || 0, href: "/garden?bead=" + id, operator: "oidc:me" }; if (extra) Object.keys(extra).forEach(function (k) { o[k] = extra[k]; }); return o; }
var gs = [
  g("w1", "working", "alive", "pa-t3", "pa", 50), g("ask", "yielding", "alive", "pa-t0", "pa", 90, { reason: { kind: "parked", text: "Waiting on X", href: "/garden?bead=gd-X" } }),
  g("down", "failed", "stuck", "pa-t7", "pa", 99), g("vis", "working", "alive", "pb-t0", "pb", 5, { operator: "oidc:flo" }),
  g("idle", "idle", "idle", "pa-t1", "pa", 5), g("seed", "working", "alive", "s1", null, 6),
  g("stw", "working", "alive", "pa-t2", "pa", 70, { role: "steward" })
];
// 1. Every gardener has a field stand in its project's field, in need order; one on a plot keeps its plot spot too; a steward is
// at its homestead, in neither; nothing has a ghost.
var s3 = W.gardenerSpots(w, gs, 3), st = W.gardenerState(w, gs);
assert.deepStrictEqual(s3.home, ["seed", "idle"], "at home: no plot, or idle");
assert.deepStrictEqual(s3.fields["project:gardenio"], ["down", "ask", "vis", "seed", "w1", "idle"], "a stand for each, need first, those on plots and at home alike");
assert.deepStrictEqual([s3.spots.ask.plot, s3.spots.ask.field], ["pa", { home: "project:gardenio", task: "pa-t0" }], "one on a plot has both a plot spot and a field stand");
assert.deepStrictEqual(W.gardenerSpots(w, gs, 1).fields, s3.fields, "at every level: the field is not capped by the plots' fences");
assert.ok(s3.fields["project:gardenio"].indexOf("stw") < 0 && !s3.spots.stw.field, "a steward stays at its homestead");
assert.ok(!("ghosts" in s3) && !("barns" in s3) && W.GHOST === undefined && W.ghostOf === undefined, "nothing has a ghost");
assert.strictEqual(W.gardenerSpots(null, gs, 3).home.length, gs.length, "no world, nobody on a plot: everyone at home");
// 2. The field's stands: those out first, then those at home; each carries its gardener, its look, its plant and its plot.
var items = W.gardenerField(st, "", "/b");
assert.deepStrictEqual(items.map(function (it) { return it.id; }), ["down", "ask", "vis", "w1", "seed", "idle"].map(function (id) { return W.FIELD + id; }));
assert.strictEqual(items.more, 0, "room for everyone");
var ga = items[1];
assert.deepStrictEqual([ga.kind, ga.task, ga.plot, ga.where, ga.href, ga.title, ga.plant], ["stand", "pa-t0", "pa", "On Plan pa", "/b/garden?bead=ask", "name-ask", w.index["pa-t0"].task], "its plant is its task's row");
assert.ok(ga.letter && !ga.ghost, "solid, its speech bubble its own");
assert.strictEqual(ga.g, st.byId.ask, "the stand carries its gardener"); assert.strictEqual(ga.look, st.look.ask, "and wears its look");
assert.deepStrictEqual([items[4].plot, items[4].plant.id, items[4].plant.title], ["", "s1", "s1"], "one at home with a task drawn nowhere: a plant made from its task");
assert.strictEqual(items[2].where, "On Plan pb", "a visitor's stand names the plot it works on");
assert.deepStrictEqual(W.gardenerField(st, "oidc:flo", "/b").map(function (it) { return it.id; }), [W.FIELD + "vis"], "Mine: the viewer's own");
assert.deepStrictEqual(W.gardenerField(st, "", "/b", 0, function (it) { return it.id !== W.FIELD + "w1"; }).length, 5, "keep leaves out one still on its way");
assert.deepStrictEqual([W.gardenerField(st, "", "/b", 0, null, "project:music").length, W.gardenerField(st, "", "/b", 0, null, "project:gardenio").length], [0, 6], "biomeId: that field alone");
// 3. Past cap: cap - 1 stands, those out first, and the last place is the "+N" badge.
var full = W.gardenerField(st, "", "/b", 4);
assert.deepStrictEqual(full.map(function (it) { return it.id; }), [W.FIELD + "down", W.FIELD + "ask", W.FIELD + "vis"], "three stands and the badge");
assert.strictEqual(full.more, 3, "+3: w1, seed and idle");
assert.strictEqual(W.gardenerField(st, "", "/b", 6).more, 0, "exactly full: no badge");
var many = []; for (var i = 0; i < 60; i++) many.push(g("m" + i, i % 3 ? "working" : "idle", i % 3 ? "alive" : "idle", "pa-t13", "pa", i));
var sm = W.gardenerState(w, many), bm = W.gardenerField(sm, "", "/b", 20);
assert.strictEqual(bm.length + bm.more, 60, "the stands and the badge account for all sixty");
assert.strictEqual(bm.length, 19); assert.ok(bm.every(function (it) { return it.plot === "pa"; }), "those out on the plot first");
// 4. The count: everyone running, resting (at home in its field), out in the plots and at a homestead; the Porch's n is the same number.
var plotOf = function (x) { var s = s3.spots[x.id]; return s && s.plot ? { id: s.plot, title: w.index[s.plot].plan.title } : null; };
var land = function (x) { var s = s3.spots[x.id]; return !!(s && s.land); };
var m = P.groups(gs, plotOf, { land: land }), m0 = P.groups(gs, plotOf, {});
assert.deepStrictEqual([m.n, m.home, m.land, m.all], [7, 2, 1, 7], "the steward is at its homestead, not the barn");
assert.deepStrictEqual([m0.home, m0.land], [3, 0], "without land, the Porch's own grouping is as before");
assert.strictEqual(W.barnWords(m), "7 gardeners, 2 need you, 6 running, 1 down: 2 resting, 4 out in the plots, 1 at a homestead", "the one down is not running; it and the one asking need you, as the Porch's Needs you group counts them (gd-GL76uVzdAQ)");
assert.strictEqual(W.barnWords(m, true), "2 need you · 6 running · 1 down · 4 out", "the short sign says it too, the need first");
assert.deepStrictEqual(W.barnCount(m), { n: 7, home: 2, land: 1, down: 1, ask: 1 }, "the account as numbers, as the home counts carry it");
assert.strictEqual(W.barnWords(W.barnCount(m), true), W.barnWords(m, true), "a count with down and ask already in it says the same");
assert.strictEqual(W.barnWords({ n: 12, home: 4, land: 0 }), "12 running: 4 resting, 8 out in the plots", "the owner's example");
assert.strictEqual(W.barnWords({ n: 5, home: 1, land: 2 }), "5 running: 1 resting, 2 out in the plots, 2 at their homesteads");
assert.strictEqual(W.barnWords({ n: 19, home: 8, land: 0 }, true), "19 running · 11 out", "the Barn's cursor sign, short");
var two = P.groups([g("a1", "yielding", "alive", "", null, 1), g("a2", "yielding", "alive", "", null, 2), g("d1", "failed", "stuck", "", null, 3)], function () { return null; }, {});
assert.strictEqual(W.barnWords(two, true), "3 need you · 2 running · 1 down · 0 out", "two asking and one down");
var allDown = P.groups([g("d1", "failed", "stuck", "", null, 3)], function () { return null; }, {});
assert.strictEqual(W.barnWords(allDown, true), "1 needs you · 1 down · 0 out", "none running: the running part is left out");
assert.strictEqual(W.barnWords(allDown), "1 gardener, 1 needs you, 1 down: 1 resting, 0 out in the plots");
assert.strictEqual(W.barnWords({ n: 0, home: 0 }), "Nobody is running"); assert.strictEqual(W.barnWords(null), "Nobody is running");
assert.strictEqual(P.summary(m).indexOf(m.n + " gardeners"), 0, "the Porch's every-gardener count is the barn's");
// 5. The Growing field (ART.drawHome over WORLD.homeFill): every stand in it, drawn opaque; the Barn holds no figure; the badge is drawn.
var alphas = [], cur = 1, fills = 0, pal = A.palette(false), hb = w.index["project:gardenio"];
var ctx = { fillRect: function () { fills++; if (cur !== 1) alphas.push(cur); }, drawImage: function () {}, set fillStyle(v) {}, get globalAlpha() { return cur; }, set globalAlpha(v) { cur = v; } };
var hf = W.homeFill(w, { agent: items }, {})["project:gardenio"];
assert.deepStrictEqual([hf.growing.items.map(function (h) { return h.kind + ":" + h.item.id; }), hf.barn.items, hf.growing.badge], [items.map(function (it) { return "stand:" + it.id; }), [], undefined], "every stand in the Growing field, in order; the Barn holds no figure (28.12)");
A.drawHome(ctx, hb.home, { part: "live", fill: hf }, pal);
assert.deepStrictEqual([alphas.length, cur], [0, 1], "no figure translucent");
var f0 = fills, h0 = hf.growing.items[0]; hf.growing.badge = { x: h0.x, y: h0.y, w: h0.w, h: h0.h, n: 3 }; A.drawHome(ctx, hb.home, { part: "live", fill: hf }, pal); assert.ok(fills - f0 > 20, "the +3 badge is drawn");
`
	var args []string
	for _, f := range []string{"world.js", "porch.js", "art.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node barn test failed: %v\n%s", err, out)
	}
}

// The farmstead's courtyard (homestead-world-design.md 29.2, 29.7; 28.8, 28.10), under node: ART.drawHome's base paints the
// lane cross and the asks' path in path tiles, the pen's trampled ground, the Growing field's soil with no fence (and no fence
// tile anywhere: the pen's is GOATS's), the Escalations' post with its flag only while one waits on a human, the barn and the
// mailbox on their keeps, its flag up with a letter, and two wheel ruts on the bay only with a release; flat at Project is a
// few fills; the dog is live, not baked; WORLD.homeFill's stands are in the Growing field, solid, and the Barn draws nothing live.
func TestHomeStripArtUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), A = require(process.argv[2]), assert = require("assert");
var doc = { plans: [{ id: "pa", title: "Plan pa", project: "gardenio", created: 1, activity: 1, counts: {}, phases: [], unphased: { id: "", counts: {}, tasks: [] } }], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } };
var w = W.layout(doc, null, { now: 1, readonly: true }), gs = [];
for (var i = 0; i < 12; i++) gs.push({ id: "m" + i, agent: "m" + i, role: "crew", project: "gardenio", state: "working", bucket: "alive", task: "x", plot: i % 2 ? { plan: "pa", phase: "" } : null, since: i, href: "/b" });
var bm = W.gardenerField(W.gardenerState(w, gs), "", "/b", 12), cur = 1, alphas = [];
var ctx = { fillRect: function () { if (cur !== 1) alphas.push(cur); }, drawImage: function () {}, set fillStyle(v) {}, get globalAlpha() { return cur; }, set globalAlpha(v) { cur = v; } };
var hm = w.index["project:gardenio"].home, rs = [], pal = A.palette(false);
var rc = { fillRect: function (x, y, wd, ht) { rs.push([x, y, wd, ht]); }, drawImage: function () {}, set fillStyle(v) {}, globalAlpha: 1 };
var on = function (o, r) { rs = []; A.drawHome(rc, hm, o, pal); return rs.filter(function (q) { return q[0] >= r.x * 16 && q[1] >= r.y * 16 && q[0] + q[2] <= (r.x + r.w) * 16 && q[1] + q[3] <= (r.y + r.h) * 16; }).length; };
assert.strictEqual(on({ part: "base", release: true }, hm.bay) - on({ part: "base" }, hm.bay), 4, "two ruts with a release, none without");
assert.ok(on({ part: "base" }, hm.keep.barn) > 100 && on({ part: "base" }, hm.keep.mailbox) > 10, "the barn and the mailbox on their keeps");
var down = on({ part: "base" }, hm.keep.mailbox);
assert.notStrictEqual(on({ part: "base", letters: true }, hm.keep.mailbox), down, "the flag is up while a letter waits");
assert.ok(on({ part: "base", flat: true }, hm.cells.growing) < 10, "flat at the Project level: a few fills");
var tl = [], sp = { get: function (name) { return { n: name }; } };   // the base's tiles by name, at their tile
var tc = { fillRect: function () {}, drawImage: function (im, x, y) { tl.push({ n: im.n, x: Math.floor(x / 16), y: Math.floor(y / 16) }); }, set fillStyle(v) {}, globalAlpha: 1 };
var tiles = function (o) { tl = []; o.sp = sp; A.drawHome(tc, hm, o, pal); return tl; }, inR = function (t, r) { return t.x >= r.x && t.y >= r.y && t.x < r.x + r.w && t.y < r.y + r.h; };
var bt = tiles({ part: "base" }), paths = bt.filter(function (t) { return /^pa/.test(t.n); }), g = hm.cells.growing, e = hm.cells.escalation, pn = hm.cells.pen;
assert.deepStrictEqual([hm.cross.xn - hm.x, hm.cross.xs - hm.x, hm.cross.y - hm.y], [22, 23, 15], "an empty farm's arms (29.10): north on column 22, south on 23, east-west on row 15");
var arm = function (k) { return k < 15 ? hm.cross.xn : hm.cross.xs; };
for (var k = 0; k < 32; k++) assert.ok((k === 15 || paths.some(function (t) { return t.x === arm(k) && t.y === hm.y + k; })) && paths.some(function (t) { return t.y === hm.cross.y && t.x === hm.x + k; }), "the arms are path tiles edge to edge, tile " + k);
assert.ok(!paths.some(function (t) { return (t.x === hm.cross.xs && t.y < hm.cross.y) || (t.x === hm.cross.xn && t.y > hm.cross.y); }), "the north arm stops at the east-west arm, and the south arm starts there");
for (k = 0; k < e.h; k++) assert.ok(paths.some(function (t) { return t.x === e.x + e.w && t.y === e.y + k; }), "a path between the Escalations and the Mailbox, tile " + k);
assert.deepStrictEqual(bt.filter(function (t) { return /^f[hv]$/.test(t.n); }), [], "no fence tile in the farmstead: not the field's, not the Escalations'");
assert.strictEqual(bt.filter(function (t) { return /^s\d$/.test(t.n) && inR(t, g); }).length, (g.w - 2) * (g.h - 2), "the Growing field is soil inside its margin");
assert.ok(paths.filter(function (t) { return inR(t, { x: pn.x + 1, y: pn.y + 1, w: pn.w - 2, h: pn.h - 2 }); }).length > 5, "the pen's ground is trampled");
var post = bt.filter(function (t) { return t.n === "escpost"; });
assert.deepStrictEqual(post.map(function (t) { return [t.x, t.y]; }), [[e.x - 1, e.y + e.h - 1]], "the Escalations' post on the north arm at the cell's south-west corner");
assert.deepStrictEqual([bt.some(function (t) { return t.n === "escflag"; }), tiles({ part: "base", asks: true }).some(function (t) { return t.n === "escflag"; })], [false, true], "its flag flies only while one waits on a human");
assert.ok(on({ part: "live" }, hm.keep.mailbox) > 0 && on({ part: "base" }, hm.keep.mailbox) === down, "the dog is live, not baked");
var fill = W.homeFill(w, { agent: bm }, {})["project:gardenio"], hf = fill.growing;
assert.ok(hf.items.length + hf.more === bm.length && hf.items.every(function (h) { return h.kind === "stand" && W.hit(h, g); }), "the stands in the Growing field, the rest counted");
assert.deepStrictEqual(fill.barn.items, [], "the Barn holds no figures");
var nf = 0; cur = 1; alphas = []; A.drawHome({ fillRect: function () { nf++; if (cur !== 1) alphas.push(cur); }, drawImage: function () {}, set fillStyle(v) {}, get globalAlpha() { return cur; }, set globalAlpha(v) { cur = v; } }, hm, { part: "live", fill: fill }, pal);
var n0 = nf; nf = 0; A.drawHome({ fillRect: function () { nf++; }, drawImage: function () {}, set fillStyle(v) {}, globalAlpha: 1 }, hm, { part: "live", fill: {} }, pal);
assert.ok(n0 > nf && alphas.length === 0, "the stands are drawn, solid");
assert.strictEqual(on({ part: "live", fill: fill }, hm.cells.barn), 0, "the Barn draws nothing live: its picture is the base's, its truck TRUCK's");
`
	var args []string
	for _, f := range []string{"world.js", "art.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node farmstead art test failed: %v\n%s", err, out)
	}
}

// The Growing field's pairs (28.12, gd-PeLoWLR7Lq), under node: ART.drawHome draws every stand solid, its plant on the pair
// slot's west tile and its figure on the east, one with no task with no plant; with a sprite cache each pair is one bitmap,
// painted again only when its frame or its gardener's look changes, so a field of stands stays inside the frame's draw budget.
func TestFieldPairsArtUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), A = require(process.argv[2]), assert = require("assert");
var tasks = []; for (var i = 0; i < 4; i++) tasks.push({ id: "pa-t" + i, title: "T" + i, type: "task", state: "in_progress", updated: 1 });
var doc = { plans: [{ id: "pa", title: "Plan pa", project: "gardenio", created: 1, activity: 1, counts: { in_progress: 4 }, phases: [{ id: "pa-p1", title: "Phase 1", counts: { in_progress: 4 }, tasks: tasks }], unphased: { id: "", counts: {}, tasks: [] } }], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } };
var w = W.layout(doc, null, { now: 1, readonly: true });
function g(id, state, bucket, task, plot, since) { return { id: id, agent: "name-" + id, role: "crew", project: "gardenio", state: state, bucket: bucket, task: task, plot: plot ? { plan: plot, phase: "" } : null, since: since || 0, href: "/b" }; }
var items = W.gardenerField(W.gardenerState(w, [g("w1", "working", "alive", "pa-t0", "pa", 1), g("down", "failed", "stuck", "pa-t1", "pa", 2), g("home", "working", "alive", "pa-t2", null, 3), g("idle", "idle", "idle", "pa-t3", null, 4)]), "", "/b");
var hm = w.index["project:gardenio"].home, fill = W.homeFill(w, { agent: items }, {})["project:gardenio"], stands = fill.growing.items, pal = A.palette(false);
assert.deepStrictEqual(stands.map(function (h) { return h.id; }).sort(), items.map(function (it) { return it.id; }).sort(), "every stand in the field");
var alphas = [], cur = 1, rs = [];
var ctx = { fillRect: function (x, y) { rs.push([x, y]); if (cur !== 1) alphas.push(cur); }, drawImage: function () {}, set fillStyle(v) {}, get globalAlpha() { return cur; }, set globalAlpha(v) { cur = v; } };
A.drawHome(ctx, hm, { part: "live", fill: fill }, pal);
assert.deepStrictEqual([alphas.length, cur], [0, 1], "no figure translucent");
var inTile = function (h, dx) { return rs.some(function (q) { return q[0] >= (h.x + dx) * 16 && q[0] < (h.x + dx + 1) * 16 && q[1] >= h.y * 16 && q[1] < (h.y + 1) * 16; }); };
stands.forEach(function (h) { assert.ok(h.item.plant && inTile(h, 0) && inTile(h, 1), h.id + ": its plant west, its figure east"); });
var rest = W.gardenerField(W.gardenerState(w, [g("rest", "idle", "idle", "", null, 1)]), "", "/b"), rf = W.homeFill(w, { agent: rest }, {})["project:gardenio"], rh = rf.growing.items[0];
rs = []; A.drawHome(ctx, hm, { part: "live", fill: rf }, pal);
assert.ok(!rh.item.plant && !inTile(rh, 0) && inTile(rh, 1), "one with no task stands with no plant");
var made = 0, blits = [], sp = A.spriteCache(function () { made++; return { getContext: function () { return ctx; } }; });
var bc = { fillRect: function () {}, drawImage: function (im, x, y) { blits.push([x, y]); }, set fillStyle(v) {} };
var live = function (fr) { blits = []; A.drawHome(bc, hm, { part: "live", fill: fill, sp: sp, frame: fr }, pal); return blits; };
assert.deepStrictEqual(live(0), stands.map(function (h) { return [h.x * 16 - A.PAIR.x, h.y * 16 - A.PAIR.y]; }), "one bitmap a pair, at its slot");
var m0 = made; live(0); assert.strictEqual(made, m0, "the same frame again paints nothing");
live(1); var m1 = made; live(0); live(1); assert.ok(m1 > m0 && made === m1, "each frame painted once");
var h0 = stands[0].item; h0.lookNow = function () { return { pose: "build", group: "crew", hat: 3 }; };
live(1); assert.strictEqual(made, m1 + 1, "a new look paints its pair again");
`
	var args []string
	for _, f := range []string{"world.js", "art.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node field pairs test failed: %v\n%s", err, out)
	}
}
