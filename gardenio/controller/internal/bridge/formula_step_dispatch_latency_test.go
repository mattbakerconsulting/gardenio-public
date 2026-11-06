package bridge

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/spawnguard"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Dispatch-latency instrumentation (task ka-0000006705).
//
// The two counters this component shipped with answer BOOLEAN questions ("did
// it spawn?", "why not?"). These tests cover the third series — the readiness
// -> spawn latency histogram that answers "how long did a ready step wait for
// its agent?", the number an operator wants when the dispatcher is spawning but
// feels slow.
//
// The invariant every test here defends: a successful spawn produces EXACTLY
// ONE accounting entry, either a latency sample or an unmeasured-reason
// increment. That is what makes the histogram's _count a trustworthy
// denominator rather than a silent undercount (advice ka-0000005924).

// latencyHistogram reads the (sampleCount, sampleSum) of the latency histogram
// child for a path label. Zero/zero if the child has not been observed.
func latencyHistogram(t *testing.T, path string) (uint64, float64) {
	t.Helper()
	m := &dto.Metric{}
	h, err := formulaStepDispatchLatencySeconds.GetMetricWithLabelValues(path)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", path, err)
	}
	if err := h.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

// latencyUnmeasuredValue reads the unmeasured counter for a reason label.
func latencyUnmeasuredValue(t *testing.T, reason string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := formulaStepDispatchLatencyUnmeasuredTotal.GetMetricWithLabelValues(reason)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", reason, err)
	}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// stepDispatchTestNow is the fixed "wall clock" the latency tests pin the
// dispatcher to, so an asserted interval is exact rather than flaky.
var stepDispatchTestNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// closedBeadAt is closedBead plus the daemon-side close timestamp the readiness
// edge is measured from — the shape a real closed blocker has.
func closedBeadAt(id string, closedAt time.Time) *beadsapi.BeadDetail {
	b := closedBead(id)
	b.ClosedAt = closedAt.Format(time.RFC3339)
	b.UpdatedAt = closedAt
	return b
}

// dispatcherAtFixedNow builds a dispatcher whose clock is pinned to
// stepDispatchTestNow, so latency == stepDispatchTestNow - blocker close time.
func dispatcherAtFixedNow(f *fakeStepDaemon) *FormulaStepDispatcher {
	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.now = func() time.Time { return stepDispatchTestNow }
	return d
}

// The headline case: a step that became ready 90s before its agent existed must
// land a 90s sample on the EVENT path. This is the measurement the board's
// p50/p95 panel reads, and the one no series carried before this task.
func TestStepDispatchLatency_ObservesReadinessToSpawnInterval(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBeadAt("gd-emit", stepDispatchTestNow.Add(-90*time.Second))
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	beforeCount, beforeSum := latencyHistogram(t, stepDispatchPathEvent)
	dispatcherAtFixedNow(f).OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn, got %d", got)
	}
	afterCount, afterSum := latencyHistogram(t, stepDispatchPathEvent)
	if afterCount != beforeCount+1 {
		t.Fatalf("sample count = %d, want %d (one spawn must observe exactly one latency sample)", afterCount, beforeCount+1)
	}
	if got := afterSum - beforeSum; got != 90 {
		t.Errorf("observed latency = %vs, want 90s (blocker closed 90s before the spawn)", got)
	}
}

