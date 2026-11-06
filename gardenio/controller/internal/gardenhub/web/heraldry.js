/* HERALDRY: a plan's arms on the World (heraldry spec gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md §6.3-6.4,
   step 4.1). The arms are drawn by the renderer (internal/heraldry), never here, as a row of their charges: the page asks
   /garden/arms.png for a PNG exactly slot x devicePixelRatio device px high (DeviceHeight) and as wide as the run needs, once
   per arms, height and theme, and art.js's drawArms blits it 1:1 onto the canvas's device pixels, so the picture is as sharp
   as the screen at any zoom. picture() hands back the loaded bitmap, or null while it loads or after it failed: the page draws
   nothing in the slot then, and onload (set once by the page) asks for a repaint when a bitmap arrives. A failed fetch is not
   retried until the page reloads, so a refused arms costs one request. gate() and drawPlot() place the picture on a plot, and aria()
   and hotAria() say it in words (step 4.3). Pure but for picture() and drawPlot(). */
var HERALDRY = (function () {
  "use strict";
  var SLOT = 24, MIN_H = 8, MAX_H = 256;   // the plot gate's CSS px (its height); the renderer's heights (render.go MinHeight, MaxHeight)
  var BADGE = 8;   // a gardener's badge, world px (5.9)
  /* The bitmap height for a CSS slot at a device pixel ratio: the slot times the ratio, rounded up, at least 1x, inside the
     renderer's range (heraldry.DeviceHeight). */
  function deviceHeight(slot, dpr) { return Math.max(MIN_H, Math.min(MAX_H, Math.ceil(slot * Math.max(dpr || 1, 1)))); }
  /* The picture's URL under base (the page's data-base): the arms as the farm carries them, the device height, the theme. */
  function src(base, arms, h, night) {
    return (base || "") + "/garden/arms.png?arms=" + encodeURIComponent(arms) + "&h=" + h + "&theme=" + (night ? "night" : "day");
  }
  var cache = {}, hooks = { onload: null, base: "" };
  /* The arms' bitmap for a slot at a ratio and theme: an HTMLImageElement deviceHeight high (and the row's width), or null
     until it has loaded (and for good once it failed, or when arms is empty or there is no DOM). The first call starts the
     fetch; later ones return the same element. */
  function picture(arms, slot, dpr, night) {
    if (!arms || typeof Image === "undefined") return null;
    var h = deviceHeight(slot || SLOT, dpr), key = (night ? "n" : "d") + h + "|" + arms, e = cache[key];
    if (!e) {
      var img = new Image();
      e = cache[key] = { img: img, ok: false };
      img.decoding = "async";
      img.onload = function () { e.ok = img.naturalWidth > 0; if (e.ok && hooks.onload) hooks.onload(); };
      img.onerror = function () { e.ok = false; };
      img.src = src(hooks.base, arms, h, night);
    }
    return e.ok ? e.img : null;
  }
  /* Where the plot gate's picture goes (step 4.3): its top-left in world px for a plot whose first bed starts at world px
     (left, top), at zoom z and ratio dpr. It hangs on the plot's top edge, the side with a two-tile path (fence and walk; the
     bottom has the fence alone, where the gardeners stand), just right of the first bed's stake and with its foot one screen px
     above that bed, so at any zoom it covers no bed and no stake. The picture is a fixed screen size, so the offset is in
     screen px. */
  function gate(left, top, z, dpr) {
    var d = Math.max(dpr || 1, 1), h = deviceHeight(SLOT, d) / d;
    return { x: left, y: top - (h + 1) / z };
  }
  /* The words a hot adds for arms (spec 8): " Arms: fire, compass, package.", or "" for
     none. The farm names them (heraldry_name), since the page has no library to name a charge with. */
  function aria(name) { return name ? " " + name.charAt(0).toUpperCase() + name.slice(1) + "." : ""; }
  /* A plan's arms at its plot gate (spec 6.3, step 4.3), for the page's plot pass at the Plan and Bead depths: p is a WORLD plot
     (tiles of T world px), off its first bed's offset in tiles (WORLD.BED.off[0]), draw ART.drawArms. At World and Project the
     rollup stamp has no room for it and the page asks only for the plot the cursor is inside (CURSOR.gatePlot). Arms the farm could not name (no heraldry_name) are arms the
     renderer refuses: nothing is fetched. No phase stake, task cell or stamp draws arms (Q16, R6). Returns drawArms' box or
     null. */
  function drawPlot(ctx, draw, p, T, off, z, dpr, night) {
    var pl = p.plan || {};
    if (!pl.heraldry || !pl.heraldry_name) return null;
    var g = gate((p.x + off) * T, (p.y + off) * T, z, dpr);
    return draw(ctx, picture(pl.heraldry, SLOT, dpr, night), g.x, g.y);
  }
  /* The screen box (CSS px) drawPlot's picture covers for plot p at camera cam ({cx, cy, z}) in view vp ({w, h, dpr}), or null
     while it has no picture: what the beacons keep off (gd-rA31ZIfHdv). */
  function gateRect(p, T, off, cam, vp, night) {
    var pl = p.plan || {}, img = pl.heraldry && pl.heraldry_name ? picture(pl.heraldry, SLOT, vp.dpr, night) : null;
    if (!img) return null;
    var d = Math.max(vp.dpr || 1, 1), g = gate((p.x + off) * T, (p.y + off) * T, cam.z, vp.dpr);
    return { x: (g.x - cam.cx) * cam.z + vp.w / 2, y: (g.y - cam.cy) * cam.z + vp.h / 2, w: img.naturalWidth / d, h: img.naturalHeight / d };
  }
  /* A gardener's task arms over the head of the figure at (x, y), higher with lift, above any plot gate in keep (device px
     boxes from drawArms), then moved across up to its width, up to its height or down 4 to where the cost is least: twice the
     world px it moved plus each world px it covers of a weighed keep box (k.c: a bed's sign, a bed, a figure, a badge drawn
     before it), so it clears them where it can (gd-M0IjVGTzSP). Its box joins keep. */
  function badge(ctx, draw, arms, x, y, z, dpr, night, lift, keep) {
    if (!arms) return null;
    var d = Math.max(dpr || 1, 1), s = (z || 1) * d, img = picture(arms, BADGE * (z || 1), d, night);
    if (!img) return null;
    var W = img.naturalWidth, H = img.naturalHeight, w = W / s, h = H / s, bx = x + 8 - w / 2, by = y - 2 - h - (lift ? h + 1 : 0), m = ctx.getTransform && ctx.getTransform();
    if (m && keep) {
      keep.forEach(function (k) {
        var dx = m.a * bx + m.e, dy = m.d * by + m.f;
        if (!k.c && dx < k.x + k.w && dx + W > k.x && dy < k.y + k.h && dy + H > k.y) by = (k.y - 1 - H - m.f) / m.d;
      });
      var ox = m.a * bx + m.e, oy = m.d * by + m.f, best = Infinity, at = [0, 0], near = keep.filter(function (k) {
        return k.c && k.x < ox + 2 * W && k.x + k.w > ox - W && k.y < oy + H + 3 * m.d && k.y + k.h > oy - H;
      });
      for (var v = -4; v <= h; v++) for (var i = 0; i <= 2 * w; i++) {
        var u = i % 2 ? (i + 1) / 2 : -i / 2, c = 2 * (Math.abs(u) + Math.abs(v)), px = ox + m.a * u, py = oy - m.d * v;
        near.forEach(function (k) {
          var ix = Math.min(px + W, k.x + k.w) - Math.max(px, k.x), iy = Math.min(py + H, k.y + k.h) - Math.max(py, k.y);
          if (ix > 0 && iy > 0) c += ix * iy * k.c / (m.a * m.d);
        });
        if (c < best) { best = c; at = [u, v]; }
      }
      bx += at[0]; by -= at[1];
    }
    var b = draw(ctx, img, bx, by);
    if (b && keep) keep.push({ x: b.x, y: b.y, w: b.w, h: b.h, c: 2 });
    return b;
  }
  /* Adds the world px box (x, y, w, h) to keep in device px, each px weighing c to a badge (5.9). */
  function shun(ctx, keep, x, y, w, h, c) {
    var m = ctx.getTransform && ctx.getTransform();
    if (m) keep.push({ x: m.a * x + m.e, y: m.d * y + m.f, w: m.a * w, h: m.d * h, c: c });
  }
  /* The arms a world hot names, in words after the rest of its name (spec 8): a plot's (and its Bead-depth sign's) are its
     plan's, the picture at its gate; a phase bed's, its stake's, are the phase's, which no picture shows (Q16); a gardener's
     are its task's line's (5.9). Tasks bear none (R6), and nothing else carries arms. */
  function hotAria(h) {
    var k = h && h.kind;
    return aria(k === "plot" || k === "plotsign" ? h.plot && h.plot.plan && h.plot.plan.heraldry_name : k === "bed" && h.node && h.node.ref ? h.node.ref.arms
      : (k === "gardener" || k === "gask") && h.g && h.g.task_arms ? h.g.task_arms.name : "");
  }
  /* The page wires the repaint and its base path once (garden.js). */
  function init(base, onload) { hooks.base = base || ""; hooks.onload = onload || null; }
  return { SLOT: SLOT, BADGE: BADGE, deviceHeight: deviceHeight, src: src, picture: picture, gate: gate, aria: aria, drawPlot: drawPlot, gateRect: gateRect, badge: badge, shun: shun, hotAria: hotAria, init: init };
})();
if (typeof module !== "undefined") module.exports = HERALDRY;
