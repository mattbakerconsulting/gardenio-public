package bridge

import (
	"context"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// maxViewedContextBeads caps how many recent open issues the viewed-channel
// live-context block lists. Small on purpose: the block is grounding context
// prepended to a spawned agent's description (which handleThreadSpawn truncates
// to 4000 chars), not an exhaustive report.
const maxViewedContextBeads = 5

// maxAssistantSuggestedPrompts is Slack's hard cap for
// assistant.threads.setSuggestedPrompts. Sending more returns invalid_arguments,
// which leaves the assistant thread uninitialized and makes Slack reject the
// user's first message with "Slack couldn't send this message" (ka-0000004757).
const maxAssistantSuggestedPrompts = 4

// capSuggestedPrompts trims a prompt list to Slack's maximum, keeping the
// highest-priority (earliest) entries. Callers should order the most relevant
// prompts first so context-aware ones survive the trim.
func capSuggestedPrompts(p []slack.AssistantThreadsPrompt) []slack.AssistantThreadsPrompt {
	if len(p) > maxAssistantSuggestedPrompts {
		return p[:maxAssistantSuggestedPrompts]
	}
	return p
}

// handleAppHomeOpened processes the app_home_opened event, which under
// agent_view (Agent messaging experience, GA 2026-06-30) REPLACES
// assistant_thread_started as the signal that a user opened the gardenio agent
// surface. Slack fires app_home_opened for every App Home open; the Messages
// tab (tab == "messages") is our DM agent surface, so we ignore the Home/About
// tabs. See docs/assistant-pane-smoke-check.md for the surface's history.
//
// Unlike the old assistant thread lifecycle, agent_view has no per-open thread:
// suggested prompts now live at the top of the Messages tab (thread_ts is no
// longer required), and message history persists as a DM — so we set the
// (idempotent, message-free) suggested prompts on every open but do NOT post a
// per-open greeting, which would spam the DM. Onboarding lives in the
// persistent prompts instead.
func (b *Bot) handleAppHomeOpened(ctx context.Context, ev *appHomeOpenedEvent) {
	if ev.Tab != "messages" {
		return
	}

	// The viewed-channel context is embedded in app_home_opened only when the
	// app is subscribed to app_context_changed (empty otherwise). See
	// agent_view_events.go for the superset struct that preserves it on parse.
	contextChannel := ev.Context.channelID()

	// Record under the SAME "thread_started" label the smoke-check alert
	// already queries — this event is the agent_view successor to
	// assistant_thread_started, so the metric series stays continuous
	// (ka-0000000892 dashboard + AssistantPaneSilent alert).
	b.recordAssistantEvent(assistantEventThreadStarted)
	b.logger.Info("agent pane opened (app_home_opened tab=messages)",
		"channel", ev.Channel,
		"user", ev.User,
		"context_channel", contextChannel)

	// Refresh the cached viewed-channel context for this DM so a later
	// "spawn an agent" resolves the project the user is looking at. Keyed by
	// the DM channel with an empty thread_ts (agent_view has no per-open
	// thread).
	//
	// SET on present, do NOT clear on absent. Slack OMITS the context field
	// entirely on an app_home_opened that carries no viewed entities — only
	// app_context_changed emits an explicit empty {} (per the app_context
	// changelog). An omitted context is therefore indistinguishable at the
	// bridge from "user is viewing nothing", and app_home_opened fires on
	// EVERY Messages-tab open (clicking into the DM, reloading Slack), most of
	// which carry no context. Clearing on those opens would wipe a channel a
	// prior context-bearing open cached and break "spawn for this channel"
	// (ka-0000004985 review). So we mirror the old assistant_view handlers'
	// purely-additive semantics: a context survives until a NEW one replaces it.
	if contextChannel != "" {
		b.setAssistantCtxChannel(ev.Channel, "", contextChannel)
	}

	if b.api == nil {
		return
	}

	// Refresh channel-level suggested prompts (thread_ts omitted — no longer
	// required under agent_view). Idempotent and posts no message, so safe on
	// every open. The context-aware prompt goes first so it survives the
	// 4-prompt cap when we know which channel the user is viewing.
	prompts := slack.AssistantThreadsSetSuggestedPromptsParameters{
		ChannelID: ev.Channel,
		Title:     "What would you like to do?",
		Prompts:   capSuggestedPrompts(b.buildAgentQuickActions(ctx, contextChannel)),
	}
	if err := b.api.SetAssistantThreadsSuggestedPromptsContext(ctx, prompts); err != nil {
		b.logger.Error("failed to set agent suggested prompts",
			"channel", ev.Channel,
			"error", err)
	}
}

// buildAgentQuickActions builds the suggested-prompt list for the agent
// Messages tab, ordered so the context-aware prompts come first and survive the
// 4-prompt cap. contextChannel is the channel the user is currently viewing
// ("" when unknown).
func (b *Bot) buildAgentQuickActions(ctx context.Context, contextChannel string) []slack.AssistantThreadsPrompt {
	var quickActions []slack.AssistantThreadsPrompt
	if contextChannel != "" {
		quickActions = append(quickActions, slack.AssistantThreadsPrompt{
			Title:   "Spawn agent for this channel",
			Message: fmt.Sprintf("spawn an agent for channel <#%s>", contextChannel),
		})

		// If this channel is mapped to a project, add a project-specific prompt.
		if project := b.projectFromChannel(ctx, contextChannel); project != "" {
			quickActions = append(quickActions, slack.AssistantThreadsPrompt{
				Title:   fmt.Sprintf("What's happening in %s?", project),
				Message: fmt.Sprintf("show the status of project %s", project),
			})
		}
	}
	quickActions = append(quickActions,
		slack.AssistantThreadsPrompt{Title: "Spawn an agent", Message: "spawn an agent"},
		slack.AssistantThreadsPrompt{Title: "Show the roster", Message: "show the roster"},
		slack.AssistantThreadsPrompt{Title: "Pending decisions", Message: "show pending decisions"},
		slack.AssistantThreadsPrompt{Title: "What agents are running?", Message: "what agents are running?"},
	)
	return quickActions
}

// handleAppContextChanged processes the app_context_changed event, the
// agent_view successor to assistant_thread_context_changed (Agent context,
// 2026-07-02). It fires when the user's active context changes while a gardenio
// surface is open.
//
// Unlike the old event, its payload carries NEITHER the DM channel nor the user
// id — only the ordered viewed `entities` — so it cannot be attributed to a
// specific DM at the bridge. We therefore cannot refresh a particular DM's
// suggested prompts or context cache from it. Its real value is the
// SUBSCRIPTION itself: subscribing to app_context_changed is what makes Slack
// embed the viewed context into app_home_opened (as `context`) and message.im
// (as `app_context`). We consume it off app_home_opened — that is where
// handleAppHomeOpened reads `context` and caches the viewed channel; the
// message.im `app_context` field is not currently parsed (the cache populated
// on open carries the viewed channel to the message/spawn path instead). We
// record this event under the existing "context_changed" label and log it for
// smoke-check parity and observability.
func (b *Bot) handleAppContextChanged(ctx context.Context, ev *appContextChangedEvent) {
	b.recordAssistantEvent(assistantEventContextChanged)
	b.logger.Info("agent context changed (app_context_changed)",
		"context_channel", ev.Context.channelID())
}

// handleAssistantMessage processes a user message in an AI container thread.
// It sets a loading indicator, then synthesizes an @mention so the existing
// mention handler routes the message (agent forwarding for bound threads,
// concierge/spawn for new threads).
func (b *Bot) handleAssistantMessage(ctx context.Context, ev *slackevents.MessageEvent) {
	channel := ev.Channel
	threadTS := ev.ThreadTimeStamp

	// Record that inbound delivery is alive. This is the counter a smoke-check
	// alerts on: if thread_started keeps climbing while message stays flat,
	// Slack has stopped delivering message.im for the pane (ka-0000000892).
	b.recordAssistantEvent(assistantEventMessage)

	// Set loading status while we process — but ONLY when the operator has opted
	// back into holding the assistant status footer. By default we suppress it:
	// this "Thinking..." status, set once here, is NOT cleared by the deferred
	// clearAssistantStatus below (that call no-ops while a stream session is
	// active), so it lingers for the whole run. A continuously-present status on
	// the DM-style assistant surface is community-reported to lock the composer,
	// blocking mid-inference follow-ups (ka-0000000115). The main threaded
	// interface — which accepts follow-ups fine — sets no status at all; leaving
	// this off makes the pane mirror it. Progress still shows in the streaming
	// message + agent card.
	if b.notifConfig.HoldAssistantStatus {
		_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
			ChannelID: channel,
			ThreadTS:  threadTS,
			Status:    "Thinking...",
			LoadingMessages: []string{
				"Processing your request...",
				"Checking agents...",
				"Working on it...",
			},
		})
	}

	// Ensure loading status is always cleared after processing, regardless of
	// which code path handleAppMention takes. Some paths (thread spawn errors,
	// ephemeral help messages) return early without clearing the status, which
	// causes Slack to show "Slack couldn't send this message" in the AI container.
	defer b.clearAssistantStatus(ctx, channel, threadTS)

	// Synthesize an @mention so handleAppMention handles routing (it already
	// resolves bound agents, spawns new ones, and posts confirmations).
	text := ev.Text
	if !strings.Contains(text, fmt.Sprintf("<@%s>", b.botUserID)) {
		text = fmt.Sprintf("<@%s> %s", b.botUserID, text)
	}
	b.handleAppMention(ctx, &slackevents.AppMentionEvent{
		User:            ev.User,
		Text:            text,
		TimeStamp:       ev.TimeStamp,
		ThreadTimeStamp: threadTS,
		Channel:         channel,
		AssistantThread: ev.AssistantThread,
	})
}

