package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Common fixture: closed decision parented under a task, with a child report
// bead that lacks the parent-child edge to the task.
func setupDecisionUnderTask(t *testing.T) *mockBeadsClient {
	t.Helper()
	mc := newMockClient()

	decFields, _ := json.Marshal(map[string]string{})
	decision := &model.Bead{
		ID:     "gd-decision-1",
		Type:   "decision",
		Status: model.StatusClosed,
		Fields: decFields,
	}
	report := &model.Bead{
		ID:     "gd-report-1",
		Type:   "report",
		Status: model.StatusClosed,
	}
	mc.Beads["gd-decision-1"] = decision
	mc.Beads["gd-report-1"] = report
	mc.ListBeadsResult = []*model.Bead{decision}

	// Decision is parented under gd-task-orig.
	mc.Deps["gd-decision-1"] = []*model.Dependency{
		{BeadID: "gd-decision-1", DependsOnID: "gd-task-orig", Type: model.DepParentChild},
	}
	// Report has no edges yet (other than the report→decision one, which lives
	// in the reverse-deps lookup).
	mc.Deps["gd-report-1"] = nil
	// Reverse deps: report is a parent-child child of the decision.
	mc.RevDeps["gd-decision-1"] = []*model.Dependency{
		{BeadID: "gd-report-1", DependsOnID: "gd-decision-1", Type: model.DepParentChild},
	}
	return mc
}

