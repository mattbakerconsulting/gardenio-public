// Package bridge provides the account rotation event watcher.
//
// Rotation subscribes to NATS events published by the controller's auth
// failover when accounts are rotated and posts Slack notifications for
// operators.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// RotationEvent is the NATS payload for account rotation events published
// by the controller's auth failover.
type RotationEvent struct {
	Event       string  `json:"event"`
	Project     string  `json:"project,omitempty"`
	FromAccount string  `json:"from_account"`
	ToAccount   string  `json:"to_account,omitempty"`
	AgentBead   string  `json:"agent_bead,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	ResetAt     string  `json:"reset_at,omitempty"`
	Utilization float64 `json:"utilization,omitempty"`
	Threshold   float64 `json:"threshold,omitempty"`
	Timestamp   string  `json:"timestamp"`
}

// RotationNotifier posts account rotation notifications to Slack.
type RotationNotifier interface {
	NotifyAccountRotating(ctx context.Context, evt RotationEvent) error
	NotifyAccountRotated(ctx context.Context, evt RotationEvent) error
	NotifyNoFallback(ctx context.Context, evt RotationEvent) error
	NotifySwitchback(ctx context.Context, evt RotationEvent) error
	NotifyAccountThrottled(ctx context.Context, evt RotationEvent) error
	NotifyAccountRecovered(ctx context.Context, evt RotationEvent) error
	NotifyAccountWarming(ctx context.Context, evt RotationEvent) error
	NotifyAccountCooled(ctx context.Context, evt RotationEvent) error
}

// Ensure Bot implements RotationNotifier.
var _ RotationNotifier = (*Bot)(nil)

// RotationConfig holds configuration for the Rotation watcher.
type RotationConfig struct {
	NatsURL   string
	NatsToken string
	// NatsUser: see HookStreamConfig.NatsUser (ka-0000000002).
	NatsUser string
	Notifier RotationNotifier
	Logger   *slog.Logger
}

// Rotation subscribes to NATS rotation events and dispatches to Slack.
type Rotation struct {
	cfg  RotationConfig
	conn *nats.Conn
	sub  *nats.Subscription
}

// NewRotation creates a new rotation event watcher.
// Returns nil if NATS URL is empty or no notifier is configured.
func NewRotation(cfg RotationConfig) *Rotation {
	if cfg.NatsURL == "" {
		cfg.Logger.Info("rotation watcher: NATS URL not configured, disabled")
		return nil
	}
	if cfg.Notifier == nil {
		cfg.Logger.Info("rotation watcher: no notifier configured, disabled")
		return nil
	}
	return &Rotation{cfg: cfg}
}

// Start connects to NATS and subscribes to rotation events.
// The subscription runs asynchronously — this method returns immediately.
func (r *Rotation) Start() error {
	opts := []nats.Option{
		nats.Name("slack-bridge-rotation"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(5 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				r.cfg.Logger.Warn("rotation: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			r.cfg.Logger.Info("rotation: NATS reconnected")
		}),
	}
	opts = append(opts, natsauth.Options(r.cfg.NatsUser, r.cfg.NatsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(r.cfg.NatsURL, opts...)
	if err != nil {
		return fmt.Errorf("rotation: NATS connect: %w", err)
	}
	r.conn = conn

	sub, err := conn.Subscribe("gardenio.rotation.>", r.handleMessage)
	if err != nil {
		conn.Close()
		return fmt.Errorf("rotation: subscribe: %w", err)
	}
	r.sub = sub

	r.cfg.Logger.Info("rotation watcher: subscribed to NATS",
		"url", r.cfg.NatsURL, "subject", "gardenio.rotation.>")
	return nil
}

// Close unsubscribes and closes the NATS connection.
func (r *Rotation) Close() {
	if r.sub != nil {
		_ = r.sub.Unsubscribe()
	}
	if r.conn != nil {
		r.conn.Close()
	}
}

func (r *Rotation) handleMessage(msg *nats.Msg) {
	var evt RotationEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		r.cfg.Logger.Warn("rotation: malformed event", "error", err)
		return
	}

	ctx := context.Background()
	var err error

	switch evt.Event {
	case "rotating":
		err = r.cfg.Notifier.NotifyAccountRotating(ctx, evt)
	case "rotated":
		err = r.cfg.Notifier.NotifyAccountRotated(ctx, evt)
	case "no_fallback":
		err = r.cfg.Notifier.NotifyNoFallback(ctx, evt)
	case "switchback":
		err = r.cfg.Notifier.NotifySwitchback(ctx, evt)
	case "account_throttled":
		err = r.cfg.Notifier.NotifyAccountThrottled(ctx, evt)
	case "account_recovered":
		err = r.cfg.Notifier.NotifyAccountRecovered(ctx, evt)
	case "account_warming":
		err = r.cfg.Notifier.NotifyAccountWarming(ctx, evt)
	case "account_cooled":
		err = r.cfg.Notifier.NotifyAccountCooled(ctx, evt)
	default:
		r.cfg.Logger.Debug("rotation: unknown event type", "event", evt.Event)
		return
	}

	if err != nil {
		// Account-scoped events (no project field) post to the default global
		// channel, which may be intentionally unset or have the bot missing
		// from it. Demote those failures to a warning so a misconfigured
		// global channel doesn't raise ERROR-level alerts on every event.
		// Project-scoped failures remain ERROR since they indicate the
		// per-project channel is missing.
		if evt.Project == "" {
			r.cfg.Logger.Warn("rotation: notification skipped (no global channel)",
				"event", evt.Event, "error", err)
			return
		}
		r.cfg.Logger.Error("rotation: notification failed",
			"event", evt.Event, "project", evt.Project, "error", err)
	}
}
