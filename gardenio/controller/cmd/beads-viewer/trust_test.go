package main

// Tests for the R5 Trust dashboard (ka-0000007085): the vmsingle series parse,
// the pure view-model builder (north-star hero, leading/lagging split, every
// primary metric paired with its balancing metric, honest degrade + awaiting-
// data flags), the instant-query round-trip, and the full /trust page render.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fullTrustVector is a vmsingle instant-query body publishing every R5 gauge
// (including quantile vecs and a positive survey-response count) so the builder
// has a fully-populated family to reduce.
const fullTrustVector = `{"status":"success","data":{"resultType":"vector","result":[
  {"metric":{"__name__":"gardenio_trust_trusted_throughput_per_week"},"value":[1700000000,"12.5"]},
  {"metric":{"__name__":"gardenio_trust_escaped_defect_rate"},"value":[1700000000,"0.05"]},
  {"metric":{"__name__":"gardenio_trust_escaped_defect_count"},"value":[1700000000,"2"]},
  {"metric":{"__name__":"gardenio_trust_review_effort_rounds","quantile":"p50"},"value":[1700000000,"1"]},
  {"metric":{"__name__":"gardenio_trust_review_effort_rounds","quantile":"p90"},"value":[1700000000,"3"]},
  {"metric":{"__name__":"gardenio_trust_trust_index_median"},"value":[1700000000,"4.2"]},
  {"metric":{"__name__":"gardenio_trust_trust_index_responses"},"value":[1700000000,"7"]},
  {"metric":{"__name__":"gardenio_trust_approval_without_comment_rate"},"value":[1700000000,"0.1"]},
  {"metric":{"__name__":"gardenio_trust_rework_cycles","quantile":"p50"},"value":[1700000000,"1"]},
  {"metric":{"__name__":"gardenio_trust_rework_cycles","quantile":"p90"},"value":[1700000000,"2"]},
  {"metric":{"__name__":"gardenio_trust_time_to_merge_seconds","quantile":"p50"},"value":[1700000000,"3600"]},
  {"metric":{"__name__":"gardenio_trust_time_to_merge_seconds","quantile":"p90"},"value":[1700000000,"86400"]},
  {"metric":{"__name__":"gardenio_trust_diff_size_lines","quantile":"p50"},"value":[1700000000,"42"]},
  {"metric":{"__name__":"gardenio_trust_diff_size_lines","quantile":"p90"},"value":[1700000000,"180"]},
  {"metric":{"__name__":"gardenio_trust_verbosity_ratio","quantile":"p50"},"value":[1700000000,"0.42"]},
  {"metric":{"__name__":"gardenio_trust_verbosity_ratio","quantile":"p90"},"value":[1700000000,"1.1"]},
  {"metric":{"__name__":"gardenio_trust_provenance_completeness"},"value":[1700000000,"1"]},
  {"metric":{"__name__":"gardenio_trust_reviewer_load_per_week","reviewer":"alice"},"value":[1700000000,"5"]},
  {"metric":{"__name__":"gardenio_trust_reviewer_load_max_per_week"},"value":[1700000000,"5"]},
  {"metric":{"__name__":"gardenio_trust_reviewer_load_mean_per_week"},"value":[1700000000,"3"]},
  {"metric":{"__name__":"gardenio_trust_peel_latency_seconds","quantile":"p50"},"value":[1700000000,"120"]},
  {"metric":{"__name__":"gardenio_trust_peel_latency_seconds","quantile":"p90"},"value":[1700000000,"600"]},
  {"metric":{"__name__":"gardenio_trust_delivered_mr_count"},"value":[1700000000,"40"]},
  {"metric":{"__name__":"gardenio_trust_total_mr_count"},"value":[1700000000,"55"]},
  {"metric":{"__name__":"gardenio_trust_window_days"},"value":[1700000000,"28"]},
  {"metric":{"__name__":"gardenio_trust_last_collect_timestamp_seconds"},"value":[1700000000,"1699999700"]}
]}}`

