package gardenhub

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The graveyard (gd-s9KzRk6boh, step 3.2 of plan gd-EGpbDpqsec). A project's closed wishes (the farm document's
// wishes.closed) are headstones in a band between the Orchard and the plots, newest first in reading order, at most
// CAPS.graveyard; the band is sized from the count, so a project with no closed wish lays out byte for byte as before, and
// the same document lays out the same twice. world.index carries wish:<id> once per stone, which WORLD.resolve and the
// cursor's pathFor (by the bead id too) find. The cursor walks the Graveyard as a band row between the Orchard and the plots;
// a stone reads its short title and "done · closed 2d ago". GOATS.stoneCard is the hover: who asked, the outcome and its
// reason in words, what the kin merged (links), the thread. art.js draws a flower only on a done wish's stone. Skipped when
// node is not installed.
func TestGraveyardUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), C = require(process.argv[2]), G = require(process.argv[3]), A = require(process.argv[4]), assert = require("assert");
var NOW = 1792000000, DAY = 86400;
function plan(id, project, created) { return { id: id, title: "Plan " + id, project: project, created: created, activity: created, counts: { open: 1 }, phases: [{ id: id + "-p1", title: "Phase 1", counts: { open: 1 }, tasks: [{ id: id + "-t1", title: "T1", type: "task", state: "open", updated: 1 }] }], unphased: { id: "", title: "Other", counts: {}, tasks: [] } }; }
function grave(id, project, closed, outcome, extra) { var w = { id: id, title: "Wish " + id, project: project, closed: closed, kin: 0, outcome: outcome, href: "/garden/c/bead/" + id }; if (extra) Object.keys(extra).forEach(function (k) { w[k] = extra[k]; }); return w; }
function doc(closed) {
  var d = { plans: [plan("gd-pa", "gardenio", 1), plan("gd-pz", "zeta", 2)], seeds: [], closed_plans: { total: 1, shown: [{ id: "gd-done", title: "Done plan", project: "gardenio", state: "complete", updated: 1 }] }, meadow: { total: 0, shown: [] }, meta: { truncated: false } };
  if (closed) d.wishes = { open: [], open_total: 0, closed: closed, closed_total: closed.length };
  return d;
}
function lay(d) { return W.layout(d, null, { now: 1 }); }
function biome(w, key) { return w.biomes.filter(function (b) { return b.key === key; })[0]; }
function strip(w) { return JSON.stringify({ biomes: w.biomes, lattice: w.lattice, bbox: w.bbox }); }

// 1. no closed wish, no graveyard: the cold layout is byte-identical with or without an empty wishes.closed, and twice over
var cold = lay(doc(null));
assert.strictEqual(strip(lay(doc([]))), strip(cold), "an empty graveyard takes no room");
assert.strictEqual(strip(lay(doc(null))), strip(cold), "deterministic");
assert.ok(!biome(cold, "gardenio").bands.graveyard && biome(cold, "gardenio").graveyard.length === 0);

