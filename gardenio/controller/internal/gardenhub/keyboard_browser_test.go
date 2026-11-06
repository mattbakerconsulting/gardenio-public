package gardenhub

import (
	"net/http/httptest"
	"testing"
	"time"
)

// The phone's on-screen keyboard (M1.3, gd-xWWqgcmzXM) in a real browser, on a 390x844 touch phone whose visual viewport the
// walk shrinks as a keyboard does on iOS and on Chrome 108 and later: the bead view's fields stay inside the visual viewport
// with their submit (gd-KomHHeYfpT), a decision's Other answer, the comment box, the edit form and the Glass's nudge box, and
// every field is 16 px so iOS does not zoom into it. Real iOS is the M4.3 walk's (gd-YJPqjmLb47). testdata/keyboard_browser.js
// holds the page side.
func TestKeyboardUnderChromium(t *testing.T) {
	underChromium(t, "keyboard_browser.js", func() *httptest.Server { return cardWorld(t, time.Now, goldenPorch()...) })
}
