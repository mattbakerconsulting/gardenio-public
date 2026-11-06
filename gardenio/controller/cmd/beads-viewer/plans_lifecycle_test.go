package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// seedPlanEpicWithState seeds a plan epic carrying an explicit plan_state, so a
// test can exercise the approve→build lifecycle from any starting state. An
// empty state seeds a legacy/hand-marked plan (ungated).
func seedPlanEpicWithState(f *fakeDaemon, id string, st planState) *beadsapi.BeadDetail {
	epic := seedPlanEpic(f, id)
	if st != planStateUnset {
		if epic.Fields == nil {
			epic.Fields = map[string]string{}
		}
		epic.Fields[planStateFieldKey] = string(st)
	}
	return epic
}

// --- planStateOf / gardenerEnableGatedByState unit coverage ---

func TestPlanStateOf(t *testing.T) {
	cases := []struct {
		name  string
		bead  *beadsapi.BeadDetail
		want  planState
		gated bool
	}{
		{"nil bead", nil, planStateUnset, false},
		{"no fields", &beadsapi.BeadDetail{ID: "gd-x"}, planStateUnset, false},
		{"empty state", &beadsapi.BeadDetail{Fields: map[string]string{planStateFieldKey: ""}}, planStateUnset, false},
		{"unknown value", &beadsapi.BeadDetail{Fields: map[string]string{planStateFieldKey: "bogus"}}, planStateUnset, false},
		{"draft", &beadsapi.BeadDetail{Fields: map[string]string{planStateFieldKey: "draft"}}, planStateDraft, true},
		{"ready", &beadsapi.BeadDetail{Fields: map[string]string{planStateFieldKey: "ready"}}, planStateReady, true},
		{"building", &beadsapi.BeadDetail{Fields: map[string]string{planStateFieldKey: "building"}}, planStateBuilding, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planStateOf(tc.bead); got != tc.want {
				t.Errorf("planStateOf = %q, want %q", got, tc.want)
			}
			if got := gardenerEnableGatedByState(planStateOf(tc.bead)); got != tc.gated {
				t.Errorf("gardenerEnableGatedByState = %v, want %v", got, tc.gated)
			}
		})
	}
}

// --- The R6 gate: enabling the gardener is blocked while the plan is still
// authoring (draft / ready), allowed once building or on an ungated plan. ---

// TestPlanGardenerToggle_BlockedWhileDraft: resuming (enabling) a paused gardener
// on a DRAFT plan is refused — the operator must Approve & build first.
func TestPlanGardenerToggle_BlockedWhileDraft(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateDraft)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "") // paused → toggle would enable
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/toggle", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("toggle: expected 200 (inline note), got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("gated enable must NOT write; got %d UpdateBeadFieldsTyped calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "Approve the plan for build") {
		t.Errorf("expected the gate note; body=%s", w.Body.String())
	}
}

// TestPlanGardenerToggle_BlockedWhileReady: same gate at ready — building is the
// only state in which enabling is permitted.
func TestPlanGardenerToggle_BlockedWhileReady(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateReady)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/toggle", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("gated enable must NOT write; got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "Approve the plan for build") {
		t.Errorf("expected the gate note; body=%s", w.Body.String())
	}
}

// TestPlanGardenerToggle_PauseAllowedWhileDraft: the gate is ENABLE-only —
// pausing a running gardener is always allowed (an operator can always stop a
// loop), even mid-draft.
func TestPlanGardenerToggle_PauseAllowedWhileDraft(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateDraft)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "") // running → toggle disables
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/toggle", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsLastID != "gd-cap1" || f.updateFieldsLastVal["enabled"] != false {
		t.Errorf("pause must write enabled=false to the gardener even on a draft; got id=%q val=%v", f.updateFieldsLastID, f.updateFieldsLastVal)
	}
}

// TestPlanGardenerToggle_EnableAllowedWhileBuilding: once building, the gardener
// may be resumed normally (the gate is open).
func TestPlanGardenerToggle_EnableAllowedWhileBuilding(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateBuilding)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "") // paused → toggle enables
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/toggle", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsLastVal["enabled"] != true {
		t.Errorf("enabling a building plan's gardener should write enabled=true, got %v", f.updateFieldsLastVal["enabled"])
	}
}

