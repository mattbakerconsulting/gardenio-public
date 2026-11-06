// Package edgeauth provides GitLab PAT validation and group-membership
// checks for the gardenio-edge-auth service (epic ka-0000003384, task
// ka-0000006756). It validates a personal access token against the GitLab
// API and confirms the user is a member of a configured group, with TTL
// caching to keep request volume off the upstream API.
package edgeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Identity is the operator identity resolved from a valid GitLab PAT whose
// owner is a member of the configured group.
type Identity struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Name     string `json:"name"`
}

// Validator validates a GitLab PAT and returns the operator identity if the
// token is valid and the user is a member of the configured group.
//
// On ErrNotInGroup the returned Identity is the partial identity resolved
// from /user (UserID, Username, Email, Name) so callers can emit audit
// records that attribute the deny to a specific actor. On any other error
// the returned Identity is nil.
type Validator interface {
	Validate(ctx context.Context, pat string) (*Identity, error)
}

// Errors returned by Validate. The shim translates these to HTTP 401/403.
var (
	// ErrEmptyToken is returned when the caller passes an empty PAT.
	ErrEmptyToken = errors.New("edgeauth: empty token")

	// ErrInvalidToken is returned when GitLab rejects the PAT (401 from
	// /user). Negative-cached for the configured TTL.
	ErrInvalidToken = errors.New("edgeauth: invalid gitlab token")

	// ErrNotInGroup is returned when the PAT resolves to a real GitLab user
	// who is not a member of the configured group. Negative-cached.
	ErrNotInGroup = errors.New("edgeauth: user is not in required group")

	// ErrGitLabUnavailable is returned when GitLab returns an unexpected
	// status or the request fails. Not cached.
	ErrGitLabUnavailable = errors.New("edgeauth: gitlab API unavailable")
)

// GitLabConfig configures GitLabValidator.
type GitLabConfig struct {
	// BaseURL is the GitLab instance base (e.g. https://gitlab.com). The
	// validator appends /api/v4 paths.
	BaseURL string

	// Group is the full path of the GitLab group whose members are
	// authorised (e.g. "gardenio" or "acme/gardenio"). Inherited
	// membership counts; the validator uses the /members/all endpoint.
	Group string

	// HTTPClient is used for outbound calls. If nil, a sane default with a
	// short timeout is used.
	HTTPClient *http.Client

	// PositiveTTL is how long successful validations are cached. Defaults
	// to 5 minutes per stage3 plan.
	PositiveTTL time.Duration

	// NegativeTTL is how long failed validations (invalid token, not in
	// group) are cached. Defaults to 30 seconds.
	NegativeTTL time.Duration

	// Now is overridable for tests. Defaults to time.Now.
	Now func() time.Time
}

// GitLabValidator validates PATs against the GitLab REST API.
type GitLabValidator struct {
	baseURL string
	group   string
	client  *http.Client
	cache   *cache
}

