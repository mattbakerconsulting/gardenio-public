package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sort"
	"sync"
	"time"

	"gardenio/kbeads/internal/metrics"
	"github.com/nats-io/nats.go"
)

// Bus dispatches events to registered handlers and optionally publishes
// events to NATS JetStream for persistence and distributed consumption.
type Bus struct {
	handlers []Handler
	js       nats.JetStreamContext
	mu       sync.RWMutex
}

// New creates a new event bus.
func New() *Bus {
	return &Bus{}
}

// SetJetStream attaches a JetStream context for event publishing.
// When set, Dispatch will publish events to JetStream after running
// local handlers. Publishing is async — errors are logged but do not
// affect handler results.
func (b *Bus) SetJetStream(js nats.JetStreamContext) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.js = js
}

// JetStreamEnabled returns true if JetStream publishing is configured.
func (b *Bus) JetStreamEnabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.js != nil
}

// JetStream returns the JetStream context, or nil if not configured.
func (b *Bus) JetStream() nats.JetStreamContext {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.js
}

// Register adds a handler to the bus. Handlers are sorted by priority on
// each Dispatch call, so registration order does not matter.
func (b *Bus) Register(h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers = append(b.handlers, h)
}

// Unregister removes a handler by ID. Returns true if a handler was removed.
func (b *Bus) Unregister(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, h := range b.handlers {
		if h.ID() == id {
			b.handlers = append(b.handlers[:i], b.handlers[i+1:]...)
			return true
		}
	}
	return false
}

// Dispatch sends an event to all registered handlers that handle its type.
// Handlers are called sequentially in priority order (lowest first).
// Handler errors are logged but do not stop the chain — the bus is resilient.
// If JetStream is configured, the event is published after handler dispatch.
func (b *Bus) Dispatch(ctx context.Context, event *Event) (*Result, error) {
	if event == nil {
		return nil, fmt.Errorf("eventbus: nil event")
	}

	b.mu.RLock()
	matching := b.matchingHandlers(event.Type)
	js := b.js
	b.mu.RUnlock()

	result := &Result{}

	for _, h := range matching {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("eventbus: context canceled: %w", err)
		}

		if err := h.Handle(ctx, event, result); err != nil {
			log.Printf("eventbus: handler %q error for %s: %v", h.ID(), event.Type, err)
		}
	}

	// Emit decision lifecycle + escalation Prometheus counters. Rides the
	// dispatch chokepoint so every decision event is counted exactly once,
	// independent of whether JetStream is configured (customer-1 telemetry
	// plan §5). Identity stays out of the live series — escalation is labelled
	// by role, never operator_id.
	if event.Type.IsDecisionEvent() {
		recordDecisionMetrics(event)
	}

	// Emit agent-lifecycle Prometheus counters (epic E4, ka-0000001152).
	// Rides the same dispatch chokepoint so every agent lifecycle/heartbeat
	// event is counted exactly once. Heartbeats are split into their own
	// high-volume counter; role labels the series, never agent_id.
	if event.Type.IsAgentEvent() {
		recordAgentMetrics(event)
	}

	// Publish to JetStream for persistence (fire-and-forget).
	if js != nil {
		b.publishToJetStream(js, event)
	}

	return result, nil
}

// recordDecisionMetrics bumps the decision lifecycle counter for any decision
// event and, for escalations, the escalation counter. The urgency / role /
// outcome label values are peeked out of the event's raw JSON payload (the
// daemon RPC path sets Raw) and normalized to bounded enums by the metrics
// package. A malformed or absent payload still counts the lifecycle event with
// urgency="unknown" so the four-event invariant holds.
func recordDecisionMetrics(event *Event) {
	eventLabel := decisionEventLabel(event.Type)
	if eventLabel == "" {
		return
	}

	var payload DecisionEventPayload
	if len(event.Raw) > 0 {
		// Best-effort: ignore unmarshal errors and fall back to empty fields,
		// which normalize to the bounded "unknown"/"other"/"requested" buckets.
		_ = json.Unmarshal(event.Raw, &payload)
	}

	metrics.ObserveDecisionLifecycle(eventLabel, payload.Urgency)

	if event.Type == EventDecisionEscalated {
		metrics.ObserveEscalation(payload.EscalatedByRole, payload.Outcome)
	}
}

