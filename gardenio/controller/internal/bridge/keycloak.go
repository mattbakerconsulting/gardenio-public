package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// keycloakRoleCacheTTL controls how long Keycloak role check results are cached.
const keycloakRoleCacheTTL = 5 * time.Minute

// KeycloakConfig holds configuration for the Keycloak Admin API client.
type KeycloakConfig struct {
	BaseURL      string // Keycloak server base URL (e.g. "https://keycloak.example.com")
	Realm        string // Realm name (e.g. "gardenio")
	ClientID     string // Service account client ID with realm-admin privileges
	ClientSecret string // Service account client secret
}

// IsConfigured reports whether all required Keycloak fields are set.
func (c KeycloakConfig) IsConfigured() bool {
	return c.BaseURL != "" && c.Realm != "" && c.ClientID != "" && c.ClientSecret != ""
}

// KeycloakRoleChecker queries the Keycloak Admin API to check whether a user
// (identified by email) has a specific realm role. Results are cached for
// keycloakRoleCacheTTL to avoid per-request API calls.
type KeycloakRoleChecker struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret string
	logger       *slog.Logger
	httpClient   *http.Client

	// Token management (client credentials grant).
	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time

	// Role check cache: "email:role" → result.
	cacheMu   sync.RWMutex
	roleCache map[string]roleCacheEntry

	// Effective gardenio roles per lowercased email, for envelope signing.
	personRoles map[string]personRolesEntry

	// Realm roles and group paths per lowercased email, for LandAccess.
	landAccess map[string]landAccessEntry
}

type roleCacheEntry struct {
	hasRole   bool
	checkedAt time.Time
}

// NewKeycloakRoleChecker creates a Keycloak role checker with the given config.
func NewKeycloakRoleChecker(cfg KeycloakConfig, logger *slog.Logger) *KeycloakRoleChecker {
	return &KeycloakRoleChecker{
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		realm:        cfg.Realm,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		logger:       logger,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		roleCache:    make(map[string]roleCacheEntry),
	}
}

// HasRealmRole checks whether the user with the given email has the specified
// realm role in Keycloak. Results are cached for keycloakRoleCacheTTL.
func (k *KeycloakRoleChecker) HasRealmRole(ctx context.Context, email, role string) (bool, error) {
	cacheKey := email + "\x00" + role

	// Check cache.
	k.cacheMu.RLock()
	if entry, ok := k.roleCache[cacheKey]; ok && time.Since(entry.checkedAt) < keycloakRoleCacheTTL {
		k.cacheMu.RUnlock()
		return entry.hasRole, nil
	}
	k.cacheMu.RUnlock()

	// Get admin access token.
	token, err := k.getToken(ctx)
	if err != nil {
		return false, fmt.Errorf("keycloak token: %w", err)
	}

	// Look up user by email.
	userID, err := k.getUserIDByEmail(ctx, token, email)
	if err != nil {
		return false, fmt.Errorf("keycloak user lookup: %w", err)
	}
	if userID == "" {
		k.cacheResult(cacheKey, false)
		return false, nil
	}

	// Check realm role mappings.
	hasRole, err := k.userHasRealmRole(ctx, token, userID, role)
	if err != nil {
		return false, fmt.Errorf("keycloak role check: %w", err)
	}

	k.cacheResult(cacheKey, hasRole)
	return hasRole, nil
}

func (k *KeycloakRoleChecker) cacheResult(key string, hasRole bool) {
	k.cacheMu.Lock()
	k.roleCache[key] = roleCacheEntry{hasRole: hasRole, checkedAt: time.Now()}
	k.cacheMu.Unlock()
}

// getToken obtains or reuses a client_credentials access token.
func (k *KeycloakRoleChecker) getToken(ctx context.Context) (string, error) {
	k.tokenMu.Lock()
	defer k.tokenMu.Unlock()

	if k.accessToken != "" && time.Now().Before(k.tokenExpiry) {
		return k.accessToken, nil
	}

	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", k.baseURL, k.realm)
	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {k.clientID},
		"client_secret": {k.clientSecret},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("token request failed: %s %s", resp.Status, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	k.accessToken = tokenResp.AccessToken
	// Refresh 30 seconds before actual expiry.
	k.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn-30) * time.Second)
	return k.accessToken, nil
}

