// Package toollatency is the P2 HOOK_EVENTS consumer (task gd-sWwPnwkZtw, plan
// gardenio/plans/agent-tool-latency-attribution.md): one durable JetStream
// consumer of the HOOK_EVENTS stream that produces, per PostToolUse event,
// bounded per-tool Prometheus histograms (the metric plane) and a full-argv
// slow-call ledger row for calls over a threshold (the record plane).
//
// DURABLE PUSH SUBSCRIPTION, NOT core-NATS Subscribe: unlike
// internal/assistantturnminter's split live-subscribe + separate
// ephemeral-ordered-consumer replay, this consumer uses a single named,
// ack-tracked JetStream push consumer bound with nats.Bind + nats.ManualAck. A
// durable push consumer is simultaneously the live path AND the resume path —
// DeliverAll only takes effect at first creation of the durable consumer;
// every subsequent (re)bind after a controller restart resumes from the
// server-tracked ack floor, so no separate backfill goroutine is needed to
// avoid losing the window across a restart.
//
// The consumer is created with js.AddConsumer and then BOUND, never created
// implicitly by js.Subscribe(..., nats.Durable(...)): nats.go deletes a
// consumer it created itself when that subscription is unsubscribed or
// drained, so Close would wipe the ack floor and the next Start would replay
// the whole retained window from DeliverAll (gd-mz8QgNjWzp).
//
// TWO-PLANE OUTPUT, BOTH BEST-EFFORT: a metric-emission or ledger-write failure
// is logged and the message is still acked — this consumer must never redeliver
// indefinitely or block the stream on a downstream (Prometheus registry /
// beads daemon) hiccup.
//
// REDACTION IS DEFENSE IN DEPTH: gb hook relay already redacts tool_input
// before it ever reaches NATS (cmd/gb/hook_relay.go redactToolInput). This
// consumer redacts AGAIN on write via RedactJSON/RedactText so a ledger row
// is never the single remaining backstop against a secret leaking into
// Postgres — argv must never become a metric label either, so only the
// bounded ClassifyCmd domain reaches gardeniometrics, never raw argv.
//
// SUBJECT/STREAM NAMES ARE HARDCODED, not imported from kbeads: kbeads is a
// separate Go module and internal/eventbus is Go-`internal`, so its
// StreamHookEvents/SubjectHookPrefix constants are unimportable from
// gardenio/controller regardless of visibility rules (confirmed by grep: no
// existing controller code imports kbeads/internal/eventbus; nats_decision.go
// only mentions it in a comment). hook_relay.go already hardcodes the
// "hooks.%s.%s" subject format for the same reason — this mirrors that.
package toollatency

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/metrics"
	"gardenio/controller/internal/natsauth"
)

// hookEventsStreamName is the JetStream stream kbeads creates for hook events
// (kbeads internal/eventbus/streams.go StreamHookEvents). Hardcoded locally —
// see the package doc comment for why this cannot be imported.
const hookEventsStreamName = "HOOK_EVENTS"

// hookEventsSubjectAll is the wildcard subject covering every agent and event
// type published under the HOOK_EVENTS stream (hooks.<agent>.<event>, built by
// cmd/gb/hook_relay.go's sanitizeSubject + fmt.Sprintf("hooks.%s.%s", ...)).
const hookEventsSubjectAll = "hooks.>"

// consumerDurableName is the JetStream durable consumer name. Stable across
// controller restarts/replicas so every (re)bind resumes the SAME
// server-tracked ack floor rather than creating a fresh consumer that replays
// (or loses) the whole retained window.
const consumerDurableName = "tool-latency-consumer"

// DefaultSlowThreshold is the default cutoff over which a tool call is written
// to the slow_tool_calls ledger (plan §P2 acceptance criteria: default 2s).
const DefaultSlowThreshold = 2 * time.Second

// hookEventBashTool is the ToolName value the Bash tool reports on
// PreToolUse/PostToolUse — the only tool name that additionally emits a
// bash_cmd_latency_seconds sample (cmd_class requires a shell command line).
const hookEventBashTool = "Bash"

// BeadAPI is the daemon surface the consumer needs — satisfied by
// *beadsapi.Client. A narrow interface so tests can inject a fake without a
// live daemon, mirroring internal/assistantturnminter.BeadAPI.
type BeadAPI interface {
	RecordSlowToolCall(ctx context.Context, row *beadsapi.SlowToolCallRow) error
}

