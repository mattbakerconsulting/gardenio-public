package main

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"gardenio/controller/internal/gardenhub"
)

// farmSpendSource feeds the /garden farm's per-project cost rows
// (gardenhub.SpendSource, gd-T6zwDjWLBI) from the real-bill tag gauge the
// /costs rollup reads by project, and the purse's account budgets
// (gardenhub.BudgetSource, gd-594kYy1IYo) from the accounts /costs lists, so
// /garden and /costs agree.
type farmSpendSource struct{ s *Server }

// errFarmSpendUnavailable is returned when vmsingle is not wired or the query
// failed: the farm then shows the figures as unknown, never as $0.
var errFarmSpendUnavailable = errors.New("costs rollup unavailable")

// buildFarmProjectSpendQuery is the per-project spend over a trailing window.
// It is unranked (not topk), so a budgeted project outside the top spenders
// still gets its figure. The window is whole seconds, rounded up.
func buildFarmProjectSpendQuery(window time.Duration) string {
	secs := int64(math.Ceil(window.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return costsTagGaugeSpendExpr(`dim="project"`, strconv.FormatInt(secs, 10)+"s")
}

// ProjectSpendUSD implements gardenhub.SpendSource. A sample that is not a
// number is skipped and a negative one (a gauge reset inside the window) is
// floored to 0, as costsRollupRowsFromVector does.
func (f farmSpendSource) ProjectSpendUSD(ctx context.Context, window time.Duration) (map[string]float64, error) {
	if f.s.metricsUpstream == "" {
		return nil, errFarmSpendUnavailable
	}
	resp, ok := f.s.instantVectorQuery(ctx, buildFarmProjectSpendQuery(window))
	if !ok {
		return nil, errFarmSpendUnavailable
	}
	out := make(map[string]float64, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		v, ok := parseVMSampleValue(r.Value[1])
		if !ok {
			continue
		}
		project := r.Metric[costsRollupTagGaugeKeyLabel]
		if project == "" {
			continue
		}
		out[project] += math.Max(0, v)
	}
	return out, nil
}

// FleetBudgetUSD implements gardenhub.BudgetSource: the budget rollup of the
// /costs summary (buildCostsSummary over costAccounts), the same accounts and
// the same shared-key dedup.
func (f farmSpendSource) FleetBudgetUSD(ctx context.Context) (gardenhub.FleetBudget, error) {
	now := time.Now().UTC()
	accounts, _, err := f.s.costAccounts(ctx, now)
	if err != nil {
		return gardenhub.FleetBudget{}, err
	}
	sum := buildCostsSummary(accounts, now)
	return gardenhub.FleetBudget{
		Accounts:     sum.BudgetCount,
		BudgetUSD:    sum.BudgetUSD,
		SpentUSD:     sum.BudgetSpentUSD,
		RemainingUSD: sum.RemainingUSD,
	}, nil
}
