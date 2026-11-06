package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTruncateAny_NilInput(t *testing.T) {
	result := truncateAny(nil, 1024)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestTruncateAny_SmallInput(t *testing.T) {
	input := map[string]string{"command": "go test ./..."}
	result := truncateAny(input, 1024)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	// Should return the original value unchanged.
	m, ok := result.(map[string]string)
	if !ok {
		t.Fatalf("expected map[string]string, got %T", result)
	}
	if m["command"] != "go test ./..." {
		t.Errorf("unexpected command: %s", m["command"])
	}
}

func TestTruncateAny_LargeInput(t *testing.T) {
	// Create a value that exceeds 100 bytes when marshaled.
	large := map[string]string{"data": string(make([]byte, 200))}
	result := truncateAny(large, 100)
	if result == nil {
		t.Fatal("expected non-nil result for large input")
	}
	s, ok := result.(string)
	if !ok {
		t.Fatalf("expected string (truncated), got %T", result)
	}
	if len(s) > 104 { // 100 bytes + "..."
		t.Errorf("truncated result too long: %d bytes", len(s))
	}
	if s[len(s)-3:] != "..." {
		t.Errorf("expected truncated string to end with '...', got %q", s[len(s)-3:])
	}
}

func TestSanitizeSubject(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"worker-1", "worker-1"},
		{"worker.1", "worker_1"},
		{"my agent", "my_agent"},
		{"clean", "clean"},
		{"a.b c", "a_b_c"},
	}
	for _, tc := range tests {
		got := sanitizeSubject(tc.input)
		if got != tc.expected {
			t.Errorf("sanitizeSubject(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestBuildRelayEvent_PreToolUse(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "sess-123",
		"cwd":             "/home/agent/workspace",
		"tool_name":       "Bash",
		"tool_use_id":     "toolu_014m2",
		"prompt_id":       "prompt-abc",
		"permission_mode": "default",
		"tool_input": map[string]any{
			"command":     "go test ./...",
			"description": "Run tests",
		},
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Event != "PreToolUse" {
		t.Errorf("expected event PreToolUse, got %s", evt.Event)
	}
	if evt.Agent != "worker-1" {
		t.Errorf("expected agent worker-1, got %s", evt.Agent)
	}
	if evt.SessionID != "sess-123" {
		t.Errorf("expected session_id sess-123, got %s", evt.SessionID)
	}
	if evt.ToolName != "Bash" {
		t.Errorf("expected tool_name Bash, got %s", evt.ToolName)
	}
	if evt.ToolInput == nil {
		t.Error("expected tool_input to be set")
	}
	// tool_use_id must be stamped on PreToolUse too, not just PostToolUse —
	// it is the pairing key a consumer needs to match this call's eventual
	// PostToolUse without guessing from tool name + arrival order.
	if evt.ToolUseID != "toolu_014m2" {
		t.Errorf("expected tool_use_id toolu_014m2, got %s", evt.ToolUseID)
	}
	if evt.PromptID != "prompt-abc" {
		t.Errorf("expected prompt_id prompt-abc, got %s", evt.PromptID)
	}
	if evt.PermissionMode != "default" {
		t.Errorf("expected permission_mode default, got %s", evt.PermissionMode)
	}
	if subject != "hooks.worker-1.PreToolUse" {
		t.Errorf("expected subject hooks.worker-1.PreToolUse, got %s", subject)
	}
}

func TestBuildRelayEvent_PostToolUse(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "PostToolUse",
		"session_id":      "sess-456",
		"tool_name":       "Read",
		"tool_input":      map[string]any{"file_path": "/tmp/test.go"},
		"tool_response":   map[string]any{"success": true, "stdout": strings.Repeat("x", 3000), "stderr": "warn"},
		"tool_use_id":     "toolu_014m2",
		"prompt_id":       "prompt-abc",
		"permission_mode": "default",
		"duration_ms":     float64(415),
	}

	evt, _, err := buildRelayEvent(input, "worker-2", "gd-TestAgent02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.ToolResponse == nil {
		t.Error("expected tool_response to be set")
	}
	// Acceptance criterion (gd-lGvUZtjpRQ): duration_ms and tool_use_id must
	// both survive buildRelayEvent — they were previously dropped on the floor.
	if evt.DurationMs != 415 {
		t.Errorf("expected duration_ms 415, got %d", evt.DurationMs)
	}
	if evt.DurationSource != "claude" {
		t.Errorf("expected duration_source claude, got %q", evt.DurationSource)
	}
	if evt.ToolUseID != "toolu_014m2" {
		t.Errorf("expected tool_use_id toolu_014m2, got %s", evt.ToolUseID)
	}
	if evt.PromptID != "prompt-abc" {
		t.Errorf("expected prompt_id prompt-abc, got %s", evt.PromptID)
	}
	if evt.PermissionMode != "default" {
		t.Errorf("expected permission_mode default, got %s", evt.PermissionMode)
	}
	// output_bytes is the response's text before the 1024-byte cut (gd-qlk31NBV6o).
	if evt.OutputBytes != 3004 {
		t.Errorf("expected output_bytes 3004, got %d", evt.OutputBytes)
	}
}

// TestBuildRelayEvent_PostToolUse_NoDurationMs covers older Claude Code
// versions that don't ship duration_ms: it must stay absent (not zero-valued
// and stamped "claude") so a consumer can tell the difference between "no
// measurement" and "measured 0ms".
func TestBuildRelayEvent_PostToolUse_NoDurationMs(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "PostToolUse",
		"session_id":      "sess-456",
		"tool_name":       "Read",
	}

	evt, _, err := buildRelayEvent(input, "worker-2", "gd-TestAgent02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.DurationMs != 0 {
		t.Errorf("expected duration_ms 0 (unset), got %d", evt.DurationMs)
	}
	if evt.DurationSource != "" {
		t.Errorf("expected duration_source unset, got %q", evt.DurationSource)
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"duration_ms", "duration_source"} {
		if _, present := wire[key]; present {
			t.Errorf("expected %q to be omitted from the wire payload, got: %s", key, payload)
		}
	}
}

