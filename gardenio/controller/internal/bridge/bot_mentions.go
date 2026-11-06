package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailproto"
	"gardenio/controller/internal/podmanager"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// handleSlackMention is the Slack mention core, which HandleCompose runs for
// every app_mention (slice 4d, inbound_compose.go).
//
// Routing priority:
//  0. If in an agent card thread → always route to the card agent (exclusive)
//  1. If the first word after @gardenio matches a known agent name → route to that agent
//  2. If in a thread bound to an agent → route to that agent
//  3. If in a channel mapped to an agent via router → route to that agent
//  4. Otherwise → auto-spawn a thread agent (message becomes thread root)
func (b *Bot) handleSlackMention(ctx context.Context, ev *slackevents.AppMentionEvent) {
	// Ignore bot-triggered mentions.
	if ev.BotID != "" {
		return
	}
	// Stamp operator on every daemon call made downstream. See
	// operatorFromSlackUser for the slack→email resolution policy.
	if ev.User != "" {
		ctx = beadsapi.WithOperator(ctx, b.operatorFromSlackUser(ctx, ev.User))
	}

	// Strip the bot mention from the message text.
	text := stripBotMention(ev.Text, b.botUserID)
	if text == "" {
		b.logger.Debug("app_mention ignored: empty after stripping mention",
			"channel", ev.Channel)
		b.noteEmptyMention(ctx, ev)
		return
	}

	// Account linking (gd-OZJ6H2S3xx): `@gardenio link` / `unlink` / `whoami`
	// manage the person's own link and never need one; every other mention
	// runs as the linked person (their own token is the bearer) and an
	// unlinked user gets the link prompt instead.
	if b.slackLinker() != nil && ev.User != "" {
		if b.tryHandleSlackLinkCommand(ctx, ev, text) {
			return
		}
		actorCtx, refusal, ok := b.withSlackActor(ctx, ev.User)
		if !ok {
			b.postSlackRefusal(ctx, ev.Channel, ev.User, "", ev.ThreadTimeStamp, refusal)
			return
		}
		ctx = actorCtx
	}

	// Permission gate: check if the user is allowed to mention the bot.
	if b.mentionGateEnabled {
		if !b.checkMentionPermission(ctx, ev.User, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp) {
			return
		}
	}

	// @gardenio rate <agent-bead-id> — works in any thread (no agent
	// binding required) because the agent bead ID is explicit.
	if agentBeadID, ok := parseRateCommand(text); ok {
		if ev.ThreadTimeStamp == "" {
			if b.api != nil {
				_, _ = b.api.PostEphemeral(ev.Channel, ev.User,
					slack.MsgOptionText(":x: `rate` only works in threads.", false))
			}
			return
		}
		b.handleRateCommand(ctx, ev, agentBeadID)
		return
	}

	// Check for in-thread command keywords (kill, clear) before normal routing.
	if cmd, cmdArgs := parseMentionCommand(text); cmd != "" {
		if ev.ThreadTimeStamp == "" {
			if b.api != nil {
				_, _ = b.api.PostEphemeral(ev.Channel, ev.User,
					slack.MsgOptionText(":x: `"+cmd+"` only works in threads with a bound agent.", false))
			}
			return
		}
		switch cmd {
		case "kill":
			b.handleMentionKill(ctx, ev, strings.Contains(cmdArgs, "--force"))
			return
		case "clear":
			b.handleMentionClear(ctx, ev)
			return
		case "summarize":
			b.handleMentionSummarize(ctx, ev)
			return
		}
	}

	// @gardenio <epic-bead-id> [...]: start (or adopt; idempotent) a gardener
	// schedule for the epic. Falls through unless the token is an existing epic.
	if b.tryHandleEpicGardenerStart(ctx, ev, text) {
		return
	}

	var agent string
	replyTS := ev.ThreadTimeStamp // timestamp to thread the confirmation reply under
	agentSpawning := false        // true if agent is still starting up (spawning or assigning)
	// threadOwned: the agent's lifecycle is tied to this thread (a thread-<ts>
	// agent via card thread or getAgentByThread), not a router-mapped crew agent or
	// a named agent. Gates watchMailDelivery's unread-mail respawn, which must not
	// rebind a persistent crew agent to this thread (ka-0000005674).
	threadOwned := false
	// agentProject is the project label of the resolved agent, hoisted so the
	// tracking-bead lineage write below can pass it to getOrCreateThreadBead.
	var agentProject string
	// liveBead is the resolved live agent's bead, for the laptop owner check.
	var liveBead *beadsapi.BeadDetail

	// Priority 0: If this is an agent card thread, always route to the card
	// agent. Card threads are exclusive — no other agent may be addressed or
	// spawned here, regardless of explicit @name mentions.
	if ev.ThreadTimeStamp != "" {
		if cardAgent := b.getCardAgentByThread(ev.Channel, ev.ThreadTimeStamp); cardAgent != "" {
			agent = cardAgent
			// Resolve pod name and spawning state for the card agent.
			agentBead, err := b.daemon.FindAgentBead(ctx, extractAgentName(cardAgent))
			if err != nil {
				// Agent bead is gone (completed/cleaned up) — respawn with
				// session resume so the agent picks up where it left off.
				b.logger.Info("mention: card agent no longer active, respawning with session resume",
					"agent", cardAgent, "channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp, "error", err)
				b.respawnThreadAgent(ctx, ev.Channel, ev.ThreadTimeStamp, cardAgent, text, ev.User)
				return
			}
			// Terminal agent: its pod is gone and a nudge would fail
			// (ErrAgentNotNudgeable), so respawn with session resume (ka-0000002089).
			if isAgentTerminal(agentBead.Fields["agent_state"]) {
				b.logger.Info("mention: card agent in terminal state, respawning with session resume",
					"agent", cardAgent, "channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp,
					"agent_state", agentBead.Fields["agent_state"])
				b.respawnThreadAgent(ctx, ev.Channel, ev.ThreadTimeStamp, cardAgent, text, ev.User)
				return
			}
			agentSpawning = isAgentStartingUp(agentBead.Fields["agent_state"])
			agentProject = agentBead.Fields["project"]
			liveBead = agentBead
			// Card threads respawn on a gone/terminal bead, so the agent is thread-owned.
			threadOwned = true
			b.logger.Info("mention: routed to card agent (card thread exclusive)",
				"agent", cardAgent, "channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp)
		}
	}

	// Priority 1: Check if the first word is an agent name.
	// Skipped if already resolved by card thread ownership (Priority 0).
	if agent == "" {
		if resolved, remaining := b.resolveAgentFromText(ctx, text); resolved != "" {
			// Validate the agent is active via FindAgentBead.
			agentBead, err := b.daemon.FindAgentBead(ctx, extractAgentName(resolved))
			if err != nil {
				b.logger.Info("mention: agent name resolved but not active",
					"agent", resolved, "channel", ev.Channel, "error", err)
				if b.api != nil {
					errMsg := fmt.Sprintf(":x: No active agent named *%s*", extractAgentName(resolved))
					if ev.AssistantThread != nil {
						// AI container threads don't support ephemeral messages —
						// post a visible reply so the user sees the error.
						_, _, _ = b.api.PostMessage(ev.Channel,
							slack.MsgOptionText(errMsg, false),
							slack.MsgOptionTS(ev.ThreadTimeStamp))
					} else {
						_, _ = b.api.PostEphemeral(ev.Channel, ev.User,
							slack.MsgOptionText(errMsg, false))
					}
				}
				return
			}
			// Terminal agent: respawn in a thread; outside one there is nothing to
			// resume into, so reject (ka-0000002089).
			if isAgentTerminal(agentBead.Fields["agent_state"]) {
				if ev.ThreadTimeStamp != "" {
					b.logger.Info("mention: named agent in terminal state, respawning with session resume",
						"agent", resolved, "channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp,
						"agent_state", agentBead.Fields["agent_state"])
					b.respawnThreadAgent(ctx, ev.Channel, ev.ThreadTimeStamp, resolved, strings.TrimSpace(remaining), ev.User)
					return
				}
				b.logger.Info("mention: named agent in terminal state and not in a thread, rejecting",
					"agent", resolved, "channel", ev.Channel, "agent_state", agentBead.Fields["agent_state"])
				if b.api != nil {
					errMsg := fmt.Sprintf(":x: Agent *%s* just shut down. Mention it in its thread to resume it.", extractAgentName(resolved))
					_, _ = b.api.PostEphemeral(ev.Channel, ev.User,
						slack.MsgOptionText(errMsg, false))
				}
				return
			}
			agent = resolved
			text = remaining
			agentSpawning = isAgentStartingUp(agentBead.Fields["agent_state"])
			agentProject = agentBead.Fields["project"]
			liveBead = agentBead
			b.logger.Info("mention: resolved agent from text",
				"agent", agent, "channel", ev.Channel)
		}
	}

	if agent == "" && ev.ThreadTimeStamp == "" {
		// Not in a thread — check if this channel is mapped to an agent via the router.
		// This handles "@gardenio <message>" in a dedicated agent channel.
		if b.router != nil {
			agent = b.router.GetAgentByChannel(ev.Channel)
		}
		if agent == "" {
			// No mapped agent: auto-spawn a thread agent rooted at this message (DMs
			// and channels alike).
			b.logger.Info("mention: top-level auto-spawn",
				"channel", ev.Channel, "user", ev.User)
			spawnEv := *ev
			spawnEv.ThreadTimeStamp = ev.TimeStamp
			b.handleThreadSpawn(ctx, &spawnEv, text)
			return
		}
		// Use the mention's own timestamp as the reply anchor so the response
		// threads back to this message.
		replyTS = ev.TimeStamp
	} else if agent == "" {
		// In a thread — reverse-lookup which agent owns this thread.
		agent = b.getAgentByThread(ev.Channel, ev.ThreadTimeStamp)
		if agent != "" {
			// A thread-<ts> owner, so thread-owned.
			threadOwned = true
		} else {
			// Route to the channel's router-mapped agent rather than spawn a duplicate.
			// It is a persistent crew agent, so threadOwned stays false.
			if b.router != nil {
				agent = b.router.GetAgentByChannel(ev.Channel)
			}
		}
		if agent == "" {
			// Orphan thread in an unmapped channel — spawn a new ephemeral agent.
			b.handleThreadSpawn(ctx, ev, text)
			return
		}

		// Validate the thread-bound agent is still active (mirrors Priority 1 validation).
		agentBead, err := b.daemon.FindAgentBead(ctx, extractAgentName(agent))
		if err != nil {
			b.logger.Info("mention: thread-bound agent no longer active, respawning with session resume",
				"agent", agent, "channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp, "error", err)
			// Agent is gone — respawn the SAME agent name so the entrypoint
			// finds the existing session JSONL and PVC for session continuity.
			b.respawnThreadAgent(ctx, ev.Channel, ev.ThreadTimeStamp, agent, text, ev.User)
			return
		}
		// Terminal agent (e.g. a reply right after shutdown): respawn the same name
		// with session resume instead of a failing nudge (ka-0000002089).
		if isAgentTerminal(agentBead.Fields["agent_state"]) {
			b.logger.Info("mention: thread-bound agent in terminal state, respawning with session resume",
				"agent", agent, "channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp,
				"agent_state", agentBead.Fields["agent_state"])
			b.respawnThreadAgent(ctx, ev.Channel, ev.ThreadTimeStamp, agent, text, ev.User)
			return
		}
		agentSpawning = isAgentStartingUp(agentBead.Fields["agent_state"])
		agentProject = agentBead.Fields["project"]
		liveBead = agentBead
	}

	if replyTS == "" {
		// For thread messages, reply in the parent thread (not under
		// the individual message which would create a sub-thread).
		if ev.ThreadTimeStamp != "" {
			replyTS = ev.ThreadTimeStamp
		} else {
			replyTS = ev.TimeStamp
		}
	}

	// Canonicalize to short name so map lookups (agentPodName, etc.)
	// and stored refs use consistent keys.
	agent = extractAgentName(agent)

	// A laptop agent runs on its runner owner's machine: only the owner may
	// mail it from Slack, the same rule as thread replies (handleThreadForward).
	if isLaptopAgent(liveBead) && !b.laptopAgentOwnedBy(ctx, liveBead, ev.User) {
		b.logger.Info("mention: non-owner mention of a laptop agent; not forwarded",
			"agent", agent, "user", ev.User, "channel", ev.Channel)
		if b.api != nil {
			_, _ = b.api.PostEphemeral(ev.Channel, ev.User, slack.MsgOptionText(
				b.themeFor(ctx, ev.Channel).Semantic(SemBlockedBy)+fmt.Sprintf(" *%s* runs on its owner's laptop; only they can message it.", agent), false))
		}
		return
	}

	// C2 interruptible freeze (ka-0000005834): abort any in-flight freeze before
	// nudging so the warm Pod A (still serving under `runsc checkpoint
	// --leave-running`) isn't torn down. Best-effort; a 404 or error means no freeze.
	b.maybeInterruptFreeze(ctx, agent)

	// Re-nudge mid-thaw (ka-0000005830, design ka-0000005125): thaw has no live
	// fallback pod, so a mid-thaw nudge is buffered (below) rather than hitting a
	// half-restored pod or racing the thaw with a respawn. Errors report not thawing.
	bufferForThaw := b.isAgentThawing(ctx, agent)

	// Check for thread control commands: @gardenio kill, @gardenio restart, @gardenio stop.
	// These only work in threads where an agent is bound.
	if ev.ThreadTimeStamp != "" && agent != "" {
		if handled := b.handleMentionThreadCommand(ctx, ev, agent, text); handled {
			return
		}
	}

	// Strip spawn-only override flags (ka-0000001727): a live pod fixed
	// project/role/model/listen/ultracode at create time, so they are inert here and
	// must not leak into the forwarded prompt. --model and --fresh (detected before
	// stripping) get an operator note on how to get them instead of a silent drop.
	hadFreshForLiveAgent, _ := parseFreshFlag(text)
	strippedText, rawModel := stripLeadingOverrideFlags(text)
	if strippedText != text {
		b.logger.Info("mention: stripped spawn-only override flags for live agent",
			"agent", extractAgentName(agent), "channel", ev.Channel,
			"had_model_flag", rawModel != "", "had_fresh_flag", hadFreshForLiveAgent)
		text = strippedText
		if rawModel != "" {
			b.noteModelSwitchUnsupported(ev, replyTS, extractAgentName(agent), rawModel)
		}
		if hadFreshForLiveAgent {
			b.noteFreshUnsupportedForLiveAgent(ev, replyTS, extractAgentName(agent))
		}
	}

	// Resolve sender display name for log/status surfaces.
	username := ev.User
	if user, err := b.api.GetUserInfo(ev.User); err == nil {
		if user.RealName != "" {
			username = user.RealName
		} else if user.Name != "" {
			username = user.Name
		}
	}
	username = appendPronouns(username, b.UserDirectory().LookupBySlackID(ctx, ev.User))

	// Unified model (ka-0000007618, decision ka-0000005492 option C): the mention is
	// recorded as one type=message bead that is both the /threads conversation
	// bubble and the agent nudge's task. The wish anchor is the parent thread ts, or
	// the mention's own ts at top level.
	wishTS := ev.ThreadTimeStamp
	if wishTS == "" {
		wishTS = ev.TimeStamp
	}
	beadID := b.recordUnifiedThreadReply(ctx, unifiedThreadReplyInput{
		Channel:    ev.Channel,
		ThreadTS:   wishTS,
		MessageTS:  ev.TimeStamp,
		Text:       text,
		SlackUser:  ev.User,
		Project:    agentProject,
		AgentName:  extractAgentName(agent),
		ExtraLabel: "slack-mention",
		LogPrefix:  "mention",
	})
	if beadID == "" {
		// Bubble failed to persist (logged in the helper); still nudge, since
		// NudgeAgent does not require a real bead.
		b.logger.Warn("mention: unified message bead create failed; continuing with bead-less nudge",
			"channel", ev.Channel, "agent", agent, "user", username)
	} else {
		b.logger.Info("mention: recorded unified message bead",
			"bead", beadID, "agent", agent, "user", username)
	}

	// Persist the message ref for response relay. chat.go's slack-mention close
	// relay skips type=message; kept for symmetry with the thread-forward path.
	if b.state != nil && beadID != "" {
		_ = b.state.SetChatMessage(beadID, MessageRef{
			ChannelID: ev.Channel,
			Timestamp: replyTS,
			Agent:     agent,
		})
	}

	// Mid-thaw: buffer the nudge until the thaw releases Pod B, bounded by a
	// deadline (design ka-0000005125). Never respawn here; it would race the
	// in-flight thaw with a duplicate spawn. On timeout, fall through to the
	// normal fallback (thread respawn / "next cycle" chat).
	if bufferForThaw {
		statusText := fmt.Sprintf("Resuming... (tracking: %s)", b.beadLink(beadID, beadID))
		b.SetThreadStatus(ctx, ev.Channel, replyTS, statusText)
		go func() {
			bgCtx := context.Background()
			delivered := b.waitForThawAndNudge(bgCtx, agent, text, beadID)
			if delivered {
				b.SetThreadStatus(bgCtx, ev.Channel, replyTS, fmt.Sprintf("Forwarded (tracking: %s)", b.beadLink(beadID, beadID)))
				return
			}
			// Wedged or fail-closed thaw: fall back so the message is never dropped.
			b.logger.Info("mention: thaw buffer did not deliver before deadline, falling through to fallback",
				"agent", agent, "bead", beadID, "thread_ts", ev.ThreadTimeStamp)
			if ev.ThreadTimeStamp != "" {
				b.respawnThreadAgent(bgCtx, ev.Channel, ev.ThreadTimeStamp, agent, text, ev.User)
				return
			}
			confirmText := fmt.Sprintf(":warning: Created task for agent but it is still resuming (tracking: %s). The agent will pick it up on its next cycle.", b.beadLink(beadID, fmt.Sprintf("`%s`", beadID)))
			_, _, _ = b.api.PostMessage(ev.Channel,
				slack.MsgOptionText(confirmText, false),
				slack.MsgOptionTS(replyTS),
			)
			b.restoreThreadStatus(bgCtx, ev.Channel, replyTS)
		}()
		// AI-container thread title/status cleanup below still applies; return
		// after it so we don't also run the non-buffered nudge paths.
		if ev.AssistantThread != nil && replyTS != "" {
			b.clearAssistantStatus(ctx, ev.Channel, replyTS)
			b.setAssistantThreadTitle(ctx, ev.Channel, replyTS,
				fmt.Sprintf("Chat with %s", agent))
		}
		return
	}

	// Nudge the agent: an interrupt mail its subscriber hands to coop. A
	// spawning agent's mail waits for the subscriber, which drains unread mail
	// when it starts; the nudge runs in a goroutine so a slow daemon does not
	// block the Slack event loop. Show an immediate status update to the user.
	if agentSpawning {
		statusText := fmt.Sprintf("Starting up... (tracking: %s)", b.beadLink(beadID, beadID))
		b.SetThreadStatus(ctx, ev.Channel, replyTS, statusText)
		go func() {
			bgCtx := context.Background()
			mailID, err := b.nudgeAgentForMentionMail(bgCtx, agent, text, beadID)
			if err != nil {
				// Terminal or lame-duck (stop_requested) agent in a THREAD →
				// session-resume respawn so the message isn't lost. The respawn
				// re-injects it as the trigger text (ka-0000002949).
				if errors.Is(err, ErrAgentNotNudgeable) && ev.ThreadTimeStamp != "" {
					b.logger.Info("mention: pod-gone nudge failure for thread agent, respawning with session resume",
						"agent", agent, "bead", beadID, "thread_ts", ev.ThreadTimeStamp, "error", err)
					b.respawnThreadForGoneNudge(bgCtx, ev.Channel, ev.ThreadTimeStamp, agent, text, ev.User)
					return
				}
				b.logger.Error("background mention nudge failed",
					"agent", agent, "bead", beadID, "error", err)
				// Without a line the thread is left on "Starting up..." forever.
				_, _, _ = b.api.PostMessage(ev.Channel,
					slack.MsgOptionText(fmt.Sprintf("%s %s did not start in time to take your message (tracking: %s). It will pick it up on its next cycle.",
						b.themeFor(bgCtx, ev.Channel).Semantic(SemWarn), agent, b.beadLink(beadID, fmt.Sprintf("`%s`", beadID))), false),
					slack.MsgOptionTS(replyTS),
				)
				b.restoreThreadStatus(bgCtx, ev.Channel, replyTS)
			} else {
				b.SetThreadStatus(bgCtx, ev.Channel, replyTS, fmt.Sprintf("Forwarded (tracking: %s)", b.beadLink(beadID, beadID)))
				if threadOwned && ev.ThreadTimeStamp != "" {
					go b.watchMailDelivery(bgCtx, mailID, agent, ev.Channel, ev.ThreadTimeStamp, text, ev.User)
				}
			}
		}()
	} else {
		mailID, nudgeErr := b.nudgeAgentForMentionMail(ctx, agent, text, beadID)

		// Show forwarding confirmation. When the agent has an active stream,
		// use the thread status bar instead of posting a chat message.
		if nudgeErr != nil {
			// Terminal or lame-duck agent in a thread: session-resume respawn rather than
			// a misleading "next cycle" reply (ka-0000002949). A pod gone while its bead
			// reads working is caught by watchMailDelivery.
			if errors.Is(nudgeErr, ErrAgentNotNudgeable) && ev.ThreadTimeStamp != "" {
				b.logger.Info("mention: pod-gone nudge failure for thread agent, respawning with session resume",
					"agent", agent, "bead", beadID, "thread_ts", ev.ThreadTimeStamp, "error", nudgeErr)
				b.respawnThreadForGoneNudge(ctx, ev.Channel, ev.ThreadTimeStamp, agent, text, ev.User)
				return
			}
			// Other failures, or outside a thread (no resume anchor): post visibly. No
			// coopmux link here; it belongs only on the agent card (ka-0000007955).
			confirmText := fmt.Sprintf(":warning: Created task for agent but nudge failed (tracking: %s). The agent will pick it up on its next cycle.", b.beadLink(beadID, fmt.Sprintf("`%s`", beadID)))
			_, _, _ = b.api.PostMessage(ev.Channel,
				slack.MsgOptionText(confirmText, false),
				slack.MsgOptionTS(replyTS),
			)
			// Restore thread status — PostMessage clears the assistant status bar.
			b.restoreThreadStatus(ctx, ev.Channel, replyTS)
		} else {
			// Show forwarding status in the thread status bar.
			statusText := fmt.Sprintf("Forwarded (tracking: %s)", b.beadLink(beadID, beadID))
			b.SetThreadStatus(ctx, ev.Channel, replyTS, statusText)
			if threadOwned && ev.ThreadTimeStamp != "" {
				go b.watchMailDelivery(context.Background(), mailID, agent, ev.Channel, ev.ThreadTimeStamp, text, ev.User)
			}
		}
	}

	// Clear assistant thread loading status and set a descriptive title
	// if this message originated from an AI container thread.
	if ev.AssistantThread != nil && replyTS != "" {
		b.clearAssistantStatus(ctx, ev.Channel, replyTS)
		b.setAssistantThreadTitle(ctx, ev.Channel, replyTS,
			fmt.Sprintf("Chat with %s", agent))
	}
}

// getAgentByThread reverse-maps (channel, thread_ts) to an agent identity
// by checking the threadAgents map, agentCards hot cache, persisted state,
// and finally the daemon (for agents with slack_thread_ts metadata).
// Used for @mention routing where all thread types apply.
func (b *Bot) getAgentByThread(channelID, threadTS string) string {
	// Check direct thread→agent mapping first (thread-spawned agents).
	if b.state != nil {
		if agent, ok := b.state.GetThreadAgent(channelID, threadTS); ok {
			if b.logger != nil {
				b.logger.Debug("getAgentByThread: found via thread-agent state",
					"channel", channelID, "thread_ts", threadTS, "agent", agent)
			}
			return agent
		}
	}

	// Check agent cards (hot cache + persisted state).
	if agent := b.findAgentCardByThread(channelID, threadTS); agent != "" {
		if b.logger != nil {
			b.logger.Debug("getAgentByThread: found via agent card",
				"channel", channelID, "thread_ts", threadTS, "agent", agent)
		}
		return agent
	}

	// Final fallback: daemon agent beads with matching slack_thread_ts metadata,
	// for mappings lost to cache pruning or a bridge restart.
	agent := b.findAgentByThreadMetadata(channelID, threadTS)
	if agent != "" {
		if b.logger != nil {
			b.logger.Debug("getAgentByThread: found via daemon metadata query",
				"channel", channelID, "thread_ts", threadTS, "agent", agent)
		}
		// Restore the mapping in state so subsequent lookups are fast.
		if b.state != nil {
			_ = b.state.SetThreadAgent(channelID, threadTS, agent)
		}
	}
	return agent
}

// findAgentByThreadMetadata queries the daemon for agent beads whose
// slack_thread_channel and slack_thread_ts metadata match the given thread.
// Returns the agent name or "" if no match is found.
func (b *Bot) findAgentByThreadMetadata(channelID, threadTS string) string {
	if b.daemon == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agents, err := b.daemon.ListAgentBeads(ctx)
	if err != nil {
		return ""
	}
	for _, a := range agents {
		if a.Metadata["slack_thread_channel"] == channelID && a.Metadata["slack_thread_ts"] == threadTS {
			// Skip terminal-state agents — they should be respawned, not resumed.
			if a.AgentState == "done" || a.AgentState == "failed" {
				continue
			}
			return extractAgentName(a.AgentName)
		}
	}
	return ""
}

// findThreadHeadByCardTS returns the wish head bead whose slack_card_ts matches
// (channel, ts), or "". Used by the 1b-11 done-reaction close gesture; unlike
// the agentCards cache and findAgentByThreadMetadata it survives restarts and
// terminal agents (ref persisted by persistHeadCardRef, indexed by migration
// 038). Filters to threadKindLabel so the agent bead's identical slack_card_ts
// never shadows the head.
func (b *Bot) findThreadHeadByCardTS(ctx context.Context, channelID, cardTS string) string {
	if b.daemon == nil || cardTS == "" {
		return ""
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{threadKindLabel},
		Fields: map[string]string{"slack_card_ts": cardTS},
		Limit:  2,
	})
	if err != nil || res == nil {
		return ""
	}
	for _, head := range res.Beads {
		// Skip a cross-channel ts collision; an empty stored channel (stamped before
		// the channel was known) matches.
		if channelID != "" && head.Fields["slack_card_channel"] != "" &&
			head.Fields["slack_card_channel"] != channelID {
			continue
		}
		return head.ID
	}
	return ""
}

// getThreadSpawnedAgent returns the agent bound to a thread only if it was
// explicitly spawned for that thread (via SetThreadAgent). Unlike getAgentByThread,
// this does NOT check agent card threads — those are status/notification threads
// where casual replies should not be auto-forwarded without an @mention.
func (b *Bot) getThreadSpawnedAgent(channelID, threadTS string) string {
	if b.state != nil {
		if agent, ok := b.state.GetThreadAgent(channelID, threadTS); ok {
			return agent
		}
	}
	return ""
}

// getCardAgentByThread returns the agent whose status card parents the thread,
// checking only agent cards (not thread-spawned agents). Enforces exclusive
// card-thread routing.
func (b *Bot) getCardAgentByThread(channelID, threadTS string) string {
	return b.findAgentCardByThread(channelID, threadTS)
}

// findAgentCardByThread searches the agentCards hot cache and persisted state
// for an agent whose card matches the given channel and thread timestamp.
// Match on both ref.Timestamp (card is the thread parent) and ref.ThreadTS
// (card posted inside a thread — e.g. thread-bound agents).
func (b *Bot) findAgentCardByThread(channelID, threadTS string) string {
	// Check agentCards hot cache.
	b.mu.Lock()
	for agent, ref := range b.agentCards {
		if ref.ChannelID == channelID && (ref.Timestamp == threadTS || ref.ThreadTS == threadTS) {
			b.mu.Unlock()
			return agent
		}
	}
	b.mu.Unlock()

	// Fall back to persisted agentCards state.
	if b.state != nil {
		for agent, ref := range b.state.AllAgentCards() {
			if ref.ChannelID == channelID && (ref.Timestamp == threadTS || ref.ThreadTS == threadTS) {
				return agent
			}
		}
	}
	return ""
}

// resolveAgentFromText checks if the first word of text matches a known active
// agent name. Matching is case-insensitive and supports both bare names
// ("crew-k8s") and project-qualified names ("gardenio/crew/k8s").
// Returns the matched agent identity and the remaining text (with agent name
// stripped), or ("", text) if no match.
func (b *Bot) resolveAgentFromText(ctx context.Context, text string) (string, string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return "", text
	}
	candidate := words[0]

	agents, err := b.daemon.ListAgentBeads(ctx)
	if err != nil {
		b.logger.Debug("failed to list agents for name resolution", "error", err)
		return "", text
	}

	candidateLower := strings.ToLower(candidate)
	for _, a := range agents {
		// Match against the short agent name (e.g., "crew-k8s").
		if strings.EqualFold(a.AgentName, candidateLower) {
			remaining := strings.TrimSpace(strings.TrimPrefix(text, candidate))
			return a.Title, remaining
		}
		// Match against the full title/identity (e.g., "crew-gardenio-crew-k8s").
		if strings.EqualFold(a.Title, candidateLower) {
			remaining := strings.TrimSpace(strings.TrimPrefix(text, candidate))
			return a.Title, remaining
		}
	}
	return "", text
}

// parseListenFlag strips a --listen flag from text, reporting whether it was
// present. On a thread spawn it makes the agent receive all thread replies.
func parseListenFlag(text string) (bool, string) {
	words := strings.Fields(text)
	filtered := make([]string, 0, len(words))
	found := false
	for _, w := range words {
		if w == "--listen" {
			found = true
			continue
		}
		filtered = append(filtered, w)
	}
	if found {
		return true, strings.Join(filtered, " ")
	}
	return false, text
}

// parseFreshFlag strips a --fresh flag (any position) from text, reporting
// whether it was present. --fresh continues the Slack thread with a clean
// conversation (no coop --resume, transcript graft or thaw). Only the respawn
// path (respawnThreadAgent) acts on it; first-spawn and live-agent paths just
// strip it.
func parseFreshFlag(text string) (bool, string) {
	words := strings.Fields(text)
	filtered := make([]string, 0, len(words))
	found := false
	for _, w := range words {
		if w == "--fresh" {
			found = true
			continue
		}
		filtered = append(filtered, w)
	}
	if found {
		return true, strings.Join(filtered, " ")
	}
	return false, text
}

// parseQuietFlag strips a --quiet flag (any position) from text, reporting
// whether it was present. On the Jira mention first-spawn path
// (jira_mentions.go) it stamps jira_quiet=true so automatic progress notes
// (spawn-ack, squawk-forward, completion-ack) skip the ticket; an explicit
// `gb jira reply` is unaffected.
func parseQuietFlag(text string) (bool, string) {
	words := strings.Fields(text)
	filtered := make([]string, 0, len(words))
	found := false
	for _, w := range words {
		if w == "--quiet" {
			found = true
			continue
		}
		filtered = append(filtered, w)
	}
	if found {
		return true, strings.Join(filtered, " ")
	}
	return false, text
}

// parseUltracodeFlag strips a --ultracode override (any position) from text,
// returning the canonical value, any unrecognized raw token (for a warning),
// and the remaining text:
//   - "--ultracode" / "--ultracode=on" → ("on", "", stripped)
//   - "--ultracode=off"                → ("off", "", stripped)
//   - "--ultracode=maybe"              → ("", "maybe", stripped)
//
// An absent flag yields ("", "", text), distinct from "off", so a respawn's
// prior "off" is never clobbered by a no-flag wake-up.
func parseUltracodeFlag(text string) (value, unrecognized, remaining string) {
	words := strings.Fields(text)
	filtered := make([]string, 0, len(words))
	for _, w := range words {
		if w == "--ultracode" {
			value = "on"
			continue
		}
		if v, ok := strings.CutPrefix(w, "--ultracode="); ok {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "on", "true", "1":
				value = "on"
			case "off", "false", "0":
				value = "off"
			default:
				unrecognized = v
			}
			continue
		}
		filtered = append(filtered, w)
	}
	if value == "" && unrecognized == "" {
		return "", "", text
	}
	return value, unrecognized, strings.Join(filtered, " ")
}

