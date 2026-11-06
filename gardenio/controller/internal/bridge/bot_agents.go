package bridge

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// triggerRunCountEntry caches a trigger's fire count with a TTL. Stored under
// b.triggerRunCountCache; protected by b.triggerRunCountCacheMu so daemon
// queries do not block the main b.mu Slack-state lock.
type triggerRunCountEntry struct {
	count    int
	cachedAt time.Time
}

// triggerRunCountTTL bounds staleness of cached trigger fire counts on agent
// cards while sparing the daemon on rapid card refreshes.
const triggerRunCountTTL = 60 * time.Second

// Prune work is batched for goroutine-yield and ctx-cancellation checkpoints;
// API rate is bounded by the shared slackCardLimiter (ka-0000000496,
// ka-0000003678, ka-0000000854).
const (
	pruneBatchSize  = 10
	pruneBatchSleep = 1500 * time.Millisecond
)

// pruneTokenFloor is the number of slackCardLimiter tokens the prune loop
// leaves for live card refreshes: prune's blocking Wait() would otherwise take
// every freed token and starve the live path's non-blocking Allow(), freezing
// cards (ka-0000008156).
const pruneTokenFloor = 1.0

// agentThreadingEnabled returns true if agent card threading is active.
func (b *Bot) agentThreadingEnabled() bool {
	return b.threadingMode == "agent"
}

// threadCardsEnabled reports whether agent cards should be posted in Slack
// threads for project/channel: the config bead must be enabled, env mode
// "owned"/"all", the project allow-listed (empty = all) and the channel not
// excluded (docs/research/thread-cards/03-config-and-rollback.md §1).
func (b *Bot) threadCardsEnabled(project, channel string) bool {
	// Runtime kill switch from the config bead.
	if !b.threadCardsConfig.Enabled {
		return false
	}
	// Env master switch: anything other than owned/all is off.
	if b.threadCardsMode != "owned" && b.threadCardsMode != "all" {
		return false
	}
	// Project allow-list. Empty list = all projects.
	if len(b.threadCardsConfig.Projects) > 0 && !slices.Contains(b.threadCardsConfig.Projects, project) {
		return false
	}
	// Channel deny-list: a thread in an excluded channel gets no card and
	// falls back to the legacy thread-reply path.
	if channel != "" && slices.Contains(b.threadCardsConfig.ExcludeChannels, channel) {
		return false
	}
	return true
}

// threadCardOwned reports whether the thread root was authored by the spawning
// user (slack_opener_user_id == slack_user_id, non-empty), the "owned" mode
// gate (docs/research/thread-cards/02-ux-design.md §Q4). A missing opener
// fails closed.
func (b *Bot) threadCardOwned(bead BeadEvent) bool {
	opener := bead.Fields["slack_opener_user_id"]
	return opener != "" && opener == bead.Fields["slack_user_id"]
}

// isUnengagedPoolAgentByName is the daemon-backed twin of isUnengagedPoolAgent
// for updateAgentCard's card-create branch, which has no SSE bead event. Fails
// open (false) on any lookup failure so a daemon blip still posts the card.
func (b *Bot) isUnengagedPoolAgentByName(ctx context.Context, agent string) bool {
	if b.daemon == nil {
		return false
	}
	detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, extractAgentName(agent))
	if err != nil || detail == nil {
		return false
	}
	return unengagedPoolAgent(detail.Labels, detail.Fields)
}

// postWarmThreadCard posts the in-thread card for an agent just assigned out of
// the prewarmed pool, mirroring NotifyAgentSpawn (a warm assignment reuses the
// bead, so no bead.created fires; ka-0000000954). The thread-bound mark is set
// even when thread cards are off so a racing refresh cannot post a top-level
// card mid-assignment.
func (b *Bot) postWarmThreadCard(ctx context.Context, agent, project, channel, threadTS, openerUserID, slackUserID string) {
	if agent == "" || channel == "" || threadTS == "" {
		return
	}
	b.mu.Lock()
	b.markThreadBound(agent)
	b.mu.Unlock()

	if !b.threadCardsEnabled(project, channel) {
		return
	}
	if b.threadCardsMode != "all" && (openerUserID == "" || openerUserID != slackUserID) {
		return
	}

	// ensureAgentCard is idempotent on the hot cache and compresses a card that
	// lives in a different channel, so a pooled agent carrying a pre-fix
	// top-level card is moved into the thread rather than duplicated.
	cardTS, err := b.ensureAgentCard(ctx, agent, channel, threadTS) // inThreadTS!
	if err != nil {
		b.logger.Error("failed to post warm-assign thread agent card",
			"agent", agent, "channel", channel, "thread_ts", threadTS, "error", err)
		return
	}
	if cardTS == "" {
		return
	}
	// As the cold path: anchor tool stats (StartCardSession no-ops on a
	// duplicate) and move lifecycle reactions onto the card.
	if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
		b.streamMgr.StartCardSession(agent, channel, cardTS)
	}
	if b.reactions != nil {
		b.reactions.SetTarget(agent, channel, cardTS)
		b.reactions.TransitionState(agent, "spawning")
	}
	b.logger.Info("posted warm-assign thread agent card",
		"agent", agent, "channel", channel, "card_ts", cardTS, "thread_ts", threadTS)
}

// markThreadBound records that agent was spawned from a Slack thread. Nil-map
// safe; b.mu must be held.
func (b *Bot) markThreadBound(agent string) {
	if b.agentThreadBound == nil {
		b.agentThreadBound = make(map[string]bool)
	}
	b.agentThreadBound[agent] = true
}

// isThreadBound returns true if the agent is known to be thread-bound.
// Safe to call with a nil map. Must be called with b.mu held.
func (b *Bot) isThreadBound(agent string) bool {
	return b.agentThreadBound[agent]
}

// cacheTriggerFieldsLocked copies trigger metadata from a bead's fields into
// the agent-keyed card caches. b.mu must be held; safe on non-trigger beads.
func (b *Bot) cacheTriggerFieldsLocked(agent string, fields map[string]string) {
	if agent == "" || fields == nil {
		return
	}
	if id := fields["trigger_id"]; id != "" {
		b.agentTriggerID[agent] = id
	}
	if title := fields["trigger_title"]; title != "" {
		b.agentTriggerTitle[agent] = title
	}
	if src := fields["source"]; src != "" {
		b.agentTriggerSource[agent] = src
	}
	// triggerSourceFieldKey so hyphenated sources match the "<source>_key" field
	// TriggerEngine.actionSpawnAgent wrote.
	if src := b.agentTriggerSource[agent]; src != "" {
		if mk := fields[triggerSourceFieldKey(src)]; mk != "" {
			b.agentTriggerMatchKey[agent] = mk
		}
	}
	// Card-footer navigability (ka-0000003131). Overwrite only on non-empty so a
	// field-poor later event can't blank an earlier value.
	if rb := fields["resolved_by"]; rb != "" {
		if b.agentResolvedBy == nil {
			b.agentResolvedBy = make(map[string]string)
		}
		b.agentResolvedBy[agent] = rb
	}
	if ow := fields["owner"]; ow != "" {
		if b.agentOwner == nil {
			b.agentOwner = make(map[string]string)
		}
		b.agentOwner[agent] = ow
	}
	// Pre-rendered spawn-vector line (ka-0000004046); same non-empty rule.
	if line := formatSpawnVectorLine(fields); line != "" {
		if b.agentSpawnVectorLine == nil {
			b.agentSpawnVectorLine = make(map[string]string)
		}
		b.agentSpawnVectorLine[agent] = line
	}
}

// beadDuration returns the truncated-to-second span between a bead's creation
// and last update, or zero when either timestamp is missing or the span is
// non-positive. Used to render the run duration on a wrap-up card.
func beadDuration(createdAt, updatedAt time.Time) time.Duration {
	if createdAt.IsZero() || updatedAt.IsZero() {
		return 0
	}
	if d := updatedAt.Sub(createdAt); d > 0 {
		return d.Truncate(time.Second)
	}
	return 0
}

// pruneStaleAgentCards compresses (not deletes, since thread replies persist)
// the cards of agents the daemon no longer lists as active, e.g. left in the
// state file across a restart.
func (b *Bot) pruneStaleAgentCards(ctx context.Context) {
	cycleStart := time.Now()
	b.mu.Lock()
	cardCount := len(b.agentCards)
	b.mu.Unlock()

	if cardCount == 0 {
		return
	}

	activeAgents, err := b.daemon.ListAgentBeads(ctx)
	if err != nil {
		b.logger.Error("prune agent cards: failed to list active agents", "error", err)
		return
	}

	// Active excludes done/failed and stop-requested agents; meta and bead feed
	// cost display and wrap-up rendering on stale cards.
	active := make(map[string]bool, len(activeAgents))
	agentMeta := make(map[string]map[string]string, len(activeAgents))
	agentBead := make(map[string]beadsapi.AgentBead, len(activeAgents))
	for _, a := range activeAgents {
		short := extractAgentName(a.AgentName)
		agentMeta[short] = a.Metadata
		agentBead[short] = a
		if a.AgentState == "done" || a.AgentState == "failed" {
			continue // Treat terminal-state agents as stale even if bead is still open.
		}
		if a.Metadata["stop_requested"] != "" {
			continue // Agent has requested shutdown.
		}
		active[short] = true
	}

	b.mu.Lock()
	var stale []string
	for agent := range b.agentCards {
		if !active[extractAgentName(agent)] {
			stale = append(stale, agent)
		}
	}
	b.mu.Unlock()

	// Compact stale state entries alongside the card prune to prevent
	// unbounded growth of decision/chat message refs in the state file.
	if b.state != nil {
		removed, err := b.state.CompactStaleEntries(active)
		if err != nil {
			b.logger.Warn("prune: state compaction failed", "error", err)
		} else if removed > 0 {
			b.logger.Info("prune: compacted stale state entries", "removed", removed)
		}
	}

	if len(stale) == 0 {
		b.logger.Info("prune agent cards: all cards are current", "total", cardCount)
		return
	}

	b.logger.Info("prune agent cards: compressing stale cards",
		"stale", len(stale), "total", cardCount, "active", len(activeAgents),
		"batch_size", pruneBatchSize)

	processed := 0
	for i, agent := range stale {
		// Yield between batches; also a ctx.Done() checkpoint (ka-0000000496).
		if i > 0 && i%pruneBatchSize == 0 {
			select {
			case <-ctx.Done():
				b.logger.Info("prune agent cards: interrupted by context cancellation",
					"processed", processed, "remaining", len(stale)-i)
				b.logger.Info("prune agent cards: cycle complete",
					"processed", processed, "stale", len(stale),
					"active", len(activeAgents), "total", cardCount,
					"elapsed", time.Since(cycleStart))
				return
			case <-time.After(pruneBatchSleep):
			}
		}

		// Honor any Retry-After from a prior chat.update (ka-0000003678).
		if !b.waitForSlackRateLimit(ctx, agent, i, len(stale)) {
			b.logger.Info("prune agent cards: cycle complete",
				"processed", processed, "stale", len(stale),
				"active", len(activeAgents), "total", cardCount,
				"elapsed", time.Since(cycleStart))
			return
		}

		// Shared Tier-3 token bucket (ka-0000000854), yielding to live cards via
		// pruneTokenFloor (ka-0000008156).
		if err := b.waitForPruneToken(ctx); err != nil {
			b.logger.Info("prune agent cards: rate limiter wait cancelled",
				"agent", agent, "processed", processed, "remaining", len(stale)-i,
				"error", err)
			b.logger.Info("prune agent cards: cycle complete",
				"processed", processed, "stale", len(stale),
				"active", len(activeAgents), "total", cardCount,
				"elapsed", time.Since(cycleStart))
			return
		}
		b.mu.Lock()
		ref, ok := b.agentCards[agent]
		state := b.agentState[agent]
		if ok {
			delete(b.agentCards, agent)
			delete(b.agentState, agent)
			delete(b.agentPending, agent)
			delete(b.agentSchedulingBlocked, agent)
			delete(b.agentSeen, agent)
			delete(b.agentSpawnChannel, agent)
			delete(b.agentSpawnInheritedBy, agent)
			delete(b.agentPodName, agent)
			delete(b.agentImageTag, agent)
			delete(b.agentRole, agent)
			delete(b.agentProject, agent)
			delete(b.agentScheduleTitle, agent)
			delete(b.agentSpawnedAt, agent)
			delete(b.agentThreadBound, agent)
			delete(b.agentTriggerID, agent)
			delete(b.agentTriggerTitle, agent)
			delete(b.agentTriggerSource, agent)
			delete(b.agentTriggerMatchKey, agent)
			delete(b.agentSpawnVectorLine, agent)
			delete(b.agentResolvedBy, agent)
			delete(b.agentOwner, agent)
		}
		b.mu.Unlock()

		if ok {
			short := extractAgentName(agent)
			// agentMeta comes from the active-only ListAgentBeads, so it misses agents
			// whose bead just closed; the status-agnostic fetch below may replace it
			// (ka-0000003171).
			rollupFields := agentMeta[short]

			// A done/failed agent that wrapped up keeps its wrap-up card rather than being
			// compacted (ka-0000006217). A closed bead drops out of the active list, so
			// fall back to a status-agnostic fetch for its wrapup (ka-0000004075).
			var (
				wrapup       string
				wrapMeta     map[string]string
				wrapDuration time.Duration
				haveWrap     bool
			)
			if bead, found := agentBead[short]; found && bead.Metadata["wrapup"] != "" {
				wrapup = bead.Metadata["wrapup"]
				wrapMeta = bead.Metadata
				wrapDuration = beadDuration(bead.CreatedAt, bead.UpdatedAt)
				haveWrap = true
			} else if (state == "" || state == "done" || state == "failed") && b.daemon != nil {
				// Status-agnostic fetch (includes CLOSED). Also the only source of a
				// reaped cycle's real outcome when the in-memory state was lost; never
				// default that to "done" (ka-0000005513).
				if detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, short); err == nil && detail != nil {
					// Closed bead's authoritative cost/turns/context fields win for
					// the rollup (ka-0000003171).
					rollupFields = detail.Fields
					if detail.Fields["wrapup"] != "" {
						wrapup = detail.Fields["wrapup"]
						wrapMeta = detail.Fields
						wrapDuration = beadDuration(detail.CreatedAt, detail.UpdatedAt)
						haveWrap = true
					}
					if state == "" {
						state = terminalCardState(detail.Fields["agent_state"], detail.Fields["close_reason"])
					}
				}
			}
			// Last resort when the bead was unrecoverable, so the card renders.
			if state == "" {
				state = "done"
			}

			// A pruned card is always terminal, so live=false (reconciled cost,
			// "at exit" context).
			th := b.themeFor(ctx, ref.ChannelID)
			var rollup agentRollup
			if !b.notifConfig.SuppressCostReporting {
				rollup = buildAgentRollup(th, rollupFields, false)
			}

			var blocks []slack.Block
			if (state == "done" || state == "failed") && haveWrap {
				blocks = b.buildWrapUpAgentCardBlocks(th, agent, state, wrapup, rollup.Cost, wrapDuration, wrapMeta)
			} else {
				// No wrap-up: compress to the compact one-liner.
				blocks = buildCompactAgentCardBlocks(th, agent, state, rollup.Usage, rollupFields)
			}
			blocks = b.withTaskRun(ctx, blocks, b.cardSubjectBead(ctx, rollupFields["task_id"]))
			err := b.editAgentCard(ctx, agent, ref, fmt.Sprintf("Agent: %s (%s)", extractAgentName(agent), state), blocks)
			// Drop the card-bead memo too, or ephemeral names grow it unbounded.
			b.forgetAgentCard(agent)
			if err != nil {
				if rateLimitErr, ok := isSlackRateLimit(err); ok {
					retryAfter := rateLimitErr.RetryAfter
					if retryAfter < 5*time.Second {
						retryAfter = 5 * time.Second
					}
					b.mu.Lock()
					b.slackRateLimitUntil = time.Now().Add(retryAfter)
					b.mu.Unlock()
					b.logger.Warn("prune agent cards: rate limited, pausing prune loop",
						"agent", agent, "retry_after", retryAfter,
						"processed", processed, "remaining", len(stale)-i-1)
					// Wait out the full Retry-After. If ctx interrupts, the stale
					// card survives in persisted state for the next cycle.
					if !b.waitForSlackRateLimit(ctx, agent, i, len(stale)) {
						b.logger.Info("prune agent cards: cycle complete",
							"processed", processed, "stale", len(stale),
							"active", len(activeAgents), "total", cardCount,
							"elapsed", time.Since(cycleStart))
						return
					}
					continue
				}
				b.logger.Warn("prune agent cards: failed to compress Slack message",
					"agent", agent, "error", err)
			} else if b.state != nil {
				// Mark compressed so a restart doesn't re-compress it
				// (ka-0000008156). Only on success, so failures are retried.
				if serr := b.state.MarkAgentCardCompressed(agent, ref); serr != nil {
					b.logger.Warn("prune agent cards: failed to persist compressed marker",
						"agent", agent, "error", serr)
				}
			}
			processed++
			b.logger.Info("compressed stale agent card", "agent", agent)
		}
	}
	b.logger.Info("prune agent cards: cycle complete",
		"processed", processed, "stale", len(stale),
		"active", len(activeAgents), "total", cardCount,
		"elapsed", time.Since(cycleStart))
}

