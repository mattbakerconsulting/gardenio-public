package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The remaining ECharts surface (/costs) links the vendored ECharts library
// plus the shared renderer (echarts_catalog.js) and the fixed-window driver
// (echarts_bead.js). All must be embedded and served by the static file
// handler, or those charts stay on "loading…". The /charts dashboard driver
// (echarts_dashboard.js) was removed when /charts swapped to embedded Grafana
// (epic ka-0000006518, task gd-MJSWECAEkQ); the per-bead chart panel swapped to
// Grafana embeds too (task ka-0000007859), so echarts_bead.js now serves only
// /costs.
func TestEChartsAssets_ServedStatic(t *testing.T) {
	requireVendoredJS(t)
	mux := mkServer(t, newFake())
	cases := []struct {
		path    string
		minSize int
	}{
		{"/assets/static/echarts.min.js", 100000},   // vendored lib is ~1 MB
		{"/assets/static/echarts_catalog.js", 2000}, // shared renderer module
		{"/assets/static/echarts_bead.js", 1000},    // /costs fixed-window driver
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: got %d want 200", tc.path, w.Code)
			continue
		}
		if got := w.Body.Len(); got < tc.minSize {
			t.Errorf("%s suspiciously small: %d bytes (want >= %d)", tc.path, got, tc.minSize)
		}
	}
}
