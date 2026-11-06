/* The farm (web/splash/farm.js, plan gd-EJGA3zDGCk step 6), run by TestFarmUnderNode:  node splash_farm_test.js <web dir>
   The layout is deterministic: the buildings, windows, walls, coping, bales, paddocks, posts, beasts, bridge, pond and birds
   are the same numbers twice and from a fresh copy of the module, their sha256 reported for the Go test to pin. The clock:
   isLit flips at civil dusk and at civil dawn (found here by bisecting the sun's elevation, not by asking the farm), the
   chimney smokes from an hour before sunset to sunrise, swifts fly by day and bats after dusk. The farmstead: the house and
   the barn floored at their doors' ground and in view from the eye, every window on its wall, the gate's posts either side
   of the lane, the walls running out from them on the ground, the bales on the cut hay, the beasts in their fenced pasture,
   the footbridge over the river, the pond's water under the eye and over the crest's line, and no hedge or wood standing in
   a building. build() makes the meshes with the real three.js, sets the hour, mirrors the pond when asked and moves the
   smoke, the grazing and the birds. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert"), crypto = require("crypto");
var web = process.argv[2], file = path.join(web, "splash", "farm.js"), F = require(file);
var L = require(path.join(web, "splash", "land.js")), FL = require(path.join(web, "splash", "flora.js")), A = require(path.join(web, "almanac.js"));

function digest(mod) { return crypto.createHash("sha256").update(JSON.stringify(mod.layout(), function (k, v) { return ArrayBuffer.isView(v) ? Array.from(v) : v; })).digest("hex"); }
/* when finds the moment between a and b (unix ms) at which the sun's elevation crosses el, to the millisecond. */
function when(a, b, el, lat, lon) {
  var s = function (t) { return A.sun(t, lat, lon).elevation - el; }, sa = s(a);
  assert.ok(sa * s(b) < 0, "the sun crosses " + el + " between " + new Date(a).toISOString() + " and " + new Date(b).toISOString());
  while (b - a > 1) { var m = Math.floor((a + b) / 2); if (s(m) * sa > 0) a = m; else b = m; }
  return b;
}
var H = 3600000, MIN = 60000;

