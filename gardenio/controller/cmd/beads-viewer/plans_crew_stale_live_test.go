package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// plans_crew_stale_live_test.go — iter-42 (ka-0000005352): the ACTIVE-plan twin
// of iter-30's terminal zombie reconcile. A crew that crashed mid-task keeps its
// alive agent_state (working/spawning/yielding) until the controller reaps it
// (audit ka-0000007486), so its heartbeat — its updated_at — is what actually
// went stale. The plan card runs TWO liveness measurements: the ⚠ stall badge
// (assistantIsLive / Engine.IsLive) freshness-gates on the 1h agentStaleThreshold
// and counts such a crew a DEAD-agent orphan, but classifyCrewBead trusted the
// alive agent_state ALONE — so the SAME crew pulsed the card's green "● N working"
// live-dot + a live face + inflated live_cost_usd while the badge called it dead.
// Two measurements on one card contradicting each other. iter-42 freshness-gates
// classifyCrewBead so the crew signal reads the SAME liveness the stall detector
// (and isAgentStale / agentResolver.isAlive) already use, and re-buckets a
// stale-heartbeat crew to the loud reachable "stuck" disposition it already flags.

// --- unit truth-table: classifyCrewBead's freshness gate ---------------------

// TestClassifyCrewBead_FreshnessGate is the core truth-table: an OPEN alive-state
// crew is live ONLY while its heartbeat is fresh; a stale heartbeat demotes it to
// the reachable "stuck" (failed) disposition, exactly as the stall badge flags it.
func TestClassifyCrewBead_FreshnessGate(t *testing.T) {
	now := crewFixtureNow
	mk := func(state, status string, updated time.Time) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			Type:      "agent",
			Status:    status,
			Fields:    map[string]string{"agent_state": state},
			UpdatedAt: updated,
		}
	}
	cases := []struct {
		name       string
		bead       *beadsapi.BeadDetail
		wantBucket string
		wantFailed bool
		wantLive   bool
	}{
		{
			name:       "fresh working crew is live",
			bead:       mk("working", "in_progress", now.Add(-5*time.Minute)),
			wantBucket: "alive", wantFailed: false, wantLive: true,
		},
		{
			name:       "fresh spawning crew is live",
			bead:       mk("spawning", "in_progress", now.Add(-30*time.Second)),
			wantBucket: "alive", wantFailed: false, wantLive: true,
		},
		{
			name: "just-under-the-bar working crew is still live",
			// 59m59s < 1h — inside the freshness window.
			bead:       mk("working", "in_progress", now.Add(-time.Hour+time.Second)),
			wantBucket: "alive", wantFailed: false, wantLive: true,
		},
		{
			name: "stale-heartbeat working crew is NOT live and reads stuck",
			// Exactly at the 1h bar — the crashed-mid-task crew (>= threshold).
			bead:       mk("working", "in_progress", now.Add(-time.Hour)),
			wantBucket: "stuck", wantFailed: true, wantLive: false,
		},
		{
			name:       "long-stale working crew is NOT live and reads stuck",
			bead:       mk("working", "in_progress", now.Add(-6*time.Hour)),
			wantBucket: "stuck", wantFailed: true, wantLive: false,
		},
		{
			name: "zero updated_at is treated as live (just-spawned, no stamp yet)",
			// Matches isAgentStale / agentResolver.isAlive: an uninitialised stamp
			// is a new agent, never flagged stale.
			bead:       mk("working", "in_progress", time.Time{}),
			wantBucket: "alive", wantFailed: false, wantLive: true,
		},
		{
			name:       "an already-stuck agent_state is unaffected (still stuck, not live)",
			bead:       mk("rate_limited", "in_progress", now.Add(-2*time.Minute)),
			wantBucket: "stuck", wantFailed: true, wantLive: false,
		},
		{
			name: "a queued (idle) crew is untouched by the freshness gate",
			// Only ALIVE states are gated — a stale queued crew stays idle (iter-31/32
			// own its wait-age), never demoted to stuck by this gate.
			bead:       mk("queued", "in_progress", now.Add(-6*time.Hour)),
			wantBucket: "idle", wantFailed: false, wantLive: false,
		},
		{
			name:       "a cleanly-closed crew is never live regardless of stamp",
			bead:       mk("done", "closed", now.Add(-time.Minute)),
			wantBucket: "idle", wantFailed: false, wantLive: false,
		},
		{
			name: "a stale alive-state CLOSED crew is not re-marked failed by freshness",
			// The gate only fires on OPEN beads (a closed crew's terminal is gone —
			// nothing to reach). A closed alive-state bead with no fail wrapup is a
			// clean finish; staleness must not manufacture a failure.
			bead:       mk("working", "closed", now.Add(-6*time.Hour)),
			wantBucket: "alive", wantFailed: false, wantLive: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bucket, failed, live := classifyCrewBead(c.bead, now)
			if bucket != c.wantBucket {
				t.Errorf("bucket = %q, want %q", bucket, c.wantBucket)
			}
			if failed != c.wantFailed {
				t.Errorf("failed = %v, want %v", failed, c.wantFailed)
			}
			if live != c.wantLive {
				t.Errorf("live = %v, want %v", live, c.wantLive)
			}
		})
	}
}

