package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestRecordMicrovmLaunch_EnumGuards guards ka-0000003938: an out-of-enum
// outcome is dropped, so launch-latency series cardinality stays bounded.
func TestRecordMicrovmLaunch_EnumGuards(t *testing.T) {
	m := NewMicrovmLaunchMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name                   string
		seconds                float64
		outcome, project, role string
		want                   bool
	}{
		{"valid success", 1.2, MicrovmLaunchSuccess, "gardenio", "thread", true},
		{"valid failed still measured", 4.5, MicrovmLaunchFailed, "gardenio", "gardener", true},
		{"empty project/role -> unknown, still recorded", 0.9, MicrovmLaunchSuccess, "", "", true},
		{"negative duration dropped", -1, MicrovmLaunchSuccess, "gardenio", "thread", false},
		{"bad outcome dropped", 0.9, "pending", "gardenio", "thread", false},
	}
	for _, c := range cases {
		if got := m.RecordLaunch(c.seconds, c.outcome, c.project, c.role); got != c.want {
			t.Errorf("%s: RecordLaunch=%v want %v", c.name, got, c.want)
		}
	}

	// Exactly the 3 valid observations above must have landed; the 2 invalid
	// ones must not have created any series.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total uint64
	var series int
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_microvm_launch_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			series++
			total += m.GetHistogram().GetSampleCount()
			if !hasLabel(m, "env", "e2e") {
				t.Errorf("series missing env=e2e label: %v", m.GetLabel())
			}
		}
	}
	if total != 3 {
		t.Errorf("sample count = %d, want 3 (only valid observations land)", total)
	}
	// 3 valid samples across 3 distinct {outcome,project,role} tuples => 3 series.
	if series != 3 {
		t.Errorf("series count = %d, want 3", series)
	}
}

// TestMicrovmLaunchNilReceiver proves a nil metric (MicroVM off / not
// constructed) is a safe no-op, so the spawn door can call it unconditionally.
func TestMicrovmLaunchNilReceiver(t *testing.T) {
	var m *MicrovmLaunchMetrics
	if got := m.RecordLaunch(1.0, MicrovmLaunchSuccess, "gardenio", "thread"); got != false {
		t.Errorf("nil RecordLaunch = %v, want false", got)
	}
	if err := m.Register(prometheus.NewRegistry()); err != nil {
		t.Errorf("nil Register = %v, want nil", err)
	}
}
