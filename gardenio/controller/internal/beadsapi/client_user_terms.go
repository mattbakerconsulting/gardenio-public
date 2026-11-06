package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// UserTerms is the daemon's view of what the caller accepted: the body of
// GET and POST /v1/users/me/terms (kbeads http_user_terms.go, Lands step 7.4).
// TermsVersion is "" when the caller has never accepted.
type UserTerms struct {
	UserBeadID      string `json:"user_bead_id"`
	TermsVersion    string `json:"terms_version"`
	TermsAcceptedAt string `json:"terms_accepted_at"`
}

// AcceptTermsAsUser records that the person whose OIDC access token is bearer
// accepted the legal documents at version, on their own user bead. The call
// authenticates as that person (doJSONAsUser), never as the viewer: the daemon
// refuses every non-OIDC caller on this route, and the bead it writes is the
// token subject's, so there is nothing to point at anyone else.
func (c *Client) AcceptTermsAsUser(ctx context.Context, bearer, version string) (*UserTerms, error) {
	if bearer == "" {
		return nil, fmt.Errorf("accept terms: %w", ErrUserBearerRequired)
	}
	var resp UserTerms
	body := map[string]string{"terms_version": version}
	if err := c.doJSONAsUser(ctx, http.MethodPost, "/v1/users/me/terms", bearer, body, &resp); err != nil {
		return nil, fmt.Errorf("accept terms %s: %w", version, err)
	}
	return &resp, nil
}

// MyTermsAsUser reads what the person whose OIDC access token is bearer last
// accepted, from their own user bead, authenticated as that person.
func (c *Client) MyTermsAsUser(ctx context.Context, bearer string) (*UserTerms, error) {
	if bearer == "" {
		return nil, fmt.Errorf("read terms: %w", ErrUserBearerRequired)
	}
	var resp UserTerms
	if err := c.doJSONAsUser(ctx, http.MethodGet, "/v1/users/me/terms", bearer, nil, &resp); err != nil {
		return nil, fmt.Errorf("read terms: %w", err)
	}
	return &resp, nil
}
