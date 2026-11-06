package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// mockJiraSquawkDaemon implements JiraSquawkClient for testing.
type mockJiraSquawkDaemon struct {
	mu         sync.Mutex
	agentBeads map[string]*beadsapi.BeadDetail
}

func (m *mockJiraSquawkDaemon) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bd, ok := m.agentBeads[agentName]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}
	return bd, nil
}

// mockJiraSquawkJira implements JiraSquawkJiraClient for testing.
type mockJiraSquawkJira struct {
	mu       sync.Mutex
	comments []jiraSquawkComment
	err      error
}

type jiraSquawkComment struct {
	Key  string
	Text string
	Vis  CommentVisibility
	// SourceCommentID is the mention-comment anchor passed via the
	// source-linked reply variant; empty for plain visibility comments (and
	// for source-linked calls that degrade to the flat form).
	SourceCommentID string
	// Markdown records that the comment arrived via the markdown-rendering
	// reply variant (H1, ka-0000004233).
	Markdown bool
}

func (m *mockJiraSquawkJira) AddCommentWithVisibility(_ context.Context, key, text string, vis CommentVisibility) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.comments = append(m.comments, jiraSquawkComment{Key: key, Text: text, Vis: vis})
	return nil
}

func (m *mockJiraSquawkJira) AddCommentWithVisibilityAndSourceLink(_ context.Context, key, text string, vis CommentVisibility, sourceCommentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.comments = append(m.comments, jiraSquawkComment{Key: key, Text: text, Vis: vis, SourceCommentID: sourceCommentID})
	return nil
}

func (m *mockJiraSquawkJira) AddMarkdownCommentWithVisibilityAndSourceLink(_ context.Context, key, text string, vis CommentVisibility, sourceCommentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.comments = append(m.comments, jiraSquawkComment{Key: key, Text: text, Vis: vis, SourceCommentID: sourceCommentID, Markdown: true})
	return nil
}

func (m *mockJiraSquawkJira) getComments() []jiraSquawkComment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]jiraSquawkComment, len(m.comments))
	copy(out, m.comments)
	return out
}

func jiraSquawkTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newJiraSquawkForwarder(daemon *mockJiraSquawkDaemon, jira *mockJiraSquawkJira) *JiraSquawkForwarder {
	return NewJiraSquawkForwarder(JiraSquawkForwarderConfig{
		Daemon: daemon,
		Jira:   jira,
		Logger: jiraSquawkTestLogger(),
	})
}

func TestJiraSquawkForwarder_ForwardsPETicket(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "PE-1234"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk1",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Build complete, all tests pass.",
		},
	})
	f.handleClosed(context.Background(), data)

	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 JIRA comment, got %d", len(comments))
	}
	if comments[0].Key != "PE-1234" {
		t.Errorf("expected key PE-1234, got %q", comments[0].Key)
	}
	if !strings.Contains(comments[0].Text, "test-agent") {
		t.Errorf("expected agent name in comment, got %q", comments[0].Text)
	}
	if !strings.Contains(comments[0].Text, "Build complete") {
		t.Errorf("expected squawk text in comment, got %q", comments[0].Text)
	}
}

func TestJiraSquawkForwarder_BlocksFSMProject(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "FSM-100"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-fsm",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Should not appear on FSM ticket",
		},
	})
	f.handleClosed(context.Background(), data)

	if len(jira.getComments()) != 0 {
		t.Error("expected no JIRA comment for FSM project")
	}
}

func TestJiraSquawkForwarder_BlocksUnauthorizedProject(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "ACME-500"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-acme",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Should not appear on ACME ticket",
		},
	})
	f.handleClosed(context.Background(), data)

	if len(jira.getComments()) != 0 {
		t.Error("expected no JIRA comment for unauthorized project")
	}
}

func TestJiraSquawkForwarder_IgnoresNonMessageBeads(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-task1",
		Type:   "task",
		Labels: []string{"bug"},
	})
	f.handleClosed(context.Background(), data)

	if f.alreadySeen("bd-task1") {
		t.Error("expected task bead to be ignored")
	}
}

func TestJiraSquawkForwarder_IgnoresMessageWithoutSquawkLabel(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-msg1",
		Type:   "message",
		Labels: []string{"from:test-agent"},
	})
	f.handleClosed(context.Background(), data)

	if f.alreadySeen("bd-msg1") {
		t.Error("expected non-squawk message to be ignored")
	}
}

func TestJiraSquawkForwarder_SkipsAgentWithoutJiraKey(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{}, // no jira_key
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-no-jira",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "No JIRA binding",
		},
	})
	f.handleClosed(context.Background(), data)

	if len(jira.getComments()) != 0 {
		t.Error("expected no JIRA comment when agent has no jira_key")
	}
}

func TestJiraSquawkForwarder_Dedup(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "PE-1234"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-dup",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "First call",
		},
	})

	// First call should forward.
	f.handleClosed(context.Background(), data)
	if len(jira.getComments()) != 1 {
		t.Fatalf("expected 1 comment after first call, got %d", len(jira.getComments()))
	}

	// Second call should be deduped.
	f.handleClosed(context.Background(), data)
	if len(jira.getComments()) != 1 {
		t.Fatalf("expected still 1 comment after dedup, got %d", len(jira.getComments()))
	}
}

