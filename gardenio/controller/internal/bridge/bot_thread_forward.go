package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/podmanager"

	"github.com/slack-go/slack"
)

// threadNudgeInterval is the minimum time between nudges for the same agent+thread.
const threadNudgeInterval = 30 * time.Second

// threadResumeHintInterval is the minimum time between resume-hint posts for
// the same thread when a reply arrives after the bound agent has exited.
const threadResumeHintInterval = 1 * time.Hour

// handleThreadForward records the human's reply as a single conversation
// message bead AND nudges the bound agent off that same bead when a non-mention
// message is posted in an agent thread.
//
// Unify model (ka-0000007618, decision ka-0000005492 → option C): there is now
// ONE bead per reply, not two. Previously this path minted a closed type=message
// "bubble" (for the /threads view) AND a separate type=task "Thread: …" tracker
// (assigned to the agent, used purely as the nudge anchor) — the tracker was
// linked to the head only by a triggered-by edge, so it never rendered in the
// thread view and just leaked open forever (nothing auto-closes it). The reply
// bead IS the message AND carries the taskiness: it is assigned to the agent and
// is the nudge target. Because it is type=message (kind=data) it does NOT enter
// the `gb ready` work queue (kind=issue only), which also removes the queue
// pollution the old type=task tracker caused.
//
// When the bound agent has exited (for any reason — clean stop, crash, yield,
// context cancel), we do NOT auto-respawn. Auto-respawn on incidental thread
// replies surprises users by resurrecting agents for days after the original
// work finished. Instead, a throttled hint in the thread tells the user to
// @mention the bot to resume — the mention path preserves session continuity
// via the existing PVC + session JSONL.
//
// The conversation bubble is recorded for EVERY reply (even rapid-fire ones, so
// nothing is hidden from the thread view — ka-0000003279), but the agent NUDGE
// is throttled to avoid spamming a busy thread. Throttled messages are still
// visible in the Slack thread and in the /threads view; the agent reads them via
// `gb slack thread` on its next interaction.
//
// It is the Slack half of HandleReply (slice 4c): in is a Slack reply, whose
// thread root is its Slack channel and thread_ts.
func (b *Bot) handleThreadForward(ctx context.Context, in conversation.Inbound, agent string) {
	agentName := extractAgentName(agent)
	root, _ := in.Root(conversation.SurfaceSlack)

	// Resolve the agent's liveness + project, but do NOT return early on a dead
	// agent. Whether or not the bound agent is still active, the human's words
	// must be recorded as a conversation bubble so /threads never drops a turn
	// (ka-0000001743: "threads view dropping user messages"). FindAgentBead
	// returns ErrAgentNotFound the moment the agent bead leaves an active status
	// (clean stop / crash / yield / ctx-cancel), so gating the record on it lost
	// every post-termination human follow-up from the thread view.
	agentBead, agentErr := b.daemon.FindAgentBead(ctx, agentName)
	agentAlive := agentErr == nil
	var agentProject string
	if agentBead != nil {
		agentProject = agentBead.Fields["project"]
	}

	// Record the human's words as a conversation message bound to the thread
	// (wish) head so it renders in the beads-viewer /threads conversation lane.
	// Done UNCONDITIONALLY and BEFORE the agent-liveness branch below —
	// recordThreadConversationMessage resolves/creates the wish head by its
	// canonical slack-thread:{channel}:{ts} label (getOrCreateThreadBead) and
	// needs no live agent; the agentName only becomes the message bead's
	// free-form Assignee, which the daemon accepts regardless of agent liveness.
	// This restores the invariant stated below (every human follow-up must be
	// visible in the thread view) for the dead-agent case too.
	//
	// A thread bound to a laptop agent (placement.go) is the runner owner's
	// own machine: a reply from anyone else is recorded, unassigned, and not
	// mailed to it, so a second person in a shared channel cannot drive the
	// owner's laptop. This matches /runner's rule (runnerCaller/ownsRunner).
	if agentAlive && isLaptopAgent(agentBead) && !b.laptopAgentOwnedBy(ctx, agentBead, in.Actor.SlackUser) {
		b.logger.Info("thread-forward: reply from a non-owner in a laptop agent's thread; recorded, not forwarded",
			"agent", agentName, "user", in.Actor.SlackUser, "channel", root.Channel, "thread_ts", root.TS)
		b.recordThreadConversationMessage(ctx, in, agentProject, "")
		return
	}
	beadID := b.recordThreadConversationMessage(ctx, in, agentProject, agentName)

	// The agent is gone (done / crashed / yielded / pruned): the conversation
	// bubble is already recorded above, so /threads keeps the turn. We do NOT
	// auto-respawn on an incidental thread reply (that surprises users by
	// resurrecting long-finished agents); instead post a throttled hint telling
	// the user to @mention the bot to resume — the mention path preserves session
	// continuity via the PVC + session JSONL.
	if !agentAlive {
		b.logger.Info("thread-forward: agent no longer active, recorded reply + posting resume hint (no auto-respawn)",
			"agent", agentName, "bead", beadID, "channel", root.Channel, "thread_ts", root.TS)
		if b.state != nil {
			_ = b.state.TouchThreadAgent(root.Channel, root.TS)
		}
		b.postThreadResumeHint(ctx, root.Channel, root.TS, agentName)
		return
	}

	// Refresh the TTL on this thread→agent binding so active threads
	// don't get cleaned up by the 24h inactivity expiry. Done after
	// the agent validation to avoid serializing concurrent forwards
	// through the state mutex before the spawnInFlight guard.
	if b.state != nil {
		_ = b.state.TouchThreadAgent(root.Channel, root.TS)
	}

	// Throttle check: skip the agent nudge for rapid-fire thread replies so a
	// busy thread doesn't hammer the agent. The conversation bubble above is
	// always recorded; only the nudge is throttled.
	if b.shouldThrottleNudge(agentName, root.TS) {
		b.logger.Debug("thread-forward: skipping nudge (throttled)",
			"agent", agentName, "channel", root.Channel)
		return
	}

	// Body for the agent nudge. For a file/image-only reply the text is empty, so
	// fall back to a short attachment placeholder ("shared an image") — otherwise
	// the agent would be nudged with a blank message and have no idea what the
	// user did (ka-0000003513). The bead itself already carries the attachment
	// metadata for the agent to inspect via `gb slack thread`.
	nudgeBody := strings.TrimSpace(in.Text)
	if nudgeBody == "" {
		nudgeBody = attachmentPlaceholderText(b.fetchMessageFiles(ctx, in.At.Channel, in.At.TS))
	}
	if nudgeBody == "" {
		nudgeBody = "(no text)"
	}

	if beadID == "" {
		// The conversation bubble failed to persist (logged in
		// recordThreadConversationMessage). Nudge with the raw text anyway so the
		// agent is never silently left out — NudgeAgent needs only agent+text.
		message := fmt.Sprintf("Slack thread reply: %s", truncateText(nudgeBody, 200))
		if err := NudgeAgent(ctx, b.daemon, b.logger, agentName, message); err != nil {
			b.logger.Error("failed to nudge agent for thread forward (no bead)",
				"agent", agentName, "error", err)
		}
		return
	}

	b.logger.Info("thread-forward: recorded reply message + nudging",
		"bead", beadID, "agent", agentName)

	// Persist message ref for response relay (kept for parity with the mention
	// path; chat.go relays a close-reason back to Slack if the agent closes a
	// labeled bead, though agents normally reply via `gb slack reply`).
	if b.state != nil {
		if err := b.state.SetChatMessage(beadID, MessageRef{
			ChannelID: root.Channel,
			Timestamp: root.TS,
			Agent:     agent,
		}); err != nil {
			b.logger.Warn("thread-forward: failed to persist chat message ref",
				"bead", beadID, "error", err)
		}
	}

	// Nudge the agent with the thread reply, anchored on the unified message bead.
	message := fmt.Sprintf("Slack thread reply (bead %s): %s", beadID, truncateText(nudgeBody, 200))
	if err := NudgeAgent(ctx, b.daemon, b.logger, agentName, message); err != nil {
		b.logger.Error("failed to nudge agent for thread forward",
			"agent", agentName, "bead", beadID, "error", err)
	}
}

