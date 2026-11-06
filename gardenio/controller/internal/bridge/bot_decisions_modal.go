package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// slackBlockAction runs one Slack card button: the Slack half of HandleAction.
// It reports whether the button was one it takes; the Slack adapter
// (handleBlockActions) goes on to the click's next block action when not.
func (b *Bot) slackBlockAction(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction) bool {
	actionID := action.ActionID

	switch {
	// Concierge Start button: spawn agent for channel message.
	case actionID == "concierge_start":
		b.handleConciergeStart(ctx, action.Value, callback)
		return true

	// Concierge Dismiss button: remove the prompt.
	case actionID == "concierge_dismiss":
		b.handleConciergeDismiss(ctx, callback)
		return true

	// Codex re-login escalation: start a controller-side device login
	// (ADR 0007). action_id = codexDeviceLoginActionID, value = escalation bead ID.
	case actionID == codexDeviceLoginActionID:
		b.handleCodexDeviceLoginStart(ctx, action.Value, callback)
		return true

	// Autodestruct confirm/cancel buttons.
	case actionID == "autodestruct_confirm" || actionID == "autodestruct_cancel":
		b.handleAutoDestructButtonAction(ctx, actionID, callback)
		return true

	// Clear button: action_id = "clear_agent", value = agent identity.
	case actionID == "clear_agent":
		b.handleClearAgent(ctx, action.Value, callback)
		return true

	// Kill thread agent button: action_id = "kill_thread_agent", value = agent identity.
	case actionID == "kill_thread_agent":
		b.handleKillThreadAgent(ctx, action.Value, callback)
		return true

	// Restart thread agent button: action_id = "restart_thread_agent", value = agent identity.
	case actionID == "restart_thread_agent":
		b.handleRestartThreadAgent(ctx, action.Value, callback)
		return true

	// Permission allow button: action_id = "permission_allow", value = "requestID|channelURL".
	case actionID == "permission_allow":
		b.handlePermissionAllow(ctx, action.Value, callback)
		return true

	// Permission deny button: action_id = "permission_deny", value = "requestID|channelURL".
	case actionID == "permission_deny":
		b.handlePermissionDeny(ctx, action.Value, callback)
		return true

	// Dismiss button: action_id = "dismiss_decision", value = beadID.
	case actionID == "dismiss_decision":
		b.handleDismiss(ctx, action.Value, callback)
		return true

	// Promote button: action_id = "promote_decision", value = beadID.
	// V2: instead of opening a human modal, nudge the requesting agent
	// to create a structured escalation via `gb escalation create`.
	case actionID == "promote_decision":
		b.returnToAgentForEscalation(ctx, action.Value, callback)
		return true

	// Human-survey rating button: action_id = "human_survey_rate_<n>",
	// value = "<agent_bead_id>:<n>".
	case strings.HasPrefix(actionID, "human_survey_rate_"):
		b.handleHumanSurveyRate(ctx, action.Value, callback)
		return true

	// Human-survey "Add feedback" button: action_id =
	// humanSurveyFeedbackActionID, value = <agent_bead_id>. Falls back
	// to SelectedOption.Value for backwards-compat with older overflow-
	// element messages still live in Slack.
	case actionID == humanSurveyFeedbackActionID:
		agentBeadID := action.Value
		if agentBeadID == "" && action.SelectedOption.Value != "" {
			agentBeadID = action.SelectedOption.Value
		}
		b.openFeedbackModal(ctx, agentBeadID, callback)
		return true

	// "Other..." button: action_id = "resolve_other_{beadID}", value = beadID.
	case strings.HasPrefix(actionID, "resolve_other_"):
		beadID := strings.TrimPrefix(actionID, "resolve_other_")
		if perm := b.checkDecisionResolvePermission(ctx, beadID, callback.User.ID); !perm.Allowed {
			b.denyDecisionResolve(ctx, beadID, perm, callback)
			return true
		}
		b.openOtherModal(ctx, beadID, callback)
		return true

	// Option button: action_id = "resolve_{beadID}_{n}", value = "{beadID}:{n}".
	case strings.HasPrefix(actionID, "resolve_"):
		// value is "beadID:optionIndex" — extract beadID and resolve.
		parts := strings.SplitN(action.Value, ":", 2)
		if len(parts) != 2 {
			return false
		}
		beadID := parts[0]
		optIndex := parts[1]
		if perm := b.checkDecisionResolvePermission(ctx, beadID, callback.User.ID); !perm.Allowed {
			b.denyDecisionResolve(ctx, beadID, perm, callback)
			return true
		}
		// Resolve the clicked option to its canonical id (stored in
		// `chosen`) plus a human display label (shown in Slack). The id is
		// what the beads-viewer and analytics consume; storing the label
		// here is the historical bug (ka-0000000003) that left ~20% of
		// resolved decisions un-badged.
		chosenID, chosenDisplay := b.resolveOptionChoice(ctx, beadID, optIndex)
		b.openResolveModal(ctx, beadID, chosenID, chosenDisplay, callback)
		return true
	}
	return false
}

// decisionOptionRow is the subset of a decision `options` entry the bridge
// needs to map between an option id and its human-facing label/short.
type decisionOptionRow struct {
	ID           string `json:"id"`
	Short        string `json:"short"`
	Label        string `json:"label"`
	ArtifactType string `json:"artifact_type"`
}