// 2. three closed wishes on gardenio: newest first, left to right on one row, ids wish:<id>
var gs = [grave("gd-x1", "gardenio", NOW - 3 * DAY, "expired", { reason: "slack-thread-idle" }), grave("gd-x2", "gardenio", NOW - DAY, "done"), grave("gd-x3", "gardenio", NOW - 2 * DAY, "abandoned", { reason: "slack-thread-agent-failed" })];
var w = lay(doc(gs)), b = biome(w, "gardenio"), z = biome(w, "zeta");
assert.strictEqual(strip(lay(doc(gs.slice().reverse()))), strip(w), "the document's order does not move a stone");
assert.deepStrictEqual(b.graveyard.map(function (s) { return s.id; }), ["wish:gd-x2", "wish:gd-x3", "wish:gd-x1"], "newest first");
b.graveyard.forEach(function (s, j) { assert.strictEqual(s.kind, "stone"); assert.deepStrictEqual([s.w, s.h, s.y], [1, 1, b.bands.graveyard.y]); assert.strictEqual(s.x, b.bands.graveyard.x + 2 * b.bands.graveyard.at[j]); assert.strictEqual(s.project, "gardenio"); b.lanes.forEach(function (l) { assert.ok(!W.hit(s, l), "no stone on a lane (X-D6): " + s.id); }); });
assert.strictEqual(z.graveyard.length, 0, "another project's graveyard is its own");
assert.strictEqual(b.counts.graves, 3);
// the band sits past the Orchard, before the plots; the Orchard moved up by its height and gap, the plots and the farmstead did not move inside their region
var gy = b.bands.graveyard, cb = biome(cold, "gardenio"), top = Math.min.apply(null, b.plots.map(function (p) { return p.y; }));
assert.ok(b.bands.orchard.y + 1 + W.BAND.graveGap <= gy.y && gy.y + gy.h + W.BAND.orchardGap <= top, "orchard, graveyard, plots");
assert.strictEqual(top - (b.bands.orchard.y), Math.min.apply(null, cb.plots.map(function (p) { return p.y; })) - cb.bands.orchard.y + gy.h + W.BAND.graveGap);
assert.ok(gy.y + gy.h + W.BAND.orchardGap <= b.home.y && b.sign.y === b.home.y, "graveyard, then the farmstead with its sign on its north edge (29.2)");
assert.ok(b.used.y <= gy.y && b.used.y + b.used.h >= gy.y + gy.h, "the used rect takes the band in");
// the shape: more stones than one row holds wrap at pitch 2; none takes no room
var g1 = { c0: 3, r0: 2, cols: 2, rows: 1 }, sh = W.shape(0, 0, g1, false, 30), lc = W.laneCells(sh.bx, Math.floor(sh.bw / W.BAND.stonePitch), W.BAND.stonePitch, 1, sh.laneX), cols = Math.floor(sh.bw / W.BAND.stonePitch) - Object.keys(lc).length;
assert.deepStrictEqual([sh.bands.graveyard.h, sh.bands.graveyard.cols, sh.bands.graveyard.at.length], [2 * Math.ceil(30 / cols) - 1, cols, cols], "the columns no lane crosses");
assert.ok(!W.shape(0, 0, g1, false, 0).bands.graveyard);
assert.strictEqual(JSON.stringify(W.shape(0, 0, g1, false, 0)), JSON.stringify(W.shape(0, 0, g1, false)));
// capped like the farm's list: at most CAPS.graveyard stones
var many = []; for (var i = 0; i < W.CAPS.graveyard + 6; i++) many.push(grave("gd-m" + (i < 10 ? "0" : "") + i, "gardenio", NOW - i, "done"));
var wm = biome(lay(doc(many)), "gardenio");
assert.strictEqual(wm.graveyard.length, W.CAPS.graveyard); assert.strictEqual(wm.graveyard[0].id, "wish:gd-m00");

// 3. world.index carries wish:<id> once, and the camera can jump to it
assert.strictEqual(w.index["wish:gd-x2"], b.graveyard[0]); assert.ok(!w.index["gd-x2"], "keyed wish:<id>, never the bare id");
var at = W.atFor(w, "stone", "wish:gd-x2"), r = W.resolve(w, at);
assert.strictEqual(at, "gardenio/wish%3Agd-x2"); assert.deepStrictEqual([r.kind, r.id, r.found, r.level], ["stone", "wish:gd-x2", true, 3]);
assert.ok(W.visible(w, W.tiles(b.foot), 1).items.indexOf(b.graveyard[1]) >= 0, "in view at the Project level");
assert.deepStrictEqual(W.search(W.searchIndex(w), "Wish gd-x3").map(function (e) { return [e.kind, e.id]; })[0], ["stone", "wish:gd-x3"]);
// the jump index lists each project's graveyard too (3.3, gd-NbWjIdLTnd), by its name or "closed wishes", and a stone by its bead id
var gidx = W.searchIndex(w), gk = function (q) { return W.search(gidx, q).map(function (e) { return [e.kind, e.id]; })[0]; };
assert.deepStrictEqual(gidx.filter(function (e) { return e.kind === "graveyard"; }).map(function (e) { return [e.id, e.title, e.key, e.note]; }), [["band:gardenio/graveyard", "Graveyard", "gardenio", "3 closed wishes"]]);
assert.deepStrictEqual([gk("grave"), gk("closed wishes"), gk("gardenio graveyard")], [["graveyard", "band:gardenio/graveyard"], ["graveyard", "band:gardenio/graveyard"], ["graveyard", "band:gardenio/graveyard"]]);
assert.deepStrictEqual(gk("gd-x3"), ["stone", "wish:gd-x3"], "the stone's bead id, exactly");
assert.ok(!W.searchIndex(cold).some(function (e) { return e.kind === "graveyard"; }), "no closed wish, no Graveyard row");
var ol = W.outline(w).filter(function (e) { return e.key === "gardenio"; })[0].children.filter(function (e) { return e.kind === "stone"; });
assert.deepStrictEqual(ol.map(function (e) { return [e.id, e.hot, e.state, e.level]; }), [["wish:gd-x2", "stone:wish:gd-x2", "done", 3], ["wish:gd-x3", "stone:wish:gd-x3", "abandoned", 3], ["wish:gd-x1", "stone:wish:gd-x1", "expired", 3]]);

