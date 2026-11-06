/* FLORA: what grows on the splash's land (plan gd-EJGA3zDGCk, step 5). The near meadow between the camera and the crest is
   thick with grass, an InstancedMesh of GRASS two-triangle blades scattered by the field map's grass weight (the splat's red,
   so none grows on the path or the lane), each bent by a slow wind field in its vertex shader and its tip tinted warm; among
   them wildflowers (poppy, cornflower, daisy, buttercup) as crossed-quad sprites whose petals are drawn in code, thickest
   along the path to the stone and the foot of the hedge. A hedgerow runs up the lane's verge on the left with a great oak
   in it, its boughs reaching over the top of the frame to close the picture; more hedgerows, rows of leafy clumps, line the
   valley's furlongs. Trees in three species: the oak's round crown, a line of poplars along the river, apple trees in the
   orchard's rows, each a tapered trunk and a few branches under a canopy of alpha-tested leaf cards that move in the wind.
   The woods on the far slopes and the ridges are billboarded impostors fading into the land's haze, and a dozen rooks
   circle over them.

   Where things are is pure and seeded, like LAND: layout() places every blade, flower, clump, tree and rook with only
   + - * /, Math.floor, Math.sqrt and Math.imul on LAND's heights, so it hashes the same everywhere; TestFloraUnderNode pins
   it. What can be seen from the camera is decided by marching the eye's ray over the heightfield, so nothing is planted
   behind the near hill. The textures (the leaf, flower and impostor atlases, the bark) are drawn on canvases when there is a
   document; under node the materials are plain. Every mesh casts and receives shadow but the impostors, which only
   receive. update(seconds) moves the wind and the rooks; with the clock pinned the scene draws one still. */
