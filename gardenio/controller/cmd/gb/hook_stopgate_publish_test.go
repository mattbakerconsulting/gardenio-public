package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeGHToken is shaped like a GitHub token, built at run time so no scanner
// mistakes the source for a leak.
var fakeGHToken = "ghp" + "_" + strings.Repeat("a1", 12)

func TestBuildStopGateEvent_Block(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")
	stdin := map[string]any{
		"hook_event_name":        "Stop",
		"session_id":             "sess-1",
		"cwd":                    "/work",
		"last_assistant_message": "Waiting on CI. export GH_TOKEN=" + fakeGHToken,
	}
	evt, subject, err := buildStopGateEvent(stdin, stopGateDecisionBlock, "decision: decision point offered before session end", "crew-a", "gd-agent")
	if err != nil {
		t.Fatal(err)
	}
	if subject != "hooks.crew-a.Stop" {
		t.Errorf("subject = %q", subject)
	}
	if evt.Event != "Stop" || evt.SessionID != "sess-1" || evt.AgentBeadID != "gd-agent" || evt.Project != "gardenio" || evt.Role != "crew" {
		t.Errorf("event fields = %+v", evt)
	}
	if evt.StopGateDecision != "block" || evt.Reason != "decision: decision point offered before session end" {
		t.Errorf("decision %q reason %q", evt.StopGateDecision, evt.Reason)
	}
	if strings.Contains(evt.LastAssistantMessage, "ghp_") || !strings.Contains(evt.LastAssistantMessage, "Waiting on CI.") {
		t.Errorf("last_assistant_message not redacted: %q", evt.LastAssistantMessage)
	}
	if stdin["last_assistant_message"] != "Waiting on CI. export GH_TOKEN="+fakeGHToken {
		t.Error("buildStopGateEvent modified its input")
	}
	b, _ := json.Marshal(evt)
	if !strings.Contains(string(b), `"stop_gate_decision":"block"`) {
		t.Errorf("payload %s has no stop_gate_decision", b)
	}
}

// The message and reason get the relay's 512-byte cut.
func TestBuildStopGateEvent_SizeCap(t *testing.T) {
	long := strings.Repeat("x", 4000)
	evt, _, err := buildStopGateEvent(map[string]any{"hook_event_name": "Stop", "last_assistant_message": long}, stopGateDecisionBlock, long, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evt.LastAssistantMessage) > 515 || len(evt.Reason) > 515 {
		t.Errorf("not capped: message %d, reason %d bytes", len(evt.LastAssistantMessage), len(evt.Reason))
	}
}

// With no hook input (the gate was run by hand) the event is still a Stop.
func TestBuildStopGateEvent_EmptyInput(t *testing.T) {
	evt, subject, err := buildStopGateEvent(nil, stopGateDecisionAllow, "job_mode", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if evt.Event != "Stop" || subject != "hooks.a.Stop" || evt.StopGateDecision != "allow" {
		t.Errorf("got %+v %q", evt, subject)
	}
}

// Gemini's AfterAgent is the canonical Stop; anything else is not published.
func TestBuildStopGateEvent_HarnessEvents(t *testing.T) {
	t.Setenv("GARDENIO_HARNESS", "gemini")
	evt, _, err := buildStopGateEvent(map[string]any{"hook_event_name": "AfterAgent", "session_id": "g", "prompt_response": "done " + "sk-ant" + "-" + strings.Repeat("b2", 8)}, stopGateDecisionAllow, "allowed", "a", "")
	if err != nil {
		t.Fatalf("AfterAgent: %v", err)
	}
	if evt.Event != "Stop" {
		t.Errorf("AfterAgent became %q", evt.Event)
	}
	if !strings.HasPrefix(evt.LastAssistantMessage, "done") || strings.Contains(evt.LastAssistantMessage, "sk-ant-") {
		t.Errorf("prompt_response not carried redacted: %q", evt.LastAssistantMessage)
	}
	if _, _, err := buildStopGateEvent(map[string]any{"hook_event_name": "SessionStart"}, stopGateDecisionAllow, "", "a", ""); err == nil {
		t.Error("a non-Stop event was built")
	}
}

// A stalled or panicking bus never holds the gate past its budget.
func TestPublishStopGateDecision_BestEffort(t *testing.T) {
	t.Setenv("BEADS_NATS_URL", "nats://127.0.0.1:1")
	orig := stopGatePublish
	t.Cleanup(func() { stopGatePublish = orig })

	var got []byte
	stopGatePublish = func(_ context.Context, subject string, payload []byte) error {
		if subject != "hooks.crew-a.Stop" {
			t.Errorf("subject %q", subject)
		}
		got = payload
		return nil
	}
	t.Setenv("BOAT_AGENT", "crew-a")
	publishStopGateDecision(context.Background(), map[string]any{"hook_event_name": "Stop", "session_id": "s"}, stopGateDecisionBlock, "r")
	if !strings.Contains(string(got), `"stop_gate_decision":"block"`) {
		t.Errorf("published %s", got)
	}

	stopGatePublish = func(context.Context, string, []byte) error {
		time.Sleep(5 * time.Second)
		return nil
	}
	// The slack is wide because this is wall-clock on a shared CI runner (880ms
	// measured once, gd-FWCGfCRSne); it only has to prove the 5s sleep is cut short.
	start := time.Now()
	publishStopGateDecision(context.Background(), nil, stopGateDecisionAllow, "allowed")
	if d := time.Since(start); d > stopGatePublishBudget+2*time.Second {
		t.Errorf("stalled bus held the gate %s", d)
	}

	stopGatePublish = func(context.Context, string, []byte) error { panic("bus exploded") }
	publishStopGateDecision(context.Background(), nil, stopGateDecisionAllow, "allowed")
}
