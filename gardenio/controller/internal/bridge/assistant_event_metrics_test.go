package bridge

import (
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue reads the current value of the assistantEventTotal series for a
// given event label. Returns 0 if the series has not been touched.
func counterValue(t *testing.T, event string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := assistantEventTotal.GetMetricWithLabelValues(event)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", event, err)
	}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestRecordAssistantEvent_ValidEnumIncrements(t *testing.T) {
	b := &Bot{logger: slog.Default()}

	for _, event := range []string{
		assistantEventThreadStarted,
		assistantEventContextChanged,
		assistantEventMessage,
	} {
		before := counterValue(t, event)
		if ok := b.recordAssistantEvent(event); !ok {
			t.Fatalf("recordAssistantEvent(%q) returned false for valid enum member", event)
		}
		after := counterValue(t, event)
		if after != before+1 {
			t.Errorf("event %q: counter = %v, want %v", event, after, before+1)
		}
	}
}

func TestRecordAssistantEvent_InvalidEventDropped(t *testing.T) {
	b := &Bot{logger: slog.Default()}

	if ok := b.recordAssistantEvent("not_a_real_event"); ok {
		t.Error("recordAssistantEvent accepted an out-of-enum event; want drop")
	}
}

func TestAssistantEventValid(t *testing.T) {
	valid := []string{assistantEventThreadStarted, assistantEventContextChanged, assistantEventMessage}
	for _, e := range valid {
		if !assistantEventValid(e) {
			t.Errorf("assistantEventValid(%q) = false, want true", e)
		}
	}
	for _, e := range []string{"", "spawn", "Message", "thread_started "} {
		if assistantEventValid(e) {
			t.Errorf("assistantEventValid(%q) = true, want false", e)
		}
	}
}

// deliveryCounterValue reads the current value of the mentionDeliveryTotal
// series for a given delivery_path label. Returns 0 if untouched.
func deliveryCounterValue(t *testing.T, path string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := mentionDeliveryTotal.GetMetricWithLabelValues(path)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", path, err)
	}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestRecordMentionDelivery_ValidEnumIncrements(t *testing.T) {
	b := &Bot{logger: slog.Default()}

	for _, path := range []string{mentionDeliveryWarm, mentionDeliveryCold} {
		before := deliveryCounterValue(t, path)
		if ok := b.recordMentionDelivery(path); !ok {
			t.Fatalf("recordMentionDelivery(%q) returned false for valid enum member", path)
		}
		after := deliveryCounterValue(t, path)
		if after != before+1 {
			t.Errorf("path %q: counter = %v, want %v", path, after, before+1)
		}
	}
}

func TestRecordMentionDelivery_InvalidPathDropped(t *testing.T) {
	b := &Bot{logger: slog.Default()}

	if ok := b.recordMentionDelivery("lukewarm"); ok {
		t.Error("recordMentionDelivery accepted an out-of-enum delivery_path; want drop")
	}
}

func TestMentionDeliveryPathValid(t *testing.T) {
	for _, p := range []string{mentionDeliveryWarm, mentionDeliveryCold} {
		if !mentionDeliveryPathValid(p) {
			t.Errorf("mentionDeliveryPathValid(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "Warm", "cold ", "respawn"} {
		if mentionDeliveryPathValid(p) {
			t.Errorf("mentionDeliveryPathValid(%q) = true, want false", p)
		}
	}
}
