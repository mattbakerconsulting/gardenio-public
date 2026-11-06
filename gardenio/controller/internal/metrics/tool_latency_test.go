package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestRecordToolLatency_EnumGuards guards gd-sWwPnwkZtw: an out-of-enum
// source is dropped, so tool-latency series cardinality stays bounded.
func TestRecordToolLatency_EnumGuards(t *testing.T) {
	m := NewToolLatencyMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name                        string
		seconds                     float64
		tool, project, harness, src string
		want                        bool
	}{
		{"valid claude", 0.5, "Read", "gardenio", "claude", ToolLatencySourceClaude, true},
		{"valid derived", 3.2, "Bash", "gardenio", "claude", ToolLatencySourceDerived, true},
		{"empty tool -> unknown, still recorded", 1.0, "", "gardenio", "claude", ToolLatencySourceClaude, true},
		{"empty project -> unknown, still recorded", 1.0, "Read", "", "claude", ToolLatencySourceClaude, true},
		{"negative duration dropped", -1, "Read", "gardenio", "claude", ToolLatencySourceClaude, false},
		{"bad source dropped", 1.0, "Read", "gardenio", "claude", "bogus", false},
	}
	for _, c := range cases {
		if got := m.RecordToolLatency(c.seconds, c.tool, c.project, c.harness, c.src); got != c.want {
			t.Errorf("%s: RecordToolLatency=%v want %v", c.name, got, c.want)
		}
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total uint64
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_tool_latency_seconds" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			total += mm.GetHistogram().GetSampleCount()
			if !hasLabel(mm, "env", "e2e") {
				t.Errorf("series missing env=e2e label: %v", mm.GetLabel())
			}
		}
	}
	if total != 4 {
		t.Errorf("total samples = %d, want 4 (the 4 valid cases)", total)
	}
}

// TestRecordBashCmdLatency_EnumGuards guards gd-sWwPnwkZtw: an out-of-enum
// source is dropped, so bash-command latency series cardinality stays bounded.
func TestRecordBashCmdLatency_EnumGuards(t *testing.T) {
	m := NewToolLatencyMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name                            string
		seconds                         float64
		cmdClass, project, harness, src string
		want                            bool
	}{
		{"valid", 3.0, "git_push", "gardenio", "claude", ToolLatencySourceClaude, true},
		{"empty project -> unknown, still recorded", 1.0, "make", "", "claude", ToolLatencySourceDerived, true},
		{"negative duration dropped", -1, "git_push", "gardenio", "claude", ToolLatencySourceClaude, false},
		{"empty cmd_class dropped", 1.0, "", "gardenio", "claude", ToolLatencySourceClaude, false},
		{"bad source dropped", 1.0, "git_push", "gardenio", "claude", "bogus", false},
	}
	for _, c := range cases {
		if got := m.RecordBashCmdLatency(c.seconds, c.cmdClass, c.project, c.harness, c.src); got != c.want {
			t.Errorf("%s: RecordBashCmdLatency=%v want %v", c.name, got, c.want)
		}
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total uint64
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_bash_cmd_latency_seconds" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			total += mm.GetHistogram().GetSampleCount()
		}
	}
	if total != 2 {
		t.Errorf("total samples = %d, want 2 (the 2 valid cases)", total)
	}
}

func TestToolLatencyMetrics_NilReceiverSafe(t *testing.T) {
	var m *ToolLatencyMetrics
	if err := m.Register(prometheus.NewRegistry()); err != nil {
		t.Errorf("nil Register should be a no-op, got %v", err)
	}
	if m.RecordToolLatency(1.0, "Read", "gardenio", "claude", ToolLatencySourceClaude) {
		t.Error("nil RecordToolLatency should return false")
	}
	if m.RecordBashCmdLatency(1.0, "git_push", "gardenio", "claude", ToolLatencySourceClaude) {
		t.Error("nil RecordBashCmdLatency should return false")
	}
}

func TestToolLatencyMetrics_HarnessLabelBounded(t *testing.T) {
	m := NewToolLatencyMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, h := range []string{"codex", "claude", "", "gemini", "gpt", "x\ny"} {
		m.RecordToolLatency(0.1, "Read", "gardenio", h, ToolLatencySourceClaude)
		m.RecordBashCmdLatency(0.1, "git_push", "gardenio", h, ToolLatencySourceClaude)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]uint64{}
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_tool_latency_seconds" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			for _, l := range mm.GetLabel() {
				if l.GetName() == "harness" {
					got[l.GetValue()] += mm.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	// empty folds into claude (pre-harness gb); unrecognized values collapse to
	// "unknown" so the label can never echo an arbitrary payload string.
	want := map[string]uint64{"codex": 1, "claude": 2, "gemini": 1, "unknown": 2}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("harness=%q samples = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("harness label values = %v, want exactly %v", got, want)
	}
}
