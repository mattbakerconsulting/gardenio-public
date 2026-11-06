package bridge

// bot_human_survey.go — Human exit-survey plumbing.
//
// Two surfaces live here:
//
//  1. HumanSurveyAPI.RegisterRoutes registers POST /api/slack/human-survey/prompt,
//     which agents call from gb stop to ask the bridge to post a block-kit
//     rating prompt as a thread reply. The bridge posts the message and returns
//     the message timestamp, which the agent persists on the agent bead for
//     dedup.
//
//  2. Bot.handleHumanSurveyRate handles button clicks with action_id
//     "human_survey_rate_*" and value "<agent_bead_id>:<rating>". It creates
//     or updates a `human_survey` bead keyed on (agent_bead_id,
//     rater_slack_user), then rewrites the original message with a
//     confirmation.
//
// Lands dark: the agent side guards posting behind human-survey-config.enabled
// defaulting to false. Until the config is flipped on, no block-kit prompts
// are posted so the Slack-side handler is never reached.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// humanSurveyBeadType is the `type` field on human_survey beads.
const humanSurveyBeadType = "human_survey"

// humanSurveyAgentLabelPrefix labels human_survey beads with their parent
// agent bead id so they can be looked up cheaply via the list API.
const humanSurveyAgentLabelPrefix = "agent:"

// humanSurveyActionsBlockID is the block_id on the rating-button action
// block. Manual-triggered prompts suffix it with ":manual" so the rate
// handler can distinguish source.
const (
	humanSurveyActionsBlockID       = "human_survey_actions"
	humanSurveyActionsManualBlockID = "human_survey_actions:manual"
)

// humanSurveyFeedbackActionID is the action_id on the "Add feedback"
// button; its value is the agent bead ID.
const humanSurveyFeedbackActionID = "human_survey_feedback"

// humanSurveyFeedbackCallbackID is the CallbackID on the feedback modal.
const humanSurveyFeedbackCallbackID = "human_survey_feedback"

// HumanSurveyAPIClient is the subset of BeadClient this API needs.
type HumanSurveyAPIClient interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
}

// AgentCardResolver returns where an agent's status card lives in Slack.
// HumanSurveyAPI uses this to route surveys to the agent card's thread when
// the agent supplies no Slack thread coords (e.g. JIRA-spawned agents
// landing in fsm-triage via channel_roles — see ka-0000005949, ka-0000002021).
// Implementations must be safe for concurrent reads. nil is acceptable —
// when nil the bridge falls back to the agent-supplied channel.
type AgentCardResolver interface {
	AgentCardLocation(agent string) (channel, threadTS string, ok bool)
}

// HumanSurveyAPI serves the POST /api/slack/human-survey/prompt endpoint.
type HumanSurveyAPI struct {
	api      *slack.Client
	daemon   HumanSurveyAPIClient
	resolver AgentCardResolver
	logger   *slog.Logger
}

// NewHumanSurveyAPI creates a new HumanSurveyAPI handler. resolver may be
// nil (e.g. webhook-only mode where there is no Bot maintaining card state);
// in that case the bridge requires the caller to supply `channel` directly.
func NewHumanSurveyAPI(api *slack.Client, daemon HumanSurveyAPIClient, resolver AgentCardResolver, logger *slog.Logger) *HumanSurveyAPI {
	return &HumanSurveyAPI{api: api, daemon: daemon, resolver: resolver, logger: logger}
}

// RegisterRoutes registers human-survey routes on the given mux.
func (a *HumanSurveyAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/slack/human-survey/prompt", a.handlePostPrompt)
}

// humanSurveyPromptRequest mirrors the agent-side request payload.
type humanSurveyPromptRequest struct {
	AgentBeadID    string `json:"agent_bead_id"`
	Channel        string `json:"channel"`
	ThreadTS       string `json:"thread_ts"`
	RatingScaleMax int    `json:"rating_scale_max"`
	MinRating      int    `json:"min_rating"`
	AskFeedback    bool   `json:"ask_feedback"`
}

