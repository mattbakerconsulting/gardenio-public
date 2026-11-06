package presence

import (
	"testing"
	"time"
)

func TestRecordHookEvent_BasicTracking(t *testing.T) {
	tr := New()

	tr.RecordHookEvent(HookEvent{
		Actor:     "alice",
		HookType:  "SessionStart",
		SessionID: "sess-1",
		CWD:       "/home/alice/project",
	})

	roster := tr.Roster(0)
	if len(roster) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(roster))
	}

	e := roster[0]
	if e.Actor != "alice" {
		t.Errorf("expected actor alice, got %s", e.Actor)
	}
	if e.LastEvent != "SessionStart" {
		t.Errorf("expected last_event SessionStart, got %s", e.LastEvent)
	}
	if e.SessionID != "sess-1" {
		t.Errorf("expected session_id sess-1, got %s", e.SessionID)
	}
	if e.CWD != "/home/alice/project" {
		t.Errorf("expected cwd /home/alice/project, got %s", e.CWD)
	}
	if e.EventCount != 1 {
		t.Errorf("expected event_count 1, got %d", e.EventCount)
	}
}

func TestRecordHookEvent_UpdatesExistingActor(t *testing.T) {
	tr := New()

	tr.RecordHookEvent(HookEvent{Actor: "bob", HookType: "SessionStart"})
	tr.RecordHookEvent(HookEvent{Actor: "bob", HookType: "PostToolUse", ToolName: "Bash"})
	tr.RecordHookEvent(HookEvent{Actor: "bob", HookType: "PostToolUse", ToolName: "Read"})

	roster := tr.Roster(0)
	if len(roster) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(roster))
	}

	e := roster[0]
	if e.EventCount != 3 {
		t.Errorf("expected 3 events, got %d", e.EventCount)
	}
	if e.ToolName != "Read" {
		t.Errorf("expected last tool Read, got %s", e.ToolName)
	}
	if e.LastEvent != "PostToolUse" {
		t.Errorf("expected last_event PostToolUse, got %s", e.LastEvent)
	}
}

func TestRecordHookEvent_IgnoresEmptyActor(t *testing.T) {
	tr := New()

	tr.RecordHookEvent(HookEvent{Actor: "", HookType: "SessionStart"})

	roster := tr.Roster(0)
	if len(roster) != 0 {
		t.Fatalf("expected 0 entries for empty actor, got %d", len(roster))
	}
}

func TestRoster_StaleThreshold(t *testing.T) {
	tr := New()

	// Record an event, then manually backdate the actor.
	tr.RecordHookEvent(HookEvent{Actor: "old-agent", HookType: "SessionStart"})
	tr.RecordHookEvent(HookEvent{Actor: "new-agent", HookType: "SessionStart"})

	tr.mu.Lock()
	tr.actors["old-agent"].lastSeen = time.Now().Add(-20 * time.Minute)
	tr.mu.Unlock()

	// With 10-minute threshold, only new-agent should appear.
	roster := tr.Roster(10 * time.Minute)
	if len(roster) != 1 {
		t.Fatalf("expected 1 entry with threshold, got %d", len(roster))
	}
	if roster[0].Actor != "new-agent" {
		t.Errorf("expected new-agent, got %s", roster[0].Actor)
	}

	// With 0 threshold, both should appear.
	all := tr.Roster(0)
	if len(all) != 2 {
		t.Fatalf("expected 2 entries without threshold, got %d", len(all))
	}
}

func TestRoster_SortedByMostRecent(t *testing.T) {
	tr := New()

	tr.RecordHookEvent(HookEvent{Actor: "first", HookType: "SessionStart"})
	time.Sleep(5 * time.Millisecond)
	tr.RecordHookEvent(HookEvent{Actor: "second", HookType: "SessionStart"})
	time.Sleep(5 * time.Millisecond)
	tr.RecordHookEvent(HookEvent{Actor: "third", HookType: "SessionStart"})

	roster := tr.Roster(0)
	if len(roster) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(roster))
	}
	if roster[0].Actor != "third" {
		t.Errorf("expected third first, got %s", roster[0].Actor)
	}
	if roster[2].Actor != "first" {
		t.Errorf("expected first last, got %s", roster[2].Actor)
	}
}

