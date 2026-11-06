package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The user-to-server leg (Lands Track B B-D3). An App with "request user
// authorization (OAuth) during installation" on sends the installer back to its
// setup URL with an installation_id and a one-time code. The code buys a
// user-to-server token: a credential acting as that GitHub user, but only
// within this App's permissions, which setup.go uses to ask GitHub who the user
// is and what they can do on the installed repository. The token is revoked as
// soon as the check is done; nothing stores it.

// UserToken is a user-to-server access token. Like Token, its value is behind
// Reveal and every formatting path prints Redacted.
type UserToken struct{ value string }

func (t UserToken) Reveal() string               { return t.value }
func (t UserToken) String() string               { return Redacted }
func (t UserToken) GoString() string             { return Redacted }
func (t UserToken) LogValue() slog.Value         { return slog.StringValue(Redacted) }
func (t UserToken) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (t UserToken) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// errNoOAuth is returned by the user-to-server calls on an App configured
// without a client id and secret.
var errNoOAuth = fmt.Errorf("%w: no OAuth client id and secret", ErrNotConfigured)

func (c *Client) hasOAuth() bool { return c.clientID != "" && len(c.clientSecret) > 0 }

// ExchangeUserCode trades the setup callback's code for a user-to-server token
// (POST <web>/login/oauth/access_token). GitHub answers a bad or reused code
// with 200 and an `error` field; that comes back as an *OAuthError carrying
// GitHub's error code only.
func (c *Client) ExchangeUserCode(ctx context.Context, code string) (UserToken, error) {
	if !c.hasOAuth() {
		return UserToken{}, errNoOAuth
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return UserToken{}, &OAuthError{Code: "missing_code"}
	}
	form := url.Values{"client_id": {c.clientID}, "client_secret": {string(c.clientSecret)}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return UserToken{}, errors.New("githubapp: build code exchange")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return UserToken{}, errors.New("githubapp: code exchange: request failed")
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		return UserToken{}, fmt.Errorf("githubapp: code exchange: HTTP %d", resp.StatusCode)
	}
	if out.Error != "" || out.AccessToken == "" {
		code := out.Error
		if code == "" {
			code = "no_access_token"
		}
		return UserToken{}, &OAuthError{Code: code}
	}
	return UserToken{value: out.AccessToken}, nil
}

// OAuthError is GitHub's refusal of a code exchange. Code is GitHub's `error`
// value (bad_verification_code, incorrect_client_credentials, ...).
type OAuthError struct{ Code string }

func (e *OAuthError) Error() string { return "githubapp: code exchange refused: " + e.Code }

// RevokeUserToken deletes a user-to-server token (DELETE
// /applications/{client_id}/token, basic auth with the App's client id and
// secret). Already gone (404) counts as success.
func (c *Client) RevokeUserToken(ctx context.Context, tok UserToken) error {
	if tok.value == "" {
		return nil
	}
	if !c.hasOAuth() {
		return errNoOAuth
	}
	body, _ := json.Marshal(map[string]string{"access_token": tok.value})
	endpoint := fmt.Sprintf("%s/applications/%s/token", c.baseURL, url.PathEscape(c.clientID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("githubapp: build user token revoke")
	}
	req.SetBasicAuth(c.clientID, string(c.clientSecret))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return errors.New("githubapp: revoke user token: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("githubapp: revoke user token: HTTP %d", resp.StatusCode)
	}
	return nil
}

// User is the GitHub account behind a user-to-server token (GET /user). The
// signals after Login are what the operator reads at approval time (spec §2.2
// item 5).
type User struct {
	ID          int64     `json:"id"`
	Login       string    `json:"login"`
	CreatedAt   time.Time `json:"created_at"`
	PublicRepos int       `json:"public_repos"`
	Followers   int       `json:"followers"`
}

// AuthenticatedUser returns the user a user-to-server token acts for.
func (c *Client) AuthenticatedUser(ctx context.Context, tok UserToken) (User, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, c.baseURL+"/user", tok.value, nil, http.StatusOK, &u); err != nil {
		return User{}, fmt.Errorf("githubapp: read authenticated user: %w", err)
	}
	if u.ID == 0 {
		return User{}, errors.New("githubapp: authenticated user has no id")
	}
	return u, nil
}

// Installation is one installation of this App (GET /app/installations/{id}).
type Installation struct {
	ID      int64 `json:"id"`
	AppID   int64 `json:"app_id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string     `json:"repository_selection"`
	SuspendedAt         *time.Time `json:"suspended_at"`
}

// Installation reads one of this App's installations with the App JWT. An
// installation of any other App is a 404 from GitHub (*HTTPError).
func (c *Client) Installation(ctx context.Context, installationID int64) (Installation, error) {
	if installationID <= 0 {
		return Installation{}, errors.New("githubapp: installation id is required")
	}
	jwt, err := c.JWT()
	if err != nil {
		return Installation{}, err
	}
	var inst Installation
	endpoint := fmt.Sprintf("%s/app/installations/%d", c.baseURL, installationID)
	if err := c.do(ctx, http.MethodGet, endpoint, jwt, nil, http.StatusOK, &inst); err != nil {
		return Installation{}, fmt.Errorf("githubapp: read installation %d: %w", installationID, err)
	}
	return inst, nil
}

// Repository is a repository as GitHub returns it to a user-to-server token:
// Permissions are that user's own on the repository.
type Repository struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	FullName   string `json:"full_name"`
	Private    bool   `json:"private"`
	Visibility string `json:"visibility"`
	// Size is the repository's size on GitHub in kilobytes.
	Size  int64 `json:"size"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Permissions struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

// maxUserInstallationRepos bounds the one page read below. Lands needs exactly
// one repository, so a second page would only ever say "too many".
const maxUserInstallationRepos = 100

// UserInstallationRepos lists the repositories of installationID that the
// token's user can access (GET /user/installations/{id}/repositories). GitHub
// answers 404 when the user has no access to the installation at all. total is
// GitHub's total_count, which can exceed len(repos).
func (c *Client) UserInstallationRepos(ctx context.Context, tok UserToken, installationID int64) (repos []Repository, total int, err error) {
	var out struct {
		TotalCount   int          `json:"total_count"`
		Repositories []Repository `json:"repositories"`
	}
	endpoint := fmt.Sprintf("%s/user/installations/%d/repositories?per_page=%d", c.baseURL, installationID, maxUserInstallationRepos)
	if err := c.do(ctx, http.MethodGet, endpoint, tok.value, nil, http.StatusOK, &out); err != nil {
		return nil, 0, fmt.Errorf("githubapp: list installation %d repositories for the user: %w", installationID, err)
	}
	return out.Repositories, out.TotalCount, nil
}

// AuthorizeURL is the App's user-authorization page. GitHub sends the user
// back to the App's callback URL with a one-time code and state. An update to
// an installation's repository selection returns without a code, so the setup
// leg sends the user here to get one.
func (c *Client) AuthorizeURL(state string) string {
	v := url.Values{"client_id": {c.clientID}}
	if state != "" {
		v.Set("state", state)
	}
	return c.webURL + "/login/oauth/authorize?" + v.Encode()
}

// InstallURL is the page that installs the App, carrying state back to its
// setup URL (https://github.com/apps/<slug>/installations/new?state=...).
func (c *Client) InstallURL(slug, state string) string {
	u := c.webURL + "/apps/" + url.PathEscape(slug) + "/installations/new"
	if state != "" {
		u += "?" + url.Values{"state": {state}}.Encode()
	}
	return u
}
