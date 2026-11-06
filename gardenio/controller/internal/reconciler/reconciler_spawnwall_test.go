package reconciler

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/wall"
)

// The reconciler's create-missing pass is the twin of the event door's Lands
// spawn wall (step 8.3): a new spawn into a refused tenant land creates no pod
// and is recorded, not queued; a running agent's pod recreation and every
// other land are untouched.

func wallBead(id, project, name, state string) beadsapi.AgentBead {
	return beadsapi.AgentBead{ID: id, Project: project, Mode: "crew", Role: "crew", AgentName: name, AgentState: state}
}

type wallSpy struct {
	checks   map[string]int
	recorded []string
}

func (s *wallSpy) hook(refuse map[string]wall.Refusal) SpawnWall {
	s.checks = map[string]int{}
	return SpawnWall{
		Check: func(_ context.Context, project, _ string) wall.Refusal {
			s.checks[project]++
			return refuse[project]
		},
		Record: func(_ context.Context, bead beadsapi.AgentBead, r wall.Refusal) {
			s.recorded = append(s.recorded, bead.ID+":"+r.Code)
		},
	}
}

func TestReconcile_SpawnWall_RefusesNewTenantSpawns(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		wallBead("bd-t1", "tomato-patch", "t1", "spawning"),
		wallBead("bd-t2", "tomato-patch", "t2", ""),
		wallBead("bd-op", "gardenio", "op", "spawning"),
	}}
	mgr := &mockManager{}
	updater := &mockStateUpdater{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetStateUpdater(updater)
	spy := &wallSpy{}
	r.SetSpawnWall(spy.hook(map[string]wall.Refusal{
		"tomato-patch": {Code: wall.CodePurseWall, Reason: "empty"},
	}))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 || mgr.created[0].Project != "gardenio" {
		t.Fatalf("only the operator land's pod may be created, got %+v", mgr.created)
	}
	slices.Sort(spy.recorded) // equal CreatedAt: the pass order is not fixed
	if len(spy.recorded) != 2 || spy.recorded[0] != "bd-t1:purse_wall" || spy.recorded[1] != "bd-t2:purse_wall" {
		t.Errorf("both refused beads must be recorded, got %v", spy.recorded)
	}
	if spy.checks["tomato-patch"] != 1 {
		t.Errorf("the wall is read once per project per pass, got %d", spy.checks["tomato-patch"])
	}
	for _, u := range updater.updates {
		if u.beadID != "bd-op" {
			t.Errorf("a refused bead must not be marked %s (budget_exceeded is its state), got %+v", u.state, u)
		}
	}
}

func TestReconcile_SpawnWall_RunningAgentRecreatedUnchecked(t *testing.T) {
	// A working agent whose pod is gone (spot loss) is not a new spawn: running
	// agents continue, and the purse watcher (step 8.4) is what parks them.
	lister := &mockLister{beads: []beadsapi.AgentBead{wallBead("bd-t1", "tomato-patch", "t1", "working")}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	spy := &wallSpy{}
	r.SetSpawnWall(spy.hook(map[string]wall.Refusal{"tomato-patch": {Code: wall.CodeFrozen}}))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("a running agent's pod must be recreated, created %d", len(mgr.created))
	}
	if spy.checks["tomato-patch"] != 0 || len(spy.recorded) != 0 {
		t.Errorf("the wall must not be asked about a running agent: checks %v recorded %v", spy.checks, spy.recorded)
	}
}