// isLaptopAgent reports whether agent bead a runs on its owner's laptop under
// a runner (gb runner join stamps the field and the label).
func isLaptopAgent(a *beadsapi.BeadDetail) bool {
	return a != nil && (podmanager.IsLaptopTarget(a.Fields["execution_target"]) ||
		slices.Contains(a.Labels, "execution_target:"+podmanager.ExecutionTargetLaptop))
}

// laptopAgentOwnedBy reports whether slackUser owns the runner laptop agent a
// runs on (its runtime_id). An unreadable runner owns nothing: fail closed.
func (b *Bot) laptopAgentOwnedBy(ctx context.Context, a *beadsapi.BeadDetail, slackUser string) bool {
	rid := strings.TrimSpace(a.Fields["runtime_id"])
	if rid == "" {
		return false
	}
	r, err := b.daemon.GetBead(ctx, rid)
	if err != nil || r == nil || r.Type != "runner" {
		b.logger.Warn("thread-forward: laptop agent's runner not readable", "agent", a.ID, "runtime_id", rid, "error", err)
		return false
	}
	return ownsRunner(b.runnerCaller(ctx, slackUser), r)
}

// recordThreadConversationMessage writes the human's thread reply as a SINGLE
// closed type=message bead that both renders in the beads-viewer /threads
// conversation lane AND carries the taskiness for the agent nudge — the unify
// model (ka-0000007618, decision ka-0000005492 → option C). It returns the bead
// id so the caller can nudge the agent against it; returns "" on any failure
// (the caller falls back to a bead-less nudge so the agent is never skipped).
//
// The bead:
//   - is bound to the thread (wish) head via the wish-pointer field thread_id,
//     so the viewer's UNION(fields.thread_id == headID, parent-child closure)
//     membership (cmd/beads-viewer/threads.go) renders it as a conversation
//     bubble (isThreadMessageBead);
//   - carries the slack-thread-reply label + the originating operator
//     (operated_by + X-Operated-By context) so the beads-viewer per-user inbox
//     attributes the "USER → AGENT" bubble to the human (user_inbox.go now
//     queries type=message for that label, not type=task);
//   - is ASSIGNED to the agent — it IS the work item, replacing the old separate
//     type=task "Thread: …" tracker. type=message is kind=data, so it never
//     enters the `gb ready` work queue;
//   - carries slack_user_id (per-reply author) + any attachment metadata.
//
// This is INBOUND (Slack → bead): it carries NO `thread-say` label, so the
// ThreadMessageForward watcher (which relays OUTBOUND composer messages back
// into Slack) does not pick it up and echo it — that would double-post the
// user's own message into their thread. CreatedBy is the resolved operator so
// the bubble attributes to the human, not the bot. The bead is closed
// immediately (a conversation utterance has no work lifecycle of its own; the
// nudge is what drives the agent). Best-effort: every failure is logged and
// swallowed, and a "" return lets the caller nudge without a bead.
func (b *Bot) recordThreadConversationMessage(ctx context.Context, in conversation.Inbound, project, agentName string) string {
	root, _ := in.Root(conversation.SurfaceSlack)
	return b.recordUnifiedThreadReply(ctx, unifiedThreadReplyInput{
		Channel:    root.Channel,
		ThreadTS:   root.TS,
		MessageTS:  in.At.TS,
		Text:       in.Text,
		SlackUser:  in.Actor.SlackUser,
		Project:    project,
		AgentName:  agentName,
		ExtraLabel: "slack-thread-reply",
		LogPrefix:  "thread-forward",
	})
}