// parseBackendFlag strips a --backend=<target> override (any position) from
// text, returning a podmanager.ExecutionTarget* constant (read by
// resolveExecutionTarget in podspec.go), any unrecognized raw token, and the
// remaining text. microvm/lambda/firecracker map to ExecutionTargetLambdaMicroVM,
// kubernetes/k8s/pod to ExecutionTargetKubernetes. An absent flag yields
// ("", "", text), distinct from "kubernetes", so a respawn's prior
// lambda_microvm is never clobbered by a no-flag wake-up.
func parseBackendFlag(text string) (target, unrecognized, remaining string) {
	words := strings.Fields(text)
	filtered := make([]string, 0, len(words))
	for _, w := range words {
		v, ok := strings.CutPrefix(w, "--backend=")
		if !ok {
			filtered = append(filtered, w)
			continue
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "microvm", "lambda", "lambda_microvm", "lambda-microvm", "firecracker", "fc":
			target = podmanager.ExecutionTargetLambdaMicroVM
		case "kubernetes", "k8s", "pod", "kube":
			target = podmanager.ExecutionTargetKubernetes
		case "":
			// bare "--backend=" with no value — leave as prose, warn.
			unrecognized = w
		default:
			unrecognized = v
		}
	}
	if target == "" && unrecognized == "" {
		return "", "", text
	}
	return target, unrecognized, strings.Join(filtered, " ")
}