func TestBuildRelayEvent_SessionStart(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "SessionStart",
		"session_id":      "sess-789",
		"source":          "startup",
	}

	evt, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Source != "startup" {
		t.Errorf("expected source startup, got %s", evt.Source)
	}
}

func TestBuildRelayEvent_Stop(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "Stop",
		"session_id":      "sess-abc",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Event != "Stop" {
		t.Errorf("expected event Stop, got %s", evt.Event)
	}
	if subject != "hooks.worker-1.Stop" {
		t.Errorf("expected subject hooks.worker-1.Stop, got %s", subject)
	}
}

func TestBuildRelayEvent_PostToolUseFailure(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "PostToolUseFailure",
		"session_id":      "sess-fail",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "npm test"},
		"error":           "command exited with code 1",
		"error_type":      "tool_error",
		"tool_use_id":     "toolu_fail1",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Event != "PostToolUseFailure" {
		t.Errorf("expected event PostToolUseFailure, got %s", evt.Event)
	}
	if evt.ToolName != "Bash" {
		t.Errorf("expected tool_name Bash, got %s", evt.ToolName)
	}
	if evt.Error != "command exited with code 1" {
		t.Errorf("expected error message, got %s", evt.Error)
	}
	if evt.ErrorType != "tool_error" {
		t.Errorf("expected error_type tool_error, got %s", evt.ErrorType)
	}
	if evt.ToolUseID != "toolu_fail1" {
		t.Errorf("expected tool_use_id toolu_fail1, got %s", evt.ToolUseID)
	}
	if subject != "hooks.worker-1.PostToolUseFailure" {
		t.Errorf("expected subject hooks.worker-1.PostToolUseFailure, got %s", subject)
	}
}

func TestBuildRelayEvent_StopFailure(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "StopFailure",
		"session_id":      "sess-stopfail",
		"error":           "stop hook timed out",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Event != "StopFailure" {
		t.Errorf("expected event StopFailure, got %s", evt.Event)
	}
	if evt.Error != "stop hook timed out" {
		t.Errorf("expected error 'stop hook timed out', got %s", evt.Error)
	}
	if subject != "hooks.worker-1.StopFailure" {
		t.Errorf("expected subject hooks.worker-1.StopFailure, got %s", subject)
	}
}

func TestBuildRelayEvent_SubagentStart(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "SubagentStart",
		"session_id":      "sess-sub",
		"agent_id":        "agent-xyz",
		"agent_type":      "Explore",
	}

	evt, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.SubagentID != "agent-xyz" {
		t.Errorf("expected subagent_id agent-xyz, got %s", evt.SubagentID)
	}
	if evt.SubagentType != "Explore" {
		t.Errorf("expected subagent_type Explore, got %s", evt.SubagentType)
	}
	// The whole point of stamping the bead id is subagent lineage: a consumer
	// must be able to attach this SubagentStart to the agent bead that spawned
	// it. evt.Agent ("worker-1") is a display name and cannot do that job.
	if evt.AgentBeadID != "gd-TestAgent01" {
		t.Errorf("expected agent_bead_id gd-TestAgent01, got %q", evt.AgentBeadID)
	}
}

