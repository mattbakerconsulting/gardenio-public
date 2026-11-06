package gardenhub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The splash (plan gd-EJGA3zDGCk) is one document at /garden/splash: the canvas, the still and its note, the stylesheet and
// the scene inlined, the base path on the body, and a bodyless 304 on a revisit. Its only requests are its own two files,
// three.js and the still, by URLs under the base path that carry their hash; nothing comes from another host. The start menu
// (gd-Fw6fffLZ3p) links to the Lands front of house and to the edge's sign-in back to /garden (gd-QDdwxnGdTY), both under the
// base path: links a visitor follows, which is why only src, link and import are held to be of this host.
func TestSplashRoute(t *testing.T) {
	mux := http.NewServeMux()
	(&Handler{BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden/splash", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET /garden/splash: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{
		`<canvas id="splash"`, `data-base="/v"`, "<script>" + splashServedJS + "</script>", "<figcaption>",
		`data-three="/v/garden/splash/three.module.min.js?v=` + splashThreeTag + `"`,
		`src="/v/garden/splash/still.png?v=` + splashStillTag + `"`,
		`<nav id="splash-menu"`, `<a id="splash-invite" href="/v/lands/">Request an invite</a>`,
		`<a id="splash-signin" href="/v/oauth2/start?rd=/v/garden">Sign in</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("splash missing %q", want)
		}
	}
	if m := regexp.MustCompile(`(?i)(src|<link[^>]+href)\s*=\s*["']?(https?:)?//|url\(\s*["']?(https?:)?//|@import|<script[^>]+src`).FindString(body); m != "" {
		t.Errorf("splash makes an external request: %q", m)
	}

	again := httptest.NewRequest("GET", "/garden/splash", nil)
	again.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, again)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revisit: %d with %d bytes, want a bodyless 304", rec.Code, rec.Body.Len())
	}

	// the two files the page fetches: embedded, typed, cached for a year
	for _, f := range []struct {
		path, ctype string
		body        []byte
	}{
		{"/garden/splash/three.module.min.js?v=" + splashThreeTag, "text/javascript", splashThree},
		{"/garden/splash/still.png?v=" + splashStillTag, "image/png", splashStill},
	} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", f.path, nil))
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), f.ctype) || !bytes.Equal(rec.Body.Bytes(), f.body) {
			t.Errorf("GET %s: %d %q, %d bytes", f.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=31536000") {
			t.Errorf("GET %s: Cache-Control %q, want a year", f.path, cc)
		}
	}

	// the script and the stylesheet inlined minified (gd-BqEPTwuyj6): a fallback to the source, which the minifier gives for
	// one it cannot parse or would miscompile, is a page nearly twice the size gzipped; each global the scripts read of one
	// another is still declared
	if len(splashServedJS) >= len(splashJS)*6/10 || len(splashServedCSS) >= len(splashCSS)*8/10 {
		t.Errorf("splash inlines %d of %d bytes of script and %d of %d of stylesheet: not minified (newDecls %v)", len(splashServedJS), len(splashJS), len(splashServedCSS), len(splashCSS), newDecls(splashJS))
	}
	for _, m := range regexp.MustCompile(`(?m)^var ([A-Z]+) = \(function`).FindAllStringSubmatch(splashJS, -1) {
		if !regexp.MustCompile(`(?:\bvar |,)` + m[1] + `=`).MatchString(splashServedJS) {
			t.Errorf("minified, the splash no longer declares var %s", m[1])
		}
	}
}

