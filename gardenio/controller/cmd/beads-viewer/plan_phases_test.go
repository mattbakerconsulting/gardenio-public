package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// seedPhasedPlanFake builds a plan epic with two ORDERED phase sub-epics
// (research then implementation, via fields.phase_order), each with task
// children, PLUS one unphased direct step and a narrative bundle. It exercises
// the whole phase render path: membership-by-phase_order, per-phase progress,
// the unphased bucket, and the § cross-ref chip (a step whose description
// references the bundle with an anchor).
func seedPhasedPlanFake() *fakeDaemon {
	f := newFake()
	tm := func(min int) time.Time { return time.Date(2026, 7, 1, 10, min, 0, 0, time.UTC) }

	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan: ship phases", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Description: "Group work into phases.",
		// Ordered phase membership: research first, implementation second.
		Fields:    map[string]string{"phase_order": `["gd-phaseR","gd-phaseI"]`},
		UpdatedAt: tm(0),
	}
	bundle := &beadsapi.BeadDetail{ID: "gd-bundle1", Title: "Plan narrative", Kind: "issue", Type: "bundle", Status: "open"}
	phaseR := &beadsapi.BeadDetail{ID: "gd-phaseR", Title: "Research", Kind: "issue", Type: "epic", Status: "closed", Assignee: "gardenio-researcher"}
	phaseI := &beadsapi.BeadDetail{ID: "gd-phaseI", Title: "Implementation", Kind: "issue", Type: "epic", Status: "in_progress"}

	// Research phase: two closed steps.
	r1 := &beadsapi.BeadDetail{ID: "gd-r1", Title: "Read the code", Kind: "issue", Type: "task", Status: "closed", CreatedAt: tm(1)}
	r2 := &beadsapi.BeadDetail{ID: "gd-r2", Title: "Write a design", Kind: "issue", Type: "task", Status: "closed", CreatedAt: tm(2)}
	// Implementation phase: one closed, one open. i-open references a bundle section.
	i1 := &beadsapi.BeadDetail{ID: "gd-i1", Title: "Build the model", Kind: "issue", Type: "task", Status: "closed", CreatedAt: tm(3)}
	i2 := &beadsapi.BeadDetail{
		ID: "gd-i2", Title: "Wire the template", Kind: "issue", Type: "task", Status: "in_progress", CreatedAt: tm(4),
		Description: "Implements the render model. see gd-bundle1#c1-1 for the spec.",
	}
	// One unphased direct step.
	loose := &beadsapi.BeadDetail{ID: "gd-loose", Title: "Loose follow-up", Kind: "issue", Type: "task", Status: "open", CreatedAt: tm(5)}

	for _, b := range []*beadsapi.BeadDetail{plan, bundle, phaseR, phaseI, r1, r2, i1, i2, loose} {
		f.beads[b.ID] = b
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		// Direct children of the plan: two phase sub-epics + bundle + loose step.
		"gd-plan1": {
			{BeadID: "gd-phaseR", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-phaseI", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-bundle1", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-loose", DependsOnID: "gd-plan1", Type: "parent-child"},
		},
		// Grandchildren under each phase.
		"gd-phaseR": {
			{BeadID: "gd-r1", DependsOnID: "gd-phaseR", Type: "parent-child"},
			{BeadID: "gd-r2", DependsOnID: "gd-phaseR", Type: "parent-child"},
		},
		"gd-phaseI": {
			{BeadID: "gd-i1", DependsOnID: "gd-phaseI", Type: "parent-child"},
			{BeadID: "gd-i2", DependsOnID: "gd-phaseI", Type: "parent-child"},
		},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	// Transitive rollup: 5 steps total, 3 closed (r1,r2,i1).
	f.epicSubtreeCount = map[string]int{"gd-plan1": 5, "gd-plan1|closed": 3}
	return f
}

// seedMoleculePhasePlanFake seeds a plan whose SECOND phase is a poured MOLECULE
// (type=molecule) rather than a hand-authored sub-epic — the emplaced-pour shape
// (ka-0000000759). isPhaseContainer must accept it so it renders as a phase with
// its molecule step children, exactly like an epic phase.
func seedMoleculePhasePlanFake() *fakeDaemon {
	f := newFake()
	tm := func(min int) time.Time { return time.Date(2026, 7, 1, 10, min, 0, 0, time.UTC) }

	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan: ship phases", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		// Phase 1 is a normal sub-epic; phase 2 is a poured molecule.
		Fields:    map[string]string{"phase_order": `["gd-phaseR","gd-molP"]`},
		UpdatedAt: tm(0),
	}
	phaseR := &beadsapi.BeadDetail{ID: "gd-phaseR", Title: "Research", Kind: "issue", Type: "epic", Status: "closed"}
	// The emplaced pour: a type=molecule container listed in phase_order. It
	// carries the provenance fields kd formula pour stamps — the recipe id and the
	// pour-time var bindings — that drive the poured-phase provenance chip
	// (ka-0000002220).
	molP := &beadsapi.BeadDetail{
		ID: "gd-molP", Title: "Poured phase", Kind: "issue", Type: "molecule", Status: "in_progress",
		Fields: map[string]string{
			"formula_id":   "gd-formX",
			"applied_vars": `{"widget":"gauge","count":"3"}`,
		},
	}
	// The recipe the molecule was poured from (the chip's deep-link target).
	formX := &beadsapi.BeadDetail{ID: "gd-formX", Title: "Widget builder", Kind: "issue", Type: "formula", Status: "open"}

	r1 := &beadsapi.BeadDetail{ID: "gd-r1", Title: "Read the code", Kind: "issue", Type: "task", Status: "closed", CreatedAt: tm(1)}
	// The molecule's step children.
	m1 := &beadsapi.BeadDetail{ID: "gd-m1", Title: "Design widget", Kind: "issue", Type: "task", Status: "closed", CreatedAt: tm(3)}
	m2 := &beadsapi.BeadDetail{ID: "gd-m2", Title: "Implement widget", Kind: "issue", Type: "task", Status: "open", CreatedAt: tm(4)}

	for _, b := range []*beadsapi.BeadDetail{plan, phaseR, molP, formX, r1, m1, m2} {
		f.beads[b.ID] = b
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {
			{BeadID: "gd-phaseR", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-molP", DependsOnID: "gd-plan1", Type: "parent-child"},
		},
		"gd-phaseR": {
			{BeadID: "gd-r1", DependsOnID: "gd-phaseR", Type: "parent-child"},
		},
		"gd-molP": {
			{BeadID: "gd-m1", DependsOnID: "gd-molP", Type: "parent-child"},
			{BeadID: "gd-m2", DependsOnID: "gd-molP", Type: "parent-child"},
		},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	f.epicSubtreeCount = map[string]int{"gd-plan1": 3, "gd-plan1|closed": 2}
	return f
}

// TestHandlePlanDetail_RendersMoleculePhase proves a poured MOLECULE listed in a
// plan's phase_order renders as a first-class phase — header + its molecule step
// children + per-phase progress — exactly like an epic phase (isPhaseContainer,
// resolved decision ka-0000003759).
func TestHandlePlanDetail_RendersMoleculePhase(t *testing.T) {
	mux := mkServer(t, seedMoleculePhasePlanFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-phase-id="gd-molP"`, // the molecule renders AS a phase
		"Poured phase",            // molecule phase title
		"Design widget",           // molecule step child
		"Implement widget",        // molecule step child
		"1/2 · 50%",               // molecule per-phase progress (1 of 2 closed)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("molecule-phase render missing %q", want)
		}
	}
}

// TestHandlePlanDetail_MoleculePhaseProvenanceChip proves the poured-phase
// provenance chip (ka-0000002220) renders on the molecule phase's HEADER row and
// links both the formula (deep-link + resolved title) and the applied vars —
// without adding a new card row (the chip lives inside .plan-phase-head, the
// same row the number/title/progress already occupy). An epic phase carries no
// formula_id, so the chip must NOT render for it.
func TestHandlePlanDetail_MoleculePhaseProvenanceChip(t *testing.T) {
	mux := mkServer(t, seedMoleculePhasePlanFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The chip renders, deep-links the formula, shows its resolved title, and
	// signals the applied-var count (2 vars: widget, count).
	for _, want := range []string{
		`class="chip plan-phase-provenance"`, // the chip class
		`href="/gd-formX"`,                   // deep-links the recipe
		"Widget builder",                     // resolved formula title (not bare id)
		"2 vars",                             // applied-var count signal
	} {
		if !strings.Contains(body, want) {
			t.Errorf("provenance chip render missing %q", want)
		}
	}

	// The applied var bindings surface in the chip's hover title (no new row).
	if !strings.Contains(body, "widget=gauge") || !strings.Contains(body, "count=3") {
		t.Errorf("provenance chip title missing applied var bindings; body:\n%s", body)
	}

	// Info-density gate: the chip lives ON the molecule phase's HEADER row, not in
	// a new element below it. Scope to the molecule phase's own section (from its
	// data-phase-id to the start of its step <ol>) and assert the chip's rendered
	// opening tag falls before that section's closing </header>. (Match the class
	// ATTRIBUTE, not the bare class name, which also appears in the <style> block.)
	const chipTag = `class="chip plan-phase-provenance"`
	molStart := strings.Index(body, `data-phase-id="gd-molP"`)
	if molStart < 0 {
		t.Fatalf("molecule phase section not found")
	}
	section := body[molStart:]
	chipIdx := strings.Index(section, chipTag)
	headEnd := strings.Index(section, "</header>")
	if chipIdx < 0 || headEnd < 0 || chipIdx > headEnd {
		t.Errorf("provenance chip must render inside the molecule phase header row (no new card row): chipIdx=%d headEnd=%d", chipIdx, headEnd)
	}

	// The epic phase (gd-phaseR — Research) carries no formula_id, so exactly ONE
	// provenance chip renders across the whole plan (only the molecule phase).
	if n := strings.Count(body, chipTag); n != 1 {
		t.Errorf("expected exactly 1 provenance chip (molecule phase only), got %d", n)
	}
}

// TestHandlePlanDetail_RendersPhaseGroups proves a plan with phase_order renders
// its steps grouped under ordered phase headers (research before
// implementation), each with per-phase progress, plus the unphased step under
// an "Other steps" heading.
func TestHandlePlanDetail_RendersPhaseGroups(t *testing.T) {
	mux := mkServer(t, seedPhasedPlanFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="plan-phase`, // a phase group renders
		"Research",          // phase 1 title
		"Implementation",    // phase 2 title
		"Read the code",     // research step
		"Build the model",   // impl step
		"Wire the template", // impl open step
		`data-phase-id="gd-phaseR"`,
		`data-phase-id="gd-phaseI"`,
		"Other steps",     // unphased bucket heading
		"Loose follow-up", // the unphased step
		"2/2 · 100%",      // research per-phase progress (both closed)
		"1/2 · 50%",       // implementation per-phase progress
	} {
		if !strings.Contains(body, want) {
			t.Errorf("phase render missing %q", want)
		}
	}

	// Ordering: Research must appear before Implementation (phase_order).
	if strings.Index(body, "Research") > strings.Index(body, "Implementation") {
		t.Error("Research phase should render before Implementation (phase_order)")
	}
	// The § cross-ref chip on the impl open step points at the bundle section.
	if !strings.Contains(body, `href="/gd-bundle1#c1-1"`) {
		t.Error("expected § cross-ref chip deep-linking gd-bundle1#c1-1")
	}
	if !strings.Contains(body, "§ c1-1") {
		t.Error("expected the § section chip label")
	}
}

// TestHandlePlanDetail_UnphasedPlanUnchanged proves a plan WITHOUT phase_order
// renders exactly as before — a flat timeline, no phase headers, no "Other
// steps" heading (the pre-phase behavior is preserved).
func TestHandlePlanDetail_UnphasedPlanUnchanged(t *testing.T) {
	mux := mkServer(t, seedPlansFake()) // seedPlansFake has no phase_order
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `class="plan-phase`) {
		t.Error("an unphased plan must not render phase groups")
	}
	if strings.Contains(body, "Other steps") {
		t.Error("an unphased plan must not render the 'Other steps' heading")
	}
	if !strings.Contains(body, "Wire the route") {
		t.Error("the flat step should still render")
	}
}

// TestHandlePlanPhaseReorder_MovesPhaseDown proves the reorder handler rewrites
// phase_order (research,impl → impl,research when research moves down) via the
// daemon's compare-and-swap primitive (ReorderBeadArrayField) and re-renders the
// fragment. The CAS seeds `expected` from the stored order and writes `next`.
func TestHandlePlanPhaseReorder_MovesPhaseDown(t *testing.T) {
	f := seedPhasedPlanFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/phases/gd-phaseR/reorder?dir=down", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if f.reorderArrayCalls != 1 {
		t.Fatalf("expected 1 CAS reorder call, got %d", f.reorderArrayCalls)
	}
	if f.reorderArrayLast.ID != "gd-plan1" || f.reorderArrayLast.Field != "phase_order" {
		t.Errorf("reorder targeted %s/%s, want gd-plan1/phase_order", f.reorderArrayLast.ID, f.reorderArrayLast.Field)
	}
	if exp := f.reorderArrayLast.Expected; len(exp) != 2 || exp[0] != "gd-phaseR" || exp[1] != "gd-phaseI" {
		t.Errorf("CAS expected = %v, want [gd-phaseR gd-phaseI] (the stored order)", exp)
	}
	if arr := f.reorderArrayLast.Next; len(arr) != 2 || arr[0] != "gd-phaseI" || arr[1] != "gd-phaseR" {
		t.Errorf("reordered phase_order = %v, want [gd-phaseI gd-phaseR]", arr)
	}
}

