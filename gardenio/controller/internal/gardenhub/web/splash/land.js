/* LAND: the ground of the splash (plan gd-EJGA3zDGCk, step 3). A heightfield of 257 x 257 vertices over 400 x 400 m, the
   scene's x east and z south (north along -z, as ALMANAC.direction has it), and a coarse ring of the same height function out
   to 1 km so the far hills fill a wide window. The camera lies on the near hill's south slope at z = CAM_Z looking north;
   20 m ahead the crest runs east-west, with a knoll just right of centre; the stone (web/splash/stone.js) stands on the
   brow 8 m ahead, right of centre, where its mark can be read; beyond, the hill falls
   into a valley with a river and climbs a far side of strip fields to two wooded ridges on the horizon.

   How the picture is shaped. A crest close in front hides whatever lies below the line from the eye over it, so the camera's
   eye is a little above the crest where it looks over (CREST_DIP below the eye), the knoll 20 m ahead rises 3 m above the
   camera's feet, and the valley shows on its far side: fields climbing from the farm and the pond to the ridges. The camera
   is pitched up so the crest crosses the frame a third of the way up (camera()).

   Deterministic and portable: the height function uses only + - * /, Math.floor, Math.sqrt and Math.imul (no Math.sin or
   exp, whose last bits may differ between engines), so heightfield() hashes the same everywhere; TestLandUnderNode pins it.
   The field map is painted in code on two 1024 px canvases, the colours and the splat (grass, earth and chalk weights in
   r, g, b; gravel the rest), which a small patch of MeshStandardMaterial blends with a tiling detail texture and normal map.
   heightFog() swaps three's fog for exponential height fog in the scene's fog colour, so the far ridges sit in haze. */
