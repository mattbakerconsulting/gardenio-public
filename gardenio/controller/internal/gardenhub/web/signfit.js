/* SIGNFIT: a tip kept on the screen (gd-O3nwaqbNUG). fit() takes the box a tip would ride to, its Thing's box and the area it
   may use (area(): the map's insets or 16 px, whichever is more, on every side), all in screen px, and returns where to draw
   it: on its own side of the Thing while that side is on the screen, else on the opposite side when that one is, then pushed
   inside the area (a tip wider than the area may use the map's whole width less the gutter, top-left first when wider still),
   then to the nearest spot above, below or beside one of keep (the camera buttons, which stand over only part of an edge, and
   the beacons' arrows) that clears them all, when it would sit under one (where none does it stays: a tip is never dropped),
   with tick, its pointer's offset along the edge facing the Thing, kept at the Thing's middle once a push moved it, or when
   the tip was placed off the Thing's middle. The steward's tip (web/stip.js) and a gardener's card (web/wall.js) are placed by
   it. Pure but for keep(). */
var SIGNFIT = (function () {
  "use strict";
  var GUT = 16, GAP = 4, TICK = 6, OPP = { above: "below", below: "above", left: "right", right: "left" };
  function area(v) {
    var l = Math.max(GUT, v.left || 0), t = Math.max(GUT, v.top || 0);
    return { x: l, y: t, w: v.w - l - Math.max(GUT, v.right || 0), h: v.h - t - Math.max(GUT, v.bottom || 0), vw: v.w };
  }
  function hit(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }
  /* Whether box b leaves area a on the side slot sl faces (an "inside" or docked label faces none). */
  function over(b, a, sl) {
    return sl === "above" ? b.y < a.y : sl === "below" ? b.y + b.h > a.y + a.h : sl === "left" ? b.x < a.x : sl === "right" ? b.x + b.w > a.x + a.w : false;
  }
  function fit(b, th, slot, a, gap, keep) {
    var g = gap === undefined ? GAP : gap, x = b.x, y = b.y, sl = slot, o = OPP[slot];
    if (o && over(b, a, slot)) {
      var f = { x: o === "right" ? th.x + th.w + g : o === "left" ? th.x - g - b.w : x, y: o === "below" ? th.y + th.h + g : o === "above" ? th.y - g - b.h : y, w: b.w, h: b.h };
      if (!over(f, a, o)) { x = f.x; y = f.y; sl = o; }
    }
    var ax = a.x, aw = a.w;
    if (b.w > aw && a.vw) { ax = GUT; aw = a.vw - 2 * GUT; }
    var clamp = function (v, lo, hi) { return Math.max(Math.ceil(lo), Math.min(Math.floor(hi), Math.round(v))); };   // whole px, never past an edge
    var cx = clamp(x, ax, ax + aw - b.w), cy = clamp(y, a.y, a.y + a.h - b.h), tick = null;
    var ks = keep || [], clear = function (p) { return !ks.some(function (r) { return hit({ x: p[0], y: p[1], w: b.w, h: b.h }, r); }); };
    if (!clear([cx, cy])) {   // the nearest spot beside one of them, inside the area and clear of all, else where it is (never dropped)
      var best = null, d = Infinity;
      ks.forEach(function (r) {
        [[cx, Math.floor(r.y - GAP - b.h)], [cx, Math.ceil(r.y + r.h + GAP)], [Math.floor(r.x - GAP - b.w), cy], [Math.ceil(r.x + r.w + GAP), cy]].forEach(function (p) {
          var n = Math.abs(p[0] - cx) + Math.abs(p[1] - cy);
          if (n < d && p[0] >= ax && p[0] + b.w <= ax + aw && p[1] >= a.y && p[1] + b.h <= a.y + a.h && clear(p)) { best = p; d = n; }
        });
      });
      if (!best) {   // none along one axis: a candidate x with a candidate y (an arrow on each of two edges, gd-wAN7P4Armw)
        var xs = [cx], ys = [cy];
        ks.forEach(function (r) { xs.push(Math.floor(r.x - GAP - b.w), Math.ceil(r.x + r.w + GAP)); ys.push(Math.floor(r.y - GAP - b.h), Math.ceil(r.y + r.h + GAP)); });
        xs.forEach(function (px) { if (px < ax || px + b.w > ax + aw) return; ys.forEach(function (py) {
          var n = Math.abs(px - cx) + Math.abs(py - cy);
          if (n < d && py >= a.y && py + b.h <= a.y + a.h && clear([px, py])) { best = [px, py]; d = n; }
        }); });
      }
      if (best) { cx = best[0]; cy = best[1]; }
    }
    var at = function (v, len) { return Math.round(Math.max(TICK, Math.min(len - TICK, v))); };
    var off = function (v, len) { return Math.abs(v - len / 2) > 2 && v >= TICK && v <= len - TICK; };   // placed off the Thing's middle, which it still spans
    if ((sl === "above" || sl === "below") && (cx !== Math.round(x) || off(th.x + th.w / 2 - cx, b.w))) tick = at(th.x + th.w / 2 - cx, b.w);
    else if ((sl === "left" || sl === "right") && cy !== Math.round(y)) tick = at(th.y + th.h / 2 - cy, b.h);
    return { x: cx, y: cy, slot: sl, tick: tick };
  }
  /* The camera buttons (on a phone a column on the right edge, over only part of it), or the controls sel picks, in the board's px. */
  function keep(sel) {
    var o = document.getElementById("board").getBoundingClientRect(), out = [];
    Array.prototype.forEach.call(document.querySelectorAll(sel || ".cam .cb"), function (c) { var r = c.getBoundingClientRect(); if (r.width && r.height) out.push({ x: r.left - o.left, y: r.top - o.top, w: r.width, h: r.height }); });
    return out;
  }
  /* The beacons' arrows ({x, y, size} centres, garden.js bcnLayout) as boxes with their 2 px ring. */
  function rings(bcns) { return (bcns || []).map(function (g) { var h = g.size / 2 + 2; return { x: g.x - h, y: g.y - h, w: 2 * h, h: 2 * h }; }); }
  return { GUT: GUT, area: area, hit: hit, fit: fit, keep: keep, rings: rings };
})();
if (typeof module !== "undefined") module.exports = SIGNFIT;
