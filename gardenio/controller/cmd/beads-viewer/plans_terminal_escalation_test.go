package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_terminal_escalation_test.go — iter-39 (ka-0000005352): a TERMINAL plan
// (closed/deferred) must NOT render the loud ⛔ "blocked by escalation" badge.
// The badge is the #2 attention tier — a HARDER stop than a ⚠ stall, "a
// privileged operator must act before the blocked work can move" — so it reads
// as a board-topping "go act NOW" call. On a FINISHED (closed) or parked
// (deferred) plan that is a card-vs-truth contradiction: a leftover un-closed
// child step can still carry an open blocks-edged escalation, but the plan those
// steps belong to is over, so approving the escalation advances nothing. It was
// the LAST unreconciled forward/act-now signal on a terminal card — its siblings
// the now/next chip + Dispatch button (iter-25), the crew live-pulse (iter-30),
// and the lifecycle chip (iter-27/28) were all already reconciled. Found live on
// the prod board: ka-0000000539 (closed 2026-08-05) rendered "⛔ blocked by
// escalation ×3 · merge MR · 3d". The fix is display-only — planRowView.
// ShowEscalationBadge gates only the FACE render; the raw Escalations.Count is
// untouched so the attention ranker + board lane are byte-identical.

// seedTerminalPlanWithEscalation returns a fake whose sole plan epic carries the
// given status but still has a leftover OPEN child step blocked by an open
// escalation — the exact live shape (a terminal plan whose leftover step still
// carries a blocks-edged escalation). status lets a caller flip closed/deferred/
// a live status to reuse the shape as the anti-over-reach control.
func seedTerminalPlanWithEscalation(status, opType string) *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = status
	f.beads["gd-step1"].Status = "open"
	f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 0}
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Approve AND merge the fix", opType, "internal")
	return f
}

// --- unit truth-table: planRowView.ShowEscalationBadge -----------------------

// TestPlanRowView_ShowEscalationBadge is the pure crux: the badge FACE renders
// iff the plan has an escalation AND is non-terminal. A closed/deferred plan
// suppresses it; a live plan with the same escalation still shows it; a plan
// with no escalation never shows it (byte-identical to today, whatever the
// status).
func TestPlanRowView_ShowEscalationBadge(t *testing.T) {
	withEsc := planEscalationInfo{Count: 2, Escalations: []planEscalationRef{{ID: "gd-e1"}, {ID: "gd-e2"}}}
	cases := []struct {
		name   string
		status string
		esc    planEscalationInfo
		want   bool
	}{
		// Non-terminal + escalation → the loud badge renders (protect iter-8/37).
		{"in_progress with esc", "in_progress", withEsc, true},
		{"open with esc", "open", withEsc, true},
		{"blocked with esc", "blocked", withEsc, true},
		// Terminal + escalation → suppressed (the reconcile).
		{"closed with esc", "closed", withEsc, false},
		{"deferred with esc", "deferred", withEsc, false},
		// No escalation → never renders, whatever the status.
		{"in_progress no esc", "in_progress", planEscalationInfo{}, false},
		{"closed no esc", "closed", planEscalationInfo{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := planRowView{Status: c.status, Escalations: c.esc}
			if got := r.ShowEscalationBadge(); got != c.want {
				t.Errorf("ShowEscalationBadge() = %v, want %v (status=%s, count=%d)",
					got, c.want, c.status, c.esc.Count)
			}
		})
	}
}

// --- HTTP render: fail-without / pass-with -----------------------------------

