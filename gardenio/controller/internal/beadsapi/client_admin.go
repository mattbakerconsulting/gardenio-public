package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// RevokeUserRequest is the request body for POST /v1/admin/revoke-user.
type RevokeUserRequest struct {
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
	Disable  bool   `json:"disable,omitempty"`
	DryRun   bool   `json:"dry_run,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RevokeUserResponse is the response from POST /v1/admin/revoke-user.
type RevokeUserResponse struct {
	UserID          string `json:"user_id"`
	Username        string `json:"username"`
	Email           string `json:"email"`
	SessionsRevoked int    `json:"sessions_revoked"`
	UserDisabled    bool   `json:"user_disabled"`
	DryRun          bool   `json:"dry_run,omitempty"`
}

// RevokeUser revokes all Keycloak sessions for a user and optionally disables
// their account. Requires admin privileges.
func (c *Client) RevokeUser(ctx context.Context, req RevokeUserRequest) (*RevokeUserResponse, error) {
	var resp RevokeUserResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/revoke-user", req, &resp); err != nil {
		return nil, fmt.Errorf("revoke user: %w", err)
	}
	return &resp, nil
}
