/* The steady title bar (web/curnav.js nav.bar, gd-yozHl8UqIs), run by TestCursorBarUnderNode: node curbar_test.js <web dir>.
   Over the fixture farm (testdata/farm_fixture.json) with short titles put on plan A, its first phase and a step (step 1 of the
   plan, gd-MpWy5X34hh): on a step the bar reads Plan › Phase › Task from the short titles, each part the Thing a press jumps the
   cursor to and an address the cursor resolves back to the same place, the step's state word at the end; a phase reads two
   parts, a plot its plan alone, the project its name; and a Thing with no short title keeps its title. The status line of
   cursor-grid spec 6.4, whole, the need words first, which the bar carries since the full label went (gd-zDdiA3HBh5): a
   project's plans and need words, a plan's phase n of N and steps, a phase's steps and growing, a task's state word, @assignee
   and age, and a harvested step's lead with every badge as an icon. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var W = require(path.join(process.argv[2], "world.js")), C = require(path.join(process.argv[2], "cursor.js")), N = require(path.join(process.argv[2], "curnav.js"));
var fixture = require(path.join(__dirname, "farm_fixture.json")), NOW = 1792001000000, report = {};
var planA = fixture.plans.filter(function (p) { return p.id === "gd-planA"; })[0];
planA.short_title = "Edge IdP"; planA.phases[0].short_title = "Foundations first"; planA.phases[0].tasks[0].short_title = "Token exchange";
var world = W.layout(fixture, null, { now: NOW, readonly: true }), nav = N.nav();
nav.build(world, null, { now: NOW });
function names(b) { return b.parts.map(function (p) { return p.name; }); }
function same(b) { b.parts.forEach(function (p, i) { assert.deepStrictEqual(C.pathFor(nav.tree, p.at), nav.path.slice(0, nav.path.length - b.parts.length + i + 1), p.at + " resolves to its place"); assert.ok(nav.tree.nodes[p.id], p.id + " is a Thing"); }); }

/* on a step: three parts, the short titles, the task's state word */
assert.ok(nav.to("gd-A1a").found, "the cursor reaches step gd-A1a");
var b = nav.bar(world.index);
assert.deepStrictEqual(names(b), ["Edge IdP", "Foundations first", "Token exchange"], "Plan › Phase › Task from the short titles");
assert.deepStrictEqual(b.parts.map(function (p) { return p.id; }), ["gd-planA", "gd-A1", "gd-A1a"], "each part is the Thing's id");
assert.strictEqual(b.parts[0].title, "Edge IdP rollout", "the whole title stays for the tooltip");
assert.strictEqual(b.state, "in review", "the step's state word ends the bar");
assert.strictEqual(b.status, "in review · @mo · 1m", "a task's status line: its state word, @assignee and age");
assert.strictEqual(b.icons, null, "an unharvested step has no badges");
same(b);
report.step = { names: names(b), state: b.state, at: b.parts.map(function (p) { return p.at; }) };

/* a step with no short title keeps its title; its state word is its own */
assert.ok(nav.to("gd-A1b").found);
b = nav.bar(world.index);
assert.deepStrictEqual(names(b), ["Edge IdP", "Foundations first", planA.phases[0].tasks[1].title]);
assert.strictEqual(b.state, "growing");

/* on the phase: two parts; on the plot: the plan alone, with its plan_state; on the project: its name, no state */
assert.ok(nav.to("gd-A1").found);
b = nav.bar(world.index); assert.deepStrictEqual(names(b), ["Edge IdP", "Foundations first"]); same(b);
assert.strictEqual(b.status, "2/4 steps · 1 growing", "a phase's status line: its steps and what grows");
assert.ok(nav.to("gd-planA").found);
b = nav.bar(world.index); assert.deepStrictEqual(names(b), ["Edge IdP"]); assert.strictEqual(b.state, C.stateWord(planA.state)); same(b);
assert.strictEqual(b.status, "1 needs you · phase 1 of 3 · 2/11 steps", "a plan's status line: the need words first, then its phase and steps");
report.plan = b.status;
nav.ascend();
b = nav.bar(world.index); assert.strictEqual(b.parts.length, 1); assert.strictEqual(b.parts[0].id, nav.path[0]); assert.strictEqual(b.state, ""); same(b);
assert.strictEqual(b.status, "1 needs you · 2 plans", "a project's status line: the need words first, then its plans");
report.project = names(b);

/* a harvested step (harvest spec 5): its lead in the status, every badge as an icon and its words, none dropped */
assert.ok(nav.to("gd-A1y").found);
b = nav.bar(world.index);
assert.strictEqual(b.status, "harvested · 16m ago", "a harvested step's lead");
assert.deepStrictEqual(b.icons.map(function (x) { return x.w; }), ["merged !405", "graded 5/5 gates", "3 tests", "checks green", "4 pictures", "merged first time"], "every badge, in priority order");
report.harvest = { status: b.status, icons: b.icons.length };

/* a home cell (homestead-world-design.md 28.6), then what a descend finds in it, each a Thing with an address, parts of its
   project's bar */
assert.ok(nav.to("~/home").found);
nav.descend([]); b = nav.bar(world.index); assert.strictEqual(b.parts.length, nav.path.length - 1); assert.strictEqual(b.parts[0].id, "home:"); same(b);
report.home = { names: names(b), at: b.parts[0].at };

/* no index at all (the layout before the farm document): titles, never a throw */
assert.ok(nav.to("gd-A1a").found);
assert.deepStrictEqual(names(nav.bar(null)), ["Edge IdP rollout", "Foundations", planA.phases[0].tasks[0].title]);
console.log(JSON.stringify(report));