// unifiedThreadReplyInput collects everything recordUnifiedThreadReply needs.
// The two callers (handleThreadForward and handleAppMention's live-agent path)
// gather these fields from different SlackEvent shapes (MessageEvent vs
// AppMentionEvent) and the mention path may need to anchor the reply on the
// mention's OWN timestamp rather than the parent thread's, so the helper takes
// explicit, event-shape-agnostic args.
type unifiedThreadReplyInput struct {
	Channel    string // Slack channel id
	ThreadTS   string // parent thread ts (the wish anchor)
	MessageTS  string // this message's own ts (for attachment fetch)
	Text       string // raw message text (post bot-mention strip for mentions)
	SlackUser  string // ev.User — the originating Slack user id
	Project    string // agent project (for thread-head creation)
	AgentName  string // canonical agent name to assign the bead to
	ExtraLabel string // "slack-thread-reply" or "slack-mention" — what the per-user inbox queries on
	LogPrefix  string // log scope prefix for messages ("thread-forward" or "mention")
	Head       string // the thread's wish head, when known (a DLQ replay); else resolved from the Slack thread
}

// recordUnifiedThreadReply writes the human's reply as a SINGLE closed
// type=message bead that both renders in the beads-viewer /threads conversation
// lane AND carries the taskiness for the agent nudge — the unify model
// (ka-0000007618, decision ka-0000005492 → option C). It returns the bead id so
// the caller can nudge the agent against it; returns "" on any failure (the
// caller falls back to a bead-less nudge so the agent is never skipped).
//
// The bead:
//   - is bound to the thread (wish) head via the wish-pointer field thread_id,
//     so the viewer's UNION(fields.thread_id == headID, parent-child closure)
//     membership (cmd/beads-viewer/threads.go) renders it as a conversation
//     bubble (isThreadMessageBead);
//   - carries the slack-thread-reply or slack-mention label + the originating
//     operator (operated_by + X-Operated-By context) so the beads-viewer
//     per-user inbox attributes the "USER → AGENT" bubble to the human
//     (user_inbox.go queries type=message for those labels);
//   - is ASSIGNED to the agent — it IS the work item, replacing the old
//     separate type=task "Thread: …" / "Mention: …" tracker. type=message is
//     kind=data, so it never enters the `gb ready` work queue;
//   - carries slack_user_id (per-reply author) + any attachment metadata.
//
// This is INBOUND (Slack → bead): it carries NO `thread-say` label, so the
// ThreadMessageForward watcher (which relays OUTBOUND composer messages back
// into Slack) does not pick it up and echo it — that would double-post the
// user's own message into their thread. CreatedBy is the resolved operator so
// the bubble attributes to the human, not the bot. The bead is closed
// immediately (a conversation utterance has no work lifecycle of its own; the
// nudge is what drives the agent).
//
// Durability (ka-0000000612): the two daemon writes (thread-head resolve and
// message create) are RETRIED on transient errors (5xx / 429 / conn-reset /
// timeout) with bounded backoff, and the message create carries an idempotency
// key (thread-reply:{channel}:{messageTS}) so a retry after a lost response — or
// a later DLQ replay — collapses to exactly ONE bead at the daemon. On terminal
// failure of a retryable stage the reply is enqueued to a durable DLQ that a
// background loop replays; the failure is logged at ERROR and counted so the
// loss is observable rather than silently swallowed as it was before. A "" return
// still lets the caller nudge the agent without a bead.
func (b *Bot) recordUnifiedThreadReply(ctx context.Context, in unifiedThreadReplyInput) string {
	text := strings.TrimSpace(in.Text)

	// Fetch attachment metadata up-front, BEFORE the empty-text gate below. Slack
	// dispatches a threaded file/image upload with an empty (or caption-only)
	// ev.Text, so gating on trimmed text alone dropped every attachment-only
	// follow-up from the /threads conversation lane (ka-0000003513). Fetching here
	// lets an attachment-only reply still persist a bubble. fetchMessageFiles
	// returns nil (no API call) when b.api is nil (tests) or the message carries
	// no files, so the text-present path is unchanged.
	files := b.fetchMessageFiles(ctx, in.Channel, in.MessageTS)

	// Nothing to record: no caption AND no attachments. (A bare join/system
	// message, or a message whose only content the bridge can't see.)
	if text == "" && len(files) == 0 {
		return ""
	}

	// Body: the caption when present, else a short placeholder describing the
	// attachments so the bubble is legible instead of blank. This is the
	// effective, always-non-empty content the DLQ entry carries so a replay
	// never depends on re-fetching files from Slack.
	body := text
	if body == "" {
		body = attachmentPlaceholderText(files)
	}

	msgID, stage, err := b.writeThreadConversationBead(ctx, in, body, files)
	if err != nil {
		// Terminal failure after the bounded transient retry. Raise to ERROR (was
		// a swallowed Warn) and count it so a silent-drop regression is scrapeable.
		threadReplyWriteFailures.WithLabelValues(stage).Inc()
		b.logger.Error(in.LogPrefix+": conversation bubble write failed after retries",
			"stage", stage, "channel", in.Channel, "thread_ts", in.ThreadTS,
			"message_ts", in.MessageTS, "error", err)
		// Only a TRANSIENT-class terminal failure is worth queuing: a daemon that
		// was down/overloaded long enough to exhaust the inline retry will likely
		// recover, so the DLQ replay lands the bubble later. A deterministic
		// failure (marshal, or a 4xx validation rejection) would fail identically
		// on every replay — queuing it just churns the DLQ until it ages out, so
		// we drop it here (logged + counted) instead. The message create carries
		// an idempotency key, so a transient failure that was really a lost
		// response replays as a no-op rather than a duplicate.
		if isTransientDaemonError(err) {
			b.enqueueThreadReplyDLQ(in, body)
		} else {
			threadReplyDLQDropped.WithLabelValues("nontransient").Inc()
		}
		return ""
	}
	return msgID
}

