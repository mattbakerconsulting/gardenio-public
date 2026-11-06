package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailproto"

	"github.com/slack-go/slack"
)

// PermissionCallbackHandler handles HTTP callbacks from gardenio-channel servers
// when Claude Code hits a tool-approval prompt. It posts a Slack message with
// approve/deny buttons and routes button clicks back to the channel server.
//
// The agent a request is for comes from the caller, never from the body alone
// (gd-eemeNhUt5I): see askerOf.
type PermissionCallbackHandler struct {
	bot    *Bot
	logger *slog.Logger

	refuseSharedBearer bool // SetSharedBearerAgents(false)
}

// NewPermissionCallbackHandler creates a handler for channel permission callbacks.
func NewPermissionCallbackHandler(bot *Bot, logger *slog.Logger) *PermissionCallbackHandler {
	return &PermissionCallbackHandler{bot: bot, logger: logger}
}

// SetSharedBearerAgents sets whether the shared bridge bearer may still name
// the asking agent (operator lands only), as on the conversation routes
// (ConversationAgentAPI.SetSharedBearerAgents). On by default.
func (h *PermissionCallbackHandler) SetSharedBearerAgents(on bool) {
	h.refuseSharedBearer = !on
}

// RegisterRoutes adds the /api/permission/callback endpoint to the given mux.
func (h *PermissionCallbackHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/permission/callback", h.handleCallback)
}

// channelPermissionCallback is the JSON payload POSTed by the channel server
// when Claude Code emits a permission_request notification.
//
// ChannelURL is accepted for backward compatibility but MUST NOT be used as
// the verdict destination (AUDIT-2026-04-18 §N3 SSRF). The verdict goes to the
// agent as mail, addressed by name, so no URL is dialed at all.
type channelPermissionCallback struct {
	Type         string `json:"type"`          // "permission_request"
	RequestID    string `json:"request_id"`    // five-letter ID from Claude Code
	ToolName     string `json:"tool_name"`     // e.g. "Bash", "Write"
	Description  string `json:"description"`   // human-readable summary
	InputPreview string `json:"input_preview"` // tool args truncated to ~200 chars
	Agent        string `json:"agent"`         // agent identity (from channel meta); a token's agent wins
	ChannelURL   string `json:"channel_url"`   // IGNORED — retained for schema compat

	// The asking agent's bead and land, when its own token named them
	// (askerOf). Never read from the body.
	agentBeadID string
	land        string
}