func TestJiraSquawkForwarder_Throttle(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "PE-1234"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	// First squawk.
	data1 := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-t1",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "First squawk",
		},
	})
	f.handleClosed(context.Background(), data1)
	if len(jira.getComments()) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(jira.getComments()))
	}

	// Second squawk from same agent (different bead) — should be throttled.
	data2 := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-t2",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Second squawk",
		},
	})
	f.handleClosed(context.Background(), data2)
	if len(jira.getComments()) != 1 {
		t.Fatalf("expected still 1 comment (throttled), got %d", len(jira.getComments()))
	}
}

func TestJiraSquawkForwarder_TruncatesLongMessage(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "PE-1234"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	longText := strings.Repeat("x", 5000)
	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-long",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         longText,
		},
	})
	f.handleClosed(context.Background(), data)

	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	// The body includes agent prefix, so just check it's not the full 5000 chars.
	if len(comments[0].Text) > jiraSquawkMaxLen+100 {
		t.Errorf("expected truncated comment, got length %d", len(comments[0].Text))
	}
}

func TestJiraSquawkForwarder_AcceptsSayLabel(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "PE-1234"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-say1",
		Type:   "message",
		Labels: []string{"say", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Legacy say message",
		},
	})
	f.handleClosed(context.Background(), data)

	if len(jira.getComments()) != 1 {
		t.Fatalf("expected 1 comment for 'say' label, got %d", len(jira.getComments()))
	}
}

func TestJiraSquawkForwarder_JiraKeyFromNotes(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{},           // no jira_key in fields
				Notes:  "jira_key: PE-5678\nfoo: bar", // but present in notes
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-notes",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Found via notes",
		},
	})
	f.handleClosed(context.Background(), data)

	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if comments[0].Key != "PE-5678" {
		t.Errorf("expected key PE-5678, got %q", comments[0].Key)
	}
}

// TestJiraSquawkForwarder_StampsOperatorAttribution covers epic ka-0000004904
// decision #4 / ka-0000006003: the forwarded squawk comment must carry an
// "Acted by <operator>" line derived from the agent bead's operated_by, with
// the daemon-resolved display name when available and the RAW operated_by
// always present (the E2E harness greps for it verbatim).
func TestJiraSquawkForwarder_StampsOperatorAttribution(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:                "bd-test-agent",
				Title:             "test-agent",
				Fields:            map[string]string{"jira_key": "PE-1234"},
				OperatedBy:        "slack:U0OPERATOR1",
				OperatedByDisplay: "Matt Baker",
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-attr",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{"source_agent": "test-agent", "text": "Build complete."},
	})
	f.handleClosed(context.Background(), data)

	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	body := comments[0].Text
	if !strings.Contains(body, "Acted by Matt Baker (slack:U0OPERATOR1)") {
		t.Errorf("expected attribution with display + raw id, got %q", body)
	}
	// Raw operated_by must appear verbatim for the harness grep.
	if !strings.Contains(body, "slack:U0OPERATOR1") {
		t.Errorf("expected raw operated_by in body, got %q", body)
	}
	// Original squawk content must still be present.
	if !strings.Contains(body, "Build complete.") {
		t.Errorf("expected squawk text retained, got %q", body)
	}
}

// TestJiraSquawkForwarder_AttributionFallsBackToFieldAndRaw covers the
// raw-fallback branch: a typed OperatedBy absent but operated_by present in
// Fields, and no resolved display name.
func TestJiraSquawkForwarder_AttributionFallsBackToFieldAndRaw(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "PE-1234", "operated_by": "oidc:matt@acme.example"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-attr-raw",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{"source_agent": "test-agent", "text": "Ping."},
	})
	f.handleClosed(context.Background(), data)

	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if !strings.Contains(comments[0].Text, "Acted by oidc:matt@acme.example") {
		t.Errorf("expected raw-fallback attribution, got %q", comments[0].Text)
	}
}

// TestJiraSquawkForwarder_NoAttributionWhenNoOperator covers the omit case: an
// agent bead with no human operator must not gain a fabricated attribution.
func TestJiraSquawkForwarder_NoAttributionWhenNoOperator(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:         "bd-test-agent",
				Title:      "test-agent",
				Fields:     map[string]string{"jira_key": "PE-1234"},
				OperatedBy: "fleet:slack-bridge",
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-noattr",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{"source_agent": "test-agent", "text": "Heartbeat."},
	})
	f.handleClosed(context.Background(), data)

	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
	if strings.Contains(comments[0].Text, "Acted by") {
		t.Errorf("expected no attribution for a non-human operator, got %q", comments[0].Text)
	}
}

// TestJiraSquawkForwarder_SuppressedOnQuietTicket pins the mention-time --quiet
// intent hint (design-doc F-E, epic ka-0000007401): when the acting agent bead
// carries jira_quiet=true the automatic squawk mirror is skipped, even on a
// writable project (PE). Without the field the identical squawk would forward
// (TestJiraSquawkForwarder_ForwardsPETicket).
func TestJiraSquawkForwarder_SuppressedOnQuietTicket(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "PE-1234", "jira_quiet": "true"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-quiet",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Should not mirror on a --quiet ticket.",
		},
	})
	f.handleClosed(context.Background(), data)

	if len(jira.getComments()) != 0 {
		t.Error("expected no JIRA comment on a --quiet ticket")
	}
}
