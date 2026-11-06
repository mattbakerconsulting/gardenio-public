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

// Plan gd-gh5J0vVbJP step 4.3 (gd-0WYDQUDLFf): /costs and /charts stay, and
// cross-link the garden's fleet Costs panel (/costs in its header since phase
// 2, /charts for a viewer who reaches /garden). The owner kept beads-viewer
// (2026-10-27), so neither page carries a retirement banner, a redirect flag
// or an old-viewer entry.
func TestGardenCostsLink(t *testing.T) {
	newSrv := func(t *testing.T, wired bool) http.Handler {
		f := newFake()
		f.listResp = &beadsapi.ListBeadsResult{}
		srv := NewServer(f, slog.Default(), "/v", "", "", nil)
		if wired {
			srv.SetGardenHandler(&gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v"})
		}
		return edgeAuthMux(srv)
	}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	const panel = `href="/v/garden?costs=fleet"`

	wired := newSrv(t, true)
	for _, path := range []string{"/costs", "/charts"} {
		w := get(wired, path)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, panel) {
			t.Errorf("%s: %d, link to the Costs panel: %v", path, w.Code, strings.Contains(body, panel))
		}
		if strings.Contains(body, `id="garden-banner"`) || strings.Contains(body, "moving into the garden") {
			t.Errorf("%s: carries a retirement banner; the page stays", path)
		}
	}
	if w := get(wired, "/costs"); !strings.Contains(w.Body.String(), `class="costs-chart-card"`) {
		t.Error("/costs keeps its chart cards")
	}

	unwired := newSrv(t, false)
	if w := get(unwired, "/charts"); w.Code != http.StatusOK || strings.Contains(w.Body.String(), panel) {
		t.Errorf("/charts without /garden wired: %d, want 200 and no Costs panel link", w.Code)
	}
	if w := get(unwired, "/costs"); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `id="garden-banner"`) {
		t.Errorf("/costs without /garden wired: %d, want 200 and no banner", w.Code)
	}
}
