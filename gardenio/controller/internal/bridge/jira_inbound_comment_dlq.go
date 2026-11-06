package bridge

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Inbound Jira comment durability: bounded transient retry + durable DLQ replay
// (epic ka-0000007401 — Slack parity with thread_reply_dlq.go / ka-0000000612).
//
// handleJiraCommentWebhook mirrors a human follow-up comment onto the ticket's
// task bead — the conversation record the spawned agent reads and the Slack
// bridge re-forwards. That mirror is best-effort: retryJiraInboundWrite absorbs a
// MOMENTARY daemon blip (a few hundred ms of 5xx / conn-reset), but a daemon
// down/overloaded long enough to EXHAUST the ~0.9s inline retry budget — a
// rollout, an OOM restart, a postgres failover — still terminally dropped the
// comment. The webhook returns 200 regardless and there is no comment re-sync, so
// the follow-up was permanently lost from the bead graph even though it lives in
// Jira. The Slack inbound reply path closed exactly this gap with a second layer:
// a durable DLQ that survives a bridge crash and replays on a ticker.
//
// This is that second layer for the Jira mirror. On a transient-class terminal
// add_comment failure the mirror is enqueued to a StateManager-backed queue;
// drainJiraInboundCommentDLQ runs on a ticker (and once on startup) and replays
// each queued mirror; a successful replay is dequeued; a permanently-failing
// entry is dropped after a bounded attempt budget so a poisoned entry cannot
// wedge the queue.
//
// EXACTLY-ONCE ON REPLAY. The kbeads comment-create endpoint has no idempotency
// key (unlike message-create on the Slack path), so a replay of a write whose
// FIRST attempt actually landed (a lost-response transient error) would double-
// post the mirror. To avoid that, the drain loop first reads the bead's existing
// comments and skips the re-post when the exact mirror body is already present.
// The mirror body embeds the Jira author + text and is stable across replays, so
// it is a reliable de-dup marker.

const (
	// jiraInboundCommentDLQInterval is the DLQ drain cadence. Mirrors
	// threadReplyDLQInterval.
	jiraInboundCommentDLQInterval = 30 * time.Second
	// jiraInboundCommentDLQMaxAttempts caps replay attempts before a queued mirror
	// is dropped as un-replayable, so a permanently-poisoned entry cannot wedge the
	// queue forever. Combined with the 30s cadence this is ~10 minutes of retries.
	// Mirrors threadReplyDLQMaxAttempts.
	jiraInboundCommentDLQMaxAttempts = 20
	// jiraInboundCommentDLQReplayTimeout bounds a single replay's daemon reads +
	// writes. Mirrors threadReplyDLQReplayTimeout.
	jiraInboundCommentDLQReplayTimeout = 20 * time.Second
)

// jiraInboundCommentDLQStore is the subset of StateManager the DLQ needs. An
// interface (rather than a concrete *StateManager) keeps the drain loop testable
// with an in-memory queue and lets the webhook config carry a nil DLQ (which
// preserves the pre-DLQ behavior — count + drop — exactly).
type jiraInboundCommentDLQStore interface {
	EnqueueJiraInboundCommentDLQ(e JiraInboundCommentDLQEntry) (evicted bool, err error)
	AllJiraInboundCommentDLQ() []JiraInboundCommentDLQEntry
	DequeueJiraInboundCommentDLQ(key string) error
	JiraInboundCommentDLQDepth() int
}

