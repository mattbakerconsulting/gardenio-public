package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// titles.js's pure half runs under node (titles, not ids: gd-9yOATV2qCs): a bead id is the workbench.js rule (a word like
// "re-introduced" is not one), text is cut into plain runs and ids with an id inside a URL left alone, a short title cuts
// at a word with an ellipsis, a known title reads in place of the id with the id kept in the tooltip, an unknown id reads
// as the id, and the farm document's titles are indexed by shape: plans, phases, tasks, waiting handles, seeds, the
// meadow and gardeners by their agent name, each by its short title where it has one (gd-QTukIA8Nvv).
func TestTitlesLogicUnderNode(t *testing.T) {
	script := `
var T = require(process.argv[1]), assert = require("assert");
assert.ok(T.isId("gd-9yOATV2qCs"));
assert.ok(T.isId("ka-0000001089"));
assert.ok(!T.isId("re-introduced"), "a word is not an id");
assert.ok(!T.isId("gd-9yOATV2qCs "), "the whole string");
assert.ok(!T.isId(""));
function kinds(s) { return T.split(s).map(function (r) { return r.k + ":" + r.s; }); }
assert.deepStrictEqual(kinds("see gd-9yOATV2qCs, then (gd-AbCdEfGh12)."), ["text:see ", "id:gd-9yOATV2qCs", "text:, then (", "id:gd-AbCdEfGh12", "text:)."]);
assert.deepStrictEqual(kinds("https://x.example/b/gd-9yOATV2qCs?x=1 and gd-9yOATV2qCs"), ["text:https://x.example/b/gd-9yOATV2qCs?x=1 and ", "id:gd-9yOATV2qCs"], "an id in a URL stays the URL's");
assert.deepStrictEqual(kinds("re-introduced the sprite"), ["text:re-introduced the sprite"]);
assert.deepStrictEqual(kinds("on agent/some-very-long-branch-name/gd-9yOATV2qCs for gd-9yOATV2qCs"), ["text:on agent/some-very-long-branch-name/gd-9yOATV2qCs for ", "id:gd-9yOATV2qCs"], "an id glued to a '/' is the path's");
assert.deepStrictEqual(kinds(""), []);
assert.deepStrictEqual(kinds(null), []);
assert.ok(T.hasId("waiting on gd-9yOATV2qCs"));
assert.ok(!T.hasId("waiting on the owner"));
assert.strictEqual(T.short("Plan A", 36), "Plan A", "a short title is whole");
assert.strictEqual(T.short("Titles, not ids: every gd-* shown in garden/lands text becomes the title", 36), "Titles, not ids: every gd-* shown…", "cut at a word");
assert.strictEqual(T.short("x".repeat(50), 10), "xxxxxxxxx…", "no word to cut at");
assert.strictEqual(Array.from(T.short("😀".repeat(40), 10)).length, 10, "graphemes, not UTF-16 units");
assert.strictEqual(T.short("  spaced \n  out  "), "spaced out", "whitespace collapsed");
assert.strictEqual(T.label("gd-9yOATV2qCs", "Titles, not ids"), "Titles, not ids");
assert.strictEqual(T.label("gd-9yOATV2qCs", ""), "gd-9yOATV2qCs", "an unknown id reads as the id");
assert.strictEqual(T.label("gd-9yOATV2qCs", "A long enough title to be cut short here", 20), "A long enough title…");
assert.strictEqual(T.tip("gd-9yOATV2qCs", "Titles"), "Titles (gd-9yOATV2qCs)", "the id stays in the tooltip");
assert.strictEqual(T.tip("gd-9yOATV2qCs", ""), "gd-9yOATV2qCs");
var ix = T.index({
  plans: [{ id: "gd-PlanAAAAA1", title: "Plan A: a steady title bar", short_title: " Plan A ", waiting: [{ id: "gd-DecisionA1", title: "Which way?", short_title: "" }],
    phases: [{ id: "gd-PhaseAAAA1", title: "Phase one", tasks: [{ id: "gd-TaskAAAAA1", title: "Step one" }] }], unphased: { tasks: [{ id: "gd-TaskBBBBB2", title: "Loose step" }] } }],
  seeds: [{ id: "gd-SeedAAAAA1", title: "A seed" }],
  meadow: { shown: [{ id: "gd-MeadowAAA1", title: "A flower" }] },
  gardeners: [{ id: "gd-AgentAAAA1", agent: "crew-one", task: "gd-TaskAAAAA1" }],
  plain: [{ id: "not an id", title: "skipped" }, { id: "gd-NoTitleAA1" }, { id: "gd-SameAsIdA1", title: "gd-SameAsIdA1" }]
});
assert.deepStrictEqual(ix, {
  "gd-PlanAAAAA1": "Plan A", "gd-DecisionA1": "Which way?", "gd-PhaseAAAA1": "Phase one", "gd-TaskAAAAA1": "Step one", "gd-TaskBBBBB2": "Loose step",
  "gd-SeedAAAAA1": "A seed", "gd-MeadowAAA1": "A flower", "gd-AgentAAAA1": "crew-one"
});
assert.deepStrictEqual(T.index(null), {});
`
	runNodeLogic(t, "titles.js", script)
}

