package main

import (
	"fmt"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/references"
)

// parentMentionsOldParent mirrors what the server's replaceBeadReferences does
// on every write: run the typed-mention parser and check whether it would
// re-materialize a parent-child edge to oldParent. It matches ANY parent-edge
// verb (parent OR the inverse child-of) since both produce the identical
// parent-child dep row — the exact set the write path would resurrect. Used to
// prove no resurrection after a reparent description rewrite.
func parentMentionsOldParent(desc, oldParent string) bool {
	for _, ref := range references.ExtractTyped(desc) {
		if references.IsParentEdgeVerb(ref.Verb) && ref.TargetID == oldParent {
			return true
		}
	}
	return false
}

// setReparentFlags sets the reparent command flags for a test and restores
// their defaults on cleanup so tests don't leak flag state into each other.
func setReparentFlags(t *testing.T, to string, keepRelated, dryRun bool) {
	t.Helper()
	if err := reparentCmd.Flags().Set("to", to); err != nil {
		t.Fatalf("set --to: %v", err)
	}
	if keepRelated {
		if err := reparentCmd.Flags().Set("keep-related", "true"); err != nil {
			t.Fatalf("set --keep-related: %v", err)
		}
	}
	if dryRun {
		if err := reparentCmd.Flags().Set("dry-run", "true"); err != nil {
			t.Fatalf("set --dry-run: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = reparentCmd.Flags().Set("to", "")
		_ = reparentCmd.Flags().Set("keep-related", "false")
		_ = reparentCmd.Flags().Set("dry-run", "false")
	})
}

func TestReparent_MovesEdgeAndRewritesDescription(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldP  = "gd-6OVtkTtBbx"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{
		ID:          child,
		Description: "Some work.\n\nparent: " + oldP + "\nrelated: gd-EAaC6mP1FC",
	}
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: oldP, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	setReparentFlags(t, newP, false, false)

	out := captureStdout(t, func() {
		if err := runReparent(reparentCmd, []string{child}); err != nil {
			t.Fatalf("runReparent: %v", err)
		}
	})

	// New edge added (child -> newP, parent-child).
	if len(mc.AddDepCalls) != 1 {
		t.Fatalf("expected 1 AddDependency call, got %d", len(mc.AddDepCalls))
	}
	add := mc.AddDepCalls[0]
	if add.BeadID != child || add.DependsOnID != newP || add.Type != string(model.DepParentChild) {
		t.Errorf("AddDependency = %+v; want child=%s dependsOn=%s type=parent-child", add, child, newP)
	}

	// Old edge removed (child -> oldP, parent-child).
	if len(mc.RemoveDepCalls) != 1 {
		t.Fatalf("expected 1 RemoveDependency call, got %d", len(mc.RemoveDepCalls))
	}
	rm := mc.RemoveDepCalls[0]
	if rm.BeadID != child || rm.DependsOnID != oldP || rm.Type != string(model.DepParentChild) {
		t.Errorf("RemoveDependency = %+v; want child=%s dependsOn=%s type=parent-child", rm, child, oldP)
	}

	// Description rewritten to point parent at newP, with old parent gone.
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}
	upd := mc.UpdateBeadCalls[0]
	if upd.Req.Description == nil {
		t.Fatal("UpdateBead description is nil; expected a rewrite")
	}
	newDesc := *upd.Req.Description
	if strings.Contains(newDesc, "parent: "+oldP) {
		t.Errorf("rewritten description still names old parent:\n%s", newDesc)
	}
	if !strings.Contains(newDesc, "parent: "+newP) {
		t.Errorf("rewritten description does not name new parent:\n%s", newDesc)
	}
	// Unrelated related: mention preserved.
	if !strings.Contains(newDesc, "related: gd-EAaC6mP1FC") {
		t.Errorf("unrelated related mention lost:\n%s", newDesc)
	}

	if !strings.Contains(out, "Parent mentions rewritten (title/description/notes): 1") {
		t.Errorf("expected rewrite count in output, got:\n%s", out)
	}
}

// TestReparent_NoEdgeResurrection is the acceptance test for ka-0000007756:
// after reparent rewrites the description, feeding that SAME rewritten text
// back through the typed-mention parser (what replaceBeadReferences does on
// every subsequent write) must NOT surface a parent edge to the old parent —
// i.e. a follow-up description write cannot resurrect the removed edge.
func TestReparent_NoEdgeResurrection(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldP  = "gd-6OVtkTtBbx"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{
		ID:          child,
		Description: "parent: " + oldP,
	}
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: oldP, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	setReparentFlags(t, newP, false, false)

	if err := runReparent(reparentCmd, []string{child}); err != nil {
		t.Fatalf("runReparent: %v", err)
	}

	newDesc := *mc.UpdateBeadCalls[0].Req.Description

	// Simulate the server re-extracting typed deps on the next write. Use the
	// helper the CLI test package already shares with the server path.
	if parentMentionsOldParent(newDesc, oldP) {
		t.Errorf("resurrection risk: rewritten description still yields parent->%s:\n%s", oldP, newDesc)
	}
}

