package gardenhub

import (
	"net/http/httptest"
	"testing"
	"time"
)

// The phone's navigation (M1.4, gd-4Y2qOgfmJE): the tab bar reaches every surface (Farm, Commons, Porch, Mailbox, Workbench,
// Glass), a tab switch pushes no history while each pane keeps its own Back, a deep link to a bead, plan or Glass opens that
// pane with its tab marked, Back from it landing on the Farm, and the Commons tab goes to the workbench's page. What is asserted lives in testdata/tabs_phone_browser.js.
func TestPhoneTabsUnderChromium(t *testing.T) {
	underChromium(t, "tabs_phone_browser.js", func() *httptest.Server {
		srv, _ := cardWorldRig(t, time.Now, "", nil, nil, true)
		return srv
	})
}
