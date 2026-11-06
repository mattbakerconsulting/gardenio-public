package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// decisionPriorityEmoji returns a colored circle emoji for the given bead priority.
// P0–1 (critical/high) → red, P2 (normal) → white, P3–4 (low/backlog) → green.
func decisionPriorityEmoji(priority int) string {
	switch {
	case priority <= 1:
		return ":red_circle:"
	case priority >= 3:
		return ":large_green_circle:"
	default:
		return ":white_circle:"
	}
}

// decisionQuestion returns the question text from a decision bead's fields.
// Prefers the canonical "prompt" field, falling back to the legacy "question"
// field for backwards compatibility with older beads.
func decisionQuestion(fields map[string]string) string {
	if v := fields["prompt"]; v != "" {
		return v
	}
	return fields["question"]
}

// resolveDecisionMentionUser looks up the Slack user ID to @mention in a
// decision notification. It checks the agent bead's slack_user_id field,
// which is set when agents are spawned via Slack.
func (b *Bot) resolveDecisionMentionUser(ctx context.Context, agent string) string {
	if agent == "" {
		return ""
	}
	agentName := extractAgentName(agent)
	detail, err := b.daemon.FindAgentBead(ctx, agentName)
	if err != nil {
		return ""
	}
	return detail.Fields["slack_user_id"]
}

