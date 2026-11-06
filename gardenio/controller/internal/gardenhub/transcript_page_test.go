package gardenhub

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// One gardener's transcript on the page (bird-captions spec 11.4, gd-kl2CL3X9jQ). The row formatting is ACTIVITY's, pure under
// node, so the plot and farm panel after it says a row the same way; the panel's own pure half (web/transcript.js) merges the
// live rows into the page it read without repeating one, counts the classes for the filter and says the summary line. Skipped
// when node is not installed.
func TestTranscriptJSUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
process.env.TZ = "UTC";   // the row's clock is the page's
var A = require(process.argv[1]), T = require(process.argv[2]), assert = require("assert");
// a row: its clock, its caption (whitespace collapsed) or the tool word, its duration, a failure and an unfinished call
assert.deepStrictEqual(A.callRow({ at: 1792870452, class: "shell", tool: "other", cap: "  Trial-merge main\n into #1282…", dur: 5120, ok: true }),
  { class: "shell", time: "19:34:12", words: "Trial-merge main into #1282…", dur: "5.1 s", failed: false, open: false });
assert.deepStrictEqual(A.callRow({ at: 0, class: "shell", tool: "other", dur: 820, ok: false }),
  { class: "shell", time: "00:00:00", words: "a shell command", dur: "820 ms", failed: true, open: false });
assert.deepStrictEqual([A.callRow({ at: 0, class: "test", tool: "go_test", dur: 0 }).words, A.callRow({ at: 0, class: "test", tool: "go_test", dur: 0 }).open], ["go test", true]);
assert.strictEqual(A.callRow({ at: 0, class: "delegate", tool: "Agent", dur: 1 }).words, "a subagent");
// a say row (what the gardener said): its words, no duration, and not unfinished though it has no ok
assert.deepStrictEqual(A.callRow({ at: 0, class: "say", tool: "", cap: "Pushed the fix; CI is green.", dur: 0 }),
  { class: "say", time: "00:00:00", words: "Pushed the fix; CI is green.", dur: "", failed: false, open: false });
assert.strictEqual(A.callRow({ at: 0, class: "", tool: "", dur: 1 }).words, "a call");
assert.deepStrictEqual([0, -1, 999, 1000, 59949, 60000, 124000, 120400].map(A.durWords), ["", "", "999 ms", "1 s", "59.9 s", "1 min", "2 min 4 s", "2 min"]);
// merge: live rows in front, newest first; one already held (no id on either side) is not repeated; at most MAX
var r = function (at, c, cap) { return { at: at, class: c, tool: "t", cap: cap || "", dur: 10, ok: true }; };
var page = [r(30, "read"), r(20, "edit"), r(10, "read")];
assert.deepStrictEqual(T.merge(page, [r(40, "git"), r(30, "read")]).map(function (x) { return x.at; }), [40, 30, 20, 10]);
assert.deepStrictEqual(T.merge(page, [r(30, "read", "a caption")]).length, 4, "a row that differs is its own");
var many = []; for (var i = 0; i < T.MAX + 5; i++) many.push(r(i, "read"));
assert.strictEqual(T.merge([], many).length, T.MAX);
// the filter: classes, the commonest first; kept rows; the summary
assert.deepStrictEqual(T.classes(page), [{ class: "read", n: 2 }, { class: "edit", n: 1 }]);
assert.deepStrictEqual(T.kept(page, "edit"), [page[1]]); assert.strictEqual(T.kept(page, ""), page);
assert.strictEqual(T.summary(3, 3, ""), "Its last 3 calls, newest first.");
assert.strictEqual(T.summary(1, 1, ""), "Its last call, newest first.");
assert.strictEqual(T.summary(2, 3, "read"), "2 of its last 3 calls were read, newest first.");
assert.strictEqual(T.summary(0, 0, ""), "No calls yet: a row is written when a call finishes.");
assert.strictEqual(T.url("gd-a b"), "/garden/farm/activity/gd-a%20b");
// a plot's or the farm's (spec 11.5, gd-X5dM2VRgDd): a scope, its route, paged by cursor only
assert.deepStrictEqual(["farm", "project:gardenio", "plot:gd-p1", "gd-a", "project:", "plot:", "farmer"].map(T.scoped), [true, true, true, false, false, false, false]);
assert.strictEqual(T.url("plot:gd-p1"), "/garden/farm/activity?scope=plot%3Agd-p1");
assert.strictEqual(T.url("farm", "o1792870452.8812"), "/garden/farm/activity?scope=farm&cursor=o1792870452.8812");
assert.strictEqual(T.url("gd-a", "n1.2"), "/garden/farm/activity/gd-a?cursor=n1.2");
// the merge order across gardeners: newest first by start; within one second the newer side (the first list) first; a row of
// one gardener is not another's; a long call that finished late lands at its start, among older rows
var g = function (id, at, c) { return { id: id, at: at, class: c, tool: "t", dur: 10, ok: true }; };
var held = [g("gd-a", 50, "read"), g("gd-b", 50, "edit"), g("gd-a", 40, "git"), g("gd-c", 20, "test")];
var live = [g("gd-c", 60, "shell"), g("gd-b", 50, "read"), g("gd-a", 50, "read"), g("gd-d", 30, "web")];
assert.deepStrictEqual(T.merge(live, held, T.HOLD).map(function (r) { return r.id + "@" + r.at + ":" + r.class; }),
  ["gd-c@60:shell", "gd-b@50:read", "gd-a@50:read", "gd-b@50:edit", "gd-a@40:git", "gd-d@30:web", "gd-c@20:test"]);