// Conversation-bubble write stages. Used as the label on the retry/failure
// metrics and as the returned stage marker so recordUnifiedThreadReply can tell
// which step failed. Note marshal is deterministic (never retried); the two
// daemon-write stages are transient-retryable.
const (
	threadStageHeadResolve = "head_resolve"
	threadStageMarshal     = "marshal"
	threadStageCreate      = "create"
)

// writeThreadConversationBead performs the daemon writes for one conversation
// bubble: resolve-or-create the thread head, marshal the message fields, create
// the closed type=message bead, and (best-effort) add the lineage edge. body is
// the always-non-empty effective content and files carries the (possibly nil)
// attachment metadata. Both callers — the live recordUnifiedThreadReply and the
// DLQ replay path — funnel through here so the retry + idempotency semantics are
// identical.
//
// Returns (beadID, "", nil) on success; ("", stage, err) when a stage failed
// after its bounded transient retry. The head-resolve and create writes retry
// 5xx/429/transport errors; marshal is a local, deterministic operation and is
// not retried. The create carries an idempotency key so a retry after a lost
// response — or a DLQ replay of a write that actually landed — returns the
// existing bead instead of a duplicate bubble.
func (b *Bot) writeThreadConversationBead(ctx context.Context, in unifiedThreadReplyInput, body string, files []slack.File) (beadID, stage string, err error) {
	// Resolve (or create) the thread head so the message has a wish-pointer to
	// bind to. Idempotent on the canonical slack-thread:{channel}:{ts} label, so
	// the retry below is safe — a head created on a lost-response attempt is
	// returned, not duplicated.
	headID := in.Head
	if headID == "" && b.state != nil {
		headID, _ = b.state.ThreadHead(in.Channel, in.ThreadTS)
	}
	headErr := b.retryTransientDaemon(ctx, threadStageHeadResolve, func() error {
		if headID != "" {
			return nil
		}
		id, _, e := b.getOrCreateThreadBead(ctx, in.Project, threadBeadFields{
			Channel:       in.Channel,
			ThreadTS:      in.ThreadTS,
			SpawnChannel:  in.Channel,
			OpenerUserID:  in.SlackUser,
			RootText:      in.Text,
			RootAtSlackTS: in.ThreadTS,
		})
		if e != nil {
			return e
		}
		if id == "" {
			return fmt.Errorf("empty thread head id")
		}
		headID = id
		return nil
	})
	if headErr != nil {
		return "", threadStageHeadResolve, headErr
	}
	if b.state != nil {
		_ = b.state.BindThreadHead(in.Channel, in.ThreadTS, headID)
	}
	// A reply to the herder's "fulfilled" note contests it (gd-EfbganTatT).
	b.reopenHerderClosedWish(ctx, headID)

	creator := b.operatorFromSlackUser(ctx, in.SlackUser)
	fieldsMap := map[string]string{
		"text":                 body,
		"slack_thread_channel": in.Channel,
		"slack_thread_ts":      in.ThreadTS,
		"thread_id":            headID, // wish-pointer: thread derivatives carry thread_id == head id
		"slack_user_id":        in.SlackUser,
		"operated_by":          creator,                   // originating operator, for the per-user inbox attribution
		"origin":               conversation.SurfaceSlack, // the mirror loop guard (commons_mirror.go)
	}
	// Enrich with file attachment metadata (slack_attachment_count /
	// slack_has_images) so the unified bead carries the same affordances the old
	// tracker did.
	maps.Copy(fieldsMap, slackFilesToFields(files))
	fieldsJSON, mErr := json.Marshal(fieldsMap)
	if mErr != nil {
		return "", threadStageMarshal, mErr
	}

	labels := []string{"slack-thread-message", threadIdentityLabel(headID)}
	if in.ExtraLabel != "" {
		labels = append(labels, in.ExtraLabel)
	}

	// Commons dual-write (channel_stamp.go): stamp the channel bead the reply
	// was said in, with its land label, which the daemon requires on a message
	// that names a channel. The unstamped body stays as the fallback below.
	stampedFields, stampedLabels := fieldsJSON, labels
	if ref, ok := b.resolveChannelBead(ctx, in.Channel); ok {
		if addLabel, ok := ref.stampFor(labels); ok {
			stamped := maps.Clone(fieldsMap)
			stamped["channel_id"] = ref.ID
			if j, err := json.Marshal(stamped); err == nil {
				stampedFields = j
				if addLabel != "" {
					stampedLabels = append(slices.Clone(labels), addLabel)
				}
			}
		}
	}

	// Stamp the originating operator on the write context so the daemon binds
	// operated_by_id (the FK the per-user inbox query filters on); see
	// user_inbox.go OperatedByID. WithOperator overrides the client default.
	writeCtx := ctx
	if isHumanShapedOperator(creator) {
		writeCtx = beadsapi.WithOperator(ctx, creator)
	}

	var msgID string
	create := func(labels []string, fieldsJSON []byte) error {
		return b.retryTransientDaemon(ctx, threadStageCreate, func() error {
			id, e := b.daemon.CreateBead(writeCtx, beadsapi.CreateBeadRequest{
				Title:          truncateText(body, 500),
				Type:           "message",
				Kind:           "data",
				Assignee:       in.AgentName, // the message IS the task: assigned to the bound agent
				Labels:         labels,
				Fields:         json.RawMessage(fieldsJSON),
				CreatedBy:      creator,
				IdempotencyKey: threadReplyIdempotencyKey(in.Channel, in.MessageTS),
			})
			if e != nil {
				return e
			}
			msgID = id
			return nil
		})
	}
	createErr := create(stampedLabels, stampedFields)
	if createErr != nil && !bytes.Equal(stampedFields, fieldsJSON) {
		// The channel stamp is a dual-write nothing reads yet, so it must never
		// cost the human's reply: write it unstamped. This covers a daemon
		// refusal (a channel re-bound or deleted since the cached lookup) and a
		// DLQ replay of a reply that first landed unstamped, before its land was
		// seeded: the daemon's idempotency lookup is scoped to the stamped
		// write's project label, so it misses that bead and the unique index
		// fails the insert, while the unlabelled retry finds and returns it.
		b.logger.Warn(in.LogPrefix+": stamped conversation bead failed, writing it without channel_id",
			"error", createErr)
		createErr = create(labels, fieldsJSON)
	}
	if createErr != nil {
		return "", threadStageCreate, createErr
	}

	// Lineage edge (auto-link M2d, epic ka-0000000933): record a `triggered-by`
	// dep from the reply message to the Slack thread head so the spawn graph
	// captures Slack-thread provenance even on forwards that catch up to a
	// long-running agent. Best-effort; the thread_id field already makes it a
	// thread member, so a failure here only loses the explicit graph edge.
	if err := b.daemon.AddDependency(writeCtx, msgID, headID, "triggered-by", "slack-bridge"); err != nil {
		b.logger.Warn(in.LogPrefix+": failed to add triggered-by dep to thread head",
			"bead", msgID, "thread_bead", headID, "error", err)
	}

	// Close the message bead so it reaches its terminal state immediately — a
	// conversation utterance has no work lifecycle. (No `thread-say` label, so
	// ThreadMessageForward does not relay it back into Slack on this close.)
	if err := b.daemon.CloseBead(writeCtx, msgID, nil); err != nil {
		b.logger.Warn(in.LogPrefix+": close conversation message bead failed",
			"id", msgID, "error", err)
	}
	return msgID, "", nil
}