// backendLabel renders a human-friendly name for an execution_target constant,
// used in operator-facing confirmation/warning messages.
func backendLabel(target string) string {
	switch target {
	case podmanager.ExecutionTargetLambdaMicroVM:
		return "Lambda MicroVM"
	case podmanager.ExecutionTargetKubernetes:
		return "Kubernetes pod"
	default:
		return target
	}
}

// parseProjectOverride extracts a project override from mention text.
// Supports three syntaxes:
//   - "project:gardenio fix the helm chart" → ("gardenio", "fix the helm chart")
//   - "--project gardenio fix the helm chart" → ("gardenio", "fix the helm chart")
//   - "--project=gardenio fix the helm chart" → ("gardenio", "fix the helm chart")
//
// Returns ("", text) if no override is found.
func parseProjectOverride(text string) (string, string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return "", text
	}

	// Syntax 1: project:<name>
	if project, ok := strings.CutPrefix(words[0], "project:"); ok && project != "" {
		remaining := strings.TrimSpace(strings.Join(words[1:], " "))
		return project, remaining
	}

	// Syntax 2: --project=<name>
	if project, ok := strings.CutPrefix(words[0], "--project="); ok && project != "" {
		remaining := strings.TrimSpace(strings.Join(words[1:], " "))
		return project, remaining
	}

	// Syntax 3: --project <name>
	if words[0] == "--project" && len(words) >= 2 {
		project := words[1]
		remaining := strings.TrimSpace(strings.Join(words[2:], " "))
		return project, remaining
	}

	return "", text
}

// parseRoleOverride extracts a leading role override from mention text, like
// parseProjectOverride:
//   - "role:qa fix the flaky test" → ("qa", "fix the flaky test")
//   - "--role qa fix the flaky test" → ("qa", "fix the flaky test")
//   - "--role=qa fix the flaky test" → ("qa", "fix the flaky test")
//
// A parsed role overrides the channel_roles default. Empty or malformed values
// ("role:", a bare "--role") leave the text unchanged and return ("", text).
func parseRoleOverride(text string) (string, string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return "", text
	}

	// Syntax 1: role:<name>
	if role, ok := strings.CutPrefix(words[0], "role:"); ok && role != "" {
		remaining := strings.TrimSpace(strings.Join(words[1:], " "))
		return role, remaining
	}

	// Syntax 2: --role=<name>
	if role, ok := strings.CutPrefix(words[0], "--role="); ok && role != "" {
		remaining := strings.TrimSpace(strings.Join(words[1:], " "))
		return role, remaining
	}

	// Syntax 3: --role <name>
	if words[0] == "--role" && len(words) >= 2 {
		role := words[1]
		remaining := strings.TrimSpace(strings.Join(words[2:], " "))
		return role, remaining
	}

	return "", text
}

// parseModelOverride extracts a leading model override from the (already
// project/role-stripped) mention text, like parseRoleOverride:
//
//   - "model:opus fix the flaky test"   → ("opus", "fix the flaky test")
//   - "--model opus fix the flaky test" → ("opus", "fix the flaky test")
//   - "--model=opus fix the flaky test" → ("opus", "fix the flaky test")
//
// The token is not validated here; callers resolve it via normalizeModel
// (bot_mentions_model.go). Empty or malformed values return ("", text).
func parseModelOverride(text string) (string, string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return "", text
	}

	// Syntax 1: model:<name>
	if model, ok := strings.CutPrefix(words[0], "model:"); ok && model != "" {
		remaining := strings.TrimSpace(strings.Join(words[1:], " "))
		return model, remaining
	}

	// Syntax 2: --model=<name>
	if model, ok := strings.CutPrefix(words[0], "--model="); ok && model != "" {
		remaining := strings.TrimSpace(strings.Join(words[1:], " "))
		return model, remaining
	}

	// Syntax 3: --model <name>
	if words[0] == "--model" && len(words) >= 2 {
		model := words[1]
		remaining := strings.TrimSpace(strings.Join(words[2:], " "))
		return model, remaining
	}

	return "", text
}

// stripLeadingOverrideFlags removes spawn-only override flags (--listen,
// --fresh, --project, --role, --model/--harness, --ultracode, --backend) from
// text addressed to an already-running agent, returning the cleaned prompt and
// the raw --model (or --harness) token, else "". A live pod cannot hot-swap
// them, so they would only leak into the prompt (ka-0000001727). Uses the same
// parser sequence as handleThreadSpawn, so a flag buried mid-prompt stays prose.
func stripLeadingOverrideFlags(text string) (stripped, rawModel string) {
	_, text = parseListenFlag(text)
	_, text = parseFreshFlag(text)
	_, text = parseProjectOverride(text)
	_, text = parseRoleOverride(text)
	var rawHarness string
	rawModel, rawHarness, text = parseModelHarnessFlags(text)
	if rawModel == "" {
		rawModel = rawHarness // surface a bare --harness in the can't-hot-swap note
	}
	_, _, text = parseUltracodeFlag(text)
	_, _, text = parseBackendFlag(text)
	return text, rawModel
}

// noteModelSwitchUnsupported tells the operator that a --model sent to a
// running agent was ignored (ANTHROPIC_MODEL is baked at spawn) and how to
// switch: kill + re-mention, or a new thread. Visible reply in assistant
// threads (no ephemerals there), ephemeral elsewhere.
func (b *Bot) noteModelSwitchUnsupported(ev *slackevents.AppMentionEvent, replyTS, agent, rawModel string) {
	if b.api == nil {
		return
	}
	msg := fmt.Sprintf(
		":information_source: *%s* is already running — I can't switch its model. `ANTHROPIC_MODEL` is baked into the pod when it spawns, so `--model %s` was ignored (I dropped it from your message). To run on a different model, `@gardenio kill` this agent then mention it again with `--model %s`, or start a new thread with the flag.",
		agent, rawModel, rawModel)
	if ev.AssistantThread != nil {
		_, _, _ = b.api.PostMessage(ev.Channel,
			slack.MsgOptionText(msg, false),
			slack.MsgOptionTS(replyTS))
		return
	}
	_, _ = b.api.PostEphemeral(ev.Channel, ev.User,
		slack.MsgOptionText(msg, false))
}

// noteFreshUnsupportedForLiveAgent tells the operator that --fresh cannot wipe
// a running agent's conversation and to kill + re-mention with --fresh instead.
// Surfaces like noteModelSwitchUnsupported.
func (b *Bot) noteFreshUnsupportedForLiveAgent(ev *slackevents.AppMentionEvent, replyTS, agent string) {
	if b.api == nil {
		return
	}
	msg := fmt.Sprintf(
		":information_source: *%s* is already running — I can't wipe its conversation mid-session (its context lives in the running pod). `--fresh` was ignored (I dropped it from your message). To start a clean conversation in this thread, `@gardenio kill` this agent first, then mention it again with `--fresh`.",
		agent)
	if ev.AssistantThread != nil {
		_, _, _ = b.api.PostMessage(ev.Channel,
			slack.MsgOptionText(msg, false),
			slack.MsgOptionTS(replyTS))
		return
	}
	_, _ = b.api.PostEphemeral(ev.Channel, ev.User,
		slack.MsgOptionText(msg, false))
}

// handleThreadSpawn spawns an ephemeral agent bound to a Slack thread when
// @gardenio is mentioned in a thread with no existing agent binding.
func (b *Bot) handleThreadSpawn(ctx context.Context, ev *slackevents.AppMentionEvent, text string) {
	b.handleThreadSpawnAt(ctx, b.slackThreadAt(ev.Channel, ev.ThreadTimeStamp), ev, text)
}

