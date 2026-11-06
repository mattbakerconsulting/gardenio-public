package server

import (
	"context"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// findMutation returns the first row in ms.mutations for (beadID, mutationType, field), or nil.
func findMutation(ms *mockStore, beadID, mutationType, field string) *model.MutationRow {
	for _, m := range ms.mutations {
		if m.BeadID == beadID && m.MutationType == mutationType && m.Field == field {
			return m
		}
	}
	return nil
}

func TestMutationHistory_Create(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "alice")
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{Title: "Hello", Type: "task"})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	row := findMutation(ms, resp.Bead.Id, model.MutationTypeCreate, "*")
	if row == nil {
		t.Fatalf("expected create mutation row for %s, got %+v", resp.Bead.Id, ms.mutations)
	}
	if row.Actor != "alice" {
		t.Fatalf("expected actor=alice, got %q", row.Actor)
	}
}

// TestMutationHistory_Create_OperatedByPair pins the gd-uJkKs75lH1 fix:
// when a bead is created and the resolver yields a non-empty operator, the
// create path emits a paired field='operated_by' mutation_type='create' row
// alongside the field='*' birth marker. Without this row, bead_operator_history
// is empty for every newly-created bead because the dibs flip path
// short-circuits on initial create (current==value).
func TestMutationHistory_Create_OperatedByPair(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "matt@acme.example")
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{Title: "Hello", Type: "task"})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	starRow := findMutation(ms, resp.Bead.Id, model.MutationTypeCreate, "*")
	if starRow == nil {
		t.Fatalf("expected field='*' create row for %s", resp.Bead.Id)
	}
	opRow := findMutation(ms, resp.Bead.Id, model.MutationTypeCreate, "operated_by")
	if opRow == nil {
		t.Fatalf("expected field='operated_by' create row for %s, got %+v", resp.Bead.Id, ms.mutations)
	}
	const want = "oidc:matt@acme.example"
	if opRow.NewValue != want || opRow.OperatedBy != want {
		t.Errorf("operated_by row: NewValue=%q OperatedBy=%q; want %q for both", opRow.NewValue, opRow.OperatedBy, want)
	}
	if opRow.OldValue != "" {
		t.Errorf("operated_by row OldValue=%q; want empty", opRow.OldValue)
	}
	if opRow.Actor != "matt@acme.example" {
		t.Errorf("operated_by row Actor=%q; want matt@acme.example", opRow.Actor)
	}
}

func TestMutationHistory_Update(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "bob")
	ms.beads["gd-mut-upd"] = &model.Bead{
		ID: "gd-mut-upd", Title: "Original", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
	}
	newTitle := "Updated"
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-mut-upd", Title: &newTitle}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	row := findMutation(ms, "gd-mut-upd", model.MutationTypeUpdate, "title")
	if row == nil {
		t.Fatalf("expected update mutation for title, got %+v", ms.mutations)
	}
	if row.OldValue != "Original" || row.NewValue != "Updated" {
		t.Fatalf("expected Original->Updated, got %q->%q", row.OldValue, row.NewValue)
	}
	if row.Actor != "bob" {
		t.Fatalf("expected actor=bob, got %q", row.Actor)
	}
}

// TestMutationHistory_DibsFlip_ProtectsAutomationOwner pins ka-0000007567:
// when a bead is currently owned by an automation-class operator (fleet:*,
// system:*, trigger:*, schedule:*), a mutation by a human operator must NOT
// flip beads.operated_by_id. Without this guard, the first agent's `kd claim`
// against a jira-bridge-created task silently re-attributed the task from
// `fleet:jira-bridge` to the agent's inherited human operator.
func TestMutationHistory_DibsFlip_ProtectsAutomationOwner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
	}{
		{"fleet", "fleet:jira-bridge"},
		{"system", "system:scheduler"},
		{"trigger", "trigger:ka-0000002740"},
		{"schedule", "schedule:gd-abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ms, ctx := testCtxAs(t, "matt@acme.example")
			ms.beads["gd-dibs-prot"] = &model.Bead{
				ID: "gd-dibs-prot", Title: "Original",
				Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
				OperatedBy: tc.current,
			}
			newTitle := "Updated"
			if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-dibs-prot", Title: &newTitle}); err != nil {
				t.Fatalf("UpdateBead: %v", err)
			}
			b := ms.beads["gd-dibs-prot"]
			if b.OperatedBy != tc.current {
				t.Errorf("dibs flipped protected current operator: got %q, want %q", b.OperatedBy, tc.current)
			}
			if row := findMutation(ms, "gd-dibs-prot", model.MutationTypeUpdate, "operated_by"); row != nil {
				t.Errorf("expected NO synthetic operated_by mutation row for protected current; got %+v", row)
			}
		})
	}
}

