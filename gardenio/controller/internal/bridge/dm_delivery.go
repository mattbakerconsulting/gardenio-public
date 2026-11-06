package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// DMDeliveryConfig holds configuration for the DM delivery watcher.
type DMDeliveryConfig struct {
	Daemon BeadClient
	Bot    *Bot
	Logger *slog.Logger

	// Machine, when non-nil, is the bridge's own Keycloak service-account
	// credential (beadsapi.MachineTokenSourceFromEnv; gd-Hc34EENbO9). The
	// delivery WRITES (dm_channel/dm_ts, close, reply capture, reminder stamp)
	// authenticate as machine:<clientId> through it; they take over an
	// agent-created dm bead, which the machine principal owns via its typed
	// gardenio-machine-slack-dm-delivery grant. Reads stay on the daemon's own
	// credential. A token failure fails the write closed (auth_unavailable); the
	// static fleet token is never substituted. nil keeps today's behavior.
	Machine beadsapi.TokenSource
}

// trackedQuestion holds metadata for a pending question DM awaiting a reply.
type trackedQuestion struct {
	beadID      string
	sourceAgent string
	targetUser  string
	deliveredAt time.Time
}

// DMDelivery watches for new dm beads and delivers them as Slack DMs.
// For question-mode DMs, it tracks the delivered message and captures thread
// replies from the target user, updating the bead and nudging the requesting agent.
type DMDelivery struct {
	daemon BeadClient
	bot    *Bot
	logger *slog.Logger
	// machine: see DMDeliveryConfig.Machine.
	machine beadsapi.TokenSource

	// seen dedups dm-bead delivery per bead ID across SSE reconnects.
	// TTL/size-bounded so it cannot leak for the process lifetime
	// (ka-0000003049, sibling of ka-0000008059); carries its own lock.
	seen *seenSet

	mu        sync.Mutex
	questions map[string]*trackedQuestion // "channel:ts" → pending question
}

// NewDMDelivery creates a new DM delivery watcher.
func NewDMDelivery(cfg DMDeliveryConfig) *DMDelivery {
	return &DMDelivery{
		daemon:    cfg.Daemon,
		bot:       cfg.Bot,
		logger:    cfg.Logger,
		machine:   cfg.Machine,
		seen:      newSeenSet(seenTTL, seenMaxEntries),
		questions: make(map[string]*trackedQuestion),
	}
}

// writeCtx returns ctx carrying the machine principal for a dm delivery write
// (WithMachinePrincipal is a no-op for a nil source).
func (d *DMDelivery) writeCtx(ctx context.Context) context.Context {
	return beadsapi.WithMachinePrincipal(ctx, d.machine)
}

// dmQuestionTTL bounds how long a delivered-question entry is retained in the
// in-memory reply-tracking map. The map was never evicted (ka-0000003049): every
// question DM ever delivered accumulated a permanent "channel:ts" entry for the
// process lifetime. Eviction is safe because findQuestionByThread recovers a
// pending question by querying open dm beads from the daemon, so a reply that
// lands after eviction still matches via that fallback — the in-memory map is
// only a fast path. 7 days is far beyond any realistic human reply latency.
const dmQuestionTTL = 7 * 24 * time.Hour

// Run starts the background dedup/reply-tracking GC loop. It blocks until ctx is
// cancelled; callers run it in a goroutine. Mirrors Agents.Run / BeadActivity.Run
// (ka-0000008059). Without it seen would only self-evict lazily on re-observation
// and questions would never evict at all.
func (d *DMDelivery) Run(ctx context.Context) {
	ticker := time.NewTicker(seenGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.seen.gc()
			d.gcQuestions()
		}
	}
}

// gcQuestions drops reply-tracking entries older than dmQuestionTTL. A late
// reply for an evicted question is still matched via findQuestionByThread's
// daemon fallback, so this only bounds the fast-path map.
func (d *DMDelivery) gcQuestions() {
	cutoff := time.Now().Add(-dmQuestionTTL)
	d.mu.Lock()
	defer d.mu.Unlock()
	evicted := 0
	for k, q := range d.questions {
		if q.deliveredAt.Before(cutoff) {
			delete(d.questions, k)
			evicted++
		}
	}
	if evicted > 0 {
		d.logger.Info("dm delivery: gc'd stale question tracking",
			"evicted", evicted, "remaining", len(d.questions))
	}
}

