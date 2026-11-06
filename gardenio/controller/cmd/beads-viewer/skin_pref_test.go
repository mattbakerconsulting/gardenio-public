package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The skin preference (skin_pref.go, gd-Ah9Zr8CxYB, plan step 4.2): the chosen
// skin lives in the user bead's preferences and the browser's copy is only the
// pre-paint cache.

func getAsViewer(t *testing.T, mux http.Handler, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d\n%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// POST /settings/skin writes only the skin key: the blob is merged, so keys
// the viewer does not model (gb prefs set) and the string-form vibe_mode
// survive.
func TestUpdateSkin_StoresAndKeepsOtherKeys(t *testing.T) {
	f := settingsFake(Preferences{})
	f.beads["gd-user-op"].Fields["preferences"] = `{"vibe_mode":"true","default_spawn_role":"crew","from_gb":"kept"}`
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedPost("/settings/skin", url.Values{"skin": {"dark-homestead"}}))
	if w.Code != http.StatusNoContent {
		t.Fatalf("POST /settings/skin = %d, want 204\n%s", w.Code, w.Body.String())
	}
	if f.updateFieldsCalls != 1 || f.updateFieldsLastID != "gd-user-op" {
		t.Fatalf("want one write to gd-user-op, got %d to %q", f.updateFieldsCalls, f.updateFieldsLastID)
	}
	blob, ok := f.updateFieldsLastVal["preferences"].(map[string]any)
	if !ok {
		t.Fatalf("preferences written as %T, want the merged map", f.updateFieldsLastVal["preferences"])
	}
	for k, want := range map[string]string{"skin": "dark-homestead", "vibe_mode": "true", "default_spawn_role": "crew", "from_gb": "kept"} {
		if blob[k] != want {
			t.Errorf("preferences[%s] = %v, want %q", k, blob[k], want)
		}
	}
	if len(f.updateFieldsLastVal) != 1 {
		t.Errorf("the skin write touched %d fields, want only preferences", len(f.updateFieldsLastVal))
	}
}

func TestUpdateSkin_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		skin   string
		authed bool
		noBead bool
		daemon error
		want   int
		writes int
	}{
		{name: "unknown skin", skin: "neon", authed: true, want: http.StatusBadRequest},
		{name: "empty skin", skin: "", authed: true, want: http.StatusBadRequest},
		{name: "anonymous", skin: "tufte", want: http.StatusUnauthorized},
		{name: "no user bead", skin: "tufte", authed: true, noBead: true, want: http.StatusNotFound},
		{name: "daemon error", skin: "tufte", authed: true, daemon: errors.New("boom"), want: http.StatusBadGateway, writes: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := settingsFake(Preferences{})
			if c.noBead {
				f.beads["gd-user-op"].Fields["email"] = "someone-else@example.com"
			}
			f.updateFieldsErr = c.daemon
			mux := mkServer(t, f)
			req := authedPost("/settings/skin", url.Values{"skin": {c.skin}})
			if !c.authed {
				req.Header.Del("X-Auth-Request-Email")
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != c.want {
				t.Errorf("status = %d, want %d", w.Code, c.want)
			}
			if f.updateFieldsCalls != c.writes {
				t.Errorf("writes = %d, want %d", f.updateFieldsCalls, c.writes)
			}
		})
	}
}

// The preferences form has no skin field, so saving it must keep the stored
// skin rather than wipe it.
func TestUpdateSettings_KeepsSkin(t *testing.T) {
	f := settingsFake(Preferences{Skin: "tufte"})
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authedPost("/settings", url.Values{"default_spawn_role": {"crew"}}))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d", w.Code)
	}
	got, ok := f.updateFieldsLastVal["preferences"].(Preferences)
	if !ok || got.Skin != "tufte" {
		t.Errorf("preferences after a form save = %#v, want Skin tufte kept", f.updateFieldsLastVal["preferences"])
	}
}