// NotifyDecision posts a Block Kit message to Slack for a new decision.
// Layout matches the beads implementation: each option is a Section block
// with numbered label, description, and right-aligned accessory button.
func (b *Bot) NotifyDecision(ctx context.Context, bead BeadEvent) error {
	question := decisionQuestion(bead.Fields)
	optionsRaw := bead.Fields["options"]
	agent := extractAgentName(bead.Assignee)
	assignee := agent
	agentDisplay := b.agentDisplayName(agent)
	agentChannel := b.resolveCardChannel(ctx, agent, "decision")
	th := b.themeFor(ctx, agentChannel)

	// Parse options — try JSON array of objects first, then strings.
	type optionObj struct {
		ID           string `json:"id"`
		Short        string `json:"short"`
		Label        string `json:"label"`
		Description  string `json:"description"`
		ArtifactType string `json:"artifact_type,omitempty"`
	}
	var optObjs []optionObj
	var optStrings []string

	if optionsRaw != "" {
		if err := json.Unmarshal([]byte(optionsRaw), &optObjs); err != nil || len(optObjs) == 0 {
			if err := json.Unmarshal([]byte(optionsRaw), &optStrings); err != nil {
				optStrings = []string{optionsRaw}
			}
		}
	}

	// Resolve who this decision is ADDRESSED to — the decision's own
	// `operated_by` if it carries one, else the requesting agent's operator
	// (bot_decisions_operated_by.go). This is both the @-mention and the DM
	// target.
	addressee := b.resolveDecisionAddressee(ctx, bead.Fields, bead.Assignee)
	mentionUserID := addressee.SlackID

	// Build Block Kit blocks — header section with priority-colored indicator.
	headerText := fmt.Sprintf("%s *Decision Needed*", decisionPriorityEmoji(bead.Priority))
	if mentionUserID != "" {
		headerText += fmt.Sprintf(" <@%s>", mentionUserID)
	}
	headerText += "\n" + truncateRunes(question, decisionQuestionMax)
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", headerText, false, false),
			nil, nil,
		),
	}

	// Predecessor chain info.
	predecessorID := bead.Fields["predecessor_id"]
	if predecessorID != "" {
		// Resolve predecessor title for human-readable display.
		var predDisplay string
		if pred, err := b.daemon.GetBead(ctx, predecessorID); err == nil {
			predDisplay = b.beadTitleLink(predecessorID, pred.Title)
		} else {
			predDisplay = b.beadLink(predecessorID, predecessorID)
		}
		chainText := fmt.Sprintf("%s _Chained from: %s_", th.Semantic(SemLink), predDisplay)
		if iterStr := bead.Fields["iteration"]; iterStr != "" && iterStr != "1" {
			chainText = fmt.Sprintf("%s _Iteration %s — chained from: %s_", th.Semantic(SemLink), iterStr, predDisplay)
		}
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", chainText, false, false),
		))
	}

	// Decision context — additional background provided by the agent, its
	// code made Slack-safe (slackContextMrkdwn: it may quote an applicant).
	// Slack mrkdwn text blocks have a 3000-char limit; truncate with ellipsis.
	// Rune-aware: a byte slice can split a multi-byte rune, emitting invalid
	// UTF-8 that Slack rejects with invalid_blocks — dropping the whole decision
	// notification (same bug family as the escalation twin ka-0000000920 /
	// ka-0000006181). A rune-based 2900 is the correct reading of the limit.
	contextBlock := -1
	if decisionCtx := bead.Fields["context"]; decisionCtx != "" {
		decisionCtx = clampDecisionContext(slackContextMrkdwn(decisionCtx), b.beadLink(bead.ID, "Read the full context"))
		contextBlock = len(blocks)
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", decisionCtx, false, false),
				nil, nil))
	}

	// Evidence — the screenshots on the beads the decision is about, under
	// the context, as a line naming where they are (decisionEvidenceLine).
	if line := b.decisionEvidenceLine(ctx, bead); line != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", th.Semantic(SemLink)+" "+line, false, false)))
	}

	// Context block — skip entirely in threaded mode since the parent card shows it.
	if b.agentThreadingEnabled() && agent != "" {
		// No context block needed — the thread parent card provides agent context.
	} else if agent != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("Agent: %s | _%s_", agentDisplay, b.beadTitleLink(bead.ID, bead.Title)), false, false),
		))
	} else {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("_%s_", b.beadTitleLink(bead.ID, bead.Title)), false, false),
		))
	}

	// Option blocks — each option is a Section with accessory button.
	if len(optObjs) > 0 || len(optStrings) > 0 {
		blocks = append(blocks, slack.NewDividerBlock())

		if len(optObjs) > 0 {
			for i, opt := range optObjs {
				// Build display: prefer Short, fall back to Label, then ID.
				title := opt.Short
				if title == "" {
					title = opt.Label
				}
				if title == "" {
					title = opt.ID
				}

				// Show Label as description if it differs from the title.
				desc := opt.Label
				if desc == title {
					desc = ""
				}

				var optText string
				if desc != "" {
					optText = fmt.Sprintf("*%d. %s*\n%s", i+1, title, desc)
				} else {
					optText = fmt.Sprintf("*%d. %s*", i+1, title)
				}
				if opt.Description != "" {
					// Rune-aware truncation (see context block above): a byte
					// slice can split a multi-byte rune into invalid UTF-8 that
					// Slack rejects with invalid_blocks.
					optText += fmt.Sprintf("\n%s", truncateRunes(opt.Description, 150))
				}
				if opt.ArtifactType != "" {
					optText += fmt.Sprintf("\n_Requires: %s_", opt.ArtifactType)
				}

				buttonLabel := "Choose"
				if len(optObjs) <= 4 {
					buttonLabel = fmt.Sprintf("Choose %d", i+1)
				}

				blocks = append(blocks,
					slack.NewSectionBlock(
						slack.NewTextBlockObject("mrkdwn", truncateRunes(optText, decisionOptionTextMax), false, false),
						nil,
						slack.NewAccessory(
							slack.NewButtonBlockElement(
								fmt.Sprintf("resolve_%s_%d", bead.ID, i+1),
								fmt.Sprintf("%s:%d", bead.ID, i+1),
								slack.NewTextBlockObject("plain_text", buttonLabel, false, false)))))
			}
		} else {
			for i, opt := range optStrings {
				optText := truncateRunes(fmt.Sprintf("*%d. %s*", i+1, opt), decisionOptionTextMax)

				buttonLabel := "Choose"
				if len(optStrings) <= 4 {
					buttonLabel = fmt.Sprintf("Choose %d", i+1)
				}

				blocks = append(blocks,
					slack.NewSectionBlock(
						slack.NewTextBlockObject("mrkdwn", optText, false, false),
						nil,
						slack.NewAccessory(
							slack.NewButtonBlockElement(
								fmt.Sprintf("resolve_%s_%d", bead.ID, i+1),
								fmt.Sprintf("%s:%d", bead.ID, i+1),
								slack.NewTextBlockObject("plain_text", buttonLabel, false, false)))))
			}
		}

		// "Other" option — own section with accessory button.
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn",
					"*Other*\n_None of the above? Provide a custom response and choose the required artifact type._", false, false),
				nil,
				slack.NewAccessory(
					slack.NewButtonBlockElement(
						fmt.Sprintf("resolve_other_%s", bead.ID),
						bead.ID,
						slack.NewTextBlockObject("plain_text", "Other...", false, false)))))
	}

	// The bead the decision asks for (its requesting_task): its run joins the
	// header line below, and the plan it is or belongs to gets the map button.
	subject := b.cardSubjectBead(ctx, bead.Fields["requesting_task"])

	// Action buttons: Dismiss and Promote at the bottom, and "Open on the map"
	// when the decision points at a plan (plan_map_link.go).
	dismissBtn := slack.NewButtonBlockElement("dismiss_decision", bead.ID,
		slack.NewTextBlockObject("plain_text", "Dismiss", false, false))
	promoteBtn := slack.NewButtonBlockElement("promote_decision", bead.ID,
		slack.NewTextBlockObject("plain_text", "Escalate (Agent)", false, false))
	actionBtns := []slack.BlockElement{dismissBtn, promoteBtn}
	if b.beadsViewerURL != "" {
		if mapBtn := planMapButton(b.beadsViewerURL, b.cardPlan(ctx, bead.Fields["requesting_plan"], subject)); mapBtn != nil {
			actionBtns = append(actionBtns, mapBtn)
		}
	}
	blocks = append(blocks,
		slack.NewActionBlock("", actionBtns...))

	// Build message options.
	fallbackText := fmt.Sprintf("Decision needed: %s", question)
	if mentionUserID != "" {
		fallbackText = fmt.Sprintf("Decision needed (<@%s>): %s", mentionUserID, question)
	}
	// Resolve target channel for this agent.
	targetChannel := agentChannel

	// Thread under agent card, slack thread, or predecessor decision.
	var threadTS string
	var threadSource string

	// Check if this agent is thread-bound (spawned from a Slack thread).
	if agent != "" {
		if slackChannel, slackTS := b.resolveAgentThread(ctx, agent); slackChannel != "" && slackTS != "" {
			targetChannel = slackChannel
			threadTS = slackTS
			threadSource = "slack_thread"
		}
	}

	// Fallback: if Assignee didn't resolve to a thread-bound agent, try the
	// requesting_agent_bead_id field which gb decision create always sets.
	if threadSource == "" {
		if reqAgentBeadID := bead.Fields["requesting_agent_bead_id"]; reqAgentBeadID != "" {
			if agentBead, err := b.daemon.GetBead(ctx, reqAgentBeadID); err == nil {
				// Always update agent identity from requesting bead so that
				// agent-card threading (below) can find the correct card.
				if agentBead.Fields["agent"] != "" {
					agent = extractAgentName(agentBead.Fields["agent"])
				}
				ch := agentBead.Fields["slack_thread_channel"]
				ts := agentBead.Fields["slack_thread_ts"]
				if isValidThreadBinding(ch, ts) {
					targetChannel = ch
					threadTS = ts
					threadSource = "slack_thread"
				} else if agent != assignee {
					// The card threads under the requesting agent's card,
					// so it goes to that agent's channel, not the one
					// resolved for the assignee.
					targetChannel = b.resolveCardChannel(ctx, agent, "decision")
				}
			}
		}
	}

	if threadSource == "" && b.agentThreadingEnabled() && agent != "" {
		// Skip card creation for thread-bound agents: if both resolveAgentThread
		// and bead-field lookups failed to find thread metadata, check the cached
		// flag to avoid creating a stray top-level card.
		b.mu.Lock()
		threadBound := b.isThreadBound(extractAgentName(agent))
		b.mu.Unlock()
		if !threadBound {
			// Agent threading mode: thread under the agent's status card.
			cardTS, err := b.ensureAgentCard(ctx, agent, targetChannel)
			if err != nil {
				b.logger.Error("failed to ensure agent card", "agent", agent, "error", err)
				// Fall through to flat posting.
			} else {
				threadTS = cardTS
				threadSource = "agent_card"
			}
		}
	}

	if threadSource == "" && agent != "" {
		b.logger.Warn("decision falling through to flat channel post",
			"bead", bead.ID, "agent", agent, "channel", targetChannel,
			"threading_enabled", b.agentThreadingEnabled())
	}

	// Predecessor threading (within the agent thread or top-level).
	if predecessorID != "" {
		if ref, ok := b.lookupMessage(predecessorID); ok && ref.ChannelID == targetChannel {
			// In agent mode, predecessor still chains within the thread.
			// In flat mode, predecessor creates the thread.
			if threadTS == "" {
				threadTS = ref.Timestamp
				threadSource = "predecessor"
			}
		}
	}

	// DM-FIRST DELIVERY (operator ruling 2026-08-27; see bot_decisions_dm.go).
	// When the decision is addressed to a resolvable human, the ANSWERABLE card
	// goes to their DM and the channel/thread gets a one-line pointer registered
	// as a mirror, so exactly one copy carries buttons while the channel still
	// shows the outcome. Any failure falls through to the channel post below,
	// unchanged — a decision that cannot be DMed must still be asked.
	//
	// Both copies are cards of the decision (decision_cards.go): the DM card is
	// recorded on its own card bead, in the Commons DM channel between the
	// requesting agent and the addressee (dm_channels.go), and the pointer is
	// the Slack face of the channel's card, which the Commons renders as the
	// whole decision.
	// The run and short title of the bead the decision asks for, when it
	// bears arms (heraldry_cards.go), on the header line of both copies.
	blocks = b.withSubjectRun(blocks, subject)

	land, author := b.decisionLand(bead, agent), agentPrincipal(agent)
	primaryChannel, primaryTS, primaryThreadTS := "", "", threadTS
	if dmRef, ok := b.deliverDecisionToDM(ctx, bead.ID, mentionUserID, fallbackText, blocks); ok {
		primaryChannel, primaryTS, primaryThreadTS = dmRef.ChannelID, dmRef.Timestamp, ""
		b.recordDecisionDMCard(ctx, land, bead.ID, author, mentionUserID, dmRef, fallbackText)
		b.postDecisionPointer(ctx, land, bead.ID, targetChannel, threadTS, mentionUserID, question)
		threadSource = "operator_dm"
	} else {
		desk := b.decisionFromDesk(ctx, bead, agent)
		ref, err := b.postDecisionCard(ctx, land, bead.ID, author, targetChannel, threadTS, fallbackText, blocks, contextBlock, desk)
		if err != nil {
			return fmt.Errorf("post decision to Slack: %w", err)
		}
		primaryChannel, primaryTS = ref.ChannelID, ref.Timestamp
	}
	channelID, ts := primaryChannel, primaryTS

	// Track the message and update pending count.
	ref := MessageRef{ChannelID: channelID, Timestamp: ts, Agent: agent, ThreadTS: primaryThreadTS}
	b.mu.Lock()
	b.messages[bead.ID] = ref
	if b.agentThreadingEnabled() && agent != "" {
		b.agentPending[agent]++
	}
	if agent != "" {
		b.agentSeen[agent] = time.Now()
	}
	b.mu.Unlock()

	if b.state != nil {
		_ = b.state.SetDecisionMessage(bead.ID, ref)
	}

	// Update agent card with new pending count.
	if threadSource == "agent_card" {
		b.updateAgentCard(ctx, agent)
	}

	// Mark predecessor as superseded if we threaded under it (flat mode only).
	if threadSource == "predecessor" {
		b.markDecisionSuperseded(ctx, predecessorID, bead.ID, targetChannel, threadTS)
	}

	b.logger.Info("posted decision to Slack",
		"bead", bead.ID, "channel", channelID, "ts", ts,
		"thread_source", threadSource, "predecessor", predecessorID)
	return nil
}

