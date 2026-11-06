package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"gardenio/kbeads/internal/store"
)

// charts_engagement.go — operator-engagement panels for the in-house chart
// backend (ka-0000004824, customer-1 telemetry plan §1.1 / §5).
//
// Three read-only instant-query panels share one privacy-critical property:
// the backbone metrics carry ONLY the opaque operator_id label, and the
// operator_id -> display/email join happens HERE, server-side, via
// store.ResolveOperators — never as a metric label (plan §4 allow-list). The
// PromQL strings in this file therefore NEVER reference email, slack_user_id,
// or display; they group by operator_id / surface / model and the handler
// decorates the result with resolved identities after the fact.
//
//   - activity : sum by(surface)(...) surface-mix + top operators by activity
//   - spend    : sum by(operator_id)(gardenio_operator_cost_usd_total) per human
//   - sessions : histogram_quantile over gardenio_operator_session_duration_seconds
//                per human — per-operator p50/p90/p99 session length (ka-0000005351)
//
// DAU/WAU/MAU live in charts_uniqueness.go and are intentionally not duplicated
// here. The security overview lands as a follow-on task under the child epic
// (see the bead tree filed for ka-0000004824).

// Backbone metric names. Kept as constants so the engagement queries and the
// privacy allow-list (gardenio/controller/internal/obspii/allowlist.go) cannot
// drift apart, and a metric rename is a one-line change here.
const (
	activityMetric = "gardenio_operator_activity_total"
	spendMetric    = "gardenio_operator_cost_usd_total"
	// sessionDurationMetric is the per-operator session-length histogram (plan
	// §5, buckets 30s…1d). Only the `_bucket` series is queried — quantiles are
	// computed query-time with histogram_quantile so the panel needs no
	// precomputed percentile. The histogram carries operator_id/project/role
	// labels; identity is still resolved server-side, never off a label.
	sessionDurationMetric = "gardenio_operator_session_duration_seconds"
)

// sessionQuantiles are the percentiles the session-duration panel reports per
// operator. Fixed (not caller-supplied) so a request cannot smuggle an
// arbitrary float into the PromQL we evaluate, and so every operator row shares
// the same quantile shape the UI can render as a fixed set of columns.
var sessionQuantiles = []struct {
	// Key is the JSON/label name (p50/p90/p99); Phi is the histogram_quantile φ.
	Key string
	Phi float64
}{
	{"p50", 0.50},
	{"p90", 0.90},
	{"p99", 0.99},
}

// unattributedOperatorID is the reserved "no human behind this sample" bucket
// (plan §3.2). The session-close push and manual/S3 resume paths emit
// operator_id="0" rather than dropping the sample; the resolver has no
// operators(id=0) row, so the handler renders it with a fixed, non-PII label
// instead of "unknown operator 0".
const unattributedOperatorID = 0

// unattributedDisplay is what operator_id="0" renders as. A stable, non-PII
// string so the unattributed bucket is legible on a dashboard without leaking
// that it is a sentinel id.
const unattributedDisplay = "unattributed"

// engagementWindow is the bounded set of look-back windows the engagement
// panels accept via ?window=. Bounded (not free-form) so a caller cannot smuggle
// an unbounded range selector into the PromQL we evaluate, and so the panel set
// matches the uniqueness windows operators already reason about.
var engagementWindows = map[string]string{
	"1d":  "1d",
	"7d":  "7d",
	"30d": "30d",
}

// defaultEngagementWindow is used when ?window= is absent or unrecognized.
const defaultEngagementWindow = "7d"

// engagementTopN caps how many operators a per-operator panel resolves and
// returns. Bounds both the operators-table join and the response size; the
// long tail past the cap is summarized into an "others" row by the caller so
// totals stay honest.
const engagementTopN = 25