// waitForPruneToken acquires one slackCardLimiter token for the prune loop
// only while the bucket holds more than pruneTokenFloor, polling rather than
// reserving so prune consumes only spare capacity (ka-0000008156). Returns
// ctx.Err() on cancellation.
func (b *Bot) waitForPruneToken(ctx context.Context) error {
	// Tokens() never exceeds burst, so clamp the floor to burst-1 or the loop
	// would hang; a burst-1 (or unlimited) limiter just takes a token.
	floor := min(pruneTokenFloor, float64(b.slackCardLimiter.Burst()-1))
	for {
		if b.slackCardLimiter.Tokens() > floor && b.slackCardLimiter.Allow() {
			return nil
		}
		// Poll well under the ~1.5s token interval.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// waitForSlackRateLimit blocks until b.slackRateLimitUntil has passed (true)
// or ctx is cancelled (false, caller should return).
func (b *Bot) waitForSlackRateLimit(ctx context.Context, agent string, processed, total int) bool {
	b.mu.Lock()
	until := b.slackRateLimitUntil
	b.mu.Unlock()
	wait := time.Until(until)
	if wait <= 0 {
		return true
	}
	b.logger.Info("prune agent cards: waiting for Slack rate limit window",
		"agent", agent, "wait", wait, "processed", processed, "remaining", total-processed)
	select {
	case <-ctx.Done():
		b.logger.Info("prune agent cards: rate-limit wait interrupted by context cancellation",
			"processed", processed, "remaining", total-processed)
		return false
	case <-time.After(wait):
		return true
	}
}

// startPeriodicPrune prunes once, then every 5 minutes (plus
// ReconcileThreadAgents) until ctx is cancelled. Run it as a goroutine: a large
// backlog must not block Socket Mode from connecting (ka-0000000496).
func (b *Bot) startPeriodicPrune(ctx context.Context) {
	b.pruneStaleAgentCards(ctx)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.pruneStaleAgentCards(ctx)
			// Recover thread→agent bindings lost to restarts or compaction.
			b.ReconcileThreadAgents(ctx)
		}
	}
}

// agentTask returns the task claimed by agent (full identity, then short name),
// or nil. The card shows its title and, when it bears arms, its run
// (withTaskRun).
func (b *Bot) agentTask(ctx context.Context, agent string) *beadsapi.BeadDetail {
	bead, err := b.daemon.ListAssignedTask(ctx, agent)
	if err == nil && bead != nil {
		return bead
	}
	if short := extractAgentName(agent); short != agent {
		bead, err = b.daemon.ListAssignedTask(ctx, short)
		if err == nil && bead != nil {
			return bead
		}
	}
	return nil
}

// ensureAgentCard posts or retrieves the agent status card and returns its ts
// for threading. A non-empty inThreadTS posts it as a reply in that thread, so
// getAgentByThread can match a thread-bound agent via ref.ThreadTS.
func (b *Bot) ensureAgentCard(ctx context.Context, agent, channelID string, inThreadTS ...string) (string, error) {
	b.mu.Lock()
	if ref, ok := b.agentCards[agent]; ok {
		if ref.ChannelID == channelID {
			b.mu.Unlock()
			return ref.Timestamp, nil
		}
		// Card exists in another channel (e.g. routing changed after a restart):
		// compress the old one rather than leave a duplicate.
		oldRef := ref
		delete(b.agentCards, agent)
		b.mu.Unlock()
		b.compressOldAgentCard(ctx, agent, oldRef)
	} else {
		b.mu.Unlock()
	}

	// Resurrect a card pruned from the hot cache: card beads first (the record
	// from slice 2 on), then persisted state as its one-release fallback (lands
	// spec 5.2).
	if beadRef, found := b.agentCardFromBeads(ctx, agent, channelID); found {
		b.mu.Lock()
		b.agentCards[agent] = beadRef
		b.mu.Unlock()
		if b.state != nil {
			_ = b.state.SetAgentCard(agent, beadRef)
		}
		b.logger.Info("resurrected agent card from card bead in ensureAgentCard",
			"agent", agent, "channel", channelID, "ts", beadRef.Timestamp)
		return beadRef.Timestamp, nil
	}
	if b.state != nil {
		if stateRef, found := b.state.GetAgentCard(agent); found && stateRef.ChannelID == channelID {
			b.mu.Lock()
			b.agentCards[agent] = stateRef
			b.mu.Unlock()
			b.logger.Info("resurrected agent card from state in ensureAgentCard",
				"agent", agent, "channel", channelID, "ts", stateRef.Timestamp)
			return stateRef.Timestamp, nil
		}
	}

	// Last resort: the slack_card_* fields persistCardRef stamps on the agent
	// bead, the only tier that survives losing the state file (ka-0000005475).
	if beadRef, found := b.agentCardRefFromBead(ctx, agent); found && beadRef.ChannelID == channelID {
		b.mu.Lock()
		b.agentCards[agent] = beadRef
		b.mu.Unlock()
		if b.state != nil {
			_ = b.state.SetAgentCard(agent, beadRef)
		}
		b.logger.Info("resurrected agent card from bead fields in ensureAgentCard",
			"agent", agent, "channel", channelID, "ts", beadRef.Timestamp)
		return beadRef.Timestamp, nil
	}

	// Re-check under lock to prevent concurrent posts (rapid SSE events).
	b.mu.Lock()
	if ref, ok := b.agentCards[agent]; ok && ref.ChannelID == channelID {
		b.mu.Unlock()
		return ref.Timestamp, nil
	}
	pending := b.agentPending[agent]
	state := b.agentState[agent]
	seen := b.agentSeen[agent]
	podName := b.agentPodName[agent]
	imageTag := b.agentImageTag[agent]
	role := b.agentRole[agent]
	schedulingBlocked := b.agentSchedulingBlocked[agent]
	trigger := b.snapshotTriggerInfo(agent)
	b.mu.Unlock()

	if trigger.ID != "" {
		trigger.RunCount = b.fetchTriggerRunCount(ctx, trigger.ID)
	}
	// Bead pod_name beats the sticky cache after a pod rename (ka-0000004617).
	agentHref := b.agentCardHref(ctx, agent, podName)
	task := b.agentTask(ctx, agent)
	var taskTitle string
	if task != nil {
		taskTitle = task.Title
	}
	subject := b.agentCardSubject(ctx, agent, trigger)
	// Initial post has no settled spend, so the cost line is empty.
	th := b.themeFor(ctx, channelID)
	rollup := b.fetchAgentRollup(ctx, th, agent)
	blocks := buildAgentCardBlocks(th, agent, subject, pending, state, taskTitle, seen, agentHref, imageTag, role, "", rollup.Context, nil, b.version, schedulingBlocked, trigger, rollup.Local)
	blocks = b.withTaskRun(ctx, blocks, task)
	var parentTS string
	if len(inThreadTS) > 0 && inThreadTS[0] != "" {
		parentTS = inThreadTS[0]
	}
	ref, err := b.postAgentCard(ctx, agent, channelID, parentTS, fmt.Sprintf("Agent: %s", extractAgentName(agent)), blocks)
	if err != nil {
		b.noteCardRefused(ctx, agent, channelID, err)
		return "", fmt.Errorf("post agent card: %w", err)
	}
	cardChannel, ts := ref.ChannelID, ref.Timestamp

	b.mu.Lock()
	b.agentCards[agent] = ref
	b.mu.Unlock()

	if b.state != nil {
		_ = b.state.SetAgentCard(agent, ref)
	}

	// Best-effort: lets chat.update find the card after a restart empties the
	// cache and state; failure degrades wrap-up to posting a new reply.
	b.persistCardRef(ctx, agent, cardChannel, ts)

	b.logger.Info("posted agent status card", "agent", agent, "channel", cardChannel, "ts", ts, "thread_ts", parentTS)
	return ts, nil
}

// persistCardRef stamps slack_card_channel/slack_card_ts on the agent bead so
// replaceAgentCardWithWrapUp can find the card with no in-memory or state
// record. The lookup is status-agnostic because cards are posted and
// re-rendered after the bead closes; a status-scoped lookup silently left the
// bead pointing at an older card (ka-0000005302).
func (b *Bot) persistCardRef(ctx context.Context, agent, channel, ts string) {
	if b.daemon == nil || channel == "" || ts == "" {
		return
	}
	detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, extractAgentName(agent))
	if err != nil || detail == nil {
		return
	}
	if detail.Fields["slack_card_channel"] == channel && detail.Fields["slack_card_ts"] == ts {
		return
	}
	if err := b.daemon.UpdateBeadFields(ctx, detail.ID, map[string]string{
		"slack_card_channel": channel,
		"slack_card_ts":      ts,
	}); err != nil {
		b.logger.Debug("failed to persist agent card ref to bead",
			"agent", agent, "error", err)
	}

	// Mirror the ref onto the wish head (the agent bead's slack_thread_bead_id)
	// so a 'done' card reaction resolves to the wish even after the agent is
	// done and the cache is gone (ka-0000000693).
	b.persistHeadCardRef(ctx, detail.Fields["slack_thread_bead_id"], channel, ts)
}

