package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planwindow"
)

const (
	testWindowTok   = "gw_Zm9yZ2V0LW1lLW5vdC1fXw"
	testWindowOther = "gw_b3RoZXItd2luZG93LW9rIQ"
)

// fakeWindowAPI answers WindowView from body or err and records each call.
type fakeWindowAPI struct {
	mu    sync.Mutex
	body  []byte
	err   error
	calls []fakeWindowCall
}

type fakeWindowCall struct {
	token string
	views int64
}

func (f *fakeWindowAPI) WindowView(_ context.Context, token string, views int64) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeWindowCall{token, views})
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

func (f *fakeWindowAPI) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func windowFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "planwindow", "testdata", "payload", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// windowClock is a settable clock for the cache.
type windowClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *windowClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *windowClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// windowServerFor is a viewer whose Window reads api, mounted as main.go
// mounts it: the /w/ patterns on an outer mux whose "/" is the auth chain
// (here, a 401 for everyone), so a test request never passes any auth.
func windowServerFor(t *testing.T, api windowAPI, logs *bytes.Buffer) (http.Handler, *windowClock) {
	t.Helper()
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	if logs != nil {
		srv.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	clock := &windowClock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	srv.window = newWindowCache(api)
	srv.window.now = clock.now
	app := http.NewServeMux()
	srv.RegisterRoutes(app)
	outer := http.NewServeMux()
	outer.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing authorization token", http.StatusUnauthorized)
	}))
	mountWindow(outer, app)
	return outer, clock
}

func windowGet(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Safari/604.1")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWindowPatternsArePublic(t *testing.T) {
	m := registeredViewerRoutes(t)
	for _, p := range windowPublicPatterns {
		if viewerRouteClasses[p] != routePublic {
			t.Errorf("%s is served without a session but is not routePublic", p)
		}
		if !slicesContains(m.patterns, p) {
			t.Errorf("%s is not registered", p)
		}
	}
}

func TestWellFormedWindowToken(t *testing.T) {
	for tok, want := range map[string]bool{
		testWindowTok:                    true,
		testWindowOther:                  true,
		"":                               false,
		"gw_":                            false,
		"gw_short":                       false,
		"gx_Zm9yZ2V0LW1lLW5vdC1fXw":      false,
		"gw_Zm9yZ2V0LW1lLW5vdC1fX":       false,
		"gw_Zm9yZ2V0LW1lLW5vdC1fXww":     false,
		"gw_Zm9yZ2V0LW1lLW5vdC1fX+":      false,
		"gw_Zm9yZ2V0LW1lLW5vdC1fX=":      false,
		"Bearer gw_Zm9yZ2V0LW1lLW5vdC1f": false,
	} {
		if got := wellFormedWindowToken(tok); got != want {
			t.Errorf("wellFormedWindowToken(%q) = %v, want %v", tok, got, want)
		}
	}
}

