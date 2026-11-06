/* What grows on the splash's land (web/splash/flora.js, plan gd-EJGA3zDGCk step 5), run by TestFloraUnderNode:
   node flora_test.js <web dir>
   The layout is deterministic: every blade, flower, hedge clump, tree, impostor and rook is the same bytes twice and from a
   fresh copy of the module, their sha256 reported for the Go test to pin. The grass is GRASS blades over the near meadow,
   rooted on the land's drawn surface, none on the path, the lane or under the stone; the four wildflowers grow thickest
   along the path and the hedge's foot; the hedges run up the verge and along the valley's furlongs where the eye can see
   them; the oak stands at the left edge of a desktop's frame with its boughs over the top of a phone's and a desktop's,
   clear of the stone; poplars line the river's north bank and apples fill the orchard's rows; the woods' impostors stand
   on the far slopes, and a dozen rooks circle above them. build() makes the group with the real three.js: the InstancedMesh
   of grass, every mesh casting and receiving shadow but the impostors, which only receive, the wind in the shaders, and
   update() moving the wind and the rooks. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert"), crypto = require("crypto");
var web = process.argv[2], file = path.join(web, "splash", "flora.js"), F = require(file), L = require(path.join(web, "splash", "land.js"));
var ST = require(path.join(web, "splash", "stone.js"));

/* digest hashes everything layout() places, the trees' limbs and leaves in their order. */
function digest(mod) {
  var h = crypto.createHash("sha256"), add = function (a) { h.update(Buffer.from(a.buffer, a.byteOffset, a.byteLength)); }, lay = mod.layout();
  ["grass", "flowers", "hedges", "impostors", "rooks"].forEach(function (k) { add(lay[k]); });
  lay.trees.forEach(function (t) {
    h.update(t.kind);
    add(new Float64Array([].concat.apply([], t.limbs)));
    add(new Float64Array(t.leaves));
  });
  return h.digest("hex");
}
function count(a, size, fn) { var n = 0; for (var i = 0; i < a.length; i += size) if (fn(a.subarray(i, i + size))) n++; return n; }