func TestSweep_MarksIdleAgentsDead(t *testing.T) {
	tr := New()

	tr.RecordHookEvent(HookEvent{Actor: "idle-agent", HookType: "SessionStart"})

	// Backdate to make it idle.
	tr.mu.Lock()
	tr.actors["idle-agent"].lastSeen = time.Now().Add(-20 * time.Minute)
	tr.mu.Unlock()

	var deadActors []string
	cfg := &ReaperConfig{
		DeadThreshold: 15 * time.Minute,
		EvictAfter:    30 * time.Minute,
		SweepInterval: time.Second,
		OnDead: func(actor, _ string) {
			deadActors = append(deadActors, actor)
		},
	}

	tr.sweep(cfg)

	if len(deadActors) != 1 || deadActors[0] != "idle-agent" {
		t.Errorf("expected idle-agent to be reaped, got %v", deadActors)
	}

	roster := tr.Roster(0)
	for _, e := range roster {
		if e.Actor == "idle-agent" && !e.Reaped {
			t.Error("expected idle-agent to have reaped=true")
		}
	}
}

func TestSweep_ResurrectedAgentNotReaped(t *testing.T) {
	tr := New()

	// Agent was reaped...
	tr.RecordHookEvent(HookEvent{Actor: "zombie", HookType: "SessionStart"})
	tr.mu.Lock()
	tr.actors["zombie"].lastSeen = time.Now().Add(-20 * time.Minute)
	tr.mu.Unlock()

	cfg := &ReaperConfig{DeadThreshold: 15 * time.Minute, EvictAfter: 30 * time.Minute}
	tr.sweep(cfg)

	// ...but comes back to life.
	tr.RecordHookEvent(HookEvent{Actor: "zombie", HookType: "PostToolUse", ToolName: "Bash"})

	roster := tr.Roster(0)
	for _, e := range roster {
		if e.Actor == "zombie" {
			if e.Reaped {
				t.Error("expected zombie to be resurrected (reaped=false)")
			}
			if e.EventCount != 2 {
				t.Errorf("expected 2 events, got %d", e.EventCount)
			}
			return
		}
	}
	t.Error("zombie not found in roster")
}

func TestSweep_EvictsEphemeralAgents(t *testing.T) {
	tr := New()

	// Agent with few events, reaped a while ago.
	tr.RecordHookEvent(HookEvent{Actor: "ephemeral", HookType: "SessionStart"})
	tr.mu.Lock()
	state := tr.actors["ephemeral"]
	state.lastSeen = time.Now().Add(-30 * time.Minute)
	state.reaped = true
	state.reapedAt = time.Now().Add(-10 * time.Minute) // reaped 10 min ago
	state.eventCount = 3                                // low event count
	tr.mu.Unlock()

	cfg := &ReaperConfig{
		DeadThreshold: 15 * time.Minute,
		EvictAfter:    30 * time.Minute, // normally 30 min
	}

	tr.sweep(cfg)

	// Ephemeral agents (<10 events) should be evicted after 5 min.
	tr.mu.RLock()
	_, exists := tr.actors["ephemeral"]
	tr.mu.RUnlock()

	if exists {
		t.Error("expected ephemeral agent to be evicted (low event count, reaped >5 min ago)")
	}
}

func TestStartReaper_StopsCleanly(t *testing.T) {
	tr := New()

	tr.StartReaper(&ReaperConfig{
		SweepInterval: 50 * time.Millisecond,
	})

	// Let it run a couple sweeps.
	time.Sleep(150 * time.Millisecond)

	// Stop should return without hanging.
	done := make(chan struct{})
	go func() {
		tr.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2 seconds")
	}
}

// A land agent's door traffic is its own entry, tagged with the land: it
// never marks a fleet actor of the same name alive, nor tags it with the land.
func TestRecordHookEvent_LandKeptApart(t *testing.T) {
	tr := New()
	tr.RecordHookEvent(HookEvent{Actor: "studio-x7rq", HookType: "PreToolUse", CWD: "/fleet"})
	tr.RecordHookEvent(HookEvent{Actor: "studio-x7rq", HookType: "door", Land: "acme"})
	tr.RecordHookEvent(HookEvent{Actor: "studio-x7rq", HookType: "door", Land: "acme"})

	roster := tr.Roster(0)
	if len(roster) != 2 {
		t.Fatalf("expected a fleet and a land entry, got %+v", roster)
	}
	byLand := map[string]Entry{}
	for _, e := range roster {
		byLand[e.Land] = e
	}
	if e := byLand[""]; e.Actor != "studio-x7rq" || e.EventCount != 1 || e.CWD != "/fleet" {
		t.Errorf("fleet entry = %+v, want its one event untouched", e)
	}
	if e := byLand["acme"]; e.Actor != "studio-x7rq" || e.EventCount != 2 || e.LastEvent != "door" {
		t.Errorf("land entry = %+v, want two door events for acme", e)
	}
}
