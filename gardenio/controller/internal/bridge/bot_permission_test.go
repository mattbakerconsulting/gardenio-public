package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

func newPermTestSlackClient(url string) *slack.Client {
	return slack.New("xoxb-test", slack.OptionAPIURL(url+"/"))
}

func permTestInteractionCallback() slack.InteractionCallback {
	return slack.InteractionCallback{
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C12345"},
			},
		},
		Message: slack.Message{
			Msg: slack.Msg{Timestamp: "1234.5678"},
		},
		User: slack.User{ID: permTestUserID, Name: "test-user"},
	}
}

// permTestUserID is the Slack user permTestInteractionCallback clicks as;
// newPermTestDaemon maps it to a person, who answers in an operator land.
const permTestUserID = "U-PERMTEST"

func newPermTestDaemon() *mockDaemon {
	m := newMockDaemon()
	m.seedUser("PermTest", map[string]string{"slack_id": permTestUserID, "email": "permtest@example.com"})
	return m
}

// seedAgentWithCoopURL registers an agent bead in the mock daemon with the
// given coop_url in its notes, where the mock daemon's stand-in mail
// subscriber hands its mail ("" for none).
func seedAgentWithCoopURL(m *mockDaemon, agentName, coopURL string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beads[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-" + agentName,
		Title: agentName,
		Type:  "agent",
		Notes: "coop_url: " + coopURL,
	}
}

func TestPermissionCallbackHandler_PostMethod(t *testing.T) {
	h := &PermissionCallbackHandler{logger: testLogger()}
	srv := httptest.NewServer(http.HandlerFunc(h.handleCallback))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", resp.StatusCode)
	}
}

func TestPermissionCallbackHandler_InvalidJSON(t *testing.T) {
	h := &PermissionCallbackHandler{logger: testLogger()}
	srv := httptest.NewServer(http.HandlerFunc(h.handleCallback))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}
}

func TestPermissionCallbackHandler_MissingFields(t *testing.T) {
	h := &PermissionCallbackHandler{logger: testLogger()}
	srv := httptest.NewServer(http.HandlerFunc(h.handleCallback))
	defer srv.Close()

	// Missing tool_name.
	body, _ := json.Marshal(channelPermissionCallback{RequestID: "abc"})
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing tool_name, got %d", resp.StatusCode)
	}
}