// parseDecisionOptionRows decodes the JSON `options` field into rows. Returns
// nil (not an error) for absent/blank/malformed options so callers degrade
// gracefully.
func parseDecisionOptionRows(raw string) []decisionOptionRow {
	if raw == "" {
		return nil
	}
	var opts []decisionOptionRow
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		return nil
	}
	return opts
}

// optionDisplayLabel returns the best human-facing label for an option:
// label, else short, else id.
func optionDisplayLabel(opt decisionOptionRow) string {
	if opt.Label != "" {
		return opt.Label
	}
	if opt.Short != "" {
		return opt.Short
	}
	return opt.ID
}

// matchOptionID resolves a free-form input string to a canonical option id.
// It matches (case-insensitive, trimmed) against each option's id, then short,
// then label. Returns the matched option id and true on a hit; "", false when
// no option matches (the caller then treats the input as free text, not an id).
func matchOptionID(opts []decisionOptionRow, input string) (string, bool) {
	needle := strings.ToLower(strings.TrimSpace(input))
	if needle == "" {
		return "", false
	}
	for _, opt := range opts {
		if opt.ID != "" && strings.ToLower(strings.TrimSpace(opt.ID)) == needle {
			return opt.ID, true
		}
	}
	for _, opt := range opts {
		if opt.Short != "" && strings.ToLower(strings.TrimSpace(opt.Short)) == needle {
			return opt.ID, true
		}
	}
	for _, opt := range opts {
		if opt.Label != "" && strings.ToLower(strings.TrimSpace(opt.Label)) == needle {
			return opt.ID, true
		}
	}
	return "", false
}

// resolveOptionChoice looks up a clicked option by its 1-based index and
// returns (chosenID, displayLabel). chosenID is the canonical option id to
// persist in the `chosen` field; displayLabel is the human string to show in
// Slack. When the decision has no structured options, chosenID falls back to
// the same string as displayLabel (there is no id to prefer), preserving the
// prior behaviour for option-less decisions.
func (b *Bot) resolveOptionChoice(ctx context.Context, beadID, optIndex string) (chosenID, displayLabel string) {
	bead, err := b.daemon.GetBead(ctx, beadID)
	if err != nil {
		b.logger.Warn("resolveOptionChoice: could not fetch bead",
			"bead", beadID, "error", err)
		fallback := fmt.Sprintf("Option %s", optIndex)
		return fallback, fallback
	}
	opts := parseDecisionOptionRows(bead.Fields["options"])
	if len(opts) == 0 {
		b.logger.Warn("resolveOptionChoice: decision has no options, using prompt as label",
			"bead", beadID, "optIndex", optIndex)
		// No structured options — there is no id to prefer, so id == display.
		if prompt := bead.Fields["prompt"]; prompt != "" {
			// Rune-aware truncation: a byte slice can split a multi-byte rune
			// into invalid UTF-8 that Slack rejects with invalid_blocks.
			prompt = truncateRunes(prompt, 80)
			return prompt, prompt
		}
		fallback := fmt.Sprintf("Option %s (no options defined)", optIndex)
		return fallback, fallback
	}
	// optIndex is 1-based.
	idx := 0
	if _, err := fmt.Sscanf(optIndex, "%d", &idx); err == nil && idx >= 1 && idx <= len(opts) {
		opt := opts[idx-1]
		display := optionDisplayLabel(opt)
		if opt.ID != "" {
			return opt.ID, display
		}
		// Malformed option with no id — fall back to the display string for
		// both so nothing is silently dropped.
		return display, display
	}
	fallback := fmt.Sprintf("Option %s", optIndex)
	return fallback, fallback
}

// lookupArtifactType fetches the bead's options and returns the artifact_type
// for the option matching `chosen`. `chosen` is normally the canonical option
// id, but this also matches on short/label so it stays correct for any caller
// (and for historical labels). Returns "" if not found.
func (b *Bot) lookupArtifactType(ctx context.Context, beadID, chosen string) string {
	bead, err := b.daemon.GetBead(ctx, beadID)
	if err != nil {
		return ""
	}
	opts := parseDecisionOptionRows(bead.Fields["options"])
	id, ok := matchOptionID(opts, chosen)
	if !ok {
		return ""
	}
	for _, opt := range opts {
		if opt.ID == id && opt.ArtifactType != "" {
			return opt.ArtifactType
		}
	}
	return ""
}

