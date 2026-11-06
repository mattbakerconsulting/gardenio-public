package main

// /trust — the R5 Trust dashboard (ka-0000007085).
//
// Surfaces the R5 success-metrics table (bundle ka-0000008221, doc
// ka-0000003649 §3) live: the north-star (Trusted Throughput) front and centre,
// the metrics split into leading vs lagging lanes, and every primary metric
// rendered alongside its anti-gaming balancing metric — so a human reads trust
// rising AND review-effort falling together, not one in isolation.
//
// Data source. The blocking instrumentation (ka-0000000493, MR !4357 — merged)
// already lands all ten R5 metrics + their balancing metrics as Prometheus
// gauges under the `gardenio_trust_*` namespace (internal/trustmetrics, run from
// gitlab-bridge). The reference doc (gardenio/docs/r5-trusted-throughput-metrics.md)
// names THIS dashboard as the consumer. So we read those gauges live rather
// than re-deriving them — one server-side instant query against the SAME
// in-cluster vmsingle the /costs headline already uses (costs_rate.go), via the
// already-wired metricsUpstream (VMSINGLE_QUERY_URL; see main.go). The browser
// never touches vmsingle directly.
//
// Honest degrade. The collector is opt-in (TRUST_METRICS_ENABLED) and off in
// most environments, so the `gardenio_trust_*` family is frequently absent. When
// it is, the page still renders the full §3 table skeleton — north-star, both
// lanes, every primary/balancing pair — with em-dash placeholders and a banner
// pointing at ka-0000000493, so an operator sees the framework's shape and knows
// exactly what to switch on. Present-but-zero gauges are real values and are
// shown verbatim (matching the costs_rate convention); only a gauge missing
// from the result vector reads "—".
//
// Two metrics have no live numeric source yet and are flagged "awaiting data"
// with their owning bead, rather than faked: the Trust index (#3) needs the
// merge-time micro-survey (ka-0000007958) to write `trust_score`, so it stays
// "awaiting data" until responses arrive; and Requested-slice completeness (the
// #6 balancing metric) has no gauge in the shipped pipeline — adding it is
// follow-up on the instrument bead (ka-0000000493).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// trustSeriesSelector is the instant-query selector pulling the whole R5 metric
// family in one round trip. The `gardenio_trust_` name prefix is exactly the
// selection contract the instrumentation promises (metrics.go namespace +
// the reference doc), so a name-regex match returns every series — scalar
// gauges, the {quantile} distribution vecs, and the per-{reviewer} load — in a
// single vector response.
const trustSeriesSelector = `{__name__=~"gardenio_trust_.+"}`

// trustMetricsBeadID is the instrumentation bead that ships the gauges this
// page reads. Surfaced in the degraded-mode banner so an operator who sees an
// empty table knows where the data comes from and how to switch it on.
const trustMetricsBeadID = "ka-0000000493"

// trustSurveyBeadID owns the merge-time micro-survey that writes the trust-index
// input (#3). Cited on the Trust-index card while it awaits responses.
const trustSurveyBeadID = "ka-0000007958"

// R5 gauge names (metrics.go). Kept as constants so the dashboard selects the
// same series the collector publishes — a rename on either side is a compile
// reminder to update both.
const (
	gaugeTrustedThroughput   = "gardenio_trust_trusted_throughput_per_week"
	gaugeEscapedDefectRate   = "gardenio_trust_escaped_defect_rate"
	gaugeEscapedDefectCount  = "gardenio_trust_escaped_defect_count"
	gaugeReviewEffort        = "gardenio_trust_review_effort_rounds"
	gaugeTrustIndexMedian    = "gardenio_trust_trust_index_median"
	gaugeTrustIndexResponses = "gardenio_trust_trust_index_responses"
	gaugeApprovalNoComment   = "gardenio_trust_approval_without_comment_rate"
	gaugeReworkCycles        = "gardenio_trust_rework_cycles"
	gaugeTimeToMerge         = "gardenio_trust_time_to_merge_seconds"
	gaugeDiffSize            = "gardenio_trust_diff_size_lines"
	gaugeVerbosityRatio      = "gardenio_trust_verbosity_ratio"
	gaugeProvenance          = "gardenio_trust_provenance_completeness"
	gaugeReviewerLoadMax     = "gardenio_trust_reviewer_load_max_per_week"
	gaugeReviewerLoadMean    = "gardenio_trust_reviewer_load_mean_per_week"
	gaugePeelLatency         = "gardenio_trust_peel_latency_seconds"
	gaugeDeliveredMRCount    = "gardenio_trust_delivered_mr_count"
	gaugeTotalMRCount        = "gardenio_trust_total_mr_count"
	gaugeWindowDays          = "gardenio_trust_window_days"
	gaugeLastCollect         = "gardenio_trust_last_collect_timestamp_seconds"
)

