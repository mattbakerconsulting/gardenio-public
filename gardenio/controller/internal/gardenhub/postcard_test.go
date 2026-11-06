package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// postcard.js's crop, caption and destination are pure, so their contract runs under node (gd-WxRuyfdUtR): the crop is the
// plot and a margin in device pixels, cut to the view, and nothing when too little of the plot shows; the caption is one line
// with the plan's title, current phase, status and steps; the card goes to the plan's thread when the Commons placed it,
// else to its project's land. A project's card frames its farmstead and first ring of plots (29.6).
func TestPostcardUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var P = require(process.argv[1]), assert = require("assert");
var vp = { w: 1280, h: 720 };

// crop: the plot plus PAD, in device px, rounded out; its CSS size rides along for the snap context
assert.deepStrictEqual(P.crop({ x: 100, y: 50, w: 200, h: 100 }, vp, 1), { x: 88, y: 38, w: 224, h: 124, css: { w: 224, h: 124 } });
assert.deepStrictEqual(P.crop({ x: 100, y: 50, w: 200, h: 100 }, vp, 2), { x: 176, y: 76, w: 448, h: 248, css: { w: 224, h: 124 } }, "DPR 2 doubles the box");
assert.deepStrictEqual(P.crop({ x: 100.4, y: 50.6, w: 10.3, h: 20.2 }, vp, 1.5), { x: 132, y: 57, w: 53, h: 68, css: { w: 34, h: 44 } }, "fractions round out");
// cut to the view: a plot running off the left and bottom shows what the view shows
assert.deepStrictEqual(P.crop({ x: -300, y: 600, w: 400, h: 400 }, vp, 1), { x: 0, y: 588, w: 112, h: 132, css: { w: 112, h: 132 } });
// a sliver in view, or none at all, is no postcard
assert.strictEqual(P.crop({ x: 1275, y: 10, w: 400, h: 400 }, vp, 1), null, "17 px across is under MIN");
assert.strictEqual(P.crop({ x: 2000, y: 10, w: 40, h: 40 }, vp, 1), null);
assert.strictEqual(P.crop(null, vp, 1), null);
assert.strictEqual(P.crop({ x: 0, y: 0, w: 40, h: 40 }, { w: 0, h: 0 }, 1), null, "no view yet");

// phase: the first phase not closed, as the plot's status counts it
var plan = { id: "gd-p", title: "Homestead World", state: "building", status: "open",
  phases: [{ title: "Bones", status: "closed" }, { title: "Time-lapse & postcards", status: "open" }, { title: "Polish", status: "open" }],
  counts: { closed: 3, open: 4, in_progress: 2 } };
assert.deepStrictEqual(P.phase(plan), { n: 2, of: 3, title: "Time-lapse & postcards" });
assert.strictEqual(P.phase({ phases: [{ status: "closed" }] }), null, "every phase closed");
assert.strictEqual(P.phase({}), null);

// caption: one line, the parts it has words for
assert.strictEqual(P.caption(plan), "Postcard: Homestead World · phase 2 of 3: Time-lapse & postcards · building · 3/9 steps");
assert.strictEqual(P.caption({ id: "gd-q", title: "  Two\n lines ", status: "in_progress" }), "Postcard: Two lines · in progress", "no phases, no steps; the bead status when plan_state is unset");
assert.strictEqual(P.caption({ id: "gd-r" }), "Postcard: gd-r", "an untitled plan is its id");
var long = P.caption({ title: new Array(300).join("x") });
assert.ok(long.length <= "Postcard: ".length + 120 && /…$/.test(long), "a long title is cut: " + long.length);
assert.ok(P.caption(plan).indexOf("\n") < 0);

// a project's card (homestead-world-design.md 29.6): its farmstead and the plots of the first ring round it (slots 1 to 8),
// never an outer ring's; its line names the project, its plans and what needs you
var home = { x: 100, y: 100, w: 32, h: 32 }, at = function (s, c, r) { return { slot: s, x: 100 + c * 35, y: 100 + r * 35, w: 32, h: 32 }; };
assert.deepStrictEqual(P.farm({ home: home, plots: [] }), home, "a farm-only project is its farmstead");
assert.deepStrictEqual(P.farm({ home: home, plots: [at(1, 1, 0)] }), { x: 100, y: 100, w: 67, h: 32 }, "one plan: the farm and the plot east of it");
assert.deepStrictEqual(P.farm({ home: home, plots: [at(1, 1, 0), at(6, -1, -1), at(9, -2, -2), at(20, 2, 1)] }), { x: 65, y: 65, w: 102, h: 67 }, "ring 2's plots are left out");
assert.strictEqual(P.farmCaption({ key: "gardenio", name: "gardenio", plots: [1, 2, 3], overflow: { n: 2 }, needs: 1 }), "Postcard: gardenio · 5 plans · 1 needs you");
assert.strictEqual(P.farmCaption({ key: "zeta", name: " Zeta\n farm ", plots: [] }), "Postcard: Zeta farm", "no plans, nothing waiting");

// target: the plan's thread when the Commons placed it, else its project's land, else nowhere
assert.deepStrictEqual(P.target({ land: "gardenio", channel: "crew", thread: "gd-t", href: "/garden/c/gardenio/crew/t/gd-t" }, "gardenio"),
  { land: "gardenio", channel: "crew", thread: "gd-t" });
assert.deepStrictEqual(P.target(null, "gardenio"), { land: "gardenio" });
assert.deepStrictEqual(P.target({ land: "gardenio", channel: "crew" }, " claimlayer "), { land: "claimlayer" }, "a place with no thread is no thread");
assert.strictEqual(P.target(null, ""), null);
`
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	out, err := exec.Command(node, "-e", script, abs(filepath.Join("web", "postcard.js"))).CombinedOutput()
	if err != nil {
		t.Fatalf("postcard.js contract failed under node: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
