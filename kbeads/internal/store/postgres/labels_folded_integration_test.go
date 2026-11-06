//go:build integration

package postgres

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// TestSingleBeadReads_FoldLabels_RealPostgres pins beadLabelsSelectSuffix
// (gd-cP9tODid8j) against a real Postgres: the sqlmock tests pin the SQL
// shape, this pins what the correlated array_agg actually returns on every
// single-bead path that folds labels in — GetBead, GetBeadIncludingDeleted,
// CloseBead, RestoreBead and TransferBead. Two invariants carried over from
// the separate queryGetLabels round trip it replaces:
//   - a bead with no labels reads back Labels == nil, not an empty slice;
//   - the labels are the same set GetLabels returns, in label order.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestSingleBeadReads_FoldLabels_RealPostgres
func TestSingleBeadReads_FoldLabels_RealPostgres(t *testing.T) {
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
	fleet := "test-labels-fold-" + genTestID(t)

	now := time.Now().UTC()
	id := genTestID(t)
	if err := st.CreateBead(ctx, &model.Bead{
		ID: id, Kind: model.KindIssue, Type: model.TypeTask,
		Title: "labels fold", Status: model.StatusOpen,
		CreatedAt: now, UpdatedAt: now, Fleet: fleet, OperatedByID: &operatorID,
	}); err != nil {
		t.Fatalf("create bead: %v", err)
	}

	got, err := st.GetBead(ctx, id)
	if err != nil {
		t.Fatalf("GetBead (no labels): %v", err)
	}
	if got.Labels != nil {
		t.Fatalf("GetBead with no labels: Labels = %#v, want nil", got.Labels)
	}

	for _, l := range []string{"zeta", "alpha", "project:gardenio"} {
		if err := st.AddLabel(ctx, id, l); err != nil {
			t.Fatalf("AddLabel %q: %v", l, err)
		}
	}
	want := []string{"alpha", "project:gardenio", "zeta"}

	separate, err := st.GetLabels(ctx, id)
	if err != nil {
		t.Fatalf("GetLabels: %v", err)
	}
	if sorted := slices.Sorted(slices.Values(separate)); !slices.Equal(sorted, want) {
		t.Fatalf("GetLabels = %v, want the set %v", separate, want)
	}

	check := func(path string, b *model.Bead) {
		t.Helper()
		if !slices.Equal(b.Labels, want) {
			t.Errorf("%s: Labels = %v, want %v", path, b.Labels, want)
		}
	}

	got, err = st.GetBead(ctx, id)
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	check("GetBead", got)

	got, err = st.CloseBead(ctx, id, "tester")
	if err != nil {
		t.Fatalf("CloseBead: %v", err)
	}
	check("CloseBead", got)

	if err := st.DeleteBead(ctx, id); err != nil {
		t.Fatalf("DeleteBead: %v", err)
	}
	got, err = st.GetBeadIncludingDeleted(ctx, id)
	if err != nil {
		t.Fatalf("GetBeadIncludingDeleted: %v", err)
	}
	check("GetBeadIncludingDeleted", got)

	got, err = st.RestoreBead(ctx, id)
	if err != nil {
		t.Fatalf("RestoreBead: %v", err)
	}
	check("RestoreBead", got)

	got, _, err = st.TransferBead(ctx, id, fleet+"-moved")
	if err != nil {
		t.Fatalf("TransferBead: %v", err)
	}
	check("TransferBead", got)
}
