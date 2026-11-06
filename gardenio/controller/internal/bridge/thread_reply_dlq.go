package bridge

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Thread-reply durability: bounded transient retry + durable DLQ replay
// (ka-0000000612).
//
// recordUnifiedThreadReply was best-effort — a transient daemon failure during
// the thread-head resolve or message create dropped the human's reply from the
// beads-viewer /threads conversation lane with no retry and no alarm. Two layers
// close that gap:
//
//  1. retryTransientDaemon wraps each daemon write with a few bounded-backoff
//     attempts, absorbing a daemon that is momentarily under load, restarting, or
//     returning a transient 5xx. The write budget stays well inside the 30s
//     per-event handler timeout (bot.go handlerCtx).
//  2. When retries are exhausted, the reply is enqueued to a durable DLQ in the
//     StateManager (survives a bridge crash). drainThreadReplyDLQ runs on a ticker
//     and replays each queued reply through the same write path; the message
//     create's idempotency key makes replay exactly-once at the daemon.

const (
	// threadWriteMaxAttempts bounds the inline transient retry per daemon write.
	// 3 attempts with the backoff below caps a single write at ~0.9s of sleeps,
	// leaving ample room under the 30s handler budget even across both the
	// head-resolve and create stages.
	threadWriteMaxAttempts = 3
	// threadWriteBaseBackoff is the first inter-attempt sleep; it doubles each
	// retry (0.3s, 0.6s).
	threadWriteBaseBackoff = 300 * time.Millisecond

	// threadReplyDLQInterval is the DLQ drain cadence.
	threadReplyDLQInterval = 30 * time.Second
	// threadReplyDLQMaxAttempts caps replay attempts before a queued reply is
	// dropped as un-replayable, so a permanently-poisoned entry cannot wedge the
	// queue forever. Combined with the 30s cadence this is ~10 minutes of retries.
	threadReplyDLQMaxAttempts = 20
	// threadReplyDLQReplayTimeout bounds a single replay's daemon writes.
	threadReplyDLQReplayTimeout = 20 * time.Second
)

// threadReplyIdempotencyKey is the stable per-reply key stamped on the message
// bead's create. It is derived from the reply's own Slack message ts (unique per
// message within a channel), so a retry after a lost response — and any later DLQ
// replay of the same reply — resolves to the one bead the daemon already created
// rather than inserting a duplicate bubble.
func threadReplyIdempotencyKey(channel, messageTS string) string {
	return "thread-reply:" + channel + ":" + messageTS
}

// retryTransientDaemon runs fn, retrying on transient (retryable) errors with
// bounded exponential backoff. Non-transient errors (4xx other than 429, marshal
// failures surfaced by callers, etc.) return immediately — retrying them only
// wastes the handler budget. Each retry is counted per stage. Respects ctx
// cancellation between attempts.
func (b *Bot) retryTransientDaemon(ctx context.Context, stage string, fn func() error) error {
	var err error
	backoff := threadWriteBaseBackoff
	for attempt := 1; attempt <= threadWriteMaxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if !isTransientDaemonError(err) || attempt == threadWriteMaxAttempts {
			return err
		}
		threadReplyWriteRetries.WithLabelValues(stage).Inc()
		b.logger.Warn("thread-reply: transient daemon error, retrying",
			"stage", stage, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}

// isTransientDaemonError reports whether err is worth retrying: a 5xx or 429 from
// the daemon, or a transport-level failure (connection reset/refused, timeout,
// context deadline). A 4xx (other than 429) is a deterministic rejection —
// retrying it cannot help and would only burn the handler budget.
func isTransientDaemonError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests ||
			apiErr.StatusCode >= http.StatusInternalServerError
	}
	// Transport-level: context deadline, or a net error (conn reset/refused,
	// i/o timeout, EOF mid-flight). These are inherently retryable.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"i/o timeout",
		"eof",
		"no such host",
		"server misbehaving",
		"tls handshake timeout",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// enqueueThreadReplyDLQ pushes a failed reply onto the durable replay queue. It
