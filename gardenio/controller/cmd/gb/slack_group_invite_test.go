package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// runGroupInvite resets the command flags, applies the given argv, and runs the
// command against the bridge server — exercising the real cobra flag parsing and
// the {channel}-in-path routing.
func runGroupInvite(t *testing.T, argv []string) (path string, received map[string]any) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath preserves %-encoding so tests can assert the channel id was
		// path-escaped on the way out (r.URL.Path is already decoded).
		path = r.URL.EscapedPath()
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackGroupInviteResponse{
			OK: true, OldChannel: "G-OLD", Channel: "G-NEW", Moved: true, Agent: "gardenio-abcd",
		})
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	slackGroupInviteCmd.Flags().Set("message", "")

	if err := slackGroupInviteCmd.ParseFlags(argv); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	posArgs := slackGroupInviteCmd.Flags().Args()

	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	err := runSlackGroupInvite(slackGroupInviteCmd, posArgs)
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatalf("runSlackGroupInvite: %v", err)
	}
	return path, received
}

// TestGroupInvite_PathAndBody verifies the channel lands in the URL path, the
// invitees land in the body, and --message is forwarded.
func TestGroupInvite_PathAndBody(t *testing.T) {
	path, body := runGroupInvite(t, []string{
		"G-OLD", "U-carol", "dave@x.com",
		"--message", "welcome",
	})

	if path != "/api/slack/groups/G-OLD/invite" {
		t.Errorf("path = %q, want /api/slack/groups/G-OLD/invite", path)
	}
	users, ok := body["users"].([]any)
	if !ok || len(users) != 2 {
		t.Fatalf("users = %v, want 2 entries", body["users"])
	}
	if users[0] != "U-carol" || users[1] != "dave@x.com" {
		t.Errorf("users = %v, want [U-carol dave@x.com]", users)
	}
	if body["message"] != "welcome" {
		t.Errorf("message = %v, want welcome", body["message"])
	}
}

// TestGroupInvite_MessageOmittedWhenUnset verifies --message is omitted from the
// body when not supplied.
func TestGroupInvite_MessageOmittedWhenUnset(t *testing.T) {
	_, body := runGroupInvite(t, []string{"G-OLD", "U-carol"})
	if _, present := body["message"]; present {
		t.Errorf("message should be omitted when unset, got %v", body["message"])
	}
}

// TestGroupInvite_PathEscapesChannel verifies a channel id is path-escaped.
func TestGroupInvite_PathEscapesChannel(t *testing.T) {
	path, _ := runGroupInvite(t, []string{"G/WEIRD ID", "U-carol"})
	if path != "/api/slack/groups/G%2FWEIRD%20ID/invite" {
		t.Errorf("path = %q, want escaped channel", path)
	}
}

// TestGroupInvite_MissingBridgeURL errors cleanly when the bridge URL is unset.
func TestGroupInvite_MissingBridgeURL(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	t.Setenv("BOAT_SLACK_BRIDGE_URL", "")
	err := runSlackGroupInvite(slackGroupInviteCmd, []string{"G-OLD", "U-carol"})
	if err == nil {
		t.Fatal("expected error when SLACK_BRIDGE_URL is unset")
	}
}