// UpdateDecision edits the Slack message to show resolved state.
// Called via SSE close event. The modal submission handler may have already
// updated the message directly, so this serves as a fallback.
func (b *Bot) UpdateDecision(ctx context.Context, beadID, chosen, rationale string) error {
	b.updateMessageResolved(ctx, beadID, chosen, rationale, "", "")
	return nil
}

// NotifyEscalation posts a highlighted notification for an escalated decision.
func (b *Bot) NotifyEscalation(ctx context.Context, bead BeadEvent) error {
	question := decisionQuestion(bead.Fields)
	agent := extractAgentName(bead.Assignee)
	agentDisplay := b.agentDisplayName(agent)
	targetChannel := b.resolveCardChannel(ctx, agent, "escalated decision")
	th := b.themeFor(ctx, targetChannel)

	// Resolve who to @mention.
	mentionUserID := b.resolveDecisionMentionUser(ctx, bead.Assignee)

	text := fmt.Sprintf("%s *ESCALATED: %s*", th.BeadType["escalation"], b.beadTitleLink(bead.ID, bead.Title))
	if mentionUserID != "" {
		text += fmt.Sprintf(" <@%s>", mentionUserID)
	}
	text += "\n" + question

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil),
	}

	// Add context — skip agent info in threaded mode since the parent card shows it.
	contextParts := []string{fmt.Sprintf("_%s_", b.beadTitleLink(bead.ID, bead.Title))}
	if agent != "" && !b.agentThreadingEnabled() {
		contextParts = append([]string{fmt.Sprintf("Agent: %s", agentDisplay)}, contextParts...)
	}
	if requestedBy := bead.Fields["requested_by"]; requestedBy != "" {
		contextParts = append(contextParts, fmt.Sprintf("Requested by: %s", requestedBy))
	}
	blocks = append(blocks, slack.NewContextBlock("",
		slack.NewTextBlockObject("mrkdwn", strings.Join(contextParts, " | "), false, false)))

	escalationFallback := fmt.Sprintf("ESCALATED: %s — %s", b.beadTitleLink(bead.ID, bead.Title), question)
	if mentionUserID != "" {
		escalationFallback = fmt.Sprintf("ESCALATED (<@%s>): %s — %s", mentionUserID, b.beadTitleLink(bead.ID, bead.Title), question)
	}
	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(escalationFallback, false),
		slack.MsgOptionBlocks(blocks...),
	}

	// Thread-bound agents: post in the originating Slack thread.
	threadResolved := false
	if agent != "" {
		if slackChannel, slackTS := b.resolveAgentThread(ctx, agent); slackChannel != "" && slackTS != "" {
			targetChannel = slackChannel
			msgOpts = append(msgOpts, slack.MsgOptionTS(slackTS))
			threadResolved = true
		}
	}
	// Fallback: try requesting_agent_bead_id if Assignee didn't resolve.
	if !threadResolved {
		if reqAgentBeadID := bead.Fields["requesting_agent_bead_id"]; reqAgentBeadID != "" {
			if agentBead, err := b.daemon.GetBead(ctx, reqAgentBeadID); err == nil {
				ch := agentBead.Fields["slack_thread_channel"]
				ts := agentBead.Fields["slack_thread_ts"]
				if isValidThreadBinding(ch, ts) {
					targetChannel = ch
					msgOpts = append(msgOpts, slack.MsgOptionTS(ts))
					threadResolved = true
				}
			}
		}
	}
	if !threadResolved && b.agentThreadingEnabled() && agent != "" {
		// Skip card creation for thread-bound agents to prevent stray top-level cards.
		b.mu.Lock()
		threadBound := b.isThreadBound(extractAgentName(agent))
		b.mu.Unlock()
		if !threadBound {
			// Agent threading mode: thread escalation under the agent's card.
			if cardTS, err := b.ensureAgentCard(ctx, agent, targetChannel); err == nil {
				msgOpts = append(msgOpts, slack.MsgOptionTS(cardTS))
			}
		}
	}

	_, _, err := b.api.PostMessageContext(ctx, targetChannel, msgOpts...)
	if err != nil {
		return fmt.Errorf("post escalation to Slack: %w", err)
	}

	b.logger.Info("posted escalation to Slack",
		"bead", bead.ID, "channel", targetChannel)
	return nil
}

