package bridge

import (
	"context"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// refusalCounterValue reads the current value of the gardenio_model_refusal_total
// series for a (served_group, served_by_fallback) label pair. Returns 0 if the
// series has not been touched.
func refusalCounterValue(t *testing.T, servedGroup, fallback string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := modelRefusalTotal.GetMetricWithLabelValues(servedGroup, fallback)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q,%q): %v", servedGroup, fallback, err)
	}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

// fallbackCounterValue reads the current value of the gardenio_model_fallback_total
// series for an (expected_group, served_group) label pair.
func fallbackCounterValue(t *testing.T, expectedGroup, servedGroup string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := modelFallbackTotal.GetMetricWithLabelValues(expectedGroup, servedGroup)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q,%q): %v", expectedGroup, servedGroup, err)
	}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestModelGroupLabel_BoundsToGroupOrUnknown(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Raw ids + dated snapshots + provider prefixes all collapse to the tier.
		{"claude-opus-5", "opus"},
		{"claude-opus-5-oauth", "opus"},
		{"anthropic/claude-opus-4-8-20260315", "opus"},
		{"us.anthropic.claude-sonnet-5", "sonnet"},
		{"claude-haiku-4-5", "haiku"},
		{"claude-fable-5-oauth", "fable"},
		// Unclassifiable ids map to the bounded sentinel — never a raw id label.
		{"", modelGroupUnknown},
		{"glm-4-plus", modelGroupUnknown},
		{"gpt-4o", modelGroupUnknown},
	}
	for _, c := range cases {
		if got := modelGroupLabel(c.in); got != c.want {
			t.Errorf("modelGroupLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBoolLabel(t *testing.T) {
	if got := boolLabel(true); got != "true" {
		t.Errorf("boolLabel(true) = %q, want \"true\"", got)
	}
	if got := boolLabel(false); got != "false" {
		t.Errorf("boolLabel(false) = %q, want \"false\"", got)
	}
}

func TestRecordModelRefusal_IncrementsBoundedSeries(t *testing.T) {
	// A refusal served by opus, not via a fallback.
	before := refusalCounterValue(t, "opus", "false")
	recordModelRefusal("claude-opus-5-oauth", false)
	if after := refusalCounterValue(t, "opus", "false"); after != before+1 {
		t.Errorf("opus/false counter = %v, want %v", after, before+1)
	}

	// A refusal served by a fallback model (sonnet) — a distinct series.
	beforeFB := refusalCounterValue(t, "sonnet", "true")
	recordModelRefusal("claude-sonnet-5", true)
	if after := refusalCounterValue(t, "sonnet", "true"); after != beforeFB+1 {
		t.Errorf("sonnet/true counter = %v, want %v", after, beforeFB+1)
	}

	// An unclassifiable served model lands on the unknown sentinel, not a raw id.
	beforeU := refusalCounterValue(t, modelGroupUnknown, "false")
	recordModelRefusal("glm-4-plus", false)
	if after := refusalCounterValue(t, modelGroupUnknown, "false"); after != beforeU+1 {
		t.Errorf("unknown/false counter = %v, want %v", after, beforeU+1)
	}
}

func TestRecordModelFallback_IncrementsTierTransitionSeries(t *testing.T) {
	// An opus -> sonnet downgrade: the actionable silent-switch case.
	before := fallbackCounterValue(t, "opus", "sonnet")
	recordModelFallback("claude-opus-5-oauth", "claude-sonnet-5")
	if after := fallbackCounterValue(t, "opus", "sonnet"); after != before+1 {
		t.Errorf("opus->sonnet counter = %v, want %v", after, before+1)
	}

	// A within-family switch (opus-5 -> opus-4-8, the exact configmap fallback
	// target) is a distinct tier-pair series (opus -> opus).
	beforeIn := fallbackCounterValue(t, "opus", "opus")
	recordModelFallback("claude-opus-5", "claude-opus-4-8")
	if after := fallbackCounterValue(t, "opus", "opus"); after != beforeIn+1 {
		t.Errorf("opus->opus counter = %v, want %v", after, beforeIn+1)
	}
}

// TestModelFallbackFlagger_IncrementsMetric proves the counter fires through the
// REAL public handler path (Flag), not just the private recorder — and that the
// increment is gated by the same per-session dedup as the durable record, so the
// series reconstructs the model-fallback-bead count 1:1 rather than the raw
// usage-turn count.
func TestModelFallbackFlagger_IncrementsMetric(t *testing.T) {
	daemon := &fakeFallbackDaemon{createID: "gd-fb-metric"}
	f := NewModelFallbackFlagger(ModelFallbackConfig{Daemon: daemon, Logger: slog.Default()})
	if f == nil {
		t.Fatal("NewModelFallbackFlagger returned nil")
	}
	evt := &messageEvent{
		EventType:     "usage",
		SessionID:     "sess-metric",
		ExpectedModel: "claude-opus-5-oauth",
		Model:         "claude-sonnet-5",
	}
	before := fallbackCounterValue(t, "opus", "sonnet")
	f.Flag(context.Background(), evt, "worker-metric", "C-thread", "1700000000.111")
	if after := fallbackCounterValue(t, "opus", "sonnet"); after != before+1 {
		t.Fatalf("counter after first Flag = %v, want %v", after, before+1)
	}
	// A second usage turn of the SAME session divergence must NOT double-count —
	// the metric latches with the durable record.
	f.Flag(context.Background(), evt, "worker-metric", "C-thread", "1700000000.222")
	if after := fallbackCounterValue(t, "opus", "sonnet"); after != before+1 {
		t.Errorf("counter after repeat Flag = %v, want %v (deduped per session)", after, before+1)
	}
}

// TestRefusalLogger_IncrementsMetric proves the refusal counter fires through the
// REAL public handler path (Log), latched by the same (agent,session,snippet)
// dedup as the durable refusal record.
func TestRefusalLogger_IncrementsMetric(t *testing.T) {
	daemon := &fakeFallbackDaemon{createID: "gd-refusal-metric"}
	f := NewRefusalLogger(RefusalLogConfig{Daemon: daemon, Logger: slog.Default()})
	if f == nil {
		t.Fatal("NewRefusalLogger returned nil")
	}
	// A refusal served by opus, not via a fallback (served == requested identity).
	evt := &messageEvent{
		EventType:      "refusal",
		SessionID:      "sess-refusal",
		StopReason:     "refusal",
		Model:          "claude-opus-5-oauth",
		RequestedModel: "claude-opus-5",
		Text:           "I can't help with that.",
	}
	before := refusalCounterValue(t, "opus", "false")
	f.Log(context.Background(), evt, "worker-refusal", "C-thread", "1700000000.111")
	if after := refusalCounterValue(t, "opus", "false"); after != before+1 {
		t.Fatalf("counter after first Log = %v, want %v", after, before+1)
	}
	// Repeat of the same refusal snippet in the same session does not double-count.
	f.Log(context.Background(), evt, "worker-refusal", "C-thread", "1700000000.222")
	if after := refusalCounterValue(t, "opus", "false"); after != before+1 {
		t.Errorf("counter after repeat Log = %v, want %v (deduped)", after, before+1)
	}
}
