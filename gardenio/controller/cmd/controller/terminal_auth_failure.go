package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/natsauth"
)

// terminalAuthFailureEvent is the JSON payload coop publishes on
// {prefix}.auth.terminal_failure when a state transition carries a terminal,
// non-failover credential error (Unauthorized / OutOfCredits) — see coop's
// maybe_publish_terminal_auth_failure in transport/nats.rs (ka-0000003962).
//
// This is a DISTINCT subject from {prefix}.auth.budget_exceeded on purpose: a
// terminal auth failure must NEVER trigger failover (rotating credentials
// cannot fix a key ACL or an out-of-credits account — it would cold-restart
// the pod forever, ka-0000001718). This listener therefore only PERSISTS the
// error onto the agent bead as a durable diagnostic record; it never calls
// authFailover.
type terminalAuthFailureEvent struct {
	Event         string `json:"event"`
	ErrorCategory string `json:"error_category"`
	SessionID     string `json:"session_id"`
	Detail        string `json:"detail"`
	Agent         string `json:"agent"`
	AgentBeadID   string `json:"agent_bead_id"`
	Project       string `json:"project"`
	Account       string `json:"account"`
}

// beadDiagnosticWriter is the subset of the beads client the terminal-auth
// listener needs: resolve an agent bead by name, read its current fields, and
// merge a typed field patch. Narrowed to an interface so the listener is unit
// testable without a live daemon.
type beadDiagnosticWriter interface {
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
}

// terminalAuthFailureListener subscribes to NATS terminal-auth-failure events
// and persists error_category + error_detail onto the affected agent bead.
// Events are debounced per-agent so a pod that re-reports the same terminal
// failure every turn only writes once per window.
type terminalAuthFailureListener struct {
	daemon beadDiagnosticWriter
	logger *slog.Logger
	conn   *nats.Conn

	debounce time.Duration
	mu       sync.Mutex
	lastSeen map[string]time.Time // agent → last event time
}

// terminalAuthFailureConfig holds configuration for the NATS listener.
type terminalAuthFailureConfig struct {
	NatsURL  string
	Daemon   beadDiagnosticWriter
	Logger   *slog.Logger
	Debounce time.Duration // default 30s
}

// newTerminalAuthFailureListener creates a listener. Returns nil if NATS is
// not configured or the daemon is nil.
func newTerminalAuthFailureListener(cfg terminalAuthFailureConfig) *terminalAuthFailureListener {
	if cfg.NatsURL == "" {
		cfg.Logger.Info("terminal auth failure listener: NATS URL not configured, disabled")
		return nil
	}
	if cfg.Daemon == nil {
		return nil
	}
	debounce := cfg.Debounce
	if debounce == 0 {
		debounce = 30 * time.Second
	}
	return &terminalAuthFailureListener{
		daemon:   cfg.Daemon,
		logger:   cfg.Logger,
		debounce: debounce,
		lastSeen: make(map[string]time.Time),
	}
}

