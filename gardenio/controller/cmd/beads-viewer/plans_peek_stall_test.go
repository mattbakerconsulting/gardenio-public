package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_peek_stall_test.go — iter-75 (ka-0000005352): the /plans peek drawer
// HEADER now carries the ⚠ "stalled" badge.
//
// THE GAP (measure-first): the peek header folded the #1 open-decision chip AND
// the #2 ⛔ blocked-by-escalation badge (iter-74) but STILL dropped the #3 ⚠
// stall badge (planAttentionStalled) — the LAST attention-tier signal both the
// index card FACE (plan_card.html) and the cockpit header (plan_detail.html)
// carry that the peek still missed. An operator opens a peek precisely to TRIAGE
// and saw the decision + escalation signals but nothing about a stalled plan (a
// dead-agent orphan, a paused gardener, or no gardener bound to watch open work).
//
// The tests below guard: (1) the default seed — open work, no gardener, no live
// agent — shows the ⚠ badge IN THE HEADER with the compact "no gardener" reason;
// (2) a running gardener suppresses it (no ink); (3) a live working crew clears
// the false gardener-state stall (reconcileStallWithCrew, the mission-named
// false-stall); (4) a TERMINAL (closed) plan never fires (deriveStall's terminal
// guard); (5) the peek badge agrees with the index card's own badge on the same
// seed (two-derivations-must-agree, advice ka-0000005924).
//
// peekHeader (plans_peek_crew_chip_test.go) returns just the
// <p class="plan-peek-meta">…</p> block, so a header assertion cannot be
// satisfied by any body content below it.

// TestPlanPeekHeader_StallBadgeNoGardener: the default seed plan has open work
// (gd-step1 is open), no gardener bound, and no live assistant — so it stalls with
// the "no gardener is bound" condition. The peek header renders the ⚠ badge with
// the compact "no gardener" face reason. FAILS pre-fix (the peek header carried no
// stall badge at all).
func TestPlanPeekHeader_StallBadgeNoGardener(t *testing.T) {
	header := peekHeader(t, peekBody(t, seedPlansFake()))

	if !strings.Contains(header, "plan-stall-badge") {
		t.Fatalf("peek header should render the ⚠ stall badge on an unwatched plan; header=%s", header)
	}
	if !strings.Contains(header, "⚠ stalled") {
		t.Errorf("badge face should read the ⚠ stalled headline; header=%s", header)
	}
	if !strings.Contains(header, "plan-stall-badge-reason") || !strings.Contains(header, "no gardener") {
		t.Errorf("badge face should carry the compact 'no gardener' reason; header=%s", header)
	}
	if !strings.Contains(header, "no gardener is bound") {
		t.Errorf("badge tooltip should name the no-gardener condition; header=%s", header)
	}
	// Fresh (no dead-agent orphan) → not loud-tinted.
	if strings.Contains(header, "plan-stall-badge-stale") {
		t.Errorf("a gardener-state stall with no dead orphan must not loud-tint; header=%s", header)
	}
}

// TestPlanPeekHeader_NoStallWhenGardenerRunning: a plan watched by a running
// gardener must not flag stalled — the density floor, no ink on a healthy plan.
func TestPlanPeekHeader_NoStallWhenGardenerRunning(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-stall-badge") {
		t.Errorf("a plan watched by a running gardener must render no ⚠ badge; header=%s", header)
	}
}

// TestPlanPeekHeader_NoStallWhenCrewWorking: a live working crew clears the false
// gardener-state stall (reconcileStallWithCrew, iter-19) — the mission-named
// false-stall ("plans with active crew but a paused/absent gardener are flagged
// stalled"). The step stays OPEN (no in_progress dead-agent orphan), so the only
// stall condition is the "no gardener" gardener-state alarm — which the live crew
// (assigned via edge) clears outright. The peek header must agree with its own
// crew chip: "● N crew · M working" and NO "⚠ stalled" beside it.
func TestPlanPeekHeader_NoStallWhenCrewWorking(t *testing.T) {
	f := seedPlansFake()
	// gd-step1 stays open (default) — a working crew assigned to it makes the crew
	// rollup live without creating an in_progress dead-agent orphan.
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-stall-badge") {
		t.Errorf("a plan with a live working crew must not flag stalled (false-stall); header=%s", header)
	}
	// The crew chip proves work is live — the two must not contradict.
	if !strings.Contains(header, "plan-peek-crew-live") {
		t.Errorf("the live-crew chip should render beside the (absent) stall badge; header=%s", header)
	}
}

// TestPlanPeekHeader_TerminalPlanNoStall: a TERMINAL (closed) plan never fires the
// stall badge — deriveStall's planTerminal guard zeroes it — so a finished plan's
// leftover open step does not manufacture a false "act now". Matches the index
// card + cockpit terminal reconcile.
func TestPlanPeekHeader_TerminalPlanNoStall(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed"
	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-stall-badge") {
		t.Errorf("a terminal plan must render no ⚠ stall badge (deriveStall terminal guard); header=%s", header)
	}
}

// TestPlanPeekHeader_StallAgreesWithIndex: the peek header stall badge and the
// index card's own stall badge are derived from the SAME seed — both must render
// the same "no gardener" verdict (two-derivations-must-agree, advice
// ka-0000005924). This pins that the peek reuses the cockpit/index derivation
// chain rather than a forked predicate that could drift.
func TestPlanPeekHeader_StallAgreesWithIndex(t *testing.T) {
	// Peek surface.
	peek := peekHeader(t, peekBody(t, seedPlansFake()))
	peekStalled := strings.Contains(peek, "plan-stall-badge") && strings.Contains(peek, "no gardener")

	// Index surface (same seed, same clock).
	mux := mkServerWithNow(t, seedPlansFake(), crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("GET /plans: expected 200, got %d", w.Code)
	}
	indexStalled := strings.Contains(w.Body.String(), "⚠ stalled") && strings.Contains(w.Body.String(), "no gardener")

	if peekStalled != indexStalled {
		t.Errorf("peek and index must agree on the stall verdict: peek=%v index=%v", peekStalled, indexStalled)
	}
	if !peekStalled {
		t.Errorf("both surfaces should flag the unwatched seed plan as stalled; peek=%v index=%v", peekStalled, indexStalled)
	}
}
