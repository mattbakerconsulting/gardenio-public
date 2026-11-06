package main

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"

	"gardenio/controller/internal/healthsnapshot"
)

// rotationEvent is the NATS payload for account rotation events.
type rotationEvent struct {
	Event       string  `json:"event"`                 // rotating, rotated, no_fallback, switchback, account_throttled, account_recovered, account_warming, account_cooled, probe_loop_stalled
	Project     string  `json:"project,omitempty"`     // project name (empty for account-level events)
	FromAccount string  `json:"from_account"`          // account being rotated away from (or throttled account)
	ToAccount   string  `json:"to_account,omitempty"`  // fallback account being rotated to
	AgentBead   string  `json:"agent_bead,omitempty"`  // triggering agent bead ID
	Reason      string  `json:"reason,omitempty"`      // human-readable reason
	ResetAt     string  `json:"reset_at,omitempty"`    // RFC3339 of binding window reset (throttle events)
	Utilization float64 `json:"utilization,omitempty"` // current utilization_5h (warming/cooled events)
	Threshold   float64 `json:"threshold,omitempty"`   // soft failover threshold (warming/cooled events)
	Timestamp   string  `json:"timestamp"`             // RFC3339 timestamp
}

// rotationNotifier publishes account rotation events to NATS for the
// slack-bridge to pick up and post as Slack notifications.
type rotationNotifier struct {
	conn    *nats.Conn
	logger  *slog.Logger
	tracker *healthsnapshot.Tracker
}

func newRotationNotifier(natsURL, natsToken, natsUser string, logger *slog.Logger, tracker *healthsnapshot.Tracker) *rotationNotifier {
	if natsURL == "" {
		logger.Info("rotation notifier disabled (no NATS URL)")
		return nil
	}

	opts := []nats.Option{
		nats.Name("gardenio-controller-rotation"),
		nats.MaxReconnects(-1),
	}
	opts = append(opts, natsauth.Options(natsUser, natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		logger.Warn("rotation notifier: failed to connect to NATS", "error", err)
		return nil
	}

	logger.Info("rotation notifier connected to NATS", "url", natsURL)
	return &rotationNotifier{conn: conn, logger: logger, tracker: tracker}
}

// NotifyRotating publishes an event when starting rotation to a fallback account.
func (n *rotationNotifier) NotifyRotating(project, fromAccount, toAccount, agentBead string) {
	n.publish(rotationEvent{
		Event:       "rotating",
		Project:     project,
		FromAccount: fromAccount,
		ToAccount:   toAccount,
		AgentBead:   agentBead,
		Reason:      "rate_limited",
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifyRotated publishes an event when rotation has completed successfully.
func (n *rotationNotifier) NotifyRotated(project, fromAccount, toAccount string) {
	n.publish(rotationEvent{
		Event:       "rotated",
		Project:     project,
		FromAccount: fromAccount,
		ToAccount:   toAccount,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifyNoFallback publishes an event when no fallback account is configured.
// resetAt is the earliest throttle_reset_at from the account chain (may be empty).
func (n *rotationNotifier) NotifyNoFallback(project, account, resetAt string) {
	n.publish(rotationEvent{
		Event:       "no_fallback",
		Project:     project,
		FromAccount: account,
		ResetAt:     resetAt,
		Reason:      "no fallback account configured",
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	})
}

func (n *rotationNotifier) publish(evt rotationEvent) {
	if n == nil || n.conn == nil {
		return
	}
	data, err := json.Marshal(evt)
	if err != nil {
		n.logger.Warn("rotation notifier: marshal failed", "error", err)
		return
	}
	subject := "gardenio.rotation." + evt.Event
	if err := n.conn.Publish(subject, data); err != nil {
		n.logger.Warn("rotation notifier: publish failed", "subject", subject, "error", err)
		return
	}
	if n.tracker != nil {
		n.tracker.RecordRotation(evt.Event, evt.Project, evt.FromAccount, evt.ToAccount, evt.Reason)
	}
	n.logger.Info("rotation event published",
		"event", evt.Event, "project", evt.Project,
		"from", evt.FromAccount, "to", evt.ToAccount)
}

// NotifyThrottled publishes an event when the probe detects an account has
// become throttled (allowed→rejected transition). This is account-scoped,
// not project-scoped — no Project field is set.
func (n *rotationNotifier) NotifyThrottled(account, resetAt string) {
	n.publish(rotationEvent{
		Event:       "account_throttled",
		FromAccount: account,
		ResetAt:     resetAt,
		Reason:      "probe detected rate limit",
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifyRecovered publishes an event when the probe detects an account has
// recovered from throttling (rejected→allowed transition).
func (n *rotationNotifier) NotifyRecovered(account string) {
	n.publish(rotationEvent{
		Event:       "account_recovered",
		FromAccount: account,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifyWarming publishes an event when the probe detects utilization crossing
// the soft failover threshold. Coopmux subscribes to this to stop sending new
// sessions to the warming account.
func (n *rotationNotifier) NotifyWarming(account string, utilization, threshold float64) {
	n.publish(rotationEvent{
		Event:       "account_warming",
		FromAccount: account,
		Utilization: utilization,
		Threshold:   threshold,
		Reason:      "utilization crossed soft failover threshold",
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifyCooled publishes an event when the probe detects utilization dropping
// back below the soft failover threshold. Coopmux subscribes to this to
// restore the account for new session assignment.
func (n *rotationNotifier) NotifyCooled(account string, utilization, threshold float64) {
	n.publish(rotationEvent{
		Event:       "account_cooled",
		FromAccount: account,
		Utilization: utilization,
		Threshold:   threshold,
		Reason:      "utilization dropped below soft failover threshold",
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifyProbeStalled publishes an alert when the probe watchdog detects the
// rate-limit probe loop has gone stale — its last tick is older than the stall
// threshold (ka-0000001889). This is the alerting signal that was missing during
// the 2026-07-05 incident, where the probe goroutine froze silently for ~24h and
// nothing noticed. staleFor is the human-readable age of the last tick; threshold
// is the configured stall bound. This is a controller-lifecycle alert, not an
// account event, so no account/project field is set.
func (n *rotationNotifier) NotifyProbeStalled(staleFor, threshold string) {
	n.publish(rotationEvent{
		Event:     "probe_loop_stalled",
		Reason:    "rate-limit probe loop last tick " + staleFor + " ago exceeds stall threshold " + threshold,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// Close closes the NATS connection.
func (n *rotationNotifier) Close() {
	if n != nil && n.conn != nil {
		n.conn.Close()
	}
}
