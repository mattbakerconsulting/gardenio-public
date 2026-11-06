/* The escalation kind vocabulary (web/esckind.js, homestead-world-design.md 28.11), run by TestEscKindUnderNode:
   node esckind_test.js <web dir>
   Every family's noun, an unknown or missing family read as "other"; the state words, each saying the state; who it waits
   on; the since span; the accessible name, saying who once; the Escalations cell's order; the plan it came from, the sign tip, when
   a cell offers the in-place answer, the cell grid and its "+N" crate past the cap, and the sign's count. Prints a JSON report whose families the Go
   test compares with the Go family table. */
"use strict";
var path = require("path"), assert = require("assert");
var ESC = require(path.join(process.argv[2], "esckind.js"));
var NOW = 1800000000 * 1000;   // ms, as the page's Date.now()
var H = 3600;

/* ---------- 1. families and nouns ---------- */
assert.deepStrictEqual(ESC.FAMILIES.map(function (f) { return ESC.noun({ family: f }); }),
  ["merge", "deploy", "config edit", "prompt install", "beads admin", "access", "infra", "read-only query", "escalation"], "one noun per family");
assert.strictEqual(ESC.family({ family: "teleport" }), "other", "a family the page does not know yet is other");
assert.strictEqual(ESC.family({}), "other", "no family is other");
assert.strictEqual(ESC.family({ family: "toString" }), "other", "an inherited key is not a family");
assert.strictEqual(ESC.noun(null), "escalation");

/* ---------- 2. state words and who ---------- */
var pend = { id: "gd-a", title: "Merge PR #1", kind: "escalation", state: "pending", human: true, since: NOW / 1000 - 2 * H, family: "merge" };
var desk = { id: "gd-b", title: "Merge PR #2", kind: "escalation", state: "pending", human: false, waits_on: "the merge desk", since: NOW / 1000 - 30, family: "merge" };
var run = { id: "gd-c", title: "Deploy the bridge", kind: "escalation", state: "approved", human: false, waits_on: "gardenio-esc-x", since: NOW / 1000 - 20 * 60, family: "deploy" };
var manual = { id: "gd-d", title: "Rotate a token", kind: "escalation", state: "approved", human: true, waits_on: "the operator", since: NOW / 1000 - 3 * 86400, family: "access" };
var rej = { id: "gd-e", title: "Grant RBAC", kind: "escalation", state: "rejected", human: false, waits_on: "gardenio-crew-y", since: NOW / 1000 - 5 * H, family: "access" };
var fail = { id: "gd-f", title: "Query the db", kind: "escalation", state: "failed", human: true, waits_on: "the operator", since: NOW / 1000 - H, family: "read" };
var odd = { id: "gd-g", title: "", kind: "escalation", state: "", human: false, since: 0, op: "teleport", family: "other" };
assert.deepStrictEqual([pend, desk, run, manual, rej, fail, odd].map(ESC.word),
  ["waiting on you", "waiting on the merge desk", "approved, running", "approved, waiting on you", "rejected, still open", "failed, still open", "open"],
  "every state word says the state");
assert.strictEqual(ESC.word({ state: "pending", human: false }), "waiting on approval", "a pending wait that names nobody");
assert.deepStrictEqual([pend, desk, run, manual, rej, fail, odd].map(ESC.who),
  ["you", "the merge desk", "gardenio-esc-x", "you", "gardenio-crew-y", "you", ""], "who: you for a human wait, else waits_on");

