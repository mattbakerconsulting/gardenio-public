package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Thread-reply durability metrics (ka-0000000612). recordUnifiedThreadReply used
// to be best-effort: a transient daemon failure (5xx / restart / conn-reset)
// during the head-resolve or message-create write dropped the human's reply from
// the beads-viewer /threads conversation lane with no retry and no alarm. The
// write now retries transient errors and, on terminal failure, enqueues the reply
// to a durable DLQ that a background loop replays. These series make each stage
// observable so a silent-drop regression is caught by a scrape rather than by a
// user noticing their message never rendered.
//
// Registered globally against the default registerer (promauto), matching the
// other bridge metrics (jira_metrics.go, gardener_resume_metrics.go). The
// slack-bridge serves /metrics from prometheus.DefaultGatherer (cmd/slack-bridge),
// so these series are scrapeable; in the controller binary the bot never runs, so
// the families stay empty.
var (
	// gardenio_thread_reply_write_failures_total counts terminal failures of the
	// conversation-bubble write, labelled by the stage that failed
	// (head_resolve / marshal / create). A terminal failure is one that survived
	// the bounded transient retry; each increment is a reply that would have been
	// silently dropped before this fix and is now (for the retryable stages)
	// enqueued to the DLQ instead.
	threadReplyWriteFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_thread_reply_write_failures_total",
			Help: "Terminal failures of the /threads conversation-bubble write, by stage (head_resolve/marshal/create).",
		},
		[]string{"stage"},
	)

	// gardenio_thread_reply_write_retries_total counts individual transient-error
	// retry attempts of the conversation-bubble write, by stage. A nonzero-but-
	// bounded rate here means the retry is absorbing daemon blips exactly as
	// intended; a spike that tracks the failure counter means retries are
	// exhausting.
	threadReplyWriteRetries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_thread_reply_write_retries_total",
			Help: "Transient-error retry attempts of the /threads conversation-bubble write, by stage (head_resolve/create).",
		},
		[]string{"stage"},
	)

	// gardenio_thread_reply_dlq_enqueued_total counts replies pushed onto the
	// durable replay DLQ after a terminal write failure.
	threadReplyDLQEnqueued = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gardenio_thread_reply_dlq_enqueued_total",
			Help: "Human thread replies enqueued to the durable DLQ after a terminal conversation-bubble write failure.",
		},
	)

	// gardenio_thread_reply_dlq_replayed_total counts replies successfully
	// re-persisted by the DLQ drain loop.
	threadReplyDLQReplayed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gardenio_thread_reply_dlq_replayed_total",
			Help: "Queued thread replies successfully re-persisted (drained) by the DLQ replay loop.",
		},
	)

	// gardenio_thread_reply_dlq_dropped_total counts replies permanently dropped
	// from the DLQ — either evicted when the bounded queue overflowed or aged out
	// after exhausting the replay attempt budget. This is the last-resort loss
	// signal: any nonzero value is a reply that never rendered and warrants a look.
	threadReplyDLQDropped = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_thread_reply_dlq_dropped_total",
			Help: "Queued thread replies permanently dropped from the DLQ, by reason (overflow/exhausted).",
		},
		[]string{"reason"},
	)

	// gardenio_thread_reply_dlq_depth is the current number of replies waiting in
	// the DLQ. A steadily-growing depth means the daemon has been unreachable long
	// enough that the drain loop cannot keep up — an alertable condition.
	threadReplyDLQDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "gardenio_thread_reply_dlq_depth",
			Help: "Current number of thread replies queued in the durable replay DLQ.",
		},
	)
)

// Zero-initialize the label children of the write-failure/drop/retry CounterVecs
// (ka-0000003771). A Prometheus *Vec exports NO series for a label combination
// until that combination is first observed, so a fleet that has never dropped a
// reply exports the parent family with zero children — the series
// gardenio_thread_reply_dlq_dropped_total / _write_failures_total are ABSENT from
// the scrape. The System Health "write-failure & DROP rate (silent-durability
// leak)" panel then queries series that do not exist and renders an empty green,
// so the one detector whose entire job is to catch a silent durability leak is
// itself silently dead. Pre-warming every known label to 0 makes the detector a
// flat-zero baseline that steps up the instant a real failure increments it —
// which is exactly what a leak detector must do. The sibling enqueued/replayed
// counters and the depth gauge are unlabeled and already self-register at 0, so
// they need no pre-warming; only the labeled families do.
//
// init() runs after the promauto var-init above registers the parents, so these
// touch existing collectors. It fires in every binary that imports this package,
// but only the slack-bridge (which runs the bot and serves DefaultGatherer on
// /metrics) actually scrapes them; in the other binaries the zero children are
// harmless (a counter that is genuinely 0 there and never increments).
func init() {
	for _, stage := range []string{threadStageHeadResolve, threadStageMarshal, threadStageCreate} {
		threadReplyWriteFailures.WithLabelValues(stage)
	}
	// marshal is deterministic and never retried, so it has no retry series.
	for _, stage := range []string{threadStageHeadResolve, threadStageCreate} {
		threadReplyWriteRetries.WithLabelValues(stage)
	}
	// The four terminal drop reasons emitted by the write + DLQ paths
	// (bot_thread_forward.go, thread_reply_dlq.go).
	for _, reason := range []string{"nontransient", "no_state", "overflow", "exhausted"} {
		threadReplyDLQDropped.WithLabelValues(reason)
	}
}
