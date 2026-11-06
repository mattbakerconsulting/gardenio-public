package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plans"
)

// --- pure-helper tests -------------------------------------------------------

// TestReorderFailure classifies a reorderArrayFieldWithRetry error: an
// exhausted-retry CAS conflict is contention (409 + "try again"); any other
// error is a genuine write failure (502 + the verb-specific base message).
func TestReorderFailure(t *testing.T) {
	conflict := &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "cas conflict"}
	if msg, status := reorderFailureFor("Failed to reorder step.", conflict); status != http.StatusConflict || !strings.Contains(msg, "try again") {
		t.Errorf("CAS conflict → (%q, %d), want a retry message + 409", msg, status)
	}
	other := errors.New("daemon exploded")
	if msg, status := reorderFailureFor("Failed to reorder step.", other); status != http.StatusBadGateway || msg != "Failed to reorder step." {
		t.Errorf("non-CAS error → (%q, %d), want the base message + 502", msg, status)
	}
}

// reorderFailureFor maps a failed reorder to the fragment message + HTTP status
// the reorder handlers write (plans.ReorderError, then planWriteStatus).
func reorderFailureFor(base string, err error) (string, int) {
	e := plans.ReorderError(base, err)
	return e.Msg, planWriteStatus(e)
}

// TestPlanStepOrder_Decodes proves step_order decodes exactly like phase_order
// and degrades to nil (creation order) on every malformed input.
func TestPlanStepOrder_Decodes(t *testing.T) {
	epic := &beadsapi.BeadDetail{Fields: map[string]string{"step_order": `["gd-s1","gd-s2"]`}}
	got := planStepOrder(epic)
	if len(got) != 2 || got[0] != "gd-s1" || got[1] != "gd-s2" {
		t.Errorf("planStepOrder = %v, want [gd-s1 gd-s2]", got)
	}
	for _, e := range []*beadsapi.BeadDetail{
		nil,
		{Fields: nil},
		{Fields: map[string]string{}},
		{Fields: map[string]string{"step_order": ""}},
		{Fields: map[string]string{"step_order": "not json"}},
	} {
		if got := planStepOrder(e); got != nil {
			t.Errorf("planStepOrder(%v) = %v, want nil", e, got)
		}
	}
}

// TestSortStepBeads_OverlayThenCreation proves the ordering contract: listed
// steps sort by their step_order position, unlisted steps follow in creation
// order, and an absent/empty overlay reproduces pure creation order (with the
// ID tie-break preserved).
func TestSortStepBeads_OverlayThenCreation(t *testing.T) {
	tm := func(min int) time.Time { return time.Date(2026, 7, 1, 10, min, 0, 0, time.UTC) }
	mk := func(id string, min int) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: id, Type: "task", CreatedAt: tm(min)}
	}

	// Explicit overlay reorders against creation order: created a<b<c, but the
	// operator moved c to the front and b to the back.
	steps := []*beadsapi.BeadDetail{mk("gd-a", 1), mk("gd-b", 2), mk("gd-c", 3)}
	sortStepBeads(steps, []string{"gd-c", "gd-a", "gd-b"})
	if steps[0].ID != "gd-c" || steps[1].ID != "gd-a" || steps[2].ID != "gd-b" {
		t.Errorf("overlay order = [%s %s %s], want [gd-c gd-a gd-b]", steps[0].ID, steps[1].ID, steps[2].ID)
	}

	// Partial overlay: only gd-c is listed → it leads, the rest follow in
	// creation order (a before b).
	steps = []*beadsapi.BeadDetail{mk("gd-a", 1), mk("gd-b", 2), mk("gd-c", 3)}
	sortStepBeads(steps, []string{"gd-c"})
	if steps[0].ID != "gd-c" || steps[1].ID != "gd-a" || steps[2].ID != "gd-b" {
		t.Errorf("partial overlay = [%s %s %s], want [gd-c gd-a gd-b]", steps[0].ID, steps[1].ID, steps[2].ID)
	}

	// No overlay: pure creation order, ID tie-break on equal CreatedAt.
	at := tm(0)
	zz := &beadsapi.BeadDetail{ID: "gd-zz", Type: "task", CreatedAt: at}
	aa := &beadsapi.BeadDetail{ID: "gd-aa", Type: "task", CreatedAt: at}
	steps = []*beadsapi.BeadDetail{zz, aa}
	sortStepBeads(steps, nil)
	if steps[0].ID != "gd-aa" || steps[1].ID != "gd-zz" {
		t.Errorf("no overlay tie-break = [%s %s], want [gd-aa gd-zz]", steps[0].ID, steps[1].ID)
	}
}

