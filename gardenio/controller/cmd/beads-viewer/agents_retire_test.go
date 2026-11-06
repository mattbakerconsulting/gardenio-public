package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// G-R4 (Lands step 6.8): the /agents fleet list says "Open it in the garden", linking the Glass's Wall (or, with
// ?project=, that project panel's Agents tab), and answers 302 there once its own redirect flag is on.
func TestGardenAgentsRetirement(t *testing.T) {
	newSrv := func(t *testing.T, env map[string]string) http.Handler {
		f := newFake()
		f.listResp = &beadsapi.ListBeadsResult{}
		srv := NewServer(f, slog.Default(), "/v", "", "", nil)
		if env != nil {
			srv.tunables = newWatcherWithEnv(t, env)
		}
		srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
		return edgeAuthMux(srv)
	}
	get := func(h http.Handler, path string, hx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		if hx {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	banner := func(w *httptest.ResponseRecorder, href string) bool {
		return strings.Contains(w.Body.String(), `id="garden-banner"`) && strings.Contains(w.Body.String(), `href="`+href+`"`)
	}

	on := newSrv(t, nil)
	for path, href := range map[string]string{
		"/agents":                            "/v/garden?glass=wall",
		"/agents?project=gardenio&role=crew": "/v/garden?tab=agents&amp;project=gardenio",
	} {
		if w := get(on, path, false); w.Code != http.StatusOK || !banner(w, href) {
			t.Errorf("%s: %d, banner to %s: %v", path, w.Code, href, banner(w, href))
		}
	}
	if w := get(newSrv(t, map[string]string{gardenAgentsBannerEnv: "false"}), "/agents", false); strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("the agents banner's flag turns it off")
	}
	if w := get(newSrv(t, map[string]string{gardenGlassBannerEnv: "false"}), "/agents", false); !strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Error("the Glass (G-R1) banner's flag does not turn off the agents one")
	}

	redirect := newSrv(t, map[string]string{gardenAgentsRedirectEnv: "true"})
	for path, want := range map[string]string{
		"/agents":                "/v/garden?glass=wall",
		"/agents?group=gardener": "/v/garden?glass=wall",
		"/agents?project=gardenio&role=crew&state=": "/v/garden?tab=agents&project=gardenio",
	} {
		if w := get(redirect, path, false); w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	if w := get(redirect, "/agents?old=1", false); w.Code != http.StatusOK {
		t.Errorf("?old=1 keeps the page: %d", w.Code)
	}
	if w := get(redirect, "/agents", true); w.Code != http.StatusOK {
		t.Errorf("an htmx request keeps the page: %d", w.Code)
	}
	if w := get(redirect, "/agents/events-status", false); w.Code == http.StatusFound {
		t.Error("a fragment of the page is not retired, got a 302")
	}
	if w := get(newSrv(t, map[string]string{gardenGlassRedirectEnv: "true"}), "/agents", false); w.Code != http.StatusOK {
		t.Errorf("the Glass (G-R1) redirect flag does not move /agents: %d", w.Code)
	}
}