// --- render regression: the index card (planCrewBulk → row.Crew) -------------

// TestPlansIndex_StaleWorkingCrewNoLivePulse proves an ACTIVE plan whose only
// alive-state crew has a stale heartbeat renders NO live pulse — no
// plan-card-crew-live class, no live-dot, no "· N working" facet, no live face.
// The crew instead surfaces in the loud reachable stuck facet. WITHOUT the
// freshness gate the card would pulse green (the exact self-contradiction beside
// the ⚠ stall badge, which already flags the same crew a dead-agent orphan).
func TestPlansIndex_StaleWorkingCrewNoLivePulse(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "in_progress"
	// A crew whose agent_state still reads "working" but whose heartbeat froze 3h
	// ago — crashed mid-task, not yet reaped.
	seedStaleCrew(f, "gd-stale", "gardenio-crashed-crew-xyz", "gd-step1", "working", 3*time.Hour)
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")

	if strings.Contains(card, "plan-card-crew-live") {
		t.Errorf("a stale-heartbeat crew must not render the live crew class (green pulse); card:\n%s", card)
	}
	if strings.Contains(card, `class="plan-card-crew-dot"`) {
		t.Errorf("a stale-heartbeat crew must not render the pulsing live-dot")
	}
	if strings.Contains(card, "plan-card-crew-working") {
		t.Errorf("a stale-heartbeat crew must not render the '· N working' facet")
	}
	if strings.Contains(card, `class="plan-card-crew-face"`) {
		t.Errorf("a stale-heartbeat crew must not render a live crew face avatar")
	}
	// It surfaces instead as the loud, reachable stuck facet — an operator can jump
	// to its terminal to reap it, matching the ⚠ stall badge on the same card.
	if !strings.Contains(card, "plan-card-crew-failed") {
		t.Errorf("a stale-heartbeat crew must surface as the reachable stuck facet; card:\n%s", card)
	}
	// The crew total is honest history regardless.
	if !strings.Contains(card, "1 crew") {
		t.Errorf("the crew total must still count the stale crew (1 crew); card:\n%s", card)
	}
}