// TestMutationHistory_DibsFlip_AllowsHumanToHumanFlip confirms the protection
// is narrow: a human-owned bead is still subject to the standard dibs rule
// (R3 §4), and a mutation by a different human still flips the operator.
func TestMutationHistory_DibsFlip_AllowsHumanToHumanFlip(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "alice@acme.example")
	ms.beads["gd-dibs-h2h"] = &model.Bead{
		ID: "gd-dibs-h2h", Title: "Original",
		Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
		OperatedBy: "oidc:bob@acme.example",
	}
	newTitle := "Updated"
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-dibs-h2h", Title: &newTitle}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	b := ms.beads["gd-dibs-h2h"]
	if b.OperatedBy != "oidc:alice@acme.example" {
		t.Errorf("expected dibs flip to oidc:alice@acme.example, got %q", b.OperatedBy)
	}
	if row := findMutation(ms, "gd-dibs-h2h", model.MutationTypeUpdate, "operated_by"); row == nil {
		t.Errorf("expected synthetic operated_by mutation row for human-to-human flip")
	}
}

// TestMutationHistory_DibsFlip_MachineNeverTakesDibs pins gd-Ndam8t6aMb: a
// machine principal's write (machine:gardenio-beads-viewer, the controller)
// is recorded with the machine as operator but never flips the bead's
// operated_by away from its human owner. Before the fix the viewer's
// routine writes moved the owner's plans out of /plans Mine.
func TestMutationHistory_DibsFlip_MachineNeverTakesDibs(t *testing.T) {
	for _, client := range []string{"gardenio-beads-viewer", "gardenio-controller"} {
		t.Run(client, func(t *testing.T) {
			srv, ms, _ := testCtx(t)
			ms.beads["gd-dibs-mach"] = &model.Bead{
				ID: "gd-dibs-mach", Title: "Plan",
				Kind: model.KindIssue, Type: model.TypeEpic, Status: model.StatusOpen,
				OperatedBy: "oidc:matt@acme.example",
			}
			ctx := machineCtx(client)
			srv.recordMutation(ctx, "gd-dibs-mach", "machine:"+client, "title", "Plan", "Plan v2", model.MutationTypeUpdate)

			parent := findMutation(ms, "gd-dibs-mach", model.MutationTypeUpdate, "title")
			if parent == nil {
				t.Fatalf("expected the parent title mutation to be recorded, got %+v", ms.mutations)
			}
			if want := "machine:" + client; parent.OperatedBy != want {
				t.Errorf("parent mutation OperatedBy=%q, want %q", parent.OperatedBy, want)
			}
			if got := ms.beads["gd-dibs-mach"].OperatedBy; got != "oidc:matt@acme.example" {
				t.Errorf("machine write flipped dibs: operated_by=%q, want oidc:matt@acme.example", got)
			}
			if row := findMutation(ms, "gd-dibs-mach", model.MutationTypeUpdate, "operated_by"); row != nil {
				t.Errorf("expected NO synthetic operated_by row for a machine write; got %+v", row)
			}
		})
	}
}

// TestMutationHistory_DibsFlip_HumanReclaimsFromMachine confirms a bead left
// held by machine:* (pre-fix history) is not protected: the next human write
// flips it back to the human.
func TestMutationHistory_DibsFlip_HumanReclaimsFromMachine(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "matt@acme.example")
	ms.beads["gd-dibs-recl"] = &model.Bead{
		ID: "gd-dibs-recl", Title: "Plan",
		Kind: model.KindIssue, Type: model.TypeEpic, Status: model.StatusOpen,
		OperatedBy: "machine:gardenio-beads-viewer",
	}
	newTitle := "Plan v2"
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-dibs-recl", Title: &newTitle}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	if got := ms.beads["gd-dibs-recl"].OperatedBy; got != "oidc:matt@acme.example" {
		t.Errorf("human write did not reclaim from machine: operated_by=%q", got)
	}
	row := findMutation(ms, "gd-dibs-recl", model.MutationTypeUpdate, "operated_by")
	if row == nil {
		t.Fatalf("expected a synthetic operated_by flip row")
	}
	if row.OldValue != "machine:gardenio-beads-viewer" || row.NewValue != "oidc:matt@acme.example" {
		t.Errorf("flip row %q -> %q, want machine:gardenio-beads-viewer -> oidc:matt@acme.example", row.OldValue, row.NewValue)
	}
}

