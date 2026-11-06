package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// TestLooksLikePhaseTitle covers the "Phase N" title detector: the numbered
// forms agents reach for when they mean to mint a phase sub-epic, and the
// non-matching forms that must NOT trip the hint (a bare "Phase", a step that
// merely mentions a phase mid-title, an unrelated task).
func TestLooksLikePhaseTitle(t *testing.T) {
	match := []string{
		"Phase 1",
		"Phase 2: Research & design",
		"phase 3 — implementation",
		"  Phase 4",
		"PHASE 5 validation",
		"Phase-6",
		"Phase #7",
		"Phase.8 cleanup",
	}
	for _, title := range match {
		if !looksLikePhaseTitle(title) {
			t.Errorf("looksLikePhaseTitle(%q) = false, want true", title)
		}
	}
	noMatch := []string{
		"Phase",                     // no number — not the "Phase N" convention
		"Phased rollout of the API", // "Phase" is not the leading token/word
		"Rework phase 2 handling",   // phase mentioned mid-title, not a declaration
		"Multiphase migration",      // no word boundary before "phase"
		"Wire the route",
		"",
	}
	for _, title := range noMatch {
		if looksLikePhaseTitle(title) {
			t.Errorf("looksLikePhaseTitle(%q) = true, want false", title)
		}
	}
}

// TestMisfiledPhaseSteps proves the helper flags only the flat steps that look
// like phase declarations, in input order, and returns nil when none do.
func TestMisfiledPhaseSteps(t *testing.T) {
	steps := []planStepView{
		{ID: "gd-a", Title: "Phase 1: research"},
		{ID: "gd-b", Title: "Wire the route"},
		{ID: "gd-c", Title: "Phase 2"},
	}
	got := misfiledPhaseSteps(steps)
	want := []string{"Phase 1: research", "Phase 2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("misfiledPhaseSteps = %v, want %v", got, want)
	}

	// A plan with no phase-looking steps yields nil (the hint self-suppresses).
	if got := misfiledPhaseSteps([]planStepView{{ID: "gd-x", Title: "Do the thing"}}); got != nil {
		t.Errorf("misfiledPhaseSteps(clean) = %v, want nil", got)
	}
}

// seedMisfiledPhasePlanFake builds a plan (no phase_order) whose direct task
// children are titled "Phase 1"/"Phase 2" — the exact authoring mistake Matt
// reported (thread ka-0000004243): phases created as flat tasks. Plus one
// ordinary step, which must NOT be flagged.
func seedMisfiledPhasePlanFake() *fakeDaemon {
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan: mislabeled phases", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Description: "Phases authored as tasks.",
		// NO phase_order — the whole point is these tasks were never registered.
	}
	p1 := &beadsapi.BeadDetail{ID: "gd-p1", Title: "Phase 1: Research", Kind: "issue", Type: "task", Status: "open"}
	p2 := &beadsapi.BeadDetail{ID: "gd-p2", Title: "Phase 2: Build", Kind: "issue", Type: "task", Status: "open"}
	ok := &beadsapi.BeadDetail{ID: "gd-ok", Title: "Wire the route", Kind: "issue", Type: "task", Status: "open"}
	for _, b := range []*beadsapi.BeadDetail{plan, p1, p2, ok} {
		f.beads[b.ID] = b
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {
			{BeadID: "gd-p1", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-p2", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-ok", DependsOnID: "gd-plan1", Type: "parent-child"},
		},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	f.epicSubtreeCount = map[string]int{"gd-plan1": 3, "gd-plan1|closed": 0}
	return f
}