// DismissDecision deletes the Slack message for an expired/dismissed decision.
func (b *Bot) DismissDecision(ctx context.Context, beadID string) error {
	ref, ok := b.lookupMessage(beadID)
	if !ok {
		// No primary — but mirrors can outlive it (the primary ref is dropped
		// on several paths that leave secondary copies alone), and a mirror
		// left behind renders an open decision that no longer exists.
		b.deleteMirrors(ctx, beadID)
		b.logger.Debug("no Slack message found for dismissed decision", "bead", beadID)
		return nil
	}

	_, _, err := b.api.DeleteMessageContext(ctx, ref.ChannelID, ref.Timestamp)
	if err != nil {
		return fmt.Errorf("delete dismissed decision from Slack: %w", err)
	}

	// Clean up tracking and update agent card.
	agent := extractAgentName(ref.Agent)
	b.mu.Lock()
	delete(b.messages, beadID)
	if b.agentThreadingEnabled() && agent != "" {
		if b.agentPending[agent] > 0 {
			b.agentPending[agent]--
		}
	}
	b.mu.Unlock()

	if b.state != nil {
		_ = b.state.RemoveDecisionMessage(beadID)
	}

	if b.agentThreadingEnabled() && agent != "" {
		b.updateAgentCard(ctx, agent)
	}

	// Delete every mirror too (decision_message_mirrors.go) — a mirror left
	// behind is a live-looking decision card for a bead that no longer exists.
	b.deleteMirrors(ctx, beadID)

	b.logger.Info("dismissed decision from Slack",
		"bead", beadID, "channel", ref.ChannelID)
	return nil
}

