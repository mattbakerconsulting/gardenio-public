package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/spawnguard"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// wallDaemon is a spawnWallDaemon over a fixed set of beads.
type wallDaemon struct {
	beads []*beadsapi.BeadDetail
	err   error
	// gateErr fails only the reads of type=gate beads.
	gateErr  error
	reads    int
	fields   map[string]map[string]string
	comments map[string][]string
}

func (d *wallDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.reads++
	if d.err != nil {
		return nil, d.err
	}
	if d.gateErr != nil && slices.Contains(q.Types, "gate") {
		return nil, d.gateErr
	}
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if slices.Contains(q.Types, b.Type) && q.Offset == 0 {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (d *wallDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	if d.fields == nil {
		d.fields = map[string]map[string]string{}
	}
	d.fields[id] = fields
	return nil
}

func (d *wallDaemon) AddComment(_ context.Context, id, _, text string) error {
	if d.comments == nil {
		d.comments = map[string][]string{}
	}
	d.comments[id] = append(d.comments[id], text)
	return nil
}

func wallConfig(kinds map[string]string) *config.Config {
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{}, PerAgentTokens: true}
	for name, kind := range kinds {
		cfg.ProjectCache[name] = config.ProjectCacheEntry{LandKind: kind}
	}
	return cfg
}

func tenantFarm() *wallDaemon {
	return &wallDaemon{beads: []*beadsapi.BeadDetail{
		{ID: "kd-1", Type: "project", Title: "tomato-patch", Status: "open", Fields: map[string]string{"land_kind": "tenant", "account": "lands-tomato-patch"}},
		{ID: "kd-2", Type: "project", Title: "gardenio", Status: "open", Fields: map[string]string{}},
	}}
}

func TestSpawnWallRefusal_CachedOperatorLandReadsNothing(t *testing.T) {
	d := tenantFarm()
	d.err = errors.New("daemon down")
	cfg := wallConfig(map[string]string{"gardenio": "", "kbeads": "operator"})
	for _, project := range []string{"gardenio", "kbeads", ""} {
		if r := spawnWallRefusal(context.Background(), cfg, d, project, "lands-gardener", time.Now()); r.Refused() {
			t.Errorf("%q refused: %+v", project, r)
		}
	}
	if d.reads != 0 {
		t.Errorf("an operator land must not cost a daemon read, got %d", d.reads)
	}
}

func TestSpawnWallRefusal_TenantLandReadLive(t *testing.T) {
	d := tenantFarm()
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant"})
	r := spawnWallRefusal(context.Background(), cfg, d, "tomato-patch", "lands-gardener", time.Now())
	if r.Code != wall.CodeTenantSpawnsOff {
		t.Errorf("tenant spawns default off: got %+v", r)
	}
	// The kill switch is read live, never from the cache.
	d.beads[0].Fields["land_frozen"] = "true"
	cfg.LandsTenantSpawns = true
	if r := spawnWallRefusal(context.Background(), cfg, d, "tomato-patch", "lands-gardener", time.Now()); r.Code != wall.CodeFrozen {
		t.Errorf("frozen land: got %+v", r)
	}
}

// The all-lands kill switch (step 10.5) at the pod doors: an open lands-pause
// gate refuses every tenant spawn, a gate that cannot be read refuses them
// too, and operator lands never read it.
func TestSpawnWallRefusal_LandsPauseGateFailsClosed(t *testing.T) {
	d := tenantFarm()
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant", "gardenio": ""})
	cfg.LandsTenantSpawns = true
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: "kd-9", Type: "gate", Title: "lands-pause", Status: "open"})
	if r := spawnWallRefusal(context.Background(), cfg, d, "tomato-patch", "lands-gardener", time.Now()); r.Code != wall.CodeLandsPaused {
		t.Errorf("open gate: got %+v, want %s", r, wall.CodeLandsPaused)
	}
	d.beads = d.beads[:len(d.beads)-1]
	d.gateErr = errors.New("daemon timeout")
	if r := spawnWallRefusal(context.Background(), cfg, d, "tomato-patch", "lands-gardener", time.Now()); r.Code != wall.CodeLandsPaused {
		t.Errorf("unreadable gate: got %+v, want %s", r, wall.CodeLandsPaused)
	}
	if r := spawnWallRefusal(context.Background(), cfg, d, "gardenio", "lands-gardener", time.Now()); r.Refused() {
		t.Errorf("operator land refused by an unreadable lands-pause gate: %+v", r)
	}
}

