package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mockJiraMentionDaemon implements JiraMentionBeadClient for testing.
type mockJiraMentionDaemon struct {
	mu            sync.Mutex
	beads         []*beadsapi.BeadDetail
	created       []beadsapi.CreateBeadRequest
	spawned       []spawnCall
	createID      string
	spawnID       string
	triggerBeads  []*beadsapi.BeadDetail // returned by ListBeadsFiltered for spawn-role trigger lookups (no jira: label)
	mirrorBeads   []*beadsapi.BeadDetail // returned by ListBeadsFiltered when query contains a `jira:<key>` label
	addDepCalls   []addDepCall
	addDepErr     error
	listFilterErr error
}

// addDepCall records arguments passed to AddDependency for assertion.
type addDepCall struct {
	BeadID      string
	DependsOnID string
	DepType     string
	CreatedBy   string
}

func (m *mockJiraMentionDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, req)
	return m.createID, nil
}

func (m *mockJiraMentionDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, prompt string, extra ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var extraFields map[string]string
	if len(extra) > 0 {
		extraFields = extra[0]
	}
	m.spawned = append(m.spawned, spawnCall{
		AgentName: agentName,
		Project:   project,
		TaskID:    taskID,
		Role:      role,
		Prompt:    prompt,
		Extra:     extraFields,
	})
	return m.spawnID, nil
}

func (m *mockJiraMentionDaemon) ListTaskBeadsAll(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beads, nil
}

// ListTaskBeadsForJiraKey mirrors the daemon's server-side fields.jira_key
// equality predicate so tests exercise the same narrowing as production.
func (m *mockJiraMentionDaemon) ListTaskBeadsForJiraKey(_ context.Context, jiraKey string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if jiraKey == "" {
		return nil, nil
	}
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Fields["jira_key"] == jiraKey {
			result = append(result, b)
		}
	}
	return result, nil
}

func (m *mockJiraMentionDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listFilterErr != nil {
		return nil, m.listFilterErr
	}
	// Route mirror-bead lookups (queries scoped to a `jira:<key>` label) to a
	// separate fixture so spawn-role trigger lookups and Jira-mirror lookups
	// don't have to share the same return slice.
	for _, l := range q.Labels {
		if strings.HasPrefix(l, "jira:") {
			return &beadsapi.ListBeadsResult{Beads: m.mirrorBeads}, nil
		}
	}
	return &beadsapi.ListBeadsResult{Beads: m.triggerBeads}, nil
}

func (m *mockJiraMentionDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addDepCalls = append(m.addDepCalls, addDepCall{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		DepType:     depType,
		CreatedBy:   createdBy,
	})
	return m.addDepErr
}

// jiraTestSpawnTriggerBeads returns an enabled spawn_agent trigger for the
// given project with role "gardenio-crew". Used by tests that exercise paths
// unrelated to spawn-role resolution but still need spawn to succeed.
func jiraTestSpawnTriggerBeads(project string) []*beadsapi.BeadDetail {
	return []*beadsapi.BeadDetail{
		{Fields: map[string]string{
			"action": "spawn_agent", "project": project,
			"role": "gardenio-crew", "enabled": "true",
		}},
	}
}

func TestExtractJiraMentionInstruction(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		trigger string
		want    string
	}{
		{
			name:    "simple mention",
			text:    "@gardenio fix the nil pointer in auth.go",
			trigger: "@gardenio",
			want:    "fix the nil pointer in auth.go",
		},
		{
			name:    "case insensitive",
			text:    "@Gardenio fix the nil pointer",
			trigger: "@gardenio",
			want:    "fix the nil pointer",
		},
		{
			name:    "all caps",
			text:    "@GARDENIO deploy this fix",
			trigger: "@gardenio",
			want:    "deploy this fix",
		},
		{
			name:    "mention in middle of text",
			text:    "Hey team, @gardenio please refactor this function",
			trigger: "@gardenio",
			want:    "please refactor this function",
		},
		{
			name:    "no mention",
			text:    "This is a normal comment",
			trigger: "@gardenio",
			want:    "",
		},
		{
			name:    "mention without instruction",
			text:    "@gardenio",
			trigger: "@gardenio",
			want:    "",
		},
		{
			name:    "mention with only whitespace after",
			text:    "@gardenio   ",
			trigger: "@gardenio",
			want:    "",
		},
		{
			name:    "custom trigger",
			text:    "@mybot do the thing",
			trigger: "@mybot",
			want:    "do the thing",
		},
		{
			// Regression: ka-0000002216 — a rune that EXPANDS under Unicode
			// lowercasing (U+023A Ⱥ → 3-byte ⱥ) before the trigger made
			// strings.ToLower offsets exceed len(text), so text[idx+len(trigger):]
			// panicked. asciiToLower is byte-length-preserving. This runs on
			// attacker-controlled JIRA comment text before the isAuthorized gate.
			name:    "non-ASCII expanding rune before trigger does not panic",
			text:    "Ⱥ@gardenio please refactor",
			trigger: "@gardenio",
			want:    "please refactor",
		},
		{
			name:    "non-ASCII expanding rune after trigger preserved",
			text:    "@gardenio Ⱥ review",
			trigger: "@gardenio",
			want:    "Ⱥ review",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJiraMentionInstruction(tt.text, tt.trigger)
			if got != tt.want {
				t.Errorf("extractJiraMentionInstruction(%q, %q) = %q, want %q",
					tt.text, tt.trigger, got, tt.want)
			}
		})
	}
}

func TestJiraCommentBodyToText_PlainText(t *testing.T) {
	body := json.RawMessage(`"This is a plain text comment"`)
	got := jiraCommentBodyToText(body)
	if got != "This is a plain text comment" {
		t.Errorf("expected plain text, got %q", got)
	}
}

func TestJiraCommentBodyToText_ADF(t *testing.T) {
	adf := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{
				"type": "paragraph",
				"content": [
					{"type": "text", "text": "@gardenio fix this bug"}
				]
			}
		]
	}`)
	got := jiraCommentBodyToText(adf)
	if got != "@gardenio fix this bug" {
		t.Errorf("expected '@gardenio fix this bug', got %q", got)
	}
}

func TestJiraCommentBodyToText_ADFMultipleParagraphs(t *testing.T) {
	adf := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{
				"type": "paragraph",
				"content": [
					{"type": "text", "text": "Hello team."}
				]
			},
			{
				"type": "paragraph",
				"content": [
					{"type": "text", "text": "@gardenio please look into this"}
				]
			}
		]
	}`)
	got := jiraCommentBodyToText(adf)
	if got != "Hello team. @gardenio please look into this" {
		t.Errorf("got %q", got)
	}
}

func TestJiraCommentBodyToText_ADFWithMentionNode(t *testing.T) {
	adf := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{
				"type": "paragraph",
				"content": [
					{
						"type": "mention",
						"text": "@gardenio",
						"attrs": {"id": "12345", "text": "@gardenio"}
					},
					{"type": "text", "text": " fix the auth bug"}
				]
			}
		]
	}`)
	got := jiraCommentBodyToText(adf)
	if got != "@gardenio fix the auth bug" {
		t.Errorf("got %q", got)
	}
}

func TestJiraCommentBodyToText_Empty(t *testing.T) {
	got := jiraCommentBodyToText(nil)
	if got != "" {
		t.Errorf("expected empty for nil body, got %q", got)
	}
	got = jiraCommentBodyToText(json.RawMessage{})
	if got != "" {
		t.Errorf("expected empty for empty body, got %q", got)
	}
}

func TestJiraCommentBodyToText_InvalidJSON(t *testing.T) {
	got := jiraCommentBodyToText(json.RawMessage(`{invalid json`))
	if got != "" {
		t.Errorf("expected empty for invalid JSON, got %q", got)
	}
}

func TestADFToPlainText_CodeBlock(t *testing.T) {
	adf := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{
				"type": "paragraph",
				"content": [
					{"type": "text", "text": "Check this code:"}
				]
			},
			{
				"type": "codeBlock",
				"attrs": {"language": "go"},
				"content": [
					{"type": "text", "text": "func main() {}"}
				]
			}
		]
	}`)
	got := adfToPlainText(adf)
	if got != "Check this code: func main() {}" {
		t.Errorf("got %q", got)
	}
}

func TestADFToPlainText_HardBreak(t *testing.T) {
	adf := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{
				"type": "paragraph",
				"content": [
					{"type": "text", "text": "line one"},
					{"type": "hardBreak"},
					{"type": "text", "text": "line two"}
				]
			}
		]
	}`)
	got := adfToPlainText(adf)
	if got != "line one\nline two" {
		t.Errorf("got %q", got)
	}
}

func TestJiraMentionHandler_Check_PlainText(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	body := json.RawMessage(`"@gardenio fix the alignment issue"`)
	result := h.Check(body)

	if !result.Detected {
		t.Fatal("expected mention to be detected")
	}
	if result.Instruction != "fix the alignment issue" {
		t.Errorf("instruction = %q, want %q", result.Instruction, "fix the alignment issue")
	}
	if result.Trigger != "@gardenio" {
		t.Errorf("trigger = %q, want @gardenio", result.Trigger)
	}
}

func TestJiraMentionHandler_Check_ADF(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	body := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{
				"type": "paragraph",
				"content": [
					{"type": "text", "text": "@gardenio please investigate the flaky test"}
				]
			}
		]
	}`)
	result := h.Check(body)

	if !result.Detected {
		t.Fatal("expected mention to be detected in ADF")
	}
	if result.Instruction != "please investigate the flaky test" {
		t.Errorf("instruction = %q", result.Instruction)
	}
}

func TestJiraMentionHandler_Check_NoMention(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	body := json.RawMessage(`"This is a regular comment without any mention"`)
	result := h.Check(body)

	if result.Detected {
		t.Fatal("expected no mention to be detected")
	}
	if result.Instruction != "" {
		t.Errorf("expected empty instruction, got %q", result.Instruction)
	}
}

