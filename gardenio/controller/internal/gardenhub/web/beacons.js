/* BEACONS: arrows at the map's edge for what needs you off-screen (gd-gtr7qBCn9z; homestead-world-design.md section 6.1): a
   plan with asks waiting on a human, or a project's Mailbox holding ones no plan carries. Touching arrows merge into one with a
   count; each has a glyph (? a decision, ! an escalation) and an arrow shape, never colour alone. targets(), layout() and
   label() are pure and run under node (beacons_test.go); measure() and render() are the DOM code. Units: world px for
   targets, CSS px of the board on screen. */
var BEACONS = (function () {
  "use strict";
  var SIZE = 36, SIZE_COARSE = 44;   // the arrow's square; 44 px for coarse pointers (design section 8)
  var MARGIN = 14, GAP = 4;          // from the board edge (room for the 12 px point), and between an arrow and a control or another arrow
  var MIN_SEEN = 24;                 // a target showing at least this much (px, each axis) is on screen and needs no arrow
  var DIRS = ["east", "south-east", "south", "south-west", "west", "north-west", "north", "north-east"];

  /* What needs you, from a WORLD.layout() result: one target per plot whose plan waits on a human, one per home strip's
     Escalations cell for its escalations waiting on a human (28.13), and one per project's Mailbox (homestead-world-design.md
     28.6) for its decisions; each only for what no plan's waiting list carries (so one ask is never pointed at twice). asks are
     the Ledger's ({id, type, project}), an escalation among them the Escalations cell's; an ask whose project has no biome is
     the Commons's, as WORLD.homeFill places it. A strip's target id is its zone, home:<key>/escalation or home:<key>/mailbox,
     with asks the ids N walks; tile is world px per tile. */
  function targets(world, asks, tile) {
    var out = [], inPlan = {};
    ((world && world.biomes) || []).forEach(function (b) {
      b.plots.forEach(function (p) {
        var w = (p.plan && p.plan.waiting) || [];
        w.forEach(function (x) { inPlan[x.id] = 1; });
        if (!(p.needs > 0)) return;
        var esc = w.length ? w.some(function (x) { return x.human && x.kind === "escalation"; }) : (p.plan.open_escalations || 0) > 0;
        out.push({ id: p.id, kind: "plan", name: p.plan.title || p.id, project: b.name, count: p.needs, glyph: esc ? "!" : "?",
          rect: { x: p.x * tile, y: p.y * tile, w: p.w * tile, h: p.h * tile } });
      });
    });
    ((world && world.biomes) || []).forEach(function (b) {
      var z = b.home && b.home.zones.escalation, on = z ? z.items.filter(function (it) { return it.wait.human && !inPlan[it.wait.id]; }) : [];
      if (on.length) out.push({ id: z.id, kind: "escalation", name: "the Escalations in " + (b.key ? b.name : "the Commons"), project: b.name, count: on.length, glyph: "!",
        asks: on.map(function (it) { return it.id; }), rect: { x: z.x * tile, y: z.y * tile, w: z.w * tile, h: z.h * tile } });
    });
    var ix = (world && world.index) || {}, by = {}, keys = [];
    (asks || []).forEach(function (a) {
      if (inPlan[a.id] || a.type === "escalation") return;
      var b = ix["project:" + String(a.project || "").trim().toLowerCase()] || ix["project:"];
      if (!b || !b.home) return;
      if (!by[b.id]) { by[b.id] = { b: b, asks: [] }; keys.push(b.id); }
      by[b.id].asks.push(a);
    });
    keys.forEach(function (id) {
      var b = by[id].b, left = by[id].asks, m = b.home.cells.mailbox;
      out.push({ id: b.home.id + "/mailbox", kind: "mailbox", name: "the Mailbox in " + (b.key ? b.name : "the Commons"), project: b.name, count: left.length, asks: left.map(function (a) { return a.id; }),
        glyph: "?", rect: { x: m.x * tile, y: m.y * tile, w: m.w * tile, h: m.h * tile } });
    });
    return out;
  }

  /* The plot (or Nursery pot) an ask belongs to: its plan's waiting list, a staked plot's or pot's plan decision, else the bed
     holding it. {id, kind, title, short} or null, for the Mailbox's "Open on the map" (gd-kNV6Eos4ON). */
  function plotOf(world, id) {
    if (!id || !world) return null;
    var hit = null, mine = function (d) { return !!d && d.id === id; };
    var on = function (p) { return (mine(p.seed && p.seed.decision) || ((p.plan && p.plan.waiting) || []).some(mine)) && !!(hit = p); };
    (world.biomes || []).some(function (b) { return b.plots.some(on) || (b.nursery || []).some(on); });
    var c = !hit && world.index && world.index[id];
    if (c && c.plot) hit = world.index[c.plot] || null;
    if (!hit) return null;
    var pl = hit.plan || hit.seed || {}, title = pl.title || hit.id;
    return { id: hit.id, kind: hit.kind, title: title, short: String(pl.short_title || "").trim() || title };
  }

  function toScreen(cam, vp, x, y) { return { x: (x - cam.cx) * cam.z + vp.w / 2, y: (y - cam.cy) * cam.z + vp.h / 2 }; }
  /* On screen: the target's rect overlaps the board below the header by MIN_SEEN px (or all of itself, if smaller) on each axis.
     A target all in view counts, whatever the float rounding of its edges (gd-VFtLgEIM6f). */
  function onScreen(r, cam, vp, top) {
    var a = toScreen(cam, vp, r.x, r.y), w = r.w * cam.z, h = r.h * cam.z, EPS = 1e-6;
    var ox = Math.min(a.x + w, vp.w) - Math.max(a.x, 0), oy = Math.min(a.y + h, vp.h) - Math.max(a.y, top);
    return ox >= Math.min(MIN_SEEN, w) - EPS && oy >= Math.min(MIN_SEEN, h) - EPS && ox > 0 && oy > 0;
  }

  /* The rect the arrows' centres run along: the board inset by half an arrow and the margin, under the header. A wide bar on
     the bottom edge (tab bar, the title bar on it: gd-GIhQagxxTN) lifts it while the map above keeps room for an arrow, else
     stays a block. null when the board is too small. */
  function edgeBox(vp, top, size, blocks) {
    var h = size / 2 + MARGIN, box = { x0: h, y0: Math.max(0, top) + h, x1: vp.w - h, y1: vp.h - h };
    (blocks || []).filter(function (b) { return b.w >= vp.w * 0.6 && b.y > vp.h / 2; }).sort(function (a, b) { return b.y + b.h - a.y - a.h; })
      .forEach(function (b) { if (b.y + b.h >= box.y1 + h - 8 && (box.y1 === vp.h - h || b.y - h - box.y0 >= size)) box.y1 = Math.min(box.y1, b.y - h); });
    return box.x1 - box.x0 >= size && box.y1 - box.y0 >= size ? box : null;
  }
  /* The box's perimeter as one coordinate t, clockwise from the top-left corner: top, right, bottom, left. */
  function perim(b) { return 2 * (b.x1 - b.x0) + 2 * (b.y1 - b.y0); }
  function atT(b, t) {
    var W = b.x1 - b.x0, H = b.y1 - b.y0, P = 2 * W + 2 * H;
    t = ((t % P) + P) % P;
    if (t <= W) return { x: b.x0 + t, y: b.y0 };
    if (t <= W + H) return { x: b.x1, y: b.y0 + t - W };
    if (t <= 2 * W + H) return { x: b.x1 - (t - W - H), y: b.y1 };
    return { x: b.x0, y: b.y1 - (t - 2 * W - H) };
  }
  function toT(b, x, y) {
    var W = b.x1 - b.x0, H = b.y1 - b.y0, d = [Math.abs(y - b.y0), Math.abs(x - b.x1), Math.abs(y - b.y1), Math.abs(x - b.x0)];
    var s = d.indexOf(Math.min.apply(null, d)), cx = Math.max(b.x0, Math.min(b.x1, x)), cy = Math.max(b.y0, Math.min(b.y1, y));
    return s === 0 ? cx - b.x0 : s === 1 ? W + cy - b.y0 : s === 2 ? W + H + b.x1 - cx : 2 * W + H + b.y1 - cy;
  }
  /* Where the ray from the box's centre towards screen point q leaves the box. null when q is the centre. */
  function edgePoint(b, q) {
    var cx = (b.x0 + b.x1) / 2, cy = (b.y0 + b.y1) / 2, dx = q.x - cx, dy = q.y - cy;
    if (!dx && !dy) return null;
    var s = Math.min(dx ? (dx > 0 ? b.x1 - cx : cx - b.x0) / Math.abs(dx) : Infinity, dy ? (dy > 0 ? b.y1 - cy : cy - b.y0) / Math.abs(dy) : Infinity);
    return { x: cx + dx * s, y: cy + dy * s };
  }
  /* The stretches of t where an arrow would touch a control (the camera buttons, drawers, pad: board rects), merged. */
  function blockedSpans(b, blocks, size) {
    var W = b.x1 - b.x0, H = b.y1 - b.y0, h = size / 2 + GAP, out = [];
    function span(a, z) { if (z > a) out.push([a, z]); }
    (blocks || []).forEach(function (r) {
      var x0 = Math.max(r.x - h, b.x0), x1 = Math.min(r.x + r.w + h, b.x1), y0 = Math.max(r.y - h, b.y0), y1 = Math.min(r.y + r.h + h, b.y1);
      var inX = function (x) { return x > r.x - h && x < r.x + r.w + h; }, inY = function (y) { return y > r.y - h && y < r.y + r.h + h; };
      if (inY(b.y0)) span(x0 - b.x0, x1 - b.x0);
      if (inX(b.x1)) span(W + y0 - b.y0, W + y1 - b.y0);
      if (inY(b.y1)) span(W + H + b.x1 - x1, W + H + b.x1 - x0);
      if (inX(b.x0)) span(2 * W + H + b.y1 - y1, 2 * W + H + b.y1 - y0);
    });
    out.sort(function (p, q) { return p[0] - q[0]; });
    var m = [];
    out.forEach(function (s) { var l = m[m.length - 1]; if (l && s[0] <= l[1]) l[1] = Math.max(l[1], s[1]); else m.push([s[0], s[1]]); });
    return m;
  }
  function free(t, spans) { for (var i = 0; i < spans.length; i++) if (t > spans[i][0] && t < spans[i][1]) return false; return true; }
  /* t, or the nearest free t past the end of the span it falls in (round the corner if need be). null when every edge is covered. */
  function nudge(t, spans, P) {
    if (free(t, spans)) return t;
    var best = null, bd = Infinity;
    spans.forEach(function (s) {
      [s[0] - 0.5, s[1] + 0.5].forEach(function (c) {
        c = ((c % P) + P) % P;
        if (!free(c, spans)) return;
        var d = Math.abs(c - t); d = Math.min(d, P - d);
        if (d < bd) { bd = d; best = c; }
      });
    });
    return best;
  }
  function touching(a, b, lim) { return Math.max(Math.abs(a.x - b.x), Math.abs(a.y - b.y)) < lim; }
  function compass(deg) { return DIRS[Math.round((((deg % 360) + 360) % 360) / 45) % 8]; }

  /* The arrows for a camera: each off-screen target projected to the edge and slid off any control; arrows that would touch
     merge (clockwise from the top-left, the Tab order), sitting where the nearest target projects, which they fly to.
     opts: size (px), top (the header's bottom, board px), blocks (board rects to keep off). */
  function layout(list, cam, vp, opts) {
    opts = opts || {};
    var size = opts.size || SIZE, top = opts.top || 0, blocks = opts.blocks || [], box = edgeBox(vp, top, size, blocks);
    if (!box || !list || !list.length) return [];
    var P = perim(box), spans = blockedSpans(box, blocks, size), cx = (box.x0 + box.x1) / 2, cy = (box.y0 + box.y1) / 2, pts = [];
    list.forEach(function (tg) {
      if (onScreen(tg.rect, cam, vp, top)) return;
      var q = toScreen(cam, vp, tg.rect.x + tg.rect.w / 2, tg.rect.y + tg.rect.h / 2), e = edgePoint(box, q);
      if (!e) return;
      var t = nudge(toT(box, e.x, e.y), spans, P);
      if (t === null) return;
      var p = atT(box, t);
      pts.push({ t: t, x: p.x, y: p.y, q: q, d: Math.hypot(q.x - cx, q.y - cy), tg: tg });
    });
    pts.sort(function (a, b) { return a.t - b.t || a.d - b.d; });
    var groups = [], lim = size + GAP;
    pts.forEach(function (p) {
      var g = groups[groups.length - 1];
      if (g && touching(g[g.length - 1], p, lim)) g.push(p); else groups.push([p]);
    });
    if (groups.length > 1 && touching(groups[groups.length - 1][groups[groups.length - 1].length - 1], groups[0][0], lim)) groups[0] = groups.pop().concat(groups[0]);
    return groups.map(function (g) {
      g.sort(function (a, b) { return a.d - b.d; });
      var r = g[0], ang = Math.atan2(r.q.y - r.y, r.q.x - r.x) * 180 / Math.PI, n = 0, esc = false;
      g.forEach(function (p) { n += p.tg.count; if (p.tg.glyph === "!") esc = true; });
      return { key: r.tg.kind + ":" + r.tg.id, x: r.x, y: r.y, size: size, angle: ang, dir: compass(ang), count: n, glyph: esc ? "!" : "?",
        targets: g.map(function (p) { return p.tg; }) };
    });
  }

  /* The bands the arrows take along the board's edges, as CURSOR.frame's HUD insets (gd-SWjiYouIQt): an arrow over one of
     rects (world px: the Things the camera frames) claims its side out to its far edge and a GAP, like TOUR.inset; null when
     none sits on a Thing. */
  function inset(groups, cam, vp, rects) {
    var rs = (rects || []).map(function (q) { var a = toScreen(cam, vp, q.x, q.y); return { x: a.x, y: a.y, w: q.w * cam.z, h: q.h * cam.z }; }), out = null;
    (groups || []).forEach(function (g) {
      var h = g.size / 2 + GAP;
      if (!rs.some(function (r) { return g.x + h > r.x && g.x - h < r.x + r.w && g.y + h > r.y && g.y - h < r.y + r.h; })) return;
      var d = [g.y, vp.w - g.x, vp.h - g.y, g.x], s = d.indexOf(Math.min.apply(null, d)), k = ["top", "right", "bottom", "left"][s];
      out = out || { top: 0, right: 0, bottom: 0, left: 0 };
      out[k] = Math.max(out[k], Math.ceil(d[s] + h));
    });
    return out;
  }

  /* The board rects of the farmstead cells holding items in view (the Escalations cell's icons and crate, the Pen's goats):
     per cell, from its top-left corner (where its count sign sits) to its last item, clipped to the board under the header.
     An arrow keeps off them like a control, since they are drawn in the world and measure() sees only the DOM
     (gd-WzYgtbwMT7). tile is world px per tile. */
  function keepOff(world, cam, vp, tile, top) {
    var out = [];
    ((world && world.biomes) || []).forEach(function (b) {
      var zs = (b.home && b.home.zones) || {};
      Object.keys(zs).forEach(function (k) {
        var z = zs[k], its = (z.items || []).concat(z.crate ? [z.crate] : []), x0 = z.x, y0 = z.y, x1 = -Infinity, y1 = -Infinity;
        if (!its.length) return;
        its.forEach(function (it) { x0 = Math.min(x0, it.x); y0 = Math.min(y0, it.y); x1 = Math.max(x1, it.x + it.w); y1 = Math.max(y1, it.y + it.h); });
        var a = toScreen(cam, vp, x0 * tile, y0 * tile), z = toScreen(cam, vp, x1 * tile, y1 * tile);
        var r = { x: Math.max(a.x, 0), y: Math.max(a.y, top || 0), w: 0, h: 0 };
        r.w = Math.min(z.x, vp.w) - r.x; r.h = Math.min(z.y, vp.h) - r.y;
        if (r.w > 0 && r.h > 0) out.push(r);
      });
    });
    return out;
  }

  /* What an arrow says to a screen reader and in its tooltip: how many, which way, what, and where activating it goes. */
  function label(g) {
    var names = g.targets.slice(0, 3).map(function (t) { return t.name + (g.targets.length > 1 && t.count > 1 ? " (" + t.count + ")" : ""); });
    if (g.targets.length > 3) names.push(g.targets.length - 3 + " more");
    var what = names.length > 1 ? names.slice(0, -1).join(", ") + " and " + names[names.length - 1] : names[0];
    return g.count + (g.count === 1 ? " needs" : " need") + " you to the " + g.dir + ": " + what + ". Fly to " + g.targets[0].name + ".";
  }

  /* The board rects the arrows keep off, and the header's bottom, measured from the live page. */
  var KEEP_OFF = ".cam,.dbtns,.dw,.tabbar,.tour,.curbar", SHADOW = 3;   // the cursor bar too (gd-yozHl8UqIs); a .btn's drop shadow (garden.css), which an arrow's ring keeps off too
  function measure(board, header) {
    var o = board.getBoundingClientRect(), top = 0, out = [];
    if (header) { var hb = header.getBoundingClientRect(); if (hb.height) top = hb.top + hb.height - o.top; }
    Array.prototype.forEach.call(document.querySelectorAll(KEEP_OFF), function (e) {
      var b = e.getBoundingClientRect();
      if (b.width && b.height) out.push({ x: b.left - o.left, y: b.top - o.top, w: b.width, h: b.height + SHADOW });
    });
    return { top: top, blocks: out };
  }
  function stop(e) { e.stopPropagation(); }
  /* Reconcile the arrow buttons in el with layout() groups, keyed by their nearest target so a focused arrow survives a move.
     go(target, keyed) flies there; keyed is true for a keyboard activation (the target's link then takes focus). */
  function render(el, groups, go) {
    var have = {}, keep = {}, i;
    for (i = 0; i < el.children.length; i++) have[el.children[i]._key] = el.children[i];
    groups.forEach(function (g, k) {
      var n = have[g.key];
      if (!n) {
        n = document.createElement("button"); n.type = "button"; n.className = "bcn"; n._key = g.key;
        n._ar = n.appendChild(document.createElement("span")); n._ar.className = "bcn-ar";
        n._gl = n.appendChild(document.createElement("span")); n._gl.className = "bcn-g";
        n._ct = n.appendChild(document.createElement("span")); n._ct.className = "bcn-n"; n._ct.hidden = true;
        n._ar.setAttribute("aria-hidden", "true"); n._gl.setAttribute("aria-hidden", "true"); n._ct.setAttribute("aria-hidden", "true");
        n.addEventListener("pointerdown", stop); n.addEventListener("dblclick", stop); n.addEventListener("wheel", stop);
        n.addEventListener("click", function (e) { e.stopPropagation(); go(n._g.targets[0], e.detail === 0); });
      }
      n._g = g; keep[g.key] = 1;
      var tr = "translate(" + Math.round(g.x - g.size / 2) + "px," + Math.round(g.y - g.size / 2) + "px)";
      if (n.style.transform !== tr) n.style.transform = tr;
      n.style.setProperty("--a", Math.round(g.angle) + "deg");
      if (n._gl.textContent !== g.glyph) n._gl.textContent = g.glyph;
      var c = g.count > 1 ? String(g.count) : "";
      if (n._ct.textContent !== c) { n._ct.textContent = c; n._ct.hidden = !c; }
      var l = label(g);
      if (n.getAttribute("aria-label") !== l) { n.setAttribute("aria-label", l); n.title = l; }
      if (el.children[k] !== n) el.insertBefore(n, el.children[k] || null);
    });
    for (var key in have) if (!keep[key]) el.removeChild(have[key]);
  }

  /* A Mailbox row (data-src "i-<id>") whose ask a plan carries gets "Part of <plan>" and "Open on the map", a link to
     ?at=<project>/<plan> that opt.go(plot id) follows on a plain click. Idempotent: it runs on every mirror. */
  function mapLinks(ul, world, opt) {
    if (!ul) return;
    ul._mapOpt = opt;
    if (!ul._mapWired) {
      ul._mapWired = true;
      ul.addEventListener("click", function (e) {
        var a = e.target && e.target.closest ? e.target.closest("a.to-map") : null;
        if (!a || e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || !ul._mapOpt.go) return;
        e.preventDefault(); ul._mapOpt.go(a.dataset.plot);
      });
    }
    Array.prototype.forEach.call(ul.children, function (li) {
      var id = String(li.dataset.src || "").replace(/^i-/, ""), p = world ? plotOf(world, id) : null, at = p ? WORLD.atFor(world, p.kind, p.id) : null;
      var row = li.querySelector(".onplan");
      if (!at) { if (row) row.remove(); return; }
      if (!row) {
        row = document.createElement("p"); row.className = "onplan";
        var of = row.appendChild(document.createElement("span"));
        of.appendChild(document.createTextNode("Part of ")); of.appendChild(document.createElement("b"));
        var a = row.appendChild(document.createElement("a")); a.className = "btn small ghost to-map"; a.textContent = "Open on the map";
        li.insertBefore(row, li.querySelector(".act"));
      }
      var link = row.querySelector("a.to-map"), name = row.querySelector("b"), href = (opt.base || "") + "/garden?at=" + at, l = "Open " + p.title + " on the map";
      if (name.textContent !== p.short) name.textContent = p.short;
      if (link.getAttribute("href") !== href) { link.setAttribute("href", href); link.dataset.plot = p.id; }
      if (link.getAttribute("aria-label") !== l) link.setAttribute("aria-label", l);
    });
  }

  return { SIZE: SIZE, SIZE_COARSE: SIZE_COARSE, MARGIN: MARGIN, GAP: GAP, MIN_SEEN: MIN_SEEN,
    targets: targets, layout: layout, inset: inset, keepOff: keepOff, label: label, onScreen: onScreen, edgeBox: edgeBox, edgePoint: edgePoint, atT: atT, toT: toT,
    blockedSpans: blockedSpans, nudge: nudge, compass: compass, plotOf: plotOf, measure: measure, render: render, mapLinks: mapLinks };
})();
if (typeof module !== "undefined") module.exports = BEACONS;
