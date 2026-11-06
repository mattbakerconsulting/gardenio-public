package main

// plans_terminal_mr_test.go — iter-45 (ka-0000005352): a TERMINAL plan
// (closed/deferred) must NOT render the LOUD "act now / rotting" MR-pill
// wait-age tint. iter-40 folded the MR's since-opened wait-age onto the pill and,
// past planMROpenStaleThreshold (7d), fires the loud amber plan-card-mr-stale
// tint. That tint was NOT reconciled against the plan epic's OWN terminal status:
// a CLOSED plan can still carry a leftover child step with an open, aged MR bead,
// so the pill rendered LOUD amber-stale on a DONE plan — while the plan's OWN
// freshness stamp (iter-38, planFreshnessStale) correctly read CALM history on
// the same card. Same card-vs-truth family as iter-38 (freshness stamp) and
// iter-39 (⛔ escalation badge). Found live on the prod board (2026-08-09 pour):
// closed plans ka-0000005928 + ka-0000005311 rendered 3 open-MR pills ALL
// carrying plan-card-mr-stale while both cards' freshness stamps read calm.
// planMRInfo.reconcileTerminal clears OpenStale on a terminal plan so the pill
// drops only the loud tint while keeping its honest age + click-through.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- unit truth-table: planMRInfo.reconcileTerminal --------------------------

// TestPlanMRInfo_ReconcileTerminal proves the reconcile clears OpenStale ONLY on
// a terminal plan, leaves every other field (URL/IID/age/CI) untouched, and is a
// no-op on a non-terminal plan or an already-calm ref.
func TestPlanMRInfo_ReconcileTerminal(t *testing.T) {
	base := func() planMRInfo {
		return planMRInfo{MRs: []planMRRef{
			{URL: "u1", IID: "1", Title: "t1", OpenAge: "8d", OpenStale: true, PipelineStatus: "success"},
			{URL: "u2", IID: "2", Title: "t2", OpenAge: "40m", OpenStale: false},
		}}
	}
	cases := []struct {
		status        string
		wantStale0    bool
		wantStale1    bool
		wantUnchanged bool // the whole info reads identically to base()
	}{
		// Terminal → the loud OpenStale is cleared on the aged ref; the calm one
		// is already false, so it stays false.
		{"closed", false, false, false},
		{"deferred", false, false, false},
		// Non-terminal → untouched, the aged MR still tints loud.
		{"in_progress", true, false, true},
		{"open", true, false, true},
		{"blocked", true, false, true},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			got := base().reconcileTerminal(c.status)
			if got.MRs[0].OpenStale != c.wantStale0 {
				t.Errorf("MRs[0].OpenStale = %v, want %v", got.MRs[0].OpenStale, c.wantStale0)
			}
			if got.MRs[1].OpenStale != c.wantStale1 {
				t.Errorf("MRs[1].OpenStale = %v, want %v", got.MRs[1].OpenStale, c.wantStale1)
			}
			// Everything OTHER than OpenStale must survive: the pill, its
			// click-through URL, IID, title, age, and CI signal are honest history.
			for i := range got.MRs {
				if got.MRs[i].URL != base().MRs[i].URL ||
					got.MRs[i].IID != base().MRs[i].IID ||
					got.MRs[i].Title != base().MRs[i].Title ||
					got.MRs[i].OpenAge != base().MRs[i].OpenAge ||
					got.MRs[i].PipelineStatus != base().MRs[i].PipelineStatus {
					t.Errorf("MRs[%d] non-stale field mutated: got %+v", i, got.MRs[i])
				}
			}
		})
	}
}

// TestPlanMRInfo_ReconcileTerminal_DoesNotMutateInput proves the reconcile is
// copy-on-write: the caller's backing slice (shared with the enrich map) is never
// mutated, so a re-render off the same map is not corrupted.
func TestPlanMRInfo_ReconcileTerminal_DoesNotMutateInput(t *testing.T) {
	in := planMRInfo{MRs: []planMRRef{{URL: "u1", OpenStale: true}}}
	_ = in.reconcileTerminal("closed")
	if !in.MRs[0].OpenStale {
		t.Errorf("reconcileTerminal mutated its input: MRs[0].OpenStale flipped to false")
	}
}

// TestPlanMRInfo_ReconcileTerminal_EmptyNoOp proves an empty info reconciles to
// itself (no panic on the zero-length slice, no allocation churn).
func TestPlanMRInfo_ReconcileTerminal_EmptyNoOp(t *testing.T) {
	if got := (planMRInfo{}).reconcileTerminal("closed"); got.Any() {
		t.Errorf("empty planMRInfo must reconcile to empty, got %+v", got)
	}
}

// --- render regression: the index card MR pill on a terminal plan ------------

// seedTerminalPlanWithAgedMR returns a fake whose sole plan epic carries the
// given terminal status but still has an OPEN, aged (9d) MR produced by a
// leftover open step — the exact live shape (a closed plan with a dangling
// stale-aged MR). The clock is pinned by the caller via mkServerWithNow.
func seedTerminalPlanWithAgedMR(status string, now time.Time) *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = status
	f.beads["gd-step1"].Status = "open"
	f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 0}
	mr := seedProducedMR(f, "gd-mrAged", "gd-step1", "450", "feat: dangling", "opened", "open",
		now.Add(-time.Hour))
	mr.CreatedAt = now.Add(-9 * 24 * time.Hour) // 9d open → would be OpenStale
	return f
}

var termMRTestNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

// TestHandlePlans_TerminalPlanMRPillNotStale is the fail-without/pass-with render
// guard: a 9d-open MR on a CLOSED plan renders the pill + its honest "· 9d" age +
// its click-through, but NOT the loud plan-card-mr-stale tint. Reverting the
// reconcile call (row.MRs = openMRs[b.ID]) reds this — the pill would carry the
// loud tint on a done plan.
func TestHandlePlans_TerminalPlanMRPillNotStale(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		t.Run(status, func(t *testing.T) {
			f := seedTerminalPlanWithAgedMR(status, termMRTestNow)
			mux := mkServerWithNow(t, f, func() time.Time { return termMRTestNow })

			req := httptest.NewRequest("GET", "/plans", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
			// The pill still renders — a closed plan's dangling open MR is real
			// history the operator may still want to reach.
			if !strings.Contains(card, "!450") {
				t.Fatalf("the open MR pill must still render on a terminal plan; card:\n%s", card)
			}
			// … and keeps its honest age (backward-looking history) …
			if !strings.Contains(card, "· 9d") {
				t.Errorf("the terminal pill must keep its honest '· 9d' age; card:\n%s", card)
			}
			// … but NOT the loud "act now" stale tint.
			if strings.Contains(card, "plan-card-mr-stale") {
				t.Errorf("a terminal plan's aged MR must NOT tint the pill loud (plan-card-mr-stale); card:\n%s", card)
			}
		})
	}
}

// TestHandlePlans_ActivePlanMRPillStaysStale is the over-reach guard (the twin of
// the terminal test): an ACTIVE plan with the SAME 9d-open MR MUST still tint the
// pill loud, so the reconcile quiets ONLY terminal plans and never suppresses a
// real live "act now" signal.
func TestHandlePlans_ActivePlanMRPillStaysStale(t *testing.T) {
	f := seedTerminalPlanWithAgedMR("in_progress", termMRTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return termMRTestNow })

	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "· 9d") {
		t.Errorf("an active plan's aged MR must render its age; card:\n%s", card)
	}
	if !strings.Contains(card, "plan-card-mr-stale") {
		t.Errorf("an active plan's 9d-open MR MUST keep the loud stale tint; card:\n%s", card)
	}
}
