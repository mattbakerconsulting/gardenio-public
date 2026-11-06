package gardenhub

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The teensy music player (internal/tunes, gd-z8nt2TpS6o) on /garden: its root sits in the HUD after the title plaque and
// before the spacer (so a phone keeps it on the title row), its stylesheet follows the page's, and the script comes last in
// the body, so the root is there when it mounts. No playlist is inlined: the script fetches it on the first press, and
// nothing in the page links the hub before then (TestPageHasNoExternalRequests keeps watching the whole page for src/href
// to another host).
func TestPageHasTunes(t *testing.T) {
	_, rec := servedPage(t)
	body := inlined(t, rec.Body.String())
	live := strings.Index(body, `id="livesr"`)
	root := strings.Index(body, `<div class="tunes" id="tunes" data-base="https://play.clonk.tv/"></div>`)
	sp := strings.Index(body, `<div class="sp"></div>`)
	if live < 0 || root < 0 || sp < 0 || !(live < root && root < sp) {
		t.Errorf("the player's root must follow the plaque and precede the spacer: plaque's live %d root %d spacer %d", live, root, sp)
	}
	css, tcss, head := strings.Index(body, "<style>"), strings.Index(body, ".tunes{--tn-bg:"), strings.Index(body, "</head>")
	if css < 0 || tcss < 0 || !(css < tcss && tcss < head) {
		t.Errorf("tunes.css must load in <head> after garden.css: style %d tunes %d head %d", css, tcss, head)
	}
	js, end := strings.Index(body, "var TUNES = (function"), strings.Index(body, "</body>")
	if js < 0 || !(root < js && js < end) {
		t.Errorf("the script must come last in the body: root %d script %d body end %d", root, js, end)
	}
	if strings.Contains(body, "TUNES_PLAYLIST") || strings.Contains(body, `"tracks":[`) {
		t.Error("the page inlines a playlist; the script fetches it on the first press")
	}
	if strings.Contains(body, `src="https://play.clonk.tv`) || strings.Contains(body, `href="https://play.clonk.tv`) {
		t.Error("the page links the hub before a press; the script sets every src and href at play time")
	}
}

// The player in a real browser (testdata/tunes_browser.js): idle, one button, no panel and no request; a press fetches the
// playlist once, plays and opens the panel with the title and the scrubber; a press pauses and folds it; Next moves on; the
// state survives a reload; a playlist that fails leaves a quiet disabled button. The audio and the hub are stubs (a made-up
// playlist), so nothing leaves the test: what is asserted is the widget's response to the fetch and the audio's events.
func TestTunesUnderChromium(t *testing.T) {
	underChromium(t, "tunes_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}
