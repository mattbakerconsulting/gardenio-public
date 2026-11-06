// mail_permission.go — control:permission mail (mailproto.ControlPermission).
//
// A Slack Allow/Deny on a Claude Code tool-approval prompt reaches the agent
// as control:permission mail carrying request_id and behavior. Claude Code
// waits for the answer on its gardenio-channel MCP server, not in the
// conversation, so the subscriber POSTs the verdict to that server's loopback
// /permission endpoint (images/agent/channel/gardenio-channel.ts), which
// relays it to Claude Code. Nothing dials in to the agent.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailproto"
	"gardenio/controller/internal/mailsub"
)

// Tunables are package variables so tests can shrink them: the verdict is
// tried permissionPostAttempts times, permissionPostRetryDelay apart, in case
// the channel server is between restarts.
var (
	permissionPostAttempts   = 3
	permissionPostRetryDelay = 2 * time.Second
)

// permissionControl is the mail subscriber's OnControl hook for
// control:permission mail. Other verbs are logged and left as they were.
type permissionControl struct {
	daemon mailsub.Daemon
	// url is the channel server's /permission endpoint on loopback; token
	// is its bearer token.
	url    string
	token  string
	client *http.Client
	logf   func(string, ...any)
}

// newPermissionControl reads the channel server's port and token from the
// same env gb setup hands the server (setup_config.go ensureGardenioChannel).
func newPermissionControl(d mailsub.Daemon, logf func(string, ...any)) *permissionControl {
	return &permissionControl{
		daemon: d,
		url:    "http://127.0.0.1:" + envOr("GARDENIO_CHANNEL_PORT", "8789") + "/permission",
		token:  envOr("GARDENIO_CHANNEL_TOKEN", os.Getenv("COOP_BROKER_TOKEN")),
		client: &http.Client{Timeout: 10 * time.Second},
		logf:   logf,
	}
}

// OnControl hands a control:permission verdict to the channel server, then
// closes the mail stamped control_acted, delivered or not: a verdict answers
// one prompt of one session, so left open it would only be replayed into the
// next session, which never asked.
func (p *permissionControl) OnControl(ctx context.Context, verb string, m beadsapi.BeadDetail) {
	if verb != mailproto.ControlPermission {
		p.logf("mail subscriber: unhandled control mail %s (verb %s)", m.ID, verb)
		return
	}
	requestID := m.Fields[mailproto.FieldPermissionRequestID]
	behavior := m.Fields[mailproto.FieldPermissionBehavior]
	if err := p.post(ctx, requestID, behavior); err != nil {
		p.logf("permission verdict %s for request %q not delivered: %v", m.ID, requestID, err)
	} else {
		p.logf("permission verdict %s delivered: request %s %s (from %s)", m.ID, requestID, behavior, mailproto.Sender(m.Labels))
	}
	if m.Status == "closed" || p.daemon == nil {
		return
	}
	acted := map[string]string{mailproto.FieldControlActed: time.Now().UTC().Format(time.RFC3339)}
	if err := p.daemon.CloseBead(ctx, m.ID, acted); err != nil {
		p.logf("closing permission verdict mail %s failed: %v", m.ID, err)
	}
}

// post sends the verdict to the channel server, retrying a failed attempt.
// A malformed verdict or a refusal (4xx) is not retried.
func (p *permissionControl) post(ctx context.Context, requestID, behavior string) error {
	if requestID == "" || (behavior != "allow" && behavior != "deny") {
		return fmt.Errorf("malformed verdict (request_id %q, behavior %q)", requestID, behavior)
	}
	if p.token == "" {
		return fmt.Errorf("no channel token (GARDENIO_CHANNEL_TOKEN or COOP_BROKER_TOKEN)")
	}
	body, err := json.Marshal(map[string]string{"request_id": requestID, "behavior": behavior})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 1; attempt <= permissionPostAttempts; attempt++ {
		var retry bool
		retry, lastErr = p.postOnce(ctx, body)
		if lastErr == nil || !retry {
			return lastErr
		}
		if attempt < permissionPostAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(permissionPostRetryDelay):
			}
		}
	}
	return fmt.Errorf("after %d attempts: %w", permissionPostAttempts, lastErr)
}

func (p *permissionControl) postOnce(ctx context.Context, body []byte) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode < 500:
		return false, fmt.Errorf("channel server refused: %s %s", resp.Status, bytes.TrimSpace(msg))
	default:
		return true, fmt.Errorf("channel server: %s %s", resp.Status, bytes.TrimSpace(msg))
	}
}