// TestBuildRelayEvent_AgentBeadIDStamped checks the bead id survives onto the
// wire payload, not just the struct — a missing json tag would pass a field
// assertion while shipping nothing to consumers.
func TestBuildRelayEvent_AgentBeadIDStamped(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "sess-1",
		"tool_name":       "Bash",
	}

	evt, _, err := buildRelayEvent(input, "worker-1", "gd-2bZJkQ8VZR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.AgentBeadID != "gd-2bZJkQ8VZR" {
		t.Fatalf("expected agent_bead_id gd-2bZJkQ8VZR, got %q", evt.AgentBeadID)
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := wire["agent_bead_id"]; got != "gd-2bZJkQ8VZR" {
		t.Errorf("expected agent_bead_id on the wire, got %v (payload: %s)", got, payload)
	}
}

// TestBuildRelayEvent_AgentBeadIDOmittedWhenUnset guards the omitempty tag.
// Off-cluster runs (a developer laptop) have no agent bead, and consumers must
// see the key absent rather than an empty string that looks like a real id.
func TestBuildRelayEvent_AgentBeadIDOmittedWhenUnset(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "sess-1",
		"tool_name":       "Bash",
	}

	evt, _, err := buildRelayEvent(input, "worker-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.AgentBeadID != "" {
		t.Errorf("expected empty agent_bead_id, got %q", evt.AgentBeadID)
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := wire["agent_bead_id"]; present {
		t.Errorf("expected agent_bead_id to be omitted, payload: %s", payload)
	}
}

