package main

// agent_k8s_creds.go — Claude credential provisioning and OAuth refresh loop.
//
// Implements the 5-priority credential cascade and the background OAuth token
// refresh goroutine.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// credMode describes how Claude credentials were provisioned.
type credMode int

const (
	credNone     credMode = iota
	credPVC               // existing credentials on PVC
	credSecret            // K8s secret mount copied to PVC
	credOAuthEnv          // CLAUDE_CODE_OAUTH_TOKEN env var
	credAPIKey            // ANTHROPIC_API_KEY env var
	credMuxFetch          // fetched from coopmux distribute endpoint
)

const (
	oauthTokenURL = "https://platform.claude.com/v1/oauth/token"
	oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e" // gitleaks:allow: public OAuth client id of Claude's first-party app, not a secret
)

// provisionCredentials runs the 5-priority cascade and returns the credMode
// that was used.
func provisionCredentials(claudeStateDir string) credMode {
	credsFile := filepath.Join(claudeStateDir, ".credentials.json")
	staging := "/tmp/claude-credentials/credentials.json"

	// 1. PVC credentials already present.
	if _, err := os.Stat(credsFile); err == nil {
		fmt.Printf("[gb agent start] using existing PVC credentials\n")
		return credPVC
	}

	// 2. K8s secret mount.
	if _, err := os.Stat(staging); err == nil {
		if err := copyFile(staging, credsFile, 0o600); err == nil {
			fmt.Printf("[gb agent start] seeded Claude credentials from K8s secret\n")
			return credSecret
		}
	}

	// 3. CLAUDE_CODE_OAUTH_TOKEN env — coop auto-writes .credentials.json.
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		fmt.Printf("[gb agent start] CLAUDE_CODE_OAUTH_TOKEN set — coop will auto-write credentials\n")
		return credOAuthEnv
	}

	// 4. ANTHROPIC_API_KEY env — API key mode, no credentials file needed.
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		fmt.Printf("[gb agent start] ANTHROPIC_API_KEY set — using API key mode\n")
		return credAPIKey
	}

	// 5. coopmux distribute endpoint.
	if muxURL := os.Getenv("COOP_MUX_URL"); muxURL != "" {
		if err := fetchMuxCredentials(muxURL, credsFile); err == nil {
			fmt.Printf("[gb agent start] seeded credentials from coopmux\n")
			return credMuxFetch
		} else {
			fmt.Printf("[gb agent start] WARNING: coopmux credential fetch failed: %v\n", err)
		}
	}

	fmt.Printf("[gb agent start] WARNING: no Claude credentials available — agent may not authenticate\n")
	return credNone
}

// fetchMuxCredentials calls the coopmux distribute endpoint and writes the
// response to credsFile.
func fetchMuxCredentials(muxURL, credsFile string) error {
	sessionID, _ := os.Hostname()
	payload, _ := json.Marshal(map[string]string{"session_id": sessionID})

	req, err := http.NewRequest(http.MethodPost, muxURL+"/api/v1/credentials/distribute", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := envOr("COOP_MUX_AUTH_TOKEN", os.Getenv("COOP_BROKER_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var creds map[string]any
	if err := json.Unmarshal(body, &creds); err != nil {
		return fmt.Errorf("invalid JSON response")
	}
	if oauth, ok := creds["claudeAiOauth"].(map[string]any); !ok || oauth["accessToken"] == nil {
		return fmt.Errorf("response missing claudeAiOauth.accessToken")
	}

	return os.WriteFile(credsFile, body, 0o600)
}

// oauthRefreshLoop runs in a goroutine and periodically refreshes OAuth tokens
// before they expire.
func oauthRefreshLoop(ctx context.Context, claudeStateDir string, mode credMode) {
	if mode == credAPIKey {
		fmt.Printf("[gb agent start] API key mode — skipping OAuth refresh loop\n")
		return
	}

	// When coopmux manages credentials, skip the local refresh loop.
	// Coopmux's distributor handles credential rotation and pushes fresh
	// tokens to sessions — running both causes race conditions.
	if os.Getenv("COOP_MUX_URL") != "" {
		fmt.Printf("[gb agent start] coopmux manages credentials — skipping local OAuth refresh loop\n")
		return
	}

	credsFile := filepath.Join(claudeStateDir, ".credentials.json")

	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	consecutiveFails := 0
	const maxFails = 5

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := maybeRefreshOAuth(credsFile); err != nil {
				consecutiveFails++
				fmt.Printf("[gb agent start] WARNING: OAuth refresh failed (%d/%d): %v\n", consecutiveFails, maxFails, err)
				if consecutiveFails >= maxFails {
					if isCoopAgentActive() {
						fmt.Printf("[gb agent start] WARNING: OAuth refresh failing but agent is active, not terminating\n")
						consecutiveFails = 0
					} else {
						fmt.Printf("[gb agent start] FATAL: OAuth refresh failed %d consecutive times, terminating\n", maxFails)
						os.Exit(1)
					}
				}
			} else {
				consecutiveFails = 0
			}
		}
	}
}

// maybeRefreshOAuth reads the credentials file and refreshes the token if it
// expires within the next hour.
func maybeRefreshOAuth(credsFile string) error {
	data, err := os.ReadFile(credsFile)
	if err != nil {
		return nil // no creds file yet — skip silently
	}

	var creds map[string]any
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil
	}

	oauth, ok := creds["claudeAiOauth"].(map[string]any)
	if !ok {
		return nil
	}

	refreshToken, _ := oauth["refreshToken"].(string)
	expiresAtF, _ := oauth["expiresAt"].(float64)
	expiresAt := int64(expiresAtF)

	if refreshToken == "" || expiresAt == 0 {
		return nil
	}

	// Coop-provisioned credentials use a sentinel expiresAt >= 10^12 ms — skip.
	if expiresAt >= 9_999_999_999_000 {
		return nil
	}

	nowMS := time.Now().UnixMilli()
	remainingMS := expiresAt - nowMS
	if remainingMS > 3_600_000 {
		return nil // more than 1 hour remaining
	}

	fmt.Printf("[gb agent start] OAuth token expires in %dm, refreshing...\n", remainingMS/60_000)

	type tokenRequest struct {
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
		ClientID     string `json:"client_id"`
	}
	type tokenResponse struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}

	body, _ := json.Marshal(tokenRequest{
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
		ClientID:     oauthClientID,
	})
	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var tok tokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil || tok.AccessToken == "" || tok.RefreshToken == "" {
		return fmt.Errorf("invalid token response")
	}

	newExpiresAt := time.Now().UnixMilli() + tok.ExpiresIn*1000
	oauth["accessToken"] = tok.AccessToken
	oauth["refreshToken"] = tok.RefreshToken
	oauth["expiresAt"] = newExpiresAt
	creds["claudeAiOauth"] = oauth

	updated, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := credsFile + ".tmp"
	if err := os.WriteFile(tmp, updated, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, credsFile); err != nil {
		return err
	}

	fmt.Printf("[gb agent start] OAuth credentials refreshed (expires in %dh)\n", tok.ExpiresIn/3600)
	return nil
}

// isCoopAgentActive polls the local coop API to check if the agent is working or idle.
func isCoopAgentActive() bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:8080/api/v1/agent")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var state struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return false
	}
	return state.State == "working" || state.State == "idle"
}

// copyFile copies src to dst with the given permissions.
func copyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, perm)
}