// Start connects to NATS and subscribes to terminal-auth-failure events.
// It blocks until ctx is cancelled.
func (l *terminalAuthFailureListener) Start(ctx context.Context, natsURL, natsToken, natsUser string) error {
	opts := []nats.Option{
		nats.Name("gardenio-controller-terminal-auth-failure"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(5 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				l.logger.Warn("terminal auth failure: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			l.logger.Info("terminal auth failure: NATS reconnected")
		}),
	}
	opts = append(opts, natsauth.Options(natsUser, natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return err
	}
	l.conn = conn

	// Coop publishes under {COOP_NATS_PREFIX}.auth.terminal_failure (default
	// prefix "coop.events", giving "coop.events.auth.terminal_failure").
	sub, err := conn.Subscribe("*.*.auth.terminal_failure", func(msg *nats.Msg) {
		l.handleMessage(ctx, msg)
	})
	if err != nil {
		conn.Close()
		return err
	}

	l.logger.Info("terminal auth failure: subscribed to NATS",
		"url", natsURL,
		"subject", "*.*.auth.terminal_failure")

	<-ctx.Done()
	_ = sub.Unsubscribe()
	conn.Close()
	return nil
}

// handleMessage processes a single terminal-auth-failure NATS message by
// persisting the diagnostic fields onto the agent bead. It performs NO
// failover — that is the entire point of the distinct subject.
func (l *terminalAuthFailureListener) handleMessage(ctx context.Context, msg *nats.Msg) {
	var evt terminalAuthFailureEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		l.logger.Debug("terminal auth failure: malformed event", "error", err)
		return
	}
	l.persist(ctx, evt, msg.Subject)
}

// persist resolves the agent bead and merges error_category + error_detail
// (and bumps total_errors) onto it. Split out from handleMessage so tests can
// drive it without a live NATS message.
func (l *terminalAuthFailureListener) persist(ctx context.Context, evt terminalAuthFailureEvent, subject string) {
	if evt.ErrorCategory == "" && evt.Detail == "" {
		l.logger.Debug("terminal auth failure: event carries no category or detail, ignoring")
		return
	}

	// Debounce per agent so a pod that 401s every turn only writes once per
	// window. Prefer the bead id, fall back to the agent name.
	debounceKey := evt.AgentBeadID
	if debounceKey == "" {
		debounceKey = evt.Agent
	}
	if debounceKey == "" {
		l.logger.Debug("terminal auth failure: event missing agent identity, ignoring",
			"subject", subject)
		return
	}
	if !l.tryDebounce(debounceKey) {
		l.logger.Debug("terminal auth failure: debounced",
			"agent", evt.Agent, "agent_bead_id", evt.AgentBeadID)
		return
	}

	l.logger.Info("terminal auth failure event received",
		"subject", subject,
		"event", evt.Event,
		"error_category", evt.ErrorCategory,
		"project", evt.Project,
		"account", evt.Account,
		"agent", evt.Agent,
		"agent_bead_id", evt.AgentBeadID,
		"session_id", evt.SessionID)

	// Resolve the target bead: the coop-injected agent_bead_id is authoritative
	// (from BOAT_AGENT_BEAD_ID); fall back to a by-name lookup when it is
	// absent (e.g. a legacy pod that never set the env var).
	beadID := evt.AgentBeadID
	if beadID == "" {
		bead, err := l.daemon.FindAgentBead(ctx, evt.Agent)
		if err != nil {
			if errors.Is(err, beadsapi.ErrAgentNotFound) {
				l.logger.Warn("terminal auth failure: no agent bead found, cannot persist",
					"agent", evt.Agent)
			} else {
				l.logger.Warn("terminal auth failure: agent bead lookup failed",
					"agent", evt.Agent, "error", err)
			}
			return
		}
		beadID = bead.ID
	}

	// Read the current total_errors so we can bump it. Best-effort: a read
	// failure just means we seed the count from zero.
	prevErrors := 0
	if bead, err := l.daemon.GetBead(ctx, beadID); err == nil && bead != nil {
		if n, perr := strconv.Atoi(bead.Fields["total_errors"]); perr == nil {
			prevErrors = n
		}
	}

	fields := map[string]any{
		"error_category": evt.ErrorCategory,
		"error_detail":   truncateDetail(evt.Detail),
		"total_errors":   prevErrors + 1,
	}
	if err := l.daemon.UpdateBeadFieldsTyped(ctx, beadID, fields); err != nil {
		l.logger.Error("terminal auth failure: failed to persist diagnostics onto bead",
			"bead", beadID, "agent", evt.Agent, "error", err)
		return
	}
	l.logger.Info("terminal auth failure: persisted diagnostics onto agent bead",
		"bead", beadID, "agent", evt.Agent, "error_category", evt.ErrorCategory)
}

// terminalAuthDetailMaxLen caps the persisted error_detail so a pathological
// multi-KB error blob cannot bloat the bead. The leading portion carries the
// signal (status code + "key not allowed to access model" / billing message).
const terminalAuthDetailMaxLen = 2000

// truncateDetail bounds the error detail written to the bead.
func truncateDetail(detail string) string {
	if len(detail) <= terminalAuthDetailMaxLen {
		return detail
	}
	return detail[:terminalAuthDetailMaxLen] + "…(truncated)"
}

// tryDebounce returns true if the event should be processed (not within the
// debounce window), and records the current time.
func (l *terminalAuthFailureListener) tryDebounce(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if last, ok := l.lastSeen[key]; ok {
		if now.Sub(last) < l.debounce {
			return false
		}
	}
	l.lastSeen[key] = now
	return true
}
