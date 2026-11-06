package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// staleRevivalEngine wires a TriggerEngine with the stale-revival-review-escalate
// sweep enabled, a recording notifier, and a MUTABLE clock (via the returned
// pointer) so the once-per-hour throttle and day-scale age thresholds can be
// exercised deterministically.
func staleRevivalEngine(t *testing.T, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon, *recordingNotifier, *time.Time) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	rec := &recordingNotifier{}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	clk := &now
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                            daemon,
		Notifier:                          rec.notify,
		Logger:                            slog.Default(),
		StaleRevivalReviewEscalateEnabled: true,
		StaleRevivalReviewEscalateNotify:  true,
		Clock:                             func() time.Time { return *clk },
	})
	return engine, daemon, rec, clk
}

// revivalReviewDecision builds an open revival-review decision fixture filed
// `ageDays` before the engine's clock, at the given priority.
func revivalReviewDecision(id string, priority int, ageDays int, now time.Time, extra map[string]string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"prompt": "trigger-reviver-revival-review for gd-abc123 (some gardener): recommendation keep-disabled.",
	}
	for k, v := range extra {
		fields[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "decision",
		Status:    "open",
		Priority:  priority,
		Title:     "trigger-reviver-revival-review for gd-abc123 (some gardener): recommendation keep-disabled.",
		Fields:    fields,
		CreatedAt: now.Add(-time.Duration(ageDays) * 24 * time.Hour),
	}
}

// lastUpdateBeadReq returns the last UpdateBead request recorded for beadID (and
// whether one exists), so a test can assert the priority the sweep wrote.
func lastUpdateBeadReq(m *mockTriggerDaemon, beadID string) (beadsapi.UpdateBeadRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out beadsapi.UpdateBeadRequest
	found := false
	for i, id := range m.updatedBeadIDs {
		if id == beadID {
			out = m.updatedBeadReqs[i]
			found = true
		}
	}
	return out, found
}

func TestStaleRevivalReviewEscalate_BumpsToP1AtSevenDays(t *testing.T) {
	engine, daemon, rec, _ := staleRevivalEngine(t)
	dec := revivalReviewDecision("gd-stale1", 2, 8, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), nil)
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	req, ok := lastUpdateBeadReq(daemon, "gd-stale1")
	if !ok {
		t.Fatalf("expected an UpdateBead call for the stale decision")
	}
	if req.Priority == nil || *req.Priority != 1 {
		t.Fatalf("expected priority bump to P1, got %v", req.Priority)
	}
	if req.Fields[staleRevivalReviewEscalatedField] != "1" {
		t.Fatalf("expected escalated-priority stamp = 1, got %v", req.Fields[staleRevivalReviewEscalatedField])
	}
	// Audit comment written under the machine operator.
	if len(daemon.commentedIDs) != 1 || daemon.commentedIDs[0] != "gd-stale1" {
		t.Fatalf("expected exactly one audit comment on gd-stale1, got %v", daemon.commentedIDs)
	}
	if len(daemon.commentSystemOperators) == 0 || daemon.commentSystemOperators[0] != staleRevivalReviewSystemOperator {
		t.Fatalf("expected comment attributed to system operator %q, got %v",
			staleRevivalReviewSystemOperator, daemon.commentSystemOperators)
	}
	if rec.count() != 1 {
		t.Fatalf("expected one notify for the escalation, got %d", rec.count())
	}
}

func TestStaleRevivalReviewEscalate_BumpsToP0AtFourteenDays(t *testing.T) {
	engine, daemon, _, _ := staleRevivalEngine(t)
	dec := revivalReviewDecision("gd-stale0", 2, 15, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), nil)
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	req, ok := lastUpdateBeadReq(daemon, "gd-stale0")
	if !ok {
		t.Fatalf("expected an UpdateBead call")
	}
	if req.Priority == nil || *req.Priority != 0 {
		t.Fatalf("expected priority bump to P0, got %v", req.Priority)
	}
}

func TestStaleRevivalReviewEscalate_FreshDecisionUntouched(t *testing.T) {
	engine, daemon, rec, _ := staleRevivalEngine(t)
	dec := revivalReviewDecision("gd-fresh", 2, 3, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), nil)
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	if _, ok := lastUpdateBeadReq(daemon, "gd-fresh"); ok {
		t.Fatalf("expected no priority bump for a 3-day-old decision")
	}
	if rec.count() != 0 {
		t.Fatalf("expected no notify, got %d", rec.count())
	}
}

func TestStaleRevivalReviewEscalate_NonRevivalDecisionUntouched(t *testing.T) {
	engine, daemon, _, _ := staleRevivalEngine(t)
	other := &beadsapi.BeadDetail{
		ID:        "gd-other",
		Type:      "decision",
		Status:    "open",
		Priority:  2,
		Title:     "reviver-throughput-remediation: fleet OFF-TARGET",
		Fields:    map[string]string{"prompt": "reviver-throughput-remediation: fleet OFF-TARGET"},
		CreatedAt: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC), // 22 days old
	}
	daemon.beads = []*beadsapi.BeadDetail{other}

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	if _, ok := lastUpdateBeadReq(daemon, "gd-other"); ok {
		t.Fatalf("expected a non-revival-review decision to be left alone")
	}
}

