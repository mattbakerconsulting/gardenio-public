package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// MailEventListener subscribes to mail-bead-create lifecycle events on NATS
// and forwards them into the scheduler's EntryEngine via OnBeadEvent.
// This is the controller-side ingestion path for trigger_mode=mail_received
// schedules: kbeads emits a "beads.mail.sent" event whenever a bead with
// type="mail" is created (see kbeads/internal/server/beads.go createBead).
//
// The listener mirrors BeadClosedEventListener: process-local, no JetStream
// durability — restart drops in-flight events. Catch-up across restarts is
// bounded by each schedule's last_run plus the reconcile-tick re-evaluation,
// so a missed event surfaces on the next mail-bead create instead of being
// replayed.
type MailEventListener struct {
	engine *EntryEngine
	logger *slog.Logger

	conn *nats.Conn
	sub  *nats.Subscription
}

// NewMailEventListener wires a listener that pushes incoming mail-sent
// events into engine.OnBeadEvent. The engine reference must outlive the
// listener.
func NewMailEventListener(engine *EntryEngine, logger *slog.Logger) *MailEventListener {
	return &MailEventListener{
		engine: engine,
		logger: logger.With("component", "mail-event-listener"),
	}
}

// natsMailSentPayload mirrors kbeads/internal/events.MailSent. Defined
// locally so the controller doesn't have to import kbeads internals.
type natsMailSentPayload struct {
	BeadID  string `json:"bead_id"`
	Channel string `json:"channel,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// Start connects to NATS and subscribes to mail-sent events. Blocks until
// ctx is cancelled, mirroring BeadClosedEventListener.Start.
func (l *MailEventListener) Start(ctx context.Context, natsURL, natsToken, natsUser string) error {
	if natsURL == "" {
		l.logger.Info("mail event listener disabled (no NATS URL)")
		return nil
	}
	if l.engine == nil {
		l.logger.Info("mail event listener disabled (no trigger engine)")
		return nil
	}

	opts := []nats.Option{
		nats.Name("gardenio-controller-mail-events"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				l.logger.Warn("mail event listener: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			l.logger.Info("mail event listener: NATS reconnected")
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

	sub, err := conn.Subscribe("beads.mail.sent", l.handle)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe() //nolint:errcheck // best-effort cleanup on shutdown
	l.sub = sub

	l.logger.Info("mail event listener subscribed",
		"subject", "beads.mail.sent")
	<-ctx.Done()
	return nil
}

// handle decodes one mail-sent event and pushes it into the trigger engine.
// Malformed payloads or events without a bead id are silently dropped.
func (l *MailEventListener) handle(msg *nats.Msg) {
	var evt natsMailSentPayload
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		l.logger.Debug("mail event: malformed payload", "error", err)
		return
	}
	if evt.BeadID == "" {
		return
	}

	payload := map[string]any{
		"watch_bead_id": evt.BeadID,
		"watch_channel": evt.Channel,
		"watch_from":    evt.From,
		"watch_to":      evt.To,
		"watch_subject": evt.Subject,
	}
	// Empty Labels: the daemon publishes the projection but not the bead's
	// label slice on this subject. Schedules that need label-level matching
	// should rely on `from:`/`channel:` watcher fields, not raw labels.
	l.engine.OnBeadEvent("mail_sent", evt.BeadID, payload, nil)
	l.logger.Debug("mail event forwarded to trigger engine",
		"bead_id", evt.BeadID,
		"from", evt.From,
		"to", evt.To,
		"channel", evt.Channel)
}
