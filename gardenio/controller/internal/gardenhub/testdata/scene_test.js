/* The World drawn with none of the signed-in page (LW.3 gd-VEbAvsTNDe), run by TestSceneDrawsAlone:
   node scene_test.js <page-scene.js bundle> <farm document>...
   Runs the page-scene bundle as served in a bare context: a document that answers what the modules ask for as they load, canvases
   that count their calls, and no me, PLANS, WORKBENCH, BEAD, CHAT, GARDEN, EventSource or XMLHttpRequest. fetch is
   there only to say whether anything called it. Each document is laid out (SCENE.lay) and drawn (SCENE.scene(cv).draw) at the
   four levels: the regions as blobs at World and Project and as baked chunks at Plan and Bead, a rollup plate per plot in view
   at World and Project, flat beds at Plan, and sprites at Bead. At World and Project every plot in view with a gardener on it
   carries the gardener badge and no other does (gd-vi77cq1oYY), on a crowded World too (more stamps in view than their cache
   holds). Prints one JSON report for the Go test. */
"use strict";
var assert = require("assert"), fs = require("fs"), vm = require("vm");
var bundle = fs.readFileSync(process.argv[2], "utf8"), docs = process.argv.slice(3);

function ctx2d(n) {
  var c = { canvas: null };
  ["fillRect", "drawImage", "clearRect", "strokeRect", "fillText", "beginPath", "closePath", "moveTo", "lineTo", "arc", "rect", "fill", "stroke", "save",
    "restore", "translate", "scale", "rotate", "setTransform", "transform", "clip", "quadraticCurveTo", "bezierCurveTo", "ellipse", "setLineDash"].forEach(function (k) {
    c[k] = function () { n[k] = (n[k] || 0) + 1; };
  });
  c.measureText = function (s) { return { width: String(s).length * 6 }; };
  c.getImageData = function (x, y, w, h) { return { data: new Uint8ClampedArray(w * h * 4), width: w, height: h }; };
  c.putImageData = c.createImageData = function () { return { data: new Uint8ClampedArray(4) }; };
  c.createLinearGradient = c.createRadialGradient = function () { return { addColorStop: function () {} }; };
  c.createPattern = function () { return {}; };
  return c;
}
var off = {};   // every bitmap's calls (chunks, sprites, stamps): the main canvas has its own counter
var baked = [];   // the named tiles a native chunk blits, at their world tile ({n, x, y}): a chunk is translated to world px
function canvas(n) {
  var cv = { width: 0, height: 0, style: {} }, x = ctx2d(n || off), di = x.drawImage, tr = x.translate;
  x.translate = function () { x.chunk = true; return tr.apply(x, arguments); };
  x.drawImage = function (im, dx, dy) { if (x.chunk && im && im.tileName) baked.push({ n: im.tileName, x: Math.floor(dx / 16), y: Math.floor(dy / 16) }); return di.apply(x, arguments); };
  x.canvas = cv; cv.getContext = function () { return x; }; return cv;
}
function el() { return { style: {}, dataset: {}, classList: { add: function () {}, remove: function () {}, toggle: function () {}, contains: function () { return false; } }, setAttribute: function () {}, appendChild: function (c) { return c; }, addEventListener: function () {} }; }
var fetched = [];
var doc = {
  body: { dataset: {} }, documentElement: { dataset: {} },
  createElement: function (t) { return t === "canvas" ? canvas() : el(); },
  getElementById: function () { return null; }, querySelector: function () { return null; }, querySelectorAll: function () { return []; },
  addEventListener: function () {}, removeEventListener: function () {}, dispatchEvent: function () {}
};
var box = { document: doc, window: {}, console: console, Date: Date, Math: Math, JSON: JSON, setTimeout: function () { return 0; }, clearTimeout: function () {},
  setInterval: function () { return 0; }, clearInterval: function () {}, fetch: function (u) { fetched.push(String(u)); return new Promise(function () {}); } };