func TestTakesDibs(t *testing.T) {
	for _, tc := range []struct {
		op   string
		want bool
	}{
		{"", false},
		{"system:scheduler", false},
		{"fleet:jira-bridge", false},
		{"machine:gardenio-beads-viewer", false},
		{"machine:gardenio-controller", false},
		{"oidc:matt@acme.example", true},
	} {
		if got := takesDibs(tc.op); got != tc.want {
			t.Errorf("takesDibs(%q)=%v, want %v", tc.op, got, tc.want)
		}
	}
	if isProtectedDibsCurrent("machine:gardenio-beads-viewer") {
		t.Error("a machine-held bead must not be protected from a human reclaim")
	}
}

func TestMutationHistory_Close(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "carol")
	ms.beads["gd-mut-cls"] = &model.Bead{
		ID: "gd-mut-cls", Title: "Open", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
	}
	if _, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-mut-cls"}); err != nil {
		t.Fatalf("CloseBead: %v", err)
	}
	row := findMutation(ms, "gd-mut-cls", model.MutationTypeUpdate, "status")
	if row == nil {
		t.Fatalf("expected status mutation for close, got %+v", ms.mutations)
	}
	if row.OldValue != "open" || row.NewValue != "closed" {
		t.Fatalf("expected open->closed, got %q->%q", row.OldValue, row.NewValue)
	}
}

func TestMutationHistory_Delete(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "dave")
	ms.beads["gd-mut-del"] = &model.Bead{
		ID: "gd-mut-del", Title: "Doomed", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
	}
	if _, err := srv.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "gd-mut-del"}); err != nil {
		t.Fatalf("DeleteBead: %v", err)
	}
	row := findMutation(ms, "gd-mut-del", model.MutationTypeDelete, "*")
	if row == nil {
		t.Fatalf("expected delete mutation, got %+v", ms.mutations)
	}
	if row.Actor != "dave" {
		t.Fatalf("expected actor=dave, got %q", row.Actor)
	}
}

func TestMutationHistory_ListMutations_Chronological(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "eve")
	ms.beads["gd-mut-list"] = &model.Bead{
		ID: "gd-mut-list", Title: "T0", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
	}
	t1 := "T1"
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-mut-list", Title: &t1}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	t2 := "T2"
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-mut-list", Title: &t2}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	got, err := ms.ListMutations(context.Background(), "gd-mut-list")
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("expected at least 2 rows, got %d", len(got))
	}
	// First update should record T0->T1, second T1->T2.
	titleRows := []*model.MutationRow{}
	for _, r := range got {
		if r.Field == "title" {
			titleRows = append(titleRows, r)
		}
	}
	if len(titleRows) != 2 {
		t.Fatalf("expected 2 title rows, got %d", len(titleRows))
	}
	if titleRows[0].OldValue != "T0" || titleRows[0].NewValue != "T1" {
		t.Fatalf("row[0]: expected T0->T1, got %q->%q", titleRows[0].OldValue, titleRows[0].NewValue)
	}
	if titleRows[1].OldValue != "T1" || titleRows[1].NewValue != "T2" {
		t.Fatalf("row[1]: expected T1->T2, got %q->%q", titleRows[1].OldValue, titleRows[1].NewValue)
	}
}

