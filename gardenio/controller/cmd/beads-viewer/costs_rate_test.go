package main

// Tests for the rolling-window fleet hourly-rate headline (ka-0000002806):
// JSON-shape parsing, the vmsingle instant-query round-trip, the agrees-with-
// the-chart query string, and the unavailable→"—" fallback.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseFleetHourlyRate(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantVal float64
		wantOK  bool
	}{
		{
			name:    "single vector sample",
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"3.5"]}]}}`,
			wantVal: 3.5,
			wantOK:  true,
		},
		{
			name:    "real zero is kept",
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"0"]}]}}`,
			wantVal: 0,
			wantOK:  true,
		},
		{
			name:   "empty result vector is unavailable",
			body:   `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			wantOK: false,
		},
		{
			name:   "error status is unavailable",
			body:   `{"status":"error","errorType":"bad_data","error":"boom"}`,
			wantOK: false,
		},
		{
			name:   "garbage body is unavailable",
			body:   `not json`,
			wantOK: false,
		},
		{
			name:   "NaN value is rejected",
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"NaN"]}]}}`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFleetHourlyRate([]byte(tc.body))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.wantVal {
				t.Errorf("val = %v, want %v", got, tc.wantVal)
			}
		})
	}
}

func TestFleetHourlyRateUSD_NoUpstream(t *testing.T) {
	s := &Server{logger: slog.Default()}
	if _, ok := s.fleetHourlyRateUSD(context.Background()); ok {
		t.Error("expected unavailable when no vmsingle upstream is wired")
	}
}

func TestFleetHourlyRateUSD_QueryAndValue(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"12.34"]}]}}`))
	}))
	defer upstream.Close()

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)

	val, ok := s.fleetHourlyRateUSD(context.Background())
	if !ok {
		t.Fatal("expected available rate")
	}
	if val != 12.34 {
		t.Errorf("val = %v, want 12.34", val)
	}
	// Instant-query endpoint, never the range endpoint.
	if gotPath != vmsingleInstantQueryPath {
		t.Errorf("upstream path = %q, want %q", gotPath, vmsingleInstantQueryPath)
	}
	// The query must be the SAME rolling-window PromQL the fleet-spend-rate chart
	// uses so the headline agrees with the chart: a max-by-deduped account_cost
	// over a [1d:5m] aligned subquery, /24, carrying the E2E-account exclusion
	// (ka-0000004779, ka-0000000170, ka-0000001436). increase()/hours — NOT
	// rate()*3600 — is what makes the instant headline equal the chart's range
	// last point; the dedup + 5m subquery grid de-bump it.
	if !strings.Contains(gotQuery, "increase((max by (account, env) (gardenio_account_cost_usd{"+costsRollupE2EAccountExclusion+"}))["+fleetHourlyRateWindow+":5m])") {
		t.Errorf("query = %q, want deduped windowed increase of E2E-excluded account_cost over [%s:5m]", gotQuery, fleetHourlyRateWindow)
	}
	if !strings.Contains(gotQuery, "/ "+fleetHourlyRateWindowHours) {
		t.Errorf("query = %q, want division by window-hours (/ %s)", gotQuery, fleetHourlyRateWindowHours)
	}
	if strings.Contains(gotQuery, "rate(") || strings.Contains(gotQuery, "* 3600") {
		t.Errorf("query = %q, must not use rate()*3600 (instant!=range on counters)", gotQuery)
	}
}

func TestFleetHourlyRate1hUSD_QueryAndValue(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"42.5"]}]}}`))
	}))
	defer upstream.Close()

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)

	val, ok := s.fleetHourlyRate1hUSD(context.Background())
	if !ok {
		t.Fatal("expected available 1h rate")
	}
	if val != 42.5 {
		t.Errorf("val = %v, want 42.5", val)
	}
	// The 1h companion (ka-0000001327) must use the [1h:5m] aligned-subquery
	// window divided by 1 — a true 1-hour moving average, distinct from the 24h
	// headline, deduped + de-bumped and E2E-excluded to match the chart
	// (ka-0000000170, ka-0000001436).
	if !strings.Contains(gotQuery, "increase((max by (account, env) (gardenio_account_cost_usd{"+costsRollupE2EAccountExclusion+"}))["+fleetHourlyRate1hWindow+":5m])") {
		t.Errorf("query = %q, want deduped windowed increase of E2E-excluded account_cost over [%s:5m]", gotQuery, fleetHourlyRate1hWindow)
	}
	if !strings.Contains(gotQuery, "/ "+fleetHourlyRate1hWindowHours) {
		t.Errorf("query = %q, want division by 1h-window hours (/ %s)", gotQuery, fleetHourlyRate1hWindowHours)
	}
	if strings.Contains(gotQuery, "rate(") || strings.Contains(gotQuery, "* 3600") {
		t.Errorf("query = %q, must not use rate()*3600 (instant!=range on counters)", gotQuery)
	}
}