// TestResolveAgentBeadID pins the precedence between the two env vars that
// carry the agent bead id, matching every other gb call site.
func TestResolveAgentBeadID(t *testing.T) {
	tests := []struct {
		name       string
		kdAgentID  string
		boatBeadID string
		want       string
	}{
		{"both unset", "", "", ""},
		{"kd only", "gd-kd", "", "gd-kd"},
		{"boat only", "", "gd-boat", "gd-boat"},
		{"kd wins over boat", "gd-kd", "gd-boat", "gd-kd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KD_AGENT_ID", tt.kdAgentID)
			t.Setenv("BOAT_AGENT_BEAD_ID", tt.boatBeadID)
			if got := resolveAgentBeadID(); got != tt.want {
				t.Errorf("resolveAgentBeadID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildRelayEvent_TeammateIdle(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "TeammateIdle",
		"session_id":      "sess-team",
		"teammate_id":     "teammate-abc",
		"teammate_type":   "research",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.TeammateID != "teammate-abc" {
		t.Errorf("expected teammate_id teammate-abc, got %s", evt.TeammateID)
	}
	if evt.TeammateType != "research" {
		t.Errorf("expected teammate_type research, got %s", evt.TeammateType)
	}
	if subject != "hooks.worker-1.TeammateIdle" {
		t.Errorf("expected subject hooks.worker-1.TeammateIdle, got %s", subject)
	}
}

func TestBuildRelayEvent_TaskCompleted(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "TaskCompleted",
		"session_id":      "sess-task",
		"task_id":         "task-123",
		"task_subject":    "Fix login bug",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.TaskID != "task-123" {
		t.Errorf("expected task_id task-123, got %s", evt.TaskID)
	}
	if evt.TaskSubject != "Fix login bug" {
		t.Errorf("expected task_subject 'Fix login bug', got %s", evt.TaskSubject)
	}
	if subject != "hooks.worker-1.TaskCompleted" {
		t.Errorf("expected subject hooks.worker-1.TaskCompleted, got %s", subject)
	}
}

func TestBuildRelayEvent_TaskCreated(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "TaskCreated",
		"session_id":      "sess-tc",
		"task_id":         "task-456",
		"task_subject":    "Refactor auth module",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.TaskID != "task-456" {
		t.Errorf("expected task_id task-456, got %s", evt.TaskID)
	}
	if evt.TaskSubject != "Refactor auth module" {
		t.Errorf("expected task_subject 'Refactor auth module', got %s", evt.TaskSubject)
	}
	if subject != "hooks.worker-1.TaskCreated" {
		t.Errorf("expected subject hooks.worker-1.TaskCreated, got %s", subject)
	}
}

func TestBuildRelayEvent_Notification(t *testing.T) {
	input := map[string]any{
		"hook_event_name":   "Notification",
		"session_id":        "sess-notif",
		"notification_type": "idle_prompt",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.NotificationType != "idle_prompt" {
		t.Errorf("expected notification_type idle_prompt, got %s", evt.NotificationType)
	}
	if subject != "hooks.worker-1.Notification" {
		t.Errorf("expected subject hooks.worker-1.Notification, got %s", subject)
	}
}

func TestBuildRelayEvent_CwdChanged(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "CwdChanged",
		"session_id":      "sess-cwd",
		"old_cwd":         "/home/agent/workspace",
		"new_cwd":         "/home/agent/worktrees/gd-abc",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.OldCWD != "/home/agent/workspace" {
		t.Errorf("expected old_cwd /home/agent/workspace, got %s", evt.OldCWD)
	}
	if evt.NewCWD != "/home/agent/worktrees/gd-abc" {
		t.Errorf("expected new_cwd, got %s", evt.NewCWD)
	}
	if subject != "hooks.worker-1.CwdChanged" {
		t.Errorf("expected subject hooks.worker-1.CwdChanged, got %s", subject)
	}
}

func TestBuildRelayEvent_FileChanged(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "FileChanged",
		"session_id":      "sess-fc",
		"file_path":       "/home/agent/workspace/main.go",
		"change_type":     "modified",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.FilePath != "/home/agent/workspace/main.go" {
		t.Errorf("expected file_path, got %s", evt.FilePath)
	}
	if evt.ChangeType != "modified" {
		t.Errorf("expected change_type modified, got %s", evt.ChangeType)
	}
	if subject != "hooks.worker-1.FileChanged" {
		t.Errorf("expected subject hooks.worker-1.FileChanged, got %s", subject)
	}
}

func TestBuildRelayEvent_PermissionRequest(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "PermissionRequest",
		"session_id":      "sess-perm",
		"tool_name":       "Bash",
		"request_id":      "req-456",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Event != "PermissionRequest" {
		t.Errorf("expected event PermissionRequest, got %s", evt.Event)
	}
	if evt.ToolName != "Bash" {
		t.Errorf("expected tool_name Bash, got %s", evt.ToolName)
	}
	if evt.RequestID != "req-456" {
		t.Errorf("expected request_id req-456, got %s", evt.RequestID)
	}
	if subject != "hooks.worker-1.PermissionRequest" {
		t.Errorf("expected subject hooks.worker-1.PermissionRequest, got %s", subject)
	}
}

func TestBuildRelayEvent_UnknownEvent(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "UnknownEvent",
		"session_id":      "sess-unk",
	}

	_, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err == nil {
		t.Error("expected error for unknown event, got nil")
	}
}

func TestBuildRelayEvent_EmptyEventName(t *testing.T) {
	input := map[string]any{
		"session_id": "sess-empty",
	}

	_, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err == nil {
		t.Error("expected error for empty event name, got nil")
	}
}

func TestBuildRelayEvent_CompactEvents(t *testing.T) {
	for _, eventName := range []string{"PreCompact", "PostCompact"} {
		t.Run(eventName, func(t *testing.T) {
			input := map[string]any{
				"hook_event_name": eventName,
				"session_id":      "sess-compact",
				"trigger":         "auto",
			}

			evt, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if evt.Trigger != "auto" {
				t.Errorf("expected trigger auto, got %s", evt.Trigger)
			}
			if evt.Event != eventName {
				t.Errorf("expected event %s, got %s", eventName, evt.Event)
			}
		})
	}
}

func TestBuildRelayEvent_SessionEnd(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "SessionEnd",
		"session_id":      "sess-end",
		"reason":          "clear",
	}

	evt, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Reason != "clear" {
		t.Errorf("expected reason clear, got %s", evt.Reason)
	}
}

func TestBuildRelayEvent_ConfigChange(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "ConfigChange",
		"session_id":      "sess-cfg",
		"source":          "project_settings",
		"file_path":       ".claude/settings.json",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.ConfigSource != "project_settings" {
		t.Errorf("expected config_source project_settings, got %s", evt.ConfigSource)
	}
	if evt.FilePath != ".claude/settings.json" {
		t.Errorf("expected file_path .claude/settings.json, got %s", evt.FilePath)
	}
	if subject != "hooks.worker-1.ConfigChange" {
		t.Errorf("expected subject hooks.worker-1.ConfigChange, got %s", subject)
	}
}

func TestBuildRelayEvent_InstructionsLoaded(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "InstructionsLoaded",
		"session_id":      "sess-inst",
		"file_path":       "/home/agent/workspace/CLAUDE.md",
		"memory_type":     "Project",
		"load_reason":     "session_start",
	}

	evt, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.InstructionFilePath != "/home/agent/workspace/CLAUDE.md" {
		t.Errorf("expected instruction_file_path, got %s", evt.InstructionFilePath)
	}
	if evt.MemoryType != "Project" {
		t.Errorf("expected memory_type Project, got %s", evt.MemoryType)
	}
	if evt.LoadReason != "session_start" {
		t.Errorf("expected load_reason session_start, got %s", evt.LoadReason)
	}
}

func TestBuildRelayEvent_Elicitation(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "Elicitation",
		"session_id":      "sess-el",
		"mcp_server_name": "mezmo",
		"message":         "Please provide API key",
		"elicitation_id":  "el-123",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.MCPServerName != "mezmo" {
		t.Errorf("expected mcp_server_name mezmo, got %s", evt.MCPServerName)
	}
	if evt.Message != "Please provide API key" {
		t.Errorf("expected message, got %s", evt.Message)
	}
	if evt.ElicitationID != "el-123" {
		t.Errorf("expected elicitation_id el-123, got %s", evt.ElicitationID)
	}
	if subject != "hooks.worker-1.Elicitation" {
		t.Errorf("expected subject hooks.worker-1.Elicitation, got %s", subject)
	}
}

func TestBuildRelayEvent_ElicitationResult(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "ElicitationResult",
		"session_id":      "sess-elr",
		"mcp_server_name": "mezmo",
		"elicitation_id":  "el-123",
		"action":          "accept",
	}

	evt, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Action != "accept" {
		t.Errorf("expected action accept, got %s", evt.Action)
	}
	if evt.MCPServerName != "mezmo" {
		t.Errorf("expected mcp_server_name mezmo, got %s", evt.MCPServerName)
	}
}

