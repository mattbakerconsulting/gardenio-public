package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// The index-card Dispatch button (Matt's ask, thread ka-0000001290) hands the
// plan's CURRENT/NEXT open step to its gardener and re-renders the whole /plans
// row in place. It replaces the old free-text spawn/wake message box. These
// tests exercise the three delivery paths + the guardrails (no open step, the
// button rendering on the index).

// TestPlanIndex_RendersDispatchButtonNotMessageBox proves the /plans index row
// carries the Dispatch button wired to the dispatch-next route, and NO LONGER
// carries the old free-text message box.
func TestPlanIndex_RendersDispatchButtonNotMessageBox(t *testing.T) {
	mux := plansAssistantMux(t, seedPlansFake(), &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodGet, "/plans", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-card-dispatch-btn") {
		t.Errorf("expected the Dispatch button on the index row; body did not contain plan-card-dispatch-btn")
	}
	if !strings.Contains(body, "/plans/gd-plan1/dispatch-next") {
		t.Errorf("expected the Dispatch button wired to the dispatch-next route")
	}
	// The old free-text message box must be gone.
	if strings.Contains(body, "plan-card-spawn") || strings.Contains(body, "plan-spawn-input") {
		t.Errorf("the old spawn/wake message box must be removed from the index card")
	}
}

// TestHandlePlanRowDispatch_NudgesLiveGardener: dispatching the next step on a
// plan with a LIVE gardener nudges it (sub-second) with a seed naming the step,
// mints no extra bead, and re-renders the whole row with a confirmation notice.
func TestHandlePlanRowDispatch_NudgesLiveGardener(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/dispatch-next", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 1 {
		t.Fatalf("expected 1 Nudge (live gardener), got %d", cm.nudgeCalls)
	}
	if !strings.Contains(cm.nudgeLastText, "gd-step1") {
		t.Errorf("nudge text = %q, want it to reference the next step id", cm.nudgeLastText)
	}
	if !strings.Contains(cm.nudgeLastText, "Wire the route") {
		t.Errorf("nudge text = %q, want it to name the step title", cm.nudgeLastText)
	}
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("live dispatch must not spawn a gardener; got %d agent CreateBead calls", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-card") {
		t.Errorf("expected the row (plan_card) to re-render; body=%s", body)
	}
	if !strings.Contains(body, "plan-card-dispatch-note") {
		t.Errorf("expected a confirmation notice in the re-rendered row; body=%s", body)
	}
	if !strings.Contains(body, "#1") {
		t.Errorf("expected the notice to name the step number; body=%s", body)
	}
}

// TestHandlePlanRowDispatch_SpawnsGardenerWhenNone: dispatching on a plan with NO
// gardener spawns a fresh plan-gardener carrying the targeted dispatch instruction
// as its first prompt (one agent CreateBead).
func TestHandlePlanRowDispatch_SpawnsGardenerWhenNone(t *testing.T) {
	f := seedPlansFake()
	f.createIDForReq = func(req beadsapi.CreateBeadRequest) string {
		if req.Type == "agent" {
			return "gd-capnew"
		}
		return "gd-other"
	}
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/dispatch-next", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("no live gardener means coopmux must not be nudged; got %d", cm.nudgeCalls)
	}
	if got := f.createCallsOfType("agent"); got != 1 {
		t.Fatalf("expected 1 agent CreateBead (spawned gardener), got %d", got)
	}
	var capReq *beadsapi.CreateBeadRequest
	for i := range f.createReqs {
		if f.createReqs[i].Type == "agent" {
			capReq = &f.createReqs[i]
			break
		}
	}
	if capReq == nil {
		t.Fatal("expected an agent CreateBead request")
	}
	fields := decodeFakeFields(t, capReq.Fields)
	if fields[planIDField] != "gd-plan1" {
		t.Errorf("spawn plan_id = %q, want gd-plan1", fields[planIDField])
	}
	if !strings.Contains(fields["prompt"], "gd-step1") {
		t.Errorf("spawn prompt = %q, want it to reference the next step id", fields["prompt"])
	}
	if !strings.Contains(w.Body.String(), "plan-card-dispatch-note") {
		t.Errorf("expected a confirmation notice; body=%s", w.Body.String())
	}
}

// TestHandlePlanRowDispatch_NoOpenStep: a plan whose only step is CLOSED has
// nothing to dispatch — the handler re-renders the row with an error note and
// spawns nothing.
func TestHandlePlanRowDispatch_NoOpenStep(t *testing.T) {
	f := seedPlansFake()
	// Close the only step so the plan has no current/next open step.
	f.beads["gd-step1"].Status = "closed"
	f.beads["gd-step1"].ClosedAt = "2026-07-02T10:00:00Z"
	f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 1}
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/dispatch-next", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("no open step must not spawn a gardener; got %d agent CreateBead calls", got)
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("no open step must not nudge; got %d", cm.nudgeCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-card-dispatch-note-err") {
		t.Errorf("expected an error note when there is no open step; body=%s", body)
	}
	if !strings.Contains(body, "No open step") {
		t.Errorf("expected the no-open-step message; body=%s", body)
	}
}