// The readiness edge is the LAST blocker to clear, not the first and not the
// triggering one. A step blocked on three siblings became workable when the
// newest close landed; measuring from the oldest would report a latency that
// includes time the step was still legitimately blocked.
func TestStepDispatchLatency_MeasuresFromLastBlockerToClear(t *testing.T) {
	f := newFakeStepDaemon()
	// Oldest -> newest. The step became ready at the 30s-ago close.
	f.beads["gd-old"] = closedBeadAt("gd-old", stepDispatchTestNow.Add(-10*time.Minute))
	f.beads["gd-mid"] = closedBeadAt("gd-mid", stepDispatchTestNow.Add(-5*time.Minute))
	f.beads["gd-new"] = closedBeadAt("gd-new", stepDispatchTestNow.Add(-30*time.Second))
	f.beads["gd-fix"] = readyStep("gd-fix")
	for _, blocker := range []string{"gd-old", "gd-mid", "gd-new"} {
		f.addBlocks("gd-fix", blocker)
	}

	beforeCount, beforeSum := latencyHistogram(t, stepDispatchPathEvent)
	// Driven by the OLDEST close on purpose: the readiness instant must come
	// from the dep graph, not from whichever event happened to wake us.
	dispatcherAtFixedNow(f).OnStepClosed(context.Background(), "gd-old")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn, got %d", got)
	}
	afterCount, afterSum := latencyHistogram(t, stepDispatchPathEvent)
	if afterCount != beforeCount+1 {
		t.Fatalf("sample count = %d, want %d", afterCount, beforeCount+1)
	}
	if got := afterSum - beforeSum; got != 30 {
		t.Errorf("observed latency = %vs, want 30s (max close over the blocking deps, not the min)", got)
	}
}

// The backfill sweep and the event path have structurally different latency
// distributions — sweep recovery legitimately runs to minutes or hours — so
// they must not pool into one meaningless quantile.
func TestStepDispatchLatency_BackfillPathLabelledSeparately(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBeadAt("gd-emit", stepDispatchTestNow.Add(-45*time.Minute))
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	beforeBackfill, beforeSum := latencyHistogram(t, stepDispatchPathBackfill)
	beforeEvent, _ := latencyHistogram(t, stepDispatchPathEvent)
	dispatcherAtFixedNow(f).BackfillReadySteps(context.Background())

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn from the sweep, got %d", got)
	}
	afterBackfill, afterSum := latencyHistogram(t, stepDispatchPathBackfill)
	if afterBackfill != beforeBackfill+1 {
		t.Fatalf("backfill sample count = %d, want %d", afterBackfill, beforeBackfill+1)
	}
	if got := afterSum - beforeSum; got != 45*60 {
		t.Errorf("observed backfill latency = %vs, want %vs (the sweep recovered a step stranded 45m)", got, 45*60)
	}
	if afterEvent, _ := latencyHistogram(t, stepDispatchPathEvent); afterEvent != beforeEvent {
		t.Errorf("event-path sample count moved (%d -> %d) on a sweep-driven dispatch — the paths must stay separable", beforeEvent, afterEvent)
	}
}

// A `partial` blocker clears the step but emits an UPDATE, not a close, so it
// carries no closed_at. updated_at is the honest stand-in; dropping the sample
// instead would blind the board to a whole clearance mode.
func TestStepDispatchLatency_PartialBlockerMeasuredFromUpdatedAt(t *testing.T) {
	f := newFakeStepDaemon()
	partial := closedBead("gd-partial")
	partial.Status = "partial"
	partial.ClosedAt = "" // a partial transition is not a close
	partial.UpdatedAt = stepDispatchTestNow.Add(-2 * time.Minute)
	f.beads["gd-partial"] = partial
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-partial")

	beforeCount, beforeSum := latencyHistogram(t, stepDispatchPathEvent)
	dispatcherAtFixedNow(f).OnStepClosed(context.Background(), "gd-partial")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn, got %d", got)
	}
	afterCount, afterSum := latencyHistogram(t, stepDispatchPathEvent)
	if afterCount != beforeCount+1 {
		t.Fatalf("sample count = %d, want %d (a partial clearance must still be measured)", afterCount, beforeCount+1)
	}
	if got := afterSum - beforeSum; got != 120 {
		t.Errorf("observed latency = %vs, want 120s (fallback to updated_at)", got)
	}
}

// Daemon/pod clock skew can put the readiness instant in the FUTURE. A clamped
// 0 would read as a suspiciously fast dispatch and quietly corrupt every
// quantile; the sample is dropped and counted where it is diagnosable.
func TestStepDispatchLatency_FutureReadinessDroppedAsClockSkew(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBeadAt("gd-emit", stepDispatchTestNow.Add(10*time.Second))
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	beforeCount, _ := latencyHistogram(t, stepDispatchPathEvent)
	beforeSkew := latencyUnmeasuredValue(t, latencyUnmeasuredClockSkew)
	dispatcherAtFixedNow(f).OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn, got %d", got)
	}
	if afterCount, _ := latencyHistogram(t, stepDispatchPathEvent); afterCount != beforeCount {
		t.Errorf("sample count moved (%d -> %d) — a negative interval must never reach the histogram", beforeCount, afterCount)
	}
	if got := latencyUnmeasuredValue(t, latencyUnmeasuredClockSkew) - beforeSkew; got != 1 {
		t.Errorf("clock_skew counter delta = %v, want 1 (a dropped sample must stay attributable)", got)
	}
}

