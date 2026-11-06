/* A tenant land's credit charts (web/credits.js, plan gd-gh5J0vVbJP step 3.3), run by TestCreditsUnderNode:
   node credits_test.js <web dir>
   The pure half: credits in words (an unknown figure is a dash), the tiles, the meter's shares and words (the wall, an
   unreadable purse), the burn line in the chart's box with its dashed run-out stopping at the wall, the run-out's words,
   the agents' bars and the rest's line, the degraded states' words. Then paint under a small DOM: every card titled,
   the SVG built through createElementNS, the readout on a pointer and on the arrows, and a degraded answer painting
   only the meter and its one line. No text ever goes in as markup. Prints a JSON report whose last line the Go test
   reads. */
"use strict";
var path = require("path"), assert = require("assert");
var CR = require(path.join(process.argv[2], "credits.js"));
var report = {};

/* ---------- 1. pure ---------- */
assert.strictEqual(CR.num(1.5), "1.50");
assert.strictEqual(CR.num(38), "38.0");
assert.strictEqual(CR.num(1234.4), "1234");
assert.strictEqual(CR.num(null), "—", "unknown is a dash, never 0");
assert.strictEqual(CR.when(1793516400), "2026-11-01 07:00 UTC");
assert.strictEqual(CR.span(1800), "within the hour");
assert.strictEqual(CR.span(66 * 3600), "in about 3 days");
var d = {
  land: "acme", range: "7d", history: "ok",
  purse: { state: "ok", granted_credits: 100, remaining_credits: 38, margin_credits: 5 },
  spent: { today: 1.5, week: 20, month: null },
  burn: { start: 0, end: 7 * 86400, points: [[0, 60], [3 * 86400, 50], [7 * 86400, 40]], rate_per_hour: 0.5, wall: 5, run_out: 7 * 86400 + 66 * 3600 },
  agents: [{ agent: "crew-a", credits: 8 }, { agent: "crew-b", credits: 2 }], rest: { count: 3, credits: 1.25 }
};
assert.deepStrictEqual(CR.tiles(d), [["Today", "1.50"], ["7 days", "20.0"], ["30 days", "—"]]);
var m = CR.meter(d.purse);
assert.strictEqual(m.pct, 38);
assert.strictEqual(m.wall, 5);
assert.strictEqual(m.words, "38.0 of 100.0 credits left; the wall is at 5.00.");
assert.ok(/At the wall/.test(CR.meter({ state: "wall", granted_credits: 50, remaining_credits: 1, margin_credits: 2.5 }).words));
assert.strictEqual(CR.meter({ state: "unknown" }).pct, null, "an unreadable purse draws no bar");
assert.strictEqual(CR.meter({ state: "ok", granted_credits: 0, remaining_credits: null }).words, "No credits granted yet.");
assert.strictEqual(CR.meter({ state: "ok", granted_credits: 10, remaining_credits: -3, margin_credits: 2 }).pct, 0, "clamped");
var b = CR.burn(d);
assert.strictEqual(b.line.length, 3);
assert.ok(b.line[0][1] < b.line[2][1], "less left is lower");
assert.ok(b.proj && b.proj[1][0] === 314 && b.proj[1][1] < b.wallY, "a run-out past the box: the dash runs to its edge, above the wall");
var soon = JSON.parse(JSON.stringify(d)); soon.burn.rate_per_hour = 2; soon.burn.run_out = soon.burn.end + 16.5 * 3600;
var bs = CR.burn(soon);
assert.ok(Math.abs(bs.proj[1][1] - bs.wallY) < 1e-9, "a run-out in the box: the dash stops on the wall");
assert.ok(bs.proj[1][0] > bs.nowX && bs.proj[1][0] < 314, "after now, inside the box");
assert.strictEqual(b.words, "At 0.50 credits an hour, the land reaches its wall in about 3 days (2026-01-10 18:00 UTC).".replace("2026-01-10 18:00", CR.when(d.burn.run_out).slice(0, 16)));
var far = JSON.parse(JSON.stringify(d)); far.burn.run_out = null;
assert.strictEqual(CR.burn(far).words, "At 0.50 credits an hour, no run-out is in sight.");
assert.ok(CR.burn(far).proj[1][1] > CR.burn(far).proj[0][1], "still burning down to the box's edge");
var idle = JSON.parse(JSON.stringify(d)); idle.burn.rate_per_hour = 0; idle.burn.run_out = null;
assert.strictEqual(CR.burn(idle).proj, null);
assert.ok(/no run-out/.test(CR.burn(idle).words));
var wall = JSON.parse(JSON.stringify(d)); wall.burn.run_out = wall.burn.end;
assert.strictEqual(CR.burn(wall).words, "At 0.50 credits an hour, the land is at its wall.");
var a = CR.agents(d);
assert.deepStrictEqual(a.rows, [["crew-a", 100, "8.00"], ["crew-b", 25, "2.00"]]);
assert.strictEqual(a.rest, "and 3 more agents: 1.25");
assert.strictEqual(CR.agents({ agents: [], rest: { count: 1, credits: 1 } }).rest, "and 1 more agent: 1.00");
assert.strictEqual(CR.historyWord({ history: "none" }), "No credit history yet.");
assert.strictEqual(CR.historyWord({ history: "unavailable" }), "Credit history is unavailable right now.");
assert.strictEqual(CR.historyWord(d), "");
report.pure = true;