// 4. the cursor: a Graveyard band row between the Orchard and the plots; a stone reads its title and how and when it closed
var t = C.tree(w, null, { now: NOW * 1000 }), band = t.nodes["band:gardenio/graveyard"], orch = t.nodes["band:gardenio/orchard"];
assert.ok(band && band.span && band.kind === "band" && band.title === "Graveyard" && band.status === "3 closed wishes");
assert.ok(orch.r < band.r && band.r < t.nodes["gd-pa"].r, "orchard, graveyard, plots");
assert.deepStrictEqual(band.kids, ["wish:gd-x2", "wish:gd-x3", "wish:gd-x1"]);
var sn = t.nodes["wish:gd-x2"];
assert.deepStrictEqual([sn.kind, sn.title, sn.status, sn.open], ["stone", "Wish gd-x2", "done · closed 1d ago", "gd-x2"]);
assert.strictEqual(t.nodes["wish:gd-x1"].status, "expired · closed 3d ago");
assert.deepStrictEqual(C.pathFor(t, "gd-x3"), ["project:gardenio", "band:gardenio/graveyard", "wish:gd-x3"], "the bead id finds its stone");
assert.deepStrictEqual(C.pathFor(t, "gardenio/wish%3Agd-x3"), C.pathFor(t, "gd-x3"));
assert.strictEqual(C.atFor(t, C.pathFor(t, "gd-x3")), "gardenio/wish%3Agd-x3");
var up = C.move(t, ["project:gardenio", "gd-pa"], "up"); assert.deepStrictEqual(up.path, ["project:gardenio", "band:gardenio/graveyard"], "up from the plots");
assert.deepStrictEqual(C.move(t, up.path, "up").path, ["project:gardenio", "band:gardenio/orchard"], "up again: the Orchard");
assert.deepStrictEqual(C.move(t, up.path, "down", up.col).path, ["project:gardenio", "gd-pa"], "and back down");
assert.ok(!C.tree(cold, null, { now: NOW * 1000 }).nodes["band:gardenio/graveyard"], "no closed wish, no Graveyard row");
assert.deepStrictEqual(C.move(C.tree(cold, null, { now: NOW * 1000 }), ["project:gardenio", "gd-pa"], "up").path, ["project:gardenio", "band:gardenio/orchard"], "the empty row is stepped over");

// the phone's Go (26.12) from a project, with wishes in two (28.2, step 8): every pen, goat, Graveyard and stone row is a cursor
// path under its own project, so choosing it is a cursor jump there; a project's name finds its pen
var dz = doc([grave("gd-x4", "gardenio", NOW, "done"), grave("gd-z4", "zeta", NOW, "expired")]);
dz.wishes.open = [{ id: "gd-o1", title: "Open here", project: "gardenio", opened: 1 }, { id: "gd-o2", title: "Open there", project: "zeta", opened: 2 }];
var wz = lay(dz), tz = C.tree(wz, null, { now: NOW * 1000 }), zrows = W.searchIndex(wz).filter(function (e) { return /^(pen|goat|graveyard|stone)$/.test(e.kind); });
assert.deepStrictEqual(zrows.map(function (e) { return e.kind + " " + e.id; }).sort(), ["goat wish:gd-o1", "goat wish:gd-o2", "graveyard band:gardenio/graveyard", "graveyard band:zeta/graveyard",
  "pen home:gardenio/pen", "pen home:zeta/pen", "stone wish:gd-x4", "stone wish:gd-z4"]);
zrows.forEach(function (e) { var pz = C.pathFor(tz, e.id); assert.ok(pz && pz[0] === "project:" + e.key && pz[pz.length - 1] === e.id, e.id + " under its project: " + JSON.stringify(pz)); });
assert.deepStrictEqual([W.search(W.searchIndex(wz), "zeta pen")[0].id, W.search(W.searchIndex(wz), "gardenio pen")[0].id], ["home:zeta/pen", "home:gardenio/pen"]);

