package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// --- VM instant-query parsing ----------------------------------------------

func TestParseVMInstantScalar(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		want   float64
		wantOK bool
	}{
		{"scalar value", `{"status":"success","data":{"resultType":"vector","result":[{"value":[1700000000,"42.5"]}]}}`, 42.5, true},
		{"real zero kept", `{"status":"success","data":{"resultType":"vector","result":[{"value":[1700000000,"0"]}]}}`, 0, true},
		{"empty vector → absent", `{"status":"success","data":{"resultType":"vector","result":[]}}`, 0, false},
		{"error status", `{"status":"error","data":{"result":[]}}`, 0, false},
		{"NaN rejected", `{"status":"success","data":{"result":[{"value":[1,"NaN"]}]}}`, 0, false},
		{"garbage", `not json`, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseVMInstantScalar([]byte(tt.body))
			if ok != tt.wantOK || (ok && math.Abs(got-tt.want) > 1e-9) {
				t.Fatalf("parseVMInstantScalar = (%v,%v), want (%v,%v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// vmStub serves /api/v1/query, returning a value keyed by an exact-or-substring
// match on the query expression, so a test can pin each PromQL side's result.
type vmStub struct {
	// byContains maps a substring of the PromQL query → scalar value. First match
	// wins (checked in insertion order via keys slice). A query matching nothing
	// returns an empty vector (→ metric absent).
	rules []vmRule

	// mu guards seen. seen records every query the stub served, so a test can
	// assert on the exact PromQL a pair emitted (not just its result).
	mu   sync.Mutex
	seen []string
}

type vmRule struct {
	contains string
	value    float64
	absent   bool // return empty vector even on match (simulate metric not present)
}

// queryFor returns the most recent served query containing sub, failing the
// test if none matched.
func (s *vmStub) queryFor(t *testing.T, sub string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range slices.Backward(s.seen) {
		if strings.Contains(q, sub) {
			return q
		}
	}
	t.Fatalf("no served query contained %q; served: %v", sub, s.seen)
	return ""
}

func (s *vmStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != vmInstantQueryPath {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("query")
		s.mu.Lock()
		s.seen = append(s.seen, q)
		s.mu.Unlock()
		for _, rule := range s.rules {
			if strings.Contains(q, rule.contains) {
				if rule.absent {
					_ = json.NewEncoder(w).Encode(vmEnvelope(nil))
					return
				}
				_ = json.NewEncoder(w).Encode(vmEnvelope(&rule.value))
				return
			}
		}
		_ = json.NewEncoder(w).Encode(vmEnvelope(nil))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func vmEnvelope(v *float64) map[string]any {
	result := []any{}
	if v != nil {
		result = append(result, map[string]any{"value": []any{1700000000, strconv.FormatFloat(*v, 'f', -1, 64)}})
	}
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}}
}

// --- daemon stub ------------------------------------------------------------

// xcheckDaemon is a minimal /v1/beads stub honoring ?type, ?status, ?limit,
// ?offset (for the pager) plus ?field predicates (for the gardener BFS). It does
// not honor updated_after — tests keep their populations small and recent, and
// P9 filters close_reason in-process anyway.
type xcheckDaemon struct {
	beads []map[string]any
}

func (d *xcheckDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			d.serveList(w, r.URL.Query())
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (d *xcheckDaemon) serveList(w http.ResponseWriter, q url.Values) {
	wantType := q.Get("type")
	wantStatus := q.Get("status")
	fieldPreds := map[string]string{}
	for _, fv := range q["field"] {
		if k, v, ok := strings.Cut(fv, "="); ok {
			fieldPreds[k] = v
		}
	}
	var matched []map[string]any
	for _, b := range d.beads {
		if wantType != "" && b["type"] != wantType {
			continue
		}
		if wantStatus != "" && b["status"] != wantStatus {
			continue
		}
		fields := xcheckBeadFields(b)
		ok := true
		for k, v := range fieldPreds {
			if fields[k] != v {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, b)
		}
	}
	// Honor limit/offset so the pager terminates.
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if offset > len(matched) {
		offset = len(matched)
	}
	page := matched[offset:]
	if limit > 0 && len(page) > limit {
		page = page[:limit]
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"beads": page, "total": len(matched)})
}

func xcheckBeadFields(b map[string]any) map[string]string {
	out := map[string]string{}
	if raw, ok := b["fields"].(map[string]any); ok {
		for k, v := range raw {
			out[k], _ = v.(string)
		}
	}
	return out
}

func xcheckAgentBeadFixt(id string, fields map[string]any) map[string]any {
	return map[string]any{"id": id, "type": "agent", "status": "open", "fields": fields}
}

func xcheckSessionBeadFixt(id string, fields map[string]any) map[string]any {
	return map[string]any{"id": id, "type": "session", "status": "open", "fields": fields}
}

// newTestSweep wires a sweep against the two stubs.
func newTestSweep(t *testing.T, daemon *xcheckDaemon, vm *vmStub) (*xcheckSweep, *gardeniometrics.XCheckMetrics, *prometheus.Registry) {
	t.Helper()
	dsrv := daemon.server(t)
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: dsrv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	var upstream string
	if vm != nil {
		upstream = vm.server(t).URL
	}
	vmClient := &xcheckVMClient{upstream: upstream, http: dsrv.Client()}
	metrics := gardeniometrics.NewXCheckMetrics("test")
	reg := prometheus.NewRegistry()
	if err := metrics.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	sweep := newXCheckSweep(client, vmClient, metrics, 0, slog.Default())
	return sweep, metrics, reg
}

func agreementOf(t *testing.T, m *gardeniometrics.XCheckMetrics, pair string) float64 {
	t.Helper()
	return testutil.ToFloat64(m.AgreementGauge().WithLabelValues(pair, "test"))
}

func pairSeriesCount(m *gardeniometrics.XCheckMetrics) int {
	return testutil.CollectAndCount(m.AgreementGauge())
}

// --- pair computations ------------------------------------------------------

func TestSweepPublishesPromQLPairs(t *testing.T) {
	vm := &vmStub{rules: []vmRule{
		// P1 sides.
		{contains: "max by (account, env) (gardenio_account_cost_usd))", value: 100},
		{contains: "gardenio_agent_attributed_cost_usd", value: 95},
		// P2 sides.
		{contains: `dim="role"`, value: 50},
		{contains: `dim="project"`, value: 50},
	}}
	sweep, metrics, _ := newTestSweep(t, &xcheckDaemon{}, vm)
	sweep.syncAll(context.Background())

	// P1: 95 vs 100 → 0.95.
	if got := agreementOf(t, metrics, xcheckPairBillConservation); math.Abs(got-0.95) > 1e-9 {
		t.Fatalf("P1 agreement = %v, want 0.95", got)
	}
	// P2: 50 vs 50 → 1.0.
	if got := agreementOf(t, metrics, xcheckPairTagConservation); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("P2 agreement = %v, want 1.0", got)
	}
}

// TestTokenParityUsesLevelsNotIncrease guards the ka-0000002557 fix: side A
// (gardenio_session_tokens_total) is push-once-at-close, so increase()/rate()
// over it is structurally 0 and pinned the pair at agreement 0.000 forever. The
// fix compares last_over_time LEVELS (a single push is still observed) of both
// cumulative counters, intersected on agent_name so the two sides cover the same
// population. This test fails if either side regresses to an increase()/rate()
// construction or drops the last_over_time level read.
func TestTokenParityUsesLevelsNotIncrease(t *testing.T) {
	vm := &vmStub{}
	sweep, _, _ := newTestSweep(t, &xcheckDaemon{}, vm)
	if _, err := sweep.computeTokenParity(context.Background()); err != nil {
		t.Fatalf("computeTokenParity: %v", err)
	}

	// Side A leads with the push-once close counter; side B leads with the
	// live-scrape token gauges. Both must read levels via last_over_time and
	// intersect on agent_name, and neither may use increase()/rate().
	sideA := vm.queryFor(t, "sum((sum by (agent_name) (last_over_time(gardenio_session_tokens_total")
	sideB := vm.queryFor(t, "sum(((sum by (agent_name) (last_over_time(gardenio_session_input_tokens_total")
	for _, q := range []string{sideA, sideB} {
		if strings.Contains(q, "increase(") || strings.Contains(q, "rate(") {
			t.Fatalf("token-parity query must not use increase()/rate() on a push-once counter: %q", q)
		}
		if !strings.Contains(q, "last_over_time(") {
			t.Fatalf("token-parity query must read levels via last_over_time: %q", q)
		}
		if !strings.Contains(q, "and on (agent_name)") {
			t.Fatalf("token-parity query must intersect sides on agent_name: %q", q)
		}
	}
}

// TestTokenParityAgreementOverSharedPopulation exercises the published score end
// to end: with the close-total level summing to 100 and the live-scrape levels
// summing to 95 over the shared agent population, the pair reads 0.95 — the
// meaningful corroboration the old increase()-on-push-once construction could
// never produce (it was structurally 0.000; ka-0000002557).
func TestTokenParityAgreementOverSharedPopulation(t *testing.T) {
	vm := &vmStub{rules: []vmRule{
		// Side A leads with the close counter (…tokens_total … and on … input…).
		{contains: "sum((sum by (agent_name) (last_over_time(gardenio_session_tokens_total", value: 100},
		// Side B leads with the live-scrape gauges (…input_tokens … and on … tokens_total).
		{contains: "sum(((sum by (agent_name) (last_over_time(gardenio_session_input_tokens_total", value: 95},
	}}
	sweep, metrics, _ := newTestSweep(t, &xcheckDaemon{}, vm)
	sweep.syncAll(context.Background())

	if got := agreementOf(t, metrics, xcheckPairTokenParity); math.Abs(got-0.95) > 1e-9 {
		t.Fatalf("token-parity agreement = %v, want 0.95", got)
	}
}

// TestTokenParityAbsentWhenPopulationDisjoint verifies the graceful-degradation
// contract survives the fix: if a producer emits nothing in the window the
// `and on (agent_name)` intersection is empty, the query returns no vector, and
// the pair is published as ABSENT rather than a fabricated score
// (ka-0000002557).
func TestTokenParityAbsentWhenPopulationDisjoint(t *testing.T) {
	vm := &vmStub{rules: []vmRule{
		{contains: "gardenio_session_tokens_total", absent: true},
		{contains: "gardenio_session_input_tokens_total", absent: true},
	}}
	sweep, metrics, _ := newTestSweep(t, &xcheckDaemon{}, vm)
	sweep.syncAll(context.Background())

	if v := agreementOf(t, metrics, xcheckPairTokenParity); v != 0 {
		t.Fatalf("token-parity must be absent when a side is empty, got %v", v)
	}
}

func TestSweepSkipsPairWhenMetricAbsent(t *testing.T) {
	vm := &vmStub{rules: []vmRule{
		// P1 side A present, side B absent → pair must be skipped entirely.
		{contains: "max by (account, env) (gardenio_account_cost_usd))", value: 100},
		{contains: "gardenio_agent_attributed_cost_usd", absent: true},
	}}
	sweep, metrics, _ := newTestSweep(t, &xcheckDaemon{}, vm)
	sweep.syncAll(context.Background())

	// No P1 agreement series should exist (only one side available).
	m := metrics.AgreementGauge()
	if testutil.CollectAndCount(m, "gardenio_xcheck_agreement") > 0 {
		// Ensure specifically P1 is absent.
		if v := testutil.ToFloat64(m.WithLabelValues(xcheckPairBillConservation, "test")); v != 0 {
			t.Fatalf("P1 should be absent when a side is missing, got %v", v)
		}
	}
}

func TestSweepNoVMUpstreamSkipsPromQLPairs(t *testing.T) {
	// No VM stub → PromQL pairs unavailable; bead-scan pairs (P4/P5/P7) still run.
	daemon := &xcheckDaemon{beads: []map[string]any{
		xcheckSessionBeadFixt("s1", map[string]any{"settled_cost_usd": "10", "total_cost_usd": "8"}),
		xcheckAgentBeadFixt("a1", map[string]any{"litellm_spend_close": "10", "litellm_spend_first_baseline": "0"}),
	}}
	sweep, metrics, _ := newTestSweep(t, daemon, nil)
	sweep.syncAll(context.Background())

	// P4 (sessions-vs-agent): session settled 10 vs agent delta 10 → 1.0.
	if got := agreementOf(t, metrics, xcheckPairSessionsVsAgent); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("P4 agreement = %v, want 1.0", got)
	}
	// P1 (PromQL) must be absent — no upstream.
	if v := testutil.ToFloat64(metrics.AgreementGauge().WithLabelValues(xcheckPairBillConservation, "test")); v != 0 {
		t.Fatalf("P1 must be absent without a metrics upstream, got %v", v)
	}
}

func TestSweepSessionsVsAgent(t *testing.T) {
	daemon := &xcheckDaemon{beads: []map[string]any{
		xcheckSessionBeadFixt("s1", map[string]any{"settled_cost_usd": "6"}),
		xcheckSessionBeadFixt("s2", map[string]any{"settled_cost_usd": "4"}),
		// Agent lifetime delta = close - firstBaseline = 12 - 2 = 10, matching Σ settled.
		xcheckAgentBeadFixt("a1", map[string]any{"litellm_spend_close": "12", "litellm_spend_first_baseline": "2"}),
	}}
	sweep, metrics, _ := newTestSweep(t, daemon, nil)
	sweep.syncAll(context.Background())

	// P4: session settled 6+4=10 vs agent delta 10 → 1.0. (P7 estimate-vs-settled
	// was removed in ka-0000007296 — its side A, session total_cost_usd, has no
	// producer since coop went cost-free, so it never published a score.)
	if got := agreementOf(t, metrics, xcheckPairSessionsVsAgent); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("P4 agreement = %v, want 1.0", got)
	}
}

// TestSweepAgentSpendParity: P10 sums only the beads that carry both the old
// and the new spend figure (ka-0000006903).
func TestSweepAgentSpendParity(t *testing.T) {
	daemon := &xcheckDaemon{beads: []map[string]any{
		// Both figures present → contributes to both sides: old settled 2+8=10,
		// new stateless recompute 2.5+8=10.5.
		xcheckAgentBeadFixt("a1", map[string]any{"total_cost_usd": "2", "spend_logs_cost_usd": "2.5"}),
		xcheckAgentBeadFixt("a2", map[string]any{"total_cost_usd": "8", "spend_logs_cost_usd": "8"}),
		// Only one figure → excluded from BOTH sides (the pair compares one
		// population): an old-path-only bead (predates the sweep) and a
		// new-path-only bead (never settled) must not skew either sum.
		xcheckAgentBeadFixt("a3", map[string]any{"total_cost_usd": "100"}),
		xcheckAgentBeadFixt("a4", map[string]any{"spend_logs_cost_usd": "50"}),
	}}
	sweep, metrics, _ := newTestSweep(t, daemon, nil)
	sweep.syncAll(context.Background())

	want := gardeniometrics.XCheckAgreement(10, 10.5)
	if got := agreementOf(t, metrics, xcheckPairAgentSpendParity); math.Abs(got-want) > 1e-9 {
		t.Fatalf("P10 agreement = %v, want %v", got, want)
	}
	valA := testutil.ToFloat64(metrics.ValueGauge().WithLabelValues(xcheckPairAgentSpendParity, gardeniometrics.XCheckSideA, "test"))
	valB := testutil.ToFloat64(metrics.ValueGauge().WithLabelValues(xcheckPairAgentSpendParity, gardeniometrics.XCheckSideB, "test"))
	if valA != 10 || valB != 10.5 {
		t.Fatalf("P10 sides = (%v, %v), want (10, 10.5) — side a is the old settled figure, side b the new recompute", valA, valB)
	}
}

// TestSweepAgentSpendParityAbsentWhenNoBeadCarriesBoth: P10 publishes nothing,
// not a hollow 0/0, when no bead carries both figures (ka-0000006903).
func TestSweepAgentSpendParityAbsentWhenNoBeadCarriesBoth(t *testing.T) {
	daemon := &xcheckDaemon{beads: []map[string]any{
		xcheckAgentBeadFixt("a1", map[string]any{"total_cost_usd": "5"}),
		xcheckAgentBeadFixt("a2", map[string]any{"spend_logs_cost_usd": "5"}),
	}}
	sweep, metrics, _ := newTestSweep(t, daemon, nil)
	sweep.syncAll(context.Background())

	if v := testutil.ToFloat64(metrics.AgreementGauge().WithLabelValues(xcheckPairAgentSpendParity, "test")); v != 0 {
		t.Fatalf("P10 must stay absent when no bead carries both figures (not a hollow 0/0), got %v", v)
	}
}

func TestSweepInterruptParity(t *testing.T) {
	vm := &vmStub{rules: []vmRule{
		{contains: "gardenio_agent_involuntary_deaths_total", value: 4},
	}}
	daemon := &xcheckDaemon{beads: []map[string]any{
		closedAgent("a1", "context_cancelled_involuntary"),
		closedAgent("a2", "stop_gate_cooldown_involuntary"),
		// The unmarked C9 family (ka-0000001146) counts on side B exactly like
		// the emitter counts it on side A — the shared-classifier guarantee.
		closedAgent("a3", "context_cancelled_unclaimed: pod terminated with tool calls recorded but no claimed issue"),
		// So does the unmarked claim-lookup-failed family (gd-TYjMaxCMZR).
		closedAgent("a6", "context_cancelled_claim_lookup_failed: pod terminated and the claimed-task lookup could not be verified"),
		closedAgent("a4", "done"),              // clean close — not counted
		closedAgent("a5", "context_cancelled"), // zero-work teardown — not counted
	}}
	sweep, metrics, _ := newTestSweep(t, daemon, vm)
	sweep.syncAll(context.Background())

	// Side A (metric) = 4, side B (bead scan) = 4 counted deaths → agreement 1.0.
	if got := agreementOf(t, metrics, xcheckPairInterruptParity); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("P9 agreement = %v, want 1.0", got)
	}
}