// TestWindow_UniformNotFound: a malformed token never reaches the daemon, and
// every daemon refusal or failure is the same 404 page as a malformed token.
func TestWindow_UniformNotFound(t *testing.T) {
	api := &fakeWindowAPI{}
	h, _ := windowServerFor(t, api, nil)
	ref := windowGet(h, "/w/not-a-token")
	if ref.Code != http.StatusNotFound || !strings.Contains(ref.Body.String(), "No window here") {
		t.Fatalf("malformed token: %d %s", ref.Code, ref.Body.String())
	}
	for _, p := range []string{"/w/gw_short", "/w/gw_Zm9yZ2V0LW1lLW5vdC1fX+", "/w/not-a-token/plan.json", "/w/" + testWindowTok + "/", "/w/" + testWindowTok + "/other", "/w/"} {
		if w := windowGet(h, p); w.Code != http.StatusNotFound || w.Body.String() != ref.Body.String() {
			t.Errorf("GET %s = %d, want the one 404 page", p, w.Code)
		}
	}
	if api.n() != 0 {
		t.Errorf("malformed tokens reached the daemon %d times", api.n())
	}

	for name, err := range map[string]error{
		"401 unknown or expired": &beadsapi.APIError{StatusCode: 401, Message: "invalid token"},
		"403":                    &beadsapi.APIError{StatusCode: 403, Message: "a window opens only GET /v1/windows/self/view"},
		"422 too large":          &beadsapi.APIError{StatusCode: 422, Message: "this plan is too large to show through a window"},
		"500":                    &beadsapi.APIError{StatusCode: 500, Message: "failed to read the plan"},
		"503":                    &beadsapi.APIError{StatusCode: 503, Message: "window lookup unavailable"},
		"transport":              errors.New("dial tcp: connection refused"),
	} {
		api := &fakeWindowAPI{err: err}
		h, _ := windowServerFor(t, api, nil)
		for _, p := range []string{"/w/" + testWindowTok, "/w/" + testWindowTok + "/plan.json"} {
			if w := windowGet(h, p); w.Code != http.StatusNotFound || w.Body.String() != ref.Body.String() {
				t.Errorf("%s: GET %s = %d %q, want the one 404 page", name, p, w.Code, w.Body.String())
			}
		}
	}
	bad := &fakeWindowAPI{body: []byte(`{"plan":`)}
	h, _ = windowServerFor(t, bad, nil)
	if w := windowGet(h, "/w/"+testWindowTok); w.Code != http.StatusNotFound || w.Body.String() != ref.Body.String() {
		t.Errorf("an undecodable payload = %d, want the one 404 page", w.Code)
	}
	post := httptest.NewRequest(http.MethodPost, "/w/"+testWindowTok, strings.NewReader("x"))
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, post)
	if pw.Code != http.StatusNotFound {
		t.Errorf("POST under /w/ = %d, want 404", pw.Code)
	}
}

// TestWindow_Headers: PW-D9 on every response under /w/, and no cookie.
func TestWindow_Headers(t *testing.T) {
	api := &fakeWindowAPI{body: windowFixture(t, "phased")}
	h, _ := windowServerFor(t, api, nil)
	for _, p := range []string{"/w/" + testWindowTok, "/w/" + testWindowTok + "/plan.json", "/w/" + testWindowTok + "/land.json", "/w/static/window.js", "/w/static/land.js", "/w/nope", "/w/" + testWindowTok + "/x"} {
		w := windowGet(h, p)
		hd := w.Result().Header
		for k, want := range map[string]string{
			"Referrer-Policy":         "no-referrer",
			"X-Robots-Tag":            "noindex, nofollow",
			"Cache-Control":           "private, no-store",
			"Content-Security-Policy": windowContentSecurityPolicy,
			"X-Content-Type-Options":  "nosniff",
		} {
			if got := hd.Get(k); got != want {
				t.Errorf("GET %s: %s = %q, want %q", p, k, got, want)
			}
		}
		if c := hd.Values("Set-Cookie"); len(c) != 0 {
			t.Errorf("GET %s set a cookie: %v", p, c)
		}
	}
	csp := windowContentSecurityPolicy
	scriptSrc := csp[strings.Index(csp, "script-src"):]
	scriptSrc = scriptSrc[:strings.Index(scriptSrc, ";")]
	if scriptSrc != "script-src 'self'" {
		t.Errorf("CSP %q: want script-src 'self' and nothing inline", scriptSrc)
	}
}

