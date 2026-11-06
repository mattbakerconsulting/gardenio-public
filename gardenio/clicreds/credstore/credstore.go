// Package credstore manages local credentials for the gb CLI.
//
// Credentials persist in one of two backends, selected automatically when
// callers pass an empty path to Load:
//
//   - OS secret store (macOS Keychain / Linux libsecret), one entry per
//     daemon URL under service "gardenio", with a small non-sensitive index
//     file at ~/.config/gardenio/index.json holding the current context
//     name and the {name → server} mapping.
//   - File fallback at ~/.config/gardenio/credentials.json with 0600 perms,
//     used when the OS secret store is unavailable (headless Linux, CI,
//     etc.) or when GB_CREDSTORE_BACKEND=file is set.
//
// Tests and explicit callers can force the file backend by passing a
// non-empty path to Load; that path is treated as the credentials file.
// Multiple named contexts allow switching between kbeads servers.
package credstore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenSource provides a valid access token, refreshing if expired.
type TokenSource interface {
	// Token returns a valid access token. If the stored token is expired,
	// it is refreshed automatically using the refresh token.
	Token(ctx context.Context) (string, error)
}

// Store manages credential contexts persisted via a pluggable backend.
type Store struct {
	backend backend
	mu      sync.Mutex
	data    storeData
}

type storeData struct {
	CurrentContext string              `json:"current_context"`
	Contexts       map[string]*Context `json:"contexts"`
}

// Context holds credentials and server info for a single kbeads endpoint:
// an OAuth bearer (AccessToken / IDToken / RefreshToken / ExpiresAt).
//
// A context an older gb login (before gd-5FThTGbOOP) wrote may still carry a
// "cookie" key holding the retired AVA cookie bundle. It is not decoded any
// more (gd-Nza6by1LUT), so such a context loads with no bearer, and the next
// Save drops the cookie from disk.
type Context struct {
	Server       string `json:"server"`
	GitLabURL    string `json:"gitlab_url"`
	ClientID     string `json:"client_id"`
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token,omitempty"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	User         *User  `json:"user,omitempty"`
}