func TestReconcile_SpawnWall_RefusedBeadRetriedWhenClear(t *testing.T) {
	// A bead the wall refused earlier (agent_state=budget_exceeded) spawns once
	// the land is clear again.
	lister := &mockLister{beads: []beadsapi.AgentBead{wallBead("bd-t1", "tomato-patch", "t1", wall.AgentState)}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	spy := &wallSpy{}
	r.SetSpawnWall(spy.hook(nil))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 || spy.checks["tomato-patch"] != 1 {
		t.Fatalf("created %d, checks %v; want the bead re-decided and spawned", len(mgr.created), spy.checks)
	}
}

func TestReconcile_SpawnWall_RefusedBeforeCaps(t *testing.T) {
	// Over the pod cap, a refused bead is still recorded as refused, not queued.
	lister := &mockLister{beads: []beadsapi.AgentBead{
		wallBead("bd-live", "gardenio", "live", "working"),
		wallBead("bd-t1", "tomato-patch", "t1", "spawning"),
	}}
	mgr := &mockManager{pods: []corev1.Pod{
		makePod("crew-gardenio-crew-live", "gardenio", "crew", "gardenio", "crew", "live", corev1.PodRunning),
	}}
	updater := &mockStateUpdater{}
	cfg := testConfig("gardenio")
	cfg.CoopMaxPods = 1
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetStateUpdater(updater)
	spy := &wallSpy{}
	r.SetSpawnWall(spy.hook(map[string]wall.Refusal{"tomato-patch": {Code: wall.CodeTenantSpawnsOff}}))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 0 || len(spy.recorded) != 1 || len(updater.updates) != 0 {
		t.Errorf("created %d recorded %v updates %+v; want refused and recorded only", len(mgr.created), spy.recorded, updater.updates)
	}
}

func TestReconcile_SpawnWall_RateAskedPerTenantBead(t *testing.T) {
	// The per-land spawn rate (step 10.4) is asked for each new spawn of a
	// tenant land the wall passed, never memoised, and never for other lands.
	lister := &mockLister{beads: []beadsapi.AgentBead{
		wallBead("bd-t1", "tomato-patch", "t1", "spawning"),
		wallBead("bd-t2", "tomato-patch", "t2", "spawning"),
		wallBead("bd-op", "gardenio", "op", "spawning"),
	}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	spy := &wallSpy{}
	hook := spy.hook(map[string]wall.Refusal{"tomato-patch": {Tenant: true}})
	var asked []string
	hook.Rate = func(_ context.Context, bead beadsapi.AgentBead) wall.Refusal {
		asked = append(asked, bead.ID)
		if len(asked) > 1 {
			return wall.Refusal{Code: wall.CodeSpawnRate, Tenant: true}
		}
		return wall.Refusal{Tenant: true}
	}
	r.SetSpawnWall(hook)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(asked)
	if len(asked) != 2 || asked[0] != "bd-t1" || asked[1] != "bd-t2" {
		t.Fatalf("rate asked for %v, want both tenant beads only", asked)
	}
	if len(mgr.created) != 2 {
		t.Errorf("created %d pods, want the operator land's and one tenant agent", len(mgr.created))
	}
	if len(spy.recorded) != 1 || !strings.HasSuffix(spy.recorded[0], ":spawn_rate") {
		t.Errorf("recorded %v, want one spawn_rate refusal", spy.recorded)
	}
}

func TestReconcile_SpawnWall_DecidedPerRole(t *testing.T) {
	// The wall is asked per project and role: with tenant spawns off the
	// land's steward starts and its crew does not (gd-KOAoUELMWo).
	steward := wallBead("bd-st", "tomato-patch", "st", "spawning")
	steward.Role = wall.StewardRole
	lister := &mockLister{beads: []beadsapi.AgentBead{
		steward,
		wallBead("bd-t1", "tomato-patch", "t1", "spawning"),
		wallBead("bd-t2", "tomato-patch", "t2", "spawning"),
	}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	asked := map[string]int{}
	var recorded []string
	r.SetSpawnWall(SpawnWall{
		Check: func(_ context.Context, project, role string) wall.Refusal {
			asked[project+"/"+role]++
			if role == wall.StewardRole {
				return wall.Refusal{Tenant: true}
			}
			return wall.Refusal{Code: wall.CodeTenantSpawnsOff, Tenant: true}
		},
		Record: func(_ context.Context, bead beadsapi.AgentBead, r wall.Refusal) {
			recorded = append(recorded, bead.ID+":"+r.Code)
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 || mgr.created[0].AgentName != "st" {
		t.Fatalf("only the steward's pod may be created, got %+v", mgr.created)
	}
	slices.Sort(recorded)
	if len(recorded) != 2 || recorded[0] != "bd-t1:tenant_spawns_off" || recorded[1] != "bd-t2:tenant_spawns_off" {
		t.Errorf("both crew beads must be recorded refused, got %v", recorded)
	}
	if asked["tomato-patch/steward"] != 1 || asked["tomato-patch/crew"] != 1 {
		t.Errorf("the wall is read once per project and role per pass, got %v", asked)
	}
}

func TestReconcile_SpawnWall_RunningAgentRecreateAsksHalted(t *testing.T) {
	// A running agent's lost pod is not recreated on a frozen or paused land
	// (gd-Nc7yMpW7U6): Halted is asked, once per project, and its refusal is
	// recorded; Check (the purse) is not asked for a running agent.
	lister := &mockLister{beads: []beadsapi.AgentBead{
		wallBead("bd-t1", "tomato-patch", "t1", "working"),
		wallBead("bd-t2", "tomato-patch", "t2", "idle"),
		wallBead("bd-op", "gardenio", "op", "working"),
	}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	spy := &wallSpy{}
	hook := spy.hook(nil)
	halted := map[string]int{}
	hook.Halted = func(_ context.Context, project string) wall.Refusal {
		halted[project]++
		if project == "tomato-patch" {
			return wall.Refusal{Code: wall.CodeFrozen, Tenant: true}
		}
		return wall.Refusal{}
	}
	r.SetSpawnWall(hook)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 || mgr.created[0].Project != "gardenio" {
		t.Fatalf("a frozen land's running agents must not get a new pod, created %+v", mgr.created)
	}
	slices.Sort(spy.recorded)
	if len(spy.recorded) != 2 || spy.recorded[0] != "bd-t1:land_frozen" || spy.recorded[1] != "bd-t2:land_frozen" {
		t.Errorf("both halted beads must be recorded, got %v", spy.recorded)
	}
	if halted["tomato-patch"] != 1 || len(spy.checks) != 0 {
		t.Errorf("halted asked %v (want once per project), checks %v (want none)", halted, spy.checks)
	}
}

func TestReconcile_SpawnWall_RunningAgentRecreatedPastThePurseWall(t *testing.T) {
	// The purse wall is not a kill switch: Halted passes a land at the wall,
	// so a running agent's pod is recreated, and a new spawn is still asked
	// of Check alone.
	lister := &mockLister{beads: []beadsapi.AgentBead{
		wallBead("bd-t1", "tomato-patch", "t1", "working"),
		wallBead("bd-t2", "tomato-patch", "t2", "spawning"),
	}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	spy := &wallSpy{}
	hook := spy.hook(map[string]wall.Refusal{"tomato-patch": {Code: wall.CodePurseWall, Tenant: true}})
	var halted []string
	hook.Halted = func(_ context.Context, project string) wall.Refusal {
		halted = append(halted, project)
		return wall.Refusal{}
	}
	r.SetSpawnWall(hook)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 || mgr.created[0].AgentName != "t1" {
		t.Fatalf("only the running agent's pod may be recreated, created %+v", mgr.created)
	}
	if len(halted) != 1 || spy.checks["tomato-patch"] != 1 {
		t.Errorf("halted asked %v, checks %v; want one each", halted, spy.checks)
	}
	if len(spy.recorded) != 1 || spy.recorded[0] != "bd-t2:purse_wall" {
		t.Errorf("recorded %v, want the new spawn refused at the purse wall", spy.recorded)
	}
}
