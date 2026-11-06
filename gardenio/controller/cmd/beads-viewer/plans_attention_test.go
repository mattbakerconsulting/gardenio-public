package main

// Tests for the attention-first plan ranking (Step 2, ka-0000004961;
// plans_attention.go). planAttention maps an enriched row to its tier;
// sortPlanRows applies the tier + within-tier recency order. Both are pure over
// already-computed planRowView fields, so these are table-driven with no daemon.

import (
	"testing"
	"time"
)

// attnRow builds a minimal enriched row carrying just the fields the ranker
// reads, so a test case declares only the signal under test.
func attnRow(id string) planRowView { return planRowView{ID: id} }

func withActivity(r planRowView, t time.Time) planRowView { r.lastActivityAt = t; return r }

// TestPlanAttention_Tiers pins the tier each attention signal maps to, and the
// first-match-wins priority when a row carries several at once.
func TestPlanAttention_Tiers(t *testing.T) {
	cases := []struct {
		name string
		row  planRowView
		want planAttentionTier
	}{
		{"open decision", planRowView{OpenDecisions: 2}, planAttentionDecision},
		{"blocking escalation", planRowView{Escalations: planEscalationInfo{Count: 1}}, planAttentionEscalation},
		{"ready awaiting approve", planRowView{PlanState: string(planStateReady)}, planAttentionReady},
		{"stalled", planRowView{Stall: planStallInfo{Stalled: true}}, planAttentionStalled},
		{"healthy residual", planRowView{PlanState: string(planStateBuilding)}, planAttentionNone},
		{"empty row", planRowView{}, planAttentionNone},
		// A draft/building/unset plan is NOT a ready plan — only `ready` earns the
		// approve tier, so a building plan with steps stays in the residual feed.
		{"draft is not ready", planRowView{PlanState: string(planStateDraft)}, planAttentionNone},
		// Priority: a decision outranks every other signal on the SAME row.
		{"decision beats escalation+ready+stall", planRowView{
			OpenDecisions: 1,
			Escalations:   planEscalationInfo{Count: 3},
			PlanState:     string(planStateReady),
			Stall:         planStallInfo{Stalled: true},
		}, planAttentionDecision},
		// Priority: escalation outranks ready and stall.
		{"escalation beats ready+stall", planRowView{
			Escalations: planEscalationInfo{Count: 1},
			PlanState:   string(planStateReady),
			Stall:       planStallInfo{Stalled: true},
		}, planAttentionEscalation},
		// Priority: ready outranks a stall — one tap unblocks the plan.
		{"ready beats stall", planRowView{
			PlanState: string(planStateReady),
			Stall:     planStallInfo{Stalled: true},
		}, planAttentionReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planAttention(tc.row); got != tc.want {
				t.Errorf("planAttention() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSortPlanRows_TierOrder proves the primary key: one row per tier, fed in
// reverse-priority order, comes back decision → escalation → ready → stalled →
// none regardless of input order.
func TestSortPlanRows_TierOrder(t *testing.T) {
	none := attnRow("gd-none")
	stalled := attnRow("gd-stall")
	stalled.Stall = planStallInfo{Stalled: true}
	ready := attnRow("gd-ready")
	ready.PlanState = string(planStateReady)
	esc := attnRow("gd-esc")
	esc.Escalations = planEscalationInfo{Count: 1}
	dec := attnRow("gd-dec")
	dec.OpenDecisions = 1

	rows := []planRowView{none, stalled, ready, esc, dec}
	sortPlanRows(rows)

	want := []string{"gd-dec", "gd-esc", "gd-ready", "gd-stall", "gd-none"}
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("tier order = %s, want %s at position %d (full: %s)", rows[i].ID, id, i, rids(rows))
		}
	}
}

// TestSortPlanRows_AttentionTierOldestFirst proves the within-tier key for an
// ATTENTION tier drains oldest-waiting-first — the key that surfaces the >30d
// backlog tail (decision ka-0000000932) instead of burying it under fresh rows.
func TestSortPlanRows_AttentionTierOldestFirst(t *testing.T) {
	old := withActivity(attnRow("gd-old"), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	old.OpenDecisions = 1
	mid := withActivity(attnRow("gd-mid"), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	mid.OpenDecisions = 1
	recent := withActivity(attnRow("gd-recent"), time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC))
	recent.OpenDecisions = 1

	rows := []planRowView{recent, old, mid}
	sortPlanRows(rows)

	want := []string{"gd-old", "gd-mid", "gd-recent"}
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("attention within-tier order = %s, want %s at position %d (full: %s)", rows[i].ID, id, i, rids(rows))
		}
	}
}

// TestSortPlanRows_ResidualTierFreshestFirst proves the residual (none) tier
// reads like a normal recent-activity feed: freshest first.
func TestSortPlanRows_ResidualTierFreshestFirst(t *testing.T) {
	old := withActivity(attnRow("gd-old"), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	recent := withActivity(attnRow("gd-recent"), time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC))
	mid := withActivity(attnRow("gd-mid"), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	rows := []planRowView{old, recent, mid}
	sortPlanRows(rows)

	want := []string{"gd-recent", "gd-mid", "gd-old"}
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("residual order = %s, want %s at position %d (full: %s)", rows[i].ID, id, i, rids(rows))
		}
	}
}

// TestSortPlanRows_ZeroActivitySinks proves an unstamped row sinks to the bottom
// of its tier in BOTH directions — it is neither the freshest nor the
// longest-waiting.
func TestSortPlanRows_ZeroActivitySinks(t *testing.T) {
	// Attention tier (oldest-first): the stamped row still leads the zero one.
	stamped := withActivity(attnRow("gd-stamped"), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	stamped.OpenDecisions = 1
	zero := attnRow("gd-zero") // no lastActivityAt
	zero.OpenDecisions = 1
	rows := []planRowView{zero, stamped}
	sortPlanRows(rows)
	if rows[0].ID != "gd-stamped" || rows[1].ID != "gd-zero" {
		t.Fatalf("attention tier: zero-activity row should sink, got %s", rids(rows))
	}

	// Residual tier (freshest-first): the stamped row still leads the zero one.
	sr := withActivity(attnRow("gd-stamped"), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	zr := attnRow("gd-zero")
	rows = []planRowView{zr, sr}
	sortPlanRows(rows)
	if rows[0].ID != "gd-stamped" || rows[1].ID != "gd-zero" {
		t.Fatalf("residual tier: zero-activity row should sink, got %s", rids(rows))
	}
}

// TestSortPlanRows_StableIDTieBreak proves rows indistinguishable on tier AND
// activity fall back to a deterministic id order, so the render is stable across
// requests regardless of the daemon's input order.
func TestSortPlanRows_StableIDTieBreak(t *testing.T) {
	ts := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	b := withActivity(attnRow("gd-b"), ts)
	a := withActivity(attnRow("gd-a"), ts)
	c := withActivity(attnRow("gd-c"), ts)
	rows := []planRowView{b, c, a}
	sortPlanRows(rows)
	want := []string{"gd-a", "gd-b", "gd-c"}
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("id tie-break = %s, want %s at position %d (full: %s)", rows[i].ID, id, i, rids(rows))
		}
	}
	// Both-zero-activity ties also fall through to the id tie-break.
	rows = []planRowView{attnRow("gd-b"), attnRow("gd-c"), attnRow("gd-a")}
	sortPlanRows(rows)
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("both-zero id tie-break = %s, want %s at position %d (full: %s)", rows[i].ID, id, i, rids(rows))
		}
	}
}

