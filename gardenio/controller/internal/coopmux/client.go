// Package coopmux provides an HTTP client for the coopmux credential broker API.
//
// Coopmux is the multi-session credential distributor. The controller pushes
// account credentials to coopmux at startup and periodically so that agent pods
// can request tokens at runtime without direct K8s secret access.
//
// Transport posture: the raw provider API key crosses the wire in the
// /api/v1/credentials/set JSON body. This client speaks plaintext HTTP and
// relies on the deployment posture (service-mesh sidecar mTLS, native mTLS,
// or NetworkPolicy-only) for confidentiality and identity. See
// gardenio/docs/security/controller-coopmux-mtls.md for the trust boundary
// and the per-environment posture declaration on coopmux.transport.posture.
package coopmux

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/httpjson"
)

// Client talks to the coopmux credential broker HTTP API.
type Client struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
}

// Config holds settings for the coopmux client.
type Config struct {
	// URL is the coopmux service URL (e.g. "http://coopmux:8080").
	URL string

	// AuthToken is the bearer token for coopmux API authentication.
	AuthToken string
}

// New creates a coopmux client. Returns nil if the URL is empty.
func New(cfg Config) *Client {
	if cfg.URL == "" {
		return nil
	}
	return &Client{
		baseURL:   strings.TrimRight(cfg.URL, "/"),
		authToken: cfg.AuthToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// AccountSync holds the data needed to sync one account to coopmux.
type AccountSync struct {
	Name       string // Account name (e.g. "matthewbaker1110")
	Provider   string // Provider name (e.g. "claude")
	Token      string // Resolved credential token
	Model      string // Model name (e.g. "claude-opus-4-7"); propagated to sessions on rotation
	LiteLLMKey string // LiteLLM virtual key for per-account budget tracking
	// BaseURL is the resolved API endpoint for this account (proxy_url >
	// base_url > controller LiteLLM default — same order as pod spawn).
	// Propagated to sessions as ANTHROPIC_BASE_URL on rotation so a switch
	// onto an account served by a different endpoint carries the endpoint
	// alongside the token/model (ka-0000002623).
	BaseURL string
}

// SyncAccounts registers accounts and pushes their tokens to coopmux.
// Returns the number of successfully synced accounts and any errors encountered.
func (c *Client) SyncAccounts(ctx context.Context, accounts []AccountSync) (synced int, failed int, err error) {
	for _, acct := range accounts {
		if acct.Token == "" {
			failed++
			continue
		}

		// Register the account (idempotent — coopmux ignores duplicates).
		if regErr := c.registerAccount(ctx, acct.Name, acct.Provider, acct.Model); regErr != nil {
			failed++
			err = fmt.Errorf("register %s: %w", acct.Name, regErr)
			continue
		}

		// Push the credential token.
		if setErr := c.setCredential(ctx, acct); setErr != nil {
			failed++
			err = fmt.Errorf("set credential %s: %w", acct.Name, setErr)
			continue
		}

		synced++
	}
	return synced, failed, err
}

// SyncSingleAccount registers and pushes credentials for a single account.
// This is the real-time counterpart of SyncAccounts — called when the controller
// detects an account bead update via SSE rather than during periodic bulk sync.
func (c *Client) SyncSingleAccount(ctx context.Context, acct AccountSync) error {
	if acct.Token == "" {
		return fmt.Errorf("empty token for account %s", acct.Name)
	}
	if err := c.registerAccount(ctx, acct.Name, acct.Provider, acct.Model); err != nil {
		return fmt.Errorf("register %s: %w", acct.Name, err)
	}
	if err := c.setCredential(ctx, acct); err != nil {
		return fmt.Errorf("set credential %s: %w", acct.Name, err)
	}
	return nil
}

// registerAccount calls POST /api/v1/credentials/new to register an account.
// Idempotent: treats "account already exists" as success so periodic sync
// can re-register without failing.  The model field is propagated to coopmux
// so that credential rotation includes the correct CLAUDE_MODEL for the new
// account.
func (c *Client) registerAccount(ctx context.Context, name, provider, model string) error {
	if provider == "" {
		provider = "claude"
	}
	body := map[string]string{
		"name":     name,
		"provider": provider,
	}
	if model != "" {
		body["model"] = model
	}
	err := c.post(ctx, "/api/v1/credentials/new", body)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// setCredential calls POST /api/v1/credentials/set to push a token.
func (c *Client) setCredential(ctx context.Context, acct AccountSync) error {
	body := map[string]string{
		"account": acct.Name,
		"token":   acct.Token,
	}
	if acct.LiteLLMKey != "" {
		body["litellm_key"] = acct.LiteLLMKey
	}
	if acct.BaseURL != "" {
		body["base_url"] = acct.BaseURL
	}
	return c.post(ctx, "/api/v1/credentials/set", body)
}

// Distribute tells coopmux to push the named account's credentials to all
// active sessions and switch them to the new account. This is called after
// auth failover rotation or switchback so that running agents pick up the
// new credentials without restarting.
func (c *Client) Distribute(ctx context.Context, account string) error {
	return c.post(ctx, "/api/v1/credentials/distribute", map[string]any{
		"account": account,
		"switch":  true,
	})
}

// RemoveAccount calls DELETE /api/v1/credentials/{account} to remove a stale
// account from coopmux. Sessions assigned to this account will show
// account_status=unknown until reassigned.
func (c *Client) RemoveAccount(ctx context.Context, account string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/credentials/"+account, nil, nil)
}

// AccountStatus represents a single account entry from coopmux's credential status.
type AccountStatus struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// ListAccounts calls GET /api/v1/credentials/status and returns the account
// names known to coopmux. Used to detect stale accounts that should be removed.
func (c *Client) ListAccounts(ctx context.Context) ([]string, error) {
	// The status endpoint returns an array of objects; we only need the names.
	var entries []AccountStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/credentials/status", nil, &entries); err != nil {
		return nil, err
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names, nil
}

// SessionAccount is the subset of a coopmux session entry the controller
// needs to reason about live credential state: which account the session is
// ACTUALLY running on right now. Coopmux reconciles assigned_account from the
// account coop itself reports (health poller), so unlike the pod's baked env
// it tracks in-process credential switches.
type SessionAccount struct {
	ID              string  `json:"id"`
	AssignedAccount *string `json:"assigned_account"`
}

// ListSessionAccounts calls GET /api/v1/sessions and returns a map of session
// id → live account name. Sessions with no assigned account are omitted.
// Session ids equal the agent pod name (coop registers under its pod name),
// so callers can look up a pod's live account directly.
func (c *Client) ListSessionAccounts(ctx context.Context) (map[string]string, error) {
	var entries []SessionAccount
	if err := c.do(ctx, http.MethodGet, "/api/v1/sessions", nil, &entries); err != nil {
		return nil, err
	}

	accounts := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.AssignedAccount != nil && *e.AssignedAccount != "" {
			accounts[e.ID] = *e.AssignedAccount
		}
	}
	return accounts, nil
}

// post performs a POST request with a JSON body.
func (c *Client) post(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// do sends one authenticated JSON request to coopmux and decodes the
// response into out (nil discards it).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return httpjson.Do(ctx, c.httpClient, httpjson.Request{
		Method: method,
		URL:    c.baseURL + path,
		Header: httpjson.Bearer(c.authToken),
		Body:   body,
	}, out)
}
