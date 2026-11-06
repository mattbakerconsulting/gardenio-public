package bridge

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
)

// landWallDaemon serves a small farm to gardenerPreFireCheck's Lands spawn
// wall (step 8.3): a tenant land "tomato-patch" and the operator land
// "gardenio". Everything else is darkBudgetDaemon's no-op stub.
type landWallDaemon struct {
	*darkBudgetDaemon
	beads   []*beadsapi.BeadDetail
	listErr error
}

func (d *landWallDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if q.Search == gardenersPauseTitle {
		return &beadsapi.ListBeadsResult{}, nil
	}
	if d.listErr != nil {
		return nil, d.listErr
	}
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if (len(q.Types) == 0 || slices.Contains(q.Types, b.Type)) &&
			(len(q.Statuses) == 0 || slices.Contains(q.Statuses, b.Status)) &&
			!slices.ContainsFunc(q.Labels, func(l string) bool { return !slices.Contains(b.Labels, l) }) &&
			q.Offset == 0 {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func newLandWallDaemon(spentUSD string) (*landWallDaemon, *beadsapi.BeadDetail) {
	land := &beadsapi.BeadDetail{ID: "kd-1", Type: "project", Title: "tomato-patch", Status: "open",
		Fields: map[string]string{"land_kind": "tenant", "account": "lands-tomato-patch"}}
	return &landWallDaemon{darkBudgetDaemon: newDarkBudgetDaemon(), beads: []*beadsapi.BeadDetail{
		land,
		{ID: "kd-2", Type: "project", Title: "gardenio", Status: "open", Fields: map[string]string{"land_frozen": "true"}},
		{ID: "kd-3", Type: "credit_rate", Status: "closed", Fields: map[string]string{"version": "1", "llm_multiplier": "1.5"}},
		{ID: "kd-4", Type: "credit_grant", Status: "closed", Labels: []string{"project:tomato-patch"},
			Fields: map[string]string{"land": "tomato-patch", "amount_credits": "100"}},
		{ID: "kd-5", Type: "account", Title: "lands-tomato-patch", Status: "open", Fields: map[string]string{
			"total_cost_usd": spentUSD, purse.FieldSpendSyncedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}},
	}}, land
}

func landGardener(project string) TriggerDef {
	return TriggerDef{ID: "gd-steward", Project: project, Labels: []string{"role:gardener"}, Gardener: &GardenerSpec{RunBudget: 5}}
}

// landSteward is a land's steward trigger as steward.Bind labels it: a
// gardener whose fires spawn the steward role.
func landSteward(project string) TriggerDef {
	return TriggerDef{ID: "gd-steward", Project: project, Role: wall.StewardRole,
		Labels:   []string{"trigger:multi", "role:gardener", "steward", "gardener-land:" + project, "project:" + project},
		Gardener: &GardenerSpec{RunBudget: 5}}
}

func withTenantSpawns(t *testing.T, on bool) {
	t.Helper()
	prev := gardenerTenantSpawns
	gardenerTenantSpawns = func() bool { return on }
	t.Cleanup(func() { gardenerTenantSpawns = prev })
}

func TestGardenerPreFire_LandWallRefuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spent    string
		frozen   bool
		spawnsOn bool
	}{
		{"frozen", "10", true, true},
		{"tenant spawns off", "10", false, false},
		{"purse at the wall", "64", false, true},
		{"purse unknown (no spend figure synced)", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTenantSpawns(t, tc.spawnsOn)
			d, land := newLandWallDaemon(tc.spent)
			if tc.frozen {
				land.Fields["land_frozen"] = "true"
			}
			if tc.spent == "" {
				d.beads[4].Fields[purse.FieldSpendSyncedAt] = ""
			}
			skip, reason := gardenerPreFireCheck(context.Background(), d, nil, landGardener("tomato-patch"))
			if !skip || reason != gardenerSkipLandWall {
				t.Fatalf("got skip=%v reason=%q, want the land wall", skip, reason)
			}
			if len(d.stateWrites) != 0 || len(d.comments) != 0 {
				t.Errorf("the wall is self-releasing and writes nothing: states %v comments %v", d.stateWrites, d.comments)
			}
		})
	}
}

func TestGardenerPreFire_LandWallPasses(t *testing.T) {
	withTenantSpawns(t, true)
	d, _ := newLandWallDaemon("10")
	// A tenant land with credits, and the operator land (frozen-looking, but
	// land_frozen means nothing on an operator land) both fire.
	for _, project := range []string{"tomato-patch", "gardenio", ""} {
		if _, reason := gardenerPreFireCheck(context.Background(), d, nil, landGardener(project)); reason == gardenerSkipLandWall {
			t.Errorf("%q: refused by the land wall", project)
		}
	}
	withTenantSpawns(t, false)
	if _, reason := gardenerPreFireCheck(context.Background(), d, nil, landGardener("gardenio")); reason == gardenerSkipLandWall {
		t.Error("tenant spawns off must not touch an operator land's gardener")
	}
}

func TestGardenerPreFire_LandReadFailsOpen(t *testing.T) {
	withTenantSpawns(t, false)
	d, _ := newLandWallDaemon("10")
	d.listErr = errors.New("daemon down")
	if _, reason := gardenerPreFireCheck(context.Background(), d, nil, landGardener("tomato-patch")); reason == gardenerSkipLandWall {
		t.Error("an unreadable land fails open in the pre-fire (the controller admission fails closed)")
	}
}

// With tenant spawns off the land's steward still fires (owner ruling
// gd-KOAoUELMWo); a plan-gardener or any other role's gardener on the land
// does not, and every other refusal still stops the steward.
func TestGardenerPreFire_StewardFiresWithTenantSpawnsOff(t *testing.T) {
	withTenantSpawns(t, false)
	d, land := newLandWallDaemon("10")
	if skip, reason := gardenerPreFireCheck(context.Background(), d, nil, landSteward("tomato-patch")); skip && reason == gardenerSkipLandWall {
		t.Fatalf("the steward with tenant spawns off: refused by the land wall")
	}
	for _, role := range []string{"", "crew", "gardener", "lands-gardener", "crew,steward"} {
		trig := landSteward("tomato-patch")
		trig.Role = role
		if skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig); !skip || reason != gardenerSkipLandWall {
			t.Errorf("role %q with tenant spawns off: skip=%v reason=%q, want the land wall", role, skip, reason)
		}
	}
	// The steward label alone is not the steward: the role decides.
	plan := landGardener("tomato-patch")
	plan.Labels = append(plan.Labels, "steward")
	if skip, reason := gardenerPreFireCheck(context.Background(), d, nil, plan); !skip || reason != gardenerSkipLandWall {
		t.Errorf("a plan-gardener labelled steward: skip=%v reason=%q, want the land wall", skip, reason)
	}

	land.Fields["land_frozen"] = "true"
	if skip, reason := gardenerPreFireCheck(context.Background(), d, nil, landSteward("tomato-patch")); !skip || reason != gardenerSkipLandWall {
		t.Errorf("frozen land, steward: skip=%v reason=%q, want the land wall", skip, reason)
	}
	land.Fields["land_frozen"] = ""
	d.beads[4].Fields["total_cost_usd"] = "64"
	if skip, reason := gardenerPreFireCheck(context.Background(), d, nil, landSteward("tomato-patch")); !skip || reason != gardenerSkipLandWall {
		t.Errorf("purse at the wall, steward: skip=%v reason=%q, want the land wall", skip, reason)
	}
}
