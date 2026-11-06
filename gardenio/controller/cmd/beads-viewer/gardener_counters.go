package main

// Gardener run-count reconciliation: the header's PRIMARY/SECONDARY counter
// model. The authoritative count the page advertises is the live IterationCount
// (== the number of rendered life-thread cards); the engine's GardenerSpec
// lifetime tally (RunsSpawned / RunsCompleted) is demoted to a labeled,
// pruned-aware SECONDARY line so the header can never advertise a spawn count
// the body cannot honor (design doc ka-0000003322 §1.3). Split out of gardener.go
// to keep that file under its size budget.

// gardenerPrunedGap is how far the engine lifetime tally (GardenerSpec.RunsSpawned)
// may exceed the live iteration count before we frame the secondary line as
// "older runs pruned". A gap of 1 is normal and benign: the engine stamps
// RunsSpawned at spawn time while the live count includes the just-spawned
// in-flight run, so the spawn count is routinely exactly one ahead mid-loop.
// Anything larger means older iteration beads have been pruned and can no
// longer be rendered (design doc §1.3) — surface that honestly with the word
// "pruned" rather than a bug-shaped warning.
const gardenerPrunedGap = 1

// reconcileCounters builds the always-shown SECONDARY engine-count line from the
// monotonic lifetime tally (RunsSpawned / RunsCompleted) and frames it against
// the authoritative live IterationCount. It is called by the page handler once
// IterationCount has been set from the rendered life-thread.
//
// The PRIMARY counter the page advertises is the live IterationCount (== the
// rendered cards). The engine tally is demoted to a labeled secondary so the
// header can never advertise a spawn count we cannot render. When RunsSpawned
// exceeds the live count by more than gardenerPrunedGap, the line is framed as
// "older runs pruned" so the operator understands why the body is shorter than
// the lifetime spawn count — not as a drift/error annotation.
func (v *gardenerView) reconcileCounters() {
	// Nothing to frame for a gardener that has never spawned or completed a run
	// and whose live thread is empty: leave the secondary line off entirely.
	if v.RunsSpawned == 0 && v.RunsCompleted == 0 && v.IterationCount == 0 {
		v.EngineCount = ""
		v.EnginePruned = false
		v.EngineDerived = false
		return
	}
	// Legacy spawn_agent / bare-trigger gardeners carry NO GardenerSpec engine
	// blob, so RunsSpawned/RunsCompleted decode to zero even when the gardener
	// has a long, healthy life-thread (bug ka-0000005661: ~21 gardeners showed
	// "0 spawned / 0 completed" beneath an Iterations line of dozens-to-hundreds).
	// A zero engine tally with a non-empty thread is not a real count — it is the
	// ABSENCE of one. DERIVE the secondary line from the rendered life-thread in
	// that case: spawned = IterationCount (the runs we can see), completed =
	// IterationCount - LiveCount (the closed ones), flagged EngineDerived so the
	// template can source-label it "from life-thread" instead of falsely claiming
	// the engine's lifetime tally. This makes the line agree with the
	// authoritative Iterations chip above instead of contradicting it. A gardener
	// with a real engine tally (>0 on either counter) keeps the engine-snapshot
	// framing as before — including the legacy completed-only shape.
	if v.RunsSpawned == 0 && v.RunsCompleted == 0 {
		completed := max(v.IterationCount-v.LiveCount, 0)
		v.EngineDerived = true
		v.EnginePruned = false
		v.EngineCount = fmtEngineCount(v.IterationCount, completed, false)
		return
	}
	v.EngineDerived = false
	// A2 (bug ka-0000006095): the pruned framing reads the gap between the
	// engine's lifetime RunsSpawned and the live IterationCount as "older runs
	// pruned". That inference is only valid when IterationCount is a REAL count
	// of the runs we could fetch. When the iterations lookup transiently FAILED
	// (IterationsUnavailable), IterationCount collapses to 0 while RunsSpawned is
	// a genuine N — so the gap is spuriously large and the line would read
	// "N spawned / 0 completed (older runs pruned)" even though nothing was
	// pruned; the runs were simply momentarily unfetchable. Suppress the pruned
	// framing in that case and keep the honest lifetime tally without the
	// misleading "(older runs pruned)" suffix.
	if v.IterationsUnavailable {
		v.EnginePruned = false
		v.EngineCount = fmtEngineCount(v.RunsSpawned, v.RunsCompleted, false)
		return
	}
	v.EnginePruned = v.RunsSpawned-v.IterationCount > gardenerPrunedGap
	v.EngineCount = fmtEngineCount(v.RunsSpawned, v.RunsCompleted, v.EnginePruned)
}

// fmtEngineCount renders the secondary engine-count line. The lifetime tally is
// always shown as "N spawned / M completed"; when older iteration beads have
// been pruned (pruned=true) it is suffixed with "(older runs pruned)" so the
// operator reads the gap as an explained lifetime count, not a lost-data bug.
func fmtEngineCount(spawned, completed int, pruned bool) string {
	s := itoa(spawned) + " spawned / " + itoa(completed) + " completed"
	if pruned {
		s += " (older runs pruned)"
	}
	return s
}
