package main

import "testing"

// TestReconcileCounters_TransientFetchNotPruned asserts the A2 fix (bug
// ka-0000006095): when the iterations lookup TRANSIENTLY FAILED, IterationCount
// collapses to 0 while RunsSpawned is a real N — reconcileCounters must NOT
// frame the resulting gap as "older runs pruned", because nothing was pruned;
// the runs were simply momentarily unfetchable.
func TestReconcileCounters_TransientFetchNotPruned(t *testing.T) {
	// A real gardener: 100 spawned / 100 completed, but the detail page's
	// iterations fetch failed, so IterationCount is a spurious 0.
	v := gardenerView{
		RunsSpawned:           100,
		RunsCompleted:         100,
		IterationCount:        0,
		IterationsUnavailable: true,
	}
	v.reconcileCounters()

	if v.EnginePruned {
		t.Fatalf("EnginePruned = true on a transient fetch failure — must be suppressed (nothing was pruned)")
	}
	want := "100 spawned / 100 completed"
	if v.EngineCount != want {
		t.Fatalf("EngineCount = %q, want %q (no pruned suffix)", v.EngineCount, want)
	}
}

// TestReconcileCounters_GenuinePrunedStillFramed is the control: the SAME large
// gap, but from a genuine fetch (IterationsUnavailable=false), still frames as
// pruned. The A2 fix must suppress the framing ONLY on the error path, never
// weaken the real pruned signal (design doc §1.3).
func TestReconcileCounters_GenuinePrunedStillFramed(t *testing.T) {
	v := gardenerView{
		RunsSpawned:           379,
		RunsCompleted:         378,
		IterationCount:        60, // 319 genuinely unfetchable-because-pruned
		IterationsUnavailable: false,
	}
	v.reconcileCounters()

	if !v.EnginePruned {
		t.Fatalf("EnginePruned = false on a genuine 379-vs-60 gap — the real pruned signal must survive")
	}
	want := "379 spawned / 378 completed (older runs pruned)"
	if v.EngineCount != want {
		t.Fatalf("EngineCount = %q, want %q", v.EngineCount, want)
	}
}

// TestReconcileCounters_UnavailableWithSmallGap confirms the suppression does
// not depend on the gap size: even a gap that would NOT normally trip the
// pruned threshold stays un-pruned on the error path, and the honest lifetime
// tally is preserved.
func TestReconcileCounters_UnavailableWithSmallGap(t *testing.T) {
	v := gardenerView{
		RunsSpawned:           2,
		RunsCompleted:         1,
		IterationCount:        0,
		IterationsUnavailable: true,
	}
	v.reconcileCounters()
	if v.EnginePruned {
		t.Fatalf("EnginePruned = true, want false on the unavailable path")
	}
	if v.EngineCount != "2 spawned / 1 completed" {
		t.Fatalf("EngineCount = %q, want %q", v.EngineCount, "2 spawned / 1 completed")
	}
}
