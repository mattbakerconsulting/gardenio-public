package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNormalizeBeadEvent(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"created", BeadEventCreated},
		{"CREATED", BeadEventCreated},
		{"  status_changed ", BeadEventStatus},
		{"claimed", BeadEventClaimed},
		{"released", BeadEventReleased},
		{"jack_on", BeadEventJackOn},
		{"", BeadEventOther},
		{"banana", BeadEventOther},
		{"bead.created", BeadEventOther}, // raw topic form is not a label
	}
	for _, tt := range tests {
		if got := NormalizeBeadEvent(tt.in); got != tt.want {
			t.Errorf("NormalizeBeadEvent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeBeadType(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"epic", "epic"},
		{"TASK", "task"},
		{"  decision ", "decision"},
		{"agent", "agent"},
		{"", "other"},
		{"widget-type", "other"}, // custom type buckets to other
		{"mystery", "other"},
	}
	for _, tt := range tests {
		if got := NormalizeBeadType(tt.in); got != tt.want {
			t.Errorf("NormalizeBeadType(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeProject(t *testing.T) {
	if got := normalizeProject(""); got != "unknown" {
		t.Errorf("normalizeProject(empty) = %q, want unknown", got)
	}
	if got := normalizeProject("  gardenio "); got != "gardenio" {
		t.Errorf("normalizeProject trims = %q, want gardenio", got)
	}
}

// TestObserveBeadEvent verifies the counter increments under normalized labels,
// including the catch-all bucketing of unknown event/type/project values.
func TestObserveBeadEvent(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(BeadEvents)
	t.Cleanup(func() { BeadEvents.Reset() })

	ObserveBeadEvent(BeadEventCreated, "gardenio", "task")
	ObserveBeadEvent(BeadEventCreated, "gardenio", "task")
	ObserveBeadEvent("banana", "", "widget-type") // → other / unknown / other

	const want = `
# HELP gardenio_beads_events_total Count of bead-domain events emitted by kbeads, by event sub-type, project, and bead type.
# TYPE gardenio_beads_events_total counter
gardenio_beads_events_total{bead_type="other",event="other",project="unknown"} 1
gardenio_beads_events_total{bead_type="task",event="created",project="gardenio"} 2
`
	if err := testutil.CollectAndCompare(BeadEvents, strings.NewReader(want)); err != nil {
		t.Errorf("unexpected metric state:\n%v", err)
	}
}

func TestObserveAgentLifecycleAndHeartbeat(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(AgentLifecycle, AgentHeartbeats)
	t.Cleanup(func() {
		AgentLifecycle.Reset()
		AgentHeartbeats.Reset()
	})

	ObserveAgentLifecycle(AgentEventStarted, "crew")
	ObserveAgentLifecycle(AgentEventCrashed, "gardener")
	ObserveAgentLifecycle("", "crew") // skipped: empty event
	ObserveAgentHeartbeat("crew")
	ObserveAgentHeartbeat("crew")

	if got := testutil.ToFloat64(AgentLifecycle.WithLabelValues(AgentEventStarted, "crew")); got != 1 {
		t.Errorf("lifecycle started/crew = %v, want 1", got)
	}
	if got := testutil.ToFloat64(AgentLifecycle.WithLabelValues(AgentEventCrashed, "gardener")); got != 1 {
		t.Errorf("lifecycle crashed/gardener = %v, want 1", got)
	}
	if got := testutil.ToFloat64(AgentHeartbeats.WithLabelValues("crew")); got != 2 {
		t.Errorf("heartbeats crew = %v, want 2", got)
	}
}

// TestObserveBeadCreatedByOperator verifies the per-operator created counter
// increments under the operator label and defaults an empty label to "unknown"
// so a create is never silently dropped.
func TestObserveBeadCreatedByOperator(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(BeadsCreatedByOperator)
	t.Cleanup(func() { BeadsCreatedByOperator.Reset() })

	ObserveBeadCreatedByOperator("slack:U123")
	ObserveBeadCreatedByOperator("slack:U123")
	ObserveBeadCreatedByOperator("agent")
	ObserveBeadCreatedByOperator("") // → unknown

	const want = `
# HELP gardenio_beads_created_by_operator_total Count of bead.created events by resolved operator (human id, or one of agent/system/fleet/unknown). One series per human who creates beads.
# TYPE gardenio_beads_created_by_operator_total counter
gardenio_beads_created_by_operator_total{operated_by="agent"} 1
gardenio_beads_created_by_operator_total{operated_by="slack:U123"} 2
gardenio_beads_created_by_operator_total{operated_by="unknown"} 1
`
	if err := testutil.CollectAndCompare(BeadsCreatedByOperator, strings.NewReader(want)); err != nil {
		t.Errorf("unexpected metric state:\n%v", err)
	}
}