// User holds the authenticated user's identity from the OIDC ID token.
type User struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	GitLabID int64  `json:"gitlab_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

// configDir returns the gardenio config directory, respecting XDG_CONFIG_HOME.
func configDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gardenio")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", "gardenio")
	}
	return filepath.Join(home, ".config", "gardenio")
}

// DefaultPath returns the default credentials file path (used by the file
// backend, and as the migration source when the keyring backend is selected).
func DefaultPath() string {
	return filepath.Join(configDir(), "credentials.json")
}

// defaultIndexPath returns the keyring-backend index file path. The index
// holds non-sensitive metadata (current context + name → server mapping)
// so that the keyring entries (one per server) can be enumerated.
func defaultIndexPath() string {
	return filepath.Join(configDir(), "index.json")
}

// Load reads a credential store. Behaviour depends on path:
//
//   - path == "" → auto-select backend. GB_CREDSTORE_BACKEND overrides
//     ("file" | "keyring" | "auto", default "auto"). Auto first probes
//     the OS keyring; if the probe fails the file backend is used
//     transparently. On first use with a populated legacy
//     credentials.json, the data is migrated into the keyring backend.
//   - path != "" → use the file backend at the given path. Tests and
//     explicit callers rely on this to pin storage to a known location.
//
// If no data is stored yet, an empty store is returned (ready for first-time
// use).
func Load(path string) (*Store, error) {
	be, err := selectBackend(path)
	if err != nil {
		return nil, err
	}
	data, err := be.load()
	if err != nil {
		return nil, err
	}
	if data.Contexts == nil {
		data.Contexts = make(map[string]*Context)
	}
	if data.CurrentContext == "" {
		data.CurrentContext = "default"
	}
	return &Store{backend: be, data: data}, nil
}

// selectBackend picks the persistence backend for Load.
func selectBackend(path string) (backend, error) {
	if path != "" {
		return &fileBackend{path: path}, nil
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("GB_CREDSTORE_BACKEND")))
	switch mode {
	case "file":
		return &fileBackend{path: DefaultPath()}, nil
	case "keyring":
		return newKeyringBackend(defaultIndexPath(), DefaultPath()), nil
	case "", "auto":
		// Try keyring; if the probe fails, fall back to file silently.
		if keyringAvailable() {
			return newKeyringBackend(defaultIndexPath(), DefaultPath()), nil
		}
		return &fileBackend{path: DefaultPath()}, nil
	default:
		return nil, fmt.Errorf("invalid GB_CREDSTORE_BACKEND=%q (want file|keyring|auto)", mode)
	}
}

// Save writes the store atomically through the active backend.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backend.save(s.data)
}

// Describe returns a human-readable description of where this store
// persists data. Used by login/status output.
func (s *Store) Describe() string {
	return s.backend.describe()
}

// CurrentContext returns the name of the active context.
func (s *Store) CurrentContext() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.CurrentContext
}

// SetCurrentContext switches the active context.
func (s *Store) SetCurrentContext(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.CurrentContext = name
}

// Context returns the named context, or nil if it does not exist.
func (s *Store) Context(name string) *Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Contexts[name]
}

// Current returns the active context, or nil if none is set.
func (s *Store) Current() *Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Contexts[s.data.CurrentContext]
}

// SetContext stores a context by name.
func (s *Store) SetContext(name string, ctx *Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Contexts[name] = ctx
}

// DeleteContext removes a named context.
func (s *Store) DeleteContext(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Contexts, name)
}

// ContextNames returns all stored context names.
func (s *Store) ContextNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.data.Contexts))
	for k := range s.data.Contexts {
		names = append(names, k)
	}
	return names
}

// IsExpired reports whether the access token has expired (or will expire
// within a 30-second grace window).
func (c *Context) IsExpired() bool {
	if c.ExpiresAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return true
	}
	return time.Now().After(t.Add(-30 * time.Second))
}

// tokenSource implements TokenSource using a Store's current context.
type tokenSource struct {
	store *Store
}

// NewTokenSource returns a TokenSource that reads from the store's current
// context and auto-refreshes expired tokens via the GitLab token endpoint.
func NewTokenSource(store *Store) TokenSource {
	return &tokenSource{store: store}
}

// Token returns a valid bearer token, refreshing if necessary.
// Prefers the OIDC id_token (JWT) over the opaque access_token because
// kbeads validates tokens via OIDC JWT verification. GitLab access tokens
// are opaque and cannot be verified by OIDC providers.
func (ts *tokenSource) Token(ctx context.Context) (string, error) {
	cur := ts.store.Current()
	if cur == nil {
		return "", fmt.Errorf("not logged in — run 'gb login' first")
	}
	if !cur.IsExpired() {
		return cur.bearerToken(), nil
	}
	if cur.RefreshToken == "" {
		return "", fmt.Errorf("token expired and no refresh token available — run 'gb login'")
	}
	if err := ts.refresh(ctx, cur); err != nil {
		return "", fmt.Errorf("refreshing token: %w (run 'gb login' to re-authenticate)", err)
	}
	return cur.bearerToken(), nil
}

// HasBearer reports whether the context carries a bearer token. One without
// (such as a context holding only the retired gb login cookie) cannot
// authenticate a CLI.
func (c *Context) HasBearer() bool {
	return c.IDToken != "" || c.AccessToken != ""
}

// bearerToken returns the id_token (JWT) if available, otherwise the access_token.
func (c *Context) bearerToken() string {
	if c.IDToken != "" {
		return c.IDToken
	}
	return c.AccessToken
}

func (ts *tokenSource) refresh(_ context.Context, c *Context) error {
	if c.GitLabURL == "" || c.ClientID == "" {
		return fmt.Errorf("missing gitlab_url or client_id in credential context")
	}
	tokenURL := c.GitLabURL + "/oauth/token"
	resp, err := http.PostForm(tokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
		"client_id":     {c.ClientID},
	})
	if err != nil {
		return fmt.Errorf("POST %s: %w", tokenURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token refresh failed: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return fmt.Errorf("decoding token response: %w", err)
	}
	c.AccessToken = tok.AccessToken
	if tok.IDToken != "" {
		c.IDToken = tok.IDToken
	}
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	c.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
	return ts.store.Save()
}