// handleThreadSpawnAt is handleThreadSpawn for any thread: a Slack thread, or
// a Commons thread with no Slack copy, known by its head alone (slice 4d). ev
// carries the asker's Slack user, when there is one. Without a Slack copy
// nothing is posted, streamed or set on a status bar, the notes go to at.Say,
// the thread's context is read from its beads, the agent spawns in the head's
// land (a --project naming another is refused), and the channel's role and
// RuntimeClass come from its channel bead.
func (b *Bot) handleThreadSpawnAt(ctx context.Context, at threadAt, ev *slackevents.AppMentionEvent, text string) {
	channel, threadTS := at.Channel, at.TS

	// Reject malformed thread bindings early — before touching any shared state.
	if !at.slack() && at.Head == "" {
		b.logger.Warn("thread-spawn: a Commons thread names no head, skipping")
		return
	}
	if at.slack() && !isValidThreadBinding(channel, threadTS) {
		b.logger.Warn("thread-spawn: invalid thread binding, skipping",
			"channel", channel, "thread_ts", threadTS)
		return
	}

	// Card threads belong exclusively to their card agent; never spawn a second.
	if cardAgent := b.getCardAgentByThread(channel, threadTS); at.slack() && cardAgent != "" {
		b.logger.Info("thread-spawn: blocked — thread belongs to agent card",
			"channel", channel, "thread_ts", threadTS, "card_agent", cardAgent)
		b.threadNote(at, fmt.Sprintf(":information_source: This thread belongs to *%s*'s card. Mention me to talk to that agent.",
			extractAgentName(cardAgent)))
		return
	}

	// Atomic guard: check both completed spawns (state) and in-flight spawns
	// under a single lock to close the race window where two concurrent
	// mentions both pass the state check before either sets spawnInFlight.
	spawnKey := at.key()
	b.mu.Lock()
	if b.spawnInFlight[spawnKey] {
		b.mu.Unlock()
		b.logger.Info("thread-spawn: spawn already in flight, skipping",
			"channel", channel, "thread_ts", threadTS)
		return
	}
	if b.state != nil {
		if agent, ok := b.threadAgentAt(at); ok {
			b.mu.Unlock()
			b.logger.Info("thread-spawn: agent already bound to thread, skipping",
				"channel", channel, "thread_ts", threadTS, "agent", agent)
			b.threadNote(at, fmt.Sprintf(":information_source: An agent (*%s*) is already working in this thread.", extractAgentName(agent)))
			return
		}
	}
	b.spawnInFlight[spawnKey] = true
	b.mu.Unlock()
	defer func() {
		time.AfterFunc(10*time.Second, func() {
			b.mu.Lock()
			delete(b.spawnInFlight, spawnKey)
			b.mu.Unlock()
		})
	}()

	// Check for --listen flag (auto-forward all thread replies without @mention).
	listen, text := parseListenFlag(text)
	listen = listen && at.slack() // --listen is Slack's: every Commons reply reaches the agent

	// --fresh is a no-op on first spawn (no prior transcript); strip it so it
	// never leaks into the agent's context.
	_, text = parseFreshFlag(text)

	// Check for explicit project override in the mention text.
	// Supports "project:<name>" and "--project <name>" syntax.
	projectOverride, text := parseProjectOverride(text)

	// Role override (role:<name>, --role <name>, --role=<name>) replaces the
	// channel_roles default below.
	roleOverride, text := parseRoleOverride(text)

	// --runner <hostname> places the thread on that local runner (spec §7).
	runnerFlag, text := parseRunnerFlag(text)

	// Model override (model:<name>, --model <name>, --model=<name>) resolves via
	// normalizeModel so only allowlisted models are selected; it is stamped into
	// fields["model"] for podspec's applyAgentModelOverride. An unrecognized value
	// is reported and the spawn uses the default.
	//
	// --harness codex|claude (or --model codex) picks the agent CLI; Claude aliases
	// and Codex models are mutually exclusive (resolveHarnessModel, ADR 0004
	// section 6), and a harness the role does not allow is refused below.
	rawModelOverride, rawHarnessOverride, text := parseModelHarnessFlags(text)
	modelOverride := ""
	harnessOverride := ""
	if rawModelOverride != "" || rawHarnessOverride != "" {
		sel, unrecognized, errMsg := resolveHarnessModel(rawHarnessOverride, rawModelOverride)
		switch {
		case errMsg != "":
			b.logger.Warn("thread-spawn: rejected harness/model override",
				"channel", channel, "thread_ts", threadTS,
				"harness", rawHarnessOverride, "model", rawModelOverride, "reason", errMsg)
			b.threadNote(at, b.themeFor(ctx, channel).Semantic(SemBlockedBy)+" "+errMsg+" Not spawning — re-mention with a valid override.")
			return
		case unrecognized:
			b.logger.Warn("thread-spawn: unrecognized model override, using default",
				"channel", channel, "thread_ts", threadTS, "raw", rawModelOverride)
			b.threadNote(at, fmt.Sprintf(
				":warning: Unrecognized model %q — spawning on the default model instead. Valid options: %s.",
				rawModelOverride, allowedModelHint()))
			harnessOverride = sel.Harness
		default:
			modelOverride, harnessOverride = sel.Model, sel.Harness
			b.logger.Info("thread-spawn: model/harness override from mention text",
				"channel", channel, "thread_ts", threadTS,
				"raw_model", rawModelOverride, "model", modelOverride, "harness", harnessOverride)
		}
	}

	// --ultracode=on|off (bare == on) is stamped into fields["ultracode"] and
	// reaches the pod as BOAT_ULTRACODE (podspec applyUltracode, overriding
	// project/role config). An unrecognized value is warned and dropped.
	ultracodeOverride, ultracodeBad, text := parseUltracodeFlag(text)
	if ultracodeBad != "" {
		b.logger.Warn("thread-spawn: unrecognized ultracode value, ignoring",
			"channel", channel, "thread_ts", threadTS, "raw", ultracodeBad)
		b.threadNote(at, fmt.Sprintf(
			":warning: Unrecognized `--ultracode=%s` — use `--ultracode=on` or `--ultracode=off`. Spawning with the default ultracode setting.",
			ultracodeBad))
	}

	// --backend=microvm|kubernetes is stamped into fields["execution_target"]
	// (read by resolveExecutionTarget), which persists so resumes keep the
	// backend. An unrecognized value is warned and dropped.
	backendOverride, backendBad, text := parseBackendFlag(text)
	if backendBad != "" {
		b.logger.Warn("thread-spawn: unrecognized backend value, ignoring",
			"channel", channel, "thread_ts", threadTS, "raw", backendBad)
		b.threadNote(at, fmt.Sprintf(
			":warning: Unrecognized `--backend=%s` — use `--backend=microvm` or `--backend=kubernetes`. Spawning on the default backend instead.",
			backendBad))
	}
	// MicroVM implies ultracode=on (such agents should use ultracode, workflows
	// and subagents) unless --ultracode=off was explicit.
	if backendOverride == podmanager.ExecutionTargetLambdaMicroVM && ultracodeOverride == "" {
		ultracodeOverride = "on"
		b.logger.Info("thread-spawn: --backend=microvm implies --ultracode=on",
			"channel", channel, "thread_ts", threadTS)
	}

	// Fetch thread context from Slack. rootAuthor is the thread root's author,
	// used to detect user-owned threads for the thread-cards feature (UX Q4).
	var threadContext, rootAuthor string
	if at.slack() {
		threadContext, rootAuthor = b.fetchThreadContext(ctx, channel, threadTS)
	} else {
		threadContext = b.commonsThreadContext(ctx, at.Head, at.Land)
	}

	// Generate a unique agent name based on the thread timestamp (a Commons
	// thread's head, when it has no Slack copy).
	agentName := "thread-" + sanitizeTS(threadTS)
	if !at.slack() {
		agentName = commonsThreadAgentName(at.Head)
	}

	// Use explicit project override if provided, otherwise infer from channel.
	// A Commons thread's agent works in the thread's land, and nowhere else
	// (section 6.2, rule 4).
	project := projectOverride
	if at.commons() {
		if projectOverride != "" && projectOverride != at.Land {
			b.threadNote(at, fmt.Sprintf("An agent of this thread works in %s; --project %s is not allowed here. Not spawning.", at.Land, projectOverride))
			return
		}
		project = at.Land
	}
	// miss is why the channel named no project, told to the person below when
	// nothing else resolves one either.
	var miss channelProjectMiss
	if project == "" {
		project, miss = b.projectForChannel(ctx, channel)
	}
	b.logger.Info("thread-spawn: project resolution",
		"channel", channel, "project", project, "override", projectOverride)
	if project == "" && b.router != nil {
		if mapped := b.router.GetAgentByChannel(channel); mapped != "" {
			project = projectFromAgentIdentity(mapped)
		}
	}

	// The agent surface (DM) has no channel→project mapping, so use the channel
	// the user was viewing next to the pane (cached by app_home_opened /
	// assistant_thread_started): legacy assistant_view keys it by (assistant
	// channel, thread_ts), an agent_view DM by (DM channel, ""). viewedChannel
	// also enriches the description below (ka-0000000778).
	var viewedChannel string
	if ev.AssistantThread != nil || strings.HasPrefix(channel, "D") {
		lookupTS := threadTS
		if ev.AssistantThread == nil && strings.HasPrefix(channel, "D") {
			lookupTS = "" // agent_view app_home_opened cache key
		}
		viewedChannel = b.getAssistantCtxChannel(channel, lookupTS)
	}
	if project == "" && viewedChannel != "" {
		project = b.projectFromChannel(ctx, viewedChannel)
		if project != "" {
			b.logger.Info("thread-spawn: resolved project from agent context channel",
				"project", project, "context_channel", viewedChannel, "channel", channel)
		}
	}

	// Final fallback for assistant threads and DMs (never in project mappings):
	// the sole project, or the configured default DM project.
	if project == "" && (ev.AssistantThread != nil || strings.HasPrefix(channel, "D")) {
		project = b.resolveDMProject(ctx, channel, ev.User, "thread-spawn")
	}

	// Reject thread spawns with no project — agents must have a project
	// so they get the correct config, secrets, and repos.
	if project == "" {
		b.logger.Warn("thread-spawn: rejected — no project resolved",
			"channel", channel, "thread_ts", threadTS)
		// A conflict or a daemon error is not "no project is mapped": say what
		// went wrong instead (gd-ByBchQbx8O).
		if reason := miss.reason(); reason != "" {
			b.threadNote(at, b.themeFor(ctx, channel).Semantic(SemFail)+" Cannot spawn agent — "+reason)
			return
		}
		{
			msg := ":x: Cannot spawn agent — no project is mapped to this channel."
			if strings.HasPrefix(channel, "D") {
				msg += "\n:bulb: To enable DM spawning, set `dm_default=true` on a project bead or configure `DEFAULT_DM_PROJECT` on the bridge."
			} else {
				msg += " Use `--project <name>` to specify one."
			}
			if projects, err := b.daemon.ListProjectBeads(ctx); err == nil && len(projects) > 0 {
				names := make([]string, 0, len(projects))
				for name := range projects {
					names = append(names, name)
				}
				msg += fmt.Sprintf("\nAvailable projects: %s", strings.Join(names, ", "))
			}
			b.threadNote(at, msg)
		}
		return
	}

	// An explicit --project=X that names no project bead rejects the spawn.
	if projectOverride != "" {
		projects, err := b.daemon.ListProjectBeads(ctx)
		if err == nil {
			if _, ok := projects[projectOverride]; !ok {
				names := make([]string, 0, len(projects))
				for name := range projects {
					names = append(names, name)
				}
				b.logger.Warn("thread-spawn: explicit project override not found, rejecting",
					"project", projectOverride, "available", names)
				b.threadNote(at, fmt.Sprintf(":x: Project %q not found. Available projects: %s",
					projectOverride, strings.Join(names, ", ")))
				return
			}
		}
	}

	// Resolve-or-create the thread bead before the agent bead, so derivations can
	// hang typed-mention edges off it. The slack-thread:{channel}:{ts} label is the
	// restart-safe race guard (gardenio/plans/slack-thread-intake-bead.md §S1).
	threadBeadID, threadBeadErr := at.Head, error(nil)
	if at.slack() {
		threadBeadID, _, threadBeadErr = b.getOrCreateThreadBead(ctx, project, threadBeadFields{
			Channel:       channel,
			ThreadTS:      threadTS,
			SpawnChannel:  channel,
			OpenerUserID:  ev.User,
			RootText:      text,
			RootAtSlackTS: threadTS,
		})
	}
	if threadBeadErr == nil {
		at.Head = threadBeadID
	}
	if threadBeadErr != nil {
		// Non-fatal: log and proceed without a thread bead — agent spawn
		// remains functional even if the thread-bead create failed.
		b.logger.Warn("thread-spawn: resolve-or-create thread bead failed; proceeding without parent edge",
			"channel", channel, "thread_ts", threadTS, "error", threadBeadErr)
	}

	// Explicit placement (local-agents spec §7): --runner, or the owner
	// mentioning in a thread they opened, hands the thread to their laptop
	// agent instead of a pod. Capacity never chooses a runner.
	if b.placeThreadOnRunner(ctx, at, ev.User, rootAuthor, runnerFlag, text) {
		return
	}

	// Append the operator's display name + pronouns (from their user bead) so the
	// agent refers to them correctly even without `gb prime`. Omitted when the
	// Slack user has no user bead.
	operatorTag := ""
	if u := b.askerUser(ctx, at, ev.User); u != nil {
		{
			name := strings.TrimSpace(u.DisplayName)
			if name != "" {
				if p := strings.TrimSpace(u.Pronouns); p != "" {
					operatorTag = fmt.Sprintf("\nOperator: %s (%s) — use these pronouns when referring to them.", name, p)
				} else {
					operatorTag = fmt.Sprintf("\nOperator: %s", name)
				}
			}
		}
	}
	surfaceName := "Slack"
	if !at.slack() {
		surfaceName = "Commons"
	}
	description := fmt.Sprintf("Thread-spawned agent for "+surfaceName+" thread.\n\n"+
		"## Thread Context\n\n%s\n\n---\n"+
		"Triggered by: %s%s", threadContext, text, operatorTag)

	// From the agent pane, add the viewed channel's live context (name + the
	// project's recent open work) so the reply is grounded (ka-0000000778).
	if note := b.buildViewedChannelContextNote(ctx, viewedChannel); note != "" {
		description += "\n\n## Viewed Channel (live context)\n\n" + note
	}
	if threadBeadID != "" {
		// Typed-mention edge: agent bead becomes a child of the thread bead.
		// Mention syntax: "parent: <id>" — see typed-mentions-graph-edges-and-cli.md.
		description += "\n\nparent: " + threadBeadID
	}
	description = truncateText(description, 4000)

	// Resolve the role once so both the pool-assign path and the cold-start
	// fallback honor it: channel_roles override, else "thread" (gap G2 in
	// ka-0000003960), or in the Commons the land's default (commonsThreadRole).
	threadRole := "thread"
	commonsRole, commonsRuntimeClass := "", ""
	if !at.slack() {
		threadRole = b.commonsThreadRole(ctx, project)
		commonsRole, commonsRuntimeClass = b.commonsChannelSettings(ctx, at.ChannelID)
	}
	if commonsRole != "" {
		threadRole = commonsRole
	} else if project != "" && at.slack() {
		if cr := b.roleFromChannel(ctx, channel, project); cr != "" {
			threadRole = cr
		}
	}
	// An explicit --role wins over the default and channel_roles. Commas become
	// dashes (as podmanager.SanitizeRole does, inlined to keep this package free
	// of client-go) so pool selectors and pod labels stay label-safe.
	if roleOverride != "" {
		threadRole = strings.ReplaceAll(roleOverride, ",", "-")
		b.logger.Info("thread-spawn: role override from mention text",
			"channel", channel, "thread_ts", threadTS, "role", threadRole)
	}
	// A Commons spawn runs only a role of its land (landSpawnRoles). Checked
	// before the pool assign, since a pooled pod's role was baked at prewarm and
	// kbeads' spawn gate never sees it; commonsCompose already refuses a bad --role.
	if at.commons() {
		if roles := b.landSpawnRoles(ctx, project); !roles[threadRole] {
			b.logger.Warn("thread-spawn: role is not one the land runs, refusing",
				"head", at.Head, "land", project, "role", threadRole)
			b.threadNote(at, fmt.Sprintf("%s does not run the %s role. Ask for one of: %s. Not spawning.",
				project, threadRole, sortedRoles(roles)))
			return
		}
	}

	// A harness override must be allowed by the final role (its own harness or
	// allowed_harnesses): refuse here rather than fail later in the controller.
	if harnessOverride != "" {
		if ok, allowedSet := b.harnessAllowedForRole(ctx, project, threadRole, harnessOverride); !ok {
			msg := harnessRoleRefusal(threadRole, harnessOverride, allowedSet)
			b.logger.Warn("thread-spawn: harness override not allowed for role",
				"channel", channel, "thread_ts", threadTS, "role", threadRole, "harness", harnessOverride)
			b.threadNote(at, b.themeFor(ctx, channel).Semantic(SemBlockedBy)+" "+msg+" Not spawning.")
			return
		}
	}

	// Channel RuntimeClass override (e.g. gb-turbo → "runsc") gives gVisor
	// freeze/thaw hot resume (epic ka-0000003080). It is passed as the pool's
	// RequireRuntimeClass so a runsc channel warm-assigns only a runsc pod and
	// otherwise cold-mints, never grafting onto runc (ka-0000004563).
	channelRuntimeClass := commonsRuntimeClass
	if at.slack() {
		channelRuntimeClass = b.runtimeClassFromChannel(ctx, channel, project)
	}

	// Resolve the operator once so the pool assign stamps fields.operated_by
	// atomically with the state flip (ka-0000000528) and userFields reuses it.
	// "" makes AssignPrewarmed skip the stamp.
	assignOperator := ""
	if ev.User != "" {
		assignOperator = b.operatorFromSlackUser(ctx, ev.User)
	} else if at.Actor != "" {
		assignOperator = at.Actor
	}

	// Try a prewarmed pool agent first to avoid the ~60-120s cold start; a runsc
	// channel requires a runsc pooled agent (ka-0000004563). --model, --harness,
	// --ultracode and --backend=microvm skip the pool: a warm pod baked its model
	// and settings at create time and cannot become a MicroVM, so only the cold
	// mint, which stamps the fields, can honor them.
	beadID, assignedAgent := "", ""
	if modelOverride == "" && harnessOverride == "" && ultracodeOverride == "" && backendOverride != podmanager.ExecutionTargetLambdaMicroVM {
		// First mention: no transcript to resume, so resumeS3Key is "" (the warm-pool
		// coop-resume graft, ka-0000000983, is respawn-only).
		beadID, assignedAgent = b.tryPoolAssign(ctx, channel, threadTS, threadBeadID, description, project, threadRole, assignOperator, channelRuntimeClass, "")
	}
	if beadID != "" {
		b.logger.Info("thread-spawn: assigned prewarmed agent",
			"bead", beadID, "agent", assignedAgent,
			"channel", channel, "thread_ts", threadTS, "listen", listen)

		// Best-effort: store the Slack user, user bead and thread bead ids for
		// decision @mentions, per-user preferences and one-hop thread lookups.
		userFields := map[string]string{}
		if ev.User != "" {
			userFields["slack_user_id"] = ev.User
			userFields["operated_by"] = assignOperator
			if u := b.UserDirectory().LookupBySlackID(ctx, ev.User); u != nil {
				userFields["user_bead_id"] = u.BeadID
			}
		}
		// Warm-assign parity with cold start (G4, ka-0000001120): backfill
		// slack_spawn_channel (squawk/card routing), role (else the prewarm-baked role
		// wins) and slack_opener_user_id (threadCardOwned fails closed without it).
		if channel != "" {
			userFields["slack_spawn_channel"] = channel
		}
		if threadRole != "" {
			userFields["role"] = threadRole
		}
		// Stamp the owner only when the root author is known; an absent value keeps
		// threadCardOwned failing closed.
		if rootAuthor != "" {
			userFields["slack_opener_user_id"] = rootAuthor
		}
		// SetWishPointer stamps thread_id and its legacy alias slack_thread_bead_id
		// (no-op on an empty head), as the restart path does (bot_agent_kill.go). The
		// /threads index counts members by thread_id only (ka-0000007241).
		beadsapi.SetWishPointer(userFields, threadBeadID)
		b.commonsSpawnFields(ctx, at, ev.User, "commons-thread", userFields)
		if len(userFields) > 0 {
			_ = b.daemon.UpdateBeadFields(ctx, beadID, userFields)
		}

		// Lineage edge (auto-link M2d, epic ka-0000000933): promote the soft
		// `parent: <id>` mention to a first-class triggered-by dep on the thread bead.
		if threadBeadID != "" {
			if err := b.daemon.AddDependency(ctx, beadID, threadBeadID, "triggered-by", "slack-bridge"); err != nil {
				b.logger.Warn("thread-spawn: failed to add triggered-by dep (pool-assign)",
					"agent_bead", beadID, "thread_bead", threadBeadID, "error", err)
			}
		}

		if b.state != nil {
			b.bindThreadAgent(at, assignedAgent)
			if listen {
				_ = b.state.SetListenThread(channel, threadTS)
			}
		}
		// Fetch pod_name for the prewarmed agent so state transitions work.
		agentShort := extractAgentName(assignedAgent)
		b.fetchAndCachePodName(ctx, agentShort)

		// Start a streaming message with minimal initial text.
		// Status info goes to the assistant thread status bar, not the chat.
		if !at.slack() {
			// A Commons thread has no Slack stream or status bar: its card is the
			// agent's status.
		} else if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
			// Anchor TTFT on the triggering message's send-time so the first
			// agent message reports time-to-first-token. ka-0000001004.
			if t, ok := slackTSToTime(ev.TimeStamp); ok {
				b.streamMgr.MarkTriggerSent(agentShort, t)
			}
			streamTS := b.streamMgr.StartSession(ctx, agentShort, channel, threadTS, ev.User)
			if streamTS != "" {
				b.mu.Lock()
				b.threadSpawnMsgs[agentShort] = MessageRef{
					ChannelID: channel, Timestamp: streamTS, ThreadTS: threadTS,
				}
				b.mu.Unlock()
			}
			// Show status info in the assistant thread status bar.
			statusText := fmt.Sprintf("Starting up... (tracking: %s)", b.beadLink(beadID, beadID))
			b.SetThreadStatus(ctx, channel, threadTS, statusText)
		} else {
			// Fallback: use thread status bar when streaming is unavailable.
			// No persistent chat message — keeps the thread clean.
			statusText := fmt.Sprintf("Starting up... (tracking: %s)", b.beadLink(beadID, beadID))
			b.SetThreadStatus(ctx, channel, threadTS, statusText)
			// Track the thread reference so updateThreadSpawnWithLink can find it.
			b.mu.Lock()
			b.threadSpawnMsgs[agentShort] = MessageRef{
				ChannelID: channel, ThreadTS: threadTS,
			}
			b.mu.Unlock()
		}
		// Post the in-thread card: cold mints get it from NotifyAgentSpawn's created
		// event, but a warm assignment fires none (ka-0000000954).
		if at.slack() {
			b.postWarmThreadCard(ctx, agentShort, project, channel, threadTS, rootAuthor, ev.User)
		}

		// Clear assistant loading status and set title for AI container threads.
		if ev.AssistantThread != nil {
			b.clearAssistantStatus(ctx, channel, threadTS)
			b.setAssistantThreadTitle(ctx, channel, threadTS,
				fmt.Sprintf("Chat with %s", agentShort))
		}
		return
	}

	// Fallback: cold-start a new agent pod. threadRole is shared with the
	// pool-assign path above.
	fields := map[string]string{
		"agent":                agentName,
		"mode":                 "crew",
		"role":                 threadRole,
		"project":              project,
		"slack_thread_channel": channel,
		"slack_thread_ts":      threadTS,
		"slack_spawn_channel":  channel,
		"spawn_source":         "slack-thread",
		"slack_user_id":        ev.User,
		// Start in agent_state=spawning so a mention racing the pod boot takes the
		// boot-retry path, not a respawn that would close the booting bead as done
		// and orphan the pod (ka-0000000292 / ka-0000005658).
		"agent_state": "spawning",
	}
	// --model → fields["model"], baked by podspec.go's applyAgentModelOverride as
	// ANTHROPIC_MODEL/CLAUDE_MODEL (+ BOAT_MODEL_PINNED). BuildSpecFromBeadInfo
	// re-reads it on reconcile, so it persists across restarts.
	if modelOverride != "" {
		fields["model"] = modelOverride
	}
	// --harness → fields["harness"], read by podspec.go as RequestedHarness ahead
	// of role/project (ADR 0004 section 4). Persists like model.
	if harnessOverride != "" {
		fields["harness"] = harnessOverride
	}
	// --ultracode → fields["ultracode"], baked as BOAT_ULTRACODE by
	// applyUltracode. Persists like model; empty honors the project/role config.
	if ultracodeOverride != "" {
		fields["ultracode"] = ultracodeOverride
	}
	// --backend → fields["execution_target"] for resolveExecutionTarget. Persists
	// (BuildSpecFromBeadInfo re-reads it, the microvm lifecycle re-stamps it);
	// empty inherits the role/project default.
	if backendOverride != "" {
		fields["execution_target"] = backendOverride
	}
	// Pin the channel's RuntimeClass (e.g. gb-turbo → runsc) for gVisor
	// freeze/thaw; empty inherits role/project/cluster default (ka-0000003080).
	if channelRuntimeClass != "" {
		fields["runtime_class"] = channelRuntimeClass
	}
	// Root author for thread-card ownership (UX Q4): owned ⟺
	// slack_opener_user_id == slack_user_id. Set only when known, so
	// threadCardOwned fails closed otherwise.
	if rootAuthor != "" {
		fields["slack_opener_user_id"] = rootAuthor
	}
	if ev.User != "" {
		fields["operated_by"] = b.operatorFromSlackUser(ctx, ev.User)
	}
	// SetWishPointer: see the pool-assign branch above (ka-0000007241).
	beadsapi.SetWishPointer(fields, threadBeadID)
	b.commonsSpawnFields(ctx, at, ev.User, "commons-thread", fields)
	stampHumanOwner(fields)
	// Resolve user bead from Slack user ID for per-user preferences.
	if ev.User != "" {
		if u := b.UserDirectory().LookupBySlackID(ctx, ev.User); u != nil {
			fields["user_bead_id"] = u.BeadID
		}
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		b.logger.Error("failed to marshal agent fields", "error", err)
		return
	}

	labels := []string{"slack-thread"}
	if !at.slack() {
		labels = []string{commonsThreadLabel}
	}
	if project != "" {
		labels = append(labels, "project:"+project)
	}

	beadID, err = b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       agentName,
		Type:        "agent",
		Description: description,
		Fields:      json.RawMessage(fieldsJSON),
		Labels:      labels,
	})
	if err != nil {
		b.logger.Error("failed to create thread-spawned agent bead",
			"channel", channel, "thread_ts", threadTS, "error", err)
		b.threadNote(at, b.themeFor(ctx, channel).Semantic(SemFail)+" Cannot spawn agent — the beads daemon did not create its agent bead. Try again in a moment.")
		return
	}

	b.logger.Info("thread-spawn: created agent bead (cold-start)",
		"bead", beadID, "agent", agentName,
		"channel", channel, "thread_ts", threadTS,
		"execution_target", backendOverride)

	// Confirm an explicit --backend so the operator knows the runtime.
	if backendOverride != "" {
		msg := fmt.Sprintf(":rocket: Spawning on the *%s* backend.", backendLabel(backendOverride))
		if backendOverride == podmanager.ExecutionTargetLambdaMicroVM && ultracodeOverride == "on" {
			msg = fmt.Sprintf(":rocket: Spawning on the *%s* backend with ultracode on (workflows + subagents).", backendLabel(backendOverride))
		}
		b.threadNote(at, msg)
	}

	// Lineage edge, as in the pool-assign branch (epic ka-0000000933).
	if threadBeadID != "" {
		if err := b.daemon.AddDependency(ctx, beadID, threadBeadID, "triggered-by", "slack-bridge"); err != nil {
			b.logger.Warn("thread-spawn: failed to add triggered-by dep (cold-start)",
				"agent_bead", beadID, "thread_bead", threadBeadID, "error", err)
		}
	}

	// Record thread→agent mapping in state.
	if b.state != nil {
		b.bindThreadAgent(at, agentName)
		if listen {
			_ = b.state.SetListenThread(channel, threadTS)
		}
	}

	// Start a streaming message with minimal initial text.
	// Status info goes to the assistant thread status bar, not the chat. A
	// Commons thread has neither.
	agentShortName := extractAgentName(agentName)
	if !at.slack() {
		return
	}
	if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
		// Anchor TTFT on the triggering message's send-time. ka-0000001004.
		if t, ok := slackTSToTime(ev.TimeStamp); ok {
			b.streamMgr.MarkTriggerSent(agentShortName, t)
		}
		streamTS := b.streamMgr.StartSession(ctx, agentShortName, channel, threadTS, ev.User)
		if streamTS != "" {
			b.mu.Lock()
			b.threadSpawnMsgs[agentShortName] = MessageRef{
				ChannelID: channel, Timestamp: streamTS, ThreadTS: threadTS,
			}
			b.mu.Unlock()
		}
		// Show status info in the assistant thread status bar.
		statusText := fmt.Sprintf("Spinning up... (tracking: %s)", b.beadLink(beadID, beadID))
		b.SetThreadStatus(ctx, channel, threadTS, statusText)
	} else {
		// Fallback: use thread status bar when streaming is unavailable.
		// No persistent chat message — keeps the thread clean.
		statusText := fmt.Sprintf("Spinning up... (tracking: %s)", b.beadLink(beadID, beadID))
		b.SetThreadStatus(ctx, channel, threadTS, statusText)
		// Track the thread reference so lifecycle updates can find it.
		b.mu.Lock()
		b.threadSpawnMsgs[agentShortName] = MessageRef{
			ChannelID: channel, ThreadTS: threadTS,
		}
		b.mu.Unlock()
	}
	// Clear assistant loading status and set title for AI container threads.
	if ev.AssistantThread != nil {
		b.clearAssistantStatus(ctx, channel, threadTS)
		b.setAssistantThreadTitle(ctx, channel, threadTS,
			fmt.Sprintf("Chat with %s", agentShortName))
	}
}

