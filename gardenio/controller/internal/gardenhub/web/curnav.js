/* CURNAV: the page's cursor, the state and glue between the pure CURSOR world (web/cursor.js) and garden.js
   (gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md sections 3.2, 3.5, 3.6 and 4.3; cited `§n`).
   It holds the path with its per-grid memory and the last framing window, says which key does what, picks the opening cursor
   (a ?at=, else the saved path in localStorage garden.cursor.v1, else depth 0 on the first project), lists the current grid's
   cells for #hots with their row and column words, builds the outline's rows from the same tree, turns the page's model
   into each biome's home zones. No DOM, so the Go tests run it under node. */
var CURNAV = (function () {
  "use strict";
  var C = typeof CURSOR !== "undefined" ? CURSOR : require("./cursor.js");
  var W = typeof WORLD !== "undefined" ? WORLD : require("./world.js");
  var A = typeof ART !== "undefined" ? ART : require("./art.js");
  var E = typeof ESCKIND !== "undefined" ? ESCKIND : require("./esckind.js");
  var T = typeof TITLES !== "undefined" ? TITLES : null;   // titles.js is required lazily by bar(): loading it listens on document
  var STORE = "garden.cursor.v1", MEMORY_MAX = 200;
  var ARROWS = { ArrowLeft: "left", ArrowRight: "right", ArrowUp: "up", ArrowDown: "down" };
  /* what a grid's children are called, for the grid's name and the descend announcement (§3.6) */
  var KIDS = { world: ["place", "places"], home: ["place", "places"], zone: ["item", "items"], biome: ["plan", "plans"], band: ["bead", "beads"], pen: ["wish", "wishes"], plot: ["phase", "phases"], bed: ["step", "steps"] };

  function last(a) { return a[a.length - 1]; }
  function plural(n, w) { return n + " " + (n === 1 ? w[0] : w[1]); }
  /* An accessible name read as sentences (gd-jhE8MpXvJm). sentences(s, more): the arms', weeds' and edges' sentences (each
     " Sentence.", or "") follow a full stop, so a bed's "0/1 steps" or a step's "3h ago" ends one before them. sayAt(s, at): a
     cell's place (nav.where's ", row r of R...") after a name that ends a sentence is a sentence of its own, " Row r of R.", not ".,". */
  function sentences(s, more) { return more && !/[.!?]$/.test(s) ? s + "." + more : s + more; }
  function sayAt(s, at) { return /[.!?]$/.test(s) ? s + at.replace(/^, r/, " R") + "." : s + at; }

  /* action(e) -> {act, dir?, step?} or null: the cursor keys of §3.2. A plain arrow moves, Shift+arrow is the sibling jump;
     Enter and + descend (= too, the unshifted + key); Escape and - ascend; Space opens the Thing; 0 is depth 0, Home a home cell
     (in the Escalations, their first cell first) and End the Escalations' last cell,
     n and N the next and previous Thing that needs you; / the jump palette; w watches the gardener at the cursor in the Glass
     and t opens its transcript (BIRDS.wire's watch, bird-captions spec 10). */
  function action(e) {
    var k = e.key, d = ARROWS[k];
    if (d) return { act: e.shiftKey ? "jump" : "move", dir: d };
    if (k === "Enter" || k === "+" || k === "=") return { act: "in" };
    if (k === "Escape" || k === "-") return { act: "out" };
    if (k === " " || k === "Spacebar") return { act: "open" };
    if (k === "0") return { act: "top" };
    if (k === "Home") return { act: "home", edge: true };
    if (k === "End") return { act: "end" };
    if (k === "n" || k === "N") return { act: "need", step: k === "N" ? -1 : 1 };
    if (k === "/") return { act: "find" };
    if ((k === "w" || k === "W") && !e.shiftKey) return { act: "watch" };
    if ((k === "t" || k === "T") && !e.shiftKey) return { act: "transcript" };
    return null;
  }

  /* The occupied coordinate range of grid g: [c0, c1) x [r0, r1); a spanning cell (a band, the plans crate) spans every column. */
  function extent(t, g) {
    var b = { c0: Infinity, c1: -Infinity, r0: Infinity, r1: -Infinity };
    g.kids.forEach(function (id) {
      var n = t.nodes[id];
      b.c0 = Math.min(b.c0, n.span ? 0 : n.c); b.c1 = Math.max(b.c1, n.span ? g.cols : n.c + 1);
      b.r0 = Math.min(b.r0, n.r); b.r1 = Math.max(b.r1, n.r + 1);
    });
    if (!g.kids.length) b = { c0: 0, c1: 0, r0: 0, r1: 0 };
    return b;
  }
  /* The Escalations zone's grid name (gd-AmIpCiVTvY): "Escalations, 3 waiting on you, 2 running", its cells counted by what
     their state words say (ESCKIND.word), and the crate's rest as "7 more", so the name sums the grid it names. */
  function escLabel(g, cells) {
    var n = [[0, "waiting on you"], [0, "running"], [0, "waiting on others"], [0, "rejected or failed"], [0, "more"]];
    cells.forEach(function (c) {
      var k = c.node, w = k.ref && k.ref.wait;
      if (k.kind === "crate") n[4][0] += k.n | 0;
      else if (w) n[E.group(w) === 2 ? 3 : w.human ? 0 : w.state === "approved" ? 1 : 2][0]++;
    });
    return [g.title].concat(n.filter(function (p) { return p[0]; }).map(function (p) { return p[0] + " " + p[1]; })).join(", ");
  }
  function isEsc(g) { return !!g && g.kind === "zone" && g.zone === "escalation"; }
  /* Depth 0 on the first project after the Commons (§3.5, 28.5), else the Commons, else nothing (an empty farm). */
  function first(t) {
    var any = null;
    for (var i = 0; i < t.root.kids.length; i++) {
      var n = t.nodes[t.root.kids[i]];
      if (n.kind !== "biome") continue;
      if (n.k >= 1) return [n.id];
      any = any || n.id;
    }
    return any ? [any] : [];
  }

  /* nav() -> the page's cursor. tree is the current CURSOR.tree, path the cursor's path, memory a container id -> the child the
     cursor last left in it (N-D4), col the column a spanning band keeps for the next move down, win the last framing window. */
  function nav() {
    var s = { tree: null, path: [], memory: {}, col: undefined, win: null };
    function remember(p) {
      for (var i = 1; i < p.length; i++) { delete s.memory[p[i - 1]]; s.memory[p[i - 1]] = p[i]; }   // re-inserted, so the oldest go first
      var keys = Object.keys(s.memory);
      for (var j = 0; j < keys.length - MEMORY_MAX; j++) delete s.memory[keys[j]];
    }
    function moved(r, keepCol) {
      if (!keepCol) s.col = undefined;
      if (!r.bumped && !r.open && r.drawer === undefined) { s.path = r.path; remember(r.path); }
      return r;
    }
    /* build(world, home, opts): a new tree for a new layout; the cursor follows its Thing (§3.4). home is homeSpec's. */
    s.build = function (world, home, opts) {
      var prev = s.tree;
      s.tree = C.tree(world, home, opts); s.tree.sigs = {};
      (world ? world.biomes : []).forEach(function (b) { s.tree.sigs[b.id] = homeSig((home || {})[b.id], opts, b.key); });
      if (s.path.length) { s.path = C.follow(s.tree, prev, s.path); if (!s.path.length) s.path = first(s.tree); }
      return s.tree;
    };
    /* patch(home, opts) -> whether anything changed: a new home on the same layout (a ledger patch, a gardener home, a truck
       loading) rebuilds only the home cells of the biomes it touched (28.6), and the cursor follows (§3.4). */
    s.patch = function (home, opts) {
      var t = s.tree, prev = { nodes: {} }, hit = false;
      if (!t || !t.sigs) return false;
      Object.keys(t.sigs).forEach(function (bid) {
        if (!t.nodes[bid]) return;
        var sg = homeSig((home || {})[bid], opts, t.nodes[bid].key), old;
        if (sg === t.sigs[bid]) return;
        t.sigs[bid] = sg; hit = true; old = C.regraft(t, bid, (home || {})[bid], opts);
        for (var id in old) prev.nodes[id] = old[id];
      });
      if (s.path.some(function (id) { return prev.nodes[id]; })) s.path = C.follow(t, prev, s.path);
      return hit;
    };
    /* open(at, saved): the opening cursor (§3.5). at is a ?at= value; saved the stored JSON. True when at was found. */
    s.open = function (at, saved) {
      var p = at ? C.pathFor(s.tree, at) : null, st = null;
      try { st = saved ? JSON.parse(saved) : null; } catch (e) { st = null; }
      if (st && st.memory && typeof st.memory === "object") for (var k in st.memory) if (typeof st.memory[k] === "string") s.memory[k] = st.memory[k];
      if (!p && st && Array.isArray(st.path) && st.path.length) p = C.pathFor(s.tree, last(st.path));
      s.path = p || first(s.tree); s.col = undefined; s.win = null; remember(s.path);
      return !!(at && p);
    };
    s.save = function () { return JSON.stringify({ path: s.path, memory: s.memory }); };
    s.node = function () { return s.tree && s.path.length ? s.tree.nodes[last(s.path)] : null; };
    s.grid = function () { return C.gridOf(s.tree, s.path); };
    s.at = function () { return C.atFor(s.tree, s.path); };
    s.move = function (dir) { var r = C.move(s.tree, s.path, dir, s.col); moved(r, true); s.col = r.col; return r; };
    s.jump = function (dir) { return moved(C.jump(s.tree, s.path, dir)); };
    s.descend = function (needs) { return moved(C.descend(s.tree, s.path, s.memory, needs)); };
    s.ascend = function () { return moved(C.ascend(s.tree, s.path)); };
    s.top = function () { return moved({ path: s.path.slice(0, 1), bumped: s.path.length < 2 }); };
    /* home(edge): the Home key (28.6, H-D11): to the home cell of the project the cursor is in or on, the Commons's with none or on
       the Commons; from a project's home cell, to the Commons's, so two presses reach the farm-wide strip. edge (the key, not the
       Menu's Home): in the Escalations, their first cell first (gd-AmIpCiVTvY). */
    s.home = function (edge) {
      var e = edge && escEdge(false);
      if (e) return moved({ path: e, bumped: false });
      var t = s.tree, b = t && s.path.length ? t.nodes[s.path[0]] : null, own = b && b.kind === "biome" && b.key ? "home:" + b.key : "";
      var p = C.pathFor(t, own && t.nodes[own] && last(s.path) !== own ? own : "home:");
      return moved({ path: p || s.path.slice(), bumped: !p || p.join("\n") === s.path.join("\n") });
    };
    /* end(): the End key: the Escalations' last cell (gd-AmIpCiVTvY); anywhere else, or already there, a bump. */
    s.end = function () { var e = escEdge(true); return moved({ path: e || s.path.slice(), bumped: !e }); };
    /* escEdge(end) -> the path to the cursor's Escalations zone's first (last) cell in reading order, or null when its grid is
       not that zone or the cursor is already there. */
    function escEdge(end) {
      var g = s.tree && s.path.length > 1 ? s.grid() : null;
      if (!isEsc(g) || !g.kids.length) return null;
      var ks = g.kids.map(function (id) { return s.tree.nodes[id]; }).sort(function (a, b) { return a.r - b.r || a.c - b.c; }), to = (end ? last(ks) : ks[0]).id;
      return to === last(s.path) ? null : s.path.slice(0, -1).concat(to);
    }
    /* to(id): a cursor jump to any Thing id, bead id or at= value (the jump palette, beacons, the outline, the bead view);
       found says whether it is in the tree (bumped is also set when the cursor is already there). */
    s.to = function (id) {
      var p = s.tree ? C.pathFor(s.tree, id) : null, r = moved({ path: p || s.path.slice(), bumped: !p || p.join("\n") === s.path.join("\n") });
      r.found = !!p;
      return r;
    };
    /* need(ids, step): to the next (step 1) or previous (-1) Thing in `ids` (beacon order) after the cursor's (section 8). */
    s.need = function (ids, step) {
      var seen = {}, list = [];
      (ids || []).forEach(function (id) { var p = C.pathFor(s.tree, id); if (p && !seen[last(p)]) { seen[last(p)] = 1; list.push(p); } });
      if (!list.length) return { path: s.path.slice(), bumped: true };
      var cur = last(s.path), i = -1;
      list.forEach(function (p, j) { if (last(p) === cur) i = j; });
      var at = i < 0 ? (step > 0 ? 0 : list.length - 1) : (i + step + list.length) % list.length;
      return moved({ path: list[at], bumped: i === at });
    };
    /* frame(vp, label): the camera for the cursor (§4.1); the window it frames is kept so the next one scrolls only at its edge. */
    s.frame = function (vp, label) {
      var f = C.frame(s.tree, s.path, vp, { prev: s.win, label: label });
      s.win = f.window;
      return f;
    };
    /* pick(x, y) -> the id of the Thing at world px (x, y), no deeper than the cursor's depth (a tap jumps the cursor to it at
       the depth it is on, §7), or null on bare ground; inGrid(id) says whether a Thing is a cell of the current grid. */
    s.pick = function (x, y) {
      var tx = x / W.TILE, ty = y / W.TILE, g = s.tree ? s.tree.root : null, out = null;
      for (var d = 0; g && d < s.path.length; d++) {
        var hit = null;
        for (var i = 0; i < g.kids.length && !hit; i++) {
          var r = s.tree.nodes[g.kids[i]].rect;
          if (r && tx >= r.x && tx < r.x + r.w && ty >= r.y && ty < r.y + r.h) hit = s.tree.nodes[g.kids[i]];
        }
        if (!hit) break;
        out = hit.id; g = hit;
      }
      return out;
    };
    /* bar(index, lin) -> the title bar's words (gd-yozHl8UqIs, gd-zDdiA3HBh5): the path's Things past the project, each {id, at,
       name, title} (name the farm document's short title, TITLES.name, else the Thing's title; title the whole one), the cursor
       Thing's state word or "", and its status line of §6.4 (CURSOR.statusOf: the need words first; icons, a harvest cell's
       badges, else null), which the bar shows in full where the full label once did. An escalation's status is its sign tip's
       line (ESCKIND.tip with lin, the plans' lineage), so a phone, whose taps move no focus to raise the tip, reads it in the bar
       on the tab bar (gd-AmIpCiVTvY); tip: true then, as it is a sentence, not a list of words. */
    s.bar = function (index, lin) {
      var p = s.path.length > 1 ? s.path.slice(1) : s.path, parts = [], st = "", tip = "", n = null;
      p.forEach(function (id, i) {
        var it = index && index[id], rec = it && it.kind !== "biome" && (it.task || it.seed || it.plan), plot;   // a biome's seed is its palette's
        n = s.tree.nodes[id];
        if (n.kind === "bed" && !rec) { plot = index && index[n.parent]; rec = plot && plot.plan ? (plot.plan.phases || []).filter(function (x) { return x.id === id; })[0] : null; }
        parts.push({ id: id, at: C.atFor(s.tree, s.path.slice(0, s.path.length - p.length + i + 1)), name: rec ? (T || (T = require("./titles.js"))).name(rec) : n.title, title: n.title });
        if (i < p.length - 1) return;
        if (n.kind === "escalation" && n.ref && n.ref.wait) tip = E.tip(n.ref.wait, s.tree.now || Date.now(), lin)[1].replace(/\.$/, "");
        st = tip || (it && it.staked && it.plan ? "staked" : C.stateWord((rec || n.ref || {}).state || ""));
      });
      var sl = tip ? { status: tip, icons: null } : C.statusOf(n), out = { parts: parts, state: st, status: sl.status, icons: sl.icons };
      if (tip) out.tip = true;
      return out;
    };
    s.inGrid = function (id) { return !!(s.tree && s.path.length && s.grid().kids.indexOf(id) >= 0); };
    /* cells() -> the current grid's occupied cells in reading order, each {node, row, col} with 1-based indices from the grid's
       used bounds (ARIA allows the gaps an empty cell leaves, §3.6), plus the grid, its rows and cols and its name. */
    s.cells = function () {
      var g = s.grid(), e = extent(s.tree, g), out = [];
      g.kids.forEach(function (id) { var n = s.tree.nodes[id]; out.push({ node: n, row: n.r - e.r0 + 1, col: n.span ? 0 : n.c - e.c0 + 1 }); });
      return { grid: g, cells: out, rows: e.r1 - e.r0, cols: e.c1 - e.c0,
        label: isEsc(g) ? escLabel(g, out) : g.title + ": " + plural(out.length, KIDS[g.zone === "pen" ? "pen" : g.kind] || KIDS.zone) + ", " + (e.r1 - e.r0) + " by " + (e.c1 - e.c0) };
    };
    /* where(cell, cells) -> ", row r of R, column c of C" for a cell's accessible name (a spanning cell has no column). */
    s.where = function (cell, all) { return ", row " + cell.row + " of " + all.rows + (cell.col ? ", column " + cell.col + " of " + all.cols : ""); };
    /* say(how) -> what the polite live region announces after a descend or an ascend (§3.6), or "" for a plain move. */
    s.say = function (how) {
      if (how !== "in" && how !== "out" && how !== "top" && how !== "to" && how !== "home") return "";
      var n = s.node(), all = s.cells(), g = all.grid;
      if (!n) return "";
      var on = n.kind === "bed" ? "phase " + n.ordinal + ", " + n.title : n.title;
      return (g === s.tree.root ? "The farm" : "Inside " + g.title) + ": " + all.rows + " by " + all.cols + " " + (KIDS[g.zone === "pen" ? "pen" : g.kind] || KIDS.zone)[1] + ", on " + on + ".";
    };
    return s;
  }

  /* homeSpec(world, model, slots, counts) -> each biome's home zones for CURSOR.tree, by biome id: WORLD.homeFill of the
     page's model (its decision, growing and agent lists, each [{item}] or items; a figure carries its gardener g) into each
     project's strip, slots[project][zone] keeping each item's slot (28.2). counts (optional), a barn's account by biome id
     (PORCH.groups' {n, home, land}), makes its Barn's words the account of everyone (5a). */
  function homeSpec(world, model, slots, counts) {
    if (!world) return {};
    var rows = {}, items = function (z) { return ((model && model[z]) || []).map(function (e) { return e.item || e; }); };
    rows.decision = items("decision"); rows.work = items("growing"); rows.agent = items("agent");
    var fill = W.homeFill(world, rows, slots || {});
    Object.keys(counts || {}).forEach(function (id) { if (fill[id] && counts[id]) fill[id].barn.count = W.barnCount(counts[id]); });
    return fill;
  }
  /* What a biome's home cell shows, as one string: patch() rebuilds the cells whose string changed. */
  function homeSig(f, opts, key) {
    var tr = null, its = function (z) { var p = f && f[z] || { items: [] }; return [p.more | 0, p.count || 0, p.stands | 0].concat(p.items.map(function (h) { var r = h.item || {}; return [h.id, h.slot, h.x, h.project, r.title, r.line, r.state, r.type, r.updated || r.u, r.on, r.where]; })); };
    (opts && opts.trucks || []).forEach(function (x) { if (x && x.key === key) tr = x; });
    return JSON.stringify([its("mailbox"), its("growing"), its("barn"), tr ? [tr.id, tr.rel.status, tr.rel.targets] : 0]);
  }

  /* outline(tree, extra) -> the outline's rows (design section 8, built from the tree so its order is the grids' reading order, §3.6):
     {kind, id, hot, title, sub, children}; hot is the tree id the page jumps the cursor to. children is built when first read
     (the page renders a branch when it opens), so a 10x farm costs its root rows per layout, not its ~15k nodes. extra: rows by
     node id after its children (28.7), else at the root. */
  function outline(t, extra) {
    extra = extra || {};
    function row(id) {
      var n = t.nodes[id], r = { kind: n.kind, id: n.id, hot: n.id, title: n.title, sub: n.status || "" }, kids = null;
      Object.defineProperty(r, "children", { enumerable: true, get: function () { return kids || (kids = n.kids.map(row).concat(extra[id] || [])); } });
      return r;
    }
    return t ? t.root.kids.map(row).concat(Object.keys(extra).filter(function (id) { return !t.nodes[id]; }).map(function (id) { return extra[id]; })) : [];
  }

  return { STORE: STORE, MEMORY_MAX: MEMORY_MAX, action: action, nav: nav, first: first, homeSpec: homeSpec, outline: outline, sentences: sentences, sayAt: sayAt };
})();
if (typeof module !== "undefined") module.exports = CURNAV;
