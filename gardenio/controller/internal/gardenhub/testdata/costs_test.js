/* The Costs panel's pure part and its panel-card stack (plan gd-gh5J0vVbJP step 2.1; garden-grafana-panels.md), run by
   TestCostsUnderNode:  node costs_test.js <web dir>
   COSTS: the set and route an id names (the fleet, or one project, encoded), the range chips and the one kept per viewer
   (a blocked or junk store is 7 days), dollars (an unknown figure is a dash), the header's figures from the farm's
   cost.projects[] (one project's, the fleet's sums over the known figures, the stale line with the oldest as_of), the
   panel's own words for a refusal and for a project it cannot chart, the frame box's words. Then paintCostCards under a
   DOM just wide enough: every card's title, scope, note and Open link, an iframe built without src with its title,
   sandbox and referrer policy; the IntersectionObserver rooted on the panel body with the 200 px margin, a card's src set
   only once it is in view and never unset; the 15 s slow line with its Open link and a late load clearing it; a range
   change reloading a started frame and only re-aiming one not yet in view; other cards refused; no Grafana base (no
   src): the words, no frame, no Open link; and no IntersectionObserver: every frame starts at once. The openers (step
   2.3): the Menu item's scope; the header purse's rows (gd-H9LerJ0v3g): Fleet for an admin's document, one row per
   project of a scoped cut, a dash and the as_of time when stale, its level's word. Prints a JSON report
   whose last line the Go test reads. */
"use strict";
var path = require("path"), assert = require("assert");
var C = require(path.join(process.argv[2], "costs.js"));
var report = {};

/* ---------- 1. pure ---------- */
assert.strictEqual(C.setOf("fleet"), "costs-fleet");
assert.strictEqual(C.setOf("gardenio"), "costs-project");
assert.strictEqual(C.chartsURL("fleet", "today"), "/garden/charts/costs-fleet?range=today&theme=dark", "the fleet takes no project");
assert.strictEqual(C.chartsURL("gardenio", "30d"), "/garden/charts/costs-project?project=gardenio&range=30d&theme=dark");
assert.strictEqual(C.chartsURL("a b&c", "nope"), "/garden/charts/costs-project?project=a%20b%26c&range=7d&theme=dark", "encoded; a range outside the chips is 7 days");
assert.deepStrictEqual(C.RANGES.map(function (r) { return r[0]; }), ["today", "7d", "30d"]);
assert.strictEqual(C.DEFAULT_RANGE, "7d");
assert.strictEqual(C.rangeWord("30d"), "30 days");
assert.strictEqual(C.scopeName("fleet"), "Fleet");
assert.strictEqual(C.scopeName("gardenio"), "gardenio");
/* one gardener's charts (step 4.2): agent:<name> is the agent set, its name the scope, no farm figures, its own 400 */
assert.strictEqual(C.agentOf("agent:gardenio-run-1"), "gardenio-run-1");
assert.strictEqual(C.agentOf("gardenio"), "");
assert.strictEqual(C.setOf("agent:gardenio-run-1"), "agent");
assert.strictEqual(C.scopeName("agent:gardenio-run-1"), "gardenio-run-1");
assert.strictEqual(C.chartsURL("agent:gardenio-run-1", "today"), "/garden/charts/agent?agent=gardenio-run-1&range=today&theme=dark");
assert.strictEqual(C.chartsURL("agent:a&b", "7d"), "/garden/charts/agent?agent=a%26b&range=7d&theme=dark", "encoded");
assert.strictEqual(C.figures({ projects: [{ project: "agent:x", has: true, today_usd: 1 }] }, "agent:x"), null, "an agent has no farm figures");
assert.strictEqual(C.unrecorded({ projects: [] }, "agent:x"), "", "nor a no-spend line");
assert.deepStrictEqual(C.failure(400, "agent:x"), { text: "There is no gardener x to chart.", retry: false });
function Store(v) { this.v = v; }
Store.prototype.getItem = function (k) { assert.strictEqual(k, "garden.costs.range"); return this.v; };
Store.prototype.setItem = function (k, v) { assert.strictEqual(k, "garden.costs.range"); this.v = v; };
var blocked = { getItem: function () { throw new Error("SecurityError"); }, setItem: function () { throw new Error("SecurityError"); } };
assert.strictEqual(C.readRange(null), "7d", "no store");
assert.strictEqual(C.readRange(blocked), "7d", "a blocked store");
assert.strictEqual(C.readRange(new Store("30d")), "30d");
assert.strictEqual(C.readRange(new Store("1y")), "7d", "a value outside the chips");
var st = new Store(null);
C.keepRange(st, "today"); assert.strictEqual(st.v, "today");
C.keepRange(st, "1y"); assert.strictEqual(st.v, "today", "only a chip is kept");
C.keepRange(blocked, "30d");   // does not throw
assert.strictEqual(C.usd(3.456), "$3.46");
assert.strictEqual(C.usd(0), "$0.00");
assert.strictEqual(C.usd(null), "—", "unknown is a dash, never $0");