// resolveWindow maps a raw ?window= value onto the allow-listed PromQL range
// selector, falling back to the default for absent / unknown input.
func resolveWindow(raw string) string {
	if w, ok := engagementWindows[raw]; ok {
		return w
	}
	return defaultEngagementWindow
}

// SurfaceCount is one row of the activity-by-surface panel: a bounded surface
// enum and the number of activity samples attributed to it in the window.
type SurfaceCount struct {
	Surface string  `json:"surface"`
	Count   float64 `json:"count"`
}

// OperatorStat is one resolved per-operator row shared by the activity and
// spend panels. Identity is resolved SERVER-SIDE; Email is present only so the
// web/iOS UX can show it on an authenticated drill-down — it is never a metric
// label. Forgotten flags a right-to-be-forgotten redaction so the UI can badge
// it; in that case Display is empty and Email is the redaction sentinel.
type OperatorStat struct {
	OperatorID int64   `json:"operator_id"`
	Display    string  `json:"display"`
	Email      string  `json:"email"`
	Slug       string  `json:"slug,omitempty"`
	Forgotten  bool    `json:"forgotten,omitempty"`
	Value      float64 `json:"value"`
}

// EngagementActivityResponse is the body of GET /v1/charts/engagement/activity.
type EngagementActivityResponse struct {
	EvaluatedAt string `json:"evaluated_at"`
	Window      string `json:"window"`
	// BySurface is the surface mix (slack/web/ios/cli/kbeads), descending by
	// count. Always present; empty when no activity in the window.
	BySurface []SurfaceCount `json:"by_surface"`
	// TopOperators is the top-N most active operators, identities resolved
	// server-side, descending by activity count.
	TopOperators []OperatorStat `json:"top_operators"`
}

// EngagementSpendResponse is the body of GET /v1/charts/engagement/spend.
type EngagementSpendResponse struct {
	EvaluatedAt string `json:"evaluated_at"`
	// TopOperators is the top-N operators by cumulative spend (USD), identities
	// resolved server-side, descending by value. Spend is a monotonic counter,
	// so this is lifetime-to-date, not windowed.
	TopOperators []OperatorStat `json:"top_operators"`
	// OthersUSD is the summed spend of every operator past the top-N cap, so a
	// dashboard total stays honest without enumerating the long tail.
	OthersUSD float64 `json:"others_usd"`
}

// OperatorSessionStat is one resolved per-operator row of the session-duration
// panel: the same server-side-resolved identity carried by OperatorStat, plus
// the operator's session-length quantiles (seconds) over the window. Quantiles
// keys the φ name (p50/p90/p99) to the computed duration. SessionCount is the
// number of session samples the operator contributed (the histogram _count),
// surfaced so the UI can show how much data a quantile rests on.
type OperatorSessionStat struct {
	OperatorID   int64              `json:"operator_id"`
	Display      string             `json:"display"`
	Email        string             `json:"email"`
	Slug         string             `json:"slug,omitempty"`
	Forgotten    bool               `json:"forgotten,omitempty"`
	Quantiles    map[string]float64 `json:"quantiles"`
	SessionCount float64            `json:"session_count"`
}

// EngagementSessionsResponse is the body of GET
// /v1/charts/engagement/sessions.
type EngagementSessionsResponse struct {
	EvaluatedAt string `json:"evaluated_at"`
	Window      string `json:"window"`
	// Quantiles echoes which percentiles every TopOperators row carries, so the
	// UI can render a fixed column set without inferring it from the data.
	Quantiles []string `json:"quantiles"`
	// TopOperators is the top-N operators by session count, identities resolved
	// server-side, descending by count. Each row carries p50/p90/p99 session
	// duration (seconds) over the window.
	TopOperators []OperatorSessionStat `json:"top_operators"`
}

