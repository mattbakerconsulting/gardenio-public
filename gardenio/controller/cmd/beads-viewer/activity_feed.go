package main

import (
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// activitySubjects are the hook events the Homestead activity feed reads (gd-H4jk9KgUAG, step 4a of plan
// gd-gAKb8Bf0Nm). `gb hook relay` publishes every hook to hooks.<agent>.<event> (cmd/gb/hook_relay.go; the daemon's
// HTTP publish lands on the same subject), so one subscription per event type covers every agent.
var activitySubjects = []string{
	"hooks.*.PreToolUse",
	"hooks.*.PostToolUse",
	"hooks.*.PostToolUseFailure",
	"hooks.*.SubagentStart",
	"hooks.*.SubagentStop",
}

// ActivityFeed subscribes to the hook events on core NATS and hands each payload to sink
// (gardenhub.Farm.ObserveHook), which reduces it to a doing class and drops tool_input. Unlike ThinkingHub there is
// no JetStream backfill and no durable consumer: the feed is ephemeral, and a missed bird does not matter.
//
// A nil *ActivityFeed is safe: Start and Close are no-ops, so the farm simply has no activity when NATS is not
// configured.
type ActivityFeed struct {
	natsURL   string
	natsToken string
	natsUser  string
	sink      func([]byte)
	logger    *slog.Logger

	conn *nats.Conn
	subs []*nats.Subscription
}

// NewActivityFeed builds a feed from the NATS connection parameters. Returns nil when natsURL or sink is unset.
func NewActivityFeed(natsURL, natsToken, natsUser string, sink func([]byte), logger *slog.Logger) *ActivityFeed {
	if natsURL == "" || sink == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ActivityFeed{natsURL: natsURL, natsToken: natsToken, natsUser: natsUser, sink: sink, logger: logger}
}

// Start connects and subscribes, returning once the interest is registered on the server (see ThinkingHub.Start for
// why it flushes). Safe on a nil feed.
func (a *ActivityFeed) Start() error {
	if a == nil {
		return nil
	}
	opts := []nats.Option{
		nats.Name("beads-viewer-activity"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				a.logger.Warn("activity: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			a.logger.Info("activity: NATS reconnected")
		}),
	}
	opts = append(opts, natsauth.Options(a.natsUser, a.natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)
	conn, err := nats.Connect(a.natsURL, opts...)
	if err != nil {
		return err
	}
	handle := func(msg *nats.Msg) { a.sink(msg.Data) }
	for _, subj := range activitySubjects {
		sub, err := conn.Subscribe(subj, handle)
		if err != nil {
			conn.Close()
			a.subs = nil
			return err
		}
		a.subs = append(a.subs, sub)
	}
	a.conn = conn
	if !conn.IsConnected() {
		// NATS is not up yet: the subscriptions are buffered and sent once
		// the retrying connect succeeds, and there is nothing to flush.
		a.logger.Warn("activity: NATS not reachable yet — subscribing once it is", "url", a.natsURL)
		return nil
	}
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		conn.Close()
		a.conn = nil
		a.subs = nil
		return err
	}
	a.logger.Info("activity: subscribed to hook events", "url", a.natsURL, "subjects", activitySubjects)
	return nil
}

// Close unsubscribes and closes the connection. Safe on a nil or unstarted feed.
func (a *ActivityFeed) Close() {
	if a == nil {
		return
	}
	for _, s := range a.subs {
		_ = s.Unsubscribe()
	}
	a.subs = nil
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
	}
}
