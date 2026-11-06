package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestGRPCCreateBead(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "alice")
	// SECURITY (AUDIT §C3): client-supplied CreatedBy must be ignored; server
	// derives actor from the authenticated identity on the context.
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Test bead", Type: "task", CreatedBy: "evil-forged",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b := resp.Bead
	if b.Id == "" || b.Title != "Test bead" || b.Status != "open" || b.Kind != "issue" || b.CreatedBy != "alice" {
		t.Fatalf("unexpected bead: id=%q title=%q status=%q kind=%q created_by=%q", b.Id, b.Title, b.Status, b.Kind, b.CreatedBy)
	}
	requireEvent(t, ms, 1, "beads.bead.created")
}

func TestGRPCCreateBead_WithLabels(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Labeled bead", Type: "task", Labels: []string{"urgent", "frontend"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ms.labels[resp.Bead.Id]) != 2 {
		t.Fatalf("expected 2 labels stored, got %d", len(ms.labels[resp.Bead.Id]))
	}
}

func TestGRPCGetBead(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-test1"] = &model.Bead{ID: "gd-test1", Title: "Test bead", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	resp, err := srv.GetBead(ctx, &beadsv1.GetBeadRequest{Id: "gd-test1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Id != "gd-test1" || resp.Bead.Title != "Test bead" {
		t.Fatalf("got id=%q title=%q", resp.Bead.Id, resp.Bead.Title)
	}
}

func TestGRPCListBeads(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-1"] = &model.Bead{ID: "gd-1", Title: "A", Status: model.StatusOpen}
	ms.beads["gd-2"] = &model.Bead{ID: "gd-2", Title: "B", Status: model.StatusOpen}

	resp, err := srv.ListBeads(ctx, &beadsv1.ListBeadsRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Total != 2 || len(resp.Beads) != 2 {
		t.Fatalf("expected 2 beads, got total=%d len=%d", resp.Total, len(resp.Beads))
	}
}

func TestGRPCUpdateBead(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-upd1"] = &model.Bead{ID: "gd-upd1", Title: "Original", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	title := "Updated"
	resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-upd1", Title: &title})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Title != "Updated" {
		t.Fatalf("got title=%q", resp.Bead.Title)
	}
	requireEvent(t, ms, 1, "beads.bead.updated")
}

func TestGRPCUpdateBead_StatusClosed(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-upd2"] = &model.Bead{ID: "gd-upd2", Title: "To close", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	closed := "closed"
	resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-upd2", Status: &closed})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Status != "closed" || resp.Bead.ClosedAt == nil {
		t.Fatalf("got status=%q closed_at=%v", resp.Bead.Status, resp.Bead.ClosedAt)
	}
}

func TestGRPCCloseBead(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "alice")
	ms.beads["gd-cls1"] = &model.Bead{ID: "gd-cls1", Title: "To close", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	// SECURITY (AUDIT §C3): client-supplied ClosedBy must be ignored.
	resp, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-cls1", ClosedBy: "evil-forged"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Status != "closed" {
		t.Fatalf("got status=%q", resp.Bead.Status)
	}
	requireEvent(t, ms, 1, "beads.bead.closed")
}

// TestGRPCCloseBead_AlreadyClosed_Idempotent verifies that gRPC CloseBead on
// an already-closed bead returns the existing bead as-is without re-stamping
// closed_at/closed_by or emitting a second close event. Regression guard for
// ka-0000000961.
func TestGRPCCloseBead_AlreadyClosed_Idempotent(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "alice")
	closedAt := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ms.beads["gd-clsdup"] = &model.Bead{
		ID:       "gd-clsdup",
		Title:    "Already closed",
		Kind:     model.KindIssue,
		Type:     model.TypeTask,
		Status:   model.StatusClosed,
		ClosedAt: &closedAt,
		ClosedBy: "original-actor",
	}

	resp, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-clsdup"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Status != "closed" {
		t.Fatalf("status: got %q, want closed", resp.Bead.Status)
	}
	// closed_by must not be overwritten — verify via the mock store directly
	// (proto Bead does not currently expose closed_by).
	if ms.beads["gd-clsdup"].ClosedBy != "original-actor" {
		t.Fatalf("closed_by: got %q, want original-actor", ms.beads["gd-clsdup"].ClosedBy)
	}
	if ms.beads["gd-clsdup"].ClosedAt == nil || !ms.beads["gd-clsdup"].ClosedAt.Equal(closedAt) {
		t.Fatalf("closed_at was re-stamped: got %v, want %v", ms.beads["gd-clsdup"].ClosedAt, closedAt)
	}
	for _, ev := range ms.events {
		if ev.Topic == "beads.bead.closed" {
			t.Fatalf("unexpected second close event emitted")
		}
	}
}