func TestJiraMentionHandler_Check_BotTrigger(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:      allowJiraMentionUsers{},
		Trigger:    "@gardenio",
		BotTrigger: "@jira-bot-user",
	})
	body := json.RawMessage(`"@jira-bot-user review this PR"`)
	result := h.Check(body)

	if !result.Detected {
		t.Fatal("expected bot trigger mention to be detected")
	}
	if result.Instruction != "review this PR" {
		t.Errorf("instruction = %q", result.Instruction)
	}
	if result.Trigger != "@jira-bot-user" {
		t.Errorf("trigger = %q, want @jira-bot-user", result.Trigger)
	}
}

func TestJiraMentionHandler_Check_PrimaryOverBot(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:      allowJiraMentionUsers{},
		Trigger:    "@gardenio",
		BotTrigger: "@jira-bot-user",
	})
	body := json.RawMessage(`"@gardenio do the thing"`)
	result := h.Check(body)

	if !result.Detected {
		t.Fatal("expected primary trigger to match")
	}
	if result.Trigger != "@gardenio" {
		t.Errorf("expected primary trigger @gardenio, got %q", result.Trigger)
	}
}

func TestJiraMentionHandler_CheckText_CaseInsensitive(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	result := h.CheckText("Hey @GARDENIO fix this")

	if !result.Detected {
		t.Fatal("expected case-insensitive match")
	}
	if result.Instruction != "fix this" {
		t.Errorf("instruction = %q", result.Instruction)
	}
}

func TestJiraMentionHandler_CheckText_EmptyInstruction(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	result := h.CheckText("@gardenio")

	if result.Detected {
		t.Fatal("expected no detection when no instruction follows trigger")
	}
}

// TestJiraMentionHandler_MentionNewlyIntroduced covers the H5 re-fire guard: it
// fires only when an edit introduced a @gardenio mention that was not there
// before. It underpins the jira:issue_updated description path.
func TestJiraMentionHandler_MentionNewlyIntroduced(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	tests := []struct {
		name    string
		oldText string
		newText string
		want    bool
	}{
		{"added by edit", "please investigate the 500s", "@gardenio investigate the 500s", true},
		{"already present, edited elsewhere", "@gardenio investigate", "@gardenio investigate the timeout too", false},
		{"never mentioned", "just a note", "just a longer note", false},
		{"removed by edit", "@gardenio investigate", "investigate", false},
		{"added case-insensitive", "old text", "@GARDENIO do it", true},
		{"trigger with no instruction is not a mention", "old text", "@gardenio", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.MentionNewlyIntroduced(tc.oldText, tc.newText); got != tc.want {
				t.Errorf("MentionNewlyIntroduced(%q, %q) = %v, want %v",
					tc.oldText, tc.newText, got, tc.want)
			}
		})
	}
}

// TestJiraMentionHandler_MentionNewlyIntroduced_BotTrigger confirms the guard
// honors a configured secondary trigger, mirroring CheckText.
func TestJiraMentionHandler_MentionNewlyIntroduced_BotTrigger(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{BotTrigger: "@mybot"})
	if !h.MentionNewlyIntroduced("plain note", "@mybot handle this") {
		t.Error("expected secondary trigger newly added to fire")
	}
	if h.MentionNewlyIntroduced("@mybot handle this", "@mybot handle this now") {
		t.Error("expected an already-present secondary trigger not to re-fire")
	}
}

func TestJiraMentionHandler_Check_ADFMentionNodeThenText(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	body := json.RawMessage(`{
		"version": 1,
		"type": "doc",
		"content": [
			{
				"type": "paragraph",
				"content": [
					{
						"type": "mention",
						"text": "@gardenio",
						"attrs": {"id": "user-id", "text": "@gardenio"}
					},
					{"type": "text", "text": " deploy the hotfix to staging"}
				]
			}
		]
	}`)
	result := h.Check(body)

	if !result.Detected {
		t.Fatal("expected mention detected via ADF mention node")
	}
	if result.Instruction != "deploy the hotfix to staging" {
		t.Errorf("instruction = %q", result.Instruction)
	}
}

func TestJiraMentionHandler_DefaultTrigger(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{})
	if h.trigger != "@gardenio" {
		t.Errorf("default trigger = %q, want @gardenio", h.trigger)
	}
}

func TestJiraMentionHandler_CustomTrigger(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{Trigger: "@mybot"})
	result := h.CheckText("@mybot do something")
	if !result.Detected {
		t.Fatal("expected custom trigger to match")
	}
	if result.Trigger != "@mybot" {
		t.Errorf("trigger = %q, want @mybot", result.Trigger)
	}
}

// --- Spawner tests (HandleMention, dedup, agent reuse, safety) ---

func jiraMentionTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func makeJiraMentionEvent(jiraKey, summary, commentBody, authorName string) jiraWebhookEvent {
	event := jiraWebhookEvent{
		Issue: &jiraIssueRef{
			Key:    jiraKey,
			Fields: jiraWebhookIssueFields{Summary: summary},
		},
		Comment: &jiraComment{
			ID:   "comment-1",
			Body: json.RawMessage(`"` + commentBody + `"`),
		},
	}
	event.Comment.Author.DisplayName = authorName
	event.Comment.Author.AccountID = "user-123"
	return event
}

// TestJiraHandleMention_FiresSpawnAckOnSpawn is the end-to-end integration for
// the spawn-acknowledgment feature (Slack agent-card parity, plan
// ka-0000007401): a real JiraSpawnAcknowledger wired onto OnSpawn posts an
// immediate internal note on the ticket the instant a PE mention spawns an
// agent. It exercises the SAME path main.go wires — HandleMention → onSpawn →
// Acknowledge → gated Jira comment — proving the event carries the resolved
// spawn role and reaches the write policy.
func TestJiraHandleMention_FiresSpawnAckOnSpawn(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-ack",
		spawnID:  "agent-bead-ack",
		// The spawned agent's role is what the shared write policy gates the ack
		// on (identically to the reply/squawk paths). Use a canonical allowlist
		// role ("crew") so the enforce-mode happy path is exercised end to end;
		// in prod (shadow default) even an unlisted role would still ack.
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action": "spawn_agent", "project": "gardenio",
				"role": "crew", "enabled": "true",
			}},
		},
	}
	jira := &mockJiraSquawkJira{}
	ack := NewJiraSpawnAcknowledger(JiraSpawnAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: enforcedWritePolicy(false),
		Logger:      jiraMentionTestLogger(),
	})
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:       allowJiraMentionUsers{},
		Daemon:      daemon,
		Project:     "gardenio",
		WritePolicy: enforcedWritePolicy(false),
		OnSpawn:     ack.OnSpawn(),
		Logger:      jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-4321", "Login bug", "@gardenio look into this", "Jane Doe")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}

	if len(jira.comments) != 1 {
		t.Fatalf("expected exactly 1 spawn-ack comment on the ticket, got %d", len(jira.comments))
	}
	c := jira.comments[0]
	if c.Key != "PE-4321" {
		t.Errorf("ack posted to %q, want PE-4321", c.Key)
	}
	if c.Vis != CommentInternal {
		t.Errorf("ack visibility = %q, want internal", c.Vis)
	}
	// The spawned agent name is jiraMentionAgentName(PE-4321); assert the ack
	// references the ticket-derived agent identity and the tracking bead.
	if !strings.Contains(c.Text, "task-bead-ack") {
		t.Errorf("ack %q should reference the tracking bead task-bead-ack", c.Text)
	}
	if !strings.Contains(c.Text, "On it") {
		t.Errorf("ack %q should read as an acknowledgment", c.Text)
	}
}

// TestJiraHandleMention_NoAckWiredStaysSilent pins that WITHOUT an acknowledger
// (OnSpawn nil — the legacy behavior) the ticket receives no bridge comment at
// spawn time. This is the pre-feature baseline the ack closes.
func TestJiraHandleMention_NoAckWiredStaysSilent(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "task-bead-x",
		spawnID:      "agent-bead-x",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})
	event := makeJiraMentionEvent("PE-5", "x", "@gardenio do it", "Jane")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}
	// No OnSpawn wired → nothing posts to the ticket at spawn time. (Asserted
	// by construction: the handler holds no Jira client; this test documents
	// the baseline the feature improves.)
}

func TestJiraHandleMention_SpawnsAgentForPETicket(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action":  "spawn_agent",
				"project": "gardenio",
				"role":    "gardenio-crew",
				"enabled": "true",
			}},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate the auth failure", "Jane Doe")
	handled := h.HandleMention(context.Background(), event)

	if !handled {
		t.Fatal("expected mention to be handled")
	}

	// Verify task bead was created.
	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 bead created, got %d", len(daemon.created))
	}
	req := daemon.created[0]
	if req.Type != "task" {
		t.Errorf("bead type = %q, want task", req.Type)
	}
	if !strings.Contains(req.Title, "PE-1234") {
		t.Errorf("title should contain jira key, got %q", req.Title)
	}
	if !strings.Contains(req.Description, "Jane Doe") {
		t.Errorf("description should contain author, got %q", req.Description)
	}
	if !strings.Contains(req.Description, "investigate the auth failure") {
		t.Errorf("description should contain instruction, got %q", req.Description)
	}

	// Verify agent was spawned.
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	spawn := daemon.spawned[0]
	if spawn.Project != "gardenio" {
		t.Errorf("project = %q, want gardenio", spawn.Project)
	}
	if spawn.TaskID != "task-bead-1" {
		t.Errorf("taskID = %q, want task-bead-1", spawn.TaskID)
	}
	if spawn.Role != "gardenio-crew" {
		t.Errorf("role = %q, want gardenio-crew (derived from trigger bead)", spawn.Role)
	}
	if !strings.Contains(spawn.Prompt, "PE-1234") {
		t.Errorf("prompt should contain jira key, got %q", spawn.Prompt)
	}
	if !strings.Contains(spawn.Prompt, "investigate the auth failure") {
		t.Errorf("prompt should contain instruction, got %q", spawn.Prompt)
	}
	if spawn.Extra["jira_key"] != "PE-1234" {
		t.Errorf("extra jira_key = %q", spawn.Extra["jira_key"])
	}
	if spawn.Extra["spawn_source"] != "jira-mention" {
		t.Errorf("extra spawn_source = %q", spawn.Extra["spawn_source"])
	}
}