var cost = { projects: [
  { project: "gardenio", has: true, has_budget: true, budget_usd: 500, period: "month", today_usd: 12.5, week_usd: 80, spent_usd: 300, remaining_usd: 200 },
  { project: "music", has: true, has_budget: false, budget_usd: null, today_usd: 1, week_usd: 4 },
  { project: "lands-a", has: false, has_budget: false, today_usd: null, week_usd: null, as_of: 1792960200 },
  { project: "lands-b", has: false, has_budget: false, today_usd: null, week_usd: null, as_of: 1792963800 }
] };
assert.deepStrictEqual(C.figures(cost, "gardenio"), { rows: [["Today", "$12.50"], ["This week", "$80.00"], ["Budget", "$500.00 a month, $200.00 left"]], stale: "" });
assert.deepStrictEqual(C.figures(cost, "music").rows, [["Today", "$1.00"], ["This week", "$4.00"]], "no budget: no budget row");
assert.deepStrictEqual(C.figures({ projects: [{ project: "x", has_budget: true, budget_usd: 50, period: "7d", remaining_usd: 0, today_usd: 0, week_usd: 60, has: true }] }, "x").rows[2], ["Budget", "$50.00 a rolling week, $0.00 left"]);
var la = C.figures(cost, "lands-a");
assert.deepStrictEqual(la.rows.slice(0, 2), [["Today", "—"], ["This week", "—"]]);
assert.strictEqual(la.stale, "The costs rollup did not answer; these are the figures from " + new Date(1792960200 * 1000).toISOString().slice(11, 16) + " UTC.");
var fl = C.figures(cost, "fleet");
assert.deepStrictEqual(fl.rows, [["Today", "$13.50"], ["This week", "$84.00"], ["Projects", "4"]], "the fleet sums the known figures");
assert.ok(/did not answer for 2 of 4 projects/.test(fl.stale) && fl.stale.indexOf(new Date(1792960200 * 1000).toISOString().slice(11, 16)) >= 0, "the oldest as_of: " + fl.stale);
assert.deepStrictEqual(C.figures({ projects: [{ project: "a", has: false }] }, "fleet").rows.slice(0, 2), [["Today", "—"], ["This week", "—"]], "nothing known is a dash, not $0");
assert.strictEqual(C.figures({ projects: [{ project: "a", has: false }] }, "a").stale, "The costs rollup did not answer.", "never read well: no as_of");
assert.strictEqual(C.figures(cost, "nobody"), null);
assert.strictEqual(C.figures(null, "fleet"), null, "no spend source");
assert.deepStrictEqual(C.failure(403, "fleet"), { text: "Costs charts are for operators for now.", retry: false });
assert.deepStrictEqual(C.failure(400, "Bad Name"), { text: "There is no project Bad Name to chart.", retry: false });
assert.strictEqual(C.failure(400, "fleet"), null, "the host's words");
assert.strictEqual(C.failure(502, "gardenio"), null);
assert.strictEqual(C.boxWord({ src: "" }, false), "Grafana isn’t set up in this environment.");
assert.strictEqual(C.boxWord({ src: "x" }, false), "Loading chart…");
assert.strictEqual(C.boxWord({ src: "x" }, true), "This chart is taking a while.");
assert.ok(C.sameCards([{ id: 1 }, { id: 2 }], [{ id: 1 }, { id: 2 }]));
assert.ok(!C.sameCards([{ id: 1 }, { id: 2 }], [{ id: 2 }, { id: 1 }]));
assert.strictEqual(C.SLOW_MS, 15000);
assert.strictEqual(C.MARGIN, "0px 0px 200px 0px");
report.pure = true;

