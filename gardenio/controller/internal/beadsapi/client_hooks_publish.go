package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// PublishHookEventRequest is the request body for POST /v1/hooks/publish.
type PublishHookEventRequest struct {
	// Subject is the NATS subject to publish to (e.g., "hooks.worker-1.PreToolUse").
	Subject string `json:"subject"`

	// Payload is the JSON-encoded hook event message.
	Payload json.RawMessage `json:"payload"`
}

// PublishHookEvent publishes a hook event to NATS via the kbeads daemon.
// The daemon handles the actual NATS connection, avoiding per-event connect overhead.
// Returns nil on success (204 No Content from daemon).
func (c *Client) PublishHookEvent(ctx context.Context, req PublishHookEventRequest) error {
	if err := c.doJSON(ctx, http.MethodPost, "/v1/hooks/publish", req, nil); err != nil {
		return fmt.Errorf("publishing hook event: %w", err)
	}
	return nil
}
