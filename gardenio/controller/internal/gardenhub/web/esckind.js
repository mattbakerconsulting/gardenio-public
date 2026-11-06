/* ESCKIND: an open escalation's words (plan gd-PJB3D7glwF, step 2 gd-ftQFRlBDZI; homestead-world-design.md 28.11), one
   vocabulary for wherever an escalation is drawn (its home strip cell, its plot, the bead view), so they never say it two
   ways. Pure and placement-agnostic: it reads one FarmWait (homestead-farm-read-model.md, the escalations blob and a plan's
   waiting[]) and draws nothing. Run under node by TestEscKindUnderNode (testdata/esckind_test.js).
     family(w) the family key, one of FAMILIES ("other" for an unknown or missing one); noun(w) its noun;
     word(w) the state word, which always says the state, so colour is never the only sign of it;
     who(w) who it waits on: "you" for a human wait, else waits_on, "" when it names nobody;
     since(w, now) "2 h" since the current wait began (now in ms), "" with no since;
     name(w, now) the accessible name: "Merge escalation: <title>, waiting on you, since 2 h";
     group(w) and compare(a, b), order(list): the cell's order, those waiting on you first (0), then the ones in hand,
       approved or with the desk (1), then rejected and failed (2), newest first within a group, then id;
     lineage(plans) and from(w, lin): the plan (else the project) it came from, read from the plans' waiting[];
     tip(w, now, lin) the sign tip's [title, text]; answerable(w, mayEsc) whether its cell offers Approve and Reject;
     cells(list, opts) the escalations cell's grid, past opts.cap a "+N" crate; count(n) a home cell's "3 escalations"
       (step 6 gd-4FIF5pzC6O, 28.11). */
var ESCKIND = (function () {
  "use strict";
  /* The keys are the Go family table's (farm_model.go Esc*, farm_escalations.go); TestEscKindFamiliesMatchGo holds the two
     to one set, so a family cannot land on one side only. */
  var NOUN = { merge: "merge", deploy: "deploy", config: "config edit", prompt: "prompt install", beads: "beads admin",
    access: "access", infra: "infra", read: "read-only query", other: "escalation" };
  var FAMILIES = Object.keys(NOUN);

  function family(w) { var f = w && w.family; return Object.prototype.hasOwnProperty.call(NOUN, f) ? f : "other"; }
  function noun(w) { return NOUN[family(w)]; }
  function who(w) { return !w ? "" : w.human ? "you" : String(w.waits_on || ""); }
  function word(w) {
    w = w || {};
    switch (w.state) {
      case "pending": return "waiting on " + (who(w) || "approval");
      case "approved": return w.human ? "approved, waiting on you" : "approved, running";
      case "rejected": return "rejected, still open";
      case "failed": return "failed, still open";
    }
    return "open";
  }
  function since(w, now) {
    if (!w || !w.since) return "";
    var d = Math.max(0, Math.floor(now / 1000) - w.since);
    return d < 3600 ? Math.max(1, Math.floor(d / 60)) + " min" : d < 86400 ? Math.floor(d / 3600) + " h" : Math.floor(d / 86400) + " d";
  }
  function name(w, now) {
    w = w || {};
    var f = family(w), lead = f === "other" ? "Escalation" : NOUN[f].charAt(0).toUpperCase() + NOUN[f].slice(1) + " escalation";
    var st = word(w), by = who(w), s = since(w, now);
    var parts = [lead + ": " + (w.title || w.id || ""), st];
    if (by && st.indexOf("waiting on " + by) < 0) parts.push("waits on " + by);   // said once: the state word may name them
    if (s) parts.push("since " + s);
    return parts.join(", ");
  }
  function group(w) {
    w = w || {};
    if (w.state === "rejected" || w.state === "failed") return 2;
    return w.human ? 0 : 1;
  }
  function compare(a, b) {
    return (group(a) - group(b)) || ((b.since || 0) - (a.since || 0)) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  }
  function order(list) { return (list || []).slice().sort(compare); }

  /* The escalation id -> its plan, from each plan's waiting[] (the farm joins requesting_plan, else requesting_task's plan;
     farm.go finishWaits): the first plan to list it wins. A plan's waiting[] is how the page knows an escalation's plan. */
  function lineage(plans) {
    var m = {};
    (plans || []).forEach(function (p) {
      ((p && p.waiting) || []).forEach(function (w) { if (w && w.kind === "escalation" && !m[w.id]) m[w.id] = p; });
    });
    return m;
  }
  /* "plan <short title>" (else its title, else its id) when a plan lists it, else "project <project>", else "" (an unprojected
     one, the Commons'). The short title is the name the plan goes by everywhere else (gd-4FIF5pzC6O). */
  function from(w, lin) {
    var p = w && lin && lin[w.id];
    return p ? "plan " + (p.short_title || p.title || p.id) : w && w.project ? "project " + w.project : "";
  }
  /* The sign tip, as the Mailbox's (garden.js tipTitle, tipText): the title, then the noun, the state, who, since and where it
     came from, in name()'s words. */
  function tip(w, now, lin) {
    w = w || {};
    var n = name(w, now), title = w.title || w.id || "", head = n.slice(0, n.indexOf(": ")), rest = n.slice(n.indexOf(": ") + 2 + title.length);
    var f = from(w, lin);
    return [title, head + rest + (f ? ", from " + f : "") + "."];
  }
  /* A pending escalation offers web/answer.js's Approve and Reject to an account that may approve one (body[data-esc]); an
     approved, rejected or failed one is past that answer (gd-yuOG6cX6VI). */
  function answerable(w, mayEsc) { return !!mayEsc && !!w && w.state === "pending"; }

  /* cells(list, opts): one cell per escalation in order(), opts.cols wide (the cell's slot columns), each
     {id, kind: "escalation", w, i, r, c}. Past opts.cap (the cell's slot count, WORLD.escOf; no limit when unset) the first cap - 1
     and, in the last cell, a crate {id: opts.id + "/more", kind: "crate", n} like a bed's (world.js planBeds); n counts
     opts.total as well, since the farm caps escalations.open, so none is hidden without a count. */
  function cells(list, opts) {
    opts = opts || {};
    var cols = Math.max(1, opts.cols | 0), ws = order(list), all = Math.max(ws.length, opts.total | 0), cap = opts.cap > 0 ? opts.cap : Infinity;
    var fit = all > cap || all > ws.length ? Math.min(ws.length, cap - 1) : ws.length, out = [];
    var at = function (i, o) { o.i = i; o.r = Math.floor(i / cols); o.c = i % cols; out.push(o); };
    ws.slice(0, fit).forEach(function (w, i) { at(i, { id: w.id, kind: "escalation", w: w }); });
    if (all > fit) at(fit, { id: (opts.id || "escalations") + "/more", kind: "crate", n: all - fit });
    return out;
  }
  /* What a project's home cell says: "1 escalation", "3 escalations"; "" for none. */
  function count(n) { return n > 0 ? n + (n === 1 ? " escalation" : " escalations") : ""; }

  return { FAMILIES: FAMILIES, NOUN: NOUN, family: family, noun: noun, who: who, word: word, since: since, name: name,
    group: group, compare: compare, order: order, lineage: lineage, from: from, tip: tip, answerable: answerable, cells: cells,
    count: count };
})();
if (typeof module !== "undefined") module.exports = ESCKIND;