var older = [g("gd-c", 20, "test"), g("gd-e", 20, "read"), g("gd-a", 5, "read")];
assert.deepStrictEqual(T.merge(held, older, T.HOLD).map(function (r) { return r.id + "@" + r.at; }), ["gd-a@50", "gd-b@50", "gd-a@40", "gd-c@20", "gd-e@20", "gd-a@5"], "an older page goes after, its overlap dropped");
var lots = []; for (var j = 0; j < 450; j++) lots.push(g("gd-" + (j % 60), 1000 - j, "read"));
var cut = T.merge(lots.slice(0, 100), lots.slice(100), T.HOLD);
assert.strictEqual(cut.length, T.HOLD); assert.strictEqual(cut[T.HOLD - 1].at, 1000 - T.HOLD + 1, "the oldest go past HOLD");
// where, and the scope's summary line
assert.deepStrictEqual(["farm", "project:gardenio", "plot:gd-p1", "gd-a"].map(T.where), ["on the farm", "in gardenio", "on this plot", ""]);
assert.strictEqual(T.summary(100, 100, "", "on this plot", "14:02:11"), "The last 100 calls on this plot, since 14:02:11, newest first.");
assert.strictEqual(T.summary(4, 100, "test", "on the farm", "14:02:11"), "4 of the last 100 calls on the farm were test, since 14:02:11, newest first.");
assert.strictEqual(T.summary(0, 0, "", "in gardenio", ""), "No calls in gardenio yet: a row is written when a call finishes.");
// each row's gardener: its name, and its plot's arms
var w = T.farmWho({ plans: [{ id: "gd-p1", heraldry: "🔥🧭 blue", heraldry_name: "arms on blue: fire, compass", project: "gardenio" }],
  gardeners: [{ id: "gd-a", agent: "g-work-1", plot: { plan: "gd-p1", phase: "" } }, { id: "gd-b", agent: "g-home", plot: null }] });
assert.deepStrictEqual(w.who["gd-a"], { name: "g-work-1", plan: "gd-p1", arms: "🔥🧭 blue", armsName: "arms on blue: fire, compass" });
assert.deepStrictEqual(w.who["gd-b"], { name: "g-home", plan: "", arms: "", armsName: "" });
assert.strictEqual(w.plans["gd-p1"].project, "gardenio");
assert.deepStrictEqual(T.farmWho(null), { who: {}, plans: {} });
`
	var args []string
	for _, f := range []string{"activity.js", "transcript.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	if out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("node transcript test failed: %v\n%s", err, out)
	}
}

// The panel is wired to what it needs: the farm stream hands each `call` event over (web/plans.js), t at the cursor is its act
// (web/curnav.js, then BIRDS.wire's watch), the Workbench knows the kind and the page loads it after the Glass, whose page
// links to it, as the agent bead's face does. Its text goes in through textContent only.
func TestTranscriptPageWiring(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tr := read("transcript.js")
	for _, want := range []struct{ file, text string }{
		{"plans.js", `['hello', 'busy', 'doing', 'call']`},
		{"curnav.js", `return { act: "transcript" }`},
		{"garden.js", `watch(ae,a.act==='transcript')`},
		{"birds.js", `WORKBENCH.open("transcript", id)`},
		{"workbench.js", `'pic', 'transcript', `},
		{"workbench.js", `'Read its transcript', 'transcript', this.id`},
		{"glass.js", `W.withPanel('', 'transcript', id)`},
		{"transcript.js", `W.register('transcript'`},
		{"transcript.js", `transcript: id`},
		{"transcript.js", `catchUp(3)`},
		{"workbench.js", `'transcript', 'plot:' + this.id`},
		{"project.js", `W.href('transcript', 'project:' + cur())`},
		{"garden.js", `farm:function(){return farmDoc;}`},
	} {
		if !strings.Contains(read(want.file), want.text) {
			t.Errorf("web/%s lacks %q", want.file, want.text)
		}
	}
	for _, banned := range []string{"innerHTML", "insertAdjacentHTML", "document.write", "before=", "since="} {
		if strings.Contains(stripJSComments(tr), banned) {
			t.Errorf("web/transcript.js carries %q: text goes in through textContent, and it pages on no unix second", banned)
		}
	}
	if !strings.Contains(pageAssets.byName["page-workbench.js"].src, pageTranscriptJS) {
		t.Error("the page must load transcript.js in the Workbench's bundle, which every viewer loads")
	}
}
