package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// geminiPayload loads a hook stdin payload captured from gemini-cli 0.64.0 in
// the phase 0 spike (testdata copy of coop/tests/fixtures/gemini/hooks).
func geminiPayload(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "gemini_hooks", name))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

func TestBuildRelayEvent_GeminiPayloads(t *testing.T) {
	cases := []struct {
		file, wantEvent string
		check           func(t *testing.T, evt *hookRelayEvent)
	}{
		{"headless-SessionStart.json", "SessionStart", func(t *testing.T, e *hookRelayEvent) {
			if e.Source != "startup" {
				t.Errorf("source = %q", e.Source)
			}
		}},
		{"headless-SessionEnd.json", "SessionEnd", nil},
		{"headless-BeforeTool.json", "PreToolUse", func(t *testing.T, e *hookRelayEvent) {
			if e.ToolName != "run_shell_command" || e.ToolInput == nil {
				t.Errorf("tool = %q input = %v", e.ToolName, e.ToolInput)
			}
		}},
		{"headless-AfterTool.json", "PostToolUse", func(t *testing.T, e *hookRelayEvent) {
			if e.ToolName != "run_shell_command" || e.ToolResponse == nil {
				t.Errorf("tool = %q response = %v", e.ToolName, e.ToolResponse)
			}
		}},
		{"headless-AfterAgent.json", "Stop", func(t *testing.T, e *hookRelayEvent) {
			if e.LastAssistantMessage != "Tool finished." {
				t.Errorf("last_assistant_message = %q, want the AfterAgent prompt_response", e.LastAssistantMessage)
			}
		}},
		{"headless-PreCompress.json", "PreCompact", nil},
		{"pty-Notification.json", "Notification", func(t *testing.T, e *hookRelayEvent) {
			if e.NotificationType != "ToolPermission" {
				t.Errorf("notification_type = %q", e.NotificationType)
			}
		}},
	}
	for _, sniff := range []bool{false, true} {
		if sniff {
			t.Setenv(envGardenioHarness, "") // payload sniffing: event names or the ~/.gemini transcript path
		} else {
			t.Setenv(envGardenioHarness, "gemini")
		}
		for _, c := range cases {
			in := geminiPayload(t, c.file)
			evt, subject, err := buildRelayEvent(in, "worker-1", "gd-abc")
			if err != nil {
				t.Fatalf("%s (sniff=%v): %v", c.file, sniff, err)
			}
			if evt.Event != c.wantEvent {
				t.Errorf("%s: event = %q, want canonical %q", c.file, evt.Event, c.wantEvent)
			}
			if evt.Harness != "gemini" {
				t.Errorf("%s (sniff=%v): harness = %q, want gemini", c.file, sniff, evt.Harness)
			}
			if want, _ := in["session_id"].(string); evt.SessionID != want || want == "" {
				t.Errorf("%s: session_id = %q, want the payload's %q", c.file, evt.SessionID, want)
			}
			if want := "hooks.worker-1." + c.wantEvent; subject != want {
				t.Errorf("%s: subject = %q, want %q", c.file, subject, want)
			}
			if c.check != nil {
				c.check(t, evt)
			}
		}
	}
}

// BeforeAgent has no relayed counterpart (UserPromptSubmit is not published for
// any harness) and the model-call events have none at all.
func TestBuildRelayEvent_GeminiSkippedEvents(t *testing.T) {
	t.Setenv(envGardenioHarness, "gemini")
	for _, name := range []string{"BeforeAgent", "BeforeModel", "AfterModel", "BeforeToolSelection"} {
		in := map[string]any{"hook_event_name": name, "session_id": "s"}
		if _, _, err := buildRelayEvent(in, "a", "b"); err != errSkipEvent {
			t.Errorf("%s: err = %v, want errSkipEvent", name, err)
		}
	}
	// The caller's payload map is not mutated by the rewrite.
	in := geminiPayload(t, "headless-AfterAgent.json")
	if _, _, err := buildRelayEvent(in, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if in["hook_event_name"] != "AfterAgent" {
		t.Errorf("input mutated: hook_event_name = %v", in["hook_event_name"])
	}
}

func TestRelayHarness_GeminiEnvAndSniffing(t *testing.T) {
	t.Setenv(envGardenioHarness, "gemini")
	if got := relayHarness(map[string]any{"hook_event_name": "PreToolUse"}); got != "gemini" {
		t.Errorf("env wins: relayHarness = %q", got)
	}
	t.Setenv(envGardenioHarness, "")
	for _, name := range []string{"BeforeAgent", "AfterAgent", "BeforeTool", "AfterTool", "PreCompress"} {
		if got := relayHarness(map[string]any{"hook_event_name": name}); got != "gemini" {
			t.Errorf("sniff %s = %q, want gemini", name, got)
		}
	}
	for _, name := range []string{"PreToolUse", "PostToolUse", "Stop", "SessionStart"} {
		if got := relayHarness(map[string]any{"hook_event_name": name}); got != "claude" {
			t.Errorf("sniff %s = %q, want claude", name, got)
		}
	}
}