// TestHandlePlanPhaseReorder_InterleavedStale is the ka-0000002075 handler-level
// regression: a phase_order carrying a STALE id between the two rendered phases
// must still transpose them on a SINGLE ▲/▼ click, writing the stale id back at
// its original index. Pre-fix, the handler swapped raw-array neighbors (A↔stale)
// so the write was [stale, A, B] — which re-renders identically to [A, B], a
// visible no-op that only "took" on the second click.
func TestHandlePlanPhaseReorder_InterleavedStale(t *testing.T) {
	f := seedPhasedPlanFake()
	// Insert a stale id (never a direct child, so it resolves to no phase) BETWEEN
	// the two rendered phases in the raw phase_order.
	f.beads["gd-plan1"].Fields["phase_order"] = `["gd-phaseR","gd-stale","gd-phaseI"]`

	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/phases/gd-phaseR/reorder?dir=down", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if f.reorderArrayCalls != 1 {
		t.Fatalf("expected 1 CAS reorder call, got %d", f.reorderArrayCalls)
	}
	arr := f.reorderArrayLast.Next
	// The two RENDERED phases transpose; the stale id stays pinned at index 1.
	want := []string{"gd-phaseI", "gd-stale", "gd-phaseR"}
	if len(arr) != 3 || arr[0] != want[0] || arr[1] != want[1] || arr[2] != want[2] {
		t.Errorf("reordered phase_order = %v, want %v (rendered phases swap, stale pinned)", arr, want)
	}
	// It must NOT be the pre-fix raw-neighbor swap, which would re-render as a
	// no-op.
	if arr[0] == "gd-stale" {
		t.Error("regression: raw-neighbor swap put the stale id first — a visible no-op on re-render")
	}
	// The CAS `expected` is the full raw stored order including the stale id, so
	// the swap lands only if nothing else touched phase_order meanwhile.
	if exp := f.reorderArrayLast.Expected; len(exp) != 3 || exp[1] != "gd-stale" {
		t.Errorf("CAS expected = %v, want the raw stored order with gd-stale pinned at index 1", exp)
	}
}