// reportEmoji returns the theme's glyph for the given artifact/report type.
func reportEmoji(th Theme, reportType string) string {
	switch reportType {
	case "plan":
		return th.BeadType["plan"]
	case "checklist":
		return th.Semantic(SemOK)
	case "diff-summary":
		return th.Semantic(SemReview)
	case "epic":
		return th.BeadType["epic"]
	case "bug":
		return th.BeadType["bug"]
	case "tldr":
		return th.Semantic(SemNote)
	default: // bundle and any unrecognised type
		return th.Semantic(SemReport)
	}
}

// slackSectionTextMax is the maximum character length for a text object in a
// Slack Block Kit section block. Slack returns "invalid_blocks" if exceeded.
const slackSectionTextMax = 3000

// decisionContextMax is the decision card's context section budget, the
// section limit less headroom (the same 2900 the link passes hold a section
// text to, slack_link_hydrate.go).
const decisionContextMax = 2900

// decisionQuestionMax and decisionOptionTextMax bound the question in the
// header section and each option's section text. Both are far over any real
// question or option; they only keep a runaway field from costing the whole
// card (a section over 3000 chars is invalid_blocks).
const (
	decisionQuestionMax   = 1500
	decisionOptionTextMax = 1500
)

// clampDecisionContext fits a decision's context into its section. Context
// that fits is returned unchanged; longer context is cut on a rune boundary (a
// byte cut can split a rune into invalid UTF-8, which Slack rejects with
// invalid_blocks, ka-0000000920) and ends with fullLink, so the cut is honest
// and the reader can open the rest (gd-fthcee65T7: gd-Jsbg1Re436 carried 5k
// chars of context).
func clampDecisionContext(decisionCtx, fullLink string) string {
	return fitSlackContext(decisionCtx, decisionContextMax, "\n_… context cut for Slack. "+fullLink+"_")
}

// reportContentBlock builds a section block for report content. If the content
// (wrapped in code fences) would exceed Slack's 3000-char section text limit,
// it is truncated with a reference to the full report bead.
func reportContentBlock(content, reportID string) *slack.SectionBlock {
	// Code fences add 8 chars: "```\n" + "\n```"
	const fenceOverhead = 8
	maxContent := slackSectionTextMax - fenceOverhead

	text := content
	if len(text) > maxContent {
		// Leave room for the truncation notice. Slice on a rune boundary — a
		// byte slice can split a multi-byte rune into invalid UTF-8 that Slack
		// rejects with invalid_blocks (see NotifyDecision context block).
		suffix := fmt.Sprintf("\n\n… [truncated — run `kd show %s` for full report]", reportID)
		keep := maxContent - len(suffix)
		if r := []rune(text); len(r) > keep {
			text = string(r[:keep])
		}
		text += suffix
	}

	return slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn",
			fmt.Sprintf("```\n%s\n```", text), false, false),
		nil, nil)
}