// TestStepOrder_IndexDetailAgree proves the load-bearing invariant (ka-0000007589
// under a reorder overlay): buildStepRows (detail timeline) and
// pickCurrentOrNextStep (index chip) assign the SAME #N to the same step when an
// explicit step_order is present. Without the shared sortStepBeads primitive the
// index chip would show the creation-rank while the detail row showed the
// reordered rank.
func TestStepOrder_IndexDetailAgree(t *testing.T) {
	tm := func(min int) time.Time { return time.Date(2026, 7, 1, 10, min, 0, 0, time.UTC) }
	children := map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Title: "A", Type: "task", Status: "open", CreatedAt: tm(1)},
		"gd-b": {ID: "gd-b", Title: "B", Type: "task", Status: "open", CreatedAt: tm(2)},
		"gd-c": {ID: "gd-c", Title: "C", Type: "task", Status: "open", CreatedAt: tm(3)},
	}
	// Operator reordered to C, A, B.
	order := []string{"gd-c", "gd-a", "gd-b"}

	rows := buildStepRows([]string{"gd-a", "gd-b", "gd-c"}, children, order)
	if len(rows) != 3 || rows[0].ID != "gd-c" || rows[0].Number != 1 {
		t.Fatalf("detail rows should lead with gd-c #1, got %+v", rows)
	}
	// The index chip's "next" (first open) must be gd-c at #1 too — feed it the
	// OPPOSITE input order to prove the overlay, not the input order, decides.
	step := pickCurrentOrNextStep([]string{"gd-b", "gd-a", "gd-c"}, children, order)
	if step == nil || step.Title != "C" || step.Number != 1 {
		t.Fatalf("index chip should pick C #1 via the same overlay, got %+v", step)
	}
	// IsFirst/IsLast reflect the reordered positions, not creation order.
	if !rows[0].IsFirst || rows[0].IsLast {
		t.Errorf("gd-c (first after reorder) IsFirst=%v IsLast=%v, want true/false", rows[0].IsFirst, rows[0].IsLast)
	}
	if rows[2].ID != "gd-b" || !rows[2].IsLast {
		t.Errorf("gd-b should be last after reorder, got id=%s IsLast=%v", rows[2].ID, rows[2].IsLast)
	}
}

// --- render-integration test -------------------------------------------------

// TestPlanCockpit_RendersEditAffordances proves the operator cockpit renders all
// five 3e direct-edit affordances (reword form, reorder ▲▼, edit-summary,
// doc-edit, add-phase) wired to the right POST routes. Uses the phased-plan seed
// so the phase branch renders too. An operator role is required (viewerCanOperate)
// so the affordances gate correctly.
func TestPlanCockpit_RendersEditAffordances(t *testing.T) {
	f := seedPhasedPlanFake()
	// Give the bundle a doc so the doc-edit affordance renders.
	f.beads["gd-doc1"] = &beadsapi.BeadDetail{
		ID: "gd-doc1", Type: "doc", Status: "open",
		Fields: map[string]string{"format": "md", "content": "Body.", "headline": "Section 1"},
	}
	f.revDeps["gd-bundle1"] = []beadsapi.Dependency{
		{BeadID: "gd-doc1", DependsOnID: "gd-bundle1", Type: "parent-child"},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`/plans/gd-plan1/steps/gd-i2/title`,   // step reword form action
		`/plans/gd-plan1/steps/gd-i2/reorder`, // step reorder arrows
		`/plans/gd-plan1/narrative"`,          // narrative summary edit form
		`/plans/gd-plan1/docs/gd-doc1`,        // bundle-doc edit form
		`/plans/gd-plan1/phases"`,             // create-phase form
		`class="plan-tl-reorder"`,             // reorder control container
		`class="plan-step-reword`,             // reword disclosure
		`class="plan-newphase`,                // add-phase disclosure
	} {
		if !strings.Contains(body, want) {
			t.Errorf("operator cockpit missing edit affordance %q", want)
		}
	}
}