func TestSpawnWallRefusal_UnreadableFailsClosed(t *testing.T) {
	d := tenantFarm()
	d.err = errors.New("daemon down")
	// Cached as a tenant land, or not cached at all: not known to be the
	// operator's, so an unreadable land is refused.
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant"})
	for _, project := range []string{"tomato-patch", "brand-new"} {
		if r := spawnWallRefusal(context.Background(), cfg, d, project, "lands-gardener", time.Now()); r.Code != wall.CodePurseUnknown {
			t.Errorf("%q: got %+v, want %s", project, r, wall.CodePurseUnknown)
		}
	}
	// Not cached, but the live read says it is no project at all: today's
	// behaviour (other guards decide).
	d.err = nil
	if r := spawnWallRefusal(context.Background(), cfg, d, "brand-new", "lands-gardener", time.Now()); r.Refused() {
		t.Errorf("no such project: refused %+v", r)
	}
}

// With tenant spawns off (the private beta, owner ruling gd-KOAoUELMWo) the
// land's steward still starts at both pod doors, past the fence, cap and rate
// checks; its gardeners and any other role are refused tenant_spawns_off.
func TestSpawnAdmission_StewardPassesTenantSpawnsOff(t *testing.T) {
	now := time.Now()
	d := fundedTenantFarm(now)
	cfg := wallConfig(nil)
	cfg.ProjectCache["tomato-patch"] = config.ProjectCacheEntry{LandKind: "tenant", Namespace: "land-tomato-patch"}
	cfg.LandsTenantSpawns = false
	limit := spawnguard.NewKeyed(10, time.Hour)
	ctx := context.Background()
	hook := reconcilerSpawnWall(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), d, limit)

	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "steward", "kd-st1", now); r.Refused() || !r.Tenant {
		t.Fatalf("the steward with tenant spawns off = %+v, want admitted", r)
	}
	if r := hook.Check(ctx, "tomato-patch", "steward"); r.Refused() || !r.Tenant {
		t.Fatalf("reconciler door, steward = %+v, want admitted", r)
	}
	for _, role := range []string{"lands-gardener", "crew", "gardener", "crew,steward"} {
		if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", role, "kd-x", now); r.Code != wall.CodeTenantSpawnsOff {
			t.Errorf("role %q with tenant spawns off = %+v, want %s", role, r, wall.CodeTenantSpawnsOff)
		}
		if r := hook.Check(ctx, "tomato-patch", role); r.Code != wall.CodeTenantSpawnsOff {
			t.Errorf("reconciler door, role %q = %+v, want %s", role, r, wall.CodeTenantSpawnsOff)
		}
	}
	// Every other refusal still stops the steward: the land fence, a
	// frozen land, the lands-pause gate and the purse wall.
	delete(cfg.ProjectCache, "tomato-patch")
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "steward", "kd-st1", now); r.Code != wall.CodeUnfenced {
		t.Errorf("unfenced land, steward = %+v, want %s", r, wall.CodeUnfenced)
	}
	cfg.ProjectCache["tomato-patch"] = config.ProjectCacheEntry{LandKind: "tenant", Namespace: "land-tomato-patch"}
	d.beads[0].Fields["land_frozen"] = "true"
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "steward", "kd-st1", now); r.Code != wall.CodeFrozen {
		t.Errorf("frozen land, steward = %+v, want %s", r, wall.CodeFrozen)
	}
	d.beads[0].Fields["land_frozen"] = ""
	d.gateErr = errors.New("daemon timeout")
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "steward", "kd-st1", now); r.Code != wall.CodeLandsPaused {
		t.Errorf("unreadable lands-pause, steward = %+v, want %s", r, wall.CodeLandsPaused)
	}
	d.gateErr = nil
	d.beads[4].Fields["total_cost_usd"] = "64" // 4 credits left, under the margin
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "steward", "kd-st1", now); r.Code != wall.CodePurseWall {
		t.Errorf("purse at the wall, steward = %+v, want %s", r, wall.CodePurseWall)
	}
}