// TestSweepInterruptParityDivergenceWhenMetricAbsent: P9 reads an absent deaths
// counter as 0, so involuntary deaths recorded on beads surface as divergence
// (ka-0000004761).
func TestSweepInterruptParityDivergenceWhenMetricAbsent(t *testing.T) {
	// The deaths metric is absent (counter never fired) but beads recorded two
	// involuntary deaths — the exact drift P9 exists to catch. It must surface as
	// disagreement (side A treated as present 0), NOT be silently skipped.
	vm := &vmStub{rules: []vmRule{
		{contains: "gardenio_agent_involuntary_deaths_total", absent: true},
	}}
	daemon := &xcheckDaemon{beads: []map[string]any{
		closedAgent("a1", "context_cancelled_involuntary"),
		closedAgent("a2", "stop_gate_cooldown_involuntary"),
	}}
	sweep, metrics, _ := newTestSweep(t, daemon, vm)
	sweep.syncAll(context.Background())

	// 0 vs 2 → agreement 0.0, and the pair IS published (not absent).
	if got := agreementOf(t, metrics, xcheckPairInterruptParity); got != 0 {
		t.Fatalf("P9 agreement = %v, want 0 (divergence surfaced)", got)
	}
	if v := testutil.ToFloat64(metrics.ValueGauge().WithLabelValues(xcheckPairInterruptParity, gardeniometrics.XCheckSideB, "test")); v != 2 {
		t.Fatalf("P9 side b = %v, want 2", v)
	}
}

