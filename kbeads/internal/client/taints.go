package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CreateTaintRequest is the body for POST /v1/taints. Preset and ScrubPolicy
// are mutually exclusive — see model.PresetName / docs/adr/taints.md §10.2.
type CreateTaintRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	OwnerTeam   string          `json:"owner_team"`
	Preset      string          `json:"preset,omitempty"`
	ScrubPolicy json.RawMessage `json:"scrub_policy,omitempty"`
	// Effects is the taint's effect set (docs/adr/taints.md §15.2). Nil
	// defaults server-side to {gate}.
	Effects []string `json:"effects,omitempty"`
}

// TaintView is one taint as returned by CreateTaint, GetTaint, or a row of
// ListTaints. ScrubPolicy, ID, and the timestamps are empty/omitted when the
// caller is neither the taint's owner team nor an admin — the daemon
// restricts GET /v1/taints/{name} to Name/Description only in that case
// (task gd-sygX3b8TAg acceptance criteria), and omits ScrubPolicy per-row
// from GET /v1/taints the same way (ADR docs/adr/taints.md §9.1).
type TaintView struct {
	ID          int64           `json:"id,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	OwnerTeam   string          `json:"owner_team,omitempty"`
	ScrubPolicy json.RawMessage `json:"scrub_policy,omitempty"`
	Effects     []string        `json:"effects,omitempty"`
	CreatedAt   time.Time       `json:"created_at,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at,omitempty"`
	RetiredAt   *time.Time      `json:"retired_at,omitempty"`
}

// ListTaintsResponse is the response from ListTaints (GET /v1/taints).
type ListTaintsResponse struct {
	Taints []TaintView `json:"taints"`
}

// CreateTaint registers a new taint (POST /v1/taints). Admin-only on the
// server (RoleAdmin) — a role check, not a team check.
func (c *HTTPClient) CreateTaint(ctx context.Context, req *CreateTaintRequest) (*TaintView, error) {
	var resp TaintView
	if err := c.doJSON(ctx, http.MethodPost, "/v1/taints", req, &resp); err != nil {
		return nil, fmt.Errorf("creating taint %s: %w", req.Name, err)
	}
	return &resp, nil
}

// ListTaints lists every taint in the registry (GET /v1/taints). Any viewer
// may call this; rows for taints the caller does not own (and is not an
// admin for) come back without ScrubPolicy.
func (c *HTTPClient) ListTaints(ctx context.Context) (*ListTaintsResponse, error) {
	var resp ListTaintsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/taints", nil, &resp); err != nil {
		return nil, fmt.Errorf("listing taints: %w", err)
	}
	return &resp, nil
}

// GetTaint fetches a single taint by name (GET /v1/taints/{name}). Returns
// an *APIError with StatusCode 404 when no such taint exists. The owner
// team or an admin gets the full row including ScrubPolicy; anyone else
// gets Name/Description only.
func (c *HTTPClient) GetTaint(ctx context.Context, name string) (*TaintView, error) {
	var resp TaintView
	path := "/v1/taints/" + url.PathEscape(name)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting taint %s: %w", name, err)
	}
	return &resp, nil
}
