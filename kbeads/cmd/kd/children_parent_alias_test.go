package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// ka-0000008107: the live gardenio graph carries a large body of legacy
// parent-child edges typed literally "parent" (pre-canonicalization,
// child-first) alongside canonical "parent-child" edges. `kd children <epic>
// --type parent-child` filters the reverse-dep edge type client-side, so before
// the fix it returned zero for epics whose children hang off "parent" edges —
// which made the triage-intake leaf-check treat every legacy tree-root as a
// leaf. These tests lock in that either alias matches BOTH edge shapes.

// childrenTable runs `kd children <epic> --type <filter>` (human table output)
// against the mock and returns the raw stdout.
func childrenTable(t *testing.T, mc *mockBeadsClient, epic, filterType string) string {
	t.Helper()
	withMockClient(t, mc)
	if err := childrenCmd.Flags().Set("type", filterType); err != nil {
		t.Fatalf("set --type: %v", err)
	}
	defer func() { _ = childrenCmd.Flags().Set("type", "") }()

	return captureStdout(t, func() {
		if err := childrenCmd.RunE(childrenCmd, []string{epic}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
}

// mixedEpic wires an epic with one legacy "parent" child and one canonical
// "parent-child" child, plus an unrelated "blocks" edge that must never match.
func mixedEpic(mc *mockBeadsClient, epic, legacyChild, canonChild, blockChild string) {
	mc.RevDeps[epic] = []*model.Dependency{
		{BeadID: legacyChild, DependsOnID: epic, Type: model.DepParentChildLegacy},
		{BeadID: canonChild, DependsOnID: epic, Type: model.DepParentChild},
		{BeadID: blockChild, DependsOnID: epic, Type: model.DepBlocks},
	}
	for _, id := range []string{legacyChild, canonChild, blockChild} {
		mc.Beads[id] = &model.Bead{ID: id, Type: model.TypeTask, Status: model.StatusOpen, Title: id}
	}
}

func TestChildren_TypeParentChild_MatchesLegacyParentEdge(t *testing.T) {
	mc := newMockClient()
	epic, legacy, canon, blocked := "gd-epicmix01", "gd-legacyc01", "gd-canonc001", "gd-blockc001"
	mixedEpic(mc, epic, legacy, canon, blocked)

	// Asking for the canonical filter must ALSO surface the legacy "parent"
	// child — the core ka-0000008107 regression.
	out := childrenTable(t, mc, epic, string(model.DepParentChild))
	if !strings.Contains(out, legacy) {
		t.Errorf("--type parent-child dropped legacy 'parent' child %q\noutput:\n%s", legacy, out)
	}
	if !strings.Contains(out, canon) {
		t.Errorf("--type parent-child dropped canonical child %q\noutput:\n%s", canon, out)
	}
	if strings.Contains(out, blocked) {
		t.Errorf("--type parent-child leaked the blocks-edge child %q\noutput:\n%s", blocked, out)
	}
}

func TestChildren_TypeParent_MatchesCanonicalParentChildEdge(t *testing.T) {
	mc := newMockClient()
	epic, legacy, canon, blocked := "gd-epicmix02", "gd-legacyc02", "gd-canonc002", "gd-blockc002"
	mixedEpic(mc, epic, legacy, canon, blocked)

	// The inverse alias must be symmetric: `--type parent` also surfaces the
	// canonical "parent-child" child.
	out := childrenTable(t, mc, epic, string(model.DepParentChildLegacy))
	if !strings.Contains(out, legacy) {
		t.Errorf("--type parent dropped legacy 'parent' child %q\noutput:\n%s", legacy, out)
	}
	if !strings.Contains(out, canon) {
		t.Errorf("--type parent dropped canonical 'parent-child' child %q\noutput:\n%s", canon, out)
	}
	if strings.Contains(out, blocked) {
		t.Errorf("--type parent leaked the blocks-edge child %q\noutput:\n%s", blocked, out)
	}
}

// TestChildren_TypeNonParentUnaffected guards that the alias widening is scoped
// to the parent-child family — a non-hierarchy filter like "blocks" must still
// match exactly, never widening into parent edges.
func TestChildren_TypeNonParentUnaffected(t *testing.T) {
	mc := newMockClient()
	epic, legacy, canon, blocked := "gd-epicmix03", "gd-legacyc03", "gd-canonc003", "gd-blockc003"
	mixedEpic(mc, epic, legacy, canon, blocked)

	out := childrenTable(t, mc, epic, string(model.DepBlocks))
	if !strings.Contains(out, blocked) {
		t.Errorf("--type blocks dropped the blocks-edge child %q\noutput:\n%s", blocked, out)
	}
	if strings.Contains(out, legacy) || strings.Contains(out, canon) {
		t.Errorf("--type blocks must not match parent-child edges\noutput:\n%s", out)
	}
}
