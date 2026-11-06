// gd-CZnK7jeKSG: a schedule window whose fire filed its task bead but never
// spawned an agent must be re-fired, not dropped as "already fired". The
// fixtures replay the herder trigger gd-AHklx9rH70 on 2026-10-31: cron
// `41 * * * *`, last fire recorded for 17:41 (last_run 17:48:25Z), and the
// 18:41 window's task bead filed at 18:47:17Z with no agent behind it.

package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const orphanWindowKey = "2026-10-31T18:41"

// herderTriggerBead is the herder trigger as it stood at 18:57Z.
func herderTriggerBead() *beadsapi.BeadDetail {
	return scheduleTriggerBead("t1", "41 * * * *", map[string]string{
		"action":         "spawn_agent",
		"role":           "herder",
		"project":        "gardenio",
		"prompt":         "tend wishes",
		"max_concurrent": "1",
		"last_run":       "2026-10-31T17:48:25Z",
		"last_match":     "2026-10-31T17:41",
	})
}

// orphanTaskBead is the task an 18:41 fire filed before its spawn was lost.
func orphanTaskBead(status string, createdAt time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        "gd-orphan",
		Type:      "task",
		Status:    status,
		Labels:    []string{"source:schedule", "trigger:t1", "project:gardenio"},
		Fields:    map[string]string{"schedule_fire_key": orphanWindowKey, "trigger_id": "t1"},
		CreatedAt: createdAt,
	}
}

// newOrphanEngine wires a schedule-source engine on daemon whose engine and
// source clocks both read *now.
func newOrphanEngine(daemon *mockTriggerDaemon, now *time.Time) *TriggerEngine {
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
		Clock:  func() time.Time { return *now },
	})
	source := NewScheduleTriggerSource(slog.Default())
	source.nowFunc = func() time.Time { return *now }
	engine.RegisterSource(source)
	return engine
}

// lastOutcome returns the newest last_outcome the engine stamped on id.
func lastOutcome(daemon *mockTriggerDaemon, id string) string {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	out := ""
	for i, uid := range daemon.updatedIDs {
		if uid == id {
			if v, ok := daemon.updatedFields[i]["last_outcome"]; ok {
				out = v
			}
		}
	}
	return out
}

// fireErrorStamps counts the last_fire_error writes on id: one per failed fire.
func fireErrorStamps(daemon *mockTriggerDaemon, id string) int {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	n := 0
	for i, uid := range daemon.updatedIDs {
		if uid == id && daemon.updatedFields[i]["last_fire_error"] != "" {
			n++
		}
	}
	return n
}

// The incident: the pre-fire dedup refresh finds the 18:41 task and, before
// this fix, suppressed the window ("all matches deduped by persisted state").
// The fire must instead spawn onto the orphan task, file no second task, and
// say so in last_outcome.
func TestOrphanFire_TaskWithoutAgentRefiresOntoThatTask(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 57, 5, 0, time.UTC)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
		herderTriggerBead(),
		orphanTaskBead("open", time.Date(2026, 10, 31, 18, 47, 17, 0, time.UTC)),
	}}
	engine := newOrphanEngine(daemon, &now)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawned, tasks, created := len(daemon.spawnedNames), daemon.spawnedTaskIDs, len(daemon.createdBeads)
	daemon.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("spawns = %d, want 1: the 18:41 window was swallowed (outcome %q)", spawned, lastOutcome(daemon, "t1"))
	}
	if tasks[0] != "gd-orphan" {
		t.Errorf("spawned onto task %q, want the orphan task gd-orphan", tasks[0])
	}
	if created != 0 {
		t.Errorf("created %d beads, want 0 (the orphan task is reused)", created)
	}
	out := lastOutcome(daemon, "t1")
	if !strings.Contains(out, "fired: spawned 1") || !strings.Contains(out, "re-fired "+orphanWindowKey+": task gd-orphan had no spawned agent") {
		t.Errorf("last_outcome = %q, want the fire and the re-fired orphan named", out)
	}
	if !engine.SeenKeys("t1")[orphanWindowKey] {
		t.Error("re-fired window not marked seen after a successful spawn")
	}
}

// After a restart, catch-up seeds the 18:41 key from the task bead, so the
// match is dropped at the seen-set filter ("all 1 match(es) deduped by
// seen-set", the outcome the herder showed at 18:57Z). That path must re-fire
// too.
func TestOrphanFire_RefiresKeySeededByCatchUp(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 57, 5, 0, time.UTC)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
		herderTriggerBead(),
		orphanTaskBead("open", time.Date(2026, 10, 31, 18, 47, 17, 0, time.UTC)),
	}}
	engine := newOrphanEngine(daemon, &now)

	engine.CatchUp(context.Background())
	if !engine.SeenKeys("t1")[orphanWindowKey] {
		t.Fatal("precondition: catch-up should seed the orphan key into seen")
	}
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 || daemon.spawnedTaskIDs[0] != "gd-orphan" {
		t.Errorf("spawns = %v onto %v, want 1 onto gd-orphan", daemon.spawnedNames, daemon.spawnedTaskIDs)
	}
}

