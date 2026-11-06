package main

// plans_epic_crew_test.go — (ka-0000005352) surface crew `assigned` DIRECTLY to
// the plan EPIC, not just to a rendered step. This is the delivery-drive-gardener
// pattern: instead of the gardener dispatching a crew onto a discrete step, the
// crew anchor to the epic itself and self-pick the next iteration. Before this the
// crew walk (planCrewBulk on the index, planCrew on the cockpit) swept only the
// rendered STEPS' reverse-`assigned` edges, so an epic-anchored crew — however
// live — was 100% invisible on the card: the card read "N crew" (or none) with no
// live-dot / working facet / faces, even while a crew was grinding the plan right
// now. Measured live on the prod board (2026-08-12): this very plan showed "28
// crew" with no working facet while its own crew (role=crew, assigned→the epic,
// agent_state=working) was working.
//
// The load-bearing TRAP: the plan-GARDENER is ALSO `assigned` to the epic (measured
// on the live board, agents carrying an `assigned` edge INTO a plan epic are
// exactly role=crew or role=plan-gardener — nothing else). So the widened walk MUST
// filter the gardener (isPlanGardenerAgent) or it would miscount the gardener as a
// working crew AND falsely exempt a gardener-state stall (HasLive). These tests pin
// both halves: the epic-anchored crew IS surfaced, the epic-anchored gardener is
// NOT, and the index chip and cockpit roster agree by construction.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedEpicGardener wires a plan-gardener agent bead `assigned` to the epic (the
// production shape: role LABEL role:plan-gardener + the assigned reverse-dep edge
// onto the epic), so a test can prove the widened crew walk filters it out.
func seedEpicGardener(f *fakeDaemon, agentID, name, epicID, state string) {
	seedAssistantBead(f, agentID, name, epicID, state, crewFixtureNow)
}

// --- unit: planCrewBulk (index) surfaces epic-anchored crew, filters gardeners ---

// TestPlanCrewBulk_EpicAnchoredCrewSurfaced proves a crew `assigned` to the epic
// (NOT a step) is counted + tallied working, and the co-assigned plan-gardener is
// filtered — so the tally is crew-only.
func TestPlanCrewBulk_EpicAnchoredCrewSurfaced(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	// A live crew anchored to the EPIC (the epic id in the step slot → the reverse
	// `assigned` edge is written onto the epic, exactly like the delivery-drive
	// pattern), plus the plan-gardener also assigned to the epic.
	seedCrew(f, "gd-epicCrew", "gardenio-datadensity-vbj", "gd-plan1", "working", "in_progress")
	seedEpicGardener(f, "gd-epicCap", "gardenio-plan-gardener-gd-plan1", "gd-plan1", "working")
	s := crewTestServer(f)

	// The card walks the rendered steps; the epic id is folded in by planCrewBulk.
	roll := s.planCrewBulk(context.Background(),
		map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]

	if roll.Total != 1 {
		t.Fatalf("Total = %d, want 1 (the epic-anchored crew; the gardener is filtered)", roll.Total)
	}
	if roll.Working != 1 {
		t.Errorf("Working = %d, want 1 (the epic-anchored crew is live)", roll.Working)
	}
	if !roll.HasLive() {
		t.Error("HasLive() = false, want true — an epic-anchored working crew makes the plan live")
	}
	if len(roll.Faces) != 1 || roll.Faces[0].ID != "gd-epicCrew" {
		t.Errorf("Faces = %+v, want exactly the epic-anchored crew face", roll.Faces)
	}
}

// TestPlanCrewBulk_EpicGardenerNeverCountsAsCrew proves an epic that has ONLY a
// plan-gardener assigned (no crew) renders no crew chip — the gardener is never
// miscounted as crew.
func TestPlanCrewBulk_EpicGardenerNeverCountsAsCrew(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedEpicGardener(f, "gd-epicCap", "gardenio-plan-gardener-gd-plan1", "gd-plan1", "working")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(),
		map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]

	if roll.Total != 0 {
		t.Errorf("Total = %d, want 0 — a plan with only its gardener assigned to the epic has no crew", roll.Total)
	}
	if roll.Any() {
		t.Error("Any() = true, want false — no crew chip should render for a gardener-only epic")
	}
}

