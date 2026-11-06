/* WORLD: the pure (no DOM) half of the Homestead world, shared by the camera, biome and test steps
   (gardenio/docs/homestead-world-design.md). Everything here is plain data in, plain data out, so Go tests run it under node.
   Units: a tile is 16 world px; the camera is {cx, cy, z} with the centre in world px and z in CSS px per world px;
   x runs east and y runs south. Rects are {x, y, w, h} in world px. */
var WORLD = (function () {
  "use strict";
  var R = typeof REASON !== "undefined" ? REASON : require("./reason.js");   // a gardener's reason, drawn (web/reason.js)
  var ACT = typeof ACTIVITY !== "undefined" ? ACTIVITY : require("./activity.js");   // what a gardener is doing (web/activity.js)
  var ESC = typeof ESCKIND !== "undefined" ? ESCKIND : require("./esckind.js");   // an escalation's order and words (web/esckind.js)
  var TILE = 16;
  var DOM = { w: 176, h: 144 };                       // one project domain, in tiles (section 3.1)
  var Z_MIN = 0.04, Z_MAX = 4;                        // section 2
  var SNAPS = [1, 1.5, 2, 3, 4];                      // resting zooms at or above 1, so pixel art stays crisp
  var EDGES = [0.11, 0.41, 0.92];                     // level boundaries in z (section 5): the finer level from each edge up
  var LEVEL_NAMES = ["World", "Project", "Plan", "Bead"];
  var CHUNK_PX = 512;                                 // a native chunk: 512x512 world px (Plan, Bead levels)

  /* FNV-1a plus a murmur3 finaliser (section 3.2). Layout only; art.js hashStr stays bare FNV-1a for sprite picks. */
  function h32(s, seed) {
    var h = (2166136261 ^ seed) >>> 0;
    for (var i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619) >>> 0; }
    h ^= h >>> 16; h = Math.imul(h, 0x85ebca6b) >>> 0; h ^= h >>> 13;
    h = Math.imul(h, 0xc2b2ae35) >>> 0; h ^= h >>> 16;
    return h >>> 0;
  }

  /* ---------- zoom levels ---------- */
  function rawLevel(z) { return z < EDGES[0] ? 0 : z < EDGES[1] ? 1 : z < EDGES[2] ? 2 : 3; }
  /* The level for zoom z: a pure function of z, the same whichever way the camera arrived (gd-bwI1PZ6jnM). It once kept the
     previous level inside an 8% band about each edge, against flicker; that showed a plan's phases as the Project depth's
     rollup boxes when zooming in and as the Plan depth's beds when zooming out to the very same zoom, so the band is gone and
     each edge sits where the finer level used to take over on the way out. prev is accepted and ignored, for the callers. */
  function lodFor(z, prev) { return rawLevel(z); }
  /* [lo, hi) of z for a level, pulled in from the edges so a programmatic move lands clear of them. */
  function levelRange(level) {
    var lo = level === 0 ? Z_MIN : EDGES[level - 1] * 1.09, hi = level === 3 ? Z_MAX : EDGES[level] * 0.91;
    return [lo, hi];
  }

  function clampZ(z) { return z < Z_MIN ? Z_MIN : z > Z_MAX ? Z_MAX : z; }
  /* Nearest resting zoom at or above 1 (pixel snap); below 1 smoothing is on, so any value rests. */
  function snapZ(z) {
    if (z < 1) return clampZ(z);
    var best = SNAPS[0];
    for (var i = 1; i < SNAPS.length; i++) if (Math.abs(Math.log(z / SNAPS[i])) < Math.abs(Math.log(z / best))) best = SNAPS[i];
    return best;
  }
  /* The resting zoom one step from z in direction dir (+1 in, -1 out), for a gesture that moved off a resting level but
     not far enough for the nearest one to differ: it rests one level on instead of springing back (gd-iYlxddKzuu). */
  function snapNext(z, dir) {
    var i = SNAPS.indexOf(snapZ(z)) + (dir > 0 ? 1 : -1);
    return SNAPS[i < 0 ? 0 : i >= SNAPS.length ? SNAPS.length - 1 : i];
  }

  /* ---------- camera ---------- */
  function toWorld(cam, vp, sx, sy) { return { x: cam.cx + (sx - vp.w / 2) / cam.z, y: cam.cy + (sy - vp.h / 2) / cam.z }; }
  function toScreen(cam, vp, wx, wy) { return { x: (wx - cam.cx) * cam.z + vp.w / 2, y: (wy - cam.cy) * cam.z + vp.h / 2 }; }
  /* The world rect on screen, grown by pad (0.25 = 25% of each side) for culling. */
  function viewRect(cam, vp, pad) {
    var w = vp.w / cam.z, h = vp.h / cam.z, px = w * (pad || 0), py = h * (pad || 0);
    return { x: cam.cx - w / 2 - px, y: cam.cy - h / 2 - py, w: w + 2 * px, h: h + 2 * py };
  }
  /* Keep the centre inside the world bbox plus one domain of slack on every side, so the map cannot be lost. */
  function clampCam(cam, bbox) {
    var out = { cx: cam.cx, cy: cam.cy, z: clampZ(cam.z) };
    if (bbox) {
      var sx = DOM.w * TILE, sy = DOM.h * TILE;
      out.cx = Math.max(bbox.x - sx, Math.min(bbox.x + bbox.w + sx, out.cx));
      out.cy = Math.max(bbox.y - sy, Math.min(bbox.y + bbox.h + sy, out.cy));
    }
    return out;
  }
  /* Zoom by a factor keeping the world point under screen (sx, sy) fixed. */
  function zoomAt(cam, factor, sx, sy, vp, bbox) {
    var z = clampZ(cam.z * factor), w = toWorld(cam, vp, sx, sy);
    return clampCam({ cx: w.x - (sx - vp.w / 2) / z, cy: w.y - (sy - vp.h / 2) / z, z: z }, bbox);
  }
  /* Drag the world by (dx, dy) screen px. */
  function panBy(cam, dx, dy, bbox) { return clampCam({ cx: cam.cx - dx / cam.z, cy: cam.cy - dy / cam.z, z: cam.z }, bbox); }
  /* Frame a world rect in the viewport. opts: pad (fraction kept as margin), level (clamp z into that level's range),
     min/max z, top (screen px at the top kept clear, as of the camera buttons: the rect is framed in the viewport below it).
     Used by every "fly to" so keys, clicks and links agree. */
  function fit(bbox, vp, opts) {
    opts = opts || {};
    var top = opts.top || 0, pad = opts.pad || 0, z = Math.min(vp.w / (bbox.w * (1 + pad * 2)), (vp.h - top) / (bbox.h * (1 + pad * 2)));
    var lo = opts.min || Z_MIN, hi = opts.max || Z_MAX;
    if (opts.level !== undefined) { var r = levelRange(opts.level); lo = Math.max(lo, r[0]); hi = Math.min(hi, r[1]); }
    if (!(z > 0) || !isFinite(z)) z = 1;
    z = Math.max(lo, Math.min(hi, z));
    return { cx: bbox.x + bbox.w / 2, cy: bbox.y + bbox.h / 2 - top / 2 / z, z: z };
  }

  /* ---------- culling and chunks ---------- */
  function hit(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }
  /* Items whose rect meets the view rect. Plain rectangle tests are enough at the counts the design budgets for. */
  function cull(items, view) { return items.filter(function (it) { return hit(it, view); }); }
  /* The `cap` items nearest (centre to centre) to (cx, cy), in their original order. Used for the DOM cap. */
  function nearest(items, cx, cy, cap) {
    if (items.length <= cap) return items;
    var d = items.map(function (it, i) { var dx = it.x + it.w / 2 - cx, dy = it.y + it.h / 2 - cy; return { i: i, d: dx * dx + dy * dy }; });
    d.sort(function (a, b) { return a.d - b.d || a.i - b.i; });
    var keep = {};
    for (var i = 0; i < cap; i++) keep[d[i].i] = 1;
    return items.filter(function (_, i) { return keep[i]; });
  }
  /* World px a chunk covers at a mip (scene.js bakes mip 1 only). */
  function chunkSpan(mip) { return CHUNK_PX * mip; }
  /* Chunk coordinates meeting the view rect at a mip, row-major. */
  function chunks(view, mip) {
    var s = chunkSpan(mip), x0 = Math.floor(view.x / s), x1 = Math.floor((view.x + view.w) / s);
    var y0 = Math.floor(view.y / s), y1 = Math.floor((view.y + view.h) / s), out = [];
    for (var j = y0; j <= y1; j++) for (var i = x0; i <= x1; i++) out.push({ i: i, j: j });
    return out;
  }

  /* A small LRU map: get() refreshes, set() evicts the oldest past max. */
  function lru(max) {
    var m = new Map();
    return {
      get: function (k) { if (!m.has(k)) return undefined; var v = m.get(k); m.delete(k); m.set(k, v); return v; },
      set: function (k, v) { if (m.has(k)) m.delete(k); m.set(k, v); while (m.size > max) m.delete(m.keys().next().value); },
      del: function (k) { m.delete(k); },
      keys: function () { return Array.from(m.keys()); },
      clear: function () { m.clear(); },
      get size() { return m.size; }
    };
  }

  /* ---------- the world rectangle ---------- */
  /* Cell 0's domain (0,0) as a world rect: tiles x -88..87, y -72..71. Biome code widens this to the occupied domains. */
  function domainRect(i, j) { return { x: (DOM.w * i - DOM.w / 2) * TILE, y: (DOM.h * j - DOM.h / 2) * TILE, w: DOM.w * TILE, h: DOM.h * TILE }; }

  /* ---------- deterministic layout (cursor-grid spec 5, gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md):
     a row-major lattice of regions, one per project; square plots packed into spiral slots; a 3x3 of 9x9 phase beds per plot ---------- */
  var SLOT = { w: 32, h: 32, dx: 35, dy: 35, cols: 7, rows: 5 };     // a plot and its pitch, in tiles (spec 5.2, N-D10): every gap a lane's width (29.3)
  var SLOT_CAP = SLOT.cols * SLOT.rows - 1;                          // 34 plots per biome: slot 0 is the farmstead (29.1)
  var LANE = SLOT.dx - SLOT.w;                                       // a lane's width, in tiles: the gap it runs in (29.3)
  var BAND = { orchardGap: 3, nurseryH: 10, potPitch: 4, potH: 5, treePitch: 2, stonePitch: 2, graveGap: 2 };
  var CAPS = { orchard: 24, nursery: 40, graveyard: 24 };   // graveyard: the farm's maxClosedWishesPerProject (farm_wishes.go)
  var OVERFLOW = { w: 16, h: 12 };                                   // the "+N more plans" crate stack, in its own row under the Nursery
  var MEMO_DAYS = 30, MEMO_CELLS = 256, MEMO_SLOTS = 4096;
  /* The world lattice (spec 5.1, N-D8; homestead-world-design.md 28.5): LATTICE_W cells wide whatever the screen; the Commons is
     cell 0 at the world origin (no room while empty), then projects oldest first. Each column is as wide as its widest region and
     each row as tall as its tallest, GAP tiles of grass between them; a region sits centred in its cell by its base width (29.11). */
  var LATTICE_W = 5, GAP = 16, COMMONS_K = 0, FIRST_K = 1;
  /* The farmstead (29.2, 29.10, 29.11): slot 0, 32 tall and homeW wide, an east-west lane arm on its row `cross` and a north and a
     south arm whose columns move with the cells' widths, so four quarters, the north-east one the Escalations and the Mailbox a
     path apart (28.13).
     cells: [zone, min, max, ladder] in the zone order the cursor and the outline read (28.6); a cell's width is a rung of its
     ladder, the widths from min to max that each add a slot column (ladderOf), sized from its count (homeWidths). The Barn has no
     ladder: it takes what the asks leave in the north half, at least its min, the empty farm's 22. step: the farm widens in steps of
     4 tiles, a multiple of every band pitch, so a band item keeps its cell (X-D14). slack: how many items past its count a cell must
     hold before it shrinks (29.10's hysteresis). bay: the truck's box under the Barn's keep, its west side the farm's west edge
     (28.10); keep: the tiles at a cell's north-west corner its pictures stand on (the mailbox and its dog, the barn:
     ART.drawHome), which no slot meets; sign: the name plate on the farm's north edge at fx + 12 at every width, which no slot meets either;
     pitch: a zone's slot pitch, sprite height and slot width, px (the Growing field's slots are pairs, a plant and a
     figure side by side, or two loose beads; its rows 32 px apart, as close as a figure's task arms on its hat clear the
     feet above, gd-7UDNpFoHWw); door: the middle of the barn's double door at ground level, px off the Barn keep's corner
     (ART.BARN_DOOR off the barn, drawn 8 px in). MEADOW_SHARE: the share of the field's slots the loose beads may take, so a gardener
     always has somewhere to stand (28.12). */
  var HOME = { cross: 15, slack: 2, step: 4, cells: [["mailbox", 5, 9], ["escalation", 3, 11], ["growing", 8, 22], ["barn", 22, 22], ["pen", 8, 23]],
    bay: { w: 6, h: 4 }, keep: { mailbox: { w: 4, h: 3 }, barn: { w: 7, h: 5 } }, sign: { w: 8, h: 2 },
    pitch: { mailbox: [17, 19, 16], escalation: [17, 19, 16], growing: [32, 32, 25, 32], barn: [16, 26, 25] }, door: [56, 72] };
  var MEADOW_SHARE = 0.5;
  var PEN = { dx: 3, dy: 2 };
  /* colsAt(zone, w) -> the slot columns a cell w tiles wide holds: penOf's for the Pen, slotsOf's for the rest. Each HOME.cells
     entry but the Barn's gains its ladder: the widths from its min to its max that each add a column, so a cell changes only
     when a column fills or empties (29.10). */
  function colsAt(zone, w) { if (zone === "pen") return Math.floor((w - 2) / PEN.dx); var p = HOME.pitch[zone]; return Math.floor(((w - 2) * TILE - (p[3] || TILE)) / p[0]) + 1; }
  HOME.cells.forEach(function (c) { if (c[0] === "barn") return; var l = [c[1]]; for (var w = c[1] + 1; w <= c[2]; w++) if (colsAt(c[0], w) > colsAt(c[0], l[l.length - 1])) l.push(w); c.push(l); });

  /* Slot k -> {c, r} on the 7x5 grid: a square spiral from the centre, slot 0 the farmstead (29.1); ring 1 clockwise from the
     east, so the first plot is beside the farm (X-D2), the outer rings clockwise from their top-left, clipped to the grid. Slot k
     is at the same place for every N, so a biome fills outward and never re-flows. */
  var SPIRAL = (function () {
    var out = [{ c: 0, r: 0 }, { c: 1, r: 0 }, { c: 1, r: 1 }, { c: 0, r: 1 }, { c: -1, r: 1 }, { c: -1, r: 0 }, { c: -1, r: -1 }, { c: 0, r: -1 }, { c: 1, r: -1 }], R, c, r, cx = (SLOT.cols - 1) / 2, cy = (SLOT.rows - 1) / 2;
    var push = function (c, r) { if (Math.abs(c) <= cx && Math.abs(r) <= cy) out.push({ c: c, r: r }); };
    for (R = 2; R <= Math.max(cx, cy); R++) {
      for (c = -R; c < R; c++) push(c, -R);
      for (r = -R; r < R; r++) push(R, r);
      for (c = R; c > -R; c--) push(c, R);
      for (r = R; r > -R; r--) push(-R, r);
    }
    return out;
  })();
  /* ---------- grid coordinates (the cursor's {c, r}, 0-based, c east and r south), each with its inverse ---------- */
  /* Lattice cell k <-> {c, r}. */
  function latticeAt(k) { return { c: k % LATTICE_W, r: Math.floor(k / LATTICE_W) }; }
  function latticeIndex(c, r) { return r * LATTICE_W + c; }
  /* Spiral slot s <-> {c, r} on the 7x5 plot grid; slot 0 is the centre, (3, 2). slotIndex is -1 off the grid. */
  function slotAt(s) { var p = SPIRAL[s]; return { c: p.c + (SLOT.cols - 1) / 2, r: p.r + (SLOT.rows - 1) / 2 }; }
  function slotIndex(c, r) {
    for (var s = 0; s < SPIRAL.length; s++) if (SPIRAL[s].c + (SLOT.cols - 1) / 2 === c && SPIRAL[s].r + (SLOT.rows - 1) / 2 === r) return s;
    return -1;
  }
  /* A plot's beds (spec 5.3): position n (0..8, the phase ordinal less one) <-> {c, r} on the 3x3, in reading order. */
  var BED = { size: 9, block: 3, off: [2, 12, 22], cap: 81, phases: 9 };
  function bedAt(n) { return { c: n % 3, r: Math.floor(n / 3) }; }
  /* A bed's cells (spec 5.4, N-D11): block-major index i (0..80) <-> {c, r} on the 9x9. Block 1 (top-left 3x3) holds 0-8 row-major,
     block 2 (top-middle) 9-17, and so on, so a short phase is a tight cluster in the top-left. */
  function cellAt(i) { var b = Math.floor(i / 9), j = i % 9; return { c: (b % 3) * 3 + j % 3, r: Math.floor(b / 3) * 3 + Math.floor(j / 3) }; }
  function cellIndex(c, r) { return (Math.floor(r / 3) * 3 + Math.floor(c / 3)) * 9 + (r % 3) * 3 + c % 3; }

  /* Slot k of a biome centred at c = {x, y} tiles whose farm has grown g tiles (29.11): the farm is g wider and the east column of
     plots g further east; nothing else moves. */
  function slotRect(c, k, g) {
    var s = SPIRAL[k], d = g | 0;
    return { x: c.x + s.c * SLOT.dx - SLOT.w / 2 + (s.c > 0 ? d : 0), y: c.y + s.r * SLOT.dy - SLOT.h / 2, w: SLOT.w + (k ? 0 : d), h: SLOT.h };
  }
  /* The rectangles a biome's shape implies for the farm's centre (cx, cy), its plot grid (fit's grid: c0, r0, cols, rows on the
     7x5, the box of the farm, slot 1 and every used slot, 29.1), whether it carries an overflow marker and how many headstones its
     graveyard holds: the band rects, the farmstead's rect (farm: slot 0, a plot and not a band; its cells are layout()'s, 29.10)
     and its sign, the plot area (the grid's slots and the lane frame round the farm, 29.3), the used rect (everything placed),
     the foot (used padded by the terrain fringe's maximum, section 3.3) and the lanes across the foot. Pure in its inputs, so a
     region can be sized before it is placed and no count moves a rect but through grid.g (H-D4, 29.11). A region is sized to its content: nothing is
     reserved for growth (spec 5.1, N-D9 promises coordinates only). The graveyard (gd-s9KzRk6boh) is rows of stones at pitch 2
     between the Orchard and the plots, in the columns no lane crosses (X-D6), pushing the Orchard up; with no stones it takes
     no room. A farm grown by grid.g (29.11) slides the biome: it is the g = 0 shape cut at the farm's base east edge (cut), every
     rect east of the cut g further east and every rect across it g wider; base keeps the g = 0 band's bx, bw and laneX, which
     layout() places the band items on before it slides them. */
  function shape(cx, cy, grid, overflow, graves) {
    var mc = (SLOT.cols - 1) / 2, mr = (SLOT.rows - 1) / 2, farm = slotRect({ x: cx, y: cy }, 0), lx = [farm.x - LANE, farm.x + farm.w];
    var box = { x: cx + (grid.c0 - mc) * SLOT.dx - SLOT.w / 2, y: cy + (grid.r0 - mr) * SLOT.dy - SLOT.h / 2, w: (grid.cols - 1) * SLOT.dx + SLOT.w, h: (grid.rows - 1) * SLOT.dy + SLOT.h };
    var area = union(box, { x: farm.x - LANE, y: farm.y - LANE, w: farm.w + 2 * LANE, h: farm.h + 2 * LANE }), bx = area.x, bw = area.w, top = area.y, bottom = area.y + area.h;
    var sn = Math.floor(bw / BAND.stonePitch), sl = laneCells(bx, sn, BAND.stonePitch, 1, lx), sat = [];
    for (var k = 0; k < sn; k++) if (!sl[k]) sat.push(k);
    var graveH = graves > 0 ? Math.ceil(graves / sat.length) * BAND.stonePitch - 1 : 0, graveY = top - BAND.orchardGap - graveH;
    var orchardY = graveH ? graveY - BAND.graveGap - 1 : top - BAND.orchardGap - 1, nurseryY = bottom + 1;
    var g = grid.g | 0, cut = lx[1], mv = function (r) { if (r.x >= cut) r.x += g; else if (r.x + r.w > cut) r.w += g; return r; };
    var s = { bx: bx, bw: bw + g, top: top, bottom: bottom, farm: slotRect({ x: cx, y: cy }, 0, g), sign: signOf(farm), overflow: null, laneX: [lx[0], lx[1] + g], cut: cut, g: g,
      base: { bx: bx, bw: bw, laneX: lx }, bands: { orchard: mv({ x: bx, y: orchardY, w: bw, h: 1 }), nursery: mv({ x: bx, y: nurseryY, w: bw, h: BAND.nurseryH }) }, plotArea: mv(area) };
    if (graveH) s.bands.graveyard = mv({ x: bx, y: graveY, w: bw, h: graveH, cols: sat.length, at: sat });
    if (overflow) s.overflow = { x: bx + bw - OVERFLOW.w + g, y: nurseryY + BAND.nurseryH + 1, w: OVERFLOW.w, h: OVERFLOW.h };
    var used = union(union(area, s.bands.orchard), s.bands.nursery);
    if (graveH) used = union(used, s.bands.graveyard);
    if (s.overflow) used = union(used, s.overflow);
    var f = s.foot = { x: used.x - 5, y: used.y - 5, w: used.w + 10, h: used.h + 10 };
    s.used = used;
    s.lanes = [{ name: "west", x: lx[0], y: f.y, w: LANE, h: f.h }, { name: "east", x: lx[1] + g, y: f.y, w: LANE, h: f.h },
      { name: "north", x: f.x, y: farm.y - LANE, w: f.w, h: LANE }, { name: "south", x: f.x, y: farm.y + farm.h, w: f.w, h: LANE }];
    return s;
  }
  /* laneCells(bx, n, p, w, xs) -> {k: 1} for each of a band row's n cells at pitch p from bx whose w tiles from the cell's west edge
     meet a north-south lane (its west column one of xs): the cells no band item stands on (29.3, X-D6). */
  function laneCells(bx, n, p, w, xs) {
    var out = {};
    for (var k = 0; k < n; k++) { var x = bx + k * p; if (xs.some(function (l) { return x < l + LANE && l < x + w; })) out[k] = 1; }
    return out;
  }
  /* The region a biome takes: its foot, as an offset from its farm's centre. */
  function regionSize(grid, overflow, graves) {
    var f = shape(0, 0, grid, overflow, graves).foot;
    return { ox: f.x, oy: f.y, w: f.w, h: f.h };
  }
  function tiles(r) { return { x: r.x * TILE, y: r.y * TILE, w: r.w * TILE, h: r.h * TILE }; }
  function union(a, b) {
    if (!a) return { x: b.x, y: b.y, w: b.w, h: b.h };
    var x = Math.min(a.x, b.x), y = Math.min(a.y, b.y);
    return { x: x, y: y, w: Math.max(a.x + a.w, b.x + b.w) - x, h: Math.max(a.y + a.h, b.y + b.h) - y };
  }
  function projectKey(p) { return String(p || "").trim().toLowerCase(); }
  /* farmstead(r, wd) -> the farmstead in slot 0's rect r (29.2, 29.10): {x, y, w, h, cells, keep, bay, sign, cross}. wd holds the
     widths of the escalation, mailbox, pen and growing cells (homeWidths'; a missing one is its min); the Barn takes the rest of
     the north half. cross is {xn, xs, y}: the north arm's column, the south arm's and the east-west arm's row. Only lines inside
     the farm move with wd: the farm, its keeps' corners, its bay and its sign are r's alone (X-D13); r.w is homeW(wd) (29.11). */
  function minsOf(wd) { var mn = {}; HOME.cells.forEach(function (c) { mn[c[0]] = (wd && wd[c[0]]) || c[1]; }); return mn; }
  function farmstead(r, wd) {
    var cells = {}, keep = {}, mn = minsOf(wd), y = HOME.cross, sh = r.h - y - 1;
    var E = mn.escalation, M = mn.mailbox, P = mn.pen, F = mn.growing, B = r.w - 2 - E - M;
    var at = { barn: [0, 0, B, y], escalation: [B + 1, 0, E, y], mailbox: [r.w - M, 0, M, y], pen: [0, y + 1, P, sh], growing: [r.w - F, y + 1, F, sh] };
    HOME.cells.forEach(function (c) { var a = at[c[0]]; cells[c[0]] = { x: r.x + a[0], y: r.y + a[1], w: a[2], h: a[3] }; });
    Object.keys(HOME.keep).forEach(function (z) { keep[z] = { x: cells[z].x, y: cells[z].y, w: HOME.keep[z].w, h: HOME.keep[z].h }; });
    var bk = keep.barn;
    return { x: r.x, y: r.y, w: r.w, h: r.h, cells: cells, keep: keep, bay: { x: bk.x, y: bk.y + bk.h, w: HOME.bay.w, h: HOME.bay.h },
      sign: signOf(r), cross: { xn: r.x + B, xs: r.x + r.w - 1 - F, y: r.y + y } };
  }
  function signOf(r) { var sg = HOME.sign; return { x: r.x + (SLOT.w - sg.w) / 2, y: r.y, w: sg.w, h: sg.h }; }   // fixed at fx + 12: a count never moves the name (29.11)
  /* homeW(wd) -> the farm's width for its cells' widths wd (29.11, X-D14): what the wider half needs (the north half the Barn's
     min, its arm, the asks' path and the asks; the south half the Pen, its arm and the field), past 32 rounded up to HOME.step. */
  function homeW(wd) {
    var mn = minsOf(wd), need = Math.max(mn.barn + 2 + mn.escalation + mn.mailbox, mn.pen + 1 + mn.growing);
    return SLOT.w + HOME.step * Math.ceil(Math.max(0, need - SLOT.w) / HOME.step);
  }
  /* homeWidths(n, prev) -> {escalation, mailbox, pen, growing}: each cell's width for its count n[zone] (29.10), prev the widths the
     farm had (the page's last world; null on a first layout). A cell grows at once to the narrowest rung of its ladder whose
     capacity, slotsOf at its real place (penOf's for the Pen), holds n; it shrinks only to the narrowest that holds n + HOME.slack,
     and only when that is narrower than prev. The Mailbox is sized first, since its width places the Escalations. Past its max a
     cell stays at its max. Each cell has its own width: the farm widens to hold them (homeW), so none is squeezed (29.11). A
     capacity is taken on a trial farm as wide as homeW gives, so the Barn is its 22 there too and the sign meets no Escalations. */
  var capMemo = {};
  function capAt(zone, w, M) {
    var k = zone + ":" + w + ":" + M;
    if (capMemo[k] === undefined) {
      var o = { mailbox: M }; o[zone] = w;
      var hs = farmstead({ x: 0, y: 0, w: homeW(o), h: SLOT.h }, o), c = hs.cells[zone], pn = zone === "pen" && penOf(null, "", c);
      capMemo[k] = pn ? pn.cols * pn.rows : slotsOf(c, zone, zone === "escalation" ? hs.sign : [hs.keep[zone], hs.sign]).length;
    }
    return capMemo[k];
  }
  function ladderOf(zone) { for (var i = 0; i < HOME.cells.length; i++) if (HOME.cells[i][0] === zone) return HOME.cells[i][3]; return null; }
  function homeWidths(n, prev) {
    var out = {}, fits = function (zone, k, M) { var l = ladderOf(zone); for (var i = 0; i < l.length; i++) if (capAt(zone, l[i], M) >= k) return l[i]; return l[l.length - 1]; };
    var size = function (zone, M) {
      var c = Math.max(0, n[zone] | 0), was = prev && prev[zone], grow = fits(zone, c, M);
      if (ladderOf(zone).indexOf(was) < 0 || grow >= was) return grow;
      var shrink = fits(zone, c + HOME.slack, M);
      return shrink < was ? shrink : was;
    };
    out.mailbox = size("mailbox");
    out.escalation = size("escalation", out.mailbox);
    out.pen = size("pen"); out.growing = size("growing");
    return out;
  }
  /* slotsOf(cell, zone, skip) -> each slot's rect (tiles) a zone's cell holds at its pitch inside a 1-tile margin, in reading order,
     less those meeting skip (a rect or a list of them): its length is the capacity. */
  function slotsOf(cell, zone, skip) {
    var p = HOME.pitch[zone], sw = p[3] || TILE, iw = (cell.w - 2) * TILE, ih = (cell.h - 2) * TILE, out = [], sk = [].concat(skip || []).filter(Boolean);
    var cols = Math.floor((iw - sw) / p[0]) + 1, rows = Math.floor((ih - p[2]) / p[1]) + 1;
    for (var i = 0; i < cols * rows; i++) {
      var r = { x: cell.x + 1 + (i % cols) * p[0] / TILE, y: cell.y + 1 + Math.floor(i / cols) * p[1] / TILE, w: sw / TILE, h: p[2] / TILE };
      if (!sk.some(function (k) { return hit(r, k); })) out.push(r);
    }
    return out;
  }
  /* looseCap(n) -> how many of a Growing field's n slots the loose beads may take (MEADOW_SHARE, 28.12). */
  function looseCap(n) { return Math.floor(n * MEADOW_SHARE); }
  /* penOf(wishes, key, cell) -> project key's pen, its fence the Pen cell (28.2): {id "home:<key>/pen", kind "pen", x, y, w, h,
     cols, rows, items, more}. It holds the newest open wishes it fits, planted oldest first (plantOrder by opened; gd-wDdun2AVwq),
     so a new wish moves no goat. Item i is {id "wish:<id>", kind "goat", wish, x, y, w, h, i, out}; out (set by layout()) is
     whether it grazes by a plot of its kin. more counts the rest and open_more[key]. */
  function penOf(wishes, key, cell) {
    var open = ((wishes && wishes.open) || []).filter(function (w) { return w && w.id && projectKey(w.project) === key; }), om = (wishes && wishes.open_more) || {};
    var cols = Math.floor((cell.w - 2) / PEN.dx), rows = Math.floor((cell.h - 2) / PEN.dy), n = Math.min(cols * rows, open.length);
    var pen = { id: "home:" + key + "/pen", kind: "pen", zone: "pen", x: cell.x, y: cell.y, w: cell.w, h: cell.h, cols: cols, rows: rows, items: [], more: open.length - n + Math.max(0, om[key] | 0), project: key };
    plantOrder(open.slice(0, n), "opened").forEach(function (wi, i) {
      pen.items.push({ id: "wish:" + wi.id, kind: "goat", wish: wi, x: cell.x + 1 + (i % cols) * PEN.dx, y: cell.y + 1 + Math.floor(i / cols) * PEN.dy, w: PEN.dx, h: PEN.dy, i: i, out: false, project: key });
    });
    return pen;
  }
  /* escOf(escalations, key, cell, skip) -> project key's Escalations zone (28.13): {id "home:<key>/escalation", kind "zone", zone, x, y,
     w, h, items, crate, more, total, project}. It holds the farm document's open escalations of that project (escalations.open; the
     Commons's have none) on ESCKIND.cells' grid at the Mailbox's pitch, one per slot (less those meeting skip, the sign). Item i is {id "esc:<id>", kind "escalation",
     zone, wait (the FarmWait), x, y, w, h, slot, project}: the prefix keeps it apart from the bead's own task cell. Past
     the cell's slots (at its max width, 29.10), crate is the last slot's "+N" {id "<zone id>/more", kind "crate", zone, n, x, y, w, h,
     slot, project}, else null;
     total is the project's count (escalations.by_project, which counts what the farm's cap left out of open), more the ones no
     item draws (28.11, step 6 gd-4FIF5pzC6O). */
  function escOf(escalations, key, cell, skip) {
    var open = ((escalations && escalations.open) || []).filter(function (w) { return w && w.id && projectKey(w.project) === key; }), pos = slotsOf(cell, "escalation", skip);
    var total = escTotal(escalations, key, open.length);
    var z = { id: "home:" + key + "/escalation", kind: "zone", zone: "escalation", x: cell.x, y: cell.y, w: cell.w, h: cell.h, items: [], crate: null, more: 0, total: total, project: key };
    var cols = pos.filter(function (r) { return r.y === pos[0].y; }).length;
    ESC.cells(open, { cols: cols, id: z.id, total: total, cap: pos.length }).forEach(function (c) {
      var r = pos[c.i];
      if (!r) { z.more += c.kind === "crate" ? c.n : 1; return; }   // cap is the slot count, so this is a guard
      if (c.kind === "crate") { z.crate = { id: c.id, kind: "crate", zone: "escalation", n: c.n, x: r.x, y: r.y, w: r.w, h: r.h, slot: c.i, project: key }; z.more += c.n; return; }
      z.items.push({ id: "esc:" + c.w.id, kind: "escalation", zone: "escalation", wait: c.w, x: r.x, y: r.y, w: r.w, h: r.h, slot: c.i, project: key });
    });
    return z;
  }
  /* escTotal(escalations, key, listed) -> project key's open escalations: escalations.by_project folded to the key, as open's
     spellings are, at least the listed ones (28.11): the Escalations cell's count (29.10). */
  function escTotal(escalations, key, listed) {
    var bp = escalations && escalations.by_project, total = 0;
    if (bp) Object.keys(bp).forEach(function (p) { if (projectKey(p) === key) total += bp[p] | 0; });
    return Math.max(total, listed | 0);
  }
  /* homeCounts(doc, key, stands) -> {escalation, mailbox, pen, growing}: what each of project key's sized cells holds by the farm
     document alone (29.10), stands the gardeners with a field stand there (P.fieldHome), tended its loose beads a gardener is
     out at, each a whole slot. The Mailbox's letters less the
     escalations, which the farm counts among them (28.13); the Pen's listed wishes and open_more; the field's stands and its
     loose beads two to a slot, room for those at most MEADOW_SHARE of it. */
  function homeCounts(doc, key, stands, tended) {
    var hc = (doc.home || {})[key] || {}, wi = doc.wishes || {}, mine = function (w) { return w && w.id && projectKey(w.project) === key; };
    var esc = escTotal(doc.escalations, key, ((doc.escalations || {}).open || []).filter(mine).length), l = Math.max(0, hc.field | 0) + (tended | 0);
    return { escalation: esc, mailbox: Math.max(0, (hc.letters | 0) - esc), pen: (wi.open || []).filter(mine).length + Math.max(0, (wi.open_more || {})[key] | 0),
      growing: Math.max((stands | 0) + Math.ceil(l / 2), 2 * Math.ceil(l / 2)) };
  }
  /* Memo v5 (spec 5.1, 28.5, 29.5): cells[key] = {k, t}, the project's lattice index and when it was last seen; slots[planId] =
     {s, t}, s >= 1, the plot's spiral slot when it was last seen, read as a rank (the plans are packed in that order, step 1 of
     layout()). A v4 memo keeps its cells and every slot shifted up by one, since slot 0 is now the farmstead (X-D9), so the first
     load re-packs each biome's plans once in the order they had; a v1, v2 or v3 memo keeps its slots, shifted the same, and drops
     its cells, so the first load also re-packs the projects into the lattice (H-D8). The Commons is always cell 0 and is not
     stored. */
  function emptyMemo() { return { v: 5, cells: {}, slots: {} }; }
  function cloneMemo(m) {
    var out = emptyMemo(), k;
    if (m && m.v >= 1 && m.v <= 5) {
      if (m.v >= 4) for (k in m.cells || {}) {
        var c = m.cells[k];
        if (k && c && isFinite(c.k) && c.k >= FIRST_K) out.cells[k] = { k: c.k | 0, t: +c.t || 0 };
      }
      var up = m.v < 5 ? 1 : 0;
      for (k in m.slots || {}) if (m.slots[k] && isFinite(m.slots[k].s)) out.slots[k] = { s: Math.max(1, (m.slots[k].s | 0) + up), t: +m.slots[k].t || 0 };
    }
    return out;
  }
  /* Drop entries not seen for MEMO_DAYS (a project gone that long frees its lattice index) and keep the newest-seen under the caps. */
  function pruneMemo(m, now) {
    var cut = now - MEMO_DAYS * 86400000;
    ["cells", "slots"].forEach(function (part) {
      var cap = part === "cells" ? MEMO_CELLS : MEMO_SLOTS, keys = Object.keys(m[part]).filter(function (k) { if (m[part][k].t < cut) { delete m[part][k]; return false; } return true; });
      if (keys.length > cap) {
        keys.sort(function (a, b) { return m[part][b].t - m[part][a].t || (a < b ? -1 : 1); });
        keys.slice(cap).forEach(function (k) { delete m[part][k]; });
      }
    });
  }
  /* Deterministic cell for a hashed id inside a band of `cells` cells, probing linearly past a taken one. */
  function hashCell(id, cells, usedCells) {
    if (cells <= 0) return -1;
    var c = h32(id, 3) % cells;
    for (var n = 0; n < cells; n++) { var k = (c + n) % cells; if (!usedCells[k]) { usedCells[k] = 1; return k; } }
    return -1;
  }
  function byCreated(a, b) { return (a.created || 0) - (b.created || 0) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); }
  function byId(a, b) { return a.id < b.id ? -1 : a.id > b.id ? 1 : 0; }
  /* plantOrder(list, key) -> a new array of list in the order a bed or the pen plants it: by list[i][key] (created or opened,
     unix seconds), then id, so a plant keeps its cell through every change but its birth and a status change moves nothing
     (gd-wDdun2AVwq). Plants without the key (an older farm document) keep the list's own order, ahead of the dated ones. */
  function plantOrder(list, key) {
    return list.map(function (it, k) { return { it: it, k: k }; }).sort(function (a, b) {
      var ca = +a.it[key] || 0, cb = +b.it[key] || 0;
      if (ca !== cb) return ca - cb;
      return ca ? byId(a.it, b.it) : a.k - b.k;
    }).map(function (e) { return e.it; });
  }
  function countsTotal(c) { return (c.in_progress || 0) + (c.review || 0) + (c.blocked || 0) + (c.open || 0) + (c.deferred || 0) + (c.closed || 0); }
  /* The used 3x3 blocks of a bed holding cursor cells 0..n-1 (block-major), as {c0, r0, cols, rows} in cell coordinates: one
     block is 3x3, two are 6x3, four to six are 9x6 (spec 5.4). An empty bed is 0x0. */
  function usedBlocks(n) {
    if (n <= 0) return { c0: 0, r0: 0, cols: 0, rows: 0 };
    var last = Math.floor((n - 1) / 9);
    return { c0: 0, r0: 0, cols: last < 3 ? (last + 1) * 3 : 9, rows: (Math.floor(last / 3) + 1) * 3 };
  }
  /* The beds of one plot: the phases in phase_order, then Other when it holds anything, one 9x9 bed each on the
     plot's 3x3 in reading order. Past nine, beds 1-8 hold the first eight and position 9 is a crate "+N phases" (plot.crate) and
     the plot is overgrown. In a bed the plants, the listed tasks and the listed closed steps together, fill block-major cells
     in the order they were created (then id, plantOrder), so a step keeps its cell when its status changes or
     it closes and a new one takes the next cell; then the crate "+N" when more are open than fit (a cursor stop, so it
     follows the plants directly), then stubble for the rest of the closed count while there is room. Each bed carries
     what its stake shows: the ordinal n, progress pips in quarters, whether it is the current phase (the first not closed, lit)
     and whether it is closed; and arms, the phase's arms in plain words for the stake's hot ("" for none and on Other), since
     the stake draws no picture.
     was is the plan's placement memo {bedId: {beadId: cell}} from the last layout and cells the new one: a
     plant keeps its cell through every later document, a new one takes the lowest free cell, and one listed no longer holds
     its cell as a dim marker (bed.gone, drawn as stubble) rather than closing the gap, reclaimed lowest first when the bed is
     full. With no memo the plants take cells 0..n-1 in plant order. */
  function bedMemo(m) {
    var out = {}, seen = {};
    if (m) Object.keys(m).forEach(function (b) { var c = m[b]; if (b && c === (c | 0) && c >= 0 && c < BED.cap && !seen[c]) { out[b] = c; seen[c] = 1; } });
    return out;
  }
  function planBeds(plan, plot, was) {
    var rows = [], beds = [], crate = null, memo = {};
    (plan.phases || []).forEach(function (ph) { rows.push({ ph: ph, other: false }); });
    var u = plan.unphased, uc = u && u.counts ? u.counts : null;
    if (u && (plan.staked || (u.tasks || []).length || (uc && countsTotal(uc)))) rows.push({ ph: u, other: true });   // a staked plot always has its bed, empty until it has steps
    if (rows.length > BED.phases) {
      var n = rows.length - (BED.phases - 1);
      rows = rows.slice(0, BED.phases - 1);
      crate = { id: plan.id + "/phases", kind: "phases", n: n, x: plot.x + BED.off[2], y: plot.y + BED.off[2], w: BED.size, h: BED.size, at: bedAt(BED.phases - 1), plot: plot.id };
    }
    var current = false;
    rows.forEach(function (row, k) {
      var ph = row.ph, at = bedAt(k), bx = plot.x + BED.off[at.c], by = plot.y + BED.off[at.r], id = row.other ? plan.id + "/other" : ph.id || plan.id + "/" + k;
      var tasks = ph.tasks || [], c = ph.counts || {}, cells = [], live = tasks.length + (ph.more || 0);
      var fit = Math.min(tasks.length, live > BED.cap ? BED.cap - 1 : BED.cap);
      var over = live - fit, total = countsTotal(c), done = c.closed || 0;
      /* the listed closed steps (FarmPhase.closed) are cursor stops with their own titles (gd-0Fj6E2vPql), planted among the
         tasks by created then id (plantOrder) so a step keeps its cell when it closes; the crate follows the plants, and the
         closed count past the listed steps is stubble after it */
      var shown = (ph.closed || []).slice(0, Math.max(0, BED.cap - fit - (over ? 1 : 0)));
      var plants = plantOrder(tasks.slice(0, fit).concat(shown), "created");
      var bm = memo[id] = bedMemo(was && was[id]), held = {}, listed = {}, gone = [], free = 0, top = 0;
      plants.forEach(function (tk) { listed[tk.id] = 1; });
      Object.keys(bm).forEach(function (b) { held[bm[b]] = 1; if (!listed[b]) gone.push(b); });
      gone.sort(function (a, b) { return bm[a] - bm[b]; });
      var next = function () {   // the lowest free cell; a full bed gives up its lowest dim marker
        while (held[free]) free++;
        var c = free;
        if (c >= BED.cap) { c = bm[gone[0]]; delete bm[gone.shift()]; }
        held[c] = 1; if (c >= top) top = c + 1;
        return c;
      };
      plants.forEach(function (tk) { if (bm[tk.id] === undefined) bm[tk.id] = next(); else if (bm[tk.id] >= top) top = bm[tk.id] + 1; });
      gone.forEach(function (g) { if (bm[g] >= top) top = bm[g] + 1; });
      plants.forEach(function (tk) { var i = bm[tk.id], a = cellAt(i); cells.push({ id: tk.id, kind: "task", x: bx + a.c, y: by + a.r, w: 1, h: 1, task: tk, plot: plot.id, phase: ph.id || "", at: a, i: i }); });
      cells.sort(function (a, b) { return a.i - b.i; });
      var ci = over ? next() : -1, ca = over ? cellAt(ci) : null;
      var closed = !row.other && (ph.status === "closed" || (total > 0 && done >= total && !live));
      var bed = { id: id, title: ph.title || (row.other ? "Other" : ph.id || "Phase " + (k + 1)), status: ph.status || "", other: row.other, n: k + 1, at: at,
        x: bx, y: by, w: BED.size, h: BED.size, cells: cells, used: usedBlocks(top), gone: gone.map(function (g) { return bm[g]; }),
        crate: over ? { id: id + "/more", kind: "crate", n: over, x: bx + ca.c, y: by + ca.r, w: 1, h: 1, at: ca, i: ci, plot: plot.id } : null,
        stubble: Math.min(Math.max(0, done - shown.length - gone.length), BED.cap - top), stubbleAt: top,
        done: done, total: total, pips: total ? Math.floor(4 * done / total) : 0, closed: closed, current: false, arms: row.other ? "" : ph.heraldry_name || "" };
      if (!row.other && !closed && !current) { bed.current = true; current = true; }
      beds.push(bed);
    });
    return { beds: beds, crate: crate, moreRows: crate ? crate.n : 0, cells: memo };
  }
  /* plotRollup(plot, gardeners) -> {cells, bar, total, needs, gardeners, hats}: what a plan's plot shows at the project depth
     (cursor-grid spec 6.6, the rollup stamp). cells are the plot's 3x3 in reading order, each at its bed's real position (at):
     a phase or the Other bed carries its frame colour (G, the lit gold that stays bright at night, for the current phase; current beats closed beats blocked-and-nothing-moving beats review), its
     fill (done / total, 0 with no tasks), closed, and the sprout / rock / fruit marks (in progress, blocked, in review) of an
     open phase; a position with no bed is empty; the ninth holds the "+N phases" crate when the plot has one. tasks, more,
     stubble and stubbleAt are the bed's own (planBeds), for the beds drawn under the stamp. bar is plan.counts by state in the
     bar's order, zero counts left out. gardeners is a count or the looks (gardenerLook) of the gardeners on the plot, in need
     order: plotHeads. */
  var ROLLUP_BAR = ["closed", "in_progress", "review", "blocked", "open", "deferred"];
  function plotRollup(plot, gardeners) {
    var cells = [], k, plan = plot.plan || {}, counts = plan.counts || {};
    for (k = 0; k < BED.phases; k++) cells.push({ at: bedAt(k), frame: "", fill: 0, closed: false, sprout: false, rock: false, fruit: false, other: false, crate: 0, empty: true });
    (plot.beds || []).forEach(function (bd) {
      var ph = (bd.other ? plan.unphased : (plan.phases || [])[bd.n - 1]) || {}, c = ph.counts || {}, open = !bd.closed;
      var blocked = c.blocked || 0, wip = c.in_progress || 0, review = c.review || 0;
      cells[bd.at.r * 3 + bd.at.c] = { at: bd.at, frame: bd.current ? "G" : bd.closed ? "L" : blocked && !wip ? "e" : review ? "o" : "k",
        fill: bd.total ? Math.min(1, bd.done / bd.total) : 0, closed: bd.closed, sprout: open && wip > 0, rock: open && blocked > 0, fruit: open && review > 0,
        other: bd.other, crate: 0, empty: false, tasks: bd.cells, more: bd.crate, stubble: bd.stubble, stubbleAt: bd.stubbleAt, staked: !!plot.staked };
    });
    if (plot.crate) cells[plot.crate.at.r * 3 + plot.crate.at.c] = { at: plot.crate.at, frame: "", fill: 0, closed: false, sprout: false, rock: false, fruit: false, other: false, crate: plot.crate.n, empty: false };
    var bar = [], total = 0;
    ROLLUP_BAR.forEach(function (s) { var n = counts[s] || 0; if (n > 0) { bar.push({ state: s, n: n }); total += n; } });
    var o = plotHeads(gardeners); o.cells = cells; o.bar = bar; o.total = total; o.needs = plot.needs || 0;
    return o;
  }
  /* plotHeads(gardeners) -> {gardeners, hats, tint}: the gardeners' badge (gd-vi77cq1oYY) from a count or the looks in need
     order; tint is the first's pose when "down" or "ask". */
  function plotHeads(gardeners) {
    var list = typeof gardeners === "number" ? null : gardeners || [], p = list && list[0] ? list[0].pose : "";
    return { gardeners: list ? list.length : Math.max(0, gardeners | 0), hats: list ? list.slice(0, 4).map(function (g) { return g && g.hat != null ? g.hat : 8; }) : [],
      tint: p === "down" || p === "ask" ? p : "" };
  }
  /* "Needs you" counts only what waits on a human (needs_human, gd-TEl9BfDL2z): an approved escalation waits on its executor.
     A document from before the field counts every open decision and escalation, as it did then. */
  function needsOf(plan) { return plan.needs_human != null ? plan.needs_human : (plan.open_decisions || 0) + (plan.open_escalations || 0); }
  function openOf(c) { return c.in_progress + c.review + c.blocked + c.open + c.deferred; }
  /* What a plan shows, as its crop tile (web/plans.js) and its plot (web/scene.js): the crop (which sprite), the progress, and
     the chips. A plan idle past STALE_S with nothing in progress wilts. */
  var STALE_S = 12 * 3600;
  function tile(p, now) {
    var c = p.counts || {}, n = { in_progress: c.in_progress || 0, review: c.review || 0, blocked: c.blocked || 0, open: c.open || 0, deferred: c.deferred || 0, closed: c.closed || 0 };
    var o = openOf(n), total = o + n.closed, nd = needsOf(p);
    var idle = now - (p.activity || p.updated || 0) > STALE_S;
    var crop = 'sprout';
    if (p.staked) crop = 'staked';   // laid out, not planted (WORLD.stakedPlan): the pale string's colour, whatever waits on it
    else if (nd > 0) crop = 'post';
    else if (n.blocked > 0 && n.in_progress === 0) crop = 'blocked';
    else if (n.review > 0) crop = 'fruit';
    else if (idle && n.in_progress === 0) crop = 'wilt';
    return { crop: crop, counts: n, open: o, total: total, done: n.closed, pct: total ? Math.round(n.closed / total * 100) : 0, needs: nd, idle: idle };
  }
  /* A ready or asked seed is staked out (homestead-world-design.md section 3): it takes a plot slot beside the plans, and its
     plot is planned from its steps (FarmSeed.steps, in step order) as one unphased bed, one cell per step in state "planned".
     stakedPlan is the plan-shaped record the plot carries, so every reader of plot.plan works on it; the seed rides as plot.seed.
     Its open decision is the one thing that waits on a human. A draft (frosted) stays a pot in the Nursery. */
  function isStaked(s) { return !!s && (s.state === "ready" || s.state === "asked"); }
  function stakedPlan(s) {
    var steps = (s.steps || []).map(function (st) { return { id: st.id, title: st.title || st.id, type: "task", state: "planned", status: st.status || "", n: st.n, heraldry: st.heraldry || "" }; });
    return { id: s.id, title: s.title, short_title: s.short_title || "", project: s.project, operator: s.operator, priority: s.priority, created: s.created, updated: s.updated, activity: s.updated,
      state: s.state, staked: true, needs_human: s.decision ? 1 : 0, counts: { open: steps.length }, phases: [],
      unphased: { id: "", title: "Steps", counts: { open: steps.length }, tasks: steps } };
  }
  /* board(biome, plots, max) -> the World level's words on a project (4b.2, owner decision gd-H6ZPz5V262): what the fully
     zoomed-out garden says about each plan of a project, in its link and the live region (no board lists them on the map,
     gd-OOt8Mx2Mps). plots is gardenerSpots(...)[3].plots, or null. One row per plan: {id, title, needs,
     pct (closed of all its steps), live (gardeners alive on its plot)}; rows that need you first, then the moving ones (a live
     gardener), then the rest, each by progress then title. rows keeps the first max; more counts the rest, the plans past the
     region's plot cap included. head is the summary line: "7 plans · 2 moving · 1 needs you · 3 escalations", parts that read zero
     left out (escalations: the strip's Escalations cell's total). on
     lists every plan with a gardener, {id, title, live, down, ask}, down then asking first. */
  function board(b, plots, max) {
    var rows = [], on = [], need = 0, moving = 0, n = b.plots.length + (b.overflow ? b.overflow.n : 0), parts;
    b.plots.forEach(function (p) {
      var sp = plots && plots[p.id], live = sp ? sp.counts.all : 0, c, total = 0, title = (p.plan.staked && p.plan.short_title) || p.plan.title || p.id;
      if (p.needs) need++;
      if (live) { moving++; on.push({ id: p.id, title: title, live: live, down: sp.counts.down || 0, ask: sp.counts.ask || 0 }); }
      if (max === 0) return;   // a header: the counts only
      c = p.plan.counts || {}; ROLLUP_BAR.forEach(function (s) { total += c[s] || 0; });
      var row = { id: p.id, title: title, needs: p.needs || 0, pct: total ? Math.round((c.closed || 0) / total * 100) : 0, live: live };
      if (p.plan.staked) row.staked = true;   // a plan staked out, not yet planted: its row says so where a planted one has its progress
      rows.push(row);
    });
    rows.sort(function (a, c) { return (c.needs > 0) - (a.needs > 0) || (c.live > 0) - (a.live > 0) || c.pct - a.pct || (a.title < c.title ? -1 : a.title > c.title ? 1 : a.id < c.id ? -1 : 1); });
    max = max == null ? rows.length : Math.max(0, Math.min(max, rows.length));
    parts = [plural(n, "plan", "plans")]; if (moving) parts.push(moving + " moving"); if (need) parts.push(need + (need === 1 ? " needs you" : " need you"));
    var esc = b.home && b.home.zones && b.home.zones.escalation ? b.home.zones.escalation.total | 0 : 0;
    if (esc) parts.push(ESC.count(esc));   // the World level draws no escalation, so its words count them (28.11)
    var nd = function (o) { return o.down ? 2 : o.ask ? 1 : 0; }; on.sort(function (a, c) { return nd(c) - nd(a); });
    return { id: b.id, name: b.name, plans: n, moving: moving, need: need, escalations: esc, head: parts.join(" · "), rows: rows.slice(0, max), more: n - max, on: on };
  }
  /* The board in words, for the project's link and the live region, then the plans with gardeners its rows leave out. */
  function boardWords(bd) {
    var w = bd.name + ": " + bd.head + ".", shown = {};
    bd.rows.forEach(function (r) { shown[r.id] = 1; w += " " + r.title + ": " + (r.needs ? plural(r.needs, "thing needs", "things need") + " you, " : "") + (r.staked ? "staked out" : r.pct + "% done") + (r.live ? ", " + plural(r.live, "gardener", "gardeners") : "") + "."; });
    if (bd.more) w += " " + plural(bd.more, "more plan", "more plans") + ": go in to see them.";
    return w + onWords((bd.on || []).filter(function (o) { return !shown[o.id]; }));
  }
  /* " Gardeners on Alpha (2, 1 down), Bravo (1)." for a board's on, "" for none. */
  function onWords(on) {
    return on.length ? " Gardeners on " + on.map(function (o) { return o.title + " (" + o.live + (o.down ? ", " + o.down + " down" : "") + (o.ask ? ", " + o.ask + " waiting on you" : "") + ")"; }).join(", ") + "." : "";
  }

  /* layout(doc, memo, opts) -> World. doc is the /garden/farm document (plans, seeds, closed_plans.shown, meadow.shown, wishes, home,
     gardeners: a project with only home items is a biome, 28.3); memo is the
     sticky store (section 3.4) or null; opts: { now (ms), readonly, tidy }. Positions are in TILES; bbox is in world px. The result
     carries `memo` (the store to persist) and `memoChanged`; opts.cells is the beds' placement memo per plan (planBeds), given
     back as `cells` (pruned to this document's plans) with `cellsChanged`. The same project and plan sets give the same map in every browser,
     and with a memo no project changes lattice cell because of another Thing, and a plot changes slot only when a plan ahead of
     it in the spiral finishes, one step back for each (N-D9); pixel positions may shift by whole column widths when a column's
     widest region grows. tidy re-numbers the lattice from scratch (slots kept). */
  function layout(doc, memo, opts) {
    opts = opts || {}; doc = doc || {};
    var now = opts.now || Date.now(), m = cloneMemo(memo), before = memo && memo.v >= 1 && memo.v < 5 ? "" : JSON.stringify(m), cm = opts.cells && typeof opts.cells === "object" ? opts.cells : {}, cells = {};
    var groups = {}, names = {}, k;
    var grp = function (project) {
      var key = projectKey(project);
      if (!groups[key]) { groups[key] = { plans: [], seeds: [], staked: [], closed: [], meadow: [], graves: [] }; names[key] = key ? String(project).trim() : "Commons"; }
      return groups[key];
    };
    var add = function (kind, it) { if (it && it.id) grp(it.project)[kind].push(it); };
    (doc.plans || []).forEach(function (p) { add("plans", p); });
    (doc.seeds || []).forEach(function (s) { add("seeds", s); if (isStaked(s)) add("staked", s); });
    ((doc.closed_plans || {}).shown || []).forEach(function (c) { add("closed", c); });
    ((doc.meadow || {}).shown || []).forEach(function (t) { add("meadow", t); });
    ((doc.wishes || {}).closed || []).forEach(function (w) { add("graves", w); });   // closed wishes: the graveyard's headstones (gd-s9KzRk6boh)
    ((doc.wishes || {}).open || []).forEach(function (w) { if (w && w.id) grp(w.project); });   // home items (28.3): open wishes, escalations, gardeners, the home counts
    ((doc.escalations || {}).open || []).forEach(function (w) { if (w && w.id) grp(w.project); });
    (doc.gardeners || []).forEach(function (g) { if (g && g.id) grp(P.isThread(g) ? "" : g.project); });
    Object.keys(doc.home || {}).forEach(function (hk) { var c = doc.home[hk] || {}; if (c.letters || c.field || c.barn || c.wishes) grp(hk); });

    /* 1. slots, per project, packed (decision gd-VfOCGDpxg2, spec 12.2): the plans with a memo'd slot keep their order (slot,
       then created and id), the newcomers follow in (created, id) order, and the i-th takes spiral slot i + 1, slot 0 being the
       farmstead (29.1). So the live plans fill slots 1..n with no fallow hole; when a plan finishes, only the plots after it step
       back, one slot each, in the same order, and a new plan takes the next slot. Past SLOT_CAP the rest overflow. A staked seed
       ranks as a plan does (the memo is keyed by bead id), so approving it, which moves its id from seeds to plans, keeps its
       slot. The grid, the box of the farm, slot 1 and the used slots (X-D3), gives the biome its shape, which sizes its region
       before anything is placed. The courtyard's cells are sized here too, per farm by its counts (29.10): the gardeners' field
       stands by people.js's rule, prev the page's last world, whose widths the hysteresis keeps; their widths give the farm's
       growth, grid.g (29.11), so the region is sized at the farm's width. */
    var keys = Object.keys(groups), fit = {};
    var stands = {}, pix = (opts.prev && opts.prev.index) || {}, has = function (id) { return groups[id.slice(8)] !== undefined; };
    (doc.gardeners || []).forEach(function (g) { var at = g && P.fieldHome(g, has); if (at) stands[at] = (stands[at] || 0) + 1; });
    var tends = {};   // the beads a running gardener off a plot is out at: its flower keeps its east tile, where that one stands (people.js standBeside)
    (doc.gardeners || []).forEach(function (g) { if (g && (g.bucket === "alive" || g.bucket === "stuck") && !(g.plot && g.plot.plan)) [g.task, g.tends].forEach(function (id) { if (id) tends[id] = 1; }); });
    keys.forEach(function (key) {
      var g = groups[key], free = {}, ranked = [], pend = [], overflow = null, c0 = 0, c1 = SPIRAL[1].c, r0 = 0, r1 = SPIRAL[1].r, age = Infinity;
      var planned = {}; g.plans.forEach(function (p) { planned[p.id] = 1; });   // a seed caught mid-approve is its plan's plot already
      g.plans.concat(g.staked.filter(function (sd) { return !planned[sd.id]; }).map(stakedPlan)).sort(byCreated).forEach(function (p) { (m.slots[p.id] ? ranked : pend).push(p); });
      ranked.sort(function (a, b) { return m.slots[a.id].s - m.slots[b.id].s || byCreated(a, b); });
      ranked.concat(pend).forEach(function (p, i) { if (i < SLOT_CAP) { free[i + 1] = p; return; } overflow = overflow || { n: 0, ids: [] }; overflow.n++; overflow.ids.push(p.id); });
      var slots = Object.keys(free).map(Number).sort(function (a, b) { return a - b; });
      slots.forEach(function (s) { var q = SPIRAL[s]; c0 = Math.min(c0, q.c); c1 = Math.max(c1, q.c); r0 = Math.min(r0, q.r); r1 = Math.max(r1, q.r); });
      var pw = pix["project:" + key], pc = pw && pw.kind === "biome" && pw.home && pw.home.cells;
      var widths = homeWidths(homeCounts(doc, key, stands["project:" + key], g.meadow.filter(function (t) { return tends[t.id]; }).length), pc && { escalation: pc.escalation.w, mailbox: pc.mailbox.w, pen: pc.pen.w, growing: pc.growing.w });
      var grid = { c0: c0 + (SLOT.cols - 1) / 2, r0: r0 + (SLOT.rows - 1) / 2, cols: c1 - c0 + 1, rows: r1 - r0 + 1, g: homeW(widths) - SLOT.w };
      g.plans.concat(g.seeds).forEach(function (it) { if (it.created > 0 && it.created < age) age = it.created; });   // the project's age: its oldest plan or seed
      var graves = Math.min(g.graves.length, CAPS.graveyard);
      fit[key] = { free: free, slots: slots, overflow: overflow, graves: graves, size: regionSize(grid, !!overflow, graves), age: age, grid: grid, widths: widths };
    });

    /* 2. lattice indices (spec 5.1). The Commons is cell 0. A memo'd project keeps its index; every remembered index stays taken
       while it is remembered, even when its project is not in this document, so nobody else lands on it. Newcomers (every project
       in a tidy) take the lowest free index from 1, oldest first (the oldest created of their plans and seeds, then h32, then key). */
    var cellOf = {}, taken = {}, newcomers = [];
    taken[COMMONS_K] = "";
    if (groups[""]) cellOf[""] = COMMONS_K;
    if (!opts.tidy) Object.keys(m.cells).sort(function (a, b) { return m.cells[b].t - m.cells[a].t || (a < b ? -1 : 1); }).forEach(function (key) {
      var c = m.cells[key].k;
      if (taken[c] === undefined) { taken[c] = key; if (groups[key]) cellOf[key] = c; }
    });
    keys.forEach(function (key) { if (cellOf[key] === undefined) newcomers.push(key); });
    newcomers.sort(function (a, b) { return fit[a].age - fit[b].age || h32(a, 1) - h32(b, 1) || (a < b ? -1 : a > b ? 1 : 0); });
    var nextK = FIRST_K;
    newcomers.forEach(function (key) { while (taken[nextK] !== undefined) nextK++; taken[nextK] = key; cellOf[key] = nextK; });

    /* 3. pixels: the lattice spans cells 0..maxK; each row as tall as its tallest region, GAP between. A region is placed by its
       base width w0, its width less its farm's growth (29.11): centred in its column's widest w0 (colW0), and the column as wide
       as its regions' furthest east edge (colW), so a farm that widens moves only the columns east of its own. An empty cell (the Commons before it holds anything, a project remembered but not in this document) keeps its
       index but takes no space, and a column or row with nothing in it is no wider than nothing (N-D0: space first; N-D9 lets
       pixels shift by whole columns when it fills). Cell 0's centre is the origin. */
    var maxK = COMMONS_K;
    keys.forEach(function (key) { maxK = Math.max(maxK, cellOf[key]); });
    var lcols = Math.min(LATTICE_W, maxK + 1), lrows = Math.floor(maxK / LATTICE_W) + 1, colW = [], colW0 = [], rowH = [], byK = {}, ci;
    for (ci = 0; ci < lcols; ci++) { colW.push(0); colW0.push(0); }
    for (ci = 0; ci < lrows; ci++) rowH.push(0);
    keys.forEach(function (key) { byK[cellOf[key]] = key; });
    var kk, w0Of = function (key) { return fit[key].size.w - fit[key].grid.g; };
    var offOf = function (key) { return Math.floor((colW0[latticeAt(cellOf[key]).c] - w0Of(key)) / 2); };
    keys.forEach(function (key) { var at = latticeAt(cellOf[key]); colW0[at.c] = Math.max(colW0[at.c], w0Of(key)); rowH[at.r] = Math.max(rowH[at.r], fit[key].size.h); });
    keys.forEach(function (key) { var at = latticeAt(cellOf[key]); colW[at.c] = Math.max(colW[at.c], offOf(key) + fit[key].size.w); });
    var colX = [-Math.floor(colW0[0] / 2)], rowY = [-Math.floor(rowH[0] / 2)];
    for (ci = 1; ci < lcols; ci++) colX.push(colX[ci - 1] + (colW[ci - 1] ? colW[ci - 1] + GAP : 0));
    for (ci = 1; ci < lrows; ci++) rowY.push(rowY[ci - 1] + (rowH[ci - 1] ? rowH[ci - 1] + GAP : 0));
    var cellRect = function (kk) { var at = latticeAt(kk); return { x: colX[at.c], y: rowY[at.r], w: colW[at.c], h: rowH[at.r] }; };
    var centres = {}, regions = {};
    keys.forEach(function (key) {
      var cr = cellRect(cellOf[key]), sz = fit[key].size, rx = cr.x + offOf(key), ry = cr.y + Math.floor((cr.h - sz.h) / 2);
      centres[key] = { x: rx - sz.ox, y: ry - sz.oy }; regions[key] = { x: rx, y: ry, w: sz.w, h: sz.h };
    });
    var order = keys.slice().sort(function (a, b) { return cellOf[a] - cellOf[b]; });

    var lattice = { w: LATTICE_W, cols: lcols, rows: lrows, cells: [] };
    for (kk = 0; kk <= maxK; kk++) { var la = latticeAt(kk); lattice.cells.push({ k: kk, c: la.c, r: la.r, id: byK[kk] !== undefined ? "project:" + byK[kk] : null, rect: cellRect(kk) }); }
    var world = { lattice: lattice, biomes: [], bbox: null, memo: m, memoChanged: false, cells: cm, cellsChanged: false, escs: {} }, index = {};
    ((doc.escalations || {}).open || []).forEach(function (w) { if (w && w.id) world.escs[w.id] = w; });   // every open escalation by id, drawn or past the cap: an executor's words (runningWords)
    order.forEach(function (key) {
      var g = groups[key], f = fit[key], c = centres[key], name = names[key], seed = h32(key, 3), sh = shape(c.x, c.y, f.grid, !!f.overflow, f.graves), ck = latticeAt(cellOf[key]);
      var b = { id: "project:" + key, kind: "biome", key: key, name: name, centre: { x: c.x, y: c.y }, region: regions[key], cell: { k: cellOf[key], c: ck.c, r: ck.r }, grid: f.grid,
        palette: h32(key, 2) % 8, seed: seed, plots: [], overflow: f.overflow, orchard: [], meadow: [], nursery: [], graveyard: [], needs: 0, home: null, pen: null,
        counts: { plans: g.plans.length, seeds: g.seeds.length, staked: g.staked.length, closed: g.closed.length, meadow: g.meadow.length, graves: g.graves.length } };
      b.x = b.region.x; b.y = b.region.y; b.w = b.region.w; b.h = b.region.h;   // the biome's rect is its region
      f.slots.forEach(function (s) {
        var p = f.free[s], r = slotRect(c, s, f.grid.g), plot = { id: p.id, kind: "plot", slot: s, at: slotAt(s), x: r.x, y: r.y, w: r.w, h: r.h, plan: p, project: key, needs: needsOf(p) };
        if (p.staked) { plot.staked = true; plot.seed = g.staked.filter(function (sd) { return sd.id === p.id; })[0]; }
        var pb = planBeds(p, plot, cm[p.id]); plot.beds = pb.beds; plot.crate = pb.crate; plot.moreRows = pb.moreRows; plot.overgrown = !!pb.crate; cells[p.id] = pb.cells;
        b.plots.push(plot); b.needs += plot.needs;
        if (!opts.readonly) m.slots[p.id] = { s: s, t: now };
      });
      /* the grid anchors the bands (shape), so a small biome stays compact; the lanes are ground, not in the index (29.3). The band
         items are placed on the g = 0 band (sh.base) and those east of the cut slid g east (slide), so none re-hashes when the
         farm steps (29.11) */
      var bx = sh.bx, bw = sh.base.bw, lxs = sh.base.laneX, orchardY = sh.bands.orchard.y, nurseryY = sh.bands.nursery.y, hs = farmstead(sh.farm, f.widths);
      var slide = function (x) { return x >= sh.cut ? x + sh.g : x; };
      b.bands = sh.bands; b.lanes = sh.lanes;
      if (b.overflow) { b.overflow.id = "overflow:" + key; b.overflow.kind = "overflow"; b.overflow.x = sh.overflow.x; b.overflow.y = sh.overflow.y; b.overflow.w = sh.overflow.w; b.overflow.h = sh.overflow.h; b.overflow.project = key; }
      /* orchard: one row of 1-tile trees at pitch 2; nursery: 3x3 pots at pitch 4x5, two rows, one per draft (a staked seed is on
         its plot); neither on a cell a lane crosses (X-D6) */
      var tcells = Math.floor(bw / BAND.treePitch), tused = laneCells(bx, tcells, BAND.treePitch, BAND.treePitch, lxs);
      g.closed.slice().sort(byId).slice(0, Math.min(CAPS.orchard, tcells)).forEach(function (c) {
        var cell = hashCell(c.id, tcells, tused); if (cell < 0) return;
        b.orchard.push({ id: c.id, kind: "tree", x: slide(bx + cell * BAND.treePitch + (h32(c.id, 4) % BAND.treePitch)), y: orchardY, w: 1, h: 1, plan: c, project: key });
      });
      /* the farmstead: a zone per cell; the Growing field's first slots hold the loose beads (by id), at most MEADOW_SHARE of
         them, the rest homeFill's (28.2, 28.12, 29.2) */
      b.home = { id: "home:" + key, kind: "home", project: key, x: hs.x, y: hs.y, w: hs.w, h: hs.h, cells: hs.cells, bay: hs.bay, keep: hs.keep, sign: hs.sign, cross: hs.cross, counts: (doc.home || {})[key] || {}, zones: {} };
      HOME.cells.forEach(function (c) { b.home.zones[c[0]] = c[0] === "pen" ? (b.pen = penOf(doc.wishes, key, hs.cells.pen)) : c[0] === "escalation" ? escOf(doc.escalations, key, hs.cells.escalation, hs.sign) : { id: b.home.id + "/" + c[0], kind: "zone", zone: c[0], project: key, x: hs.cells[c[0]].x, y: hs.cells[c[0]].y, w: hs.cells[c[0]].w, h: hs.cells[c[0]].h }; });
      /* two loose beads to a slot, a tile each, in halves q (slot q >> 1, east q & 1); a tended one takes a whole slot */
      var field = slotsOf(hs.cells.growing, "growing"), loose = 2 * looseCap(field.length), q = 0, full = false;
      g.meadow.slice().sort(byId).forEach(function (t) {
        var two = !!tends[t.id], at = two ? q + (q & 1) : q;
        if (full || at + (two ? 2 : 1) > loose) { full = true; return; }
        var r = field[at >> 1]; b.meadow.push({ id: t.id, kind: "flower", x: r.x + (at & 1), y: r.y, w: 1, h: 1, task: t, project: key, slot: at >> 1 });
        q = at + (two ? 2 : 1);
      });
      b.home.zones.growing.more = g.meadow.length - b.meadow.length;
      b.home.zones.growing.halves = q;   // the halves the Meadow takes, the first of homeFill's
      var pcols = Math.floor(bw / BAND.potPitch), prows = Math.floor(BAND.nurseryH / BAND.potH), pl = laneCells(bx, pcols, BAND.potPitch, 3, lxs), pused = {};
      for (var pc = 0; pc < pcols * prows; pc++) if (pl[pc % pcols]) pused[pc] = 1;
      g.seeds.filter(function (s) { return !isStaked(s); }).sort(byId).slice(0, Math.min(CAPS.nursery, pcols * prows)).forEach(function (s) {
        var cell = hashCell(s.id, pcols * prows, pused); if (cell < 0) return;
        b.nursery.push({ id: s.id, kind: "seed", x: slide(bx + (cell % pcols) * BAND.potPitch), y: nurseryY + Math.floor(cell / pcols) * BAND.potH + 1, w: 3, h: 3, seed: s, project: key });
      });
      /* graveyard: one 1-tile headstone per closed wish at pitch 2, newest first in reading order (so not hashed: a wish that
         closes takes the first stone and the rest step along); its id is wish:<id>, the key the camera jumps to */
      var gy = sh.bands.graveyard;
      if (gy) g.graves.slice().sort(function (a, c) { return (c.closed || 0) - (a.closed || 0) || byId(a, c); }).slice(0, f.graves).forEach(function (w, j) {
        b.graveyard.push({ id: "wish:" + w.id, kind: "stone", x: slide(bx + gy.at[j % gy.cols] * BAND.stonePitch), y: gy.y + Math.floor(j / gy.cols) * BAND.stonePitch, w: 1, h: 1, wish: w, project: key });
      });
      /* the sign is on the farmstead's north edge (29.2); used = everything placed, the plot area with its lane frame among it; foot =
         used padded by the terrain fringe's maximum */
      b.sign = sh.sign;
      var used = union(union(sh.plotArea, b.bands.orchard), b.bands.nursery);
      if (gy) used = union(used, gy);
      b.plots.forEach(function (p) { used = union(used, p); });
      if (b.overflow) used = union(used, b.overflow);
      b.used = used; b.foot = { x: used.x - 5, y: used.y - 5, w: used.w + 10, h: used.h + 10 };
      world.biomes.push(b);
      world.bbox = union(world.bbox, tiles(b.foot));
      index[b.id] = b;
      b.plots.forEach(function (p) { index[p.id] = p; p.beds.forEach(function (bed) { bed.cells.forEach(function (c) { index[c.id] = c; }); }); });
      b.orchard.concat(b.meadow, b.nursery, b.graveyard).forEach(function (it) { index[it.id] = it; });
      index[b.home.id] = b.home;
      Object.keys(b.home.zones).forEach(function (z) { index[b.home.zones[z].id] = b.home.zones[z]; });
      b.home.zones.escalation.items.forEach(function (it) { index[it.id] = it; });
      if (!opts.readonly && key) m.cells[key] = { k: cellOf[key], t: now };
    });
    /* bbox: every footprint, world px (the origin domain when empty); a goat is out when a plot of its kin is on the map */
    if (!world.biomes.length) world.bbox = domainRect(0, 0);
    world.biomes.forEach(function (b) { b.pen.items.forEach(function (it) { it.out = (it.wish.plots || []).some(function (pl) { var p = pl && index[pl.plan]; return !!p && p.kind === "plot"; }); index[it.id] = it; }); });
    world.index = index;
    if (!opts.readonly) { pruneMemo(m, now); world.memoChanged = JSON.stringify(m) !== before; world.cells = cells; world.cellsChanged = JSON.stringify(cells) !== JSON.stringify(cm); }
    return world;
  }
  /* For a chunk bake over rect (world px): a test for the tiles inside the fences of b's plots under it (paths and beds, which
     the plot paints whole), or null when no plot meets the rect. */
  function plotInside(b, rect) {
    var ps = b.plots.filter(function (p) { return hit(tiles(p), rect); });
    if (!ps.length) return null;
    return function (tx, ty) { for (var k = 0; k < ps.length; k++) { var p = ps[k]; if (tx > p.x && ty > p.y && tx < p.x + p.w - 1 && ty < p.y + p.h - 1) return true; } return false; };
  }
  /* Is tile (tx, ty) inside a biome's terrain: the used rect padded by 3 + 2 * noise tiles (section 3.3), an organic fringe. */
  function inFoot(b, tx, ty) {
    var u = b.used, dx = Math.max(u.x - tx, 0, tx - (u.x + u.w - 1)), dy = Math.max(u.y - ty, 0, ty - (u.y + u.h - 1));
    if (!dx && !dy) return true;
    var d = Math.max(dx, dy) + Math.min(dx, dy) * 0.5, n = (h32(tx + "," + ty, b.seed) % 1000) / 1000;
    return d <= 3 + 2 * n;
  }
  /* The two-level cull (section 6): biomes whose footprint meets the view, then their plots and band items, then, at levels 2
     and 3 only, the task cells of the visible plots. All rects are returned in world px. */
  function visible(world, view, level) {
    var out = { biomes: [], plots: [], items: [], tasks: [], overflow: [] };
    if (!world) return out;
    world.biomes.forEach(function (b) {
      if (!hit(tiles(b.foot), view)) return;
      out.biomes.push(b);
      b.plots.forEach(function (p) {
        if (!hit(tiles(p), view)) return;
        out.plots.push(p);
        if (level >= 2) p.beds.forEach(function (bed) { bed.cells.forEach(function (c) { if (hit(tiles(c), view)) out.tasks.push(c); }); });
      });
      if (b.overflow && hit(tiles(b.overflow), view)) out.overflow.push(b.overflow);
      if (level >= 1) b.orchard.concat(b.meadow, b.nursery, b.graveyard).forEach(function (it) { if (hit(tiles(it), view)) out.items.push(it); });
    });
    return out;
  }

  /* homeFill(world, rows, slots) -> {<biome id>: {mailbox, growing, barn}} (28.2, 28.12, H-D13), each {items, more}: rows.decision
     (letters; an escalation is escOf's, 28.13), rows.agent (gardenerField's stands) and rows.work (loose work, less one already a
     cell, a flower or a placed stand's plant) in their project's strip, else the Commons's. slots[key][zone][id] keeps a live
     item's slot; a newcomer takes the lowest free one. The field places the stands first, in their rows' (need) order, then the
     work, two to a slot (a tile each; slots[key].growing keeps its half, slot << 1 | east), which with the Meadow's flowers
     takes at most looseCap slots. When anything does not fit (layout's zones.growing.more
     too) the field's last slot is its "+N", growing.badge {x, y, w, h, slot, n}, n counting the stands and loose beads left out;
     more is those homeFill left out. The Barn places nothing (its bay is the truck's, 28.10). Each placed item is in world.index:
     {id, kind letter | stand | plant, zone, x, y, w, h, slot, project, item}; a plant is its tile, 1 wide. */
  var FILL = [["decision", "mailbox", "letter"], ["agent", "growing", "stand"], ["work", "growing", "plant"]], HOME_ZONES = ["mailbox", "growing", "barn"];
  function homeFill(world, rows, slots) {
    var out = {}, ix = world.index, by = {};
    (world.homeIds || []).forEach(function (id) { delete ix[id]; });
    world.homeIds = [];
    FILL.forEach(function (f) {
      ((rows && rows[f[0]]) || []).forEach(function (it) {
        if (!it || !it.id || (f[0] === "work" && ix[it.id]) || (f[0] === "decision" && it.type === "escalation")) return;   // an escalation is the Escalations cell's (28.13)
        var b = ix[it.g ? P.homeFor(it.g) : "project:" + projectKey(it.project)] || ix["project:"];
        if (b && b.kind === "biome") ((by[b.id] = by[b.id] || {})[f[0]] = by[b.id][f[0]] || []).push(it);
      });
    });
    world.biomes.forEach(function (b) {
      var o = out[b.id] = {}, sk = slots[b.key] = slots[b.key] || {}, rb = by[b.id] || {};
      HOME_ZONES.forEach(function (z) {
        var pos = z === "barn" ? [] : slotsOf(b.home.cells[z], z, [b.home.keep[z], b.home.sign]), a = sk[z] = sk[z] || {}, used = {}, keep = {}, placed = [];
        var gr = z === "growing", m = gr ? b.home.zones.growing.halves | 0 : 0, fixed = Math.ceil(m / 2), n = pos.length, half = {};
        /* take(list, kind, cap, max, avoid, halves) -> how many of list it left out: up to max of them, the sticky first, then the
           rest to the lowest free place under cap, avoid's last. A place is a slot, or with halves a slot's west or east tile (q,
           slot q >> 1): the field's loose beads go two to a slot, a stand takes a whole one (28.12). */
        var take = function (list, kind, cap, max, avoid, halves) {
          var k = 0, free = halves ? function (q) { return q >= m && !half[q] && !(used[q >> 1] && q >> 1 >= fixed); } : function (q) { return !used[q]; };
          var put = function (it, q) { a[it.id] = q; keep[it.id] = 1; if (halves) half[q] = 1; else used[q] = 1; placed.push([halves ? q / 2 : q, it, kind, halves ? q & 1 : 0]); k++; };
          var pend = list.filter(function (it) { var q = a[it.id]; if (k < max && q !== undefined && q < cap && free(q)) { put(it, q); return false; } return true; });
          pend.forEach(function (it) {
            if (k >= max) return;
            var s = -1; for (var q = 0; q < cap && s < 0; q++) if (free(q) && !(avoid && avoid[q])) s = q;
            for (q = 0; q < cap && s < 0; q++) if (free(q)) s = q;
            if (s >= 0) put(it, s);
          });
          return list.length - k;
        };
        for (var i = 0; i < fixed; i++) used[i] = 1;
        var stands = z === "growing" ? rb.agent || [] : [], plants = {};
        stands.forEach(function (it) { if (it.task) plants[it.task] = 1; });
        var work = z === "growing" ? (rb.work || []).filter(function (it) { return !plants[it.id]; }) : z === "mailbox" ? rb.decision || [] : [];
        var room = n - fixed, lc = gr ? Math.max(0, 2 * looseCap(n) - m) : n, held = {};
        var spare = gr ? 2 * (room - stands.length) + (m & 1) : room;   // the places the work may take past the stands: halves in the field
        var badge = gr && n > fixed && (stands.length > room || work.length > Math.min(lc, spare) || b.home.zones.growing.more > 0);
        var cap = badge ? n - 1 : n, sn = Math.min(stands.length, cap - fixed), more = stands.length - sn;
        work.forEach(function (it) { if (a[it.id] !== undefined) held[gr ? a[it.id] >> 1 : a[it.id]] = 1; });   // a newcomer stand moves no loose bead while there is room
        more += take(stands.slice(0, sn), "stand", cap, sn, held);   // need order picks who stands: a crash or an ask is never behind the badge
        more += gr ? take(work, "plant", 2 * cap, lc, null, true) : take(work, "letter", cap, lc);
        Object.keys(a).forEach(function (id) { if (!keep[id]) delete a[id]; });
        o[z] = { items: placed.sort(function (p, q) { return p[0] - q[0]; }).map(function (e) {
          var s = Math.floor(e[0]), r = pos[s], lb = e[2] === "plant", h = { id: e[1].id, kind: e[2], zone: z, x: r.x + e[3], y: r.y, w: lb ? 1 : r.w, h: r.h, slot: s, project: b.key, item: e[1] };
          ix[h.id] = h; world.homeIds.push(h.id);
          return h;
        }), more: more };
        if (gr) o[z].stands = stands.length;   // every stand, placed or behind the "+N": the field's "N working" (28.12)
        if (badge) { var br = pos[n - 1]; o[z].badge = { x: br.x, y: br.y, w: br.w, h: br.h, slot: n - 1, n: more + (b.home.zones.growing.more | 0) }; }
      });
    });
    return out;
  }

  /* ---------- identity, deep links and search (section 7) ---------- */
  /* `at=<project>[/<id>]`: each segment is URL-encoded; "~" is the Commons; an id of home, mailbox, escalation, growing, barn or pen is that
     project's farmstead or courtyard cell (28.7, 29.6: no bead id is one, having a prefix and a dash). parseAt decodes one. */
  function decodeSeg(s) { try { return decodeURIComponent(s); } catch (e) { return s; } }
  function encodeSeg(s) { return encodeURIComponent(s); }
  function parseAt(at) {
    at = String(at === undefined || at === null ? "" : at).trim();
    if (!at) return null;
    var i = at.indexOf("/"), p = decodeSeg(i < 0 ? at : at.slice(0, i)).trim(), id = i < 0 ? "" : decodeSeg(at.slice(i + 1)).trim();
    return { raw: p, project: p === "~" ? "" : projectKey(p), id: id };
  }
  /* The query string of a URL with ?at= and &z= replaced (or removed when at is empty); other parameters are kept as written. */
  function parseQuery(search) {
    var out = [];
    String(search || "").replace(/^\?/, "").split("&").forEach(function (kv) { if (!kv) return; var i = kv.indexOf("="); out.push([decodeSeg(i < 0 ? kv : kv.slice(0, i)), i < 0 ? "" : kv.slice(i + 1)]); });
    return out;   // [key, rawValue] pairs, in order
  }
  function queryGet(search, key) { var q = parseQuery(search); for (var i = 0; i < q.length; i++) if (q[i][0] === key) return q[i][1]; return null; }
  function linkQuery(search, at, level) {
    var parts = [];
    parseQuery(search).forEach(function (kv) { if (kv[0] !== "at" && kv[0] !== "z") parts.push(encodeSeg(kv[0]) + (kv[1] ? "=" + kv[1] : "")); });
    if (at) { parts.push("at=" + at); if (level !== undefined && level !== null) parts.push("z=" + level); }
    return parts.length ? "?" + parts.join("&") : "";
  }
  /* z= is a level 0..3, never a raw zoom (section 7); anything else is "the target's natural level". */
  function levelParam(v) { var n = parseInt(v, 10); return isNaN(n) || n < 0 || n > 3 || String(v).trim() === "" ? null : n; }
  function projectOf(index, it) { return it.kind === "biome" ? it.key : it.project !== undefined ? it.project : index[it.plot] ? index[it.plot].project : undefined; }
  /* The at= value for a placed thing, or null when it is not in the world (28.7, 29.6): a farmstead is <project>/home, a zone its word, a
     field stand its field's (<project>/growing, 28.12). */
  function atFor(world, kind, id) {
    var index = world && world.index || {}, it = index[id];
    if (!it) return null;
    if (it.kind === "biome") return it.key === "" ? "~" : encodeSeg(it.key);
    var key = projectOf(index, it);
    if (key === undefined) return null;
    return (key === "" ? "~" : encodeSeg(key)) + "/" + (it.kind === "home" ? "home" : it.zone && it.kind !== "letter" && it.kind !== "plant" && it.kind !== "escalation" ? it.zone : encodeSeg(id));
  }
  var NATURAL = { biome: 1, home: 1, zone: 2, plot: 2, overflow: 2, phase: 2, task: 3, tree: 3, flower: 3, seed: 3, stone: 3, pen: 2, goat: 3, letter: 3, plant: 3, stand: 3, escalation: 3, truck: 2 };   // truck: web/truck.js
  var ZONE_WORD = { home: 1, mailbox: 1, escalation: 1, growing: 1, barn: 1, pen: 1 };
  /* resolve(world, at) -> {kind, id, rect (tiles), level, biome, found, missing, moved} or null. An id finds its rectangle through
     the index; a phase id its bed; a plan past the plot cap the overflow marker; a zone word its farmstead or courtyard cell. An unknown id falls
     back to its project with found=false and missing=<id>; a single segment that is not a project is tried as an id; an unknown
     project is nothing. A legacy yard link (H-D12) lands on the Commons's farmstead or pen, or the thing named, with moved "yard". */
  function resolve(world, at) {
    var q = typeof at === "string" ? parseAt(at) : at;
    if (!q || !world) return null;
    var index = world.index || {}, biome = index["project:" + q.project] || null;
    var rectOf = function (it) { var r = it.kind === "biome" ? it.used : it; return { x: r.x, y: r.y, w: r.w, h: r.h }; };
    var hit1 = function (kind, id, rect, level, b, missing) { return { kind: kind, id: id, rect: rect, level: level, biome: b || null, found: !missing, missing: missing || "" }; };
    var zone = function (b, w) { var it = b && index["home:" + b.key + (w === "home" ? "" : "/" + w)]; return it ? hit1(it.kind, it.id, rectOf(it), NATURAL[it.kind], b) : null; };
    if (!biome && q.raw.toLowerCase() === "yard") {
      var cm = index["project:"], pen = cm && cm.pen && cm.pen.items.length ? "pen" : "home", r0 = q.id && q.id !== "pen" ? resolve(world, q.id) : null;
      r0 = r0 || zone(cm, q.id === "pen" ? pen : "home");
      if (r0) r0.moved = "yard";
      return r0;
    }
    if (biome && ZONE_WORD[q.id]) return zone(biome, q.id);
    var byId = function (id) {
      var it = index[id];
      if (it) return hit1(it.kind, it.id, rectOf(it), NATURAL[it.kind] || 3, it.kind === "biome" ? it : index["project:" + projectOf(index, it)]);
      var bs = biome ? [biome] : world.biomes;
      for (var i = 0; i < bs.length; i++) {
        var b = bs[i];
        for (var j = 0; j < b.plots.length; j++) { var p = b.plots[j]; for (var k = 0; k < p.beds.length; k++) if (p.beds[k].id && p.beds[k].id === id) { var r = hit1("phase", id, rectOf(p.beds[k]), 2, b); r.plot = p.id; return r; } }
        if (b.overflow && b.overflow.ids.indexOf(id) >= 0) return hit1("overflow", b.overflow.id, rectOf(b.overflow), 2, b);
      }
      return null;
    };
    if (q.id) { var r = byId(q.id); if (r) return r; return biome ? hit1("biome", biome.id, rectOf(biome), 1, biome, q.id) : null; }
    if (biome) return hit1("biome", biome.id, rectOf(biome), 1, biome);
    return q.raw ? byId(q.raw) : null;
  }
  /* What the camera is over at world px (x, y) for a level: the task cell (Bead), plot or farmstead (Plan and in) or biome under
     the point, else null (the wild). Gives the at= the page writes back into the URL after the camera settles. */
  function locate(world, x, y, level) {
    if (!world) return null;
    var tx = x / TILE, ty = y / TILE, inside = function (r) { return tx >= r.x && tx < r.x + r.w && ty >= r.y && ty < r.y + r.h; };
    for (var i = 0; i < world.biomes.length; i++) {
      var b = world.biomes[i];
      if (!inside(b.foot)) continue;
      if (level >= 2 && inside(b.home)) return { at: atFor(world, "home", b.home.id), kind: "home", id: b.home.id };
      if (level >= 2) for (var j = 0; j < b.plots.length; j++) {
        var p = b.plots[j];
        if (!inside(p)) continue;
        if (level >= 3) for (var k = 0; k < p.beds.length; k++) for (var c = 0; c < p.beds[k].cells.length; c++) if (inside(p.beds[k].cells[c])) return { at: atFor(world, "task", p.beds[k].cells[c].id), kind: "task", id: p.beds[k].cells[c].id };
        return { at: atFor(world, "plot", p.id), kind: "plot", id: p.id };
      }
      return { at: atFor(world, "biome", b.id), kind: "biome", id: b.id };
    }
    return null;
  }

  /* ---------- the jump index (section 7): {kind, id, title, project, key} for every placed thing, built once per layout ----------
     Each biome's home strip (28.7), its pen and goats and its graveyard are in it too (3.3, gd-NbWjIdLTnd), so a phone reaches
     them from the camera column's Go: bead is the wish's own id (an exact id finds its goat or stone), aka words the row answers
     to besides its title, note the count or the goat's state the row says. With st (gardenerState's), every field stand is in it
     under its field's project, found by its agent's name or bead id (28.12). */
  var KIND_RANK = { biome: 0, home: 1, pen: 2, plot: 3, stand: 4, task: 5, escalation: 6, seed: 7, tree: 8, flower: 9, goat: 10, graveyard: 11, stone: 12 };
  /* homeWords(c) -> the home blob's counts in words (28.6): working the field's stands, field its loose beads (28.12); the
     Barn holds no figures, so it has no count here. */
  function homeWords(c) {
    c = c || {};
    return [c.letters ? plural(c.letters, "letter", "letters") : "", ESC.count(c.escalations | 0), c.working ? c.working + " working" : "", c.field ? c.field + " growing" : "", c.wishes ? plural(c.wishes, "open wish", "open wishes") : ""].filter(Boolean).join(" · ");
  }
  function searchIndex(world, st) {
    var out = [];
    if (!world) return out;
    world.biomes.forEach(function (b, n) {
      var pen = b.pen;
      out.push({ kind: "biome", id: b.id, title: b.name, project: b.name, key: b.key, n: n });
      var hc = b.home.counts, fs = st && st[3].fields[b.id] || [];
      out.push({ kind: "home", id: b.home.id, title: b.name + " home", project: b.name, key: b.key, n: n, aka: "yard mailbox barn letters", note: homeWords({ letters: hc.letters, working: fs.length, field: hc.field, wishes: hc.wishes }) });
      fs.forEach(function (id) { var g = st.byId[id], sp = st[3].spots[id]; out.push({ kind: "stand", id: P.FIELD + id, bead: id, title: g.agent || id, project: b.name, key: b.key, aka: "gardener agent", note: P.standWord(sp.field.task && (g.task_title || g.tends_title || sp.field.task)) }); });
      if (pen.items.length) {
        out.push({ kind: "pen", id: pen.id, title: b.name + " pen", project: b.name, key: b.key, aka: "goats open wishes", note: plural(pen.items.length + pen.more, "open wish", "open wishes") });
        pen.items.forEach(function (it) { var w = it.wish; out.push({ kind: "goat", id: it.id, bead: w.id, title: w.title || w.id, project: b.name, key: b.key, note: it.out ? "out grazing by its plan" : w.tending ? "tended" : "untended" }); });
      }
      b.home.zones.escalation.items.forEach(function (it) { var w = it.wait; out.push({ kind: "escalation", id: it.id, bead: w.id, title: w.title || w.id, project: b.name, key: b.key, aka: "escalation " + ESC.noun(w), note: ESC.word(w) }); });
      if (b.graveyard.length) out.push({ kind: "graveyard", id: "band:" + b.key + "/graveyard", title: "Graveyard", project: b.name, key: b.key, aka: "headstones closed wishes", note: plural(b.graveyard.length, "closed wish", "closed wishes") });
      b.plots.forEach(function (p) {
        out.push({ kind: "plot", id: p.id, title: p.plan.title || p.id, project: b.name, key: b.key, state: p.plan.state || "" });
        p.beds.forEach(function (bd) { bd.cells.forEach(function (c) { out.push({ kind: "task", id: c.id, title: c.task.title || c.id, project: b.name, key: b.key, state: c.task.state || "", plan: p.plan.title || p.id }); }); });
      });
      b.orchard.forEach(function (t) { out.push({ kind: "tree", id: t.id, title: t.plan.title || t.id, project: b.name, key: b.key, state: t.plan.state || "closed" }); });
      b.meadow.forEach(function (f) { out.push({ kind: "flower", id: f.id, title: f.task.title || f.id, project: b.name, key: b.key, state: f.task.state || "" }); });
      b.nursery.forEach(function (s) { out.push({ kind: "seed", id: s.id, title: s.seed.title || s.id, project: b.name, key: b.key, state: s.seed.state || "" }); });
      b.graveyard.forEach(function (g) { out.push({ kind: "stone", id: g.id, bead: g.wish.id, title: g.wish.title || g.wish.id, project: b.name, key: b.key, state: g.wish.outcome || "" }); });
    });
    return out;
  }
  /* Rank the index for a query: an exact id (a goat's or stone's is its wish's bead id, never its "wish:" key), then an id
     prefix, an exact or prefix title, a word prefix (of the title or of a band's aka words), any substring, and for several
     words all of them somewhere in the title, id, aka or project. Ties go by kind (projects, home strips, pens, plans, beads,
     goats, graveyards, stones) then title then id, projects and home strips in world order. An empty query lists the projects and
     their home strips, led by the home strip whose id is lead (the cursor's project's, a phone's way home: 28.6, gd-cTKXpm8JH7)
     and then that project's field stands, at most LEAD_STANDS of them (a phone's way to a gardener: 28.12, gd-a628FpJRO5).
     At most cap (8) results. */
  var HOMEISH = { biome: 1, home: 1 }, LEAD_STANDS = 5;
  function search(idx, q, cap, lead) {
    cap = cap || 8; q = String(q || "").trim().toLowerCase().replace(/\s+/g, " ");
    var hits = [], words = q.split(" "), stands = 0, lk = lead && lead.indexOf("home:") === 0 ? lead.slice(5) : null;
    (idx || []).forEach(function (e) {
      var s = -1, id = String(e.bead || e.id).toLowerCase(), t = String(e.title || "").toLowerCase(), pj = String(e.project || "").toLowerCase(), aka = " " + String(e.aka || "").toLowerCase();
      if (!q) s = e.kind in HOMEISH ? (e.id === lead ? 0 : 1) : e.kind === "stand" && lk && e.key === lk && stands++ < LEAD_STANDS ? 0 : -1;
      else if (id === q) s = 0;
      else if (id.indexOf(q) === 0) s = 1;
      else if (t === q || t.indexOf(q) === 0) s = 2;
      else if (t.indexOf(" " + q) >= 0 || aka.indexOf(" " + q) >= 0) s = 3;
      else if (t.indexOf(q) >= 0 || id.indexOf(q) >= 0) s = 4;
      else if (words.length > 1 && words.every(function (w) { return t.indexOf(w) >= 0 || id.indexOf(w) >= 0 || pj.indexOf(w) >= 0 || aka.indexOf(" " + w) >= 0; })) s = 5;
      else if (e.kind === "plot" && pj === q) s = 6;
      if (s >= 0) hits.push({ e: e, s: s, i: hits.length });
    });
    var proj = function (a, b) { return a.e.kind in HOMEISH && b.e.kind in HOMEISH ? a.e.n - b.e.n : !q && a.e.kind === "stand" && b.e.kind === "stand" ? a.i - b.i : 0; };   // a home strip after its own project, in world order; the lead's stands in their field's order
    hits.sort(function (a, b) {
      return a.s - b.s || proj(a, b) || (KIND_RANK[a.e.kind] === undefined ? 9 : KIND_RANK[a.e.kind]) - (KIND_RANK[b.e.kind] === undefined ? 9 : KIND_RANK[b.e.kind]) ||
        (a.e.title < b.e.title ? -1 : a.e.title > b.e.title ? 1 : 0) || (a.e.id < b.e.id ? -1 : a.e.id > b.e.id ? 1 : 0);
    });
    return hits.slice(0, cap).map(function (h) { return h.e; });
  }

  /* ---------- accessibility (section 8) ---------- */
  /* outline(world) -> the accessible model: a project > plan > bead tree that mirrors World exactly. Roots are the biomes in
     world order; a biome's children are its grid's cells in grid order, the farmstead among its plots (29.6: its zones in the
     courtyard's order, the Growing field with the loose beads, the Pen the goats; a plot its tasks in bed order), the overflow, its seeds,
     harvested plans and closed wishes' headstones. Every id in layout()'s world.index appears once, with the same
     `kind:id` the hot links use, so an outline entry can hand focus to its hot. Pure, so the Go test runs it under node. */
  var NATURAL_LEVEL = { biome: 1, home: 1, zone: 2, pen: 2, plot: 2, overflow: 2, task: 3, seed: 3, tree: 3, flower: 3, stone: 3, goat: 3, escalation: 3 };
  function byPos(a, b) { return a.y - b.y || a.x - b.x || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); }
  function outline(world) {
    var roots = [], byRect = function (a, b) { return byPos(a.rect, b.rect); };
    if (!world) return roots;
    var entry = function (kind, id, title, rect, extra) {
      var e = { kind: kind, id: id, hot: kind + ":" + id, title: title || id, level: NATURAL_LEVEL[kind], rect: { x: rect.x, y: rect.y, w: rect.w, h: rect.h } };
      if (extra) Object.keys(extra).forEach(function (k) { e[k] = extra[k]; });
      return e;
    };
    world.biomes.forEach(function (b) {
      var kids = [], zs = b.home.zones, zk = { growing: b.meadow.slice().sort(byPos).map(function (f) { return entry("flower", f.id, f.task.title, f, { state: f.task.state || "", assignee: f.task.assignee || "" }); }),
        pen: b.pen.items.map(function (it) { return entry("goat", it.id, it.wish.title, it, { state: it.out ? "out" : "" }); }),
        escalation: zs.escalation.items.map(function (it) { return entry("escalation", it.id, it.wait.title || it.wait.id, it, { state: it.wait.state || "", family: ESC.family(it.wait) }); }) };
      kids.push(entry("home", b.home.id, "Home", b.home, { children: HOME.cells.map(function (c) { var z = zs[c[0]]; return entry(z.kind, z.id, c[0], z, { children: zk[c[0]] || [] }); }).sort(byRect) }));
      b.plots.forEach(function (p) {
        var tasks = [];
        p.beds.forEach(function (bed) { bed.cells.forEach(function (c) { tasks.push(entry("task", c.id, c.task.title, c, { state: c.task.state || "", type: c.task.type || "", phase: bed.title, assignee: c.task.assignee || "" })); }); });
        kids.push(entry("plot", p.id, p.plan.title, p, { needs: p.needs, counts: p.plan.counts || {}, slot: p.slot, children: tasks }));
      });
      kids.sort(byRect);   // the farmstead and the plots share one grid and pitch, so a row's cells share a y
      if (b.overflow) kids.push(entry("overflow", b.overflow.id, "+" + b.overflow.n + " more plans", b.overflow, { n: b.overflow.n, project: b.key }));
      b.nursery.slice().sort(byPos).forEach(function (s) { kids.push(entry("seed", s.id, s.seed.title, s, { state: s.seed.state || "" })); });
      b.orchard.slice().sort(byPos).forEach(function (t) { kids.push(entry("tree", t.id, t.plan.title, t, { state: t.plan.state || "closed" })); });
      b.graveyard.forEach(function (g) { kids.push(entry("stone", g.id, g.wish.title, g, { state: g.wish.outcome || "" })); });   // already in reading order
      roots.push(entry("biome", b.id, b.name, b.used, { key: b.key, needs: b.needs, counts: b.counts, children: kids, hot: "sign:" + b.id }));
    });
    return roots;
  }
  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }
  /* wantHot(h, level, z, coarse) -> is this hot link in the DOM at this level? Levels first (h.lv is a bitmask of levels; strip
     beads are Bead-level only, except decisions which show from Plan). On a coarse pointer a one-tile thing under MIN_HOT CSS px
     is not a target (section 8): the plot stands in for its tasks until a tile is wide enough, and the outline keeps every one
     reachable. Fine kinds are the single-tile sprites; a seed pot is three tiles and clears the bar at the Bead level. */
  var MIN_HOT = 44, FINE = { task: 1, tree: 1, flower: 1, gardener: 1, gmore: 1, gask: 1 };   // a steward is not fine: it stands alone by its sign, so its padded hit box crowds nothing, and a tap must reach it (Lands 9.8)
  function wantHot(h, level, z, coarse) {
    if (coarse && FINE[h.kind] && Math.min(h.w, h.h) * z < MIN_HOT) return false;
    if (h.lv !== undefined) {
      if (h.lv & (1 << level)) return true;
      return !!(coarse && h.kind === "plot" && level === 3 && TILE * z < MIN_HOT);
    }
    return h.kind !== "bead" || level >= 3 || (level === 2 && h.zone === "decision");
  }
  /* The gardeners on the map, their words and the stewards at their homesteads (web/people.js), built on this file's grid and
     words; WORLD hands their names out as its own. */
  var P = (typeof PEOPLE !== "undefined" ? PEOPLE : require("./people.js"))({ TILE: TILE, HOME: HOME, SLOT: SLOT, h32: h32,
    hit: hit, cellAt: cellAt, tile: tile, atFor: atFor, projectKey: projectKey, projectOf: projectOf, byId: byId, plural: plural });
  var api = {
    TILE: TILE, DOM: DOM, Z_MIN: Z_MIN, Z_MAX: Z_MAX, SNAPS: SNAPS, EDGES: EDGES, LEVEL_NAMES: LEVEL_NAMES, CHUNK_PX: CHUNK_PX,
    h32: h32, lodFor: lodFor, levelRange: levelRange, clampZ: clampZ, snapZ: snapZ, snapNext: snapNext,
    toWorld: toWorld, toScreen: toScreen, viewRect: viewRect, clampCam: clampCam, zoomAt: zoomAt, panBy: panBy, fit: fit,
    hit: hit, cull: cull, nearest: nearest, chunkSpan: chunkSpan, chunks: chunks, lru: lru, domainRect: domainRect,
    isStaked: isStaked, stakedPlan: stakedPlan, SLOT: SLOT, SLOT_CAP: SLOT_CAP, SPIRAL: SPIRAL, LANE: LANE, BED: BED, BAND: BAND, CAPS: CAPS, GAP: GAP, PEN: PEN, HOME: HOME, MEADOW_SHARE: MEADOW_SHARE, COMMONS_K: COMMONS_K, FIRST_K: FIRST_K, penOf: penOf, escOf: escOf, escTotal: escTotal, farmstead: farmstead, homeW: homeW, homeWidths: homeWidths, homeCounts: homeCounts, ladderOf: ladderOf, capAt: capAt, laneCells: laneCells, slotsOf: slotsOf, homeFill: homeFill, projectOf: projectOf, homeWords: homeWords, slotRect: slotRect, shape: shape, regionSize: regionSize, tiles: tiles, union: union, projectKey: projectKey,
    LATTICE_W: LATTICE_W, latticeAt: latticeAt, latticeIndex: latticeIndex, slotAt: slotAt, slotIndex: slotIndex, bedAt: bedAt, cellAt: cellAt, cellIndex: cellIndex, planBeds: planBeds, plotRollup: plotRollup, plotHeads: plotHeads, needsOf: needsOf, openOf: openOf, tile: tile, board: board, boardWords: boardWords, onWords: onWords,
    emptyMemo: emptyMemo, layout: layout, inFoot: inFoot, plotInside: plotInside, visible: visible,
    parseAt: parseAt, parseQuery: parseQuery, queryGet: queryGet, linkQuery: linkQuery, levelParam: levelParam, atFor: atFor, resolve: resolve, locate: locate,
    searchIndex: searchIndex, search: search,
    NATURAL_LEVEL: NATURAL_LEVEL, MIN_HOT: MIN_HOT, outline: outline, wantHot: wantHot
  };
  Object.keys(P).forEach(function (k) { api[k] = P[k]; });
  return api;
})();
if (typeof module !== "undefined") module.exports = WORLD;