// SpawnOrRespawnForViewerReply gives a beads-viewer /threads reply the same
// outcome as an @gardenio mention (ka-0000005407). The viewer's thread-say bead
// is relayed into Slack as the bot, which handleMessageEvent drops at bot.go's
// botUserID guard, so the forwarder calls this after the Slack post. Like
// handleAppMention's thread branch:
//
//   - No agent bound        → spawn a fresh thread agent (handleThreadSpawn).
//   - Agent gone / terminal → respawn the same name with session resume
//     (respawnThreadAgent).
//   - Live agent            → nudge it (nudgeAgentForMention) without a second
//     bubble: the thread-say bead already renders in the lane.
//
// msgBeadID anchors the nudge; operator is the bead's CreatedBy email, resolved
// to a Slack user id when possible ("" is tolerated). The thread-say close
// event fires once (deduped by the forwarder's alreadySeen) and the bot's
// repost is still dropped, so there is no echo loop.
func (b *Bot) SpawnOrRespawnForViewerReply(ctx context.Context, channel, threadTS, text, msgBeadID, operator string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if !isValidThreadBinding(channel, threadTS) {
		b.logger.Warn("viewer-reply-spawn: invalid thread binding, skipping",
			"channel", channel, "thread_ts", threadTS)
		return
	}

	// Stamp an operator: the SSE watcher's ctx carries none, and the daemon
	// fail-closes writes without one (R2 enforcement, HTTP 400). A bare email
	// becomes oidc:<email> (as operatorFromSlackUser does); oidc:/slack: values
	// pass through; empty falls back to the slack-bridge system operator.
	switch {
	case operator == "":
		ctx = beadsapi.WithSystemOperator(ctx, "slack-bridge")
	case isHumanShapedOperator(operator):
		ctx = beadsapi.WithOperator(ctx, operator)
	default:
		ctx = beadsapi.WithOperator(ctx, "oidc:"+operator)
	}

	// Best-effort Slack user id, for DM-project inference and thread-owner
	// detection; "" when unresolved.
	userID := ""
	if operator != "" {
		if u := b.UserDirectory().LookupByEmail(ctx, operator); u != nil {
			userID = u.SlackID
		}
	}

	// getAgentByThread is daemon-backed, so it survives a bridge restart.
	agent := b.getAgentByThread(channel, threadTS)
	if agent == "" {
		// First reply: spawn fresh. handleThreadSpawn's in-flight and bound-agent
		// guards make this idempotent against a concurrent Slack spawn.
		b.logger.Info("viewer-reply-spawn: no bound agent, spawning fresh thread agent",
			"channel", channel, "thread_ts", threadTS)
		b.handleThreadSpawn(ctx, &slackevents.AppMentionEvent{
			Channel:         channel,
			ThreadTimeStamp: threadTS,
			TimeStamp:       threadTS,
			Text:            text,
			User:            userID,
		}, text)
		return
	}

	agentName := extractAgentName(agent)

	// Missing bead or terminal agent_state: respawn with session resume, as
	// handleAppMention's thread-bound branch does.
	agentBead, err := b.daemon.FindAgentBead(ctx, agentName)
	if err != nil {
		b.logger.Info("viewer-reply-spawn: bound agent no longer active, respawning with session resume",
			"channel", channel, "thread_ts", threadTS, "agent", agentName, "error", err)
		b.respawnThreadAgent(ctx, channel, threadTS, agentName, text, userID)
		return
	}
	if isAgentTerminal(agentBead.Fields["agent_state"]) {
		b.logger.Info("viewer-reply-spawn: bound agent in terminal state, respawning with session resume",
			"channel", channel, "thread_ts", threadTS, "agent", agentName,
			"agent_state", agentBead.Fields["agent_state"])
		b.respawnThreadAgent(ctx, channel, threadTS, agentName, text, userID)
		return
	}

	// Live agent: nudge, anchored on the thread-say bead (no duplicate bubble).
	// An agent that went terminal before the nudge is respawned (ka-0000002089).
	if err := b.nudgeAgentForMention(ctx, agentName, text, msgBeadID); err != nil {
		// Terminal or lame-duck: session-resume respawn; a thread reply always has
		// the anchor (ka-0000002949).
		if errors.Is(err, ErrAgentNotNudgeable) {
			b.logger.Info("viewer-reply-spawn: pod-gone nudge failure, respawning with session resume",
				"channel", channel, "thread_ts", threadTS, "agent", agentName, "error", err)
			b.respawnThreadForGoneNudge(ctx, channel, threadTS, agentName, text, userID)
			return
		}
		// The mail could not be written; log it. The words are in the thread.
		b.logger.Error("viewer-reply-spawn: nudge failed",
			"channel", channel, "thread_ts", threadTS, "agent", agentName, "error", err)
		return
	}
	b.logger.Info("viewer-reply-spawn: nudged live agent",
		"channel", channel, "thread_ts", threadTS, "agent", agentName)
}

