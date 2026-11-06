package bridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Jira follow-up WAKE durability (epic ka-0000007401 — Slack parity, GAP 3a,
// task ka-0000002721).
//
// When a human posts a follow-up @gardenio comment on a ticket, the webhook
// wakes the assigned agent: a live agent is nudged (interrupt mail via NudgeFunc), a
// stopped one is re-spawned under the same name (session resume). That wake ran
// with NO transient retry — the resolver called SpawnAgent / the NudgeFunc
// exactly once, and deliverJiraFollowUp merely logged the error and returned.
// Because the webhook returns 200 regardless and there is no wake re-sync, a
// momentary daemon blip (rollout / restart / 5xx / conn-reset) during the wake
// silently failed to wake the agent: the human's follow-up landed in the bead
// conversation (that mirror is retried by ka-0000004763) but the agent was never
// nudged/respawned to read it, so the reply never came.
//
// Slack closed this same class on its reply path with a bounded transient retry
// (retryTransientDaemon, thread_reply_dlq.go). This is the Jira wake analog: it
// reuses the SAME package-level isTransientDaemonError classifier and the SAME
// threadWrite* backoff budget (3 attempts, 0.3s→0.6s) so both bridges absorb
// daemon blips identically.
//
// The wake runs SYNCHRONOUSLY inside the Jira webhook HTTP handler
// (handleJiraCommentWebhook → deliverJiraFollowUp, before the 200 is written),
// so the retry must stay well inside Jira's webhook delivery timeout. A booting
// agent needs no wait at all: its mail subscriber drains unread mail when it
// starts. The residual failure surface is a transient daemon error across
// FindAgentBead / SpawnAgent / the nudge mail write — exactly what this bounded
// retry covers (~0.9s of sleeps worst case).

// jiraWakeStage labels the two wake operations wrapped by the retry, so the
// metrics distinguish a dropped live-agent nudge from a dropped respawn.
const (
	jiraWakeStageNudge = "nudge" // live-agent nudge delivery (NudgeFunc / coop)
	jiraWakeStageSpawn = "spawn" // same-name respawn of a stopped agent
)

var (
	// gardenio_jira_followup_wake_retries_total counts individual transient-error
	// retry ATTEMPTS of a follow-up wake, by stage (nudge / spawn). Nonzero but
	// bounded means the retry is absorbing daemon blips as intended; a spike
	// tracking the failure counter means retries are exhausting.
	jiraFollowUpWakeRetries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_followup_wake_retries_total",
			Help: "Transient-error retry attempts of a Jira follow-up wake (nudge/respawn of the assigned agent), by stage (nudge/spawn).",
		},
		[]string{"stage"},
	)

	// gardenio_jira_followup_wake_failures_total counts TERMINAL wake failures —
	// a wake that survived the bounded transient retry (or was a deterministic
	// non-retryable error). Each increment is a human follow-up whose agent was
	// never woken, so the reply never came even though the comment reached the
	// bead. Non-flat means the conversational loop is silently stalling on
	// follow-ups.
	jiraFollowUpWakeFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_followup_wake_failures_total",
			Help: "Terminal failures of a Jira follow-up wake, by stage (nudge/spawn). Non-flat means follow-up comments are reaching the bead but never waking the agent.",
		},
		[]string{"stage"},
	)
)

// retryJiraWake runs fn, retrying transient (retryable) daemon errors with
// bounded exponential backoff — the Jira follow-up-wake analog of
// Bot.retryTransientDaemon (thread_reply_dlq.go) and retryJiraInboundWrite
// (jira_wake's sibling on the comment-mirror path). Non-transient errors (4xx
// other than 429, marshal failures, a missing bead) return immediately —
// retrying a deterministic rejection only wastes the synchronous webhook budget.
// Each retry ATTEMPT is counted per stage; the caller counts the terminal
// failure. The write budget (threadWriteMaxAttempts / threadWriteBaseBackoff)
// stays well inside the webhook handler's request lifetime. Respects ctx
// cancellation between attempts.
func retryJiraWake(ctx context.Context, stage string, logger *slog.Logger, fn func() error) error {
	var err error
	backoff := threadWriteBaseBackoff
	for attempt := 1; attempt <= threadWriteMaxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if !isTransientDaemonError(err) || attempt == threadWriteMaxAttempts {
			return err
		}
		jiraFollowUpWakeRetries.WithLabelValues(stage).Inc()
		if logger != nil {
			logger.Warn("jira-resolver: transient daemon error waking agent, retrying",
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
