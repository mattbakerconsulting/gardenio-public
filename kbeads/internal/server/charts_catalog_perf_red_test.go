package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// charts_catalog_perf_red_test.go — tests for the W2b perf RED chart-catalog
// entries (ka-0000003074): request-rate, error-ratio, and latency-quantile
// panels built on the live fastapi_sponsor_api_* perf-federated metrics.
//
// These tests pin the rubric-driven invariants established at authoring time
// (gardenio/docs/data-correctness-rubric.md): each panel hard-pins
// source="perf" (C2 honest-aggregation), the latency panel uses the highr
// (fine-grained) histogram so p95 is not pinned to a bucket edge (rubric §2.2),
// the error panel guards its denominator (C5 existence-honesty), and none of the
// panels reference the absent spanmetrics families (which would fail C5).

// TestPerfRED_PresentAndSourcePinned asserts all three perf RED charts exist and
// every query they emit is scoped to source="perf" so dev/prod series on the
// same family cannot leak in.
func TestPerfRED_PresentAndSourcePinned(t *testing.T) {
	cat := chartCatalog()
	ids := []string{
		"perf-fastapi-request-rate",
		"perf-fastapi-error-ratio",
		"perf-fastapi-latency-quantiles",
	}
	for _, id := range ids {
		def, ok := cat[id]
		if !ok {
			t.Fatalf("perf RED chart %q missing from catalog", id)
		}
		if def.Category != "perf" {
			t.Errorf("%s: category = %q, want perf", id, def.Category)
		}
		// Primary query + every overlay query must pin source="perf".
		queries := []string{def.QueryTemplate}
		for _, ex := range def.ExtraSeries {
			queries = append(queries, ex.QueryTemplate)
		}
		for _, q := range queries {
			if !strings.Contains(q, `source="perf"`) {
				t.Errorf("%s: query must pin source=\"perf\" (C2):\n  %s", id, q)
			}
			// The bead's original span-derived premise does not hold — no panel
			// may reference the absent spanmetrics families (would fail C5).
			if strings.Contains(q, "span_calls_total") || strings.Contains(q, "span_duration_milliseconds") {
				t.Errorf("%s: references absent spanmetrics family (C5 MISSING-SERIES):\n  %s", id, q)
			}
		}
	}
}

// TestPerfRED_LatencyUsesHighrHistogram guards that the latency panel reads the
// highr (fine-grained) duration histogram and carries all three quantiles. The
// coarse non-highr histogram has only le ∈ {0.1,0.5,1.0}, which would pin p95 to
// a bucket edge — rubric §2.2 (latency-quantile honesty).
func TestPerfRED_LatencyUsesHighrHistogram(t *testing.T) {
	def := chartCatalog()["perf-fastapi-latency-quantiles"]
	const highr = "fastapi_sponsor_api_http_request_duration_highr_seconds_bucket"

	if !strings.Contains(def.QueryTemplate, highr) {
		t.Errorf("latency p95 must read the highr histogram (rubric §2.2):\n  %s", def.QueryTemplate)
	}
	if !strings.Contains(def.QueryTemplate, "histogram_quantile(0.95") {
		t.Errorf("primary series must be p95:\n  %s", def.QueryTemplate)
	}
	// p50 + p99 overlays, both on the highr histogram and the primary axis.
	wantQuantiles := map[string]bool{"p50": false, "p99": false}
	for _, ex := range def.ExtraSeries {
		if _, ok := wantQuantiles[ex.Name]; ok {
			wantQuantiles[ex.Name] = true
		}
		if !strings.Contains(ex.QueryTemplate, highr) {
			t.Errorf("overlay %s must read the highr histogram:\n  %s", ex.Name, ex.QueryTemplate)
		}
		if ex.YAxisIndex != 0 {
			t.Errorf("overlay %s must share the primary seconds axis (YAxisIndex 0), got %d", ex.Name, ex.YAxisIndex)
		}
	}
	for q, present := range wantQuantiles {
		if !present {
			t.Errorf("latency panel missing the %s overlay", q)
		}
	}
	if def.YFormat != "duration_seconds" {
		t.Errorf("latency YFormat = %q, want duration_seconds", def.YFormat)
	}
}