// buildViewedChannelContextNote assembles a compact "live context" block for the
// channel the user is viewing next to the agent pane (ka-0000000778). It resolves
// the viewed channel's human name via conversations.info (best-effort) and lists
// the project's most-recently-updated open issues, so a question asked with the
// pane open is grounded in what is actually happening in that project without the
// spawned agent needing a first round-trip to discover it.
//
// The project is resolved FROM the viewed channel (not from the spawn's resolved
// project, which an explicit --project override could make disagree), so the note
// always describes the channel-to-project mapping accurately.
//
// Returns "" when there is nothing useful to add (no viewed channel, the channel
// maps to no project) so the caller can cheaply skip the block. Callers append
// the result to the spawned agent's description; the note is deliberately bounded
// (one header line + up to maxViewedContextBeads rows) so it fits inside that
// description's 4000-char budget.
func (b *Bot) buildViewedChannelContextNote(ctx context.Context, viewedChannel string) string {
	if viewedChannel == "" {
		return ""
	}
	project := b.projectFromChannel(ctx, viewedChannel)
	if project == "" {
		return ""
	}

	// Resolve the viewed channel's display name (best-effort). Fall back to the
	// raw <#id> mention, which Slack still renders as a clickable channel link,
	// so a conversations.info failure degrades gracefully rather than blanking.
	channelLabel := fmt.Sprintf("<#%s>", viewedChannel)
	if b.api != nil {
		if info, err := b.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: viewedChannel}); err == nil && info != nil && info.Name != "" {
			channelLabel = "#" + info.Name
		} else if err != nil {
			b.logger.Debug("viewed-channel context: conversations.info failed",
				"channel", viewedChannel, "error", err)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "You're viewing %s next to this pane, which maps to project `%s`.", channelLabel, project)

	// List the project's most-recently-updated open work (newest first) so the
	// agent can answer "what's happening in <project>?" from grounded context.
	if b.daemon != nil {
		res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Labels:           []string{"project:" + project},
			Kinds:            []string{"issue"},
			Statuses:         []string{"open", "in_progress"},
			ExcludeScheduled: true,
			Sort:             "-updated_at",
			Limit:            maxViewedContextBeads,
		})
		switch {
		case err != nil:
			b.logger.Debug("viewed-channel context: bead list failed",
				"project", project, "error", err)
		case res != nil && len(res.Beads) > 0:
			sb.WriteString("\nRecent open work in this project (most-recently-updated first):")
			for _, bd := range res.Beads {
				title := truncateText(strings.TrimSpace(bd.Title), 100)
				fmt.Fprintf(&sb, "\n- %s [%s] %s", bd.ID, bd.Status, title)
			}
		default:
			sb.WriteString("\nNo open issues are currently tracked for this project.")
		}
	}
	return sb.String()
}