// TestPlanGardenerToggle_UngatedPlanUnaffected: a plan that never opted into the
// lifecycle (no plan_state) toggles exactly as before this task — enabling is
// NOT blocked. Guards backward compatibility for every pre-existing plan.
func TestPlanGardenerToggle_UngatedPlanUnaffected(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1") // no plan_state
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/gardener/toggle", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsLastVal["enabled"] != true {
		t.Errorf("ungated plan: resume should write enabled=true, got %v", f.updateFieldsLastVal["enabled"])
	}
}

// --- draft ↔ ready lifecycle (/plans/{id}/state) ---

// TestPlanState_MarkReady flips a draft to ready via an explicit state=ready
// form value.
func TestPlanState_MarkReady(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateDraft)
	mux := planGardenerMux(t, f)

	form := url.Values{"state": {"ready"}}
	req := operatorPost("/plans/gd-plan1/state", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("state: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastID != "gd-plan1" || f.updateFieldsLastVal[planStateFieldKey] != "ready" {
		t.Errorf("mark ready should write plan_state=ready to the epic; got id=%q val=%v", f.updateFieldsLastID, f.updateFieldsLastVal)
	}
	// The re-rendered bar carries the ready chip + the Approve & build affordance.
	if !strings.Contains(w.Body.String(), `id="plan-agent"`) {
		t.Errorf("state response should be the gardener bar fragment; body=%s", w.Body.String())
	}
}

// TestPlanState_KeepEditing reverts a ready plan to draft (state=draft).
func TestPlanState_KeepEditing(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateReady)
	mux := planGardenerMux(t, f)

	form := url.Values{"state": {"draft"}}
	req := operatorPost("/plans/gd-plan1/state", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsLastVal[planStateFieldKey] != "draft" {
		t.Errorf("keep editing should write plan_state=draft; got %v", f.updateFieldsLastVal[planStateFieldKey])
	}
}

// TestPlanState_TogglesWithoutValue: absent an explicit state value, the lever
// toggles draft↔ready.
func TestPlanState_TogglesWithoutValue(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateDraft)
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/state", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsLastVal[planStateFieldKey] != "ready" {
		t.Errorf("toggle from draft should write ready; got %v", f.updateFieldsLastVal[planStateFieldKey])
	}
}

// TestPlanState_BuildingRefusesFlip: a building plan can't be flipped back to
// draft/ready via the state lever — the operator pauses the gardener instead.
func TestPlanState_BuildingRefusesFlip(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateBuilding)
	mux := planGardenerMux(t, f)

	form := url.Values{"state": {"draft"}}
	req := operatorPost("/plans/gd-plan1/state", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("building plan state flip must NOT write; got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "pause the gardener") {
		t.Errorf("expected the building-refusal note; body=%s", w.Body.String())
	}
}

// TestPlanState_NoOpTransitionSkipsWrite: requesting the state the plan is
// already in re-renders without a spurious write or audit comment.
func TestPlanState_NoOpTransitionSkipsWrite(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateReady)
	mux := planGardenerMux(t, f)

	form := url.Values{"state": {"ready"}}
	req := operatorPost("/plans/gd-plan1/state", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("no-op transition must NOT write; got %d calls", f.updateFieldsCalls)
	}
}

// --- Approve & build (/plans/{id}/build) ---

// TestPlanBuild_ApprovesAndStartsGardener is the happy path: a ready plan with a
// bound (paused) gardener is flipped to building AND its gardener is enabled, in
// one gesture.
func TestPlanBuild_ApprovesAndStartsGardener(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateReady)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "") // gardener paused until approval
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/build", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("build: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	// Two writes, in order: epic plan_state=building, then gardener enabled=true.
	if len(f.updateFieldsHistory) != 2 {
		t.Fatalf("build should write twice (state then enable); got %d: %+v", len(f.updateFieldsHistory), f.updateFieldsHistory)
	}
	if f.updateFieldsHistory[0].ID != "gd-plan1" || f.updateFieldsHistory[0].Fields[planStateFieldKey] != "building" {
		t.Errorf("first write should flip the epic to building; got %+v", f.updateFieldsHistory[0])
	}
	if f.updateFieldsHistory[1].ID != "gd-cap1" || f.updateFieldsHistory[1].Fields["enabled"] != true {
		t.Errorf("second write should enable the gardener; got %+v", f.updateFieldsHistory[1])
	}
}

