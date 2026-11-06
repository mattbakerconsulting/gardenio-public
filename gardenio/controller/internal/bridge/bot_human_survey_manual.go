// bot_human_survey_manual.go — Manual-trigger paths for the human exit
// survey and the "Add feedback" modal.
//
// Two manual-trigger paths post a fresh human_survey prompt into a Slack
// thread:
//
//   1. :bar_chart: reaction on a bot-authored message — handled in
//      bot_reactions.go, which delegates to handleBarChartReaction here.
//
//   2. @gardenio rate <agent-bead-id> mention — handled in bot_mentions.go,
//      which parses the command via parseRateCommand and delegates to
//      handleRateCommand here.
//
// Both paths run through postManualHumanSurveyPrompt, which dedups on the
// agent bead's `human_survey_prompt_ts` field (same field the agent-side
// stop flow uses). If an existing prompt timestamp is younger than the
// expire window, the new prompt is skipped.
//
// The "Add feedback" button on any human_survey prompt fires a
// block_actions event with action_id=humanSurveyFeedbackActionID; the bot
// opens a modal with a single multiline text input. Submission updates
// the matching human_survey bead's `feedback` field (XSS-escaped before
// being rendered back into Slack).
package bridge

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// humanSurveyManualExpireWindow is how long a posted human_survey prompt
// blocks a fresh manual trigger. Mirrors the agent-side
// HumanSurveyConfig.ExpireHours default of 168 hours (7 days).
const humanSurveyManualExpireWindow = 168 * time.Hour

// humanSurveyPromptTSField is the agent-bead field used for dedup. Kept in
// sync with gb/human_survey.go:humanSurveyPromptTSField.
const humanSurveyPromptTSField = "human_survey_prompt_ts"

// humanSurveyEmoji is the Slack reaction name that triggers a manual
// survey prompt. Without colons — Slack's reaction_added events use the
// bare name.
const humanSurveyEmoji = "bar_chart"

// humanSurveyRateKeyword is the first word of "@gardenio rate <bead-id>"
// mentions.
const humanSurveyRateKeyword = "rate"

// parseRateCommand recognizes "rate <agent-bead-id>" and returns the ID.
// Returns ("", false) if the text is not a rate command. The agent bead
// ID is the second whitespace-separated token. Any trailing tokens are
// ignored so users can add a note like "@gardenio rate ka-ABC please".
func parseRateCommand(text string) (string, bool) {
	words := strings.Fields(strings.TrimSpace(text))
	if len(words) < 2 {
		return "", false
	}
	if strings.ToLower(words[0]) != humanSurveyRateKeyword {
		return "", false
	}
	id := strings.TrimSpace(words[1])
	if id == "" {
		return "", false
	}
	return id, true
}

// handleRateCommand is the mention-path entry for "@gardenio rate <id>".
// It resolves the agent bead, then posts a fresh prompt (or no-ops on
// dedup).
func (b *Bot) handleRateCommand(ctx context.Context, ev *slackevents.AppMentionEvent, agentBeadID string) {
	channel := ev.Channel
	threadTS := ev.ThreadTimeStamp
	if threadTS == "" {
		threadTS = ev.TimeStamp
	}

	if _, err := b.daemon.GetBead(ctx, agentBeadID); err != nil {
		b.logger.Warn("rate: agent bead not found",
			"agent_bead_id", agentBeadID, "error", err)
		if b.api != nil {
			_, _ = b.api.PostEphemeral(channel, ev.User,
				slack.MsgOptionText(fmt.Sprintf(":x: Agent bead `%s` not found.", agentBeadID), false))
		}
		return
	}

	b.postManualHumanSurveyPrompt(ctx, agentBeadID, channel, threadTS, ev.User)
}

// handleBarChartReaction is the reaction-path entry for :bar_chart:. It
// resolves the agent bead from the thread the reacted-to message lives
// in, then posts a fresh prompt (or no-ops on dedup).
func (b *Bot) handleBarChartReaction(ctx context.Context, ev *slackevents.ReactionAddedEvent) {
	// Reactions on non-message items never have thread context.
	if ev.Item.Type != "message" {
		return
	}
	channel := ev.Item.Channel
	// Use the reacted-to message timestamp as the thread anchor; if the
	// message is itself a reply, Slack will thread under its own root.
	threadTS := ev.Item.Timestamp

	// Resolve agent bead via thread metadata.
	agentName := b.getAgentByThread(channel, threadTS)
	if agentName == "" {
		b.logger.Debug(":bar_chart: reaction: no agent bound to thread",
			"channel", channel, "ts", threadTS)
		return
	}

	agentBead, err := b.daemon.FindAgentBead(ctx, extractAgentName(agentName))
	if err != nil || agentBead == nil {
		b.logger.Debug(":bar_chart: reaction: agent bead not found",
			"agent", agentName, "error", err)
		return
	}

	b.postManualHumanSurveyPrompt(ctx, agentBead.ID, channel, threadTS, ev.User)
}

