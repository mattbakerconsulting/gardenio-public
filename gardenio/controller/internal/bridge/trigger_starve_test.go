package bridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// Cooldown-starvation SYMPTOM detector (Alert B of epic ka-0000005107, task
// ka-0000000052). During the 2026-07-01 outage the prod jira-bridge
// cooldown-skipped 414 consecutive cycles (~13h) with zero evalOne outcomes
// while its triggers' JQL matched open issues. These tests pin the detector
// that turns that condition into gardenio_trigger_starved=1 within ~15m.

// countingStarveSource is a mockTriggerSource that also implements the
// approximateCounter interface with a scripted count/error.
type countingStarveSource struct {
	mockTriggerSource
	count      int64
	countErr   error
	countCalls int
}

func (s *countingStarveSource) ApproximateMatchCount(_ context.Context, _ TriggerDef) (int64, bool, error) {
	s.countCalls++
	return s.count, true, s.countErr
}

// starveTestEngine builds an engine with compressed thresholds and a fake
// clock the test advances. minCycles=3 keeps loops short; minDuration=10m
// exercises the wall-clock arm via clock jumps.
func starveTestEngine(logBuf *bytes.Buffer) (*TriggerEngine, *time.Time) {
	now := time.Now().UTC()
	var w = logBuf
	if w == nil {
		w = &bytes.Buffer{}
	}
	e := NewTriggerEngine(TriggerEngineConfig{
		Daemon:            &mockTriggerDaemon{},
		Logger:            slog.New(slog.NewTextHandler(w, nil)),
		StarveMinCycles:   3,
		StarveMinDuration: 10 * time.Minute,
		StarveRecheckTTL:  5 * time.Minute,
		Clock:             func() time.Time { return now },
	})
	return e, &now
}

// coolTrig returns a trigger whose LastChecked keeps it inside the cooldown
// gate at evaluation time — the shape a foreign stamper produces every cycle.
func coolTrig(id string) TriggerDef {
	return TriggerDef{
		ID: id, Title: "starvable", CooldownMinutes: 1,
		LastChecked: time.Now().UTC().Add(-10 * time.Second),
	}
}

func starvedValue(t *testing.T, source, trigger string) float64 {
	t.Helper()
	return testutil.ToFloat64(triggerStarvedGauge.WithLabelValues(source, trigger))
}

func streakValue(t *testing.T, source, trigger string) float64 {
	t.Helper()
	return testutil.ToFloat64(triggerCooldownSkipStreak.WithLabelValues(source, trigger))
}

// starvedSeriesExists reports whether the starved gauge currently carries a
// series for (source, trigger) WITHOUT creating it (WithLabelValues would).
func starvedSeriesExists(t *testing.T, source, trigger string) bool {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	triggerStarvedGauge.Collect(ch)
	close(ch)
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("writing metric: %v", err)
		}
		labels := map[string]string{}
		for _, lp := range pb.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["source"] == source && labels["trigger"] == trigger {
			return true
		}
	}
	return false
}

func TestStarveDetector_PureCooldownSkipsTripAfterThresholds(t *testing.T) {
	var logBuf bytes.Buffer
	e, now := starveTestEngine(&logBuf)
	src := &countingStarveSource{mockTriggerSource: mockTriggerSource{name: "jira"}, count: 7}
	trig := coolTrig("t-starve-trip")
	defer e.pruneStarveForSource("jira", nil)

	// Cycles 1..2: below the cycle threshold — never starved no matter the clock.
	for range 2 {
		e.evalOne(context.Background(), src, trig)
		*now = now.Add(30 * time.Second)
	}
	if got := streakValue(t, "jira", trig.ID); got != 2 {
		t.Fatalf("streak = %v, want 2", got)
	}
	if got := starvedValue(t, "jira", trig.ID); got != 0 {
		t.Fatalf("starved = %v before thresholds, want 0", got)
	}

	// Cycle 3 crosses the cycle threshold but not the duration threshold
	// (only 1m elapsed, minDuration=10m, 3×cooldown=3m).
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "jira", trig.ID); got != 0 {
		t.Fatalf("starved = %v before duration threshold, want 0", got)
	}
	if src.countCalls != 0 {
		t.Fatalf("upstream count must not be queried below thresholds, got %d calls", src.countCalls)
	}

	// Jump the clock past max(minDuration, 3×cooldown) and skip once more:
	// both thresholds crossed, count>0 → STARVED.
	*now = now.Add(11 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "jira", trig.ID); got != 1 {
		t.Fatalf("starved = %v after thresholds with count>0, want 1", got)
	}
	if src.countCalls != 1 {
		t.Fatalf("countCalls = %d, want 1", src.countCalls)
	}
	if out := logBuf.String(); !strings.Contains(out, "STARVED") || !strings.Contains(out, "level=WARN") {
		t.Errorf("expected WARN STARVED log, got:\n%s", out)
	}
	if !strings.Contains(logBuf.String(), "ka-0000005107") {
		t.Errorf("STARVED log must reference the runbook epic, got:\n%s", logBuf.String())
	}
}

