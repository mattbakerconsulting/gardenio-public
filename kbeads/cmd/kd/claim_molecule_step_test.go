package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// claim_molecule_step_test.go pins the ka-0000006622 fix: an agent holding its
// assignment/iteration bead can `kd claim` a molecule STEP poured for that
// assignment — the single-claim guard exempts a step whose molecule descends
// from (parent-child) or was triggered/sourced by (triggered-by) the held
// bead. When lineage can't be established the guard error still fires, now
// pointing at the `kd update --assignee --status=in_progress` workaround.

const (
	heldBeadID = "gd-sumpsrk20v" // the assignment/iteration bead the agent already holds
	molID      = "gd-e0y0rhxg15" // molecule poured for the held bead
	stepID     = "gd-ggmv80trjs" // a step of that molecule
)

// heldInProgress seeds the mock so the actor already holds heldBeadID
// in_progress (kind=issue), which is what checkNoClaimed lists.
func heldInProgress(mc *mockBeadsClient) {
	held := &model.Bead{
		ID:     heldBeadID,
		Title:  "Iter 1: drive the poured molecule",
		Kind:   model.KindIssue,
		Type:   model.TypeTask,
		Status: model.StatusInProgress,
	}
	mc.Beads[heldBeadID] = held
	mc.ListBeadsResult = []*model.Bead{held}
}

// step builds a step bead whose parent-child parent is molID.
func stepUnder(mol string) *model.Bead {
	return &model.Bead{
		ID:     stepID,
		Title:  "step: do the thing",
		Kind:   model.KindIssue,
		Type:   model.TypeTask,
		Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: stepID, DependsOnID: mol, Type: model.DepParentChild},
		},
	}
}

// TestCheckNoClaimed_ExemptsMoleculeStep_ParentChild proves a step is claimable
// while holding the bead its molecule is parented under.
func TestCheckNoClaimed_ExemptsMoleculeStep_ParentChild(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	heldInProgress(mc)
	mc.Beads[stepID] = stepUnder(molID)
	mc.Beads[molID] = &model.Bead{
		ID:   molID,
		Type: model.TypeMolecule,
		Dependencies: []*model.Dependency{
			{BeadID: molID, DependsOnID: heldBeadID, Type: model.DepParentChild},
		},
	}

	if err := checkNoClaimed(context.Background(), stepID); err != nil {
		t.Fatalf("step under molecule parented by held bead should be exempt, got: %v", err)
	}
}

// TestCheckNoClaimed_ExemptsMoleculeStep_TriggeredBy proves the triggered-by
// molecule→held edge (the source-bead lineage from formula pour) also exempts.
func TestCheckNoClaimed_ExemptsMoleculeStep_TriggeredBy(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	heldInProgress(mc)
	mc.Beads[stepID] = stepUnder(molID)
	mc.Beads[molID] = &model.Bead{
		ID:   molID,
		Type: model.TypeMolecule,
		Dependencies: []*model.Dependency{
			{BeadID: molID, DependsOnID: heldBeadID, Type: model.DepTriggeredBy},
		},
	}

	if err := checkNoClaimed(context.Background(), stepID); err != nil {
		t.Fatalf("step under molecule triggered-by held bead should be exempt, got: %v", err)
	}
}

// TestCheckNoClaimed_ExemptsMoleculeStep_SourceBeadIDsField proves the
// source_bead_ids provenance field on the molecule exempts even when no dep
// edge names the held bead.
func TestCheckNoClaimed_ExemptsMoleculeStep_SourceBeadIDsField(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	heldInProgress(mc)
	mc.Beads[stepID] = stepUnder(molID)
	mc.Beads[molID] = &model.Bead{
		ID:     molID,
		Type:   model.TypeMolecule,
		Fields: json.RawMessage(`{"source_bead_ids":["` + heldBeadID + `"]}`),
	}

	if err := checkNoClaimed(context.Background(), stepID); err != nil {
		t.Fatalf("step under molecule with source_bead_ids=held should be exempt, got: %v", err)
	}
}