// The splash exported for the landing image (gd-QDdwxnGdTY): the three files in the directory, the page the viewer's
// template with its two links absolute on the app's origin and its two assets relative and byte for byte the embedded ones,
// and no request that needs beads-viewer: nothing by a rooted or absolute URL but the two links.
func TestExportSplash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site")
	if err := ExportSplash(dir, "https://gardenio.clonk.tv/"); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	for _, want := range []string{
		`<canvas id="splash"`, `data-base=""`, "<script>" + splashServedJS + "</script>",
		`data-three="./three.module.min.js?v=` + splashThreeTag + `"`,
		`src="./still.png?v=` + splashStillTag + `"`,
		`<a id="splash-invite" href="https://gardenio.clonk.tv/lands/">Request an invite</a>`,
		`<a id="splash-signin" href="https://gardenio.clonk.tv/oauth2/start?rd=/garden">Sign in</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exported splash missing %q", want)
		}
	}
	// splash.js import()s data-three, and import() takes a module specifier, not a URL: one that does not start ./, ../ or /
	// (or a scheme) is bare and only an import map resolves it, so a bare data-three fails to load and the exported page always
	// shows the still (gd-OeWiwpmr5Y). TestExportSplashUnderChromium sees it draw.
	if m := regexp.MustCompile(`data-three="([^"]*)"`).FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "./") {
		t.Errorf("exported splash's data-three is not ./-relative (%q): import() resolves a bare specifier only by an import map", m)
	}
	for _, m := range regexp.MustCompile(`(?:src|href|data-three)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if u := m[1]; strings.HasPrefix(u, "/") {
			t.Errorf("exported splash names %q, a URL on the host it is served from", u)
		}
	}
	if m := regexp.MustCompile(`(?i)(src|<link[^>]+href)\s*=\s*["']?(https?:)?//|url\(\s*["']?(https?:)?//|@import|<script[^>]+src`).FindString(body); m != "" {
		t.Errorf("exported splash makes an external request: %q", m)
	}
	for name, want := range map[string][]byte{"three.module.min.js": splashThree, "still.png": splashStill} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes (%v), want the embedded %d", name, len(got), err, len(want))
		}
	}
}

