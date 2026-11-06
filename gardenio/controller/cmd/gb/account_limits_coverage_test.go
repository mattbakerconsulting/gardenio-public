package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// ── runAccountLimits ──────────────────────────────────────────────────────

func TestRunAccountLimits_NoBeads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountLimits(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunAccountLimits_NoProbeData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-A1", "title": "acct-1", "type": "account",
					"status": "open", "labels": []string{},
					"fields": json.RawMessage(`{"provider":"anthropic"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountLimits(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunAccountLimits_WithProbeData(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := fmt.Sprintf(`{"provider":"anthropic","utilization_5h":"0.42","utilization_7d":"0.15","throttle_status":"rejected","representative_claim":"5h","overage_disabled_reason":"quota","throttle_reset_at":"%s","probe_checked_at":"%s","soft_failover_state":"warming","soft_failover_threshold":"0.8"}`, future, past)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-A1", "title": "acct-z", "type": "account",
					"status": "open", "labels": []string{},
					"fields": json.RawMessage(fields),
				},
				{
					"id": "gd-A2", "title": "acct-a", "type": "account",
					"status": "open", "labels": []string{},
					"fields": json.RawMessage(`{"provider":"anthropic","utilization_5h":"0.99","utilization_7d":"1.0","soft_failover_threshold":"0.5"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountLimits(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunAccountLimits_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"beads": []map[string]any{
				{
					"id": "gd-A1", "title": "acct", "type": "account",
					"status": "open", "labels": []string{},
					"fields": json.RawMessage(`{"utilization_5h":"0.1"}`),
				},
			},
		})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	oldJ := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJ })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountLimits(cmd, nil); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestRunAccountLimits_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runAccountLimits(cmd, nil); err == nil {
		t.Error("expected error")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func TestFmtPct_Extra(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0%"},
		{0.5, "50.0%"},
		{0.9995, "100%"},
		{0.123, "12.3%"},
	}
	for _, c := range cases {
		if got := fmtPct(c.in); got != c.want {
			t.Errorf("fmtPct(%v): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestFmtFailoverState_Extra(t *testing.T) {
	if got := fmtFailoverState("warming", 0.8); got != "WARMING @80.0%" {
		t.Errorf("warming: got %q", got)
	}
	if got := fmtFailoverState("warming", 0); got != "WARMING" {
		t.Errorf("warming no threshold: got %q", got)
	}
	if got := fmtFailoverState("", 0.5); got != "@50.0%" {
		t.Errorf("threshold: got %q", got)
	}
	if got := fmtFailoverState("", 0); got != "-" {
		t.Errorf("empty: got %q", got)
	}
}

func TestFmtDuration_Extra(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 30*time.Minute, "2h30m"},
		{25 * time.Hour, "1d1h"},
		{48 * time.Hour, "2d"},
	}
	for _, c := range cases {
		if got := fmtDuration(c.d); got != c.want {
			t.Errorf("fmtDuration(%v): got %q want %q", c.d, got, c.want)
		}
	}
}

func TestParseFieldFloat_Extra(t *testing.T) {
	if got := parseFieldFloat(""); got != 0 {
		t.Errorf("empty: %v", got)
	}
	if got := parseFieldFloat("3.14"); got != 3.14 {
		t.Errorf("3.14: %v", got)
	}
	if got := parseFieldFloat("not-a-num"); got != 0 {
		t.Errorf("bad: %v", got)
	}
}
