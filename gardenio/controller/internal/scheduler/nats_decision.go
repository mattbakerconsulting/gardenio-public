package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// DecisionEventListener subscribes to decision-resolution events on NATS and
// forwards them into the scheduler's EntryEngine via OnDecisionResolved.
// This is the controller-side ingestion path for trigger_mode=decision_resolved
// schedules: kbeads emits a "DecisionResponded" event when a decision is
// resolved (subject: decisions.<scope>.DecisionResponded), and the scheduler's
// reconcile loop then matches buffered events against decision_resolved
// trigger entries.
//
// The listener is process-local; it relies on the running scheduler's
// EntryEngine to do the matching and fire decisions, so a restart drops
// in-flight events but does not lose schedule state (catch-up is bounded by
// each schedule's `last_run` plus the reconcile-tick re-evaluation).
type DecisionEventListener struct {
	engine *EntryEngine
	logger *slog.Logger

	conn *nats.Conn
	sub  *nats.Subscription
}

// NewDecisionEventListener wires a listener that pushes incoming decision
// events into engine.OnDecisionResolved. The engine reference must outlive
// the listener.
func NewDecisionEventListener(engine *EntryEngine, logger *slog.Logger) *DecisionEventListener {
	return &DecisionEventListener{
		engine: engine,
		logger: logger.With("component", "decision-event-listener"),
	}
}

// natsDecisionPayload mirrors kbeads/internal/eventbus.DecisionEventPayload.
// Defined locally so the controller does not have to import kbeads internals.
type natsDecisionPayload struct {
	DecisionID  string `json:"decision_id"`
	Question    string `json:"question"`
	Urgency     string `json:"urgency,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
	Options     int    `json:"option_count"`
	ChosenIndex int    `json:"chosen_index,omitempty"`
	ChosenLabel string `json:"chosen_label,omitempty"`
	ResolvedBy  string `json:"resolved_by,omitempty"`
	Rationale   string `json:"rationale,omitempty"`
	// Watcher metadata. The decision bead's own routing fields are emitted
	// alongside the canonical eventbus payload so trigger_mode=decision_resolved
	// schedules can match without a daemon round-trip. All optional —
	// schedules with empty watch_* fields treat these as wildcards.
	Project string `json:"project,omitempty"`
	Role    string `json:"role,omitempty"`
	BeadID  string `json:"bead_id,omitempty"`
	// Labels accompany the event so field leaves like "label:project:foo" can
	// match without a separate fetch.
	Labels []string `json:"labels,omitempty"`
}

// Start connects to NATS and subscribes to decision-resolved events.
// Blocks until ctx is cancelled, mirroring probeEventListener.Start.
func (l *DecisionEventListener) Start(ctx context.Context, natsURL, natsToken, natsUser string) error {
	if natsURL == "" {
		l.logger.Info("decision event listener disabled (no NATS URL)")
		return nil
	}
	if l.engine == nil {
		l.logger.Info("decision event listener disabled (no trigger engine)")
		return nil
	}

	opts := []nats.Option{
		nats.Name("gardenio-controller-decision-events"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				l.logger.Warn("decision event listener: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			l.logger.Info("decision event listener: NATS reconnected")
		}),
	}
	opts = append(opts, natsauth.Options(natsUser, natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return err
	}
	defer conn.Close()
	l.conn = conn

	// Subscribe to decisions.<scope>.DecisionResponded — kbeads emits the
	// EventDecisionResponded event when a decision is resolved (see
	// kbeads/internal/eventbus/streams.go SubjectForDecisionEvent).
	sub, err := conn.Subscribe("decisions.*.DecisionResponded", l.handle)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe() //nolint:errcheck // best-effort cleanup on shutdown
	l.sub = sub

	l.logger.Info("decision event listener subscribed",
		"subject", "decisions.*.DecisionResponded")
	<-ctx.Done()
	return nil
}

// handle decodes one decision event and pushes it into the trigger engine.
// Malformed payloads are silently dropped (matching probeEventListener).
func (l *DecisionEventListener) handle(msg *nats.Msg) {
	var evt natsDecisionPayload
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		l.logger.Debug("decision event: malformed payload", "error", err)
		return
	}
	if evt.DecisionID == "" {
		// Without a decision id we have nothing to forward usefully.
		return
	}

	payload := map[string]any{
		"decision_id":   evt.DecisionID,
		"question":      evt.Question,
		"urgency":       evt.Urgency,
		"requested_by":  evt.RequestedBy,
		"option_count":  evt.Options,
		"chosen_index":  evt.ChosenIndex,
		"chosen_label":  evt.ChosenLabel,
		"resolved_by":   evt.ResolvedBy,
		"rationale":     evt.Rationale,
		"watch_project": evt.Project,
		"watch_role":    evt.Role,
		"watch_bead_id": evt.BeadID,
	}
	l.engine.OnDecisionResolved(evt.DecisionID, payload, evt.Labels)
	l.logger.Debug("decision event forwarded to trigger engine",
		"decision_id", evt.DecisionID,
		"project", evt.Project,
		"role", evt.Role,
		"bead_id", evt.BeadID)
}
