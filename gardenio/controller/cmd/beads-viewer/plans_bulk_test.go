package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plans_bulk_test.go — bulk multi-select verbs on the EXISTING /plans index:
// disable/enable the bound plan gardener (task ka-0000004771) and Mark complete
// the plan itself (task ka-0000005361). A near-drop-in adaptation of the shipped
// /triggers bulk pattern: the per-bead flip reuses setGardenerEnabledCore (the
// complete verb reuses completePlanCore), and the response is the cleared bar +
// OOB-swapped plan rows.
//
// The checkbox value — and so every bead_id here — is the PLAN-EPIC id, re-keyed
// from the bound gardener's trigger-bead id by ka-0000005361 because 48 of the 56
// live open plans have no gardener and the old key hid the selection model on 86%
// of the board. The gardener verbs resolve epic → gardener server-side.

// plansBulkReq builds a POST /plans/bulk/{action} request carrying the given
// PLAN-EPIC bead_id form values, an operator email + role, so the operatorOnly
// route admits it and EmailFromRequest sees the actor. (The checkbox — and hence
// these ids — carried the bound GARDENER id until the close-out re-key
// ka-0000005361; see plans_bulk.go's header for why the epic won.)
func plansBulkReq(action string, ids ...string) *http.Request {
	form := url.Values{}
	for _, id := range ids {
		form.Add("bead_id", id)
	}
	req := httptest.NewRequest(http.MethodPost, "/plans/bulk/"+action, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	return req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
}

// seedPlansBulkFake serves two plan epics on the index, each with its own bound
// gardener, so a "select all then disable" flips BOTH — the multi-select the task
// is about. Both gardeners are running (enabled=true) so a Disable is a real flip.
func seedPlansBulkFake() *fakeDaemon {
	f := seedPlansFake()
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	seedPlanGardener(f, "gd-cap2", "gd-plan2", "true")
	return f
}

// --- backend: the bulk flip ---------------------------------------------------

// TestPlansBulkDisable_FlipsEveryFilteredGardener is the headline case: two
// selected gardeners → BOTH flip enabled=false via the shared core, and each
// affected plan row is OOB-swapped back so its unified chip updates live. This is
// the "disable all of Matt's monorepo gardeners in one click" acceptance.
func TestPlansBulkDisable_FlipsEveryFilteredGardener(t *testing.T) {
	f := seedPlansBulkFake()
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("disable", "gd-plan1", "gd-plan2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	// Both gardeners flipped through the shared core (a JSON bool, not the string).
	if f.updateFieldsCalls != 2 {
		t.Fatalf("UpdateBeadFieldsTyped: want 2 calls, got %d", f.updateFieldsCalls)
	}
	if got := f.updateFieldsLastVal["enabled"]; got != false {
		t.Errorf("enabled write: want false, got %v", got)
	}
	// The write targets the GARDENER bead ids, not the plan epics.
	var sawCap1, sawCap2 bool
	for _, c := range f.updateFieldsHistory {
		switch c.ID {
		case "gd-cap1":
			sawCap1 = true
		case "gd-cap2":
			sawCap2 = true
		}
		if c.ID == "gd-plan1" || c.ID == "gd-plan2" {
			t.Errorf("bulk flip must target the gardener bead, not the epic %s", c.ID)
		}
	}
	if !sawCap1 || !sawCap2 {
		t.Errorf("both gardeners must be flipped: cap1=%v cap2=%v", sawCap1, sawCap2)
	}
	// A best-effort audit comment per successful flip (shared-core behaviour).
	if f.addCommentCalls != 2 {
		t.Errorf("AddComment: want 2, got %d", f.addCommentCalls)
	}
	body := w.Body.String()
	// Cleared toolbar present.
	if !strings.Contains(body, `id="plans-bulk-bar"`) {
		t.Errorf("cleared toolbar missing from response\nbody=%s", snippetN(body, 800))
	}
	// OOB-swapped plan rows for BOTH affected plans, each reading the paused chip
	// now (the flip updated the unified state chip in the re-render).
	for _, epic := range []string{"gd-plan1", "gd-plan2"} {
		if !strings.Contains(body, `id="plan-card-`+epic+`"`) {
			t.Errorf("missing OOB plan row for %s\nbody=%s", epic, snippetN(body, 1200))
		}
	}
	if !strings.Contains(body, `hx-swap-oob="outerHTML"`) {
		t.Errorf("OOB swap attribute missing\nbody=%s", snippetN(body, 1200))
	}
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-paused") {
		t.Errorf("a disabled gardener's re-rendered row should read the paused chip\nbody=%s", snippetN(body, 1200))
	}
	if strings.Contains(body, "plan-agent-chip plan-agent-chip-running") {
		t.Errorf("no disabled row should still read the running chip\nbody=%s", snippetN(body, 1200))
	}
}

// TestPlansBulkEnable_FlipsEveryGardener is the symmetric resume: two paused
// gardeners → both flip enabled=true.
func TestPlansBulkEnable_FlipsEveryGardener(t *testing.T) {
	f := seedPlansFake()
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")
	seedPlanGardener(f, "gd-cap2", "gd-plan2", "false")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("enable", "gd-plan1", "gd-plan2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if f.updateFieldsCalls != 2 {
		t.Fatalf("UpdateBeadFieldsTyped: want 2, got %d", f.updateFieldsCalls)
	}
	if got := f.updateFieldsLastVal["enabled"]; got != true {
		t.Errorf("enabled write: want true, got %v", got)
	}
	// Enabled gardener, no live iteration → the calm "idle" chip, not green
	// "running" (Matt's ask, thread ka-0000001290). The flip still updated the
	// unified state chip in the re-render; it just reads idle because nothing is
	// executing yet.
	if body := w.Body.String(); !strings.Contains(body, "plan-agent-chip plan-agent-chip-idle") {
		t.Errorf("an enabled gardener's re-rendered row (no live agent) should read the idle chip\nbody=%s", snippetN(body, 1200))
	}
}

// TestPlansBulkDisable_DedupesIDs proves a duplicated bead_id (a double-checked
// row, or shift-select overlap) flips the gardener exactly once.
func TestPlansBulkDisable_DedupesIDs(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("disable", "gd-plan1", "gd-plan1", "gd-plan1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 1 {
		t.Errorf("dedupe: want 1 update call, got %d", f.updateFieldsCalls)
	}
}

// TestPlansBulkDisable_EmptyIDs proves a submit with nothing checked re-renders
// the bar with an inline error and writes nothing.
func TestPlansBulkDisable_EmptyIDs(t *testing.T) {
	f := seedPlansFake()
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("disable"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("no updates on empty ids, got %d", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "Select at least one plan") {
		t.Errorf("missing empty-selection error\nbody=%s", snippetN(w.Body.String(), 400))
	}
}

// TestPlansBulkDisable_Unauthenticated proves a request with no operator email is
// rejected before any write (the handler's own email guard, independent of the
// route RBAC).
func TestPlansBulkDisable_Unauthenticated(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	form := url.Values{}
	form.Add("bead_id", "gd-plan1")
	req := httptest.NewRequest(http.MethodPost, "/plans/bulk/disable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No X-Auth-Request-Email; operator ROLE present so the route admits it and
	// the handler's own email guard is what rejects.
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("unauthenticated must not write, got %d calls", f.updateFieldsCalls)
	}
}

// TestPlansBulkDisable_RequiresOperator proves the route is operator-gated: a
// read-only viewer never reaches the handler and nothing is written.
func TestPlansBulkDisable_RequiresOperator(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	form := url.Values{}
	form.Add("bead_id", "gd-plan1")
	req := httptest.NewRequest(http.MethodPost, "/plans/bulk/disable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("a read-only viewer must not reach the bulk handler; got 200")
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("a rejected viewer bulk must not write; got %d calls", f.updateFieldsCalls)
	}
}

// TestPlansBulkDisable_SkipsNonPlanBead proves a stray non-EPIC id in the form is
// skipped with an inline error while valid plans still flip (partial success, no
// rollback). Post-re-key the guard is "is this a plan epic?", not "is this a
// gardener?" — the checkbox now carries epic ids.
func TestPlansBulkDisable_SkipsNonPlanBead(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	f.beads["gd-task1"] = &beadsapi.BeadDetail{ID: "gd-task1", Type: "task", Status: "open"}
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("disable", "gd-plan1", "gd-task1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if f.updateFieldsCalls != 1 {
		t.Errorf("only the gardener is flipped, want 1 update, got %d", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "is not a plan") {
		t.Errorf("expected inline error about the non-plan bead\nbody=%s", snippetN(w.Body.String(), 600))
	}
}

// TestPlansBulkDisable_SkipsClosedGardener proves a closed (retired) gardener is
// rejected — nothing is written and, with no successful flip, the response is a
// 502 (all-failures), matching the trigger bulk.
func TestPlansBulkDisable_SkipsClosedGardener(t *testing.T) {
	f := seedPlansFake()
	cap := seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	cap.Status = "closed"
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("disable", "gd-plan1"))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status: want 502, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("closed gardener must not be written, got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "gardener is closed") {
		t.Errorf("expected inline error about the closed gardener\nbody=%s", snippetN(w.Body.String(), 600))
	}
}

// TestPlansBulkEnable_GateBlocksStillAuthoringPlan proves the R6 build gate holds
// on the bulk path too: enabling a gardener on a still-authoring (draft) plan is
// refused, nothing is written for it, and the bar surfaces the inline note — the
// bulk endpoint is NOT a build-gate bypass. A sibling non-gated gardener still
// flips (partial success).
func TestPlansBulkEnable_GateBlocksStillAuthoringPlan(t *testing.T) {
	f := seedPlansFake()
	// plan1 is a still-authoring draft; plan2 is ungated (building/unset).
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateDraft)}
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Second plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false") // draft plan → enable blocked
	seedPlanGardener(f, "gd-cap2", "gd-plan2", "false") // ungated → enable allowed
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("enable", "gd-plan1", "gd-plan2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	// Only the ungated gardener flipped.
	if f.updateFieldsCalls != 1 {
		t.Errorf("only the ungated gardener should flip, want 1 update, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-cap2" {
		t.Errorf("the flipped gardener should be gd-cap2 (ungated), got %q", f.updateFieldsLastID)
	}
	if !strings.Contains(w.Body.String(), "approve the plan for build") {
		t.Errorf("expected the inline build-gate note\nbody=%s", snippetN(w.Body.String(), 600))
	}
}

// --- frontend: index render ---------------------------------------------------

// TestPlansIndex_BulkBarRendersForOperator proves the /plans index carries the
// bulk scope + bar + per-row checkboxes for an operator, wired to the plans-
// scoped endpoints with the confirm-over-5 gate.
func TestPlansIndex_BulkBarRendersForOperator(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	for _, want := range []string{
		`data-bulk-scope="plans"`,
		`id="plans-bulk-bar"`,
		`hx-post="/plans/bulk/disable"`,
		`hx-post="/plans/bulk/enable"`,
		`class="plans-row-select"`,
		`data-bulk-confirm-over="5"`,
		`data-bulk-select-all`,
		// The close-out verb shares the same selection (task ka-0000005361).
		`hx-post="/plans/bulk/complete"`,
		// The checkbox value is the PLAN-EPIC id, NOT the bound gardener's id —
		// re-keyed by ka-0000005361 so the selection model reaches the 86% of open
		// plans that have no gardener at all.
		`value="gd-plan1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/plans index missing %q\nbody=%s", want, snippetN(body, 1600))
		}
	}
	// The confirm noun is the plan-gardener noun (not the trigger default).
	if !strings.Contains(body, `data-bulk-confirm-noun="plan gardeners"`) {
		t.Errorf("bulk forms should declare the plan-gardeners confirm noun\nbody=%s", snippetN(body, 1600))
	}
	// The checkbox must NOT carry the gardener id any more — that key is exactly
	// what hid the selection model on gardener-less plans.
	if strings.Contains(body, `class="plans-row-select" name="bead_id" value="gd-cap1"`) {
		t.Errorf("the bulk checkbox must carry the plan-epic id, not the gardener id\nbody=%s", snippetN(body, 1600))
	}
}

// TestPlansIndex_CheckboxRendersForGardenerlessPlan is the headline of the
// close-out re-key (task ka-0000005361): a plan with NO bound gardener still gets a
// bulk checkbox, because "Mark complete" is a verb on the PLAN and 48 of the 56
// live open plans have no gardener. Pre-re-key this row rendered no checkbox at all
// — the exact reason bulk close-out could not have worked by simply adding a verb
// to the old toolbar.
func TestPlansIndex_CheckboxRendersForGardenerlessPlan(t *testing.T) {
	f := seedPlansFake() // no gardener bound to gd-plan1

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	if !strings.Contains(body, `class="plans-row-select"`) {
		t.Errorf("a gardener-less plan must still render a bulk checkbox (it can be completed)\nbody=%s", snippetN(body, 1200))
	}
	if !strings.Contains(body, `value="gd-plan1"`) {
		t.Errorf("the checkbox should carry the plan-epic id\nbody=%s", snippetN(body, 1200))
	}
}

// TestPlansIndex_CheckboxRendersForRetiredGardener proves a retired (closed)
// gardener's plan STILL renders a checkbox post-re-key (task ka-0000005361). It
// previously rendered none — correct when the only verbs flipped the gardener, wrong
// now: a plan whose gardener has retired is a prime close-out candidate, and the
// gardener verbs report "gardener is closed" per-plan rather than needing the row
// hidden. The bulk bar itself is unchanged (page-level scope).
func TestPlansIndex_CheckboxRendersForRetiredGardener(t *testing.T) {
	f := seedPlansFake()
	cap := seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	cap.Status = "closed"

	body := plansIndexBodyOperator(t, f, "/plans?operated_by=")
	if !strings.Contains(body, `id="plans-bulk-bar"`) {
		t.Errorf("the bulk bar should still render for an operator\nbody=%s", snippetN(body, 1200))
	}
	if !strings.Contains(body, `class="plans-row-select"`) {
		t.Errorf("a retired-gardener plan must still render a bulk checkbox (it can be completed)\nbody=%s", snippetN(body, 1200))
	}
}

// TestPlansIndex_NoBulkBarForViewer proves the whole bulk affordance is operator-
// gated: a read-only viewer sees neither the bar nor any checkbox (parity with
// the per-row lever + spawn-form gates). The task's "read-only viewer sees no
// checkboxes/bar."
func TestPlansIndex_NoBulkBarForViewer(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	req := plansIndexReq("/plans?operated_by=", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `id="plans-bulk-bar"`) {
		t.Errorf("a read-only viewer must not see the bulk bar")
	}
	if strings.Contains(body, `class="plans-row-select"`) {
		t.Errorf("a read-only viewer must not see bulk checkboxes")
	}
	// But the viewer still sees plan state (the unified chip), just no levers.
	// Enabled gardener, no live agent → the idle tone (Matt's ask, thread
	// ka-0000001290); the point of this assertion is that the chip renders at all
	// for a viewer, regardless of tone.
	if !strings.Contains(body, "plan-agent-chip plan-agent-chip-idle") {
		t.Errorf("a read-only viewer should still see the gardener state chip\nbody=%s", snippetN(body, 800))
	}
}
