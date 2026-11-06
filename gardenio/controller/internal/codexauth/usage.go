package codexauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultUsageURL is the ChatGPT backend route the Codex CLI itself reads for
// plan limits. It is an internal route and may change shape: callers treat any
// failure as "unknown" and fail open (contract gd-3gTWiNEhg1 §3).
const DefaultUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// UsageURLAllowedHosts is the allowlist for UsageURL overrides.
var UsageURLAllowedHosts = []string{"chatgpt.com"}

// Window is one rate-limit window.
type Window struct {
	// UsedFraction is 0.0-1.0 (the API reports used_percent 0-100).
	UsedFraction float64
	// ResetAt is zero when the API gave no reset information.
	ResetAt time.Time
	// Seconds is the window length when reported, else 0.
	Seconds int64
}

// Usage is the subscription's limit state.
type Usage struct {
	// Primary is the short (5h) window, Secondary the weekly one. Either may be
	// nil when the API omits it.
	Primary   *Window
	Secondary *Window
	// LimitReached is true when the plan reports it is out of quota.
	LimitReached bool
}

// UsageReader reads wham/usage.
type UsageReader struct {
	HTTP *http.Client
	URL  string
	Now  func() time.Time
}

// NewUsageReader builds a reader. An empty url means DefaultUsageURL.
func NewUsageReader(url string) *UsageReader {
	if url == "" {
		url = DefaultUsageURL
	}
	return &UsageReader{HTTP: &http.Client{Timeout: 15 * time.Second}, URL: url, Now: time.Now}
}

type wireWindow struct {
	UsedPercent        float64  `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *float64 `json:"reset_at"`
	ResetsAt           *float64 `json:"resets_at"`
}

type wireUsage struct {
	RateLimit *struct {
		LimitReached    bool        `json:"limit_reached"`
		Allowed         *bool       `json:"allowed"`
		PrimaryWindow   *wireWindow `json:"primary_window"`
		SecondaryWindow *wireWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

func (r *UsageReader) window(w *wireWindow) *Window {
	if w == nil {
		return nil
	}
	out := &Window{UsedFraction: w.UsedPercent / 100, Seconds: w.LimitWindowSeconds}
	switch {
	case w.ResetAt != nil && *w.ResetAt > 0:
		out.ResetAt = time.Unix(int64(*w.ResetAt), 0)
	case w.ResetsAt != nil && *w.ResetsAt > 0:
		out.ResetAt = time.Unix(int64(*w.ResetsAt), 0)
	case w.ResetAfterSeconds != nil && *w.ResetAfterSeconds > 0:
		out.ResetAt = r.Now().Add(time.Duration(*w.ResetAfterSeconds) * time.Second)
	}
	return out
}

// Read fetches the limits with the account's access token. Errors carry the
// HTTP status only.
func (r *UsageReader) Read(ctx context.Context, accessToken, accountID string) (Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return Usage{}, fmt.Errorf("wham/usage: building request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("wham/usage: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		return Usage{}, fmt.Errorf("wham/usage: HTTP %d", resp.StatusCode)
	}
	var w wireUsage
	if err := json.Unmarshal(data, &w); err != nil || w.RateLimit == nil {
		return Usage{}, fmt.Errorf("wham/usage: unexpected response shape")
	}
	u := Usage{
		Primary:      r.window(w.RateLimit.PrimaryWindow),
		Secondary:    r.window(w.RateLimit.SecondaryWindow),
		LimitReached: w.RateLimit.LimitReached || (w.RateLimit.Allowed != nil && !*w.RateLimit.Allowed),
	}
	if u.Primary == nil && u.Secondary == nil && !u.LimitReached {
		return Usage{}, fmt.Errorf("wham/usage: no windows in response")
	}
	return u, nil
}