// jiraInboundCommentReplayClient is the daemon surface the drain loop needs to
// replay a mirror exactly-once: GetComments for the existence check, AddComment
// to re-post, and UpdateBeadFields to (re)set the reply anchor. It is a superset
// of the webhook's AddComment/UpdateBeadFields needs plus GetComments; the real
// *beadsapi.Client satisfies it. Kept separate from JiraWebhookBeadClient so
// adding GetComments here does not force every webhook test fake to implement it.
type jiraInboundCommentReplayClient interface {
	GetComments(ctx context.Context, beadID string) ([]beadsapi.Comment, error)
	AddComment(ctx context.Context, beadID, author, text string) error
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// jiraInboundCommentDLQ is the durable DLQ wiring: a state-backed queue plus the
// daemon client to replay against. A nil *jiraInboundCommentDLQ is a valid "no
// DLQ" value — enqueue on it is a no-op, so a webhook configured without a DLQ
// (tests, controller binary) keeps the exact pre-DLQ terminal-drop behavior.
type jiraInboundCommentDLQ struct {
	store  jiraInboundCommentDLQStore
	daemon jiraInboundCommentReplayClient
	logger *slog.Logger
}

// NewJiraInboundCommentDLQ builds a durable inbound-comment DLQ backed by the
// given state store and daemon client. Returns nil (a valid "no DLQ" value —
// every method is nil-safe) when either dependency is nil, so a caller that has
// no state manager degrades cleanly to the pre-DLQ count-and-drop behavior. Wire
// the result into JiraWebhookConfig.InboundCommentDLQ and start its drain loop
// with StartDrain.
func NewJiraInboundCommentDLQ(store jiraInboundCommentDLQStore, daemon jiraInboundCommentReplayClient, logger *slog.Logger) *jiraInboundCommentDLQ {
	if store == nil || daemon == nil {
		return nil
	}
	return &jiraInboundCommentDLQ{store: store, daemon: daemon, logger: logger}
}

// StartDrain runs the DLQ replay loop until ctx is cancelled (exported wrapper so
// cmd/jira-bridge can launch it in a goroutine). No-op on a nil receiver.
func (d *jiraInboundCommentDLQ) StartDrain(ctx context.Context) {
	d.startDrain(ctx)
}

// jiraInboundCommentDLQKey is the stable per-mirror key. Derived from the target
// bead and the originating Jira comment id so a re-enqueue of the same comment
// coalesces to one entry. When the comment id is absent (rare — Jira always
// stamps one on real events) the body is folded in so distinct comments on the
// same bead do not collide onto one key.
func jiraInboundCommentDLQKey(beadID, jiraCommentID, body string) string {
	if jiraCommentID != "" {
		return beadID + ":" + jiraCommentID
	}
	return beadID + ":body:" + body
}

// enqueue pushes a terminally-failed mirror onto the durable replay queue. It is
// a no-op (with a loud log + counted drop) when the DLQ or its store is nil,
// since there is nowhere durable to hold the entry — matching
// enqueueThreadReplyDLQ's nil-state handling. Safe to call on a nil receiver.
func (d *jiraInboundCommentDLQ) enqueue(e JiraInboundCommentDLQEntry) {
	if d == nil || d.store == nil {
		jiraInboundCommentDLQDropped.WithLabelValues("no_state").Inc()
		if d != nil && d.logger != nil {
			d.logger.Error("jira-webhook: inbound comment mirror lost — no state store for DLQ",
				"bead", e.BeadID, "jira_key", e.JiraKey, "jira_comment_id", e.JiraComment)
		}
		return
	}
	e.Key = jiraInboundCommentDLQKey(e.BeadID, e.JiraComment, e.CommentText)
	// Stamp the enqueue time so the 500-cap oldest-eviction (in the store) has a
	// real ordering key — matching enqueueThreadReplyDLQ. On a coalescing
	// re-enqueue the store preserves the original EnqueuedAt, so this is only the
	// timestamp for a genuinely-new entry.
	e.EnqueuedAt = time.Now()
	evicted, err := d.store.EnqueueJiraInboundCommentDLQ(e)
	if err != nil {
		d.logger.Error("jira-webhook: failed to enqueue inbound comment mirror to DLQ",
			"bead", e.BeadID, "jira_key", e.JiraKey, "error", err)
		return
	}
	jiraInboundCommentDLQEnqueued.Inc()
	if evicted {
		jiraInboundCommentDLQDropped.WithLabelValues("overflow").Inc()
	}
	jiraInboundCommentDLQDepth.Set(float64(d.store.JiraInboundCommentDLQDepth()))
	d.logger.Info("jira-webhook: inbound comment mirror enqueued to DLQ for replay",
		"bead", e.BeadID, "jira_key", e.JiraKey, "jira_comment_id", e.JiraComment,
		"depth", d.store.JiraInboundCommentDLQDepth())
}

// startDrain runs the DLQ replay loop until ctx is cancelled. It drains once on
// startup (so a mirror queued just before a restart replays promptly) then on
// each tick. No-op on a nil receiver / nil store. Mirrors
// startThreadReplyDLQDrain.
func (d *jiraInboundCommentDLQ) startDrain(ctx context.Context) {
	if d == nil || d.store == nil {
		return
	}
	ticker := time.NewTicker(jiraInboundCommentDLQInterval)
	defer ticker.Stop()
	d.drain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.drain(ctx)
		}
	}
}