// openResolveModal opens a modal for confirming a decision choice with optional
// rationale. chosenID is the canonical option id persisted in `chosen`;
// chosenDisplay is the human label shown to the operator in Slack.
func (b *Bot) openResolveModal(ctx context.Context, beadID, chosenID, chosenDisplay string, callback slack.InteractionCallback) {
	// Build and open the modal immediately (trigger_id expires in 3s).
	titleText := slack.NewTextBlockObject("plain_text", "Resolve Decision", false, false)
	submitText := slack.NewTextBlockObject("plain_text", "Confirm", false, false)
	closeText := slack.NewTextBlockObject("plain_text", "Cancel", false, false)

	// Fetch the decision question for display.
	question := beadID // fallback
	bead, err := b.daemon.GetBead(ctx, beadID)
	if err == nil {
		if q := decisionQuestion(bead.Fields); q != "" {
			question = q
		}
	}

	blocks := slack.Blocks{
		BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*%s*", question), false, false),
				nil, nil,
			),
			slack.NewDividerBlock(),
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("%s Selected: *%s*", b.themeFor(ctx, callback.Channel.ID).Semantic(SemOK), chosenDisplay), false, false),
				nil, nil,
			),
			slack.NewInputBlock(
				"rationale",
				slack.NewTextBlockObject("plain_text", "Notes (optional)", false, false),
				slack.NewTextBlockObject("plain_text", "Extra context or notes for the agent — not a justification for your choice.", false, false),
				slack.NewPlainTextInputBlockElement(
					slack.NewTextBlockObject("plain_text", "Add any extra context or notes (optional)", false, false),
					"rationale_input",
				),
			),
		},
	}

	// Make rationale optional.
	inputBlock := blocks.BlockSet[3].(*slack.InputBlock)
	inputBlock.Optional = true

	// Encode metadata: beadID|chosenID|channelID|messageTS|chosenDisplay.
	// chosenDisplay is LAST because a label may legitimately contain "|" —
	// handleResolveSubmission splits with a cap of 5 so any stray separators
	// stay inside the display tail and never corrupt the id/channel/ts fields.
	metadata := fmt.Sprintf("%s|%s|%s|%s|%s", beadID, chosenID, callback.Channel.ID, callback.Message.Timestamp, chosenDisplay)

	modal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           titleText,
		Submit:          submitText,
		Close:           closeText,
		Blocks:          blocks,
		PrivateMetadata: metadata,
		CallbackID:      "resolve_decision",
	}

	if _, err := b.api.OpenView(callback.TriggerID, modal); err != nil {
		// views.open fails for external / Slack Connect users: a trigger_id
		// minted by a user in a workspace where the app is not installed
		// cannot be exchanged for a modal (documented Slack limitation). It
		// also fails when the trigger expired (>3s after the click). Either
		// way the user's intent was explicit — they clicked a specific
		// option — so resolve directly and confirm via ephemeral instead of
		// going silently dark (ka-0000005533).
		b.logger.Warn("resolve modal failed to open, falling back to direct resolve",
			"bead", beadID, "user", callback.User.ID, "team", callback.User.TeamID, "error", err)
		b.resolveDecisionWithoutModal(ctx, beadID, chosenID, chosenDisplay, callback)
	}
}

// resolveDecisionWithoutModal resolves a decision straight from the option
// button click, without the confirm-with-rationale modal. Used as the
// fallback when views.open fails — which is the norm for Slack Connect /
// external-workspace owners (ka-0000005533). Success and failure are both
// reported to the clicker via postInteractionEphemeral so this path is never
// silent. chosenID is persisted in `chosen`; chosenDisplay is shown to the user.
func (b *Bot) resolveDecisionWithoutModal(ctx context.Context, beadID, chosenID, chosenDisplay string, callback slack.InteractionCallback) {
	user := callback.User.Name
	if user == "" {
		user = callback.User.ID
	}
	rationale := fmt.Sprintf("Chosen by @%s via Slack", user)

	fields := map[string]string{
		"chosen":    chosenID,
		"rationale": rationale,
	}
	if at := b.lookupArtifactType(ctx, beadID, chosenID); at != "" {
		fields["required_artifact"] = at
		fields["artifact_status"] = "pending"
	}

	if err := b.daemon.CloseBead(ctx, beadID, fields); err != nil {
		b.logger.Error("failed to resolve decision without modal",
			"bead", beadID, "user", callback.User.ID, "error", err)
		b.postInteractionEphemeral(ctx, callback, callback.Channel.ID, fmt.Sprintf(
			"%s Couldn't record your choice *%s* for `%s`: %v\nReply in this decision's thread with your choice instead, or resolve it from the beads UI.",
			b.themeFor(ctx, callback.Channel.ID).Semantic(SemWarn), chosenDisplay, beadID, err))
		return
	}

	// Directly update the Slack message to show resolved state.
	b.updateMessageResolved(ctx, beadID, chosenDisplay, rationale, callback.Channel.ID, callback.Message.Timestamp)

	// Nudge the requesting agent so it wakes up immediately instead of waiting
	// for the next gb yield poll cycle (see handleResolveSubmission).
	go b.nudgeDecisionAgent(beadID)

	b.postInteractionEphemeral(ctx, callback, callback.Channel.ID, fmt.Sprintf(
		"%s Recorded your choice *%s* for `%s`. (The confirmation dialog couldn't be opened — Slack doesn't allow it for external-workspace users — so your choice was recorded directly.)",
		b.themeFor(ctx, callback.Channel.ID).Semantic(SemOK), chosenDisplay, beadID))

	b.logger.Info("decision resolved via modal-less fallback",
		"bead", beadID, "chosen", chosenID, "user", user)
}

