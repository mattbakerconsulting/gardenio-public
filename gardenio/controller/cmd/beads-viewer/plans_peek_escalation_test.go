package main

import (
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// plans_peek_escalation_test.go — iter-74 (ka-0000005352): the /plans peek drawer
// HEADER now carries the ⛔ "blocked by escalation" badge.
//
// THE GAP (measure-first): the peek header folded nearly every index-card FACE
// triage signal — status · lifecycle · priority (iter-72) · gardener · crew+stuck
// (iter-65/71) · project · open-decision chip · freshness (iter-73) — but dropped
// the ⛔ blocked-by-escalation badge, the #2 attention tier (planAttentionEscalation):
// a HARDER operator stop than a ⚠ stall or an open decision, "a privileged operator
// must act before the blocked work can move". The index card FACE (plan_card.html)
// and the cockpit banner both carry it; the peek — opened precisely to TRIAGE —
// showed the open-decision chip yet nothing about a work-blocking escalation.
//
// The tests below guard: (1) a plan with an open blocking escalation shows the
// badge IN THE HEADER with the oldest op-label + wait-age; (2) an aged (>7d)
// escalation loud-tints the badge (plan-esc-badge-stale); (3) a plan with NO
// escalation renders NO badge (no ink); (4) a TERMINAL plan whose leftover open
// step still carries an escalation suppresses the badge FACE (the iter-39
// terminal reconcile), so the peek adds no false "act now"; (5) the header count
// agrees with the count the same seed renders on the cockpit banner
// (two-derivations-must-agree, advice ka-0000005924).
//
// peekHeader (plans_peek_crew_chip_test.go) returns just the
// <p class="plan-peek-meta">…</p> block, so a header assertion cannot be
// satisfied by any body content below it (e.g. the per-step escalation chip).

// seedPeekEscalationAt wires an open escalation that `blocks` the plan's gd-step1
// with a caller-chosen CreatedAt, so the age vocabulary is under test control
// (seedPlanEscalation's own stamp is 2026-07-08, AFTER crewFixtureNow).
func seedPeekEscalationAt(f *fakeDaemon, escID, title, opType string, createdAt time.Time) {
	esc := seedPlanEscalation(f, escID, "gd-step1", title, opType, "internal")
	esc.CreatedAt = createdAt
}

// TestPlanPeekHeader_EscalationBadge: a plan with an open blocking escalation
// shows the ⛔ badge in the header, with the oldest escalation's op-label
// ("AWS IAM" for aws_iam) and its wait-age. Seed CreatedAt is 3 days before
// crewFixtureNow (2026-07-05 12:00) → "3d". FAILS pre-fix (the peek header
// carried no escalation badge at all).
func TestPlanPeekHeader_EscalationBadge(t *testing.T) {
	f := seedPlansFake()
	seedPeekEscalationAt(f, "gd-esc1", "Grant IAM for deploy", "aws_iam",
		crewFixtureNow.Add(-3*24*time.Hour))
	header := peekHeader(t, peekBody(t, f))

	if !strings.Contains(header, "plan-esc-badge") {
		t.Fatalf("peek header should render the ⛔ escalation badge; header=%s", header)
	}
	if !strings.Contains(header, "blocked by escalation") {
		t.Errorf("badge face should read the ⛔ headline; header=%s", header)
	}
	if !strings.Contains(header, "AWS IAM") {
		t.Errorf("badge should carry the oldest escalation's op-label %q; header=%s", "AWS IAM", header)
	}
	if !strings.Contains(header, "plan-esc-badge-age") || !strings.Contains(header, "3d") {
		t.Errorf("badge should carry the oldest escalation's wait-age %q; header=%s", "3d", header)
	}
	// Fresh (<7d) → not loud-tinted.
	if strings.Contains(header, "plan-esc-badge-stale") {
		t.Errorf("a fresh (<7d) escalation must not loud-tint the badge; header=%s", header)
	}
	// Links to the full cockpit's escalation section (peek is read-only).
	if !strings.Contains(header, "/plans/gd-plan1#plan-escalations") {
		t.Errorf("badge should link to the cockpit #plan-escalations section; header=%s", header)
	}
}

// TestPlanPeekHeader_EscalationBadgeStale: an escalation aged past the 7d bar
// loud-tints the badge (plan-esc-badge-stale) with the stale reason in the
// tooltip — the same signal the index badge carries (OldestStale).
func TestPlanPeekHeader_EscalationBadgeStale(t *testing.T) {
	f := seedPlansFake()
	seedPeekEscalationAt(f, "gd-esc1", "Rotate the prod secret", "credential_rotation",
		crewFixtureNow.Add(-10*24*time.Hour))
	header := peekHeader(t, peekBody(t, f))

	if !strings.Contains(header, "plan-esc-badge-stale") {
		t.Fatalf("an escalation aged >7d should loud-tint the badge; header=%s", header)
	}
	if !strings.Contains(header, "(stale)") {
		t.Errorf("stale badge tooltip should name the stale reason; header=%s", header)
	}
	if !strings.Contains(header, "plan-esc-badge-age") || !strings.Contains(header, "10d") {
		t.Errorf("stale badge should still carry the wait-age %q; header=%s", "10d", header)
	}
}

// TestPlanPeekHeader_NoEscalationNoBadge: a plan with no blocking escalation
// (the default seed) renders NO badge — the density floor, no ink on a healthy
// plan.
func TestPlanPeekHeader_NoEscalationNoBadge(t *testing.T) {
	header := peekHeader(t, peekBody(t, seedPlansFake()))
	if strings.Contains(header, "plan-esc-badge") {
		t.Errorf("a plan with no blocking escalation must render no ⛔ badge; header=%s", header)
	}
	if strings.Contains(header, "blocked by escalation") {
		t.Errorf("no escalation headline should appear on a healthy plan; header=%s", header)
	}
}

// TestPlanPeekHeader_TerminalPlanSuppressesBadge: a TERMINAL (closed) plan whose
// leftover open step still carries an open escalation suppresses the badge FACE
// — the iter-39 terminal reconcile (ShowEscalationBadge = Count>0 AND not
// terminal): approving the escalation advances nothing on a finished plan, so the
// peek adds no false "act now". Matches the index card + the cockpit reconcile.
func TestPlanPeekHeader_TerminalPlanSuppressesBadge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed"
	seedPeekEscalationAt(f, "gd-esc1", "Grant IAM for deploy", "aws_iam",
		crewFixtureNow.Add(-3*24*time.Hour))
	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-esc-badge") {
		t.Errorf("a terminal plan must suppress the ⛔ badge FACE (iter-39 reconcile); header=%s", header)
	}
}

