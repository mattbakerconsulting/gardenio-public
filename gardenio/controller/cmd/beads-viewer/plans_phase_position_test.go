package main

// plans_phase_position_test.go — iter-58 (ka-0000005352): the multi-phase plan's
// PHASE POSITION facet ("· phase 2/5") on the /plans card progress row. The
// segmented bar + "N/N steps · X%" label answer "how far through the TASKS" and
// the now/next chip names ONE step, but neither answers WHICH PHASE the plan is
// on — the coarse lifecycle milestone the mission names. Measured live
// (2026-08-11): 3 of 16 active plans are multi-phase (4-5 phases via
// fields.phase_order) yet the card rendered the phase STRUCTURE without ever
// saying which phase is in flight.
//
// planPhasePosition is a PURE fold over the Grid already on the row (buildPlanGrid),
// so it costs zero extra daemon call and can never disagree with the phase rows /
// progress rollup the card renders off the same walk.

import (
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- unit: planPhasePosition over synthetic grids -----------------------------

// TestPlanPhasePosition_CurrentIsEarliestUnfinished pins the core rule: the
// CURRENT phase is the earliest (in phase_order) phase still carrying a
// non-closed square — the phase work is in or will next reach. Here phase 1 is
// fully closed and phase 2 has an in_progress step, so the plan reads "phase 2/3".
func TestPlanPhasePosition_CurrentIsEarliestUnfinished(t *testing.T) {
	grid := []planGridPhase{
		{Title: "Research", IsPhase: true, Squares: []planGridSquare{
			{ID: "gd-r1", Status: "closed"},
			{ID: "gd-r2", Status: "closed"},
		}},
		{Title: "Build", IsPhase: true, Squares: []planGridSquare{
			{ID: "gd-b1", Status: "in_progress"},
			{ID: "gd-b2", Status: "open"},
		}},
		{Title: "Verify", IsPhase: true, Squares: []planGridSquare{
			{ID: "gd-v1", Status: "open"},
		}},
	}
	cur, total := planPhasePosition(grid)
	if cur != 2 || total != 3 {
		t.Fatalf("planPhasePosition = (%d, %d), want (2, 3) — earliest unfinished phase of 3", cur, total)
	}
}

// TestPlanPhasePosition_FirstPhaseInFlight proves a plan whose FIRST phase still
// has open work reads "phase 1/M" — work has not yet cleared phase 1.
func TestPlanPhasePosition_FirstPhaseInFlight(t *testing.T) {
	grid := []planGridPhase{
		{Title: "A", IsPhase: true, Squares: []planGridSquare{{ID: "gd-a1", Status: "open"}}},
		{Title: "B", IsPhase: true, Squares: []planGridSquare{{ID: "gd-b1", Status: "open"}}},
	}
	cur, total := planPhasePosition(grid)
	if cur != 1 || total != 2 {
		t.Fatalf("planPhasePosition = (%d, %d), want (1, 2)", cur, total)
	}
}

// TestPlanPhasePosition_AllDoneClampsToTotal proves that when EVERY phase is
// fully closed the position clamps to total — a done 4-phase plan reads
// "phase 4/4", never "phase 5/4".
func TestPlanPhasePosition_AllDoneClampsToTotal(t *testing.T) {
	grid := []planGridPhase{
		{Title: "A", IsPhase: true, Squares: []planGridSquare{{ID: "gd-a1", Status: "closed"}}},
		{Title: "B", IsPhase: true, Squares: []planGridSquare{{ID: "gd-b1", Status: "closed"}}},
		{Title: "C", IsPhase: true, Squares: []planGridSquare{{ID: "gd-c1", Status: "closed"}}},
		{Title: "D", IsPhase: true, Squares: []planGridSquare{{ID: "gd-d1", Status: "closed"}}},
	}
	cur, total := planPhasePosition(grid)
	if cur != 4 || total != 4 {
		t.Fatalf("planPhasePosition = (%d, %d), want (4, 4) — all-done clamps to last phase", cur, total)
	}
}

// TestPlanPhasePosition_EmptyPhaseIsCurrent proves an EMPTY phase (a phase
// sub-epic with no issue children yet — buildPlanGrid still emits it as a
// labeled square-less row) is treated as NOT-done, so it becomes the current
// phase rather than being skipped. Phase 1 done, phase 2 empty → "phase 2/3":
// the plan has reached phase 2 but it has no steps yet.
func TestPlanPhasePosition_EmptyPhaseIsCurrent(t *testing.T) {
	grid := []planGridPhase{
		{Title: "A", IsPhase: true, Squares: []planGridSquare{{ID: "gd-a1", Status: "closed"}}},
		{Title: "B (empty)", IsPhase: true, Squares: nil},
		{Title: "C", IsPhase: true, Squares: []planGridSquare{{ID: "gd-c1", Status: "open"}}},
	}
	cur, total := planPhasePosition(grid)
	if cur != 2 || total != 3 {
		t.Fatalf("planPhasePosition = (%d, %d), want (2, 3) — an empty upcoming phase is the current one", cur, total)
	}
}

// TestPlanPhasePosition_FlatTrailerNotCounted proves the unphased "Other" flat
// trailer (IsPhase false) is NOT counted as a milestone phase: a plan with two
// real phases plus a leftover flat row reads "phase X/2", never /3.
func TestPlanPhasePosition_FlatTrailerNotCounted(t *testing.T) {
	grid := []planGridPhase{
		{Title: "A", IsPhase: true, Squares: []planGridSquare{{ID: "gd-a1", Status: "closed"}}},
		{Title: "B", IsPhase: true, Squares: []planGridSquare{{ID: "gd-b1", Status: "in_progress"}}},
		{Title: "Other", IsPhase: false, Squares: []planGridSquare{{ID: "gd-f1", Status: "open"}}},
	}
	cur, total := planPhasePosition(grid)
	if cur != 2 || total != 2 {
		t.Fatalf("planPhasePosition = (%d, %d), want (2, 2) — the flat 'Other' trailer is not a phase", cur, total)
	}
}

// TestPlanPhasePosition_SinglePhaseNoFacet proves a plan with a single real
// phase yields (0, 0) — the no-facet signal. One phase is not a milestone
// sequence, so the card shows nothing rather than a pointless "phase 1/1".
func TestPlanPhasePosition_SinglePhaseNoFacet(t *testing.T) {
	grid := []planGridPhase{
		{Title: "Only", IsPhase: true, Squares: []planGridSquare{{ID: "gd-o1", Status: "open"}}},
	}
	cur, total := planPhasePosition(grid)
	if cur != 0 || total != 0 {
		t.Fatalf("planPhasePosition = (%d, %d), want (0, 0) — a single-phase plan has no milestone", cur, total)
	}
}

// TestPlanPhasePosition_FlatPlanNoFacet proves a pure-flat plan (no phases, the
// single untitled IsPhase-false row) yields (0, 0).
func TestPlanPhasePosition_FlatPlanNoFacet(t *testing.T) {
	grid := []planGridPhase{
		{Title: "", IsPhase: false, Squares: []planGridSquare{
			{ID: "gd-1", Status: "open"}, {ID: "gd-2", Status: "closed"},
		}},
	}
	cur, total := planPhasePosition(grid)
	if cur != 0 || total != 0 {
		t.Fatalf("planPhasePosition = (%d, %d), want (0, 0) — a flat plan has no phases", cur, total)
	}
}

// TestPlanPhasePosition_NilGrid proves the empty/nil grid degrades to (0, 0).
func TestPlanPhasePosition_NilGrid(t *testing.T) {
	if cur, total := planPhasePosition(nil); cur != 0 || total != 0 {
		t.Fatalf("planPhasePosition(nil) = (%d, %d), want (0, 0)", cur, total)
	}
}

// --- unit: buildPlanGrid stamps IsPhase correctly -----------------------------

// TestBuildPlanGrid_IsPhaseFlag proves buildPlanGrid marks real phase rows
// IsPhase=true and the trailing unphased "Other" row IsPhase=false — the flag
// planPhasePosition keys the phase count off. A phased plan with a leftover flat
// step yields [phase(true), Other(false)].
func TestBuildPlanGrid_IsPhaseFlag(t *testing.T) {
	plan := &beadsapi.BeadDetail{ID: "gd-p", Type: "epic", Fields: map[string]string{
		"phase_order": `["gd-ph1"]`,
	}}
	direct := map[string]*beadsapi.BeadDetail{
		"gd-ph1": {ID: "gd-ph1", Type: "epic", Status: "in_progress", Title: "Research"},
		"gd-f1":  {ID: "gd-f1", Type: "task", Status: "open", Title: "Loose end"},
	}
	grand := map[string]*beadsapi.BeadDetail{
		"gd-s1": {ID: "gd-s1", Type: "task", Status: "closed", Title: "Scope"},
	}
	grid := buildPlanGrid(plan, []string{"gd-ph1"}, []string{"gd-ph1", "gd-f1"},
		map[string][]string{"gd-ph1": {"gd-s1"}}, direct, grand)

	if len(grid) != 2 {
		t.Fatalf("grid has %d rows, want 2 (Research phase + Other flat)", len(grid))
	}
	if !grid[0].IsPhase {
		t.Errorf("grid[0] (Research) IsPhase = false, want true")
	}
	if grid[1].IsPhase {
		t.Errorf("grid[1] (Other flat trailer) IsPhase = true, want false")
	}
}

// --- render: the facet on the served /plans index -----------------------------

// TestPlansIndex_PhasePositionRendersForMultiPhase seeds a real two-phase plan
// (phase_order with two phase sub-epics, phase 1 fully done + phase 2 in flight)
// and asserts the served card renders "· phase 2/2". Fail-without: dropping the
// row wiring or the template gate reds this.
func TestPlansIndex_PhasePositionRendersForMultiPhase(t *testing.T) {
	f := seedMultiPhasePlan("gd-plan1", []string{"closed", "in_progress"})
	card := phasePlanCardHTML(t, f)
	if !strings.Contains(card, "· phase 2/2") {
		t.Fatalf("multi-phase card missing '· phase 2/2' facet:\n%s", card)
	}
	if !strings.Contains(card, "plan-progress-phase") {
		t.Errorf("phase facet missing its .plan-progress-phase class")
	}
}

// TestPlansIndex_PhasePositionAbsentForSinglePhase proves a plan with ONE phase
// renders NO phase facet — the self-suppress gate (PhaseTotal >= 2).
func TestPlansIndex_PhasePositionAbsentForSinglePhase(t *testing.T) {
	f := seedMultiPhasePlan("gd-plan1", []string{"in_progress"})
	card := phasePlanCardHTML(t, f)
	if strings.Contains(card, "plan-progress-phase") {
		t.Fatalf("single-phase card unexpectedly renders the phase facet:\n%s", card)
	}
}

// --- seed helper --------------------------------------------------------------

// seedMultiPhasePlan builds a fresh fake serving ONE plan epic with len(phaseStatuses)
// phase sub-epics in fields.phase_order, each holding exactly one task step whose
// status matches the corresponding phaseStatuses entry (so the phase reads done when
// "closed", in flight otherwise). This drives the two-tier reverse-deps walk
// planIndexStepData performs, so the served card's Grid — and thus its phase-position
// facet — is derived exactly as in production.
func seedMultiPhasePlan(planID string, phaseStatuses []string) *fakeDaemon {
	f := &fakeDaemon{
		beads:   map[string]*beadsapi.BeadDetail{},
		revDeps: map[string][]beadsapi.Dependency{},
	}
	phaseIDs := make([]string, len(phaseStatuses))
	for i := range phaseStatuses {
		phaseIDs[i] = fmt.Sprintf("%s-ph%d", planID, i+1)
	}
	order := "["
	for i, pid := range phaseIDs {
		if i > 0 {
			order += ","
		}
		order += fmt.Sprintf("%q", pid)
	}
	order += "]"
	plan := &beadsapi.BeadDetail{
		ID: planID, Title: "Plan " + planID, Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Fields:    map[string]string{"phase_order": order},
		UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	f.beads[planID] = plan
	for i, pid := range phaseIDs {
		phase := &beadsapi.BeadDetail{
			ID: pid, Title: fmt.Sprintf("Phase %d", i+1), Kind: "issue", Type: "epic",
			Status: "in_progress", UpdatedAt: time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC),
		}
		f.beads[pid] = phase
		f.revDeps[planID] = append(f.revDeps[planID],
			beadsapi.Dependency{BeadID: pid, DependsOnID: planID, Type: "parent-child"})
		// One step per phase, carrying the phase's status so the grid square
		// (and thus phaseSquaresAllClosed) reflects done vs in-flight.
		stepID := pid + "-step"
		f.beads[stepID] = &beadsapi.BeadDetail{
			ID: stepID, Title: "Step " + stepID, Kind: "issue", Type: "task",
			Status: phaseStatuses[i], UpdatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		}
		f.revDeps[pid] = append(f.revDeps[pid],
			beadsapi.Dependency{BeadID: stepID, DependsOnID: pid, Type: "parent-child"})
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	return f
}

func phasePlanCardHTML(t *testing.T, f *fakeDaemon) string {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) }
	mux := mkServerWithNow(t, f, now)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != 200 {
		t.Fatalf("GET /plans: expected 200, got %d", w.Code)
	}
	re := regexp.MustCompile(`(?s)<li class="plan-card".*?</li>`)
	m := re.FindString(w.Body.String())
	if m == "" {
		t.Fatalf("no plan-card in body:\n%s", w.Body.String())
	}
	return m
}