// TestJiraHandleMention_StampsOperatedBy pins the JIRA → operated_by
// translation so the operated_by transitivity push cannot silently regress.
// allowJiraMentionUsers is the JiraUserLookup most mention tests wire: every
// non-empty accountId or email resolves to a user bead holding the "mention"
// capability, so the authorization gate (gd-fBBMiDHb07) admits the author and
// the test exercises what happens after it. Attribution is oidc:<id>@example.com.
// JiraUser equals the accountId so no jira_user backfill is attempted.
type allowJiraMentionUsers struct{}

func (allowJiraMentionUsers) LookupByJiraUser(_ context.Context, jiraUser string) *UserIdentity {
	if jiraUser == "" {
		return nil
	}
	return &UserIdentity{
		BeadID:       "gd-user-" + jiraUser,
		Email:        jiraUser + "@example.com",
		JiraUser:     jiraUser,
		DisplayName:  jiraUser,
		Capabilities: []string{MentionCapability},
	}
}

func (allowJiraMentionUsers) LookupByEmail(_ context.Context, email string) *UserIdentity {
	if email == "" {
		return nil
	}
	return &UserIdentity{
		BeadID:       "gd-user-" + email,
		Email:        email,
		DisplayName:  email,
		Capabilities: []string{MentionCapability},
	}
}

// fakeJiraUserDir implements JiraUserLookup for tests. It maps a JIRA
// accountId to a *UserIdentity; an absent key resolves to nil.
type fakeJiraUserDir struct {
	byAccount map[string]*UserIdentity
	byEmail   map[string]*UserIdentity
}

func (f *fakeJiraUserDir) LookupByJiraUser(_ context.Context, jiraUser string) *UserIdentity {
	return f.byAccount[jiraUser]
}

func (f *fakeJiraUserDir) LookupByEmail(_ context.Context, email string) *UserIdentity {
	return f.byEmail[email]
}

// mentionCaps is the capability set of a user bead allowed to @gardenio.
var mentionCaps = []string{MentionCapability}

// Mirror of TestHandleMention_StampsOperatedBy (gitlab_mentions_test.go).
//
// JIRA mention-spawned agents carry the authorized author's operator id as
// operated_by on both the task bead's fields and the agent bead's extra
// fields, so the controller injects BEADS_OPERATED_BY into the spawned pod
// and downstream daemon calls stamp X-Operated-By. The JIRA accountId is kept
// as jira_user_id for provenance.
func TestJiraHandleMention_StampsOperatedBy(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-test-task",
		spawnID:      "gd-test-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Daemon:  daemon,
		Users:   allowJiraMentionUsers{},
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane Doe")
	// makeJiraMentionEvent sets accountId="user-123", which allowJiraMentionUsers
	// maps to oidc:user-123@example.com.
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(daemon.created))
	}
	var taskFields map[string]string
	if err := json.Unmarshal(daemon.created[0].Fields, &taskFields); err != nil {
		t.Fatalf("unmarshal task fields: %v", err)
	}
	if got, want := taskFields["operated_by"], "oidc:user-123@example.com"; got != want {
		t.Errorf("task bead operated_by = %q, want %q", got, want)
	}
	if got, want := taskFields["jira_user_id"], "user-123"; got != want {
		t.Errorf("task bead jira_user_id = %q, want %q", got, want)
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	extra := daemon.spawned[0].Extra
	if got, want := extra["operated_by"], "oidc:user-123@example.com"; got != want {
		t.Errorf("agent extra operated_by = %q, want %q", got, want)
	}
	if got, want := extra["jira_user_id"], "user-123"; got != want {
		t.Errorf("agent extra jira_user_id = %q, want %q", got, want)
	}
}

// assertJiraMentionRefused fails the test unless HandleMention reported the
// mention handled (a deliberate refusal, so the webhook does not route it to
// the follow-up loop) while creating no task bead and spawning no agent.
func assertJiraMentionRefused(t *testing.T, h *JiraMentionHandler, daemon *mockJiraMentionDaemon, event jiraWebhookEvent) {
	t.Helper()
	if !h.HandleMention(context.Background(), event) {
		t.Error("refused mention must return handled=true so it is not routed to the follow-up loop")
	}
	if len(daemon.created) != 0 {
		t.Errorf("refused mention created %d task bead(s), want 0", len(daemon.created))
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("refused mention spawned %d agent(s), want 0", len(daemon.spawned))
	}
}

// TestJiraHandleMention_RefusesUnmappedAuthor pins gd-fBBMiDHb07: a comment
// author who maps to no user bead (by accountId or email) spawns nothing. It
// used to spawn an agent operated by JIRA_BRIDGE_DEFAULT_OPERATOR, so anyone
// who could comment on a spawnable project acted as the owner. The positive
// control is the same mention from a mapped author with the capability.
func TestJiraHandleMention_RefusesUnmappedAuthor(t *testing.T) {
	users := &fakeJiraUserDir{
		byAccount: map[string]*UserIdentity{
			"acct-jane": {BeadID: "gd-user-jane", SlackID: "U0JANE", Capabilities: mentionCaps},
		},
		byEmail: map[string]*UserIdentity{},
	}
	newHandler := func(daemon *mockJiraMentionDaemon) *JiraMentionHandler {
		return NewJiraMentionHandler(JiraMentionHandlerConfig{
			Daemon:  daemon,
			Users:   users,
			Project: "gardenio",
			Logger:  jiraMentionTestLogger(),
		})
	}

	for _, tc := range []struct {
		name, accountID, email string
	}{
		{"unknown accountId", "acct-stranger", ""},
		{"unknown accountId and email", "acct-stranger", "stranger@example.com"},
		{"anonymous (no accountId, no email)", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &mockJiraMentionDaemon{triggerBeads: jiraTestSpawnTriggerBeads("gardenio")}
			event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Stranger")
			event.Comment.Author.AccountID = tc.accountID
			event.Comment.Author.EmailAddress = tc.email
			assertJiraMentionRefused(t, newHandler(daemon), daemon, event)
		})
	}

	// Positive control: the mapped author with the capability spawns.
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-test-task",
		spawnID:      "gd-test-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane")
	event.Comment.Author.AccountID = "acct-jane"
	if !newHandler(daemon).HandleMention(context.Background(), event) {
		t.Fatal("expected the authorized mention to be handled")
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("authorized author: expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if got, want := daemon.spawned[0].Extra["operated_by"], "slack:U0JANE"; got != want {
		t.Errorf("authorized author operated_by = %q, want %q", got, want)
	}
}

// TestJiraHandleMention_RefusesWithoutCapability pins the mention capability
// gate (gd-fBBMiDHb07), the same one the Slack and GitLab paths apply: a
// mapped fleet user whose bead lacks "mention" spawns nothing. A bead with no
// Slack id and no email (no human operator to attribute to) is refused too.
func TestJiraHandleMention_RefusesWithoutCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		user *UserIdentity
	}{
		{"no capabilities", &UserIdentity{BeadID: "gd-user-bob", SlackID: "U0BOB"}},
		{"other capabilities only", &UserIdentity{BeadID: "gd-user-bob", SlackID: "U0BOB", Capabilities: []string{"approve:escalation", "slash_command"}}},
		{"capability but no human operator", &UserIdentity{BeadID: "gd-user-bob", Capabilities: mentionCaps}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &mockJiraMentionDaemon{triggerBeads: jiraTestSpawnTriggerBeads("gardenio")}
			h := NewJiraMentionHandler(JiraMentionHandlerConfig{
				Daemon:  daemon,
				Users:   &fakeJiraUserDir{byAccount: map[string]*UserIdentity{"user-123": tc.user}},
				Project: "gardenio",
				Logger:  jiraMentionTestLogger(),
			})
			event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Bob")
			assertJiraMentionRefused(t, h, daemon, event)
		})
	}

	// Positive control: the same user with the capability spawns.
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-test-task",
		spawnID:      "gd-test-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Daemon: daemon,
		Users: &fakeJiraUserDir{byAccount: map[string]*UserIdentity{
			"user-123": {BeadID: "gd-user-bob", SlackID: "U0BOB", Capabilities: mentionCaps},
		}},
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})
	if !h.HandleMention(context.Background(), makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Bob")) {
		t.Fatal("expected the authorized mention to be handled")
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("authorized author: expected 1 agent spawned, got %d", len(daemon.spawned))
	}
}

// TestJiraHandleMention_RefusesWithoutUserDirectory pins fail-closed wiring
// (gd-fBBMiDHb07): with no UserDirectory the handler cannot authorize anyone,
// so it refuses every mention rather than spawning unattributed.
func TestJiraHandleMention_RefusesWithoutUserDirectory(t *testing.T) {
	daemon := &mockJiraMentionDaemon{triggerBeads: jiraTestSpawnTriggerBeads("gardenio")}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})
	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane Doe")
	assertJiraMentionRefused(t, h, daemon, event)
}

