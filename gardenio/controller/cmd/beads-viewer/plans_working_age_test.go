package main

// plans_working_age_test.go — (ka-0000005352): the LIVE (working) crew facet's
// RUNTIME age. The "· M working" count was the LAST live-crew facet on the /plans
// card carrying a count with no temporal signal — its siblings the queued facet
// ("· 12m", iter-32), the stuck facet ("· 6h", iter-13), and the now-step chip
// ("· 5d", iter-43) all surface HOW LONG. A crew grinding a plan for 6 hours read
// identically to one spawned 6 minutes ago. This folds the longest-running live
// crew's elapsed runtime onto the facet + the chip tooltip.
//
// The basis is RUNTIME (now - CreatedAt), NOT the last-activity age the queued /
// stuck facets use — and that difference is load-bearing. A queued/stuck crew's
// updated_at FREEZES (the reconciler re-marks an unchanged state → no PATCH → no
// bump), so now-updatedAt IS its wait. A WORKING crew is the opposite: it
// heartbeats every cycle (that is exactly what classifyCrewBead's freshness gate
// keys off — a working crew whose heartbeat froze is re-bucketed to loud "stuck"),
// so now-updatedAt would always read ~0s and carry no signal. now-CreatedAt reads
// how long the crew has been ON the plan — the live-effort twin of the calm spend
// facet. Calm-only (no stale tint): a long-running working crew is context, not an
// alarm.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- unit: the rollup carries the longest-running working crew's runtime -------

// TestPlanCrewBulk_WorkingOldestAge proves the rollup reports the RUNTIME of the
// longest-running LIVE crew (the earliest CreatedAt — it has been on the plan
// longest), NOT the newest, and ages on CreatedAt not UpdatedAt. With the clock at
// 12:00, a crew created at 08:00 has run 4h; one created at 11:00 has run 1h. The
// longest runtime (4h) is reported. Both carry a FRESH updated_at (a working crew
// heartbeats), so an updated_at-based age would wrongly read ~5m for both — this
// pins that the facet ages on runtime.
func TestPlanCrewBulk_WorkingOldestAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	longRunner := seedCrew(f, "gd-wL", "gardenio-long-worker", "gd-step1", "working", "in_progress")
	longRunner.CreatedAt = time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)   // 4h runtime
	longRunner.UpdatedAt = time.Date(2026, 7, 5, 11, 55, 0, 0, time.UTC) // fresh heartbeat (5m)
	shortRunner := seedCrew(f, "gd-wS", "gardenio-short-worker", "gd-step1", "working", "in_progress")
	shortRunner.CreatedAt = time.Date(2026, 7, 5, 11, 0, 0, 0, time.UTC)  // 1h runtime
	shortRunner.UpdatedAt = time.Date(2026, 7, 5, 11, 58, 0, 0, time.UTC) // fresh heartbeat (2m)
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Working != 2 {
		t.Fatalf("Working = %d, want 2 (two live crew)", roll.Working)
	}
	if got := roll.WorkingOldestAge(); got != "4h" {
		t.Errorf("WorkingOldestAge = %q, want %q (the LONGEST-running crew's runtime, aged on CreatedAt not UpdatedAt)", got, "4h")
	}
	// No-new-fan-out budget: the runtime is a fold over beads already in hand, so
	// the rollup still pays exactly ONE reverse assigned-edge batch + ONE agent
	// hydration batch — no per-crew round-trip for the age.
	if f.batchGetDepsCalls != 1 || f.batchGetBeadsCalls != 1 {
		t.Errorf("fan-out = %d deps + %d beads, want 1 + 1 (the working runtime adds no daemon call)", f.batchGetDepsCalls, f.batchGetBeadsCalls)
	}
}

// TestPlanCrewBulk_WorkingOldestAge_Undatable proves a working crew with no
// CreatedAt contributes no runtime, so the facet reads count-only exactly as
// before — the empty-age degrade path.
func TestPlanCrewBulk_WorkingOldestAge_Undatable(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	c := seedCrew(f, "gd-wU", "gardenio-undatable-worker", "gd-step1", "working", "in_progress")
	c.CreatedAt = time.Time{} // unset — cannot contribute a runtime
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Working != 1 {
		t.Fatalf("Working = %d, want 1", roll.Working)
	}
	if got := roll.WorkingOldestAge(); got != "" {
		t.Errorf("WorkingOldestAge = %q, want empty (an undatable working crew reads count-only)", got)
	}
}

// TestPlanCrewBulk_WorkingAge_TerminalReconcileClears proves a TERMINAL
// (closed/deferred) plan folds Working→Done and clears the runtime age — a done
// plan has no crew "working now", so the live-forward runtime must not linger. The
// zombie-working reconcile family (iter-30).
func TestPlanCrewBulk_WorkingAge_TerminalReconcileClears(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed" // terminal
	z := seedCrew(f, "gd-wZ", "gardenio-zombie-worker", "gd-step1", "working", "in_progress")
	z.CreatedAt = time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)
	z.UpdatedAt = time.Date(2026, 7, 5, 11, 55, 0, 0, time.UTC)
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	reconciled := roll.reconcileTerminal("closed")
	if reconciled.Working != 0 {
		t.Errorf("reconciled Working = %d, want 0 (terminal plan folds working→done)", reconciled.Working)
	}
	if got := reconciled.WorkingOldestAge(); got != "" {
		t.Errorf("reconciled WorkingOldestAge = %q, want empty (a done plan has no live runtime)", got)
	}
}

// --- render: the facet lands on the index card + tooltip ----------------------

// TestPlansIndex_WorkingRuntimeFacet proves the rendered index card folds the
// longest-running crew's runtime onto the "· M working" facet (a lighter tabular
// age span) and into the chip's hover tooltip, and that the facet is calm (no
// stale/red class — a working crew's runtime is context, not an alarm).
func TestPlansIndex_WorkingRuntimeFacet(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	c := seedCrew(f, "gd-crewW", "gardenio-crew-w", "gd-step1", "working", "in_progress")
	c.CreatedAt = time.Date(2026, 7, 5, 8, 0, 0, 0, time.UTC)   // 4h runtime
	c.UpdatedAt = time.Date(2026, 7, 5, 11, 55, 0, 0, time.UTC) // fresh heartbeat
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The working facet folds a lighter runtime-age span after the "1 working" count.
	if !strings.Contains(body, `<span class="plan-card-crew-working-age">4h</span>`) {
		t.Errorf("expected the working facet to fold the longest-running crew's runtime ('4h') as a lighter age span")
	}
	// The chip tooltip names the runtime ("longest running 4h").
	if !strings.Contains(body, "longest running 4h") {
		t.Errorf("expected the crew chip tooltip to name the longest-running crew's runtime")
	}
}

// TestPlansIndex_WorkingRuntimeFacet_NoneWithoutWorking proves a plan with NO
// working crew (only finished) renders no working-runtime span — the facet is
// gated on the live count, so a plan with nothing working adds no ink.
func TestPlansIndex_WorkingRuntimeFacet_NoneWithoutWorking(t *testing.T) {
	f := seedPlansFake()
	seedCrew(f, "gd-crewD", "gardenio-crew-d", "gd-step1", "done", "closed")
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `plan-card-crew-working-age">`) {
		t.Errorf("a plan with no working crew must not render a working-runtime age span")
	}
}