/* the openers (step 2.3): the Menu's Costs item opens the cursor's project, the fleet's off every project */
assert.strictEqual(C.scopeOf("gardenio"), "gardenio");
assert.strictEqual(C.scopeOf(""), "fleet", "the Commons and every project: the fleet");
/* the header purse (gd-H9LerJ0v3g): an admin's document is one Fleet row of $ today and $ remaining with its level */
var at = Date.UTC(2026, 9, 31, 20, 10) / 1000;
assert.deepStrictEqual(C.purseRows({ scope: "fleet", has: true, today_usd: 13.5, has_budget: true, remaining_usd: 86.5, level: "ok", projects: [] }),
  [{ name: "Fleet", today: "$13.50", left: "$86.50", level: "ok", stale: "" }]);
assert.deepStrictEqual(C.purseRows({ scope: "fleet", has: true, today_usd: 0, projects: [] }),
  [{ name: "Fleet", today: "$0.00", left: "—", level: "", stale: "" }], "no account budget: a dash for $ remaining, never $0");
assert.deepStrictEqual(C.purseRows({ scope: "fleet", as_of: at, has_budget: true, remaining_usd: 4, level: "warn", projects: [] }),
  [{ name: "Fleet", today: "—", left: "$4.00", level: "warn", stale: "as of 20:10 UTC" }], "the rollup down: a dash and the as_of, the budget kept");
assert.strictEqual(C.purseRows({ scope: "fleet", projects: [] })[0].stale, "not read yet");
/* a scoped cut: one row per project, never a sum; $ remaining hidden only with no budget; a tenant land's row has none here */
var cut = { scope: "project", has: true, today_usd: 3.3, projects: [
  { project: "alpha", has: true, has_budget: false, budget_usd: null, today_usd: 1.1, remaining_usd: null },
  { project: "beta", has: true, has_budget: true, today_usd: 2.2, remaining_usd: 0, level: "over" },
  { project: "gamma", has: false, has_budget: true, today_usd: null, remaining_usd: null, as_of: at },
  { project: "zeta", has: true, has_budget: false, purse: { state: "wall", remaining_credits: 1, granted_credits: 50 } }] };   // ForProjects' credits-only cut
assert.deepStrictEqual(C.purseRows(cut), [
  { name: "alpha", today: "$1.10", left: null, level: "", stale: "" },
  { name: "beta", today: "$2.20", left: "$0.00", level: "over", stale: "" },
  { name: "gamma", today: "—", left: "—", level: "", stale: "as of 20:10 UTC" }]);
assert.deepStrictEqual(C.purseRows({ projects: [{ project: "a", has: true }] }), [], "no scope: no purse");
assert.deepStrictEqual(C.purseRows({ projects: [] , scope: "project" }), []);
assert.deepStrictEqual(C.purseRows(null), [], "no spend source");
assert.strictEqual(C.levelWord("warn"), "low");
assert.strictEqual(C.levelWord("over"), "over budget");
assert.strictEqual(C.levelWord("ok"), "");
assert.strictEqual(C.purseSay(C.purseRows(cut)[1]), "beta: $2.20 today, $0.00 left, over budget");
assert.strictEqual(C.purseSay(C.purseRows(cut)[0]), "alpha: $1.10 today", "no budget: no $ remaining");
assert.strictEqual(C.purseSay(C.purseRows(cut)[2]), "gamma: — today (as of 20:10 UTC), — left");
assert.strictEqual(C.purseSay({ name: "zeta", credits: "1.00", left: null }), "zeta: 1.00 credits left", "a tenant land's credits row (paintPurse adds it from landHere)");
report.openers = true;

