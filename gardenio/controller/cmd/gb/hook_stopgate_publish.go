package main

// Stop-gate decisions on HOOK_EVENTS (gd-fSdUbOmf3g).
//
// The Stop hook runs only gb hook stop-gate, not gb hook relay, so neither the
// Stop event nor the gate's block reached the bus, and gb session sweep could
// not see an agent that was blocked while waiting on a background job and then
// held its turn with a foreground poll (survey class stop-gate-wait). The gate
// now publishes one Stop event per verdict, built by the relay's own
// buildRelayEvent, with stop_gate_decision ("block" or "allow") and the reason.
// If the relay is ever wired on Stop as well, consumers must key on
// stop_gate_decision to tell the two apart. The bridge hookstream already does:
// it skips the "Turn complete" thread chunk for a block.
//
// Publishing is best-effort: it never changes the verdict, and the gate waits
// at most stopGatePublishBudget for it before exiting.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/toollatency"
)

// stopGatePublishBudget is the longest the gate waits on the bus.
const stopGatePublishBudget = 500 * time.Millisecond

// stopGateReasonMax caps the reason, the same cap the relay puts on the last
// assistant message.
const stopGateReasonMax = 512

const (
	stopGateDecisionBlock = "block"
	stopGateDecisionAllow = "allow"
)

// stopGatePublish sends one encoded event. A variable so tests can capture or
// stall it.
var stopGatePublish = publishStopGateEvent

// buildStopGateEvent is the Stop event for a gate verdict. stdinEvent is the
// hook input (empty when there was none); the last assistant message is
// redacted before the relay's usual 512-byte cut, so it carries nothing the
// relay would not publish for Stop.
func buildStopGateEvent(stdinEvent map[string]any, decision, reason, agentName, agentBeadID string) (*hookRelayEvent, string, error) {
	input := make(map[string]any, len(stdinEvent)+1)
	for k, v := range stdinEvent {
		input[k] = v
	}
	if name, _ := input["hook_event_name"].(string); name == "" {
		input["hook_event_name"] = "Stop"
	}
	if msg, ok := input["last_assistant_message"].(string); ok {
		input["last_assistant_message"] = toollatency.RedactText(msg)
	}
	if msg, ok := input["prompt_response"].(string); ok {
		input["prompt_response"] = toollatency.RedactText(msg) // Gemini AfterAgent
	}
	evt, subject, err := buildRelayEvent(input, agentName, agentBeadID)
	if err != nil {
		return nil, "", err
	}
	if evt.Event != "Stop" {
		return nil, "", errSkipEvent
	}
	evt.StopGateDecision = decision
	evt.Reason = truncateString(toollatency.RedactText(reason), stopGateReasonMax)
	return evt, subject, nil
}

// publishStopGateDecision puts the gate's verdict on the bus, best-effort and
// bounded by stopGatePublishBudget. Call it just before the gate exits.
func publishStopGateDecision(ctx context.Context, stdinEvent map[string]any, decision, reason string) {
	if daemon == nil && os.Getenv("BEADS_NATS_URL") == "" {
		return
	}
	evt, subject, err := buildStopGateEvent(stdinEvent, decision, reason, resolveAgentName(), resolveAgentBeadID())
	if err != nil {
		return
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, stopGatePublishBudget)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- stopGatePublish(ctx, subject, payload)
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[stop-gate] note: decision not published to HOOK_EVENTS: %v\n", err)
	}
}

// readStopGateStdin decodes the hook input, or returns an empty map. A
// terminal or /dev/null (a character device) is not read, so a gate run by hand
// does not wait on a human.
func readStopGateStdin() map[string]any {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return map[string]any{}
	}
	var ev map[string]any
	if err := json.NewDecoder(os.Stdin).Decode(&ev); err != nil || ev == nil {
		return map[string]any{}
	}
	return ev
}

// publishStopGateEvent sends the way the relay does: the daemon's publish
// endpoint, then direct NATS.
func publishStopGateEvent(ctx context.Context, subject string, payload []byte) error {
	if daemon != nil {
		err := daemon.PublishHookEvent(ctx, beadsapi.PublishHookEventRequest{Subject: subject, Payload: payload})
		if err == nil || ctx.Err() != nil {
			return err
		}
	}
	if natsURL := os.Getenv("BEADS_NATS_URL"); natsURL != "" {
		return publishToNATS(natsURL, subject, payload)
	}
	return nil
}
