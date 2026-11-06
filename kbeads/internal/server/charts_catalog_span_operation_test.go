package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// charts_catalog_span_operation_test.go — tests for the operation-level (span_name
// topk) perf RED chart-catalog entries (W2b parity, ka-0000001551). These port the
// ACME distributed-traces dashboard's highest-value panels (W2a inventory
// ka-0000003866) — "Top Operations by Volume", "Operations with Errors", "Slowest
// Operations" — as catalog charts keyed by the OTel span_name label. Parity target:
// gardenio/helm/gardenio/dashboards/monorepo-distributed-traces.json panels 9/10/11.
//
// They pin the same rubric-driven invariants as the per-service span RED entries
// (gardenio/docs/data-correctness-rubric.md): every query hard-pins source="perf"
// (C2) AND span_kind="SPAN_KIND_SERVER" so RED measures each service's own request
// surface; the slowest-operations panel is in MILLISECONDS (C3) reading p99; each
// keys on span_name and accepts a top_n selector.

// spanOperationIDs are the three operation-level span RED catalog entries.
var spanOperationIDs = []string{
	"perf-span-top-operations",
	"perf-span-operation-errors",
	"perf-span-slowest-operations",
}

// TestSpanOperation_PresentAndScoped asserts all three operation-level charts exist,
// live in the perf category, are keyed by span_name, accept a top_n selector, and
// pin BOTH source="perf" and span_kind="SPAN_KIND_SERVER" on every query (C2).
func TestSpanOperation_PresentAndScoped(t *testing.T) {
	cat := chartCatalog()
	for _, id := range spanOperationIDs {
		def, ok := cat[id]
		if !ok {
			t.Fatalf("operation-level span chart %q missing from catalog", id)
		}
		if def.Category != "perf" {
			t.Errorf("%s: category = %q, want perf", id, def.Category)
		}
		if def.SeriesField != "span_name" || def.SeriesLabel != "span_name" {
			t.Errorf("%s: must key on span_name (the operation), got field=%q label=%q", id, def.SeriesField, def.SeriesLabel)
		}
		// A top_n selector so an operator can widen/narrow the ranking.
		spec, ok := def.Params["top_n"]
		if !ok || spec.Type != "int" {
			t.Errorf("%s: must expose an int top_n param for the ranking cap", id)
		}
		if got, _ := def.DefaultParams["top_n"].(int); got != 10 {
			t.Errorf("%s: default top_n = %v, want 10 (the ACME panel-11 default)", id, def.DefaultParams["top_n"])
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
				t.Errorf("%s: query must filter span_kind=\"SPAN_KIND_SERVER\" (C2):\n  %s", id, q)
			}
			if !strings.Contains(q, "span_name") {
				t.Errorf("%s: query must group by span_name (the operation dimension):\n  %s", id, q)
			}
		}
	}
}

// TestSpanOperation_ErrorsRankByVolume guards that the "operations with errors"
// panel plots errored-spans/sec (a RATE, ranking by error VOLUME) over the
// STATUS_CODE_ERROR server spans — NOT a per-operation ratio — so the loudest
// failing endpoint sorts first (the ACME parity reading).
func TestSpanOperation_ErrorsRankByVolume(t *testing.T) {
	def := chartCatalog()["perf-span-operation-errors"]
	q := def.QueryTemplate
	if !strings.Contains(q, `status_code="STATUS_CODE_ERROR"`) {
		t.Errorf("error panel must select OTel STATUS_CODE_ERROR:\n  %s", q)
	}
	if !strings.Contains(q, "rate(") {
		t.Errorf("error panel must be a rate (errored spans/sec), not a raw count:\n  %s", q)
	}
	if def.YFormat != "rate" {
		t.Errorf("error panel YFormat = %q, want rate (it ranks by error volume, not a 0..1 ratio)", def.YFormat)
	}
	// A ratio would carry a `> 0` denominator guard; a volume rate must not
	// divide at all (a divided quantity is the per-service ratio chart's job).
	if strings.Contains(q, "/ (") || strings.Contains(q, "> 0)") {
		t.Errorf("error VOLUME panel must not divide into a ratio:\n  %s", q)
	}
}

