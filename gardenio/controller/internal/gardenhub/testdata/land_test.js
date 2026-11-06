/* The land (web/splash/land.js, plan gd-EJGA3zDGCk step 3), run by TestLandUnderNode:  node land_test.js <web dir>
   The heightfield is deterministic: the same bytes twice and from a fresh copy of the module, its sha256 reported for the Go
   test to pin. The camera lies EYE m above the near slope with the crest about 20 m ahead; the crest's summit (the knoll the
   stone stands on) is higher than the eye and 3 m above the camera's feet; the valley floor and the river are far below it;
   the near ridge and the far ridge stand above the crest, the far one higher. Through the real three.js camera the crest
   sits a third of the way up the frame and the stone right of centre, on a phone and a desktop, and the farm and the pond
   show over the crest, the pond's water under the eye. The painter lays out every crop, the hedges, the orchard, the woods, the river, the pond, the lane
   and the path, the same shapes in both layers. build() makes the field and the far ring, the ring under the field. Prints
   a JSON report. */
"use strict";
var path = require("path"), assert = require("assert"), crypto = require("crypto");
var web = process.argv[2], file = path.join(web, "splash", "land.js"), L = require(file);
var near = function (a, b, tol, what) { assert.ok(Math.abs(a - b) <= tol, what + ": " + a + ", want " + b + " +- " + tol); };
function sha(a) { return crypto.createHash("sha256").update(Buffer.from(a.buffer, a.byteOffset, a.byteLength)).digest("hex"); }
function round(v) { return +v.toFixed(2); }