func TestBuildRelayEvent_WorktreeCreate(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "WorktreeCreate",
		"session_id":      "sess-wt",
		"name":            "fix/gd-abc123",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.WorktreeName != "fix/gd-abc123" {
		t.Errorf("expected worktree_name fix/gd-abc123, got %s", evt.WorktreeName)
	}
	if subject != "hooks.worker-1.WorktreeCreate" {
		t.Errorf("expected subject hooks.worker-1.WorktreeCreate, got %s", subject)
	}
}

func TestBuildRelayEvent_WorktreeRemove(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "WorktreeRemove",
		"session_id":      "sess-wtr",
		"worktree_path":   "/home/agent/worktrees/gd-abc123",
	}

	evt, _, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.WorktreePath != "/home/agent/worktrees/gd-abc123" {
		t.Errorf("expected worktree_path, got %s", evt.WorktreePath)
	}
}

func TestBuildRelayEvent_Setup(t *testing.T) {
	input := map[string]any{
		"hook_event_name": "Setup",
		"session_id":      "sess-setup",
		"trigger":         "init",
	}

	evt, subject, err := buildRelayEvent(input, "worker-1", "gd-TestAgent01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Trigger != "init" {
		t.Errorf("expected trigger init, got %s", evt.Trigger)
	}
	if subject != "hooks.worker-1.Setup" {
		t.Errorf("expected subject hooks.worker-1.Setup, got %s", subject)
	}
}

func TestRelayEventJSON_OmitsEmptyFields(t *testing.T) {
	evt := hookRelayEvent{
		Agent:     "worker-1",
		SessionID: "sess-123",
		Event:     "Stop",
		TS:        "2026-03-02T07:35:00.000Z",
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// These fields should be omitted for a Stop event.
	for _, key := range []string{"tool_name", "tool_input", "tool_response", "tool_use_id", "prompt_id", "permission_mode", "duration_ms", "duration_source", "source", "reason", "subagent_id", "subagent_type", "teammate_id", "teammate_type", "task_id", "task_subject", "trigger", "cwd", "config_source", "instruction_file_path", "memory_type", "load_reason", "mcp_server_name", "message", "elicitation_id", "action", "worktree_name", "worktree_path"} {
		if _, ok := m[key]; ok {
			t.Errorf("expected field %q to be omitted, but it was present", key)
		}
	}

	// These fields should always be present.
	for _, key := range []string{"agent", "session_id", "event", "ts"} {
		if _, ok := m[key]; !ok {
			t.Errorf("expected field %q to be present", key)
		}
	}
}