// TestPlanPeekHeader_MultiEscalationCount: two open escalations blocking two
// distinct steps render the "×N" count on the badge — proving the header count
// is the SAME planEscalationInfo.Count the cockpit banner derives from the same
// seed (two-derivations-must-agree, advice ka-0000005924). gd-step2 is added as a
// second plan child so both escalations are reachable off the peek's own walk.
func TestPlanPeekHeader_MultiEscalationCount(t *testing.T) {
	f := seedPlansFake()
	// A second open step child so the second escalation is walk-reachable.
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Second step", Type: "task", Status: "open",
		Labels: []string{"project:gardenio"},
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 0}
	// Two open escalations, one blocking each step (seedPlanEscalation writes the
	// forward blocks-edge). Both fresh (<7d) so the badge stays calm; the count is
	// what is under test.
	seedPeekEscalationAt(f, "gd-esc1", "Grant IAM", "aws_iam", crewFixtureNow.Add(-3*24*time.Hour))
	esc2 := seedPlanEscalation(f, "gd-esc2", "gd-step2", "Merge the MR", "gitlab_mr_merge", "internal")
	esc2.CreatedAt = crewFixtureNow.Add(-2 * 24 * time.Hour)

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, "plan-esc-badge") {
		t.Fatalf("two blocking escalations should still render the badge; header=%s", header)
	}
	if !strings.Contains(header, "&times;2") && !strings.Contains(header, "×2") {
		t.Errorf("badge should carry the ×2 count for two escalations; header=%s", header)
	}
}
