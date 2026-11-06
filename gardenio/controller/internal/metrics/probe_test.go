package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestProbeMetrics_RecordAndRegister(t *testing.T) {
	pm := NewProbeMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := pm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A tick both increments the counter and zeroes the staleness gauge.
	pm.SetSecondsSinceLastTick(42)
	pm.RecordTick()
	pm.RecordTick()

	if got := testutil.ToFloat64(pm.ticks.WithLabelValues("e2e")); got != 2 {
		t.Errorf("ticks = %v, want 2", got)
	}
	if got := testutil.ToFloat64(pm.secsSinceLastTick.WithLabelValues("e2e")); got != 0 {
		t.Errorf("seconds_since_last_tick = %v, want 0 (reset by RecordTick)", got)
	}

	pm.RecordPanicRecovered()
	pm.RecordPanicRecovered()
	pm.RecordPanicRecovered()
	if got := testutil.ToFloat64(pm.panicsRecovered.WithLabelValues("e2e")); got != 3 {
		t.Errorf("panics_recovered = %v, want 3", got)
	}

	pm.RecordStallDetected()
	if got := testutil.ToFloat64(pm.stallsDetected.WithLabelValues("e2e")); got != 1 {
		t.Errorf("stalls_detected = %v, want 1", got)
	}

	// The watchdog publishes the current staleness.
	pm.SetSecondsSinceLastTick(123.5)
	if got := testutil.ToFloat64(pm.secsSinceLastTick.WithLabelValues("e2e")); got != 123.5 {
		t.Errorf("seconds_since_last_tick = %v, want 123.5", got)
	}

	// A negative staleness is a clock-skew guard — ignored, prior value retained.
	pm.SetSecondsSinceLastTick(-1)
	if got := testutil.ToFloat64(pm.secsSinceLastTick.WithLabelValues("e2e")); got != 123.5 {
		t.Errorf("seconds_since_last_tick after negative = %v, want 123.5 (unchanged)", got)
	}
}

func TestProbeMetrics_NilReceiver(t *testing.T) {
	var pm *ProbeMetrics
	// Every method must be nil-safe (metrics disabled).
	if err := pm.Register(prometheus.NewRegistry()); err != nil {
		t.Errorf("nil Register: %v", err)
	}
	pm.RecordTick()
	pm.RecordPanicRecovered()
	pm.RecordStallDetected()
	pm.SetSecondsSinceLastTick(5)
}