// getUserIDByEmail looks up a Keycloak user by exact email match.
func (k *KeycloakRoleChecker) getUserIDByEmail(ctx context.Context, token, email string) (string, error) {
	u := fmt.Sprintf("%s/admin/realms/%s/users?email=%s&exact=true&max=1",
		k.baseURL, k.realm, url.QueryEscape(email))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("user lookup failed: %s %s", resp.Status, string(body))
	}

	var users []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", nil
	}
	return users[0].ID, nil
}

// userHasRealmRole checks if a Keycloak user has the given realm role.
func (k *KeycloakRoleChecker) userHasRealmRole(ctx context.Context, token, userID, roleName string) (bool, error) {
	u := fmt.Sprintf("%s/admin/realms/%s/users/%s/role-mappings/realm",
		k.baseURL, k.realm, userID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("role mapping request failed: %s %s", resp.Status, string(body))
	}

	var roles []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&roles); err != nil {
		return false, err
	}
	for _, r := range roles {
		if r.Name == roleName {
			return true, nil
		}
	}
	return false, nil
}

// keycloakPersonRolesTTL bounds how long a person's effective roles are cached
// for envelope signing. It is deliberately much shorter than
// keycloakRoleCacheTTL: these roles are signed into a write credential, so a
// revoked admin must stop acting within a minute, not five.
const keycloakPersonRolesTTL = 30 * time.Second

// gardenioCanonicalRoles are the only roles the daemon accepts in a v2
// edge-auth envelope (kbeads/internal/edgeauth parseCanonicalRoles). Anything
// else Keycloak returns for a user is dropped here rather than signed.
var gardenioCanonicalRoles = map[string]bool{
	"gardenio-viewer":   true,
	"gardenio-operator": true,
	"gardenio-admin":    true,
}

type personRolesEntry struct {
	roles     []string
	checkedAt time.Time
}

// GardenioRoles returns the canonical gardenio-* roles Keycloak grants the
// user with the given email, sorted. Roles are EFFECTIVE: direct realm
// mappings, composites and group-inherited roles (the owner holds
// gardenio-admin through the gardenio-admins group, which the plain
// /role-mappings/realm listing does not show). A user Keycloak does not know,
// or has disabled, yields no roles and no error. A Keycloak failure yields an
// error and never a cached or partial role set, so callers fail closed.
func (k *KeycloakRoleChecker) GardenioRoles(ctx context.Context, email string) ([]string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, nil
	}

	k.cacheMu.RLock()
	if e, ok := k.personRoles[email]; ok && time.Since(e.checkedAt) < keycloakPersonRolesTTL {
		k.cacheMu.RUnlock()
		return append([]string(nil), e.roles...), nil
	}
	k.cacheMu.RUnlock()

	token, err := k.getToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("keycloak token: %w", err)
	}
	userID, err := k.getEnabledUserIDByEmail(ctx, token, email)
	if err != nil {
		return nil, fmt.Errorf("keycloak user lookup: %w", err)
	}
	var roles []string
	if userID != "" {
		roles, err = k.effectiveGardenioRoles(ctx, token, userID)
		if err != nil {
			return nil, fmt.Errorf("keycloak role lookup: %w", err)
		}
	}

	k.cacheMu.Lock()
	if k.personRoles == nil {
		k.personRoles = make(map[string]personRolesEntry)
	}
	k.personRoles[email] = personRolesEntry{roles: roles, checkedAt: time.Now()}
	k.cacheMu.Unlock()
	return append([]string(nil), roles...), nil
}