// sessionCountByOperatorQuery returns the PromQL ranking operators by how many
// session samples they contributed in the window — the histogram's `_count`
// series. Used to pick the top-N before computing the (more expensive)
// per-operator quantiles, and grouped by the opaque operator_id only.
func sessionCountByOperatorQuery(window string) string {
	return fmt.Sprintf(
		"sum by (operator_id)(increase(%s_count[%s]))",
		sessionDurationMetric, window,
	)
}

// sessionQuantileByOperatorQuery returns the PromQL for one quantile of the
// per-operator session-duration histogram over the window. histogram_quantile
// is evaluated query-time over the `_bucket` series, grouped by operator_id and
// the synthetic `le` bucket label only — no PII label is referenced.
func sessionQuantileByOperatorQuery(phi float64, window string) string {
	return fmt.Sprintf(
		"histogram_quantile(%s, sum by (operator_id, le)(increase(%s_bucket[%s])))",
		strconv.FormatFloat(phi, 'g', -1, 64), sessionDurationMetric, window,
	)
}

// activityBySurfaceQuery returns the PromQL for the surface-mix panel:
// distinct-membership-safe count of activity per surface over the window.
// last_over_time (not increase) for the same reason the uniqueness panel uses
// it — a counter reset inside the window must not zero a surface out.
func activityBySurfaceQuery(window string) string {
	return fmt.Sprintf(
		"sum by (surface)(last_over_time(%s[%s]))",
		activityMetric, window,
	)
}

// activityByOperatorQuery returns the PromQL for the top-operators panel:
// per-operator activity total over the window, grouped by the opaque
// operator_id only.
func activityByOperatorQuery(window string) string {
	return fmt.Sprintf(
		"sum by (operator_id)(last_over_time(%s[%s]))",
		activityMetric, window,
	)
}

// spendByOperatorQuery returns the PromQL for the per-operator spend panel:
// cumulative USD per operator. No window — spend is lifetime-to-date.
func spendByOperatorQuery() string {
	return fmt.Sprintf("sum by (operator_id)(%s)", spendMetric)
}

// operatorRollupValue is the `rollup` enum value on the total-cost catalog chart
// that selects the operator dimension. It is also the synthetic series-label key
// computeOperatorSeries stamps onto each returned series after the server-side
// operator_id -> name resolve, so the catalog reshaper (which reads
// def.SeriesLabel == "operator" after withRollupSeries) keys on the resolved
// human name — never on operator_id or any PII label.
const operatorRollupValue = "operator"

// operatorSpendFlowQuery returns the PromQL for the operator branch of the
// total-cost catalog chart: per-bucket real-bill USD flow per operator, grouped
// by the OPAQUE operator_id only. gardenio_operator_cost_usd_total is a monotonic
// counter (incremented once per session at close, metrics/spend.go), so
// increase() over the range step is the per-bucket flow — mirroring the
// stacked-bar semantics the four label dims get from realBillTagFlowOr, so the
// operator rollup renders the same kind of chart (bars = spend in that bucket),
// not a cumulative line.
//
// Like the sibling engagement panels (activityByOperatorQuery /
// spendByOperatorQuery), the PromQL NEVER references email / display / slug: it
// groups on operator_id, and the id -> human join happens SERVER-SIDE in
// computeOperatorSeries via store.ResolveOperators. No {{.Selector}} is applied
// — the operator branch is fleet-wide per operator, exactly as the engagement
// spend panel ships (the operator metric carries only operator_id/project/model;
// the chart's agent/role filters are not labels on it).
func operatorSpendFlowQuery(step string) string {
	return fmt.Sprintf("sum by (operator_id)(increase(%s[%s]))", spendMetric, step)
}