// persistHeadCardRef stamps slack_card_channel/slack_card_ts onto wish head
// headID so the 1b-11 close handler resolves a card reaction with one indexed
// lookup (migration 038). Best-effort: failure degrades to the legacy
// cache/agent-metadata path.
func (b *Bot) persistHeadCardRef(ctx context.Context, headID, channel, ts string) {
	if b.daemon == nil || headID == "" || channel == "" || ts == "" {
		return
	}
	head, err := b.daemon.GetBead(ctx, headID)
	if err != nil || head == nil {
		return
	}
	if head.Fields["slack_card_channel"] == channel && head.Fields["slack_card_ts"] == ts {
		return
	}
	if err := b.daemon.UpdateBeadFields(ctx, headID, map[string]string{
		"slack_card_channel": channel,
		"slack_card_ts":      ts,
	}); err != nil {
		b.logger.Debug("failed to persist card ref to wish head bead",
			"head", headID, "error", err)
	}
}

// compressOldAgentCard compacts an agent card left behind in its old channel
// and removes it from state.
func (b *Bot) compressOldAgentCard(ctx context.Context, agent string, ref MessageRef) {
	blocks := buildCompactAgentCardBlocks(b.themeFor(ctx, ref.ChannelID), agent, "moved", "", nil)
	err := b.editAgentCard(ctx, agent, ref, fmt.Sprintf("Agent: %s (moved)", extractAgentName(agent)), blocks)
	if err != nil {
		b.logger.Warn("failed to compress old agent card after channel change",
			"agent", agent, "old_channel", ref.ChannelID, "error", err)
	}
	if b.state != nil {
		_ = b.state.RemoveAgentCard(agent)
	}
	b.logger.Info("compressed old agent card after channel change",
		"agent", agent, "old_channel", ref.ChannelID)
}

// NotifyAgentSpawn records an agent bead's initial spawning state and posts
// its card (top-level, in-thread, or a flat notice, per mode).
func (b *Bot) NotifyAgentSpawn(ctx context.Context, bead BeadEvent) {
	// Skip already-closed beads: on SSE replay these slip past
	// CatchUpAgents' active-only dedup and would become zombie cards.
	if bead.ID != "" {
		if detail, err := b.daemon.GetBead(ctx, bead.ID); err == nil && detail.Status == "closed" {
			b.logger.Debug("skipping spawn for closed agent bead", "id", bead.ID)
			return
		}
	}

	// Prewarmed pool agents get a card only once assigned to a thread.
	if bead.Fields["agent_state"] == "prewarmed" {
		b.logger.Debug("skipping spawn notification for prewarmed agent",
			"id", bead.ID, "agent", bead.Fields["agent"])
		return
	}

	agent := bead.Assignee
	// The created event often lacks Assignee — reconstruct from fields.
	if agent == "" {
		if n := bead.Fields["agent"]; n != "" {
			agent = n
		}
	}
	if agent == "" {
		agent = bead.Title
	}
	if agent == "" {
		return
	}
	// Canonicalize to the short name used for all map keys.
	agent = extractAgentName(agent)

	now := time.Now()
	b.mu.Lock()
	b.agentState[agent] = "spawning"
	b.agentSeen[agent] = now
	b.agentSpawnedAt[agent] = now
	if role := bead.Fields["role"]; role != "" {
		b.agentRole[agent] = role
	}
	if project := bead.Fields["project"]; project != "" {
		b.agentProject[agent] = project
	}
	if schedTitle := bead.Fields["schedule_title"]; schedTitle != "" {
		b.agentScheduleTitle[agent] = schedTitle
	}
	b.cacheTriggerFieldsLocked(agent, bead.Fields)
	if spawnCh := bead.Fields["slack_spawn_channel"]; spawnCh != "" {
		b.agentSpawnChannel[agent] = spawnCh
		b.recordInheritedSpawnRoleLocked(agent, bead.Fields)
	}
	// A schedule channel override routes all notifications.
	if schedCh := bead.Fields["schedule_slack_channel"]; schedCh != "" {
		b.agentSpawnChannel[agent] = schedCh
		delete(b.agentSpawnInheritedBy, agent) // a schedule channel is an explicit pin
	}
	b.mu.Unlock()

	// Also picks up slack_spawn_channel if the created event lacked it.
	b.fetchAndCachePodName(ctx, agent)

	// Thread-bound agents get no top-level spawn notice; handleThreadSpawn already
	// started a stream there. Also check bead fields, since this SSE handler can
	// race handleThreadSpawn's thread mapping.
	slackChannel, slackTS := b.resolveAgentThread(ctx, agent)
	if slackChannel == "" || slackTS == "" {
		if ch := bead.Fields["slack_thread_channel"]; ch != "" {
			if ts := bead.Fields["slack_thread_ts"]; ts != "" {
				slackChannel, slackTS = ch, ts
			}
		}
	}
	// Thread spawns always set spawn_source "slack-thread*", even before the
	// thread fields propagate.
	if slackChannel == "" || slackTS == "" {
		if src := bead.Fields["spawn_source"]; strings.HasPrefix(src, "slack-thread") {
			b.mu.Lock()
			b.markThreadBound(agent)
			b.mu.Unlock()
			b.logger.Debug("skipping spawn notification for thread-sourced agent (spawn_source fallback)",
				"agent", agent, "spawn_source", src)
			return
		}
	}
	if slackChannel != "" && slackTS != "" {
		// Cache thread-bound so later paths skip the daemon lookup and a
		// transient resolveAgentThread failure can't post a stray card.
		b.mu.Lock()
		b.markThreadBound(agent)
		b.mu.Unlock()

		b.logger.Debug("skipping spawn notification for thread-bound agent",
			"agent", agent, "channel", slackChannel, "thread_ts", slackTS)

		// Thread cards (UX Q2): post one anchor card in the thread and point
		// tool stats/reactions at it, replacing the legacy message-edit path.
		// Gate (UX Q4): "owned" needs threadCardOwned, "all" bypasses it.
		// ensureAgentCard is idempotent, so a double fire won't double-post.
		if b.threadCardsEnabled(bead.Fields["project"], slackChannel) && (b.threadCardsMode == "all" || b.threadCardOwned(bead)) {
			cardTS, err := b.ensureAgentCard(ctx, agent, slackChannel, slackTS) // inThreadTS!
			if err != nil {
				b.logger.Error("failed to post thread agent card",
					"agent", agent, "channel", slackChannel, "thread_ts", slackTS, "error", err)
				// Fall through to the legacy path.
			} else if cardTS != "" {
				if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
					b.streamMgr.StartCardSession(agent, slackChannel, cardTS)
				}
				if b.reactions != nil {
					b.reactions.SetTarget(agent, slackChannel, cardTS)
					b.reactions.TransitionState(agent, "spawning")
				}
				b.logger.Info("posted thread agent card",
					"agent", agent, "channel", slackChannel, "card_ts", cardTS, "thread_ts", slackTS)
				return
			}
		}

		// StartSession no-ops if handleThreadSpawn already started it.
		if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
			b.streamMgr.StartSession(ctx, agent, slackChannel, slackTS, bead.Fields["slack_user_id"])
		}

		if b.reactions != nil {
			b.reactions.SetTarget(agent, slackChannel, slackTS)
			b.reactions.TransitionState(agent, "spawning")
		}
		return
	}

	channel := b.resolveChannel(agent)
	th := b.themeFor(ctx, channel)

	if schedTitle := bead.Fields["schedule_title"]; schedTitle != "" {
		schedText := fmt.Sprintf("%s Scheduled task *%s* started (`%s`)", th.Semantic(SemSchedule), schedTitle, agent)
		if cron := bead.Fields["schedule_cron"]; cron != "" {
			schedText += fmt.Sprintf(" · `%s`", cron)
		}

		// Threading: post the card first and thread the schedule notice
		// under it, keeping per-run messages out of the channel.
		if b.agentThreadingEnabled() {
			cardTS, err := b.ensureAgentCard(ctx, agent, channel)
			if err != nil {
				b.logger.Error("failed to post agent spawn card for scheduled agent",
					"agent", agent, "error", err)
			}
			if b.reactions != nil && cardTS != "" {
				b.reactions.SetTarget(agent, channel, cardTS)
				b.reactions.TransitionState(agent, "spawning")
			}
			if cardTS != "" {
				if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
					b.streamMgr.StartCardSession(agent, channel, cardTS)
				}
				_, err = b.postAgentNotice(ctx, agent, b.agentEventLand(bead, agent), channel, cardTS,
					agentNotice(fmt.Sprintf("Scheduled task %s started (%s)", schedTitle, agent), schedText))
				if err != nil {
					b.logger.Error("failed to post scheduled agent spawn message in thread",
						"agent", agent, "schedule", schedTitle, "error", err)
				}
			}
			return
		}

		msg, err := b.postAgentNotice(ctx, agent, b.agentEventLand(bead, agent), channel, "",
			agentNotice(fmt.Sprintf("Scheduled task %s started (%s)", schedTitle, agent), schedText))
		msgTS := msg.TS
		if err != nil {
			b.logger.Error("failed to post scheduled agent spawn message",
				"agent", agent, "schedule", schedTitle, "error", err)
		}
		if b.reactions != nil && msgTS != "" {
			b.reactions.SetTarget(agent, channel, msgTS)
			b.reactions.TransitionState(agent, "spawning")
		}
		return
	}

	if b.agentThreadingEnabled() {
		cardTS, err := b.ensureAgentCard(ctx, agent, channel)
		if err != nil {
			b.logger.Error("failed to post agent spawn card",
				"agent", agent, "error", err)
		}
		if b.streamMgr != nil && !b.notifConfig.SuppressToolStream && cardTS != "" {
			b.streamMgr.StartCardSession(agent, channel, cardTS)
		}
		if b.reactions != nil && cardTS != "" {
			b.reactions.SetTarget(agent, channel, cardTS)
			b.reactions.TransitionState(agent, "spawning")
		}
	} else {
		displayName := b.agentDisplayName(agent)
		spawnText := fmt.Sprintf("%s *Agent spawned: %s*", th.Semantic(SemSpawn), displayName)
		if role := bead.Fields["role"]; role != "" {
			spawnText += fmt.Sprintf(" (%s)", role)
		}
		msg, err := b.postAgentNotice(ctx, agent, b.agentEventLand(bead, agent), channel, "",
			agentNotice(fmt.Sprintf("Agent spawned: %s", extractAgentName(agent)), spawnText))
		msgTS := msg.TS
		if err != nil {
			b.logger.Error("failed to post agent spawn message",
				"agent", agent, "error", err)
		}
		if b.reactions != nil && msgTS != "" {
			b.reactions.SetTarget(agent, channel, msgTS)
			b.reactions.TransitionState(agent, "spawning")
		}
	}
}

