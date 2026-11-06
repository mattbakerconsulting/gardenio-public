/* STONE: the splash's standing stone (plan gd-EJGA3zDGCk, step 4). A quarried block W x H x D m with its arrises worn round
   and chipped and its faces displaced by noise, on a short dry-stone plinth, turned TURN so its front face (local +z) meets
   the camera at three-quarters. The front is a dressed panel with the gardenio mark cut DEPTH m into it (the sprout from
   gardenio/keycloak-theme/gardenio/login/resources/img/logo.svg above its wordmark, MARK_W of the face's width) and gems
   set round the mark in four tinctures.

   Where it stands. LAND.STONE: on the brow of the hill about 8 m ahead of the camera, right of centre, its head against the
   valley, not on the knoll 20 m off on the crest: there the face is 50 px wide on a phone and the wordmark a smudge of 35 px;
   here the wordmark is about 80 px wide, legible at 390 px.

   How the mark is cut. The logo's paths are drawn on a MAP px canvas laid over the front face, a pixel PX m square (the
   face takes the canvas's left W/H; the strip right of it is plain stone, where the other faces sample). That mask, blurred
   into a steep-walled cut, is used three ways on channel uv1: a displacement map on the finely subdivided front face, so
   the letters are really DEPTH m deep and throw real shadow; a normal map from the same height, for the bright rim the
   chisel leaves; and an AO-roughness map (r ambient occlusion, g roughness, b an albedo tint) so the cut floor is duller
   and darker than the polished face. Granite grain is a tiling map on uv; weathering, rain streaks, lichen on the faces
   turned from the sun and moss at the foot are vertex colours.

   Deterministic and portable, like LAND: geometry(), plinth() and gem() use only + - * /, Math.floor, Math.sqrt and
   Math.imul, so their vertex positions hash the same everywhere; TestStoneUnderNode pins the hash. The canvas maps need a
   document; under node build() makes the stone without the mark. */
