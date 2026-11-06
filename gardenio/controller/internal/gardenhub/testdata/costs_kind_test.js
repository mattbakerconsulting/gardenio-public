/* The Costs panel's DOM half, the Workbench's costs kind (plan gd-gh5J0vVbJP step 2.4, gd-5HipEi6w2o; 2.1's web/costs.js),
   run by TestCostsKindUnderNode:  node costs_kind_test.js <web dir>
   costs.js loads under a stub WORKBENCH and a small DOM (the Workbench's #bv-title, #bv-kind and #bv-body, the farm's cost
   through GARDEN.cost, a localStorage), and the kind it registers is driven the way workbench.js drives it:
   - paint: the loading paint (header and chips, no cards); then the answer: the title, the header's figures with the
     farm's stale line, the chips (a radiogroup, the stored range checked and the only one in the tab order), the cards
     and the dashboard and sign-in links, the lazy observer rooted on the panel body; keyboard focus runs the checked chip,
     then each card's Open link and its frame, then the two links, in document order, with nothing else taken out of it;
   - the chips: a click keeps the range and refreshes for it, the arrows move the choice and the focus, wrapping;
   - hold: a new range's answer for the same cards keeps every card and frame in place and only moves their srcs and
     links; a first answer for the old range (a chip chosen while it was on its way) fetches again, only while the panel
     is still on that id;
   - stale: the farm document moving repaints the header (its stale line comes and goes), never while another kind is open;
   - failed: a refresh that fails keeps the charts and says it could not change the range, with Try again; a first load
     that fails keeps the header with the panel's own words (403) or the host's with Try again, and the chips go; no charts
     says so;
   - no chart for a chip (gd-TGYiliW9OR): with no cards, or Grafana not set up, the chips go and one line says why;
   - no cost row (gd-vViKmjtrTj): a project the route charts but the farm has no row for says the garden has recorded no
     spend for it, never a figure;
   - closed: the observer goes and the farm document no longer repaints anything.
   The kind is no live panel: it registers no live, hold or stale hook, so the host never holds it. Prints a JSON report
   whose last line the Go test reads. */
"use strict";
var path = require("path"), assert = require("assert");
var report = {};

/* ---------- a small DOM ---------- */
var active = null;
function El(tag) {
  this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.on = {}; this.hidden = false; this.className = "";
  this._t = ""; this.id = ""; this.dataset = {}; this.parentNode = null; this.disabled = false;
  var self = this;
  this.classList = {
    add: function (c) { if (!self.classList.contains(c)) self.className = (self.className + " " + c).trim(); },
    remove: function (c) { self.className = self.className.split(/\s+/).filter(function (x) { return x !== c; }).join(" "); },
    contains: function (c) { return self.className.split(/\s+/).indexOf(c) >= 0; }
  };
}
Object.defineProperty(El.prototype, "textContent", {
  get: function () { return this._t + this.children.map(function (c) { return c.textContent; }).join(""); },
  set: function (v) { this._t = String(v); this.children.forEach(function (c) { c.parentNode = null; }); this.children = []; }
});
Object.defineProperty(El.prototype, "ownerDocument", { get: function () { return doc; } });
El.prototype.appendChild = function (c) { if (c.parentNode) c.parentNode.removeChild(c); this.children.push(c); c.parentNode = this; return c; };
El.prototype.removeChild = function (c) { this.children = this.children.filter(function (x) { return x !== c; }); c.parentNode = null; return c; };
El.prototype.replaceWith = function (n) {
  var p = this.parentNode, i = p.children.indexOf(this);
  if (n.parentNode) n.parentNode.removeChild(n);
  p.children[i] = n; n.parentNode = p; this.parentNode = null;
};
El.prototype.setAttribute = function (k, v) { this.attrs[k] = String(v); };
El.prototype.getAttribute = function (k) { return k in this.attrs ? this.attrs[k] : null; };
El.prototype.addEventListener = function (n, f) { this.on[n] = f; };
El.prototype.fire = function (n) { if (this.on[n]) this.on[n]({}); };
El.prototype.focus = function () { active = this; };
/* the one selector costs.js asks for: [data-range="x"] */
El.prototype.querySelector = function (sel) {
  var m = /^\[data-range="([^"]*)"\]$/.exec(sel);
  assert.ok(m, "an unexpected selector " + sel);
  return all(this, function (e) { return e !== this && e.dataset && e.dataset.range === m[1]; })[0] || null;
};
function all(e, pred, out) { out = out || []; if (pred(e)) out.push(e); e.children.forEach(function (c) { all(c, pred, out); }); return out; }
function byClass(e, c) { return all(e, function (x) { return x.classList && x.classList.contains(c); }); }
function byTag(e, t) { return all(e, function (x) { return x.tagName === t; }); }
function key(e) { var x = { key: e, prevented: false, preventDefault: function () { x.prevented = true; } }; return x; }

