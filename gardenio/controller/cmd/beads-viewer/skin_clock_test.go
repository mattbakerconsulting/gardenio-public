package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The time of day (skin_clock.go, gd-yV1HX7zGNE) is sky.js's rule on the
// viewer's skins: night from 19:00 to 07:00 local, or on a dark OS; a stored
// skin wins outright. The pure part runs under node, like TestSkyLogicUnderNode.
func TestSkinClockUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src := filepath.Join(t.TempDir(), "skin_clock.js")
	if err := os.WriteFile(src, []byte(skinClockJS), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
var S = require(process.argv[1]), assert = require("assert");
assert.strictEqual(S.DUSK, 19); assert.strictEqual(S.DAWN, 7);
assert.strictEqual(S.KEY, "beads-viewer:skin");
// the clock, then the OS; the viewer's pair by default
assert.strictEqual(S.resolve(12, false), "homestead");
assert.strictEqual(S.resolve(12, true), "dark-homestead", "a dark OS is night by day too");
assert.strictEqual(S.resolve(19, false), "dark-homestead", "dusk is night");
assert.strictEqual(S.resolve(23, false), "dark-homestead");
assert.strictEqual(S.resolve(0, false), "dark-homestead");
assert.strictEqual(S.resolve(6, false), "dark-homestead", "before dawn is night");
assert.strictEqual(S.resolve(7, false), "homestead", "dawn is day");
assert.strictEqual(S.resolve(18, false), "homestead");
// the Lands front door's pair: the same hours
assert.strictEqual(S.resolve(12, false, "baseline", "dark-terminal"), "baseline");
assert.strictEqual(S.resolve(21, false, "baseline", "dark-terminal"), "dark-terminal");
assert.strictEqual(S.resolve(9, true, "baseline", "dark-terminal"), "dark-terminal");
// every hour agrees with /garden's isNightHour
for (var h = 0; h < 24; h++) assert.strictEqual(S.resolve(h, false) === "dark-homestead", h >= 19 || h < 7, "hour " + h);
// the timer lands on the boundary, in local wall-clock time
function mins(h, m) { return S.msToFlip(new Date(2026, 9, 20, h, m || 0, 0, 0)) / 60000; }
assert.strictEqual(mins(12), 7 * 60, "noon: dusk is 7 h away");
assert.strictEqual(mins(18, 30), 30);
assert.strictEqual(mins(19), 12 * 60, "at dusk the next flip is dawn");
assert.strictEqual(mins(3), 4 * 60);
assert.strictEqual(mins(6, 59), 1);
assert.strictEqual(mins(7), 12 * 60, "at dawn the next flip is dusk");
// the season from the month (getMonth: 0 is January), northern hemisphere
var want = ["winter", "winter", "spring", "spring", "spring", "summer", "summer", "summer", "autumn", "autumn", "autumn", "winter"];
for (var m = 0; m < 12; m++) assert.strictEqual(S.seasonOf(m), want[m], "month " + m);
assert.strictEqual(S.seasonOf(new Date(2026, 9, 24).getMonth()), "autumn");
// the PWA chrome: the Homestead sky's top band by day and by night, the old slate for the rest
assert.strictEqual(S.chromeOf("homestead"), "#5f98d6");
assert.strictEqual(S.chromeOf("dark-homestead"), "#0b1030");
assert.strictEqual(S.chromeOf("dense-admin"), "#1e2f42");
assert.strictEqual(S.chromeOf("toString"), "#1e2f42", "only the sky table's own keys");
`
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil {
		t.Fatalf("node skin clock test failed: %v\n%s", err, out)
	}
}

// The string contract of the inline script: sky.js's hours, the stored key
// winning before the clock is read, Auto forgetting the key, and the
// boundary timer plus the OS-preference listener that carry an open tab
// across dusk.
func TestSkinClock_ScriptContract(t *testing.T) {
	for _, want := range []string{
		`var DUSK = 19, DAWN = 7, KEY = 'beads-viewer:skin';`,
		`function isNightHour(h) { return h >= DUSK || h < DAWN; }`,
		`if (pick) { paint(pick); return; }`,
		`root.setAttribute('data-skin', s);`,
		`if (saved) store(saved);`,
		`pick = (s && s !== 'auto') ? s : '';`,
		`localStorage.removeItem(KEY)`,
		`setTimeout(function () { timer = 0; apply(); }, msToFlip(new Date()) + 50)`,
		`matchMedia('(prefers-color-scheme: dark)')`,
		`mq.addEventListener('change', apply)`,
		`'visibilitychange'`,
		// The season is stamped on every apply, stored skin or not.
		`root.setAttribute('data-season', seasonOf(new Date().getMonth()));
    if (pick) {`,
	} {
		if !strings.Contains(skinClockJS, want) {
			t.Errorf("skinClockJS lacks %s", want)
		}
	}
	if strings.Contains(skinClockJS, "</") {
		t.Error("skinClockJS must not contain \"</\": it is inlined in a <script>")
	}
}

// Every page that reads beads-viewer:skin resolves it the same way: the
// script is inlined in <head> (before any paint) and started with that
// page's day and night skins; nothing else reads the key or the OS
// preference on its own, so the viewer, /login and /lands agree on night.
func TestSkinClock_InlinedOnEveryFrontDoor(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	h := srv.handleLogin(loginPickerConfig{enabled: true, forwardAuthURL: "https://auth.example.com", gitlabEnabled: true, gitlabLabel: "Sign in with GitLab"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", rec.Code)
	}
	lands := landsDo(landsViewer(t, nil), http.MethodGet, "/lands/", principal{}, "", nil)
	if lands.Code != http.StatusOK {
		t.Fatalf("GET /lands/ = %d", lands.Code)
	}
	pages := map[string]struct{ body, start string }{
		"/agents": {renderThemedPage(t, "", "/agents"), `SKIN_CLOCK.start('homestead', 'dark-homestead', "");`},
		"/login":  {rec.Body.String(), `SKIN_CLOCK.start('homestead', 'dark-homestead');`},
		"/lands/": {lands.Body.String(), `SKIN_CLOCK.start('homestead', 'dark-homestead');`},
	}
	for path, p := range pages {
		head := p.body[:max(strings.Index(p.body, "</head>"), 0)]
		def, start := strings.Index(head, "var SKIN_CLOCK = (function"), strings.Index(head, p.start)
		if def < 0 || start < def {
			t.Errorf("%s: <head> must define SKIN_CLOCK and then run %s (def %d, start %d)", path, p.start, def, start)
		}
		if n := strings.Count(p.body, "'beads-viewer:skin'"); n != 1 {
			t.Errorf("%s names beads-viewer:skin %d times; only SKIN_CLOCK reads or writes it", path, n)
		}
		if n := strings.Count(p.body, "prefers-color-scheme: dark)')"); n != 1 {
			t.Errorf("%s reads the OS preference in script %d times; only SKIN_CLOCK does", path, n)
		}
	}
}