// operatorSeriesLabel maps an opaque operator_id onto the human-readable,
// non-PII-safe series label the total-cost operator rollup renders. It reuses
// the exact contract the engagement spend panel resolves under: operator_id=0 is
// the reserved "unattributed" bucket; a right-to-be-forgotten operator (or one
// with an empty resolved identity) redacts to the opaque "operator <id>" so a
// deleted human's name can never re-surface; an id absent from the operators
// table falls back to the same opaque form. Only a live, resolved operator gets
// a real name (display, or slug when display is empty) — and even then that name
// lives ONLY in the render payload, never in a metric label or PromQL selector.
func operatorSeriesLabel(id int64, resolved map[int64]store.ResolvedOperator) string {
	if id == unattributedOperatorID {
		return unattributedDisplay
	}
	if ro, ok := resolved[id]; ok && !ro.Forgotten {
		if ro.Display != "" {
			return ro.Display
		}
		if ro.Slug != "" {
			return ro.Slug
		}
	}
	// Unresolved, forgotten, or empty identity: opaque, non-PII fallback.
	return fmt.Sprintf("operator %d", id)
}

// computeOperatorSeries is the render-path branch for the total-cost chart's
// operator rollup. It runs the operator-metric range query (grouped by the
// opaque operator_id), resolves every id -> human display SERVER-SIDE in one
// batched store round trip (store.ResolveOperators — the same single join point
// the engagement spend panel uses), and returns VM series RE-KEYED under the
// synthetic "operator" label carrying the resolved name. Series that resolve to
// the same label (e.g. multiple forgotten operators, or unresolved ids that
// never collide in practice) are merged point-wise so the stacked bar has one
// segment per distinct operator.
//
// This is the whole privacy contract of approach A: the operator_id -> name join
// is confined to THIS function, after the query and before the reshape. No name
// is ever emitted into a metric label, recording rule, or selector; the four
// label dims are untouched (they never reach this branch).
func (s *BeadsServer) computeOperatorSeries(ctx context.Context, req *chartRequest) ([]VMSeries, error) {
	series, err := s.vmClient.QueryRange(ctx, operatorSpendFlowQuery(promDuration(req.Step)), req.Since, req.Until, req.Step)
	if err != nil {
		return nil, err
	}

	// Collect the distinct operator_ids present across the returned series so the
	// resolve is a single batched lookup bounded by operator cardinality.
	idSet := make(map[int64]struct{}, len(series))
	for _, ser := range series {
		idSet[parseOperatorID(ser.Metric)] = struct{}{}
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	resolved, err := s.store.ResolveOperators(ctx, ids)
	if err != nil {
		return nil, err
	}

	return relabelOperatorSeries(series, resolved), nil
}

// relabelOperatorSeries re-keys operator_id-labeled VM series onto the resolved
// human name under the synthetic "operator" label, merging any series that
// resolve to the same label by summing values at matching timestamps. The kept
// series share the range query's step-aligned time grid, so the merge is a plain
// per-timestamp accumulation. Output order is deterministic (first-seen label
// order) so the downstream etag stays stable across requests.
func relabelOperatorSeries(series []VMSeries, resolved map[int64]store.ResolvedOperator) []VMSeries {
	type merged struct {
		order []int64
		byTS  map[int64]float64
	}
	byLabel := make(map[string]*merged)
	labelOrder := make([]string, 0, len(series))
	for _, ser := range series {
		label := operatorSeriesLabel(parseOperatorID(ser.Metric), resolved)
		m, ok := byLabel[label]
		if !ok {
			m = &merged{byTS: make(map[int64]float64, len(ser.Points))}
			byLabel[label] = m
			labelOrder = append(labelOrder, label)
		}
		for _, p := range ser.Points {
			ts := p.T.Unix()
			if _, seen := m.byTS[ts]; !seen {
				m.order = append(m.order, ts)
			}
			m.byTS[ts] += p.Value
		}
	}

	out := make([]VMSeries, 0, len(byLabel))
	for _, label := range labelOrder {
		m := byLabel[label]
		sort.Slice(m.order, func(i, j int) bool { return m.order[i] < m.order[j] })
		pts := make([]VMPoint, 0, len(m.order))
		for _, ts := range m.order {
			pts = append(pts, VMPoint{T: time.Unix(ts, 0).UTC(), Value: m.byTS[ts]})
		}
		out = append(out, VMSeries{Metric: map[string]string{operatorRollupValue: label}, Points: pts})
	}
	return out
}

// handleChartsEngagementActivity handles GET /v1/charts/engagement/activity.
//
// Query params: ?window=1d|7d|30d (default 7d).
func (s *BeadsServer) handleChartsEngagementActivity(w http.ResponseWriter, r *http.Request) {
	if s.vmClient == nil {
		writeError(w, http.StatusServiceUnavailable, "charts backend not configured (no VictoriaMetrics endpoint)")
		return
	}
	window := resolveWindow(r.URL.Query().Get("window"))
	at := time.Now().UTC()

	surfaces, err := s.vmClient.QueryVector(r.Context(), activityBySurfaceQuery(window), at)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsEngagementActivity", err, "activity-by-surface query failed")
		return
	}
	operators, err := s.vmClient.QueryVector(r.Context(), activityByOperatorQuery(window), at)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsEngagementActivity", err, "activity-by-operator query failed")
		return
	}

	bySurface := make([]SurfaceCount, 0, len(surfaces))
	for _, sm := range surfaces {
		bySurface = append(bySurface, SurfaceCount{Surface: labelOrUnknown(sm.Metric, "surface"), Count: sm.Value})
	}
	sort.Slice(bySurface, func(i, j int) bool { return bySurface[i].Count > bySurface[j].Count })

	top, _, err := s.resolveOperatorStats(r.Context(), operators, engagementTopN)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsEngagementActivity", err, "operator identity resolution failed")
		return
	}

	writeJSON(w, http.StatusOK, EngagementActivityResponse{
		EvaluatedAt:  at.Format(time.RFC3339),
		Window:       window,
		BySurface:    bySurface,
		TopOperators: top,
	})
}

