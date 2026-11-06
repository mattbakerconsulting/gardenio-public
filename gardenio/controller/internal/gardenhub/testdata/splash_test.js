/* The splash's scene and its page logic (web/splash.js, plan gd-EJGA3zDGCk), run by TestSplashScene:  node splash_test.js <web dir>
   SPLASH.world with the vendored three.js itself: a perspective camera at 50 degrees where LAND.camera puts it, the sky dome,
   the land and its far ring, STONE's grey stone on its plinth on the brow right of centre with its gems, FLORA's meadow,
   hedges, trees, woods and rooks, FARM's farmstead and pond, LIGHT's sun in two cascades, the near one casting shadow from a
   tight frustum round the stone onto the land, with the sky's hemisphere light, the stars and the fireflies, and height fog
   in the palette's fog colour (testdata/splash_light_test.js tests the light itself). SPLASH.start with a stand-in
   WebGLRenderer (node has no WebGL): sRGB output, ACES tone mapping at the light's exposure, shadows on, the sky's horizon
   as the clear colour, the pixel ratio capped at 2, a frame per ResizeObserver report at the canvas's aspect; with motion, a
   frame every animation frame with the wind and the rooks moving, the light moving with the clock only when it runs free,
   at(t) relighting the scene, and the frame time logged to the console after a warm-up and then once a minute. SPLASH.motion: the wind blows when the clock runs free, under ?wind=1, and never under reduced motion.
   SPLASH.phone: a coarse pointer or a small screen, whose pond is a flat mirror colour; elsewhere boot's pond is a mirror.
   SPLASH.boot paints the field map on the page's canvases, reads the
   place, and falls back to the still, never a blank canvas, when there is no WebGL, when three.js does not load and when
   the renderer throws. SPLASH.clock's ?t= pin. The time (step 8): the shadow map drawn again only when the light has moved a
   quarter of a degree; the clock following the real one to the second on a timer when nothing moves, playing round the
   local day a day a minute, pinned by set(t, 0), telling its listeners; the camera breathing a few centimetres only when
   asked; ?t= written back to the second; the scrubber's minutes and keys; the readout's words. The quality (step 9):
   QUALITY's tiers, its guess from the device, ?q=, the warm-up's verdict and the watch's step down after 3 s over budget;
   the world at each tier (its blades, the mid ground's trees, the cascades and their maps, the pond) and a step down
   between two frames; the pixel ratio capped by the tier; pause and dispose; the light moving once a minute under reduced
   motion; and boot's lifecycle (hidden, off screen, pagehide, pageshow, the preference changing). ?perf=1's readout: the
   log's line over the last 10 s of frames, the time running and the step-downs, on the page only under ?perf=1. The view
   (plan gd-I5QHVfGLbD, VIEW): at the default the camera where it always was; look() narrowing the fov by the zoom, turning
   the aim by the yaw and pitch and drawing a frame with nothing moving; glide() easing there over the animation frames,
   which stop when it lands; the breath's aim wander over the zoom; boot opening at ?z=&look=; the canvas's gestures (a
   wheel, a drag, a double tap) moving the view and writing it to the address. Runs in UTC. Prints a JSON report. */
"use strict";
process.env.TZ = "UTC";
var path = require("path"), assert = require("assert");
var web = process.argv[2], S = require(path.join(web, "splash.js")), A = require(path.join(web, "almanac.js")), L = require(path.join(web, "splash", "land.js"));
var ST = require(path.join(web, "splash", "stone.js")), FL = require(path.join(web, "splash", "flora.js")), FM = require(path.join(web, "splash", "farm.js"));
var LT = require(path.join(web, "splash", "light.js")), Q = require(path.join(web, "splash", "quality.js")), V = require(path.join(web, "splash", "view.js"));
var NOON = Date.UTC(2026, 9, 26, 12), DAY_MS = 86400000;
function mm0(reduce) { return { matchMedia: function (q) { return { matches: reduce && /reduce/.test(q) }; } }; }

