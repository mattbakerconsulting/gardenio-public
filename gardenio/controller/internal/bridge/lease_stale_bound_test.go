package bridge

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Bug ka-0000001095: a single hung trigger-spawned agent must NOT hold its
// max_concurrent lease (or the one_spawn_at_a_time gardener gate) forever. These
// tests cover both the under-threshold side (fresh agent still holds the lease —
// the ka-0000003120 regression guard) and the over-threshold side (a stale agent
// is released so the lane resumes firing), for both engine paths.

// leaseTestClock is a fixed instant the tests anchor staleness math on.
var leaseTestNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// leaseEngine builds a TriggerEngine with a fixed clock and a mock daemon seeded
// with the given beads, plus a spawn_agent source that yields one match for t1.
func leaseEngine(t *testing.T, beads ...*beadsapi.BeadDetail) (*TriggerEngine, *mockTriggerDaemon) {
	t.Helper()
	daemon := &mockTriggerDaemon{beads: beads}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
		Clock:  func() time.Time { return leaseTestNow },
	})
	return engine, daemon
}

// leaseTrigger builds a max_concurrent=1 spawn trigger for id t1 with a single
// pending match, optionally with a per-trigger lease_stale_hours override.
func leaseSource(leaseStaleHours string) *mockTriggerSource {
	trig := TriggerDef{
		ID: "t1", Title: "Single-threaded", Source: "test",
		Action: TriggerActionSpawnAgent, Role: "crew", Project: "myproj",
		Prompt: "do work", MaxConcurrent: 1, LastChecked: pastLastChecked(),
	}
	if leaseStaleHours != "" {
		trig.LeaseStaleAfter = leaseStaleHoursField(map[string]string{"lease_stale_hours": leaseStaleHours})
	}
	return &mockTriggerSource{
		name:     "test",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{"t1": {{Key: "M1", Title: "Match 1"}}},
	}
}

func leaseAgent(id string, updatedAgo time.Duration) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID: id, Type: "agent", Status: "in_progress",
		Fields: map[string]string{"spawned_by": "trigger:t1", "agent": id + "-name"},
	}
	if updatedAgo >= 0 {
		b.UpdatedAt = leaseTestNow.Add(-updatedAgo)
	}
	return b
}

// leaseAgentAges builds a holder with independent updated_at and created_at ages
// (negative ⇒ leave that timestamp zero). The livelock case sets updatedAgo
// small (churn keeps it fresh) but createdAgo large (the bead was minted long
// ago) — the exact ka-0000002023 shape the updated_at bound alone cannot catch.
func leaseAgentAges(id string, updatedAgo, createdAgo time.Duration) *beadsapi.BeadDetail {
	b := leaseAgent(id, updatedAgo)
	if createdAgo >= 0 {
		b.CreatedAt = leaseTestNow.Add(-createdAgo)
	}
	return b
}

// --- liveSpawnCount (max_concurrent lease) ---

// Over-threshold: the only live agent is stale past the 3h default, so the lease
// is released and the max_concurrent=1 trigger fires again.
func TestLease_StaleHolderReleased_FiresAgain(t *testing.T) {
	engine, daemon := leaseEngine(t, leaseAgent("gd-hung", 8*time.Hour)) // 8h > 3h
	engine.RegisterSource(leaseSource(""))
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn (stale holder released), got %d", len(daemon.spawnedNames))
	}
	// Observable: a one-shot comment on the trigger naming the stale agent.
	if !slices.Contains(daemon.commentedIDs, "t1") {
		t.Errorf("expected a lease-stale comment on t1, got commentedIDs=%v", daemon.commentedIDs)
	}
}

// Under-threshold: the live agent is fresh (well under 3h), so the lease holds
// and the trigger does NOT double-fire — the ka-0000003120 regression guard.
func TestLease_FreshHolderStillBlocks(t *testing.T) {
	engine, daemon := leaseEngine(t, leaseAgent("gd-working", 90*time.Minute)) // 90m < 3h
	engine.RegisterSource(leaseSource(""))
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("expected 0 spawns (fresh holder holds the lease), got %d", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.commentedIDs, "t1") {
		t.Errorf("fresh holder must not emit a lease-stale comment, got %v", daemon.commentedIDs)
	}
}

