/* CURSOR: the pure (no DOM) cursor world, the grid of grids walked LSDJ-style
   (gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md, sections 3, 4.1 and 6; cited `§n`).
   A tree of grids is built once per WORLD.layout(); the cursor is a path of ids from a world cell down to the cursor's Thing,
   and every function here maps (tree, path, viewport) to plain data, so the Go tests run it under node and the page can never
   make text a function of the pointer: signs() takes no pointer at all (N-D1).
   Coordinates come from indices (the lattice index, the plot's spiral slot, a bed's place in the 3x3, a task's block-major
   place in the 9x9), never from pixels, so a Thing keeps its coordinate whatever else grows (N-D9). Rects come from the layout,
   in tiles; frame() and signs() return world px and screen px. */
var CURSOR = (function () {
  "use strict";
  var W = typeof WORLD !== "undefined" ? WORLD : require("./world.js");
  var A = typeof ART !== "undefined" ? ART : require("./art.js");   // a closed step's harvest badges and words (harvest spec 4.2)
  var E = typeof ESCKIND !== "undefined" ? ESCKIND : require("./esckind.js");   // an escalation's words (web/esckind.js)
  var TILE = W.TILE, SNAPS = W.SNAPS, Z_MIN = W.Z_MIN, Z_MAX = W.Z_MAX;
  var LATTICE_W = W.LATTICE_W;                                        // world columns (N-D8)
  var PCOLS = W.SLOT.cols, PROWS = W.SLOT.rows;                        // a project's 7x5 slot grid
  var ROW = { orchard: 0, graveyard: 1, plots: 2, nursery: 2 + PROWS, crate: 3 + PROWS };   // a project's rows: the bands above (the graveyard past the Orchard, gd-s9KzRk6boh), the plots with the home cell at their centre (29.6), nursery, the "+N" crate (§5.2)
  var PLAN = 3, BED = 9, BLOCK = 3, BAND_W = 9;                        // a plan is 3x3 beds; a bed 9x9 tasks in 3x3 blocks; a band grid is 9 wide
  var PAD = 0.06, CELL_MIN = { fine: 24, coarse: 32 };                 // framing margin and the smallest cell worth framing whole (§4.1)
  /* The child labels and the World's plates and board (§6.1, 6.2): the 12 px monospace glyph width (0.6 em, rounded up: at 7 a
     28 ch line overran by 6 px), line height, plate padding, pointer gap, edge gutter, a board head's name cut (garden.js), the
     child label's cut, its screen width and how many a grid gets. The cursor's Thing has no label: its lineage, title and status
     line are the title bar's (garden.js syncBar, gd-zDdiA3HBh5). */
  var LBL = { ch: 7.25, line: 16, padX: 4, padY: 3, gap: 4, prefix: 16, child: 10, childPx: 56, childCap: 35 };
  var RING = 6;   // how far the cursor's ring reaches outside its Thing in screen px (css/080-cursor.css: the ::before inset): signs keep off it (gd-dS4KEs0ohA)
  var DIRS = { left: [-1, 0], right: [1, 0], up: [0, -1], down: [0, 1] };
  var ZONE = { mailbox: "Mailbox", escalation: "Escalations", growing: "Growing field", barn: "Barn", pen: "Pen" };   // the home cell's zones (28.6, 28.13)
  /* Each zone's [column, row] in the home cell's 4 x 2 grid, the courtyard's quarters (29.2, 29.6): the west quarters in column
     0, the north-east one's Escalations and Mailbox in columns 2 and 3, the Growing field under the Mailbox. Column 1 stays
     empty so the Pen and the Barn are further off than the east cells: Down from the Escalations is the field, never the Pen,
     and Up from the field is the Mailbox, else the Escalations, never the Barn. */
  var ZONE_AT = { barn: [0, 0], escalation: [2, 0], mailbox: [3, 0], pen: [0, 1], growing: [3, 1] }, ZONE_COLS = 4, ZONE_ROWS = 2;
  var BANDS = [["orchard", "Orchard", "tree"], ["graveyard", "Graveyard", "stone"], ["nursery", "Nursery", "seed"]];
  var TEXTLESS = { task: 1, flower: 1, tree: 1, seed: 1, stone: 1, item: 1, goat: 1, escalation: 1 };      // children that never get a short label: glyph only (§6.1)
  var STONE_WORD = { done: "done", abandoned: "abandoned", expired: "expired" };   // a closed wish's outcome on its headstone's status line
  var STATE_WORD = { in_progress: "growing", review: "in review", blocked: "blocked", open: "planted", deferred: "dormant", closed: "closed", asked: "waiting on an answer", planned: "staked" };   // planned: a staked plot's step; asked: a seed whose plan decision is open (9.11b)

  function last(a) { return a[a.length - 1]; }
  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }
  function rectOf(r) { return { x: r.x, y: r.y, w: r.w, h: r.h }; }
  function union(a, b) { return W.union(a, b); }
  function stateWord(st) { return STATE_WORD[st] || st || ""; }
  /* "4m": the page's age format, from a time in unix seconds. */
  function ago(t, now) { var d = Math.max(0, Math.floor(now / 1000) - (t || 0)); return d < 60 ? d + "s" : d < 3600 ? Math.floor(d / 60) + "m" : d < 86400 ? Math.floor(d / 3600) + "h" : Math.floor(d / 86400) + "d"; }
  /* A task's place in a 9x9 bed, block-major (N-D11): tasks 1-9 fill the top-left 3x3 in reading order, 10-18 the top-middle. */
  var blockCell = W.cellAt;
  /* The first words of s that fit in n characters, with an ellipsis when cut (a single long word is cut mid-word). */
  function shortName(s, n) {
    s = String(s || "").trim().replace(/\s+/g, " ");
    if (s.length <= n) return s;
    var out = "", words = s.split(" ");
    for (var i = 0; i < words.length; i++) { var next = out ? out + " " + words[i] : words[i]; if (next.length > n - 1) break; out = next; }
    return (out || s.slice(0, n - 1)) + "…";
  }
  /* Parts that would read zero are left out (§6.4). */
  function line(parts) { return parts.filter(function (p) { return !!p; }).join(" · "); }
  /* A harvest cell's words (harvest spec 5): lead is the base word, who, when and a check that did not go green; icons are every
     badge as {k, w} in priority order; status is the whole status line, lead first and then the badge words in the same order
     (`harvested · @who · 2d ago · merged #825 · graded 5/5 gates · 3 tests · checks green · merged after 2 rounds`). */
  function harvestLine(tk, now) {
    var h = tk.harvest, icons = A.harvestBadges(h), t = h.closed || tk.updated;
    var failed = h.ci && h.ci !== "success" ? "checks " + (/^fail/.test(h.ci) ? "failed" : h.ci) : "";
    var lead = line([A.harvestWord(h), tk.assignee ? "@" + tk.assignee : "", t ? ago(t, now) + " ago" : "", failed]);
    return { lead: lead, icons: icons, status: line([lead].concat(icons.map(function (b) { return b.w; }))) };
  }

  /* ---------- the tree (§3.1) ---------- */
  function mk(t, id, kind, title, rect, extra) {
    /* one shape for every node (the 10x farm builds ~15k of them per layout) */
    var n = { id: id, kind: kind, title: title || id, rect: rect ? rectOf(rect) : null, parent: null, c: 0, r: 0, span: false, cols: 0, rows: 0, cells: null, kids: [],
      key: null, k: -1, zone: "", needs: 0, status: "", lead: "", icons: null, open: "", drawer: undefined, n: 0, ordinal: 0, current: false, done: 0, total: 0, ref: null, geo: null };
    if (extra) for (var k in extra) n[k] = extra[k];
    t.nodes[id] = n;
    return n;
  }
  function grid(n, cols, rows) {
    n.cols = cols; n.rows = rows; n.cells = [];
    for (var r = 0; r < rows; r++) { var row = []; for (var c = 0; c < cols; c++) row.push(null); n.cells.push(row); }
  }
  /* Put kid at (c, r) of parent's grid; a span takes the whole row and sits on its middle column. A taken cell drops the newcomer
     (it is listed in t.dropped), so a malformed document can never put two Things in one cell. */
  function put(t, parent, kid, c, r, span) {
    if (r < 0 || r >= parent.rows || c < 0 || c >= parent.cols) { t.dropped.push(kid.id); delete t.nodes[kid.id]; return false; }
    if (span) { for (var x = 0; x < parent.cols; x++) if (parent.cells[r][x]) { t.dropped.push(kid.id); delete t.nodes[kid.id]; return false; } }
    else if (parent.cells[r][c]) { t.dropped.push(kid.id); delete t.nodes[kid.id]; return false; }
    if (span) { for (x = 0; x < parent.cols; x++) parent.cells[r][x] = kid.id; c = Math.floor((parent.cols - 1) / 2); }
    else parent.cells[r][c] = kid.id;
    kid.parent = parent.id; kid.c = c; kid.r = r; kid.span = !!span;
    parent.kids.push(kid.id);
    return true;
  }
  function byPos(a, b) { return a.y - b.y || a.x - b.x || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); }
  /* A grid's children in reading order, rebuilt from its cells (a spanning cell once). */
  function readKids(n) {
    if (!n.cells) return;
    var out = [];
    for (var r = 0; r < n.rows; r++) for (var c = 0; c < n.cols; c++) { var id = n.cells[r][c]; if (id && id !== out[out.length - 1]) out.push(id); }   // a span's cells are adjacent
    n.kids = out;
  }
  function sumCounts(c) { c = c || {}; return (c.in_progress || 0) + (c.review || 0) + (c.blocked || 0) + (c.open || 0) + (c.deferred || 0) + (c.closed || 0); }

  /* tree(world, home, opts) -> {root, nodes, alias, dropped, now}. world is a WORLD.layout() result. home (optional) is each
     biome's WORLD.homeFill entry by biome id, {mailbox, growing, barn: {items, more, count?}}, its items in tiles in slot order
     (28.2); count is the barn's account (WORLD.barnCount). opts: now (ms), gardeners (the WORLD.gardenerState result, for the
     "3 gardeners" status parts), weeds (id -> count), trucks (TRUCK.place's, the Barn zone's last item, 28.10). */
  function tree(world, home, opts) {
    opts = opts || {};
    var now = opts.now || Date.now(), gst = opts.gardeners || null, weeds = opts.weeds || {};
    var t = { nodes: {}, alias: {}, dropped: [], now: now };
    var bbox = world && world.bbox ? { x: world.bbox.x / TILE, y: world.bbox.y / TILE, w: world.bbox.w / TILE, h: world.bbox.h / TILE } : null;
    var root = t.root = mk(t, "world", "world", "The farm", bbox);
    var biomes = world ? world.biomes : [], taken = {}, ks = {}, maxK = 0;
    /* the lattice index: memo'd by the layout (biome.cell.k); else the Commons is cell 0 and projects follow from 1 in world
       order (homestead-world-design.md 28.5) */
    biomes.forEach(function (b) { var k = b.cell ? b.cell.k : NaN; if (isFinite(k) && k >= 0 && !taken[k]) { ks[b.id] = k; taken[k] = 1; } });
    biomes.forEach(function (b) { if (ks[b.id] === undefined && b.key === "" && !taken[0]) { ks[b.id] = 0; taken[0] = 1; } });
    var next = 1;
    biomes.forEach(function (b) { if (ks[b.id] !== undefined) return; while (taken[next]) next++; ks[b.id] = next; taken[next] = 1; });
    biomes.forEach(function (b) { maxK = Math.max(maxK, ks[b.id]); });
    grid(root, LATTICE_W, Math.floor(maxK / LATTICE_W) + 1);
    biomes.forEach(function (b) { var bn = buildBiome(t, root, b, ks[b.id], gst, weeds); buildHome(t, bn, b, (home || {})[b.id], truckOf(opts, b.key)); });
    for (var id in t.nodes) if (t.nodes[id].kids.length > 1) readKids(t.nodes[id]);
    return t;
  }
  function truckOf(opts, key) { return (opts.trucks || []).filter(function (tr) { return tr && tr.key === key; })[0] || null; }
  /* regraft(tree, biome id, home, opts) -> the replaced nodes by id: the biome's home cell rebuilt in place from its new
     homeFill entry (a ledger patch, CURNAV patch), so a 10x farm's ~15k nodes are not rebuilt for one letter (28.6). */
  function regraft(t, bid, f, opts) {
    var bn = t.nodes[bid], old = {}, hid = bn && bn.ref && bn.ref.home ? bn.ref.home.id : "";
    if (!hid) return old;
    (function walk(k) { var n = t.nodes[k]; if (!n) return; old[k] = n; delete t.nodes[k]; n.kids.forEach(walk); })(hid);
    for (var a in t.alias) if (old[t.alias[a]]) delete t.alias[a];
    bn.cells.forEach(function (row) { for (var c = 0; c < row.length; c++) if (row[c] === hid) row[c] = null; });
    bn.kids = bn.kids.filter(function (id) { return id !== hid; });
    buildHome(t, bn, bn.ref, f, truckOf(opts || {}, bn.key));
    readKids(bn);
    (function walk(k) { var n = t.nodes[k]; if (!n) return; readKids(n); n.kids.forEach(walk); })(hid);
    return old;
  }
  /* The release truck's words (28.10): "release 2026.305.3 loading", or "live on prod". */
  function truckWords(rel) {
    var on = (rel.targets || []).filter(function (d) { return d.tag === rel.tag; }).map(function (d) { return d.name; });
    return rel.status === "live" ? "live" + (on.length ? " on " + on.join(", ") : "") : "release " + rel.tag + " " + (rel.shipping ? "loading" : rel.status);
  }
  /* A home item's place in its zone's grid (28.6): the column and row it is drawn in, from its slot rect and the zone's pitch
     (W.HOME.pitch), so the keeps and the bay a slot skips are holes; a goat's from its pen index. dims are the grid's. */
  function zoneAt(cell, z, pen) {
    if (z === "pen") return { cols: Math.max(1, pen.cols), rows: Math.max(1, pen.rows), at: function (it) { return [it.i % pen.cols, Math.floor(it.i / pen.cols)]; } };
    var p = W.HOME.pitch[z], k = z === "growing" ? p[0] / TILE : 1;   // the field's columns are tiles: two loose beads to a pair slot (28.12)
    return { cols: k * (Math.floor(((cell.w - 2) * TILE - (p[3] || TILE)) / p[0]) + 1), rows: Math.floor(((cell.h - 2) * TILE - p[2]) / p[1]) + 1,
      at: function (it) { return [Math.round((it.x - cell.x - 1) * TILE * k / p[0]), Math.round((it.y - cell.y - 1) * TILE / p[1])]; } };
  }
  /* One home item's node: a loose bead (a Meadow flower), a placed letter, plant or field stand (homeFill's, its row in item; a
     stand opens its gardener and is titled by its line, "<agent> — working: <task>" or "<agent> — resting", 28.12), a goat, which a wish id finds, or an escalation (escOf's, 28.13), which its bead id finds
     unless that is its plot's task. Null when its id is already a node (a plan's task). */
  function homeItem(t, it, z, key) {
    if (!it || !it.id || t.nodes[it.id]) return null;
    if (it.kind === "flower") return mk(t, it.id, "flower", it.task.title, it, { key: key, zone: z, open: it.id, ref: it, status: line([stateWord(it.task.state), it.task.updated ? ago(it.task.updated, t.now) : ""]) });
    if (it.kind === "goat") {
      var w = it.wish, g = mk(t, it.id, "goat", w.title || w.id, it, { key: key, zone: z, open: w.id, ref: it,
        status: line([it.out ? "out grazing by its plan" : "in the pen", w.tending ? "tended" : "untended", w.opened ? ago(w.opened, t.now) : ""]) });
      if (!t.alias[w.id]) t.alias[w.id] = it.id;
      return g;
    }
    if (it.kind === "escalation") {
      var e = it.wait, en = mk(t, it.id, "escalation", e.title || e.id, it, { key: key, zone: z, open: e.id, ref: it, needs: e.human ? 1 : 0,
        status: line([E.word(e), E.since(e, t.now) ? "since " + E.since(e, t.now) : ""]) });
      if (!t.alias[e.id] && !t.nodes[e.id]) t.alias[e.id] = it.id;
      return en;
    }
    var r = it.item || {}, u = r.updated || r.u;
    if (r.g && !t.alias[r.g.id] && !t.nodes[r.g.id]) t.alias[r.g.id] = it.id;   // a stand is found by its gardener's id
    return mk(t, it.id, "item", r.line || r.title, it, { key: key, zone: z, open: r.g ? r.g.id : it.id, ref: it,
      status: line([z === "mailbox" ? "decision waiting on you" : r.where || "", stateWord(r.state), u ? ago(u, t.now) : ""]) });
  }
  /* The home cell (29.6, H-D5): slot 0's cell of every biome's plot grid, reached by the arrows like any plot and a stop even
     when empty (Home must land somewhere), descending into its zones laid out as the courtyard (ZONE_AT), each a stop only when
     it holds something (N-D3), each into its items in slot order, the field's stands put before its flowers. f is the biome's
     homeFill entry; tr its release truck, the Barn's only item (28.10, 28.12). */
  function buildHome(t, bn, b, f, tr) {
    var h = b.home, key = b.key, pen = b.pen || { items: [], more: 0, cols: 1, rows: 1 };
    if (!h || !bn) return;
    var hn = mk(t, h.id, "home", "Home", h, { key: key });
    var at = slotCell(0);
    if (!put(t, bn, hn, at[0], at[1])) return;
    grid(hn, ZONE_COLS, ZONE_ROWS);
    f = f || {};
    var part = function (z) { return f[z] || { items: [], more: 0 }; };
    var esc = h.zones.escalation || { items: [], more: 0 }, escNeeds = 0;
    var gi = part("growing").items, isStand = function (it) { return it.kind === "stand"; };
    var lists = { mailbox: part("mailbox").items, escalation: esc.items, growing: gi.filter(isStand).concat(b.meadow, gi.filter(function (it) { return !isStand(it); })), barn: [], pen: pen.items };
    var more = { mailbox: part("mailbox").more | 0, escalation: esc.more | 0, growing: (h.zones.growing.more | 0) + (part("growing").more | 0), barn: 0, pen: pen.more | 0 };
    var all = {}, working = part("growing").stands !== undefined ? part("growing").stands | 0 : gi.filter(isStand).length, outs = pen.items.filter(function (it) { return it.out; }).length;
    W.HOME.cells.forEach(function (c) {
      var z = c[0], cell = h.cells[z], g = zoneAt(cell, z, pen), zn = mk(t, h.id + "/" + z, "zone", ZONE[z], cell, { key: key, zone: z }), lastR = 0, n = 0;
      grid(zn, g.cols + (z === "barn" && tr ? 1 : 0), g.rows);
      lists[z].forEach(function (it) {
        var kid = homeItem(t, it, z, key), at = kid && g.at(it);
        if (kid && put(t, zn, kid, at[0], at[1])) { n++; lastR = Math.max(lastR, at[1]); escNeeds += kid.kind === "escalation" ? kid.needs : 0; }
      });
      var cr = z === "escalation" && esc.crate, ca = cr && g.at(cr);   // past ESCKIND.CAP: the "+N" crate in the cell's last slot opens the Mail pane on them all (28.11)
      if (cr && !t.nodes[cr.id]) put(t, zn, mk(t, cr.id, "crate", "+" + cr.n + " more escalations", cr, { key: key, zone: z, open: zn.id, n: cr.n, status: "open the Mailbox to see them all" }), ca[0], ca[1]);
      all[z] = n + more[z];
      var mw = more[z] && !cr ? "+" + more[z] + " not shown" : "";
      if (z === "mailbox") { zn.needs = all[z]; zn.status = line([all[z] ? plural(all[z], "decision", "decisions") + " waiting" : "", mw]); }
      else if (z === "escalation") { zn.needs = escNeeds; zn.status = line([E.count(all[z]), escNeeds ? plural(escNeeds, "needs you", "need you") : "", mw]); }
      else if (z === "growing") zn.status = line([working ? working + " working" : "", all[z] > working ? all[z] - working + " growing" : "", mw]);
      else if (z === "pen") zn.status = all[z] ? line([plural(all[z], "open wish", "open wishes"), outs ? outs + " out grazing" : ""]) : "";
      else {
        var tk = tr && !t.nodes[tr.id] ? mk(t, tr.id, "truck", "Release " + tr.tag, tr.bay, { key: key, zone: z, open: tr.id, ref: tr, status: truckWords(tr.rel) }) : null;
        if (tk) put(t, zn, tk, g.cols, lastR);
        zn.status = line([part("barn").count ? W.barnWords(part("barn").count, true) : "", tk ? tk.status : ""]);
      }
      if (zn.kids.length || (z === "barn" && part("barn").count && part("barn").count.n)) put(t, hn, zn, ZONE_AT[z][0], ZONE_AT[z][1]); else delete t.nodes[zn.id];   // the Barn holds no figure, but its account of every running agent stays a stop (28.12)
    });
    var words = W.homeWords({ letters: all.mailbox, escalations: all.escalation, working: working, field: all.growing - working, wishes: all.pen });
    hn.needs = all.mailbox + escNeeds;
    hn.status = line([hn.needs ? plural(hn.needs, "needs you", "need you") : "", words]) || "No letters · nobody in the field · nothing growing";
  }
  /* Spiral slot k's [column, row] in a project's grid: the 7x5 slot grid in ROW.plots onwards, slot 0 (the farmstead) its centre. */
  function slotCell(k) { var s = W.SPIRAL[k]; return s ? [s.c + (PCOLS - 1) / 2, ROW.plots + s.r + (PROWS - 1) / 2] : null; }
  function buildBiome(t, root, b, k, gst, weeds) {
    var key = b.key, nplans = b.plots.length + (b.overflow ? b.overflow.n : 0), gb = gst && gst[3] && gst[3].biomes[b.id];
    var bn = mk(t, b.id, "biome", b.name, b.used, { key: key, k: k, needs: b.needs || 0, ref: b,
      status: line([plural(nplans, "plan", "plans"), b.needs ? plural(b.needs, "needs you", "need you") : "", gb && gb.all ? plural(gb.all, "gardener", "gardeners") : "", weeds[b.id] ? plural(weeds[b.id], "weed", "weeds") : ""]) });
    put(t, root, bn, k % LATTICE_W, Math.floor(k / LATTICE_W));
    grid(bn, PCOLS, ROW.crate + 1);
    b.plots.forEach(function (p) {
      var at = slotCell(p.slot);
      if (!at) return;
      put(t, bn, buildPlot(t, p, key, gst, weeds), at[0], at[1]);
    });
    /* the bands: one spanning cell each (the home cell is buildHome's) */
    BANDS.forEach(function (bd) { buildBand(t, bn, key, bd, b[bd[0]] || [], ROW[bd[0]], b.bands && b.bands[bd[0]]); });
    /* past 35 plans: the "+N more plans" crate in its own row; A opens the Plans drawer (§5.2) */
    if (b.overflow) {
      var cr = mk(t, b.overflow.id, "crate", "+" + b.overflow.n + " more plans", b.overflow, { key: key, drawer: key, n: b.overflow.n });
      if (put(t, bn, cr, 0, ROW.crate, true)) b.overflow.ids.forEach(function (id) { if (!t.alias[id]) t.alias[id] = cr.id; });
    }
    return bn;
  }
  /* A band (bd: [name, title, item kind]) of list in parent's row r: one spanning cell, descending into a 9-wide grid of its items
     in reading order; an empty band is no cell. */
  function buildBand(t, parent, key, bd, list, r, rect) {
    var items = list.slice().sort(byPos);
    if (!items.length) return;
    var grave = bd[0] === "graveyard";
    var band = mk(t, "band:" + key + "/" + bd[0], "band", bd[1], rect || items.reduce(union, null), { key: key, status: plural(items.length, grave ? "closed wish" : "bead", grave ? "closed wishes" : "beads") });
    if (!put(t, parent, band, 0, r, true)) return;
    grid(band, BAND_W, Math.floor((items.length - 1) / BAND_W) + 1);
    items.forEach(function (it, j) {
      /* a headstone (gd-s9KzRk6boh) reads its wish's short title and when it closed; it opens the wish's head bead */
      if (grave) {
        var w = it.wish || {}, sn = mk(t, it.id, "stone", w.title, it, { key: key, open: w.id, ref: it, status: line([STONE_WORD[w.outcome] || "done", w.closed ? "closed " + ago(w.closed, t.now) + " ago" : ""]) });
        if (put(t, band, sn, j % BAND_W, Math.floor(j / BAND_W)) && w.id && !t.alias[w.id]) t.alias[w.id] = it.id;
        return;
      }
      var ref = it.task || it.plan || it.seed || {}, st = bd[0] === "orchard" ? "harvested" : stateWord(ref.state);
      var n = mk(t, it.id, bd[2], ref.title, it, { key: key, open: it.id, ref: it, status: line([st, ref.updated ? ago(ref.updated, t.now) : ""]) });
      put(t, band, n, j % BAND_W, Math.floor(j / BAND_W));
    });
  }
  function buildPlot(t, p, key, gst, weeds) {
    var plan = p.plan || {}, pc = plan.counts || {}, total = sumCounts(pc), phases = plan.phases || [];
    var cur = -1;
    for (var i = 0; i < phases.length; i++) if (phases[i].status !== "closed") { cur = i; break; }
    var gp = gst && gst[3] && gst[3].plots[p.id];
    /* a staked plot's sign: the plan's short title, and "staked" in its status (homestead-world-design.md 26.14) */
    var pn = mk(t, p.id, "plot", p.staked && plan.short_title || plan.title, p, { key: key, needs: p.needs || 0, open: p.id, ref: p,
      status: line([p.staked ? "staked" : "", phases.length && cur >= 0 ? "phase " + (cur + 1) + " of " + phases.length : "", p.staked ? plural(total, "step", "steps") : total ? (pc.closed || 0) + "/" + total + " steps" : "", p.needs ? plural(p.needs, "needs you", "need you") : "",
        gp && gp.counts.all ? plural(gp.counts.all, "gardener", "gardeners") : "", weeds[p.id] ? plural(weeds[p.id], "weed", "weeds") : ""]) });
    grid(pn, PLAN, PLAN);
    var beds = p.beds || [], crate = p.crate || null, room = PLAN * PLAN;
    /* past 9: eight beds and a crate in cell 9 (§5.3) */
    beds.forEach(function (bed, j) {
      var bi = bed.at ? bed.at.r * PLAN + bed.at.c : j;
      if (bi >= (crate ? room - 1 : room)) return;
      var other = bed.other || !bed.id, ph = other ? plan.unphased : phases.filter(function (x) { return x.id === bed.id; })[0];
      var bn = buildBed(t, bed, p, ph || {}, bi, key, other, bed.current !== undefined ? bed.current : cur >= 0 && ph === phases[cur]);
      put(t, pn, bn, bi % PLAN, Math.floor(bi / PLAN));
    });
    if (crate) {
      var cn = mk(t, p.id + "/phases", "crate", "+" + crate.n + " phases", crate, { key: key, open: p.id, n: crate.n, status: "more than 9 phases" });
      var ci = crate.at ? crate.at.r * PLAN + crate.at.c : room - 1;
      put(t, pn, cn, ci % PLAN, Math.floor(ci / PLAN));
    }
    return pn;
  }
  function buildBed(t, bed, p, ph, bi, key, other, current) {
    var id = other && !bed.id ? p.id + "/other" : bed.id, c = ph.counts || {}, total = isFinite(bed.total) ? bed.total : sumCounts(c);
    var bn = mk(t, id, "bed", bed.title, bed, { key: key, ordinal: bi + 1, open: other ? p.id : bed.id, current: !!current, ref: bed, done: isFinite(bed.done) ? bed.done : c.closed || 0, total: total,
      status: line([total ? (c.closed || 0) + "/" + total + " steps" : "", c.blocked ? c.blocked + " blocked" : "", c.in_progress ? c.in_progress + " growing" : ""]) });
    grid(bn, BED, BED);
    var cells = bed.cells || [];
    cells.forEach(function (cell, j) {
      var ci = isFinite(cell.i) ? cell.i : j, tk = cell.task || {};
      if (ci >= BED * BED) return;
      var at = blockCell(ci), n = mk(t, cell.id, "task", tk.title, cell, { key: key, open: cell.id, ref: cell,
        status: line([stateWord(tk.state), tk.assignee ? "@" + tk.assignee : "", tk.updated ? ago(tk.updated, t.now) : ""]) });
      if (tk.state === "closed" && tk.harvest) { var hv = harvestLine(tk, t.now); n.status = hv.status; n.lead = hv.lead; n.icons = hv.icons; }   // a harvest cell (harvest spec 5)
      put(t, bn, n, at.c, at.r);
    });
    /* the "+N" crate after the tasks and the stubble (§5.4); A opens the phase's bead view */
    var over = bed.crate ? bed.crate.n : 0;
    if (over > 0) {
      var ci = isFinite(bed.crate.i) ? bed.crate.i : Math.min(BED * BED - 1, cells.length + (bed.stubble || 0)), at = blockCell(ci);
      put(t, bn, mk(t, id + "/more", "crate", "+" + over + " more steps", bed.crate, { key: key, open: bn.open, n: over }), at.c, at.r);
    }
    return bn;
  }

  /* ---------- paths and links (§3.5) ---------- */
  function chain(t, id) { var out = [], n = t.nodes[id]; while (n && n.parent) { out.unshift(n.id); n = t.nodes[n.parent]; } return out; }
  /* pathFor(tree, id) -> the path to a Thing, or null. id is a node id, a bead id (a plan past the plot cap finds its crate, a
     wish its goat) or an at= value (`gardenio`, `gardenio/<id>`, `~/<id>`, `gardenio/home`, `gardenio/<zone>`; 28.7). An at=
     whose id is gone falls back to its project, as WORLD.resolve does; a zone with nothing in it to its home cell. A legacy
     `yard` link (H-D12) lands on the thing it names wherever it is, the Commons's pen for `yard/pen`, else the Commons's home. */
  function pathFor(t, id) {
    if (id === undefined || id === null || id === "" || !t) return null;
    id = String(id);
    if (t.nodes[id] && id !== "world") return chain(t, id);
    if (t.alias[id]) return chain(t, t.alias[id]);
    var q = W.parseAt(id);
    if (!q) return null;
    var bid = "project:" + q.project, b = t.nodes[bid], hid = "home:" + q.project, found = function (x) { return x && t.nodes[x] && x !== "world" ? x : t.alias[x]; };
    if (!b && q.raw.toLowerCase() === "yard") { var y = (q.id === "pen" ? found("home:/pen") : found(q.id)) || "home:"; return t.nodes[y] ? chain(t, y) : null; }
    if (b && ZONE_RE.test(q.id)) return chain(t, q.id !== "home" && t.nodes[hid + "/" + q.id] ? hid + "/" + q.id : t.nodes[hid] ? hid : bid);
    if (q.id) { var x = found(q.id); return x ? chain(t, x) : b ? [bid] : null; }
    return b ? [bid] : null;
  }
  var ZONE_RE = /^(home|mailbox|escalation|growing|barn|pen)$/;   // the reserved id words (28.7)
  /* atFor(tree, path) -> the at= value for the cursor's Thing: what WORLD.resolve reads back for every Thing it knows; a field
     stand its field's, <project>/growing, as WORLD.atFor says (28.7, 28.12). */
  function atFor(t, path) {
    var n = path && path.length ? t.nodes[last(path)] : null;
    if (!n) return "";
    var seg = n.key === "" ? "~" : encodeURIComponent(n.key);
    return n.kind === "biome" ? seg : seg + "/" + (n.kind === "home" ? "home" : n.kind === "zone" || (n.ref && n.ref.kind === "stand") ? n.zone : encodeURIComponent(n.id));
  }

  /* ---------- moving (§3.2, 3.3) ---------- */
  function gridOf(t, path) { return path.length > 1 ? t.nodes[path[path.length - 2]] : t.root; }
  function hasKids(n) { return !!(n && n.cells && n.kids.length); }
  /* The occupied cell an arrow picks from (c, r) in direction d (§3.2): the nearest in the same row (column for up and down)
     ahead, else the nearest ahead by distance along d, then off-axis distance, ties to the smaller row (column). A spanning cell
     (a band, the plans crate) is in every column of its row and is reached up or down only. Pure in the occupied cells, so the
     same farm gives the same walk. */
  function pick(t, g, c, r, d, self) {
    var best = null, bk = null;
    for (var y = 0; y < g.rows; y++) for (var x = 0; x < g.cols; x++) {
      var id = g.cells[y][x];
      if (!id || id === self || (d[0] && t.nodes[id].span)) continue;
      var along = (x - c) * d[0] + (y - r) * d[1], off = d[0] ? y - r : x - c, k = [off === 0 ? 0 : 1, along, Math.abs(off), d[0] ? y : x];
      if (along <= 0) continue;
      if (!bk || k[0] < bk[0] || (k[0] === bk[0] && (k[1] < bk[1] || (k[1] === bk[1] && (k[2] < bk[2] || (k[2] === bk[2] && k[3] < bk[3])))))) { best = id; bk = k; }
    }
    return best;
  }
  /* move(tree, path, dir, col) -> {path, bumped, col}. dir is left, right, up or down. col is the column the page kept from the
     last move (a spanning band has none of its own: down from it returns to the column the cursor came up from, §5.2). */
  function move(t, path, dir, col) {
    var n = path && path.length ? t.nodes[last(path)] : null, g = n ? gridOf(t, path) : null, d = DIRS[dir];
    if (!n || !g || !d) return { path: path ? path.slice() : [], bumped: true, col: col };
    var c = n.span && isFinite(col) && col >= 0 && col < g.cols ? col : n.c, id = n.span && d[0] ? null : pick(t, g, c, n.r, d, n.id);
    if (!id) return { path: path.slice(), bumped: true, col: c };
    return { path: path.slice(0, -1).concat(id), bumped: false, col: t.nodes[id].span ? c : t.nodes[id].c };
  }
  /* The child of grid g at (c, r) for a cursor coming down into it (§3.3, 3.4): the coordinate clamped to the used bounds, else
     the nearest occupied cell by Manhattan distance, ties in reading order. Cells like `like` (its kind, and having children when
     it does) are preferred, so a sibling jump lands plot on plot and bed on bed. */
  function landAt(t, g, c, r, like) {
    if (!hasKids(g)) return null;
    var cand = [], want = function (n) { return !like || (n.kind === like.kind && (!hasKids(like) || hasKids(n))); };
    var scan = function (strict) {
      for (var y = 0; y < g.rows; y++) for (var x = 0; x < g.cols; x++) { var id = g.cells[y][x]; if (id && (!strict || want(t.nodes[id]))) cand.push({ id: id, c: x, r: y }); }
    };
    scan(true); if (!cand.length) scan(false);
    var c0 = Infinity, c1 = -1, r0 = Infinity, r1 = -1;
    cand.forEach(function (e) { c0 = Math.min(c0, e.c); c1 = Math.max(c1, e.c); r0 = Math.min(r0, e.r); r1 = Math.max(r1, e.r); });
    c = Math.max(c0, Math.min(c1, c)); r = Math.max(r0, Math.min(r1, r));
    var best = null, bd = Infinity;
    cand.forEach(function (e) { var dd = Math.abs(e.c - c) + Math.abs(e.r - r); if (dd < bd) { bd = dd; best = e.id; } });   // cand is in reading order, so the first at a distance wins ties
    return best;
  }
  /* The next sibling like `like` in direction dir from path, at the same depth, or null at the grid's edge. */
  function stepLike(t, path, dir, like) {
    var m = move(t, path, dir), col = m.col;
    while (!m.bumped) {
      var n = t.nodes[last(m.path)];
      if (n.kind === like.kind && hasKids(n)) return m.path;
      m = move(t, m.path, dir, col); col = m.col;
    }
    return null;
  }
  /* jump(tree, path, dir) -> {path, bumped}: Shift+arrow (§3.3). The parent moves to its next sibling grid in dir and the path
     comes back down on the child at the same coordinate; a parent at its edge bubbles the step up a level. At the world's edge
     (and at depth 0, where the world is the only grid) it bumps. */
  function jump(t, path, dir) {
    if (!path || path.length < 2 || !DIRS[dir] || !t.nodes[last(path)]) return { path: path ? path.slice() : [], bumped: true };
    var up = path.slice(0, -1), parent = t.nodes[last(up)], base = up, np = stepLike(t, up, dir, parent);
    /* bubbled down onto a container with nothing inside (a plan with no phases): keep going the same way */
    for (;;) {
      if (!np) { var j = jump(t, base, dir); if (j.bumped) return { path: path.slice(), bumped: true }; np = j.path; }
      if (hasKids(t.nodes[last(np)])) break;
      base = np; np = stepLike(t, np, dir, parent);
    }
    var old = t.nodes[last(path)];
    return { path: np.concat(landAt(t, t.nodes[last(np)], old.c, old.r, old)), bumped: false };
  }
  /* descend(tree, path, memory, needs) -> {path, bumped, open?, drawer?} (N-D4). Into the child last left in this grid
     (memory: container id -> child id), else the first child holding something that needs you in `needs` order (beacon order;
     ids or at= values), else the first child in reading order. On a leaf, or a container with nothing inside, open is the bead
     to open (a crate's: the plan or phase it stands for); the plans crate gives drawer, the project whose Plans drawer opens. */
  function descend(t, path, memory, needs) {
    var n = path && path.length ? t.nodes[last(path)] : null;
    if (!n) return { path: path ? path.slice() : [], bumped: true };
    if (!hasKids(n)) {
      if (n.drawer !== undefined) return { path: path.slice(), bumped: false, drawer: n.drawer };
      if (n.open) return { path: path.slice(), bumped: false, open: n.open };
      return { path: path.slice(), bumped: true };
    }
    var kid = memory && memory[n.id] && t.nodes[memory[n.id]] && t.nodes[memory[n.id]].parent === n.id ? memory[n.id] : null;
    for (var i = 0; !kid && needs && i < needs.length; i++) {
      var p = pathFor(t, needs[i]), d = p ? p.indexOf(n.id) : -1;
      if (d >= 0 && d + 1 < p.length) kid = p[d + 1];
    }
    return { path: path.concat(kid || firstKid(t, n)), bumped: false };
  }
  /* The first child in reading order; in a project, the first plot, so the bands and the crate above and below the plots are
     not where a descend lands unless that is all there is. */
  function firstKid(t, n) {
    for (var i = 0; n.kind === "biome" && i < n.kids.length; i++) if (t.nodes[n.kids[i]].kind === "plot") return n.kids[i];
    return n.kids[0];
  }
  /* ascend(tree, path) -> {path, bumped}: the cursor goes to the container; at depth 0 nothing. */
  function ascend(t, path) {
    if (!path || path.length < 2) return { path: path ? path.slice() : [], bumped: true };
    return { path: path.slice(0, -1), bumped: false };
  }
  /* follow(tree, prevTree, path) -> path (§3.4). The cursor keeps its Thing by id across layouts while it exists. When it is
     gone, the cursor comes down from the deepest surviving container onto the cell at the gone Thing's coordinate (prevTree
     says where it was), or the nearest occupied one; a grid with nothing in it keeps the cursor on its container. */
  function follow(t, prev, path) {
    if (!path || !path.length) return [];
    var id = last(path);
    if (t.nodes[id]) return chain(t, id);
    var i = path.length - 2;
    while (i >= 0 && !t.nodes[path[i]]) i--;
    var out = i >= 0 ? chain(t, path[i]) : [];
    for (var j = i + 1; j < path.length; j++) {
      var g = out.length ? t.nodes[last(out)] : t.root, old = prev && prev.nodes[path[j]], kid = landAt(t, g, old ? old.c : 0, old ? old.r : 0, old || null);
      if (!kid) break;
      out.push(kid);
    }
    return out;
  }

  /* ---------- the camera's framing (§4.1) ---------- */
  function occupied(t, g) { return g.kids.map(function (id) { return t.nodes[id]; }); }
  /* The grid's used bounds in tiles: its occupied cells' rects; at depth 3 grown to whole 3x3 blocks. */
  function usedBounds(t, g) {
    var box = null;
    occupied(t, g).forEach(function (n) { if (n.rect) box = union(box, n.rect); });
    if (box && g.kind === "bed") {
      var ext = coordBounds(t, g), geo = geometry(t, g);
      box = union(box, cellRect(geo, ext.c0, ext.r0)); box = union(box, cellRect(geo, ext.c1 - 1, ext.r1 - 1));
    }
    return box || g.rect || { x: 0, y: 0, w: 1, h: 1 };
  }
  /* The occupied coordinate range [c0, c1) x [r0, r1), whole blocks in a bed. */
  function coordBounds(t, g) {
    var b = { c0: Infinity, c1: -Infinity, r0: Infinity, r1: -Infinity };
    occupied(t, g).forEach(function (n) {
      var cc0 = n.span ? 0 : n.c, cc1 = n.span ? g.cols : n.c + 1;
      b.c0 = Math.min(b.c0, cc0); b.c1 = Math.max(b.c1, cc1); b.r0 = Math.min(b.r0, n.r); b.r1 = Math.max(b.r1, n.r + 1);
    });
    if (g.kind === "bed") { b.c0 = Math.floor(b.c0 / BLOCK) * BLOCK; b.r0 = Math.floor(b.r0 / BLOCK) * BLOCK; b.c1 = Math.ceil(b.c1 / BLOCK) * BLOCK; b.r1 = Math.ceil(b.r1 / BLOCK) * BLOCK; }
    return b;
  }
  /* A grid's cell pitch, cell size and origin in tiles, from its occupied cells' rects (a uniform grid: beds, plans, projects).
     Cached on the node; spans are left out. */
  function geometry(t, g) {
    if (g.geo) return g.geo;
    var kids = occupied(t, g).filter(function (n) { return !n.span && n.rect; }), px = Infinity, py = Infinity, cw = Infinity, ch = Infinity;
    kids.forEach(function (a, i) {
      cw = Math.min(cw, a.rect.w); ch = Math.min(ch, a.rect.h);
      for (var j = i + 1; j < kids.length; j++) {
        var b = kids[j];
        if (a.c !== b.c) px = Math.min(px, Math.abs((b.rect.x - a.rect.x) / (b.c - a.c)));
        if (a.r !== b.r) py = Math.min(py, Math.abs((b.rect.y - a.rect.y) / (b.r - a.r)));
      }
    });
    if (!kids.length) { cw = ch = 1; }
    if (!(px > 0) || !isFinite(px)) px = cw; if (!(py > 0) || !isFinite(py)) py = ch;
    var a0 = kids[0];
    g.geo = { px: px, py: py, cw: cw, ch: ch, ox: a0 ? a0.rect.x - a0.c * px : 0, oy: a0 ? a0.rect.y - a0.r * py : 0 };
    return g.geo;
  }
  /* The cell size (tiles) the CELL_MIN rule reads: the smallest cell of a uniform grid; in the world, whose regions differ, the
     median region, so one small region does not zoom a whole lattice in. */
  function cellSize(t, g, geo) {
    if (g.kind !== "world") return Math.min(geo.cw, geo.ch);
    var s = occupied(t, g).filter(function (n) { return n.rect; }).map(function (n) { return Math.min(n.rect.w, n.rect.h); }).sort(function (a, b) { return a - b; });
    return s.length ? s[Math.floor(s.length / 2)] : 1;
  }
  function cellRect(geo, c, r) { return { x: geo.ox + c * geo.px, y: geo.oy + r * geo.py, w: geo.cw, h: geo.ch }; }
  function insets(vp) { var l = vp.left || 0, r = vp.right || 0, tp = vp.top || 0, b = vp.bottom || 0; return { x: l, y: tp, w: Math.max(1, vp.w - l - r), h: Math.max(1, vp.h - tp - b) }; }
  /* The resting zoom for a fit: at or above 1, the largest of SNAPS not above it, so pixel art stays crisp (§4.1). */
  function rest(z) {
    z = Math.max(Z_MIN, Math.min(Z_MAX, z));
    if (z < 1) return z;
    var best = SNAPS[0];
    SNAPS.forEach(function (s) { if (s <= z + 1e-9) best = s; });
    return best;
  }
  /* The smallest resting zoom at or above z. */
  function restUp(z) {
    if (z < 1) return Math.max(Z_MIN, z);
    for (var i = 0; i < SNAPS.length; i++) if (SNAPS[i] >= z - 1e-9) return SNAPS[i];
    return Z_MAX;
  }
  /* The camera centre that puts world rect r (tiles) in the middle of the inset view at zoom z. */
  function centreOn(r, view, vp, z) {
    return { cx: (r.x + r.w / 2) * TILE - (view.x + view.w / 2 - vp.w / 2) / z, cy: (r.y + r.h / 2) * TILE - (view.y + view.h / 2 - vp.h / 2) / z };
  }
  /* frame(tree, path, vp, opts) -> {cx, cy, z, window, bounds}. vp: {w, h} CSS px, with the HUD insets top/right/bottom/left
     and coarse (a touch pointer), and zmin (a zoom the fit does not rest below, so a refit inside a beacon's band keeps the
     level of detail it had without one, gd-SWjiYouIQt). opts: prev (the last window, so a window scrolls only at its edge). Fits the current grid's used bounds with 6% padding (N-D5); when a cell would be under CELL_MIN px it frames a
     window of whole rows and columns (whole blocks at depth 3) around the cursor at the zoom where cells meet CELL_MIN. A project
     is framed on its farmstead (29.6): its grid's bounds grow to be centred there, zooming out no further than the level of detail
     the bounds alone fit at, where the camera moves off the farmstead only as far as keeps the bounds in view; and a window of the
     lattice is moved to centre the cursor's project's farmstead. */
  function frame(t, path, vp, opts) {
    opts = opts || {}; vp = vp || { w: 1280, h: 800 };
    var n = path && path.length ? t.nodes[last(path)] : null, g = n ? gridOf(t, path) : t.root;
    if (!hasKids(g)) { var rr = g.rect || { x: 0, y: 0, w: 1, h: 1 }; return { cx: (rr.x + rr.w / 2) * TILE, cy: (rr.y + rr.h / 2) * TILE, z: 1, window: null, bounds: W.tiles(rr) }; }
    var view = insets(vp), used = usedBounds(t, g), farm = farmOf(g.kind === "biome" ? g : n), box = g.kind === "biome" && farm ? around(used, farm) : used;
    var fitZ = fitOf(box, view), uz = box === used ? 0 : fitOf(used, view);
    if (uz) fitZ = Math.max(fitZ, Math.min(uz, W.levelRange(W.lodFor(uz))[0]));   // centring on the farmstead never drops a level of detail: past it the edges are cut
    var geo = geometry(t, g), min = vp.coarse ? CELL_MIN.coarse : CELL_MIN.fine, cell = cellSize(t, g, geo) * TILE, z = rest(Math.max(fitZ, vp.zmin || 0)), win = null, focus = box;
    var floor = g.kind === "plot" || g.kind === "bed" ? W.levelRange(2)[0] : 0;   // inside a plot the art is never under the Plan depth's
    /* a window when cells would be under CELL_MIN, the grid does not fit even at the widest zoom (a phone sees a window of the
       lattice, §12.3), or a plot's grid fits only under the Plan depth (a short map: a phone sideways, gd-e83LE907oK) */
    if (n && !lattice(g)) {
      /* a home cell: its zones are places of their own sizes, not a lattice to take whole rows and columns of. Its art is drawn
         a pixel to a pixel at z 1 and what a zone holds is a tile (a figure, a letter, a plant), so a view that cannot hold the
         whole farmstead at z 1 frames the cursor's own zone, fitted, zoomed in no further than a tile needs (gd-bkePy06Ont) */
      if (z < 1) {
        focus = n.rect || box;
        z = Math.min(restUp(min / TILE), rest(fitOf(focus, view)));
        win = { grid: g.id, c0: n.c, r0: n.r, cols: 1, rows: 1 };
      }
    } else if (n && (cell * z < min || fitZ < Z_MIN || z < floor)) {
      z = Math.min(Z_MAX, Math.max(floor, restUp(min / cell)));
      var unit = g.kind === "bed" ? BLOCK : 1, cb = coordBounds(t, g);
      var span = function (avail, pitch, lo, hi) { var k = Math.max(unit, Math.floor(avail / (pitch * TILE * z * (1 + 2 * PAD)) / unit) * unit); return Math.min(k, hi - lo); };
      var nc = span(view.w, geo.px, cb.c0, cb.c1), nr = span(view.h, geo.py, cb.r0, cb.r1);
      var place = function (cur, size, lo, hi, prev0) {
        var a = Math.floor(cur / unit) * unit, s;
        if (isFinite(prev0) && prev0 >= lo && prev0 + size <= hi) {
          s = prev0;
          if (size <= 2 * unit) { if (cur < s) s = a; else if (cur >= s + size) s = a + unit - size; }   // too small to keep a margin: scroll when the cursor leaves
          else {
            while (cur < s + unit && s > lo) s -= unit;                 // within a cell (a block) of the near edge: scroll one step
            while (cur >= s + size - unit && s + size < hi) s += unit;
          }
        } else s = a - Math.floor((size - unit) / 2 / unit) * unit;
        return Math.max(lo, Math.min(hi - size, s));
      };
      var pw = opts.prev && opts.prev.grid === g.id && opts.prev.cols === nc && opts.prev.rows === nr ? opts.prev : null;
      var c0 = place(n.c, nc, cb.c0, cb.c1, pw ? pw.c0 : NaN), r0 = place(n.r, nr, cb.r0, cb.r1, pw ? pw.r0 : NaN);
      win = { grid: g.id, c0: c0, r0: r0, cols: nc, rows: nr };
      focus = union(cellRect(geo, c0, r0), cellRect(geo, c0 + nc - 1, r0 + nr - 1));
      if (g.kind === "world" && farm) focus = { x: farm.x + farm.w / 2 - focus.w / 2, y: farm.y + farm.h / 2 - focus.h / 2, w: focus.w, h: focus.h };
    }
    var cam = centreOn(focus, view, vp, z);
    cam.z = z;
    if (uz && !win) cam = keepIn(used, cam, view, vp);   // the farmstead as near the middle as the grid's bounds, kept in view, allow
    if (n && n.rect) cam = keepIn(n.rect, cam, view, vp);
    return { cx: cam.cx, cy: cam.cy, z: cam.z, window: win, bounds: W.tiles(used) };
  }
  /* The farmstead of project node b (its slot-0 rect, tiles), or null for anything else. */
  function farmOf(b) { var h = b && b.kind === "biome" && b.ref && b.ref.home; return h ? { x: h.x, y: h.y, w: h.w, h: h.h } : null; }
  /* The smallest rect holding box whose centre is r's. */
  function around(box, r) {
    var cx = r.x + r.w / 2, cy = r.y + r.h / 2, hw = Math.max(cx - box.x, box.x + box.w - cx), hh = Math.max(cy - box.y, box.y + box.h - cy);
    return { x: cx - hw, y: cy - hh, w: 2 * hw, h: 2 * hh };
  }
  /* The zoom that fits world rect r (tiles) in the inset view with the framing margin. */
  function fitOf(r, view) { return Math.min(view.w / (r.w * TILE * (1 + 2 * PAD)), view.h / (r.h * TILE * (1 + 2 * PAD))); }
  /* Whether grid g's cells sit on a uniform lattice a window can take whole rows and columns of: every grid but a home cell. */
  function lattice(g) { return g.kind !== "home"; }
  /* The cursor's own cell stays in the view (gd-bkePy06Ont): a fit that rests above the grid's own (vp.zmin, a snap), or a window
     whose lattice cells are not where the Thing is drawn (a band's seeds, a project in the world), can leave it over an edge or
     off the screen. It is panned in, keeping up to the framing margin from the edge it was over; one bigger than the view is
     fitted and centred, zooming out, never in. */
  function keepIn(r, cam, view, vp) {
    var s = screenRect(r, cam, vp);
    if (within(s, view)) return cam;
    if (s.w > view.w || s.h > view.h) { var z = Math.min(cam.z, rest(fitOf(r, view))), c = centreOn(r, view, vp, z); c.z = z; return c; }
    var shift = function (lo, size, vlo, vsize) {
      var m = Math.min(size * PAD, (vsize - size) / 2);
      return lo < vlo ? lo - vlo - m : lo + size > vlo + vsize ? lo + size - vlo - vsize + m : 0;
    };
    return { cx: cam.cx + shift(s.x, s.w, view.x, view.w) / cam.z, cy: cam.cy + shift(s.y, s.h, view.y, view.h) / cam.z, z: cam.z };
  }

  /* ---------- signs (§6) ---------- */
  function screenRect(r, cam, vp) { return { x: (r.x * TILE - cam.cx) * cam.z + vp.w / 2, y: (r.y * TILE - cam.cy) * cam.z + vp.h / 2, w: r.w * TILE * cam.z, h: r.h * TILE * cam.z }; }
  function overlap(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }
  function within(a, v) { return a.x >= v.x && a.y >= v.y && a.x + a.w <= v.x + v.w && a.y + a.h <= v.y + v.h; }
  /* A World header plate's box by its project's screen rect th (gd-zceqmx40pD), h high: whole (w wide) then its name alone (wt),
     each flush with the project's left edge, else its right, over it, inside its top edge, under it or inside its foot; then
     the same pushed into the view v. The first inside v, clear of the rects in taken and of every other project's rect in
     projects, else null. brief: the name alone. */
  function plateBox(th, w, wt, h, v, taken, projects) {
    var b = null, ys = [th.y - LBL.gap - h, th.y + 2, th.y + th.h + LBL.gap, th.y + th.h - h - 2];
    var ok = function (c) { return within(c, v) && !taken.concat(projects).some(function (o) { return overlap(o, c); }); };
    [[w, 0], [wt, 0], [w, 1], [wt, 1]].some(function (wp) {
      var hw = wp[0], xs = wp[1] ? [Math.max(v.x + 4, Math.min(v.x + v.w - hw, th.x))] : [th.x, th.x + th.w - hw];
      return ys.some(function (y) { return xs.some(function (x) { var c = { x: x, y: y, w: hw, h: h, brief: hw < w }; if (ok(c)) b = c; return !!b; }); });
    });
    return b;
  }
  /* The World depth's header plates (gd-zceqmx40pD), in screen px: one for each of heads ({id, r, w, wt, h}, nearest first;
     plateBox in pv, clear of the other projects' rects in projects ({id, r}) and of the plates placed before it). taken: what
     every plate keeps off. { id: box }, a plate dropped where none fits. */
  function worldSigns(heads, projects, pv, taken) {
    var boxes = {}, list = [];
    heads.forEach(function (h) {
      var b = plateBox(h.r, h.w, h.wt, h.h, pv, taken.concat(list), projects.filter(function (o) { return o.id !== h.id; }).map(function (o) { return o.r; }));
      if (b) { list.push(b); boxes[h.id] = b; }
    });
    return boxes;
  }
  /* The plot node the cursor is inside at the Plan and Bead depths (the grid is a plot or one of its beds), or null. */
  function inPlot(t, path) { var g = gridOf(t, path); return g.kind === "plot" ? g : g.kind === "bed" && g.parent ? t.nodes[g.parent] : null; }
  /* The plot whose arms the page draws at every level of detail: the plot the cursor is inside when its plan bears arms the farm
     named, else null, so a short map that frames the plan at Project art still shows them (gd-rA31ZIfHdv). */
  function gatePlot(t, path) {
    var p = path && path.length ? inPlot(t, path) : null, pl = p && p.ref && p.ref.plan;
    return pl && pl.heraldry && pl.heraldry_name && p.rect ? p : null;
  }
  /* statusOf(n) -> {status, icons}: the status line of §6.4 for the title bar (garden.js syncBar, gd-zDdiA3HBh5), whole: its
     parts with the need words first; on a harvest cell its lead, with its badges ({k, w} in priority order, harvest spec 5) as
     icons, else null. */
  function statusOf(n) {
    var hv = !!(n && n.icons && n.icons.length), parts = ((n ? hv ? n.lead : n.status : "") || "").split(" · ").filter(Boolean);
    var need = function (p) { return /\bneeds? you\b/.test(p); };
    return { status: line(parts.filter(need).concat(parts.filter(function (p) { return !need(p); }))), icons: hv ? n.icons : null };
  }
  function glyphOf(n) {
    if (n.kind === "bed") return String(n.ordinal);
    if (n.kind === "crate") return "+" + n.n;
    return n.needs ? "!" + n.needs : "";
  }
  /* glyphW(g): a glyph's painted width in screen px, its plate round the 3x5 font at 2x (garden.js drawGlyphs). fitGlyph(g, left,
     right) -> {g, x}: the glyph to paint for a cell whose screen edges are left and right, and its x, kept inside the cell so a
     narrow cell's mark never runs into its east neighbour's or past the farm's edge (gd-G2rS3GKYve): the whole glyph 2 px in
     where it fits, else its first character alone (the "!" or the "+"; the count stays in the hot link and the tip), else that
     character flush with the east edge. */
  function glyphW(g) { return g.length * 8 + 4; }
  function fitGlyph(g, left, right) {
    if (left + 2 + glyphW(g) > right) g = g.charAt(0);
    return { g: g, x: Math.min(left + 2, right - glyphW(g)) };
  }
  function pipsOf(n) { return n.kind === "bed" && n.total ? Math.floor(4 * n.done / n.total) : undefined; }
  /* signs(tree, path, vp, z) -> [{id, tier, text?, glyph?, pips?, anchor, slot?, box?}] (§6). A function of the cursor path and
     the viewport only: z is the zoom, or the frame() result to place against (its window); either way the camera centre is the
     framing's, never a camera mid-ease. Tiers: the cursor's Thing has none (its words are the title bar's, gd-zDdiA3HBh5); its
     children a short label where they are at least 56 px wide and the label clears earlier ones in reading order, else their
     glyph; the rest of the current grid glyph only. A child label stays inside the cursor's Thing, clear of its ring: one at its
     foot or east edge is moved up or left into it, and one wider or taller than the Thing is a glyph (gd-dS4KEs0ohA). anchor is
     the Thing's rect in world px; box is the label's in screen px at the framing (the page rides it on the world transform). */
  function signs(t, path, vp, z) {
    var n = path && path.length ? t.nodes[last(path)] : null;
    if (!n) return [];
    vp = vp || { w: 1280, h: 800 };
    var fr = z && typeof z === "object" ? z : frame(t, path, vp), cam = { cx: fr.cx, cy: fr.cy, z: typeof z === "number" ? z : fr.z };
    var view = insets(vp), g = gridOf(t, path), out = [], taken = [];
    var labels = 0, nr = n.rect ? screenRect(n.rect, cam, vp) : null, inner = nr && { x: nr.x + 2, y: nr.y + 2, w: nr.w - 4, h: nr.h - 4 };
    if (hasKids(n)) n.kids.forEach(function (id) {
      var k = t.nodes[id], kr = screenRect(k.rect || n.rect, cam, vp), s = { id: id, tier: "glyph", glyph: glyphOf(k), anchor: W.tiles(k.rect || n.rect) }, p = pipsOf(k);
      if (p !== undefined) s.pips = p;
      if (!TEXTLESS[k.kind] && kr.w >= LBL.childPx && labels < LBL.childCap) {
        var text = shortName(k.title, LBL.child), b = { x: kr.x + 2, y: kr.y + 2, w: text.length * LBL.ch + 2 * LBL.padX, h: LBL.line + 2 * LBL.padY };
        if (inner) { b.x = Math.max(inner.x, Math.min(b.x, inner.x + inner.w - b.w)); b.y = Math.max(inner.y, Math.min(b.y, inner.y + inner.h - b.h)); }
        if (within(b, view) && (!inner || within(b, inner)) && !taken.some(function (o) { return overlap(o, b); })) { s.tier = "child"; s.text = text; s.slot = "inside"; s.box = b; taken.push(b); labels++; }
      }
      out.push(s);
    });
    g.kids.forEach(function (id) {
      if (id === n.id) return;
      var k = t.nodes[id], s = { id: id, tier: "glyph", glyph: glyphOf(k), anchor: W.tiles(k.rect || g.rect) }, p = pipsOf(k);
      if (p !== undefined) s.pips = p;
      out.push(s);
    });
    return out;
  }

  return {
    LATTICE_W: LATTICE_W, ROW: ROW, PAD: PAD, RING: RING, CELL_MIN: CELL_MIN, LBL: LBL, DIRS: DIRS,
    tree: tree, regraft: regraft, pathFor: pathFor, atFor: atFor, move: move, jump: jump, descend: descend, ascend: ascend, follow: follow, frame: frame, signs: signs, stateWord: stateWord,
    glyphW: glyphW, fitGlyph: fitGlyph, blockCell: blockCell, shortName: shortName, statusOf: statusOf, gridOf: gridOf, usedBounds: usedBounds, plateBox: plateBox, worldSigns: worldSigns, gatePlot: gatePlot,
    insets: insets, screenRect: screenRect, overlap: overlap, within: within
  };
})();
if (typeof module !== "undefined") module.exports = CURSOR;