// NotifyAgentState records an agent bead's new agent_state and refreshes its
// card.
func (b *Bot) NotifyAgentState(_ context.Context, bead BeadEvent) {
	agent := bead.Assignee
	// Updated events may also lack Assignee (as in NotifyAgentSpawn).
	if agent == "" {
		if n := bead.Fields["agent"]; n != "" {
			agent = n
		}
	}
	if agent == "" {
		return
	}
	agent = extractAgentName(agent)
	state := bead.Fields["agent_state"]

	// Skip unengaged pool agents with no card, or a top-level card leaks
	// (ka-0000003743, ka-0000007013). isUnengagedPoolAgent keys off the label, not
	// state, because pool beads move prewarmed → queued → done under a pod cap.
	// Once an agent has a card, updates flow normally.
	if state == "prewarmed" || isUnengagedPoolAgent(bead) {
		b.mu.Lock()
		_, hasCard := b.agentCards[agent]
		if !hasCard {
			b.agentState[agent] = state
			b.agentSeen[agent] = time.Now()
			b.mu.Unlock()
			b.logger.Debug("skipping state update for unengaged pool agent without card",
				"agent", agent, "id", bead.ID, "state", state)
			return
		}
		b.mu.Unlock()
	}

	b.mu.Lock()
	prevState := b.agentState[agent]
	b.agentState[agent] = state
	// pod_scheduling_blocked=="true" shows "stuck: cluster capacity"
	// (ka-0000005976); anything else clears it.
	if b.agentSchedulingBlocked == nil {
		b.agentSchedulingBlocked = make(map[string]bool)
	}
	b.agentSchedulingBlocked[agent] = bead.Fields["pod_scheduling_blocked"] == "true"
	b.agentSeen[agent] = time.Now()
	_, hasPod := b.agentPodName[agent]
	// Recover routing from bead fields after a restart, or resolveChannel
	// falls back to the default and a duplicate card is posted.
	if b.agentSpawnChannel[agent] == "" {
		if ch := bead.Fields["slack_spawn_channel"]; ch != "" {
			b.agentSpawnChannel[agent] = ch
			b.recordInheritedSpawnRoleLocked(agent, bead.Fields)
		}
		if ch := bead.Fields["schedule_slack_channel"]; ch != "" {
			b.agentSpawnChannel[agent] = ch
			delete(b.agentSpawnInheritedBy, agent) // a schedule channel is an explicit pin
		}
	}
	// Recover thread-bound after a restart, so no stray top-level card.
	if !b.isThreadBound(agent) {
		if isValidThreadBinding(bead.Fields["slack_thread_channel"], bead.Fields["slack_thread_ts"]) {
			b.markThreadBound(agent)
		} else if strings.HasPrefix(bead.Fields["spawn_source"], "slack-thread") {
			b.markThreadBound(agent)
		}
	}
	if b.agentProject[agent] == "" {
		if p := bead.Fields["project"]; p != "" {
			b.agentProject[agent] = p
		}
	}
	if b.agentRole[agent] == "" {
		if r := bead.Fields["role"]; r != "" {
			b.agentRole[agent] = r
		}
	}
	// Recover trigger context after a restart.
	if b.agentTriggerID[agent] == "" {
		b.cacheTriggerFieldsLocked(agent, bead.Fields)
	}
	b.mu.Unlock()

	if !hasPod {
		b.fetchAndCachePodName(context.Background(), agent)
	}

	if b.reactions != nil && state != prevState {
		b.reactions.TransitionState(agent, state)
	}

	// Add the coopmux link to a thread spawn message once pod_name exists.
	if prevState == "spawning" && state == "working" {
		b.updateThreadSpawnWithLink(agent)
	}

	// Terminal transitions (once per change).
	if (state == "done" || state == "failed") && state != prevState {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if b.streamMgr != nil {
			b.streamMgr.StopSession(ctx, agent, state)
		}

		if slackChannel, slackTS := b.resolveAgentThread(ctx, agent); slackChannel != "" && slackTS != "" {
			// Thread cards (UX Q1): edit the anchor card into its wrap-up and
			// post at most a one-line ping; else the legacy
			// postThreadStateReply (doc 03 §4). Same gate as the spawn side.
			if b.threadCardsEnabled(bead.Fields["project"], slackChannel) && (b.threadCardsMode == "all" || b.threadCardOwned(bead)) {
				b.reconcileThreadCardTerminal(ctx, agent, state, bead, slackChannel, slackTS)
			} else {
				b.postThreadStateReply(ctx, agent, state, bead, slackChannel, slackTS)
			}
			// Keep the thread→agent mapping so a later reply can resume the
			// same agent (same name, same PVC).
			return
		}
	}

	if (state == "done" || state == "failed") && state != prevState {
		b.mu.Lock()
		schedTitle := b.agentScheduleTitle[agent]
		spawnedAt := b.agentSpawnedAt[agent]
		b.mu.Unlock()

		if schedTitle != "" {
			b.postScheduleStateNotification(agent, schedTitle, state, spawnedAt, bead)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Render the wrap-up on done/failed, and on stopping: gb stop --wrapup's
	// suspend path leaves the bead open with agent_state=stopping
	// (ka-0000006840). stopping is gated on a real transition since the reconciler
	// may re-emit it, and deliberately skips the terminal teardown above so a thaw
	// can resume.
	wrapup := bead.Fields["wrapup"]
	switch {
	case (state == "done" || state == "failed") && wrapup != "":
		b.replaceAgentCardWithWrapUp(ctx, agent, state, wrapup)
	case state == "stopping" && state != prevState && wrapup != "":
		b.replaceAgentCardWithWrapUp(ctx, agent, state, wrapup)
	default:
		b.updateAgentCard(ctx, agent)
	}
}

// updateThreadSpawnWithLink sets the thread status bar to "Working..." on
// spawning→working, rather than posting lifecycle messages.
func (b *Bot) updateThreadSpawnWithLink(agent string) {
	// An active stream already updates the thread status.
	if b.streamMgr != nil && b.streamMgr.HasSession(agent) {
		return
	}

	b.mu.Lock()
	ref, ok := b.threadSpawnMsgs[agent]
	b.mu.Unlock()

	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b.SetThreadStatus(ctx, ref.ChannelID, ref.ThreadTS, "Working...")
}

// postThreadStateReply posts a terminal agent's state and wrapup in its
// thread. After a streaming spawn (finalized by StopSession) it deletes the
// stream message and posts only extra details; otherwise it replaces the spawn
// message with a final reply.
func (b *Bot) postThreadStateReply(ctx context.Context, agent, state string, bead BeadEvent, channel, threadTS string) {
	th := b.themeFor(ctx, channel)
	var emoji, status string
	switch state {
	case "done":
		emoji = th.Semantic(SemOK)
		status = "finished — mention @gardenio in this thread to resume"
	case "failed":
		emoji = th.Semantic(SemFail)
		status = "failed"
	default:
		return // Only post for terminal states.
	}

	agentLink := b.agentThreadLink(agent)

	b.mu.Lock()
	spawnRef, hasSpawn := b.threadSpawnMsgs[agent]
	if hasSpawn {
		delete(b.threadSpawnMsgs, agent)
	}
	b.mu.Unlock()

	// StopSession already finalized a streaming spawn.
	streamHandled := hasSpawn && b.streamMgr != nil && !b.streamMgr.HasSession(agent)
	// With tool streaming on, the spawn message was a stream (started by
	// handleThreadSpawn).
	if hasSpawn && b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
		streamHandled = true
	}

	if streamHandled {
		// Delete the now-finished live status message.
		if hasSpawn {
			delErr := b.deleteSlackMessage(ctx, spawnRef)
			if delErr != nil {
				b.logger.Debug("failed to delete stream message",
					"agent", agent, "error", delErr)
			}
		}

		var extra string
		if reason := bead.Fields["close_reason"]; reason != "" {
			extra += fmt.Sprintf("\n> %s", truncateText(reason, 500))
		}
		if wrapupJSON := bead.Fields["wrapup"]; wrapupJSON != "" {
			extra += b.formatWrapUpSlack(th, wrapupJSON)
		}
		if !b.notifConfig.SuppressCostReporting {
			if costLine := formatCostSummary(bead.Fields); costLine != "" {
				extra += fmt.Sprintf("\n%s %s", th.Semantic(SemCost), costLine)
			}
			// Turns + context, matching the card (ka-0000003171).
			if ct := formatAgentContextTurns(th, bead.Fields, false); ct != "" {
				extra += fmt.Sprintf("\n%s", ct)
			}
		}
		if extra != "" {
			text := fmt.Sprintf("%s %s %s.%s", emoji, agentLink, status, extra)
			_, _ = b.postAgentNotice(ctx, agent, b.agentEventLand(bead, agent), channel, threadTS,
				conversation.Message{Text: text})
		}
		return
	}

	// Non-streaming: delete the spawn message, then post a final reply.
	if hasSpawn && spawnRef.Timestamp != "" {
		delErr := b.deleteSlackMessage(ctx, spawnRef)
		if delErr != nil {
			b.logger.Debug("failed to delete spawn message",
				"agent", agent, "error", delErr)
		}
	}

	text := fmt.Sprintf("%s %s %s.", emoji, agentLink, status)
	if reason := bead.Fields["close_reason"]; reason != "" {
		text += fmt.Sprintf("\n> %s", truncateText(reason, 500))
	}
	if wrapupJSON := bead.Fields["wrapup"]; wrapupJSON != "" {
		text += b.formatWrapUpSlack(th, wrapupJSON)
	}
	if !b.notifConfig.SuppressCostReporting {
		if costLine := formatCostSummary(bead.Fields); costLine != "" {
			text += fmt.Sprintf("\n%s %s", th.Semantic(SemCost), costLine)
		}
		// Turns + context, as above (ka-0000003171).
		if ct := formatAgentContextTurns(th, bead.Fields, false); ct != "" {
			text += fmt.Sprintf("\n%s", ct)
		}
	}

	_, err := b.postAgentNotice(ctx, agent, b.agentEventLand(bead, agent), channel, threadTS,
		conversation.Message{Text: text})
	if err != nil {
		b.logger.Error("failed to post thread state reply",
			"agent", agent, "state", state, "error", err)
	}
}

// reconcileThreadCardTerminal handles a terminal state on the thread-cards path
// (UX Q1, docs/research/thread-cards/02-ux-design.md §Q1): it edits the anchor
// card into its wrap-up/terminal form (edits don't re-notify) and posts at most
// one one-line ping when threadCardPingWanted. It replaces
// postThreadStateReply, which would duplicate the wrap-up and always ping.
func (b *Bot) reconcileThreadCardTerminal(ctx context.Context, agent, state string, bead BeadEvent, channel, threadTS string) {
	// (1) An empty wrapup (e.g. crash, no gb stop) renders the bare terminal state.
	b.replaceAgentCardWithWrapUp(ctx, agent, state, bead.Fields["wrapup"])

	// (2) Conditional completion ping (D3).
	if !b.threadCardPingWanted(ctx, agent, bead) {
		return
	}

	th := b.themeFor(ctx, channel)
	var emoji, status string
	switch state {
	case "done":
		emoji = th.Semantic(SemOK)
		status = "finished — see card above; mention @gardenio in this thread to resume"
	case "failed":
		emoji = th.Semantic(SemFail)
		status = "failed — see card above"
	default:
		return // Only ping for terminal states.
	}

	// One line: the ping only re-surfaces the thread in the sidebar.
	text := fmt.Sprintf("%s %s %s.", emoji, b.agentThreadLink(agent), status)
	if _, err := b.postAgentNotice(ctx, agent, b.agentEventLand(bead, agent), channel, threadTS,
		conversation.Message{Text: text}); err != nil {
		b.logger.Error("failed to post thread-card completion ping",
			"agent", agent, "state", state, "error", err)
	}
}

// threadCardPingWanted reports whether a thread-card terminal should ping (UX
// Q1 / D3): the wrap-up declares pull_requests, or the run exceeded
// PingThresholdSeconds. A threshold of 0 disables duration-based pings.
func (b *Bot) threadCardPingWanted(ctx context.Context, agent string, bead BeadEvent) bool {
	if w, ok := parseWrapUp(bead.Fields["wrapup"]); ok && len(w.PullRequests) > 0 {
		return true
	}

	thresholdSecs := b.threadCardsConfig.PingThresholdSeconds
	if thresholdSecs <= 0 {
		return false
	}
	// Zero (unknown) duration fails closed, so no ping.
	dur := b.fetchAgentDuration(ctx, agent)
	return dur > time.Duration(thresholdSecs)*time.Second
}

// postScheduleStateNotification posts a concise done/failed notice for a
// scheduled agent, threaded under its card when threading is enabled.
func (b *Bot) postScheduleStateNotification(agent, schedTitle, state string, spawnedAt time.Time, bead BeadEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	channel := b.resolveChannel(agent)
	th := b.themeFor(ctx, channel)

	var emoji, verb string
	switch state {
	case "done":
		emoji = th.Semantic(SemOK)
		verb = "completed"
	case "failed":
		emoji = th.Semantic(SemFail)
		verb = "failed"
	default:
		return
	}

	text := fmt.Sprintf("%s Scheduled task *%s* %s", emoji, schedTitle, verb)

	if !spawnedAt.IsZero() {
		dur := time.Since(spawnedAt).Truncate(time.Second)
		text += fmt.Sprintf(" in %s", formatDuration(dur))
	}

	if state == "failed" {
		if reason := bead.Fields["close_reason"]; reason != "" {
			text += fmt.Sprintf(": %s", truncateText(reason, 300))
		}
	}

	if wrapupJSON := bead.Fields["wrapup"]; wrapupJSON != "" {
		text += b.formatWrapUpSlack(th, wrapupJSON)
	}

	// Cost, then turns + context, matching the card (ka-0000003171).
	if !b.notifConfig.SuppressCostReporting {
		if costLine := formatCostSummary(bead.Fields); costLine != "" {
			text += fmt.Sprintf("\n%s %s", th.Semantic(SemCost), costLine)
		}
		if ct := formatAgentContextTurns(th, bead.Fields, false); ct != "" {
			text += fmt.Sprintf("\n%s", ct)
		}
	}

	// ensureAgentCard resurrects or creates the card so this stays in-thread
	// after a prune or restart.
	var threadTS string
	if b.agentThreadingEnabled() {
		if cardTS, err := b.ensureAgentCard(ctx, agent, channel); err != nil {
			b.logger.Warn("failed to ensure agent card for schedule notification",
				"agent", agent, "error", err)
		} else {
			threadTS = cardTS
		}
	}

	_, err := b.postAgentNotice(ctx, agent, b.agentEventLand(bead, agent), channel, threadTS,
		agentNotice(fmt.Sprintf("Scheduled task %s %s", schedTitle, verb), text))
	if err != nil {
		b.logger.Error("failed to post schedule state notification",
			"agent", agent, "schedule", schedTitle, "state", state, "error", err)
	}
}

// formatDuration formats d as e.g. "45s", "4m 12s", "1h 23m".
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// NotifyAgentTaskUpdate refreshes an agent's card when it claims a task, so the
// title appears without waiting for a pod phase change.
func (b *Bot) NotifyAgentTaskUpdate(_ context.Context, agentName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agent := extractAgentName(agentName)

	b.mu.Lock()
	_, hasCard := b.agentCards[agent]
	if hasCard {
		b.agentSeen[agent] = time.Now()
	}
	b.mu.Unlock()

	if hasCard {
		b.updateAgentCard(ctx, agent)
	}
}

// cardUpdateMinInterval is the per-agent debounce between card chat.updates,
// keeping many active agents under Slack's ~50 req/min limit.
const cardUpdateMinInterval = 5 * time.Second

// updateAgentCard re-renders an agent's status card, debounced per agent;
// terminal states (done/failed) bypass the debounce.
func (b *Bot) updateAgentCard(ctx context.Context, agent string) {
	b.mu.Lock()
	_, ok := b.agentCards[agent]
	state := b.agentState[agent]
	b.mu.Unlock()

	if !ok {
		// Not in the hot cache: resurrect from the card bead, then state
		// (lands spec 5.2), so a pruned card isn't duplicated.
		if beadRef, found := b.agentCardFromBeads(ctx, agent, ""); found {
			b.mu.Lock()
			b.agentCards[agent] = beadRef
			b.mu.Unlock()
			b.logger.Info("resurrected agent card from card bead",
				"agent", agent, "channel", beadRef.ChannelID, "ts", beadRef.Timestamp)
			ok = true
		} else if b.state != nil {
			if stateRef, found := b.state.GetAgentCard(agent); found {
				b.mu.Lock()
				b.agentCards[agent] = stateRef
				b.mu.Unlock()
				b.logger.Info("resurrected agent card from persisted state",
					"agent", agent, "channel", stateRef.ChannelID, "ts", stateRef.Timestamp)
				ok = true
			}
		}
	}

	if !ok {
		// No card under this identity (e.g. identity drift between short and
		// full names): create one under the canonical identity if threading.
		if b.agentThreadingEnabled() {
			// Thread-bound agents use streaming messages; a card here would
			// later leak as a compacted top-level line. Check the cached flag
			// before the daemon lookup so a transient failure can't post one.
			b.mu.Lock()
			threadBound := b.isThreadBound(agent)
			b.mu.Unlock()
			if threadBound {
				return
			}
			if ch, ts := b.resolveAgentThread(ctx, agent); ch != "" && ts != "" {
				// Cache it so later calls skip the daemon.
				b.mu.Lock()
				b.markThreadBound(agent)
				b.mu.Unlock()
				return
			}
			// No card for an unengaged pool agent: this branch is reached from
			// the agent stream (SessionStart → RefreshAgentCard), not SSE, so
			// the guards in NotifyAgentSpawn/NotifyAgentState don't cover it
			// (ka-0000007013, ka-0000003743, ka-0000000954).
			if b.isUnengagedPoolAgentByName(ctx, agent) {
				b.logger.Debug("skipping card create for unengaged pool agent",
					"agent", agent)
				return
			}
			channel := b.resolveChannel(agent)
			if _, err := b.ensureAgentCard(ctx, agent, channel); err != nil {
				b.logger.Error("failed to create agent card on state update",
					"agent", agent, "error", err)
			}
		}
		return
	}

	// Debounce non-terminal updates (and during Retry-After backoff).
	isTerminal := state == "done" || state == "failed"
	b.mu.Lock()
	lastUpdate := b.agentCardLastUpdate[agent]
	backingOff := time.Now().Before(b.slackRateLimitUntil)
	if !isTerminal && (time.Since(lastUpdate) < cardUpdateMinInterval || backingOff) {
		b.agentCardDirty[agent] = true
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	// The global limiter caps the direct path too.
	if !isTerminal && !b.slackCardLimiter.Allow() {
		b.mu.Lock()
		b.agentCardDirty[agent] = true
		b.mu.Unlock()
		return
	}

	b.flushAgentCard(ctx, agent)
}

// flushAgentCard re-renders and edits an agent's card now, clearing its dirty
// mark. Called directly (bypassing debounce) or by the periodic flusher.
func (b *Bot) flushAgentCard(ctx context.Context, agent string) {
	b.mu.Lock()
	ref, ok := b.agentCards[agent]
	pending := b.agentPending[agent]
	state := b.agentState[agent]
	seen := b.agentSeen[agent]
	podName := b.agentPodName[agent]
	imageTag := b.agentImageTag[agent]
	role := b.agentRole[agent]
	schedulingBlocked := b.agentSchedulingBlocked[agent]
	trigger := b.snapshotTriggerInfo(agent)
	delete(b.agentCardDirty, agent)
	b.agentCardLastUpdate[agent] = time.Now()
	b.mu.Unlock()

	if !ok {
		return
	}

	task := b.agentTask(ctx, agent)
	var taskTitle string
	if task != nil {
		taskTitle = task.Title
	}
	// One status-agnostic fetch for all usage lines (ka-0000007210,
	// ka-0000003171): Usage (context + cost + turns) for the compact thread card,
	// Context (context + turns) for the full card, which has its own Cost line.
	th := b.themeFor(ctx, ref.ChannelID)
	rollup := b.fetchAgentRollup(ctx, th, agent)
	costLine, usageLine, contextLine := rollup.Cost, rollup.Usage, rollup.Context
	if trigger.ID != "" {
		trigger.RunCount = b.fetchTriggerRunCount(ctx, trigger.ID)
	}

	// Bead pod_name beats the cache after a pod rename (ka-0000004617).
	agentHref := b.agentCardHref(ctx, agent, podName)

	var toolStats *AgentToolStats
	if b.streamMgr != nil {
		toolStats = b.streamMgr.GetSessionStats(agent)
	}

	subject := b.agentCardSubject(ctx, agent, trigger)
	var blocks []slack.Block
	// Change 3 (UX Q3): a working thread card (non-empty ThreadTS) stays
	// compact; non-thread cards and terminal states keep the full layout.
	isTerminalState := state == "done" || state == "failed"
	if ref.ThreadTS != "" && pending == 0 && !isTerminalState {
		blocks = buildCompactWorkingCardBlocks(th, agent, subject, state, agentHref, role, usageLine, toolStats)
	} else {
		blocks = buildAgentCardBlocks(th, agent, subject, pending, state, taskTitle, seen, agentHref, imageTag, role, costLine, contextLine, toolStats, b.version, schedulingBlocked, trigger, rollup.Local)
	}
	blocks = b.withTaskRun(ctx, blocks, task)
	err := b.editAgentCard(ctx, agent, ref, fmt.Sprintf("Agent: %s", extractAgentName(agent)), blocks)
	if err != nil {
		if rateLimitErr, ok := isSlackRateLimit(err); ok {
			retryAfter := rateLimitErr.RetryAfter
			if retryAfter < 5*time.Second {
				retryAfter = 5 * time.Second
			}
			b.logger.Warn("agent card update rate limited, pausing all card updates",
				"agent", agent, "retry_after", retryAfter)
			b.mu.Lock()
			b.agentCardDirty[agent] = true
			b.slackRateLimitUntil = time.Now().Add(retryAfter)
			// Make all dirty cards eligible as soon as the backoff ends.
			for dirtyAgent := range b.agentCardDirty {
				b.agentCardLastUpdate[dirtyAgent] = time.Time{}
			}
			b.mu.Unlock()
			return
		}
		b.logger.Error("failed to update agent card", "agent", agent, "error", err)
	}
}

// startCardUpdateFlusher flushes debounced dirty cards every
// cardUpdateMinInterval until ctx is cancelled.
func (b *Bot) startCardUpdateFlusher(ctx context.Context) {
	ticker := time.NewTicker(cardUpdateMinInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.flushDirtyCards(ctx)
		}
	}
}

// flushDirtyCards flushes dirty cards whose debounce window has elapsed, gated
// by the shared limiter; the rest stay dirty for the next tick.
func (b *Bot) flushDirtyCards(ctx context.Context) {
	b.mu.Lock()
	now := time.Now()
	// Skip the whole cycle during a Retry-After backoff.
	if now.Before(b.slackRateLimitUntil) {
		b.mu.Unlock()
		return
	}
	var ready []string
	for agent := range b.agentCardDirty {
		if now.Sub(b.agentCardLastUpdate[agent]) >= cardUpdateMinInterval {
			ready = append(ready, agent)
		}
	}
	b.mu.Unlock()

	for _, agent := range ready {
		if !b.slackCardLimiter.Allow() {
			break // rate limit budget exhausted; remaining cards flush next cycle
		}
		b.flushAgentCard(ctx, agent)
	}
}

// replaceAgentCardWithWrapUp edits the agent's original card in place into
// its wrap-up, locating it via resolveAgentCardRef. With no card ts anywhere
// (legacy agents) it falls back to updateAgentCard.
func (b *Bot) replaceAgentCardWithWrapUp(ctx context.Context, agent, state, wrapupJSON string) {
	ref, ok := b.resolveAgentCardRef(ctx, agent)
	if !ok {
		b.updateAgentCard(ctx, agent)
		return
	}

	duration := b.fetchAgentDuration(ctx, agent)
	// Agent bead fields drive the restart-condition line (next_respawn_at,
	// respawn_count) and the rollup; nil degrades to wrap-up only.
	var fields map[string]string
	// Status-agnostic: the bead is normally closed by now (ka-0000005302).
	if b.daemon != nil {
		if detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, extractAgentName(agent)); err == nil && detail != nil {
			fields = detail.Fields
		}
	}
	// Cost from the same snapshot so it can't disagree with the chips.
	var costLine string
	if !b.notifConfig.SuppressCostReporting {
		costLine = formatCostSummary(fields)
	}
	blocks := b.buildWrapUpAgentCardBlocks(b.themeFor(ctx, ref.ChannelID), agent, state, wrapupJSON, costLine, duration, fields)
	blocks = b.withTaskRun(ctx, blocks, b.cardSubjectBead(ctx, fields["task_id"]))
	err := b.editAgentCard(ctx, agent, ref, fmt.Sprintf("Agent: %s (%s)", extractAgentName(agent), state), blocks)
	if err != nil {
		b.logger.Error("failed to replace agent card with wrapup",
			"agent", agent, "error", err)
	}
}

