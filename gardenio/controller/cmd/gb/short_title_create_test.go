package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Pins what `gb escalation create` and `gb decision create` send as
// short_title once the beadsapi create choke point derives it (plan
// gd-YZATAIOhfW Task 6): neither command authors one, Derive does.

// captureShortTitle points the gb daemon client at a stub that records the
// short_title of the first POST /v1/beads and answers every list with nothing.
func captureShortTitle(t *testing.T) *string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			var req struct {
				ShortTitle string `json:"short_title"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if got == "" {
				got = req.ShortTitle
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-short-title"})
			return
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	t.Cleanup(srv.Close)

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon })
	return &got
}

func TestEscalationCreate_ShortTitleIsRefPrefix(t *testing.T) {
	got := captureShortTitle(t)

	cmd := escalationCreateCmd
	cmd.SetContext(t.Context())
	cmd.Flags().Set("title", "Merge PR #295: first-admin hook brute-force flag (gd-DAy1iWvO4y…)")
	cmd.Flags().Set("operation-type", "test")
	t.Cleanup(func() {
		cmd.Flags().Set("title", "")
		cmd.Flags().Set("operation-type", "")
	})

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("escalation create: %v", err)
	}
	if *got != "Merge PR #295" {
		t.Errorf("short_title = %q, want %q", *got, "Merge PR #295")
	}
}

func TestDecisionCreate_ShortTitleKeepsQuestion(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	got := captureShortTitle(t)

	runDecisionCreate(t, "cicd watchdog fired 3 times overnight — extend watchdog coverage?", "")
	if *got != "extend watchdog coverage?" {
		t.Errorf("short_title = %q, want %q", *got, "extend watchdog coverage?")
	}
}