// recordAgentMetrics bumps the agent-lifecycle counter for any agent event.
// Heartbeats route to gardenio_agents_heartbeats_total (high-volume, split out);
// started/stopped/crashed/idle route to gardenio_agents_lifecycle_total. The
// role label is peeked from the event's raw JSON payload (the daemon RPC path
// sets Raw) and normalized to a bounded enum; a malformed or absent payload
// counts with role="other".
func recordAgentMetrics(event *Event) {
	var payload AgentEventPayload
	if len(event.Raw) > 0 {
		// Best-effort: ignore unmarshal errors and fall back to an empty role,
		// which normalizes to "other".
		_ = json.Unmarshal(event.Raw, &payload)
	}

	if event.Type == EventAgentHeartbeat {
		metrics.ObserveAgentHeartbeat(payload.Role)
		return
	}

	eventLabel := agentEventLabel(event.Type)
	if eventLabel == "" {
		return
	}
	metrics.ObserveAgentLifecycle(eventLabel, payload.Role)
}

// agentEventLabel maps an agent lifecycle EventType onto the bounded metric
// `event` label value. Returns "" for the heartbeat type (counted separately)
// and for non-agent types.
func agentEventLabel(t EventType) string {
	switch t {
	case EventAgentStarted:
		return metrics.AgentEventStarted
	case EventAgentStopped:
		return metrics.AgentEventStopped
	case EventAgentCrashed:
		return metrics.AgentEventCrashed
	case EventAgentIdle:
		return metrics.AgentEventIdle
	}
	return ""
}

// decisionEventLabel maps a decision EventType onto the bounded metric `event`
// label value (1:1 with EventDecisionCreated/Responded/Escalated/Expired).
// Returns "" for non-decision types.
func decisionEventLabel(t EventType) string {
	switch t {
	case EventDecisionCreated:
		return metrics.DecisionEventCreated
	case EventDecisionResponded:
		return metrics.DecisionEventResponded
	case EventDecisionEscalated:
		return metrics.DecisionEventEscalated
	case EventDecisionExpired:
		return metrics.DecisionEventExpired
	}
	return ""
}

// publishToJetStream publishes an event to the appropriate JetStream stream.
// Errors are logged but never propagated — JetStream is supplementary to
// local dispatch, not a prerequisite.
//
// When Raw is set (normal daemon RPC path), the original Claude Code JSON is
// published as-is — this preserves maximum fidelity for external consumers.
// When Raw is empty (programmatic events), the Event struct is marshaled with
// a published_at timestamp.
func (b *Bus) publishToJetStream(js nats.JetStreamContext, event *Event) {
	subject := SubjectForEvent(event.Type)

	// For decision events, extract requested_by from the payload to build a
	// scoped subject (decisions.<requestedBy>.<EventType>). This allows agents
	// to subscribe only to their own decisions.
	if event.Type.IsDecisionEvent() && len(event.Raw) > 0 {
		var peek struct {
			RequestedBy string `json:"requested_by"`
		}
		if json.Unmarshal(event.Raw, &peek) == nil {
			subject = SubjectForDecisionEvent(event.Type, peek.RequestedBy)
		}
	}

	// For hook events (not any specialized category), use agent-scoped subjects.
	// hooks.<actor>.<EventType> allows per-agent subscriptions.
	if isHookEvent(event.Type) {
		subject = SubjectForHookEvent(event.Type, event.Actor)
	}

	// For message events, use agent-scoped subjects.
	// coop.events.messages.<actor>.<EventType> allows per-agent subscriptions.
	if event.Type.IsMessageEvent() {
		subject = SubjectForMessageEvent(event.Type, event.Actor)
	}

	// Use the raw JSON if available, otherwise marshal the event.
	// When Raw is set AND Actor is known, inject actor into the JSON so
	// external NATS subscribers can attribute events to agents.
	var data []byte
	if len(event.Raw) > 0 {
		data = event.Raw
		if event.Actor != "" {
			data = injectActorIntoRaw(data, event.Actor)
		}
	} else {
		now := time.Now().UTC()
		event.PublishedAt = &now
		var err error
		data, err = json.Marshal(event)
		if err != nil {
			log.Printf("eventbus: failed to marshal event for JetStream: %v", err)
			return
		}
	}

	ack, err := js.Publish(subject, data)
	if err != nil {
		log.Printf("eventbus: JetStream publish to %s failed: %v", subject, err)
	} else {
		log.Printf("eventbus: JetStream published to %s (stream=%s seq=%d, %d bytes)",
			subject, ack.Stream, ack.Sequence, len(data))
	}
}

