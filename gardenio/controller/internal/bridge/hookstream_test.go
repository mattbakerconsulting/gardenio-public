package bridge

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestParseBashOutput_GitCommit(t *testing.T) {
	output := `[main abc1234] fix(bridge): handle nil pointer
 2 files changed, 15 insertions(+), 3 deletions(-)`

	got := parseBashOutput(output)
	if !contains(got, "Committed abc1234") {
		t.Errorf("expected commit hash, got %q", got)
	}
	if !contains(got, "handle nil pointer") {
		t.Errorf("expected commit message, got %q", got)
	}
}

func TestParseBashOutput_GitPush(t *testing.T) {
	output := `To https://github.com/org/repo.git
   abc1234..def5678  main -> main`

	got := parseBashOutput(output)
	if !contains(got, "Pushed") {
		t.Errorf("expected push detection, got %q", got)
	}
	if !contains(got, "main") {
		t.Errorf("expected branch info, got %q", got)
	}
}

func TestParseBashOutput_GitCommitAndPush(t *testing.T) {
	output := `[fix/streaming abc1234] feat: add streaming support
 5 files changed, 200 insertions(+)
To https://github.com/org/repo.git
   111aaa..abc1234  fix/streaming -> fix/streaming`

	got := parseBashOutput(output)
	if !contains(got, "Committed abc1234") {
		t.Errorf("expected commit, got %q", got)
	}
	if !contains(got, "Pushed") {
		t.Errorf("expected push, got %q", got)
	}
}

func TestParseBashOutput_TestPassed(t *testing.T) {
	output := `ok  	gardenio/controller/internal/bridge	0.015s`

	got := parseBashOutput(output)
	if !contains(got, "ok") {
		t.Errorf("expected test passed, got %q", got)
	}
}

func TestParseBashOutput_TestFailed(t *testing.T) {
	output := `--- FAIL: TestSomething (0.00s)
FAIL	gardenio/controller/internal/bridge	0.015s`

	got := parseBashOutput(output)
	if !contains(got, "FAIL") {
		t.Errorf("expected test failed, got %q", got)
	}
}

func TestParseBashOutput_NoMatch(t *testing.T) {
	output := "ls -la\ntotal 42\ndrwxr-xr-x 5 user user 4096 Jan 1 00:00 ."

	got := parseBashOutput(output)
	if got != "" {
		t.Errorf("expected empty for non-matching output, got %q", got)
	}
}

func TestParseBashOutput_NilResponse(t *testing.T) {
	got := parseBashOutput(nil)
	if got != "" {
		t.Errorf("expected empty for nil, got %q", got)
	}
}

func TestParseBashOutput_MapResponse(t *testing.T) {
	resp := map[string]any{
		"stdout": "[main abc1234] fix something\n 1 file changed",
	}

	got := parseBashOutput(resp)
	if !contains(got, "Committed abc1234") {
		t.Errorf("expected commit from map response, got %q", got)
	}
}