func TestAdminLinkDecisionReports_AddsMissingEdge(t *testing.T) {
	mc := setupDecisionUnderTask(t)

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer func() {
		_ = adminLinkDecisionReportsCmd.Flags().Set("dry-run", "false")
		_ = adminLinkDecisionReportsCmd.Flags().Set("limit", "1000")
	}()

	out := captureStdout(t, func() {
		if err := adminLinkDecisionReportsCmd.RunE(adminLinkDecisionReportsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.AddDepCalls); got != 1 {
		t.Fatalf("expected 1 AddDependency call, got %d", got)
	}
	got := mc.AddDepCalls[0]
	if got.BeadID != "gd-report-1" || got.DependsOnID != "gd-task-orig" || got.Type != string(model.DepParentChild) {
		t.Errorf("unexpected AddDependency: %+v", got)
	}
	for _, want := range []string{
		"linked: gd-report-1 → gd-task-orig",
		"edges added:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminLinkDecisionReports_DryRunNoWrites(t *testing.T) {
	mc := setupDecisionUnderTask(t)

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminLinkDecisionReportsCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set dry-run: %v", err)
	}
	defer func() { _ = adminLinkDecisionReportsCmd.Flags().Set("dry-run", "false") }()

	out := captureStdout(t, func() {
		if err := adminLinkDecisionReportsCmd.RunE(adminLinkDecisionReportsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.AddDepCalls); got != 0 {
		t.Fatalf("dry-run wrote %d edges; expected 0", got)
	}
	for _, want := range []string{
		"dry-run",
		"would link: gd-report-1 → gd-task-orig",
		"would add",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAdminLinkDecisionReports_IdempotentSkip(t *testing.T) {
	mc := setupDecisionUnderTask(t)
	// Pre-existing edge on the report → originating task.
	mc.Deps["gd-report-1"] = []*model.Dependency{
		{BeadID: "gd-report-1", DependsOnID: "gd-task-orig", Type: model.DepParentChild},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminLinkDecisionReportsCmd.RunE(adminLinkDecisionReportsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.AddDepCalls); got != 0 {
		t.Fatalf("expected no writes when edge already exists; got %d", got)
	}
	if !strings.Contains(out, "edges skipped (already linked): 1") {
		t.Errorf("expected skipped count, got:\n%s", out)
	}
}

func TestAdminLinkDecisionReports_AgentParentFallback(t *testing.T) {
	mc := newMockClient()

	decFields, _ := json.Marshal(map[string]string{
		"requesting_agent_bead_id": "gd-agent-x",
	})
	decision := &model.Bead{
		ID:     "gd-decision-1",
		Type:   "decision",
		Status: model.StatusClosed,
		Fields: decFields,
	}
	report := &model.Bead{
		ID:     "gd-report-1",
		Type:   "report",
		Status: model.StatusClosed,
	}
	mc.Beads["gd-decision-1"] = decision
	mc.Beads["gd-report-1"] = report
	mc.ListBeadsResult = []*model.Bead{decision}

	// Decision has NO parent-child parent.
	mc.Deps["gd-decision-1"] = nil
	// Agent IS parented under gd-task-via-agent.
	mc.Deps["gd-agent-x"] = []*model.Dependency{
		{BeadID: "gd-agent-x", DependsOnID: "gd-task-via-agent", Type: model.DepParentChild},
	}
	mc.Deps["gd-report-1"] = nil
	mc.RevDeps["gd-decision-1"] = []*model.Dependency{
		{BeadID: "gd-report-1", DependsOnID: "gd-decision-1", Type: model.DepParentChild},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminLinkDecisionReportsCmd.RunE(adminLinkDecisionReportsCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if got := len(mc.AddDepCalls); got != 1 {
		t.Fatalf("expected 1 edge from agent-parent fallback, got %d", got)
	}
	if mc.AddDepCalls[0].DependsOnID != "gd-task-via-agent" {
		t.Errorf("got %s; want gd-task-via-agent", mc.AddDepCalls[0].DependsOnID)
	}
}

func TestAdminLinkDecisionReports_StandaloneSkipped(t *testing.T) {
	mc := newMockClient()

	decFields, _ := json.Marshal(map[string]string{})
	decision := &model.Bead{
		ID:     "gd-decision-1",
		Type:   "decision",
		Status: model.StatusClosed,
		Fields: decFields,
	}
	report := &model.Bead{
		ID:     "gd-report-1",
		Type:   "report",
		Status: model.StatusClosed,
	}
	mc.Beads["gd-decision-1"] = decision
	mc.Beads["gd-report-1"] = report
	mc.ListBeadsResult = []*model.Bead{decision}
	// No deps anywhere — decision is standalone.
	mc.RevDeps["gd-decision-1"] = []*model.Dependency{
		{BeadID: "gd-report-1", DependsOnID: "gd-decision-1", Type: model.DepParentChild},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := adminLinkDecisionReportsCmd.RunE(adminLinkDecisionReportsCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.AddDepCalls); got != 0 {
		t.Fatalf("expected no edges for standalone decision, got %d", got)
	}
	if !strings.Contains(out, "standalone (no originating task): 1") {
		t.Errorf("expected standalone counter, got:\n%s", out)
	}
}

func TestAdminLinkDecisionReports_LimitStopsScan(t *testing.T) {
	mc := newMockClient()
	// 3 closed decisions, but --limit=2 should stop after the second.
	for i, id := range []string{"gd-d1", "gd-d2", "gd-d3"} {
		mc.Beads[id] = &model.Bead{ID: id, Type: "decision", Status: model.StatusClosed}
		// Each has a parent task and a report child.
		taskID := "gd-task-" + string(rune('a'+i))
		reportID := "gd-r" + string(rune('1'+i))
		mc.Beads[reportID] = &model.Bead{ID: reportID, Type: "report", Status: model.StatusClosed}
		mc.Deps[id] = []*model.Dependency{
			{BeadID: id, DependsOnID: taskID, Type: model.DepParentChild},
		}
		mc.Deps[reportID] = nil
		mc.RevDeps[id] = []*model.Dependency{
			{BeadID: reportID, DependsOnID: id, Type: model.DepParentChild},
		}
	}
	mc.ListBeadsResult = []*model.Bead{
		mc.Beads["gd-d1"], mc.Beads["gd-d2"], mc.Beads["gd-d3"],
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminLinkDecisionReportsCmd.Flags().Set("limit", "2"); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	defer func() { _ = adminLinkDecisionReportsCmd.Flags().Set("limit", "1000") }()

	if err := adminLinkDecisionReportsCmd.RunE(adminLinkDecisionReportsCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if got := len(mc.AddDepCalls); got != 2 {
		t.Errorf("limit=2 wrote %d edges; want 2", got)
	}
}