// TestHandlePlanPhaseReorder_BoundaryNoOp proves moving the first phase "up" is
// a harmless no-op (no field write, 200) rather than an error or a corrupt
// order.
func TestHandlePlanPhaseReorder_BoundaryNoOp(t *testing.T) {
	f := seedPhasedPlanFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/phases/gd-phaseR/reorder?dir=up", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.reorderArrayCalls != 0 {
		t.Errorf("boundary reorder should issue no CAS write, got %d", f.reorderArrayCalls)
	}
}

// TestHandlePlanPhaseReorder_NonOperatorForbidden proves the reorder route is
// operator-gated.
func TestHandlePlanPhaseReorder_NonOperatorForbidden(t *testing.T) {
	f := seedPhasedPlanFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/phases/gd-phaseR/reorder?dir=down", nil)
	// No operator role.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Errorf("non-operator reorder should be rejected, got 200")
	}
	if f.reorderArrayCalls != 0 {
		t.Errorf("non-operator reorder must not write, got %d CAS writes", f.reorderArrayCalls)
	}
}

// TestHandlePlanPhaseReorder_RetriesOnCASConflict proves the reorder survives a
// transient compare-and-swap conflict: the daemon rejects the first N writes
// (409), and reorderArrayFieldWithRetry re-reads + re-derives and retries until
// it lands, without dropping the reorder or surfacing an error.
func TestHandlePlanPhaseReorder_RetriesOnCASConflict(t *testing.T) {
	f := seedPhasedPlanFake()
	f.reorderArrayConflicts = 2 // first two CAS attempts 409, third succeeds
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/phases/gd-phaseR/reorder?dir=down", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retries, got %d (body: %s)", w.Code, w.Body.String())
	}
	if f.reorderArrayCalls != 3 {
		t.Errorf("expected 3 CAS attempts (2 conflicts + 1 success), got %d", f.reorderArrayCalls)
	}
	if arr := f.reorderArrayLast.Next; len(arr) != 2 || arr[0] != "gd-phaseI" || arr[1] != "gd-phaseR" {
		t.Errorf("final reordered phase_order = %v, want [gd-phaseI gd-phaseR]", arr)
	}
}