// isHookEvent returns true if the event type is a hook event — i.e., not
// any of the specialized event categories (decision, agent, mail, etc.).
func isHookEvent(t EventType) bool {
	return !t.IsDecisionEvent() && !t.IsAgentEvent() && !t.IsMailEvent() &&
		!t.IsMutationEvent() && !t.IsConfigEvent() && !t.IsGateEvent() &&
		!t.IsJackEvent() && !t.IsMessageEvent()
}

// PublishRaw publishes arbitrary JSON data to a JetStream subject.
// Used by non-hook event producers (e.g., mutation events) that don't flow
// through the Dispatch handler chain. Returns silently if JetStream is not
// enabled.
func (b *Bus) PublishRaw(subject string, data []byte) {
	b.mu.RLock()
	js := b.js
	b.mu.RUnlock()

	if js == nil {
		return
	}

	ack, err := js.Publish(subject, data)
	if err != nil {
		log.Printf("eventbus: JetStream publish to %s failed: %v", subject, err)
	} else {
		log.Printf("eventbus: JetStream published to %s (stream=%s seq=%d, %d bytes)",
			subject, ack.Stream, ack.Sequence, len(data))
	}
}

// PublishDeduped publishes arbitrary JSON data to a JetStream subject with a
// deterministic dedupe message id (JetStream's Nats-Msg-Id header). When the
// target stream declares a Duplicates window (PLAN_EVENTS), a second publish of
// the SAME msgID landing inside that window is silently dropped by the server —
// giving at-least-once delivery WITHOUT at-least-once side effects. Used by the
// plan step-added signal, where the daemon may re-emit the same (plan, step)
// edge (a double-write, a reconnect replay, or an ON CONFLICT DO NOTHING
// re-insert) but the consumer must nudge exactly once.
//
// Returns silently if JetStream is not enabled. A duplicate is not an error —
// ack.Duplicate is logged so the dedupe path is observable but never alarms.
func (b *Bus) PublishDeduped(subject, msgID string, data []byte) {
	b.mu.RLock()
	js := b.js
	b.mu.RUnlock()

	if js == nil {
		return
	}

	ack, err := js.Publish(subject, data, nats.MsgId(msgID))
	if err != nil {
		log.Printf("eventbus: JetStream deduped publish to %s (msg-id=%s) failed: %v", subject, msgID, err)
		return
	}
	if ack.Duplicate {
		log.Printf("eventbus: JetStream deduped publish to %s (msg-id=%s) dropped as duplicate (stream=%s)",
			subject, msgID, ack.Stream)
		return
	}
	log.Printf("eventbus: JetStream deduped published to %s (msg-id=%s stream=%s seq=%d, %d bytes)",
		subject, msgID, ack.Stream, ack.Sequence, len(data))
}

// Handlers returns all registered handlers (for introspection/status reporting).
func (b *Bus) Handlers() []Handler {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Handler, len(b.handlers))
	copy(out, b.handlers)
	return out
}

// injectActorIntoRaw adds or overwrites the "actor" field in a JSON object.
// Returns the original data unchanged if parsing fails.
func injectActorIntoRaw(raw json.RawMessage, actor string) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return raw
	}
	obj["actor"] = actorJSON
	enriched, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return enriched
}

// matchingHandlers returns handlers that handle the given event type, sorted
// by priority (lowest first). Must be called with at least a read lock held.
func (b *Bus) matchingHandlers(eventType EventType) []Handler {
	var matched []Handler
	for _, h := range b.handlers {
		if slices.Contains(h.Handles(), eventType) {
			matched = append(matched, h)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Priority() < matched[j].Priority()
	})
	return matched
}