(async function () {
  var THREE = await import(path.join(web, "vendor", "three.module.min.js"));

  // deterministic: the same bytes twice, and from a fresh copy of the module
  var t0 = Date.now(), hash = digest(F), ms = Date.now() - t0;
  assert.strictEqual(digest(F), hash, "the same layout twice");
  delete require.cache[require.resolve(file)];
  assert.strictEqual(digest(require(file)), hash, "the same layout from a fresh module");
  var lay = F.layout(), p = { path: F.curve(L.PATH, 0.5), lane: F.curve(L.LANE, 1), verge: F.curve(F.VERGE, 0.5) };

  // the grass: GRASS blades on the near meadow, rooted on the drawn ground, off the path, the lane and the stone's foot
  var g = lay.grass, n = g.length / 8, m = F.MEADOW, worst = 0, onPath = 0, onLane = 0, underStone = 0, lo = Infinity, hi = 0;
  assert.strictEqual(n, F.GRASS, "GRASS blades");
  assert.ok(F.GRASS >= 50000, "a dense meadow: " + F.GRASS);
  for (var i = 0; i < g.length; i += 8) {
    var x = g[i], z = g[i + 2];
    assert.ok(z >= m.z0 && z <= m.z1 && Math.abs(x) <= m.w0 + m.spread * (L.CAM_Z + 1 - z), "blade " + i / 8 + " in the meadow: " + x + ", " + z);
    worst = Math.max(worst, Math.abs(g[i + 1] - F.ground(x, z)));
    if (F.dist(p.path, x, z) < 0.22) onPath++;
    if (F.dist(p.lane, x, z) < 1.7) onLane++;
    if (Math.hypot(x - L.STONE.x, z - L.STONE.z) < 1) underStone++;
    lo = Math.min(lo, g[i + 4]); hi = Math.max(hi, g[i + 4]);
  }
  assert.ok(worst < 1e-4, "every blade rooted on the land's drawn surface: off by " + worst);
  assert.deepStrictEqual([onPath, onLane, underStone], [0, 0, 0], "no grass on the path, the lane or under the stone");
  assert.ok(lo >= F.BLADE.h[0] * 0.75 && hi <= F.BLADE.h[1] * 1.25 && hi - lo > 0.3, "blades of many heights: " + lo + " to " + hi);
  var near = count(g, 8, function (b) { return b[2] > L.CAM_Z - 6; }) / 1, far = count(g, 8, function (b) { return b[2] < L.CREST_Z + 6; });
  assert.ok(near > 1000 && far > 1000, "grass from under the camera to the crest: " + near + " near, " + far + " far");

  // the flowers: all four kinds, thickest along the path and at the hedge's foot
  var fl = lay.flowers, kinds = [0, 0, 0, 0], band = { path: 0, hedge: 0, open: 0 }, area = { path: 0, hedge: 0, open: 0 };
  for (i = 0; i < fl.length; i += 6) {
    kinds[fl[i + 5]]++;
    var dp = F.dist(p.path, fl[i], fl[i + 2]), dh = F.dist(p.verge, fl[i], fl[i + 2]);
    band[dp < 2.5 ? "path" : dh < 4 ? "hedge" : "open"]++;
    assert.ok(Math.abs(fl[i + 1] - F.ground(fl[i], fl[i + 2])) < 1e-4, "flower " + i / 6 + " on the ground");
  }
  assert.ok(kinds.every(function (k) { return k > 200; }), "poppies, cornflowers, daisies and buttercups: " + kinds);
  // the bands' areas inside the meadow, by sampling it on a 0.5 m grid
  for (var sz = m.z0; sz <= m.z1; sz += 0.5) for (var sx = -40; sx <= 40; sx += 0.5) {
    if (Math.abs(sx) > m.w0 + m.spread * (L.CAM_Z + 1 - sz) || F.meadow(sx, sz) < 0.5) continue;
    var a = F.dist(p.path, sx, sz), b = F.dist(p.verge, sx, sz);
    area[a < 2.5 ? "path" : b < 4 ? "hedge" : "open"] += 0.25;
  }
  var dens = { path: band.path / area.path, hedge: band.hedge / area.hedge, open: band.open / area.open };
  assert.ok(dens.path > 4 * dens.open && dens.hedge > 4 * dens.open, "flowers thickest along the path and the hedge: " + JSON.stringify(dens));

  // the hedges: up the verge from behind the camera to the crest, and along the valley's furlongs where the eye sees them
  var hd = lay.hedges, verge = count(hd, 7, function (c) { return c[6] === 1; }), valley = hd.length / 7 - verge;
  assert.ok(verge > 60 && valley > 600, "a hedge up the verge and hedgerows in the valley: " + verge + ", " + valley);
  assert.strictEqual(count(hd, 7, function (c) { return c[6] === 1 && (F.dist(p.verge, c[0], c[2]) > 0.5 || c[2] > 186 || c[2] < 134); }), 0, "the verge's clumps on its line");
  assert.strictEqual(count(hd, 7, function (c) { return c[6] === 0 && !F.seen(c[0], c[1] + c[5], c[2]); }), 0, "every valley clump in sight");
  assert.strictEqual(count(hd, 7, function (c) { return c[6] === 0 && Math.abs(c[2] - L.river(c[0])) < 6; }), 0, "no hedge across the river");

  // the trees: one oak, poplars along the river, apples in the orchard's rows
  var by = {};
  lay.trees.forEach(function (t) { (by[t.kind] = by[t.kind] || []).push(t); });
  assert.strictEqual(by.oak.length, 1, "one oak");
  assert.ok(by.poplar.length >= 10 && by.poplar.every(function (t) { var d = L.river(t.x) - t.z; return d > 6 && d < 10; }), "a line of poplars on the river's north bank: " + by.poplar.length);
  var o = L.ORCHARD;
  assert.ok(by.apple.length === 32 && by.apple.every(function (t) { return t.x > o.x0 && t.x < o.x1 && t.z > o.z0 && t.z < o.z1; }), "apples in the orchard's rows: " + by.apple.length);
  lay.trees.forEach(function (t) {
    assert.ok(t.limbs.length >= 4 && t.leaves.length / 8 >= 50, t.kind + " has limbs and a crown: " + t.limbs.length + ", " + t.leaves.length / 8);
    var foot = F.ground(t.x, t.z);
    assert.ok(t.limbs[0][1] < foot && t.limbs[0][1] > foot - 0.5, t.kind + "'s trunk stands in the ground");
    assert.ok(t.limbs.every(function (l) { return l.every(isFinite) && l[3] > 0 && l[7] > 0; }), t.kind + "'s limbs taper");
  });
  var oak = by.oak[0];
  assert.ok(oak.leaves.length / 8 > 2500, "the oak's crown is full: " + oak.leaves.length / 8 + " leaf cards");

  // the oak through the camera: its foot at the left edge of a desktop, its boughs into the top of a phone's and a
  // desktop's frame, and no leaf over the stone
  var view = L.camera(50), cam = new THREE.PerspectiveCamera(50, 1, 0.3, 3000), frames = {};
  cam.position.fromArray(view.eye); cam.lookAt(new THREE.Vector3().fromArray(view.at));
  var stoneTop = F.ground(L.STONE.x, L.STONE.z) + ST.H + 0.3, v = new THREE.Vector3();
  [["phone", 390 / 844], ["desktop", 1280 / 800]].forEach(function (d) {
    cam.aspect = d[1]; cam.updateProjectionMatrix(); cam.updateMatrixWorld();
    var foot = v.set(oak.x, F.ground(oak.x, oak.z) + 1, oak.z).project(cam).toArray(), top = 0, over = 0, onStone = 0;
    var st = [v.set(L.STONE.x - 0.8, stoneTop - ST.H - 0.3, L.STONE.z).project(cam).clone(), v.set(L.STONE.x + 0.8, stoneTop, L.STONE.z).project(cam).clone()];
    for (var k = 0; k < oak.leaves.length; k += 8) {
      var q = v.set(oak.leaves[k], oak.leaves[k + 1], oak.leaves[k + 2]).project(cam);
      if (q.z > 1 || Math.abs(q.x) > 1) continue;
      if (q.y > 0.85) top++;
      if (q.y > 1) over++;
      if (q.x > st[0].x && q.x < st[1].x && q.y > st[0].y && q.y < st[1].y) onStone++;
    }
    frames[d[0]] = { foot: foot.map(function (c) { return +c.toFixed(2); }), top: top, over: over };
    assert.ok(top > 150 && over > 150, d[0] + ": the oak's boughs reach into and over the top of the frame: " + top + " in its top band, " + over + " above");
    assert.strictEqual(onStone, 0, d[0] + ": no leaf over the stone");
    if (d[0] === "desktop") assert.ok(foot[0] < -0.6 && foot[0] > -1 && foot[1] < 0, "the oak stands at the desktop's left edge: " + foot);
    else assert.ok(foot[0] < -1, "the oak's trunk is off a phone's left edge, its boughs in: " + foot);
  });

  // the impostors on the far slopes and ridges, in sight; the rooks over the far wood
  var im = lay.impostors;
  assert.ok(im.length / 6 > 1000, "woods: " + im.length / 6 + " impostors");
  assert.strictEqual(count(im, 6, function (t) { return t[2] > F.NEAREST; }), 0, "no impostor nearer than NEAREST");
  assert.strictEqual(count(im, 6, function (t) { return !F.seen(t[0], t[1] + 0.6 + t[3] * 0.9, t[2]); }), 0, "every impostor in sight");
  var rk = lay.rooks;
  assert.strictEqual(rk.length / 7, F.ROOKS, "a dozen rooks");
  assert.strictEqual(count(rk, 7, function (b) { return b[2] < -100 && b[1] - F.ground(b[0], b[2]) > 25 && b[3] > 5 && Math.abs(b[4]) < 0.3; }), F.ROOKS, "circling slowly high over the far wood");

  // build: the meshes, their shadows, the wind in their shaders; update moves the wind and the rooks
  var group = F.build(THREE, null, { palette: { light: 0xffcc88, strength: 3 } }), names = {};
  group.children.forEach(function (c) { names[c.name] = c; });
  assert.deepStrictEqual(Object.keys(names).sort(), ["flowers", "grass", "hedge leaves", "hedges", "impostors", "leaves", "limbs", "rooks"], "what grows");
  assert.ok(names.grass.isInstancedMesh && names.grass.count === F.GRASS, "the grass is an InstancedMesh of GRASS blades");
  assert.strictEqual(names.grass.geometry.index.count, 6, "two triangles a blade");
  Object.keys(names).forEach(function (k) {
    var c = names[k];
    if (k === "impostors") assert.ok(!c.castShadow && c.receiveShadow, "the impostors receive shadow but cast none");
    else assert.ok(c.castShadow && c.receiveShadow, k + " casts and receives shadow");
  });
  var sh = { uniforms: {}, vertexShader: "#include <common>\n#include <beginnormal_vertex>\n#include <begin_vertex>\n#include <uv_vertex>", fragmentShader: "#include <common>\n#include <normal_fragment_begin>\n#include <emissivemap_fragment>\n#include <fog_pars_fragment>\n#include <fog_fragment>" };
  names.grass.material.onBeforeCompile(sh);
  assert.ok(/floraGust/.test(sh.vertexShader) && /bladeOff/.test(sh.vertexShader) && /uFogFall/.test(sh.fragmentShader) && /uGlow/.test(sh.fragmentShader), "the grass bends in the wind, glows and fogs");
  sh.vertexShader = "#include <common>\n#include <begin_vertex>";
  names.grass.customDepthMaterial.onBeforeCompile(sh);
  assert.ok(/bladeOff/.test(sh.vertexShader), "the grass's shadow bends with it");
  assert.ok(names.leaves.material.alphaTest > 0 || !names.leaves.material.map, "the leaf cards are cut out");
  var before = new THREE.Matrix4(), after = new THREE.Matrix4();
  names.rooks.getMatrixAt(0, before);
  F.update(group, 30);
  names.rooks.getMatrixAt(0, after);
  var u = {}; names.grass.material.onBeforeCompile({ uniforms: u, vertexShader: "", fragmentShader: "" });
  assert.ok(u.uTime.value === 30 && !before.equals(after), "update moves the wind and the rooks");
  assert.ok(u.uGlow.value.r > 0 && u.uGlow.value.r > u.uGlow.value.b, "the light glows warm through the leaves");

  var tri = 0;
  group.children.forEach(function (c) { tri += (c.geometry.index ? c.geometry.index.count : c.geometry.attributes.position.count) / 3 * (c.isInstancedMesh ? c.count : 1); });
  console.log(JSON.stringify({
    hash: hash, ms: ms, blades: n, flowers: fl.length / 6, kinds: kinds, density: { path: +dens.path.toFixed(2), hedge: +dens.hedge.toFixed(2), open: +dens.open.toFixed(2) },
    hedges: [verge, valley], trees: { oak: by.oak.length, poplar: by.poplar.length, apple: by.apple.length }, leaves: names.leaves.count + names["hedge leaves"].count,
    impostors: im.length / 6, rooks: rk.length / 7, triangles: Math.round(tri), oak: frames
  }));
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