var LAND = (function () {
  "use strict";
  var SIZE = 400, N = 257, SEED = 20261026, MAP = 1024;
  // the ring: out to 1 km in 25 m cells, FAR_DROP under the field at the seam and FAR_UNDER beneath it inside
  var FAR = 1000, FAR_CELL = 25, FAR_DROP = 0.3, FAR_UNDER = 4;
  var CAM_Z = 170, CREST_Z = 150, EYE = 1.4, CREST_DIP = 0.8, THIRD = -1 / 3;
  var KNOLL = { x: 6.5, z: CREST_Z, rx: 9, rz: 7, h: 2.4 }, STONE = { x: 0.85, z: CAM_Z - 8 };
  // the pond sits in a hollow west of the farmyard, below the eye: a horizontal surface above the eye cannot be seen, and
  // over the crest only ground lower than the eye shows water, a glint seen almost edge on (web/splash/farm.js)
  var POND = { x: -58, z: -38, r: 10 }, FARM = { x: -30, z: -50, r: 20 }, ORCHARD = { x0: 34, x1: 86, z0: -104, z1: -76 };
  // height fog: its density at sea level, how fast it thins per metre up, and the clear air before it starts (m)
  var FOG_FALL = 0.005, FOG_DENSITY = 0.0036, FOG_START = 150;
  var DETAIL = 128, DETAIL_REPEAT = 160; // the detail texture's size in px, and its tiles across the field (2.5 m each)

  /* The valley's profile from north to south: [z, height]. Between keys a cubic with finite-difference slopes, flat at a
     key that is a peak or a trough. The far ridge (-330, in the ring) and the near ridge (-125) on the horizon, the far
     slope up from the valley floor, the river near z 38, the near hill's crest at CREST_Z and its south slope down behind
     the camera. */
  var PROFILE = [[-1000, 90], [-620, 70], [-330, 138], [-200, 52], [-160, 60], [-125, 66], [-75, 44], [-40, 26], [0, 12],
    [38, 4], [90, 7], [118, 16], [136, 26.5], [CREST_Z, 30], [CAM_Z, 30 + CREST_DIP - EYE], [200, 27],
    [260, 19], [420, 6], [1000, 0]];

  function clamp(x, lo, hi) { return x < lo ? lo : x > hi ? hi : x; }
  function smooth(e0, e1, x) { var k = clamp((x - e0) / (e1 - e0), 0, 1); return k * k * (3 - 2 * k); }
  /* bump is 1 at the centre of an ellipse falling smoothly to 0 at its rim; r2 is the squared normalised distance. */
  function bump(r2) { return r2 >= 1 ? 0 : (1 - r2) * (1 - r2); }

  /* hash gives [0, 1) for a lattice point and a seed: integer mixing only, the same bits in every engine. */
  function hash(ix, iz, s) {
    var h = Math.imul(ix | 0, 0x27d4eb2d) ^ Math.imul(iz | 0, 0x165667b1) ^ Math.imul(s | 0, 0x9e3779b1);
    h = Math.imul(h ^ (h >>> 15), 0x85ebca6b);
    h = Math.imul(h ^ (h >>> 13), 0xc2b2ae35);
    return ((h ^ (h >>> 16)) >>> 0) / 4294967296;
  }
  /* noise is value noise in [0, 1) with a quintic fade; px (and pz, else px) make it tile every px cells in x, pz in z. */
  function noise(x, z, s, px, pz) {
    var ix = Math.floor(x), iz = Math.floor(z), fx = x - ix, fz = z - iz;
    var ux = fx * fx * fx * (fx * (fx * 6 - 15) + 10), uz = fz * fz * fz * (fz * (fz * 6 - 15) + 10);
    var x1 = ix + 1, z1 = iz + 1;
    if (px) {
      pz = pz || px;
      ix = ((ix % px) + px) % px; iz = ((iz % pz) + pz) % pz; x1 = (ix + 1) % px; z1 = (iz + 1) % pz;
    }
    var a = hash(ix, iz, s), b = hash(x1, iz, s), c = hash(ix, z1, s), d = hash(x1, z1, s);
    return a + (b - a) * ux + (c - a) * uz + (a - b - c + d) * ux * uz;
  }
  /* fbm sums octaves of noise, each twice the frequency and half the weight, turned 37 degrees from the last so the
     lattice does not show; it returns [-1, 1). */
  function fbm(x, z, octaves, s) {
    var sum = 0, amp = 1, total = 0;
    for (var i = 0; i < octaves; i++) {
      sum += noise(x, z, s + i * 101) * amp;
      total += amp;
      var nx = x * 1.6 - z * 1.2, nz = x * 1.2 + z * 1.6; // x2 and a turn of 36.87 degrees
      x = nx + 17.3; z = nz - 9.1; amp *= 0.5;
    }
    return sum / total * 2 - 1;
  }
  /* rng is a seeded stream in [0, 1) for the painter's choices. */
  function rng(seed) {
    var n = 0;
    return function () { return hash(n++, 7, seed); };
  }

  /* profile reads PROFILE at z. */
  function profile(z) {
    var P = PROFILE, n = P.length;
    if (z <= P[0][0]) return P[0][1];
    if (z >= P[n - 1][0]) return P[n - 1][1];
    var i = 1;
    while (P[i][0] < z) i++;
    var a = P[i - 1], b = P[i], w = b[0] - a[0], t = (z - a[0]) / w;
    var ma = slope(i - 1) * w, mb = slope(i) * w, t2 = t * t, t3 = t2 * t;
    return (2 * t3 - 3 * t2 + 1) * a[1] + (t3 - 2 * t2 + t) * ma + (-2 * t3 + 3 * t2) * b[1] + (t3 - t2) * mb;
  }
  function slope(i) {
    var P = PROFILE;
    if (i === 0 || i === P.length - 1) return 0;
    var l = P[i - 1][1], c = P[i][1], r = P[i + 1][1];
    if ((c >= l && c >= r) || (c <= l && c <= r)) return 0;
    return (r - l) / (P[i + 1][0] - P[i - 1][0]);
  }

  /* river gives the river's z at x: a slow meander about z 38. */
  function river(x) { return 38 + 16 * fbm(x / 110, 3.7, 3, SEED + 2); }

  /* raw is the height at x, z before the pond and the farmyard are levelled. */
  function raw(x, z) {
    var h = profile(z);
    var nearHill = smooth(112, 140, z), far = smooth(40, -90, z), valley = 1 - nearHill - far;
    // the near hill is a long whaleback, falling away east and west of the camera's line
    h -= nearHill * 14 * smooth(0, 170, x < 0 ? -x : x);
    // the knoll on the crest, right of centre behind the stone
    var kx = (x - KNOLL.x) / KNOLL.rx, kz = (z - KNOLL.z) / KNOLL.rz;
    h += KNOLL.h * bump(kx * kx + kz * kz);
    // big shapes on the far side and the ridges, rolling folds in the valley, a little everywhere; the meadow between the
    // camera and the crest is kept nearly smooth so the framing holds
    var corridor = bump((x * x) / (30 * 30) + ((z - 160) * (z - 160)) / (24 * 24));
    h += fbm(x / 150, z / 150, 5, SEED) * (16 * far + 2 * valley);
    // the ridges rise and fall along their length, the far one more
    h += fbm(x / 90, 0.5, 3, SEED + 3) * 12 * far + fbm(x / 160, 4.5, 4, SEED + 4) * 40 * smooth(-200, -330, z);
    h += fbm(x / 26, z / 26, 4, SEED + 1) * (0.9 + 0.6 * far) * (1 - 0.85 * corridor);
    // the river's channel
    var dz = (z - river(x)) / 7;
    h -= 2.2 * bump(dz * dz) * (1 - nearHill);
    return h;
  }

  var levels = null;
  /* height is the ground's height at x, z: raw, with the pond's hollow and the farmyard's terrace levelled. */
  function height(x, z) {
    if (!levels) levels = { pond: raw(POND.x, POND.z), farm: raw(FARM.x, FARM.z) };
    var h = raw(x, z);
    var fd = Math.sqrt((x - FARM.x) * (x - FARM.x) + (z - FARM.z) * (z - FARM.z));
    h += (levels.farm - h) * smooth(FARM.r, FARM.r * 0.55, fd);
    var pd = Math.sqrt((x - POND.x) * (x - POND.x) + (z - POND.z) * (z - POND.z));
    h += (levels.pond - h) * smooth(POND.r * 1.8, POND.r, pd);
    h -= 0.8 * smooth(POND.r, POND.r * 0.6, pd);
    return h;
  }

  /* pondLevel is the height of the pond's levelled floor, where its water stands (web/splash/farm.js). */
  function pondLevel() { height(POND.x, POND.z); return levels.pond; }

  /* heightfield(n, size) -> Float32Array of n * n heights, row j at z = -size/2 + j * size/(n-1), column i likewise in x. */
  function heightfield(n, size) {
    n = n || N; size = size || SIZE;
    var out = new Float32Array(n * n), step = size / (n - 1), half = size / 2;
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) out[j * n + i] = height(-half + i * step, -half + j * step);
    return out;
  }

  /* camera(fov) -> {eye, at, crest, pitch}: the eye EYE m above the ground at (0, CAM_Z); the crest, the point straight
     ahead that rises highest in the eye's view; and the point it looks at, pitched up so that the crest sits a third of the
     way up a frame of fov degrees vertical. */
  function camera(fov) {
    var eye = [0, height(0, CAM_Z) + EYE, CAM_Z], best = -Infinity, crest = null;
    for (var z = CAM_Z - 2; z > 60; z -= 0.25) {
      var y = height(0, z), a = (y - eye[1]) / (CAM_Z - z);
      if (a > best) { best = a; crest = [0, y, z]; }
    }
    var tanHalf = Math.tan(fov * Math.PI / 360), rise = Math.atan(best), pitch = rise - Math.atan(THIRD * tanHalf);
    var at = [0, eye[1] + Math.tan(pitch) * 100, CAM_Z - 100];
    return { eye: eye, at: at, crest: crest, pitch: pitch * 180 / Math.PI };
  }

  /* ---- the field map ---- */

  /* The farm's lane, from the crest down across the river to the farmyard, and the worn path from the camera to the stone,
     as [x, z] points the painter joins with curves. */
  var LANE = [[-15, 164], [-10, 147], [-19, 130], [-16, 112], [-28, 88], [-21, 62], [-26, 38], [-19, 14], [-27, -14], [-22, -34], [FARM.x, FARM.z]];
  var PATH = [[-3.5, 196], [-2.6, 184], [-1.6, 174], [-0.9, 167], [STONE.x - 0.5, STONE.z + 1.3]];
  var CROPS = {
    wheat: { c: ["#c4922c", "#cf9e36", "#b58226"], rows: 1.2, row: "rgba(250,226,150,0.35)", m: [0.75, 0.25, 0] },
    barley: { c: ["#9aa63e", "#a6b048", "#8c9a36"], rows: 1.1, row: "rgba(214,224,150,0.35)", m: [0.85, 0.15, 0] },
    hay: { c: ["#bfae5c", "#c9b866", "#b2a052"], rows: 4, row: "rgba(150,132,70,0.75)", m: [0.6, 0.4, 0] }, // windrows
    plough: { c: ["#5a4232", "#64493a", "#4e392b"], rows: 0.9, row: "rgba(30,20,14,0.55)", m: [0, 1, 0] }, // furrows
    pasture: { c: ["#5a8c2c", "#649834", "#4f8026"], rows: 0, m: [1, 0, 0] }
  };
  var ROTATION = [["wheat", "barley"], ["hay", "pasture"], ["plough", "wheat"], ["barley", "plough"], ["pasture", "wheat"], ["hay", "barley"]];
  var HEDGE = "#2a3f18", WOOD = ["#22381a", "#2c4622", "#1c3015", "#365226"], MEADOW = ["#5f8a32", "#6b953a", "#577f2c", "#7a9c44"];

  /* fields lays out the valley's strip fields: furlongs (cells between hedgerows, their corners jittered), each cut into
     strips along its long side in two alternating crops. Pure and seeded, so the layout is the same on every load. */
  function fields() {
    var r = rng(SEED + 5), cols = [-200], rows = [-118], out = [];
    for (var x = -200; x < 200;) { x += 44 + r() * 40; cols.push(Math.min(x, 200)); }
    for (var z = -118; z < 122;) { z += 20 + r() * 14; rows.push(Math.min(z, 122)); }
    var jit = function (i, j) { return [(hash(i, j, SEED + 6) - 0.5) * 12, (hash(i, j, SEED + 7) - 0.5) * 6]; };
    for (var j = 0; j + 1 < rows.length; j++) {
      for (var i = 0; i + 1 < cols.length; i++) {
        var q = [[i, j], [i + 1, j], [i + 1, j + 1], [i, j + 1]].map(function (p) {
          var d = jit(p[0], p[1]), edgeX = p[0] === 0 || p[0] === cols.length - 1;
          return [cols[p[0]] + (edgeX ? 0 : d[0]), rows[p[1]] + d[1]];
        });
        var w = cols[i + 1] - cols[i], h = rows[j + 1] - rows[j], k = r();
        out.push({ quad: q, along: w > h * 1.4 ? (k < 0.7 ? "z" : "x") : "x", crops: ROTATION[Math.floor(r() * ROTATION.length)], strip: 7 + r() * 8, kind: k < 0.08 ? "copse" : "field" });
      }
    }
    return out;
  }

  /* woodedness at x, z: above 0 is wood. The ridges and the far slope's top are wooded with ragged edges, and a few copses
     stand lower down. */
  function wooded(x, z) { return fbm(x / 60, z / 60, 4, SEED + 9) * 0.5 + smooth(-96, -140, z) * 0.9 - smooth(-220, -320, z) * 0.45 - 0.35; }

  /* paint draws the field map on ctx, a size x size 2D context whose y runs north to south over the field: layer "albedo"
     the colours, "splat" the materials (red grass, green earth, blue chalk, black gravel). It returns what it drew, by kind. */
  function paint(ctx, size, layer) {
    // r places what both layers draw, so they agree; rc picks colours and places what only the albedo draws
    var s = size / SIZE, al = layer !== "splat", r = rng(SEED + 11), rc = rng(SEED + 12), drawn = {};
    function X(x) { return (x + SIZE / 2) * s; }
    function Z(z) { return (z + SIZE / 2) * s; }
    function count(k) { drawn[k] = (drawn[k] || 0) + 1; }
    function mat(m) { return "rgb(" + Math.round(m[0] * 255) + "," + Math.round(m[1] * 255) + "," + Math.round((m[2] || 0) * 255) + ")"; }
    function pick(a) { return a[Math.floor(rc() * a.length)]; }
    function blob(x, z, rad, fill) { ctx.fillStyle = fill; ctx.beginPath(); ctx.arc(X(x), Z(z), rad * s, 0, Math.PI * 2); ctx.fill(); }
    function curve(pts, width, stroke) {
      ctx.strokeStyle = stroke; ctx.lineWidth = width * s; ctx.lineCap = "round"; ctx.lineJoin = "round";
      ctx.beginPath(); ctx.moveTo(X(pts[0][0]), Z(pts[0][1]));
      for (var i = 1; i < pts.length - 1; i++) {
        ctx.quadraticCurveTo(X(pts[i][0]), Z(pts[i][1]), X((pts[i][0] + pts[i + 1][0]) / 2), Z((pts[i][1] + pts[i + 1][1]) / 2));
      }
      var e = pts[pts.length - 1];
      ctx.lineTo(X(e[0]), Z(e[1]));
      ctx.stroke();
    }
    function quad(q) { ctx.beginPath(); ctx.moveTo(X(q[0][0]), Z(q[0][1])); for (var i = 1; i < 4; i++) ctx.lineTo(X(q[i][0]), Z(q[i][1])); ctx.closePath(); }

    // the meadow everywhere, mottled
    ctx.fillStyle = al ? MEADOW[0] : mat([1, 0, 0]);
    ctx.fillRect(0, 0, size, size);
    if (al) for (var m = 0; m < 2600; m++) blob(-200 + rc() * 400, -200 + rc() * 400, 1.5 + rc() * 5, pick(MEADOW));

    // the strip fields between hedgerows
    var fs = fields();
    fs.forEach(function (f) {
      ctx.save(); quad(f.quad); ctx.clip();
      var xs = f.quad.map(function (p) { return p[0]; }), zs = f.quad.map(function (p) { return p[1]; });
      var x0 = Math.min.apply(null, xs) - 2, x1 = Math.max.apply(null, xs) + 2, z0 = Math.min.apply(null, zs) - 2, z1 = Math.max.apply(null, zs) + 2;
      if (f.kind === "copse") {
        ctx.fillStyle = al ? WOOD[1] : mat([1, 0, 0]); ctx.fillRect(X(x0), Z(z0), (x1 - x0) * s, (z1 - z0) * s);
        if (al) for (var b = 0; b < (x1 - x0) * (z1 - z0) / 10; b++) blob(x0 + rc() * (x1 - x0), z0 + rc() * (z1 - z0), 1.8 + rc() * 2.2, pick(WOOD));
        count("copse");
      } else {
        var n = 0, lo = f.along === "z" ? x0 : z0, hi = f.along === "z" ? x1 : z1;
        for (var a = lo; a < hi; a += f.strip, n++) {
          var crop = CROPS[f.crops[n % 2]], b0 = a, b1 = Math.min(hi, a + f.strip);
          var rect = f.along === "z" ? [X(b0), Z(z0), (b1 - b0) * s, (z1 - z0) * s] : [X(x0), Z(b0), (x1 - x0) * s, (b1 - b0) * s];
          ctx.fillStyle = al ? pick(crop.c) : mat(crop.m);
          ctx.fillRect(rect[0], rect[1], rect[2], rect[3]);
          // the crop's rows run along the strip: wheat and barley drills, windrows on the cut hay, furrows in the plough
          if (al && crop.rows) {
            ctx.fillStyle = crop.row;
            for (var t = b0 + crop.rows / 2; t < b1; t += crop.rows) {
              if (f.along === "z") ctx.fillRect(X(t), Z(z0), Math.max(1, 0.35 * s), (z1 - z0) * s);
              else ctx.fillRect(X(x0), Z(t), (x1 - x0) * s, Math.max(1, 0.35 * s));
            }
          }
          count(f.crops[n % 2]);
        }
        // the dead furrow between strips
        if (al) { ctx.fillStyle = "rgba(60,50,30,0.25)"; for (a = lo + f.strip; a < hi; a += f.strip) { if (f.along === "z") ctx.fillRect(X(a), Z(z0), 1, (z1 - z0) * s); else ctx.fillRect(X(x0), Z(a), (x1 - x0) * s, 1); } }
      }
      ctx.restore();
    });
    // hedgerows on every furlong's edge, with a tree now and then
    fs.forEach(function (f) {
      quad(f.quad); ctx.strokeStyle = al ? HEDGE : mat([1, 0, 0]); ctx.lineWidth = 1.8 * s; ctx.stroke();
      if (al) for (var e = 0; e < 4; e++) if (rc() < 0.45) {
        var p = f.quad[e], q = f.quad[(e + 1) % 4], k = rc();
        blob(p[0] + (q[0] - p[0]) * k, p[1] + (q[1] - p[1]) * k, 2.4 + rc() * 1.6, pick(WOOD));
      }
      count("hedge");
    });

    // the orchard: grass under apple trees in rows
    var o = ORCHARD;
    ctx.fillStyle = al ? "#7ea24c" : mat([1, 0, 0]);
    ctx.fillRect(X(o.x0), Z(o.z0), (o.x1 - o.x0) * s, (o.z1 - o.z0) * s);
    for (var oz = o.z0 + 3; oz < o.z1 - 1; oz += 6) for (var ox = o.x0 + 3; ox < o.x1 - 1; ox += 6) {
      if (al) { blob(ox + 0.6, oz + 0.6, 2.3, "rgba(30,40,20,0.45)"); blob(ox, oz, 2, pick(["#3d6a2a", "#47752f", "#386226"])); }
      count("apple");
    }
    ctx.strokeStyle = al ? HEDGE : mat([1, 0, 0]); ctx.lineWidth = 1.6 * s; ctx.strokeRect(X(o.x0), Z(o.z0), (o.x1 - o.x0) * s, (o.z1 - o.z0) * s);

    // woodland on the far slopes and the ridges
    for (var wz = -200; wz < -60; wz += 3.2) for (var wx = -200; wx < 200; wx += 3.2) {
      var jx = wx + r() * 3, jz = wz + r() * 3;
      if (wooded(jx, jz) > 0) { blob(jx, jz, 2.4 + r() * 1.8, al ? pick(WOOD) : mat([1, 0, 0])); count("wood"); }
    }

    // the river, its banks green, and the pond by the farm
    var rv = [];
    for (var rx = -204; rx <= 204; rx += 6) rv.push([rx, river(rx)]);
    curve(rv, 11, al ? "#5d8a3c" : mat([0.7, 0.3, 0]));
    curve(rv, 4.5, al ? "#4e6b72" : mat([0, 1, 0]));
    count("river");
    blob(POND.x, POND.z, POND.r + 2.2, al ? "#58823a" : mat([0.6, 0.4, 0]));
    blob(POND.x, POND.z, POND.r, al ? "#3c5960" : mat([0, 1, 0]));
    if (al) blob(POND.x - 2, POND.z - 2, POND.r * 0.55, "rgba(140,170,180,0.35)");
    count("pond");

    // the farmyard and the lane down to it, gravel with a grass crown
    blob(FARM.x, FARM.z, FARM.r * 0.6, al ? "#9b8a66" : mat([0.15, 0.25, 0]));
    curve(LANE, 4.6, al ? "#8f7e5c" : mat([0, 0.2, 0]));
    curve(LANE, 3.4, al ? "#b7a47c" : mat([0, 0, 0]));
    if (al) curve(LANE, 0.8, "#7f9a4c");
    count("lane");

    // the near hill: chalk breaking through the turf round the stone, and the worn path up to it
    for (var c = 0; c < 14; c++) {
      var ca = r() * Math.PI * 2, cr = 1.2 + r() * 3;
      blob(STONE.x + Math.cos(ca) * cr * 1.4, STONE.z + Math.sin(ca) * cr, 0.12 + r() * 0.25, al ? pick(["rgba(150,142,114,0.6)", "rgba(136,128,102,0.6)"]) : mat([0.5, 0, 0.5]));
    }
    curve(PATH, 1.0, al ? "#7d7250" : mat([0.4, 0.5, 0.1]));
    curve(PATH, 0.45, al ? "#a4936e" : mat([0, 0.6, 0.4]));
    blob(STONE.x, STONE.z, 1.6, al ? "#94876a" : mat([0.2, 0.5, 0.3]));
    count("path");
    return drawn;
  }

  /* detailPixels(n) -> RGBA bytes of a tiling n x n detail texture, one material's pattern in each channel: blades of grass,
     clods of earth, smooth chalk, speckled gravel. And detailNormals turns the same patterns into a tiling normal map. */
  function detailPixels(n) {
    var px = new Uint8Array(n * n * 4);
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) {
      var u = i / n, v = j / n, k = (j * n + i) * 4; // one tile is u, v in [0, 1), so each noise tiles at its own scale
      var blades = noise(u * 64, v * 16, SEED + 20, 64, 16) * 0.6 + noise(u * 128, v * 32, SEED + 21, 128, 32) * 0.4; // long in v
      var clods = noise(u * 16, v * 16, SEED + 22, 16) * 0.5 + noise(u * 32, v * 32, SEED + 23, 32) * 0.3 + noise(u * 64, v * 64, SEED + 24, 64) * 0.2;
      var chalk = 0.6 + noise(u * 8, v * 8, SEED + 25, 8) * 0.4;
      var grit = hash(i, j, SEED + 26);
      px[k] = Math.round(blades * 255); px[k + 1] = Math.round(clods * 255); px[k + 2] = Math.round(chalk * 255); px[k + 3] = Math.round(grit * 255);
    }
    return px;
  }
  function detailNormals(n, px) {
    var out = new Uint8Array(n * n * 4);
    function hgt(i, j) { var k = ((((j % n) + n) % n) * n + (((i % n) + n) % n)) * 4; return (px[k] * 0.5 + px[k + 1] * 0.35 + px[k + 3] * 0.15) / 255; }
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) {
      var dx = (hgt(i + 1, j) - hgt(i - 1, j)) * 2.5, dy = (hgt(i, j + 1) - hgt(i, j - 1)) * 2.5, l = Math.sqrt(dx * dx + dy * dy + 1), k = (j * n + i) * 4;
      out[k] = Math.round((-dx / l * 0.5 + 0.5) * 255); out[k + 1] = Math.round((dy / l * 0.5 + 0.5) * 255); out[k + 2] = Math.round((1 / l * 0.5 + 0.5) * 255); out[k + 3] = 255;
    }
    return out;
  }

  /* ---- three.js ---- */

  /* heightFog patches a material's shader (call it from onBeforeCompile) so its fog is exponential height fog: thick in the
     valley, thinning with height, integrated along the ray from the eye past the first FOG_START m of clear air (so the
     near valley keeps its colour and the haze gathers on the ridges), in the scene's fog colour as the sky shows it. The
     scene's FogExp2 density is the density at sea level; FOG_FALL how fast it thins per metre up. */
  function heightFog(shader) {
    shader.uniforms.uFogFall = { value: FOG_FALL };
    shader.uniforms.uFogStart = { value: FOG_START };
    shader.fragmentShader = shader.fragmentShader
      .replace("#include <fog_pars_fragment>", "#include <fog_pars_fragment>\nuniform float uFogFall;\nuniform float uFogStart;")
      .replace("#include <fog_fragment>", [
        "#ifdef USE_FOG",
        "  vec3 fogRay = transpose(mat3(viewMatrix)) * -vViewPosition;",
        "  float fogK = uFogFall * fogRay.y;",
        "  float fogLen = abs(fogK) > 1e-4 ? (1.0 - exp(-fogK)) / fogK : 1.0;",
        "  float fogAmount = 1.0 - exp(-fogDensity * exp(-uFogFall * cameraPosition.y) * max(0.0, length(fogRay) - uFogStart) * fogLen);",
        "  gl_FragColor.rgb = mix(gl_FragColor.rgb, sRGBTransferOETF(vec4(fogColor, 1.0)).rgb, clamp(fogAmount, 0.0, 1.0));",
        "#endif"
      ].join("\n"));
  }

  /* blend patches the terrain's MeshStandardMaterial: the splat's four material weights scale the colour map by each
     material's detail pattern (faded out with distance, where it would only shimmer) and set the roughness. */
  function blend(shader, splat, detail) {
    shader.uniforms.uSplat = { value: splat };
    shader.uniforms.uDetail = { value: detail };
    shader.fragmentShader = shader.fragmentShader
      .replace("#include <map_pars_fragment>", "#include <map_pars_fragment>\nuniform sampler2D uSplat;\nuniform sampler2D uDetail;")
      .replace("#include <map_fragment>", [
        "#include <map_fragment>",
        "  vec3 splatW = texture2D(uSplat, vMapUv).rgb;",
        "  vec4 matW = vec4(splatW, max(0.0, 1.0 - splatW.r - splatW.g - splatW.b));",
        "  matW /= max(1e-3, matW.r + matW.g + matW.b + matW.a);",
        "  vec4 det = texture2D(uDetail, vMapUv * " + DETAIL_REPEAT.toFixed(1) + ");",
        "  float detNear = 1.0 - smoothstep(30.0, 140.0, length(vViewPosition));",
        "  diffuseColor.rgb *= mix(1.0, 0.72 + 0.56 * dot(matW, det), detNear);",
        "  diffuseColor.rgb = mix(diffuseColor.rgb, vec3(0.80, 0.78, 0.70), matW.b * 0.35 * detNear);"
      ].join("\n"))
      .replace("#include <roughnessmap_fragment>", "float roughnessFactor = dot(matW, vec4(0.96, 0.9, 0.78, 0.88));");
  }

  /* grid builds a BufferGeometry for the n x n heightfield hs over size m, uv (0,1) at the north-west corner so a canvas
     texture's top row lies along the north edge. */
  function grid(THREE, hs, n, size) {
    var pos = new Float32Array(n * n * 3), uv = new Float32Array(n * n * 2), step = size / (n - 1), half = size / 2;
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) {
      var k = j * n + i;
      pos[k * 3] = -half + i * step; pos[k * 3 + 1] = hs[k]; pos[k * 3 + 2] = -half + j * step;
      uv[k * 2] = i / (n - 1); uv[k * 2 + 1] = 1 - j / (n - 1);
    }
    var idx = new Uint32Array((n - 1) * (n - 1) * 6), t = 0;
    for (j = 0; j + 1 < n; j++) for (i = 0; i + 1 < n; i++) {
      var a = j * n + i, b = a + 1, c = a + n, d = c + 1;
      idx[t++] = a; idx[t++] = c; idx[t++] = b; idx[t++] = b; idx[t++] = c; idx[t++] = d;
    }
    var g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.BufferAttribute(pos, 3));
    g.setAttribute("uv", new THREE.BufferAttribute(uv, 2));
    g.setIndex(new THREE.BufferAttribute(idx, 1));
    g.computeVertexNormals();
    return g;
  }

  /* ring builds the far land: FAR_CELL m cells out to FAR m on every side, coloured woods and fields by vertex. It runs on
     under the field, FAR_UNDER m down, and is FAR_DROP m low at the seam, so the field's edge always lies on top and no
     crack between the two can show the sky. */
  function ring(THREE) {
    var n = FAR * 2 / FAR_CELL + 1, pos = new Float32Array(n * n * 3), col = new Float32Array(n * n * 3), c = new THREE.Color();
    for (var j = 0; j < n; j++) for (var i = 0; i < n; i++) {
      var x = -FAR + i * FAR_CELL, z = -FAR + j * FAR_CELL, k = (j * n + i) * 3;
      var inside = x > -SIZE / 2 && x < SIZE / 2 && z > -SIZE / 2 && z < SIZE / 2;
      pos[k] = x; pos[k + 1] = height(x, z) - (inside ? FAR_UNDER : FAR_DROP); pos[k + 2] = z;
      c.set(wooded(x, z) > 0 ? WOOD[0] : hash(i, j, SEED + 30) < 0.5 ? MEADOW[2] : CROPS[ROTATION[Math.floor(hash(i, j, SEED + 31) * 6)][0]].c[0]);
      col[k] = c.r; col[k + 1] = c.g; col[k + 2] = c.b;
    }
    var idx = [];
    for (j = 0; j + 1 < n; j++) for (i = 0; i + 1 < n; i++) {
      var a = j * n + i, b = a + 1, cc = a + n, d = cc + 1;
      idx.push(a, cc, b, b, cc, d);
    }
    var g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.BufferAttribute(pos, 3));
    g.setAttribute("color", new THREE.BufferAttribute(col, 3));
    g.setIndex(idx);
    g.computeVertexNormals();
    var m = new THREE.MeshStandardMaterial({ vertexColors: true, roughness: 1 });
    m.onBeforeCompile = heightFog;
    var mesh = new THREE.Mesh(g, m);
    mesh.name = "far";
    return mesh;
  }

  /* build(THREE, doc) -> a Group of the land: the field ("land") and the far ring ("far"). With a document it paints the
     field map's two canvases and makes the detail textures; without one (node) the field is a plain meadow green. */
  function build(THREE, doc) {
    var group = new THREE.Group(), hs = heightfield(N, SIZE);
    var mat = new THREE.MeshStandardMaterial({ color: doc ? 0xffffff : MEADOW[0], roughness: 0.95 }), splat = null;
    if (doc) {
      var tex = function (layer) {
        var cv = doc.createElement("canvas");
        cv.width = cv.height = MAP;
        paint(cv.getContext("2d"), MAP, layer);
        var t = new THREE.CanvasTexture(cv);
        t.anisotropy = 8;
        if (layer === "albedo") t.colorSpace = THREE.SRGBColorSpace;
        return t;
      };
      var px = detailPixels(DETAIL), wrap = function (t) {
        t.wrapS = t.wrapT = THREE.RepeatWrapping; t.magFilter = THREE.LinearFilter; t.minFilter = THREE.LinearMipmapLinearFilter;
        t.generateMipmaps = true; t.anisotropy = 8; t.needsUpdate = true; return t;
      };
      var detail = wrap(new THREE.DataTexture(px, DETAIL, DETAIL)), normals = wrap(new THREE.DataTexture(detailNormals(DETAIL, px), DETAIL, DETAIL));
      normals.repeat.set(DETAIL_REPEAT, DETAIL_REPEAT);
      splat = tex("splat");
      mat.map = tex("albedo");
      mat.normalMap = normals;
      mat.normalScale = new THREE.Vector2(0.7, 0.7);
      mat.onBeforeCompile = function (shader) { blend(shader, splat, detail); heightFog(shader); };
    } else {
      mat.onBeforeCompile = heightFog;
    }
    var land = new THREE.Mesh(grid(THREE, hs, N, SIZE), mat);
    land.name = "land";
    land.userData.splat = splat || null; // FLORA reads the grass's weight from it
    group.add(land, ring(THREE));
    group.name = "lands";
    return group;
  }

  return {
    SIZE: SIZE, N: N, FAR: FAR, FAR_CELL: FAR_CELL, FAR_DROP: FAR_DROP, FAR_UNDER: FAR_UNDER, CAM_Z: CAM_Z, CREST_Z: CREST_Z, EYE: EYE, MAP: MAP,
    FOG_DENSITY: FOG_DENSITY, FOG_FALL: FOG_FALL, FOG_START: FOG_START, STONE: STONE, KNOLL: KNOLL, POND: POND, FARM: FARM, ORCHARD: ORCHARD, LANE: LANE, PATH: PATH,
    CROPS: CROPS, hash: hash, noise: noise, fbm: fbm, profile: profile, river: river, height: height, pondLevel: pondLevel, heightfield: heightfield,
    camera: camera, fields: fields, wooded: wooded, paint: paint, detailPixels: detailPixels, heightFog: heightFog, build: build
  };
})();
if (typeof module !== "undefined") module.exports = LAND;