// The vendored three.js is the file web/vendor/VENDOR.md records: its sha256 matches, and it is the version recorded there.
// A re-vendor (make vendor-three) that changes the file without the record, or the record without the file, fails here.
func TestSplashThreeVendored(t *testing.T) {
	doc, err := os.ReadFile("web/vendor/VENDOR.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("\\| File sha256 \\| `([0-9a-f]{64})` \\|").FindSubmatch(doc)
	if m == nil {
		t.Fatal("VENDOR.md records no File sha256")
	}
	sum := sha256.Sum256(splashThree)
	if got := hex.EncodeToString(sum[:]); got != string(m[1]) {
		t.Errorf("three.module.min.js sha256 %s, VENDOR.md records %s", got, m[1])
	}
	v := regexp.MustCompile(`\| Version \| \d+\.(\d+)\.\d+ \(r(\d+)\) \|`).FindSubmatch(doc)
	if v == nil || string(v[1]) != string(v[2]) {
		t.Fatalf("VENDOR.md's Version row unreadable: %q", v)
	}
	if !bytes.Contains(splashThree[:4096], []byte(`="`+string(v[2])+`"`)) {
		t.Errorf("three.module.min.js does not declare REVISION %s near its top", v[2])
	}
}

// The splash's scene runs under node (testdata/splash_test.js) with the vendored three.js: the camera on the land, the sky,
// the stone on the brow with its plinth and gems, the almanac's light and its shadow, and the height fog; the renderer's
// settings and its frames on resize; the field map painted and the mark cut at boot; the still whenever WebGL cannot draw.
func TestSplashScene(t *testing.T) {
	r := nodeScript(t, "splash_test.js")
	t.Logf("splash: three r%v, %v, box at %v in view, fallbacks %v", r["three"], r["objects"], r["boxInView"], r["fallbacks"])
}

// The splash's view maths (web/splash/view.js, plan gd-I5QHVfGLbD step 1) run under node (testdata/splash_view_test.js) with
// the vendored three.js as the camera to check against: the zoom, yaw and pitch held to their bounds, the default view the
// designed frame, a zoom toward a corner keeping the corner's direction, a drag of the frame's height turning the pitch by
// the fov over the zoom, the wheel's and the pinch's factors, and ?z=&look= round-tripping, with nothing at the default.
func TestSplashView(t *testing.T) {
	r := nodeScript(t, "splash_view_test.js")
	t.Logf("view: worst direction moved %v rad, (yaw, pitch) at zoom 8 toward each point %v", r["worst"], r["zoomed"])
}

// The splash's time controls in Chromium (plan gd-EJGA3zDGCk step 8): the readout at a pinned ?t=, Tab to the scrubber and its
// arrow keys (10 minutes, an hour with Shift) relighting the scene and writing ?t= into the address, that address opened in
// a second page drawing the same frame, a click on the track, Play and Pause by keyboard, and Now back to the clock. What is
// asserted lives in testdata/splash_controls_browser.js.
func TestSplashControlsUnderChromium(t *testing.T) {
	underChromium(t, "splash_controls_browser.js", func() *httptest.Server {
		mux := http.NewServeMux()
		(&Handler{}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	})
}

// The splash's pointer gestures in Chromium (plan gd-I5QHVfGLbD step 3, gd-lKZIU12lAT): the wheel zooming toward the pointer
// and writing ?z= when quiet, a ctrl wheel harder, a mouse drag turning the yaw and writing look=, a two-finger pinch, a
// double tap zooming 2x and a double click at the most zoom back to the default, and the address opened in a second page
// drawing the same frame. What is asserted lives in testdata/splash_gestures_browser.js.
func TestSplashGesturesUnderChromium(t *testing.T) {
	underChromium(t, "splash_gestures_browser.js", func() *httptest.Server {
		mux := http.NewServeMux()
		(&Handler{}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	})
}

// The exported splash in Chromium (gd-QDdwxnGdTY, gd-OeWiwpmr5Y): ExportSplash's directory served as plain files under
// /landing/, as the landing image serves it, with no viewer behind it. The page loads three.js from beside itself and draws
// in WebGL (body.dataset.gl on), so a data-three that import() cannot resolve, which leaves the page on the still, fails here.
// What is asserted lives in testdata/splash_export_browser.js.
func TestExportSplashUnderChromium(t *testing.T) {
	underChromium(t, "splash_export_browser.js", func() *httptest.Server {
		dir := t.TempDir()
		if err := ExportSplash(dir, "https://gardenio.clonk.tv"); err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.Handle("/landing/", http.StripPrefix("/landing/", http.FileServer(http.Dir(dir))))
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	})
}

// The splash's quality tiers in Chromium (plan gd-EJGA3zDGCk step 9): a phone starting at the low tier, its frame times over a
// 10 s run read from the line the page logs and held to the 33 ms budget where the GL is hardware (reported, not asserted,
// on a software rasteriser such as CI's SwiftShader, which takes seconds a frame), reduced motion stopping the animation
// frames, a hidden page not drawing and pagehide losing the WebGL context; the laptop's high tier at a reduced size held to
// 16 ms on hardware GL, skipped with the reason on software GL. What is asserted lives in
// testdata/splash_quality_browser.js.
func TestSplashQualityUnderChromium(t *testing.T) {
	underChromium(t, "splash_quality_browser.js", func() *httptest.Server {
		mux := http.NewServeMux()
		(&Handler{}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	})
}

// The splash's ?perf=1 frame readout in Chromium (gd-8nCJKznQzj): on a 390 px phone, light and dark, the readout in the top
// corner carries the console's frame line with the tier and the budget, then the time running and the step-downs, written
// again every 2 s; it is selectable, not announced, inside the screen and clear of the time controls; without ?perf=1
// there is no readout. What is asserted lives in testdata/splash_perf_browser.js.
func TestSplashPerfUnderChromium(t *testing.T) {
	underChromium(t, "splash_perf_browser.js", func() *httptest.Server {
		mux := http.NewServeMux()
		(&Handler{}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	})
}
