package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Porch groups the farm's gardeners by plot (gd-ikrXlqYTL5; homestead-agents-as-people.md sections 2 and 6). porch.js
// keeps the grouping pure so it runs without a browser: "Needs you" first, holding every gardener down or waiting on a
// human wherever it stands (gd-J3l6W4X6DJ), then plots in need order with the viewer's first, "In the field" (those at
// home) last, rows the viewer's own first and then need first, and the "Mine" scope. Skipped when node is not installed.
func TestPorchGroupsUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var P = require(process.argv[1]), assert = require("assert");
var g = function (id, plan, state, op, since) { return { id: id, agent: "n-" + id, state: state, bucket: state === "failed" ? "stuck" : state === "working" || state === "yielding" ? "alive" : "idle", operator: op || "oidc:me", since: since || 100, plan: plan }; };
var plots = { p1: { id: "p1", title: "Beta plot" }, p2: { id: "p2", title: "Alpha plot" }, p3: { id: "p3", title: "Gamma plot" } };
var at = function (x) { return x.plan ? plots[x.plan] : null; };
var list = [g("a", "p1", "working"), g("b", "", "failed"), g("c", "p2", "working", "oidc:flo"), g("d", "p1", "failed", "oidc:flo"),
  g("e", "", "idle"), g("f", "p3", "yielding", "oidc:flo"), g("h", "p2", "working", "oidc:me", 50)];
var m = P.groups(list, at, { me: "oidc:me" }), shape = m.groups.map(function (x) { return [x.title, x.rows.map(function (r) { return r.id; })]; });
assert.deepStrictEqual(shape, [
  ["Needs you", ["b", "d", "f"]], // down or asking, at the barn or on a plot: mine first, then need first
  ["Alpha plot", ["h", "c"]],     // holds one of mine; mine first in the rows; h has waited longest
  ["Beta plot", ["a"]],           // holds one of mine
  ["In the field", ["e"]]         // the unplaced bucket, those at home in their fields (28.12), is last
], JSON.stringify(shape));
assert.ok(m.groups[0].need && !m.groups[1].need, "the Needs you group is marked");
assert.deepStrictEqual([m.n, m.work, m.home, m.all], [7, 3, 2, 7], "rows, at work, at home, every gardener");
assert.strictEqual(P.summary(m), "7 gardeners · 3 at work");

var only = P.groups(list, at, { me: "oidc:me", only: true });
assert.deepStrictEqual(only.groups.map(function (x) { return x.title + ":" + x.rows.length; }), ["Needs you:1", "Alpha plot:1", "Beta plot:1", "In the field:1"], "Mine keeps the viewer's own; h has waited longest");
assert.deepStrictEqual([only.n, only.home, only.all], [4, 2, 7], "the page's one gardener count follows the scope (gd-rC46vJomwy)");

var none = P.groups(list, at, { only: true });
assert.deepStrictEqual([none.groups.length, none.n, none.all], [0, 0, 7], "Mine without an identity shows nothing");
assert.deepStrictEqual(P.groups(null, at, {}).groups, []);
assert.strictEqual(P.summary({ n: 1, work: 0 }), "1 gardener · 0 at work");

// The reason (farm_reason.go) decides need: parked or down needs you wherever it is; stale or sleeping does not; a kind this
// client does not know is no reason, so the state decides.
var why = function (id, state, kind) { var o = g(id, "p1", state); o.reason = { kind: kind, text: kind }; return o; };
var rs = P.groups([why("pk", "idle", "parked"), why("dn", "working", "down"), why("sl", "idle", "sleeping"), why("st", "working", "stale"), why("pe", "yielding", "purse_empty")], at, {});
assert.deepStrictEqual(rs.groups.map(function (x) { return x.title + ":" + x.rows.map(function (r) { return r.id; }).join(","); }), ["Needs you:dn,pe,pk", "Beta plot:st,sl"]);

