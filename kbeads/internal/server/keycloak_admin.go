package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KeycloakAdminConfig holds configuration for the Keycloak Admin REST API client.
type KeycloakAdminConfig struct {
	// BaseURL is the Keycloak server base URL (e.g., "https://keycloak.example.com/auth").
	BaseURL string

	// Realm is the Keycloak realm to manage (e.g., "gardenio").
	Realm string

	// ClientID is the service account client ID (e.g., "gardenio-admin-cli").
	ClientID string

	// ClientSecret is the service account client secret.
	ClientSecret string
}

// IsConfigured returns true when all required fields are set.
func (c *KeycloakAdminConfig) IsConfigured() bool {
	return c.BaseURL != "" && c.Realm != "" && c.ClientID != "" && c.ClientSecret != ""
}

// KeycloakAdminClient interacts with the Keycloak Admin REST API for user
// session management (lookup, list sessions, revoke sessions).
type KeycloakAdminClient struct {
	cfg        KeycloakAdminConfig
	httpClient *http.Client

	// cached admin token
	token       string
	tokenExpiry time.Time
}

// NewKeycloakAdminClient creates a new admin client from config.
func NewKeycloakAdminClient(cfg KeycloakAdminConfig) *KeycloakAdminClient {
	return &KeycloakAdminClient{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// keycloakUser represents a user from the Keycloak Admin API.
type keycloakUser struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Email         string `json:"email"`
	FirstName     string `json:"firstName"`
	LastName      string `json:"lastName"`
	Enabled       bool   `json:"enabled"`
	EmailVerified bool   `json:"emailVerified"`
}

// keycloakSession represents an active session from the Keycloak Admin API.
type keycloakSession struct {
	ID         string            `json:"id"`
	Username   string            `json:"username"`
	UserID     string            `json:"userId"`
	IPAddress  string            `json:"ipAddress"`
	Start      int64             `json:"start"`
	LastAccess int64             `json:"lastAccess"`
	Clients    map[string]string `json:"clients"`
}

// RevokeUserResult contains the result of a user session revocation.
type RevokeUserResult struct {
	UserID          string `json:"user_id"`
	Username        string `json:"username"`
	Email           string `json:"email"`
	SessionsRevoked int    `json:"sessions_revoked"`
	UserDisabled    bool   `json:"user_disabled"`
}

// getAdminToken obtains (or reuses a cached) admin access token via the
// client_credentials grant against the realm's token endpoint.
func (c *KeycloakAdminClient) getAdminToken(ctx context.Context) (string, error) {
	// Return cached token if still valid (with 30s buffer).
	if c.token != "" && time.Now().Before(c.tokenExpiry.Add(-30*time.Second)) {
		return c.token, nil
	}

	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token",
		strings.TrimRight(c.cfg.BaseURL, "/"), c.cfg.Realm)

	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("token request failed (status %d): %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}

	c.token = tokenResp.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return c.token, nil
}

// adminRequest builds and executes an authenticated request against the
// Keycloak Admin REST API.
func (c *KeycloakAdminClient) adminRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	token, err := c.getAdminToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get admin token: %w", err)
	}

	reqURL := fmt.Sprintf("%s/admin/realms/%s%s",
		strings.TrimRight(c.cfg.BaseURL, "/"), c.cfg.Realm, path)

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.httpClient.Do(req)
}

// FindUserByEmail looks up a Keycloak user by email address.
// Returns nil if no user is found.
func (c *KeycloakAdminClient) FindUserByEmail(ctx context.Context, email string) (*keycloakUser, error) {
	path := fmt.Sprintf("/users?email=%s&exact=true", url.QueryEscape(email))
	resp, err := c.adminRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("find user failed (status %d): %s", resp.StatusCode, body)
	}

	var users []keycloakUser
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("decode users: %w", err)
	}

	// exact=true should return at most one, but filter to be safe.
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			return &u, nil
		}
	}
	return nil, nil
}

// FindUserByUsername looks up a Keycloak user by username.
// Returns nil if no user is found.
func (c *KeycloakAdminClient) FindUserByUsername(ctx context.Context, username string) (*keycloakUser, error) {
	path := fmt.Sprintf("/users?username=%s&exact=true", url.QueryEscape(username))
	resp, err := c.adminRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("find user failed (status %d): %s", resp.StatusCode, body)
	}

	var users []keycloakUser
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, fmt.Errorf("decode users: %w", err)
	}

	for _, u := range users {
		if strings.EqualFold(u.Username, username) {
			return &u, nil
		}
	}
	return nil, nil
}

// ListUserSessions returns all active sessions for a user.
func (c *KeycloakAdminClient) ListUserSessions(ctx context.Context, userID string) ([]keycloakSession, error) {
	path := fmt.Sprintf("/users/%s/sessions", url.PathEscape(userID))
	resp, err := c.adminRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("list sessions failed (status %d): %s", resp.StatusCode, body)
	}

	var sessions []keycloakSession
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	return sessions, nil
}

// LogoutUser revokes all sessions for a user (DELETE /users/{id}/sessions).
func (c *KeycloakAdminClient) LogoutUser(ctx context.Context, userID string) error {
	path := fmt.Sprintf("/users/%s/sessions", url.PathEscape(userID))
	resp, err := c.adminRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("logout user failed (status %d): %s", resp.StatusCode, body)
	}
	return nil
}

// DisableUser sets the user's enabled flag to false, preventing new logins.
func (c *KeycloakAdminClient) DisableUser(ctx context.Context, userID string) error {
	body := strings.NewReader(`{"enabled":false}`)
	path := fmt.Sprintf("/users/%s", url.PathEscape(userID))
	resp, err := c.adminRequest(ctx, http.MethodPut, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("disable user failed (status %d): %s", resp.StatusCode, respBody)
	}
	return nil
}

// RevokeUser performs a full offboarding revocation: finds the user, counts
// active sessions, revokes all sessions, and optionally disables the account.
// Returns a summary of actions taken.
func (c *KeycloakAdminClient) RevokeUser(ctx context.Context, email, username string, disable bool) (*RevokeUserResult, error) {
	var user *keycloakUser
	var err error

	if email != "" {
		user, err = c.FindUserByEmail(ctx, email)
	} else if username != "" {
		user, err = c.FindUserByUsername(ctx, username)
	} else {
		return nil, fmt.Errorf("email or username is required")
	}
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}

	// Count active sessions before revoking.
	sessions, err := c.ListUserSessions(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	// Revoke all sessions.
	if err := c.LogoutUser(ctx, user.ID); err != nil {
		return nil, fmt.Errorf("revoke sessions: %w", err)
	}

	result := &RevokeUserResult{
		UserID:          user.ID,
		Username:        user.Username,
		Email:           user.Email,
		SessionsRevoked: len(sessions),
	}

	// Optionally disable the account to prevent new logins.
	if disable {
		if err := c.DisableUser(ctx, user.ID); err != nil {
			return result, fmt.Errorf("sessions revoked but disable failed: %w", err)
		}
		result.UserDisabled = true
	}

	return result, nil
}
