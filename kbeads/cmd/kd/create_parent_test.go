package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// TestCreateCmd_ParentTaskSentAsParentBeadID pins gd-kcCrpdrlm2: `kd create
// --parent gd-X` hands the parent to the daemon as parent_bead_id, so the
// server wires the parent-child dep inside the create transaction and
// create-time gates (BEADS_HERALDRY_GATE on a plan's phase epic) see it. No
// follow-up AddDependency is issued — that post-create write is what hid the
// parent from the gate.
func TestCreateCmd_ParentTaskSentAsParentBeadID(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	setFlag(t, "type", "epic")
	setFlag(t, "project", "gardenio")
	setFlag(t, "parent-task", "gd-PLAN1")
	defer resetCreateFlags(t)
	defer func() { _ = createCmd.Flags().Set("parent-task", "") }()

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a phase"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", got)
	}
	if got := mc.CreateBeadCalls[0].ParentBeadID; got != "gd-PLAN1" {
		t.Errorf("ParentBeadID = %q, want gd-PLAN1", got)
	}
	if got := len(mc.AddDepCalls); got != 0 {
		t.Errorf("expected no AddDependency call (parent-child wired by CreateBead.ParentBeadID), got %d: %+v", got, mc.AddDepCalls)
	}
}

// TestCreateCmd_NoParent_LeavesParentBeadIDEmpty — --no-parent beats an
// ambient --parent-task, so no parent_bead_id reaches the daemon.
func TestCreateCmd_NoParent_LeavesParentBeadIDEmpty(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	setFlag(t, "project", "gardenio")
	setFlag(t, "parent-task", "gd-PLAN1")
	setFlag(t, "no-parent", "true")
	defer resetCreateFlags(t)
	defer func() {
		_ = createCmd.Flags().Set("parent-task", "")
		_ = createCmd.Flags().Set("no-parent", "false")
	}()

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a loose task"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := mc.CreateBeadCalls[0].ParentBeadID; got != "" {
		t.Errorf("ParentBeadID = %q, want empty under --no-parent", got)
	}
}

// unknownParentClient refuses any create that names a parent the way the
// daemon does for a parent_bead_id with no bead behind it.
type unknownParentClient struct {
	*mockBeadsClient
}

func (c unknownParentClient) CreateBead(ctx context.Context, req *client.CreateBeadRequest) (*model.Bead, error) {
	if req.ParentBeadID != "" {
		c.mu.Lock()
		c.CreateBeadCalls = append(c.CreateBeadCalls, req)
		c.mu.Unlock()
		return nil, errors.New("HTTP 400: parent_bead_id refers to unknown bead " + req.ParentBeadID)
	}
	return c.mockBeadsClient.CreateBead(ctx, req)
}

// TestCreateCmd_UnknownParent_CreatesUnparentedWithWarning keeps the old
// non-fatal contract: --parent-task defaults to $BOAT_TASK_ID, so a stale
// parent must warn, not cost the create.
func TestCreateCmd_UnknownParent_CreatesUnparentedWithWarning(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	beadsClient = unknownParentClient{mc}

	setFlag(t, "project", "gardenio")
	setFlag(t, "parent-task", "gd-GONE")
	defer resetCreateFlags(t)
	defer func() { _ = createCmd.Flags().Set("parent-task", "") }()

	var stderr string
	_ = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := createCmd.RunE(createCmd, []string{"a task"}); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if got := len(mc.CreateBeadCalls); got != 2 {
		t.Fatalf("expected the refused create then an unparented retry (2 calls), got %d", got)
	}
	if got := mc.CreateBeadCalls[1].ParentBeadID; got != "" {
		t.Errorf("retry ParentBeadID = %q, want empty", got)
	}
	if !strings.Contains(stderr, "parent-task gd-GONE not found") {
		t.Errorf("expected an unknown-parent warning on stderr, got %q", stderr)
	}
}