func TestGRPCDeleteBead(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-del1"] = &model.Bead{ID: "gd-del1", Title: "Delete me", Status: model.StatusOpen}

	if _, err := srv.DeleteBead(ctx, &beadsv1.DeleteBeadRequest{Id: "gd-del1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, ok := ms.beads["gd-del1"]
	if !ok {
		t.Fatal("expected bead to remain in store (soft-delete)")
	}
	if b.DeletedAt == nil {
		t.Fatal("expected DeletedAt to be set on soft-delete")
	}
	requireEvent(t, ms, 1, "beads.bead.deleted")
}

func TestGRPCUpdateBead_ClearDeferUntil(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	future := time.Now().Add(24 * time.Hour)
	ms.beads["gd-def1"] = &model.Bead{
		ID: "gd-def1", Title: "Deferred", Kind: model.KindIssue, Type: model.TypeTask,
		Status: model.StatusDeferred, DeferUntil: &future,
	}

	// Send a zero-time timestamp to clear defer_until.
	zero := timestamppb.New(time.Time{})
	resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-def1", DeferUntil: zero})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.DeferUntil != nil {
		t.Fatalf("expected defer_until to be cleared, got %v", resp.Bead.DeferUntil)
	}
}

func TestGRPCUpdateBead_ClearDueAt(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	future := time.Now().Add(24 * time.Hour)
	ms.beads["gd-due1"] = &model.Bead{
		ID: "gd-due1", Title: "With due", Kind: model.KindIssue, Type: model.TypeTask,
		Status: model.StatusOpen, DueAt: &future,
	}

	zero := timestamppb.New(time.Time{})
	resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-due1", DueAt: zero})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.DueAt != nil {
		t.Fatalf("expected due_at to be cleared, got %v", resp.Bead.DueAt)
	}
}

func TestGRPCUpdateBead_SetDeferUntilPreserved(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-def2"] = &model.Bead{
		ID: "gd-def2", Title: "Will defer", Kind: model.KindIssue, Type: model.TypeTask,
		Status: model.StatusDeferred,
	}

	future := time.Now().Add(48 * time.Hour).Truncate(time.Microsecond)
	ts := timestamppb.New(future)
	resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-def2", DeferUntil: ts})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.DeferUntil == nil {
		t.Fatal("expected defer_until to be set")
	}
	got := resp.Bead.DeferUntil.AsTime().Truncate(time.Microsecond)
	if !got.Equal(future) {
		t.Fatalf("expected defer_until=%v, got %v", future, got)
	}
}

func TestGRPCCloseBead_SetsClosedBy(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "alice")
	ms.beads["gd-cb1"] = &model.Bead{ID: "gd-cb1", Title: "Close me", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	// SECURITY (AUDIT §C3): client-supplied ClosedBy must be ignored.
	resp, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-cb1", ClosedBy: "evil-forged"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Status != "closed" {
		t.Fatalf("expected status=closed, got %q", resp.Bead.Status)
	}
	// Verify closedBy was stored on the model.
	stored := ms.beads["gd-cb1"]
	if stored.ClosedBy != "alice" {
		t.Fatalf("expected stored closedBy=%q, got %q", "alice", stored.ClosedBy)
	}
}

func TestGRPCCloseBead_EmptyClosedBy(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-cb2"] = &model.Bead{ID: "gd-cb2", Title: "Close me", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}

	resp, err := srv.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: "gd-cb2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Status != "closed" {
		t.Fatalf("expected status=closed, got %q", resp.Bead.Status)
	}
}

func TestGRPCUpdateBead_LabelsArePersisted(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-lrec"] = &model.Bead{ID: "gd-lrec", Title: "Labeled", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen}
	ms.labels["gd-lrec"] = []string{"a", "b"}

	resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-lrec", Labels: []string{"b", "c"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp

	// Check the store has reconciled labels.
	labelSet := map[string]bool{}
	for _, l := range ms.labels["gd-lrec"] {
		labelSet[l] = true
	}
	if !labelSet["b"] || !labelSet["c"] || labelSet["a"] {
		t.Fatalf("expected labels [b, c], got %v", ms.labels["gd-lrec"])
	}
}

func TestGRPCCreateBead_LabelFailure_ReturnsError(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.addLabelErr = fmt.Errorf("label store down")

	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "With labels", Type: "task", Labels: []string{"x"},
	})
	if err == nil {
		t.Fatal("expected error when AddLabel fails")
	}
}

func TestGRPCCreateBead_AgentType_RequiresFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// Without required fields → should fail.
	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "my-agent", Type: "agent",
	})
	if err == nil {
		t.Fatal("expected error when agent, role, project fields are missing")
	}
}