// TestHandlePlanPhaseReorder_ExhaustedConflictsSurface409 proves that a
// persistent CAS conflict storm (more than the retry bound) surfaces a 409 the
// operator can retry, rather than silently dropping the reorder or 500ing.
func TestHandlePlanPhaseReorder_ExhaustedConflictsSurface409(t *testing.T) {
	f := seedPhasedPlanFake()
	f.reorderArrayConflicts = 100 // every attempt conflicts → retries exhausted
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/phases/gd-phaseR/reorder?dir=down", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on exhausted CAS retries, got %d", w.Code)
	}
	if f.reorderArrayCalls != maxCASReorderAttempts {
		t.Errorf("expected %d CAS attempts before giving up, got %d", maxCASReorderAttempts, f.reorderArrayCalls)
	}
}

// TestPlansIndex_CurrentStepDescendsIntoPhases proves the /plans index
// "current or next step" chip finds a grandchild step under a phase sub-epic
// (the impl phase's in_progress "Wire the template"), not nil — the pre-phase
// code stopped at direct children and would have shown no chip.
func TestPlansIndex_CurrentStepDescendsIntoPhases(t *testing.T) {
	mux := mkServer(t, seedPhasedPlanFake())
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The in_progress grandchild step is the current step surfaced on the card.
	if !strings.Contains(body, "Wire the template") {
		t.Error("index card should surface the in_progress grandchild step as current/next")
	}
	// Its chip Number must be the PER-PHASE rank (2nd of the 2 Implementation
	// steps), matching what the detail page shows — NOT the global creation
	// rank across all phases (which would be #4). Regression guard for the
	// global-vs-per-phase numbering bug (ka-0000005926 review).
	if !strings.Contains(body, `<span class="plan-step-num">#2</span>`) {
		t.Error("index chip Number should be the per-phase rank #2, not the global rank #4")
	}
	if strings.Contains(body, `<span class="plan-step-num">#4</span>`) {
		t.Error("index chip must not show the global creation rank #4 for a per-phase step")
	}
}

// TestPickPlanCurrentOrNextStep_PerPhaseNumbering unit-tests the precedence +
// per-phase numbering directly: an in_progress step in a LATER phase beats a
// not-started step in an EARLIER phase ("now" before "next"), and its Number is
// its rank WITHIN its phase, not the global concatenated rank.
func TestPickPlanCurrentOrNextStep_PerPhaseNumbering(t *testing.T) {
	tm := func(min int) time.Time { return time.Date(2026, 7, 1, 10, min, 0, 0, time.UTC) }
	// Phase A: one open (not started). Phase B: one closed + one in_progress.
	direct := map[string]*beadsapi.BeadDetail{
		"gd-A": {ID: "gd-A", Type: "epic"},
		"gd-B": {ID: "gd-B", Type: "epic"},
	}
	grand := map[string]*beadsapi.BeadDetail{
		"gd-a1": {ID: "gd-a1", Type: "task", Status: "open", CreatedAt: tm(1)},
		"gd-b1": {ID: "gd-b1", Type: "task", Status: "closed", CreatedAt: tm(2)},
		"gd-b2": {ID: "gd-b2", Type: "task", Status: "in_progress", CreatedAt: tm(3)},
	}
	order := []string{"gd-A", "gd-B"}
	bySub := map[string][]string{"gd-A": {"gd-a1"}, "gd-B": {"gd-b1", "gd-b2"}}
	got := pickPlanCurrentOrNextStep(nil, order, []string{"gd-A", "gd-B"}, bySub, direct, grand)
	if got == nil {
		t.Fatal("expected the in_progress step, got nil")
	}
	// in_progress in phase B beats the open step in phase A.
	if got.Status != "in_progress" {
		t.Errorf("precedence: expected in_progress, got %q", got.Status)
	}
	// Its number is 2 (2nd within phase B), NOT 3 (global creation rank).
	if got.Number != 2 {
		t.Errorf("per-phase numbering: expected #2, got #%d", got.Number)
	}
}

