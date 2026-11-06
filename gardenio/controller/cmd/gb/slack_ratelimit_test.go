package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestBridgeChannelListError_RateLimitHeader verifies a 429 with a Retry-After
// header produces a distinct "rate limited" error carrying the retry hint,
// rather than a bare "HTTP 500" that reads like the channel is missing
// (ka-0000001584).
func TestBridgeChannelListError_RateLimitHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	got := bridgeChannelListError(resp).Error()
	if !strings.Contains(got, "rate limited") {
		t.Errorf("error %q should mention rate limiting", got)
	}
	if !strings.Contains(got, "42s") {
		t.Errorf("error %q should include the Retry-After hint (42s)", got)
	}
}

// TestBridgeChannelListError_RateLimitBody verifies the retry hint is recovered
// from the JSON body when the Retry-After header is absent.
func TestBridgeChannelListError_RateLimitBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slack rate limited, retry in 17s","retry_after_s":17}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	got := bridgeChannelListError(resp).Error()
	if !strings.Contains(got, "rate limited") {
		t.Errorf("error %q should mention rate limiting", got)
	}
	if !strings.Contains(got, "17s") {
		t.Errorf("error %q should include the retry hint from the body (17s)", got)
	}
}

// TestBridgeChannelListError_Non429 keeps the legacy generic message for other
// non-200 statuses.
func TestBridgeChannelListError_Non429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	got := bridgeChannelListError(resp).Error()
	if !strings.Contains(got, "HTTP 502") {
		t.Errorf("error %q should report the raw status for non-429", got)
	}
}

// TestRunSlackChannel_ByName_RateLimited verifies the name-resolution path
// surfaces the rate-limit message end to end (the exact symptom in the bug:
// `gb slack channel <name>` under rate limit).
func TestRunSlackChannel_ByName_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)

	cmd := &cobra.Command{}
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Int("max-chars", 8000, "")
	cmd.Flags().Bool("include-bots", false, "")

	err := runSlackChannel(cmd, []string{"acme-devops"})
	if err == nil {
		t.Fatal("expected an error under rate limiting")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error %q should mention rate limiting, not a generic HTTP code", err.Error())
	}
}
