package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What a gardener is doing, on the page (gd-H4jk9KgUAG). activity.js is pure under node: the calls an event says started
// (its n when it folds several), the held doing moved on by each event and seeded from the document without a patch's
// doing-less entries wiping it, waiting after 5 s, and the card's line. Then the card leads its activity with it. Skipped
// when node is not installed.
func TestActivityJSUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var listeners = {};
global.document = { addEventListener: function (t, fn) { listeners[t] = fn; } };   // so activity.js keeps a held map, and an event moves it on
var document_fire = function (id, ev) { listeners.farmactivity({ detail: ev }); };
var A = require(process.argv[1]), W = require(process.argv[2]), assert = require("assert");
// calls started: n when folded, else 1 for a call's start and 0 for a finish, a thought, a subagent
assert.deepStrictEqual([
  { doing: "read", tool: "Read", dur: 0 }, { doing: "read", tool: "Read", dur: 12 }, { doing: "think", tool: "", dur: 0 },
  { doing: "delegate", tool: "subagent", dur: 0 }, { doing: "delegate", tool: "Agent", dur: 0 }, { doing: "read", tool: "Read", dur: 9, n: 3 }, { doing: "edit", tool: "Edit", dur: 0, n: 0 }
].map(A.started), [1, 0, 0, 0, 1, 3, 0]);
// held: counts calls, open while a call is out
var h = A.next(null, { id: "gd-a", doing: "test", tool: "go_test", at: 100, dur: 0 });
assert.deepStrictEqual(h, { class: "test", tool: "go_test", at: 100, calls: 1, open: true });
h = A.next(h, { id: "gd-a", doing: "test", tool: "go_test", at: 104, dur: 4000 });
assert.deepStrictEqual([h.calls, h.open, h.at], [1, false, 104]);
// the card's line
assert.strictEqual(A.line(h, 108), "doing: go test · 4 s ago · 1 call this session");
assert.strictEqual(A.line({ class: "git", tool: "git_other", at: 0, calls: 212, open: false }, 125), "doing: git · 2 min ago · 212 calls this session");
assert.strictEqual(A.line({ class: "shell", tool: "other", at: 0, calls: 2 }, 1), "doing: a shell command · 1 s ago · 2 calls this session");
assert.strictEqual(A.line({ class: "read", tool: "Read", at: 100, calls: 3, open: true }, 104), "doing: Read · 4 s ago · 3 calls this session");
assert.strictEqual(A.line({ class: "read", tool: "Read", at: 100, calls: 3, open: true }, 105), "doing: waiting on Read · 5 s ago · 3 calls this session");
assert.strictEqual(A.line({ class: "think", tool: "", at: 100, calls: 3 }, 100), "doing: thinking · 0 s ago · 3 calls this session");
// a say (what the gardener said, gd-HHGjxt59Mr): one bird, no call counted, never open, "saying" on the line
assert.strictEqual(A.started({ doing: "say", tool: "", dur: 0, cap: "Pushed the fix." }), 1);
var hs = A.next({ class: "test", tool: "go_test", at: 100, calls: 3, open: false }, { id: "gd-a", doing: "say", tool: "", at: 110, dur: 0, cap: "Pushed the fix." });
assert.deepStrictEqual(hs, { class: "say", tool: "", at: 110, calls: 3, open: false });
assert.strictEqual(A.line(hs, 120), "doing: saying · 10 s ago · 3 calls this session");
assert.strictEqual(A.word(hs, 120), "saying");
assert.strictEqual(A.line({ class: "delegate", tool: "subagent", at: 0, calls: 3, open: true }, 7200), "doing: a subagent · 2 h ago · 3 calls this session");
assert.strictEqual(A.line(null, 1), "");
// seed: the document's doing where newer; a patch's entry (no doing) keeps what is held; a gardener gone is dropped
var held = { "gd-a": { class: "edit", tool: "Edit", at: 200, calls: 5, open: false }, "gd-gone": { class: "read", tool: "Read", at: 1, calls: 1 } };
var s = A.seed(held, { gardeners: [{ id: "gd-a", doing: { class: "read", tool: "Read", at: 150, calls: 4 } }, { id: "gd-b", doing: { class: "waiting", tool: "Bash", at: 90, calls: 9 } }, { id: "gd-c" }] });
assert.deepStrictEqual(Object.keys(s).sort(), ["gd-a", "gd-b"]);
assert.strictEqual(s["gd-a"].class, "edit", "held is newer than the document");
assert.deepStrictEqual([s["gd-b"].class, s["gd-b"].open, s["gd-b"].calls], ["waiting", true, 9]);
assert.strictEqual(A.seed(s, { gardeners: [{ id: "gd-a" }] })["gd-a"].calls, 5, "a patch's entry has no doing: what is held stays");
// the pose (4c): by class, the hoe ("") for edit and shell; waiting after 5 s; a subagent out until it is back; stale after STALE_S
var at = function (c, o) { return { class: c, tool: "", at: 100, calls: 1, open: !!o }; };
assert.deepStrictEqual(["read", "edit", "shell", "test", "build", "git", "beads", "deploy", "web", "think", "nonsense"].map(function (c) { return A.pose(at(c), 101); }),
  ["read", "", "", "test", "build", "git", "beads", "deploy", "web", "think", ""]);
