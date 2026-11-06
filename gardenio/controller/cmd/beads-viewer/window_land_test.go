package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/planwindow"
)

// A land window (Lands: the land window, plan gd-4iVYF9tGsn, step LW.4
// gd-OJOsIH52NE): the same token and route as a plan window, answered with a
// land view (internal/planwindow/testdata/land).

func windowLandFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "planwindow", "testdata", "land", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWindowViewIsLand(t *testing.T) {
	for body, want := range map[string]bool{
		string(windowLandFixture(t, "daemon")): true,
		`{"land":"x","plans":[]}`:              true,
		`{"plan":{"title":"x"}}`:               false,
		`{"land":null,"plan":{"title":"x"}}`:   false,
		`{}`:                                   false,
		`{"land":`:                             false,
	} {
		if got := windowViewIsLand([]byte(body)); got != want {
			t.Errorf("windowViewIsLand(%.40s) = %v, want %v", body, got, want)
		}
	}
}

// TestWindow_LandPage: a land window's page is the World's canvas (hidden
// until land.js shows it) over a plain list of the land's plans, which is the
// whole page with JS off. It links nowhere, has no form, no inline script and
// no token, unfurls as text, and hygiene withholds what it hits.
func TestWindow_LandPage(t *testing.T) {
	api := &fakeWindowAPI{body: windowLandFixture(t, "mixed")}
	h, _ := windowServerFor(t, api, nil)
	w := windowGet(h, "/w/"+testWindowTok)
	if w.Code != http.StatusOK {
		t.Fatalf("land page = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<title>Orchard — a window on a land</title>",
		`<meta property="og:title" content="Orchard">`,
		`<meta property="og:description" content="5 open plans · 5 waiting on the operator · 3 crew at work">`,
		`<script src="/w/static/land.js"></script>`,
		"A window on a land · read-only",
		"<h1>Orchard</h1>",
		`<div class="w-world" id="w-world" hidden>`,
		`<canvas id="w-cv" tabindex="0" role="img" aria-label="Map of Orchard.`,
		`<aside class="w-card" id="w-card" aria-labelledby="w-card-h" hidden></aside>`,
		`<li data-plan="plan-1" class="w-st-working">`, `<span class="w-what">Grow the orchard</span>`,
		"Building · Now: Plant · 2 of 5 steps done · 2 waiting on the operator · 2 crew at work",
		`<li data-plan="plan-2" class="w-st-todo">`, "Sketch a pond", "Draft · 0 of 2 steps done",
		`<li data-plan="plan-6" class="w-st-done">`, "Clear the brambles", "Finished · Complete · 1 of 1 steps done",
		"<span class=\"w-what\">" + planwindow.WithheldText + "</span>",
		"Shared from gardenio Lands · read-only · updated",
		"This link stops working on 29 November 2026.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("land page lacks %q", want)
		}
	}
	for _, never := range []string{testWindowTok, "ops@example.com", "<a ", "og:image", "og:url", "<form", "SKIN_CLOCK", "window.js", "plan.json", "a window on a plan"} {
		if strings.Contains(body, never) {
			t.Errorf("land page has %q", never)
		}
	}
	if n, src := strings.Count(body, "<script"), strings.Count(body, "<script src="); n != 1 || src != 1 {
		t.Errorf("%d scripts, %d with src: want land.js alone, and the CSP allows no inline script", n, src)
	}
	if r := windowGet(h, "/w/"+testWindowTok+"?refresh=1"); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `id="w-plans"`) {
		t.Errorf("refresh = %d", r.Code)
	}
}

// TestWindow_LandJSON: land.json is the cleaned land view shaped for the
// scene, behind the same per-window cache and ETag as plan.json; plan.json on
// a land window, and land.json on a plan window, are the uniform 404.
func TestWindow_LandJSON(t *testing.T) {
	api := &fakeWindowAPI{body: windowLandFixture(t, "mixed")}
	h, clock := windowServerFor(t, api, nil)
	ref := windowGet(h, "/w/not-a-token").Body.String()
	page, lj := "/w/"+testWindowTok, "/w/"+testWindowTok+"/land.json"

	windowGet(h, page)
	r := windowGet(h, lj)
	etag := r.Result().Header.Get("ETag")
	if r.Code != http.StatusOK || etag == "" || r.Result().Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("land.json = %d, ETag %q", r.Code, etag)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(r.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != "closed_plans gardeners plans seeds" {
		t.Errorf("land.json keys = %v, want the scene's", got)
	}
	want, err := json.Marshal(planwindow.ToScene(planwindow.CleanLand(mustDecodeLand(t, windowLandFixture(t, "mixed")))))
	if err != nil {
		t.Fatal(err)
	}
	if r.Body.String() != string(want) {
		t.Errorf("land.json is not ToScene(CleanLand(view)):\n got %s\nwant %s", r.Body, want)
	}
	if strings.Contains(r.Body.String(), "ops@example.com") || strings.Contains(r.Body.String(), testWindowTok) {
		t.Errorf("land.json carries a withheld title or the token: %s", r.Body)
	}
	if nm := windowGet(h, lj, "If-None-Match", etag); nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
		t.Errorf("land.json with its ETag = %d, want 304", nm.Code)
	}
	if p := windowGet(h, "/w/"+testWindowTok+"/plan.json"); p.Code != http.StatusNotFound || p.Body.String() != ref {
		t.Errorf("plan.json on a land window = %d, want the one 404 page", p.Code)
	}
	if api.n() != 1 || api.calls[0].views != 1 {
		t.Errorf("daemon calls = %+v; want one read, for the page view, shared by land.json", api.calls)
	}
	clock.add(windowViewTTL / 2)
	windowGet(h, lj)
	if api.n() != 1 {
		t.Errorf("a read inside the TTL: %d reads", api.n())
	}
	clock.add(windowViewTTL)
	windowGet(h, lj)
	if api.n() != 2 || api.calls[1].views != 0 {
		t.Errorf("after the TTL = %+v; want one more read with no views", api.calls)
	}

	plan := &fakeWindowAPI{body: windowFixture(t, "phased")}
	h, _ = windowServerFor(t, plan, nil)
	if w := windowGet(h, lj); w.Code != http.StatusNotFound || w.Body.String() != ref {
		t.Errorf("land.json on a plan window = %d, want the one 404 page", w.Code)
	}
	if w := windowGet(h, page); !strings.Contains(w.Body.String(), `<script src="/w/static/window.js"></script>`) || strings.Contains(w.Body.String(), "land.js") {
		t.Error("a plan window's page does not load window.js alone")
	}
}

