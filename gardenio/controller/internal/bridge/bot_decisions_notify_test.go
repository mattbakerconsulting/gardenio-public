package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// --- Pure function tests ---

func TestDecisionPriorityEmoji(t *testing.T) {
	tests := []struct {
		priority int
		want     string
	}{
		{0, ":red_circle:"},
		{1, ":red_circle:"},
		{2, ":white_circle:"},
		{3, ":large_green_circle:"},
		{4, ":large_green_circle:"},
	}
	for _, tc := range tests {
		if got := decisionPriorityEmoji(tc.priority); got != tc.want {
			t.Errorf("decisionPriorityEmoji(%d) = %q, want %q", tc.priority, got, tc.want)
		}
	}
}

func TestDecisionQuestion_PromptVsLegacy(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"prompt preferred", map[string]string{"prompt": "Deploy?", "question": "Legacy?"}, "Deploy?"},
		{"question fallback", map[string]string{"question": "Legacy?"}, "Legacy?"},
		{"both empty", map[string]string{}, ""},
		{"nil fields", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionQuestion(tc.fields); got != tc.want {
				t.Errorf("decisionQuestion(%v) = %q, want %q", tc.fields, got, tc.want)
			}
		})
	}
}

func TestReportEmoji(t *testing.T) {
	tests := []struct {
		reportType string
		want       string
	}{
		{"plan", Garden.BeadType["plan"]},
		{"checklist", Garden.Semantic(SemOK)},
		{"diff-summary", Garden.Semantic(SemReview)},
		{"epic", Garden.BeadType["epic"]},
		{"bug", Garden.BeadType["bug"]},
		{"tldr", Garden.Semantic(SemNote)},
		{"unknown", Garden.Semantic(SemReport)},
		{"", Garden.Semantic(SemReport)},
	}
	for _, tc := range tests {
		if got := reportEmoji(Garden, tc.reportType); got != tc.want {
			t.Errorf("reportEmoji(%q) = %q, want %q", tc.reportType, got, tc.want)
		}
	}
	// Business is plain bracket text, never a Garden glyph.
	if got, want := reportEmoji(Business, "tldr"), "[NOTE]"; got != want {
		t.Errorf("Business reportEmoji(tldr) = %q, want %q", got, want)
	}
	if got, want := reportEmoji(Business, "plan"), "[PLAN]"; got != want {
		t.Errorf("Business reportEmoji(plan) = %q, want %q", got, want)
	}
}

// --- NotifyDecision tests ---