assert.strictEqual(A.pose(at("test", true), 104), "test"); assert.strictEqual(A.pose(at("test", true), 105), "waiting");
assert.strictEqual(A.pose(at("delegate", true), 100 + 3600), "delegate", "a subagent out is never stale nor waiting");
assert.strictEqual(A.pose(at("delegate", false), 101), "", "a subagent back: the hoe");
assert.strictEqual(A.pose(at("read"), 100 + A.STALE_S - 1), "read"); assert.strictEqual(A.pose(at("read"), 100 + A.STALE_S), "", "stale: the hoe");
assert.strictEqual(A.pose(null, 1), "");
assert.strictEqual(A.seed({}, { gardeners: [{ id: "gd-d", doing: { class: "delegate", tool: "subagent", at: 1, calls: 2 } }] })["gd-d"].open, true, "the document's delegate is a subagent out");
// ... and in words, for the outline: every class has one
assert.deepStrictEqual(["read", "edit", "shell", "test", "build", "git", "beads", "deploy", "web", "delegate", "think"].map(function (c) { return A.word(at(c, c === "delegate"), 101); }),
  ["reading", "editing", "running a command", "running tests", "building", "using git", "writing beads", "deploying", "on the web", "delegating to a subagent", "thinking"]);
assert.strictEqual(A.word(at("test", true), 106), "waiting on a tool"); assert.strictEqual(A.word(at("read"), 100 + A.STALE_S), "");
// the map: one at work wears what it does; a reason's pose (or not at work) wins; the outline says the word
var gw = { id: "gd-w", agent: "w", role: "crew", project: "gardenio", state: "working", bucket: "alive", since: 1, href: "/garden?bead=gd-w" };
var gp = { id: "gd-p", agent: "p", role: "crew", project: "gardenio", state: "working", bucket: "alive", since: 1, href: "/garden?bead=gd-p", reason: { kind: "parked", text: "Waiting on you" } };
var gi = { id: "gd-i", agent: "i", role: "crew", project: "gardenio", state: "idle", bucket: "alive", since: 1, href: "/garden?bead=gd-i" };
var st = W.gardenerState({ biomes: [], index: {} }, [gw, gp, gi]), now = Date.now() / 1000;
assert.strictEqual(st.lookOf("gd-w").pose, "work", "nothing heard: the hoe");
[gw, gp, gi].forEach(function (g) { document_fire(g.id, { id: g.id, doing: "git", tool: "git_push", at: now, dur: 0 }); });
assert.deepStrictEqual([st.lookOf("gd-w").pose, st.lookOf("gd-p").pose, st.lookOf("gd-i").pose], ["git", "ask", "stand"]);
assert.strictEqual(st.lookOf("gd-w"), st.lookOf("gd-w"), "one look object per pose, not one per frame");
assert.deepStrictEqual([W.doingWord(st, "gd-w"), W.doingWord(st, "gd-p"), W.doingWord(null, "gd-w")], ["using git", "", ""]);
// the card: with nothing heard, no doing line
var g = { id: "gd-z", agent: "z", role: "crew", project: "gardenio", state: "working", bucket: "alive", since: 1, href: "/garden?bead=gd-z", reason: null };
var card = W.gardenerCard(g, null, null, "/b", function () { return "4m"; });
assert.ok(!card.lines.some(function (l) { return l[0] === "act"; }) && !/doing:/.test(card.tip), card.tip);
`
	var args []string
	for _, f := range []string{"activity.js", "world.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node activity test failed: %v\n%s", err, out)
	}
}

// activity.js loads before world.js, which reads ACTIVITY as it loads, and plans.js hands the stream's activity events over.
func TestActivityJSOnThePage(t *testing.T) {
	a, w := strings.Index(pageHTML, `{{asset "activity.js"}}`), strings.Index(pageHTML, `{{asset "world.js"}}`)
	if a < 0 || w < 0 || a > w {
		t.Errorf("activity.js at %d, world.js at %d: activity.js must load first", a, w)
	}
	if !strings.Contains(pagePlansJS, "addEventListener('activity'") || !strings.Contains(pagePlansJS, "'farmactivity'") {
		t.Error("plans.js does not hand the activity events over")
	}
}
