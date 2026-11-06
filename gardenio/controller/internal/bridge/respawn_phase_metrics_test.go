package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// respawnPhaseSampleCount reads the running sample count of one
// gardenio_slackbridge_respawn_phase_seconds child series. A HistogramVec's
// WithLabelValues returns an Observer (not a Collector), so testutil.ToFloat64
// won't take it — write the child metric directly and read its SampleCount,
// mirroring coldRespawnSampleCount in agentstream_test.go.
func respawnPhaseSampleCount(t *testing.T, phase, role string) uint64 {
	t.Helper()
	m := &dto.Metric{}
	obs := respawnPhaseSeconds.WithLabelValues(phase, role)
	if err := obs.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("write respawn-phase histogram child {%s,%s}: %v", phase, role, err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestRespawnPhaseValid(t *testing.T) {
	cases := map[string]bool{
		respawnPhaseDispatch: true,
		respawnPhaseBoot:     true,
		respawnPhaseTotal:    true,
		"":                   false,
		"init":               false,
		"ready":              false,
	}
	for in, want := range cases {
		if got := respawnPhaseValid(in); got != want {
			t.Errorf("respawnPhaseValid(%q)=%v, want %v", in, got, want)
		}
	}
}

// TestRecordRespawnPhase_ObservesAndCountsSlow proves the histogram observes a
// sample per valid phase and that a phase over its anomaly threshold bumps the
// slow counter, while an under-threshold phase does not.
func TestRecordRespawnPhase_ObservesAndCountsSlow(t *testing.T) {
	respawnPhaseSeconds.Reset()
	respawnPhaseSlowTotal.Reset()

	// A healthy respawn: dispatch fast, boot fast, total fast — no slow counts.
	if !recordRespawnPhase("crew", respawnPhaseDispatch, 1) {
		t.Fatal("recordRespawnPhase(dispatch,1) returned false, want observed")
	}
	recordRespawnPhase("crew", respawnPhaseBoot, 40)
	recordRespawnPhase("crew", respawnPhaseTotal, 41)

	if got := testutil.CollectAndCount(respawnPhaseSeconds); got != 3 {
		t.Errorf("respawnPhaseSeconds series = %d, want 3 (dispatch/boot/total)", got)
	}
	if got := testutil.CollectAndCount(respawnPhaseSlowTotal); got != 0 {
		t.Errorf("respawnPhaseSlowTotal series = %d, want 0 (nothing over threshold)", got)
	}

	// An anomalous respawn: each phase over its threshold bumps the slow counter.
	recordRespawnPhase("crew", respawnPhaseDispatch, 20) // > 15
	recordRespawnPhase("crew", respawnPhaseBoot, 200)    // > 150
	recordRespawnPhase("crew", respawnPhaseTotal, 220)   // > 180

	for _, phase := range []string{respawnPhaseDispatch, respawnPhaseBoot, respawnPhaseTotal} {
		if got := testutil.ToFloat64(respawnPhaseSlowTotal.WithLabelValues(phase)); got != 1 {
			t.Errorf("respawnPhaseSlowTotal{%s} = %v, want 1", phase, got)
		}
	}
}

// TestRecordRespawnPhase_DropsBadInput proves an out-of-enum phase and a
// negative (clock-skew) duration are dropped, keeping the series bounded and
// uncorrupted — mirroring metrics.RecordResumeLatency.
func TestRecordRespawnPhase_DropsBadInput(t *testing.T) {
	respawnPhaseSeconds.Reset()
	respawnPhaseSlowTotal.Reset()

	if recordRespawnPhase("crew", "bogus-phase", 5) {
		t.Error("recordRespawnPhase with bad phase returned true, want dropped")
	}
	if recordRespawnPhase("crew", respawnPhaseDispatch, -1) {
		t.Error("recordRespawnPhase with negative duration returned true, want dropped")
	}
	if got := testutil.CollectAndCount(respawnPhaseSeconds); got != 0 {
		t.Errorf("respawnPhaseSeconds series = %d, want 0 (both inputs dropped)", got)
	}
}

// TestRecordRespawnPhase_EmptyRoleCollapses proves an empty role is stored as
// "unknown" rather than leaking an empty-string label.
func TestRecordRespawnPhase_EmptyRoleCollapses(t *testing.T) {
	respawnPhaseSeconds.Reset()
	recordRespawnPhase("", respawnPhaseTotal, 10)
	if got := respawnPhaseSampleCount(t, respawnPhaseTotal, "unknown"); got != 1 {
		t.Errorf("respawnPhaseSeconds{total,unknown}_count = %v, want 1", got)
	}
}

// TestHandleSessionStart_ObservesRespawnPhases proves the end-to-end wiring: a
// respawn armed with MarkRespawnPhase records one dispatch, one boot, and one
// total sample when the agent first proves life via SessionStart — and that a
// second SessionStart does not double-observe (the stamp is consumed).
func TestHandleSessionStart_ObservesRespawnPhases(t *testing.T) {
	respawnPhaseSeconds.Reset()
	mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
	ctx := context.Background()

	t0 := time.Now().Add(-30 * time.Second) // follow-up arrived 30s ago
	dispatchDone := t0.Add(2 * time.Second) // bridge dispatch took ~2s
	mgr.MarkRespawnPhase("phase-agent", t0, dispatchDone, "thread")
	mgr.StartSession(ctx, "phase-agent", "C1", "ts-1", "U1")
	mgr.HandleSessionStart(ctx, "phase-agent", "resume")

	for _, phase := range []string{respawnPhaseDispatch, respawnPhaseBoot, respawnPhaseTotal} {
		if got := respawnPhaseSampleCount(t, phase, "thread"); got != 1 {
			t.Errorf("respawnPhaseSeconds{%s,thread}_count = %v, want 1", phase, got)
		}
	}

	// Second SessionStart must not re-observe — the stamp was consumed.
	mgr.HandleSessionStart(ctx, "phase-agent", "resume")
	if got := respawnPhaseSampleCount(t, respawnPhaseTotal, "thread"); got != 1 {
		t.Errorf("respawnPhaseSeconds{total,thread}_count = %v after 2nd SessionStart, want 1 (no double-observe)", got)
	}
}

// TestRespawnPhaseMetricsOnDefaultRegistry proves the phase metrics are
// registered on the DEFAULT Prometheus registry (via promauto) — the contract
// the slack-bridge /metrics endpoint (prometheus.DefaultGatherer) depends on. A
// metric on a private registry would increment but never be scraped.
func TestRespawnPhaseMetricsOnDefaultRegistry(t *testing.T) {
	respawnPhaseSeconds.Reset()
	respawnPhaseSlowTotal.Reset()
	recordRespawnPhase("crew", respawnPhaseBoot, 200) // one histogram sample + one slow count

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"gardenio_slackbridge_respawn_phase_seconds",
		"gardenio_slackbridge_respawn_phase_slow_total",
	} {
		if !names[want] {
			t.Errorf("metric family %q not on the default registry (would never be scraped via /metrics)", want)
		}
	}
}