(async function () {
  var THREE = await import(path.join(web, "vendor", "three.module.min.js"));
  assert.ok(/^\d+$/.test(THREE.REVISION), "three.js loads as an ES module under node: " + THREE.REVISION);

  // the world at noon on 26 Oct 2026 at 45 N 0 E, with the real three.js
  var w = S.world(THREE, { t: NOON, lat: 45, lon: 0 }), kinds = {}, pal = A.skyPalette(NOON, 45, 0);
  w.scene.traverse(function (o) { if (o !== w.scene) kinds[o.type] = (kinds[o.type] || 0) + 1; });
  assert.deepStrictEqual(kinds, { Mesh: 5 + ST.GEMS.length + 8 + 20, Group: 5, Points: 4, DirectionalLight: 2, HemisphereLight: 1, Object3D: 1 },
    "the sky, the land and its ring, the stone, its plinth and its gems, what grows, the farm, the light: two cascades and the far one's target, the sky light, the stars and the fireflies");
  var farm = w.scene.getObjectByName("farm");
  assert.ok(farm && w.farm === farm && farm.getObjectByName("pond").userData.reflect === false, "the farm, its pond a flat colour unless asked to reflect");
  assert.ok(!farm.getObjectByName("lit windows").visible && !farm.getObjectByName("smoke").visible && farm.getObjectByName("swifts").visible, "at noon the windows are dark, the chimney cold and the swifts out");
  assert.ok(w.scene.getObjectByName("land").layers.isEnabled(FM.REFLECT) && farm.getObjectByName("farm stone").layers.isEnabled(FM.REFLECT) &&
    !w.scene.getObjectByName("grass").layers.isEnabled(FM.REFLECT) && !w.scene.getObjectByName("stone body").layers.isEnabled(FM.REFLECT) && w.scene.getObjectByName("sun").layers.isEnabled(FM.REFLECT),
    "the pond's mirror sees the land, the farm and the light, not the near meadow or the stone on the brow");
  var dusk = S.world(THREE, { t: Date.UTC(2026, 9, 26, 17, 30), lat: 45, lon: 0 }).farm;
  assert.ok(dusk.getObjectByName("lit windows").visible && dusk.getObjectByName("smoke").visible && dusk.getObjectByName("bats").visible && !dusk.getObjectByName("swifts").visible, "after dusk the windows are lit, the chimney smokes and the bats are out");
  var flora = w.scene.getObjectByName("flora");
  assert.ok(flora && w.flora === flora && flora.getObjectByName("grass").count === FL.GRASS && flora.getObjectByName("rooks").isInstancedMesh, "the flora: the grass's blades and the rest");
  assert.ok(w.camera.isPerspectiveCamera && w.camera.fov === 50, "a perspective camera at 50 degrees vertical");
  var view = L.camera(50);
  assert.deepStrictEqual(w.camera.position.toArray(), view.eye, "the camera where the land puts it, on the near slope");
  ["sky", "land", "far"].forEach(function (n) { assert.ok(w.scene.getObjectByName(n).isMesh, "the scene has its " + n); });
  var stone = w.scene.getObjectByName("stone"), body = stone.getObjectByName("stone body"), hsl = {}, grey = [0, 0, 0];
  var cols = body.geometry.attributes.color;
  for (var i = 0; i < cols.count; i++) { grey[0] += cols.getX(i); grey[1] += cols.getY(i); grey[2] += cols.getZ(i); }
  new THREE.Color(grey[0] / cols.count, grey[1] / cols.count, grey[2] / cols.count).getHSL(hsl, THREE.SRGBColorSpace);
  assert.ok(hsl.s < 0.15, "the stone is grey: " + JSON.stringify(hsl));
  stone.updateMatrixWorld();
  var box = new THREE.Box3().setFromObject(stone), ground = L.height(L.STONE.x, L.STONE.z);
  assert.ok(box.min.y < ground && box.min.y > ground - 0.3, "the stone's plinth stands on the brow, sunk a little: min y " + box.min.y + ", ground " + ground);
  // the stone is in front of the camera, in view and right of centre
  w.camera.aspect = 390 / 844; w.camera.updateProjectionMatrix(); w.camera.updateMatrixWorld();
  var c = box.getCenter(new THREE.Vector3()).project(w.camera);
  assert.ok(c.x > 0 && c.x < 0.9 && Math.abs(c.y) < 0.5 && c.z < 1, "the block in view right of centre at a phone's aspect: " + c.toArray());
  // the frame at each screen (step 10): the field of view widens on a phone upright, never cropping the stone: the crest a
  // third of the way up, the stone whole right of centre, the oak at the left, its trunk or its crown
  assert.strictEqual(S.fov(1280 / 800), 50); assert.strictEqual(S.fov(844 / 390), 50); assert.strictEqual(S.fov(390 / 844), 80); assert.strictEqual(S.fov(1), 60);
  var framed = {};
  [["phone", 390 / 844], ["phone on its side", 844 / 390], ["desktop", 1280 / 800]].forEach(function (a) {
    var f = S.fov(a[1]), fv = L.camera(f), fc = new THREE.PerspectiveCamera(f, a[1], 0.3, 3000);
    fc.position.fromArray(fv.eye); fc.lookAt(new THREE.Vector3().fromArray(fv.at)); fc.updateMatrixWorld();
    var cr = new THREE.Vector3().fromArray(fv.crest).project(fc), sb = new THREE.Box3().setFromObject(stone.getObjectByName("stone body")), xs = [], ys = [];
    [sb.min.x, sb.max.x].forEach(function (x) { [sb.min.y, sb.max.y].forEach(function (y) { [sb.min.z, sb.max.z].forEach(function (z) { var q = new THREE.Vector3(x, y, z).project(fc); xs.push(q.x); ys.push(q.y); }); }); });
    var g = L.height(FL.OAK.x, FL.OAK.z), oak = [new THREE.Vector3(FL.OAK.x, g + 1, FL.OAK.z).project(fc), new THREE.Vector3(FL.OAK.x + 5, g + 8, FL.OAK.z).project(fc)];
    assert.ok(Math.abs(cr.y + 1 / 3) < 0.01, a[0] + ": the crest a third of the way up: " + cr.y);
    assert.ok(Math.min.apply(null, xs) > -0.1 && Math.max.apply(null, xs) < 0.95 && Math.min.apply(null, ys) > -0.95 && Math.max.apply(null, ys) < 0.95 && Math.min.apply(null, xs) + Math.max.apply(null, xs) > 0.2,
      a[0] + ": the stone whole, right of centre: x " + Math.min.apply(null, xs) + ".." + Math.max.apply(null, xs) + ", y " + Math.min.apply(null, ys) + ".." + Math.max.apply(null, ys));
    assert.ok(oak.some(function (q) { return q.x > -1 && q.x < 0 && Math.abs(q.y) < 1; }), a[0] + ": the oak at the left: " + oak.map(function (q) { return q.x.toFixed(2) + "," + q.y.toFixed(2); }).join(" "));
    framed[a[0]] = { fov: f, stone: [+Math.min.apply(null, xs).toFixed(2), +Math.max.apply(null, xs).toFixed(2)] };
  });
  // what the canvas says: the hour, the sun and the moon in the almanac's words, and the stone
  assert.strictEqual(S.label(A.skyPalette(Date.UTC(2026, 9, 26, 17, 15), 45, 0)), "Dusk over the farm: the sun just below the horizon in the west; a full moon, 100 percent, rising in the east; the gardenio stone on its hill");
  assert.strictEqual(S.label(w.palette), "Midday over the farm: the sun high in the south; the gardenio stone on its hill");
  // the light at noon: the sun, from the almanac's direction, colour and strength; the sky and the fog from its palette
  var sun = w.scene.getObjectByName("sun"), dir = sun.position.clone().sub(stone.position).normalize();
  assert.ok(sun.isDirectionalLight && sun.target === stone && w.light === w.scene.getObjectByName("light"), "the sun shines on the stone");
  var far = w.scene.getObjectByName("sun far");
  assert.ok(far.isDirectionalLight && far.castShadow && far.shadow.mapSize.x === 1024 && far.intensity === sun.intensity && far.color.equals(sun.color), "the far cascade, the same light");
  var sc = sun.shadow.camera;
  assert.ok(sun.castShadow && sun.shadow.mapSize.x >= 2048 && sc.right - sc.left <= 10 && sc.near < 300 && sc.far > 300, "the sun casts shadow from a tight frustum round the stone");
  assert.ok(w.scene.getObjectByName("land").receiveShadow && body.castShadow && body.receiveShadow, "the stone throws shadow on itself and on the land");
  assert.ok(dir.distanceTo(new THREE.Vector3().fromArray(pal.direction)) < 1e-6 && dir.z > 0.5, "from the almanac's direction, the south behind the camera: " + dir.toArray());
  assert.ok(Math.abs(sun.intensity - pal.strength) < 1e-9 && sun.color.getHex() === new THREE.Color(pal.light).getHex(), "the almanac's light");
  var hemi = w.scene.getObjectByName("sky light");
  assert.ok(hemi.isHemisphereLight && hemi.color.getHex() === new THREE.Color(pal.zenith).getHex() && hemi.groundColor.getHex() === new THREE.Color(pal.ground).getHex(), "the sky light from the palette");
  assert.ok(w.scene.fog.isFogExp2 && w.scene.fog.color.getHex() === new THREE.Color(pal.fog).getHex() && w.scene.fog.density === L.FOG_DENSITY, "height fog in the palette's fog colour");
  assert.ok(!w.scene.getObjectByName("fireflies").layers.isEnabled(FM.REFLECT) && w.scene.getObjectByName("stars").layers.isEnabled(FM.REFLECT), "the pond shows the stars, not the fireflies over the meadow");
  var land = w.scene.getObjectByName("land"), shader = { uniforms: {}, fragmentShader: "#include <fog_pars_fragment>\n#include <fog_fragment>" };
  land.material.onBeforeCompile(shader);
  assert.ok(/uFogFall/.test(shader.fragmentShader) && /exp\(-uFogFall \* cameraPosition\.y\)/.test(shader.fragmentShader) && shader.uniforms.uFogFall.value === L.FOG_FALL, "the land's fog thins with height");
  var sky = w.scene.getObjectByName("sky");
  assert.ok(sky.material.fog === false && sky.material.side === THREE.BackSide, "the sky dome is seen from inside, unfogged");

  // start, with a stand-in renderer and a stand-in window
  var made = [];
  function Renderer(opts) { this.opts = opts; this.calls = []; this.shadowMap = {}; made.push(this); }
  Renderer.prototype.setClearColor = function (col, a) { this.clear = [col.getHexString(THREE.SRGBColorSpace), a]; };
  Renderer.prototype.setPixelRatio = function (r) { this.ratio = r; };
  Renderer.prototype.setSize = function (wd, ht, css) { this.size = [wd, ht, css]; };
  // three draws the shadow map when it is asked to and clears the ask (autoUpdate off); the stand-in records the ask
  Renderer.prototype.render = function (sc, cam) { this.calls.push([sc, cam.aspect, !!this.shadowMap.needsUpdate]); this.shadowMap.needsUpdate = false; };
  var T = Object.assign({}, THREE, { WebGLRenderer: Renderer }), observed = [];
  var win = { devicePixelRatio: 3, ResizeObserver: function (fn) { this.observe = function (el) { observed.push([el, fn]); }; } };
  var cv = { clientWidth: 390, clientHeight: 844, dataset: {} };
  var v = S.start(T, cv, win), r = made[0];
  assert.strictEqual(r.opts.canvas, cv, "the renderer draws on the page's canvas");
  assert.strictEqual(r.outputColorSpace, THREE.SRGBColorSpace, "sRGB output");
  assert.strictEqual(r.toneMapping, THREE.ACESFilmicToneMapping, "ACES tone mapping");
  assert.strictEqual(r.toneMappingExposure, undefined, "the exposure is set as a frame is drawn");
  assert.ok(r.shadowMap.enabled && r.shadowMap.type === THREE.PCFShadowMap, "shadows on");
  assert.strictEqual(observed.length, 1, "the canvas is observed for its size");
  assert.strictEqual(r.calls.length, 0, "no frame before the observer reports");
  observed[0][1]();
  assert.strictEqual(r.ratio, 2, "a 3x screen draws at 2x");
  assert.deepStrictEqual(r.size, [390, 844, false], "the drawing buffer follows the canvas, its CSS size left to the page");
  assert.ok(r.calls.length === 1 && Math.abs(r.calls[0][1] - 390 / 844) < 1e-9 && cv.dataset.frames === "1", "one frame at the canvas's aspect");
  var aim = new THREE.Vector3().fromArray(L.camera(80).at).sub(v.camera.position).normalize();
  assert.ok(v.camera.fov === 80 && v.view.at.join() === L.camera(80).at.join() && v.camera.getWorldDirection(new THREE.Vector3()).dot(aim) > 1 - 1e-9,
    "a phone upright frames the scene at 80 degrees, pitched for it: the crest a third of the way up");
  assert.strictEqual(r.toneMappingExposure, LT.exposure(v.palette), "drawn at the light's exposure");
  assert.deepStrictEqual(r.clear, [new THREE.Color(v.palette.horizon).getHexString(THREE.SRGBColorSpace), 1], "the sky's horizon as the clear colour");
  var NIGHT = Date.UTC(2026, 9, 26, 23);
  v.at(NIGHT);
  assert.ok(v.t === NIGHT && v.palette.source === "moon" && v.scene.getObjectByName("moon") && r.toneMappingExposure > 2 && r.calls.length === 2 && cv.dataset.frames === "2",
    "at(t) relights the scene and draws it: the moon, the night's exposure " + r.toneMappingExposure);
  assert.ok(v.farm.getObjectByName("lit windows").visible && v.scene.getObjectByName("stars").visible && !v.scene.getObjectByName("fireflies").visible, "the farm's hour and the stars follow, no fireflies in the dead of night");
  v.at(NOON);
  cv.clientWidth = 1280; cv.clientHeight = 800; win.devicePixelRatio = 1;
  observed[0][1]();
  assert.ok(r.ratio === 1 && Math.abs(v.camera.aspect - 1.6) < 1e-9 && cv.dataset.frames === "4", "a resize draws again at the new aspect");
  assert.ok(v.camera.fov === S.FOV && v.view.at.join() === L.camera(S.FOV).at.join(), "and a desktop at 50 degrees");
  assert.strictEqual(S.pixelRatio(undefined), 1);
  assert.ok(v.perf && v.perf.draw.length === 4 && cv.dataset.frames === "4", "the draws are timed, and nothing draws by itself without motion");
  assert.ok(r.shadowMap.autoUpdate === false, "the shadow map is drawn when asked, not every frame");
  assert.deepStrictEqual(r.calls.map(function (c) { return c[2]; }), [true, true, true, false], "the shadow map drawn on the first frame and when the light moves (noon to night and back), not on a resize");
  v.at(NOON + 30000);
  v.at(NOON + 50000);
  assert.deepStrictEqual(r.calls.slice(4).map(function (c) { return c[2]; }), [false, false], "the sun 50 s on, under a quarter of a degree: the same shadow map");
  v.at(NOON + 90000);
  assert.ok(r.calls[6][2] && v.t === NOON + 90000, "90 s on, past a quarter of a degree: the shadow map drawn again");

  // with motion: a frame every animation frame, the wind and the rooks moving, the frame time logged after the warm-up
  var queued = [], logs = [], now = 0, mcv = { clientWidth: 390, clientHeight: 844, dataset: {} };
  var mwin = Object.assign({}, win, { performance: { now: function () { return now; } }, console: { info: function (m) { logs.push(m); } },
    requestAnimationFrame: function (fn) { queued.push(fn); } });
  var mv = S.start(T, mcv, mwin, { motion: true, t: NOON });
  var rook = new THREE.Matrix4(), rook2 = new THREE.Matrix4(), u = {};
  mv.flora.getObjectByName("rooks").getMatrixAt(0, rook);
  for (var f = 0; f < 400; f++) { now = f * 16.7; queued.shift()(now); }
  mv.flora.getObjectByName("rooks").getMatrixAt(0, rook2);
  mv.flora.getObjectByName("grass").material.onBeforeCompile({ uniforms: u, vertexShader: "", fragmentShader: "" });
  var fu = {};
  mv.farm.getObjectByName("smoke").material.onBeforeCompile({ uniforms: fu, vertexShader: "", fragmentShader: "" });
  assert.ok(Math.abs(fu.uTime.value - 399 * 0.0167) < 1e-6, "the farm's smoke, beasts and birds move with the frames: t " + fu.uTime.value);
  assert.ok(mcv.dataset.frames === "400" && Math.abs(u.uTime.value - 399 * 0.0167) < 1e-6 && !rook.equals(rook2), "every animation frame draws the wind and the rooks on: " + mcv.dataset.frames + ", t " + u.uTime.value);
  assert.ok(mv.t === NOON && Math.abs(mv.scene.getObjectByName("fireflies").material.uniforms.uTime.value - 399 * 0.0167) < 1e-6, "a pinned clock keeps its light while the wind blows; the fireflies' clock runs");
  var rq = [], rnow = 0, rv = S.start(T, { clientWidth: 390, clientHeight: 844, dataset: {} }, Object.assign({}, mwin, { performance: { now: function () { return rnow; } }, console: { info: function () {} }, requestAnimationFrame: function (fn) { rq.push(fn); } }), { motion: true, run: true, t: NOON });
  var sun0 = rv.scene.getObjectByName("sun").position.clone();
  rq.shift()(0); rnow = 600000; rq.shift()(rnow);
  assert.ok(rv.t === NOON + 600000 && rv.scene.getObjectByName("sun").position.distanceTo(sun0) > 1, "a clock running free moves the light with it: 10 minutes on, the sun has moved");
  rv.at(NOON); rnow += 60000; rq.shift()(rnow);
  assert.strictEqual(rv.t, NOON + 60000, "at(t) sets the running clock, which runs on from there");
  assert.ok(logs.length === 1 && /^splash: frame 16\.7 ms \(p95 16\.7\), draw [0-9.]+ ms \(p95 [0-9.]+\) over \d+ frames, \d+ blades$/.test(logs[0]), "the frame time logged after the warm-up, no tier without one: " + logs);
  assert.ok(mv.perf.last.n > 250 && mv.perf.last.frame === 16.7, "the frames counted since the start: " + JSON.stringify(mv.perf.last));
  for (; f < 4000; f++) { now = f * 16.7; queued.shift()(now); }
  assert.strictEqual(logs.length, 2, "then once a minute");
  assert.deepStrictEqual(S.summary({ frame: [10, 20, 30, 40], draw: [] }), { n: 4, frame: 25, frame95: 40, draw: 0, draw95: 0 }, "the mean and the 95th percentile");

  assert.deepStrictEqual(mv.camera.position.toArray(), view.eye, "the camera still without breathe");

  // the world at each tier: its blades, the mid ground's trees, the cascades and their maps, the pond
  function tierOf(wq) {
    var o = wq.light.userData, fl = wq.flora;
    return { blades: fl.getObjectByName("grass").count, flowers: fl.getObjectByName("flowers").count, leaves: fl.getObjectByName("leaves").count, woods: fl.getObjectByName("impostors").count,
      limbs: fl.getObjectByName("limbs").geometry.drawRange.count, near: o.near.visible ? o.near.shadow.mapSize.x : 0, far: o.far.shadow.mapSize.x, pond: wq.farm.getObjectByName("pond").userData.mode };
  }
  var hi = tierOf(S.world(THREE, { t: NOON, lat: 45, lon: 0, tier: "high" })), lo = S.world(THREE, { t: NOON, lat: 45, lon: 0, tier: "low" }), low = tierOf(lo);
  var md = tierOf(S.world(THREE, { t: NOON, lat: 45, lon: 0, tier: "mid" })), cnt = lo.flora.userData.counts;
  assert.ok(hi.blades === 60000 && hi.flowers === FL.FLOWERS && hi.near === 2048 && hi.far === 1024 && hi.pond === "mirror" && hi.woods === cnt.woods && hi.limbs === cnt.limbs, "high: every blade, two cascades, the mirror: " + JSON.stringify(hi));
  assert.ok(md.blades === 25000 && md.near === 0 && md.far === 2048 && md.pond === "tint" && md.leaves === hi.leaves, "mid: 25,000 blades, one 2048 px cascade, the mirror colour: " + JSON.stringify(md));
  assert.ok(low.blades === 8000 && low.flowers === Math.round(FL.FLOWERS * 8000 / 60000) && low.near === 0 && low.far === 1024 && low.pond === "water", "low: 8,000 blades, one 1024 px cascade, no reflection: " + JSON.stringify(low));
  assert.ok(low.leaves === cnt.oakLeaves && low.leaves < hi.leaves / 4 && low.limbs === cnt.oakLimbs && low.woods === cnt.woods + 63 && hi.woods === cnt.woods, "low: only the oak grown, the 31 poplars and 32 apple trees impostors beside the woods: " + JSON.stringify(low));
  var mi = lo.flora.getObjectByName("impostors"), mm4 = new THREE.Matrix4(), mp = new THREE.Vector3(), mq = new THREE.Quaternion(), ms = new THREE.Vector3(), tr = FL.trees();
  mi.getMatrixAt(cnt.woods, mm4); mm4.decompose(mp, mq, ms);
  assert.ok(Math.abs(mp.x - tr[1].x) < 1e-3 && Math.abs(mp.z - tr[1].z) < 1e-3 && ms.y > 15 && ms.x < ms.y / 2, "the first poplar's impostor where it grows, tall and narrow: " + ms.toArray());
  mi.getMatrixAt(cnt.midWoods - 1, mm4); mm4.decompose(mp, mq, ms);
  assert.ok(Math.abs(mp.x - tr[tr.length - 1].x) < 1e-3 && ms.y > 2 && ms.y < 8 && ms.x > 2, "the last apple tree's impostor, round and small: " + ms.toArray());
  assert.ok(lo.scene.getObjectByName("land").layers.isEnabled(FM.REFLECT) && lo.farm.getObjectByName("pond").material.isMeshLambertMaterial, "low's pond lit water, no mirror pass");
  // a tinted pond follows the sky as the light moves
  var tv2 = S.start(T, { clientWidth: 10, clientHeight: 10, dataset: {} }, win, { t: NOON, lat: 45, lon: 0, tier: "mid" }), tpond = tv2.farm.getObjectByName("pond"), noonL = tpond.material.color.getHSL({}).l;
  tv2.at(Date.UTC(2026, 9, 26, 23));
  assert.ok(tpond.material.color.getHSL({}).l < noonL - 0.1, "a tinted pond follows the sky, darker at night: " + noonL.toFixed(2) + " to " + tpond.material.color.getHSL({}).l.toFixed(2));

  // start at a tier: the pixel ratio capped by it, the warm-up and the watch stepping down while the wind blows
  var aq = [], anow = 0, alogs = [], acv = { clientWidth: 390, clientHeight: 844, dataset: {} };
  var awin = Object.assign({}, win, { devicePixelRatio: 3, performance: { now: function () { return anow; } }, console: { info: function (m) { alogs.push(m); } }, requestAnimationFrame: function (fn) { aq.push(fn); } });
  var av = S.start(T, acv, awin, { motion: true, t: NOON, tier: "high", adapt: true, budget: 16, roll: S.ROLL }), ar = made[made.length - 1];
  aq.shift()(0);
  assert.ok(ar.ratio === 2 && acv.dataset.tier === "high" && av.judging === "warm-up", "high draws a 3x screen at 2x, and warms up");
  for (var af = 1; af <= 25; af++) { anow += 30; aq.shift()(anow); }
  assert.ok(av.tier.name === "mid" && acv.dataset.tier === "mid" && av.judging === "watch" && alogs[0] === "splash: mid tier (warm-up)", "30 ms frames at high: the warm-up steps to mid: " + alogs);
  assert.ok(ar.ratio === 1.5 && av.flora.getObjectByName("grass").count === 25000 && !av.light.userData.near.visible && av.light.userData.far.shadow.mapSize.x === 2048 && av.farm.getObjectByName("pond").userData.mode === "tint",
    "mid drawn between two frames: 1.5 px, 25,000 blades, one 2048 px cascade, the tinted pond");
  assert.strictEqual(ar.calls.filter(function (c) { return c[2]; }).length, 2, "the shadow map drawn on the first frame and again at the new tier, the light pinned");
  for (af = 0; af < 160 && av.tier.name === "mid"; af++) { anow += 30; aq.shift()(anow); }
  assert.ok(av.tier.name === "low" && alogs[1] === "splash: low tier (3 s over budget)" && af > 90 && af < 140, "30 ms frames at mid for 3 s: low, after " + af + " frames");
  anow += 30; aq.shift()(anow);
  assert.ok(ar.ratio === 1 && av.flora.getObjectByName("grass").count === 8000 && av.light.userData.far.shadow.mapSize.x === 1024 && av.farm.getObjectByName("pond").userData.mode === "water" && av.flora.userData.impostors, "low: 1 px, 8,000 blades, 1024 px, the water, the impostors");
  for (af = 0; af < 300; af++) { anow += 60; aq.shift()(anow); }
  assert.ok(av.tier.name === "low" && av.judging === null && alogs.filter(function (m) { return / tier \(/.test(m); }).length === 2, "nothing under low: it stops judging: " + av.judging + " " + alogs);
  assert.ok(av.log().tier === "low" && /, 8000 blades, low tier, budget 16 ms$/.test(alogs[alogs.length - 1]), "the log names the tier and its budget: " + alogs[alogs.length - 1]);
  // ?perf=1's readout: the log's line over the frames of the last 10 s, the time since the first frame and the step-downs
  var roll = av.perf.roll, said = S.gauge(av.perf, 8000, anow);
  assert.ok(roll.length === 167 && roll[0][0] > anow - S.ROLL && roll.every(function (r) { return r[1] === 60; }), "the roll keeps the last 10 s of frames: " + roll.length + " from " + (anow - roll[0][0]));
  assert.deepStrictEqual(av.perf.steps.map(function (k) { return k.from + ">" + k.to; }), ["high>mid", "mid>low"], "the step-downs kept");
  assert.ok(/^splash: frame 60 ms \(p95 60\), draw [0-9.]+ ms \(p95 [0-9.]+\) over 167 frames, 8000 blades, low tier, budget 16 ms\nrunning 2\d s, the last 10 s; stepped high -> mid at 0 s, mid -> low at [34] s$/.test(said),
    "the readout: the log's words over the last 10 s, then the time running and the step-downs: " + JSON.stringify(said));
  assert.deepStrictEqual([
    S.gauge({ roll: [], steps: [], tier: "mid", budget: 16, held: true, began: 0 }, 25000, 83500),
    S.gauge({ roll: [[1000, 20, 5], [1020, 40, 7]], steps: [], tier: "low", budget: 33, held: false, began: 500 }, 8000, 1020),
    S.gauge({ roll: null, steps: [], tier: null, budget: null, held: false, began: null }, 60000, 5)
  ], [
    "splash: frame 0 ms (p95 0), draw 0 ms (p95 0) over 0 frames, 25000 blades, mid tier, budget 16 ms\nrunning 1 m 23 s, the last 10 s; held at mid by ?q=",
    "splash: frame 30 ms (p95 40), draw 6 ms (p95 7) over 2 frames, 8000 blades, low tier, budget 33 ms\nrunning 0 s, the last 10 s; no step-downs",
    "splash: frame 0 ms (p95 0), draw 0 ms (p95 0) over 0 frames, 60000 blades\nrunning 0 s, the last 10 s"
  ], "the readout held by ?q=, with no step-downs, and before a frame without a tier");
  assert.deepStrictEqual(["?perf=1", "?t=x&perf=1&q=low", "?perf=10", "?perf=0", "", "?xperf=1"].map(S.perfOn), [true, true, false, false, false, false], "?perf=1 asks for the readout");
  assert.strictEqual(mv.perf.roll, null, "without a roll no frames are kept for it");
  // pause: no frames while paused, and the gap not counted when it draws on
  var pf = av.perf.frames;
  av.pause(true); anow += 16; aq.shift()(anow);
  assert.ok(aq.length === 0 && av.perf.frames === pf && av.paused, "paused, the animation frames stop");
  var pdraws = ar.calls.length;
  anow += 600000; av.pause(false);
  assert.ok(aq.length === 1 && !av.paused, "unpaused, they start again");
  aq.shift()(anow); anow += 16.7; aq.shift()(anow);
  assert.ok(av.perf.frame.length === 1 && Math.abs(av.perf.frame[0] - 16.7) < 1e-6 && ar.calls.length === pdraws + 2, "the paused ten minutes are not a frame: " + JSON.stringify(av.perf.frame) + " " + (ar.calls.length - pdraws));
  // dispose: the GPU let go, nothing drawn after
  ar.dispose = function () { ar.disposed = (ar.disposed || 0) + 1; };
  ar.forceContextLoss = function () { ar.lost = true; };
  var disposed = 0;
  av.scene.traverse(function (o) { if (o.geometry) o.geometry.addEventListener("dispose", function () { disposed++; }); });
  av.dispose(); av.dispose();
  var after = ar.calls.length;
  av.draw(); av.at(NOON); av.pause(false); (aq.shift() || function () {})(anow + 16);
  assert.ok(ar.disposed === 1 && ar.lost && disposed > 40 && acv.dataset.gone === "1" && ar.calls.length === after, "disposed: every geometry, the renderer and its context, and nothing drawn after: " + disposed + " geometries");

  // the time: following the clock with nothing moving, the light moves once a second on a timer
  var tq = [], tnow = 0, timers = [], heard = [], tcv = { clientWidth: 390, clientHeight: 844, dataset: {} };
  var twin = Object.assign({}, win, { performance: { now: function () { return tnow; } }, console: { info: function () {} }, requestAnimationFrame: function (fn) { tq.push(fn); }, setInterval: function (fn, ms) { timers.push([fn, ms]); } });
  var tv = S.start(T, tcv, twin, { run: true, t: NOON });
  tv.listen(function (t) { heard.push(t); });
  assert.ok(tq.length === 0 && timers.length === 1 && timers[0][1] === 1000 && tv.rate === 1, "following the clock, still: no animation frames, a one-second timer");
  timers[0][0](); tnow = 1500; timers[0][0](); var drawn = tcv.dataset.frames; tnow = 1900; timers[0][0]();
  assert.ok(tv.t === NOON + 1000 && drawn === "1" && tcv.dataset.frames === "1" && heard.join() === String(NOON + 1000), "the light moves to the whole second and draws, and only when it moves: " + tv.t + ", " + tcv.dataset.frames);
  // playing: a day a minute on the animation frames, round the local day
  tv.set(NOON, S.PLAY);
  assert.ok(tv.rate === 1440 && tq.length === 1, "Play runs on the animation frames");
  tnow += 1000; tq.shift()(tnow);
  assert.strictEqual(tv.t, NOON + 24 * 60000, "a second played is 24 minutes of the day");
  tnow += 29000; tq.shift()(tnow);
  assert.strictEqual(tv.t, Date.UTC(2026, 9, 26), "half a minute played from noon goes round to the day's midnight, not the next day's");
  tnow += 15000; tq.shift()(tnow);
  assert.strictEqual(tv.t, Date.UTC(2026, 9, 26, 6), "and on through the same day");
  tnow += 100;
  assert.ok(tv.now() === Date.UTC(2026, 9, 26, 6) + 144000 && tv.t === Date.UTC(2026, 9, 26, 6), "now() is where the day has got to before a frame draws it");
  // pinned: the frames stop, and set() draws once on the next animation frame
  tv.set(NOON, 0);
  var f0 = +tcv.dataset.frames;
  tnow += 1000; tq.shift()(tnow);
  assert.ok(tq.length === 0 && tv.t === NOON && +tcv.dataset.frames === f0 + 1, "pinned with nothing moving, the animation frames stop");
  tv.set(NOON + 3600000, 0); tv.set(NOON + 7200000, 0);
  assert.ok(tq.length === 1 && tv.t === NOON + 7200000 && heard[heard.length - 1] === NOON + 7200000, "set() relights at once and asks for one frame however often it is called");
  tq.shift()(tnow);
  assert.ok(+tcv.dataset.frames === f0 + 2 && tq.length === 0, "the frame drawn, nothing more");
  timers[0][0]();
  assert.strictEqual(tv.t, NOON + 7200000, "the timer leaves a pinned clock alone");
  // animate: the wind and the breath, the camera drifting a few centimetres from where the land puts it
  tv.animate(true, true);
  for (var k = 0; k < 200; k++) { tnow += 100; tq.shift()(tnow); }
  var drift = tv.camera.position.distanceTo(new THREE.Vector3().fromArray(view.eye));
  assert.ok(drift > 0.001 && drift < 0.06 && tq.length === 1, "breathing, the camera drifts a few centimetres: " + drift.toFixed(4) + " m");
  tv.animate(false, false); tq.shift()(tnow);
  assert.ok(tq.length === 0 && tv.camera.position.toArray().join() === view.eye.join(), "and stops where it began");
  // under reduced motion the light follows the clock by the second but the scene is drawn again once a minute; calm() sets it
  // back by the preference
  var rmt = [], rmnow = 0, rmcv = { clientWidth: 390, clientHeight: 844, dataset: {} }, rmq = [];
  var rmw = Object.assign({}, win, { performance: { now: function () { return rmnow; } }, console: { info: function () {} }, requestAnimationFrame: function (fn) { rmq.push(fn); }, setInterval: function (fn) { rmt.push(fn); },
    location: { search: "" }, matchMedia: function (q) { return { matches: /reduce/.test(q) }; } });
  var rmv = S.start(T, rmcv, rmw, { run: true, t: NOON, every: S.STILL_EVERY });
  for (var rs = 1; rs <= 125; rs++) { rmnow = rs * 1000; rmt[0](); }
  assert.ok(rmv.t === NOON + 124000 && rmcv.dataset.frames === "3" && rmq.length === 0, "reduced motion: 2 min on, the light by the second, the scene drawn at 2, 62 and 122 s: " + (rmv.t - NOON) + ", " + rmcv.dataset.frames);
  rmw.matchMedia = function () { return { matches: false }; };
  S.calm(rmw, rmv);
  assert.ok(rmq.length === 1, "the preference lifted: the wind blows again");
  rmnow += 1000; rmq.shift()(rmnow);
  assert.ok(rmv.t === NOON + 125000 && rmcv.dataset.frames === "4", "and the frames draw the light by the second: " + (rmv.t - NOON));
  var most = { eye: 0, aim: 0 };
  for (var bs = 0; bs < 600; bs += 0.25) {
    var b = S.breath(bs, 20);
    most.eye = Math.max(most.eye, Math.hypot.apply(null, b.eye)); most.aim = Math.max(most.aim, Math.atan(Math.hypot.apply(null, b.aim) / 20) * 180 / Math.PI);
  }
  assert.ok(most.eye > 0.02 && most.eye <= 0.05 && most.aim > 0.05 && most.aim < 0.3, "the breath: the eye within a few cm, the aim within a fraction of a degree: " + JSON.stringify(most));
  assert.deepStrictEqual(S.breath(0, 20).eye.map(Math.abs).map(function (x) { return x < 0.03; }), [true, true, true]);

  // the view (plan gd-I5QHVfGLbD): at the default the camera stands and looks exactly where it did before the view, at
  // each screen's designed fov, so the goldens do not move
  var RAD = Math.PI / 180, looked = {};
  function designed(base, aspect) {
    var dv = L.camera(base), dc = new THREE.PerspectiveCamera(base, aspect, 0.3, 3000);
    dc.position.fromArray(dv.eye); dc.lookAt(new THREE.Vector3().fromArray(dv.at));
    return dc;
  }
  function bearing(cam) { var d = cam.getWorldDirection(new THREE.Vector3()); return [Math.atan2(d.x, -d.z) / RAD, Math.asin(d.y) / RAD]; }
  function viewer(wd, ht, opts) {
    var q = [], cvs = { clientWidth: wd, clientHeight: ht, dataset: {} };
    var vw = S.start(T, cvs, Object.assign({}, win, { performance: { now: function () { return 0; } }, console: { info: function () {} }, requestAnimationFrame: function (fn) { q.push(fn); } }), Object.assign({ t: NOON }, opts));
    observed[observed.length - 1][1]();
    return { v: vw, q: q, cv: cvs };
  }
  [[1280, 800], [390, 844]].forEach(function (sz) {
    var lv = viewer(sz[0], sz[1]).v, dc = designed(S.fov(sz[0] / sz[1]), sz[0] / sz[1]);
    assert.ok(lv.camera.fov === dc.fov && lv.camera.position.toArray().join() === dc.position.toArray().join() && Math.abs(Math.abs(lv.camera.quaternion.dot(dc.quaternion)) - 1) < 1e-9 &&
      lv.camera.quaternion.toArray().every(function (x, i) { return Math.abs(x - dc.quaternion.toArray()[i]) < 1e-9; }) && JSON.stringify(lv.state()) === JSON.stringify(V.view()),
      sz.join("x") + ": at the default view the camera is the designed frame's: fov " + lv.camera.fov + ", " + lv.camera.quaternion.toArray());
  });
  // look() with nothing moving (a pinned clock, no wind): the fov the designed one over the zoom, the aim turned about the up
  // axis by the yaw and up by the pitch, drawn on the next animation frame however often it is asked
  var lk = viewer(1280, 800), lv0 = lk.v, b0 = bearing(designed(50, 1280 / 800)), lf = +lk.cv.dataset.frames;
  assert.ok(lk.q.length === 0 && lf === 1, "pinned and still: no animation frames before a look");
  assert.deepStrictEqual(lv0.look({ zoom: 4, yaw: 10, pitch: 5 }), { zoom: 4, yaw: 10, pitch: 5 }, "look returns the view it took");
  lv0.look({ zoom: 4, yaw: 10, pitch: 5 });
  assert.ok(lk.q.length === 1 && +lk.cv.dataset.frames === lf, "a look asks for one frame, however often it is called");
  lk.q.shift()(16);
  var b4 = bearing(lv0.camera);
  assert.ok(+lk.cv.dataset.frames === lf + 1 && lk.q.length === 0, "and the frame drew, nothing more");
  assert.ok(Math.abs(Math.tan(lv0.camera.fov * RAD / 2) - Math.tan(25 * RAD) / 4) < 1e-12 && lv0.camera.fov === V.fov(50, 4), "zoom 4: the fov's tangent a quarter of the designed one's: " + lv0.camera.fov);
  assert.ok(Math.abs(b4[0] - b0[0] - 10) < 1e-9 && Math.abs(b4[1] - b0[1] - 5) < 1e-9 && lv0.camera.position.toArray().join() === L.camera(50).eye.join(),
    "the aim turned 10 degrees right about the up axis and 5 up, the eye where it stood: " + b0.map(function (x) { return x.toFixed(3); }) + " -> " + b4.map(function (x) { return x.toFixed(3); }));
  assert.deepStrictEqual(lv0.state(), { zoom: 4, yaw: 10, pitch: 5 }, "state() reads it back");
  looked.zoom4 = { fov: +lv0.camera.fov.toFixed(3), bearing: b4.map(function (x) { return +x.toFixed(3); }) };
  assert.deepStrictEqual(lv0.look({ zoom: 20, yaw: -100, pitch: "up" }), { zoom: V.ZOOM_MAX, yaw: -V.YAW, pitch: 0 }, "a look held to the bounds");
  lv0.look(V.view()); lk.q.shift()(32);
  var back = designed(50, 1280 / 800);
  assert.ok(lv0.camera.fov === 50 && lv0.camera.quaternion.toArray().every(function (x, i) { return Math.abs(x - back.quaternion.toArray()[i]) < 1e-9; }), "and back to the designed frame");
  // glide (a gesture's change, not under reduced motion): the view eases to its goal over the animation frames, which stop
  // again once it lands with nothing else moving; goal() is where it is going all along
  var gk = viewer(1280, 800), gv = gk.v;
  assert.deepStrictEqual(gv.glide({ zoom: 4, yaw: 10 }), { zoom: 4, yaw: 10, pitch: 0 }, "glide returns where the view is going");
  assert.ok(gk.q.length === 1 && gv.state().zoom === 1 && gv.goal().zoom === 4, "and stands until the frame");
  gk.q.shift()(75);
  var mid = gv.state();
  assert.ok(mid.zoom > 1 && mid.zoom < 4 && mid.yaw > 0 && mid.yaw < 10 && gk.q.length === 1, "half way through the ease, part way there, the frames running on: " + JSON.stringify(mid));
  gk.q.shift()(150);
  assert.ok(JSON.stringify(gv.state()) === JSON.stringify({ zoom: 4, yaw: 10, pitch: 0 }) && gk.q.length === 0 && gv.camera.fov === V.fov(50, 4), "landed at 150 ms, and the frames stop: " + JSON.stringify(gv.state()));
  // breathing, the aim's wander is divided by the zoom (a zoomed-in frame does not shake); the eye drifts as it did
  function wander(st) {
    var bw = viewer(1280, 800, { motion: true, breathe: true, view: st });
    for (var k = 1; k <= 120; k++) bw.q.shift()(k * 250);
    var aim = V.aim(L.camera(50), V.clamp(st)), want = new THREE.Vector3().fromArray(aim.at).sub(new THREE.Vector3().fromArray(aim.eye)).normalize();
    return { aim: Math.acos(Math.min(1, bw.v.camera.getWorldDirection(new THREE.Vector3()).dot(want))), eye: bw.v.camera.position.distanceTo(new THREE.Vector3().fromArray(aim.eye)) };
  }
  var w1 = wander(V.view()), w8 = wander({ zoom: 8, yaw: 0, pitch: 0 });
  assert.ok(w1.aim > 0 && w8.aim > 0 && w8.aim < w1.aim / 4 && Math.abs(w8.eye - w1.eye) < 1e-9, "zoomed 8x the aim wanders an eighth as far, the eye the same: " + JSON.stringify([w1, w8]));
  looked.wander = [w1.aim, w8.aim].map(function (x) { return +(x / RAD).toFixed(5); });

  // the scrubber, the link and the readout
  assert.strictEqual(S.rfc3339(Date.UTC(2026, 9, 26, 12, 10, 30, 999)), "2026-10-26T12:10:30Z", "?t= to the second, in UTC");
  assert.strictEqual(S.clock("?t=" + S.rfc3339(NOON + 610000), 5), NOON + 610000, "and read back the same");
  assert.strictEqual(S.query("?lat=52&t=a&lon=1", { t: "2026-10-26T12:00:00Z", play: null }), "?lat=52&t=2026-10-26T12:00:00Z&lon=1", "?t= rewritten in place, the rest kept");
  assert.strictEqual(S.query("?t=a&play=1", { t: null, play: null }), "", "Now drops ?t= and ?play=");
  assert.strictEqual(S.query("", { play: "1" }), "?play=1");
  assert.ok(S.dayStart(NOON) === Date.UTC(2026, 9, 26) && S.minute(NOON, S.dayStart(NOON)) === 720 && S.minute(NOON + DAY_MS, S.dayStart(NOON)) === 1439, "the scrubber's minute of the local day");
  assert.deepStrictEqual([S.step(720, "ArrowRight"), S.step(720, "ArrowLeft"), S.step(720, "ArrowRight", true), S.step(720, "ArrowLeft", true), S.step(5, "ArrowLeft"), S.step(1435, "ArrowRight", true), S.step(720, "Home")],
    [730, 710, 780, 660, 0, 1439, null], "arrows by 10 minutes, an hour with Shift, held to the day");
  var rd = S.reading(NOON, pal), night = A.skyPalette(Date.UTC(2026, 9, 26, 23), 45, 0), rn = S.reading(Date.UTC(2026, 9, 26, 23, 5), night);
  assert.deepStrictEqual(rd, { time: "12:00", date: "26 Oct", twilight: "day", sun: "sun " + Math.round(pal.sun.elevation) + "\u00b0", moon: pal.moon.phase + " moon " + Math.round(pal.moon.fraction * 100) + "%" }, "the readout at noon: " + JSON.stringify(rd));
  assert.ok(rn.time === "23:05" && rn.twilight === "night" && rn.sun === "sun \u2212" + Math.abs(Math.round(night.sun.elevation)) + "\u00b0", "and at night: " + JSON.stringify(rn));
  assert.deepStrictEqual([S.breathes("", mm0(false)), S.breathes("?t=x", mm0(false)), S.breathes("", mm0(true)), S.breathes("?wind=1&t=x", mm0(false))], [true, false, false, false], "the camera breathes unless ?t= pins the clock or motion is reduced");

  // the wind blows when the clock runs free or ?wind=1 asks, never under reduced motion
  var mm = function (reduce) { return { matchMedia: function (q) { return { matches: reduce && /reduce/.test(q) }; } }; };
  assert.deepStrictEqual(["", "?t=2026-10-26T16:00:00Z", "?t=2026-10-26T16:00:00Z&wind=1", "?lat=52"].map(function (q) { return S.motion(q, mm(false)); }), [true, false, true, true], "motion by the query");
  assert.deepStrictEqual([S.motion("", mm(true)), S.motion("?wind=1", mm(true)), S.motion("", {})], [false, false, true], "never under reduced motion");
  var pm = function (coarse, w, h) { return { screen: { width: w, height: h }, matchMedia: function (q) { return { matches: coarse && /coarse/.test(q) }; } }; };
  assert.deepStrictEqual([S.phone(pm(true, 1280, 800)), S.phone(pm(false, 390, 844)), S.phone(pm(false, 1440, 900)), S.phone({})], [true, true, false, false], "a phone by its pointer or its screen");

  // the quality tiers (step 9): the knobs, the guess, ?q=, the warm-up and the watch
  assert.deepStrictEqual(Q.ORDER.map(function (n) { var t = Q.TIERS[n]; return [n, t.grass, t.cascades, t.near, t.far, t.pond, t.ratio, t.impostors]; }),
    [["low", 8000, 1, 0, 1024, "water", 1, true], ["mid", 25000, 1, 0, 2048, "tint", 1.5, false], ["high", 60000, 2, 2048, 1024, "mirror", 2, false]], "the three tiers' knobs");
  assert.deepStrictEqual(["?q=low", "?t=x&q=MID", "?q=high&t=x", "?q=ultra", "", "?quality=low"].map(Q.override), ["low", "mid", "high", null, null, null], "?q= names a tier");
  assert.deepStrictEqual([{ phone: true, cores: 16 }, { cores: 16, dpr: 1 }, { cores: 8, dpr: 2 }, { cores: 8, dpr: 3 }, { cores: 4 }, { cores: 2 }, {}, { cores: 2, dpr: 3 }].map(Q.guess),
    ["low", "high", "high", "mid", "mid", "low", "mid", "low"], "the guess: a phone low, a laptop by its cores, a dense screen a tier less");
  assert.deepStrictEqual([Q.pick("?q=high", { phone: true }), Q.pick("", { phone: true })], [{ tier: "high", held: true }, { tier: "low", held: false }], "?q= holds the tier, else the guess may step down");
  assert.deepStrictEqual([Q.budget(true), Q.budget(false), Q.limit(16), Q.below("high"), Q.below("mid"), Q.below("low")], [33, 16, 20, "mid", "low", null], "the budgets, the slack and the order");
  function frames(n, ms) { var a = []; for (var i = 0; i < n; i++) a.push(i < Q.SKIP ? 900 : ms); return a; }
  assert.strictEqual(Q.warm(frames(10, 16.7), 16, "high"), null, "the warm-up wants its 20 frames");
  assert.deepStrictEqual([Q.warm(frames(20, 16.7), 16, "high"), Q.warm(frames(20, 30), 16, "high"), Q.warm(frames(20, 45), 16, "high"), Q.warm(frames(20, 45), 16, "mid"), Q.warm(frames(20, 70), 33, "low")],
    ["high", "mid", "low", "low", "low"], "a 60 Hz frame is in budget; 30 ms a tier down, 45 two; never under low");
  assert.deepStrictEqual([Q.warm([54000, 9000, 9000, 9000], 33, "low"), Q.warm([900, 900, 400, 400], 16, "high")], ["low", "low"], "frames of seconds are judged on a few");
  var qw = Q.watch(16), steps = [];
  for (var qf = 1; qf <= 400; qf++) if (qw.frame(qf * 16.7, 16.7)) steps.push(qf);
  assert.deepStrictEqual(steps, [], "in budget, the watch never steps down");
  var qt = 400 * 16.7;
  for (qf = 1; qf <= 200; qf++) { qt += 25; if (qw.frame(qt, 25)) steps.push(qf); }
  assert.ok(steps.length === 1 && steps[0] >= 120 && steps[0] <= 160, "3 s running over budget step down once: frame " + steps);
  qw = Q.watch(16); steps = []; qt = 0;
  for (qf = 0; qf < 1000; qf++) { var qd = (qf % 100) < 40 ? 25 : 16.7; qt += qd; if (qw.frame(qt, qd)) steps.push(qf); }
  assert.deepStrictEqual(steps, [], "over budget a second at a time with a second in budget between, never 3 s running: no step");

  // boot: WebGL, or the still and its note. The page's canvases paint on a 2D context that counts what is drawn.
  var painted = 0;
  function ctx2d() {
    return new Proxy({}, {
      get: function (t, k) {
        if (k === "getImageData") return function (x, y, wd, ht) { return { data: new Uint8ClampedArray(wd * ht * 4) }; };
        if (k === "createRadialGradient" || k === "createLinearGradient") return function () { painted++; return { addColorStop: function () {} }; };
        return k in t ? t[k] : function () { painted++; };
      },
      set: function (t, k, val) { t[k] = val; return true; }
    });
  }
  // the page's events: on(target) gives a target addEventListener and an emit(type, event) that fires what it heard
  function on(t) {
    var heard = {};
    t.addEventListener = function (type, fn) { (heard[type] = heard[type] || []).push(fn); };
    t.emit = function (type, e) { (heard[type] || []).forEach(function (fn) { fn(e || {}); }); };
    return t;
  }
  function page(gl, search, nav) {
    var flat = { hidden: true }, canvas = on({ clientWidth: 100, clientHeight: 100, dataset: {}, attrs: {}, getAttribute: function (k) { return this.attrs[k]; }, setAttribute: function (k, v) { this.attrs[k] = String(v); },
      getBoundingClientRect: function () { return { left: 0, top: 0, width: 100, height: 100 }; } }), seen = [], reduce = on({ matches: false }), added = [];
    var doc = on({
      defaultView: { Path2D: function () {} },
      visibilityState: "visible",
      body: { dataset: { three: "/v/garden/splash/three.module.min.js?v=x" }, appendChild: function (el) { added.push(el); } },
      getElementById: function (id) { return id === "splash" ? canvas : id === "splash-flat" ? flat : null; },
      createElement: function (tag) {
        if (tag === "output") return { tagName: "OUTPUT", attrs: {}, textContent: "", setAttribute: function (k, v) { this.attrs[k] = v; } };
        return { getContext: function (k) { return gl ? (k === "2d" ? ctx2d() : {}) : null; } };
      },
    });
    var w = on({ location: { pathname: "/garden/splash", hash: "", search: search || "?t=2026-10-26T12:00:00Z", reload: function () { w.reloaded = true; } },
      history: { state: null, replaceState: function (st, t, u) { w.location.search = u.slice(u.indexOf("?")); } }, setTimeout: function (fn) { w.quiet = fn; }, clearTimeout: function () {}, devicePixelRatio: 1, ResizeObserver: win.ResizeObserver, navigator: nav || { hardwareConcurrency: 8 },
      IntersectionObserver: function (fn) { this.observe = function (el) { seen.push([el, fn]); }; },
      matchMedia: function (q) { return /reduce/.test(q) ? reduce : { matches: false }; } });
    return { doc: doc, flat: flat, canvas: canvas, win: w, seen: seen, reduce: reduce, added: added };
  }
  var boots = {};
  var p = page(false), asked = [];
  await S.boot(p.doc, p.win, function (u) { asked.push(u); return Promise.resolve(T); });
  assert.ok(p.doc.body.dataset.gl === "off" && !p.flat.hidden && asked.length === 0, "no WebGL: the still, and three.js never fetched");
  assert.strictEqual(S.t, Date.UTC(2026, 9, 26, 12), "boot reads ?t=");
  boots.nowebgl = p.doc.body.dataset.why;
  p = page(true);
  await S.boot(p.doc, p.win, function (u) { asked.push(u); return Promise.reject(new Error("Failed to fetch dynamically imported module")); });
  assert.ok(p.doc.body.dataset.gl === "off" && !p.flat.hidden && asked[0] === p.doc.body.dataset.three, "three.js does not load: the still");
  boots.noload = p.doc.body.dataset.why;
  p = page(true);
  await S.boot(p.doc, p.win, function () { return Promise.resolve(Object.assign({}, THREE, { WebGLRenderer: function () { throw new Error("Error creating WebGL context."); } })); });
  assert.ok(p.doc.body.dataset.gl === "off" && !p.flat.hidden, "the renderer throws: the still");
  boots.nocontext = p.doc.body.dataset.why;
  p = page(true, "?t=2026-10-26T12:00:00Z&lat=52&lon=-1.5");
  await S.boot(p.doc, p.win, function () { return Promise.resolve(T); });
  assert.ok(p.doc.body.dataset.gl === "on" && p.flat.hidden, "WebGL: the canvas, the still hidden");
  assert.strictEqual(p.canvas.attrs["aria-label"], S.label(S.view.palette), "the canvas named for the picture at ?t=");
  assert.ok(/^Midday over the farm: the sun [a-z ]+ in the south; the gardenio stone on its hill$/.test(p.canvas.attrs["aria-label"]), p.canvas.attrs["aria-label"]);
  assert.ok(p.added.length === 0 && S.view.perf.roll === null, "without ?perf=1 no readout is added and no frames kept for it");
  var pond = S.view.farm.getObjectByName("pond");
  assert.ok(S.view.tier.name === "high" && pond.userData.reflect && pond.userData.target.isWebGLRenderTarget && pond.userData.camera.layers.mask === 1 << FM.REFLECT, "a laptop of 8 cores starts high, its pond a mirror, its camera seeing the mirror's layer");
  // the lifecycle: hidden or off screen it pauses, on pagehide it lets the GPU go, from the back-forward cache it loads again
  var bv = S.view;
  p.doc.visibilityState = "hidden"; p.doc.emit("visibilitychange");
  assert.ok(bv.paused, "the page hidden: paused");
  p.doc.visibilityState = "visible"; p.doc.emit("visibilitychange");
  assert.ok(!bv.paused && p.seen.length === 1 && p.seen[0][0] === p.canvas, "visible again: drawing; the canvas watched for being on screen");
  p.seen[0][1]([{ isIntersecting: false }]);
  assert.ok(bv.paused, "the canvas off screen: paused");
  p.seen[0][1]([{ isIntersecting: true }]);
  assert.ok(!bv.paused, "on screen again: drawing");
  p.win.emit("pageshow", { persisted: true });
  assert.ok(!p.win.reloaded, "a first pageshow does not reload");
  p.win.emit("pagehide");
  assert.ok(p.canvas.dataset.gone === "1" && bv.paused, "pagehide: everything let go");
  p.win.emit("pageshow", { persisted: true });
  assert.ok(p.win.reloaded, "back from the back-forward cache: the page loads afresh");
  assert.deepStrictEqual(S.place, { lat: 52, lon: -1.5 }, "boot reads ?lat=&lon=");
  assert.strictEqual(S.view.rate, 0, "?t= pins the clock");
  // the tier by the device and ?q=: a phone low, ?q= held
  p = page(true, "?t=2026-10-26T12:00:00Z&q=mid", { hardwareConcurrency: 2 });
  await S.boot(p.doc, p.win, function () { return Promise.resolve(T); });
  assert.ok(S.view.tier.name === "mid" && S.view.judging === null && S.view.farm.getObjectByName("pond").userData.mode === "tint", "?q=mid holds a 2-core laptop at mid, unjudged");
  p = page(true, "?t=2026-10-26T12:00:00Z&wind=1", { hardwareConcurrency: 8 });
  p.win.screen = { width: 390, height: 844 };
  await S.boot(p.doc, p.win, function () { return Promise.resolve(T); });
  assert.ok(S.view.tier.name === "low" && S.view.judging === "warm-up" && S.view.perf.budget === 33, "a phone starts low against a 33 ms budget, and warms up");
  // ?perf=1: the readout added to the body, aria-live off, written again every 2 s until pagehide
  p = page(true, "?t=2026-10-26T12:00:00Z&wind=1&perf=1", { hardwareConcurrency: 8 });
  var ptimers = [], pcleared = [];
  p.win.setInterval = function (fn, ms) { ptimers.push([fn, ms]); return ptimers.length; };
  p.win.clearInterval = function (id) { pcleared.push(id); };
  await S.boot(p.doc, p.win, function () { return Promise.resolve(T); });
  var pel = p.added[0], ptick = ptimers.filter(function (x) { return x[1] === S.METER_EVERY; });
  assert.ok(p.added.length === 1 && pel.id === "splash-perf" && pel.tagName === "OUTPUT" && pel.attrs["aria-live"] === "off" && pel.attrs["aria-label"] && ptick.length === 1 && S.view.perf.roll,
    "?perf=1 adds the readout, not announced, on a 2 s timer: " + JSON.stringify(pel));
  assert.ok(/^splash: frame 0 ms \(p95 0\), draw 0 ms \(p95 0\) over 0 frames, \d+ blades, high tier, budget 16 ms\nrunning 0 s, the last 10 s; no step-downs$/.test(pel.textContent), "before a frame it names the tier and the budget: " + JSON.stringify(pel.textContent));
  S.view.perf.roll.push([100, 17, 4]);
  ptick[0][0]();
  assert.ok(/ over 1 frames, /.test(pel.textContent), "the timer writes it again: " + JSON.stringify(pel.textContent));
  p.win.emit("pagehide");
  assert.ok(pcleared.indexOf(ptimers.indexOf(ptick[0]) + 1) >= 0, "pagehide stops its timer");
  // ?z=&look= opens the page zoomed and turned, from the first frame (a 100 px square canvas: the designed fov 60)
  p = page(true, "?t=2026-10-26T12:00:00Z&z=4&look=12,-3");
  await S.boot(p.doc, p.win, function () { return Promise.resolve(T); });
  observed[observed.length - 1][1]();
  var bz = bearing(S.view.camera), bd = bearing(designed(60, 1));
  assert.ok(JSON.stringify(p.win.__splash.state()) === JSON.stringify({ zoom: 4, yaw: 12, pitch: -3 }) && S.view.camera.fov === V.fov(60, 4) && p.canvas.dataset.frames === "1" &&
    Math.abs(bz[0] - bd[0] - 12) < 1e-9 && Math.abs(bz[1] - bd[1] + 3) < 1e-9, "?z=4&look=12,-3 opens zoomed 4x, 12 degrees right and 3 down: " + JSON.stringify(bz));
  p.win.__splash.look({ zoom: 2 });
  assert.deepStrictEqual(p.win.__splash.state(), { zoom: 2, yaw: 0, pitch: 0 }, "the probe's look moves the view");
  // the gestures (no animation frames here, so each change lands at once): a wheel zooms toward the pointer and writes ?z= once
  // quiet, a finger's drag turns the view and writes look= as it lifts, and a double tap zooms 2x
  var stopped = 0;
  p.canvas.emit("wheel", { deltaY: -300, deltaMode: 0, clientX: 50, clientY: 50, preventDefault: function () { stopped++; } });
  assert.ok(stopped === 1 && Math.abs(S.view.state().zoom - 2 * Math.exp(0.6)) < 1e-9 && p.win.location.search.indexOf("z=3.64") < 0, "the wheel zooms, the page never scrolls: " + JSON.stringify(S.view.state()));
  p.win.quiet();
  assert.ok(/[?&]z=3\.64(&|$)/.test(p.win.location.search), "and the address says so once the wheel is quiet: " + p.win.location.search);
  var yaw0 = S.view.state().yaw;
  p.canvas.emit("pointerdown", { pointerId: 1, pointerType: "touch", clientX: 50, clientY: 50, timeStamp: 1000 });
  p.canvas.emit("pointermove", { pointerId: 1, pointerType: "touch", clientX: 60, clientY: 50 });
  p.canvas.emit("pointerup", { pointerId: 1, pointerType: "touch", clientX: 60, clientY: 50 });
  assert.ok(Math.abs(S.view.state().yaw - (yaw0 - 10 * 60 / 100 / S.view.state().zoom)) < 1e-9 && /look=/.test(p.win.location.search), "a drag of 10 px right turns the view left: " + p.win.location.search);
  p.win.__splash.look({ zoom: 2 });
  p.canvas.emit("pointerdown", { pointerId: 2, pointerType: "touch", clientX: 50, clientY: 50, timeStamp: 5000 });
  p.canvas.emit("pointerup", { pointerId: 2, pointerType: "touch", clientX: 50, clientY: 50 });
  p.canvas.emit("pointerdown", { pointerId: 3, pointerType: "touch", clientX: 52, clientY: 51, timeStamp: 5200 });
  assert.ok(Math.abs(S.view.state().zoom - 4) < 1e-9 && /[?&]z=4(&|$)/.test(p.win.location.search), "a double tap zooms 2x: " + JSON.stringify(S.view.state()));
  p = page(true, "?t=2026-10-26T12:00:00Z&play=1");
  await S.boot(p.doc, p.win, function () { return Promise.resolve(T); });
  assert.ok(S.view.rate === S.PLAY && S.view.t === NOON, "?play=1 plays the day from ?t=");
  assert.ok(painted > 10000, "the field map painted on the page's canvases: " + painted + " calls");
  var lit = S.view.scene.getObjectByName("land").material;
  assert.ok(lit.map && lit.map.isCanvasTexture && lit.normalMap && lit.normalMap.repeat.x > 50, "the land wears its painted map and a tiling detail normal map");
  var cut = S.view.scene.getObjectByName("stone body").material;
  assert.ok(cut.displacementMap && cut.displacementMap.channel === 1 && cut.normalMap.channel === 1, "the mark cut in the stone's face");
  var bs = { uniforms: {}, fragmentShader: "#include <map_pars_fragment>\n#include <map_fragment>\n#include <roughnessmap_fragment>\n#include <fog_pars_fragment>\n#include <fog_fragment>" };
  lit.onBeforeCompile(bs);
  assert.ok(bs.uniforms.uSplat.value.isCanvasTexture && bs.uniforms.uDetail.value.isDataTexture && /roughnessFactor = dot\(matW/.test(bs.fragmentShader) && /uFogFall/.test(bs.fragmentShader),
    "the land's material blends grass, earth, chalk and gravel from the splat, and its fog thins with height");

  // ?t= pins the clock: unix ms, or a date string; without one, or unreadable, now
  assert.strictEqual(S.clock("?t=1793030400000", 5), 1793030400000);
  assert.strictEqual(S.clock("?x=1&t=2026-10-26T12:00:00Z", 5), Date.UTC(2026, 9, 26, 12));
  assert.strictEqual(S.clock("", 5), 5);
  assert.strictEqual(S.clock("?t=soon", 5), 5);

  console.log(JSON.stringify({ three: THREE.REVISION, objects: kinds, boxInView: c.toArray().map(function (n) { return +n.toFixed(3); }), framed: framed, looked: looked, light: [pal.source, +pal.strength.toFixed(2)], fallbacks: boots }));
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
