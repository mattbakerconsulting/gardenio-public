package main

// costs_rate.go — the server-side fleet hourly spend-rate used by the /costs
// summary headline and the /health cost tile (ka-0000002806).
//
// The headline "rate" number was previously GrandTotalUSD / hours-since-UTC-
// midnight — an *average over the day-so-far*, not a current rate. It read
// absurdly high on a quiet night: a small, unstable denominator in the early
// UTC hours (the <0.1 floor divided just-after-midnight spend by 0.1 → 10x),
// and a lumpy numerator because gardenio_agent_spend_usd_total increments once
// per session AT CLOSE — a long session closing just after midnight dumped its
// whole multi-hour cost into the early bucket, then got divided by a tiny
// "hours". The headline therefore disagreed with the rolling-window chart on
// the very same page.
//
// ka-0000004779 (this fix) re-pointed the headline AND the /costs "Fleet spend
// rate" chart onto gardenio_account_cost_usd — the LiteLLM-synced account-cost
// gauge (refreshed every ~5 min, stable cardinality, 0 resets/24h) — instead of
// the sparse at-close gardenio_agent_spend_usd_total counter. Two bugs fixed at once:
//
//  1. ACCURACY. The at-close counter reads multiples LOW versus the actual
//     LiteLLM bill (it lags up to a day and only ticks at session close); the
//     account-cost gauge tracks the real bill.
//  2. HEADLINE vs CHART. The query is now increase(metric[W]) / Whours rather
//     than rate(metric[W]) * 3600. On a counter, VictoriaMetrics extrapolates
//     rate() to the query edge on an INSTANT query but accumulates per-step on a
//     RANGE query, so the instant headline and the chart's last range point
//     disagreed by multiples. increase()/hours yields the same value whether
//     evaluated as instant or range, so the headline and the chart agree by
//     construction. (Confirmed vs live prod vmsingle: instant $55.1/hr == range
//     last point; the old rate()*3600 range last point read ~$3415/hr.)
//
// The headline stays an INSTANT query against the SAME PromQL the fleet-spend-
// rate chart renders (kbeads charts_catalog.go: fleet-spend-rate), so they match.
// A genuinely quiet trailing window reads ~$0/hr.
//
// ka-0000001436: both queries below carry the E2E-account exclusion
// (costsRollupE2EAccountExclusion, defined in costs_rollup.go) so the fleet
// headline drops the same retention-tail e2e test-account series the by-account
// rollup already drops — keeping headline == sum(rollup rows) even once an E2E
// harness accrues real spend. The kbeads chart catalog bakes the byte-identical
// matcher (accountCostE2EExclusion) into its account-cost gauge selector.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// fleetHourlyRateWindow is the look-back window for the headline. It MUST match
// the window the /costs page chart embed renders with: the fleet-spend-rate
// catalog chart defaults to window=1d (kbeads charts_catalog.go), and the cost
// card embeds it without overriding that default (echarts_bead.js passes only
// since/until/step). Keeping the headline on the same window is what makes the
// two agree. If the chart embed ever pins a different window, change this
// constant — and fleetHourlyRateWindowHours below — in lockstep.
const fleetHourlyRateWindow = "1d"

// fleetHourlyRateWindowHours is fleetHourlyRateWindow expressed in hours, the
// divisor that turns a windowed counter-increase into a per-hour rate. Must
// match windowHours(fleetHourlyRateWindow) in kbeads charts_catalog.go: "1d"→24.
const fleetHourlyRateWindowHours = "24"

// fleetHourlyRate1hWindow / fleetHourlyRate1hWindowHours are the SHORT-window
// counterpart shown alongside the 24h headline (ka-0000001327). A [1h] window
// is a 1-hour moving average — it reacts to load swings the 24h average hides,
// so the operator can read "right now" pace next to the smoothed daily one.
// "1h" → divide the windowed increase by 1 to get USD/hour.
const fleetHourlyRate1hWindow = "1h"
const fleetHourlyRate1hWindowHours = "1"

// fleetHourlyRateQuery is the instant-query PromQL for the fleet spend rate in
// USD/hour. It mirrors the fleet-spend-rate catalog chart's primary series
// (kbeads charts_catalog.go) with no selector (fleet-wide). increase()/hours —
// not rate()*3600 — so the instant headline equals the chart's range last point
// (ka-0000004779); see the file header for why rate() disagrees between instant
// and range on a counter.
//
// The `max by (account, env) (...)` dedup and the `[<window>:5m]` aligned
// subquery match the chart verbatim (ka-0000000170): the dedup collapses
// per-pod gauge duplicates across agent-pod churn, and the 5m subquery grid
// removes the per-step extrapolation sawtooth — so the headline stays numerically
// identical to the (now de-bumped) chart it sits beside.
//
// It also carries the E2E-account exclusion (costsRollupE2EAccountExclusion,
// ka-0000001436): the headline is the fleet SUM of the by-account rollup, which
// already drops the retention-tail e2e series, so filtering it here keeps the
// headline equal to sum(rollup rows). Without it, the first E2E harness run that
// accrues real LiteLLM cost would make headline > sum(rollup rows) with no
// visible cause — the headline-vs-table disagreement class ka-0000004779 exists
// to eliminate. Keep this in lockstep with charts_catalog.go's fleet-spend-rate
// QueryTemplate, which bakes the same accountCostE2EExclusion into its gauge
// selector; TestFleetHourlyRateQuery_ExcludesE2E pins the string on both sides.
const fleetHourlyRateQuery = "sum(increase((max by (account, env) (gardenio_account_cost_usd{" +
	costsRollupE2EAccountExclusion + "}))[" + fleetHourlyRateWindow + ":5m])) / " + fleetHourlyRateWindowHours