// RegisterHandlers registers SSE event handlers for dm bead created events.
func (d *DMDelivery) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", d.handleCreated)
	d.logger.Info("dm delivery watcher registered SSE handlers",
		"topics", []string{"beads.bead.created"})
}

func (d *DMDelivery) handleCreated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	// Only handle dm beads.
	if bead.Type != "dm" {
		return
	}

	// Dedup on SSE reconnect.
	if d.alreadySeen(bead.ID) {
		return
	}

	targetUser := bead.Fields["target_user"]
	if targetUser == "" {
		d.logger.Warn("dm bead missing target_user", "id", bead.ID)
		return
	}

	// Check for group DM (multiple targets).
	targetUsersRaw := bead.Fields["target_users"]
	isGroup := targetUsersRaw != ""

	// Resolve non-Slack-ID targets via the user directory.
	targetUser = d.resolveTargetUser(ctx, targetUser, bead.ID)

	var groupTargets []string
	if isGroup {
		for _, u := range strings.Split(targetUsersRaw, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				groupTargets = append(groupTargets, d.resolveTargetUser(ctx, u, bead.ID))
			}
		}
	}

	dmMode := bead.Fields["dm_mode"]
	isQuestion := dmMode == "question"
	isReview := dmMode == "review"

	// Verbosity seam (gb-ux epic ka-0000005059, plan §5): question/review DMs
	// stay actionable and reply-tracked in BOTH modes (guardrail), but the
	// simple default trims the framing preamble to the ask + link. Vibe mode
	// keeps the full-context DM. Resolved from the (already-resolved) target
	// Slack id; a group DM keys off its primary target. Plain fire-and-forget
	// DMs are directed communication, not lifecycle chatter, so they are left
	// unchanged.
	vibe := d.bot != nil && d.bot.vibeModeForSlackUser(ctx, targetUser)
	th := d.bot.agentTheme(ctx, bead.Fields["source_agent"])

	// Build the DM text.
	var text string
	switch {
	case isReview:
		text = buildReviewDMText(th, bead, vibe)
	case isQuestion:
		text = buildQuestionDMText(th, bead, vibe)
	default:
		text = buildDMText(bead)
	}

	d.logger.Info("delivering DM",
		"id", bead.ID,
		"target", targetUser,
		"group", isGroup,
		"source", bead.Fields["source_agent"],
		"mode", bead.Fields["dm_mode"])

	if d.bot == nil {
		d.logger.Warn("dm delivery: bot is nil, cannot deliver", "id", bead.ID)
		return
	}

	// Deliver the DM — group or single.
	var ch, ts string
	var err error
	if isGroup && len(groupTargets) > 1 {
		ch, ts, err = d.bot.SendGroupDM(ctx, groupTargets, text)
	} else {
		ch, ts, err = d.bot.SendDM(ctx, targetUser, text)
	}
	if err != nil {
		d.logger.Error("dm delivery failed",
			"id", bead.ID, "target", targetUser, "error", err)
		return
	}

	// Update the bead with delivery metadata.
	if err := d.daemon.UpdateBeadFields(d.writeCtx(ctx), bead.ID, map[string]string{
		"dm_channel": ch,
		"dm_ts":      ts,
	}); err != nil {
		d.logger.Error("dm delivery: failed to update bead with delivery info",
			"id", bead.ID, "error", err)
	}

	// Question and review DMs stay open for reply tracking.
	if isQuestion || isReview {
		d.trackQuestion(bead.ID, ch, ts, bead.Fields["source_agent"], targetUser)
		d.scheduleReminder(bead.ID, ch, ts, targetUser, bead.Fields["source_agent"], bead.Fields["timeout_minutes"])
		d.logger.Info("dm delivered (tracking for reply)",
			"id", bead.ID, "target", targetUser, "channel", ch, "ts", ts, "mode", dmMode)
		return
	}

	// Close the bead after successful delivery (fire-and-forget DMs).
	if err := d.daemon.CloseBead(d.writeCtx(ctx), bead.ID, nil); err != nil {
		d.logger.Error("dm delivery: failed to close bead",
			"id", bead.ID, "error", err)
	}

	d.logger.Info("dm delivered",
		"id", bead.ID, "target", targetUser, "channel", ch, "ts", ts)
}