// drain replays every queued mirror once. A successful replay (or a skip because
// the mirror is already present on the bead) is dequeued; a failed one has its
// attempt count bumped and is dropped once it exhausts the attempt budget so a
// poisoned entry cannot wedge the queue. Mirrors drainThreadReplyDLQ.
func (d *jiraInboundCommentDLQ) drain(ctx context.Context) {
	if d == nil || d.store == nil {
		return
	}
	entries := d.store.AllJiraInboundCommentDLQ()
	if len(entries) == 0 {
		jiraInboundCommentDLQDepth.Set(0)
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		replayCtx, cancel := context.WithTimeout(ctx, jiraInboundCommentDLQReplayTimeout)
		err := d.replayOne(replayCtx, e)
		cancel()
		if err == nil {
			if derr := d.store.DequeueJiraInboundCommentDLQ(e.Key); derr != nil {
				d.logger.Warn("jira-comment-dlq: replayed but failed to dequeue",
					"key", e.Key, "error", derr)
			} else {
				jiraInboundCommentDLQReplayed.Inc()
				d.logger.Info("jira-comment-dlq: replayed queued comment mirror",
					"bead", e.BeadID, "jira_key", e.JiraKey, "jira_comment_id", e.JiraComment)
			}
			continue
		}
		// Replay failed again. Bump attempts; drop the entry once it exhausts the
		// budget so a permanently-poisoned mirror cannot wedge the queue forever.
		e.Attempts++
		if e.Attempts >= jiraInboundCommentDLQMaxAttempts {
			d.logger.Error("jira-comment-dlq: dropping comment mirror after exhausting replay attempts",
				"bead", e.BeadID, "jira_key", e.JiraKey, "attempts", e.Attempts, "error", err)
			jiraInboundCommentDLQDropped.WithLabelValues("exhausted").Inc()
			_ = d.store.DequeueJiraInboundCommentDLQ(e.Key)
			continue
		}
		if _, eerr := d.store.EnqueueJiraInboundCommentDLQ(e); eerr != nil {
			d.logger.Warn("jira-comment-dlq: failed to persist bumped attempt count",
				"key", e.Key, "error", eerr)
		}
		d.logger.Warn("jira-comment-dlq: replay failed, will retry",
			"bead", e.BeadID, "jira_key", e.JiraKey, "attempts", e.Attempts, "error", err)
	}
	jiraInboundCommentDLQDepth.Set(float64(d.store.JiraInboundCommentDLQDepth()))
}

// replayOne re-mirrors a single queued comment exactly-once. It first checks the
// bead's existing comments for this exact mirror body: the comment-create
// endpoint has no idempotency key, so a lost-response transient failure may have
// actually landed the write, and blindly re-posting would double the bubble. If
// the body is already present the AddComment is skipped (treated as a success);
// otherwise it re-posts. Either way the reply-anchor fields are (re)set so a
// later gb jira reply threads to the right comment. A read/write error is
// returned so the caller bumps the attempt count and retries later.
func (d *jiraInboundCommentDLQ) replayOne(ctx context.Context, e JiraInboundCommentDLQEntry) error {
	existing, err := d.daemon.GetComments(ctx, e.BeadID)
	if err != nil {
		return err
	}
	alreadyPresent := false
	for _, c := range existing {
		if strings.TrimSpace(c.Text) == strings.TrimSpace(e.CommentText) {
			alreadyPresent = true
			break
		}
	}
	if !alreadyPresent {
		if err := d.daemon.AddComment(ctx, e.BeadID, "jira-bridge", e.CommentText); err != nil {
			return err
		}
	}
	// (Re)set the reply-anchor fields. This is idempotent — setting the same
	// fields twice is harmless — so it runs whether or not the comment itself was
	// re-posted, closing the set_fields half of the original terminal failure too.
	if e.SetHasComments || e.JiraComment != "" {
		fields := map[string]string{}
		if e.SetHasComments {
			fields["jira_has_comments"] = "true"
		}
		if e.JiraComment != "" {
			fields["jira_comment_id"] = e.JiraComment
		}
		if err := d.daemon.UpdateBeadFields(ctx, e.BeadID, fields); err != nil {
			return err
		}
	}
	return nil
}