var FLORA = (function () {
  "use strict";
  var L = typeof LAND !== "undefined" ? LAND : require("./land.js");
  var SEED = 20261027, FOV = 50;
  // the near meadow: from just past the crest to under the camera, as wide as a desktop's view and a little more
  var GRASS = 60000, FLOWERS = 2600, MEADOW = { z0: L.CREST_Z - 6, z1: L.CAM_Z + 1, w0: 3, spread: 0.95 };
  var BLADE = { h: [0.22, 0.58], w: [0.035, 0.075] }, FLOWER = { h: [0.2, 0.42] };
  // the hedge up the lane's east verge (the lane: LAND.LANE), the oak standing in it 13 m ahead and 9 m left of the eye
  var VERGE = [[-12.6, 186], [-12.2, 172], [-11.4, 164], [-10.0, 157], [-9.2, 150], [-9.6, 142], [-11.5, 134]];
  var OAK = { x: -8.8, z: 157 };
  var CLUMP = 2.0, NEAR_CLUMP = 1.1; // spacing of the valley's hedge clumps and the verge's, m
  var ROOKS = 12, NEAREST = 90; // the impostors stand north of z NEAREST, 80 m and more from the eye
  var WIND = { dir: [0.8, -0.6], strength: 0.42 }; // from the south-west, the prevailing wind, in the scene's x and z

  function clamp(x, lo, hi) { return x < lo ? lo : x > hi ? hi : x; }
  function smooth(e0, e1, x) { var k = clamp((x - e0) / (e1 - e0), 0, 1); return k * k * (3 - 2 * k); }
  function lerp(a, b, t) { return a + (b - a) * t; }
  /* rnd is LAND's hash as a stream keyed by (i, k): the k-th number of item i. */
  function rnd(seed) { return function (i, k) { return L.hash(i, k, seed); }; }

  /* ---- the ground, as the land's mesh has it ---- */

  var hs = null, ringCache = {};
  /* ground is the height of the land's drawn surface at x, z: the 257-vertex field's triangles inside it, the far ring's
     25 m triangles beyond (FAR_DROP under the height function), so what stands on it stands on what is drawn. */
  function ground(x, z) {
    var half = L.SIZE / 2;
    if (x > -half && x < half && z > -half && z < half) {
      if (!hs) hs = L.heightfield(L.N, L.SIZE);
      var step = L.SIZE / (L.N - 1), u = (x + half) / step, v = (z + half) / step, i = Math.floor(u), j = Math.floor(v);
      i = Math.min(i, L.N - 2); j = Math.min(j, L.N - 2);
      return tri(u - i, v - j, hs[j * L.N + i], hs[j * L.N + i + 1], hs[(j + 1) * L.N + i], hs[(j + 1) * L.N + i + 1]);
    }
    var c = L.FAR_CELL, fu = (x + L.FAR) / c, fv = (z + L.FAR) / c, fi = Math.floor(fu), fj = Math.floor(fv);
    return tri(fu - fi, fv - fj, far(fi, fj), far(fi + 1, fj), far(fi, fj + 1), far(fi + 1, fj + 1));
  }
  function far(i, j) {
    var k = i + "," + j;
    if (!(k in ringCache)) ringCache[k] = L.height(-L.FAR + i * L.FAR_CELL, -L.FAR + j * L.FAR_CELL) - L.FAR_DROP;
    return ringCache[k];
  }
  /* tri interpolates a cell's corners a (i, j), b (i+1, j), c (i, j+1), d (i+1, j+1) on the triangles a c b and b c d. */
  function tri(fu, fv, a, b, c, d) { return fu + fv < 1 ? a + (b - a) * fu + (c - a) * fv : d + (c - d) * (1 - fu) + (b - d) * (1 - fv); }

  var eye = null;
  /* seen says whether the point x, y, z can be seen from the camera's eye: no ground on the ray between rises above it. */
  function seen(x, y, z) {
    if (!eye) eye = L.camera(FOV).eye;
    var dx = x - eye[0], dy = y - eye[1], dz = z - eye[2], d = Math.sqrt(dx * dx + dz * dz), n = Math.ceil(d / 2), half = L.SIZE / 2;
    for (var s = 1; s < n; s++) {
      var t = s / n, px = eye[0] + dx * t, pz = eye[2] + dz * t;
      if (px <= -half || px >= half || pz <= -half || pz >= half) continue; // the near hill is all inside the field
      if (ground(px, pz) > eye[1] + dy * t + 0.05) return false;
    }
    return true;
  }

  /* ---- lines on the map ---- */

  /* curve turns [x, z] points into the polyline LAND's painter strokes through them (quadratic curves through the midpoints),
     a point every step m or so. */
  function curve(pts, step) {
    var out = [pts[0]], prev = pts[0];
    for (var i = 1; i < pts.length - 1; i++) {
      var c = pts[i], e = [(pts[i][0] + pts[i + 1][0]) / 2, (pts[i][1] + pts[i + 1][1]) / 2];
      var len = Math.sqrt((c[0] - prev[0]) * (c[0] - prev[0]) + (c[1] - prev[1]) * (c[1] - prev[1])) + Math.sqrt((e[0] - c[0]) * (e[0] - c[0]) + (e[1] - c[1]) * (e[1] - c[1]));
      var n = Math.max(2, Math.ceil(len / step));
      for (var k = 1; k <= n; k++) {
        var t = k / n, a = (1 - t) * (1 - t), b = 2 * t * (1 - t), d = t * t;
        out.push([a * prev[0] + b * c[0] + d * e[0], a * prev[1] + b * c[1] + d * e[1]]);
      }
      prev = e;
    }
    out.push(pts[pts.length - 1]);
    return out;
  }
  /* dist is the distance from x, z to the polyline pl. */
  function dist(pl, x, z) {
    var best = Infinity;
    for (var i = 0; i + 1 < pl.length; i++) {
      var ax = pl[i][0], az = pl[i][1], bx = pl[i + 1][0] - ax, bz = pl[i + 1][1] - az, l = bx * bx + bz * bz;
      var t = l > 0 ? clamp(((x - ax) * bx + (z - az) * bz) / l, 0, 1) : 0, ex = x - ax - bx * t, ez = z - az - bz * t;
      var d = ex * ex + ez * ez;
      if (d < best) best = d;
    }
    return Math.sqrt(best);
  }
  var lines = null;
  function paths() {
    if (!lines) {
      lines = { path: curve(L.PATH, 0.5), lane: curve(L.LANE, 1), verge: curve(VERGE, 0.5) };
      lines.nearLane = lines.lane.filter(function (q) { return q[1] > MEADOW.z0 - 12; }); // the meadow's stretch of it
    }
    return lines;
  }

  /* meadow is the grass's weight at x, z from the map's own lines, as the splat paints it: none on the path, the lane or the
     stone's worn ring, some at the path's edge. In a browser the splat itself is read instead (build). */
  function meadow(x, z) {
    var p = paths(), dp = dist(p.path, x, z), dl = dist(p.nearLane, x, z);
    var ds = Math.sqrt((x - L.STONE.x) * (x - L.STONE.x) + (z - L.STONE.z) * (z - L.STONE.z));
    return Math.min(smooth(0.22, 0.6, dp), smooth(1.7, 2.6, dl), smooth(1.2, 1.8, ds));
  }
  /* clear keeps the plinth's footprint bare whatever the weight says, so no blade comes up through the stone. */
  function clear(x, z) { return smooth(1.0, 1.3, Math.sqrt((x - L.STONE.x) * (x - L.STONE.x) + (z - L.STONE.z) * (z - L.STONE.z))); }

  /* ---- layout: where everything grows ---- */

  /* inMeadow picks a point of the near meadow for draw i of a stream r, uniform over its area: [x, z], or null. */
  function inMeadow(r, i) {
    var m = MEADOW, d = (m.z1 - m.z0) * r(i, 1), z = m.z1 - d, w = m.w0 + m.spread * (L.CAM_Z + 1 - z), wmax = m.w0 + m.spread * (m.z1 - m.z0 + 1);
    var x = (r(i, 2) * 2 - 1) * wmax;
    return Math.abs(x) <= w ? [x, z] : null;
  }

  /* grass(n, weight) -> Float32Array of n blades, 8 numbers each: x, y, z of its root, its turn (radians), height, width,
     its wind phase in [0, 1) and its shade in [0, 1) (a dry blade near 1). weight(x, z) in [0, 1] is the grass's share. */
  function grass(n, weight) {
    weight = weight || meadow;
    var r = rnd(SEED + 1), out = new Float32Array(n * 8), got = 0;
    for (var i = 0; got < n && i < n * 6; i++) {
      var p = inMeadow(r, i);
      if (!p || r(i, 3) >= weight(p[0], p[1]) * clear(p[0], p[1])) continue;
      var k = got * 8, tall = r(i, 4);
      out[k] = p[0]; out[k + 1] = ground(p[0], p[1]); out[k + 2] = p[1];
      out[k + 3] = r(i, 5) * 6.2831853;
      out[k + 4] = lerp(BLADE.h[0], BLADE.h[1], tall * tall) * (0.8 + 0.4 * L.noise(p[0] / 3, p[1] / 3, SEED + 2)); // in tussocks
      out[k + 5] = lerp(BLADE.w[0], BLADE.w[1], r(i, 6));
      out[k + 6] = r(i, 7);
      out[k + 7] = r(i, 8);
      got++;
    }
    return got < n ? out.subarray(0, got * 8) : out;
  }

  var KINDS = ["poppy", "cornflower", "daisy", "buttercup"];
  // the mix by where a flower grows: along the path, at the hedge's foot, out in the meadow
  var MIX = { path: [0.12, 0.13, 0.4, 0.35], hedge: [0.38, 0.32, 0.12, 0.18], meadow: [0.2, 0.15, 0.3, 0.35] };
  /* flowerWeight is how thickly flowers grow at x, z: a few everywhere in the grass, many in a band either side of the path
     and along the hedge's foot. It returns [weight, where]. */
  function flowerWeight(x, z, weight) {
    var g = (weight || meadow)(x, z) * clear(x, z);
    if (g <= 0) return [0, "meadow"];
    var p = paths(), dp = dist(p.path, x, z), dh = dist(p.verge, x, z);
    var path = smooth(0.35, 0.7, dp) * (1 - smooth(1.6, 3.2, dp)), hedge = smooth(1.0, 1.6, dh) * (1 - smooth(2.6, 4.5, dh));
    var w = g * (0.05 + 0.95 * Math.max(path, hedge)) * (dh < 1.0 ? 0 : 1);
    return [w, path > hedge ? (path > 0.2 ? "path" : "meadow") : hedge > 0.2 ? "hedge" : "meadow"];
  }
  /* flowers(n, weight) -> Float32Array of up to n flowers, 6 numbers each: x, y, z, turn, height, kind (an index of KINDS). */
  function flowers(n, weight) {
    var r = rnd(SEED + 3), out = new Float32Array(n * 6), got = 0;
    for (var i = 0; got < n && i < n * 40; i++) {
      var p = inMeadow(r, i);
      if (!p) continue;
      var fw = flowerWeight(p[0], p[1], weight);
      if (r(i, 3) >= fw[0]) continue;
      var mix = MIX[fw[1]], pick = r(i, 4), kind = 0;
      while (kind < 3 && pick >= mix[kind]) { pick -= mix[kind]; kind++; }
      var k = got * 6;
      out[k] = p[0]; out[k + 1] = ground(p[0], p[1]); out[k + 2] = p[1];
      out[k + 3] = r(i, 5) * 6.2831853;
      out[k + 4] = lerp(FLOWER.h[0], FLOWER.h[1], r(i, 6));
      out[k + 5] = kind;
      got++;
    }
    return out.subarray(0, got * 6);
  }

  /* hedges() -> Float32Array of hedge clumps, 7 numbers each: x, y, z, turn, width, height, near (1 on the lane's verge, 0 in
     the valley). The valley's run along every furlong's edge and round the orchard, with gaps where the river, the lane, the
     pond and the farmyard are, and only where the eye can see them; the verge's run from behind the camera over the crest. */
  function hedges() {
    var r = rnd(SEED + 4), out = [], n = 0, p = paths();
    function clump(x, z, w, h, near) {
      var y = ground(x, z);
      if (!near && !seen(x, y + h, z)) return;
      out.push(x, y, z, r(n, 9) * 6.2831853, w, h, near ? 1 : 0);
      n++;
    }
    function run(ax, az, bx, bz, spacing, near) {
      var len = Math.sqrt((bx - ax) * (bx - ax) + (bz - az) * (bz - az)), m = Math.max(1, Math.floor(len / spacing));
      var nx = -(bz - az) / len, nz = (bx - ax) / len;
      for (var k = 0; k < m; k++) {
        var t = (k + 0.5 + (r(n, 1) - 0.5) * 0.5) / m, o = (r(n, 2) - 0.5) * (near ? 0.5 : 0.8);
        var x = ax + (bx - ax) * t + nx * o, z = az + (bz - az) * t + nz * o;
        if (!near && gap(x, z)) { n++; continue; }
        var big = r(n, 3);
        clump(x, z, near ? 1.5 + big * 0.9 : 2.2 + big * 1.2, near ? 1.5 + r(n, 4) * 1.1 : 1.7 + r(n, 4) * 1.4, near);
      }
    }
    function gap(x, z) {
      var dr = Math.abs(z - L.river(x)), dp = Math.sqrt((x - L.POND.x) * (x - L.POND.x) + (z - L.POND.z) * (z - L.POND.z));
      var df = Math.sqrt((x - L.FARM.x) * (x - L.FARM.x) + (z - L.FARM.z) * (z - L.FARM.z)), o = L.ORCHARD;
      var inOrchard = x > o.x0 + 1 && x < o.x1 - 1 && z > o.z0 + 1 && z < o.z1 - 1;
      return dr < 6 || dp < L.POND.r + 3 || df < L.FARM.r || inOrchard || dist(p.lane, x, z) < 3.2; // the farmstead (farm.js) has walls
    }
    var seenEdge = {};
    L.fields().forEach(function (f) {
      for (var e = 0; e < 4; e++) {
        var a = f.quad[e], b = f.quad[(e + 1) % 4], key = [a, b].map(function (q) { return q[0].toFixed(2) + "," + q[1].toFixed(2); }).sort().join("|");
        if (seenEdge[key]) continue;
        seenEdge[key] = true;
        run(a[0], a[1], b[0], b[1], CLUMP, false);
      }
    });
    var o = L.ORCHARD;
    [[o.x0, o.z0, o.x1, o.z0], [o.x1, o.z0, o.x1, o.z1], [o.x1, o.z1, o.x0, o.z1], [o.x0, o.z1, o.x0, o.z0]].forEach(function (s) { run(s[0], s[1], s[2], s[3], CLUMP, false); });
    for (var i = 0; i + 1 < p.verge.length; i++) {
      var a = p.verge[i], b = p.verge[i + 1];
      if (Math.sqrt((a[0] - OAK.x) * (a[0] - OAK.x) + (a[1] - OAK.z) * (a[1] - OAK.z)) < 1.2) continue; // the oak's trunk
      run(a[0], a[1], b[0], b[1], NEAR_CLUMP, true);
    }
    return new Float32Array(out);
  }

  /* ---- trees ---- */

  /* unit is a seeded direction, roughly uniform, from the stream r at item i (no trigonometry, so it hashes the same). */
  function unit(r, i, k) {
    for (var t = 0; t < 8; t++) {
      var x = r(i, k + t * 3) * 2 - 1, y = r(i, k + t * 3 + 1) * 2 - 1, z = r(i, k + t * 3 + 2) * 2 - 1, l = x * x + y * y + z * z;
      if (l > 0.01 && l <= 1) { l = Math.sqrt(l); return [x / l, y / l, z / l]; }
    }
    return [0, 1, 0];
  }
  function norm(v) { var l = Math.sqrt(v[0] * v[0] + v[1] * v[1] + v[2] * v[2]) || 1; return [v[0] / l, v[1] / l, v[2] / l]; }

  /* The species: the trunk's height to its first fork and its radius at the foot; how many limbs each fork throws, how long
     and how thick they are against their parent, how much they turn up, out and toward the lean, curl up along their length,
     kink at a joint and droop; the leaf clusters at the ends of the last forks (cards each, their radius and the card's size). */
  var SEG = 3;
  var SPECIES = {
    oak: { trunk: 2.9, radius: 0.58, forks: [5, 3, 3], len: [6.8, 3.4, 2.0], thin: 0.55, up: [0.0, 0.12, 0.25], spread: [0.9, 0.6, 0.7], lean: [0.4, 0, 0.26], curl: 0.16, kink: 0.18, droop: 0.16, cluster: [46, 1.9, 1.25], cell: 0 },
    poplar: { trunk: 2.2, radius: 0.32, spindle: [20, 2.4], cluster: [6, 1.3, 1.4], cell: 1 },
    apple: { trunk: 1.2, radius: 0.15, forks: [4, 3], len: [1.6, 1.1], thin: 0.55, up: [0.45, 0.5], spread: [1.1, 0.8], lean: [0, 0, 0], curl: 0.2, kink: 0.2, droop: 0.05, cluster: [7, 0.95, 0.85], cell: 2 }
  };

  /* grow(kind, x, z, seed, scale) -> {limbs, leaves}: a tree of SPECIES[kind] standing at x, z. limbs are tapered segments
     [x0, y0, z0, r0, x1, y1, z1, r1]; leaves are cards [x, y, z, size, ox, oy, oz, shade]: where, how big, the way out of the
     crown (its normal, so the crown is lit as one rounded mass) and how deep in the crown it is (0 deep, 1 out in the sun). */
  function grow(kind, x, z, seed, scale) {
    var sp = SPECIES[kind], r = rnd(seed), limbs = [], tips = [], id = 0, y0 = ground(x, z) - 0.25 * scale;
    var top = [x, y0 + (sp.trunk + 0.25) * scale, z];
    limbs.push([x, y0, z, sp.radius * scale, top[0], top[1], top[2], sp.radius * scale * 0.72]);
    if (sp.spindle) {
      // a poplar: a straight leader and short limbs up it, the leaves in a spindle round it
      var tall = sp.spindle[0] * scale, girth = sp.spindle[1] * scale, lead = [x, top[1] + tall, z];
      limbs.push([top[0], top[1], top[2], sp.radius * scale * 0.72, lead[0], lead[1], lead[2], 0.03]);
      for (var s = 0; s < 22; s++) {
        var h = (s + 0.5) / 22, at = top[1] + tall * h * 0.96, rad = girth * Math.sqrt(Math.max(0, h * (1 - h) * 4)) * (0.85 + 0.3 * r(s, 1));
        var u = unit(r, s, 2), ux = u[0], uz = u[2], ul = Math.sqrt(ux * ux + uz * uz) || 1;
        var tip = [x + ux / ul * rad * 0.8, at + rad * 0.5, z + uz / ul * rad * 0.8];
        limbs.push([x, at - rad * 0.3, z, 0.06 * scale * (1.2 - h), tip[0], tip[1], tip[2], 0.02]);
        tips.push([tip[0], tip[1], tip[2], 0.75 + 0.5 * h]);
        tips.push([x + (r(s, 30) - 0.5) * rad, at, z + (r(s, 31) - 0.5) * rad, 0.75 + 0.5 * h]);
      }
    } else {
      // each limb sets out from its parent toward the lean and curls up (or droops) along its SEG segments, kinked a little at
      // each joint, so a bough reads as grown, not as a stick
      var fork = function (from, dir, level, radius) {
        var n = sp.forks[level];
        for (var b = 0; b < n; b++) {
          id++;
          var u = unit(r, id, 1), lean = sp.lean, up = sp.up[level], spread = sp.spread[level];
          var d = norm([dir[0] * (1 - spread) + u[0] * spread + lean[0], dir[1] * (1 - spread) + Math.abs(u[1]) * spread * 0.5 + up, dir[2] * (1 - spread) + u[2] * spread + lean[2]]);
          // a bough leaves the trunk thinner than the trunk; a branch leaves its bough about as thick as the bough ends
          var len = sp.len[level] * scale * (0.75 + 0.5 * r(id, 30)), r0 = radius * (level ? 0.9 : 0.62), rr = r0 * sp.thin * (0.85 + 0.3 * r(id, 31)), at = from, ds = d;
          for (var g = 1; g <= SEG; g++) {
            var k = g / SEG, j = unit(r, id * 8 + g, 50);
            ds = norm([d[0] + j[0] * sp.kink, d[1] + sp.curl * k + j[1] * sp.kink, d[2] + j[2] * sp.kink]);
            var next = [at[0] + ds[0] * len / SEG, at[1] + ds[1] * len / SEG - sp.droop * len / SEG * level * k, at[2] + ds[2] * len / SEG];
            var rb = lerp(r0, rr, k); // each segment runs a little past its joint, so the bend shows no seam
            limbs.push([at[0], at[1], at[2], lerp(r0, rr, (g - 1) / SEG), next[0] + ds[0] * rb * 0.8, next[1] + ds[1] * rb * 0.8, next[2] + ds[2] * rb * 0.8, rb]);
            if (level + 1 === sp.forks.length && g === 2) tips.push([next[0], next[1] + 0.2 * scale, next[2], 0.7]);
            at = next;
          }
          if (level + 1 < sp.forks.length) fork(at, ds, level + 1, rr);
          else tips.push([at[0], at[1], at[2], 1]);
        }
      };
      fork(top, [0, 1, 0], 0, sp.radius * scale * 0.72);
    }
    // the crown's centre and size, for the leaves' way out and their depth in it
    var c = [0, 0, 0];
    tips.forEach(function (t) { c[0] += t[0]; c[1] += t[1]; c[2] += t[2]; });
    c = [c[0] / tips.length, c[1] / tips.length, c[2] / tips.length];
    var reach = 0;
    tips.forEach(function (t) { reach = Math.max(reach, Math.sqrt((t[0] - c[0]) * (t[0] - c[0]) + (t[1] - c[1]) * (t[1] - c[1]) * 1.6 + (t[2] - c[2]) * (t[2] - c[2]))); });
    var leaves = [], cl = sp.cluster;
    tips.forEach(function (t, ti) {
      var count = Math.round(cl[0] * t[3]);
      for (var k = 0; k < count; k++) {
        var q = unit(r, 5000 + ti * 64 + k, 1), rr = cl[1] * scale * Math.sqrt(r(5000 + ti * 64 + k, 40));
        var p = [t[0] + q[0] * rr, t[1] + q[1] * rr * 0.75, t[2] + q[2] * rr];
        var o = [p[0] - c[0], (p[1] - c[1]) + reach * 0.35, p[2] - c[2]], ol = Math.sqrt(o[0] * o[0] + o[1] * o[1] + o[2] * o[2]) || 1;
        var depth = clamp(Math.sqrt((p[0] - c[0]) * (p[0] - c[0]) + (p[1] - c[1]) * (p[1] - c[1]) + (p[2] - c[2]) * (p[2] - c[2])) / (reach || 1), 0, 1);
        var shade = clamp(0.25 + 0.75 * depth * depth + 0.25 * (p[1] - c[1]) / (reach || 1), 0, 1);
        leaves.push(p[0], p[1], p[2], cl[2] * scale * (0.8 + 0.4 * r(5000 + ti * 64 + k, 41)), o[0] / ol, o[1] / ol, o[2] / ol, shade);
      }
    });
    return { limbs: limbs, leaves: leaves, crown: c, reach: reach };
  }

  /* trees() -> [{kind, x, z, scale, seed}]: the oak in the hedge on the left; the poplars along the river's north bank,
     where their heads show over the near hill; the apple trees in the orchard's rows, as the painter set them. */
  function trees() {
    var r = rnd(SEED + 6), out = [{ kind: "oak", x: OAK.x, z: OAK.z, scale: 1, seed: SEED + 100 }];
    for (var x = -196, i = 0; x < 196; x += 7 + r(i, 1) * 3, i++) {
      var z = L.river(x) - 7 - r(i, 2) * 2, s = 0.9 + 0.25 * r(i, 3);
      if (seen(x, ground(x, z) + (SPECIES.poplar.trunk + SPECIES.poplar.spindle[0] * 0.8) * s, z)) out.push({ kind: "poplar", x: x, z: z, scale: s, seed: SEED + 200 + i });
    }
    var o = L.ORCHARD, k = 0;
    for (var oz = o.z0 + 3; oz < o.z1 - 1; oz += 6) for (var ox = o.x0 + 3; ox < o.x1 - 1; ox += 6, k++) {
      out.push({ kind: "apple", x: ox + (r(k, 4) - 0.5) * 0.6, z: oz + (r(k, 5) - 0.5) * 0.6, scale: 0.9 + 0.3 * r(k, 6), seed: SEED + 400 + k });
    }
    return out;
  }

  /* impostors() -> Float32Array, 6 numbers a tree: x, y, z of its foot, height, width, kind (0 round, 1 tall, 2 dark, 3
     broad): the woods on the far slopes and the ridges out to the ring (sparser with distance), trees in the copses and
     standards in the valley's hedgerows, where the eye can see them and no nearer than NEAREST. */
  function impostors() {
    var r = rnd(SEED + 7), out = [], n = 0, half = L.SIZE / 2;
    function tree(x, z, h, kind) {
      if (z > NEAREST) return; // nearer, a flat card would show for what it is
      var y = ground(x, z);
      if (seen(x, y + h * 0.9, z)) out.push(x, y - 0.6, z, h, h * (kind === 1 ? 0.55 : kind === 3 ? 0.95 : 0.75), kind);
      n++;
    }
    function kindOf(i) { var k = r(i, 7); return k < 0.5 ? 0 : k < 0.75 ? 1 : k < 0.85 ? 2 : 3; }
    // the woods: a jittered grid, 5.5 m in the field, wider in the ring the farther it is
    for (var z = -half; z < -60; z += 5.5) for (var x = -half; x < half; x += 5.5, n++) {
      var jx = x + r(n, 1) * 5, jz = z + r(n, 2) * 5;
      if (L.wooded(jx, jz) > 0.04) tree(jx, jz, 12 + 9 * r(n, 3), kindOf(n));
    }
    for (z = -620; z < -60; z += 11) for (x = -760; x < 760; x += 11, n++) {
      jx = x + r(n, 1) * 10; jz = z + r(n, 2) * 10;
      if (jx > -half && jx < half && jz > -half) continue;
      var spare = smooth(250, 700, Math.sqrt(jx * jx + (jz - L.CAM_Z) * (jz - L.CAM_Z)));
      if (L.wooded(jx, jz) > 0.04 && r(n, 4) > spare * 0.6) tree(jx, jz, 14 + 10 * r(n, 3), kindOf(n));
    }
    // the copses and the hedgerow standards
    L.fields().forEach(function (f, fi) {
      var q = f.quad;
      if (f.kind === "copse") {
        for (var k = 0; k < 18; k++) {
          var a = r(fi * 100 + k, 1), b = r(fi * 100 + k, 2);
          var px = lerp(lerp(q[0][0], q[1][0], a), lerp(q[3][0], q[2][0], a), b), pz = lerp(lerp(q[0][1], q[1][1], a), lerp(q[3][1], q[2][1], a), b);
          tree(px, pz, 11 + 7 * r(fi * 100 + k, 3), kindOf(fi * 100 + k));
        }
      }
      for (var e = 0; e < 2; e++) if (r(fi, 10 + e) < 0.45) {
        var t = 0.2 + 0.6 * r(fi, 12 + e), s = q[e * 2], u = q[e * 2 + 1], hx = lerp(s[0], u[0], t), hz = lerp(s[1], u[1], t);
        if (Math.abs(hz - L.river(hx)) > 6) tree(hx, hz, 10 + 6 * r(fi, 14 + e), 0);
      }
    });
    return new Float32Array(out);
  }

  /* rooks() -> Float32Array, 7 numbers a bird: the centre x, y, z of its circle over the far wood, its radius, its angular
     speed (rad/s, the sign its way round), its phase and its wingbeat phase. Two loose flocks. */
  function rooks() {
    var r = rnd(SEED + 8), out = [], flocks = [[-46, -150, 32], [64, -168, 40]];
    for (var i = 0; i < ROOKS; i++) {
      var f = flocks[i % 2], cx = f[0] + (r(i, 1) - 0.5) * 30, cz = f[1] + (r(i, 2) - 0.5) * 24;
      out.push(cx, ground(cx, cz) + f[2] + r(i, 3) * 14, cz, 12 + r(i, 4) * 22, (0.12 + 0.1 * r(i, 5)) * (i % 2 ? 1 : -1), r(i, 6) * 6.2831853, r(i, 7));
    }
    return new Float32Array(out);
  }

  var memo = null;
  /* layout(n, weight) -> everything placed: n blades (GRASS without it) and flowers in proportion, scattered by weight (the
     pure meadow without it), the hedges, the trees grown, the impostors and the rooks. */
  function layout(n, weight) {
    n = n || GRASS;
    if (!memo) memo = {
      hedges: hedges(), impostors: impostors(), rooks: rooks(),
      trees: trees().map(function (t) { var g = grow(t.kind, t.x, t.z, t.seed, t.scale); g.kind = t.kind; g.x = t.x; g.z = t.z; return g; })
    };
    return Object.assign({ grass: grass(n, weight), flowers: flowers(Math.round(FLOWERS * n / GRASS), weight) }, memo);
  }

  /* ---- the pictures, drawn in code ---- */

  /* atlas draws a canvas of cols x rows cells of size px, calling draw(ctx, cell, rng) in each with the origin at the cell's
     bottom middle, y up, one unit the cell's height. */
  function atlas(doc, cols, rows, w, h, cells, draw) {
    var cv = doc.createElement("canvas");
    cv.width = cols * w; cv.height = rows * h;
    var ctx = cv.getContext("2d"), r = rnd(SEED + 9), n = 0;
    for (var c = 0; c < cells; c++) {
      ctx.save();
      ctx.translate((c % cols) * w + w / 2, (rows - Math.floor(c / cols)) * h); // row 0 at the bottom, where uv v = 0 is
      ctx.scale(h, -h);
      draw(ctx, c, function () { return r(n++, c); }, w / h);
      ctx.restore();
    }
    return cv;
  }
  function ellipse(ctx, x, y, rx, ry, a, fill) { ctx.fillStyle = fill; ctx.beginPath(); ctx.ellipse(x, y, Math.max(rx, 1e-4), Math.max(ry, 1e-4), a, 0, Math.PI * 2); ctx.fill(); }
  function stroke(ctx, pts, width, col) {
    ctx.strokeStyle = col; ctx.lineWidth = width; ctx.lineCap = "round"; ctx.beginPath(); ctx.moveTo(pts[0], pts[1]);
    for (var i = 2; i < pts.length; i += 2) ctx.lineTo(pts[i], pts[i + 1]);
    ctx.stroke();
  }

  /* flowerAtlas: four tall cells, a poppy, a cornflower, a daisy and a buttercup, each a couple of stems with leaves and the
     flower heads drawn petal by petal. */
  function flowerAtlas(doc) {
    return atlas(doc, 4, 1, 128, 256, 4, function (ctx, kind, rr, aspect) {
      var heads = kind === 2 || kind === 3 ? 3 : 2, half = aspect / 2;
      for (var s = 0; s < heads; s++) {
        var x = (s - (heads - 1) / 2) * half * 0.55 + (rr() - 0.5) * 0.04, top = 0.62 + rr() * 0.3, bend = (rr() - 0.5) * 0.12;
        stroke(ctx, [x, 0, x + bend * 0.4, top * 0.5, x + bend, top], 0.012, "#3f6a26");
        for (var l = 0; l < 2; l++) { var ly = top * (0.15 + 0.3 * l), side = l ? 1 : -1; ellipse(ctx, x + side * 0.05, ly, 0.06, 0.012, side * 0.6, "#4c7a2c"); }
        var hx = x + bend, hy = top, sz = (kind === 0 ? 0.11 : kind === 1 ? 0.085 : kind === 2 ? 0.09 : 0.07) * (0.85 + rr() * 0.3);
        if (kind === 0) { // poppy: four broad scarlet petals cupped, a dark heart
          for (var p = 0; p < 4; p++) { var a = p * Math.PI / 2 + rr() * 0.4; ellipse(ctx, hx + Math.cos(a) * sz * 0.45, hy + Math.sin(a) * sz * 0.35, sz * 0.62, sz * 0.5, a, p % 2 ? "#d0201a" : "#e2321f"); }
          ellipse(ctx, hx, hy + sz * 0.2, sz * 0.5, sz * 0.25, 0, "rgba(255,110,80,0.5)");
          ellipse(ctx, hx, hy, sz * 0.2, sz * 0.2, 0, "#1d1410");
        } else if (kind === 1) { // cornflower: a ring of ragged blue florets, darker inside
          for (p = 0; p < 9; p++) { a = p * Math.PI * 2 / 9; ellipse(ctx, hx + Math.cos(a) * sz * 0.6, hy + Math.sin(a) * sz * 0.6, sz * 0.42, sz * 0.2, a, p % 2 ? "#3e64d6" : "#2f50c4"); }
          ellipse(ctx, hx, hy, sz * 0.38, sz * 0.38, 0, "#2a3c9a");
        } else if (kind === 2) { // daisy: white rays round a yellow disc, tipped pink
          for (p = 0; p < 14; p++) { a = p * Math.PI * 2 / 14; ellipse(ctx, hx + Math.cos(a) * sz * 0.6, hy + Math.sin(a) * sz * 0.6, sz * 0.48, sz * 0.12, a, p % 3 ? "#fbfaf4" : "#f6e6ea"); }
          ellipse(ctx, hx, hy, sz * 0.3, sz * 0.3, 0, "#f2c21c");
        } else { // buttercup: five glossy yellow petals, a bright gloss on each
          for (p = 0; p < 5; p++) { a = p * Math.PI * 2 / 5 + 0.3; ellipse(ctx, hx + Math.cos(a) * sz * 0.5, hy + Math.sin(a) * sz * 0.5, sz * 0.5, sz * 0.38, a, "#f2c80c"); }
          for (p = 0; p < 5; p++) { a = p * Math.PI * 2 / 5 + 0.3; ellipse(ctx, hx + Math.cos(a) * sz * 0.55, hy + Math.sin(a) * sz * 0.55, sz * 0.18, sz * 0.12, a, "rgba(255,250,200,0.8)"); }
          ellipse(ctx, hx, hy, sz * 0.18, sz * 0.18, 0, "#c99a08");
        }
      }
    });
  }

  /* leafAtlas: four square cells of leaf sprays, an oak's lobed leaves, a poplar's small diamond leaves pale beneath, an
     apple's ovals with fruit, and a hawthorn's for the hedges with a few haws. */
  function leafAtlas(doc) {
    var greens = [["#2f5a1e", "#3b6b24", "#467a2a", "#27501a"], ["#5d8a2a", "#6f9a34", "#8aad48", "#4f7a22"], ["#3e6e26", "#4a7d2c", "#5a8a34", "#356020"], ["#2c5220", "#386228", "#44702e", "#25461a"]];
    return atlas(doc, 2, 2, 256, 256, 4, function (ctx, cell, rr) {
      var g = greens[cell], count = [30, 46, 28, 60][cell];
      stroke(ctx, [0, 0.02, 0.02, 0.5, -0.05, 0.9], 0.02, "#4a3a28");
      for (var i = 0; i < count; i++) {
        var a = rr() * Math.PI * 2, d = Math.sqrt(rr()) * 0.4, x = Math.cos(a) * d, y = 0.5 + Math.sin(a) * d, ang = a + (rr() - 0.5) * 1.2, col = g[Math.floor(rr() * 4)];
        if (cell === 0) { // oak: a leaf and its lobes
          var len = 0.13 + rr() * 0.05;
          ellipse(ctx, x, y, len * 0.55, len * 0.24, ang, col);
          for (var lb = -2; lb <= 2; lb++) for (var sd = -1; sd <= 1; sd += 2) {
            var lx = x + Math.cos(ang) * lb * len * 0.2 - Math.sin(ang) * sd * len * 0.2, ly = y + Math.sin(ang) * lb * len * 0.2 + Math.cos(ang) * sd * len * 0.2;
            ellipse(ctx, lx, ly, len * 0.11, len * 0.09, ang, col);
          }
        } else if (cell === 1) { // poplar: small diamonds, some turned to show their pale side
          ellipse(ctx, x, y, 0.045, 0.04, ang, rr() < 0.25 ? "#b6c290" : col);
        } else if (cell === 2) { // apple: ovals
          ellipse(ctx, x, y, 0.07, 0.035, ang, col);
        } else { // hawthorn: small and dense
          ellipse(ctx, x, y, 0.04, 0.03, ang, col);
          ellipse(ctx, x + 0.025, y + 0.01, 0.025, 0.02, ang, col);
        }
      }
      if (cell === 2) for (i = 0; i < 5; i++) { // apples
        var fx = (rr() - 0.5) * 0.5, fy = 0.3 + rr() * 0.45;
        ellipse(ctx, fx, fy, 0.045, 0.045, 0, rr() < 0.5 ? "#b8261c" : "#c94b1e");
        ellipse(ctx, fx - 0.012, fy + 0.014, 0.014, 0.012, 0, "rgba(255,220,160,0.7)");
      }
      if (cell === 3) for (i = 0; i < 8; i++) ellipse(ctx, (rr() - 0.5) * 0.6, 0.2 + rr() * 0.6, 0.016, 0.016, 0, "#8e1a14"); // haws
    });
  }

  /* treeAtlas: the impostors' four silhouettes, a round-crowned tree, a tall one, a dark conifer and a broad one, their
     crowns lit from above and darker under. */
  function treeAtlas(doc) {
    return atlas(doc, 4, 1, 128, 128, 4, function (ctx, kind, rr) {
      stroke(ctx, [0, 0, 0, 0.35], 0.04, "#3a3024");
      var blobs = [[0, 0.58, 0.4, 0.36], [0, 0.55, 0.26, 0.42], [0, 0.5, 0.3, 0.45], [0, 0.52, 0.46, 0.3]][kind];
      var cols = kind === 2 ? ["#1c3018", "#23381c", "#2b4222"] : ["#2a4620", "#355428", "#406030"];
      for (var layer = 0; layer < 3; layer++) for (var i = 0; i < 26; i++) {
        var a = rr() * Math.PI * 2, d = Math.sqrt(rr()), x = blobs[0] + Math.cos(a) * d * blobs[2] * 0.8, y = blobs[1] + Math.sin(a) * d * blobs[3] * 0.8;
        if (kind === 2) x *= 1 - (y - 0.1) * 0.9;
        var s = (kind === 2 ? 0.07 : 0.1) * (1 - layer * 0.25);
        ellipse(ctx, x, y + layer * 0.03, s, s * 0.9, 0, layer === 0 ? cols[0] : layer === 1 ? cols[1] : y > blobs[1] ? cols[2] : cols[1]);
      }
    });
  }

  /* barkPixels(n) -> RGBA bytes of a tiling n x n bark: dark furrows running up between pale grey-brown ridges. */
  function barkPixels(n) {
    var px = new Uint8Array(n * n * 4);
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) {
      var u = i / n, v = j / n, k = (j * n + i) * 4;
      var ridge = L.noise(u * 16 + L.noise(u * 4, v * 4, SEED + 11, 4) * 3, v * 3, SEED + 10, 16, 3), fleck = L.hash(i, j, SEED + 12);
      var t = 0.45 + 0.55 * smooth(0.25, 0.7, ridge) + (fleck - 0.5) * 0.12;
      px[k] = Math.round(clamp(92 * t, 0, 255)); px[k + 1] = Math.round(clamp(84 * t, 0, 255)); px[k + 2] = Math.round(clamp(72 * t, 0, 255)); px[k + 3] = 255;
    }
    return px;
  }

  /* ---- three.js ---- */

  // the uniforms shared by every swaying material: seconds, the wind's way (x, z) and strength, and the glow of the light
  // through a leaf (light() sets it from the almanac's palette)
  var uniforms = null, GLOW = 0.06;
  var WIND_GLSL = [
    "uniform float uTime;",
    "uniform vec3 uWind;",
    "float floraGust(vec2 p) {",
    "  float s = dot(p, uWind.xy);",
    "  return 0.55 + 0.3 * sin(s * 0.19 - uTime * 1.35) + 0.15 * sin(s * 0.53 + p.x * 0.11 - uTime * 2.3);",
    "}",
    "vec3 floraLocal(vec3 off) {", // a world offset in an instance's frame (its matrix a turn, a scale and a move)
    "  mat3 m = mat3(instanceMatrix);",
    "  return (transpose(m) * off) / vec3(dot(m[0], m[0]), dot(m[1], m[1]), dot(m[2], m[2]));",
    "}"
  ].join("\n");

  /* patch returns an onBeforeCompile that gives a shader the wind's uniforms and functions, puts vert (an object of chunk
     name -> code to put after it, or to replace it when the code starts with "=") in the vertex shader, and the land's
     height fog in its fragment shader; a depth material passes fog false. */
  function patch(vert, fog, frag) {
    return function (shader) {
      shader.uniforms.uTime = uniforms.uTime;
      shader.uniforms.uWind = uniforms.uWind;
      shader.uniforms.uGlow = uniforms.uGlow;
      var vs = shader.vertexShader.replace("#include <common>", "#include <common>\n" + WIND_GLSL + "\n" + (vert.head || ""));
      Object.keys(vert).forEach(function (chunk) {
        if (chunk === "head") return;
        var code = vert[chunk];
        vs = vs.replace("#include <" + chunk + ">", code.charAt(0) === "=" ? code.slice(1) : "#include <" + chunk + ">\n" + code);
      });
      shader.vertexShader = vs;
      if (frag) Object.keys(frag).forEach(function (chunk) { shader.fragmentShader = shader.fragmentShader.replace("#include <" + chunk + ">", frag[chunk]); });
      if (fog) L.heightFog(shader);
    };
  }
  /* oneSided keeps a double-sided material's normals as given on both faces, so a blade or a card lit from behind is lit
     like its front, not as if its normal turned under. */
  function oneSided(THREE, glow) {
    var f = { normal_fragment_begin: THREE.ShaderChunk.normal_fragment_begin.replace("gl_FrontFacing ? 1.0 : - 1.0", "1.0") };
    if (glow) { // a thin leaf or blade passes some of the sun's light: its own colour, lit from within, by the light's strength
      f.common = "#include <common>\nuniform vec3 uGlow;";
      f.emissivemap_fragment = "#include <emissivemap_fragment>\ntotalEmissiveRadiance += diffuseColor.rgb * uGlow * " + glow.toFixed(2) + ";";
    }
    return f;
  }

  /* instanced makes an InstancedMesh of geometry g and material m, count instances from mat(i, matrix) and colour(i, color),
     casting and receiving shadow, named name, with depth for its shadow pass. */
  function instanced(THREE, g, m, count, name, set, depth) {
    var mesh = new THREE.InstancedMesh(g, m, count), mx = new THREE.Matrix4(), col = new THREE.Color();
    for (var i = 0; i < count; i++) { set(i, mx, col); mesh.setMatrixAt(i, mx); mesh.setColorAt(i, col); }
    mesh.instanceMatrix.needsUpdate = true;
    if (mesh.instanceColor) mesh.instanceColor.needsUpdate = true;
    mesh.castShadow = mesh.receiveShadow = true;
    if (depth) mesh.customDepthMaterial = depth;
    mesh.computeBoundingSphere();
    mesh.name = name;
    return mesh;
  }
  function attr(THREE, g, name, arr, size) { g.setAttribute(name, new THREE.InstancedBufferAttribute(arr, size)); }
  function compose(THREE, mx, x, y, z, turn, sx, sy, sz) {
    var q = new THREE.Quaternion().setFromAxisAngle(new THREE.Vector3(0, 1, 0), turn);
    return mx.compose(new THREE.Vector3(x, y, z), q, new THREE.Vector3(sx, sy, sz));
  }
  function texture(THREE, cv, mips) {
    var t = new THREE.CanvasTexture(cv);
    t.colorSpace = THREE.SRGBColorSpace;
    t.anisotropy = 4;
    if (mips === false) { t.generateMipmaps = false; t.minFilter = THREE.LinearFilter; }
    return t;
  }

  /* The grass's blade: two triangles from a root w wide to a tip a tenth of that, one unit tall; its colour dark at the
     root, warm at the tip. The vertex shader bends it in the wind by its height squared and gives it a normal mostly up,
     so the meadow is lit as a sward. */
  var GRASS_VERT = {
    head: "attribute float aPhase;",
    beginnormal_vertex: "=vec3 bladeUp = normalize(vec3(0.0, 1.0, 0.0) + 0.35 * normalize(mat3(instanceMatrix)[2]));\nvec3 objectNormal = transpose(mat3(instanceMatrix)) * bladeUp;",
    begin_vertex: [
      "vec3 bladeRoot = instanceMatrix[3].xyz;",
      "float bladeH = length(instanceMatrix[1].xyz);",
      "float bladeSway = uWind.z * (floraGust(bladeRoot.xz) + 0.22 * sin(uTime * 2.6 + aPhase * 6.2831853));",
      "vec3 bladeOff = vec3(uWind.x, 0.0, uWind.y) * bladeSway * position.y * position.y * bladeH;",
      "bladeOff.y = -dot(bladeOff, bladeOff) * 0.6 / max(bladeH, 0.05);",
      "transformed += floraLocal(bladeOff);"
    ].join("\n")
  };
  function bladeGeometry(THREE) {
    var g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute([-0.5, 0, 0, 0.5, 0, 0, -0.05, 1, 0.04, 0.05, 1, 0.04], 3));
    g.setAttribute("normal", new THREE.Float32BufferAttribute([0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1], 3));
    g.setAttribute("uv", new THREE.Float32BufferAttribute([0, 0, 1, 0, 0, 1, 1, 1], 2));
    g.setAttribute("color", new THREE.Float32BufferAttribute([0.42, 0.46, 0.36, 0.42, 0.46, 0.36, 1.18, 1.12, 0.78, 1.18, 1.12, 0.78], 3));
    g.setIndex([0, 1, 2, 2, 1, 3]);
    return g;
  }
  // the grass's greens from lush to dry, a blade's shade picks along them
  var SWARD = ["#4f8028", "#5a8c2c", "#649834", "#6f9a3a", "#7c9e40", "#94a24a", "#b0a85a"];

  function grassMesh(THREE, blades) {
    var n = blades.length / 8, g = bladeGeometry(THREE), phase = new Float32Array(n), c = new THREE.Color();
    for (var i = 0; i < n; i++) phase[i] = blades[i * 8 + 6];
    attr(THREE, g, "aPhase", phase, 1);
    var m = new THREE.MeshStandardMaterial({ vertexColors: true, roughness: 0.82, side: THREE.DoubleSide });
    m.onBeforeCompile = patch(GRASS_VERT, true, oneSided(THREE, 0.5));
    m.customProgramCacheKey = function () { return "flora-grass"; };
    var depth = new THREE.MeshDepthMaterial({ side: THREE.DoubleSide });
    depth.onBeforeCompile = patch({ head: GRASS_VERT.head, begin_vertex: GRASS_VERT.begin_vertex });
    depth.customProgramCacheKey = function () { return "flora-grass-depth"; };
    return instanced(THREE, g, m, n, "grass", function (i, mx, col) {
      var b = blades.subarray(i * 8, i * 8 + 8), s = b[7] * b[7] * (SWARD.length - 1), k = Math.floor(s);
      compose(THREE, mx, b[0], b[1] - 0.02, b[2], b[3], b[5], b[4], b[5]);
      col.set(SWARD[k]).lerp(c.set(SWARD[Math.min(k + 1, SWARD.length - 1)]), s - k);
    }, depth);
  }

  /* cards: a sprite's geometry, n quads one unit wide and tall (crossed when n is 2, about their vertical middle) on y from 0
     to 1, or centred when mid; the card's normal +z. */
  function cards(THREE, n, mid) {
    var pos = [], nor = [], uv = [], idx = [], y0 = mid ? -0.5 : 0;
    for (var q = 0; q < n; q++) {
      var cx = q === 0 ? 1 : 0, cz = q === 0 ? 0 : 1, b = q * 4;
      pos.push(-0.5 * cx, y0, -0.5 * cz, 0.5 * cx, y0, 0.5 * cz, -0.5 * cx, y0 + 1, -0.5 * cz, 0.5 * cx, y0 + 1, 0.5 * cz);
      for (var k = 0; k < 4; k++) nor.push(cz, 0, -cx);
      uv.push(0, 0, 1, 0, 0, 1, 1, 1);
      idx.push(b, b + 1, b + 2, b + 2, b + 1, b + 3);
    }
    var g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(pos, 3));
    g.setAttribute("normal", new THREE.Float32BufferAttribute(nor, 3));
    g.setAttribute("uv", new THREE.Float32BufferAttribute(uv, 2));
    g.setIndex(idx);
    return g;
  }

  /* A cutout material, opaque where its atlas cell's alpha is over cut, the cell picked by aCell; cols x rows cells. */
  function cutout(THREE, map, cols, rows, vert, name, extra) {
    var cell = "#ifdef USE_MAP\n  vMapUv = (vMapUv + vec2(mod(aCell, " + cols.toFixed(1) + "), floor(aCell / " + cols.toFixed(1) + "))) / vec2(" + cols.toFixed(1) + ", " + rows.toFixed(1) + ");\n#endif";
    var v = Object.assign({}, vert, { head: "attribute float aCell;\nattribute float aPhase;\n" + (vert.head || ""), uv_vertex: cell });
    var opts = Object.assign({ roughness: 0.8, side: THREE.DoubleSide, alphaTest: map ? 0.45 : 0, map: map || null }, extra || {});
    delete opts.glow;
    var m = new THREE.MeshStandardMaterial(opts);
    m.onBeforeCompile = patch(v, true, oneSided(THREE, extra && extra.glow));
    m.customProgramCacheKey = function () { return name; };
    var depth = new THREE.MeshDepthMaterial({ map: map || null, alphaTest: map ? 0.45 : 0, side: THREE.DoubleSide });
    var dv = { head: v.head, uv_vertex: cell };
    if (v.begin_vertex) dv.begin_vertex = v.begin_vertex;
    depth.onBeforeCompile = patch(dv);
    depth.customProgramCacheKey = function () { return name + "-depth"; };
    return { material: m, depth: depth };
  }

  /* The flowers: crossed quads that nod in the wind like the grass, a little stiffer. */
  var FLOWER_VERT = {
    beginnormal_vertex: "=vec3 objectNormal = transpose(mat3(instanceMatrix)) * vec3(0.0, 1.0, 0.0);",
    begin_vertex: [
      "vec3 stemRoot = instanceMatrix[3].xyz;",
      "float stemH = length(instanceMatrix[1].xyz);",
      "float stemSway = uWind.z * 0.7 * (floraGust(stemRoot.xz) + 0.3 * sin(uTime * 2.1 + aPhase * 6.2831853));",
      "vec3 stemOff = vec3(uWind.x, 0.0, uWind.y) * stemSway * position.y * position.y * stemH * 0.5;",
      "transformed += floraLocal(stemOff);"
    ].join("\n")
  };
  function flowerMesh(THREE, fl, doc) {
    var n = fl.length / 6, g = cards(THREE, 2), cell = new Float32Array(n), phase = new Float32Array(n);
    for (var i = 0; i < n; i++) { cell[i] = fl[i * 6 + 5]; phase[i] = L.hash(i, 1, SEED + 13); }
    attr(THREE, g, "aCell", cell, 1); attr(THREE, g, "aPhase", phase, 1);
    var mat = cutout(THREE, doc ? texture(THREE, flowerAtlas(doc)) : null, 4, 1, FLOWER_VERT, "flora-flowers", { glow: 0.6 });
    var tint = ["#d0201a", "#3e64d6", "#fbfaf4", "#f2c80c"];
    return instanced(THREE, g, mat.material, n, "flowers", function (k, mx, col) {
      var f = fl.subarray(k * 6, k * 6 + 6);
      compose(THREE, mx, f[0], f[1] - 0.02, f[2], f[3], f[4] * 0.5, f[4], f[4] * 0.5);
      col.set(doc ? "#ffffff" : tint[f[5]]);
    }, mat.depth);
  }

  /* The leaf cards: each turned its own way, lit with the crown's normal (aOut, the way out of the crown) so a canopy reads as
     one mass, and shaded darker deep inside; they sway with the wind as the boughs carry them, and flutter. */
  var LEAF_VERT = {
    head: "attribute vec3 aOut;",
    beginnormal_vertex: "=vec3 objectNormal = transpose(mat3(instanceMatrix)) * aOut;",
    begin_vertex: [
      "vec3 leafAt = instanceMatrix[3].xyz;",
      "float leafSway = uWind.z * aPhase * (0.6 * floraGust(leafAt.xz * 0.5) + 0.25 * sin(uTime * 1.7 + leafAt.y * 0.4 + leafAt.x * 0.3));",
      "vec3 leafOff = vec3(uWind.x, 0.0, uWind.y) * leafSway + vec3(0.0, 0.04, 0.0) * sin(uTime * 5.3 + leafAt.x * 7.0 + leafAt.z * 5.0) * position.x;",
      "transformed += floraLocal(leafOff);"
    ].join("\n")
  };
  function leafMesh(THREE, list, map, name) {
    var n = list.length, g = cards(THREE, 1, true), cell = new Float32Array(n), phase = new Float32Array(n), out = new Float32Array(n * 3);
    for (var i = 0; i < n; i++) { cell[i] = list[i].cell; phase[i] = list[i].give; out.set(list[i].out, i * 3); }
    attr(THREE, g, "aCell", cell, 1); attr(THREE, g, "aPhase", phase, 1); attr(THREE, g, "aOut", out, 3);
    var mat = cutout(THREE, map, 2, 2, LEAF_VERT, "flora-" + name, { roughness: 0.75, glow: 1 });
    var q = new THREE.Quaternion(), e = new THREE.Euler(), v = new THREE.Vector3(), s = new THREE.Vector3();
    return instanced(THREE, g, mat.material, n, name, function (k, mx, col) {
      var c = list[k];
      e.set(c.rot[0], c.rot[1], c.rot[2]); q.setFromEuler(e);
      mx.compose(v.set(c.at[0], c.at[1], c.at[2]), q, s.set(c.size, c.size, c.size));
      col.setRGB(c.col[0], c.col[1], c.col[2]);
    }, mat.depth);
  }

  /* limbGeometry merges every tree's limbs into one geometry: a seven-sided tapered tube each, the bark's uv round and up it. */
  var LIMB_SIDES = 7; // two triangles a side, six indices
  function limbGeometry(THREE, limbs) {
    var S = LIMB_SIDES, pos = [], nor = [], uv = [], idx = [];
    limbs.forEach(function (l) {
      var a = [l[0], l[1], l[2]], b = [l[4], l[5], l[6]], d = norm([b[0] - a[0], b[1] - a[1], b[2] - a[2]]);
      var len = Math.sqrt((b[0] - a[0]) * (b[0] - a[0]) + (b[1] - a[1]) * (b[1] - a[1]) + (b[2] - a[2]) * (b[2] - a[2]));
      var side = Math.abs(d[1]) < 0.9 ? [0, 1, 0] : [1, 0, 0];
      var u = norm([d[1] * side[2] - d[2] * side[1], d[2] * side[0] - d[0] * side[2], d[0] * side[1] - d[1] * side[0]]);
      var w = [d[1] * u[2] - d[2] * u[1], d[2] * u[0] - d[0] * u[2], d[0] * u[1] - d[1] * u[0]], base = pos.length / 3;
      for (var e = 0; e < 2; e++) for (var k = 0; k <= S; k++) {
        var ang = k / S * Math.PI * 2, cs = Math.cos(ang), sn = Math.sin(ang), rad = e ? l[7] : l[3], o = e ? b : a;
        var nx = u[0] * cs + w[0] * sn, ny = u[1] * cs + w[1] * sn, nz = u[2] * cs + w[2] * sn;
        pos.push(o[0] + nx * rad, o[1] + ny * rad, o[2] + nz * rad);
        nor.push(nx, ny, nz);
        uv.push(k / S * Math.max(1, Math.round(rad * 6)), e * len / 0.8);
      }
      for (k = 0; k < S; k++) idx.push(base + k, base + k + 1, base + S + 1 + k, base + k + 1, base + S + 2 + k, base + S + 1 + k);
    });
    var g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(pos, 3));
    g.setAttribute("normal", new THREE.Float32BufferAttribute(nor, 3));
    g.setAttribute("uv", new THREE.Float32BufferAttribute(uv, 2));
    g.setIndex(idx);
    return g;
  }

  /* clumpGeometry: a hedge's leafy clump, an icosphere one unit across sitting on y 0, lumpy and flat underneath, darker
     at its foot. */
  function clumpGeometry(THREE) {
    var ico = new THREE.IcosahedronGeometry(0.5, 1), p = ico.attributes.position, at = {}, pos = [], col = [], idx = [];
    for (var i = 0; i < p.count; i++) { // the icosphere comes unindexed: weld its corners so the clump is lit smooth
      var x = p.getX(i), y = p.getY(i), z = p.getZ(i), key = x.toFixed(3) + "," + y.toFixed(3) + "," + z.toFixed(3);
      if (!(key in at)) {
        var k = 0.8 + 0.4 * L.noise(x * 5 + 9, z * 5 + y * 3 + 9, SEED + 14), ny = Math.max(y * k, -0.2) + 0.5, t = 0.55 + 0.45 * smooth(0, 1, ny);
        at[key] = pos.length / 3;
        pos.push(x * k, ny * 0.95, z * k);
        col.push(t, t, t * 0.94);
      }
      idx.push(at[key]);
    }
    var g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(pos, 3));
    g.setAttribute("color", new THREE.Float32BufferAttribute(col, 3));
    g.setIndex(idx);
    g.computeVertexNormals();
    return g;
  }

  /* The impostors: one quad each turned to face the camera about its upright, lit as a mass facing it and the sky; they
     receive the sun's shadow but cast none. */
  var IMPOSTOR_VERT = {
    beginnormal_vertex: "=vec3 imFace = normalize(vec3(viewMatrix[0][2], 0.0, viewMatrix[2][2]));\nvec3 objectNormal = transpose(mat3(instanceMatrix)) * normalize(imFace + vec3(0.0, 0.9, 0.0));",
    begin_vertex: "=vec3 imRight = normalize(vec3(viewMatrix[0][0], 0.0, viewMatrix[2][0]));\nvec3 transformed = vec3(imRight.x * position.x, position.y, imRight.z * position.x);"
  };

  /* rookGeometry: a V of two swept wings about a short body, 1 m across; aWing is how far out along a wing a vertex is. */
  function rookGeometry(THREE) {
    var g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute([0, 0, 0.16, 0, 0.01, -0.2, -0.5, 0.05, -0.12, 0.5, 0.05, -0.12, -0.22, 0.02, 0.06, 0.22, 0.02, 0.06], 3));
    g.setAttribute("normal", new THREE.Float32BufferAttribute([0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0], 3));
    g.setAttribute("aWing", new THREE.Float32BufferAttribute([0, 0, 1, 1, 0.45, 0.45], 1));
    g.setIndex([0, 4, 1, 4, 2, 1, 0, 1, 5, 5, 1, 3]);
    return g;
  }
  var ROOK_VERT = {
    head: "attribute float aWing;\nattribute float aPhase;",
    begin_vertex: "transformed.y += aWing * 0.28 * sin(uTime * 6.5 + aPhase * 6.2831853);"
  };

  /* build(THREE, doc, opts) -> a Group of everything that grows, its meshes named grass, flowers, hedges, hedge leaves,
     limbs, leaves, impostors and rooks. opts.grass is the blades to plant (GRASS without it; the quality tiers of step 9
     scale it), opts.impostors draws the mid ground's trees as impostors (the low tier; see quality()); opts.splat a canvas texture of the land's splat, whose red is the grass's weight in a browser; opts.palette
     the almanac's sky, whose light shines through the leaves. */
  function build(THREE, doc, opts) {
    opts = opts || {};
    uniforms = { uTime: { value: 0 }, uWind: { value: new THREE.Vector3(WIND.dir[0], WIND.dir[1], WIND.strength) }, uGlow: { value: new THREE.Color(0, 0, 0) } };
    if (opts.palette) light(opts.palette);
    var lay = layout(opts.grass || GRASS, splatWeight(opts.splat)), blades = lay.grass, fl = lay.flowers;
    var group = new THREE.Group(), r = rnd(SEED + 15);
    group.name = "flora";
    group.add(grassMesh(THREE, blades), flowerMesh(THREE, fl, doc));

    // the hedges: the clumps, and on the near verge a coat of leaf cards over each
    var hd = lay.hedges, nh = hd.length / 7, clumpMat = new THREE.MeshStandardMaterial({ vertexColors: true, roughness: 0.95 });
    clumpMat.onBeforeCompile = L.heightFog;
    var hedgeCols = ["#34501e", "#3d5a24", "#2f4a1c", "#476a2a", "#3a5622"];
    group.add(instanced(THREE, clumpGeometry(THREE), clumpMat, nh, "hedges", function (i, mx, col) {
      var h = hd.subarray(i * 7, i * 7 + 7);
      var core = h[6] ? 0.78 : 1; // a verge clump's leaves stand out past its body
      compose(THREE, mx, h[0], h[1] - 0.15, h[2], h[3], h[4] * core, h[5] * core, h[4] * 0.8 * core);
      col.set(hedgeCols[Math.floor(r(i, 1) * hedgeCols.length)]);
    }));
    var leaves = [], hedgeLeaves = [], limbs = [];
    for (var i = 0; i < nh; i++) {
      var h = hd.subarray(i * 7, i * 7 + 7);
      if (!h[6]) continue;
      for (var k = 0; k < 34; k++) {
        var j = i * 64 + k, a = r(j, 2) * 6.2831853, up = r(j, 3), rr = 0.5 * Math.sqrt(1 - up * up) * (0.85 + 0.25 * r(j, 4));
        var o = [Math.cos(a) * rr * 2, up, Math.sin(a) * rr * 2];
        hedgeLeaves.push({ at: [h[0] + o[0] * h[4] * 0.5, h[1] - 0.1 + (0.5 + up * 0.5) * h[5] * 0.95, h[2] + o[2] * h[4] * 0.4], size: 0.45 + 0.25 * r(j, 5), cell: 3,
          rot: [r(j, 6) * 3, r(j, 7) * 6.3, r(j, 8) * 3], out: norm([o[0], up + 0.3, o[2]]), give: 0.15, col: shade(0.35 + 0.65 * up, 0.9) });
      }
    }
    // the trees: bark for their limbs, leaf cards for their crowns
    lay.trees.forEach(function (t, ti) {
      limbs = limbs.concat(t.limbs);
      var cell = SPECIES[t.kind].cell, give = t.kind === "poplar" ? 0.5 : t.kind === "oak" ? 0.35 : 0.2;
      for (var c = 0; c < t.leaves.length; c += 8) {
        var j = ti * 100000 + c;
        leaves.push({ at: [t.leaves[c], t.leaves[c + 1], t.leaves[c + 2]], size: t.leaves[c + 3], cell: cell, rot: [r(j, 1) * 3.1, r(j, 2) * 6.3, r(j, 3) * 3.1],
          out: [t.leaves[c + 4], t.leaves[c + 5], t.leaves[c + 6]], give: give * (0.5 + 0.5 * t.leaves[c + 7]), col: shade(t.leaves[c + 7], t.kind === "poplar" ? 1.08 : 1) });
      }
    });
    // the oak's leaves and limbs come first (trees() puts it first), so the low tier draws only them and the mid ground's
    // trees as impostors (quality())
    var oak = { leaves: lay.trees[0].leaves.length / 8, limbs: lay.trees[0].limbs.length };
    var map = doc ? texture(THREE, leafAtlas(doc)) : null;
    group.add(leafMesh(THREE, hedgeLeaves, map, "hedge leaves"), leafMesh(THREE, leaves, map, "leaves"));
    var bark = new THREE.MeshStandardMaterial({ color: doc ? 0xffffff : 0x5d5246, roughness: 0.95 });
    if (doc) {
      bark.map = new THREE.DataTexture(barkPixels(64), 64, 64);
      bark.map.wrapS = bark.map.wrapT = THREE.RepeatWrapping; bark.map.colorSpace = THREE.SRGBColorSpace;
      bark.map.magFilter = THREE.LinearFilter; bark.map.minFilter = THREE.LinearMipmapLinearFilter; bark.map.generateMipmaps = true; bark.map.needsUpdate = true;
    }
    bark.onBeforeCompile = L.heightFog;
    var limbMesh = new THREE.Mesh(limbGeometry(THREE, limbs), bark);
    limbMesh.name = "limbs";
    limbMesh.castShadow = limbMesh.receiveShadow = true;
    group.add(limbMesh);

    // the woods' impostors: receive, never cast; after them the mid ground's trees as cards, drawn only at the low tier
    var mid = midImpostors(lay.trees.slice(1)), far = lay.impostors.length / 6, im = new Float32Array(lay.impostors.length + mid.length);
    im.set(lay.impostors); im.set(mid, lay.impostors.length);
    var ni = im.length / 6, ig = cards(THREE, 1), icell = new Float32Array(ni);
    for (i = 0; i < ni; i++) icell[i] = im[i * 6 + 5];
    attr(THREE, ig, "aCell", icell, 1); attr(THREE, ig, "aPhase", new Float32Array(ni), 1);
    var imMat = cutout(THREE, doc ? texture(THREE, treeAtlas(doc)) : null, 4, 1, IMPOSTOR_VERT, "flora-impostors", { roughness: 0.95 });
    var woods = instanced(THREE, ig, imMat.material, ni, "impostors", function (k, mx, col) {
      var t = im.subarray(k * 6, k * 6 + 6), v = 0.82 + 0.3 * r(k, 20);
      compose(THREE, mx, t[0], t[1], t[2], 0, t[4], t[3], t[4]);
      col.setRGB(v * (doc ? 1 : 0.17), v * (doc ? 1 : 0.28), v * (doc ? 0.95 : 0.12));
    });
    woods.castShadow = false;
    woods.frustumCulled = false; // the quads turn in the shader; their spheres are not where they are drawn
    group.add(woods);

    // the rooks
    var rk = lay.rooks, rg = rookGeometry(THREE), rookMat = new THREE.MeshStandardMaterial({ color: 0x15161a, roughness: 0.7, side: THREE.DoubleSide });
    var rphase = new Float32Array(ROOKS);
    for (i = 0; i < ROOKS; i++) rphase[i] = rk[i * 7 + 6];
    attr(THREE, rg, "aPhase", rphase, 1);
    rookMat.onBeforeCompile = patch(ROOK_VERT, true);
    rookMat.customProgramCacheKey = function () { return "flora-rooks"; };
    var birds = instanced(THREE, rg, rookMat, ROOKS, "rooks", function (k, mx, col) { col.set(0xffffff); });
    birds.frustumCulled = false;
    group.add(birds);
    group.userData.update = function (t) { fly(THREE, birds, rk, t); };
    group.userData.update(0);
    group.userData.counts = { grass: blades.length / 8, flowers: fl.length / 6, leaves: leaves.length, oakLeaves: oak.leaves, limbs: limbMesh.geometry.index.count, oakLimbs: oak.limbs * LIMB_SIDES * 6, woods: far, midWoods: ni };
    quality(group, { grass: opts.grass || GRASS, impostors: !!opts.impostors });
    return group;
  }

  /* midImpostors -> the impostors' 6 numbers for each grown tree in trees (the poplars and the orchard): its foot, its height
     to the top of its crown and its crown's width, a poplar tall (kind 1) and an apple round (0). */
  function midImpostors(trees) {
    var out = [];
    trees.forEach(function (t) {
      var foot = ground(t.x, t.z), top = foot, half = 0;
      for (var c = 0; c < t.leaves.length; c += 8) {
        var s = t.leaves[c + 3] * 0.5;
        top = Math.max(top, t.leaves[c + 1] + s);
        half = Math.max(half, Math.abs(t.leaves[c] - t.x) + s, Math.abs(t.leaves[c + 2] - t.z) + s);
      }
      out.push(t.x, foot - 0.6, t.z, top - foot + 0.6, half * 2 * 1.05, t.kind === "poplar" ? 1 : 0);
    });
    return out;
  }

  /* quality(group, q) draws what grows at a tier (QUALITY.TIERS): the first q.grass blades of those planted (they are
     scattered uniformly, so any first n are an even thinning) and the flowers in proportion; with q.impostors, the poplars
     and the orchard as impostor cards beside the far woods, only the oak grown. It changes counts and draw ranges, never
     buffers, so a tier steps down between two frames. */
  function quality(group, q) {
    var o = group.userData.counts, k = Math.min(1, (q.grass || GRASS) / Math.max(1, o.grass));
    group.getObjectByName("grass").count = Math.min(o.grass, q.grass || GRASS);
    group.getObjectByName("flowers").count = Math.round(o.flowers * k);
    group.getObjectByName("leaves").count = q.impostors ? o.oakLeaves : o.leaves;
    group.getObjectByName("limbs").geometry.setDrawRange(0, q.impostors ? o.oakLimbs : o.limbs);
    group.getObjectByName("impostors").count = q.impostors ? o.midWoods : o.woods;
    group.userData.impostors = !!q.impostors;
  }

  /* shade -> an [r, g, b] multiplier for a leaf at depth d in its crown (0 deep, 1 out in the sun), warmer and lighter outside. */
  function shade(d, k) { var v = (0.42 + 0.62 * d) * k; return [v * (0.96 + 0.08 * d), v, v * (0.9 - 0.05 * d)]; }

  /* fly sets each rook on its circle at t seconds: heading along it, banked into the turn, bobbing a little. */
  var flyTmp = null;
  function fly(THREE, mesh, rk, t) {
    if (!flyTmp) flyTmp = { m: new THREE.Matrix4(), q: new THREE.Quaternion(), e: new THREE.Euler(0, 0, 0, "YXZ"), p: new THREE.Vector3(), s: new THREE.Vector3(1.1, 1.1, 1.1) };
    var f = flyTmp;
    for (var i = 0; i < ROOKS; i++) {
      var b = rk.subarray(i * 7, i * 7 + 7), a = b[5] + b[4] * t, cs = Math.cos(a), sn = Math.sin(a);
      f.p.set(b[0] + cs * b[3], b[1] + 1.5 * Math.sin(t * 0.4 + b[5] * 3), b[2] + sn * b[3]);
      var way = b[4] > 0 ? 1 : -1; // the velocity is (-sin, cos) * way
      f.e.set(0, Math.atan2(-sn * way, cs * way), -way * 0.35);
      f.q.setFromEuler(f.e);
      mesh.setMatrixAt(i, f.m.compose(f.p, f.q, f.s));
    }
    mesh.instanceMatrix.needsUpdate = true;
  }

  /* light sets the glow through the leaves from a palette of ALMANAC.skyPalette: its light's colour by its strength. */
  function light(p) { if (uniforms) uniforms.uGlow.value.set(p.light).multiplyScalar(p.strength * GLOW); }

  /* update moves the wind and the rooks to t seconds of the page's own clock. */
  function update(group, t) {
    if (uniforms) uniforms.uTime.value = t;
    if (group && group.userData.update) group.userData.update(t);
  }

  /* splatWeight reads a splat canvas texture's red, the grass's weight, as a function of x, z; null without one. */
  function splatWeight(tex) {
    var cv = tex && tex.image;
    if (!cv || !cv.getContext || !cv.width) return null;
    var data;
    try { data = cv.getContext("2d").getImageData(0, 0, cv.width, cv.height).data; } catch (e) { return null; }
    var n = cv.width, half = L.SIZE / 2;
    return function (x, z) {
      var i = clamp(Math.floor((x + half) / L.SIZE * n), 0, n - 1), j = clamp(Math.floor((z + half) / L.SIZE * cv.height), 0, cv.height - 1);
      return data[(j * n + i) * 4] / 255;
    };
  }

  return {
    GRASS: GRASS, FLOWERS: FLOWERS, NEAREST: NEAREST, MEADOW: MEADOW, BLADE: BLADE, OAK: OAK, VERGE: VERGE, ROOKS: ROOKS, WIND: WIND, KINDS: KINDS, SPECIES: SPECIES,
    ground: ground, seen: seen, curve: curve, dist: dist, meadow: meadow, flowerWeight: flowerWeight, grass: grass, flowers: flowers, hedges: hedges,
    grow: grow, trees: trees, impostors: impostors, midImpostors: midImpostors, rooks: rooks, layout: layout, build: build, quality: quality, light: light, update: update
  };
})();
if (typeof module !== "undefined") module.exports = FLORA;