// trustSeries holds the parsed `gardenio_trust_*` vector keyed for lookup.
// `scalar` carries un-labelled gauges (and the quantile vecs are split out into
// `quantile[name][p50|p90]`). `present` records that a name appeared at all so
// the view can distinguish "absent" (→ em-dash) from "present, value 0" (→ a
// real zero we keep, matching the costs_rate.go convention).
type trustSeries struct {
	scalar   map[string]float64
	quantile map[string]map[string]float64
	present  map[string]bool
}

// has reports whether name appeared in the result vector at all (any label set).
func (t *trustSeries) has(name string) bool { return t != nil && t.present[name] }

// scalarVal returns an un-labelled gauge's value and whether it was present.
func (t *trustSeries) scalarVal(name string) (float64, bool) {
	if t == nil {
		return 0, false
	}
	v, ok := t.scalar[name]
	return v, ok
}

// quantileVal returns a distribution gauge's value at quantile q ("p50"/"p90").
func (t *trustSeries) quantileVal(name, q string) (float64, bool) {
	if t == nil {
		return 0, false
	}
	qs, ok := t.quantile[name]
	if !ok {
		return 0, false
	}
	v, ok := qs[q]
	return v, ok
}

// vmSeriesResponse is the subset of vmsingle's /api/v1/query JSON we read for a
// multi-sample "vector" result: each sample carries its label set (`metric`,
// including `__name__` and any `quantile`/`reviewer`) and a
// `value: [<ts>, "<float as string>"]`.
type vmSeriesResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// parseTrustSeries extracts the R5 gauge family from a vmsingle instant "vector"
// response. Split out from the HTTP round-trip so the JSON-shape handling is
// unit-testable without a server. Returns (nil, false) for any non-success
// status or unparseable body; an empty-but-successful vector returns a non-nil
// empty *trustSeries with ok=true (the family is simply not published yet — the
// page renders its honest degraded state rather than an error).
func parseTrustSeries(body []byte) (*trustSeries, bool) {
	var parsed vmSeriesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	if parsed.Status != "success" {
		return nil, false
	}
	ts := &trustSeries{
		scalar:   map[string]float64{},
		quantile: map[string]map[string]float64{},
		present:  map[string]bool{},
	}
	for _, r := range parsed.Result() {
		name := r.Metric["__name__"]
		if name == "" {
			continue
		}
		v, ok := parseVMSampleValue(r.Value[1])
		if !ok {
			continue
		}
		ts.present[name] = true
		if q := r.Metric["quantile"]; q != "" {
			qs := ts.quantile[name]
			if qs == nil {
				qs = map[string]float64{}
				ts.quantile[name] = qs
			}
			qs[q] = v
			continue
		}
		// Un-labelled (or {reviewer}-labelled) gauge. The reviewer-keyed series
		// are summarised by the max/mean roll-up gauges, which we read directly;
		// collapsing the rest by name keeps the lookup flat. Last write wins,
		// which is harmless for the single-sample scalar gauges this page reads.
		ts.scalar[name] = v
	}
	return ts, true
}

// Result is a tiny accessor so parseTrustSeries can range over the decoded
// samples without leaking the anonymous struct type into the signature.
func (r vmSeriesResponse) Result() []struct {
	Metric map[string]string  `json:"metric"`
	Value  [2]json.RawMessage `json:"value"`
} {
	return r.Data.Result
}