// PostReport inlines the report into the resolved decision message.
// For long reports (>2000 chars), creates a Slack Canvas document with the
// full content and posts a link + preview instead of truncating.
// If the original message was deleted, falls back to posting the report as a
// new message in the same thread or channel.
func (b *Bot) PostReport(ctx context.Context, reportID, decisionID, reportType, content string) error {
	ref, ok := b.lookupMessage(decisionID)
	if !ok {
		b.logger.Debug("no Slack message found for report's decision", "decision", decisionID)
		return nil
	}

	// Fetch the decision bead to get its priority, title, and tldr limit.
	priority := 2 // default: normal
	decisionTitle := decisionID
	var decFields map[string]string
	if dec, err := b.daemon.GetBead(ctx, decisionID); err == nil {
		priority = dec.Priority
		decisionTitle = b.beadTitleLink(decisionID, dec.Title)
		decFields = dec.Fields
	}

	emoji := decisionPriorityEmoji(priority) + " " + reportEmoji(b.themeFor(ctx, ref.ChannelID), reportType)

	// tldr reports render as short-form inline (blockquote) and NEVER hit the
	// Canvas path — the length validator in `gb decision report` already
	// guaranteed the content fits. Routing tldr to canvas would defeat the
	// purpose of the short-form format.
	if reportType == "tldr" {
		maxChars := tldrMaxCharsFromFields(decFields)
		return b.postTldrInline(ctx, ref, reportID, decisionID, decisionTitle, content, emoji, maxChars)
	}

	// For long content, create a Canvas document and post a link instead
	// of inlining (which would truncate at 3000 chars).
	if shouldUseCanvas(content) && b.canvas != nil {
		return b.postReportAsCanvas(ctx, ref, reportID, decisionID, decisionTitle, reportType, content, emoji)
	}

	// Short content: inline directly in the decision message.
	return b.postReportInline(ctx, ref, reportID, decisionID, decisionTitle, reportType, content, emoji)
}

// tldrMaxCharsFromFields looks up the max_chars limit for the decision's chosen
// tldr option. Mirrors the parsing logic in `gb decision report`. Defaults to
// 500 when options, chosen, or artifact_params.max_chars are missing or invalid.
func tldrMaxCharsFromFields(fields map[string]string) int {
	const defaultMax = 500
	if fields == nil {
		return defaultMax
	}
	optionsJSON := fields["options"]
	chosenID := fields["chosen"]
	if optionsJSON == "" {
		return defaultMax
	}
	var opts []map[string]any
	if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
		return defaultMax
	}
	for _, opt := range opts {
		id, _ := opt["id"].(string)
		if id != chosenID {
			continue
		}
		params, _ := opt["artifact_params"].(map[string]any)
		if params == nil {
			return defaultMax
		}
		if v, ok := params["max_chars"].(float64); ok && v > 0 {
			return int(v)
		}
		return defaultMax
	}
	return defaultMax
}

// tldrBlockquote prefixes every line of s with "> " so it renders as a Slack
// blockquote. An empty input yields "> " (an empty quote), which Slack renders
// as a single quoted blank line.
func tldrBlockquote(s string) string {
	if s == "" {
		return "> "
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// buildTldrBlocks constructs the Slack Block Kit payload for a tldr report:
// header section, blockquote body section, and a context footer. No divider,
// no code fence, no Canvas link — tldr is always short-form inline.
func buildTldrBlocks(decisionTitle, content, emoji string, maxChars int) []slack.Block {
	header := fmt.Sprintf("%s *TL;DR* — %s", emoji, decisionTitle)
	body := tldrBlockquote(content)
	contextLine := fmt.Sprintf("_Decision: %s · %d / %d chars_", decisionTitle, len(content), maxChars)
	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", header, false, false),
			nil, nil),
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", body, false, false),
			nil, nil),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", contextLine, false, false)),
	}
}

// postTldrInline appends the tldr blocks to the original decision message.
// If the original message can't be fetched or updated, falls back to posting a
// new thread reply. Never calls the Canvas path.
func (b *Bot) postTldrInline(ctx context.Context, ref MessageRef, reportID, decisionID, decisionTitle, content, emoji string, maxChars int) error {
	newBlocks := buildTldrBlocks(decisionTitle, content, emoji, maxChars)

	existing, messageFound := b.fetchDecisionMessage(ctx, ref, decisionID)
	if !messageFound {
		return b.postTldrFallback(ctx, ref, decisionID, decisionTitle, content, emoji, maxChars)
	}

	var allBlocks []slack.Block
	if len(existing.Blocks.BlockSet) > 0 {
		allBlocks = append(existing.Blocks.BlockSet, newBlocks...)
	} else {
		allBlocks = append([]slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", existing.Text, false, false),
				nil, nil),
		}, newBlocks...)
	}

	_, _, _, updateErr := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
		slack.MsgOptionBlocks(allBlocks...),
	)
	if updateErr != nil {
		b.logger.Warn("failed to inline tldr, falling back to new message",
			"decision", decisionID, "error", updateErr)
		return b.postTldrFallback(ctx, ref, decisionID, decisionTitle, content, emoji, maxChars)
	}
	return nil
}

// postTldrFallback posts the tldr as a new thread reply when the original
// decision message is unavailable.
func (b *Bot) postTldrFallback(ctx context.Context, ref MessageRef, decisionID, decisionTitle, content, emoji string, maxChars int) error {
	blocks := buildTldrBlocks(decisionTitle, content, emoji, maxChars)
	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(fmt.Sprintf("TL;DR for decision %s", decisionTitle), false),
		slack.MsgOptionBlocks(blocks...),
	}
	threadTS := ref.ThreadTS
	if threadTS == "" {
		threadTS = ref.Timestamp
	}
	if threadTS != "" {
		msgOpts = append(msgOpts, slack.MsgOptionTS(threadTS))
	}
	if _, _, err := b.api.PostMessageContext(ctx, ref.ChannelID, msgOpts...); err != nil {
		return fmt.Errorf("post tldr fallback: %w", err)
	}
	b.logger.Info("posted tldr as fallback message",
		"decision", decisionID, "channel", ref.ChannelID)
	return nil
}

