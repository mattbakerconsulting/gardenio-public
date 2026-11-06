/* EDGES (Homestead World phase 3, gd-CNhEh30Ykf): a step waiting on a bead in another project is drawn as an irrigation
   channel between the two regions. Water would run from the blocker into the step that waits on it, so the channel's sluice
   gate (a wooden board across it) sits at the waiting end, shut until the blocker closes.
   The data is the farm's blocked_by (open blockers only, capped server side) plus blocked_by_project, the project of each
   blocker whose label differs from the step's, so a blocker the document does not carry (past a cap, inside a seed) still
   has a region. A region is where a thing is drawn: a step's is its plan's, whatever its own label says.
   Per level the channel joins regions (World) or plots (Project, Plan, Bead); channels with the same two ends merge, at most
   MAX_CHANNELS are drawn, and each is routed square through grass, never across a plot: out of its plot into the gutter
   between plot rows (or columns), along it out of the region, across the gap between the two regions, and in the same way
   into the plot that waits. Channels sharing a run are laid side by side. State is never colour alone: the bead view and the hot links' accessible
   names say "Blocked by <id> in <project>".
   A project-scoped viewer's document never carries a blocker they cannot read: the server cuts it to its project's name in
   blocked_by_foreign (gd-U2pbNv3lLC), "" for an unlabelled one. Such an edge has no blocker id and no region on the map, so it
   links nowhere and reads "Blocked by a bead in <project>" ("another project" for ""). It is drawn as a bridge (gd-DgEhq38EnY):
   a short channel from the step that waits out of its region by the nearest side no other region lies beyond, under a
   wooden footbridge at its far end and named there for the project, so it never reaches a neighbour's region; the foreign
   edges of one plot (one region at the World level) share one bridge, named for all of them.
   Inside a field (gd-Y4gXDBblIG, gd-osWVpshPrj) a step's blockers on its plot are thin channels: 2 px of water in a 1 px bank
   (a blue halo by night), from the blocker's centre out to its edge, along the seams between the tiles past the cells in the
   way, and in from the waiting cell's edge to its centre, with no gate; comet dashes step toward the step that waits. They
   lie under the plants at Bead and over the flat squares at Plan, and never cross another cell. index(), route(),
   fieldRoute(), lines() and aria() are pure;
   draw(), drawField() and drawLegend() use only fillStyle and fillRect, and drawNames() drawImage (off the page not at all),
   so they run under the node canvas mock. scene.js and garden.js keep the hooks. */
