/* The splash's light (web/splash/light.js, plan gd-EJGA3zDGCk step 7), run by TestSplashLightUnderNode:  node splash_light_test.js <web dir>
   With the vendored three.js, the land and the stone: the light is two directional lights of the same colour and strength, the
   near cascade a 2048 px square round the stone, the far one 1024 px fitted round FAR_BOX along the light at every hour, fine
   enough a texel for the oak's dapple, and three's light loop patched once so the pair sums to one light; through a day at
   45 N in October and 52 N at midsummer, a minute at a time, nothing pops: the light's colour times its strength, the
   ambient, the exposure, the sky's colours, the stars and the fireflies all move by small steps, and the light's direction
   jumps only at the hand-over from the sun to the moon, where neither shines. The exposure is 1 by day and opens at night,
   further under a new moon. The moon is lit on the side towards the sun as far as its lit fraction says; its face's frame is
   orthonormal. The stars are seeded and turn with the sidereal clock about the pole, westward, a star on the meridian at
   hour angle 0, and come out under civil twilight; the fireflies drift over the meadow, above the grass and clear of the
   stone, from civil dusk to astronomical night. The stone's shadow, read from the scene, falls west at 07:00 and east at
   17:00 at the equinox and short and north at noon, and far west at an October dawn. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var web = process.argv[2], A = require(path.join(web, "almanac.js")), L = require(path.join(web, "splash", "land.js"));
var ST = require(path.join(web, "splash", "stone.js")), LT = require(path.join(web, "splash", "light.js"));
var MIN = 60000, DAY = 86400000;

(async function () {
  var THREE = await import(path.join(web, "vendor", "three.module.min.js"));
  var lands = L.build(THREE), stone = ST.build(THREE, null, null), scene = new THREE.Scene(), fog = new THREE.FogExp2(0, L.FOG_DENSITY);
  var eye = new THREE.Vector3().fromArray(L.camera(50).eye), NOON = Date.UTC(2026, 9, 26, 12);
  var rig = LT.build(THREE, { t: NOON, lat: 45, lon: 0, stone: stone, eye: eye, fog: fog });
  scene.add(lands, stone, rig);
  scene.updateMatrixWorld(true);
  var o = rig.userData, near = rig.getObjectByName("sun"), far = rig.getObjectByName("sun far");

  // the two cascades, near first, and three's light loop patched once
  var lights = rig.children.filter(function (c) { return c.isDirectionalLight; });
  assert.ok(lights[0] === near && lights[1] === far && near.castShadow && far.castShadow, "the near cascade first among the lights that cast shadow, as cascade() reads them");
  var nsc = near.shadow.camera;
  assert.ok(near.target === stone && near.shadow.mapSize.x === 2048 && nsc.right - nsc.left === 8 && nsc.top - nsc.bottom === 8, "the near cascade: 2048 px over 8 m round the stone, 4 mm a texel");
  assert.ok(far.shadow.mapSize.x === 1024 && far.target === rig.getObjectByName("far target"), "the far cascade: 1024 px");
  var chunk = THREE.ShaderChunk.lights_fragment_begin, pars = THREE.ShaderChunk.shadowmap_pars_fragment;
  LT.cascade(THREE); LT.cascade(THREE);
  assert.ok(THREE.ShaderChunk.lights_fragment_begin === chunk && chunk.split("splashNear()").length === 3 && pars.split("float splashNear()").length === 2, "the light loop patched once, however often it is asked");
  assert.ok(/getDirectionalLightInfo\( directionalLight, directLight \);\s*#if defined\( USE_SHADOWMAP \) && NUM_DIR_LIGHT_SHADOWS == 2 && NUM_DIR_LIGHTS == 2\s*directLight\.color \*= UNROLLED_LOOP_INDEX == 0 \? splashNear\(\) : 1\.0 - splashNear\(\);/.test(chunk),
    "inside the directional loop: light 0 by the near weight, light 1 by the rest");

  // the far cascade holds FAR_BOX at every hour, its texel fine enough for the oak's dapple
  var B = LT.FAR_BOX, worst = 0, v = new THREE.Vector3(), m = new THREE.Matrix4();
  for (var h = 0; h < 24; h++) {
    LT.update(rig, NOON + (h - 12) * 3600000);
    var sc = far.shadow.camera;
    m.multiplyMatrices(sc.projectionMatrix, sc.matrixWorldInverse);
    for (var i = 0; i < 8; i++) {
      v.set(i & 1 ? B.x1 : B.x0, i & 2 ? B.y1 : B.y0, i & 4 ? B.z1 : B.z0).applyMatrix4(m);
      assert.ok(Math.abs(v.x) <= 1 + 1e-9 && Math.abs(v.y) <= 1 + 1e-9 && Math.abs(v.z) <= 1 + 1e-9, "hour " + h + ": the far box's corner " + i + " in the far cascade: " + v.toArray());
    }
    var texel = Math.max(sc.right - sc.left, sc.top - sc.bottom) / 1024;
    worst = Math.max(worst, texel);
    assert.ok(far.position.clone().sub(far.target.position).normalize().distanceTo(near.position.clone().sub(stone.position).normalize()) < 1e-9, "hour " + h + ": both cascades along the one light");
  }
  assert.ok(worst < 0.3, "the far cascade's texel at most " + worst.toFixed(3) + " m, under the oak's leaf clusters");

  // a day, a minute at a time: nothing pops
  function lum(c, k) { return [c.r * k, c.g * k, c.b * k]; }
  function d3(a, b) { return Math.max(Math.abs(a[0] - b[0]), Math.abs(a[1] - b[1]), Math.abs(a[2] - b[2])); }
  var steps = {};
  [[Date.UTC(2026, 9, 26), 45, 0], [Date.UTC(2026, 5, 21), 52, -1.5]].forEach(function (day) {
    var r = LT.build(THREE, { t: day[0], lat: day[1], lon: day[2], stone: stone, eye: eye, fog: fog }), u = r.userData, prev = null, swaps = 0;
    for (var t = day[0]; t <= day[0] + DAY; t += MIN) {
      var p = LT.update(r, t), sk = u.sky.material.uniforms;
      var now = {
        light: lum(u.near.color, u.near.intensity), farLight: lum(u.far.color, u.far.intensity), sky: lum(u.hemi.color, u.hemi.intensity), exposure: u.exposure,
        stars: u.stars.material.uniforms.uK.value, flies: u.fireflies.material.uniforms.uK.value, zenith: sk.uZenith.value.toArray(), fog: fog.color.toArray(),
        dir: u.near.position.clone().sub(u.near.target.position).normalize(), source: p.source, strength: p.strength
      };
      assert.ok(d3(now.light, now.farLight) < 1e-12, "the cascades shine alike");
      if (prev) {
        ["light", "sky", "zenith", "fog"].forEach(function (k) { var s = d3(now[k], prev[k]); steps[k] = Math.max(steps[k] || 0, s); });
        ["exposure", "stars", "flies"].forEach(function (k) { var s = Math.abs(now[k] - prev[k]); steps[k] = Math.max(steps[k] || 0, s); });
        if (now.source !== prev.source) {
          swaps++;
          assert.ok(now.strength < 0.005 && prev.strength < 0.005, "the sun hands over to the moon (or back) where neither shines: " + new Date(t).toISOString() + " " + prev.strength + " to " + now.strength);
        } else if (now.strength > 0.01) {
          assert.ok(now.dir.angleTo(prev.dir) < 0.6 * Math.PI / 180, "the light moves smoothly: " + new Date(t).toISOString());
        }
      }
      prev = now;
    }
    assert.ok(swaps === 2, "one dusk and one dawn hand-over in the day: " + swaps);
  });
  // the sun's strength (3 at full) grows by under 3% of it a minute at a midsummer sunrise, the almanac's ramp; the
  // fireflies come out over ten minutes or more
  assert.ok(steps.light < 0.08 && steps.sky < 0.02 && steps.zenith < 0.02 && steps.fog < 0.02, "the light, the sky light, the sky and the fog move by small steps: " + JSON.stringify(steps));
  assert.ok(steps.exposure < 0.05 && steps.stars < 0.03 && steps.flies < 0.1, "the exposure, the stars and the fireflies too: " + JSON.stringify(steps));

  // the exposure: 1 by day, open at night, wider under a new moon; the stars out at night and under the moon; the fireflies at dusk
  var at = function (iso, lat) { return A.skyPalette(Date.parse(iso), lat == null ? 45 : lat, 0); };
  var noon = at("2026-10-26T12:00:00Z"), full = at("2026-10-27T00:00:00Z"), dark = at("2026-11-09T23:00:00Z"), dusk = at("2026-10-26T17:50:00Z");
  assert.ok(LT.exposure(noon) === 1 && LT.exposure(full) > 2 && LT.exposure(full) < LT.exposure(dark) && LT.exposure(dark) === LT.EXPOSE_MAX, "exposure by day " + LT.exposure(noon) + ", under a full moon " + LT.exposure(full).toFixed(2) + ", a new one " + LT.exposure(dark));
  assert.ok(dark.strength === 0 && dark.moon.fraction < 0.01, "a new moon gives no light: only the stars, the farm's windows and the gems");
  assert.ok(LT.starlight(noon) === 0 && LT.starlight(dark) === 1 && LT.starlight(full) > 0.5 && LT.starlight(full) < 1, "stars: none by day, all on a moonless night, the faint ones washed out by a full moon");
  assert.ok(LT.starlight(at("2026-10-26T17:00:00Z")) < 0.05 && LT.starlight(at("2026-10-26T17:30:00Z")) > 0.2, "the stars come out under civil twilight");
  assert.ok(LT.fireflies(dusk) > 0.9 && LT.fireflies(noon) === 0 && LT.fireflies(dark) === 0 && LT.fireflies(at("2026-10-26T16:30:00Z")) === 0, "fireflies from civil dusk until astronomical night");
  assert.ok(LT.warmth(noon) === 0 && LT.warmth(at("2026-10-26T07:00:00Z")) > 0.5 && LT.warmth(dark) === 0, "the sky warms round a low sun only");
  var nr = LT.build(THREE, { t: Date.parse("2026-10-27T00:00:00Z"), lat: 45, lon: 0, stone: stone, eye: eye, fog: fog }).userData;
  assert.ok(nr.near.name === "moon" && nr.far.name === "moon far" && nr.near.color.getHex() === new THREE.Color(full.light).getHex() && Math.abs(nr.near.intensity - full.strength) < 1e-12, "at night the moon is the light, its colour and strength the almanac's");
  assert.ok(nr.hemi.color.getHex() === new THREE.Color(LT.NIGHT_SKY).getHex() && nr.stars.visible && !nr.fireflies.visible, "the night's sky light a dim blue, the stars out, no fireflies at midnight");

  // the moon: lit towards the sun, as much of the disc as its lit fraction
  function lit(f) { // the shader's terminator: lit where x > (1 - 2f) sqrt(1 - y^2), x towards the sun
    var n = 0, on = 0, side = 0;
    for (var y = -1; y <= 1; y += 0.01) for (var x = -1; x <= 1; x += 0.01) {
      if (x * x + y * y > 1) continue;
      n++;
      if (x > (1 - 2 * f) * Math.sqrt(1 - y * y)) { on++; side += x; }
    }
    return { fraction: on / n, side: side };
  }
  [0.05, 0.25, 0.5, 0.75, 0.97].forEach(function (f) { var l = lit(f); assert.ok(Math.abs(l.fraction - f) < 0.01 && l.side > 0, "a moon " + f + " lit shows " + l.fraction.toFixed(3) + " of its disc, on the sun's side"); });
  var shader = rig.getObjectByName("sky").material.fragmentShader;
  assert.ok(shader.indexOf("float term = ( 1.0 - 2.0 * uMoonLit ) * sqrt( max( 1.0 - q.y * q.y, 0.0 ) );") >= 0 && shader.indexOf("vec2 q = vec2( dot( o, uToSun )") >= 0, "the sky's shader draws that terminator, x towards the sun");
  ["2026-11-17T19:00:00Z", "2026-11-03T05:00:00Z", "2026-10-26T22:00:00Z"].forEach(function (iso) {
    var p = at(iso), b = LT.bodies(p), mv = new THREE.Vector3().fromArray(b.moon), ts = new THREE.Vector3().fromArray(b.toSun), up = new THREE.Vector3().fromArray(b.up), east = new THREE.Vector3().fromArray(b.east);
    assert.ok(Math.abs(ts.length() - 1) < 1e-9 && Math.abs(ts.dot(mv)) < 1e-9 && Math.abs(up.dot(mv)) < 1e-9 && Math.abs(up.dot(east)) < 1e-9 && Math.abs(east.length() - 1) < 1e-9, iso + ": the moon's face frame is orthonormal on the sky");
    if (p.moon.fraction < 0.98) assert.ok(ts.dot(new THREE.Vector3().fromArray(b.sun)) > 0, iso + ": its lit limb towards the sun");
  });

  // the stars: seeded, turned by the sidereal clock about the pole
  var s1 = LT.starField(), s2 = LT.starField();
  assert.ok(s1.size.length === LT.STARS && LT.STARS >= 2000 && Buffer.from(s1.position.buffer).equals(Buffer.from(s2.position.buffer)), "a few thousand stars, the same every time");
  function place(lat, lst, ra, dec) { // a star's direction in the scene through pole()
    var R = LT.pole(lat, lst), e = [Math.cos(dec) * Math.cos(ra), Math.cos(dec) * Math.sin(ra), Math.sin(dec)];
    return [R[0] * e[0] + R[3] * e[1] + R[6] * e[2], R[1] * e[0] + R[4] * e[1] + R[7] * e[2], R[2] * e[0] + R[5] * e[1] + R[8] * e[2]];
  }
  function near3(a, b) { return d3(a, b) < 1e-9; }
  var RAD = Math.PI / 180;
  assert.ok(near3(place(45, 123, 0, Math.PI / 2), A.direction(45, 0)), "the pole star stands over the north at the latitude");
  assert.ok(near3(place(45, 100, 100 * RAD, 20 * RAD), A.direction(65, 180)), "a star at hour angle 0 on the meridian, 90 - lat + dec up");
  var e0 = place(45, 100, 100 * RAD, 0), e1 = place(45, 115, 100 * RAD, 0);
  assert.ok(e1[0] < e0[0] - 0.1, "an hour on it has moved west");
  assert.ok(near3(place(-35, 40, 40 * RAD, 0), A.direction(55, 0)), "south of the equator the equator crosses the meridian in the north");
  var sm = o.stars.matrix.elements;
  assert.ok(Math.abs(new THREE.Matrix4().fromArray(sm).determinant()) > 0.99, "the stars' frame is a rotation");

  // the fireflies: over the meadow, above the grass, clear of the stone
  var sw = LT.swarm(), MD = LT.MEADOW;
  assert.ok(sw.position.length === LT.FIREFLIES * 3, "the fireflies");
  for (var k = 0; k < LT.FIREFLIES; k++) {
    var x = sw.position[k * 3], y = sw.position[k * 3 + 1], z = sw.position[k * 3 + 2], g = L.height(x, z);
    assert.ok(x >= MD.x0 && x <= MD.x1 && z >= MD.z0 && z <= MD.z1 && y - g >= 0.3 && y - g <= 1.6 && (Math.abs(x - L.STONE.x) >= 1.4 || Math.abs(z - L.STONE.z) >= 1.4), "firefly " + k + " over the meadow");
  }

  // the stone's shadow, read from the scene
  function shadow(iso, lat, lon) { LT.update(rig, Date.parse(iso)); rig.userData.lat = lat; rig.userData.lon = lon; LT.update(rig, Date.parse(iso)); scene.updateMatrixWorld(true); return LT.tip(THREE, rig, stone, lands); }
  rig.userData.lat = 45; rig.userData.lon = 0;
  var am = shadow("2026-03-20T07:00:00Z", 45, 0), pm = shadow("2026-03-20T17:00:00Z", 45, 0), md = shadow("2026-03-20T12:00:00Z", 45, 0), dawn = shadow("2026-10-26T07:00:00Z", 45, 0), deep = shadow("2026-11-09T23:00:00Z", 45, 0);
  assert.ok(am.source === "sun" && am.west && am.bearing > 225 && am.bearing < 315, "the equinox at 07:00: the shadow west of the stone " + JSON.stringify(am));
  assert.ok(pm.source === "sun" && pm.east && pm.bearing > 45 && pm.bearing < 135, "at 17:00 east " + JSON.stringify(pm));
  assert.ok(md.length < am.length / 2 && md.length < pm.length / 2 && (md.bearing > 340 || md.bearing < 20), "at noon short and to the north " + JSON.stringify(md));
  assert.ok(dawn.west && dawn.length > 50, "a long one at an October dawn: " + dawn.length.toFixed(0) + " m");
  assert.ok(deep === null || deep.strength === 0, "under a new moon in the dead of night no light throws one");

  console.log(JSON.stringify({ farTexel: +worst.toFixed(3), steps: Object.keys(steps).reduce(function (a, k) { a[k] = +steps[k].toFixed(4); return a; }, {}),
    exposure: { noon: LT.exposure(noon), full: +LT.exposure(full).toFixed(2), dark: LT.exposure(dark) },
    shadow: { am: [+am.bearing.toFixed(0), +am.length.toFixed(1)], noon: [+md.bearing.toFixed(0), +md.length.toFixed(1)], pm: [+pm.bearing.toFixed(0), +pm.length.toFixed(1)], dawn: +dawn.length.toFixed(0) } }));
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
