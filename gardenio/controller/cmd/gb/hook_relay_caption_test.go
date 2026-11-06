package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The relay's caption fields (bird captions spec 6.2, gd-gCHKhIROvB): tool_description and tool_file_path are copied
// out of tool_input before truncateAny cuts it to 1024 bytes, so a long Bash command or a long Write no longer costs the
// hub its caption.

func TestBuildRelayEvent_CaptionFieldsSurviveTruncation(t *testing.T) {
	long := strings.Repeat("x", 2000)
	for _, c := range []struct {
		name, tool         string
		in                 map[string]any
		wantDesc, wantPath string
	}{
		{"Bash, command over 1024 bytes", "Bash", map[string]any{"command": "echo " + long, "description": "Trial-merge main into #1282"}, "Trial-merge main into #1282", ""},
		{"Monitor", "Monitor", map[string]any{"command": long, "description": "Watch CI"}, "Watch CI", ""},
		{"Agent", "Agent", map[string]any{"description": "Explore transcripts", "prompt": long}, "Explore transcripts", ""},
		{"Write, content over 1024 bytes", "Write", map[string]any{"content": long, "file_path": "/home/agent/work/a.go"}, "", "/home/agent/work/a.go"},
		{"Edit", "Edit", map[string]any{"file_path": "/w/b.go", "old_string": long, "new_string": "y"}, "", "/w/b.go"},
		{"NotebookEdit", "NotebookEdit", map[string]any{"notebook_path": "/w/n.ipynb", "new_source": long}, "", "/w/n.ipynb"},
		{"Grep: neither", "Grep", map[string]any{"pattern": "x", "description": "not copied"}, "", ""},
		{"Bash with no description", "Bash", map[string]any{"command": "ls"}, "", ""},
		{"secret in the description", "Bash", map[string]any{"command": "x", "description": "Use token=hunter2abc and sk-ant-PLANTEDKEY1"}, "Use token=[REDACTED] and [REDACTED]", ""},
	} {
		evt, _, err := buildRelayEvent(map[string]any{"hook_event_name": "PreToolUse", "session_id": "s", "tool_name": c.tool, "tool_input": c.in}, "w", "gd-a")
		if err != nil {
			t.Fatal(err)
		}
		if evt.ToolDescription != c.wantDesc || evt.ToolFilePath != c.wantPath {
			t.Errorf("%s: tool_description %q tool_file_path %q, want %q %q", c.name, evt.ToolDescription, evt.ToolFilePath, c.wantDesc, c.wantPath)
		}
	}

	// The long Bash input is still cut (a string), and the description it lost is in its own field.
	evt, _, _ := buildRelayEvent(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": long, "description": "Keep me"}}, "w", "gd-a")
	if s, ok := evt.ToolInput.(string); !ok || strings.Contains(s, "Keep me") {
		t.Fatalf("tool_input should be cut before the description: %T %.60v", evt.ToolInput, evt.ToolInput)
	}
	raw, _ := json.Marshal(evt)
	if !strings.Contains(string(raw), `"tool_description":"Keep me"`) {
		t.Errorf("payload lacks tool_description: %.200s", raw)
	}
}

func TestBuildRelayEvent_CaptionFieldsCapAndScope(t *testing.T) {
	desc := strings.Repeat("é", 200) // 400 bytes
	evt, _, _ := buildRelayEvent(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "x", "description": desc}}, "w", "gd-a")
	if n := len(evt.ToolDescription); n > toolCaptionFieldMax || n < toolCaptionFieldMax-1 || !strings.HasPrefix(desc, evt.ToolDescription) {
		t.Errorf("tool_description is %d bytes, want at most %d on a rune boundary", n, toolCaptionFieldMax)
	}

	// Only a call's start carries them: the caption is made when the call starts.
	for _, ev := range []string{"PostToolUse", "PostToolUseFailure"} {
		evt, _, _ := buildRelayEvent(map[string]any{"hook_event_name": ev, "tool_name": "Bash", "tool_input": map[string]any{"command": "x", "description": "d"}}, "w", "gd-a")
		raw, _ := json.Marshal(evt)
		if strings.Contains(string(raw), "tool_description") || strings.Contains(string(raw), "tool_file_path") {
			t.Errorf("%s carries a caption field: %s", ev, raw)
		}
	}
}

func TestBuildRelayEvent_CaptionFieldsGeminiAndCodex(t *testing.T) {
	t.Setenv(envGardenioHarness, "")
	// Gemini's run_shell_command has a description (spec 5.2 row 1).
	evt, _, err := buildRelayEvent(geminiPayload(t, "headless-BeforeTool.json"), "w", "gd-a")
	if err != nil {
		t.Fatal(err)
	}
	if evt.Event != "PreToolUse" || evt.ToolDescription != "mock tool call" {
		t.Errorf("Gemini BeforeTool: event %q tool_description %q", evt.Event, evt.ToolDescription)
	}
	// Codex's shell call has none: the hub leaves it to the Haiku fallback.
	for _, in := range codexPayloads(t) {
		if in["hook_event_name"] != "PreToolUse" {
			continue
		}
		evt, _, err := buildRelayEvent(in, "w", "gd-a")
		if err != nil {
			t.Fatal(err)
		}
		if evt.ToolName != "Bash" || evt.ToolDescription != "" || evt.ToolFilePath != "" {
			t.Errorf("Codex PreToolUse: tool %q tool_description %q tool_file_path %q", evt.ToolName, evt.ToolDescription, evt.ToolFilePath)
		}
	}
}
