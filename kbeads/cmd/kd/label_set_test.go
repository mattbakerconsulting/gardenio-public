package main

import (
	"strconv"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// label_set_test.go covers `kd label set` / `kd label replace` — the atomic label
// MOVE verbs added for bug ka-0000003820. The headline property under test is
// ATOMICITY: a move must leave as ONE UpdateBead call carrying the complete
// desired set, never an AddLabel+RemoveLabel pair that can strand the bead
// carrying both the old and the new label.

// setLabelSetFlag sets a bool flag on one of the label MOVE commands for a single
// test, restoring the PRIOR value afterwards (the commands are package-level and
// shared — same reason setLabelRemoveIfExists does it).
func setLabelSetFlag(t *testing.T, cmd *cobra.Command, name string, v bool) {
	t.Helper()
	f := cmd.Flags().Lookup(name)
	if f == nil {
		t.Fatalf("no --%s flag registered on %s", name, cmd.Name())
	}
	prev := f.Value.String()
	if err := cmd.Flags().Set(name, strconv.FormatBool(v)); err != nil {
		t.Fatalf("set --%s=%v: %v", name, v, err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set(name, prev) })
}

// beadWithLabels registers a bead carrying `labels` on the mock, which both
// GetBead (the read half of every MOVE) and the label calls consult.
func beadWithLabels(mc *mockBeadsClient, id string, labels ...string) {
	mc.Beads[id] = &model.Bead{ID: id, Labels: labels}
	mc.Labels[id] = labels
}

// TestLabelSet_IsOneAtomicUpdateCall is the core atomicity regression: setting a
// label set must produce exactly ONE UpdateBead carrying the full desired set,
// and ZERO Add/RemoveLabel calls. If this ever regresses to the add+remove pair,
// the double-scoped intermediate state ka-0000003820 is about becomes reachable.
func TestLabelSet_IsOneAtomicUpdateCall(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "scope:project-broadcast", "project:gardenio")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelSetCmd, "dry-run", false)
	setLabelSetFlag(t, labelSetCmd, "allow-empty", false)

	if err := labelSetCmd.RunE(labelSetCmd, []string{bead, "scope:on-demand"}); err != nil {
		t.Fatalf("label set: %v", err)
	}

	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times; want exactly 1 (the move must be atomic)", len(mc.UpdateBeadCalls))
	}
	got := mc.UpdateBeadCalls[0].Req.Labels
	if len(got) != 1 || got[0] != "scope:on-demand" {
		t.Errorf("PATCH labels = %v; want exactly [scope:on-demand] — the complete desired set", got)
	}
	// The whole point: no per-label add/remove round-trips.
	if len(mc.AddLabelCalls) != 0 || len(mc.RemoveLabelCalls) != 0 {
		t.Errorf("got %d AddLabel + %d RemoveLabel calls; want 0 of each (one PATCH does both halves)",
			len(mc.AddLabelCalls), len(mc.RemoveLabelCalls))
	}
}

// TestLabelReplace_SwapsOneLabelKeepingOthers covers `replace`'s reason to exist
// over `set`: the caller names only the label being moved, and everything else
// survives — so a label cannot be dropped by omission.
func TestLabelReplace_SwapsOneLabelKeepingOthers(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "project:gardenio", "role:crew", "kind:ced-audit")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelReplaceCmd, "dry-run", false)
	setLabelSetFlag(t, labelReplaceCmd, "if-exists", false)

	if err := labelReplaceCmd.RunE(labelReplaceCmd, []string{bead, "role:crew", "role:dataplane-fixer"}); err != nil {
		t.Fatalf("label replace: %v", err)
	}

	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times; want 1", len(mc.UpdateBeadCalls))
	}
	got := strings.Join(mc.UpdateBeadCalls[0].Req.Labels, ",")
	// Position preserved so the printed diff reads as a swap, not a reshuffle.
	if got != "project:gardenio,role:dataplane-fixer,kind:ced-audit" {
		t.Errorf("labels = %q; want the swap in place with the other labels intact", got)
	}
}

// TestLabelReplace_AbsentOldLabelIsNotSuccess is the typo guard: replacing a
// label the bead never carried must fail loudly and write NOTHING, so a
// misspelled label name cannot read as a completed move. Mirrors
// `kd label remove`'s absent-label error (ka-0000000424).
func TestLabelReplace_AbsentOldLabelIsNotSuccess(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "project:gardenio")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelReplaceCmd, "dry-run", false)
	setLabelSetFlag(t, labelReplaceCmd, "if-exists", false)

	err := labelReplaceCmd.RunE(labelReplaceCmd, []string{bead, "not-a-real-label", "role:crew"})
	if err == nil {
		t.Fatal("replacing an absent label returned nil (exit 0) — the no-op reported success")
	}
	if !strings.Contains(err.Error(), "no such label") {
		t.Errorf("error = %q; want it to contain %q", err.Error(), "no such label")
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("wrote %d UpdateBead calls; want 0 — a failed swap must not mutate", len(mc.UpdateBeadCalls))
	}
}

// TestLabelReplace_IfExistsDegradesToEnsurePresent covers the scripted path:
// with --if-exists an absent old label is not an error, and the verb instead
// guarantees the new label is present (the idempotent end state).
func TestLabelReplace_IfExistsDegradesToEnsurePresent(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "project:gardenio")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelReplaceCmd, "dry-run", false)
	setLabelSetFlag(t, labelReplaceCmd, "if-exists", true)

	if err := labelReplaceCmd.RunE(labelReplaceCmd, []string{bead, "gone", "scope:on-demand"}); err != nil {
		t.Fatalf("label replace --if-exists: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times; want 1", len(mc.UpdateBeadCalls))
	}
	got := strings.Join(mc.UpdateBeadCalls[0].Req.Labels, ",")
	if got != "project:gardenio,scope:on-demand" {
		t.Errorf("labels = %q; want the new label appended to the existing set", got)
	}
}