// TestPlanCrewBulk_EpicAndStepCrewBothCounted proves the widened walk is ADDITIVE:
// a step-anchored crew and an epic-anchored crew both count (deduped), so a plan
// that mixes the two dispatch patterns reads the full tally.
func TestPlanCrewBulk_EpicAndStepCrewBothCounted(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-stepCrew", "gardenio-step-crew", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-epicCrew", "gardenio-epic-crew", "gd-plan1", "done", "closed")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(),
		map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]

	if roll.Total != 2 {
		t.Fatalf("Total = %d, want 2 (one step-anchored + one epic-anchored crew)", roll.Total)
	}
	if roll.Working != 1 || roll.Done != 1 {
		t.Errorf("working/done = %d/%d, want 1/1", roll.Working, roll.Done)
	}
}

// --- unit: planCrew (cockpit) mirrors the index, epicID-threaded ---------------

// TestPlanCrew_EpicAnchoredCrewSurfaced proves the cockpit roster surfaces the
// epic-anchored crew when the epicID is threaded, and filters the gardener — the
// cockpit twin of the index tally, so the two agree by construction.
func TestPlanCrew_EpicAnchoredCrewSurfaced(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-epicCrew", "gardenio-datadensity-vbj", "gd-plan1", "working", "in_progress")
	seedEpicGardener(f, "gd-epicCap", "gardenio-plan-gardener-gd-plan1", "gd-plan1", "working")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "gd-plan1", steps, nil)

	if info.Total != 1 {
		t.Fatalf("Total = %d, want 1 (epic-anchored crew; gardener filtered)", info.Total)
	}
	if info.Working != 1 || !info.HasLive() {
		t.Errorf("Working=%d HasLive=%v, want 1/true", info.Working, info.HasLive())
	}
	if len(info.Members) != 1 || info.Members[0].ID != "gd-epicCrew" {
		t.Errorf("Members = %+v, want exactly the epic-anchored crew", info.Members)
	}
}

// TestPlanCrew_EmptyEpicIDStepsOnly proves that with an empty epicID the cockpit
// walk covers steps ONLY — byte-identical to the pre-iteration behavior, so a
// caller with no epic id in hand is never regressed and an epic-anchored crew is
// simply not surfaced there.
func TestPlanCrew_EmptyEpicIDStepsOnly(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-stepCrew", "gardenio-step-crew", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-epicCrew", "gardenio-epic-crew", "gd-plan1", "working", "in_progress")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)

	// Only the step-anchored crew is walked; the epic edge is not swept.
	if info.Total != 1 || info.Members[0].ID != "gd-stepCrew" {
		t.Errorf("Total=%d Members=%+v, want only the step-anchored crew (epicID empty ⇒ steps-only)", info.Total, info.Members)
	}
}

// TestPlanCrew_IndexCockpitAgreeOnEpicCrew proves the two derivations of the same
// quantity match (advice ka-0000005924): the index rollup and the cockpit roster
// count the SAME epic-anchored crew and filter the SAME gardener.
func TestPlanCrew_IndexCockpitAgreeOnEpicCrew(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-epicCrew1", "gardenio-a", "gd-plan1", "working", "in_progress")
	seedCrew(f, "gd-epicCrew2", "gardenio-b", "gd-plan1", "done", "closed")
	seedEpicGardener(f, "gd-epicCap", "gardenio-plan-gardener-gd-plan1", "gd-plan1", "working")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(),
		map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "gd-plan1", steps, nil)

	if roll.Total != info.Total || roll.Working != info.Working || roll.Done != info.Done {
		t.Errorf("index rollup {T=%d W=%d D=%d} != cockpit roster {T=%d W=%d D=%d}",
			roll.Total, roll.Working, roll.Done, info.Total, info.Working, info.Done)
	}
	if roll.Total != 2 {
		t.Errorf("Total = %d, want 2 (both crew; gardener filtered on both surfaces)", roll.Total)
	}
}

// --- served index: the live-crew chip renders for an epic-anchored crew --------

// TestPlansIndex_EpicAnchoredCrewRendersLiveChip drives the whole served /plans
// index and asserts an epic-anchored working crew lights the live-dot + working
// facet on the card — the operator-visible payoff (acceptance criterion #2).
func TestPlansIndex_EpicAnchoredCrewRendersLiveChip(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-epicCrew", "gardenio-datadensity-vbj", "gd-plan1", "working", "in_progress")
	seedEpicGardener(f, "gd-epicCap", "gardenio-plan-gardener-gd-plan1", "gd-plan1", "working")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-card-crew-live") {
		t.Error("expected the live-dot class on a plan with an epic-anchored working crew")
	}
	if !strings.Contains(body, "1 working") {
		t.Error("expected the '· 1 working' facet for the epic-anchored crew")
	}
	// The gardener must not inflate the count — exactly one crew.
	if !strings.Contains(body, "1 crew") {
		t.Errorf("expected '1 crew' (gardener filtered); body crew chip missing it")
	}
}