func (h *PermissionCallbackHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var cb channelPermissionCallback
	if err := json.NewDecoder(r.Body).Decode(&cb); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if cb.RequestID == "" || cb.ToolName == "" {
		http.Error(w, "missing request_id or tool_name", http.StatusBadRequest)
		return
	}

	if h.bot != nil {
		if err := h.askerOf(r.Context(), &cb); err != nil {
			writeSayError(w, err)
			return
		}
	}

	if cb.ChannelURL != "" {
		h.logger.Warn("permission callback supplied channel_url — ignoring (SSRF defense); verdict URL resolved server-side",
			"request_id", cb.RequestID, "agent", cb.Agent)
	}

	h.logger.Info("permission callback received",
		"request_id", cb.RequestID,
		"tool", cb.ToolName,
		"agent", cb.Agent)

	// Post to Slack asynchronously — don't block the channel server.
	if h.bot != nil {
		go h.postPermissionPrompt(context.Background(), cb)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"received":true}`)
}

// askerOf settles the agent cb is for from the request's caller, as the
// conversation routes do (the top of api_conversation_caller.go):
//
//   - the agent's own per-agent token names it, with its bead and land; a body
//     agent naming another agent is refused;
//   - an OIDC admin names it in the body;
//   - the shared bridge bearer names it in the body too, which any pod holding
//     the bearer can claim, so it reaches only operator lands, and nothing
//     once SetSharedBearerAgents(false).
func (h *PermissionCallbackHandler) askerOf(ctx context.Context, cb *channelPermissionCallback) error {
	named := extractAgentName(cb.Agent)
	if c, ok := AgentCallerFromContext(ctx); ok {
		if named != "" && named != c.Agent {
			return sayErrorf(http.StatusForbidden,
				"the agent token is agent %s's; the request names agent %s", c.Agent, named)
		}
		cb.Agent, cb.agentBeadID, cb.land = c.Agent, c.BeadID, c.Land
		return nil
	}
	if named == "" {
		return sayErrorf(http.StatusBadRequest, "agent is required")
	}
	cb.Agent = named
	if operatorCaller(ctx) {
		return nil
	}
	if h.refuseSharedBearer {
		return errSharedBearerRetired
	}
	_, land := h.bot.agentBeadAndLand(ctx, named)
	return h.bot.refuseTenantLand(ctx, land, "agent "+named)
}

// postPermissionPrompt posts a Slack message with approve/deny buttons for the
// given permission request. The message is posted in the agent's thread.
func (h *PermissionCallbackHandler) postPermissionPrompt(ctx context.Context, cb channelPermissionCallback) {
	// Resolve the target channel and thread for this agent.
	channel, threadTS := h.bot.resolveThreadedChannel(ctx, cb.Agent)
	if channel == "" {
		channel = h.bot.channel // fallback to default channel
	}

	// Build the Slack message blocks.
	toolEmoji := ":wrench:"
	switch cb.ToolName {
	case "Bash":
		toolEmoji = ":terminal:"
	case "Write":
		toolEmoji = ":pencil2:"
	case "Edit":
		toolEmoji = ":pencil:"
	}

	headerText := fmt.Sprintf("%s *Permission Request* — `%s`", toolEmoji, cb.ToolName)
	if cb.Agent != "" {
		headerText = fmt.Sprintf("%s *Permission Request* — `%s` on _%s_", toolEmoji, cb.ToolName, cb.Agent)
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", headerText, false, false),
			nil, nil,
		),
	}

	if cb.Description != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", cb.Description, false, false),
			nil, nil,
		))
	}

	if cb.InputPreview != "" {
		// Truncate long previews for Slack's 3000-char block limit.
		preview := cb.InputPreview
		if len(preview) > 500 {
			preview = preview[:497] + "..."
		}
		// AUDIT §N9: prevent mrkdwn injection by neutralizing triple-backtick
		// runs inside attacker-controlled preview content.
		preview = sanitizeCodeFence(preview)
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("```%s```", preview), false, false),
			nil, nil,
		))
	}

	// The prompt is the card of a type=permission bead (decision_cards.go), so
	// the Commons shows it too.
	subject, land := h.bot.writePermissionBead(ctx, cb)

	// Button value encodes only requestID, agent name and the permission bead
	// (empty when its write failed), so the click closes exactly that bead. The
	// verdict is mailed to the agent; nothing in the payload names a URL to
	// dial (AUDIT §N3).
	buttonValue := cb.RequestID + "|" + cb.Agent + "|" + subject

	blocks = append(blocks,
		slack.NewActionBlock("permission_actions",
			slack.NewButtonBlockElement("permission_allow", buttonValue,
				slack.NewTextBlockObject("plain_text", "Allow", false, false),
			).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement("permission_deny", buttonValue,
				slack.NewTextBlockObject("plain_text", "Deny", false, false),
			).WithStyle(slack.StyleDanger),
		),
	)

	_, err := h.bot.postSubjectCard(ctx, land, subject, agentPrincipal(cb.Agent), channel, threadTS, "", blocks)
	if err != nil {
		h.logger.Error("failed to post permission prompt to Slack",
			"agent", cb.Agent, "request_id", cb.RequestID, "error", err)
	}
}

// sanitizeCodeFence neutralizes mrkdwn code-fence breakout in untrusted text.
// A triple-backtick inside a ```...``` section closes the block and lets the
// remainder render as mrkdwn (<!channel>, <!here>, clickable links). We insert
// a zero-width space between each pair of backticks so runs of length ≥2
// cannot be parsed as a fence. See AUDIT-2026-04-18 §N9.
func sanitizeCodeFence(s string) string {
	return strings.ReplaceAll(s, "`", "`\u200b")
}

// handlePermissionAllow processes an "Allow" button click for a permission request.
func (b *Bot) handlePermissionAllow(ctx context.Context, value string, callback slack.InteractionCallback) {
	b.resolvePermission(ctx, value, "allow", callback)
}

// handlePermissionDeny processes a "Deny" button click for a permission request.
func (b *Bot) handlePermissionDeny(ctx context.Context, value string, callback slack.InteractionCallback) {
	b.resolvePermission(ctx, value, "deny", callback)
}

// resolvePermission sends the verdict to the channel server and updates the
// Slack message. The clicker must be an operator or a member of the request's
// land (permissionAnswerer); anyone else is told so and nothing is sent.
func (b *Bot) resolvePermission(ctx context.Context, value, behavior string, callback slack.InteractionCallback) {
	// Parse buttonValue: "requestID|agent|permissionBeadID" ("requestID|agent"
	// on a prompt posted before the bead was carried). The verdict URL is
	// resolved server-side from the agent bead (never from attacker-controlled
	// input).
	parts := strings.SplitN(value, "|", 3)
	if len(parts) < 2 {
		b.logger.Error("invalid permission button value", "value", value)
		return
	}
	requestID := parts[0]
	agentName := parts[1]
	subjectID := ""
	if len(parts) == 3 {
		subjectID = parts[2]
	}

	req, refusal := b.permissionRequest(ctx, requestID, agentName, subjectID)
	if refusal != "" {
		b.refuseSlackUser(ctx, callback, refusal)
		return
	}
	var land string
	if req != nil {
		land = projectFromLabels(req.Labels)
	} else {
		_, land = b.agentBeadAndLand(ctx, agentName)
	}

	who, refusal := b.permissionAnswerer(ctx, callback.User.ID, land)
	if refusal != "" {
		b.logger.Info("permission answer refused",
			"request_id", requestID, "agent", agentName, "land", land, "user", callback.User.ID)
		b.refuseSlackUser(ctx, callback, refusal)
		return
	}

	err := b.deliverPermissionVerdict(ctx, req, requestID, agentName, behavior, who)

	// Replace the original message with the resolved state. A mention by user
	// id renders the clicker's current name; the username is theirs to change.
	_, _, _, updateErr := b.api.UpdateMessageContext(ctx, callback.Channel.ID, callback.Message.Timestamp,
		slack.MsgOptionBlocks(permissionResultBlocks(behavior, requestID, "<@"+who.SlackID+">", err)...),
	)
	if updateErr != nil {
		b.logger.Error("failed to update permission message", "error", updateErr)
	}
}

// deliverPermissionVerdict mails behavior for requestID to agentName and
// records the answer, who's, on the request bead req (nil for a prompt with no
// bead). Nothing dials the agent: its mail subscriber hands the verdict to the
// loopback gardenio-channel server, which answers the prompt. It returns the
// delivery failure, if any.
func (b *Bot) deliverPermissionVerdict(ctx context.Context, req *beadsapi.BeadDetail, requestID, agentName, behavior string, who permissionAnswerer) error {
	var err error
	if b.daemon == nil {
		err = fmt.Errorf("no bead daemon configured")
	} else {
		_, err = MailPermissionVerdict(ctx, b.daemon, b.logger, agentName, requestID, behavior)
	}

	if req != nil {
		b.closePermissionBead(ctx, req.ID, behavior, who, err)
	}
	if err != nil {
		b.logger.Error("failed to send permission verdict",
			"request_id", requestID, "behavior", behavior, "agent", agentName, "error", err)
	} else {
		b.logger.Info("permission verdict sent",
			"request_id", requestID, "behavior", behavior, "agent", agentName,
			"user", who.SlackID, "principal", who.Principal)
	}
	return err
}

// MailPermissionVerdict sends agentName a control:permission mail carrying
// requestID and behavior ("allow" or "deny") and returns its id. The agent's
// mail subscriber POSTs them to its loopback gardenio-channel /permission
// endpoint, which answers Claude Code's tool-approval prompt
// (cmd/gb/mail_permission.go). Plain mail text would not: the prompt waits on
// the channel, not on the conversation. The daemon accepts control: mail only
// from bridges, the controller and operators, so an agent cannot answer its
// own prompts this way.
func MailPermissionVerdict(ctx context.Context, daemon NudgeBeadClient, logger *slog.Logger, agentName, requestID, behavior string) (string, error) {
	if err := checkNudgeable(ctx, daemon, agentName); err != nil {
		return "", err
	}
	fields, err := json.Marshal(map[string]string{
		mailproto.FieldPermissionRequestID: requestID,
		mailproto.FieldPermissionBehavior:  behavior,
	})
	if err != nil {
		return "", err
	}
	id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       fmt.Sprintf("Permission %s: %s", requestID, behavior),
		Type:        "mail",
		Kind:        "data",
		Description: fmt.Sprintf("Permission decision from Slack for request %s: %s", requestID, behavior),
		Assignee:    agentName,
		Labels:      []string{mailproto.FromLabel(NudgeSender), mailproto.ControlLabel(mailproto.ControlPermission)},
		CreatedBy:   NudgeSender,
		Fields:      fields,
	})
	if err != nil {
		logger.Warn("permission verdict mail create failed", "agent", agentName, "error", err)
		return "", fmt.Errorf("create permission verdict mail for %q: %w", agentName, err)
	}
	return id, nil
}

// permissionResultBlocks is a permission prompt's resolved face: behavior by
// whom (Slack mrkdwn), with the delivery failure when there was one.
func permissionResultBlocks(behavior, requestID, by string, verdictErr error) []slack.Block {
	emoji := ":white_check_mark:"
	label := "Allowed"
	if behavior == "deny" {
		emoji = ":no_entry_sign:"
		label = "Denied"
	}
	resultText := fmt.Sprintf("%s *%s* by %s", emoji, label, by)
	if verdictErr != nil {
		resultText += fmt.Sprintf("\n:warning: _Verdict delivery failed: %s_", verdictErr)
	}
	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", resultText, false, false),
			nil, nil,
		),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("Permission `%s` — request `%s`", behavior, requestID), false, false),
		),
	}
}

// permissionRequest returns the open permission bead a click answers: the one
// the button names, checked against the request and agent it carries, or, on
// a prompt that predates the bead id, the single open bead for the pair. nil
// with no refusal is a prompt with no bead (its write failed, or it predates
// permission beads, or the pair is ambiguous): the verdict is still sent, and
// nothing is closed.
func (b *Bot) permissionRequest(ctx context.Context, requestID, agent, subjectID string) (*beadsapi.BeadDetail, slackPersonRefusal) {
	if b.daemon == nil {
		return nil, ""
	}
	if subjectID != "" {
		bd, err := b.daemon.GetBead(ctx, subjectID)
		switch {
		case err != nil:
			b.logger.Warn("permission: reading the request bead failed", "bead", subjectID, "error", err)
			return nil, "I couldn't read this permission request right now, so I did not answer it. Try again in a moment."
		case bd == nil || bd.Type != "permission" || bd.Fields["request_id"] != requestID || bd.Fields["agent"] != agent:
			b.logger.Error("permission: button names a bead that is not its request", "bead", subjectID, "request_id", requestID, "agent", agent)
			return nil, "This permission prompt does not match its request, so I did not answer it."
		case bd.Status == "closed":
			return nil, "This permission request was already answered."
		}
		return bd, ""
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"permission"},
		Statuses: []string{"open"},
		Fields:   map[string]string{"request_id": requestID, "agent": agent},
		Limit:    2,
	})
	if err != nil || len(res.Beads) != 1 || res.Beads[0] == nil {
		if err == nil && len(res.Beads) > 1 {
			b.logger.Warn("permission: request is ambiguous across lands; closing none", "request_id", requestID, "agent", agent)
		}
		return nil, ""
	}
	return res.Beads[0], ""
}

// agentBeadAndLand returns the bead and land of the agent named agent, "" for
// what is unknown. Names are not unique across lands: prefer the bead id an
// agent's own token carries where there is one.
func (b *Bot) agentBeadAndLand(ctx context.Context, agent string) (beadID, land string) {
	if detail := b.findAgentBeadForCard(ctx, agent); detail != nil && detail.ID != "" {
		beadID = detail.ID
		land = projectFromLabels(detail.Labels)
		if land == "" {
			land = strings.TrimSpace(detail.Fields["project"])
		}
	}
	if land == "" {
		land = b.landOf(nil, agent)
	}
	return beadID, land
}
