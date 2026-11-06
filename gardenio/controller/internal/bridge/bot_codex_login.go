package bridge

// bot_codex_login.go — the "Start device login" button on the keeper's Codex
// re-login escalation (ADR 0007). The press is the explicit operator act that
// starts a ChatGPT-plan device login on the leader controller. The verification
// URL and one-time code come back as an EPHEMERAL reply visible only to the
// person who pressed: they are never written to the escalation bead, a comment,
// a log line or any agent-readable surface.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	codexDeviceLoginActionID = "codex_device_login_start"
	// CodexDeviceLoginSpecAction is operation_spec.action on the escalation.
	CodexDeviceLoginSpecAction = "codex_device_login"
	// codexLoginCapability authorises the press: starting a login is an
	// escalation-approving act (it grants the cluster the owner's plan).
	codexLoginCapability = "approve:escalation"
)

// codexDeviceLoginAccount returns the account an escalation asks to re-login, from
// its operation_spec. ok is false for any other escalation.
func codexDeviceLoginAccount(fields map[string]string) (string, bool) {
	raw := strings.TrimSpace(fields["operation_spec"])
	if raw == "" {
		return "", false
	}
	var spec struct {
		Action  string `json:"action"`
		Account string `json:"account"`
	}
	if json.Unmarshal([]byte(raw), &spec) != nil || spec.Action != CodexDeviceLoginSpecAction || spec.Account == "" {
		return "", false
	}
	return spec.Account, true
}

// codexDeviceLoginButton is the extra button for an escalation card, nil if the
// escalation is not a Codex re-login.
func codexDeviceLoginButton(bead BeadEvent) *slack.ButtonBlockElement {
	if _, ok := codexDeviceLoginAccount(bead.Fields); !ok {
		return nil
	}
	return slack.NewButtonBlockElement(codexDeviceLoginActionID, bead.ID,
		slack.NewTextBlockObject("plain_text", "Start device login", false, false))
}

// handleCodexDeviceLoginStart handles a press. beadID comes from the button value
// but everything that matters (type, status, account) is re-read from the bead.
func (b *Bot) handleCodexDeviceLoginStart(ctx context.Context, beadID string, callback slack.InteractionCallback) {
	channel := callback.Channel.ID
	th := b.themeFor(ctx, channel)
	reply := func(text string) { b.postInteractionEphemeral(ctx, callback, channel, text) }
	warn, info := th.Semantic(SemWarn), th.Semantic(SemInfo)

	user := b.UserDirectory().LookupBySlackID(ctx, callback.User.ID)
	if user == nil || !user.HasCapability(codexLoginCapability) {
		b.logger.Info("codex device login denied", "bead", beadID, "slack_user", callback.User.ID)
		reply(th.Semantic(SemDenied) + " Starting a Codex login needs the `" + codexLoginCapability + "` capability.")
		return
	}
	bead, err := b.daemon.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		reply(warn + " Could not read the escalation; try again.")
		return
	}
	account, ok := codexDeviceLoginAccount(bead.Fields)
	if bead.Type != "escalation" || !ok {
		reply(warn + " That is not a Codex re-login escalation.")
		return
	}
	if bead.Status != "open" && bead.Status != "in_progress" {
		reply(info + " That escalation is already " + bead.Status + ".")
		return
	}
	if b.controllerURL == "" {
		reply(warn + " The controller URL is not configured here. Use the fallback: `gb codex login`.")
		return
	}

	res, status, errMsg := b.startCodexDeviceLogin(ctx, account, beadID, user.Email)
	if errMsg != "" {
		b.logger.Warn("codex device login start failed", "bead", beadID, "account", account, "status", status)
		reply(warn + " Could not start the device login: " + errMsg + ". Fallback: `gb codex login`.")
		return
	}
	b.logger.Info("codex device login handed to operator", "bead", beadID, "account", account, "operator", user.Email)

	reply(fmt.Sprintf("%s *Codex device login for `%s`*\n"+
		"1. Open <%s>, signed in as the *ChatGPT plan owner*.\n"+
		"2. Enter the code `%s`.\n"+
		"Valid until %s. Approve only a code you just requested here. Only you can see this message; the code is not stored anywhere. "+
		"The controller stores the new login itself and closes the escalation.",
		th.Semantic(SemLink), account, res.VerificationURL, res.UserCode, res.ExpiresAt.UTC().Format("15:04 UTC")))
}

type codexDeviceLoginResponse struct {
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// startCodexDeviceLogin asks the controller to start a flow. It retries a few
// times on 503 because a non-leader replica can answer. The response body is
// never logged: on success it holds the code.
func (b *Bot) startCodexDeviceLogin(ctx context.Context, account, escalationID, operator string) (codexDeviceLoginResponse, int, string) {
	body, _ := json.Marshal(map[string]string{"account": account, "escalation_id": escalationID, "started_by": operator})
	url := strings.TrimRight(b.controllerURL, "/") + "/api/v1/codex/device-login"
	client := &http.Client{Timeout: 45 * time.Second}
	var status int
	var errMsg string
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return codexDeviceLoginResponse{}, 0, "internal error"
		}
		req.Header.Set("Content-Type", "application/json")
		setControllerAPIAuth(req)
		resp, err := client.Do(req)
		if err != nil {
			status, errMsg = 0, "controller unreachable"
		} else {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
			resp.Body.Close()
			status = resp.StatusCode
			if status == http.StatusOK {
				var out codexDeviceLoginResponse
				if json.Unmarshal(data, &out) != nil || out.UserCode == "" || out.VerificationURL == "" {
					return codexDeviceLoginResponse{}, status, "unreadable controller response"
				}
				return out, status, ""
			}
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(data, &e)
			errMsg = e.Error
			if errMsg == "" {
				errMsg = fmt.Sprintf("controller answered HTTP %d", status)
			}
			if status != http.StatusServiceUnavailable {
				return codexDeviceLoginResponse{}, status, errMsg
			}
		}
		select {
		case <-ctx.Done():
			return codexDeviceLoginResponse{}, status, errMsg
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return codexDeviceLoginResponse{}, status, errMsg
}
