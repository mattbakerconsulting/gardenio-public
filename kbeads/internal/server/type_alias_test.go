package server

import (
	"encoding/json"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

func TestCreateBead_TemplateAlias(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// Creating a bead with deprecated type "template" should resolve to "formula".
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title: "My template", Type: "template", CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b := resp.Bead
	if b.Type != "formula" {
		t.Errorf("type = %q, want 'formula' (alias resolved)", b.Type)
	}
	if b.Kind != "data" {
		t.Errorf("kind = %q, want 'data' (formula kind)", b.Kind)
	}
}

func TestCreateBead_MoleculeType(t *testing.T) {
	srv, _, ctx := testCtx(t)

	// Creating a bead with canonical type "molecule" should work directly.
	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "My molecule",
		Type:   "molecule",
		Fields: []byte(`{"formula_id":"gd-f-1","applied_vars":{"env":"prod"}}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b := resp.Bead
	if b.Type != "molecule" {
		t.Errorf("type = %q, want molecule", b.Type)
	}
	if b.Kind != "issue" {
		t.Errorf("kind = %q, want issue", b.Kind)
	}

	// Verify fields were stored.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if string(fields["formula_id"]) != `"gd-f-1"` {
		t.Errorf("formula_id = %s, want \"gd-f-1\"", fields["formula_id"])
	}
}

func TestCreateBead_FormulaType(t *testing.T) {
	srv, _, ctx := testCtx(t)

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "Deploy workflow",
		Type:   "formula",
		Fields: []byte(`{"vars":[{"name":"env"}],"steps":[{"id":"s1","title":"Build"}]}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b := resp.Bead
	if b.Type != "formula" {
		t.Errorf("type = %q, want formula", b.Type)
	}
	if b.Kind != "data" {
		t.Errorf("kind = %q, want data", b.Kind)
	}
}

func TestListBeads_TemplateAliasFilter(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	ms.beads["gd-f-1"] = &model.Bead{
		ID: "gd-f-1", Title: "Formula 1", Type: "formula", Kind: "data", Status: "open",
	}

	resp, err := srv.ListBeads(ctx, &beadsv1.ListBeadsRequest{
		Type: []string{"template"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, b := range resp.Beads {
		if b.Id == "gd-f-1" {
			found = true
		}
	}
	if !found {
		t.Error("formula bead gd-f-1 should be found when filtering by 'template' alias")
	}
}