// resolveAgentCardRef locates an agent's original card: hot cache, card beads,
// persisted state, then the agent bead's slack_card_* fields, and finally (for
// wrap-up only) slack_thread_*. Returns false when no usable ts exists.
func (b *Bot) resolveAgentCardRef(ctx context.Context, agent string) (MessageRef, bool) {
	b.mu.Lock()
	ref, ok := b.agentCards[agent]
	b.mu.Unlock()
	if ok && ref.ChannelID != "" && ref.Timestamp != "" {
		return ref, true
	}

	// Card bead, then state (lands spec 5.2).
	if beadRef, found := b.agentCardFromBeads(ctx, agent, ""); found {
		return beadRef, true
	}
	if b.state != nil {
		if stateRef, found := b.state.GetAgentCard(agent); found && stateRef.ChannelID != "" && stateRef.Timestamp != "" {
			return stateRef, true
		}
	}

	// One fetch serves both the card tier and the thread fallback.
	detail := b.findAgentBeadForCard(ctx, agent)
	if ref, found := agentCardRefFromFields(agent, detail); found {
		return ref, true
	}

	// Not offered to ensureAgentCard: slack_thread_ts is the triggering
	// (often human) message, which the post path must never adopt and edit.
	if detail != nil {
		ch := detail.Fields["slack_thread_channel"]
		ts := detail.Fields["slack_thread_ts"]
		if ch != "" && ts != "" {
			return MessageRef{ChannelID: ch, Timestamp: ts, Agent: agent}, true
		}
	}

	return MessageRef{}, false
}

// findAgentBeadForCard fetches the agent bead for card resolution, or nil on
// any failure. Status-agnostic because the bead is closed while its terminal
// card is still being re-rendered; a status-scoped lookup made later
// re-renders post a second card instead of editing the first (ka-0000005302).
func (b *Bot) findAgentBeadForCard(ctx context.Context, agent string) *beadsapi.BeadDetail {
	if b.daemon == nil {
		return nil
	}
	detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, extractAgentName(agent))
	if err != nil {
		return nil
	}
	return detail
}

// agentCardRefFromBead reads the slack_card_* card location persistCardRef
// stamped on the agent bead, the tier that survives a redeploy
// (ka-0000005475). Shared by resolveAgentCardRef and ensureAgentCard.
func (b *Bot) agentCardRefFromBead(ctx context.Context, agent string) (MessageRef, bool) {
	return agentCardRefFromFields(agent, b.findAgentBeadForCard(ctx, agent))
}

// agentCardRefFromFields extracts a bridge-owned card ref (slack_card_* only,
// never slack_thread_*) from a fetched agent bead.
func agentCardRefFromFields(agent string, detail *beadsapi.BeadDetail) (MessageRef, bool) {
	if detail == nil {
		return MessageRef{}, false
	}
	ch := detail.Fields["slack_card_channel"]
	ts := detail.Fields["slack_card_ts"]
	if ch == "" || ts == "" {
		return MessageRef{}, false
	}
	return MessageRef{ChannelID: ch, Timestamp: ts, Agent: agent}, true
}

// fetchAgentDuration returns the agent's run time from bead created_at to now,
// or 0 when unknown. Status-agnostic: the bead is usually closed by now
// (ka-0000005302).
func (b *Bot) fetchAgentDuration(ctx context.Context, agent string) time.Duration {
	if b.daemon == nil {
		return 0
	}
	detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, extractAgentName(agent))
	if err != nil || detail == nil || detail.CreatedAt.IsZero() {
		return 0
	}
	d := time.Since(detail.CreatedAt)
	if d < 0 {
		return 0
	}
	return d.Truncate(time.Second)
}

