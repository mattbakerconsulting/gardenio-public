package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_terminal_agent_chip_test.go — iter-54 (ka-0000005352): a TERMINAL plan
// (closed/deferred) must NOT render the "not started" plan-agent none-chip. The
// chip is LOOP-primary: its none-tone face renders the literal word "not started"
// (agentChipWord's planAssistantStateNone arm) whenever no gardener loop is bound
// AND no assistant iteration has run. On an ACTIVE gardener-less plan that is
// honest, useful signal (nothing is driving this plan). But on a FINISHED (closed)
// or parked (deferred) plan it is a card-vs-truth contradiction: whether automation
// ever drove the plan is moot history once the plan is over, and the word flatly
// contradicts the closed status chip + the 100% progress bar beside it. Found live
// on the prod board (status=all, 128 cards): 38 CLOSED plans + 1 blocked rendered
// "not started", many at N/N steps · 100% with a full-green bar, 3 also carrying
// iter-41's calm-green "✓ steps done" marker — so a done plan read "closed · 100% ·
// ✓ steps done · not started" at once.
//
// This is the last unreconciled automation signal on a terminal card — the twin of
// the iter-27/28 lifecycle chip, iter-38 freshness stamp, iter-39 ⛔ escalation
// badge, iter-45 MR-pill stale tint, and iter-46 decision chip terminal-reconciles.
// The fix is display-only: planAgentChipView.reconcileTerminal clears ONLY the
// none-tone "not started" Label on a terminal plan; the underlying panel state is
// untouched, so the cockpit header (which renders the SAME chip off the live state)
// is byte-identical.

// --- unit truth-table: planAgentChipView.reconcileTerminal -------------------

// TestPlanAgentChip_ReconcileTerminal is the pure crux: the "not started" none-face
// is cleared iff the chip is none-tone AND the plan is terminal. Every other
// combination is returned byte-identical.
func TestPlanAgentChip_ReconcileTerminal(t *testing.T) {
	cases := []struct {
		name      string
		inLabel   string
		inTone    string
		status    string
		wantLabel string
	}{
		// none-tone "not started" on a terminal plan → cleared (the reconcile).
		{"none closed", "not started", "none", "closed", ""},
		{"none deferred", "not started", "none", "deferred", ""},
		// none-tone on a NON-terminal plan → kept (honest "nothing drives this").
		{"none open", "not started", "none", "open", "not started"},
		{"none in_progress", "not started", "none", "in_progress", "not started"},
		{"none blocked", "not started", "none", "blocked", "not started"},
		// A real loop word on a terminal plan → kept (retired/paused is truthful
		// terminal history, like iter-27's genuine `complete` lifecycle chip — NOT a
		// contradiction). Only the none-tone face is a lie on a done plan.
		{"retired closed", "retired", "retired", "closed", "retired"},
		{"paused deferred", "paused", "paused", "deferred", "paused"},
		{"retired · live closed", "retired · live", "retired", "closed", "retired · live"},
		// A live gardener-less assistant on a terminal plan → kept (its tone is the
		// agent state, not "none", so it is not the "not started" lie).
		{"live closed", "live", "live", "closed", "live"},
		{"parked deferred", "parked", "parked", "deferred", "parked"},
		// Idempotent: an already-empty chip stays empty.
		{"empty closed", "", "none", "closed", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := planAgentChipView{Label: c.inLabel, Tone: c.inTone, Title: "t"}
			got := in.reconcileTerminal(c.status)
			if got.Label != c.wantLabel {
				t.Errorf("reconcileTerminal(%s).Label = %q, want %q (tone=%s)",
					c.status, got.Label, c.wantLabel, c.inTone)
			}
			// Tone/Title/Dot must survive — the reconcile touches only Label.
			if got.Tone != c.inTone {
				t.Errorf("Tone = %q, want %q (reconcile must not change tone)", got.Tone, c.inTone)
			}
			if got.Title != "t" {
				t.Errorf("Title = %q, want unchanged", got.Title)
			}
		})
	}
}

// --- HTTP render: fail-without / pass-with -----------------------------------

// TestPlansIndex_TerminalPlanSuppressesNotStartedChip is the render regression: a
// CLOSED (and a DEFERRED) gardener-less plan renders NO "not started" chip.
// Reverting the enrich seam (dropping .reconcileTerminal) or the template gate reds
// this. seedPlansFake builds gd-plan1 gardener-less with no assistant, so its chip
// is the "not started" none-face; flipping the plan status to a terminal one is the
// whole setup, exactly like the iter-39/46 terminal twins.
func TestPlansIndex_TerminalPlanSuppressesNotStartedChip(t *testing.T) {
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
			if strings.Contains(body, ">not started<") {
				t.Errorf("%s plan must render NO 'not started' plan-agent chip; the card had one", status)
			}
			// Belt-and-suspenders: a bare empty none-pill must not render either — the
			// template gate ({{if .Label}}) drops the whole pill, so the none-tone
			// class must be absent from the card markup (it also appears in <style>,
			// so assert on the OPENING chip tag exactly as the decision twin does).
			if strings.Contains(body, `<span class="chip plan-agent-chip plan-agent-chip-none"`) {
				t.Errorf("%s plan must render no empty none-tone pill (the {{if .Label}} gate failed)", status)
			}
		})
	}
}

// TestPlansIndex_ActivePlanStillShowsNotStartedChip is the anti-over-reach guard:
// an ACTIVE (in_progress) gardener-less plan with no assistant MUST still render the
// "not started" chip — there it is honest signal (nothing is driving the plan; an
// operator may want to bind a gardener). The reconcile must quiet it ONLY on a
// terminal plan. seedPlansFake's default status is in_progress, so this is the
// un-mutated shape.
func TestPlansIndex_ActivePlanStillShowsNotStartedChip(t *testing.T) {
	f := seedPlansFake() // default status is in_progress, gardener-less, no assistant
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, ">not started<") {
		t.Errorf("an active in_progress gardener-less plan MUST still render the 'not started' chip")
	}
}

// --- non-regression: the raw signal (lane) is untouched ----------------------

// TestTerminalPlanAgentChip_BoardLaneUnchanged proves the reconcile is
// display-only: a CLOSED plan still lanes to Closed and a LIVE plan lanes by its
// own work-state regardless of the chip suppression. The chip clear must never move
// a card between lanes.
func TestTerminalPlanAgentChip_BoardLaneUnchanged(t *testing.T) {
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
			r := planRowView{Status: c.status}
			if got := planBoardLaneKey(r); got != c.wantLane {
				t.Errorf("planBoardLaneKey(status=%s) = %q, want %q — the reconcile must not change lanes",
					c.status, got, c.wantLane)
			}
		})
	}
}