// fetchTrustSeries runs the single instant query against vmsingle and parses the
// R5 gauge family. The bool is false when no upstream is wired (local dev /
// tests) or the upstream errors / returns a non-200 / unparseable body — the
// handler then renders the degraded skeleton. A successful-but-empty vector
// returns a non-nil empty series with ok=true. Mirrors queryFleetRate's
// round-trip and availability rules (costs_rate.go).
func (s *Server) fetchTrustSeries(ctx context.Context) (*trustSeries, bool) {
	if s.metricsUpstream == "" {
		return nil, false
	}
	client := s.metricsHTTP
	if client == nil {
		client = &http.Client{Timeout: metricsProxyTimeout}
	}

	u := s.metricsUpstream + vmsingleInstantQueryPath + "?query=" + url.QueryEscape(trustSeriesSelector)
	ctx, cancel := context.WithTimeout(ctx, metricsProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		s.logger.Warn("trust dashboard: build request", "error", err)
		return nil, false
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("trust dashboard: upstream error", "error", err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("trust dashboard: upstream status", "status", resp.StatusCode)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		s.logger.Warn("trust dashboard: read body", "error", err)
		return nil, false
	}
	return parseTrustSeries(body)
}

// trustDashboardView is the page view model.
type trustDashboardView struct {
	// Available is true once the `gardenio_trust_*` family is being published in
	// this environment (the collector is on and has run at least once). When
	// false the table renders its skeleton and the degraded banner shows.
	Available bool
	// MetricsBeadID / SurveyBeadID back the banner + awaiting-data card links.
	MetricsBeadID string
	SurveyBeadID  string

	// NorthStar is metric #1, rendered as the hero with its balancing metric
	// (escaped-defect rate) beside it.
	NorthStar trustMetricCard
	// Leading / Lagging are the §3.2 lanes (north-star excluded from Lagging —
	// it has its own hero).
	Leading []trustMetricCard
	Lagging []trustMetricCard

	// Context strip (window + delivered/total MR counts + freshness).
	WindowDays       string
	DeliveredMRCount string
	TotalMRCount     string
	LastCollectedAgo string
}

// trustMetricCard is one primary R5 metric paired with its anti-gaming
// balancing metric. The primary value is the headline; Secondary carries the
// p90 (or a context number) when the metric is a distribution. Balancing is the
// paired metric that must move the right way too — surfaced on the same card so
// a gain is never read in isolation.
type trustMetricCard struct {
	Num         int
	Name        string
	Definition  string
	Lane        string // "leading" | "lagging"
	IsNorthStar bool

	Value          string // formatted primary value, or "—" when absent
	ValueAvailable bool
	Secondary      string // e.g. "p90 4" — empty when not applicable
	Target         string // §3.1 target / direction text

	// AwaitingData marks a metric with no live numeric source yet (Trust index
	// before any survey responses). AwaitingNote explains it; AwaitingBeadID
	// links the owning follow-up bead.
	AwaitingData bool
	AwaitingNote string
	AwaitingBead string

	Balancing trustBalancingCell
}

// trustBalancingCell is the anti-gaming metric paired with a primary metric.
type trustBalancingCell struct {
	Name           string
	Value          string // formatted, or "—"
	ValueAvailable bool
	Note           string // why this balances the primary (the §3.1 rationale)
	// AwaitingData / AwaitingBead flag a balancing metric with no live source
	// yet (Requested-slice completeness — no gauge in the shipped pipeline).
	AwaitingData bool
	AwaitingBead string
}

// buildTrustDashboard reduces a parsed gauge family into the page view model.
// PURE (no I/O, no clock beyond the passed `now`) so it is fully unit-testable.
// `ts` may be nil (upstream unavailable) — every card then renders "—" and
// Available stays false. `now` stamps the "collected N ago" freshness label.
func buildTrustDashboard(ts *trustSeries, now time.Time) trustDashboardView {
	v := trustDashboardView{
		MetricsBeadID: trustMetricsBeadID,
		SurveyBeadID:  trustSurveyBeadID,
	}
	// The family is "available" once any series is present — the collector
	// Publishes the whole set together, so one present gauge means the pipeline
	// has run in this environment.
	v.Available = ts != nil && len(ts.present) > 0

	// Shared cell builders so the same balancing metric reads identically
	// wherever it is paired (e.g. escaped-defect rate balances both #1 and #4).
	escapedDefect := func() trustBalancingCell {
		c := trustBalancingCell{
			Name: "Escaped-defect rate",
			Note: "post-merge reverts/incidents per delivered MR — throughput must not buy bugs",
		}
		if rate, ok := ts.scalarVal(gaugeEscapedDefectRate); ts.has(gaugeEscapedDefectRate) {
			c.Value, c.ValueAvailable = fmtPercent(rate), ok
			if cnt, okc := ts.scalarVal(gaugeEscapedDefectCount); okc && cnt > 0 {
				c.Value += " (" + fmtCount(cnt) + ")"
			}
		} else {
			c.Value = trustDash
		}
		return c
	}

	// #1 ⭐ Trusted Throughput — the hero, balanced by escaped-defect rate.
	north := trustMetricCard{
		Num:         1,
		Name:        "Trusted Throughput",
		Definition:  "Upstream MRs merged that passed human review with ≤1 rework cycle, per week",
		Lane:        trustLaneLagging,
		IsNorthStar: true,
		Target:      "↑ trend; ≥ target rate set after baseline",
		Balancing:   escapedDefect(),
	}
	north.Value, north.ValueAvailable = fmtScalar(ts, gaugeTrustedThroughput, fmtPerWeek)
	v.NorthStar = north

	// --- Leading lane (§3.2): diff size, verbosity ratio, provenance, peel. ---

	// #6 Diff size — balanced by Requested-slice completeness (no gauge yet).
	diff := trustMetricCard{
		Num:        6,
		Name:       "Diff size",
		Definition: "Lines changed per trickle MR",
		Lane:       trustLaneLeading,
		Target:     "p50 small, p90 bounded",
		Balancing: trustBalancingCell{
			Name:         "Requested-slice completeness",
			Note:         "small must still fully answer the ask",
			AwaitingData: true,
			// No gauge in the shipped pipeline — adding it is follow-up on the
			// instrument bead that owns the compute layer.
			AwaitingBead: trustMetricsBeadID,
			Value:        trustDash,
		},
	}
	diff.Value, diff.ValueAvailable, diff.Secondary = fmtQuantilePair(ts, gaugeDiffSize, fmtLines)

	// #7 Verbosity ratio — balanced by provenance completeness (shared #8).
	verbosity := trustMetricCard{
		Num:        7,
		Name:       "Verbosity ratio",
		Definition: "Description prose ÷ diff substance (provenance excluded)",
		Lane:       trustLaneLeading,
		Target:     "below the terseness-gate threshold",
		Balancing: trustBalancingCellFromScalar(ts, "Provenance completeness",
			"terse ≠ missing the receipt", gaugeProvenance, fmtPercent),
	}
	verbosity.Value, verbosity.ValueAvailable, verbosity.Secondary = fmtQuantilePair(ts, gaugeVerbosityRatio, fmtRatio2)

	// #8 Provenance completeness — balanced by verbosity ratio (shared #7).
	provenance := trustMetricCard{
		Num:        8,
		Name:       "Provenance completeness",
		Definition: "% trickle MRs with a full structured provenance block + bead links",
		Lane:       trustLaneLeading,
		Target:     "100%",
		Balancing:  trustBalancingCellFromQuantile(ts, "Verbosity ratio", "provenance must stay structured, not balloon", gaugeVerbosityRatio, "p50", fmtRatio2),
	}
	provenance.Value, provenance.ValueAvailable = fmtScalar(ts, gaugeProvenance, fmtPercent)

	// #10 Peel latency — balanced by diff size / completeness.
	peel := trustMetricCard{
		Num:        10,
		Name:       "Peel latency",
		Definition: "Peel request (bead) → trickle MR opened",
		Lane:       trustLaneLeading,
		Target:     "↓; responsive",
		Balancing:  trustBalancingCellFromQuantile(ts, "Diff size", "fast peels must stay small and complete", gaugeDiffSize, "p50", fmtLines),
	}
	peel.Value, peel.ValueAvailable, peel.Secondary = fmtQuantilePair(ts, gaugePeelLatency, fmtDurationSeconds)

	v.Leading = []trustMetricCard{diff, verbosity, provenance, peel}

	// --- Lagging lane (§3.2): review effort, trust index, rework, TTM, load. --

	// #2 Review effort — balanced by diff size p50.
	effort := trustMetricCard{
		Num:        2,
		Name:       "Review effort per delivered MR",
		Definition: "Human review rounds to merge a trickle MR",
		Lane:       trustLaneLagging,
		Target:     "↓ vs. baseline",
		Balancing:  trustBalancingCellFromQuantile(ts, "Diff size", "can't cut review time by shipping trivia", gaugeDiffSize, "p50", fmtLines),
	}
	effort.Value, effort.ValueAvailable, effort.Secondary = fmtQuantilePair(ts, gaugeReviewEffort, fmtRounds)

	// #3 Trust index — survey-fed; balanced by approval-without-comment rate.
	trust := trustMetricCard{
		Num:        3,
		Name:       "Trust index",
		Definition: "Median reviewer-reported confidence at merge (1–5 survey)",
		Lane:       trustLaneLagging,
		Target:     "median ≥ 4, ↑ trend",
		Balancing:  trustBalancingCellFromScalar(ts, "Approval-without-comment rate", "high trust must be engaged, not rubber-stamped", gaugeApprovalNoComment, fmtPercent),
	}
	// The trust index reads only from merge-time survey responses; until any
	// arrive the gauge is a meaningless 0, so flag it "awaiting data" rather than
	// reporting a fake score. responses>0 means real input has landed.
	responses, _ := ts.scalarVal(gaugeTrustIndexResponses)
	if ts.has(gaugeTrustIndexMedian) && responses > 0 {
		trust.Value, trust.ValueAvailable = fmtScalar(ts, gaugeTrustIndexMedian, fmtScore)
		trust.Secondary = fmtCount(responses) + " responses"
	} else {
		trust.AwaitingData = true
		trust.AwaitingNote = "needs merge-time micro-survey responses"
		trust.AwaitingBead = trustSurveyBeadID
		trust.Value = trustDash
	}

	// #4 Rework cycles — balanced by escaped-defect rate (shared #1).
	rework := trustMetricCard{
		Num:        4,
		Name:       "Rework cycles",
		Definition: "Review rounds beyond the first before merge",
		Lane:       trustLaneLagging,
		Target:     "p50 = 1, p90 ≤ 2",
		Balancing:  escapedDefect(),
	}
	rework.Value, rework.ValueAvailable, rework.Secondary = fmtQuantilePair(ts, gaugeReworkCycles, fmtRounds)

	// #5 Time-to-merge — balanced by the trust index (fast merges stay trusted).
	ttm := trustMetricCard{
		Num:        5,
		Name:       "Time-to-merge",
		Definition: "Open → merged for a trickle MR",
		Lane:       trustLaneLagging,
		Target:     "↓; bounded",
	}
	if ts.has(gaugeTrustIndexMedian) && responses > 0 {
		ttm.Balancing = trustBalancingCellFromScalar(ts, "Trust index", "fast merges must stay trusted, not coerced", gaugeTrustIndexMedian, fmtScore)
	} else {
		ttm.Balancing = trustBalancingCell{
			Name:         "Trust index",
			Note:         "fast merges must stay trusted, not coerced",
			AwaitingData: true,
			AwaitingBead: trustSurveyBeadID,
			Value:        trustDash,
		}
	}
	ttm.Value, ttm.ValueAvailable, ttm.Secondary = fmtQuantilePair(ts, gaugeTimeToMerge, fmtDurationSeconds)

	// #9 Reviewer load — balanced by Trusted Throughput (shared #1).
	load := trustMetricCard{
		Num:        9,
		Name:       "Reviewer load",
		Definition: "Reviews per engineer per week (most-pinned reviewer)",
		Lane:       trustLaneLagging,
		Target:     "↓ and flatter (no single reviewer pinned)",
		Balancing:  trustBalancingCellFromScalar(ts, "Trusted Throughput", "don't cut fatigue by cutting delivery", gaugeTrustedThroughput, fmtPerWeek),
	}
	if maxV, ok := fmtScalar(ts, gaugeReviewerLoadMax, fmtPerWeek); ok {
		load.Value, load.ValueAvailable = maxV, true
		if meanV, okm := fmtScalar(ts, gaugeReviewerLoadMean, fmtPerWeek); okm {
			load.Secondary = "mean " + meanV
		}
	} else {
		load.Value = trustDash
	}

	v.Lagging = []trustMetricCard{effort, trust, rework, ttm, load}

	// Context strip.
	if wd, ok := ts.scalarVal(gaugeWindowDays); ok && wd > 0 {
		v.WindowDays = strconv.FormatFloat(wd, 'f', 0, 64)
	}
	if dc, ok := ts.scalarVal(gaugeDeliveredMRCount); ok {
		v.DeliveredMRCount = fmtCount(dc)
	}
	if tc, ok := ts.scalarVal(gaugeTotalMRCount); ok {
		v.TotalMRCount = fmtCount(tc)
	}
	if lc, ok := ts.scalarVal(gaugeLastCollect); ok && lc > 0 {
		v.LastCollectedAgo = fmtTimeAgo(time.Unix(int64(lc), 0), now)
	}

	return v
}

// handleTrust renders /trust — the R5 Trust dashboard. One server-side instant
// query against vmsingle; the page degrades to its skeleton + banner when the
// opt-in collector isn't publishing.
func (s *Server) handleTrust(w http.ResponseWriter, r *http.Request) {
	ts, _ := s.fetchTrustSeries(r.Context())
	view := buildTrustDashboard(ts, s.now())
	s.render(w, r, "trust.html", map[string]any{
		"Trust": view,
	})
}

// --- formatting helpers -----------------------------------------------------

// trustDash is the placeholder for an absent metric value. A real zero is shown
// as "0"; only a gauge missing from the result vector reads this em-dash.
const trustDash = "—"

const (
	trustLaneLeading = "leading"
	trustLaneLagging = "lagging"
)

// fmtScalar formats an un-labelled gauge through f, returning (trustDash, false)
// when the gauge is absent from the vector.
func fmtScalar(ts *trustSeries, name string, f func(float64) string) (string, bool) {
	if v, ok := ts.scalarVal(name); ok && ts.has(name) {
		return f(v), true
	}
	return trustDash, false
}

// fmtQuantilePair formats a distribution gauge's p50 as the headline and p90 as
// the secondary ("p90 <x>"). Returns (trustDash, false, "") when p50 is absent.
func fmtQuantilePair(ts *trustSeries, name string, f func(float64) string) (value string, ok bool, secondary string) {
	p50, has50 := ts.quantileVal(name, "p50")
	if !has50 {
		return trustDash, false, ""
	}
	value = f(p50)
	if p90, has90 := ts.quantileVal(name, "p90"); has90 {
		secondary = "p90 " + f(p90)
	}
	return value, true, secondary
}

// trustBalancingCellFromScalar builds a balancing cell from an un-labelled gauge.
func trustBalancingCellFromScalar(ts *trustSeries, name, note, gauge string, f func(float64) string) trustBalancingCell {
	c := trustBalancingCell{Name: name, Note: note}
	c.Value, c.ValueAvailable = fmtScalar(ts, gauge, f)
	return c
}

// trustBalancingCellFromQuantile builds a balancing cell from one quantile of a
// distribution gauge.
func trustBalancingCellFromQuantile(ts *trustSeries, name, note, gauge, q string, f func(float64) string) trustBalancingCell {
	c := trustBalancingCell{Name: name, Note: note}
	if v, ok := ts.quantileVal(gauge, q); ok {
		c.Value, c.ValueAvailable = f(v), true
	} else {
		c.Value = trustDash
	}
	return c
}

// fmtPerWeek renders a per-week rate with one decimal, e.g. "12.5 / wk".
func fmtPerWeek(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64) + " / wk"
}