// humanSurveyPromptResponse is the bridge's reply to the agent.
type humanSurveyPromptResponse struct {
	OK        bool   `json:"ok"`
	Timestamp string `json:"timestamp"`
}

// handlePostPrompt posts a block-kit rating prompt as a thread reply and
// returns the message timestamp.
func (a *HumanSurveyAPI) handlePostPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req humanSurveyPromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.AgentBeadID == "" {
		http.Error(w, `{"error":"agent_bead_id is required"}`, http.StatusBadRequest)
		return
	}
	// When the agent supplied no channel (JIRA-spawned and other non-thread
	// agents have no Slack thread coords — see ka-0000005949, ka-0000002021),
	// route the survey to the agent card's location so it lands in the same
	// channel/thread the operator already associates with the agent (e.g.
	// fsm-triage agents → fsm-triage channel via channel_roles routing in
	// resolveChannel).
	if req.Channel == "" && a.resolver != nil {
		if ch, ts, ok := a.resolver.AgentCardLocation(req.AgentBeadID); ok {
			req.Channel = ch
			if req.ThreadTS == "" {
				req.ThreadTS = ts
			}
		}
	}
	if req.Channel == "" {
		http.Error(w, `{"error":"channel is required (and no agent card found to resolve)"}`, http.StatusBadRequest)
		return
	}
	// thread_ts remains optional — survey may be posted non-threaded when no
	// thread context is available.
	// Defaults match DefaultHumanSurveyConfig on the agent side: a −1/0/+1
	// scale. RatingScaleMax==0 means "agent did not specify"; RatingScaleMax
	// of zero is also a degenerate range we never want to render. MinRating
	// of zero is ambiguous (could be "agent did not specify" or "explicitly
	// non-negative"), so we only fill in -1 when the max also looks unset.
	if req.RatingScaleMax <= 0 {
		req.RatingScaleMax = 1
		if req.MinRating == 0 {
			req.MinRating = -1
		}
	}

	blocks := buildHumanSurveyBlocks(req.AgentBeadID, req.MinRating, req.RatingScaleMax, "stop")
	fallback := fmt.Sprintf("Rate this agent session (%s)", req.AgentBeadID)

	opts := []slack.MsgOption{
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(blocks...),
	}
	if req.ThreadTS != "" {
		opts = append(opts, slack.MsgOptionTS(req.ThreadTS))
	}
	_, ts, err := a.api.PostMessage(req.Channel, opts...)
	if err != nil {
		a.logger.Error("failed to post human-survey prompt",
			"agent", req.AgentBeadID, "channel", req.Channel, "thread_ts", req.ThreadTS, "error", err)
		http.Error(w, `{"error":"failed to post prompt"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(humanSurveyPromptResponse{OK: true, Timestamp: ts})
}

// buildHumanSurveyBlocks renders the Slack block-kit message: a header,
// a row of rating buttons (min..max) plus an "Add feedback" button,
// and a hint. The minimum-rating button is styled `danger` (worse than
// nothing) and the maximum is styled `primary` (helpful). For the
// canonical −1/0/+1 scale the buttons render with thumbs-down / neutral /
// thumbs-up emoji affordances; other ranges fall back to plain integers.
// The `source` parameter ("stop" or "manual") is encoded in the action
// block's block_id so the rate handler can record provenance of the click.
func buildHumanSurveyBlocks(agentBeadID string, minRating, maxRating int, source string) []slack.Block {
	if maxRating < minRating {
		maxRating = minRating
	}

	header := slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn",
			fmt.Sprintf("*Was this agent session helpful?*  (`%s`)", agentBeadID),
			false, false),
		nil, nil,
	)
	hint := slack.NewContextBlock("", slack.NewTextBlockObject("mrkdwn",
		humanSurveyHintText(minRating, maxRating),
		false, false))

	elements := make([]slack.BlockElement, 0, maxRating-minRating+2)
	for n := minRating; n <= maxRating; n++ {
		btn := slack.NewButtonBlockElement(
			fmt.Sprintf("human_survey_rate_%d", n),
			fmt.Sprintf("%s:%d", agentBeadID, n),
			slack.NewTextBlockObject("plain_text", humanSurveyButtonLabel(n, minRating, maxRating), true, false),
		)
		switch n {
		case minRating:
			btn.Style = slack.StyleDanger
		case maxRating:
			btn.Style = slack.StylePrimary
		}
		elements = append(elements, btn)
	}
	// "Add feedback" button alongside the rating buttons. Clicking it fires
	// a block_actions event with action_id=humanSurveyFeedbackActionID and
	// value=<agent_bead_id>, which the bot routes to openFeedbackModal.
	// Left unstyled so it doesn't compete visually with the danger/primary
	// rating buttons.
	elements = append(elements, slack.NewButtonBlockElement(
		humanSurveyFeedbackActionID,
		agentBeadID,
		slack.NewTextBlockObject("plain_text", "Add feedback", false, false),
	))

	blockID := humanSurveyActionsBlockID
	if source == "manual" {
		blockID = humanSurveyActionsManualBlockID
	}
	actions := slack.NewActionBlock(blockID, elements...)

	return []slack.Block{header, actions, hint}
}

// humanSurveyButtonLabel returns the user-facing button text for a rating
// `n` within [min,max]. The canonical −1/0/+1 scale gets thumbs-down /
// neutral / thumbs-up emoji affordances. Any other range falls back to
// signed integer labels (e.g. "+2", "0", "−3") so the displayed value is
// unambiguous about which end of the scale is positive.
func humanSurveyButtonLabel(n, minRating, maxRating int) string {
	if minRating == -1 && maxRating == 1 {
		switch n {
		case -1:
			return ":thumbsdown: −1"
		case 0:
			return ":neutral_face: 0"
		case 1:
			return ":thumbsup: +1"
		}
	}
	if n > 0 {
		return fmt.Sprintf("+%d", n)
	}
	if n < 0 {
		return fmt.Sprintf("−%d", -n)
	}
	return "0"
}

// humanSurveyHintText composes the hint shown beneath the rating buttons.
// For the canonical −1/0/+1 scale the wording is calibrated to the
// thumbs-down/neutral/thumbs-up affordance; other ranges get a generic
// "min = worst, max = best" hint.
func humanSurveyHintText(minRating, maxRating int) string {
	if minRating == -1 && maxRating == 1 {
		return "−1 = worse than nothing; 0 = neutral; +1 = helpful."
	}
	return fmt.Sprintf("Pick a rating %d–%d. %d = worst; %d = best.",
		minRating, maxRating, minRating, maxRating)
}

// handleHumanSurveyRate processes rating button clicks. `value` is the
// button's value string: "<agent_bead_id>:<rating>". The click is recorded
// as a `type=rating` bead via writeRatingFromSlackClick (single-writer
// path; see plans/beads-viewer-human-ratings-slack-fold.md §6).
func (b *Bot) handleHumanSurveyRate(ctx context.Context, value string, callback slack.InteractionCallback) {
	agentBeadID, rating, ok := parseHumanSurveyValue(value)
	if !ok {
		b.logger.Warn("human-survey: malformed button value", "value", value)
		return
	}

	// Source ("stop" or "manual") is carried on the parent action block's
	// block_id; see buildHumanSurveyBlocks.
	source := "stop"
	if len(callback.ActionCallback.BlockActions) > 0 &&
		callback.ActionCallback.BlockActions[0].BlockID == humanSurveyActionsManualBlockID {
		source = "manual"
	}

	raterSlackUser := callback.User.ID
	now := time.Now().UTC()

	// Check that the agent bead still exists. Missing → rewrite the
	// message with an error and stop (no bead to attach to).
	agentBead, err := b.daemon.GetBead(ctx, agentBeadID)
	if err != nil || agentBead == nil {
		b.logger.Warn("human-survey: agent bead not found",
			"agent_bead_id", agentBeadID, "error", err)
		b.rewriteHumanSurveyMessage(ctx, callback,
			fmt.Sprintf(":warning: Could not record rating — agent bead `%s` not found.", agentBeadID))
		return
	}

	// Derive slack thread coords from the callback (authoritative for the
	// message the user just clicked on).
	slackChannel := callback.Channel.ID
	threadTS := callback.Message.ThreadTimestamp
	if threadTS == "" {
		threadTS = callback.Message.Timestamp
	}

	// Pre-check: did the rater already leave feedback? Used below to omit
	// the follow-up "Add feedback" button when feedback already exists on
	// the rating bead.
	hasFeedback := false
	isUpdate := false
	if existing := b.findExistingRating(ctx, agentBeadID, "", raterSlackUser); existing != nil {
		isUpdate = true
		if existing.Fields["feedback"] != "" {
			hasFeedback = true
		}
	}

	b.writeRatingFromSlackClick(ctx, ratingWriteRequest{
		agentBead:     agentBead,
		agentBeadID:   agentBeadID,
		scoreNative:   rating,
		source:        "slack-" + source,
		slackChannel:  slackChannel,
		slackThreadTS: threadTS,
		slackUserID:   raterSlackUser,
		answeredAt:    now,
	})

	confirmText := fmt.Sprintf(":white_check_mark: Rating *%d* recorded — thanks!", rating)
	if isUpdate {
		confirmText = fmt.Sprintf(":white_check_mark: Rating updated to *%d* — thanks!", rating)
	}
	b.rewriteHumanSurveyConfirmation(ctx, callback, agentBeadID, confirmText, hasFeedback)
}

// parseHumanSurveyValue parses "<agent_bead_id>:<rating>" into its parts.
func parseHumanSurveyValue(value string) (string, int, bool) {
	idx := strings.LastIndex(value, ":")
	if idx <= 0 || idx == len(value)-1 {
		return "", 0, false
	}
	agentBeadID := value[:idx]
	ratingStr := value[idx+1:]
	var rating int
	if _, err := fmt.Sscanf(ratingStr, "%d", &rating); err != nil {
		return "", 0, false
	}
	return agentBeadID, rating, true
}

// rewriteHumanSurveyMessage updates the original Slack message with a
// confirmation/error block, replacing the buttons so the prompt can't be
// re-clicked.
func (b *Bot) rewriteHumanSurveyMessage(ctx context.Context, callback slack.InteractionCallback, text string) {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}
	_, _, _, err := b.api.UpdateMessageContext(ctx, callback.Channel.ID, callback.Message.Timestamp,
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		b.logger.Error("human-survey: failed to rewrite message",
			"channel", callback.Channel.ID, "ts", callback.Message.Timestamp, "error", err)
	}
}

// rewriteHumanSurveyConfirmation updates the original Slack message with a
// rating confirmation and — unless the rater has already left feedback —
// an "Add feedback" button wired to the same feedback modal as the
// pre-rating button. This is how raters leave free-text
// feedback as an immediate follow-up to the rating click.
func (b *Bot) rewriteHumanSurveyConfirmation(ctx context.Context, callback slack.InteractionCallback, agentBeadID, text string, hasFeedback bool) {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}
	if !hasFeedback && agentBeadID != "" {
		btn := slack.NewButtonBlockElement(
			humanSurveyFeedbackActionID,
			agentBeadID,
			slack.NewTextBlockObject("plain_text", "Add feedback", false, false),
		)
		blocks = append(blocks, slack.NewActionBlock(
			humanSurveyActionsBlockID+":followup", btn))
	}
	_, _, _, err := b.api.UpdateMessageContext(ctx, callback.Channel.ID, callback.Message.Timestamp,
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		b.logger.Error("human-survey: failed to rewrite confirmation",
			"channel", callback.Channel.ID, "ts", callback.Message.Timestamp, "error", err)
	}
}