// TestStepNumbering_CreatedAtCollisionTieBreaksByID proves buildStepRows
// (detail timeline) and pickCurrentOrNextStep (index chip) assign the SAME
// stable #N when two sibling steps share an exact CreatedAt, regardless of the
// order the daemon returned them in — the two child endpoints have no ORDER BY,
// so without the ID tie-break the numbers could transpose between the two
// surfaces (ka-0000007589 item 3).
func TestStepNumbering_CreatedAtCollisionTieBreaksByID(t *testing.T) {
	at := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	children := map[string]*beadsapi.BeadDetail{
		"gd-zz": {ID: "gd-zz", Title: "Z step", Type: "task", Status: "open", CreatedAt: at},
		"gd-aa": {ID: "gd-aa", Title: "A step", Type: "task", Status: "open", CreatedAt: at},
	}
	// Feed the two surfaces OPPOSITE input orders — the numbering must agree.
	rows := buildStepRows([]string{"gd-zz", "gd-aa"}, children, nil)
	if len(rows) != 2 || rows[0].ID != "gd-aa" || rows[0].Number != 1 || rows[1].ID != "gd-zz" {
		t.Fatalf("buildStepRows should tie-break by ID (gd-aa first), got %+v", rows)
	}
	step := pickCurrentOrNextStep([]string{"gd-aa", "gd-zz"}, children, nil)
	if step == nil {
		t.Fatal("expected a next step")
	}
	if step.Title != "A step" || step.Number != 1 {
		t.Errorf("index chip should pick #1 %q via the same ID tie-break, got #%d %q",
			"A step", step.Number, step.Title)
	}
}

// TestPlanProgress_CountsRenderedPhaseSteps proves planProgress counts EXACTLY
// the rendered steps, spanning BOTH the unphased flat steps AND every phase's
// steps — so a mixed phased plan's header chip equals the sum of the visible
// rows (ka-0000005926 + ka-0000004410). The seed has 4 phase steps (3 closed) +
// 1 unphased open step = 5 total, 3 closed. It must report the rendered count
// and IGNORE any transitive subtree number: epicSubtreeCount is set to a
// deliberately different value (99/98) that must not leak into the header.
func TestPlanProgress_CountsRenderedPhaseSteps(t *testing.T) {
	f := seedPhasedPlanFake()
	// A transitive subtree count that disagrees with the rendered rows — the
	// header must NOT track it (this is the 23/27-vs-25-rows bug, inverted).
	f.epicSubtreeCount = map[string]int{"gd-plan1": 99, "gd-plan1|closed": 98}
	s := testServerWithDaemon(f)
	steps, phases := s.planTimeline(t.Context(), "gd-plan1")
	total, closed, pct := planProgress(steps, phases)
	if total != 5 || closed != 3 {
		t.Errorf("rendered progress = %d/%d, want 5/3 (4 phase steps + 1 flat, 3 closed)", closed, total)
	}
	if pct != 60 {
		t.Errorf("rendered pct = %d, want 60", pct)
	}
}

// TestBuildPlanEventFilters_IncludesPhaseGrandchildren proves the SSE interest
// set now covers phase sub-epics AND their grandchild steps, so a step closed
// under a phase re-renders the timeline live (the pre-phase filter dropped
// grandchild events).
func TestBuildPlanEventFilters_IncludesPhaseGrandchildren(t *testing.T) {
	s := testServerWithDaemon(seedPhasedPlanFake())
	f := s.buildPlanEventFilters(t.Context(), "gd-plan1", nil)
	for _, id := range []string{"gd-plan1", "gd-phaseR", "gd-phaseI", "gd-r1", "gd-r2", "gd-i1", "gd-i2"} {
		if _, ok := f.steps[id]; !ok {
			t.Errorf("SSE steps interest set missing %q (phase descent)", id)
		}
	}
	// The phase sub-epics are tracked so a step added under them rebuilds.
	for _, id := range []string{"gd-phaseR", "gd-phaseI"} {
		if _, ok := f.phaseEpicIDs[id]; !ok {
			t.Errorf("phaseEpicIDs missing %q", id)
		}
	}
	// The bundle is the narrative target.
	if f.bundleID != "gd-bundle1" {
		t.Errorf("bundleID = %q, want gd-bundle1", f.bundleID)
	}
}

