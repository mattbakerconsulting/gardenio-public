package steward

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/steward/gardenerwatch"
)

// A building plan of the home garden with no live gardener is shown to the
// steward every wake, with how its gardener stopped and the ask that follows
// (gd-hVTkwcDwxK): 18 of 26 such plans went unnoticed for days on 10-31.

func homePlan(id, title, state string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "epic", Title: title, Status: "open",
		Labels: []string{"project:" + home, "plan"}, Fields: map[string]string{"plan_state": state}}
}

func homeStep(d *fakeDaemon, id, plan, status string, waiting bool) {
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: id, Type: "task", Title: "step " + id, Status: status,
		Labels: []string{"project:" + home}})
	d.parent[id] = plan
	if waiting {
		d.waiting[id] = true
	}
}

// The gardener-less list is gardenerwatch's, tested there; here, the home
// prompt shows it and says what it caps.
func TestRenderPrompt_StalledPlansCapped(t *testing.T) {
	d := newHomeFarm()
	for i := range gardenerwatch.StalledShown + 3 {
		d.beads = append(d.beads, homePlan("gd-q"+string(rune('a'+i)), "plan", "building"))
	}
	st, err := LoadLand(context.Background(), d, home, time.Now(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// The farm's own gd-plan makes 24 gardener-less plans.
	if len(st.Land.Stalled) != gardenerwatch.StalledShown || st.Land.StalledMore != 4 {
		t.Fatalf("shown %d, more %d", len(st.Land.Stalled), st.Land.StalledMore)
	}
	p, _ := RenderPrompt(st.Land)
	if !strings.Contains(p, "(4 more not shown.)") {
		t.Fatalf("the cap is not said:\n%s", p)
	}
	for _, want := range []string{
		"Plans with no live gardener (building, but nothing runs their steps):\n- " + gardenerwatch.DescribeStalled(st.Land.Stalled[0]) + "\n",
		"SEE TO PLANS WITH NO LIVE GARDENER", "gb plan rearm <plan>", "gb plan gardener <plan>", "gb plan complete <plan>",
		"max_runs, wall_clock, no_progress_for, bead_query", "you bind, fire\n   and switch the home garden's plan gardeners yourself",
		`kd comment add <plan> "steward: re-armed (or rebound) the gardener`, "steps ready; seen to\"", `End it "; seen to" every time`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("home prompt lacks %q", want)
		}
	}
	// The owner asked (11-02) for no decisions that ask them to run a command.
	for _, gone := range []string{"has no gardener: rebind it?", "I will rebind it", "I will complete it", "You cannot bind a gardener"} {
		if strings.Contains(p, gone) {
			t.Errorf("home prompt still has %q", gone)
		}
	}
}

// A tenant land's steward runs its own plans' steps, so it is shown none.
func TestLoadLand_TenantHasNoStalledPlans(t *testing.T) {
	st, err := LoadLand(context.Background(), newFarm(), land, time.Now(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := RenderPrompt(st.Land)
	if len(st.Land.Stalled) != 0 || strings.Contains(p, "no live gardener") {
		t.Fatalf("tenant land shows gardener-less plans: %+v", st.Land.Stalled)
	}
}