// 5. the hover: the outcome and its reason in words, what the kin merged, the thread
var ago = function (s) { return Math.round((NOW - s) / DAY) + "d"; };
var c1 = G.stoneCard(grave("gd-y1", "gardenio", NOW - 2 * DAY, "done", { asker: "oidc:matt@example.com", kin: 3, reason: "slack-thread-user-close",
  produce: [{ id: "gd-k1", kind: "mr", title: "Idle threads close themselves", url: "https://example.com/mr/77", iid: "#77", ready: true }, { id: "gd-k2", kind: "mr", title: "A very long merge request title that goes on and on", url: "https://example.com/mr/78", ready: true }], produce_more: 1 }), "/v", ago);
assert.strictEqual(c1.title, "Wish gd-y1"); assert.strictEqual(c1.href, "/v/garden/c/bead/gd-y1");
assert.deepStrictEqual(c1.lines.slice(0, 4), [["", "Asked by matt"], ["rsn", "Done: the asker closed it"], ["who", "closed wish · gardenio · closed 2d ago"], ["", "Its kin merged 3 merge requests, the newest 2 here"]]);
assert.deepStrictEqual(c1.lines[4][1].map(function (k) { return [k.text, k.href]; }), [["#77 Idle threads close themselves", "https://example.com/mr/77"], ["A very long merge request title that go…", "https://example.com/mr/78"]]);
assert.deepStrictEqual(c1.lines[5][1], [{ text: "Thread in the Commons", href: "/v/garden/c/bead/gd-y1", cls: "gchat", id: "gd-y1" }]);
assert.ok(c1.tip.indexOf("Its kin merged 3.") >= 0);
var c2 = G.stoneCard(grave("gd-y2", "", NOW, "abandoned", { reason: "slack-thread-agent-failed", kin: 2 }), "", ago);
assert.deepStrictEqual(c2.lines.slice(0, 4), [["", "Nobody is named as asking"], ["rsn rsn-sleeping", "Abandoned: its agent failed and nobody took it up"], ["who", "closed wish · closed 0d ago"], ["", "Its kin merged nothing"]]);
assert.strictEqual(c2.lines.length, 5, "no merge request links");
assert.strictEqual(G.stoneCard(grave("gd-y3", "", NOW, "expired", { reason: "slack-thread-idle-backfill" }), "", ago).lines[1][1], "Expired: the thread went quiet and closed itself");
assert.strictEqual(G.stoneCard(grave("gd-y4", "", NOW, "done", { reason: "shipped in #1131" }), "", ago).lines[1][1], "Done: shipped in #1131", "a typed reason as written");
assert.strictEqual(G.stoneCard(grave("gd-y5", "", NOW, "done"), "", ago).lines[3][1], "Nothing came of it");

// 6. the stone: a flower at a done wish's foot, a plain stone otherwise; both 16 x 16 in the tile
var pal = A.palette(false);
function paint(flower) { var cols = {}, box = [99, 99, -1, -1], ctx = { set fillStyle(v) { this._f = v; }, get fillStyle() { return this._f; }, fillRect: function (x, y, ww, hh) { cols[this._f] = 1; box = [Math.min(box[0], x), Math.min(box[1], y), Math.max(box[2], x + ww), Math.max(box[3], y + hh)]; } }; A.drawHeadstone(ctx, pal, 0, 0, flower); return { cols: cols, box: box }; }
var plain = paint(false), flow = paint(true);
assert.ok(plain.cols[pal.r] && plain.cols[pal.k] && !plain.cols[pal.F], "plain: grey stone, outlined, no flower");
assert.ok(flow.cols[pal.r] && flow.cols[pal.F] && flow.cols[pal.Y], "done: a pink flower with a yellow eye");
[plain, flow].forEach(function (p) { assert.ok(p.box[0] >= 0 && p.box[1] >= 0 && p.box[2] <= 16 && p.box[3] <= 16); });
console.log("ok");
`
	web := func(f string) string { p, _ := filepath.Abs(filepath.Join("web", f)); return p }
	out, err := exec.Command(node, "-e", script, web("world.js"), web("cursor.js"), web("goats.js"), web("art.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("graveyard under node: %v\n%s", err, out)
	}
}
