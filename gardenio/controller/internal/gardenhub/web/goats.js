/* GOATS: wishes as goats (gd-ODxztYntyd, gd-kqTYEsMvhv and gd-2cPnm71cxT, steps 2.1-2.3 of plan gd-EGpbDpqsec, in the pattern of
   web/birds.js). Every open wish (a thread head, the farm document's wishes.open, farm_wishes.go) is a goat that grazes along
   the path round the plots of its kin: the plot of its newest kin on the map and every other plot of its kin in that project,
   so a wish with kin in several plans laps them all (the ring round the box of its plots); one with none on the map stands in
   the pen (below). The ring is its tour: a goat grazes a spell at a spot, walks on round the ring to
   the next, a slow lap; now and then it turns up a gutter between a kin plot's beds, where its body fits the gutter (the
   Bead level: one across y from 1.5x, across x from 2x). The goats of one tour are spread evenly round it and move on together, so they keep their distance.
   A goat never stands on a bed (the ring is the path between plots, a gutter is clear of beds), never grazes on a lane (29.3:
   a side of the ring that would run down a lane runs on its verge, and a spot where the ring crosses one moves on), and never grazes under
   a gardener standing on a plot or on the ground its birds fly out over (up off its head and away, BIRDS.route; its hawk
   circles over the head): such a spot moves on round the ring until it is clear. Honest limit: a walk may
   cross that ground on its way. A goat grazes head down, chewing, now and then looks up,
   and walks in STEP ms two-steps, facing the way it walks. It is the 16 x 12 sprite below in one of three coats (by its id),
   with day and night colours, drawn in whole device px per sprite px: PX CSS px at the Plan level, ZK times the zoom (never
   under BEAD_PX) at the Bead level. Each is ringed so it reads on every ground, pale round a dark coat and dark round the
   white one (goats_test.go). Goats are drawn before the birds and the gardeners. Plan and Bead level
   only, only in view, at most MAX drawn (the newest wishes); the rest are counted on their plot's sign ("+2 wishes"). At the
   Project level a project's sign says how many wishes it has open ("3 wishes"); at the World level nothing. The page wakes
   for a goat's next step (onWake), not every frame. Reduced motion: each goat grazes still beside its plot, at its place
   round the ring.
   Ghost or solid (2.3): a wish nothing tends (wishes.open[].tending false: none of its kin is in progress or in review) is a
   ghost goat, drawn whole at GHOST alpha (from a sprite of its own, so its ring never shows through its coat); it turns solid
   while a gardener works any of its kin. Hover or a tap names the wish and who asked (card(), in the steward's tip,
   web/stip.js); a click, or a second tap, opens its thread in the Commons (CHAT.open, the gardeners' Chat). The card's "Zoom to
   it on the map" (gd-xlRpUpTjPZ; goTo, through wire's go) jumps to the plot an out goat grazes by, else to its pen cell;
   Enter on an out goat's pen cell follows it too (grazes). at() finds the
   goat under a board point from what the last draw drew.
   The pen (3.1, gd-kVKuWqIK1v): WORLD.layout lists each project's open wishes, oldest first (so a new wish takes the next cell
   and moves no goat, gd-wDdun2AVwq), in a cell of the fenced Pen of its home strip (biome.pen, the index's wish:<id>;
   homestead-world-design.md 28.2). A wish with no kin on the map grazes in its cell, looking up now and then and turning
   between spells, ghost or solid as anywhere, every one drawn (no MAX: the pen is where they are counted); a goat out grazing
   by its kin keeps its cell, where an open gate says it is out, and hovering the gate names the wish as its goat does. The
   fence is drawn from the Project level in, the goats and gates at Plan and Bead. The pen's sign counts the wishes past its
   cap ("+3 not in the pen").
   A closed wish is no goat but a headstone in its project's graveyard (3.2, gd-s9KzRk6boh; WORLD.layout places it, art.js
   draws it): stoneCard() is its tip, in the same shape as card().
   ring(), tourOf(), place(), wander(), admit(), keepOuts(), pxOf(), card() and stoneCard() are pure and run under node (goats_test.go);
   garden.js calls draw() per render and sign() per refresh of the signs, and wires the pointer through wire(). Units: world
   px, ms. */