(async function () {
  var THREE = await import(path.join(web, "vendor", "three.module.min.js"));

  // deterministic: the same bytes twice, and from a fresh copy of the module (no state carried between calls)
  var hs = L.heightfield(), hash = sha(hs);
  assert.strictEqual(hs.length, L.N * L.N, "257 x 257 heights");
  assert.ok(hs.every(isFinite), "every height is a number");
  assert.strictEqual(sha(L.heightfield()), hash, "the same heightfield twice");
  delete require.cache[require.resolve(file)];
  assert.strictEqual(sha(require(file).heightfield()), hash, "the same heightfield from a fresh module");
  assert.strictEqual(hs[(L.N - 1) / 2 * L.N + (L.N - 1) / 2], Math.fround(L.height(0, 0)), "the middle vertex is height(0, 0)");

  // the camera on the near slope, the crest ahead and above, the valley below
  var cam = L.camera(50), eye = cam.eye, feet = L.height(0, L.CAM_Z);
  near(eye[1] - feet, 1.4, 1e-9, "the eye above the slope");
  near(L.CAM_Z - cam.crest[2], 20, 3, "the crest ahead of the camera, in m");
  var summit = -Infinity, valley = Infinity, ridge1 = -Infinity;
  for (var x = -15; x <= 15; x += 0.25) for (var z = L.CREST_Z - 6; z <= L.CREST_Z + 6; z += 0.25) summit = Math.max(summit, L.height(x, z));
  for (x = -150; x <= 150; x += 2) {
    for (z = 0; z <= 100; z += 2) valley = Math.min(valley, L.height(x, z));
    for (z = -200; z <= -100; z += 2) ridge1 = Math.max(ridge1, L.height(x, z));
  }
  assert.ok(summit > eye[1], "the crest is higher than the camera: summit " + summit + ", eye " + eye[1]);
  near(summit - feet, 3, 0.5, "the crest's summit above the camera's feet");
  assert.ok(valley < eye[1] - 20, "the valley is lower than the camera: floor " + valley + ", eye " + eye[1]);
  assert.ok(L.height(0, L.river(0)) < L.height(0, L.river(0) - 12) && L.height(0, L.river(0)) < L.height(0, L.river(0) + 12), "the river runs in a channel");
  var ridge2 = L.height(0, -330);
  assert.ok(ridge1 > summit + 20 && ridge2 > ridge1, "the near ridge above the crest and the far ridge above it: " + ridge1 + ", " + ridge2);
  assert.ok(cam.pitch > 2 && cam.pitch < 10, "pitched up a few degrees: " + cam.pitch);

  // through the camera: the crest a third of the way up, the stone right of centre, at a phone's and a desktop's aspect
  var c = new THREE.PerspectiveCamera(50, 1, 0.3, 3000), frames = {};
  c.position.fromArray(eye); c.lookAt(new THREE.Vector3().fromArray(cam.at));
  var stone = new THREE.Vector3(L.STONE.x, L.height(L.STONE.x, L.STONE.z) + 0.8, L.STONE.z);
  [["phone", 390 / 844], ["desktop", 1280 / 800]].forEach(function (a) {
    c.aspect = a[1]; c.updateProjectionMatrix(); c.updateMatrixWorld();
    var cr = new THREE.Vector3().fromArray(cam.crest).project(c), st = stone.clone().project(c);
    near(cr.y, -1 / 3, 0.01, a[0] + ": the crest's height in the frame");
    assert.ok(st.x > 0.05 && st.x < 0.9 && Math.abs(st.y) < 0.9, a[0] + ": the stone right of centre and in view: " + st.toArray());
    frames[a[0]] = { crest: round(cr.y), stone: [round(st.x), round(st.y)] };
  });
  // the farm and the pond show over the crest: above the line from the eye over it; and the pond's water, all of it from its
  // near rim back, lies under the eye, or its surface could not be seen at all
  var over = function (x, z, y) {
    var d = Math.sqrt(x * x + (L.CAM_Z - z) * (L.CAM_Z - z));
    return (y == null ? L.height(x, z) : y) - (eye[1] + (cam.crest[1] - eye[1]) / (L.CAM_Z - cam.crest[2]) * d);
  };
  var water = L.pondLevel(), rim = over(L.POND.x, L.POND.z + L.POND.r, water);
  assert.ok(over(L.FARM.x, L.FARM.z) > 2 && rim > 1, "the farm and the pond's near rim in view: " + over(L.FARM.x, L.FARM.z) + ", " + rim);
  assert.ok(water < eye[1] - 2, "the pond's water under the eye: " + water + " against " + eye[1]);

  // the painter: every crop and feature, the same shapes in both layers
  function ctx2d() {
    var n = { fills: 0, strokes: 0 };
    return new Proxy(n, {
      get: function (t, k) { return k in t ? t[k] : function () { if (k === "fill" || k === "fillRect") t.fills++; if (k === "stroke" || k === "strokeRect") t.strokes++; }; },
      set: function (t, k, v) { t[k] = v; return true; }
    });
  }
  var ca = ctx2d(), cs = ctx2d(), drawn = L.paint(ca, L.MAP, "albedo"), splat = L.paint(cs, L.MAP, "splat");
  ["wheat", "barley", "hay", "plough", "pasture", "hedge", "apple", "wood", "river", "pond", "lane", "path"].forEach(function (k) {
    assert.ok(drawn[k] > 0, "the field map has " + k + ": " + JSON.stringify(drawn));
  });
  assert.deepStrictEqual(splat, drawn, "the splat lays out the same shapes as the colours");
  assert.ok(L.fields().length >= 30, "strip fields in furlongs: " + L.fields().length);
  assert.deepStrictEqual(JSON.stringify(L.fields()), JSON.stringify(L.fields()), "the layout is the same every time");

  // build: the field and the far ring, the ring under the field inside and just under it at the seam
  var g = L.build(THREE), land = g.getObjectByName("land"), far = g.getObjectByName("far");
  var lp = land.geometry.attributes.position;
  assert.strictEqual(lp.count, L.N * L.N, "the field has a vertex per height");
  assert.strictEqual(land.geometry.index.count, (L.N - 1) * (L.N - 1) * 6, "two triangles a cell");
  assert.strictEqual(lp.getY(1234), hs[1234], "the field's vertices are the heightfield");
  var fp = far.geometry.attributes.position, fn = L.FAR * 2 / L.FAR_CELL + 1, at = function (x, z) {
    var i = (x + L.FAR) / L.FAR_CELL, j = (z + L.FAR) / L.FAR_CELL;
    return fp.getY(j * fn + i);
  };
  near(at(0, 0), L.height(0, 0) - L.FAR_UNDER, 1e-4, "the ring under the field");
  near(at(200, -100), L.height(200, -100) - L.FAR_DROP, 1e-4, "the ring at the seam");
  near(at(-500, -500), L.height(-500, -500) - L.FAR_DROP, 1e-4, "the ring beyond");
  land.geometry.computeBoundingBox();
  var bb = land.geometry.boundingBox;
  near(bb.max.x - bb.min.x, L.SIZE, 1e-6, "the field is 400 m across");
  // the normals face up: the land is seen from above
  land.geometry.computeVertexNormals();
  assert.ok(land.geometry.attributes.normal.getY(5000) > 0.5, "the field's normals face up");

  // the detail texture tiles: across its seam each pattern steps no more than between any two neighbours inside it
  var dn = 128, px = L.detailPixels(dn);
  for (var ch = 0; ch < 3; ch++) {
    var seam = 0, inside = 0;
    for (var j = 0; j < dn; j++) {
      seam = Math.max(seam, Math.abs(px[(j * dn + dn - 1) * 4 + ch] - px[(j * dn) * 4 + ch]));
      for (var i = 0; i + 1 < dn; i++) inside = Math.max(inside, Math.abs(px[(j * dn + i + 1) * 4 + ch] - px[(j * dn + i) * 4 + ch]));
    }
    assert.ok(seam <= inside, "the detail's channel " + ch + " tiles: " + seam + " across the seam, " + inside + " inside");
  }

  console.log(JSON.stringify({
    hash: hash, eye: round(eye[1]), feet: round(feet), crest: cam.crest.map(round), summit: round(summit), valley: round(valley),
    ridges: [round(ridge1), round(ridge2)], pitch: round(cam.pitch), frames: frames, fields: L.fields().length, drawn: drawn
  }));
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