func TestParseTrustSeries(t *testing.T) {
	ts, ok := parseTrustSeries([]byte(fullTrustVector))
	if !ok {
		t.Fatal("expected ok=true for a success vector")
	}
	if !ts.has(gaugeTrustedThroughput) {
		t.Error("trusted-throughput gauge should be present")
	}
	if v, ok := ts.scalarVal(gaugeTrustedThroughput); !ok || v != 12.5 {
		t.Errorf("trusted-throughput = %v,%v want 12.5,true", v, ok)
	}
	// Quantile vecs split out by {quantile}.
	if v, ok := ts.quantileVal(gaugeDiffSize, "p50"); !ok || v != 42 {
		t.Errorf("diff-size p50 = %v,%v want 42,true", v, ok)
	}
	if v, ok := ts.quantileVal(gaugeDiffSize, "p90"); !ok || v != 180 {
		t.Errorf("diff-size p90 = %v,%v want 180,true", v, ok)
	}
	// A name absent from the vector is not present.
	if ts.has("gardenio_trust_does_not_exist") {
		t.Error("absent gauge should not be present")
	}
}

func TestParseTrustSeries_EmptyVectorIsAvailableButEmpty(t *testing.T) {
	ts, ok := parseTrustSeries([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	if !ok {
		t.Fatal("expected ok=true for an empty-but-successful vector")
	}
	if ts == nil {
		t.Fatal("expected non-nil series")
	}
	if len(ts.present) != 0 {
		t.Errorf("present = %v, want empty", ts.present)
	}
}

func TestParseTrustSeries_ErrorAndGarbage(t *testing.T) {
	if _, ok := parseTrustSeries([]byte(`{"status":"error","error":"boom"}`)); ok {
		t.Error("error status must be unavailable")
	}
	if _, ok := parseTrustSeries([]byte(`not json`)); ok {
		t.Error("garbage body must be unavailable")
	}
}

func TestBuildTrustDashboard_FullySurfacesR5Table(t *testing.T) {
	ts, _ := parseTrustSeries([]byte(fullTrustVector))
	now := time.Unix(1700000000, 0)
	v := buildTrustDashboard(ts, now)

	if !v.Available {
		t.Fatal("expected Available=true when the gauge family is published")
	}

	// North-star: #1, value formatted per-week, with its escaped-defect balance.
	if v.NorthStar.Num != 1 || !v.NorthStar.IsNorthStar {
		t.Errorf("north-star = #%d northstar=%v, want #1 true", v.NorthStar.Num, v.NorthStar.IsNorthStar)
	}
	if v.NorthStar.Value != "12.5 / wk" {
		t.Errorf("north-star value = %q, want %q", v.NorthStar.Value, "12.5 / wk")
	}
	if !strings.Contains(v.NorthStar.Balancing.Value, "5%") {
		t.Errorf("north-star balance = %q, want it to contain 5%%", v.NorthStar.Balancing.Value)
	}
	if !strings.Contains(v.NorthStar.Balancing.Name, "Escaped-defect") {
		t.Errorf("north-star balance name = %q, want Escaped-defect", v.NorthStar.Balancing.Name)
	}

	// Lane split (§3.2): leading={6,7,8,10}, lagging={2,3,4,5,9} (north-star #1
	// has its own hero, not in the lagging lane).
	leadingNums := metricNums(v.Leading)
	if got, want := leadingNums, []int{6, 7, 8, 10}; !equalInts(got, want) {
		t.Errorf("leading nums = %v, want %v", got, want)
	}
	laggingNums := metricNums(v.Lagging)
	if got, want := laggingNums, []int{2, 3, 4, 5, 9}; !equalInts(got, want) {
		t.Errorf("lagging nums = %v, want %v", got, want)
	}
	for _, c := range v.Leading {
		if c.Lane != trustLaneLeading {
			t.Errorf("#%d lane = %q, want leading", c.Num, c.Lane)
		}
	}
	for _, c := range v.Lagging {
		if c.Lane != trustLaneLagging {
			t.Errorf("#%d lane = %q, want lagging", c.Num, c.Lane)
		}
	}

	// Every metric card carries a non-empty balancing metric (the anti-gaming
	// pairing is the whole point of the dashboard).
	for _, c := range append(append([]trustMetricCard{v.NorthStar}, v.Leading...), v.Lagging...) {
		if c.Balancing.Name == "" {
			t.Errorf("#%d %q has no balancing metric", c.Num, c.Name)
		}
	}

	// Distribution metric surfaces p50 headline + p90 secondary.
	diff := cardByNum(t, v.Leading, 6)
	if diff.Value != "42 lines" {
		t.Errorf("diff-size value = %q, want %q", diff.Value, "42 lines")
	}
	if diff.Secondary != "p90 180 lines" {
		t.Errorf("diff-size secondary = %q, want %q", diff.Secondary, "p90 180 lines")
	}

	// Trust index (#3) is live here (responses>0).
	trust := cardByNum(t, v.Lagging, 3)
	if trust.AwaitingData {
		t.Error("trust index should be live when responses>0")
	}
	if trust.Value != "4.2" {
		t.Errorf("trust index value = %q, want 4.2", trust.Value)
	}

	// Context strip populated.
	if v.WindowDays != "28" || v.DeliveredMRCount != "40" || v.TotalMRCount != "55" {
		t.Errorf("context = window %q delivered %q total %q, want 28/40/55", v.WindowDays, v.DeliveredMRCount, v.TotalMRCount)
	}
	if v.LastCollectedAgo == "" {
		t.Error("expected a non-empty collected-ago label")
	}
}

func TestBuildTrustDashboard_AwaitingDataWhenNoSurvey(t *testing.T) {
	// A vector with zero trust-index responses → the trust index (#3) and the
	// time-to-merge balancing metric (which reads the trust index) both flag
	// awaiting-data instead of fabricating a score.
	body := `{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"__name__":"gardenio_trust_trusted_throughput_per_week"},"value":[1700000000,"3"]},
	  {"metric":{"__name__":"gardenio_trust_trust_index_median"},"value":[1700000000,"0"]},
	  {"metric":{"__name__":"gardenio_trust_trust_index_responses"},"value":[1700000000,"0"]}
	]}}`
	ts, _ := parseTrustSeries([]byte(body))
	v := buildTrustDashboard(ts, time.Unix(1700000000, 0))

	trust := cardByNum(t, v.Lagging, 3)
	if !trust.AwaitingData {
		t.Error("trust index should await data when responses=0")
	}
	if trust.AwaitingBead != trustSurveyBeadID {
		t.Errorf("trust index awaiting bead = %q, want %q", trust.AwaitingBead, trustSurveyBeadID)
	}

	ttm := cardByNum(t, v.Lagging, 5)
	if !ttm.Balancing.AwaitingData {
		t.Error("time-to-merge balance (trust index) should await data when responses=0")
	}

	// The #6 balancing metric (requested-slice completeness) has no gauge in the
	// shipped pipeline, so it always awaits data and cites the instrument bead.
	diff := cardByNum(t, v.Leading, 6)
	if !diff.Balancing.AwaitingData {
		t.Error("requested-slice completeness should always await data (no gauge yet)")
	}
	if diff.Balancing.AwaitingBead != trustMetricsBeadID {
		t.Errorf("completeness awaiting bead = %q, want %q", diff.Balancing.AwaitingBead, trustMetricsBeadID)
	}
}

func TestBuildTrustDashboard_DegradedWhenNil(t *testing.T) {
	// nil series (upstream unavailable): full skeleton, every value an em-dash,
	// Available=false. The framework still renders so the operator sees its shape.
	v := buildTrustDashboard(nil, time.Unix(1700000000, 0))
	if v.Available {
		t.Error("Available should be false for a nil series")
	}
	if v.NorthStar.Value != trustDash {
		t.Errorf("north-star value = %q, want em-dash", v.NorthStar.Value)
	}
	if len(v.Leading) != 4 || len(v.Lagging) != 5 {
		t.Errorf("lanes = %d leading / %d lagging, want 4/5 even when degraded", len(v.Leading), len(v.Lagging))
	}
	if v.MetricsBeadID != trustMetricsBeadID {
		t.Errorf("metrics bead id = %q, want %q", v.MetricsBeadID, trustMetricsBeadID)
	}
}

func TestBuildTrustDashboard_PresentZeroIsRealNotDash(t *testing.T) {
	// A present gauge of value 0 is a real zero (matching costs_rate convention),
	// not the absent em-dash.
	body := `{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"__name__":"gardenio_trust_trusted_throughput_per_week"},"value":[1700000000,"0"]}
	]}}`
	ts, _ := parseTrustSeries([]byte(body))
	v := buildTrustDashboard(ts, time.Unix(1700000000, 0))
	if !v.NorthStar.ValueAvailable {
		t.Error("a present zero gauge should be ValueAvailable=true")
	}
	if v.NorthStar.Value != "0.0 / wk" {
		t.Errorf("north-star value = %q, want %q", v.NorthStar.Value, "0.0 / wk")
	}
}

func TestFetchTrustSeries_NoUpstream(t *testing.T) {
	s := &Server{logger: slog.Default()}
	if _, ok := s.fetchTrustSeries(context.Background()); ok {
		t.Error("expected unavailable when no vmsingle upstream is wired")
	}
}

func TestFetchTrustSeries_QueryAndParse(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fullTrustVector))
	}))
	defer upstream.Close()

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)

	ts, ok := s.fetchTrustSeries(context.Background())
	if !ok {
		t.Fatal("expected available series")
	}
	if gotPath != vmsingleInstantQueryPath {
		t.Errorf("upstream path = %q, want %q", gotPath, vmsingleInstantQueryPath)
	}
	// The selector pulls the whole gardenio_trust_* family in one query.
	if gotQuery != trustSeriesSelector {
		t.Errorf("query = %q, want %q", gotQuery, trustSeriesSelector)
	}
	if v, ok := ts.scalarVal(gaugeTrustedThroughput); !ok || v != 12.5 {
		t.Errorf("parsed trusted-throughput = %v,%v want 12.5,true", v, ok)
	}
}

