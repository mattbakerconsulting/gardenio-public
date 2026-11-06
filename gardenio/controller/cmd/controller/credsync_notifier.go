package main

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// credSyncEvent is the NATS payload for credential sync events.
type credSyncEvent struct {
	Event    string `json:"event"`              // synced, failed, single_synced, single_failed
	Account  string `json:"account,omitempty"`  // account name (single sync)
	Provider string `json:"provider,omitempty"` // credential provider (e.g. "claude")
	Synced   int    `json:"synced,omitempty"`   // number of accounts synced (bulk sync)
	Failed   int    `json:"failed,omitempty"`   // number of accounts that failed (bulk sync)
	Reason   string `json:"reason,omitempty"`   // failure reason
	Trigger  string `json:"trigger,omitempty"`  // what triggered the sync: "startup", "periodic", "account_updated", "config_auth_updated"
	// Timestamp is the RFC3339 time of the event.
	Timestamp string `json:"timestamp"`
}

// credSyncNotifier publishes credential sync events to NATS so operators
// can observe credential lifecycle events.
type credSyncNotifier struct {
	conn   *nats.Conn
	logger *slog.Logger
}

func newCredSyncNotifier(natsURL, natsToken, natsUser string, logger *slog.Logger) *credSyncNotifier {
	if natsURL == "" {
		logger.Info("credential sync notifier disabled (no NATS URL)")
		return nil
	}

	opts := []nats.Option{
		nats.Name("gardenio-controller-credsync"),
		nats.MaxReconnects(-1),
	}
	opts = append(opts, natsauth.Options(natsUser, natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		logger.Warn("credential sync notifier: failed to connect to NATS", "error", err)
		return nil
	}

	logger.Info("credential sync notifier connected to NATS", "url", natsURL)
	return &credSyncNotifier{conn: conn, logger: logger}
}

// NotifyBulkSynced publishes an event after a successful bulk credential sync.
func (n *credSyncNotifier) NotifyBulkSynced(synced, failed int, trigger string) {
	n.publish(credSyncEvent{
		Event:     "synced",
		Synced:    synced,
		Failed:    failed,
		Trigger:   trigger,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifyBulkFailed publishes an event when bulk credential sync fails entirely.
func (n *credSyncNotifier) NotifyBulkFailed(reason, trigger string) {
	n.publish(credSyncEvent{
		Event:     "failed",
		Reason:    reason,
		Trigger:   trigger,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifySingleSynced publishes an event after pushing a single account's credential.
func (n *credSyncNotifier) NotifySingleSynced(account, provider string) {
	n.publish(credSyncEvent{
		Event:     "single_synced",
		Account:   account,
		Provider:  provider,
		Trigger:   "account_updated",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// NotifySingleFailed publishes an event when a single account credential push fails.
func (n *credSyncNotifier) NotifySingleFailed(account, reason string) {
	n.publish(credSyncEvent{
		Event:     "single_failed",
		Account:   account,
		Reason:    reason,
		Trigger:   "account_updated",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

func (n *credSyncNotifier) publish(evt credSyncEvent) {
	if n == nil || n.conn == nil {
		return
	}
	data, err := json.Marshal(evt)
	if err != nil {
		n.logger.Warn("credential sync notifier: marshal failed", "error", err)
		return
	}
	subject := "gardenio.credentials." + evt.Event
	if err := n.conn.Publish(subject, data); err != nil {
		n.logger.Warn("credential sync notifier: publish failed", "subject", subject, "error", err)
		return
	}
	n.logger.Debug("credential sync event published",
		"event", evt.Event, "account", evt.Account,
		"synced", evt.Synced, "failed", evt.Failed, "trigger", evt.Trigger)
}

// Close closes the NATS connection.
func (n *credSyncNotifier) Close() {
	if n != nil && n.conn != nil {
		n.conn.Close()
	}
}
