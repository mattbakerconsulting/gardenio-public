package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The page's side of the viewport cut (gd-fBGGNdgrLc): viewcut.js names the gardeners in view and a margin round it (none
// below the Plan level), POSTs that for the stream its hello named, once per change and at most every WAIT ms, and says it
// again on a new stream; activity.js takes a `doing` event and birds.js a `busy` one. Skipped when node is not installed.
func TestViewCutJSUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var listeners = {}, posts = [];
global.document = { body: { dataset: { base: "/v" } }, addEventListener: function (t, fn) { (listeners[t] = listeners[t] || []).push(fn); } };
global.fetch = function (url, o) { posts.push({ url: url, body: JSON.parse(o.body) }); return Promise.resolve({ ok: true }); };
var fire = function (t, d) { (listeners[t] || []).forEach(function (fn) { fn({ detail: d }); }); };
var V = require(process.argv[1]), A = require(process.argv[2]), B = require(process.argv[3]), assert = require("assert");

// want: below the Plan level no ids; from it, the shown gardeners in the view or its margin, sorted by id
var spots = { "gd-in": { x: 50, y: 50, shown: true }, "gd-edge": { x: 120, y: 50, shown: true }, "gd-far": { x: 200, y: 50, shown: true },
  "gd-hidden": { x: 50, y: 50, shown: false }, "gd-home": { home: true } };
var gs = { 2: { spots: spots }, 3: { spots: spots } }, view = { x: 0, y: 0, w: 100, h: 100 };
assert.deepStrictEqual(V.want({ level: 2, view: view, gs: gs }), { level: 2, ids: ["gd-edge", "gd-in"] }, "the margin is a quarter of the view each side");
assert.deepStrictEqual(V.want({ level: 1, view: view, gs: gs }), { level: 1, ids: [] });
assert.deepStrictEqual(V.want({ level: 0, view: view, gs: null }), { level: 0, ids: [] });
assert.strictEqual(V.key({ level: 0, ids: [] }), V.key({ level: 1, ids: ["x"] }), "below Plan the level alone is the view");
assert.notStrictEqual(V.key({ level: 2, ids: ["a"] }), V.key({ level: 2, ids: ["a", "b"] }));
// the field stands (5b, 28.4, 28.12): the gardeners at home while their project's Growing field meets the grown view, none once
// it is out; one out on a plot counts from its plot spot only
var bw = { index: { "project:a": { home: { cells: { growing: { x: 2, y: 2, w: 8, h: 12 } } } }, "project:b": { home: { cells: { growing: { x: 400, y: 400, w: 8, h: 12 } } } } } };
spots["gd-out"] = { plot: "pz", shown: false };
var gh = { 2: { spots: spots, fields: { "project:a": ["gd-home", "gd-out"], "project:b": ["gd-homeb"] } }, world: bw }, far = { x: 4000, y: 4000, w: 100, h: 100 };
assert.deepStrictEqual(V.want({ level: 2, view: view, gs: gh }).ids, ["gd-edge", "gd-home", "gd-in"], "a's field is in view: its stands of those at home too, never b's");
far.x = 3000; far.spots = null; spots["gd-farplot"] = { x: 3050, y: 4050, shown: true };
assert.deepStrictEqual(V.want({ level: 2, view: far, gs: gh }).ids, ["gd-farplot"], "both fields out of view: only the plot's");
assert.deepStrictEqual(V.want({ level: 2, view: { x: 6300, y: 6300, w: 100, h: 100 }, gs: gh }).ids, ["gd-homeb"], "b's field in view: its own stands");
// the cap keeps the nearest the middle
var many = {}; for (var i = 0; i < V.CAP + 10; i++) many["gd-" + (1000 + i)] = { x: 50 + i * 0.01, y: 50, shown: true };
var w = V.want({ level: 3, view: view, gs: { 3: { spots: many } } });
assert.strictEqual(w.ids.length, V.CAP); assert.ok(w.ids.indexOf("gd-1000") >= 0 && w.ids.indexOf("gd-" + (1000 + V.CAP + 9)) < 0);

// the page: nothing is posted before the hello; then at once; then once per change, at most every WAIT ms
V.see({ level: 2, view: view, gs: gs });
assert.strictEqual(posts.length, 0, "no stream named yet");
fire("farmhello", { sid: "s1" });
assert.deepStrictEqual(posts, [{ url: "/v/garden/farm/events/view", body: { sid: "s1", level: 2, ids: ["gd-edge", "gd-in"] } }]);
V.see({ level: 2, view: view, gs: gs });
V.see({ level: 2, view: { x: 1, y: 0, w: 100, h: 100 }, gs: gs });
V.see({ level: 1, view: { x: 2, y: 0, w: 100, h: 100 }, gs: gs });
V.see({ level: 0, view: { x: 3, y: 0, w: 100, h: 100 }, gs: gs });
setTimeout(function () {
  assert.strictEqual(posts.length, 2, "a pan and a zoom out within WAIT are one POST: " + JSON.stringify(posts));
  assert.deepStrictEqual(posts[1].body, { sid: "s1", level: 0, ids: [] });
  fire("farmhello", { sid: "s2" });
  assert.strictEqual(posts.length, 3, "a new stream is told the view at once");
  assert.strictEqual(posts[2].body.sid, "s2");

  // activity.js: a doing event as a gardener comes into view, unless what is held is newer
  fire("farmactivity", { id: "gd-a", doing: "edit", tool: "Edit", at: 200, dur: 0 });
  fire("farmdoing", { id: "gd-a", class: "read", tool: "Read", at: 150, calls: 7 });
  assert.strictEqual(A.of("gd-a").class, "edit", "an older doing does not win");
  fire("farmdoing", { id: "gd-a", class: "test", tool: "go_test", at: 300, calls: 9 });
  assert.deepStrictEqual(A.of("gd-a"), { class: "test", tool: "go_test", at: 300, calls: 9, open: false });
  // birds.js: a busy event marks its projects busy, as long ago as it says
  B.pin(1e9);
  fire("farmbusy", { busy: { alpha: 0, beta: 29, gamma: 40 } });
  assert.strictEqual(B.busySig(), "alpha,beta");
  B.pin(1e9 + 2000);
  assert.strictEqual(B.busySig(), "alpha", "beta was busy 31 s ago");
}, V.WAIT + 50);
`
	var args []string
	for _, f := range []string{"viewcut.js", "activity.js", "birds.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node viewcut test failed: %v\n%s", err, out)
	}
}

// viewcut.js loads before garden.js, which calls it per render, and plans.js hands the stream's hello, busy and doing over.
func TestViewCutJSOnThePage(t *testing.T) {
	v, g := strings.Index(pageHTML, `{{asset "viewcut.js"}}`), strings.Index(pageHTML, `{{asset "garden.js"}}`)
	if v < 0 || g < 0 || v > g {
		t.Errorf("viewcut.js at %d, garden.js at %d: viewcut.js must load first", v, g)
	}
	if !strings.Contains(pageAppJS, "VIEWCUT.see(") {
		t.Error("garden.js does not tell viewcut.js what is in view")
	}
	if !strings.Contains(pagePlansJS, "['hello', 'busy', 'doing', 'call']") || !strings.Contains(pagePlansJS, "'farm' + n") {
		t.Error("plans.js does not hand the hello, busy, doing and call events over")
	}
}
