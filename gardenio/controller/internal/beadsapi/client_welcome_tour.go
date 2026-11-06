package beadsapi

import (
	"context"
	"net/http"
)

// The welcome tour's seen flag (Lands Track B step 9.14b): when the signed-in
// person had the /garden welcome tour, kept on their own user bead as
// welcome_tour_seen_at by kbeads (/v1/users/me/welcome-tour). Both calls need
// the person's own OIDC bearer: kbeads answers 403 for static, fleet and
// machine callers and 404 when the person has no user bead.

// WelcomeTour is the caller's welcome tour state; SeenAt is "" while it is due.
type WelcomeTour struct {
	UserBeadID string `json:"user_bead_id"`
	SeenAt     string `json:"seen_at"`
}

// MyWelcomeTour returns when the caller had the welcome tour.
func (c *Client) MyWelcomeTour(ctx context.Context) (*WelcomeTour, error) {
	var resp WelcomeTour
	if err := c.doJSON(ctx, http.MethodGet, "/v1/users/me/welcome-tour", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SeenMyWelcomeTour records that the caller has had the welcome tour; kbeads
// keeps the first time.
func (c *Client) SeenMyWelcomeTour(ctx context.Context) (*WelcomeTour, error) {
	var resp WelcomeTour
	if err := c.doJSON(ctx, http.MethodPost, "/v1/users/me/welcome-tour", struct{}{}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
