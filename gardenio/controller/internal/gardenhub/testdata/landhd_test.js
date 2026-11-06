/* The land header (web/landhd.js, Lands Track B step 8.8), run by TestLandHeaderUnderNode:
   node landhd_test.js <web dir>
   The pure half: ?land= read from a search, whether the farm shows a land, the purse in words at each level (ok, half used,
   low, the wall, unknown, none granted; an operator land has none) and the steward's run cap while it is hit. Then the page
   under a small DOM: off a land the plaque is untouched; on a land the farm does not show, still untouched; once the farm
   document shows it, the plaque names the land, the row says the purse and links its credits, and a later document moves
   the words; a capped steward's run cap shows beside it, linking the steward, and lifts with the next day's document.
   Prints a JSON report whose last line the Go test reads. */
"use strict";
var path = require("path"), assert = require("assert");
var file = path.join(process.argv[2], "landhd.js");
var L = require(file);
var report = {};

/* ---------- 1. pure ---------- */
assert.strictEqual(L.landOf("?land=acme"), "acme");
assert.strictEqual(L.landOf("?at=gardenio&land=a%26b&x=1"), "a&b");
assert.strictEqual(L.landOf("?land=+acme+"), "acme", "trimmed, + is a space");
assert.strictEqual(L.landOf("?landing=acme"), "");
assert.strictEqual(L.landOf("?land=%E0"), "", "a bad escape is no land");
assert.strictEqual(L.landOf(""), "");
var doc = { plans: [{ project: "acme" }], seeds: [], gardeners: [{ project: "zeta" }], cost: { projects: [{ project: "tomato" }] } };
assert.ok(L.known(doc, "acme") && L.known(doc, "zeta") && L.known(doc, "tomato"));
assert.ok(!L.known(doc, "other") && !L.known(null, "acme") && !L.known(doc, ""));
assert.strictEqual(L.purse(null), null, "an operator land has no purse");
assert.strictEqual(L.purse({ state: "unmetered" }), null);
var ok = L.purse({ state: "ok", granted_credits: 100, remaining_credits: 85, used_pct: 15 });
assert.deepStrictEqual(ok, { left: "85 of 100 credits left", level: "ok", tone: "", title: "15% used." });
assert.strictEqual(L.purse({ state: "ok", granted_credits: 100, remaining_credits: 45, used_pct: 55 }).level, "half used");
var low = L.purse({ state: "ok", granted_credits: 100, remaining_credits: 18.5, used_pct: 81.5 });
assert.deepStrictEqual([low.left, low.level, low.tone], ["18.5 of 100 credits left", "low", "warn"]);
var wall = L.purse({ state: "wall", granted_credits: 100, remaining_credits: 3.2, used_pct: 96.8 });
assert.deepStrictEqual([wall.left, wall.level, wall.tone], ["3.20 of 100 credits left", "at the wall", "alert"]);
assert.ok(/new agents wait/.test(wall.title), "the wall says what it stops, in words");
var unk = L.purse({ state: "unknown", granted_credits: 100, remaining_credits: null });
assert.deepStrictEqual([unk.left, unk.level, unk.tone], ["Credits unknown", "new agents wait", "alert"]);
assert.strictEqual(L.purse({ state: "ok", granted_credits: 0, remaining_credits: 0, used_pct: null }).left, "No credits granted yet");
assert.strictEqual(L.num(null), "—", "an unknown figure is a dash, never 0");
assert.strictEqual(L.cap(null), null, "no steward (an operator land), no cap");
assert.strictEqual(L.cap({ max_runs_per_day: 24, runs_today: 3, capped: false }), null, "a running steward says nothing");
assert.deepStrictEqual(L.cap({ max_runs_per_day: 24, runs_today: 24, capped: true, resumes_at: 1792800000 }),
  { runs: "24 of 24 runs today", back: "back at 00:00 UTC" }, "the cap in words, the day counted in UTC");
assert.strictEqual(L.cap({ max_runs_per_day: 2, runs_today: 5, capped: true, resumes_at: 1792800000 + 5400 }).back, "back at 01:30 UTC");
assert.strictEqual(L.cap({ max_runs_per_day: 2, runs_today: 2, capped: true }).back, "back at 00:00 UTC", "no resumes_at: the next UTC midnight");
report.pure = true;