// tryPoolAssign asks the controller's pool for a prewarmed agent, returning
// (beadID, agentName) or ("", "") when unavailable; callers fall back to cold
// start. AssignPrewarmed stamps threadBeadID and operatedBy atomically with the
// state flip, so the warm pod's first `kd create` sees thread_id
// (maybeCopyThreadID, ka-0000001160) and its writes are attributed
// (ka-0000000528). resumeS3Key (ka-0000000983), set only on the respawn path
// with WARM_POOL_RESUME_ENABLED, makes the pod coop --resume that transcript;
// "" starts a fresh warm session.
func (b *Bot) tryPoolAssign(ctx context.Context, channel, threadTS, threadBeadID, description, project, role, operatedBy, requireRuntimeClass, resumeS3Key string) (string, string) {
	if b.controllerURL == "" {
		return "", ""
	}

	reqBody, err := json.Marshal(map[string]string{
		"channel":     channel,
		"thread_ts":   threadTS,
		"description": description,
		"project":     project,
		"role":        role,
		// Warm-pool coop-resume graft (ka-0000000983); "" skips the stamp.
		"resume_s3_key": resumeS3Key,
		// Stamped atomically with the state flip (ka-0000000528).
		"operated_by": operatedBy,
		// Same race-close as operated_by (ka-0000001160); "" skips the stamp.
		"thread_id": threadBeadID,
		// Match only a warm pod on this runtime (runsc for a pinned channel like
		// gb-turbo); "" matches any pool (ka-0000004563).
		"require_runtime_class": requireRuntimeClass,
	})
	if err != nil {
		return "", ""
	}

	assignURL := strings.TrimRight(b.controllerURL, "/") + "/api/v1/pool/assign"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, assignURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", ""
	}
	req.Header.Set("Content-Type", "application/json")
	setControllerAPIAuth(req)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		b.logger.Debug("pool assign request failed", "error", err)
		return "", ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b.logger.Debug("pool assign returned non-200", "status", resp.StatusCode)
		return "", ""
	}

	var result struct {
		BeadID    string `json:"bead_id"`
		AgentName string `json:"agent_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		b.logger.Warn("pool assign: failed to decode response", "error", err)
		return "", ""
	}

	return result.BeadID, result.AgentName
}

// fetchThreadContext returns the thread's non-bot messages as agent context,
// plus the root message's author (UX Q4 thread ownership; "" when unknown).
func (b *Bot) fetchThreadContext(ctx context.Context, channel, threadTS string) (context string, rootAuthor string) {
	if threadTS == "" {
		return "(no thread context — top-level message)", ""
	}
	msgs, _, _, err := b.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
		Limit:     50,
	})
	if err != nil {
		b.logger.Error("failed to fetch thread context",
			"channel", channel, "thread_ts", threadTS, "error", err)
		return "(could not fetch thread context)", ""
	}

	// The root is the first message.
	if len(msgs) > 0 {
		rootAuthor = msgs[0].User
	}

	var buf strings.Builder
	for _, msg := range msgs {
		// Skip bot messages to keep the prompt clean.
		if msg.BotID != "" || msg.SubType == "bot_message" {
			continue
		}
		author := msg.User
		if author == "" {
			author = msg.Username
		}
		line := fmt.Sprintf("**%s**: %s\n", author, msg.Text)
		// Annotate messages that have file attachments.
		for _, f := range msg.Files {
			line += fmt.Sprintf("  [attachment: %s (%s) — /api/slack/files/%s]\n", f.Name, f.Mimetype, f.ID)
		}
		if buf.Len()+len(line) > 3000 {
			buf.WriteString("\n_(thread truncated)_\n")
			break
		}
		buf.WriteString(line)
	}

	if buf.Len() == 0 {
		return "(empty thread)", rootAuthor
	}
	return buf.String(), rootAuthor
}

// sanitizeTS converts a Slack timestamp like "1234567890.123456" to a safe
// identifier fragment "1234567890-123456".
func sanitizeTS(ts string) string {
	return strings.ReplaceAll(ts, ".", "-")
}

// projectFromAgentIdentity extracts the project name from an agent identity
// like "gardenio/crew/agent-name" → "gardenio". Returns "" if not project-qualified.
func projectFromAgentIdentity(identity string) string {
	parts := strings.Split(identity, "/")
	if len(parts) >= 2 {
		return parts[0]
	}
	return ""
}

// resolveAgentThread returns a thread-bound agent's validated thread channel
// and ts, or empty strings for regular agents.
func (b *Bot) resolveAgentThread(ctx context.Context, agent string) (channel, threadTS string) {
	agentName := extractAgentName(agent)
	detail, err := b.daemon.FindAgentBead(ctx, agentName)
	if err != nil {
		b.logger.Debug("resolveAgentThread: agent bead not found",
			"agent", agentName, "error", err)
		return "", ""
	}
	ch := detail.Fields["slack_thread_channel"]
	ts := detail.Fields["slack_thread_ts"]
	if !isValidThreadBinding(ch, ts) {
		return "", ""
	}
	return ch, ts
}

// isUnengagedPoolAgent reports whether an agent bead is a prewarmed-pool member
// never assigned to a thread or task. Such agents never get a top-level Slack
// card; their prewarmed/queued/expired churn would flood the channel
// (ka-0000007013). Keyed off the "prewarmed" label and the absence of the
// markers assignPrewarmed stamps (spawn_source=slack-thread-pool, a thread
// binding, task_id), not the volatile agent_state.
func isUnengagedPoolAgent(bead BeadEvent) bool {
	return unengagedPoolAgent(bead.Labels, bead.Fields)
}

// unengagedPoolAgent is the labels+fields core of isUnengagedPoolAgent, shared
// with the card-create guard on daemon BeadDetails; the two must not drift
// (ka-0000000954).
func unengagedPoolAgent(labels []string, fields map[string]string) bool {
	if !slices.Contains(labels, "prewarmed") {
		return false
	}
	// Any engagement marker means the agent has left the pool — let cards flow.
	if strings.HasPrefix(fields["spawn_source"], "slack-thread") {
		return false
	}
	if isValidThreadBinding(fields["slack_thread_channel"], fields["slack_thread_ts"]) {
		return false
	}
	if fields["task_id"] != "" {
		return false
	}
	return true
}

// isValidThreadBinding validates that a Slack thread binding has both a
// non-empty channel ID and a well-formed timestamp (contains a dot separator).
func isValidThreadBinding(channel, threadTS string) bool {
	if channel == "" || threadTS == "" {
		return false
	}
	// Slack channel IDs start with C, D, or G.
	if len(channel) < 2 {
		return false
	}
	// Slack timestamps are "seconds.microseconds" format.
	if !strings.Contains(threadTS, ".") {
		return false
	}
	return true
}

// parseMentionCommand checks if mention text starts with a known command
// keyword (kill, clear, summarize). Returns (keyword, remaining) or ("", text).
func parseMentionCommand(text string) (string, string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return "", text
	}
	keyword := strings.ToLower(words[0])
	switch keyword {
	case "kill", "clear", "summarize":
		remaining := strings.TrimSpace(strings.Join(words[1:], " "))
		return keyword, remaining
	}
	return "", text
}

// handleMentionKill kills the thread-bound agent on "@gardenio kill" in a
// thread, without needing its name.
func (b *Bot) handleMentionKill(ctx context.Context, ev *slackevents.AppMentionEvent, force bool) {
	channel := ev.Channel
	threadTS := ev.ThreadTimeStamp
	userID := ev.User

	agent := b.getAgentByThread(channel, threadTS)
	if agent == "" {
		if b.api != nil {
			_, _ = b.api.PostEphemeral(channel, userID,
				slack.MsgOptionText(":x: No agent is bound to this thread.", false))
		}
		return
	}
	agent = extractAgentName(agent)

	// Only the agent's owner may kill it (bot_kill_owner.go); the kill is held
	// to the bead that was checked and runs as the person.
	bead := b.slackKillTarget(ctx, "@mention kill", agent, channel, threadTS, userID)
	if bead == nil {
		return
	}
	actorCtx := context.WithoutCancel(ctx)

	// Acknowledge immediately — graceful shutdown can take 30s+.
	if b.api != nil {
		_, _, _ = b.api.PostMessage(channel,
			slack.MsgOptionText(fmt.Sprintf(":hourglass_flowing_sand: Killing thread agent *%s*…", agent), false),
			slack.MsgOptionTS(threadTS))
	}

	go func() {
		if err := b.killAgentBead(actorCtx, agent, bead.ID, force); err != nil {
			b.logger.Error("mention-kill: failed", "agent", agent, "error", err)
			if b.api != nil {
				_, _, _ = b.api.PostMessage(channel,
					slack.MsgOptionText(fmt.Sprintf(":x: Failed to kill agent *%s*: %s", agent, err.Error()), false),
					slack.MsgOptionTS(threadTS))
			}
			return
		}
		b.logger.Info("killed thread agent via mention", "agent", agent, "user", userID)
		if b.api != nil {
			_, _, _ = b.api.PostMessage(channel,
				slack.MsgOptionText(fmt.Sprintf(":skull: Thread agent *%s* terminated.", agent), false),
				slack.MsgOptionTS(threadTS))
		}
	}()
}

// handleMentionClear unbinds the thread's agent on "@gardenio clear" without
// killing it, so the next @mention in the thread spawns a fresh agent.
func (b *Bot) handleMentionClear(ctx context.Context, ev *slackevents.AppMentionEvent) {
	channel := ev.Channel
	threadTS := ev.ThreadTimeStamp
	userID := ev.User

	agent := b.getAgentByThread(channel, threadTS)
	if agent == "" {
		if b.api != nil {
			_, _ = b.api.PostEphemeral(channel, userID,
				slack.MsgOptionText(":x: No agent is bound to this thread.", false))
		}
		return
	}
	agent = extractAgentName(agent)

	// Remove thread→agent mapping.
	if b.state != nil {
		_ = b.state.RemoveThreadAgent(channel, threadTS)
		_ = b.state.RemoveListenThread(channel, threadTS)
	}

	b.logger.Info("cleared thread agent mapping via mention",
		"agent", agent, "channel", channel, "thread_ts", threadTS, "user", userID)

	if b.api != nil {
		_, _, _ = b.api.PostMessage(channel,
			slack.MsgOptionText(
				fmt.Sprintf(":broom: Cleared thread binding for *%s*. Mention me again to spawn a new agent here.", agent),
				false),
			slack.MsgOptionTS(threadTS))
	}
}

// MentionCapability is the capability string checked on user beads when the
// mention permission gate is enabled.
const MentionCapability = "mention"

// checkMentionPermission reports whether the Slack user may @mention the bot,
// posting a reason-specific ephemeral denial (and reaction) when not. A
// Keycloak realm role check (when keycloakChecker and keycloakMentionRole are
// set) takes precedence; on API error it falls through to the user bead's
// "mention" capability. Every check emits a "mention_audit" log entry.
func (b *Bot) checkMentionPermission(ctx context.Context, userID, channel, threadTS, messageTS string) bool {
	dir := b.UserDirectory()
	// Only a pinned slack_id (admin-written, or set by the confirmed link)
	// names the person. The Slack profile email is never trusted here: Slack
	// admins, guests and SSO mapping control it (gd-W4FkF80BlX).
	user := dir.LookupBySlackID(ctx, userID)
	if user == nil {
		if dir.SlackEmailHint(ctx, userID) {
			b.logMentionAudit("denied", "unlinked_email_match", userID, "", "", "", channel)
			b.postMentionDeniedUnlinked(channel, userID, threadTS)
			b.reactDenied(channel, messageTS)
			return false
		}
		b.logMentionAudit("denied", "unknown_user", userID, "", "", "", channel)
		b.postMentionDeniedUnknown(channel, userID, threadTS)
		b.reactDenied(channel, messageTS)
		return false
	}

	// Keycloak role check (primary when configured).
	if b.keycloakChecker != nil && b.keycloakMentionRole != "" {
		if user.Email == "" {
			b.logMentionAudit("denied", "no_email_for_keycloak", userID, user.BeadID, user.DisplayName, user.Team, channel)
			b.postMentionDeniedNoCap(channel, userID, threadTS, user.DisplayName)
			b.reactDenied(channel, messageTS)
			return false
		}
		hasRole, err := b.keycloakChecker.HasRealmRole(ctx, user.Email, b.keycloakMentionRole)
		if err != nil {
			b.logger.Warn("keycloak role check failed, falling back to capability check",
				"error", err, "email", user.Email, "role", b.keycloakMentionRole)
			// Fall through to capability check below.
		} else if !hasRole {
			b.logMentionAudit("denied", "missing_keycloak_role", userID, user.BeadID, user.DisplayName, user.Team, channel)
			b.postMentionDeniedNoCap(channel, userID, threadTS, user.DisplayName)
			b.reactDenied(channel, messageTS)
			return false
		} else {
			b.logMentionAudit("allowed", "keycloak_role", userID, user.BeadID, user.DisplayName, user.Team, channel)
			return true
		}
	}

	// Fallback: user bead capability check.
	if !user.HasCapability(MentionCapability) {
		b.logMentionAudit("denied", "missing_capability", userID, user.BeadID, user.DisplayName, user.Team, channel)
		b.postMentionDeniedNoCap(channel, userID, threadTS, user.DisplayName)
		b.reactDenied(channel, messageTS)
		return false
	}

	b.logMentionAudit("allowed", "", userID, user.BeadID, user.DisplayName, user.Team, channel)
	return true
}

// logMentionAudit emits a structured "mention_audit:" log entry for a mention
// permission check.
func (b *Bot) logMentionAudit(outcome, reason, slackUserID, beadID, displayName, team, channel string) {
	args := []any{
		"outcome", outcome,
		"slack_user_id", slackUserID,
		"channel", channel,
	}
	if reason != "" {
		args = append(args, "reason", reason)
	}
	if beadID != "" {
		args = append(args, "user_bead", beadID)
	}
	if displayName != "" {
		args = append(args, "display_name", displayName)
	}
	if team != "" {
		args = append(args, "team", team)
	}
	b.logger.Info("mention_audit: "+outcome, args...)
}

// postMentionDeniedUnknown sends a Block Kit ephemeral message when the user
// has no user bead at all (completely unknown to the system).
func (b *Bot) postMentionDeniedUnknown(channel, userID, threadTS string) {
	if b.api == nil {
		return
	}
	blocks := b.mentionDeniedBlocks(
		":lock: Access Required",
		"Your account is not registered with this bot. An administrator needs to create a user profile for you before you can interact with it.",
		"Ask a workspace admin to register your Slack account. Once set up, you'll be able to @mention the bot to get started.",
	)
	fallback := "Access required — your account is not registered with this bot."
	b.postEphemeralBlocks(channel, userID, threadTS, fallback, blocks)
}

// postMentionDeniedUnlinked sends a Block Kit ephemeral message when the
// Slack user's id is on no user bead but their Slack profile email matches
// one. The email is not proof of who they are, so they must link (or have an
// admin pin their Slack id) before anything is granted.
func (b *Bot) postMentionDeniedUnlinked(channel, userID, threadTS string) {
	if b.api == nil {
		return
	}
	help := "Ask a workspace admin to add your Slack id to your gardenio user."
	if b.slackLinker() != nil {
		help = "Say `@gardenio link` to link this Slack account (you sign in once, then confirm), or ask a workspace admin to add your Slack id to your gardenio user."
	}
	blocks := b.mentionDeniedBlocks(
		b.themeFor(context.Background(), channel).Semantic(SemDenied)+" Link Your Account",
		"This Slack account isn't linked to a gardenio user yet. A matching Slack profile email is not enough to act as that user.",
		help,
	)
	fallback := "Access required — link this Slack account to your gardenio user."
	b.postEphemeralBlocks(channel, userID, threadTS, fallback, blocks)
}

// postMentionDeniedNoCap sends a Block Kit ephemeral message when the user
// exists in the directory but lacks the "mention" capability.
func (b *Bot) postMentionDeniedNoCap(channel, userID, threadTS, displayName string) {
	if b.api == nil {
		return
	}
	greeting := "your"
	if displayName != "" {
		greeting = displayName + "'s"
	}
	blocks := b.mentionDeniedBlocks(
		":lock: Permission Denied",
		fmt.Sprintf("I found %s profile, but it doesn't include permission to use @mentions. The `%s` capability is required.", greeting, MentionCapability),
		"Ask a workspace admin to add the mention capability to your user profile.",
	)
	fallback := "Permission denied — your profile lacks the mention capability."
	b.postEphemeralBlocks(channel, userID, threadTS, fallback, blocks)
}

// mentionDeniedBlocks builds the standard Block Kit layout for denial messages:
// header section, explanation, divider, and a help context line.
func (b *Bot) mentionDeniedBlocks(header, body, help string) []slack.Block {
	headerBlock := slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", header, false, false),
		nil, nil,
	)
	bodyBlock := slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", body, false, false),
		nil, nil,
	)
	divider := slack.NewDividerBlock()
	helpBlock := slack.NewContextBlock("",
		slack.NewTextBlockObject("mrkdwn", ":bulb: "+help, false, false),
	)
	return []slack.Block{headerBlock, bodyBlock, divider, helpBlock}
}

// postEphemeralBlocks posts an ephemeral message with Block Kit blocks and a
// plain-text fallback (for notifications / clients that don't render blocks).
func (b *Bot) postEphemeralBlocks(channel, userID, threadTS, fallback string, blocks []slack.Block) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(blocks...),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _ = b.api.PostEphemeral(channel, userID, opts...)
}

// reactDenied adds the theme's "refused" reaction to the message at ts so the
// user sees a signal even if they miss the ephemeral; skipped when ts is "".
func (b *Bot) reactDenied(channel, ts string) {
	if b.api == nil || ts == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = b.api.AddReaction(b.themeFor(ctx, channel).Reaction(ReactionRefused), slack.ItemRef{
		Channel:   channel,
		Timestamp: ts,
	})
}

// stripBotMention removes all <@BOTID> occurrences from text and trims whitespace.
// noteEmptyMention answers a mention with nothing after the bot's name: an
// ephemeral to the person (a visible reply in an AI container thread, which
// has no ephemerals), once per person and thread in noteOnceTTL.
func (b *Bot) noteEmptyMention(ctx context.Context, ev *slackevents.AppMentionEvent) {
	if b.api == nil || ev.User == "" {
		return
	}
	threadTS := ev.ThreadTimeStamp
	if !b.noteOnce("empty-mention:" + ev.Channel + ":" + threadTS + ":" + ev.User) {
		return
	}
	msg := b.themeFor(ctx, ev.Channel).Semantic(SemInfo) + " You mentioned me with no message. Say what you need after @gardenio."
	if ev.AssistantThread != nil {
		_, _, _ = b.api.PostMessage(ev.Channel,
			slack.MsgOptionText(msg, false),
			slack.MsgOptionTS(threadTS))
		return
	}
	opts := []slack.MsgOption{slack.MsgOptionText(msg, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _ = b.api.PostEphemeral(ev.Channel, ev.User, opts...)
}

func stripBotMention(text, botUserID string) string {
	mention := fmt.Sprintf("<@%s>", botUserID)
	text = strings.ReplaceAll(text, mention, "")
	return strings.TrimSpace(text)
}

// respawnThreadForGoneNudge handles a thread nudge whose pod is effectively
// gone (terminal or stopping bead, or mail unread past MailRespawnAfter): it
// closes the stale active bead, then session-resume respawns. The close is
// required: the bead can still read working/spawning (statusreporter lag), and
// respawnThreadAgent's guard would treat it as alive and skip (ka-0000002949).
func (b *Bot) respawnThreadForGoneNudge(ctx context.Context, channel, threadTS, agent, text, user string) {
	agentName := extractAgentName(agent)
	if bead, err := b.daemon.FindAgentBead(ctx, agentName); err == nil && !isAgentTerminal(bead.Fields["agent_state"]) {
		// Close with agent_state=done (not "stopping", the frozen-awaiting-thaw
		// signal, and no false crash-red) so the reconciler reaps the pod and the
		// respawn guard falls through. Best-effort.
		if err := b.daemon.CloseBead(ctx, bead.ID, map[string]string{"agent_state": "done"}); err != nil {
			b.logger.Warn("mention: failed to close stale pod-gone agent bead before respawn",
				"agent", agentName, "bead", bead.ID, "error", err)
		} else {
			b.logger.Info("mention: closed stale pod-gone agent bead before session-resume respawn",
				"agent", agentName, "bead", bead.ID, "prior_agent_state", bead.Fields["agent_state"])
		}
	}
	b.respawnThreadAgent(ctx, channel, threadTS, agentName, text, user)
}

// nudgeAgentForMention delivers a mention nudge to the target agent as
// interrupt mail. A booting agent needs no retry: its subscriber drains unread
// mail when it starts.
func (b *Bot) nudgeAgentForMention(ctx context.Context, agent, text, beadID string) error {
	_, err := b.nudgeAgentForMentionMail(ctx, agent, text, beadID)
	return err
}

// nudgeAgentForMentionMail is nudgeAgentForMention returning the mail id, so
// the mention path can watch delivery (watchMailDelivery).
func (b *Bot) nudgeAgentForMentionMail(ctx context.Context, agent, text, beadID string) (string, error) {
	return b.nudgeAgentWordsMail(ctx, agent, "Slack mention", text, beadID)
}

// nudgeAgentWords is nudgeAgentForMention with the nudge's label: what the
// words are ("Slack mention", "Commons mention", "Commons reply").
func (b *Bot) nudgeAgentWords(ctx context.Context, agent, label, text, beadID string) error {
	_, err := b.nudgeAgentWordsMail(ctx, agent, label, text, beadID)
	return err
}

// nudgeAgentWordsMail is nudgeAgentWords returning the mail id.
func (b *Bot) nudgeAgentWordsMail(ctx context.Context, agent, label, text, beadID string) (string, error) {
	agentName := extractAgentName(agent)

	message := fmt.Sprintf("%s (bead %s): %s", label, beadID, text)

	mailID, err := NudgeAgentMail(ctx, b.daemon, b.logger, agentName, message)
	if err != nil {
		b.logger.Error("failed to nudge agent for mention",
			"agent", agentName, "bead", beadID, "error", err)
		return "", err
	}

	// Warm delivery: the live-nudge fast path the warm-idle keepalive
	// (ka-0000005793) maximises; respawnThreadAgent counts cold ones
	// (gardenio_mention_delivery_total).
	b.recordMentionDelivery(mentionDeliveryWarm)

	b.logger.Info("nudged agent for mention",
		"agent", agentName, "bead", beadID, "mail", mailID)
	return mailID, nil
}

// MailRespawnAfter is how long a pod agent's mention mail may sit undelivered
// before the bridge treats the pod as gone and respawns it (spec §10). A local
// runner is never respawned (D5: work is held while its laptop is away).
var MailRespawnAfter = 2 * time.Minute

// watchMailDelivery waits MailRespawnAfter, then checks whether the agent took
// mailID (closed, or stamped delivered_at by mailsub once coop accepted it).
// Untaken mail to a pod agent means its pod is gone while its bead lags: close
// and session-resume respawn in the thread. A starting-up agent is left be; a
// local agent gets one note instead. execution_target "local" is the spec's
// earlier spelling of laptop (gd-GTMwKmHM7o).
func (b *Bot) watchMailDelivery(ctx context.Context, mailID, agentName, channel, threadTS, text, user string) {
	if mailID == "" {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(MailRespawnAfter):
	}
	mail, err := b.daemon.GetBead(ctx, mailID)
	if err != nil || mail == nil || mail.Status != "open" || mail.Fields[mailproto.FieldDeliveredAt] != "" {
		return
	}
	name := extractAgentName(agentName)
	agent, err := b.daemon.FindAgentBeadAnyStatus(ctx, name)
	if err != nil || agent == nil {
		return
	}
	if isLaptopAgent(agent) || agent.Fields["execution_target"] == "local" {
		b.logger.Info("mention: mail to local agent not yet picked up, holding",
			"agent", name, "mail", mailID, "after", MailRespawnAfter)
		if b.api != nil {
			_, _, _ = b.api.PostMessage(channel,
				slack.MsgOptionText(fmt.Sprintf("%s has not picked up the message yet (its laptop may be offline). Work is held.", name), false),
				slack.MsgOptionTS(threadTS))
		}
		return
	}
	if agent.Status != "closed" && isAgentStartingUp(agent.Fields["agent_state"]) {
		return
	}
	b.logger.Warn("mention: mail still undelivered past deadline, respawning pod agent",
		"agent", name, "mail", mailID, "after", MailRespawnAfter, "agent_state", agent.Fields["agent_state"])
	// The respawn carries the text itself; left open, the mail would be
	// drained into the resumed session a second time.
	if err := b.daemon.CloseBead(ctx, mailID, nil); err != nil {
		b.logger.Warn("mention: closing undelivered mail before respawn failed", "mail", mailID, "error", err)
	}
	if b.respawnHook != nil {
		b.respawnHook(name)
		return
	}
	b.respawnThreadForGoneNudge(ctx, channel, threadTS, name, text, user)
}

// handleMentionThreadCommand runs a thread @mention control command (kill,
// stop, restart), reporting whether it handled one.
func (b *Bot) handleMentionThreadCommand(ctx context.Context, ev *slackevents.AppMentionEvent, agent, text string) bool {
	cmd, force := parseThreadCommand(text)
	if cmd == "" {
		return false
	}

	channelID := ev.Channel
	threadTS := ev.ThreadTimeStamp
	userID := ev.User

	switch cmd {
	case "kill", "stop":
		// Only the agent's owner may kill it (bot_kill_owner.go).
		bead := b.slackKillTarget(ctx, "@mention "+cmd, agent, channelID, threadTS, userID)
		if bead == nil {
			return true
		}
		actorCtx := context.WithoutCancel(ctx)
		if b.api != nil {
			_, _, _ = b.api.PostMessage(channelID,
				slack.MsgOptionText(fmt.Sprintf(":hourglass_flowing_sand: Killing thread agent *%s*…", agent), false),
				slack.MsgOptionTS(threadTS),
			)
		}
		go func() {
			if err := b.killAgentBead(actorCtx, agent, bead.ID, force); err != nil {
				b.logger.Error("mention kill: failed", "agent", agent, "error", err)
				if b.api != nil {
					_, _ = b.api.PostEphemeral(channelID, userID,
						slack.MsgOptionText(fmt.Sprintf(":x: Failed to kill agent %q: %s", agent, err.Error()), false))
				}
				return
			}
			b.logger.Info("killed thread agent via mention", "agent", agent, "user", userID)
			if b.api != nil {
				_, _, _ = b.api.PostMessage(channelID,
					slack.MsgOptionText(fmt.Sprintf(":skull: Thread agent *%s* terminated.", agent), false),
					slack.MsgOptionTS(threadTS),
				)
			}
		}()
		return true

	case "restart":
		// Reopen debounce (ka-0000000034): a burst of restarts (or one racing the
		// Restart button) coalesces into the leading one; the restart carries no
		// user content, so nothing is lost.
		if !b.reopenDebouncer.Allow(agent) {
			b.logger.Info("mention restart: coalesced rapid restart (debounced)",
				"agent", agent, "channel", channelID, "thread_ts", threadTS, "user", userID)
			if b.api != nil {
				_, _, _ = b.api.PostMessage(channelID,
					slack.MsgOptionText(fmt.Sprintf(":arrows_counterclockwise: Agent *%s* is already restarting — coalescing your request.", agent), false),
					slack.MsgOptionTS(threadTS),
				)
			}
			return true
		}
		if b.api != nil {
			_, _, _ = b.api.PostMessage(channelID,
				slack.MsgOptionText(fmt.Sprintf(":arrows_counterclockwise: Restarting thread agent *%s*…", agent), false),
				slack.MsgOptionTS(threadTS),
			)
		}
		go func() {
			b.respawnThreadAgent(context.Background(), channelID, threadTS, agent,
				"Restarted via @mention command", userID)
		}()
		return true
	}

	return false
}

// parseThreadCommand returns a bare thread control command (kill, stop,
// restart; only --force may follow) and whether --force was given, so prose
// like "kill the deployment process" is not a command.
func parseThreadCommand(text string) (cmd string, force bool) {
	text = strings.TrimSpace(strings.ToLower(text))
	words := strings.Fields(text)
	if len(words) == 0 {
		return "", false
	}

	switch words[0] {
	case "kill", "stop", "restart":
		cmd = words[0]
	default:
		return "", false
	}

	// Only recognize as a command if no extra words beyond flags.
	for _, w := range words[1:] {
		if w == "--force" {
			force = true
		} else {
			// Extra words like "kill the deployment" — not a command.
			return "", false
		}
	}
	return cmd, force
}

// gardenerStartArgs builds the `gb gardener start` argv; pure so tests can
// assert it without stubbing gardenerStartRunner (ka-0000006141). operatedBy
// (e.g. "slack:U07K…") is forwarded so the trigger bead is owner-attributed
// to the human, not the bridge's bot identity (ka-0000003284).
// --i-read-the-gardener-instructions satisfies the read-the-guide gate
// (ka-0000007415, gardener.go), which targets human authors, not this fixed
// argv; without it the entrypoint fails for everyone.
func gardenerStartArgs(beadID, operatedBy string) []string {
	args := []string{"gardener", "start", beadID, "--i-read-the-gardener-instructions"}
	if operatedBy != "" {
		args = append(args, "--operated-by", operatedBy)
	}
	return args
}

// gardenerStartRunner runs `gb gardener start` with gardenerStartArgs,
// returning combined output. Tests override it.
var gardenerStartRunner = func(ctx context.Context, beadID, operatedBy string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gb", gardenerStartArgs(beadID, operatedBy)...)
	return cmd.CombinedOutput()
}

// tryHandleEpicGardenerStart implements the §T7 entrypoint
// (gardenio/plans/epic-mention-looping-gardener.md): if the leading token
// names a type=epic bead, it runs `gb gardener start <id>` and returns true
// (caller stops routing); an "already exists" failure counts as success.
// Returns false (fall through) for a non-bead-ID token, a failed lookup, or a
// non-epic bead.
func (b *Bot) tryHandleEpicGardenerStart(ctx context.Context, ev *slackevents.AppMentionEvent, text string) bool {
	words := strings.Fields(text)
	if len(words) == 0 {
		return false
	}
	candidate := words[0]
	if !beadIDPattern.MatchString(candidate) {
		return false
	}
	if b.daemon == nil {
		return false
	}

	bead, err := b.daemon.GetBead(ctx, candidate)
	if err != nil {
		b.logger.Debug("epic-gardener-mention: bead lookup failed, falling through",
			"bead", candidate, "channel", ev.Channel, "error", err)
		return false
	}
	if bead.Type != "epic" {
		return false
	}

	// Attribute the trigger bead to the mentioning human (tiers 1-2 of the
	// beads-viewer owner resolver), not the bot (ka-0000003284).
	var operator string
	if u := strings.TrimSpace(ev.User); u != "" {
		operator = "slack:" + u
	}
	out, runErr := gardenerStartRunner(ctx, candidate, operator)
	already := runErr != nil && strings.Contains(string(out), "already exists")
	if runErr != nil && !already {
		b.logger.Error("epic-gardener-mention: gb gardener start failed",
			"bead", candidate, "error", runErr, "out", string(out))
		if b.api != nil {
			replyTS := ev.ThreadTimeStamp
			if replyTS == "" {
				replyTS = ev.TimeStamp
			}
			_, _, _ = b.api.PostMessage(ev.Channel,
				slack.MsgOptionText(
					fmt.Sprintf(":x: Failed to start gardener for %s: %s",
						b.beadLink(candidate, candidate), runErr),
					false),
				slack.MsgOptionTS(replyTS))
		}
		return true
	}

	b.logger.Info("epic-gardener-mention: started gardener",
		"bead", candidate, "already", already, "channel", ev.Channel, "user", ev.User)

	if b.api != nil {
		replyTS := ev.ThreadTimeStamp
		if replyTS == "" {
			replyTS = ev.TimeStamp
		}
		var msg string
		if already {
			msg = fmt.Sprintf(":anchor: Gardener already running for epic %s.",
				b.beadLink(candidate, candidate))
		} else {
			msg = fmt.Sprintf(":anchor: Started gardener for epic %s.",
				b.beadLink(candidate, candidate))
		}
		_, _, _ = b.api.PostMessage(ev.Channel,
			slack.MsgOptionText(msg, false),
			slack.MsgOptionTS(replyTS))
	}
	return true
}

// maybeInterruptFreeze asks the controller to abort any in-flight freeze of
// the agent (C2 interruptible freeze, ka-0000005834) so a warm mid-checkpoint
// Pod A stays live for the inbound message. Best-effort and non-blocking: any
// error or a 404 means nothing to interrupt. No-op without controllerURL;
// inert unless the controller has FreezeInterruptEnabled.
func (b *Bot) maybeInterruptFreeze(ctx context.Context, agent string) {
	if b.controllerURL == "" || agent == "" {
		return
	}
	body, err := json.Marshal(map[string]string{"agent": agent})
	if err != nil {
		return
	}
	url := strings.TrimRight(b.controllerURL, "/") + "/api/v1/freeze/interrupt"
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	setControllerAPIAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Unreachable or timed out: the agent is almost always already live.
		b.logger.Debug("freeze interrupt: controller call failed (proceeding; agent assumed live)",
			"agent", agent, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		b.logger.Info("freeze interrupt: aborted in-flight checkpoint; agent kept live for message",
			"agent", agent)
	}
}

// thawBufferPollInterval is how often the mid-thaw buffer re-checks; ≈1s
// matches the thawepoch ack cadence (design ka-0000005125). A var for tests.
var thawBufferPollInterval = time.Second

// thawBufferDeadline bounds a mid-thaw buffer before the respawn / "next
// cycle" fallback. A fixed upper bound (the controller's tunables live in a
// separate binary) comfortably above a healthy thaw (ThawRehydrateDeadline
// 60s, ThawHardDeadline) plus a fail-closed cold start, so only a wedged thaw
// times out. A var for tests.
var thawBufferDeadline = 5 * time.Minute

// isAgentThawing reports whether the controller has a thaw of the agent in
// flight (GET /api/v1/thaw/status), so the mention path can buffer the nudge
// (ka-0000005830 / design ka-0000005125). Any error, non-200, missing
// controllerURL or flag-off controller (FreezeThawEnabled) returns false.
func (b *Bot) isAgentThawing(ctx context.Context, agent string) bool {
	if b.controllerURL == "" || agent == "" {
		return false
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	statusURL := strings.TrimRight(b.controllerURL, "/") + "/api/v1/thaw/status?agent=" + url.QueryEscape(agent)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, statusURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Unreachable or timed out: assume not thawing.
		b.logger.Debug("thaw status: controller call failed (proceeding, not buffering)",
			"agent", agent, "error", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var out struct {
		Thawing bool `json:"thawing"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	if out.Thawing {
		b.logger.Info("thaw status: agent is mid-thaw; buffering nudge until rehydration completes",
			"agent", agent)
	}
	return out.Thawing
}