box.window = box;
vm.createContext(box);
vm.runInContext(bundle, box, { filename: "page-scene.js" });
var gone = ["me", "PLANS", "WORKBENCH", "BEAD", "CHAT", "GARDEN", "EventSource", "XMLHttpRequest", "VIEWCUT"].filter(function (k) { return vm.runInContext("typeof " + k, box) !== "undefined"; });
assert.deepStrictEqual(gone, [], "the scene's context has signed-in globals");
var S = box.SCENE, W = box.WORLD, A = box.ART;
assert.ok(S && S.scene && S.lay, "the bundle defines SCENE.scene and SCENE.lay");
var seen = {};
["drawRollupPlate", "drawPlotFlat", "drawBead", "drawBiomeGround", "drawSign"].forEach(function (k) { var f = A[k]; A[k] = function () { seen[k] = (seen[k] || 0) + 1; return f.apply(this, arguments); }; });
var cache = A.spriteCache;   // each cached tile canvas carries its name, so a chunk's blits say what they are
A.spriteCache = function () { var c = cache.apply(this, arguments), get = c.get; c.get = function (name) { var t = get.apply(this, arguments); t.tileName = name; return t; }; return c; };
var badged = {}, marks = A.drawRollupMarks;   // the plots (by world px top left) whose marks carry the gardener badge
A.drawRollupMarks = function (c, pal, x, y, side, s, r) { if (r.gardeners > 0) badged[x + "," + y] = r.gardeners; return marks.apply(this, arguments); };
/* The badge on every plot in view with a gardener and on no other; returns how many carry it. */
function badges(st, cam, vp, what) {
  var n = 0;
  W.visible(st.world, W.viewRect(cam, vp, 0), 1).plots.forEach(function (p) {
    var gp = st.gs && st.gs[3].plots[p.id], got = badged[p.x * W.TILE + "," + p.y * W.TILE];
    assert.strictEqual(got, gp ? gp.ids.length : undefined, what + ": plot " + p.id + (gp ? " has gardeners and no badge" : " has none and a badge"));
    if (got) n++;
  });
  return n;
}

