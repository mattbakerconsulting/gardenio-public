package main

// iter-28 (ka-0000005352): finish the terminal-lifecycle-chip reconcile.
//
// iter-27 (gd-OP9vykZaKC, MR !6973) reconciled the DISPLAY lifecycle chip
// against the epic's terminal status (planLifecycleChipState) on TWO surfaces —
// the /plans INDEX card (row.LifecycleChip) and the /plans/{id} COCKPIT header
// (planGardenerView.LifecycleChip) — so a closed/deferred plan no longer shows a
// stale "building"/"draft"/"ready" chip. Its own wrap-up named the remaining
// follow-up: three MORE render sites still showed the raw plan_state word, so a
// FINISHED plan read "building" on the deck/peek while the index correctly
// suppressed it — a card-vs-truth self-contradiction ACROSS surfaces:
//
//   - the /plans/{id}/card deck fragment  (deck_card.go   → deck_card.html)
//   - the /plans/{id}/peek drawer         (plans_board.go → plan_peek.html)
//   - the /deck mobile card face          (deck.go        → deck.html)
//
// This iteration routes all three display values through the SAME
// planLifecycleChipState iter-27 shipped — display-only, no gate touched (the
// deck's Approve&build gates on the Go-derived planAttentionReady tier; the
// peek's Mark-complete/Reopen levers gate on the raw plan_state). Live proof
// (2026-08-07 daemon pour, 500 plan epics): 15 closed plans carried a stale
// non-terminal plan_state (6 building / 6 draft / 3 ready).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// seedDeckCardTerminalState clones seedDeckCardFatFace's plan but stamps a
// terminal status + a stale non-terminal plan_state — the exact live shape a
// hand-close / drain-cascade leaves (status terminal, plan_state never rewound).
func seedDeckCardTerminalState(status string, st planState) *fakeDaemon {
	f := seedDeckCardFatFace()
	f.beads["gd-plan1"].Status = status
	f.beads["gd-plan1"].Fields[planStateFieldKey] = string(st)
	return f
}

// TestDeckCard_TerminalPlanSuppressesStaleLifecycleChip proves the /card deck
// fragment drops the stale non-terminal chip on a terminal plan. Without the
// planLifecycleChipState reconcile in handlePlanCard, the fragment rendered
// deck-card-state-building (etc.) beside the closed status chip.
func TestDeckCard_TerminalPlanSuppressesStaleLifecycleChip(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		for _, st := range []planState{planStateBuilding, planStateDraft, planStateReady} {
			t.Run(status+"/"+string(st), func(t *testing.T) {
				mux := mkServer(t, seedDeckCardTerminalState(status, st))
				req := httptest.NewRequest("GET", "/plans/gd-plan1/card", nil)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d", w.Code)
				}
				body := w.Body.String()
				// deck_card.html renders the chip as deck-card-state-<state> — assert
				// on the class-with-value closing quote so this can't false-match any
				// CSS rule naming the class.
				badChip := `deck-card-state-` + string(st) + `"`
				if strings.Contains(body, badChip) {
					t.Errorf("%s deck card with stale plan_state=%s must render NO lifecycle chip; body had %q", status, st, badChip)
				}
			})
		}
	}
}

// TestDeckCard_TerminalCompletePlanKeepsChip is the anti-over-reach guard: a
// plan CLOSED with a genuine plan_state=complete STILL shows its complete chip —
// that is the truthful terminal signal the reconcile preserves.
func TestDeckCard_TerminalCompletePlanKeepsChip(t *testing.T) {
	mux := mkServer(t, seedDeckCardTerminalState("closed", planStateComplete))
	req := httptest.NewRequest("GET", "/plans/gd-plan1/card", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, `deck-card-state-complete"`) {
		t.Errorf("a closed+complete deck card MUST keep its complete lifecycle chip")
	}
}

// TestDeckCard_ActivePlanKeepsLifecycleChip is the anti-over-reach guard for the
// common case: an OPEN plan with plan_state=building MUST still render its
// building chip — the reconcile suppresses only on a TERMINAL plan.
func TestDeckCard_ActivePlanKeepsLifecycleChip(t *testing.T) {
	mux := mkServer(t, seedDeckCardTerminalState("in_progress", planStateBuilding))
	req := httptest.NewRequest("GET", "/plans/gd-plan1/card", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, `deck-card-state-building"`) {
		t.Errorf("a live in_progress deck card MUST still render its building lifecycle chip")
	}
}

// seedPeekTerminalState stamps seedPlansFake's gd-plan1 to a terminal status +
// stale plan_state so the peek drawer renders the exact live shape.
func seedPeekTerminalState(status string, st planState) *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = status
	if f.beads["gd-plan1"].Fields == nil {
		f.beads["gd-plan1"].Fields = map[string]string{}
	}
	f.beads["gd-plan1"].Fields[planStateFieldKey] = string(st)
	return f
}