// ToolMetrics is the metrics surface the consumer needs — satisfied by
// *metrics.ToolLatencyMetrics. A nil ToolMetrics is tolerated (both Record
// methods are nil-receiver-safe no-ops), mirroring the other Record* metric
// collectors in this repo.
type ToolMetrics interface {
	RecordToolLatency(seconds float64, tool, project, harness, source string) bool
	RecordBashCmdLatency(seconds float64, cmdClass, project, harness, source string) bool
}

var _ ToolMetrics = (*metrics.ToolLatencyMetrics)(nil)

// hookEventWire is the on-wire shape of a gb hook-relay event this consumer
// cares about — a subset of cmd/gb/hook_relay.go's hookRelayEvent. Only the
// fields PreToolUse/PostToolUse populate are decoded; every other hook event
// type is ignored (see handleMessage).
type hookEventWire struct {
	Event          string `json:"event"`
	TS             string `json:"ts"`
	AgentBeadID    string `json:"agent_bead_id"`
	SessionID      string `json:"session_id"`
	Project        string `json:"project"`
	Role           string `json:"role"`
	Harness        string `json:"harness"`
	IsSidechain    bool   `json:"is_sidechain"`
	ToolName       string `json:"tool_name"`
	ToolInput      any    `json:"tool_input"`
	ToolUseID      string `json:"tool_use_id"`
	DurationMs     int64  `json:"duration_ms"`
	DurationSource string `json:"duration_source"`
}

// Consumer subscribes to the HOOK_EVENTS JetStream stream and, for every
// PostToolUse event, emits the bounded per-tool metric plane and (above
// threshold) a slow_tool_calls ledger row.
//
// A nil *Consumer is safe: Start/Close are no-ops, mirroring the nil-safe
// Minter pattern — the host degrades to "no tool-latency attribution" when
// NATS is not configured.
type Consumer struct {
	natsURL     string
	natsToken   string
	natsUser    string
	daemon      BeadAPI
	toolMetrics ToolMetrics
	threshold   time.Duration
	logger      *slog.Logger
	// now is injected so tests can drive pairing TTL/eviction deterministically.
	now func() time.Time

	pairing *Pairing

	mu   sync.Mutex
	conn *nats.Conn
	sub  *nats.Subscription
	// closed is closed by the NATS ClosedHandler, so Close can wait for a
	// connection drain to finish.
	closed chan struct{}
}

// New builds a consumer from the NATS connection parameters, the daemon client
// it writes slow-call ledger rows through, and the metrics collector it
// records histogram samples on. Returns nil when natsURL is empty (NATS not
// configured) — callers treat a nil consumer as "tool-latency attribution
// disabled", which every method tolerates. threshold <= 0 falls back to
// DefaultSlowThreshold.
func New(natsURL, natsToken, natsUser string, daemon BeadAPI, toolMetrics ToolMetrics, threshold time.Duration, logger *slog.Logger) *Consumer {
	if natsURL == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if threshold <= 0 {
		threshold = DefaultSlowThreshold
	}
	return &Consumer{
		natsURL:     natsURL,
		natsToken:   natsToken,
		natsUser:    natsUser,
		daemon:      daemon,
		toolMetrics: toolMetrics,
		threshold:   threshold,
		logger:      logger,
		now:         func() time.Time { return time.Now() },
		pairing:     NewPairing(),
	}
}

