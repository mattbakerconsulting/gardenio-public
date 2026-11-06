package bridge

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
)

// newManageTestAPI wires a SlackChannelAPI to a mock Slack server.
func newManageTestAPI(handler http.HandlerFunc) (*SlackChannelAPI, *httptest.Server) {
	slackSrv := httptest.NewServer(handler)
	api := slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/"))
	return NewSlackChannelAPI(api, slog.Default()), slackSrv
}

func TestCreateChannel_Public(t *testing.T) {
	var mu sync.Mutex
	var gotName, gotPrivate string

	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			mu.Lock()
			gotName = r.FormValue("name")
			gotPrivate = r.FormValue("is_private")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "C999", "name": gotName, "is_private": false},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	body := `{"name":"Proj Falcon","private":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/create", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp createChannelResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.ID != "C999" {
		t.Errorf("resp = %+v, want ok + id C999", resp)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotName != "proj-falcon" {
		t.Errorf("Slack got name %q, want normalized proj-falcon", gotName)
	}
	if gotPrivate != "false" {
		t.Errorf("is_private = %q, want false", gotPrivate)
	}
}

func TestCreateChannel_PrivateWithInviteByEmail(t *testing.T) {
	var mu sync.Mutex
	var invitedUsers, isPrivate string

	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			mu.Lock()
			isPrivate = r.FormValue("is_private")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "G555", "name": "secret", "is_private": true},
			})
		case "/users.lookupByEmail":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"id": "U777"},
			})
		case "/conversations.invite":
			mu.Lock()
			invitedUsers = r.FormValue("users")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "G555"},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	body := `{"name":"secret","private":true,"invite":["alice@example.com","U888"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/create", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp createChannelResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK || !resp.Private {
		t.Errorf("resp = %+v, want ok + private", resp)
	}
	if len(resp.Invited) != 2 {
		t.Errorf("invited = %v, want 2 users", resp.Invited)
	}

	mu.Lock()
	defer mu.Unlock()
	if isPrivate != "true" {
		t.Errorf("is_private = %q, want true", isPrivate)
	}
	// alice resolved to U777, U888 passed through.
	if !strings.Contains(invitedUsers, "U777") || !strings.Contains(invitedUsers, "U888") {
		t.Errorf("invited users = %q, want U777 and U888", invitedUsers)
	}
}

