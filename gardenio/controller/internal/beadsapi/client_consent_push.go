package beadsapi

import (
	"context"
	"net/http"
)

// Standing consent to push CI fixes to the signed-in person's MRs
// (gd-QV2WuUjely), kept on their own user bead as consent_push_to_mr_auto by
// kbeads (/v1/users/me/consent-push). Both calls need the person's own OIDC
// bearer: kbeads answers 403 for static, fleet and machine callers and 404 when
// the person has no user bead. A consent-to-push decision addressed to someone
// who gave it is answered by the daemon, never by the filing agent.

// ConsentPush is the caller's standing consent.
type ConsentPush struct {
	UserBeadID string `json:"user_bead_id"`
	Auto       bool   `json:"auto"`
}

// MyConsentPush returns whether the caller has given standing consent.
func (c *Client) MyConsentPush(ctx context.Context) (*ConsentPush, error) {
	var resp ConsentPush
	if err := c.doJSON(ctx, http.MethodGet, "/v1/users/me/consent-push", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetMyConsentPush gives (auto=true) or withdraws (auto=false) the caller's
// standing consent.
func (c *Client) SetMyConsentPush(ctx context.Context, auto bool) (*ConsentPush, error) {
	var resp ConsentPush
	if err := c.doJSON(ctx, http.MethodPost, "/v1/users/me/consent-push", map[string]bool{"auto": auto}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