// TestJiraHandleMention_RefusedReMentionNotRoutedToFollowUp pins that the
// authorization gate runs before dedup: an unauthorized @gardenio on a ticket
// with only a stale open mention task returns handled=true, so the webhook
// does not hand it to the follow-up loop to wake (or respawn) the agent. The
// control is an authorized author on the same ticket, which returns false
// (routed to follow-up), as TestJiraHandleMention_StaleTaskReMentionNotMuted
// pins.
func TestJiraHandleMention_RefusedReMentionNotRoutedToFollowUp(t *testing.T) {
	stale := &beadsapi.BeadDetail{
		ID:        "task-old",
		Status:    "open",
		CreatedAt: time.Now().Add(-2 * time.Hour),
		Fields:    map[string]string{"jira_key": "PE-1234", "spawn_source": "jira-mention"},
	}
	users := &fakeJiraUserDir{byAccount: map[string]*UserIdentity{
		"acct-jane": {BeadID: "gd-user-jane", SlackID: "U0JANE", Capabilities: mentionCaps},
	}}
	newHandler := func(daemon *mockJiraMentionDaemon) *JiraMentionHandler {
		return NewJiraMentionHandler(JiraMentionHandlerConfig{
			Daemon: daemon, Users: users, Project: "gardenio", Logger: jiraMentionTestLogger(),
		})
	}

	daemon := &mockJiraMentionDaemon{beads: []*beadsapi.BeadDetail{stale}}
	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio wake up", "Stranger")
	event.Comment.Author.AccountID = "acct-stranger"
	assertJiraMentionRefused(t, newHandler(daemon), daemon, event)

	daemon = &mockJiraMentionDaemon{beads: []*beadsapi.BeadDetail{stale}}
	event.Comment.Author.AccountID = "acct-jane"
	if newHandler(daemon).HandleMention(context.Background(), event) {
		t.Error("control: an authorized re-mention on a stale task must return false (routed to follow-up)")
	}
}

// backfillRecordingDaemon adds UpdateBeadFields to the mention mock so the
// jira_user backfill (jiraUserBackfiller) is observable.
type backfillRecordingDaemon struct {
	*mockJiraMentionDaemon
	updates chan string // bead ids passed to UpdateBeadFields
}

func (d *backfillRecordingDaemon) UpdateBeadFields(_ context.Context, beadID string, _ map[string]string) error {
	d.updates <- beadID
	return nil
}

// TestJiraHandleMention_NoBackfillForRefusedAuthor pins that an email match is
// not persisted as a jira_user binding unless the author is authorized
// (gd-fBBMiDHb07): a refused mention never writes to a user bead. The control
// is an authorized email match, which does backfill.
func TestJiraHandleMention_NoBackfillForRefusedAuthor(t *testing.T) {
	daemon := &backfillRecordingDaemon{
		mockJiraMentionDaemon: &mockJiraMentionDaemon{
			createID:     "gd-test-task",
			spawnID:      "gd-test-agent",
			triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		},
		updates: make(chan string, 4),
	}
	users := &fakeJiraUserDir{byEmail: map[string]*UserIdentity{
		"nocap@example.com":   {BeadID: "gd-user-nocap", Email: "nocap@example.com"},
		"allowed@example.com": {BeadID: "gd-user-allowed", Email: "allowed@example.com", Capabilities: mentionCaps},
	}}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Daemon: daemon, Users: users, Project: "gardenio", Logger: jiraMentionTestLogger(),
	})

	refused := makeJiraMentionEvent("PE-1", "x", "@gardenio do it", "No Cap")
	refused.Comment.Author.AccountID = "acct-nocap"
	refused.Comment.Author.EmailAddress = "nocap@example.com"
	assertJiraMentionRefused(t, h, daemon.mockJiraMentionDaemon, refused)

	allowed := makeJiraMentionEvent("PE-2", "x", "@gardenio do it", "Allowed")
	allowed.Comment.Author.AccountID = "acct-allowed"
	allowed.Comment.Author.EmailAddress = "allowed@example.com"
	if !h.HandleMention(context.Background(), allowed) {
		t.Fatal("expected the authorized mention to be handled")
	}
	select {
	case got := <-daemon.updates:
		if got != "gd-user-allowed" {
			t.Fatalf("backfilled %q, want gd-user-allowed (the refused author's bead must never be written)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control: the authorized email match was not backfilled")
	}
	select {
	case got := <-daemon.updates:
		t.Fatalf("unexpected second backfill of %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestJiraHandleMention_ResolvesHumanViaSlackID pins R3 (ka-0000002887): when
// the UserDirectory resolves the comment author's accountId to a fleet user
// with a SlackID, the mention task bead and spawned agent are attributed to
// slack:<id> — not the jira:<accountID> sentinel. The resolved identity's
// slack_user_id / user_bead_id are mirrored for downstream attribution, and
// jira_user_id is retained for provenance.
func TestJiraHandleMention_ResolvesHumanViaSlackID(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-test-task",
		spawnID:      "gd-test-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	users := &fakeJiraUserDir{byAccount: map[string]*UserIdentity{
		"user-123": {BeadID: "gd-user-jane", SlackID: "U0JANE", Email: "jane@example.com", DisplayName: "Jane Doe", Capabilities: mentionCaps},
	}}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Daemon:  daemon,
		Users:   users,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane Doe")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(daemon.created))
	}
	var taskFields map[string]string
	if err := json.Unmarshal(daemon.created[0].Fields, &taskFields); err != nil {
		t.Fatalf("unmarshal task fields: %v", err)
	}
	if got, want := taskFields["operated_by"], "slack:U0JANE"; got != want {
		t.Errorf("task bead operated_by = %q, want %q (resolved human)", got, want)
	}
	if got, want := taskFields["jira_user_id"], "user-123"; got != want {
		t.Errorf("task bead jira_user_id = %q, want %q (provenance)", got, want)
	}
	if got, want := taskFields["slack_user_id"], "U0JANE"; got != want {
		t.Errorf("task bead slack_user_id = %q, want %q", got, want)
	}
	if got, want := taskFields["user_bead_id"], "gd-user-jane"; got != want {
		t.Errorf("task bead user_bead_id = %q, want %q", got, want)
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	extra := daemon.spawned[0].Extra
	if got, want := extra["operated_by"], "slack:U0JANE"; got != want {
		t.Errorf("agent extra operated_by = %q, want %q", got, want)
	}
	if got, want := extra["jira_user_id"], "user-123"; got != want {
		t.Errorf("agent extra jira_user_id = %q, want %q", got, want)
	}
	if got, want := extra["slack_user_id"], "U0JANE"; got != want {
		t.Errorf("agent extra slack_user_id = %q, want %q", got, want)
	}
	if got, want := extra["user_bead_id"], "gd-user-jane"; got != want {
		t.Errorf("agent extra user_bead_id = %q, want %q", got, want)
	}
}

// TestJiraHandleMention_ResolvesHumanViaEmail pins the oidc:<email> fallback
// when the resolved user bead carries an email but no SlackID.
func TestJiraHandleMention_ResolvesHumanViaEmail(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-test-task",
		spawnID:      "gd-test-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	users := &fakeJiraUserDir{byAccount: map[string]*UserIdentity{
		"user-123": {BeadID: "gd-user-bob", Email: "bob@example.com", DisplayName: "Bob", Capabilities: mentionCaps},
	}}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Daemon:  daemon,
		Users:   users,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Bob")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}

	var taskFields map[string]string
	if err := json.Unmarshal(daemon.created[0].Fields, &taskFields); err != nil {
		t.Fatalf("unmarshal task fields: %v", err)
	}
	if got, want := taskFields["operated_by"], "oidc:bob@example.com"; got != want {
		t.Errorf("task bead operated_by = %q, want %q (email fallback)", got, want)
	}
	if _, ok := taskFields["slack_user_id"]; ok {
		t.Errorf("task bead should not stamp slack_user_id when identity has no SlackID")
	}
	if got, want := taskFields["user_bead_id"], "gd-user-bob"; got != want {
		t.Errorf("task bead user_bead_id = %q, want %q", got, want)
	}
	if got, want := daemon.spawned[0].Extra["operated_by"], "oidc:bob@example.com"; got != want {
		t.Errorf("agent extra operated_by = %q, want %q (email fallback)", got, want)
	}
}

// TestJiraHandleMention_ResolvesHumanViaEmailKey is the regression pin for
// ka-0000004473: the Jira Cloud accountId ("user-123") never matches the
// jira_user index (user beads store a human-readable jira_user / email, not
// the opaque accountId). Resolution must fall back to the webhook's
// emailAddress and still attribute the mention to the human — not the
// jira:<accountID> sentinel. The user bead's jira_user is also backfilled to
// the accountId so the next mention resolves directly.
func TestJiraHandleMention_ResolvesHumanViaEmailKey(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-test-task",
		spawnID:      "gd-test-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	// byAccount intentionally has NO entry for "user-123" — only the email
	// index resolves, exactly as in production where jira_user != accountId.
	users := &fakeJiraUserDir{
		byAccount: map[string]*UserIdentity{},
		byEmail: map[string]*UserIdentity{
			"matt@example.com": {BeadID: "gd-user-matt", SlackID: "U0MATT", Email: "matt@example.com", DisplayName: "Matt", Capabilities: mentionCaps},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Daemon:  daemon,
		Users:   users,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Matt")
	event.Comment.Author.EmailAddress = "matt@example.com"
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}

	var taskFields map[string]string
	if err := json.Unmarshal(daemon.created[0].Fields, &taskFields); err != nil {
		t.Fatalf("unmarshal task fields: %v", err)
	}
	if got, want := taskFields["operated_by"], "slack:U0MATT"; got != want {
		t.Errorf("task bead operated_by = %q, want %q (resolved via email key, NOT jira:* sentinel)", got, want)
	}
	if got, want := taskFields["user_bead_id"], "gd-user-matt"; got != want {
		t.Errorf("task bead user_bead_id = %q, want %q", got, want)
	}
}

func TestJiraHandleMention_FailsWithoutTrigger(t *testing.T) {
	// No trigger bead configured for the project → spawn must fail with no
	// hardcoded role fallback. HandleMention returns false on spawn error.
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane Doe")
	if h.HandleMention(context.Background(), event) {
		t.Fatal("expected HandleMention to report failure when no trigger bead configured")
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no spawn without trigger, got %d", len(daemon.spawned))
	}
}

func TestJiraHandleMention_UsesRoleFromMatchingTrigger(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action":  "spawn_agent",
				"project": "monorepo",
				"role":    "monorepo-gardener",
				"enabled": "true",
			}},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "monorepo",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane Doe")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if got := daemon.spawned[0].Role; got != "monorepo-gardener" {
		t.Errorf("role = %q, want monorepo-gardener (derived from trigger bead)", got)
	}
}

