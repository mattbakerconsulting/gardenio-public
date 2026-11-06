package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestThreadAgentEntry_SetAndGet(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	if err := sm.SetThreadAgent("C1", "1.1", "agent-a"); err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	agent, ok := sm.GetThreadAgent("C1", "1.1")
	if !ok || agent != "agent-a" {
		t.Fatalf("expected agent-a, got %q ok=%v", agent, ok)
	}

	// Verify timestamp was set.
	sm.mu.RLock()
	entry := sm.data.ThreadAgents[threadAgentKey("C1", "1.1")]
	sm.mu.RUnlock()

	if entry.LastActive.Before(before) || entry.LastActive.After(after) {
		t.Errorf("LastActive %v not in [%v, %v]", entry.LastActive, before, after)
	}
}

func TestThreadAgentEntry_TouchRefreshesTimestamp(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	_ = sm.SetThreadAgent("C1", "1.1", "agent-a")

	sm.mu.RLock()
	original := sm.data.ThreadAgents[threadAgentKey("C1", "1.1")].LastActive
	sm.mu.RUnlock()

	// Touch should refresh the timestamp.
	time.Sleep(time.Millisecond)
	if err := sm.TouchThreadAgent("C1", "1.1"); err != nil {
		t.Fatal(err)
	}

	sm.mu.RLock()
	touched := sm.data.ThreadAgents[threadAgentKey("C1", "1.1")].LastActive
	sm.mu.RUnlock()

	if !touched.After(original) {
		t.Errorf("TouchThreadAgent did not refresh timestamp: original=%v touched=%v",
			original, touched)
	}
}

func TestThreadAgentEntry_TouchNoopForMissing(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Touch a non-existent key should be a no-op.
	if err := sm.TouchThreadAgent("C1", "1.1"); err != nil {
		t.Fatal(err)
	}

	if len(sm.data.ThreadAgents) != 0 {
		t.Errorf("expected empty map, got %d entries", len(sm.data.ThreadAgents))
	}
}

func TestCleanExpiredThreadAgents(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Set two entries: one fresh, one with a backdated timestamp.
	_ = sm.SetThreadAgent("C1", "1.1", "fresh-agent")

	sm.mu.Lock()
	sm.data.ThreadAgents[threadAgentKey("C2", "2.2")] = ThreadAgentEntry{
		Agent:      "stale-agent",
		LastActive: time.Now().Add(-25 * time.Hour), // 25h ago
	}
	// Also set a listen flag for the stale entry.
	sm.data.ListenThreads[threadAgentKey("C2", "2.2")] = true
	sm.mu.Unlock()

	removed, err := sm.CleanExpiredThreadAgents(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	// Fresh entry should survive.
	if agent, ok := sm.GetThreadAgent("C1", "1.1"); !ok || agent != "fresh-agent" {
		t.Errorf("fresh entry missing: agent=%q ok=%v", agent, ok)
	}
	// Stale entry should be gone.
	if _, ok := sm.GetThreadAgent("C2", "2.2"); ok {
		t.Error("stale entry should have been removed")
	}
	// Listen flag for stale entry should also be removed.
	if sm.IsListenThread("C2", "2.2") {
		t.Error("listen flag for stale entry should have been removed")
	}
}

func TestCleanExpiredThreadAgents_ZeroTimestamp(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Entries with zero timestamps (from old state files) should NOT be
	// cleaned up — they lack timing information.
	sm.mu.Lock()
	sm.data.ThreadAgents[threadAgentKey("C1", "1.1")] = ThreadAgentEntry{
		Agent:      "legacy-agent",
		LastActive: time.Time{}, // zero value
	}
	sm.mu.Unlock()

	removed, err := sm.CleanExpiredThreadAgents(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed (zero timestamp preserved), got %d", removed)
	}
	if _, ok := sm.GetThreadAgent("C1", "1.1"); !ok {
		t.Error("zero-timestamp entry should have been preserved")
	}
}

// TestCompactStaleEntries_PreservesAgentCards verifies that agent cards are
// intentionally preserved during compaction (not removed with other stale
// entries) so that findAgentCardByThread can still identify card threads.
func TestCompactStaleEntries_PreservesAgentCards(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Set up: one stale agent card and one stale decision message.
	_ = sm.SetAgentCard("dead-agent", MessageRef{ChannelID: "C1", Timestamp: "1.1"})

	sm.mu.Lock()
	sm.data.DecisionMessages["dec-1"] = MessageRef{ChannelID: "C1", Timestamp: "2.2", Agent: "dead-agent"}
	sm.mu.Unlock()

	activeAgents := map[string]bool{} // no active agents

	removed, err := sm.CompactStaleEntries(activeAgents)
	if err != nil {
		t.Fatal(err)
	}

	// Decision message should be removed.
	if removed != 1 {
		t.Errorf("expected 1 removed (stale decision), got %d", removed)
	}
	sm.mu.RLock()
	if _, ok := sm.data.DecisionMessages["dec-1"]; ok {
		t.Error("stale decision message should have been removed")
	}
	sm.mu.RUnlock()

	// Agent card should be preserved.
	if ref, ok := sm.GetAgentCard("dead-agent"); !ok {
		t.Error("agent card should be preserved during compaction")
	} else if ref.ChannelID != "C1" {
		t.Errorf("unexpected card ref: %+v", ref)
	}
}

func TestCompactStaleEntries_IncludesExpiredThreadAgents(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Set up: one fresh thread agent, one expired.
	_ = sm.SetThreadAgent("C1", "1.1", "active-agent")

	sm.mu.Lock()
	sm.data.ThreadAgents[threadAgentKey("C2", "2.2")] = ThreadAgentEntry{
		Agent:      "expired-agent",
		LastActive: time.Now().Add(-25 * time.Hour),
	}
	sm.data.ListenThreads[threadAgentKey("C2", "2.2")] = true
	sm.mu.Unlock()

	activeAgents := map[string]bool{
		"active-agent":  true,
		"expired-agent": true, // still "active" by agent list but expired by TTL
	}

	removed, err := sm.CompactStaleEntries(activeAgents)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed (expired thread agent), got %d", removed)
	}

	// Fresh entry survives.
	if _, ok := sm.GetThreadAgent("C1", "1.1"); !ok {
		t.Error("fresh thread agent should survive")
	}
	// Expired entry removed even though agent is in activeAgents.
	if _, ok := sm.GetThreadAgent("C2", "2.2"); ok {
		t.Error("expired thread agent should have been removed")
	}
}