/* ---------- 2. paint under a small DOM ---------- */
function El(tag, ns) { this.tagName = tag; this.ns = ns || ""; this.children = []; this.attrs = {}; this.on = {}; this.style = {}; this.className = ""; this._t = ""; this.id = ""; this.ownerDocument = doc; }
Object.defineProperty(El.prototype, "textContent", { get: function () { return this._t + this.children.map(function (c) { return c.textContent; }).join(""); }, set: function (v) { this._t = String(v); this.children = []; } });
Object.defineProperty(El.prototype, "innerHTML", { set: function () { throw new Error("markup"); } });
El.prototype.appendChild = function (c) { this.children.push(c); return c; };
El.prototype.setAttribute = function (k, v) { if (k === "class") this.className = String(v); this.attrs[k] = String(v); };
El.prototype.addEventListener = function (n, f) { this.on[n] = f; };
El.prototype.getBoundingClientRect = function () { return { left: 0, width: 320 }; };
function all(e, pred, out) { out = out || []; if (pred(e)) out.push(e); e.children.forEach(function (c) { all(c, pred, out); }); return out; }
function cls(e, c) { return all(e, function (x) { return x.className.split(/\s+/).indexOf(c) >= 0; }); }
var doc = { createElement: function (t) { return new El(t); }, createElementNS: function (ns, t) { return new El(t, ns); } };
var host = new El("div"); host.appendChild(new El("p"));
CR.paint(host, d);
assert.deepStrictEqual(cls(host, "gp-t").map(function (h) { return h.textContent; }), ["Credits spent", "Remaining of granted", "Burn to the wall", "Credits per agent"]);
cls(host, "cr-card").forEach(function (s) { assert.strictEqual(s.attrs["aria-labelledby"], s.children[0].id); });
assert.deepStrictEqual(all(host, function (x) { return x.tagName === "dd"; }).map(function (x) { return x.textContent; }), ["1.50", "20.0", "—"]);
var svgs = all(host, function (x) { return x.tagName === "svg"; });
assert.strictEqual(svgs.length, 2);
svgs.forEach(function (s) { assert.strictEqual(s.ns, "http://www.w3.org/2000/svg"); assert.strictEqual(s.attrs.role, "img"); assert.ok(s.attrs["aria-label"]); });
assert.strictEqual(cls(host, "cr-fill")[0].attrs.width, "38");
assert.strictEqual(cls(host, "cr-proj").length, 1);
assert.deepStrictEqual(cls(host, "cr-lbl").map(function (x) { return x.textContent; }), ["100.0", "now", "wall"], "the scale in words");
var chart = svgs[1], read = cls(host, "cr-read")[0];
assert.strictEqual(read.textContent, "", "no readout until asked");
chart.on.pointermove({ clientX: 140 });
assert.strictEqual(read.textContent, CR.when(3 * 86400) + ": 50.0 credits left", "the nearest point");
var stop = false;
chart.on.keydown({ key: "ArrowRight", preventDefault: function () { stop = true; } });
assert.ok(stop && /40\.0 credits left/.test(read.textContent), "the arrows step");
assert.strictEqual(cls(host, "cr-bar-fill")[1].style.width, "25.0%");
assert.deepStrictEqual(cls(host, "cr-name").map(function (x) { return x.textContent; }), ["crew-a", "crew-b"]);
report.paint = true;

["none", "unavailable"].forEach(function (h) {
  var x = { land: "acme", history: h, purse: d.purse, spent: null, burn: { points: [] }, agents: [] };
  CR.paint(host, x);
  assert.deepStrictEqual(cls(host, "gp-t").map(function (e) { return e.textContent; }), ["Remaining of granted"], h + ": only the purse's meter");
  assert.strictEqual(cls(host, "cr-none")[0].textContent, CR.historyWord(x));
});
report.degraded = true;

console.log(JSON.stringify(report));