// TestPerfRED_ErrorRatioGuardsDenominator guards that the error-ratio panel
// divides by a `> 0`-guarded request total so empty buckets are NaN-gapped
// (honest-empty, R2 / C5) rather than rendering 0/0 or a misleading flat 0.
func TestPerfRED_ErrorRatioGuardsDenominator(t *testing.T) {
	def := chartCatalog()["perf-fastapi-error-ratio"]
	q := def.QueryTemplate
	if !strings.Contains(q, `status=~"4xx|5xx"`) {
		t.Errorf("error numerator must select the 4xx|5xx status classes:\n  %s", q)
	}
	if !strings.Contains(q, "> 0)") {
		t.Errorf("error ratio denominator must carry a > 0 guard (C5 honest-empty):\n  %s", q)
	}
	if def.YFormat != "ratio" {
		t.Errorf("error-ratio YFormat = %q, want ratio", def.YFormat)
	}
}

// TestPerfRED_RequestRate_RendersByStatus drives the request-rate chart end to
// end against a stub VM and asserts the long-format rows are keyed by status.
func TestPerfRED_RequestRate_RendersByStatus(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	body := `{"status":"success","data":{"resultType":"matrix","result":[` +
		matrixSeries("status", "2xx", start, step, "0.5", "0.7") + `,` +
		matrixSeries("status", "5xx", start, step, "0.0", "0.1") +
		`]}}`
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "fastapi_sponsor_api_http_requests_total", body: body}))

	def := chartCatalog()["perf-fastapi-request-rate"]
	req := &chartRequest{
		Since: time.Unix(start, 0).UTC(),
		Until: time.Unix(start+2*step, 0).UTC(),
		Step:  time.Hour,
		Echo:  map[string]any{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}
	if len(render.Data) != 4 { // 2 statuses × 2 points
		t.Fatalf("want 4 rows (2 status × 2 pts), got %d: %+v", len(render.Data), render.Data)
	}
	// Every row carries a status series key and the requests_per_sec value field.
	for _, row := range render.Data {
		if _, ok := row["status"]; !ok {
			t.Errorf("row missing status series key: %+v", row)
		}
		if _, ok := row["requests_per_sec"]; !ok {
			t.Errorf("row missing requests_per_sec value: %+v", row)
		}
	}
	if render.Meta.SeriesCount != 2 {
		t.Errorf("series count = %d, want 2", render.Meta.SeriesCount)
	}
}

// TestPerfRED_HonestEmptyWhenNoPerfTraffic asserts the panels render an honest
// empty time series (R2) when the perf metrics return no data, rather than
// erroring or fabricating a zero line.
func TestPerfRED_HonestEmptyWhenNoPerfTraffic(t *testing.T) {
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t)) // no rules → always empty matrix

	for _, id := range []string{"perf-fastapi-request-rate", "perf-fastapi-error-ratio", "perf-fastapi-latency-quantiles"} {
		def := chartCatalog()[id]
		req := &chartRequest{
			Since: time.Unix(1700000000, 0).UTC(),
			Until: time.Unix(1700003600, 0).UTC(),
			Step:  time.Hour,
			Echo:  map[string]any{},
		}
		render, err := s.computeChartRender(context.Background(), def, req, time.Now().UTC())
		if err != nil {
			t.Errorf("%s: empty series should not error: %v", id, err)
			continue
		}
		if len(render.Data) != 0 || !render.Meta.Empty {
			t.Errorf("%s: want honest-empty render, got %d rows empty=%v", id, len(render.Data), render.Meta.Empty)
		}
	}
}
