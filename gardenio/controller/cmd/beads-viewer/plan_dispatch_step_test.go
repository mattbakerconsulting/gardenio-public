package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// The Dispatch button (Matt's ask, thread ka-0000002484) hands ONE open step to
// the plan's gardener, spawning one if none is bound. It routes through
// promptPlanAssistant — the SAME nudge-live / wake-parked / spawn-if-none
// primitive the add-step path and the plan-agent message box use — so a
// dispatch never mints a second unwatched agent. These tests exercise the three
// delivery paths plus the guardrails (closed step, missing ids, non-operator).

// TestPlanStepTimeline_RendersDispatchButtonOnOpenStep proves the operator view
// of the step timeline carries the Dispatch button on an OPEN step (wired to the
// dispatch route) and that a CLOSED step shows Reopen instead — never Dispatch.
func TestPlanStepTimeline_RendersDispatchButtonOnOpenStep(t *testing.T) {
	f := seedPlansFake() // gd-step1 is an open child of gd-plan1
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Closed step", Type: "task", Status: "closed",
		CreatedAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	mux := plansAssistantMux(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodGet, "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-step-dispatch") {
		t.Errorf("expected the Dispatch button on the open step; body did not contain plan-step-dispatch")
	}
	if !strings.Contains(body, "/plans/gd-plan1/steps/gd-step1/dispatch") {
		t.Errorf("expected the Dispatch button wired to the open step's dispatch route")
	}
	// The dispatch verb must NOT be offered for the closed step.
	if strings.Contains(body, "/plans/gd-plan1/steps/gd-step2/dispatch") {
		t.Errorf("closed step must not render a Dispatch button")
	}
}

// TestHandlePlanDispatchStep_NudgesLiveGardener: dispatching a step on a plan
// with a LIVE gardener nudges it (sub-second) with a seed naming the step, mints
// no extra bead, and re-renders the timeline with a confirmation notice.
func TestHandlePlanDispatchStep_NudgesLiveGardener(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/steps/gd-step1/dispatch", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 1 {
		t.Fatalf("expected 1 Nudge (live gardener), got %d", cm.nudgeCalls)
	}
	if cm.nudgeLastID != "sess-gd-asstlive" {
		t.Errorf("nudge sessionID = %q, want sess-gd-asstlive", cm.nudgeLastID)
	}
	if !strings.Contains(cm.nudgeLastText, "gd-step1") {
		t.Errorf("nudge text = %q, want it to reference the step id", cm.nudgeLastText)
	}
	if !strings.Contains(cm.nudgeLastText, "Wire the route") {
		t.Errorf("nudge text = %q, want it to name the step title", cm.nudgeLastText)
	}
	// A live nudge spawns no new agent.
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("live dispatch must not spawn a gardener; got %d agent CreateBead calls", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-steps-notice") {
		t.Errorf("expected a confirmation notice in the re-rendered timeline; body=%s", body)
	}
	if !strings.Contains(body, "plan-timeline") {
		t.Errorf("expected the step timeline to re-render; body=%s", body)
	}
}

// TestHandlePlanDispatchStep_SpawnsGardenerWhenNone: dispatching a step on a plan
// with NO gardener spawns a fresh plan-gardener carrying the targeted dispatch
// instruction as its first prompt (one agent CreateBead).
func TestHandlePlanDispatchStep_SpawnsGardenerWhenNone(t *testing.T) {
	f := seedPlansFake()
	f.createIDForReq = func(req beadsapi.CreateBeadRequest) string {
		if req.Type == "agent" {
			return "gd-capnew"
		}
		return "gd-other"
	}
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/steps/gd-step1/dispatch", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("no live gardener means coopmux must not be nudged; got %d", cm.nudgeCalls)
	}
	// Exactly one create: the spawned gardener (dispatch mutates no step bead).
	if got := f.createCallsOfType("agent"); got != 1 {
		t.Fatalf("expected 1 agent CreateBead (spawned gardener), got %d", got)
	}
	// The spawned bead is a plan-gardener carrying the dispatch instruction.
	var cap *beadsapi.CreateBeadRequest
	for i := range f.createReqs {
		if f.createReqs[i].Type == "agent" {
			cap = &f.createReqs[i]
			break
		}
	}
	if cap == nil {
		t.Fatal("expected an agent CreateBead request")
	}
	fields := decodeFakeFields(t, cap.Fields)
	if fields["role"] != planGardenerRole {
		t.Errorf("spawn role = %q, want %q", fields["role"], planGardenerRole)
	}
	if fields[planIDField] != "gd-plan1" {
		t.Errorf("spawn plan_id = %q, want gd-plan1", fields[planIDField])
	}
	if !strings.Contains(fields["prompt"], "gd-step1") {
		t.Errorf("spawn prompt = %q, want it to reference the step id", fields["prompt"])
	}
	if !strings.Contains(w.Body.String(), "plan-steps-notice") {
		t.Errorf("expected a confirmation notice; body=%s", w.Body.String())
	}
}

