package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestOutputProgressBlock_NonScheduledAgent_Empty(t *testing.T) {
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{}}
	cleanup := setupScheduleTestDaemon(t, agent, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputProgressBlock(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output, got: %q", buf.String())
	}
}

func TestOutputProgressBlock_EmptyAgentID_Empty(t *testing.T) {
	var buf bytes.Buffer
	outputProgressBlock(&buf, "")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for empty agentID, got: %q", buf.String())
	}
}

func TestOutputProgressBlock_NoProgressSpec_Empty(t *testing.T) {
	agent := &mockBead{
		ID:     "gd-agent",
		Type:   "agent",
		Fields: map[string]string{"schedule_id": "gd-sched"},
	}
	sched := &mockBead{
		ID:     "gd-sched",
		Type:   "schedule",
		Fields: map[string]string{},
	}
	cleanup := setupScheduleTestDaemon(t, agent, sched)
	defer cleanup()

	var buf bytes.Buffer
	outputProgressBlock(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output without progress spec, got: %q", buf.String())
	}
}

func TestOutputProgressBlock_SurfaceOff_Empty(t *testing.T) {
	agent := &mockBead{
		ID:     "gd-agent",
		Type:   "agent",
		Fields: map[string]string{"schedule_id": "gd-sched"},
	}
	sched := &mockBead{
		ID:   "gd-sched",
		Type: "schedule",
		Fields: map[string]string{
			"progress": `{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}],"windows":[{"name":"this_run","scope":"since_run_start"}]}`,
		},
	}
	cleanup := setupScheduleTestDaemon(t, agent, sched)
	defer cleanup()

	var buf bytes.Buffer
	outputProgressBlock(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output when surface.in_prime=false, got: %q", buf.String())
	}
}

func TestOutputProgressBlock_RendersWhenSurfaceOn(t *testing.T) {
	agent := &mockBead{
		ID:     "gd-agent",
		Type:   "agent",
		Fields: map[string]string{"schedule_id": "gd-sched"},
	}
	sched := &mockBead{
		ID:   "gd-sched",
		Type: "schedule",
		Fields: map[string]string{
			"assign_bead":    "gd-EPIC",
			"progress":       `{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}],"windows":[{"name":"this_run","scope":"since_run_start"}],"surface":{"in_prime":true}}`,
			"progress_state": `{"events":[{"kind":"child_closed","target_id":"gd-A","occurred_at":"2026-04-29T10:00:00Z"}],"current_run_start":"2026-04-29T09:00:00Z"}`,
		},
	}
	cleanup := setupScheduleTestDaemon(t, agent, sched)
	defer cleanup()

	var buf bytes.Buffer
	outputProgressBlock(&buf, "gd-agent")
	out := buf.String()
	if !strings.Contains(out, "## Loop Progress") {
		t.Errorf("expected Loop Progress header, got: %q", out)
	}
	if !strings.Contains(out, "child_closed") {
		t.Errorf("expected child_closed in output, got: %q", out)
	}
}
