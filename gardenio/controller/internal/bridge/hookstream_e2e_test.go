package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

// hookE2EFixture sets up a mock StreamClient + AgentStreamManager with
// a pre-started session so that handler calls produce observable chunks.
type hookE2EFixture struct {
	mock *mockStreamClient
	mgr  *AgentStreamManager
	hs   *HookStream
}

func newHookE2EFixture(t *testing.T) *hookE2EFixture {
	t.Helper()
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	// Start a session so handlers can dispatch to the stream.
	ctx := context.Background()
	mgr.StartSession(ctx, "test-agent", "C-E2E", "thread-e2e", "U-E2E")

	hs := &HookStream{
		cfg: HookStreamConfig{
			NatsURL:       "nats://unused", // not connecting
			StreamManager: mgr,
			Logger:        testLogger(),
		},
	}
	return &hookE2EFixture{mock: mock, mgr: mgr, hs: hs}
}

// deliver publishes a hookEvent to the HookStream handler as a NATS message.
func (f *hookE2EFixture) deliver(t *testing.T, evt hookEvent) {
	t.Helper()
	payload, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal hookEvent: %v", err)
	}
	subject := "hooks." + evt.Agent + "." + evt.Event
	if evt.Agent == "" {
		subject = "hooks.test-agent." + evt.Event
	}
	f.hs.handleMessage(&nats.Msg{Subject: subject, Data: payload})
}

// lastChunkJSON returns the chunksJSON from the most recent appendChunks call.
func (f *hookE2EFixture) lastChunkJSON(t *testing.T) string {
	t.Helper()
	calls := f.mock.getCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].method == "appendChunks" {
			return calls[i].chunksJSON
		}
	}
	t.Fatal("no appendChunks call found")
	return ""
}

// chunkCount returns the number of appendChunks calls.
func (f *hookE2EFixture) chunkCount() int {
	count := 0
	for _, c := range f.mock.getCalls() {
		if c.method == "appendChunks" {
			count++
		}
	}
	return count
}

// --- E2E tests: hook event → NATS message → hookstream dispatch → stream manager ---

func TestE2E_ConfigChange(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:        "test-agent",
		SessionID:    "sess-cfg",
		Event:        "ConfigChange",
		ConfigSource: "project_settings",
		FilePath:     ".claude/settings.json",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Config: project_settings") {
		t.Errorf("expected 'Config: project_settings' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, ".claude/settings.json") {
		t.Errorf("expected file path in chunk output, got %s", chunk)
	}
}

func TestE2E_InstructionsLoaded(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:               "test-agent",
		SessionID:           "sess-inst",
		Event:               "InstructionsLoaded",
		InstructionFilePath: "/home/agent/workspace/CLAUDE.md",
		MemoryType:          "Project",
		LoadReason:          "session_start",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Instructions: session_start") {
		t.Errorf("expected 'Instructions: session_start' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "CLAUDE.md") {
		t.Errorf("expected file path in chunk output, got %s", chunk)
	}
}

func TestE2E_Setup(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:     "test-agent",
		SessionID: "sess-setup",
		Event:     "Setup",
		Trigger:   "init",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Setup: init") {
		t.Errorf("expected 'Setup: init' in chunk, got %s", chunk)
	}
}

func TestE2E_Elicitation(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:         "test-agent",
		SessionID:     "sess-el",
		Event:         "Elicitation",
		MCPServerName: "mezmo",
		Message:       "Please provide API key",
		ElicitationID: "el-123",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Elicitation: mezmo") {
		t.Errorf("expected 'Elicitation: mezmo' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "Please provide API key") {
		t.Errorf("expected elicitation message in chunk output, got %s", chunk)
	}
}

func TestE2E_ElicitationResult(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:         "test-agent",
		SessionID:     "sess-elr",
		Event:         "ElicitationResult",
		MCPServerName: "mezmo",
		ElicitationID: "el-123",
		Action:        "accept",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Elicitation: accept") {
		t.Errorf("expected 'Elicitation: accept' in chunk, got %s", chunk)
	}
}

func TestE2E_WorktreeCreate(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:        "test-agent",
		SessionID:    "sess-wt",
		Event:        "WorktreeCreate",
		WorktreeName: "fix/gd-abc123",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Worktree created") {
		t.Errorf("expected 'Worktree created' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "fix/gd-abc123") {
		t.Errorf("expected worktree name in chunk output, got %s", chunk)
	}
}