// A step with no blocking dep has no "it became ready" instant at all. The
// spawn is real, so it must be accounted for — under its own reason, not folded
// into a timestamp-parsing failure.
func TestStepDispatchLatency_NoBlockingDepsAccountedUnmeasured(t *testing.T) {
	f := newFakeStepDaemon()
	step := readyStep("gd-lone")
	f.beads["gd-lone"] = step // eligible, but dep-less

	beforeCount, _ := latencyHistogram(t, stepDispatchPathBackfill)
	beforeNoDeps := latencyUnmeasuredValue(t, latencyUnmeasuredNoBlockingDeps)
	dispatcherAtFixedNow(f).BackfillReadySteps(context.Background())

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn, got %d", got)
	}
	if afterCount, _ := latencyHistogram(t, stepDispatchPathBackfill); afterCount != beforeCount {
		t.Errorf("sample count moved (%d -> %d) — there is no readiness instant to measure from", beforeCount, afterCount)
	}
	if got := latencyUnmeasuredValue(t, latencyUnmeasuredNoBlockingDeps) - beforeNoDeps; got != 1 {
		t.Errorf("no_blocking_deps counter delta = %v, want 1", got)
	}
}

// A cleared blocker with neither closed_at nor updated_at makes max(closed_at) a
// fiction. Refusing to guess keeps fabricated samples out of the histogram, and
// the reason bucket says why the spawn is missing from it.
func TestStepDispatchLatency_MissingBlockerTimestampAccountedUnmeasured(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit") // no closed_at, no updated_at
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	beforeCount, _ := latencyHistogram(t, stepDispatchPathEvent)
	beforeMissing := latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing)
	dispatcherAtFixedNow(f).OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn, got %d", got)
	}
	if afterCount, _ := latencyHistogram(t, stepDispatchPathEvent); afterCount != beforeCount {
		t.Errorf("sample count moved (%d -> %d) — an unresolvable readiness instant must not be guessed", beforeCount, afterCount)
	}
	if got := latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing) - beforeMissing; got != 1 {
		t.Errorf("blocker_time_missing counter delta = %v, want 1", got)
	}
}

// A step blocked on a mix of timestamped and untimestamped cleared blockers is
// UNMEASURABLE, not "measurable from the ones we can read": the dep we cannot
// read may well have been the last to clear, so a max over the readable subset
// would overstate the latency.
func TestStepDispatchLatency_PartiallyReadableBlockersAreUnmeasured(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-timed"] = closedBeadAt("gd-timed", stepDispatchTestNow.Add(-1*time.Hour))
	f.beads["gd-untimed"] = closedBead("gd-untimed")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-timed")
	f.addBlocks("gd-fix", "gd-untimed")

	beforeCount, _ := latencyHistogram(t, stepDispatchPathEvent)
	beforeMissing := latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing)
	dispatcherAtFixedNow(f).OnStepClosed(context.Background(), "gd-timed")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn, got %d", got)
	}
	if afterCount, _ := latencyHistogram(t, stepDispatchPathEvent); afterCount != beforeCount {
		t.Errorf("sample count moved (%d -> %d) — a max over the readable subset would overstate the wait", beforeCount, afterCount)
	}
	if got := latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing) - beforeMissing; got != 1 {
		t.Errorf("blocker_time_missing counter delta = %v, want 1", got)
	}
}

