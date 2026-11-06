package server

import (
	"context"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	dto "github.com/prometheus/client_model/go"
)

// legacySentinelOutcomeCount reads the current value of
// gardenio_beads_operated_by_legacy_sentinel_total{outcome=<outcome>}. The metric is a
// package-global CounterVec, so tests read deltas rather than absolute values.
func legacySentinelOutcomeCount(t *testing.T, outcome string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := operatedByLegacySentinelTotal.WithLabelValues(outcome).Write(m); err != nil {
		t.Fatalf("read metric outcome=%q: %v", outcome, err)
	}
	return m.GetCounter().GetValue()
}

// TestLegacySentinelCutoverActive_EnvOverride pins the operator override
// precedence: BEADS_LEGACY_SENTINEL_CUTOVER truthy forces the gate ON and
// falsy forces it OFF, regardless of the compile-time cut-over date.
func TestLegacySentinelCutoverActive_EnvOverride(t *testing.T) {
	// A time far in the past so the date branch, if reached, is OFF.
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	// A time far in the future so the date branch, if reached, is ON.
	future := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		val  string
		now  time.Time
		want bool
	}{
		{"1", past, true},    // force ON despite past date
		{"true", past, true}, // force ON
		{"on", past, true},   // force ON
		{"0", future, false}, // force OFF despite future date
		{"false", future, false},
		{"off", future, false},
	}
	for _, tc := range cases {
		t.Setenv("BEADS_LEGACY_SENTINEL_CUTOVER", tc.val)
		if got := legacySentinelCutoverActive(tc.now); got != tc.want {
			t.Errorf("override=%q now=%v: active=%v; want %v", tc.val, tc.now, got, tc.want)
		}
	}
}

// TestLegacySentinelCutoverActive_DateBranch pins the date-based activation:
// with no env override, the gate is OFF strictly before the cut-over day and
// ON on/after it (UTC).
func TestLegacySentinelCutoverActive_DateBranch(t *testing.T) {
	t.Setenv("BEADS_LEGACY_SENTINEL_CUTOVER", "")

	cut, err := time.Parse("2006-01-02", legacySentinelCutoverDate)
	if err != nil {
		t.Fatalf("cut-over constant %q does not parse: %v", legacySentinelCutoverDate, err)
	}

	if legacySentinelCutoverActive(cut.Add(-time.Second)) {
		t.Errorf("active at one second BEFORE cut-over; want inactive")
	}
	if !legacySentinelCutoverActive(cut) {
		t.Errorf("inactive AT the cut-over instant; want active")
	}
	if !legacySentinelCutoverActive(cut.Add(24 * time.Hour)) {
		t.Errorf("inactive one day AFTER cut-over; want active")
	}
}

// TestResolveAuditOperatorRequired_LegacySentinel_CutoverActive_HeaderRejected
// is the core acceptance test (AC-1): with the cut-over active, a write whose
// only attribution is the legacy sentinel (delivered via X-Operated-By) must be
// REFUSED — resolveAuditOperatorRequired returns ok=false with the
// legacy_sentinel_cutover source — instead of FK-binding the sentinel row.
func TestResolveAuditOperatorRequired_LegacySentinel_CutoverActive_HeaderRejected(t *testing.T) {
	t.Setenv("BEADS_LEGACY_SENTINEL_CUTOVER", "1") // force cut-over ON

	srv := newTestServerWithAgentBead(nil)

	rejBefore := legacySentinelOutcomeCount(t, "rejected")

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "none"})
	ctx = applyOperatedByHeader(ctx, operatorLegacyPreAttribution, "none", "test")

	value, id, source, ok := srv.resolveAuditOperatorRequired(ctx)
	if ok {
		t.Fatalf("resolveAuditOperatorRequired returned ok=true; want ok=false (cut-over must reject sentinel)")
	}
	if value != "" || id != 0 {
		t.Fatalf("refused write must return empty value/id; got (%q, %d)", value, id)
	}
	if source != refusalSourceLegacySentinelCutover {
		t.Fatalf("source = %q; want %q", source, refusalSourceLegacySentinelCutover)
	}

	// The rejected-outcome metric must have incremented so the alert can page.
	if got := legacySentinelOutcomeCount(t, "rejected"); got != rejBefore+1 {
		t.Fatalf("rejected metric = %v; want %v (one rejected sentinel write)", got, rejBefore+1)
	}

	// classifyOperatorRefusal (used by CreateBead's newOperatorRefusal, which
	// discards the returned source) must also surface the cut-over label from
	// the trace breadcrumb.
	if got := classifyOperatorRefusal(ctx); got != refusalSourceLegacySentinelCutover {
		t.Fatalf("classifyOperatorRefusal = %q; want %q", got, refusalSourceLegacySentinelCutover)
	}
}