// TestPlansIndex_FreshWorkingCrewStillPulses is the anti-over-reach guard: a
// FRESH working crew on an active plan MUST still render the live pulse + working
// facet — the gate demotes ONLY a stale-heartbeat crew, never a healthy one.
func TestPlansIndex_FreshWorkingCrewStillPulses(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "in_progress"
	// seedCrew stamps updated_at at crewFixtureNow − 5m — well inside the 1h bar.
	seedCrew(f, "gd-fresh", "gardenio-healthy-crew-abc", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")

	if !strings.Contains(card, "plan-card-crew-live") {
		t.Errorf("a fresh working crew MUST still render the green pulse; card:\n%s", card)
	}
	if !strings.Contains(card, "plan-card-crew-working") {
		t.Errorf("a fresh working crew MUST still render the '· N working' facet")
	}
	if !strings.Contains(card, `class="plan-card-crew-face"`) {
		t.Errorf("a fresh working crew MUST still render a live crew face avatar")
	}
	// A healthy crew is NOT stuck.
	if strings.Contains(card, "plan-card-crew-failed") {
		t.Errorf("a fresh working crew must not surface as stuck; card:\n%s", card)
	}
}

// --- render regression: the cockpit roster (planCrew) ------------------------

// TestPlanDetail_StaleWorkingCrewNoLivePulse proves the cockpit crew roster on an
// active plan with a stale-heartbeat working crew renders no live signal — no
// header "working" chip, no is-live member, no live-dot — matching the index card
// by construction, and surfaces the crew in the loud stuck tally instead.
func TestPlanDetail_StaleWorkingCrewNoLivePulse(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "in_progress"
	seedStaleCrew(f, "gd-stale", "gardenio-crashed-crew-xyz", "gd-step1", "working", 3*time.Hour)
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Assert on rendered ELEMENT markup, not the bare class (the class also appears
	// in the page's shared <style> block).
	if strings.Contains(body, `class="chip plan-crew-count plan-crew-count-working"`) {
		t.Errorf("a stale-heartbeat crew must not render the '(N) working' header chip")
	}
	if strings.Contains(body, `<span class="plan-crew-live-dot"`) {
		t.Errorf("a stale-heartbeat crew must not render a per-member live-dot element")
	}
	// It surfaces in the loud reachable stuck tally instead.
	if !strings.Contains(body, `class="chip plan-crew-count plan-crew-count-failed"`) {
		t.Errorf("a stale-heartbeat crew must surface as the reachable stuck tally chip")
	}
	// The raw agent_state string is preserved as a factual record on the member row.
	if !strings.Contains(body, "working") {
		t.Errorf("the member's raw agent_state should still read 'working' as a factual record")
	}
}

// TestPlanDetail_FreshWorkingCrewStillPulses is the cockpit anti-over-reach twin:
// a fresh working crew still renders the header working chip + a live member.
func TestPlanDetail_FreshWorkingCrewStillPulses(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-fresh", "gardenio-healthy-crew-abc", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="chip plan-crew-count plan-crew-count-working"`) {
		t.Errorf("a fresh working crew MUST still render the '(N) working' header chip")
	}
	if !strings.Contains(body, `<span class="plan-crew-live-dot"`) {
		t.Errorf("a fresh working crew MUST still render a per-member live-dot element")
	}
}

// --- index ↔ cockpit parity --------------------------------------------------

// TestStaleCrewLiveReconcile_IndexCockpitParity proves the index rollup and the
// cockpit roster classify the SAME stale-heartbeat crew identically: both report
// zero working, and both count it in the open-stuck tally — the by-construction
// parity the crew-classification family always holds (both flow through
// classifyCrewBead with the same clock).
func TestStaleCrewLiveReconcile_IndexCockpitParity(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "in_progress"
	seedStaleCrew(f, "gd-stale", "gardenio-crashed-crew-xyz", "gd-step1", "working", 3*time.Hour)
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "in_progress"}}
	cockpit := s.planCrew(context.Background(), "", steps, nil)

	index := s.planCrewBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1"},
	}, nil)["gd-plan1"]

	if cockpit.Working != index.Working {
		t.Errorf("parity: cockpit.Working=%d != index.Working=%d", cockpit.Working, index.Working)
	}
	if cockpit.Working != 0 {
		t.Errorf("both surfaces must report 0 working for a stale-heartbeat crew, got %d", cockpit.Working)
	}
	if cockpit.OpenStuck() != index.OpenStuck() {
		t.Errorf("parity: cockpit.OpenStuck=%d != index.OpenStuck=%d", cockpit.OpenStuck(), index.OpenStuck())
	}
	if index.OpenStuck() != 1 {
		t.Errorf("the stale crew must count in the open-stuck tally, got %d", index.OpenStuck())
	}
	if cockpit.Total != index.Total || index.Total != 1 {
		t.Errorf("parity: totals must agree at 1, got cockpit=%d index=%d", cockpit.Total, index.Total)
	}
}

// TestPlanCrewBulk_StaleWorkingCrewAddsNoFanOut proves the freshness gate reads
// only beads already in hand — the rollup still pays exactly ONE reverse
// assigned-edge batch + ONE agent hydration batch, no per-crew round-trip.
func TestPlanCrewBulk_StaleWorkingCrewAddsNoFanOut(t *testing.T) {
	f := seedPlansFake()
	seedStaleCrew(f, "gd-stale", "gardenio-crashed-crew-xyz", "gd-step1", "working", 3*time.Hour)
	s := crewTestServer(f)

	_ = s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if f.batchGetDepsCalls != 1 || f.batchGetBeadsCalls != 1 {
		t.Errorf("fan-out = %d deps + %d beads, want 1 + 1 (the freshness gate adds no daemon call)", f.batchGetDepsCalls, f.batchGetBeadsCalls)
	}
}
