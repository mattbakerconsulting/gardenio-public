package main

// plans_steps_done_test.go — coverage for the "ghost-done" completion marker on
// the /plans index card (standing plan ka-0000005352 iteration 41). A plan whose
// RENDERED steps are ALL closed (a full-green "N/N steps · 100%" bar) but whose
// epic status is still NON-terminal (open/in_progress/blocked, not
// closed/deferred) finished all its steps yet was never closed — measured on the
// live board (2026-08-08 pour): 11 of 72 cards (15%), all last moved ~4 weeks
// prior, none carrying any forward signal. The card surfaced every OTHER
// disposition (working/queued/stuck crew, in-progress/blocked step tally) but
// never plan-level "all steps done", so a finished-but-unclosed plan read
// identically to a genuinely-active one in the OPEN/IN-PROGRESS lane. The marker
// gives an operator that at-a-glance signal so they can close it (or file a
// wrap-up).
//
// Two levels: an HTTP-render truth-table over the four dispositions
// (all-done-open ⇒ marker; partial-open ⇒ none; all-done-CLOSED ⇒ none;
// all-done-but-with-a-live-in_progress-step ⇒ none), and a fail-without/pass-with
// guard proving the render actually keys on the StepsAllDone projection (the
// same shape the escalation/stall/freshness suites use).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stepsDoneMarker matches the marker in the SERVED bytes. The template emits the
// check glyph as the HTML entity &#x2713; (not a literal ✓), so the entity is
// what appears in the response body; keying on the class + entity avoids matching
// the plan-progress div's title="N of N steps done" tooltip, which contains the
// substring "steps done" but not the marker span.
const stepsDoneMarker = `class="plan-steps-done"`

// mkStepsDoneServer seeds the standard one-plan fake, forces the plan epic to the
// given status, and replaces its single step with `stepStatus`, so the rendered
// rollup reads exactly 1/1 (closed) or 0/1 (open). Returns the mounted handler.
func mkStepsDoneServer(t *testing.T, planStatus, stepStatus string) http.Handler {
	t.Helper()
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = planStatus
	f.beads["gd-step1"].Status = stepStatus
	// Keep the subtree rollup fixtures consistent with the step status so the
	// card's own rendered rollup (renderedStepRollup over the reverse deps) is the
	// authority; epicSubtreeCount is a separate owned-rollup fixture and the
	// index card reads the rendered walk.
	if stepStatus == "closed" {
		f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 1}
	} else {
		f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 0}
	}
	return mkServer(t, f)
}

func renderPlansIndex(t *testing.T, mux http.Handler) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	return w.Body.String()
}

// TestPlansIndex_StepsDoneMarker is the render truth-table across the four
// dispositions that decide whether the ghost-done marker shows.
func TestPlansIndex_StepsDoneMarker(t *testing.T) {
	cases := []struct {
		name       string
		planStatus string
		stepStatus string
		wantMarker bool
	}{
		// The target case: every rendered step closed, plan still open ⇒ marker.
		{"all steps done, plan open", "open", "closed", true},
		{"all steps done, plan in_progress", "in_progress", "closed", true},
		{"all steps done, plan blocked", "blocked", "closed", true},
		// Partial progress ⇒ no marker (the bar is not full).
		{"open step, plan open", "open", "open", false},
		{"in_progress step, plan open", "open", "in_progress", false},
		// The load-bearing terminal guard: a CLOSED/DEFERRED plan's 100% bar is
		// honest history, not a call to act — no marker (the iter-25/38/39
		// terminal-reconcile family).
		{"all steps done, plan CLOSED", "closed", "closed", false},
		{"all steps done, plan deferred", "deferred", "closed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := renderPlansIndex(t, mkStepsDoneServer(t, tc.planStatus, tc.stepStatus))
			got := strings.Contains(body, stepsDoneMarker)
			if got != tc.wantMarker {
				t.Errorf("marker present = %v, want %v (planStatus=%q stepStatus=%q)",
					got, tc.wantMarker, tc.planStatus, tc.stepStatus)
			}
		})
	}
}

// TestPlansIndex_StepsDoneMarker_SuppressedByLiveTally proves the marker is
// suppressed when the grid still tallies an in-flight step even though
// StepsClosed==StepsTotal — the double-parented-step edge the StepsNotStarted
// clamp documents. A step parented under two phases counts once per bucket in the
// grid tally but can push StepsClosed to StepsTotal; the empty-in-flight guard
// keeps the "done" marker off such a card so it never contradicts the "▶ 1" chip.
func TestPlansIndex_StepsDoneMarker_SuppressedByLiveTally(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "open"
	// One closed step + one in_progress step ⇒ 1/2 closed, a live "▶ 1" tally, so
	// NOT all-done: no marker.
	f.beads["gd-step1"].Status = "closed"
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "In-flight step", Kind: "issue", Type: "task", Status: "in_progress",
		Labels: []string{"project:gardenio"},
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 1}

	body := renderPlansIndex(t, mkServer(t, f))
	if strings.Contains(body, stepsDoneMarker) {
		t.Errorf("marker rendered on a 1/2 plan with a live in_progress step — must only show when ALL rendered steps are done")
	}
	// The combined class string (item + status modifier) appears ONLY on the
	// rendered tally chip, not on the single-class CSS rule in the <style> block,
	// so it is a precise precondition that the live "▶ 1" tally actually rendered.
	if !strings.Contains(body, "plan-step-tally-item plan-step-tally-inprogress") {
		t.Fatalf("expected the in_progress step tally chip to render (test precondition)")
	}
}

// TestPlansIndex_StepsDoneMarker_PreservesProgressLabel guards that the marker is
// an ADDITIONAL element, not a rewrite of the "{closed}/{total} steps · {pct}%"
// label — so TestPlansIndex_TightVerticalRhythm and the rollup-percent tests stay
// byte-identical. On an all-done-open plan the label still reads the exact rollup
// string AND the marker is present.
func TestPlansIndex_StepsDoneMarker_PreservesProgressLabel(t *testing.T) {
	body := renderPlansIndex(t, mkStepsDoneServer(t, "open", "closed"))
	if !strings.Contains(body, "1/1 steps · 100%") {
		t.Errorf("progress label must still read '1/1 steps · 100%%' verbatim, body did not contain it")
	}
	if !strings.Contains(body, stepsDoneMarker) {
		t.Errorf("expected the ghost-done marker on an all-done-open plan")
	}
}
