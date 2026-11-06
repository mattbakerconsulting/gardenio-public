package gardenhub

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestCameraGrowsUnderChromium walks the camera across a re-layout that widens a farm (homestead-world-design.md 29.11, X-D14;
// step 15 gd-gFMObwPAzs; testdata/camgrow_browser.js): a document that widens a column west of the cursor's project leaves
// that project's farm at the same screen px, one that grows the cursor's framed farm flies to the new framing, and with a
// pointer down only the anchor runs. The page's own camera code is what is under test, so it runs in a real browser.
func TestCameraGrowsUnderChromium(t *testing.T) {
	underChromium(t, "camgrow_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
