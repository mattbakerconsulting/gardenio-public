package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plans_gardener_control_test.go — step 5 (gd-cconyepcBc) unit coverage for the
// /plans-index plan-gardener control room (epic ka-0000004792). Steps 1-4 + 6 each
// shipped their own tests (plans_gardener_state_test.go, plans_index_test.go,
// plans_master_switch_test.go, plans_bulk_test.go); this file closes the seams
// the per-step tests left uncovered and that step 5 explicitly calls out:
//
//   - the TOGGLE-CORE REUSE seam itself: setGardenerEnabledCore exercised in
//     isolation (the closed-gardener guard, the JSON-bool write the daemon schema
//     requires, and the best-effort audit that still reports success when the
//     comment write fails). Every surface — the /gardener bar, the /plans row
//     toggle, and the bulk endpoint — funnels through this one body, so pinning it
//     directly guarantees none of them can fork the enable/disable semantics.
//   - the BULK "select all on the FILTERED set" invariant: a gardener that was
//     filtered OUT of the /plans list (never submitted in bead_id) is left
//     untouched while every submitted gardener flips. This is the acceptance the
//     task names ("disable all of Matt's monorepo gardeners … while any gardener
//     filtered OUT is untouched"); the existing bulk tests only prove the
//     submitted set flips, not that a non-submitted gardener is spared.

// --- toggle-core reuse: setGardenerEnabledCore in isolation --------------------

// coreReq builds a bare POST carrying the operator email the core stamps onto its
// audit comment. The core never inspects the route/path, only r.Context() (for
// the daemon calls) and the passed email, so a minimal request suffices.
func coreReq() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/plans/gd-plan1/gardener/pause-toggle", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	return req
}

// TestSetGardenerEnabledCore_DisableWritesJSONBool pins the load-bearing contract
// the whole control room rests on: the `enabled` field is written as a JSON
// bool, NOT the string "false". The daemon's typed-field validation 400-rejects
// the string form (ka-0000003627), so this is what keeps every toggle surface
// from silently 502-ing. Also proves the best-effort audit comment fires and the
// core returns nil (success) with the write targeting the gardener bead id.
func TestSetGardenerEnabledCore_DisableWritesJSONBool(t *testing.T) {
	f := newFake()
	f.beads["gd-cap1"] = &beadsapi.BeadDetail{
		ID: "gd-cap1", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "gardener-epic:gd-plan1"},
		Fields: map[string]string{"enabled": "true"},
	}
	s := testServerWithDaemon(f)

	opErr := s.setGardenerEnabledCore(coreReq(), f.beads["gd-cap1"], false, "op@acme.example")
	if opErr != nil {
		t.Fatalf("setGardenerEnabledCore returned error: %+v", opErr)
	}
	if f.updateFieldsCalls != 1 {
		t.Fatalf("UpdateBeadFieldsTyped: want 1 call, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-cap1" {
		t.Errorf("write targeted %q, want the gardener bead gd-cap1", f.updateFieldsLastID)
	}
	// The write MUST be a JSON bool, not the string "false" — the string form is
	// 400-rejected by the daemon field validation.
	got, ok := f.updateFieldsLastVal["enabled"]
	if !ok {
		t.Fatalf("no enabled field written: %+v", f.updateFieldsLastVal)
	}
	if b, isBool := got.(bool); !isBool || b != false {
		t.Errorf("enabled write = %#v (%T), want the bool false", got, got)
	}
	// Best-effort audit comment on the gardener bead.
	if f.addCommentCalls != 1 || f.addCommentLastID != "gd-cap1" {
		t.Errorf("audit comment: calls=%d lastID=%q, want 1 on gd-cap1", f.addCommentCalls, f.addCommentLastID)
	}
}

// TestSetGardenerEnabledCore_EnableWritesJSONBoolTrue is the symmetric resume: the
// core writes the bool true (never the string "true").
func TestSetGardenerEnabledCore_EnableWritesJSONBoolTrue(t *testing.T) {
	f := newFake()
	f.beads["gd-cap1"] = &beadsapi.BeadDetail{
		ID: "gd-cap1", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "gardener-epic:gd-plan1"},
		Fields: map[string]string{"enabled": "false"},
	}
	s := testServerWithDaemon(f)

	if opErr := s.setGardenerEnabledCore(coreReq(), f.beads["gd-cap1"], true, "op@acme.example"); opErr != nil {
		t.Fatalf("setGardenerEnabledCore returned error: %+v", opErr)
	}
	got, ok := f.updateFieldsLastVal["enabled"]
	if !ok {
		t.Fatalf("no enabled field written: %+v", f.updateFieldsLastVal)
	}
	if b, isBool := got.(bool); !isBool || b != true {
		t.Errorf("enabled write = %#v (%T), want the bool true", got, got)
	}
}