// fmtPercent renders a 0–1 fraction as a whole-number percentage, e.g. "8%".
func fmtPercent(v float64) string {
	return strconv.FormatFloat(v*100, 'f', 0, 64) + "%"
}

// fmtScore renders a 1–5 survey score with one decimal, e.g. "4.2".
func fmtScore(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// fmtCount renders an integer-valued gauge without a decimal point.
func fmtCount(v float64) string {
	return strconv.FormatInt(int64(v+0.5), 10)
}

// fmtLines renders a diff-size value as whole lines, e.g. "42 lines".
func fmtLines(v float64) string {
	n := fmtCount(v)
	if n == "1" {
		return "1 line"
	}
	return n + " lines"
}

// fmtRounds renders a review-rounds value, e.g. "1 round" / "2 rounds". Rounds
// can be fractional (a p90 across MRs), so keep one decimal when not whole.
func fmtRounds(v float64) string {
	var n string
	if v == float64(int64(v)) {
		n = strconv.FormatInt(int64(v), 10)
	} else {
		n = strconv.FormatFloat(v, 'f', 1, 64)
	}
	if n == "1" {
		return "1 round"
	}
	return n + " rounds"
}

// fmtRatio2 renders a unitless ratio (verbosity) with two decimals, e.g. "0.42".
func fmtRatio2(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// fmtDurationSeconds renders a seconds value as a compact human duration, e.g.
// "0s", "45s", "12m", "3h 10m", "2d 4h". Mirrors fmtTimeAgo's granularity so the
// page reads consistently with the rest of the viewer.
func fmtDurationSeconds(secs float64) string {
	if secs <= 0 {
		return "0s"
	}
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		if h == 0 {
			return strconv.Itoa(days) + "d"
		}
		return strconv.Itoa(days) + "d " + strconv.Itoa(h) + "h"
	}
}