// THE ACCOUNTING INVARIANT (advice ka-0000005924). Across a mixed batch —
// measurable, dep-less, and untimestamped steps — every successful spawn lands
// in exactly one place:
//
//	_count + unmeasured_total == spawned_total
//
// This is what lets an operator read the histogram's _count as a denominator
// instead of wondering which spawns are missing from it.
func TestStepDispatchLatency_CountPlusUnmeasuredReconcilesWithSpawns(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBeadAt("gd-emit", stepDispatchTestNow.Add(-60*time.Second))
	f.beads["gd-untimed"] = closedBead("gd-untimed")
	// Measurable: two steps behind a timestamped blocker.
	for _, id := range []string{"gd-m1", "gd-m2"} {
		f.beads[id] = readyStep(id)
		f.addBlocks(id, "gd-emit")
	}
	// Unmeasurable, blocker_time_missing: one step behind an untimestamped blocker.
	f.beads["gd-u1"] = readyStep("gd-u1")
	f.addBlocks("gd-u1", "gd-untimed")
	// Unmeasurable, no_blocking_deps: one dep-less step.
	f.beads["gd-u2"] = readyStep("gd-u2")

	beforeEvent, _ := latencyHistogram(t, stepDispatchPathEvent)
	beforeBackfill, _ := latencyHistogram(t, stepDispatchPathBackfill)
	beforeUnmeasured := latencyUnmeasuredValue(t, latencyUnmeasuredNoBlockingDeps) +
		latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing) +
		latencyUnmeasuredValue(t, latencyUnmeasuredClockSkew)
	beforeSpawned := spawnCounterValue(t, "monorepo-fix", "gd-formula1", "monorepo")

	// The sweep reaches every eligible step regardless of what unblocked it.
	dispatcherAtFixedNow(f).BackfillReadySteps(context.Background())

	afterEvent, _ := latencyHistogram(t, stepDispatchPathEvent)
	afterBackfill, _ := latencyHistogram(t, stepDispatchPathBackfill)
	observed := float64(afterEvent-beforeEvent) + float64(afterBackfill-beforeBackfill)
	unmeasured := latencyUnmeasuredValue(t, latencyUnmeasuredNoBlockingDeps) +
		latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing) +
		latencyUnmeasuredValue(t, latencyUnmeasuredClockSkew) - beforeUnmeasured
	spawned := spawnCounterValue(t, "monorepo-fix", "gd-formula1", "monorepo") - beforeSpawned

	if spawned != 4 {
		t.Fatalf("precondition: want 4 spawns, got %v", spawned)
	}
	if observed+unmeasured != spawned {
		t.Errorf("accounting broken: observed(%v) + unmeasured(%v) = %v, want spawned(%v)",
			observed, unmeasured, observed+unmeasured, spawned)
	}
	if observed != 2 {
		t.Errorf("observed samples = %v, want 2 (only the two steps behind a timestamped blocker are measurable)", observed)
	}
}

// A dispatch that never spawns must never observe a latency. Otherwise the
// histogram measures "time until we gave up", which is a different quantity
// wearing the same name, and the reconciliation above breaks.
func TestStepDispatchLatency_RefusedSpawnObservesNothing(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBeadAt("gd-emit", stepDispatchTestNow.Add(-5*time.Second))
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	// A one-slot guard with its slot already consumed refuses this spawn.
	guard := spawnguard.New(spawnguard.Config{
		RateMax: 1, RateWindow: time.Hour,
		TripThreshold: 100, TripWindow: time.Minute, TripCooldown: time.Minute,
	})
	if ok, _ := guard.Allow(); !ok {
		t.Fatal("precondition: first Allow should consume the only slot")
	}
	d := NewFormulaStepDispatcher(f, guard, quietLogger())
	d.now = func() time.Time { return stepDispatchTestNow }

	beforeCount, _ := latencyHistogram(t, stepDispatchPathEvent)
	beforeUnmeasured := latencyUnmeasuredValue(t, latencyUnmeasuredNoBlockingDeps) +
		latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing) +
		latencyUnmeasuredValue(t, latencyUnmeasuredClockSkew)
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("precondition: guard must refuse, got %d spawns", got)
	}
	if afterCount, _ := latencyHistogram(t, stepDispatchPathEvent); afterCount != beforeCount {
		t.Errorf("sample count moved (%d -> %d) on a refused dispatch", beforeCount, afterCount)
	}
	afterUnmeasured := latencyUnmeasuredValue(t, latencyUnmeasuredNoBlockingDeps) +
		latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing) +
		latencyUnmeasuredValue(t, latencyUnmeasuredClockSkew)
	if afterUnmeasured != beforeUnmeasured {
		t.Errorf("unmeasured counter moved (%v -> %v) on a refused dispatch — a decline is not an unmeasurable spawn", beforeUnmeasured, afterUnmeasured)
	}
}