// buildDMText formats the DM message from bead fields.
func buildDMText(bead *BeadEvent) string {
	subject := bead.Fields["subject"]
	body := bead.Fields["body"]
	agent := extractAgentName(bead.Fields["source_agent"])

	var parts []string
	if subject != "" {
		parts = append(parts, fmt.Sprintf("*%s*", subject))
	}
	if body != "" {
		parts = append(parts, body)
	}
	if agent != "" {
		parts = append(parts, fmt.Sprintf("_— sent by agent %s_", agent))
	}

	if len(parts) == 0 {
		return bead.Title
	}
	return strings.Join(parts, "\n")
}

// HandleReply checks if a DM thread reply matches a tracked question or review DM.
// Supports multiple replies: the first reply sets bead fields (replied, reply_text,
// reply_ts, reply_user); subsequent replies are stored as bead comments. Each reply
// nudges the requesting agent. The bead stays open for follow-up replies until the
// agent explicitly closes it.
// Returns true if the reply was handled.
func (d *DMDelivery) HandleReply(ctx context.Context, channel, threadTS, replyText, replyUser string) bool {
	q := d.peekQuestion(channel, threadTS)
	if q == nil {
		// Fallback: check daemon for open question/review DMs matching this thread.
		// This handles the case where the bridge restarted and lost in-memory state.
		q = d.findQuestionByThread(ctx, channel, threadTS)
		if q == nil {
			return false
		}
		// Re-track so subsequent replies within this session don't hit the daemon.
		d.trackQuestion(q.beadID, channel, threadTS, q.sourceAgent, q.targetUser)
	}

	d.logger.Info("dm reply received",
		"bead", q.beadID, "user", replyUser, "agent", q.sourceAgent)

	// Check if this is the first reply or a follow-up.
	isFirstReply := !d.isAlreadyReplied(ctx, q.beadID)

	if isFirstReply {
		// First reply: update bead fields directly.
		if err := d.daemon.UpdateBeadFields(d.writeCtx(ctx), q.beadID, map[string]string{
			"replied":    "true",
			"reply_text": replyText,
			"reply_ts":   threadTS,
			"reply_user": replyUser,
		}); err != nil {
			d.logger.Error("dm reply: failed to update bead", "id", q.beadID, "error", err)
		}
	} else {
		// Subsequent reply: store as a bead comment.
		commentText := fmt.Sprintf("Reply from %s: %s", replyUser, replyText)
		if err := d.daemon.AddComment(d.writeCtx(ctx), q.beadID, replyUser, commentText); err != nil {
			d.logger.Error("dm reply: failed to add comment", "id", q.beadID, "error", err)
		}
	}

	// Post confirmation in thread.
	if d.bot != nil {
		_ = d.bot.replyInThread(ctx, channel, threadTS,
			d.bot.agentTheme(ctx, q.sourceAgent).Semantic(SemOK)+" Reply captured — the agent has been notified.")
	}

	// Nudge the requesting agent.
	agentName := extractAgentName(q.sourceAgent)
	if agentName != "" {
		message := fmt.Sprintf("Reply to your DM (bead %s) from %s: %s", q.beadID, replyUser, truncateText(replyText, 200))
		if err := NudgeAgent(ctx, d.daemon, d.logger, agentName, message); err != nil {
			d.logger.Warn("dm reply: failed to nudge agent", "agent", agentName, "error", err)
		}
	}

	return true
}

// trackQuestion records a delivered question DM for reply tracking.
func (d *DMDelivery) trackQuestion(beadID, channel, ts, sourceAgent, targetUser string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := channel + ":" + ts
	d.questions[key] = &trackedQuestion{
		beadID:      beadID,
		sourceAgent: sourceAgent,
		targetUser:  targetUser,
		deliveredAt: time.Now(),
	}
}

// peekQuestion returns a tracked question by channel+ts without removing it,
// or nil if not found. Unlike popQuestion, this keeps the entry so follow-up
// replies in the same thread continue to match.
func (d *DMDelivery) peekQuestion(channel, threadTS string) *trackedQuestion {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := channel + ":" + threadTS
	return d.questions[key]
}