// TestPlanCockpit_HidesEditAffordancesFromViewer proves a non-operator viewer
// sees the read-only cockpit — none of the 3e write forms render.
func TestPlanCockpit_HidesEditAffordancesFromViewer(t *testing.T) {
	f := seedPhasedPlanFake()
	mux := mkServer(t, f)

	// An explicit non-operator (viewer) role → viewerCanOperate is false. (A
	// request with NO roles hits the dev bypass and is treated as operator, so
	// the read-only case must carry a real viewer role — mirroring how the
	// add-step affordance is gated, plans_test.go.)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, forbidden := range []string{
		`/steps/gd-i2/title`,
		`/steps/gd-i2/reorder`,
		`class="plan-tl-reorder"`,
		`class="plan-newphase`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("read-only cockpit must not render edit affordance %q", forbidden)
		}
	}
}

// --- handler tests -----------------------------------------------------------

// TestHandlePlanNarrativeEdit_WritesThrough proves the narrative-summary edit is
// a write-through Description PATCH on the plan epic and re-renders the fragment.
func TestHandlePlanNarrativeEdit_WritesThrough(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/narrative", "narrative=A+crisp+new+summary")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if f.updateLastID != "gd-plan1" {
		t.Errorf("updated bead = %q, want gd-plan1", f.updateLastID)
	}
	if f.updateLastReq.Description == nil || *f.updateLastReq.Description != "A crisp new summary" {
		t.Errorf("description PATCH = %v, want 'A crisp new summary'", f.updateLastReq.Description)
	}
	// The re-rendered fragment carries the new summary.
	if !strings.Contains(w.Body.String(), "A crisp new summary") {
		t.Error("re-rendered narrative fragment should show the new summary")
	}
}

// TestHandlePlanNarrativeEdit_TooLong rejects an oversized summary with the
// draft preserved and no daemon write.
func TestHandlePlanNarrativeEdit_TooLong(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	big := "narrative=" + strings.Repeat("x", maxPlanNarrativeLen+1)
	req := operatorPost("/plans/gd-plan1/narrative", big)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("oversized summary must not write; got %d UpdateBead calls", f.updateCalls)
	}
}

// TestHandlePlanNarrativeEdit_RequiresOperator: the route is operator-gated.
func TestHandlePlanNarrativeEdit_RequiresOperator(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	// No operator role / email header → the operatorOnly wrapper rejects.
	req := httptest.NewRequest(http.MethodPost, "/plans/gd-plan1/narrative",
		strings.NewReader("narrative=nope"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("anonymous narrative edit should be rejected, got 200")
	}
	if f.updateCalls != 0 {
		t.Errorf("anonymous edit must not write; got %d UpdateBead calls", f.updateCalls)
	}
}

// TestHandlePlanStepTitleEdit_RewordsStep proves a step reword is a write-through
// Title PATCH on the step bead of THIS plan.
func TestHandlePlanStepTitleEdit_RewordsStep(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/steps/gd-step1/title", "title=Wire+the+webhook+route")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if f.updateLastID != "gd-step1" {
		t.Errorf("reworded bead = %q, want gd-step1", f.updateLastID)
	}
	if f.updateLastReq.Title == nil || *f.updateLastReq.Title != "Wire the webhook route" {
		t.Errorf("title PATCH = %v, want 'Wire the webhook route'", f.updateLastReq.Title)
	}
}