/* ---------- 2. the page, under a small DOM ---------- */
function el(tag) {
  var e = { tagName: tag, hidden: false, textContent: "", title: "", href: "", dataset: {}, children: [], cls: {} };
  e.classList = { toggle: function (c, on) { e.cls[c] = !!on; } };
  e.querySelector = function (sel) { return e.q[sel] || null; };
  return e;
}
function page(search, farm) {
  var listeners = {}, h1 = el("h1"), plaque = el("div"), box = el("div"), pEl = el("a"), left = el("span"), level = el("span");
  var cEl = el("a"), runs = el("span"), back = el("span"), sEl = el("a");
  h1.textContent = "The Garden"; box.hidden = true; pEl.hidden = true; cEl.hidden = true; sEl.hidden = true;
  plaque.q = { h1: h1 }; pEl.q = { ".lh-left": left, ".lh-level": level }; cEl.q = { ".lh-runs": runs, ".lh-back": back };
  box.q = { ".lh-purse": pEl, ".lh-cap": cEl, ".lh-steward": sEl };
  global.document = {
    title: "The Garden", body: { dataset: { base: "/v" } },
    querySelector: function (s) { return s === ".crumb .plaque" ? plaque : null; },
    getElementById: function (id) { return id === "land-hd" ? box : null; },
    addEventListener: function (n, f) { listeners[n] = f; }
  };
  global.location = { search: search };
  global.window = { GARDEN: { farm: function () { return farm; } } };
  delete require.cache[require.resolve(file)];
  require(file);
  return { h1: h1, plaque: plaque, box: box, pEl: pEl, left: left, level: level, cEl: cEl, runs: runs, back: back, sEl: sEl,
    doc: function (d) { if (listeners.farmdoc) listeners.farmdoc({ detail: d }); } };
}
var tenantDoc = { plans: [{ project: "acme" }], cost: { projects: [{ project: "acme", purse: { state: "ok", granted_credits: 100, remaining_credits: 85, used_pct: 15 } }] } };

var off = page("?at=gardenio", tenantDoc);
assert.strictEqual(off.h1.textContent, "The Garden", "off a land the plaque is the Garden's");
assert.ok(off.box.hidden);

var early = page("?land=acme", null);
assert.strictEqual(early.h1.textContent, "The Garden", "before the farm arrives nothing is renamed");
assert.ok(early.box.hidden);
early.doc({ plans: [{ project: "other" }] });
assert.ok(early.box.hidden, "a land the farm does not show leaves the plaque as it was");
early.doc(tenantDoc);
assert.strictEqual(early.h1.textContent, "acme");
assert.strictEqual(global.document.title, "acme — The Garden");
assert.ok(!early.box.hidden && !early.pEl.hidden && early.plaque.cls["is-land"]);
assert.strictEqual(early.left.textContent, "85 of 100 credits left");
assert.strictEqual(early.level.textContent, "ok");
assert.strictEqual(early.pEl.href, "/v/garden?costs=land:acme", "the purse opens the land's credits");
early.doc({ plans: [{ project: "acme" }], cost: { projects: [{ project: "acme", purse: { state: "wall", granted_credits: 100, remaining_credits: 3.2, used_pct: 96.8 } }] } });
assert.strictEqual(early.level.textContent, "at the wall");
assert.strictEqual(early.pEl.dataset.tone, "alert");
early.doc({ plans: [{ project: "acme" }], cost: { projects: [{ project: "acme" }] } });
assert.ok(!early.box.hidden && early.pEl.hidden, "an operator land keeps its name, with no purse");

assert.ok(early.cEl.hidden, "no steward, no cap");

var STW = { id: "gd-stw1", href: "/garden?bead=gd-stw1", max_runs_per_day: 24, runs_today: 3, capped: false };
var stw = function (s) { return { plans: [{ project: "acme" }], cost: { projects: [{ project: "acme", steward: Object.assign({}, STW, s) }] } }; };
var capped = page("?land=acme", stw({}));
assert.ok(!capped.box.hidden && capped.cEl.hidden, "a steward under its cap says nothing");
capped.doc(stw({ runs_today: 24, capped: true, resumes_at: 1792800000 }));
assert.ok(!capped.cEl.hidden && capped.pEl.hidden, "the cap shows on a land with no purse too");
assert.strictEqual(capped.runs.textContent, "24 of 24 runs today");
assert.strictEqual(capped.back.textContent, "back at 00:00 UTC");
assert.strictEqual(capped.cEl.href, "/v/garden?bead=gd-stw1", "the cap opens the steward's bead");
capped.doc(stw({ runs_today: 0 }));
assert.ok(capped.cEl.hidden, "the next day's document lifts it");
capped.doc({ plans: [{ project: "acme" }], cost: { projects: [{ project: "other", steward: Object.assign({}, STW, { capped: true }) }] } });
assert.ok(capped.cEl.hidden, "another land's capped steward is not this land's");
assert.ok(capped.sEl.hidden, "no steward of this land: no Steward link");
capped.doc(stw({}));
assert.ok(!capped.sEl.hidden, "a land with a steward: Steward shows (gd-LisW87zrnf)");
assert.strictEqual(capped.sEl.href, "/v/garden/c/s/acme", "Steward opens the land's steward view");
assert.strictEqual(L.stewardHref("/v", { cost: { projects: [{ project: "a b", steward: STW }] } }, "a b"), "/v/garden/c/s/a%20b");
assert.strictEqual(L.stewardHref("", { cost: { projects: [{ project: "acme" }] } }, "acme"), "", "no steward row: no link");

var at = page("?land=acme", tenantDoc);
assert.strictEqual(at.h1.textContent, "acme", "a farm already loaded names the land at once");
report.page = true;

console.log(JSON.stringify(report));