var GOATS = (function () {
  "use strict";
  var W = typeof WORLD !== "undefined" ? WORLD : require("./world.js");
  var B = typeof BIRDS !== "undefined" ? BIRDS : require("./birds.js");
  var A = typeof ART !== "undefined" ? ART : require("./art.js");   // the pen's fence (ART.drawFenceClip) and the palette
  var MAX = 12, PX = 1, BEAD_PX = 2, ZK = 1;   // goats drawn; CSS px per sprite px at Plan (a goat 16 x 12); at Bead ZK x the zoom, never under BEAD_PX
  var PER = 12000, WS = [2000, 5000], WALK = 6000, STEP = 200, SPEED = 32, LOOK = [0.4, 0.55];   // a spell (ms), when in it its one walk sets off, the longest walk (a longer way is trotted), one step of it, the pace (world px a second), the look up (of a graze)
  var ADV = 96, JIT = 24, KEEP = 34;   // how far round its tour a goat moves on in a spell, how far a spot strays from that, half the room it keeps from the next goat round (world px; 2 KEEP clears a Plan goat's width and height, round a corner too)
  var GUT = 3, DEPTH = [3, 6], SHIFT = 16, SHIFT_MAX = 384, BIRD_PAD = 20;   // one spell in GUT turns up a near gutter, DEPTH tiles in from the path's middle; a spot not clear moves on by SHIFT, at most SHIFT_MAX; the room kept round a bird's way (world px)
  var RIM = "#e6e2cc", DAY_RIM = "#fff8e6", INK = "#2a1a14";   // the outlines: a dark goat's at night and by day, the white goat's
  var GHOST = 0.5, HIT_PAD = 4, TAP_PAD = 16, GATE = { w: 24, h: 20 };   // an untended wish's alpha; the CSS px a mouse may miss a goat by, and a finger (a Plan goat's 16 x 12 grows to 48 x 44, the 44 px rule); an out goat's open gate in its pen cell, world px
  var T = W.TILE, INKS = "abcdef";   // the sprite's letters, each its colour's place in the palette

  /* The sprite, 16 x 12, facing right: rows of the letters a to f, each the colour at its place in the coat's palette (a the
     coat; b its shade, the belly and the far legs; c horns, nose and hooves; d the eye; e the beard and the tail's tuft), and .
     (none). Frame 0 grazes, head down, its four legs square; frame 1 walks, head up, legs in stride (and looks up); frame 2
     chews, frame 0 with its jaw and beard a px lower; frame 3 is the walk's other step, legs gathered under it, hooves together. */
  var SP = {
    f: [
      ["................",
       "................",
       "................",
       "................",
       "ee..........cc..",
       ".eaaaaaaaaa.c...",
       "..aaaaaaaaaaaa..",
       "..aaaaaaaaaaaaa.",
       "..bbbbbbbbbaadaa",
       "..a..b..b..aaaac",
       "..a..b..b..a.ee.",
       "..c..c..c..c...."],
      ["..........cc....",
       "...........cc...",
       "...........aaaa.",
       "..........aaada.",
       "ee........aaaaac",
       ".eaaaaaaaaaa.e..",
       "..aaaaaaaaaa.e..",
       "..aaaaaaaaaa....",
       "..bbbbbbbbbb....",
       "..a..b..b..a....",
       ".a...b..b...a...",
       ".c...c..c...c..."],
      ["................",
       "................",
       "................",
       "................",
       "ee..........cc..",
       ".eaaaaaaaaa.c...",
       "..aaaaaaaaaaaa..",
       "..aaaaaaaaaaaaa.",
       "..bbbbbbbbbaadaa",
       "..a..b..b..aaaac",
       "..a..b..b..a.aa.",
       "..c..c..c..c.ee."],
      ["..........cc....",
       "...........cc...",
       "...........aaaa.",
       "..........aaada.",
       "ee........aaaaac",
       ".eaaaaaaaaaa.e..",
       "..aaaaaaaaaa.e..",
       "..aaaaaaaaaa....",
       "..bbbbbbbbbb....",
       "..a..b..b..a....",
       "...a.b..b.a.....",
       "....cc..cc......"]
    ]
  };
  /* The coats, a colour per letter by day and by night: a the coat (the one the rim is chosen against), b its shade, c the
     horns, nose and hooves (3:1 off the coat), d the eye, e the beard and tuft. pale: ringed in INK, else in the pale rims. */
  var COATS = {
    white: { day: ["#eee6d2", "#c8bca4", "#6a5240", "#2a1a14", "#a89a84"], night: ["#d8d2c0", "#aaa494", "#6e5c4a", "#2a1a14", "#938a7a"], pale: true },
    brown: { day: ["#5e3418", "#3e200e", "#e8cfa4", "#f0b830", "#2a160a"], night: ["#4e2c18", "#341c0c", "#d8c09a", "#d8a430", "#24140a"] },
    black: { day: ["#2c2830", "#1a171e", "#c8bca8", "#e8b030", "#7a7280"], night: ["#2a2630", "#1a171e", "#bcb0a0", "#d8a430", "#6c6672"] }
  };
  var COAT_KEYS = ["white", "brown", "black"];
  SP.w = SP.f[0][0].length; SP.h = SP.f[0].length;
  /* each frame as runs of one colour {x, y, w, c, i} (c its letter, i that letter's place in the palette), facing right and
     mirrored, so a goat is a few fillRects either way */
  SP.runs = SP.f.map(function (rows) {
    var out = [];
    rows.forEach(function (row, y) {
      for (var x = 0; x < row.length; x++) {
        var c = row.charAt(x), i = INKS.indexOf(c);
        if (c === ".") continue;
        if (i < 0) throw new Error("goats: the sprite has " + JSON.stringify(c) + ", not a to f");
        var r = out[out.length - 1];
        if (r && r.y === y && r.c === c && r.x + r.w === x) r.w++; else out.push({ x: x, y: y, w: 1, c: c, i: i });
      }
    });
    return out;
  });
  SP.left = SP.runs.map(function (rs) { return rs.map(function (r) { return { x: SP.w - r.x - r.w, y: r.y, w: r.w, c: r.c, i: r.i }; }); });

  /* The outline a coat is ringed in. */
  function rimOf(c, night) { return c.pale ? INK : night ? RIM : DAY_RIM; }
  /* The coat of a wish id: the same goat every time. */
  function coatOf(id) { return COAT_KEYS[W.h32(String(id), 11) % COAT_KEYS.length]; }
  /* onLane(lanes, pt) -> is world px point pt on a tile of one of lanes (tiles, a biome's b.lanes)? */
  function onLane(lanes, pt) {
    var tx = Math.floor(pt.x / T), ty = Math.floor(pt.y / T);
    return (lanes || []).some(function (l) { return tx >= l.x && tx < l.x + l.w && ty >= l.y && ty < l.y + l.h; });
  }
  /* sides(r, lanes) -> {x0, x1, y0, y1}, world px: the ring round box r (tiles) one tile out, in the path round it; a side
     that would run down a lane (29.3) runs instead on that lane's verge, the middle of the box's own edge tiles beside it, so
     the goats graze beside the lanes, not on them (X-D11). */
  function sides(r, lanes) {
    var x0 = (r.x - 1) * T, x1 = (r.x + r.w + 1) * T, y0 = (r.y - 1) * T, y1 = (r.y + r.h + 1) * T, my = (r.y + r.h / 2) * T, mx = (r.x + r.w / 2) * T;
    if (onLane(lanes, { x: x0, y: my })) x0 = (r.x + 0.5) * T;
    if (onLane(lanes, { x: x1, y: my })) x1 = (r.x + r.w - 0.5) * T;
    if (onLane(lanes, { x: mx, y: y0 })) y0 = (r.y + 0.5) * T;
    if (onLane(lanes, { x: mx, y: y1 })) y1 = (r.y + r.h - 0.5) * T;
    return { x0: x0, x1: x1, y0: y0, y1: y1 };
  }
  /* ring(r, lanes) -> the ring round box r (tiles) on its sides(), as a closed polyline (world px) from its bottom-left
     corner, along the bottom, up the right, along the top and down the left. */
  function ring(r, lanes) {
    var e = sides(r, lanes), x0 = e.x0, x1 = e.x1, y0 = e.y0, y1 = e.y1;
    return [{ x: x0, y: y1 }, { x: x1, y: y1 }, { x: x1, y: y0 }, { x: x0, y: y0 }, { x: x0, y: y1 }];
  }
  /* The arc of point pt on tour t's ring (world px round it from its start), or null off it. */
  function arcOf(t, pt) {
    for (var i = 1; i < t.pts.length; i++) {
      var a = t.pts[i - 1], b = t.pts[i];
      if (pt.x >= Math.min(a.x, b.x) && pt.x <= Math.max(a.x, b.x) && pt.y >= Math.min(a.y, b.y) && pt.y <= Math.max(a.y, b.y)) return t.cum[i - 1] + Math.abs(pt.x - a.x) + Math.abs(pt.y - a.y);
    }
    return null;
  }
  /* tourOf(plots, lanes) -> the tour of a goat of these plots (at least one; a goat with none stands in its pen): {pts, cum (the arc at each corner), len, box (world
     px), mouths, lanes}. The ring round the box of its plots beside its biome's lanes (ring), and the mouths of their gutters on it, {s (its arc), x, y, dx, dy (the way
     in), axis (v: a gutter across x, h: across y)}, by arc: each plot's gutters (freeLines) run from the path on one side
     to the path on the other. */
  /* freeLines(n) -> the lines clear of beds across a plot n tiles wide, in tiles from its corner: the path round it and the
     gutters between its beds (the same both ways, WORLD.BED). */
  function freeLines(n) {
    var o = W.BED.off, sz = W.BED.size, out = [-1];
    for (var i = 1; i < o.length; i++) out.push((o[i - 1] + sz + o[i]) / 2);
    out.push(n + 1);
    return out;
  }
  function tourOf(plots, lanes) {
    var r = null;
    (plots || []).forEach(function (p) {
      if (!r) { r = { x: p.x, y: p.y, w: p.w, h: p.h }; return; }
      var x1 = Math.max(r.x + r.w, p.x + p.w), y1 = Math.max(r.y + r.h, p.y + p.h);
      r.x = Math.min(r.x, p.x); r.y = Math.min(r.y, p.y); r.w = x1 - r.x; r.h = y1 - r.y;
    });
    var box = r, e = sides(box, lanes), pts = ring(box, lanes), cum = [0];
    for (var i = 1; i < pts.length; i++) cum.push(cum[i - 1] + Math.abs(pts[i].x - pts[i - 1].x) + Math.abs(pts[i].y - pts[i - 1].y));
    var t = { pts: pts, cum: cum, len: cum[cum.length - 1], box: { x: pts[0].x, y: pts[2].y, w: pts[1].x - pts[0].x, h: pts[0].y - pts[2].y }, mouths: [], lanes: lanes || [] };
    var mouth = function (x, y, dx, dy, axis) { var s = arcOf(t, { x: x, y: y }); if (s !== null) t.mouths.push({ s: s, x: x, y: y, dx: dx, dy: dy, axis: axis }); };
    (plots || []).forEach(function (p) {   // a plot's edge on the box's edge meets the ring on that side, a verge or the path
      freeLines(p.w).slice(1, -1).forEach(function (l) { var x = (p.x + l) * T; mouth(x, p.y === box.y ? e.y0 : (p.y - 1) * T, 0, 1, "v"); mouth(x, p.y + p.h === box.y + box.h ? e.y1 : (p.y + p.h + 1) * T, 0, -1, "v"); });
      freeLines(p.h).slice(1, -1).forEach(function (l) { var y = (p.y + l) * T; mouth(p.x === box.x ? e.x0 : (p.x - 1) * T, y, 1, 0, "h"); mouth(p.x + p.w === box.x + box.w ? e.x1 : (p.x + p.w + 1) * T, y, -1, 0, "h"); });
    });
    t.mouths.sort(function (a, b) { return a.s - b.s; });
    return t;
  }
  /* The point s round tour t (any s: it laps), whole world px. */
  function pointAt(t, s) {
    var u = ((s % t.len) + t.len) % t.len;
    for (var i = 1; i < t.pts.length; i++) {
      if (u > t.cum[i]) continue;
      var a = t.pts[i - 1], b = t.pts[i], d = t.cum[i] - t.cum[i - 1], k = d ? (u - t.cum[i - 1]) / d : 0;
      return { x: Math.round(a.x + (b.x - a.x) * k), y: Math.round(a.y + (b.y - a.y) * k) };
    }
    return { x: t.pts[0].x, y: t.pts[0].y };
  }
  /* d as the shortest way round a tour of length n, in [-n/2, n/2). */
  function wrap(d, n) { return ((d % n) + n + n / 2) % n - n / 2; }
  /* place(wishes, world) -> the goats, in the wishes' order (newest first): {id, title, asker, href, tending, kin (kin_total), opened, project, coat,
     herder, checked, acted (FarmWish.herder_*),
     plot (the newest kin's plot on the map, or "pen"), pkey (that plot's project key, "" in the pen), plots (every kin plot on the map in that plot's project, in kin
     order: farm_wishes.go lists them so), tour (tourOf), off (its place round the tour), adv, jit, phase}. The goats of one
     tour are spread evenly round it and share its pace and its clock, so they keep KEEP from each other round it: a crowd
     moves on less each spell (adv), and strays less (jit), and one too crowded to move grazes still. */
  function place(wishes, world) {
    var idx = world && world.index || {}, by = {}, out = [];
    (wishes || []).forEach(function (w) {
      if (!w || !w.id) return;
      var ps = [], c = idx["wish:" + w.id];
      (w.plots || []).forEach(function (pl) { var p = pl && idx[pl.plan]; if (p && p.kind === "plot" && ps.indexOf(p) < 0 && (!ps.length || p.project === ps[0].project)) ps.push(p); });
      if (!ps.length && !(c && c.kind === "goat")) return;   // in no pen cell (past what it holds): counted on the pen's sign
      var g = { id: w.id, title: w.title || w.id, asker: w.asker || "", href: w.href || "", tending: !!w.tending, kin: w.kin_total || 0, opened: w.opened || 0, project: w.project || "", arms: w.arms || [], coat: coatOf(w.id), plot: ps.length ? ps[0].id : "pen", pkey: ps.length ? ps[0].project || "" : "", plots: ps.map(function (p) { return p.id; }),
        herder: w.herder_verdict || "", checked: w.herder_checked_at || 0, acted: !!w.herder_acted };
      if (c && c.kind === "goat") { g.cell = { x: (c.x + c.w / 2) * T, y: (c.y + c.h / 2) * T, w: c.w * T, h: c.h * T }; g.out = ps.length > 0; }   // its place in the pen, its gate there when it is out
      out.push(g);
      if (!ps.length) { g.pen = true; g.phase = W.h32(g.id, 13) % PER; return; }
      var key = g.plots.slice().sort().join(",");
      (by[key] = by[key] || []).push(g);
    });
    Object.keys(by).forEach(function (key) {
      var list = by[key], ps = list[0].plots.map(function (id) { return idx[id]; }), b = idx["project:" + ps[0].project], t = tourOf(ps, b && b.lanes), gap = t.len / list.length;
      var adv = Math.max(0, Math.min(ADV, Math.floor(gap - 2 * KEEP))), jit = Math.max(0, Math.min(JIT, Math.floor((gap - adv) / 2 - KEEP)));
      var o0 = W.h32(key, 17) % t.len, ph = W.h32(key, 13) % PER;
      list.forEach(function (g, i) { g.tour = t; g.off = Math.round(o0 + gap * i); g.adv = adv; g.jit = jit; g.phase = ph; });
    });
    return out;
  }
  function meets(a, v) { return a.x < v.x + v.w && v.x < a.x + a.w && a.y < v.y + v.h && v.y < a.y + a.h; }
  /* Is a goat of size o.size centred on pt clear of every box of o.keep? */
  function clear(pt, o) {
    var k = o.keep, sz = o.size || { w: SP.w, h: SP.h };
    if (!k || !k.length) return true;
    var bx = { x: pt.x - sz.w / 2, y: pt.y - sz.h / 2, w: sz.w, h: sz.h };
    for (var i = 0; i < k.length; i++) if (meets(bx, k[i])) return false;
    return true;
  }
  /* The first clear point off the lanes from arc s on round goat g's tour, by SHIFT up to SHIFT_MAX; none clear: the first
     point off the lanes on round it, whoever stands there (the ring is off the lanes but where it crosses one). */
  function onRing(g, s, o) {
    var t = g.tour, p, j;
    for (j = 0; j <= SHIFT_MAX; j += SHIFT) { p = pointAt(t, s + j); if (clear(p, o) && !onLane(t.lanes, p)) return { s: s + j, x: p.x, y: p.y, m: null }; }
    for (j = 0; j < t.len; j += SHIFT) { p = pointAt(t, s + j); if (!onLane(t.lanes, p)) break; }
    return { s: s + j, x: p.x, y: p.y, m: null };
  }
  /* Where goat g grazes in its k-th spell, {s (its arc round the tour, unbounded), x, y, m (the gutter's mouth, or null)}: adv on
     from the last, strayed by up to jit, by its id. One spell in GUT it turns up the nearest gutter that fits (o.fit[axis])
     within half a spell's way, DEPTH tiles in, when that is clear; else it is on the ring, moved on until clear. */
  function spotOf(g, k, o) {
    if (g.memoFor !== o || g.memoN > 32) { g.memoFor = o; g.memo = {}; g.memoN = 0; }   // the same o, the same spots: a frame within a spell computes none
    var c = g.memo[k];
    if (!c) { c = g.memo[k] = spotAt(g, k, o); g.memoN++; }
    return c;
  }
  /* The way goat g walks in its k-th spell, from the last spell's spot to this one's (routeOf), memoised like the spots. */
  function walkOf(g, k, o) { var r = spotOf(g, k, o) && g.memo["r" + k]; return r || (g.memo["r" + k] = routeOf(g.tour, spotOf(g, k - 1, o), spotOf(g, k, o))); }
  function spotAt(g, k, o) {
    var t = g.tour, base = g.off + k * g.adv;
    if (o.fit && W.h32(g.id + ":" + k, 19) % GUT === 0) {
      var m = null, h = W.h32(g.id + ":" + k, 23);
      t.mouths.forEach(function (c) { var d = Math.abs(wrap(c.s - base, t.len)); if (o.fit[c.axis] && d <= g.adv / 2 && (!m || d < m.d)) m = { d: d, c: c }; });
      if (m) {
        var c = m.c, dep = DEPTH[0] * T + h % ((DEPTH[1] - DEPTH[0]) * T + 1), pt = { x: c.x + c.dx * dep, y: c.y + c.dy * dep };
        if (clear(pt, o) && !onLane(t.lanes, pt)) return { s: base + wrap(c.s - base, t.len), x: pt.x, y: pt.y, m: c };
      }
    }
    return onRing(g, base + (g.jit ? Math.round(((W.h32(g.id + ":" + k, 5) % 1001) / 500 - 1) * g.jit) : 0), o);
  }
  /* The corners of tour t strictly between arcs s0 and s1, in the order a walk from s0 to s1 meets them. */
  function corners(t, s0, s1) {
    var out = [], lo = Math.min(s0, s1), hi = Math.max(s0, s1), n = t.pts.length - 1;
    for (var lap = Math.floor(lo / t.len); lap * t.len < hi; lap++) {
      for (var j = 0; j < n; j++) { var v = lap * t.len + t.cum[j]; if (v > lo && v < hi) out.push({ v: v, x: t.pts[j].x, y: t.pts[j].y }); }
    }
    return out.sort(function (a, b) { return s1 >= s0 ? a.v - b.v : b.v - a.v; });
  }
  /* The way from spot a to spot b, {pts, len, dur}: out of a's gutter, round the ring, up b's; dur its time at SPEED in whole
     steps, at most WALK. */
  function routeOf(t, a, b) {
    var pts = [{ x: a.x, y: a.y }], len = 0;
    var add = function (p) { var q = pts[pts.length - 1]; if (p.x !== q.x || p.y !== q.y) { len += Math.abs(p.x - q.x) + Math.abs(p.y - q.y); pts.push({ x: p.x, y: p.y }); } };
    add(pointAt(t, a.s));
    corners(t, a.s, b.s).forEach(add);
    add(pointAt(t, b.s));
    add(b);
    return { pts: pts, len: len, dur: Math.min(WALK, Math.ceil(len / SPEED * 1000 / STEP) * STEP) };
  }
  /* The point d along route pts, and the way a goat faces there: its run across x, else the last before it, else the next. */
  function along(pts, d) {
    if (pts.length < 2) return { x: pts[0].x, y: pts[0].y, dir: 0 };   // no way to go: two spells on one spot
    var i = 1, left = d;
    for (; i < pts.length - 1; i++) { var s = Math.abs(pts[i].x - pts[i - 1].x) + Math.abs(pts[i].y - pts[i - 1].y); if (left <= s) break; left -= s; }
    var a = pts[i - 1], b = pts[i], n = Math.abs(b.x - a.x) + Math.abs(b.y - a.y), k = n ? Math.min(1, left / n) : 1, dir = 0;
    for (var j = i; j > 0 && !dir; j--) dir = Math.sign(pts[j].x - pts[j - 1].x);
    for (j = i + 1; j < pts.length && !dir; j++) dir = Math.sign(pts[j].x - pts[j - 1].x);
    return { x: Math.round(a.x + (b.x - a.x) * k), y: Math.round(a.y + (b.y - a.y) * k), dir: dir };
  }
  /* The way a goat faces at the end of route r. */
  function faceOf(r, dir0) { return along(r.pts, r.len).dir || dir0; }
  /* When in spell k goat g's walk sets off (ms), by its id, so the goats of one tour do not walk in step. */
  function setOff(g, k) { return WS[0] + W.h32(g.id + ":" + k, 7) % (WS[1] - WS[0]); }
  /* A goat grazing at p from e0 to e1 (ms in the spell), at e: head down, chewing (frames 0 and 2 by turns, a STEP each), and up
     through LOOK of it. */
  function graze(p, e0, e1, e, dir) {
    var l0 = e0 + Math.round((e1 - e0) * LOOK[0]), l1 = e0 + Math.round((e1 - e0) * LOOK[1]), up = e >= l0 && e < l1;
    if (up) return { x: p.x, y: p.y, f: 1, dir: dir, walking: false, next: l1 - e };
    var u = e % STEP;
    return { x: p.x, y: p.y, f: Math.floor(e / STEP) % 2 ? 2 : 0, dir: dir, walking: false, next: Math.min(STEP - u, e < l0 ? l0 - e : e1 - e) };
  }
  /* wander(g, t, o) -> {x, y, f, dir, walking, next}: goat g at t (its centre, world px), its frame (0 and 2 grazing and chewing
     by turns; 1 and 3 walking, a step each; 1 looking up), the way it faces (1 right, -1 left), and the ms until it next looks different. o = {still, fit {v, h}: the
     gutters its body fits, keep: the boxes it never grazes under (keepOuts), size: its box, world px}; o true is still. Each
     PER ms spell it grazes, walks once (setting off at setOff, at SPEED) to the spell's spot and grazes there into the next.
     Still (reduced motion), or too crowded to move: grazing at its place round the ring. */
  function wander(g, t, o) {
    o = o === true ? { still: true } : o || {};
    var dir0 = W.h32(g.id, 3) % 2 ? 1 : -1;
    if (g.pen) return penned(g, t, o.still, dir0);
    if (o.still || !g.adv) { var h = onRing(g, g.off, o); return { x: h.x, y: h.y, f: 0, dir: dir0, walking: false, next: Infinity }; }
    var u = t + g.phase, k = Math.floor(u / PER), e = u - k * PER, ws = setOff(g, k);
    var a = spotOf(g, k - 1, o), b = spotOf(g, k, o), r = walkOf(g, k, o);
    if (e < ws) {   // still grazing where the last spell's walk ended
      var r0 = walkOf(g, k - 1, o);
      return graze(a, setOff(g, k - 1) + r0.dur - PER, ws, e, faceOf(r0, dir0));
    }
    if (e < ws + r.dur) {
      var n = r.dur / STEP, i = Math.min(n, Math.floor((e - ws) / STEP) + 1), q = along(r.pts, r.len * i / n);
      return { x: q.x, y: q.y, f: i % 2 ? 1 : 3, dir: q.dir || dir0, walking: true, next: STEP - (e - ws) % STEP };
    }
    return graze(b, ws + r.dur, PER + setOff(g, k + 1), e, faceOf(r, dir0));
  }
  /* A goat in the pen grazes at its cell's middle, looks up through each spell's LOOK and turns now and then, between spells;
     still (reduced motion), head down at its cell, facing dir0. */
  function penned(g, t, still, dir0) {
    var c = g.cell;
    if (still) return { x: c.x, y: c.y, f: 0, dir: dir0, walking: false, next: Infinity };
    var u = t + g.phase, k = Math.floor(u / PER);
    return graze(c, 0, PER, u - k * PER, W.h32(g.id + ":" + k, 29) % 3 ? dir0 : -dir0);
  }
  /* The box goat g may stand in over its time, world px, padded by pad: its tour's (its gutters are inside it), or its pen cell. */
  function range(g, pad) { var b = g.pen ? { x: g.cell.x - g.cell.w / 2, y: g.cell.y - g.cell.h / 2, w: g.cell.w, h: g.cell.h } : g.tour.box; return { x: b.x - pad, y: b.y - pad, w: b.w + 2 * pad, h: b.h + 2 * pad }; }
  /* admit(goats, view, max) -> {shown, more}: of the goats whose tour is in view (world px; none: all), the first max (MAX) in
     order, the newest wishes; more counts the rest by plot, for its sign. A goat in the pen is in view when its cell is and is
     always shown (a pen holds what its cell fits, standing still between looks): the pen is where they are counted. */
  function admit(goats, view, max) {
    var cap = max == null ? MAX : max, out = { shown: [], more: {} }, n = 0;
    (goats || []).forEach(function (g) {
      if (view && !meets(range(g, 2 * T), view)) return;
      if (g.pen) out.shown.push(g); else if (n < cap) { out.shown.push(g); n++; } else out.more[g.plot] = (out.more[g.plot] || 0) + 1;
    });
    return out;
  }
  /* keepOuts(lv, world) -> the boxes no goat grazes under, world px: each gardener standing shown on a plot at this level (lv,
     a level of WORLD.gardenerState), its 16 x 24 figure, and the ground its birds fly out over (BIRDS.route: up off its head
     and away, left or right; its hawk circles over the head), padded by BIRD_PAD. */
  function keepOuts(lv, world) {
    var out = [];
    if (!lv || !lv.spots || !world || !world.index) return out;
    Object.keys(lv.spots).sort().forEach(function (id) {
      var s = lv.spots[id], p = s && s.shown ? world.index[s.plot] : null, x0, x1, y0, y1;
      if (!p) return;
      out.push({ x: s.x, y: s.y, w: 16, h: 24 });
      var pts = B.route(s, "sparrow", 0).pts.concat(B.route(s, "sparrow", 1).pts);   // its way up, left and right
      x0 = Math.min.apply(null, pts.map(function (q) { return q.x; })); x1 = Math.max.apply(null, pts.map(function (q) { return q.x; }));
      y0 = Math.min.apply(null, pts.map(function (q) { return q.y; })); y1 = Math.max.apply(null, pts.map(function (q) { return q.y; }));
      out.push({ x: x0 - BIRD_PAD, y: y0 - BIRD_PAD, w: x1 - x0 + 2 * BIRD_PAD, h: y1 - y0 + 2 * BIRD_PAD });
    });
    return out;
  }

  /* Who asked, as a handle: the operated_by's name before its domain. */
  function handle(who) { return String(who || "").replace(/^oidc:/, "").split("@")[0]; }
  /* goTo(g) -> {id, at}: where the card's "Zoom to it on the map" takes the cursor (CURNAV.to on id): the plot the goat
     grazes by (g.grazes, or place's plot) when it is out, else its cell in the pen (wish:<id>); and that place's ?at= value,
     the link's href without script (<project>/<plot>, "~" for the loose beads' project; <project>/wish:<id>). */
  function goTo(g) {
    var p = g.grazes || g.plot, k = g.pkey || "";
    if (p && p !== "pen") return { id: p, at: (k === "" ? "~" : encodeURIComponent(k)) + "/" + encodeURIComponent(p) };
    return { id: "wish:" + g.id, at: (g.project ? encodeURIComponent(W.projectKey(g.project)) : "~") + "/" + encodeURIComponent("wish:" + g.id) };
  }
  /* The herder's verdict in words, with what it does (acted: only when it did; gardenhub.HerderWords), and its tip line's class. */
  var HERDER = { fulfilled: ["fulfilled, closing", "rsn"], "in-progress": ["in progress", "who"], unfulfilled: ["unfulfilled", "rsn rsn-parked", "agent woken"], unclear: ["unclear", "rsn rsn-parked", "asked"] };
  function herderWords(v, acted) { var h = HERDER[v]; return h ? h[0] + (acted && h[2] ? ", " + h[2] : "") : ""; }
  /* card(g, base, ago) -> the goat's tip, as WORLD.stewardCard's {title, lines, href, tip}: the wish, who asked, whether it is
     tended (never alpha alone) and how much came of it, the herder's verdict, its thread in the Commons and "Zoom to it on the map" (goTo; a
     gate's card too, so from the pen an out goat is a click away). ago(unix s) is the page's "3 days". arms: its kin's
     line arms (FarmWish.arms), drawn before the title by stip.js and named in tip. */
  function card(g, base, ago) {
    var who = handle(g.asker), lines = [], n = g.kin || 0, to = goTo(g), arms = g.arms || [], hv = herderWords(g.herder, g.acted);
    var st = g.tending ? "Tended: a gardener is at work on it" : n ? "Untended: none of its " + (n === 1 ? "step" : n + " steps") + " is in progress" : "Untended: nothing has come of it yet";
    lines.push(["", who ? "Asked by " + who : "Nobody is named as asking"]);
    lines.push(["rsn" + (g.tending ? "" : " rsn-sleeping"), st]);
    if (hv) lines.push([HERDER[g.herder][1], "Herder: " + hv + (g.checked && ago ? " · checked " + ago(g.checked) + " ago" : "")]);
    lines.push(["who", "wish" + (g.project ? " · " + g.project : "") + (g.opened && ago ? " · opened " + ago(g.opened) + " ago" : "") + (g.out ? " · out grazing by its plan" : "")]);
    lines.push(["why", [{ text: "Thread in the Commons", href: (base || "") + g.href, cls: "gchat", id: g.id }, { text: "Zoom to it on the map", href: (base || "") + "/garden?at=" + to.at, cls: "ggoto", id: to.id }]]);
    return { title: g.title, arms: arms, lines: lines, href: (base || "") + g.href, tip: "Wish: " + g.title + (who ? ", asked by " + who : "") + ". " + st + "." + (hv ? " Herder: " + hv + "." : "") +
      (arms.length ? " " + arms.map(function (a) { return a.name || a.canonical; }).join("; ").replace(/^./, function (c) { return c.toUpperCase(); }) + "." : "") };
  }

  /* A closed wish's outcome (FarmClosedWish.outcome) and its close reason in words: the bridge's slugs said plainly, anything
     else (a kd close reason someone typed) as written. */
  var OUTCOME = { done: "Done", abandoned: "Abandoned", expired: "Expired" };
  var REASON = { "slack-thread-idle": "the thread went quiet and closed itself", "slack-thread-idle-backfill": "the thread went quiet and closed itself",
    "slack-thread-user-close": "the asker closed it", "slack-thread-agent-failed": "its agent failed and nobody took it up" };
  function outcomeWord(o) { return OUTCOME[o] || "Done"; }
  function reasonWords(r) { r = String(r || "").trim(); return REASON[r] || r; }
  /* stoneCard(w, base, ago) -> a headstone's tip (gd-s9KzRk6boh), as card's {title, lines, href, tip}: the closed wish, who
     asked, how it ended and why (the stone itself shows only the title and when it closed), what its kin merged (the merge
     requests from the produce bin's read, FarmClosedWish.produce) and its thread in the Commons. */
  function stoneCard(w, base, ago) {
    var who = handle(w.asker), lines = [], why = reasonWords(w.reason), st = outcomeWord(w.outcome) + (why ? ": " + why : "");
    var prod = w.produce || [], more = w.produce_more || 0, n = prod.length + more, href = (base || "") + (w.href || "/garden/c/bead/" + encodeURIComponent(w.id));
    lines.push(["", who ? "Asked by " + who : "Nobody is named as asking"]);
    lines.push(["rsn" + (w.outcome === "done" || !w.outcome ? "" : " rsn-sleeping"), st]);
    lines.push(["who", "closed wish" + (w.project ? " · " + w.project : "") + (w.closed && ago ? " · closed " + ago(w.closed) + " ago" : "")]);
    lines.push(["", n ? "Its kin merged " + (n === 1 ? "1 merge request" : n + " merge requests") + (more ? ", the newest " + prod.length + " here" : "") : w.kin ? "Its kin merged nothing" : "Nothing came of it"]);
    if (prod.length) lines.push(["why", prod.map(function (p) { var t = String(p.title || "").trim(); return { text: (p.iid ? p.iid + " " : "") + (t.length > 40 ? t.slice(0, 39) + "…" : t), href: p.url || "" }; })]);
    lines.push(["why", [{ text: "Thread in the Commons", href: href, cls: "gchat", id: w.id }]]);
    return { title: w.title, lines: lines, href: href, tip: "Closed wish: " + w.title + (who ? ", asked by " + who : "") + ". " + st + "." + (n ? " Its kin merged " + n + "." : "") };
  }

  /* ---------- the page's side ---------- */
  var pinned = null, force = false, wishes = [], counts = {}, placed = null, placedFor = null, placedDoc = -1, docN = 0, more = {}, penMore = {}, fulfilled = 0, said = "", drawn = [], wakeFn = null, timer = null, timerAt = Infinity;
  var kept = [], keptFor = null, keptLv = -1, wo = {};
  function now() { return pinned === null ? Date.now() : pinned; }
  /* countOf(doc) -> each project's open wishes (its key: WORLD.projectKey), the ones the farm's cap left out too (open_more). */
  function countOf(doc) {
    var out = {}, add = function (p, n) { var k = W.projectKey(p); if (k && n > 0) out[k] = (out[k] || 0) + n; };
    ((doc && doc.open) || []).forEach(function (w) { if (w && w.id) add(w.project, 1); });
    var m = (doc && doc.open_more) || {};
    Object.keys(m).forEach(function (p) { add(p, m[p]); });
    return out;
  }
  if (typeof document !== "undefined" && document.addEventListener) {
    document.addEventListener("farmdoc", function (e) { var w = (e.detail || {}).wishes; wishes = (w && w.open) || []; counts = countOf(w); fulfilled = (w && w.fulfilled) || 0; docN++; });
  }
  /* Wake the page in ms for a goat's next step (one timer, the soonest). */
  function arm(ms) {
    if (!wakeFn || !isFinite(ms) || typeof setTimeout === "undefined") return;
    var t = now(), at = t + Math.max(16, ms);
    if (timer && at >= timerAt && timerAt > t) return;   // one sooner is already set (an overdue one is replaced)
    if (timer) clearTimeout(timer);
    timerAt = at;
    timer = setTimeout(function () { timer = null; timerAt = Infinity; if (wakeFn) wakeFn(); }, at - t);
  }
  /* pxOf(z, dpr, level) -> device px per sprite px, whole so the goat stays crisp: PX CSS px at the Plan level (never under the
     zoom's own), ZK x the zoom at the Bead level but never under BEAD_PX. */
  function pxOf(z, dpr, level) { return Math.max(1, Math.round((level > 2 ? Math.max(BEAD_PX, ZK * z) : Math.max(z, PX)) * dpr)); }
  /* The frame goats are drawn in: device px when the context can say its transform (the page), {a, e, f} the world transform's
     scale and offset, n the device px per sprite px and r the outline's width; else world px, one per sprite px (node). */
  function screen(ctx, z, level) {
    var m = z && ctx.getTransform ? ctx.getTransform() : null;
    if (!m) return { a: 1, e: 0, f: 0, n: 1, r: 1, d: 1 };
    var dpr = Math.round(m.a / z * 1000) / 1000, n = pxOf(z, dpr, level);   // the matrix is single precision
    return { m: m, a: m.a, e: m.e, f: m.f, n: n, r: Math.max(1, Math.round(dpr), Math.round(n / 4)), d: dpr };
  }
  /* One goat of coat c, frame f, facing dir, centred on world point (x, y): its outline first, then its colours. Returns
     the box it covers, its outline in it. A ghost (alpha under 1) is drawn whole from its own sprite (sprite()) at that alpha,
     so its ring does not show through its coat; with no canvas to make one (node), it is the same fills at that alpha. */
  function paint(ctx, sc, c, x, y, f, dir, night, alpha) {
    var cs = night ? c.night : c.day, n = sc.n, k = sc.r, runs = dir < 0 ? SP.left[f] : SP.runs[f];
    var x0 = Math.round(sc.a * x + sc.e - SP.w * n / 2), y0 = Math.round(sc.a * y + sc.f - SP.h * n / 2), a0 = ctx.globalAlpha;
    var box = { x: x0 - k, y: y0 - k, w: SP.w * n + 2 * k, h: SP.h * n + 2 * k };
    if (alpha < 1) {
      var bm = ctx.drawImage ? sprite(c, f, dir, night, n, k) : null;
      ctx.globalAlpha = (a0 == null ? 1 : a0) * alpha;
      if (bm) { ctx.drawImage(bm, box.x, box.y); ctx.globalAlpha = a0; return box; }
      paint(ctx, sc, c, x, y, f, dir, night, 1);
      ctx.globalAlpha = a0;
      return box;
    }
    ctx.fillStyle = rimOf(c, night);
    runs.forEach(function (r) { ctx.fillRect(x0 + r.x * n - k, y0 + r.y * n - k, r.w * n + 2 * k, n + 2 * k); });
    runs.forEach(function (r) { ctx.fillStyle = cs[r.i]; ctx.fillRect(x0 + r.x * n, y0 + r.y * n, r.w * n, n); });
    return box;
  }
  /* A goat drawn solid onto a canvas of its own, its outline in it, kept per coat, frame, way, night and size (a few dozen at
     most); null with no document to make a canvas in. */
  var sprites = {}, spriteN = 0;
  function sprite(c, f, dir, night, n, k) {
    var key = [COAT_KEYS.filter(function (q) { return COATS[q] === c; })[0], f, dir < 0 ? -1 : 1, night ? 1 : 0, n, k].join();
    if (key in sprites) return sprites[key];
    if (typeof document === "undefined" || !document.createElement) return null;
    if (spriteN > 96) { sprites = {}; spriteN = 0; }
    var cv = document.createElement("canvas"), x = cv.getContext && cv.getContext("2d");
    if (!x) return (sprites[key] = null);
    cv.width = SP.w * n + 2 * k; cv.height = SP.h * n + 2 * k;
    paint(x, { a: 1, e: 0, f: 0, n: n, r: k }, c, k + SP.w * n / 2, k + SP.h * n / 2, f, dir, night, 1);
    spriteN++;
    return (sprites[key] = cv);
  }
  /* placedIn(world) -> the goats placed on this layout for the current document (place), kept until either changes. */
  function placedIn(world) { if (placedFor !== world || placedDoc !== docN) { placed = place(wishes, world); placedFor = world; placedDoc = docN; } return placed; }
  /* grazes(id, world) -> the plot the wish's goat grazes by on this layout (its newest kin's, place's plot), "pen" when it
     stands in the pen, or "" when it is no goat there: Enter on an out goat's pen cell follows it to that plot (garden.js
     cursorAct, gd-xlRpUpTjPZ). */
  function grazes(id, world) { var gs = world ? placedIn(world) : [], i; for (i = 0; i < gs.length; i++) if (gs[i].id === id) return gs[i].plot; return ""; }
  /* Draws the goats in world px (the caller has set the world transform), before the birds and the gardeners. o = {level, z
     (the camera's zoom), night, view (world px), world, gs (WORLD.gardenerState: the gardeners a goat keeps clear of), reduce}.
     A goat turns up the gutters (1 tile) its body fits at this zoom. Below the Plan level none. Returns false: a goat asks for
     its next step through onWake, not for every frame. */
  function draw(ctx, o) {
    var t = now(), pens = ((o.world && o.world.biomes) || []).map(function (b) { return b.pen; }).filter(function (p) { return p && p.items.length; });
    drawn = [];
    if (o.level >= 1) pens.forEach(function (pen) { if (!o.view || meets(W.tiles(pen), o.view)) A.drawFenceClip(ctx, A.palette(o.night), pen.x, pen.y, pen.w, pen.h, pen.x, pen.y, Math.max(pen.w, pen.h), o.sp); });   // each pen, from the Project level in
    if (o.level < 2 || !o.world) { more = {}; penMore = {}; return false; }
    placedIn(o.world);
    var a = admit(placed, o.view, MAX), sc = screen(ctx, o.z, o.level), soon = Infinity;
    more = a.more; penMore = {}; pens.forEach(function (pen) { penMore[pen.id] = pen.more || 0; });
    var gone = placed.filter(function (g) { return g.out && (!o.view || meets(range({ pen: true, cell: g.cell }, 0), o.view)); });
    gone.forEach(function (g) { gate(ctx, g.cell, o.night); });   // a goat out grazing: its pen cell's gate stands open
    if (!a.shown.length && !gone.length) return false;
    if (keptFor !== o.gs || keptLv !== o.level) { kept = keepOuts(o.gs && o.gs[o.level], o.world); keptFor = o.gs; keptLv = o.level; }
    var bw = SP.w * sc.n / sc.a, bh = SP.h * sc.n / sc.a, e = 0.01;   // its body, world px; the matrix is single precision
    var key = [o.reduce && !force, bw <= T + e, bh <= T + e, Math.round(bw + 2 * sc.r / sc.a), Math.round(bh + 2 * sc.r / sc.a)].join();
    if (wo.key !== key || wo.keep !== kept) wo = { key: key, still: o.reduce && !force, fit: bw <= T + e || bh <= T + e ? { v: bw <= T + e, h: bh <= T + e } : null, keep: kept, size: { w: bw + 2 * sc.r / sc.a, h: bh + 2 * sc.r / sc.a } };   // kept while nothing changes, so the goats' spots are too
    if (sc.m) ctx.setTransform(1, 0, 0, 1, 0, 0);
    a.shown.forEach(function (g) {
      var p = wander(g, t, wo), bx = paint(ctx, sc, COATS[g.coat], p.x, p.y, p.f, p.dir, o.night, g.tending ? 1 : GHOST), d = sc.d || 1;
      drawn.push({ g: g, id: g.id, plot: g.plot, x: p.x, y: p.y, f: p.f, dir: p.dir, box: bx, css: { x: bx.x / d, y: bx.y / d, w: bx.w / d, h: bx.h / d }, wbox: { x: p.x - bw / 2, y: p.y - bh / 2, w: bw, h: bh } });
      if (p.next < soon) soon = p.next;
    });
    if (sc.m) ctx.setTransform(sc.m.a, sc.m.b, sc.m.c, sc.m.d, sc.m.e, sc.m.f);
    gone.forEach(function (g) {   // last, so at() finds a gate under the goats; its box in CSS px through the world transform
      var c = g.cell, m = sc.m || { a: 1, e: 0, f: 0 }, d = sc.d || 1, gb = { x: c.x - GATE.w / 2, y: c.y - GATE.h / 2, w: GATE.w, h: GATE.h };
      drawn.push({ g: g, id: g.id, plot: "pen", out: true, x: c.x, y: c.y, f: 0, dir: 1, css: { x: (m.a * gb.x + m.e) / d, y: (m.a * gb.y + m.f) / d, w: m.a * gb.w / d, h: m.a * gb.h / d }, wbox: gb });
    });
    arm(soon);
    return false;
  }
  /* The open gate of a goat out grazing, in its pen cell c (world px, the world transform set): a post and a gate of two rails
     swung open off it, in the fence's colours and ink, GATE wide and tall about the cell's middle. */
  function gate(ctx, c, night) {
    var pal = A.palette(night), x = Math.round(c.x - GATE.w / 2), y = Math.round(c.y - GATE.h / 2), k = pal.k;
    var box = function (col, bx, by, bw, bh) { ctx.fillStyle = col; ctx.fillRect(x + bx, y + by, bw, bh); };
    box(k, 0, 0, 6, GATE.h); box(pal.c, 1, 1, 2, GATE.h - 2); box(pal.B, 3, 1, 2, GATE.h - 2);   // the post
    [3, 12].forEach(function (ry) { box(k, 6, ry, GATE.w - 6, 5); box(pal.c, 6, ry + 1, GATE.w - 7, 1); box(pal.b, 6, ry + 2, GATE.w - 7, 2); });   // the rails, swung out
    box(k, GATE.w - 5, 2, 5, GATE.h - 6); box(pal.b, GATE.w - 4, 3, 3, GATE.h - 8);   // its far stile, short: it stands open towards you
  }
  /* at(x, y, pad) -> the goat the last draw drew under board point (x, y) (CSS px; the canvas fills the board), missed by up to
     pad (HIT_PAD) CSS px round its box, its outline in it: the last drawn, which is on top. {id, title, asker, href, tending, kin,
     opened, project, arms, herder, checked, acted, plot (where it was hit: its plot, or "pen" for a goat in the pen and for an open gate), out (a gate's
     hit), grazes (the plot its newest kin is on, "pen" when it stands in the pen) and pkey (that plot's project key), x, y,
     w, h (its box at that moment, world px)}, or null. */
  function at(x, y, pad) {
    var p = pad == null ? HIT_PAD : pad;
    for (var i = drawn.length - 1; i >= 0; i--) {
      var d = drawn[i], b = d.css;
      if (x >= b.x - p && x < b.x + b.w + p && y >= b.y - p && y < b.y + b.h + p) {
        var g = d.g, w = d.wbox;
        return { id: g.id, title: g.title, asker: g.asker, href: g.href, tending: g.tending, kin: g.kin, opened: g.opened, project: g.project, arms: g.arms, herder: g.herder, checked: g.checked, acted: g.acted, plot: d.plot, out: !!d.out, grazes: g.plot, pkey: g.pkey || "", x: w.x, y: w.y, w: w.w, h: w.h };
      }
    }
    return null;
  }
  /* wire(board, cv, env) -> {click, go, enter}: the page's pointer on the goats, which are drawn, not links (garden.js wires
     it). env = {level(), local(e) (board px), open(g): its thread in the Commons, tapped(): the cell's tap is spent (garden.js
     tapWas), world(): the layout, to(id, keyed) / into(id): the cursor onto a Thing / into a plot (garden.js cursorTo,
     cursorInto), focus(): the cursor's link takes focus}. The ground the goats are hit on is the canvas and the cursor's cell
     buttons that span the pen (penGround): its home cell's, the Pen zone's and a goat's own cell, which cover every penned goat
     and its gate, so from the strip a goat still names itself. A mouse over a goat on that ground, at the Plan level or closer, names its wish (the steward's tip,
     STIP.goat) and off it the tip goes; click(e, x, y), the board's click on the canvas (and a pen cell's, before the cell's
     own: the board's capture), says whether it was a goat's: a mouse's opens its thread, a finger's first tap names it (TAP_PAD
     round it, the 44 px rule) and the next opens the thread. Browser only.
     go(id), the way to a goat (goTo's id; the card's "Zoom to it on the map", through STIP's env().go, and enter): its pen
     cell as it is, framed at the Bead level; the plot it grazes by from inside, the cursor on the plot's first phase as a
     gardener's "Show on the map" (into), since the plot itself frames its project at the Project level, where no goat is drawn.
     False when it is not on the map.
     enter(n), Enter on the cursor's Thing n (garden.js cursorAct): an out goat's pen cell follows it to the plot it grazes by
     (grazes), since a goat has nothing to descend into, so the key that goes in goes where it went; false for any other Thing. */
  function wire(board, cv, env) {
    var ptr = "";
    function penGround(e) { var t = e.target, n = t && t._n; return t === cv || !!(n && (n.kind === "goat" || n.kind === "home" || n.zone === "pen")); }
    board.addEventListener("pointerdown", function (e) { ptr = e.pointerType || ""; }, true);
    board.addEventListener("pointermove", function (e) {
      if (e.pointerType !== "mouse" || e.buttons) return;
      var q = env.local(e), g = penGround(e) && env.level() >= 2 ? at(q[0], q[1]) : null;
      if (g) { if (STIP.goatOf() !== g.id) STIP.goat(g, false); } else if (STIP.goatOf()) STIP.goat(null);
    });
    board.addEventListener("pointerleave", function (e) { if (e.pointerType === "mouse") STIP.goat(null); });
    function click(e, x, y) {
      var finger = ptr && ptr !== "mouse", g = env.level() >= 2 ? at(x, y, finger ? TAP_PAD : HIT_PAD) : null;
      if (!g) { if (STIP.goatOf()) STIP.goat(null, true); return false; }
      e.preventDefault();
      if (finger && STIP.goatOf() !== g.id) { STIP.goat(g, true); return true; }   // the first tap names it
      STIP.goat(null, true);
      env.open(g);
      return true;
    }
    board.addEventListener("click", function (e) {   // a goat or gate under a cell that spans the pen: its card and its thread, not the cell's tap
      if (e.target === cv || e.defaultPrevented || !penGround(e) || e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      var q = env.local(e);
      if (click(e, q[0], q[1])) { e.stopImmediatePropagation(); env.tapped(); }
    }, true);
    function go(id) { var ok = String(id).indexOf("wish:") === 0 ? env.to(id, false) : env.into(id); if (ok) env.focus(); return ok; }
    function enter(n) { var p = n && n.kind === "goat" && n.ref && n.ref.out ? grazes(n.open, env.world()) : ""; return !!p && p !== "pen" && go(p); }
    return { click: click, go: go, enter: enter };
  }
  /* The plots with goats not drawn, and how many, as one string. */
  function moreSig() { return Object.keys(more).sort().map(function (k) { return k + ":" + more[k]; }).join(","); }
  /* What the signs say, as one string: the plots' goats past MAX and the projects' open wishes. */
  function sig() { return moreSig() + "|" + JSON.stringify(penMore) + "|" + fulfilled + "|" + Object.keys(counts).sort().map(function (k) { return k + ":" + counts[k]; }).join(","); }
  /* sign(n, status, level) -> status: the words for them on cursor Thing n's status line in the title bar (garden.js
     syncBar): at the Plan and Bead levels a plot's gains "+N wishes" for its goats past MAX; at the Project level a project's
     says how many wishes it has open, "3 wishes". Each project's pen's, those past what it holds and those the herder will
     close (FarmWishes.fulfilled). */
  function sign(n, status, level) {
    var k = 0, w = "";
    said = sig();
    if (!n) return status;
    if (level === 1 && n.kind === "biome" && (k = counts[n.key])) w = k + (k === 1 ? " wish" : " wishes");
    else if (level >= 2 && n.kind === "plot" && (k = more[n.id])) w = "+" + k + (k === 1 ? " wish" : " wishes");
    else if (level >= 2 && n.kind === "pen") w = [penMore[n.id] ? "+" + penMore[n.id] + " not in the pen" : "", fulfilled ? fulfilled + " fulfilled, closing" : ""].filter(Boolean).join(" · ");
    return w ? (status ? status + " · " + w : w) : status;
  }

  return {
    MAX: MAX, GATE: GATE, PX: PX, BEAD_PX: BEAD_PX, ZK: ZK, GHOST: GHOST, HIT_PAD: HIT_PAD, TAP_PAD: TAP_PAD, PER: PER, WS: WS, WALK: WALK, STEP: STEP, SPEED: SPEED, ADV: ADV, JIT: JIT, KEEP: KEEP,
    GUT: GUT, DEPTH: DEPTH, SHIFT_MAX: SHIFT_MAX, BIRD_PAD: BIRD_PAD, SP: SP, COATS: COATS, RIM: RIM, DAY_RIM: DAY_RIM, INK: INK,
    INKS: INKS, rimOf: rimOf, coatOf: coatOf, onLane: onLane, sides: sides, ring: ring, freeLines: freeLines, tourOf: tourOf, pointAt: pointAt, routeOf: routeOf, along: along, place: place, wander: wander, admit: admit, keepOuts: keepOuts, gate: gate, goTo: goTo, grazes: grazes,
    countOf: countOf, pxOf: pxOf, card: card, herderWords: herderWords, stoneCard: stoneCard, outcomeWord: outcomeWord, reasonWords: reasonWords, draw: draw, sign: sign, at: at, wire: wire,
    /* have the goats past MAX, or a project's open wishes, changed since the signs last said? (garden.js re-reads them every
       few seconds) */
    stale: function () { return sig() !== said; },
    onWake: function (fn) { wakeFn = fn; },
    /* test hooks (the goldens): pin the clock, walk under reduced motion, read what the last draw drew (css: its box on the
       board, CSS px) */
    pin: function (ms) { pinned = ms === undefined ? null : ms; },
    motion: function (on) { force = !!on; },
    list: function () { return drawn.map(function (d) { return { id: d.id, plot: d.plot, x: d.x, y: d.y, f: d.f, dir: d.dir, ghost: !d.g.tending, pen: !!d.g.pen, out: !!d.out, css: d.css }; }); }
  };
})();
if (typeof module !== "undefined") module.exports = GOATS;
