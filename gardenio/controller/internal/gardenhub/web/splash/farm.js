/* FARM: the farmstead at the valley's near edge, where the lane arrives (plan gd-EJGA3zDGCk, step 6). A stone farmhouse
   with a slate roof, two dormers, a chimney at each gable and its windows; a timber barn on a stone plinth, its doors open
   on the gable that faces the lane and a hayloft over them, a lamp on its bracket; a dry-stone wall either side of the
   lane where it comes into the yard, and a timber gate swung back off it; a well and a cart in the yard. Round bales lie
   in the cut hay strips of the fields above the farm, the pasture strips beside them are fenced, with cattle and sheep
   in them that stand and graze; a wooden footbridge crosses the river beside the lane's ford; and the pond west of the
   yard holds the sky and the trees in a planar reflection (a Reflector's pass at half the canvas's resolution, mirror()),
   or at the poorer quality tiers (step 9) a tinted mirror colour that follows the sky, or only its own water, lit.

   Life by the clock (hours()): the windows and the barn lamp are lit from civil dusk to civil dawn (the sun 6 degrees
   down, isLit), the chimney smokes, a soft ribbon of puffs drifting downwind, from an hour before sunset until sunrise
   (smoking), swifts cross the yard while the sun is up and bats once it is dusk.

   Where things are is pure and seeded, like LAND and FLORA: layout() places every building, window, wall stone, bale,
   post, beast and bird with only + - * /, Math.floor, Math.sqrt and Math.imul on the land's drawn surface, so it is the
   same everywhere; TestFarmUnderNode pins it. The buildings face the ways their frames give as unit vectors, never as
   angles. The textures (coursed stone, slate, weathered boards) are made in code as pixels when there is a document;
   under node the materials are plain. update(group, seconds) moves the smoke, the grazing heads and the birds. */