// TestSetGardenerEnabledCore_RejectsClosedGardener pins the closed-gardener guard:
// a retired (closed) gardener is refused with a 409 BEFORE any write, so a
// retired loop can never be resurrected through the shared core (the bulk
// endpoint and the /gardener bar both lean on this guard). This is the ONE guard
// every surface inherits for free by funneling through the core.
func TestSetGardenerEnabledCore_RejectsClosedGardener(t *testing.T) {
	f := newFake()
	closed := &beadsapi.BeadDetail{
		ID: "gd-cap1", Type: "trigger", Status: "closed",
		Labels: []string{"trigger:schedule", "role:gardener", "gardener-epic:gd-plan1"},
		Fields: map[string]string{"enabled": "true"},
	}
	s := testServerWithDaemon(f)

	opErr := s.setGardenerEnabledCore(coreReq(), closed, false, "op@acme.example")
	if opErr == nil {
		t.Fatal("setGardenerEnabledCore accepted a closed gardener; want a 409 error")
	}
	if opErr.Status != http.StatusConflict {
		t.Errorf("closed-gardener error status = %d, want 409", opErr.Status)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("a closed gardener must not be written, got %d update calls", f.updateFieldsCalls)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("a rejected toggle must not audit, got %d comment calls", f.addCommentCalls)
	}
}

// TestSetGardenerEnabledCore_AuditCommentIsBestEffort proves the audit comment is
// best-effort: when AddComment fails, the state change is ALREADY committed, so
// the core still reports success (nil) and the caller re-renders the new state.
// The mutation_history is the canonical record; a dropped comment must never
// snap the switch back or 502 a flip that actually landed.
func TestSetGardenerEnabledCore_AuditCommentIsBestEffort(t *testing.T) {
	f := newFake()
	f.addCommentErr = context.DeadlineExceeded // comment write fails
	f.beads["gd-cap1"] = &beadsapi.BeadDetail{
		ID: "gd-cap1", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "gardener-epic:gd-plan1"},
		Fields: map[string]string{"enabled": "true"},
	}
	s := testServerWithDaemon(f)

	opErr := s.setGardenerEnabledCore(coreReq(), f.beads["gd-cap1"], false, "op@acme.example")
	if opErr != nil {
		t.Fatalf("a failed audit comment must not fail the toggle: %+v", opErr)
	}
	// The state change still landed.
	if f.updateFieldsCalls != 1 {
		t.Errorf("the field write must land even when the audit fails, got %d calls", f.updateFieldsCalls)
	}
	// The comment WAS attempted (best-effort, not skipped).
	if f.addCommentCalls != 1 {
		t.Errorf("the audit comment should be attempted once, got %d", f.addCommentCalls)
	}
}

// TestSetGardenerEnabledCore_UpdateFailureReturns502 proves a hard field-write
// failure surfaces as a 502 gardenerOpError — the status each caller maps to its
// own inline error rendering. This is the "Failed to update" path the bulk
// endpoint counts as a per-bead failure and the /gardener bar renders as an
// http.Error.
func TestSetGardenerEnabledCore_UpdateFailureReturns502(t *testing.T) {
	f := newFake()
	f.updateFieldsErr = context.DeadlineExceeded
	open := &beadsapi.BeadDetail{
		ID: "gd-cap1", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "gardener-epic:gd-plan1"},
		Fields: map[string]string{"enabled": "true"},
	}
	s := testServerWithDaemon(f)

	opErr := s.setGardenerEnabledCore(coreReq(), open, false, "op@acme.example")
	if opErr == nil {
		t.Fatal("a failed field write must return an error")
	}
	if opErr.Status != http.StatusBadGateway {
		t.Errorf("update-failure status = %d, want 502", opErr.Status)
	}
	// A failed write must NOT emit a misleading "success" audit comment.
	if f.addCommentCalls != 0 {
		t.Errorf("a failed write must not audit, got %d comment calls", f.addCommentCalls)
	}
}

