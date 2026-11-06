package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// charts_engagement_test.go — engagement panel + server-side identity
// resolution tests (ka-0000004824).
//
// The privacy-critical acceptance is asserted directly here:
//   - the PromQL the panels evaluate NEVER references email / display /
//     slack_user_id — it groups on operator_id / surface only (TestEngagementQueries_NoPIILabels);
//   - identity is resolved SERVER-SIDE from the operators table, never read off
//     a metric label (TestResolveOperatorStats_ResolvesServerSide);
//   - a right-to-be-forgotten operator redacts to "deleted operator" with the
//     opaque id preserved (TestResolveOperatorStats_ForgottenRedacted).

// fakeVMVector is an httptest server returning a caller-supplied labeled
// instant vector for any query matching a substring key. Unlike fakeVM (which
// returns a single scalar) this exercises VMClient.QueryVector's multi-sample,
// per-series-label parse path.
func fakeVMVector(t *testing.T, byMatch map[string][]VMSample) *VMClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		q := r.Form.Get("query")
		var samples []VMSample
		for match, ss := range byMatch {
			if strings.Contains(q, match) {
				samples = ss
				break
			}
		}
		type vmResult struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		results := make([]vmResult, 0, len(samples))
		for _, s := range samples {
			results = append(results, vmResult{
				Metric: s.Metric,
				// VictoriaMetrics emits the value as a STRING, mirroring the
				// Prometheus HTTP API contract the parser depends on.
				Value: []any{1780000000, strconvFloat(s.Value)},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": results},
		})
	}))
	t.Cleanup(srv.Close)
	return NewVMClient(srv.URL, "")
}

func strconvFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// seedOperator registers an operator id with a display/email (and optional
// forgotten flag + slug) in the mock store, mirroring what the real operators
// table + ResolveOperators join would return.
func seedOperator(m *mockStore, id int64, kind, subject, display, email, slug string, forgotten bool) {
	if m.operators == nil {
		m.operators = map[int64]struct{ Kind, Subject string }{}
	}
	if m.operatorIdentities == nil {
		m.operatorIdentities = map[int64]struct{ Display, Email string }{}
	}
	if m.operatorSlugs == nil {
		m.operatorSlugs = map[string]int64{}
	}
	if m.operatorForgotten == nil {
		m.operatorForgotten = map[int64]bool{}
	}
	m.operators[id] = struct{ Kind, Subject string }{kind, subject}
	m.operatorIdentities[id] = struct{ Display, Email string }{display, email}
	if slug != "" {
		m.operatorSlugs[slug] = id
	}
	m.operatorForgotten[id] = forgotten
}

func TestEngagementQueries_NoPIILabels(t *testing.T) {
	queries := []string{
		activityBySurfaceQuery("7d"),
		activityByOperatorQuery("7d"),
		spendByOperatorQuery(),
	}
	forbidden := []string{"email", "display", "slack_user_id", "slack:", "oidc_email"}
	for _, q := range queries {
		for _, bad := range forbidden {
			if strings.Contains(q, bad) {
				t.Errorf("query %q must NOT reference PII label %q", q, bad)
			}
		}
		if strings.Contains(q, "increase(") || strings.Contains(q, "rate(") {
			t.Errorf("activity query %q must use last_over_time, not increase/rate", q)
		}
	}
	if !strings.Contains(activityByOperatorQuery("7d"), "by (operator_id)") {
		t.Error("activity-by-operator must group by operator_id only")
	}
}

