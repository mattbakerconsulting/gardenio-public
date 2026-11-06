package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// groupInviteSlackServer returns a Slack test server that satisfies the calls
// the group-invite handler makes: conversations.members (returns the current
// mpim members), conversations.open (returns the superset mpim), chat.postMessage
// (hand-offs), and users.lookupByEmail. The mpim channel it returns from
// conversations.open is chosen by openChannelFor(users) so a test can make the
// re-open either "move" (new channel) or "no-op" (same channel).
func groupInviteSlackServer(t *testing.T, members []string, openChannelFor func(users string) string) (*httptest.Server, *[]slackCall) {
	t.Helper()
	var calls []slackCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.members":
			calls = append(calls, slackCall{Method: "conversations.members", Channel: r.FormValue("channel")})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":               true,
				"members":          members,
				"response_metadata": map[string]any{"next_cursor": ""},
			})
		case "/conversations.open":
			users := r.FormValue("users")
			calls = append(calls, slackCall{Method: "conversations.open", Text: users})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": openChannelFor(users)},
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

// newGroupInviteTestBot wires a Bot with a StateManager and a known botUserID so
// the superset build excludes the bot.
func newGroupInviteTestBot(t *testing.T, daemon BeadClient, srv *httptest.Server) *Bot {
	t.Helper()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "U-BOT"
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	return bot
}

// doGroupInvite POSTs a groupInviteRequest through the real handler, setting the
// {channel} path value, and returns the decoded response + status.
func doGroupInvite(t *testing.T, api *GroupInviteAPI, channel string, body map[string]any) (groupInviteResponse, int) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/slack/groups/"+channel+"/invite", bytes.NewReader(raw))
	req.SetPathValue("channel", channel)
	rec := httptest.NewRecorder()
	api.handleInviteGroup(rec, req)
	var resp groupInviteResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	return resp, rec.Code
}

// TestGroupInvite_MovesAgentBinding is the core verify test: a new invitee yields
// a NEW mpim, the bound agent migrates to it, the agent bead's binding fields are
// updated, and both conversations get a hand-off.
func TestGroupInvite_MovesAgentBinding(t *testing.T) {
	daemon := newMockDaemon()
	// Seed an agent bead already bound to the old mpim (as groups/open would).
	daemon.beads["bd-agent-1"] = &beadsapi.BeadDetail{
		ID: "bd-agent-1", Type: "agent", Title: "gardenio-crew-abc",
		Fields: map[string]string{
			"agent": "gardenio-crew-abc", "project": "gardenio", "mode": "crew",
			"slack_thread_channel": "G-OLD", "slack_spawn_channel": "G-OLD",
			"spawn_source": "group-dm",
		},
	}

	// Old members: bot + alice + bob. Invitee carol -> superset differs -> new ch.
	srv, calls := groupInviteSlackServer(t, []string{"U-BOT", "U-alice", "U-bob"},
		func(users string) string { return "G-NEW" })
	defer srv.Close()

	bot := newGroupInviteTestBot(t, daemon, srv)
	if err := bot.state.SetThreadAgent("G-OLD", "", "gardenio-crew-abc"); err != nil {
		t.Fatal(err)
	}
	api := NewGroupInviteAPI(bot, daemon)

	resp, code := doGroupInvite(t, api, "G-OLD", map[string]any{"users": []string{"U-carol"}})
	if code != http.StatusOK || !resp.OK {
		t.Fatalf("expected ok 200, got %d (%s)", code, resp.Error)
	}
	if !resp.Moved {
		t.Error("expected moved=true for a new invitee")
	}
	if resp.OldChannel != "G-OLD" || resp.Channel != "G-NEW" {
		t.Errorf("channels: want old=G-OLD new=G-NEW, got old=%q new=%q", resp.OldChannel, resp.Channel)
	}
	if resp.Agent != "gardenio-crew-abc" {
		t.Errorf("agent: want gardenio-crew-abc, got %q", resp.Agent)
	}

	// Binding migrated in state: new bound, old cleared.
	if a, ok := bot.state.GetThreadAgent("G-NEW", ""); !ok || a != "gardenio-crew-abc" {
		t.Errorf("new binding: want gardenio-crew-abc, got %q (ok=%v)", a, ok)
	}
	if _, ok := bot.state.GetThreadAgent("G-OLD", ""); ok {
		t.Error("old binding should have been removed")
	}

	// Agent bead binding fields updated to the new channel.
	f := daemon.beads["bd-agent-1"].Fields
	if f["slack_thread_channel"] != "G-NEW" || f["slack_spawn_channel"] != "G-NEW" {
		t.Errorf("bead binding fields not updated: %v", f)
	}

	// Superset re-open carried the invitee and excluded the bot.
	var sawOpen, oldHandoff, newHandoff, sawMembers bool
	for _, c := range *calls {
		switch c.Method {
		case "conversations.members":
			sawMembers = true
			if c.Channel != "G-OLD" {
				t.Errorf("conversations.members channel: want G-OLD, got %q", c.Channel)
			}
		case "conversations.open":
			sawOpen = true
			if c.Text != "U-alice,U-bob,U-carol" {
				t.Errorf("superset users: want U-alice,U-bob,U-carol (bot excluded), got %q", c.Text)
			}
		case "chat.postMessage":
			if c.Channel == "G-OLD" {
				oldHandoff = true
			}
			if c.Channel == "G-NEW" {
				newHandoff = true
			}
		}
	}
	if !sawMembers {
		t.Error("expected conversations.members read")
	}
	if !sawOpen {
		t.Error("expected conversations.open re-open")
	}
	if !oldHandoff {
		t.Error("expected hand-off posted in old mpim")
	}
	if !newHandoff {
		t.Error("expected hand-off posted in new mpim")
	}
}

