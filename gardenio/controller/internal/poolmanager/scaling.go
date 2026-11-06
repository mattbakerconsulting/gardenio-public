package poolmanager

import (
	"sync"
	"time"
)

// spawnWindow is the sliding window over which spawn rate is measured.
const spawnWindow = 15 * time.Minute

// spawnTracker records assignment timestamps per project for predictive scaling.
// Thread-safe: guarded by its own mutex, independent of Manager.mu.
type spawnTracker struct {
	mu      sync.Mutex
	spawns  map[string][]time.Time // project → sorted timestamps
	nowFunc func() time.Time       // injectable clock for testing
}

func newSpawnTracker() *spawnTracker {
	return &spawnTracker{
		spawns:  make(map[string][]time.Time),
		nowFunc: time.Now,
	}
}

// Record adds a spawn event for a project at the current time.
func (t *spawnTracker) Record(project string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spawns[project] = append(t.spawns[project], t.nowFunc())
}

// Rate returns the number of spawns in the last spawnWindow for a project.
func (t *spawnTracker) Rate(project string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.nowFunc().Add(-spawnWindow)
	times := t.spawns[project]
	// Prune old entries while counting recent ones.
	recent := times[:0]
	for _, ts := range times {
		if !ts.Before(cutoff) {
			recent = append(recent, ts)
		}
	}
	t.spawns[project] = recent
	return len(recent)
}

// targetPoolSize computes the desired pool size based on recent spawn rate.
// The formula scales linearly between minSize and maxSize:
//   - 0 spawns in the window → minSize
//   - spawnsForMax or more → maxSize
//
// spawnsForMax is the spawn count at which we want the pool at max capacity.
// A reasonable default is maxSize itself (1 spawn per slot in the window).
func targetPoolSize(spawnRate, minSize, maxSize int) int {
	if maxSize <= minSize {
		return minSize
	}
	// Scale linearly: each spawn in the window adds roughly one extra slot.
	// spawnsForMax = maxSize: at max capacity when recent spawns equal max pool size.
	spawnsForMax := maxSize
	target := minSize + (spawnRate*(maxSize-minSize)+spawnsForMax-1)/spawnsForMax
	if target > maxSize {
		target = maxSize
	}
	if target < minSize {
		target = minSize
	}
	return target
}