// getEnabledUserIDByEmail is getUserIDByEmail that also requires an exact
// (case-insensitive) email match and an enabled account; it returns "" for a
// missing or disabled user.
func (k *KeycloakRoleChecker) getEnabledUserIDByEmail(ctx context.Context, token, email string) (string, error) {
	u := fmt.Sprintf("%s/admin/realms/%s/users?email=%s&exact=true&max=2",
		k.baseURL, k.realm, url.QueryEscape(email))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := k.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("user lookup failed: %s %s", resp.Status, string(body))
	}
	var users []struct {
		ID      string `json:"id"`
		Email   string `json:"email"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return "", err
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) && u.Enabled {
			return u.ID, nil
		}
	}
	return "", nil
}

func (k *KeycloakRoleChecker) effectiveGardenioRoles(ctx context.Context, token, userID string) ([]string, error) {
	all, err := k.effectiveRealmRoles(ctx, token, userID)
	if err != nil {
		return nil, err
	}
	var roles []string
	for _, r := range all {
		if gardenioCanonicalRoles[r] {
			roles = append(roles, r)
		}
	}
	return roles, nil
}

// effectiveRealmRoles returns the names of every realm role the user holds,
// direct, composite or group-inherited, sorted and deduplicated.
func (k *KeycloakRoleChecker) effectiveRealmRoles(ctx context.Context, token, userID string) ([]string, error) {
	u := fmt.Sprintf("%s/admin/realms/%s/users/%s/role-mappings/realm/composite",
		k.baseURL, k.realm, url.PathEscape(userID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("role mapping request failed: %s %s", resp.Status, string(body))
	}
	var mapped []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mapped); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var roles []string
	for _, r := range mapped {
		if r.Name != "" && !seen[r.Name] {
			seen[r.Name] = true
			roles = append(roles, r.Name)
		}
	}
	sort.Strings(roles)
	return roles, nil
}

type landAccessEntry struct {
	roles, groups []string
	checkedAt     time.Time
}

// LandAccess returns the user's effective realm roles and the paths of the
// realm groups they are in (/project-<land>, /project-<land>/owners, ...),
// which give their land roles (conversation.LandRolesFromGroups). A user
// Keycloak does not know, or has disabled, has neither and no error. Cached
// per email for keycloakPersonRolesTTL; a failure is never cached.
func (k *KeycloakRoleChecker) LandAccess(ctx context.Context, email string) (roles, groups []string, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, nil, nil
	}
	k.cacheMu.RLock()
	if e, ok := k.landAccess[email]; ok && time.Since(e.checkedAt) < keycloakPersonRolesTTL {
		k.cacheMu.RUnlock()
		return slices.Clone(e.roles), slices.Clone(e.groups), nil
	}
	k.cacheMu.RUnlock()

	token, err := k.getToken(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("keycloak token: %w", err)
	}
	userID, err := k.getEnabledUserIDByEmail(ctx, token, email)
	if err != nil {
		return nil, nil, fmt.Errorf("keycloak user lookup: %w", err)
	}
	if userID != "" {
		if roles, err = k.effectiveRealmRoles(ctx, token, userID); err != nil {
			return nil, nil, fmt.Errorf("keycloak role lookup: %w", err)
		}
		if groups, err = k.userGroupPaths(ctx, token, userID); err != nil {
			return nil, nil, fmt.Errorf("keycloak group lookup: %w", err)
		}
	}

	k.cacheMu.Lock()
	if k.landAccess == nil {
		k.landAccess = make(map[string]landAccessEntry)
	}
	k.landAccess[email] = landAccessEntry{roles: roles, groups: groups, checkedAt: time.Now()}
	k.cacheMu.Unlock()
	return slices.Clone(roles), slices.Clone(groups), nil
}

// userGroupPaths returns the paths of the groups the user is directly in.
func (k *KeycloakRoleChecker) userGroupPaths(ctx context.Context, token, userID string) ([]string, error) {
	u := fmt.Sprintf("%s/admin/realms/%s/users/%s/groups?briefRepresentation=true&max=1000",
		k.baseURL, k.realm, url.PathEscape(userID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("group request failed: %s %s", resp.Status, string(body))
	}
	var groups []struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		return nil, err
	}
	var paths []string
	for _, g := range groups {
		if g.Path != "" {
			paths = append(paths, g.Path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}
