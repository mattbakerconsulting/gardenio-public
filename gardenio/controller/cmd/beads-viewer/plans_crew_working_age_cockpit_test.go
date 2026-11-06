package main

// plans_crew_working_age_cockpit_test.go — (ka-0000005352): the COCKPIT roster's
// "N working" chip RUNTIME age, the cockpit-side twin of the index card's "· M
// working 4h" facet (plans_working_age_test.go). The roster header rendered every
// OTHER crew disposition's wait-age — queued ("· 12m", iter-32) and stuck ("·
// 6h", iter-33) both carry HOW LONG — but the primary "N working" chip was the
// last one still count-only, so a crew grinding a plan for 4 hours read
// identically to one spawned 4 minutes ago. This folds the longest-running live
// crew's elapsed RUNTIME onto the chip face + tooltip, reading the SAME
// oldestCrewRuntime helper over the SAME candidate set the index rollup uses so
// the two surfaces agree by construction (the family signature, advice
// ka-0000005924).
//
// The basis is RUNTIME (now - CreatedAt), NOT last-activity — identical to the
// index facet's load-bearing choice: a working crew heartbeats so now-updatedAt
// reads ~0s and carries no signal, whereas now-CreatedAt reads how long it has
// been ON the plan.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- unit: the cockpit rollup carries the longest-running working crew's runtime -

// TestPlanCrew_WorkingOldestAge proves the cockpit roster reports the RUNTIME of
// the longest-running LIVE crew (the earliest CreatedAt — on the plan longest),
// NOT the newest, and ages on CreatedAt not UpdatedAt. Mirror of the index-side
// TestPlanCrewBulk_WorkingOldestAge: with the clock at 12:00, a crew created at
// 08:00 has run 4h; one at 11:00 has run 1h. Both carry a fresh heartbeat, so an
// updated_at-based age would wrongly read ~5m for both — this pins runtime.
func TestPlanCrew_WorkingOldestAge(t *testing.T) {
	f := seedPlansFake()
	longRunner := seedCrew(f, "gd-cwL", "gardenio-long-worker", "gd-step1", "working", "in_progress")
	longRunner.CreatedAt = time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)   // 4h runtime
	longRunner.UpdatedAt = time.Date(2026, 7, 5, 11, 55, 0, 0, time.UTC) // fresh heartbeat (5m)
	shortRunner := seedCrew(f, "gd-cwS", "gardenio-short-worker", "gd-step1", "working", "in_progress")
	shortRunner.CreatedAt = time.Date(2026, 7, 5, 11, 0, 0, 0, time.UTC)  // 1h runtime
	shortRunner.UpdatedAt = time.Date(2026, 7, 5, 11, 58, 0, 0, time.UTC) // fresh heartbeat (2m)
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Working != 2 {
		t.Fatalf("Working = %d, want 2 (two live crew)", info.Working)
	}
	if got := info.WorkingOldestAge(); got != "4h" {
		t.Errorf("WorkingOldestAge = %q, want %q (the LONGEST-running crew's runtime, aged on CreatedAt not UpdatedAt)", got, "4h")
	}
}

// TestPlanCrew_WorkingOldestAge_Undatable proves a working crew with no CreatedAt
// contributes no runtime, so the chip reads count-only exactly as before.
func TestPlanCrew_WorkingOldestAge_Undatable(t *testing.T) {
	f := seedPlansFake()
	c := seedCrew(f, "gd-cwU", "gardenio-undatable-worker", "gd-step1", "working", "in_progress")
	c.CreatedAt = time.Time{} // unset — cannot contribute a runtime
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Working != 1 {
		t.Fatalf("Working = %d, want 1", info.Working)
	}
	if got := info.WorkingOldestAge(); got != "" {
		t.Errorf("WorkingOldestAge = %q, want empty (an undatable working crew reads count-only)", got)
	}
}

