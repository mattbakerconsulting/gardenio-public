package main

import (
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Homestead skin (plan gd-JaMxRKsc8z, step gd-SSXmSJtQNd): the viewer's
// default look, one of the switcher's entries next to Dense admin and friends,
// day (homestead) and night (dark-homestead). These tests pin the contract
// between layout.html / login.html, meadow.css and
// templates/static/homestead/homestead.css so the default cannot silently
// drift back to baseline, the skin cannot leak into the other skins, and the
// art stays same-origin.

const homesteadCSSPath = "templates/static/homestead/homestead.css"

func readHomesteadCSS(t *testing.T) string {
	t.Helper()
	css, err := fs.ReadFile(staticFS, homesteadCSSPath)
	if err != nil {
		t.Fatalf("homestead.css is not embedded: %v", err)
	}
	return string(css)
}

// Every full-page render stamps the Homestead as the skin and links its
// stylesheet, under the meadow theme and under the plain rollback alike: the
// skin is a look inside the plate, the theme is the scene around it.
func TestLayout_HomesteadIsTheDefaultSkin(t *testing.T) {
	for _, theme := range []string{themeMeadow, themePlain} {
		body := renderThemedPage(t, theme, "/agents")
		if !strings.Contains(body, `<html lang="en" data-skin="homestead"`) {
			t.Errorf("%s render of /agents does not default data-skin to homestead", theme)
		}
		if !strings.Contains(body, `/assets/static/homestead/homestead.css`) {
			t.Errorf("%s render of /agents does not link homestead.css", theme)
		}
		if !strings.Contains(body, `|| 'homestead'`) {
			t.Errorf("%s render: the skin switcher must fall back to homestead when nothing is stored", theme)
		}
		if strings.Contains(body, `<html lang="en" data-skin="baseline"`) {
			t.Errorf("%s render still stamps data-skin=\"baseline\" on <html>", theme)
		}
	}
}

// The switcher offers Auto, then both Homestead skins, and keeps every earlier
// candidate, so Dense admin and friends remain one click away.
func TestLayout_SkinSwitcherOffersHomestead(t *testing.T) {
	body := renderThemedPage(t, "", "/agents")
	i := strings.Index(body, `<select class="skin-switcher"`)
	if i < 0 {
		t.Fatal("no skin switcher in the nav")
	}
	sel := body[i : i+strings.Index(body[i:], "</select>")]
	// Auto (gd-yV1HX7zGNE) heads the list: selected when nothing is stored,
	// it hands the skin back to the clock (skin_clock.go).
	if !strings.HasPrefix(strings.TrimSpace(sel[strings.Index(sel, ">")+1:]), `<option value="auto">Auto (day or night by your clock)</option>`) {
		t.Error("the skin switcher's first entry must be Auto (day or night by your clock)")
	}
	if !strings.Contains(body, `sel.value = SKIN_CLOCK.auto() ? 'auto' : current;`) || !strings.Contains(body, `SKIN_CLOCK.set(sel.value);`) {
		t.Error("the switcher must select Auto when nothing is stored and store a choice through SKIN_CLOCK.set")
	}
	want := []string{
		`<option value="auto">Auto (day or night by your clock)</option>`,
		`<option value="homestead">Homestead</option>`,
		`<option value="dark-homestead">Homestead night</option>`,
		`<option value="baseline">Baseline</option>`,
		`<option value="dense-admin">Dense admin</option>`,
		`<option value="dark-terminal">Dark terminal</option>`,
		`<option value="tufte">Tufte</option>`,
	}
	last := -1
	for _, w := range want {
		j := strings.Index(sel, w)
		if j < 0 {
			t.Errorf("skin switcher lacks %s", w)
			continue
		}
		if j < last {
			t.Errorf("skin switcher option %s is out of order (Homestead first, then the earlier candidates)", w)
		}
		last = j
	}
}

// The sign-in picker is standalone (no layout) and must carry the same default
// and the skin's tokens: meadow.css frames its card with var(--bg), which only
// the skin defines.
func TestLoginPicker_DefaultsToHomestead(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	h := srv.handleLogin(loginPickerConfig{
		enabled:              true,
		forwardAuthURL:       "https://auth.example.com",
		keycloakLocalEnabled: true,
		keycloakLocalLabel:   "Sign in with password",
		gitlabEnabled:        true,
		gitlabLabel:          "Sign in with GitLab",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<html lang="en" data-skin="homestead"`) {
		t.Error("/login does not default data-skin to homestead")
	}
	if !strings.Contains(body, `/assets/static/homestead/homestead.css`) {
		t.Error("/login does not link homestead.css")
	}
}

// The sign-in picker's own colours are the skin's tokens (step 4.1), so it
// wears the Homestead day and night without the scene too: every colour,
// background and border in its inline style reads a var(), and a literal is
// allowed only as that var()'s fallback (the pre-skin look, for a stylesheet
// that did not load).
func TestLoginPicker_ColoursComeFromTheSkin(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	h := srv.handleLogin(loginPickerConfig{enabled: true, forwardAuthURL: "https://auth.example.com", keycloakLocalEnabled: true, keycloakLocalLabel: "Sign in with password"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	_, style, _ := strings.Cut(rec.Body.String(), "<style>")
	style, _, _ = strings.Cut(style, "</style>")
	fallback := regexp.MustCompile(`var\(--[a-z0-9-]+, [^()]*(?:\([^()]*\))?\)`)
	decls := regexp.MustCompile(`(?:^|[;{\s])((?:background|color|border(?:-color)?)\s*:\s*[^;}]+)`).FindAllStringSubmatch(style, -1)
	if len(decls) < 8 {
		t.Fatalf("found only %d colour declarations in the picker's style; did it move?", len(decls))
	}
	for _, d := range decls {
		if bare := fallback.ReplaceAllString(d[1], "var()"); regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\(`).MatchString(bare) {
			t.Errorf("login.html %q paints a literal colour, not the skin's token", strings.TrimSpace(d[1]))
		}
	}
	if !strings.Contains(style, "min-height: 100vh; box-sizing: border-box;") {
		t.Error("the picker's 100vh body must be border-box, or its padding scrolls the page past the scene")
	}
}

// The Lands front of house wears the Homestead too (step 4.1): the same
// default skin and day/night pair as the viewer, with the scene and without it
// (BEADS_VIEWER_THEME=plain), homestead.css imported as a cascade layer from
// the sessionless /lands/static/homestead/, and every --l-* colour token the
// pages paint with mapped from the skin's.
func TestLands_WearsTheHomestead(t *testing.T) {
	for _, theme := range []string{themeMeadow, themePlain} {
		srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
		srv.theme = theme
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		for _, p := range []string{"/lands/", "/lands/terms"} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s GET %s = %d", theme, p, rec.Code)
			}
			body := rec.Body.String()
			for _, want := range []string{
				`<html lang="en" data-skin="homestead"`,
				`SKIN_CLOCK.start('homestead', 'dark-homestead');`,
				`<style>@import url("/lands/static/homestead/homestead.css") layer(skin);</style>`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("%s GET %s: missing %s", theme, p, want)
				}
			}
		}
	}

	rec := httptest.NewRecorder()
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/lands/", nil))
	body := rec.Body.String()
	m := regexp.MustCompile(`(?m)^\s*html:is\(\[data-skin="homestead"\], \[data-skin="dark-homestead"\]\) \{([^}]*)\}`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("lands.html has no --l-* mapping gated on the Homestead skins")
	}
	mapped := map[string]bool{}
	for _, d := range regexp.MustCompile(`(--l-[a-z-]+):\s*var\(--[a-z-]+\)`).FindAllStringSubmatch(m[1], -1) {
		mapped[d[1]] = true
	}
	// The tokens the skin has a colour for; --l-ok-*, --l-ph-* and --l-shade keep the page's own (homestead.css has
	// no success, placeholder or shade token), by day from :root and by night from the dark palette.
	for _, tok := range []string{"--l-bg", "--l-card", "--l-text", "--l-muted", "--l-rule", "--l-accent", "--l-on-accent", "--l-focus", "--l-note-bg", "--l-note-fg", "--l-err-bg", "--l-err-fg"} {
		if !mapped[tok] {
			t.Errorf("lands.html does not map %s from the Homestead skin", tok)
		}
	}
}

