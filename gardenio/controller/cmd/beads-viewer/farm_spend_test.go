package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// TestBuildFarmProjectSpendQuery pins that the farm reads the by-project
// rollup's expression, unranked, over a whole-second window.
func TestBuildFarmProjectSpendQuery(t *testing.T) {
	q := buildFarmProjectSpendQuery(36*time.Hour + 1500*time.Millisecond)
	want := costsTagGaugeSpendExpr(`dim="project"`, "129602s")
	if q != want {
		t.Fatalf("query = %s\nwant %s", q, want)
	}
	if strings.Contains(q, "topk") {
		t.Errorf("query is ranked: %s", q)
	}
	rollup := buildCostsRollupQuery("project", map[string]string{}, "7d")
	if rollup != "topk(50, "+costsTagGaugeSpendExpr(`dim="project"`, "7d")+")" {
		t.Errorf("by-project rollup no longer wraps the shared expression: %s", rollup)
	}
	if got := buildFarmProjectSpendQuery(0); !strings.Contains(got, "[1s:") {
		t.Errorf("zero window not floored to 1s: %s", got)
	}
}

func TestFarmSpendSource(t *testing.T) {
	ctx := context.Background()

	// No vmsingle wired: unknown, never an empty (= $0) map.
	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	if m, err := (farmSpendSource{srv}).ProjectSpendUSD(ctx, time.Hour); err == nil {
		t.Fatalf("no upstream: got %v, want an error", m)
	}

	var query string
	status := http.StatusOK
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"value":"gardenio"},"value":[1700000000,"12.5"]},` +
			`{"metric":{"value":"monorepo"},"value":[1700000000,"-0.3"]},` +
			`{"metric":{"value":"broken"},"value":[1700000000,"NaN"]},` +
			`{"metric":{},"value":[1700000000,"4"]}` +
			`]}}`))
	}))
	t.Cleanup(upstream.Close)
	srv.SetMetricsProxy(upstream.URL)

	m, err := (farmSpendSource{srv}).ProjectSpendUSD(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if query != buildFarmProjectSpendQuery(7*24*time.Hour) {
		t.Errorf("vmsingle got %s", query)
	}
	if len(m) != 2 || m["gardenio"] != 12.5 || m["monorepo"] != 0 {
		t.Errorf("spend = %v, want gardenio 12.5, monorepo 0 (floored), nothing else", m)
	}

	status = http.StatusServiceUnavailable
	if m, err := (farmSpendSource{srv}).ProjectSpendUSD(ctx, time.Hour); err == nil {
		t.Fatalf("upstream 503: got %v, want an error", m)
	}
}

// TestFarmSpendSourceFleetBudget: the purse's account budgets are /costs'
// budget rollup: spend_limit_usd, else max_budget; spent since the last reset
// (a missing baseline counts as 0, so a never-reset account is not unspent);
// remaining floored at 0 per account; a shared key's budget booked once; E2E
// accounts and accounts with no budget left out; a failed list an error.
func TestFarmSpendSourceFleetBudget(t *testing.T) {
	acct := func(id, title string, fields map[string]string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: id, Title: title, Type: "account", Status: "open", Fields: fields}
	}
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		// $100 limit, $30 spent since the reset: $70 left.
		acct("gd-a1", "alpha", map[string]string{"spend_limit_usd": "100", "max_budget": "999", "total_cost_usd": "130", "spend_at_last_reset": "100"}),
		// Only the operator hint, never reset, overspent: $0 left, $60 spent.
		acct("gd-a2", "beta", map[string]string{"max_budget": "50", "total_cost_usd": "60"}),
		// A key shared with delta: its budget is booked once, on gamma.
		acct("gd-a3", "gamma", map[string]string{"spend_limit_usd": "40", "total_cost_usd": "10", "litellm_key": "sk-shared"}),
		acct("gd-a4", "delta", map[string]string{"spend_limit_usd": "40", "total_cost_usd": "10", "litellm_key": "sk-shared"}),
		// No budget (zero and junk are none).
		acct("gd-a5", "epsilon", map[string]string{"spend_limit_usd": "0", "max_budget": "lots", "total_cost_usd": "500"}),
		// An E2E account never counts.
		acct("gd-e2e", "e2e-acct-x", map[string]string{"spend_limit_usd": "1000", "total_cost_usd": "1"}),
	}, Total: 6}
	srv := NewServer(f, slog.Default(), "", "", "", nil)

	got, err := (farmSpendSource{srv}).FleetBudgetUSD(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := gardenhub.FleetBudget{Accounts: 3, BudgetUSD: 190, SpentUSD: 100, RemainingUSD: 100}
	if got != want {
		t.Errorf("fleet budget = %+v, want %+v", got, want)
	}
	// /costs sums the same figures.
	accounts, _, _ := srv.costAccounts(context.Background(), time.Now())
	if s := buildCostsSummary(accounts, time.Now()); s.BudgetCount != 3 || s.RemainingUSD != 100 || s.BudgetSpentUSD != 100 {
		t.Errorf("/costs summary budgets = %d, $%v left, $%v spent; want 3, $100, $100", s.BudgetCount, s.RemainingUSD, s.BudgetSpentUSD)
	}

	// No account with a budget: none, never a $0 budget.
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{acct("gd-a5", "epsilon", map[string]string{"total_cost_usd": "5"})}, Total: 1}
	if got, err = (farmSpendSource{srv}).FleetBudgetUSD(context.Background()); err != nil || got.Accounts != 0 {
		t.Errorf("no budgets = %+v (err %v), want 0 accounts", got, err)
	}

	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	if got, err := (farmSpendSource{srv}).FleetBudgetUSD(context.Background()); err == nil {
		t.Errorf("failed list: got %+v, want an error", got)
	}
}