// TestMutationHistory_DependencyAdd is the Proposal C acceptance test
// (ka-0000007756): adding a dependency edge records a mutation_history row so
// `kd audit bead` can answer "why did this edge change" post-hoc — previously
// dep add/remove was only visible in the live event stream.
func TestMutationHistory_DependencyAdd(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "matt@acme.example")
	seedBead(ms, "gd-child01", model.TypeTask, model.StatusOpen, nil, nil)
	if _, err := srv.AddDependency(ctx, &beadsv1.AddDependencyRequest{
		BeadId: "gd-child01", DependsOnId: "gd-epicAAA1", Type: "parent-child",
	}); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	row := findMutation(ms, "gd-child01", model.MutationTypeCreate, "dep.parent-child")
	if row == nil {
		t.Fatalf("expected dep.parent-child create row, got %+v", ms.mutations)
	}
	if row.NewValue != "gd-child01 -> gd-epicAAA1" {
		t.Errorf("dep create NewValue=%q; want %q", row.NewValue, "gd-child01 -> gd-epicAAA1")
	}
	if row.OldValue != "" {
		t.Errorf("dep create OldValue=%q; want empty", row.OldValue)
	}
}

// TestMutationHistory_DependencyRemove pins the remove-edge audit row.
func TestMutationHistory_DependencyRemove(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "matt@acme.example")
	seedBead(ms, "gd-child01", model.TypeTask, model.StatusOpen, nil, nil)
	// Seed the edge — an audit row is only correct for a delete that matched
	// (ka-0000001966); a 0-match must leave no history behind.
	ms.deps["gd-child01"] = []*model.Dependency{
		{BeadID: "gd-child01", DependsOnID: "gd-epicAAA1", Type: model.DepParentChild},
	}
	if _, err := srv.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{
		BeadId: "gd-child01", DependsOnId: "gd-epicAAA1", Type: "parent-child",
	}); err != nil {
		t.Fatalf("RemoveDependency: %v", err)
	}
	row := findMutation(ms, "gd-child01", model.MutationTypeDelete, "dep.parent-child")
	if row == nil {
		t.Fatalf("expected dep.parent-child delete row, got %+v", ms.mutations)
	}
	if row.OldValue != "gd-child01 -> gd-epicAAA1" {
		t.Errorf("dep delete OldValue=%q; want %q", row.OldValue, "gd-child01 -> gd-epicAAA1")
	}
	if row.NewValue != "" {
		t.Errorf("dep delete NewValue=%q; want empty", row.NewValue)
	}
}

// TestMutationHistory_DependencyAdd_DoesNotFlipDibs guards the key safety
// property of Proposal C: a dependency edge change is NOT an ownership signal,
// so recording it must never flip beads.operated_by (which would silently
// re-attribute a bead every time an edge was added to it).
//
// CRITICAL: the bead's current owner is a DIFFERENT HUMAN (oidc:bob@acme.example)
// than the actor (matt@acme.example). This is a human-to-human setup where the
// standard dibs rule WOULD flip (proven by
// TestMutationHistory_DibsFlip_AllowsHumanToHumanFlip). Using a protected
// automation owner (fleet:*/system:*) here would make the test vacuous —
// maybeFlipDibs skips protected owners regardless of which record path is used,
// so the test would pass even if recordDependencyMutation wrongly called the
// dibs-flipping recordMutation. The human owner is what makes this a real guard.
func TestMutationHistory_DependencyAdd_DoesNotFlipDibs(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "matt@acme.example")
	ms.beads["gd-child01"] = &model.Bead{
		ID: "gd-child01", Title: "Child", Kind: model.KindIssue, Type: model.TypeTask,
		Status: model.StatusOpen, OperatedBy: "oidc:bob@acme.example",
	}
	if _, err := srv.AddDependency(ctx, &beadsv1.AddDependencyRequest{
		BeadId: "gd-child01", DependsOnId: "gd-epicAAA1", Type: "parent-child",
	}); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	// Owner must be UNCHANGED — a dep add is not an ownership signal.
	if b := ms.beads["gd-child01"]; b.OperatedBy != "oidc:bob@acme.example" {
		t.Errorf("dep add flipped operator: got %q, want oidc:bob@acme.example", b.OperatedBy)
	}
	// And no synthetic operated_by mutation row from a dibs flip.
	if row := findMutation(ms, "gd-child01", model.MutationTypeUpdate, "operated_by"); row != nil {
		t.Errorf("dep add emitted a synthetic operated_by row; edges must not flip dibs: %+v", row)
	}
	// Sanity: the dep audit row itself IS recorded (the feature still works).
	if row := findMutation(ms, "gd-child01", model.MutationTypeCreate, "dep.parent-child"); row == nil {
		t.Errorf("expected dep.parent-child audit row to be recorded, got %+v", ms.mutations)
	}
}