func TestSessionQueries_NoPIILabels_QuantileShape(t *testing.T) {
	queries := []string{
		sessionCountByOperatorQuery("7d"),
		sessionQuantileByOperatorQuery(0.5, "7d"),
		sessionQuantileByOperatorQuery(0.9, "30d"),
		sessionQuantileByOperatorQuery(0.99, "1d"),
	}
	forbidden := []string{"email", "display", "slack_user_id", "slack:", "oidc_email"}
	for _, q := range queries {
		for _, bad := range forbidden {
			if strings.Contains(q, bad) {
				t.Errorf("query %q must NOT reference PII label %q", q, bad)
			}
		}
	}
	// Quantile queries must use histogram_quantile over the _bucket series,
	// grouped by operator_id + le only — that is the contract the panel rests on.
	pq := sessionQuantileByOperatorQuery(0.9, "7d")
	if !strings.Contains(pq, "histogram_quantile(0.9,") {
		t.Errorf("quantile query must call histogram_quantile with the φ: %q", pq)
	}
	if !strings.Contains(pq, "gardenio_operator_session_duration_seconds_bucket[7d]") {
		t.Errorf("quantile query must read the _bucket series over the window: %q", pq)
	}
	if !strings.Contains(pq, "by (operator_id, le)") {
		t.Errorf("quantile query must group by operator_id+le only: %q", pq)
	}
	// The count ranking reads the _count series, grouped by operator_id only.
	cq := sessionCountByOperatorQuery("7d")
	if !strings.Contains(cq, "gardenio_operator_session_duration_seconds_count[7d]") || !strings.Contains(cq, "by (operator_id)") {
		t.Errorf("count query must sum the _count series by operator_id: %q", cq)
	}
}

