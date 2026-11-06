package server

import (
	"context"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDualWrite_CreateBead_OIDC_PopulatesBothColumns covers the P2 acceptance
// criterion end-to-end: a CreateBead under an OIDC identity stamps both the
// legacy operated_by TEXT column and the new operated_by_id FK pointer on
// every audit-bearing mutation row (the paired '*' + 'operated_by' create
// rows).
//
// Reads the rows out of the in-memory mutation_history table populated by the
// mockStore — the resolver runs through the same code path as production,
// just against a map-backed store.
func TestDualWrite_CreateBead_OIDC_PopulatesBothColumns(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "matt@acme.example")

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{Title: "DW", Type: "task"})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}

	star := findMutation(ms, resp.Bead.Id, model.MutationTypeCreate, "*")
	if star == nil {
		t.Fatalf("missing field='*' create row")
	}
	if star.OperatedBy != "oidc:matt@acme.example" {
		t.Errorf("star row OperatedBy = %q; want oidc:matt@acme.example", star.OperatedBy)
	}
	if star.OperatedByID == nil || *star.OperatedByID == 0 {
		t.Errorf("star row OperatedByID = %v; want non-nil non-zero", star.OperatedByID)
	}

	opRow := findMutation(ms, resp.Bead.Id, model.MutationTypeCreate, "operated_by")
	if opRow == nil {
		t.Fatalf("missing field='operated_by' create row")
	}
	if opRow.OperatedBy != "oidc:matt@acme.example" {
		t.Errorf("op row OperatedBy = %q; want oidc:matt@acme.example", opRow.OperatedBy)
	}
	if opRow.OperatedByID == nil || *opRow.OperatedByID == 0 {
		t.Errorf("op row OperatedByID = %v; want non-nil non-zero", opRow.OperatedByID)
	}

	// Both rows should carry the SAME operator id — same human, same write
	// burst.
	if star.OperatedByID != nil && opRow.OperatedByID != nil && *star.OperatedByID != *opRow.OperatedByID {
		t.Errorf("paired rows have different OperatedByID: star=%d op=%d", *star.OperatedByID, *opRow.OperatedByID)
	}

	// And the operators table should contain exactly one (kind, subject) entry.
	if got, ok := ms.operatorByPair["oidc\x00matt@acme.example"]; !ok || got == 0 {
		t.Errorf("operators table missing pair (oidc, matt@acme.example); got %d ok=%v", got, ok)
	}
}

// TestCreateBead_Anonymous_FailsClosed pins the R2 server-enforcement
// (ka-0000005772) invariant: an anonymous CreateBead — the resolver
// cannot attribute the request to any operator — is rejected with
// InvalidArgument. No bead row and no mutation_history rows land. This
// replaces the pre-R2 sentinel-fallback tests (history-stays-nil,
// bead-carries-sentinel-FK) that asserted the silent-write behavior we
// just removed.
func TestCreateBead_Anonymous_FailsClosed(t *testing.T) {
	srv, ms, _ := newTestServer()
	ctx := context.Background()

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{Title: "anon", Type: "task"})
	if err == nil {
		t.Fatalf("CreateBead succeeded under anonymous ctx; want InvalidArgument (resp=%v)", resp)
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("CreateBead err code = %v; want InvalidArgument", got)
	}
	if len(ms.beads) != 0 {
		t.Errorf("anonymous CreateBead wrote %d bead(s); want 0", len(ms.beads))
	}
	if len(ms.mutations) != 0 {
		t.Errorf("anonymous CreateBead wrote %d mutation row(s); want 0", len(ms.mutations))
	}
}