// TestFleetHourlyRateQuery_ExcludesE2E pins the ka-0000001436 fix at the
// constant level: both fleet-rate headline queries must embed the
// E2E-account exclusion so the headline drops the same retention-tail e2e
// series the by-account rollup already drops (keeping headline == sum(rollup
// rows) even once an E2E harness accrues real spend). The exclusion is the SAME
// costsRollupE2EAccountExclusion the native rollup and Accounts dropdown use, so
// all three viewer surfaces filter identically; the kbeads chart catalog pins the
// byte-identical literal (accountCostE2EExclusion) on its side.
func TestFleetHourlyRateQuery_ExcludesE2E(t *testing.T) {
	for name, q := range map[string]string{
		"fleetHourlyRateQuery":   fleetHourlyRateQuery,
		"fleetHourlyRate1hQuery": fleetHourlyRate1hQuery,
	} {
		if !strings.Contains(q, costsRollupE2EAccountExclusion) {
			t.Errorf("%s must carry the E2E-account exclusion %q:\n  %s", name, costsRollupE2EAccountExclusion, q)
		}
		// The exclusion belongs INSIDE the account-cost gauge's own matcher, not
		// wrapped around the whole expression — verify it sits directly on the gauge.
		if !strings.Contains(q, "gardenio_account_cost_usd{"+costsRollupE2EAccountExclusion+"}") {
			t.Errorf("%s must apply the exclusion inside the account-cost gauge matcher:\n  %s", name, q)
		}
	}
}

func TestFleetHourlyRateUSD_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)
	if _, ok := s.fleetHourlyRateUSD(context.Background()); ok {
		t.Error("expected unavailable on upstream 500")
	}
}

func TestApplyFleetHourlyRate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"2.5"]}]}}`))
	}))
	defer upstream.Close()

	s := &Server{logger: slog.Default()}
	s.SetMetricsProxy(upstream.URL)

	sum := &costsSummaryView{}
	s.applyFleetHourlyRate(context.Background(), sum)
	if !sum.HourlyRateAvailable {
		t.Fatal("HourlyRateAvailable = false, want true")
	}
	if sum.HourlyRateUSD != 2.5 {
		t.Errorf("HourlyRateUSD = %v, want 2.5", sum.HourlyRateUSD)
	}
	if sum.ProjectedTodayUSD != 2.5*24 {
		t.Errorf("ProjectedTodayUSD = %v, want %v", sum.ProjectedTodayUSD, 2.5*24)
	}
	// The 1h moving-average companion (ka-0000001327) is fetched and set too.
	// This upstream answers every window with the same value.
	if !sum.HourlyRate1hAvailable {
		t.Fatal("HourlyRate1hAvailable = false, want true")
	}
	if sum.HourlyRate1hUSD != 2.5 {
		t.Errorf("HourlyRate1hUSD = %v, want 2.5", sum.HourlyRate1hUSD)
	}
}

func TestApplyFleetHourlyRate_UnavailableLeavesZero(t *testing.T) {
	// No upstream → headline must stay zero / unavailable so the surface shows
	// "—" rather than a fabricated number (the whole point of ka-0000002806).
	s := &Server{logger: slog.Default()}
	sum := &costsSummaryView{HourlyRateUSD: 999, ProjectedTodayUSD: 999}
	s.applyFleetHourlyRate(context.Background(), sum)
	if sum.HourlyRateAvailable {
		t.Error("HourlyRateAvailable = true, want false")
	}
	// applyFleetHourlyRate must not touch the fields when unavailable; the
	// caller is responsible for leaving them at the buildCostsSummary zero.
	if sum.HourlyRateUSD != 999 {
		t.Errorf("HourlyRateUSD = %v, want untouched (999)", sum.HourlyRateUSD)
	}
}
