package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenhub"
)

// everyRedirectOn turns on the redirect flag of every retirement (R1-R10, and G-R4 since step 6.8), as the 5.13 sweep
// leaves them.
var everyRedirectOn = map[string]string{
	gardenQueuesRedirectEnv:   "true",
	gardenBeadRedirectEnv:     "true",
	gardenSearchRedirectEnv:   "true",
	gardenBundleRedirectEnv:   "true",
	gardenPlansRedirectEnv:    "true",
	gardenActivityRedirectEnv: "true",
	gardenProjectRedirectEnv:  "true",
	gardenGardenerRedirectEnv: "true",
	gardenSettingsRedirectEnv: "true",
	gardenAgentsRedirectEnv:   "true",
}

func sweepServer(t *testing.T, env map[string]string) http.Handler {
	t.Helper()
	srv := NewServer(seedPlansFake(), slog.Default(), "/v", "", "", nil)
	if env != nil {
		srv.tunables = newWatcherWithEnv(t, env)
	}
	srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
	return edgeAuthMux(srv)
}

func sweepGet(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// WB-16: with every flag flipped, every retired GET page answers 302 to its panel, the id and the page's own query
// carried; ?old=1 keeps each page, and never rides into the garden.
func TestRedirectSweep(t *testing.T) {
	h := sweepServer(t, everyRedirectOn)
	for path, want := range map[string]string{
		"/decisions":                          "/v/garden?queue=decisions",
		"/decisions/gd-dec1":                  "/v/garden?bead=gd-dec1",
		"/escalations":                        "/v/garden?queue=escalations",
		"/inbox":                              "/v/garden?queue=inbox",
		"/inbox/gd-step1":                     "/v/garden?bead=gd-step1",
		"/gd-step1":                           "/v/garden?bead=gd-step1",
		"/gd-step1?tab=comments":              "/v/garden?tab=comments&bead=gd-step1",
		"/search?q=edge":                      "/v/garden?q=edge",
		"/bundle/gd-bun1":                     "/v/garden?bundle=gd-bun1",
		"/plans":                              "/v/garden",
		"/plans/gd-plan1":                     "/v/garden?plan=gd-plan1",
		"/active?project=gardenio":            "/v/garden?tab=activity&project=gardenio",
		"/feed":                               "/v/garden",
		"/p/gardenio":                         "/v/garden?project=gardenio",
		"/gardener/gd-cap":                    "/v/garden?gardener=gd-cap",
		"/gardeners":                          "/v/garden",
		"/create-gardener?project=gardenio":   "/v/garden?tab=gardeners&project=gardenio",
		"/quick-spawn?task=gd-step1":          "/v/garden?bead=gd-step1",
		"/settings":                           "/v/garden?settings=me",
		"/agents":                             "/v/garden?glass=wall",
		"/agents?project=gardenio":            "/v/garden?tab=agents&project=gardenio",
		"/gd-step1?old=0":                     "/v/garden?bead=gd-step1",
		"/plans/gd-plan1?old=&at=gardenio":    "/v/garden?at=gardenio&plan=gd-plan1",
		"/p/gardenio?view=overview&old=false": "/v/garden?project=gardenio",
	} {
		if w := sweepGet(h, path); w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	for _, path := range []string{"/plans?old=1", "/plans/gd-plan1?old=1", "/gd-step1?old=1", "/decisions?old=1", "/settings?old=1", "/p/gardenio?old=1"} {
		if w := sweepGet(h, path); w.Code == http.StatusFound && strings.Contains(w.Header().Get("Location"), "/garden") {
			t.Errorf("%s: 302 to %q, want the old page", path, w.Header().Get("Location"))
		}
	}
	// the banner on a kept page links the panel, not the page again
	w := sweepGet(sweepServer(t, nil), "/plans/gd-plan1?old=1")
	if body := w.Body.String(); !strings.Contains(body, `href="/v/garden?plan=gd-plan1"`) || strings.Contains(body, `href="/v/garden?old=1`) {
		t.Errorf("/plans/gd-plan1?old=1: the banner should link /v/garden?plan=gd-plan1 without old=1")
	}
}

// The old viewer lists every page the garden does not open, each a registered, classified route that none of the retirements sweeps into the
// garden. Each page is served from the test fake, which most of them cannot fully render: a page that panics on it has
// got past the retirement wrapper (which redirects before the page runs), so only a redirect into the garden fails.
func TestOldViewerPagesSurviveTheSweep(t *testing.T) {
	srv := NewServer(seedPlansFake(), slog.Default(), "/v", "", "", nil)
	srv.tunables = newWatcherWithEnv(t, everyRedirectOn)
	srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := edgeAuth(mux)

	w := sweepGet(h, "/old-viewer")
	if w.Code != http.StatusOK {
		t.Fatalf("/old-viewer: %d, want 200", w.Code)
	}
	body := w.Body.String()
	var pages []oldViewerPage
	for _, g := range oldViewerGroups {
		pages = append(pages, g.Pages...)
	}
	if len(pages) == 0 {
		t.Fatal("the old viewer lists nothing")
	}
	for _, p := range pages {
		href := `href="/v` + strings.ReplaceAll(p.Path, "&", "&amp;") + `"`
		if !strings.Contains(body, href) {
			t.Errorf("/old-viewer does not link %s (%s)", p.Name, href)
		}
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, p.Path, nil))
		if _, ok := viewerRouteClasses[pattern]; !ok || !strings.HasPrefix(pattern, "GET /") || pattern == "GET /{id}" {
			t.Errorf("%s: %s is served by %q, want its own classified GET route", p.Name, p.Path, pattern)
			continue
		}
		func() {
			defer func() { _ = recover() }()
			r := sweepGet(h, p.Path)
			if r.Code/100 == 3 && strings.Contains(r.Header().Get("Location"), "/garden") {
				t.Errorf("%s: %s answers %d to %q; a page the old viewer lists must stay", p.Name, p.Path, r.Code, r.Header().Get("Location"))
			}
		}()
	}
}
