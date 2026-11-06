package gardenhub

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestStakedDeepLink: a staked plot is reached by its plan's id (homestead-world-design.md section 7 and 26.14, plan
// gd-2RdeAx8nnW step 3). ?at=<project>/<plan id> resolves to the plot at the Plan level, and its steps to their cells by
// step id; the cursor's tree finds the same plot and writes the same at= back; ?bead=<plan id> names the plan; and the
// plot's name in the title bar is the plan's short title (its title when it has none) with "staked" in its status line. Once
// approved the same at= is the planted plot.
func TestStakedDeepLink(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), C = require(process.argv[2]), assert = require("assert");
function plan(id, created) { return { id: id, title: "Plan " + id, project: "gardenio", created: created, activity: created, counts: { open: 1 }, phases: [{ id: id + "-p1", title: "Phase 1", counts: { open: 1 }, tasks: [{ id: id + "-t0", title: "T0", type: "task", state: "open", updated: 1 }] }], unphased: { id: "", title: "Other", counts: {}, tasks: [] } }; }
var seed = { id: "gd-stake1", title: "Staked plots: a plan waiting on your answer is laid out in the fields", short_title: "stake out asked plans", project: "gardenio", created: 20, updated: 20, state: "asked", status: "open", phases: 0, tasks: 2,
  steps: [{ id: "gd-st1", title: "Read model", n: 1, status: "open" }, { id: "gd-st2", title: "World layout", n: 2, status: "open" }], summary: "The narrative.",
  decision: { id: "gd-ask", prompt: "Run it?", options: [{ id: "approve", short: "Run the plan" }] } };
var bare = { id: "gd-stake2", title: "No short title here", project: "gardenio", created: 30, updated: 30, state: "ready", status: "open", phases: 0, tasks: 0, steps: [] };
function doc(plans, seeds) { return { plans: plans, seeds: seeds, closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] }, meta: { truncated: false } }; }
var w = W.layout(doc([plan("gd-plan1", 10)], [seed, bare]), null, { now: 1 });

// ?at=<project>/<plan id>: the staked plot, at the Plan level
var r = W.resolve(w, "gardenio/gd-stake1");
assert.deepStrictEqual([r.kind, r.id, r.level, r.found, r.biome.key], ["plot", "gd-stake1", 2, true, "gardenio"]);
var p = w.index["gd-stake1"]; assert.ok(p.staked); assert.deepStrictEqual(r.rect, { x: p.x, y: p.y, w: p.w, h: p.h });
assert.strictEqual(W.resolve(w, "gd-stake1").id, "gd-stake1", "a bare plan id too");
assert.strictEqual(W.atFor(w, "plot", "gd-stake1"), "gardenio/gd-stake1");
// a step is its cell, by step id
r = W.resolve(w, "gardenio/gd-st2"); assert.deepStrictEqual([r.kind, r.id, r.level, r.found], ["task", "gd-st2", 3, true]);
// ?at= and ?bead= in one link: the query keeps both
assert.strictEqual(W.queryGet("?at=gardenio/gd-stake1&bead=gd-stake1", "at"), "gardenio/gd-stake1");
assert.strictEqual(W.queryGet("?at=gardenio/gd-stake1&bead=gd-stake1", "bead"), "gd-stake1");

// the cursor: the same plot from the at=, and the same at= back
var t = C.tree(w, null, { now: 1000 }), path = C.pathFor(t, "gardenio/gd-stake1");
assert.deepStrictEqual(path, ["project:gardenio", "gd-stake1"]);
assert.strictEqual(C.atFor(t, path), "gardenio/gd-stake1");
assert.strictEqual(C.pathFor(t, "gardenio/gd-st1").pop(), "gd-st1", "a step's cell");

// its name in the title bar: the short title, and "staked" in its status line (gd-zDdiA3HBh5: no label beside it)
var n = t.nodes["gd-stake1"];
assert.strictEqual(n.title, "stake out asked plans");
assert.strictEqual(C.statusOf(n).status, "1 needs you · staked · 2 steps", "the need words first, then staked");
assert.ok(!C.signs(t, path, { w: 390, h: 844, coarse: true }).some(function (s) { return s.id === "gd-stake1"; }), "no sign for the cursor's own plot");
var n2 = t.nodes["gd-stake2"];
assert.deepStrictEqual([n2.title, C.statusOf(n2).status], ["No short title here", "staked · 0 steps"], "no short title: the title");
// a planted plot's is as it was: its title, and no "staked"
var n1 = t.nodes["gd-plan1"];
assert.deepStrictEqual([n1.title, C.statusOf(n1).status], ["Plan gd-plan1", "phase 1 of 1 · 0/1 steps"]);

// approved: the same at= is the planted plot, in the same place
var planted = plan("gd-stake1", 20), w2 = W.layout(doc([plan("gd-plan1", 10), planted], [bare]), JSON.parse(JSON.stringify(w.memo)), { now: 2 });
r = W.resolve(w2, "gardenio/gd-stake1");
assert.deepStrictEqual([r.kind, r.found, !!w2.index["gd-stake1"].staked, r.rect], ["plot", true, false, { x: p.x, y: p.y, w: p.w, h: p.h }]);
`
	world, err := filepath.Abs(filepath.Join("web", "world.js"))
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := filepath.Abs(filepath.Join("web", "cursor.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, world, cursor).CombinedOutput()
	if err != nil {
		t.Fatalf("node staked deep link test failed: %v\n%s", err, out)
	}
}
