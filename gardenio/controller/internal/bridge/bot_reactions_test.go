package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// --- helpers ----------------------------------------------------------------

func makeReactionEvent(user, emoji, channel, ts string) *slackevents.ReactionAddedEvent {
	return &slackevents.ReactionAddedEvent{
		Type:     "reaction_added",
		User:     user,
		Reaction: emoji,
		Item: slackevents.Item{
			Type:      "message",
			Channel:   channel,
			Timestamp: ts,
		},
	}
}

func makeReactionRule(id, emoji, match, project, role, prompt string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"emoji":   emoji,
		"match":   match,
		"project": project,
		"role":    role,
		"prompt":  prompt,
	}
	var labels []string
	if project != "" {
		labels = []string{"project:" + project}
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "reaction-rule",
		Title:  "Test rule: " + emoji,
		Status: "open",
		Labels: labels,
		Fields: fields,
	}
}

// newFakeSlackServerWithHistory returns a test server that responds to
// conversations.replies with the given message text.
func newFakeSlackServerWithHistory(t *testing.T, msgText string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "conversations.replies") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{"text": msgText, "ts": "1234567890.123456"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
}

// --- unit tests for pure functions ------------------------------------------

func TestMatchReactionRulesByEmoji(t *testing.T) {
	rules := []*beadsapi.BeadDetail{
		makeReactionRule("r1", "runner", "", "monorepo", "crew", "re-run tests"),
		makeReactionRule("r2", "eyes", "", "monorepo", "crew", "review"),
		makeReactionRule("r3", "runner", "e2e", "gardenio", "crew", "e2e specific"),
	}

	matched := matchReactionRulesByEmoji("runner", rules)
	if len(matched) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matched))
	}
	if matched[0].ID != "r1" || matched[1].ID != "r3" {
		t.Errorf("unexpected match IDs: %s, %s", matched[0].ID, matched[1].ID)
	}

	matched = matchReactionRulesByEmoji("eyes", rules)
	if len(matched) != 1 || matched[0].ID != "r2" {
		t.Fatalf("expected 1 match for eyes, got %d", len(matched))
	}

	matched = matchReactionRulesByEmoji("thumbsup", rules)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches for thumbsup, got %d", len(matched))
	}
}

func TestInterpolatePrompt(t *testing.T) {
	tmpl := "Re-run tests for MR !{mr_iid} on branch {source_branch}. User: {user}"
	vars := map[string]string{
		"mr_iid":        "456",
		"source_branch": "fix/login",
		"user":          "U12345",
	}
	got := interpolatePrompt(tmpl, vars)
	want := "Re-run tests for MR !456 on branch fix/login. User: U12345"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestInterpolatePrompt_MissingVars(t *testing.T) {
	tmpl := "Run {action} on {target}"
	vars := map[string]string{"action": "tests"}
	got := interpolatePrompt(tmpl, vars)
	want := "Run tests on {target}" // unresolved placeholders stay
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMRIIDPattern(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"MR !456 is ready", "456"},
		{"See #123 for details", "123"},
		{"!99 is ready", "99"},
		{"no MR here", ""},
		{"!5 too short", ""}, // single digit, < 2
	}
	for _, tt := range tests {
		m := mrIIDPattern.FindStringSubmatch(tt.text)
		got := ""
		if len(m) > 1 {
			got = m[1]
		}
		if got != tt.want {
			t.Errorf("mrIIDPattern(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestBranchPattern(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"branch: fix/login-bug", "fix/login-bug"},
		{"source_branch: agent/bot/fix", "agent/bot/fix"},
		{"Branch: main", "main"},
		{"no branch here", ""},
	}
	for _, tt := range tests {
		m := branchPattern.FindStringSubmatch(tt.text)
		got := ""
		if len(m) > 1 {
			got = m[1]
		}
		if got != tt.want {
			t.Errorf("branchPattern(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestReactionSpawnThrottle(t *testing.T) {
	throttle := newReactionSpawnThrottle()
	throttle.cooldown = 50 * time.Millisecond // short for testing

	if !throttle.Allow("key1") {
		t.Error("first call should be allowed")
	}
	if throttle.Allow("key1") {
		t.Error("second call within cooldown should be throttled")
	}
	if !throttle.Allow("key2") {
		t.Error("different key should be allowed")
	}
}

func TestReactionSpawnThrottle_Cleanup(t *testing.T) {
	throttle := newReactionSpawnThrottle()
	throttle.cooldown = 1 * time.Millisecond

	throttle.Allow("key1")
	time.Sleep(5 * time.Millisecond)
	throttle.Cleanup()

	throttle.mu.Lock()
	count := len(throttle.seen)
	throttle.mu.Unlock()
	if count != 0 {
		t.Errorf("expected 0 entries after cleanup, got %d", count)
	}
}

// --- integration tests with mock bot ----------------------------------------

func TestHandleReactionAdded_MatchingRule_SpawnsAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("monorepo")
	srv := newFakeSlackServerWithHistory(t, "E2E test results: e2e-test-summary MR !789 branch: agent/fix/thing")
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "e2e-test-summary", "monorepo", "crew",
				"Re-run e2e tests. MR: {mr_iid}"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := makeReactionEvent("U_USER", "runner", "C123", "1234567890.123456")
	bot.handleReactionAdded(context.Background(), ev)

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(agentBeads))
	}
	if agentBeads[0].Fields["project"] != "monorepo" {
		t.Errorf("expected project monorepo, got %s", agentBeads[0].Fields["project"])
	}
	if agentBeads[0].Fields["role"] != "crew" {
		t.Errorf("expected role crew, got %s", agentBeads[0].Fields["role"])
	}
	prompt := agentBeads[0].Fields["prompt"]
	if !strings.Contains(prompt, "789") {
		t.Errorf("expected MR IID 789 in prompt, got %q", prompt)
	}
}

func TestHandleReactionAdded_NoMatchingEmoji_NoSpawn(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "", "monorepo", "crew", "do something"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := makeReactionEvent("U_USER", "thumbsup", "C123", "1234567890.123456")
	bot.handleReactionAdded(context.Background(), ev)

	if len(filterAgentBeads(daemon.beads)) != 0 {
		t.Fatal("no agent should be spawned for non-matching emoji")
	}
}

func TestHandleReactionAdded_MessageDoesNotMatch_NoSpawn(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServerWithHistory(t, "Some random message about deployment")
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "e2e-test-summary", "monorepo", "crew", "run tests"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := makeReactionEvent("U_USER", "runner", "C123", "1234567890.123456")
	bot.handleReactionAdded(context.Background(), ev)

	if len(filterAgentBeads(daemon.beads)) != 0 {
		t.Fatal("no agent should be spawned when message doesn't match pattern")
	}
}

func TestHandleReactionAdded_BotOwnReaction_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "", "monorepo", "crew", "run tests"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := makeReactionEvent("BOT", "runner", "C123", "1234567890.123456")
	bot.handleReactionAdded(context.Background(), ev)

	if len(filterAgentBeads(daemon.beads)) != 0 {
		t.Fatal("bot's own reactions should be ignored")
	}
}

func TestHandleReactionAdded_Throttled_SecondReaction(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("monorepo")
	srv := newFakeSlackServerWithHistory(t, "some message")
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "", "monorepo", "crew", "run tests"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := makeReactionEvent("U_USER", "runner", "C123", "1234567890.123456")

	bot.handleReactionAdded(context.Background(), ev)
	if len(filterAgentBeads(daemon.beads)) != 1 {
		t.Fatalf("expected 1 spawned agent after first reaction, got %d", len(filterAgentBeads(daemon.beads)))
	}

	bot.handleReactionAdded(context.Background(), ev)
	if len(filterAgentBeads(daemon.beads)) != 1 {
		t.Fatalf("expected still 1 spawned agent after throttled reaction, got %d", len(filterAgentBeads(daemon.beads)))
	}
}

func TestHandleReactionAdded_NonMessageItem_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "", "monorepo", "crew", "run tests"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := &slackevents.ReactionAddedEvent{
		User:     "U_USER",
		Reaction: "runner",
		Item: slackevents.Item{
			Type:    "file",
			Channel: "C123",
		},
	}
	bot.handleReactionAdded(context.Background(), ev)

	if len(filterAgentBeads(daemon.beads)) != 0 {
		t.Fatal("reactions on non-message items should be ignored")
	}
}

