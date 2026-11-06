package main

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenhub"
)

func usdp(v float64) *float64 { return &v }

// The /costs by-project rows carry the budget the /garden header shows
// (gd-GmJYMnSKGJ): copied from the farm's cost row, never recomputed.
func TestRollupBudgetOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *gardenhub.FarmCostProject
		want *costsRollupBudget
	}{
		{"no farm row", nil, nil},
		{"no budget", &gardenhub.FarmCostProject{Project: "a", Has: true}, nil},
		{"month, ok", &gardenhub.FarmCostProject{Has: true, HasBudget: true, BudgetUSD: usdp(250), Period: gardenhub.BudgetMonth, RemainingUSD: usdp(59.8), Level: gardenhub.BudgetLevelOK},
			&costsRollupBudget{BudgetLabel: "$250.00 a month", RemainingLabel: "$59.80", Level: "ok"}},
		{"7d, over", &gardenhub.FarmCostProject{Has: true, HasBudget: true, BudgetUSD: usdp(40), Period: gardenhub.Budget7d, RemainingUSD: usdp(0), Level: gardenhub.BudgetLevelOver},
			&costsRollupBudget{BudgetLabel: "$40.00 a rolling week", RemainingLabel: "$0.00", Level: "over"}},
		// The farm's spend read is down: the budget is known, what is left is a dash, never the full budget.
		{"spend unknown", &gardenhub.FarmCostProject{Has: false, HasBudget: true, BudgetUSD: usdp(100), Period: gardenhub.BudgetMonth},
			&costsRollupBudget{BudgetLabel: "$100.00 a month", RemainingLabel: "—"}},
	} {
		got := rollupBudgetOf(tc.in)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: rollupBudgetOf = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestApplyRollupRowBudgets(t *testing.T) {
	farm := map[string]*gardenhub.FarmCostProject{
		"gardenio": {Project: "gardenio", Has: true, HasBudget: true, BudgetUSD: usdp(100), Period: gardenhub.BudgetMonth, RemainingUSD: usdp(20), Level: gardenhub.BudgetLevelWarn},
		"quiet":    {Project: "quiet", Has: true},
	}
	rows := []costsRollupRow{{Key: "gardenio"}, {Key: "quiet"}, {Key: "(none)"}}
	applyRollupRowBudgets(rows, func(p string) *gardenhub.FarmCostProject { return farm[p] })
	if b := rows[0].Budget; b == nil || b.Level != "warn" || b.RemainingLabel != "$20.00" {
		t.Errorf("gardenio budget = %+v", b)
	}
	if rows[1].Budget != nil || rows[2].Budget != nil {
		t.Errorf("rows without a budget got one: %+v, %+v", rows[1].Budget, rows[2].Budget)
	}
}

// The table renders the budget line with its level class, and the flag in words.
func TestCostsRollupTable_RendersBudgetLevel(t *testing.T) {
	srv := NewServer(agentLinkFake(), slog.Default(), "", "", "", nil)
	view := costsRollupView{Available: true, RollupBy: "project", RangeLabel: "last 24h", TotalLabel: "$90.00", Rows: []costsRollupRow{
		{Rank: 1, Key: "gardenio", USDLabel: "$80.00", Budget: &costsRollupBudget{BudgetLabel: "$100.00 a month", RemainingLabel: "$0.00", Level: "over"}},
		{Rank: 2, Key: "beta", USDLabel: "$8.00", Budget: &costsRollupBudget{BudgetLabel: "$40.00 a rolling week", RemainingLabel: "$8.00", Level: "warn"}},
		{Rank: 3, Key: "alpha", USDLabel: "$2.00"},
	}}
	w := httptest.NewRecorder()
	srv.renderPartial(w, httptest.NewRequest("GET", "/costs/rollup?part=table", nil), "templates/partials/costs_rollup.html", "costs_rollup_table", map[string]any{"Rollup": view})
	body := w.Body.String()
	for _, want := range []string{
		"costs-rollup-budget costs-rollup-budget-over", "budget $100.00 a month · $0.00 left", "over budget",
		"costs-rollup-budget costs-rollup-budget-warn", "budget $40.00 a rolling week · $8.00 left", "75%+ spent",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("table lacks %q; body:\n%s", want, body)
		}
	}
	if n := strings.Count(body, `class="meta costs-rollup-budget`); n != 2 {
		t.Errorf("%d budget lines, want 2 (alpha has no budget)", n)
	}
}