// homestead.css may only ever match the two Homestead skins: every top-level
// selector is gated on html[data-skin="homestead"] or
// html[data-skin="dark-homestead"] (directly or through :is()/:where()), and
// never on data-theme, so the skin is independent of the scene's kill switch
// and the six other skins keep their look untouched.
func TestHomesteadCSS_ScopedToTheHomesteadSkins(t *testing.T) {
	css := readHomesteadCSS(t)
	sels := regexp.MustCompile(`(?m)^((?:html|:where\(html)[^{]*)\{`).FindAllStringSubmatch(css, -1)
	if len(sels) < 40 {
		t.Fatalf("found only %d top-level selectors; the regexp or the file changed shape", len(sels))
	}
	for _, m := range sels {
		sel := strings.TrimSpace(m[1])
		if !strings.Contains(sel, `[data-skin="homestead"]`) && !strings.Contains(sel, `[data-skin="dark-homestead"]`) {
			t.Errorf("selector %q is not gated on a Homestead skin", sel)
		}
		if strings.Contains(sel, `data-theme`) && !strings.Contains(sel, `:not([data-theme="meadow"])`) {
			t.Errorf("selector %q keys on data-theme; the skin must not depend on the scene", sel)
		}
		if strings.Contains(sel, `[data-skin^="dark"]`) || strings.Contains(sel, `[data-skin="baseline"]`) {
			t.Errorf("selector %q reaches into another skin", sel)
		}
	}
	// Nested blocks (the inline skins' `& { }` style) would escape the regexp
	// above, so the file must stay flat.
	if strings.Contains(css, "\n  & ") || strings.Contains(css, "& {") {
		t.Error("homestead.css must not use CSS nesting; every rule is a flat, gated selector")
	}
}

// Every url() in homestead.css is relative and embedded (the meadow's font
// and sprout by ../ path, the skin's own masks under img/), so the viewer's
// CSP (img-src 'self', font-src 'self') needs no change.
func TestHomesteadCSS_SameOriginOnly(t *testing.T) {
	css := readHomesteadCSS(t)
	dir := path.Dir(homesteadCSSPath)
	seen := 0
	for _, m := range regexp.MustCompile(`url\(\s*["']?([^"')]+)`).FindAllStringSubmatch(css, -1) {
		u := m[1]
		seen++
		if strings.Contains(u, ":") || strings.HasPrefix(u, "/") {
			t.Errorf("homestead.css references %q; only relative same-origin URLs are allowed", u)
			continue
		}
		// A sprite sheet's part is named by #view; the file is what must exist.
		file, _, _ := strings.Cut(u, "#")
		if _, err := fs.Stat(staticFS, path.Join(dir, file)); err != nil {
			t.Errorf("homestead.css references %q, which is not embedded (%v)", u, err)
		}
	}
	if seen < 3 {
		t.Errorf("expected the font and at least two masks, saw %d url()s", seen)
	}
	for _, want := range []string{"img/leaf.svg", "img/flower.svg", "../meadow/fonts/pixelify-sans-latin-600-normal.woff2"} {
		if !strings.Contains(css, `url("`+want+`")`) {
			t.Errorf("homestead.css should reference %s", want)
		}
	}
}