/* ---------- 3. since and the accessible name ---------- */
assert.deepStrictEqual([pend, desk, run, manual, odd].map(function (w) { return ESC.since(w, NOW); }), ["2 h", "1 min", "20 min", "3 d", ""], "since");
assert.strictEqual(ESC.since({ since: NOW / 1000 + 50 }, NOW), "1 min", "a clock ahead of the farm's is not negative");
assert.strictEqual(ESC.name(pend, NOW), "Merge escalation: Merge PR #1, waiting on you, since 2 h", "the task's example");
assert.strictEqual(ESC.name(desk, NOW), "Merge escalation: Merge PR #2, waiting on the merge desk, since 1 min", "who is said once");
assert.strictEqual(ESC.name(run, NOW), "Deploy escalation: Deploy the bridge, approved, running, waits on gardenio-esc-x, since 20 min");
assert.strictEqual(ESC.name(manual, NOW), "Access escalation: Rotate a token, approved, waiting on you, since 3 d");
assert.strictEqual(ESC.name(rej, NOW), "Access escalation: Grant RBAC, rejected, still open, waits on gardenio-crew-y, since 5 h");
assert.strictEqual(ESC.name(fail, NOW), "Read-only query escalation: Query the db, failed, still open, waits on you, since 1 h");
assert.strictEqual(ESC.name(odd, NOW), "Escalation: gd-g, open", "other says escalation once; no title, the id");
assert.strictEqual(ESC.name({ family: "config", title: "Edit values", state: "pending", human: true }, NOW), "Config edit escalation: Edit values, waiting on you");

/* ---------- 4. the Escalations cell's order ---------- */
var pend2 = { id: "gd-h", state: "pending", human: true, since: pend.since + 60, family: "deploy" };
var pend3 = { id: "gd-0", state: "pending", human: true, since: pend.since, family: "prompt" };
var input = [fail, odd, rej, run, desk, pend, manual, pend2, pend3];
var got = ESC.order(input).map(function (w) { return w.id; });
assert.deepStrictEqual(got, ["gd-h", "gd-0", "gd-a", "gd-d", "gd-b", "gd-c", "gd-g", "gd-f", "gd-e"],
  "waiting on you first, then in hand, then rejected and failed; newest first within a group, then id");
assert.deepStrictEqual(input.map(function (w) { return w.id; }), ["gd-f", "gd-g", "gd-e", "gd-c", "gd-b", "gd-a", "gd-d", "gd-h", "gd-0"], "order copies; the farm's list is left as it came");
assert.deepStrictEqual([pend, desk, run, manual, rej, fail].map(ESC.group), [0, 1, 1, 0, 2, 2]);
assert.deepStrictEqual(ESC.order(null), []);

/* ---------- 5. where it came from, the sign tip and the in-place answer ---------- */
var plans = [
  { id: "gd-p1", title: "Escalation yard", project: "gardenio", waiting: [{ id: "gd-x", kind: "decision" }, { id: "gd-a", kind: "escalation" }] },
  { id: "gd-p2", title: "", project: "gardenio", waiting: [{ id: "gd-a", kind: "escalation" }, { id: "gd-c", kind: "escalation" }] },
  { id: "gd-p3", title: "No waits" }, null,
  { id: "gd-p4", title: "Escalation yard: retire the produce bin and show every open escalation", short_title: "Escalation yard", waiting: [{ id: "gd-s", kind: "escalation" }] }];
var lin = ESC.lineage(plans);
assert.deepStrictEqual(Object.keys(lin).sort(), ["gd-a", "gd-c", "gd-s"], "escalations only; a decision in waiting[] is not one");
assert.strictEqual(ESC.from({ id: "gd-s" }, lin), "plan Escalation yard", "a plan goes by its short title");
assert.strictEqual(lin["gd-a"].id, "gd-p1", "the first plan to list it wins");
assert.strictEqual(ESC.from(pend, lin), "plan Escalation yard");
assert.strictEqual(ESC.from(run, lin), "plan gd-p2", "a plan with no title is its id");
assert.strictEqual(ESC.from({ id: "gd-z", project: "kbeads" }, lin), "project kbeads", "no plan lists it: its project");
assert.strictEqual(ESC.from(odd, lin), "", "unprojected, no plan: the Commons'");
assert.deepStrictEqual(ESC.lineage(null), {});
assert.deepStrictEqual(ESC.tip(pend, NOW, lin), ["Merge PR #1", "Merge escalation, waiting on you, since 2 h, from plan Escalation yard."], "title, family, state, since, plan");
assert.deepStrictEqual(ESC.tip(rej, NOW, lin), ["Grant RBAC", "Access escalation, rejected, still open, waits on gardenio-crew-y, since 5 h."], "who it waits on");
assert.deepStrictEqual(ESC.tip({ id: "gd-k", title: "Merge PR #9: fix(x): y", state: "pending", human: true, family: "merge", project: "coop" }, NOW, lin),
  ["Merge PR #9: fix(x): y", "Merge escalation, waiting on you, from project coop."], "a colon in the title is the title's");