func TestArchiveChannel(t *testing.T) {
	var mu sync.Mutex
	var archivedChannel string

	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.archive" {
			mu.Lock()
			archivedChannel = r.FormValue("channel")
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/archive",
		strings.NewReader(`{"channel":"C123"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp channelActionResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK || resp.Channel != "C123" {
		t.Errorf("resp = %+v, want ok + C123", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if archivedChannel != "C123" {
		t.Errorf("Slack archived %q, want C123", archivedChannel)
	}
}

func TestArchiveChannel_MissingChannel(t *testing.T) {
	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/archive",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestArchiveChannel_SlackError asserts that a Slack-side failure
// (conversations.archive → ok:false) surfaces as a 500 with the Slack error
// echoed in the response, rather than a silent success.
func TestArchiveChannel_SlackError(t *testing.T) {
	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.archive" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "already_archived"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/archive",
		strings.NewReader(`{"channel":"C123"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	var resp channelActionResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false on Slack error")
	}
	if !strings.Contains(resp.Error, "already_archived") {
		t.Errorf("error = %q, want it to contain 'already_archived'", resp.Error)
	}
}

// TestUnarchiveChannel covers the reversible pair of archive: a happy-path
// unarchive forwards the channel to conversations.unarchive and returns ok.
func TestUnarchiveChannel(t *testing.T) {
	var mu sync.Mutex
	var unarchivedChannel string

	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.unarchive" {
			mu.Lock()
			unarchivedChannel = r.FormValue("channel")
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/unarchive",
		strings.NewReader(`{"channel":"C123"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp channelActionResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK || resp.Channel != "C123" {
		t.Errorf("resp = %+v, want ok + C123", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if unarchivedChannel != "C123" {
		t.Errorf("Slack unarchived %q, want C123", unarchivedChannel)
	}
}

// TestUnarchiveChannel_MissingChannel asserts a missing channel is a 400
// before any Slack call.
func TestUnarchiveChannel_MissingChannel(t *testing.T) {
	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/unarchive",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestInviteToChannel_SlackError asserts that when conversations.invite fails
// for every resolved user (so no user is invited), the handler surfaces a 500
// with the Slack error rather than a false ok.
func TestInviteToChannel_SlackError(t *testing.T) {
	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.invite" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not_in_channel"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/invite",
		strings.NewReader(`{"channel":"C123","users":["U111"]}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	var resp inviteResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false when invite fails for all users")
	}
	if !strings.Contains(resp.Error, "not_in_channel") {
		t.Errorf("error = %q, want it to contain 'not_in_channel'", resp.Error)
	}
}

// TestCreateChannel_SlackError asserts a create failure (conversations.create
// → ok:false) is surfaced as a 500 in the create response shape.
func TestCreateChannel_SlackError(t *testing.T) {
	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.create" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "name_taken"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/create",
		strings.NewReader(`{"name":"dupe"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	var resp createChannelResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false on create failure")
	}
	if !strings.Contains(resp.Error, "name_taken") {
		t.Errorf("error = %q, want it to contain 'name_taken'", resp.Error)
	}
}

// TestCreateChannel_MissingName asserts an empty/whitespace-only name is a 400
// before any Slack call.
func TestCreateChannel_MissingName(t *testing.T) {
	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	// "!!!" normalizes to the empty string (all chars stripped) → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/create",
		strings.NewReader(`{"name":"!!!"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestCreateChannel_InviteWarning asserts that when the channel is created but
// a follow-up invite fails, the create still succeeds (200/ok) and the failure
// is reported non-fatally via the Warning field — matching the handler's
// "warnings don't fail the create" contract.
func TestCreateChannel_InviteWarning(t *testing.T) {
	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "C321", "name": "proj", "is_private": false},
			})
		case "/conversations.invite":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/create",
		strings.NewReader(`{"name":"proj","invite":["U999"]}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (create succeeds despite invite failure); body=%s", w.Code, w.Body.String())
	}
	var resp createChannelResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK || resp.ID != "C321" {
		t.Errorf("resp = %+v, want ok + id C321", resp)
	}
	if !strings.Contains(resp.Warning, "invite") || !strings.Contains(resp.Warning, "user_not_found") {
		t.Errorf("warning = %q, want it to mention the invite failure", resp.Warning)
	}
}

func TestInviteToChannel(t *testing.T) {
	var mu sync.Mutex
	var gotChannel, gotUsers string

	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.invite" {
			mu.Lock()
			gotChannel = r.FormValue("channel")
			gotUsers = r.FormValue("users")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "channel": map[string]any{"id": "C123"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/invite",
		strings.NewReader(`{"channel":"C123","users":["U111","U222"]}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp inviteResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK || len(resp.Invited) != 2 {
		t.Errorf("resp = %+v, want ok + 2 invited", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotChannel != "C123" {
		t.Errorf("channel = %q, want C123", gotChannel)
	}
	if !strings.Contains(gotUsers, "U111") || !strings.Contains(gotUsers, "U222") {
		t.Errorf("users = %q, want U111 and U222", gotUsers)
	}
}

func TestInviteToChannel_MissingArgs(t *testing.T) {
	channelAPI, slackSrv := newManageTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterManageRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/channels/invite",
		strings.NewReader(`{"channel":"C123"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestNormalizeChannelName(t *testing.T) {
	cases := map[string]string{
		"#Proj Falcon":  "proj-falcon",
		"Incident.1234": "incident-1234",
		"already-good":  "already-good",
		"UPPER_Case":    "upper_case",
		"weird!@#chars": "weirdchars",
	}
	for in, want := range cases {
		if got := normalizeChannelName(in); got != want {
			t.Errorf("normalizeChannelName(%q) = %q, want %q", in, got, want)
		}
	}
}
