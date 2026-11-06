package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// SlackOperatorAlias mirrors server.SlackOperatorAlias — one row of the slack
// alias resync feed (ADR docs/adr/human-attribution-identity-v2.md §7
// follow-up #1).
type SlackOperatorAlias struct {
	OperatorID int64  `json:"operator_id"`
	Subject    string `json:"subject"`
	Display    string `json:"display"`
	Email      string `json:"email"`
}

// ListSlackOperatorAliasesResponse is the response body for
// GET /v1/admin/operators/slack-aliases.
type ListSlackOperatorAliasesResponse struct {
	Aliases []SlackOperatorAlias `json:"aliases"`
}

// UpdateOperatorIdentityRequest is the body for
// POST /v1/admin/operators/{id}/identity. The bridge holds a machine token, so
// it may only send FromUserBead: the daemon derives display and email from the
// person's type:user bead (gd-iRFGU6xSK7).
type UpdateOperatorIdentityRequest struct {
	FromUserBead bool `json:"from_user_bead"`
}

// UpdateOperatorIdentityResponse signals whether the operator row was
// matched. Updated=false means the id was not found (operator merged or
// deleted) — caller should drop the alias from its retry queue. NoUserBead
// means no user bead resolves to the operator, so nothing was written.
type UpdateOperatorIdentityResponse struct {
	Updated    bool   `json:"updated"`
	NoUserBead bool   `json:"no_user_bead,omitempty"`
	Display    string `json:"display,omitempty"`
	Email      string `json:"email,omitempty"`
}

// ListSlackOperatorAliases returns every (kind=slack) alias joined with its
// parent operator's current display/email. Admin-only.
func (c *Client) ListSlackOperatorAliases(ctx context.Context) (*ListSlackOperatorAliasesResponse, error) {
	var resp ListSlackOperatorAliasesResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/admin/operators/slack-aliases", nil, &resp); err != nil {
		return nil, fmt.Errorf("list slack operator aliases: %w", err)
	}
	return &resp, nil
}

// ResyncOperatorIdentity asks the daemon to rewrite operators.display and
// operators.email for the given operator id from the person's user bead.
// Idempotent — replaying is safe. Returns Updated=false when no row matched
// (operator merged/deleted), so callers can drop the alias from retry queues.
func (c *Client) ResyncOperatorIdentity(ctx context.Context, operatorID int64) (*UpdateOperatorIdentityResponse, error) {
	body := UpdateOperatorIdentityRequest{FromUserBead: true}
	path := fmt.Sprintf("/v1/admin/operators/%d/identity", operatorID)
	var resp UpdateOperatorIdentityResponse
	if err := c.doJSON(ctx, http.MethodPost, path, body, &resp); err != nil {
		return nil, fmt.Errorf("resync operator identity %d: %w", operatorID, err)
	}
	return &resp, nil
}