func TestE2E_WorktreeRemove(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:        "test-agent",
		SessionID:    "sess-wtr",
		Event:        "WorktreeRemove",
		WorktreePath: "/home/agent/.beads/worktrees/gd-abc123",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Worktree removed") {
		t.Errorf("expected 'Worktree removed' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "gd-abc123") {
		t.Errorf("expected worktree path in chunk output, got %s", chunk)
	}
}

func TestE2E_SessionStart(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:     "test-agent",
		SessionID: "sess-start",
		Event:     "SessionStart",
		Source:    "startup",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Session: startup") {
		t.Errorf("expected 'Session: startup' in chunk, got %s", chunk)
	}
}

func TestE2E_SessionEnd(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:     "test-agent",
		SessionID: "sess-end",
		Event:     "SessionEnd",
		Reason:    "clear",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Session ended: clear") {
		t.Errorf("expected 'Session ended: clear' in chunk, got %s", chunk)
	}
}

func TestE2E_PreCompact(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:     "test-agent",
		SessionID: "sess-compact",
		Event:     "PreCompact",
		Trigger:   "auto",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Compacting context") {
		t.Errorf("expected 'Compacting context' in chunk, got %s", chunk)
	}
}

func TestE2E_PostCompact(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:     "test-agent",
		SessionID: "sess-compact",
		Event:     "PostCompact",
		Trigger:   "auto",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Context compacted") {
		t.Errorf("expected 'Context compacted' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "trigger: auto") {
		t.Errorf("expected 'trigger: auto' in chunk output, got %s", chunk)
	}
}

