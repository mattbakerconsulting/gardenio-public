package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// vmDarkVector is a vmsingle instant "vector" response carrying a single
// scalar sample — the shape parseWebhookDark reads. value is the stringified
// float the deadman gauge sets (0 = live, 1 = dark).
func vmDarkVector(value string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"` + value + `"]}]}}`
}

func TestParseWebhookDark(t *testing.T) {
	cases := []struct {
		name string
		body string
		want webhookDarkState
	}{
		{"dark", vmDarkVector("1"), webhookDarkDark},
		{"live", vmDarkVector("0"), webhookDarkLive},
		{"dark-float", vmDarkVector("1.0"), webhookDarkDark},
		{"live-float", vmDarkVector("0.0"), webhookDarkLive},
		{"empty-vector", `{"status":"success","data":{"resultType":"vector","result":[]}}`, webhookDarkUnknown},
		{"error-status", `{"status":"error","data":{"resultType":"vector","result":[]}}`, webhookDarkUnknown},
		{"garbage", `not json`, webhookDarkUnknown},
		{"nan", vmDarkVector("NaN"), webhookDarkUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseWebhookDark([]byte(c.body)); got != c.want {
				t.Errorf("parseWebhookDark(%s) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// TestWebhookDarkForSource_SourceWithoutGauge confirms a source that exports no
// deadman gauge (slack / controller) never even issues a round-trip — it must
// short-circuit to Unknown so those rows stay purely age-classified.
func TestWebhookDarkForSource_SourceWithoutGauge(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(vmDarkVector("1")))
	}))
	defer upstream.Close()

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)

	if got := s.webhookDarkForSource(t.Context(), "source:slack"); got != webhookDarkUnknown {
		t.Errorf("slack source dark = %v, want Unknown (no deadman gauge)", got)
	}
	if called {
		t.Error("expected NO upstream round-trip for a source without a deadman gauge")
	}
}

// TestWebhookDarkForSource_NoUpstream confirms the whole feature is inert when
// no vmsingle is wired — the local-dev / test default — so the tile degrades to
// its prior age-only behavior.
func TestWebhookDarkForSource_NoUpstream(t *testing.T) {
	s := &Server{logger: slog.Default()}
	if got := s.webhookDarkForSource(t.Context(), "source:jira"); got != webhookDarkUnknown {
		t.Errorf("no-upstream jira dark = %v, want Unknown", got)
	}
}

// TestWebhookDarkForSource_QueryAndVerdict exercises the full instant-query
// round-trip: the jira source must query the jira deadman gauge and read the
// dark verdict back.
func TestWebhookDarkForSource_QueryAndVerdict(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vmDarkVector("1")))
	}))
	defer upstream.Close()

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)

	if got := s.webhookDarkForSource(t.Context(), "source:jira"); got != webhookDarkDark {
		t.Fatalf("jira dark = %v, want Dark", got)
	}
	if gotPath != vmsingleInstantQueryPath {
		t.Errorf("path = %q, want %q (instant query, never range)", gotPath, vmsingleInstantQueryPath)
	}
	if !strings.Contains(gotQuery, "gardenio_jira_webhook_dark") {
		t.Errorf("query = %q, want the jira deadman gauge", gotQuery)
	}
}

// mkHealthServerWithMetrics is mkHealthServer plus a wired vmsingle upstream,
// so the bridges tile can exercise the dark-verdict path end-to-end.
func mkHealthServerWithMetrics(t *testing.T, daemon beadAPI, beadsURL, metricsURL string) *http.ServeMux {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", beadsURL, "", nil)
	srv.SetMetricsProxy(metricsURL)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux
}

// TestHealthTile_Bridges_DarkVerdictShowsDarkPill is the acceptance test for
// GAP-D: a fresh last-event bead would normally render "ok", but a tripped
// jira/gitlab deadman gauge must escalate the row to the loud "dark" pill.
func TestHealthTile_Bridges_DarkVerdictShowsDarkPill(t *testing.T) {
	now := time.Now()
	f := newFake()
	// A recent bead per source — absent the dark verdict every row is "ok".
	f.listResp = &beadsapi.ListBeadsResult{
		Total: 1,
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-src-recent", CreatedAt: now.Add(-2 * time.Minute)},
		},
	}
	snap := fakeSnapshotServer(t, nil)

	// vmsingle returns dark=1 for the jira gauge, live=0 for gitlab; any other
	// query (there is none for slack/controller) yields an empty vector.
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "gardenio_jira_webhook_dark"):
			_, _ = w.Write([]byte(vmDarkVector("1")))
		case strings.Contains(q, "gardenio_gitlab_webhook_dark"):
			_, _ = w.Write([]byte(vmDarkVector("0")))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer vm.Close()

	mux := mkHealthServerWithMetrics(t, f, snap.URL, vm.URL)

	req := httptest.NewRequest("GET", "/health/tile/bridges", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// The dark pill must render (jira went dark).
	if !strings.Contains(body, ">dark<") {
		t.Errorf("expected a dark pill for the tripped jira deadman, body=%s", body)
	}
	// gitlab's monitor ran and read live -> the "monitored" affordance.
	if !strings.Contains(body, ">monitored<") {
		t.Errorf("expected a monitored pill for the live gitlab deadman, body=%s", body)
	}
	// slack + controller have no gauge, so no dark/monitored escalation — the
	// dark pill count stays exactly one.
	if n := strings.Count(body, ">dark<"); n != 1 {
		t.Errorf("expected exactly 1 dark pill (jira only), got %d, body=%s", n, body)
	}
}

// TestHealthTile_Bridges_DarkOverridesStale confirms the verdict wins over the
// age classification even when the last-event age would read "stale" — a dark
// hook must not hide behind a merely-old row.
func TestHealthTile_Bridges_DarkOverridesStale(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Total: 1,
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-src-old", CreatedAt: now.Add(-12 * time.Hour)}, // stale by age
		},
	}
	snap := fakeSnapshotServer(t, nil)
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "gardenio_gitlab_webhook_dark") {
			_, _ = w.Write([]byte(vmDarkVector("1")))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer vm.Close()

	mux := mkHealthServerWithMetrics(t, f, snap.URL, vm.URL)
	req := httptest.NewRequest("GET", "/health/tile/bridges", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, ">dark<") {
		t.Errorf("expected dark pill to override stale age, body=%s", body)
	}
}

// TestHealthTile_Bridges_NoUpstreamUnchanged confirms that with no vmsingle
// wired the tile renders exactly as before GAP-D — no dark/monitored pills.
func TestHealthTile_Bridges_NoUpstreamUnchanged(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Total: 1,
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-src-recent", CreatedAt: now.Add(-2 * time.Minute)},
		},
	}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL) // no metrics proxy

	req := httptest.NewRequest("GET", "/health/tile/bridges", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, ">dark<") || strings.Contains(body, ">monitored<") {
		t.Errorf("no-upstream tile must not render dark/monitored pills, body=%s", body)
	}
	if !strings.Contains(body, ">ok<") {
		t.Errorf("no-upstream tile should still classify fresh rows ok, body=%s", body)
	}
}