var STONE = (function () {
  "use strict";
  var L = typeof LAND !== "undefined" ? LAND : require("./land.js");
  var SEED = 20261104;
  var W = 1.1, H = 1.6, D = 0.9, BEVEL = 0.035, BEVEL_STEPS = 5;
  var HX = W / 2, HY = H / 2, HZ = D / 2;
  // vertex spacing: FINE across the front where the mark and the gems are, COARSE elsewhere; ROCK for the plinth's stones
  var FINE = 0.0055, COARSE = 0.018, ROCK = 0.045, FINE_Y = [-0.3, 0.46];
  var DEPTH = 0.02, MAP = 1024, PX = H / MAP, FACE_U = W / H, STRIP = [0.74, 0.98];
  var GRAIN = 256, GRAIN_TILE = 0.35; // the granite map's size in px, and the metres one tile covers
  // the plinth: two courses of rubble standing OUT m proud of the stone all round, its stones DEEP m into the wall
  var COURSES = [0.13, 0.12], OUT = 0.18, DEEP = 0.22, JOINT = 0.012, BATTER = 0.035, SINK = 0.02, BURY = 0.07;
  var TURN = -0.55; // radians about y: the face looks south-south-west, so the morning sun rakes across it from the left

  /* The mark: the logo's paths, in its 153 x 30 units, in the order logo.svg has them (TestStoneUnderNode checks they are
     the file's): the sprout's stem and two leaves, then the wordmark. */
  var SPROUT = ["M14 26V12h2.2v14z", "M15 14C15 8.5 11.2 5.6 5 5.6 5 11.4 8.6 14.6 15 14z", "M16 11.4C16 6.4 19.2 3 25 3c0 5.2-3 8.4-9 8.4z"];
  var WORD = "M40.63 9.54Q43.26 9.54 44.74 11.62L44.84 11.62L45.15 9.80L48.43 9.80L48.43 24.03Q48.43 27.07 46.64 28.65Q44.84 30.24 41.33 30.24Q39.82 30.24 38.54 30.06Q37.25 29.88 36.03 29.41L36.03 26.31Q38.65 27.41 41.57 27.41Q44.56 27.41 44.56 24.18L44.56 23.90Q44.56 23.45 44.59 22.99Q44.63 22.52 44.66 22.15L44.56 22.15Q43.83 23.27 42.81 23.77Q41.80 24.26 40.53 24.26Q38 24.26 36.59 22.32Q35.17 20.39 35.17 16.93Q35.17 13.44 36.63 11.49Q38.08 9.54 40.63 9.54M41.85 12.69Q39.12 12.69 39.12 16.98Q39.12 21.22 41.90 21.22Q43.39 21.22 44.10 20.37Q44.82 19.53 44.82 17.42L44.82 16.95Q44.82 14.67 44.11 13.68Q43.41 12.69 41.85 12.69M58.31 9.52Q61.17 9.52 62.69 10.75Q64.21 11.99 64.21 14.54L64.21 24L61.51 24L60.75 22.08L60.65 22.08Q59.74 23.22 58.73 23.74Q57.71 24.26 55.94 24.26Q54.05 24.26 52.80 23.16Q51.55 22.05 51.55 19.76Q51.55 17.50 53.14 16.42Q54.72 15.34 57.89 15.24L60.36 15.16L60.36 14.54Q60.36 13.42 59.78 12.90Q59.19 12.38 58.15 12.38Q57.11 12.38 56.13 12.68Q55.14 12.98 54.15 13.42L52.88 10.79Q54.02 10.19 55.41 9.86Q56.80 9.52 58.31 9.52M60.36 17.42L58.86 17.47Q56.98 17.53 56.26 18.15Q55.53 18.77 55.53 19.79Q55.53 20.67 56.05 21.05Q56.57 21.43 57.40 21.43Q58.65 21.43 59.51 20.68Q60.36 19.94 60.36 18.59L60.36 17.42M76.12 9.54Q76.41 9.54 76.80 9.57Q77.19 9.60 77.42 9.65L77.13 13.29Q76.95 13.24 76.60 13.20Q76.25 13.16 75.99 13.16Q75 13.16 74.09 13.51Q73.18 13.86 72.62 14.64Q72.06 15.42 72.06 16.77L72.06 24L68.19 24L68.19 9.80L71.13 9.80L71.70 12.20L71.88 12.20Q72.51 11.10 73.60 10.32Q74.69 9.54 76.12 9.54M84.49 24.26Q82.13 24.26 80.63 22.40Q79.14 20.54 79.14 16.93Q79.14 13.29 80.64 11.42Q82.15 9.54 84.60 9.54Q86.13 9.54 87.12 10.14Q88.11 10.74 88.68 11.62L88.81 11.62Q88.73 11.21 88.63 10.41Q88.52 9.62 88.52 8.79L88.52 4.24L92.40 4.24L92.40 24L89.43 24L88.68 22.15L88.52 22.15Q87.95 23.04 86.99 23.65Q86.03 24.26 84.49 24.26M85.84 21.17Q87.46 21.17 88.11 20.22Q88.76 19.27 88.78 17.37L88.78 16.95Q88.78 14.87 88.15 13.78Q87.51 12.69 85.79 12.69Q84.52 12.69 83.79 13.79Q83.06 14.90 83.06 16.98Q83.06 19.06 83.79 20.11Q84.52 21.17 85.84 21.17M102.30 9.54Q105.24 9.54 106.96 11.22Q108.67 12.90 108.67 15.99L108.67 17.86L99.52 17.86Q99.57 19.50 100.50 20.44Q101.42 21.37 103.06 21.37Q104.43 21.37 105.55 21.10Q106.67 20.83 107.87 20.26L107.87 23.25Q106.83 23.77 105.67 24.01Q104.51 24.26 102.87 24.26Q100.74 24.26 99.10 23.47Q97.47 22.67 96.53 21.06Q95.59 19.45 95.59 17.01Q95.59 14.51 96.44 12.86Q97.28 11.21 98.79 10.38Q100.30 9.54 102.30 9.54M102.33 12.30Q101.21 12.30 100.47 13.03Q99.73 13.76 99.60 15.29L105.03 15.29Q105.01 13.99 104.36 13.14Q103.71 12.30 102.33 12.30M119.88 9.54Q122.17 9.54 123.54 10.78Q124.92 12.01 124.92 14.74L124.92 24L121.05 24L121.05 15.71Q121.05 14.17 120.50 13.40Q119.96 12.64 118.76 12.64Q116.99 12.64 116.34 13.85Q115.69 15.06 115.69 17.32L115.69 24L111.82 24L111.82 9.80L114.78 9.80L115.30 11.62L115.51 11.62Q116.19 10.53 117.37 10.04Q118.55 9.54 119.88 9.54M130.85 4.24Q131.71 4.24 132.33 4.64Q132.96 5.05 132.96 6.14Q132.96 7.20 132.33 7.62Q131.71 8.04 130.85 8.04Q129.97 8.04 129.36 7.62Q128.74 7.20 128.74 6.14Q128.74 5.05 129.36 4.64Q129.97 4.24 130.85 4.24M128.90 9.80L132.77 9.80L132.77 24L128.90 24L128.90 9.80M149.73 16.88Q149.73 20.41 147.87 22.34Q146.01 24.26 142.81 24.26Q140.83 24.26 139.29 23.40Q137.74 22.54 136.86 20.89Q135.97 19.24 135.97 16.88Q135.97 13.34 137.82 11.44Q139.66 9.54 142.89 9.54Q144.89 9.54 146.42 10.40Q147.96 11.26 148.84 12.88Q149.73 14.51 149.73 16.88M139.92 16.88Q139.92 18.98 140.61 20.06Q141.30 21.14 142.86 21.14Q144.40 21.14 145.09 20.06Q145.77 18.98 145.77 16.88Q145.77 14.77 145.09 13.72Q144.40 12.66 142.84 12.66Q141.30 12.66 140.61 13.72Q139.92 14.77 139.92 16.88";
  // their extents in logo units: the sprout's (centred on its stem) and the wordmark's, the tail of its g included
  var SPROUT_BOX = [5, 3, 25, 26], STEM_X = 15.1, WORD_BOX = [35.17, 4.24, 149.73, 30.24];
  var MARK_W = 0.7, SPROUT_SCALE = 1.7, MARK_GAP = 5, MARK_Y = 0.1; // the mark's centre MARK_Y m above the face's

  /* The gems, placed by hand in face metres (x right, y up from the face's centre) with their girdle's diameter: a garnet
     over the sprout, emeralds beside its leaves, topazes at the wordmark's ends, sapphires and a garnet under it. */
  var GEMS = [
    [0, 0.41, 0.04, "garnet"],
    [-0.19, 0.225, 0.032, "emerald"], [0.19, 0.225, 0.032, "emerald"],
    [-0.445, -0.03, 0.028, "topaz"], [0.445, -0.03, 0.028, "topaz"],
    [-0.15, -0.215, 0.03, "sapphire"], [0.15, -0.215, 0.03, "sapphire"],
    [0, -0.24, 0.036, "garnet"]
  ];
  var TINCTURES = {
    garnet: { color: 0x9b1028, ior: 1.79 }, sapphire: { color: 0x1d44b8, ior: 1.77 },
    emerald: { color: 0x0f8a50, ior: 1.58 }, topaz: { color: 0xe8a826, ior: 1.62 }
  };

  function clamp(x, lo, hi) { return x < lo ? lo : x > hi ? hi : x; }
  function smooth(e0, e1, x) { var k = clamp((x - e0) / (e1 - e0), 0, 1); return k * k * (3 - 2 * k); }

  /* hash3 gives [0, 1) for a 3D lattice point and a seed, by integer mixing (LAND.hash's, one axis more). */
  function hash3(ix, iy, iz, s) {
    var h = Math.imul(ix | 0, 0x27d4eb2d) ^ Math.imul(iy | 0, 0x165667b1) ^ Math.imul(iz | 0, 0x1b873593) ^ Math.imul(s | 0, 0x9e3779b1);
    h = Math.imul(h ^ (h >>> 15), 0x85ebca6b);
    h = Math.imul(h ^ (h >>> 13), 0xc2b2ae35);
    return ((h ^ (h >>> 16)) >>> 0) / 4294967296;
  }
  function fade(t) { return t * t * t * (t * (t * 6 - 15) + 10); }
  /* noise3 is 3D value noise in [0, 1). */
  function noise3(x, y, z, s) {
    var ix = Math.floor(x), iy = Math.floor(y), iz = Math.floor(z), u = fade(x - ix), v = fade(y - iy), w = fade(z - iz);
    function lerp(a, b, t) { return a + (b - a) * t; }
    function row(j, k) { return lerp(hash3(ix, iy + j, iz + k, s), hash3(ix + 1, iy + j, iz + k, s), u); }
    return lerp(lerp(row(0, 0), row(1, 0), v), lerp(row(0, 1), row(1, 1), v), w);
  }
  /* fbm3 sums octaves of noise3 in [-1, 1), each turned by a fixed rational rotation and doubled, so no lattice shows. */
  function fbm3(x, y, z, octaves, s) {
    var sum = 0, amp = 1, total = 0;
    for (var i = 0; i < octaves; i++) {
      sum += noise3(x, y, z, s + i * 131) * amp;
      total += amp;
      var nx = 1.6 * y + 1.2 * z, ny = -1.6 * x + 0.72 * y - 0.96 * z, nz = -1.2 * x - 0.96 * y + 1.28 * z;
      x = nx + 5.3; y = ny - 2.9; z = nz + 7.1; amp *= 0.5;
    }
    return sum / total * 2 - 1;
  }
  /* rng is a seeded stream in [0, 1). */
  function rng(seed) { var n = 0; return function () { return hash3(n++, 3, 11, seed); }; }

  /* axis lists the sample positions along one side of a box of half extent h with arrises of radius bevel: steps samples
     across each arris, then the flat between at spacing fine inside [lo, hi] and coarse outside it, ending exactly at +-h. */
  function axis(h, bevel, steps, fine, coarse, lo, hi) {
    var b = h - bevel, a = [], cuts = [-b], k;
    for (k = 0; k < steps; k++) a.push(-h + bevel * k / steps);
    if (lo > -b && lo < b) cuts.push(lo);
    if (hi > -b && hi < b && hi > lo) cuts.push(hi);
    cuts.push(b);
    for (var i = 0; i + 1 < cuts.length; i++) {
      var x0 = cuts[i], x1 = cuts[i + 1], step = x0 >= lo && x1 <= hi ? fine : coarse, n = Math.max(1, Math.ceil((x1 - x0) / step));
      for (k = 0; k < n; k++) a.push(x0 + (x1 - x0) * k / n);
    }
    for (k = 0; k < steps; k++) a.push(b + bevel * k / steps);
    a.push(h);
    return a;
  }

  /* The six faces of a box: the fixed axis and its sign, then the two axes the face's grid runs along, ordered so u x v
     points out of the box and the grid's triangles wind outwards. The front (+z) runs x then y. */
  var FACES = [[2, 1, 0, 1], [2, -1, 1, 0], [0, 1, 1, 2], [0, -1, 2, 1], [1, 1, 2, 0], [1, -1, 0, 2]];

  /* box builds a rounded, displaced box: o.h its half extents, o.bevel the arrises' radius, o.axes the three sample lists.
     Every grid point p of a face goes to the rounded box's surface s with its normal n (p pulled onto the box shrunk by
     bevel, then out by bevel along the way it was pulled), then o.relief(s, n) m along n and, when given, o.bite(s, out) m
     along out (a fixed direction, where n would fold the surface). o.shade(s, n, out) writes its colour (sRGB), o.uv1(face,
     p) its second uv. Faces share their edge samples, so the seams are closed; the normals are
     the triangles' summed and, on the seams, shared across faces. o.at and o.lean (dy per m of x and of z) place it. */
  function box(o) {
    var h = o.h, ax = o.axes, ix = [h[0] - o.bevel, h[1] - o.bevel, h[2] - o.bevel], at = o.at || [0, 0, 0], lean = o.lean || [0, 0];
    var count = 0, nIdx = 0;
    FACES.forEach(function (f) { var nu = ax[f[2]].length, nv = ax[f[3]].length; count += nu * nv; nIdx += (nu - 1) * (nv - 1) * 6; });
    var pos = new Float32Array(count * 3), nrm = new Float32Array(count * 3), uv = new Float32Array(count * 2), uv1 = new Float32Array(count * 2);
    var col = new Float32Array(count * 3), idx = new Uint32Array(nIdx), seams = new Map(), p = [0, 0, 0], s = [0, 0, 0], n = [0, 0, 0], rgb = [0, 0, 0], off = [0, 0, 0];
    var v = 0, t = 0;
    FACES.forEach(function (f, fi) {
      var fa = f[0], ua = f[2], va = f[3], us = ax[ua], vs = ax[va], nu = us.length, nv = vs.length, first = v;
      for (var j = 0; j < nv; j++) for (var i = 0; i < nu; i++) {
        p[fa] = f[1] * h[fa]; p[ua] = us[i]; p[va] = vs[j];
        var dx = p[0] - clamp(p[0], -ix[0], ix[0]), dy = p[1] - clamp(p[1], -ix[1], ix[1]), dz = p[2] - clamp(p[2], -ix[2], ix[2]);
        var l = Math.sqrt(dx * dx + dy * dy + dz * dz);
        n[0] = dx / l; n[1] = dy / l; n[2] = dz / l;
        s[0] = p[0] - dx + n[0] * o.bevel; s[1] = p[1] - dy + n[1] * o.bevel; s[2] = p[2] - dz + n[2] * o.bevel;
        var d = o.relief(s, n);
        off[0] = off[1] = off[2] = 0;
        if (o.bite) o.bite(s, off);
        var x = s[0] + n[0] * d + off[0], y = s[1] + n[1] * d + off[1], z = s[2] + n[2] * d + off[2];
        pos[v * 3] = x + at[0]; pos[v * 3 + 1] = y + x * lean[0] + z * lean[1] + at[1]; pos[v * 3 + 2] = z + at[2];
        o.shade(s, n, rgb);
        col[v * 3] = rgb[0]; col[v * 3 + 1] = rgb[1]; col[v * 3 + 2] = rgb[2];
        uv[v * 2] = p[ua] / GRAIN_TILE; uv[v * 2 + 1] = p[va] / GRAIN_TILE;
        var w = o.uv1 ? o.uv1(fi, p) : null;
        uv1[v * 2] = w ? w[0] : 0; uv1[v * 2 + 1] = w ? w[1] : 0;
        if (i === 0 || j === 0 || i === nu - 1 || j === nv - 1) {
          var key = p[0] + "," + p[1] + "," + p[2], list = seams.get(key);
          if (!list) seams.set(key, list = []);
          list.push(v);
        }
        v++;
      }
      for (j = 0; j + 1 < nv; j++) for (i = 0; i + 1 < nu; i++) {
        var a = first + j * nu + i, b = a + 1, c = a + nu, e = c + 1;
        idx[t++] = a; idx[t++] = b; idx[t++] = e; idx[t++] = a; idx[t++] = e; idx[t++] = c;
      }
    });
    normals(pos, idx, nrm, seams);
    return { positions: pos, normals: nrm, uv: uv, uv1: uv1, colors: col, index: idx };
  }

  /* normals sums each triangle's (area-weighted) normal into its corners, shares the sums across a seam's copies of a
     point, and normalises. */
  function normals(pos, idx, out, seams) {
    for (var t = 0; t < idx.length; t += 3) {
      var a = idx[t] * 3, b = idx[t + 1] * 3, c = idx[t + 2] * 3;
      var ux = pos[b] - pos[a], uy = pos[b + 1] - pos[a + 1], uz = pos[b + 2] - pos[a + 2];
      var vx = pos[c] - pos[a], vy = pos[c + 1] - pos[a + 1], vz = pos[c + 2] - pos[a + 2];
      var nx = uy * vz - uz * vy, ny = uz * vx - ux * vz, nz = ux * vy - uy * vx;
      [a, b, c].forEach(function (k) { out[k] += nx; out[k + 1] += ny; out[k + 2] += nz; });
    }
    seams.forEach(function (list) {
      if (list.length < 2) return;
      var sx = 0, sy = 0, sz = 0;
      list.forEach(function (k) { sx += out[k * 3]; sy += out[k * 3 + 1]; sz += out[k * 3 + 2]; });
      list.forEach(function (k) { out[k * 3] = sx; out[k * 3 + 1] = sy; out[k * 3 + 2] = sz; });
    });
    for (var k = 0; k < out.length; k += 3) {
      var l = Math.sqrt(out[k] * out[k] + out[k + 1] * out[k + 1] + out[k + 2] * out[k + 2]) || 1;
      out[k] /= l; out[k + 1] /= l; out[k + 2] /= l;
    }
  }

  /* merge joins box() outputs into one. */
  function merge(parts) {
    var nv = 0, ni = 0;
    parts.forEach(function (g) { nv += g.positions.length / 3; ni += g.index.length; });
    var out = { positions: new Float32Array(nv * 3), normals: new Float32Array(nv * 3), uv: new Float32Array(nv * 2), uv1: new Float32Array(nv * 2), colors: new Float32Array(nv * 3), index: new Uint32Array(ni) };
    var v = 0, t = 0;
    parts.forEach(function (g) {
      out.positions.set(g.positions, v * 3); out.normals.set(g.normals, v * 3); out.colors.set(g.colors, v * 3);
      out.uv.set(g.uv, v * 2); out.uv1.set(g.uv1, v * 2);
      for (var i = 0; i < g.index.length; i++) out.index[t++] = g.index[i] + v;
      v += g.positions.length / 3;
    });
    return out;
  }

  /* ---- the stone ---- */

  /* chips are the bites broken from the arrises: [x, y, z, radius, depth, and the unit axis they bite along] on the box's
     edges, seeded, none reaching the panel the mark and the gems are cut in. A chip's depth is at most 3/4 of its radius
     a metre (its slope under 1), so pushed along its edge's diagonal the stone's surface cannot fold over itself. */
  var chips = (function () {
    var r = rng(SEED + 1), out = [], h = [HX, HY, HZ], keep = [0.5, -0.32, 0.5, 0.48]; // the panel: |x| < 0.5, -0.32 < y < 0.48
    while (out.length < 22) {
      var e = Math.floor(r() * 12), along = Math.floor(e / 4), sa = e & 1 ? 1 : -1, sb = e & 2 ? 1 : -1, c = [0, 0, 0];
      var o1 = (along + 1) % 3, o2 = (along + 2) % 3, rad = 0.035 + r() * 0.09;
      c[along] = (r() * 2 - 1) * h[along]; c[o1] = sa * (h[o1] - BEVEL * 0.3); c[o2] = sb * (h[o2] - BEVEL * 0.3);
      var axis = [0, 0, 0];
      axis[o1] = sa * Math.SQRT1_2; axis[o2] = sb * Math.SQRT1_2;
      var dx = Math.max(0, Math.abs(c[0]) - keep[0]), dy = Math.max(0, c[1] - keep[3], keep[1] - c[1]);
      var front = HZ - c[2];
      if (Math.sqrt(dx * dx + dy * dy + front * front) < rad) continue;
      out.push([c[0], c[1], c[2], rad, 0.4 + r() * 0.3, axis[0], axis[1], axis[2]]);
    }
    return out;
  })();

  /* dressed is 1 on the front's flat panel, where the mason dressed the face, falling to 0 over its arrises. */
  function dressed(s, n) { return smooth(0.75, 0.97, n[2]) * smooth(HX - 0.01, HX - 0.07, Math.abs(s[0])) * smooth(HY - 0.01, HY - 0.07, Math.abs(s[1])); }

  /* chipDepth is how far the chips bite in at s. */
  function chipDepth(s) {
    var d = 0;
    for (var i = 0; i < chips.length; i++) {
      var c = chips[i], dx = s[0] - c[0], dy = s[1] - c[1], dz = s[2] - c[2], dd = dx * dx + dy * dy + dz * dz, r2 = c[3] * c[3];
      if (dd < r2) d += (r2 - dd) / (2 * c[3]) * c[4];
    }
    return d;
  }

  /* relief: the quarried faces are lumpy by a couple of centimetres and the dressed panel true to a millimetre or so. */
  function relief(s, n) {
    var k = dressed(s, n), x = s[0], y = s[1], z = s[2], d = 0;
    if (k < 1) d += (fbm3(x * 2.2, y * 2.2, z * 2.2, 2, SEED + 2) * 0.016 + fbm3(x * 7, y * 7, z * 7, 3, SEED + 3) * 0.007 + fbm3(x * 24, y * 24, z * 24, 2, SEED + 4) * 0.0022) * (1 - k);
    if (k > 0) d += fbm3(x * 9, y * 9, z * 9, 2, SEED + 5) * 0.0012 * k;
    return d;
  }

  /* bite pushes s into the stone along each chip's axis, rough-floored. */
  function bite(s, out) {
    for (var i = 0; i < chips.length; i++) {
      var c = chips[i], dx = s[0] - c[0], dy = s[1] - c[1], dz = s[2] - c[2], dd = dx * dx + dy * dy + dz * dz, r2 = c[3] * c[3];
      if (dd >= r2) continue;
      var d = (r2 - dd) / (2 * c[3]) * c[4] * (0.9 + 0.1 * noise3(s[0] * 25, s[1] * 25, s[2] * 25, SEED + 6));
      out[0] -= c[5] * d; out[1] -= c[6] * d; out[2] -= c[7] * d;
    }
  }

  var COS = Math.cos(TURN), SIN = Math.sin(TURN);
  /* shade colours the stone (sRGB, to multiply the granite map): the dressed panel pale and clean; the quarried faces
     weathered darker with rain streaks down them; the chips' fresh stone pale; lichen where the face turns from the sun
     (north, as the stone stands turned TURN) and on the top; moss at the foot. */
  function shade(s, n, out) {
    var k = dressed(s, n), x = s[0], y = s[1], z = s[2];
    var g = 0.8 + 0.08 * fbm3(x * 3, y * 3, z * 3, 2, SEED + 7) - 0.14 * Math.max(0, fbm3(x * 26, y * 2.2, z * 26, 2, SEED + 8)) * (1 - k);
    var r = 0.8 * g, gg = 0.8 * g, b = 0.79 * g;
    r += (1.0 - r) * k; gg += (1.0 - gg) * k; b += (1.0 - b) * k;
    var fresh = Math.min(1, chipDepth(s) * 40);
    r += (0.98 - r) * fresh * 0.6; gg += (0.96 - gg) * fresh * 0.6; b += (0.93 - b) * fresh * 0.6;
    var north = -(-n[0] * SIN + n[2] * COS), top = smooth(0.55, 0.95, n[1]);
    var patch = fbm3(x * 6, y * 6, z * 6, 3, SEED + 9) * 0.5 + fbm3(x * 22, y * 22, z * 22, 2, SEED + 10) * 0.25;
    var lichen = smooth(0.0, 0.25, patch + smooth(-0.2, 0.7, north) * 0.7 + top * 0.45 - 0.55) * (1 - 0.85 * k) * (1 - fresh);
    var hue = noise3(x * 4, y * 4, z * 4, SEED + 11), lc = hue < 0.4 ? [0.62, 0.66, 0.5] : hue < 0.8 ? [0.7, 0.68, 0.36] : [0.8, 0.52, 0.2];
    r += (lc[0] - r) * lichen * 0.85; gg += (lc[1] - gg) * lichen * 0.85; b += (lc[2] - b) * lichen * 0.85;
    var moss = smooth(-HY + 0.26, -HY + 0.04, y) * smooth(-0.15, 0.2, patch + 0.1) * (1 - 0.6 * k);
    r += (0.22 - r) * moss; gg += (0.32 - gg) * moss; b += (0.12 - b) * moss;
    out[0] = r; out[1] = gg; out[2] = b;
  }

  /* uv1 lays the front face on the mark's canvas (its left W/H, a pixel PX m square, y up) and every other face on the
     plain strip right of it, so their normal maps have a frame and nothing is cut. */
  function markUV(face, p) {
    if (face === 0) return [(p[0] + HX) / H, (p[1] + HY) / H];
    var f = FACES[face], hu = [HX, HY, HZ][f[2]], hv = [HX, HY, HZ][f[3]];
    return [STRIP[0] + (STRIP[1] - STRIP[0]) * (p[f[2]] + hu) / (2 * hu), 0.02 + 0.96 * (p[f[3]] + hv) / (2 * hv)];
  }

  /* geometry() -> the stone's body, centred on the origin, front to +z: {positions, normals, uv, uv1, colors (sRGB), index}. */
  function geometry() {
    return box({
      h: [HX, HY, HZ], bevel: BEVEL, relief: relief, bite: bite, shade: shade, uv1: markUV,
      axes: [axis(HX, BEVEL, BEVEL_STEPS, FINE, COARSE, -Infinity, Infinity), axis(HY, BEVEL, BEVEL_STEPS, FINE, COARSE, FINE_Y[0], FINE_Y[1]), axis(HZ, BEVEL, BEVEL_STEPS, FINE, COARSE, Infinity, Infinity)]
    });
  }

  /* surface(x, y) -> the front face's z at face point x, y: where the gems are set. */
  function surface(x, y) {
    var s = [x, y, HZ], n = [0, 0, 1];
    return HZ + relief(s, n);
  }

  /* ---- the plinth ---- */

  /* stones lays out the plinth's rubble: COURSES of stones round a frame OUT m wider than the stone, each course BATTER m
     in from the one below and its joints broken over the joints beneath. [centre, half extents, seed, lean]; y from the
     plinth's foot. The middle is hollow: the stone covers it. */
  function stones() {
    var r = rng(SEED + 20), out = [], y = 0;
    COURSES.forEach(function (ch, c) {
      var ex = HX + OUT - c * BATTER, ez = HZ + OUT - c * BATTER, cy = y + ch / 2;
      function row(lo, hi, fixed, alongX) {
        var t = lo, first = true;
        while (t < hi - 0.01) {
          var len = first && c % 2 ? 0.12 + r() * 0.1 : 0.22 + r() * 0.18;
          if (hi - (t + len) < 0.12) len = hi - t;
          var hl = (len - JOINT) / 2, hh = (ch - JOINT) / 2 * (0.86 + 0.14 * r()), hd = DEEP / 2 * (0.86 + 0.2 * r());
          var at = alongX ? [t + len / 2, cy - (ch / 2 - JOINT / 2 - hh), fixed] : [fixed, cy - (ch / 2 - JOINT / 2 - hh), t + len / 2];
          out.push({ at: at, h: alongX ? [hl, hh, hd] : [hd, hh, hl], seed: SEED + 30 + out.length, lean: [(r() - 0.5) * 0.06, (r() - 0.5) * 0.06] });
          t += len; first = false;
        }
      }
      row(-ex, ex, ez - DEEP / 2, true);
      row(-ex, ex, -ez + DEEP / 2, true);
      row(-ez + DEEP, ez - DEEP, ex - DEEP / 2, false);
      row(-ez + DEEP, ez - DEEP, -ex + DEEP / 2, false);
      y += ch;
    });
    return out;
  }

  /* plinth() -> the plinth's rubble as one geometry, its foot at y = 0, like geometry()'s. */
  function plinth() {
    return merge(stones().map(function (st) {
      var bev = Math.min(0.03, st.h[1] * 0.6), sd = st.seed;
      return box({
        h: st.h, bevel: bev, at: st.at, lean: st.lean,
        axes: st.h.map(function (hh) { return axis(hh, bev, 3, ROCK, ROCK, Infinity, Infinity); }),
        relief: function (s) {
          return fbm3(s[0] * 6, s[1] * 6, s[2] * 6, 3, sd) * 0.012 + fbm3(s[0] * 20, s[1] * 20, s[2] * 20, 2, sd + 1) * 0.003;
        },
        shade: function (s, n, out) {
          var g = 0.64 + 0.1 * fbm3(s[0] * 4 + sd, s[1] * 4, s[2] * 4, 2, sd + 2);
          var moss = smooth(-0.1, 0.25, fbm3(s[0] * 9, s[1] * 9, s[2] * 9, 2, sd + 3) + smooth(0.3, 0.9, n[1]) * 0.25 - smooth(-0.2, 0.6, -n[1]) * 0.1);
          out[0] = g * 0.98 + (0.24 - g * 0.98) * moss; out[1] = g * 0.96 + (0.34 - g * 0.96) * moss; out[2] = g * 0.92 + (0.13 - g * 0.92) * moss;
        }
      });
    }));
  }

  /* ---- the gems ---- */

  /* gem(d) -> the positions (non-indexed, so each facet is flat) of a low-poly brilliant d m across at the girdle, its
     axis along +z with the girdle at z = 0: an octagonal table, sixteen crown facets down to the girdle turned half a step,
     eight pavilion facets to the culet. */
  function gem(d) {
    var r = d / 2, c1 = Math.sqrt(2 + Math.SQRT2) / 2, s1 = Math.sqrt(2 - Math.SQRT2) / 2; // cos and sin of 22.5 degrees
    var dirs = [];
    for (var k = 0; k < 16; k++) {
      // the sixteen directions at 22.5 degree steps: the first quadrant's four, turned a quarter at a time
      var q = [[1, 0], [c1, s1], [Math.SQRT1_2, Math.SQRT1_2], [s1, c1]][k % 4], quad = Math.floor(k / 4);
      dirs.push(quad === 0 ? q : quad === 1 ? [-q[1], q[0]] : quad === 2 ? [-q[0], -q[1]] : [q[1], -q[0]]);
    }
    var crown = r * 0.34, table = r * 0.56, culet = -r * 0.82, out = [];
    function tri(a, b, c) { out.push(a[0], a[1], a[2], b[0], b[1], b[2], c[0], c[1], c[2]); }
    function T(i) { var q = dirs[(i * 2 + 1) % 16]; return [q[0] * table, q[1] * table, crown]; } // table corners at odd steps
    function G(i) { var q = dirs[(i * 2) % 16]; return [q[0] * r, q[1] * r, 0]; } // girdle corners at even steps
    var top = [0, 0, crown], tip = [0, 0, culet];
    for (var i = 0; i < 8; i++) {
      tri(top, T(i), T(i + 1));
      tri(T(i), G(i + 1), T(i + 1)); // the star and bezel facets between table and girdle
      tri(T(i), G(i), G(i + 1));
      tri(G(i + 1), G(i), tip);
    }
    return new Float32Array(out);
  }

  /* gems() -> where the gems sit, in the stone's frame: [x, y, z, d, tincture], z the face's surface there, so the
     crown stands proud and the pavilion is buried. */
  function gems() {
    return GEMS.map(function (g) { return [g[0], g[1], surface(g[0], g[1]) + 0.0015, g[2], g[3]]; });
  }

  /* ---- the mark ---- */

  /* layout() -> the mark's two transforms from logo units to face metres (x right, y up from the face's centre),
     {x: x0 + ux * k, y: y0 - uy * k}, and the boxes [x0, y0, x1, y1] of the whole, the sprout and the wordmark: the
     wordmark MARK_W of the face wide, the sprout
     SPROUT_SCALE times the wordmark's scale above it, MARK_GAP units between, the whole centred MARK_Y m above the
     middle of the face. */
  function layout() {
    var k = MARK_W * W / (WORD_BOX[2] - WORD_BOX[0]), sk = SPROUT_SCALE * k;
    var hs = (SPROUT_BOX[3] - SPROUT_BOX[1]) * sk, hw = (WORD_BOX[3] - WORD_BOX[1]) * k, total = hs + MARK_GAP * k + hw;
    var top = MARK_Y + total / 2, wtop = top - hs - MARK_GAP * k;
    return {
      sprout: { k: sk, x0: -STEM_X * sk, y0: top + SPROUT_BOX[1] * sk },
      word: { k: k, x0: -(WORD_BOX[0] + WORD_BOX[2]) / 2 * k, y0: wtop + WORD_BOX[1] * k },
      box: [-MARK_W * W / 2, top - total, MARK_W * W / 2, top],
      sproutBox: [(SPROUT_BOX[0] - STEM_X) * sk, top - hs, (SPROUT_BOX[2] - STEM_X) * sk, top],
      wordBox: [-MARK_W * W / 2, wtop - hw, MARK_W * W / 2, wtop]
    };
  }

  /* mask(doc) -> the cut's mask on the MAP x MAP canvas, rows bottom-up (row 0 is the face's foot) as the textures read
     it: 1 where the mark is cut to its floor, the gems' seats cut half as deep. */
  function mask(doc) {
    var cv = doc.createElement("canvas"), ctx;
    cv.width = cv.height = MAP;
    ctx = cv.getContext("2d");
    var P = (doc.defaultView || {}).Path2D || Path2D, lay = layout();
    function at(t) { ctx.setTransform(t.k / PX, 0, 0, t.k / PX, (HX + t.x0) / PX, (HY - t.y0) / PX); }
    ctx.fillStyle = "#000";
    at(lay.sprout); SPROUT.forEach(function (d) { ctx.fill(new P(d)); });
    at(lay.word); ctx.fill(new P(WORD));
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.globalAlpha = 0.25;
    GEMS.forEach(function (g) { ctx.beginPath(); ctx.arc((HX + g[0]) / PX, (HY - g[1]) / PX, g[2] * 0.54 / PX, 0, Math.PI * 2); ctx.fill(); });
    var px = ctx.getImageData(0, 0, MAP, MAP).data, out = new Float32Array(MAP * MAP);
    for (var j = 0; j < MAP; j++) for (var i = 0; i < MAP; i++) out[(MAP - 1 - j) * MAP + i] = px[(j * MAP + i) * 4 + 3] / 255;
    return out;
  }

  /* blur is a box blur of radius r over an n x n field, along rows then columns. */
  function blur(a, n, r) {
    var tmp = new Float32Array(a.length), out = new Float32Array(a.length), w = 2 * r + 1;
    for (var pass = 0; pass < 2; pass++) {
      var src = pass ? tmp : a, dst = pass ? out : tmp;
      for (var line = 0; line < n; line++) {
        var sum = 0, k;
        for (k = -r; k <= r; k++) sum += src[idx(line, clamp(k, 0, n - 1))];
        for (k = 0; k < n; k++) {
          dst[idx(line, k)] = sum / w;
          sum += src[idx(line, Math.min(n - 1, k + r + 1))] - src[idx(line, Math.max(0, k - r))];
        }
      }
      function idx(line, k) { return pass ? k * n + line : line * n + k; }
    }
    return out;
  }

  /* cutMaps(m, n) -> the three maps' RGBA bytes from mask m (n x n, rows bottom-up): disp, white on the face and black on
     the cut's floor, its walls steep but not sheer; normal, the tangent-space normal of that height (DEPTH m deep, PX m a
     pixel); orm, r ambient occlusion, g roughness (the polished face, the dull floor, the plain strip rougher), b the
     albedo's tint (the floor darker). */
  function cutMaps(m, n) {
    var c = blur(blur(m, n, 2), n, 1), N = n * n, disp = new Uint8Array(N * 4), normal = new Uint8Array(N * 4), orm = new Uint8Array(N * 4);
    var strip = Math.floor(n * (FACE_U + STRIP[0]) / 2), k;
    for (k = 0; k < N; k++) c[k] = smooth(0.15, 0.85, c[k]);
    function hgt(i, j) { return -DEPTH * c[clamp(j, 0, n - 1) * n + clamp(i, 0, n - 1)]; }
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) {
      k = j * n + i;
      var cut = c[k], o = k * 4, d = Math.round((1 - cut) * 255);
      disp[o] = disp[o + 1] = disp[o + 2] = d; disp[o + 3] = 255;
      var dx = (hgt(i + 1, j) - hgt(i - 1, j)) / (2 * PX), dy = (hgt(i, j + 1) - hgt(i, j - 1)) / (2 * PX), l = Math.sqrt(dx * dx + dy * dy + 1);
      normal[o] = Math.round((-dx / l * 0.5 + 0.5) * 255); normal[o + 1] = Math.round((-dy / l * 0.5 + 0.5) * 255); normal[o + 2] = Math.round((1 / l * 0.5 + 0.5) * 255); normal[o + 3] = 255;
      var plain = i >= strip;
      orm[o] = Math.round((1 - 0.45 * cut - 1.6 * cut * (1 - cut)) * 255); // the floor sees less sky, the walls half of it
      orm[o + 1] = Math.round((plain ? 0.92 : 0.46 + 0.5 * cut) * 255);
      orm[o + 2] = Math.round((1 - 0.4 * cut) * 255);
      orm[o + 3] = 255;
    }
    return { disp: disp, normal: normal, orm: orm };
  }

  /* granitePixels(n) -> sRGB RGBA bytes of a tiling n x n granite: a grey groundmass of quartz with pale feldspar grains,
     a few pinkish, and black flecks of mica. */
  function granitePixels(n) {
    var px = new Uint8Array(n * n * 4);
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) {
      var u = i / n, v = j / n, k = (j * n + i) * 4;
      var base = 0.66 + 0.06 * (L.noise(u * 16, v * 16, SEED + 40, 16) - 0.5) + 0.07 * (L.hash(i, j, SEED + 41) - 0.5);
      var r = base * 0.98, g = base, b = base * 1.03;
      var fel = L.noise(u * 64, v * 64, SEED + 42, 64) * 0.7 + L.noise(u * 128, v * 128, SEED + 43, 128) * 0.3;
      if (fel > 0.6) { var f = smooth(0.6, 0.68, fel) * 0.8, pink = L.noise(u * 12, v * 12, SEED + 44, 12) > 0.62 ? 1 : 0; r += (0.84 - r) * f; g += (0.83 - 0.04 * pink - g) * f; b += (0.84 - 0.06 * pink - b) * f; }
      var mica = L.noise(u * 96, v * 96, SEED + 45, 96) * 0.6 + L.noise(u * 192, v * 192, SEED + 46, 192) * 0.4;
      if (mica > 0.72) { var m = smooth(0.72, 0.78, mica) * 0.85; r += (0.16 - r) * m; g += (0.16 - g) * m; b += (0.18 - b) * m; }
      px[k] = Math.round(clamp(r, 0, 1) * 255); px[k + 1] = Math.round(clamp(g, 0, 1) * 255); px[k + 2] = Math.round(clamp(b, 0, 1) * 255); px[k + 3] = 255;
    }
    return px;
  }

  /* ---- three.js ---- */

  /* place() -> where the stone stands: x, z (LAND.STONE), the plinth's foot y (BURY m below the lowest ground under it), and
     the turn. */
  function place() {
    var x = L.STONE.x, z = L.STONE.z, ex = HX + OUT, ez = HZ + OUT, low = Infinity;
    [[-1, -1], [1, -1], [-1, 1], [1, 1], [0, 0]].forEach(function (c) {
      var lx = c[0] * ex, lz = c[1] * ez;
      low = Math.min(low, L.height(x + lx * COS + lz * SIN, z - lx * SIN + lz * COS));
    });
    return { x: x, z: z, foot: low - BURY, turn: TURN };
  }

  function geom(THREE, g) {
    var out = new THREE.BufferGeometry(), c = new THREE.Color(), col = new Float32Array(g.colors.length);
    for (var k = 0; k < col.length; k += 3) {
      c.setRGB(g.colors[k], g.colors[k + 1], g.colors[k + 2], THREE.SRGBColorSpace);
      col[k] = c.r; col[k + 1] = c.g; col[k + 2] = c.b;
    }
    out.setAttribute("position", new THREE.BufferAttribute(g.positions, 3));
    out.setAttribute("normal", new THREE.BufferAttribute(g.normals, 3));
    out.setAttribute("uv", new THREE.BufferAttribute(g.uv, 2));
    out.setAttribute("uv1", new THREE.BufferAttribute(g.uv1, 2));
    out.setAttribute("color", new THREE.BufferAttribute(col, 3));
    out.setIndex(new THREE.BufferAttribute(g.index, 1));
    return out;
  }

  function texture(THREE, px, n, opts) {
    var t = new THREE.DataTexture(px, n, n);
    t.magFilter = THREE.LinearFilter; t.minFilter = THREE.LinearMipmapLinearFilter; t.generateMipmaps = true; t.anisotropy = 8;
    if (opts.repeat) t.wrapS = t.wrapT = THREE.RepeatWrapping;
    if (opts.srgb) t.colorSpace = THREE.SRGBColorSpace;
    if (opts.channel) t.channel = opts.channel;
    t.needsUpdate = true;
    return t;
  }

  /* facets patches a gem's shader so its facets catch the sky: the view reflected off each facet reads a gradient of the
     palette's ground, horizon and zenith, weighted by Fresnel, in place of an environment map (prefiltering one costs a
     phone, and SwiftShader, seconds). */
  function facets(shader, sky) {
    shader.uniforms.uZenith = { value: sky.zenith };
    shader.uniforms.uHorizon = { value: sky.horizon };
    shader.uniforms.uGround = { value: sky.ground };
    shader.fragmentShader = shader.fragmentShader
      .replace("void main() {", "uniform vec3 uZenith;\nuniform vec3 uHorizon;\nuniform vec3 uGround;\nvoid main() {")
      .replace("#include <lights_fragment_end>", [
        "#include <lights_fragment_end>",
        "  vec3 gemV = normalize(vViewPosition);",
        "  vec3 gemR = inverseTransformDirection(reflect(-gemV, normal), viewMatrix);",
        "  vec3 gemSky = gemR.y < 0.0 ? mix(uHorizon, uGround, smoothstep(0.0, 0.4, -gemR.y)) : mix(uHorizon, uZenith, smoothstep(0.0, 0.6, gemR.y));",
        "  float gemF = 0.08 + 0.92 * pow(1.0 - clamp(dot(normal, gemV), 0.0, 1.0), 5.0);",
        "  reflectedLight.indirectSpecular += gemSky * gemF * 1.6;"
      ].join("\n"));
  }

  /* build(THREE, doc, sky) -> a Group named "stone": the body ("stone body"), the gems ("gem <tincture>") set in its face
     and the plinth ("plinth") under it, the group's origin the body's centre, placed and turned on the land by place().
     With a document the mark is cut in the face; without one (node) the face is plain. sky, ALMANAC.skyPalette's, is what
     the gems' facets reflect, kept on the group's userData.sky (THREE.Colors) for the light to change as the sky does. */
  function build(THREE, doc, sky) {
    var at = place(), group = new THREE.Group(), grain = texture(THREE, granitePixels(GRAIN), GRAIN, { repeat: true, srgb: true });
    var mat = new THREE.MeshStandardMaterial({ color: 0xffffff, map: grain, vertexColors: true, roughness: 0.62 });
    var maps = doc ? cutMaps(mask(doc), MAP) : null;
    if (maps) {
      mat.displacementMap = texture(THREE, maps.disp, MAP, { channel: 1 });
      mat.displacementScale = DEPTH; mat.displacementBias = -DEPTH;
      mat.normalMap = texture(THREE, maps.normal, MAP, { channel: 1 });
      mat.roughnessMap = mat.aoMap = texture(THREE, maps.orm, MAP, { channel: 1 });
      mat.roughness = 1; mat.aoMapIntensity = 1;
    }
    mat.onBeforeCompile = function (shader) {
      if (maps) shader.fragmentShader = shader.fragmentShader.replace("#include <color_fragment>", "#include <color_fragment>\n  diffuseColor.rgb *= texture2D(roughnessMap, vRoughnessMapUv).b;");
      L.heightFog(shader);
    };
    var body = new THREE.Mesh(geom(THREE, geometry()), mat);
    body.name = "stone body";
    body.castShadow = body.receiveShadow = true;

    var rubble = new THREE.Mesh(geom(THREE, plinth()), new THREE.MeshStandardMaterial({ color: 0xffffff, map: grain, vertexColors: true, roughness: 0.94 }));
    rubble.material.onBeforeCompile = L.heightFog;
    rubble.name = "plinth";
    rubble.castShadow = rubble.receiveShadow = true;
    var top = COURSES.reduce(function (a, b) { return a + b; }, 0);
    rubble.position.y = -HY - top + SINK;

    group.add(body, rubble);
    var shapes = {}, skyCol = sky ? { zenith: new THREE.Color(sky.zenith), horizon: new THREE.Color(sky.horizon), ground: new THREE.Color(sky.ground) } :
      { zenith: new THREE.Color(0x5a8bd6), horizon: new THREE.Color(0xb8cde6), ground: new THREE.Color(0x4a5a30) };
    gems().forEach(function (g) {
      var t = TINCTURES[g[4]], d = g[3];
      if (!shapes[d]) {
        shapes[d] = new THREE.BufferGeometry();
        shapes[d].setAttribute("position", new THREE.BufferAttribute(gem(d), 3));
        shapes[d].computeVertexNormals();
      }
      var m = new THREE.Mesh(shapes[d], new THREE.MeshPhysicalMaterial({
        color: t.color, emissive: t.color, emissiveIntensity: 0.25, roughness: 0.03, metalness: 0, ior: t.ior,
        transmission: 0.6, thickness: d * 0.8, attenuationColor: t.color, attenuationDistance: d,
        clearcoat: 1, clearcoatRoughness: 0.03, specularIntensity: 1
      }));
      m.material.onBeforeCompile = function (shader) { facets(shader, skyCol); };
      m.position.set(g[0], g[1], g[2]);
      m.name = "gem " + g[4];
      group.add(m);
    });
    group.position.set(at.x, at.foot + top - SINK + HY, at.z);
    group.rotation.y = TURN;
    group.name = "stone";
    group.userData.sky = skyCol;
    return group;
  }

  return {
    W: W, H: H, D: D, BEVEL: BEVEL, DEPTH: DEPTH, MAP: MAP, PX: PX, MARK_W: MARK_W, TURN: TURN, COURSES: COURSES, OUT: OUT,
    SPROUT: SPROUT, WORD: WORD, GEMS: GEMS, TINCTURES: TINCTURES, chips: chips,
    axis: axis, geometry: geometry, plinth: plinth, stones: stones, gem: gem, gems: gems, surface: surface, layout: layout,
    mask: mask, blur: blur, cutMaps: cutMaps, granitePixels: granitePixels, place: place, build: build
  };
})();
if (typeof module !== "undefined") module.exports = STONE;