// TestSweepInterruptParityAbsentWhenCleanWindow: with no deaths metric and no
// involuntary closes, P9 publishes nothing (ka-0000004761).
func TestSweepInterruptParityAbsentWhenCleanWindow(t *testing.T) {
	// Metric absent AND no involuntary beads → clean window, pair stays absent.
	vm := &vmStub{rules: []vmRule{{contains: "gardenio_agent_involuntary_deaths_total", absent: true}}}
	daemon := &xcheckDaemon{beads: []map[string]any{closedAgent("a1", "done")}}
	sweep, metrics, _ := newTestSweep(t, daemon, vm)
	sweep.syncAll(context.Background())
	if v := testutil.ToFloat64(metrics.AgreementGauge().WithLabelValues(xcheckPairInterruptParity, "test")); v != 0 {
		t.Fatalf("P9 must be absent in a clean window, got %v", v)
	}
}

func closedAgent(id, closeReason string) map[string]any {
	return map[string]any{"id": id, "type": "agent", "status": "closed",
		"fields": map[string]any{"close_reason": closeReason}}
}

func TestSweepRetiresStalePairWhenSourceVanishes(t *testing.T) {
	vm := &vmStub{rules: []vmRule{
		{contains: `dim="role"`, value: 50},
		{contains: `dim="project"`, value: 50},
	}}
	sweep, metrics, _ := newTestSweep(t, &xcheckDaemon{}, vm)
	sweep.syncAll(context.Background())
	if agreementOf(t, metrics, xcheckPairTagConservation) == 0 {
		t.Fatal("P2 should be published on first sweep")
	}

	// Source vanishes: both P2 sides now absent. Next sweep must CLEAR the stale
	// series rather than leave a frozen healthy score.
	vm.rules = []vmRule{{contains: `dim="role"`, absent: true}, {contains: `dim="project"`, absent: true}}
	sweep.syncAll(context.Background())
	if v := testutil.ToFloat64(metrics.AgreementGauge().WithLabelValues(xcheckPairTagConservation, "test")); v != 0 {
		t.Fatalf("P2 agreement should be cleared after source vanished, got %v", v)
	}
}