(async function () {
  var THREE = await import(path.join(web, "vendor", "three.module.min.js"));

  // deterministic: the same numbers twice, and from a fresh copy of the module
  var t0 = Date.now(), hash = digest(F), ms = Date.now() - t0;
  assert.strictEqual(digest(F), hash, "the same layout twice");
  delete require.cache[require.resolve(file)];
  assert.strictEqual(digest(require(file)), hash, "the same layout from a fresh module");

  // the clock, at the splash's default place and in an English midsummer
  var clock = [];
  [[Date.UTC(2026, 9, 26), 45, 0], [Date.UTC(2026, 5, 21), 52, -1.5]].forEach(function (c) {
    var day = c[0], lat = c[1], lon = c[2], noon = A.noon(day + 12 * H, lon);
    var sunset = when(noon, noon + 12 * H, F.SET, lat, lon), dusk = when(noon, noon + 12 * H, F.CIVIL, lat, lon);
    var dawn = when(noon + 12 * H, noon + 24 * H, F.CIVIL, lat, lon), sunrise = when(noon + 12 * H, noon + 24 * H, F.SET, lat, lon);
    var lit = function (t) { return F.isLit(t, lat, lon); }, smoke = function (t) { return F.smoking(t, lat, lon); };
    assert.deepStrictEqual([lit(noon), lit(dusk - MIN), lit(dusk + MIN), lit(noon + 12 * H), lit(dawn - MIN), lit(dawn + MIN)], [false, false, true, true, true, false],
      "the windows light at civil dusk (" + new Date(dusk).toISOString() + ") and go out at civil dawn (" + new Date(dawn).toISOString() + ")");
    assert.deepStrictEqual([lit(dusk - 1000), lit(dusk + 1000), lit(dawn - 1000), lit(dawn + 1000)], [false, true, true, false], "to the second");
    assert.deepStrictEqual([smoke(noon), smoke(sunset - H - MIN), smoke(sunset - H + MIN), smoke(sunset + H), smoke(noon + 12 * H), smoke(sunrise - MIN), smoke(sunrise + MIN)],
      [false, false, true, true, true, true, false], "the chimney smokes from an hour before sunset to sunrise");
    var h = [noon, sunset - 10 * MIN, sunset + 10 * MIN, dusk + MIN, noon + 12 * H, dawn + MIN].map(function (t) { var o = F.hours(t, lat, lon); return [o.swifts, o.bats]; });
    assert.deepStrictEqual(h, [[true, false], [true, false], [false, false], [false, true], [false, true], [false, false]], "swifts by day, bats from dusk to dawn");
    clock.push({ lat: lat, sunset: new Date(sunset).toISOString(), dusk: new Date(dusk).toISOString(), dawn: new Date(dawn).toISOString(), sunrise: new Date(sunrise).toISOString() });
  });

  var lay = F.layout(), eye = L.camera(50).eye, near = 1e-3;
  // the house and the barn: floored at their doors, standing on the yard's terrace near the farm's middle, in view
  [["house", F.HOUSE], ["barn", F.BARN]].forEach(function (b) {
    var o = lay[b[0]], door = F.P(o.frame, b[1].door[0], 0, b[1].door[1]);
    assert.ok(Math.abs(o.frame.y - FL.ground(door[0], door[2])) < near, b[0] + " floored at its door's ground");
    assert.ok(o.low < o.frame.y && o.corners.every(function (c) { return FL.ground(c[0], c[1]) >= o.low; }), b[0] + "'s walls reach down to the ground at every corner");
    assert.ok(o.corners.every(function (c) { return Math.hypot(c[0] - L.FARM.x, c[1] - L.FARM.z) < L.FARM.r; }), b[0] + " inside the farmstead");
    var top = F.P(o.frame, 0, o.ridge, 0);
    assert.ok(FL.seen(top[0], top[1], top[2]) && FL.seen(door[0], door[1] + 2.2, door[2]), b[0] + " seen over the crest from the eye");
  });
  // every window on its building's wall, most lit at night, the house's front windows seen from the eye
  var walls = 0, front = 0;
  lay.windows.forEach(function (w) {
    var b = w.kind === "barn" ? lay.barn : lay.house, f = b.frame, dx = w.at[0] - f.x, dz = w.at[2] - f.z;
    var u = dx * f.fz - dz * f.fx, v = dx * f.fx + dz * f.fz, onFace = Math.abs(Math.abs(u) - b.len / 2 - 0.03) < 0.01 || Math.abs(Math.abs(v) - b.depth / 2 - 0.03) < 0.01;
    if (onFace || w.kind === "dormer") walls++;
    if (w.kind === "house" && v > b.depth / 2 && FL.seen(w.at[0], w.at[1], w.at[2])) front++;
  });
  assert.strictEqual(walls, lay.windows.length, "every window on a wall or a dormer");
  assert.ok(front >= 6, "the house's front windows seen from the eye: " + front);
  var on = lay.windows.filter(function (w) { return w.on; }).length;
  assert.ok(on >= lay.windows.length * 0.6 && on < lay.windows.length, "most windows lit at night, not all: " + on + " of " + lay.windows.length);
  assert.ok(lay.chimneys.length === 2 && lay.chimneys.every(function (c) { return c[1] > lay.house.frame.y + F.HOUSE.ridge + 1; }), "two chimneys, their pots over the ridge");

  // the gate across the lane's way into the yard, the walls running out from its posts along the ground
  var g = lay.gate, laneAt = g.lane, gap = Math.hypot(g.posts[0][0] - g.posts[1][0], g.posts[0][2] - g.posts[1][2]);
  assert.ok(gap > 3 && gap < 4.5 && g.posts.every(function (p) { return Math.abs(Math.hypot(p[0] - laneAt[0], p[2] - laneAt[1]) - gap / 2) < near; }), "the gate's posts either side of the lane: " + gap);
  assert.ok(g.posts[0][0] < g.posts[1][0] && g.leaf[1] < -0.5, "hung on the west post, swung open into the yard: " + g.leaf);
  var lane = FL.curve(L.LANE, 0.5), wallPts = 0;
  lay.walls.forEach(function (run, k) {
    assert.ok(Math.abs(run[0] - g.posts[k][0]) < near && Math.abs(run[2] - g.posts[k][2]) < near, "wall run " + k + " starts at its gate post");
    for (var i = 0; i < run.length; i += 3) {
      wallPts++;
      assert.ok(Math.abs(run[i + 1] - FL.ground(run[i], run[i + 2])) < near, "the wall on the ground");
      assert.ok(FL.dist(lane, run[i], run[i + 2]) > 1.4, "no wall across the lane: " + run[i] + ", " + run[i + 2]);
    }
  });
  assert.ok(lay.coping.length / 6 > 120, "a coping of upright stones: " + lay.coping.length / 6);

  // the bales on the cut hay, in sight; the fenced pasture beside it with its beasts
  var bl = lay.bales, fields = L.fields(), hay = 0;
  function cropAt(x, z) { // the crop LAND's painter gives the strip at x, z
    for (var i = 0; i < fields.length; i++) {
      var f = fields[i];
      if (f.kind !== "field" || !F.inside(f.quad, x, z, 0)) continue;
      var xs = f.quad.map(function (p) { return p[0]; }), zs = f.quad.map(function (p) { return p[1]; });
      var lo = f.along === "z" ? Math.min.apply(null, xs) - 2 : Math.min.apply(null, zs) - 2, at = f.along === "z" ? x : z;
      return f.crops[Math.floor((at - lo) / f.strip) % 2];
    }
    return null;
  }
  for (var i = 0; i < bl.length; i += 7) {
    if (cropAt(bl[i], bl[i + 2]) === "hay") hay++;
    assert.ok(Math.abs(bl[i + 1] - bl[i + 5] + 0.06 - FL.ground(bl[i], bl[i + 2])) < near && FL.seen(bl[i], bl[i + 1] + bl[i + 5], bl[i + 2]), "bale " + i / 7 + " lying on the ground in sight");
  }
  assert.ok(bl.length / 7 >= 30 && hay === bl.length / 7, "round bales on the cut hay: " + hay + " of " + bl.length / 7);
  assert.strictEqual(lay.paddocks.length, F.PADDOCKS, "the paddocks");
  lay.paddocks.forEach(function (p) {
    var c = [0, 0]; p.ring.forEach(function (q) { c[0] += q[0] / p.ring.length; c[1] += q[1] / p.ring.length; });
    assert.strictEqual(cropAt(c[0], c[1]), "pasture", "a paddock is pasture");
  });
  var bs = lay.beasts, kinds = [0, 0];
  for (i = 0; i < bs.length; i += 7) {
    kinds[bs[i + 5]]++;
    var mine = lay.paddocks.some(function (p) { return F.inside(p.ring, bs[i], bs[i + 2], 0.5); });
    assert.ok(mine && Math.abs(bs[i + 1] - FL.ground(bs[i], bs[i + 2])) < near && Math.abs(Math.hypot(bs[i + 3], bs[i + 4]) - 1) < 1e-6, "beast " + i / 7 + " standing inside a fence");
  }
  assert.ok(kinds[0] >= F.COWS && kinds[1] >= 4, "cattle and sheep: " + kinds);
  for (i = 0; i < lay.posts.length; i += 3) assert.ok(Math.abs(lay.posts[i + 1] - FL.ground(lay.posts[i], lay.posts[i + 2])) < near, "post on the ground");

  // the footbridge over the river by the lane's ford, its ends on either bank
  var br = lay.bridge, rz = L.river(br.at[0]);
  assert.ok(br.at[1] - br.len / 2 < rz - 3 && br.at[1] + br.len / 2 > rz + 3 && FL.dist(lane, br.at[0], br.at[1]) < 8, "the footbridge spans the river beside the lane");
  assert.ok(F.deckY(br, 0.5) > FL.ground(br.at[0], rz) + 1.5 && Math.abs(F.deckY(br, 0) - FL.ground(br.at[0], br.at[1] - br.len / 2) - 0.25) < near, "its deck clears the water and lands on the banks");

  // the pond: water under the eye, its near rim over the crest's line, in the land's hollow
  var pd = lay.pond;
  assert.ok(pd.y < eye[1] - 2 && FL.seen(pd.x, pd.y, pd.z + pd.r - 0.5) && FL.seen(pd.x, pd.y, pd.z - pd.r + 0.5), "the pond's water under the eye and seen from its near rim to its far one");
  var bank = Infinity, deep = -Infinity;
  for (var a = 0; a < 64; a++) {
    var sx = Math.sin(a / 64 * 2 * Math.PI), sz = Math.cos(a / 64 * 2 * Math.PI);
    bank = Math.min(bank, FL.ground(pd.x + pd.r * sx, pd.z + pd.r * sz) - pd.y);
    deep = Math.max(deep, FL.ground(pd.x + pd.r * 0.6 * sx, pd.z + pd.r * 0.6 * sz) - pd.y);
  }
  assert.ok(FL.ground(pd.x, pd.z) < pd.y - 0.5 && deep < 0 && bank > 0, "the water fills its hollow, the bank over its edge all round: " + deep + ", " + bank);

  // nothing growing or standing in the buildings
  var fl = FL.layout(), hd = fl.hedges, im = fl.impostors;
  [lay.house, lay.barn].forEach(function (b) {
    for (var k = 0; k < hd.length; k += 7) assert.ok(!F.inside(b.corners, hd[k], hd[k + 2], -hd[k + 4] * 0.5), "no hedge in the " + (b === lay.house ? "house" : "barn"));
    for (k = 0; k < im.length; k += 6) assert.ok(!F.inside(b.corners, im[k], im[k + 2], -im[k + 4] * 0.5), "no wood in a building");
  });
  // the swifts over the yard, the bats round the barn, all in the air
  [["swifts", F.SWIFTS], ["bats", F.BATS]].forEach(function (b) {
    var a = lay[b[0]];
    assert.strictEqual(a.length / 8, b[1], b[0]);
    for (var k = 0; k < a.length; k += 8) assert.ok(a[k + 4] - FL.ground(a[k], a[k + 1]) > 2, b[0] + " fly above the ground");
  });

  // the textures: tiling pixels, made in code
  var px = F.stonePixels(32, [196, 182, 156], 0.012, 0.62, 1), sl = F.slatePixels(32, 2), tb = F.timberPixels(32, 3);
  assert.ok(px.length === 32 * 32 * 4 && sl.length === px.length && tb.length === px.length, "stone, slate and timber pixels");
  assert.ok(new Set(Array.from(px.filter(function (v, k) { return k % 4 === 0; }))).size > 20, "the stone's courses vary");

  // build: the meshes, the hour, the pond, and update moving the smoke, the heads and the birds
  var noon = F.build(THREE, null, { t: Date.UTC(2026, 9, 26, 12), lat: 45, lon: 0 }), names = {};
  noon.children.forEach(function (c) { names[c.name] = c; });
  assert.deepStrictEqual(Object.keys(names).sort(), ["bales", "bats", "cattle", "coping", "dry wall", "farm dark", "farm glass", "farm hay", "farm paint", "farm slate", "farm stone", "farm timber",
    "halos", "lamp", "lamp pool", "lit windows", "pond", "posts", "rails", "sheep", "smoke", "swifts"], "the farm's parts");
  ["farm stone", "farm slate", "farm timber", "dry wall", "bales", "cattle", "coping", "posts"].forEach(function (k) { assert.ok(names[k].castShadow && names[k].receiveShadow, k + " casts and receives shadow"); });
  assert.ok(names.cattle.isInstancedMesh && names.cattle.count === kinds[0] && names.sheep.count === kinds[1] && names.bales.count === bl.length / 7, "the beasts and the bales are instanced");
  assert.deepStrictEqual(["lit windows", "halos", "lamp pool", "smoke", "swifts", "bats"].map(function (k) { return names[k].visible; }), [false, false, false, false, true, false], "noon: dark windows, a cold chimney, swifts");
  assert.ok(!names.pond.userData.reflect && names.pond.material.isMeshBasicMaterial, "the pond a tinted mirror colour without opts.reflect");
  var night = F.build(THREE, null, { t: Date.UTC(2026, 9, 26, 22), lat: 45, lon: 0, reflect: true }), pond = night.getObjectByName("pond");
  assert.deepStrictEqual(["lit windows", "halos", "lamp pool", "smoke", "swifts", "bats"].map(function (k) { return night.getObjectByName(k).visible; }), [true, true, true, true, false, true], "night: lit windows, smoke, bats");
  assert.ok(pond.userData.reflect && pond.material.isShaderMaterial && pond.userData.target.isWebGLRenderTarget && pond.userData.camera.layers.mask === 1 << F.REFLECT && typeof pond.onBeforeRender === "function", "the pond a mirror when asked");
  var vs = { uniforms: {}, vertexShader: "#include <common>\n#include <beginnormal_vertex>\n#include <begin_vertex>", fragmentShader: "#include <common>\n#include <fog_pars_fragment>\n#include <fog_fragment>" };
  names.cattle.material.onBeforeCompile(vs);
  assert.ok(/farmGraze/.test(vs.vertexShader) && /aHead/.test(vs.vertexShader) && /uFogFall/.test(vs.fragmentShader), "the cattle lower their heads to graze, in the land's fog");
  var bird = new THREE.Matrix4(), bird2 = new THREE.Matrix4(), u = {};
  names.swifts.getMatrixAt(0, bird);
  F.update(noon, 12);
  names.swifts.getMatrixAt(0, bird2);
  names.smoke.material.onBeforeCompile({ uniforms: u, vertexShader: "", fragmentShader: "" });
  assert.ok(!bird.equals(bird2) && u.uTime.value === 12, "update flies the swifts and moves the smoke and the heads");

  var tri = 0;
  noon.children.forEach(function (c) { if (!c.isPoints) tri += (c.geometry.index ? c.geometry.index.count : c.geometry.attributes.position.count) / 3 * (c.isInstancedMesh ? c.count : 1); });
  console.log(JSON.stringify({ hash: hash, ms: ms, clock: clock, windows: [on, lay.windows.length], front: front, wall: wallPts, coping: lay.coping.length / 6, bales: bl.length / 7,
    paddocks: lay.paddocks.map(function (p) { return p.kind; }), beasts: kinds, posts: lay.posts.length / 3, pond: +pd.y.toFixed(2), eye: +eye[1].toFixed(2), triangles: Math.round(tri) }));
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