// The seed makes the series NAMES exist on a freshly-restarted pod (the C5
// existence problem bug ka-0000003699 hit the spawn counter with), and it must
// do so WITHOUT fabricating samples: a zero-count histogram child leaves
// histogram_quantile at NaN ("no data"), never at a made-up 0.
func TestStepDispatchLatency_SeedPublishesSeriesWithoutFabricatingSamples(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	h := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "seed_probe_seconds", Help: "probe", Buckets: []float64{1}},
		[]string{"path"},
	)
	reg.MustRegister(h)
	h.WithLabelValues(stepDispatchPathEvent)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(families) != 1 {
		t.Fatalf("seeded family count = %d, want 1 (the metric NAME must be present before any observation)", len(families))
	}
	metrics := families[0].GetMetric()
	if len(metrics) != 1 {
		t.Fatalf("seeded series count = %d, want 1", len(metrics))
	}
	if got := metrics[0].GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("seeded sample count = %d, want 0 (a seed must not fabricate a sample)", got)
	}
	for _, b := range metrics[0].GetHistogram().GetBucket() {
		if b.GetCumulativeCount() != 0 {
			t.Errorf("seeded bucket le=%v has count %d, want 0 (a seeded bucket must not move a quantile)",
				b.GetUpperBound(), b.GetCumulativeCount())
		}
	}

	// And the real constructor seeds both paths, so a per-path panel renders its
	// axes before the first spawn.
	NewFormulaStepDispatcher(newFakeStepDaemon(), nil, quietLogger())
	for _, path := range []string{stepDispatchPathEvent, stepDispatchPathBackfill} {
		if _, err := formulaStepDispatchLatencySeconds.GetMetricWithLabelValues(path); err != nil {
			t.Errorf("path %q not seeded: %v", path, err)
		}
	}
}

// stepDispatchPathFor is total over its input: the backfill sentinel is the only
// value that means "the sweep drove this", and every real bead id is the event
// path. A wrong verdict here would file recovery latency as event latency and
// wreck both distributions.
func TestStepDispatchPathFor(t *testing.T) {
	cases := map[string]string{
		stepDispatchBackfillSentinel: stepDispatchPathBackfill,
		"gd-emit":                    stepDispatchPathEvent,
		"":                           stepDispatchPathEvent,
	}
	for in, want := range cases {
		if got := stepDispatchPathFor(in); got != want {
			t.Errorf("stepDispatchPathFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// An out-of-enum reason must land in a bounded bucket rather than minting a new
// label value — the same hard-enum discipline recordDecline uses.
func TestStepDispatchLatency_UnknownUnmeasuredReasonIsBounded(t *testing.T) {
	d := NewFormulaStepDispatcher(newFakeStepDaemon(), nil, quietLogger())

	before := latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing)
	d.recordLatencyUnmeasured("some_future_typo")
	if got := latencyUnmeasuredValue(t, latencyUnmeasuredBlockerTimeMissing) - before; got != 1 {
		t.Errorf("out-of-enum reason delta on blocker_time_missing = %v, want 1", got)
	}

	// An out-of-enum PATH is likewise coerced rather than minting a series.
	beforeEvent, _ := latencyHistogram(t, stepDispatchPathEvent)
	d.now = func() time.Time { return stepDispatchTestNow }
	d.recordDispatchLatency("not_a_path", stepReadiness{at: stepDispatchTestNow.Add(-3 * time.Second)})
	afterEvent, _ := latencyHistogram(t, stepDispatchPathEvent)
	if afterEvent != beforeEvent+1 {
		t.Errorf("out-of-enum path sample count = %d, want %d (coerced to the event path)", afterEvent, beforeEvent+1)
	}
}