// The night skin earns the viewer's dark component overrides by name
// (html[data-skin^="dark"] matches dark-homestead) and must undo the terminal
// mannerisms that block inherits: the monospace body, the "$ " home prompt,
// the "▸ " bullets and the glyphs on empties, labels and banners.
func TestHomesteadCSS_NightUndoesTheTerminal(t *testing.T) {
	css := readHomesteadCSS(t)
	for _, want := range []string{
		`nav a[href$="/"]::before { content: none; }`,
		`h1::before { content: none; }`,
		`h3::before { content: none; }`,
		`html[data-skin="dark-homestead"] ul li::before { content: none; }`,
		`.field-list dt::after { content: none; }`,
		`.search-form label::before { content: none; }`,
		`.search-form button::before { content: none; }`,
		`html[data-skin="dark-homestead"] a:hover { background: transparent; padding: 0; }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("homestead.css lacks the night reset %q", want)
		}
	}
	if !strings.Contains(css, `html[data-skin="dark-homestead"] {`) || !strings.Contains(css, `color-scheme: dark;`) {
		t.Error("homestead.css must define the night tokens on html[data-skin=\"dark-homestead\"] with color-scheme: dark")
	}
}

// Motion is decorative and off under prefers-reduced-motion: every keyframe
// the skin declares is also switched off inside that media query.
func TestHomesteadCSS_MotionRespectsReducedMotion(t *testing.T) {
	// Comments out of the way, so a prose "animation" never reads as a rule.
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(readHomesteadCSS(t), "")
	names := regexp.MustCompile(`@keyframes\s+([a-z0-9-]+)`).FindAllStringSubmatch(css, -1)
	if len(names) == 0 {
		t.Fatal("expected at least one @keyframes in homestead.css")
	}
	i := strings.Index(css, "@media (prefers-reduced-motion: reduce)")
	if i < 0 {
		t.Fatal("homestead.css has no prefers-reduced-motion block")
	}
	block := css[i:]
	block = block[:strings.Index(block, "\n}\n")+3]
	if !strings.Contains(block, "animation: none") {
		t.Error("the reduced-motion block must set animation: none")
	}
	for _, n := range names {
		users := regexp.MustCompile(`([^{}]+)\{[^{}]*animation:[^;]*\b`+regexp.QuoteMeta(n[1])+`\b`).FindAllStringSubmatch(css, -1)
		if len(users) == 0 {
			t.Errorf("keyframes %s is declared but never used", n[1])
		}
		for _, u := range users {
			for _, sel := range strings.Split(u[1], ",") {
				sel = strings.TrimSpace(sel)
				sel = sel[strings.LastIndex(sel, "\n")+1:]
				if !strings.Contains(block, sel) {
					t.Errorf("selector %q animates %s but is not switched off under prefers-reduced-motion", sel, n[1])
				}
			}
		}
	}
}

// meadow.css paints the scene, plate, sign and controls for the Homestead
// skins too: every selector that matched baseline-or-dark now matches
// homestead as well, and nothing in it was left on the old pair.
func TestMeadowCSS_FramesTheHomesteadSkins(t *testing.T) {
	css, err := fs.ReadFile(staticFS, "templates/static/meadow/meadow.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	if strings.Contains(s, `:is([data-skin="baseline"], [data-skin^="dark"])`) {
		t.Error(`meadow.css still has a :is([data-skin="baseline"], [data-skin^="dark"]) selector that skips the homestead skin`)
	}
	n := strings.Count(s, `:is([data-skin="baseline"], [data-skin="homestead"], [data-skin^="dark"])`)
	if n < 30 {
		t.Errorf("meadow.css matches the homestead skin in only %d selectors; the scene, plate, nav, sign and login rules should all", n)
	}
}

// The static route serves the stylesheet and the masks with usable content
// types, so the skin paints on a cold cache.
func TestStaticRoute_ServesHomesteadAssets(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	cases := map[string]string{
		"/assets/static/homestead/homestead.css":            "text/css",
		"/assets/static/homestead/img/leaf.svg":             "image/svg+xml",
		"/assets/static/homestead/img/flower.svg":           "image/svg+xml",
		"/assets/static/homestead/img/vine-frame-day.svg":   "image/svg+xml",
		"/assets/static/homestead/img/vine-frame-night.svg": "image/svg+xml",
		"/assets/static/homestead/img/vine-rule-day.svg":    "image/svg+xml",
		"/assets/static/homestead/img/vine-rule-night.svg":  "image/svg+xml",
		"/assets/static/homestead/img/stone.svg":            "image/svg+xml",
		"/assets/static/homestead/img/earth.svg":            "image/svg+xml",
		"/assets/static/homestead/img/water.svg":            "image/svg+xml",
		"/assets/static/homestead/img/moss-day.svg":         "image/svg+xml",
		"/assets/static/homestead/img/moss-night.svg":       "image/svg+xml",
		"/assets/static/homestead/img/gems.svg":             "image/svg+xml",
		"/assets/static/homestead/img/figures.svg":          "image/svg+xml",
	}
	for p, wantType := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("GET %s Content-Type = %q, want %s", p, ct, wantType)
		}
	}
}

// The open plate (step 3.0, gd-2skiMOyS7f): the plate is paper laid over the
// meadow, translucent, with a vine 9-slice round its edge and a window of sky
// under the shelf. These pin the parts that keep it readable and contained:
// the alphas and washes are tokens per skin, cards, reading surfaces and the
// plain theme stay opaque, the sign-in picker is left alone, the wash only
// ever sits on the childless pseudo-elements (a filter on body would capture
// its fixed overlays), and the art is the generated pair per palette.
func TestHomesteadCSS_OpenPlate(t *testing.T) {
	css := readHomesteadCSS(t)
	day := css[strings.Index(css, `html[data-skin="homestead"] {`):]
	day = day[:strings.Index(day, "\n}\n")]
	night := css[strings.Index(css, `html[data-skin="dark-homestead"] {`):]
	night = night[:strings.Index(night, "\n}\n")]
	for name, block := range map[string]string{"day": day, "night": night} {
		for _, tok := range []string{"--hs-plate-alpha:", "--hs-plate-wash:", "--hs-plate-alpha-flat:"} {
			if !strings.Contains(block, tok) {
				t.Errorf("the %s tokens lack %s", name, tok)
			}
		}
		for _, art := range []string{"vine-frame", "vine-rule"} {
			if want := `url("img/` + art + `-` + name + `.svg")`; !strings.Contains(block, want) {
				t.Errorf("the %s tokens should name %s", name, want)
			}
		}
	}
	for _, want := range []string{
		// The plate and its vine; the picker is a scene with a card, not a plate.
		`body:not(.gd-login) {
  background: color-mix(in srgb, var(--bg) var(--hs-plate-alpha-flat), transparent);`,
		`border-image: var(--hs-art-vine-frame) 12 / calc(var(--gd-px) * 12) / var(--gd-px) round;`,
		// Long reading and the plain theme stay on solid paper.
		`body:not(.gd-login):has(.reading-col),`,
		`:not([data-theme="meadow"]) body:not(.gd-login) { background: var(--bg); }`,
		// The window, only where anchor positioning exists.
		`@supports (anchor-name: --hs-plate) {`,
		`anchor-name: --hs-shelf;`,
		`.gd-scene { z-index: -2; }`,
		// h1's vine rule.
		`var(--hs-art-vine-rule) 0 0 / calc(var(--gd-px) * 24) calc(var(--gd-px) * 4) no-repeat,`,
		`flex: 0 0 100%;`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("homestead.css lacks %q", want)
		}
	}
	// backdrop-filter only inside the window block, on body's pseudo-elements.
	block := css[strings.Index(css, "@supports (anchor-name: --hs-plate) {"):]
	block = block[:strings.Index(block, "\n}\n")]
	if strings.Count(css, "backdrop-filter:") != strings.Count(block, "backdrop-filter:") || !strings.Contains(block, "backdrop-filter: var(--hs-plate-wash);") {
		t.Error("the wash (backdrop-filter) must live only in the anchored window block, on body::before/::after")
	}
	for _, line := range strings.Split(css, "\n") {
		if strings.Contains(line, "filter:") && strings.Contains(line, "body:not(.gd-login) {") {
			t.Errorf("a filter on body itself would capture its fixed-position overlays: %q", line)
		}
	}
	// The 9-slice's slice numbers are image cells, so the frame must carry an intrinsic size.
	frame, err := fs.ReadFile(staticFS, "templates/static/homestead/img/vine-frame-day.svg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(frame), `width="56" height="56"`) {
		t.Error("vine-frame-day.svg must declare width=56 height=56: border-image-slice 12 counts its cells")
	}
}

// The art budget (step 3.1, gd-b2hzcYUTji): the skin's art is one flat folder
// of SVGs under 60 KB together, and every url() in homestead.css names a file
// that is embedded; a #fragment must be a <view> its sheet declares.
func TestHomesteadArt_UnderBudget(t *testing.T) {
	const dir = "templates/static/homestead/img"
	const budget = 60 * 1024
	entries, err := fs.ReadDir(staticFS, dir)
	if err != nil {
		t.Fatalf("%s is not embedded: %v", dir, err)
	}
	total := 0
	for _, e := range entries {
		name := path.Join(dir, e.Name())
		if e.IsDir() {
			t.Errorf("%s is a directory; the art is one flat folder of SVGs", name)
			continue
		}
		b, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatal(err)
		}
		if path.Ext(name) != ".svg" || !strings.HasPrefix(string(b), "<svg") {
			t.Errorf("%s is not an SVG; the art budget is SVG only", name)
		}
		total += len(b)
	}
	if total >= budget {
		t.Errorf("the art under %s is %d bytes, over the %d-byte budget", dir, total, budget)
	}

	css := readHomesteadCSS(t)
	urls := regexp.MustCompile(`url\(\s*["']?([^"')]+)`).FindAllStringSubmatch(css, -1)
	for _, m := range urls {
		file, view, _ := strings.Cut(m[1], "#")
		b, err := fs.ReadFile(staticFS, path.Join(path.Dir(homesteadCSSPath), file))
		if err != nil {
			t.Errorf("homestead.css references %q, which does not resolve to an embedded file (%v)", m[1], err)
			continue
		}
		if view != "" && !strings.Contains(string(b), `<view id="`+view+`"`) {
			t.Errorf("homestead.css references %q, but %s declares no <view id=%q>", m[1], file, view)
		}
	}
}

// Textures (3.1) are shade, not paint: every ink in stone, earth and water is
// drawn at an alpha, and laid over either skin's paper and slate it moves the
// ground by at most 6 in CIELAB lightness, so every text ink the skins set
// keeps 4.5:1 on the darkest and lightest textured pixel.
func TestHomesteadArt_TexturesStayFaint(t *testing.T) {
	css := readHomesteadCSS(t)
	skins := map[string]map[string]string{
		"day":   homesteadTokens(t, css, `html[data-skin="homestead"] {`),
		"night": homesteadTokens(t, css, `html[data-skin="dark-homestead"] {`),
	}
	ink := regexp.MustCompile(`<path fill="(#[0-9a-f]{6})"(?: fill-opacity="([0-9.]+)")?`)
	for _, tex := range []string{"stone", "earth", "water"} {
		b, err := fs.ReadFile(staticFS, "templates/static/homestead/img/"+tex+".svg")
		if err != nil {
			t.Fatal(err)
		}
		inks := ink.FindAllStringSubmatch(string(b), -1)
		if len(inks) == 0 {
			t.Fatalf("%s.svg has no inks", tex)
		}
		for name, tok := range skins {
			for _, ground := range []string{"--bg", "--gd-surface", "--gd-bg"} {
				g := hexRGB(t, tok[ground])
				for _, m := range inks {
					if m[2] == "" {
						t.Errorf("%s.svg paints %s at full strength; a texture ink needs a fill-opacity", tex, m[1])
						continue
					}
					a, _ := strconv.ParseFloat(m[2], 64)
					px := blendOver(hexRGB(t, m[1]), a, g)
					if d := math.Abs(lstar(px) - lstar(g)); d > 6 {
						t.Errorf("%s.svg ink %s/%s moves the %s %s by %.1f L*, over 6", tex, m[1], m[2], name, ground, d)
					}
					for _, text := range []string{"--text", "--gd-muted", "--accent"} {
						if c := contrast(hexRGB(t, tok[text]), px); c < 4.5 {
							t.Errorf("%s %s on %s under %s.svg ink %s is %.2f:1, under 4.5", name, text, ground, tex, m[1], c)
						}
					}
				}
			}
		}
	}
}

// The gems (3.1) gleam apart: each band's keyframes move one gem at a time
// through glint, sparkle, glint (0.6s in three steps) and back to rest, and
// with the band's duration every gem comes round again after 9 to 14 seconds,
// never starting together with another gem on its band.
func TestHomesteadCSS_GemsGleamApart(t *testing.T) {
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(readHomesteadCSS(t), "")
	bands := regexp.MustCompile(`animation: (hs-gleam-[a-z]+) ([0-9.]+)s step-end [0-9.]+s infinite;`).FindAllStringSubmatch(css, -1)
	if len(bands) != 3 {
		t.Fatalf("want three gleaming bands (top, foot, sign) with step-end timing, found %d", len(bands))
	}
	gems := 0
	frame := regexp.MustCompile(`(?m)^  ([0-9.]+)% \{ background-position-y: ([^;]+); \}$`)
	for _, b := range bands {
		name := b[1]
		dur, _ := strconv.ParseFloat(b[2], 64)
		i := strings.Index(css, "@keyframes "+name+" {")
		if i < 0 {
			t.Fatalf("%s is animated but has no @keyframes", name)
		}
		block := css[i : i+strings.Index(css[i:], "\n}\n")]
		type kf struct {
			at   float64
			vals []string
		}
		var kfs []kf
		for _, m := range frame.FindAllStringSubmatch(block, -1) {
			at, _ := strconv.ParseFloat(m[1], 64)
			kfs = append(kfs, kf{at * dur / 100, strings.Split(m[2], ", ")})
		}
		if len(kfs) < 3 || kfs[0].at != 0 || kfs[len(kfs)-1].at != dur {
			t.Fatalf("%s: want keyframes from 0%% to 100%%, got %d", name, len(kfs))
		}
		rest := kfs[len(kfs)-1].vals
		var starts []float64
		for layer := range rest {
			still := true
			for _, k := range kfs {
				still = still && k.vals[layer] == rest[layer]
			}
			if still {
				continue // a layer that never moves is not a gem (the foot band's gardener)
			}
			gems++
			var mine []float64
			for k := 0; k < len(kfs)-1; k++ {
				prev := rest
				if k > 0 {
					prev = kfs[k-1].vals
				}
				if kfs[k].vals[layer] == rest[layer] || prev[layer] != rest[layer] {
					continue
				}
				e, steps := k, 0
				for e < len(kfs) && kfs[e].vals[layer] != rest[layer] {
					e, steps = e+1, steps+1
				}
				if steps != 3 {
					t.Errorf("%s gem %d: a gleam takes %d steps, want 3", name, layer, steps)
				}
				if l := kfs[e].at - kfs[k].at; math.Abs(l-0.6) > 0.05 {
					t.Errorf("%s gem %d: a gleam lasts %.2fs, want 0.6s", name, layer, l)
				}
				mine = append(mine, kfs[k].at)
			}
			if len(mine) == 0 {
				t.Errorf("%s gem %d never gleams", name, layer)
				continue
			}
			for j, s := range mine {
				next := dur + mine[0]
				if j+1 < len(mine) {
					next = mine[j+1]
				}
				if gap := next - s; gap < 9 || gap > 14 {
					t.Errorf("%s gem %d: %.2fs between gleams, want 9 to 14", name, layer, gap)
				}
				for _, o := range starts {
					if math.Abs(o-s) < 0.6 {
						t.Errorf("%s: two gems gleam together at %.2fs", name, s)
					}
				}
			}
			starts = append(starts, mine...)
		}
	}
	if gems < 4 || gems > 6 {
		t.Errorf("found %d gems; the temple has four to six", gems)
	}
}

// Empty states (3.1): "nothing yet" carries .empty-yet and grows the sprout
// with the watering can; "nothing matched" keeps the blossom with the cat.
func TestHomesteadCSS_EmptyStatesSproutOrBlossom(t *testing.T) {
	css := readHomesteadCSS(t)
	for _, want := range []string{
		`.empty.empty-yet::before {`,
		`mask-image: url("../meadow/img/sprout.svg");`,
		`p.empty::after {`,
		`p.empty.empty-yet::after { background-position-x: calc(var(--gd-px) * -12); }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("homestead.css lacks %q", want)
		}
	}
	for file, text := range map[string]string{
		"templates/partials/plan_steps.html": `<p class="empty empty-yet">No steps yet.`,
		"templates/agents.html":              `<p class="empty empty-yet">No Agents &mdash; running agents will appear here.`,
		"templates/index.html":               `<p class="empty empty-yet">Ready queue empty.`,
	} {
		b, err := fs.ReadFile(templateFS, file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), text) {
			t.Errorf("%s: want %s", file, text)
		}
	}
}

// Living signals (3.2): the butterflies (day) and fireflies (night) hold
// still while the pointer rests on the meadow off the plate, or while the
// keyboard is at work inside it, on the three skins the scene frames. Only
// that layer pauses: clouds, grass and stars keep their own pace.
func TestMeadowCSS_SceneLifePausesOnHover(t *testing.T) {
	css, err := fs.ReadFile(staticFS, "templates/static/meadow/meadow.css")
	if err != nil {
		t.Fatal(err)
	}
	gate := `html[data-theme="meadow"]:is([data-skin="baseline"], [data-skin="homestead"], [data-skin^="dark"])`
	for _, want := range []string{
		gate + `:hover:not(:has(> body:hover)) .gd-scene > .gd-l6,`,
		gate + `:has(> body :focus-visible) .gd-scene > .gd-l6 { animation-play-state: paused; }`,
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("meadow.css lacks the scene pause %q", want)
		}
	}
	if n := strings.Count(string(css), "animation-play-state"); n != 1 {
		t.Errorf("meadow.css sets animation-play-state %d times; only the butterflies/fireflies rule should", n)
	}
}

// Butterflies you can see (3.8, gd-wyucyyLuAz): layer F still drifts a cell
// at a time at the 3.2 pace, and its sprite (a 320 x 110 cell tile drawn on
// the scene's foot) carries 10 to 12 butterflies 5 or 6 cells across, about
// 11 per 1280 px of laptop meadow at 18 to 24 px each. On a phone (3 px a
// cell, so 390 px is 130 cells) the plate hides all but the foot strip, about
// 37 cells tall: every 130-cell stretch of the tile holds a whole butterfly
// inside that strip.
func TestMeadowButterflies_SeenOnPhoneAndLaptop(t *testing.T) {
	css, err := fs.ReadFile(staticFS, "templates/static/meadow/meadow.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "animation: gd-meadow-flit 400s steps(320) infinite;") {
		t.Error("layer F must keep the 3.2 flit: 400s, steps(320), one cell per step")
	}
	svg, err := fs.ReadFile(staticFS, "templates/static/meadow/img/butterflies.svg")
	if err != nil {
		t.Fatal(err)
	}
	const tileW, tileH, footStrip, phoneCells = 320, 110, 37, 130
	if !strings.Contains(string(svg), fmt.Sprintf(`viewBox="0 0 %d %d"`, tileW, tileH)) {
		t.Fatalf("butterflies.svg must stay a %d x %d cell tile (meadow.css sizes it so)", tileW, tileH)
	}
	filled := map[[2]int]bool{}
	for _, m := range regexp.MustCompile(`M(\d+) (\d+)h(\d+)v(\d+)h-\d+z`).FindAllStringSubmatch(string(svg), -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		for j := 0; j < h; j++ {
			for i := 0; i < w; i++ {
				filled[[2]int{x + i, y + j}] = true
			}
		}
	}
	// One butterfly = one 4-connected group of cells (none sits on the seam).
	type box struct{ x0, y0, x1, y1 int }
	var flies []box
	seen := map[[2]int]bool{}
	for c := range filled {
		if seen[c] {
			continue
		}
		b := box{c[0], c[1], c[0], c[1]}
		stack := [][2]int{c}
		seen[c] = true
		for len(stack) > 0 {
			p := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			b.x0, b.y0 = min(b.x0, p[0]), min(b.y0, p[1])
			b.x1, b.y1 = max(b.x1, p[0]), max(b.y1, p[1])
			for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				q := [2]int{p[0] + d[0], p[1] + d[1]}
				if filled[q] && !seen[q] {
					seen[q] = true
					stack = append(stack, q)
				}
			}
		}
		flies = append(flies, b)
	}
	if n := len(flies); n < 10 || n > 12 {
		t.Errorf("butterflies.svg draws %d butterflies; want 10 to 12 per tile (per 1280 px on a laptop)", n)
	}
	for _, b := range flies {
		if w := b.x1 - b.x0 + 1; w < 5 || w > 6 {
			t.Errorf("butterfly at (%d,%d) is %d cells across; want 5 or 6 (18 to 24 px at 4 px a cell)", b.x0, b.y0, w)
		}
	}
	for x0 := 0; x0 < tileW; x0++ {
		ok := false
		for _, b := range flies {
			start := (b.x0 - x0 + tileW) % tileW
			if b.y0 >= tileH-footStrip && start+(b.x1-b.x0) < phoneCells {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("a phone showing cells %d to %d of the tile has no butterfly in the %d-cell foot strip", x0, (x0+phoneCells-1)%tileW, footStrip)
			break
		}
	}
}

// Living signals (3.2): a row or card that arrives after first paint rises
// two cells in three stepped frames, keyed on htmx's .htmx-added (present
// only between a swap and its settle, never on first paint). The offset is a
// transform, so nothing shifts layout; every element that takes the offset
// carries the stepped transition; reduced motion drops both; and the two
// streams that insert rows without htmx (the feed and the agents table) put
// the class on for htmx's settle delay.
func TestHomesteadCSS_RowsRiseOnSwapsOnly(t *testing.T) {
	css := readHomesteadCSS(t)
	const gate = `html:is([data-skin="homestead"], [data-skin="dark-homestead"])`
	const step = `transform 360ms steps(3, jump-none)`
	offset := gate + ` :is(.feed-card, .agents-row, .bead-row).htmx-added,
` + gate + ` .comments-section.htmx-added .comment-new { transform: translateY(calc(var(--gd-px) * 2)); }`
	if !strings.Contains(css, offset) {
		t.Fatalf("homestead.css lacks the rise-in offset rule:\n%s", offset)
	}
	for _, want := range []string{
		gate + ` :where(.feed-card, .agents-row, .comment) { transition: ` + step + `; }`,
		`transition: background-color 120ms ease-out, ` + step + `;`, // .bead-row keeps its hover
		// layout.html's skin-agnostic 250ms glide would override the transition.
		gate + ` .comment-new { animation: comment-flash 1.5s ease-out; }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("homestead.css lacks %q", want)
		}
	}
	// The htmx-keyed rules move nothing but the transform.
	bare := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	for _, m := range regexp.MustCompile(`([^{}]*\.htmx-added[^{}]*)\{([^{}]*)\}`).FindAllStringSubmatch(bare, -1) {
		for _, decl := range strings.Split(m[2], ";") {
			prop, _, _ := strings.Cut(strings.TrimSpace(decl), ":")
			if prop != "" && prop != "transform" {
				t.Errorf("rule %q sets %s; the rise-in may only set transform", strings.TrimSpace(m[1]), prop)
			}
		}
	}
	i := strings.Index(css, "@media (prefers-reduced-motion: reduce)")
	if i < 0 {
		t.Fatal("homestead.css has no prefers-reduced-motion block")
	}
	block := css[i:]
	block = block[:strings.Index(block, "\n}\n")+3]
	for _, want := range []string{
		gate + ` :where(.feed-card, .agents-row, .comment) { transition: none; }`,
		gate + ` .bead-row,`,
		gate + ` .comments-section.htmx-added .comment-new { transform: none; }`,
		gate + ` :is(.feed-card, .agents-row, .bead-row).htmx-added,`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the reduced-motion block lacks %q", want)
		}
	}
	for _, file := range []string{"templates/static/feed.js", "templates/static/agents_stream.js"} {
		b, err := fs.ReadFile(staticFS, file)
		if err != nil {
			t.Fatal(err)
		}
		js := string(b)
		if !strings.Contains(js, `el.classList.add('htmx-added');`) ||
			!strings.Contains(js, `getComputedStyle(el).transform;`) ||
			!strings.Contains(js, `setTimeout(function () { el.classList.remove('htmx-added'); }, 20);`) ||
			!strings.Contains(js, "riseIn(") {
			t.Errorf("%s does not give its streamed rows the htmx arrival (riseIn)", file)
		}
	}
	layout, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(layout), `document.addEventListener('htmx:afterSwap', function(evt) {
    if (/homestead$/.test(document.documentElement.getAttribute('data-skin') || '')) getComputedStyle(evt.target).transform;`) {
		t.Error("layout.html must read the swapped-in style on htmx:afterSwap, so the rise-in starts even when no frame lands before the settle")
	}
}

// Cadence (3.2, the /garden feedback in gd-OpU3ePStLF): the skin's motion is
// leisurely. No animation or moving transition runs faster than 300ms, and
// nothing loops faster than the breathing dot (2.4s) unless the pointer is
// holding it there (the sign's sprout wiggles only under :hover).
func TestHomesteadCSS_LeisurelyCadence(t *testing.T) {
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(readHomesteadCSS(t), "")
	dur := regexp.MustCompile(`(?:^|\s)(\d*\.?\d+)(ms|s)\b`)
	seconds := func(v string) (float64, bool) {
		m := dur.FindStringSubmatch(v)
		if m == nil {
			return 0, false
		}
		f, _ := strconv.ParseFloat(m[1], 64)
		if m[2] == "ms" {
			f /= 1000
		}
		return f, true
	}
	seen := 0
	for _, m := range regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		sel := strings.TrimSpace(m[1])
		for _, decl := range strings.Split(m[2], ";") {
			prop, val, ok := strings.Cut(strings.TrimSpace(decl), ":")
			if !ok || (prop != "animation" && prop != "transition") {
				continue
			}
			for _, part := range splitTopLevel(val) {
				part = strings.TrimSpace(part)
				if part == "none" || (prop == "transition" && !strings.HasPrefix(part, "transform")) {
					continue // colour fades on hover are feedback, not motion
				}
				d, ok := seconds(part)
				if !ok {
					t.Errorf("%s on %q has no duration: %q", prop, sel, part)
					continue
				}
				seen++
				if d < 0.3 {
					t.Errorf("%s %q on %q runs %.2fs; nothing here is faster than 300ms", prop, part, sel, d)
				}
				if strings.Contains(part, "infinite") && !strings.Contains(sel, ":hover") && d < 2.4 {
					t.Errorf("%q on %q loops every %.2fs; nothing loops faster than the breathing dot (2.4s)", part, sel, d)
				}
			}
		}
	}
	if seen < 8 {
		t.Errorf("checked only %d animations and transitions; the parser or the file changed shape", seen)
	}
}

// splitTopLevel splits a CSS list on the commas outside parentheses, so
// steps(3, jump-none) stays one part.
func splitTopLevel(v string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range v {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, v[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, v[start:])
}

// The season (step 3.4, gd-WP5vygf7Ly): every data-season SKIN_CLOCK can
// stamp has a block for each skin that names the season's vine tokens, vine
// art, mask sheet and ink, every band a pseudo-element shows is painted in an
// ink the season defines, and nothing in the season section moves.
func TestHomesteadSeason_EverySeasonBothSkinsNoMotion(t *testing.T) {
	css := readHomesteadCSS(t)
	for _, season := range []string{"spring", "summer", "autumn", "winter"} {
		for _, skin := range []string{"homestead", "dark-homestead"} {
			tok := homesteadSeasonTokens(t, css, skin, season)
			for _, name := range []string{"--gd-vine", "--gd-vine-dark", "--hs-art-vine-frame", "--hs-art-vine-rule", "--hs-season-art", "--hs-season-rows", "--hs-season-ink"} {
				if tok[name] == "" {
					t.Errorf("%s in %s sets no %s", skin, season, name)
				}
			}
			if want := `url("img/season-` + season + `.svg")`; tok["--hs-season-art"] != want {
				t.Errorf("%s in %s: --hs-season-art = %q, want %q", skin, season, tok["--hs-season-art"], want)
			}
			// Autumn and winter paint a second band (russet, dry stems) under the first.
			if two := season == "autumn" || season == "winter"; two != (tok["--hs-season-ink-2"] != "") {
				t.Errorf("%s in %s: --hs-season-ink-2 = %q; autumn and winter, and only they, have a second band", skin, season, tok["--hs-season-ink-2"])
			}
		}
	}

	start := strings.Index(css, "/* ---- Season (3.4)")
	if start < 0 {
		t.Fatal("homestead.css has no Season (3.4) section")
	}
	section := css[start:]
	section = section[:strings.Index(section[1:], "/* ---- ")+1]
	rules := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(section, "")
	for _, moving := range []string{"animation", "@keyframes", "transition"} {
		if strings.Contains(rules, moving) {
			t.Errorf("the season section uses %s; a season accent holds still (the scene has motion enough)", moving)
		}
	}
	// Each accent layer: the bed's ::after and ::before, the grass's ::after.
	for _, want := range []string{
		`.gd-scene > .gd-l4::after,`,
		`.gd-scene > .gd-l4::before,`,
		`.gd-scene > .gd-l5::after {`,
		`mask: var(--hs-season-art) 0 0 / calc(var(--gd-px) * 320) calc(var(--gd-px) * var(--hs-season-rows)) repeat-x;`,
		`background: var(--hs-season-ink-2);`,
		`html[data-skin="homestead"][data-season="summer"] .gd-scene > .gd-l1 {`,
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("the season section lacks %q", want)
		}
	}
}

// The vine tints with the season (3.4): --gd-vine and --gd-vine-dark are the
// leaf and deep-leaf inks of the art each block names, so the tokens and the
// generated SVGs (testdata/homestead_art.js) cannot drift apart, and the mask
// sheets are as tall as the bands the CSS cuts them into.
func TestHomesteadSeason_VineTokensMatchArt(t *testing.T) {
	css := readHomesteadCSS(t)
	dir := path.Dir(homesteadCSSPath)
	for _, season := range []string{"spring", "summer", "autumn", "winter"} {
		for _, skin := range []string{"homestead", "dark-homestead"} {
			tok := homesteadSeasonTokens(t, css, skin, season)
			for _, art := range []string{"--hs-art-vine-frame", "--hs-art-vine-rule"} {
				file := strings.TrimSuffix(strings.TrimPrefix(tok[art], `url("`), `")`)
				b, err := fs.ReadFile(staticFS, path.Join(dir, file))
				if err != nil {
					t.Errorf("%s in %s: %s %q is not embedded: %v", skin, season, art, file, err)
					continue
				}
				for _, ink := range []string{"--gd-vine", "--gd-vine-dark"} {
					if !strings.Contains(string(b), `<path fill="`+tok[ink]+`"`) {
						t.Errorf("%s in %s: %s %s is not an ink of %s", skin, season, ink, tok[ink], file)
					}
				}
			}
			b, err := fs.ReadFile(staticFS, path.Join(dir, "img/season-"+season+".svg"))
			if err != nil {
				t.Fatal(err)
			}
			if want := `width="320" height="` + tok["--hs-season-rows"] + `"`; !strings.Contains(string(b), want) {
				t.Errorf("season-%s.svg should declare %s, the sheet --hs-season-rows sizes", season, want)
			}
		}
	}
	// Spring is 3.0's vine, so the plate with no season and in spring is one plate.
	day := homesteadSeasonTokens(t, css, "homestead", "spring")
	if day["--hs-art-vine-frame"] != `url("img/vine-frame-day.svg")` {
		t.Errorf("spring's vine should be 3.0's own (vine-frame-day.svg), got %s", day["--hs-art-vine-frame"])
	}
}

// homesteadSeasonTokens reads one skin's tokens for one season, values verbatim.
func homesteadSeasonTokens(t *testing.T, css, skin, season string) map[string]string {
	t.Helper()
	head := `html[data-skin="` + skin + `"][data-season="` + season + `"] {`
	i := strings.Index(css, head)
	if i < 0 {
		t.Fatalf("homestead.css has no %s block", head)
	}
	block := css[i : i+strings.Index(css[i:], "\n}\n")]
	tok := map[string]string{}
	for _, m := range regexp.MustCompile(`(--[a-z0-9-]+): ([^;]+);`).FindAllStringSubmatch(block, -1) {
		tok[m[1]] = m[2]
	}
	return tok
}

// homesteadTokens reads a skin's hex tokens, following one var() hop.
func homesteadTokens(t *testing.T, css, head string) map[string]string {
	t.Helper()
	i := strings.Index(css, head)
	if i < 0 {
		t.Fatalf("no %s block", head)
	}
	block := css[i : i+strings.Index(css[i:], "\n}\n")]
	tok := map[string]string{}
	for _, m := range regexp.MustCompile(`(--[a-z-]+): ([^;]+);`).FindAllStringSubmatch(block, -1) {
		tok[m[1]] = m[2]
	}
	for k, v := range tok {
		if ref, ok := strings.CutPrefix(v, "var("); ok {
			tok[k] = tok[strings.TrimSuffix(ref, ")")]
		}
	}
	return tok
}

func hexRGB(t *testing.T, s string) [3]float64 {
	t.Helper()
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		t.Fatalf("not a hex colour: %q", s)
	}
	return [3]float64{float64(r), float64(g), float64(b)}
}

func blendOver(fg [3]float64, a float64, bg [3]float64) [3]float64 {
	var o [3]float64
	for i := range o {
		o[i] = bg[i]*(1-a) + fg[i]*a
	}
	return o
}

func relLuminance(c [3]float64) float64 {
	var l [3]float64
	for i, v := range c {
		v /= 255
		if v <= 0.04045 {
			l[i] = v / 12.92
		} else {
			l[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
}

func lstar(c [3]float64) float64 {
	y := relLuminance(c)
	if y > 216.0/24389 {
		return 116*math.Cbrt(y) - 16
	}
	return y * 24389 / 27
}

func contrast(a, b [3]float64) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
