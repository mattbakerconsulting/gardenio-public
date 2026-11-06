package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The welcome tour (step 9.14b, welcome.go): the page carries the tour a land's person is due, and only while it is due, so
// its ETag moves when they have had it; POST /garden/welcome records it.
func TestWelcomeTourPage(t *testing.T) {
	f := newFake()
	f.set([]*beadsapi.BeadDetail{bead("gd-w1", "bug", 5)}, nil)
	h := New(f, "/v", nil)
	_ = h.refresh(context.Background())
	var seen atomic.Bool
	var fail atomic.Bool
	page := &Handler{Hub: h, BasePath: "/v", Viewer: func(*http.Request) string { return "oidc:ann@example.com" },
		Welcome: func(*http.Request) WelcomeTour {
			if seen.Load() {
				return WelcomeTour{}
			}
			return WelcomeTour{Land: "gatehouse", Role: "guest", Gate: true}
		},
		WelcomeSeen: func(*http.Request) error {
			if fail.Load() {
				return errors.New("daemon down")
			}
			seen.Store(true)
			return nil
		}}
	mux := http.NewServeMux()
	page.Register(mux)
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		return rec
	}
	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/garden/welcome", nil))
		return rec
	}

	due := get()
	if want := `data-welcome="gatehouse" data-welcome-role="guest" data-welcome-gate="1"`; !strings.Contains(due.Body.String(), want) {
		t.Fatalf("a due page lacks %s", want)
	}

	fail.Store(true)
	if rec := post(); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("a failed record: %d %s, want 503 and ok false", rec.Code, rec.Body)
	}
	fail.Store(false)
	if rec := post(); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Fatalf("POST /garden/welcome: %d %s", rec.Code, rec.Body)
	}

	had := get()
	if strings.Contains(had.Body.String(), "data-welcome") {
		t.Fatal("the page still runs the tour after it was had")
	}
	if due.Header().Get("ETag") == had.Header().Get("ETag") {
		t.Fatal("the ETag did not move when the tour was had: a revisit would be a 304 of the due page")
	}

	// No hooks: nobody is toured, and the record answers 503.
	plain := &Handler{Hub: h, BasePath: "/v"}
	mux = http.NewServeMux()
	plain.Register(mux)
	if rec := get(); strings.Contains(rec.Body.String(), "data-welcome") {
		t.Fatal("a page without the hook runs the tour")
	}
	if rec := post(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /garden/welcome without the hook: %d, want 503", rec.Code)
	}
}

// TOUR.welcome is the welcome tour's stops: the land, how to ask, and #general, each framing the land and offering nothing
// to do; the gatehouse is worded for a guest with no land of their own, and says how to get one.
func TestWelcomeTourUnderNode(t *testing.T) {
	script := `
var TOUR = require(process.argv[1]), assert = require("assert");
var g = TOUR.welcome({ land: "gatehouse", role: "guest", gate: true, at: "gatehouse" });
assert.deepStrictEqual(g.map(function (s) { return s.key; }), ["welcome:land", "welcome:ask", "welcome:general"]);
assert.deepStrictEqual(g.map(function (s) { return s.title; }), ["Welcome to the gatehouse", "How to ask", "Your steward said hello"]);
assert.ok(g.every(function (s) { return s.kind === "welcome" && s.at === "gatehouse" && s.cur === "gatehouse" && s.land === "gatehouse" && s.actions.length === 0 && s.bead === ""; }));
assert.ok(/starts here, as a guest of gatehouse/.test(g[0].whys[0][1]), g[0].whys[0][1]);
assert.deepStrictEqual(g[1].whys.map(function (w) { return w[0]; }), ["Ask in words", "A land of your own"]);
assert.ok(/gatehouse's steward greeted you by name in #general/.test(g[2].whys[0][1]), g[2].whys[0][1]);
var own = TOUR.welcome({ land: "zinnia", role: "owner", gate: false, at: null });
assert.strictEqual(own[0].title, "Welcome to zinnia");
assert.strictEqual(own[0].whys[0][1], "zinnia is your land.");
assert.deepStrictEqual(own[1].whys.map(function (w) { return w[0]; }), ["Ask in words"], "only the gatehouse says how to get a land");
assert.ok(own.every(function (s) { return s.at === null; }), "a land off the map frames nothing");
assert.strictEqual(TOUR.welcome({ land: "zinnia", role: "member" })[0].whys[0][1], "You are a member of zinnia.");
assert.ok(/^You are a guest of zinnia/.test(TOUR.welcome({ land: "zinnia", role: "guest" })[0].whys[0][1]));
console.log("welcome node checks passed");
`
	runNodeLogic(t, "tour.js", script)
}

// The welcome tour in a real Chromium (step 9.14b), on the card farm cut to one land as a scoped person sees it: the first
// /garden runs it once and it ends at the land's steward; a revisit does not run it; a phone runs it on the Farm, and Escape
// dismisses it. What is asserted lives in testdata/welcome_browser.js.
func TestWelcomeTourUnderChromium(t *testing.T) {
	underChromium(t, "welcome_browser.js", func() *httptest.Server {
		srv, rig := cardWorldRig(t, time.Now, "oidc:ann@example.com", []string{"gardenio"}, nil, false)
		var seen atomic.Bool
		rig.page.Welcome = func(*http.Request) WelcomeTour {
			if seen.Load() {
				return WelcomeTour{}
			}
			return WelcomeTour{Land: "gardenio", Role: "guest", Gate: true}
		}
		rig.page.WelcomeSeen = func(*http.Request) error { seen.Store(true); return nil }
		rig.mux.HandleFunc("POST /test/welcome/reset", func(w http.ResponseWriter, _ *http.Request) { seen.Store(false); w.WriteHeader(http.StatusNoContent) })
		return srv
	})
}
