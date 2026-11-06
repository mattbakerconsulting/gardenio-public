//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// TestListBeads_FieldsOnly_RealPostgres runs the fields_only projection
// (gd-6PXLuR3juD) on a real Postgres: the sqlmock test pins the SQL text, this
// pins what jsonb_each + jsonb_object_agg return. A row keeps only the asked
// keys, a row carrying none of them reads {} (not NULL, not an error), and
// FieldsOnly wins over OmitFields.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestListBeads_FieldsOnly_RealPostgres
func TestListBeads_FieldsOnly_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	st, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	operatorID := anyOperatorID(t, dsn)
	fleet := "test-fields-only-" + genTestID(t)
	now := time.Now().UTC()

	create := func(title, fields string) string {
		t.Helper()
		id := genTestID(t)
		if err := st.CreateBead(ctx, &model.Bead{
			ID: id, Kind: model.KindIssue, Type: model.TypeTask,
			Title: title, Status: model.StatusOpen, Fields: json.RawMessage(fields),
			CreatedAt: now, UpdatedAt: now, Fleet: fleet, OperatedByID: &operatorID,
		}); err != nil {
			t.Fatalf("create bead %s: %v", title, err)
		}
		return id
	}
	full := create("full", `{"total_cost_usd":"1.50","agent":"a-1","prompt":"a long prompt","nested":{"x":1}}`)
	none := create("none", `{"prompt":"only a prompt"}`)

	beads, _, err := st.ListBeads(ctx, model.BeadFilter{
		Fleet:      fleet,
		FieldsOnly: []string{"total_cost_usd", "agent"},
		OmitFields: []string{"agent"},
	})
	if err != nil {
		t.Fatalf("ListBeads with FieldsOnly: %v", err)
	}
	got := map[string]map[string]any{}
	for _, b := range beads {
		var m map[string]any
		if err := json.Unmarshal(b.Fields, &m); err != nil {
			t.Fatalf("bead %s fields %q: %v", b.ID, b.Fields, err)
		}
		got[b.ID] = m
	}
	if len(got) != 2 {
		t.Fatalf("want both beads listed, got %v", got)
	}
	if f := got[full]; len(f) != 2 || f["total_cost_usd"] != "1.50" || f["agent"] != "a-1" {
		t.Errorf("full bead: fields = %v, want only total_cost_usd and agent", f)
	}
	if f := got[none]; f == nil || len(f) != 0 {
		t.Errorf("bead with none of the keys: fields = %v, want {}", f)
	}
}