// TestWritePlanEvent_PhaseOrderChangeRebuildsFilters is the ka-0000004061
// regression: a plan epic that gains a NEW phase sub-epic in fields.phase_order
// AFTER an SSE client connected must rebuild the connect-time interest set so a
// step LATER added under that phase streams live (pre-fix it only appeared on
// reload). writePlanEvent's phase-membership rebuild lived ONLY on the
// beads.dependency.* branch; a phase_order write fires beads.bead.updated on the
// epic, which never rebuilt filters.phaseEpicIDs — so the later grandchild
// dependency failed the intoPhase interest check and the event was dropped.
//
// The connect-time-only guard (TestBuildPlanEventFilters_IncludesPhaseGrandchildren)
// cannot see this: it builds filters once against a static fake. This drives the
// mid-session change directly — stale filters (only gd-phaseR listed) → a
// phase_order bead.updated on the epic → the rebuild must fold in gd-phaseI, its
// phaseEpicIDs entry, AND its grandchild steps; then a dependency edge into the
// freshly-listed phase must actually stream a plan-updated frame.
//
// Mutation check: delete the `wrapper.Bead.ID == epicID && planPhaseOrderChanged`
// rebuild in writePlanEvent (plans_events.go) and both the phaseEpicIDs assertion and the
// streamed-frame assertion fail.
func TestWritePlanEvent_PhaseOrderChangeRebuildsFilters(t *testing.T) {
	f := seedPhasedPlanFake()
	// Simulate the client connecting BEFORE gd-phaseI was added to the phase
	// list: at connect time the plan lists only the research phase. gd-phaseI is
	// already a direct child (so it's in `steps`) but is NOT yet a phase, so its
	// grandchild steps (gd-i1, gd-i2) are outside the interest set.
	f.beads["gd-plan1"].Fields["phase_order"] = `["gd-phaseR"]`

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	filters := srv.buildPlanEventFilters(t.Context(), "gd-plan1", nil)

	// Precondition: the not-yet-a-phase sub-epic and its steps are out of scope.
	if _, ok := filters.phaseEpicIDs["gd-phaseI"]; ok {
		t.Fatal("precondition: gd-phaseI must not be a tracked phase before the phase_order change")
	}
	for _, id := range []string{"gd-i1", "gd-i2"} {
		if _, ok := filters.steps[id]; ok {
			t.Fatalf("precondition: grandchild %q must be out of the interest set before the change", id)
		}
	}

	// Mid-session: the gardener adds gd-phaseI to the plan's phase list. The
	// daemon fires beads.bead.updated on the epic carrying the bare per-field
	// key (kbeads beads_field_changes) — the live epic now lists both phases.
	f.beads["gd-plan1"].Fields["phase_order"] = `["gd-phaseR","gd-phaseI"]`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1/events", nil)
	updated := beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data: []byte(`{"bead":{"id":"gd-plan1","type":"epic",` +
			`"fields":{"phase_order":["gd-phaseR","gd-phaseI"]}},` +
			`"changes":{"phase_order":["gd-phaseR","gd-phaseI"],"fields":true}}`),
	}
	srv.writePlanEvent(t.Context(), w, req, w, updated, "gd-plan1", "in_progress", &filters, false)

	// The rebuild must now track the new phase AND descend into its grandchild
	// steps, so a subsequent step add/close under it re-renders the timeline.
	if _, ok := filters.phaseEpicIDs["gd-phaseI"]; !ok {
		t.Error("phase_order change must rebuild filters to include the new phase sub-epic gd-phaseI")
	}
	for _, id := range []string{"gd-i1", "gd-i2"} {
		if _, ok := filters.steps[id]; !ok {
			t.Errorf("phase_order change must fold new phase grandchild %q into the step interest set", id)
		}
	}

	// Symptom check: a step added under the freshly-listed phase (a dependency
	// edge whose target is gd-phaseI) now passes the intoPhase interest gate and
	// streams a plan-updated frame. Pre-fix, gd-phaseI was absent from
	// phaseEpicIDs so this edge was dropped and the step appeared only on reload.
	w2 := httptest.NewRecorder()
	depEvt := beadsapi.SSEEvent{
		Event: "beads.dependency.added",
		Data:  []byte(`{"dependency":{"bead_id":"gd-i3","depends_on_id":"gd-phaseI","type":"parent-child"}}`),
	}
	srv.writePlanEvent(t.Context(), w2, req, w2, depEvt, "gd-plan1", "in_progress", &filters, false)
	if !strings.Contains(w2.Body.String(), "event: plan-updated") {
		t.Errorf("a step added under the newly-listed phase must stream a plan-updated frame, got: %s", w2.Body.String())
	}
}

// --- unit tests for the pure helpers ---

func TestPlanPhaseOrder_Decodes(t *testing.T) {
	epic := &beadsapi.BeadDetail{Fields: map[string]string{"phase_order": `["gd-a","gd-b","gd-c"]`}}
	got := planPhaseOrder(epic)
	if len(got) != 3 || got[0] != "gd-a" || got[2] != "gd-c" {
		t.Errorf("planPhaseOrder = %v, want [gd-a gd-b gd-c]", got)
	}
	// Absent / empty / unparseable all degrade to nil (no phases).
	for _, epic := range []*beadsapi.BeadDetail{
		{Fields: nil},
		{Fields: map[string]string{}},
		{Fields: map[string]string{"phase_order": ""}},
		{Fields: map[string]string{"phase_order": "not json"}},
	} {
		if got := planPhaseOrder(epic); got != nil {
			t.Errorf("planPhaseOrder(%v) = %v, want nil", epic.Fields, got)
		}
	}
}

// TestResolvedPhaseIDs proves the shared membership helper drops ids that do not
// resolve to a current direct-child epic (stale/deleted/re-parented + non-epic
// children) and dedupes first-occurrence-wins, preserving order — the single
// source of truth both the render (planPhases) and the reorder handler key off.
func TestResolvedPhaseIDs(t *testing.T) {
	direct := map[string]*beadsapi.BeadDetail{
		"gd-A":    {ID: "gd-A", Type: "epic"},
		"gd-mol":  {ID: "gd-mol", Type: "molecule"}, // a poured molecule IS a phase container
		"gd-B":    {ID: "gd-B", Type: "epic"},
		"gd-task": {ID: "gd-task", Type: "task"}, // a non-container child is not a phase
	}
	// Raw: an epic, a stale id (absent from direct), a non-container child, a
	// molecule (kept), a dup, then another epic.
	got := resolvedPhaseIDs([]string{"gd-A", "gd-stale", "gd-task", "gd-mol", "gd-A", "gd-B"}, direct)
	if len(got) != 3 || got[0] != "gd-A" || got[1] != "gd-mol" || got[2] != "gd-B" {
		t.Errorf("resolvedPhaseIDs = %v, want [gd-A gd-mol gd-B] (stale/non-container/dup dropped, molecule kept)", got)
	}
}

