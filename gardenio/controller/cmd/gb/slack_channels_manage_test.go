package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureStdout(t, fn) is defined in news_test.go: it runs fn (which returns an
// error), fatals if that error is non-nil, redirects os.Stdout for its duration,
// and returns everything written as a []byte.

// ── splitCSV ───────────────────────────────────────────────────────

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "  ,  , ", nil},
		{"single", "alice@x.com", []string{"alice@x.com"}},
		{"multiple", "a,b,c", []string{"a", "b", "c"}},
		{"trims spaces", " a , b ,c ", []string{"a", "b", "c"}},
		{"drops empties", "a,,b,", []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCSV(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("splitCSV(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ── resolveChannelArg ──────────────────────────────────────────────

func TestResolveChannelArg_PassthroughIDs(t *testing.T) {
	// A value that already looks like a channel id (C…/G…) must pass through
	// verbatim without touching the bridge — even with a leading '#'.
	for _, in := range []string{"C0123ABCD", "#C0123ABCD", "G0456DEFG"} {
		got, err := resolveChannelArg("http://unreachable.invalid", in)
		if err != nil {
			t.Fatalf("resolveChannelArg(%q) unexpected error: %v", in, err)
		}
		want := strings.TrimPrefix(in, "#")
		if got != want {
			t.Errorf("resolveChannelArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveChannelArg_ResolvesName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/channels" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]slackChannelInfo{
			{ID: "C-RESOLVED", Name: "proj-falcon"},
		})
	}))
	defer server.Close()

	got, err := resolveChannelArg(server.URL, "proj-falcon")
	if err != nil {
		t.Fatalf("resolveChannelArg: %v", err)
	}
	if got != "C-RESOLVED" {
		t.Errorf("resolveChannelArg = %q, want C-RESOLVED", got)
	}
}

func TestResolveChannelArg_NameNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]slackChannelInfo{})
	}))
	defer server.Close()

	if _, err := resolveChannelArg(server.URL, "nope"); err == nil {
		t.Fatal("expected error for unknown channel name")
	}
}

// ── channel-create ─────────────────────────────────────────────────

func TestSlackChannelCreate_PostsBodyAndPrints(t *testing.T) {
	var gotPath string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackCreateChannelResponse{
			OK: true, ID: "C-NEW", Name: "proj-falcon", Private: true,
			Invited: []string{"alice@x.com"}, Warning: "some invites skipped",
		})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	// Reset flags between runs so state doesn't leak across tests.
	slackChannelCreateCmd.Flags().Set("private", "true")
	slackChannelCreateCmd.Flags().Set("topic", "Falcon launch")
	slackChannelCreateCmd.Flags().Set("purpose", "the falcon channel")
	slackChannelCreateCmd.Flags().Set("invite", "alice@x.com, bob@x.com")
	defer func() {
		slackChannelCreateCmd.Flags().Set("private", "false")
		slackChannelCreateCmd.Flags().Set("topic", "")
		slackChannelCreateCmd.Flags().Set("purpose", "")
		slackChannelCreateCmd.Flags().Set("invite", "")
	}()

	out := string(captureStdout(t, func() error {
		return runSlackChannelCreate(slackChannelCreateCmd, []string{"proj-falcon"})
	}))

	if gotPath != "/api/slack/channels/create" {
		t.Errorf("path = %q, want /api/slack/channels/create", gotPath)
	}
	if body["name"] != "proj-falcon" || body["private"] != true {
		t.Errorf("body name/private = %v/%v", body["name"], body["private"])
	}
	if body["topic"] != "Falcon launch" || body["purpose"] != "the falcon channel" {
		t.Errorf("body topic/purpose = %v/%v", body["topic"], body["purpose"])
	}
	invited, ok := body["invite"].([]any)
	if !ok || len(invited) != 2 {
		t.Errorf("invite = %v, want 2 entries", body["invite"])
	}
	if !strings.Contains(out, "Created private channel #proj-falcon (C-NEW)") {
		t.Errorf("output missing created line: %q", out)
	}
	if !strings.Contains(out, "Invited 1 member") || !strings.Contains(out, "Warning: some invites skipped") {
		t.Errorf("output missing invited/warning: %q", out)
	}
}

func TestSlackChannelCreate_OmitsOptionalFields(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackCreateChannelResponse{OK: true, ID: "C1", Name: "n"})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	_ = captureStdout(t, func() error {
		return runSlackChannelCreate(slackChannelCreateCmd, []string{"n"})
	})

	for _, k := range []string{"topic", "purpose", "invite"} {
		if _, present := body[k]; present {
			t.Errorf("body[%q] should be omitted when unset, got %v", k, body[k])
		}
	}
}

func TestSlackChannelCreate_JSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackCreateChannelResponse{OK: true, ID: "C-JSON", Name: "jc"})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	jsonOutput = true
	defer func() { jsonOutput = false }()

	out := captureStdout(t, func() error {
		return runSlackChannelCreate(slackChannelCreateCmd, []string{"jc"})
	})

	var got slackCreateChannelResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got.ID != "C-JSON" {
		t.Errorf("json ID = %q, want C-JSON", got.ID)
	}
}

