package main

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// codexPayloads loads the hook stdin payloads captured from codex-cli 0.162.1
// in the spike (testdata copy of coop/tests/fixtures/codex/hook-payloads.jsonl).
func codexPayloads(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open("testdata/codex_hook_payloads.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("fixture line: %v", err)
		}
		out = append(out, m)
	}
	if len(out) != 6 {
		t.Fatalf("fixture has %d payloads, want 6", len(out))
	}
	return out
}

func TestBuildRelayEvent_CodexPayloads(t *testing.T) {
	t.Setenv(envGardenioHarness, "") // exercise payload sniffing
	const session = "01a13cb8-970e-74a2-b81f-f40294555aa8"
	const turn = "01a13cb8-9773-7ea2-a4bd-d7e696620f13"

	for _, in := range codexPayloads(t) {
		evt, subject, err := buildRelayEvent(in, "worker-1", "gd-abc")
		name, _ := in["hook_event_name"].(string)
		if name == "UserPromptSubmit" {
			// Not a relayed event on either harness (hooks.json wires check-mail only).
			if err != errSkipEvent {
				t.Errorf("UserPromptSubmit: err = %v, want errSkipEvent", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if evt.Harness != "codex" {
			t.Errorf("%s: harness = %q, want codex", name, evt.Harness)
		}
		if evt.SessionID != session {
			t.Errorf("%s: session_id = %q", name, evt.SessionID)
		}
		if want := "hooks.worker-1." + name; subject != want {
			t.Errorf("subject = %q, want %q", subject, want)
		}
		if evt.CWD != "/workspace" {
			t.Errorf("%s: cwd = %q", name, evt.CWD)
		}

		switch name {
		case "SessionStart":
			if evt.Source != "startup" {
				t.Errorf("source = %q", evt.Source)
			}
		case "PreToolUse", "PostToolUse":
			if evt.ToolName != "Bash" || evt.ToolUseID != "call_398c5e51" {
				t.Errorf("%s: tool = %q id = %q", name, evt.ToolName, evt.ToolUseID)
			}
			if evt.PromptID != turn {
				t.Errorf("%s: prompt_id = %q, want turn_id %q", name, evt.PromptID, turn)
			}
			if evt.PermissionMode != "bypassPermissions" {
				t.Errorf("%s: permission_mode = %q", name, evt.PermissionMode)
			}
			in, _ := evt.ToolInput.(map[string]any)
			if in["command"] != "echo hello-from-tool" {
				t.Errorf("%s: tool_input = %v", name, evt.ToolInput)
			}
			if name == "PostToolUse" && evt.ToolResponse != "hello-from-tool\n" {
				t.Errorf("tool_response = %v", evt.ToolResponse)
			}
		case "Stop":
			if evt.LastAssistantMessage == "" {
				t.Error("Stop: last_assistant_message missing")
			}
		}
	}
}

func TestBuildRelayEvent_CodexJSONShapeKeepsConsumerFields(t *testing.T) {
	t.Setenv(envGardenioHarness, "")
	evt, _, err := buildRelayEvent(codexPayloads(t)[2], "worker-1", "gd-abc")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(evt)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"agent", "agent_bead_id", "session_id", "event", "ts", "tool_name", "tool_use_id", "harness"} {
		if _, ok := m[k]; !ok {
			t.Errorf("relay JSON missing %q: %s", k, b)
		}
	}
}

func TestRelayHarness(t *testing.T) {
	claude := map[string]any{"hook_event_name": "PreToolUse", "session_id": "s", "transcript_path": "/home/agent/.claude/projects/x/s.jsonl"}
	codexTurn := map[string]any{"hook_event_name": "PreToolUse", "turn_id": "t"}
	codexRollout := map[string]any{"hook_event_name": "SessionStart", "transcript_path": "/c/sessions/2026/10/14/rollout-2026-10-14T23-21-32-abc.jsonl"}

	t.Setenv(envGardenioHarness, "")
	for _, tc := range []struct {
		name string
		in   map[string]any
		want string
	}{
		{"claude payload", claude, "claude"},
		{"turn_id", codexTurn, "codex"},
		{"rollout path", codexRollout, "codex"},
	} {
		if got := relayHarness(tc.in); got != tc.want {
			t.Errorf("%s: relayHarness = %q, want %q", tc.name, got, tc.want)
		}
	}

	// The pod env wins over sniffing.
	t.Setenv(envGardenioHarness, "claude")
	if got := relayHarness(codexTurn); got != "claude" {
		t.Errorf("env claude: got %q", got)
	}
	t.Setenv(envGardenioHarness, "codex")
	if got := relayHarness(claude); got != "codex" {
		t.Errorf("env codex: got %q", got)
	}
}

func TestBuildRelayEvent_ClaudeUnchanged(t *testing.T) {
	t.Setenv(envGardenioHarness, "")
	in := map[string]any{
		"hook_event_name": "PreToolUse", "session_id": "abc", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "ls"}, "tool_use_id": "tu1", "prompt_id": "p1",
	}
	evt, _, err := buildRelayEvent(in, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if evt.Harness != "claude" || evt.PromptID != "p1" || evt.SessionID != "abc" {
		t.Errorf("unexpected claude event: %+v", evt)
	}
}
