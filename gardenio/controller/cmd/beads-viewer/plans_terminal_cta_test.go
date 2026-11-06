package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// iter-25 (ka-0000000857): a TERMINAL plan (closed/deferred) must render NO
// live-work call-to-action — neither the "NEXT #N" now/next chip nor the
// operator "Dispatch #N →" button. Both gate on planRowView.CurrentOrNextStep,
// which planIndexEnrich now nils for a terminal plan (planStatusTerminal). On a
// finished/parked plan a leftover un-closed child step is not work anyone is
// expected to pick up, so surfacing "next #N" + a Dispatch button was a
// card-vs-truth contradiction (the mission's false-freshness family, iter-7/19/
// 23/24). Found live on the prod board: ka-0000000539 / ka-0000005928 /
// ka-0000000492 / ka-0000002350 all closed yet each rendered "next #N" + Dispatch.

// seedTerminalPlanWithOpenStep returns a fake whose sole plan epic is CLOSED but
// still carries one non-closed child step — the exact live shape (a closed plan
// with a leftover open step). status lets a caller flip to "deferred" too.
func seedTerminalPlanWithOpenStep(status string) *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = status
	// A leftover open step: pickCurrentOrNextStep would surface it as "next".
	f.beads["gd-step1"].Status = "open"
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 0}
	return f
}

// TestPlansIndex_TerminalPlanRendersNoNowNextChip proves a CLOSED plan with a
// leftover open step renders no now/next step chip (the affordant deep-link or
// the plain div) — without the planIndexEnrich guard the card would render
// "plan-card-step" with a "next" kind for the leftover step.
func TestPlansIndex_TerminalPlanRendersNoNowNextChip(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		t.Run(status, func(t *testing.T) {
			f := seedTerminalPlanWithOpenStep(status)
			mux := mkServer(t, f)
			w := httptest.NewRecorder()
			// status=all so the terminal plan is not filtered out of the query.
			mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			// The step title only appears via the now/next chip on the index card
			// (the task-grid square carries it in title=/aria-label= attrs, so we
			// assert on the chip's own markup, not the bare title).
			if strings.Contains(body, `class="plan-card-step`) {
				t.Errorf("%s plan must render no now/next step chip; the card had a plan-card-step element", status)
			}
			// Belt-and-suspenders: the "next" pill word must not appear either.
			if strings.Contains(body, `plan-step-kind-next">next</span>`) {
				t.Errorf("%s plan must not render the calm 'next' pill for a leftover open step", status)
			}
		})
	}
}

// TestPlansIndex_TerminalPlanRendersNoDispatchButton proves the operator
// Dispatch button is gone from a terminal plan's card — the button gates on
// CurrentOrNextStep.ID, which the guard nils, so a finished plan no longer
// offers to cold-spawn a gardener onto a leftover step.
func TestPlansIndex_TerminalPlanRendersNoDispatchButton(t *testing.T) {
	f := seedTerminalPlanWithOpenStep("closed")
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the rendered button markup, not the bare class — the class name
	// also appears in the page's <style> block (the shared .plan-card-dispatch-btn
	// rules), so a bare-substring check would false-match the CSS.
	if strings.Contains(body, `class="bead-action plan-card-dispatch-btn"`) {
		t.Errorf("a closed plan must render no Dispatch button element")
	}
	if strings.Contains(body, "/plans/gd-plan1/dispatch-next") {
		t.Errorf("a closed plan must not wire the dispatch-next POST on its card")
	}
}

// TestPlansIndex_ActivePlanStillRendersCTA is the anti-over-reach guard: an
// in_progress plan with an open step MUST still render both the now/next chip
// and the Dispatch button — the guard must suppress the CTA only on a TERMINAL
// plan, never on a live one (protecting iter-7/14's now/next + the Dispatch
// affordance).
func TestPlansIndex_ActivePlanStillRendersCTA(t *testing.T) {
	f := seedTerminalPlanWithOpenStep("in_progress") // reuse the shape, but live
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-card-step`) {
		t.Errorf("a live in_progress plan MUST still render the now/next step chip")
	}
	if !strings.Contains(body, `class="bead-action plan-card-dispatch-btn"`) {
		t.Errorf("a live in_progress plan MUST still render the Dispatch button element")
	}
}

// TestPlansIndex_TerminalPlanKeepsTaskGridAndProgress proves the guard is
// surgical: a closed plan STILL renders its task-grid and progress bar — that
// history is honest, not a call to act. Only the CTA affordances are suppressed.
func TestPlansIndex_TerminalPlanKeepsTaskGridAndProgress(t *testing.T) {
	f := seedTerminalPlanWithOpenStep("closed")
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-taskgrid"`) {
		t.Errorf("a closed plan must still render its task-grid (honest history)")
	}
	if !strings.Contains(body, "plan-progress-label") {
		t.Errorf("a closed plan must still render its progress bar (honest history)")
	}
}

// TestHandlePlanRowDispatch_TerminalPlanIsNoOp proves the defense-in-depth
// handler guard: a stale Dispatch POST against a now-closed plan does NOT
// cold-spawn a gardener — it re-renders the row with an honest note. This
// guards the window where a page loaded before the plan closed still carries a
// live button.
func TestHandlePlanRowDispatch_TerminalPlanIsNoOp(t *testing.T) {
	f := seedTerminalPlanWithOpenStep("closed")
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/dispatch-next", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if got := f.createCallsOfType("agent"); got != 0 {
		t.Errorf("a terminal plan must not spawn a gardener; got %d agent CreateBead calls", got)
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("a terminal plan must not nudge a gardener; got %d nudge calls", cm.nudgeCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-card-dispatch-note-err") {
		t.Errorf("expected an error note for a terminal-plan dispatch; body=%s", body)
	}
	if !strings.Contains(body, "closed") {
		t.Errorf("expected the note to name the terminal status; body=%s", body)
	}
}

// TestPlanIndexEnrich_TerminalPlanNilsCurrentOrNextStep is the unit-level crux:
// planIndexEnrich must nil CurrentOrNextStep for a terminal plan and keep it for
// a live one, from the SAME leftover-open-step shape — the single seam both the
// page render and renderPlanRowWith share.
func TestPlanIndexEnrich_TerminalPlanNilsCurrentOrNextStep(t *testing.T) {
	cases := []struct {
		status  string
		wantCTA bool
	}{
		{"closed", false},
		{"deferred", false},
		{"in_progress", true},
		{"open", true},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			f := seedTerminalPlanWithOpenStep(c.status)
			s := planAssistantTestServer(f)
			rows := s.planIndexEnrich(t.Context(), []*beadsapi.BeadDetail{f.beads["gd-plan1"]})
			if len(rows) != 1 {
				t.Fatalf("expected 1 enriched row, got %d", len(rows))
			}
			gotCTA := rows[0].CurrentOrNextStep != nil
			if gotCTA != c.wantCTA {
				t.Errorf("status=%s: CurrentOrNextStep!=nil = %v, want %v", c.status, gotCTA, c.wantCTA)
			}
		})
	}
}
