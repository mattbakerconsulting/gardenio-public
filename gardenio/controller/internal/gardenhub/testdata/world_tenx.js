/* A deterministic "10x" farm document for the Homestead World tests (gardenio/docs/homestead-world-design.md, sections 6
   and 9): 50 projects, 360 plans, about 8,000 listed tasks, seeds, harvested plans and loose beads, shaped exactly like
   the /garden/farm document (farm_model.go). Pure and seeded, so the layout golden and the frame budget see the same
   world on every machine. Loaded by the node scripts next to it; never served. */
"use strict";

/* mulberry32: a small seeded PRNG, so the fixture is byte-identical everywhere. */
function rng(seed) {
  var a = seed >>> 0;
  return function () {
    a = (a + 0x6D2B79F5) >>> 0;
    var t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

var ADJ = ["amber", "brisk", "cedar", "dusty", "ember", "fjord", "gable", "hazel", "iron", "juniper", "kelp", "lunar", "maple", "nettle", "ochre", "pebble", "quartz", "rowan", "sable", "tidal", "umber", "velvet", "willow", "yarrow", "zephyr"];
var NOUN = ["atlas", "beacon", "canopy", "delta", "engine", "forge", "grove", "harbor", "isle", "jetty", "kiln", "lantern", "meadow", "nexus", "orchard", "prism", "quarry", "ridge", "summit", "trellis", "vale", "wharf"];
var STATES = ["open", "in_progress", "review", "blocked", "deferred"];
var TYPES = ["task", "task", "task", "feature", "bug", "chore"];
var WORDS = ["Wire", "Ship", "Tune", "Audit", "Move", "Cache", "Split", "Guard", "Trim", "Land", "Fold", "Probe"];
var THINGS = ["the login theme", "the farm snapshot", "chunk baking", "the stream patch", "the memo cap", "the ring pulse", "hot links", "label placement", "the minimap", "deep links", "the outline", "the nursery band"];

/* Plans per project, largest first: one project past the 35-plot cap, a long tail of one-plan projects. Sums to 360. */
function planCounts() {
  var c = [36, 31, 24, 20, 18, 16, 14, 13, 12, 11, 10, 10, 9, 9, 8, 8, 7, 7, 7, 6, 6, 6, 5, 5, 5, 5, 4, 4, 4, 4, 3, 3, 3, 3, 3, 2, 2, 2, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1, 1, 1];
  var sum = c.reduce(function (a, b) { return a + b; }, 0);
  if (c.length !== 50 || sum !== 360) throw new Error("plan counts " + c.length + "/" + sum);
  return c;
}

function projectNames() {
  var out = ["gardenio", "music-clonk", "moorpark"], r = rng(7), seen = {};
  out.forEach(function (n) { seen[n] = 1; });
  while (out.length < 50) {
    var n = ADJ[Math.floor(r() * ADJ.length)] + "-" + NOUN[Math.floor(r() * NOUN.length)];
    if (!seen[n]) { seen[n] = 1; out.push(n); }
  }
  return out;
}

function id(r, prefix) {
  var s = "", A = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789";
  for (var i = 0; i < 10; i++) s += A[Math.floor(r() * A.length)];
  return "gd-" + (prefix || "") + s;
}
function pick(r, a) { return a[Math.floor(r() * a.length)]; }
function title(r) { return pick(r, WORDS) + " " + pick(r, THINGS); }
function counts() { return { in_progress: 0, review: 0, blocked: 0, open: 0, deferred: 0, closed: 0 }; }

/* tenx(opts) -> the document. opts.seed changes every id and draw; opts.scale multiplies the plan counts (1 = 10x). */
function tenx(opts) {
  opts = opts || {};
  var r = rng(opts.seed || 1), base = 1792000000, names = projectNames(), per = planCounts(), scale = opts.scale || 1;
  var doc = { v: 1, built_at: "2026-10-18T00:00:00Z", plans: [], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, with_mr: 0, shown: [] },
    piles: { closed_decisions: 120, closed_escalations: 60, closed_work: 4000, closed_plans: 0 }, meta: { schema: 1, rows: 0, truncated: false } };
  var tasks = 0;
  names.forEach(function (project, pi) {
    var n = Math.max(1, Math.round(per[pi] * scale));
    for (var k = 0; k < n; k++) {
      var pid = id(r, "P"), created = base - Math.floor(r() * 90 * 86400), phases = [], pc = counts(), nph = 2 + Math.floor(r() * 6);
      for (var ph = 0; ph < nph; ph++) {
        var phid = id(r, "H"), tl = [], c = counts(), listed = 2 + Math.floor(r() * 12), more = r() < 0.03 ? 5 + Math.floor(r() * 40) : 0;
        for (var t = 0; t < listed; t++) {
          var st = pick(r, STATES), ty = r() < 0.04 ? "decision" : pick(r, TYPES);
          var task = { id: id(r, "T"), title: title(r), type: ty, state: st, priority: Math.floor(r() * 5), project: project, updated: base - Math.floor(r() * 7 * 86400) };
          if (r() < 0.3) task.assignee = project + "-crew-" + Math.floor(r() * 9);
          if (st === "review") task.mr = { url: "https://github.com/example/" + project + "/pull/" + Math.floor(r() * 900), iid: "1", state: "opened" };
          if (st === "blocked") task.blocked_by = ["gd-D" + Math.floor(r() * 999)];
          c[st]++; tl.push(task); tasks++;
        }
        c.open += more; c.closed = Math.floor(r() * 10);
        phases.push({ id: phid, title: "Phase " + (ph + 1) + ": " + title(r), status: ph === 0 ? "in_progress" : "open", counts: c, tasks: tl, more: more || undefined });
        Object.keys(c).forEach(function (s) { pc[s] += c[s]; });
      }
      var plan = { id: pid, title: title(r) + " in " + project, state: "building", status: "in_progress", project: project, operator: "oidc:owner@example.com", priority: Math.floor(r() * 4),
        created: created, updated: created + 3600, activity: base - Math.floor(r() * 3 * 86400), phases: phases, unphased: { id: "", title: "", status: "", counts: counts(), tasks: [] }, counts: pc,
        open_decisions: r() < 0.1 ? 1 + Math.floor(r() * 2) : 0, open_escalations: r() < 0.05 ? 1 : 0 };
      doc.plans.push(plan);
    }
    /* seeds (about 5 a project, every one a draft: a ready seed is staked on a plot, which TestWorldLayoutStaked covers, and the
       10x golden stays the farm with no ready seed; the draw that once picked the state still runs, so every id is as it was),
       harvested plans (2 shown a project) and a few loose beads */
    var ns = Math.floor(r() * 10);
    for (var s = 0; s < ns; s++) doc.seeds.push({ id: id(r, "S"), title: title(r), state: (r(), "frosted"), status: "open", project: project, operator: "oidc:owner@example.com", priority: 2, created: base - Math.floor(r() * 86400 * 30), updated: base, phases: 1 + Math.floor(r() * 5), tasks: Math.floor(r() * 20) });
    /* the document shows at most 24 harvested plans in all (maxClosedPlansShown); the first 24 projects get one each */
    if (pi < 24) doc.closed_plans.shown.push({ id: id(r, "C"), title: title(r), state: "complete", project: project, updated: base - Math.floor(r() * 86400 * 60) });
    doc.closed_plans.total += 2 + Math.floor(r() * 6);
  });
  /* the meadow is capped at 40 shown in the document; a few have no project and live in the Commons */
  for (var m = 0; m < 40; m++) {
    var mp = m % 7 === 0 ? "" : names[Math.floor(r() * names.length)];
    doc.meadow.shown.push({ id: id(r, "L"), title: title(r), type: pick(r, TYPES), state: pick(r, STATES), priority: 2, project: mp, updated: base - Math.floor(r() * 86400) });
  }
  doc.meadow.total = 300;
  doc.piles.closed_plans = doc.closed_plans.total;
  doc.meta.rows = doc.plans.length + doc.seeds.length + tasks + doc.meadow.shown.length;
  return doc;
}

/* withGardeners(doc, n) -> doc with n gardeners (gardenio/docs/homestead-agents-as-people.md section 2), shaped like the farm's
   gardeners[]: two thirds on the plots of the project with the most plans (one to four a plot, so some plots carry a "+N"),
   most of the rest on other projects' plots, and a few at home. States cycle through working, asking, down and starting. */
function withGardeners(doc, n) {
  var r = rng(17), by = {}, out = [], STATES = [["working", "alive"], ["working", "alive"], ["yielding", "alive"], ["failed", "stuck"], ["spawning", "alive"], ["working", "alive"]];
  doc.plans.forEach(function (p) { (by[p.project] = by[p.project] || []).push(p); });
  var dense = Object.keys(by).sort(function (a, b) { return by[b].length - by[a].length; })[0], others = doc.plans.filter(function (p) { return p.project !== dense; });
  for (var i = 0; i < n; i++) {
    var st = STATES[i % STATES.length], plan = null;
    if (i < Math.round(n * 2 / 3)) plan = by[dense][Math.floor(r() * Math.min(12, by[dense].length))];
    else if (i < n - 5) plan = pick(r, others);
    var ph = plan && plan.phases[Math.floor(r() * plan.phases.length)], task = ph ? ph.tasks[Math.floor(r() * ph.tasks.length)] : null;
    var gid = id(r, "G");
    out.push({ id: gid, agent: "crew-" + i, role: i % 7 ? "crew" : "plan-gardener", project: plan ? plan.project : "", operator: "oidc:owner@example.com", state: plan ? st[0] : "idle", bucket: plan ? st[1] : "idle",
      task: task ? task.id : "", task_title: task ? task.title : "", plot: plan ? { plan: plan.id, phase: ph.id } : null, since: 1792000000 - i * 60, href: "/garden?bead=" + gid, reason: null });
  }
  doc.gardeners = out;
  return doc;
}

/* withClosed(doc) -> doc with each phase's closed count listed as FarmPhase.closed[] rows (cursor stops, PR #872), the plan's
   12 newest carrying a harvest object (farm_harvest.go, harvest spec 3) with a mix of bases and badges, so the frame budget
   sees harvest cells. The layout golden is the plain tenx() and never sees these. */
function withClosed(doc) {
  var r = rng(23), base = 1792000000, BASES = ["sheaf", "sheaf", "sheaf", "trampled", ""];
  doc.plans.forEach(function (plan) {
    var rows = [];
    plan.phases.forEach(function (ph) {
      ph.closed = [];
      for (var i = 0; i < (ph.counts.closed || 0); i++) {
        var row = { id: id(r, "K"), title: title(r), type: "task", state: "closed", priority: 2, project: plan.project, updated: base - Math.floor(r() * 30 * 86400) };
        ph.closed.push(row); rows.push(row);
      }
    });
    rows.sort(function (a, b) { return b.updated - a.updated; }).slice(0, 12).forEach(function (row) {
      var h = { closed: row.updated }, b = pick(r, BASES);
      if (b) h.base = b;
      if (b === "sheaf") {
        h.mr = "#" + Math.floor(r() * 900);
        if (r() < 0.6) h.ux = [3 + Math.floor(r() * 3), 5];
        if (r() < 0.6) h.pics = Math.floor(r() * 9);
        if (r() < 0.7) h.tests = Math.floor(r() * 6);
        h.ci = r() < 0.8 ? "success" : "failed";
        h.rounds = 1 + Math.floor(r() * 3);
      }
      row.harvest = h;
    });
  });
  return doc;
}

/* A deep copy, so a mutation never touches the base document. */
function clone(doc) { return JSON.parse(JSON.stringify(doc)); }

module.exports = { tenx: tenx, withGardeners: withGardeners, withClosed: withClosed, clone: clone, rng: rng, projectNames: projectNames, planCounts: planCounts };