func TestStaleRevivalReviewEscalate_MatchesOnPromptWhenTitleDiffers(t *testing.T) {
	engine, daemon, _, _ := staleRevivalEngine(t)
	dec := &beadsapi.BeadDetail{
		ID:        "gd-promptmatch",
		Type:      "decision",
		Status:    "open",
		Priority:  2,
		Title:     "some unrelated title",
		Fields:    map[string]string{"prompt": "trigger-reviver-revival-review for gd-xyz: keep-disabled"},
		CreatedAt: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC), // 12 days old → P1
	}
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	req, ok := lastUpdateBeadReq(daemon, "gd-promptmatch")
	if !ok || req.Priority == nil || *req.Priority != 1 {
		t.Fatalf("expected prompt-match decision escalated to P1, got ok=%v req=%+v", ok, req)
	}
}

func TestStaleRevivalReviewEscalate_Idempotent(t *testing.T) {
	engine, daemon, rec, clk := staleRevivalEngine(t)
	dec := revivalReviewDecision("gd-idem", 2, 8, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), nil)
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())
	// Advance past the hourly throttle and run again; the reflected priority (P1)
	// now equals the age target, so the second run must be a no-op.
	*clk = clk.Add(2 * time.Hour)
	engine.SweepStaleRevivalReviewEscalate(context.Background())

	if n := countUpdateBeadCalls(daemon, "gd-idem"); n != 1 {
		t.Fatalf("expected exactly one priority bump across two runs, got %d", n)
	}
	if rec.count() != 1 {
		t.Fatalf("expected exactly one notify across two runs, got %d", rec.count())
	}
}

func TestStaleRevivalReviewEscalate_LadderP2ToP1ToP0(t *testing.T) {
	engine, daemon, _, clk := staleRevivalEngine(t)
	// Filed 8 days before the initial clock → first run escalates P2→P1.
	dec := revivalReviewDecision("gd-ladder", 2, 8, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), nil)
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())
	if req, _ := lastUpdateBeadReq(daemon, "gd-ladder"); req.Priority == nil || *req.Priority != 1 {
		t.Fatalf("first run: expected P1, got %v", req.Priority)
	}

	// Advance the clock to make the decision 15 days old → next run escalates to P0.
	*clk = clk.Add(7 * 24 * time.Hour)
	engine.SweepStaleRevivalReviewEscalate(context.Background())
	if req, _ := lastUpdateBeadReq(daemon, "gd-ladder"); req.Priority == nil || *req.Priority != 0 {
		t.Fatalf("second run: expected P0, got %v", req.Priority)
	}
	if n := countUpdateBeadCalls(daemon, "gd-ladder"); n != 2 {
		t.Fatalf("expected exactly two bumps across the ladder, got %d", n)
	}
}

func TestStaleRevivalReviewEscalate_RespectsOperatorDeescalation(t *testing.T) {
	engine, daemon, _, _ := staleRevivalEngine(t)
	// The sweep already escalated this decision to P1 (stamp=1), but an operator
	// deliberately set it back to P2. A fresh 8-day age would target P1 again, but
	// the prior-escalation stamp must stop the sweep from fighting the operator.
	dec := revivalReviewDecision("gd-deesc", 2, 8, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		map[string]string{staleRevivalReviewEscalatedField: "1"})
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	if _, ok := lastUpdateBeadReq(daemon, "gd-deesc"); ok {
		t.Fatalf("expected no re-escalation to a level the sweep already reached")
	}
}

func TestStaleRevivalReviewEscalate_DeescalationStillEscalatesToNewerLevel(t *testing.T) {
	engine, daemon, _, _ := staleRevivalEngine(t)
	// Already escalated to P1 (stamp=1), operator set back to P2, but the decision
	// is now 15 days old → target P0, which is strictly more urgent than the prior
	// stamp, so the sweep DOES escalate to P0.
	dec := revivalReviewDecision("gd-deesc0", 2, 15, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		map[string]string{staleRevivalReviewEscalatedField: "1"})
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	req, ok := lastUpdateBeadReq(daemon, "gd-deesc0")
	if !ok || req.Priority == nil || *req.Priority != 0 {
		t.Fatalf("expected escalation to P0 past the prior P1 stamp, got ok=%v req=%+v", ok, req)
	}
}

func TestStaleRevivalReviewEscalate_DisabledIsNoOp(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	dec := revivalReviewDecision("gd-off", 2, 20, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), nil)
	daemon.beads = []*beadsapi.BeadDetail{dec}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                            daemon,
		Logger:                            slog.Default(),
		StaleRevivalReviewEscalateEnabled: false,
		Clock:                             func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	if _, ok := lastUpdateBeadReq(daemon, "gd-off"); ok {
		t.Fatalf("disabled sweep must be a no-op")
	}
}

func TestStaleRevivalReviewEscalate_AlreadyUrgentUntouched(t *testing.T) {
	engine, daemon, _, _ := staleRevivalEngine(t)
	// A 20-day-old decision already at P0: age target is P0, which is not more
	// urgent than the current priority, so the sweep leaves it alone.
	dec := revivalReviewDecision("gd-alreadyp0", 0, 20, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), nil)
	daemon.beads = []*beadsapi.BeadDetail{dec}

	engine.SweepStaleRevivalReviewEscalate(context.Background())

	if _, ok := lastUpdateBeadReq(daemon, "gd-alreadyp0"); ok {
		t.Fatalf("expected a decision already at P0 to be left alone")
	}
}

// countUpdateBeadCalls counts how many UpdateBead calls targeted beadID.
func countUpdateBeadCalls(m *mockTriggerDaemon, beadID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range m.updatedBeadIDs {
		if id == beadID {
			n++
		}
	}
	return n
}