// TestPlanBuild_RefusedWhileDraft: a draft plan can't be built directly — the
// operator must mark it ready first (the two-step deliberate gate).
func TestPlanBuild_RefusedWhileDraft(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateDraft)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/build", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("building a draft must NOT write; got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "Mark the plan ready") {
		t.Errorf("expected the draft-refusal note; body=%s", w.Body.String())
	}
}

// TestPlanBuild_NoGardenerConflicts: approving a ready plan with no bound gardener
// re-renders with an inline note (nothing to enable) and does not flip state.
func TestPlanBuild_NoGardener(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateReady)
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/build", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("build with no gardener must NOT flip state; got %d calls", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "No gardener is bound") {
		t.Errorf("expected the no-gardener note; body=%s", w.Body.String())
	}
}

// TestPlanBuild_IdempotentWhileBuilding: approving an already-building plan is a
// no-op re-render (double-click safe), no second write.
func TestPlanBuild_IdempotentWhileBuilding(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateBuilding)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/build", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateFieldsCalls != 0 {
		t.Errorf("re-approving a building plan must NOT write; got %d calls", f.updateFieldsCalls)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- Bar rendering: the lifecycle chip + levers surface per state ---

func TestPlanGardenerBar_RendersLifecycleAffordances(t *testing.T) {
	cases := []struct {
		name       string
		state      planState
		wantChip   string   // substring that must appear
		wantLevers []string // substrings that must appear
		absent     []string // substrings that must NOT appear
	}{
		{
			name:       "draft shows Mark ready + Abandon (cheap exit)",
			state:      planStateDraft,
			wantChip:   "plan-lifecycle-chip-draft",
			wantLevers: []string{"Mark ready", "/plans/gd-plan1/state", "Abandon", "/plans/gd-plan1/abandon"},
			absent:     []string{"Approve &amp; build"},
		},
		{
			name:       "ready shows Approve & build + Keep editing + Abandon",
			state:      planStateReady,
			wantChip:   "plan-lifecycle-chip-ready",
			wantLevers: []string{"Approve &amp; build", "Keep editing", "/plans/gd-plan1/build", "Abandon", "/plans/gd-plan1/abandon"},
			absent:     []string{"Mark ready"},
		},
		{
			name:       "building shows the building chip + Mark complete + Abandon, no approve lever",
			state:      planStateBuilding,
			wantChip:   "plan-lifecycle-chip-building",
			wantLevers: []string{"the bound gardener is building", "Mark complete", "/plans/gd-plan1/complete", "Abandon", "/plans/gd-plan1/abandon"},
			absent:     []string{"Approve &amp; build", "Mark ready", "/plans/gd-plan1/reopen"},
		},
		{
			name:       "complete shows the complete chip + Reopen, no complete/abandon lever",
			state:      planStateComplete,
			wantChip:   "plan-lifecycle-chip-complete",
			wantLevers: []string{"Reopen", "/plans/gd-plan1/reopen"},
			absent:     []string{"Mark complete", "Approve &amp; build", "/plans/gd-plan1/complete", "/plans/gd-plan1/abandon"},
		},
		{
			name:       "abandoned shows the abandoned chip + Reopen, no complete/abandon lever",
			state:      planStateAbandoned,
			wantChip:   "plan-lifecycle-chip-abandoned",
			wantLevers: []string{"Reopen", "/plans/gd-plan1/reopen"},
			absent:     []string{"Mark complete", "Approve &amp; build", "/plans/gd-plan1/complete", "/plans/gd-plan1/abandon"},
		},
		{
			name:       "unset shows no lifecycle chrome",
			state:      planStateUnset,
			wantChip:   "",
			wantLevers: nil,
			absent:     []string{"plan-lifecycle-chip", "Approve &amp; build", "Mark ready", "Abandon"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			seedPlanEpicWithState(f, "gd-plan1", tc.state)
			seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "")
			mux := planGardenerMux(t, f)

			// GET /plans/{id}/gardener renders the bar standalone through the parsed
			// partial set — the real render path (handlePlanGardenerView).
			req := httptest.NewRequest("GET", "/plans/gd-plan1/agent", nil)
			req.Header.Set("X-Auth-Request-Email", "op@acme.example")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("gardener bar: expected 200, got %d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()

			if tc.wantChip != "" && !strings.Contains(body, tc.wantChip) {
				t.Errorf("want chip %q in body; got %s", tc.wantChip, body)
			}
			for _, want := range tc.wantLevers {
				if !strings.Contains(body, want) {
					t.Errorf("want %q in body; got %s", want, body)
				}
			}
			for _, no := range tc.absent {
				if strings.Contains(body, no) {
					t.Errorf("did NOT want %q in body; got %s", no, body)
				}
			}
		})
	}
}