// shouldThrottleNudge returns true if a nudge was sent recently for this agent+thread.
// Uses persistent state if available (survives restarts), falling back to in-memory map.
func (b *Bot) shouldThrottleNudge(agent, threadTS string) bool {
	key := agent + ":" + threadTS

	// Use persistent state when available.
	if b.state != nil {
		throttled, err := b.state.CheckAndSetNudgeThrottle(key, threadNudgeInterval)
		if err != nil {
			b.logger.Warn("thread-forward: failed to persist nudge throttle", "key", key, "error", err)
		}
		if throttled {
			b.logger.Debug("thread-forward: nudge throttled (persisted)",
				"agent", agent, "thread_ts", threadTS)
			return true
		}
		return false
	}

	// Fallback to in-memory map when state is nil (tests).
	b.mu.Lock()
	defer b.mu.Unlock()
	if last, ok := b.lastThreadNudge[key]; ok && time.Since(last) < threadNudgeInterval {
		b.logger.Debug("thread-forward: nudge throttled",
			"agent", agent, "thread_ts", threadTS, "last_nudge_ago", time.Since(last))
		return true
	}
	b.lastThreadNudge[key] = time.Now()
	return false
}

// postThreadResumeHint posts a throttled one-line hint in a thread whose bound
// agent has exited, nudging the user to @mention the bot to resume. The throttle
// is keyed on (agent, thread) and uses persistent state when available so a
// restart does not re-spam the hint.
func (b *Bot) postThreadResumeHint(ctx context.Context, channel, threadTS, agentName string) {
	if b.api == nil {
		return
	}
	key := "resume-hint:" + agentName + ":" + threadTS
	if b.state != nil {
		throttled, err := b.state.CheckAndSetNudgeThrottle(key, threadResumeHintInterval)
		if err != nil {
			b.logger.Warn("thread-forward: failed to persist resume-hint throttle", "key", key, "error", err)
		}
		if throttled {
			return
		}
	} else {
		b.mu.Lock()
		if last, ok := b.lastThreadNudge[key]; ok && time.Since(last) < threadResumeHintInterval {
			b.mu.Unlock()
			return
		}
		b.lastThreadNudge[key] = time.Now()
		b.mu.Unlock()
	}

	root := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: threadTS}
	if _, err := b.conversationSlack().Reply(ctx, root, conversation.Message{Text: ":sleeping:"}, false); err != nil {
		b.logger.Warn("thread-forward: failed to post resume hint",
			"channel", channel, "thread_ts", threadTS, "agent", agentName, "error", err)
	}
}