// setAssistantThreadTitle sets a descriptive title on an assistant thread.
// Called after routing to help users find past conversations in the History tab.
func (b *Bot) setAssistantThreadTitle(ctx context.Context, channel, threadTS, title string) {
	if title == "" {
		return
	}
	// Truncate to 255 chars (Slack limit for thread titles).
	title = truncateText(title, 255)
	err := b.api.SetAssistantThreadsTitleContext(ctx, slack.AssistantThreadsSetTitleParameters{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Title:     title,
	})
	if err != nil {
		b.logger.Debug("failed to set assistant thread title",
			"channel", channel, "thread_ts", threadTS, "error", err)
	}
}

// clearAssistantStatus clears the loading indicator on an assistant thread.
// Should be called after a response has been posted. Skips clearing if a
// streaming session is active for this thread — the stream flush loop manages
// the status in that case, and clearing here would cause flickering.
func (b *Bot) clearAssistantStatus(ctx context.Context, channel, threadTS string) {
	if b.streamMgr != nil && b.streamMgr.HasSessionForThread(channel, threadTS) {
		return
	}
	_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Status:    "",
	})
}

// SetThreadStatus implements ThreadStatusSetter for the AgentStreamManager.
// It updates the assistant thread status indicator with the agent's current
// activity (e.g., "reading files...", "running commands...").
//
// A NON-EMPTY status is suppressed unless notifConfig.HoldAssistantStatus is
// set. This is the composer-unlock fix (ka-0000000115): the assistant pane held
// this status footer populated for the entire run (flush re-set it every tick;
// RestoreThreadStatus re-set it after every post; HandleBeadClaimed /
// HandleDecisionCreated / the mention "Starting up..." also set it) — the only
// axis on which it diverged from the main threaded interface, which sets no
// status and accepts mid-inference follow-ups. A continuously-held status on a
// DM-style surface is community-reported to lock the Slack composer. Suppressing
// non-empty writes here (the single choke point for all the callers above) makes
// the pane mirror the known-good interface; live progress still renders in the
// streaming message and agent card.
//
// An EMPTY status (a clear) ALWAYS passes through — clearing a stale indicator
// is always safe and desirable (e.g. StopSession's terminal clear at
// agentstream.go).
func (b *Bot) SetThreadStatus(ctx context.Context, channelID, threadTS, status string) {
	if b.api == nil {
		return
	}
	if status != "" && !b.notifConfig.HoldAssistantStatus {
		return
	}
	_ = b.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID: channelID,
		ThreadTS:  threadTS,
		Status:    status,
	})
}

