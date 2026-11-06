package gardenhub

// farm_plan_cost.go — a plan's crew spend (gd-kcy4IFNzv5). The controller rolls
// it up onto the plan epic's plan_spend_* fields (internal/planspend); the farm
// only reads the fields off the epic row it already has, so the figure costs no
// daemon call here.

import (
	"math"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planspend"
)

// PlanCostOf reads a plan epic's rollup fields: the figure a crop tile, a tree and the cockpit show (web/plans.js
// spendText). Exported for the beads viewer's test that it agrees with the /plans crew card.
func PlanCostOf(b *beadsapi.BeadDetail, now time.Time) FarmPlanCost {
	return planCostAt(b, now, time.Time{})
}

// planCostAt is PlanCostOf judging Stale by passAt, the newest rollup stamp
// across the plans the farm holds (planSpendPassAt), since the controller does
// not re-stamp an unchanged figure (planspend.ReadWithPass).
func planCostAt(b *beadsapi.BeadDetail, now, passAt time.Time) FarmPlanCost {
	r := planspend.ReadWithPass(b.Fields, now, passAt)
	if !r.Has {
		return FarmPlanCost{}
	}
	usd := math.Round(r.USD*100) / 100
	c := FarmPlanCost{Has: true, USD: &usd, Crew: r.Crew, Live: r.Live, Stale: r.Stale}
	if !r.SyncedAt.IsZero() {
		c.SyncedAt = r.SyncedAt.Unix()
	}
	return c
}

// planSpendPassAt is the newest plan_spend_synced_at among plans: when the
// controller's rollup pass last wrote any plan, zero when it never has.
func planSpendPassAt(plans map[string]*beadsapi.BeadDetail, now time.Time) time.Time {
	var at time.Time
	for _, b := range plans {
		if t := planspend.Read(b.Fields, now).SyncedAt; t.After(at) {
			at = t
		}
	}
	return at
}

// slimCostOf is PlanCostOf cut to the figure and its crew, for a seed or a closed plan; it carries no
// Stale, so it needs no passAt.
func slimCostOf(b *beadsapi.BeadDetail, now time.Time) FarmSlimCost {
	c := PlanCostOf(b, now)
	return FarmSlimCost{Has: c.Has, USD: c.USD, Crew: c.Crew}
}