// postManualHumanSurveyPrompt dedups on the agent bead's
// human_survey_prompt_ts field, then posts a fresh block-kit rating
// prompt and persists the resulting message timestamp back on the agent
// bead. Best-effort: errors are logged and swallowed.
func (b *Bot) postManualHumanSurveyPrompt(ctx context.Context, agentBeadID, channel, threadTS, raterUserID string) {
	if channel == "" || threadTS == "" {
		b.logger.Debug("manual survey: missing thread coords",
			"agent_bead_id", agentBeadID, "channel", channel, "thread_ts", threadTS)
		return
	}

	agentBead, err := b.daemon.GetBead(ctx, agentBeadID)
	if err != nil || agentBead == nil {
		b.logger.Warn("manual survey: agent bead fetch failed",
			"agent_bead_id", agentBeadID, "error", err)
		return
	}

	if existing := agentBead.Fields[humanSurveyPromptTSField]; existing != "" {
		if !humanSurveyPromptExpired(existing, time.Now(), humanSurveyManualExpireWindow) {
			b.logger.Info("manual survey: existing non-expired prompt, skipping",
				"agent_bead_id", agentBeadID, "existing_ts", existing)
			return
		}
	}

	if b.api == nil {
		return
	}

	blocks := buildHumanSurveyBlocks(agentBeadID, -1, 1, "manual")
	fallback := fmt.Sprintf("Rate this agent session (%s)", agentBeadID)
	_, ts, err := b.api.PostMessage(channel,
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		b.logger.Error("manual survey: post prompt failed",
			"agent_bead_id", agentBeadID, "channel", channel, "thread_ts", threadTS, "error", err)
		return
	}

	if err := b.daemon.UpdateBeadFields(ctx, agentBeadID, map[string]string{
		humanSurveyPromptTSField: ts,
	}); err != nil {
		b.logger.Warn("manual survey: persist prompt_ts failed",
			"agent_bead_id", agentBeadID, "ts", ts, "error", err)
	}

	b.logger.Info("manual survey: prompt posted",
		"agent_bead_id", agentBeadID, "channel", channel, "thread_ts", threadTS,
		"prompt_ts", ts, "rater", raterUserID)
}

// humanSurveyPromptExpired reports whether a stored prompt timestamp (a
// Slack ts in "seconds.microseconds" form) is older than `expireWindow`
// relative to `now`. Malformed timestamps are treated as expired so a
// bogus value can never permanently block a fresh prompt.
func humanSurveyPromptExpired(ts string, now time.Time, expireWindow time.Duration) bool {
	secsStr, _, _ := strings.Cut(ts, ".")
	if secsStr == "" {
		secsStr = ts
	}
	var secs int64
	if _, err := fmt.Sscanf(secsStr, "%d", &secs); err != nil || secs <= 0 {
		return true
	}
	posted := time.Unix(secs, 0)
	return now.Sub(posted) >= expireWindow
}

// openFeedbackModal opens a Slack modal with a single multiline text
// input for freeform feedback. The modal's PrivateMetadata carries the
// agent bead ID, the reacted-from channel, and the thread ts so the
// submission handler can attach the feedback to the right human_survey
// bead and post a confirmation in the right thread.
func (b *Bot) openFeedbackModal(_ context.Context, agentBeadID string, callback slack.InteractionCallback) {
	if b.api == nil {
		return
	}
	titleText := slack.NewTextBlockObject("plain_text", "Add Feedback", false, false)
	submitText := slack.NewTextBlockObject("plain_text", "Submit", false, false)
	closeText := slack.NewTextBlockObject("plain_text", "Cancel", false, false)

	threadTS := callback.Message.ThreadTimestamp
	if threadTS == "" {
		threadTS = callback.Message.Timestamp
	}

	blocks := slack.Blocks{
		BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("*Feedback for agent session* `%s`", agentBeadID),
					false, false),
				nil, nil,
			),
			slack.NewInputBlock(
				"feedback",
				slack.NewTextBlockObject("plain_text", "Your feedback", false, false),
				nil,
				&slack.PlainTextInputBlockElement{
					Type:        slack.METPlainTextInput,
					ActionID:    "feedback_input",
					Multiline:   true,
					Placeholder: slack.NewTextBlockObject("plain_text", "What worked, what didn't…", false, false),
				},
			),
		},
	}

	// Metadata format: agentBeadID|channelID|threadTS
	metadata := fmt.Sprintf("%s|%s|%s", agentBeadID, callback.Channel.ID, threadTS)

	modal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           titleText,
		Submit:          submitText,
		Close:           closeText,
		Blocks:          blocks,
		PrivateMetadata: metadata,
		CallbackID:      humanSurveyFeedbackCallbackID,
	}

	if _, err := b.api.OpenView(callback.TriggerID, modal); err != nil {
		b.logger.Error("human-survey: open feedback modal failed",
			"agent_bead_id", agentBeadID, "error", err)
	}
}