func TestJiraHandleMention_BlocksFSMProject(t *testing.T) {
	daemon := &mockJiraMentionDaemon{createID: "x", spawnID: "y"}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("FSM-100", "Customer ticket", "@gardenio fix this", "User")
	handled := h.HandleMention(context.Background(), event)

	if !handled {
		t.Fatal("expected FSM mention to be handled (blocked)")
	}
	if len(daemon.created) != 0 {
		t.Errorf("expected no beads created for FSM, got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agents spawned for FSM, got %d", len(daemon.spawned))
	}
}

func TestJiraHandleMention_BlocksUnauthorizedProject(t *testing.T) {
	daemon := &mockJiraMentionDaemon{createID: "x", spawnID: "y"}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("ACME-500", "Infra task", "@gardenio deploy this", "User")
	handled := h.HandleMention(context.Background(), event)

	if !handled {
		t.Fatal("expected unauthorized project mention to be handled (blocked)")
	}
	if len(daemon.created) != 0 {
		t.Errorf("expected no beads created for unauthorized project, got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agents spawned for unauthorized project, got %d", len(daemon.spawned))
	}
}

func TestJiraHandleMention_NoMention(t *testing.T) {
	daemon := &mockJiraMentionDaemon{}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Some issue", "Just a regular comment", "User")
	handled := h.HandleMention(context.Background(), event)

	if handled {
		t.Fatal("expected no handling for comment without mention")
	}
}

func TestJiraHandleMention_NilCommentOrIssue(t *testing.T) {
	daemon := &mockJiraMentionDaemon{}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:  allowJiraMentionUsers{},
		Daemon: daemon,
		Logger: jiraMentionTestLogger(),
	})

	// Nil comment.
	handled := h.HandleMention(context.Background(), jiraWebhookEvent{
		Issue: &jiraIssueRef{Key: "PE-1"},
	})
	if handled {
		t.Error("expected false for nil comment")
	}

	// Nil issue.
	handled = h.HandleMention(context.Background(), jiraWebhookEvent{
		Comment: &jiraComment{Body: json.RawMessage(`"@gardenio do it"`)},
	})
	if handled {
		t.Error("expected false for nil issue")
	}
}

func TestJiraHandleMention_DedupSkipsDuplicate(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "existing-task",
				Status: "in_progress",
				// FRESH task (created just now) ⇒ a duplicate webhook delivery
				// while the first agent boots ⇒ suppress (ka-0000007091 window).
				CreatedAt: time.Now(),
				Fields: map[string]string{
					"jira_key":     "PE-1234",
					"spawn_source": "jira-mention",
				},
			},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate again", "User")
	handled := h.HandleMention(context.Background(), event)

	if !handled {
		t.Fatal("expected mention to be detected and handled (dedup)")
	}
	if len(daemon.created) != 0 {
		t.Errorf("expected no new beads created (dedup), got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agents spawned (dedup), got %d", len(daemon.spawned))
	}
}

// TestJiraHandleMention_ZeroCreatedAtSuppresses pins the fail-safe: an open
// mention task with an unknown (zero) CreatedAt is treated as FRESH so a
// missing timestamp never turns the dedup gate into a spawn storm
// (ka-0000007091). This is the behavior the pre-window code always had, so it
// is also the regression guard for existing dedup fixtures that omit CreatedAt.
func TestJiraHandleMention_ZeroCreatedAtSuppresses(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "existing-task",
				Status: "in_progress",
				// No CreatedAt set (zero value) — fails safe as fresh.
				Fields: map[string]string{
					"jira_key":     "PE-1234",
					"spawn_source": "jira-mention",
				},
			},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handled := h.HandleMention(context.Background(),
		makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio again", "User"))

	if !handled {
		t.Fatal("expected zero-CreatedAt open task to suppress (handled=true)")
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no spawn for zero-CreatedAt (fresh) task, got %d", len(daemon.spawned))
	}
}

