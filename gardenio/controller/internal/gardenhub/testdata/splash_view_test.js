/* The splash's view maths (web/splash/view.js, plan gd-I5QHVfGLbD step 1), run by TestSplashView:  node splash_view_test.js <web dir>
   The bounds hold: a view past them is held at them, one inside them is kept, and what cannot be read is the default. The
   default view aims where LAND.camera does, so the designed frame does not move. VIEW.ray agrees with three's own camera
   (fov from VIEW.fov, aimed by VIEW.aim) on the direction under a pixel, and a zoom toward a corner or any point keeps
   that point's direction within 1e-6 radians, on a laptop and on a phone upright; zooming back out there comes home to the
   default. A drag by the frame's full height turns the pitch by the fov at zoom 1 and by the fov over the zoom at zoom 4,
   the ground following the finger. The wheel's and the pinch's factors. The address: ?z=&look= round-trips through
   SPLASH.query to two decimals, and the default view writes nothing. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var web = process.argv[2], V = require(path.join(web, "splash", "view.js")), L = require(path.join(web, "splash", "land.js"));
var S = require(path.join(web, "splash.js"));
var RAD = Math.PI / 180;
function near(a, b, eps, what) { assert.ok(Math.abs(a - b) <= eps, what + ": " + a + ", want " + b); }
function angle(u, v) {
  var c = u[0] * v[0] + u[1] * v[1] + u[2] * v[2], x = [u[1] * v[2] - u[2] * v[1], u[2] * v[0] - u[0] * v[2], u[0] * v[1] - u[1] * v[0]];
  return Math.atan2(Math.hypot(x[0], x[1], x[2]), c);
}

(async function () {
  var THREE = await import(path.join(web, "vendor", "three.module.min.js"));

  // the bounds
  var D = V.view(), lo = { zoom: 1, yaw: -V.YAW, pitch: V.PITCH.min }, hi = { zoom: V.ZOOM_MAX, yaw: V.YAW, pitch: V.PITCH.max };
  assert.deepStrictEqual(D, { zoom: 1, yaw: 0, pitch: 0 }, "the default view is the designed frame");
  assert.deepStrictEqual([V.ZOOM_MAX, V.YAW, V.PITCH], [8, 60, { min: -25, max: 40 }], "zoom to 8, yaw 60 either way, pitch -25 to +40");
  assert.deepStrictEqual(V.clamp({ zoom: 0.5, yaw: -100, pitch: -90 }), lo, "under the bounds: held at them");
  assert.deepStrictEqual(V.clamp({ zoom: 20, yaw: 100, pitch: 90 }), hi, "over the bounds: held at them");
  assert.deepStrictEqual([V.clamp(lo), V.clamp(hi)], [lo, hi], "at the bounds: kept");
  assert.deepStrictEqual(V.clamp({ zoom: 2.5, yaw: -12, pitch: 7 }), { zoom: 2.5, yaw: -12, pitch: 7 }, "inside them: kept");
  assert.deepStrictEqual([V.clamp(null), V.clamp({ zoom: NaN, yaw: Infinity, pitch: "3" })], [D, D], "what is not a finite number is the default's");

  // the fov: the designed one at zoom 1, the half-angle's tangent over the zoom past it
  near(V.fov(50, 1), 50, 1e-12, "fov at zoom 1");
  near(Math.tan(V.fov(50, 8) * RAD / 2), Math.tan(25 * RAD) / 8, 1e-12, "fov at zoom 8: tan of its half a 1/8 of the designed one's");

  // the default view aims where LAND.camera does
  // the phone upright's 80 degrees put its bottom corners 40 degrees under its middle, past PITCH.min: a zoom there is held
  var frames = [{ name: "laptop", w: 1600, h: 900, held: false }, { name: "phone", w: 390, h: 844, held: true }], worst = 0, zoomed = {};
  frames.forEach(function (f) {
    var aspect = f.w / f.h, base = S.fov(aspect), v = L.camera(base), a = V.aim(v, D);
    assert.deepStrictEqual(a.eye, v.eye, f.name + ": the eye is the view's");
    for (var i = 0; i < 3; i++) near(a.at[i], v.at[i], 1e-9, f.name + ": the default aims at the view's point, axis " + i);
    var cam = new THREE.PerspectiveCamera(base, aspect, 0.3, 3000);
    // dir is three's direction under (px, py) at state: the camera VIEW.fov and VIEW.aim put
    function dir(s, px, py) {
      var p = V.aim(v, s);
      cam.fov = V.fov(base, s.zoom);
      cam.position.fromArray(p.eye);
      cam.lookAt(p.at[0], p.at[1], p.at[2]);
      cam.updateProjectionMatrix();
      cam.updateMatrixWorld(true);
      var q = new THREE.Vector3(2 * px / f.w - 1, 1 - 2 * py / f.h, 0.5).unproject(cam).sub(cam.position).normalize();
      return q.toArray();
    }
    var points = [[0, 0], [f.w, 0], [0, f.h], [f.w, f.h], [f.w * 0.3, f.h * 0.6], [f.w / 2, f.h / 2]];
    [D, { zoom: 3, yaw: 20, pitch: -10 }].forEach(function (s) {
      points.forEach(function (pt) {
        assert.ok(angle(V.ray(s, pt[0], pt[1], f.w, f.h, base, aspect, v.pitch), dir(s, pt[0], pt[1])) < 1e-9, f.name + ": VIEW.ray is three's ray at " + pt + " " + JSON.stringify(s));
      });
    });
    // a zoom toward a point keeps its direction, step by step to the most; zooming back out there comes home
    points.forEach(function (pt, n) {
      if (f.held && (n === 2 || n === 3)) {
        var low = V.zoomAt(D, V.ZOOM_MAX, pt[0], pt[1], f.w, f.h, base, aspect, v.pitch);
        assert.deepStrictEqual([low.zoom, low.pitch], [V.ZOOM_MAX, V.PITCH.min], f.name + ": a zoom toward " + pt + ", under the bound, is held at it");
        return;
      }
      keep(pt);
    });
    function keep(pt) {
      var s = D, before = dir(D, pt[0], pt[1]);
      for (var k = 0; k < 3; k++) {
        s = V.zoomAt(s, 2, pt[0], pt[1], f.w, f.h, base, aspect, v.pitch);
        var e = angle(dir(s, pt[0], pt[1]), before);
        worst = Math.max(worst, e);
        assert.ok(e < 1e-6, f.name + ": zoomed to " + s.zoom + " toward " + pt + ", the point's direction moved " + e + " rad");
        assert.deepStrictEqual(V.clamp(s), s, f.name + ": a zoom toward " + pt + " stays inside the bounds");
      }
      assert.strictEqual(s.zoom, V.ZOOM_MAX, f.name + ": three doublings is the most zoom");
      zoomed[f.name + " " + pt.map(Math.round)] = [s.yaw, s.pitch].map(function (x) { return Math.round(x * 10) / 10; });
      assert.deepStrictEqual(V.zoomAt(s, 2, pt[0], pt[1], f.w, f.h, base, aspect, v.pitch), s, f.name + ": a zoom past the most is no change");
      var home = V.zoomAt(s, 1 / V.ZOOM_MAX, pt[0], pt[1], f.w, f.h, base, aspect, v.pitch);
      assert.strictEqual(home.zoom, 1, f.name + ": zoomed back out");
      near(home.yaw, 0, 1e-9, f.name + ": back out toward " + pt + ", the yaw");
      near(home.pitch, 0, 1e-9, f.name + ": back out toward " + pt + ", the pitch");
    }
    assert.deepStrictEqual(V.zoomAt(D, 0.5, 0, 0, f.w, f.h, base, aspect, v.pitch), D, f.name + ": zooming out of the designed frame is no change");
  });

  // a drag: the frame's full height turns the pitch by the fov at zoom 1 and by the fov over the zoom at zoom 4
  var H = 900, F = 50, from = { zoom: 1, yaw: 0, pitch: V.PITCH.min };
  near(V.drag(from, 0, H, F, H).pitch, V.PITCH.min + F, 1e-9, "a drag down the frame's height at zoom 1 pitches up by the fov");
  near(V.drag({ zoom: 4, yaw: 0, pitch: V.PITCH.min }, 0, H, F, H).pitch, V.PITCH.min + F / 4, 1e-9, "at zoom 4, by the fov over 4");
  near(V.drag(D, H / 2, 0, F, H).yaw, -F / 2, 1e-9, "a drag right turns the view left, by the same degrees a pixel");
  assert.deepStrictEqual(V.drag(D, -1e6, -1e6, F, H), { zoom: 1, yaw: V.YAW, pitch: V.PITCH.min }, "a long drag is held at the bounds");
  // the ground follows the finger: the direction under the finger after a small drag is the one it started on, nearly
  (function () {
    var aspect = 16 / 9, base = S.fov(aspect), v = L.camera(base), x = 800, y = 600, s = { zoom: 4, yaw: 0, pitch: 0 };
    var d0 = V.ray(s, x, y, 1600, 900, base, aspect, v.pitch), d1 = V.ray(V.drag(s, 10, 6, base, 900), x + 10, y + 6, 1600, 900, base, aspect, v.pitch);
    assert.ok(angle(d0, d1) < 0.02 * RAD * 10, "the ground under the finger follows it: " + angle(d0, d1) / RAD + " deg");
  })();

  // the wheel and the pinch
  near(V.wheel(0, 0), 1, 0, "no scroll, no zoom");
  assert.ok(V.wheel(-100, 0) > 1 && V.wheel(100, 0) < 1, "scrolling up zooms in, down out");
  near(V.wheel(-100, 0) * V.wheel(100, 0), 1, 1e-12, "a scroll and its opposite cancel");
  near(V.wheel(-3, 1), V.wheel(-3 * V.LINE, 0), 1e-12, "a line is LINE px");
  near(V.wheel(1, 2), V.wheel(V.PAGE, 0), 1e-12, "a page is PAGE px");
  assert.ok(V.wheel(-100, 0) > 1.1 && V.wheel(-100, 0) < 1.4, "a wheel's notch is a modest step: " + V.wheel(-100, 0));
  assert.deepStrictEqual([V.pinch(100, 200), V.pinch(200, 100), V.pinch(0, 50), V.pinch(50, 0), V.pinch(NaN, 5)], [2, 0.5, 1, 1, 1], "the pinch's factor");

  // the address
  assert.deepStrictEqual(V.write(D), { z: null, look: null }, "the default view writes nothing");
  assert.strictEqual(S.query("?t=x&z=2&look=1,2", V.write(D)), "?t=x", "so SPLASH.query drops both");
  assert.deepStrictEqual(V.write({ zoom: 1.001, yaw: 0.004, pitch: -0.004 }), { z: null, look: null }, "nor a view that rounds to it");
  assert.deepStrictEqual(V.write({ zoom: 2.345678, yaw: 3.14159, pitch: -2.71828 }), { z: "2.35", look: "3.14,-2.72" }, "two decimals");
  assert.deepStrictEqual(V.write({ zoom: 3, yaw: 0, pitch: 0 }), { z: "3", look: null }, "a zoom alone writes z alone");
  [{ zoom: 2.35, yaw: 3.14, pitch: -2.72 }, { zoom: 8, yaw: -60, pitch: 40 }, { zoom: 1, yaw: 12.5, pitch: 0 }, { zoom: 1.5, yaw: 0, pitch: -25 }].forEach(function (s) {
    var q = S.query("?t=x&q=low", V.write(s));
    assert.deepStrictEqual(V.parse(q), s, "the address round-trips: " + q);
  });
  var rough = { zoom: 2.3456, yaw: -7.891, pitch: 10.006 };
  assert.deepStrictEqual(V.parse(S.query("", V.write(rough))), { zoom: 2.35, yaw: -7.89, pitch: 10.01 }, "to two decimals");
  assert.deepStrictEqual(V.parse("?look=10%2C-3.5&z=2"), { zoom: 2, yaw: 10, pitch: -3.5 }, "an escaped comma");
  assert.deepStrictEqual(V.parse("?z=100&look=999,-999"), { zoom: 8, yaw: 60, pitch: -25 }, "held to the bounds");
  ["", "?t=x", "?z=abc&look=1", "?z=&look=,", "?z=2x&look=1,2,3", "?z=0x10&look=1e2,3", "?look=%E0%A4%A"].forEach(function (q) {
    assert.deepStrictEqual(V.parse(q), D, "what it cannot read is the default view: " + JSON.stringify(q));
  });
  assert.deepStrictEqual(V.parse("?z=nope&look=5,6"), { zoom: 1, yaw: 5, pitch: 6 }, "each parameter read on its own");

  console.log(JSON.stringify({ worst: worst, zoomed: zoomed }));
})().catch(function (e) { console.error(e); process.exit(1); });