// handleFeedbackSubmission processes the "Add feedback" modal submission.
// It writes the feedback (XSS-escaped) to the `type=rating` bead for this
// (agent_bead_id, rater) pair via writeRatingFromSlackClick, which upserts
// (creates the bead with score=0 if no prior rating exists for the rater),
// then posts a confirmation in the originating thread.
func (b *Bot) handleFeedbackSubmission(ctx context.Context, callback slack.InteractionCallback) {
	metadata := callback.View.PrivateMetadata
	parts := strings.SplitN(metadata, "|", 3)
	if len(parts) < 1 || parts[0] == "" {
		b.logger.Error("human-survey: malformed feedback modal metadata", "metadata", metadata)
		return
	}
	agentBeadID := parts[0]
	channel := ""
	threadTS := ""
	if len(parts) >= 3 {
		channel = parts[1]
		threadTS = parts[2]
	}

	rawFeedback := ""
	if v, ok := callback.View.State.Values["feedback"]["feedback_input"]; ok {
		rawFeedback = v.Value
	}
	rawFeedback = strings.TrimSpace(rawFeedback)
	if rawFeedback == "" {
		return
	}
	// XSS: escape HTML entities so Slack special syntax (<!channel>,
	// <@U123>, <#C456|name>, <http://…>) in user-submitted feedback is
	// never interpreted when the feedback is rendered back into Slack.
	escaped := slackEscapeMrkdwn(rawFeedback)

	rater := callback.User.ID
	now := time.Now().UTC()

	agentBead, err := b.daemon.GetBead(ctx, agentBeadID)
	if err != nil || agentBead == nil {
		b.logger.Warn("human-survey: feedback agent bead fetch failed",
			"agent_bead_id", agentBeadID, "error", err)
		return
	}

	b.writeRatingFromSlackClick(ctx, ratingWriteRequest{
		agentBead:     agentBead,
		agentBeadID:   agentBeadID,
		source:        "slack-feedback-only",
		slackChannel:  channel,
		slackThreadTS: threadTS,
		slackUserID:   rater,
		feedback:      escaped,
		feedbackRaw:   rawFeedback,
		answeredAt:    now,
	})
	b.postFeedbackConfirmation(channel, threadTS, agentBeadID, escaped)
	b.logger.Info("human-survey: feedback recorded",
		"agent_bead_id", agentBeadID, "rater", rater)
}

// postFeedbackConfirmation posts a threaded acknowledgement. Best-effort.
func (b *Bot) postFeedbackConfirmation(channel, threadTS, agentBeadID, escapedFeedback string) {
	if b.api == nil || channel == "" || threadTS == "" {
		return
	}
	text := fmt.Sprintf(":white_check_mark: Feedback recorded for `%s`.", agentBeadID)
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", "> "+escapedFeedback, false, false)),
	}
	if _, _, err := b.api.PostMessage(channel,
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(blocks...),
	); err != nil {
		b.logger.Warn("human-survey: post feedback confirmation failed",
			"channel", channel, "thread_ts", threadTS, "error", err)
	}
}

// slackEscapeMrkdwn escapes HTML special chars so user-supplied text
// can't be interpreted as Slack special syntax (<!channel>, <@Uxx>,
// <#Cxx|name>, <http://…>) when rendered in an mrkdwn block.
//
// Slack's mrkdwn parser only consumes `<…>` sequences, so escaping `<`
// and `>` is sufficient to neutralise all pings, channel mentions, and
// link autoformatting. We also escape `&` to keep the replacement
// reversible for any downstream consumer that HTML-unescapes the field.
func slackEscapeMrkdwn(s string) string {
	return html.EscapeString(s)
}