// Boundary: a legitimately-long run just under 3h (matching the observed 106-124m
// cluster with margin) still holds the lease; the 469m/547m outages would not.
func TestLease_JustUnderThreshold_StillBlocks(t *testing.T) {
	engine, daemon := leaseEngine(t, leaseAgent("gd-longrun", 124*time.Minute)) // top of the legit cluster
	engine.RegisterSource(leaseSource(""))
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("expected 0 spawns (124m < 3h legit long run), got %d", len(daemon.spawnedNames))
	}
}

// A zero/absent updated_at can't prove inactivity, so it must NOT be released
// (fail toward keeping the lease held). Regression guard for the existing
// ka-0000003120 fixtures, which seed agents with no UpdatedAt.
func TestLease_ZeroUpdatedAt_NotStale(t *testing.T) {
	engine, daemon := leaseEngine(t, leaseAgent("gd-notimestamp", -1)) // -1 ⇒ leave UpdatedAt zero
	engine.RegisterSource(leaseSource(""))
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("expected 0 spawns (zero updated_at is not stale), got %d", len(daemon.spawnedNames))
	}
}

// Per-trigger override: with lease_stale_hours=1, an agent idle 90m is now past
// the (1h) bound and released, even though it would hold under the 3h default.
func TestLease_PerTriggerOverride_Releases(t *testing.T) {
	engine, daemon := leaseEngine(t, leaseAgent("gd-idle", 90*time.Minute))
	engine.RegisterSource(leaseSource("1")) // 1h override < 90m
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn (90m > 1h override), got %d", len(daemon.spawnedNames))
	}
}

// The stale-holder comment is one-shot: two Evaluate cycles with the same stale
// agent produce exactly one comment on the trigger.
func TestLease_StaleComment_OneShot(t *testing.T) {
	engine, daemon := leaseEngine(t, leaseAgent("gd-hung", 8*time.Hour))
	engine.RegisterSource(leaseSource(""))
	engine.Evaluate(context.Background())
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	n := 0
	for _, id := range daemon.commentedIDs {
		if id == "t1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 one-shot lease-stale comment across 2 cycles, got %d", n)
	}
}

// --- gardenerAgentStillRunning (one_spawn_at_a_time gate) ---

// gardenerStaleDaemon returns a mock whose GetBead yields one agent with a chosen
// status and updated_at.
func gardenerStaleDaemon(agentID, status string, updatedAgo time.Duration) *mockTriggerDaemon {
	b := &beadsapi.BeadDetail{ID: agentID, Type: "agent", Status: status,
		Fields: map[string]string{"agent": agentID + "-name"}}
	if updatedAgo >= 0 {
		b.UpdatedAt = leaseTestNow.Add(-updatedAgo)
	}
	return &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{b}}
}

func TestGardenerAgentStillRunning_FreshBlocks(t *testing.T) {
	d := gardenerStaleDaemon("gd-a", "in_progress", 90*time.Minute) // < 3h
	if !gardenerAgentStillRunning(context.Background(), d, slog.Default(), "gd-a", 0, leaseTestNow) {
		t.Fatalf("fresh in_progress agent (90m) must count as still-running")
	}
}

func TestGardenerAgentStillRunning_StaleReleased(t *testing.T) {
	d := gardenerStaleDaemon("gd-a", "in_progress", 8*time.Hour) // > 3h
	if gardenerAgentStillRunning(context.Background(), d, slog.Default(), "gd-a", 0, leaseTestNow) {
		t.Fatalf("stale in_progress agent (8h > 3h) must be released so the gardener resumes")
	}
}

func TestGardenerAgentStillRunning_ZeroUpdatedAtBlocks(t *testing.T) {
	d := gardenerStaleDaemon("gd-a", "in_progress", -1) // zero updated_at
	if !gardenerAgentStillRunning(context.Background(), d, slog.Default(), "gd-a", 0, leaseTestNow) {
		t.Fatalf("zero updated_at is not provably stale — must keep the gate held")
	}
}

