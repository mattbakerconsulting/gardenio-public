/* VIEW: where the splash looks (plan gd-I5QHVfGLbD, step 1, gd-JwD56Df9nj), so a visitor can zoom into the scene and look
   round it. The view is {zoom, yaw, pitch}. zoom 1 is the designed frame, SPLASH.fov(aspect) degrees high; zoom z narrows
   that field of view by z (its half-angle's tangent over z), up to ZOOM_MAX, so the eye never moves and the land, the stone,
   the shadows and the pond's mirror stay as they are. yaw and pitch are degrees added to the aim LAND.camera(fov) gives:
   yaw turned about the eye, right positive (towards +x, the view looking north along -z), pitch up positive. They are held
   inside YAW and PITCH so the viewer never turns onto the unbuilt slope behind the camera or under the hill.

   The page's gestures come here as numbers: a wheel or a pinch is a zoom factor (wheel, pinch) that zoomAt spends on the
   point under the pointer, keeping that point under it; a drag turns the view so the ground follows the finger (drag). The
   view is written into the address as ?z=<zoom>&look=<yaw>,<pitch> (write, read back by parse), nothing at the default.
   All of it is pure, with no three.js: testdata/splash_view_test.js runs it under node. */
var VIEW = (function () {
  "use strict";
  var ZOOM_MAX = 8;
  // how far the view turns from the designed frame, in degrees: either way, and down and up
  var YAW = 60, PITCH = { min: -25, max: 40 };
  // a wheel's zoom: exp(-WHEEL a pixel scrolled), a line LINE px, a page PAGE px; a wheel's notch (100 px) is about 1.22x
  var WHEEL = 0.002, LINE = 16, PAGE = 800;
  var RAD = Math.PI / 180;

  function lim(x, lo, hi) { return x < lo ? lo : x > hi ? hi : x; }
  function fin(x, d) { return typeof x === "number" && isFinite(x) ? x : d; }

  /* view is the default: the designed frame. */
  function view() { return { zoom: 1, yaw: 0, pitch: 0 }; }

  /* fov is the vertical field of view in degrees at zoom for a designed frame of base degrees: base at zoom 1, the tangent of
     its half-angle divided by zoom past it, so everything in the middle of the frame looks zoom times as big. At zoom 1 it
     is base itself, not base through a tangent and back, so the default view's camera is the designed frame's. */
  function fov(base, zoom) { return !(zoom > 1) ? base : 2 * Math.atan(Math.tan(base * RAD / 2) / zoom) / RAD; }

  /* clamp is state held to the bounds, as a new view: zoom from 1 to ZOOM_MAX, yaw within YAW either way, pitch from
     PITCH.min to PITCH.max; a field that is missing or not a finite number is the default's. */
  function clamp(state) {
    state = state || {};
    return {
      zoom: lim(fin(state.zoom, 1), 1, ZOOM_MAX),
      yaw: lim(fin(state.yaw, 0), -YAW, YAW),
      pitch: lim(fin(state.pitch, 0), PITCH.min, PITCH.max)
    };
  }

  /* aim(view, state) -> {eye, at}: where the camera stands and the point it looks at, from LAND.camera's view {eye, at}. The
     eye is the view's; at is as far from it as the view's, yaw turned about the eye from the view's bearing and pitch added
     to the view's pitch. At the default state it is the view's own aim. */
  function aim(v, state) {
    var e = v.eye, d = [v.at[0] - e[0], v.at[1] - e[1], v.at[2] - e[2]], reach = Math.hypot(d[0], d[1], d[2]);
    var yaw = Math.atan2(d[0], -d[2]) + state.yaw * RAD, pitch = Math.asin(d[1] / reach) + state.pitch * RAD;
    var c = Math.cos(pitch);
    return { eye: e.slice(), at: [e[0] + reach * c * Math.sin(yaw), e[1] + reach * Math.sin(pitch), e[2] - reach * c * Math.cos(yaw)] };
  }

  // tans are the tangents (right, up) of the ray through CSS px (px, py) of a width x height frame at the base fov's zoom
  function tans(zoom, px, py, width, height, base, aspect) {
    var tv = Math.tan(fov(base, zoom) * RAD / 2);
    return [(2 * px / width - 1) * tv * (aspect || width / height), (1 - 2 * py / height) * tv];
  }

  /* ray is the unit direction [x, y, z] under CSS px (px, py) of a width x height frame at state, base the designed fov, aspect
     the frame's width over its height and pitch0 the view's own pitch (LAND.camera(base).pitch, degrees); x right, y up, -z
     the view's bearing, as three's camera has it after lookAt with +y up. */
  function ray(state, px, py, width, height, base, aspect, pitch0) {
    var t = tans(state.zoom, px, py, width, height, base, aspect), yaw = state.yaw * RAD, pitch = ((pitch0 || 0) + state.pitch) * RAD;
    var sy = Math.sin(yaw), cy = Math.cos(yaw), sp = Math.sin(pitch), cp = Math.cos(pitch);
    // forward, right and up of the camera, summed as forward + right * tan + up * tan
    var x = sy * cp + t[0] * cy - t[1] * sy * sp, y = sp + t[1] * cp, z = -cy * cp + t[0] * sy + t[1] * cy * sp, n = Math.hypot(x, y, z);
    return [x / n, y / n, z / n];
  }

  /* zoomAt is state zoomed by factor toward CSS px (px, py) of a width x height frame (base the designed fov, aspect the
     frame's width over its height, pitch0 the view's own pitch as ray takes it): the zoom times factor, held to the bounds,
     and the yaw and pitch that keep the direction under (px, py) what it was, so the point under the pointer stays under
     it. Where that yaw or pitch is past a bound it is held there and the point slides. A zoom already at its bound is state
     unchanged. */
  function zoomAt(state, factor, px, py, width, height, base, aspect, pitch0) {
    var s = clamp(state), zoom = lim(s.zoom * fin(factor, 1), 1, ZOOM_MAX);
    if (zoom === s.zoom) return s;
    var d = ray(s, px, py, width, height, base, aspect, pitch0), t = tans(zoom, px, py, width, height, base, aspect);
    // the pitch whose ray through (a, b) rises as d does: b cos p + sin p = sin(rise) * |(a, b, 1)|
    var a = t[0], b = t[1], k = d[1] * Math.hypot(a, b, 1) / Math.hypot(b, 1);
    var pitch = Math.asin(lim(k, -1, 1)) - Math.atan(b);
    // then the yaw that puts the ray's bearing on d's: the ray lies atan2(a, cos p - b sin p) right of the camera's
    var yaw = Math.atan2(d[0], -d[2]) - Math.atan2(a, Math.cos(pitch) - b * Math.sin(pitch));
    yaw = Math.atan2(Math.sin(yaw), Math.cos(yaw));
    return clamp({ zoom: zoom, yaw: yaw / RAD, pitch: pitch / RAD - (pitch0 || 0) });
  }

  /* drag is state turned by a drag of dx, dy CSS px over a frame height px high at the designed fov, so the ground follows
     the finger: a drag right turns the view left, a drag down pitches it up, by fov / height / zoom degrees a pixel (a drag
     the frame's height turns it by the fov at zoom 1). Held to the bounds. */
  function drag(state, dx, dy, base, height) {
    var s = clamp(state), k = base / Math.max(1, height) / s.zoom;
    return clamp({ zoom: s.zoom, yaw: s.yaw - fin(dx, 0) * k, pitch: s.pitch + fin(dy, 0) * k });
  }

  /* wheel is the zoom factor of a wheel event's deltaY in its deltaMode (0 pixels, 1 lines, 2 pages): exp(-WHEEL x pixels),
     a line LINE px and a page PAGE px; scrolling up (deltaY < 0) zooms in, and a scroll and its opposite cancel. */
  function wheel(deltaY, deltaMode) {
    return Math.exp(-WHEEL * fin(deltaY, 0) * (deltaMode === 1 ? LINE : deltaMode === 2 ? PAGE : 1));
  }

  /* pinch is the zoom factor of two fingers moved from d0 px apart to d1: d1 / d0, 1 when either is not a positive distance. */
  function pinch(d0, d1) { return fin(d0, 0) > 0 && fin(d1, 0) > 0 ? d1 / d0 : 1; }

  // num reads a plain decimal number, NaN for anything else
  function num(v) { return /^[+-]?(\d+\.?\d*|\.\d+)$/.test(v) ? +v : NaN; }
  function param(search, k) {
    var m = new RegExp("[?&]" + k + "=([^&#]*)").exec(search || "");
    try { return m ? decodeURIComponent(m[1]) : ""; } catch (e) { return ""; }
  }

  /* parse reads the view from a location.search: ?z=<zoom>&look=<yaw>,<pitch>, held to the bounds. A parameter it cannot
     read (missing, not a number, a look without both numbers) is the default's. */
  function parse(search) {
    var look = param(search, "look").split(",").map(num);
    if (look.length !== 2 || look.some(isNaN)) look = [0, 0];
    return clamp({ zoom: num(param(search, "z")), yaw: look[0], pitch: look[1] });
  }

  // two rounds to two decimals
  function two(x) { return Math.round(x * 100) / 100 || 0; }

  /* write is the address's parameters for state, held to the bounds, as SPLASH.query takes them: {z: "2.5", look:
     "10.25,-3.5"}, each to two decimals, and z null at zoom 1 and look null at yaw and pitch 0 (after rounding), so the
     default view's address carries neither. */
  function write(state) {
    var s = clamp(state), z = two(s.zoom), y = two(s.yaw), p = two(s.pitch);
    return { z: z === 1 ? null : String(z), look: y === 0 && p === 0 ? null : y + "," + p };
  }

  return { ZOOM_MAX: ZOOM_MAX, YAW: YAW, PITCH: PITCH, WHEEL: WHEEL, LINE: LINE, PAGE: PAGE,
    view: view, fov: fov, clamp: clamp, aim: aim, ray: ray, zoomAt: zoomAt, drag: drag, wheel: wheel, pinch: pinch, parse: parse, write: write };
})();
if (typeof module !== "undefined") module.exports = VIEW;