// TestGroupInvite_AlreadyMember is the idempotent no-op path: the invitee is
// already a member, Slack returns the same channel, no binding move, and the old
// mpim gets no forward pointer.
func TestGroupInvite_AlreadyMember(t *testing.T) {
	daemon := newMockDaemon()
	// Same channel returned regardless of superset (invitee already present).
	srv, calls := groupInviteSlackServer(t, []string{"U-BOT", "U-alice", "U-carol"},
		func(users string) string { return "G-OLD" })
	defer srv.Close()

	bot := newGroupInviteTestBot(t, daemon, srv)
	if err := bot.state.SetThreadAgent("G-OLD", "", "gardenio-crew-xyz"); err != nil {
		t.Fatal(err)
	}
	api := NewGroupInviteAPI(bot, daemon)

	resp, code := doGroupInvite(t, api, "G-OLD", map[string]any{"users": []string{"U-carol"}})
	if code != http.StatusOK || !resp.OK {
		t.Fatalf("expected ok 200, got %d (%s)", code, resp.Error)
	}
	if resp.Moved {
		t.Error("expected moved=false for an already-present invitee")
	}
	if resp.Channel != "G-OLD" {
		t.Errorf("channel: want G-OLD, got %q", resp.Channel)
	}
	// Existing binding reported and untouched.
	if resp.Agent != "gardenio-crew-xyz" {
		t.Errorf("agent: want gardenio-crew-xyz, got %q", resp.Agent)
	}
	if a, ok := bot.state.GetThreadAgent("G-OLD", ""); !ok || a != "gardenio-crew-xyz" {
		t.Errorf("binding should be unchanged, got %q (ok=%v)", a, ok)
	}
	// No forward pointer into the old (== current) conversation: exactly one
	// hand-off post, and it is on G-OLD.
	handoffs := 0
	for _, c := range *calls {
		if c.Method == "chat.postMessage" {
			handoffs++
			if c.Channel != "G-OLD" {
				t.Errorf("unexpected hand-off channel %q", c.Channel)
			}
		}
	}
	if handoffs != 1 {
		t.Errorf("expected exactly 1 hand-off on no-op, got %d", handoffs)
	}
}

