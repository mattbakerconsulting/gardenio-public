package main

// init_agent_wait_creds.go — wait for coopmux to be populated with credentials
// before starting coop, so the first Claude session is not killed by an early
// credential switch. See ka-0000006291.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// waitForCoopmuxCredentials polls coopmux's GET /api/v1/credentials/status
// until at least one account reports has_api_key=true. When preferredAccount
// is non-empty, the wait targets that specific account.
//
// Why this exists: gb init starts coop very early in the pod lifecycle.
// Before the controller has finished its initial syncAccountsToCoopmux pass,
// the broker holds empty api_keys for every account. Once the controller
// POSTs /api/v1/credentials/set, the broker sees token_changed=true and
// emits CredentialEvent::Refreshed; the distributor consumes that event and
// POSTs /api/v1/session/switch (with switch=true) to every coop session
// whose assigned_account matches — coop then exits with
// exitCodeCredentialSwitch (10), killing the in-flight Claude session
// before it can do any work.
//
// By blocking here until the broker reports has_api_key=true, the
// controller's first set_token call will already have happened (or it lands
// during the wait and is a no-op once we proceed). The next periodic sync
// sees token_changed=false → no Refreshed event → no distributor switch →
// the first Claude session survives.
//
// On timeout, this function logs a warning and returns — coop still gets
// started, just at risk of the original race. We prefer "best effort" over
// "block the pod indefinitely" so a misconfigured coopmux does not wedge
// every agent.
func waitForCoopmuxCredentials(ctx context.Context, muxURL, preferredAccount string, timeout time.Duration) {
	if muxURL == "" {
		return
	}
	statusURL := strings.TrimRight(muxURL, "/") + "/api/v1/credentials/status"
	token := envOr("COOP_MUX_AUTH_TOKEN", os.Getenv("COOP_BROKER_TOKEN"))
	client := &http.Client{Timeout: 5 * time.Second}

	deadline := time.Now().Add(timeout)
	backoff := 500 * time.Millisecond
	const maxBackoff = 4 * time.Second

	var lastLog time.Time
	attempts := 0

	for {
		attempts++
		if ctx.Err() != nil {
			return
		}
		if time.Now().After(deadline) {
			logInit("WARNING: coopmux credentials not populated after %s (preferred=%q) — starting coop anyway",
				timeout, preferredAccount)
			return
		}

		ready, err := checkCoopmuxCredentialsReady(ctx, client, statusURL, token, preferredAccount)
		if err == nil && ready {
			if preferredAccount != "" {
				logInit("coopmux credentials ready for account %q (%d attempts)", preferredAccount, attempts)
			} else {
				logInit("coopmux credentials ready (%d attempts)", attempts)
			}
			return
		}

		if time.Since(lastLog) >= 10*time.Second {
			if err != nil {
				logInit("waiting for coopmux credentials: %v", err)
			} else {
				logInit("waiting for coopmux credentials (preferred=%q)", preferredAccount)
			}
			lastLog = time.Now()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// checkCoopmuxCredentialsReady performs a single GET against the status
// endpoint and returns true when credentials are populated. When
// preferredAccount is set, only that account counts; otherwise any account
// with has_api_key=true satisfies the check.
func checkCoopmuxCredentialsReady(
	ctx context.Context,
	client *http.Client,
	url, token, preferredAccount string,
) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status %d", resp.StatusCode)
	}

	var statuses []struct {
		Name      string `json:"name"`
		HasAPIKey bool   `json:"has_api_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		return false, fmt.Errorf("decode status: %w", err)
	}

	if preferredAccount != "" {
		for _, s := range statuses {
			if s.Name == preferredAccount && s.HasAPIKey {
				return true, nil
			}
		}
		return false, nil
	}
	for _, s := range statuses {
		if s.HasAPIKey {
			return true, nil
		}
	}
	return false, nil
}
