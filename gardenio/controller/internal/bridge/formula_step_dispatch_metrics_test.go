package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/spawnguard"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// declineCounterValue reads the current value of the decline counter for a
// reason label. Returns 0 if the series has not been touched.
func declineCounterValue(t *testing.T, reason string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := formulaStepDispatchDeclinedTotal.GetMetricWithLabelValues(reason)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", reason, err)
	}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// spawnCounterValue reads the current value of the spawn counter for a
// (role, formula, project) label set. Returns 0 if untouched.
func spawnCounterValue(t *testing.T, role, formula, project string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := formulaStepDispatchSpawnedTotal.GetMetricWithLabelValues(role, formula, project)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q,%q,%q): %v", role, formula, project, err)
	}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestStepDispatchMetrics_SpawnIncrementsCounter proves the affirmative half:
// a successful dispatch bumps the spawn counter on the step's (role, formula,
// project) — the "did it spawn anything today?" signal.
func TestStepDispatchMetrics_SpawnIncrementsCounter(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix") // role monorepo-fix, formula gd-formula1, project monorepo
	f.addBlocks("gd-fix", "gd-emit")

	before := spawnCounterValue(t, "monorepo-fix", "gd-formula1", "monorepo")
	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("precondition: want 1 spawn, got %d", got)
	}
	after := spawnCounterValue(t, "monorepo-fix", "gd-formula1", "monorepo")
	if after != before+1 {
		t.Errorf("spawn counter = %v, want %v (a spawn must bump gardenio_formula_step_dispatch_spawned_total)", after, before+1)
	}
}

// TestStepDispatchMetrics_SpawnCounterReconcilesWithStamp is the two-way
// cross-check (advice ka-0000005924): the spawn counter and the count of steps
// stamped dispatched_by=formula-step-dispatcher are two independent derivations
// of the same quantity and must agree. Three eligible steps -> +3 on the
// counter AND 3 beads carrying the durable stamp.
func TestStepDispatchMetrics_SpawnCounterReconcilesWithStamp(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	for _, id := range []string{"gd-fix1", "gd-fix2", "gd-fix3"} {
		f.beads[id] = readyStep(id)
		f.addBlocks(id, "gd-emit")
	}

	before := spawnCounterValue(t, "monorepo-fix", "gd-formula1", "monorepo")
	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	// Derivation A: the counter delta.
	counterDelta := spawnCounterValue(t, "monorepo-fix", "gd-formula1", "monorepo") - before

	// Derivation B: beads carrying the durable dispatched_by stamp.
	stamped := 0
	for _, b := range f.beads {
		if b.Fields["dispatched_by"] == "formula-step-dispatcher" {
			stamped++
		}
	}

	if counterDelta != 3 {
		t.Errorf("spawn counter delta = %v, want 3", counterDelta)
	}
	if stamped != 3 {
		t.Errorf("beads stamped dispatched_by=formula-step-dispatcher = %d, want 3", stamped)
	}
	if int(counterDelta) != stamped {
		t.Errorf("reconciliation failed: counter delta %v != stamped-bead count %d — the two derivations of 'agents dispatched' disagree (advice ka-0000005924)", counterDelta, stamped)
	}
}

// TestStepDispatchMetrics_NotOptedInIncrementsDecline is the G1 signature: a
// step that never opted in records a not_opted_in decline and zero spawns, so a
// dashboard shows a flat spawn series beside a climbing decline series.
func TestStepDispatchMetrics_NotOptedInIncrementsDecline(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	step := readyStep("gd-fix")
	delete(step.Fields, "suggest_new_agent")
	f.beads["gd-fix"] = step
	f.addBlocks("gd-fix", "gd-emit")

	before := declineCounterValue(t, declineNotOptedIn)
	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("precondition: want no spawn, got %d", got)
	}
	if got := declineCounterValue(t, declineNotOptedIn); got != before+1 {
		t.Errorf("not_opted_in decline counter = %v, want %v", got, before+1)
	}
}

