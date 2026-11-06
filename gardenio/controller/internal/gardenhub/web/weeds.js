/* WEEDS: maintenance lint for the Homestead world (gd-ytTNc3Fu9M). Pure rules over the /garden/farm document find the things
   that need tending, and each one says why it is a weed:
     - unlabelled: a plan, seed or bead with no project:<name> label (it lands in the Commons, not its project's region);
     - phaseless:  a plan being built with no phases (every step sits in one "Other" row);
     - overgrown:  a plan with more than 9 beds (phases plus Other): its plot shows eight and a crate (cursor-grid spec 5.3);
     - frosted:    a draft plan with no update for FROST_S;
     - orphan:     an open PR whose bead is closed, or that no bead links (piles.orphan_mrs, read server side);
     - wilt:       an in-progress bead with no update for WILT_S;
     - untended:   an open wish (a thread head, the document's wishes.open) older than STALE_S that nothing tends (none of its
                   kin is in progress or in review: its ghost goat, web/goats.js). Its line is in the wish's own bead view, and
                   its region tallies it.
   A project with no activity for DORMANT_S is dormant: fog lies over its region at the World and Project levels.
   lint() and index() are pure (node-tested by page_test.go); draw() and drawLegend() paint with fillRect and drawImage only,
   so they also run under the node canvas mock. garden.js keeps the hooks: index on each document, draw per level, and the
   bead view's "Weeds" lines (web/workbench.js, through GARDEN.known). */