// TestSortPlanRows_FullRankingScenario is the end-to-end order over a mixed deck
// that exercises tiers AND both within-tier directions at once.
func TestSortPlanRows_FullRankingScenario(t *testing.T) {
	mk := func(id string, day int, mut func(*planRowView)) planRowView {
		r := withActivity(attnRow(id), time.Date(2026, 7, day, 0, 0, 0, 0, time.UTC))
		mut(&r)
		return r
	}
	rows := []planRowView{
		mk("gd-fresh", 28, func(r *planRowView) {}),                                        // residual, freshest
		mk("gd-stale", 2, func(r *planRowView) {}),                                          // residual, oldest
		mk("gd-dec-new", 27, func(r *planRowView) { r.OpenDecisions = 1 }),                  // decision, newer
		mk("gd-dec-old", 3, func(r *planRowView) { r.OpenDecisions = 1 }),                   // decision, oldest → very top
		mk("gd-esc", 10, func(r *planRowView) { r.Escalations = planEscalationInfo{Count: 1} }),
		mk("gd-ready", 15, func(r *planRowView) { r.PlanState = string(planStateReady) }),
		mk("gd-stall", 20, func(r *planRowView) { r.Stall = planStallInfo{Stalled: true} }),
	}
	sortPlanRows(rows)
	want := []string{
		"gd-dec-old", "gd-dec-new", // decisions, oldest-first
		"gd-esc",   // escalation
		"gd-ready", // ready
		"gd-stall", // stalled
		"gd-fresh", "gd-stale", // residual, freshest-first
	}
	if got := rids(rows); got != join(want) {
		t.Fatalf("full ranking =\n  %s\nwant\n  %s", got, join(want))
	}
}

// rids renders row ids as a compact, space-joined string for failure messages.
func rids(rows []planRowView) string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return join(ids)
}

func join(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += " "
		}
		out += id
	}
	return out
}