func TestE2E_TeammateIdle(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliver(t, hookEvent{
		Agent:        "test-agent",
		SessionID:    "sess-team",
		Event:        "TeammateIdle",
		TeammateID:   "teammate-xyz",
		TeammateType: "research",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Teammate idle: research") {
		t.Errorf("expected 'Teammate idle: research' in chunk, got %s", chunk)
	}
}

// TestE2E_FullPipeline_MultipleEvents verifies that a sequence of events
// flows through the pipeline correctly, simulating a realistic agent session.
func TestE2E_FullPipeline_MultipleEvents(t *testing.T) {
	f := newHookE2EFixture(t)

	events := []hookEvent{
		{Agent: "test-agent", SessionID: "sess-full", Event: "SessionStart", Source: "startup"},
		{Agent: "test-agent", SessionID: "sess-full", Event: "Setup", Trigger: "init"},
		{Agent: "test-agent", SessionID: "sess-full", Event: "ConfigChange", ConfigSource: "user_settings", FilePath: ".claude/settings.json"},
		{Agent: "test-agent", SessionID: "sess-full", Event: "InstructionsLoaded", InstructionFilePath: "/workspace/CLAUDE.md", LoadReason: "session_start"},
		{Agent: "test-agent", SessionID: "sess-full", Event: "PreToolUse", ToolName: "Read", ToolInput: map[string]any{"file_path": "/workspace/main.go"}},
		{Agent: "test-agent", SessionID: "sess-full", Event: "PostToolUse", ToolName: "Read", ToolInput: map[string]any{"file_path": "/workspace/main.go"}, ToolResponse: map[string]any{"output": "package main"}},
		{Agent: "test-agent", SessionID: "sess-full", Event: "WorktreeCreate", WorktreeName: "fix/my-branch"},
		{Agent: "test-agent", SessionID: "sess-full", Event: "PreCompact", Trigger: "auto"},
		{Agent: "test-agent", SessionID: "sess-full", Event: "PostCompact", Trigger: "auto"},
		{Agent: "test-agent", SessionID: "sess-full", Event: "TeammateIdle", TeammateID: "helper-1", TeammateType: "Explore"},
		{Agent: "test-agent", SessionID: "sess-full", Event: "SessionEnd", Reason: "done"},
	}

	for _, evt := range events {
		f.deliver(t, evt)
	}

	// Events that send task_update chunks: SessionStart, Setup, ConfigChange,
	// InstructionsLoaded, WorktreeCreate, PreCompact, PostCompact, TeammateIdle,
	// SessionEnd = 9 (PreToolUse/PostToolUse update tool counts, not task chunks).
	chunkCalls := f.chunkCount()
	if chunkCalls < 9 {
		t.Errorf("expected at least 9 appendChunks calls for task_update events, got %d", chunkCalls)
	}
}

// TestE2E_UnknownEventIgnored verifies that unknown events are silently dropped.
func TestE2E_UnknownEventIgnored(t *testing.T) {
	f := newHookE2EFixture(t)
	before := f.chunkCount()

	payload, _ := json.Marshal(hookEvent{
		Agent:     "test-agent",
		SessionID: "sess-unk",
		Event:     "UnknownEvent",
	})
	f.hs.handleMessage(&nats.Msg{
		Subject: "hooks.test-agent.UnknownEvent",
		Data:    payload,
	})

	after := f.chunkCount()
	if after != before {
		t.Errorf("unknown event should not produce chunks, but count changed from %d to %d", before, after)
	}
}

// TestE2E_MalformedPayloadIgnored verifies that malformed JSON is silently dropped.
func TestE2E_MalformedPayloadIgnored(t *testing.T) {
	f := newHookE2EFixture(t)
	before := f.chunkCount()

	f.hs.handleMessage(&nats.Msg{
		Subject: "hooks.test-agent.PreToolUse",
		Data:    []byte(`{invalid json`),
	})

	after := f.chunkCount()
	if after != before {
		t.Errorf("malformed JSON should not produce chunks, but count changed from %d to %d", before, after)
	}
}

// TestE2E_AgentFromSubjectFallback verifies that when the agent field is empty
// in the payload, the hookstream extracts the agent name from the NATS subject.
func TestE2E_AgentFromSubjectFallback(t *testing.T) {
	f := newHookE2EFixture(t)

	payload, _ := json.Marshal(hookEvent{
		Agent:     "", // empty — should fall back to subject
		SessionID: "sess-fb",
		Event:     "Setup",
		Trigger:   "init",
	})
	f.hs.handleMessage(&nats.Msg{
		Subject: "hooks.test-agent.Setup",
		Data:    payload,
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Setup: init") {
		t.Errorf("expected 'Setup: init' via subject fallback, got %s", chunk)
	}
}

// TestE2E_NoSessionSilentDrop verifies that events for agents without
// an active stream session are silently dropped (no panics, no errors).
func TestE2E_NoSessionSilentDrop(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	// No StartSession — agent has no session.

	hs := &HookStream{
		cfg: HookStreamConfig{
			NatsURL:       "nats://unused",
			StreamManager: mgr,
			Logger:        testLogger(),
		},
	}

	payload, _ := json.Marshal(hookEvent{
		Agent:        "no-session-agent",
		SessionID:    "sess-none",
		Event:        "ConfigChange",
		ConfigSource: "user_settings",
	})
	hs.handleMessage(&nats.Msg{
		Subject: "hooks.no-session-agent.ConfigChange",
		Data:    payload,
	})

	for _, c := range mock.getCalls() {
		if c.method == "appendChunks" {
			t.Error("expected no chunk calls for agent without session")
		}
	}
}

// TestE2E_StopGateDecision: gb hook stop-gate publishes a Stop for every
// verdict (gd-fSdUbOmf3g). A block does not end the turn, so it must not put a
// "Turn complete" chunk in the thread stream; an allow, or a Stop with no
// verdict, still does.
func TestE2E_StopGateDecision(t *testing.T) {
	tests := []struct {
		name      string
		decision  string
		wantChunk bool
	}{
		{name: "block", decision: "block", wantChunk: false},
		{name: "allow", decision: "allow", wantChunk: true},
		{name: "no verdict", decision: "", wantChunk: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHookE2EFixture(t)
			evt := map[string]any{
				"agent":                  "test-agent",
				"session_id":             "sess-stop-gate",
				"event":                  "Stop",
				"last_assistant_message": "Waiting on CI",
			}
			if tt.decision != "" {
				evt["stop_gate_decision"] = tt.decision
				evt["reason"] = "decision point offered before session end"
			}
			payload, err := json.Marshal(evt)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			before := f.chunkCount()
			f.hs.handleMessage(&nats.Msg{Subject: "hooks.test-agent.Stop", Data: payload})

			got := f.chunkCount() > before
			if got != tt.wantChunk {
				t.Fatalf("chunk sent = %v, want %v", got, tt.wantChunk)
			}
			if tt.wantChunk {
				chunk := f.lastChunkJSON(t)
				if !strings.Contains(chunk, "Turn complete") || !strings.Contains(chunk, "Waiting on CI") {
					t.Errorf("expected 'Turn complete' chunk with the last message, got %s", chunk)
				}
			}
		})
	}
}

// TestE2E_AllRelevantEvents verifies that every event type in relevantHookEvents
// is dispatched without errors when delivered through the hookstream.
func TestE2E_AllRelevantEvents(t *testing.T) {
	events := map[string]hookEvent{
		"PreToolUse":         {Event: "PreToolUse", ToolName: "Read"},
		"PostToolUse":        {Event: "PostToolUse", ToolName: "Read"},
		"PostToolUseFailure": {Event: "PostToolUseFailure", ToolName: "Bash", Error: "exit 1", ErrorType: "tool_error"},
		"Stop":               {Event: "Stop", LastAssistantMessage: "Done"},
		"StopFailure":        {Event: "StopFailure", Error: "timeout"},
		"SubagentStart":      {Event: "SubagentStart", SubagentID: "sub-1", SubagentType: "Explore"},
		"SubagentStop":       {Event: "SubagentStop", SubagentID: "sub-1", SubagentType: "Explore"},
		"Notification":       {Event: "Notification", NotificationType: "idle_prompt"},
		"TaskCreated":        {Event: "TaskCreated", TaskID: "task-1", TaskSubject: "Fix bug"},
		"TaskCompleted":      {Event: "TaskCompleted", TaskID: "task-1", TaskSubject: "Fix bug"},
		"CwdChanged":         {Event: "CwdChanged", OldCWD: "/old", NewCWD: "/new"},
		"FileChanged":        {Event: "FileChanged", FilePath: "/main.go", ChangeType: "modified"},
		"PermissionRequest":  {Event: "PermissionRequest", ToolName: "Bash", RequestID: "req-1"},
		"SessionStart":       {Event: "SessionStart", Source: "startup"},
		"SessionEnd":         {Event: "SessionEnd", Reason: "clear"},
		"PreCompact":         {Event: "PreCompact", Trigger: "auto"},
		"PostCompact":        {Event: "PostCompact", Trigger: "auto"},
		"TeammateIdle":       {Event: "TeammateIdle", TeammateID: "tm-1", TeammateType: "research"},
		"ConfigChange":       {Event: "ConfigChange", ConfigSource: "user_settings"},
		"InstructionsLoaded": {Event: "InstructionsLoaded", InstructionFilePath: "/CLAUDE.md", LoadReason: "session_start"},
		"Elicitation":        {Event: "Elicitation", MCPServerName: "mezmo", Message: "Confirm"},
		"ElicitationResult":  {Event: "ElicitationResult", MCPServerName: "mezmo", Action: "accept"},
		"WorktreeCreate":     {Event: "WorktreeCreate", WorktreeName: "fix/branch"},
		"WorktreeRemove":     {Event: "WorktreeRemove", WorktreePath: "/worktrees/gd-123"},
		"Setup":              {Event: "Setup", Trigger: "init"},
	}

	for name, evt := range events {
		t.Run(name, func(t *testing.T) {
			f := newHookE2EFixture(t)
			evt.Agent = "test-agent"
			evt.SessionID = "sess-all"
			// Should not panic.
			f.deliver(t, evt)
		})
	}
}

// TestE2E_FieldMappingFidelity verifies that the JSON field names used by
// gb hook relay (hookRelayEvent) match the field names expected by the
// hookstream (hookEvent). This catches regressions where a field is added
// to one struct but named differently in the other.
func TestE2E_FieldMappingFidelity(t *testing.T) {
	// Simulate what gb hook relay produces (raw JSON with the relay field names).
	relayJSON := `{
		"agent": "fidelity-agent",
		"session_id": "sess-fidelity",
		"event": "Elicitation",
		"ts": "2026-04-05T00:00:00Z",
		"mcp_server_name": "test-server",
		"message": "Confirm action",
		"elicitation_id": "el-fidelity"
	}`

	// Unmarshal into hookEvent (the hookstream struct).
	var received hookEvent
	if err := json.Unmarshal([]byte(relayJSON), &received); err != nil {
		t.Fatalf("unmarshal to hookEvent failed: %v", err)
	}

	if received.Event != "Elicitation" {
		t.Errorf("Event: expected Elicitation, got %s", received.Event)
	}
	if received.MCPServerName != "test-server" {
		t.Errorf("MCPServerName: expected test-server, got %s", received.MCPServerName)
	}
	if received.Message != "Confirm action" {
		t.Errorf("Message: expected 'Confirm action', got %s", received.Message)
	}
	if received.ElicitationID != "el-fidelity" {
		t.Errorf("ElicitationID: expected el-fidelity, got %s", received.ElicitationID)
	}
	if received.Agent != "fidelity-agent" {
		t.Errorf("Agent: expected fidelity-agent, got %s", received.Agent)
	}
}

// TestE2E_FieldMappingFidelity_AllEventTypes tests field mapping for every
// event type that has type-specific fields, ensuring gb hook relay JSON
// deserializes correctly into the hookstream's hookEvent struct.
func TestE2E_FieldMappingFidelity_AllEventTypes(t *testing.T) {
	tests := []struct {
		name string
		json string
		check func(t *testing.T, evt hookEvent)
	}{
		{
			name: "ConfigChange",
			json: `{"agent":"a","session_id":"s","event":"ConfigChange","config_source":"project_settings","file_path":".claude/settings.json"}`,
			check: func(t *testing.T, evt hookEvent) {
				if evt.ConfigSource != "project_settings" {
					t.Errorf("ConfigSource: got %s", evt.ConfigSource)
				}
				if evt.FilePath != ".claude/settings.json" {
					t.Errorf("FilePath: got %s", evt.FilePath)
				}
			},
		},
		{
			name: "InstructionsLoaded",
			json: `{"agent":"a","session_id":"s","event":"InstructionsLoaded","instruction_file_path":"/CLAUDE.md","memory_type":"Project","load_reason":"session_start"}`,
			check: func(t *testing.T, evt hookEvent) {
				if evt.InstructionFilePath != "/CLAUDE.md" {
					t.Errorf("InstructionFilePath: got %s", evt.InstructionFilePath)
				}
				if evt.MemoryType != "Project" {
					t.Errorf("MemoryType: got %s", evt.MemoryType)
				}
				if evt.LoadReason != "session_start" {
					t.Errorf("LoadReason: got %s", evt.LoadReason)
				}
			},
		},
		{
			name: "WorktreeCreate",
			json: `{"agent":"a","session_id":"s","event":"WorktreeCreate","worktree_name":"fix/branch"}`,
			check: func(t *testing.T, evt hookEvent) {
				if evt.WorktreeName != "fix/branch" {
					t.Errorf("WorktreeName: got %s", evt.WorktreeName)
				}
			},
		},
		{
			name: "WorktreeRemove",
			json: `{"agent":"a","session_id":"s","event":"WorktreeRemove","worktree_path":"/worktrees/gd-123"}`,
			check: func(t *testing.T, evt hookEvent) {
				if evt.WorktreePath != "/worktrees/gd-123" {
					t.Errorf("WorktreePath: got %s", evt.WorktreePath)
				}
			},
		},
		{
			name: "TeammateIdle",
			json: `{"agent":"a","session_id":"s","event":"TeammateIdle","teammate_id":"tm-1","teammate_type":"research"}`,
			check: func(t *testing.T, evt hookEvent) {
				if evt.TeammateID != "tm-1" {
					t.Errorf("TeammateID: got %s", evt.TeammateID)
				}
				if evt.TeammateType != "research" {
					t.Errorf("TeammateType: got %s", evt.TeammateType)
				}
			},
		},
		{
			name: "ElicitationResult",
			json: `{"agent":"a","session_id":"s","event":"ElicitationResult","mcp_server_name":"mezmo","elicitation_id":"el-1","action":"deny"}`,
			check: func(t *testing.T, evt hookEvent) {
				if evt.MCPServerName != "mezmo" {
					t.Errorf("MCPServerName: got %s", evt.MCPServerName)
				}
				if evt.Action != "deny" {
					t.Errorf("Action: got %s", evt.Action)
				}
			},
		},
		{
			name: "SessionStart",
			json: `{"agent":"a","session_id":"s","event":"SessionStart","source":"resume"}`,
			check: func(t *testing.T, evt hookEvent) {
				if evt.Source != "resume" {
					t.Errorf("Source: got %s", evt.Source)
				}
			},
		},
		{
			name: "SessionEnd",
			json: `{"agent":"a","session_id":"s","event":"SessionEnd","reason":"user_exit"}`,
			check: func(t *testing.T, evt hookEvent) {
				if evt.Reason != "user_exit" {
					t.Errorf("Reason: got %s", evt.Reason)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var evt hookEvent
			if err := json.Unmarshal([]byte(tc.json), &evt); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			tc.check(t, evt)
		})
	}
}

// --- E2E tests: coop message forward → hookstream → stream manager ---

// deliverMessage publishes a messageEvent to the HookStream message handler.
func (f *hookE2EFixture) deliverMessage(t *testing.T, evt messageEvent) {
	t.Helper()
	payload, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal messageEvent: %v", err)
	}
	subject := "coop.events.messages." + evt.EventType
	f.hs.handleMessageForward(&nats.Msg{Subject: subject, Data: payload})
}

func TestE2E_MessageForward_AssistantText(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliverMessage(t, messageEvent{
		EventType: "assistant_text",
		Agent:     "test-agent",
		SessionID: "sess-msg",
		Text:      "Here is my analysis of the codebase...",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Generating response") {
		t.Errorf("expected 'Generating response' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "Here is my analysis") {
		t.Errorf("expected text preview in chunk, got %s", chunk)
	}
}

func TestE2E_MessageForward_Thinking(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliverMessage(t, messageEvent{
		EventType: "thinking",
		Agent:     "test-agent",
		SessionID: "sess-msg",
		Text:      "Let me reason about the best approach...",
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Thinking...") {
		t.Errorf("expected 'Thinking...' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "Let me reason") {
		t.Errorf("expected thinking preview in chunk, got %s", chunk)
	}
}

func TestE2E_MessageForward_Usage(t *testing.T) {
	f := newHookE2EFixture(t)
	f.deliverMessage(t, messageEvent{
		EventType:    "usage",
		Agent:        "test-agent",
		SessionID:    "sess-msg",
		InputTokens:  15000,
		OutputTokens: 3000,
		CacheRead:    10000,
		DurationMs:   2500,
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Token usage") {
		t.Errorf("expected 'Token usage' in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "15K in") {
		t.Errorf("expected input tokens in chunk, got %s", chunk)
	}
	if !strings.Contains(chunk, "2.5s") {
		t.Errorf("expected duration in chunk, got %s", chunk)
	}
}

func TestE2E_MessageForward_K8sPodName(t *testing.T) {
	// When k8s.pod_name is set, it should be used as the agent name.
	f := newHookE2EFixture(t)
	f.deliverMessage(t, messageEvent{
		EventType: "assistant_text",
		Agent:     "wrong-agent",
		SessionID: "sess-k8s",
		Text:      "Response from pod",
		K8s:       &k8sMeta{PodName: "test-agent"},
	})

	chunk := f.lastChunkJSON(t)
	if !strings.Contains(chunk, "Generating response") {
		t.Errorf("expected chunk from k8s pod_name resolution, got %s", chunk)
	}
}

func TestE2E_MessageForward_EmptyAgent_Ignored(t *testing.T) {
	f := newHookE2EFixture(t)
	initial := f.chunkCount()
	f.deliverMessage(t, messageEvent{
		EventType: "assistant_text",
		SessionID: "sess-empty",
		Text:      "Should be ignored",
	})

	if f.chunkCount() != initial {
		t.Error("expected no chunk for message with empty agent")
	}
}

func TestE2E_MessageForward_NoSession_Ignored(t *testing.T) {
	f := newHookE2EFixture(t)
	initial := f.chunkCount()
	f.deliverMessage(t, messageEvent{
		EventType: "assistant_text",
		Agent:     "unknown-agent",
		SessionID: "sess-nosess",
		Text:      "Should be ignored",
	})

	if f.chunkCount() != initial {
		t.Error("expected no chunk for agent without session")
	}
}

func TestE2E_MessageForward_EmptyText_Ignored(t *testing.T) {
	f := newHookE2EFixture(t)
	initial := f.chunkCount()
	f.deliverMessage(t, messageEvent{
		EventType: "assistant_text",
		Agent:     "test-agent",
		SessionID: "sess-empty-text",
		Text:      "",
	})

	if f.chunkCount() != initial {
		t.Error("expected no chunk for assistant_text with empty text")
	}
}