var report = { docs: [] };
docs.forEach(function (file) {
  var d = JSON.parse(fs.readFileSync(file, "utf8")), st = S.lay(d), main = {}, cv = canvas(main), sc = S.scene(cv), out = { file: file.split("/").pop(), levels: [] };
  assert.ok(st.world.biomes.length > 0, file + ": no region laid out");
  var vp = { w: 390, h: 844, dpr: 2 };
  cv.width = vp.w * vp.dpr; cv.height = vp.h * vp.dpr;
  var b = st.world.biomes.filter(function (x) { return x.plots.length; })[0] || st.world.biomes[0], f = W.tiles(b.foot), p = b.plots[0], pr = p ? W.tiles(p) : f;
  for (var level = 0; level <= 3; level++) {
    var r = W.levelRange(level), z = Math.sqrt(r[0] * Math.min(r[1], 4)), at = level >= 2 ? pr : f;
    var cam = { cx: at.x + at.w / 2, cy: at.y + at.h / 2, z: z };
    Object.keys(main).forEach(function (k) { delete main[k]; }); seen = {}; badged = {};
    var again = false;
    for (var i = 0; i < 40; i++) { again = sc.draw({ cam: cam, vp: vp, world: st.world, gs: st.gs, sst: st.sst, edgeIx: st.edgeIx, weedIx: st.weedIx, nb: st.nb }); if (!again) break; }   // a dense region bakes its chunks over a few frames
    var lv = { level: level, lod: W.lodFor(z), fills: main.fillRect || 0, blits: main.drawImage || 0, frames: i + 1, settled: !again };
    Object.keys(seen).forEach(function (k) { lv[k] = seen[k]; });
    if (level <= 1) lv.badges = badges(st, cam, vp, out.file + " level " + level);
    out.levels.push(lv);
  }
  /* The region baked whole at Plan (homestead-world-design.md 29.4, 29.7): a planted plot's ring all hedge, its corners
     rounded, a staked plot's all string, no fence tile anywhere in the region (round its edge, the Growing field or a plot),
     and every lane tile in the foot path. */
  var pz = Math.sqrt(W.levelRange(2)[0] * W.levelRange(2)[1]), pvp = { w: Math.ceil(f.w * pz) + 2, h: Math.ceil(f.h * pz) + 2, dpr: 1 };
  var pcv = canvas({}), psc = S.scene(pcv);   // a scene of its own, so no chunk is cached from the levels above
  pcv.width = pvp.w; pcv.height = pvp.h; baked = [];
  for (var j = 0; j < 80 && psc.draw({ cam: { cx: f.x + f.w / 2, cy: f.y + f.h / 2, z: pz }, vp: pvp, world: st.world, gs: st.gs, sst: st.sst, edgeIx: st.edgeIx, weedIx: st.weedIx, nb: st.nb }); j++);
  var inR = function (t, r) { return t.x >= r.x && t.y >= r.y && t.x < r.x + r.w && t.y < r.y + r.h; }, fences = baked.filter(function (t) { return /^f[hv]$/.test(t.n) && inR(t, b.foot); });
  assert.deepStrictEqual(fences, [], out.file + ": a fence tile in the region");
  var at = {}; baked.forEach(function (t) { (at[t.x + "," + t.y] = at[t.x + "," + t.y] || []).push(t.n); });
  out.hedged = out.strung = 0;
  b.plots.forEach(function (p) {
    for (var ty = p.y; ty < p.y + p.h; ty++) for (var tx = p.x; tx < p.x + p.w; tx++) {
      if (inR({ x: tx, y: ty }, { x: p.x + 1, y: p.y + 1, w: p.w - 2, h: p.h - 2 })) continue;
      var corner = (tx === p.x ? 0 : tx === p.x + p.w - 1 ? 1 : -1), want;
      if (corner >= 0 && (ty === p.y || ty === p.y + p.h - 1)) corner += ty === p.y ? 0 : 2; else corner = -1;
      want = p.staked ? /^str\|/ : corner >= 0 ? new RegExp("^hedgeC" + corner + "$") : ty === p.y || ty === p.y + p.h - 1 ? /^hedgeH$/ : /^hedgeV$/;
      assert.ok((at[tx + "," + ty] || []).some(function (n) { return want.test(n); }), out.file + ": plot " + p.id + (p.staked ? " (staked)" : "") + " has no " + want + " at " + tx + "," + ty + ": " + at[tx + "," + ty]);
      assert.ok(!(at[tx + "," + ty] || []).some(function (n) { return (p.staked ? /^hedge/ : /^str\|/).test(n); }), out.file + ": plot " + p.id + " mixes hedge and string at " + tx + "," + ty);
    }
    if (p.staked) out.strung++; else out.hedged++;
  });
  var paved = {}; baked.forEach(function (t) { if (/^pa/.test(t.n)) paved[t.x + "," + t.y] = 1; });
  assert.strictEqual(b.lanes.length, 4, out.file + ": four lanes");
  out.lanes = 0;
  b.lanes.forEach(function (l) { for (var y = l.y; y < l.y + l.h; y++) for (var x = l.x; x < l.x + l.w; x++) if (W.inFoot(b, x, y)) { assert.ok(paved[x + "," + y], out.file + ": lane " + l.name + " not path at " + x + "," + y); out.lanes++; } });
  report.docs.push(out);
});
/* a crowded World: 4 projects of 30 plans, a gardener on every third plan (one down, one asking), all in one desktop view */
var crowd = { plans: [], gardeners: [] };
for (var pj = 0; pj < 4; pj++) for (var pn = 0; pn < 30; pn++) {
  var pid = "gd-c" + pj + "-" + pn;
  crowd.plans.push({ id: pid, title: "Plan " + pid, project: "crowd" + pj, created: 1792000000 + pn, counts: { open: 1 }, phases: [], unphased: { id: "", title: "Other", counts: { open: 1 }, tasks: [] } });
  if (pn % 3 === 0) crowd.gardeners.push({ id: "g-" + pid, agent: "g-" + pid, role: "crew", project: "crowd" + pj, bucket: pn === 3 ? "stuck" : "alive", state: pn === 3 ? "failed" : "working", since: 1, plot: { plan: pid } });
}
var cst = S.lay(crowd), cmain = {}, ccv = canvas(cmain), csc = S.scene(ccv), cvp = { w: 1920, h: 1080, dpr: 1 }, bb = cst.world.bbox, ccam = W.fit(bb, cvp, { level: 0 });
ccv.width = cvp.w; ccv.height = cvp.h; badged = {};
csc.draw({ cam: ccam, vp: cvp, world: cst.world, gs: cst.gs, sst: cst.sst, edgeIx: cst.edgeIx, weedIx: cst.weedIx, nb: cst.nb });
var cin = W.visible(cst.world, W.viewRect(ccam, cvp, 0), 1).plots.length;
assert.ok(cin > 96, "the crowded World has more plots in view than the stamps' cache: " + cin);
report.crowd = { plots: cin, badges: badges(cst, ccam, cvp, "crowded World") };
assert.strictEqual(report.crowd.badges, 40, "a badge on each of the 40 plots with a gardener");
report.fetched = fetched;
console.log(JSON.stringify(report));
