package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Jira inbound-comment durability metrics (epic ka-0000007401 — Slack parity).
//
// handleJiraCommentWebhook mirrors a human follow-up comment onto the ticket's
// task bead (the conversation record the spawned agent reads and that the Slack
// bridge re-forwards). That write used to be single-shot best-effort: a
// transient daemon blip (rollout / restart / 5xx / conn-reset) during the
// mirror dropped the comment from the bead conversation with no retry and no
// alarm — the same silent-drop failure mode the Slack inbound reply path closed
// with a bounded transient retry + DLQ (ka-0000000612, thread_reply_dlq.go).
// The Jira webhook returns 200 regardless and there is no comment re-sync, so a
// dropped mirror was permanently lost from the bead graph even though the
// comment still lives in Jira.
//
// The mirror writes now retry transient errors (retryJiraInboundWrite). These
// series make each stage observable so a silent-drop regression is caught by a
// scrape rather than by an agent noticing a follow-up never reached its
// context. Agents cannot read pod logs (ka-0000006777), so the counter is the
// signal that the retry is absorbing daemon blips (retries nonzero, failures
// flat) versus terminally dropping comments (failures climbing).
//
// Registered globally against the default registerer (promauto), matching the
// other bridge metrics (jira_metrics.go, jira_mention_metrics.go). The
// jira-bridge serves /metrics from prometheus.DefaultGatherer, so these series
// are scrapeable; in the controller binary the webhook never runs, so the
// families stay empty.
var (
	// gardenio_jira_inbound_comment_write_retries_total counts individual
	// transient-error retry ATTEMPTS of an inbound comment-mirror write, by
	// stage (add_comment / set_fields). A nonzero-but-bounded rate means the
	// retry is absorbing daemon blips exactly as intended; a spike that tracks
	// the failure counter means retries are exhausting.
	jiraInboundCommentWriteRetries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_inbound_comment_write_retries_total",
			Help: "Transient-error retry attempts of an inbound Jira comment-mirror write onto a task bead, by stage (add_comment/set_fields).",
		},
		[]string{"stage"},
	)

	// gardenio_jira_inbound_comment_write_failures_total counts TERMINAL failures
	// of an inbound comment-mirror write — one that survived the bounded
	// transient retry (or was a deterministic non-retryable error). Each
	// increment is a comment (add_comment) or reply-anchor field update
	// (set_fields) that did not reach the bead conversation. add_comment
	// failures are the ones that silently drop a human follow-up; set_fields
	// failures leave a stale jira_comment_id reply anchor but do not lose the
	// comment text.
	jiraInboundCommentWriteFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_inbound_comment_write_failures_total",
			Help: "Terminal failures of an inbound Jira comment-mirror write onto a task bead, by stage (add_comment/set_fields). Non-flat means follow-up comments are being dropped from the bead conversation.",
		},
		[]string{"stage"},
	)

	// Jira inbound-comment DLQ metrics (epic ka-0000007401 — Slack parity with
	// thread_reply_metrics.go). The bounded transient retry above absorbs a
	// momentary daemon blip; a daemon down/overloaded long enough to EXHAUST that
	// retry (a rollout is typically longer than the ~0.9s retry budget) would
	// still terminally drop the mirror. The durable DLQ (jira_inbound_comment_dlq.go)
	// catches those add_comment terminal failures, persists them in the state
	// file, and replays them when the daemon recovers. These series make each DLQ
	// stage observable so a silent-drop regression is caught by a scrape.

	// gardenio_jira_inbound_comment_dlq_enqueued_total counts comment mirrors pushed
	// onto the durable replay DLQ after a transient-class terminal write failure.
	jiraInboundCommentDLQEnqueued = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gardenio_jira_inbound_comment_dlq_enqueued_total",
			Help: "Inbound Jira comment mirrors enqueued to the durable DLQ after a transient-class terminal write failure.",
		},
	)

	// gardenio_jira_inbound_comment_dlq_replayed_total counts comment mirrors
	// successfully re-persisted by the DLQ drain loop.
	jiraInboundCommentDLQReplayed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gardenio_jira_inbound_comment_dlq_replayed_total",
			Help: "Queued inbound Jira comment mirrors successfully re-persisted (drained) by the DLQ replay loop.",
		},
	)

	// gardenio_jira_inbound_comment_dlq_dropped_total counts comment mirrors
	// permanently dropped from the DLQ, by reason: overflow (bounded queue full),
	// exhausted (replay attempt budget spent), nontransient (a deterministic
	// terminal failure never queued in the first place). Any nonzero value is a
	// human follow-up that never reached the bead conversation and warrants a look.
	jiraInboundCommentDLQDropped = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_inbound_comment_dlq_dropped_total",
			Help: "Inbound Jira comment mirrors permanently dropped from the DLQ, by reason (overflow/exhausted/nontransient/no_state).",
		},
		[]string{"reason"},
	)

	// gardenio_jira_inbound_comment_dlq_depth is the current number of comment
	// mirrors waiting in the DLQ. A steadily-growing depth means the daemon has
	// been unreachable long enough that the drain loop cannot keep up.
	jiraInboundCommentDLQDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "gardenio_jira_inbound_comment_dlq_depth",
			Help: "Current number of inbound Jira comment mirrors queued in the durable replay DLQ.",
		},
	)
)