// TestWindow_CacheETagAndViews: one daemon read serves a window for 20 s,
// whoever asks; a plan.json whose ETag matches is a 304 with no read; and
// the next read carries the human page views served in between, not the
// refreshes or the bots.
func TestWindow_CacheETagAndViews(t *testing.T) {
	api := &fakeWindowAPI{body: windowFixture(t, "phased")}
	h, clock := windowServerFor(t, api, nil)
	page, js := "/w/"+testWindowTok, "/w/"+testWindowTok+"/plan.json"

	if w := windowGet(h, page); w.Code != http.StatusOK {
		t.Fatalf("page = %d", w.Code)
	}
	windowGet(h, page) // a second visitor
	windowGet(h, page, "User-Agent", "Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)")
	windowGet(h, page+"?refresh=1") // window.js after a change
	pj := windowGet(h, js)
	etag := pj.Result().Header.Get("ETag")
	if pj.Code != http.StatusOK || etag == "" || pj.Result().Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("plan.json = %d, ETag %q", pj.Code, etag)
	}
	var v planwindow.View
	if err := json.Unmarshal(pj.Body.Bytes(), &v); err != nil || v.Title != "Orchard: a public seed swap" {
		t.Fatalf("plan.json = %v %+v", err, v)
	}
	if nm := windowGet(h, js, "If-None-Match", etag); nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
		t.Errorf("plan.json with its ETag = %d, want 304", nm.Code)
	}
	if api.n() != 1 || api.calls[0].views != 1 || api.calls[0].token != testWindowTok {
		t.Fatalf("daemon calls = %+v; want one read for the first view", api.calls)
	}

	clock.add(windowViewTTL + time.Second)
	if nm := windowGet(h, js, "If-None-Match", etag); nm.Code != http.StatusNotModified {
		t.Errorf("an unchanged plan after the TTL = %d, want 304", nm.Code)
	}
	if api.n() != 2 || api.calls[1].views != 1 {
		t.Errorf("second read = %+v; want the one human view served from cache, not the bot or the refresh", api.calls)
	}
	clock.add(windowViewTTL + time.Second)
	windowGet(h, js)
	if api.n() != 3 || api.calls[2].views != 0 {
		t.Errorf("a refresh-only read = %+v; want views 0", api.calls[2])
	}

	// Another window is another entry.
	windowGet(h, "/w/"+testWindowOther)
	if api.n() != 4 || api.calls[3].token != testWindowOther {
		t.Errorf("another window shared the first one's entry: %+v", api.calls)
	}
}

// TestWindow_RefusalIsCached: a refused token is not asked again for 20 s,
// so a burst of guesses at one token costs one daemon read.
func TestWindow_RefusalIsCached(t *testing.T) {
	api := &fakeWindowAPI{err: &beadsapi.APIError{StatusCode: 401, Message: "invalid token"}}
	h, clock := windowServerFor(t, api, nil)
	for range 5 {
		windowGet(h, "/w/"+testWindowTok)
	}
	if api.n() != 1 {
		t.Errorf("%d daemon reads for one refused token in 20 s", api.n())
	}
	clock.add(windowViewTTL)
	api.err = nil
	api.body = windowFixture(t, "flat")
	if w := windowGet(h, "/w/"+testWindowTok); w.Code != http.StatusOK || api.n() != 2 {
		t.Errorf("after the TTL: %d, %d reads", w.Code, api.n())
	}
}

// TestWindow_FailureKeepsLastView: a daemon failure (5xx, a timeout, a
// payload that does not decode) is not a refusal. A window that opened keeps
// serving its last view, stale, and a refusal still closes it (gd-rXMNcFrzDp).
func TestWindow_FailureKeepsLastView(t *testing.T) {
	api := &fakeWindowAPI{body: windowFixture(t, "phased")}
	h, clock := windowServerFor(t, api, nil)
	page, js := "/w/"+testWindowTok, "/w/"+testWindowTok+"/plan.json"
	first := windowGet(h, js)
	if first.Code != http.StatusOK {
		t.Fatalf("first read = %d", first.Code)
	}
	for name, fail := range map[string]func(){
		"500":       func() { api.err = &beadsapi.APIError{StatusCode: 500, Message: "failed to read the window's target"} },
		"503":       func() { api.err = &beadsapi.APIError{StatusCode: 503, Message: "unavailable"} },
		"timeout":   func() { api.err = context.DeadlineExceeded },
		"undecoded": func() { api.err, api.body = nil, []byte(`{"plan":`) },
	} {
		fail()
		clock.add(windowViewTTL + time.Second)
		n := api.n()
		if w := windowGet(h, js); w.Code != http.StatusOK || w.Body.String() != first.Body.String() {
			t.Errorf("%s: plan.json = %d, want the last view", name, w.Code)
		}
		if w := windowGet(h, page); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Orchard: a public seed swap") {
			t.Errorf("%s: page = %d, want the last view", name, w.Code)
		}
		if api.n() != n+1 {
			t.Errorf("%s: %d daemon reads after the TTL, want 1 (the failure is held for the TTL)", name, api.n()-n)
		}
	}
	api.err = &beadsapi.APIError{StatusCode: 401, Message: "invalid token"}
	clock.add(windowViewTTL + time.Second)
	if w := windowGet(h, page); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "No window here") {
		t.Errorf("a refusal after a good view = %d, want the 404", w.Code)
	}
}

