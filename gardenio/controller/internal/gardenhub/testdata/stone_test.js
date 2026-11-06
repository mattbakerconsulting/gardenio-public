/* The stone (web/splash/stone.js, plan gd-EJGA3zDGCk step 4), run by TestStoneUnderNode:  node stone_test.js <web dir>
   The geometry is deterministic: the body's, the plinth's and the gems' vertex positions are the same bytes twice and from a
   fresh copy of the module, their sha256 reported for the Go test to pin. The body is a closed W x H x D block with its
   arrises chipped and its dressed panel flat where the mark is cut; the plinth's two courses stand under it and proud of it.
   The mark is logo.svg's paths, the wordmark MARK_W of the face wide with the sprout above it; the gems, 7 to 9 of 2 to 4 cm
   in the four tinctures, frame it without touching it or each other. The cut's maps: white face and black floor, normals
   facing out of the cut, the floor rougher, darker and occluded. Through three.js's camera the wordmark is wide enough to
   read on a phone. build() makes the group, its materials on the right uv channels, the shaders patched. Prints a JSON
   report. */
"use strict";
var path = require("path"), assert = require("assert"), crypto = require("crypto"), fs = require("fs");
var web = process.argv[2], file = path.join(web, "splash", "stone.js"), S = require(file), L = require(path.join(web, "splash", "land.js"));
var near = function (a, b, tol, what) { assert.ok(Math.abs(a - b) <= tol, what + ": " + a + ", want " + b + " +- " + tol); };

/* digest hashes everything the builders place: the body's and the plinth's positions, each gem's shape and its seat. */
function digest(mod) {
  var h = crypto.createHash("sha256"), add = function (a) { h.update(Buffer.from(a.buffer, a.byteOffset, a.byteLength)); };
  add(mod.geometry().positions);
  add(mod.plinth().positions);
  mod.gems().forEach(function (g) { add(mod.gem(g[3])); add(new Float64Array(g.slice(0, 4))); h.update(g[4]); });
  return h.digest("hex");
}
function bounds(p) {
  var lo = [Infinity, Infinity, Infinity], hi = [-Infinity, -Infinity, -Infinity];
  for (var i = 0; i < p.length; i += 3) for (var a = 0; a < 3; a++) { lo[a] = Math.min(lo[a], p[i + a]); hi[a] = Math.max(hi[a], p[i + a]); }
  return { lo: lo, hi: hi };
}