// Start connects to NATS and binds the durable JetStream push consumer. Safe
// on a nil consumer (no-op).
//
// The durable name is fixed (consumerDurableName), so a restart or an
// additional replica binds the SAME consumer: JetStream resumes delivery from
// the server-tracked ack floor rather than from DeliverAll again, which is
// what makes this "genuinely durable" rather than a core-NATS subscribe that
// would silently lose everything published while the controller was down.
func (c *Consumer) Start() error {
	if c == nil {
		return nil
	}

	closed := make(chan struct{})
	// connected is closed on the first connect, immediate or retried
	// (natsauth.StartupRetry): binding the durable needs a live server.
	connected := make(chan struct{})
	var connectedOnce sync.Once
	opts := []nats.Option{
		nats.Name("gardenio-tool-latency-consumer"),
		nats.ClosedHandler(func(_ *nats.Conn) { close(closed) }),
		nats.ConnectHandler(func(_ *nats.Conn) { connectedOnce.Do(func() { close(connected) }) }),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				c.logger.Warn("tool-latency-consumer: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			c.logger.Info("tool-latency-consumer: NATS reconnected")
		}),
	}
	opts = append(opts, natsauth.Options(c.natsUser, c.natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(c.natsURL, opts...)
	if err != nil {
		return err
	}

	if !conn.IsConnected() {
		// NATS is not up yet: keep the conn and bind once it connects.
		c.mu.Lock()
		c.conn = conn
		c.closed = closed
		c.mu.Unlock()
		c.logger.Warn("tool-latency-consumer: NATS not reachable yet — binding the durable once it is", "url", c.natsURL)
		go func() {
			select {
			case <-connected:
			case <-closed:
				return
			}
			sub, err := c.bind(conn)
			if err != nil {
				c.logger.Warn("tool-latency-consumer: bind after connect failed — tool latency will not be attributed", "error", err)
				return
			}
			c.mu.Lock()
			if c.conn == conn {
				c.sub = sub
			}
			c.mu.Unlock()
			c.logger.Info("tool-latency-consumer: subscribed to hook events",
				"url", c.natsURL, "subject", hookEventsSubjectAll, "durable", consumerDurableName)
		}()
		return nil
	}

	sub, err := c.bind(conn)
	if err != nil {
		conn.Close()
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.sub = sub
	c.closed = closed
	c.mu.Unlock()

	c.logger.Info("tool-latency-consumer: subscribed to hook events",
		"url", c.natsURL, "subject", hookEventsSubjectAll, "durable", consumerDurableName)
	return nil
}

// bind creates the durable consumer if needed and binds a subscription to it.
// It needs a connected conn.
func (c *Consumer) bind(conn *nats.Conn) (*nats.Subscription, error) {
	js, err := conn.JetStream()
	if err != nil {
		return nil, err
	}
	if err := ensureDurable(js, conn.NewInbox()); err != nil {
		return nil, err
	}
	return js.Subscribe(hookEventsSubjectAll, c.handleMessage,
		nats.Bind(hookEventsStreamName, consumerDurableName),
		nats.ManualAck(),
	)
}

// ensureDurable creates the durable push consumer if it does not exist yet.
// deliverSubject is used only at creation; a later bind reuses the subject
// stored in the consumer's config. Another replica creating it first is not
// an error — both then bind the same consumer.
func ensureDurable(js nats.JetStreamContext, deliverSubject string) error {
	_, err := js.ConsumerInfo(hookEventsStreamName, consumerDurableName)
	if err == nil || !errors.Is(err, nats.ErrConsumerNotFound) {
		return err
	}
	_, err = js.AddConsumer(hookEventsStreamName, &nats.ConsumerConfig{
		Durable:        consumerDurableName,
		DeliverSubject: deliverSubject,
		DeliverPolicy:  nats.DeliverAllPolicy,
		AckPolicy:      nats.AckExplicitPolicy,
		MaxAckPending:  4096,
		FilterSubject:  hookEventsSubjectAll,
	})
	var apiErr *nats.APIError
	if errors.As(err, &apiErr) &&
		(apiErr.ErrorCode == nats.JSErrCodeConsumerAlreadyExists || apiErr.ErrorCode == nats.JSErrCodeConsumerNameExists) {
		return nil
	}
	return err
}

// Close drains the connection and waits for the drain to finish, so every
// in-flight delivery callback completes and its ack is flushed to the server
// before the connection closes. The durable consumer is left in place (it is
// bound, not library-owned), so the next Start resumes from its ack floor.
// Safe on a nil consumer or one that was never started.
func (c *Consumer) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	conn, closed := c.conn, c.closed
	c.sub, c.conn, c.closed = nil, nil, nil
	c.mu.Unlock()

	if conn == nil {
		return
	}
	if err := conn.Drain(); err != nil {
		c.logger.Warn("tool-latency-consumer: drain failed", "error", err)
		conn.Close()
	}
	// Drain closes the connection when done, or after nats.DefaultDrainTimeout.
	<-closed
}

// handleMessage is the JetStream delivery callback. It ALWAYS acks — a
// malformed event or a downstream (metrics/daemon) failure is logged and
// swallowed, never left to redeliver indefinitely, per the best-effort,
// log-and-continue contract (plan §P2).
func (c *Consumer) handleMessage(msg *nats.Msg) {
	defer func() {
		if err := msg.Ack(); err != nil {
			c.logger.Warn("tool-latency-consumer: ack failed", "error", err, "subject", msg.Subject)
		}
	}()

	var evt hookEventWire
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		c.logger.Warn("tool-latency-consumer: malformed event, dropping", "error", err, "subject", msg.Subject)
		return
	}

	ts := parseEventTS(evt.TS, c.nowFn())

	switch evt.Event {
	case "PreToolUse":
		if evt.ToolUseID != "" {
			c.pairing.Observe(evt.ToolUseID, ts)
		}
	case "PostToolUse":
		c.handlePostToolUse(&evt, ts)
	}
}

