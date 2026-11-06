package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// WhoamiResponse mirrors the daemon's GET /v1/whoami payload. OperatorID is
// the authenticated subject (the OIDC email when Source="auth"); Source is
// one of "auth", "token", or "anonymous".
type WhoamiResponse struct {
	OperatorID string `json:"operator_id"`
	Source     string `json:"source"`
}

// Whoami returns the authenticated identity for the current daemon client.
// Used by gb identity link-operator to resolve the caller's OIDC email
// before looking up the matching user bead.
func (c *Client) Whoami(ctx context.Context) (*WhoamiResponse, error) {
	var resp WhoamiResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/whoami", nil, &resp); err != nil {
		return nil, fmt.Errorf("whoami: %w", err)
	}
	return &resp, nil
}

// MeResponse is the part of the daemon's GET /v1/me payload clients read.
// Agent and AgentBead are set only when the credential is a per-agent token:
// the daemon takes them from the token, never from X-Actor.
type MeResponse struct {
	OperatorID   string `json:"operator_id"`
	Source       string `json:"source"`
	TokenProject string `json:"token_project,omitempty"`
	Agent        string `json:"agent,omitempty"`
	AgentBead    string `json:"agent_bead,omitempty"`
	// Unrestricted is the daemon's scope verdict: false means kbeads confines
	// every read this credential makes to its lands.
	Unrestricted bool `json:"unrestricted"`
}

// Me returns who the daemon says this client's credential is.
func (c *Client) Me(ctx context.Context) (*MeResponse, error) {
	var resp MeResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/me", nil, &resp); err != nil {
		return nil, fmt.Errorf("me: %w", err)
	}
	return &resp, nil
}

// MeAs is Me for a credential that is not the client's own: the request goes
// out on bearer alone, whatever the client's read and write settings, so the
// answer is about that bearer and never about the client. beads-viewer asks it
// who a caller of its machine routes is.
func (c *Client) MeAs(ctx context.Context, bearer string) (*MeResponse, error) {
	if bearer == "" {
		return nil, fmt.Errorf("me: %w", ErrUserBearerRequiredForRead)
	}
	var resp MeResponse
	if err := c.doJSONAsUser(ctx, http.MethodGet, "/v1/me", bearer, nil, &resp); err != nil {
		return nil, fmt.Errorf("me: %w", err)
	}
	return &resp, nil
}