// TestHandlePlanDispatchStep_WakesParkedGardener: dispatching a step on a plan
// with a PARKED gardener revives THAT bead in place (session_resume=true, status
// flipped to in_progress) — no new bead, no coopmux Nudge.
func TestHandlePlanDispatchStep_WakesParkedGardener(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstpark", "gardenio-plan-asst-park", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/steps/gd-step1/dispatch", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("parked wake must not call coopmux Nudge; got %d", cm.nudgeCalls)
	}
	// No new agent bead — the parked gardener is revived in place.
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("parked wake must not mint a new gardener; got %d agent CreateBead calls", got)
	}
	// ONE atomic write (ka-0000001583): the wake flips status + refreshes fields in
	// a single UpdateBead, so the session_resume flag rides on updateLastReq.Fields.
	if f.updateLastID != "gd-asstpark" || f.updateLastReq.Fields["session_resume"] != "true" {
		t.Errorf("wake should refresh session_resume=true on gd-asstpark; got id=%q fields=%v",
			f.updateLastID, f.updateLastReq.Fields)
	}
}

// TestHandlePlanDispatchStep_ClosedStepIsNoOp: dispatching an already-done step
// is a no-op — no gardener contact, a friendly notice, and 200.
func TestHandlePlanDispatchStep_ClosedStepIsNoOp(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "closed"
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/steps/gd-step1/dispatch", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("a closed step must not contact the gardener; got %d nudges", cm.nudgeCalls)
	}
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("a closed step must not spawn a gardener; got %d agent CreateBead calls", got)
	}
	if !strings.Contains(w.Body.String(), "already done") {
		t.Errorf("expected the 'already done' notice; body=%s", w.Body.String())
	}
}

// TestHandlePlanDispatchStep_GardenerFailureSurfacesError: a hard spawn failure
// re-renders the timeline with an inline error rather than a bare HTTP error, so
// the operator keeps the step list in view.
func TestHandlePlanDispatchStep_GardenerFailureSurfacesError(t *testing.T) {
	f := seedPlansFake()
	f.createErr = errors.New("daemon exploded")
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/steps/gd-step1/dispatch", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on spawn failure, got %d body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to hand the step") {
		t.Errorf("expected the inline failure notice; body=%s", w.Body.String())
	}
}

func TestHandlePlanDispatchStep_NonOperatorForbidden(t *testing.T) {
	f := seedPlansFake()
	mux := plansAssistantMux(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodPost, "/plans/gd-plan1/steps/gd-step1/dispatch", nil)
	req.Header.Set("X-Auth-Request-Email", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("viewer must not be able to dispatch a step; got 200")
	}
	if f.createCallsOfType("agent") != 0 {
		t.Errorf("viewer dispatch spawned %d gardeners, want 0", f.createCallsOfType("agent"))
	}
}

// TestDispatchNoticeLabel keys the operator-facing confirmation on the delivery
// outcome and prefers the gardener's real name when known.
func TestDispatchNoticeLabel(t *testing.T) {
	if got := dispatchNoticeLabel(planDeliverySent, "gardenio-cap-1"); !strings.Contains(got, "gardenio-cap-1") || !strings.Contains(got, "live") {
		t.Errorf("sent label = %q, want it to name the gardener + say it's live", got)
	}
	if got := dispatchNoticeLabel(planDeliverySpawning, ""); !strings.Contains(got, "Spawning") {
		t.Errorf("spawning label = %q, want it to say Spawning", got)
	}
	if got := dispatchNoticeLabel(planDeliveryQueuedWaking, ""); !strings.Contains(strings.ToLower(got), "waking") {
		t.Errorf("queued label = %q, want it to mention waking", got)
	}
}
