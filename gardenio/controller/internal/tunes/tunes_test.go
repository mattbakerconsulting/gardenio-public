package tunes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Both pages inline the script in a <script>, so it may not close it.
func TestEmbeddedSourcesStayInsideTheirScript(t *testing.T) {
	if strings.Contains(strings.ToLower(JS), "</script") {
		t.Error("web/tunes.js contains </script, which would close the <script> that inlines it")
	}
}

// The pieces the pages inline: the stylesheet keys off the host tokens with a fallback, the script mounts on #tunes and
// fetches the hub's playlist.json itself (the browser walk in gardenhub checks that it waits for a press), and the root
// carries the hub as data-base.
func TestEmbeddedPieces(t *testing.T) {
	for _, want := range []string{"var(--surface,var(--l-card,", "var(--accent,var(--l-accent,", ".tunes.corner{position:fixed", "@media (pointer:coarse)"} {
		if !strings.Contains(CSS, want) {
			t.Errorf("tunes.css missing %s", want)
		}
	}
	for _, want := range []string{"var TUNES = (function", "document.getElementById('tunes')", "LIST = 'playlist.json'", "fetch(resolve(base, LIST)", "credentials: 'omit'", "audio.preload = 'none'", "sessionStorage", "navigator.mediaSession"} {
		if !strings.Contains(JS, want) {
			t.Errorf("tunes.js missing %s", want)
		}
	}
	for _, bad := range []string{"XMLHttpRequest", "prefers-color-scheme", "@keyframes", "animation:"} {
		if strings.Contains(JS, bad) || strings.Contains(CSS, bad) {
			t.Errorf("the player must not use %s: it follows the host page's theme and motion", bad)
		}
	}
	if n := strings.Count(JS, "fetch("); n != 1 {
		t.Errorf("tunes.js calls fetch( %d times; it fetches one thing, the playlist, in load()", n)
	}
	if got := Markup(false); got != `<div class="tunes" id="tunes" data-base="https://play.clonk.tv/"></div>` {
		t.Errorf("Markup(false) = %s", got)
	}
	if got := Markup(true); !strings.Contains(got, `class="tunes corner"`) {
		t.Errorf("Markup(true) = %s", got)
	}
}

// The pure part of tunes.js under node: testdata/tunes_test.js.
func TestTunesLogicUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := filepath.Abs(filepath.Join("web", "tunes.js"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, filepath.Join("testdata", "tunes_test.js"), js)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