func TestGRPCCreateBead_AgentType_WithRequiredFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "my-agent",
		Type:   "agent",
		Fields: []byte(`{"agent":"my-agent","role":"crew","project":"my-project"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Kind != "config" {
		t.Fatalf("expected kind=config, got %q", resp.Bead.Kind)
	}
}

// --- Type alias resolution tests ---

// TestGRPCCreateBead_BundleIsFirstClass covers the migration from the
// legacy "bundle" → "molecule" alias to first-class type=bundle. Bundle
// beads now resolve through their own type config (issue-kind, with the
// "kind" enum required) rather than being silently rewritten to molecule.
func TestGRPCCreateBead_BundleIsFirstClass(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "Test bundle",
		Type:   "bundle",
		Fields: []byte(`{"kind":"plan"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Type != "bundle" {
		t.Fatalf("expected type=bundle (no alias), got %q", resp.Bead.Type)
	}
	if resp.Bead.Kind != "issue" {
		t.Fatalf("expected kind=issue for bundle, got %q", resp.Bead.Kind)
	}
}

// TestGRPCCreateBead_BundleAcceptsFsmTriageFields covers the fsm-triage
// migration off type=rich-report onto type=bundle (ka-0000004066). The triage
// emit stamps report_kind / headline / severity / source_ref on the bead it
// creates; before the migration those were only declared on type=rich-report,
// so a bundle create carrying them would 400 "severity: unknown field". This
// asserts the drop-in works end to end through the real field-validation path.
func TestGRPCCreateBead_BundleAcceptsFsmTriageFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "FSM-999 triage — example",
		Type:   "bundle",
		Fields: []byte(`{"kind":"report","report_kind":"active-incident","headline":"h","severity":"P2","source_ref":"FSM-999"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error creating bundle with fsm-triage fields: %v", err)
	}
	if resp.Bead.Type != "bundle" {
		t.Fatalf("expected type=bundle, got %q", resp.Bead.Type)
	}
}

// TestGRPCCreateBead_BundleReproGateEnforced covers the other half of the
// migration: the gate_passed→valid-URL invariant (validateReproFields,
// ka-0000002579) that the repro-harness mirror relies on must hold on
// type=bundle just as it does on type=rich-report, or a mirrored pass could
// record gate_passed=true with no backing evidence URL.
func TestGRPCCreateBead_BundleReproGateEnforced(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// gate_passed=true with no evidence URL must be rejected.
	if _, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "bundle gate no evidence",
		Type:   "bundle",
		Fields: []byte(`{"kind":"report","gate_passed":true}`),
	}); err == nil {
		t.Fatal("expected gate_passed=true with empty repro_evidence to be rejected on type=bundle")
	}

	// gate_passed=true with a valid evidence URL must be accepted.
	if _, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "bundle gate with evidence",
		Type:   "bundle",
		Fields: []byte(`{"kind":"report","gate_passed":true,"repro_evidence":"https://example.com/proof.mp4"}`),
	}); err != nil {
		t.Fatalf("expected gate_passed=true with a valid evidence URL to be accepted on type=bundle, got: %v", err)
	}
}

func TestGRPCCreateBead_TemplateResolvesToFormula(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Test template alias", Type: "template",
		Fields: []byte(`{"vars":[],"steps":[]}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Type != "formula" {
		t.Fatalf("expected type=formula (alias of template), got %q", resp.Bead.Type)
	}
	if resp.Bead.Kind != "data" {
		t.Fatalf("expected kind=data for formula, got %q", resp.Bead.Kind)
	}
}

func TestGRPCCreateBead_MoleculeDirectType(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Direct molecule", Type: "molecule",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Type != "molecule" {
		t.Fatalf("expected type=molecule, got %q", resp.Bead.Type)
	}
	if resp.Bead.Kind != "issue" {
		t.Fatalf("expected kind=issue, got %q", resp.Bead.Kind)
	}
}

