package main

import (
	"fmt"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestBurnCmd_BasicForce(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Test mol", Type: "molecule", Status: model.StatusOpen,
	}
	mc.Beads["gd-child-1"] = &model.Bead{ID: "gd-child-1", Type: "task"}
	mc.Beads["gd-child-2"] = &model.Bead{ID: "gd-child-2", Type: "task"}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-child-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
		{BeadID: "gd-child-2", DependsOnID: "gd-mol-1", Type: "parent-child"},
		{BeadID: "gd-other", DependsOnID: "gd-mol-1", Type: "related"}, // Not a child.
	}
	withMockClient(t, mc)

	cmd := newBurnCmd()
	cmd.SetArgs([]string{"gd-mol-1", "--force"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("burn: %v", err)
		}
	})

	// Should delete 2 children + molecule.
	if got := len(mc.DeleteBeadCalls); got != 3 {
		t.Fatalf("expected 3 DeleteBead calls, got %d", got)
	}
	if mc.DeleteBeadCalls[0] != "gd-child-1" {
		t.Errorf("first delete = %q, want gd-child-1", mc.DeleteBeadCalls[0])
	}
	if mc.DeleteBeadCalls[1] != "gd-child-2" {
		t.Errorf("second delete = %q, want gd-child-2", mc.DeleteBeadCalls[1])
	}
	if mc.DeleteBeadCalls[2] != "gd-mol-1" {
		t.Errorf("third delete = %q, want gd-mol-1", mc.DeleteBeadCalls[2])
	}

	if !strings.Contains(out, "Burned molecule gd-mol-1") {
		t.Errorf("output missing confirmation, got:\n%s", out)
	}
	if !strings.Contains(out, "2 children deleted") {
		t.Errorf("output missing child count, got:\n%s", out)
	}
}

func TestBurnCmd_DryRun(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Dry mol", Type: "molecule",
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-child-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
	}
	withMockClient(t, mc)

	cmd := newBurnCmd()
	cmd.SetArgs([]string{"gd-mol-1", "--dry-run"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("dry-run: %v", err)
		}
	})

	// Should not delete anything.
	if len(mc.DeleteBeadCalls) != 0 {
		t.Errorf("dry-run deleted %d beads, want 0", len(mc.DeleteBeadCalls))
	}
	if !strings.Contains(out, "Would burn molecule gd-mol-1") {
		t.Errorf("output missing dry-run preview, got:\n%s", out)
	}
	if !strings.Contains(out, "gd-child-1") {
		t.Errorf("output missing child ID, got:\n%s", out)
	}
}

func TestBurnCmd_RequiresForce(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Protected mol", Type: "molecule",
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-child-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
	}
	withMockClient(t, mc)

	cmd := newBurnCmd()
	cmd.SetArgs([]string{"gd-mol-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("burn without force: %v", err)
		}
	})

	// Should not delete anything without --force.
	if len(mc.DeleteBeadCalls) != 0 {
		t.Errorf("burn without --force deleted %d beads, want 0", len(mc.DeleteBeadCalls))
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("output should mention --force, got:\n%s", out)
	}
}

func TestBurnCmd_NotMolecule(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-task"] = &model.Bead{ID: "gd-task", Type: "task"}
	withMockClient(t, mc)

	cmd := newBurnCmd()
	cmd.SetArgs([]string{"gd-task"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-molecule")
	}
	if !strings.Contains(err.Error(), "not molecule") {
		t.Errorf("error = %q, want 'not molecule'", err)
	}
}

func TestBurnCmd_ChildDeleteError(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Fail mol", Type: "molecule",
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-child-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
		{BeadID: "gd-child-2", DependsOnID: "gd-mol-1", Type: "parent-child"},
	}
	mc.DeleteErrs["gd-child-1"] = fmt.Errorf("permission denied")
	withMockClient(t, mc)

	cmd := newBurnCmd()
	cmd.SetArgs([]string{"gd-mol-1", "--force"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("burn with partial failure: %v", err)
		}
	})

	// Should still attempt to delete child-2 and the molecule despite child-1 failure.
	if !strings.Contains(out, "warning") {
		t.Errorf("output should warn about child-1 failure, got:\n%s", out)
	}
	if len(mc.DeleteBeadCalls) != 3 {
		t.Errorf("expected 3 delete calls (even with failure), got %d", len(mc.DeleteBeadCalls))
	}
}

func TestBurnCmd_NoChildren(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Empty mol", Type: "molecule",
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{}
	withMockClient(t, mc)

	// No children means no --force required.
	cmd := newBurnCmd()
	cmd.SetArgs([]string{"gd-mol-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("burn empty mol: %v", err)
		}
	})

	if len(mc.DeleteBeadCalls) != 1 {
		t.Errorf("expected 1 delete call (just molecule), got %d", len(mc.DeleteBeadCalls))
	}
	if !strings.Contains(out, "0 children deleted") {
		t.Errorf("output = %q, want '0 children deleted'", out)
	}
}