func TestStarveDetector_PassingCooldownGateResets(t *testing.T) {
	var logBuf bytes.Buffer
	e, now := starveTestEngine(&logBuf)
	src := &countingStarveSource{mockTriggerSource: mockTriggerSource{name: "jira"}, count: 7}
	trig := coolTrig("t-starve-reset")
	defer e.pruneStarveForSource("jira", nil)

	// Drive to starved.
	for range 3 {
		e.evalOne(context.Background(), src, trig)
		*now = now.Add(30 * time.Second)
	}
	*now = now.Add(11 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "jira", trig.ID); got != 1 {
		t.Fatalf("starved = %v, want 1", got)
	}

	// One cycle past the cooldown gate (stale LastChecked → evaluates,
	// no-new-matches path) clears the verdict and the streak.
	logBuf.Reset()
	past := trig
	past.LastChecked = time.Now().UTC().Add(-1 * time.Hour)
	e.evalOne(context.Background(), src, past)
	if got := starvedValue(t, "jira", trig.ID); got != 0 {
		t.Errorf("starved = %v after winning a fire window, want 0", got)
	}
	if got := streakValue(t, "jira", trig.ID); got != 0 {
		t.Errorf("streak = %v after winning a fire window, want 0", got)
	}
	if !strings.Contains(logBuf.String(), "starvation cleared") {
		t.Errorf("expected starvation-cleared log, got:\n%s", logBuf.String())
	}
}

func TestStarveDetector_ZeroUpstreamCountSuppresses(t *testing.T) {
	e, now := starveTestEngine(nil)
	src := &countingStarveSource{mockTriggerSource: mockTriggerSource{name: "jira"}, count: 0}
	trig := coolTrig("t-starve-zero")
	defer e.pruneStarveForSource("jira", nil)

	for range 3 {
		e.evalOne(context.Background(), src, trig)
		*now = now.Add(30 * time.Second)
	}
	*now = now.Add(11 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "jira", trig.ID); got != 0 {
		t.Errorf("starved = %v with upstream count 0, want 0 (nothing is being missed)", got)
	}
}

func TestStarveDetector_CountErrorFailsTowardStarved(t *testing.T) {
	e, now := starveTestEngine(nil)
	src := &countingStarveSource{
		mockTriggerSource: mockTriggerSource{name: "jira"},
		countErr:          errors.New("jira 500"),
	}
	trig := coolTrig("t-starve-err")
	defer e.pruneStarveForSource("jira", nil)

	for range 3 {
		e.evalOne(context.Background(), src, trig)
		*now = now.Add(30 * time.Second)
	}
	*now = now.Add(11 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "jira", trig.ID); got != 1 {
		t.Errorf("starved = %v on count error, want 1 (fail toward paging)", got)
	}
}

