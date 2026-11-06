package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// charts_catalog_span_red_test.go — tests for the span-derived perf RED
// chart-catalog entries (ka-0000008091): per-service request-rate, error-ratio,
// and latency-quantile panels built on the spanmetrics families
// (span_calls_total / span_duration_milliseconds_*) the perf OTel collector
// emits, federated into gardenio-vmsingle under source="perf".
//
// These pin the rubric-driven invariants (gardenio/docs/data-correctness-rubric.md):
// every query hard-pins source="perf" (C2) AND span_kind="SPAN_KIND_SERVER" so
// RED measures each service's own request surface (C2 honest-aggregation, the
// same discipline W2c used on VT server spans); the latency panel is in
// MILLISECONDS (C3 — the family is span_duration_MILLISECONDS_*); the error
// panel guards its denominator (R2/C5 honest-empty).

// spanREDIDs are the three span-derived RED catalog entries.
var spanREDIDs = []string{
	"perf-span-request-rate",
	"perf-span-error-ratio",
	"perf-span-latency-quantiles",
}

// TestSpanRED_PresentAndScoped asserts all three span RED charts exist and every
// query they emit is scoped to BOTH source="perf" (C2: no dev/prod leak) and
// span_kind="SPAN_KIND_SERVER" (C2: client/internal spans would inflate RED).
func TestSpanRED_PresentAndScoped(t *testing.T) {
	cat := chartCatalog()
	for _, id := range spanREDIDs {
		def, ok := cat[id]
		if !ok {
			t.Fatalf("span RED chart %q missing from catalog", id)
		}
		if def.Category != "perf" {
			t.Errorf("%s: category = %q, want perf", id, def.Category)
		}
		queries := []string{def.QueryTemplate}
		for _, ex := range def.ExtraSeries {
			queries = append(queries, ex.QueryTemplate)
		}
		for _, q := range queries {
			if !strings.Contains(q, `source="perf"`) {
				t.Errorf("%s: query must pin source=\"perf\" (C2):\n  %s", id, q)
			}
			if !strings.Contains(q, `span_kind="SPAN_KIND_SERVER"`) {
				t.Errorf("%s: query must filter span_kind=\"SPAN_KIND_SERVER\" — RED is the server surface, client/internal spans would inflate it (C2):\n  %s", id, q)
			}
		}
	}
}

// TestSpanRED_LatencyIsMilliseconds guards that the latency panel reads the
// span_duration_milliseconds_bucket histogram and is labelled in milliseconds
// (C3 unit-scale — the family name carries the unit), carrying all three
// quantiles on one shared axis.
func TestSpanRED_LatencyIsMilliseconds(t *testing.T) {
	def := chartCatalog()["perf-span-latency-quantiles"]
	const bucket = "span_duration_milliseconds_bucket"

	if !strings.Contains(def.QueryTemplate, bucket) {
		t.Errorf("latency p95 must read %s:\n  %s", bucket, def.QueryTemplate)
	}
	if !strings.Contains(def.QueryTemplate, "histogram_quantile(0.95") {
		t.Errorf("primary series must be p95:\n  %s", def.QueryTemplate)
	}
	if def.YFormat != "duration_ms" {
		t.Errorf("latency YFormat = %q, want duration_ms (the family is span_duration_MILLISECONDS_*, C3)", def.YFormat)
	}
	// p50 + p99 overlays, both on the ms histogram and the primary axis.
	wantQuantiles := map[string]bool{"p50": false, "p99": false}
	for _, ex := range def.ExtraSeries {
		if _, ok := wantQuantiles[ex.Name]; ok {
			wantQuantiles[ex.Name] = true
		}
		if !strings.Contains(ex.QueryTemplate, bucket) {
			t.Errorf("overlay %s must read %s:\n  %s", ex.Name, bucket, ex.QueryTemplate)
		}
		if ex.YFormat != "duration_ms" {
			t.Errorf("overlay %s YFormat = %q, want duration_ms", ex.Name, ex.YFormat)
		}
		if ex.YAxisIndex != 0 {
			t.Errorf("overlay %s must share the primary ms axis (YAxisIndex 0), got %d", ex.Name, ex.YAxisIndex)
		}
	}
	for q, present := range wantQuantiles {
		if !present {
			t.Errorf("latency panel missing the %s overlay", q)
		}
	}
}

// TestSpanRED_ErrorRatioGuardsDenominator guards that the error-ratio panel
// counts STATUS_CODE_ERROR (OTel span status, NOT HTTP Nxx) over all server
// spans with a `> 0` denominator guard so empty windows NaN-gap (R2/C5).
func TestSpanRED_ErrorRatioGuardsDenominator(t *testing.T) {
	def := chartCatalog()["perf-span-error-ratio"]
	q := def.QueryTemplate
	if !strings.Contains(q, `status_code="STATUS_CODE_ERROR"`) {
		t.Errorf("error numerator must select OTel STATUS_CODE_ERROR:\n  %s", q)
	}
	if !strings.Contains(q, "> 0)") {
		t.Errorf("error ratio denominator must carry a > 0 guard (C5 honest-empty):\n  %s", q)
	}
	if def.YFormat != "ratio" {
		t.Errorf("error-ratio YFormat = %q, want ratio", def.YFormat)
	}
}

// TestSpanRED_RequestRate_RendersByService drives the request-rate chart end to
// end against a stub VM and asserts the long-format rows are keyed by service.
func TestSpanRED_RequestRate_RendersByService(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	body := `{"status":"success","data":{"resultType":"matrix","result":[` +
		matrixSeries("service_name", "site-api", start, step, "0.5", "0.7") + `,` +
		matrixSeries("service_name", "sponsor-api", start, step, "1.0", "1.1") +
		`]}}`
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "span_calls_total", body: body}))

	def := chartCatalog()["perf-span-request-rate"]
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
	if len(render.Data) != 4 { // 2 services × 2 points
		t.Fatalf("want 4 rows (2 service × 2 pts), got %d: %+v", len(render.Data), render.Data)
	}
	for _, row := range render.Data {
		if _, ok := row["service_name"]; !ok {
			t.Errorf("row missing service_name series key: %+v", row)
		}
		if _, ok := row["spans_per_sec"]; !ok {
			t.Errorf("row missing spans_per_sec value: %+v", row)
		}
	}
	if render.Meta.SeriesCount != 2 {
		t.Errorf("series count = %d, want 2", render.Meta.SeriesCount)
	}
}

// TestSpanRED_HonestEmptyWhenNoTraffic asserts the panels render an honest empty
// time series (R2) when the spanmetrics return no data, rather than erroring or
// fabricating a zero line.
func TestSpanRED_HonestEmptyWhenNoTraffic(t *testing.T) {
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t)) // no rules → always empty matrix

	for _, id := range spanREDIDs {
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