func TestHandleEngagementSessions_EndToEnd_ServerSideResolution(t *testing.T) {
	m := newMockStore()
	seedOperator(m, 7, "oidc", "a@b.c", "Alice", "a@b.c", "alice", false)
	seedOperator(m, 9, "slack", "U0OPERATOR1", "Bob", "bob@b.c", "bob", false)
	s := NewBeadsServer(m, nil)
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{
		// Ranking query — Bob(9) has more sessions than Alice(7).
		"_count": {
			{Metric: map[string]string{"operator_id": "9"}, Value: 40},
			{Metric: map[string]string{"operator_id": "7"}, Value: 10},
			{Metric: map[string]string{"operator_id": "0"}, Value: 3}, // unattributed
		},
		// Every quantile query matches "_bucket"; return per-operator durations.
		"_bucket": {
			{Metric: map[string]string{"operator_id": "9"}, Value: 300},
			{Metric: map[string]string{"operator_id": "7"}, Value: 120},
			{Metric: map[string]string{"operator_id": "0"}, Value: 60},
		},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/engagement/sessions?window=7d", nil)
	rec := httptest.NewRecorder()
	s.handleChartsEngagementSessions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp EngagementSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != "7d" {
		t.Errorf("window = %q, want 7d", resp.Window)
	}
	// Quantile column set is fixed: p50/p90/p99.
	if len(resp.Quantiles) != 3 || resp.Quantiles[0] != "p50" || resp.Quantiles[2] != "p99" {
		t.Errorf("quantiles = %v, want [p50 p90 p99]", resp.Quantiles)
	}
	// Ordered by session count desc: Bob(40), Alice(10), unattributed(3).
	if len(resp.TopOperators) != 3 {
		t.Fatalf("got %d operators, want 3", len(resp.TopOperators))
	}
	if resp.TopOperators[0].OperatorID != 9 || resp.TopOperators[0].Display != "Bob" {
		t.Errorf("top[0] = %+v, want operator 9 resolved to 'Bob'", resp.TopOperators[0])
	}
	if resp.TopOperators[0].SessionCount != 40 {
		t.Errorf("top[0].SessionCount = %v, want 40", resp.TopOperators[0].SessionCount)
	}
	// Every reported quantile key must be present on every row.
	for _, key := range []string{"p50", "p90", "p99"} {
		if _, ok := resp.TopOperators[0].Quantiles[key]; !ok {
			t.Errorf("top[0] missing quantile %q: %+v", key, resp.TopOperators[0].Quantiles)
		}
	}
	if resp.TopOperators[1].OperatorID != 7 || resp.TopOperators[1].Display != "Alice" || resp.TopOperators[1].Slug != "alice" {
		t.Errorf("top[1] = %+v, want operator 7 resolved with slug", resp.TopOperators[1])
	}
	last := resp.TopOperators[2]
	if last.OperatorID != unattributedOperatorID || last.Display != unattributedDisplay {
		t.Errorf("top[2] = %+v, want unattributed bucket", last)
	}
	// Identity is resolved server-side: the slack_user_id never crosses the wire.
	if strings.Contains(rec.Body.String(), "U0OPERATOR1") {
		t.Error("response leaked a slack_user_id — identity must resolve server-side")
	}
}

func TestHandleEngagementSessions_TopNCap(t *testing.T) {
	m := newMockStore()
	counts := make([]VMSample, 0, engagementTopN+5)
	buckets := make([]VMSample, 0, engagementTopN+5)
	for i := int64(1); i <= int64(engagementTopN)+5; i++ {
		seedOperator(m, i, "oidc", "", "op", "", "", false)
		idStr := strconv.FormatInt(i, 10)
		counts = append(counts, VMSample{Metric: map[string]string{"operator_id": idStr}, Value: float64(i)})
		buckets = append(buckets, VMSample{Metric: map[string]string{"operator_id": idStr}, Value: float64(i * 10)})
	}
	s := NewBeadsServer(m, nil)
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{
		"_count":  counts,
		"_bucket": buckets,
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/engagement/sessions", nil)
	rec := httptest.NewRecorder()
	s.handleChartsEngagementSessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp EngagementSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.TopOperators) != engagementTopN {
		t.Errorf("got %d operators, want top-N cap %d", len(resp.TopOperators), engagementTopN)
	}
	// Highest-count operator is ranked first.
	if resp.TopOperators[0].SessionCount != float64(engagementTopN)+5 {
		t.Errorf("top[0].SessionCount = %v, want %d", resp.TopOperators[0].SessionCount, engagementTopN+5)
	}
}

func TestHandleEngagementSessions_503WhenNoVM(t *testing.T) {
	s := NewBeadsServer(newMockStore(), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/charts/engagement/sessions", nil)
	s.handleChartsEngagementSessions(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when VM unconfigured", rec.Code)
	}
}

func TestResolveWindow_BoundedAllowlist(t *testing.T) {
	for raw, want := range map[string]string{
		"1d": "1d", "7d": "7d", "30d": "30d",
		"":           defaultEngagementWindow,
		"9999d":      defaultEngagementWindow,
		"1d])evil[(": defaultEngagementWindow, // injection attempt → default
		"7":          defaultEngagementWindow,
	} {
		if got := resolveWindow(raw); got != want {
			t.Errorf("resolveWindow(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestResolveOperatorStats_ResolvesServerSide(t *testing.T) {
	m := newMockStore()
	seedOperator(m, 7, "oidc", "matthew.baker@acme.example", "Matthew Baker", "matthew.baker@acme.example", "matthew.baker", false)
	seedOperator(m, 9, "slack", "U0OPERATOR1", "Ada L", "ada@acme.example", "ada", false)
	s := NewBeadsServer(m, nil)

	// Metric samples carry ONLY operator_id — no display/email anywhere.
	samples := []VMSample{
		{Metric: map[string]string{"operator_id": "7"}, Value: 42},
		{Metric: map[string]string{"operator_id": "9"}, Value: 100},
		{Metric: map[string]string{"operator_id": "0"}, Value: 5}, // unattributed
	}
	top, others, err := s.resolveOperatorStats(context.Background(), samples, 25)
	if err != nil {
		t.Fatalf("resolveOperatorStats: %v", err)
	}
	if others != 0 {
		t.Errorf("others = %v, want 0 (under cap)", others)
	}
	// Descending by value: 9 (100), 7 (42), 0 (5).
	if len(top) != 3 {
		t.Fatalf("got %d stats, want 3", len(top))
	}
	if top[0].OperatorID != 9 || top[0].Display != "Ada L" {
		t.Errorf("top[0] = %+v, want operator 9 resolved to 'Ada L'", top[0])
	}
	if top[1].OperatorID != 7 || top[1].Display != "Matthew Baker" || top[1].Slug != "matthew.baker" {
		t.Errorf("top[1] = %+v, want operator 7 resolved with slug", top[1])
	}
	if top[2].OperatorID != unattributedOperatorID || top[2].Display != unattributedDisplay {
		t.Errorf("top[2] = %+v, want unattributed bucket", top[2])
	}
}

func TestResolveOperatorStats_ForgottenRedacted(t *testing.T) {
	m := newMockStore()
	seedOperator(m, 7, "oidc", "gone@acme.example", "Should Not Appear", "gone@acme.example", "gone", true)
	s := NewBeadsServer(m, nil)

	samples := []VMSample{{Metric: map[string]string{"operator_id": "7"}, Value: 3}}
	top, _, err := s.resolveOperatorStats(context.Background(), samples, 25)
	if err != nil {
		t.Fatalf("resolveOperatorStats: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("got %d stats, want 1", len(top))
	}
	got := top[0]
	if !got.Forgotten {
		t.Error("forgotten operator must set Forgotten=true")
	}
	if got.Display != "" {
		t.Errorf("forgotten display = %q, want empty", got.Display)
	}
	if got.Email != "deleted operator" {
		t.Errorf("forgotten email = %q, want 'deleted operator'", got.Email)
	}
	if got.OperatorID != 7 {
		t.Errorf("opaque id must persist: got %d, want 7", got.OperatorID)
	}
	// The redaction must never leak the original display/email.
	if strings.Contains(got.Display+got.Email, "Should Not Appear") || strings.Contains(got.Email, "gone@") {
		t.Errorf("forgotten operator leaked PII: %+v", got)
	}
}

func TestResolveOperatorStats_TopNCapAndOthers(t *testing.T) {
	m := newMockStore()
	samples := make([]VMSample, 0, 5)
	for i := int64(1); i <= 5; i++ {
		seedOperator(m, i, "oidc", "", "op"+string(rune('0'+i)), "", "", false)
		samples = append(samples, VMSample{Metric: map[string]string{"operator_id": strconv.FormatInt(i, 10)}, Value: float64(i)})
	}
	s := NewBeadsServer(m, nil)
	top, others, err := s.resolveOperatorStats(context.Background(), samples, 2)
	if err != nil {
		t.Fatalf("resolveOperatorStats: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("got %d stats, want 2 (cap)", len(top))
	}
	// Top 2 by value are 5 and 4; others = 3+2+1 = 6.
	if top[0].Value != 5 || top[1].Value != 4 {
		t.Errorf("top values = [%v,%v], want [5,4]", top[0].Value, top[1].Value)
	}
	if others != 6 {
		t.Errorf("others = %v, want 6", others)
	}
}

func TestHandleEngagementActivity_EndToEnd(t *testing.T) {
	m := newMockStore()
	seedOperator(m, 7, "oidc", "a@b.c", "Alice", "a@b.c", "alice", false)
	s := NewBeadsServer(m, nil)
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{
		"by (surface)": {
			{Metric: map[string]string{"surface": "slack"}, Value: 30},
			{Metric: map[string]string{"surface": "web"}, Value: 70},
		},
		"by (operator_id)": {
			{Metric: map[string]string{"operator_id": "7"}, Value: 100},
		},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/engagement/activity?window=7d", nil)
	rec := httptest.NewRecorder()
	s.handleChartsEngagementActivity(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp EngagementActivityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != "7d" {
		t.Errorf("window = %q, want 7d", resp.Window)
	}
	// Surfaces descending by count: web(70), slack(30).
	if len(resp.BySurface) != 2 || resp.BySurface[0].Surface != "web" {
		t.Errorf("by_surface = %+v, want web first", resp.BySurface)
	}
	if len(resp.TopOperators) != 1 || resp.TopOperators[0].Display != "Alice" {
		t.Errorf("top_operators = %+v, want Alice resolved", resp.TopOperators)
	}
	// The raw JSON body must not contain the slack_user_id anywhere — identity
	// is resolved to a display name, the provider id never crosses the wire.
	if strings.Contains(rec.Body.String(), "U0OPERATOR1") {
		t.Error("response leaked a slack_user_id")
	}
}

func TestHandleEngagement_503WhenNoVM(t *testing.T) {
	s := NewBeadsServer(newMockStore(), nil)
	for _, path := range []string{"/v1/charts/engagement/activity", "/v1/charts/engagement/spend"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		switch path {
		case "/v1/charts/engagement/activity":
			s.handleChartsEngagementActivity(rec, req)
		default:
			s.handleChartsEngagementSpend(rec, req)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 when VM unconfigured", path, rec.Code)
		}
	}
}

func TestQuantile_NearestRankPrecise(t *testing.T) {
	xs := []float64{10, 20, 30, 40, 100}
	if got := quantile(xs, 0.50); got != 30 {
		t.Errorf("p50 = %v, want 30", got)
	}
	if got := quantile(xs, 0.95); got != 100 {
		t.Errorf("p95 = %v, want 100", got)
	}
	if got := quantile(nil, 0.95); got != 0 {
		t.Errorf("p95(empty) = %v, want 0", got)
	}
}
