package main

import (
	"fmt"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestSquashCmd_BasicSquash(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Test mol", Type: "molecule", Status: model.StatusOpen,
	}
	mc.Beads["gd-child-1"] = &model.Bead{
		ID: "gd-child-1", Title: "Step A", Type: "task", Status: model.StatusOpen,
	}
	mc.Beads["gd-child-2"] = &model.Bead{
		ID: "gd-child-2", Title: "Step B", Type: "task", Status: model.StatusClosed,
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-child-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
		{BeadID: "gd-child-2", DependsOnID: "gd-mol-1", Type: "parent-child"},
		{BeadID: "gd-related", DependsOnID: "gd-mol-1", Type: "related"}, // Not a child.
	}
	withMockClient(t, mc)

	cmd := newSquashCmd()
	cmd.SetArgs([]string{"gd-mol-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("squash: %v", err)
		}
	})

	// Should add a digest comment.
	if len(mc.AddCommentCalls) != 1 {
		t.Fatalf("expected 1 AddComment call, got %d", len(mc.AddCommentCalls))
	}
	comment := mc.AddCommentCalls[0]
	if comment.BeadID != "gd-mol-1" {
		t.Errorf("comment bead = %q, want gd-mol-1", comment.BeadID)
	}
	// Auto-generated digest should contain child titles.
	if !strings.Contains(comment.Text, "Step A") || !strings.Contains(comment.Text, "Step B") {
		t.Errorf("digest missing child titles, got:\n%s", comment.Text)
	}

	// Should close only the open child (child-1), skip already-closed child-2.
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("expected 1 CloseBead call, got %d", len(mc.CloseBeadCalls))
	}
	if mc.CloseBeadCalls[0].ID != "gd-child-1" {
		t.Errorf("closed bead = %q, want gd-child-1", mc.CloseBeadCalls[0].ID)
	}

	if !strings.Contains(out, "1 children closed") {
		t.Errorf("output missing close count, got:\n%s", out)
	}
}

func TestSquashCmd_CustomSummary(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "My mol", Type: "molecule",
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{}
	withMockClient(t, mc)

	cmd := newSquashCmd()
	cmd.SetArgs([]string{"gd-mol-1", "--summary", "Custom digest text"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("squash with summary: %v", err)
		}
	})

	if len(mc.AddCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(mc.AddCommentCalls))
	}
	if mc.AddCommentCalls[0].Text != "Custom digest text" {
		t.Errorf("comment text = %q, want 'Custom digest text'", mc.AddCommentCalls[0].Text)
	}
}

func TestSquashCmd_CloseMol(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "My mol", Type: "molecule",
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{}
	withMockClient(t, mc)

	cmd := newSquashCmd()
	cmd.SetArgs([]string{"gd-mol-1", "--close-mol"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("squash --close-mol: %v", err)
		}
	})

	// Should close the molecule itself.
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("expected 1 CloseBead call (the molecule), got %d", len(mc.CloseBeadCalls))
	}
	if mc.CloseBeadCalls[0].ID != "gd-mol-1" {
		t.Errorf("closed = %q, want gd-mol-1", mc.CloseBeadCalls[0].ID)
	}
	if !strings.Contains(out, "Molecule closed") {
		t.Errorf("output missing molecule closed, got:\n%s", out)
	}
}

func TestSquashCmd_DryRun(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Dry mol", Type: "molecule",
	}
	mc.Beads["gd-child-1"] = &model.Bead{
		ID: "gd-child-1", Title: "Step X", Type: "task", Status: model.StatusOpen,
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-child-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
	}
	withMockClient(t, mc)

	cmd := newSquashCmd()
	cmd.SetArgs([]string{"gd-mol-1", "--dry-run", "--close-mol"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("dry-run: %v", err)
		}
	})

	// No mutations.
	if len(mc.AddCommentCalls) != 0 {
		t.Errorf("dry-run made %d comment calls, want 0", len(mc.AddCommentCalls))
	}
	if len(mc.CloseBeadCalls) != 0 {
		t.Errorf("dry-run made %d close calls, want 0", len(mc.CloseBeadCalls))
	}

	if !strings.Contains(out, "Would squash molecule gd-mol-1") {
		t.Errorf("output missing dry-run preview, got:\n%s", out)
	}
	if !strings.Contains(out, "Would also close the molecule") {
		t.Errorf("output missing close-mol preview, got:\n%s", out)
	}
}

func TestSquashCmd_NotMolecule(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-task"] = &model.Bead{ID: "gd-task", Type: "epic"}
	withMockClient(t, mc)

	cmd := newSquashCmd()
	cmd.SetArgs([]string{"gd-task"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-molecule")
	}
	if !strings.Contains(err.Error(), "not molecule") {
		t.Errorf("error = %q, want 'not molecule'", err)
	}
}

func TestSquashCmd_ChildCloseError(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Fail mol", Type: "molecule",
	}
	mc.Beads["gd-child-1"] = &model.Bead{
		ID: "gd-child-1", Title: "Step", Type: "task", Status: model.StatusOpen,
	}
	mc.Beads["gd-child-2"] = &model.Bead{
		ID: "gd-child-2", Title: "Step 2", Type: "task", Status: model.StatusOpen,
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-child-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
		{BeadID: "gd-child-2", DependsOnID: "gd-mol-1", Type: "parent-child"},
	}
	mc.CloseErrs["gd-child-1"] = fmt.Errorf("already closed")
	withMockClient(t, mc)

	cmd := newSquashCmd()
	cmd.SetArgs([]string{"gd-mol-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("squash with partial failure: %v", err)
		}
	})

	// Should still try to close child-2 despite child-1 failure.
	if len(mc.CloseBeadCalls) != 2 {
		t.Errorf("expected 2 close attempts, got %d", len(mc.CloseBeadCalls))
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("output should warn about close failure, got:\n%s", out)
	}
	// Only 1 successful close.
	if !strings.Contains(out, "1 children closed") {
		t.Errorf("output = %q, want '1 children closed'", out)
	}
}

func TestSquashCmd_AutoDigestFormat(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Auth refactor", Type: "molecule",
	}
	mc.Beads["gd-child-1"] = &model.Bead{
		ID: "gd-child-1", Title: "Extract tokens", Type: "task", Status: model.StatusOpen,
	}
	mc.Beads["gd-child-2"] = &model.Bead{
		ID: "gd-child-2", Title: "Write tests", Type: "task", Status: model.StatusClosed,
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-child-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
		{BeadID: "gd-child-2", DependsOnID: "gd-mol-1", Type: "parent-child"},
	}
	withMockClient(t, mc)

	cmd := newSquashCmd()
	cmd.SetArgs([]string{"gd-mol-1"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("squash: %v", err)
		}
	})

	digest := mc.AddCommentCalls[0].Text
	if !strings.Contains(digest, "Squash digest for Auth refactor") {
		t.Errorf("digest missing title header, got:\n%s", digest)
	}
	if !strings.Contains(digest, "[open] Extract tokens") {
		t.Errorf("digest missing open child status, got:\n%s", digest)
	}
	if !strings.Contains(digest, "[closed] Write tests") {
		t.Errorf("digest missing closed child status, got:\n%s", digest)
	}
}
