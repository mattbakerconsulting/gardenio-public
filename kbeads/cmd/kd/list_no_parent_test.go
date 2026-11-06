package main

import (
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/pflag"
)

// TestList_NoParentFlag locks in that `kd list --no-parent` round-trips into
// ListBeadsRequest.NoParent, which the HTTP client maps to ?no_parent=true /
// filter.NoParent — the server-side orphan predicate that lets the
// triage-intake sweep ask for parent-less open work in one query instead of
// the N+1 `kd dep list` loop it ran before. Regression guard for ka-0000005267.
func TestList_NoParentFlag(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-jjKwPTXzFF", Title: "orphan", Type: "task", Status: "open"}}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListNoParentFlags(t)
	if err := listCmd.Flags().Set("no-parent", "true"); err != nil {
		t.Fatalf("setting --no-parent: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	if !mc.ListBeadsCalls[0].NoParent {
		t.Errorf("NoParent = false, want true")
	}
}

// TestList_NoParentFlag_Unset confirms that without --no-parent the request
// carries NoParent=false (the flag default never leaks a spurious orphan
// filter into an ordinary `kd list`).
func TestList_NoParentFlag_Unset(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-AAA", Title: "x", Type: "task"}}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListNoParentFlags(t)

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	if mc.ListBeadsCalls[0].NoParent {
		t.Errorf("NoParent = true, want false (flag unset)")
	}
}

// resetListNoParentFlags clears the listCmd flags this file mutates so a
// previous subtest's setting doesn't leak across RunE calls within one
// process. Mirrors resetListParentFlags in list_parent_test.go.
func resetListNoParentFlags(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "no-parent" {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	})
}