func TestReconcilerSpawnWall_RecordsOnce(t *testing.T) {
	d := tenantFarm()
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant"})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hook := reconcilerSpawnWall(cfg, logger, d, nil)
	r := hook.Check(context.Background(), "tomato-patch", "lands-gardener")
	if !r.Refused() {
		t.Fatal("tenant spawns are off by default")
	}
	bead := beadsapi.AgentBead{ID: "kd-ag1", Project: "tomato-patch", AgentName: "crew-1", Metadata: map[string]string{"agent_state": "spawning"}}
	hook.Record(context.Background(), bead, r)
	if got := d.fields["kd-ag1"]; got["agent_state"] != wall.AgentState || got[wall.FieldCode] != wall.CodeTenantSpawnsOff {
		t.Errorf("fields = %v", got)
	}
	bead.Metadata = d.fields["kd-ag1"]
	hook.Record(context.Background(), bead, r)
	if len(d.comments["kd-ag1"]) != 1 {
		t.Errorf("comments = %d, want 1 across two passes", len(d.comments["kd-ag1"]))
	}
}

// The reconciler's recreate of a running agent's pod asks the land's kill
// switches (gd-Nc7yMpW7U6): a frozen tenant land is refused, a funded one and
// an operator land the cache knows are not, and a project-less bead is not
// asked about.
func TestReconcilerSpawnWall_HaltedHook(t *testing.T) {
	d := tenantFarm()
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant", "gardenio": ""})
	hook := reconcilerSpawnWall(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), d, nil)
	if hook.Halted == nil {
		t.Fatal("the reconciler's recreate door must be wired to the land's kill switches")
	}
	if r := hook.Halted(context.Background(), "tomato-patch"); r.Refused() {
		t.Errorf("a tenant land that is not frozen or paused is not halted (tenant spawns off and the purse are not its concern): %+v", r)
	}
	d.beads[0].Fields["land_frozen"] = "true"
	if r := hook.Halted(context.Background(), "tomato-patch"); r.Code != wall.CodeFrozen {
		t.Errorf("frozen land: got %+v, want %s", r, wall.CodeFrozen)
	}
	d.err = errors.New("daemon down")
	reads := d.reads
	for _, project := range []string{"gardenio", ""} {
		if r := hook.Halted(context.Background(), project); r.Refused() {
			t.Errorf("%q halted: %+v", project, r)
		}
	}
	if d.reads != reads {
		t.Errorf("an operator land or no project must not cost a daemon read, got %d", d.reads-reads)
	}
	if hook := reconcilerSpawnWall(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil); hook.Halted != nil {
		t.Error("with no daemon there is no wall to ask")
	}
}

// fundedTenantFarm is tenantFarm with a funded purse: 100 credits at x1.5,
// $10 spent, synced a minute ago.
func fundedTenantFarm(now time.Time) *wallDaemon {
	d := tenantFarm()
	d.beads = append(d.beads,
		&beadsapi.BeadDetail{ID: "kd-3", Type: "credit_rate", Title: "v1", Status: "closed", Fields: map[string]string{"version": "1", "llm_multiplier": "1.5"}},
		&beadsapi.BeadDetail{ID: "kd-4", Type: "credit_grant", Title: "g", Status: "closed", Labels: []string{"project:tomato-patch"}, Fields: map[string]string{"land": "tomato-patch", "amount_credits": "100"}},
		&beadsapi.BeadDetail{ID: "kd-5", Type: "account", Title: "lands-tomato-patch", Status: "open", Fields: map[string]string{
			"total_cost_usd": "10", purse.FieldSpendSyncedAt: now.Add(-time.Minute).Format(time.RFC3339),
		}},
	)
	return d
}

