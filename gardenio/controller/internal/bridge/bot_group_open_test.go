package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// groupOpenSlackServer returns a Slack test server that satisfies the calls the
// group-open handler makes: conversations.open (returns an mpim channel),
// chat.postMessage (intro), and users.lookupByEmail. It records the open + post
// calls so tests can assert on them.
func groupOpenSlackServer(t *testing.T, mpimChannel string) (*httptest.Server, *[]slackCall) {
	t.Helper()
	var calls []slackCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			calls = append(calls, slackCall{Method: "conversations.open", Text: r.FormValue("users")})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": mpimChannel},
			})
		case "/chat.postMessage":
			calls = append(calls, slackCall{
				Method:  "chat.postMessage",
				Channel: r.FormValue("channel"),
				Text:    r.FormValue("text"),
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "channel": r.FormValue("channel"), "ts": "9999.8888",
			})
		case "/users.lookupByEmail":
			calls = append(calls, slackCall{Method: "users.lookupByEmail", Text: r.FormValue("email")})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "user": map[string]any{"id": "U-from-email"},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	return srv, &calls
}

// newGroupOpenTestBot wires a Bot with a StateManager (needed for the
// thread→agent binding) plus a default channel/project mapping.
func newGroupOpenTestBot(t *testing.T, daemon BeadClient, srv *httptest.Server) *Bot {
	t.Helper()
	bot := newTestBot(daemon, srv)
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	return bot
}

// doGroupOpen POSTs a groupOpenRequest through the real handler and returns the
// decoded response + status.
func doGroupOpen(t *testing.T, api *GroupOpenAPI, body map[string]any) (groupOpenResponse, int) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/slack/groups/open", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	api.handleOpenGroup(rec, req)
	var resp groupOpenResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	return resp, rec.Code
}

// TestGroupOpen_OpenBindIntro is the core verify test: open + spawn/bind + intro.
func TestGroupOpen_OpenBindIntro(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-DEFAULT")
	srv, calls := groupOpenSlackServer(t, "G-MPIM-1")
	defer srv.Close()

	bot := newGroupOpenTestBot(t, daemon, srv)
	bot.channel = "C-DEFAULT"
	api := NewGroupOpenAPI(bot, daemon)

	resp, code := doGroupOpen(t, api, map[string]any{
		"users":   []string{"U-alice", "U-bob"},
		"project": "gardenio",
		"role":    "crew",
		"prompt":  "Investigate the pager",
		"spawn":   true,
		"message": "Kicking off",
	})

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", code, resp.Error)
	}
	if !resp.OK {
		t.Fatalf("expected ok, got error: %s", resp.Error)
	}
	if resp.Channel != "G-MPIM-1" {
		t.Errorf("expected channel G-MPIM-1, got %q", resp.Channel)
	}
	if resp.Agent == "" {
		t.Error("expected a bound agent name")
	}
	if resp.AgentID == "" {
		t.Error("expected a bound agent bead ID")
	}
	if resp.TS != "9999.8888" {
		t.Errorf("expected intro ts, got %q", resp.TS)
	}

	// The agent bead must be spawned with the group-dm binding fields.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	fields := agentBeads[0].Fields
	if fields["project"] != "gardenio" {
		t.Errorf("project: want gardenio, got %q", fields["project"])
	}
	if fields["spawn_source"] != "group-dm" {
		t.Errorf("spawn_source: want group-dm, got %q", fields["spawn_source"])
	}
	if fields["mode"] != "crew" {
		t.Errorf("mode: want crew, got %q", fields["mode"])
	}
	if fields["slack_thread_channel"] != "G-MPIM-1" {
		t.Errorf("slack_thread_channel: want G-MPIM-1, got %q", fields["slack_thread_channel"])
	}

	// The thread→agent binding must be recorded in state (empty ts binds the
	// whole mpim), so inbound mpim messages resolve to this agent.
	boundAgent, ok := bot.state.GetThreadAgent("G-MPIM-1", "")
	if !ok || boundAgent != resp.Agent {
		t.Errorf("thread binding: want %q, got %q (ok=%v)", resp.Agent, boundAgent, ok)
	}

	// Verify the calls: conversations.open with both users, and an intro post.
	var sawOpen, sawIntro bool
	for _, c := range *calls {
		switch c.Method {
		case "conversations.open":
			sawOpen = true
			if c.Text != "U-alice,U-bob" {
				t.Errorf("conversations.open users: want U-alice,U-bob, got %q", c.Text)
			}
		case "chat.postMessage":
			if c.Channel == "G-MPIM-1" && c.Text == "Kicking off" {
				sawIntro = true
			}
		}
	}
	if !sawOpen {
		t.Error("expected conversations.open call")
	}
	if !sawIntro {
		t.Error("expected intro chat.postMessage on the mpim")
	}
}

