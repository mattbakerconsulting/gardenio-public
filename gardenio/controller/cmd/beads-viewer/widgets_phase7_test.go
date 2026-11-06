package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// TestRegisteredWidgets_Phase7SlugsPresent verifies both Phase 7 widgets
// landed in the registry. Bead: ka-0000005419.
func TestRegisteredWidgets_Phase7SlugsPresent(t *testing.T) {
	got := registeredWidgets()
	for _, slug := range []string{"vm-sparklines", "event-timeline"} {
		if !slices.Contains(got, slug) {
			t.Errorf("Phase 7 slug %q missing from registry: %v", slug, got)
		}
	}
}

func TestSanitizeSourceKind(t *testing.T) {
	cases := map[string]string{
		"mezmo":            "mezmo",
		"VictoriaMetrics":  "code",  // case-insensitive only for known forms
		"victoria-metrics": "victoria-metrics",
		"":                 "code",
		"unknown":          "code",
		"  jira  ":         "jira",
		"GITLAB":           "gitlab",
	}
	for in, want := range cases {
		if got := sanitizeSourceKind(in); got != want {
			t.Errorf("sanitizeSourceKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTimePoints_PointsArray(t *testing.T) {
	raw := map[string]any{
		"points": []any{
			map[string]any{"when": "2026-05-06T12:00:00Z", "value": 100.0},
			map[string]any{"when": "2026-05-06T12:01:00Z", "value": 110.0},
			map[string]any{"when": "garbage", "value": 999.0},        // dropped
			map[string]any{"when": "2026-05-06T12:02:00Z", "value": "x"}, // dropped
			map[string]any{"when": "2026-05-06T12:03:00Z", "value": 120.0},
		},
	}
	xs, ys := parseTimePoints(raw)
	if len(xs) != 3 || len(ys) != 3 {
		t.Fatalf("want 3 valid points, got xs=%d ys=%d", len(xs), len(ys))
	}
	if ys[0] != 100.0 || ys[1] != 110.0 || ys[2] != 120.0 {
		t.Errorf("ys: %+v", ys)
	}
	if xs[1]-xs[0] != 60 {
		t.Errorf("expected 60s spacing, got %d", xs[1]-xs[0])
	}
}

func TestParseTimePoints_ParallelArrays(t *testing.T) {
	raw := map[string]any{
		"x": []any{float64(1700000000), float64(1700000060), float64(1700000120)},
		"y": []any{1.0, 2.0, 3.0},
	}
	xs, ys := parseTimePoints(raw)
	if len(xs) != 3 || len(ys) != 3 {
		t.Fatalf("want 3 points, got %d/%d", len(xs), len(ys))
	}
}

func TestRenderVMSparklines_Full(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	r, ok := lookupWidget("vm-sparklines")
	if !ok {
		t.Fatal("vm-sparklines not registered")
	}
	payload := `{
		"title":"VM sparklines",
		"group_id":"ii-ts-test",
		"cross_filter_badge":"informational",
		"series":[
			{"service":"study-web","metric":"p95","unit":"ms","state":"warn","points":[
				{"when":"2026-05-06T12:00:00Z","value":220.0},
				{"when":"2026-05-06T12:01:00Z","value":260.0},
				{"when":"2026-05-06T12:02:00Z","value":315.0}
			]},
			{"service":"sponsor-api","metric":"p95","unit":"ms","state":"healthy","points":[
				{"when":"2026-05-06T12:00:00Z","value":80.0},
				{"when":"2026-05-06T12:01:00Z","value":85.0}
			]}
		]
	}`
	html, err := r(payload, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"VM sparklines",
		"study-web",
		"sponsor-api",
		`data-state="warn"`,
		`data-state="healthy"`,
		`data-uplot-config=`,
		`data-uplot-kind="sparkline"`,
		`data-ii-ts-group="ii-ts-test"`,
		`ii-w-crossfilter-badge`,
		"315.00", // latest of first series
	} {
		if !strings.Contains(s, want) {
			t.Errorf("vm-sparklines missing %q in:\n%s", want, s)
		}
	}
}

func TestRenderVMSparklines_Empty(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	r, _ := lookupWidget("vm-sparklines")
	html, err := r(`{"title":"empty"}`, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	if !strings.Contains(s, "No metric series available") {
		t.Errorf("expected empty fallback in:\n%s", s)
	}
}

func TestRenderEventTimeline_Full(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	r, ok := lookupWidget("event-timeline")
	if !ok {
		t.Fatal("event-timeline not registered")
	}
	payload := `{
		"title":"Event timeline",
		"group_id":"ii-ts-test",
		"events":[
			{"when":"2026-05-06T11:50:00Z","kind":"gitlab","title":"deploy !2185","url":"https://gitlab.com"},
			{"when":"2026-05-06T11:52:00Z","kind":"mezmo","title":"401 spike","detail":"1204 events"},
			{"when":"2026-05-06T11:55:00Z","kind":"jira","title":"FSM-12345 created"},
			{"when":"2026-05-06T11:58:00Z","kind":"BAD-KIND","title":"unknown source falls back"}
		]
	}`
	html, err := r(payload, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"Event timeline",
		`data-source-kind="gitlab"`,
		`data-source-kind="mezmo"`,
		`data-source-kind="jira"`,
		`data-source-kind="code"`, // bad kind falls back to "code"
		`data-uplot-kind="timeline"`,
		`data-ii-ts-group="ii-ts-test"`,
		"deploy !2185",
		"FSM-12345 created",
		`class="ii-relative-time"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("event-timeline missing %q in:\n%s", want, s)
		}
	}
}

func TestRenderEventTimeline_Empty(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	r, _ := lookupWidget("event-timeline")
	html, err := r(`{"title":"none"}`, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	if !strings.Contains(s, "No timeline events") {
		t.Errorf("expected empty-state in:\n%s", s)
	}
}

func TestRenderEventTimeline_LanesDeterministic(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	r, _ := lookupWidget("event-timeline")
	// Lanes are derived from first-seen kind order — sorted events should
	// produce deterministic lane indices.
	payload := `{
		"events":[
			{"when":"2026-05-06T12:00:00Z","kind":"mezmo","title":"a"},
			{"when":"2026-05-06T12:01:00Z","kind":"jira","title":"b"},
			{"when":"2026-05-06T12:02:00Z","kind":"mezmo","title":"c"}
		]
	}`
	html, err := r(payload, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	// Lane index 0 should be mezmo (first by sorted order), lane 1 should be
	// jira. Confirm by looking for the lane header.
	if !strings.Contains(s, `data-source-kind="mezmo" data-lane="0"`) {
		t.Errorf("expected mezmo at lane 0:\n%s", s)
	}
	if !strings.Contains(s, `data-source-kind="jira" data-lane="1"`) {
		t.Errorf("expected jira at lane 1:\n%s", s)
	}
}

func TestMarshalSparkConfig_RoundTrip(t *testing.T) {
	s := sparkSeries{
		Service: "study-web", Metric: "p95", Unit: "ms",
		X: []int64{1, 2, 3}, Y: []float64{10, 20, 30}, Min: 10, Max: 30,
	}
	out := MarshalSparkConfig(s, "g1")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("not valid JSON: %v in %s", err, out)
	}
	if cfg["group_id"] != "g1" || cfg["service"] != "study-web" || cfg["metric"] != "p95" {
		t.Errorf("config shape: %+v", cfg)
	}
}

func TestMarshalTimelineConfig_RoundTrip(t *testing.T) {
	v := eventTimelineView{
		GroupID: "g1", MinUnix: 1, MaxUnix: 100,
		Lanes:  []timelineLane{{Index: 0, Kind: "mezmo", Label: "Logs", Count: 1}},
		Events: []timelineEvent{{Unix: 50, Kind: "mezmo", Title: "e1", Lane: 0}},
	}
	out := MarshalTimelineConfig(v)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("not valid JSON: %v in %s", err, out)
	}
	if cfg["group_id"] != "g1" {
		t.Errorf("group_id: %v", cfg["group_id"])
	}
}

// TestStaticAsset_ServesUplot confirms uPlot's CSS+JS bundles ship in the
// embed and reach the wire over /assets/static/.
func TestStaticAsset_ServesUplot(t *testing.T) {
	mux := mkServer(t, newFake())
	for _, path := range []string{"/assets/static/uplot.min.js", "/assets/static/uplot.min.css"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for %s, got %d", path, w.Code)
		}
		if w.Body.Len() < 100 {
			t.Errorf("%s suspiciously small: %d bytes", path, w.Body.Len())
		}
	}
}

func TestStaticAsset_ServesIncidentIntelUplotJS(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/incident_intel_uplot.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"IncidentIntel.timeseries",
		"subscribe",
		"broadcast",
		"initSparkline",
		"initTimeline",
		"data-uplot-config",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incident_intel_uplot.js missing %q", want)
		}
	}
}

func TestLayout_LinksUplotAssets(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		"/assets/static/uplot.min.js",
		"/assets/static/uplot.min.css",
		"/assets/static/incident_intel_uplot.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("layout missing %s", want)
		}
	}
}

func TestIncidentIntelGallery_Phase7SectionRenders(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/dev/incident-intel/gallery", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Phase 7 — time-series widgets (uPlot)",
		`data-widget-type="vm-sparklines"`,
		`data-widget-type="event-timeline"`,
		`data-uplot-kind="sparkline"`,
		`data-uplot-kind="timeline"`,
		`data-ii-ts-group="ii-ts-incident-001"`, // shared group → cross-chart hover sync
		"ii-w-crossfilter-badge",                // D4 informational
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Phase 7 gallery body missing %q", want)
		}
	}
}
