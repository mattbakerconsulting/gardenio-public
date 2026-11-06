package eventbus

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// rawDecision builds a decision event with a JSON payload, mirroring the
// daemon RPC path that sets Event.Raw from the incoming request body.
func rawDecision(t *testing.T, typ EventType, p DecisionEventPayload) *Event {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &Event{Type: typ, Raw: raw}
}

// TestDispatchEmitsDecisionLifecycleAllFourEvents verifies the lifecycle
// counter increments on each of the four decision events, with urgency
// normalized to the bounded enum.
func TestDispatchEmitsDecisionLifecycleAllFourEvents(t *testing.T) {
	metrics.DecisionsLifecycle.Reset()
	metrics.Escalations.Reset()

	b := New()
	ctx := context.Background()

	events := []struct {
		typ     EventType
		urgency string
		label   string
		bucket  string
	}{
		{EventDecisionCreated, "low", metrics.DecisionEventCreated, metrics.UrgencyLow},
		{EventDecisionResponded, "medium", metrics.DecisionEventResponded, metrics.UrgencyMedium},
		{EventDecisionEscalated, "high", metrics.DecisionEventEscalated, metrics.UrgencyHigh},
		{EventDecisionExpired, "critical", metrics.DecisionEventExpired, metrics.UrgencyCritical},
	}

	for _, e := range events {
		evt := rawDecision(t, e.typ, DecisionEventPayload{
			DecisionID: "gd-test",
			Urgency:    e.urgency,
			// EscalatedByRole only meaningful for the escalation event.
			EscalatedByRole: "crew",
			Outcome:         "approved",
		})
		if _, err := b.Dispatch(ctx, evt); err != nil {
			t.Fatalf("Dispatch(%s): %v", e.typ, err)
		}
	}

	for _, e := range events {
		got := testutil.ToFloat64(metrics.DecisionsLifecycle.WithLabelValues(e.label, e.bucket))
		if got != 1 {
			t.Errorf("lifecycle{event=%q,urgency=%q} = %v, want 1", e.label, e.bucket, got)
		}
	}

	if total := testutil.CollectAndCount(metrics.DecisionsLifecycle); total != 4 {
		t.Errorf("expected 4 distinct lifecycle series, got %d", total)
	}
}

// TestDispatchEmitsEscalationOnlyOnEscalated verifies the escalation counter
// fires solely on DecisionEscalated, labelled by role (not operator_id) and
// outcome from the payload.
func TestDispatchEmitsEscalationOnlyOnEscalated(t *testing.T) {
	metrics.DecisionsLifecycle.Reset()
	metrics.Escalations.Reset()

	b := New()
	ctx := context.Background()

	// A created event must NOT bump the escalation counter.
	if _, err := b.Dispatch(ctx, rawDecision(t, EventDecisionCreated,
		DecisionEventPayload{DecisionID: "gd-c", Urgency: "low"})); err != nil {
		t.Fatalf("dispatch created: %v", err)
	}
	if total := testutil.CollectAndCount(metrics.Escalations); total != 0 {
		t.Fatalf("escalation counter should be empty after a created event, got %d series", total)
	}

	// An escalation event bumps it, keyed by role + outcome.
	if _, err := b.Dispatch(ctx, rawDecision(t, EventDecisionEscalated,
		DecisionEventPayload{DecisionID: "gd-e", Urgency: "high", EscalatedByRole: "gardener", Outcome: "requested"})); err != nil {
		t.Fatalf("dispatch escalated: %v", err)
	}

	if n := testutil.ToFloat64(metrics.Escalations.WithLabelValues(metrics.RoleGardener, metrics.OutcomeRequested)); n != 1 {
		t.Errorf("escalations{role=gardener,outcome=requested} = %v, want 1", n)
	}
}

// TestDispatchDecisionMalformedPayloadStillCounts verifies a missing/garbled
// payload still counts the lifecycle event (urgency → unknown), preserving the
// four-event invariant even when Raw is absent or unparseable.
func TestDispatchDecisionMalformedPayloadStillCounts(t *testing.T) {
	metrics.DecisionsLifecycle.Reset()
	metrics.Escalations.Reset()

	b := New()
	ctx := context.Background()

	// No Raw at all (programmatic event).
	if _, err := b.Dispatch(ctx, &Event{Type: EventDecisionCreated}); err != nil {
		t.Fatalf("dispatch no-raw: %v", err)
	}
	// Garbled Raw.
	if _, err := b.Dispatch(ctx, &Event{Type: EventDecisionExpired, Raw: json.RawMessage("{not json")}); err != nil {
		t.Fatalf("dispatch bad-raw: %v", err)
	}

	if n := testutil.ToFloat64(metrics.DecisionsLifecycle.WithLabelValues(metrics.DecisionEventCreated, metrics.UrgencyUnknown)); n != 1 {
		t.Errorf("created/unknown = %v, want 1", n)
	}
	if n := testutil.ToFloat64(metrics.DecisionsLifecycle.WithLabelValues(metrics.DecisionEventExpired, metrics.UrgencyUnknown)); n != 1 {
		t.Errorf("expired/unknown = %v, want 1", n)
	}
}

// TestDispatchNonDecisionEventNoMetrics verifies non-decision events do not
// touch the decision counters.
func TestDispatchNonDecisionEventNoMetrics(t *testing.T) {
	metrics.DecisionsLifecycle.Reset()
	metrics.Escalations.Reset()

	b := New()
	if _, err := b.Dispatch(context.Background(), &Event{Type: EventPreToolUse}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if total := testutil.CollectAndCount(metrics.DecisionsLifecycle); total != 0 {
		t.Errorf("non-decision event bumped lifecycle counter: %d series", total)
	}
	if total := testutil.CollectAndCount(metrics.Escalations); total != 0 {
		t.Errorf("non-decision event bumped escalation counter: %d series", total)
	}
}

// TestDecisionEventLabel maps each decision EventType 1:1 to its bounded label
// and returns "" for non-decision types.
func TestDecisionEventLabel(t *testing.T) {
	cases := map[EventType]string{
		EventDecisionCreated:   metrics.DecisionEventCreated,
		EventDecisionResponded: metrics.DecisionEventResponded,
		EventDecisionEscalated: metrics.DecisionEventEscalated,
		EventDecisionExpired:   metrics.DecisionEventExpired,
		EventPreToolUse:        "",
		EventSessionStart:      "",
	}
	for typ, want := range cases {
		if got := decisionEventLabel(typ); got != want {
			t.Errorf("decisionEventLabel(%q) = %q, want %q", typ, got, want)
		}
	}
}
