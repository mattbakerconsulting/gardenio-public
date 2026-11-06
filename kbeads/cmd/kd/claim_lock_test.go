package main

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
)

// claim_lock_test.go pins gd-SlLPKtUGWZ on the client: a lock bead (label
// `lock`) is not work, so the single-claim rule neither refuses taking one
// while the run holds its task nor counts a held one, and a lock's current
// holder is left for the daemon to judge live or dead.

const lockBeadID = "gd-z2z1F4nEJJ"

func lockBead(holder string) *model.Bead {
	return &model.Bead{
		ID: lockBeadID, Title: "merge-desk pick lock", Kind: model.KindIssue, Type: model.TypeTask,
		Status: model.StatusOpen, Assignee: holder, Labels: []string{"lock", "project:gardenio"},
	}
}

func TestCheckNoClaimed_LockNotRefusedWhileHoldingTask(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	heldInProgress(mc)
	mc.Beads[lockBeadID] = lockBead("")

	if err := checkNoClaimed(context.Background(), lockBeadID); err != nil {
		t.Fatalf("claiming a lock bead while holding a task must pass, got: %v", err)
	}
}

func TestCheckNoClaimed_HeldLockDoesNotCount(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	held := lockBead(actor)
	held.Status = model.StatusInProgress
	mc.Beads[lockBeadID] = held
	mc.ListBeadsResult = []*model.Bead{held}
	mc.Beads["gd-task"] = &model.Bead{ID: "gd-task", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	if err := checkNoClaimed(context.Background(), "gd-task"); err != nil {
		t.Fatalf("a held lock must not count as a claimed work item, got: %v", err)
	}
}

func TestCheckNoClaimed_PlainTaskStillRefused(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	heldInProgress(mc)
	mc.Beads["gd-task"] = &model.Bead{ID: "gd-task", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	if err := checkNoClaimed(context.Background(), "gd-task"); err == nil {
		t.Fatal("a second plain task must still be refused")
	}
}

func TestCheckClaimable_HeldLockLeftToDaemon(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	mc.Beads[lockBeadID] = lockBead("some-other-run")

	if err := checkClaimable(context.Background(), lockBeadID); err != nil {
		t.Fatalf("a held lock is the daemon's call (it refuses a live holder), got: %v", err)
	}
	mc.Beads["gd-task"] = &model.Bead{ID: "gd-task", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, Assignee: "some-other-run"}
	if err := checkClaimable(context.Background(), "gd-task"); err == nil {
		t.Fatal("a plain task claimed by another run must still be refused")
	}
}
