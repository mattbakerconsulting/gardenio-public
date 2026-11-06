package gardenhub

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planspend"
)

// setPlanSpend writes the controller's rollup fields on a plan epic.
func setPlanSpend(b *beadsapi.BeadDetail, usd string, synced int64, crew, live string) {
	b.Fields[planspend.FieldUSD] = usd
	b.Fields[planspend.FieldSyncedAt] = time.Unix(synced, 0).UTC().Format(time.RFC3339)
	b.Fields[planspend.FieldCrew] = crew
	b.Fields[planspend.FieldLive] = live
}

// TestFarmPlanCost: a plan, a seed and a closed plan carry the controller's
// rollup off their epic rows; a plan never rolled up says so with a null figure.
func TestFarmPlanCost(t *testing.T) {
	f := newTestFarm(fixtureWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)

	p := findPlan(s, "gd-planA")
	if p == nil {
		t.Fatal("plan A missing")
	}
	c := p.Cost
	if !c.Has || c.USD == nil || *c.USD != 12.35 || c.Crew != 3 || c.Live != 1 || c.Stale || c.SyncedAt != baseUnix+10_000-300 {
		t.Errorf("plan A cost = %+v (usd %v), want has, $12.35, 3 crew, 1 live, fresh", c, c.USD)
	}
	for _, seed := range s.Seeds {
		if seed.Cost.Has || seed.Cost.USD != nil {
			t.Errorf("seed %s never rolled up but cost = %+v", seed.ID, seed.Cost)
		}
	}
	var done *FarmClosedPlan
	for i := range s.ClosedPlans.Shown {
		if s.ClosedPlans.Shown[i].ID == "gd-done1" {
			done = &s.ClosedPlans.Shown[i]
		}
	}
	if done == nil || !done.Cost.Has || done.Cost.USD == nil || *done.Cost.USD != 4.5 || done.Cost.Crew != 2 {
		t.Errorf("closed plan cost = %+v, want has, $4.50, 2 crew", done)
	}
}

// TestPlanCostOfStale: a figure that was moving (live crew) goes stale three
// rollup intervals after its stamp; a settled one never does.
func TestPlanCostOfStale(t *testing.T) {
	now := time.Unix(baseUnix, 0)
	old := baseUnix - int64(planspend.StaleAfter/time.Second) - 60
	moving := fb("gd-P", "epic", "open", "", 0)
	setPlanSpend(moving, "1", old, "2", "1")
	if c := PlanCostOf(moving, now); !c.Has || !c.Stale {
		t.Errorf("moving figure past the stale bar: %+v, want stale", c)
	}
	settled := fb("gd-Q", "epic", "open", "", 0)
	setPlanSpend(settled, "1", old, "2", "0")
	if c := PlanCostOf(settled, now); !c.Has || c.Stale {
		t.Errorf("settled figure: %+v, want not stale", c)
	}
	fresh := fb("gd-R", "epic", "open", "", 0)
	setPlanSpend(fresh, "1", baseUnix-60, "2", "1")
	if c := PlanCostOf(fresh, now); c.Stale {
		t.Errorf("fresh moving figure: %+v, want not stale", c)
	}
}

// TestPlanCostPassAt: the controller no longer re-stamps an unchanged moving
// figure (gd-vwMhcJgts2), so an old own stamp stays fresh while another plan
// was written within StaleAfter, and greys once no plan has been.
func TestPlanCostPassAt(t *testing.T) {
	now := time.Unix(baseUnix, 0)
	old := baseUnix - int64(planspend.StaleAfter/time.Second) - 60
	quiet := fb("gd-P", "epic", "open", "", 0)
	setPlanSpend(quiet, "1", old, "2", "1")
	busy := fb("gd-Q", "epic", "open", "", 0)
	setPlanSpend(busy, "3", baseUnix-120, "1", "1")
	pass := planSpendPassAt(map[string]*beadsapi.BeadDetail{"gd-P": quiet, "gd-Q": busy}, now)
	if pass.Unix() != baseUnix-120 {
		t.Fatalf("passAt = %v, want the newest stamp", pass)
	}
	if c := planCostAt(quiet, now, pass); !c.Has || c.Stale {
		t.Errorf("unchanged figure with the pass alive: %+v, want not stale", c)
	}
	if c := planCostAt(quiet, now, time.Unix(old, 0)); !c.Stale {
		t.Errorf("no plan written for StaleAfter: %+v, want stale", c)
	}
}