// Every page hands the stored skin to SKIN_CLOCK.start before the first paint,
// a stored value outside the set is dropped (the cache decides), and the nav
// switcher saves only when there is a user bead to save to and the viewer may
// reach POST /settings/skin (admin-only, so a scoped viewer keeps it local).
func TestSkinPref_HandedToSkinClockInHead(t *testing.T) {
	cases := []struct {
		name, stored, wantStart string
		anonymous, wantSave     bool
		scoped                  bool
	}{
		{name: "stored night", stored: "dark-homestead", wantStart: `SKIN_CLOCK.start('homestead', 'dark-homestead', "dark-homestead");`, wantSave: true},
		{name: "stored auto", stored: "auto", wantStart: `SKIN_CLOCK.start('homestead', 'dark-homestead', "auto");`, wantSave: true},
		{name: "never chosen", wantStart: `SKIN_CLOCK.start('homestead', 'dark-homestead', "");`, wantSave: true},
		{name: "junk value", stored: `x" onload="`, wantStart: `SKIN_CLOCK.start('homestead', 'dark-homestead', "");`, wantSave: true},
		{name: "anonymous", anonymous: true, wantStart: `SKIN_CLOCK.start('homestead', 'dark-homestead', "");`},
		{name: "scoped viewer", stored: "tufte", scoped: true, wantStart: `SKIN_CLOCK.start('homestead', 'dark-homestead', "tufte");`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mux := mkServer(t, settingsFake(Preferences{Skin: c.stored}))
			req := httptest.NewRequest(http.MethodGet, "/gd-user-op", nil)
			if !c.anonymous {
				req.Header.Set("X-Auth-Request-Email", authorEmail)
			}
			if c.scoped {
				req = req.WithContext(withViewerScope(req.Context(), viewerScope{Projects: []string{"gardenio"}}))
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			body := w.Body.String()
			if !strings.Contains(body, `<select class="skin-switcher"`) {
				t.Fatal("page has no skin switcher")
			}
			head := body[:max(strings.Index(body, "</head>"), 0)]
			if !strings.Contains(head, c.wantStart) {
				t.Errorf("<head> lacks %s", c.wantStart)
			}
			if got := strings.Contains(body, `<select class="skin-switcher" aria-label="Page style" data-skin-save="/settings/skin">`); got != c.wantSave {
				t.Errorf("switcher carries the save URL = %v, want %v", got, c.wantSave)
			}
		})
	}
}

// The Look section: one radio card per skin in switcher order, the stored skin
// checked, each card's preview painted in its own colours.
func TestSettings_LookSection(t *testing.T) {
	body := getAsViewer(t, mkServer(t, settingsFake(Preferences{Skin: "dense-admin"})), "/settings")
	i := strings.Index(body, `id="look-choices"`)
	if i < 0 {
		t.Fatal("settings page has no Look section")
	}
	if strings.Index(body, `id="look-heading"`) > i || !strings.Contains(body, `>Look</h2>`) {
		t.Error("the Look section must open with its Look heading")
	}
	if strings.Index(body, `id="settings-block"`) < i {
		t.Error("Look must come before the preferences block")
	}
	last := i
	for _, s := range viewerSkins {
		radio := `<input type="radio" name="skin" value="` + s.Value + `"`
		j := strings.Index(body, radio)
		if j < 0 {
			t.Errorf("Look lacks a card for %s", s.Value)
			continue
		}
		if j < last {
			t.Errorf("Look card %s is out of switcher order", s.Value)
		}
		last = j
		checked := strings.HasPrefix(body[j+len(radio):], " checked>")
		if checked != (s.Value == "dense-admin") {
			t.Errorf("card %s checked = %v; only the stored skin is checked", s.Value, checked)
		}
		if !strings.Contains(body, `style="--look-sky: `+s.Sky+`; --look-paper: `+s.Paper+`; --look-ink: `+s.Ink+`; --look-accent: `+s.Accent+`"`) {
			t.Errorf("card %s preview is not painted in its colours", s.Value)
		}
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Error("a Look preview colour was rejected by html/template")
	}
	for _, want := range []string{`SKIN_CLOCK.set(e.target.value);`, `window.bvSaveSkin(v)`, `id="look-adopt"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Look section lacks %s", want)
		}
	}
	// Never chosen: Auto is checked until the script corrects it to the cache.
	body = getAsViewer(t, mkServer(t, settingsFake(Preferences{})), "/settings")
	if !strings.Contains(body, `<input type="radio" name="skin" value="auto" checked>`) {
		t.Error("with no stored skin the Look section checks Auto")
	}
}

// The nav switcher and the Look section offer the same closed set that
// POST /settings/skin accepts, in the same order.
func TestSkinPref_SwitcherMatchesSet(t *testing.T) {
	body := getAsViewer(t, mkServer(t, settingsFake(Preferences{})), "/settings")
	i := strings.Index(body, `<select class="skin-switcher"`)
	if i < 0 {
		t.Fatal("no skin switcher")
	}
	sel := body[i : i+strings.Index(body[i:], "</select>")]
	var got []string
	for _, m := range regexp.MustCompile(`<option value="([^"]+)">`).FindAllStringSubmatch(sel, -1) {
		got = append(got, m[1])
	}
	var want []string
	for _, s := range viewerSkins {
		want = append(want, s.Value)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("switcher offers %v, want viewerSkins %v", got, want)
	}
}