// waitForThawAndNudge polls until the thaw releases and the agent bead is live
// again (rehydrated Pod B, or the fail-closed cold-start pod), then delivers
// the nudge (design ka-0000005125). Returns false on deadline, terminal agent
// or ErrAgentNotNudgeable, for the caller's fallback. It never respawns, so it
// cannot race the in-flight thaw with a duplicate spawn.
func (b *Bot) waitForThawAndNudge(ctx context.Context, agent, text, beadID string) bool {
	deadline := time.Now().Add(thawBufferDeadline)
	ticker := time.NewTicker(thawBufferPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			b.logger.Info("thaw buffer: deadline exceeded while waiting for thaw to release",
				"agent", agent, "bead", beadID, "deadline", thawBufferDeadline)
			return false
		}
		// Still thawing: neither nudge nor respawn.
		if b.isAgentThawing(ctx, agent) {
			continue
		}
		// Thaw released; wait for a live agent bead.
		bead, err := b.daemon.FindAgentBead(ctx, extractAgentName(agent))
		if err != nil {
			// Bead temporarily gone (fail-closed teardown → cold start); keep waiting.
			b.logger.Debug("thaw buffer: agent bead not yet resolvable post-thaw, still waiting",
				"agent", agent, "bead", beadID, "error", err)
			continue
		}
		if isAgentTerminal(bead.Fields["agent_state"]) {
			// Fail-closed to terminal: defer to the caller's fallback.
			b.logger.Info("thaw buffer: agent went terminal after thaw, deferring to fallback",
				"agent", agent, "bead", beadID, "agent_state", bead.Fields["agent_state"])
			return false
		}
		if agentLooksDead(bead) {
			// Closed or stopping mid-handover: keep polling under the deadline.
			continue
		}
		// Live: deliver; retry transient failures under the deadline.
		if err := b.nudgeAgentForMention(ctx, agent, text, beadID); err != nil {
			if errors.Is(err, ErrAgentNotNudgeable) {
				b.logger.Info("thaw buffer: agent not nudgeable post-thaw, deferring to fallback",
					"agent", agent, "bead", beadID, "error", err)
				return false
			}
			b.logger.Debug("thaw buffer: nudge attempt failed, retrying under deadline",
				"agent", agent, "bead", beadID, "error", err)
			continue
		}
		b.logger.Info("thaw buffer: delivered buffered nudge to rehydrated agent",
			"agent", agent, "bead", beadID)
		return true
	}
}