var titleEl = new El("h2"), kindEl = new El("span"), bodyEl = new El("div");
titleEl.id = "bv-title"; kindEl.id = "bv-kind"; bodyEl.id = "bv-body";
var docOn = {};
var doc = {
  createElement: function (t) { return new El(t); },
  createTextNode: function (t) { var n = new El("#text"); n._t = String(t); return n; },
  getElementById: function (id) { return { "bv-title": titleEl, "bv-kind": kindEl, "bv-body": bodyEl }[id] || null; },
  body: new El("body"), // no data-costs and none of the openers (step 2.3's Menu item and purse chip): the kind alone
  addEventListener: function (n, f) { docOn[n] = f; }
};
global.document = doc;
function farmdoc(detail) { docOn.farmdoc({ detail: detail }); }

var timers = [], tid = 0;
global.setTimeout = function (f, ms) { timers.push({ id: ++tid, f: f, ms: ms || 0 }); return tid; };
global.clearTimeout = function (id) { timers = timers.filter(function (t) { return t.id !== id; }); };
function runDue(ms) { var due = timers.filter(function (t) { return t.ms <= ms; }); timers = timers.filter(function (t) { return t.ms > ms; }); due.forEach(function (t) { t.f(); }); }

var observers = [];
global.IntersectionObserver = function (cb, o) { this.cb = cb; this.o = o; this.watch = []; this.gone = false; observers.push(this); };
IntersectionObserver.prototype.observe = function (e) { this.watch.push(e); };
IntersectionObserver.prototype.unobserve = function (e) { this.watch = this.watch.filter(function (x) { return x !== e; }); };
IntersectionObserver.prototype.disconnect = function () { this.watch = []; this.gone = true; };
IntersectionObserver.prototype.show = function (e) { this.cb([{ target: e, isIntersecting: true }]); };

var stored = { "garden.costs.range": "30d" }, kept = [];
var farmCost = { projects: [
  { project: "gardenio", has: true, has_budget: true, budget_usd: 500, period: "month", today_usd: 12.5, week_usd: 80, spent_usd: 300, remaining_usd: 200 },
  { project: "music", has: false, has_budget: false, today_usd: null, week_usd: null, as_of: 1792960200 }
] };
global.window = {
  localStorage: { getItem: function (k) { return k in stored ? stored[k] : null; }, setItem: function (k, v) { stored[k] = v; kept.push(v); } },
  GARDEN: { cost: function () { return farmCost; } }
};

