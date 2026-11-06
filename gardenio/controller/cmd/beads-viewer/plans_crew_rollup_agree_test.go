package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/planspend"
)

// plans_crew_rollup_agree_test.go — the /plans crew card and the controller's
// plan_spend_usd rollup (internal/planspend, gd-kcy4IFNzv5) must sum the same
// crew to the cent: /garden shows the rollup, /plans its own live aggregation.
// The fixture covers every edge of the walk, so a change to either side's crew
// set or per-agent figure goes red here.

// seedPlanSpendAgreeFake extends the /plans seed with a phase, a sub-plan and a
// crew of every kind. Expected: $5.80 over 4 crew, 2 of them live.
func seedPlanSpendAgreeFake() *fakeDaemon {
	f := seedPlansFake()
	plan := f.beads["gd-plan1"]
	plan.Fields = map[string]string{"phase_order": `["gd-ph1"]`}

	add := func(b *beadsapi.BeadDetail, parent string) {
		f.beads[b.ID] = b
		f.revDeps[parent] = append(f.revDeps[parent], beadsapi.Dependency{BeadID: b.ID, DependsOnID: parent, Type: "parent-child"})
	}
	// A phase listed in phase_order, with a step under it.
	add(&beadsapi.BeadDetail{ID: "gd-ph1", Title: "Phase 1", Type: "epic", Status: "open"}, "gd-plan1")
	add(&beadsapi.BeadDetail{ID: "gd-step2", Title: "Phase step", Type: "task", Status: "in_progress"}, "gd-ph1")
	// A child epic NOT in phase_order is a sub-plan: its crew are not this plan's.
	add(&beadsapi.BeadDetail{ID: "gd-sub1", Title: "Sub-plan", Type: "epic", Status: "open"}, "gd-plan1")
	add(&beadsapi.BeadDetail{ID: "gd-substep", Title: "Sub-plan step", Type: "task", Status: "open"}, "gd-sub1")

	cost := func(b *beadsapi.BeadDetail, total, live string) {
		b.Fields["total_cost_usd"] = total
		if live != "" {
			b.Fields["live_cost_usd"] = live
		}
	}
	// Live crew on a flat step: total + live estimate = $2.50. Also assigned to
	// the phase step, and counted once.
	c1 := seedCrew(f, "gd-c1", "gardenio-agree-live", "gd-step1", "working", "in_progress")
	cost(c1, "2.00", "0.50")
	f.revDeps["gd-step2"] = append(f.revDeps["gd-step2"], beadsapi.Dependency{BeadID: "gd-c1", DependsOnID: "gd-step2", Type: "assigned"})
	// Closed crew on the phase step: total only, its stale live figure ignored = $1.75.
	cost(seedCrew(f, "gd-c2", "gardenio-agree-done", "gd-step2", "idle", "closed"), "1.75", "9.99")
	// Alive-state crew whose heartbeat is 2h old: not live, total only = $0.40.
	c3 := seedCrew(f, "gd-c3", "gardenio-agree-hung", "gd-step1", "working", "in_progress")
	c3.UpdatedAt = crewFixtureNow.Add(-2 * time.Hour)
	cost(c3, "0.40", "3.00")
	// Live crew anchored on the epic itself: $1.15.
	cost(seedCrew(f, "gd-c4", "gardenio-agree-epic", "gd-plan1", "working", "in_progress"), "1.10", "0.05")
	// The plan-gardener is assigned to the epic but is not crew.
	g := seedCrew(f, "gd-g1", "gardenio-plan-gardener-agree", "gd-plan1", "working", "in_progress")
	g.Fields["role"] = "plan-gardener"
	g.Labels = []string{planGardenerRoleLabel}
	cost(g, "50.00", "1.00")
	// Crew of the sub-plan.
	cost(seedCrew(f, "gd-c5", "gardenio-agree-sub", "gd-substep", "idle", "closed"), "7.00", "")
	return f
}

func TestPlanCrewSpend_AgreesWithControllerRollup(t *testing.T) {
	f := seedPlanSpendAgreeFake()
	ctx := context.Background()
	plan := f.beads["gd-plan1"]

	// /plans: the index card's rollup over its rendered steps.
	s := NewServer(f, slog.Default(), "", "", "", nil)
	s.SetNow(crewFixtureNowFn)
	steps := s.planIndexStepData(ctx, []*beadsapi.BeadDetail{plan})["gd-plan1"]
	if steps == nil {
		t.Fatal("planIndexStepData returned nothing for gd-plan1")
	}
	card := s.planCrewBulk(ctx, map[string][]string{"gd-plan1": steps.RenderedStepIDs}, nil)["gd-plan1"]

	// /garden: the controller's rollup over the same daemon.
	rolls, err := planspend.Plans(ctx, f, []*beadsapi.BeadDetail{plan}, crewFixtureNow)
	if err != nil {
		t.Fatalf("planspend.Plans: %v", err)
	}
	r := rolls["gd-plan1"]

	cents := func(usd float64) int64 { return int64(math.Round(usd * 100)) }
	if cents(r.USD) != 580 {
		t.Errorf("controller rollup = $%.4f over %d crew, want $5.80 over 4", r.USD, r.Crew)
	}
	if cents(card.spendUSD) != cents(r.USD) {
		t.Errorf("/plans crew card spend $%.4f != controller rollup $%.4f", card.spendUSD, r.USD)
	}
	if card.Total != r.Crew || r.Crew != 4 {
		t.Errorf("crew count: /plans card %d, controller rollup %d, want 4", card.Total, r.Crew)
	}
	if r.Live != 2 {
		t.Errorf("controller rollup live = %d, want 2 (gd-c1, gd-c4)", r.Live)
	}
}

