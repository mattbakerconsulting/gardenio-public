package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/oidcauth"
)

// iter-27 (ka-0000005352): a TERMINAL plan (closed/deferred) must not render a
// stale NON-terminal authoring-lifecycle chip (draft/ready/building) on either
// the /plans index card OR the /plans/{id} cockpit header. A plan can be closed
// while its plan_state field still reads a non-terminal value — a hand-close, a
// drain cascade, or a "Mark complete" that closed the epic without stamping
// plan_state=complete never rewinds the field. The chip then rendered that stale
// value beside the closed status chip, most egregiously "building" whose tooltip
// claims "the bound gardener is dispatching crew step by step" on a FINISHED
// plan. Live proof (2026-08-07 daemon pour, 143 plan epics): 15 closed plans
// carried a non-terminal chip — 6 building, 6 draft, 3 ready. This is the same
// card-vs-truth family iter-19/23 (stall), iter-24 (freshness) and iter-25
// (terminal CTA) closed. planLifecycleChipState reconciles the DISPLAY chip
// against the epic's terminal status; both surfaces render off the reconciled
// value so they agree by construction.

// TestPlanLifecycleChipState is the crux truth table: the pure reconcile must
// suppress every non-terminal chip on a terminal plan, preserve a genuine
// complete chip, and pass every non-terminal plan through unchanged.
func TestPlanLifecycleChipState(t *testing.T) {
	cases := []struct {
		name   string
		st     planState
		status string
		want   planState
	}{
		// Terminal plan (closed/deferred) + stale non-terminal state ⇒ suppressed.
		{"closed+building suppressed", planStateBuilding, "closed", planStateUnset},
		{"closed+draft suppressed", planStateDraft, "closed", planStateUnset},
		{"closed+ready suppressed", planStateReady, "closed", planStateUnset},
		{"deferred+building suppressed", planStateBuilding, "deferred", planStateUnset},
		{"deferred+draft suppressed", planStateDraft, "deferred", planStateUnset},
		// Terminal plan + genuine terminal state ⇒ preserved (truthful signal).
		{"closed+complete preserved", planStateComplete, "closed", planStateComplete},
		{"deferred+complete preserved", planStateComplete, "deferred", planStateComplete},
		// Terminal plan already ungated ⇒ stays ungated (no chip either way).
		{"closed+unset stays unset", planStateUnset, "closed", planStateUnset},
		// Non-terminal plan ⇒ every state passes through unchanged (the common
		// case — an open/in_progress plan's chip reads exactly as before).
		{"open+building unchanged", planStateBuilding, "open", planStateBuilding},
		{"open+draft unchanged", planStateDraft, "open", planStateDraft},
		{"in_progress+ready unchanged", planStateReady, "in_progress", planStateReady},
		{"open+complete unchanged", planStateComplete, "open", planStateComplete},
		{"blocked+building unchanged", planStateBuilding, "blocked", planStateBuilding},
		{"open+unset unchanged", planStateUnset, "open", planStateUnset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planLifecycleChipState(tc.st, tc.status); got != tc.want {
				t.Errorf("planLifecycleChipState(%q, %q) = %q, want %q", tc.st, tc.status, got, tc.want)
			}
		})
	}
}

// seedTerminalPlanWithState seeds a plan epic that is CLOSED (or deferred) but
// still carries an explicit non-terminal plan_state — the exact live shape (a
// closed plan whose plan_state was never rewound).
func seedTerminalPlanWithState(status string, st planState) *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = status
	if f.beads["gd-plan1"].Fields == nil {
		f.beads["gd-plan1"].Fields = map[string]string{}
	}
	f.beads["gd-plan1"].Fields[planStateFieldKey] = string(st)
	return f
}