// postInteractionEphemeral sends an ephemeral-style notice to the user who
// triggered an interaction. It prefers the interaction's response_url —
// which Slack delivers cross-workspace, so it reaches external / Slack
// Connect users for whom chat.postEphemeral and views.open may not be
// authorized — and falls back to chat.postEphemeral when the callback
// carries no response_url (e.g. view submissions, where the channel comes
// from modal private metadata instead). Best-effort.
func (b *Bot) postInteractionEphemeral(ctx context.Context, callback slack.InteractionCallback, channelID, text string) {
	if callback.ResponseURL != "" {
		msg := &slack.WebhookMessage{
			Text:         text,
			ResponseType: slack.ResponseTypeEphemeral,
		}
		err := slack.PostWebhookContext(ctx, callback.ResponseURL, msg)
		if err == nil {
			return
		}
		b.logger.Warn("interaction response_url post failed, falling back to chat.postEphemeral",
			"user", callback.User.ID, "error", err)
	}
	if b.api == nil || channelID == "" || callback.User.ID == "" {
		return
	}
	_, _ = b.api.PostEphemeral(channelID, callback.User.ID,
		slack.MsgOptionText(text, false))
}

// openOtherModal opens a modal for custom freeform text response.
func (b *Bot) openOtherModal(ctx context.Context, beadID string, callback slack.InteractionCallback) {
	titleText := slack.NewTextBlockObject("plain_text", "Custom Response", false, false)
	submitText := slack.NewTextBlockObject("plain_text", "Submit", false, false)
	closeText := slack.NewTextBlockObject("plain_text", "Cancel", false, false)

	// Fetch the decision question for display.
	question := beadID
	bead, err := b.daemon.GetBead(ctx, beadID)
	if err == nil {
		if q := decisionQuestion(bead.Fields); q != "" {
			question = q
		}
	}

	// Build artifact_type select options: "none" plus all valid types.
	artifactTypeOpts := []*slack.OptionBlockObject{
		slack.NewOptionBlockObject("none",
			slack.NewTextBlockObject("plain_text", "None (no artifact required)", false, false), nil),
	}
	for _, at := range beadsapi.ArtifactTypes {
		artifactTypeOpts = append(artifactTypeOpts,
			slack.NewOptionBlockObject(at,
				slack.NewTextBlockObject("plain_text", at, false, false), nil))
	}

	blocks := slack.Blocks{
		BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*%s*", question), false, false),
				nil, nil,
			),
			slack.NewDividerBlock(),
			slack.NewInputBlock(
				"response",
				slack.NewTextBlockObject("plain_text", "Your Response", false, false),
				nil,
				&slack.PlainTextInputBlockElement{
					Type:        slack.METPlainTextInput,
					ActionID:    "response_input",
					Multiline:   true,
					Placeholder: slack.NewTextBlockObject("plain_text", "Type your response...", false, false),
				},
			),
			func() slack.Block {
				sel := slack.NewOptionsSelectBlockElement(
					slack.OptTypeStatic,
					slack.NewTextBlockObject("plain_text", "Choose artifact type...", false, false),
					"artifact_type_input",
					artifactTypeOpts...,
				)
				sel.InitialOption = artifactTypeOpts[0]
				return slack.NewInputBlock(
					"artifact_type",
					slack.NewTextBlockObject("plain_text", "Required Artifact Type", false, false),
					slack.NewTextBlockObject("plain_text", "What artifact will you produce?", false, false),
					sel,
				)
			}(),
		},
	}

	// Encode metadata: beadID|channelID|messageTS
	otherMetadata := fmt.Sprintf("%s|%s|%s", beadID, callback.Channel.ID, callback.Message.Timestamp)

	modal := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           titleText,
		Submit:          submitText,
		Close:           closeText,
		Blocks:          blocks,
		PrivateMetadata: otherMetadata,
		CallbackID:      "resolve_other",
	}

	if _, err := b.api.OpenView(callback.TriggerID, modal); err != nil {
		// A freeform response needs the modal, so there is no direct-resolve
		// fallback here — but the failure must be loud, and thread replies
		// are a working alternative path (handleThreadReply resolves open
		// decisions from replies, cross-workspace included). ka-0000005533.
		b.logger.Warn("other modal failed to open",
			"bead", beadID, "user", callback.User.ID, "team", callback.User.TeamID, "error", err)
		b.postInteractionEphemeral(ctx, callback, callback.Channel.ID, fmt.Sprintf(
			"%s Couldn't open the custom-response dialog for `%s` — Slack doesn't allow modals for external-workspace (Slack Connect) users. Reply directly in this decision's thread instead; thread replies resolve the decision.",
			b.themeFor(ctx, callback.Channel.ID).Semantic(SemWarn), beadID))
	}
}

// handleDismiss dismisses a decision (deletes message, closes bead).
func (b *Bot) handleDismiss(ctx context.Context, beadID string, callback slack.InteractionCallback) {
	fields := map[string]string{
		"chosen":    "dismissed",
		"rationale": fmt.Sprintf("Dismissed by @%s via Slack", callback.User.Name),
	}
	if err := b.daemon.CloseBead(ctx, beadID, fields); err != nil {
		b.logger.Error("failed to dismiss decision", "bead", beadID, "error", err)
		b.postInteractionEphemeral(ctx, callback, callback.Channel.ID, fmt.Sprintf(
			"%s Couldn't dismiss `%s`: %v", b.themeFor(ctx, callback.Channel.ID).Semantic(SemWarn), beadID, err))
		return
	}

	// Delete the Slack message.
	_, _, _ = b.api.DeleteMessage(callback.Channel.ID, callback.MessageTs)

	b.logger.Info("decision dismissed", "bead", beadID, "user", callback.User.Name)
}

