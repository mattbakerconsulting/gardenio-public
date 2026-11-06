package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// charts_uniqueness.go — DAU/WAU/MAU query-time uniqueness panel.
//
// Customer-1 user-engagement instrumentation, plan §1.2 / §3.5
// (gardenio/docs/research/customer-1-telemetry-plan.md), epic ka-0000007629,
// task ka-0000006905.
//
// Distinct active-operator counts (DAU/WAU/MAU) are computed QUERY-TIME from
// the backbone counter gardenio_operator_activity_total, with NO HyperLogLog
// and NO recording rules. The whole approach rests on one PromQL idiom:
//
//	count( count by (operator_id) ( last_over_time(gardenio_operator_activity_total[<window>]) ) )
//
// Why last_over_time and not increase: distinct-membership only needs "did
// this operator's series have ANY sample in the window", which last_over_time
// answers correctly. increase() would (a) drop a series with a single sample
// in the window and (b) misattribute a counter reset (pod restart) inside the
// window — both DROP a genuinely-active operator from the distinct set and
// UNDERCOUNT. This is validated against a real VictoriaMetrics fixture in
// charts_uniqueness_vm_test.go and documented in
// gardenio/docs/research/dau-wau-mau-benchmark.md.
//
// "Active" (plan §1.2, operator decision gate ka-0000001248 sub-question b) is
// any operator who authored a mutation / spawn / session — i.e. any series of
// the backbone counter. The unattributed bucket operator_id="0" is a real,
// counted distinct operator (it keeps DAU/WAU/MAU denominators honest); it is
// not filtered out here.

// uniquenessMetric is the backbone counter the windows aggregate over. Kept as
// a constant so the three window queries cannot drift apart.
const uniquenessMetric = "gardenio_operator_activity_total"

// uniquenessWindow describes one DAU/WAU/MAU panel: a stable key, the PromQL
// range-vector window, and the human label clients render.
type uniquenessWindow struct {
	Key    string // "dau" / "wau" / "mau"
	Label  string // "DAU" / "WAU" / "MAU"
	Window string // PromQL range selector: "1d" / "7d" / "30d"
}

// uniquenessWindows is the fixed v1 panel set. The plan pins exactly these
// three windows (§1.2); adding finer windows is a catalog change, not a code
// change, once the full chart catalog lands.
var uniquenessWindows = []uniquenessWindow{
	{Key: "dau", Label: "DAU", Window: "1d"},
	{Key: "wau", Label: "WAU", Window: "7d"},
	{Key: "mau", Label: "MAU", Window: "30d"},
}

// uniquenessQuery returns the exact PromQL the panel evaluates for a window.
// Centralised (and unit-tested) so the last_over_time membership semantics are
// asserted in one place and the three windows stay structurally identical.
func uniquenessQuery(window string) string {
	return fmt.Sprintf(
		"count(count by (operator_id)(last_over_time(%s[%s])))",
		uniquenessMetric, window,
	)
}

// UniquenessPanel is one rendered DAU/WAU/MAU number.
type UniquenessPanel struct {
	Key   string `json:"key"`   // dau / wau / mau
	Label string `json:"label"` // DAU / WAU / MAU
	// Window is the PromQL range used, echoed for client display / debugging.
	Window string `json:"window"`
	// Count is the distinct active-operator count. Always present (0 when no
	// operators were active in the window).
	Count int64 `json:"count"`
}

// UniquenessResponse is the body of GET /v1/charts/uniqueness.
type UniquenessResponse struct {
	// EvaluatedAt is the instant (UTC, RFC3339) the windows were evaluated at.
	EvaluatedAt string            `json:"evaluated_at"`
	Panels      []UniquenessPanel `json:"panels"`
}

// uniquenessCache is the L1 step-aligned cache for the uniqueness panels. The
// counts move slowly (a new distinct operator per window is a rare event), so
// a short TTL collapses a dashboard's repeated polls and concurrent viewers
// into one VictoriaMetrics round-trip per TTL — the singleflight-with-TTL
// idiom the arch doc (§10.2) points at fleetCollector for. Mutex-guarded; the
// cached payload is immutable once stored.
type uniquenessCache struct {
	ttl time.Duration

	mu       sync.Mutex
	fetched  time.Time
	snapshot *UniquenessResponse
}

// uniquenessCacheTTL is the default L1 TTL. 30s is well below the cadence at
// which a distinct-operator count meaningfully changes while still keeping the
// panel feeling live.
const uniquenessCacheTTL = 30 * time.Second

// SetVMClient wires the chart backend's VictoriaMetrics client and enables the
// /v1/charts/uniqueness endpoint. Passing nil (the default) leaves the
// endpoint returning 503 "charts backend not configured" — kbeads deployments
// without a vmselect endpoint simply don't serve charts.
func (s *BeadsServer) SetVMClient(c *VMClient) {
	s.vmClient = c
	if c != nil && s.uniqCache == nil {
		s.uniqCache = &uniquenessCache{ttl: uniquenessCacheTTL}
	}
}

// handleChartsUniqueness handles GET /v1/charts/uniqueness.
//
// Response: { "evaluated_at": "...", "panels": [ {key,label,window,count}, ... ] }
//
// All three windows are evaluated against the same instant so DAU ≤ WAU ≤ MAU
// holds for a single response. Results are cached for uniquenessCacheTTL.
func (s *BeadsServer) handleChartsUniqueness(w http.ResponseWriter, r *http.Request) {
	if s.vmClient == nil {
		writeError(w, http.StatusServiceUnavailable, "charts backend not configured (no VictoriaMetrics endpoint)")
		return
	}

	if cached := s.uniqCache.get(); cached != nil {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	resp, err := s.computeUniqueness(r.Context(), time.Now().UTC())
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsUniqueness", err, "uniqueness query failed")
		return
	}

	s.uniqCache.put(resp)
	writeJSON(w, http.StatusOK, resp)
}

// computeUniqueness evaluates all three windows at the given instant. It is
// separated from the handler so the VM-backed correctness test can drive it
// directly with a deterministic evaluation time.
func (s *BeadsServer) computeUniqueness(ctx context.Context, at time.Time) (*UniquenessResponse, error) {
	panels := make([]UniquenessPanel, 0, len(uniquenessWindows))
	for _, win := range uniquenessWindows {
		q := uniquenessQuery(win.Window)
		val, ok, err := s.vmClient.QueryScalar(ctx, q, at)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): %w", win.Label, win.Window, err)
		}
		var count int64
		if ok {
			count = int64(val)
		}
		panels = append(panels, UniquenessPanel{
			Key:    win.Key,
			Label:  win.Label,
			Window: win.Window,
			Count:  count,
		})
	}
	return &UniquenessResponse{
		EvaluatedAt: at.Format(time.RFC3339),
		Panels:      panels,
	}, nil
}

// get returns the cached response when still fresh, else nil.
func (c *uniquenessCache) get() *UniquenessResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshot == nil || time.Since(c.fetched) > c.ttl {
		return nil
	}
	return c.snapshot
}

// put stores resp as the current snapshot.
func (c *uniquenessCache) put(resp *UniquenessResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = resp
	c.fetched = time.Now()
}