assert.deepStrictEqual(ESC.tip(odd, NOW, lin), ["gd-g", "Escalation, open."], "no title: the id");
assert.deepStrictEqual([pend, desk, run, manual, rej, fail].map(function (w) { return ESC.answerable(w, true); }), [true, true, false, false, false, false],
  "only a pending escalation is answered in place");
assert.strictEqual(ESC.answerable(pend, false), false, "not for an account that may not approve");
assert.strictEqual(ESC.answerable(null, true), false);

/* ---------- 6. the cell grid, the crate and the sign's count ---------- */
var grid = ESC.cells(input, { cols: 4, id: "home:gardenio/escalation" });
assert.deepStrictEqual(grid.map(function (c) { return [c.id, c.r, c.c]; }),
  [["gd-h", 0, 0], ["gd-0", 0, 1], ["gd-a", 0, 2], ["gd-d", 0, 3], ["gd-b", 1, 0], ["gd-c", 1, 1], ["gd-g", 1, 2], ["gd-f", 1, 3], ["gd-e", 2, 0]],
  "one cell each, in the cell's order, cols wide");
assert.ok(grid.every(function (c) { return c.kind === "escalation" && c.w.id === c.id; }));
var many = [];
for (var k = 0; k < 30; k++) many.push({ id: "gd-m" + (k < 10 ? "0" : "") + k, state: "pending", human: true, since: 1000 + k, family: "merge" });
var full = ESC.cells(many.slice(0, 24), { cols: 6, cap: 24 });
assert.ok(!("CAP" in ESC), "no fixed cap: the cell's slots are (29.10)");
assert.strictEqual(full.length, 24, "at the cap every one has its cell");
assert.ok(full.every(function (c) { return c.kind === "escalation"; }), "and no crate");
var over = ESC.cells(many, { cols: 6, id: "home:gardenio/escalation", cap: 24 }), last = over[over.length - 1];
assert.strictEqual(over.length, 24, "past the cap the grid stays the cap's size");
assert.deepStrictEqual([last.id, last.kind, last.n, last.i, last.r, last.c], ["home:gardenio/escalation/more", "crate", 7, 23, 3, 5], "23 cells and a +7 crate in the last");
assert.strictEqual(ESC.cells(many, { cols: 6 }).length, 30, "with no cap given, every one has its cell");
assert.strictEqual(over[0].id, "gd-m29", "the newest first");
var capped = ESC.cells(many.slice(0, 10), { cols: 6, total: 12 });
assert.deepStrictEqual(capped.map(function (c) { return c.kind; }).filter(function (k) { return k === "crate"; }).length, 1, "the farm sent fewer than it counted: a crate");
assert.strictEqual(capped[capped.length - 1].n, 2, "it counts the ones the farm did not send");
assert.strictEqual(capped[capped.length - 1].id, "escalations/more");
assert.deepStrictEqual(ESC.cells([], { cols: 4 }), [], "an empty cell has no grid");
assert.deepStrictEqual(ESC.cells([pend], {}).map(function (c) { return [c.r, c.c]; }), [[0, 0]], "no cols is one column");
assert.deepStrictEqual([0, 1, 3].map(ESC.count), ["", "1 escalation", "3 escalations"]);

console.log(JSON.stringify({ families: ESC.FAMILIES, order: got }));