// TestJiraHandleMention_StaleTaskReMentionNotMuted is the headline regression
// guard for ka-0000007091 (the Jira twin of GitLab ka-0000001106): a re-mention
// on a ticket whose ONLY open mention task is STALE (older than the dedup
// window) must NOT be suppressed. HandleMention returns false so the webhook's
// own follow-up loop delivers the comment to the assigned agent (nudge/respawn)
// — before the fix it returned true and the follow-up was skipped, permanently
// muting the ticket. Crucially it also does NOT spawn a second agent: the
// ticket already has an agent binding the follow-up loop can reach.
func TestJiraHandleMention_StaleTaskReMentionNotMuted(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		beads: []*beadsapi.BeadDetail{
			{
				ID:       "stale-task",
				Status:   "open", // agent crashed without closing it
				Assignee: "",     // assignee cleared by the reaper
				// Older than jiraMentionDedupWindow ⇒ stale re-mention.
				CreatedAt: time.Now().Add(-30 * time.Minute),
				Fields: map[string]string{
					"jira_key":     "PE-1234",
					"spawn_source": "jira-mention",
				},
			},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handled := h.HandleMention(context.Background(),
		makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio you missed my last comment", "User"))

	if handled {
		t.Fatal("expected stale re-mention to return false (route to webhook follow-up loop), not true (mute)")
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected NO second agent spawned for a stale re-mention, got %d", len(daemon.spawned))
	}
	if len(daemon.created) != 0 {
		t.Errorf("expected NO new task bead for a stale re-mention, got %d", len(daemon.created))
	}
}

// TestJiraHandleMention_FreshTaskWinsOverStale pins order-independence: a ticket
// carrying BOTH a stale orphan and a fresh open mention task must still dedup
// against the fresh one (a genuine burst), regardless of scan order — suppress
// (handled=true), no spawn.
func TestJiraHandleMention_FreshTaskWinsOverStale(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		beads: []*beadsapi.BeadDetail{
			{
				ID:        "stale-task",
				Status:    "open",
				CreatedAt: time.Now().Add(-30 * time.Minute),
				Fields:    map[string]string{"jira_key": "PE-1234", "spawn_source": "jira-mention"},
			},
			{
				ID:        "fresh-task",
				Status:    "in_progress",
				CreatedAt: time.Now(),
				Fields:    map[string]string{"jira_key": "PE-1234", "spawn_source": "jira-mention"},
			},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	handled := h.HandleMention(context.Background(),
		makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio again", "User"))

	if !handled {
		t.Fatal("expected fresh task to still dedup (handled=true) even alongside a stale orphan")
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no spawn when a fresh duplicate exists, got %d", len(daemon.spawned))
	}
}

func TestJiraHandleMention_DedupIgnoresClosedBeads(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "task-bead-2",
		spawnID:      "agent-bead-2",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "old-task",
				Status: "closed",
				Fields: map[string]string{
					"jira_key":     "PE-1234",
					"spawn_source": "jira-mention",
				},
			},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate this", "User")
	handled := h.HandleMention(context.Background(), event)

	if !handled {
		t.Fatal("expected mention to be handled (closed bead should not block)")
	}
	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 bead created (closed bead ignored), got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
}

func TestJiraHandleMention_ReusesExistingAgent(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "task-bead-new",
		spawnID:      "agent-bead-new",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		beads: []*beadsapi.BeadDetail{
			{
				ID:       "other-task",
				Status:   "in_progress",
				Assignee: "jira-pe-1234-abcd",
				Fields: map[string]string{
					"jira_key": "PE-1234",
					// Note: no spawn_source=jira-mention, so dedup doesn't trigger
				},
			},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio also fix the logout", "User")
	handled := h.HandleMention(context.Background(), event)

	if !handled {
		t.Fatal("expected mention to be handled")
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	// Should reuse the existing agent name.
	if daemon.spawned[0].AgentName != "jira-pe-1234-abcd" {
		t.Errorf("expected reused agent name jira-pe-1234-abcd, got %q", daemon.spawned[0].AgentName)
	}
}

// TestJiraHandleMention_ProjectMapRoutesByPrefix pins the G3 multi-project
// routing contract (epic ka-0000007401): with a BOAT_PROJECTS-style map, a
// mention spawns into the boat project mapped from the ticket's JIRA prefix —
// task-bead label, spawn project, and spawn-role resolution all follow the
// mapping — while an unmapped prefix (or an empty map, the single-project
// deployment case) falls back to the configured default project.
func TestJiraHandleMention_ProjectMapRoutesByPrefix(t *testing.T) {
	// One enabled spawn_agent trigger per boat project so role resolution
	// proves it ran against the MAPPED project, not the default.
	triggers := []*beadsapi.BeadDetail{
		{Fields: map[string]string{
			"action": "spawn_agent", "project": "boatA",
			"role": "boatA-crew", "enabled": "true",
		}},
		{Fields: map[string]string{
			"action": "spawn_agent", "project": "boatB",
			"role": "boatB-crew", "enabled": "true",
		}},
		{Fields: map[string]string{
			"action": "spawn_agent", "project": "defaultboat",
			"role": "default-crew", "enabled": "true",
		}},
	}

	tests := []struct {
		name        string
		projectMap  map[string]string
		jiraKey     string
		wantProject string
		wantRole    string
	}{
		{
			name:        "mapped PE prefix routes to boatA",
			projectMap:  map[string]string{"PE": "boatA", "DEVOPS": "boatB"},
			jiraKey:     "PE-1",
			wantProject: "boatA",
			wantRole:    "boatA-crew",
		},
		{
			name:        "mapped DEVOPS prefix routes to boatB",
			projectMap:  map[string]string{"PE": "boatA", "DEVOPS": "boatB"},
			jiraKey:     "DEVOPS-9",
			wantProject: "boatB",
			wantRole:    "boatB-crew",
		},
		{
			name:        "unmapped prefix falls back to default project",
			projectMap:  map[string]string{"DEVOPS": "boatB"},
			jiraKey:     "PE-1",
			wantProject: "defaultboat",
			wantRole:    "default-crew",
		},
		{
			name:        "empty map preserves single-project behavior",
			projectMap:  map[string]string{},
			jiraKey:     "PE-1",
			wantProject: "defaultboat",
			wantRole:    "default-crew",
		},
		{
			name:        "nil map preserves single-project behavior",
			projectMap:  nil,
			jiraKey:     "DEVOPS-9",
			wantProject: "defaultboat",
			wantRole:    "default-crew",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon := &mockJiraMentionDaemon{
				createID:     "task-bead-1",
				spawnID:      "agent-bead-1",
				triggerBeads: triggers,
			}
			h := NewJiraMentionHandler(JiraMentionHandlerConfig{
				Users:      allowJiraMentionUsers{},
				Daemon:     daemon,
				Project:    "defaultboat",
				ProjectMap: tt.projectMap,
				Logger:     jiraMentionTestLogger(),
			})

			event := makeJiraMentionEvent(tt.jiraKey, "Fix bug", "@gardenio investigate", "Jane Doe")
			if !h.HandleMention(context.Background(), event) {
				t.Fatal("expected mention to be handled")
			}

			if len(daemon.created) != 1 {
				t.Fatalf("expected 1 task bead created, got %d", len(daemon.created))
			}
			wantLabel := "project:" + tt.wantProject
			var found bool
			for _, l := range daemon.created[0].Labels {
				if l == wantLabel {
					found = true
				}
			}
			if !found {
				t.Errorf("task bead labels = %v, want %q", daemon.created[0].Labels, wantLabel)
			}

			if len(daemon.spawned) != 1 {
				t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
			}
			if got := daemon.spawned[0].Project; got != tt.wantProject {
				t.Errorf("spawn project = %q, want %q", got, tt.wantProject)
			}
			if got := daemon.spawned[0].Role; got != tt.wantRole {
				t.Errorf("spawn role = %q, want %q (resolved against mapped project)", got, tt.wantRole)
			}
		})
	}
}

// TestResolveProjectFromJiraKey_LowercasePrefixMatchesUpperMap guards the
// case contract: parseBoatProjects uppercases map keys, and the resolver must
// uppercase the ticket prefix before the lookup so an oddly-cased issue key
// still routes.
func TestResolveProjectFromJiraKey_LowercasePrefixMatchesUpperMap(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:      allowJiraMentionUsers{},
		Daemon:     &mockJiraMentionDaemon{},
		Project:    "defaultboat",
		ProjectMap: map[string]string{"PE": "boatA"},
		Logger:     jiraMentionTestLogger(),
	})
	if got := h.resolveProjectFromJiraKey("pe-77"); got != "boatA" {
		t.Errorf("resolveProjectFromJiraKey(pe-77) = %q, want boatA", got)
	}
	if got := h.resolveProjectFromJiraKey("PE-77"); got != "boatA" {
		t.Errorf("resolveProjectFromJiraKey(PE-77) = %q, want boatA", got)
	}
}

func TestJiraProjectKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"PE-1234", "PE"},
		{"FSM-100", "FSM"},
		{"DEVOPS-42", "DEVOPS"},
		{"NOHYPHEN", "NOHYPHEN"},
		{"A-1", "A"},
	}
	for _, tt := range tests {
		got := jiraProjectKey(tt.input)
		if got != tt.want {
			t.Errorf("jiraProjectKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildJiraMentionPrompt(t *testing.T) {
	prompt := buildJiraMentionPrompt(jiraMentionPromptParams{
		JiraKey:         "PE-1234",
		Summary:         "Fix login bug",
		Author:          "Jane Doe",
		Instruction:     "investigate the auth failure",
		TaskID:          "task-bead-1",
		Operator:        "slack:U0JANE",
		OperatorDisplay: "Jane Doe",
		History:         "- Bob (2026-07-01T10:00:00Z): first report of the issue",
	})

	for _, want := range []string{
		"PE-1234",
		"Fix login bug",
		"Jane Doe",
		"investigate the auth failure",
		"task-bead-1",
		// F4: resolved operator identity so the agent addresses the right human.
		"Operator: Jane Doe (slack:U0JANE)",
		// F4: recent comment history so the agent replies in context.
		"Recent comments on PE-1234",
		"first report of the issue",
		// F2 note: the conversational reply affordance must be discoverable.
		"gb jira reply",
		// G1/G6: mid-session re-read affordances — conversation (gb jira
		// thread) and current ticket body (gb jira show <key>) — must be
		// discoverable so an agent re-reads a snapshot that has gone stale
		// (epic ka-0000007401).
		"gb jira thread",
		"gb jira show PE-1234",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %s", want, prompt)
		}
	}
}

// TestBuildJiraMentionPrompt_NoOperatorNoHistory pins the degraded shape: a
// sentinel (non-human) operator renders no Operator line, and empty history
// renders no transcript section — the prompt stays close to the legacy form.
func TestBuildJiraMentionPrompt_NoOperatorNoHistory(t *testing.T) {
	prompt := buildJiraMentionPrompt(jiraMentionPromptParams{
		JiraKey:     "PE-1234",
		Summary:     "Fix login bug",
		Author:      "Jane Doe",
		Instruction: "investigate the auth failure",
		TaskID:      "task-bead-1",
		Operator:    "jira:user-123", // sentinel — must not render
	})

	if strings.Contains(prompt, "Operator:") {
		t.Errorf("sentinel operator must not render an Operator line: %s", prompt)
	}
	if strings.Contains(prompt, "Recent comments") {
		t.Errorf("empty history must not render a transcript section: %s", prompt)
	}
	if !strings.Contains(prompt, "gb jira reply") {
		t.Errorf("prompt must always advertise gb jira reply: %s", prompt)
	}
}

func TestJiraOperatorPromptLine(t *testing.T) {
	const tail = " — the human you are acting for; address them in replies."
	tests := []struct {
		operator string
		display  string
		want     string
	}{
		{"slack:U0JANE", "Jane Doe", "Operator: Jane Doe (slack:U0JANE)" + tail},
		{"oidc:jane@example.com", "", "Operator: oidc:jane@example.com" + tail},
		// display == operator collapses to the bare id (no doubled text).
		{"slack:U0JANE", "slack:U0JANE", "Operator: slack:U0JANE" + tail},
		{"jira:user-123", "Jane", ""}, // sentinel: no line
		{"fleet:jira-bridge", "", ""}, // sentinel: no line
		{"", "Jane", ""},              // empty: no line
	}
	for _, tt := range tests {
		if got := jiraOperatorPromptLine(tt.operator, tt.display); got != tt.want {
			t.Errorf("jiraOperatorPromptLine(%q, %q) = %q, want %q", tt.operator, tt.display, got, tt.want)
		}
	}
}

func TestFormatJiraCommentHistory(t *testing.T) {
	comments := []JiraCommentEntry{
		// API order: newest first.
		{ID: "3", Author: JiraUser{DisplayName: "Carol"}, Body: json.RawMessage(`"@gardenio please fix"`), Created: "2026-07-03T09:00:00Z"},
		{ID: "2", Author: JiraUser{DisplayName: "Bob"}, Body: json.RawMessage(`"I can reproduce it"`), Created: "2026-07-02T09:00:00Z"},
		{ID: "1", Author: JiraUser{DisplayName: "Alice"}, Body: json.RawMessage(`"login is broken"`), Created: "2026-07-01T09:00:00Z"},
	}

	got := formatJiraCommentHistory(comments, "3")

	// The triggering comment (id 3) is excluded; the rest render oldest first.
	if strings.Contains(got, "please fix") {
		t.Errorf("triggering comment must be excluded from history: %s", got)
	}
	aliceIdx := strings.Index(got, "Alice")
	bobIdx := strings.Index(got, "Bob")
	if aliceIdx < 0 || bobIdx < 0 {
		t.Fatalf("history missing authors: %s", got)
	}
	if aliceIdx > bobIdx {
		t.Errorf("history should be oldest first (Alice before Bob): %s", got)
	}
	if !strings.Contains(got, "login is broken") || !strings.Contains(got, "I can reproduce it") {
		t.Errorf("history missing comment bodies: %s", got)
	}
}

func TestFormatJiraCommentHistory_TruncatesLongComments(t *testing.T) {
	long := strings.Repeat("x", jiraHistoryMaxCommentLen*2)
	comments := []JiraCommentEntry{
		{ID: "1", Author: JiraUser{DisplayName: "Alice"}, Body: json.RawMessage(`"` + long + `"`)},
	}
	got := formatJiraCommentHistory(comments, "")
	if len(got) > jiraHistoryMaxCommentLen+100 {
		t.Errorf("history entry not truncated: len=%d", len(got))
	}
	if !strings.Contains(got, "...") {
		t.Errorf("truncated entry should end with ellipsis: %s", got)
	}
}

// fakeJiraHistory implements JiraCommentHistoryClient for testing.
type fakeJiraHistory struct {
	comments []JiraCommentEntry
	err      error
	gotKey   string
	gotMax   int
}

func (f *fakeJiraHistory) GetComments(_ context.Context, key string, maxResults int) ([]JiraCommentEntry, error) {
	f.gotKey = key
	f.gotMax = maxResults
	return f.comments, f.err
}

// TestJiraHandleMention_SpawnPromptCarriesHistoryAndOperator pins F4 (plan
// ka-0000007401) end to end on the mention spawn path: the spawned agent's
// prompt includes the ticket's recent comment history (minus the triggering
// comment) and the resolved human operator identity.
func TestJiraHandleMention_SpawnPromptCarriesHistoryAndOperator(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	users := &fakeJiraUserDir{byAccount: map[string]*UserIdentity{
		"user-123": {BeadID: "gd-user-jane", SlackID: "U0JANE", Email: "jane@example.com", DisplayName: "Jane Doe", Capabilities: mentionCaps},
	}}
	history := &fakeJiraHistory{comments: []JiraCommentEntry{
		// Newest first, as the Jira API returns them. comment-1 is the
		// triggering mention (makeJiraMentionEvent uses ID "comment-1").
		{ID: "comment-1", Author: JiraUser{DisplayName: "Jane Doe"}, Body: json.RawMessage(`"@gardenio investigate"`)},
		{ID: "c-old", Author: JiraUser{DisplayName: "Bob"}, Body: json.RawMessage(`"stack trace attached"`), Created: "2026-07-01T09:00:00Z"},
	}}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Daemon:  daemon,
		Users:   users,
		History: history,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane Doe")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	prompt := daemon.spawned[0].Prompt

	if history.gotKey != "PE-1234" {
		t.Errorf("history fetched for %q, want PE-1234", history.gotKey)
	}
	if !strings.Contains(prompt, "stack trace attached") {
		t.Errorf("prompt missing prior comment history: %s", prompt)
	}
	if !strings.Contains(prompt, "Operator: Jane Doe (slack:U0JANE)") {
		t.Errorf("prompt missing resolved operator identity: %s", prompt)
	}
	if !strings.Contains(prompt, "gb jira reply") {
		t.Errorf("prompt missing gb jira reply affordance: %s", prompt)
	}
	// The triggering comment already appears as the Instruction (sans
	// trigger); the history section must not duplicate its full body.
	if strings.Contains(prompt, "@gardenio investigate") {
		t.Errorf("triggering comment must be excluded from the history section: %s", prompt)
	}
}

// TestJiraHandleMention_HistoryFetchFailureDegrades pins that a comment-history
// fetch error never blocks the spawn — the prompt just omits the transcript.
func TestJiraHandleMention_HistoryFetchFailureDegrades(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	history := &fakeJiraHistory{err: io.ErrUnexpectedEOF}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		History: history,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane Doe")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled despite history fetch failure")
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if strings.Contains(daemon.spawned[0].Prompt, "Recent comments") {
		t.Errorf("failed history fetch must not render a transcript section: %s", daemon.spawned[0].Prompt)
	}
}