// --- Terminal completion (/plans/{id}/complete + /reopen), task ka-0000000616 ---

// TestPlanComplete_ClosesAndDisables is the happy path: a building plan with a
// bound (enabled) gardener is stamped plan_state=complete, the gardener is
// disabled, and the plan epic is closed — the three terminal writes.
func TestPlanComplete_ClosesAndDisables(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateBuilding)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "") // running → complete disables it
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/complete", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	// Gardener disabled (enabled=false) BEFORE the state/close writes.
	var sawDisable, sawComplete bool
	for _, c := range f.updateFieldsHistory {
		if c.ID == "gd-cap1" && c.Fields["enabled"] == false {
			sawDisable = true
		}
		if c.ID == "gd-plan1" && c.Fields[planStateFieldKey] == "complete" {
			sawComplete = true
		}
	}
	if !sawDisable {
		t.Errorf("complete should disable the gardener (enabled=false); history=%+v", f.updateFieldsHistory)
	}
	if !sawComplete {
		t.Errorf("complete should stamp plan_state=complete; history=%+v", f.updateFieldsHistory)
	}
	if f.closeCalls != 1 || f.closeLastID != "gd-plan1" {
		t.Errorf("complete should close the plan epic once; closeCalls=%d lastID=%q", f.closeCalls, f.closeLastID)
	}
}

// TestPlanComplete_NoGardenerStillCompletes: a plan with no bound gardener still
// completes — it stamps complete + closes the epic (nothing to disable).
func TestPlanComplete_NoGardenerStillCompletes(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateBuilding)
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/complete", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastVal[planStateFieldKey] != "complete" {
		t.Errorf("complete should stamp plan_state=complete; got %v", f.updateFieldsLastVal[planStateFieldKey])
	}
	if f.closeCalls != 1 {
		t.Errorf("complete should close the plan epic once; got %d", f.closeCalls)
	}
}

// TestPlanComplete_IdempotentWhenAlreadyComplete: a plan already complete + closed
// re-renders without a second close.
func TestPlanComplete_IdempotentWhenAlreadyComplete(t *testing.T) {
	f := newFake()
	epic := seedPlanEpicWithState(f, "gd-plan1", planStateComplete)
	epic.Status = "closed"
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/complete", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.closeCalls != 0 {
		t.Errorf("re-completing a closed+complete plan must NOT close again; got %d", f.closeCalls)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestPlanReopen_ReopensAndRestoresBuilding: a completed plan is reopened
// (status=open) and its plan_state restored to building; the gardener is left as-is.
func TestPlanReopen_ReopensAndRestoresBuilding(t *testing.T) {
	f := newFake()
	epic := seedPlanEpicWithState(f, "gd-plan1", planStateComplete)
	epic.Status = "closed"
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/reopen", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reopen: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateLastID != "gd-plan1" || f.updateLastReq.Status == nil || *f.updateLastReq.Status != "open" {
		t.Errorf("reopen should set the epic status=open; got id=%q req=%+v", f.updateLastID, f.updateLastReq)
	}
	if f.updateFieldsLastVal[planStateFieldKey] != "building" {
		t.Errorf("reopen should restore plan_state=building; got %v", f.updateFieldsLastVal[planStateFieldKey])
	}
	// The gardener must NOT be re-enabled on reopen (R6: no silent restart).
	for _, c := range f.updateFieldsHistory {
		if c.ID == "gd-cap1" && c.Fields["enabled"] == true {
			t.Errorf("reopen must NOT re-enable the gardener; history=%+v", f.updateFieldsHistory)
		}
	}
}

// --- Terminal abandonment (/plans/{id}/abandon), task ka-0000001377 ---