// TestWindow_Page: the page says what §4 lists, is complete without JS, links
// nowhere, and carries no inline script and no token.
func TestWindow_Page(t *testing.T) {
	api := &fakeWindowAPI{body: windowFixture(t, "phased")}
	h, _ := windowServerFor(t, api, nil)
	w := windowGet(h, "/w/"+testWindowTok)
	body := w.Body.String()
	for _, want := range []string{
		"<h1>Orchard: a public seed swap</h1>",
		`<span class="w-chip w-state-building">Building</span>`,
		"Now: <strong>The swap table</strong>",
		"3 of 8 steps done · 37%",
		"Building: claiming a packet; a refresh bug blocks it",
		"Waiting on the operator",
		"2 waiting, 1 needs a deploy, 1 needs a merge.",
		"Should a claim expire after a week?", "Waiting for an answer", "Answered: Spring and summer", "Withdrawn",
		"Groundwork", "2 of 2 done", "Offer and claim a packet", "In progress", "Blocked", "Deferred", "Not started",
		"<code>orchard-claim-a-packet-k2v</code>", "(working now)", "(idle)",
		"Waiting on the operator for a merge", "Step done: List seeds by season", "Answered: Which seasons go on the first page?",
		"<h4>Where it stands</h4>", "<li>Next: the welcome note.</li>",
		"Shared from gardenio Lands · read-only · updated",
		"This link stops working on 27 November 2026.",
		`<meta property="og:title" content="Orchard: a public seed swap">`,
		`<meta property="og:description" content="Building · Now: The swap table · 3 of 8 steps done">`,
		`<script src="/w/static/window.js"></script>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if n := strings.Count(body, "Orchard: a public seed swap"); n != 3 {
		t.Errorf("the title is on the page %d times; want <title>, og:title and the h1, not the narrative's own # line again", n)
	}
	for _, never := range []string{testWindowTok, "<a ", "og:image", "og:url", "<form", "skinClock", "SKIN_CLOCK"} {
		if strings.Contains(body, never) {
			t.Errorf("page has %q", never)
		}
	}
	if n, src := strings.Count(body, "<script"), strings.Count(body, "<script src="); n != src {
		t.Errorf("%d scripts, %d with src: the CSP allows no inline script", n, src)
	}
}

func TestWindow_PageWithholds(t *testing.T) {
	p := planwindow.Payload{Plan: planwindow.PlanFacts{Title: "Ask ops@example.com", State: "building"},
		Narrative: planwindow.NarrativeFacts{Docs: []planwindow.DocFact{{Content: "Fine paragraph.\n\nMail ops@example.com about it."}}}}
	b, _ := json.Marshal(p)
	h, _ := windowServerFor(t, &fakeWindowAPI{body: b}, nil)
	body := windowGet(h, "/w/"+testWindowTok).Body.String()
	if strings.Contains(body, "ops@example.com") || !strings.Contains(body, "<h1>"+planwindow.WithheldText+"</h1>") || !strings.Contains(body, `<p class="w-withheld">`) || !strings.Contains(body, "Fine paragraph.") {
		t.Errorf("hygiene did not withhold field by field:\n%s", body)
	}
}

// TestWindow_NeverLogsPathOrToken runs every handler, the failures included,
// with a debug logger and finds neither the path nor the token in it.
func TestWindow_NeverLogsPathOrToken(t *testing.T) {
	var logs bytes.Buffer
	for _, api := range []*fakeWindowAPI{
		{body: windowFixture(t, "phased")},
		{body: windowLandFixture(t, "mixed")},
		{err: &beadsapi.APIError{StatusCode: 401, Message: "invalid token"}},
		{err: &beadsapi.APIError{StatusCode: 500, Message: "boom"}},
		{err: errors.New("dial tcp: refused")},
		{body: []byte("{")},
	} {
		h, _ := windowServerFor(t, api, &logs)
		for _, p := range []string{"/w/" + testWindowTok, "/w/" + testWindowTok + "/plan.json", "/w/" + testWindowTok + "/land.json", "/w/" + testWindowTok + "?refresh=1", "/w/" + testWindowTok + "/x", "/w/gw_bad", "/w/static/window.js", "/w/static/land.js"} {
			windowGet(h, p)
		}
	}
	if s := logs.String(); strings.Contains(s, testWindowTok) || strings.Contains(s, "gw_") || strings.Contains(s, "/w/") {
		t.Errorf("the logs name a window path or token:\n%s", s)
	}
}

// TestWindow_OutsideTheAuthChain: the /w/ routes answer through the public
// mux, and the outer "/" (the auth chain) never sees a /w/ path.
func TestWindow_OutsideTheAuthChain(t *testing.T) {
	api := &fakeWindowAPI{body: windowFixture(t, "flat")}
	h, _ := windowServerFor(t, api, nil)
	for p, want := range map[string]int{
		"/w/" + testWindowTok:                200,
		"/w/" + testWindowTok + "/plan.json": 200,
		"/w/static/window.js":                200,
		"/w/" + testWindowTok + "/":          404,
		"/w/":                                404,
		"/plans":                             401,
	} {
		if w := windowGet(h, p); w.Code != want {
			t.Errorf("GET %s = %d, want %d", p, w.Code, want)
		}
	}
}

// TestWindow_BearerOutsideW is the viewer half of the negative matrix (PW.6;
// kbeads' half is TestWindow_BearerOpensOneRoute): a live window token sent as
// a bearer, on every registered route outside /w/, through the chain the
// cluster runs (dev bypass with the edge secret, operator, machine, scope),
// opens nothing. A routePublic route answers it as it answers no credential;
// every other route is 403, the machine routes included, since the daemon
// refuses a window bearer on /v1/me.
func TestWindow_BearerOutsideW(t *testing.T) {
	me := &fakeMe{err: &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "a window opens only GET /v1/windows/view"}}
	h := edgeChain(t, edgeTestSecret, me.who)
	n := 0
	for _, pattern := range registeredViewerRoutes(t).patterns {
		method, path := concretePath(pattern, "claimlayer")
		if strings.HasPrefix(path, "/w/") {
			continue
		}
		n++
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+testWindowTok)
		code := serveReq(h, r)
		anon := serveReq(h, httptest.NewRequest(method, path, nil))
		switch viewerRouteClasses[pattern] {
		case routePublic:
			if code != anon {
				t.Errorf("%s as a window: %d, anonymous %d", pattern, code, anon)
			}
		default:
			if code != http.StatusForbidden {
				t.Errorf("%s as a window: %d, want 403", pattern, code)
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d routes exercised", n)
	}
	if me.asked == 0 {
		t.Error("no machine route asked the daemon who the window bearer is")
	}

	// Under /w/ only the path's token opens a window, land.json (LW.4)
	// included: a live window token sent as a bearer opens nothing when the
	// path's token is malformed (the daemon is never asked), and with another
	// path token the daemon is asked for that one alone.
	api := &fakeWindowAPI{body: windowLandFixture(t, "mixed")}
	wh, _ := windowServerFor(t, api, nil)
	for _, p := range []string{"/w/gw_bad/land.json", "/w/not-a-token/land.json", "/w/gw_bad/plan.json", "/w/gw_bad"} {
		if w := windowGet(wh, p, "Authorization", "Bearer "+testWindowTok); w.Code != http.StatusNotFound {
			t.Errorf("GET %s with a window bearer = %d, want 404", p, w.Code)
		}
	}
	if api.n() != 0 {
		t.Fatalf("a bearer reached the daemon: %+v", api.calls)
	}
	if w := windowGet(wh, "/w/"+testWindowOther+"/land.json", "Authorization", "Bearer "+testWindowTok); w.Code != http.StatusOK {
		t.Errorf("land.json = %d", w.Code)
	}
	if api.n() != 1 || api.calls[0].token != testWindowOther {
		t.Errorf("daemon calls = %+v; want the path's token alone", api.calls)
	}
}

func TestWindow_JS(t *testing.T) {
	h, _ := windowServerFor(t, &fakeWindowAPI{}, nil)
	w := windowGet(h, "/w/static/window.js")
	js := w.Body.String()
	if w.Result().Header.Get("Content-Type") != "text/javascript; charset=utf-8" || !strings.Contains(js, "var SKIN_CLOCK") ||
		!strings.Contains(js, "SKIN_CLOCK.start('homestead', 'dark-homestead');") || !strings.Contains(js, "'/plan.json'") || !strings.Contains(js, "If-None-Match") {
		t.Errorf("window.js = %s", js[:min(len(js), 200)])
	}
}

func TestWindowCrawler(t *testing.T) {
	for ua, want := range map[string]bool{
		"": true,
		"Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)":                                  true,
		"Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)":                           true,
		"facebookexternalhit/1.1 Facebot Twitterbot/1.0":                                              true,
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)":                    true,
		"Microsoft Office Word 2014":                                                                  true,
		"WhatsApp/2.23.20.0":                                                                          true,
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/130 Safari/537.36": false,
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Safari/604.1":    false,
	} {
		if got := windowCrawler(ua); got != want {
			t.Errorf("windowCrawler(%q) = %v, want %v", ua, got, want)
		}
	}
}

func TestWindowBlock(t *testing.T) {
	got := string(windowBlock("# Title\n\nSee [the doc](https://internal.example/x) and <https://a.example/b> ![pic](https://img.example/p.png) <script>alert(1)</script>"))
	for _, never := range []string{"<a ", "href", "<img", "<script", "<h1"} {
		if strings.Contains(got, never) {
			t.Errorf("block has %q: %s", never, got)
		}
	}
	for _, want := range []string{"<h3>Title</h3>", "the doc", "https://a.example/b", "pic"} {
		if !strings.Contains(got, want) {
			t.Errorf("block lacks %q: %s", want, got)
		}
	}
}

func TestWindowAgo(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		-time.Minute: "just now", 30 * time.Second: "just now", 5 * time.Minute: "5 min ago",
		3 * time.Hour: "3 h ago", 50 * time.Hour: "2 d ago", 40 * 24 * time.Hour: "18 Sep 2026",
	} {
		if got := windowAgo(now.Add(-d), now); got != want {
			t.Errorf("windowAgo(-%v) = %q, want %q", d, got, want)
		}
	}
}

// blockingWindowAPI is fakeWindowAPI answering only once release is closed.
type blockingWindowAPI struct {
	fakeWindowAPI
	release chan struct{}
	started atomic.Int64
}

func (b *blockingWindowAPI) WindowView(ctx context.Context, token string, views int64) (json.RawMessage, error) {
	b.started.Add(1)
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.fakeWindowAPI.WindowView(ctx, token, views)
}

// TestWindow_LateReadFillsCache: a read the visitor stopped waiting on still
// ends and fills the cache, so the next visitor sees the window instead of
// "No window here", and a visitor meanwhile joins it rather than starting
// another (gd-FC7tIACC5f: kbeads answered two of three reads after the
// viewer gave up).
func TestWindow_LateReadFillsCache(t *testing.T) {
	api := &blockingWindowAPI{fakeWindowAPI: fakeWindowAPI{body: windowFixture(t, "phased")}, release: make(chan struct{})}
	clock := &windowClock{t: time.Date(2026, 11, 3, 19, 9, 0, 0, time.UTC)}
	c := newWindowCache(api)
	c.now, c.readTimeout = clock.now, 20*time.Millisecond
	ctx := context.Background()

	if _, err := c.get(ctx, testWindowTok, true); !errors.Is(err, errWindowNotFound) {
		t.Fatalf("a window whose first read is slow = %v, want the 404", err)
	}
	clock.add(windowViewTTL + time.Second)
	if _, err := c.get(ctx, testWindowTok, true); !errors.Is(err, errWindowNotFound) {
		t.Fatalf("second visitor during the read = %v, want the 404", err)
	}
	if n := api.started.Load(); n != 1 {
		t.Fatalf("%d daemon reads started, want the second visitor to join the first", n)
	}

	e := c.entry(windowKey(testWindowTok), clock.now())
	e.mu.Lock()
	r := e.read
	e.mu.Unlock()
	if r == nil {
		t.Fatal("no read in flight after the visitors gave up on it")
	}
	close(api.release)
	<-r.done
	snap, err := c.get(ctx, testWindowTok, false)
	if err != nil || snap == nil || snap.view.Title != "Orchard: a public seed swap" {
		t.Fatalf("after the late answer = %v, %v; want the view it read", snap, err)
	}
	if n := api.started.Load(); n != 1 || api.calls[0].views != 1 {
		t.Errorf("daemon reads %d, calls %+v; want the one read, reporting the first view", n, api.calls)
	}
	e.mu.Lock()
	pending := e.pending
	e.mu.Unlock()
	if pending != 1 {
		t.Errorf("pending views = %d, want the second visitor's still to report", pending)
	}
}