var EDGES = (function () {
  "use strict";
  var MAX_CHANNELS = 48;   // channels drawn per level, most edges first; the rest still read in words
  var LINES_MAX = 8;       // edge lines in one bead view; the rest are counted
  var ARIA_MAX = 3;        // edges named in one accessible name
  var LINE_PX = 8;         // the channel's width on screen, at every zoom (below the World level at most MAX_W tiles)
  var MAX_W = 1.6;         // tiles: the widest channel that fits the 3-tile gutter between plots (WORLD.SLOT pitch 35, plot 32)
  var GUTTER = 1;          // tiles from a plot's edge to the middle of the gutter beside it
  var STUB = 4;            // tiles a bridge's channel runs past its region's foot: short of the middle of the WORLD.GAP (16) to a neighbour
  var NAME_PX = 11;        // a bridge's name, bold, in CSS px at every zoom
  var FIELD_MAX = 12;      // channels drawn inside one plot, in document order
  var FIELD_W = 1 / 8;     // tiles: the water of a channel inside a field (2 px of a 16 px tile), in a bank half as wide each side
  var FIELD_P = 5 / 16;    // tiles: the pitch of a field channel's comets, which step a sixteenth a frame
  var DAY = { bank: "#2a1a14", water: "#3a8fd6", ripple: "#bfe6ff", gate: "#9a6630", mid: "#7fc0ea", halo: "#2a1a14" };
  var NIGHT = { bank: "#0b0e14", water: "#5ab0ff", ripple: "#d8eeff", gate: "#b07a3c", mid: "#8fd0ff", halo: "#1e3a5a" };   // brighter: the night ground is dark teal; a field channel's bank a blue halo
  var NAME = { day: ["#2a1a14", "#f4ecd8"], night: ["#f2f2ee", "#0b0e14"] };   // a bridge's name: its ink over a halo

  function key(p) { return String(p || "").trim().toLowerCase(); }   // WORLD.projectKey: the biome a project names
  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }
  function words(list) { return list.length < 2 ? list.join("") : list.slice(0, -1).join(", ") + " and " + list[list.length - 1]; }

  /* Every thing the document carries, by id: {project (its region's key), plan (the plot it is drawn on, "" if none), title}. */
  function places(doc) {
    var at = {};
    (doc.plans || []).forEach(function (p) {
      var k = key(p.project);
      at[p.id] = { key: k, plan: p.id, title: p.title };
      (p.phases || []).concat(p.unphased ? [p.unphased] : []).forEach(function (ph) {
        (ph.tasks || []).forEach(function (t) { at[t.id] = { key: k, plan: p.id, title: t.title, task: t }; });
      });
    });
    (doc.seeds || []).forEach(function (s) { at[s.id] = { key: key(s.project), plan: "", title: s.title }; });
    ((doc.meadow || {}).shown || []).forEach(function (t) { at[t.id] = { key: key(t.project), plan: "", title: t.title, task: t }; });
    return at;
  }
  /* The region key of an item the layout placed. */
  function regionOf(index, it) { return it.kind === "biome" ? it.key : it.project !== undefined ? it.project : index[it.plot] ? index[it.plot].project : undefined; }

  /* edges(doc, world) -> [Edge], every blocked_by edge whose two ends lie in different regions, in document order.
     Edge: { task, title (the waiting step's), plan (its plot, "" for a loose bead), from (its region key), blocker,
     blockerTitle ("" when the document does not carry it), to (the blocker's region key), project (the blocker's region as
     people say it), drawn (both regions are on the map; for a foreign edge, foreign: true, the waiting one) }. */
  function edges(doc, world) {
    var out = [];
    if (!doc) return out;
    var at = places(doc), index = (world && world.index) || {};
    var name = function (k, raw) { var b = index["project:" + k]; return b ? b.name : String(raw || "").trim() || "no project"; };
    Object.keys(at).forEach(function (id) {
      var w = at[id], t = w.task;
      if (!t || !(t.blocked_by || []).length && !(t.blocked_by_foreign || []).length) return;
      var extra = t.blocked_by_project || {};
      (t.blocked_by || []).forEach(function (bid) {
        var it = index[bid], b = at[bid], raw = Object.prototype.hasOwnProperty.call(extra, bid) ? extra[bid] : t.project;
        var to = it ? regionOf(index, it) : b ? b.key : key(raw);
        if (to === undefined) to = key(raw);
        if (to === w.key) return;
        out.push({ task: id, title: t.title || id, plan: w.plan, from: w.key, blocker: bid, blockerTitle: b ? b.title || "" : "", to: to,
          project: name(to, raw), fromName: name(w.key, t.project), drawn: !!(index["project:" + w.key] && index["project:" + to]) });
      });
      (t.blocked_by_foreign || []).forEach(function (raw) {
        out.push({ task: id, title: t.title || id, plan: w.plan, from: w.key, blocker: "", blockerTitle: "", to: key(raw),
          project: String(raw || "").trim() || "another project", fromName: name(w.key, t.project), foreign: true, drawn: !!index["project:" + w.key] });
      });
    });
    return out;
  }

  /* The rect (tiles) and anchor id an end of an edge is drawn at: the middle of its region at the World level (lv 0, a point,
     so the channel reads across the whole region), its plot, flower or pot below that, else its region. reg is the region's
     foot, which the route crosses between. */
  function anchor(index, id, plan, k, lv) {
    var b = index["project:" + k];
    if (!b) return null;
    if (lv > 0) {
      var it = index[id];
      if (it && it.kind === "task") it = index[it.plot];
      if (!it && plan) it = index[plan];
      if (it && it.kind !== "biome") return { id: it.id, r: { x: it.x, y: it.y, w: it.w, h: it.h }, reg: b.foot };
    }
    var u = b.used;
    return { id: b.id, r: lv > 0 ? b.foot : { x: u.x + u.w / 2, y: u.y + u.h / 2, w: 0, h: 0 }, reg: b.foot };
  }

  /* route(s, d, sr, dr) -> { axis: "h" | "v", pts: [[x, y], ...], sides: [start, end] } or null when the regions overlap. s and
     d are the two ends' rects (tiles), sr and dr their regions' (an end that is its region passes the same rect, an end at a
     point a rect with no size: either starts straight off, from its facing side or the point). Regions side
     by side ("h"): an end inside its region steps out of its top or bottom (the side facing the other end) into the gutter
     beside it and runs along that gutter; the route turns at the middle of the gap between the regions. One above the other
     ("v"): the same with x and y swapped. Diagonal neighbours take the axis with the wider gap. sides[i] is the axis an end may
     slide along its rect's side ("x" on a top or bottom side, "y" on a left or right one). Water runs s -> d. */
  function route(s, d, sr, dr) {
    sr = sr || s; dr = dr || d;
    var gx = Math.max(dr.x - (sr.x + sr.w), sr.x - (dr.x + dr.w)), gy = Math.max(dr.y - (sr.y + sr.h), sr.y - (dr.y + dr.h));
    if (gx <= 0 && gy <= 0) return null;
    var h = gx >= gy, X = h ? 0 : 1, Y = 1 - X;
    /* in a frame where the regions sit side by side: u along the route's main axis, v across it */
    var u = function (r) { return h ? { a: r.x, b: r.x + r.w, c: r.y + r.h / 2, lo: r.y, hi: r.y + r.h } : { a: r.y, b: r.y + r.h, c: r.x + r.w / 2, lo: r.x, hi: r.x + r.w }; };
    var S = u(s), Dd = u(d), SR = u(sr), DR = u(dr), fwd = DR.a + DR.b > SR.a + SR.b, m = fwd ? (SR.b + DR.a) / 2 : (DR.b + SR.a) / 2;
    var pt = function (a, v) { var q = []; q[X] = a; q[Y] = v; return q; };
    var pts = [], sides = [];
    var end = function (A, R, other, isStart) {
      var mc = (A.a + A.b) / 2, out = [];
      if (A.a === A.b || A === R || (A.a === R.a && A.b === R.b && A.lo === R.lo && A.hi === R.hi)) {   // a point, or the region itself: its facing side
        out = [pt(isStart === fwd ? A.b : A.a, A.c), pt(m, A.c)];
        return { pts: out, side: h ? "y" : "x" };
      }
      var up = other.c < A.c, edge = up ? A.lo : A.hi, g = edge + (up ? -GUTTER : GUTTER);
      out = [pt(mc, edge), pt(mc, g), pt(m, g)];
      return { pts: out, side: h ? "x" : "y" };
    };
    var a = end(S, SR, Dd, true), b = end(Dd, DR, S, false);
    pts = a.pts.concat(b.pts.slice().reverse());
    sides = [a.side, b.side];
    pts = pts.filter(function (q, i) { return !i || q[0] !== pts[i - 1][0] || q[1] !== pts[i - 1][1]; });
    return { axis: h ? "h" : "v", pts: pts, sides: sides };
  }

  /* outward(world, k, r) -> { side, free, at, edge }: the side of region k's foot a bridge from rect r (tiles; a point at World)
     leaves by: the nearest free one (no region beyond), else the nearest. From a point: bottom, right, left, top,
     since the region's board stands above it. at: where the channel ends, STUB tiles out, on r's gutter; edge: the foot's. */
  function outward(world, k, r) {
    var b = world.index["project:" + k], f = b.foot, others = [];
    (world.biomes || []).forEach(function (o) { if (o !== b) others.push(o.foot); });
    var across = function (o, h) { return h ? o.y < f.y + f.h && f.y < o.y + o.h : o.x < f.x + f.w && f.x < o.x + o.w; };
    var gx = r.w ? r.x + r.w + GUTTER : r.x, gy = r.h ? r.y + r.h + GUTTER : r.y;
    var sides = [
      { side: "top", d: r.y - f.y, free: !others.some(function (o) { return o.y + o.h <= f.y && across(o, false); }), at: [gx, f.y - STUB], edge: f.y },
      { side: "right", d: f.x + f.w - (r.x + r.w), free: !others.some(function (o) { return o.x >= f.x + f.w && across(o, true); }), at: [f.x + f.w + STUB, gy], edge: f.x + f.w },
      { side: "bottom", d: f.y + f.h - (r.y + r.h), free: !others.some(function (o) { return o.y >= f.y + f.h && across(o, false); }), at: [gx, f.y + f.h + STUB], edge: f.y + f.h },
      { side: "left", d: r.x - f.x, free: !others.some(function (o) { return o.x + o.w <= f.x && across(o, true); }), at: [f.x - STUB, gy], edge: f.x }
    ];
    if (!r.w && !r.h) sides = [sides[2], sides[1], sides[3], sides[0]].map(function (s, i) { s.d = i; return s; });
    var pick = sides.filter(function (s) { return s.free; });
    if (!pick.length) pick = sides;
    return pick.reduce(function (a, s) { return s.d < a.d ? s : a; });
  }
  /* bridgeRoute(c, world): a bridge's route from its far end into the rect that waits, minus points mid straight run. */
  function bridgeRoute(c, world) {
    var o = outward(world, c.key, c.dst), at = { x: o.at[0], y: o.at[1], w: 0, h: 0 }, rt = route(at, c.dst, at, c.dstReg);
    if (!rt) return null;
    var p = rt.pts;
    rt.pts = p.filter(function (q, i) { return !i || i === p.length - 1 || !((p[i - 1][0] === q[0] && q[0] === p[i + 1][0]) || (p[i - 1][1] === q[1] && q[1] === p[i + 1][1])); });
    c.src = c.srcReg = at; c.side = o.side; c.free = o.free; c.edge = o.edge;
    return rt;
  }

  /* channels(list, world, lv) -> { shown: [Channel], dropped }: the edges merged per pair of ends at a level, most edges first
     (then by key, so the cut is stable), cut at MAX_CHANNELS, each routed and given a lane: channels sharing a run (a row or
     column a straight part lies on) take lanes 0, 1, -1, 2, ... so they lie side by side. Channel: { id, src, dst (rects,
     tiles), route, lane, n, edges }; a bridge's also { bridge: true, names (its projects as people say them), side, free, edge }. */
  function channels(list, world, lv) {
    var index = (world && world.index) || {}, by = {}, all = [];
    list.forEach(function (e) {
      if (!e.drawn) return;
      if (e.foreign) {   // one bridge per rect that waits, named for every project it waits on there
        var w = anchor(index, e.task, e.plan, e.from, lv);
        if (!w) return;
        var bid = "bridge>" + w.id, bc = by[bid];
        if (!bc) { bc = by[bid] = { id: bid, bridge: true, key: e.from, dst: w.r, dstReg: w.reg, dstId: w.id, n: 0, edges: [], names: [] }; all.push(bc); }
        bc.n++; bc.edges.push(e);
        if (bc.names.indexOf(e.project) < 0) bc.names.push(e.project);
        return;
      }
      var d = anchor(index, e.task, e.plan, e.from, lv), s = anchor(index, e.blocker, "", e.to, lv);
      if (!d || !s) return;
      var id = s.id + ">" + d.id, c = by[id];
      if (!c) { c = by[id] = { id: id, src: s.r, dst: d.r, srcReg: s.reg, dstReg: d.reg, srcId: s.id, dstId: d.id, n: 0, edges: [] }; all.push(c); }
      c.n++; c.edges.push(e);
    });
    all.sort(function (a, b) { return b.n - a.n || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); });
    var shown = [], used = {};
    all.forEach(function (c) {
      if (shown.length >= MAX_CHANNELS) return;
      c.route = c.bridge ? bridgeRoute(c, world) : route(c.src, c.dst, c.srcReg, c.dstReg);
      if (!c.route) return;
      var p = c.route.pts, runs = [];
      for (var j = 1; j < p.length; j++) {
        var hz = p[j][1] === p[j - 1][1], q = hz ? 0 : 1;
        runs.push({ k: (hz ? "h:" : "v:") + Math.round(p[j][1 - q]), a: Math.min(p[j][q], p[j - 1][q]), b: Math.max(p[j][q], p[j - 1][q]) });
      }
      var clash = function (lane) { return runs.some(function (r) { return (used[r.k + "#" + lane] || []).some(function (u) { return u[0] < r.b + 1 && r.a < u[1] + 1; }); }); };
      for (var i = 0; ; i++) { c.lane = i % 2 ? (i + 1) / 2 : -i / 2; if (!clash(c.lane)) break; }
      runs.forEach(function (r) { (used[r.k + "#" + c.lane] || (used[r.k + "#" + c.lane] = [])).push([r.a, r.b]); });
      shown.push(c);
    });
    return { shown: shown, dropped: all.length - shown.length };
  }

  /* fieldRoute(a, b, plot) -> { pts }, tiles: from the centre of cell a (the blocker) to the centre of cell b on one plot,
     never through another cell: out of a to the seam between the tile rows (or columns) on b's side, along it, and in to b by
     its edge. Neighbours join centre to centre. In one row (or column) the seam is the one nearer the plot's middle; otherwise
     the row seam on b's side of a, then, when b's row is not beside it, the column seam on a's side of b. */
  function fieldRoute(a, b, plot) {
    var ca = [a.x + 0.5, a.y + 0.5], cb = [b.x + 0.5, b.y + 0.5], dx = b.x - a.x, dy = b.y - a.y, pts;
    var inner = function (v, lo, n) { return v + 0.5 < lo + n / 2 ? 1 : 0; };   // the seam past v nearer the plot's middle: beyond it (1) or before it (0)
    if (Math.abs(dx) + Math.abs(dy) === 1) pts = [ca, cb];
    else if (dy === 0) { var sy = a.y + inner(a.y, plot.y, plot.h); pts = [ca, [ca[0], sy], [cb[0], sy], cb]; }
    else if (dx === 0) { var sx = a.x + inner(a.x, plot.x, plot.w); pts = [ca, [sx, ca[1]], [sx, cb[1]], cb]; }
    else {
      var ry = dy > 0 ? a.y + 1 : a.y;
      if (b.y === ry || b.y + 1 === ry) pts = [ca, [ca[0], ry], [cb[0], ry], cb];
      else { var cx = dx > 0 ? b.x : b.x + 1; pts = [ca, [ca[0], ry], [cx, ry], [cx, cb[1]], cb]; }
    }
    return { pts: pts };
  }
  /* fieldRuns(list) -> the channels' half-tile steps, each once (the first's way wins), joined while they touch and run the
     same way: { h (a row), at (its line: a tile's centre line or a seam), a <= b, d (the water's way, +-1) }. */
  function fieldRuns(list) {
    var seen = {}, lines = {}, out = [];
    list.forEach(function (c) {
      var p = c.pts;
      for (var j = 1; j < p.length; j++) {
        var h = p[j][1] === p[j - 1][1], k = h ? 0 : 1, d = Math.sign(p[j][k] - p[j - 1][k]), ln = (h ? "h" : "v") + p[j][1 - k];
        for (var u = p[j - 1][k]; u !== p[j][k]; u += d / 2) {
          var lo = Math.min(u, u + d / 2);
          if (seen[ln + ":" + lo]) continue;
          seen[ln + ":" + lo] = 1; (lines[ln] || (lines[ln] = [])).push({ h: h, at: p[j][1 - k], a: lo, b: lo + 0.5, d: d });
        }
      }
    });
    Object.keys(lines).sort().forEach(function (k) {
      lines[k].sort(function (x, y) { return x.a - y.a; }).forEach(function (s) { var r = out[out.length - 1]; if (r && r.h === s.h && r.at === s.at && r.b === s.a && r.d === s.d) r.b = s.b; else out.push(s); });
    });
    return out;
  }
  /* fields(doc, world) -> {plotId: { shown: [{ task, blocker, pts }], dropped, runs }}: the blockers on a step's plot. */
  function fields(doc, world) {
    var index = (world && world.index) || {}, at = doc ? places(doc) : {}, out = {};
    Object.keys(at).forEach(function (id) {
      var w = index[id], t = at[id].task, p = w && index[w.plot];
      if (!t || !p || w.kind !== "task") return;
      (t.blocked_by || []).forEach(function (bid) {
        var a = index[bid], f = out[w.plot];
        if (!a || a.kind !== "task" || a.plot !== w.plot) return;
        if (!f) f = out[w.plot] = { shown: [], dropped: 0 };
        if (f.shown.length >= FIELD_MAX) { f.dropped++; return; }
        var r = fieldRoute(a, w, p);
        r.task = id; r.blocker = bid; f.shown.push(r);
      });
    });
    Object.keys(out).forEach(function (k) { out[k].runs = fieldRuns(out[k].shown); });
    return out;
  }

  /* index(doc, world) -> ix: the edges by what they touch (for the bead view and names), the channels per level (0, and 1 for
     every level below the World) and those inside each field. Built once per farm document. */
  function index(doc, world) {
    var list = edges(doc, world), ix = { edges: list, waits: {}, holds: {}, plotIn: {}, plotOut: {}, biomeIn: {}, biomeOut: {}, levels: [], fields: fields(doc, world) };
    var index = (world && world.index) || {}, at = doc ? places(doc) : {};
    var push = function (m, k, e) { if (k) (m[k] || (m[k] = [])).push(e); };
    list.forEach(function (e) {
      push(ix.waits, e.task, e); push(ix.holds, e.blocker, e);
      push(ix.plotIn, e.plan, e);
      var bi = index[e.blocker], bp = bi && bi.kind === "task" ? bi.plot : bi && bi.kind === "plot" ? bi.id : at[e.blocker] ? at[e.blocker].plan : "";
      push(ix.plotOut, bp, e);
      push(ix.biomeIn, "project:" + e.from, e); push(ix.biomeOut, "project:" + e.to, e);
    });
    ix.levels = [channels(list, world, 0), channels(list, world, 1)];
    return ix;
  }

  function waitLine(e, self) { return "Blocked by " + (e.foreign ? "a bead" : e.blocker) + " in " + e.project + (self ? "" : ": " + e.title + " waits on it"); }
  function holdLine(e, self) { return "Blocks " + e.task + " in " + e.fromName + (self ? "" : ": it waits on " + (e.blockerTitle || e.blocker)); }
  function names(list, f) { var seen = {}, out = []; list.forEach(function (e) { var n = f(e); if (!seen[n]) { seen[n] = 1; out.push(n); } }); return out; }
  /* What a region's edges come to, in one sentence, or "" when it has none. */
  function biomeSentence(ix, id) {
    var i = ix.biomeIn[id] || [], o = ix.biomeOut[id] || [], parts = [];
    if (i.length) parts.push(plural(names(i, function (e) { return e.task; }).length, "step here waits", "steps here wait") + " on beads in " + words(names(i, function (e) { return e.project; })));
    if (o.length) parts.push(plural(names(o, function (e) { return e.blocker; }).length, "bead here holds", "beads here hold") + " up steps in " + words(names(o, function (e) { return e.fromName; })));
    return parts.length ? "Irrigation channels: " + parts.join("; ") + "." : "";
  }
  /* The bead view's "Across projects" lines for a world Thing (web/workbench.js, through GARDEN.known; the info card that listed
     them retired with cursor-grid spec N-D14), as [cls, text, href, title]: a step, flower or pot says what it waits on and
     what waits on it, in another project; a plot the same for every step on it; a region one sentence. href(bead) is the
     in-garden bead view link. max lines (default LINES_MAX), then a count. */
  function lines(ix, h, href, max) {
    if (!ix || !h) return [];
    max = max || LINES_MAX;
    var k = h.kind, id = h.id, rows = [];
    if (k === "biome" || k === "sign") { var s = biomeSentence(ix, id); return s ? [["edge", s]] : []; }
    if (k === "plot" || k === "plotsign") {
      (ix.plotIn[id] || []).forEach(function (e) { rows.push([waitLine(e, false), e.blocker, e.blockerTitle]); });
      (ix.plotOut[id] || []).forEach(function (e) { rows.push([holdLine(e, e.blocker === id), e.task, e.title]); });
    } else if (k === "task" || k === "flower" || k === "seed") {
      (ix.waits[id] || []).forEach(function (e) { rows.push([waitLine(e, true), e.blocker, e.blockerTitle]); });
      (ix.holds[id] || []).forEach(function (e) { rows.push([holdLine(e, true), e.task, e.title]); });
    } else return [];
    var out = rows.slice(0, max).map(function (r) { return href && r[1] ? ["edge", r[0], href({ id: r[1] }), r[2] || r[1]] : ["edge", r[0]]; });
    if (rows.length > max) out.push(["edge", "+" + plural(rows.length - max, "more edge", "more edges") + " to other projects"]);
    return out;
  }
  /* The same facts for a hot link's accessible name, "" when there are none: " Blocked by gd-x in kbeads. Blocks gd-y in coop." */
  function aria(ix, h) {
    if (!ix || !h) return "";
    var k = h.kind, id = h.id, ws, hs;
    if (k === "biome" || k === "sign") { var s = biomeSentence(ix, id); return s ? " " + s : ""; }
    if (k === "plot" || k === "plotsign") { ws = ix.plotIn[id] || []; hs = ix.plotOut[id] || []; }
    else if (k === "task" || k === "flower" || k === "seed") { ws = ix.waits[id] || []; hs = ix.holds[id] || []; }
    else return "";
    var all = ws.map(function (e) { return waitLine(e, true) + "."; }).concat(hs.map(function (e) { return holdLine(e, true) + "."; }));
    if (!all.length) return "";
    return " " + all.slice(0, ARIA_MAX).join(" ") + (all.length > ARIA_MAX ? " And " + plural(all.length - ARIA_MAX, "more edge", "more edges") + " to other projects." : "");
  }

  /* width(z, level, T): the channel's width in world px: LINE_PX on screen, below the World level at most MAX_W tiles. */
  function width(z, lv, T) { return lv > 0 ? Math.min(LINE_PX / z, MAX_W * T) : LINE_PX / z; }
  /* The points of a channel in world px at width W, its lane shifting it sideways: every bend by the lane on both axes (so each
     straight run moves across itself), each end along its rect's side, kept a width inside it (an end at a point moves freely).
     A bridge on a free side runs at least four widths past its region. */
  function lanePts(c, T, W) {
    var p = c.route.pts, o = c.lane * W * 1.5, n = p.length;
    var out = p.map(function (q, i) { return i && i < n - 1 ? [q[0] * T + o, q[1] * T + o] : [q[0] * T, q[1] * T]; });
    var clamp = function (v, lo, hi) { return lo > hi ? (lo + hi) / 2 : v < lo ? lo : v > hi ? hi : v; };
    [[0, 1, c.src, c.route.sides[0]], [n - 1, n - 2, c.dst, c.route.sides[1]]].forEach(function (e) {
      var k = e[3] === "x" ? 0 : 1, r = e[2], lo = (k ? r.y : r.x) * T + W, hi = (k ? r.y + r.h : r.x + r.w) * T - W;
      out[e[0]][k] = (k ? r.h : r.w) ? clamp(out[e[0]][k] + o, lo, hi) : out[e[0]][k] + o;   // a point end moves with its lane
      out[e[1]][k] = out[e[0]][k];   // the run off the end stays square
    });
    if (c.bridge && c.free) {   // a bridge's far end, pushed out to four widths past the foot's edge
      var q = out[0], v = c.side === "top" || c.side === "bottom" ? 1 : 0;
      q[v] = c.side === "top" || c.side === "left" ? Math.min(q[v], c.edge * T - 4 * W) : Math.max(q[v], c.edge * T + 4 * W);
    }
    return out;
  }
  function hit(r, v) { return !v || (r.x < v.x + v.w && v.x < r.x + r.w && r.y < v.y + v.h && v.y < r.y + r.h); }
  /* One straight run from a to b, width W: a dark bank, the water, light ripples along it, shifted sh px along the axis.
     Each end runs on by half a width, so the corners close. */
  function run(ctx, a, b, W, col, view, sh) {
    var h = a[1] === b[1], x0 = Math.min(a[0], b[0]) - W / 2, y0 = Math.min(a[1], b[1]) - W / 2, len = h ? Math.abs(b[0] - a[0]) + W : Math.abs(b[1] - a[1]) + W;
    var r = h ? { x: x0, y: y0, w: len, h: W } : { x: x0, y: y0, w: W, h: len };
    if (!hit(r, view)) return 0;
    var n = 2, i = W * 0.2, from = 0, to = len, P = W * 3;
    sh = sh || 0;
    ctx.fillStyle = col.bank; ctx.fillRect(r.x, r.y, r.w, r.h);
    ctx.fillStyle = col.water; ctx.fillRect(r.x + i, r.y + i, r.w - 2 * i, r.h - 2 * i);
    if (view) { from = Math.max(0, (h ? view.x - r.x : view.y - r.y)); to = Math.min(len, (h ? view.x + view.w - r.x : view.y + view.h - r.y)); }
    ctx.fillStyle = col.ripple;
    for (var s = Math.floor((from - sh) / P) * P + W + sh; s + W <= to && s + W <= len - W; s += P) {
      if (s < 0) continue;
      n++;
      if (h) ctx.fillRect(r.x + s, r.y + W * 0.42, W, W * 0.16); else ctx.fillRect(r.x + W * 0.42, r.y + s, W * 0.16, W);
    }
    return n;
  }
  /* The sluice gate at the waiting end: a board across the channel, just outside the rect it feeds. */
  function gate(ctx, pts, W, col) {
    var e = pts[pts.length - 1], f = pts[pts.length - 2], h = e[1] === f[1], dir = h ? Math.sign(e[0] - f[0]) || 1 : Math.sign(e[1] - f[1]) || 1, t = W * 0.5, l = W * 1.8, e1 = W * 0.15;
    var x = h ? e[0] - dir * W * 0.75 - t / 2 : e[0] - l / 2, y = h ? e[1] - l / 2 : e[1] - dir * W * 0.75 - t / 2, w = h ? t : l, hh = h ? l : t;
    ctx.fillStyle = col.bank; ctx.fillRect(x - e1, y - e1, w + 2 * e1, hh + 2 * e1);
    ctx.fillStyle = col.gate; ctx.fillRect(x, y, w, hh);
  }
  /* A bridge's footbridge: three planks across the channel, 1.5 widths in from its far end p[0], edged dark. */
  function deck(ctx, p, W, col) {
    var e = p[0], f = p[1], h = e[1] === f[1], dir = h ? Math.sign(e[0] - f[0]) || 1 : Math.sign(e[1] - f[1]) || 1, l = W * 1.6, a = W * 2.4, e1 = W * 0.15;
    var cx = h ? e[0] - dir * W * 1.5 : e[0], cy = h ? e[1] : e[1] - dir * W * 1.5, w = h ? l : a, hh = h ? a : l, x = cx - w / 2, y = cy - hh / 2;
    ctx.fillStyle = col.bank; ctx.fillRect(x - e1, y - e1, w + 2 * e1, hh + 2 * e1);
    ctx.fillStyle = col.gate; ctx.fillRect(x, y, w, hh);
    ctx.fillStyle = col.bank;   // the gaps between the planks, which lie across the channel
    for (var i = 1; i < 3; i++) { if (h) ctx.fillRect(x + l * i / 3 - e1 / 2, y, e1, hh); else ctx.fillRect(x, y + l * i / 3 - e1 / 2, w, e1); }
    return { x: x - e1, y: y - e1, w: w + 2 * e1, h: hh + 2 * e1 };
  }
  /* What a bridge is named: its project, two of them, or two and a count. */
  function nameOf(c) { var l = c.names || []; return l.length < 3 ? words(l) : l[0] + ", " + l[1] + " +" + (l.length - 2); }
  /* A name baked per word, theme and DPR into a canvas {img, w, h} (device px): NAME_PX bold display font, ink over a 1px halo.
     Off the page (node) only its size. */
  var nameCache = {}, nameN = 0, fam = null;
  function family() {
    if (fam === null) {
      fam = "ui-monospace,Menlo,Consolas,monospace";
      try { var v = getComputedStyle(document.documentElement).getPropertyValue("--font-display").trim(); if (v) fam = v; } catch (e) { /* node */ }
    }
    return fam;
  }
  function bake(word, night, dpr) {
    var key = (night ? "n" : "d") + dpr + "|" + word, c = nameCache[key];
    if (c) return c;
    if (++nameN > 64) { nameCache = {}; nameN = 1; }
    var fs = Math.round(NAME_PX * dpr), hw = Math.max(1, Math.round(dpr)), font = "700 " + fs + "px " + family(), cs = night ? NAME.night : NAME.day;
    var cv = typeof document !== "undefined" && document.createElement ? document.createElement("canvas") : null, x = cv && cv.getContext ? cv.getContext("2d") : null;
    if (x && x.measureText) x.font = font;
    var w = x && x.measureText ? Math.ceil(x.measureText(word).width) : Math.ceil(word.length * fs * 0.6);
    c = { w: w + 2 * hw, h: Math.ceil(fs * 1.3) + 2 * hw, img: null };
    if (x) {
      cv.width = c.w; cv.height = c.h;   // which resets the context
      x.font = font; x.textBaseline = "middle"; x.lineJoin = "round"; x.lineWidth = 2 * hw;
      x.strokeStyle = cs[1]; x.strokeText(word, hw, c.h / 2); x.fillStyle = cs[0]; x.fillText(word, hw, c.h / 2);
      c.img = cv;
    }
    return nameCache[key] = c;
  }
  /* nameBox(c, p, W, size, k, safe): a bridge's name's box (world px): beyond the far end p[0] off east or west, beside the
     tip off top or bottom, never on the water; k steps it past a name already there. Off safe (the map the HUD leaves) it
     slides back along the channel onto it, and beside a top or bottom tip it goes to the tip's west side when its east side
     runs off safe's (gd-8cz6uje2WZ). */
  function nameBox(c, p, W, size, k, safe) {
    var e = p[0], g = W * 0.6, w = size.w, h = size.h, s = c.side;
    var x = s === "right" ? e[0] + W / 2 + g + k * (w + g) : s === "left" ? e[0] - W / 2 - g - w - k * (w + g) : e[0] + W * 1.2 + g;
    var y = s === "top" ? e[1] - h / 2 - k * (h + g) : s === "bottom" ? e[1] - h / 2 + k * (h + g) : e[1] - h / 2;
    if (safe) {
      if (s === "right") x = Math.max(Math.min(x, safe.x + safe.w - w), p[1][0]);
      else if (s === "left") x = Math.min(Math.max(x, safe.x), p[1][0] - w);
      else if (s === "bottom") y = Math.max(Math.min(y, safe.y + safe.h - h), p[1][1]);
      else y = Math.min(Math.max(y, safe.y), p[1][1] - h);
      if ((s === "top" || s === "bottom") && x + w > safe.x + safe.w) x = e[0] - W * 1.2 - g - w;
    }
    return { x: x, y: y, w: w, h: h };
  }
  /* draw(ctx, ix, o): the channels of o.level, in world px under the caller's world transform. o: { level, z, T, night, view
     (world px, optional: runs outside it are skipped) }. Returns the number of fillRect calls, for the perf test. */
  function draw(ctx, ix, o) {
    if (!ix) return 0;
    var set = ix.levels[o.level > 0 ? 1 : 0], W = width(o.z, o.level, o.T), col = o.night ? NIGHT : DAY, n = 0;
    if (!set) return 0;
    set.shown.forEach(function (c) {
      var p = lanePts(c, o.T, W), e = p[p.length - 1];
      for (var i = 0; i + 1 < p.length; i++) n += run(ctx, p[i], p[i + 1], W, col, o.view);
      if (hit({ x: e[0] - 2 * W, y: e[1] - 2 * W, w: 4 * W, h: 4 * W }, o.view)) { gate(ctx, p, W, col); n += 2; }
      if (c.bridge && hit({ x: p[0][0] - 3 * W, y: p[0][1] - 3 * W, w: 6 * W, h: 6 * W }, o.view)) { deck(ctx, p, W, col); n += 4; }
    });
    return n;
  }
  /* One run of a field channel, r of fieldRuns in tiles at T px a tile: the bank (by night a halo) a sixteenth of a tile each
     side of FIELD_W of water, and comets every FIELD_P stepping a sixteenth a frame the water's way: a tail of mid water then
     a bright head. The comets' phase is taken along the line, so runs that meet on one line keep step. Returns its fillRects. */
  function thin(ctx, r, T, col, frame, night) {
    var u = T / 16, W = FIELD_W * T, P = FIELD_P * T, h = r.h, len = (r.b - r.a) * T, x0 = h ? r.a * T : r.at * T - W / 2, y0 = h ? r.at * T - W / 2 : r.a * T, n = 2;
    if (!(len > 0)) return 0;
    ctx.fillStyle = night ? col.halo : col.bank; ctx.fillRect(x0 - u, y0 - u, (h ? len : W) + 2 * u, (h ? W : len) + 2 * u);
    ctx.fillStyle = col.water; ctx.fillRect(x0, y0, h ? len : W, h ? W : len);
    var along = function (s, l) { var q = r.d > 0 ? s : len - s - l, q0 = Math.max(0, q), q1 = Math.min(len, q + l); if (q1 <= q0) return; n++; if (h) ctx.fillRect(x0 + q0, y0, q1 - q0, W); else ctx.fillRect(x0, y0 + q0, W, q1 - q0); };
    var sh = (((frame || 0) * u - r.d * r.a * T) % P + P) % P;   // where the first comet's tail starts, so the pattern runs on along the line
    for (var s = sh - P; s < len; s += P) { ctx.fillStyle = col.mid; along(s, 1.5 * u); ctx.fillStyle = col.ripple; along(s + 1.5 * u, u); }
    return n;
  }
  /* drawField(ctx, ix, plot, o): plot's channels as thin runs, no gate. o: { T, night, frame (the scene's tick: the comets
     step a sixteenth of a tile with the water) }. Returns its fillRect calls. */
  function drawField(ctx, ix, plot, o) {
    var f = ix && ix.fields && ix.fields[plot], col = o.night ? NIGHT : DAY, n = 0;
    if (!f) return 0;
    f.runs.forEach(function (r) { n += thin(ctx, r, o.T, col, o.frame, !!o.night); });
    return n;
  }
  /* safeView(sv, cam, vp): garden.js's signView (board px, HUD insets) in world px at cam, null before its first refresh. */
  function safeView(sv, cam, vp) {
    if (!sv) return null;
    var l = sv.left || 0, t = sv.top || 0, w = Math.max(1, sv.w - l - (sv.right || 0)), h = Math.max(1, sv.h - t - (sv.bottom || 0)), z = cam.z;
    return { x: cam.cx + (l - vp.w / 2) / z, y: cam.cy + (t - vp.h / 2) / z, w: w / z, h: h / z };
  }
  /* drawNames(ctx, ix, o) -> the boxes (world px) of o.level's bridge names: draw's o plus dpr and safe. Drawn after the
     plots; off the page the boxes only. */
  function drawNames(ctx, ix, o) {
    var set = ix && ix.levels[o.level > 0 ? 1 : 0], out = [];
    if (!set) return out;
    var W = width(o.z, o.level, o.T), dpr = o.dpr || 1;
    set.shown.forEach(function (c) {
      if (!c.bridge) return;
      var p = lanePts(c, o.T, W), b = bake(nameOf(c), o.night, dpr), size = { w: b.w / (o.z * dpr), h: b.h / (o.z * dpr) }, r;
      for (var k = 0; k < 4; k++) { r = nameBox(c, p, W, size, k, o.safe); if (!out.some(function (t) { return hit(r, t); })) break; }
      out.push(r);
      if (!b.img || !ctx.drawImage || !hit(r, o.view)) return;
      var m = ctx.getTransform ? ctx.getTransform() : null, x = r.x, y = r.y;
      if (m && m.a) { x = (Math.round(m.a * x + m.e) - m.e) / m.a; y = (Math.round(m.d * y + m.f) - m.f) / m.d; }   // on whole device px, so the name is sharp
      ctx.drawImage(b.img, x, y, size.w, size.h);
    });
    return out;
  }
  /* A legend swatch onto a 24 x 28 canvas: a channel ending in its gate. */
  function drawLegend(ctx, night) {
    var col = night ? NIGHT : DAY;
    run(ctx, [3, 14], [17, 14], 6, col, null);
    gate(ctx, [[3, 14], [23, 14]], 6, col);
  }

  return { MAX_CHANNELS: MAX_CHANNELS, LINES_MAX: LINES_MAX, ARIA_MAX: ARIA_MAX, LINE_PX: LINE_PX, GUTTER: GUTTER, STUB: STUB, FIELD_MAX: FIELD_MAX, FIELD_W: FIELD_W, FIELD_P: FIELD_P, edges: edges, route: route, width: width, channels: channels,
    fieldRoute: fieldRoute, drawField: drawField, outward: outward, nameOf: nameOf, nameBox: nameBox, index: index, lines: lines, aria: aria, lanePts: lanePts, draw: draw, safeView: safeView, drawNames: drawNames, drawLegend: drawLegend };
})();
if (typeof module !== "undefined") module.exports = EDGES;