// NewGitLabValidator builds a validator. The caller is responsible for
// supplying a non-empty Group; an empty Group is rejected so misconfigured
// deployments fail closed at startup rather than admit everyone.
func NewGitLabValidator(cfg GitLabConfig) (*GitLabValidator, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		return nil, errors.New("edgeauth: GitLabConfig.BaseURL is required")
	}
	if cfg.Group == "" {
		return nil, errors.New("edgeauth: GitLabConfig.Group is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	pos := cfg.PositiveTTL
	if pos <= 0 {
		pos = 5 * time.Minute
	}
	neg := cfg.NegativeTTL
	if neg <= 0 {
		neg = 30 * time.Second
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &GitLabValidator{
		baseURL: base,
		group:   cfg.Group,
		client:  client,
		cache:   newCache(pos, neg, now),
	}, nil
}

// Validate validates a PAT and returns the operator identity. Results are
// cached; repeat calls with the same PAT inside the TTL hit the cache.
func (v *GitLabValidator) Validate(ctx context.Context, pat string) (*Identity, error) {
	if pat == "" {
		return nil, ErrEmptyToken
	}
	if id, err, ok := v.cache.get(pat); ok {
		// id may be non-nil even when err != nil: ErrNotInGroup caches
		// the partial identity so the deny audit can attribute an actor.
		return id, err
	}

	id, err := v.validateUncached(ctx, pat)
	if errors.Is(err, ErrInvalidToken) {
		v.cache.putNegative(pat, nil, err)
		return nil, err
	}
	if errors.Is(err, ErrNotInGroup) {
		// id carries the partial identity from /user so audit records on
		// the deny can attribute the request to a specific actor.
		v.cache.putNegative(pat, id, err)
		return id, err
	}
	if err != nil {
		// Transient failures (GitLab 5xx, network errors) are not cached
		// so the next request gets a fresh attempt.
		return nil, err
	}
	v.cache.putPositive(pat, id)
	return id, nil
}

func (v *GitLabValidator) validateUncached(ctx context.Context, pat string) (*Identity, error) {
	id, err := v.fetchUser(ctx, pat)
	if err != nil {
		return nil, err
	}
	if err := v.checkGroupMembership(ctx, pat, id.UserID); err != nil {
		// Return the partial identity alongside the error so the caller
		// can stamp an audit record with the actor that was denied.
		return id, err
	}
	return id, nil
}

// fetchUser calls GET /api/v4/user, which both validates the PAT (returns
// 401 if invalid) and returns the user record.
func (v *GitLabValidator) fetchUser(ctx context.Context, pat string) (*Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.baseURL+"/api/v4/user", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build user request: %v", ErrGitLabUnavailable, err)
	}
	// Authorization: Bearer accepts both personal/group access tokens AND
	// OAuth2 tokens (e.g. the token a `glab auth login` session holds),
	// whereas PRIVATE-TOKEN only accepts PATs. Using Bearer lets operators
	// authenticate with their existing glab session token — no PAT to mint.
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitLabUnavailable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusUnauthorized, http.StatusForbidden:
		drain(resp.Body)
		return nil, ErrInvalidToken
	default:
		drain(resp.Body)
		return nil, fmt.Errorf("%w: /user returned %d", ErrGitLabUnavailable, resp.StatusCode)
	}

	// email is the account's primary email, read with the caller's own
	// token, not a claim the user asserts. GitLab only makes an address
	// primary once it is confirmed: a changed primary waits in
	// unconfirmed_email until the confirmation link is followed, so there is
	// no email_verified to check here (reviewed for gd-ddpTGbl4Te).
	var body struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Email    string `json:"email"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode /user: %v", ErrGitLabUnavailable, err)
	}
	if body.ID == 0 {
		return nil, fmt.Errorf("%w: /user returned no id", ErrGitLabUnavailable)
	}
	return &Identity{
		UserID:   body.ID,
		Username: body.Username,
		Email:    body.Email,
		Name:     body.Name,
	}, nil
}

// checkGroupMembership calls GET /api/v4/groups/<group>/members/all/<user_id>.
// The "all" variant includes inherited membership from parent groups.
func (v *GitLabValidator) checkGroupMembership(ctx context.Context, pat string, userID int64) error {
	path := fmt.Sprintf("/api/v4/groups/%s/members/all/%d",
		url.PathEscape(v.group), userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%w: build members request: %v", ErrGitLabUnavailable, err)
	}
	// Bearer, not PRIVATE-TOKEN — see fetchUser: accepts PATs and OAuth tokens.
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGitLabUnavailable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		drain(resp.Body)
		return nil
	case http.StatusNotFound:
		// 404 means the user is not a member (GitLab returns 404 for
		// non-member lookups against /members/<id>).
		drain(resp.Body)
		return ErrNotInGroup
	case http.StatusUnauthorized, http.StatusForbidden:
		// The PAT lacked permission to read the group — treat as not a
		// member; from the shim's perspective the user can't prove
		// membership, so deny.
		drain(resp.Body)
		return ErrNotInGroup
	default:
		drain(resp.Body)
		return fmt.Errorf("%w: /groups/%s/members returned %d",
			ErrGitLabUnavailable, v.group, resp.StatusCode)
	}
}

func drain(r io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 8*1024))
}