func TestHandleReactionAdded_NoRules_NoAction(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	bot.reactionRuleCache = &reactionRuleCache{
		rules:     []*beadsapi.BeadDetail{},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := makeReactionEvent("U_USER", "runner", "C123", "1234567890.123456")
	bot.handleReactionAdded(context.Background(), ev)

	if len(filterAgentBeads(daemon.beads)) != 0 {
		t.Fatal("no agent should be spawned when there are no rules")
	}
}

func TestHandleReactionAdded_NoMatchPattern_SpawnsOnAnyMessage(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("monorepo")
	srv := newFakeSlackServerWithHistory(t, "any random message here")
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "", "monorepo", "crew", "run something"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	ev := makeReactionEvent("U_USER", "runner", "C123", "1234567890.123456")
	bot.handleReactionAdded(context.Background(), ev)

	if len(filterAgentBeads(daemon.beads)) != 1 {
		t.Fatalf("expected 1 spawned agent for rule with no match pattern, got %d", len(filterAgentBeads(daemon.beads)))
	}
}

// The bridge spawns for a rule only in the project its label names, and
// never in a tenant land: a tenant agent that wrote a rule in its own land and
// repointed fields.project must not get a fleet spawn (gd-bRLiEzvMsy).
func TestHandleReactionAdded_RuleProjectFence(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		land   string // land_kind of the "monorepo" project bead; "" seeds none
		spawn  bool
	}{
		{"label matches", []string{"project:monorepo"}, "", true},
		{"label matches, operator land", []string{"project:monorepo"}, "operator", true},
		{"label names another project", []string{"project:tomato-patch"}, "", false},
		{"one of two labels differs", []string{"project:monorepo", "project:tomato-patch"}, "", false},
		{"no project label", nil, "", false},
		{"tenant land", []string{"project:monorepo"}, "tenant", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			daemon.seedProject("monorepo")
			if tc.land != "" {
				seedLand(daemon, "monorepo", tc.land)
				daemon.mu.Lock()
				delete(daemon.beads, "proj-monorepo")
				daemon.mu.Unlock()
			}
			srv := newFakeSlackServerWithHistory(t, "anything")
			defer srv.Close()
			bot := newTestBot(daemon, srv)
			bot.botUserID = "BOT"
			rule := makeReactionRule("rule-1", "eyes", "", "monorepo", "crew", "p")
			rule.Labels = tc.labels
			bot.reactionRuleCache = &reactionRuleCache{
				rules:     []*beadsapi.BeadDetail{rule},
				fetchedAt: time.Now(),
				ttl:       5 * time.Minute,
			}
			bot.handleReactionAdded(context.Background(), makeReactionEvent("U_USER", "eyes", "C123", "1234567890.123456"))
			if got := len(filterAgentBeads(daemon.beads)) == 1; got != tc.spawn {
				t.Errorf("spawned = %v, want %v", got, tc.spawn)
			}
		})
	}
}
