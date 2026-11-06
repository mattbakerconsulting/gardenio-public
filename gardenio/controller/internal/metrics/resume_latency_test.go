package metrics

import (
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestRecordResumeLatency_EnumGuards guards ka-0000003577: an out-of-enum
// label value is dropped, so resume-latency series cardinality stays bounded.
func TestRecordResumeLatency_EnumGuards(t *testing.T) {
	rm := NewResumeMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := rm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name                        string
		seconds                     float64
		regime, role, runtime, path string
		want                        bool
	}{
		{"valid R4 warm", 0.42, ResumeRegimeR4, "crew", ResumeRuntimeRunsc, ResumePathThawWarm, true},
		{"valid R3 fresh", 1.3, ResumeRegimeR3, "gardener", ResumeRuntimeRunsc, ResumePathThawFresh, true},
		{"valid microvm-resume", 12.5, ResumeRegimeMicrovm, "gardenio-microvms", ResumeRuntimeMicrovm, ResumePathMicrovmResume, true},
		{"empty role -> unknown, still recorded", 0.5, ResumeRegimeR4, "", ResumeRuntimeRunsc, ResumePathThawWarm, true},
		{"negative duration dropped", -1, ResumeRegimeR4, "crew", ResumeRuntimeRunsc, ResumePathThawWarm, false},
		{"bad regime dropped", 0.5, "R9", "crew", ResumeRuntimeRunsc, ResumePathThawWarm, false},
		{"bad path dropped", 0.5, ResumeRegimeR4, "crew", ResumeRuntimeRunsc, "teleport", false},
		{"bad runtime dropped", 0.5, ResumeRegimeR4, "crew", "kata", ResumePathThawWarm, false},
	}
	for _, c := range cases {
		if got := rm.RecordResumeLatency(c.seconds, c.regime, c.role, c.runtime, c.path); got != c.want {
			t.Errorf("%s: RecordResumeLatency=%v want %v", c.name, got, c.want)
		}
	}

	// Exactly the 4 valid observations above must have landed; the 4 invalid
	// ones must not have created any series.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total uint64
	var series int
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_resume_latency_seconds" {
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
	if total != 4 {
		t.Errorf("expected 4 recorded samples, got %d", total)
	}
	// 4 distinct label tuples: (R4,crew,warm), (R3,gardener,fresh),
	// (microvm,gardenio-microvms,microvm-resume), (R4,unknown,warm).
	if series != 4 {
		t.Errorf("expected 4 distinct series, got %d", series)
	}
}

// TestRecordMicrovmSuspend_EnumGuards covers the step-6 suspend counter: valid
// outcomes land, an out-of-enum outcome is dropped, and an empty role collapses
// to "unknown" (ka-0000002861).
func TestRecordMicrovmSuspend_EnumGuards(t *testing.T) {
	rm := NewResumeMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := rm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name          string
		role, outcome string
		want          bool
	}{
		{"valid suspended", "gardenio-microvms", MicrovmSuspendOutcomeSuspended, true},
		{"valid fallback-terminated", "gardenio-microvms", MicrovmSuspendOutcomeFallbackTerminated, true},
		{"empty role -> unknown, still counted", "", MicrovmSuspendOutcomeSuspended, true},
		{"bad outcome dropped", "gardenio-microvms", "hibernated", false},
	}
	for _, c := range cases {
		if got := rm.RecordMicrovmSuspend(c.role, c.outcome); got != c.want {
			t.Errorf("%s: RecordMicrovmSuspend=%v want %v", c.name, got, c.want)
		}
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	var series int
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_microvm_suspends_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			series++
			total += m.GetCounter().GetValue()
			if !hasLabel(m, "env", "e2e") {
				t.Errorf("series missing env=e2e label: %v", m.GetLabel())
			}
		}
	}
	// 3 valid increments (bad outcome dropped) across 3 distinct {role,outcome}
	// tuples: (gardenio-microvms,suspended), (gardenio-microvms,fallback-terminated),
	// (unknown,suspended).
	if total != 3 {
		t.Errorf("expected 3 counted suspends, got %v", total)
	}
	if series != 3 {
		t.Errorf("expected 3 distinct series, got %d", series)
	}
}

// TestRecordMicrovmSuspend_NilReceiverSafe: a nil metric (MicroVM off) is a safe
// no-op so the teardown branch can call it unconditionally.
func TestRecordMicrovmSuspend_NilReceiverSafe(t *testing.T) {
	var rm *ResumeMetrics
	if rm.RecordMicrovmSuspend("crew", MicrovmSuspendOutcomeSuspended) {
		t.Error("nil receiver must report false (no-op)")
	}
}

func TestRecordResumeLatency_NilReceiverSafe(t *testing.T) {
	var rm *ResumeMetrics
	if rm.RecordResumeLatency(0.5, ResumeRegimeR4, "crew", ResumeRuntimeRunsc, ResumePathThawWarm) {
		t.Error("nil receiver must report false (no-op)")
	}
	if err := rm.Register(prometheus.NewRegistry()); err != nil {
		t.Errorf("nil Register must be a no-op, got %v", err)
	}
}

func TestResumeMetrics_EmptyRoleCollapsesToUnknown(t *testing.T) {
	rm := NewResumeMetrics("")
	reg := prometheus.NewRegistry()
	_ = rm.Register(reg)
	rm.RecordResumeLatency(0.3, ResumeRegimeR4, "", ResumeRuntimeRunsc, ResumePathThawWarm)
	mfs, _ := reg.Gather()
	found := false
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_resume_latency_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if hasLabel(m, "role", "unknown") {
				found = true
			}
		}
	}
	if !found {
		t.Error("empty role must be recorded as role=unknown")
	}
}

func hasLabel(m *dto.Metric, name, value string) bool {
	for _, l := range m.GetLabel() {
		if l.GetName() == name && l.GetValue() == value {
			return true
		}
	}
	return false
}

// Guard: the metric name in the plan prose is "_ms"-flavored but the histogram
// unit is seconds (Prometheus convention). Pin the name so a rename is a
// deliberate, reviewed change — dashboards/alerts query this string.
func TestResumeMetricName(t *testing.T) {
	rm := NewResumeMetrics("e2e")
	reg := prometheus.NewRegistry()
	_ = rm.Register(reg)
	rm.RecordResumeLatency(0.5, ResumeRegimeR4, "crew", ResumeRuntimeRunsc, ResumePathThawWarm)
	mfs, _ := reg.Gather()
	var names []string
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	if !slices.Contains(names, "gardenio_resume_latency_seconds") {
		t.Errorf("expected gardenio_resume_latency_seconds, got %s", strings.Join(names, ","))
	}
}