// TITLES.name reads a farm record's short title when it has one, else its title, else its id; TITLES.chain reads a task
// back to its plan and phase through the map's own layout index (WORLD.layout), with no search over the farm: the cursor
// bar's Plan › Phase › Task (gd-MpWy5X34hh).
func TestTitlesNameAndChainUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var T = require(process.argv[1]), W = require(process.argv[2]), assert = require("assert");
assert.strictEqual(T.name({ id: "gd-TaskAAAAA1", title: "Carry short titles in the farm document", short_title: "carry short titles" }), "carry short titles", "the short title when present");
assert.strictEqual(T.name({ id: "gd-TaskAAAAA1", title: "Carry short titles", short_title: "" }), "Carry short titles", "else the title");
assert.strictEqual(T.name({ id: "gd-TaskAAAAA1", title: "Carry short titles", short_title: "   " }), "Carry short titles", "a blank short title is none");
assert.strictEqual(T.name({ id: "gd-TaskAAAAA1", title: "Carry short titles" }), "Carry short titles", "a document without the key");
assert.strictEqual(T.name({ id: "gd-TaskAAAAA1" }), "gd-TaskAAAAA1", "else the id");
assert.strictEqual(T.name(null), "");
var doc = { plans: [{ id: "gd-PlanAAAAA1", title: "Lands UX affordances: a steady title bar", short_title: "Lands UX affordances", project: "gardenio", created: 1, activity: 1,
    counts: { in_progress: 1, open: 1, closed: 1 },
    phases: [{ id: "gd-PhaseAAAA1", title: "Phase one, in full", short_title: "phase one", counts: { in_progress: 1, closed: 1 },
      tasks: [{ id: "gd-TaskAAAAA1", title: "Step one, in full", short_title: "step one", type: "task", state: "in_progress", created: 2, updated: 2 }],
      closed: [{ id: "gd-TaskCCCCC3", title: "A closed step", type: "task", state: "closed", created: 1, updated: 1 }] },
      { id: "gd-PhaseBBBB2", title: "Phase two", counts: {}, tasks: [] }],
    unphased: { id: "", title: "Other", counts: { open: 1 }, tasks: [{ id: "gd-TaskBBBBB2", title: "A loose step", type: "task", state: "open", created: 3, updated: 3 }] } }],
  seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } };
var ix = W.layout(doc, null, { now: 1, readonly: true }).index;
function parts(id) { return T.chain(ix, id).map(function (p) { return p.kind + ":" + p.id + ":" + p.name; }); }
assert.deepStrictEqual(parts("gd-TaskAAAAA1"), ["plan:gd-PlanAAAAA1:Lands UX affordances", "phase:gd-PhaseAAAA1:phase one", "task:gd-TaskAAAAA1:step one"], "plan › phase › task, short titles");
assert.deepStrictEqual(parts("gd-TaskCCCCC3"), ["plan:gd-PlanAAAAA1:Lands UX affordances", "phase:gd-PhaseAAAA1:phase one", "task:gd-TaskCCCCC3:A closed step"], "a closed step has its chain, its title with no short title");
assert.deepStrictEqual(parts("gd-TaskBBBBB2"), ["plan:gd-PlanAAAAA1:Lands UX affordances", "task:gd-TaskBBBBB2:A loose step"], "an unphased step: plan › task");
assert.deepStrictEqual(parts("gd-PlanAAAAA1"), ["plan:gd-PlanAAAAA1:Lands UX affordances"], "a plot is its plan");
assert.strictEqual(T.chain(ix, "gd-TaskAAAAA1")[0].title, "Lands UX affordances: a steady title bar", "the title rides along for the tooltip");
assert.deepStrictEqual(T.chain(ix, "gd-NotThere01"), [], "an id not on the map");
assert.deepStrictEqual(T.chain(ix, "project:gardenio"), [], "a biome has no chain");
assert.deepStrictEqual(T.chain(null, "gd-TaskAAAAA1"), []);
`
	titles, err := filepath.Abs(filepath.Join("web", "titles.js"))
	if err != nil {
		t.Fatal(err)
	}
	world, err := filepath.Abs(filepath.Join("web", "world.js"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "-e", script, titles, world).CombinedOutput(); err != nil {
		t.Fatalf("node titles name/chain test failed: %v\n%s", err, out)
	}
}

// titles.js is inlined before every renderer that calls it, and the renderers that put bead ids in front of a person
// route them through it: the Porch, the toasts, the bead view's free text; and the workbench (commons2.js, on its own page).
func TestTitlesWiredIntoRenderers(t *testing.T) {
	_, rec := servedPage(t)
	body := inlined(t, rec.Body.String())
	at := strings.Index(body, "var TITLES = ")
	if at < 0 {
		t.Fatal("web/titles.js is not inlined in the page")
	}
	for _, later := range []string{"var REASON = ", "var BEAD = ", "var PORCH = "} {
		if i := strings.Index(body, later); i >= 0 && i < at {
			t.Errorf("titles.js must load before %s", strings.TrimSuffix(later, " = "))
		}
	}
	for name, src := range map[string]string{"commons2.js": pageCommons2JS, "porch.js": pagePorchJS, "chat.js": pageChatJS, "garden.js": pageAppJS, "workbench.js": pageWorkbenchJS} {
		if !strings.Contains(src, "TITLES.") {
			t.Errorf("%s does not route its bead ids through TITLES", name)
		}
	}
}