// TestPlanTileSpend_AgreesWithPlansCard: the figure a /garden crop tile, tree
// and cockpit say for a plan (gd-BL2JQbsfyy) is the /plans crew card's, for
// the same fixture. The controller's rollup is written onto the epic as
// cmd/controller's plan spend sync writes it, read back as the farm reads it
// (gardenhub.PlanCostOf), and said by web/plans.js spendText under node; it
// must read "$5.80, 4 gardeners", the card's fmtUSD over its own crew.
func TestPlanTileSpend_AgreesWithPlansCard(t *testing.T) {
	f := seedPlanSpendAgreeFake()
	ctx := context.Background()
	plan := f.beads["gd-plan1"]

	s := NewServer(f, slog.Default(), "", "", "", nil)
	s.SetNow(crewFixtureNowFn)
	steps := s.planIndexStepData(ctx, []*beadsapi.BeadDetail{plan})["gd-plan1"]
	if steps == nil {
		t.Fatal("planIndexStepData returned nothing for gd-plan1")
	}
	card := s.planCrewBulk(ctx, map[string][]string{"gd-plan1": steps.RenderedStepIDs}, nil)["gd-plan1"]
	want := fmtUSD(card.spendUSD) + ", " + strconv.Itoa(card.Total) + " gardeners"
	if want != "$5.80, 4 gardeners" {
		t.Fatalf("/plans crew card = %q, want the fixture's $5.80 over 4 crew", want)
	}

	rolls, err := planspend.Plans(ctx, f, []*beadsapi.BeadDetail{plan}, crewFixtureNow)
	if err != nil {
		t.Fatalf("planspend.Plans: %v", err)
	}
	r := rolls["gd-plan1"]
	epic := &beadsapi.BeadDetail{ID: plan.ID, Type: plan.Type, Status: plan.Status, Fields: map[string]string{
		planspend.FieldUSD:      planspend.FormatUSD(r.USD),
		planspend.FieldSyncedAt: crewFixtureNow.Format(time.RFC3339),
		planspend.FieldCrew:     strconv.Itoa(r.Crew),
		planspend.FieldLive:     strconv.Itoa(r.Live),
	}}
	cost := gardenhub.PlanCostOf(epic, crewFixtureNow.Add(time.Minute))
	if !cost.Has || cost.Stale || cost.USD == nil || fmtUSD(*cost.USD) != fmtUSD(card.spendUSD) || cost.Crew != card.Total {
		t.Fatalf("farm cost = %+v, want the card's %s over %d crew, fresh", cost, fmtUSD(card.spendUSD), card.Total)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: the Go figures agree; the tile's words are not checked")
	}
	plans, err := filepath.Abs(filepath.Join("..", "..", "internal", "gardenhub", "web", "plans.js"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cost)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", `process.stdout.write(require(process.argv[1]).spendText(JSON.parse(process.argv[2])))`, plans, string(raw)).CombinedOutput()
	if err != nil {
		t.Fatalf("node plans.js spendText: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("crop tile says %q, /plans crew card %q", got, want)
	}
}

// TestPlanCrewClassification_MatchesPlanspend pins the copies planspend keeps of
// the crew classification: the live flag, the heartbeat threshold and the
// plan-gardener filter.
func TestPlanCrewClassification_MatchesPlanspend(t *testing.T) {
	if planspend.CrewStaleAfter != agentStaleThreshold {
		t.Errorf("planspend.CrewStaleAfter = %v, agentStaleThreshold = %v", planspend.CrewStaleAfter, agentStaleThreshold)
	}
	states := []string{"working", "spawning", "yielding", "idle", "failed", "rate_limited", "done", ""}
	for s := range agentStateAlive {
		states = append(states, s)
	}
	ages := []time.Duration{0, 5 * time.Minute, agentStaleThreshold - time.Second, agentStaleThreshold, 3 * time.Hour}
	for _, state := range states {
		for _, status := range []string{"open", "in_progress", "closed"} {
			for _, age := range ages {
				b := &beadsapi.BeadDetail{ID: "gd-x", Type: "agent", Status: status, Fields: map[string]string{"agent_state": state}}
				if age > 0 {
					b.UpdatedAt = crewFixtureNow.Add(-age)
				}
				_, _, live := classifyCrewBead(b, crewFixtureNow)
				if got := planspend.CrewLive(b, crewFixtureNow); got != live {
					t.Errorf("state=%q status=%q age=%v: planspend.CrewLive = %v, classifyCrewBead live = %v", state, status, age, got, live)
				}
			}
		}
	}
	for _, b := range []*beadsapi.BeadDetail{
		{Fields: map[string]string{"role": "crew"}},
		{Fields: map[string]string{"role": "plan-gardener"}},
		{Fields: map[string]string{"role": " plan-gardener "}},
		{Fields: map[string]string{}, Labels: []string{planGardenerRoleLabel}},
		{Fields: map[string]string{"role": "crew"}, Labels: []string{"role:crew"}},
	} {
		if got, want := planspend.IsPlanGardener(b), isPlanGardenerAgent(b); got != want {
			t.Errorf("fields=%v labels=%v: planspend.IsPlanGardener = %v, isPlanGardenerAgent = %v", b.Fields, b.Labels, got, want)
		}
	}
}