// buildWrapUpAgentCardBlocks builds the outcome-first wrap-up card: a header
// (glyph, outcome, name, duration, cost), accomplishments, MR/PR and bead
// links, de-emphasized blockers/handoff detail, and a Clear button. A zero
// duration is omitted; fields (agent bead, may be nil) feeds the
// restart-condition line and rollup chips.
func (b *Bot) buildWrapUpAgentCardBlocks(th Theme, agent, agentState, wrapupJSON, costLine string, duration time.Duration, fields map[string]string) []slack.Block {
	name := extractAgentName(agent)

	w, parsed := parseWrapUp(wrapupJSON)
	glyph, label := wrapUpOutcome(th, agentState, w)

	// Zero inference turns and no human wrap-up means a platform failure
	// (model outage, pod-startup fault), not a lazy agent: badge it as such
	// (ka-0000000315).
	platformFailure := zeroInferenceTurn(fields) && !hasHumanWrapUp(w)
	if platformFailure {
		glyph, label = th.Glyph(LifecycleAborted), platformFailureLabel
	}

	// Glyph stays next to its label (accessibility rule), then the name.
	metaText := fmt.Sprintf("%s %s · *%s*", glyph, label, name)
	if duration > 0 {
		metaText += fmt.Sprintf(" · ran %s", formatDuration(duration))
	}
	if costLine != "" {
		metaText += fmt.Sprintf(" · %s", costLine)
	}
	// Turns + context (ka-0000003171): terminal, so live=false; skipped when
	// cost reporting is suppressed.
	if !b.notifConfig.SuppressCostReporting {
		if ct := formatAgentContextTurns(th, fields, false); ct != "" {
			metaText += fmt.Sprintf(" · %s", ct)
		}
	}

	// A validated tldr becomes a plain_text header banner with the metadata as a
	// context line below (ka-0000002919); without one the metadata is the section
	// header. Header text must be mrkdwn-unescaped and fit Slack's 150-char cap.
	var blocks []slack.Block
	tldr := ""
	if parsed {
		tldr = strings.TrimSpace(w.TLDR)
	}
	// Platform-failure headline (ka-0000000315) replaces the synthesized tldr.
	if platformFailure {
		tldr = platformFailureTLDR
	}
	// Prior-turn fallback (ka-0000003692): a resumed agent's live wrapup is
	// deleted, so use prior_wrapup's tldr, marked "Previous turn:". It ranks below
	// the real tldr and the platform-failure override, above the close_reason /
	// failure_reason derivations. Gated on prior_wrapup, not prior_agent_id, which
	// the same-id revive deletes (ka-0000006931).
	if tldr == "" {
		if pw := priorWrapupTLDR(fields); pw != "" {
			tldr = priorTurnTLDRPrefix + pw
		}
	}
	// Derive a headline from close_reason (ka-0000000101), e.g. for crashes that
	// bypassed the close path, so the card is never metadata-only.
	if tldr == "" {
		if cr := strings.TrimSpace(fields["close_reason"]); cr != "" {
			tldr = closeReasonTLDR(cr)
		}
	}
	// Then failure_reason / agent_state, for beads closed with an empty
	// close_reason (Fix B, ka-0000005547).
	if tldr == "" {
		tldr = failureReasonTLDR(strings.TrimSpace(fields["failure_reason"]), strings.TrimSpace(fields["agent_state"]))
	}
	if tldr != "" {
		// slackHeaderText caps the composed string: an unmapped glyph
		// renders as a long shortcode, and overflow fails the whole
		// chat.update with invalid_blocks (ka-0000006181).
		headline := slackHeaderText(
			fmt.Sprintf("%s %s —", outcomeHeaderGlyph(th, glyph), label),
			truncateRunes(unescapeSlackMrkdwn(tldr), wrapUpTLDRHeaderCap))
		blocks = []slack.Block{
			slack.NewHeaderBlock(
				slack.NewTextBlockObject("plain_text", headline, true, false)),
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn", metaText, false, false)),
		}
	} else {
		blocks = []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", metaText, false, false),
				nil, nil),
		}
	}

	// Restart condition (ka-0000000754): when a respawn is armed, the "I will
	// come back when X" line goes right under the headline; it can be driven by
	// bead fields alone, so it renders for unparsed wrap-ups too.
	if rc := restartConditionLine(th, w, fields); rc != "" {
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", rc, false, false),
				nil, nil),
		)
	}

	// Successor continuation (ka-0000005086): link a successor=new card forward
	// via close_reason "superseded: continued as <id>"; empty otherwise.
	if fields != nil {
		if cl := b.successorContinuationLine(th, w, fields["close_reason"]); cl != "" {
			blocks = append(blocks,
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn", cl, false, false),
					nil, nil),
			)
		}
	}

	if !parsed {
		// Unparseable wrap-up: surface the raw text so nothing is lost.
		if raw := strings.TrimSpace(wrapupJSON); raw != "" {
			blocks = append(blocks,
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("> %s", truncateText(raw, 500)), false, false),
					nil, nil),
			)
		}
		return append(blocks, b.clearAgentActionBlock(agent))
	}

	if acc := strings.TrimSpace(w.Accomplishments); acc != "" {
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Accomplishments:* %s", truncateText(acc, 1000)), false, false),
				nil, nil),
		)
	}

	var links []string
	if len(w.PullRequests) > 0 {
		links = append(links, fmt.Sprintf("*MRs/PRs:* %s", strings.Join(w.PullRequests, ", ")))
	}
	if len(w.BeadsClosed) > 0 {
		linked := make([]string, len(w.BeadsClosed))
		for i, id := range w.BeadsClosed {
			linked[i] = b.beadLink(id, id)
		}
		links = append(links, fmt.Sprintf("*Beads:* %s", strings.Join(linked, ", ")))
	}
	for k, v := range w.Custom {
		links = append(links, fmt.Sprintf("*%s:* %s", k, truncateText(v, 200)))
	}
	if len(links) > 0 {
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", strings.Join(links, "\n"), false, false),
				nil, nil),
		)
	}

	// Blockers + handoff go in a de-emphasized context block.
	var detail []string
	if blk := strings.TrimSpace(w.Blockers); blk != "" {
		detail = append(detail, fmt.Sprintf("%s *Blockers:* %s", th.Semantic(SemBlockedBy), truncateText(blk, 500)))
	}
	if h := strings.TrimSpace(w.HandoffNotes); h != "" {
		detail = append(detail, fmt.Sprintf("%s *Handoff:* %s", th.Semantic(SemHandoff), truncateText(h, 500)))
	}
	if len(detail) > 0 {
		blocks = append(blocks,
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn", strings.Join(detail, "\n"), false, false),
			),
		)
	}

	return append(blocks, b.clearAgentActionBlock(agent))
}

// clearAgentActionBlock returns the actions block with the wrap-up card's Clear
// button.
func (b *Bot) clearAgentActionBlock(agent string) slack.Block {
	clearBtn := slack.NewButtonBlockElement(
		"clear_agent",
		agent,
		slack.NewTextBlockObject("plain_text", "Clear", false, false),
	)
	return slack.NewActionBlock("", clearBtn)
}

// effectiveAgentState promotes a stale "spawning"/"assigning" state to
// "working" once tool stats show a tool event or a SessionStart (Live), since
// the bead's agent_state can lag or never be written (ka-0000005799,
// ka-0000003479). Other states are never overridden.
func effectiveAgentState(agentState string, toolStats *AgentToolStats) string {
	if (agentState == "spawning" || agentState == "assigning") &&
		toolStats != nil && (toolStats.TotalTools > 0 || toolStats.Live) {
		return "working"
	}
	return agentState
}

// buildAgentCardBlocks builds an agent status card. subject is the card title
// (agentCardSubject; "" falls back to the slug), taskTitle the in-progress
// bead ("" if idle), seen the last activity (zero = unknown), agentHref the
// agentCardHref link target ("" for none), and imageTag/bridgeVersion are shown
// in the context line.
func buildAgentCardBlocks(th Theme, agent, subject string, pendingCount int, agentState, taskTitle string, seen time.Time, agentHref, imageTag, role, costLine, contextLine string, toolStats *AgentToolStats, bridgeVersion string, schedulingBlocked bool, trigger triggerCardInfo, local *localBadge) []slack.Block {
	name := extractAgentName(agent)
	project := extractAgentProject(agent)
	agentState = effectiveAgentState(agentState, toolStats)
	// The slug stays in the context line as the bead identifier.
	title := subject
	if title == "" {
		title = name
	}

	var indicator, status string
	switch {
	case pendingCount > 0:
		// Decision pending is the primary indicator (ka-0000000977); the
		// decision UI lives in NotifyDecision's thread reply. A count shows
		// when more than one is outstanding.
		indicator = th.Glyph(LifecycleNeedsDecision)
		if pendingCount == 1 {
			status = "decision pending"
		} else {
			status = fmt.Sprintf("%d decisions pending", pendingCount)
		}
	case agentState == "working":
		indicator = th.Glyph(LifecycleWorking)
		if toolStats != nil && toolStats.TotalTools > 0 {
			status = buildCardStatusFromToolStats(toolStats)
		} else {
			status = "working"
		}
	case agentState == "spawning" && schedulingBlocked:
		// Pod Pending+Unschedulable, vs a normal slow spawn
		// (ka-0000005976).
		indicator = th.Glyph(LifecycleBlocked)
		status = "stuck: cluster capacity"
	case agentState == "spawning":
		indicator = th.Glyph(LifecycleSpawning)
		status = "starting"
	case agentState == "done":
		indicator = th.Glyph(LifecycleDone)
		status = "done"
	case agentState == "failed":
		indicator = th.Glyph(LifecycleAborted)
		status = "failed"
	case agentState == "rate_limited":
		indicator = th.Glyph(LifecycleBlocked)
		status = "rate limited"
	case agentState == "paused":
		// A laptop agent whose runner went away: held, not dead (D5).
		indicator = th.Glyph(LifecycleBlocked)
		status = "paused (laptop offline)"
	default:
		// No idle glyph in the locked vocabulary; the label carries it.
		indicator = th.Glyph(LifecycleSpawning)
		status = "idle"
	}

	// Status first and the agent link last: Slack previews use the leading
	// text (ka-0000002097, ka-0000003557).
	displayName := agentCardLink(agentHref, title)
	headerText := fmt.Sprintf("%s %s", indicator, status)
	if project != "" {
		headerText += fmt.Sprintf(" \u00b7 _%s_", project)
	}
	// A laptop agent's default role is "local"; the badge already says so.
	if role != "" && (local == nil || role != "local") {
		headerText += fmt.Sprintf(" \u00b7 %s", role)
	}
	if local != nil {
		headerText += local.headerSuffix(th)
	}
	headerText += fmt.Sprintf(" \u00b7 *%s*", displayName)

	contextText := fmt.Sprintf("`%s` \u00b7 Decisions thread below", agent)
	if imageTag != "" {
		contextText += fmt.Sprintf(" \u00b7 %s", imageTag)
	}
	if !seen.IsZero() {
		contextText += fmt.Sprintf(" \u00b7 _%s_", formatAge(seen))
	}
	if bridgeVersion != "" {
		contextText += fmt.Sprintf(" \u00b7 %s", bridgeVersion)
	}
	if taskTitle != "" {
		contextText += fmt.Sprintf("\n%s %s", th.Tool(ToolDefault), truncateText(taskTitle, 200))
	}
	// Context-size chip only (ka-0000007210); cost comes from costLine.
	if contextLine != "" {
		contextText += fmt.Sprintf("\n%s", contextLine)
	}
	// A laptop agent runs on its owner's own Claude login, so the fleet has
	// no spend to show (local-agents spec D1).
	if local != nil {
		contextText += fmt.Sprintf("\n%s owner-billed", th.Semantic(SemCost))
	} else if costLine != "" {
		contextText += fmt.Sprintf("\n%s %s", th.Semantic(SemCost), costLine)
	}
	// How the agent was spawned (ka-0000004046).
	if trigger.SpawnVectorLine != "" {
		contextText += "\n" + themeSpawnVectorLine(th, trigger.SpawnVectorLine)
	}
	if line := formatTriggerCardLine(th, trigger); line != "" {
		contextText += "\n" + line
	}
	// Ownership footer: gardener and human owner (ka-0000003131).
	if line := formatOwnershipCardLine(th, trigger); line != "" {
		contextText += "\n" + line
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", headerText, false, false),
			nil, nil),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", contextText, false, false)),
	}

	// Clear button for terminated agents.
	if agentState == "done" || agentState == "failed" {
		clearBtn := slack.NewButtonBlockElement(
			"clear_agent",
			agent,
			slack.NewTextBlockObject("plain_text", "Clear", false, false),
		)
		blocks = append(blocks, slack.NewActionBlock("", clearBtn))
	}

	return blocks
}

// buildCardStatusFromToolStats builds the card's status string in the thread
// status bar format, e.g.:
//
//	"running commands... (44 tools) [9m57s] · last: Bash(git status)"
//	"Working... (12 tools) [2m30s] · last: Read(settings.json)"
func buildCardStatusFromToolStats(stats *AgentToolStats) string {
	var b strings.Builder
	if stats.Activity != "" {
		fmt.Fprintf(&b, "%s...", stats.Activity)
	} else {
		b.WriteString("working")
	}
	if stats.TotalTools > 0 {
		fmt.Fprintf(&b, " (%d tools)", stats.TotalTools)
	}
	if !stats.StartedAt.IsZero() {
		elapsed := time.Since(stats.StartedAt).Truncate(time.Second)
		fmt.Fprintf(&b, " [%s]", formatDuration(elapsed))
	}
	if stats.LastTool != "" {
		if stats.LastToolLabel != "" {
			fmt.Fprintf(&b, " · last: %s(%s)", stats.LastTool, stats.LastToolLabel)
		} else {
			fmt.Fprintf(&b, " · last: %s", stats.LastTool)
		}
	}
	return b.String()
}

