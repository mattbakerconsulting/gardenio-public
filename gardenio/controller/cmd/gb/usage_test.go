package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestAccountLiteLLMURL(t *testing.T) {
	const global = "https://litellm.app.devops.acme.example"
	// Empty / whitespace base_url falls back to the global --litellm-url.
	if got := accountLiteLLMURL(global, ""); got != global {
		t.Errorf("empty base_url should fall back to global, got %q", got)
	}
	if got := accountLiteLLMURL(global, "   "); got != global {
		t.Errorf("whitespace base_url should fall back to global, got %q", got)
	}
	// A non-empty per-account base_url wins (ka-0000003221).
	const incluster = "http://gardenio-e2e-litellm:4000"
	if got := accountLiteLLMURL(global, incluster); got != incluster {
		t.Errorf("per-account base_url should win, got %q", got)
	}
}

func TestQueryLiteLLMKeyInfo_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/info" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("key"); got != "sk-test-key" {
			t.Errorf("key param = %q, want %q", got, "sk-test-key")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer master-key-123" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer master-key-123")
		}

		resp := liteLLMKeyInfoResponse{
			Info: liteLLMKeyInfo{
				Key:   "sk-test-key",
				Spend: 142.30,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := srv.Client()
	info, err := queryLiteLLMKeyInfo(context.Background(), client, srv.URL, "master-key-123", "sk-test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Spend != 142.30 {
		t.Errorf("Spend = %f, want 142.30", info.Spend)
	}
}

func TestQueryLiteLLMKeyInfo_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()

	client := srv.Client()
	_, err := queryLiteLLMKeyInfo(context.Background(), client, srv.URL, "bad-key", "sk-test")
	if err == nil {
		t.Fatal("expected error for HTTP 403")
	}
}

func TestQuerySpendLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spend/logs" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		logs := []liteLLMSpendLog{
			{APIKey: "sk-test", Spend: 10.50, StartTime: "2026-03-21T08:00:00"},
			{APIKey: "sk-test", Spend: 5.25, StartTime: "2026-03-21T14:00:00"},
			{APIKey: "sk-test", Spend: 20.00, StartTime: "2026-03-19T10:00:00"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logs)
	}))
	defer srv.Close()

	client := srv.Client()
	dayStart := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)
	weekStart := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC) // Monday

	daily, weekly := queryPeriodSpend(context.Background(), client, srv.URL, "master", "sk-test", dayStart, weekStart)

	// Daily: 10.50 + 5.25 = 15.75
	if daily < 15.74 || daily > 15.76 {
		t.Errorf("daily = %f, want ~15.75", daily)
	}
	// Weekly: 10.50 + 5.25 + 20.00 = 35.75
	if weekly < 35.74 || weekly > 35.76 {
		t.Errorf("weekly = %f, want ~35.75", weekly)
	}
}

func TestQuerySpendLogs_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := srv.Client()
	daily, weekly := queryPeriodSpend(context.Background(), client, srv.URL, "master", "sk-test",
		time.Now(), time.Now().AddDate(0, 0, -7))
	if daily != 0 || weekly != 0 {
		t.Errorf("expected (0, 0) on error, got (%f, %f)", daily, weekly)
	}
}

func TestFmtSpend(t *testing.T) {
	if got := fmtSpend(0); got != "-" {
		t.Errorf("fmtSpend(0) = %q, want '-'", got)
	}
	if got := fmtSpend(142.305); got != "$142.31" {
		t.Errorf("fmtSpend(142.305) = %q, want '$142.31'", got)
	}
}

func TestKeySuffix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"sk-proj-abc123xyz789", "...xyz789"},
		{"short", "short"},
		{"sk-123", "sk-123"},
		{"abcdefghij", "...efghij"},
	}
	for _, tt := range tests {
		if got := keySuffix(tt.input); got != tt.want {
			t.Errorf("keySuffix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate short = %q", got)
	}
	long := "this is a very long error message that should be truncated"
	got := truncate(long, 20)
	if len(got) != 20 {
		t.Errorf("truncate len = %d, want 20", len(got))
	}
}