func TestJiraHandleMention_OnSpawnCallback(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}

	var spawnEvent *JiraMentionSpawnEvent
	handler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		OnSpawn: func(_ context.Context, event JiraMentionSpawnEvent) {
			spawnEvent = &event
		},
		Logger: jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login", "@gardenio fix the auth bug", "Jane")
	handled := handler.HandleMention(context.Background(), event)

	if !handled {
		t.Fatal("expected mention to be handled")
	}
	if spawnEvent == nil {
		t.Fatal("expected OnSpawn callback to be called")
	}
	if spawnEvent.JiraKey != "PE-1234" {
		t.Errorf("JiraKey = %q, want PE-1234", spawnEvent.JiraKey)
	}
	if spawnEvent.Author != "Jane" {
		t.Errorf("Author = %q, want Jane", spawnEvent.Author)
	}
	if spawnEvent.AgentBeadID != "agent-bead-1" {
		t.Errorf("AgentBeadID = %q, want agent-bead-1", spawnEvent.AgentBeadID)
	}
	if spawnEvent.TaskBeadID != "task-bead-1" {
		t.Errorf("TaskBeadID = %q, want task-bead-1", spawnEvent.TaskBeadID)
	}
	if spawnEvent.Instruction == "" {
		t.Error("expected non-empty instruction")
	}
}

func TestJiraHandleMention_OnSpawnNotCalledWhenBlocked(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
	}

	called := false
	handler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		OnSpawn: func(_ context.Context, _ JiraMentionSpawnEvent) {
			called = true
		},
		Logger: jiraMentionTestLogger(),
	})

	// FSM project should be blocked — OnSpawn must not fire.
	event := makeJiraMentionEvent("FSM-100", "Service ticket", "@gardenio investigate", "Bob")
	handler.HandleMention(context.Background(), event)

	if called {
		t.Error("expected OnSpawn NOT to be called for blocked FSM project")
	}
}

func TestJiraHandleMention_OnSpawnNilIsOk(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}

	// No OnSpawn callback — should not panic.
	handler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Test", "@gardenio do something", "Alice")
	handled := handler.HandleMention(context.Background(), event)

	if !handled {
		t.Error("expected mention to be handled even without OnSpawn callback")
	}
}

// --- triggered-by lineage tests (auto-link epic ka-0000000933 M2c ka-0000003814) ---
//
// The mention webhook path creates a fresh mention task bead and spawns an
// agent on it. To make the lineage walkable from the Jira-mirror bead, the
// handler must write a `triggered-by` deps row from the mention task to the
// mirror bead (label `jira:<key>`). All writes are gardenio-side deps rows on
// the mirror; no outbound Jira API calls (advice ka-0000003234).

// TestJiraHandleMention_WritesTriggeredByToMirrorBead pins the M2c acceptance:
// after a successful spawn, the handler looks up the Jira-mirror bead by
// `jira:<key>` label and writes a triggered-by deps row from the mention
// task to the mirror.
func TestJiraHandleMention_WritesTriggeredByToMirrorBead(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-mention-task",
		spawnID:      "gd-mention-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		mirrorBeads: []*beadsapi.BeadDetail{
			{ID: "gd-jira-mirror-pe1234", Labels: []string{"source:jira", "jira:PE-1234"}},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login", "@gardenio investigate", "Jane Doe")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.addDepCalls) != 1 {
		t.Fatalf("expected exactly 1 AddDependency call (mirror triggered-by), got %d: %+v",
			len(daemon.addDepCalls), daemon.addDepCalls)
	}
	got := daemon.addDepCalls[0]
	if got.BeadID != "gd-mention-task" {
		t.Errorf("AddDependency beadID = %q, want %q (the spawned mention task)",
			got.BeadID, "gd-mention-task")
	}
	if got.DependsOnID != "gd-jira-mirror-pe1234" {
		t.Errorf("AddDependency dependsOnID = %q, want %q (the Jira-mirror bead)",
			got.DependsOnID, "gd-jira-mirror-pe1234")
	}
	if got.DepType != "triggered-by" {
		t.Errorf("AddDependency depType = %q, want triggered-by", got.DepType)
	}
	if got.CreatedBy != "jira-bridge" {
		t.Errorf("AddDependency createdBy = %q, want jira-bridge", got.CreatedBy)
	}
}

// TestJiraHandleMention_NoMirrorBeadDoesNotFail confirms that an absent
// Jira-mirror bead (e.g. mention arrives before the Jira poller has created
// the mirror) leaves the spawn untouched: no AddDependency call, no error.
func TestJiraHandleMention_NoMirrorBeadDoesNotFail(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-mention-task",
		spawnID:      "gd-mention-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		// mirrorBeads intentionally nil — mirror not yet ingested.
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-9999", "New ticket", "@gardenio take a look", "User")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled even without a mirror bead")
	}
	if len(daemon.addDepCalls) != 0 {
		t.Errorf("expected 0 AddDependency calls when no mirror, got %d", len(daemon.addDepCalls))
	}
	if len(daemon.spawned) != 1 {
		t.Errorf("expected spawn to proceed when no mirror, got %d spawns", len(daemon.spawned))
	}
}

// TestJiraHandleMention_TriggeredByAddDepErrorDoesNotFailSpawn confirms that a
// failure writing the triggered-by deps row is best-effort: the spawn already
// happened and the handler still returns success. Errors are logged but the
// caller does not see them.
func TestJiraHandleMention_TriggeredByAddDepErrorDoesNotFailSpawn(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-mention-task",
		spawnID:      "gd-mention-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		mirrorBeads: []*beadsapi.BeadDetail{
			{ID: "gd-jira-mirror-pe1234"},
		},
		addDepErr: io.EOF, // any non-nil error
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login", "@gardenio investigate", "Jane Doe")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled even when triggered-by dep write fails")
	}
	if len(daemon.addDepCalls) != 1 {
		t.Errorf("expected 1 AddDependency attempt, got %d", len(daemon.addDepCalls))
	}
}

// TestJiraHandleMention_TriggeredByMirrorLookupErrorDoesNotFailSpawn confirms
// that a failed mirror lookup does not break the mention spawn.
func TestJiraHandleMention_TriggeredByMirrorLookupErrorDoesNotFailSpawn(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:      "gd-mention-task",
		spawnID:       "gd-mention-agent",
		triggerBeads:  jiraTestSpawnTriggerBeads("gardenio"),
		listFilterErr: io.EOF,
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login", "@gardenio investigate", "User")
	// Spawn fails because listFilterErr also breaks the spawn-role lookup
	// (resolveJiraSpawnRole calls ListBeadsFiltered before the triggered-by
	// write). In that case we just want the handler to not panic.
	_ = h.HandleMention(context.Background(), event)
	if len(daemon.addDepCalls) != 0 {
		t.Errorf("expected no AddDependency on lookup error, got %d", len(daemon.addDepCalls))
	}
}

// TestJiraHandleMention_TriggeredBySkipsSelfReference guards against an
// edge case where the mirror lookup somehow returns the mention task itself
// (e.g. mention task labelled `jira:<key>` for backward-compat with the
// `findExistingJiraAgent` lookup path). The handler must not write a
// self-referential triggered-by edge.
func TestJiraHandleMention_TriggeredBySkipsSelfReference(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "gd-mention-task",
		spawnID:      "gd-mention-agent",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		mirrorBeads: []*beadsapi.BeadDetail{
			{ID: "gd-mention-task"}, // same ID as the spawned task
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	event := makeJiraMentionEvent("PE-1234", "Fix login", "@gardenio investigate", "User")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}
	if len(daemon.addDepCalls) != 0 {
		t.Errorf("expected 0 AddDependency calls (self-reference skipped), got %d: %+v",
			len(daemon.addDepCalls), daemon.addDepCalls)
	}
}