// captures the effective body (never blank — a caption-less attachment reply
// carries its synthesized placeholder) so a replay does not depend on re-fetching
// Slack files. No-op (with a loud log) when state is nil (tests / stateless mode),
// since there is nowhere durable to hold the entry.
func (b *Bot) enqueueThreadReplyDLQ(in unifiedThreadReplyInput, body string) {
	if b.state == nil {
		b.logger.Error(in.LogPrefix+": thread reply lost — no state store for DLQ",
			"channel", in.Channel, "thread_ts", in.ThreadTS, "message_ts", in.MessageTS)
		threadReplyDLQDropped.WithLabelValues("no_state").Inc()
		return
	}
	entry := ThreadReplyDLQEntry{
		IdempotencyKey: threadReplyIdempotencyKey(in.Channel, in.MessageTS),
		Channel:        in.Channel,
		ThreadTS:       in.ThreadTS,
		MessageTS:      in.MessageTS,
		Text:           body, // effective, always-non-empty content
		SlackUser:      in.SlackUser,
		Project:        in.Project,
		AgentName:      in.AgentName,
		ExtraLabel:     in.ExtraLabel,
		Head:           in.Head,
		EnqueuedAt:     time.Now(),
	}
	if entry.Head == "" {
		entry.Head, _ = b.state.ThreadHead(in.Channel, in.ThreadTS)
	}
	evicted, err := b.state.EnqueueThreadReplyDLQ(entry)
	if err != nil {
		b.logger.Error(in.LogPrefix+": failed to enqueue thread reply to DLQ",
			"channel", in.Channel, "message_ts", in.MessageTS, "error", err)
		return
	}
	threadReplyDLQEnqueued.Inc()
	if evicted {
		threadReplyDLQDropped.WithLabelValues("overflow").Inc()
	}
	threadReplyDLQDepth.Set(float64(b.state.ThreadReplyDLQDepth()))
	b.logger.Info(in.LogPrefix+": thread reply enqueued to DLQ for replay",
		"channel", in.Channel, "message_ts", in.MessageTS, "depth", b.state.ThreadReplyDLQDepth())
}

// startThreadReplyDLQDrain runs the DLQ replay loop until ctx is cancelled. It
// drains once on startup (so a reply queued just before a restart replays
// promptly) then on each tick.
func (b *Bot) startThreadReplyDLQDrain(ctx context.Context) {
	if b.state == nil {
		return
	}
	ticker := time.NewTicker(threadReplyDLQInterval)
	defer ticker.Stop()
	b.drainThreadReplyDLQ(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.drainThreadReplyDLQ(ctx)
		}
	}
}

// drainThreadReplyDLQ replays every queued reply once. A successful replay is
// dequeued; a failed one has its attempt count bumped and is dropped once it
// exhausts threadReplyDLQMaxAttempts (a poisoned entry must not wedge the queue).
// The message create's idempotency key makes a replay of a write that actually
// landed a no-op at the daemon, so replay is safe even if the original failure
// was a lost response rather than a rejected write.
func (b *Bot) drainThreadReplyDLQ(ctx context.Context) {
	if b.state == nil {
		return
	}
	entries := b.state.AllThreadReplyDLQ()
	if len(entries) == 0 {
		threadReplyDLQDepth.Set(0)
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		in := unifiedThreadReplyInput{
			Channel:    e.Channel,
			ThreadTS:   e.ThreadTS,
			MessageTS:  e.MessageTS,
			Text:       e.Text,
			SlackUser:  e.SlackUser,
			Project:    e.Project,
			AgentName:  e.AgentName,
			ExtraLabel: e.ExtraLabel,
			Head:       e.Head,
			LogPrefix:  "thread-reply-dlq",
		}
		replayCtx, cancel := context.WithTimeout(ctx, threadReplyDLQReplayTimeout)
		// e.Text is the effective, always-non-empty body captured at enqueue time;
		// pass it straight through as the bead content. files are re-fetched only
		// for the (best-effort) attachment metadata fields.
		files := b.fetchMessageFiles(replayCtx, e.Channel, e.MessageTS)
		_, stage, err := b.writeThreadConversationBead(replayCtx, in, e.Text, files)
		cancel()
		if err == nil {
			if derr := b.state.DequeueThreadReplyDLQ(e.IdempotencyKey); derr != nil {
				b.logger.Warn("thread-reply-dlq: replayed but failed to dequeue",
					"key", e.IdempotencyKey, "error", derr)
			} else {
				threadReplyDLQReplayed.Inc()
				b.logger.Info("thread-reply-dlq: replayed queued reply",
					"channel", e.Channel, "message_ts", e.MessageTS)
			}
			continue
		}
		// Replay failed again. Bump attempts; drop the entry once it exhausts the
		// budget so a permanently-poisoned reply cannot wedge the queue forever.
		e.Attempts++
		if e.Attempts >= threadReplyDLQMaxAttempts {
			b.logger.Error("thread-reply-dlq: dropping reply after exhausting replay attempts",
				"channel", e.Channel, "message_ts", e.MessageTS, "attempts", e.Attempts,
				"stage", stage, "error", err)
			threadReplyDLQDropped.WithLabelValues("exhausted").Inc()
			_ = b.state.DequeueThreadReplyDLQ(e.IdempotencyKey)
			continue
		}
		if _, eerr := b.state.EnqueueThreadReplyDLQ(e); eerr != nil {
			b.logger.Warn("thread-reply-dlq: failed to persist bumped attempt count",
				"key", e.IdempotencyKey, "error", eerr)
		}
		b.logger.Warn("thread-reply-dlq: replay failed, will retry",
			"channel", e.Channel, "message_ts", e.MessageTS, "attempts", e.Attempts,
			"stage", stage, "error", err)
	}
	threadReplyDLQDepth.Set(float64(b.state.ThreadReplyDLQDepth()))
}
