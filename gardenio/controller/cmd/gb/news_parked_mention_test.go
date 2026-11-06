package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestPrintNewsBeadMarksParkedRow pins that an in-progress row the daemon flags
// parked_on_decision (an open operator decision names it in prose,
// ka-0000003797) renders the [parked: <id>] badge, and an un-flagged row does
// not (no false positive). This is the text-mention complement to the
// requesting_task-field parking classifyStale surfaces for the STALE bucket.
func TestPrintNewsBeadMarksParkedRow(t *testing.T) {
	parked := &beadsapi.BeadDetail{
		ID: "gd-ip1", Type: "task", Title: "parked in-progress", Assignee: "agent-x",
		ParkedOnDecision: true, ParkedOnDecisionBeadID: "gd-decN",
	}
	free := &beadsapi.BeadDetail{
		ID: "gd-ip2", Type: "task", Title: "free in-progress", Assignee: "agent-y",
	}

	out := captureReadyStdout(t, func() { printNewsBead(parked) })
	if !strings.Contains(out, "gd-ip1") || !strings.Contains(out, "[parked: gd-decN]") {
		t.Errorf("parked in-progress row missing badge:\n%s", out)
	}

	out2 := captureReadyStdout(t, func() { printNewsBead(free) })
	if strings.Contains(out2, "parked") {
		t.Errorf("free in-progress row must not be marked parked:\n%s", out2)
	}
}