func TestThreadAgentEntry_BackwardCompatUnmarshal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Write old-format state with plain string thread_agents values.
	oldState := map[string]interface{}{
		"thread_agents": map[string]string{
			"C1:1.1": "old-agent",
			"C2:2.2": "other-agent",
		},
		"listen_threads": map[string]bool{
			"C1:1.1": true,
		},
	}
	data, _ := json.Marshal(oldState)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Load should succeed with backward-compat unmarshal.
	sm, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}

	agent, ok := sm.GetThreadAgent("C1", "1.1")
	if !ok || agent != "old-agent" {
		t.Errorf("expected old-agent, got %q ok=%v", agent, ok)
	}

	agent, ok = sm.GetThreadAgent("C2", "2.2")
	if !ok || agent != "other-agent" {
		t.Errorf("expected other-agent, got %q ok=%v", agent, ok)
	}

	// Migrated entries should have zero timestamps.
	sm.mu.RLock()
	entry := sm.data.ThreadAgents[threadAgentKey("C1", "1.1")]
	sm.mu.RUnlock()
	if !entry.LastActive.IsZero() {
		t.Errorf("expected zero LastActive for migrated entry, got %v", entry.LastActive)
	}
}

func TestThreadAgentEntry_NewFormatPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Create and save with new format.
	sm1, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = sm1.SetThreadAgent("C1", "1.1", "new-agent")

	// Reload from disk.
	sm2, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}

	agent, ok := sm2.GetThreadAgent("C1", "1.1")
	if !ok || agent != "new-agent" {
		t.Errorf("expected new-agent, got %q ok=%v", agent, ok)
	}

	// Timestamp should survive round-trip.
	sm2.mu.RLock()
	entry := sm2.data.ThreadAgents[threadAgentKey("C1", "1.1")]
	sm2.mu.RUnlock()
	if entry.LastActive.IsZero() {
		t.Error("expected non-zero LastActive after round-trip")
	}
}

func TestRemoveThreadAgentByAgent_WithEntries(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	_ = sm.SetThreadAgent("C1", "1.1", "agent-a")
	_ = sm.SetThreadAgent("C2", "2.2", "agent-a")
	_ = sm.SetThreadAgent("C3", "3.3", "agent-b")
	_ = sm.SetListenThread("C1", "1.1")

	if err := sm.RemoveThreadAgentByAgent("agent-a"); err != nil {
		t.Fatal(err)
	}

	if _, ok := sm.GetThreadAgent("C1", "1.1"); ok {
		t.Error("C1:1.1 should have been removed")
	}
	if _, ok := sm.GetThreadAgent("C2", "2.2"); ok {
		t.Error("C2:2.2 should have been removed")
	}
	if agent, ok := sm.GetThreadAgent("C3", "3.3"); !ok || agent != "agent-b" {
		t.Errorf("agent-b should survive: agent=%q ok=%v", agent, ok)
	}
	if sm.IsListenThread("C1", "1.1") {
		t.Error("listen flag for agent-a should have been removed")
	}
}