func TestToolResponseString(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"string", "hello", "hello"},
		{"map_stdout", map[string]any{"stdout": "out"}, "out"},
		{"map_output", map[string]any{"output": "out"}, "out"},
		{"nil", nil, ""},
		{"int", 42, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolResponseString(tt.in)
			if got != tt.want {
				t.Errorf("toolResponseString(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractToolLabel(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    any
		want     string
	}{
		{"bash_description", "Bash", map[string]any{"description": "Show working tree status", "command": "git status"}, "Show working tree status"},
		{"bash_command_fallback", "Bash", map[string]any{"command": "git log --oneline -5"}, "git log --oneline -5"},
		{"bash_command_truncated", "Bash", map[string]any{"command": "some very long command that exceeds the fifty character limit for display"}, "some very long command that exceeds the fifty char..."},
		{"bash_cd_strip", "Bash", map[string]any{"command": "cd /foo && git status"}, "git status"},
		{"bash_nil_input", "Bash", nil, ""},
		{"bash_empty_input", "Bash", map[string]any{}, ""},
		{"read_file", "Read", map[string]any{"file_path": "/home/user/project/settings.json"}, "settings.json"},
		{"edit_file", "Edit", map[string]any{"file_path": "/app/src/main.go"}, "main.go"},
		{"grep_pattern", "Grep", map[string]any{"pattern": "HandleToolEvent"}, "HandleToolEvent"},
		{"glob_pattern", "Glob", map[string]any{"pattern": "**/*.go"}, "**/*.go"},
		{"agent_description", "Agent", map[string]any{"description": "Search for doots code"}, "Search for doots code"},
		{"webfetch_url", "WebFetch", map[string]any{"url": "https://example.com/api/docs"}, "https://example.com/api/docs"},
		{"websearch_query", "WebSearch", map[string]any{"query": "React hooks documentation"}, "React hooks documentation"},
		{"lsp_operation", "LSP", map[string]any{"operation": "goToDefinition", "filePath": "/app/src/main.go"}, "goToDefinition main.go"},
		{"lsp_operation_only", "LSP", map[string]any{"operation": "findReferences"}, "findReferences"},
		{"notebook_edit", "NotebookEdit", map[string]any{"notebook_path": "/home/user/analysis.ipynb"}, "analysis.ipynb"},
		{"skill", "Skill", map[string]any{"skill": "commit"}, "commit"},
		{"send_message", "SendMessage", map[string]any{"to": "researcher"}, "to: researcher"},
		{"mcp_tool_query", "mcp__mezmo__analyze_logs", map[string]any{"query": "level:error app:api"}, "level:error app:api"},
		{"mcp_tool_url", "mcp__playwright__browser_navigate", map[string]any{"url": "https://example.com"}, "https://example.com"},
		{"mcp_tool_description", "mcp__custom__tool", map[string]any{"description": "Fetch user data"}, "Fetch user data"},
		{"unknown_with_generic_field", "CustomTool", map[string]any{"query": "search term"}, "search term"},
		{"unknown_tool", "Unknown", map[string]any{"foo": "bar"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractToolLabel(tt.toolName, tt.input)
			if got != tt.want {
				t.Errorf("extractToolLabel(%q, %v) = %q, want %q", tt.toolName, tt.input, got, tt.want)
			}
		})
	}
}

func TestHookStream_LastEventAt(t *testing.T) {
	h := &HookStream{}
	if got := h.LastEventAt(); !got.IsZero() {
		t.Errorf("LastEventAt before any message = %v, want zero", got)
	}

	before := time.Now()
	// handleMessage early-returns on malformed JSON but still updates the
	// last-event timestamp — the field tracks NATS delivery, not parse success.
	h.handleMessage(&nats.Msg{Subject: "hooks.x.PreToolUse", Data: []byte(`{`)})
	got := h.LastEventAt()
	if got.IsZero() {
		t.Fatalf("LastEventAt after handleMessage is zero")
	}
	if got.Before(before) {
		t.Errorf("LastEventAt = %v, want >= %v", got, before)
	}

	h2 := &HookStream{}
	h2.handleMessageForward(&nats.Msg{Subject: "coop.events.messages.text", Data: []byte(`{`)})
	if h2.LastEventAt().IsZero() {
		t.Errorf("LastEventAt after handleMessageForward is zero")
	}
}

func TestAgentFromSubject(t *testing.T) {
	tests := []struct {
		subject string
		want    string
	}{
		{"hooks.worker-1.PreToolUse", "worker-1"},
		{"hooks.my-agent.PostToolUse", "my-agent"},
		{"hooks.short", ""},
		{"hooks", ""},
	}
	for _, tt := range tests {
		got := agentFromSubject(tt.subject)
		if got != tt.want {
			t.Errorf("agentFromSubject(%q) = %q, want %q", tt.subject, got, tt.want)
		}
	}
}
