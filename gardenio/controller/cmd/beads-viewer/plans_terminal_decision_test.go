package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_terminal_decision_test.go — iter-46 (ka-0000005352): a TERMINAL plan
// (closed/deferred) must NOT render the loud "N to answer" open-decision chip.
// The chip is the #1 attention tier — planAttention ranks an open-decision plan
// at planAttentionDecision (= iota, the first/most-urgent tier, ABOVE even the
// ⛔ escalation badge) — so it reads as a board-topping "you must answer this
// NOW" call. On a FINISHED (closed) or parked (deferred) plan that is a
// card-vs-truth contradiction: a leftover un-closed child step can still carry an
// open clarifying-question decision, but the plan those steps belong to is over,
// so answering it advances nothing. This is the decision twin of iter-39's
// ShowEscalationBadge suppression (whose own doc claimed the escalation badge was
// "the LAST unreconciled forward/act-now signal" — but the HIGHER-tier decision
// chip beside it was overlooked). Found live on the prod board: closed plans
// ka-0000003452 (3 open decisions) + ka-0000000492 (1) rendered the loud chip on
// a done plan. The fix is display-only — planRowView.ShowDecisionChip gates only
// the chip FACE; the raw OpenDecisions count is untouched so the attention ranker
// + board lane are byte-identical.

// --- unit truth-table: planRowView.ShowDecisionChip --------------------------

// TestPlanRowView_ShowDecisionChip is the pure crux: the chip FACE renders iff
// the plan has an open decision AND is non-terminal. A closed/deferred plan
// suppresses it; a live plan with the same decision still shows it; a plan with
// no open decision never shows it (byte-identical to today, whatever the status).
func TestPlanRowView_ShowDecisionChip(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		openDec int
		want    bool
	}{
		// Non-terminal + open decision → the loud chip renders (protect iter-4).
		{"in_progress with decision", "in_progress", 3, true},
		{"open with decision", "open", 1, true},
		{"blocked with decision", "blocked", 2, true},
		// Terminal + open decision → suppressed (the reconcile).
		{"closed with decision", "closed", 3, false},
		{"deferred with decision", "deferred", 1, false},
		// No open decision → never renders, whatever the status.
		{"in_progress no decision", "in_progress", 0, false},
		{"closed no decision", "closed", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := planRowView{Status: c.status, OpenDecisions: c.openDec}
			if got := r.ShowDecisionChip(); got != c.want {
				t.Errorf("ShowDecisionChip() = %v, want %v (status=%s, openDecisions=%d)",
					got, c.want, c.status, c.openDec)
			}
		})
	}
}

// --- HTTP render: fail-without / pass-with -----------------------------------

// TestPlansIndex_TerminalPlanSuppressesDecisionChip is the render regression: a
// CLOSED (and a DEFERRED) plan with an open decision child renders NO "N to
// answer" chip. Reverting the template seam (gating on .OpenDecisions instead of
// .ShowDecisionChip) reds this — the terminal plan would render the loud chip
// again. seedPlansFake already builds gd-plan1 with one open decision child
// (gd-dec1) that renders "1 to answer"; flipping the plan status to a terminal
// one is the whole setup, exactly like iter-39's seedTerminalPlanWithEscalation.
func TestPlansIndex_TerminalPlanSuppressesDecisionChip(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		t.Run(status, func(t *testing.T) {
			f := seedPlansFake()
			f.beads["gd-plan1"].Status = status
			mux := mkServer(t, f)
			w := httptest.NewRecorder()
			// status=all so the terminal plan is not filtered out of the query.
			mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			// Assert on the rendered chip ANCHOR, not the bare class — the
			// chip-attention class also appears in the page's <style> block, so a
			// bare-substring check would false-match the CSS.
			if strings.Contains(body, `<a class="chip chip-attention`) {
				t.Errorf("%s plan must render NO 'N to answer' decision chip; the card had one", status)
			}
			// Belt-and-suspenders: the chip's headline text must be gone too.
			if strings.Contains(body, "1 to answer") {
				t.Errorf("%s plan must not render the '1 to answer' chip face", status)
			}
		})
	}
}

// TestPlansIndex_ActivePlanStillShowsDecisionChip is the anti-over-reach guard:
// an in_progress plan with the SAME open-decision shape MUST still render the
// loud "N to answer" chip — the reconcile must quiet it ONLY on a terminal plan,
// never on a live one (protecting the iter-4 wait-age signal). seedPlansFake's
// default status is already in_progress, so this is the un-mutated shape.
func TestPlansIndex_ActivePlanStillShowsDecisionChip(t *testing.T) {
	f := seedPlansFake() // default status is in_progress
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<a class="chip chip-attention`) {
		t.Errorf("a live in_progress plan MUST still render the 'N to answer' decision chip")
	}
	if !strings.Contains(body, "1 to answer") {
		t.Errorf("a live in_progress plan MUST still render the '1 to answer' chip face")
	}
}

// --- non-regression: the raw signal (lane + ranker) is untouched -------------

// TestTerminalPlanDecision_BoardLaneUnchanged proves the reconcile is
// display-only: a CLOSED plan with an open decision still lanes to Closed (its
// raw status wins in planBoardLaneKey regardless), and a LIVE decision plan still
// lanes by its own work-state (an in_progress plan with only an open decision —
// no escalation, no stall — is genuinely in flight, so it lanes to in_progress).
// The chip suppression must never move a card between lanes.
func TestTerminalPlanDecision_BoardLaneUnchanged(t *testing.T) {
	cases := []struct {
		status   string
		wantLane string
	}{
		{"closed", "closed"},
		{"deferred", "deferred"},
		{"in_progress", "in_progress"},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			r := planRowView{Status: c.status, OpenDecisions: 3}
			if got := planBoardLaneKey(r); got != c.wantLane {
				t.Errorf("planBoardLaneKey(status=%s) = %q, want %q — the reconcile must not change lanes",
					c.status, got, c.wantLane)
			}
			// And the raw count the ranker reads is preserved on every status.
			if r.OpenDecisions != 3 {
				t.Errorf("status=%s: OpenDecisions = %d, want 3 (raw signal must survive)", c.status, r.OpenDecisions)
			}
		})
	}
}

// TestTerminalPlanDecision_AttentionTierUntouched proves the attention ranker
// still sees the true decision state: planAttention keys off OpenDecisions, which
// the display-only reconcile never mutates, so a live decision plan is still
// ranked planAttentionDecision (the #1 tier).
func TestTerminalPlanDecision_AttentionTierUntouched(t *testing.T) {
	r := planRowView{Status: "in_progress", OpenDecisions: 1}
	if got := planAttention(r); got != planAttentionDecision {
		t.Errorf("planAttention = %v, want planAttentionDecision — the reconcile must not touch the ranker", got)
	}
}