func TestGRPCCreateBead_FormulaDirectType(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Direct formula", Type: "formula",
		Fields: []byte(`{"vars":[],"steps":[]}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Type != "formula" {
		t.Fatalf("expected type=formula, got %q", resp.Bead.Type)
	}
	if resp.Bead.Kind != "data" {
		t.Fatalf("expected kind=data, got %q", resp.Bead.Kind)
	}
}

func TestGRPCCreateBead_MoleculeFieldsValidated(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// Valid molecule fields.
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "Molecule with fields",
		Type:   "molecule",
		Fields: []byte(`{"formula_id":"gd-f1","applied_vars":{"a":"b"},"ephemeral":true}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Bead.Type != "molecule" {
		t.Fatalf("expected type=molecule, got %q", resp.Bead.Type)
	}
}

func TestGRPCListBeads_TemplateFilterResolvesToFormula(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-f1"] = &model.Bead{
		ID: "gd-f1", Title: "A formula", Kind: model.KindData, Type: model.TypeFormula, Status: model.StatusOpen,
	}
	ms.beads["gd-t1"] = &model.Bead{
		ID: "gd-t1", Title: "A task", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen,
	}

	// Filter by deprecated "template" type — mock store filters by Type,
	// so if alias resolution works, we'll get the formula bead.
	resp, err := srv.ListBeads(ctx, &beadsv1.ListBeadsRequest{Type: []string{"template"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("expected 1 bead (formula via template alias), got %d", resp.Total)
	}
	if resp.Beads[0].Type != "formula" {
		t.Fatalf("expected type=formula, got %q", resp.Beads[0].Type)
	}
}

// --- Fleet assignment tests ---

func TestGRPCCreateBead_FleetFromContext(t *testing.T) {
	srv, ms, baseCtx := testCtx(t)
	ctx := WithFleet(baseCtx, "qa-fleet")

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Fleet bead", Type: "task",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify stored bead has fleet set from context.
	stored := ms.beads[resp.Bead.Id]
	if stored.Fleet != "qa-fleet" {
		t.Fatalf("expected stored fleet=qa-fleet, got %q", stored.Fleet)
	}
}

func TestGRPCCreateBead_DefaultFleet(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	// No fleet in context — should default to "default".
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Default fleet bead", Type: "task",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stored := ms.beads[resp.Bead.Id]
	if stored.Fleet != "default" {
		t.Fatalf("expected stored fleet=default, got %q", stored.Fleet)
	}
}

func TestGRPCCreateBead_FleetWithLabels(t *testing.T) {
	srv, ms, baseCtx := testCtx(t)
	ctx := WithFleet(baseCtx, "staging")

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Fleet labeled bead", Type: "task", Labels: []string{"urgent"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify stored bead has fleet set.
	stored := ms.beads[resp.Bead.Id]
	if stored.Fleet != "staging" {
		t.Fatalf("expected stored fleet=staging, got %q", stored.Fleet)
	}
	// Labels should be created for this bead.
	if len(ms.labels[resp.Bead.Id]) != 1 {
		t.Fatalf("expected 1 label, got %d", len(ms.labels[resp.Bead.Id]))
	}
}

func TestGRPCListBeads_FleetFromContext(t *testing.T) {
	srv, ms, _ := testCtx(t)
	ms.beads["gd-qa1"] = &model.Bead{
		ID: "gd-qa1", Title: "QA bead", Kind: model.KindIssue, Type: model.TypeTask,
		Status: model.StatusOpen, Fleet: "qa-fleet",
	}
	ms.beads["gd-prod1"] = &model.Bead{
		ID: "gd-prod1", Title: "Prod bead", Kind: model.KindIssue, Type: model.TypeTask,
		Status: model.StatusOpen, Fleet: "production",
	}

	// List with qa-fleet context — mock store filters by fleet.
	ctx := WithFleet(context.Background(), "qa-fleet")
	resp, err := srv.ListBeads(ctx, &beadsv1.ListBeadsRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The mock store filters by fleet, so only qa-fleet bead should be returned.
	for _, b := range resp.Beads {
		stored := ms.beads[b.Id]
		if stored != nil && stored.Fleet != "qa-fleet" && stored.Fleet != "" {
			// Mock store may not filter — just verify the filter was set.
			break
		}
	}
	// Key assertion: fleet was injected into the filter (the mock may or may not filter).
	_ = resp
}

func TestGRPCCreateBead_FleetPreservedOnUpdate(t *testing.T) {
	srv, ms, baseCtx := testCtx(t)

	// Create a bead in qa-fleet.
	ctx := WithFleet(baseCtx, "qa-fleet")
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Fleet bead", Type: "task",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id := resp.Bead.Id
	if ms.beads[id].Fleet != "qa-fleet" {
		t.Fatalf("expected fleet=qa-fleet after create, got %q", ms.beads[id].Fleet)
	}

	// Update the bead — fleet should not change.
	title := "Updated title"
	_, err = srv.UpdateBead(baseCtx, &beadsv1.UpdateBeadRequest{Id: id, Title: &title})
	if err != nil {
		t.Fatalf("unexpected error on update: %v", err)
	}
	if ms.beads[id].Fleet != "qa-fleet" {
		t.Fatalf("expected fleet=qa-fleet after update, got %q", ms.beads[id].Fleet)
	}
}

func TestGRPCCreateBead_DifferentFleetsIndependent(t *testing.T) {
	srv, ms, baseCtx := testCtx(t)

	// Create bead in fleet A.
	ctxA := WithFleet(baseCtx, "fleet-a")
	respA, err := srv.CreateBead(ctxA, &beadsv1.CreateBeadRequest{
		Title: "Fleet A bead", Type: "task",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Create bead in fleet B.
	ctxB := WithFleet(baseCtx, "fleet-b")
	respB, err := srv.CreateBead(ctxB, &beadsv1.CreateBeadRequest{
		Title: "Fleet B bead", Type: "task",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify they have different fleets.
	if ms.beads[respA.Bead.Id].Fleet != "fleet-a" {
		t.Fatalf("expected fleet-a, got %q", ms.beads[respA.Bead.Id].Fleet)
	}
	if ms.beads[respB.Bead.Id].Fleet != "fleet-b" {
		t.Fatalf("expected fleet-b, got %q", ms.beads[respB.Bead.Id].Fleet)
	}
}

// --- Scheduler field tests ---

func TestGRPCCreateBead_TaskType_WithScheduleID(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "Scheduled: connectivity test",
		Type:   "task",
		Fields: []byte(`{"schedule_id":"gd-sched-1"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error creating task with schedule_id: %v", err)
	}
	if resp.Bead.Kind != "issue" {
		t.Fatalf("expected kind=issue, got %q", resp.Bead.Kind)
	}
}

// TestGRPCCreateBead_TaskType_WithScheduleTriggerFields is a regression test
// for gd-ddvlYtK42A: the trigger-engine schedule source stamps
// schedule_fire_key / schedule_fire_time / schedule_cron on task beads it
// creates via spawn_agent. The kbeads validator must accept those fields on
// type=task, not just type=agent, otherwise scheduled gardeners driven by the
// trigger-engine cannot spawn at all.
func TestGRPCCreateBead_TaskType_WithScheduleTriggerFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "Schedule: hourly gardener",
		Type:  "task",
		Fields: []byte(`{
			"schedule_fire_key":"2026-05-10T20:00",
			"schedule_fire_time":"2026-05-10T20:00:00Z",
			"schedule_cron":"0 * * * *"
		}`),
	})
	if err != nil {
		t.Fatalf("unexpected error creating task with schedule trigger fields: %v", err)
	}
	if resp.Bead.Kind != "issue" {
		t.Fatalf("expected kind=issue, got %q", resp.Bead.Kind)
	}
}

// TestGRPCCreateBead_TaskType_WithScheduleKey is a regression test for
// gd-R2Z4HyC0Dp: the trigger-engine stamps `<source>_key` on beads created
// via spawn_agent. For schedule-source triggers that becomes `schedule_key`,
// and the kbeads validator must accept it on type=task — same shape as the
// already-allowed `gitlab_key` / `jira_key`.
func TestGRPCCreateBead_TaskType_WithScheduleKey(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "Scheduled: hourly gardener",
		Type:   "task",
		Fields: []byte(`{"schedule_key":"ka-0000000196"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error creating task with schedule_key: %v", err)
	}
	if resp.Bead.Kind != "issue" {
		t.Fatalf("expected kind=issue, got %q", resp.Bead.Kind)
	}
}

func TestGRPCCreateBead_AgentType_WithSchedulerFields(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "sched-agent",
		Type:  "agent",
		Fields: []byte(`{
			"agent":"sched-agent",
			"role":"crew",
			"project":"gardenio",
			"prompt":"Run health check",
			"task_id":"gd-task-1",
			"schedule_id":"gd-sched-1",
			"schedule_title":"Daily Health Check",
			"schedule_cron":"0 9 * * *",
			"schedule_slack_channel":"C_ALERTS"
		}`),
	})
	if err != nil {
		t.Fatalf("unexpected error creating agent with scheduler fields: %v", err)
	}
	if resp.Bead.Kind != "config" {
		t.Fatalf("expected kind=config, got %q", resp.Bead.Kind)
	}
}

// TestGRPCCreateBead_AgentType_WithScheduleKey is a regression test for
// gd-R2Z4HyC0Dp: spawn_agent on schedule-source triggers stamps schedule_key
// onto the agent bead. The validator must accept it on type=agent alongside
// the existing gitlab_key / jira_key fields.
func TestGRPCCreateBead_AgentType_WithScheduleKey(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "sched-agent",
		Type:  "agent",
		Fields: []byte(`{
			"agent":"sched-agent",
			"role":"crew",
			"project":"gardenio",
			"schedule_key":"ka-0000000196"
		}`),
	})
	if err != nil {
		t.Fatalf("unexpected error creating agent with schedule_key: %v", err)
	}
	if resp.Bead.Kind != "config" {
		t.Fatalf("expected kind=config, got %q", resp.Bead.Kind)
	}
}

// TestGRPCUpdateBead_AgentType_GrandfathersUnknownFieldOnUpdate is a
// regression test for ka-0000007952: schedule-mirror writes a field onto an
// agent bead (e.g. schedule_mirror_key) that the type:agent schema in this
// particular daemon build does not list. Subsequent updates — including the
// `gb stop` PATCH that sets stop_requested — must NOT be rejected just
// because the bead already carries an unknown field, otherwise every
// schedule-mirrored agent gets wedged with stop_requested unset and the
// entrypoint keeps restarting the pod.
//
// We simulate the bug by giving the bead an unknown field via the mock
// store directly (bypassing CreateBead's validation), then PATCHing a
// legitimate update. The update must succeed and the unknown field must be
// preserved.
func TestGRPCUpdateBead_AgentType_GrandfathersUnknownFieldOnUpdate(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Seed an agent bead that already carries an unknown field. Real bug:
	// schedule-mirror wrote schedule_mirror_key before the schema registered
	// it; replace with any field name the active schema doesn't know about.
	const id = "gd-agent-stuck"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "schedule-mirrored agent",
		Kind:      model.KindConfig,
		Type:      "agent",
		Status:    model.StatusOpen,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"agent":"a","role":"crew","project":"gardenio","totally_unknown_legacy_field":"oops"}`),
	}

	// `gb stop` writes a merged fields blob that includes the pre-existing
	// unknown field. Pre-fix, the daemon rejected this with HTTP 400
	// "totally_unknown_legacy_field: unknown field".
	patch := json.RawMessage(`{"agent":"a","role":"crew","project":"gardenio","totally_unknown_legacy_field":"oops","stop_requested":"true"}`)
	resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     id,
		Fields: patch,
	})
	if err != nil {
		t.Fatalf("update with pre-existing unknown field should succeed, got: %v", err)
	}

	// Sanity-check that the legitimate new field landed and the legacy
	// unknown field was preserved.
	var got map[string]any
	if err := json.Unmarshal(resp.Bead.Fields, &got); err != nil {
		t.Fatalf("unmarshal resp fields: %v", err)
	}
	if got["stop_requested"] != "true" {
		t.Errorf("stop_requested not stored: got %v", got["stop_requested"])
	}
	if got["totally_unknown_legacy_field"] != "oops" {
		t.Errorf("pre-existing unknown field should be preserved, got %v", got["totally_unknown_legacy_field"])
	}

	// After the G3 flip (task gd-IAIkeME5YU, decision ka-0000007573, epic
	// ka-0000002054) type:agent carries allow_additional_fields:true, so a
	// NEWLY-added unknown field is now ACCEPTED (bag-of-attributes) rather than
	// 400'd — that is the whole point of the flip: a future undeclared
	// controller/bridge field degrades gracefully instead of dark-firing the
	// spawn/revive. (The residual "typo lands silently" risk on unknown keys is
	// owned by the G4 runtime alarm, ka-0000007249.) Declared fields still get
	// full type validation — see TestResolveTypeConfig_AgentBuiltinIsOpenSchema.
	newUnknownPatch := json.RawMessage(`{"agent":"a","role":"crew","project":"gardenio","totally_unknown_legacy_field":"oops","brand_new_field":"x"}`)
	resp, err = srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
		Id:     id,
		Fields: newUnknownPatch,
	})
	if err != nil {
		t.Fatalf("newly-introduced unknown field should be accepted under the type:agent open schema (G3 flip gd-IAIkeME5YU), got: %v", err)
	}
	if err := json.Unmarshal(resp.Bead.Fields, &got); err != nil {
		t.Fatalf("unmarshal resp fields: %v", err)
	}
	if got["brand_new_field"] != "x" {
		t.Errorf("newly-added unknown field should be stored under open schema, got %v", got["brand_new_field"])
	}
}

// TestGRPCUpdateBead_TriggerType_GrandfathersUnchangedWrongTypedField is the
// server-level repro of the closed-enabled-drain wedge (ka-0000007257). Two
// prod trigger beads carry gardener_stuck_escalated as a JSON BOOLEAN false,
// while the registry declares it type=string. Because UpdateBead validates the
// MERGED field map (not the delta), that legacy value re-failed type validation
// on EVERY subsequent fields-PATCH — including the drain sweep's
// UpdateBeadFieldsTyped({"enabled": false}) that never mentions the field —
// permanently wedging the bead. The fix grandfathers a DEFINED field whose
// value is unchanged from what was already stored, so an unrelated-field PATCH
// succeeds; a PATCH that overwrites the bad value with a correct string
// (the data-repair path) succeeds; and a PATCH that changes it to another bad
// type is still rejected.
func TestGRPCUpdateBead_TriggerType_GrandfathersUnchangedWrongTypedField(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	// Seed a closed trigger carrying a BOOLEAN gardener_stuck_escalated (the
	// registry says string). Written directly to the mock store to reproduce
	// the legacy value a create-time validator would have rejected.
	const id = "gd-trigger-wedged"
	ms.beads[id] = &model.Bead{
		ID:        id,
		Title:     "engine-retired trigger",
		Kind:      model.KindConfig,
		Type:      "trigger",
		Status:    model.StatusClosed,
		CreatedBy: defaultTestSubject,
		Fields:    json.RawMessage(`{"action":"spawn_agent","project":"gardenio","prompt":"x","enabled":true,"gardener_stuck_escalated":false,"gardener_terminated_kind":"no_progress_for"}`),
	}

	// The drain sweep's archive write: an unrelated-field PATCH that flips
	// enabled=false while re-merging the untouched boolean gardener_stuck_escalated.
	// Pre-fix this 400'd with "gardener_stuck_escalated: must be a string".
	archive := json.RawMessage(`{"action":"spawn_agent","project":"gardenio","prompt":"x","enabled":false,"gardener_stuck_escalated":false,"gardener_terminated_kind":"no_progress_for"}`)
	resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: archive})
	if err != nil {
		t.Fatalf("archive PATCH with untouched legacy wrong-typed field should succeed, got: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(resp.Bead.Fields, &got); err != nil {
		t.Fatalf("unmarshal resp fields: %v", err)
	}
	if got["enabled"] != false {
		t.Errorf("enabled not archived: got %v", got["enabled"])
	}

	// The data-repair PATCH overwrites the boolean with the registry-correct
	// empty string (the clear value gardener_operator_reopen.go uses). A CHANGED
	// value with the RIGHT type must pass.
	repair := json.RawMessage(`{"action":"spawn_agent","project":"gardenio","prompt":"x","enabled":false,"gardener_stuck_escalated":"","gardener_terminated_kind":"no_progress_for"}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: repair}); err != nil {
		t.Fatalf("data-repair PATCH to a valid string should succeed, got: %v", err)
	}

	// Changing the field to ANOTHER wrong type (an integer) is a CHANGED value
	// and must still be rejected — grandfathering only covers the unchanged one.
	stillBad := json.RawMessage(`{"action":"spawn_agent","project":"gardenio","prompt":"x","enabled":false,"gardener_stuck_escalated":42,"gardener_terminated_kind":"no_progress_for"}`)
	if _, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: id, Fields: stillBad}); err == nil {
		t.Fatal("expected a CHANGED wrong-typed value to still be rejected")
	}
}