func TestSweepP5RollupVsTreeAbsentWithNoGardeners(t *testing.T) {
	// No gardener beads → P5 has nothing to corroborate; must stay absent (not 0/0).
	sweep, metrics, _ := newTestSweep(t, &xcheckDaemon{}, nil)
	sweep.syncAll(context.Background())
	if v := testutil.ToFloat64(metrics.AgreementGauge().WithLabelValues(xcheckPairRollupVsTree, "test")); v != 0 {
		t.Fatalf("P5 must be absent with no gardeners, got %v", v)
	}
}

func TestParseXCheckFloat(t *testing.T) {
	cases := map[string]float64{"": 0, "12.5": 12.5, "-3": 0, "  4 ": 4, "bad": 0}
	for in, want := range cases {
		if got := parseXCheckFloat(in); got != want {
			t.Fatalf("parseXCheckFloat(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSweepEmitsNoSeriesWhenEverythingAbsent(t *testing.T) {
	// No VM, no beads → every pair unavailable → zero series published.
	sweep, metrics, _ := newTestSweep(t, &xcheckDaemon{}, nil)
	sweep.syncAll(context.Background())
	if n := pairSeriesCount(metrics); n != 0 {
		t.Fatalf("expected 0 agreement series with no sources, got %d", n)
	}
}
