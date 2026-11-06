package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestIsMemoryEligible_GardenerRole(t *testing.T) {
	if !isMemoryEligible(context.Background(), "", "gardener", nil) {
		t.Error("expected gardener role to be memory-eligible")
	}
	if !isMemoryEligible(context.Background(), "", "epic-gardener", nil) {
		t.Error("expected epic-gardener role to be memory-eligible")
	}
}

func TestIsMemoryEligible_CrewRole_NoBead(t *testing.T) {
	if isMemoryEligible(context.Background(), "", "crew", nil) {
		t.Error("expected crew role with no agent ID to be ineligible")
	}
}

func TestIsMemoryEligible_ScheduledAgent(t *testing.T) {
	bead := &mockBead{
		ID:     "gd-test123",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"schedule_id": "gd-sched1"},
	}
	cleanup := setupScheduleTestDaemon(t, bead, nil)
	defer cleanup()

	if !isMemoryEligible(context.Background(), "gd-test123", "crew", nil) {
		t.Error("expected scheduled crew agent to be memory-eligible")
	}
}

func TestIsMemoryEligible_NonScheduledCrew(t *testing.T) {
	bead := &mockBead{
		ID:     "gd-test456",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{},
	}
	cleanup := setupScheduleTestDaemon(t, bead, nil)
	defer cleanup()

	if isMemoryEligible(context.Background(), "gd-test456", "crew", nil) {
		t.Error("expected non-scheduled crew agent to be ineligible")
	}
}

func TestOutputMemoryPointer_EligibleNoOp(t *testing.T) {
	var buf bytes.Buffer
	outputMemoryPointer(&buf, "", "gardener")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for memory-eligible agent, got %d bytes: %s", buf.Len(), buf.String())
	}
}

func TestOutputMemoryPointer_IneligibleShortPointer(t *testing.T) {
	var buf bytes.Buffer
	outputMemoryPointer(&buf, "", "crew")

	out := buf.String()
	if out == "" {
		t.Fatal("expected non-empty pointer for ineligible agent")
	}
	if len(out) > 500 {
		t.Errorf("expected pointer ≤500 chars, got %d: %s", len(out), out)
	}
	if !strings.Contains(out, memoryProtocolAdviceID) {
		t.Errorf("expected pointer to contain advice ID %q, got: %s", memoryProtocolAdviceID, out)
	}
	if !strings.Contains(out, "## Memory") {
		t.Errorf("expected pointer to contain '## Memory' header, got: %s", out)
	}
}