/* ---------- 2. the card stack under a small DOM ---------- */
function El(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.on = {}; this.hidden = false; this.className = ""; this._t = ""; this.id = ""; this.ownerDocument = doc; var self = this; this.classList = { add: function (c) { if (!self.classList.contains(c)) self.className = (self.className + " " + c).trim(); }, remove: function (c) { self.className = self.className.split(/\s+/).filter(function (x) { return x !== c; }).join(" "); }, contains: function (c) { return self.className.split(/\s+/).indexOf(c) >= 0; } }; }
Object.defineProperty(El.prototype, "textContent", { get: function () { return this._t + this.children.map(function (c) { return c.textContent; }).join(""); }, set: function (v) { this._t = String(v); this.children = []; } });
El.prototype.appendChild = function (c) { this.children.push(c); c.parentNode = this; return c; };
El.prototype.setAttribute = function (k, v) { this.attrs[k] = String(v); };
El.prototype.getAttribute = function (k) { return k in this.attrs ? this.attrs[k] : null; };
El.prototype.addEventListener = function (n, f) { this.on[n] = f; };
El.prototype.fire = function (n) { if (this.on[n]) this.on[n]({}); };
function all(e, pred, out) { out = out || []; if (pred(e)) out.push(e); e.children.forEach(function (c) { all(c, pred, out); }); return out; }
function byClass(e, c) { return all(e, function (x) { return x.classList && x.classList.contains(c); }); }
var body = new El("div"); body.id = "bv-body";
var doc = {
  createElement: function (t) { return new El(t); },
  createTextNode: function (t) { var n = new El("#text"); n._t = String(t); n.classList.contains = function () { return false; }; return n; },
  getElementById: function (id) { return id === "bv-body" ? body : null; }
};
body.ownerDocument = doc;
global.document = doc;
var timers = [], tid = 0;
global.setTimeout = function (f, ms) { timers.push({ id: ++tid, f: f, ms: ms }); return tid; };
global.clearTimeout = function (id) { timers = timers.filter(function (t) { return t.id !== id; }); };
function tick(ms) { var due = timers.filter(function (t) { return t.ms <= ms; }); timers = timers.filter(function (t) { return t.ms > ms; }); due.forEach(function (t) { t.f(); }); }
var observers = [];
global.IntersectionObserver = function (cb, o) { this.cb = cb; this.o = o; this.watch = []; this.gone = false; observers.push(this); };
IntersectionObserver.prototype.observe = function (e) { this.watch.push(e); };
IntersectionObserver.prototype.unobserve = function (e) { this.watch = this.watch.filter(function (x) { return x !== e; }); };
IntersectionObserver.prototype.disconnect = function () { this.watch = []; this.gone = true; };
IntersectionObserver.prototype.show = function (e) { this.cb([{ target: e, isIntersecting: true }]); };

