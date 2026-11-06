package bridge

import (
	"context"
	"log/slog"
	"time"
)

// retryJiraInboundWrite runs fn, retrying transient (retryable) daemon errors
// with bounded exponential backoff — the Jira inbound-comment analog of
// Bot.retryTransientDaemon (thread_reply_dlq.go), reusing the same package-level
// isTransientDaemonError classifier and threadWrite* backoff budget so both
// bridges absorb daemon blips identically.
//
// A follow-up comment mirrored onto a task bead is best-effort today: on a
// transient daemon failure (5xx / restart / conn-reset) the comment is dropped
// from the bead conversation with no retry (jira_webhook.go), and the webhook
// still returns 200 with no re-sync — so the comment is permanently lost from
// the bead graph even though it lives in Jira. Wrapping the write in this
// bounded retry closes that silent-drop window for the common case (a momentary
// daemon rollout/restart) without a durable DLQ.
//
// Non-transient errors (4xx other than 429, a missing bead, marshal failures)
// return immediately — retrying a deterministic rejection only wastes the
// synchronous webhook-handler budget. Each retry ATTEMPT is counted per stage;
// the caller counts the terminal failure. The write budget (3 attempts,
// 0.3s→0.6s backoff) stays well inside the webhook handler's request lifetime.
// Respects ctx cancellation between attempts.
func retryJiraInboundWrite(ctx context.Context, stage string, logger *slog.Logger, fn func() error) error {
	var err error
	backoff := threadWriteBaseBackoff
	for attempt := 1; attempt <= threadWriteMaxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if !isTransientDaemonError(err) || attempt == threadWriteMaxAttempts {
			return err
		}
		jiraInboundCommentWriteRetries.WithLabelValues(stage).Inc()
		if logger != nil {
			logger.Warn("jira-webhook: transient daemon error mirroring comment, retrying",
				"stage", stage, "attempt", attempt, "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}
