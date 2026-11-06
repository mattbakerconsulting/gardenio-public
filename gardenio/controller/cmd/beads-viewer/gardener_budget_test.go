package main

import "testing"

// TestBudgetBurn_StandingSuppressesMeter is the direct-method regression for bug
// ka-0000006238: the run-budget burn meter must be suppressed for a STANDING
// gardener even when it carries a (refilled, non-enforced) run_budget. Otherwise
// a standing gardener — routinely RunsSpawned >> RunBudget — renders a red "at
// budget / over" bar that reads as a false "about to stop on max_runs / runaway"
// signal, exactly the misread that spawned a phantom-runaway investigation.
func TestBudgetBurn_StandingSuppressesMeter(t *testing.T) {
	cases := []struct {
		name        string
		view        gardenerView
		wantHasData bool
	}{
		{
			name:        "standing_with_refilled_budget_suppressed",
			view:        gardenerView{IsStanding: true, RunBudget: 1672, RunsSpawned: 2093},
			wantHasData: false,
		},
		{
			name:        "standing_under_budget_still_suppressed",
			view:        gardenerView{IsStanding: true, RunBudget: 400, RunsSpawned: 50},
			wantHasData: false,
		},
		{
			name:        "non_standing_over_budget_still_renders",
			view:        gardenerView{IsStanding: false, RunBudget: 1672, RunsSpawned: 2093},
			wantHasData: true,
		},
		{
			name:        "non_standing_under_budget_renders",
			view:        gardenerView{IsStanding: false, RunBudget: 200, RunsSpawned: 77},
			wantHasData: true,
		},
		{
			name:        "no_budget_suppressed_regardless",
			view:        gardenerView{IsStanding: false, RunBudget: 0, RunsSpawned: 12},
			wantHasData: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.view.BudgetBurn()
			if b.HasData != tc.wantHasData {
				t.Fatalf("BudgetBurn().HasData = %v, want %v", b.HasData, tc.wantHasData)
			}
			// A suppressed meter must be a fully zero-value struct so the template
			// renders nothing — never a stray "over"/"at budget" leak.
			if !tc.wantHasData && (b.Level != "" || b.Over || b.Pct != 0 || b.Spawned != 0) {
				t.Errorf("suppressed meter should be zero-value, got %+v", b)
			}
			// A rendered non-standing over-budget meter must still flag the true
			// overrun (guarding the blast radius of the standing gate).
			if tc.name == "non_standing_over_budget_still_renders" && (!b.Over || b.Level != "over") {
				t.Errorf("non-standing over-budget meter lost its over/level flags: %+v", b)
			}
		})
	}
}