// fetchMessageFiles re-fetches a single message to extract its Files array.
// Both AppMentionEvent and MessageEvent in slack-go lack a Files field,
// so we call GetConversationReplies with Limit:1+Inclusive to get the full message.
func (b *Bot) fetchMessageFiles(ctx context.Context, channel, ts string) []slack.File {
	if b.api == nil {
		return nil
	}
	msgs, _, _, err := b.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: ts,
		Limit:     1,
		Inclusive: true,
	})
	if err != nil {
		b.logger.Debug("failed to fetch message files", "channel", channel, "ts", ts, "error", err)
		return nil
	}
	for _, msg := range msgs {
		if msg.Timestamp == ts {
			return msg.Files
		}
	}
	return nil
}

// attachmentPlaceholderText builds a short, human-legible body for a thread
// reply that carries attachments but no caption. Slack sends an image/file-only
// thread upload with empty text, so without a synthesized body the /threads
// conversation bubble would render blank (ka-0000003513). Uses an image/file
// emoji + a count + the first filename(s) so the operator can tell what was
// shared at a glance. Returns "" when there are no files (caller keeps its own
// text in that case).
func attachmentPlaceholderText(files []slack.File) string {
	if len(files) == 0 {
		return ""
	}
	hasImage := false
	for _, f := range files {
		if strings.HasPrefix(f.Mimetype, "image/") {
			hasImage = true
			break
		}
	}
	icon := ":paperclip:"
	noun := "file"
	if hasImage {
		icon = ":frame_with_picture:"
		noun = "image"
	}
	if len(files) == 1 {
		name := strings.TrimSpace(files[0].Name)
		if name == "" {
			return fmt.Sprintf("%s _(shared a %s)_", icon, noun)
		}
		return fmt.Sprintf("%s _(shared %s)_", icon, name)
	}
	return fmt.Sprintf("%s _(shared %d %ss)_", icon, len(files), noun)
}

// slackFilesToFields returns bead fields for attachment metadata.
func slackFilesToFields(files []slack.File) map[string]string {
	if len(files) == 0 {
		return nil
	}
	fields := map[string]string{
		"slack_attachment_count": fmt.Sprintf("%d", len(files)),
	}
	for _, f := range files {
		if strings.HasPrefix(f.Mimetype, "image/") {
			fields["slack_has_images"] = "true"
			break
		}
	}
	// Where each file opens, so the Commons can show it (commons_mirror.go).
	if refs := slackFileRefs(files); len(refs) > 0 {
		raw, _ := json.Marshal(refs) // a []fileRef always marshals
		fields[conversation.BeadFiles] = string(raw)
	}
	return fields
}