func (c *Consumer) nowFn() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// parseEventTS parses hookRelayEvent.TS (time.RFC3339Nano, stamped by gb hook
// relay). An unparseable/empty timestamp falls back to now so a single bad
// event degrades to "treat it as just arrived" rather than aborting it.
func parseEventTS(s string, now time.Time) time.Time {
	if s == "" {
		return now
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return now
	}
	return t
}

// handlePostToolUse resolves the call's duration (native "claude" duration if
// present, else a "derived" one from the paired PreToolUse timestamp), emits
// the metric-plane samples, and — above threshold — writes a redacted
// slow_tool_calls ledger row.
func (c *Consumer) handlePostToolUse(evt *hookEventWire, postTS time.Time) {
	seconds, source, ok := c.resolveDuration(evt, postTS)
	if !ok {
		// No native duration and no matching PreToolUse observation (e.g. the
		// consumer started after the pair's Pre event, or the pairing entry
		// aged out) — nothing to attribute latency to.
		return
	}

	project := evt.Project
	c.toolMetrics.RecordToolLatency(seconds, evt.ToolName, project, evt.Harness, source)

	var cmdClass string
	if evt.ToolName == hookEventBashTool {
		cmdClass = ClassifyCmd(bashCommandFromToolInput(evt.ToolInput))
		c.toolMetrics.RecordBashCmdLatency(seconds, cmdClass, project, evt.Harness, source)
	}

	if seconds < c.threshold.Seconds() {
		return
	}
	c.recordSlowCall(evt, postTS, seconds, source, cmdClass)
}

// resolveDuration returns the call's duration in seconds and its source
// ("claude" | "derived"). ok is false when neither a native duration nor a
// pairing match is available.
func (c *Consumer) resolveDuration(evt *hookEventWire, postTS time.Time) (seconds float64, source string, ok bool) {
	if evt.DurationSource == metrics.ToolLatencySourceClaude && evt.DurationMs > 0 {
		return float64(evt.DurationMs) / 1000.0, metrics.ToolLatencySourceClaude, true
	}
	if evt.ToolUseID == "" {
		return 0, "", false
	}
	d, found := c.pairing.Take(evt.ToolUseID, postTS)
	if !found {
		return 0, "", false
	}
	return d.Seconds(), metrics.ToolLatencySourceDerived, true
}

// recordSlowCall builds and best-effort writes a slow_tool_calls ledger row.
// Both CommandRedacted and ToolInputRedacted are scrubbed again here — the
// second of the two independent redaction layers (gb hook relay already
// redacted tool_input before publishing) — so a ledger row never relies on
// only one layer holding.
func (c *Consumer) recordSlowCall(evt *hookEventWire, ts time.Time, seconds float64, source, cmdClass string) {
	if c.daemon == nil {
		return
	}

	row := &beadsapi.SlowToolCallRow{
		TS:          ts,
		AgentBeadID: evt.AgentBeadID,
		SessionID:   evt.SessionID,
		Project:     evt.Project,
		Role:        evt.Role,
		ToolName:    evt.ToolName,
		ToolUseID:   evt.ToolUseID,
		DurationMs:  int64(seconds * 1000),
		Source:      source,
		CmdClass:    cmdClass,
		IsSidechain: evt.IsSidechain,
	}

	if evt.ToolName == hookEventBashTool {
		row.CommandRedacted = RedactText(bashCommandFromToolInput(evt.ToolInput))
	}
	if evt.ToolInput != nil {
		if raw, err := json.Marshal(evt.ToolInput); err == nil {
			row.ToolInputRedacted = json.RawMessage(RedactJSON(raw))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.daemon.RecordSlowToolCall(ctx, row); err != nil {
		c.logger.Warn("tool-latency-consumer: failed to record slow tool call",
			"error", err, "tool", evt.ToolName, "session_id", evt.SessionID)
	}
}

// bashCommandFromToolInput extracts the "command" string field a Bash
// tool_input carries (possibly already redacted by gb hook relay). Tolerates
// any shape (map, nil, non-string) by returning "" rather than panicking.
func bashCommandFromToolInput(toolInput any) string {
	m, ok := toolInput.(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := m["command"].(string)
	return cmd
}