// TestBuildKeylessUsage exercises the keyless (controller-synced) path: spend
// figures are read straight off the account-bead cost fields that spend_sync.go
// writes, with no LiteLLM master key. since-reset = total - spend_at_last_reset.
func TestBuildKeylessUsage(t *testing.T) {
	syncTime := time.Date(2026, 6, 23, 18, 30, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		// Account with full synced spend fields and a recorded reset.
		{
			ID:        "acct-1",
			Title:     "anthropic-prod-01",
			UpdatedAt: syncTime,
			Fields: map[string]string{
				"litellm_key":         "sk-prod-01-abcdef",
				"provider":            "anthropic",
				"status":              "active",
				"total_cost_usd":      "142.50",
				"daily_cost_usd":      "12.25",
				"weekly_cost_usd":     "80.00",
				"spend_at_last_reset": "100.00",
				"last_reset_at":       "2026-06-20T00:00:00Z",
			},
		},
		// Account with no recorded reset and no status → status defaults to
		// "active". Newest UpdatedAt, so it should drive latestSync.
		{
			ID:        "acct-2",
			Title:     "anthropic-prod-02",
			UpdatedAt: syncTime.Add(5 * time.Minute),
			Fields: map[string]string{
				"litellm_key":    "sk-prod-02-ghijkl", // gitleaks:allow: fake key
				"provider":       "anthropic",
				"total_cost_usd": "10.00",
			},
		},
		// No litellm_key → skipped, matching live mode's filter.
		{
			ID:    "acct-3",
			Title: "scaffold-no-key",
			Fields: map[string]string{
				"total_cost_usd": "999.00",
			},
		},
		// nil bead → skipped without panic.
		nil,
	}

	results, latestSync := buildKeylessUsage(beads)

	if len(results) != 2 {
		t.Fatalf("expected 2 accounts (key'd only), got %d", len(results))
	}

	r0 := results[0]
	if r0.Name != "anthropic-prod-01" {
		t.Errorf("results[0].Name = %q, want anthropic-prod-01", r0.Name)
	}
	if r0.Provider != "anthropic" {
		t.Errorf("results[0].Provider = %q, want anthropic", r0.Provider)
	}
	if r0.Status != "active" {
		t.Errorf("results[0].Status = %q, want active", r0.Status)
	}
	if r0.Total != 142.50 {
		t.Errorf("results[0].Total = %v, want 142.50", r0.Total)
	}
	if r0.Daily != 12.25 {
		t.Errorf("results[0].Daily = %v, want 12.25", r0.Daily)
	}
	if r0.Weekly != 80.00 {
		t.Errorf("results[0].Weekly = %v, want 80.00", r0.Weekly)
	}
	// since-reset = total - spend_at_last_reset = 142.50 - 100.00 = 42.50
	if r0.SinceReset < 42.49 || r0.SinceReset > 42.51 {
		t.Errorf("results[0].SinceReset = %v, want ~42.50", r0.SinceReset)
	}
	if r0.ResetAt != "2026-06-20T00:00:00Z" {
		t.Errorf("results[0].ResetAt = %q", r0.ResetAt)
	}
	if r0.KeySuffix != "...abcdef" {
		t.Errorf("results[0].KeySuffix = %q, want ...abcdef", r0.KeySuffix)
	}

	// Account 2: no spend_at_last_reset → SinceReset stays 0; no status → active.
	r1 := results[1]
	if r1.Status != "active" {
		t.Errorf("results[1].Status = %q, want active (defaulted)", r1.Status)
	}
	if r1.SinceReset != 0 {
		t.Errorf("results[1].SinceReset = %v, want 0 (no reset snapshot)", r1.SinceReset)
	}

	// latestSync is the newest UpdatedAt across key'd beads.
	want := syncTime.Add(5 * time.Minute)
	if !latestSync.Equal(want) {
		t.Errorf("latestSync = %v, want %v", latestSync, want)
	}
}

// TestBuildKeylessUsage_NoAccounts confirms an empty / key-less input yields
// no rows and a zero sync time (the caller renders the "no accounts" notice).
func TestBuildKeylessUsage_NoAccounts(t *testing.T) {
	results, latestSync := buildKeylessUsage([]*beadsapi.BeadDetail{
		{ID: "x", Title: "no-key", Fields: map[string]string{"total_cost_usd": "5"}},
	})
	if len(results) != 0 {
		t.Errorf("expected 0 results for key-less accounts, got %d", len(results))
	}
	if !latestSync.IsZero() {
		t.Errorf("expected zero latestSync, got %v", latestSync)
	}
}
