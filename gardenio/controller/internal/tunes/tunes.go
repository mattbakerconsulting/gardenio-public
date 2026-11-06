// Package tunes is the teensy music player on the Homestead (/garden) HUD and the Lands front-of-house pages
// (gd-z8nt2TpS6o): idle, one small note button; while a song plays, and only then, a little panel opens beside it with
// the track's name and a scrubber (no EQ: the thread asked for a scrubber instead).
//
// Nothing about the music is in this repo: the playlist and the songs live at Base. The browser fetches Base's
// playlist.json once, on the first press, and streams each song from there as it plays (the hub answers with
// Access-Control-Allow-Origin: * and byte ranges, so both the fetch and the scrubber work across the origin). If the
// playlist cannot be had, the button goes quietly disabled.
//
// Both pages inline the two pieces here, so first paint still needs no external request (TestPageHasNoExternalRequests):
// CSS styles the widget from the host page's tokens (--surface, --ink, --edge, --accent on /garden; --l-card, --l-text,
// --l-rule, --l-accent on /lands) with a night palette as the fallback; JS is web/tunes.js, which mounts on #tunes and
// exposes its pure helpers as TUNES for the node tests.
package tunes

import _ "embed"

// Base is the hub the playlist is fetched from and every playlist path is relative to.
const Base = "https://play.clonk.tv/"

var (
	//go:embed web/tunes.js
	JS string
	//go:embed web/tunes.css
	CSS string
)

// Markup is the widget's root. corner places it fixed in the page's bottom-right corner (the Lands pages); without it the
// root sits inline where the host puts it (the Homestead HUD's control group).
func Markup(corner bool) string {
	cls := "tunes"
	if corner {
		cls += " corner"
	}
	return `<div class="` + cls + `" id="tunes" data-base="` + Base + `"></div>`
}