// TestGroupInvite_LegacyUnbound is the legacy fallback: no agent bound to the old
// mpim. The superset is still re-opened and hand-offs posted, but agent="" and
// moved reflects the new channel with nothing to migrate.
func TestGroupInvite_LegacyUnbound(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls := groupInviteSlackServer(t, []string{"U-BOT", "U-alice"},
		func(users string) string { return "G-NEW2" })
	defer srv.Close()

	bot := newGroupInviteTestBot(t, daemon, srv)
	api := NewGroupInviteAPI(bot, daemon)

	resp, code := doGroupInvite(t, api, "G-LEGACY", map[string]any{"users": []string{"U-dave"}})
	if code != http.StatusOK || !resp.OK {
		t.Fatalf("expected ok 200, got %d (%s)", code, resp.Error)
	}
	if !resp.Moved {
		t.Error("expected moved=true")
	}
	if resp.Agent != "" {
		t.Errorf("expected no migrated agent for an unbound mpim, got %q", resp.Agent)
	}
	// Both hand-offs still posted.
	var oldHandoff, newHandoff bool
	for _, c := range *calls {
		if c.Method == "chat.postMessage" && c.Channel == "G-LEGACY" {
			oldHandoff = true
		}
		if c.Method == "chat.postMessage" && c.Channel == "G-NEW2" {
			newHandoff = true
		}
	}
	if !oldHandoff || !newHandoff {
		t.Errorf("expected hand-offs in both conversations, old=%v new=%v", oldHandoff, newHandoff)
	}
}

// TestGroupInvite_ResolvesEmail resolves an email invitee via users.lookupByEmail
// before re-opening.
func TestGroupInvite_ResolvesEmail(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls := groupInviteSlackServer(t, []string{"U-BOT", "U-alice"},
		func(users string) string { return "G-NEW3" })
	defer srv.Close()

	bot := newGroupInviteTestBot(t, daemon, srv)
	api := NewGroupInviteAPI(bot, daemon)

	resp, code := doGroupInvite(t, api, "G-OLD", map[string]any{"users": []string{"carol@x.com"}})
	if code != http.StatusOK || !resp.OK {
		t.Fatalf("expected ok 200, got %d (%s)", code, resp.Error)
	}
	var sawLookup bool
	for _, c := range *calls {
		if c.Method == "users.lookupByEmail" && c.Text == "carol@x.com" {
			sawLookup = true
		}
		if c.Method == "conversations.open" && c.Text != "U-alice,U-from-email" {
			t.Errorf("superset users: want U-alice,U-from-email, got %q", c.Text)
		}
	}
	if !sawLookup {
		t.Error("expected users.lookupByEmail for the email invitee")
	}
}

// TestGroupInvite_NoUsers rejects a request with no invitees.
func TestGroupInvite_NoUsers(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := groupInviteSlackServer(t, []string{"U-BOT"}, func(string) string { return "G-X" })
	defer srv.Close()
	bot := newGroupInviteTestBot(t, daemon, srv)
	api := NewGroupInviteAPI(bot, daemon)

	_, code := doGroupInvite(t, api, "G-OLD", map[string]any{"users": []string{"", "  "}})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty users, got %d", code)
	}
}

// TestGroupInvite_NoChannel rejects a request with an empty channel path value.
func TestGroupInvite_NoChannel(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := groupInviteSlackServer(t, []string{"U-BOT"}, func(string) string { return "G-X" })
	defer srv.Close()
	bot := newGroupInviteTestBot(t, daemon, srv)
	api := NewGroupInviteAPI(bot, daemon)

	_, code := doGroupInvite(t, api, "", map[string]any{"users": []string{"U-carol"}})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty channel, got %d", code)
	}
}

// TestGroupInvite_MethodNotAllowed: the route pattern is POST-only; a GET must
// not reach the handler. We assert the handler itself is registered POST-only by
// exercising the mux.
func TestGroupInvite_MethodNotAllowed(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := groupInviteSlackServer(t, []string{"U-BOT"}, func(string) string { return "G-X" })
	defer srv.Close()
	bot := newGroupInviteTestBot(t, daemon, srv)
	api := NewGroupInviteAPI(bot, daemon)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/slack/groups/G-OLD/invite", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", rec.Code)
	}
}
