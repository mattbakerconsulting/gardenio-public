package main

import (
	"context"
	"encoding/json"
	"testing"
)

// ── escalation reflag tests (ka-0000004918) ──────────────────────────────
//
// Mirrors the decision reflag suite: both verbs feed the shared runReflag
// body, so this suite locks the escalation-specific type gate plus the core
// bump/cap/label behaviors against the same reflagDaemonStub.

func makeReflagEscalation(id string, priority int, fields map[string]any, labels []string) map[string]any {
	b := makeReflagBead(id, priority, fields, labels)
	b["type"] = "escalation"
	b["kind"] = "issue"
	return b
}

// resetEscalationReflagFlags resets the escalation reflag command's flag state
// across tests; cobra commands are global singletons in this package.
func resetEscalationReflagFlags(t *testing.T) {
	t.Helper()
	escalationReflagCmd.ResetFlags()
	escalationReflagCmd.Flags().String("reason", "", "reason for reflag (recorded as audit comment and included in Slack repost)")
	escalationReflagCmd.Flags().Int("priority", -1, "set priority explicitly (0=critical .. 4=backlog) instead of bumping by one toward P0")
}

func TestEscalationReflag_BumpsPriorityByOne(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-e1", 3, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationReflagFlags(t)

	cmd := escalationReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-e1"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	var newPri int = -1
	for _, p := range stub.patches {
		if v, ok := p["priority"].(float64); ok {
			newPri = int(v)
		}
	}
	if newPri != 2 {
		t.Errorf("priority after reflag = %d, want 2 (3 → 2)", newPri)
	}
	if len(stub.comments) != 1 {
		t.Errorf("got %d audit comments, want 1", len(stub.comments))
	}
}

func TestEscalationReflag_CapsAtP0(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-e2", 0, map[string]any{"reflag_count": "3"}, []string{"escalated"}),
	}
	setupReflagDaemon(t, stub)
	resetEscalationReflagFlags(t)

	cmd := escalationReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-e2"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	// At P0, no priority patch should be sent (newPriority == oldPriority).
	for _, p := range stub.patches {
		if _, ok := p["priority"]; ok {
			t.Errorf("priority patch sent at P0 cap; got %v", p["priority"])
		}
	}
	// reflag_count should still bump from 3 to 4 in the fields patch.
	foundCountBump := false
	for _, p := range stub.patches {
		fields, ok := p["fields"].(map[string]any)
		if !ok {
			continue
		}
		switch v := fields["reflag_count"].(type) {
		case float64:
			if v == 4 {
				foundCountBump = true
			}
		case string:
			if v == "4" {
				foundCountBump = true
			}
		}
	}
	if !foundCountBump {
		t.Errorf("expected reflag_count to bump from 3 to 4 in patches; got %#v", stub.patches)
	}
}

func TestEscalationReflag_AddsEscalatedLabelOnceAtP0P1(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-e3", 2, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationReflagFlags(t)

	cmd := escalationReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-e3"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	// Should have a label patch including "escalated" since 2 → 1.
	foundLabelPatch := false
	for _, p := range stub.patches {
		if labels, ok := p["labels"].([]any); ok {
			for _, l := range labels {
				if s, _ := l.(string); s == "escalated" {
					foundLabelPatch = true
				}
			}
		}
	}
	if !foundLabelPatch {
		t.Errorf("expected 'escalated' label patch when priority drops to P1; got patches %#v", stub.patches)
	}
}

func TestEscalationReflag_ExplicitPriorityFlag(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-e4", 3, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationReflagFlags(t)

	cmd := escalationReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("priority", "0"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, []string{"gd-e4"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	var newPri int = -1
	for _, p := range stub.patches {
		if v, ok := p["priority"].(float64); ok {
			newPri = int(v)
		}
	}
	if newPri != 0 {
		t.Errorf("--priority=0 should jump to P0; got %d", newPri)
	}
}

func TestEscalationReflag_RejectsClosedEscalation(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-e5", 2, nil, nil),
	}
	stub.bead["status"] = "closed"
	setupReflagDaemon(t, stub)
	resetEscalationReflagFlags(t)

	cmd := escalationReflagCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-e5"})
	if err == nil {
		t.Fatal("expected error reflagging closed escalation; got nil")
	}
}

func TestEscalationReflag_RejectsNonEscalationBead(t *testing.T) {
	// A decision bead must be rejected by `gb escalation reflag` exactly the
	// way `gb decision reflag` rejects escalations — the shared body's type
	// gate is directional.
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-e6", 2, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationReflagFlags(t)

	cmd := escalationReflagCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-e6"})
	if err == nil {
		t.Fatal("expected error reflagging non-escalation bead; got nil")
	}
	if len(stub.patches) != 0 {
		var raw []byte
		raw, _ = json.Marshal(stub.patches)
		t.Errorf("no patches expected on type rejection; got %s", raw)
	}
}

func TestEscalationReflag_RejectsPriorityOutOfRange(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-e7", 2, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationReflagFlags(t)

	cmd := escalationReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("priority", "9"); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, []string{"gd-e7"})
	if err == nil {
		t.Fatal("expected error for out-of-range priority; got nil")
	}
}
