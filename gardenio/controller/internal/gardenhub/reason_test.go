package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A gardener's reason, drawn (gd-J3l6W4X6DJ, plan step 2b; homestead-agents-as-people.md section 2.3). reason.js is pure
// under node: each kind's pose (down a scarecrow, parked the speech bubble, sleeping zzz, stale wilting, starting a sprout),
// a kind it does not know leaving the pose to the state, the letter a parked gardener's bubble opens, and the line its card
// leads with. Then where the map uses it: the need order, the bubble's button over a parked gardener on its plot and at
// the barn, the card's first line and its state words, and every new figure and mark drawn with its rim. Skipped when node
// is not installed.
func TestReasonUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var R = require(process.argv[1]), W = require(process.argv[2]), A = require(process.argv[3]), assert = require("assert");
var g = function (id, state, bucket, kind, text, href) { var o = { id: id, agent: "n-" + id, role: "crew", project: "gardenio", state: state, bucket: bucket, since: 1, href: "/garden?bead=" + id, reason: null }; if (kind) o.reason = { kind: kind, text: text || kind, href: href }; return o; };
// the pose: the reason's first, else the state's
assert.deepStrictEqual(["down", "parked", "sleeping", "stale", "starting"].map(function (k) { return R.pose(g("x", "working", "alive", k)); }), ["down", "ask", "sleep", "wilt", "start"]);
assert.strictEqual(R.pose(g("x", "yielding", "alive", "purse_empty")), "ask", "a kind this client does not know: the state decides");
assert.strictEqual(R.of(g("x", "working", "alive", "purse_empty")), null);
assert.deepStrictEqual([R.pose(g("a", "failed", "stuck")), R.pose(g("b", "working", "alive")), R.pose(g("c", "idle", "idle"))], ["down", "work", "stand"]);
assert.ok(R.needsYou(g("x", "idle", "idle", "parked")) && R.needsYou(g("x", "working", "alive", "down")) && !R.needsYou(g("x", "working", "alive", "stale")));
// the letter: the handle's id from the parked reason's href
var pk = g("pk", "yielding", "alive", "parked", "Waiting on Merge PR #700", "/garden?bead=gd-H%2F1");
assert.strictEqual(R.letter(pk), "gd-H/1");
assert.strictEqual(R.letter(g("x", "failed", "stuck", "down", "Failed", "/garden?bead=gd-H")), "", "only a parked gardener has a letter");
// the card's first line: a link to the letter when parked, else the words
assert.deepStrictEqual(R.line(pk, "/b"), ["rsn rsn-parked", [{ text: "Waiting on Merge PR #700", href: "/b/garden?bead=gd-H%2F1", cls: "gletter", id: "gd-H/1" }]]);
assert.deepStrictEqual(R.line(g("dn", "failed", "stuck", "down", "Failed: OOMKilled"), "/b"), ["rsn rsn-down", "Failed: OOMKilled"]);
assert.strictEqual(R.line(g("w", "working", "alive"), "/b"), null, "simply working: no line");