// TestPlansIndex_TerminalPlanSuppressesStaleLifecycleChip proves a CLOSED /
// DEFERRED plan carrying a stale non-terminal plan_state renders NO lifecycle
// chip on the index card. Without the LifecycleChip reconcile the card would
// render plan-lifecycle-chip-building (etc.) beside the closed status chip.
func TestPlansIndex_TerminalPlanSuppressesStaleLifecycleChip(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		for _, st := range []planState{planStateBuilding, planStateDraft, planStateReady} {
			t.Run(status+"/"+string(st), func(t *testing.T) {
				f := seedTerminalPlanWithState(status, st)
				mux := mkServer(t, f)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
				if w.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d", w.Code)
				}
				body := w.Body.String()
				// The rendered chip element carries the state in its class + as its
				// text. Assert on the class-with-state (the <style> block only names
				// the class WITHOUT a trailing quote/`>`), so this can't false-match
				// the CSS rules for .plan-lifecycle-chip-building.
				badChip := `plan-lifecycle-chip-` + string(st) + `"`
				if strings.Contains(body, badChip) {
					t.Errorf("%s plan with stale plan_state=%s must render NO lifecycle chip; card had %q", status, st, badChip)
				}
				// The most egregious symptom: the "dispatching crew" tooltip on a
				// finished plan.
				if st == planStateBuilding && strings.Contains(body, "the bound gardener is dispatching crew step by step") {
					t.Errorf("%s plan must not claim the gardener is dispatching crew (building tooltip)", status)
				}
			})
		}
	}
}

// TestPlansIndex_TerminalCompletePlanKeepsChip proves the reconcile is not
// over-reach: a plan CLOSED with a genuine terminal plan_state=complete STILL
// renders its "complete" chip — that is the truthful terminal signal, and it now
// carries the correct "Complete — …" tooltip (the index previously dropped the
// complete branch, so it fell through to the "Building…" copy).
func TestPlansIndex_TerminalCompletePlanKeepsChip(t *testing.T) {
	f := seedTerminalPlanWithState("closed", planStateComplete)
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `plan-lifecycle-chip-complete"`) {
		t.Errorf("a closed+complete plan MUST keep its complete lifecycle chip")
	}
	// The complete tooltip branch the index dropped before iter-27 — a completed
	// plan must read the SAME copy the cockpit header shows, not "Building…".
	if !strings.Contains(body, "Complete — this plan is done") {
		t.Errorf("a complete plan's chip must carry the Complete tooltip, not fall through to Building")
	}
	if strings.Contains(body, "the bound gardener is dispatching crew step by step") {
		t.Errorf("a complete plan must not render the Building 'dispatching crew' tooltip")
	}
}

// TestPlansIndex_ActivePlanKeepsLifecycleChip is the anti-over-reach guard: an
// OPEN/in_progress plan with plan_state=building MUST still render its building
// chip — the reconcile suppresses the chip only on a TERMINAL plan, never on a
// live one.
func TestPlansIndex_ActivePlanKeepsLifecycleChip(t *testing.T) {
	f := seedTerminalPlanWithState("in_progress", planStateBuilding)
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `plan-lifecycle-chip-building"`) {
		t.Errorf("a live in_progress plan MUST still render its building lifecycle chip")
	}
}

// TestPlanCockpit_TerminalPlanSuppressesStaleLifecycleChip proves index↔cockpit
// PARITY: the cockpit header must also drop the stale non-terminal chip on a
// terminal plan (both surfaces reconcile through planLifecycleChipState), while
// the Reopen lever — gated on the RAW plan_state via LifecycleGated — still
// renders so a closed plan is not stranded.
func TestPlanCockpit_TerminalPlanSuppressesStaleLifecycleChip(t *testing.T) {
	f := seedTerminalPlanWithState("closed", planStateBuilding)
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `plan-lifecycle-chip-building"`) {
		t.Errorf("cockpit header of a closed plan with stale plan_state=building must render NO building lifecycle chip")
	}
	if strings.Contains(body, "the bound gardener is dispatching crew step by step") {
		t.Errorf("cockpit of a closed plan must not claim the gardener is dispatching crew")
	}
}

// TestPlanCockpit_LifecycleChipDefaultsToRawState guards the pure projection:
// planGardenerViewFromBead has no epic status in hand, so it must default
// LifecycleChip to the raw plan_state (the reconcile is applied later, only on
// the full-card render path). A caller that reads the view without the render
// path still sees the true state.
func TestPlanCockpit_LifecycleChipDefaultsToRawState(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	cap := seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	s := mkPlanGardenerServer(f)
	v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, planStateBuilding, true, "")
	if v.LifecycleChip != planStateBuilding {
		t.Errorf("planGardenerViewFromBead must default LifecycleChip to the raw state; got %q want %q", v.LifecycleChip, planStateBuilding)
	}
}