func TestSlackChannelCreate_BridgeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackCreateChannelResponse{OK: false, Error: "name_taken"})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	err := runSlackChannelCreate(slackChannelCreateCmd, []string{"dup"})
	if err == nil || !strings.Contains(err.Error(), "name_taken") {
		t.Fatalf("expected create-failed error, got %v", err)
	}
}

func TestSlackChannelCreate_MissingBridgeURL(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	if err := runSlackChannelCreate(slackChannelCreateCmd, []string{"x"}); err == nil {
		t.Fatal("expected error when SLACK_BRIDGE_URL is unset")
	}
}

// ── channel-archive / channel-unarchive (runChannelAction) ─────────

func TestRunChannelAction_ArchiveResolvesAndPosts(t *testing.T) {
	var gotPath string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/slack/channels":
			_ = json.NewEncoder(w).Encode([]slackChannelInfo{{ID: "C-ARCH", Name: "old-chan"}})
		case "/api/slack/channels/archive":
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(slackChannelActionResponse{OK: true, Channel: "C-ARCH"})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	out := string(captureStdout(t, func() error {
		return runChannelAction(slackChannelArchiveCmd, "old-chan", "/api/slack/channels/archive", "Archived")
	}))

	if gotPath != "/api/slack/channels/archive" {
		t.Errorf("path = %q", gotPath)
	}
	if body["channel"] != "C-ARCH" {
		t.Errorf("channel = %v, want C-ARCH (name should be resolved)", body["channel"])
	}
	if !strings.Contains(out, "Archived channel C-ARCH") {
		t.Errorf("output missing archived line: %q", out)
	}
}

func TestRunChannelAction_BridgeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackChannelActionResponse{OK: false, Error: "not_in_channel"})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	// Pass an id so no name-resolution round trip is needed.
	err := runChannelAction(slackChannelUnarchiveCmd, "C-XYZ", "/api/slack/channels/unarchive", "Unarchived")
	if err == nil || !strings.Contains(err.Error(), "not_in_channel") {
		t.Fatalf("expected unarchive-failed error, got %v", err)
	}
}

func TestRunChannelAction_MissingBridgeURL(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	err := runChannelAction(slackChannelArchiveCmd, "C-XYZ", "/api/slack/channels/archive", "Archived")
	if err == nil {
		t.Fatal("expected error when SLACK_BRIDGE_URL is unset")
	}
}

func TestRunSlackChannelArchive_Wrappers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackChannelActionResponse{OK: true, Channel: "C-W"})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	_ = captureStdout(t, func() error {
		if err := runSlackChannelArchive(slackChannelArchiveCmd, []string{"C-W"}); err != nil {
			return err
		}
		return runSlackChannelUnarchive(slackChannelUnarchiveCmd, []string{"C-W"})
	})
}

// ── invite ─────────────────────────────────────────────────────────

func TestRunSlackInvite_PostsUsersAndPrints(t *testing.T) {
	var gotPath string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackInviteResponse{
			OK: true, Channel: "C-INV", Invited: []string{"U-a", "U-b"},
		})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	out := string(captureStdout(t, func() error {
		return runSlackInvite(slackInviteCmd, []string{"C-INV", "U-a", "U-b"})
	}))

	if gotPath != "/api/slack/channels/invite" {
		t.Errorf("path = %q", gotPath)
	}
	users, ok := body["users"].([]any)
	if !ok || len(users) != 2 || users[0] != "U-a" || users[1] != "U-b" {
		t.Errorf("users = %v, want [U-a U-b]", body["users"])
	}
	if !strings.Contains(out, "Invited 2 member(s) to C-INV") {
		t.Errorf("output missing invited line: %q", out)
	}
}

func TestRunSlackInvite_JSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackInviteResponse{OK: true, Channel: "C1", Invited: []string{"U1"}})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	jsonOutput = true
	defer func() { jsonOutput = false }()

	out := captureStdout(t, func() error {
		return runSlackInvite(slackInviteCmd, []string{"C1", "U1"})
	})
	var got slackInviteResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got.Channel != "C1" {
		t.Errorf("json Channel = %q, want C1", got.Channel)
	}
}

func TestRunSlackInvite_BridgeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackInviteResponse{OK: false, Error: "cant_invite"})
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	err := runSlackInvite(slackInviteCmd, []string{"C-INV", "U-a"})
	if err == nil || !strings.Contains(err.Error(), "cant_invite") {
		t.Fatalf("expected invite-failed error, got %v", err)
	}
}

func TestRunSlackInvite_MissingBridgeURL(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	if err := runSlackInvite(slackInviteCmd, []string{"C-INV", "U-a"}); err == nil {
		t.Fatal("expected error when SLACK_BRIDGE_URL is unset")
	}
}

// ── postSlackJSON transport errors ─────────────────────────────────

func TestPostSlackJSON_Non2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	var out slackChannelActionResponse
	err := postSlackJSON(server.URL, "/api/slack/channels/archive", map[string]any{"channel": "C1"}, &out)
	if err == nil {
		t.Fatal("expected error on non-2xx bridge response")
	}
}

func TestPostSlackJSON_Unreachable(t *testing.T) {
	var out slackChannelActionResponse
	err := postSlackJSON("http://127.0.0.1:0", "/api/slack/x", map[string]any{}, &out)
	if err == nil {
		t.Fatal("expected error when bridge is unreachable")
	}
}