// on the map: a parked gardener on a plot has its bubble's button where the bubble is drawn, at the levels its sprite is
function plan(id, n) {
  var tasks = []; for (var i = 0; i < n; i++) tasks.push({ id: id + "-t" + i, title: "T" + i, type: "task", state: "in_progress", updated: 1 });
  return { id: id, title: "Plan " + id, project: "gardenio", created: 1, activity: 1, counts: { in_progress: n }, phases: [{ id: id + "-p1", title: "P1", counts: { in_progress: n }, tasks: tasks }], unphased: { id: "", counts: {}, tasks: [] } };
}
var w = W.layout({ plans: [plan("pa", 4)], seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } }, null, { now: 1, readonly: true });
var on = function (o, task) { o.task = task; o.plot = { plan: "pa", phase: "" }; return o; };
var gs = [on(pk, "pa-t0"), on(g("dn", "working", "alive", "down", "Pod lost"), "pa-t1"), on(g("st", "working", "alive", "stale", "No progress for 3h"), "pa-t2"), on(g("w", "working", "alive"), "pa-t3")];
var st = W.gardenerState(w, gs), ask = st.hots.filter(function (h) { return h.kind === "gask"; }), spr = st.hots.filter(function (h) { return h.kind === "gardener" && h.id === "pk"; })[0];
assert.strictEqual(ask.length, 1, "one bubble button: the parked gardener's");
assert.deepStrictEqual([ask[0].id, ask[0].lv, ask[0].g], ["pk", spr.lv, pk]);
var s3 = st[3].spots.pk, B = R.BUBBLE;
assert.deepStrictEqual(ask[0].rect, { x: (s3.x + B.dx) / 16, y: (s3.y + B.dy) / 16, w: B.w / 16, h: B.h / 16 });
assert.deepStrictEqual(st[3].plots.pa.ids, ["dn", "pk", "st", "w"], "need first: down (a pod lost while working), then asking, then at work (stale or not)");
assert.deepStrictEqual(st[3].plots.pa.counts, { all: 4, work: 2, ask: 1, down: 1 }, "the signs count the reason");
assert.deepStrictEqual(["dn", "pk", "st"].map(function (id) { return st.look[id].pose; }), ["down", "ask", "wilt"]);
// the card leads with the reason; the state words follow it
var card = W.gardenerCard(pk, st, w, "/b", function () { return "4m"; });
assert.strictEqual(card.lines[0][0], "rsn rsn-parked"); assert.strictEqual(card.lines[0][1][0].cls, "gletter");
assert.ok(/^Parked · Waiting on Merge PR #700 · crew/.test(card.tip), card.tip);   // the Glass word (GL-4)
assert.deepStrictEqual(W.gardenerCard(gs[3], st, w, "/b", function () { return "4m"; }).lines[0], ["", "crew · gardenio"], "simply working: no reason line");
assert.deepStrictEqual(["dn", "st"].map(function (id) { return W.gardenerWord(st.byId[id], false); }).concat(W.gardenerWord(g("z", "dormant", "dormant", "sleeping"), true), W.gardenerWord(g("q", "queued", "idle", "starting"), true)), ["Down", "At work", "Asleep", "Starting"]);
assert.strictEqual(W.gardenerTip(ask[0], st, w, "/b", function () { return "4m"; }).text, "Waiting on Merge PR #700. Open the letter in the Mailbox to answer it.");
// in the field: the stand wears its pose and carries its letter, and ART.drawHome paints it with its bubble
var home = W.gardenerField(W.gardenerState(w, [g("hp", "idle", "idle", "parked", "Waiting on X", "/garden?bead=gd-X"), g("hz", "dormant", "dormant", "sleeping", "Sleeping")]), "", "/b");
assert.deepStrictEqual(home.map(function (it) { return [it.id, it.blocked, it.letter]; }), [[W.FIELD + "hp", true, "gd-X"], [W.FIELD + "hz", false, ""]]);
var fills = 0, ctx = { fillRect: function () { fills++; }, drawImage: function () {}, set fillStyle(v) { if (!/^#[0-9a-f]{6}$/i.test(v)) throw new Error("bad colour " + v); } };
var hm = w.index["project:gardenio"].home, gc = hm.cells.growing, paint = function (it) { var f0 = fills; A.drawHome(ctx, hm, { part: "live", fill: { growing: { items: [{ kind: "stand", x: gc.x + 1, y: gc.y + 1, w: 2, h: 1, item: it }], more: 0 } } }, A.palette(false)); return fills - f0; };
assert.ok(paint(home[0]) > paint(Object.assign({}, home[0], { blocked: false, look: null, lookNow: null })), "the parked stand's bubble is drawn");
// every reason's figure and mark draws in both schemes, the marks only where a reason puts them
var count = function (pose, n) { var f0 = fills; A.drawGardener(ctx, { pose: pose, group: "crew", hat: 0 }, 32, 48, 1, n); return fills - f0; };
[false, true].forEach(function (n) { var stand = count("stand", n); ["sleep", "wilt", "start"].forEach(function (p) { assert.ok(count(p, n) > stand, p + " draws its mark"); }); });
["starting", "asleep", "gwilt"].forEach(function (k) { var f0 = fills; A.drawLegend(ctx, k, false); assert.ok(fills - f0 > 100, "legend " + k + " draws a person"); });
Object.keys(A.SP.MARKS).forEach(function (k) {
  var m = A.SP.MARKS[k];
  for (var y = 0; y < m.length; y++) for (var x = 0; x < m[y].length; x++) {
    var c = m[y][x]; if (c === ".") continue;
    var edge = y === 0 || x === 0 || y === m.length - 1 || x === m[y].length - 1 || m[y - 1][x] === "." || m[y + 1][x] === "." || m[y][x - 1] === "." || m[y][x + 1] === ".";
    if (edge) assert.strictEqual(c, "@", k + " at " + x + "," + y + " is " + c);
  }
});
`
	var args []string
	for _, f := range []string{"reason.js", "world.js", "art.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node reason test failed: %v\n%s", err, out)
	}
}

// reason.js loads before art.js and world.js, which read REASON as they load, and the card's letter link and the Mailbox
// row it opens keep the names reason.js binds to.
func TestReasonModuleWiring(t *testing.T) {
	r, a, w := strings.Index(pageHTML, `{{asset "reason.js"}}`), strings.Index(pageHTML, `{{asset "art.js"}}`), strings.Index(pageHTML, `{{asset "world.js"}}`)
	if r < 0 || r > a || r > w {
		t.Error("garden.html must load reason.js before art.js and world.js")
	}
	for _, want := range []string{`id="d-mail"`, `id="dbtn-mail"`} {
		if !strings.Contains(pageHTML, want) {
			t.Errorf("garden.html lost %s, which reason.js opens", want)
		}
	}
	if !strings.Contains(pageAppJS, "REASON.open(REASON.letter(h.g))") {
		t.Error("garden.js: a speech bubble's button no longer opens its letter")
	}
}
