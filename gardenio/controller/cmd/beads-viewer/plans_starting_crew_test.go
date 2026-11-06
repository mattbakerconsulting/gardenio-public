package main

// plans_starting_crew_test.go — (ka-0000005352): the "(K starting)" honest
// sub-qualifier on the "· M working" crew facet. A crew in the "spawning"
// agent_state has been dispatched and its pod is BOOTING, but it has not begun
// grinding its step yet. spawning ∈ agentStateAlive, so classifyCrewBead already
// counts it live — it rides the green pulse + face + Working count exactly like a
// grinding crew. Before this the "· M working" facet read a confident green count
// identical whether the M crew were up-and-working or still coming up: the LAST
// alive sub-state that masqueraded as full "working", the active twin of iter-31's
// queued≠done and iter-42's stale≠working honesty splits.
//
// The fix is a DISPLAY-ONLY subset counter (Starting ≤ Working): HasLive, the
// faces, and the stall-exemption are all untouched (a booting crew IS live and IS
// watching the plan, so it must keep exempting a false gardener-state stall). It
// only lets the working facet tell the honest sub-truth — "· 2 working (1
// starting)". Gated 0<Starting<Working: an all-spawning set adds no qualifier (the
// bare count already reads "just started"), a set with none reads exactly as
// before. Calm/muted — a booting crew is context, not an alarm. Index + cockpit
// read the SAME alive bucket + raw agent_state, so their qualifier agrees by
// construction (advice ka-0000005924).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- unit: the rollup counts spawning crew as a subset of Working --------------

// TestPlanCrewBulk_StartingSubsetOfWorking proves a spawning crew is counted BOTH
// as Working (it is live) AND as Starting (its pod is still booting), so Starting
// is a strict subset — and HasStarting fires when some-but-not-all live crew are
// starting.
func TestPlanCrewBulk_StartingSubsetOfWorking(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	// One crew grinding, one still booting — both fresh, both alive.
	seedCrew(f, "gd-wG", "gardenio-grinding", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-wB", "gardenio-booting", "gd-step1", "spawning", "in_progress")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Working != 2 {
		t.Fatalf("Working = %d, want 2 (both spawning + working are live)", roll.Working)
	}
	if roll.Starting != 1 {
		t.Errorf("Starting = %d, want 1 (only the spawning crew is still booting)", roll.Starting)
	}
	if !roll.HasStarting() {
		t.Errorf("HasStarting() = false, want true (1 of 2 live crew is starting)")
	}
	// A booting crew is still live — HasLive must stay true so the stall-exemption
	// and green pulse are untouched.
	if !roll.HasLive() {
		t.Errorf("HasLive() = false, want true (a booting crew is still live)")
	}
	// No new fan-out: Starting is folded off the SAME hydrated beads.
	if f.batchGetDepsCalls != 1 || f.batchGetBeadsCalls != 1 {
		t.Errorf("fan-out = %d deps + %d beads, want 1 + 1 (the starting subset adds no daemon call)", f.batchGetDepsCalls, f.batchGetBeadsCalls)
	}
}

// TestPlanCrewBulk_AllStartingNoQualifier proves that when EVERY live crew is
// spawning (Starting == Working) the qualifier does NOT fire — the bare "· K
// working" already reads "just started", so a "(K starting)" would be pure
// redundant ink. This is the render-only discrimination discipline (iter-60..64).
func TestPlanCrewBulk_AllStartingNoQualifier(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-wB1", "gardenio-boot1", "gd-step1", "spawning", "in_progress")
	seedCrew(f, "gd-wB2", "gardenio-boot2", "gd-step1", "spawning", "in_progress")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Working != 2 || roll.Starting != 2 {
		t.Fatalf("Working=%d Starting=%d, want 2/2 (both booting)", roll.Working, roll.Starting)
	}
	if roll.HasStarting() {
		t.Errorf("HasStarting() = true, want false (all-spawning set adds no qualifier)")
	}
}

// TestPlanCrewBulk_NoStartingWhenNoneSpawning proves a plan whose live crew are
// all grinding carries Starting==0 and no qualifier — the facet reads exactly as
// before.
func TestPlanCrewBulk_NoStartingWhenNoneSpawning(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-wG1", "gardenio-grind1", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-wG2", "gardenio-grind2", "gd-step1", "working", "in_progress")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Starting != 0 {
		t.Errorf("Starting = %d, want 0 (no crew is spawning)", roll.Starting)
	}
	if roll.HasStarting() {
		t.Errorf("HasStarting() = true, want false")
	}
}