func TestReparent_KeepRelatedAddsBackReference(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldP  = "gd-6OVtkTtBbx"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{ID: child, Description: "parent: " + oldP}
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: oldP, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	setReparentFlags(t, newP, true, false)

	if err := runReparent(reparentCmd, []string{child}); err != nil {
		t.Fatalf("runReparent: %v", err)
	}
	newDesc := *mc.UpdateBeadCalls[0].Req.Description
	if !strings.Contains(newDesc, "related: "+oldP) {
		t.Errorf("--keep-related did not add related back-reference:\n%s", newDesc)
	}
}

// TestReparent_ScrubsTitleAndNotesFields is the regression lock for the
// multi-field gap (ka-0000007756, source-verified): replaceBeadReferences
// re-extracts parent mentions from title AND description AND notes on every
// write, so a stale `parent: gd-OLD` in the title or notes resurrects the edge
// exactly as one in the description does. reparent must scrub all three.
func TestReparent_ScrubsTitleAndNotesFields(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldP  = "gd-6OVtkTtBbx"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{
		ID:          child,
		Title:       "fix parent(" + oldP + ")",
		Description: "no parent mention here",
		Notes:       "child-of: " + oldP,
	}
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: oldP, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	setReparentFlags(t, newP, false, false)

	if err := runReparent(reparentCmd, []string{child}); err != nil {
		t.Fatalf("runReparent: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}
	req := mc.UpdateBeadCalls[0].Req
	// Title parent mention repointed.
	if req.Title == nil || parentMentionsOldParent(*req.Title, oldP) {
		t.Errorf("title still names old parent: %+v", req.Title)
	}
	if req.Title == nil || !parentMentionsOldParent(*req.Title, newP) {
		t.Errorf("title does not name new parent: %+v", req.Title)
	}
	// Notes child-of mention repointed.
	if req.Notes == nil || parentMentionsOldParent(*req.Notes, oldP) {
		t.Errorf("notes still names old parent: %+v", req.Notes)
	}
	if req.Notes == nil || !parentMentionsOldParent(*req.Notes, newP) {
		t.Errorf("notes does not name new parent: %+v", req.Notes)
	}
	// Description had no parent mention → not sent as an update field.
	if req.Description != nil {
		t.Errorf("description was rewritten but had no parent mention: %+v", req.Description)
	}
}

// TestReparent_StaleTextOnlyParentScrubbed covers the resurrection footgun
// directly: the old parent-child EDGE was already removed manually, but the
// `parent: gd-OLD` TEXT was left behind. reparent must still rewrite that
// text even though there is no live edge to remove.
func TestReparent_StaleTextOnlyParentScrubbed(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldP  = "gd-6OVtkTtBbx"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{ID: child, Description: "parent: " + oldP}
	// No parent-child dep row — the edge is already gone, only stale text.
	mc.Deps[child] = nil
	withMockClient(t, mc)
	setReparentFlags(t, newP, false, false)

	if err := runReparent(reparentCmd, []string{child}); err != nil {
		t.Fatalf("runReparent: %v", err)
	}
	// No old edge to remove.
	if len(mc.RemoveDepCalls) != 0 {
		t.Errorf("expected 0 RemoveDependency calls (no live old edge), got %d", len(mc.RemoveDepCalls))
	}
	// But the stale text is still scrubbed.
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected description rewrite of stale text, got %d UpdateBead calls", len(mc.UpdateBeadCalls))
	}
	newDesc := *mc.UpdateBeadCalls[0].Req.Description
	if strings.Contains(newDesc, "parent: "+oldP) {
		t.Errorf("stale text-only parent not scrubbed:\n%s", newDesc)
	}
}

func TestReparent_DryRunAppliesNothing(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldP  = "gd-6OVtkTtBbx"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{ID: child, Description: "parent: " + oldP}
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: oldP, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	setReparentFlags(t, newP, false, true)

	out := captureStdout(t, func() {
		if err := runReparent(reparentCmd, []string{child}); err != nil {
			t.Fatalf("runReparent: %v", err)
		}
	})
	if len(mc.AddDepCalls) != 0 || len(mc.RemoveDepCalls) != 0 || len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("dry-run mutated state: add=%d remove=%d update=%d",
			len(mc.AddDepCalls), len(mc.RemoveDepCalls), len(mc.UpdateBeadCalls))
	}
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected DRY RUN banner, got:\n%s", out)
	}
}

func TestReparent_RejectsSelfParent(t *testing.T) {
	const child = "ka-0000003755"
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{ID: child}
	withMockClient(t, mc)
	setReparentFlags(t, child, false, false)

	err := runReparent(reparentCmd, []string{child})
	if err == nil {
		t.Fatal("expected error when reparenting a bead onto itself")
	}
	if !strings.Contains(err.Error(), "itself") {
		t.Errorf("error = %v; expected a self-parent rejection", err)
	}
}

func TestReparent_RequiresTo(t *testing.T) {
	const child = "ka-0000003755"
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{ID: child}
	withMockClient(t, mc)
	setReparentFlags(t, "", false, false)

	err := runReparent(reparentCmd, []string{child})
	if err == nil {
		t.Fatal("expected error when --to is empty")
	}
	if !strings.Contains(err.Error(), "--to") {
		t.Errorf("error = %v; expected a --to-required message", err)
	}
}