func TestGardenerAgentStillRunning_OverrideReleases(t *testing.T) {
	d := gardenerStaleDaemon("gd-a", "in_progress", 90*time.Minute)
	// 1h override ⇒ 90m is stale.
	if gardenerAgentStillRunning(context.Background(), d, slog.Default(), "gd-a", time.Hour, leaseTestNow) {
		t.Fatalf("90m > 1h override must release the gate")
	}
}

func TestGardenerAgentStillRunning_ClosedNotRunning(t *testing.T) {
	// A terminal status is not running regardless of freshness (unchanged behavior).
	d := gardenerStaleDaemon("gd-a", "closed", 1*time.Minute)
	if gardenerAgentStillRunning(context.Background(), d, slog.Default(), "gd-a", 0, leaseTestNow) {
		t.Fatalf("closed agent must not count as still-running")
	}
}

// --- field parsing ---

func TestLeaseStaleHoursField(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"3", 3 * time.Hour},
		{"1.5", 90 * time.Minute},
		{"0", 0},
		{"-2", 0},
		{"abc", 0},
		{"Inf", 0},
	}
	for _, c := range cases {
		got := leaseStaleHoursField(map[string]string{"lease_stale_hours": c.in})
		if got != c.want {
			t.Errorf("leaseStaleHoursField(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// Default fallback via leaseStaleThreshold.
	if got := leaseStaleThreshold(TriggerDef{}); got != defaultLeaseStaleAfter {
		t.Errorf("leaseStaleThreshold(zero) = %v, want %v", got, defaultLeaseStaleAfter)
	}
	if got := leaseStaleThreshold(TriggerDef{LeaseStaleAfter: time.Hour}); got != time.Hour {
		t.Errorf("leaseStaleThreshold(1h override) = %v, want 1h", got)
	}
}

// ParseTriggerDef wires lease_stale_hours through to LeaseStaleAfter.
func TestParseTriggerDef_LeaseStaleHours(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID: "t1", Type: "trigger", Status: "open",
		Fields: map[string]string{"action": "spawn_agent", "lease_stale_hours": "4"},
	}
	trig := ParseTriggerDef(bead, "test")
	if trig.LeaseStaleAfter != 4*time.Hour {
		t.Errorf("ParseTriggerDef lease_stale_hours=4 → LeaseStaleAfter=%v, want 4h", trig.LeaseStaleAfter)
	}
}

// --- created_at lease age ceiling (bug ka-0000002023) ---

// THE bug: a LIVELOCKED holder keeps updated_at fresh by churning (re-boot every
// ~10.5m for 15h), so the updated_at bound never releases it and the
// max_concurrent=1 lane stays dark. The created_at ceiling (2× the 3h default =
// 6h) is immune to that churn and releases it, so the trigger fires again.
func TestLease_LivelockedHolderReleasedByAgeCeiling_FiresAgain(t *testing.T) {
	// updated_at fresh (2m — the churn) but created_at 15h ago (> 6h ceiling).
	engine, daemon := leaseEngine(t, leaseAgentAges("gd-shnzqcgxdw", 2*time.Minute, 15*time.Hour))
	engine.RegisterSource(leaseSource(""))
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn (livelocked holder released by age ceiling), got %d", len(daemon.spawnedNames))
	}
	// Observable: the one-shot lease-release comment naming the holder.
	if !slices.Contains(daemon.commentedIDs, "t1") {
		t.Errorf("expected a lease-release comment on t1, got commentedIDs=%v", daemon.commentedIDs)
	}
}

// A fresh, YOUNG holder (both updated_at and created_at recent) is a genuinely
// live run and must still hold the lease — the ceiling must not double-spawn on
// top of a legitimate long run under 6h. Regression guard against over-release.
func TestLease_YoungFreshHolder_StillBlocks(t *testing.T) {
	// updated_at 2m, created_at 2h (well under the 6h ceiling AND the 3h stale bound).
	engine, daemon := leaseEngine(t, leaseAgentAges("gd-young", 2*time.Minute, 2*time.Hour))
	engine.RegisterSource(leaseSource(""))
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("expected 0 spawns (young fresh holder holds the lease), got %d", len(daemon.spawnedNames))
	}
}

