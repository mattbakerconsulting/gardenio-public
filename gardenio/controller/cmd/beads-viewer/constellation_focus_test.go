package main

import (
	"strings"
	"testing"
)

// The hover-highlight feature (ka-0000007349) is implemented in the embedded
// constellation.js and the focus CSS in layout.html. There is no JS test
// harness in this package, so these tests pin the JS↔CSS↔template contract
// from Go: they assert the data attributes the script toggles and the CSS
// rules that hide-by-dimming (preserving the baked SVG coords). A regression
// that drops the attributes or the dimming rule fails here.
//
// The former staged click-to-subtree prune was removed: a plain node click
// now simply navigates to the clicked bead (the node is an <a> anchor and the
// click handler no longer intercepts it). TestFocusJS_ClickNavigates pins that.

func readStaticFile(t *testing.T, name string) string {
	t.Helper()
	b, err := staticFS.ReadFile("templates/static/" + name)
	if err != nil {
		t.Fatalf("read static %s: %v", name, err)
	}
	return string(b)
}

func readTemplateFile(t *testing.T, name string) string {
	t.Helper()
	b, err := templateFS.ReadFile("templates/" + name)
	if err != nil {
		t.Fatalf("read template %s: %v", name, err)
	}
	return string(b)
}

// TestFocusJS_ScopesToBeadConstellation pins that the focus binding only
// targets the bead-page constellation, NOT the gardener map
// (figure.cap-constellation) which owns its own pan/zoom + shift-click
// re-root in maps.js. Hijacking clicks there would break the map.
func TestFocusJS_ScopesToBeadConstellation(t *testing.T) {
	js := readStaticFile(t, "constellation.js")
	if !strings.Contains(js, "figure.constellation:not(.cap-constellation)") {
		t.Errorf("constellation.js must scope focus binding to figure.constellation:not(.cap-constellation) so it never hijacks the gardener map; selector not found")
	}
	if !strings.Contains(js, "bindFocus") {
		t.Errorf("constellation.js missing bindFocus; focus feature not wired into init()")
	}
	if !strings.Contains(js, "bindFocus(root)") {
		t.Errorf("constellation.js init() must call bindFocus(root) so the feature re-binds after htmx:afterSwap")
	}
}

// TestFocusJS_UsesDimNotRemove pins the critical operator constraint: pruned
// nodes/edges must NOT move. The script must hide by toggling attributes,
// never by removing DOM nodes or re-laying-out. We assert it does not call
// remove()/removeChild on graph elements and that it toggles data-focus-out.
func TestFocusJS_UsesDimNotRemove(t *testing.T) {
	js := readStaticFile(t, "constellation.js")
	if !strings.Contains(js, "data-focus-out") {
		t.Errorf("constellation.js must toggle data-focus-out to hide-by-dimming; attribute not found")
	}
	// Guard against an accidental switch to DOM removal / re-layout.
	for _, banned := range []string{".remove()", "removeChild(", "appendChild("} {
		if strings.Contains(js, banned) {
			t.Errorf("constellation.js focus logic must not use %q — hide-by-dimming preserves baked coords; found it", banned)
		}
	}
}

// TestFocusJS_ClickNavigates pins that a plain node click navigates to the
// clicked bead (Slack request: remove the click-to-subtree prune; a click
// should simply navigate). The handler must NOT intercept node clicks — nodes
// are <a> anchors, so the browser follows them natively. Regressing this back
// into a focus-cycle (preventDefault on node click) fails here.
func TestFocusJS_ClickNavigates(t *testing.T) {
	js := readStaticFile(t, "constellation.js")
	// The removed staged-prune machinery must be gone.
	for _, gone := range []string{"cycleFocus", "applyFocus", "MAX_FOCUS_LEVEL", "bfsSet", "data-focus-root", "data-focus-mode', 'click'", "addEventListener('dblclick'"} {
		if strings.Contains(js, gone) {
			t.Errorf("constellation.js must not retain the removed click-focus machinery; found %q", gone)
		}
	}
	// bindFocus must not register a click listener that hijacks node clicks.
	if strings.Contains(js, "addEventListener('click'") {
		// The only legitimate click listener lives in bindEdges (edge
		// navigation). bindFocus must not add one.
		idx := strings.Index(js, "function bindFocus(")
		end := strings.Index(js[idx:], "function bindMapsHover(")
		if idx >= 0 && end > 0 && strings.Contains(js[idx:idx+end], "addEventListener('click'") {
			t.Errorf("bindFocus must not bind a click listener — a node click should navigate natively via its <a> anchor")
		}
	}
}

// TestFocusCSS_DimsInPlace pins the CSS that drives hide-by-dimming. The
// rule must set opacity (not display:none, which could be fine but the
// operator asked for ghosting) AND pointer-events:none so pruned elements
// neither catch clicks nor trigger hover.
func TestFocusCSS_DimsInPlace(t *testing.T) {
	css := readTemplateFile(t, "layout.html")
	if !strings.Contains(css, "data-focus-out") {
		t.Errorf("layout.html missing data-focus-out CSS rule; focus dimming will not render")
	}
	// Find the focus-out block and assert it dims + drops pointer-events.
	idx := strings.Index(css, "line[data-focus-out]")
	if idx < 0 {
		t.Fatalf("layout.html missing edge focus-out selector")
	}
	block := css[idx:min(idx+200, len(css))]
	if !strings.Contains(block, "opacity") {
		t.Errorf("focus-out CSS must dim via opacity; block:\n%s", block)
	}
	if !strings.Contains(block, "pointer-events: none") {
		t.Errorf("focus-out CSS must set pointer-events:none so pruned elements are inert; block:\n%s", block)
	}
}