var R = {}, refreshes = [], current = null;
global.WORKBENCH = {
  register: function (k, r) { R[k] = r; },
  current: function () { return current; },
  refresh: function (k, id) { refreshes.push(k + ":" + id); },
  refreshFailure: function (text, noun, words) { return String(text).replace(/^Could not load [^:]*?(?=: | \(|\.$)/, words || "Could not refresh this " + noun); }   // web/workbench.js's
};

var C = require(path.join(process.argv[2], "costs.js"));
var r = R.costs;

/* ---------- the registration ---------- */
assert.ok(r, "costs.js registers the costs kind");
assert.strictEqual(r.noun, "costs");
assert.strictEqual(r.failure, C.failure, "the panel's own words for a failed load");
assert.strictEqual(r.url("fleet"), "/garden/charts/costs-fleet?range=30d&theme=dark", "the stored range is the first one asked for");
assert.strictEqual(r.url("gardenio"), "/garden/charts/costs-project?project=gardenio&range=30d&theme=dark");
["live", "hold", "stale"].forEach(function (k) { assert.strictEqual(r[k], undefined, "no live panel: no " + k + " hook"); });
report.registered = true;

/* ---------- paint ---------- */
function panels(range, n) {
  var out = [];
  for (var id = 1; id <= (n || 3); id++) {
    out.push({ id: id, title: "Panel " + id, scope: "USD · " + range, note: "", src: "https://grafana.example/d-solo/u?panelId=" + id + "&range=" + range,
      open_url: "https://grafana.example/d/u?viewPanel=" + id + "&range=" + range, embed_html: "<iframe>" });
  }
  return out;
}
function answer(range, n) { return { set: "costs-fleet", project: "", range: range, theme: "dark", open_dashboard_url: "https://grafana.example/d/u?range=" + range, panels: panels(range, n) }; }
/* the panel's own line, under the chips (the header's stale line is a bv-note too) */
function line() { return bodyEl.children[2]; }
function parts() { return bodyEl.children.map(function (c) { return c.className; }); }

current = { kind: "costs", id: "fleet" };
r.paint("fleet", null, "Loading…");   // the host's loading paint
assert.strictEqual(kindEl.textContent, "costs");
assert.strictEqual(titleEl.textContent, "Costs: Fleet");
assert.deepStrictEqual(parts(), ["gp-figs", "qv-scope gp-chips", "bv-note", "gp-cards", "gp-links"], "the loading paint: header, chips, the line, no cards yet");
assert.strictEqual(line().textContent, "Loading…");
assert.strictEqual(byTag(bodyEl, "SECTION").length, 0);
assert.strictEqual(observers.length, 0, "nothing to watch while loading");

r.paint("fleet", answer("30d"), "");
assert.deepStrictEqual(parts(), ["gp-figs", "qv-scope gp-chips", "bv-note", "gp-cards", "gp-links"]);
var figs = bodyEl.children[0];
assert.deepStrictEqual(byTag(figs, "DT").map(function (e) { return e.textContent; }), ["Today", "This week", "Projects"]);
assert.deepStrictEqual(byTag(figs, "DD").map(function (e) { return e.textContent; }), ["$12.50", "$80.00", "2"], "the fleet sums the known figures");
assert.ok(/did not answer for 1 of 2 projects/.test(byClass(figs, "bv-note")[0].textContent), "the farm's stale line rides with the figures");
var chips = bodyEl.children[1];
assert.strictEqual(chips.getAttribute("role"), "radiogroup");
assert.strictEqual(chips.getAttribute("aria-label"), "Time range");
assert.deepStrictEqual(chips.children.map(function (b) { return [b.textContent, b.getAttribute("role"), b.getAttribute("aria-checked"), b.tabIndex]; }),
  [["Today", "radio", "false", -1], ["7 days", "radio", "false", -1], ["30 days", "radio", "true", 0]], "the stored range is checked and the one tab stop");
chips.children.forEach(function (b) { assert.strictEqual(b.type, "button", "a chip submits nothing"); });
var cards = byTag(bodyEl, "SECTION"), frames = byTag(bodyEl, "IFRAME");
assert.strictEqual(cards.length, 3);
assert.deepStrictEqual(frames.map(function (f) { return f.getAttribute("title"); }), ["Panel 1 Grafana panel", "Panel 2 Grafana panel", "Panel 3 Grafana panel"], "each frame is titled by its card");
cards.forEach(function (s) { var h = byTag(s, "H3")[0]; assert.strictEqual(s.getAttribute("aria-labelledby"), h.id, "a card is labelled by its title"); });
assert.strictEqual(observers.length, 1);
var io = observers[0];
assert.strictEqual(io.o.root, bodyEl, "the lazy load watches the panel body");
var links = bodyEl.children[4], dash = byClass(links, "gp-dash")[0], foot = byClass(links, "gp-foot")[0];
assert.strictEqual(dash.textContent, "Open the dashboard in Grafana");
[dash, byTag(foot, "A")[0]].forEach(function (a) { assert.ok(/(^| )ext( |$)/.test(a.className), "a new-tab link draws its arrow as .ext's mask, not U+2197"); });
assert.strictEqual(dash.href, "https://grafana.example/d/u?range=30d");
assert.strictEqual(foot.textContent, "Charts blank? Open Grafana once to sign in");
[dash, byTag(foot, "A")[0]].forEach(function (a) { assert.strictEqual(a.target, "_blank"); assert.strictEqual(a.rel, "noopener"); });

/* keyboard focus, in document order: what a Tab walk reaches (a button or a link with an href, a frame) and is not
   taken out of the order with a negative tabIndex */
function tabbable(e) {
  if (e.hidden) return false;
  var t = e.tagName, natural = t === "BUTTON" || t === "IFRAME" || (t === "A" && !!e.href);
  return natural && !e.disabled && !(typeof e.tabIndex === "number" && e.tabIndex < 0);
}
function visible(e, out) { out = out || []; if (e.hidden) return out; out.push(e); e.children.forEach(function (c) { visible(c, out); }); return out; }
function focusOrder() {
  return visible(bodyEl).filter(tabbable).map(function (e) {
    return e.tagName === "BUTTON" ? (e.dataset.range ? "chip " + e.dataset.range : e.textContent) : e.tagName === "IFRAME" ? "frame " + e.getAttribute("title") : e.getAttribute("aria-label") || e.textContent;
  });
}
var order = ["chip 30d",
  "Open Panel 1 in Grafana", "frame Panel 1 Grafana panel",
  "Open Panel 2 in Grafana", "frame Panel 2 Grafana panel",
  "Open Panel 3 in Grafana", "frame Panel 3 Grafana panel",
  "Open the dashboard in Grafana", "Open Grafana once to sign in"];
assert.deepStrictEqual(focusOrder(), order, "chips, then each card's Open link and its frame, then the dashboard and sign-in links");
all(bodyEl, function (e) { return e.tagName === "A" || e.tagName === "IFRAME"; }).forEach(function (e) {
  assert.ok(e.tabIndex === undefined || e.tabIndex >= 0, "no link or frame is taken out of the tab order");
});
io.show(cards[1]);
assert.deepStrictEqual(focusOrder(), order, "a frame coming into view changes nothing in the order");
report.paint = true;

/* ---------- the chips ---------- */
chips.children[1].onclick();
assert.deepStrictEqual(kept, ["7d"], "the chosen range is kept per viewer");
assert.deepStrictEqual(refreshes, ["costs:fleet"], "and fetched");
assert.strictEqual(r.url("fleet"), "/garden/charts/costs-fleet?range=7d&theme=dark");
assert.deepStrictEqual(chips.children.map(function (b) { return [b.getAttribute("aria-checked"), b.tabIndex]; }), [["false", -1], ["true", 0], ["false", -1]]);
chips.children[1].onclick();
assert.strictEqual(refreshes.length, 1, "the range already shown fetches nothing");
var k = key("ArrowRight");
chips.children[1].onkeydown(k);
assert.ok(k.prevented);
assert.strictEqual(active, chips.children[2], "the focus moves with the choice");
assert.strictEqual(stored["garden.costs.range"], "30d");
chips.children[2].onkeydown(key("ArrowDown"));
assert.strictEqual(active, chips.children[0], "and wraps past the last chip");
chips.children[0].onkeydown(key("ArrowUp"));
assert.strictEqual(active, chips.children[2], "and back past the first");
chips.children[2].onkeydown(key("ArrowLeft"));
assert.strictEqual(active, chips.children[1]);
var tab = key("Tab");
chips.children[1].onkeydown(tab);
assert.ok(!tab.prevented, "Tab leaves the group as ever");
assert.strictEqual(stored["garden.costs.range"], "7d");
assert.deepStrictEqual(refreshes, ["costs:fleet", "costs:fleet", "costs:fleet", "costs:fleet", "costs:fleet"]);
assert.deepStrictEqual(focusOrder()[0], "chip 7d", "the checked chip is the group's one tab stop");
report.chips = true;

/* ---------- hold: the new range's answer keeps the cards in place ---------- */
var f1 = frames[1];
assert.strictEqual(f1.getAttribute("src"), "https://grafana.example/d-solo/u?panelId=2&range=30d", "the frame in view started");
f1.fire("load");
r.paint("fleet", answer("7d"), "");
assert.strictEqual(observers.length, 1, "no new observer: the stack is kept");
assert.ok(!io.gone);
assert.deepStrictEqual(byTag(bodyEl, "SECTION"), cards, "the same cards");
assert.deepStrictEqual(byTag(bodyEl, "IFRAME"), frames, "the same frames");
assert.strictEqual(f1.getAttribute("src"), "https://grafana.example/d-solo/u?panelId=2&range=7d", "a started frame reloads in place");
assert.strictEqual(frames[0].getAttribute("src"), null, "one not yet in view still waits");
assert.strictEqual(byClass(cards[0], "gp-open")[0].href, "https://grafana.example/d/u?viewPanel=1&range=7d", "the Open links move");
assert.strictEqual(byClass(bodyEl, "gp-dash")[0].href, "https://grafana.example/d/u?range=7d", "and the dashboard link");
assert.strictEqual(bodyEl.children[0], figs, "the header stays");
assert.strictEqual(line().textContent, "");
assert.deepStrictEqual(focusOrder(), ["chip 7d"].concat(order.slice(1)), "the order holds across the range change");

/* a first answer for the old range: fetch the new one, while the panel is still on that id */
var n = refreshes.length;
r.landed("fleet", { range: "7d" });
assert.strictEqual(timers.filter(function (t) { return t.ms === 0; }).length, 0, "the range asked for: nothing to do");
r.landed("fleet", { range: "30d" });
runDue(0);
assert.strictEqual(refreshes.length, n + 1, "an answer for the old range fetches again");
current = { kind: "costs", id: "gardenio" };
r.landed("fleet", { range: "30d" });
runDue(0);
assert.strictEqual(refreshes.length, n + 1, "not once the panel moved on");
current = { kind: "costs", id: "fleet" };
report.hold = true;

/* ---------- stale: the farm document moves ---------- */
farmdoc({ cost: { projects: [{ project: "gardenio", has: true, today_usd: 20, week_usd: 90 }, { project: "music", has: true, today_usd: 1, week_usd: 2 }] } });
var figs2 = bodyEl.children[0];
assert.notStrictEqual(figs2, figs, "the header is painted again");
assert.deepStrictEqual(byTag(figs2, "DD").map(function (e) { return e.textContent; }), ["$21.00", "$92.00", "2"]);
assert.strictEqual(byClass(figs2, "bv-note").length, 0, "the rollup answered: no stale line");
farmdoc({ cost: { projects: [{ project: "gardenio", has: false, as_of: 1792960200 }] } });
assert.ok(/^The costs rollup did not answer for 1 of 1 projects; these are the figures from \d\d:\d\d UTC\.$/.test(byClass(bodyEl.children[0], "bv-note")[0].textContent), "it went down: the line comes back with its as of");
assert.deepStrictEqual(byTag(bodyEl.children[0], "DD").slice(0, 2).map(function (e) { return e.textContent; }), ["—", "—"], "unknown is a dash, never $0");
farmdoc({ cost: null });
assert.strictEqual(bodyEl.children[0].children.length, 0, "no spend source: no figures");
var shown = bodyEl.children[0];
current = { kind: "plan", id: "gd-p" };
farmdoc({ cost: farmCost });
assert.strictEqual(bodyEl.children[0], shown, "another kind open: the header is left alone");
current = { kind: "costs", id: "fleet" };
farmdoc({ cost: farmCost });
assert.deepStrictEqual(byTag(bodyEl.children[0], "DD").map(function (e) { return e.textContent; }), ["$12.50", "$80.00", "2"]);
assert.deepStrictEqual(byTag(bodyEl, "SECTION"), cards, "the cards never move with the figures");
report.stale = true;

/* ---------- failed ---------- */
/* a refresh that fails keeps the charts and says so over them, with Try again */
r.failed("fleet", "Could not load the costs: the beads daemon did not answer (502).", true);
var note = line();
assert.ok(note.classList.contains("err"));
assert.strictEqual(note.getAttribute("role"), "status", "said, not only shown");
assert.ok(/^Could not change the range: the beads daemon did not answer \(502\)\. Try again$/.test(note.textContent), note.textContent);
assert.deepStrictEqual(byTag(bodyEl, "SECTION"), cards, "the charts stay");
var retry = byClass(note, "bv-retry")[0];
assert.strictEqual(retry.type, "button");
n = refreshes.length;
retry.onclick();
assert.ok(retry.disabled);
assert.strictEqual(retry.textContent, "Loading…");
assert.strictEqual(refreshes.length, n + 1);
r.failed("fleet", "Could not load the costs (500).", false);
note = line();
assert.strictEqual(note.textContent, "Could not change the range (500).");
assert.strictEqual(byClass(note, "bv-retry").length, 0, "no second try offered when none would pass");
assert.deepStrictEqual(focusOrder(), ["chip 7d"].concat(order.slice(1)), "a failed refresh leaves the focus order whole");

/* a first load that fails: the header stays, the panel's words (403), no charts, and the chips go */
current = { kind: "costs", id: "gardenio" };
r.paint("gardenio", null, "Loading…");
assert.ok(io.gone, "another id: the old stack lets its observer go");
var f403 = C.failure(403, "gardenio");
r.paint("gardenio", null, f403.text, true, f403.retry);
assert.strictEqual(titleEl.textContent, "Costs: gardenio");
assert.deepStrictEqual(parts(), ["gp-figs", "qv-scope gp-chips", "bv-note err", "gp-cards", "gp-links"]);
assert.deepStrictEqual(byTag(bodyEl.children[0], "DD").map(function (e) { return e.textContent; }), ["$12.50", "$80.00", "$500.00 a month, $200.00 left"], "the farm's figures paint without the charts");
assert.strictEqual(bodyEl.children[2].textContent, "Costs charts are for operators for now.");
assert.strictEqual(byClass(bodyEl, "bv-retry").length, 0);
assert.strictEqual(byTag(bodyEl, "IFRAME").length, 0);
assert.strictEqual(bodyEl.children[4].children.length, 0, "no dashboard link without an answer");
assert.ok(bodyEl.children[1].hidden, "a failed load: no range to choose");
assert.deepStrictEqual(focusOrder(), [], "nothing is left to reach");
/* the host's words, with Try again */
r.paint("gardenio", null, "Could not load gardenio: the garden did not answer.", true, true);
assert.strictEqual(byClass(bodyEl.children[2], "bv-retry").length, 1);
assert.deepStrictEqual(focusOrder(), ["Try again"]);
/* the loading paint keeps the chips, so they do not jump in when the answer lands */
r.paint("gardenio", null, "Loading…");
assert.ok(!bodyEl.children[1].hidden, "loading: the chips stay");
/* no charts for the scope: no chips, one line says why */
r.paint("music", { set: "costs-project", project: "music", range: "7d", panels: [] }, "");
assert.strictEqual(byClass(bodyEl, "bv-empty")[0].textContent, "There are no charts for music.");
assert.ok(/did not answer; these are the figures from/.test(byClass(bodyEl.children[0], "bv-note")[0].textContent), "music's figures are the farm's stale ones");
assert.deepStrictEqual(parts(), ["gp-figs", "qv-scope gp-chips", "bv-note gp-off", "bv-note", "gp-cards", "gp-links"]);
assert.ok(bodyEl.children[1].hidden);
assert.strictEqual(bodyEl.children[2].textContent, "No range to choose: there are no charts here.");
/* Grafana not set up: the cards keep their words, the chips go */
var unset = answer("7d"); unset.panels = unset.panels.map(function (p) { return Object.assign({}, p, { src: "", open_url: "" }); });
r.paint("fleet", unset, "");
assert.ok(bodyEl.children[1].hidden);
assert.strictEqual(byClass(bodyEl, "gp-off")[0].textContent, "No range to choose: Grafana isn’t set up here.");
assert.strictEqual(byTag(bodyEl, "SECTION").length, unset.panels.length, "the cards stay, as words");
assert.ok(focusOrder().every(function (f) { return !/^chip /.test(f); }), "no chip is reachable");
/* a project the route charts that the farm has no cost row for: the header says so, never $0 */
r.paint("zeta", answer("7d"), "");
assert.ok(!bodyEl.children[1].hidden, "charts: the chips are back");
assert.strictEqual(byClass(bodyEl, "gp-off").length, 0);
assert.strictEqual(byTag(bodyEl.children[0], "DD").length, 0);
assert.strictEqual(bodyEl.children[0].textContent, "The garden has recorded no spend for zeta yet.");
/* a project there is none of (400): only the failure line, not "no spend yet" over it (gd-weOKYDBRGQ) */
var f400 = C.failure(400, "zeta");
r.paint("zeta", null, f400.text, true, f400.retry);
assert.strictEqual(bodyEl.children[0].textContent, "", "no figures line over a failed load");
assert.strictEqual(bodyEl.children[2].textContent, "There is no project zeta to chart.");
farmdoc({ cost: farmCost });
assert.strictEqual(bodyEl.children[0].textContent, "", "nor once the farm moves");
report.failed = true;

/* ---------- closed ---------- */
r.paint("fleet", answer("7d"), "");
var io2 = observers[observers.length - 1];
assert.ok(!io2.gone);
r.closed();
assert.ok(io2.gone, "closing lets the observer go");
var before = bodyEl.children[0];
farmdoc({ cost: farmCost });
assert.strictEqual(bodyEl.children[0], before, "a closed panel paints nothing");
report.closed = true;

console.log(JSON.stringify(report));