// handleChartsEngagementSpend handles GET /v1/charts/engagement/spend.
func (s *BeadsServer) handleChartsEngagementSpend(w http.ResponseWriter, r *http.Request) {
	if s.vmClient == nil {
		writeError(w, http.StatusServiceUnavailable, "charts backend not configured (no VictoriaMetrics endpoint)")
		return
	}
	at := time.Now().UTC()
	samples, err := s.vmClient.QueryVector(r.Context(), spendByOperatorQuery(), at)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsEngagementSpend", err, "spend-by-operator query failed")
		return
	}
	top, others, err := s.resolveOperatorStats(r.Context(), samples, engagementTopN)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsEngagementSpend", err, "operator identity resolution failed")
		return
	}
	writeJSON(w, http.StatusOK, EngagementSpendResponse{
		EvaluatedAt:  at.Format(time.RFC3339),
		TopOperators: top,
		OthersUSD:    others,
	})
}

// handleChartsEngagementSessions handles GET /v1/charts/engagement/sessions.
//
// Per-operator session-duration percentiles (p50/p90/p99) over the window.
// Operators are ranked by session count, top-N capped, and identities resolved
// SERVER-SIDE — the same privacy contract as activity + spend. The φ values are
// fixed (sessionQuantiles), never caller-supplied.
//
// Query params: ?window=1d|7d|30d (default 7d).
func (s *BeadsServer) handleChartsEngagementSessions(w http.ResponseWriter, r *http.Request) {
	if s.vmClient == nil {
		writeError(w, http.StatusServiceUnavailable, "charts backend not configured (no VictoriaMetrics endpoint)")
		return
	}
	window := resolveWindow(r.URL.Query().Get("window"))
	at := time.Now().UTC()

	// 1. Rank operators by session count and pick the top-N. A single operator
	//    yields one _count series (already summed by the PromQL grouping); the
	//    count both orders the panel and tells the UI how much data each
	//    quantile rests on.
	counts, err := s.vmClient.QueryVector(r.Context(), sessionCountByOperatorQuery(window), at)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsEngagementSessions", err, "session-count-by-operator query failed")
		return
	}
	countByID := make(map[int64]float64, len(counts))
	for _, sm := range counts {
		countByID[parseOperatorID(sm.Metric)] += sm.Value
	}
	keep := topOperatorIDs(countByID, engagementTopN)

	// 2. One instant query per quantile, each grouped by operator_id only. The
	//    φ set is small and fixed, so a handful of queries is cheaper and
	//    simpler than a single multi-quantile query to disentangle.
	quantileByID := make(map[int64]map[string]float64, len(keep))
	wanted := make(map[int64]struct{}, len(keep))
	for _, id := range keep {
		wanted[id] = struct{}{}
		quantileByID[id] = make(map[string]float64, len(sessionQuantiles))
	}
	for _, q := range sessionQuantiles {
		samples, err := s.vmClient.QueryVector(r.Context(), sessionQuantileByOperatorQuery(q.Phi, window), at)
		if err != nil {
			writeServerError(r.Context(), w, "handleChartsEngagementSessions", err, "session-quantile query failed")
			return
		}
		for _, sm := range samples {
			id := parseOperatorID(sm.Metric)
			if _, ok := wanted[id]; !ok {
				continue // outside the top-N — don't resolve or report it
			}
			// histogram_quantile emits NaN for an empty/degenerate bucket set;
			// render it as a 0 so the JSON stays valid and the UI shows "no
			// data" rather than a NaN that fails to marshal.
			v := sm.Value
			if math.IsNaN(v) || math.IsInf(v, 0) {
				v = 0
			}
			quantileByID[id][q.Key] = v
		}
	}

	// 3. Resolve only the operators we surface — server-side, batched, the same
	//    single join point as resolveOperatorStats.
	resolved, err := s.store.ResolveOperators(r.Context(), keep)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsEngagementSessions", err, "operator identity resolution failed")
		return
	}

	qKeys := make([]string, 0, len(sessionQuantiles))
	for _, q := range sessionQuantiles {
		qKeys = append(qKeys, q.Key)
	}
	top := make([]OperatorSessionStat, 0, len(keep))
	for _, id := range keep {
		// Ensure every reported quantile key is present (0 when the query
		// returned no series for this operator) so the UI's columns are stable.
		qs := quantileByID[id]
		for _, q := range sessionQuantiles {
			if _, ok := qs[q.Key]; !ok {
				qs[q.Key] = 0
			}
		}
		stat := OperatorSessionStat{OperatorID: id, Quantiles: qs, SessionCount: countByID[id]}
		if id == unattributedOperatorID {
			stat.Display = unattributedDisplay
		} else if ro, ok := resolved[id]; ok {
			stat.Display = ro.Display
			stat.Email = ro.Email
			stat.Slug = ro.Slug
			stat.Forgotten = ro.Forgotten
		} else {
			stat.Display = fmt.Sprintf("operator %d", id)
		}
		top = append(top, stat)
	}

	writeJSON(w, http.StatusOK, EngagementSessionsResponse{
		EvaluatedAt:  at.Format(time.RFC3339),
		Window:       window,
		Quantiles:    qKeys,
		TopOperators: top,
	})
}