// fleetHourlyRate1hQuery is the 1h-window variant of fleetHourlyRateQuery,
// built with the same dedup + aligned-subquery + increase()/hours shape (and the
// same E2E-account exclusion) so it stays instant==range stable and matches the
// chart's "spend (1h)" companion.
const fleetHourlyRate1hQuery = "sum(increase((max by (account, env) (gardenio_account_cost_usd{" +
	costsRollupE2EAccountExclusion + "}))[" + fleetHourlyRate1hWindow + ":5m])) / " + fleetHourlyRate1hWindowHours

// vmsingleInstantQueryPath is the upstream instant-query endpoint. Distinct
// from the proxy's range-query path; reached only server-side (the daemon
// bearer / cluster-internal Service is never exposed to the browser).
const vmsingleInstantQueryPath = "/api/v1/query"

// vmInstantResponse is the subset of vmsingle's /api/v1/query JSON we read: a
// "vector" result whose single sample's value is [<ts>, "<stringified float>"].
type vmInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value [2]json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// fleetHourlyRateUSD returns the current fleet spend rate in USD/hour from
// vmsingle, computed as the rolling-window rate that backs the /costs chart.
//
// The bool is false (→ headline renders "—", matching the chart's empty state)
// when:
//   - no vmsingle upstream is wired (local dev / tests),
//   - the upstream errors or returns a non-200 / unparseable body, or
//   - the metric does not exist yet (empty result vector) — i.e. the E3 spend
//     counter has not landed in this environment.
//
// A present result is returned verbatim even when it is 0.00, so a genuinely
// quiet trailing window reads "$0.00/h" rather than the misleading "—".
//
// The 24h-window query backs the headline; fleetHourlyRate1hUSD runs the same
// round-trip against the 1h-window query for the short-window companion.
func (s *Server) fleetHourlyRateUSD(ctx context.Context) (float64, bool) {
	return s.queryFleetRate(ctx, fleetHourlyRateQuery)
}

// fleetHourlyRate1hUSD returns the 1-hour moving-average fleet spend rate in
// USD/hour (ka-0000001327) — the load-reactive companion to the 24h headline.
// Same availability semantics as fleetHourlyRateUSD.
func (s *Server) fleetHourlyRate1hUSD(ctx context.Context) (float64, bool) {
	return s.queryFleetRate(ctx, fleetHourlyRate1hQuery)
}

// queryFleetRate runs a single instant-query against vmsingle for the given
// USD/hour PromQL and parses the scalar result. Shared by the 24h and 1h
// rate accessors so the HTTP round-trip and availability rules live in one place.
func (s *Server) queryFleetRate(ctx context.Context, query string) (float64, bool) {
	if s.metricsUpstream == "" {
		return 0, false
	}
	client := s.metricsHTTP
	if client == nil {
		client = &http.Client{Timeout: metricsProxyTimeout}
	}

	u := s.metricsUpstream + vmsingleInstantQueryPath + "?query=" + url.QueryEscape(query)
	ctx, cancel := context.WithTimeout(ctx, metricsProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		s.logger.Warn("fleet hourly rate: build request", "error", err)
		return 0, false
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("fleet hourly rate: upstream error", "error", err)
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("fleet hourly rate: upstream status", "status", resp.StatusCode)
		return 0, false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		s.logger.Warn("fleet hourly rate: read body", "error", err)
		return 0, false
	}
	return parseFleetHourlyRate(body)
}

// parseFleetHourlyRate extracts the scalar USD/hour from a vmsingle instant
// "vector" response. Split out so the JSON-shape handling is unit-testable
// without an HTTP round-trip. Returns (0, false) for any shape that does not
// carry exactly one finite numeric sample.
func parseFleetHourlyRate(body []byte) (float64, bool) {
	var parsed vmInstantResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false
	}
	if parsed.Status != "success" || len(parsed.Data.Result) == 0 {
		return 0, false
	}
	// The value is [<unix ts>, "<float as string>"]; the second element is the
	// JSON string we want. An empty vector (handled above) means the metric is
	// absent; a present sample of "0" is a real zero we keep.
	raw := parsed.Data.Result[0].Value[1]
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return 0, false
	}
	str = strings.TrimSpace(str)
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return 0, false
	}
	// vmsingle can emit NaN/Inf as "NaN"/"+Inf"; ParseFloat accepts those, but
	// they are not a meaningful rate, so reject.
	if v != v || v > 1e300 || v < -1e300 {
		return 0, false
	}
	return v, true
}

// applyFleetHourlyRate sets the rolling-window headline rate (and the derived
// 24h projection) on a summary from vmsingle. Shared by /costs and the /health
// cost tile so both surfaces show the same number. On unavailability the rate
// fields are left zero and HourlyRateAvailable stays false, so the surfaces
// render their honest "—" state rather than a fabricated average.
func (s *Server) applyFleetHourlyRate(ctx context.Context, sum *costsSummaryView) {
	// The 1h moving average (ka-0000001327) is fetched independently and shown
	// alongside the 24h headline so the operator can read load-reactive "right
	// now" pace next to the smoothed daily one. It has its own availability flag
	// so a transient miss on one window doesn't blank the other.
	if rate1h, ok := s.fleetHourlyRate1hUSD(ctx); ok {
		sum.HourlyRate1hUSD = rate1h
		sum.HourlyRate1hAvailable = true
	}

	rate, ok := s.fleetHourlyRateUSD(ctx)
	if !ok {
		return
	}
	sum.HourlyRateUSD = rate
	sum.HourlyRateAvailable = true
	// Projected-today is the trailing-window rate extended across a full day.
	// On the same rolling basis this reads as "if the last window's pace held
	// for 24h" — consistent with the headline rather than extrapolating the
	// old inflated average.
	sum.ProjectedTodayUSD = rate * 24
}
