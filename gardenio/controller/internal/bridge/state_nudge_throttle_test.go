package bridge

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCheckAndSetNudgeThrottle_FirstCall(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	throttled, err := sm.CheckAndSetNudgeThrottle("agent:1.1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if throttled {
		t.Error("first call should not be throttled")
	}
}

func TestCheckAndSetNudgeThrottle_WithinInterval(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// First call: not throttled.
	if throttled, _ := sm.CheckAndSetNudgeThrottle("agent:1.1", 30*time.Second); throttled {
		t.Fatal("first call should not be throttled")
	}

	// Second call within interval: throttled.
	throttled, err := sm.CheckAndSetNudgeThrottle("agent:1.1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !throttled {
		t.Error("second call within interval should be throttled")
	}
}

func TestCheckAndSetNudgeThrottle_AfterInterval(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Backdate the throttle entry.
	sm.mu.Lock()
	sm.data.NudgeThrottles["agent:1.1"] = time.Now().Add(-31 * time.Second)
	sm.mu.Unlock()

	// Should NOT be throttled (interval expired).
	throttled, err := sm.CheckAndSetNudgeThrottle("agent:1.1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if throttled {
		t.Error("call after interval should not be throttled")
	}
}

func TestCheckAndSetNudgeThrottle_DifferentKeys(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Key A: not throttled.
	if throttled, _ := sm.CheckAndSetNudgeThrottle("agent-a:1.1", 30*time.Second); throttled {
		t.Error("key A first call should not be throttled")
	}

	// Key B: not throttled (independent).
	if throttled, _ := sm.CheckAndSetNudgeThrottle("agent-b:2.2", 30*time.Second); throttled {
		t.Error("key B should not be throttled by key A")
	}

	// Key A: throttled (same key, within interval).
	if throttled, _ := sm.CheckAndSetNudgeThrottle("agent-a:1.1", 30*time.Second); !throttled {
		t.Error("key A second call should be throttled")
	}
}

func TestCheckAndSetNudgeThrottle_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Create and set throttle.
	sm1, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if throttled, _ := sm1.CheckAndSetNudgeThrottle("agent:1.1", 30*time.Second); throttled {
		t.Fatal("first call should not be throttled")
	}

	// "Restart": create a new StateManager from the same file.
	sm2, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}

	// Should be throttled — state was persisted.
	throttled, err := sm2.CheckAndSetNudgeThrottle("agent:1.1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !throttled {
		t.Error("throttle should survive restart")
	}
}

func TestCleanExpiredNudgeThrottles(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Set up: one recent, one expired.
	sm.mu.Lock()
	sm.data.NudgeThrottles["recent"] = time.Now()
	sm.data.NudgeThrottles["expired"] = time.Now().Add(-2 * time.Hour)
	sm.mu.Unlock()

	removed, err2 := sm.CleanExpiredNudgeThrottles(time.Hour)
	if err2 != nil {
		t.Fatal(err2)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	sm.mu.RLock()
	if _, ok := sm.data.NudgeThrottles["recent"]; !ok {
		t.Error("recent entry should survive")
	}
	if _, ok := sm.data.NudgeThrottles["expired"]; ok {
		t.Error("expired entry should be removed")
	}
	sm.mu.RUnlock()
}

// TestCleanExpiredNudgeThrottles_ConcurrentSafe verifies that
// CleanExpiredNudgeThrottles is safe to call concurrently with
// CheckAndSetNudgeThrottle. This is a regression test for the race
// where CleanExpiredNudgeThrottles was exported without mutex protection.
func TestCleanExpiredNudgeThrottles_ConcurrentSafe(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("agent-%d:1.1", idx)
			// Interleave writes and cleans.
			for range 10 {
				sm.CheckAndSetNudgeThrottle(key, time.Millisecond)
				sm.CleanExpiredNudgeThrottles(time.Millisecond)
			}
		}(i)
	}
	wg.Wait()
}

func TestCompactStaleEntries_CleansNudgeThrottles(t *testing.T) {
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Add an expired nudge throttle entry.
	sm.mu.Lock()
	sm.data.NudgeThrottles["old-key"] = time.Now().Add(-2 * time.Hour)
	sm.mu.Unlock()

	removed, err := sm.CompactStaleEntries(map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed (expired throttle), got %d", removed)
	}
}