// topOperatorIDs returns the operator ids with the highest value, capped at
// topN, in deterministic order (value desc, then id asc as a stable tiebreak).
// Shared by the session panel's count-ranking; the long tail past the cap is
// simply dropped (the session panel reports per-operator quantiles, which have
// no meaningful "others" rollup the way a summable count or spend does).
func topOperatorIDs(byID map[int64]float64, topN int) []int64 {
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if byID[ids[i]] != byID[ids[j]] {
			return byID[ids[i]] > byID[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if topN > 0 && len(ids) > topN {
		ids = ids[:topN]
	}
	return ids
}

// resolveOperatorStats turns operator_id-keyed VM samples into the top-N
// resolved OperatorStat rows plus the summed value of the long tail past the
// cap. Identity resolution is one batched store round trip
// (store.ResolveOperators) — the single server-side join point. Samples whose
// operator_id label is missing or non-numeric are folded into the unattributed
// bucket rather than dropped, so a malformed label never silently loses spend
// or activity.
func (s *BeadsServer) resolveOperatorStats(ctx context.Context, samples []VMSample, topN int) (top []OperatorStat, others float64, err error) {
	// Collapse to value-by-operator-id first (a single operator can have
	// multiple series — e.g. activity across surfaces — already summed by the
	// PromQL grouping, but fold defensively in case the query shape changes).
	byID := make(map[int64]float64, len(samples))
	for _, sm := range samples {
		id := parseOperatorID(sm.Metric)
		byID[id] += sm.Value
	}
	if len(byID) == 0 {
		return []OperatorStat{}, 0, nil
	}

	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	// Deterministic order before the cap: value desc, then id asc as a stable
	// tiebreak so equal-value operators don't shuffle between requests.
	sort.Slice(ids, func(i, j int) bool {
		if byID[ids[i]] != byID[ids[j]] {
			return byID[ids[i]] > byID[ids[j]]
		}
		return ids[i] < ids[j]
	})

	keep := ids
	if topN > 0 && len(ids) > topN {
		keep = ids[:topN]
		for _, id := range ids[topN:] {
			others += byID[id]
		}
	}

	// Resolve only the operators we actually surface — server-side, batched.
	resolved, err := s.store.ResolveOperators(ctx, keep)
	if err != nil {
		return nil, 0, err
	}

	top = make([]OperatorStat, 0, len(keep))
	for _, id := range keep {
		stat := OperatorStat{OperatorID: id, Value: byID[id]}
		if id == unattributedOperatorID {
			stat.Display = unattributedDisplay
		} else if ro, ok := resolved[id]; ok {
			applyResolved(&stat, ro)
		} else {
			// Operator id present in metrics but absent from the operators
			// table (e.g. a pruned/merged row). Non-PII fallback, never a label.
			stat.Display = fmt.Sprintf("operator %d", id)
		}
		top = append(top, stat)
	}
	return top, others, nil
}

// applyResolved copies a server-side-resolved identity onto a stat, preserving
// the right-to-be-forgotten redaction the store already applied.
func applyResolved(stat *OperatorStat, ro store.ResolvedOperator) {
	stat.Display = ro.Display
	stat.Email = ro.Email
	stat.Slug = ro.Slug
	stat.Forgotten = ro.Forgotten
}

// parseOperatorID reads the operator_id label off a sample, returning the
// unattributed bucket (0) when the label is absent or non-numeric.
func parseOperatorID(metric map[string]string) int64 {
	raw, ok := metric["operator_id"]
	if !ok {
		return unattributedOperatorID
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return unattributedOperatorID
	}
	return id
}

// labelOrUnknown returns metric[label], or "unknown" when the label is absent
// or empty — so a surface-mix row is always legible.
func labelOrUnknown(metric map[string]string, label string) string {
	if v := metric[label]; v != "" {
		return v
	}
	return "unknown"
}

// quantile returns the φ-quantile (0..1) of xs using nearest-rank on a sorted
// copy. Precise (not histogram-bucketed) since the caller holds the exact
// samples, so no +Inf-pinned p95 is possible. Returns 0 for an empty input.
func quantile(xs []float64, phi float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)
	if phi <= 0 {
		return sorted[0]
	}
	if phi >= 1 {
		return sorted[len(sorted)-1]
	}
	// Nearest-rank: rank = ceil(phi * N), 1-based.
	rank := int(phi*float64(len(sorted)) + 0.9999999)
	rank = max(rank, 1)
	rank = min(rank, len(sorted))
	return sorted[rank-1]
}
