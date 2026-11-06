package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/scheduler"
)

func TestFirePrompt(t *testing.T) {
	e := &TriggerEngine{logger: discardLogger()}
	steward := TriggerDef{ID: "t1", Prompt: "stored", Labels: []string{"steward", "project:tp"}}
	other := TriggerDef{ID: "t2", Prompt: "plain", Labels: []string{"project:tp"}}

	if got := e.firePrompt(context.Background(), steward, TriggerMatch{}); got != "stored" {
		t.Fatalf("no renderer: got %q, want the stored prompt", got)
	}

	e.RegisterPromptRenderer("steward", func(_ context.Context, trig TriggerDef, _ FireEvent) (string, error) {
		return "live for " + trig.ID, nil
	})
	if got := e.firePrompt(context.Background(), steward, TriggerMatch{}); got != "live for t1" {
		t.Fatalf("renderer: got %q", got)
	}
	if got := e.firePrompt(context.Background(), other, TriggerMatch{}); got != "plain" {
		t.Fatalf("a trigger without the label keeps its prompt, got %q", got)
	}

	e.RegisterPromptRenderer("steward", func(context.Context, TriggerDef, FireEvent) (string, error) {
		return "", errors.New("daemon down")
	})
	if got := e.firePrompt(context.Background(), steward, TriggerMatch{}); got != "stored" {
		t.Fatalf("a failed render falls back to the stored prompt, got %q", got)
	}
}

// The renderer is handed the entry and the bead that woke the fire, as a
// multi-trigger match records them.
func TestFirePrompt_HandsTheRendererTheWake(t *testing.T) {
	e := &TriggerEngine{logger: discardLogger()}
	trig := TriggerDef{ID: "t1", Labels: []string{"steward"},
		Triggers: []scheduler.TriggerEntry{{Kind: scheduler.TriggerKindBeadEvent, Label: "general-message"}}}
	var got FireEvent
	e.RegisterPromptRenderer("steward", func(_ context.Context, _ TriggerDef, ev FireEvent) (string, error) {
		got = ev
		return "x", nil
	})
	m := multiDecisionToMatch(trig, scheduler.FireDecision{Kind: scheduler.TriggerKindBeadEvent, Time: time.Now(),
		Event: &scheduler.TriggerEvent{BeadID: "m1", EventType: "created"}})
	e.firePrompt(context.Background(), trig, m)
	if want := (FireEvent{Entry: "general-message", BeadID: "m1", EventType: "created"}); got != want {
		t.Fatalf("renderer saw %+v, want %+v", got, want)
	}
}