var WEEDS = (function () {
  "use strict";
  var HOUR = 3600, DAY = 86400;
  var WILT_S = 12 * HOUR;      // in progress with no update for this long = wilted (the scene's and plans.js's threshold)
  var FROST_S = 7 * DAY;       // a draft untouched this long is long-frosted
  var DORMANT_S = 14 * DAY;    // a project with no activity this long is dormant, under fog
  var STALE_S = 7 * DAY;       // an open wish this old that nothing tends is an untended wish
  var LINES_MAX = 8;           // weed lines in one bead view; the rest are counted
  var GLYPH_PX = 20;           // the thistle's screen size where it is drawn at a constant size (Project level, plot corners)
  /* Neglect weight per rule, for rank(): an orphaned PR or a wilted bead holds work up; the rest are tidiness. A weed's score
     is its weight times 1 + log2(1 + days since it became one). */
  var RULES = {
    orphan: { name: "Orphaned PR", weight: 3, one: "orphaned PR", many: "orphaned PRs" },
    wilt: { name: "Wilted", weight: 2, one: "wilted bead", many: "wilted beads" },
    untended: { name: "Untended wish", weight: 1, one: "untended wish", many: "untended wishes" },
    frosted: { name: "Long-frosted draft", weight: 1, one: "long-frosted draft", many: "long-frosted drafts" },
    phaseless: { name: "Plan without phases", weight: 1, one: "plan without phases", many: "plans without phases" },
    overgrown: { name: "Overgrown plan", weight: 1, one: "overgrown plan", many: "overgrown plans" },
    unlabelled: { name: "No project label", weight: 1, one: "unlabelled bead", many: "unlabelled beads" }
  };
  var ORDER = ["orphan", "wilt", "untended", "frosted", "phaseless", "overgrown", "unlabelled"];
  var BEDS = 9;                // a plot's 3x3 of phase beds (WORLD.BED.phases)

  function key(p) { return String(p || "").trim().toLowerCase(); }   // WORLD.projectKey: the biome a project names
  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }
  /* An age in words: hours under two days, then days. */
  function age(s) { s = Math.max(0, s); return s < 2 * DAY ? plural(Math.floor(s / HOUR), "hour", "hours") : plural(Math.floor(s / DAY), "day", "days"); }
  function openTasks(p) {
    var n = 0;
    (p.phases || []).concat(p.unphased ? [p.unphased] : []).forEach(function (ph) { var c = ph.counts || {}; n += (c.in_progress || 0) + (c.review || 0) + (c.blocked || 0) + (c.open || 0) + (c.deferred || 0); });
    return n;
  }

  /* lint(doc, now) -> { weeds: [Weed], fog: {projectKey: {last, idle}}, counts: {rule: n} }. now is unix seconds.
     Weed: { rule, id, kind (plan|task|seed|flower|pr|wish), key (the biome it sits in), plan (its plan, for a step), title, why,
     since (unix s, when it became a weed), score }. A step's biome is its plan's, whatever its own label says. Inside an
     unlabelled plan only the plan is a weed, not each of its steps. */
  function lint(doc, now) {
    var weeds = [], last = {};
    doc = doc || {};
    var touch = function (project, t) { var k = key(project); if (t > 0 && !(last[k] >= t)) last[k] = t; if (!(k in last)) last[k] = 0; };
    var add = function (rule, it, why, since) {
      var w = { rule: rule, id: it.id, kind: it.kind, key: it.key, plan: it.plan || "", bead: it.bead || "", title: it.title || it.id, why: why, since: since || 0 };
      w.score = RULES[rule].weight * (1 + Math.log2(1 + Math.max(0, now - w.since) / DAY));   // age counts, on a log scale, so the rule's weight still tells
      weeds.push(w);
    };
    var bead = function (t, kind, k, plan, labelled) {
      var it = { id: t.id, kind: kind, key: k, plan: plan, title: t.title };
      if (labelled && !key(t.project)) add("unlabelled", it, kind === "flower" ? "it grows in the Commons. Add project:<name>." : "its project's lists miss it. Add project:<name>.", t.updated);
      if (t.state === "in_progress" && now - (t.updated || 0) > WILT_S)
        add("wilt", it, "in progress, but no change for " + age(now - (t.updated || 0)) + " (it wilts at 12 hours)." + (t.assignee ? " Gardener: " + t.assignee + "." : " Nobody is assigned."), (t.updated || 0) + WILT_S);
    };
    (doc.plans || []).forEach(function (p) {
      var k = key(p.project), labelled = !!k;
      touch(p.project, p.activity || p.updated);
      var it = { id: p.id, kind: "plan", key: k, title: p.title };
      if (!labelled) add("unlabelled", it, "it grows in the Commons. Add project:<name> to the plan epic.", p.created || p.updated);
      if (!(p.phases || []).length) {
        var n = openTasks(p);
        add("phaseless", it, (n ? plural(n, "open step sits", "open steps sit") + " in one Other row" : "nothing is planted yet") + ". Give the plan a phase_order.", p.created || p.updated);
      }
      var u = p.unphased, beds = (p.phases || []).length + (u && ((u.tasks || []).length || openTasks({ phases: [u] }) || (u.counts || {}).closed) ? 1 : 0);
      if (beds > BEDS) add("overgrown", it, "more than " + BEDS + " phases (" + beds + " beds); split the plan.", p.created || p.updated);
      (p.phases || []).concat(p.unphased ? [p.unphased] : []).forEach(function (ph) { (ph.tasks || []).forEach(function (t) { bead(t, "task", k, p.id, labelled); }); });
    });
    (doc.seeds || []).forEach(function (s) {
      var k = key(s.project), it = { id: s.id, kind: "seed", key: k, title: s.title };
      touch(s.project, s.updated);
      if (!k) add("unlabelled", it, "it waits in the Commons. Add project:<name> to the plan epic.", s.created || s.updated);
      if (s.state === "frosted" && now - (s.updated || 0) > FROST_S)
        add("frosted", it, "no update for " + age(now - (s.updated || 0)) + ". Finish it and mark it ready, or close it.", (s.updated || 0) + FROST_S);
    });
    ((doc.meadow || {}).shown || []).forEach(function (t) { touch(t.project, t.updated); bead(t, "flower", key(t.project), "", true); });
    ((doc.closed_plans || {}).shown || []).forEach(function (c) { touch(c.project, c.updated); });
    ((doc.wishes || {}).open || []).forEach(function (w) {
      if (!w || !w.id || w.tending || !(now - (w.opened || now) > STALE_S)) return;
      var n = w.kin_total || 0;
      add("untended", { id: w.id, kind: "wish", key: key(w.project), title: w.title },
        "open for " + age(now - w.opened) + " and nothing tends it: " + (n ? "none of its " + plural(n, "step", "steps") + " is in progress. Work one, or close the wish." : "no plan or step has come of it. Start on it, or close the wish."), w.opened + STALE_S);
    });
    ((doc.piles || {}).orphan_mrs || []).forEach(function (o) {
      var pr = "PR" + (o.iid ? " #" + o.iid : "") + (o.repo ? " in " + o.repo : "");
      var why = o.why === "bead_closed" ? pr + " is still open, but its bead " + o.bead + " is closed. Merge or close the PR, or reopen the bead." : pr + " is open, but no bead links it. Link one with gb mr link, or close the PR.";
      add("orphan", { id: o.url, kind: "pr", key: key(o.project), title: o.title || pr, bead: o.bead }, why, o.since);
    });
    var fog = {};
    Object.keys(last).forEach(function (k) { if (last[k] > 0 && now - last[k] > DORMANT_S) fog[k] = { last: last[k], idle: now - last[k] }; });
    var counts = {};
    ORDER.forEach(function (r) { counts[r] = 0; });
    weeds.forEach(function (w) { counts[w.rule]++; });
    return { weeds: weeds, fog: fog, counts: counts, total: weeds.length, orphanTotal: (doc.piles || {}).orphan_mr_total || 0 };
  }
  /* The most neglected first (the chore tour's order): score, then the older weed, then id. */
  function rank(weeds) {
    return weeds.slice().sort(function (a, b) { return b.score - a.score || a.since - b.since || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0) || (a.rule < b.rule ? -1 : 1); });
  }

  /* index(lint, world) -> where each weed sits in a laid-out world (WORLD.layout):
     { by: {id: [Weed]} for the placed thing a weed names (a task cell, plot, seed pot or flower), wishes: {wishId: [Weed]} (a
       wish is no placed thing: its goat wanders), plots: {planId: [Weed]} (the
       plan's own weeds and its steps', placed or past the cap), biomes: {biomeId: [Weed]} (everything in the region, orphaned
       PRs included), fog: {biomeId: fog}, unplaced: [Weed] (in no biome: an orphaned PR whose project has no region) }. */
  function index(L, world) {
    var ix = { by: {}, wishes: {}, plots: {}, biomes: {}, fog: {}, unplaced: [], lint: L };
    if (!L) return ix;
    var push = function (m, k, w) { (m[k] || (m[k] = [])).push(w); };
    var at = world && world.index || {}, biomes = {};
    (world && world.biomes || []).forEach(function (b) { biomes[b.key] = b; if (L.fog[b.key]) ix.fog[b.id] = L.fog[b.key]; });
    rank(L.weeds).forEach(function (w) {
      var b = biomes[w.key];
      if (w.kind === "wish") push(ix.wishes, w.id, w);   // its bead view says so wherever its region is
      if (!b) { ix.unplaced.push(w); return; }
      push(ix.biomes, b.id, w);
      if (w.kind === "plan" || (w.kind === "seed" && at[w.id] && at[w.id].kind === "plot")) push(ix.plots, w.id, w);   // a staked seed is its plot's own weed
      else if (w.plan) push(ix.plots, w.plan, w);
      if (w.kind !== "pr" && at[w.id]) push(ix.by, w.id, w);
    });
    return ix;
  }
  function tally(ws) {
    var c = {}, out = [];
    ws.forEach(function (w) { c[w.rule] = (c[w.rule] || 0) + 1; });
    ORDER.forEach(function (r) { if (c[r]) out.push(plural(c[r], RULES[r].one, RULES[r].many)); });
    return out.join(", ");
  }
  function fogLine(f) { return "Dormant: no activity for " + age(f.idle) + ", so fog lies over this region."; }
  /* The bead view's "Weeds" lines for a world Thing (web/workbench.js, through GARDEN.known; the info card that listed them retired
     with cursor-grid spec N-D14): [cls, text, href, title], the "why is this a weed" answers, each the rule's name then its why.
     A plot lists its weeds and its steps' (max, default LINES_MAX, then a count); a biome says whether it is dormant, tallies
     its weeds and names its orphaned PRs; a task, seed, flower or wish lists its own. href(bead) is the in-garden bead view link
     (garden.js beadHref). */
  function lines(ix, h, href, max) {
    if (!ix || !h) return [];
    max = max || LINES_MAX;
    var k = h.kind, ws, out = [], fog = null, id = h.id;
    if (k === "biome" || k === "sign") {
      fog = ix.fog[id];
      ws = (ix.biomes[id] || []).filter(function (w) { return w.kind === "pr"; });
      var all = ix.biomes[id] || [];
      if (fog) out.push(["weed", fogLine(fog)]);
      if (all.length) out.push(["weed", "Weeds to tend: " + tally(all) + "."]);
    } else if (k === "plot" || k === "plotsign") ws = ix.plots[id] || [];
    else if (k === "task" || k === "seed" || k === "flower") ws = ix.by[id] || [];
    else if (k === "wish") ws = ix.wishes[id] || [];
    else return [];
    ws.slice(0, max).forEach(function (w) {
      var self = (k === "task" || k === "seed" || k === "flower" || k === "wish") || ((w.kind === "plan" || w.kind === "seed") && w.id === id);
      var text = RULES[w.rule].name + ": " + (self || w.kind === "pr" ? "" : w.title + ", ") + w.why;
      var bead = w.kind === "pr" ? w.bead : self ? "" : w.id;
      out.push(bead && href ? ["weed", text, href({ id: bead }), bead] : ["weed", text]);
    });
    if (ws.length > max) out.push(["why", "+" + plural(ws.length - max, "more weed", "more weeds")]);
    return out;
  }
  /* The accessible name's suffix for a world hot: the same facts in a few words (state is never colour alone). */
  function aria(ix, h) {
    if (!ix || !h) return "";
    var k = h.kind, ws = [], s = "";
    if (k === "biome" || k === "sign") { var f = ix.fog[h.id]; if (f) s += " Dormant for " + age(f.idle) + "."; ws = ix.biomes[h.id] || []; }
    else if (k === "plot" || k === "plotsign") ws = ix.plots[h.id] || [];
    else if (k === "task" || k === "seed" || k === "flower") ws = ix.by[h.id] || [];
    else if (k === "wish") ws = ix.wishes[h.id] || [];
    if (ws.length) s += " Weeds: " + tally(ws) + ".";
    return s;
  }
  /* Does a plot hold a weed (its plan's or a step's)? Its corner wears a thistle. */
  function plotWeedy(ix, id) { return !!(ix.plots[id] && ix.plots[id].length); }

  /* ---------- drawing ---------- */
  /* A thistle: a purple head on a spiky stem, outlined, so it reads on grass, soil and crops and is not the blocked step's
     grass tuft (weeds and a rock). Palette keys are art.js's. */
  var THISTLE = [
    "................",
    "......kkkk......",
    ".....kWvvWk.....",
    "....kvVvvVvk....",
    "....kvvVVvvk....",
    ".....kVvvVk.....",
    "......kLLk......",
    ".....kLmmLk.....",
    "......kLLk......",
    "..kk...kLk..kk..",
    ".kmLk..kLk.kLmk.",
    "..kLLk.kLkkLLk..",
    "...kLLkkLkLLk...",
    "....kLLLLLLk....",
    ".....kkkkkk.....",
    "................"
  ];
  function put(ctx, pal, rows, x, y, s) {
    for (var r = 0; r < rows.length; r++) for (var c = 0; c < rows[r].length; c++) {
      var ch = rows[r].charAt(c);
      if (ch === "." || !pal[ch]) continue;
      ctx.fillStyle = pal[ch]; ctx.fillRect(x + c * s, y + r * s, s, s);
    }
  }
  function paintThistle(ctx, pal) { put(ctx, pal, THISTLE, 0, 0, 1); }
  /* Fog over a footprint (tiles; T world px a tile): a pale wash, then drifting banks placed by the region's key, so a dormant
     region reads as fogged and not as a lighter biome. Static: nothing animates, with or without reduced motion. */
  function fogBanks(seed, f) {
    var out = [], h = seed >>> 0, n = 3 + Math.floor(f.h / 24);
    for (var i = 0; i < n; i++) {
      h = Math.imul(h ^ (h >>> 15), 2246822507) >>> 0; h = Math.imul(h ^ (h >>> 13), 3266489909) >>> 0;
      var w = Math.max(6, Math.round(f.w * (0.35 + (h % 40) / 100))), y = f.y + 2 + Math.round((i + 0.5) * (f.h - 4) / n);
      out.push({ x: f.x + 2 + ((h >>> 8) % Math.max(1, f.w - w - 4)), y: y, w: w, h: 2 + ((h >>> 20) % 2) });
    }
    return out;
  }
  function drawFog(ctx, pal, b, T, night) {
    var f = b.foot, a = ctx.globalAlpha;
    ctx.globalAlpha = a * (night ? 0.42 : 0.5); ctx.fillStyle = pal.q;
    ctx.fillRect((f.x + 1) * T, (f.y + 1) * T, (f.w - 2) * T, (f.h - 2) * T);
    ctx.globalAlpha = a * (night ? 0.5 : 0.6); ctx.fillStyle = pal.j;
    fogBanks(b.seed || 0, f).forEach(function (k) { ctx.fillRect(k.x * T, k.y * T, k.w * T, k.h * T); ctx.fillRect((k.x + 2) * T, (k.y - 1) * T, (k.w - 4) * T, T); });
    ctx.globalAlpha = a;
  }
  /* draw(ctx, o): the weed layer, in world px under garden.js's world transform, after the plots and band items.
     o = { ix, vis (WORLD.visible), level, z, pal, T, night, bmp(key, w, h, paint) }.
     World and Project: fog over the dormant regions in view. Project and in: a thistle on the corner of each plot holding a weed,
     at a constant GLYPH_PX (never under a tile), so a plot says so even where a phase label covers its first cells. Plan: a
     thistle on each weedy step, seed and flower too. Bead: the same, smaller and off the plant's middle, except a step that is
     only wilted, whose wilted sprite already says so. */
  function draw(ctx, o) {
    var ix = o.ix, T = o.T, lv = o.level, img = o.bmp("weeds-thistle", 16, 16, paintThistle);
    if (!ix) return;
    var glyph = function (x, y, s) { ctx.drawImage(img, x, y, s, s); };
    if (lv <= 1) o.vis.biomes.forEach(function (b) { if (ix.fog[b.id]) drawFog(ctx, o.pal, b, T, o.night); });
    if (lv === 0) return;
    var big = Math.max(T, GLYPH_PX / o.z);
    o.vis.plots.forEach(function (p) {
      var corner = function (s) { glyph((p.x + p.w) * T - s * 0.8, (p.y + p.h) * T - s * 0.8, s); };
      if (plotWeedy(ix, p.id)) corner(big);
      if (lv === 1) return;
      p.beds.forEach(function (bd) {
        bd.cells.forEach(function (c) {
          var ws = ix.by[c.id];
          if (!ws || !ws.length) return;
          if (lv === 2) glyph(c.x * T, c.y * T, T);
          else if (ws.some(function (w) { return w.rule !== "wilt"; })) glyph(c.x * T + T * 0.45, c.y * T - T * 0.1, T * 0.65);
        });
      });
    });
    o.vis.items.forEach(function (it) {
      if (!ix.by[it.id] || !ix.by[it.id].length) return;
      if (lv === 1) glyph(it.x * T, it.y * T - big / 2, big);
      else if (it.kind === "seed") glyph((it.x + it.w) * T - T, it.y * T, T);
      else glyph(it.x * T + T * 0.45, it.y * T - T * 0.1, T * (lv === 2 ? 1 : 0.65));
    });
  }
  /* A legend glyph onto a 24 x 28 canvas, like ART.drawLegend: "thistle" or "fog". */
  function drawLegend(ctx, kind, pal) {
    if (kind === "thistle") { put(ctx, pal, THISTLE, 4, 6, 1); return; }
    ctx.fillStyle = pal.g; ctx.fillRect(0, 4, 24, 20); ctx.fillStyle = pal.d; ctx.fillRect(0, 20, 24, 4);
    var a = ctx.globalAlpha; ctx.globalAlpha = a * 0.5; ctx.fillStyle = pal.q; ctx.fillRect(0, 4, 24, 20);
    ctx.globalAlpha = a * 0.7; ctx.fillStyle = pal.j; ctx.fillRect(2, 9, 16, 2); ctx.fillRect(6, 15, 16, 3); ctx.fillRect(4, 8, 8, 1);
    ctx.globalAlpha = a;
  }

  return {
    WILT_S: WILT_S, FROST_S: FROST_S, DORMANT_S: DORMANT_S, STALE_S: STALE_S, LINES_MAX: LINES_MAX, GLYPH_PX: GLYPH_PX, RULES: RULES, ORDER: ORDER, THISTLE: THISTLE,
    lint: lint, rank: rank, index: index, lines: lines, aria: aria, age: age, fogBanks: fogBanks, draw: draw, drawLegend: drawLegend
  };
})();
if (typeof module !== "undefined") module.exports = WEEDS;