// slackViewSubmission runs one Slack modal submission: the Slack half of
// HandleSubmit. It reports whether the modal was one it takes.
func (b *Bot) slackViewSubmission(ctx context.Context, callback slack.InteractionCallback) bool {
	switch callback.View.CallbackID {
	case "resolve_decision":
		b.handleResolveSubmission(ctx, callback)
	case "resolve_other":
		b.handleOtherSubmission(ctx, callback)
	case "promote_decision":
		b.handlePromoteSubmission(ctx, callback)
	case humanSurveyFeedbackCallbackID:
		b.handleFeedbackSubmission(ctx, callback)
	default:
		return false
	}
	return true
}

// handleResolveSubmission processes the resolve decision modal submission.
func (b *Bot) handleResolveSubmission(ctx context.Context, callback slack.InteractionCallback) {
	metadata := callback.View.PrivateMetadata
	// Metadata format: beadID|chosenID|channelID|messageTS|chosenDisplay.
	// The 5th (display) field is optional for backward compatibility with
	// modals opened before this field existed; it may itself contain "|" so
	// the split cap is 5 and everything past the 4th separator is the display.
	parts := strings.SplitN(metadata, "|", 5)
	if len(parts) < 2 {
		b.logger.Error("invalid resolve modal metadata", "metadata", metadata)
		return
	}
	beadID := parts[0]
	chosenID := parts[1]
	channelID := ""
	messageTS := ""
	if len(parts) >= 4 {
		channelID = parts[2]
		messageTS = parts[3]
	}
	// Human display label for Slack messages; falls back to the id when the
	// modal predates the display field.
	chosenDisplay := chosenID
	if len(parts) >= 5 && parts[4] != "" {
		chosenDisplay = parts[4]
	}

	// Defense in depth: re-check permission on submit. The button handler
	// already gates modal opening, but a replayed/stale view should not be
	// able to resolve a decision the submitter is not addressed by.
	if perm := b.checkDecisionResolvePermission(ctx, beadID, callback.User.ID); !perm.Allowed {
		b.denyDecisionResolve(ctx, beadID, perm, callback)
		return
	}

	// Extract rationale from form values.
	rationale := ""
	if v, ok := callback.View.State.Values["rationale"]["rationale_input"]; ok {
		rationale = v.Value
	}

	// Build attribution.
	user := callback.User.Name
	if rationale != "" {
		rationale = fmt.Sprintf("%s — @%s via Slack", rationale, user)
	} else {
		rationale = fmt.Sprintf("Chosen by @%s via Slack", user)
	}

	fields := map[string]string{
		"chosen":    chosenID,
		"rationale": rationale,
	}

	// Look up artifact_type from the chosen option.
	if at := b.lookupArtifactType(ctx, beadID, chosenID); at != "" {
		fields["required_artifact"] = at
		fields["artifact_status"] = "pending"
	}

	if err := b.daemon.CloseBead(ctx, beadID, fields); err != nil {
		b.logger.Error("failed to resolve decision from modal",
			"bead", beadID, "error", err)
		// The modal has already closed — without this the failure is
		// invisible to the submitter (ka-0000005533: silence is the bug).
		b.postInteractionEphemeral(ctx, callback, channelID, fmt.Sprintf(
			"%s Your choice *%s* for `%s` was NOT recorded: %v",
			b.themeFor(ctx, channelID).Semantic(SemWarn), chosenDisplay, beadID, err))
		return
	}

	// Directly update the Slack message to show resolved state.
	b.updateMessageResolved(ctx, beadID, chosenDisplay, rationale, channelID, messageTS)

	// Nudge the requesting agent so it wakes up immediately instead of waiting
	// for the next gb yield poll cycle. Run async — nudge is best-effort and
	// retries can take up to 14s, which would block the event handler.
	// Use a detached context because the HTTP request context is cancelled
	// when this handler returns, which would abort the nudge goroutine.
	go b.nudgeDecisionAgent(beadID)

	b.logger.Info("decision resolved via modal",
		"bead", beadID, "chosen", chosenID, "user", user)
}

