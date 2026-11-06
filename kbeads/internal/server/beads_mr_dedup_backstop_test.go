package server

import (
	"context"
	"encoding/json"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// beads_mr_dedup_backstop_test.go — the 23505 race backstop in createBead's
// type=merge-request path (ka-0000005686 HALF-2). The per-mr_url mutex is the
// intra-process guarantee; migration 042 makes the fields.mr_url index UNIQUE so
// a multi-replica daemon (or any path that reached CreateBead without the mutex)
// can still lose the insert race and get a 23505. The backstop must then re-run
// the by-URL lookup and return the winner verbatim rather than surface the raw
// constraint error.

const backstopMRURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/909"

// pqUniqueErr is a stand-in for lib/pq's *pq.Error with SQLState()=="23505",
// matching what isUniqueViolation (beads_idempotency.go) inspects.
type pqUniqueErr struct{}

func (pqUniqueErr) Error() string    { return "duplicate key value violates unique constraint" }
func (pqUniqueErr) SQLState() string { return "23505" }

// mrRaceStore wraps a mockStore to simulate the exact race the backstop closes:
// at preCreate-lookup time the winner is NOT yet visible (a concurrent insert
// has not committed), so createBead proceeds to insert; the insert then raises
// 23505 AND the winner becomes visible (the concurrent tx committed), so the
// backstop's re-lookup finds it.
type mrRaceStore struct {
	*mockStore
	winner       *model.Bead
	hideWinner   bool // when true, the 23505 fires but no winner becomes visible
	insertFailed bool
}

// RunInTransaction passes the WRAPPER (not the inner mockStore) as tx so the
// overridden CreateBead below runs inside the transaction.
func (r *mrRaceStore) RunInTransaction(_ context.Context, fn func(tx store.Store) error) error {
	return fn(r)
}

func (r *mrRaceStore) CreateBead(ctx context.Context, bead *model.Bead) error {
	if !r.insertFailed && mrURLFromFields(bead.Fields) == backstopMRURL {
		// The concurrent insert wins the race: it commits (winner becomes
		// visible to the next lookup, unless hideWinner) and our insert hits the
		// unique index.
		r.insertFailed = true
		if !r.hideWinner {
			r.mockStore.beads[r.winner.ID] = r.winner
		}
		return pqUniqueErr{}
	}
	return r.mockStore.CreateBead(ctx, bead)
}

// TestCreateMergeRequest_UniqueViolationBackstopReturnsWinner drives the race:
// preCreate misses, the insert 23505s, and the backstop returns the winner
// verbatim with no error.
func TestCreateMergeRequest_UniqueViolationBackstopReturnsWinner(t *testing.T) {
	ms := newMockStore()
	winner := &model.Bead{
		ID:     "gd-winner",
		Type:   "merge-request",
		Kind:   model.KindIssue,
		Status: model.StatusClosed,
		Title:  "MR !909 (winner)",
		Fields: json.RawMessage(`{"mr_url":"` + backstopMRURL + `","mr_state":"merged"}`),
	}
	rs := &mrRaceStore{mockStore: ms, winner: winner}
	srv := NewBeadsServer(rs, &events.NoopPublisher{})
	srv.userProvisioner = nil
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: defaultTestSubject})

	resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "MR !909 (racer that lost)",
		Type:   "merge-request",
		Labels: []string{"project:gardenio"},
		Fields: []byte(`{"mr_url":"` + backstopMRURL + `","mr_state":"opened"}`),
	})
	if err != nil {
		t.Fatalf("backstop should have swallowed the 23505 and returned the winner, got err: %v", err)
	}
	if resp.Bead.Id != "gd-winner" {
		t.Errorf("backstop returned id=%q, want the winner gd-winner", resp.Bead.Id)
	}
	if !rs.insertFailed {
		t.Errorf("test did not exercise the insert-failure path")
	}
}

// TestCreateMergeRequest_UniqueViolationNoWinnerStillErrors verifies the
// backstop does not mask a genuine failure: if the re-lookup finds nothing
// (a unique violation with no recoverable winner), the original error surfaces.
func TestCreateMergeRequest_UniqueViolationNoWinnerStillErrors(t *testing.T) {
	ms := newMockStore()
	// A winner that is NEVER made visible: CreateBead 23505s but the re-lookup
	// still misses, so the error must propagate rather than be swallowed.
	rs := &mrRaceStore{mockStore: ms, winner: &model.Bead{ID: "gd-invisible"}}
	// Override: fail the insert but do NOT insert the winner into the store.
	rs.hideWinner = true
	srv := NewBeadsServer(rs, &events.NoopPublisher{})
	srv.userProvisioner = nil
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: defaultTestSubject})

	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "MR !909 (no recoverable winner)",
		Type:   "merge-request",
		Labels: []string{"project:gardenio"},
		Fields: []byte(`{"mr_url":"` + backstopMRURL + `","mr_state":"opened"}`),
	})
	if err == nil {
		t.Fatalf("backstop swallowed a unique violation with no recoverable winner; want the error to surface")
	}
}
