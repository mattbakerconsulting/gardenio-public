/* NEIGHBOURS (Homestead World phase 2, gd-MFcv2hSfue): whose plan a plot is. Each plot's fence is painted in its operator's
   colour and pattern, so neighbouring plots run by different people read apart at every zoom; the legend lists the operators
   on the farm, and the info card says the operator by name, so identity is never colour alone (design section 6, rule 1).
   With "Mine" on (the header's Mine / Everyone toggle, localStorage garden.scope) the signed-in user's plots stay lit and every
   other plot is dimmed; nothing is hidden, so positions never change (design section 10.3).
   The operator is the plan's lowercased operated_by alias (FarmPlan.operator), else its owner. The colour and pattern come
   from a hash of the alias, probed past ones already taken on this farm: up to 8 operators get 8 colours, up to 4 get 4
   patterns, and the same set of operators always gets the same styles. Drawing uses only fillStyle and fillRect, so it runs
   under the node mock like art.js. */
var NEIGHBOURS = (function () {
  "use strict";
  /* Fence paints: clear of the gold "needs you" ring and of the crop colours (yellow, grey, orange, olive, green) a plot is
     filled with at the Project and Plan levels. Neighbours in the list differ in lightness, so the probe's next pick still
     reads apart in greyscale. */
  var COLOURS = [
    { c: "#3d7bf0", name: "blue" }, { c: "#f3f0ff", name: "chalk" }, { c: "#b04fd8", name: "violet" }, { c: "#7fd8ff", name: "sky" },
    { c: "#c8321e", name: "brick" }, { c: "#ff8cc6", name: "pink" }, { c: "#16867a", name: "teal" }, { c: "#e8a8ff", name: "lilac" }
  ];
  var PATTERNS = ["solid", "dashed", "dotted", "double"], EDGE = "#1a1210", DIM = "rgba(12,16,24,0.6)";

  /* FNV-1a, 32 bit: the operator's preferred colour and pattern. */
  function h32(s) {
    var h = 0x811c9dc5;
    s = String(s);
    for (var i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 0x01000193) >>> 0; }
    return h >>> 0;
  }
  /* whose: the alias a plan or seed belongs to, "" when it names nobody. */
  function whose(x) { return x ? String(x.operator || x.owner || "").trim().toLowerCase() : ""; }
  /* nameOf: an alias as people say it: no "oidc:" scheme, an email's local part. */
  function nameOf(op) {
    var s = String(op || "").replace(/^[a-z][a-z0-9+.-]*:/, ""), at = s.indexOf("@");
    return (at > 0 ? s.slice(0, at) : s) || "nobody";
  }
  function mine(x, me) { return !!me && whose(x) === String(me).toLowerCase(); }

  /* assign: {alias: style} for every operator of a plan or seed in the farm document. Aliases are taken in name order and
     each probes from its hashed colour (and pattern) to the first one nobody holds yet; past 8 (or 4) a colour (pattern)
     repeats, never the pair. */
  function assign(doc) {
    var ops = {}, list, out = {}, usedC = {}, usedP = {}, usedCP = {};
    ((doc && doc.plans) || []).concat((doc && doc.seeds) || []).forEach(function (x) { var o = whose(x); if (o) ops[o] = 1; });
    list = Object.keys(ops).sort();
    list.forEach(function (op) {
      var h = h32(op), c0 = h % COLOURS.length, p0 = (h >>> 8) % PATTERNS.length, c = -1, p = -1, k;
      for (k = 0; k < COLOURS.length && c < 0; k++) if (!usedC[(c0 + k) % COLOURS.length]) c = (c0 + k) % COLOURS.length;
      if (c < 0) c = c0;
      for (k = 0; k < PATTERNS.length && p < 0; k++) if (!usedP[(p0 + k) % PATTERNS.length]) p = (p0 + k) % PATTERNS.length;
      for (k = 0; k < PATTERNS.length && (p < 0 || usedCP[c + "," + p]); k++) p = (p0 + k) % PATTERNS.length;
      usedC[c] = usedP[p] = usedCP[c + "," + p] = 1;
      out[op] = { op: op, name: nameOf(op), colour: COLOURS[c].c, colourName: COLOURS[c].name, pattern: PATTERNS[p] };
    });
    return out;
  }
  /* styleOf: the fence style of a plan or seed, or null for one with no operator (its fence stays plain wood). */
  function styleOf(styles, x) { var o = whose(x); return (o && styles && styles[o]) || null; }
  /* describe: the words for a style, "matt (you), blue dashed fence"; what the card and the legend say. */
  function describe(st, me) {
    if (!st) return "No operator, plain fence";
    return st.name + (me && st.op === String(me).toLowerCase() ? " (you)" : "") + ", " + st.colourName + " " + st.pattern + " fence";
  }
  /* legendRows: the styles for the legend, the viewer's own first, then by name. */
  function legendRows(styles, me) {
    me = String(me || "").toLowerCase();
    return Object.keys(styles || {}).map(function (k) { return styles[k]; }).sort(function (a, b) {
      return (b.op === me) - (a.op === me) || (a.name < b.name ? -1 : a.name > b.name ? 1 : a.op < b.op ? -1 : 1);
    });
  }

  /* lineRects: the painted runs of a pattern along a horizontal line of length len and thickness t, in [x, y, w, h] from
     (0, 0). u is the pattern's unit (world px). The dark edge under them shows through the gaps. */
  function lineRects(len, t, pattern, u) {
    var out = [], on, off, x;
    if (pattern === "double") { var s = Math.max(t / 3, 0.5); return [[0, 0, len, s], [0, t - s, len, s]]; }
    if (pattern === "dashed") { on = 3 * u; off = 1.5 * u; } else if (pattern === "dotted") { on = u; off = u; } else return [[0, 0, len, t]];
    for (x = 0; x < len; x += on + off) out.push([x, 0, Math.min(on, len - x), t]);
    return out;
  }
  /* ringRects: a pattern around the inside edge of r ({x, y, w, h}, world px), band thickness t. The top and bottom runs
     span the full width; the sides fill between them. */
  function ringRects(r, t, pattern, u) {
    var out = [], ih = r.h - 2 * t;
    lineRects(r.w, t, pattern, u).forEach(function (a) { out.push([r.x + a[0], r.y + a[1], a[2], a[3]]); out.push([r.x + a[0], r.y + r.h - t + (t - a[1] - a[3]), a[2], a[3]]); });
    if (ih > 0) lineRects(ih, t, pattern, u).forEach(function (a) { out.push([r.x + a[1], r.y + t + a[0], a[3], a[2]]); out.push([r.x + r.w - t + (t - a[1] - a[3]), r.y + t + a[0], a[3], a[2]]); });
    return out;
  }
  /* bandFor: the band's thickness and pattern unit at zoom z: at least 4 screen px (so a double line reads as two), at least a quarter tile, so a fence reads
     on a 30 px plot at the Project level and sits on the fence rail at the Bead level. */
  function bandFor(z, tile) { var t = Math.max(4 / z, tile / 4); return { t: t, u: Math.max(t, tile / 2) }; }
  /* fence: paint a plot's operator fence over its edge: a dark band, then the pattern in the operator's colour inset by one
     screen px. r in world px; the caller's transform is the world one. */
  function fence(ctx, r, st, z, tile) {
    if (!st) return;
    var b = bandFor(z, tile), e = 1 / z, t = b.t;
    ctx.fillStyle = EDGE;
    ctx.fillRect(r.x, r.y, r.w, t + 2 * e); ctx.fillRect(r.x, r.y + r.h - t - 2 * e, r.w, t + 2 * e);
    ctx.fillRect(r.x, r.y, t + 2 * e, r.h); ctx.fillRect(r.x + r.w - t - 2 * e, r.y, t + 2 * e, r.h);
    ctx.fillStyle = st.colour;
    ringRects({ x: r.x + e, y: r.y + e, w: r.w - 2 * e, h: r.h - 2 * e }, t, st.pattern, b.u).forEach(function (a) { ctx.fillRect(a[0], a[1], a[2], a[3]); });
  }
  /* dim: shade a rectangle that is not the viewer's, with Mine on. */
  function dim(ctx, r) { ctx.fillStyle = DIM; ctx.fillRect(r.x, r.y, r.w, r.h); }
  /* biomeMine: a project region holds a plot or seed of the viewer's (at the World level it is lit, not dimmed). */
  function biomeMine(b, me) {
    return (b.plots || []).some(function (p) { return mine(p.plan, me); }) || (b.nursery || []).some(function (it) { return mine(it.seed, me); });
  }

  var api = { COLOURS: COLOURS, PATTERNS: PATTERNS, h32: h32, whose: whose, nameOf: nameOf, mine: mine, assign: assign, styleOf: styleOf,
    describe: describe, legendRows: legendRows, lineRects: lineRects, ringRects: ringRects, bandFor: bandFor, fence: fence, dim: dim,
    biomeMine: biomeMine, legend: function () {} };
  if (typeof document === "undefined") return api;

  /* legend: the "Whose fence" rows under the legend, one swatch and name per operator on the farm (rebuilt per farm document;
     a handful of rows). Text goes in through textContent only. */
  api.legend = function (styles, me) {
    var host = document.getElementById("legend");
    if (!host || !host.parentNode) return;
    var ul = document.getElementById("legend-ops"), rows = legendRows(styles, me);
    if (!ul) {
      var h = document.createElement("h3"); h.id = "legend-ops-h"; h.textContent = "Whose fence";
      ul = document.createElement("ul"); ul.id = "legend-ops"; ul.setAttribute("aria-labelledby", "legend-ops-h");
      host.parentNode.insertBefore(h, host.nextSibling); host.parentNode.insertBefore(ul, h.nextSibling);
    }
    ul.textContent = "";
    document.getElementById("legend-ops-h").hidden = !rows.length;
    rows.forEach(function (st) {
      var li = document.createElement("li"), c = document.createElement("canvas"), x = c.getContext && c.getContext("2d"), s = document.createElement("span");
      c.width = 36; c.height = 12; c.className = "fence"; c.setAttribute("aria-hidden", "true");
      if (x) {
        x.fillStyle = EDGE; x.fillRect(0, 0, 36, 12); x.fillStyle = st.colour;
        lineRects(34, 6, st.pattern, 4).forEach(function (a) { x.fillRect(1 + a[0], 3 + a[1], a[2], a[3]); });
      }
      s.textContent = describe(st, me);
      li.appendChild(c); li.appendChild(s); ul.appendChild(li);
    });
  };
  return api;
})();
if (typeof module !== "undefined") module.exports = NEIGHBOURS;