func TestNotifyDecision_PostsMessageWithOptions(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedChannel, postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedChannel = r.FormValue("channel")
			postedText = r.FormValue("text")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	opts, _ := json.Marshal([]string{"option-a", "option-b"})
	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-1",
		Type:     "decision",
		Title:    "Deploy question",
		Priority: 2,
		Assignee: "gardenio/crew/test-agent",
		Fields: map[string]string{
			"prompt":  "Should we deploy?",
			"options": string(opts),
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if postedChannel != "C123" {
		t.Errorf("expected channel C123, got %q", postedChannel)
	}
	if !strings.Contains(postedText, "Should we deploy?") {
		t.Errorf("expected text to contain question, got %q", postedText)
	}

	// Verify message tracking.
	ref, ok := bot.lookupMessage("dec-1")
	if !ok {
		t.Fatal("expected decision message to be tracked")
	}
	if ref.ChannelID != "C123" || ref.Timestamp != "1111.2222" {
		t.Errorf("unexpected ref: channel=%q ts=%q", ref.ChannelID, ref.Timestamp)
	}
	if ref.Agent != "test-agent" {
		t.Errorf("expected ref.Agent=test-agent, got %q", ref.Agent)
	}
}

func TestNotifyDecision_JSONObjectOptions(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	type optionObj struct {
		ID           string `json:"id"`
		Short        string `json:"short"`
		Label        string `json:"label"`
		Description  string `json:"description"`
		ArtifactType string `json:"artifact_type,omitempty"`
	}
	opts, _ := json.Marshal([]optionObj{
		{ID: "opt-1", Short: "Deploy now", Label: "Deploy immediately", Description: "Push to prod"},
		{ID: "opt-2", Short: "Wait", Label: "Wait for review"},
	})

	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-2",
		Type:     "decision",
		Title:    "Deploy timing",
		Priority: 1,
		Assignee: "test-agent",
		Fields: map[string]string{
			"prompt":  "When to deploy?",
			"options": string(opts),
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	if _, ok := bot.lookupMessage("dec-2"); !ok {
		t.Error("expected decision message to be tracked")
	}
}

func TestNotifyDecision_ThreadsUnderAgentThread(t *testing.T) {
	daemon := newMockDaemon()
	// Seed agent bead with slack_thread_channel and slack_thread_ts.
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-1",
		Type:  "agent",
		Title: "test-agent",
		Fields: map[string]string{
			"agent":                "test-agent",
			"slack_thread_channel": "C-THREAD",
			"slack_thread_ts":      "9999.0001",
		},
	}

	var mu sync.Mutex
	var postedChannel, postedThreadTS string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedChannel = r.FormValue("channel")
			postedThreadTS = r.FormValue("thread_ts")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-THREAD", "ts": "9999.0002"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-3",
		Type:     "decision",
		Assignee: "test-agent",
		Fields: map[string]string{
			"prompt":  "Thread decision?",
			"options": `["yes","no"]`,
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if postedChannel != "C-THREAD" {
		t.Errorf("expected channel C-THREAD, got %q", postedChannel)
	}
	if postedThreadTS != "9999.0001" {
		t.Errorf("expected thread_ts 9999.0001, got %q", postedThreadTS)
	}
}

func TestNotifyDecision_FallbackToRequestingAgentBeadID(t *testing.T) {
	daemon := newMockDaemon()
	// The agent in Assignee is NOT findable (no bead). But
	// requesting_agent_bead_id points to a bead with thread binding.
	daemon.beads["bd-req-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-req-agent",
		Type:  "agent",
		Title: "req-agent",
		Fields: map[string]string{
			"agent":                "req-agent",
			"slack_thread_channel": "C-REQ",
			"slack_thread_ts":      "8888.0001",
		},
	}

	var mu sync.Mutex
	var postedChannel, postedThreadTS string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedChannel = r.FormValue("channel")
			postedThreadTS = r.FormValue("thread_ts")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-REQ", "ts": "8888.0002"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-4",
		Type:     "decision",
		Assignee: "some-actor-user", // not an agent bead
		Fields: map[string]string{
			"prompt":                   "Fallback test?",
			"options":                  `["a","b"]`,
			"requesting_agent_bead_id": "bd-req-agent",
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if postedChannel != "C-REQ" {
		t.Errorf("expected fallback channel C-REQ, got %q", postedChannel)
	}
	if postedThreadTS != "8888.0001" {
		t.Errorf("expected fallback thread_ts 8888.0001, got %q", postedThreadTS)
	}
}

// Regression test: when requesting_agent_bead_id has no thread binding but
// stores a full-path agent name (e.g., "gardenio/crew/my-agent"), agent
// threading should still find the existing card keyed by short name.
func TestNotifyDecision_FallbackAgentFullPath_ThreadsUnderCard(t *testing.T) {
	daemon := newMockDaemon()
	// The requesting agent bead has a full-path agent field and NO thread binding.
	daemon.beads["bd-req-fullpath"] = &beadsapi.BeadDetail{
		ID:    "bd-req-fullpath",
		Type:  "agent",
		Title: "my-agent",
		Fields: map[string]string{
			"agent": "gardenio/crew/my-agent",
			// No slack_thread_channel / slack_thread_ts — forces agent_card fallback.
		},
	}

	var mu sync.Mutex
	var posts []struct{ channel, threadTS string }
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			posts = append(posts, struct{ channel, threadTS string }{
				r.FormValue("channel"), r.FormValue("thread_ts"),
			})
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-DEFAULT", "ts": "9999.0002"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"
	bot.threadingMode = "agent"
	// Pre-populate agent card with SHORT name key (as NotifyAgentSpawn does).
	bot.agentCards["my-agent"] = MessageRef{ChannelID: "C-DEFAULT", Timestamp: "1111.0001"}

	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-fullpath",
		Type:     "decision",
		Assignee: "some-user",
		Fields: map[string]string{
			"prompt":                   "Full path agent test?",
			"options":                  `["yes","no"]`,
			"requesting_agent_bead_id": "bd-req-fullpath",
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// The decision should be threaded under the existing agent card.
	found := false
	for _, p := range posts {
		if p.threadTS == "1111.0001" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected decision to thread under agent card ts=1111.0001, got posts: %+v", posts)
	}
}

func TestNotifyDecision_AgentThreadingModeUpdatesCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "5555.5555"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	// Pre-populate agent card so ensureAgentCard returns it.
	bot.agentCards["test-agent"] = MessageRef{ChannelID: "C123", Timestamp: "5555.5555"}

	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-5",
		Type:     "decision",
		Assignee: "test-agent",
		Fields: map[string]string{
			"prompt":  "Card update test?",
			"options": `["yes"]`,
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	// Pending count should have been incremented.
	if got := bot.agentPending["test-agent"]; got != 1 {
		t.Errorf("expected agentPending=1, got %d", got)
	}

	// Agent card should have been updated.
	mu.Lock()
	defer mu.Unlock()
	if updateCount < 1 {
		t.Error("expected agent card update (chat.update) to be called")
	}
}

func TestNotifyDecision_PredecessorChain(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dec-prev"] = &beadsapi.BeadDetail{
		ID:    "dec-prev",
		Type:  "decision",
		Title: "Previous decision",
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// Track the predecessor message so predecessor threading works.
	bot.messages["dec-prev"] = MessageRef{ChannelID: "C123", Timestamp: "1000.0001"}

	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-chained",
		Type:     "decision",
		Priority: 2,
		Fields: map[string]string{
			"prompt":         "Follow up?",
			"options":        `["yes"]`,
			"predecessor_id": "dec-prev",
			"iteration":      "2",
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	// Predecessor should have been fetched for display title.
	if daemon.getGetCalls() < 1 {
		t.Error("expected daemon.GetBead to be called for predecessor")
	}
}

func TestNotifyDecision_ContextTruncation(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// Build context > 2900 chars.
	longCtx := strings.Repeat("x", 3000)
	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-long",
		Type:     "decision",
		Priority: 2,
		Fields: map[string]string{
			"prompt":  "Long context?",
			"options": `["yes"]`,
			"context": longCtx,
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	// Just verify it completed without error — truncation is internal.
	if _, ok := bot.lookupMessage("dec-long"); !ok {
		t.Error("expected decision message to be tracked")
	}
}

// --- @mention tests ---

func TestNotifyDecision_MentionsSlackUser(t *testing.T) {
	daemon := newMockDaemon()
	// Seed agent bead with slack_user_id (set by spawn).
	daemon.beads["mention-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-mention-agent",
		Type:  "agent",
		Title: "mention-agent",
		Fields: map[string]string{
			"agent":         "mention-agent",
			"slack_user_id": "U12345HUMAN",
		},
	}

	var mu sync.Mutex
	var postedText, postedBlocks string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedText = r.FormValue("text")
			postedBlocks = r.FormValue("blocks")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	opts, _ := json.Marshal([]string{"approve", "reject"})
	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-mention",
		Type:     "decision",
		Title:    "Deploy approval",
		Priority: 1,
		Assignee: "mention-agent",
		Fields: map[string]string{
			"prompt":  "Should we deploy to prod?",
			"options": string(opts),
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Fallback text should contain the @mention.
	if !strings.Contains(postedText, "<@U12345HUMAN>") {
		t.Errorf("expected fallback text to contain <@U12345HUMAN>, got %q", postedText)
	}
	// Block Kit blocks should contain the @mention in the header.
	// Slack API JSON-escapes angle brackets as \u003c/\u003e.
	if !strings.Contains(postedBlocks, "@U12345HUMAN") {
		t.Errorf("expected blocks to contain @U12345HUMAN mention, got %q", postedBlocks)
	}
}

func TestNotifyDecision_NoMentionWhenNoSlackUser(t *testing.T) {
	daemon := newMockDaemon()
	// Agent bead exists but has no slack_user_id.
	daemon.beads["no-user-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-no-user-agent",
		Type:  "agent",
		Title: "no-user-agent",
		Fields: map[string]string{
			"agent": "no-user-agent",
		},
	}

	var mu sync.Mutex
	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedText = r.FormValue("text")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	opts, _ := json.Marshal([]string{"yes", "no"})
	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-no-mention",
		Type:     "decision",
		Priority: 2,
		Assignee: "no-user-agent",
		Fields: map[string]string{
			"prompt":  "Proceed?",
			"options": string(opts),
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Should not contain any @mention.
	if strings.Contains(postedText, "<@") {
		t.Errorf("expected no @mention in text, got %q", postedText)
	}
}

func TestNotifyEscalation_MentionsSlackUser(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["esc-mention-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-esc-mention",
		Type:  "agent",
		Title: "esc-mention-agent",
		Fields: map[string]string{
			"agent":         "esc-mention-agent",
			"slack_user_id": "U99HUMAN",
		},
	}

	var mu sync.Mutex
	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedText = r.FormValue("text")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.3333"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	err := bot.NotifyEscalation(context.Background(), BeadEvent{
		ID:       "dec-esc-mention",
		Type:     "decision",
		Title:    "Urgent item",
		Priority: 0,
		Assignee: "esc-mention-agent",
		Labels:   []string{"escalated"},
		Fields:   map[string]string{"prompt": "Handle now?"},
	})
	if err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(postedText, "<@U99HUMAN>") {
		t.Errorf("expected escalation text to contain <@U99HUMAN>, got %q", postedText)
	}
}

func TestResolveDecisionMentionUser_ReturnsEmptyForUnknownAgent(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// No agent bead exists.
	userID := bot.resolveDecisionMentionUser(context.Background(), "unknown-agent")
	if userID != "" {
		t.Errorf("expected empty user ID for unknown agent, got %q", userID)
	}

	// Empty agent name.
	userID = bot.resolveDecisionMentionUser(context.Background(), "")
	if userID != "" {
		t.Errorf("expected empty user ID for empty agent, got %q", userID)
	}
}

// --- NotifyEscalation tests ---

func TestNotifyEscalation_PostsMessage(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedText = r.FormValue("text")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.3333"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	err := bot.NotifyEscalation(context.Background(), BeadEvent{
		ID:       "dec-esc",
		Type:     "decision",
		Title:    "Urgent deploy",
		Priority: 0,
		Assignee: "test-agent",
		Labels:   []string{"escalated"},
		Fields: map[string]string{
			"prompt":       "Deploy to prod?",
			"requested_by": "boss",
		},
	})
	if err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(postedText, "ESCALATED") {
		t.Errorf("expected escalation text to contain ESCALATED, got %q", postedText)
	}
}

func TestNotifyEscalation_ThreadsUnderAgentThread(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["esc-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-esc-agent",
		Type:  "agent",
		Title: "esc-agent",
		Fields: map[string]string{
			"agent":                "esc-agent",
			"slack_thread_channel": "C-ESC",
			"slack_thread_ts":      "7777.0001",
		},
	}

	var mu sync.Mutex
	var postedChannel string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedChannel = r.FormValue("channel")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-ESC", "ts": "7777.0002"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	err := bot.NotifyEscalation(context.Background(), BeadEvent{
		ID:       "dec-esc-2",
		Type:     "decision",
		Assignee: "esc-agent",
		Fields:   map[string]string{"prompt": "Escalated?"},
	})
	if err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if postedChannel != "C-ESC" {
		t.Errorf("expected channel C-ESC, got %q", postedChannel)
	}
}

// --- DismissDecision tests ---

func TestDismissDecision_DeletesMessage(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var deletedTS string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.delete" {
			_ = r.ParseForm()
			mu.Lock()
			deletedTS = r.FormValue("ts")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.messages["dec-dismiss"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "3333.4444",
		Agent:     "test-agent",
	}

	err := bot.DismissDecision(context.Background(), "dec-dismiss")
	if err != nil {
		t.Fatalf("DismissDecision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if deletedTS != "3333.4444" {
		t.Errorf("expected deleted ts 3333.4444, got %q", deletedTS)
	}

	// Message should be removed from tracking.
	if _, ok := bot.lookupMessage("dec-dismiss"); ok {
		t.Error("expected decision message to be removed from tracking")
	}
}

func TestDismissDecision_NoopWhenNotTracked(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Dismissing an untracked decision should be a no-op.
	err := bot.DismissDecision(context.Background(), "dec-unknown")
	if err != nil {
		t.Fatalf("DismissDecision: %v", err)
	}
}

func TestDismissDecision_DecrementsPendingCount(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.threadingMode = "agent"
	bot.messages["dec-pending"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "4444.5555",
		Agent:     "test-agent",
	}
	bot.agentPending["test-agent"] = 3

	err := bot.DismissDecision(context.Background(), "dec-pending")
	if err != nil {
		t.Fatalf("DismissDecision: %v", err)
	}

	if got := bot.agentPending["test-agent"]; got != 2 {
		t.Errorf("expected agentPending=2 after dismiss, got %d", got)
	}
}

// --- UpdateDecision tests ---

func TestUpdateDecision_UpdatesMessageToResolved(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updatedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			mu.Lock()
			updatedText = r.FormValue("text")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.messages["dec-resolve"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "5555.6666",
		Agent:     "test-agent",
	}

	err := bot.UpdateDecision(context.Background(), "dec-resolve", "option-a", "Chosen by user")
	if err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(updatedText, "option-a") {
		t.Errorf("expected updated text to contain chosen option, got %q", updatedText)
	}
}

func TestUpdateDecision_NoopWhenNotTracked(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Should not error when no tracked message exists.
	err := bot.UpdateDecision(context.Background(), "dec-missing", "opt", "reason")
	if err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}
}

func TestUpdateDecision_ClearsAgentToPreventDoubleDec(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.threadingMode = "agent"
	bot.messages["dec-dbl"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "6666.7777",
		Agent:     "dbl-agent",
	}
	bot.agentPending["dbl-agent"] = 2

	_ = bot.UpdateDecision(context.Background(), "dec-dbl", "chosen", "reason")

	// Agent field should be cleared to prevent double-decrement.
	ref, ok := bot.lookupMessage("dec-dbl")
	if !ok {
		t.Fatal("expected message to still be tracked")
	}
	if ref.Agent != "" {
		t.Errorf("expected Agent field cleared, got %q", ref.Agent)
	}

	// Pending should have been decremented.
	if got := bot.agentPending["dbl-agent"]; got != 1 {
		t.Errorf("expected agentPending=1, got %d", got)
	}
}

// TestNotifyDecision_ContextRuneTruncation is the regression guard for the
// decision twin of the escalation byte-truncation bug (ka-0000000920 /
// ka-0000006181). The decision `context` field was truncated by BYTES
// (decisionCtx[:2897]); when the boundary lands mid multi-byte rune the emitted
// mrkdwn is invalid UTF-8, Slack rejects the whole message with invalid_blocks,
// and the decision card is silently lost (the operator never sees the gate).
// The fix truncates by runes via truncateRunes.
//
// Detection mirrors TestNotifyNewEscalation_ContextRuneTruncation: a mid-rune
// byte cut leaves an incomplete UTF-8 sequence that Go's json.Marshal renders
// as the replacement char U+FFFD (JSON-escaped to the literal �). Both a
// byte cut and a rune cut yield a clipped string, so the presence of U+FFFD in
// the posted blocks is the discriminating signature of the bug.
func TestNotifyDecision_ContextRuneTruncation(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedBlocks string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedBlocks = r.FormValue("blocks")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// Em-dashes are 3 bytes each; 4000 runes = 12000 bytes, so both truncation
	// schemes clip. A byte cut at 2897 lands mid-rune (2897 % 3 != 0) and
	// corrupts; a rune cut at 2900 stays clean. This isolates the defect to the
	// truncation strategy, not to whether truncation happens.
	longCtx := strings.Repeat("—", 4000)
	opts, _ := json.Marshal([]string{"option-a", "option-b"})
	err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-runes",
		Type:     "decision",
		Title:    "Rune-boundary decision",
		Priority: 2,
		Assignee: "gardenio/crew/test-agent",
		Fields: map[string]string{
			"prompt":  "Proceed?",
			"options": string(opts),
			"context": longCtx,
		},
	})
	if err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	mu.Lock()
	blocks := postedBlocks
	mu.Unlock()
	if blocks == "" {
		t.Fatal("no blocks payload captured")
	}
	// U+FFFD in the posted blocks means a multi-byte rune was split (the
	// invalid_blocks bug). slack-go JSON-marshals the payload, so it surfaces
	// either as the raw rune or the JSON-escaped ASCII literal �.
	if strings.ContainsRune(blocks, '�') || strings.Contains(strings.ToLower(blocks), `\ufffd`) {
		tail := blocks
		if len(tail) > 40 {
			tail = tail[len(tail)-40:]
		}
		t.Errorf("posted blocks contain U+FFFD replacement char — decision context was byte-truncated mid-rune: tail=%q", tail)
	}
	// Sanity: the em-dash context actually reached the payload.
	if !strings.Contains(blocks, `—`) && !strings.Contains(blocks, "—") {
		t.Errorf("expected em-dash context in posted blocks, got none")
	}
}