// handleOtherSubmission processes the custom response modal submission.
func (b *Bot) handleOtherSubmission(ctx context.Context, callback slack.InteractionCallback) {
	metadata := callback.View.PrivateMetadata
	// Metadata format: beadID|channelID|messageTS
	parts := strings.SplitN(metadata, "|", 3)
	beadID := parts[0]
	channelID := ""
	messageTS := ""
	if len(parts) >= 3 {
		channelID = parts[1]
		messageTS = parts[2]
	}

	// Defense in depth: re-check permission on submit (see
	// handleResolveSubmission).
	if perm := b.checkDecisionResolvePermission(ctx, beadID, callback.User.ID); !perm.Allowed {
		b.denyDecisionResolve(ctx, beadID, perm, callback)
		return
	}

	response := ""
	if v, ok := callback.View.State.Values["response"]["response_input"]; ok {
		response = v.Value
	}
	if response == "" {
		return
	}

	user := callback.User.Name
	rationale := fmt.Sprintf("Custom response by @%s via Slack", user)

	// A "custom" response may still name one of the offered options (operators
	// often re-type an option's short/label). Resolve it to the canonical id
	// so the decision badges correctly and analytics collapse to one bucket.
	// Only genuinely free-form text (no option match) lands in `response_text`
	// with `chosen` left empty — mirroring the `gb decision respond --text`
	// contract — so a sentence is never recorded as if it were an option id.
	fields := b.decisionResolutionFields(ctx, beadID, response)
	fields["rationale"] = rationale

	// Extract artifact_type from the dropdown; set required_artifact if not "none".
	if v, ok := callback.View.State.Values["artifact_type"]["artifact_type_input"]; ok {
		if at := v.SelectedOption.Value; at != "" && at != "none" {
			fields["required_artifact"] = at
			fields["artifact_status"] = "pending"
		}
	}

	if err := b.daemon.CloseBead(ctx, beadID, fields); err != nil {
		b.logger.Error("failed to resolve decision from other modal",
			"bead", beadID, "error", err)
		// The modal has already closed — without this the failure is
		// invisible to the submitter (ka-0000005533: silence is the bug).
		b.postInteractionEphemeral(ctx, callback, channelID, fmt.Sprintf(
			"%s Your response for `%s` was NOT recorded: %v",
			b.themeFor(ctx, channelID).Semantic(SemWarn), beadID, err))
		return
	}

	// Directly update the Slack message to show resolved state.
	b.updateMessageResolved(ctx, beadID, response, rationale, channelID, messageTS)

	// Nudge the requesting agent so it wakes up immediately instead of waiting
	// for the next gb yield poll cycle. Run async — nudge is best-effort and
	// retries can take up to 14s, which would block the event handler.
	// Use a detached context because the HTTP request context is cancelled
	// when this handler returns, which would abort the nudge goroutine.
	go b.nudgeDecisionAgent(beadID)

	b.logger.Info("decision resolved via custom response",
		"bead", beadID, "response", response, "user", user)
}

// decisionResolutionFields maps a free-form operator response to the correct
// resolution fields for a decision. It fetches the decision's options and
// delegates to decisionResolutionFieldsFor. This is the shared normaliser for
// every Slack path that accepts free-form text (thread reply, custom-response
// modal) — the fix for ka-0000000003.
func (b *Bot) decisionResolutionFields(ctx context.Context, beadID, text string) map[string]string {
	optionsRaw := ""
	if bead, err := b.daemon.GetBead(ctx, beadID); err == nil {
		optionsRaw = bead.Fields["options"]
	} else {
		b.logger.Warn("decisionResolutionFields: could not fetch bead",
			"bead", beadID, "error", err)
	}
	return decisionResolutionFieldsFor(optionsRaw, text)
}

// decisionResolutionFieldsFor is the pure core of decisionResolutionFields:
// given a decision's raw `options` JSON and an operator's free-form text, it
// returns the resolution fields to persist:
//   - on an option match (id/short/label, case-insensitive, trimmed):
//     {"chosen": <option id>};
//   - on a shutdown keyword (shut down / stand down): {"chosen": text}
//     verbatim, so the respawn-skip guard (isShutdownDecision) still fires;
//   - otherwise: {"response_text": text} with NO `chosen`, so genuinely
//     free-form answers are recorded as text rather than fabricated option ids.
//
// Taking the options JSON directly (rather than a bead id) lets callers that
// already hold the bead avoid a second GetBead.
func decisionResolutionFieldsFor(optionsRaw, text string) map[string]string {
	trimmed := strings.TrimSpace(text)
	opts := parseDecisionOptionRows(optionsRaw)
	if id, ok := matchOptionID(opts, trimmed); ok {
		return map[string]string{"chosen": id}
	}
	// Preserve shutdown-intent detection: the respawn guards key off `chosen`,
	// so a "shut down" reply must stay in `chosen` even though it is not an
	// option pick, or a dead agent would be needlessly respawned.
	if isShutdownDecision(trimmed) {
		return map[string]string{"chosen": trimmed}
	}
	return map[string]string{"response_text": trimmed}
}