// TestHandlePlanStepTitleEdit_RejectsForeignStep: a step id that is not a child
// of the plan is refused (404) and never written — the ownership guard.
func TestHandlePlanStepTitleEdit_RejectsForeignStep(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-foreign"] = &beadsapi.BeadDetail{ID: "gd-foreign", Title: "Not mine", Type: "task", Status: "open"}
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/steps/gd-foreign/title", "title=hijack")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a foreign step, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("foreign step reword must not write; got %d UpdateBead calls", f.updateCalls)
	}
}

// TestHandlePlanStepTitleEdit_RejectsNonIssueUnderPhase closes the ownership gap
// the 3e adversarial review caught: a NON-issue bead (a nested epic / doc /
// bundle) parented under a phase sub-epic is not a rendered step and must not be
// retitle-able through the reword verb. The phase branch of planStepParent used
// to resolve membership by edge only (no type check), unlike the flat branch —
// this asserts both branches now gate on isIssueType.
func TestHandlePlanStepTitleEdit_RejectsNonIssueUnderPhase(t *testing.T) {
	f := seedPhasedPlanFake()
	// A nested EPIC parented under phase gd-phaseI — a real parent-child child,
	// but not an issue-type step the timeline renders.
	f.beads["gd-nested"] = &beadsapi.BeadDetail{ID: "gd-nested", Title: "Nested sub-plan", Type: "epic", Status: "open"}
	f.revDeps["gd-phaseI"] = append(f.revDeps["gd-phaseI"],
		beadsapi.Dependency{BeadID: "gd-nested", DependsOnID: "gd-phaseI", Type: "parent-child"})
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/steps/gd-nested/title", "title=hijack+the+epic")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a non-issue bead under a phase, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("a non-issue bead must not be retitle-able; got %d UpdateBead calls", f.updateCalls)
	}
}

// TestHandlePlanStepTitleEdit_RejectsEmpty: an empty/whitespace title is rejected
// inline with no write (a step must keep a title).
func TestHandlePlanStepTitleEdit_RejectsEmpty(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/steps/gd-step1/title", "title=+++")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty title, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("empty reword must not write; got %d UpdateBead calls", f.updateCalls)
	}
}

// TestHandlePlanStepReorder_FlatWritesParentStepOrder proves reordering a flat
// step writes a SEEDED step_order to the PLAN epic (the flat step's parent), and
// that the write moves the target relative to its siblings. seedPlansFake has one
// step; add a second so a move is meaningful.
func TestHandlePlanStepReorder_FlatWritesParentStepOrder(t *testing.T) {
	f := seedPlansFake()
	// Add a second flat step so the plan has [gd-step1, gd-step2] in creation
	// order; moving gd-step2 up should write ["gd-step2","gd-step1"].
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Second step", Type: "task", Status: "open",
		CreatedAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
	}
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/steps/gd-step2/reorder?dir=up", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if f.reorderArrayLast.ID != "gd-plan1" || f.reorderArrayLast.Field != planStepFieldKey {
		t.Errorf("reorder CAS targeted %s/%s, want the plan epic gd-plan1/step_order", f.reorderArrayLast.ID, f.reorderArrayLast.Field)
	}
	// Never-reordered group: the stored step_order is absent, so the CAS expects
	// the empty array and writes the full seeded+reordered order.
	if len(f.reorderArrayLast.Expected) != 0 {
		t.Errorf("CAS expected = %v, want empty (step_order was never set)", f.reorderArrayLast.Expected)
	}
	got := f.reorderArrayLast.Next
	if len(got) != 2 || got[0] != "gd-step2" || got[1] != "gd-step1" {
		t.Errorf("step_order = %v, want [gd-step2 gd-step1] (moved gd-step2 up)", got)
	}
}