(async function () {
  var THREE = await import(path.join(web, "vendor", "three.module.min.js"));

  // deterministic: the same bytes twice, and from a fresh copy of the module
  var t0 = Date.now(), hash = digest(S), ms = Date.now() - t0;
  assert.strictEqual(digest(S), hash, "the same geometry twice");
  delete require.cache[require.resolve(file)];
  assert.strictEqual(digest(require(file)), hash, "the same geometry from a fresh module");

  // the body: about W x H x D, closed, its normals out, every number a number
  var g = S.geometry(), n = g.positions.length / 3, b = bounds(g.positions);
  assert.ok([g.positions, g.normals, g.uv, g.uv1, g.colors].every(function (a) { return a.every(isFinite); }), "every attribute is a number");
  [S.W, S.H, S.D].forEach(function (size, a) { near(b.hi[a] - b.lo[a], size, 0.04, "the body's extent on axis " + a); });
  var pos = {}, key = function (i) { return g.positions[i * 3] + "," + g.positions[i * 3 + 1] + "," + g.positions[i * 3 + 2]; };
  for (var i = 0; i < n; i++) pos[i] = key(i);
  var edges = new Map();
  for (var t = 0; t < g.index.length; t += 3) for (var e = 0; e < 3; e++) {
    var a = pos[g.index[t + e]], c = pos[g.index[t + (e + 1) % 3]], k = a < c ? a + "|" + c : c + "|" + a;
    edges.set(k, (edges.get(k) || 0) + 1);
  }
  var open = 0;
  edges.forEach(function (v) { if (v !== 2) open++; });
  assert.strictEqual(open, 0, "the body is closed: every edge between two triangles");
  var out = 0;
  for (i = 0; i < n; i++) {
    var x = g.positions[i * 3], y = g.positions[i * 3 + 1], z = g.positions[i * 3 + 2];
    var dot = g.normals[i * 3] * x / S.W + g.normals[i * 3 + 1] * y / S.H + g.normals[i * 3 + 2] * z / S.D;
    if (dot > 0) out++;
    near(Math.hypot(g.normals[i * 3], g.normals[i * 3 + 1], g.normals[i * 3 + 2]), 1, 1e-5, "a unit normal");
  }
  assert.ok(out / n > 0.995, "the normals face out: " + out + " of " + n);

  // the dressed panel is true where the mark and the gems are; the chips bite the arrises elsewhere, a few centimetres deep
  var lay = S.layout(), box = lay.box, flat = 0, worst = 0, bitten = 0;
  for (i = 0; i < n; i++) {
    x = g.positions[i * 3]; y = g.positions[i * 3 + 1]; z = g.positions[i * 3 + 2];
    if (g.normals[i * 3 + 2] > 0.9 && Math.abs(x) < 0.47 && y > -0.3 && y < 0.46) { flat++; worst = Math.max(worst, Math.abs(z - S.D / 2)); }
  }
  assert.ok(flat > 20000 && worst < 0.004, "the panel is dressed true: " + flat + " vertices within " + worst + " m");
  S.chips.forEach(function (ch) { if (ch[3] * ch[4] / 2 > 0.02) bitten++; });
  assert.ok(S.chips.length >= 12 && bitten >= 4, "chips on the arrises, some deep: " + S.chips.length + ", " + bitten);
  var lowest = S.surface(0, 0);
  near(lowest, S.D / 2, 0.003, "the face's surface at its centre");

  // the plinth: two courses under the stone, proud of it all round, its foot at 0
  var pl = S.plinth(), pb = bounds(pl.positions), top = S.COURSES[0] + S.COURSES[1];
  assert.ok(pl.positions.every(isFinite) && pl.normals.every(isFinite), "the plinth's numbers");
  near(pb.lo[1], 0, 0.03, "the plinth's foot");
  near(pb.hi[1], top, 0.03, "the plinth's top");
  assert.ok(pb.hi[0] - pb.lo[0] > S.W + S.OUT && pb.hi[2] - pb.lo[2] > S.D + S.OUT, "the plinth stands proud of the stone: " + JSON.stringify(pb));
  var st = S.stones();
  assert.ok(st.length >= 16 && st.every(function (s) { return s.h.every(function (h) { return h > 0.03; }); }), "rubble in courses: " + st.length + " stones");

  // the mark: logo.svg's paths, the wordmark MARK_W of the face wide, the sprout above it, inside the panel
  var svg = fs.readFileSync(path.join(web, "..", "..", "..", "..", "keycloak-theme", "gardenio", "login", "resources", "img", "logo.svg"), "utf8");
  var ds = []; svg.replace(/\sd="([^"]+)"/g, function (m, d) { ds.push(d); });
  assert.deepStrictEqual(S.SPROUT.concat([S.WORD]), ds, "the mark is the logo's paths, in order");
  near(box[2] - box[0], S.MARK_W * S.W, 1e-9, "the wordmark's width");
  near(box[0] + box[2], 0, 1e-9, "the mark is centred");
  assert.ok(lay.sproutBox[1] > lay.wordBox[3] && box[1] > -0.3 && box[3] < 0.46, "the sprout above the wordmark, both in the panel: " + JSON.stringify(lay));
  near(lay.sproutBox[0] + lay.sproutBox[2], -0.2 * lay.sprout.k, 1e-9, "the sprout centred on its stem");

  // the gems: 7 to 9, 2 to 4 cm, four tinctures, framing the mark without touching it or each other, seated on the face
  var gs = S.gems(), tinct = {};
  assert.ok(gs.length >= 7 && gs.length <= 9, "7 to 9 gems: " + gs.length);
  gs.forEach(function (gm, gi) {
    tinct[gm[4]] = (tinct[gm[4]] || 0) + 1;
    assert.ok(gm[3] >= 0.02 && gm[3] <= 0.04 && S.TINCTURES[gm[4]], "gem " + gi + " is 2 to 4 cm in a tincture: " + gm);
    [lay.sproutBox, lay.wordBox].forEach(function (bx) {
      var dx = Math.max(bx[0] - gm[0], 0, gm[0] - bx[2]), dy = Math.max(bx[1] - gm[1], 0, gm[1] - bx[3]);
      assert.ok(Math.hypot(dx, dy) > gm[3] / 2 + 0.02, "gem " + gi + " clear of the mark: " + gm + " by " + bx);
    });
    assert.ok(Math.abs(gm[0]) + gm[3] / 2 < 0.49 && Math.abs(gm[1]) + gm[3] / 2 < 0.73, "gem " + gi + " on the dressed panel");
    near(gm[2], S.D / 2, 0.005, "gem " + gi + " seated on the face");
    gs.forEach(function (o, oi) { if (oi > gi) assert.ok(Math.hypot(o[0] - gm[0], o[1] - gm[1]) > (o[3] + gm[3]) / 2 + 0.05, "gems " + gi + " and " + oi + " apart"); });
  });
  assert.deepStrictEqual(Object.keys(tinct).sort(), ["emerald", "garnet", "sapphire", "topaz"], "the four tinctures");
  var facets = S.gem(0.03), gb = bounds(facets);
  assert.strictEqual(facets.length, 32 * 9, "a low-poly brilliant: 32 facets");
  near(gb.hi[0] - gb.lo[0], 0.03, 1e-6, "the girdle's diameter");
  assert.ok(gb.hi[2] > 0.003 && gb.lo[2] < -0.01, "the crown proud, the pavilion buried: " + gb.lo[2] + ", " + gb.hi[2]);

  // the cut's maps, from a square cut in the middle of a small field
  var m = 64, mask = new Float32Array(m * m);
  for (var j = 24; j < 40; j++) for (i = 24; i < 40; i++) mask[j * m + i] = 1;
  var blurred = S.blur(mask, m, 2), sum = function (a) { return a.reduce(function (p, v) { return p + v; }, 0); };
  near(sum(blurred), sum(mask), 1e-3, "the blur keeps the cut's area");
  var maps = S.cutMaps(mask, m), px = function (a, i, j) { return Array.from(a.slice((j * m + i) * 4, (j * m + i) * 4 + 4)); };
  assert.deepStrictEqual(px(maps.disp, 4, 4).slice(0, 3), [255, 255, 255], "the face undisplaced");
  assert.deepStrictEqual(px(maps.disp, 32, 32).slice(0, 3), [0, 0, 0], "the floor DEPTH down");
  assert.deepStrictEqual(px(maps.normal, 4, 4).slice(0, 3), [128, 128, 255], "the face's normal straight out");
  var lw = px(maps.normal, 24, 32), bw = px(maps.normal, 32, 24);
  assert.ok(lw[0] > 200 && Math.abs(lw[1] - 128) < 2, "the left wall faces right, into the cut: " + lw);
  assert.ok(bw[1] > 200 && Math.abs(bw[0] - 128) < 2, "the lower wall faces up: " + bw);
  var face = px(maps.orm, 4, 4), floor = px(maps.orm, 32, 32);
  assert.ok(floor[0] < face[0] && floor[1] > face[1] && floor[2] < face[2], "the floor occluded, duller, darker: " + face + " / " + floor);
  var big = S.cutMaps(new Float32Array(S.MAP * S.MAP), S.MAP), stripPx = Array.from(big.orm.slice((10 * S.MAP + 1000) * 4, (10 * S.MAP + 1000) * 4 + 3));
  assert.ok(stripPx[1] > 220 && stripPx[0] === 255, "the plain strip rough, unoccluded: " + stripPx);

  // through the camera: the stone right of centre on a phone and a desktop, the wordmark wide enough to read on the phone
  var p = S.place(), view = L.camera(50), cam = new THREE.PerspectiveCamera(50, 1, 0.3, 3000);
  cam.position.fromArray(view.eye); cam.lookAt(new THREE.Vector3().fromArray(view.at));
  var foot = Infinity;
  [[-1, -1], [1, -1], [-1, 1], [1, 1]].forEach(function (c) { foot = Math.min(foot, L.height(p.x + c[0], p.z + c[1])); });
  assert.ok(p.foot < foot && p.foot > foot - 0.2, "the plinth's foot sunk in the turf: " + p.foot + ", ground " + foot);
  near(p.z, L.CAM_Z - 8, 1e-9, "on the brow 8 m ahead");
  var group = S.build(THREE), world = function (x, y) { group.updateMatrixWorld(); return new THREE.Vector3(x, y, S.D / 2).applyMatrix4(group.matrixWorld); };
  var frames = {};
  [["phone", 390, 844], ["desktop", 1280, 800]].forEach(function (d) {
    cam.aspect = d[1] / d[2]; cam.updateProjectionMatrix(); cam.updateMatrixWorld();
    var l = world(box[0], box[1] + 0.06).project(cam), r = world(box[2], box[1] + 0.06).project(cam), c = world(0, 0).project(cam);
    var wpx = (r.x - l.x) / 2 * d[1];
    assert.ok(c.x > 0.02 && c.x < 0.8 && Math.abs(c.y) < 0.7, d[0] + ": the stone right of centre and in view: " + c.toArray());
    assert.ok(wpx > 70, d[0] + ": the wordmark " + wpx.toFixed(0) + " px wide, legible");
    frames[d[0]] = { centre: [+c.x.toFixed(2), +c.y.toFixed(2)], wordmark_px: Math.round(wpx) };
  });

  // build: the body, the plinth under it and the gems, on the land; materials on their channels; shaders patched
  var body = group.getObjectByName("stone body"), rubble = group.getObjectByName("plinth"), gems = group.children.filter(function (o) { return /^gem /.test(o.name); });
  assert.ok(body.isMesh && rubble.isMesh && gems.length === gs.length, "the body, the plinth and the gems");
  assert.ok(body.castShadow && body.receiveShadow && rubble.castShadow && rubble.receiveShadow, "the stone and the plinth throw and take shadow");
  ["position", "normal", "uv", "uv1", "color"].forEach(function (k) { assert.ok(body.geometry.attributes[k], "the body has " + k); });
  assert.ok(body.material.map && body.material.map.wrapS === THREE.RepeatWrapping && body.material.map.colorSpace === THREE.SRGBColorSpace && body.material.vertexColors, "granite grain under the weathering");
  group.updateMatrixWorld();
  var bb = new THREE.Box3().setFromObject(rubble);
  near(bb.min.y, p.foot, 0.03, "the plinth stands on its foot");
  near(new THREE.Box3().setFromObject(body).min.y, bb.max.y, 0.04, "the stone stands on the plinth");
  near(group.rotation.y, S.TURN, 1e-12, "turned to the camera");
  gems.forEach(function (gm) {
    assert.ok(gm.material.isMeshPhysicalMaterial && gm.material.transmission > 0 && gm.material.clearcoat === 1 && gm.material.emissiveIntensity > 0, gm.name + ": transmission, clearcoat, a touch of emissive");
    var sh = { uniforms: {}, fragmentShader: "void main() {\n#include <lights_fragment_end>\n}" };
    gm.material.onBeforeCompile(sh);
    assert.ok(sh.uniforms.uZenith.value.isColor && /indirectSpecular \+= gemSky/.test(sh.fragmentShader), gm.name + ": the facets reflect the sky");
  });
  var fog = { uniforms: {}, fragmentShader: "#include <color_fragment>\n#include <fog_pars_fragment>\n#include <fog_fragment>" };
  body.material.onBeforeCompile(fog);
  assert.ok(/uFogFall/.test(fog.fragmentShader) && !/vRoughnessMapUv/.test(fog.fragmentShader), "without the mark: height fog, no tint");
  // the mark's maps on channel 1, as build() sets them when it has a document (a stand-in canvas whose mask is empty)
  var doc = { defaultView: { Path2D: function () {} }, createElement: function () {
    return { getContext: function () {
      return new Proxy({}, { get: function (o, k) { return k === "getImageData" ? function () { return { data: new Uint8ClampedArray(S.MAP * S.MAP * 4) }; } : k in o ? o[k] : function () {}; }, set: function (o, k, v) { o[k] = v; return true; } });
    } };
  } };
  var marked = S.build(THREE, doc).getObjectByName("stone body").material;
  assert.ok([marked.displacementMap, marked.normalMap, marked.roughnessMap, marked.aoMap].every(function (tx) { return tx && tx.channel === 1; }), "the mark's maps on uv1");
  assert.ok(marked.displacementScale === S.DEPTH && marked.displacementBias === -S.DEPTH && marked.roughnessMap === marked.aoMap, "cut DEPTH m in; ambient occlusion and roughness one map");
  var tint = { uniforms: {}, fragmentShader: "#include <color_fragment>\n#include <fog_pars_fragment>\n#include <fog_fragment>" };
  marked.onBeforeCompile(tint);
  assert.ok(/diffuseColor\.rgb \*= texture2D\(roughnessMap, vRoughnessMapUv\)\.b/.test(tint.fragmentShader) && /uFogFall/.test(tint.fragmentShader), "the cut's floor tinted from the map's blue");
  var grain = S.granitePixels(32);
  assert.deepStrictEqual(grain, S.granitePixels(32), "the granite is the same every time");

  console.log(JSON.stringify({ hash: hash, ms: ms, vertices: n, triangles: g.index.length / 3, plinth: pl.positions.length / 3, stones: st.length, gems: gs.length, chips: S.chips.length, panel_mm: +(worst * 1000).toFixed(2), frames: frames }));
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