assert.strictEqual(P.taskLine({ task: "t1", task_title: "Fix it" }, plots.p1), "Tending: Fix it");
assert.strictEqual(P.taskLine({ task: "p1" }, plots.p1), "Tending Beta plot", "a plan gardener tends the plan itself");
assert.strictEqual(P.taskLine({ task: "t2", task_title: "Loose" }, null), "Assigned: Loose (not on a plot)");
assert.strictEqual(P.taskLine({ task: "t3" }, null), "Assigned: t3 (not on a plot)");
assert.strictEqual(P.taskLine({}, null), "", "at home with no task: nothing");
// a thread agent at home (5b): the thread it answers, never its thread head as a task; on a plot it tends as anyone does
assert.strictEqual(P.taskLine({ role: "thread", chat: "/c", thread_title: "a pen for the goats", task: "h1", task_title: "Commons thread: a pen for the goats" }, null), "In the Commons: a pen for the goats");
assert.strictEqual(P.taskLine({ role: "dm", chat: "/c" }, null), "In the Commons: a thread", "no title read: still in the Commons");
assert.strictEqual(P.taskLine({ role: "thread", chat: "/c", thread_title: "x", task: "t1", task_title: "Fix it" }, plots.p1), "Tending: Fix it");
assert.strictEqual(P.taskLine({ role: "crew", chat: "/c", thread_title: "x" }, null), "", "a crew with a thread is just at home");
assert.strictEqual(P.taskLine({ tends: "t1", tends_title: "Fix it" }, plots.p1), "Sent to: Fix it", "sent to a bead by the garden (web/tend.js), standing on its plot");
assert.strictEqual(P.taskLine({ tends: "gd-m" }, null), "Sent to: gd-m (not on a plot)", "sent to a meadow bead");
assert.strictEqual(P.taskLine({ tends: "t1", task: "t2", task_title: "Own" }, plots.p1), "Tending: Own", "a task of its own comes first");
assert.strictEqual(P.taskLine({ escalation: "gd-e1", escalation_title: "Merge PR #1114" }, plots.p1, { id: "gd-e1", family: "merge" }), "Running merge escalation: Merge PR #1114", "an escalation's executor, standing on the escalation's plot (farm escalation), its family from the farm document (step 7)");
assert.strictEqual(P.taskLine({ escalation: "gd-e1" }, null), "Running escalation: gd-e1", "an executor whose escalation is off any plan, and not open: no family");
assert.strictEqual(P.taskLine({ escalation: "gd-e1", task: "t2", task_title: "Own" }, plots.p1), "Tending: Own", "a task of its own comes before the escalation");
`
	porch, err := filepath.Abs(filepath.Join("web", "porch.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, porch).CombinedOutput()
	if err != nil {
		t.Fatalf("node porch logic test failed: %v\n%s", err, out)
	}
}

// The Porch's DOM under the mock page (testdata/world_dom_mock.js, which boots garden.js and porch.js without a browser):
// grouping ("Needs you" first, then the plot, then the Meadow wildflower gd-gAway stands by, the barn last; gd-6FCoWelnx2),
// the unplaced bucket, the scope, the row, the pip, Go to plot / Go there and a sprite opening its row.
// testdata/porch_dom_test.js holds the assertions.
func TestPorchUnderNode(t *testing.T) {
	r := nodeScript(t, "porch_dom_test.js")
	if r["ok"] != true || r["groups"] != float64(4) || r["rows"] != float64(15) {
		t.Fatalf("porch DOM report: %v", r)
	}
}

// The Porch is its own module, loaded before garden.js, and the page keeps the pieces porch.js binds to.
func TestPorchModuleWiring(t *testing.T) {
	for _, want := range []string{`id="d-porch"`, `id="porch-empty"`, `id="porch-sub"`, `id="dpip-porch"`, `data-go="porch"`} {
		if !strings.Contains(pageHTML, want) {
			t.Errorf("garden.html is missing %s", want)
		}
	}
	if p, a := strings.Index(pageHTML, `{{asset "porch.js"}}`), strings.Index(pageHTML, `{{asset "garden.js"}}`); p < 0 || p > a {
		t.Error("garden.html must load porch.js before garden.js")
	}
	if !strings.Contains(pageAppJS, "PORCH.sync(") || !strings.Contains(pageAppJS, "PORCH.show(") {
		t.Error("garden.js no longer hands the Porch its gardeners")
	}
}

// A gardener's spend in words (gd-iSYItBAyA5): WORLD.costWords turns farm gardeners[].cost into the Porch row's chip, its
// words for a screen reader and the card's tip, "+" and "at least" while a session is still open (gd-UIlh0DR2VG), and no
// figure at all when none is known, never a made-up $0. PORCH.groups sums the rows' spend under the scope, so "Mine" is the
// viewer's own, and summary says it. Skipped when node is not installed.
func TestGardenerCostWordsUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var P = require(process.argv[1]), W = require(process.argv[2]), assert = require("assert");
var cost = function (spent, atLeast, text) { return { spent_usd: spent, live_usd: null, at_least: atLeast, gardener_usd: null, text: text || "" }; };
assert.deepStrictEqual(W.costWords({ cost: cost(3.2, true) }), { usd: 3.2, atLeast: true, chip: "$3.20+", label: "spent $3.20, at least" });
assert.deepStrictEqual(W.costWords({ cost: cost(12, false) }), { usd: 12, atLeast: false, chip: "$12.00", label: "spent $12.00" }, "a dormant gardener's spend is the whole of it");
assert.strictEqual(W.costWords({ cost: cost(null, true, "Nothing counted yet") }), null, "nothing counted: no figure");
assert.strictEqual(W.costWords({ cost: null }), null, "no cost known");
assert.strictEqual(W.costWords({}), null, "a farm document from before cost");

var g = function (id, op, c) { return { id: id, agent: id, state: "working", bucket: "alive", operator: op, since: 1, cost: c }; };
var at = function () { return null; };
var list = [g("a", "oidc:me", cost(4.1, true)), g("b", "oidc:me", cost(0.67, false)), g("c", "oidc:flo", cost(3.1, true)), g("d", "oidc:me", cost(null, true)), g("e", "oidc:flo", null)];
var all = P.groups(list, at, { me: "oidc:me" }), mine = P.groups(list, at, { me: "oidc:me", only: true });
assert.deepStrictEqual(all.cost, { usd: 7.87, atLeast: true }, "everyone's spend, in whole cents");
assert.deepStrictEqual(mine.cost, { usd: 4.77, atLeast: true }, "Mine sums only the viewer's own");
assert.strictEqual(P.summary(mine), "3 gardeners · 3 at work · spent at least $4.77");
var exact = P.groups([g("x", "oidc:me", cost(1.5, false)), g("y", "oidc:me", cost(2, false)), g("w", "oidc:me", cost(null, true))], at, {});
assert.strictEqual(P.summary(exact), "3 gardeners · 3 at work · spent $3.50", "every figure whole: no at least, not even from a row with no figure");
assert.strictEqual(P.groups([g("z", "oidc:me", cost(null, true))], at, {}).cost, null, "no row with a figure: no sum");
assert.strictEqual(P.summary(P.groups([g("z", "oidc:me", null)], at, {})), "1 gardener · 1 at work");

// the gardener's card: a cost line in the farm's words, and the tip says it short
var cg = { id: "gd-c", agent: "g-c", role: "crew", project: "gardenio", state: "working", bucket: "alive", since: 1, href: "/garden?bead=gd-c", reason: null,
  cost: cost(3.2, true, "Spent at least $3.20; this session is counted in full when it ends.") };
var card = W.gardenerCard(cg, null, null, "/b", function () { return "4m"; });
assert.deepStrictEqual(card.lines.filter(function (l) { return l[0] === "cost"; }), [["cost", "Spent at least $3.20; this session is counted in full when it ends."]]);
assert.ok(/ · 4m · spent \$3\.20, at least$/.test(card.tip), card.tip);
cg.cost = null;
card = W.gardenerCard(cg, null, null, "/b", function () { return "4m"; });
assert.ok(!card.lines.some(function (l) { return l[0] === "cost"; }) && !/spent/.test(card.tip), "no figure: no cost line, nothing in the tip");
`
	porch, err := filepath.Abs(filepath.Join("web", "porch.js"))
	if err != nil {
		t.Fatal(err)
	}
	world, err := filepath.Abs(filepath.Join("web", "world.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, porch, world).CombinedOutput()
	if err != nil {
		t.Fatalf("node gardener cost words test failed: %v\n%s", err, out)
	}
}