// nudgeDecisionAgent looks up the requesting agent from the decision bead and
// delivers a nudge via the unified NudgeAgent path (tries MCP channel first,
// falls back to coop PTY, respawns dead agents). Uses a detached context with
// timeout since callers launch this as a goroutine from HTTP handlers whose
// request context is cancelled on return.
func (b *Bot) nudgeDecisionAgent(decisionBeadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dec, err := b.daemon.GetBead(ctx, decisionBeadID)
	if err != nil {
		b.logger.Debug("nudge: could not fetch decision bead", "bead", decisionBeadID, "error", err)
		return
	}

	// Resolve agent name: prefer Assignee, fall back to CreatedBy.
	agentName := dec.Assignee
	if agentName == "" {
		agentName = dec.CreatedBy
	}
	if agentName == "" {
		b.logger.Debug("nudge: decision has no assignee or created_by", "bead", decisionBeadID)
		return
	}

	// Build a nudge message with full decision context.
	chosen := dec.Fields["chosen"]
	responseText := dec.Fields["response_text"]
	rationale := dec.Fields["rationale"]
	prompt := decisionQuestion(dec.Fields)

	message := fmt.Sprintf("Decision %s resolved", decisionBeadID)
	if prompt != "" {
		message += fmt.Sprintf(": %s", prompt)
	}
	if chosen != "" {
		message += fmt.Sprintf("\nChosen: %s", chosen)
	}
	// A free-text answer (no option matched) lands in response_text, not
	// chosen — surface it so the agent still sees the operator's reply.
	if responseText != "" {
		message += fmt.Sprintf("\nResponse: %s", responseText)
	}
	if rationale != "" {
		message += fmt.Sprintf("\nRationale: %s", rationale)
	}
	if ra := dec.Fields["required_artifact"]; ra != "" {
		message += fmt.Sprintf("\nArtifact required (%s). Use `gb decision report %s` to submit.", ra, decisionBeadID)
	}

	if err := NudgeAgent(ctx, b.daemon, b.logger, agentName, message); err != nil {
		// For shutdown decisions, don't respawn — the agent is already stopped,
		// which is the desired end state.
		if isShutdownDecision(chosen) {
			b.logger.Info("agent already stopped for shutdown decision, skipping respawn",
				"agent", agentName, "decision", decisionBeadID, "chosen", chosen)
			return
		}
		b.logger.Warn("nudge: failed to nudge agent for decision response, attempting respawn",
			"agent", agentName, "decision", decisionBeadID, "error", err)
		if b.decisionRespawner != nil {
			bead := BeadEvent{
				ID:        dec.ID,
				Type:      dec.Type,
				Title:     dec.Title,
				Status:    dec.Status,
				Assignee:  dec.Assignee,
				CreatedBy: dec.CreatedBy,
				Labels:    dec.Labels,
				Fields:    dec.Fields,
			}
			b.decisionRespawner.RespawnForDecision(ctx, agentName, bead, message)
		}
		return
	}

	b.logger.Info("nudged agent for decision response",
		"agent", agentName, "decision", decisionBeadID)
}

// returnToAgentForEscalation nudges the requesting agent to create a structured
// escalation via `gb escalation create` instead of the human filling out a modal.
// The agent knows best what it needs — operation type, script, credentials, etc.
func (b *Bot) returnToAgentForEscalation(ctx context.Context, beadID string, callback slack.InteractionCallback) {
	dec, err := b.daemon.GetBead(ctx, beadID)
	if err != nil {
		b.logger.Error("failed to fetch decision for agent return", "bead", beadID, "error", err)
		return
	}
	if dec.Status == "closed" {
		b.logger.Warn("decision already closed, ignoring escalate click", "bead", beadID)
		return
	}

	promotedBy := fmt.Sprintf("@%s via Slack", callback.User.Name)
	agentName, ok := b.returnDecisionToAgent(ctx, dec, promotedBy)
	if !ok {
		return
	}

	// Update the Slack message to show the return-to-agent state.
	channelID := callback.Channel.ID
	messageTS := callback.Message.Timestamp
	if channelID != "" && messageTS != "" {
		returnedBlocks := []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("%s *Returned to Agent for Escalation*\n\n"+
						"Agent `%s` has been instructed to create a structured escalation "+
						"via `gb escalation create`.\n\nReturned by %s",
						b.themeFor(ctx, channelID).BeadType["agent"], agentName, promotedBy),
					false, false),
				nil, nil),
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("Original decision: %s", b.beadLink(beadID, fmt.Sprintf("`%s`", beadID))), false, false)),
		}
		_, _, _, updateErr := b.api.UpdateMessageContext(ctx, channelID, messageTS,
			slack.MsgOptionBlocks(returnedBlocks...),
		)
		if updateErr != nil {
			b.logger.Error("failed to update Slack message after agent return",
				"bead", beadID, "error", updateErr)
		}
	}

	b.logger.Info("decision returned to agent for escalation",
		"decision", beadID, "agent", agentName, "user", callback.User.Name)
}

// returnDecisionToAgent closes the open decision dec as returned to its agent
// and nudges the agent (respawning it when the nudge fails) to file a
// structured escalation instead. promotedBy names who did it, in the words
// the agent and the record read ("@ann via Slack"). ok is false when dec names
// no agent to return it to.
func (b *Bot) returnDecisionToAgent(ctx context.Context, dec *beadsapi.BeadDetail, promotedBy string) (agentName string, ok bool) {
	beadID := dec.ID
	question := decisionQuestion(dec.Fields)

	// Resolve agent name: prefer requesting_agent_bead_id, fall back to assignee/created_by.
	agentName = dec.Fields["requesting_agent_bead_id"]
	if agentName == "" {
		agentName = dec.Assignee
	}
	if agentName == "" {
		agentName = dec.CreatedBy
	}
	if agentName == "" {
		b.logger.Error("cannot return to agent: no agent identified on decision", "bead", beadID)
		return "", false
	}

	// Build a nudge instructing the agent to create an escalation.
	nudgeMsg := fmt.Sprintf(
		"Decision %s has been escalated by %s. Create a structured escalation for this decision using `gb escalation create`.\n\n"+
			"Original question: %s\n\n"+
			"Use `gb escalation create` with:\n"+
			"  --title \"Escalation: %s\"\n"+
			"  --operation-type <type>     (general, aws_iam, k8s_rbac, helm_deploy)\n"+
			"  --script '<script>'         (the actual commands to run)\n"+
			"  --credentials '<json>'      (what access is needed)\n"+
			"  --expected-outputs '<json>' (what the escalation will produce)\n"+
			"  --sensitivity <level>       (public, internal, sensitive, secret)\n"+
			"  --context '%s'\n\n"+
			"The agent should fill in the details — it knows best what operation, script, and credentials are needed.",
		beadID, promotedBy, question, truncate(question, 60), question,
	)

	// Close the decision as returned to agent.
	closeFields := map[string]string{
		"chosen":    "returned_to_agent_for_escalation",
		"rationale": fmt.Sprintf("Returned to agent for structured escalation by %s", promotedBy),
	}
	if err := b.daemon.CloseBead(ctx, beadID, closeFields); err != nil {
		b.logger.Error("failed to close decision after agent return", "bead", beadID, "error", err)
	}

	// Nudge the agent.
	if err := NudgeAgent(ctx, b.daemon, b.logger, agentName, nudgeMsg); err != nil {
		b.logger.Error("failed to nudge agent for escalation creation",
			"agent", agentName, "decision", beadID, "error", err)
		// If nudge fails and we have a respawner, try respawning.
		if b.decisionRespawner != nil {
			bead := BeadEvent{
				ID:        dec.ID,
				Type:      dec.Type,
				Title:     dec.Title,
				Status:    dec.Status,
				Assignee:  dec.Assignee,
				CreatedBy: dec.CreatedBy,
				Labels:    dec.Labels,
				Fields:    dec.Fields,
			}
			b.decisionRespawner.RespawnForDecision(ctx, agentName, bead, nudgeMsg)
		}
	}

	return agentName, true
}

