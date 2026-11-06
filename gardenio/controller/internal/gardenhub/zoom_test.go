package gardenhub

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The page's root opts out of the browser's overscroll actions (gd-AOBOyh4iu4). On a phone the tab bar sits
// outside the board's touch-action:none, so a pinch whose first finger lands on them is the browser's own gesture; Chrome for
// Android then reads a downward drift at the top of the page as pull-to-refresh and reloads it from scratch, which is what
// zooming in and out on a phone looked like. overscroll-behavior:none on html and body is the documented switch for it.
func TestRootHasNoPullToRefresh(t *testing.T) {
	for _, sel := range []string{"html", "body"} {
		re := regexp.MustCompile(`(?m)^` + sel + `\{[^}]*\boverscroll-behavior:none\b`)
		if !re.MatchString(pageCSS) {
			t.Errorf("garden.css: the %s rule does not set overscroll-behavior:none", sel)
		}
	}
}

// Zooming never reloads /garden or rebuilds it from scratch: a desktop browser zoom (devicePixelRatio and CSS viewport
// change) and a phone's two-finger pinch on the map both keep the page's state, its cursor and its address a same-document
// one (ZOOM_MODE=desktop|phone picks the emulation; the default is desktop). What is asserted lives in
// testdata/zoom_browser.js.
func TestZoomUnderChromium(t *testing.T) {
	underChromium(t, "zoom_browser.js", func() *httptest.Server {
		return cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	})
}

// The chrome around the map on a phone opts out of the browser's pinch-zoom (gd-KZDKktrdQC): the bar along the top
// and the tab bar sit outside the board's touch-action:none, so a pinch with a finger on one of them zoomed the whole page
// (5x in Chromium) and the browser redrew all of it on release, which read as the page refreshing. pan-x pan-y keeps their
// scrolling and taps and drops pinch-zoom and double-tap zoom; the glass terminal and the reading panels keep theirs.
func TestMapChromeHasNoPagePinch(t *testing.T) {
	for _, sel := range []string{`\.crumb`, `\.tabbar`} {
		re := regexp.MustCompile(`(?m)^\s*` + sel + `\{([^}]*)\}`)
		m := re.FindStringSubmatch(pageCSS)
		if m == nil {
			t.Errorf("garden.css: no %s rule", sel)
			continue
		}
		ta := regexp.MustCompile(`\btouch-action:([^;]+)`).FindStringSubmatch(m[1])
		if ta == nil || (strings.TrimSpace(ta[1]) != "pan-x pan-y" && strings.TrimSpace(ta[1]) != "none") {
			t.Errorf("garden.css: the %s rule lets a pinch zoom the page (touch-action %q, want pan-x pan-y)", sel, ta)
		}
	}
}

// A phone's pinches and toolbar never make /garden look reloaded: a pinch from the chrome around the map is no page zoom, a
// board resized by the toolbar is painted in the frame it is resized in (never one blank frame per step of the slide), the
// cursor keeps its framing, and the page never loads again. What is asserted lives in testdata/phone_pinch_browser.js.
func TestPhonePinchUnderChromium(t *testing.T) {
	underChromium(t, "phone_pinch_browser.js", func() *httptest.Server {
		return cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	})
}