func TestIsPhaseContainer(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{"epic", &beadsapi.BeadDetail{Type: "epic"}, true},
		{"molecule", &beadsapi.BeadDetail{Type: "molecule"}, true},
		{"task", &beadsapi.BeadDetail{Type: "task"}, false},
		{"bug", &beadsapi.BeadDetail{Type: "bug"}, false},
		{"bundle", &beadsapi.BeadDetail{Type: "bundle"}, false},
	}
	for _, tc := range cases {
		if got := isPhaseContainer(tc.bead); got != tc.want {
			t.Errorf("isPhaseContainer(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFirstBundleSectionRef(t *testing.T) {
	// A matching anchored ref is lifted; Slug is the bare heading-anchor token
	// (the Href fragment sans `#`) that plan_gutter.js maps a highlight against.
	ref, ok := firstBundleSectionRef("see gd-bundle1#C1.1 for details", "gd-bundle1")
	if !ok || ref.Label != "C1.1" || ref.Href != "gd-bundle1#c1-1" || ref.Slug != "c1-1" {
		t.Errorf("ref = %+v (ok=%v), want label C1.1 href gd-bundle1#c1-1 slug c1-1", ref, ok)
	}
	// A bare bundle ref with no anchor is ignored (the chip deep-links a section).
	if _, ok := firstBundleSectionRef("see gd-bundle1 for the plan", "gd-bundle1"); ok {
		t.Error("a bare bundle ref (no #anchor) should not produce a chip")
	}
	// A ref to a DIFFERENT bead is ignored.
	if _, ok := firstBundleSectionRef("see gd-other0000#c1-1", "gd-bundle1"); ok {
		t.Error("a ref to a non-bundle bead should not produce a chip")
	}
	// No bundle → no chip.
	if _, ok := firstBundleSectionRef("see gd-bundle1#c1-1", ""); ok {
		t.Error("no bundle id should produce no chip")
	}
}

// TestRenderedStepRollup proves the index rollup helper counts EXACTLY the
// timeline's rendered rows and excludes non-step descendants (ka-0000004410):
//   - direct issue children count; a direct bundle/decision/epic does NOT;
//   - phase sub-epics contribute their own issue children;
//   - a grandchild under a step is NOT walked (only direct children are passed
//     in), so it never inflates the count.
func TestRenderedStepRollup(t *testing.T) {
	// Flat plan (no phases): 3 direct children — 2 issue tasks (1 closed) + 1
	// bundle. Rendered rollup counts only the 2 tasks → 1/2.
	beads := map[string]*beadsapi.BeadDetail{
		"gd-t1":  {ID: "gd-t1", Type: "task", Status: "closed"},
		"gd-t2":  {ID: "gd-t2", Type: "task", Status: "open"},
		"gd-bun": {ID: "gd-bun", Type: "bundle", Status: "open"},
		"gd-dec": {ID: "gd-dec", Type: "decision", Status: "open"},
	}
	total, closed, _, _ := renderedStepRollup(nil, []string{"gd-t1", "gd-t2", "gd-bun", "gd-dec"}, nil, beads, nil)
	if total != 2 || closed != 1 {
		t.Errorf("flat rollup = %d/%d, want 1/2 (bundle + decision excluded)", closed, total)
	}

	// Phased plan: two phase sub-epics + one unphased loose task + one direct
	// bundle. phaseR has 2 tasks (both closed); phaseI has 2 tasks (1 closed).
	// Loose task open. Rendered = 4 phase steps (3 closed) + 1 flat = 5/3.
	direct := map[string]*beadsapi.BeadDetail{
		"gd-pR":    {ID: "gd-pR", Type: "epic", Status: "closed"},
		"gd-pI":    {ID: "gd-pI", Type: "epic", Status: "in_progress"},
		"gd-loose": {ID: "gd-loose", Type: "task", Status: "open"},
		"gd-bun":   {ID: "gd-bun", Type: "bundle", Status: "open"},
	}
	grand := map[string]*beadsapi.BeadDetail{
		"gd-r1": {ID: "gd-r1", Type: "task", Status: "closed"},
		"gd-r2": {ID: "gd-r2", Type: "task", Status: "closed"},
		"gd-i1": {ID: "gd-i1", Type: "task", Status: "closed"},
		"gd-i2": {ID: "gd-i2", Type: "task", Status: "open"},
	}
	order := []string{"gd-pR", "gd-pI"}
	bySub := map[string][]string{"gd-pR": {"gd-r1", "gd-r2"}, "gd-pI": {"gd-i1", "gd-i2"}}
	directIDs := []string{"gd-pR", "gd-pI", "gd-loose", "gd-bun"}
	total, closed, _, _ = renderedStepRollup(order, directIDs, bySub, direct, grand)
	if total != 5 || closed != 3 {
		t.Errorf("phased rollup = %d/%d, want 3/5 (4 phase steps + 1 loose, 3 closed)", closed, total)
	}

	// A phase_order id that is NOT a current direct-child epic is skipped (its
	// children are not counted), and a step double-parented as BOTH a phase
	// grandchild AND a direct child counts once (under the phase), matching
	// planTimeline's flat-exclusion.
	direct2 := map[string]*beadsapi.BeadDetail{
		"gd-pR":   {ID: "gd-pR", Type: "epic", Status: "open"},
		"gd-dup":  {ID: "gd-dup", Type: "task", Status: "open"}, // also under phaseR
		"gd-solo": {ID: "gd-solo", Type: "task", Status: "closed"},
	}
	grand2 := map[string]*beadsapi.BeadDetail{"gd-dup": {ID: "gd-dup", Type: "task", Status: "open"}}
	total, closed, _, _ = renderedStepRollup(
		[]string{"gd-pR", "gd-ghost"}, // gd-ghost absent from direct2 → skipped
		[]string{"gd-pR", "gd-dup", "gd-solo"},
		map[string][]string{"gd-pR": {"gd-dup"}},
		direct2, grand2,
	)
	// gd-dup counts once (phase), gd-solo once (flat) → 2 total, 1 closed.
	if total != 2 || closed != 1 {
		t.Errorf("dedupe rollup = %d/%d, want 1/2 (gd-dup counted once, gd-ghost skipped)", closed, total)
	}
}

// TestRenderedStepRollup_LastActivity proves the rollup's third return is the
// newest updated_at over the RENDERED set — the rendered issue children plus the
// phase sub-epics themselves (ka-0000002873) — and that a bundle/decision child
// (not rendered as a step) never drives it. This is the instant the /plans index
// card folds with the epic's own updated_at to show "last change".
func TestRenderedStepRollup_LastActivity(t *testing.T) {
	ts := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("bad time %q: %v", s, err)
		}
		return tm
	}
	// Flat plan: the newest STEP is gd-t2 (Jul 4); the bundle is newer (Jul 9)
	// but must NOT count — it is not a rendered step.
	beads := map[string]*beadsapi.BeadDetail{
		"gd-t1":  {ID: "gd-t1", Type: "task", Status: "closed", UpdatedAt: ts("2026-07-02T00:00:00Z")},
		"gd-t2":  {ID: "gd-t2", Type: "task", Status: "open", UpdatedAt: ts("2026-07-04T00:00:00Z")},
		"gd-bun": {ID: "gd-bun", Type: "bundle", Status: "open", UpdatedAt: ts("2026-07-09T00:00:00Z")},
	}
	_, _, la, _ := renderedStepRollup(nil, []string{"gd-t1", "gd-t2", "gd-bun"}, nil, beads, nil)
	if !la.Equal(ts("2026-07-04T00:00:00Z")) {
		t.Errorf("flat last-activity = %v, want the newest step (Jul 4), not the bundle", la)
	}

	// Phased plan: a phase sub-epic's OWN updated_at (Jul 6) is newer than any of
	// its grandchildren (Jul 3/5), so it drives the last-activity.
	direct := map[string]*beadsapi.BeadDetail{
		"gd-pR": {ID: "gd-pR", Type: "epic", Status: "in_progress", UpdatedAt: ts("2026-07-06T00:00:00Z")},
	}
	grand := map[string]*beadsapi.BeadDetail{
		"gd-r1": {ID: "gd-r1", Type: "task", Status: "closed", UpdatedAt: ts("2026-07-03T00:00:00Z")},
		"gd-r2": {ID: "gd-r2", Type: "task", Status: "open", UpdatedAt: ts("2026-07-05T00:00:00Z")},
	}
	_, _, la, _ = renderedStepRollup(
		[]string{"gd-pR"},
		[]string{"gd-pR"},
		map[string][]string{"gd-pR": {"gd-r1", "gd-r2"}},
		direct, grand,
	)
	if !la.Equal(ts("2026-07-06T00:00:00Z")) {
		t.Errorf("phased last-activity = %v, want the phase sub-epic's own updated_at (Jul 6)", la)
	}
}