var FARM = (function () {
  "use strict";
  var L = typeof LAND !== "undefined" ? LAND : require("./land.js");
  var FL = typeof FLORA !== "undefined" ? FLORA : require("./flora.js");
  var A = typeof ALMANAC !== "undefined" ? ALMANAC : require("../almanac.js");
  var SEED = 20261028;
  // the clock: civil dusk and dawn (the sun 6 degrees down), sunset and sunrise (its upper limb on the horizon), an hour
  var CIVIL = -6, SET = -0.833, HOUR = 3600000;
  // the buildings: where each stands, the way its front looks (a ground vector, x east and z south), its length along
  // the ridge, its depth, the height of its eaves and of its ridge (m), and its door (u along, w out), whose ground its floor
  // is level with: both stand on the slope up from the yard, their backs cut into it
  var HOUSE = { x: -37, z: -55, face: [-1, 3], len: 10.5, depth: 6.4, eaves: 5.4, ridge: 8.3, door: [0.1, 3.2] };
  var BARN = { x: -18, z: -51, face: [-2, 1], len: 12, depth: 8.5, eaves: 4.4, ridge: 8.4, door: [6, 0] };
  var WELL = { x: -33, z: -45.5 }, CART = { x: -24, z: -47 };
  var GATE_Z = -38.6, GATE = 3.6; // where the gate stands across the lane's way into the yard, and the gap it hangs in
  var WALL_W = [[-31, -39.8], [-36, -40.8], [-41, -42], [-45, -43.9], [-47.2, -46.6]], WALL_E = [[-17, -38.9], [-12.6, -40.6], [-10.2, -43.6]];
  var WALL = { h: 1.15, base: 0.72, top: 0.42, step: 0.5, cope: 0.24 };
  var BALE = { r: 0.75, w: 1.2, every: 7, max: 90, reach: 150 }; // a round bale's radius and width, one every m along a windrow
  var PADDOCKS = 3, POST = 2.4, COWS = 4, SHEEP = 7; // the fenced pasture strips nearest the yard, their posts' spacing, the beasts in each
  var SWIFTS = 10, BATS = 7, PUFFS = 90; // the swifts over the yard by day, the bats by the barn after dusk, the puffs a chimney's ribbon holds
  var REFLECT = 1; // the layer the pond's mirror camera sees (splash.js puts on it all that the pond can show)
  var GLOW = 0xffb25a; // a lit window's and the lamp's colour
  // the buildings' meshes by material, named apart from the stone on the brow and the rest of the scene
  var NAMES = { stone: "farm stone", wall: "dry wall", slate: "farm slate", timber: "farm timber", paint: "farm paint", glass: "farm glass", dark: "farm dark", hay: "farm hay", lamp: "lamp", lit: "lit windows" };

  function clamp(x, lo, hi) { return x < lo ? lo : x > hi ? hi : x; }
  function smooth(e0, e1, x) { var k = clamp((x - e0) / (e1 - e0), 0, 1); return k * k * (3 - 2 * k); }
  function lerp(a, b, t) { return a + (b - a) * t; }
  function rnd(seed) { return function (i, k) { return L.hash(i, k, seed); }; }
  function unit2(x, z) { var l = Math.sqrt(x * x + z * z) || 1; return [x / l, z / l]; }

  /* ---- the clock ---- */

  /* isLit says whether the windows and the barn lamp are lit at t (unix ms) seen from lat, lon (ALMANAC's defaults without
     them): from civil dusk, the sun 6 degrees under the horizon, to civil dawn. */
  function isLit(t, lat, lon) { return A.sun(t, lat, lon).elevation < CIVIL; }
  /* smoking says whether the chimney smokes at t: from an hour before sunset, through the night, until sunrise. */
  function smoking(t, lat, lon) { return A.sun(t, lat, lon).elevation < SET || A.sun(t + HOUR, lat, lon).elevation < SET; }
  /* hours(t, lat, lon) -> {lit, smoke, swifts, bats}: what the farm does at t. Swifts fly while the sun is up, bats from
     civil dusk to civil dawn. */
  function hours(t, lat, lon) {
    var el = A.sun(t, lat, lon).elevation;
    return { lit: el < CIVIL, smoke: smoking(t, lat, lon), swifts: el >= SET, bats: el < CIVIL };
  }

  /* ---- frames on the ground ---- */

  /* frame(x, z, fx, fz, y) is a building's frame: its origin on the ground at x, z (y, else the drawn ground there), its
     front f (local +w) the unit of fx, fz and its right r = (fz, -fx) (local +u), up y. */
  function frame(x, z, fx, fz, y) { var f = unit2(fx, fz); return { x: x, y: y == null ? FL.ground(x, z) : y, z: z, fx: f[0], fz: f[1] }; }
  /* P is the world point at u right, y up and w ahead in frame fr. */
  function P(fr, u, y, w) { return [fr.x + u * fr.fz + w * fr.fx, fr.y + y, fr.z - u * fr.fx + w * fr.fz]; }
  /* sub is the frame at u, y, w of fr facing du right and dw ahead of it. */
  function sub(fr, u, y, w, du, dw) { var p = P(fr, u, y, w); return frame(p[0], p[2], du * fr.fz + dw * fr.fx, -du * fr.fx + dw * fr.fz, p[1]); }

  /* building(b) -> the frame of b, its floor level with the ground at its door, its walls carried down to the lowest ground
     under its corners, and its corners on the ground. */
  function building(b) {
    var fr = frame(b.x, b.z, b.face[0], b.face[1]), lo = fr.y, corners = [], door = P(fr, b.door[0], 0, b.door[1]);
    [[-1, -1], [1, -1], [1, 1], [-1, 1]].forEach(function (s) {
      var p = P(fr, s[0] * b.len / 2, 0, s[1] * b.depth / 2), g = FL.ground(p[0], p[2]);
      corners.push([p[0], p[2]]);
      lo = Math.min(lo, g);
    });
    fr.y = FL.ground(door[0], door[2]);
    lo = Math.min(lo, fr.y);
    return { frame: fr, low: lo - 0.3, corners: corners, len: b.len, depth: b.depth, eaves: b.eaves, ridge: b.ridge };
  }

  /* roofY is the height of a ridge-along-u roof's slope at w (the eaves at w = +-depth/2, the ridge at 0). */
  function roofY(b, w) { return b.eaves + (b.ridge - b.eaves) * (1 - Math.abs(w) / (b.depth / 2)); }

  /* windows(house, barn) -> [{at, out, w, h, on, kind}]: every window's centre a hair proud of its wall, the way it looks,
     its size, whether it is lit at night (most are) and on what. The house: four down and three up on its front, a window
     in each dormer and one in each storey of each gable; the barn: a small one high in each long side. */
  function windows(h, b) {
    var r = rnd(SEED + 1), out = [], n = 0;
    function win(fr, u, y, w, du, dw, ww, hh, kind) {
      var c = P(fr, u + du * 0.03, y, w + dw * 0.03), o = [du * fr.fz + dw * fr.fx, -du * fr.fx + dw * fr.fz];
      out.push({ at: c, out: o, w: ww, h: hh, on: r(n++, 1) < 0.8 ? 1 : 0, kind: kind });
    }
    var f = h.frame, d2 = h.depth / 2, l2 = h.len / 2;
    [-3.8, -1.7, 1.9, 3.9].forEach(function (u) { win(f, u, 1.55, d2, 0, 1, 0.9, 1.25, "house"); });
    [-3.8, 0.1, 3.9].forEach(function (u) { win(f, u, 4.0, d2, 0, 1, 0.9, 1.1, "house"); });
    [-2.2, 2.2].forEach(function (u) { win(f, u, 6.95, d2 - 1.0, 0, 1, 0.8, 0.85, "dormer"); });
    [-1, 1].forEach(function (s) { win(f, s * l2, 1.55, -0.6, s, 0, 0.8, 1.1, "house"); win(f, s * l2, 4.0, 0.7, s, 0, 0.8, 1.0, "house"); });
    var g = b.frame;
    [-1, 1].forEach(function (s) { [-3, 2].forEach(function (u) { win(g, u, 3.3, s * b.depth / 2, 0, s, 0.7, 0.55, "barn"); }); });
    return out;
  }

  /* chimneys(house) -> the top of each chimney's pots, where the smoke leaves, the east one first. */
  function chimneys(h) { return [1, -1].map(function (s) { return P(h.frame, s * (h.len / 2 - 0.45), h.ridge + 1.75, 0); }); }

  /* ---- the lane, the walls and the gate ---- */

  var laneLine = null;
  function lane() { if (!laneLine) laneLine = FL.curve(L.LANE, 0.5); return laneLine; }
  /* crossing finds where the lane's line first crosses z = c going north (toward the yard), with its heading [x, z]. */
  function crossing(c) {
    var pl = lane();
    for (var i = 0; i + 1 < pl.length; i++) {
      var a = pl[i], b = pl[i + 1];
      if ((a[1] - c) * (b[1] - c) <= 0 && a[1] !== b[1]) {
        var t = (c - a[1]) / (b[1] - a[1]);
        return { x: lerp(a[0], b[0], t), z: c, dir: unit2(b[0] - a[0], b[1] - a[1]) };
      }
    }
    return null;
  }

  /* gate() -> {lane, posts: [[x, y, z] x2], leaf: [x, z] the way the open gate's leaf runs, len}: the gate's posts either
     side of the lane where it comes into the yard, the west one first, its leaf hung on the west post and swung back into
     the yard. */
  function gate() {
    var c = crossing(GATE_Z), side = [c.dir[1], -c.dir[0]]; // the lane runs north into the yard; its left hand is west
    var a = [c.x + side[0] * GATE / 2, c.z + side[1] * GATE / 2], b = [c.x - side[0] * GATE / 2, c.z - side[1] * GATE / 2];
    var leaf = unit2(-side[0] * 0.34 + c.dir[0] * 0.94, -side[1] * 0.34 + c.dir[1] * 0.94); // open about 70 degrees, into the yard
    return { lane: [c.x, c.z], posts: [[a[0], FL.ground(a[0], a[1]), a[1]], [b[0], FL.ground(b[0], b[1]), b[1]]], leaf: leaf, len: GATE - 0.25 };
  }

  /* walls() -> [[x, y, z, ...]]: the two runs of dry-stone wall, one from each gate post outward along the yard's southern
     rim, a point every WALL.step m on the drawn ground. */
  function walls(gt) {
    return [[gt.posts[0]].concat(WALL_W), [gt.posts[1]].concat(WALL_E)].map(function (pts) {
      var out = [];
      for (var i = 0; i + 1 < pts.length; i++) {
        var a = pts[i], b = pts[i + 1], ax = a[0], az = a.length === 3 ? a[2] : a[1], bx = b[0], bz = b[1];
        var len = Math.sqrt((bx - ax) * (bx - ax) + (bz - az) * (bz - az)), m = Math.max(1, Math.ceil(len / WALL.step));
        for (var k = i ? 1 : 0; k <= m; k++) { var x = lerp(ax, bx, k / m), z = lerp(az, bz, k / m); out.push(x, FL.ground(x, z), z); }
      }
      return out;
    });
  }

  /* coping(runs) -> the upright stones along the walls' tops, 6 numbers each: x, y, z, the run's heading x, z, and a lean. */
  function coping(runs) {
    var r = rnd(SEED + 2), out = [], n = 0;
    runs.forEach(function (w) {
      for (var i = 0; i + 3 < w.length; i += 3) {
        var dx = w[i + 3] - w[i], dz = w[i + 5] - w[i + 2], len = Math.sqrt(dx * dx + dz * dz), m = Math.max(1, Math.round(len / WALL.cope));
        for (var k = 0; k < m; k++, n++) {
          var t = (k + 0.5) / m;
          out.push(w[i] + dx * t, lerp(w[i + 1], w[i + 4], t) + WALL.h, w[i + 2] + dz * t, dx / len, dz / len, (r(n, 1) - 0.5) * 0.3);
        }
      }
    });
    return out;
  }

  /* ---- the fields: bales in the hay, fences round the pasture ---- */

  /* strips(f) -> the field's strips as LAND's painter cuts them: [{lo, hi, crop}] across its long side (along says which). */
  function strips(f) {
    var xs = f.quad.map(function (p) { return p[0]; }), zs = f.quad.map(function (p) { return p[1]; });
    var x0 = Math.min.apply(null, xs) - 2, x1 = Math.max.apply(null, xs) + 2, z0 = Math.min.apply(null, zs) - 2, z1 = Math.max.apply(null, zs) + 2;
    var lo = f.along === "z" ? x0 : z0, hi = f.along === "z" ? x1 : z1, out = [], n = 0;
    for (var a = lo; a < hi; a += f.strip, n++) out.push({ lo: a, hi: Math.min(hi, a + f.strip), crop: f.crops[n % 2] });
    return out;
  }
  /* clip cuts the convex polygon pts (of [x, z]) to the band lo <= p[k] <= hi, k 0 for x and 1 for z. */
  function clip(pts, k, lo, hi) {
    function cut(poly, keep, edge) {
      var out = [];
      for (var i = 0; i < poly.length; i++) {
        var a = poly[i], b = poly[(i + 1) % poly.length], ia = keep(a), ib = keep(b);
        if (ia) out.push(a);
        if (ia !== ib) { var t = (edge - a[k]) / (b[k] - a[k]); out.push([lerp(a[0], b[0], t), lerp(a[1], b[1], t)]); }
      }
      return out;
    }
    var p = cut(pts, function (q) { return q[k] >= lo; }, lo);
    return p.length ? cut(p, function (q) { return q[k] <= hi; }, hi) : p;
  }
  /* inside says whether x, z is in the polygon pts more than m from each edge (pts convex, either winding). */
  function inside(pts, x, z, m) {
    var sign = 0;
    for (var i = 0; i < pts.length; i++) {
      var a = pts[i], b = pts[(i + 1) % pts.length], ex = b[0] - a[0], ez = b[1] - a[1], l = Math.sqrt(ex * ex + ez * ez) || 1;
      var c = (ex * (z - a[1]) - ez * (x - a[0])) / l;
      if (Math.abs(c) < m) return false;
      if (sign === 0) sign = c > 0 ? 1 : -1;
      else if ((c > 0 ? 1 : -1) !== sign) return false;
    }
    return true;
  }
  function centroid(pts) { var x = 0, z = 0; pts.forEach(function (p) { x += p[0]; z += p[1]; }); return [x / pts.length, z / pts.length]; }
  function far(x, z) { return Math.sqrt((x - L.FARM.x) * (x - L.FARM.x) + (z - L.FARM.z) * (z - L.FARM.z)); }

  /* bales() -> Float32Array, 7 numbers a bale: x, y, z of its middle, its axis x, z (across the windrow, as a baler drops
     them), its radius, its width. Along the middle of every cut hay strip within reach of the yard where the eye sees
     them, BALE.every m apart and now and then a pair. */
  function bales() {
    var r = rnd(SEED + 3), out = [], n = 0;
    L.fields().forEach(function (f) {
      if (f.kind !== "field" || f.crops.indexOf("hay") < 0) return;
      strips(f).forEach(function (s) {
        if (s.crop !== "hay") return;
        var poly = clip(f.quad, f.along === "z" ? 0 : 1, s.lo, s.hi);
        if (poly.length < 3) return;
        var c = centroid(poly), mid = (s.lo + s.hi) / 2, zs = poly.map(function (p) { return f.along === "z" ? p[1] : p[0]; });
        if (far(f.along === "z" ? mid : c[0], f.along === "z" ? c[1] : mid) > BALE.reach) return;
        for (var a = Math.min.apply(null, zs) + r(n, 1) * BALE.every; a < Math.max.apply(null, zs); a += BALE.every * (0.7 + 0.6 * r(n, 2))) {
          for (var pair = 0; pair < (r(n, 3) < 0.25 ? 2 : 1); pair++, n++) {
            var off = (r(n, 4) - 0.5) * 1.6 + pair * 1.9, x = f.along === "z" ? mid + off : a, z = f.along === "z" ? a : mid + off;
            if (!inside(poly, x, z, 1.4) || out.length / 7 >= BALE.max) continue;
            var rad = BALE.r * (0.94 + 0.12 * r(n, 6)), y = FL.ground(x, z) + rad - 0.06; // settled a little into the stubble
            if (!FL.seen(x, y + rad, z)) continue;
            var ax = f.along === "z" ? 1 : 0, az = f.along === "z" ? 0 : 1, tw = (r(n, 5) - 0.5) * 0.5; // a little turned
            var axis = unit2(ax + az * tw, az - ax * tw);
            out.push(x, y, z, axis[0], axis[1], rad, BALE.w * (0.95 + 0.1 * r(n, 7)));
          }
        }
      });
    });
    return new Float32Array(out);
  }

  /* paddocks() -> [{ring, kind}]: the PADDOCKS pasture strips nearest the yard that the eye can see, each a ring of [x, z]
     a little inside its strip (where its fence runs) and what grazes it, cattle and sheep by turns. */
  function paddocks() {
    var all = [];
    L.fields().forEach(function (f) {
      if (f.kind !== "field" || f.crops.indexOf("pasture") < 0) return;
      strips(f).forEach(function (s) {
        if (s.crop !== "pasture") return;
        var poly = clip(f.quad, f.along === "z" ? 0 : 1, s.lo, s.hi);
        if (poly.length < 3) return;
        var c = centroid(poly), area = 0;
        for (var i = 0; i < poly.length; i++) { var a = poly[i], b = poly[(i + 1) % poly.length]; area += a[0] * b[1] - b[0] * a[1]; }
        if (Math.abs(area) / 2 < 120 || !FL.seen(c[0], FL.ground(c[0], c[1]) + 1, c[1])) return;
        // the fence a little inside the strip, its corners pulled toward the middle
        var ring = poly.map(function (p) { var dx = c[0] - p[0], dz = c[1] - p[1], l = Math.sqrt(dx * dx + dz * dz) || 1; return [p[0] + dx / l * 0.9, p[1] + dz / l * 0.9]; });
        all.push({ ring: ring, d: far(c[0], c[1]) });
      });
    });
    all.sort(function (a, b) { return a.d - b.d; });
    return all.slice(0, PADDOCKS).map(function (p, i) { return { ring: p.ring, kind: i % 2 ? "sheep" : "cattle" }; });
  }

  /* posts(rings) -> Float32Array, 3 numbers a post: its foot on the ground, every POST m or less round each ring. */
  function posts(rings) {
    var out = [];
    rings.forEach(function (ring) {
      for (var i = 0; i < ring.length; i++) {
        var a = ring[i], b = ring[(i + 1) % ring.length], len = Math.sqrt((b[0] - a[0]) * (b[0] - a[0]) + (b[1] - a[1]) * (b[1] - a[1]));
        var m = Math.max(1, Math.ceil(len / POST));
        for (var k = 0; k < m; k++) { var x = lerp(a[0], b[0], k / m), z = lerp(a[1], b[1], k / m); out.push(x, FL.ground(x, z), z); }
      }
    });
    return new Float32Array(out);
  }

  /* beasts(pads) -> Float32Array, 7 numbers a beast: x, y, z of its feet, its heading x, z, its kind (0 a cow, 1 a sheep)
     and its grazing phase: COWS or SHEEP inside each paddock, a little apart, where the eye can see them. */
  function beasts(pads) {
    var r = rnd(SEED + 4), out = [], n = 0;
    pads.forEach(function (p) {
      var want = p.kind === "cattle" ? COWS : SHEEP, got = 0, xs = p.ring.map(function (q) { return q[0]; }), zs = p.ring.map(function (q) { return q[1]; });
      var x0 = Math.min.apply(null, xs), x1 = Math.max.apply(null, xs), z0 = Math.min.apply(null, zs), z1 = Math.max.apply(null, zs), mine = [];
      for (var tries = 0; got < want && tries < 400; tries++, n++) {
        var x = lerp(x0, x1, r(n, 1)), z = lerp(z0, z1, r(n, 2));
        if (!inside(p.ring, x, z, 1.6)) continue;
        if (mine.some(function (q) { return (q[0] - x) * (q[0] - x) + (q[1] - z) * (q[1] - z) < 6.25; })) continue;
        var y = FL.ground(x, z);
        if (!FL.seen(x, y + 1, z)) continue;
        var hd = unit2(r(n, 3) * 2 - 1, r(n, 4) * 2 - 1);
        out.push(x, y, z, hd[0], hd[1], p.kind === "cattle" ? 0 : 1, r(n, 5));
        mine.push([x, z]); got++;
      }
    });
    return new Float32Array(out);
  }

  /* bridge() -> {at, dir, len, y0, y1, rise, planks}: the footbridge beside the lane's ford, from bank to bank north to
     south over the river, its deck's ends a little over the ground at the banks and arched between. */
  function bridge() {
    var pl = lane(), best = null;
    for (var i = 0; i + 1 < pl.length && !best; i++) {
      var a = pl[i], b = pl[i + 1], da = a[1] - L.river(a[0]), db = b[1] - L.river(b[0]);
      if (da * db <= 0) { var t = da / (da - db); best = [lerp(a[0], b[0], t), lerp(a[1], b[1], t)]; }
    }
    var x = best[0] + 4.5, zc = L.river(x), len = 11, n = Math.round(len / 0.26);
    var y0 = FL.ground(x, zc - len / 2) + 0.25, y1 = FL.ground(x, zc + len / 2) + 0.25;
    return { at: [x, zc], dir: [0, 1], len: len, y0: y0, y1: y1, rise: 0.55, planks: n, width: 1.3 };
  }
  /* deckY is the bridge's deck height at s in [0, 1] from its north end. */
  function deckY(br, s) { return lerp(br.y0, br.y1, s) + br.rise * 4 * s * (1 - s); }

  /* birds(n, seed, cx, cz, r0, r1, h0, h1, w0, w1) -> Float32Array, 8 numbers a bird: the centre x, z of its loop, its
     radii across and along, its height over the ground, its angular speed (rad/s, the sign its way round), its phase and
     its wingbeat phase. */
  function birds(n, seed, cx, cz, r0, r1, h0, h1, w0, w1) {
    var r = rnd(seed), out = [];
    for (var i = 0; i < n; i++) {
      var x = cx + (r(i, 1) - 0.5) * r0, z = cz + (r(i, 2) - 0.5) * r0;
      out.push(x, z, lerp(r0, r1, r(i, 3)), lerp(r0, r1, r(i, 4)) * 0.6, FL.ground(x, z) + lerp(h0, h1, r(i, 5)), lerp(w0, w1, r(i, 6)) * (i % 2 ? 1 : -1), r(i, 7) * 6.2831853, r(i, 8));
    }
    return new Float32Array(out);
  }

  var memo = null;
  /* layout() -> everything placed: the house and the barn (frames, corners, windows, chimneys), the gate, the walls and their
     coping, the well and the cart, the bales, the paddocks with their posts and beasts, the bridge, the pond's water and
     the swifts' and the bats' loops. Pure: the same numbers on every engine. */
  function layout() {
    if (memo) return memo;
    var h = building(HOUSE), b = building(BARN), gt = gate(), runs = walls(gt), pads = paddocks();
    var well = frame(WELL.x, WELL.z, 1, 2), cart = frame(CART.x, CART.z, -3, -1);
    var pond = { x: L.POND.x, z: L.POND.z, r: L.POND.r, y: L.pondLevel() - 0.2 }; // under the levelled floor, so the bank hides its edge where the drawn ground sags
    memo = {
      house: h, barn: b, windows: windows(h, b), chimneys: chimneys(h), gate: gt, walls: runs, coping: coping(runs),
      well: well, cart: cart, bales: bales(), paddocks: pads, posts: posts(pads.map(function (p) { return p.ring; })), beasts: beasts(pads),
      bridge: bridge(), pond: pond,
      swifts: birds(SWIFTS, SEED + 5, L.FARM.x, L.FARM.z, 14, 32, 7, 22, 0.45, 0.8),
      bats: birds(BATS, SEED + 6, BARN.x, BARN.z, 4, 11, 2.5, 7, 1.1, 1.9)
    };
    return memo;
  }

  /* ---- the pictures, made in code ---- */

  /* stonePixels(n, base, gap, seed) -> RGBA bytes of a tiling n x n patch of coursed stone two metres square: courses of
     stones of many lengths, each its own shade of base [r, g, b], rounded at its edges and parted by dark joints gap m wide.
     A dry wall's joints are deep and dark; a house's are pointed and pale. */
  function stonePixels(n, base, gap, joint, seed) {
    var r = rnd(seed), px = new Uint8Array(n * n * 4), C = 9, hs = [], sum = 0, edges = [0], joints = [];
    for (var k = 0; k < C; k++) { hs.push(0.65 + 0.7 * r(k, 1)); sum += hs[k]; }
    for (k = 0; k < C; k++) edges.push(edges[k] + hs[k] / sum);
    for (k = 0; k < C; k++) { var js = [], u = r(k, 2) * 0.2, m = 0; while (u < 0.999) { js.push(u); u += 0.1 + 0.17 * r(k, 10 + m++); } joints.push(js); }
    for (var j = 0; j < n; j++) {
      var v = (j + 0.5) / n, c = 0;
      while (c < C - 1 && v >= edges[c + 1]) c++;
      var dv = Math.min(v - edges[c], edges[c + 1] - v) * 2;
      for (var i = 0; i < n; i++) {
        var uu = (i + 0.5) / n, jl = joints[c], s = jl.length - 1, du = 1;
        for (var q = 0; q < jl.length; q++) {
          if (jl[q] <= uu) s = q;
          var d = Math.abs(uu - jl[q]); du = Math.min(du, d, 1 - d);
        }
        var edge = Math.min(du * 2, dv), shade = 0.78 + 0.32 * L.hash(c, s, seed + 7), grain = L.noise(uu * 48, v * 48, seed + 8, 48) * 0.18 + L.hash(i, j, seed + 9) * 0.08;
        var round = smooth(gap, gap + 0.05, edge), t = edge < gap ? joint : (0.82 + 0.18 * round) * shade * (0.9 + grain);
        var lich = L.noise(uu * 9, v * 9, seed + 10, 9) > 0.78 && edge >= gap ? 0.12 : 0, k4 = (j * n + i) * 4;
        px[k4] = Math.round(clamp(base[0] * t * (1 - lich * 0.3) + lich * 60, 0, 255));
        px[k4 + 1] = Math.round(clamp(base[1] * t + lich * 40, 0, 255));
        px[k4 + 2] = Math.round(clamp(base[2] * t * (1 - lich), 0, 255));
        px[k4 + 3] = 255;
      }
    }
    return px;
  }
  /* slatePixels(n, seed) -> RGBA bytes of a tiling metre of slate roof: five courses of slates a quarter metre wide, each
     course's joints halfway along the last's, every slate its own blue-grey, lighter toward its tail, a dark line of
     shadow under each course's edge and a few spots of moss. */
  function slatePixels(n, seed) {
    var px = new Uint8Array(n * n * 4), ROWS = 5, PER = 4;
    for (var j = 0; j < n; j++) {
      var v = (j + 0.5) / n * ROWS, row = Math.floor(v), fv = v - row;
      for (var i = 0; i < n; i++) {
        var u = (i + 0.5) / n * PER + (row % 2) * 0.5, col = Math.floor(u), fu = u - col, slate = ((col % PER) + PER) % PER;
        var shade = 0.8 + 0.28 * L.hash(slate, row, seed), purple = L.hash(slate, row, seed + 1) * 0.08;
        var t = shade * (0.88 + 0.16 * fv) * (fv < 0.07 ? 0.45 : 1) * (fu < 0.025 || fu > 0.975 ? 0.55 : 1) * (0.94 + 0.1 * L.noise((i / n) * 64, (j / n) * 16, seed + 2, 64, 16));
        var moss = L.noise((i / n) * 6, (j / n) * 6, seed + 3, 6) > 0.8 && fv > 0.5 ? 0.35 : 0, k4 = (j * n + i) * 4;
        px[k4] = Math.round(clamp(lerp(76 * t * (1 + purple), 92, moss), 0, 255));
        px[k4 + 1] = Math.round(clamp(lerp(82 * t, 104, moss), 0, 255));
        px[k4 + 2] = Math.round(clamp(lerp(94 * t * (1 + purple * 0.5), 52, moss), 0, 255));
        px[k4 + 3] = 255;
      }
    }
    return px;
  }
  /* timberPixels(n, seed) -> RGBA bytes of two tiling metres of weathered weatherboard: eight upright boards, each its own
     silvered brown, the grain running up it in streaks, dark gaps between and a knot here and there. */
  function timberPixels(n, seed) {
    var px = new Uint8Array(n * n * 4), B = 8;
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) {
      var u = (i + 0.5) / n * B, board = Math.floor(u), fu = u - board, v = (j + 0.5) / n;
      var shade = 0.72 + 0.36 * L.hash(board, 1, seed), silver = L.hash(board, 2, seed);
      var grain = L.noise(u * 9 + L.noise(u * 2, v * 3, seed + 1, B * 2, 3) * 2, v * 3, seed + 2, B * 9, 3);
      var kx = L.hash(board, 3, seed), ky = L.hash(board, 4, seed), kd = Math.sqrt((fu - 0.3 - kx * 0.4) * (fu - 0.3 - kx * 0.4) * 9 + (v - ky) * (v - ky) * 400);
      var t = shade * (0.78 + 0.34 * grain) * (fu < 0.05 || fu > 0.97 ? 0.35 : 1) * (kd < 1 ? 0.6 + 0.3 * kd : 1), k4 = (j * n + i) * 4;
      px[k4] = Math.round(clamp(lerp(118, 132, silver) * t, 0, 255));
      px[k4 + 1] = Math.round(clamp(lerp(100, 128, silver) * t, 0, 255));
      px[k4 + 2] = Math.round(clamp(lerp(80, 120, silver) * t, 0, 255));
      px[k4 + 3] = 255;
    }
    return px;
  }

  /* ---- three.js ---- */

  /* mesher gathers triangles of one material: positions, normals, uv in metres over the material's tile, and a colour. */
  function mesher(tile) {
    var pos = [], nor = [], uv = [], col = [];
    function cross(a, b, c) {
      var ux = b[0] - a[0], uy = b[1] - a[1], uz = b[2] - a[2], vx = c[0] - a[0], vy = c[1] - a[1], vz = c[2] - a[2];
      var n = [uy * vz - uz * vy, uz * vx - ux * vz, ux * vy - uy * vx], l = Math.sqrt(n[0] * n[0] + n[1] * n[1] + n[2] * n[2]) || 1;
      return [n[0] / l, n[1] / l, n[2] / l];
    }
    function push(p, n, t, k) { pos.push(p[0], p[1], p[2]); nor.push(n[0], n[1], n[2]); uv.push(t[0], t[1]); col.push(k[0], k[1], k[2]); }
    var m = {
      /* quad a b c d (a and b along its bottom, d over a) facing out (a direction it should face); uv u along a to b, v
         along a to d, in metres over the tile; k its colour. */
      quad: function (a, b, c, d, out, k) {
        var ux = [b[0] - a[0], b[1] - a[1], b[2] - a[2]], vx = [d[0] - a[0], d[1] - a[1], d[2] - a[2]];
        var ul = Math.sqrt(ux[0] * ux[0] + ux[1] * ux[1] + ux[2] * ux[2]) || 1, vl = Math.sqrt(vx[0] * vx[0] + vx[1] * vx[1] + vx[2] * vx[2]) || 1;
        function tc(p) { var q = [p[0] - a[0], p[1] - a[1], p[2] - a[2]]; return [(q[0] * ux[0] + q[1] * ux[1] + q[2] * ux[2]) / ul / tile, (q[0] * vx[0] + q[1] * vx[1] + q[2] * vx[2]) / vl / tile]; }
        var n = cross(a, b, d), flip = out && n[0] * out[0] + n[1] * out[1] + n[2] * out[2] < 0;
        if (flip) n = [-n[0], -n[1], -n[2]];
        k = k || [1, 1, 1];
        var tri = flip ? [a, d, c, a, c, b] : [a, b, c, a, c, d];
        tri.forEach(function (p) { push(p, n, tc(p), k); });
      },
      /* tri a b c facing out, uv by a's quad frame along a to b and up. */
      tri: function (a, b, c, out, k) {
        var n = cross(a, b, c), flip = out && n[0] * out[0] + n[1] * out[1] + n[2] * out[2] < 0;
        if (flip) n = [-n[0], -n[1], -n[2]];
        var ux = [b[0] - a[0], b[2] - a[2]], ul = Math.sqrt(ux[0] * ux[0] + ux[1] * ux[1]) || 1;
        function tc(p) { return [((p[0] - a[0]) * ux[0] + (p[2] - a[2]) * ux[1]) / ul / tile, (p[1] - a[1]) / tile]; }
        (flip ? [a, c, b] : [a, b, c]).forEach(function (p) { push(p, n, tc(p), k || [1, 1, 1]); });
      },
      /* box in frame fr from u0 to u1, y0 to y1, w0 to w1, its five faces but the bottom. */
      box: function (fr, u0, u1, y0, y1, w0, w1, k, bottom) {
        var c = function (u, y, w) { return P(fr, u, y, w); }, f = [fr.fx, 0, fr.fz], r = [fr.fz, 0, -fr.fx];
        m.quad(c(u0, y0, w1), c(u1, y0, w1), c(u1, y1, w1), c(u0, y1, w1), f, k);
        m.quad(c(u1, y0, w0), c(u0, y0, w0), c(u0, y1, w0), c(u1, y1, w0), [-f[0], 0, -f[2]], k);
        m.quad(c(u1, y0, w1), c(u1, y0, w0), c(u1, y1, w0), c(u1, y1, w1), r, k);
        m.quad(c(u0, y0, w0), c(u0, y0, w1), c(u0, y1, w1), c(u0, y1, w0), [-r[0], 0, -r[2]], k);
        m.quad(c(u0, y1, w1), c(u1, y1, w1), c(u1, y1, w0), c(u0, y1, w0), [0, 1, 0], k);
        if (bottom) m.quad(c(u0, y0, w0), c(u1, y0, w0), c(u1, y0, w1), c(u0, y0, w1), [0, -1, 0], k);
      },
      /* prism: an n-sided upright cylinder in frame fr at u, w from y0 to y1, radius rad, its sides and its top. */
      prism: function (fr, u, w, y0, y1, rad, n, k, top) {
        for (var i = 0; i < n; i++) {
          var a0 = i / n * Math.PI * 2, a1 = (i + 1) / n * Math.PI * 2;
          var p0 = P(fr, u + Math.cos(a0) * rad, y0, w + Math.sin(a0) * rad), p1 = P(fr, u + Math.cos(a1) * rad, y0, w + Math.sin(a1) * rad);
          var q0 = [p0[0], p0[1] + y1 - y0, p0[2]], q1 = [p1[0], p1[1] + y1 - y0, p1[2]], mid = P(fr, u, y0, w);
          m.quad(p0, p1, q1, q0, [(p0[0] + p1[0]) / 2 - mid[0], 0, (p0[2] + p1[2]) / 2 - mid[2]], k);
          if (top !== false) m.tri([q0[0], q0[1], q0[2]], [q1[0], q1[1], q1[2]], [mid[0], mid[1] + y1 - y0, mid[2]], [0, 1, 0], k);
        }
      },
      /* wheel: an n-sided disc across frame fr's u axis at u, y, w, radius rad, thickness th: its two faces and its tyre. */
      wheel: function (fr, u, y, w, rad, th, n, k) {
        var r = [fr.fz, 0, -fr.fx];
        for (var i = 0; i < n; i++) {
          var a0 = i / n * Math.PI * 2, a1 = (i + 1) / n * Math.PI * 2;
          var e = function (a, s) { return P(fr, u + s * th / 2, y + Math.sin(a) * rad, w + Math.cos(a) * rad); };
          m.quad(e(a0, -1), e(a1, -1), e(a1, 1), e(a0, 1), [0, Math.sin((a0 + a1) / 2), 0], k);
          [-1, 1].forEach(function (s) { m.tri(P(fr, u + s * th / 2, y, w), e(a0, s), e(a1, s), [r[0] * s, 0, r[2] * s], k); });
        }
      },
      geometry: function (THREE) {
        var g = new THREE.BufferGeometry();
        g.setAttribute("position", new THREE.Float32BufferAttribute(pos, 3));
        g.setAttribute("normal", new THREE.Float32BufferAttribute(nor, 3));
        g.setAttribute("uv", new THREE.Float32BufferAttribute(uv, 2));
        g.setAttribute("color", new THREE.Float32BufferAttribute(col, 3));
        return g;
      },
      count: function () { return pos.length / 3; }
    };
    return m;
  }

  /* gable builds a building whose ridge runs along its length: walls from its lowest ground to the eaves (stone, or a stone
     plinth and boards over it), the gable ends up to the ridge, the roof's two slopes overhanging, and its ridge. */
  function gable(ms, b, walls, plinth, eave) {
    var f = b.frame, l2 = b.len / 2, d2 = b.depth / 2, lo = b.low - f.y, fw = [f.fx, 0, f.fz], rw = [f.fz, 0, -f.fx];
    function wallBox(mat, y0, y1) {
      mat.quad(P(f, -l2, y0, d2), P(f, l2, y0, d2), P(f, l2, y1, d2), P(f, -l2, y1, d2), fw);
      mat.quad(P(f, l2, y0, -d2), P(f, -l2, y0, -d2), P(f, -l2, y1, -d2), P(f, l2, y1, -d2), [-fw[0], 0, -fw[2]]);
      mat.quad(P(f, l2, y0, d2), P(f, l2, y0, -d2), P(f, l2, y1, -d2), P(f, l2, y1, d2), rw);
      mat.quad(P(f, -l2, y0, -d2), P(f, -l2, y0, d2), P(f, -l2, y1, d2), P(f, -l2, y1, -d2), [-rw[0], 0, -rw[2]]);
    }
    if (plinth) { wallBox(ms.stone, lo, plinth); wallBox(walls, plinth, b.eaves); } else wallBox(walls, lo, b.eaves);
    [-1, 1].forEach(function (s) { walls.tri(P(f, s * l2, b.eaves, s * d2), P(f, s * l2, b.eaves, -s * d2), P(f, s * l2, b.ridge, 0), [rw[0] * s, 0, rw[2] * s]); });
    var o = eave, v = 0.3, drop = o * (b.ridge - b.eaves) / d2, k = [0.92, 0.94, 1];
    [-1, 1].forEach(function (s) {
      var a = P(f, -l2 - v, b.eaves - drop, s * (d2 + o)), c = P(f, l2 + v, b.eaves - drop, s * (d2 + o));
      var out = [fw[0] * s * (b.ridge - b.eaves), d2, fw[2] * s * (b.ridge - b.eaves)];
      ms.slate.quad(a, c, P(f, l2 + v, b.ridge, 0), P(f, -l2 - v, b.ridge, 0), out, k);
      ms.slate.quad(a, c, P(f, l2 + v, b.ridge - 0.12, 0), P(f, -l2 - v, b.ridge - 0.12, 0), [-out[0], -out[1], -out[2]], [0.5, 0.5, 0.5]); // its underside
      ms.timber.quad(P(f, -l2 - v, b.eaves - drop - 0.18, s * (d2 + o)), P(f, l2 + v, b.eaves - drop - 0.18, s * (d2 + o)), c, a, [fw[0] * s, 0, fw[2] * s], [0.6, 0.55, 0.5]); // fascia
    });
    ms.slate.box(f, -l2 - v, l2 + v, b.ridge - 0.08, b.ridge + 0.08, -0.14, 0.14, [0.55, 0.52, 0.5]); // ridge tiles
  }

  /* house builds the farmhouse: stone walls, the roof, a chimney with two pots at each gable, the dormers with their own
     little roofs, the windows' frames, sills and lintels, and the door. */
  function house(ms, h) {
    var f = h.frame, d2 = h.depth / 2, l2 = h.len / 2;
    gable(ms, h, ms.stone, 0, 0.35);
    // quoins: dressed corner stones, pale, every other course long one way then the other
    [-1, 1].forEach(function (s) {
      for (var y = 0, q = 0; y < h.eaves - 0.2; y += 0.38, q++) {
        var long = q % 2 ? 0.55 : 0.3, short = q % 2 ? 0.3 : 0.55;
        ms.paint.box(f, s > 0 ? l2 - long : -l2, s > 0 ? l2 + 0.02 : -l2 + long, y, y + 0.32, d2 - 0.02, d2 + 0.02, [0.92, 0.88, 0.78]);
        ms.paint.box(f, s > 0 ? l2 - 0.02 : -l2 - 0.02, s > 0 ? l2 + 0.02 : -l2 + 0.02, y, y + 0.32, d2 - short, d2, [0.92, 0.88, 0.78]);
      }
    });
    // the chimneys, above each gable's apex
    [-1, 1].forEach(function (s) {
      var u = s * (l2 - 0.45);
      ms.stone.box(f, u - 0.45, u + 0.45, h.ridge - 1.4, h.ridge + 1.35, -0.32, 0.32, [0.95, 0.92, 0.88]);
      ms.stone.box(f, u - 0.52, u + 0.52, h.ridge + 1.2, h.ridge + 1.38, -0.38, 0.38, [0.8, 0.78, 0.75]); // its cap
      [-0.2, 0.2].forEach(function (p) { ms.dark.prism(f, u + p, 0, h.ridge + 1.38, h.ridge + 1.72, 0.11, 8, [0.62, 0.35, 0.25]); });
    });
    // the dormers: a stone front with its window, cheeks back into the roof, a slate gable roof
    [-2.2, 2.2].forEach(function (u) {
      var w0 = d2 - 1.0, base = roofY(h, w0) - 0.25, top = base + 1.45, ap = top + 0.55, back = 0.4;
      ms.stone.box(f, u - 0.75, u + 0.75, base, top, back, w0, [0.97, 0.95, 0.9]);
      ms.stone.tri(P(f, u - 0.75, top, w0), P(f, u + 0.75, top, w0), P(f, u, ap, w0), [f.fx, 0, f.fz]);
      [-1, 1].forEach(function (s) {
        var a = P(f, u + s * 0.92, top - 0.12, w0 + 0.22), b = P(f, u + s * 0.92, top - 0.12, back), out = [f.fz * s * 0.55, 0.92, -f.fx * s * 0.55];
        ms.slate.quad(a, b, P(f, u, ap + 0.04, back), P(f, u, ap + 0.04, w0 + 0.22), out, [0.92, 0.94, 1]);
      });
    });
    // the door: a pale stone surround, a lintel and planks painted deep green; a step
    ms.paint.box(f, -0.65, 0.85, 0, 2.45, d2, d2 + 0.03, [0.9, 0.86, 0.76]);
    ms.timber.box(f, -0.45, 0.65, 0, 2.15, d2 + 0.02, d2 + 0.05, [0.25, 0.42, 0.3]);
    ms.stone.box(f, -0.8, 1.0, -0.4, 0.14, d2, d2 + 0.45, [0.85, 0.83, 0.8]);
  }

  /* barn builds the barn: a stone plinth under weatherboards, the roof, the doors on its east gable standing open on the
     dark inside, the hayloft over them with hay showing and a hoist beam, the lamp on its bracket beside the doors. */
  function barn(ms, b) {
    var f = b.frame, l2 = b.len / 2;
    gable(ms, b, ms.timber, 0.7, 0.4);
    var g = sub(f, l2, 0, 0, 1, 0); // the doors' gable, its front facing out of the barn's east end (local u of the barn)
    ms.dark.quad(P(g, -1.8, 0, 0.03), P(g, 1.8, 0, 0.03), P(g, 1.8, 3.8, 0.03), P(g, -1.8, 3.8, 0.03), [g.fx, 0, g.fz], [0.08, 0.07, 0.06]);
    ms.hay.quad(P(g, -1.7, 0, 0.04), P(g, -0.4, 0, 0.04), P(g, -0.4, 1.1, 0.04), P(g, -1.7, 1.1, 0.04), [g.fx, 0, g.fz], [0.45, 0.38, 0.22]); // a bale inside
    ms.timber.box(g, -1.95, 1.95, 3.8, 4.05, 0, 0.12, [0.7, 0.62, 0.52]); // the doors' head
    [-1, 1].forEach(function (s) { // each leaf open outward about 120 degrees, its outer face to the yard
      var leaf = sub(g, s * 1.8, 0, 0, s * 0.5, 0.87);
      ms.timber.box(leaf, -0.04, 0.04, 0.05, 3.7, 0, 1.75, [0.85, 0.8, 0.72]);
      ms.timber.box(leaf, -0.06, 0.06, 0.4, 0.55, 0.05, 1.7, [0.6, 0.55, 0.48]); // ledges
      ms.timber.box(leaf, -0.06, 0.06, 3.1, 3.25, 0.05, 1.7, [0.6, 0.55, 0.48]);
    });
    var loft = 5.4;
    ms.dark.quad(P(g, -0.8, loft - 0.7, 0.03), P(g, 0.8, loft - 0.7, 0.03), P(g, 0.8, loft + 0.7, 0.03), P(g, -0.8, loft + 0.7, 0.03), [g.fx, 0, g.fz], [0.1, 0.08, 0.06]);
    ms.hay.quad(P(g, -0.75, loft - 0.7, 0.05), P(g, 0.75, loft - 0.7, 0.05), P(g, 0.75, loft + 0.05, 0.05), P(g, -0.75, loft + 0.05, 0.05), [g.fx, 0, g.fz], [0.95, 0.85, 0.55]);
    ms.timber.box(g, -0.12, 0.12, b.ridge - 0.65, b.ridge - 0.4, 0, 0.9, [0.6, 0.52, 0.42]); // the hoist beam
    // the lamp: a bracket, an iron hood, a bulb that is lit at night
    ms.dark.box(g, 2.3, 2.36, 4.05, 4.12, 0, 0.45, [0.12, 0.12, 0.12]);
    ms.dark.box(g, 2.2, 2.46, 3.86, 4.0, 0.3, 0.55, [0.14, 0.14, 0.14]);
    ms.lamp.box(g, 2.24, 2.42, 3.7, 3.86, 0.34, 0.52, [1, 1, 1], true);
    return P(g, 2.33, 3.78, 0.6); // where the lamp's glow is
  }

  /* yard builds the well (a round stone kerb, two posts, a little roof and a windlass) and the cart (a bed of boards on two
     wheels, its shafts down on the ground). */
  function yard(ms, well, cart) {
    var w = sub(well, 0, 0, 0, 1, 0);
    ms.stone.prism(w, 0, 0, -0.3, 0.85, 0.78, 12, [0.92, 0.9, 0.86]);
    ms.dark.prism(w, 0, 0, 0.6, 0.87, 0.6, 12, [0.05, 0.05, 0.06], true);
    [-1, 1].forEach(function (s) { ms.timber.box(w, s * 0.72 - 0.06, s * 0.72 + 0.06, 0.6, 2.05, -0.06, 0.06, [0.7, 0.62, 0.52]); });
    ms.timber.box(w, -0.75, 0.75, 1.35, 1.45, -0.05, 0.05, [0.6, 0.52, 0.45]);
    [-1, 1].forEach(function (s) { ms.slate.quad(P(w, -0.95, 1.78, s * 0.7), P(w, 0.95, 1.78, s * 0.7), P(w, 0.95, 2.25, 0), P(w, -0.95, 2.25, 0), [0, 1, s * 0.6], [0.95, 0.95, 1]); });
    var c = cart, k = [0.82, 0.68, 0.5];
    ms.timber.box(c, -0.8, 0.8, 0.75, 0.86, -1.3, 1.3, k, true); // the bed
    [-1, 1].forEach(function (s) { ms.timber.box(c, s * 0.8 - 0.05, s * 0.8 + 0.05, 0.86, 1.25, -1.3, 1.3, k); });
    ms.timber.box(c, -0.8, 0.8, 0.86, 1.25, -1.35, -1.3, k);
    [-1, 1].forEach(function (s) { ms.timber.box(c, s * 0.45 - 0.05, s * 0.45 + 0.05, 0.12, 0.75, 1.3, 3.2, k); }); // the shafts, resting on the ground
    [-1, 1].forEach(function (s) { ms.dark.wheel(c, s * 0.95, 0.62, -0.1, 0.62, 0.1, 12, [0.3, 0.22, 0.16]); });
  }

  /* wall builds a run of dry-stone wall: its battered faces and top from the points of run, following the ground. */
  function wallMesh(m, run) {
    for (var i = 0; i + 3 < run.length; i += 3) {
      var a = [run[i], run[i + 1], run[i + 2]], b = [run[i + 3], run[i + 4], run[i + 5]], dx = b[0] - a[0], dz = b[2] - a[2], l = Math.sqrt(dx * dx + dz * dz) || 1;
      var nx = -dz / l, nz = dx / l, B = WALL.base / 2, T = WALL.top / 2, H = WALL.h;
      [-1, 1].forEach(function (s) {
        m.quad([a[0] + nx * B * s, a[1] - 0.2, a[2] + nz * B * s], [b[0] + nx * B * s, b[1] - 0.2, b[2] + nz * B * s], [b[0] + nx * T * s, b[1] + H, b[2] + nz * T * s], [a[0] + nx * T * s, a[1] + H, a[2] + nz * T * s], [nx * s, 0.2, nz * s]);
      });
      m.quad([a[0] - nx * T, a[1] + H, a[2] - nz * T], [b[0] - nx * T, b[1] + H, b[2] - nz * T], [b[0] + nx * T, b[1] + H, b[2] + nz * T], [a[0] + nx * T, a[1] + H, a[2] + nz * T], [0, 1, 0]);
    }
    // the run's ends
    [[0, 3], [run.length - 3, run.length - 6]].forEach(function (e) {
      var a = [run[e[0]], run[e[0] + 1], run[e[0] + 2]], b = [run[e[1]], run[e[1] + 1], run[e[1] + 2]], dx = a[0] - b[0], dz = a[2] - b[2], l = Math.sqrt(dx * dx + dz * dz) || 1;
      var nx = -dz / l, nz = dx / l, B = WALL.base / 2, T = WALL.top / 2;
      m.quad([a[0] - nx * B, a[1] - 0.2, a[2] - nz * B], [a[0] + nx * B, a[1] - 0.2, a[2] + nz * B], [a[0] + nx * T, a[1] + WALL.h, a[2] + nz * T], [a[0] - nx * T, a[1] + WALL.h, a[2] - nz * T], [dx / l, 0, dz / l]);
    });
  }

  /* gateMesh builds the gate's two stone-capped posts and its leaf of five bars, a brace and two stiles, swung open. */
  function gateMesh(ms, gt) {
    gt.posts.forEach(function (p) { var f = frame(p[0], p[2], 1, 0, p[1]); ms.stone.box(f, -0.3, 0.3, -0.2, 1.45, -0.3, 0.3, [0.85, 0.83, 0.8]); });
    var h = gt.posts[0], f = frame(h[0], h[2], -gt.leaf[1], gt.leaf[0], h[1]), L2 = gt.len, k = [0.9, 0.86, 0.78]; // u runs along the leaf
    ms.timber.box(f, 0.35, 0.47, 0.15, 1.25, -0.04, 0.04, k);
    ms.timber.box(f, L2 - 0.12, L2, 0.15, 1.15, -0.04, 0.04, k);
    for (var i = 0; i < 5; i++) { var y = 0.25 + i * 0.22; ms.timber.box(f, 0.35, L2, y, y + 0.08, -0.03, 0.03, k); }
    // the brace, up from the hinge's foot to the far stile's top, in short steps
    for (var s = 0, steps = 9; s < steps; s++) { var u0 = 0.45 + (L2 - 0.6) * s / steps; ms.timber.box(f, u0, u0 + (L2 - 0.6) / steps, 0.3 + 0.9 * s / steps, 0.38 + 0.9 * s / steps, -0.025, 0.025, k); }
  }

  /* bridgeMesh builds the footbridge: its planks across an arched deck, two stringers, posts and a handrail each side. */
  function bridgeMesh(m, br) {
    var x = br.at[0], z0 = br.at[1] - br.len / 2, W = br.width / 2, k = [0.8, 0.72, 0.6];
    for (var i = 0; i < br.planks; i++) {
      var s0 = i / br.planks, s1 = s0 + 0.85 / br.planks, y0 = deckY(br, s0), y1 = deckY(br, s1), za = z0 + br.len * s0, zb = z0 + br.len * s1;
      m.quad([x - W, y0, za], [x + W, y0, za], [x + W, y1, zb], [x - W, y1, zb], [0, 1, 0], [k[0] * (0.85 + 0.3 * L.hash(i, 1, SEED + 7)), k[1], k[2]]);
      m.quad([x - W, y0 - 0.05, za], [x + W, y0 - 0.05, za], [x + W, y0, za], [x - W, y0, za], [0, 0, -1], k);
    }
    [-1, 1].forEach(function (s) {
      for (var j = 0; j <= 6; j++) {
        var t = j / 6, z = z0 + br.len * t, y = deckY(br, t), f = frame(x + s * (W - 0.05), z, 0, 1, y);
        m.box(f, -0.05, 0.05, -1.2, 1.0, -0.05, 0.05, k);
        if (j < 6) {
          var t1 = (j + 1) / 6, ya = y + 0.95, yb = deckY(br, t1) + 0.95, zb = z0 + br.len * t1;
          m.quad([x + s * (W - 0.1), ya, z], [x + s * (W - 0.1), yb, zb], [x + s * (W - 0.1), yb + 0.08, zb], [x + s * (W - 0.1), ya + 0.08, z], [s, 0, 0], k);
          m.quad([x + s * (W - 0.05), ya + 0.08, z], [x + s * (W - 0.05), yb + 0.08, zb], [x + s * (W + 0.05), yb + 0.08, zb], [x + s * (W + 0.05), ya + 0.08, z], [0, 1, 0], k);
          m.quad([x + s * W, y - 0.25, z], [x + s * W, deckY(br, t1) - 0.25, zb], [x + s * W, deckY(br, t1) - 0.02, zb], [x + s * W, y - 0.02, z], [s, 0, 0], [0.6, 0.52, 0.42]);
        }
      }
    });
  }

  /* windowMeshes puts each window's pale frame, its glass and a stone sill on the walls, and its lit pane in the lit mesh. */
  function windowMeshes(ms, list) {
    list.forEach(function (w) {
      var f = frame(w.at[0], w.at[2], w.out[0], w.out[1], w.at[1]), hw = w.w / 2, hh = w.h / 2, fw = [f.fx, 0, f.fz];
      ms.paint.quad(P(f, -hw - 0.09, -hh - 0.09, 0), P(f, hw + 0.09, -hh - 0.09, 0), P(f, hw + 0.09, hh + 0.09, 0), P(f, -hw - 0.09, hh + 0.09, 0), fw, w.kind === "barn" ? [0.55, 0.5, 0.44] : [0.93, 0.92, 0.86]);
      ms.glass.quad(P(f, -hw, -hh, 0.01), P(f, hw, -hh, 0.01), P(f, hw, hh, 0.01), P(f, -hw, hh, 0.01), fw);
      if (w.on) {
        ms.lit.quad(P(f, -hw, -hh, 0.015), P(f, -0.03, -hh, 0.015), P(f, -0.03, hh, 0.015), P(f, -hw, hh, 0.015), fw);
        ms.lit.quad(P(f, 0.03, -hh, 0.015), P(f, hw, -hh, 0.015), P(f, hw, hh, 0.015), P(f, 0.03, hh, 0.015), fw);
      }
      if (w.kind !== "barn") ms.stone.box(f, -hw - 0.14, hw + 0.14, -hh - 0.17, -hh - 0.09, -0.02, 0.12, [1.05, 1.02, 0.95]);
    });
  }

  /* basicFog patches a MeshBasicMaterial's shader (onBeforeCompile) with the land's height fog, which reads the view
     position its shader lacks; without it three's plain distance fog washes out what is far. */
  function basicFog(sh) {
    sh.vertexShader = sh.vertexShader.replace("#include <common>", "#include <common>\nvarying vec3 vViewPosition;").replace("#include <project_vertex>", "#include <project_vertex>\nvViewPosition = -mvPosition.xyz;");
    sh.fragmentShader = sh.fragmentShader.replace("#include <common>", "#include <common>\nvarying vec3 vViewPosition;");
    L.heightFog(sh);
  }
  /* metres makes a PointsMaterial's size a size in metres at any field of view: three scales an attenuated point by half the
     drawing buffer's height over its depth, as if the view were 90 degrees high, so its size is put through the projection's
     own vertical scale. grow, when given, is GLSL multiplying it further. */
  function metres(sh, grow) { sh.vertexShader = sh.vertexShader.replace("gl_PointSize = size;", "gl_PointSize = size * projectionMatrix[1][1]" + (grow ? " * (" + grow + ")" : "") + ";"); }

  /* material makes a MeshStandardMaterial of colour col, with the pixels' texture (tiling, sRGB) as its map and bump when
     there is a document, and the land's height fog; its colour by vertex for a mesher's geometry, by instance when inst. */
  function material(THREE, doc, col, pixels, rough, bump, inst) {
    var m = new THREE.MeshStandardMaterial({ color: doc && pixels ? 0xffffff : col, roughness: rough, vertexColors: !inst });
    if (doc && pixels) {
      var t = new THREE.DataTexture(pixels(), 128, 128);
      t.wrapS = t.wrapT = THREE.RepeatWrapping; t.colorSpace = THREE.SRGBColorSpace; t.anisotropy = 4;
      t.magFilter = THREE.LinearFilter; t.minFilter = THREE.LinearMipmapLinearFilter; t.generateMipmaps = true; t.needsUpdate = true;
      m.map = t;
      if (bump) { m.bumpMap = t; m.bumpScale = bump; }
    }
    m.onBeforeCompile = L.heightFog;
    return m;
  }

  // the farm's own uniforms: seconds, and the wind the smoke drifts in
  var uniforms = null;
  /* patch returns an onBeforeCompile putting the farm's time, vert.head and the code of vert (chunk name -> code after it, or
     replacing it when it starts with "=") in the vertex shader, frag likewise in the fragment shader, then the land's height fog
     unless fog is false. */
  function patch(vert, frag, fog) {
    return function (shader) {
      shader.uniforms.uTime = uniforms.uTime;
      shader.uniforms.uWind = uniforms.uWind;
      function put(src, code) {
        Object.keys(code || {}).forEach(function (chunk) { if (chunk === "head") return; var c = code[chunk]; src = src.replace("#include <" + chunk + ">", c.charAt(0) === "=" ? c.slice(1) : "#include <" + chunk + ">\n" + c); });
        return src;
      }
      var head = (vert && vert.head) || "";
      shader.vertexShader = put(shader.vertexShader.replace("#include <common>", "#include <common>\nuniform float uTime;\nuniform vec3 uWind;\n" + head), vert);
      shader.fragmentShader = put(shader.fragmentShader, frag);
      if (fog !== false) L.heightFog(shader);
    };
  }

  /* beastGeometry: a low-poly cow (kind 0) or sheep (1) built of boxes, standing on y 0 and facing +z; aHead is the neck's
     pivot and how much a vertex goes with the head as it lowers to graze; the colour darkens the legs and the face. */
  function beastGeometry(THREE, kind) {
    var m = mesher(1), head = [], f = frame(0, 0, 0, 1, 0), pv = kind ? [0, 0.62, 0.48] : [0, 1.22, 0.82];
    function part(u0, u1, y0, y1, w0, w1, k, h) { var before = m.count(); m.box(f, u0, u1, y0, y1, w0, w1, k, true); for (var i = before; i < m.count(); i++) head.push(pv[0], pv[1], pv[2], h); }
    if (!kind) {
      part(-0.36, 0.36, 0.72, 1.48, -0.95, 0.85, [1, 1, 1], 0);
      [[-0.24, -0.78], [0.24, -0.78], [-0.24, 0.66], [0.24, 0.66]].forEach(function (l) { part(l[0] - 0.08, l[0] + 0.08, 0, 0.76, l[1] - 0.08, l[1] + 0.08, [0.7, 0.7, 0.7], 0); });
      part(-0.17, 0.17, 1.0, 1.42, 0.8, 1.42, [0.85, 0.85, 0.85], 1); // neck and head
      part(-0.14, 0.14, 0.98, 1.2, 1.42, 1.6, [0.6, 0.55, 0.52], 1); // the muzzle
      part(-0.28, 0.28, 1.36, 1.44, 1.05, 1.12, [0.95, 0.92, 0.85], 1); // horns
      part(-0.03, 0.03, 0.75, 1.42, -0.99, -0.95, [0.6, 0.6, 0.6], 0); // the tail
    } else {
      part(-0.3, 0.3, 0.38, 0.9, -0.52, 0.5, [1, 1, 1], 0);
      [[-0.17, -0.38], [0.17, -0.38], [-0.17, 0.36], [0.17, 0.36]].forEach(function (l) { part(l[0] - 0.05, l[0] + 0.05, 0, 0.42, l[1] - 0.05, l[1] + 0.05, [0.18, 0.17, 0.16], 0); });
      part(-0.12, 0.12, 0.62, 0.92, 0.46, 0.82, [0.2, 0.19, 0.18], 1);
    }
    var g = m.geometry(THREE);
    g.setAttribute("aHead", new THREE.Float32BufferAttribute(head, 4));
    return g;
  }
  // a beast lowers its head to graze for most of each spell, and raises it to look about between
  var BEAST_VERT = {
    head: "attribute vec4 aHead;\nattribute float aPhase;\nfloat farmGraze() { float f = fract(uTime / 23.0 + aPhase); return 0.95 * smoothstep(0.08, 0.16, f) * (1.0 - smoothstep(0.78, 0.86, f)); }\nvec3 farmNod(vec3 p, float a) { vec3 q = p - aHead.xyz; float c = cos(a), s = sin(a); return aHead.xyz + vec3(q.x, q.y * c - q.z * s, q.y * s + q.z * c); }",
    beginnormal_vertex: "float farmA = farmGraze() * aHead.w;\nobjectNormal = mix(objectNormal, farmNod(objectNormal + aHead.xyz, farmA) - aHead.xyz, aHead.w);",
    begin_vertex: "transformed = mix(transformed, farmNod(transformed, farmGraze() * aHead.w), aHead.w);"
  };
  function beastMesh(THREE, list, kind, name) {
    var idx = [];
    for (var i = 0; i < list.length; i += 7) if (list[i + 5] === kind) idx.push(i);
    var g = beastGeometry(THREE, kind), phase = new Float32Array(idx.length);
    idx.forEach(function (k, j) { phase[j] = list[k + 6]; });
    g.setAttribute("aPhase", new THREE.InstancedBufferAttribute(phase, 1));
    var m = new THREE.MeshStandardMaterial({ vertexColors: true, roughness: 0.9 });
    m.onBeforeCompile = patch(BEAST_VERT, null);
    m.customProgramCacheKey = function () { return "farm-" + name; };
    var depth = new THREE.MeshDepthMaterial();
    depth.onBeforeCompile = patch({ head: BEAST_VERT.head, begin_vertex: BEAST_VERT.begin_vertex }, null, false);
    depth.customProgramCacheKey = function () { return "farm-" + name + "-depth"; };
    var coats = kind ? ["#ece6d6", "#e2dccb", "#f2eee2", "#d8d0bc"] : ["#6a3c22", "#2a2522", "#8a5a34", "#efe8dc"];
    var mesh = new THREE.InstancedMesh(g, m, idx.length), mx = new THREE.Matrix4(), q = new THREE.Quaternion(), up = new THREE.Vector3(0, 1, 0), c = new THREE.Color();
    idx.forEach(function (k, j) {
      q.setFromAxisAngle(up, Math.atan2(list[k + 3], list[k + 4]));
      mx.compose(new THREE.Vector3(list[k], list[k + 1] - 0.03, list[k + 2]), q, new THREE.Vector3(1, 1, 1));
      mesh.setMatrixAt(j, mx);
      mesh.setColorAt(j, c.set(coats[Math.floor(L.hash(j, kind, SEED + 8) * coats.length)]));
    });
    mesh.castShadow = mesh.receiveShadow = true;
    mesh.customDepthMaterial = depth;
    mesh.name = name;
    mesh.computeBoundingSphere();
    return mesh;
  }
  /* unitBox: a unit box from y0 (0.5 without it: from 0 to 1) (centred in x and z), for the posts, rails, coping and bales' instancing. */
  function unitBox(THREE, y0) { var g = new THREE.BoxGeometry(1, 1, 1); g.translate(0, y0 == null ? 0.5 : y0, 0); return g; }

  /* instances makes an InstancedMesh of g and m, n instances placed by set(i, matrix, colour), casting and receiving. */
  function instances(THREE, g, m, n, name, set) {
    var mesh = new THREE.InstancedMesh(g, m, n), mx = new THREE.Matrix4(), c = new THREE.Color();
    for (var i = 0; i < n; i++) { set(i, mx, c); mesh.setMatrixAt(i, mx); mesh.setColorAt(i, c); }
    mesh.instanceMatrix.needsUpdate = true;
    mesh.castShadow = mesh.receiveShadow = true;
    mesh.name = name;
    mesh.computeBoundingSphere();
    return mesh;
  }
  /* along composes a matrix at x, y, z turned so its local x runs along dx, dy, dz (the length sx), y up, z across. */
  function along(THREE, mx, x, y, z, dx, dy, dz, sx, sy, sz, lean) {
    var l = Math.sqrt(dx * dx + dy * dy + dz * dz) || 1, xa = new THREE.Vector3(dx / l, dy / l, dz / l), ya = new THREE.Vector3(0, 1, 0);
    var za = new THREE.Vector3().crossVectors(xa, ya).normalize(); ya.crossVectors(za, xa).normalize();
    if (lean) ya.applyAxisAngle(xa, lean), za.applyAxisAngle(xa, lean);
    mx.makeBasis(xa, ya, za);
    mx.scale(new THREE.Vector3(sx, sy, sz));
    mx.setPosition(x, y, z);
    return mx;
  }

  /* fences builds the paddocks' posts and their two rails between each post and the next. */
  function fences(THREE, mat, lay) {
    var ps = lay.posts, group = [], rails = [];
    var post = instances(THREE, unitBox(THREE, 0.5), mat, ps.length / 3, "posts", function (i, mx, c) {
      mx.makeScale(0.13, 1.25, 0.13).setPosition(ps[i * 3], ps[i * 3 + 1] - 0.15, ps[i * 3 + 2]);
      c.setRGB(0.78 + 0.2 * L.hash(i, 1, SEED + 9), 0.72, 0.62);
    });
    var at = 0;
    lay.paddocks.forEach(function (p) {
      var n = 0;
      for (var i = 0; i < p.ring.length; i++) {
        var a = p.ring[i], b = p.ring[(i + 1) % p.ring.length], len = Math.sqrt((b[0] - a[0]) * (b[0] - a[0]) + (b[1] - a[1]) * (b[1] - a[1]));
        n += Math.max(1, Math.ceil(len / POST));
      }
      for (var k = 0; k < n; k++) { var i0 = at + k, i1 = at + (k + 1) % n; rails.push([i0, i1]); }
      at += n;
    });
    var rail = instances(THREE, unitBox(THREE, 0), mat, rails.length * 2, "rails", function (i, mx, c) {
      var e = rails[i >> 1], a = e[0] * 3, b = e[1] * 3, y = i & 1 ? 1.0 : 0.55;
      along(THREE, mx, (ps[a] + ps[b]) / 2, (ps[a + 1] + ps[b + 1]) / 2 + y - 0.15, (ps[a + 2] + ps[b + 2]) / 2, ps[b] - ps[a], ps[b + 1] - ps[a + 1], ps[b + 2] - ps[a + 2],
        Math.sqrt((ps[b] - ps[a]) * (ps[b] - ps[a]) + (ps[b + 1] - ps[a + 1]) * (ps[b + 1] - ps[a + 1]) + (ps[b + 2] - ps[a + 2]) * (ps[b + 2] - ps[a + 2])) + 0.1, 0.09, 0.05);
      c.setRGB(0.82, 0.76, 0.66);
    });
    group.push(post, rail);
    return group;
  }

  /* glowTexture: a soft round glow, white in the middle to clear at the rim, for the window halos and the smoke's puffs. */
  function glowTexture(THREE, doc, soft) {
    var cv = doc.createElement("canvas");
    cv.width = cv.height = 64;
    var ctx = cv.getContext("2d"), g = ctx.createRadialGradient(32, 32, 0, 32, 32, 32);
    g.addColorStop(0, "rgba(255,255,255,1)"); g.addColorStop(soft ? 0.45 : 0.18, "rgba(255,255,255," + (soft ? 0.55 : 0.5) + ")"); g.addColorStop(1, "rgba(255,255,255,0)");
    ctx.fillStyle = g; ctx.fillRect(0, 0, 64, 64);
    var t = new THREE.CanvasTexture(cv);
    t.colorSpace = THREE.SRGBColorSpace;
    return t;
  }

  /* The smoke: each puff rises from its chimney's pot and drifts downwind as it ages over twenty seconds, swelling and
     thinning, wandering a little; its age from the farm's time and its own phase, so a still frame shows the whole ribbon. */
  var SMOKE_VERT = {
    head: "attribute float aSeed;\nvarying float vSmoke;",
    begin_vertex: [
      "float smokeAge = fract(uTime / 20.0 + aSeed);",
      "vec2 smokeWind = normalize(uWind.xy);",
      "transformed += vec3(smokeWind.x, 0.0, smokeWind.y) * smokeAge * 16.0 + vec3(0.0, 7.0 * sqrt(smokeAge) + 1.5 * smokeAge, 0.0);",
      "transformed.xz += vec2(sin(aSeed * 40.0 + uTime * 0.3), cos(aSeed * 31.0 + uTime * 0.23)) * smokeAge * 1.6;",
      "vSmoke = smoothstep(0.0, 0.06, smokeAge) * (1.0 - smokeAge) * (1.0 - smokeAge);"
    ].join("\n")
  };
  function smokeMesh(THREE, doc, tops, p) {
    var pos = [], seed = [];
    tops.forEach(function (t, c) { for (var i = 0; i < PUFFS * (c ? 0.6 : 1); i++) { pos.push(t[0], t[1], t[2]); seed.push(L.hash(i, c, SEED + 10)); } });
    var g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(pos, 3));
    g.setAttribute("aSeed", new THREE.Float32BufferAttribute(seed, 1));
    var col = new THREE.Color(0x9a9ca2).lerp(new THREE.Color(p ? p.fog : 0x9a9ca2), 0.35);
    var m = new THREE.PointsMaterial({ color: col, size: 1.3, sizeAttenuation: true, transparent: true, opacity: 0.36, depthWrite: false, fog: false, map: doc ? glowTexture(THREE, doc, true) : null });
    m.onBeforeCompile = function (sh) {
      patch(SMOKE_VERT, { common: "=#include <common>\nvarying float vSmoke;", color_fragment: "diffuseColor.a *= vSmoke;" }, false)(sh);
      metres(sh, "1.0 + 4.0 * fract(uTime / 20.0 + aSeed)"); // a puff swells as it drifts
    };
    m.customProgramCacheKey = function () { return "farm-smoke"; };
    var pts = new THREE.Points(g, m);
    pts.name = "smoke";
    pts.frustumCulled = false; // the puffs move in the shader
    return pts;
  }

  /* birdGeometry: a swift's scythe wings (kind 0), or a bat's broad ones (1), about a short body; aWing how far out. */
  function birdGeometry(THREE, kind) {
    var s = kind ? 0.32 : 0.42, g = new THREE.BufferGeometry();
    var p = kind ? [0, 0, 0.06, 0, 0, -0.06, -s, 0.02, 0.0, s, 0.02, 0.0, -s * 0.6, 0, -0.1, s * 0.6, 0, -0.1] : [0, 0, 0.12, 0, 0, -0.1, -s, 0.03, -0.16, s, 0.03, -0.16, -s * 0.4, 0, 0.02, s * 0.4, 0, 0.02];
    g.setAttribute("position", new THREE.Float32BufferAttribute(p, 3));
    g.setAttribute("normal", new THREE.Float32BufferAttribute([0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0], 3));
    g.setAttribute("aWing", new THREE.Float32BufferAttribute([0, 0, 1, 1, 0.5, 0.5], 1));
    g.setIndex([0, 4, 1, 4, 2, 1, 0, 1, 5, 5, 1, 3, 4, 2, 1, 5, 1, 3]);
    return g;
  }
  function birdMesh(THREE, list, kind, name) {
    var n = list.length / 8, g = birdGeometry(THREE, kind), ph = new Float32Array(n);
    for (var i = 0; i < n; i++) ph[i] = list[i * 8 + 7];
    g.setAttribute("aPhase", new THREE.InstancedBufferAttribute(ph, 1));
    var m = new THREE.MeshStandardMaterial({ color: kind ? 0x2a211c : 0x1b1a1e, roughness: 0.8, side: THREE.DoubleSide });
    m.onBeforeCompile = function (sh) {
      patch({ head: "attribute float aWing;\nattribute float aPhase;", begin_vertex: "transformed.y += aWing * " + (kind ? "0.18 * sin(uTime * 19.0 + aPhase * 6.2831853)" : "0.12 * sin(uTime * 11.0 + aPhase * 6.2831853) * step(0.5, fract(uTime * 0.4 + aPhase))") + ";" })(sh);
    };
    m.customProgramCacheKey = function () { return "farm-" + name; };
    var mesh = new THREE.InstancedMesh(g, m, n);
    mesh.name = name;
    mesh.frustumCulled = false;
    mesh.castShadow = false;
    mesh.userData.kind = kind;
    mesh.userData.list = list;
    return mesh;
  }
  /* fly sets each bird on its loop at t seconds: an ellipse about its centre, a swift's swept wide and smooth, a bat's
     jinking, heading along it and banked into the turn. */
  var tmp = null;
  function fly(THREE, mesh, t) {
    if (!tmp) tmp = { m: new THREE.Matrix4(), q: new THREE.Quaternion(), e: new THREE.Euler(0, 0, 0, "YXZ"), p: new THREE.Vector3(), s: new THREE.Vector3(1, 1, 1) };
    var b = mesh.userData.list, bat = mesh.userData.kind, n = b.length / 8;
    for (var i = 0; i < n; i++) {
      var k = i * 8, a = b[k + 6] + b[k + 5] * t, j = bat ? 0.35 * Math.sin(t * 3.1 + b[k + 6] * 5) + 0.2 * Math.sin(t * 5.3 + i) : 0.25 * Math.sin(a * 2 + i);
      var x = b[k] + Math.cos(a + j) * b[k + 2], z = b[k + 1] + Math.sin(a + j) * b[k + 3], y = b[k + 4] + (bat ? 0.8 : 2.5) * Math.sin(a * (bat ? 3 : 1.5) + i);
      var a2 = a + 0.01 * (b[k + 5] > 0 ? 1 : -1), x2 = b[k] + Math.cos(a2 + j) * b[k + 2], z2 = b[k + 1] + Math.sin(a2 + j) * b[k + 3];
      tmp.p.set(x, y, z);
      tmp.e.set(0, Math.atan2(x2 - x, z2 - z), (b[k + 5] > 0 ? -1 : 1) * (bat ? 0.25 : 0.5));
      tmp.q.setFromEuler(tmp.e);
      mesh.setMatrixAt(i, tmp.m.compose(tmp.p, tmp.q, tmp.s));
    }
    mesh.instanceMatrix.needsUpdate = true;
  }

  /* ---- the pond ---- */

  /* mirror(THREE, geometry, opts) -> a Mesh that shows, on its plane (the geometry's xy, facing +z), the scene seen from the
     camera's mirror image, drawn before each frame into a render target at opts.scale of the drawing buffer (half): the
     Reflector of three's examples, its oblique near plane cutting away what lies under the water, the mirror camera seeing
     only what is on layer REFLECT. The water's own colour (opts.tint) shows looking down into it; the reflection grows to
     nearly all of it at a grazing angle (Schlick's Fresnel). */
  function mirror(THREE, geometry, opts) {
    var scale = opts.scale || 0.5, target = new THREE.WebGLRenderTarget(2, 2, { type: THREE.HalfFloatType, samples: 0 });
    var tm = new THREE.Matrix4(), cam = new THREE.PerspectiveCamera(), plane = new THREE.Plane(), clip = new THREE.Vector4(), q = new THREE.Vector4();
    var n = new THREE.Vector3(), wp = new THREE.Vector3(), cp = new THREE.Vector3(), rot = new THREE.Matrix4(), look = new THREE.Vector3(), view = new THREE.Vector3(), aim = new THREE.Vector3(), size = new THREE.Vector2();
    cam.layers.set(REFLECT);
    var m = new THREE.ShaderMaterial({
      uniforms: THREE.UniformsUtils.merge([THREE.UniformsLib.fog, { tReflect: { value: null }, uTexMatrix: { value: null }, uTint: { value: new THREE.Color(opts.tint) } }]),
      vertexShader: [
        "uniform mat4 uTexMatrix;", "varying vec4 vReflUv;", "varying vec3 vViewPosition;", "varying vec3 vWorld;",
        "void main() {", "  vReflUv = uTexMatrix * vec4(position, 1.0);", "  vec4 mv = modelViewMatrix * vec4(position, 1.0);", "  vViewPosition = -mv.xyz;",
        "  vWorld = (modelMatrix * vec4(position, 1.0)).xyz;", "  gl_Position = projectionMatrix * mv;", "}"
      ].join("\n"),
      fragmentShader: [
        "uniform sampler2D tReflect;", "uniform vec3 uTint;", "varying vec4 vReflUv;", "varying vec3 vViewPosition;", "varying vec3 vWorld;", "#include <fog_pars_fragment>",
        "void main() {", "  vec3 refl = texture2DProj(tReflect, vReflUv).rgb;", "  float c = clamp(normalize(cameraPosition - vWorld).y, 0.0, 1.0);",
        "  float fres = 0.02 + 0.98 * pow(1.0 - c, 5.0);", "  gl_FragColor = vec4(mix(uTint, refl * 0.92, fres), 1.0);",
        "  #include <tonemapping_fragment>", "  #include <colorspace_fragment>", "  #include <fog_fragment>", "}"
      ].join("\n"),
      fog: true
    });
    m.uniforms.tReflect.value = target.texture;
    m.uniforms.uTexMatrix.value = tm;
    m.onBeforeCompile = L.heightFog;
    var mesh = new THREE.Mesh(geometry, m);
    mesh.userData.target = target;
    mesh.userData.camera = cam;
    mesh.onBeforeRender = function (renderer, scene, camera) {
      wp.setFromMatrixPosition(mesh.matrixWorld); cp.setFromMatrixPosition(camera.matrixWorld);
      rot.extractRotation(mesh.matrixWorld); n.set(0, 0, 1).applyMatrix4(rot);
      view.subVectors(wp, cp);
      if (view.dot(n) > 0) return; // seen from under the water
      view.reflect(n).negate().add(wp);
      rot.extractRotation(camera.matrixWorld);
      look.set(0, 0, -1).applyMatrix4(rot).add(cp);
      aim.subVectors(wp, look).reflect(n).negate().add(wp);
      cam.position.copy(view);
      cam.up.set(0, 1, 0).applyMatrix4(rot).reflect(n);
      cam.lookAt(aim);
      cam.far = camera.far;
      cam.updateMatrixWorld();
      cam.projectionMatrix.copy(camera.projectionMatrix);
      tm.set(0.5, 0, 0, 0.5, 0, 0.5, 0, 0.5, 0, 0, 0.5, 0.5, 0, 0, 0, 1).multiply(cam.projectionMatrix).multiply(cam.matrixWorldInverse).multiply(mesh.matrixWorld);
      // the oblique near plane: the mirror camera sees nothing under the water
      plane.setFromNormalAndCoplanarPoint(n, wp).applyMatrix4(cam.matrixWorldInverse);
      clip.set(plane.normal.x, plane.normal.y, plane.normal.z, plane.constant);
      var e = cam.projectionMatrix.elements;
      q.set((Math.sign(clip.x) + e[8]) / e[0], (Math.sign(clip.y) + e[9]) / e[5], -1, (1 + e[10]) / e[14]);
      clip.multiplyScalar(2 / clip.dot(q));
      e[2] = clip.x; e[6] = clip.y; e[10] = clip.z + 1; e[14] = clip.w;
      renderer.getDrawingBufferSize(size);
      var w = Math.max(2, Math.round(size.x * scale)), h = Math.max(2, Math.round(size.y * scale));
      if (target.width !== w || target.height !== h) target.setSize(w, h);
      mesh.visible = false;
      var was = renderer.getRenderTarget(), auto = renderer.shadowMap.autoUpdate;
      renderer.shadowMap.autoUpdate = false;
      renderer.setRenderTarget(target);
      renderer.state.buffers.depth.setMask(true);
      if (renderer.autoClear === false) renderer.clear();
      renderer.render(scene, cam);
      renderer.shadowMap.autoUpdate = auto;
      renderer.setRenderTarget(was);
      if (camera.viewport !== undefined) renderer.state.viewport(camera.viewport);
      mesh.visible = true;
      mesh.userData.drawn = (mesh.userData.drawn || 0) + 1;
    };
    return mesh;
  }

  /* pondMesh is the pond's water, a disc on its level, in a mode of the quality tiers (QUALITY.TIERS): "mirror", the
     mirror; "tint", a flat tinted mirror colour, the sky's horizon over the water's own dark green (sky() keeps it the sky's
     as the light moves); "water", the water's own colour lit by the sun or the moon, no reflection. Each is fogged with the
     land. */
  var WATER = 0x24363a, MIRRORED = 0.55;
  function pondMesh(THREE, pond, mode, p) {
    var g = new THREE.CircleGeometry(pond.r, 40), mesh, m;
    var tint = new THREE.Color(WATER), sky = new THREE.Color(p ? p.horizon : 0x9ab4c8);
    if (mode === "mirror") mesh = mirror(THREE, g, { tint: tint, scale: 0.5 });
    else if (mode === "water") {
      m = new THREE.MeshLambertMaterial({ color: tint.clone().multiplyScalar(1.3) });
      m.onBeforeCompile = L.heightFog;
      m.customProgramCacheKey = function () { return "farm-pond-water"; };
      mesh = new THREE.Mesh(g, m);
    } else {
      m = new THREE.MeshBasicMaterial({ color: tint.clone().lerp(sky, MIRRORED) });
      m.onBeforeCompile = function (sh) { sh.vertexShader = sh.vertexShader.replace("#include <common>", "#include <common>\nvarying vec3 vViewPosition;").replace("#include <project_vertex>", "#include <project_vertex>\nvViewPosition = -mvPosition.xyz;"); sh.fragmentShader = sh.fragmentShader.replace("#include <common>", "#include <common>\nvarying vec3 vViewPosition;"); L.heightFog(sh); };
      m.customProgramCacheKey = function () { return "farm-pond-flat"; };
      mesh = new THREE.Mesh(g, m);
      mesh.userData.sky = sky;
      mode = "tint";
    }
    mesh.rotation.x = -Math.PI / 2;
    mesh.position.set(pond.x, pond.y, pond.z);
    mesh.receiveShadow = true;
    mesh.name = "pond";
    mesh.userData.reflect = mode === "mirror";
    mesh.userData.mode = mode;
    return mesh;
  }

  /* pond(THREE, group, mode, p) makes the farm's pond over in mode (pondMesh's) under palette p, letting the old one's
     geometry, material and mirror's target go; it returns the pond. The same mode leaves it be. */
  function pond(THREE, group, mode, p) {
    var old = group.getObjectByName("pond");
    if (old.userData.mode === mode) return old;
    var mesh = pondMesh(THREE, group.userData.pond, mode, p);
    mesh.layers.mask = old.layers.mask;
    group.remove(old);
    old.geometry.dispose();
    old.material.dispose();
    if (old.userData.target) old.userData.target.dispose();
    group.add(mesh);
    return mesh;
  }

  /* sky keeps a tinted pond the colour of palette p's horizon over the water as the light moves. */
  function sky(group, p) {
    var m = group.getObjectByName("pond");
    if (m && m.userData.mode === "tint") m.material.color.set(WATER).lerp(m.userData.sky.set(p.horizon), MIRRORED);
  }

  /* reflected puts obj and everything under it on the mirror's layer but the parts named in skip (and theirs): the near
     things the pond, far off and low, can never show. */
  function reflected(obj, skip) {
    skip = skip || [];
    (function walk(o) {
      if (skip.indexOf(o.name) >= 0) return;
      o.layers.enable(REFLECT);
      o.children.forEach(walk);
    })(obj);
  }

  /* build(THREE, doc, opts) -> the farm, a Group: the meshes of the buildings, the walls and the yard by material (NAMES); the lit windows, the windows' and the lamp's halos and the lamp's pool of light; the
     coping, bales, posts, rails, cattle and sheep; the pond; the smoke; the swifts and the bats. opts.t (unix ms), opts.lat
     and opts.lon set the hour (hours()); opts.palette (ALMANAC.skyPalette) the smoke's and the flat pond's colours;
     opts.pond is the pond's mode (pondMesh(): "mirror", "tint" or "water"; without it opts.reflect draws the mirror, else the
     tint). */
  function build(THREE, doc, opts) {
    opts = opts || {};
    uniforms = { uTime: { value: 0 }, uWind: { value: new THREE.Vector3(FL.WIND.dir[0], FL.WIND.dir[1], FL.WIND.strength) } };
    var lay = layout(), group = new THREE.Group(), p = opts.palette;
    group.name = "farm";
    var ms = { stone: mesher(2), slate: mesher(1), timber: mesher(2), paint: mesher(1), glass: mesher(1), dark: mesher(1), hay: mesher(1), lamp: mesher(1), lit: mesher(1), wall: mesher(2) };
    house(ms, lay.house);
    var lampAt = barn(ms, lay.barn);
    yard(ms, lay.well, lay.cart);
    lay.walls.forEach(function (run) { wallMesh(ms.wall, run); });
    gateMesh(ms, lay.gate);
    bridgeMesh(ms.timber, lay.bridge);
    windowMeshes(ms, lay.windows);
    var mats = {
      stone: material(THREE, doc, 0xb4a88e, function () { return stonePixels(128, [196, 182, 156], 0.012, 0.62, SEED + 20); }, 0.92, 0.012),
      wall: material(THREE, doc, 0x8f8a7e, function () { return stonePixels(128, [160, 154, 140], 0.03, 0.16, SEED + 21); }, 0.95, 0.02),
      slate: material(THREE, doc, 0x4c525c, function () { return slatePixels(128, SEED + 22); }, 0.7, 0.004),
      timber: material(THREE, doc, 0x7e705e, function () { return timberPixels(128, SEED + 23); }, 0.88, 0.006),
      paint: material(THREE, null, 0xffffff, null, 0.75),
      glass: new THREE.MeshStandardMaterial({ color: 0x2a3640, roughness: 0.15, metalness: 0.1, vertexColors: true }),
      dark: material(THREE, null, 0xffffff, null, 0.95),
      hay: material(THREE, null, 0xd2b660, null, 1),
      lamp: new THREE.MeshBasicMaterial({ color: 0x3a3226, vertexColors: true }),
      lit: new THREE.MeshBasicMaterial({ color: GLOW, vertexColors: true, toneMapped: false }) // lamplight, not tone mapped down to beige
    };
    mats.glass.onBeforeCompile = L.heightFog;
    mats.lamp.onBeforeCompile = mats.lit.onBeforeCompile = basicFog;
    Object.keys(ms).forEach(function (k) {
      if (!ms[k].count()) return;
      var mesh = new THREE.Mesh(ms[k].geometry(THREE), mats[k]);
      mesh.name = NAMES[k];
      mesh.castShadow = k !== "lit" && k !== "glass";
      mesh.receiveShadow = true;
      group.add(mesh);
    });

    // the halos round the lit windows and the lamp, and the lamp's pool on the ground before the barn's doors
    var halo = [];
    lay.windows.forEach(function (w) { if (w.on) halo.push(w.at[0] + w.out[0] * 0.3, w.at[1], w.at[2] + w.out[1] * 0.3); });
    halo.push(lampAt[0], lampAt[1], lampAt[2]);
    var hg = new THREE.BufferGeometry();
    hg.setAttribute("position", new THREE.Float32BufferAttribute(halo, 3));
    var halos = new THREE.Points(hg, new THREE.PointsMaterial({ color: GLOW, size: 2.6, sizeAttenuation: true, transparent: true, opacity: 0.5, blending: THREE.AdditiveBlending, depthWrite: false, fog: false, map: doc ? glowTexture(THREE, doc) : null }));
    halos.material.onBeforeCompile = function (sh) { metres(sh); };
    halos.material.customProgramCacheKey = function () { return "farm-halos"; };
    halos.name = "halos";
    var pool = new THREE.Mesh(new THREE.CircleGeometry(3.2, 24), new THREE.MeshBasicMaterial({ color: GLOW, transparent: true, opacity: 0.35, blending: THREE.AdditiveBlending, depthWrite: false, fog: false, map: doc ? glowTexture(THREE, doc, true) : null }));
    pool.rotation.x = -Math.PI / 2;
    pool.position.set(lampAt[0], FL.ground(lampAt[0], lampAt[2]) + 0.06, lampAt[2]);
    pool.name = "lamp pool";
    group.add(halos, pool);

    // the walls' coping, the bales, the fences and what grazes in them
    var cp = lay.coping, wallMat = material(THREE, doc, 0x8f8a7e, function () { return stonePixels(128, [160, 154, 140], 0.03, 0.16, SEED + 21); }, 0.95, 0.02, true);
    group.add(instances(THREE, unitBox(THREE, 0), wallMat, cp.length / 6, "coping", function (i, mx, c) {
      var k = i * 6;
      along(THREE, mx, cp[k], cp[k + 1], cp[k + 2], cp[k + 3], 0, cp[k + 4], 0.12 + 0.06 * L.hash(i, 2, SEED + 11), 0.2 + 0.12 * L.hash(i, 3, SEED + 11), 0.42, cp[k + 5]);
      var v = 0.75 + 0.3 * L.hash(i, 4, SEED + 11); c.setRGB(v, v, v * 0.96);
    }));
    var bl = lay.bales, baleGeo = new THREE.CylinderGeometry(1, 1, 1, 16, 1);
    var baleMat = material(THREE, null, 0xffffff, null, 1, 0, true);
    group.add(instances(THREE, baleGeo, baleMat, bl.length / 7, "bales", function (i, mx, c) {
      var k = i * 7;
      // the cylinder's axis (its y) along the bale's axis
      var ax = new THREE.Vector3(bl[k + 3], 0, bl[k + 4]), q = new THREE.Quaternion().setFromUnitVectors(new THREE.Vector3(0, 1, 0), ax);
      mx.compose(new THREE.Vector3(bl[k], bl[k + 1], bl[k + 2]), q, new THREE.Vector3(bl[k + 5], bl[k + 6], bl[k + 5]));
      var v = L.hash(i, 1, SEED + 12); c.setRGB(0.8 + 0.12 * v, 0.68 + 0.1 * v, 0.38 + 0.06 * v);
    }));
    fences(THREE, material(THREE, doc, 0x7e705e, function () { return timberPixels(128, SEED + 23); }, 0.88, 0.006, true), lay).forEach(function (m) { group.add(m); });
    group.add(beastMesh(THREE, lay.beasts, 0, "cattle"), beastMesh(THREE, lay.beasts, 1, "sheep"));

    group.userData.pond = lay.pond;
    group.add(pondMesh(THREE, lay.pond, opts.pond || (opts.reflect ? "mirror" : "tint"), p));
    group.add(smokeMesh(THREE, doc, lay.chimneys, p));
    var sw = birdMesh(THREE, lay.swifts, 0, "swifts"), bt = birdMesh(THREE, lay.bats, 1, "bats");
    group.add(sw, bt);
    group.userData.update = function (t) { fly(THREE, sw, t); fly(THREE, bt, t + 7); };
    group.userData.update(0);
    set(group, hours(opts.t == null ? Date.now() : opts.t, opts.lat, opts.lon));
    return group;
  }

  /* set shows what the farm does at an hour (an object of hours()): the lit windows, their halos and the lamp's pool, the
     lamp's bulb, the smoke, the swifts and the bats. */
  function set(group, h) {
    ["lit windows", "halos", "lamp pool"].forEach(function (n) { var o = group.getObjectByName(n); if (o) o.visible = h.lit; });
    var lamp = group.getObjectByName("lamp");
    if (lamp) lamp.material.color.set(h.lit ? GLOW : 0x3a3226).multiplyScalar(h.lit ? 1.8 : 1);
    group.getObjectByName("smoke").visible = h.smoke;
    group.getObjectByName("swifts").visible = h.swifts;
    group.getObjectByName("bats").visible = h.bats;
    group.userData.hours = h;
  }

  /* update moves the smoke, the grazing and the birds to t seconds of the page's own clock. */
  function update(group, t) {
    if (uniforms) uniforms.uTime.value = t;
    if (group && group.userData.update) group.userData.update(t);
  }

  return {
    SEED: SEED, CIVIL: CIVIL, SET: SET, HOUSE: HOUSE, BARN: BARN, WALL: WALL, BALE: BALE, PADDOCKS: PADDOCKS, COWS: COWS, SHEEP: SHEEP, SWIFTS: SWIFTS, BATS: BATS, REFLECT: REFLECT,
    isLit: isLit, smoking: smoking, hours: hours, frame: frame, P: P, layout: layout, roofY: roofY, deckY: deckY, inside: inside, crossing: crossing,
    stonePixels: stonePixels, slatePixels: slatePixels, timberPixels: timberPixels, mirror: mirror, reflected: reflected, build: build, pond: pond, sky: sky, set: set, update: update
  };
})();
if (typeof module !== "undefined") module.exports = FARM;