function panels(range) {
  return [1, 2, 3].map(function (id) {
    return { id: id, title: "Panel " + id, scope: "USD · " + range, note: id === 2 ? "" : "note " + id,
      src: "https://grafana.example/d-solo/u?panelId=" + id + "&range=" + range, open_url: "https://grafana.example/d/u?viewPanel=" + id + "&range=" + range, embed_html: "<iframe>" };
  });
}
var host = new El("div");
host.appendChild(new El("p"));   // painted over
var stack = C.paintCostCards(host, panels("7d"));
var cards = host.children, frames = all(host, function (e) { return e.tagName === "IFRAME"; });
assert.strictEqual(cards.length, 3, "one card per panel, the host emptied first");
assert.strictEqual(frames.length, 3);
var c0 = cards[0];
assert.strictEqual(c0.tagName, "SECTION");
var h3 = all(c0, function (e) { return e.tagName === "H3"; })[0];
assert.strictEqual(h3.textContent, "Panel 1");
assert.strictEqual(c0.getAttribute("aria-labelledby"), h3.id, "the card is labelled by its title");
assert.ok(h3.id && h3.id !== all(cards[1], function (e) { return e.tagName === "H3"; })[0].id, "each title its own id");
var open0 = byClass(c0, "gp-open")[0];
assert.strictEqual(open0.href, "https://grafana.example/d/u?viewPanel=1&range=7d");
assert.strictEqual(open0.target, "_blank");
assert.strictEqual(open0.rel, "noopener");
assert.strictEqual(open0.getAttribute("aria-label"), "Open Panel 1 in Grafana");
assert.strictEqual(byClass(c0, "gp-scope")[0].textContent, "USD · 7d");
assert.strictEqual(byClass(c0, "gp-note")[0].textContent, "note 1");
assert.strictEqual(byClass(cards[1], "gp-note").length, 0, "no note, no line");
var f0 = frames[0];
assert.strictEqual(f0.getAttribute("src"), null, "built without src");
assert.strictEqual(f0.getAttribute("title"), "Panel 1 Grafana panel");
assert.strictEqual(f0.getAttribute("sandbox"), "allow-scripts allow-same-origin allow-popups");
assert.strictEqual(f0.getAttribute("referrerpolicy"), "same-origin");
assert.strictEqual(f0.getAttribute("scrolling"), "no");
assert.strictEqual(f0.getAttribute("loading"), "lazy", "native lazy stays as a fallback");
assert.strictEqual(byClass(c0, "gp-word")[0].textContent, "Loading chart…", "the skeleton's words");
assert.strictEqual(observers.length, 1);
var io = observers[0];
assert.strictEqual(io.o.root, body, "rooted on the panel body by default");
assert.strictEqual(io.o.rootMargin, "0px 0px 200px 0px");
assert.strictEqual(io.watch.length, 3);
f0.fire("load");   // the about:blank load before any src
assert.ok(!byClass(c0, "gp-box")[0].classList.contains("loaded"), "a load before the src is not the chart");
io.show(c0);
assert.strictEqual(f0.getAttribute("src"), "https://grafana.example/d-solo/u?panelId=1&range=7d", "in view: the src");
assert.strictEqual(frames[1].getAttribute("src"), null, "out of view: none yet");
assert.strictEqual(io.watch.length, 2, "a started card is no longer watched");
f0.fire("load");
var box0 = byClass(c0, "gp-box")[0];
assert.ok(box0.classList.contains("loaded"));
assert.ok(byClass(c0, "gp-word")[0].hidden, "the skeleton label hides on load");
io.show(cards[2]);
tick(15000);
var w2 = byClass(cards[2], "gp-word")[0];
assert.ok(/^This chart is taking a while\. Open$/.test(w2.textContent), w2.textContent);
assert.ok(!w2.hidden);
assert.strictEqual(byClass(w2, "gp-open")[0].href, "https://grafana.example/d/u?viewPanel=3&range=7d", "the slow line carries the card's Open");
assert.strictEqual(byClass(c0, "gp-word")[0].hidden, true, "a loaded frame is never called slow");
frames[2].fire("load");
assert.ok(w2.hidden, "a late load clears it");
report.lazy = true;

/* a range change: a started frame reloads, one not yet in view only changes what it will load */
assert.ok(stack.update(panels("30d")));
assert.strictEqual(f0.getAttribute("src"), "https://grafana.example/d-solo/u?panelId=1&range=30d", "the loaded frame reloads");
assert.ok(!box0.classList.contains("loaded"), "and shows its skeleton again");
assert.strictEqual(byClass(c0, "gp-word")[0].textContent, "Loading chart…");
assert.strictEqual(open0.href, "https://grafana.example/d/u?viewPanel=1&range=30d");
assert.strictEqual(frames[1].getAttribute("src"), null, "still none out of view");
io.show(cards[1]);
assert.strictEqual(frames[1].getAttribute("src"), "https://grafana.example/d-solo/u?panelId=2&range=30d", "it loads the new range when it comes into view");
assert.strictEqual(stack.update(panels("30d").slice(1)), false, "other cards: paint again");
var noBase = panels("7d").map(function (p) { return Object.assign({}, p, { src: "", open_url: "" }); });
assert.strictEqual(stack.update(noBase), false, "a base that went: paint again");
stack.stop();
assert.ok(io.gone, "stop lets the observer go");
report.update = true;