// formatTriggerCardLine renders the context line for the spawning trigger, or
// "" without a trigger id:
//
//	:zap: trigger: <linked-title> · via <source>: `<match-key>` · run #N
//
// The title links to beads-viewer when configured; run #0 (unknown) is omitted.
func formatTriggerCardLine(th Theme, t triggerCardInfo) string {
	if t.ID == "" {
		return ""
	}
	titleText := t.Title
	if titleText == "" {
		titleText = t.ID
	}
	var titleRendered string
	if t.ViewerBaseURL != "" {
		// Bead pages are "GET /{id}" (cmd/beads-viewer/server.go); "/b/<id>"
		// is a fragment route and 404s.
		titleRendered = fmt.Sprintf("<%s/%s|%s>",
			strings.TrimRight(t.ViewerBaseURL, "/"), t.ID, escapeSlackMrkdwn(titleText))
	} else {
		titleRendered = escapeSlackMrkdwn(titleText)
	}
	line := fmt.Sprintf("%s trigger: %s", th.Semantic(SemTrigger), titleRendered)
	switch {
	case t.Source != "" && t.MatchKey != "":
		line += fmt.Sprintf(" \u00b7 via %s: `%s`", t.Source, t.MatchKey)
	case t.Source != "":
		line += fmt.Sprintf(" \u00b7 via %s", t.Source)
	case t.MatchKey != "":
		line += fmt.Sprintf(" \u00b7 `%s`", t.MatchKey)
	}
	if t.RunCount > 0 {
		line += fmt.Sprintf(" \u00b7 run #%d", t.RunCount)
	}
	return line
}

// formatOwnershipCardLine renders the card's ownership footer (ka-0000003131),
// or "" when neither half is set:
//
//	:compass: Resolved by: <gardener> · Owner: <@U123>
//
// The owner renders via formatOwnerMention.
func formatOwnershipCardLine(th Theme, t triggerCardInfo) string {
	var parts []string
	if t.ResolvedBy != "" {
		parts = append(parts, "Resolved by: "+escapeSlackMrkdwn(t.ResolvedBy))
	}
	if t.Owner != "" {
		parts = append(parts, "Owner: "+formatOwnerMention(t.Owner))
	}
	if len(parts) == 0 {
		return ""
	}
	return th.Semantic(SemAssignee) + " " + strings.Join(parts, " \u00b7 ")
}

// formatOwnerMention renders an owner id: slack:U… becomes a `<@U…>` mention;
// any other typed id (oidc:<email>) has its operator prefix stripped.
func formatOwnerMention(owner string) string {
	if uid, ok := extractSlackUserIDOperator(owner); ok {
		return fmt.Sprintf("<@%s>", uid)
	}
	return escapeSlackMrkdwn(stripOperatorPrefix(owner))
}

// escapeSlackMrkdwn escapes the three Slack-mrkdwn meta characters (`<`, `>`,
// `&`) inside a link label so user-supplied trigger titles cannot break the
// surrounding `<url|label>` syntax.
func escapeSlackMrkdwn(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// unescapeSlackMrkdwn reverses the "&lt;"/"&gt;" escaping that
// normalizeAndValidateTLDR (cmd/gb/wrapup.go) applies to a stored tldr, for
// plain_text header blocks. "&amp;" is never produced, so it is left alone.
func unescapeSlackMrkdwn(s string) string {
	r := strings.NewReplacer("&lt;", "<", "&gt;", ">")
	return r.Replace(s)
}

// outcomeHeaderGlyph maps a legacy emoji shortcode to the theme glyph for its
// lifecycle state, since an unmapped `:shortcode:` can overflow Slack's
// 150-char header cap (ka-0000006181). Other values pass through unchanged.
func outcomeHeaderGlyph(th Theme, glyph string) string {
	if state, ok := legacyOutcomeShortcodes[glyph]; ok {
		return th.Glyph(state)
	}
	return glyph
}

// legacyOutcomeShortcodes maps retired outcome emoji shortcodes to their
// lifecycle states, so the theme (via th.Glyph) owns the rendered glyph.
var legacyOutcomeShortcodes = map[string]string{
	":white_check_mark:": LifecycleDone,
	":no_entry:":         LifecycleBlocked,
	// Legacy "aborted" / platform-failure glyphs.
	":warning:":           LifecycleAborted,
	":satellite_antenna:": LifecycleAborted,
	":arrow_upper_right:": LifecycleHandedOff,
	// successor=new: work continues on a freshly-minted agent (ka-0000005086).
	":link:": LifecycleContinued,
	// Respawn-armed / restart_when: progressing (ka-0000006181).
	":hourglass_flowing_sand:": LifecycleRestarted,
	":x:":                      LifecycleAborted,
	":headstone:":              LifecycleReaped,
	":speech_balloon:":         LifecycleNeedsDecision,
}

// buildCompactWorkingCardBlocks builds the compact two-block card for a
// working thread card (Change 3 / UX Q3): the tool-stat status line, without
// the heavy context block. Terminal states use buildAgentCardBlocks.
func buildCompactWorkingCardBlocks(th Theme, agent, subject, agentState, agentHref, role, usageLine string, toolStats *AgentToolStats) []slack.Block {
	name := extractAgentName(agent)
	project := extractAgentProject(agent)
	agentState = effectiveAgentState(agentState, toolStats)
	title := subject
	if title == "" {
		title = name
	}

	var indicator, status string
	switch {
	case agentState == "working":
		indicator = th.Glyph(LifecycleWorking)
		if toolStats != nil && toolStats.TotalTools > 0 {
			status = buildCardStatusFromToolStats(toolStats)
		} else {
			status = "working"
		}
	case agentState == "spawning":
		indicator = th.Glyph(LifecycleSpawning)
		status = "starting"
	default:
		indicator = th.Glyph(LifecycleSpawning)
		status = agentState
	}

	// Status first, agent link last, as in buildAgentCardBlocks
	// (ka-0000003557).
	displayName := agentCardLink(agentHref, title)
	headerText := fmt.Sprintf("%s %s", indicator, status)
	if project != "" {
		headerText += fmt.Sprintf(" · _%s_", project)
	}
	if role != "" {
		headerText += fmt.Sprintf(" · %s", role)
	}
	headerText += fmt.Sprintf(" · *%s*", displayName)

	contextText := fmt.Sprintf("`%s` · Decisions thread below", agent)
	// Lead with live context size + cost (ka-0000007210).
	if usageLine != "" {
		contextText = fmt.Sprintf("%s · `%s`", usageLine, agent)
	}

	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", headerText, false, false),
			nil, nil),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", contextText, false, false)),
	}
}

// terminalCardState derives the card state for a closed agent bead whose
// in-memory agent_state was lost, from its persisted agent_state and
// close_reason, rather than painting every lost state a false "done"
// (ka-0000005513):
//
//   - rate_limited        → "rate_limited"
//   - failed              → "failed"
//   - abnormal reap       → "reaped"
//   - clean done / benign → "done"
func terminalCardState(beadAgentState, closeReason string) string {
	switch beadAgentState {
	case "rate_limited":
		return "rate_limited"
	case "failed":
		return "failed"
	case "done":
		// Benign orphan cleanups are genuine completions.
		return "done"
	}

	// No agent_state: any non-clean close_reason is "reaped".
	switch {
	case closeReason == "":
		// No signal: keep the historical default.
		return "done"
	case strings.HasPrefix(closeReason, "orphan: source task"):
		return "done"
	default:
		// podless_leak:, stale:, context_cancelled, idle_ttl_expired,
		// "queued for …", "spawn expired …", orphan: (non-source-task), etc.
		return "reaped"
	}
}

// isHoldCanary reports whether an agent bead is a "hold canary" probe that is
// expected to be reaped without a wrap-up (ka-0000007028): wrapup_optout=true,
// role=gvisor-poc, or "HOLD canary" in the prompt (case-insensitive).
func isHoldCanary(fields map[string]string) bool {
	if fields == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(fields["wrapup_optout"]), "true") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(fields["role"]), "gvisor-poc") {
		return true
	}
	if strings.Contains(strings.ToLower(fields["prompt"]), "hold canary") {
		return true
	}
	return false
}

// buildCompactAgentCardBlocks builds the single-line card (with a Clear
// button) that replaces a finished agent's full card. usageLine is the
// formatAgentUsageLine triad, since this card has no separate cost line
// (ka-0000003171).
func buildCompactAgentCardBlocks(th Theme, agent, agentState, usageLine string, fields map[string]string) []slack.Block {
	name := extractAgentName(agent)

	var indicator, stateLabel string
	switch agentState {
	case "done":
		indicator = th.Glyph(LifecycleDone)
		stateLabel = "agent finished"
	case "failed":
		indicator = th.Glyph(LifecycleAborted)
		stateLabel = agentState
	case "rate_limited":
		indicator = th.Glyph(LifecycleBlocked)
		stateLabel = "rate limited"
	case "reaped":
		// Died without a clean self-report; never ✅ (ka-0000005513).
		indicator = th.Glyph(LifecycleReaped)
		stateLabel = "reaped"
	case "moved":
		indicator = th.Glyph(LifecycleHandedOff)
		stateLabel = agentState
	default:
		indicator = th.Glyph(LifecycleSpawning)
		stateLabel = agentState
	}

	// Hold canaries never wrap up by design, so show a neutral glyph rather
	// than the no-wrap-up stigma, which must stay meaningful (ka-0000007028).
	if isHoldCanary(fields) {
		switch agentState {
		case "reaped", "failed", "":
			indicator = th.Semantic(SemIdle)
			stateLabel = "hold canary · no wrap-up expected"
		}
	}

	// Glyph stays next to its label (accessibility rule).
	text := fmt.Sprintf("%s %s · %s", indicator, stateLabel, name)
	if usageLine != "" {
		text += fmt.Sprintf(" · %s", usageLine)
	}

	clearBtn := slack.NewButtonBlockElement(
		"clear_agent",
		agent,
		slack.NewTextBlockObject("plain_text", "Clear", false, false),
	)

	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil,
			slack.NewAccessory(clearBtn),
		),
	}
}

// formatCostSummary formats per-agent cost fields, e.g.
// "$1.23 · 50k in · 12k out · 3 sessions", or "" when there are none.
func formatCostSummary(fields map[string]string) string {
	cost := fields["total_cost_usd"]
	inputTok := fields["total_input_tokens"]
	outputTok := fields["total_output_tokens"]
	sessions := fields["session_count"]

	if cost == "" && inputTok == "" && outputTok == "" {
		return ""
	}

	var parts []string
	if cost != "" {
		if f, err := strconv.ParseFloat(cost, 64); err == nil && f > 0 {
			parts = append(parts, fmt.Sprintf("$%.2f", f))
		}
	}
	if inputTok != "" {
		if n, err := strconv.ParseInt(inputTok, 10, 64); err == nil && n > 0 {
			parts = append(parts, fmt.Sprintf("%s in", formatTokenCount(n)))
		}
	}
	if outputTok != "" {
		if n, err := strconv.ParseInt(outputTok, 10, 64); err == nil && n > 0 {
			outStr := fmt.Sprintf("%s out", formatTokenCount(n))
			// Thinking tokens: a chars/4 estimate already inside output.
			if thinkTok := fields["total_thinking_tokens"]; thinkTok != "" {
				if t, err := strconv.ParseInt(thinkTok, 10, 64); err == nil && t > 0 {
					outStr += fmt.Sprintf(" (%s thinking est.)", formatTokenCount(t))
				}
			}
			parts = append(parts, outStr)
		}
	}
	if sessions != "" {
		if n, err := strconv.ParseInt(sessions, 10, 64); err == nil && n > 0 {
			parts = append(parts, fmt.Sprintf("%d sessions", n))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// formatTokenCount formats n as e.g. "500", "1.2k", "1.2M".
func formatTokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// triggerCardInfo bundles the trigger context shown on an agent card.
// Fields are optional; an empty ID disables rendering.
type triggerCardInfo struct {
	ID            string // trigger bead id (ka-XXX)
	Title         string // trigger bead title
	Source        string // "jira", "gitlab", "schedule", …
	MatchKey      string // source-specific match key (e.g. "FOO-123")
	RunCount      int    // total fires (count of beads labeled trigger:<id>); 0 = unknown
	ViewerBaseURL string // beads-viewer base URL for linking the trigger title

	// SpawnVectorLine is the pre-rendered spawn-vector line from
	// formatSpawnVectorLine (e.g. ":hook: FSM-1234 fsm webhook"); "" if unknown.
	SpawnVectorLine string

	// ResolvedBy is the spawning gardener's display name (ka-0000003131);
	// empty for non-gardener spawns.
	ResolvedBy string
	// Owner is the human operator id spawned on behalf of; may be empty.
	Owner string
}

// snapshotTriggerInfo collects the cached trigger context for an agent.
// Must be called with b.mu held.
func (b *Bot) snapshotTriggerInfo(agent string) triggerCardInfo {
	return triggerCardInfo{
		ID:              b.agentTriggerID[agent],
		Title:           b.agentTriggerTitle[agent],
		Source:          b.agentTriggerSource[agent],
		MatchKey:        b.agentTriggerMatchKey[agent],
		ViewerBaseURL:   b.beadsViewerURL,
		SpawnVectorLine: b.agentSpawnVectorLine[agent],
		ResolvedBy:      b.agentResolvedBy[agent],
		Owner:           b.agentOwner[agent],
	}
}

// fetchTriggerRunCount returns a trigger's fire count (beads labeled
// "trigger:<id>"), cached for triggerRunCountTTL. 0 means unknown (error or
// empty triggerID), not zero.
func (b *Bot) fetchTriggerRunCount(ctx context.Context, triggerID string) int {
	if triggerID == "" || b.daemon == nil {
		return 0
	}
	b.triggerRunCountCacheMu.Lock()
	if entry, ok := b.triggerRunCountCache[triggerID]; ok && time.Since(entry.cachedAt) < triggerRunCountTTL {
		b.triggerRunCountCacheMu.Unlock()
		return entry.count
	}
	b.triggerRunCountCacheMu.Unlock()

	result, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"trigger:" + triggerID},
		// Limit=1: only Total is consumed.
		Limit: 1,
	})
	if err != nil {
		b.logger.Debug("fetchTriggerRunCount: list beads failed",
			"trigger", triggerID, "error", err)
		return 0
	}
	count := result.Total
	b.triggerRunCountCacheMu.Lock()
	b.triggerRunCountCache[triggerID] = triggerRunCountEntry{count: count, cachedAt: time.Now()}
	b.triggerRunCountCacheMu.Unlock()
	return count
}