// TestHandlePlanStepReorder_PhasedWritesPhaseStepOrder proves reordering a step
// that lives under a phase writes step_order to the PHASE sub-epic, not the plan
// epic — the parent-resolution rule.
func TestHandlePlanStepReorder_PhasedWritesPhaseStepOrder(t *testing.T) {
	f := seedPhasedPlanFake()
	mux := mkServer(t, f)

	// gd-i2 lives under phase gd-phaseI (steps gd-i1, gd-i2). Move gd-i2 up.
	req := operatorPost("/plans/gd-plan1/steps/gd-i2/reorder?dir=up", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if f.reorderArrayLast.ID != "gd-phaseI" || f.reorderArrayLast.Field != planStepFieldKey {
		t.Errorf("phased reorder CAS targeted %s/%s, want the phase sub-epic gd-phaseI/step_order", f.reorderArrayLast.ID, f.reorderArrayLast.Field)
	}
	got := f.reorderArrayLast.Next
	if len(got) != 2 || got[0] != "gd-i2" || got[1] != "gd-i1" {
		t.Errorf("phase step_order = %v, want [gd-i2 gd-i1]", got)
	}
}

// TestHandlePlanStepReorder_BoundaryNoOp: moving the first step up is a harmless
// no-op — 200, no field write.
func TestHandlePlanStepReorder_BoundaryNoOp(t *testing.T) {
	f := seedPhasedPlanFake()
	mux := mkServer(t, f)

	// gd-i1 is first in phase gd-phaseI; moving it up is a boundary no-op.
	req := operatorPost("/plans/gd-plan1/steps/gd-i1/reorder?dir=up", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (no-op), got %d", w.Code)
	}
	if f.reorderArrayCalls != 0 {
		t.Errorf("boundary reorder must not issue a CAS write; got %d ReorderBeadArrayField calls", f.reorderArrayCalls)
	}
}

// TestHandlePlanStepReorder_RetriesOnCASConflict proves a flat-step reorder
// survives a transient CAS conflict: the daemon 409s the first attempts and the
// retry loop re-reads + re-derives and lands the same reordered step_order.
func TestHandlePlanStepReorder_RetriesOnCASConflict(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Second step", Type: "task", Status: "open",
		CreatedAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
	}
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.reorderArrayConflicts = 1 // first CAS 409s, second succeeds
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/steps/gd-step2/reorder?dir=up", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d body=%q", w.Code, w.Body.String())
	}
	if f.reorderArrayCalls != 2 {
		t.Errorf("expected 2 CAS attempts (1 conflict + 1 success), got %d", f.reorderArrayCalls)
	}
	got := f.reorderArrayLast.Next
	if len(got) != 2 || got[0] != "gd-step2" || got[1] != "gd-step1" {
		t.Errorf("step_order = %v, want [gd-step2 gd-step1] after retry", got)
	}
}

// TestHandlePlanCreatePhase_MintsAndGrowsPhaseOrder proves the GROW verb mints a
// type=epic child (project-scoped, linked parent-child) and APPENDS it to the
// plan's phase_order.
func TestHandlePlanCreatePhase_MintsAndGrowsPhaseOrder(t *testing.T) {
	f := seedPhasedPlanFake()
	f.createResp = "gd-phaseNew"
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/phases", "title=Validation")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	// A type=epic child was minted, project-scoped from the plan.
	if f.createLastReq.Type != "epic" {
		t.Errorf("created type = %q, want epic", f.createLastReq.Type)
	}
	if f.createLastReq.Title != "Validation" {
		t.Errorf("created title = %q, want Validation", f.createLastReq.Title)
	}
	if !containsStr(f.createLastReq.Labels, "project:gardenio") {
		t.Errorf("created labels = %v, want project:gardenio born-on", f.createLastReq.Labels)
	}
	// Linked parent-child under the plan.
	if f.addDepLastType != "parent-child" || f.addDepLastDep != "gd-plan1" || f.addDepLastBead != "gd-phaseNew" {
		t.Errorf("phase link = (%s %s -> %s), want parent-child gd-phaseNew -> gd-plan1",
			f.addDepLastBead, f.addDepLastType, f.addDepLastDep)
	}
	// phase_order GREW via the atomic append primitive: the new phase is appended
	// to the plan's phase_order (not a read-modify-write splice).
	if f.appendArrayCalls != 1 {
		t.Fatalf("expected 1 append call, got %d", f.appendArrayCalls)
	}
	if f.appendArrayLast.ID != "gd-plan1" || f.appendArrayLast.Field != planPhaseFieldKey || f.appendArrayLast.Element != "gd-phaseNew" {
		t.Errorf("append = (%s %s += %s), want gd-plan1 phase_order += gd-phaseNew",
			f.appendArrayLast.ID, f.appendArrayLast.Field, f.appendArrayLast.Element)
	}
	// The append-if-absent reflected onto the stored order: research, impl, then
	// the new phase.
	got := decodeArrayField(f.beads["gd-plan1"], planPhaseFieldKey)
	if len(got) != 3 || got[0] != "gd-phaseR" || got[1] != "gd-phaseI" || got[2] != "gd-phaseNew" {
		t.Errorf("grown phase_order = %v, want [gd-phaseR gd-phaseI gd-phaseNew]", got)
	}
}