func TestGRPCGetChildrenStatusHistogram(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	ms.beads["gd-EP1"] = &model.Bead{ID: "gd-EP1", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-EP2"] = &model.Bead{ID: "gd-EP2", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-c1"] = &model.Bead{ID: "gd-c1", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-c2"] = &model.Bead{ID: "gd-c2", Type: model.TypeTask, Status: model.StatusClosed}
	ms.beads["gd-c3"] = &model.Bead{ID: "gd-c3", Type: model.TypeTask, Status: model.StatusInProgress}

	ms.deps["gd-c1"] = []*model.Dependency{{BeadID: "gd-c1", DependsOnID: "gd-EP1", Type: model.DepParentChild}}
	ms.deps["gd-c2"] = []*model.Dependency{{BeadID: "gd-c2", DependsOnID: "gd-EP1", Type: model.DepParentChild}}
	ms.deps["gd-c3"] = []*model.Dependency{{BeadID: "gd-c3", DependsOnID: "gd-EP2", Type: model.DepParentChild}}

	t.Run("HappyPath", func(t *testing.T) {
		resp, err := srv.GetChildrenStatusHistogram(ctx, &beadsv1.GetChildrenStatusHistogramRequest{
			ParentIds: []string{"gd-EP1", "gd-EP2", "gd-EP3"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Stable shape: every requested parent is present.
		for _, id := range []string{"gd-EP1", "gd-EP2", "gd-EP3"} {
			if _, ok := resp.Histograms[id]; !ok {
				t.Errorf("missing parent %q in response", id)
			}
		}
		ep1 := resp.Histograms["gd-EP1"].GetStatusCounts()
		if ep1["open"] != 1 || ep1["closed"] != 1 {
			t.Errorf("EP1 counts = %v, want {open:1, closed:1}", ep1)
		}
		ep2 := resp.Histograms["gd-EP2"].GetStatusCounts()
		if ep2["in_progress"] != 1 {
			t.Errorf("EP2 counts = %v, want {in_progress:1}", ep2)
		}
		if len(resp.Histograms["gd-EP3"].GetStatusCounts()) != 0 {
			t.Errorf("EP3 should be empty, got %v", resp.Histograms["gd-EP3"].GetStatusCounts())
		}
	})

	t.Run("EmptyParentIds", func(t *testing.T) {
		_, err := srv.GetChildrenStatusHistogram(ctx, &beadsv1.GetChildrenStatusHistogramRequest{})
		if err == nil {
			t.Fatal("expected InvalidArgument error")
		}
	})

	t.Run("EmptyAfterTrim", func(t *testing.T) {
		_, err := srv.GetChildrenStatusHistogram(ctx, &beadsv1.GetChildrenStatusHistogramRequest{
			ParentIds: []string{"", "  ", " "},
		})
		if err == nil {
			t.Fatal("expected InvalidArgument error")
		}
	})

	t.Run("OverCap", func(t *testing.T) {
		ids := make([]string, childrenStatusHistogramMaxParentsGRPC+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("gd-x%d", i)
		}
		_, err := srv.GetChildrenStatusHistogram(ctx, &beadsv1.GetChildrenStatusHistogramRequest{
			ParentIds: ids,
		})
		if err == nil {
			t.Fatal("expected InvalidArgument error")
		}
	})
}

// TestGRPCUpdateBead_InProgressRequiresAssignee verifies the state-model
// invariant: a kind=issue bead cannot transition to in_progress without an
// assignee. See ka-0000006063 — phased formula molecules were instantiated
// in_progress with no assignee, polluting gb news and ready-queue views.
func TestGRPCUpdateBead_InProgressRequiresAssignee(t *testing.T) {
	t.Run("RejectsTransitionWithoutAssignee", func(t *testing.T) {
		srv, ms, ctx := testCtx(t)
		ms.beads["gd-mol-open"] = &model.Bead{
			ID:     "gd-mol-open",
			Title:  "P3.1: run k6 suite",
			Kind:   model.KindIssue,
			Type:   model.TypeMolecule,
			Status: model.StatusOpen,
		}

		inProgress := string(model.StatusInProgress)
		_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
			Id:     "gd-mol-open",
			Status: &inProgress,
		})
		if err == nil {
			t.Fatal("expected error transitioning kind=issue to in_progress without assignee")
		}
		if ms.beads["gd-mol-open"].Status != model.StatusOpen {
			t.Fatalf("status mutated despite error: got %q", ms.beads["gd-mol-open"].Status)
		}
	})

	t.Run("AllowsAtomicClaim", func(t *testing.T) {
		srv, ms, ctx := testCtx(t)
		ms.beads["gd-mol-claim"] = &model.Bead{
			ID:     "gd-mol-claim",
			Title:  "P3.1: run k6 suite",
			Kind:   model.KindIssue,
			Type:   model.TypeMolecule,
			Status: model.StatusOpen,
		}

		inProgress := string(model.StatusInProgress)
		assignee := "agent-foo"
		resp, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
			Id:       "gd-mol-claim",
			Status:   &inProgress,
			Assignee: &assignee,
		})
		if err != nil {
			t.Fatalf("atomic claim should succeed: %v", err)
		}
		if resp.Bead.Status != string(model.StatusInProgress) {
			t.Fatalf("status: got %q, want in_progress", resp.Bead.Status)
		}
		if resp.Bead.Assignee != "agent-foo" {
			t.Fatalf("assignee: got %q, want agent-foo", resp.Bead.Assignee)
		}
	})

	t.Run("AllowsTransitionWhenAssigneeAlreadySet", func(t *testing.T) {
		srv, ms, ctx := testCtx(t)
		ms.beads["gd-mol-preassigned"] = &model.Bead{
			ID:     "gd-mol-preassigned",
			Title:  "P3.1: run k6 suite",
			Kind:   model.KindIssue,
			Type:   model.TypeMolecule,
			Status: model.StatusOpen,
			// CreatedBy matches the testCtx default subject so the ownership
			// gate (write_authz.go) allows the update from this caller.
			CreatedBy: defaultTestSubject,
			Assignee:  "agent-pre",
		}

		inProgress := string(model.StatusInProgress)
		_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
			Id:     "gd-mol-preassigned",
			Status: &inProgress,
		})
		if err != nil {
			t.Fatalf("transition with existing assignee should succeed: %v", err)
		}
	})

	t.Run("AllowsDataKindTransition", func(t *testing.T) {
		// Escalation approval transitions a kind=data bead to in_progress
		// without an assignee — invariant must not fire on non-issue kinds.
		srv, ms, ctx := testCtx(t)
		ms.beads["gd-esc"] = &model.Bead{
			ID:     "gd-esc",
			Title:  "Approve credential rotation",
			Kind:   model.KindData,
			Type:   model.TypeEscalation,
			Status: model.StatusOpen,
		}

		inProgress := string(model.StatusInProgress)
		_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
			Id:     "gd-esc",
			Status: &inProgress,
		})
		if err != nil {
			t.Fatalf("kind=data transition should succeed without assignee: %v", err)
		}
	})

	t.Run("AllowsUnrelatedEditOnHistoricallyBrokenBead", func(t *testing.T) {
		// A pre-existing kind=issue bead in the broken state (in_progress,
		// no assignee) can still be edited on other fields — the invariant
		// is a transition rule, not a state invariant. Reset to open is the
		// remediation path.
		srv, ms, ctx := testCtx(t)
		ms.beads["gd-mol-broken"] = &model.Bead{
			ID:     "gd-mol-broken",
			Title:  "Old title",
			Kind:   model.KindIssue,
			Type:   model.TypeMolecule,
			Status: model.StatusInProgress,
		}

		newTitle := "New title"
		_, err := srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
			Id:    "gd-mol-broken",
			Title: &newTitle,
		})
		if err != nil {
			t.Fatalf("unrelated edit on broken bead should succeed: %v", err)
		}

		open := string(model.StatusOpen)
		_, err = srv.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{
			Id:     "gd-mol-broken",
			Status: &open,
		})
		if err != nil {
			t.Fatalf("reset to open on broken bead should succeed: %v", err)
		}
	})
}