// postReportFallback posts a report as a new message when the original decision
// message is unavailable (deleted, expired, etc.).
func (b *Bot) postReportFallback(ctx context.Context, ref MessageRef, reportID, decisionID, decisionTitle, reportType, content, emoji string) error {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("%s *Report (%s)* — Decision: _%s_", emoji, reportType, decisionTitle), false, false),
			nil, nil),
		slack.NewDividerBlock(),
		reportContentBlock(content, reportID),
	}

	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(fmt.Sprintf("Report (%s) for decision %s", reportType, decisionTitle), false),
		slack.MsgOptionBlocks(blocks...),
	}

	// Thread under the original thread if available.
	threadTS := ref.ThreadTS
	if threadTS == "" {
		threadTS = ref.Timestamp
	}
	if threadTS != "" {
		msgOpts = append(msgOpts, slack.MsgOptionTS(threadTS))
	}

	_, _, err := b.api.PostMessageContext(ctx, ref.ChannelID, msgOpts...)
	if err != nil {
		return fmt.Errorf("post report fallback: %w", err)
	}

	b.logger.Info("posted report as fallback message",
		"decision", decisionID, "report_type", reportType, "channel", ref.ChannelID)
	return nil
}

// postReportAsCanvas creates a Canvas document for the report and posts a
// link + preview in the decision thread. Falls back to inline posting if
// Canvas creation fails.
func (b *Bot) postReportAsCanvas(ctx context.Context, ref MessageRef, reportID, decisionID, decisionTitle, reportType, content, emoji string) error {
	title := fmt.Sprintf("Report (%s) — %s", reportType, decisionTitle)
	canvasID, err := b.canvas.CreateReportCanvas(ctx, title, reportType, content)
	if err != nil {
		b.logger.Warn("canvas: failed to create, falling back to inline report",
			"decision", decisionID, "error", err)
		// Fall back to the standard inline flow (will truncate).
		return b.postReportInline(ctx, ref, reportID, decisionID, decisionTitle, reportType, content, emoji)
	}

	// Best-effort: grant channel read access so thread participants can view it.
	if err := b.canvas.SetCanvasChannelAccess(ctx, canvasID, ref.ChannelID); err != nil {
		b.logger.Debug("canvas: failed to set channel access (non-fatal)",
			"canvas_id", canvasID, "channel", ref.ChannelID, "error", err)
	}

	// Build blocks with Canvas link and content preview.
	preview := firstLines(content, 500)
	blocks := canvasReportBlocks(emoji, reportType, decisionTitle, canvasID, preview)

	// Try to append to the existing decision message.
	existing, messageFound := b.fetchDecisionMessage(ctx, ref, decisionID)
	if messageFound {
		var allBlocks []slack.Block
		if len(existing.Blocks.BlockSet) > 0 {
			allBlocks = append(existing.Blocks.BlockSet, blocks...)
		} else {
			allBlocks = append([]slack.Block{
				slack.NewSectionBlock(
					slack.NewTextBlockObject("mrkdwn", existing.Text, false, false),
					nil, nil),
			}, blocks...)
		}

		_, _, _, updateErr := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
			slack.MsgOptionBlocks(allBlocks...),
		)
		if updateErr == nil {
			b.logger.Info("posted canvas report in decision message",
				"decision", decisionID, "canvas_id", canvasID, "channel", ref.ChannelID)
			return nil
		}
		b.logger.Warn("canvas: failed to update decision message, posting as new message",
			"decision", decisionID, "error", updateErr)
	}

	// Fallback: post as a new thread reply with Canvas link.
	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(fmt.Sprintf("Report (%s) for decision %s", reportType, decisionTitle), false),
		slack.MsgOptionBlocks(blocks...),
	}
	threadTS := ref.ThreadTS
	if threadTS == "" {
		threadTS = ref.Timestamp
	}
	if threadTS != "" {
		msgOpts = append(msgOpts, slack.MsgOptionTS(threadTS))
	}
	_, _, err = b.api.PostMessageContext(ctx, ref.ChannelID, msgOpts...)
	if err != nil {
		return fmt.Errorf("post canvas report: %w", err)
	}

	b.logger.Info("posted canvas report as thread reply",
		"decision", decisionID, "canvas_id", canvasID, "channel", ref.ChannelID)
	return nil
}