// TestReparent_InverseChildOfNoResurrection is the regression lock for the
// child-of resurrection gap (ka-0000007756 review): a bead whose description
// uses the INVERSE `child-of` surface form materializes the SAME parent-child
// edge as `parent`, so reparent must repoint it too — otherwise removing the
// edge and leaving the `child-of: gd-OLD` text resurrects it on the next write.
func TestReparent_InverseChildOfNoResurrection(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldP  = "gd-6OVtkTtBbx"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{
		ID:          child,
		Description: "history\n\nchild-of: " + oldP + "\nchild-of(" + oldP + ") also",
	}
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: oldP, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	setReparentFlags(t, newP, false, false)

	if err := runReparent(reparentCmd, []string{child}); err != nil {
		t.Fatalf("runReparent: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected description rewrite of child-of text, got %d UpdateBead calls", len(mc.UpdateBeadCalls))
	}
	newDesc := *mc.UpdateBeadCalls[0].Req.Description
	// No parent-edge mention (parent OR child-of) of the old parent may survive.
	if parentMentionsOldParent(newDesc, oldP) {
		t.Errorf("resurrection risk: child-of mention of old parent survived:\n%s", newDesc)
	}
	// The new parent must be reachable as a parent edge (via child-of here).
	if !parentMentionsOldParent(newDesc, newP) {
		t.Errorf("expected a parent-edge mention of new parent after rewrite:\n%s", newDesc)
	}
}

// TestReparent_CycleGuardAbortLeavesOldEdge pins the fail-safe apply order: the
// new-parent edge is added FIRST so the server's cycle guard runs before any
// destructive step. If AddDependency fails (e.g. the move would create a
// cycle), reparent must abort WITHOUT removing the old edge or rewriting the
// description — leaving the bead exactly where it was.
func TestReparent_CycleGuardAbortLeavesOldEdge(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldP  = "gd-6OVtkTtBbx"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{ID: child, Description: "parent: " + oldP}
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: oldP, Type: model.DepParentChild},
	}
	// Simulate the server rejecting the add (cycle guard / FailedPrecondition).
	mc.AddDepErr = fmt.Errorf("parent-child dependency %s -> %s would create a cycle", child, newP)
	withMockClient(t, mc)
	setReparentFlags(t, newP, false, false)

	err := runReparent(reparentCmd, []string{child})
	if err == nil {
		t.Fatal("expected reparent to fail when the new-parent add is rejected")
	}
	if !strings.Contains(err.Error(), "adding new parent-child edge") {
		t.Errorf("error = %v; expected it to mention the failed add", err)
	}
	// NOTHING destructive may have happened: no old edge removed, no rewrite.
	if len(mc.RemoveDepCalls) != 0 {
		t.Errorf("cycle-guard abort still removed %d edge(s); old parent must be preserved", len(mc.RemoveDepCalls))
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("cycle-guard abort still rewrote the description (%d update calls)", len(mc.UpdateBeadCalls))
	}
}

// TestReparent_MultipleStaleParents covers the documented multi-parent cleanup:
// a bead that accumulated several parent-child edges (from prior manual churn)
// must have ALL of them removed and ALL parent mentions repointed to the single
// new parent.
func TestReparent_MultipleStaleParents(t *testing.T) {
	const (
		child = "ka-0000003755"
		oldA  = "gd-QX2HjNRNk7"
		oldB  = "gd-oafvu0y3CL"
		newP  = "ka-0000002510"
	)
	mc := newMockClient()
	mc.Beads[child] = &model.Bead{
		ID:          child,
		Description: "parent: " + oldA + "\nchild-of(" + oldB + ")",
	}
	mc.Deps[child] = []*model.Dependency{
		{BeadID: child, DependsOnID: oldA, Type: model.DepParentChild},
		{BeadID: child, DependsOnID: oldB, Type: model.DepParentChild},
	}
	withMockClient(t, mc)
	setReparentFlags(t, newP, false, false)

	if err := runReparent(reparentCmd, []string{child}); err != nil {
		t.Fatalf("runReparent: %v", err)
	}
	// Both stale edges removed.
	removed := map[string]bool{}
	for _, rm := range mc.RemoveDepCalls {
		if rm.Type == string(model.DepParentChild) {
			removed[rm.DependsOnID] = true
		}
	}
	if !removed[oldA] || !removed[oldB] {
		t.Errorf("expected both %s and %s removed, got removals: %+v", oldA, oldB, mc.RemoveDepCalls)
	}
	// Description scrubbed of BOTH old parents (parent + child-of forms).
	newDesc := *mc.UpdateBeadCalls[0].Req.Description
	if parentMentionsOldParent(newDesc, oldA) || parentMentionsOldParent(newDesc, oldB) {
		t.Errorf("a stale parent mention survived:\n%s", newDesc)
	}
	if !parentMentionsOldParent(newDesc, newP) {
		t.Errorf("expected new parent mention after rewrite:\n%s", newDesc)
	}
}