// TestSpanOperation_SlowestIsMillisecondsP99 guards the slowest-operations panel
// reads the span_duration_milliseconds_bucket histogram at p99 (the ACME panel-11
// quantile), keyed by (span_name, le), and is labelled in milliseconds (C3).
func TestSpanOperation_SlowestIsMillisecondsP99(t *testing.T) {
	def := chartCatalog()["perf-span-slowest-operations"]
	q := def.QueryTemplate
	if !strings.Contains(q, "span_duration_milliseconds_bucket") {
		t.Errorf("slowest-operations must read span_duration_milliseconds_bucket:\n  %s", q)
	}
	if !strings.Contains(q, "histogram_quantile(0.99") {
		t.Errorf("slowest-operations must be p99 (the ACME panel-11 quantile):\n  %s", q)
	}
	if !strings.Contains(q, "by(span_name, le)") && !strings.Contains(q, "by (span_name, le)") {
		t.Errorf("slowest-operations must group by(span_name, le) so each operation gets its own quantile:\n  %s", q)
	}
	if def.YFormat != "duration_ms" {
		t.Errorf("slowest-operations YFormat = %q, want duration_ms (family is span_duration_MILLISECONDS_*, C3)", def.YFormat)
	}
}

// TestSpanOperation_TopOperationsRendersBySpanName drives the top-operations chart
// end to end against a stub VM and asserts the long-format rows are keyed by
// span_name (the operation), one series per operation.
func TestSpanOperation_TopOperationsRendersBySpanName(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	body := `{"status":"success","data":{"resultType":"matrix","result":[` +
		matrixSeries("span_name", "GET /studies", start, step, "2.0", "2.4") + `,` +
		matrixSeries("span_name", "POST /login", start, step, "0.5", "0.6") +
		`]}}`
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "span_calls_total", body: body}))

	def := chartCatalog()["perf-span-top-operations"]
	req := &chartRequest{
		Since: time.Unix(start, 0).UTC(),
		Until: time.Unix(start+2*step, 0).UTC(),
		Step:  time.Hour,
		Echo:  map[string]any{"top_n": 10},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}
	if len(render.Data) != 4 { // 2 operations × 2 points
		t.Fatalf("want 4 rows (2 operations × 2 pts), got %d: %+v", len(render.Data), render.Data)
	}
	for _, row := range render.Data {
		if _, ok := row["span_name"]; !ok {
			t.Errorf("row missing span_name series key: %+v", row)
		}
		if _, ok := row["spans_per_sec"]; !ok {
			t.Errorf("row missing spans_per_sec value: %+v", row)
		}
	}
	if render.Meta.SeriesCount != 2 {
		t.Errorf("series count = %d, want 2", render.Meta.SeriesCount)
	}
}

// TestSpanOperation_TopNTrimsRanking asserts top_n caps the number of operation
// series to the N with the highest window total — the ranking membership an
// operator expects from a "top operations" panel (topNSeries, no rollup tail).
func TestSpanOperation_TopNTrimsRanking(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	// Three operations, descending volume; top_n=2 must keep the two busiest.
	body := `{"status":"success","data":{"resultType":"matrix","result":[` +
		matrixSeries("span_name", "busy", start, step, "5.0", "5.0") + `,` +
		matrixSeries("span_name", "medium", start, step, "3.0", "3.0") + `,` +
		matrixSeries("span_name", "quiet", start, step, "0.1", "0.1") +
		`]}}`
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "span_calls_total", body: body}))

	def := chartCatalog()["perf-span-top-operations"]
	req := &chartRequest{
		Since: time.Unix(start, 0).UTC(),
		Until: time.Unix(start+2*step, 0).UTC(),
		Step:  time.Hour,
		Echo:  map[string]any{"top_n": 2},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}
	if render.Meta.SeriesCount != 2 {
		t.Fatalf("top_n=2 must keep 2 operation series, got %d", render.Meta.SeriesCount)
	}
	// The quiet operation must be dropped (no "other" rollup on a ranking panel).
	for _, row := range render.Data {
		if name, _ := row["span_name"].(string); name == "quiet" || name == otherSeriesLabel {
			t.Errorf("top_n=2 ranking should drop %q (no rollup tail), but it is present: %+v", name, row)
		}
	}
}

// TestSpanOperation_HonestEmptyWhenNoTraffic asserts the operation panels render an
// honest empty series (R2) when the spanmetrics return no data, rather than
// erroring or fabricating a zero line.
func TestSpanOperation_HonestEmptyWhenNoTraffic(t *testing.T) {
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t)) // no rules → always empty matrix

	for _, id := range spanOperationIDs {
		def := chartCatalog()[id]
		req := &chartRequest{
			Since: time.Unix(1700000000, 0).UTC(),
			Until: time.Unix(1700003600, 0).UTC(),
			Step:  time.Hour,
			Echo:  map[string]any{"top_n": 10},
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