// The PWA chrome is the Homestead sky: theme-color is the top band of
// meadow.css's .gd-sky by day (SKIN_CLOCK retints it by night), the splash is
// homestead.css's --gd-sky-fallback, and the script's sky table holds the
// same colours.
func TestSkinPref_ChromeIsTheHomesteadSky(t *testing.T) {
	meadow, err := os.ReadFile("templates/static/meadow/meadow.css")
	if err != nil {
		t.Fatal(err)
	}
	homestead, err := os.ReadFile("templates/static/homestead/homestead.css")
	if err != nil {
		t.Fatal(err)
	}
	firstBand := func(selector string) string {
		m := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{\s*background-image: linear-gradient\(to bottom,\s*(#[0-9a-f]{6})`).FindSubmatch(meadow)
		if m == nil {
			t.Fatalf("meadow.css has no sky gradient at %s", selector)
		}
		return string(m[1])
	}
	if got := firstBand(`html[data-theme="meadow"] .gd-scene > .gd-sky`); got != homesteadSkyDay {
		t.Errorf("day sky top band = %s, homesteadSkyDay = %s", got, homesteadSkyDay)
	}
	if got := firstBand(`html[data-theme="meadow"][data-skin^="dark"] .gd-scene > .gd-sky`); got != homesteadSkyNight {
		t.Errorf("night sky top band = %s, homesteadSkyNight = %s", got, homesteadSkyNight)
	}
	fallbacks := regexp.MustCompile(`--gd-sky-fallback: (#[0-9a-f]{6});`).FindAllSubmatch(homestead, -1)
	if len(fallbacks) != 2 || string(fallbacks[0][1]) != homesteadSkyFallbackDay || string(fallbacks[1][1]) != homesteadSkyFallbackNight {
		t.Errorf("homestead.css --gd-sky-fallback (day, night) must be %s, %s", homesteadSkyFallbackDay, homesteadSkyFallbackNight)
	}
	if want := `var SKY = { 'homestead': '` + homesteadSkyDay + `', 'dark-homestead': '` + homesteadSkyNight + `' }`; !strings.Contains(skinClockJS, want) {
		t.Errorf("skinClockJS lacks %s", want)
	}
	if pwaThemeColor != homesteadSkyDay || pwaBackgroundColor != homesteadSkyFallbackDay {
		t.Errorf("manifest colours = %s / %s, want the day sky %s / %s", pwaThemeColor, pwaBackgroundColor, homesteadSkyDay, homesteadSkyFallbackDay)
	}
	s := newPWATestServer("")
	rr := httptest.NewRecorder()
	s.handleWebManifest(rr, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if m := decodeManifest(t, rr); m.BackgroundColor != homesteadSkyFallbackDay {
		t.Errorf("manifest background_color = %s, want the sky fallback %s", m.BackgroundColor, homesteadSkyFallbackDay)
	}
}