// TestStepDispatchMetrics_EligibilityReasonsMapToDistinctCodes drives every
// eligibility-gate decline through the real considerStep path and asserts each
// one lands on its OWN bounded reason code — the property that makes "why
// didn't my step fire?" answerable by topk(reason).
func TestStepDispatchMetrics_EligibilityReasonsMapToDistinctCodes(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*beadsapi.BeadDetail)
		wantCode string
	}{
		{"not open", func(b *beadsapi.BeadDetail) { b.Status = "closed" }, declineNotOpen},
		{"no fields", func(b *beadsapi.BeadDetail) { b.Fields = nil }, declineNoFields},
		{"not a formula step", func(b *beadsapi.BeadDetail) { delete(b.Fields, "source_step_id") }, declineNotFormulaStep},
		{"not opted in", func(b *beadsapi.BeadDetail) { b.Fields["suggest_new_agent"] = "false" }, declineNotOptedIn},
		{"no role", func(b *beadsapi.BeadDetail) {
			delete(b.Fields, "step_role")
			b.Labels = []string{"project:monorepo"}
		}, declineNoRole},
		{"already stamped", func(b *beadsapi.BeadDetail) { b.Fields["dispatched_agent"] = "monorepo-fix-x" }, declineAlreadyStamped},
		{"already assigned", func(b *beadsapi.BeadDetail) { b.Assignee = "some-crew" }, declineAlreadyAssigned},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeStepDaemon()
			f.beads["gd-emit"] = closedBead("gd-emit")
			step := readyStep("gd-fix")
			tc.mutate(step)
			f.beads["gd-fix"] = step
			f.addBlocks("gd-fix", "gd-emit")

			before := declineCounterValue(t, tc.wantCode)
			NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

			if got := declineCounterValue(t, tc.wantCode); got != before+1 {
				t.Errorf("decline[%s] = %v, want %v", tc.wantCode, got, before+1)
			}
			if got := f.spawnCount(); got != 0 {
				t.Errorf("want no spawn for an ineligible step, got %d", got)
			}
		})
	}
}

// TestStepDispatchMetrics_StillBlockedIncrementsDecline: a step whose sibling
// blockers are still open records still_blocked, not a spawn.
func TestStepDispatchMetrics_StillBlockedIncrementsDecline(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-a"] = closedBead("gd-a")
	f.beads["gd-b"] = &beadsapi.BeadDetail{ID: "gd-b", Status: "open"}
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-a")
	f.addBlocks("gd-fix", "gd-b")

	before := declineCounterValue(t, declineStillBlocked)
	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-a")

	if got := declineCounterValue(t, declineStillBlocked); got != before+1 {
		t.Errorf("still_blocked decline = %v, want %v", got, before+1)
	}
}

