package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// sweepNow is the fixed wall-clock instant sweeper tests run at.
var sweepNow = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

// makeClosedAgentBead builds a closed agent bead as the daemon returns it, with
// title = agent name (the production convention the sweep keys names off).
func makeClosedAgentBead(id, agentName string, createdAt, closedAt time.Time, fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fieldsRaw, _ := json.Marshal(fields)
	return map[string]any{
		"id":         id,
		"title":      agentName,
		"type":       "agent",
		"status":     "closed",
		"created_at": createdAt.Format(time.RFC3339),
		"closed_at":  closedAt.Format(time.RFC3339),
		"fields":     json.RawMessage(fieldsRaw),
	}
}

func TestSweep_SettlesUnsettledClosedBead(t *testing.T) {
	// A bead closed 1h ago with a baseline but no settlement watermark (the
	// in-memory retry queue died with a controller restart) is found and settled
	// from the tag's current total.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeClosedAgentBead("gd-a1", "alpha",
			sweepNow.Add(-3*time.Hour), sweepNow.Add(-1*time.Hour),
			map[string]any{spendBaselineField: "4.25", spendFirstBaselineField: "4.25"})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	r.sweepSettlements(context.Background(), sweepNow)

	updates := daemon.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 settlement write, got %d", len(updates))
	}
	if cost := updates[0].Fields["total_cost_usd"].(float64); cost != 3.50 {
		t.Errorf("total_cost_usd = %v, want 3.50", cost)
	}
	if got := updates[0].Fields[spendCloseField].(float64); got != 7.75 {
		t.Errorf("%s = %v, want 7.75", spendCloseField, got)
	}
}

func TestSweep_SettlesLostFinalSessionOfRespawner(t *testing.T) {
	// A respawner settled through 7.75 (watermark) but its final session's
	// settlement was lost; the tag now reads 10.00. The sweep books the residual
	// and catches the lifetime total up.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 30, TotalSpend: 10.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeClosedAgentBead("gd-a1", "alpha",
			sweepNow.Add(-3*time.Hour), sweepNow.Add(-1*time.Hour),
			map[string]any{
				spendBaselineField:      "7.75",
				spendCloseField:         "7.75",
				spendFirstBaselineField: "4.25",
				"total_cost_usd":        "3.50",
			})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	r.sweepSettlements(context.Background(), sweepNow)

	updates := daemon.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 settlement write, got %d", len(updates))
	}
	if cost := updates[0].Fields["total_cost_usd"].(float64); cost != 5.75 {
		t.Errorf("total_cost_usd = %v, want 5.75 (lifetime caught up)", cost)
	}
}

func TestSweep_SkipsSettledRecentAndAnchorlessBeads(t *testing.T) {
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:settled", LogCount: 20, TotalSpend: 7.75},
		{IndividualRequestTag: "agent:fresh", LogCount: 5, TotalSpend: 3.00},
		{IndividualRequestTag: "agent:anchorless", LogCount: 5, TotalSpend: 9.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{
			// Fully settled: watermark == tag total.
			makeClosedAgentBead("gd-s1", "settled",
				sweepNow.Add(-3*time.Hour), sweepNow.Add(-1*time.Hour),
				map[string]any{spendBaselineField: "4.25", spendCloseField: "7.75"}),
			// Closed inside the grace window: the inline/retry path still owns it.
			makeClosedAgentBead("gd-f1", "fresh",
				sweepNow.Add(-1*time.Hour), sweepNow.Add(-time.Minute),
				map[string]any{spendBaselineField: "0"}),
			// No baseline and no watermark: unsettleable, must not be retried.
			makeClosedAgentBead("gd-n1", "anchorless",
				sweepNow.Add(-3*time.Hour), sweepNow.Add(-1*time.Hour), nil),
		},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	r.sweepSettlements(context.Background(), sweepNow)

	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Fatalf("expected no settlement writes, got %d: %+v", len(updates), updates)
	}
}

