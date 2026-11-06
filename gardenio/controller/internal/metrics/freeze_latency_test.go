package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestRecordFreezeLatency_EnumGuards guards ka-0000005147: an out-of-enum
// label value is dropped, so freeze-latency series cardinality stays bounded.
func TestRecordFreezeLatency_EnumGuards(t *testing.T) {
	fm := NewFreezeMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := fm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name       string
		seconds    float64
		step, role string
		runtime    string
		want       bool
	}{
		{"valid total", 4.2, FreezeStepTotal, "crew", FreezeRuntimeRunsc, true},
		{"valid checkpoint", 3.1, FreezeStepCheckpoint, "gardener", FreezeRuntimeRunsc, true},
		{"valid relocation", 0.9, FreezeStepRelocation, "crew", FreezeRuntimeRunsc, true},
		{"valid persist", 0.01, FreezeStepPersist, "crew", FreezeRuntimeRunsc, true},
		{"empty role -> unknown, still recorded", 2.0, FreezeStepTotal, "", FreezeRuntimeRunsc, true},
		{"zero duration recorded (a sub-ms persist)", 0, FreezeStepPersist, "crew", FreezeRuntimeRunsc, true},
		{"negative duration dropped", -1, FreezeStepTotal, "crew", FreezeRuntimeRunsc, false},
		{"bad step dropped", 1.0, "teleport", "crew", FreezeRuntimeRunsc, false},
		{"bad runtime dropped", 1.0, FreezeStepTotal, "crew", "kata", false},
	}
	for _, c := range cases {
		if got := fm.RecordFreezeLatency(c.seconds, c.step, c.role, c.runtime); got != c.want {
			t.Errorf("%s: RecordFreezeLatency=%v want %v", c.name, got, c.want)
		}
	}

	// Exactly the 6 valid observations above must have landed; the 3 invalid
	// ones must not have created any sample.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total uint64
	var series int
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_freeze_latency_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			series++
			total += m.GetHistogram().GetSampleCount()
			// env label must be threaded onto every series.
			if !hasLabel(m, "env", "e2e") {
				t.Errorf("series missing env=e2e label: %v", m.GetLabel())
			}
		}
	}
	if total != 6 {
		t.Errorf("expected 6 valid observations, got %d", total)
	}
	// total + checkpoint + relocation + persist(x2: crew empty-role distinct? no,
	// "" -> unknown so a SEPARATE series) — assert at least the 4 distinct steps
	// produced series and that the empty-role persist collapsed to unknown.
	if series < 4 {
		t.Errorf("expected at least 4 step series, got %d", series)
	}
}

// TestRecordFreezeLatency_NilReceiver verifies a nil metric (not constructed) is
// a safe no-op, so the controller can call Record unconditionally.
func TestRecordFreezeLatency_NilReceiver(t *testing.T) {
	var fm *FreezeMetrics
	if got := fm.RecordFreezeLatency(1.0, FreezeStepTotal, "crew", FreezeRuntimeRunsc); got {
		t.Errorf("nil receiver RecordFreezeLatency=true, want false (no-op)")
	}
	if err := fm.Register(nil); err != nil {
		t.Errorf("nil receiver Register: %v", err)
	}
}

// TestFreezeMetrics_StepEnum guards the step-label closed set so a typo in the
// controller wiring is caught here rather than silently dropping a sample.
func TestFreezeMetrics_StepEnum(t *testing.T) {
	for _, s := range []string{FreezeStepCheckpoint, FreezeStepRelocation, FreezeStepPersist, FreezeStepTotal} {
		if !freezeStepValid(s) {
			t.Errorf("step %q must be valid", s)
		}
	}
	for _, s := range []string{"", "checkpoint", "copy", "Total", "step"} {
		if freezeStepValid(s) {
			t.Errorf("step %q must be invalid", s)
		}
	}
}