// TestStepDispatchMetrics_GuardRefusedIncrementsDecline: a spawn-guard refusal
// is a decline, records guard_refused, and leaves no durable stamp
// (ka-0000004839).
func TestStepDispatchMetrics_GuardRefusedIncrementsDecline(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	guard := spawnguard.New(spawnguard.Config{
		RateMax: 1, RateWindow: time.Hour,
		TripThreshold: 100, TripWindow: time.Minute, TripCooldown: time.Minute,
	})
	if ok, _ := guard.Allow(); !ok {
		t.Fatal("precondition: first Allow should consume the only slot")
	}

	before := declineCounterValue(t, declineGuardRefused)
	NewFormulaStepDispatcher(f, guard, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if got := declineCounterValue(t, declineGuardRefused); got != before+1 {
		t.Errorf("guard_refused decline = %v, want %v", got, before+1)
	}
}

// TestStepDispatchMetrics_SpawnFailedIncrementsDecline: SpawnAgent erroring is a
// decline, records spawn_failed (not a spawn), and does not touch the spawn
// counter.
func TestStepDispatchMetrics_SpawnFailedIncrementsDecline(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")
	f.spawnErr = errors.New("no capacity")

	beforeDecline := declineCounterValue(t, declineSpawnFailed)
	beforeSpawn := spawnCounterValue(t, "monorepo-fix", "gd-formula1", "monorepo")
	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if got := declineCounterValue(t, declineSpawnFailed); got != beforeDecline+1 {
		t.Errorf("spawn_failed decline = %v, want %v", got, beforeDecline+1)
	}
	if got := spawnCounterValue(t, "monorepo-fix", "gd-formula1", "monorepo"); got != beforeSpawn {
		t.Errorf("a failed spawn must NOT bump the spawn counter: got %v, want %v", got, beforeSpawn)
	}
}

// TestStepDispatchMetrics_ListDepsFailedIncrementsDecline: a GetReverseDependencies
// error loses the whole close event for dispatch — recorded as listdeps_failed.
func TestStepDispatchMetrics_ListDepsFailedIncrementsDecline(t *testing.T) {
	f := newFakeStepDaemon()
	f.revDepErr = errors.New("daemon down")

	before := declineCounterValue(t, declineListDepsFailed)
	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if got := declineCounterValue(t, declineListDepsFailed); got != before+1 {
		t.Errorf("listdeps_failed decline = %v, want %v", got, before+1)
	}
}

// TestStepDispatchMetrics_DedupIncrementsDecline: a replayed close for a step
// already dispatched this process records already_dispatched_process, so a burst
// of SSE replays is visible as declines rather than silence.
func TestStepDispatchMetrics_DedupIncrementsDecline(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit") // spawns

	before := declineCounterValue(t, declineAlreadyDispatchedProcess)
	d.OnStepClosed(context.Background(), "gd-emit") // replay -> dedup decline
	d.OnStepClosed(context.Background(), "gd-emit") // replay -> dedup decline

	if got := declineCounterValue(t, declineAlreadyDispatchedProcess); got != before+2 {
		t.Errorf("already_dispatched_process decline = %v, want %v", got, before+2)
	}
}

// TestStepDispatchDeclineReasonValid_Enum guards the cardinality bound: only the
// enumerated codes (plus the unknown catch-all) are members.
func TestStepDispatchDeclineReasonValid_Enum(t *testing.T) {
	valid := []string{
		declineListDepsFailed, declineAlreadyDispatchedProcess, declineHydrateFailed,
		declineBlockCheckFailed, declineStillBlocked, declineNotOpen, declineNoFields,
		declineNotFormulaStep, declineNotOptedIn, declineNoRole, declineAlreadyStamped,
		declineAlreadyAssigned, declineLostRace, declineGuardRefused, declineStampFailed,
		declineSpawnFailed, declineUnknown,
	}
	for _, r := range valid {
		if !stepDispatchDeclineReasonValid(r) {
			t.Errorf("stepDispatchDeclineReasonValid(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "declined", "Still_Blocked", "spawn_failed ", "made_up"} {
		if stepDispatchDeclineReasonValid(r) {
			t.Errorf("stepDispatchDeclineReasonValid(%q) = true, want false", r)
		}
	}
}

// TestStepDispatchMetrics_UnknownReasonRoutesToBucket: an out-of-enum reason is
// recorded under 'unknown' rather than dropped or minting a new label — a
// loss-prevention component must not undercount declines.
func TestStepDispatchMetrics_UnknownReasonRoutesToBucket(t *testing.T) {
	d := NewFormulaStepDispatcher(newFakeStepDaemon(), nil, quietLogger())

	before := declineCounterValue(t, declineUnknown)
	d.recordDecline("a-reason-that-is-not-in-the-enum")
	if got := declineCounterValue(t, declineUnknown); got != before+1 {
		t.Errorf("unknown-bucket counter = %v, want %v (an out-of-enum reason must land in 'unknown', not be dropped)", got, before+1)
	}
}

// spawnedSeriesCount returns how many series the spawn CounterVec currently
// publishes to the registry — the store-presence signal the dashboard C5
// existence check (kbeads charts_audit.go ClassifyExistence) probes.
func spawnedSeriesCount(t *testing.T) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() {
		formulaStepDispatchSpawnedTotal.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}
	return n
}

// TestStepDispatchMetrics_SeedPresentBeforeAnySpawn is the durable fix for the
// thrice-re-found "spawned_total dark" bug (ka-0000003699, re-find of
// ka-0000007851 / ka-0000000749). A CounterVec publishes no series until its
// first WithLabelValues() call, so a freshly-restarted trigger-engine pod that
// has not yet dispatched a (rare, bursty) formula spawn used to expose ZERO
// spawned_total series — indistinguishable from a dead emitter to the dashboard
// C5 existence probe. Constructing the dispatcher must seed a zero baseline so
// the series name is present from t=0.
func TestStepDispatchMetrics_SeedPresentBeforeAnySpawn(t *testing.T) {
	// Constructing the dispatcher seeds the baseline series (no spawn required).
	NewFormulaStepDispatcher(newFakeStepDaemon(), nil, quietLogger())

	if got := spawnedSeriesCount(t); got < 1 {
		t.Fatalf("spawn counter must publish >=1 series after construction (the zero baseline) so the metric name is present before any spawn; got %d", got)
	}

	// The baseline is a zero-valued sentinel: it must not be counted as a real
	// spawn, so its value stays 0 and it never inflates increase()/sum().
	if got := spawnCounterValue(t, stepDispatchSeedLabel, stepDispatchSeedLabel, stepDispatchSeedLabel); got != 0 {
		t.Errorf("seed baseline series must be zero-valued (contributes 0 to increase/sum); got %v", got)
	}
}

// TestStepDispatchMetrics_EmptyLabelsCoerced: a spawn missing source_formula /
// project must not produce a blank, un-selectable series.
func TestStepDispatchMetrics_EmptyLabelsCoerced(t *testing.T) {
	d := NewFormulaStepDispatcher(newFakeStepDaemon(), nil, quietLogger())

	before := spawnCounterValue(t, "monorepo-fix", "unknown", "unknown")
	d.recordSpawn("monorepo-fix", "", "")
	if got := spawnCounterValue(t, "monorepo-fix", "unknown", "unknown"); got != before+1 {
		t.Errorf("empty formula/project should coerce to 'unknown': got %v, want %v", got, before+1)
	}
}
