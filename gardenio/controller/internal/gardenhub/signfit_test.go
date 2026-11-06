package gardenhub

import (
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A tip kept on the screen (gd-O3nwaqbNUG): web/signfit.js is pure where it decides, so its contract runs under node. area is
// the map less its insets or 16 px, whichever is more, on every side; fit leaves a tip that is on the screen where it is; a tip
// that leaves on the side it faces flips to the other side of its Thing when that one is on the screen, and stays put when it
// is not; then it is pushed inside the area (one wider than the area may use the map's whole width less the gutter), then off any camera
// button it would sit under (along one axis, else along both), its pointer moved to the Thing's middle and kept 6 px from its ends. On a phone at the Bead level a plot's sign hung off the left edge of the map. Skipped when node is not
// installed.
func TestSignFitUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var F = require(process.argv[1]), assert = require("assert");
// the area: 16 px in, or the map's insets where they are more
assert.deepStrictEqual(F.area({ w: 390, h: 844 }), { x: 16, y: 16, w: 358, h: 812, vw: 390 });
assert.deepStrictEqual(F.area({ w: 390, h: 844, top: 90, right: 60, bottom: 180, left: 4 }), { x: 16, y: 90, w: 314, h: 574, vw: 390 });
var a = F.area({ w: 390, h: 844 });
// on the screen: where it is, the pointer at its middle
assert.deepStrictEqual(F.fit({ x: 100, y: 200, w: 120, h: 40 }, { x: 130, y: 244, w: 60, h: 60 }, "above", a), { x: 100, y: 200, slot: "above", tick: null });
assert.deepStrictEqual(F.fit({ x: 100, y: 200, w: 200, h: 40 }, { x: 100, y: 244, w: 60, h: 60 }, "above", a), { x: 100, y: 200, slot: "above", tick: 30 }, "placed flush with the Thing's left edge, not pushed: the tick still points at its middle");
// the phone at Bead zoom (gd-O3nwaqbNUG): a sign above a plot hangs off the left edge; pushed in, its pointer at the plot's middle
assert.deepStrictEqual(F.fit({ x: -80, y: 200, w: 200, h: 40 }, { x: -10, y: 244, w: 60, h: 60 }, "above", a), { x: 16, y: 200, slot: "above", tick: 6 });
assert.deepStrictEqual(F.fit({ x: -40, y: 200, w: 200, h: 40 }, { x: 30, y: 244, w: 60, h: 60 }, "above", a), { x: 16, y: 200, slot: "above", tick: 44 });
// off the right edge: pushed in by its right edge
assert.deepStrictEqual(F.fit({ x: 300, y: 200, w: 120, h: 40 }, { x: 330, y: 244, w: 60, h: 60 }, "below", a).x, 254);
// leaves the top facing up: flips below the Thing
assert.deepStrictEqual(F.fit({ x: 100, y: -10, w: 120, h: 40 }, { x: 130, y: 34, w: 60, h: 60 }, "above", a), { x: 100, y: 98, slot: "below", tick: null });
// and the bottom facing down: flips above
assert.deepStrictEqual(F.fit({ x: 100, y: 820, w: 120, h: 40 }, { x: 130, y: 756, w: 60, h: 60 }, "below", a), { x: 100, y: 712, slot: "above", tick: null });
// left and right
assert.deepStrictEqual(F.fit({ x: -100, y: 300, w: 120, h: 40 }, { x: 24, y: 290, w: 60, h: 60 }, "left", a), { x: 88, y: 300, slot: "right", tick: null });
assert.deepStrictEqual(F.fit({ x: 330, y: 300, w: 120, h: 40 }, { x: 266, y: 290, w: 60, h: 60 }, "right", a), { x: 142, y: 300, slot: "left", tick: null });
// the other side is off the screen too (a Thing taller than the map): stays on its side, pushed in
assert.deepStrictEqual(F.fit({ x: 100, y: -10, w: 120, h: 40 }, { x: 130, y: 34, w: 60, h: 900 }, "above", a), { x: 100, y: 16, slot: "above", tick: null });
// a left label pushed down along the edge it points from: its pointer moves to the Thing's middle
assert.deepStrictEqual(F.fit({ x: 100, y: -30, w: 60, h: 40 }, { x: 164, y: -40, w: 40, h: 40 }, "left", a), { x: 100, y: 16, slot: "left", tick: 6 });
// wider than the area: from its left edge
assert.strictEqual(F.fit({ x: -50, y: 300, w: 400, h: 40 }, { x: 0, y: 344, w: 60, h: 60 }, "above", a).x, 16);
// inside its Thing or docked: never flips, only pushed in
assert.deepStrictEqual(F.fit({ x: -20, y: 300, w: 120, h: 40 }, { x: -60, y: 260, w: 300, h: 300 }, "inside", a), { x: 16, y: 300, slot: "inside", tick: null });
// a phone's tip wider than the map less the camera column: the whole width less the gutter, and up off the Go button
var ph = F.area({ w: 390, h: 844, right: 60, top: 108, bottom: 180 }), cams = [{ x: 338, y: 622, w: 44, h: 44 }, { x: 331, y: 672, w: 51, h: 44 }];
assert.deepStrictEqual(F.fit({ x: 16, y: 600, w: 323, h: 40 }, { x: -880, y: -1500, w: 1760, h: 2100 }, "below", ph, 4, cams), { x: 16, y: 578, slot: "below", tick: null });
assert.deepStrictEqual(F.fit({ x: 16, y: 300, w: 323, h: 40 }, { x: 0, y: 0, w: 60, h: 60 }, "below", ph, 4, cams), { x: 16, y: 300, slot: "below", tick: 14 }, "not under the Thing's middle: the tick points at it");
// a phone on its side, an arrow on the top edge and one on the right (gd-wAN7P4Armw): down off the first is 1 px under the
// second, left off the second is under the first and down past the second leaves the map, so it goes down and left at once
var side = F.area({ w: 740, h: 390, top: 58, right: 59, bottom: 60 }), arrows = [{ x: 294, y: 66, w: 48, h: 48 }, { x: 680, y: 138, w: 48, h: 48 }];
assert.deepStrictEqual(F.fit({ x: 321, y: 58, w: 360, h: 149 }, { x: -436, y: -1286, w: 2352, h: 2640 }, "inside", side, 4, arrows), { x: 316, y: 118, slot: "inside", tick: null });
// a gap of its own (the steward's tip stands 8 px off its sprite)
assert.strictEqual(F.fit({ x: 100, y: 0, w: 120, h: 40 }, { x: 150, y: 20, w: 16, h: 32 }, "above", a, 8).y, 60);
console.log("ok");
`
	mod, err := filepath.Abs(filepath.Join("web", "signfit.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, mod).CombinedOutput()
	if err != nil {
		t.Fatalf("signfit under node: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("signfit under node printed %q", out)
	}
}

// The page loads signfit.js before stip.js and garden.js, and places the steward's tip with it.
func TestSignFitWired(t *testing.T) {
	fit, stip, app := strings.Index(pageHTML, `{{asset "signfit.js"}}`), strings.Index(pageHTML, `{{asset "stip.js"}}`), strings.Index(pageHTML, `{{asset "garden.js"}}`)
	if fit < 0 || stip < fit || app < fit {
		t.Errorf("garden.html script order: signfit.js %d, stip.js %d, garden.js %d; want signfit.js before both", fit, stip, app)
	}
	for _, want := range []string{"BEACONS.render(bcnEl,bcnNow=bcnLayout(cam),goBeacon)", "view:signView,bcns:bcnNow", "sv=signView=signVp()"} {
		if !strings.Contains(pageAppJS, want) {
			t.Errorf("garden.js has no %q", want)
		}
	}
	if !strings.Contains(pageStipJS, "SIGNFIT.fit(") {
		t.Error("stip.js does not place the steward's tip with SIGNFIT.fit")
	}
}

// On a phone, upright and on its side, by day and by night, at the Bead level: with a biome, a plot, a task and a seed under
// the cursor, and the steward's tip open, the camera cut so the Thing's middle sits on each edge of the map, the tip stays 16 px
// inside the map with its pointer on its Thing. What is asserted lives in testdata/signfit_browser.js.
func TestSignFitUnderChromium(t *testing.T) {
	underChromium(t, "signfit_browser.js", func() *httptest.Server {
		return cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	})
}