// TestCheckNoClaimed_ExemptsNestedMoleculeStep proves a step nested one level
// below the molecule (step→substep) still resolves up to the molecule.
func TestCheckNoClaimed_ExemptsNestedMoleculeStep(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	heldInProgress(mc)

	const parentStepID = "gd-b33rxxa8ne"
	// substep → parent step → molecule → held
	mc.Beads[stepID] = &model.Bead{
		ID: stepID, Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: stepID, DependsOnID: parentStepID, Type: model.DepParentChild},
		},
	}
	mc.Beads[parentStepID] = &model.Bead{
		ID: parentStepID, Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{
			{BeadID: parentStepID, DependsOnID: molID, Type: model.DepParentChild},
		},
	}
	mc.Beads[molID] = &model.Bead{
		ID: molID, Type: model.TypeMolecule,
		Dependencies: []*model.Dependency{
			{BeadID: molID, DependsOnID: heldBeadID, Type: model.DepParentChild},
		},
	}

	if err := checkNoClaimed(context.Background(), stepID); err != nil {
		t.Fatalf("nested step should resolve up to molecule and be exempt, got: %v", err)
	}
}

// TestCheckNoClaimed_RejectsUnrelatedStep proves the guard still fires for a
// molecule step whose molecule does NOT trace back to the held bead, and that
// the error points at the kd update workaround.
func TestCheckNoClaimed_RejectsUnrelatedStep(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	heldInProgress(mc)
	mc.Beads[stepID] = stepUnder(molID)
	mc.Beads[molID] = &model.Bead{
		ID:   molID,
		Type: model.TypeMolecule,
		Dependencies: []*model.Dependency{
			{BeadID: molID, DependsOnID: "gd-berameq1lq", Type: model.DepParentChild},
		},
	}

	err := checkNoClaimed(context.Background(), stepID)
	if err == nil {
		t.Fatal("step under an UNRELATED molecule must not be exempt")
	}
	if !strings.Contains(err.Error(), "kd update "+stepID+" --assignee") {
		t.Errorf("guard error should point at the `kd update --assignee --status` workaround, got:\n%s", err)
	}
	if !strings.Contains(err.Error(), "--status=in_progress") {
		t.Errorf("workaround hint should include --status=in_progress, got:\n%s", err)
	}
}

// TestCheckNoClaimed_RejectsPlainBead proves an ordinary (non-step) target is
// still rejected when the agent already holds a bead.
func TestCheckNoClaimed_RejectsPlainBead(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	heldInProgress(mc)
	const otherID = "gd-jiuidp8blv"
	mc.Beads[otherID] = &model.Bead{
		ID: otherID, Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
		// No parent-child parent — not a molecule step.
	}

	err := checkNoClaimed(context.Background(), otherID)
	if err == nil {
		t.Fatal("claiming an unrelated plain bead while holding another must be rejected")
	}
	if !strings.Contains(err.Error(), "you already have claimed bead "+heldBeadID) {
		t.Errorf("guard error should name the held bead, got:\n%s", err)
	}
}

// TestCheckNoClaimed_NoHeldBead_AllowsClaim proves the guard is a no-op when the
// agent holds nothing in progress.
func TestCheckNoClaimed_NoHeldBead_AllowsClaim(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	// ListBeadsResult defaults to empty — no in-progress beads.
	if err := checkNoClaimed(context.Background(), stepID); err != nil {
		t.Fatalf("with no held bead the guard must allow the claim, got: %v", err)
	}
}

// TestBeadFieldStringSlice covers the array, single-string, and miss paths of
// the provenance-field extractor.
func TestBeadFieldStringSlice(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		key  string
		want []string
	}{
		{"array", `{"source_bead_ids":["gd-a","gd-b"]}`, "source_bead_ids", []string{"gd-a", "gd-b"}},
		{"single-string", `{"source_bead_ids":"gd-a"}`, "source_bead_ids", []string{"gd-a"}},
		{"missing-key", `{"other":["gd-a"]}`, "source_bead_ids", nil},
		{"empty-fields", ``, "source_bead_ids", nil},
		{"not-json", `not json`, "source_bead_ids", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := beadFieldStringSlice(json.RawMessage(tc.raw), tc.key)
			if len(got) != len(tc.want) {
				t.Fatalf("beadFieldStringSlice(%q,%q) = %v, want %v", tc.raw, tc.key, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("beadFieldStringSlice(%q,%q) = %v, want %v", tc.raw, tc.key, got, tc.want)
				}
			}
		})
	}
}
