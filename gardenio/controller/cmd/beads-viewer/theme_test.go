package main

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// S1j (gd-cxU8eyiGyR): the Dawn Meadow look for the beads viewer. These tests
// pin the contract between the Go side (theme.go, the render seam, the static
// route) and templates/static/meadow/ so the viewer and the login page cannot
// drift apart, and so BEADS_VIEWER_THEME=plain really is a whole rollback.

// chartThemeResources is where the login theme (S1h) keeps the originals the
// viewer's copies must match. Relative to this package directory.
const chartThemeResources = "../../../keycloak-theme/gardenio/login/resources"

// meadowMarkers are the three things the theme adds to every layout page:
// the html attribute meadow.css keys off, the stylesheet, and the scene.
var meadowMarkers = []string{
	`data-theme="meadow"`,
	`/assets/static/meadow/meadow.css`,
	`<div class="gd-scene" aria-hidden="true">`,
}

func renderThemedPage(t *testing.T, theme, path string) string {
	t.Helper()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	srv := NewServer(f, testLogger(), "", "", "", &fakeScreenClient{})
	if theme != "" {
		srv.theme = theme
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestThemeFromEnv(t *testing.T) {
	cases := map[string]string{
		"":        themeMeadow,
		"meadow":  themeMeadow,
		"plain":   themePlain,
		" Plain ": themePlain,
		"PLAIN":   themePlain,
		"nope":    themeMeadow, // a typo never strips the product's face
	}
	for in, want := range cases {
		if got := themeFromEnv(in); got != want {
			t.Errorf("themeFromEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

// The default Server renders the meadow: attribute, stylesheet, scene, and the
// sign in the nav. This is what every full-page render gets with no env set.
func TestLayout_MeadowIsTheDefaultLook(t *testing.T) {
	body := renderThemedPage(t, "", "/agents")
	for _, m := range meadowMarkers {
		if !strings.Contains(body, m) {
			t.Errorf("default render of /agents lacks %q", m)
		}
	}
	if !strings.Contains(body, `class="gd-sign"`) {
		t.Error("default render has no nav sign (.gd-sign)")
	}
	// The sign's text is its accessible name: no aria-hidden, no icon-only link.
	if strings.Contains(body, `class="gd-sign" aria-hidden`) {
		t.Error("the nav sign must not be aria-hidden: its text is the accessible name")
	}
	if !strings.Contains(body, `>gardenio</a>`) {
		t.Error("the nav sign must read 'gardenio'")
	}
}

// BEADS_VIEWER_THEME=plain is the whole rollback: nothing the theme adds may
// survive in the markup, and the plain nav is exactly the pre-theme one.
func TestLayout_PlainThemeIsACompleteRollback(t *testing.T) {
	body := renderThemedPage(t, themePlain, "/agents")
	for _, m := range meadowMarkers {
		if strings.Contains(body, m) {
			t.Errorf("plain render of /agents still contains %q", m)
		}
	}
	if strings.Contains(body, "gd-sign") || strings.Contains(body, "gd-scene") {
		t.Error("plain render still carries theme markup (gd-sign / gd-scene)")
	}
	if strings.Contains(body, `data-theme=`) {
		t.Error("plain render must not stamp any data-theme attribute")
	}
}

// Simple mode has no `/` home affordance by decision ka-0000007562 (Feed IS
// home); the sign honours that by linking to /feed there and to `/` only in
// vibe mode. nav_vibe_mode_test.go already asserts `href="/"` is vibe-only;
// this pins where the sign points in the default mode.
func TestLayout_SignLinksToFeedInSimpleMode(t *testing.T) {
	body := renderThemedPage(t, themeMeadow, "/agents")
	nav := body[strings.Index(body, `<nav aria-label="Site">`):strings.Index(body, "</nav>")]
	if !strings.Contains(nav, `<a class="gd-sign" href="/feed">gardenio</a>`) {
		t.Errorf("simple-mode sign must link to /feed, nav was:\n%s", nav)
	}
	if strings.Contains(nav, `href="/">`) {
		t.Error("simple-mode nav must not gain a `/` link through the sign")
	}
}

// The login picker is standalone (no layout) and sign-in-adjacent, so it
// carries the same contract: scene on by default, nothing under plain.
func TestLoginPicker_CarriesTheme(t *testing.T) {
	render := func(theme string) string {
		t.Helper()
		srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
		srv.theme = theme
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
			t.Fatalf("GET /login (%s) = %d, want 200\n%s", theme, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	meadow := render(themeMeadow)
	for _, m := range meadowMarkers {
		if !strings.Contains(meadow, m) {
			t.Errorf("meadow /login lacks %q", m)
		}
	}
	if !strings.Contains(meadow, `<body class="gd-login">`) {
		t.Error("meadow /login must mark its body .gd-login so meadow.css can frame the card instead of the body")
	}
	plain := render(themePlain)
	for _, m := range meadowMarkers {
		if strings.Contains(plain, m) {
			t.Errorf("plain /login still contains %q", m)
		}
	}
}

// The tiles and the font are copies of the chart's login theme. They must be
// byte-identical (one scene, two renderers), and the whole folder must stay
// under the 300 KB the plan allows.
func TestMeadowAssetsMirrorChartTheme(t *testing.T) {
	if _, err := os.Stat(chartThemeResources); err != nil {
		t.Skipf("chart theme resources not reachable from this checkout: %v", err)
	}
	var total int64
	checked := 0
	err := fs.WalkDir(staticFS, "templates/static/meadow", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ours, err := fs.ReadFile(staticFS, p)
		if err != nil {
			return err
		}
		total += int64(len(ours))
		rel, _ := filepath.Rel("templates/static/meadow", p)
		if rel == "meadow.css" {
			return nil // the viewer's own stylesheet (scene-meadow.css with viewer selectors)
		}
		theirs, err := os.ReadFile(filepath.Join(chartThemeResources, rel))
		if err != nil {
			t.Errorf("%s: no original in the chart theme (%v); the viewer must only carry copies of the login scene", rel, err)
			return nil
		}
		if !bytes.Equal(ours, theirs) {
			t.Errorf("%s differs from the chart's login theme copy; re-copy from %s (one scene, two renderers)", rel, chartThemeResources)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 14 {
		t.Errorf("checked only %d mirrored assets, want the 13 tiles + sprout + font + licence", checked)
	}
	const budget = 300 * 1024
	if total > budget {
		t.Errorf("templates/static/meadow is %d bytes, over the %d byte budget", total, budget)
	}
	t.Logf("meadow assets: %d files mirrored, %d bytes total", checked, total)
}

// Every url() in meadow.css is relative (same origin, no CDN, no data:), and
// the only font is the self-hosted one whose OFL licence ships next to it.
func TestMeadowCSS_SameOriginOnly(t *testing.T) {
	css, err := fs.ReadFile(staticFS, "templates/static/meadow/meadow.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`url\(\s*["']?([^"')]+)`).FindAllStringSubmatch(string(css), -1) {
		u := m[1]
		if strings.Contains(u, ":") || strings.HasPrefix(u, "/") {
			t.Errorf("meadow.css references %q; only relative same-origin URLs are allowed", u)
		}
		if _, err := fs.Stat(staticFS, "templates/static/meadow/"+u); err != nil {
			t.Errorf("meadow.css references %q, which is not embedded", u)
		}
	}
	if !strings.Contains(string(css), `pixelify-sans-latin-600-normal.woff2`) {
		t.Error("meadow.css must self-host the Pixelify Sans face")
	}
	if _, err := fs.Stat(staticFS, "templates/static/meadow/fonts/OFL-PixelifySans.txt"); err != nil {
		t.Error("the font's OFL licence must ship next to it")
	}
	// Only the two skins the theme is designed for may match: every rule is
	// gated on data-theme, so the four other candidate skins keep their look
	// and the plain theme sees none of it.
	for _, sel := range regexp.MustCompile(`(?m)^((?:html|:where\(html)[^{]*)\{`).FindAllStringSubmatch(string(css), -1) {
		if !strings.Contains(sel[1], `[data-theme="meadow"]`) {
			t.Errorf("selector %q is not gated on html[data-theme=\"meadow\"]", strings.TrimSpace(sel[1]))
		}
	}
}

// The static route serves the stylesheet, a tile and the font with usable
// content types (woff2 is not in Go's built-in table; theme.go registers it).
func TestStaticRoute_ServesMeadowAssets(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	cases := map[string]string{
		"/assets/static/meadow/meadow.css":                                 "text/css",
		"/assets/static/meadow/img/sun.svg":                                "image/svg+xml",
		"/assets/static/meadow/img/sprout.svg":                             "image/svg+xml",
		"/assets/static/meadow/fonts/pixelify-sans-latin-600-normal.woff2": "font/woff2",
	}
	for path, wantType := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("GET %s Content-Type = %q, want prefix %q", path, ct, wantType)
		}
	}
}
