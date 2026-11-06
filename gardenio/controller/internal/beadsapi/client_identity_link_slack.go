package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// SlackIdentityLinkRequest is the body for POST/DELETE /v1/identity-links/slack
// (kbeads http_identity_link_slack.go). The Slack user id is the one the
// bridge bound to the sign-in state, never one the browser supplied.
type SlackIdentityLinkRequest struct {
	SlackUserID string `json:"slack_user_id"`
	SlackTeamID string `json:"slack_team_id,omitempty"`
	LinkedVia   string `json:"linked_via,omitempty"`
}

// SlackIdentityLinkResponse reports the user bead the bind landed on.
type SlackIdentityLinkResponse struct {
	UserBeadID    string `json:"user_bead_id"`
	Email         string `json:"email"`
	SlackUserID   string `json:"slack_user_id"`
	AlreadyLinked bool   `json:"already_linked,omitempty"`
}

// LinkSlackIdentityAsUser records the Slack bind on the user bead of the
// person whose OIDC access token is bearer: the request authenticates as that
// person (doJSONAsUser), never as the bridge, so the daemon's audit row for the
// link is oidc:<person>. 409 (slack_id_conflict) means the id or the bead is
// already bound elsewhere and nothing was written.
func (c *Client) LinkSlackIdentityAsUser(ctx context.Context, bearer string, req SlackIdentityLinkRequest) (*SlackIdentityLinkResponse, error) {
	if bearer == "" {
		return nil, fmt.Errorf("link slack identity: %w", ErrUserBearerRequired)
	}
	var resp SlackIdentityLinkResponse
	if err := c.doJSONAsUser(ctx, http.MethodPost, "/v1/identity-links/slack", bearer, req, &resp); err != nil {
		return nil, fmt.Errorf("link slack identity %s: %w", req.SlackUserID, err)
	}
	return &resp, nil
}

// UnlinkSlackIdentityAsUser clears the Slack bind from the person's own user
// bead, authenticated as the person. Idempotent on the daemon.
func (c *Client) UnlinkSlackIdentityAsUser(ctx context.Context, bearer string, req SlackIdentityLinkRequest) (*SlackIdentityLinkResponse, error) {
	if bearer == "" {
		return nil, fmt.Errorf("unlink slack identity: %w", ErrUserBearerRequired)
	}
	var resp SlackIdentityLinkResponse
	if err := c.doJSONAsUser(ctx, http.MethodDelete, "/v1/identity-links/slack", bearer, req, &resp); err != nil {
		return nil, fmt.Errorf("unlink slack identity %s: %w", req.SlackUserID, err)
	}
	return &resp, nil
}