func TestPermissionCallbackHandler_ValidRequest(t *testing.T) {
	h := &PermissionCallbackHandler{logger: testLogger()}
	srv := httptest.NewServer(http.HandlerFunc(h.handleCallback))
	defer srv.Close()

	body, _ := json.Marshal(channelPermissionCallback{
		RequestID:    "abcde",
		ToolName:     "Bash",
		Description:  "Run a shell command",
		InputPreview: "ls -la",
		Agent:        "test-agent",
		ChannelURL:   "http://10.0.0.5:8789",
	})
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Received bool `json:"received"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Received {
		t.Error("expected received=true")
	}
}

// assertVerdictMail checks the verdict went to agent as control:permission
// mail carrying the request and the behavior: nothing dials the agent, and its
// subscriber hands the fields to the channel server.
func assertVerdictMail(t *testing.T, daemon *mockDaemon, agent, requestID, behavior string) {
	t.Helper()
	req := daemon.lastCreateBead()
	if req.Type != "mail" || req.Assignee != agent {
		t.Fatalf("expected verdict mail to %s, got type=%q assignee=%q", agent, req.Type, req.Assignee)
	}
	if !hasLabel(req.Labels, "control:permission") || !hasLabel(req.Labels, "from:"+NudgeSender) {
		t.Errorf("verdict mail must be control:permission from %s, labels %v", NudgeSender, req.Labels)
	}
	var fields map[string]string
	if err := json.Unmarshal(req.Fields, &fields); err != nil || fields["request_id"] != requestID || fields["behavior"] != behavior {
		t.Errorf("verdict mail fields %s, want request_id=%s behavior=%s", req.Fields, requestID, behavior)
	}
}

// A verdict for an agent that is stopping is not mailed: no session would
// answer the prompt, and the Slack card says delivery failed.
func TestMailPermissionVerdict_RefusesStoppingAgent(t *testing.T) {
	daemon := newPermTestDaemon()
	daemon.beads["gone"] = &beadsapi.BeadDetail{ID: "bd-gone", Title: "gone", Type: "agent",
		Fields: map[string]string{"agent_state": "working", "stop_requested": "true"}}
	if _, err := MailPermissionVerdict(context.Background(), daemon, testLogger(), "gone", "req", "allow"); !errors.Is(err, ErrAgentNotNudgeable) {
		t.Fatalf("want ErrAgentNotNudgeable, got %v", err)
	}
	if n := len(daemon.mails); n != 0 {
		t.Fatalf("no mail should be sent, got %d", n)
	}
}

func TestResolvePermission_Allow(t *testing.T) {
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer fakeSlack.Close()

	daemon := newPermTestDaemon()
	seedAgentWithCoopURL(daemon, "my-agent", "")

	bot := &Bot{
		api:    newPermTestSlackClient(fakeSlack.URL),
		daemon: daemon,
		logger: testLogger(),
	}

	bot.resolvePermission(context.Background(), "test-req|my-agent", "allow", permTestInteractionCallback())
	assertVerdictMail(t, daemon, "my-agent", "test-req", "allow")
}

func TestResolvePermission_Deny(t *testing.T) {
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer fakeSlack.Close()

	daemon := newPermTestDaemon()
	seedAgentWithCoopURL(daemon, "my-agent", "")

	bot := &Bot{
		api:    newPermTestSlackClient(fakeSlack.URL),
		daemon: daemon,
		logger: testLogger(),
	}

	bot.resolvePermission(context.Background(), "deny-req|my-agent", "deny", permTestInteractionCallback())
	assertVerdictMail(t, daemon, "my-agent", "deny-req", "deny")
}

func TestResolvePermission_InvalidValue(t *testing.T) {
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer fakeSlack.Close()

	bot := &Bot{
		api:    newPermTestSlackClient(fakeSlack.URL),
		daemon: newMockDaemon(),
		logger: testLogger(),
	}

	// Should not panic on malformed value.
	bot.resolvePermission(context.Background(), "no-pipe-separator", "allow", permTestInteractionCallback())
}

// TestResolvePermission_AgentWithoutCoopURL verifies an agent bead with no
// coop_url still gets its verdict (as mail), and that nothing is dialed.
func TestResolvePermission_AgentWithoutCoopURL(t *testing.T) {
	// Observer: any unexpected outbound POST to this server would be a bug,
	// but without a configured URL the verdict should simply not fire.
	var hits int
	var mu sync.Mutex
	unexpected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer unexpected.Close()

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer fakeSlack.Close()

	daemon := newPermTestDaemon()
	// Agent exists but has no coop_url in notes.
	daemon.beads["ghost-agent"] = &beadsapi.BeadDetail{
		ID: "bd-ghost", Title: "ghost-agent", Type: "agent", Notes: "",
	}

	bot := &Bot{
		api:    newPermTestSlackClient(fakeSlack.URL),
		daemon: daemon,
		logger: testLogger(),
	}

	bot.resolvePermission(context.Background(), "req|ghost-agent", "allow", permTestInteractionCallback())

	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("expected zero outbound verdict POSTs, got %d", hits)
	}
	assertVerdictMail(t, daemon, "ghost-agent", "req", "allow")
}

// TestPermissionCallback_IgnoresPayloadChannelURL (AUDIT §4.7):
// The bridge MUST ignore the attacker-supplied channel_url in the callback
// payload: the verdict is mail to the agent bead's name. The mock daemon
// plays the agent's subscriber, handing the mail to the bead's coop_url.
func TestPermissionCallback_IgnoresPayloadChannelURL(t *testing.T) {
	var authoritativeHits int
	var mu sync.Mutex
	authoritativeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authoritativeHits++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
	}))
	defer authoritativeSrv.Close()

	attackerHit := make(chan struct{}, 1)
	attackerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case attackerHit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attackerSrv.Close()

	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer fakeSlack.Close()

	daemon := newPermTestDaemon()
	seedAgentWithCoopURL(daemon, "a1", authoritativeSrv.URL)

	bot := &Bot{
		api:     newPermTestSlackClient(fakeSlack.URL),
		daemon:  daemon,
		channel: "C-default",
		logger:  testLogger(),
	}

	// Simulate operator clicking Allow after the callback. The attacker-
	// controlled channel_url has already been discarded at callback time;
	// this step just verifies resolvePermission uses the bead lookup.
	bot.resolvePermission(context.Background(), "X|a1", "allow", permTestInteractionCallback())

	mu.Lock()
	hits := authoritativeHits
	mu.Unlock()
	if hits != 1 {
		t.Fatalf("expected verdict delivered to authoritative coop URL once, got %d hits", hits)
	}
	select {
	case <-attackerHit:
		t.Fatal("attacker-controlled URL received verdict — SSRF not mitigated")
	default:
	}
}

// TestPermissionCallback_SanitizesMrkdwnInPreview (AUDIT §N9): a crafted
// InputPreview containing triple-backticks must not escape the code block
// and inject mrkdwn directives (<!channel>, clickable links, etc.).
func TestPermissionCallback_SanitizesMrkdwnInPreview(t *testing.T) {
	var postedBlocks string
	var mu sync.Mutex
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		if b := r.Form.Get("blocks"); b != "" {
			postedBlocks = b
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true, "message_ts":"1"}`))
	}))
	defer fakeSlack.Close()

	bot := &Bot{
		api:     newPermTestSlackClient(fakeSlack.URL),
		daemon:  newMockDaemon(),
		channel: "C-default",
		logger:  testLogger(),
	}
	h := &PermissionCallbackHandler{bot: bot, logger: testLogger()}

	cb := channelPermissionCallback{
		Type:         "permission_request",
		RequestID:    "PWN02",
		ToolName:     "Bash",
		Agent:        "victim",
		InputPreview: "ls\n```\n<!channel> click <https://evil.invalid|here>\n```\n",
	}
	h.postPermissionPrompt(context.Background(), cb)

	mu.Lock()
	defer mu.Unlock()
	if postedBlocks == "" {
		t.Fatal("expected blocks to be posted to Slack")
	}
	// The wrapped preview section has the form ```<sanitized>```. Isolate the
	// <sanitized> substring so we only inspect attacker-controlled content.
	openFence := "```ls"
	openIdx := strings.Index(postedBlocks, openFence)
	if openIdx < 0 {
		t.Fatalf("preview code fence missing from payload: %s", postedBlocks)
	}
	after := postedBlocks[openIdx+3:]
	closeIdx := strings.Index(after, "```")
	if closeIdx < 0 {
		t.Fatalf("closing code fence missing from payload: %s", postedBlocks)
	}
	inner := after[:closeIdx]
	// Property: no run of ≥2 raw backticks can survive inside the fence —
	// otherwise mrkdwn would parse it as a fence terminator and render the
	// remainder (including the attacker's <!channel>) as mrkdwn.
	if strings.Contains(inner, "``") {
		t.Errorf("raw backtick run survived sanitization inside preview: %q", inner)
	}
	if !strings.Contains(inner, "`\u200b") {
		t.Errorf("expected zero-width-space after each backtick in preview, got: %q", inner)
	}
}