/* no Grafana base: the words, no frame, no Open */
var h2 = new El("div");
var s2 = C.paintCostCards(h2, noBase, { root: host });
assert.strictEqual(all(h2, function (e) { return e.tagName === "IFRAME"; }).length, 0);
assert.strictEqual(byClass(h2, "gp-open").length, 0);
assert.ok(byClass(h2.children[0], "gp-box")[0].classList.contains("unset"));
assert.strictEqual(byClass(h2.children[0], "gp-word")[0].textContent, "Grafana isn’t set up in this environment.");
assert.strictEqual(observers.length, 1, "nothing to watch: no observer");
tick(1e9);
assert.strictEqual(byClass(h2.children[0], "gp-word")[0].textContent, "Grafana isn’t set up in this environment.", "never slow");
s2.stop();
report.unset = true;

/* opts.root, and no IntersectionObserver: every frame starts at once */
var h3h = new El("div");
C.paintCostCards(h3h, panels("today"), { root: host });
assert.strictEqual(observers[1].o.root, host, "opts.root is the scroll container watched");
delete global.IntersectionObserver;
var h4 = new El("div");
C.paintCostCards(h4, panels("today"));
assert.deepStrictEqual(all(h4, function (e) { return e.tagName === "IFRAME"; }).map(function (f) { return f.getAttribute("src"); }), panels("today").map(function (p) { return p.src; }));
report.noObserver = true;

/* rangeOff (gd-TGYiliW9OR): a chip has something to move only while some card has a chart */
assert.strictEqual(C.rangeOff({ panels: panels("7d") }), "", "charts: the chips stay");
assert.strictEqual(C.rangeOff({ panels: noBase }), "No range to choose: Grafana isn’t set up here.");
assert.strictEqual(C.rangeOff({ panels: [] }), "No range to choose: there are no charts here.");
assert.strictEqual(C.rangeOff({ panels: [noBase[0], panels("7d")[1]] }), "", "one chart is enough");
report.rangeOff = true;

/* unrecorded (gd-vViKmjtrTj): a project the farm has no row for, with a blob, says so; nothing else does */
var blob = { projects: [{ project: "gardenio", has: true, today_usd: 1, week_usd: 2 }] };
assert.strictEqual(C.unrecorded(blob, "zeta"), "The garden has recorded no spend for zeta yet.");
assert.strictEqual(C.unrecorded(blob, "gardenio"), "", "a row: its figures say it");
assert.strictEqual(C.unrecorded(blob, "fleet"), "", "the fleet is no project");
assert.strictEqual(C.unrecorded(null, "zeta"), "", "no blob: the farm cannot say");
report.unrecorded = true;

/* a tenant land's credits (step 3.3): land:<name> is the costs-land route, no theme, no farm figures, and its own 404 */
assert.strictEqual(C.landOf("land:acme"), "acme");
assert.strictEqual(C.setOf("land:acme"), "costs-land");
assert.strictEqual(C.scopeName("land:acme"), "acme");
assert.strictEqual(C.chartsURL("land:acme", "today"), "/garden/charts/costs-land?land=acme&range=today");
assert.strictEqual(C.chartsURL("land:a&b", "1y"), "/garden/charts/costs-land?land=a%26b&range=7d", "encoded; a range outside the chips is 7 days");
assert.strictEqual(C.figures(blob, "land:gardenio"), null, "no dollar figures over a land's credits");
assert.strictEqual(C.unrecorded(blob, "land:zeta"), "");
assert.strictEqual(C.rangeOff({ land: "acme", history: "ok" }), "", "the chips move the burn line");
assert.deepStrictEqual(C.failure(404, "land:zeta"), { text: "There is no land zeta to chart.", retry: false });
var lands = { projects: [{ project: "gardenio" }, { project: "home", purse: { state: "unmetered" } }, { project: "acme", purse: { state: "ok", remaining_credits: 38 } }, { project: "beta", purse: { state: "wall", remaining_credits: 1 } }] };
assert.deepStrictEqual(C.landHere(lands, "beta"), { land: "beta", left: 1 }, "the cursor's land");
assert.deepStrictEqual(C.landHere(lands, "gardenio"), { land: "acme", left: 38 }, "off a land: the first with a purse, never an unmetered one");
assert.strictEqual(C.landHere({ projects: [{ project: "home", purse: { state: "unmetered" } }] }, ""), null);
assert.strictEqual(C.landHere(null, ""), null);
report.land = true;

console.log(JSON.stringify(report));