// TestPlanCrewBulk_StartingClearedOnTerminal proves a terminal plan folds the
// booting crew into Done and zeroes Starting — a closed plan has no crew still
// coming up.
func TestPlanCrewBulk_StartingClearedOnTerminal(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed"
	seedCrew(f, "gd-wG", "gardenio-grinding", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-wB", "gardenio-booting", "gd-step1", "spawning", "in_progress")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	reconciled := roll.reconcileTerminal("closed")
	if reconciled.Working != 0 {
		t.Errorf("reconciled Working = %d, want 0 (terminal folds working→done)", reconciled.Working)
	}
	if reconciled.Starting != 0 {
		t.Errorf("reconciled Starting = %d, want 0 (a done plan has no booting crew)", reconciled.Starting)
	}
	if reconciled.HasStarting() {
		t.Errorf("reconciled HasStarting() = true, want false")
	}
}

// --- unit: index ↔ cockpit agree by construction -------------------------------

// TestStartingCount_IndexCockpitAgree proves the index rollup's Starting and the
// cockpit roster's Starting classify the SAME crew set identically — the
// two-derivations-must-agree invariant (advice ka-0000005924).
func TestStartingCount_IndexCockpitAgree(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-wG", "gardenio-grinding", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-wB", "gardenio-booting", "gd-step1", "spawning", "in_progress")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	steps := []planStepView{{ID: "gd-step1", Status: "in_progress"}}
	info := s.planCrew(context.Background(), "", steps, nil)

	if roll.Starting != info.Starting {
		t.Errorf("index Starting = %d, cockpit Starting = %d — the two surfaces must agree", roll.Starting, info.Starting)
	}
	if roll.HasStarting() != info.HasStarting() {
		t.Errorf("index HasStarting = %v, cockpit HasStarting = %v — must agree", roll.HasStarting(), info.HasStarting())
	}
}

// --- render: the qualifier lands on the index card -----------------------------

// TestPlansIndex_StartingQualifier proves the rendered index card folds the calm
// "(K starting)" qualifier onto the "· M working" facet and names it in the chip
// tooltip when some-but-not-all live crew are booting.
func TestPlansIndex_StartingQualifier(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewG", "gardenio-crew-g", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "spawning", "in_progress")
	mux := mkServerWithNow(t, f, func() time.Time { return crewFixtureNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<span class="plan-card-crew-starting" aria-label="1 still starting up">(1 starting)</span>`) {
		t.Errorf("expected the working facet to fold a calm '(1 starting)' qualifier for the booting crew")
	}
	// The facet still reads "2 working" — Starting does NOT subtract from the live count.
	if !strings.Contains(body, "· 2 working") {
		t.Errorf("expected the facet to still read '· 2 working' (Starting is a subset, not a subtraction)")
	}
}

// TestPlansIndex_StartingQualifier_NoneWhenAllGrinding proves a plan whose live
// crew are all grinding renders NO starting qualifier — the facet reads exactly as
// before.
func TestPlansIndex_StartingQualifier_NoneWhenAllGrinding(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewG1", "gardenio-crew-g1", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-crewG2", "gardenio-crew-g2", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, func() time.Time { return crewFixtureNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Match the rendered SPAN, not the CSS rule of the same class name in <style>.
	if strings.Contains(w.Body.String(), `<span class="plan-card-crew-starting"`) {
		t.Errorf("a plan with no booting crew must not render a starting qualifier")
	}
}

// --- render: the qualifier lands on the cockpit roster header ------------------

// TestPlanDetail_StartingQualifier proves the cockpit crew-roster header folds the
// same "(K starting)" qualifier onto its "N working" chip — the cockpit twin of
// the index card facet, so the two surfaces read the same booting count.
func TestPlanDetail_StartingQualifier(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewG", "gardenio-crew-g", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-crewB", "gardenio-crew-b", "gd-step1", "spawning", "in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<span class="plan-crew-count-starting">(1 starting)</span>`) {
		t.Errorf("expected the cockpit working chip to fold a '(1 starting)' qualifier")
	}
	// The header still reads "2 working" — Starting is a subset, not a subtraction.
	if !strings.Contains(body, "2 working") {
		t.Errorf("expected the cockpit header to still read '2 working'")
	}
}