// isAlreadyReplied checks if the bead has already received a first reply.
// Uses in-memory bead state from the daemon to avoid race conditions.
func (d *DMDelivery) isAlreadyReplied(ctx context.Context, beadID string) bool {
	bead, err := d.daemon.GetBead(ctx, beadID)
	if err != nil {
		return false
	}
	return bead.Fields["replied"] == "true"
}

// findQuestionByThread queries the daemon for an open question or review DM matching
// the thread. Used as fallback after bridge restart when in-memory tracking is lost.
// Searches open beads to catch both unreplied and multi-reply DMs.
func (d *DMDelivery) findQuestionByThread(ctx context.Context, channel, threadTS string) *trackedQuestion {
	result, err := d.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"dm"},
		Statuses: []string{"open"},
		Limit:    100,
	})
	if err != nil {
		d.logger.Warn("dm reply fallback query failed", "error", err)
		return nil
	}
	for _, b := range result.Beads {
		mode := b.Fields["dm_mode"]
		if (mode == "question" || mode == "review") && b.Fields["dm_channel"] == channel && b.Fields["dm_ts"] == threadTS {
			return &trackedQuestion{
				beadID:      b.ID,
				sourceAgent: b.Fields["source_agent"],
				targetUser:  b.Fields["target_user"],
			}
		}
	}
	return nil
}

// scheduleReminder schedules a one-shot reminder for an unanswered question DM.
func (d *DMDelivery) scheduleReminder(beadID, channel, ts, targetUser, sourceAgent, timeoutStr string) {
	minutes, _ := strconv.Atoi(timeoutStr)
	if minutes <= 0 {
		return
	}
	timeout := time.Duration(minutes) * time.Minute

	time.AfterFunc(timeout, func() {
		ctx := context.Background()

		// Check if already replied or closed.
		bead, err := d.daemon.GetBead(ctx, beadID)
		if err != nil {
			d.logger.Warn("dm reminder: failed to check bead", "id", beadID, "error", err)
			return
		}
		if bead.Status == "closed" || bead.Fields["replied"] == "true" || bead.Fields["reminder_sent"] == "true" {
			return
		}

		// Send reminder in the DM thread.
		agent := extractAgentName(sourceAgent)
		reminderText := fmt.Sprintf("%s Reminder: agent *%s* is waiting for your reply to this question.", d.bot.agentTheme(ctx, sourceAgent).Semantic(SemQuestion), agent)
		if d.bot != nil {
			_ = d.bot.replyInThread(ctx, channel, ts, reminderText)
		}

		// Mark reminder sent.
		_ = d.daemon.UpdateBeadFields(d.writeCtx(ctx), beadID, map[string]string{
			"reminder_sent": "true",
		})

		d.logger.Info("dm reminder sent", "id", beadID, "target", targetUser)
	})
}

// buildQuestionDMText formats a question DM. In vibe mode it carries the full
// context framing (agent name, task context, reply hint); in the simple default
// it trims to the ask itself plus a one-line reply hint, keeping the DM
// actionable and reply-tracked while cutting the preamble (plan §5).
func buildQuestionDMText(th Theme, bead *BeadEvent, vibe bool) string {
	question := bead.Fields["body"]
	agent := extractAgentName(bead.Fields["source_agent"])
	taskID := bead.Fields["task_context"]

	if !vibe {
		// Terse: the ask, then a one-line reply hint. Attribute the agent
		// inline so the recipient still knows who is asking.
		var parts []string
		if question != "" {
			parts = append(parts, question)
		} else {
			parts = append(parts, th.Semantic(SemQuestion)+" A question from an agent.")
		}
		hint := "_Reply in this thread to answer."
		if agent != "" {
			hint += fmt.Sprintf(" (agent %s)", agent)
		}
		hint += "_"
		parts = append(parts, hint)
		return strings.Join(parts, "\n")
	}

	var parts []string
	if agent != "" {
		parts = append(parts, fmt.Sprintf("%s *Question from agent %s*", th.Semantic(SemQuestion), agent))
	} else {
		parts = append(parts, th.Semantic(SemQuestion)+" *Question from an agent*")
	}
	if taskID != "" {
		parts = append(parts, fmt.Sprintf("_Context: task %s_", taskID))
	}
	parts = append(parts, "")
	parts = append(parts, question)
	parts = append(parts, "")
	parts = append(parts, "_Reply in this thread to answer._")

	return strings.Join(parts, "\n")
}