// TestPlansIndex_TerminalPlanSuppressesEscalationBadge is the render regression:
// a CLOSED plan with an open blocking escalation renders NO ⛔ badge. Reverting
// the template seam (gating on .Escalations.Count instead of .ShowEscalationBadge)
// reds this — the closed plan would render the loud badge again.
func TestPlansIndex_TerminalPlanSuppressesEscalationBadge(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		t.Run(status, func(t *testing.T) {
			f := seedTerminalPlanWithEscalation(status, "gitlab_mr_merge")
			mux := mkServer(t, f)
			w := httptest.NewRecorder()
			// status=all so the terminal plan is not filtered out of the query.
			mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			// Assert on the rendered badge ANCHOR, not the bare class — the class
			// name also appears in the page's <style> block (the .plan-esc-badge
			// rules), so a bare-substring check would false-match the CSS.
			if strings.Contains(body, `<a class="plan-esc-badge`) {
				t.Errorf("%s plan must render NO ⛔ escalation badge; the card had one", status)
			}
			// Belt-and-suspenders: the badge's headline text must be gone too.
			if strings.Contains(body, "⛔ blocked by escalation") {
				t.Errorf("%s plan must not render the '⛔ blocked by escalation' face", status)
			}
		})
	}
}

// TestPlansIndex_ActivePlanStillShowsEscalationBadge is the anti-over-reach
// guard: an in_progress plan with the SAME escalation shape MUST still render the
// loud ⛔ badge — the reconcile must quiet it ONLY on a terminal plan, never on a
// live one (protecting the iter-8 wait-age + iter-37 op-face signal).
func TestPlansIndex_ActivePlanStillShowsEscalationBadge(t *testing.T) {
	f := seedTerminalPlanWithEscalation("in_progress", "gitlab_mr_merge") // reuse the shape, but live
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<a class="plan-esc-badge`) {
		t.Errorf("a live in_progress plan MUST still render the ⛔ escalation badge")
	}
	if !strings.Contains(body, "⛔ blocked by escalation") {
		t.Errorf("a live in_progress plan MUST still render the '⛔ blocked by escalation' face")
	}
}

// --- non-regression: the raw signal (lane + ranker) is untouched -------------

// TestTerminalPlanEscalation_BoardLaneUnchanged proves the reconcile is
// display-only: a CLOSED plan with an escalation still lanes to Closed (its raw
// status wins in planBoardLaneKey regardless), and a LIVE escalation-blocked plan
// still lanes to Stalled/Blocked (Escalations.Count > 0 is untouched). The badge
// suppression must never move a card between lanes.
func TestTerminalPlanEscalation_BoardLaneUnchanged(t *testing.T) {
	cases := []struct {
		status   string
		wantLane string
	}{
		{"closed", "closed"},
		{"deferred", "deferred"},
		{"in_progress", planBoardLaneStalled}, // escalation forces the Stalled/Blocked lane
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			r := planRowView{
				Status:      c.status,
				Escalations: planEscalationInfo{Count: 1, Escalations: []planEscalationRef{{ID: "gd-e1"}}},
			}
			if got := planBoardLaneKey(r); got != c.wantLane {
				t.Errorf("planBoardLaneKey(status=%s) = %q, want %q — the reconcile must not change lanes",
					c.status, got, c.wantLane)
			}
			// And the raw count the ranker reads is preserved on every status.
			if r.Escalations.Count != 1 {
				t.Errorf("status=%s: Escalations.Count = %d, want 1 (raw signal must survive)", c.status, r.Escalations.Count)
			}
		})
	}
}

// TestTerminalPlanEscalation_AttentionTierUntouched proves the attention ranker
// still sees the true escalation state: planAttention keys off Escalations.Count,
// which the display-only reconcile never mutates, so a plan's tier is derived
// from real state (a live escalation-blocked plan is still planAttentionEscalation).
func TestTerminalPlanEscalation_AttentionTierUntouched(t *testing.T) {
	r := planRowView{
		Status:      "in_progress",
		Escalations: planEscalationInfo{Count: 1, Escalations: []planEscalationRef{{ID: "gd-e1"}}},
	}
	if got := planAttention(r); got != planAttentionEscalation {
		t.Errorf("planAttention = %v, want planAttentionEscalation — the reconcile must not touch the ranker", got)
	}
}
