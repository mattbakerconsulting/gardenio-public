package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// accountExhaustionEvent is the JSON payload published by coop on
// {prefix}.account.exhausted when all credential profiles are rate-limited
// and the agent enters Parked state. Also used for {prefix}.auth.budget_exceeded
// events which fire on individual rate limit detections (before full exhaustion).
type accountExhaustionEvent struct {
	Event           string `json:"event"`
	SessionID       string `json:"session_id"`
	ParkedReason    string `json:"parked_reason"`
	ResumeAtEpochMS *int64 `json:"resume_at_epoch_ms"`
	Detail          string `json:"detail"`
	Agent           string `json:"agent"`
	Project         string `json:"project"`
	Account         string `json:"account"`
}

// accountExhaustionListener subscribes to NATS account exhaustion events and
// triggers auth failover. Events are debounced per-account so that multiple
// agents reporting the same exhausted account within a window only trigger
// one failover attempt.
type accountExhaustionListener struct {
	failover *authFailover
	logger   *slog.Logger
	conn     *nats.Conn
	sub      *nats.Subscription

	debounce time.Duration
	mu       sync.Mutex
	lastSeen map[string]time.Time // account → last event time
}

// accountExhaustionConfig holds configuration for the NATS listener.
type accountExhaustionConfig struct {
	NatsURL   string
	NatsToken string
	Failover  *authFailover
	Logger    *slog.Logger
	Debounce  time.Duration // default 30s
}

// newAccountExhaustionListener creates a listener. Returns nil if NATS is not
// configured or failover is nil.
func newAccountExhaustionListener(cfg accountExhaustionConfig) *accountExhaustionListener {
	if cfg.NatsURL == "" {
		cfg.Logger.Info("account exhaustion listener: NATS URL not configured, disabled")
		return nil
	}
	if cfg.Failover == nil {
		return nil
	}
	debounce := cfg.Debounce
	if debounce == 0 {
		debounce = 30 * time.Second
	}
	return &accountExhaustionListener{
		failover: cfg.Failover,
		logger:   cfg.Logger,
		debounce: debounce,
		lastSeen: make(map[string]time.Time),
	}
}

// Start connects to NATS and subscribes to account exhaustion events.
// It blocks until ctx is cancelled.
func (l *accountExhaustionListener) Start(ctx context.Context, natsURL, natsToken, natsUser string) error {
	opts := []nats.Option{
		nats.Name("gardenio-controller-account-exhaustion"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(5 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				l.logger.Warn("account exhaustion: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			l.logger.Info("account exhaustion: NATS reconnected")
		}),
	}
	opts = append(opts, natsauth.Options(natsUser, natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return err
	}
	l.conn = conn

	// Subscribe to account.exhausted events (Parked state — all profiles depleted).
	// Coop publishes under {COOP_NATS_PREFIX}.account.exhausted (default prefix
	// "coop.events", giving "coop.events.account.exhausted").
	sub, err := conn.Subscribe("*.*.account.exhausted", func(msg *nats.Msg) {
		l.handleMessage(ctx, msg)
	})
	if err != nil {
		conn.Close()
		return err
	}
	l.sub = sub

	// Subscribe to auth.budget_exceeded events (individual rate limit detections).
	// Coop publishes these on every rate_limit_error or budget_exceeded error,
	// even when no profiles are registered. This is the primary reactive path
	// for triggering auth failover.
	authSub, err := conn.Subscribe("*.*.auth.budget_exceeded", func(msg *nats.Msg) {
		l.handleMessage(ctx, msg)
	})
	if err != nil {
		_ = sub.Unsubscribe()
		conn.Close()
		return err
	}

	l.logger.Info("account exhaustion: subscribed to NATS",
		"url", natsURL,
		"subjects", []string{"*.*.account.exhausted", "*.*.auth.budget_exceeded"})

	// Block until context is cancelled.
	<-ctx.Done()
	_ = sub.Unsubscribe()
	_ = authSub.Unsubscribe()
	conn.Close()
	return nil
}

// handleMessage processes a single account exhaustion NATS message.
func (l *accountExhaustionListener) handleMessage(ctx context.Context, msg *nats.Msg) {
	var evt accountExhaustionEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		l.logger.Debug("account exhaustion: malformed event", "error", err)
		return
	}

	if evt.Project == "" {
		l.logger.Debug("account exhaustion: event missing project field")
		return
	}

	// Debounce key: prefer the real account name, but fall back to the project
	// when the event carries no account (coop publishes an empty account before
	// any credential profile has registered). The fallback is ONLY for the
	// debounce key — we must not report the project name as if it were a real
	// account bead, which produced misleading "account=monorepo" log lines and
	// downstream events (ka-0000005652).
	account := evt.Account
	debounceAccount := account
	if debounceAccount == "" {
		debounceAccount = evt.Project
	}
	debounceKey := evt.Project + "/" + debounceAccount

	if !l.tryDebounce(debounceKey) {
		l.logger.Debug("account exhaustion: debounced",
			"project", evt.Project, "account", account)
		return
	}

	l.logger.Info("account exhaustion event received",
		"subject", msg.Subject,
		"event", evt.Event,
		"project", evt.Project,
		"account", account,
		"agent", evt.Agent,
		"parked_reason", evt.ParkedReason,
		"session_id", evt.SessionID)

	// Trigger auth failover using the same path as SSE budget_exceeded events.
	// The agent field from the NATS event is the agent name, not the bead ID.
	// authFailover.Handle only uses the bead ID for logging, so pass agent name.
	if err := l.failover.Handle(ctx, evt.Project, evt.Agent); err != nil {
		l.logger.Error("account exhaustion: auth failover failed",
			"project", evt.Project, "agent", evt.Agent, "error", err)
	}
}

// tryDebounce returns true if the event should be processed (not within the
// debounce window), and records the current time.
func (l *accountExhaustionListener) tryDebounce(key string) bool {
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
