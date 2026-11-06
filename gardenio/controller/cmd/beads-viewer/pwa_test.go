package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newPWATestServer builds the minimal Server the manifest handler needs: it
// touches only basePath and logger.
func newPWATestServer(basePath string) *Server {
	return &Server{
		basePath: basePath,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func decodeManifest(t *testing.T, rr *httptest.ResponseRecorder) webManifest {
	t.Helper()
	var m webManifest
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\nbody: %s", err, rr.Body.String())
	}
	return m
}

func TestHandleWebManifest_Headers(t *testing.T) {
	s := newPWATestServer("")
	rr := httptest.NewRecorder()
	s.handleWebManifest(rr, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// Browsers key manifest handling on this exact media type.
	if ct := rr.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("Content-Type = %q, want application/manifest+json", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc == "" {
		t.Errorf("Cache-Control not set; want a caching directive")
	}
}

func TestHandleWebManifest_CoreFields(t *testing.T) {
	s := newPWATestServer("")
	rr := httptest.NewRecorder()
	s.handleWebManifest(rr, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	m := decodeManifest(t, rr)

	// start_url must land on the Deck — installing to reach the triage surface
	// is the whole point of this step.
	if m.StartURL != "/deck" {
		t.Errorf("StartURL = %q, want /deck", m.StartURL)
	}
	if m.Scope != "/" {
		t.Errorf("Scope = %q, want /", m.Scope)
	}
	// display:standalone is what strips the browser chrome — the "feels like an
	// app" requirement. Without it the install is just a bookmark.
	if m.Display != "standalone" {
		t.Errorf("Display = %q, want standalone", m.Display)
	}
	if m.ThemeColor != pwaThemeColor {
		t.Errorf("ThemeColor = %q, want %q", m.ThemeColor, pwaThemeColor)
	}
	if m.Name == "" || m.ShortName == "" {
		t.Errorf("Name/ShortName must be non-empty, got %q / %q", m.Name, m.ShortName)
	}
}

func TestHandleWebManifest_IconsInstallable(t *testing.T) {
	s := newPWATestServer("")
	rr := httptest.NewRecorder()
	s.handleWebManifest(rr, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	m := decodeManifest(t, rr)

	// Chrome's installability bar requires at least one 192px and one 512px
	// PNG icon; Android's adaptive icon wants a maskable one. Assert all three.
	var has192, has512, hasMaskable bool
	for _, ic := range m.Icons {
		if ic.Type != "image/png" {
			t.Errorf("icon %q type = %q, want image/png", ic.Src, ic.Type)
		}
		if !strings.HasPrefix(ic.Src, "/assets/static/pwa/") {
			t.Errorf("icon src %q not under the static PWA path", ic.Src)
		}
		switch {
		case ic.Sizes == "192x192":
			has192 = true
		case ic.Sizes == "512x512" && ic.Purpose == "maskable":
			hasMaskable = true
		case ic.Sizes == "512x512":
			has512 = true
		}
	}
	if !has192 {
		t.Error("manifest is missing a 192x192 icon (Chrome installability requirement)")
	}
	if !has512 {
		t.Error("manifest is missing a 512x512 'any' icon (Chrome installability requirement)")
	}
	if !hasMaskable {
		t.Error("manifest is missing a maskable icon (Android adaptive-icon requirement)")
	}
}

func TestHandleWebManifest_HonorsBasePath(t *testing.T) {
	// Under unifiedHost the viewer is mounted at a strip-prefix. Every URL the
	// manifest emits must carry that prefix, or a home-screen launch requests
	// the wrong origin path and lands on a 404 / login wall.
	s := newPWATestServer("/beads")
	rr := httptest.NewRecorder()
	s.handleWebManifest(rr, httptest.NewRequest(http.MethodGet, "/beads/manifest.webmanifest", nil))
	m := decodeManifest(t, rr)

	if m.StartURL != "/beads/deck" {
		t.Errorf("StartURL = %q, want /beads/deck", m.StartURL)
	}
	if m.Scope != "/beads/" {
		t.Errorf("Scope = %q, want /beads/", m.Scope)
	}
	for _, ic := range m.Icons {
		if !strings.HasPrefix(ic.Src, "/beads/assets/static/pwa/") {
			t.Errorf("icon src %q missing base-path prefix", ic.Src)
		}
	}
}

// TestPWARoutes_ServedThroughRealMux drives the actual registered routes
// end-to-end: it proves the manifest route is wired into RegisterRoutes and
// that every icon the manifest advertises is fetchable through the same
// /assets/static/ FileServer the running server uses, with a PNG content-type.
// A green here is the closest headless proxy for "the browser can install it".
func TestPWARoutes_ServedThroughRealMux(t *testing.T) {
	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// 1. The manifest route resolves and decodes.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /manifest.webmanifest through mux = %d, want 200", rr.Code)
	}
	m := decodeManifest(t, rr)

	// 2. Every advertised icon is actually served by the mux as a PNG. This is
	// the check that would have caught a wrong path / missing embed / bad glob.
	for _, ic := range m.Icons {
		iconRR := httptest.NewRecorder()
		mux.ServeHTTP(iconRR, httptest.NewRequest(http.MethodGet, ic.Src, nil))
		if iconRR.Code != http.StatusOK {
			t.Errorf("GET %s through mux = %d, want 200", ic.Src, iconRR.Code)
			continue
		}
		if ct := iconRR.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("GET %s Content-Type = %q, want image/png", ic.Src, ct)
		}
	}

	// 3. The apple-touch-icon (referenced only from the HTML head, not the
	// manifest) is served too.
	appleRR := httptest.NewRecorder()
	mux.ServeHTTP(appleRR, httptest.NewRequest(http.MethodGet, "/assets/static/pwa/apple-touch-icon.png", nil))
	if appleRR.Code != http.StatusOK {
		t.Errorf("GET apple-touch-icon through mux = %d, want 200", appleRR.Code)
	}
}

// TestPWAHeadTagsRendered proves the layout head actually emits the PWA
// discovery tags on a real rendered page. Without the <link rel="manifest">
// and apple-* metas in the served HTML, the icons + manifest exist but no
// browser ever finds them — the install prompt never appears. The bead-show
// page (/gd-aaa) is a plain full-layout page, a good proxy for "every page".
func TestPWAHeadTagsRendered(t *testing.T) {
	mux := mkServer(t, newFake())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/gd-aaa", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /gd-aaa = %d, want 200", w.Code)
	}
	body := w.Body.String()

	wantFragments := []string{
		`<link rel="manifest" href="/manifest.webmanifest" crossorigin="use-credentials">`,
		`<meta name="theme-color" content="` + pwaThemeColor + `">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<meta name="mobile-web-app-capable" content="yes">`,
		`<link rel="apple-touch-icon" href="/assets/static/pwa/apple-touch-icon.png">`,
	}
	for _, frag := range wantFragments {
		if !strings.Contains(body, frag) {
			t.Errorf("rendered head missing PWA tag:\n  %s", frag)
		}
	}
	// The manifest fetch MUST be credentialed: the whole viewer is behind OIDC,
	// and an anonymous manifest fetch 401s (ka-0000003163 auth caveat).
	if !strings.Contains(body, `crossorigin="use-credentials"`) {
		t.Error("manifest link must use crossorigin=\"use-credentials\" so the auth cookie rides along behind OIDC")
	}
}

// TestPWAIconsEmbedded guards that the icon PNGs the manifest advertises are
// actually present in the embedded static FS and are real PNGs — otherwise the
// manifest points at 404s and the install prompt silently never appears.
func TestPWAIconsEmbedded(t *testing.T) {
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	for _, name := range []string{
		"templates/static/pwa/icon-192.png",
		"templates/static/pwa/icon-512.png",
		"templates/static/pwa/icon-maskable-512.png",
		"templates/static/pwa/apple-touch-icon.png",
		"templates/static/pwa/icon-32.png",
	} {
		b, err := staticFS.ReadFile(name)
		if err != nil {
			t.Errorf("embedded PWA asset %s missing: %v", name, err)
			continue
		}
		if len(b) < len(pngMagic) || string(b[:len(pngMagic)]) != string(pngMagic) {
			t.Errorf("embedded PWA asset %s is not a PNG", name)
		}
	}
}