// The reaper closes a never-claimed run task. Spawning onto a closed task
// would hand the agent dead work, so the re-fire files a fresh task.
func TestOrphanFire_ReapedOrphanTaskGetsFreshTask(t *testing.T) {
	now := time.Date(2026, 10, 31, 19, 10, 0, 0, time.UTC)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
		herderTriggerBead(),
		orphanTaskBead("closed", time.Date(2026, 10, 31, 18, 47, 17, 0, time.UTC)),
	}}
	engine := newOrphanEngine(daemon, &now)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("spawns = %d, want 1", len(daemon.spawnedNames))
	}
	if daemon.spawnedTaskIDs[0] == "gd-orphan" {
		t.Error("spawned onto the reaped (closed) orphan task, want a fresh task")
	}
	if len(daemon.createdBeads) != 1 {
		t.Errorf("created %d beads, want 1 fresh task", len(daemon.createdBeads))
	}
}

// A task filed moments ago may belong to a fire still in flight: wait out the
// backoff, keep the window deduped, and name the wait in last_outcome.
func TestOrphanFire_FreshOrphanWaitsBackoff(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 49, 0, 0, time.UTC)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
		herderTriggerBead(),
		orphanTaskBead("open", time.Date(2026, 10, 31, 18, 47, 17, 0, time.UTC)),
	}}
	engine := newOrphanEngine(daemon, &now)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawned := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if spawned != 0 {
		t.Fatalf("spawns = %d, want 0 inside the backoff", spawned)
	}
	out := lastOutcome(daemon, "t1")
	if !strings.Contains(out, orphanWindowKey+": task gd-orphan has no spawned agent; re-fire at 18:52:17Z") {
		t.Errorf("last_outcome = %q, want the held orphan named with its re-fire time", out)
	}

	now = now.Add(4 * time.Minute)
	engine.Evaluate(context.Background())
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("spawns = %d after the backoff, want 1", len(daemon.spawnedNames))
	}
}

// When an agent was spawned for the key the window really fired, whatever the
// seen set says: no re-fire.
func TestOrphanFire_SpawnedAgentKeepsWindowDeduped(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 57, 5, 0, time.UTC)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
		herderTriggerBead(),
		orphanTaskBead("open", time.Date(2026, 10, 31, 18, 47, 17, 0, time.UTC)),
		{
			ID:     "gd-agent-1841",
			Type:   "agent",
			Status: "closed",
			Fields: map[string]string{"spawned_by": "trigger:t1", "schedule_key": orphanWindowKey},
		},
	}}
	engine := newOrphanEngine(daemon, &now)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 || len(daemon.createdBeads) != 0 {
		t.Errorf("spawns = %d, created = %d; want 0 and 0 when the window's agent exists",
			len(daemon.spawnedNames), len(daemon.createdBeads))
	}
}

// A claimed task means someone is on the window, even without a trigger
// agent: leave it alone.
func TestOrphanFire_ClaimedTaskKeepsWindowDeduped(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 57, 5, 0, time.UTC)
	task := orphanTaskBead("in_progress", time.Date(2026, 10, 31, 18, 47, 17, 0, time.UTC))
	task.Assignee = "someone"
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{herderTriggerBead(), task}}
	engine := newOrphanEngine(daemon, &now)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("spawns = %d, want 0 for a claimed task", len(daemon.spawnedNames))
	}
}

// The 04:41 shape: the spawn keeps being denied. The engine retries on the
// backoff, not every cycle, and never files a second task for the window. The
// denial files one trigger_reseal escalation for the streak (gd-c4Xz4i95F7),
// not one per re-fire.
func TestOrphanFire_FailingSpawnRetriesOnBackoffWithoutNewTasks(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 57, 5, 0, time.UTC)
	trigger := herderTriggerBead()
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			trigger,
			orphanTaskBead("open", time.Date(2026, 10, 31, 18, 47, 17, 0, time.UTC)),
		},
		spawnErr: errors.New("creating bead: HTTP 403: spawn denied: role herder is privileged"),
	}
	engine := newOrphanEngine(daemon, &now)

	engine.Evaluate(context.Background())
	if got := fireErrorStamps(daemon, "t1"); got != 1 {
		t.Fatalf("fire attempts = %d after the first cycle, want 1", got)
	}
	// The daemon persists the stamp; the mock does not, so carry it onto the
	// trigger bead as the next cycle's read would see it.
	stamped, _ := fieldValueFor(daemon, "t1", "last_fire_error")
	daemon.mu.Lock()
	trigger.Fields["last_fire_error"] = stamped
	daemon.mu.Unlock()

	now = now.Add(30 * time.Second)
	engine.Evaluate(context.Background())
	if got := fireErrorStamps(daemon, "t1"); got != 1 {
		t.Errorf("fire attempts = %d 30s later, want still 1 (inside the backoff)", got)
	}
	if out := lastOutcome(daemon, "t1"); !strings.Contains(out, "has no spawned agent; re-fire at 19:02:05Z") {
		t.Errorf("last_outcome = %q, want the held re-fire named", out)
	}

	now = now.Add(5 * time.Minute)
	engine.Evaluate(context.Background())
	if got := fireErrorStamps(daemon, "t1"); got != 2 {
		t.Errorf("fire attempts = %d after the backoff, want 2", got)
	}

	if escs := spawnDeniedEscalations(daemon); len(escs) != 1 {
		t.Errorf("filed %d spawn-denied escalations across failing re-fires, want 1 for the streak", len(escs))
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, req := range daemon.createdBeads {
		if req.Type != "escalation" {
			t.Errorf("created a %s bead %q across failing re-fires, want no new task for the window", req.Type, req.Title)
		}
	}
}