// tryHandleSlackLinkCommand handles `@gardenio link`, `unlink` and `whoami`
// (account linking, gd-OZJ6H2S3xx). Returns true when the text was one of
// them. `link` always issues a fresh single-use button, even when already
// linked (re-link after a Keycloak-side revoke).
func (b *Bot) tryHandleSlackLinkCommand(ctx context.Context, ev *slackevents.AppMentionEvent, text string) bool {
	l := b.slackLinker()
	if l == nil {
		return false
	}
	words := strings.Fields(strings.ToLower(text))
	if len(words) != 1 {
		return false
	}
	switch words[0] {
	case "link":
		b.postSlackRefusal(ctx, ev.Channel, ev.User, "", ev.ThreadTimeStamp, refusalNeedsLink)
	case "unlink":
		email, err := l.Unlink(ctx, ev.User)
		switch {
		case err != nil:
			b.logger.Error("slack unlink failed", "user", ev.User, "error", err)
			b.postSlackRefusal(ctx, ev.Channel, ev.User, "", ev.ThreadTimeStamp, "Unlink failed; try again.")
		case email == "":
			b.postSlackRefusal(ctx, ev.Channel, ev.User, "", ev.ThreadTimeStamp, "Your Slack account is not linked.")
		default:
			b.postSlackRefusal(ctx, ev.Channel, ev.User, "", ev.ThreadTimeStamp,
				slackPersonRefusal("Unlinked from *"+email+"*. Slack actions are refused until you link again."))
		}
	case "whoami":
		if email, ok := l.Linked(ev.User); ok {
			b.postSlackRefusal(ctx, ev.Channel, ev.User, "", ev.ThreadTimeStamp, slackPersonRefusal("You act in gardenio as *oidc:"+email+"* (capped at operator)."))
		} else {
			b.postSlackRefusal(ctx, ev.Channel, ev.User, "", ev.ThreadTimeStamp, refusalNeedsLink)
		}
	default:
		return false
	}
	return true
}