// handlePromoteSubmission processes the promote decision modal submission.
func (b *Bot) handlePromoteSubmission(ctx context.Context, callback slack.InteractionCallback) {
	metadata := callback.View.PrivateMetadata
	parts := strings.SplitN(metadata, "|", 3)
	beadID := parts[0]
	channelID := ""
	messageTS := ""
	if len(parts) >= 3 {
		channelID = parts[1]
		messageTS = parts[2]
	}

	opType := "general"
	if v, ok := callback.View.State.Values["operation_type"]["operation_type_input"]; ok {
		if v.SelectedOption.Value != "" {
			opType = v.SelectedOption.Value
		}
	}
	execTarget := "local"
	if v, ok := callback.View.State.Values["execution_target"]["execution_target_input"]; ok {
		if v.SelectedOption.Value != "" {
			execTarget = v.SelectedOption.Value
		}
	}

	promotedBy := fmt.Sprintf("@%s via Slack", callback.User.Name)

	escalationID, err := PromoteDecisionToEscalation(ctx, b.daemon, beadID, opType, execTarget, promotedBy)
	if err != nil {
		b.logger.Error("failed to promote decision from modal",
			"bead", beadID, "error", err)
		return
	}

	// Update the Slack message to show promoted state.
	if channelID != "" && messageTS != "" {
		promotedBlocks := []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("%s *Promoted to Escalation*\n\nThis decision has been promoted to escalation %s.\nOperation type: *%s* | Target: *%s*\nPromoted by %s",
						b.themeFor(ctx, channelID).BeadType["escalation"], b.beadLink(escalationID, fmt.Sprintf("`%s`", escalationID)), opType, execTarget, promotedBy),
					false, false),
				nil, nil),
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("Original decision: %s", b.beadLink(beadID, fmt.Sprintf("`%s`", beadID))), false, false)),
		}
		_, _, _, updateErr := b.api.UpdateMessageContext(ctx, channelID, messageTS,
			slack.MsgOptionBlocks(promotedBlocks...),
		)
		if updateErr != nil {
			b.logger.Error("failed to update Slack message after promotion",
				"bead", beadID, "error", updateErr)
		}
	}

	b.logger.Info("decision promoted to escalation via Slack",
		"decision", beadID, "escalation", escalationID,
		"operation_type", opType, "execution_target", execTarget,
		"user", callback.User.Name)
}

// markDecisionSuperseded replaces the predecessor decision message with a
// "Superseded" notice linking to the new follow-up decision.
func (b *Bot) markDecisionSuperseded(ctx context.Context, predecessorID, newDecisionID, channelID, messageTS string) {
	_, _, _, err := b.api.UpdateMessageContext(ctx, channelID, messageTS,
		slack.MsgOptionBlocks(b.supersededBlocks(predecessorID, newDecisionID)...),
	)
	if err != nil {
		b.logger.Error("failed to mark decision as superseded",
			"predecessor", predecessorID, "successor", newDecisionID, "error", err)
	}

	// Any mirror of the predecessor (see decision_message_mirrors.go) must be
	// told too, or it keeps showing live buttons for a decision that has been
	// replaced.
	b.updateMirrorsSuperseded(ctx, predecessorID, newDecisionID)
}

// supersededBlocks renders the "this decision was replaced" state. Shared by the
// primary message update and the mirror fanout so both copies read identically.
func (b *Bot) supersededBlocks(predecessorID, newDecisionID string) []slack.Block {
	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("*Superseded*\n\nA follow-up decision (%s) has been posted in this thread.\n_Please refer to the latest decision in the thread below._", b.beadLink(newDecisionID, fmt.Sprintf("`%s`", newDecisionID))),
				false, false),
			nil, nil),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("Original decision: %s", b.beadLink(predecessorID, fmt.Sprintf("`%s`", predecessorID))), false, false)),
	}
}