// PostThreadMessage posts a durable plain message into a Slack thread. Used by
// the stream manager to post the "Resumed in Xs" confirmation when a cold
// thread-agent resume completes (ka-0000005084). Implements ThreadMessagePoster.
// After posting, it restores the assistant status bar (Slack clears it on any
// bot post), mirroring the mention-path PostMessage handling.
func (b *Bot) PostThreadMessage(ctx context.Context, channelID, threadTS, text string) {
	if b.api == nil || text == "" {
		return
	}
	if _, _, err := b.api.PostMessage(channelID,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
	); err != nil {
		b.logger.Warn("failed to post thread message", "channel", channelID, "thread_ts", threadTS, "error", err)
		return
	}
	b.restoreThreadStatus(ctx, channelID, threadTS)
}

// restoreThreadStatus re-sets the assistant thread status from the active
// streaming session after a chat message has been posted. Slack clears the
// assistant thread status whenever a bot message is posted, which interrupts
// the live status bar. Call this after any PostMessage to a thread that has
// an active streaming session.
func (b *Bot) restoreThreadStatus(ctx context.Context, channelID, threadTS string) {
	if b.streamMgr == nil {
		return
	}
	// Composer-unlock (ka-0000000115): when we are not holding the status footer,
	// re-applying it after every post is exactly the continuously-held status we
	// removed. SetThreadStatus would drop the re-set anyway, so skip the session
	// scan + status rebuild entirely.
	if !b.notifConfig.HoldAssistantStatus {
		return
	}
	// Find the active session for this thread and re-apply its last status.
	b.streamMgr.RestoreThreadStatus(ctx, channelID, threadTS)
}

// isAssistantThread checks if a message event originated from an AI container
// thread by checking for the presence of the AssistantThread field.
func isAssistantThread(ev *slackevents.MessageEvent) bool {
	return ev.AssistantThread != nil
}

// assistantCtxKey builds the map key for the assistant context channel cache.
func assistantCtxKey(channel, threadTS string) string {
	return channel + ":" + threadTS
}

// setAssistantCtxChannel caches the context channel (the channel the user is
// currently viewing) for an assistant thread. This is needed because individual
// message events in assistant threads only carry an ActionToken, not the full
// context with the viewed channel ID.
func (b *Bot) setAssistantCtxChannel(channel, threadTS, ctxChannel string) {
	b.mu.Lock()
	b.assistantCtxChannel[assistantCtxKey(channel, threadTS)] = ctxChannel
	b.mu.Unlock()
}

// getAssistantCtxChannel returns the cached context channel for an assistant
// thread, or "" if not cached.
func (b *Bot) getAssistantCtxChannel(channel, threadTS string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.assistantCtxChannel[assistantCtxKey(channel, threadTS)]
}