// TestGroupOpen_NoSpawn opens the mpim and posts the default intro without
// spawning an agent when spawn is false and no scoping flag is set.
func TestGroupOpen_NoSpawn(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := groupOpenSlackServer(t, "G-MPIM-2")
	defer srv.Close()

	bot := newGroupOpenTestBot(t, daemon, srv)
	api := NewGroupOpenAPI(bot, daemon)

	resp, code := doGroupOpen(t, api, map[string]any{
		"users": []string{"U-alice", "U-bob"},
		"spawn": false,
	})
	if code != http.StatusOK || !resp.OK {
		t.Fatalf("expected ok 200, got %d (%s)", code, resp.Error)
	}
	if resp.Agent != "" {
		t.Errorf("expected no agent, got %q", resp.Agent)
	}
	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Errorf("expected 0 agent beads, got %d", n)
	}
	// Default intro is still posted.
	if resp.TS == "" {
		t.Error("expected an intro message ts even without spawn")
	}
}

// TestGroupOpen_ResolvesEmail resolves an email member to a Slack ID via
// users.lookupByEmail before opening the mpim.
func TestGroupOpen_ResolvesEmail(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls := groupOpenSlackServer(t, "G-MPIM-3")
	defer srv.Close()

	bot := newGroupOpenTestBot(t, daemon, srv)
	api := NewGroupOpenAPI(bot, daemon)

	resp, code := doGroupOpen(t, api, map[string]any{
		"users": []string{"alice@x.com", "U-bob"},
	})
	if code != http.StatusOK || !resp.OK {
		t.Fatalf("expected ok 200, got %d (%s)", code, resp.Error)
	}
	var sawLookup bool
	for _, c := range *calls {
		if c.Method == "users.lookupByEmail" && c.Text == "alice@x.com" {
			sawLookup = true
		}
		if c.Method == "conversations.open" && c.Text != "U-from-email,U-bob" {
			t.Errorf("conversations.open users: want U-from-email,U-bob, got %q", c.Text)
		}
	}
	if !sawLookup {
		t.Error("expected users.lookupByEmail for the email member")
	}
}

// TestGroupOpen_NoUsers rejects a request with no members.
func TestGroupOpen_NoUsers(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := groupOpenSlackServer(t, "G-MPIM-4")
	defer srv.Close()
	bot := newGroupOpenTestBot(t, daemon, srv)
	api := NewGroupOpenAPI(bot, daemon)

	_, code := doGroupOpen(t, api, map[string]any{"users": []string{"", "  "}})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty users, got %d", code)
	}
}

// TestGroupOpen_MethodNotAllowed rejects non-POST.
func TestGroupOpen_MethodNotAllowed(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := groupOpenSlackServer(t, "G-MPIM-5")
	defer srv.Close()
	bot := newGroupOpenTestBot(t, daemon, srv)
	api := NewGroupOpenAPI(bot, daemon)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/groups/open", nil)
	rec := httptest.NewRecorder()
	api.handleOpenGroup(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}
