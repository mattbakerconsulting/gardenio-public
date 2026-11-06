package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// runGroupOpen resets the command flags, applies the given argv, and runs the
// command against the bridge server — exercising the real cobra flag parsing.
func runGroupOpen(t *testing.T, argv []string) map[string]any {
	t.Helper()
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/groups/open" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackGroupOpenResponse{
			OK: true, Channel: "G-TEST", Agent: "gardenio-abcd",
		})
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	// Reset flags to defaults so tests don't bleed into each other.
	slackGroupOpenCmd.Flags().Set("project", "")
	slackGroupOpenCmd.Flags().Set("role", "")
	slackGroupOpenCmd.Flags().Set("message", "")
	slackGroupOpenCmd.Flags().Set("prompt", "")
	slackGroupOpenCmd.Flags().Set("spawn", "false")

	// Parse the flags then run with the residual (positional) args.
	if err := slackGroupOpenCmd.ParseFlags(argv); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	posArgs := slackGroupOpenCmd.Flags().Args()

	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	err := runSlackGroupOpen(slackGroupOpenCmd, posArgs)
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatalf("runSlackGroupOpen: %v", err)
	}
	return received
}

// TestGroupOpen_FlagsAndBody verifies --project/--role/--message/--prompt parse
// and land in the POST body, and that any scoping flag implies spawn=true.
func TestGroupOpen_FlagsAndBody(t *testing.T) {
	body := runGroupOpen(t, []string{
		"U-alice", "bob@x.com",
		"--project", "gardenio",
		"--role", "gardener",
		"--message", "hello team",
		"--prompt", "do the thing",
	})

	users, ok := body["users"].([]any)
	if !ok || len(users) != 2 {
		t.Fatalf("users = %v, want 2 entries", body["users"])
	}
	if users[0] != "U-alice" || users[1] != "bob@x.com" {
		t.Errorf("users = %v, want [U-alice bob@x.com]", users)
	}
	if body["project"] != "gardenio" {
		t.Errorf("project = %v, want gardenio", body["project"])
	}
	if body["role"] != "gardener" {
		t.Errorf("role = %v, want gardener", body["role"])
	}
	if body["message"] != "hello team" {
		t.Errorf("message = %v, want hello team", body["message"])
	}
	if body["prompt"] != "do the thing" {
		t.Errorf("prompt = %v, want do the thing", body["prompt"])
	}
	// A scoping flag implies spawn.
	if body["spawn"] != true {
		t.Errorf("spawn = %v, want true (implied by --project/--role)", body["spawn"])
	}
}

// TestGroupOpen_NoScopingFlagsNoSpawn verifies that with no scoping flag and no
// --spawn, the request carries spawn=false (open-only).
func TestGroupOpen_NoScopingFlagsNoSpawn(t *testing.T) {
	body := runGroupOpen(t, []string{"U-alice", "U-bob"})
	if body["spawn"] != false {
		t.Errorf("spawn = %v, want false", body["spawn"])
	}
	if _, present := body["project"]; present {
		t.Errorf("project should be omitted when unset, got %v", body["project"])
	}
}

// TestGroupOpen_ExplicitSpawnFlag verifies --spawn alone forces spawn=true.
func TestGroupOpen_ExplicitSpawnFlag(t *testing.T) {
	body := runGroupOpen(t, []string{"U-alice", "U-bob", "--spawn"})
	if body["spawn"] != true {
		t.Errorf("spawn = %v, want true (explicit --spawn)", body["spawn"])
	}
}

// TestGroupOpen_MissingBridgeURL errors cleanly when the bridge URL is unset.
func TestGroupOpen_MissingBridgeURL(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	t.Setenv("BOAT_SLACK_BRIDGE_URL", "")
	err := runSlackGroupOpen(slackGroupOpenCmd, []string{"U-alice"})
	if err == nil {
		t.Fatal("expected error when SLACK_BRIDGE_URL is unset")
	}
}
