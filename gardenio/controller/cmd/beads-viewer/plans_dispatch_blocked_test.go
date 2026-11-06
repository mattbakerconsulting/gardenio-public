package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// iter-44 (gd-9NXJlh1T97): a BLOCKED current/next step must render NO actionable
// "Dispatch #N →" button. pickCurrentOrNextStep returns the first NON-CLOSED
// step, which INCLUDES a status=blocked step; iter-14 (ka-0000002840) already
// paints that a red "BLOCKED · cannot start" now/next chip and flagged the
// remaining hole in its own comment — "the same step feeds the Dispatch button,
// so it read as dispatchable when it is not." Offering a live Dispatch button
// (whose confirm + gardener seed both say the step "is ready to start") beside a
// red BLOCKED chip is the card-vs-truth contradiction iter-25 killed for a
// TERMINAL plan. The fix gates the button (and a defense-in-depth handler
// refusal) on planCardStep.Dispatchable(), paired 1:1 with Kind(). Found live on
// the 2026-08-09 prod board: gd-N5eejfdOsh, ka-0000008223, gd-iZgUPihhfc each
// rendered a blocked next-step chip AND the Dispatch button.

// TestPlanCardStep_Dispatchable is the unit-level source of truth: Dispatchable
// mirrors Kind()'s three-state switch — true for a "now" (in_progress) and a
// "next" (ready open) step, false ONLY for a "blocked" step — so the button gate
// and the chip label can never drift.
func TestPlanCardStep_Dispatchable(t *testing.T) {
	cases := []struct {
		status   string
		wantKind string
		want     bool
	}{
		{"in_progress", "now", true},
		{"open", "next", true},
		{"deferred", "next", true}, // any non-closed, non-blocked status reads "next"
		{"blocked", "blocked", false},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			s := planCardStep{ID: "gd-x", Number: 1, Title: "t", Status: c.status}
			if got := s.Kind(); got != c.wantKind {
				t.Errorf("Kind()=%q, want %q", got, c.wantKind)
			}
			if got := s.Dispatchable(); got != c.want {
				t.Errorf("Dispatchable()=%v, want %v", got, c.want)
			}
			// Anti-drift: Dispatchable is exactly "not blocked" — assert the pairing
			// holds against Kind() directly, the property the two are locked on.
			if got := s.Dispatchable(); got != (s.Kind() != "blocked") {
				t.Errorf("Dispatchable() must equal (Kind() != blocked); got %v for kind %q", got, s.Kind())
			}
		})
	}
}

// seedPlanWithBlockedNextStep returns a fake whose sole live plan's current/next
// step is BLOCKED (the first non-closed step is status=blocked) — the exact live
// shape a card renders "BLOCKED #1 …" for. A prior closed step is present so the
// plan reads as genuinely-active (in_progress), not terminal.
func seedPlanWithBlockedNextStep() *fakeDaemon {
	f := seedPlansFake()
	// Step 1 closed (so the plan has real progress, and #2 is the current/next).
	f.beads["gd-step1"].Status = "closed"
	f.beads["gd-step1"].ClosedAt = "2026-07-02T10:00:00Z"
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	// Step 2 BLOCKED — pickCurrentOrNextStep surfaces it as the current/next step.
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Wedged on a dependency", Kind: "issue", Type: "task",
		Status: "blocked", Labels: []string{"project:gardenio"},
		CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 1}
	return f
}

// TestPlansIndex_BlockedNextStepRendersNoDispatchButton proves the operator
// Dispatch button is suppressed when the current/next step is blocked — the
// button gates on .CurrentOrNextStep.Dispatchable, false for a blocked step.
// Fail-without/pass-with: dropping the .Dispatchable term from the template gate
// reds this test.
func TestPlansIndex_BlockedNextStepRendersNoDispatchButton(t *testing.T) {
	f := seedPlanWithBlockedNextStep()
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the rendered button markup, not the bare class (the class also
	// appears in the page <style> block).
	if strings.Contains(body, `class="bead-action plan-card-dispatch-btn"`) {
		t.Errorf("a plan whose current/next step is BLOCKED must render no Dispatch button")
	}
	if strings.Contains(body, "/plans/gd-plan1/dispatch-next") {
		t.Errorf("a blocked-next-step plan must not wire the dispatch-next POST on its card")
	}
}

// TestPlansIndex_BlockedNextStepStillRendersBlockedChip is the surgical guard: the
// button drops but the now/next CHIP stays — iter-14's red "BLOCKED" pill is the
// whole point (it tells the operator WHAT is wedged). Only the actionable button
// is suppressed, not the informational chip.
func TestPlansIndex_BlockedNextStepStillRendersBlockedChip(t *testing.T) {
	f := seedPlanWithBlockedNextStep()
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-card-step`) {
		t.Errorf("a blocked-next-step plan MUST still render the now/next step chip (the informational BLOCKED pill)")
	}
	if !strings.Contains(body, `plan-step-kind-blocked">blocked</span>`) {
		t.Errorf("a blocked next step MUST render the red 'blocked' pill (iter-14); body did not contain it")
	}
}

// TestPlansIndex_ReadyNextStepStillRendersDispatchButton is the anti-over-reach
// guard: a plan whose current/next step is a ready OPEN ("next") step MUST still
// render the Dispatch button — the gate suppresses ONLY a blocked step, never a
// dispatchable one (protecting the whole point of the button).
func TestPlansIndex_ReadyNextStepStillRendersDispatchButton(t *testing.T) {
	f := seedPlanWithBlockedNextStep()
	// Flip the blocked step to a ready open one — now it IS dispatchable.
	f.beads["gd-step2"].Status = "open"
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="bead-action plan-card-dispatch-btn"`) {
		t.Errorf("a plan whose current/next step is a ready OPEN step MUST still render the Dispatch button")
	}
}

// TestHandlePlanRowDispatch_BlockedStepIsNoOp proves the defense-in-depth handler
// guard: a stale Dispatch POST against a plan whose current/next step is blocked
// does NOT nudge or cold-spawn a gardener — it re-renders the row with an honest
// note. Guards the window where a page loaded before the step was blocked still
// carries a live button.
func TestHandlePlanRowDispatch_BlockedStepIsNoOp(t *testing.T) {
	f := seedPlanWithBlockedNextStep()
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/dispatch-next", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("a blocked next step must not spawn a gardener; got %d agent CreateBead calls", got)
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("a blocked next step must not nudge a gardener; got %d nudge calls", cm.nudgeCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-card-dispatch-note-err") {
		t.Errorf("expected an error note for a blocked-step dispatch; body=%s", body)
	}
	if !strings.Contains(body, "blocked") {
		t.Errorf("expected the note to name the blocked condition; body=%s", body)
	}
}

// TestHandlePlanRowDispatch_ReadyStepStillDispatches is the handler anti-over-reach
// twin: the same shape but with a READY next step still dispatches (spawns the
// gardener), proving the blocked guard is surgical and does not suppress a genuine
// dispatch.
func TestHandlePlanRowDispatch_ReadyStepStillDispatches(t *testing.T) {
	f := seedPlanWithBlockedNextStep()
	f.beads["gd-step2"].Status = "open" // ready → dispatchable
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
	if got := f.createCallsOfType("agent"); got != 1 {
		t.Fatalf("a ready next step MUST still dispatch (spawn the gardener); got %d agent CreateBead calls", got)
	}
	if !strings.Contains(w.Body.String(), "plan-card-dispatch-note") {
		t.Errorf("expected a confirmation notice for a ready-step dispatch; body=%s", w.Body.String())
	}
}