// --- bulk: the "select all on the FILTERED set" invariant ---------------------

// TestPlansBulkDisable_LeavesFilteredOutGardenerUntouched is the headline bulk
// acceptance the task names: filtering the /plans list (e.g. to Matt's monorepo
// plans), Select-all, and Disable flips EVERY filtered gardener — while a gardener
// that was filtered OUT (never rendered, so never in the submitted bead_id set)
// stays exactly as it was. handlePlans renders only the filtered rows and
// project_bulk.js's selectAll() only checks boxes already in the DOM, so the
// submitted set IS the filtered set. Here we model that at the handler boundary:
// two gardeners are submitted (the "filtered" ones) and a third is NOT — and we
// prove the third is never even loaded, let alone written.
func TestPlansBulkDisable_LeavesFilteredOutGardenerUntouched(t *testing.T) {
	f := seedPlansFake()
	// Two plans + gardeners that ARE in the filtered view.
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
	// A THIRD gardener that belongs to a plan filtered OUT of the view — it exists
	// in the daemon but is NOT submitted in the bulk form (its row was never
	// rendered, so its checkbox was never in the DOM to Select-all).
	filteredOut := seedPlanGardener(f, "gd-cap3", "gd-plan3", "true")

	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	// Only the two FILTERED plans are submitted (mirrors Select-all over the
	// filtered DOM). gd-plan3 / its gardener gd-cap3 are deliberately absent.
	mux.ServeHTTP(w, plansBulkReq("disable", "gd-plan1", "gd-plan2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	// Exactly the two filtered gardeners flipped.
	if f.updateFieldsCalls != 2 {
		t.Fatalf("want 2 flips (only the filtered gardeners), got %d", f.updateFieldsCalls)
	}
	for _, c := range f.updateFieldsHistory {
		if c.ID == "gd-cap3" {
			t.Errorf("a filtered-OUT gardener must never be written; gd-cap3 was flipped")
		}
	}
	// The filtered-out gardener's own bead is untouched — its enabled field still
	// reads its original value (nothing mutated it in place).
	if got := filteredOut.Fields["enabled"]; got != "true" {
		t.Errorf("filtered-out gardener gd-cap3 enabled = %q, want it left at true", got)
	}
	// And no OOB row was emitted for its plan — only the flipped plans re-render.
	if strings.Contains(w.Body.String(), `id="plan-card-gd-plan3"`) {
		t.Errorf("a filtered-out plan must not be OOB-swapped\nbody=%s", snippetN(w.Body.String(), 1200))
	}
}

// TestPlansBulkDisable_ReportsGardenerlessPlan pins the "no gardener bound" branch
// of the gardener verbs post-re-key (ka-0000005361). Since the checkbox now carries
// the PLAN-EPIC id, it renders on EVERY plan — including the 48-of-56 live open
// plans with no gardener — so a Select-all → Disable WILL include gardener-less
// plans. Those must be REPORTED inline, not silently skipped: a silent skip would
// make "select all + disable" look like it worked while touching nothing on most
// of the board. A sibling gardener-bound plan still flips (partial success, no
// rollback).
//
// This replaces the pre-re-key TestPlansBulkDisable_SkipsGardenerWithNoOwnedEpic,
// whose guard (a gardener bead carrying no gardener-epic binding) is unreachable now
// that the form carries epic ids rather than gardener ids.
func TestPlansBulkDisable_ReportsGardenerlessPlan(t *testing.T) {
	f := seedPlansFake()
	// plan1 HAS a gardener; plan2 deliberately has none.
	plan2 := &beadsapi.BeadDetail{
		ID: "gd-plan2", Title: "Gardenerless plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan2"] = plan2
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-plan1"], plan2}, Total: 2,
	}
	f.epicSubtreeCount["gd-plan2"] = 0
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansBulkReq("disable", "gd-plan1", "gd-plan2"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	// Only the gardener-bound plan's gardener flipped.
	if f.updateFieldsCalls != 1 {
		t.Errorf("only the gardener-bound plan should flip, want 1 update, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-cap1" {
		t.Errorf("the flipped gardener should be gd-cap1, got %q", f.updateFieldsLastID)
	}
	// The gardener-less plan is REPORTED, not silently dropped.
	if !strings.Contains(w.Body.String(), "gd-plan2 has no gardener bound") {
		t.Errorf("expected the inline note naming the gardener-less plan\nbody=%s", snippetN(w.Body.String(), 600))
	}
}

// TestPlansBulkEnable_UnknownIDSkippedValidStillFlips proves a bulk request that
// mixes a valid gardener with a bead_id the daemon can't load (a stale checkbox
// from a since-deleted plan) surfaces the load failure inline while the valid
// gardener still flips — the no-rollback partial-success policy, on the LOAD path
// specifically (the existing tests cover the guard/update failure paths, not the
// GetBead failure).
func TestPlansBulkEnable_UnknownIDSkippedValidStillFlips(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	// gd-ghost is not in f.beads → GetBead returns not-found.
	mux.ServeHTTP(w, plansBulkReq("enable", "gd-plan1", "gd-ghost"))

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 (partial success), got %d body=%s", w.Code, snippetN(w.Body.String(), 800))
	}
	if f.updateFieldsCalls != 1 {
		t.Errorf("the valid gardener should still flip, want 1 update, got %d", f.updateFieldsCalls)
	}
	if f.updateFieldsLastID != "gd-cap1" {
		t.Errorf("the flipped gardener should be gd-cap1, got %q", f.updateFieldsLastID)
	}
	if !strings.Contains(w.Body.String(), "Failed to load gd-ghost") {
		t.Errorf("expected the inline load-failure note for the unknown id\nbody=%s", snippetN(w.Body.String(), 600))
	}
}

// TestPlansBulkDisable_EmptyFilteredSelectionIsNoOp is the degenerate end of the
// filter invariant: if the filtered view renders no rows at all, a Select-all →
// Disable submits nothing and the endpoint is a clean no-op with the
// empty-selection message — never a write.
func TestPlansBulkDisable_EmptyFilteredSelectionIsNoOp(t *testing.T) {
	f := seedPlansFake()
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	// No bead_id values — the filtered DOM had no checkboxes to select.
	mux.ServeHTTP(w, plansBulkReq("disable"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("an empty filtered selection must write nothing, got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "Select at least one plan") {
		t.Errorf("missing empty-selection note\nbody=%s", snippetN(w.Body.String(), 400))
	}
}

// --- read-only viewer parity across the two remaining levers ------------------

// TestPlansMasterSwitchToggle_RequiresOperatorRole pins the ROUTE-layer operator
// gate on the master switch POST (the existing master-switch tests cover the
// handler's own email guard via a direct call, but not the operatorOnly route
// rejecting a viewer). A read-only viewer's POST never reaches the handler and
// opens no gate — parity with the render gate that hides the lever from viewers.
func TestPlansMasterSwitchToggle_RequiresOperatorRole(t *testing.T) {
	f := masterSwitchFake(t, false, false)
	mux := mkServer(t, f)

	req := httptest.NewRequest(http.MethodPost, "/plans/master-switch/toggle", nil)
	req.Header.Set("X-Auth-Request-Email", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("a read-only viewer must not reach the master switch handler; got 200")
	}
	for _, r := range f.createReqs {
		if r.Type == "gate" {
			t.Errorf("a rejected viewer toggle must not open a gate: %+v", r)
		}
	}
	if f.closeCalls != 0 {
		t.Errorf("a rejected viewer toggle must not close a gate, got %d closes", f.closeCalls)
	}
}