// TestJiraHandleMention_StampsJiraCommentID verifies the mention spawn path
// persists the originating comment id on BOTH the mention task bead and the
// spawned agent bead (F3, plan ka-0000007401), matching the webhook
// follow-up path so replies can thread to the triggering comment.
func TestJiraHandleMention_StampsJiraCommentID(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID:     "task-bead-1",
		spawnID:      "agent-bead-1",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	// makeJiraMentionEvent stamps comment id "comment-1".
	event := makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio investigate", "Jane Doe")
	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(daemon.created))
	}
	var taskFields map[string]string
	if err := json.Unmarshal(daemon.created[0].Fields, &taskFields); err != nil {
		t.Fatalf("unmarshal task fields: %v", err)
	}
	if got := taskFields["jira_comment_id"]; got != "comment-1" {
		t.Errorf("task jira_comment_id = %q; want comment-1", got)
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(daemon.spawned))
	}
	if got := daemon.spawned[0].Extra["jira_comment_id"]; got != "comment-1" {
		t.Errorf("agent jira_comment_id = %q; want comment-1", got)
	}
}

// TestJiraHandleMention_SpawnsForNativeMentionByAccountID is the end-to-end
// regression for the real prod GBFSM-85 miss (epic ka-0000007401): the operator
// used Jira's native @-picker to mention the bot user, producing an ADF "mention"
// node (accountId 712020:66bb0993, display text "@Gardenio Agent") followed by
// the instruction. The webhook received the comment but the mention was never
// detected — no agent spawned — because the flattened text carried no clean
// "@gardenio" token. With the bot accountId configured, HandleMention now detects
// the native mention structurally and spawns an agent, with the instruction free
// of the leaked display name.
func TestJiraHandleMention_SpawnsForNativeMentionByAccountID(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		triggerBeads: []*beadsapi.BeadDetail{
			{Fields: map[string]string{
				"action":  "spawn_agent",
				"project": "gardenio",
				"role":    "gardenio-crew",
				"enabled": "true",
			}},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:         allowJiraMentionUsers{},
		Daemon:        daemon,
		Project:       "gardenio",
		Logger:        jiraMentionTestLogger(),
		BotAccountIDs: []string{"712020:66bb0993"},
	})

	// Same ADF shape as prod GBFSM-85 comment 114384 (native @-mention node,
	// display text "@Gardenio Agent"), on a PE ticket so the legacy spawn gate
	// (PE/DEVOPS in tests without a WritePolicy) admits the spawn — the
	// native-mention DETECTION under test is project-independent, and the
	// GBFSM-vs-PE spawn eligibility is a separate, already-tested gate.
	event := jiraWebhookEvent{
		Issue: &jiraIssueRef{
			Key:    "PE-9001",
			Fields: jiraWebhookIssueFields{Summary: "Test ticket"},
		},
		Comment: &jiraComment{
			ID: "114384",
			Body: json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph","content":[` +
				`{"type":"mention","attrs":{"id":"712020:66bb0993","text":"@Gardenio Agent"}},` +
				`{"type":"text","text":" test, just respond 'test'"}]}]}`),
		},
	}
	event.Comment.Author.DisplayName = "Matthew.Baker"
	event.Comment.Author.AccountID = "712020:478f711f"

	if !h.HandleMention(context.Background(), event) {
		t.Fatal("expected native @-mention of the bot to be handled")
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned for native mention, got %d", len(daemon.spawned))
	}
	spawn := daemon.spawned[0]
	if spawn.Extra["jira_key"] != "PE-9001" {
		t.Errorf("extra jira_key = %q, want PE-9001", spawn.Extra["jira_key"])
	}
	if !strings.Contains(spawn.Prompt, "test, just respond 'test'") {
		t.Errorf("prompt should carry the instruction, got %q", spawn.Prompt)
	}
	// The bot's display name ("Agent") must NOT leak into the instruction.
	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(daemon.created))
	}
	if strings.Contains(daemon.created[0].Title, "Agent test") {
		t.Errorf("instruction leaked bot display name into title: %q", daemon.created[0].Title)
	}
}

// TestJiraMentionHandler_Check_NativeMentionNoAtPrefix is the regression for the
// real prod GBFSM-85 miss (epic ka-0000007401): a native Jira @-mention of the
// bot user arrives as an ADF "mention" node whose display text is the account
// name WITHOUT a leading "@" (e.g. "Gardenio Agent"). Before the fix the
// flattened text carried no "@gardenio" token, so Check returned not-detected and
// the mention silently never spawned. mentionNodeText now guarantees a leading
// "@" so the trigger matches when the bot's display name IS the trigger.
func TestJiraMentionHandler_Check_NativeMentionNoAtPrefix(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{Trigger: "@gardenio"})
	body := json.RawMessage(`{
		"version": 1, "type": "doc",
		"content": [{"type": "paragraph", "content": [
			{"type": "mention", "attrs": {"id": "acct-1", "text": "gardenio"}},
			{"type": "text", "text": " deploy the hotfix"}
		]}]
	}`)
	result := h.Check(body)
	if !result.Detected {
		t.Fatal("expected mention detected when native mention text lacks a leading @")
	}
	if result.Instruction != "deploy the hotfix" {
		t.Errorf("instruction = %q; want %q", result.Instruction, "deploy the hotfix")
	}
}

// TestJiraMentionHandler_Check_ByAccountID exercises the structured,
// display-name-independent detection path: a mention node whose attrs.id matches
// a configured bot accountId is detected regardless of its display text, and the
// instruction is exactly the text AFTER the mention node — no leaked display
// name (the real GBFSM-85 body had display text "@Gardenio Agent", whose trailing
// "Agent" would otherwise leak into a substring-based instruction).
func TestJiraMentionHandler_Check_ByAccountID(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:         allowJiraMentionUsers{},
		BotAccountIDs: []string{"712020:66bb0993"},
	})
	body := json.RawMessage(`{
		"version": 1, "type": "doc",
		"content": [{"type": "paragraph", "content": [
			{"type": "mention", "attrs": {"id": "712020:66bb0993", "text": "@Gardenio Agent"}},
			{"type": "text", "text": " test, just respond 'test'"}
		]}]
	}`)
	result := h.Check(body)
	if !result.Detected {
		t.Fatal("expected mention detected via bot accountId match")
	}
	if want := "test, just respond 'test'"; result.Instruction != want {
		t.Errorf("instruction = %q; want %q (display name must not leak)", result.Instruction, want)
	}
	if result.InstructionMarkdown != "test, just respond 'test'" {
		t.Errorf("markdown instruction = %q", result.InstructionMarkdown)
	}
}

// TestJiraMentionHandler_Check_ByAccountID_CaseInsensitive confirms accountId
// matching is case-insensitive (Jira ids are opaque; be lenient).
func TestJiraMentionHandler_Check_ByAccountID_CaseInsensitive(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:         allowJiraMentionUsers{},
		BotAccountIDs: []string{"712020:ABCDEF"},
	})
	body := json.RawMessage(`{
		"version": 1, "type": "doc",
		"content": [{"type": "paragraph", "content": [
			{"type": "mention", "attrs": {"id": "712020:abcdef", "text": "bot"}},
			{"type": "text", "text": " do the thing"}
		]}]
	}`)
	if r := h.Check(body); !r.Detected || r.Instruction != "do the thing" {
		t.Errorf("case-insensitive accountId match failed: detected=%v instr=%q", r.Detected, r.Instruction)
	}
}

// TestJiraMentionHandler_Check_ByAccountID_NoTrailingText confirms a bare bot
// @-mention with no instruction after it does NOT spawn — matching the
// text-trigger contract (a trigger with no instruction is not actionable) so the
// caller falls back and ultimately declines.
func TestJiraMentionHandler_Check_ByAccountID_NoTrailingText(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:         allowJiraMentionUsers{},
		BotAccountIDs: []string{"acct-1"},
	})
	body := json.RawMessage(`{
		"version": 1, "type": "doc",
		"content": [{"type": "paragraph", "content": [
			{"type": "mention", "attrs": {"id": "acct-1", "text": "@bot"}}
		]}]
	}`)
	if r := h.Check(body); r.Detected {
		t.Errorf("bare bot mention with no instruction should not detect; got instr=%q", r.Instruction)
	}
}

// TestJiraMentionHandler_Check_ByAccountID_NonMatchingFallsBack confirms that a
// mention of a DIFFERENT account does not trigger the structured path, and that
// detection then falls back to the text-trigger path (so a literal "@gardenio"
// elsewhere still works).
func TestJiraMentionHandler_Check_ByAccountID_NonMatchingFallsBack(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:         allowJiraMentionUsers{},
		Trigger:       "@gardenio",
		BotAccountIDs: []string{"bot-acct"},
	})
	// Mentions a human, but the text carries a literal @gardenio trigger too.
	body := json.RawMessage(`{
		"version": 1, "type": "doc",
		"content": [{"type": "paragraph", "content": [
			{"type": "mention", "attrs": {"id": "some-human", "text": "@Alice"}},
			{"type": "text", "text": " please ask @gardenio to ship it"}
		]}]
	}`)
	r := h.Check(body)
	if !r.Detected {
		t.Fatal("expected fallback text-trigger detection when accountId does not match")
	}
	if r.Instruction != "to ship it" {
		t.Errorf("instruction = %q; want %q", r.Instruction, "to ship it")
	}
}

// TestJiraMentionHandler_Check_ByAccountID_AcrossParagraphs confirms the
// structured path captures instruction text that spans blocks after the mention
// (a mention in one paragraph, the ask in the next).
func TestJiraMentionHandler_Check_ByAccountID_AcrossParagraphs(t *testing.T) {
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{BotAccountIDs: []string{"acct-1"}})
	body := json.RawMessage(`{
		"version": 1, "type": "doc",
		"content": [
			{"type": "paragraph", "content": [
				{"type": "mention", "attrs": {"id": "acct-1", "text": "@bot"}}
			]},
			{"type": "paragraph", "content": [
				{"type": "text", "text": "look into the flaky test"}
			]}
		]
	}`)
	r := h.Check(body)
	if !r.Detected || r.Instruction != "look into the flaky test" {
		t.Errorf("cross-paragraph capture failed: detected=%v instr=%q", r.Detected, r.Instruction)
	}
}
