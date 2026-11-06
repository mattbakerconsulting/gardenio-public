package gardenhub

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// cursor.js is the pure cursor world (gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md, section 10.1),
// so its contract runs under node (testdata/cursor_test.js): the tree over a layout in the spec's geometry, the fixture farm
// and the 10x farm (every Thing in exactly one grid, coordinates matching the rects, block-major beds, Other and the crates,
// the band rows, every biome's home cell at its plot grid's centre and its zones as the courtyard's quarters, the truck last in the Barn); move skipping empties,
// onto the home cell from each side and back, and bumping at an edge; sibling jumps with clamping, nearest occupied
// and bubbling to the world's edge; descend by memory, then need, then reading order, with a leaf's open; follow by id, then
// coordinate, then nearest, then ascend; atFor/pathFor round trips (and WORLD.resolve reads them back); frame's used bounds,
// window and snapped zoom at 1280x800, 390x520 coarse and 1920x1080; signs' tiers (no label for the cursor's Thing, child
// labels clear of each other, glyphs) and the World's header plates and status line (statusOf). Numbers are logged; run
// with -v to see them.
func TestCursorUnderNode(t *testing.T) {
	r := nodeScript(t, "cursor_test.js")
	t.Logf("frame %v; 10x %v", r["frame"], r["tenx"])
}

// signs() is a function of the cursor path and the viewport only (N-D1): its parameters name no pointer, and nothing in
// cursor.js reads one, so text can never answer the hand. A source check, like TestReducedMotionCoversEveryAnimation's
// wiring checks.
func TestCursorSignsTakeNoPointer(t *testing.T) {
	sig := regexp.MustCompile(`function signs\(([^)]*)\)`).FindStringSubmatch(pageCursorJS)
	if sig == nil {
		t.Fatal("cursor.js has no signs() function")
	}
	if got := strings.ReplaceAll(sig[1], " ", ""); got != "t,path,vp,z" {
		t.Errorf("signs(%s): want exactly (t, path, vp, z)", sig[1])
	}
	code := regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`).ReplaceAllString(pageCursorJS, "") // the comments may say "pointer"
	if m := regexp.MustCompile(`(?i)\b(pointer\w*|mouse\w*|hover\w*|ptr|clientX|clientY|pageX|pageY|offsetX|offsetY)\b`).FindString(code); m != "" {
		t.Errorf("cursor.js mentions %q: the cursor world must not read the pointer", m)
	}
	if regexp.MustCompile(`\b(document|window)\.`).MatchString(code) {
		t.Error("cursor.js touches the DOM; it must stay pure")
	}
}

// curnav.js is the page's cursor over the pure world (cursor-grid spec sections 3.2, 3.5, 3.6 and 4.3), so it runs under node
// too (testdata/curnav_test.js): the key map; the opening cursor from an at=, else the saved path, else depth 0 on the first
// project; moves, descends and ascends with the per-grid memory a descend honours, capped and saved; cursor jumps and the
// needs walk; the grid's cells with 1-based rows and columns from its used bounds, its name and a cell's place in words; the
// live region's sentence; each strip's zones from the page's model, a patch rebuilding only its home cell, the Home key; the outline's rows from the tree; and what
// pointer and touch ask of it (build step 4): the Thing under a tap or a zoom, no deeper than the cursor.
func TestCurNavUnderNode(t *testing.T) {
	r := nodeScript(t, "curnav_test.js")
	t.Logf("memory %v; grid %v", r["memory"], r["grid"])
}

// The steady title bar (gd-yozHl8UqIs) is CURNAV nav.bar over the pure world, so it runs under node too
// (testdata/curbar_test.js) on the fixture farm with short titles put on plan A, its first phase and a step: on a step the bar
// reads Plan › Phase › Task from the short titles (step 1, gd-MpWy5X34hh), each part the Thing a press jumps the cursor to with an
// address that resolves back to the same place, and the step's state word at the end; a phase reads two parts, a plot its plan
// alone, the project its name; a Thing with no short title keeps its title, and no index at all still names everything. The
// bar carries the status line of cursor-grid spec 6.4 the full label used to (gd-zDdiA3HBh5), whole and need words first, for
// a project, a plan, a phase, a task and a harvested step with its badges.
func TestCursorBarUnderNode(t *testing.T) {
	r := nodeScript(t, "curbar_test.js")
	t.Logf("step %v", r["step"])
}

// The cursor in Chromium (cursor-grid spec 10.4, desktop; the pointer and touch are later steps): ?at= names the
// cursor and z= is ignored, #hots is a role=grid with one tab stop named by row and column, Tab lands on the cursor and leaves,
// the arrows move focus, Enter descends to a step and announces, Escape ascends, Shift and an arrow jumps plans, the address
// follows without z= and a reload (or a bare /garden) restores the cursor, and Home and 0.
// What is asserted lives in testdata/cursor_browser.js.
func TestCursorUnderChromium(t *testing.T) {
	underChromium(t, "cursor_browser.js", func() *httptest.Server {
		return cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	})
}

// Pointer and touch drive the cursor in Chromium (cursor-grid spec 4.3, 7 and 10.4; build step 4): a click or tap moves the
// cursor and a second goes in (or opens a step), a double-click is the two, a click outside the grid jumps, a mouse drag does
// nothing, a wheel notch is one depth step and a small wheel springs back, and a swipe on a phone is one cursor step. What is
// asserted lives in testdata/pointer_browser.js.
func TestCursorPointerUnderChromium(t *testing.T) {
	underChromium(t, "pointer_browser.js", func() *httptest.Server {
		return cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	})
}