// resolveAgentPod returns the pod_name for a card render's agent link and the
// agent bead id that keys its Glass (Lands GL-13, step 6.8). It reads the
// bead's pod_name note, which SyncAll keeps current across thaw, warm-pool
// claims and restarts, rather than the sticky b.agentPodName cache, and
// self-heals the cache (ka-0000004617). On fetch failure or an unwritten note
// it falls back to cachedPodName.
func (b *Bot) resolveAgentPod(ctx context.Context, agent, cachedPodName string) (podName, agentID string) {
	if b.coopmuxPublicURL == "" && b.beadsViewerURL == "" {
		// No agent link is rendered; skip the round-trip.
		return cachedPodName, ""
	}
	detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, extractAgentName(agent))
	if err != nil || detail == nil {
		return cachedPodName, ""
	}
	podName = beadsapi.ParseNotes(detail.Notes)["pod_name"]
	if podName == "" {
		// Note not written yet: don't clobber a good cache.
		return cachedPodName, detail.ID
	}
	if podName != cachedPodName {
		b.mu.Lock()
		b.agentPodName[agent] = podName
		b.mu.Unlock()
	}
	return podName, detail.ID
}

// agentCardHref is where a card render links the agent's name: agentCardHref
// over the pod and agent bead resolveAgentPod reads.
func (b *Bot) agentCardHref(ctx context.Context, agent, cachedPodName string) string {
	podName, agentID := b.resolveAgentPod(ctx, agent, cachedPodName)
	return agentCardHref(b.beadsViewerURL, agentID, b.coopmuxPublicURL, podName)
}

// fetchAndCachePodName caches an active agent's bead details (pod_name,
// image_tag, routing) via cacheAgentBeadDetail.
func (b *Bot) fetchAndCachePodName(ctx context.Context, agent string) {
	detail, err := b.daemon.FindAgentBead(ctx, agent)
	if err != nil {
		b.logger.Debug("could not fetch agent bead for pod_name", "agent", agent, "error", err)
		return
	}
	b.cacheAgentBeadDetail(agent, detail)
}

// cacheAgentBeadDetail fills the agent's in-memory caches (pod, image tag,
// role, project, spawn channel, card footer, thread-bound) from its agent bead,
// writing only non-empty values.
func (b *Bot) cacheAgentBeadDetail(agent string, detail *beadsapi.BeadDetail) {
	notes := beadsapi.ParseNotes(detail.Notes)
	podName := notes["pod_name"]
	imageTag := extractImageTag(notes["image_tag"])
	role := detail.Fields["role"]

	project := detail.Fields["project"]

	b.mu.Lock()
	if podName != "" {
		b.agentPodName[agent] = podName
	}
	if imageTag != "" {
		b.agentImageTag[agent] = imageTag
	}
	if role != "" {
		b.agentRole[agent] = role
	}
	if project != "" {
		b.agentProject[agent] = project
	}
	if spawnCh := detail.Fields["slack_spawn_channel"]; spawnCh != "" {
		b.agentSpawnChannel[agent] = spawnCh
		b.recordInheritedSpawnRoleLocked(agent, detail.Fields)
	}
	// The schedule channel override wins and is an explicit pin.
	if schedCh := detail.Fields["schedule_slack_channel"]; schedCh != "" {
		b.agentSpawnChannel[agent] = schedCh
		delete(b.agentSpawnInheritedBy, agent)
	}
	// Card footer (ka-0000003131), recovered after a resume or field-poor event.
	if rb := detail.Fields["resolved_by"]; rb != "" {
		b.agentResolvedBy[agent] = rb
	}
	if ow := detail.Fields["owner"]; ow != "" {
		b.agentOwner[agent] = ow
	}
	// Recover thread-bound, as NotifyAgentState does, or an agent that
	// stopped before a restart gets a stray card (gd-ckbFu30SZ1).
	if isValidThreadBinding(detail.Fields["slack_thread_channel"], detail.Fields["slack_thread_ts"]) ||
		strings.HasPrefix(detail.Fields["spawn_source"], "slack-thread") {
		b.markThreadBound(agent)
	}
	b.mu.Unlock()
}

// extractImageTag returns the tag of an image reference
// ("ghcr.io/org/agent:v2026.58.3" → "v2026.58.3"); a bare tag is returned as-is.
func extractImageTag(image string) string {
	if image == "" {
		return ""
	}
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// extractAgentProject returns the first segment (project) of an agent identity.
// "gardenio/crew/test-bot" → "gardenio", "test-bot" → ""
func extractAgentProject(identity string) string {
	if i := strings.Index(identity, "/"); i >= 0 {
		return identity[:i]
	}
	return ""
}

// AgentCardLocation returns the channel and ts of the agent's status card (hot
// cache, then persisted state) so callers can post or thread "where the card
// lives" (ka-0000002021). ok is false when no card has been observed.
func (b *Bot) AgentCardLocation(agent string) (channel, ts string, ok bool) {
	if b == nil || agent == "" {
		return "", "", false
	}
	b.mu.Lock()
	if ref, found := b.agentCards[agent]; found && ref.ChannelID != "" && ref.Timestamp != "" {
		b.mu.Unlock()
		return ref.ChannelID, ref.Timestamp, true
	}
	b.mu.Unlock()
	if b.state != nil {
		if ref, found := b.state.GetAgentCard(agent); found && ref.ChannelID != "" && ref.Timestamp != "" {
			return ref.ChannelID, ref.Timestamp, true
		}
	}
	return "", "", false
}

// resolveCardChannel is resolveChannel for a card posted on an agent's behalf
// (decision, escalation, result, pingback; card names it in the log). When the
// routing caches are empty (restart, missed spawn), it refreshes them from the
// agent bead first (gd-35LkF76wAr), and warns if the default channel is still
// used for lack of a project.
func (b *Bot) resolveCardChannel(ctx context.Context, agent, card string) string {
	if agent == "" || b.daemon == nil {
		return b.resolveChannel(agent)
	}
	b.mu.Lock()
	spawnCh := b.agentSpawnChannel[agent]
	project := b.agentProject[agent]
	role := b.agentRole[agent]
	b.mu.Unlock()
	if spawnCh == "" && (project == "" || role == "") {
		b.refreshAgentRouting(ctx, agent)
	}

	ch := b.resolveChannel(agent)
	if ch == b.channel {
		b.mu.Lock()
		project = b.agentProject[agent]
		b.mu.Unlock()
		if project == "" {
			b.logger.Warn("agent has no project; posting card to the default channel",
				"agent", agent, "card", card, "channel", ch)
		}
	}
	return ch
}

// refreshAgentRouting reloads agent's routing caches from its agent bead,
// falling back to a closed bead since results can land after the agent ends.
func (b *Bot) refreshAgentRouting(ctx context.Context, agent string) {
	detail, err := b.daemon.FindAgentBead(ctx, agent)
	if err != nil || detail == nil {
		detail, err = b.daemon.FindAgentBeadAnyStatus(ctx, agent)
	}
	if err != nil || detail == nil {
		b.logger.Debug("could not fetch agent bead for card routing", "agent", agent, "error", err)
		return
	}
	b.cacheAgentBeadDetail(agent, detail)
}

// resolveChannel returns the target Slack channel for an agent. Priority
// (gd-3JHijlgZrE; keep in step with conversation.Place):
//  1. Router override/pattern match (if router configured)
//  2. Explicit pin: /spawn channel, --slack-spawn-channel, slack_spawn_channel
//     from a Slack-bound task, or schedule_slack_channel. An inherited spawn
//     channel (slack_spawn_channel_inherited_from_role) pins only when the
//     spawner had the same role.
//  3. Role-scoped channel for (project, role) via project channel_roles
//     3b. Inherited spawn channel from a different-role spawner
//  4. Project primary channel (first of the project bead's slack_channel)
//  5. Router default / bot default channel
//
// The rules live in explainChannelRoute, shared with gb slack route
// (GET /api/slack/route).
func (b *Bot) resolveChannel(agent string) string {
	return b.explainChannelRoute(agent, false).Channel
}

// recordInheritedSpawnRoleLocked records whether the agent's (just written,
// non-empty) spawn channel was inherited from a spawner, per
// slack_spawn_channel_inherited_from_role; without the marker it is an
// explicit pin and any stale marker is dropped. Caller holds b.mu.
func (b *Bot) recordInheritedSpawnRoleLocked(agent string, fields map[string]string) {
	if from, ok := fields[beadsapi.FieldSlackSpawnChannelInheritedFromRole]; ok {
		b.agentSpawnInheritedBy[agent] = from
		return
	}
	delete(b.agentSpawnInheritedBy, agent)
}

// projectChannelCacheTTL bounds the project→channel caches on the hot
// resolveChannel path.
const projectChannelCacheTTL = 30 * time.Second

// projectPrimaryChannel returns a project's first Slack channel, or "", from a
// TTL cache. On refresh failure the last good cache keeps serving so a daemon
// blip doesn't reroute everything to the default channel.
func (b *Bot) projectPrimaryChannel(project string) string {
	b.mu.Lock()
	if b.projectChannelCache != nil && time.Since(b.projectChannelCacheAt) < projectChannelCacheTTL {
		ch := b.projectChannelCache[project]
		b.mu.Unlock()
		return ch
	}
	b.mu.Unlock()

	projects, err := b.daemon.ListProjectBeads(context.Background())
	if err != nil {
		b.mu.Lock()
		ch, warm := b.projectChannelCache[project], b.projectChannelCache != nil
		b.mu.Unlock()
		b.logger.Warn("resolveChannel: failed to list projects; serving last good project channel cache",
			"project", project, "cache_warm", warm, "error", err)
		return ch
	}

	if conflicts := beadsapi.ChannelConflicts(projects); len(conflicts) > 0 {
		b.logger.Error("Slack channel conflict: multiple projects claim the same channel",
			"conflicts", conflicts)
	}
	// Repo conflicts cause MR-mention split-routing (ka-0000002164).
	if conflicts := beadsapi.GitURLConflicts(projects); len(conflicts) > 0 {
		b.logger.Warn("project git_url conflict: multiple projects claim the same git_url",
			"conflicts", conflicts)
	}
	if conflicts := beadsapi.ReposConflicts(projects); len(conflicts) > 0 {
		b.logger.Warn("project repos[] conflict: multiple projects list the same repo",
			"conflicts", conflicts)
	}

	cache := make(map[string]string, len(projects))
	roleCache := make(map[string]map[string]string, len(projects))
	for name, info := range projects {
		if len(info.SlackChannels) > 0 {
			cache[name] = info.SlackChannels[0]
		}
		if len(info.ChannelRoles) > 0 {
			roles := make(map[string]string, len(info.ChannelRoles))
			for ch, rs := range info.ChannelRoles {
				for _, r := range rs {
					if r == "" {
						continue
					}
					// Lowest channel ID wins when a role is on several
					// channels (ka-0000006075), matching ChannelForRole.
					if existing, ok := roles[r]; !ok || ch < existing {
						roles[r] = ch
					}
				}
			}
			if len(roles) > 0 {
				roleCache[name] = roles
			}
		}
	}

	b.mu.Lock()
	b.projectChannelCache = cache
	b.projectRoleChannelCache = roleCache
	b.projectChannelCacheAt = time.Now()
	b.mu.Unlock()

	return cache[project]
}

// projectRoleChannel returns the channel for role in the project's
// channel_roles, or "", sharing projectPrimaryChannel's cache.
func (b *Bot) projectRoleChannel(project, role string) string {
	if project == "" || role == "" {
		return ""
	}
	b.mu.Lock()
	fresh := b.projectRoleChannelCache != nil && time.Since(b.projectChannelCacheAt) < projectChannelCacheTTL
	b.mu.Unlock()
	if !fresh {
		b.projectPrimaryChannel(project)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if roles, ok := b.projectRoleChannelCache[project]; ok {
		return roles[role]
	}
	return ""
}

// Agent kill, clear, respawn, and coop shutdown functions are in bot_agent_kill.go.