func mustDecodeLand(t *testing.T, b []byte) planwindow.LandPayload {
	t.Helper()
	p, err := planwindow.DecodeLand(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestWindow_LandJS: land.js is the skin clock, gardenhub's page-scene.js
// bundle and window-world.js, which finds land.json from its own location.
func TestWindow_LandJS(t *testing.T) {
	h, _ := windowServerFor(t, &fakeWindowAPI{}, nil)
	w := windowGet(h, "/w/static/land.js")
	js := w.Body.String()
	if w.Code != http.StatusOK || w.Result().Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("land.js = %d %q", w.Code, w.Result().Header.Get("Content-Type"))
	}
	sceneJS, ok := gardenhub.SceneScript()
	if !ok {
		t.Fatal("gardenhub's page-scene.js bundle did not build")
	}
	// the bundle as /garden serves it, minified, so it is found whole rather than by a line of its source
	clock, scene, glue := strings.Index(js, "var SKIN_CLOCK"), strings.Index(js, sceneJS), strings.Index(js, "'/land.json'")
	if clock < 0 || scene < clock || glue < scene || !strings.Contains(js, "SKIN_CLOCK.start('homestead', 'dark-homestead');") || !strings.Contains(js, "If-None-Match") {
		t.Errorf("land.js is not the skin clock, then the scene, then the glue (%d, %d, %d)", clock, scene, glue)
	}
}

// The land routes are served outside the auth chain, with PW-D9's headers.
func TestWindow_LandOutsideTheAuthChain(t *testing.T) {
	h, _ := windowServerFor(t, &fakeWindowAPI{body: windowLandFixture(t, "daemon")}, nil)
	for p, want := range map[string]int{
		"/w/" + testWindowTok:                200,
		"/w/" + testWindowTok + "/land.json": 200,
		"/w/" + testWindowTok + "/plan.json": 404,
		"/w/static/land.js":                  200,
	} {
		w := windowGet(h, p)
		if w.Code != want {
			t.Errorf("GET %s = %d, want %d", p, w.Code, want)
		}
		if w.Result().Header.Get("Content-Security-Policy") != windowContentSecurityPolicy || w.Result().Header.Get("Cache-Control") != "private, no-store" || len(w.Result().Header.Values("Set-Cookie")) != 0 {
			t.Errorf("GET %s: headers %v", p, w.Result().Header)
		}
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/w/"+testWindowTok+"/land.json", nil))
	if post.Code != http.StatusNotFound {
		t.Errorf("POST land.json = %d, want 404", post.Code)
	}
}

// TestWindowLandUnderChromium opens a land window's page in a real Chromium
// (testdata/window_land_browser.js), at a phone's and a desktop's size, by day
// and by night: land.js draws the World on the canvas and shows it, a tap on
// a plot and a plan's name in the list each open its read-only card, Escape
// closes it, nothing is fetched outside /w/ and the skin, and with JS off the
// page is the plain list. WINDOW_LAND_SHOTS=<dir> keeps a screenshot per
// state; WINDOW_LAND_PROBES=<probes.js> adds the commons-ux probes to them.
func TestWindowLandUnderChromium(t *testing.T) {
	skip := t.Skipf
	if os.Getenv("GARDENHUB_BROWSER_REQUIRED") == "1" {
		skip = t.Fatalf
	}
	node, err := exec.LookPath("node")
	if err != nil {
		skip("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		skip("playwright-core not installed under %s (make test-gardenhub-browser)", mods)
	}
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.window = newWindowCache(&fakeWindowAPI{body: windowLandFixture(t, "mixed")})
	app := http.NewServeMux()
	srv.RegisterRoutes(app)
	hs := httptest.NewServer(app)
	t.Cleanup(hs.Close)
	script, _ := filepath.Abs(filepath.Join("testdata", "window_land_browser.js"))
	cmd := exec.Command(node, script, hs.URL+"/w/"+testWindowTok, mods)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		skip("no Chromium for playwright-core: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("land window checks failed: %v\n%s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
