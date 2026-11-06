package main

import (
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/pflag"
)

// TestList_ParentFlag locks in that `kd list --parent <epic>` round-trips the
// epic id into ListBeadsRequest.Parent (which the HTTP client maps to the
// ?parent= subtree query / filter.ParentSubtreeID). This is the REVERSE
// child query the meta-gardener scorer must use to count an epic's children:
// it counts beads whose parent-child edge points AT the epic, NOT the epic's
// own forward depends_on edges (which point UP at a grandparent and would
// 0-out the count). Regression guard for the false STALE-STOP in
// ka-0000000208.
func TestList_ParentFlag(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-Child001", Title: "child", Type: "task", Status: "open"}}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListParentFlags(t)
	if err := listCmd.Flags().Set("parent", "gd-Epic0000001"); err != nil {
		t.Fatalf("setting --parent: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	req := mc.ListBeadsCalls[0]
	if req.Parent != "gd-Epic0000001" {
		t.Errorf("Parent = %q, want gd-Epic0000001", req.Parent)
	}
}

// TestList_ParentFlag_Unset confirms that when --parent is NOT passed, the
// request carries an empty Parent (no subtree filter leaks in from the flag
// default). Mirrors the unset case in TestList_MentionsFlag.
func TestList_ParentFlag_Unset(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-AAA", Title: "x", Type: "task"}}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListParentFlags(t)

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	if got := mc.ListBeadsCalls[0].Parent; got != "" {
		t.Errorf("Parent = %q, want empty (flag unset)", got)
	}
}

// resetListParentFlags clears the listCmd flags this file mutates so a
// previous subtest's setting doesn't leak across RunE calls within one
// process. Mirrors resetListMentionFlags in list_mentions_test.go.
func resetListParentFlags(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "parent" {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	})
}