// TestPlanAbandon_ClosesStampsAndDisables: a building plan abandoned stamps
// plan_state=abandoned, disables the gardener, and closes the epic — the same
// close+disable as complete but with the distinct terminal state.
func TestPlanAbandon_ClosesStampsAndDisables(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateBuilding)
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/abandon", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("abandon: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var sawDisable, sawAbandon bool
	for _, c := range f.updateFieldsHistory {
		if c.ID == "gd-cap1" && c.Fields["enabled"] == false {
			sawDisable = true
		}
		if c.ID == "gd-plan1" && c.Fields[planStateFieldKey] == "abandoned" {
			sawAbandon = true
		}
	}
	if !sawDisable {
		t.Errorf("abandon should disable the gardener (enabled=false); history=%+v", f.updateFieldsHistory)
	}
	if !sawAbandon {
		t.Errorf("abandon should stamp plan_state=abandoned; history=%+v", f.updateFieldsHistory)
	}
	if f.closeCalls != 1 || f.closeLastID != "gd-plan1" {
		t.Errorf("abandon should close the plan epic once; closeCalls=%d lastID=%q", f.closeCalls, f.closeLastID)
	}
}

// TestPlanAbandon_FromDraft: a draft plan can be abandoned directly (the cheap
// exit needs no walk-forward to building).
func TestPlanAbandon_FromDraft(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateDraft)
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/abandon", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("abandon from draft: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateFieldsLastVal[planStateFieldKey] != "abandoned" {
		t.Errorf("abandon should stamp plan_state=abandoned; got %v", f.updateFieldsLastVal[planStateFieldKey])
	}
	if f.closeCalls != 1 {
		t.Errorf("abandon should close the plan epic once; got %d", f.closeCalls)
	}
}

// TestPlanAbandon_IdempotentWhenAlreadyAbandoned: an already abandoned + closed
// plan re-renders without a second close.
func TestPlanAbandon_IdempotentWhenAlreadyAbandoned(t *testing.T) {
	f := newFake()
	epic := seedPlanEpicWithState(f, "gd-plan1", planStateAbandoned)
	epic.Status = "closed"
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/abandon", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.closeCalls != 0 {
		t.Errorf("re-abandoning a closed+abandoned plan must NOT close again; got %d", f.closeCalls)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestPlanReopen_FromAbandoned: an abandoned plan reopens to building exactly
// like a completed one (both terminal states share the single back-edge).
func TestPlanReopen_FromAbandoned(t *testing.T) {
	f := newFake()
	epic := seedPlanEpicWithState(f, "gd-plan1", planStateAbandoned)
	epic.Status = "closed"
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/reopen", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reopen from abandoned: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "open" {
		t.Errorf("reopen should set the epic status=open; got req=%+v", f.updateLastReq)
	}
	if f.updateFieldsLastVal[planStateFieldKey] != "building" {
		t.Errorf("reopen should restore plan_state=building; got %v", f.updateFieldsLastVal[planStateFieldKey])
	}
}

// TestPlanReopen_NoOpWhenOpen: reopening an already-open, non-complete plan
// re-renders without writes (a stale click).
func TestPlanReopen_NoOpWhenOpen(t *testing.T) {
	f := newFake()
	seedPlanEpicWithState(f, "gd-plan1", planStateBuilding) // open + building
	mux := planGardenerMux(t, f)

	req := operatorPost("/plans/gd-plan1/reopen", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if f.updateCalls != 0 || f.updateFieldsCalls != 0 {
		t.Errorf("reopening an open plan must NOT write; updateCalls=%d fieldsCalls=%d", f.updateCalls, f.updateFieldsCalls)
	}
}

// TestPlanComplete_HandClosedPlanOffersReopen: a plan closed BY HAND (no terminal
// plan_state) still renders the Reopen lever, so a legacy close isn't stranded.
func TestPlanComplete_HandClosedPlanOffersReopen(t *testing.T) {
	f := newFake()
	epic := seedPlanEpicWithState(f, "gd-plan1", planStateBuilding)
	epic.Status = "closed" // closed by hand, plan_state still "building"
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "false", "open", "")
	mux := planGardenerMux(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1/agent", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "/plans/gd-plan1/reopen") {
		t.Errorf("a hand-closed plan should offer Reopen; body=%s", body)
	}
	if strings.Contains(body, "/plans/gd-plan1/complete") {
		t.Errorf("a closed plan must NOT offer Mark complete; body=%s", body)
	}
}