func TestFetchTrustSeries_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)
	if _, ok := s.fetchTrustSeries(context.Background()); ok {
		t.Error("expected unavailable on upstream 500")
	}
}

func TestHandleTrust_DegradedRendersSkeleton(t *testing.T) {
	// No upstream wired → page renders 200 with the framework skeleton and the
	// degraded banner pointing at the instrument bead.
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/trust", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Trust dashboard",
		"Trusted Throughput",
		"Leading indicators",
		"Lagging indicators",
		"Escaped-defect rate",
		trustMetricsBeadID, // degraded banner cites the instrument bead
	} {
		if !strings.Contains(body, want) {
			t.Errorf("degraded /trust body missing %q", want)
		}
	}
}

func TestHandleTrust_PopulatedFromUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fullTrustVector))
	}))
	defer upstream.Close()

	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/trust", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Live values surface, and the degraded banner does NOT (the family is present).
	if !strings.Contains(body, "12.5 / wk") {
		t.Errorf("populated /trust body missing the north-star value")
	}
	if strings.Contains(body, "TRUST_METRICS_ENABLED") {
		t.Errorf("populated /trust should not show the degraded banner")
	}
}

// --- test helpers -----------------------------------------------------------

func metricNums(cards []trustMetricCard) []int {
	out := make([]int, len(cards))
	for i, c := range cards {
		out[i] = c.Num
	}
	return out
}

func cardByNum(t *testing.T, cards []trustMetricCard, num int) trustMetricCard {
	t.Helper()
	for _, c := range cards {
		if c.Num == num {
			return c
		}
	}
	t.Fatalf("no metric card #%d in %v", num, metricNums(cards))
	return trustMetricCard{}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