// TestPlanLastActivity proves the detail-side fold takes the newest instant
// across the epic, the flat steps, and every phase's steps + the phase itself
// (ka-0000002873), and that the epic's own updated_at is the floor when it is
// the newest (a freshly-created plan with older/no steps).
func TestPlanLastActivity(t *testing.T) {
	ts := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("bad time %q: %v", s, err)
		}
		return tm
	}
	epic := ts("2026-07-01T00:00:00Z")

	// A flat step newer than the epic wins.
	steps := []planStepView{
		{ID: "gd-s1", UpdatedAt: ts("2026-07-02T00:00:00Z")},
		{ID: "gd-s2", UpdatedAt: ts("2026-07-05T00:00:00Z")},
	}
	if got := planLastActivity(epic, steps, nil); !got.Equal(ts("2026-07-05T00:00:00Z")) {
		t.Errorf("last-activity = %v, want newest flat step (Jul 5)", got)
	}

	// A phase step newer than any flat step wins.
	phases := []planPhaseView{{
		ID:        "gd-p1",
		UpdatedAt: ts("2026-07-03T00:00:00Z"),
		Steps: []planStepView{
			{ID: "gd-ps1", UpdatedAt: ts("2026-07-08T00:00:00Z")},
		},
	}}
	if got := planLastActivity(epic, steps, phases); !got.Equal(ts("2026-07-08T00:00:00Z")) {
		t.Errorf("last-activity = %v, want newest phase step (Jul 8)", got)
	}

	// No steps at all → the epic's own updated_at is the floor.
	if got := planLastActivity(epic, nil, nil); !got.Equal(epic) {
		t.Errorf("last-activity = %v, want the epic's own updated_at (no steps)", got)
	}
}
