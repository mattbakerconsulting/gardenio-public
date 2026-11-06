package main

import (
	"strings"
	"testing"
	"time"
)

// plans_peek_freshness_test.go — iter-73 (ka-0000005352): the /plans peek drawer
// HEADER now carries the "last change" freshness stamp.
//
// THE GAP (measure-first): the peek header folded nearly every index-card FACE
// triage signal — status · lifecycle · gardener-live · crew+stuck (iter-65/71) ·
// project · decisions · priority (iter-72) — but dropped the ONE temporal signal
// both the index card head (.plan-card-updated, plan_card.html) AND the cockpit
// header (.plan-header-updated, plan_detail.html) carry: the "last change" stamp.
// An operator opening a peek to TRIAGE could not tell whether the plan moved 5m
// ago or has been dead 3 weeks, nor whether it is stale (>7d no activity).
//
// The tests below guard four things: (1) an active plan shows the stamp with the
// compact age off its own last change; (2) an ACTIVE plan gone stale (>7d) tints
// the stamp loud; (3) a live crew's heartbeat FRESHENS the stamp (the
// false-freshness fold, ka-0000004310) — proving it reads the SAME crew already
// in hand, not a stale epic updated_at; (4) a TERMINAL plan's honest old stamp
// stays calm (no loud stale tint), matching the index card's own gate.
//
// peekHeader (plans_peek_crew_chip_test.go) returns just the <p
// class="plan-peek-meta">…</p> block, so an assertion on the header stamp cannot
// be satisfied by any body content below it.

// TestPlanPeekHeader_FreshnessStamp: an active plan shows the "last change" stamp
// with the compact age (fmtTimeAgo) of its own last change — the seed plan's
// UpdatedAt is 2026-07-01 10:00, crewFixtureNow is 2026-07-05 12:00 → "4d" — and,
// being fresh (<7d), NOT loud-tinted.
func TestPlanPeekHeader_FreshnessStamp(t *testing.T) {
	header := peekHeader(t, peekBody(t, seedPlansFake()))
	if !strings.Contains(header, "plan-peek-updated") {
		t.Fatalf("active-plan peek header should render the freshness stamp; header=%s", header)
	}
	if !strings.Contains(header, ">4d<") {
		t.Errorf("freshness stamp should read the plan's own last-change age %q; header=%s", "4d", header)
	}
	if strings.Contains(header, "plan-peek-updated-stale") {
		t.Errorf("a fresh (<7d) plan must not loud-tint the freshness stamp; header=%s", header)
	}
	if !strings.Contains(header, `title="Last change 2026-07-01T10:00:00Z"`) {
		t.Errorf("freshness stamp should carry the precise Last change RFC tooltip; header=%s", header)
	}
}

// TestPlanPeekHeader_FreshnessStale: an ACTIVE plan whose newest activity is older
// than the 7d freshness bar tints the stamp loud (plan-peek-updated-stale) with the
// stale reason in the tooltip — the same signal the index card's freshness stamp
// carries (planFreshnessStale). FAILS pre-fix (the header carried no stamp at all).
func TestPlanPeekHeader_FreshnessStale(t *testing.T) {
	f := seedPlansFake()
	// 10 days before crewFixtureNow — past planFreshnessStaleThreshold (7d) — with
	// the plan still active (in_progress), no fresher step/crew activity.
	f.beads["gd-plan1"].UpdatedAt = crewFixtureNow.Add(-10 * 24 * time.Hour)

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, "plan-peek-updated-stale") {
		t.Errorf("an active plan stale past 7d must loud-tint the freshness stamp; header=%s", header)
	}
	if !strings.Contains(header, "has not moved in over a week (stale)") {
		t.Errorf("the stale stamp tooltip should explain the staleness; header=%s", header)
	}
	if !strings.Contains(header, ">10d<") {
		t.Errorf("the stale stamp should read the true age %q; header=%s", "10d", header)
	}
}

// TestPlanPeekHeader_CrewHeartbeatFreshensStamp: a step claimed long ago whose
// crew is heartbeating NOW must read fresh — the false-freshness fold
// (ka-0000004310) that the index card carries. The seed plan's epic updated_at is
// "4d" stale, but a live crew stamped at crewFixtureNow−5m must pull the stamp to
// "5m" — proving the peek stamp reads the SAME crew roster already in hand, not the
// rarely-moving epic updated_at. Guards against a "4d ago" stamp beside a pulsing
// "1 working" crew chip — the self-contradiction the fold exists to kill.
func TestPlanPeekHeader_CrewHeartbeatFreshensStamp(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewLive", "gardenio-wire-route-live", "gd-step1", "working", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, ">5m<") {
		t.Errorf("a live crew heartbeat (now-5m) must freshen the stamp to %q, not the epic's 4d; header=%s", "5m", header)
	}
	if strings.Contains(header, ">4d<") {
		t.Errorf("the stamp must not read the stale epic updated_at when a crew is heartbeating; header=%s", header)
	}
	if strings.Contains(header, "plan-peek-updated-stale") {
		t.Errorf("a plan with a live crew is not stale; header=%s", header)
	}
}

// TestPlanPeekHeader_TerminalPlanNoStaleTint: a TERMINAL (closed) plan renders its
// honest old "last change" stamp but NOT loud-tinted — planFreshnessStale gates on
// the plan's own status, so a finished plan's weeks-old history stays calm, exactly
// as the index card's freshness stamp does (iter-38). The stamp still renders (the
// operator wants the honest age); only the alarm tint is suppressed.
func TestPlanPeekHeader_TerminalPlanNoStaleTint(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed"
	f.beads["gd-plan1"].UpdatedAt = crewFixtureNow.Add(-10 * 24 * time.Hour)

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, "plan-peek-updated") {
		t.Fatalf("a terminal plan should still render its honest last-change stamp; header=%s", header)
	}
	if !strings.Contains(header, ">10d<") {
		t.Errorf("the terminal stamp should read the true age %q; header=%s", "10d", header)
	}
	if strings.Contains(header, "plan-peek-updated-stale") {
		t.Errorf("a TERMINAL plan's old stamp must stay calm (no loud stale tint); header=%s", header)
	}
}