func TestStarveDetector_CountRecheckHonorsTTL(t *testing.T) {
	e, now := starveTestEngine(nil)
	src := &countingStarveSource{mockTriggerSource: mockTriggerSource{name: "jira"}, count: 7}
	trig := coolTrig("t-starve-ttl")
	defer e.pruneStarveForSource("jira", nil)

	for range 3 {
		e.evalOne(context.Background(), src, trig)
		*now = now.Add(30 * time.Second)
	}
	*now = now.Add(11 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if src.countCalls != 1 {
		t.Fatalf("countCalls = %d, want 1", src.countCalls)
	}

	// Two more starved cycles inside the TTL: cached verdict, no new calls.
	*now = now.Add(30 * time.Second)
	e.evalOne(context.Background(), src, trig)
	*now = now.Add(30 * time.Second)
	e.evalOne(context.Background(), src, trig)
	if src.countCalls != 1 {
		t.Errorf("countCalls = %d inside TTL, want 1 (cached)", src.countCalls)
	}

	// Past the TTL the count is re-queried.
	*now = now.Add(6 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if src.countCalls != 2 {
		t.Errorf("countCalls = %d past TTL, want 2", src.countCalls)
	}
}

func TestStarveDetector_LongCooldownTriggerNeedsProportionallyLongerStreak(t *testing.T) {
	// A cooldown_minutes=30 trigger legitimately skips most cycles on its own
	// stamp. The duration threshold scales with the trigger's cooldown
	// (3×30m=90m > minDuration 10m), so crossing minCycles + minDuration
	// alone must NOT mark it starved.
	e, now := starveTestEngine(nil)
	src := &countingStarveSource{mockTriggerSource: mockTriggerSource{name: "jira"}, count: 7}
	trig := TriggerDef{
		ID: "t-starve-longcool", Title: "slow", CooldownMinutes: 30,
		LastChecked: time.Now().UTC().Add(-10 * time.Second),
	}
	defer e.pruneStarveForSource("jira", nil)

	for range 3 {
		e.evalOne(context.Background(), src, trig)
		*now = now.Add(30 * time.Second)
	}
	*now = now.Add(11 * time.Minute) // past minDuration, inside 3×cooldown
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "jira", trig.ID); got != 0 {
		t.Errorf("starved = %v inside 3x its own cooldown, want 0", got)
	}

	// Past 3×cooldown the verdict flips.
	*now = now.Add(90 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "jira", trig.ID); got != 1 {
		t.Errorf("starved = %v past 3x cooldown, want 1", got)
	}
}

func TestStarveDetector_SourceWithoutCounterUsesStreakAlone(t *testing.T) {
	e, now := starveTestEngine(nil)
	src := &mockTriggerSource{name: "gitlab"}
	trig := coolTrig("t-starve-nocounter")
	defer e.pruneStarveForSource("gitlab", nil)

	for range 3 {
		e.evalOne(context.Background(), src, trig)
		*now = now.Add(30 * time.Second)
	}
	*now = now.Add(11 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "gitlab", trig.ID); got != 1 {
		t.Errorf("starved = %v for counter-less source past thresholds, want 1", got)
	}
}

func TestStarveDetector_PruneDropsStateAndSeries(t *testing.T) {
	e, now := starveTestEngine(nil)
	src := &countingStarveSource{mockTriggerSource: mockTriggerSource{name: "jira"}, count: 7}
	trig := coolTrig("t-starve-prune")

	for range 3 {
		e.evalOne(context.Background(), src, trig)
		*now = now.Add(30 * time.Second)
	}
	*now = now.Add(11 * time.Minute)
	e.evalOne(context.Background(), src, trig)
	if got := starvedValue(t, "jira", trig.ID); got != 1 {
		t.Fatalf("starved = %v, want 1", got)
	}

	// The trigger closes: the next cycle no longer loads it. Its state and
	// series must be dropped so it cannot page forever.
	e.pruneStarveForSource("jira", nil)
	e.mu.Lock()
	_, kept := e.starve[trig.ID]
	e.mu.Unlock()
	if kept {
		t.Error("starve state must be pruned for unloaded triggers")
	}
	if starvedSeriesExists(t, "jira", trig.ID) {
		t.Error("gardenio_trigger_starved series must be dropped after prune so a closed trigger cannot page forever")
	}

	// A different source's entries survive a jira prune.
	other := coolTrig("t-starve-other")
	gitlabSrc := &mockTriggerSource{name: "gitlab"}
	e.evalOne(context.Background(), gitlabSrc, other)
	e.pruneStarveForSource("jira", nil)
	e.mu.Lock()
	_, kept = e.starve[other.ID]
	e.mu.Unlock()
	if !kept {
		t.Error("prune scoped to one source must not drop another source's state")
	}
	e.pruneStarveForSource("gitlab", nil)
}

func TestJiraTriggerSource_ApproximateMatchCount(t *testing.T) {
	// Unsupported when SourceData is not jiraTriggerData.
	s := NewJiraTriggerSource(nil)
	if _, supported, _ := s.ApproximateMatchCount(context.Background(), TriggerDef{}); supported {
		t.Error("non-JQL trigger must report supported=false")
	}
	if _, supported, _ := s.ApproximateMatchCount(context.Background(), TriggerDef{SourceData: jiraTriggerData{}}); supported {
		t.Error("empty JQL must report supported=false")
	}
}