func TestSanitizeCodeFence(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"no backticks", "hello world", "hello world"},
		{"single backtick", "a`b", "a`\u200bb"},
		{"triple backticks", "```", "`\u200b`\u200b`\u200b"},
		{"fence breakout attempt", "x```\n<!channel>", "x`\u200b`\u200b`\u200b\n<!channel>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCodeFence(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeCodeFence(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Property: no run of ≥2 consecutive backticks remains.
			if strings.Contains(got, "``") {
				t.Errorf("sanitized output still contains a backtick run: %q", got)
			}
		})
	}
}

// TestPermissionCallback_ButtonValueNoURL (AUDIT §N3): the button value
// encoded into the posted Slack prompt must NOT contain the attacker-
// supplied channel URL. Only requestID + agent name are stored.
func TestPermissionCallback_ButtonValueNoURL(t *testing.T) {
	var postedBlocks string
	var mu sync.Mutex
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		if b := r.Form.Get("blocks"); b != "" {
			postedBlocks = b
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true, "message_ts":"1"}`))
	}))
	defer fakeSlack.Close()

	daemon := newMockDaemon()

	bot := &Bot{
		api:     newPermTestSlackClient(fakeSlack.URL),
		daemon:  daemon,
		channel: "C-default",
		logger:  testLogger(),
	}
	h := &PermissionCallbackHandler{bot: bot, logger: testLogger()}

	cb := channelPermissionCallback{
		Type:       "permission_request",
		RequestID:  "PWN01",
		ToolName:   "Bash",
		Agent:      "victim",
		ChannelURL: "http://attacker.invalid/evil",
	}
	h.postPermissionPrompt(context.Background(), cb)

	mu.Lock()
	defer mu.Unlock()
	if postedBlocks == "" {
		t.Fatal("expected blocks to be posted to Slack")
	}
	if strings.Contains(postedBlocks, "attacker.invalid") {
		t.Errorf("attacker URL leaked into Slack button value: %s", postedBlocks)
	}
	if !strings.Contains(postedBlocks, "PWN01|victim") {
		t.Errorf("expected button value 'requestID|agent' format, got: %s", postedBlocks)
	}
}