func TestSpawnAdmission_PerLandSpawnRate(t *testing.T) {
	now := time.Now()
	d := fundedTenantFarm(now)
	cfg := wallConfig(map[string]string{"tomato-patch": "tenant", "gardenio": ""})
	cfg.ProjectCache["tomato-patch"] = config.ProjectCacheEntry{LandKind: "tenant", Namespace: "land-tomato-patch"}
	cfg.LandsTenantSpawns = true
	limit := spawnguard.NewKeyed(1, time.Hour)
	ctx := context.Background()

	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "lands-gardener", "kd-ag1", now); r.Refused() {
		t.Fatalf("first spawn of a funded tenant land refused: %+v", r)
	}
	// The reconciler deciding the same agent again keeps its one slot.
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "lands-gardener", "kd-ag1", now); r.Refused() {
		t.Fatalf("the same agent at the second door refused: %+v", r)
	}
	r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "lands-gardener", "kd-ag2", now)
	if r.Code != wall.CodeSpawnRate {
		t.Fatalf("second agent in the hour = %+v, want %s", r, wall.CodeSpawnRate)
	}
	// Operator lands never meet the rate.
	for i := 0; i < 5; i++ {
		if r := spawnAdmission(ctx, cfg, d, limit, "gardenio", "crew", fmt.Sprintf("kd-op%d", i), now); r.Refused() {
			t.Fatalf("operator land spawn %d refused: %+v", i, r)
		}
	}
	// A wall refusal wins and takes no slot.
	cfg.LandsTenantSpawns = false
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "lands-gardener", "kd-ag3", now); r.Code != wall.CodeTenantSpawnsOff {
		t.Errorf("tenant spawns off = %+v, want %s", r, wall.CodeTenantSpawnsOff)
	}
	if got := limit.Count("tomato-patch"); got != 1 {
		t.Errorf("slots held = %d, want 1", got)
	}
}

func TestReconcilerSpawnWall_RateHook(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hook := reconcilerSpawnWall(wallConfig(nil), logger, tenantFarm(), spawnguard.NewKeyed(1, time.Hour))
	ctx := context.Background()
	if r := hook.Rate(ctx, beadsapi.AgentBead{ID: "kd-ag1", Project: "tomato-patch"}); r.Refused() {
		t.Fatalf("first agent refused: %+v", r)
	}
	if r := hook.Rate(ctx, beadsapi.AgentBead{ID: "kd-ag2", Project: "tomato-patch"}); r.Code != wall.CodeSpawnRate {
		t.Errorf("second agent = %+v, want %s", r, wall.CodeSpawnRate)
	}
}

func TestRecordSpawnWallRefusal_CountsOncePerBeadAndCode(t *testing.T) {
	d := tenantFarm()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := landSpawnRefusals.WithLabelValues("count-land", wall.CodeSpawnRate)
	before := testutil.ToFloat64(c)
	r := wall.Refusal{Code: wall.CodeSpawnRate, Reason: "cap", Tenant: true}
	recordSpawnWallRefusal(context.Background(), logger, d, "count-land", "crew-1", "kd-ag9", map[string]string{"agent_state": "spawning"}, r)
	// The next pass re-decides the bead with its refusal already recorded.
	recordSpawnWallRefusal(context.Background(), logger, d, "count-land", "crew-1", "kd-ag9", d.fields["kd-ag9"], r)
	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Errorf("refusals counted = %v, want 1", got)
	}
}