// TestLabelSet_NoOpDoesNotWrite ensures "already in the target state" is
// distinguishable from "changed" and burns no write. A success line that prints
// identically either way is unfalsifiable.
func TestLabelSet_NoOpDoesNotWrite(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "project:gardenio", "role:crew")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelSetCmd, "dry-run", false)
	setLabelSetFlag(t, labelSetCmd, "allow-empty", false)

	if err := labelSetCmd.RunE(labelSetCmd, []string{bead, "project:gardenio", "role:crew"}); err != nil {
		t.Fatalf("label set: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("wrote %d UpdateBead calls; want 0 for an unchanged label set", len(mc.UpdateBeadCalls))
	}
}

// TestLabelSet_DryRunWritesNothing locks --dry-run as read-only.
func TestLabelSet_DryRunWritesNothing(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "scope:project-broadcast")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelSetCmd, "dry-run", true)
	setLabelSetFlag(t, labelSetCmd, "allow-empty", false)

	if err := labelSetCmd.RunE(labelSetCmd, []string{bead, "scope:on-demand"}); err != nil {
		t.Fatalf("label set --dry-run: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 0 || len(mc.RemoveLabelCalls) != 0 {
		t.Errorf("--dry-run mutated state (%d updates, %d removes); want none",
			len(mc.UpdateBeadCalls), len(mc.RemoveLabelCalls))
	}
}

// TestLabelSet_RefusesEmptySetByDefault guards the destructive case: an
// accidentally-empty argument (unquoted glob, empty shell var) would otherwise
// strip a bead's project:/targeting labels and drop it out of every scoped query.
func TestLabelSet_RefusesEmptySetByDefault(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "project:gardenio")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelSetCmd, "dry-run", false)
	setLabelSetFlag(t, labelSetCmd, "allow-empty", false)

	err := labelSetCmd.RunE(labelSetCmd, []string{bead, ""})
	if err == nil {
		t.Fatal("an empty label set was accepted; want a refusal")
	}
	if !strings.Contains(err.Error(), "refusing to remove every label") {
		t.Errorf("error = %q; want the empty-set refusal", err.Error())
	}
	if len(mc.UpdateBeadCalls) != 0 || len(mc.RemoveLabelCalls) != 0 {
		t.Errorf("the refused call still mutated state; want no writes")
	}
}

// TestLabelSet_AllowEmptyStripsViaRemoveNotEmptyPatch covers the deliberate
// wipe. It must NOT go out as `labels: []` — the daemon infers set-ness from a
// NON-EMPTY array, so an empty one is an ambiguous no-op-or-wipe. The verb falls
// back to per-label removes instead.
func TestLabelSet_AllowEmptyStripsViaRemoveNotEmptyPatch(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "project:gardenio", "role:crew")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelSetCmd, "dry-run", false)
	setLabelSetFlag(t, labelSetCmd, "allow-empty", true)

	if err := labelSetCmd.RunE(labelSetCmd, []string{bead, ""}); err != nil {
		t.Fatalf("label set --allow-empty: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("sent %d UpdateBead calls; want 0 — an empty PATCH array is ambiguous",
			len(mc.UpdateBeadCalls))
	}
	if len(mc.RemoveLabelCalls) != 2 {
		t.Errorf("made %d RemoveLabel calls; want 2 (one per surviving label)", len(mc.RemoveLabelCalls))
	}
}

// TestLabelSet_DeduplicatesRepeatedArgs ensures a doubled arg cannot produce a
// duplicate label row.
func TestLabelSet_DeduplicatesRepeatedArgs(t *testing.T) {
	const bead = "gd-tdtofv8g2z"
	mc := newMockClient()
	beadWithLabels(mc, bead, "old")
	withMockClient(t, mc)
	setLabelSetFlag(t, labelSetCmd, "dry-run", false)
	setLabelSetFlag(t, labelSetCmd, "allow-empty", false)

	if err := labelSetCmd.RunE(labelSetCmd, []string{bead, "role:crew", "role:crew"}); err != nil {
		t.Fatalf("label set: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times; want 1", len(mc.UpdateBeadCalls))
	}
	if got := mc.UpdateBeadCalls[0].Req.Labels; len(got) != 1 {
		t.Errorf("labels = %v; want a single deduplicated label", got)
	}
}

// TestLabelSetDiff reports exactly what a write changes, which is what makes the
// printed success line falsifiable rather than a bare "Set label(s)".
func TestLabelSetDiff(t *testing.T) {
	added, removed := labelSetDiff(
		[]string{"keep", "drop-a", "drop-b"},
		[]string{"keep", "new"})

	if strings.Join(added, ",") != "new" {
		t.Errorf("added = %v; want [new]", added)
	}
	if strings.Join(removed, ",") != "drop-a,drop-b" {
		t.Errorf("removed = %v; want [drop-a drop-b]", removed)
	}
}

// TestLabelSet_MissingBeadIsNotALabelWipe guards the read half: every MOVE verb
// is read-then-set, so a typo'd bead id must fail loudly rather than treating an
// empty read as "this bead has no labels" and writing over it.
func TestLabelSet_MissingBeadIsNotALabelWipe(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	setLabelSetFlag(t, labelSetCmd, "dry-run", false)
	setLabelSetFlag(t, labelSetCmd, "allow-empty", false)

	err := labelSetCmd.RunE(labelSetCmd, []string{"gd-nonexistent", "role:crew"})
	if err == nil {
		t.Fatal("setting labels on a nonexistent bead returned nil; want an error")
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("wrote to a nonexistent bead; want no writes")
	}
}