// TestPlanCrew_WorkingAge_TerminalReconcileClears proves a TERMINAL plan folds
// Working→Done and clears the cockpit runtime age — a done plan has no crew
// "working now", so the live-forward runtime must not linger (the zombie-working
// reconcile family, iter-30). Twin of the index-side
// TestPlanCrewBulk_WorkingAge_TerminalReconcileClears.
func TestPlanCrew_WorkingAge_TerminalReconcileClears(t *testing.T) {
	f := seedPlansFake()
	z := seedCrew(f, "gd-cwZ", "gardenio-zombie-worker", "gd-step1", "working", "in_progress")
	z.CreatedAt = time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)
	z.UpdatedAt = time.Date(2026, 7, 5, 11, 55, 0, 0, time.UTC)
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if got := info.WorkingOldestAge(); got != "4h" {
		t.Fatalf("pre-reconcile WorkingOldestAge = %q, want 4h", got)
	}
	reconciled := info.reconcileTerminal("closed")
	if reconciled.Working != 0 {
		t.Errorf("reconciled Working = %d, want 0 (terminal plan folds working→done)", reconciled.Working)
	}
	if got := reconciled.WorkingOldestAge(); got != "" {
		t.Errorf("reconciled WorkingOldestAge = %q, want empty (a done plan has no live runtime)", got)
	}
}

// TestPlanCrew_WorkingOldestAge_MatchesCockpitAndIndex is the two-derivations
// keystone (advice ka-0000005924): the cockpit roster (planCrew) and the index
// rollup (planCrewBulk) MUST report the SAME longest-running-working runtime for
// the SAME plan — they read the SAME oldestCrewRuntime helper over the SAME
// candidate set + s.now() clock, so a divergence would be a real bug in one.
func TestPlanCrew_WorkingOldestAge_MatchesCockpitAndIndex(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	longRunner := seedCrew(f, "gd-cwM1", "gardenio-long-worker", "gd-step1", "working", "in_progress")
	longRunner.CreatedAt = time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)   // 3h runtime
	longRunner.UpdatedAt = time.Date(2026, 7, 5, 11, 55, 0, 0, time.UTC) // fresh
	shortRunner := seedCrew(f, "gd-cwM2", "gardenio-short-worker", "gd-step1", "working", "in_progress")
	shortRunner.CreatedAt = time.Date(2026, 7, 5, 11, 30, 0, 0, time.UTC) // 30m runtime
	shortRunner.UpdatedAt = time.Date(2026, 7, 5, 11, 58, 0, 0, time.UTC) // fresh
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	cockpit := s.planCrew(context.Background(), "", steps, nil)
	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]

	if cockpit.WorkingOldestAge() == "" {
		t.Fatalf("cockpit WorkingOldestAge empty, want a runtime")
	}
	if cockpit.WorkingOldestAge() != roll.WorkingOldestAge() {
		t.Errorf("index/cockpit disagree on working runtime: cockpit=%q index=%q (must agree by construction)",
			cockpit.WorkingOldestAge(), roll.WorkingOldestAge())
	}
}

// --- render: the facet lands on the cockpit roster chip + tooltip --------------

// TestPlanDetail_WorkingRuntimeFacet proves the rendered cockpit crew roster folds
// the longest-running crew's runtime onto the "N working" chip (a lighter tabular
// age span) and into the chip's hover tooltip, calm (no stale/red class).
func TestPlanDetail_WorkingRuntimeFacet(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	c := seedCrew(f, "gd-crewCW", "gardenio-crew-cw", "gd-step1", "working", "in_progress")
	c.CreatedAt = time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)   // 4h runtime
	c.UpdatedAt = time.Date(2026, 7, 5, 11, 55, 0, 0, time.UTC) // fresh heartbeat
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The working chip folds a lighter runtime-age span after the "1 working" count.
	if !strings.Contains(body, `<span class="plan-crew-count-working-age">· 4h</span>`) {
		t.Errorf("expected the cockpit working chip to fold the longest-running crew's runtime ('· 4h') as a lighter age span")
	}
	// The chip tooltip names the runtime ("longest running 4h").
	if !strings.Contains(body, "longest running 4h") {
		t.Errorf("expected the cockpit crew chip tooltip to name the longest-running crew's runtime")
	}
}

// TestPlanDetail_WorkingRuntimeFacet_NoneWithoutWorking proves a plan with NO
// working crew (only finished) renders no working-runtime span on the cockpit —
// the facet is gated on the live count, so nothing working adds no ink.
func TestPlanDetail_WorkingRuntimeFacet_NoneWithoutWorking(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-crewCD", "gardenio-crew-cd", "gd-step1", "done", "closed")
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `plan-crew-count-working-age">`) {
		t.Errorf("a plan with no working crew must not render a cockpit working-runtime age span")
	}
}
