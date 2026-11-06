/* ART: pixel sprites and painters for /garden. Uses only ctx.fillStyle and ctx.fillRect (plus ctx.drawImage of the
   canvases a spriteCache hands back, when the page passes one, and of a plan's arms PNG, drawArms) so it also runs under a
   node mock.
   Ported from the Homestead prototype (gardenio/docs/prototypes/homestead); the sprite and palette data is unchanged. */
var ART = (function () {
  "use strict";

  /* ---------- palettes ---------- */
  var DAY = {
    k: "#2a1a14", K: "#5a3a2c",
    g: "#5fae3e", h: "#7cc957", d: "#458f36", D: "#357a30",
    s: "#a8703c", t: "#7e4c26", u: "#c8905a",
    l: "#35a03c", m: "#5ccf4f", n: "#a6ee7a", L: "#1f6e34",
    w: "#3a86e0", x: "#78c0f4", y: "#2757b0", z: "#dff6ff",
    b: "#b5793f", B: "#85532a", c: "#dba35e",
    r: "#9ea4b4", R: "#6c728a", q: "#d4d8e4",
    e: "#d94a3a", E: "#9a2c2c", f: "#ff8e7c",
    o: "#f28c28", O: "#b45c12", p: "#ffc850",
    Y: "#f2d44a", Z: "#b89824",
    v: "#a672d8", V: "#6c46a4", W: "#dcb8f6",
    a: "#f6caa0", A: "#c48c66",
    j: "#fffaf0", J: "#d9d0c0",
    i: "#4c7ae0", I: "#2f4c9c",
    F: "#ff9ec4", P: "#e4cc94", Q: "#bd9f66", N: "#f2e2b4",
    G: "#ffe680", T: "#8c7a3c",
    M: "#2a1a14"
  };
  var EMISSIVE = { G: 1, f: 0, p: 0 };
  /* M: rim() of a mark on soil, k by day and dimmed like k at night. The pale moonlit rim (#a8b4e8, gd-bN2j5Evukh) read as a
     white outline round every step sprite; the owner asked for it gone (11-02). */
  function hex(c) { return [parseInt(c.substr(1, 2), 16), parseInt(c.substr(3, 2), 16), parseInt(c.substr(5, 2), 16)]; }
  function toHex(r) { return "#" + r.map(function (v) { v = Math.max(0, Math.min(255, Math.round(v))); return (v < 16 ? "0" : "") + v.toString(16); }).join(""); }
  function night(c) { var r = hex(c), l = (r[0] + r[1] + r[2]) / 3; return toHex([r[0] * .42 + l * .1 + 6, r[1] * .5 + l * .1 + 14, r[2] * .72 + l * .12 + 46]); }
  /* id names the palette for the sprite cache: scheme plus the tag (or the recoloured values when there is no tag), so day,
     night and each biome keep separate bitmaps of the same map. The same id is the same colours, so each palette is built once
     and shared (callers only read it): the map asks for one per biome per frame, and a night one recolours ~60 hexes. */
  var PALS = {}, NPALS = 0;
  function palette(isNight, extra, tag) {
    var k, id = isNight ? "n" : "d";
    if (extra && !tag) for (k in extra) id += k + extra[k];
    id += tag || "";
    if (PALS[id]) return PALS[id];
    var out = {};
    for (k in DAY) out[k] = !isNight || EMISSIVE[k] ? DAY[k] : night(DAY[k]);
    if (extra) for (k in extra) out[k] = isNight ? night(extra[k]) : extra[k];
    out.id = id;
    if (++NPALS > 512) { PALS = {}; NPALS = 1; }   // a bound for a long session of gardeners' palettes
    return (PALS[id] = out);
  }
  function shade(c, f) { var r = hex(c); return toHex([r[0] * f, r[1] * f, r[2] * f]); }

  /* ---------- drawing primitives ---------- */
  function putMap(ctx, rows, x, y, pal, s, remap) {
    s = s || 1;
    for (var r = 0; r < rows.length; r++) {
      var row = rows[r], c = 0, n = row.length;
      while (c < n) {
        var ch = row.charAt(c);
        if (ch === ".") { c++; continue; }
        var e = c + 1;
        while (e < n && row.charAt(e) === ch) e++;
        var col = pal[(remap && remap[ch]) || ch];
        if (col) { ctx.fillStyle = col; ctx.fillRect(x + c * s, y + r * s, (e - c) * s, s); }
        c = e;
      }
    }
  }
  function box(ctx, pal, ch, x, y, w, h) { ctx.fillStyle = pal[ch]; ctx.fillRect(x, y, w, h); }
  /* A cache of tile sprites, each painted once with putMap into a small canvas and blitted with drawImage after that: a native
     512x512 chunk of ground, fences and soil is ~1-2k drawImage instead of ~100k fillRect. art.js has no DOM, so the page passes
     createCanvas(w, h); the key is the map's name plus the palette's id, so day, night and the eight biome palettes never share a
     bitmap. The terrain painters below take the cache as an optional last argument and fall back to fillRect runs without it. */
  function spriteCache(createCanvas) {
    var m = {}, n = 0;
    var paint = function (name, pal, w, h, fn) {   // fn(ctx) paints the w x h bitmap once per name and palette
      var k = name + "|" + pal.id, c = m[k];
      if (c) return c;
      c = createCanvas(w, h); fn(c.getContext("2d")); n++;
      return (m[k] = c);
    };
    return {
      get: function (name, rows, pal, w, h) { return paint(name, pal, w || 16, h || 16, function (x) { putMap(x, rows, 0, 0, pal, 1); }); },
      paint: paint,
      size: function () { return n; }
    };
  }
  function tile(ctx, sp, name, rows, x, y, pal) { if (sp) ctx.drawImage(sp.get(name, rows, pal, rows[0].length, rows.length), x, y); else putMap(ctx, rows, x, y, pal, 1); }
  var hs = function (a, b, c) { var h = (a * 374761393 + b * 668265263 + c * 2147483647) | 0; h = Math.imul(h ^ (h >>> 13), 1274126177); return ((h ^ (h >>> 16)) >>> 0) / 4294967296; };

  /* raster helpers for procedural sprites */
  function grid(w, h) { var g = []; for (var j = 0; j < h; j++) { g.push([]); for (var i = 0; i < w; i++) g[j].push("."); } return g; }
  function rect(g, x, y, w, h, c) { for (var j = y; j < y + h; j++) for (var i = x; i < x + w; i++) if (g[j] && i >= 0 && i < g[0].length) g[j][i] = c; }
  function disc(g, cx, cy, r, c) { for (var j = 0; j < g.length; j++) for (var i = 0; i < g[0].length; i++) if ((i - cx) * (i - cx) + (j - cy) * (j - cy) <= r * r) g[j][i] = c; }
  function rows(g) { return g.map(function (r) { return r.join(""); }); }
  function outline(g, c) {
    var h = g.length, w = g[0].length, add = [];
    for (var j = 0; j < h; j++) for (var i = 0; i < w; i++) if (g[j][i] === ".") {
      if ((j > 0 && g[j - 1][i] !== "." && g[j - 1][i] !== c) || (j < h - 1 && g[j + 1][i] !== "." && g[j + 1][i] !== c) || (i > 0 && g[j][i - 1] !== "." && g[j][i - 1] !== c) || (i < w - 1 && g[j][i + 1] !== "." && g[j][i + 1] !== c)) add.push([i, j]);
    }
    add.forEach(function (p) { g[p[1]][p[0]] = c; });
  }
  /* each k beside a "." or the edge made M */
  function rim(rs) {
    var at = function (i, j) { return j < 0 || j >= rs.length || i < 0 || i >= rs[j].length ? "." : rs[j].charAt(i); };
    return rs.map(function (r, j) { return r.replace(/k/g, function (c, i) { return at(i - 1, j) === "." || at(i + 1, j) === "." || at(i, j - 1) === "." || at(i, j + 1) === "." ? "M" : c; }); });
  }
  function merge(layers) { // overlay equal-size row arrays; later layers win
    var h = 0, w = 0; layers.forEach(function (l) { h = Math.max(h, l.length); l.forEach(function (r) { w = Math.max(w, r.length); }); });
    var g = grid(w, h);
    layers.forEach(function (l) { l.forEach(function (r, j) { for (var i = 0; i < r.length; i++) if (r.charAt(i) !== ".") g[j][i] = r.charAt(i); }); });
    return rows(g);
  }

  /* ---------- hand-drawn sprites (16 wide) ---------- */
  var S = {};
  S.mound = [
    "................", "................", "................", "................", "................", "................", "................", "................", "................",
    "....kkkkkkkk....", "..kkuuuuuuuukk..", ".kuuussssssstkk.", ".kussssssssssttk", ".ksssssssssssttk", "..kttttttttttk..", "...kkkkkkkkkk..."
  ];
  S.seed = [
    "................", "................", "................", "................", "................", "................", "................", "................", "................",
    "................", "................", "......kK........", "......Kk........", "................", "................", "................"
  ];
  S.sprout = [
    "................", "................", "................", "................",
    "....kkk..kkk....", "...kmnnkkmnnk...", "...kmmlkkLmmk...", "....kkLmmLkk....",
    "......kLLk......", "......kLLk......", "................", "................", "................", "................", "................", "................"
  ];
  S.leafy = [
    "................", "................", "....kk....kk....", "...kmnk..knmk...", "..kmnmmkkmmmnk..", "..kkmmLmmLmmkk..",
    "...kLmmLLmmLk...", "....kkmLLmkk....", ".....kkLLkk.....", "......kLLk......", "................", "................", "................", "................", "................", "................"
  ];
  S.turnip = [
    "................", "................", "................", "................", "................", "................", "................", "......kLLk......",
    ".....kLmmLk.....", "....kWWWWWWk....", "...kWjjWWWvvk...", "...kWjWWWWvvk...", "....kWWvvvVk....", ".....kkVVkk.....", "................", "................"
  ];
  S.pumpkin = [
    "................", "................", "................", "................", "................", "................", "................", ".......kLk......",
    "...kkkkkLkkkk...", "..kppoooLooOOk..", ".kpooookoooOOOk.", ".kpoookkkkooOOk.", ".kpoooookoooOOk.", "..kooookooOOOk..", "...kkOOkOOkkk...", "................"
  ];
  S.carrot = [
    "................", "................", "................", "................", "................", "......kLLk......", ".....kLmmLk.....", "....kmmnLmmk....",
    ".....kkLLkk.....", "....kookkook....", "....kpooooOk....", ".....kpooOOk....", ".....kooOOk.....", "......kOOk......", ".......kk.......", "................"
  ];
  S.tomato = [
    "................", "................", "................", "................", "................", "................", ".....kk.kk......", "....kLLkLLk.....",
    "...kkLmLLmkk....", "..keffeeeeeEk...", ".kefffeeeeeeEk..", ".keffeeeeeeEEk..", ".keeeeeeeeeEEk..", "..keeeeeeeEEk...", "...kkEEEEEkk....", ".....kkkkk......"
  ];
  S.stubble = [
    "................", "................", "................", "................", "................", "................", "....p..p..p.....", "....Y..Y..Y.....",
    "....Z..Z..Z.....", "....Z..Z..ZY....", "................", "................", "................", "................", "................", "................"
  ];
  /* A harvest cell's bases (harvest spec 4.1): the sheaf, a fan of ears on stalks gathered to a Z tie at mid height, over
     a Z shadow, for a step that yielded; trampled, the stalks bent flat, for one abandoned. Both keep left of
     column 11 and under row 5, clear of the badge slots (BADGE_AT). */
  S.sheaf = [
    "................", "................", "................", "................", "................",
    ".p.p.p.p.p......", ".pYpYpYpYp......", "..YpYpYpY.......", "...YYYYY........", "....YYY.........", "...ZZZZZ........",
    "....YYY.........", "...YYYYY........", "..Y.Y.Y.Y.......", ".ZZZZZZZZZ......", "................"
  ];
  S.trampled = [
    "................", "................", "................", "................", "................", "................", "................", "................", "................",
    "...Z.....Z......", ".ZZTZZ.ZZTZ.....", "..T..ZZZ..T.....", ".TTZZT.TZZT.....", "................", "................", "................"
  ];
  S.crate = [
    "................", "................", "................", "................", "................", "................", "................", "................", "................", "................",
    "........kkkkkkk.", "........kcccccck", "........kbkbkbbk", "........kBBBBBBk", "........kkkkkkk.", "................"
  ];
  S.rock = [
    "................", "................", "................", "................", ".....kkkkkk.....", "...kkqqqrrrkk...", "..kqqrrrrrrRRk..", "..kqrrrrrrrRRk..",
    ".kkrrrrrrrrRRkk.", ".krrrrrrrrRRRRk.", ".kkRRRRRRRRRRkk.", "..kkkkkkkkkkkk..", "................", "................", "................", "................"
  ];
  S.weeds = [
    "................", "................", "................", "................", "................", "................", "................", "................", "................",
    "l..............l", "Lm............ml", "LLl..........lLL", ".L............L.", "................", "................", "................"
  ];
  S.beetle = [
    "................", "................", "................", "................", "................", "................", "................", "...........k.k..",
    "..........kkkkk.", "..........keKek.", "..........kkkkk.", ".........k.k.k.k", "................", "................", "................", "................"
  ];
  S.flag = [ // priority pennant, X is recoloured per priority
    "................", "................", "............k...", "............kXXk", "............kXXk", "............kk..", "............k...", "............k...", "............k...", "............k...", "............k...", "................", "................", "................", "................", "................"
  ];
  S.drops1 = ["................", "..w.........w...", "..x.........x...", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................"];
  S.drops2 = ["................", "................", "..x...........w.", "..w...........x.", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................"];
  S.spark1 = ["................", "..Y.............", ".YjY..........Y.", "..Y..........YjY", "..............Y.", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................", "................"];
  S.spark2 = ["................", "................", "..............Y.", "...........Y.YjY", "..Y...........Y.", ".YjY............", "..Y.............", "................", "................", "................", "................", "................", "................", "................", "................", "................"];

  S.mulch = [
    "................", "................", "................", "................", "................", ".......kk.......", ".....kkYYkk.....", "....kYYpYYZk....",
    "...kYpYYYYYZk...", "..kYYYYpYYYZZk..", "..kYYYYYYYYZZk..", "..kZZZZZZZZZZk..", "...kkkkkkkkkk...", "................", "................", "................"
  ];
  S.lock = ["...kkk...", "..kYYYk..", "..kY.Yk..", ".kkkkkkk.", ".kYYYYYk.", ".kYYkYYk.", ".kYYkYYk.", ".kZZZZZk.", ".kkkkkkk."];
  S.cloud = ["....kkk.......", "..kkjjjkk.....", ".kjjjjjjjkk...", "kjjjjjjjjjjk..", "kqqqqqqqqqqqk.", ".kkkkkkkkkkk..", "..w..w..w.w...", "...w..w..w....", ".w..w....w.w.."];
  S.star = ["....k....", "...kYk...", "kkkkYkkkk", "kYYYjYYYk", ".kYYjYYk.", "..kYYYk..", ".kYYkYYk.", ".kYk.kYk.", "..k...k.."];

  S.frost = [
    "................", "................", "................", "................", "................", "................", ".....z..........", "....zxz....z....",
    "...zxxz...zxz...", "..zxxxxz.zxxz...", "..zxzzxzzxzzz...", ".kzzxzkzzzxzzk..", "................", "................", "................", "................"
  ];
  S.post = [
    "................", "...kkkkkkkkkk...", "...kjjjjjjjjk...", "...kjjeeeejjk...", "...kjjjjjeejk...", "...kjjjeeejjk...", "...kjjjejjjjk...", "...kjjjjjjjjk...",
    "...kjjjejjjjk...", "...kkkkkkkkkk...", "......kbck......", "......kbck......", "......kbBk......", "................", "................", "................"
  ];
  S.chest = [
    "................", "................", "................", "................", "................", "....kkkkkkkk....", "...kbbbbbbbbk...", "..kbcbbbbbbcbk..",
    "..kkkkkkkkkkkk..", "..kBBBBYYBBBBk..", "..kBBBBYkBBBBk..", "..kBBBBBBBBBBk..", "..kkkkkkkkkkkk..", "................", "................", "................"
  ];
  S.pile = [
    "................", "................", "................", "................", "................", "......kkkkkk....", "......kccccck...", "......kbbbbbk...",
    "..kkkkkkBBBBkkk.", "..kcccckkkkcccck", "..kbbbbkcccbbbbk", "..kBBBBkbbbBBBBk", "..kkkkkkkkkkkkkk", "................", "................", "................"
  ];
  S.stake = [".kkkkk...", ".kjjjjk..", ".kjjjjjk.", ".kjjjjk..", ".kkkkk...", ".kb......", ".kb......", ".kB......", ".kB......", "kkkk....."];

  /* villager, 16x24. H/h hat, S/s shirt, I/B trousers and boots */
  var VIL0 = [
    "................", ".....kkkkkk.....", "....kHHHHHHk....", "...kHHhHHHHHk...", "..kkkkkkkkkkkk..", "..kaaaaaaaaaak..", "..kaakaaaakaak..", "..kaaaaaaaaaak..",
    "...kaaaAAaaak...", "....kkkkkkkk....", "...kSSSSSSSSk...", "..kSSSSssSSSSk..", ".kaSSSSssSSSSak.", ".kaSSSSssSSSSak.", "..kkSSSSSSSSkk..", "...kSSSSSSSSk...",
    "...kkkkkkkkkk...", "...kIIIIkIIIk...", "...kIIIIkIIIk...", "...kIIIIkIIIk...", "...kkkkkkkkkk...", "..kBBBBBkBBBBk..", "..kkkkkkkkkkkk..", "................"
  ];
  var VIL1 = [
    "................", "................", ".....kkkkkk.....", "....kHHHHHHk....", "...kHHhHHHHHk...", "..kkkkkkkkkkkk..", "..kaaaaaaaaaak..", "..kaakaaaakaak..",
    "..kaaaaaaaaaak..", "...kaaaAAaaak...", "....kkkkkkkk....", "...kSSSSSSSSk...", "..kSSSSssSSSSk..", ".kaSSSSssSSSSak.", ".kaSSSSssSSSSak.", "..kkSSSSSSSSkk..",
    "...kSSSSSSSSk...", "..kIIIIkkIIIIk..", ".kIIIIk..kIIIIk.", ".kIIIIk..kIIIIk.", ".kkkkkk..kkkkkk.", "kBBBBk....kBBBBk", "kkkkkk....kkkkkk", "................"
  ];
  var VILSIT = [
    "................", "................", "................", ".....kkkkkk.....", "....kHHHHHHk....", "...kHHhHHHHHk...", "..kkkkkkkkkkkk..", "..kaaaaaaaaaak..",
    "..kaakaaaakaak..", "..kaaaaaaaaaak..", "...kaaaAAaaak...", "....kkkkkkkk....", "...kSSSSSSSSk...", "..kSSSSssSSSSk..", ".kaSSSSssSSSSak.", ".kaSSSSssSSSSak.",
    "..kkSSSSSSSSkk..", ".kIIIIIIIIIIIIk.", ".kIIIIIIIIIIIIk.", ".kkkkkkkkkkkkkk.", ".kBBBBkkkkBBBBk.", "..kkkkk..kkkkk..", "................", "................"
  ];
  var SCARE = [
    "................", "......kkkk......", "....kkYYYYkk....", "..kYYYYYYYYYYk..", "...kkkkkkkkkk...", "....kTTTTTTk....", "....kTkTTkTk....", "....kTTTTTTk....",
    ".....kTkkTk.....", "..kkkkbbbbkkkk..", ".kYbbbbbbbbbbYk.", "kYk.kvvvvvvk.kYk", "....kvvVvvvk....", "....kvvvvVvk....", ".....kvvvvk.....", "......kbbk......",
    "......kbBk......", "......kbBk......", "......kbBk......", "......kbBk......", ".....kkbBkk.....", "....kkkkkkkk....", "................", "................"
  ];
  /* Phase 2's reasons (web/reason.js; homestead-agents-as-people.md section 2.3): the villager asleep (eyes shut; zzz rises over
     it), slumped (stale: head and arms hang, a wilted sprout on its hat), and the sprout on the hat of one starting up. */
  var shut = function (m, j) { return m.map(function (r, i) { return i === j ? "..kakkaaaakkak.." : r; }); };
  var VILWILT = [
    "................", "................", "................", ".....kkkkkk.....", "....kHHHHHHk....", "...kHHhHHHHHk...", "..kkkkkkkkkkkk..", "..kaaaaaaaaaak..",
    "..kaaaaaaaaaak..", "..kaakaaaakaak..", "...kaaaAAaaak...", "..kkkkkkkkkkkk..", "..kSSSSssSSSSk..", ".kSSSSSssSSSSSk.", ".kaSSSSSSSSSSak.", ".kakSSSSSSSSkak.",
    "...kkkkkkkkkk...", "...kIIIIkIIIk...", "...kIIIIkIIIk...", "...kIIIIkIIIk...", "...kkkkkkkkkk...", "..kBBBBBkBBBBk..", "..kkkkkkkkkkkk..", "................"
  ];
  var VILSLEEP = shut(VILSIT, 8);
  var MARKS = (function () {
    var mk = function (w, h, draw) { var g = grid(w + 4, h + 4); draw(function (x, y, c) { g[y + 2][x + 2] = c; }); outline(g, "k"); outline(g, "@"); return rows(g); };
    var z = function (put, x, y, n) { for (var i = 0; i < n; i++) { put(x + i, y, "j"); put(x + i, y + n - 1, "j"); put(x + n - 1 - i, y + i, "j"); } };
    var plant = function (src, remap) { return function (put) { src.forEach(function (r, j) { for (var i = 0; i < r.length; i++) if (r.charAt(i) !== ".") put(i, j, remap && remap[r.charAt(i)] || r.charAt(i)); }); }; };
    return {
      zzz: mk(13, 12, function (put) { z(put, 0, 9, 3); z(put, 3, 4, 4); z(put, 8, 0, 5); }),
      sprout: mk(5, 4, plant(["m...m", ".mlm.", "..l..", "..L.."])),
      wilted: mk(5, 5, plant(["..l..", ".mlm.", "m.l.m", "..l..", "..L.."], wiltMap()))
    };
  })();
  var MAIL_UP = [
    "...........kkk..", "...........keek.", "...........kkkk.", "............k...", "....kkkkkkkkk...", "...kiiiiiiiiik..", "..kiwxxxiiiiik..", "..kiiiiiiiiiik..",
    "..kiiiiiiiiiik..", "..kIIIIIIIIIIk..", "...kkkkkkkkkk...", "......kbck......", "......kbck......", "......kbBk......", "......kbBk......", "......kbBk......",
    "......kbBk......", "......kbBk......", "......kbBk......", ".....kkbBkk.....", "................", "................"
  ];
  var MAIL_DOWN = [
    "................", "................", "................", "................", "....kkkkkkkkk...", "...kiiiiiiiiik..", "..kiwxxxiiiiik..", "..kiiiiiiiiiike.",
    "..kiiiiiiiiiikee", "..kIIIIIIIIIIk.k", "...kkkkkkkkkk...", "......kbck......", "......kbck......", "......kbBk......", "......kbBk......", "......kbBk......",
    "......kbBk......", "......kbBk......", "......kbBk......", ".....kkbBkk.....", "................", "................"
  ];
  var DOG0 = [
    "................", "................", "................", "...........kk...", "..........kddk..", "......kkkkkddkk.", ".....kdddddddAk.", "..k.kdddddddkkk.",
    "..dkdddddddddAk.", "..kdddddddddk...", "...kdddddddk....", "...kdkddkdk.....", "...kkkkkkkk.....", "................", "................", "................"
  ];
  DOG0 = DOG0.map(function (r) { return r.replace(/d/g, "c").replace(/A/g, "b"); });
  var DOG1 = DOG0.slice(); DOG1[6] = ".....kcccccccbk."; DOG1[7] = "...kkcccccccckkk"; DOG1[8] = "..kcccccccccbk..";
  S.sign = [ // wooden plate, 24 wide pieces are tiled by the painter
    "kkkkkkkkkkkkkkkk", "kccccccccccccccK", "kbbbbbbbbbbbbbbK", "kbbbbbbbbbbbbbbK", "kBBBBBBBBBBBBBBK", "kkkkkkkkkkkkkkkk"
  ];

  /* ---------- procedural sprites ---------- */
  var grass = [];
  (function () {
    for (var v = 0; v < 5; v++) {
      var g = grid(16, 16);
      rect(g, 0, 0, 16, 16, "g");
      for (var i = 0; i < 14; i++) { var x = Math.floor(hs(v, i, 1) * 16), y = Math.floor(hs(v, i, 2) * 16); g[y][x] = hs(v, i, 3) > .5 ? "h" : "d"; }
      for (var t = 0; t < 2; t++) { var tx = 1 + Math.floor(hs(v, t, 7) * 13), ty = 3 + Math.floor(hs(v, t, 8) * 11); g[ty][tx] = "d"; g[ty - 1][tx - 1] = "h"; g[ty - 1][tx + 1 > 15 ? 15 : tx + 1] = "h"; g[ty - 2][tx] = "h"; }
      if (v === 3) { g[9][5] = "l"; g[10][5] = "L"; g[8][5] = "j"; g[8][4] = "Y"; g[8][6] = "Y"; g[7][5] = "Y"; g[9][4] = "j"; g[9][6] = "j"; }
      if (v === 4) { g[5][11] = "L"; g[6][11] = "L"; g[4][11] = "F"; g[4][10] = "F"; g[4][12] = "F"; g[3][11] = "F"; g[5][10] = "F"; g[5][12] = "F"; g[4][11] = "Y"; }
      grass.push(rows(g));
    }
  })();
  var pathT = [];
  (function () {
    for (var v = 0; v < 3; v++) {
      var g = grid(16, 16); rect(g, 0, 0, 16, 16, "P");
      for (var i = 0; i < 16; i++) { var x = Math.floor(hs(v, i, 11) * 16), y = Math.floor(hs(v, i, 12) * 16); g[y][x] = hs(v, i, 13) > .5 ? "N" : "Q"; }
      pathT.push(rows(g));
    }
  })();
  function soilPix(px, py) {
    var m = ((py % 8) + 8) % 8, n = hs(px, py, 21);
    if (m === 0) return n > .7 ? "s" : "u";
    if (m === 3 || m === 4) return n > .8 ? "s" : "t";
    return n > .93 ? "u" : n < .06 ? "t" : "s";
  }
  function soilRect(ctx, pal, x, y, w, h, s, remap) { // x,y,w,h in logical px at scale s; remap recolours (the dark blocks)
    for (var j = 0; j < h / s; j++) {
      var run = null, start = 0;
      for (var i = 0; i <= w / s; i++) {
        var ch = i < w / s ? soilPix(Math.floor(x / s) + i, Math.floor(y / s) + j) : null;
        if (ch !== run) { if (run) { ctx.fillStyle = pal[remap ? remap[run] : run]; ctx.fillRect(x + start * s, y + j * s, (i - start) * s, s); } run = ch; start = i; }
      }
    }
  }
  /* Soil as 16x16 tiles for the sprite cache: four variants cut from the same soilPix texture (rows keep the bed's furrow
     pattern, py % 8, because beds sit on the tile grid), picked per tile by hash so a bed does not visibly repeat. */
  var SOIL = [0, 1, 2, 3].map(function (v) { var g = grid(16, 16); for (var j = 0; j < 16; j++) for (var i = 0; i < 16; i++) g[j][i] = soilPix(i + 16 * v, j); return rows(g); });
  /* The second soil tone (cursor-grid spec 5.4): the same furrows one step darker. A bed's 3x3 blocks alternate the two, so its
     block structure reads without gutters. */
  var SOIL_DARK = { u: "s", s: "t", t: "K" };
  var SOIL2 = SOIL.map(function (t) { return t.map(function (r) { return r.replace(/[ust]/g, function (c) { return SOIL_DARK[c]; }); }); });
  function soilTiles(ctx, pal, x, y, w, h, sp, dark) { // x,y,w,h in world px; off the tile grid (or without a cache) it is soilRect
    if (!sp || ((x | y | w | h) & 15)) return soilRect(ctx, pal, x, y, w, h, 1, dark ? SOIL_DARK : null);
    for (var ty = y / 16; ty < (y + h) / 16; ty++) for (var tx = x / 16; tx < (x + w) / 16; tx++) { var v = Math.floor(hs(tx, ty, 23) * 4); tile(ctx, sp, (dark ? "S" : "s") + v, (dark ? SOIL2 : SOIL)[v], tx * 16, ty * 16, pal); }
  }
  var fenceH = (function () {
    var g = grid(16, 16);
    [3, 9].forEach(function (y) { rect(g, 0, y, 16, 1, "k"); rect(g, 0, y + 1, 16, 1, "c"); rect(g, 0, y + 2, 16, 1, "B"); rect(g, 0, y + 3, 16, 1, "k"); });
    rect(g, 6, 1, 4, 14, "k"); rect(g, 7, 2, 1, 12, "c"); rect(g, 8, 2, 1, 12, "b"); rect(g, 6, 14, 4, 1, "k");
    rect(g, 6, 15, 4, 1, "D");
    return rows(g);
  })();
  var fenceV = (function () {
    var g = grid(16, 16);
    rect(g, 6, 0, 4, 16, "k"); rect(g, 7, 0, 1, 16, "c"); rect(g, 8, 0, 1, 16, "B");
    rect(g, 4, 5, 8, 6, "k"); rect(g, 5, 6, 6, 1, "c"); rect(g, 5, 7, 6, 2, "b"); rect(g, 5, 9, 6, 1, "B");
    return rows(g);
  })();
  /* A planted plot's hedge (homestead-world-design.md 29.4): a 12 px band of leaves in the palette's dark green (d), lit from the
     top left, k round it. inb(i, j) is its shape, also past the tile's edge where it runs on, so no seam is lit or outlined.
     Bands span rows 2-13 and columns 2-13, so the corner's mask mirrored still meets them. */
  function hedgeTile(inb, seed) {
    var g = grid(16, 16), i, j;
    for (j = 0; j < 16; j++) for (i = 0; i < 16; i++) {
      if (!inb(i, j)) { if (inb(i - 1, j) || inb(i + 1, j) || inb(i, j - 1) || inb(i, j + 1)) g[j][i] = "k"; continue; }
      var lit = !inb(i, j - 1) || !inb(i, j - 2) || !inb(i - 1, j), dark = !inb(i, j + 1) || !inb(i, j + 2) || !inb(i + 1, j) || !inb(i + 2, j);
      var ch = lit ? "g" : dark ? "D" : "d", r = hs(i, j, seed);
      if (r > .84) ch = ch === "g" ? "d" : "D"; else if (r < .12) ch = ch === "D" ? "d" : lit ? "h" : "g";
      g[j][i] = ch;
    }
    return rows(g);
  }
  var md = function (v) { return ((v % 16) + 16) % 16; };
  var hedgeH = hedgeTile(function (i, j) { var m = md(i); return (j >= 2 && j <= 13) || (j === 1 && ((m >= 2 && m <= 5) || (m >= 9 && m <= 12))) || (j === 14 && m >= 5 && m <= 8); }, 41);
  var hedgeV = hedgeTile(function (i, j) { var m = md(j); return (i >= 2 && i <= 13) || (i === 1 && ((m >= 3 && m <= 6) || m >= 11)) || (i === 14 && m >= 6 && m <= 9); }, 43);
  /* hedgeC[k]: the corner, k = 0 top left, 1 top right, 2 bottom left, 3 bottom right; its outer corner rounded. */
  var hedgeC = [0, 1, 2, 3].map(function (k) {
    return hedgeTile(function (i, j) { if (k & 1) i = 15 - i; if (k & 2) j = 15 - j; return ((j >= 2 && j <= 13 && i >= 2) || (i >= 2 && i <= 13 && j >= 2)) && i + j > 5; }, 47 + k);
  });
  var BARN = (function () {
    var g = grid(96, 66);
    for (var y = 2; y < 28; y++) { var inset = Math.round((28 - y) * 18 / 26); for (var x = inset; x < 96 - inset; x++) { var band = Math.floor((y - 2) / 4) % 2; var shingle = (x + (Math.floor((y - 2) / 4) % 2) * 3) % 6 === 0; g[y][x] = shingle ? "E" : band ? "e" : "e"; if ((y - 2) % 4 === 3) g[y][x] = "E"; if (x === inset || x === inset + 1) g[y][x] = "f"; } }
    rect(g, 4, 28, 88, 2, "E");
    for (var wy = 30; wy < 64; wy++) for (var wx = 6; wx < 90; wx++) g[wy][wx] = (Math.floor((wx - 6) / 5) % 2) ? "c" : "b";
    for (var px = 6; px < 90; px += 5) rect(g, px, 30, 1, 34, "B");
    rect(g, 6, 30, 84, 2, "B"); rect(g, 6, 62, 84, 2, "B");
    rect(g, 36, 38, 24, 26, "B"); rect(g, 37, 39, 10, 25, "b"); rect(g, 49, 39, 10, 25, "b"); rect(g, 47, 39, 2, 25, "k");
    for (var d = 0; d < 10; d++) { g[39 + d * 2 + 0][37 + d] = "j"; g[39 + d * 2 + 1][37 + d] = "j"; g[39 + d * 2][58 - d] = "j"; g[39 + d * 2 + 1][58 - d] = "j"; }
    rect(g, 40, 14, 16, 11, "k"); rect(g, 41, 15, 14, 9, "B"); rect(g, 42, 16, 12, 7, "Y"); rect(g, 42, 19, 12, 1, "Z"); rect(g, 47, 15, 2, 9, "k");
    rect(g, 12, 38, 12, 10, "k"); rect(g, 13, 39, 10, 8, "x"); rect(g, 13, 39, 10, 2, "z"); rect(g, 17, 39, 2, 8, "k"); rect(g, 13, 43, 10, 1, "k");
    rect(g, 72, 38, 12, 10, "k"); rect(g, 73, 39, 10, 8, "x"); rect(g, 73, 39, 10, 2, "z"); rect(g, 77, 39, 2, 8, "k"); rect(g, 73, 43, 10, 1, "k");
    rect(g, 4, 64, 88, 2, "d");
    outline(g, "k");
    return rows(g);
  })();
  function plate(w) { var g = grid(w, 12); rect(g, 0, 0, w, 12, "b"); rect(g, 0, 1, w, 1, "c"); rect(g, 0, 10, w, 2, "B"); rect(g, 0, 0, w, 1, "k"); rect(g, 0, 11, w, 1, "k"); rect(g, 0, 0, 1, 12, "k"); rect(g, w - 1, 0, 1, 12, "k"); g[2][2] = "k"; g[2][w - 3] = "k"; g[9][2] = "k"; g[9][w - 3] = "k"; return rows(g); }

  /* ---------- bead -> composed 16x16 sprite ---------- */
  var FRUIT = { task: "turnip", feature: "pumpkin", chore: "carrot", bug: "tomato" };
  var PRIO = { 0: "e", 1: "o", 2: "Y", 3: "w", 4: "q" };
  function wiltMap() { return { l: "Z", m: "Y", n: "p", L: "T" }; }
  function beadRows(b, f) {
    var L = b.wild ? [] : [S.mound], remap = null;
    if (b.t === "pile") { L.push(S.pile); return merge(L); }
    if (b.t === "decision") { L.push(b.s === "closed" || b.s === "done" ? S.pile : S.post); return merge(L); }
    if (b.t === "escalation") { if (b.s === "closed" || b.s === "done") { L.push(S.pile); return merge(L); } L.push(escRows(b.fam, b.w, f)); return rim(merge(L)); }
    if (b.s === "open" && b.wild) { L.push(S.sprout); }
    else if (b.s === "open") { L.push(S.seed); var fl = S.flag.map(function (r) { return r.replace(/X/g, PRIO[b.p == null ? 2 : b.p]); }); if (b.p != null) L.push(fl); if (b.frozen) L.push(S.frost); }
    else if (b.s === "wip") { L.push(S.sprout); if (b.who && !b.stale) L.push(f % 2 ? S.drops1 : S.drops2); if (b.stale) remap = wiltMap(); }
    else if (b.s === "review") { L.push(S.leafy); L.push(S[FRUIT[b.t]]); L.push(f % 2 ? S.spark1 : S.spark2); }
    else if (b.s === "done") L.push(b.base === "sheaf" ? S.sheaf : b.base === "trampled" ? S.trampled : S.stubble);
    else if (b.s === "deferred") { L.push(S.mulch); }
    else if (b.s === "blocked") { L.push(S.weeds); L.push(S.rock); }
    if (b.t === "bug" && b.s !== "done") { var bt = S.beetle.slice(); if (f % 2) bt = bt.map(function (r, j) { return j === 8 ? "..........k.k..." : r; }); L.push(bt); }
    if (b.orphan) L.push(S.crate);
    var m = rim(merge(L));   // badges after: they keep their k
    if (remap) m = m.map(function (r) { return r.replace(/[lmnL]/g, function (c) { return remap[c]; }); });
    if (b.s === "done" && b.badges && b.badges.length) m = merge([m].concat(b.badges.map(badgeLayer)));
    return m;
  }

  /* ---------- harvest: a closed step's badges (harvest spec 4.2) ----------
     Six 5x5 marks, each composed into the cell as an extra 16x16 layer the way S.flag and S.frost are. A cell has room for
     two at 1x and four at 2x and above (BADGE_AT: the top row, right to left, then down the right edge), kept in priority
     order; the label row and the bead view say all six. Only the lantern's G flame is emissive, so at night the palette dims
     every other badge and a lit lantern marks the steps whose checks went green. */
  var BADGE = {
    crate: ["kkkkk", "kccck", "kbkbk", "kBBBk", "kkkkk"],
    rosette: [".YYY.", "YYjYY", ".YYY.", ".e.e.", ".e.e."],
    vial: [".kkk.", ".qzq.", "qzzzq", "qwwwq", ".qqq."],
    lantern: ["..k..", ".kkk.", "kGGGk", "kGGGk", ".kkk."],
    photo: ["kkkkk", "kxxjk", "kxxjk", "klllk", "kkkkk"],
    seal: [".EEE.", "EejeE", "EeeeE", "EeeeE", ".EEE."]
  };
  var BADGE_ORDER = ["crate", "rosette", "vial", "lantern", "photo", "seal"], BADGE_AT = [[11, 0], [6, 0], [11, 5], [11, 10]];
  var badgeMemo = {};
  function badgeLayer(k, i) {
    var key = k + i;
    if (badgeMemo[key]) return badgeMemo[key];
    var g = grid(16, 16), at = BADGE_AT[i];
    BADGE[k].forEach(function (r, j) { for (var c = 0; c < 5; c++) if (r.charAt(c) !== ".") g[at[1] + j][at[0] + c] = r.charAt(c); });
    return (badgeMemo[key] = rows(g));
  }
  /* A closed row's harvest object (farm_harvest.go) -> its badges, in priority order, each with its word. A ci that is not
     success lights nothing (its word is the status line's, step 5). */
  function harvestBadges(h) {
    h = h || {};
    var out = [], ux = h.ux || [];
    if (h.mr) out.push({ k: "crate", w: "merged " + h.mr });
    if (ux.length === 2 && ux[1] > 0) out.push({ k: "rosette", w: "graded " + ux[0] + "/" + ux[1] + " gates", n: ux[0] });
    if (h.tests > 0) out.push({ k: "vial", w: h.tests + (h.tests === 1 ? " test" : " tests") });
    if (h.ci === "success") out.push({ k: "lantern", w: "checks green" });
    if (h.pics > 0) out.push({ k: "photo", w: h.pics + (h.pics === 1 ? " picture" : " pictures") });
    if (h.rounds >= 1) out.push({ k: "seal", w: h.rounds === 1 ? "merged first time" : "merged after " + h.rounds + " rounds", n: h.rounds >= 2 ? h.rounds : 0 });
    return out;
  }
  /* A badge alone, for the words beside it (harvest spec 5 and 6.2): the 5x5 mark at 2 px a pixel on a 12 px canvas in the day
     palette, aria-hidden because the word beside it is its name. Only the page calls it (it makes a DOM canvas). */
  function badgeIcon(k) {
    var c = document.createElement("canvas");
    c.width = c.height = 12; c.className = "bdg"; c.setAttribute("aria-hidden", "true");
    if (BADGE[k]) putMap(c.getContext("2d"), BADGE[k], 1, 1, palette(false), 2);
    return c;
  }
  var HARVEST_WORD = { sheaf: "harvested", trampled: "abandoned" };
  /* A closed step in words for its hot's accessible name: the base word, then every badge's (state is never colour alone). */
  function harvestWords(t) { var h = t.harvest || {}; return [HARVEST_WORD[h.base] || "closed"].concat(harvestBadges(h).map(function (b) { return b.w; })); }
  /* A closed farm task -> the bead model beadRows draws: its base and the badges the cell has room for. wide is a cell drawn
     at 2x or more on a fine pointer (four badges, and the rosette's and the seal's digits); a phone keeps the 1x two. The
     key names the bitmap for the page's cache. */
  function harvestBead(t, wide) {
    var h = t.harvest || {}, bs = harvestBadges(h).slice(0, wide ? 4 : 2), b = { t: "task", s: "done", wild: true, base: h.base || "", wide: !!wide };
    b.badges = bs.map(function (x) { return x.k; });
    b.digits = wide ? bs.map(function (x) { return Math.min(x.n || 0, 9); }) : [];
    b.key = [b.base, b.badges.join(","), b.digits.join(","), wide ? 2 : 1].join("|");
    return b;
  }
  var harvestMemo = {};
  /* The cell at (x, y): 16x16 at 1x; a wide one at twice the resolution (32x32, so the page blits it into 16 logical px), its
     digits drawn at the finer pixel inside their badge: the gates passed on the rosette's disc, the rounds on the seal's. */
  function drawHarvest(ctx, pal, b, x, y) {
    var m = harvestMemo[b.key] || (harvestMemo[b.key] = beadRows(b, 0)), s = b.wide ? 2 : 1;
    putMap(ctx, m, x, y, pal, s);
    if (s < 2) return;
    b.badges.forEach(function (k, i) {
      var n = b.digits[i], at = BADGE_AT[i], dx = x + at[0] * 2 + 4, dy = y + at[1] * 2 + (k === "seal" ? 3 : 1);
      if (!n || (k !== "rosette" && k !== "seal")) return;
      ctx.fillStyle = k === "seal" ? pal.e : pal.Y; ctx.fillRect(dx, dy - 1, 3, 6);   // the disc under the digit, its highlight cleared
      ctx.fillStyle = k === "seal" ? pal.j : pal.k;
      DIGITS[n].forEach(function (r, j) { for (var c = 0; c < 3; c++) if (r.charAt(c) === "k") ctx.fillRect(dx + c, dy + j, 1, 1); });
    });
  }

  /* ---------- agents ---------- */
  function agentPal(isNight, a) { var base = { H: a.hat, h: shade(a.hat, .75), S: a.shirt, s: a.shirt2 || shade(a.shirt, .78) }; return palette(isNight, base); }
  /* face "l" mirrors the 16 px figure and its watering can, which then hangs west of it. */
  function drawAgent(ctx, a, x, y, f, isNight, s, face) {
    s = s || 1; var pal = agentPal(isNight, a), bob = (a.state === "running" && f % 2) ? -1 * s : 0, l = face === "l";
    var put = function (map, yy) { putMap(ctx, l ? flipMap(map) : map, x, yy, pal, s); }, can = function (ch, dx, dy, w, h) { ctx.fillStyle = pal[ch]; ctx.fillRect(l ? x + (16 - dx - w) * s : x + dx * s, dy, w * s, h * s); };
    ctx.fillStyle = isNight ? "#0a1230" : "#2f6b2a"; ctx.fillRect(x + 3 * s, y + 22 * s, 10 * s, 2 * s);
    if (a.state === "crashed") { put(SCARE, y); return; }
    if (a.state === "idle" || a.state === "asleep") { put(a.state === "asleep" ? VILSLEEP : VILSIT, y); return; }
    if (a.state === "wilt") { put(VILWILT, y); return; }
    put(a.state === "running" && f % 2 ? VIL1 : VIL0, y + bob);
    if (a.state === "running") { // watering can
      var cy = y + 12 * s + bob;
      can("R", 13, cy, 5, 3); can("k", 13, cy + 3 * s, 5, 1); can("r", 14, cy, 3, 1); can("R", 18, cy - s, 1, 2);
    }
  }
  var flipped = [];   // [map, its mirror] pairs, one per map
  function flipMap(map) {
    for (var i = 0; i < flipped.length; i++) if (flipped[i][0] === map) return flipped[i][1];
    var m = map.map(function (r) { return r.split("").reverse().join(""); }); flipped.push([map, m]); return m;
  }
  /* A reason's mark on a 16 x 24 figure whose hat's top is at y + 1, its top left at (x, y) (MARKS): zzz rising over the head
     of one asleep, bobbing with f as the bubble does; the sprout growing from the hat of one starting up, wilted on one stale.
     Within the figure's own column, so a neighbour on the plot never hides it. Other poses have none. */
  function drawMark(ctx, pal, pose, x, y, f, sp) {
    var m = pose === "sleep" ? "zzz" : pose === "start" ? "sprout" : pose === "wilt" ? "wilted" : "", map = MARKS[m];
    if (!map) return;
    var mx = m === "zzz" ? x + 8 : x + 4, my = m === "zzz" ? y - 15 + (f % 2 ? -1 : 0) : m === "sprout" ? y - 5 : y - 6;
    if (sp) ctx.drawImage(sp.get("gd|" + m, map, pal, map[0].length, map.length), mx, my);
    else putMap(ctx, map, mx, my, pal, 1);
  }
  function drawBubble(ctx, pal, x, y, f, s, kind) {
    s = s || 1; var yy = y + (f % 2 ? -s : 0);
    ctx.fillStyle = pal.k; ctx.fillRect(x, yy, 14 * s, 11 * s); ctx.fillStyle = pal.j; ctx.fillRect(x + s, yy + s, 12 * s, 9 * s);
    ctx.fillStyle = pal.k; ctx.fillRect(x + 3 * s, yy + 11 * s, 3 * s, 2 * s); ctx.fillStyle = pal.j; ctx.fillRect(x + 4 * s, yy + 10 * s, 2 * s, 2 * s);
    ctx.fillStyle = pal.e;
    if (kind === "!") { ctx.fillRect(x + 6 * s, yy + 2 * s, 2 * s, 4 * s); ctx.fillRect(x + 6 * s, yy + 7 * s, 2 * s, 2 * s); }
    else if (kind === "...") { ctx.fillStyle = pal.k; [3, 6, 9].forEach(function (d) { ctx.fillRect(x + d * s, yy + 5 * s, 2 * s, 2 * s); }); }   // a gardener thinking (4c)
    else { ctx.fillRect(x + 5 * s, yy + 2 * s, 4 * s, s); ctx.fillRect(x + 8 * s, yy + 3 * s, 2 * s, 2 * s); ctx.fillRect(x + 6 * s, yy + 5 * s, 2 * s, s); ctx.fillRect(x + 6 * s, yy + 7 * s, 2 * s, s); ctx.fillRect(x + 4 * s, yy + 3 * s, 2 * s, 2 * s); }
  }


  /* A hedge on the ring of tiles round (x, y)'s wT x hT, its corners rounded. */
  function drawHedge(ctx, pal, x, y, wT, hT, sp) {
    var i, r = x + (wT - 1) * 16, b = y + (hT - 1) * 16;
    for (i = 1; i < wT - 1; i++) { tile(ctx, sp, "hedgeH", hedgeH, x + i * 16, y, pal); tile(ctx, sp, "hedgeH", hedgeH, x + i * 16, b, pal); }
    for (i = 1; i < hT - 1; i++) { tile(ctx, sp, "hedgeV", hedgeV, x, y + i * 16, pal); tile(ctx, sp, "hedgeV", hedgeV, r, y + i * 16, pal); }
    [[x, y], [r, y], [x, b], [r, b]].forEach(function (c, k) { tile(ctx, sp, "hedgeC" + k, hedgeC[k], c[0], c[1], pal); });
  }
  /* Wild grass for the world outside the biomes, picked by absolute tile so chunk seams never show.
     detail=false is a flat approximation for zoomed-out chunks, where individual tiles are under a pixel. */
  /* skip(tx, ty), when given, names tiles something else will paint over (a biome's ground), so their grass is not drawn. */
  /* sp, when given, is a spriteCache: each grass variant is painted once and blitted per tile. */
  function drawWild(ctx, night, tx0, ty0, nx, ny, detail, skip, sp) {
    var pal = palette(night);
    if (!detail) { ctx.fillStyle = pal.g; ctx.fillRect(tx0 * 16, ty0 * 16, nx * 16, ny * 16); return; }
    for (var ty = ty0; ty < ty0 + ny; ty++) for (var tx = tx0; tx < tx0 + nx; tx++) {
      if (skip && skip(tx, ty)) continue;
      var r = hs(tx, ty, 6), v = r > .96 ? 3 : r > .92 ? 4 : Math.floor(hs(tx, ty, 9) * 3);
      tile(ctx, sp, "g" + v, grass[v], tx * 16, ty * 16, pal);
    }
  }

  /* Live bead -> the prototype's bead model. in-progress = sprout; report attached = fruiting; blocked on a decision = weeds + rock. */
  function growingBead(it) {
    var t = FRUIT[it.type] ? it.type : "task";
    var s = it.blocked ? "blocked" : it.report ? "review" : "wip";
    return { t: t, s: s, stale: !!it.stale, who: !it.stale, p: it.prio };
  }
  var rowsMemo = {};
  function beadMemo(b, f) {
    var k = [b.t, b.s, b.stale ? 1 : 0, b.who ? 1 : 0, f % 2].join("|");
    return rowsMemo[k] || (rowsMemo[k] = beadRows(b, f));
  }
  /* A home figure. One the farm document placed at home (it.look, WORLD.gardenerLook) wears its gardener colours and its
     pose, so it looks the same at the barn as on its plot (sitting where a plot gardener stands); one at work doing something
     (it.lookNow, WORLD.gardenerState lookOf; 5b) is drawn as a plot gardener in that doing pose (gd: the look); one from the
     hub's list, before the farm's gardeners are in, wears the look its gardener will (it.wear, garden.js hubWear) with the hub's
     state, so the farm streaming in changes no one's colours (gd-wJVEevAYPP); one with neither, the straw hat and the first shirt. */
  var POSE_STATE = { work: "running", down: "crashed", ask: "waiting", sleep: "asleep", wilt: "wilt", start: "standing", stand: "idle" };
  function villager(it) {
    var st = it.state, lk = it.lookNow ? it.lookNow() || it.look : it.look, w = lk || it.wear || {}, sh = gardenerWear(w.group, w.shirt);
    if (lk && (DOING_PROP[lk.pose] || lk.pose === "think")) return { gd: lk, state: "doing", pose: lk.pose };
    var state = lk ? POSE_STATE[lk.pose] || "idle" : st === "working" ? "running" : (st === "failed" || st === "crashed" || st === "error" || st === "dead") ? "crashed" : (it.blocked ? "waiting" : "idle");
    return { hat: GD_HATS[w.hat] || GD_HATS[8], shirt: sh[0], shirt2: sh[1], state: state, pose: lk ? lk.pose : undefined };
  }

  /* A villager figure for item it at (x, y), solid, its speech bubble over it: "?" asking, "!" crashed. a, when given, is
     villager(it) already; face "l" turns the figure to its west. */
  function drawVillager(ctx, pal, it, x, y, f, isNight, a, face) {
    a = a || villager(it);
    if (a.gd) drawGardener(ctx, a.gd, x, y, f, isNight, null, face); else drawAgent(ctx, a, x, y, f, isNight, 1, face);
    if (a.state === "waiting") drawBubble(ctx, pal, x + 10, y - 12, f, 1, "?");
    if (a.state === "crashed") drawBubble(ctx, pal, x + 10, y - 12, f, 1, "!");
    if (!a.gd) drawMark(ctx, pal, a.pose, x, a.state === "asleep" || a.state === "wilt" ? y + 2 : y, f);   // a doing pose draws its own bubble; sitting and slumped, the hat is 2 px lower
  }
  /* A field stand (28.12) on its pair slot at (x, y): the plant for its bead on the west tile (none for one with no task) and
     its figure on the east tile, facing it. With sp, the pair is one bitmap per figure, plant, frame parity and palette, so it
     repaints only when one of those changes; PAIR is that bitmap's box about (x, y), room for the bubble and the marks above. */
  var PAIR = { x: 2, y: 18, w: 46, h: 46 };
  function drawPair(ctx, pal, it, x, y, f, isNight, sp) {
    var a = villager(it), b = it.plant ? growingBead(it.plant) : null, paint = function (c, px, py) {
      if (b) putMap(c, beadMemo(b, f), px, py, pal, 1);
      drawVillager(c, pal, it, px + 16, py, f, isNight, a, "l");
    };
    if (!sp || !sp.paint) { paint(ctx, x, y); return; }
    var k = "pair|" + JSON.stringify(a) + "|" + (b ? beadKey(b, f) : "") + "|" + (f % 2);
    ctx.drawImage(sp.paint(k, pal, PAIR.w, PAIR.h, function (c) { paint(c, PAIR.x, PAIR.y); }), x - PAIR.x, y - PAIR.y);
  }
  /* ---------- the farmstead (homestead-world-design.md 29.2, 29.7; 28.8, 28.10) ----------
     The home sprites in one biome's farmstead (WORLD's b.home, slot 0), world px, at the biome's palette. model: {part "base" |
     "live" (both when unset), flat, night, frame, seed, letters, asks, release, fill (WORLD.homeFill's entry), sp}. The base is
     still between data updates, so the page bakes it with the terrain: the lane arms and the asks' path (homePaths), the pen's
     trampled bare ground, the Growing field's soil with no fence, the Escalations' post with its flag up while one waits on a human
     (asks, 28.13, 29.4), the barn and mailbox (flag up with letters) on their keeps, and ruts on the bay with a release; flat is
     it at Project, a few fills. The live part is the dog and the letters, escalations (full at its max width, a "+N" crate in
     the cell's last), the Growing field's stands as pairs (drawPair) and its loose beads, its last place its "+N" when full. The
     Barn has no live part: its picture is the base's and its truck TRUCK's. The Pen's fence is GOATS's. */
  function drawHome(ctx, strip, model, pal) {
    var m = model || {}, c = strip.cells, f = m.frame || 0, T = 16, seed = m.seed | 0, fill = m.fill || {}, sp = m.sp, g = c.growing, e = c.escalation;
    var mk = strip.keep.mailbox, mx = mk.x * T + 16, my = mk.y * T + 20, bk = strip.keep.barn, ps = homePaths(strip), pn = c.pen;
    if (m.part !== "live") {
      var onPath = function (tx, ty) { return inRects(ps, tx, ty); }, inFarm = function (tx, ty) { return inRects([strip], tx, ty); };
      if (m.flat) ps.forEach(function (r) { ctx.fillStyle = pal.P; ctx.fillRect(r.x * T, r.y * T, r.w * T, r.h * T); });
      else ps.forEach(function (r) { for (var ty = r.y; ty < r.y + r.h; ty++) for (var tx = r.x; tx < r.x + r.w; tx++) pathTile(ctx, pal, tx, ty, 4 + seed % 7, onPath, inFarm, sp); });
      if (m.flat) { ctx.fillStyle = pal.P; ctx.fillRect((pn.x + 2) * T, (pn.y + 2) * T, (pn.w - 4) * T, (pn.h - 4) * T); }
      else for (var py = pn.y + 1; py < pn.y + pn.h - 1; py++) for (var px = pn.x + 1; px < pn.x + pn.w - 1; px++) {   // trampled bare, grass left in tufts along the fence
        var rim = px === pn.x + 1 || py === pn.y + 1 || px === pn.x + pn.w - 2 || py === pn.y + pn.h - 2, v = Math.floor(hs(px, py, 32) * 3);
        if (!rim || hs(px, py, 31 + seed % 5) < .5) tile(ctx, sp, "pa" + v, pathT[v], px * T, py * T, pal);
      }
      if (m.flat) { ctx.fillStyle = pal.s; ctx.fillRect((g.x + 1) * T, (g.y + 1) * T, (g.w - 2) * T, (g.h - 2) * T); }
      else soilTiles(ctx, pal, (g.x + 1) * T, (g.y + 1) * T, (g.w - 2) * T, (g.h - 2) * T, sp);
      var ex = (e.x - 1) * T, ey = (e.y + e.h - 1) * T;   // the post on the north arm's bottom tile, at the cell's south-west corner (29.4)
      tile(ctx, sp, "escpost", ESC_POST, ex, ey, pal);
      if (m.asks) tile(ctx, sp, "escflag", ESC_FLAG, ex + 3, ey - 4, pal);   // its flag, as the mailbox's is up with letters
      tile(ctx, sp, "barn", BARN, bk.x * T + 8, bk.y * T + 8, pal);
      tile(ctx, sp, m.letters ? "mail1" : "mail0", m.letters ? MAIL_UP : MAIL_DOWN, mx, my, pal);
      if (m.release) [14, 6].forEach(function (dy) { var b = strip.bay, y = (b.y + b.h) * T - dy; ctx.fillStyle = pal.t; ctx.fillRect(b.x * T + 2, y, b.w * T - 4, 2); ctx.fillStyle = pal.u; ctx.fillRect(b.x * T + 2, y + 2, b.w * T - 4, 1); });
    }
    if (m.part === "base") return;
    putMap(ctx, f % 3 === 0 ? DOG1 : DOG0, seed & 1 ? mx - 12 : mx + 14, my + 10, pal, 1);   // the dog sits west of the mailbox or east, by the seed
    if (m.letters) drawBubble(ctx, pal, mx + 14, my - 14, f, 1, "?");
    (fill.mailbox ? fill.mailbox.items : []).forEach(function (h) { putMap(ctx, beadMemo({ t: "decision", s: "open" }, f), h.x * T, h.y * T, pal, 1); });
    var ez = strip.zones && strip.zones.escalation;
    (ez ? ez.items : []).forEach(function (h) { drawEscalation(ctx, pal, h.wait.family, h.wait.state, h.x * T, h.y * T, 1, f); });
    if (ez && ez.crate) drawEscMore(ctx, pal, ez.crate.x * T, ez.crate.y * T, ez.crate.n);
    (fill.growing ? fill.growing.items : []).forEach(function (h) {   // the stands as pairs, the loose beads on their slot's west tile (28.12)
      if (h.kind === "stand") drawPair(ctx, pal, h.item, h.x * T, h.y * T, f, m.night, sp);
      else putMap(ctx, beadMemo(growingBead(h.item), f), h.x * T, h.y * T, pal, 1);
    });
    var gb = fill.growing && fill.growing.badge;
    if (gb) drawBarnMore(ctx, pal, gb.x * T + (gb.w * T - 16) / 2, gb.y * T + 5, gb.n);   // centred in the field's last place
  }
  /* The Escalations cell's "+N" (28.11): a crate in its last slot with the number in pixel digits on a plate above it (99 at
     most; the cell's words say them all), as the barn's "+N" is. */
  function drawEscMore(ctx, pal, x, y, n) {
    putMap(ctx, CRATE_RIM, x - 4, y, pal, 1);   // the sprite's crate is its right half: centred in the slot
    var ds = String(Math.min(n, 99)).split(""), w = 4 + 4 * ds.length, dx = x + 8 - w / 2 | 0, dy = y + 2;
    ctx.fillStyle = pal.k; ctx.fillRect(dx - 1, dy - 1, w + 1, 7);
    ctx.fillStyle = pal.j; ctx.fillRect(dx, dy + 2, 3, 1); ctx.fillRect(dx + 1, dy + 1, 1, 3);   // the plus
    ds.forEach(function (c, k) { DIGITS[c].forEach(function (r, j) { for (var q = 0; q < 3; q++) if (r.charAt(q) === "k") ctx.fillRect(dx + 4 + 4 * k + q, dy + j, 1, 1); }); });
  }
  /* The farmstead's paths (29.2, 29.10), tile rects: the east-west arm on its row, the north arm on its column above it, the south
     arm on its own column below it, and the 1-tile asks' path between the Escalations and the Mailbox. */
  function homePaths(home) {
    var e = home.cells.escalation, x = home.cross, s = x.y + 1;
    return [{ x: home.x, y: x.y, w: home.w, h: 1 }, { x: x.xn, y: home.y, w: 1, h: x.y - home.y }, { x: x.xs, y: s, w: 1, h: home.y + home.h - s }, { x: e.x + e.w, y: e.y, w: 1, h: e.h }];
  }
  function inRects(rs, tx, ty) { for (var k = 0; k < rs.length; k++) { var r = rs[k]; if (tx >= r.x && ty >= r.y && tx < r.x + r.w && ty < r.y + r.h) return true; } return false; }
  /* One path tile at (tx, ty), its variant by hash seed k, with a 1 px kerb (Q) on each side whose neighbour is ground: on(tx, ty)
     says a tile is path, inside(tx, ty) that it is ground this path borders (off it, a lane runs out, so it has no kerb). */
  function pathTile(ctx, pal, tx, ty, k, on, inside, sp) {
    var v = Math.floor(hs(tx, ty, k) * 3), x = tx * 16, y = ty * 16, edge = function (dx, dy) { return !on(tx + dx, ty + dy) && inside(tx + dx, ty + dy); };
    tile(ctx, sp, "pa" + v, pathT[v], x, y, pal);
    ctx.fillStyle = pal.Q;
    if (edge(-1, 0)) ctx.fillRect(x, y, 1, 16);
    if (edge(1, 0)) ctx.fillRect(x + 15, y, 1, 16);
    if (edge(0, -1)) ctx.fillRect(x, y, 16, 1);
    if (edge(0, 1)) ctx.fillRect(x, y + 15, 16, 1);
  }
  /* A biome's lanes (29.3, WORLD's b.lanes) as path tiles over the tile window [tx0, tx0+n) x [ty0, ty0+n): those inside(tx, ty)
     says are its ground, so a lane runs out into the foot's fringe; home is the farmstead, whose cross meets the lanes at its
     gates (no kerb across a gate). */
  function drawLanes(ctx, pal, lanes, home, tx0, ty0, n, inside, sp) {
    var ps = home ? homePaths(home) : [], on = function (tx, ty) { return inRects(lanes, tx, ty) || inRects(ps, tx, ty); };
    lanes.forEach(function (l, i) {
      var done = lanes.slice(0, i);   // a crossing is painted once
      for (var ty = Math.max(l.y, ty0); ty < Math.min(l.y + l.h, ty0 + n); ty++) for (var tx = Math.max(l.x, tx0); tx < Math.min(l.x + l.w, tx0 + n); tx++)
        if (inside(tx, ty) && !inRects(done, tx, ty)) pathTile(ctx, pal, tx, ty, 5, on, inside, sp);
    });
  }
  /* The Growing field's "+N" (28.12; the barn's, 5a, moved): past its places, the stands and loose beads it has no room for,
     as the plots' "+N" badge (drawGardenerMore) with the number in pixel digits on it (99 at most; the field's words say all). */
  function drawBarnMore(ctx, pal, x, y, n) {
    drawGardenerMore(ctx, pal, x, y);
    var ds = String(Math.min(n, 99)).split(""), dx = x + 8 - (3 + 4 * ds.length) / 2 | 0, dy = y + 6;
    ctx.fillStyle = pal.j; ctx.fillRect(dx, dy + 2, 3, 1); ctx.fillRect(dx + 1, dy + 1, 1, 3);   // the plus
    ds.forEach(function (c, k) { DIGITS[c].forEach(function (r, j) { for (var q = 0; q < 3; q++) if (r.charAt(q) === "k") ctx.fillRect(dx + 4 + 4 * k + q, dy + j, 1, 1); }); });
  }

  /* ---------- project biomes (gardenio/docs/homestead-world-design.md section 3-4, 6): terrain, plots, beds, bands ----------
     Everything here is fillRect-only like the rest of the file. Positions are world px; a plot is 16 x 12 tiles with a 14 x 10
     interior of soil beds, one bed row per phase. The eight biome palettes recolour the grass trio (g, h, d) so neighbouring
     projects read as different ground; the index is h32(name, 2) % 8 and the choice is the layout's, not this file's. */
  var BIOME_PALS = [
    { g: "#5fae3e", h: "#7cc957", d: "#458f36", name: "meadow" },
    { g: "#6fb24a", h: "#93cf62", d: "#4f8f3a", name: "spring" },
    { g: "#8aa83c", h: "#a9c457", d: "#6a8630", name: "olive" },
    { g: "#4fa86a", h: "#6ec386", d: "#3a855a", name: "mint" },
    { g: "#9aa64a", h: "#bcc46a", d: "#78833a", name: "straw" },
    { g: "#5a9f8a", h: "#7ebca6", d: "#45806e", name: "teal" },
    { g: "#7fb54e", h: "#a4d36a", d: "#5e9240", name: "lime" },
    { g: "#4c8f45", h: "#68ab5f", d: "#386d35", name: "moss" }
  ];
  function biomePalette(isNight, pi) { var k = ((pi % 8) + 8) % 8, b = BIOME_PALS[k]; return palette(isNight, { g: b.g, h: b.h, d: b.d }, "b" + k); }
  /* Biome ground for the tiles of one chunk: the biome's grass variant wherever inside(tx, ty) says the tile is in the footprint,
     with a darker rim one tile wide on the outside edge so the shape reads. Wild grass elsewhere is already under it. skip(tx,
     ty), when given, names inside tiles something else paints over whole (a plot's paths and beds), so their grass is not drawn. */
  function drawBiomeGround(ctx, isNight, pi, tx0, ty0, nx, ny, inside, sp, skip) {
    var pal = biomePalette(isNight, pi);
    for (var ty = ty0; ty < ty0 + ny; ty++) for (var tx = tx0; tx < tx0 + nx; tx++) {
      if (!inside(tx, ty) || (skip && skip(tx, ty))) continue;
      var r = hs(tx, ty, 6 + pi), v = r > .95 ? 3 : r > .9 ? 4 : Math.floor(hs(tx, ty, 9) * 3);
      tile(ctx, sp, "g" + v, grass[v], tx * 16, ty * 16, pal);
      var e = (!inside(tx - 1, ty) ? 1 : 0) | (!inside(tx + 1, ty) ? 2 : 0) | (!inside(tx, ty - 1) ? 4 : 0) | (!inside(tx, ty + 1) ? 8 : 0);
      if (e) { ctx.fillStyle = pal.d; if (e & 1) ctx.fillRect(tx * 16, ty * 16, 1, 16); if (e & 2) ctx.fillRect(tx * 16 + 15, ty * 16, 1, 16); if (e & 4) ctx.fillRect(tx * 16, ty * 16, 16, 1); if (e & 8) ctx.fillRect(tx * 16, ty * 16 + 15, 16, 1); }
    }
  }
  /* The name sign: a plate on two posts, w px wide (the label text is DOM, sized to the screen, not painted here). */
  function drawSign(ctx, pal, x, y, w) {
    putMap(ctx, plate(w), x, y, pal, 1);
    [x + 6, x + w - 9].forEach(function (px) { box(ctx, pal, "k", px, y + 12, 4, 20); box(ctx, pal, "b", px + 1, y + 12, 1, 19); box(ctx, pal, "B", px + 2, y + 12, 1, 19); });
  }
  /* The fence tiles of a ring (fx, fy, fw, fh in tiles) that fall inside the tile window [tx0, tx0+n) x [ty0, ty0+n): a chunk
     bakes only its own part of a biome's ring. */
  function drawFenceClip(ctx, pal, fx, fy, fw, fh, tx0, ty0, n, sp) {
    var x1 = tx0 + n, y1 = ty0 + n, tx, ty;
    for (tx = Math.max(fx, tx0); tx < Math.min(fx + fw, x1); tx++) {
      if (fy >= ty0 && fy < y1) tile(ctx, sp, "fh", fenceH, tx * 16, fy * 16, pal);
      if (fh > 1 && fy + fh - 1 >= ty0 && fy + fh - 1 < y1) tile(ctx, sp, "fh", fenceH, tx * 16, (fy + fh - 1) * 16, pal);
    }
    for (ty = Math.max(fy + 1, ty0); ty < Math.min(fy + fh - 1, y1); ty++) {
      if (fx >= tx0 && fx < x1) tile(ctx, sp, "fv", fenceV, fx * 16, ty * 16, pal);
      if (fw > 1 && fx + fw - 1 >= tx0 && fx + fw - 1 < x1) tile(ctx, sp, "fv", fenceV, (fx + fw - 1) * 16, ty * 16, pal);
    }
  }
  /* A square plot (cursor-grid spec 5.2, N-D10): PLOT tiles a side, a fence ring, 1-tile paths around and between its 3x3 of beds. */
  var PLOT = 32, BLOCK = 48;   // tiles a side; a bed's 3x3 block in world px
  /* The 3x3 blocks of a bed (world px), each with its tone: dark on alternate blocks. */
  function blocks(b, each) { for (var j = 0; j * BLOCK < b.h; j++) for (var i = 0; i * BLOCK < b.w; i++) each(b.x + i * BLOCK, b.y + j * BLOCK, (i + j) % 2 === 1); }
  /* Plot at native size: the hedge ring (a staked plot's string), path tiles inside it, and each bed's soil in two block tones.
     beds: [{x, y, w, h}] in world px, on the tile grid. hp, when given, is the hedge's palette (the biome's, so its dark green). */
  function drawPlotBase(ctx, pal, x, y, beds, sp, staked, hp) {
    if (staked) drawStringFence(ctx, pal, x, y, PLOT, PLOT, sp); else drawHedge(ctx, hp || pal, x, y, PLOT, PLOT, sp);
    var inBed = function (px, py) { for (var k = 0; k < beds.length; k++) { var b = beds[k]; if (px >= b.x && px < b.x + b.w && py >= b.y && py < b.y + b.h) return true; } return false; };
    for (var ty = 1; ty < PLOT - 1; ty++) for (var tx = 1; tx < PLOT - 1; tx++) {
      var px = x + tx * 16, py = y + ty * 16;
      if (!inBed(px, py)) { var v = Math.floor(hs(px >> 4, py >> 4, 4) * 3); tile(ctx, sp, "pa" + v, pathT[v], px, py, pal); }
    }
    beds.forEach(function (b) { blocks(b, function (bx, by, dark) { (staked ? paleSoil : soilTiles)(ctx, pal, bx, by, BLOCK, BLOCK, sp, dark); }); });
  }
  /* A staked plot (homestead-world-design.md section 7), laid out but not planted: its fence is surveyor's string with a stake
     at each corner and every fourth tile, its beds pale turned soil (drawPlotBase with staked). stringRows is one 16x16 tile:
     h and v the string's spans across and down ([from, to) px) or null, post a stake in the middle. */
  var stringMemo = {}, PALE = { u: "N", s: "P", t: "Q" }, PALE_DARK = { u: "P", s: "Q", t: "s" };
  function stringRows(key, h, v, post) {
    if (stringMemo[key]) return stringMemo[key];
    var g = grid(16, 16);
    if (h) { rect(g, h[0], 8, h[1] - h[0], 1, "J"); rect(g, h[0], 9, h[1] - h[0], 1, "Q"); }
    if (v) { rect(g, 8, v[0], 1, v[1] - v[0], "J"); rect(g, 9, v[0], 1, v[1] - v[0], "Q"); }
    if (post) { rect(g, 6, 3, 4, 11, "k"); rect(g, 7, 4, 1, 9, "c"); rect(g, 8, 4, 1, 9, "b"); rect(g, 6, 14, 4, 1, "D"); }
    return (stringMemo[key] = rows(g));
  }
  function drawStringFence(ctx, pal, x, y, wT, hT, sp) {
    var put = function (tx, ty, h, v, post) { var key = "str|" + h + "|" + v + "|" + post; tile(ctx, sp, key, stringRows(key, h, v, post), x + tx * 16, y + ty * 16, pal); };
    var L = [8, 16], R = [0, 9], F = [0, 16], i;
    for (i = 1; i < wT - 1; i++) { put(i, 0, F, null, i % 4 === 0); put(i, hT - 1, F, null, i % 4 === 0); }
    for (i = 1; i < hT - 1; i++) { put(0, i, null, F, i % 4 === 0); put(wT - 1, i, null, F, i % 4 === 0); }
    put(0, 0, L, L, true); put(wT - 1, 0, R, L, true); put(0, hT - 1, L, R, true); put(wT - 1, hT - 1, R, R, true);
  }
  var PALE_SOIL = [PALE, PALE_DARK].map(function (m) { return SOIL.map(function (t) { return t.map(function (r) { return r.replace(/[ust]/g, function (c) { return m[c]; }); }); }); });
  function paleSoil(ctx, pal, x, y, w, h, sp, dark) {
    if (!sp || ((x | y | w | h) & 15)) return soilRect(ctx, pal, x, y, w, h, 1, dark ? PALE_DARK : PALE);
    for (var ty = y / 16; ty < (y + h) / 16; ty++) for (var tx = x / 16; tx < (x + w) / 16; tx++) { var v = Math.floor(hs(tx, ty, 23) * 4); tile(ctx, sp, (dark ? "pS" : "ps") + v, PALE_SOIL[dark ? 1 : 0][v], tx * 16, ty * 16, pal); }
  }
  function drawStakedBase(ctx, pal, x, y, beds, sp) { drawPlotBase(ctx, pal, x, y, beds, sp, true); }
  /* A staked plot's step at the Bead level: a stake marker (a post with a paper tag) where its plant will grow; the frosted
     variant (an ice-blue tag) is for a draft's steps, unused while drafts are pots. */
  var STEP_STAKE = (function () {
    var g = grid(16, 16);
    rect(g, 6, 7, 4, 8, "k"); rect(g, 7, 7, 1, 7, "c"); rect(g, 8, 7, 1, 7, "b"); rect(g, 5, 15, 6, 1, "D");
    rect(g, 2, 1, 12, 7, "k"); rect(g, 3, 2, 10, 5, "j"); rect(g, 5, 3, 7, 1, "J"); rect(g, 5, 5, 5, 1, "J"); g[4][3] = "k";
    return rows(g);
  })();
  var STEP_FROST = STEP_STAKE.map(function (r) { return r.replace(/[jJcb]/g, function (c) { return { j: "z", J: "x", c: "q", b: "r" }[c]; }); });
  function drawStepStake(ctx, pal, x, y, frosted) { putMap(ctx, frosted ? STEP_FROST : STEP_STAKE, x, y, pal, 1); }
  /* Plot at the Plan level (7-16 px per tile): a flat frame in the crop's colour, the path, each bed's blocks in two flat tones,
     and one flat square per task with a glyph so a state is never colour alone. crop is PLANS.tile().crop; cells: [{x, y, state,
     type}] in world px. */
  var CROP_COL = { post: "Y", blocked: "R", fruit: "o", wilt: "Z", sprout: "m", staked: "J" };   // staked: a staked plot's pale string
  var STATE_COL = { in_progress: "m", review: "o", blocked: "R", open: "c", deferred: "Z" };
  function drawPlotFlat(ctx, pal, x, y, crop, beds, cells) {
    var side = PLOT * 16;
    box(ctx, pal, "k", x, y, side, side); box(ctx, pal, CROP_COL[crop] || "m", x + 2, y + 2, side - 4, side - 4); box(ctx, pal, "P", x + 16, y + 16, side - 32, side - 32);
    beds.forEach(function (b) { blocks(b, function (bx, by, dark) { box(ctx, pal, crop === "staked" ? (dark ? "P" : "N") : dark ? "t" : "s", bx, by, BLOCK, BLOCK); }); });   // a staked plot's beds are pale turned soil
    cells.forEach(function (c) { drawTaskFlat(ctx, pal, c.x, c.y, c.state, c.type); });
  }
  /* A bed's stake (cursor-grid spec 5.3), one 16x16 tile on the path at the bed's top-left corner: a plate with the phase's
     ordinal as a pixel digit, on a post, over a 4-pip progress bar (done / total in quarters). The current phase's plate is lit
     gold; a closed phase's is bleached. So "1, then 2, then 3" and "we are on 4" read with no text. */
  var DIGITS = {
    0: [".k.", "k.k", "k.k", "k.k", ".k."], 1: [".k.", "kk.", ".k.", ".k.", "kkk"], 2: ["kk.", "..k", ".k.", "k..", "kkk"], 3: ["kk.", "..k", ".k.", "..k", "kk."],
    4: ["k.k", "k.k", "kkk", "..k", "..k"], 5: ["kkk", "k..", "kk.", "..k", "kk."], 6: [".kk", "k..", "kk.", "k.k", ".k."],
    7: ["kkk", "..k", ".k.", ".k.", ".k."], 8: ["kkk", "k.k", "kkk", "k.k", "kkk"], 9: [".k.", "k.k", ".kk", "..k", "kk."]
  };
  var stakeMemo = {};
  function stakeRows(n, pips, lit, closed) {
    var key = [n, pips, lit ? 1 : 0, closed ? 1 : 0].join("|");
    if (stakeMemo[key]) return stakeMemo[key];
    var g = grid(16, 16), face = lit ? "G" : closed ? "J" : "c";
    rect(g, 2, 0, 12, 9, "k"); rect(g, 3, 1, 10, 7, face); rect(g, 3, 7, 10, 1, lit ? "Y" : closed ? "r" : "b");
    (DIGITS[n] || []).forEach(function (r, j) { for (var i = 0; i < 3; i++) if (r.charAt(i) === "k") g[2 + j][6 + i] = "k"; });
    rect(g, 7, 9, 2, 3, "k"); g[9][7] = "B";
    rect(g, 1, 12, 14, 4, "k");
    for (var i = 0; i < 4; i++) rect(g, 2 + i * 3 + (i > 1 ? 1 : 0), 13, 2, 2, i < pips ? "n" : "K");
    return (stakeMemo[key] = rows(g));
  }
  function drawStake(ctx, pal, x, y, n, pips, lit, closed, sp) {
    var r = stakeRows(n, pips, lit, closed);
    if (sp) tile(ctx, sp, "stake|" + [n, pips, lit ? 1 : 0, closed ? 1 : 0].join("|"), r, x, y, pal); else putMap(ctx, r, x, y, pal, 1);
  }
  /* The "+N phases" crate on a plot's ninth bed (cursor-grid spec 5.3): a crate stack in the middle of the 9x9 bed. */
  var PILE_RIM = rim(S.pile), CRATE_RIM = rim(S.crate);
  function drawPhaseCrate(ctx, pal, x, y) {
    putMap(ctx, PILE_RIM, x + 24, y + 56, pal, 3); putMap(ctx, CRATE_RIM, x + 40, y + 40, pal, 3);
  }
  /* A plot's marks at the Plan and Bead levels (p is a WORLD plot, tiles): each phase bed's stake on the path at its top-left
     corner, and the "+N phases" crate when it has one. The Other bed has no stake. sp, when given, is the page's spriteCache. */
  function drawPlotMarks(ctx, pal, p, sp) {
    p.beds.forEach(function (bd) { if (!bd.other) drawStake(ctx, pal, (bd.x - 1) * 16, (bd.y - 1) * 16, bd.n, bd.pips, bd.current, bd.closed, sp); });
    if (p.crate) drawPhaseCrate(ctx, pal, p.crate.x * 16, p.crate.y * 16);
  }
  /* A plan's arms at its plot gate (heraldry spec 6.3-6.4, step 4.1): img is HERALDRY.picture()'s renderer PNG, already
     slot x devicePixelRatio device px wide, so it is blitted 1:1 onto the canvas's device pixels with its top-left at world
     point (x, y) snapped to a whole device px: the same picture at every zoom, as sharp as the screen, never scaled (no
     smoothing to blur it, no quantised copy). The context's transform is the world's when it can say so (the page); else
     (node) world px are device px. Returns the box it covers in device px, or null when there is no picture yet. */
  function drawArms(ctx, img, x, y) {
    if (!img || !img.naturalWidth) return null;
    var m = ctx.getTransform ? ctx.getTransform() : null, w = img.naturalWidth, h = img.naturalHeight;
    var dx = m ? Math.round(m.a * x + m.e) : Math.round(x), dy = m ? Math.round(m.d * y + m.f) : Math.round(y);
    if (m) ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.drawImage(img, dx, dy, w, h);
    if (m) ctx.setTransform(m.a, m.b, m.c, m.d, m.e, m.f);
    return { x: dx, y: dy, w: w, h: h };
  }
  function drawTaskFlat(ctx, pal, x, y, state, type, u) {   // u scales the 16 px square (the beds under the rollup stamp); 1 when left out
    u = u || 1;
    var px = function (ch, dx, dy, w, h) { box(ctx, pal, ch, x + dx * u, y + dy * u, w * u, h * u); };
    if (state === "planned") { px("k", 6, 6, 4, 9); px("b", 7, 7, 2, 7); px("k", 2, 1, 12, 6); px("j", 3, 2, 10, 4); return; }   // a staked step: its stake marker, no square
    px("k", 1, 1, 14, 14); px(STATE_COL[state] || "c", 2, 2, 12, 12);
    if (state === "blocked") px("k", 5, 6, 6, 5);                                  // the rock
    else if (state === "review") { px("e", 6, 5, 4, 4); px("L", 7, 3, 2, 2); }     // the fruit
    else if (state === "in_progress") { px("L", 7, 4, 2, 8); px("n", 5, 4, 2, 2); px("n", 9, 4, 2, 2); } // the sprout
    else if (state === "deferred") px("T", 4, 9, 8, 2);                             // mulch
    else px("k", 7, 7, 2, 2);                                                       // a seed
    if (type === "bug") px("e", 11, 3, 2, 2);
    else if (type === "decision") px("j", 3, 3, 3, 3);
  }
  /* The plot rollup at the project depth (cursor-grid spec 6.6, the stamp; the mock is
     gardenio/docs/prototypes/homestead/plot-rollup-options.html, option B). x, y are the plot's top left in world px, side its
     side in screen px and s world px per screen px (1 / cam.z), so every line is whole screen px at any zoom, like the need
     ring. r is WORLD.plotRollup; crop is PLANS.tile().crop. The plate: a k frame, a ring in the crop colour, the P face. On it
     a 3x3 of phase cells at the beds' real positions (frame by state; a closed phase is leaf with a tick, an open one fills
     with leaf to done / total; sprout, rock and fruit marks), from 90 px each bed drawn as the plan depth draws it under a
     2 px progress strip; under 36 px no cells. Under them the bar, every task of the plan by state. drawRollupMarks paints the
     marks that ride the fence. The page paints the parts apart, so the stamp lies over a plot's operator fence (NB.fence, the
     outer 6 px) rather than under it: drawRollupPlate, the fence, drawRollupStamp (baked), the need ring, drawRollupMarks. */
  var ROLLUP_COL = { closed: "l", in_progress: "m", review: "o", blocked: "R", open: "c", deferred: "Z" };
  var TICK = [[0, 2], [1, 3], [2, 4], [3, 3], [4, 2], [5, 1], [6, 0]];
  function drawPlotRollup(ctx, pal, x, y, side, s, r, crop) { drawRollupPlate(ctx, pal, x, y, side, s, crop); drawRollupStamp(ctx, pal, x, y, side, s, r); drawRollupMarks(ctx, pal, x, y, side, s, r); }
  function drawRollupPlate(ctx, pal, x, y, side, s, crop) {
    box(ctx, pal, "k", x, y, side * s, side * s); box(ctx, pal, CROP_COL[crop] || "m", x + s, y + s, (side - 2) * s, (side - 2) * s); box(ctx, pal, "P", x + 2 * s, y + 2 * s, (side - 4) * s, (side - 4) * s);
  }
  function drawRollupStamp(ctx, pal, x, y, side, s, r) {
    var px = function (ch, dx, dy, w, h) { if (w > 0 && h > 0) box(ctx, pal, ch, x + dx * s, y + dy * s, w * s, h * s); };
    var L = Math.round(side);
    if (L >= 36) {
      var gap = L >= 60 ? 2 : 1, c = Math.floor((L - 10 - 2 * gap) / 3), gx = Math.round((L - 3 * c - 2 * gap) / 2), beds = L >= 90, f = c - 2;
      r.cells.forEach(function (cl) {
        var cx = gx + cl.at.c * (c + gap), cy = 4 + cl.at.r * (c + gap);
        if (cl.empty) return;
        if (cl.crate) {   // "+N phases": two stacked crates
          var crate = function (dx, dy, w, h) { px("k", dx, dy, w, h); px("b", dx + 1, dy + 1, w - 2, h - 2); px("B", dx + 1, dy + h - 2, w - 2, 1); };
          var lw = Math.round(c * .7), lh = Math.max(4, Math.round(c * .35)), uw = Math.round(c * .5), uh = Math.max(4, Math.round(c * .3));
          crate(cx + Math.round((c - lw) / 2), cy + c - 1 - lh, lw, lh); crate(cx + Math.round((c - uw) / 2), cy + c - lh - uh, uw, uh);
          return;
        }
        px(cl.frame, cx, cy, c, c); px(cl.staked ? "Q" : "t", cx + 1, cy + 1, f, f);   // a staked plot's bed is pale turned soil
        var h;
        if (beds) {   // the bed as the plan depth draws it, scaled into the face; the fill becomes a 2 px strip
          var q = f / 9, u = q / 16, fx = cx + 1, fy = cy + 1;
          for (var j = 0; j < 3; j++) for (var i = 0; i < 3; i++) px((i + j) % 2 ? (cl.staked ? "P" : "t") : cl.staked ? "N" : "s", fx + i * 3 * q, fy + j * 3 * q, 3 * q, 3 * q);
          for (var k = 0; k < (cl.stubble || 0); k++) { var sa = cellOf(cl.stubbleAt + k); px("Q", fx + (sa.c + .2) * q, fy + (sa.r + .2) * q, q * .6, q * .6); }
          (cl.tasks || []).forEach(function (t) { drawTaskFlat(ctx, pal, x + (fx + t.at.c * q) * s, y + (fy + t.at.r * q) * s, t.task.state, t.task.type, u * s); });
          if (cl.more) { px("k", fx + cl.more.at.c * q, fy + cl.more.at.r * q, q, q); px("b", fx + (cl.more.at.c + .15) * q, fy + (cl.more.at.r + .15) * q, q * .7, q * .7); }
          h = 2; px("l", fx, fy + f - 2, Math.round(f * (cl.closed ? 1 : cl.fill)), 2);
        } else if (cl.closed) { h = f; px("l", cx + 1, cy + 1, f, f); }
        else { h = Math.round(f * cl.fill); px("l", cx + 1, cy + 1 + f - h, f, h); }
        if (cl.closed) {   // the tick, 7x5 (2x on a cell of 48 px or more)
          var tu = c >= 48 ? 2 : 1, ox = cx + Math.round((c - 7 * tu) / 2), oy = cy + Math.round((c - 5 * tu) / 2);
          TICK.forEach(function (p) { px("j", ox + p[0] * tu, oy + p[1] * tu, tu, tu); });
          return;
        }
        var room = f - h;
        if (cl.sprout && room >= 2) { var sh = Math.max(2, Math.floor(room * .6)); px("n", cx + Math.floor(c / 2) - 1, cy + 1 + room - sh, 2, sh); }
        if (cl.rock) px("k", cx + 2, cy + 2, 3, 2);
        if (cl.fruit) px("o", cx + c - 5, cy + 2, 3, 3);
      });
    }
    var bw = L - 8, acc = 0;   // the bar: every task of the plan by state
    px("k", 3, L - 6, bw + 2, 6); px("P", 4, L - 5, bw, 4);
    r.bar.forEach(function (b) { var w = Math.min(bw - acc, Math.round(bw * b.n / r.total)); px(ROLLUP_COL[b.state] || "c", 4 + acc, L - 5, w, 4); acc += w; });
  }
  /* The rollup's plate marks, the same at every depth: "!N" on the top-right fence when a human is wanted (9 at most) and its
     mirror below, the gardeners' badge (gd-vi77cq1oYY): the neediest's head and the count, red when one is down, gold when one
     asks; under a 64 px plot (1x) the head and its colour alone, no smudged digit (gd-Rq3bxDdnuZ). The badge sits inside the
     bottom fence, clear of the "!N" below (gd-hX9BRyBq1G). Painted after the fence and need ring. Arguments as drawPlotRollup;
     r may be WORLD.plotHeads. */
  function drawRollupMarks(ctx, pal, x, y, side, s, r) {
    var px = function (ch, dx, dy, w, h) { box(ctx, pal, ch, x + dx * s, y + dy * s, w * s, h * s); };
    var L = Math.round(side), isNight = String(pal.id || "").charAt(0) === "n";
    if (r.needs > 0) {
      var nx = L - 15;
      px("k", nx, -4, 13, 9); px("G", nx + 1, -3, 11, 7); px("k", nx + 3, -2, 1, 3); px("k", nx + 3, 2, 1, 1);
      (DIGITS[Math.min(r.needs, 9)] || []).forEach(function (row, j) { for (var i = 0; i < 3; i++) if (row.charAt(i) === "k") px("k", nx + 6 + i, -2 + j, 1, 1); });
    }
    if (r.gardeners > 0) {   // 2x from a 64 px plot, so it reads at a glance
      var u = L >= 64 ? 2 : 1, pw = u > 1 ? 13 : 9, gx = L - 2 - pw * u, gy = L - 9 * u, hat = GD_HATS[r.hats[0]] || GD_HATS[8], q = function (ch, dx, dy, w, h) { px(ch, gx + dx * u, gy + dy * u, w * u, h * u); };
      q("k", 0, 0, pw, 9); q(r.tint === "down" ? "f" : r.tint === "ask" ? "G" : "N", 1, 1, pw - 2, 7); q("k", 2, 2, 5, 5);
      ctx.fillStyle = isNight ? night(hat) : hat; ctx.fillRect(x + (gx + 3 * u) * s, y + (gy + 3 * u) * s, 3 * u * s, 2 * u * s); q("a", 3, 5, 3, 1);
      if (u > 1) (DIGITS[Math.min(r.gardeners, 9)] || []).forEach(function (row, j) { for (var i = 0; i < 3; i++) if (row.charAt(i) === "k") q("k", 8 + i, 2 + j, 1, 1); });
    }
  }
  /* An open escalation's sprites (plan gd-PJB3D7glwF step 3, homestead-world-design.md 28): one 16x16 two-tone map per family,
     keyed by the farm's family strings (farm_model.go Esc*, web/esckind.js FAMILIES), each outlined in k and sitting on a plot's
     mound line. The bodies keep clear of the top-right corner (columns 9 up, rows 0-6) that the state dressing takes. */
  var ESC = (function () {
    var draw = function (fn) { var g = grid(16, 16); fn(g); outline(g, "k"); return rows(g); };
    var crate = function (g, x) { rect(g, x, 8, 5, 5, "b"); rect(g, x, 10, 5, 1, "B"); rect(g, x, 12, 5, 1, "B"); rect(g, x + 2, 8, 1, 5, "B"); };
    return {
      merge: draw(function (g) { crate(g, 1); crate(g, 10); rect(g, 1, 5, 14, 1, "b"); rect(g, 1, 6, 14, 1, "B"); }),   // two crates joined by a plank
      deploy: draw(function (g) {   // a wheelbarrow, its wheel at the front
        rect(g, 4, 6, 9, 1, "r"); rect(g, 5, 7, 8, 1, "r"); rect(g, 6, 8, 6, 1, "R"); rect(g, 7, 9, 4, 1, "R");
        rect(g, 1, 5, 3, 1, "R"); rect(g, 6, 10, 1, 2, "R");
        disc(g, 12, 11, 1.5, "R"); g[11][12] = "r";
      }),
      config: draw(function (g) {   // a hoe laid over a plan sheet
        rect(g, 1, 7, 12, 6, "j"); rect(g, 3, 9, 6, 1, "J"); rect(g, 3, 11, 8, 1, "J"); rect(g, 10, 8, 2, 2, "J");
        for (var i = 0; i < 9; i++) g[3 + i][2 + i] = "b";
        rect(g, 1, 1, 3, 2, "R"); g[1][1] = "r";
      }),
      prompt: draw(function (g) {   // a scroll, rolled at both ends
        rect(g, 2, 4, 6, 7, "N"); rect(g, 1, 3, 8, 2, "Q"); rect(g, 1, 11, 8, 2, "Q");
        rect(g, 3, 6, 4, 1, "Q"); rect(g, 3, 8, 3, 1, "Q");
      }),
      beads: draw(function (g) {   // a string of beads, sagging
        [[1, 4], [3, 7], [6, 9], [9, 10], [12, 9]].forEach(function (p) { rect(g, p[0], p[1], 2, 2, "V"); g[p[1]][p[0]] = "v"; });
      }),
      access: draw(function (g) {   // a key: its bow, the shaft, two teeth
        disc(g, 4, 9, 2.6, "Y"); g[9][4] = "."; rect(g, 5, 11, 2, 1, "Z");
        rect(g, 7, 9, 7, 1, "Y"); rect(g, 7, 10, 7, 1, "Z"); rect(g, 11, 11, 1, 2, "Z"); rect(g, 13, 11, 1, 2, "Z");
      }),
      infra: draw(function (g) {   // a water pump: the barrel, its handle, the spout and a drop
        rect(g, 3, 4, 4, 8, "r"); rect(g, 6, 4, 1, 8, "R"); rect(g, 2, 3, 6, 1, "R"); rect(g, 2, 12, 6, 1, "R");
        rect(g, 1, 2, 1, 1, "R"); rect(g, 0, 1, 1, 1, "R"); rect(g, 7, 6, 3, 1, "r"); rect(g, 9, 7, 1, 1, "R"); rect(g, 9, 9, 1, 2, "w");
      }),
      read: draw(function (g) {   // a spyglass, its lens at the wide end
        rect(g, 1, 9, 3, 2, "O"); rect(g, 4, 8, 4, 4, "o"); rect(g, 8, 7, 5, 6, "o"); rect(g, 7, 8, 1, 4, "O"); rect(g, 12, 7, 1, 6, "O");
        rect(g, 13, 8, 1, 4, "x");
      }),
      other: S.chest
    };
  })();
  /* The state dressing drawn over the family (never colour alone): a raised flag while it is pending, a gear turning by frame
     while it is approved and running (frame 0 under reduced motion: the page passes f = 0), a crack down the body once it is
     rejected or failed. The crack splits only the body's own pixels (k, with a gap to its right), so it follows each family's
     shape. */
  var ESC_FLAG = draw16(function (g) { rect(g, 11, 0, 1, 7, "k"); rect(g, 12, 0, 3, 3, "e"); rect(g, 12, 2, 3, 1, "E"); g[0][14] = "E"; });
  /* The Escalations' post (29.4): a stake hugging its tile's east side, so the cross's middle stays clear; ESC_FLAG flies on it. */
  var ESC_POST = draw16(function (g) { rect(g, 12, 4, 1, 10, "c"); rect(g, 13, 4, 1, 10, "b"); rect(g, 11, 15, 5, 1, "D"); });
  var ESC_GEAR = [0, 1].map(function (f) {
    return draw16(function (g) {
      rect(g, 11, 2, 3, 3, "r"); g[3][12] = "k";
      (f ? [[10, 1], [14, 1], [10, 5], [14, 5]] : [[12, 1], [10, 3], [14, 3], [12, 5]]).forEach(function (p) { g[p[1]][p[0]] = "R"; });
    });
  });
  var ESC_CRACK = [[8, 2], [7, 3], [7, 4], [8, 5], [8, 6], [7, 7], [6, 8], [7, 9], [7, 10], [8, 11], [7, 12], [7, 13]];
  function draw16(fn) { var g = grid(16, 16); fn(g); outline(g, "k"); return rows(g); }
  var ESC_STATES = ["pending", "approved", "rejected", "failed"];
  /* The escalation's map for (family, state) at frame f: an unknown family is other's chest, an unknown state undressed. */
  var escMemo = {};
  function escRows(fam, state, f) {
    if (!ESC[fam]) fam = "other";
    var spin = state === "approved" ? (f || 0) % 2 : 0, key = fam + "|" + state + "|" + spin;
    if (escMemo[key]) return escMemo[key];
    var body = ESC[fam];
    if (state === "rejected" || state === "failed") {
      var g = body.map(function (r) { return r.split(""); }), fill = function (c) { return c && c !== "." && c !== "k"; };
      ESC_CRACK.forEach(function (p) { if (fill(g[p[1]][p[0]])) { g[p[1]][p[0]] = "k"; if (fill(g[p[1]][p[0] + 1])) g[p[1]][p[0] + 1] = "."; } });
      body = rows(g);
    }
    var L = [body];
    if (state === "pending") L.push(ESC_FLAG);
    else if (state === "approved") L.push(ESC_GEAR[spin]);
    return (escMemo[key] = merge(L));
  }
  /* The escalation sprite alone, wherever it is placed: n device px per sprite px, rounded to a whole number so it stays crisp,
     its outline the plot's rim. */
  function drawEscalation(ctx, pal, fam, state, x, y, n, f) { putMap(ctx, rim(escRows(fam, state, f)), x, y, pal, Math.max(1, Math.round(n || 1))); }
  /* A plot whose newest merged PR shipped (web/truck.js): a 9 x 9 plate (k outline, N face) on its top fence at the left, the
     need mark's mirror, with the 5 x 5 crate on it; 2x from a 64 px plot like the heads, and low enough that the cursor's ring
     (garden.css .hot.cur, outside the plot) covers only its rim. Arguments as drawRollupMarks. */
  function drawShippedMark(ctx, pal, x, y, side, s) {
    var u = Math.round(side) >= 64 ? 2 : 1, px = function (ch, dx, dy, w, h) { box(ctx, pal, ch, x + dx * u * s, y + dy * u * s, w * u * s, h * u * s); };
    px("k", 2, -1, 9, 9); px("N", 3, 0, 7, 7); px("k", 4, 1, 5, 5); px("b", 5, 2, 3, 3); px("B", 5, 4, 3, 1);
  }
  function cellOf(i) { var b = Math.floor(i / 9), j = i % 9; return { c: (b % 3) * 3 + j % 3, r: Math.floor(b / 3) * 3 + Math.floor(j / 3) }; }   // WORLD.cellAt
  /* A farm task -> the sprite model beadRows draws (the same plants the Growing field uses). */
  function taskBead(t, now) {
    var ty = FRUIT[t.type] ? t.type : t.type === "decision" || t.type === "escalation" ? t.type : "task", s;
    if (t.type === "decision" || t.type === "escalation") s = t.state === "closed" ? "closed" : "open";
    else s = t.state === "in_progress" ? "wip" : t.state === "review" ? "review" : t.state === "blocked" ? "blocked" : t.state === "deferred" ? "deferred" : t.state === "closed" ? "done" : "open";
    var stale = s === "wip" && now && now - (t.updated || 0) > 12 * 3600;
    var b = { t: ty, s: s, stale: !!stale, who: s === "wip" && !stale, p: t.priority };
    if (ty === "escalation") { b.fam = t.family || "other"; b.w = t.wait || ""; }   // its family sprite and state dressing (FarmTask family, wait)
    return b;
  }
  /* The name of a bead model's bitmap at frame f, for a cache of drawn cells (scene.js): the same key is the same pixels. */
  function beadKey(b, f) { return [b.t, b.s, b.stale ? 1 : 0, b.who ? 1 : 0, b.p == null ? "-" : b.p, b.fam || "", b.w || "", f % 2].join("|"); }
  var spriteMemo = {};
  function beadSprite(b, f) {
    var k = beadKey(b, f) + "|" + (b.frozen ? 1 : 0) + "|" + (b.wild ? 1 : 0);
    return spriteMemo[k] || (spriteMemo[k] = beadRows(b, f));
  }
  function drawBead(ctx, pal, b, x, y, f) { putMap(ctx, beadSprite(b, f), x, y, pal, 1); }
  /* Nursery pot, 3 x 3 tiles: a clay pot with a sprout, frosted (drafting) or green (ready to approve). */
  var POT = (function () {
    var g = grid(48, 48);
    rect(g, 10, 24, 28, 4, "k"); rect(g, 11, 25, 26, 2, "c"); rect(g, 13, 28, 22, 14, "b"); rect(g, 13, 28, 4, 14, "c"); rect(g, 31, 28, 4, 14, "B");
    rect(g, 15, 42, 18, 3, "B"); rect(g, 14, 42, 20, 1, "k"); rect(g, 15, 45, 18, 1, "k"); rect(g, 12, 28, 1, 14, "k"); rect(g, 35, 28, 1, 14, "k");
    rect(g, 14, 22, 20, 2, "t"); rect(g, 14, 21, 20, 1, "k");
    rect(g, 23, 10, 2, 11, "L"); rect(g, 19, 12, 4, 3, "m"); rect(g, 25, 9, 4, 3, "m"); rect(g, 20, 11, 2, 1, "n"); rect(g, 26, 8, 2, 1, "n"); rect(g, 22, 7, 4, 3, "l");
    outline(g, "k");
    return rows(g);
  })();
  var FROST_POT = POT.map(function (r, j) { return j < 22 ? r.replace(/[lmnL]/g, function (c) { return c === "L" ? "x" : "z"; }) : r; });
  function drawPot(ctx, pal, x, y, frosted) { putMap(ctx, frosted ? FROST_POT : POT, x, y, pal, 1); }
  /* Wildflowers for the meadow band: four 16 x 16 sprites, picked by hash. */
  var FLOWERS = (function () {
    var cols = [["Y", "j"], ["F", "j"], ["W", "v"], ["o", "p"]], out = [];
    cols.forEach(function (c, v) {
      var g = grid(16, 16), x = 5 + (v % 2) * 3, y = 6 + Math.floor(v / 2) * 2;
      rect(g, x + 1, y + 3, 1, 6, "L"); rect(g, x - 1, y + 6, 2, 1, "l"); rect(g, x + 2, y + 5, 2, 1, "l");
      rect(g, x, y, 3, 3, c[0]); rect(g, x + 1, y - 1, 1, 1, c[0]); rect(g, x - 1, y + 1, 1, 1, c[0]); rect(g, x + 3, y + 1, 1, 1, c[0]); rect(g, x + 1, y + 3, 1, 1, c[0]); rect(g, x + 1, y + 1, 1, 1, c[1]);
      if (v >= 2) { rect(g, x + 6, y + 2, 2, 2, c[0]); rect(g, x + 6, y + 4, 1, 4, "L"); }
      out.push(rows(g));
    });
    return out;
  })();
  function drawFlower(ctx, pal, x, y, v) { putMap(ctx, FLOWERS[((v % 4) + 4) % 4], x, y, pal, 1); }
  /* Orchard tree, one tile: a round canopy on a short trunk, for a harvested plan. */
  var MINITREE = (function () {
    var g = grid(16, 16); rect(g, 7, 11, 2, 4, "B"); disc(g, 8, 6, 5, "l"); disc(g, 6, 5, 3, "m"); rect(g, 5, 3, 1, 1, "n"); rect(g, 10, 8, 2, 1, "L"); rect(g, 7, 9, 3, 1, "L");
    rect(g, 6, 6, 1, 1, "e"); rect(g, 10, 5, 1, 1, "e"); outline(g, "k"); return rows(g);
  })();
  function drawMiniTree(ctx, pal, x, y) { putMap(ctx, MINITREE, x, y, pal, 1); }
  /* Graveyard headstone, one tile, for a closed wish (gd-s9KzRk6boh): a round-topped grey stone with two carved lines on a tuft
     of grass; a wish that was done has a pink flower at its foot, an abandoned or expired one stands plain. */
  var STONES = (function () {
    var out = [];
    [false, true].forEach(function (flower) {
      var g = grid(16, 16); rect(g, 3, 14, 9, 1, "l"); disc(g, 7, 6, 3, "r"); rect(g, 4, 6, 7, 8, "r"); rect(g, 5, 5, 1, 8, "q"); rect(g, 9, 7, 1, 7, "R");
      rect(g, 6, 8, 3, 1, "R"); rect(g, 6, 10, 3, 1, "R");
      if (flower) { rect(g, 13, 10, 1, 4, "L"); rect(g, 12, 12, 1, 1, "l"); rect(g, 12, 8, 3, 1, "F"); rect(g, 13, 7, 1, 3, "F"); rect(g, 13, 8, 1, 1, "Y"); }
      outline(g, "k"); out.push(rows(g));
    });
    return out;
  })();
  function drawHeadstone(ctx, pal, x, y, flower) { putMap(ctx, STONES[flower ? 1 : 0], x, y, pal, 1); }
  /* The "+N more plans" marker: a crate stack on a plate-sized footprint. */
  function drawOverflow(ctx, pal, x, y) {
    putMap(ctx, S.pile, x + 48, y + 64, pal, 2); putMap(ctx, S.crate, x + 112, y + 80, pal, 2); putMap(ctx, S.crate, x + 144, y + 72, pal, 2);
  }
  /* The band dots at the Project and Plan levels: one square per item in the band's colour (trees, flowers and pots are under
     a few screen px there). s is the square's side in world px, centred in the tile. */
  function drawDot(ctx, pal, kind, x, y, s) {
    s = s || 6; var o = (16 - s) / 2;
    box(ctx, pal, "k", x + o, y + o, s, s); box(ctx, pal, kind === "tree" ? "L" : kind === "seed" ? "c" : kind === "stone" ? "q" : "F", x + o + 1, y + o + 1, s - 2, s - 2);
  }

  /* ---------- gardeners on the farm (gardenio/docs/homestead-agents-as-people.md section 5) ----------
     A gardener is the villager in one of four poses: work (a hoe, two frames), stand, ask (stand under a "?" bubble) and down
     (the scarecrow in the gardener's colours); Phase 2's reasons add sleep (eyes shut under zzz), wilt (slumped by a wilted
     sprout) and start (stand by a sprout), web/reason.js. The shirt is the gardener's own, the hat its plan's (WORLD.gardenerLook).
     Each map is padded to 22 x 26 so a 1 px rim fits outside the k outline: cream by day, the emissive gold at night, which is
     what keeps the silhouette at 3:1 or more on every ground (section 5.3). People are drawn on the dynamic layer, never into
     a chunk, and each (pose, frame, group, hat, shirt, scheme) bitmap is painted once into the sprite cache.
     GD_HATS: the hat by the plan's index (WORLD.gardenerLook), 8 the straw hat of one with no plot. GD_WEAR: the eight shirts
     (light, shade) a gardener's own index picks, so one tells from the next; GD_SHIRTS: the two groups dressed by role instead,
     the steward's raspberry and a thread agent's pink (5b), each dE 15+ from every GD_WEAR shirt (TestGardenersUnderNode). */
  var GD_HATS = ["#d94a3a", "#3a86e0", "#f2d44a", "#a672d8", "#fffaf0", "#f28c28", "#ff9ec4", "#78c0f4", "#e4cc94"];
  var GD_WEAR = [["#4c7ae0", "#3758a1"], ["#f28c28", "#ae651d"], ["#a672d8", "#78529c"], ["#ffbe0b", "#b88908"], ["#4fc3c8", "#398c90"], ["#d9d0c0", "#9c968a"], ["#ffadad", "#b87d7d"], ["#8d99ae", "#666e7d"]];   // blue, orange, violet, amber, teal, cream, salmon, slate: dE 26+ from each other, 10+ from every prop and ground
  var GD_SHIRTS = { steward: ["#c4405a", "#86243a"], thread: ["#e0559a", "#9c2e66"] };
  /* The (light, shade) shirt of a look: its group's when the group dresses by role, else its own by index. */
  function gardenerWear(group, shirt) { return GD_SHIRTS[group] || GD_WEAR[shirt] || GD_WEAR[0]; }
  var GD_W = 22, GD_H = 26, GD_X = 3, GD_Y = 1;   // the padded map, and where the 16 x 24 figure sits in it
  /* Each doing pose's prop colour (4f), which TestGardenersUnderNode holds apart from every shirt and every ground by day and by
     night: the magnifier's glass, the can, the pot, the crate, the page, the pennant, the spyglass, the glove, the hoe. */
  var DOING_PROP = { read: "x", test: "q", build: "K", git: "c", beads: "j", deploy: "e", web: "Y", delegate: "c", waiting: "b" };
  var GD = (function () {
    var body = function (src, remap) {
      var g = grid(GD_W, GD_H);
      src.forEach(function (r, j) { for (var i = 0; i < r.length; i++) { var c = r.charAt(i); if (c !== ".") g[j + GD_Y][i + GD_X] = remap && remap[c] || c; } });
      return g;
    };
    var finish = function (g) { outline(g, "k"); outline(g, "@"); return rows(g); };
    /* the hoe: a handle down the right side; its blade on the ground by the boots (frame 0) or raised over the head (frame 1) */
    var hoe = function (up) {
      var g = body(VIL0), x = GD_X + 16;
      if (up) { rect(g, x, 2, 1, 15, "b"); rect(g, x - 3, 2, 3, 2, "r"); g[3][x - 3] = "R"; }
      else { rect(g, x, 8, 1, 15, "b"); rect(g, x - 3, 23, 4, 1, "R"); g[22][x] = "r"; }
      return finish(g);
    };
    /* section 7's transition poses: arms up for the flight back to the barn; walking home with a scroll (two frames) */
    var fly = function () {
      var g = body(VIL0.map(function (r, j) { return j === 12 || j === 13 ? "..kSSSSssSSSSk.." : r; }));
      [GD_X + 1, GD_X + 14].forEach(function (x) { rect(g, x, GD_Y + 6, 1, 6, "S"); rect(g, x, GD_Y + 3, 1, 3, "a"); });
      return finish(g);
    };
    var scroll = function (src, dy) { var g = body(src); rect(g, GD_X + 13, GD_Y + 10 + dy, 3, 5, "j"); rect(g, GD_X + 13, GD_Y + 10 + dy, 3, 1, "J"); rect(g, GD_X + 13, GD_Y + 14 + dy, 3, 1, "J"); return finish(g); };
    /* Phase 4's doing poses (4c, gd-kxCviRUVA2; web/activity.js ACTIVITY.pose), two frames each (f % 2). 4f (gd-1EMXaKkRPI) made
       them tell apart at Bead zoom, one CSS px a pixel: each has its own whole-body posture (a kneel, a stride, a lean, one arm
       or both raised) and a big prop that reaches into the padding, out to where its rim still fits (columns 2 to 19, rows 2 to
       23 of the padded map), never above the hat, where the still bird perches. edit and a shell command keep the hoe, and
       think is the stand under a "..." bubble (drawGardener). Coordinates below are the padded map's. */
    var row = function (src, j, r) { return src.map(function (s, i) { return i === j ? r : s; }); };
    var arms = function (src, l, r) { var a = (l ? ".ka" : "..k") + "SSSSssSSSS" + (r ? "ak." : "k.."); return row(row(src, 12, a), 13, a); };   // a hand on each hip, or not
    var paste = function (g, src, x, y, from, to) { for (var j = from || 0; j < (to || src.length); j++) for (var i = 0; i < src[j].length; i++) if (src[j].charAt(i) !== ".") g[j + y][i + x] = src[j].charAt(i); return g; };
    var line = function (g, pts, c) { pts.forEach(function (p) { g[p[1]][p[0]] = c; }); return g; };
    var blank = function (n) { var r = []; while (n--) r.push("................"); return r; };
    /* knelt low over the bed, eyes down: the hat 5 rows lower than standing */
    var KNEEL = blank(6).concat(VIL0.slice(1, 6), [VIL0[7], VIL0[6], VIL0[8], VIL0[9], VIL0[10], VIL0[12], "..kkSSSSSSSSkk..", VIL1[17], VIL1[19], VIL1[20], VIL1[21], VIL1[22], VIL0[23]]);
    var doing = {
      /* read: knelt low, a big magnifier held over the bed, brass rim, its glass glinting */
      read: function (fr) {
        var g = paste(grid(GD_W, GD_H), KNEEL, 2, 1), y = 16 + fr;
        rect(g, 15, y, 4, 6, "Y"); rect(g, 14, y + 1, 6, 4, "Y"); rect(g, 15, y + 1, 4, 4, "x"); g[y + 1][15] = g[y + 2][15] = "z"; g[y + 1][16] = fr ? "x" : "j";
        return line(g, [[13, y + 5], [12, y + 6]], "b");
      },
      /* test: a silver watering can in the right hand, its spout out past the hip, pouring on the bed; the drops fall by turns */
      test: function (fr) {
        var g = body(VIL0);
        rect(g, 12, 15, 6, 5, "k"); rect(g, 13, 16, 4, 3, "q"); rect(g, 13, 18, 4, 1, "r"); line(g, [[13, 14], [14, 13], [15, 13], [16, 14]], "k");
        line(g, [[18, 16], [18, 17], [19, 17], [19, 18]], "r"); line(g, fr ? [[18, 21], [19, 23]] : [[19, 20], [18, 22]], "x");
        return g;
      },
      /* build: knelt behind a big iron pot on the fire, the long spoon stirring */
      build: function (fr) {
        var g = body(blank(2).concat(VIL0.slice(0, 20)));
        line(g, fr ? [[17, 7], [16, 8], [16, 9], [15, 10], [15, 11], [14, 12], [14, 13], [13, 14]] : [[13, 7], [13, 8], [13, 9], [14, 10], [14, 11], [14, 12], [14, 13], [14, 14]], "b");
        g[12][14] = "a";
        rect(g, 3, 15, 16, 1, "R"); rect(g, 4, 16, 14, 5, "K"); rect(g, 5, 21, 12, 1, "K"); rect(g, 5, 17, 3, 2, "R");
        [6, 9, 12, 15].forEach(function (x, i) { g[22][x] = (i + fr) % 2 ? "Y" : "o"; g[23][x] = "e"; g[22][x + 1] = "o"; g[23][x + 1] = (i + fr) % 2 ? "o" : "e"; });
        return g;
      },
      /* git: striding with a wide crate in both arms, bobbing */
      git: function (fr) {
        var g = body(arms(VIL0, false, false).slice(0, 17).concat(VIL1.slice(17))), y = 11 + fr;
        rect(g, 3, y, 16, 6, "B"); rect(g, 4, y + 1, 14, 4, "c"); rect(g, 4, y + 3, 14, 1, "b"); g[y + 1][5] = g[y + 1][16] = g[y + 4][5] = g[y + 4][16] = "B";
        g[y + 2][2] = g[y + 2][19] = "a";
        return g;
      },
      /* beads: a tall white clipboard held up on the left, eyes on it, the pencil writing down the page */
      beads: function (fr) {
        var g = body(row(arms(VIL0, false, false), 6, "..kakaaaakaaak.."));
        rect(g, 2, 9, 7, 11, "B"); rect(g, 3, 10, 5, 9, "j"); rect(g, 4, 9, 3, 1, "q");
        [11, 13, 15].forEach(function (j) { if (j < 15 || fr) rect(g, 4, j, 3, 1, "R"); });
        rect(g, 9, 14, 6, 1, "s"); g[14][9] = "a"; line(g, [[8, 13 + fr], [7, 12 + fr]], "Y"); g[14 + fr][6] = "k";
        return g;
      },
      /* deploy: the left arm up on a tall pole, its red pennant flying over the hat's corner */
      deploy: function (fr) {
        var g = body(arms(VIL0, false, true));
        rect(g, 2, 2, 1, 22, "b"); rect(g, 4, 10, 1, 3, "S"); g[9][3] = g[10][3] = "a";
        if (fr) { rect(g, 3, 2, 4, 1, "e"); rect(g, 3, 3, 5, 1, "e"); rect(g, 3, 4, 3, 1, "e"); g[5][3] = "e"; g[3][7] = "E"; }
        else { rect(g, 3, 2, 5, 1, "e"); rect(g, 3, 3, 4, 1, "e"); rect(g, 3, 4, 3, 1, "e"); g[5][3] = g[5][4] = "e"; g[2][7] = "E"; }
        return g;
      },
      /* web: a brass spyglass to the eye, out past the right edge, sweeping up and back */
      web: function (fr) {
        var g = body(arms(VIL0, true, false));
        rect(g, 17, 9, 1, 4, "S"); g[9][16] = "a";
        rect(g, 12, 7, 5, 2, "Y"); rect(g, 17, 6 - fr, 3, 3, "Y"); g[7][17] = g[8][17] = "Z"; if (fr) g[8][18] = g[8][19] = "Y";
        return g;
      },
      /* delegate: both arms flung up in a V, calling the hawk that circles the plot (4b), the leather glove waving */
      delegate: function (fr) {
        var g = body(arms(VIL0, false, false));
        line(g, [[4, 11], [4, 10], [3, 9], [3, 8]], "S"); line(g, [[2, 6], [2, 7]], "a");
        line(g, [[17, 11], [17, 10], [18, 9], [18, 8]], "S");
        rect(g, 18, 4 - fr, 2, 3, "c"); g[7 - fr][19] = "B";
        return g;
      },
      /* waiting: leant hard on the hoe planted on the left, the body tilted over it, head up at the bird */
      waiting: function (fr) {
        var src = row(row(VIL0, 5, fr ? "..kaaakaaaakak.." : "..kaakaaaakaak.."), 6, "..kaaaaaaaaaak..");
        var g = paste(paste(paste(paste(grid(GD_W, GD_H), src, 0, 1, 0, 10), arms(src, false, true), 1, 1, 10, 15), src, 2, 1, 15, 17), src, 3, 1, 17, 24);
        rect(g, 2, 11, 1, 12, "b"); rect(g, 2, 23, 4, 1, "R"); g[22][3] = "R"; rect(g, 2, 10, 2, 1, "a"); line(g, [[3, 11], [3, 12]], "S");
        return g;
      }
    };
    var maps = { work0: hoe(false), work1: hoe(true), stand: finish(body(VIL0)), down: finish(body(SCARE, { Y: "H", Z: "h", v: "S", V: "s" })),
      sleep: finish(body(shut(VIL0, 6))), wilt: finish(body(VILWILT)),
      fly: fly(), walk0: scroll(VIL0, 0), walk1: scroll(VIL1, 1) };
    Object.keys(doing).forEach(function (k) { maps[k + "0"] = finish(doing[k](0)); maps[k + "1"] = finish(doing[k](1)); });
    return maps;
  })();
  /* The steward (Lands 9.8, homestead-agents-as-people.md section 9.2): its own sprite, the villager in the steward's raspberry
     waistcoat with a shepherd's crook at its left side, minding its land by the homestead sign. At work it reads its ledger (the
     scroll, two frames) instead of hoeing; down it is the scarecrow, crook and all. Same 22 x 26 padded map and rim as GD. */
  var GDS = (function () {
    var body = function (src, remap) {
      var g = grid(GD_W, GD_H), x = GD_X - 1;
      src.forEach(function (r, j) { for (var i = 0; i < r.length; i++) { var c = r.charAt(i); if (c !== ".") g[j + GD_Y][i + GD_X] = remap && remap[c] || c; } });
      rect(g, x, GD_Y + 3, 1, 20, "b"); g[GD_Y + 2][x + 1] = g[GD_Y + 2][x + 2] = g[GD_Y + 3][x + 3] = "b";   // the staff, its hook over the hat's brim
      return g;
    };
    var finish = function (g) { outline(g, "k"); outline(g, "@"); return rows(g); };
    var ledger = function (src, dy) { var g = body(src); rect(g, GD_X + 4, GD_Y + 11 + dy, 8, 4, "j"); rect(g, GD_X + 4, GD_Y + 11 + dy, 8, 1, "J"); rect(g, GD_X + 7, GD_Y + 12 + dy, 2, 3, "J"); return finish(g); };
    return { work0: ledger(VIL0, 0), work1: ledger(VIL0, 1), stand: finish(body(VIL0)), down: finish(body(SCARE, { Y: "H", Z: "h", v: "S", V: "s" })),
      sleep: finish(body(shut(VIL0, 6))), wilt: finish(body(VILWILT)) };
  })();
  /* The truck a gardener rides to its plot (section 7), 24 x 16 with its outline and rim: a pickup in the gardener's shirt
     colour, its hat in the cab window. Facing right (r) and mirrored (l). */
  var TRUCK = (function () {
    var g = grid(24, 16);
    rect(g, 2, 7, 12, 4, "S"); rect(g, 2, 10, 12, 1, "s"); rect(g, 13, 3, 7, 8, "S"); rect(g, 19, 6, 3, 5, "S"); rect(g, 13, 10, 9, 1, "s");
    rect(g, 15, 4, 4, 3, "x"); rect(g, 15, 4, 3, 1, "H"); rect(g, 15, 5, 3, 1, "a"); g[7][21] = "Y"; g[10][21] = "r";
    [5, 17].forEach(function (x) { rect(g, x, 11, 3, 3, "k"); g[12][x + 1] = "r"; });
    outline(g, "k"); outline(g, "@");
    var r = rows(g);
    return { r: r, l: r.map(function (row) { return row.split("").reverse().join(""); }) };
  })();
  var gdPals = {};
  function gardenerPal(isNight, group, hat, shirt) {
    var h = GD_HATS[hat] || GD_HATS[8], sh = gardenerWear(group, shirt), k = (isNight ? "n" : "d") + group + hat + "|" + sh[0];
    if (gdPals[k]) return gdPals[k];
    var p = palette(isNight, { H: h, h: shade(h, .75), S: sh[0], s: sh[1] }, "gd" + group + hat + sh[0]);
    p["@"] = isNight ? DAY.G : DAY.j;   // the rim: not dimmed at night
    return (gdPals[k] = p);
  }
  /* A figure facing left: its padded map mirrored (GD_X columns either side, so it stands in place), once per set and name. */
  var gdLeft = {};
  function leftMap(set, name) { var k = (set === GDS ? "s|" : "d|") + name; return gdLeft[k] || (gdLeft[k] = set[name].map(function (r) { return r.split("").reverse().join(""); })); }
  /* One gardener with its 16 x 24 figure's top left at (x, y). look = {pose, group, shirt, hat} (WORLD.gardenerLook). f is the
     animation frame: the hoe swings and the bubble bobs only when the page lets it (Bead level, motion allowed). sp, when
     given, is the page's spriteCache. face "l" (WORLD.gardenerSpots) mirrors the figure, not its bubble or marks. */
  function drawGardener(ctx, look, x, y, f, isNight, sp, face) {
    var pal = gardenerPal(isNight, look.group, look.hat, look.shirt), set = look.group === "steward" ? GDS : GD;
    var name = set[look.pose + "0"] ? look.pose + (f % 2) : set[look.pose] ? look.pose : "stand", l = face === "l", map = l ? leftMap(set, name) : set[name];   // a two-frame pose (the hoe, the doing poses) by f
    if (sp) ctx.drawImage(sp.get((set === GDS ? "gs|" : "gd|") + name + (l ? "|l" : "") + "|" + look.group + "|" + look.hat + "|" + look.shirt, map, pal, GD_W, GD_H), x - GD_X, y - GD_Y);
    else putMap(ctx, map, x - GD_X, y - GD_Y, pal, 1);
    if (look.pose === "ask") drawBubble(ctx, pal, x + 9, y - 12, f, 1, "?");   // REASON.BUBBLE: its button sits there
    else if (look.pose === "think") drawBubble(ctx, pal, x + 9, y - 12, f, 1, "...");
    else drawMark(ctx, pal, look.pose, x, y, f, sp);
  }
  /* Section 7's figures. A truck with its 24 x 16 box's top left at (x, y), facing dir ("r" or "l"); a gardener flying (arms
     up) or walking home with a scroll (f: 0 or 1) with the 16 x 24 figure's top left at (x, y), as drawGardener. */
  function drawTruck(ctx, look, x, y, dir, isNight, sp) {
    var pal = gardenerPal(isNight, look.group, look.hat, look.shirt), d = dir === "l" ? "l" : "r";
    if (sp) ctx.drawImage(sp.get("gd|truck" + d + "|" + look.group + "|" + look.hat + "|" + look.shirt, TRUCK[d], pal, 24, 16), x, y);
    else putMap(ctx, TRUCK[d], x, y, pal, 1);
  }
  /* The release truck (web/truck.js): the same pickup in a red of its own, an empty cab, s world px per sprite px; and a 5 x 5
     crate (the shipped mark's) with its top left at (x, y), k (1) world px per px. */
  function drawReleaseTruck(ctx, x, y, dir, isNight, s) {
    var pal = palette(isNight, { H: "#2a1a14", h: "#2a1a14", S: "#d94a3a", s: "#9a2c2c" }, "reltruck");
    pal["@"] = isNight ? DAY.G : DAY.j;
    putMap(ctx, TRUCK[dir === "l" ? "l" : "r"], x, y, pal, s || 1);
  }
  function drawCrate(ctx, pal, x, y, k) { k = k || 1; ["k0055", "b1133", "c1131", "B1331"].forEach(function (r) { box(ctx, pal, r[0], x + r[1] * k, y + r[2] * k, r[3] * k, r[4] * k); }); }   // its lid on top
  function drawMover(ctx, look, name, x, y, isNight, sp) {
    var pal = gardenerPal(isNight, look.group, look.hat, look.shirt);
    if (sp) ctx.drawImage(sp.get("gd|" + name + "|" + look.group + "|" + look.hat + "|" + look.shirt, GD[name], pal, GD_W, GD_H), x - GD_X, y - GD_Y);
    else putMap(ctx, GD[name], x - GD_X, y - GD_Y, pal, 1);
  }
  function drawFlyer(ctx, look, x, y, isNight, sp) { drawMover(ctx, look, "fly", x, y, isNight, sp); }
  function drawWalker(ctx, look, x, y, f, isNight, sp) { drawMover(ctx, look, f % 2 ? "walk1" : "walk0", x, y, isNight, sp); }
  /* A burst particle: 2 x 2 in the gardener's hat (H) or shirt (S) colour inside a 1 px outline, centred on (x, y). */
  function drawParticle(ctx, look, x, y, ch, isNight) {
    var pal = gardenerPal(isNight, look.group, look.hat, look.shirt);
    box(ctx, pal, "k", Math.round(x) - 2, Math.round(y) - 2, 4, 4); box(ctx, pal, ch === "H" ? "H" : "S", Math.round(x) - 1, Math.round(y) - 1, 2, 2);
  }
  /* A puff of smoke centred on (x, y): five cream clouds that spread as k goes from 0 to 1. */
  function drawPuff(ctx, x, y, k, isNight) {
    var pal = palette(isNight), r = 2 + Math.round(6 * k), s = k < .7 ? 5 : 3;
    [[0, 0], [-r, 0], [r, 0], [0, -r], [0, r]].forEach(function (o) {
      var cx = Math.round(x + o[0] - s / 2), cy = Math.round(y + o[1] - s / 2);
      box(ctx, pal, "J", cx, cy, s, s); box(ctx, pal, "j", cx + 1, cy + 1, s - 2, s - 2);
    });
  }
  /* The "+N" badge beside a plot's gardeners: a 16 x 16 plate; its number is a DOM label. */
  function drawGardenerMore(ctx, pal, x, y) {
    box(ctx, pal, "k", x, y + 1, 16, 14); box(ctx, pal, "K", x + 1, y + 2, 14, 12); box(ctx, pal, "c", x + 1, y + 2, 14, 1);
  }
  /* The gardeners on the visible plots at level 2 (Plan) or 3 (Bead) (design section 5.4), each on its empty tile facing what it
     tends, with each plot's "+N" badge. st is WORLD.gardenerState; f is the animation frame, 0 to hold the figures still. skip(id), when given, is true for
     a gardener a transition draws in its place (web/transitions.js); badge(id, x, y, lift) its arms, bubble down, lifted at every
     other gardener of a run side by side in one row, so neighbours' badges do not overlap. */
  function drawPlotGardeners(ctx, pal, st, level, plots, f, isNight, sp, skip, badge) {
    var gl = st[level], look = st.lookOf || function (id) { return st.look[id]; };   // lookOf: the look with its doing pose (WORLD.gardenerState)
    plots.forEach(function (p) {
      var pl = gl.plots[p.id], at = {}; if (!pl) return;
      pl.shown.forEach(function (id) { var s = gl.spots[id]; at[s.row + ":" + s.col] = 1; });
      pl.shown.forEach(function (id) {
        var s = gl.spots[id], k = 0; if (skip && skip(id)) return;
        var lk = look(id); drawGardener(ctx, lk, s.x, s.y, f, isNight, sp, s.face);
        while (at[s.row + ":" + (s.col - k - 1)]) k++;
        if (badge && lk.pose !== "ask" && lk.pose !== "think") badge(id, s.x, s.y, k % 2 === 1);
      });
      if (pl.more) drawGardenerMore(ctx, pal, pl.moreX, pl.moreY);
    });
  }

  /* The stewards at their lands' homesteads (Lands 9.8) at level 2 (Plan) or 3 (Bead): list is WORLD.stewardState's, each
     {x, y, look}, culled by the page; the escalation executors' stands (WORLD.escStands) the same, with a face. f and
     skip as drawPlotGardeners. */
  function drawStewards(ctx, list, f, isNight, sp, skip) {
    list.forEach(function (s) { if (!(skip && skip(s.id))) drawGardener(ctx, s.look, s.x, s.y, f, isNight, sp, s.face); });
  }
  /* One legend glyph onto a small (24x28) canvas. The gardener rows draw the real sprite (crew shirt, meadow hat). */
  function drawLegend(ctx, kind, night) {
    var pal = palette(night);
    var bead = { sprout: { t: "task", s: "wip", who: true }, fruit: { t: "feature", s: "review" }, wilt: { t: "task", s: "wip", stale: true },
      blocked: { t: "task", s: "blocked" }, bug: { t: "bug", s: "wip", who: true }, post: { t: "decision", s: "open" } }[kind];
    if (bead) { putMap(ctx, beadRows(bead, 0), 4, 6, pal, 1); return; }
    if (kind === "truck") { drawTruck(ctx, { group: "crew", hat: 0 }, 0, 8, "r", night); return; }
    if (kind === "release") { drawReleaseTruck(ctx, 0, 8, "r", night, 1); drawCrate(ctx, pal, 3, 10); drawCrate(ctx, pal, 8, 10); return; }   // the release truck, loaded
    if (kind === "steward") { drawGardener(ctx, { pose: "stand", group: "steward", hat: 0 }, 5, 2, 0, night); return; }
    if (kind === "thread") { drawGardener(ctx, { pose: "stand", group: "thread", hat: 0 }, 4, 2, 0, night); return; }   // a thread agent's shirt (5b)
    if (kind.indexOf("do-") === 0) {   // a doing pose (4c): "do-read" and so on; think under its bubble, lower as ask is
      var dp = kind.slice(3);
      if (dp === "think") { drawGardener(ctx, { pose: "stand", group: "crew", hat: 0 }, 1, 4, 0, night); drawBubble(ctx, pal, 10, 0, 0, 1, "..."); }
      else drawGardener(ctx, { pose: dp, group: "crew", hat: 0 }, 4, 2, 0, night);
      return;
    }
    var pose = { working: "work", idle: "stand", crashed: "down", ask: "ask", asleep: "sleep", gwilt: "wilt", starting: "start" }[kind];
    if (pose === "ask") { drawGardener(ctx, { pose: "stand", group: "crew", hat: 0 }, 1, 4, 0, night); drawBubble(ctx, pal, 10, 0, 0, 1, "?"); }
    else if (pose === "sleep") { putMap(ctx, GD.sleep, 2 - GD_X, 13 - GD_Y, gardenerPal(night, "crew", 0), 1); putMap(ctx, MARKS.zzz, 7, -1, gardenerPal(night, "crew", 0), 1); }   // lower, so the zzz rises over the hat inside the glyph's 24 x 28
    else if (pose) drawGardener(ctx, { pose: pose, group: "crew", hat: 0 }, 4, pose === "start" || pose === "wilt" ? 5 : 2, 0, night);   // room over the hat for its sprout
  }

  return {
    palette: palette, spriteCache: spriteCache, drawWild: drawWild, drawHome: drawHome, PAIR: PAIR,
    drawLegend: drawLegend,
    BIOME_PALS: BIOME_PALS, biomePalette: biomePalette, drawBiomeGround: drawBiomeGround, drawSign: drawSign, drawPlotBase: drawPlotBase, drawHedge: drawHedge, drawStakedBase: drawStakedBase, drawStringFence: drawStringFence, drawStepStake: drawStepStake, drawPlotFlat: drawPlotFlat,
    PLOT: PLOT, drawPlotRollup: drawPlotRollup, drawRollupPlate: drawRollupPlate, drawRollupStamp: drawRollupStamp, drawRollupMarks: drawRollupMarks, stakeRows: stakeRows, drawStake: drawStake, drawPhaseCrate: drawPhaseCrate, drawPlotMarks: drawPlotMarks, drawArms: drawArms,
    drawShippedMark: drawShippedMark, DIGITS: DIGITS,
    drawTaskFlat: drawTaskFlat, taskBead: taskBead, beadRows: beadRows, beadKey: beadKey, drawBead: drawBead, ESC_STATES: ESC_STATES, escRows: escRows, drawEscalation: drawEscalation, harvestBadges: harvestBadges, harvestWords: harvestWords, harvestWord: function (h) { return HARVEST_WORD[(h || {}).base] || "closed"; }, badgeIcon: badgeIcon, harvestBead: harvestBead, drawHarvest: drawHarvest, BADGE_ORDER: BADGE_ORDER, drawPot: drawPot, drawFlower: drawFlower, drawMiniTree: drawMiniTree, drawHeadstone: drawHeadstone, drawOverflow: drawOverflow, drawDot: drawDot, STATE_COL: STATE_COL, CROP_COL: CROP_COL,
    drawFenceClip: drawFenceClip, drawLanes: drawLanes, homePaths: homePaths, drawBubble: drawBubble,
    GD_HATS: GD_HATS, GD_WEAR: GD_WEAR, GD_SHIRTS: GD_SHIRTS, gardenerWear: gardenerWear, GD_MAPS: GD, DOING_PROP: DOING_PROP, GDS_MAPS: GDS, gardenerPal: gardenerPal, drawGardener: drawGardener, drawGardenerMore: drawGardenerMore, drawPlotGardeners: drawPlotGardeners, drawStewards: drawStewards,
    TRUCK: TRUCK, drawTruck: drawTruck, drawReleaseTruck: drawReleaseTruck, drawCrate: drawCrate, drawFlyer: drawFlyer, drawWalker: drawWalker, drawParticle: drawParticle, drawPuff: drawPuff,
    BARN_DOOR: [48, 64],   // the middle of the barn's double door at ground level, px off the BARN sprite's top left: where trucks leave and gardeners arrive
    SP:{ MAIL_UP: MAIL_UP, VIL0: VIL0, VILSIT: VILSIT, SCARE: SCARE, DOG0: DOG0, BARN: BARN, VILWILT: VILWILT, MARKS: MARKS }
  };
})();
if (typeof module !== "undefined") module.exports = ART;