// buildReviewDMText formats an MR review request DM. In vibe mode it carries the
// full context (agent, task, MR title, pipeline status, custom message); in the
// simple default it trims to the MR link (+ title) and a one-line reply hint,
// keeping the request actionable and reply-tracked (plan §5).
func buildReviewDMText(th Theme, bead *BeadEvent, vibe bool) string {
	mrURL := bead.Fields["mr_url"]
	agent := extractAgentName(bead.Fields["source_agent"])
	taskID := bead.Fields["task_context"]
	mrTitle := bead.Fields["mr_title"]
	pipelineStatus := bead.Fields["mr_pipeline_status"]
	customMessage := bead.Fields["body"]

	if !vibe {
		// Terse: the ask + link. Keep the MR title (the "what") and the URL (the
		// action), drop the context/pipeline framing.
		var parts []string
		head := th.Semantic(SemReview) + " MR review request"
		if agent != "" {
			head += fmt.Sprintf(" from agent %s", agent)
		}
		parts = append(parts, head)
		if mrTitle != "" {
			parts = append(parts, fmt.Sprintf("*%s*", mrTitle))
		}
		if mrURL != "" {
			parts = append(parts, mrURL)
		}
		parts = append(parts, "_Reply in this thread with your review feedback._")
		return strings.Join(parts, "\n")
	}

	var parts []string
	if agent != "" {
		parts = append(parts, fmt.Sprintf("%s *MR review request from agent %s*", th.Semantic(SemReview), agent))
	} else {
		parts = append(parts, th.Semantic(SemReview)+" *MR review request*")
	}
	if taskID != "" {
		parts = append(parts, fmt.Sprintf("_Context: task %s_", taskID))
	}

	parts = append(parts, "")

	if mrTitle != "" {
		parts = append(parts, fmt.Sprintf("*%s*", mrTitle))
	}
	if mrURL != "" {
		parts = append(parts, mrURL)
	}
	if pipelineStatus != "" {
		parts = append(parts, fmt.Sprintf("Pipeline: %s", pipelineStatus))
	}

	if customMessage != "" {
		parts = append(parts, "")
		parts = append(parts, customMessage)
	}

	parts = append(parts, "")
	parts = append(parts, "_Reply in this thread with your review feedback._")

	return strings.Join(parts, "\n")
}

// resolveTargetUser attempts to resolve a target user identifier to a Slack
// user ID. If the input already looks like a Slack user ID (starts with "U"),
// it is returned as-is. Otherwise, the user directory is consulted to look up
// the identifier as a GitLab username, GitHub username, or email address.
func (d *DMDelivery) resolveTargetUser(ctx context.Context, target, beadID string) string {
	// Already a Slack user ID.
	if strings.HasPrefix(target, "U") {
		return target
	}

	if d.bot == nil {
		return target
	}

	dir := d.bot.UserDirectory()

	// Try GitLab username first (most common for MR review flows).
	if u := dir.LookupByGitLabUsername(ctx, target); u != nil && u.SlackID != "" {
		d.logger.Info("resolved target via GitLab username",
			"bead", beadID, "input", target, "slack_id", u.SlackID)
		return u.SlackID
	}

	// Try GitHub username.
	if u := dir.LookupByGitHubUsername(ctx, target); u != nil && u.SlackID != "" {
		d.logger.Info("resolved target via GitHub username",
			"bead", beadID, "input", target, "slack_id", u.SlackID)
		return u.SlackID
	}

	// Try email.
	if strings.Contains(target, "@") {
		if u := dir.LookupByEmail(ctx, target); u != nil && u.SlackID != "" {
			d.logger.Info("resolved target via email",
				"bead", beadID, "input", target, "slack_id", u.SlackID)
			return u.SlackID
		}
	}

	d.logger.Warn("could not resolve target to Slack ID, using as-is",
		"bead", beadID, "target", target)
	return target
}

func (d *DMDelivery) alreadySeen(beadID string) bool {
	return d.seen.markSeen(beadID)
}
