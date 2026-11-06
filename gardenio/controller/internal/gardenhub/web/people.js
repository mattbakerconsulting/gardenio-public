/* PEOPLE: the gardeners on the world (gardenio/docs/homestead-agents-as-people.md sections 4 and 5): who stands where, their
   looks, their words and cards, and the stewards at their lands' homesteads. Pure like web/world.js, which builds it once from
   its grid and words, PEOPLE(W), and hands out what it returns as its own names (WORLD.gardenerSpots and the rest). */
var PEOPLE = function (W) {
  "use strict";
  var R = typeof REASON !== "undefined" ? REASON : require("./reason.js");   // a gardener's reason, drawn (web/reason.js)
  var ACT = typeof ACTIVITY !== "undefined" ? ACTIVITY : require("./activity.js");   // what a gardener is doing (web/activity.js)
  var ESC = typeof ESCKIND !== "undefined" ? ESCKIND : require("./esckind.js");   // an escalation's order and words (web/esckind.js)
  var TILE = W.TILE, HOME = W.HOME, SLOT = W.SLOT, h32 = W.h32, hit = W.hit, cellAt = W.cellAt, tile = W.tile, atFor = W.atFor,
    projectKey = W.projectKey, projectOf = W.projectOf, byId = W.byId, plural = W.plural;
  /* ---------- gardeners on their plots (gardenio/docs/homestead-agents-as-people.md sections 4 and 5) ----------
     The farm document's gardeners[] placed on the world: who stands on which plot, in which column, and who is at home in the
     barn. Pure, so the Go tests run it under node; the page runs it when the data changes, never per frame. */
  var GARDENER_CAP = [0, 0, 3, 6];   // gardeners not on a task cell drawn per plot at World, Project, Plan, Bead; the rest are a "+N" badge (section 4.3). A gardener tending its task's cell is never capped (4.2)
  /* Need first: down, then asking (the pose, so a reason counts), then at work, then the rest; ties go to the earliest since, then the id. */
  function needRank(g) { var p = R.pose(g); return p === "down" ? 0 : p === "ask" ? 1 : g.state === "working" ? 2 : 3; }
  function byNeed(a, b) { return needRank(a) - needRank(b) || (a.since || 0) - (b.since || 0) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); }
  /* homeFor(g) -> the biome whose strip is a gardener's home (28.4, H-D9, H-D10): "project:<key>" for one with a project, the
     Commons ("project:") for one without and for every thread agent. A steward (a role token "steward", Lands 9.8; section 9.2)
     lives instead at its land's homestead, the name sign of that biome: never on a plot or at a barn; stewardState draws it. */
  function isSteward(g) { return String(g && g.role || "").split(",").some(function (r) { return r.trim().toLowerCase() === "steward"; }); }
  function homeFor(g) { return "project:" + (isThread(g) ? "" : projectKey(g && g.project)); }
  /* fieldHome(g, has) -> the biome id whose Growing field gardener g stands in (28.12), has(id) saying which biomes are laid out:
     its home (homeFor), else the Commons's; null for a steward whose land is laid out, which stands at its sign instead. One rule
     for gardenerSpots' stands and the field's size (WORLD.homeCounts, 29.10), so the two never drift. */
  function fieldHome(g, has) { var land = homeFor(g); if (!has(land)) return "project:"; return isSteward(g) && land !== "project:" ? null : land; }
  /* barnDoor(world, biomeId) -> {x, y, biome}: a 16 x 24 figure's top left standing at the door of that biome's barn, else the
     Commons's (28.3), world px; null when neither strip is laid out. The ride truck leaves from it and comes back to it (28.10). */
  function barnDoor(world, biomeId) {
    var ix = (world && world.index) || {}, b = ix[biomeId] && ix[biomeId].home ? ix[biomeId] : ix["project:"], k = b && b.home && b.home.keep.barn;
    return k ? { x: k.x * TILE + HOME.door[0] - 8, y: k.y * TILE + HOME.door[1] - 24, biome: b.id } : null;
  }
  /* homeTile(world, g) -> {x, y, biome, drawn} | null: gardener g's place at home, the one origin of its walks home, its fly-back
     and its birds (28.4): its field stand's figure tile when homeFill placed one (drawn; the east tile of its pair slot, 28.12),
     else its barn's door (barnDoor). */
  function homeTile(world, g) {
    var f = g && world && world.index && world.index[FIELD + g.id];
    if (f && f.kind === "stand") return { x: (f.x + f.w - 1) * TILE, y: f.y * TILE, biome: "project:" + f.project, drawn: true };
    var d = barnDoor(world, homeFor(g));
    if (d) d.drawn = false;
    return d;
  }
  /* A thread agent (a role token "thread", or "dm"; 5b, gd-1kFVOmUjiK) is sent by the bridge to answer a thread in the Commons and
     stops after each reply. With no plot it is at the barn all the same, but its words say what it serves: "In the Commons: <the
     thread head's title>" (the farm's thread_title) on the Porch, in the outline, its card, its tip and its barn link. */
  function isThread(g) { return String(g && g.role || "").split(",").some(function (r) { r = r.trim().toLowerCase(); return r === "thread" || r === "dm"; }); }
  function homeWord(g) { return isThread(g) && g.chat ? "In the Commons: " + (g.thread_title || "a thread") : "At the barn"; }
  /* The sprite a gardener wears (section 5.2): the pose from its reason else its state (REASON.pose); the group from the first role
     token that names one (a steward's own sprite, a thread agent's pink shirt; the rest dress alike); the shirt its own, h32(id, 3)
     into ART.GD_WEAR, so one gardener tells from the next; the hat its plan's, h32(plan, 2) into ART.GD_HATS, so a plot's gardeners
     wear one hat (the straw hat, 8, with no plot). A steward minds its land, so its hat is its land's biome (World design 3.3). */
  var ROLE_GROUP = { crew: "crew", "plan-gardener": "gardener", "plan-captain": "gardener", "release-captain": "gardener", gardener: "gardener", steward: "steward", infra: "infra", cicd: "infra", thread: "thread", dm: "thread" };
  function gardenerLook(g) {
    var group = "other", roles = String(g.role || "").split(",");
    for (var i = 0; i < roles.length; i++) { var r = ROLE_GROUP[roles[i].trim().toLowerCase()]; if (r) { group = r; break; } }
    var key = group === "steward" ? projectKey(g.project) : g.plot && g.plot.plan ? String(g.plot.plan) : "", pose = R.pose(g);
    return { pose: pose, group: group, shirt: h32(String(g.id || ""), 3) % 8, hat: key ? h32(key, 2) % 8 : 8 };
  }
  /* Where a gardener can stand besides a plan's plot: a band item or a goat (the owner, gd-6FCoWelnx2: "workers in the orchard
     and other areas should show up there"). STAND_PLACE names each one's place in words; standItem(world, g) is the item the
     gardener's task (else the bead it tends; a wish by its goat or headstone id) is drawn as, or null; standTitle(it) the title
     of what is stood at (a plot's plan, a tree's plan, a flower's bead, a pot's seed, a stone's or goat's wish); standWhere(it)
     the words an outline row or a field stand says: the plan's title on a plot, "In the Meadow: <title>" elsewhere. */
  var STAND_PLACE = { flower: "the Meadow", seed: "the Nursery", tree: "the Orchard", stone: "the Graveyard", goat: "the pen" };
  function standItem(world, g) {
    var ix = world && world.index; if (!ix || !g) return null;
    var ids = [g.task, g.tends], i, it;
    for (i = 0; i < ids.length; i++) { if (!ids[i]) continue; it = ix[ids[i]] || ix["wish:" + ids[i]]; if (it && STAND_PLACE[it.kind]) return it; }
    return null;
  }
  function standTitle(it) {
    if (!it) return "";
    var b = it.kind === "plot" || it.kind === "tree" ? it.plan : it.kind === "flower" ? it.task : it.kind === "seed" ? it.seed : it.kind === "stone" || it.kind === "goat" ? it.wish : null;
    return (b && b.title) || it.id;
  }
  function standWhere(it) { return !it ? "" : it.kind === "plot" ? standTitle(it) : "In " + STAND_PLACE[it.kind] + ": " + standTitle(it); }
  /* fieldWhere(st, g) -> where gardener g stands in words (28.12): its field, and the plot or band item it is out at too: "In the
     field of gardenio and on <plan>", "In the field of gardenio and in the Meadow: <bead>", at home "In the field of gardenio";
     "" with no field stand (a steward at its homestead). st is gardenerState's. */
  function fieldWhere(st, g) {
    var sp = st && g && st[3].spots[g.id], ix = (st && st.world && st.world.index) || {}, b = sp && sp.field ? ix[sp.field.home] : null, p = sp && sp.plot ? ix[sp.plot] : null;
    if (!b) return "";
    var w = p ? standWhere(p) : "";
    return "In the field of " + (b.key ? b.name : "the Commons") + (!p ? "" : p.kind === "plot" ? " and on " + w : " and i" + w.slice(1));
  }
  /* bandOf(world, it) -> {rect, items}: the band an item stands in (the Growing field for a flower, the pen's inside for a goat,
     else its biome's band), bounding the columns beside it, and the band's other items, which nobody stands on; null for a plot
     or an item whose biome is gone. */
  function bandOf(world, it) {
    var b = it && it.kind !== "plot" ? world.index["project:" + it.project] : null;
    if (!b || b.kind !== "biome") return null;
    if (it.kind === "goat") return b.pen ? { rect: { x: b.pen.x + 1, y: b.pen.y + 1, w: b.pen.w - 2, h: b.pen.h - 2 }, items: b.pen.items } : null;
    if (it.kind === "flower") { var z = b.home && b.home.zones.growing; return z ? { rect: { x: z.x + 1, y: z.y + 1, w: z.w - 2, h: z.h - 2 }, items: b.meadow } : null; }
    var r = it.kind === "seed" ? b.bands.nursery : it.kind === "tree" ? b.bands.orchard : b.bands.graveyard;
    return r ? { rect: r, items: it.kind === "seed" ? b.nursery : it.kind === "tree" ? b.orchard : b.graveyard } : null;
  }
  /* The gardeners beside a band item or a goat (gd-6FCoWelnx2): feet on its bottom row, from the column right of it, the
     nearest free column inside its band (bandOf; the item's own row otherwise), never on the band's items, capped as a plot's
     fence (GARDENER_CAP), past the cap the "+N" badge in the next free column; plots[<item id>] holds them with no lane, so a
     walk to or from one goes straight (web/transitions.js). Each counts on its biome's sign as one on a plot does. taken
     ("col:row") is shared by every stand of the call, so those beside two neighbouring flowers never share a tile. */
  function standBeside(out, world, p, list, cap, count, taken) {
    var band = bandOf(world, p), r = band && band.rect, row = p.y + p.h - 1, lo = r ? r.x : p.x, hi = r ? r.x + r.w - 1 : p.x + p.w, right = p.x + p.w, n = 0, key = function (c) { return c + ":" + row; };
    var plot = out.plots[p.id] = { ids: list.map(function (g) { return g.id; }), shown: [], more: 0, moreX: 0, moreY: TILE * row, counts: { all: 0, work: 0, ask: 0, down: 0 }, lane: null };
    var b = out.biomes["project:" + p.project] = out.biomes["project:" + p.project] || { all: 0, work: 0, ask: 0, down: 0 };
    (band ? band.items : [p]).forEach(function (it) { if (it.y <= row && row < it.y + it.h) for (var c = it.x; c < it.x + it.w; c++) taken[key(c)] = 1; });
    var free = function (c) { for (var d = 0; d <= hi - lo; d++) { if (c + d >= lo && c + d <= hi && !taken[key(c + d)]) return c + d; if (d && c - d >= lo && c - d <= hi && !taken[key(c - d)]) return c - d; } return null; };
    list.forEach(function (g) {
      count(plot.counts, g); count(b, g);
      var col = n < cap ? free(right) : null;
      if (col === null) { out.spots[g.id] = { plot: p.id, shown: false }; return; }
      taken[key(col)] = 1; n++; plot.shown.push(g.id);
      out.spots[g.id] = { plot: p.id, col: col, row: row, x: col * TILE, y: TILE * row - 8, shown: true, cell: "", face: col < p.x ? "r" : "l", fence: true };
    });
    plot.more = list.length - plot.shown.length;
    if (plot.more && cap) { var mc = free(right); plot.moreX = (mc === null ? hi : mc) * TILE; }
  }
  /* gardenerSpots(world, gardeners, level) -> {spots, plots, home, fields, biomes}. spots[id] is {plot, col, row, x, y, shown,
     cell, face, fence, field} (world px, the 16 x 24 sprite's top left, feet on the row; face "l" or "r", the side it looks to) or
     {home: <biome id>, field}, at home (homeFor, else the Commons's, 28.3). field is {home: <biome id>, task}: every running
     gardener, on a plot or at home, also stands in that biome's Growing field (28.12), task the bead its plant is for ("" for
     none). One stands on a plot when its plot is in the world and it is
     alive or stuck, on an empty tile of the field beside what it tends, facing it: its task's cell, uncapped, else a bed
     (bedBox), capped at GARDENER_CAP[level]. The fence is the overflow, and past the cap the "+N" badge takes the next free
     fence column. plots[id] = {ids (need order), shown (need order), more, moreX, moreY, counts, lane}; home is the home
     ids in need order; fields[biome id] the ids of every gardener with a field stand there, in need order; biomes[id] = counts
     of the gardeners on its plots.
     counts: {all, work, ask, down}. lane is where a walk between plots runs (web/transitions.js), as the sprite's top left: y in the 2-tile path under the
     plot, l and r in the paths left and right of it, and its project, since only one project's paths line up. One alive or
     stuck with no plot in the world whose task (else tends) is drawn as a band item or a goat stands beside it instead
     (standItem, standBeside; gd-6FCoWelnx2): only an idle or dormant gardener, or one whose task is drawn nowhere, goes home. */
  function gardenerSpots(world, gardeners, level) {
    var out = { spots: {}, plots: {}, home: [], fields: {}, biomes: {} }, cap = GARDENER_CAP[level] || 0, on = {}, ix = world && world.index || {}, stood = {}, fld = {};
    var count = function (c, g) { var r = needRank(g); c.all++; if (r === 0) c.down++; else if (r === 1) c.ask++; else c.work++; };
    var field = function (g, at) { fld[g.id] = { home: at, task: fieldTask(g) }; (out.fields[at] = out.fields[at] || []).push(g.id); };
    (gardeners || []).slice().sort(byNeed).forEach(function (g) {
      var at = fieldHome(g, function (id) { return !!ix[id]; });
      if (at === null) { out.spots[g.id] = { land: homeFor(g) }; return; }   // at its homestead (stewardState)
      var p = g.plot ? ix[g.plot.plan] : null, up = g.bucket === "alive" || g.bucket === "stuck";
      if (!(p && p.kind === "plot") && up) p = standItem(world, g);   // its work drawn as a band item or a goat: it stands there (gd-6FCoWelnx2)
      field(g, at);
      if (!p || !up) { out.spots[g.id] = { home: at }; out.home.push(g.id); return; }
      (on[p.id] = on[p.id] || []).push(g);
    });
    Object.keys(on).forEach(function (pid) {
      if (world.index[pid].kind !== "plot") { standBeside(out, world, world.index[pid], on[pid], cap, count, stood); return; }
      var p = world.index[pid], list = on[pid], taken = plotMarks(p), fenceRow = p.y + SLOT.h - 1, fence = TILE * fenceRow - 8, inner = SLOT.w - 2, gate = 0;   // feet on the bottom fence; columns inside the fence
      var gx = (SLOT.dx - SLOT.w) / 2, gy = (SLOT.dy - SLOT.h) / 2, lane = { y: TILE * (p.y + SLOT.h + gy) - 24, l: TILE * (p.x - gx) - 8, r: TILE * (p.x + SLOT.w + gx) - 8, project: p.project };
      var plot = out.plots[pid] = { ids: list.map(function (g) { return g.id; }), shown: [], more: 0, moreX: 0, moreY: fence + 8, counts: { all: 0, work: 0, ask: 0, down: 0 }, lane: lane };
      var b = out.biomes["project:" + p.project] = out.biomes["project:" + p.project] || { all: 0, work: 0, ask: 0, down: 0 };
      var key = function (row, c) { return c + ":" + row; };
      /* the nearest free column to c in a row, right first then left, inside the fence; null when the row is full */
      var free = function (row, c) {
        for (var d = 0; d < inner; d++) { if (c + d <= p.x + inner && !taken[key(row, c + d)]) return c + d; if (d && c - d > p.x && !taken[key(row, c - d)]) return c - d; }
        return null;   // every column taken (tile columns are negative west of the origin, so no -1)
      };
      var stand = function (g, row, col, face, cell, f) { taken[key(row, col)] = 1; out.spots[g.id] = { plot: pid, col: col, row: row, x: col * TILE, y: TILE * row - 8, shown: true, cell: cell || "", face: face, fence: !!f }; };
      var hide = function (g) { out.spots[g.id] = { plot: pid, shown: false }; };
      var onFence = function (g, t, cell) { var col = free(fenceRow, p.x + 1); if (col === null) return false; stand(g, fenceRow, col, t && t.x1 < col ? "l" : "r", cell, true); return true; };   // from the gate
      var tend = function (g, t, cell) { var at = besideOf(p, t, taken); if (!at) return false; stand(g, at.row, at.col, at.face, cell); return true; };
      var rest = [];
      list.forEach(function (g) {
        count(plot.counts, g); count(b, g);
        var cell = world.index[g.task || g.escalation];   // its task, else the escalation it was spawned to run (farm escalation)
        if (cell && cell.kind === "task" && cell.plot === pid) {   // on its task: always drawn where people are drawn
          var t = { x0: cell.x, x1: cell.x, y0: cell.y, y1: cell.y };
          if (!cap || !(tend(g, t, cell.id) || onFence(g, t, cell.id))) hide(g);
          return;
        }
        rest.push(g);
      });
      rest.forEach(function (g) {
        if (gate >= cap) { hide(g); return; }
        var t = bedBox(p, g.task);
        if (tend(g, t) || onFence(g, t)) gate++; else hide(g);
      });
      plot.shown = plot.ids.filter(function (id) { return out.spots[id].shown; });
      plot.more = list.length - plot.shown.length;
      if (plot.more && cap) {
        var last = p.x + 1;
        plot.shown.forEach(function (id) { var s = out.spots[id]; if (s.fence) last = s.col; });
        var mc = free(fenceRow, last);
        plot.moreX = (mc === null ? p.x + inner : mc) * TILE;
      }
    });
    Object.keys(fld).forEach(function (id) { out.spots[id].field = fld[id]; });
    return out;
  }
  /* fieldTask(g) -> the bead g's field stand has its plant for: its task, else what it was sent to tend; "" for a thread agent,
     whose thread head is no plant (28.12). */
  function fieldTask(g) { return isThread(g) ? "" : g.task || g.tends || ""; }
  /* plotMarks(p) -> {"x:y": 1}, the tiles of plot p nobody stands on: task cells, crates, stubble and gone stubs, stakes, the "+N phases" bed. */
  function plotMarks(p) {
    var m = {}, put = function (x, y) { m[x + ":" + y] = 1; };
    (p.beds || []).forEach(function (bd) {
      bd.cells.forEach(function (c) { put(c.x, c.y); });
      if (bd.crate) put(bd.crate.x, bd.crate.y);
      for (var i = 0; i < bd.stubble; i++) { var a = cellAt(bd.stubbleAt + i); put(bd.x + a.c, bd.y + a.r); }
      (bd.gone || []).forEach(function (gi) { var a = cellAt(gi); put(bd.x + a.c, bd.y + a.r); });   // a step that left, drawn as a stub
      if (!bd.other) put(bd.x - 1, bd.y - 1);
    });
    if (p.crate) for (var y = 0; y < p.crate.h; y++) for (var x = 0; x < p.crate.w; x++) put(p.crate.x + x, p.crate.y + y);
    return m;
  }
  /* bedBox(p, task) -> {x0, x1, y0, y1} (tiles), the used blocks of the bed tended by one not on a task cell: its phase's, else
     the growing (a step in progress), else sown, else any bed nearest the gate; null without beds. */
  function bedBox(p, task) {
    var beds = p.beds || [], own = beds.filter(function (bd) { return bd.id === task; })[0], gx = p.x + 1, gy = p.y + SLOT.h - 1;
    var box = function (bd) { var u = bd.used, w = u.cols || bd.w, h = u.rows || bd.h; return { x0: bd.x, x1: bd.x + w - 1, y0: bd.y, y1: bd.y + h - 1 }; };
    var near = function (a, b) { return (Math.abs(a.x - gx) + Math.abs(a.y + a.h - 1 - gy)) - (Math.abs(b.x - gx) + Math.abs(b.y + b.h - 1 - gy)) || a.n - b.n; };
    if (own) return box(own);
    var growing = beds.filter(function (bd) { return bd.cells.some(function (c) { return c.task.state === "in_progress"; }); });
    var sown = beds.filter(function (bd) { return bd.cells.length; });
    var pick = (growing.length ? growing : sown.length ? sown : beds).slice().sort(near)[0];
    return pick ? box(pick) : null;
  }
  /* besideOf(p, t, taken) -> {col, row, face} | null: the free tile inside p's fence nearest box t, never in its columns, so it
     faces t. Fewest steps first, then t's rows, rows above, right; last a tile whose head covers a taken one, or right of a
     harvest cell, whose badges it would cover (gd-USSMfRaAol). */
  function besideOf(p, t, taken) {
    if (!t) return null;
    var best = null, bk = Infinity, badged = {};
    (p.beds || []).forEach(function (bd) { bd.cells.forEach(function (c) { if (c.task.state === "closed" && c.task.harvest) badged[(c.x + 1) + ":" + c.y] = 1; }); });
    for (var y = p.y + 1; y <= p.y + SLOT.h - 2; y++) for (var x = p.x + 1; x <= p.x + SLOT.w - 2; x++) {
      if ((x >= t.x0 && x <= t.x1) || taken[x + ":" + y]) continue;
      var dr = y < t.y0 ? t.y0 - y : y > t.y1 ? y - t.y1 : 0, dc = x < t.x0 ? t.x0 - x : x - t.x1;
      var k = (taken[x + ":" + (y - 1)] || badged[x + ":" + y] ? 1e6 : 0) + (dc + dr) * 1e4 + dr * 100 + (y > t.y1 ? 10 : 0) + (x > t.x1 ? 0 : 1);
      if (k < bk) { bk = k; best = { col: x, row: y, face: x < t.x0 ? "r" : "l" }; }
    }
    return best;
  }
  /* The outline's Gardeners groups (6.2, 28.7) by home cell id: a project's gardeners, need first, then by name. A hot is the
     sprite (gardener:<id>), else the plot's "+N", else (at home) its field figure's link (bead:field:<id>); one at home says
     "In the field of <project>" (fieldWhere). spots3/spots2: gardenerSpots at Bead/Plan. */
  function gardenerOutline(world, gardeners, spots2, spots3) {
    var list = (gardeners || []).slice().sort(function (a, b) { return needRank(a) - needRank(b) || (a.agent < b.agent ? -1 : a.agent > b.agent ? 1 : 0) || byNeed(a, b); });
    var ix = world.index, out = {}, homeOf = function (bid) { var b = ix[bid] || ix["project:"]; return b && b.home ? b.home.id : "home:"; };
    list.forEach(function (g) {
      var s3 = spots3.spots[g.id] || { home: homeFor(g) }, s2 = spots2.spots[g.id] || {}, p = s3.plot ? ix[s3.plot] : null, bid = s3.land || (p ? "project:" + projectOf(ix, p) : s3.home);   // no spot: its own barn
      var rw = runningWords(g, escWait(world, g)), hw = homeWord(g), e = { kind: "gardener", id: g.id, title: g.agent || g.id, state: g.state || "", bucket: g.bucket || "", plot: rw || (p ? standWhere(p) : hw !== "At the barn" ? hw : fieldWhere({ 3: spots3, world: world }, g) || "In the field"), level: 3 };   // "In the field": its field's biome is not laid out
      if (s3.land) { var b = ix[s3.land], at = stewardSpot(b); e.plot = "At the homestead of " + b.name; e.hot = "steward:" + b.key; e.level = 2; e.rect = { x: at.col, y: at.row - 1, w: 1, h: 2 }; }
      else if (!p) { e.hot = "bead:" + FIELD + g.id; e.barn = true; }   // its field figure's link
      else if (s3.shown) { e.hot = "gardener:" + g.id; e.level = s2.shown ? 2 : 3; e.rect = { x: s3.col, y: s3.row - 1, w: 1, h: 2 }; }
      else { e.hot = "gmore:" + p.id + "@3"; e.rect = { x: p.x, y: p.y, w: p.w, h: p.h }; }
      var h = homeOf(bid), gr = out[h] = out[h] || { kind: "gardeners", id: "gardeners:" + h.slice(5), hot: "gardeners:" + h.slice(5), title: "Gardeners", level: 3, children: [], n: 0, down: 0 };
      gr.children.push(e); gr.n++; if (e.bucket === "stuck") gr.down++;
      gr.sub = plural(gr.n, "gardener", "gardeners") + (gr.down ? " · " + gr.down + " down" : "");
    });
    return out;
  }
  /* gardenerState(world, gardeners) -> null for a document without gardeners[], else what the page draws from, built once per farm
     document: 2 and 3 (gardenerSpots at Plan and Bead), look and byId per id, and hots, the links of the gardeners on plots and of
     the plots' "+N" badges ({kind, id, rect in tiles, lv bitmask, plot, g | more}), and the speech bubble of each one parked on
     a letter (kind gask: a button to the Mailbox, web/reason.js), and esc, the escalation executors' second stands (escStands).
     A gardener shown at both levels stands in one place, so one link serves both. */
  function gardenerState(world, gardeners) {
    if (!gardeners) return null;
    var st = { 2: gardenerSpots(world, gardeners, 2), 3: gardenerSpots(world, gardeners, 3), look: {}, byId: {}, hots: [] };
    gardeners.forEach(function (g) { st.byId[g.id] = g; st.look[g.id] = gardenerLook(g); });
    var as = {};   // lookOf: one at work wears the pose for what it is doing now (ACTIVITY.poseOf, step 4c); a reason's pose wins
    st.lookOf = function (id) { var lk = st.look[id], p = lk && lk.pose === "work" ? ACT.poseOf(id) : "", k = id + "|" + p; return !p ? lk : as[k] || (as[k] = { pose: p, group: lk.group, shirt: lk.shirt, hat: lk.hat }); };
    st.world = world;
    st.esc = escStands(world, gardeners);
    st.homeSig = JSON.stringify(Object.keys(st[3].fields).sort().map(function (b) { return [b, st[3].fields[b].map(function (id) { return st.byId[id]; })]; }));   // the fields' stands: the page rebuilds the strips when they change
    Object.keys(st[3].plots).forEach(function (pid) {
      var p = world.index[pid];
      st[3].plots[pid].shown.forEach(function (id) {
        var s = st[3].spots[id], g = st.byId[id], lv = (st[2].spots[id].shown ? 4 : 0) | 8, bb = R.BUBBLE;
        st.hots.push({ kind: "gardener", id: id, rect: { x: s.x / TILE, y: s.y / TILE, w: 1, h: 1.5 }, lv: lv, g: g, plot: p });
        if (R.letter(g)) st.hots.push({ kind: "gask", id: id, rect: { x: (s.x + bb.dx) / TILE, y: (s.y + bb.dy) / TILE, w: bb.w / TILE, h: bb.h / TILE }, lv: lv, g: g, plot: p });
      });
      [2, 3].forEach(function (lv) { var pl = st[lv].plots[pid]; if (pl.more) st.hots.push({ kind: "gmore", id: pid + "@" + lv, rect: { x: pl.moreX / TILE, y: pl.moreY / TILE, w: 1, h: 1 }, lv: 1 << lv, more: pl.more, plot: p }); });
    });
    return st;
  }
  /* escStands(world, gardeners) -> {list, byId, hots}: an escalation's executor (escalation, no task or send: farm escalation)
     also stands beside its escalation's icon in the Escalations cell while the icon is drawn (step 7 gd-B5aDmNhmjz), so who is
     on an escalation shows from the farmstead. Feet on the icon's bottom edge, right of its slot when that is clear of the
     cell's other icons and its crate, else on the cell's east margin column, which no slot reaches (slotsOf); facing the icon.
     One per spot, need first; with no icon (closed, or past the cap) there is no such stand. It is a second stand: the plot
     stand (gardenerSpots) is unchanged, and this one neither rides nor walks (web/transitions.js), coming and going with the
     icon. list[i] is {id "xs:<gardener id>", gid, esc (the icon's id), col, row, x, y (the sprite's top left, world px),
     face, look, g}; hots[i] is its link (kind gesc, the gardener's id), at Plan and Bead. */
  function escStands(world, gardeners) {
    var out = { list: [], byId: {}, hots: [] }, ix = world && world.index || {}, taken = {};
    (gardeners || []).slice().sort(byNeed).forEach(function (g) {
      var it = g && g.escalation && !g.task && !g.tends ? ix["esc:" + g.escalation] : null, z = it && ix["home:" + it.project + "/escalation"];
      if (!it || !z) return;
      var right = { x: it.x + it.w, y: it.y, w: 1, h: it.h }, others = z.items.concat(z.crate ? [z.crate] : []);
      var col = others.some(function (o) { return o !== it && hit(right, o); }) ? z.x + z.w - 1 : right.x, k = col + ":" + it.y;
      if (taken[k]) return;
      taken[k] = 1;
      var e = { id: "xs:" + g.id, gid: g.id, esc: it.id, col: col, row: it.y + it.h - 1, x: col * TILE, y: (it.y + it.h) * TILE - 24, face: "l", look: gardenerLook(g), g: g };
      out.list.push(e); out.byId[g.id] = e;
      out.hots.push({ kind: "gesc", id: g.id, rect: { x: col, y: e.y / TILE, w: 1, h: 1.5 }, lv: 4 | 8, g: g, esc: it });
    });
    return out;
  }
  /* runningWords(g, w) -> "Running merge escalation: Merge PR #1114", what an escalation's executor is doing (its card, Porch
     row and outline row, step 7); w is its escalation's FarmWait when the farm document has it open (world.escs), for the
     family. "" for a gardener with a task of its own or no escalation. */
  function runningWords(g, w) {
    if (!g || !g.escalation || g.task) return "";
    return "Running " + (w && ESC.family(w) !== "other" ? ESC.noun(w) + " escalation" : "escalation") + ": " + (g.escalation_title || (w && w.title) || g.escalation);
  }
  function escWait(world, g) { return world && world.escs && g && g.escalation ? world.escs[g.escalation] || null : null; }
  /* ---------- the stewards at their lands' homesteads (Lands 9.8; homestead-agents-as-people.md section 9.2) ----------
     A land's steward is one figure beside its biome's name sign, live run or not. The farm's cost row carries the steward
     (its trigger, its name, and its reason: the purse wall, else a line of its memory, farm_steward.go); gardeners[] carries a
     run of it while one is live (homeFor sends it here, never to a plot or the barn). Pure, built once per farm document. */
  /* The figure's tile beside the sign: right of the plate, feet on its posts' row. */
  function stewardSpot(b) { return { col: b.sign.x + b.sign.w, row: b.sign.y + b.sign.h - 1 }; }
  /* stewardState(world, cost, gardeners, gst) -> {list, byId, hots}. cost is the farm's cost document, gst gardenerState. One
     entry per land with a biome on the map and a steward row or a live run: {id: "steward:<key>", land: biome id, x, y (the 16 x
     24 sprite's top left, world px), col, row, look, g}. g reads as a gardener (REASON.pose, gardenerWord): the live run's agent,
     state, since, href and thread when one is up, else the steward's trigger; its reason is the run's own (down, parked...),
     else, unless the run is at work, the row's; note is the row's (the memory line), cap the row's run cap. A parked reason
     adds the speech bubble's hot (gask), as for a gardener. */
  function stewardState(world, cost, gardeners, gst) {
    var out = { list: [], byId: {}, hots: [] }, rows = {}, runs = {};
    if (!world || !world.index) return out;
    ((cost && cost.projects) || []).forEach(function (r) { var k = projectKey(r.project); if (r.steward && k && world.index["project:" + k]) rows[k] = r.steward; });
    (gardeners || []).slice().sort(byNeed).forEach(function (g) {
      var land = homeFor(g), k = land.slice(8); if (!isSteward(g) || !k || !world.index[land] || runs[k]) return;
      if (gst && gst[3].spots[g.id] && !gst[3].spots[g.id].land) return;
      runs[k] = g;
    });
    Object.keys(rows).concat(Object.keys(runs).filter(function (k) { return !rows[k]; })).sort().forEach(function (k) {
      var row = rows[k] || null, run = runs[k] || null, b = world.index["project:" + k], at = stewardSpot(b);
      var reason = run && run.reason ? run.reason : run && run.state === "working" ? null : row && row.reason || null;
      var g = { id: "steward:" + k, agent: run && run.agent || row && row.agent || k + "-steward", role: "steward", project: run && run.project || b.name,
        state: run ? run.state : "", bucket: run ? run.bucket : "idle", since: run ? run.since : 0, href: run ? run.href : row.href, chat: run && run.chat || "",
        reason: reason, note: row && row.reason && row.reason !== reason ? row.reason : null, cap: row && row.capped ? row : null, run: run ? run.id : "" };
      var e = { id: g.id, land: b.id, key: k, col: at.col, row: at.row, x: at.col * TILE, y: TILE * at.row - 8, look: gardenerLook(g), g: g };
      out.list.push(e); out.byId[g.id] = e;
      out.hots.push({ kind: "steward", id: k, rect: { x: at.col, y: at.row - 0.5, w: 1, h: 1.5 }, lv: 4 | 8, g: g, st: e });
      if (R.letter(g)) { var bb = R.BUBBLE; out.hots.push({ kind: "gask", id: g.id, rect: { x: (e.x + bb.dx) / TILE, y: (e.y + bb.dy) / TILE, w: bb.w / TILE, h: bb.h / TILE }, lv: 4 | 8, g: g }); }
    });
    return out;
  }
  /* A steward's state in words: at work while a run is live, else minding its land, asleep or waiting as its reason says. */
  function stewardWord(g) {
    var p = R.pose(g);
    if (g.run && (g.state === "working" || p !== "stand")) return gardenerWord(g);
    return p === "sleep" ? "Asleep" : p === "ask" ? "Waiting on you" : p === "down" ? "Down" : "Minding the land";
  }
  /* A steward's card (its tip on hover, focus and tap): its reason (REASON.line), the memory line when the reason is not it,
     the run cap when hit, then steward · land, its state and since, and its links, Talk to the steward first. */
  function stewardCard(g, base, ago) {
    var lines = [], rl = R.line(g, base), r = R.of(g);
    if (rl) lines.push(rl);
    if (g.note && g.note.text) lines.push(["rsn rsn-" + g.note.kind, g.note.text]);
    if (g.cap) lines.push(["", "Today's " + g.cap.max_runs_per_day + " runs are used; it wakes again at 00:00 UTC"]);
    lines.push(["", "steward · " + g.project]);
    lines.push(["who", stewardWord(g) + (g.since ? " · " + ago(g.since) : "")]);
    // first, always: its view in the workbench, its conversation in the land's #general (steward view S2, gd-LisW87zrnf)
    var links = [{ text: "Talk to the steward", href: base + "/garden/c/s/" + encodeURIComponent(g.project), cls: "gsteward" }, { text: "Open bead", href: base + g.href }];
    if (g.chat) links.push({ text: "Thread in the Commons", href: base + g.chat, cls: "gchat", id: g.run });
    lines.push(["why", links]);
    var said = (r && r.text) || (g.note && g.note.text) || "";
    return { title: g.agent, lines: lines, href: base + g.href, tip: stewardWord(g) + (said ? " · " + said : "") + " · steward of " + g.project + (g.cap ? " · today's runs are used" : "") };
  }
  /* What a gardener at work on the map is doing, in words for its outline row (ACTIVITY.wordOf: "reading"), "" when its pose is
     not the work one or nothing current is heard. st is gardenerState's. */
  function doingWord(st, id) { var lk = st && st.look[id]; return lk && lk.pose === "work" ? ACT.wordOf(id) : ""; }
  /* A gardener's state in words (the card, its link's name, the outline): never colour alone. */
  // a gardener's Glass state (GL-4): down Error, at a decision Prompt, parked or asleep Parked, else its agent_state
  var GPOSE = { down: "error", ask: "prompt", sleep: "parked", start: "starting" };
  function glassState(g) { var r = R.of(g); return { state: r && r.kind === "parked" ? "parked" : GPOSE[R.pose(g)] || g.state, rate_limited: g.state === "rate_limited" }; }
  /* coop's states (GL-4) as the Glass's chip says them: [word, tone]; any other is said as it is */
  var GSTATES = { starting: ["Starting", "go"], spawning: ["Starting", "go"], working: ["Working", "ok"], idle: ["Idle", ""],
    prompt: ["Prompt", "ask"], error: ["Error", "bad"], failed: ["Error", "bad"], parked: ["Parked", "dim"], restarting: ["Restarting", "go"],
    exited: ["Exited", "dim"], done: ["Exited", "dim"], stopped: ["Exited", "dim"] };
  function gwords(s) { return String(s == null ? "" : s).replace(/[_]+/g, " ").trim(); }
  /* Who a prompt waits on, in words (rubric C9): the viewer when they may answer it (respond), else an operator, and what it asks */
  function waitLine(prompt, respond) {
    var p = prompt && typeof prompt === "object" ? prompt : {};
    var what = p.type === "permission" && p.tool ? "it asks to use " + p.tool : "it is stopped at a prompt";
    return respond ? "Waiting on you: " + what + ". Answer it below." : "Waiting on an operator: " + what + ".";
  }
  /* The Glass's chip for a state, {key, word, tone, pulse, detail}: s is internal/glass State or an agent_state string.
     Rate limited wins; Prompt and Error pulse. Here, not in glass.js: every viewer's gardener cards say it, and only an
     operator's page loads the Glass (web/lazy.js). */
  function stateView(s, respond) {
    if (!s) return { key: "unknown", word: "State unknown", tone: "dim", pulse: false, detail: "" };
    if (typeof s === "string") s = { state: s };
    var key = String(s.state || "").toLowerCase(), st = GSTATES[key], v, e = String(s.error_detail || "");
    if (s.rate_limited) v = { key: "rate_limited", word: "Rate limited", tone: "warn" };
    else v = { key: st ? key : "other", word: st ? st[0] : gwords(s.state) || "State unknown", tone: st ? st[1] : "dim" };
    v.pulse = !!s.attention || key === "prompt" || key === "error";
    v.detail = key === "prompt" && !s.rate_limited ? waitLine(s.prompt, respond)
      : key === "parked" && s.parked_reason ? "Parked: " + gwords(s.parked_reason) + "." : e ? (e.length > 200 ? e.slice(0, 199) + "…" : e) : "";
    return v;
  }
  function glassView(g) { return stateView(glassState(g)); }
  function gardenerWord(g) {
    var p = R.pose(g);
    if (g.bucket === "stuck") return "Down (" + (g.state || "stuck") + ")";
    if (p === "down") return "Down";
    if (p === "ask") return "Waiting on a human";
    if (p === "sleep") return "Asleep";
    if (g.state === "working") return "At work";
    if (g.state === "spawning" || p === "start") return "Starting";
    return "Resting";   // where it rests is fieldWhere's
  }
  /* The Growing fields' stands (28.12): every gardener with a field stand (st[3].fields), those out on plots or band items first
     in need order, then those at home; with `mine`, only that operator's; only those keep(item) passes (ANIM.still: one on its way
     has no slot). An item is {id "field:<gardener id>", kind "stand", g, task, plant, plot, href, where, aria, look, lookNow}:
     plant is the row ART.growingBead draws (its task's cell or flower row, else one from the task's words), null with no task;
     plot its plot's id when out, where (and aria) where it is out or that it is in the Commons, line its outline row's words,
     "<agent> — working: <task title>" or "<agent> — resting" (standLine). Past cap stands it keeps cap - 1
     and more is the "+N"'s N. biomeId (28.4) fills that biome's field alone. */
  var FIELD = "field:";
  function gardenerField(st, mine, base, cap, keep, biomeId) {
    var out = [], w = st.world, ix = (w && w.index) || {}, s3 = st[3], ids = [];
    if (biomeId === undefined) Object.keys(s3.fields).forEach(function (b) { ids = ids.concat(s3.fields[b]); });
    else ids = s3.fields[biomeId] || [];
    ids = ids.slice().sort(function (a, b) { return byNeed(st.byId[a], st.byId[b]); });
    var add = function (id) {
      var g = st.byId[id], sp = s3.spots[id], fd = sp.field, p = sp.plot ? ix[sp.plot] : null, c = fd.task ? ix[fd.task] : null;
      if (mine && g.operator !== mine) return;
      var plant = c && c.task && (c.kind === "task" || c.kind === "flower") ? c.task : fd.task ? { id: fd.task, title: g.task_title || g.tends_title || fd.task, type: "task", state: "in_progress" } : null;
      var it = { id: FIELD + g.id, kind: "stand", g: g, task: fd.task, plant: plant, plot: p ? p.id : "", href: base + g.href, title: g.agent || g.id, project: g.project || "", state: g.state || "", type: "agent",
        prio: 2, blocked: R.pose(g) === "ask", letter: R.letter(g), u: g.since || 0, look: st.look[g.id], lookNow: function () { return st.lookOf ? st.lookOf(g.id) : st.look[g.id]; } };
      if (p) it.where = (p.kind === "plot" ? "On " : "") + standWhere(p); else if (homeWord(g) !== "At the barn") it.where = homeWord(g);
      it.line = standLine(g, plant ? plant.title || plant.id : "");
      it.aria = figureAria(st, g, true);
      if (!keep || keep(it)) out.push(it);
    };
    ids.filter(function (id) { return s3.spots[id].plot; }).concat(ids.filter(function (id) { return !s3.spots[id].plot; })).forEach(add);
    var more = cap && out.length > cap ? out.length - cap + 1 : 0, list = more ? out.slice(0, cap - 1) : out;
    list.more = more;
    return list;
  }
  /* standWord(task) -> what a field stand is doing in words (28.12): "working: <task title>", "resting" with no task (none, or a
     thread agent); standLine(g, task) leads it with the agent: "<agent> — working: <task title>". */
  function standWord(task) { return task ? "working: " + task : "resting"; }
  function standLine(g, task) { return (g.agent || g.id) + " — " + standWord(task); }
  /* figureAria(st, g, field) -> a gardener figure's accessible name, saying where its click goes (28.12, H-D21): the field
     figure's "<agent>, working <task> on <plot>: watch in the Glass", the plot figure's (field false) "<agent>, tending <task>
     on <plot>, in the field of <project>: go home". One with no task says its state word; one beside a band item names it. */
  function figureAria(st, g, field) {
    var sp = st[3].spots[g.id] || {}, ix = (st.world && st.world.index) || {}, p = sp.plot ? ix[sp.plot] : null, fd = sp.field || {}, b = fd.home ? ix[fd.home] : null;
    var task = g.task_title || g.tends_title || fd.task || "", on = p ? " on " + standTitle(p) + (p.kind === "plot" ? "" : " in " + STAND_PLACE[p.kind]) : "";
    var name = g.agent || g.id;
    var thread = !p && isThread(g) && g.chat ? " in the Commons on " + (g.thread_title || "a thread") : "";   // the thread it answers leads a phone's strip (5b)
    if (field) return name + (task ? ", working " + task : ", " + gardenerWord(g).toLowerCase()) + (on || thread) + ": watch in the Glass";
    return name + (task ? ", tending " + task : "") + on + (b ? ", in the field of " + b.name : "") + ": go home";
  }
  /* The barn's account, in words: c is PORCH.groups' {n, home, land, groups}, every gardener in scope, those resting at home
     (in its field, on no plot) and those at a land's homestead (a steward, Lands 9.8); the rest are out in the plots. Of the n, those down (failed, stuck) are
     not running, and those down or waiting on a human need you, as the Porch's Needs you group counts them (gd-GL76uVzdAQ):
     so the sign never counts a failed gardener as running and says in words that one needs you, not only by its "!". The
     barn card and the Barn's link say it whole; short is the Barn's cursor sign ("5 need you · 18 running · 3 down · 11 out"),
     the need first, as a phone's sign drops its parts from the end. barnCount(c) is that account as plain numbers, {n, home, land, down,
     ask} (a count that already has down and ask, as the Barn's spec carries, keeps them). */
  function barnCount(c) {
    if (!c || typeof c !== "object") return c;
    var down = c.down || 0, ask = c.ask || 0;
    if (c.groups) { down = ask = 0; c.groups.forEach(function (grp) { if (grp.need) grp.rows.forEach(function (g) { if (R.pose(g) === "down") down++; else ask++; }); }); }
    return { n: c.n, home: c.home, land: c.land || 0, down: down, ask: ask };
  }
  function barnWords(c, short) {
    if (!c || !c.n) return "Nobody is running";
    c = barnCount(c);
    var out = c.n - c.home - c.land, run = c.n - c.down, need = c.down + c.ask, parts = [];
    if (need) parts.push(need + (need === 1 ? " needs you" : " need you"));
    if (run) parts.push(run + " running");
    if (c.down) parts.push(c.down + " down");
    if (short) return parts.concat([out + " out"]).join(" · ");   // a sign's parts: past its cap it drops them from the end, never the need
    var where = c.home + " resting, " + out + " out in the plots" + (c.land ? ", " + c.land + " at " + (c.land === 1 ? "a homestead" : "their homesteads") : "");
    return (need ? plural(c.n, "gardener", "gardeners") + ", " : "") + parts.join(", ") + ": " + where;
  }
  /* A gardener's spend (gardeners[].cost), or null with no figure (never a made-up $0): chip "$3.20+" ("+" a lower bound,
     gd-UIlh0DR2VG) and label "spent $3.20, at least". */
  function costWords(g) {
    var c = g && g.cost, v = c && c.spent_usd != null ? Number(c.spent_usd) : NaN;
    if (!isFinite(v)) return null;
    var s = "$" + v.toFixed(2), al = !!c.at_least;
    return { usd: v, atLeast: al, chip: s + (al ? "+" : ""), label: "spent " + s + (al ? ", at least" : "") };
  }
  /* A gardener's card (section 6.1): led by its reason (section 2.3, REASON.line), then role and project, its state and since,
     its spend, the step it tends (or is assigned at the barn), and its links, the bead and its thread in the Commons when it has one
     (named for where it goes: the agent chat it replaced is retired, gd-2Y9GQqK0Ok). ago(t) is the page's "4m". */
  function gardenerCard(g, st, world, base, ago) {
    var s = st && st[3].spots[g.id], p = s && s.plot && world ? world.index[s.plot] : null, lines = [], rl = R.line(g, base), r = R.of(g), rw = runningWords(g, escWait(world, g));
    if (rl) lines.push(rl);
    lines.push(["", (g.role || "agent") + (g.project ? " · " + g.project : "")]);
    var gw = glassView(g).word;
    lines.push(["who", gw + (!p && homeWord(g) === "At the barn" ? " at the barn" : "") + " · " + ago(g.since || 0)]);
    var al = ACT.lineOf(g.id); if (al) lines.push(["act", al]);
    var cw = costWords(g); if (cw) lines.push(["cost", g.cost.text || cw.label]);
    if (p && p.kind !== "plot") lines.push(["", "Tending: " + standTitle(p) + " in " + STAND_PLACE[p.kind]]);   // beside a band item or a goat (gd-6FCoWelnx2)
    else if (p) lines.push(["", (g.task_title ? "Tending: " + g.task_title + " in " : rw ? rw + " in " : "Tending ") + standTitle(p)]);
    else if (homeWord(g) !== "At the barn") lines.push(["", homeWord(g)]);   // a thread agent at home: the thread it answers, not its thread head as a task
    else if (g.task) lines.push(["", "Assigned: " + (g.task_title || g.task) + " (not on a plot)"]);
    else if (rw) lines.push(["", rw]);   // an escalation executor (farm escalation) whose escalation is on no plot: at the barn, and beside its icon (escStands)
    var links = [{ text: "Open bead", href: base + g.href }];
    if (g.chat) links.push({ text: "Thread in the Commons", href: base + g.chat, cls: "gchat", id: g.id });
    lines.push(["why", links]);
    return { title: g.agent || g.id, lines: lines, href: base + g.href, tip: gw + (r && r.text ? " · " + r.text : "") + " · " + (g.role || "agent") + (g.project ? " · " + g.project : "") + (!p && homeWord(g) !== "At the barn" ? " · in the Commons: " + (g.thread_title || "a thread") : g.task_title ? " · tending " + g.task_title : rw ? " · r" + rw.slice(1) : "") + (g.task_arms && g.task_arms.name ? " · " + g.task_arms.name : "") + " · " + ago(g.since || 0) + (al ? " · " + al : "") + (cw ? " · " + cw.label : "") };
  }
  /* The tip of a gardener's link, of its speech bubble, or of a plot's "+N" badge: {title, text}. */
  function gardenerTip(h, st, world, base, ago) {
    if (h.kind === "gask") return { title: h.g.agent || h.g.id, text: R.of(h.g).text + ". Open the letter in the Mailbox to answer it." };
    if (h.kind === "steward") return { title: h.g.agent, text: stewardCard(h.g, base, ago).tip };
    if (h.kind === "gmore") return { title: standTitle(h.plot), text: "+" + plural(h.more, "more gardener", "more gardeners") + (h.plot.kind === "plot" ? " on this plot." : " here.") + " Open the Porch to see every one." };
    return { title: h.g.agent || h.g.id, text: gardenerCard(h.g, st, world, base, ago).tip };
  }
  /* Who a plan's bead view lists under "Gardeners here" (web/workbench.js, through GARDEN.known): with gardeners[] (st), the
     gardeners standing on its plot, need first, each {id, name, word (gardenerWord), task (what it tends)}; without,
     the assignees of its growing steps, {name} alone, as before the farm document carried gardeners. */
  function plotWho(st, plot) {
    if (st) {
      var gp = st[3].plots[plot.id];
      return gp ? gp.ids.map(function (id) { var g = st.byId[id]; return { id: id, name: g.agent || id, word: gardenerWord(g), task: g.task_title || "" }; }) : [];
    }
    var who = [], seen = {};
    plot.beds.forEach(function (bd) { bd.cells.forEach(function (c) { var a = c.task.assignee; if (c.task.state === "in_progress" && a && !seen[a]) { seen[a] = 1; who.push({ name: a }); } }); });
    return who;
  }
  /* A plot's tip's " Gardeners here: g-down, down (failed); g-work, at work." (six, then "and N more"), "" with none. */
  function whoWords(st, plot) {
    var who = plotWho(st, plot), n = who.length - 6;
    return who.length ? " Gardeners here: " + who.slice(0, 6).map(function (w) { return w.name + (w.word ? ", " + w.word.toLowerCase() : ""); }).join("; ") + (n > 0 ? "; and " + n + " more" : "") + "." : "";
  }
  /* GARDEN.known(id) for a gardener: fills what the map did not (o is the page's answer so far). */
  function gardenerKnown(o, st, id) {
    var g = st.byId[id]; if (!g) return o;
    o.title = o.title || g.agent; o.status = o.status || g.state || ""; o.type = o.type || "agent"; o.project = o.project || g.project || "";
    o.updated = o.updated || g.since || 0; o.onMap = o.onMap || !!st[3].spots[id].shown;
    var p = st.world && st[3].spots[id].plot && st.world.index[st[3].spots[id].plot];
    if (p) o.plotAt = atFor(st.world, "plot", p.id);   // its plot's ?at=, the Glass's Go to its plot (web/glass.js)
    return o;
  }

  /* itemWords(zone, it) -> [name, sentence]: what a home strip's bead looks like and why, for its card, its hot link's label and its
     outline row. zone is "decision", "escalation" (it a FarmWait, 28.13), "growing" or "agent". */
  function itemWords(zone, it) {
    if (zone === "escalation") {
      var f = ESC.family(it), what = f === "other" ? "escalation" : ESC.noun(it) + " escalation", an = /^[aeiou]/.test(what) ? "An " : "A ";
      var name = what.charAt(0).toUpperCase() + what.slice(1);
      if (it.state === "pending") return [name, it.human || !it.waits_on ? an + what + " waits for a human to approve it. Open it to decide." : an + what + " waits on " + it.waits_on + ". Open it to see it."];
      if (it.state === "approved") return [name, it.human ? an + what + " was approved and waits on you to carry it out. Open it to see it." : an + what + " was approved and is running. Open it to see it."];
      return [name, an + what + " is " + ESC.word(it) + ". Open it to see why."];
    }
    if (zone === "decision") return it.type === "escalation" ? ["Ready to harvest", "An escalation is waiting for a human to approve it. Open it to decide."] : ["Ready to harvest", "A decision is waiting for a human. Open it to answer."];
    if (zone === "growing") {
      if (it.blocked) return ["Weeds and a rock", "Waiting on a decision before it can grow."];
      if (it.report) return ["Fruiting", "Work is in progress and a report is attached."];
      if (it.stale) return ["Wilted", "In progress, but nothing has changed for 12 hours or more."];
      return ["Growing", "In progress." + (it.type === "bug" ? " A beetle is on it: this is a bug." : "")];
    }
    var st = it.state || "unknown";
    if (st === "working") return ["Gardener at work", "Agent is working: " + st + "."];
    if (st === "failed" || st === "crashed" || st === "error" || st === "dead") return ["Scarecrow", "The agent is down (" + st + ")."];
    return ["Gardener resting", "Agent state: " + st + (it.blocked ? "; waiting on a decision." : ".")];
  }
  return {
    escStands: escStands, runningWords: runningWords, GARDENER_CAP: GARDENER_CAP, byNeed: byNeed, homeFor: homeFor, fieldHome: fieldHome,
    barnDoor: barnDoor, homeTile: homeTile, STAND_PLACE: STAND_PLACE, standItem: standItem, standTitle: standTitle,
    standWhere: standWhere, fieldWhere: fieldWhere, standWord: standWord, standLine: standLine, isThread: isThread, homeWord: homeWord, gardenerLook: gardenerLook, gardenerSpots: gardenerSpots,
    plotMarks: plotMarks, bedBox: bedBox, besideOf: besideOf, gardenerOutline: gardenerOutline, gardenerState: gardenerState,
    gardenerWord: gardenerWord, glassState: glassState, glassView: glassView, stateView: stateView, waitLine: waitLine,
    doingWord: doingWord, gardenerField: gardenerField, FIELD: FIELD, figureAria: figureAria, barnWords: barnWords, barnCount: barnCount,
    costWords: costWords, gardenerCard: gardenerCard, gardenerTip: gardenerTip, isSteward: isSteward, stewardSpot: stewardSpot,
    stewardState: stewardState, stewardWord: stewardWord, stewardCard: stewardCard, plotWho: plotWho, whoWords: whoWords,
    gardenerKnown: gardenerKnown, itemWords: itemWords
  };
};
if (typeof module !== "undefined") module.exports = PEOPLE;
