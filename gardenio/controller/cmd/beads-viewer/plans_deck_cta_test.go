package main

import "testing"

// The /plans header Deck entry point (ka-0000001498, epic ka-0000000492): a pill
// linking /plans → /deck carrying a count of plans that need a human. These
// tests pin the two invariants the pill relies on: (1) the count uses the SAME
// planAttention predicate the deck's own deckCards applies, so the header can
// never claim a different actionable count than the deck would render for the
// same rows; (2) the deck URL carries the active project scope.

func TestCountPlansNeedingHuman(t *testing.T) {
	// One row per attention tier + two that are planAttentionNone. The count must
	// be exactly the four attention-tier rows — the same set deckCards keeps.
	rows := []planRowView{
		{OpenDecisions: 2},                          // decision
		{Escalations: planEscalationInfo{Count: 1}}, // escalation
		{PlanState: string(planStateReady)},         // ready
		{Stall: planStallInfo{Stalled: true}},       // stalled
		{PlanState: string(planStateBuilding)},      // none (healthy)
		{},                                          // none (empty)
	}
	if got, want := countPlansNeedingHuman(rows), 4; got != want {
		t.Fatalf("countPlansNeedingHuman() = %d, want %d", got, want)
	}

	// The count must agree, row-for-row, with the deck's own eligibility filter.
	// deckCards drops planAttentionNone; the header count keeps the complement.
	if got, want := countPlansNeedingHuman(rows), len(deckCards(rows)); got != want {
		t.Fatalf("count %d disagrees with deckCards length %d — the pill would lie about the deck", got, want)
	}
}

func TestCountPlansNeedingHuman_Empty(t *testing.T) {
	if got := countPlansNeedingHuman(nil); got != 0 {
		t.Fatalf("countPlansNeedingHuman(nil) = %d, want 0", got)
	}
	if got := countPlansNeedingHuman([]planRowView{{}, {PlanState: string(planStateDraft)}}); got != 0 {
		t.Fatalf("countPlansNeedingHuman(all-clear) = %d, want 0", got)
	}
}

func TestDeckURLForPlans(t *testing.T) {
	cases := []struct {
		name          string
		base, project string
		want          string
	}{
		{"no project → bare deck", "", "", "/deck"},
		{"basepath preserved", "/beads", "", "/beads/deck"},
		{"project scope carried", "", "gardenio", "/deck?project=gardenio"},
		{"basepath + project", "/beads", "gardenio", "/beads/deck?project=gardenio"},
		{"project value is escaped", "", "a b/c", "/deck?project=a+b%2Fc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deckURLForPlans(tc.base, tc.project); got != tc.want {
				t.Errorf("deckURLForPlans(%q, %q) = %q, want %q", tc.base, tc.project, got, tc.want)
			}
		})
	}
}