// Boundary: a fresh holder created just under the 6h ceiling (5h) still holds;
// just over (7h) is released. Pins the ceiling exactly at 2× the 3h default.
func TestLease_AgeCeilingBoundary(t *testing.T) {
	// Just under: 5h < 6h ceiling, updated_at fresh ⇒ holds.
	{
		engine, daemon := leaseEngine(t, leaseAgentAges("gd-5h", 2*time.Minute, 5*time.Hour))
		engine.RegisterSource(leaseSource(""))
		engine.Evaluate(context.Background())
		daemon.mu.Lock()
		if len(daemon.spawnedNames) != 0 {
			daemon.mu.Unlock()
			t.Fatalf("5h < 6h ceiling must hold, got %d spawns", len(daemon.spawnedNames))
		}
		daemon.mu.Unlock()
	}
	// Just over: 7h > 6h ceiling, updated_at fresh ⇒ released.
	{
		engine, daemon := leaseEngine(t, leaseAgentAges("gd-7h", 2*time.Minute, 7*time.Hour))
		engine.RegisterSource(leaseSource(""))
		engine.Evaluate(context.Background())
		daemon.mu.Lock()
		if len(daemon.spawnedNames) != 1 {
			daemon.mu.Unlock()
			t.Fatalf("7h > 6h ceiling must release, got %d spawns", len(daemon.spawnedNames))
		}
		daemon.mu.Unlock()
	}
}

// A zero/absent created_at can't prove age, so the ceiling must NOT release on
// it (fail toward keeping the lease held) — symmetric with the zero-updated_at
// guard. A fresh-updated_at holder with no created_at still blocks.
func TestLease_ZeroCreatedAt_CeilingNotApplied(t *testing.T) {
	// updated_at fresh (2m), created_at zero ⇒ neither bound releases ⇒ holds.
	engine, daemon := leaseEngine(t, leaseAgentAges("gd-nocreate", 2*time.Minute, -1))
	engine.RegisterSource(leaseSource(""))
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("expected 0 spawns (zero created_at not over ceiling), got %d", len(daemon.spawnedNames))
	}
}

// The per-trigger lease_stale_hours override scales BOTH bounds: with a 1h
// override the ceiling is 2h, so a livelocked holder (fresh updated_at) created
// 3h ago is released even though it would hold under the 6h default ceiling.
func TestLease_AgeCeilingScalesWithOverride(t *testing.T) {
	engine, daemon := leaseEngine(t, leaseAgentAges("gd-override", 2*time.Minute, 3*time.Hour))
	engine.RegisterSource(leaseSource("1")) // 1h override ⇒ 2h ceiling < 3h age
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn (3h age > 2h ceiling under 1h override), got %d", len(daemon.spawnedNames))
	}
}

// Direct unit coverage of the helper predicate at the boundary.
func TestLeaseHolderOverAgeCeiling(t *testing.T) {
	now := leaseTestNow
	threshold := 3 * time.Hour // ceiling = 6h
	cases := []struct {
		name       string
		createdAgo time.Duration // <0 ⇒ zero created_at
		want       bool
	}{
		{"zero created_at not over", -1, false},
		{"under ceiling", 5 * time.Hour, false},
		{"exactly at ceiling not over", 6 * time.Hour, false},
		{"over ceiling", 6*time.Hour + time.Minute, true},
		{"well over (the 15h wedge)", 15 * time.Hour, true},
	}
	for _, c := range cases {
		b := &beadsapi.BeadDetail{ID: "gd-x"}
		if c.createdAgo >= 0 {
			b.CreatedAt = now.Add(-c.createdAgo)
		}
		if got := leaseHolderOverAgeCeiling(b, threshold, now); got != c.want {
			t.Errorf("%s: leaseHolderOverAgeCeiling = %v, want %v", c.name, got, c.want)
		}
	}
	// Nil bead is never over the ceiling.
	if leaseHolderOverAgeCeiling(nil, threshold, now) {
		t.Errorf("nil bead must not be over the ceiling")
	}
}