// TestHandlePlanCreatePhase_RequiresTitle: an empty phase title is rejected with
// no create.
func TestHandlePlanCreatePhase_RequiresTitle(t *testing.T) {
	f := seedPhasedPlanFake()
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/phases", "title=+")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty phase title, got %d", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("empty phase title must not create; got %d CreateBead calls", f.createCalls)
	}
}

// TestHandlePlanDocEdit_WritesThroughBundleDoc proves editing a bundle-doc body
// is a write-through fields.content PATCH on the type=doc bead that hangs off the
// plan's narrative bundle.
func TestHandlePlanDocEdit_WritesThroughBundleDoc(t *testing.T) {
	f := seedPhasedPlanFake()
	// Hang a real doc off the narrative bundle gd-bundle1.
	f.beads["gd-doc1"] = &beadsapi.BeadDetail{
		ID: "gd-doc1", Type: "doc", Status: "open",
		Fields: map[string]string{"format": "md", "content": "Old body."},
	}
	f.revDeps["gd-bundle1"] = []beadsapi.Dependency{
		{BeadID: "gd-doc1", DependsOnID: "gd-bundle1", Type: "parent-child"},
	}
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/docs/gd-doc1", "content=A+rewritten+section+body.")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if f.updateFieldsLastID != "gd-doc1" {
		t.Errorf("doc content write on %q, want gd-doc1", f.updateFieldsLastID)
	}
	if got, _ := f.updateFieldsLastVal["content"].(string); got != "A rewritten section body." {
		t.Errorf("content PATCH = %q, want 'A rewritten section body.'", got)
	}
}

// TestHandlePlanDocEdit_RejectsImageDoc: an image/link doc carries no editable
// text body (its payload is content_url), so the edit is refused.
func TestHandlePlanDocEdit_RejectsImageDoc(t *testing.T) {
	f := seedPhasedPlanFake()
	f.beads["gd-img"] = &beadsapi.BeadDetail{
		ID: "gd-img", Type: "doc", Status: "open",
		Fields: map[string]string{"format": "image", "content_url": "https://example/x.png"},
	}
	f.revDeps["gd-bundle1"] = []beadsapi.Dependency{
		{BeadID: "gd-img", DependsOnID: "gd-bundle1", Type: "parent-child"},
	}
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/docs/gd-img", "content=nope")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an image doc, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("image doc edit must not write; got %d UpdateBeadFieldsTyped calls", f.updateFieldsCalls)
	}
}

// TestHandlePlanDocEdit_RejectsNonBundleDoc: a doc id that is not in the plan's
// narrative bundle is refused so the route can't rewrite an arbitrary bead.
func TestHandlePlanDocEdit_RejectsNonBundleDoc(t *testing.T) {
	f := seedPhasedPlanFake()
	f.beads["gd-strayDoc"] = &beadsapi.BeadDetail{ID: "gd-strayDoc", Type: "doc", Status: "open",
		Fields: map[string]string{"format": "md", "content": "unrelated"}}
	mux := mkServer(t, f)

	req := operatorPost("/plans/gd-plan1/docs/gd-strayDoc", "content=hijack")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a doc outside the plan's bundle, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("stray doc edit must not write; got %d UpdateBeadFieldsTyped calls", f.updateFieldsCalls)
	}
}

// containsStr is a tiny local slice-membership helper for the label assertions.
func containsStr(ss []string, want string) bool {
	return slices.Contains(ss, want)
}
