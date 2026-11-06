package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// BeadClosedEventListener subscribes to bead-close lifecycle events on NATS
// and forwards them into the scheduler's EntryEngine via OnBeadEvent.
// This is the controller-side ingestion path for trigger_mode=bead_closed
// schedules: kbeads emits a "beads.bead.closed" event when any bead
// transitions to closed (see kbeads/internal/events/events.go), and the
// scheduler's reconcile loop then matches buffered events against the
// schedule's watcher fields.
//
// The listener is process-local; it relies on the running scheduler's
// EntryEngine to do the matching and fire decisions, so a restart drops
// in-flight events but does not lose schedule state (catch-up is bounded by
// each schedule's `last_run` plus the reconcile-tick re-evaluation).
type BeadClosedEventListener struct {
	engine *EntryEngine
	logger *slog.Logger

	conn *nats.Conn
	sub  *nats.Subscription
}

// NewBeadClosedEventListener wires a listener that pushes incoming
// bead-close events into engine.OnBeadEvent. The engine reference must
// outlive the listener.
func NewBeadClosedEventListener(engine *EntryEngine, logger *slog.Logger) *BeadClosedEventListener {
	return &BeadClosedEventListener{
		engine: engine,
		logger: logger.With("component", "bead-closed-event-listener"),
	}
}

// natsBeadClosedPayload mirrors kbeads/internal/events.BeadClosed. Defined
// locally so the controller doesn't have to import kbeads internals.
type natsBeadClosedPayload struct {
	Bead           *natsBeadClosedBead `json:"bead"`
	ClosedBy       string              `json:"closed_by,omitempty"`
	DecisionAnswer *natsDecisionAnswer `json:"decision_answer,omitempty"`
}

// natsBeadClosedBead is a minimal projection of model.Bead carrying just
// the fields the trigger engine needs to evaluate watcher matches. Extra
// fields published by the daemon are ignored.
type natsBeadClosedBead struct {
	ID     string   `json:"id"`
	Type   string   `json:"type"`
	Title  string   `json:"title,omitempty"`
	Status string   `json:"status,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// natsDecisionAnswer mirrors kbeads/internal/events.DecisionAnswer so
// trigger:bead watchers can match on the chosen option for resolved
// decisions. See ka-0000005470 (GAP-4).
type natsDecisionAnswer struct {
	Chosen       string `json:"chosen,omitempty"`
	ResponseText string `json:"response_text,omitempty"`
	RespondedBy  string `json:"responded_by,omitempty"`
	RespondedAt  string `json:"responded_at,omitempty"`
	Cancelled    bool   `json:"cancelled,omitempty"`
}

// Start connects to NATS and subscribes to bead-close events.
// Blocks until ctx is cancelled, mirroring DecisionEventListener.Start.
func (l *BeadClosedEventListener) Start(ctx context.Context, natsURL, natsToken, natsUser string) error {
	if natsURL == "" {
		l.logger.Info("bead-closed event listener disabled (no NATS URL)")
		return nil
	}
	if l.engine == nil {
		l.logger.Info("bead-closed event listener disabled (no trigger engine)")
		return nil
	}

	opts := []nats.Option{
		nats.Name("gardenio-controller-bead-closed-events"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				l.logger.Warn("bead-closed event listener: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			l.logger.Info("bead-closed event listener: NATS reconnected")
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

	sub, err := conn.Subscribe("beads.bead.closed", l.handle)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe() //nolint:errcheck // best-effort cleanup on shutdown
	l.sub = sub

	l.logger.Info("bead-closed event listener subscribed",
		"subject", "beads.bead.closed")
	<-ctx.Done()
	return nil
}

// handle decodes one bead-close event and pushes it into the trigger
// engine. Malformed payloads or events without a bead body are silently
// dropped (matching DecisionEventListener.handle).
func (l *BeadClosedEventListener) handle(msg *nats.Msg) {
	var evt natsBeadClosedPayload
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		l.logger.Debug("bead-closed event: malformed payload", "error", err)
		return
	}
	if evt.Bead == nil || evt.Bead.ID == "" {
		return
	}

	project, role := projectAndRoleFromLabels(evt.Bead.Labels)
	payload := map[string]any{
		"watch_bead_id": evt.Bead.ID,
		"watch_type":    evt.Bead.Type,
		"watch_project": project,
		"watch_role":    role,
		"closed_by":     evt.ClosedBy,
		"title":         evt.Bead.Title,
		"status":        evt.Bead.Status,
	}
	// Decision resolutions carry an explicit answer payload (ka-0000005470).
	// Promote those fields onto the watcher payload so trigger:bead conditions
	// can match on `chosen`, `response_text`, etc. without rehydrating the
	// bead. Decision-related keys are skipped for non-decision closes.
	if evt.DecisionAnswer != nil {
		if evt.DecisionAnswer.Chosen != "" {
			payload["chosen"] = evt.DecisionAnswer.Chosen
		}
		if evt.DecisionAnswer.ResponseText != "" {
			payload["response_text"] = evt.DecisionAnswer.ResponseText
		}
		if evt.DecisionAnswer.RespondedBy != "" {
			payload["responded_by"] = evt.DecisionAnswer.RespondedBy
		}
		if evt.DecisionAnswer.RespondedAt != "" {
			payload["responded_at"] = evt.DecisionAnswer.RespondedAt
		}
		if evt.DecisionAnswer.Cancelled {
			payload["canceled"] = "true"
		}
	}
	l.engine.OnBeadEvent("closed", evt.Bead.ID, payload, evt.Bead.Labels)
	l.logger.Debug("bead-closed event forwarded to trigger engine",
		"bead_id", evt.Bead.ID,
		"type", evt.Bead.Type,
		"project", project,
		"role", role)
}

// projectAndRoleFromLabels extracts the conventional `project:<name>` and
// `role:<name>` labels off a bead's label list. Returns ("","") when
// neither is present.
func projectAndRoleFromLabels(labels []string) (project, role string) {
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, "project:"); ok && project == "" {
			project = rest
			continue
		}
		if rest, ok := strings.CutPrefix(l, "role:"); ok && role == "" {
			role = rest
		}
	}
	return project, role
}