// TestPlanPeek_TerminalPlanSuppressesStaleLifecycleChip proves the slide-out
// drawer drops the stale non-terminal chip on a terminal plan — while the Reopen
// lever (gated on the RAW plan_state, ShowReopen) still renders so a closed plan
// is not stranded.
func TestPlanPeek_TerminalPlanSuppressesStaleLifecycleChip(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		for _, st := range []planState{planStateBuilding, planStateDraft, planStateReady} {
			t.Run(status+"/"+string(st), func(t *testing.T) {
				mux := mkServer(t, seedPeekTerminalState(status, st))
				req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
				req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d", w.Code)
				}
				body := w.Body.String()
				badChip := `plan-peek-state-` + string(st) + `"`
				if strings.Contains(body, badChip) {
					t.Errorf("%s peek drawer with stale plan_state=%s must render NO lifecycle chip; body had %q", status, st, badChip)
				}
				// The display suppression must NOT disturb the terminal levers: a
				// closed/deferred plan still offers Reopen (gated on the raw state).
				if !strings.Contains(body, "plan-lifecycle-reopen") {
					t.Errorf("%s peek drawer must still render the Reopen lever (gated on the raw plan_state)", status)
				}
			})
		}
	}
}

// TestPlanPeek_ActivePlanKeepsLifecycleChip guards the common case: an OPEN plan
// with plan_state=building still renders its building chip in the drawer.
func TestPlanPeek_ActivePlanKeepsLifecycleChip(t *testing.T) {
	mux := mkServer(t, seedPeekTerminalState("in_progress", planStateBuilding))
	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, `plan-peek-state-building"`) {
		t.Errorf("a live in_progress peek drawer MUST still render its building lifecycle chip")
	}
}

// seedDeckPageTerminalPlan builds a one-plan deck where the plan is TERMINAL
// (deferred) with a stale plan_state AND in an attention tier (stalled) so it is
// NOT filtered out of the deck. A deferred plan reaches the index via
// activeEnumStatuses; a stall keeps it on the deck (deckCards drops only the
// none tier).
func seedDeckPageTerminalPlan(st planState) *fakeDaemon {
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID:     "gd-plan1",
		Title:  "Deferred plan with a stale lifecycle state",
		Kind:   "issue",
		Type:   "epic",
		Status: "deferred",
		Labels: []string{"plan", "project:gardenio"},
		Fields: map[string]string{planStateFieldKey: string(st)},
	}
	// An in_progress step with no live agent + a gardener-less plan gives the row a
	// stall (planAttentionStalled), so deckCards keeps it. The step is a
	// parent-child child of the plan.
	step := &beadsapi.BeadDetail{
		ID:     "gd-step1",
		Title:  "An orphaned step",
		Kind:   "issue",
		Type:   "task",
		Status: "open",
		Labels: []string{"project:gardenio"},
	}
	f.beads[plan.ID] = plan
	f.beads[step.ID] = step
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {{BeadID: "gd-step1", DependsOnID: "gd-plan1", Type: "parent-child"}},
	}
	return f
}

// TestDeckPage_TerminalPlanSuppressesStaleLifecycleChip proves the /deck mobile
// card face drops the stale non-terminal chip on a terminal plan (deckCards
// reconciles PlanState through planLifecycleChipState at the projection site).
func TestDeckPage_TerminalPlanSuppressesStaleLifecycleChip(t *testing.T) {
	for _, st := range []planState{planStateBuilding, planStateDraft, planStateReady} {
		t.Run(string(st), func(t *testing.T) {
			body := getDeck(t, seedDeckPageTerminalPlan(st), "/deck").Body.String()
			badChip := `plan-lifecycle-chip-` + string(st) + `"`
			if strings.Contains(body, badChip) {
				t.Errorf("deferred deck card with stale plan_state=%s must render NO lifecycle chip; body had %q", st, badChip)
			}
		})
	}
}

// TestDeckCards_ReconcilesTerminalPlanState is the projection-level crux for the
// /deck page: deckCards must reconcile a terminal row's PlanState to the display
// value, while an open row passes through unchanged.
func TestDeckCards_ReconcilesTerminalPlanState(t *testing.T) {
	rows := []planRowView{
		// A deferred (terminal) plan with a stale building state, forced onto the
		// deck by a stall tier — its display PlanState must reconcile to unset.
		{ID: "gd-term", Status: "deferred", PlanState: string(planStateBuilding), Stall: planStallInfo{Stalled: true}},
		// An open plan in the ready tier — passes through unchanged.
		{ID: "gd-open", Status: "open", PlanState: string(planStateReady)},
	}
	cards := deckCards(rows)
	got := map[string]string{}
	for _, c := range cards {
		got[c.PlanID] = c.PlanState
	}
	if got["gd-term"] != string(planStateUnset) {
		t.Errorf("terminal deck card PlanState = %q, want %q (reconciled)", got["gd-term"], planStateUnset)
	}
	if got["gd-open"] != string(planStateReady) {
		t.Errorf("open deck card PlanState = %q, want %q (unchanged)", got["gd-open"], planStateReady)
	}
}
