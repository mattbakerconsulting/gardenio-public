package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Windows (Lands: the Window, PW.4 gd-tJe0GrtFDw): a land owner's or an
// operator's mint, list, revoke and preview of the read-only public windows
// on a plan or on a whole land (Lands: the land window, LW.5 gd-aUQRspcbeS).
// kbeads admits only the owner tier of the plan's land or an
// unrestricted operator, on their own sign-in: the shared static token and
// agent tokens are refused, so these calls go out on the person's bearer (gb's
// login session, or the viewer's WithUserBearer). A land window's target is
// the land itself: it has no plan, and its view is the land's projection.

// Window is a window as its owner sees it. It never carries the token.
type Window struct {
	ID string `json:"id"`
	// TargetKind is plan or land; PlanID is empty on a land window.
	TargetKind string `json:"target_kind,omitempty"`
	PlanID     string `json:"plan_id"`
	Land       string `json:"land"`
	// State is live, expired or revoked.
	State        string     `json:"state"`
	CreatedAt    time.Time  `json:"created_at"`
	CreatedBy    string     `json:"created_by"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokedBy    string     `json:"revoked_by,omitempty"`
	ViewCount    int64      `json:"view_count"`
	LastViewedAt *time.Time `json:"last_viewed_at,omitempty"`
}

// MintedWindow is a new window with its token, which the daemon returns this
// once and keeps only as a hash.
type MintedWindow struct {
	Window Window `json:"window"`
	Token  string `json:"token"`
	// Path is where the viewer serves the window: /w/<token>.
	Path string `json:"path"`
}

// CreateWindow mints a window on the plan epic planID that expires in
// expiresInDays (0 for the daemon's default, 30).
func (c *Client) CreateWindow(ctx context.Context, planID string, expiresInDays int) (*MintedWindow, error) {
	return c.createWindow(ctx, "plan_id", planID, expiresInDays)
}

// CreateLandWindow mints a window on the whole of land that expires in
// expiresInDays (0 for the daemon's default, 30).
func (c *Client) CreateLandWindow(ctx context.Context, land string, expiresInDays int) (*MintedWindow, error) {
	return c.createWindow(ctx, "land", land, expiresInDays)
}

func (c *Client) createWindow(ctx context.Context, key, target string, expiresInDays int) (*MintedWindow, error) {
	body := map[string]any{key: target}
	if expiresInDays != 0 {
		body["expires_in_days"] = expiresInDays
	}
	var out MintedWindow
	if err := c.doJSON(ctx, http.MethodPost, "/v1/windows", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListWindows returns land's windows, newest first: live, expired and revoked.
func (c *Client) ListWindows(ctx context.Context, land string) ([]Window, error) {
	var out struct {
		Windows []Window `json:"windows"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/windows?land="+url.QueryEscape(land), nil, &out); err != nil {
		return nil, err
	}
	return out.Windows, nil
}

// RevokeWindow revokes a window; its link stops opening on the next request.
// Revoking a revoked window returns it again.
func (c *Client) RevokeWindow(ctx context.Context, id string) (*Window, error) {
	var out struct {
		Window Window `json:"window"`
	}
	if err := c.doJSON(ctx, http.MethodDelete, "/v1/windows/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out.Window, nil
}

// PreviewWindow returns the projection a window minted now on planID would
// serve (the GET /v1/windows/self/view body, for planwindow.Decode), without
// minting one.
func (c *Client) PreviewWindow(ctx context.Context, planID string, expiresInDays int) (json.RawMessage, error) {
	return c.previewWindow(ctx, "plan", planID, expiresInDays)
}

// PreviewLandWindow returns the land projection a window minted now on land
// would serve (for planwindow.DecodeLand), without minting one.
func (c *Client) PreviewLandWindow(ctx context.Context, land string, expiresInDays int) (json.RawMessage, error) {
	return c.previewWindow(ctx, "land", land, expiresInDays)
}

func (c *Client) previewWindow(ctx context.Context, key, target string, expiresInDays int) (json.RawMessage, error) {
	q := url.Values{key: {target}}
	if expiresInDays != 0 {
		q.Set("expires_in_days", strconv.Itoa(expiresInDays))
	}
	var out json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/v1/windows/preview?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