// The land fence at admission (step 10.2): a tenant land the wall admits is
// refused with land_unfenced until the project cache places its pods.
func TestSpawnAdmission_LandFence(t *testing.T) {
	now := time.Now()
	d := fundedTenantFarm(now)
	ctx := context.Background()

	// The cache does not know the project yet.
	cfg := wallConfig(map[string]string{"gardenio": ""})
	cfg.LandsTenantSpawns = true
	if r := spawnAdmission(ctx, cfg, d, nil, "tomato-patch", "lands-gardener", "kd-ag1", now); r.Code != wall.CodeUnfenced || !r.Tenant {
		t.Fatalf("uncached tenant land = %+v, want %s", r, wall.CodeUnfenced)
	}
	// Known as a tenant land but with no namespace.
	cfg.ProjectCache["tomato-patch"] = config.ProjectCacheEntry{LandKind: "tenant"}
	if r := spawnAdmission(ctx, cfg, d, nil, "tomato-patch", "lands-gardener", "kd-ag1", now); r.Code != wall.CodeUnfenced {
		t.Fatalf("tenant land with no namespace = %+v, want %s", r, wall.CodeUnfenced)
	}
	// A bead naming any namespace but its own was not written by the
	// provisioner: refused, not trusted.
	for _, bad := range []string{"gardenio", "land-other"} {
		cfg.ProjectCache["tomato-patch"] = config.ProjectCacheEntry{LandKind: "tenant", Namespace: bad}
		if r := spawnAdmission(ctx, cfg, d, nil, "tomato-patch", "lands-gardener", "kd-ag1", now); r.Code != wall.CodeUnfenced {
			t.Fatalf("tenant land naming namespace %q = %+v, want %s", bad, r, wall.CodeUnfenced)
		}
	}
	cfg.ProjectCache["tomato-patch"] = config.ProjectCacheEntry{LandKind: "tenant", Namespace: "land-tomato-patch"}
	// With per-agent tokens off a tenant pod would have no daemon token
	// (step 10.3): refused.
	cfg.PerAgentTokens = false
	if r := spawnAdmission(ctx, cfg, d, nil, "tomato-patch", "lands-gardener", "kd-ag1", now); r.Code != wall.CodeUnfenced {
		t.Fatalf("tenant land with per-agent tokens off = %+v, want %s", r, wall.CodeUnfenced)
	}
	cfg.PerAgentTokens = true
	if r := spawnAdmission(ctx, cfg, d, nil, "tomato-patch", "lands-gardener", "kd-ag1", now); r.Refused() {
		t.Fatalf("fenced tenant land refused: %+v", r)
	}
	// Operator lands never meet the fence check.
	if r := spawnAdmission(ctx, cfg, d, nil, "gardenio", "crew", "kd-op1", now); r.Refused() {
		t.Fatalf("operator land refused: %+v", r)
	}

	// The reconciler's door refuses the same way.
	hook := reconcilerSpawnWall(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), d, nil)
	if r := hook.Check(ctx, "tomato-patch", "lands-gardener"); r.Refused() {
		t.Fatalf("reconciler refused a fenced tenant land: %+v", r)
	}
	delete(cfg.ProjectCache, "tomato-patch")
	if r := hook.Check(ctx, "tomato-patch", "lands-gardener"); r.Code != wall.CodeUnfenced {
		t.Fatalf("reconciler on an uncached tenant land = %+v, want %s", r, wall.CodeUnfenced)
	}
}

// The land's live-agent cap at both pod doors (step 9.5): with
// max_live_agents 2 and the steward's slot held, one gardener runs; a second
// is refused with max_live_agents and takes no slot of the hour; the steward
// itself is never refused.
func TestSpawnAdmission_MaxLiveAgents(t *testing.T) {
	now := time.Now()
	d := fundedTenantFarm(now)
	d.beads[0].Fields[wall.FieldMaxLiveAgents] = "2"
	d.beads = append(d.beads, &beadsapi.BeadDetail{
		ID: "kd-live1", Type: "agent", Status: "open", Labels: []string{"project:tomato-patch"},
		Fields: map[string]string{"project": "tomato-patch", "role": "lands-gardener", "agent_state": "working"},
	})
	cfg := wallConfig(nil)
	cfg.ProjectCache["tomato-patch"] = config.ProjectCacheEntry{LandKind: "tenant", Namespace: "land-tomato-patch"}
	cfg.LandsTenantSpawns = true
	limit := spawnguard.NewKeyed(10, time.Hour)
	ctx := context.Background()

	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "lands-gardener", "kd-ag2", now); r.Code != wall.CodeMaxLiveAgents || !r.Tenant {
		t.Fatalf("second gardener on a land of 2 = %+v, want %s", r, wall.CodeMaxLiveAgents)
	}
	if got := limit.Count("tomato-patch"); got != 0 {
		t.Errorf("a cap refusal took %d slot(s) of the hour, want 0", got)
	}
	// The live gardener re-decided does not count against itself.
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "lands-gardener", "kd-live1", now); r.Refused() {
		t.Errorf("the live gardener re-decided = %+v, want admitted", r)
	}
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "steward", "kd-st1", now); r.Refused() {
		t.Errorf("the steward on a full land = %+v, want admitted", r)
	}
	hook := reconcilerSpawnWall(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), d, limit)
	if r := hook.Rate(ctx, beadsapi.AgentBead{ID: "kd-ag2", Project: "tomato-patch", Role: "lands-gardener"}); r.Code != wall.CodeMaxLiveAgents {
		t.Errorf("reconciler door = %+v, want %s", r, wall.CodeMaxLiveAgents)
	}
	// The gardener finishes: the slot frees.
	d.beads[len(d.beads)-1].Fields["agent_state"] = "done"
	if r := spawnAdmission(ctx, cfg, d, limit, "tomato-patch", "lands-gardener", "kd-ag2", now); r.Refused() {
		t.Errorf("after the gardener finished = %+v, want admitted", r)
	}
}