// TestHandlePlanDetail_PhaseMislabelHint proves the /plans cockpit surfaces the
// phase-mislabel hint — naming the offending steps — when a plan has flat
// "Phase N" task children and no phase_order (ka-0000002630). It renders with no
// OIDC roles on the request, so viewerCanOperate is true (dev-bypass / internal
// caller) and the OPERATOR branch shows: the ＋ New phase GUI button, NOT a shell
// command (ka-0000004472 — never tell an operator in the viewer to run kd/gb).
func TestHandlePlanDetail_PhaseMislabelHint(t *testing.T) {
	mux := mkServer(t, seedMisfiledPhasePlanFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"plan-phase-hint",              // the banner renders
		"These steps look like phases", // its lead-in
		"Phase 1: Research",            // names the offending step
		"Phase 2: Build",               // and the second
		"＋ New phase",                  // operator gets the GUI action…
	} {
		if !strings.Contains(body, want) {
			t.Errorf("phase-mislabel hint missing %q", want)
		}
	}
	// …and NEVER a `gb`/`kd` shell command inside the hint (ka-0000004472). Scope
	// the assertion to the hint paragraph so an unrelated command chip elsewhere
	// on the page can't false-fail it.
	hintStart := strings.Index(body, "These steps look like phases")
	if hintStart < 0 {
		t.Fatal("phase-mislabel hint paragraph not found")
	}
	hintEnd := strings.Index(body[hintStart:], "</p>")
	if hintEnd < 0 {
		hintEnd = len(body) - hintStart
	}
	hint := body[hintStart : hintStart+hintEnd]
	if strings.Contains(hint, "gb plan add-phase") {
		t.Error("operator-facing hint must not tell the operator to run a `gb` shell command")
	}
	// The ordinary step must NOT be listed in the hint.
	if strings.Contains(hint, "Wire the route") {
		t.Error("hint should not list the ordinary (non-phase) step 'Wire the route'")
	}
}

// TestHandlePlanDetail_PhaseMislabelHintNonOperator proves that a NON-operator
// viewer (an agent / headless caller, identified by an OIDC role below the
// operator minimum) still gets the actionable `gb plan add-phase` command in the
// hint — the command IS the path for a caller who isn't in the browser GUI
// (ka-0000004472). This is the complement of the operator branch above.
func TestHandlePlanDetail_PhaseMislabelHintNonOperator(t *testing.T) {
	mux := mkServer(t, seedMisfiledPhasePlanFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "gb plan add-phase gd-plan1") {
		t.Error("non-operator hint should point at `gb plan add-phase`, scoped to this plan")
	}
	if strings.Contains(body, "＋ New phase") {
		t.Error("non-operator must not be shown the operator-only ＋ New phase button")
	}
}

// TestHandlePlanDetail_NoPhaseHintWhenClean proves a plan whose steps are NOT
// titled "Phase N" shows no hint — the banner self-suppresses (no false
// positive on an ordinary plan).
func TestHandlePlanDetail_NoPhaseHintWhenClean(t *testing.T) {
	mux := mkServer(t, seedPlansFake()) // its one step is "Wire the route"
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Assert on the banner's lead-in text, not the class name — the CSS rule
	// `.plan-phase-hint` lives in the page's <style> block and is always present.
	if strings.Contains(w.Body.String(), "These steps look like phases") {
		t.Error("a plan with no 'Phase N' steps must not render the phase-mislabel hint")
	}
}

// TestHandlePlanDetail_NoPhaseHintWhenProperlyPhased proves that a step titled
// after a phase but ACTUALLY grouped under a phase sub-epic (in phase_order) is
// not flagged — the hint targets only the UNPHASED flat steps, so a correctly
// authored phased plan stays clean even if a phase's own step mentions "Phase".
func TestHandlePlanDetail_NoPhaseHintWhenProperlyPhased(t *testing.T) {
	f := seedPhasedPlanFake()
	// Rename a properly-phased grandchild step to a "Phase N" shape. Because it is
	// a phased step (under gd-phaseI, in phase_order), it is NOT a flat step and
	// must not trip the hint.
	f.beads["gd-i2"].Title = "Phase 2 wiring"
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "These steps look like phases") {
		t.Error("a properly-phased step named 'Phase N' must not trigger the flat-step hint")
	}
}