// postReportInline is the original inline report posting logic, used as a
// fallback when Canvas creation fails.
func (b *Bot) postReportInline(ctx context.Context, ref MessageRef, reportID, decisionID, decisionTitle, reportType, content, emoji string) error {
	existing, messageFound := b.fetchDecisionMessage(ctx, ref, decisionID)
	if !messageFound {
		return b.postReportFallback(ctx, ref, reportID, decisionID, decisionTitle, reportType, content, emoji)
	}

	var blocks []slack.Block
	if len(existing.Blocks.BlockSet) > 0 {
		blocks = existing.Blocks.BlockSet
	} else {
		blocks = []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", existing.Text, false, false),
				nil, nil),
		}
	}

	blocks = append(blocks, slack.NewDividerBlock())
	blocks = append(blocks,
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("%s *Report (%s)*", emoji, reportType), false, false),
			nil, nil))
	blocks = append(blocks,
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("Decision _%s_ · %s", decisionTitle, reportType), false, false)))
	blocks = append(blocks, reportContentBlock(content, reportID))

	_, _, _, updateErr := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
		slack.MsgOptionBlocks(blocks...),
	)
	if updateErr != nil {
		b.logger.Warn("failed to inline report, falling back to new message",
			"decision", decisionID, "error", updateErr)
		return b.postReportFallback(ctx, ref, reportID, decisionID, decisionTitle, reportType, content, emoji)
	}
	return nil
}

// fetchDecisionMessage retrieves the existing Slack message for a decision.
// Returns the message and true if found, or an empty message and false if not.
func (b *Bot) fetchDecisionMessage(ctx context.Context, ref MessageRef, decisionID string) (slack.Message, bool) {
	if ref.ThreadTS != "" {
		replies, _, _, err := b.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: ref.ChannelID,
			Timestamp: ref.ThreadTS,
			Oldest:    ref.Timestamp,
			Latest:    ref.Timestamp,
			Inclusive: true,
			Limit:     1,
		})
		if err != nil {
			b.logger.Warn("fetch decision message from thread failed",
				"decision", decisionID, "error", err)
			return slack.Message{}, false
		}
		for _, m := range replies {
			if m.Timestamp == ref.Timestamp {
				return m, true
			}
		}
		return slack.Message{}, false
	}

	msgs, err := b.api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: ref.ChannelID,
		Latest:    ref.Timestamp,
		Inclusive: true,
		Limit:     1,
	})
	if err != nil {
		b.logger.Warn("fetch decision message failed",
			"decision", decisionID, "error", err)
		return slack.Message{}, false
	}
	if len(msgs.Messages) > 0 && msgs.Messages[0].Timestamp == ref.Timestamp {
		return msgs.Messages[0], true
	}
	return slack.Message{}, false
}

// firstLines returns the first n characters of s, breaking at a newline if possible.
func firstLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Try to break at a newline.
	for i := n; i > n/2; i-- {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s[:n]
}

// postDecisionCard posts a decision's card (postSubjectCardBead). When Slack
// refuses the blocks, it logs Slack's own reason, which names the bad block
// (slackBlockRenderError), and posts the card once more without the context
// section at contextBlock (-1: none), ending with a link to the full decision
// instead. Long agent-written context is the part of the card most likely to
// cross a limit, and a card without it still carries every button: the
// decision must still be asked (gd-fthcee65T7). The retry goes to Slack
// directly and is stamped on the card bead the Router already wrote, as the
// escalation card's text retry is.
//
// machine is postSubjectCardBead's: a decision the merge desk asks
// (decisionFromDesk) has its Commons copy in #machines.
func (b *Bot) postDecisionCard(ctx context.Context, land, subject string, author conversation.Principal, slackChannel, threadTS, text string, blocks []slack.Block, contextBlock int, machine bool) (MessageRef, error) {
	ref, cardBead, err := b.postSubjectCardBead(ctx, land, subject, author, slackChannel, threadTS, text, blocks, machine)
	if err == nil {
		return ref, nil
	}
	detail, isBlockErr := slackBlockRenderError(err)
	if !isBlockErr || contextBlock < 0 || contextBlock >= len(blocks) {
		return MessageRef{}, err
	}
	b.logger.Error("decision blocks rejected by Slack, retrying without the context section",
		"id", subject, "channel", slackChannel, "detail", detail)
	compact := append([]slack.Block{}, blocks[:contextBlock]...)
	compact = append(compact, slack.NewContextBlock("",
		slack.NewTextBlockObject("mrkdwn", "_Context too long for Slack: "+b.beadLink(subject, "read it on the decision")+"_", false, false)))
	compact = append(compact, blocks[contextBlock+1:]...)
	opts := []slack.MsgOption{slack.MsgOptionText(text, false), slack.MsgOptionBlocks(compact...)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	channelID, ts, err := b.api.PostMessageContext(ctx, slackChannel, opts...)
	if err != nil {
		return MessageRef{}, err
	}
	if channelID == "" {
		channelID = slackChannel
	}
	if cardBead != "" && ts != "" {
		stamp := map[string]string{conversation.SurfaceSlack + "_channel": channelID, conversation.SurfaceSlack + "_ts": ts}
		if err := b.daemon.UpdateBeadFields(ctx, cardBead, stamp); err != nil {
			b.logger.Warn("card: decision retry copy not recorded on its card bead", "bead", cardBead, "error", err)
		}
	}
	return MessageRef{ChannelID: channelID, Timestamp: ts, ThreadTS: threadTS}, nil
}