// TestSweep_NameReuseGuardSkipsSupersededBead: the settlement sweep skips a
// bead whose agent name a newer bead reuses, so it never settles against the
// newer bead's spend (ka-0000007288).
func TestSweep_NameReuseGuardSkipsSupersededBead(t *testing.T) {
	// Bead gd-old closed unsettled, but a NEWER bead reuses the agent name (a
	// respawning gardener). The tag's current total includes the newer bead's
	// spend, so settling gd-old against it would steal that spend — skip it.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 50, TotalSpend: 20.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{
			makeClosedAgentBead("gd-old", "alpha",
				sweepNow.Add(-10*time.Hour), sweepNow.Add(-5*time.Hour),
				map[string]any{spendBaselineField: "4.25"}),
			makeClosedAgentBead("gd-new", "alpha",
				sweepNow.Add(-4*time.Hour), sweepNow.Add(-1*time.Minute),
				map[string]any{spendBaselineField: "12.00"}),
		},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	r.sweepSettlements(context.Background(), sweepNow)

	// gd-old is guarded (newer bead exists); gd-new is inside the grace window.
	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Fatalf("expected no settlement writes, got %d: %+v", len(updates), updates)
	}
}

func TestSweep_PublishesUnsettledBacklogGauge(t *testing.T) {
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75},
		{IndividualRequestTag: "agent:beta", LogCount: 20, TotalSpend: 3.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{
			makeClosedAgentBead("gd-a1", "alpha",
				sweepNow.Add(-3*time.Hour), sweepNow.Add(-1*time.Hour),
				map[string]any{spendBaselineField: "4.25"}),
			makeClosedAgentBead("gd-b1", "beta",
				sweepNow.Add(-3*time.Hour), sweepNow.Add(-1*time.Hour),
				map[string]any{spendBaselineField: "0", spendCloseField: "3.00"}), // settled
		},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))
	sm := gardeniometrics.NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := sm.Register(reg); err != nil {
		t.Fatal(err)
	}
	r.metrics = sm

	r.sweepSettlements(context.Background(), sweepNow)

	got := testutil.ToFloat64(sm.UnsettledSessionsGauge())
	if got != 1 {
		t.Errorf("gardenio_unsettled_sessions = %v, want 1", got)
	}
}

func TestSettleSessionBead_WritesSettledCostToLatestSession(t *testing.T) {
	// The settlement books the session's cost onto the agent's newest session
	// bead, in the dedicated settled_cost_usd field (never total_cost_usd, which
	// the in-pod gb close could clobber).
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75},
	}
	older, _ := json.Marshal(map[string]any{"agent_bead_id": "gd-a1"})
	newer, _ := json.Marshal(map[string]any{"agent_bead_id": "gd-a1"})
	daemon := &daemonStub{
		beads: []map[string]any{
			makeAgentBead("gd-a1", map[string]any{spendBaselineField: "4.25", spendFirstBaselineField: "4.25"}),
			{
				"id": "gd-sess-old", "title": "session:alpha:1", "type": "session", "status": "closed",
				"created_at": sweepNow.Add(-4 * time.Hour).Format(time.RFC3339),
				"fields":     json.RawMessage(older),
			},
			{
				"id": "gd-sess-new", "title": "session:alpha:2", "type": "session", "status": "open",
				"created_at": sweepNow.Add(-1 * time.Hour).Format(time.RFC3339),
				"fields":     json.RawMessage(newer),
			},
		},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	bead := &beadsapi.BeadDetail{
		ID:     "gd-a1",
		Fields: map[string]string{spendBaselineField: "4.25", spendFirstBaselineField: "4.25"},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	var sessionUpdate *daemonUpdate
	for _, u := range daemon.getUpdates() {
		if u.BeadID == "gd-sess-new" {
			u := u
			sessionUpdate = &u
		}
		if u.BeadID == "gd-sess-old" {
			t.Fatalf("settled the older session bead: %+v", u)
		}
	}
	if sessionUpdate == nil {
		t.Fatal("expected a settled_cost_usd write on the newest session bead")
	}
	if got := sessionUpdate.Fields[sessionSettledCostField].(float64); got != 3.50 {
		t.Errorf("%s = %v, want 3.50", sessionSettledCostField, got)
	}
	if _, present := sessionUpdate.Fields["total_cost_usd"]; present {
		t.Errorf("session settlement must not touch total_cost_usd (gb owns it)")
	}
}
