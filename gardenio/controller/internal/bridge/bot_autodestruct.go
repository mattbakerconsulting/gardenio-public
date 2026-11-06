package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// handleAutoDestructCommand processes the /autodestruct slash command.
// Usage: /autodestruct [clear|status]
//
// Without arguments, prompts for confirmation before triggering autodestruct.
// With "clear", resumes normal reconciliation.
// With "status", shows current autodestruct state.
func (b *Bot) handleAutoDestructCommand(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	if !b.isAutoDestructAllowed(cmd.UserID) {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(th.Semantic(SemDenied)+" You are not authorized to use autodestruct.", false))
		return
	}

	subcommand := strings.TrimSpace(cmd.Text)

	switch subcommand {
	case "clear":
		b.autoDestructClear(ctx, cmd)
	case "status":
		b.autoDestructStatus(ctx, cmd)
	default:
		b.autoDestructConfirm(ctx, cmd)
	}
}

// isAutoDestructAllowed checks if the Slack user ID is in the AUTODESTRUCT_ALLOWED_USERS list.
func (b *Bot) isAutoDestructAllowed(userID string) bool {
	allowed := os.Getenv("AUTODESTRUCT_ALLOWED_USERS")
	if allowed == "" {
		return false
	}
	for _, id := range strings.Split(allowed, ",") {
		if strings.TrimSpace(id) == userID {
			return true
		}
	}
	return false
}

// autoDestructConfirm posts an ephemeral confirmation message with Confirm/Cancel buttons.
func (b *Bot) autoDestructConfirm(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn",
				th.Semantic(SemWarn)+" *Autodestruct* will force-delete all agent pods and pause reconciliation.\n\nAre you sure?",
				false, false),
			nil, nil),
		slack.NewActionBlock("autodestruct_confirm_actions",
			slack.NewButtonBlockElement(
				"autodestruct_confirm", "confirm",
				slack.NewTextBlockObject("plain_text", "Confirm Autodestruct", false, false),
			).WithStyle(slack.StyleDanger),
			slack.NewButtonBlockElement(
				"autodestruct_cancel", "cancel",
				slack.NewTextBlockObject("plain_text", "Cancel", false, false),
			),
		),
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionBlocks(blocks...))
}

// autoDestructClear calls the controller's autodestruct/clear endpoint.
func (b *Bot) autoDestructClear(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	result, err := b.callAutoDestruct(ctx, "POST", "/autodestruct/clear", cmd.UserID)
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Autodestruct clear failed: %s", err), false))
		return
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemOK)+" Autodestruct cleared. Reconciliation resumed.\n%s", result), false))

	// Announce to channel.
	user := b.resolveSlackUser(cmd.UserID)
	_, _, _ = b.api.PostMessage(cmd.ChannelID,
		slack.MsgOptionText(fmt.Sprintf("%s *Autodestruct cleared* by %s — reconciliation resumed.", th.Semantic(SemResumed), user), false))
}

// autoDestructStatus calls the controller's autodestruct/status endpoint.
func (b *Bot) autoDestructStatus(ctx context.Context, cmd slack.SlashCommand) {
	th := b.themeFor(ctx, cmd.ChannelID)
	result, err := b.callAutoDestruct(ctx, "GET", "/autodestruct/status", cmd.UserID)
	if err != nil {
		_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
			slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemFail)+" Failed to get autodestruct status: %s", err), false))
		return
	}

	_, _ = b.api.PostEphemeral(cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemInfo)+" Autodestruct status:\n```%s```", result), false))
}

// handleAutoDestructButtonAction processes the confirm/cancel button clicks.
func (b *Bot) handleAutoDestructButtonAction(ctx context.Context, actionID string, callback slack.InteractionCallback) {
	userID := callback.User.ID
	channelID := callback.Channel.ID
	th := b.themeFor(ctx, channelID)

	if !b.isAutoDestructAllowed(userID) {
		_, _ = b.api.PostEphemeral(channelID, userID,
			slack.MsgOptionText(th.Semantic(SemDenied)+" You are not authorized to use autodestruct.", false))
		return
	}

	switch actionID {
	case "autodestruct_confirm":
		result, err := b.callAutoDestruct(ctx, "POST", "/autodestruct", userID)
		if err != nil {
			_, _ = b.api.PostEphemeral(channelID, userID,
				slack.MsgOptionText(fmt.Sprintf("%s Autodestruct failed: %s", th.Semantic(SemFail), err), false))
			return
		}

		// Parse killed count from response.
		var resp struct {
			Killed int `json:"killed"`
		}
		_ = json.Unmarshal([]byte(result), &resp)

		_, _ = b.api.PostEphemeral(channelID, userID,
			slack.MsgOptionText(fmt.Sprintf("%s Autodestruct triggered — %d pod(s) terminated.", th.Semantic(SemStop), resp.Killed), false))

		// Announce to channel.
		user := b.resolveSlackUser(userID)
		_, _, _ = b.api.PostMessage(channelID,
			slack.MsgOptionText(fmt.Sprintf("%s *Autodestruct triggered* by %s — %d pod(s) terminated. Use `/autodestruct clear` to resume.", th.Semantic(SemStop), user, resp.Killed), false))

	case "autodestruct_cancel":
		_, _ = b.api.PostEphemeral(channelID, userID,
			slack.MsgOptionText(th.Semantic(SemStop)+" Autodestruct cancelled.", false))
	}
}

// callAutoDestruct makes an HTTP request to the controller's autodestruct endpoint.
func (b *Bot) callAutoDestruct(ctx context.Context, method, path, userID string) (string, error) {
	if b.controllerURL == "" {
		return "", fmt.Errorf("controller URL not configured")
	}

	token := os.Getenv("AUTODESTRUCT_TOKEN")
	if token == "" {
		return "", fmt.Errorf("AUTODESTRUCT_TOKEN not configured")
	}

	url := strings.TrimRight(b.controllerURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Actor", fmt.Sprintf("slack:%s", userID))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling controller: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return string(body), nil
}

// resolveSlackUser returns a display name for a Slack user ID.
func (b *Bot) resolveSlackUser(userID string) string {
	user, err := b.api.GetUserInfo(userID)
	if err != nil {
		return fmt.Sprintf("<@%s>", userID)
	}
	if user.RealName != "" {
		return user.RealName
	}
	if user.Name != "" {
		return user.Name
	}
	return fmt.Sprintf("<@%s>", userID)
}