// TestResolveAuditOperatorRequired_LegacySentinel_CutoverActive_AgentBeadPath
// covers the inheritance variant (AC-1): no header, but the agent bead carries
// operated_by=system:legacy-pre-attribution. Pre-cut-over this FK-binds the
// sentinel (the sawLegacy rescue); with the cut-over active it must refuse.
func TestResolveAuditOperatorRequired_LegacySentinel_CutoverActive_AgentBeadPath(t *testing.T) {
	t.Setenv("BEADS_LEGACY_SENTINEL_CUTOVER", "1")

	bead := &model.Bead{
		ID:     "agent-x",
		Fields: []byte(`{"operated_by":"system:legacy-pre-attribution"}`),
	}
	srv := newTestServerWithAgentBead(bead)

	ctx := withTestAgent(context.Background(), "agent-x")
	_, _, source, ok := srv.resolveAuditOperatorRequired(ctx)
	if ok {
		t.Fatalf("ok=true; want ok=false (agent-bead sentinel must be refused post-cut-over)")
	}
	if source != refusalSourceLegacySentinelCutover {
		t.Fatalf("source = %q; want %q", source, refusalSourceLegacySentinelCutover)
	}
}

// TestResolveAuditOperatorRequired_LegacySentinel_CutoverInactive_StillBinds
// pins the pre-cut-over behavior: with the gate OFF the sentinel still
// FK-resolves exactly as before this change (zero behavior change), and the
// accepted-outcome metric increments (so the alert sees a non-zero series only
// after the date, never a false page before it).
func TestResolveAuditOperatorRequired_LegacySentinel_CutoverInactive_StillBinds(t *testing.T) {
	t.Setenv("BEADS_LEGACY_SENTINEL_CUTOVER", "0") // force cut-over OFF

	srv := newTestServerWithAgentBead(nil)

	accBefore := legacySentinelOutcomeCount(t, "accepted")

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "none"})
	ctx = applyOperatedByHeader(ctx, operatorLegacyPreAttribution, "none", "test")

	value, id, _, ok := srv.resolveAuditOperatorRequired(ctx)
	if !ok {
		t.Fatalf("ok=false; want ok=true (sentinel must still bind pre-cut-over)")
	}
	if value != operatorLegacyPreAttribution {
		t.Fatalf("value = %q; want %q", value, operatorLegacyPreAttribution)
	}
	if id == 0 {
		t.Fatalf("id = 0; want non-zero (seeded sentinel row)")
	}
	if got := legacySentinelOutcomeCount(t, "accepted"); got != accBefore+1 {
		t.Fatalf("accepted metric = %v; want %v", got, accBefore+1)
	}
}

// TestResolveAuditOperatorRequired_RealOperator_CutoverActive_Unaffected pins
// that the cut-over gate is a no-op for a real human operator: a normal
// slack:U… write resolves fine even with the cut-over ON, and neither
// sentinel-outcome counter moves.
func TestResolveAuditOperatorRequired_RealOperator_CutoverActive_Unaffected(t *testing.T) {
	t.Setenv("BEADS_LEGACY_SENTINEL_CUTOVER", "1")

	srv := newTestServerWithAgentBead(nil)

	accBefore := legacySentinelOutcomeCount(t, "accepted")
	rejBefore := legacySentinelOutcomeCount(t, "rejected")

	ctx := withTestAgent(context.Background(), "agent-x")
	ctx = applyOperatedByHeader(ctx, "slack:U0OPERATOR1", "fleet", "test")

	value, id, _, ok := srv.resolveAuditOperatorRequired(ctx)
	if !ok {
		t.Fatalf("ok=false; want ok=true (real operator must resolve with cut-over active)")
	}
	if value != "slack:U0OPERATOR1" || id == 0 {
		t.Fatalf("real operator resolution = (%q, %d); want (slack:U0OPERATOR1, non-zero)", value, id)
	}
	if got := legacySentinelOutcomeCount(t, "accepted"); got != accBefore {
		t.Fatalf("accepted metric moved for a non-sentinel write: %v -> %v", accBefore, got)
	}
	if got := legacySentinelOutcomeCount(t, "rejected"); got != rejBefore {
		t.Fatalf("rejected metric moved for a non-sentinel write: %v -> %v", rejBefore, got)
	}
}
