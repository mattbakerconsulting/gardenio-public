package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeAliasLinker is a stub for slackOIDCAliasLinker. It records the bead ids
// the cycle asked the daemon to resync: the cycle has no way to hand the
// daemon an alias list (the interface has no such method), which is the
// property gd-uAvleAZUvV depends on.
type fakeAliasLinker struct {
	mu       sync.Mutex
	pages    [][]*beadsapi.BeadDetail // emitted in order, then returns empty
	pageIdx  int
	listErr  error
	resyncFn func(beadID string) (*beadsapi.LinkOperatorAliasesResponse, error)
	resynced []string
}

func (f *fakeAliasLinker) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.pageIdx >= len(f.pages) {
		return &beadsapi.ListBeadsResult{}, nil
	}
	out := f.pages[f.pageIdx]
	f.pageIdx++
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeAliasLinker) ResyncUserBeadAliases(_ context.Context, beadID string) (*beadsapi.LinkOperatorAliasesResponse, error) {
	f.mu.Lock()
	f.resynced = append(f.resynced, beadID)
	fn := f.resyncFn
	f.mu.Unlock()
	if fn != nil {
		return fn(beadID)
	}
	return &beadsapi.LinkOperatorAliasesResponse{OperatorID: 1, AliasesInserted: 1}, nil
}

func aliasUserBead(id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "user", Kind: "config", Fields: map[string]string{"email": id + "@example.com"}}
}

func runCycle(ctx context.Context, d slackOIDCAliasLinker) {
	b := &Bot{logger: newSilentLogger()}
	b.runSlackOIDCOperatorAliasResyncCycle(ctx, d, b.logger)
}

// The cycle asks the daemon to resync every user bead, by id.
func TestRunSlackOIDCOperatorAliasResyncCycle_ResyncsEveryUserBeadByID(t *testing.T) {
	daemon := &fakeAliasLinker{pages: [][]*beadsapi.BeadDetail{{aliasUserBead("gd-a"), aliasUserBead("gd-b"), aliasUserBead("gd-c")}}}
	runCycle(context.Background(), daemon)
	if got := daemon.resynced; len(got) != 3 || got[0] != "gd-a" || got[1] != "gd-b" || got[2] != "gd-c" {
		t.Fatalf("resynced = %v, want [gd-a gd-b gd-c]", got)
	}
}

// The bridge never composes an alias list: the daemon interface it depends on
// has no method that takes one. A compile-time guard so a future change that
// adds LinkOperatorAliases back to the loop's dependency fails here.
func TestSlackOIDCAliasLinker_HasNoAliasListMethod(t *testing.T) {
	var _ slackOIDCAliasLinker = (*fakeAliasLinker)(nil)
	if _, ok := any((*fakeAliasLinker)(nil)).(interface {
		LinkOperatorAliases(context.Context, []beadsapi.OperatorAliasRef, string) (*beadsapi.LinkOperatorAliasesResponse, error)
	}); ok {
		t.Fatal("fake exposes LinkOperatorAliases; the resync loop must not depend on a caller-supplied alias list")
	}
}

func TestRunSlackOIDCOperatorAliasResyncCycle_IdempotentOnReplay(t *testing.T) {
	beads := []*beadsapi.BeadDetail{aliasUserBead("gd-a")}
	daemon := &fakeAliasLinker{
		pages: [][]*beadsapi.BeadDetail{beads, beads},
		resyncFn: func(string) (*beadsapi.LinkOperatorAliasesResponse, error) {
			return &beadsapi.LinkOperatorAliasesResponse{OperatorID: 42, AlreadyLinked: true}, nil
		},
	}
	runCycle(context.Background(), daemon)
	runCycle(context.Background(), daemon)
	if got := len(daemon.resynced); got != 2 {
		t.Fatalf("resync calls = %d, want 2 (one per cycle)", got)
	}
}

func TestRunSlackOIDCOperatorAliasResyncCycle_ResyncErrorContinues(t *testing.T) {
	daemon := &fakeAliasLinker{
		pages: [][]*beadsapi.BeadDetail{{aliasUserBead("gd-a"), aliasUserBead("gd-b")}},
		resyncFn: func(string) (*beadsapi.LinkOperatorAliasesResponse, error) {
			return nil, errors.New("daemon transient")
		},
	}
	runCycle(context.Background(), daemon)
	if got := len(daemon.resynced); got != 2 {
		t.Errorf("resync calls = %d, want 2 (loop keeps going after per-row errors)", got)
	}
}

func TestRunSlackOIDCOperatorAliasResyncCycle_ListErrorReturnsCleanly(t *testing.T) {
	daemon := &fakeAliasLinker{listErr: errors.New("daemon down")}
	runCycle(context.Background(), daemon)
	if got := len(daemon.resynced); got != 0 {
		t.Errorf("resync calls = %d, want 0 when list fails", got)
	}
}

func TestRunSlackOIDCOperatorAliasResyncCycle_CtxCanceledAborts(t *testing.T) {
	daemon := &fakeAliasLinker{pages: [][]*beadsapi.BeadDetail{{aliasUserBead("gd-a")}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runCycle(ctx, daemon)
	if got := len(daemon.resynced); got != 0 {
		t.Errorf("resync calls = %d, want 0 after cancel", got)
	}
}

// aliasPrunerFake adds the optional revocation call to fakeAliasLinker.
type aliasPrunerFake struct {
	*fakeAliasLinker
	pruneCalls int
	pruneResp  *beadsapi.PruneOperatorAliasesResponse
}

func (f *aliasPrunerFake) PruneUserBeadAliases(context.Context) (*beadsapi.PruneOperatorAliasesResponse, error) {
	f.pruneCalls++
	if f.pruneResp != nil {
		return f.pruneResp, nil
	}
	return &beadsapi.PruneOperatorAliasesResponse{}, nil
}

// A 409 collision from the daemon is counted and logged but never retried as a
// merge; the other beads in the cycle are still resynced and the prune pass
// still runs.
func TestRunSlackOIDCOperatorAliasResyncCycle_CollisionRefusedNoMerge(t *testing.T) {
	inner := &fakeAliasLinker{
		pages: [][]*beadsapi.BeadDetail{{aliasUserBead("gd-mallory"), aliasUserBead("gd-bob")}},
		resyncFn: func(id string) (*beadsapi.LinkOperatorAliasesResponse, error) {
			if id == "gd-mallory" {
				return nil, &beadsapi.APIError{StatusCode: 409, Message: "aliases already resolve to different operators"}
			}
			return &beadsapi.LinkOperatorAliasesResponse{OperatorID: 1, AliasesInserted: 1}, nil
		},
	}
	daemon := &aliasPrunerFake{fakeAliasLinker: inner}
	runCycle(context.Background(), daemon)
	if got := len(inner.resynced); got != 2 {
		t.Fatalf("resync calls = %d, want 2 (one refusal must not stop the cycle)", got)
	}
	if daemon.pruneCalls != 1 {
		t.Fatalf("prune calls = %d, want 1", daemon.pruneCalls)
	}
}

// A bead with fewer than two linkable identities is a no-op, not an error.
func TestRunSlackOIDCOperatorAliasResyncCycle_NothingToLinkIsNotAnError(t *testing.T) {
	daemon := &fakeAliasLinker{
		pages: [][]*beadsapi.BeadDetail{{aliasUserBead("gd-solo")}},
		resyncFn: func(string) (*beadsapi.LinkOperatorAliasesResponse, error) {
			return &beadsapi.LinkOperatorAliasesResponse{NothingToLink: true}, nil
		},
	}
	runCycle(context.Background(), daemon)
	if len(daemon.resynced) != 1 {
		t.Fatalf("resync calls = %d, want 1", len(daemon.resynced))
	}
}

// After a complete scan the cycle asks the daemon to prune once. It sends no
// keep list (PruneUserBeadAliases takes none): the daemon computes it.
func TestRunSlackOIDCOperatorAliasResyncCycle_PrunesOncePerCompleteScan(t *testing.T) {
	daemon := &aliasPrunerFake{
		fakeAliasLinker: &fakeAliasLinker{pages: [][]*beadsapi.BeadDetail{{aliasUserBead("gd-a"), aliasUserBead("gd-b")}}},
		pruneResp: &beadsapi.PruneOperatorAliasesResponse{
			Deleted: []beadsapi.OperatorAliasRef{{Kind: "slack", Subject: "UOLD"}}, Count: 1,
		},
	}
	runCycle(context.Background(), daemon)
	if daemon.pruneCalls != 1 {
		t.Fatalf("prune calls = %d, want 1", daemon.pruneCalls)
	}
}

// A failed scan must not revoke anything: pruning on a partial view would
// delete aliases of beads the scan never reached.
func TestRunSlackOIDCOperatorAliasResyncCycle_NoPruneOnListError(t *testing.T) {
	daemon := &aliasPrunerFake{fakeAliasLinker: &fakeAliasLinker{listErr: errors.New("boom")}}
	runCycle(context.Background(), daemon)
	if daemon.pruneCalls != 0 {
		t.Errorf("prune calls = %d after a failed scan, want 0", daemon.pruneCalls)
	}
}

// No user beads at all: nothing to reconcile against, so no prune call.
func TestRunSlackOIDCOperatorAliasResyncCycle_NoPruneWithoutUserBeads(t *testing.T) {
	daemon := &aliasPrunerFake{fakeAliasLinker: &fakeAliasLinker{}}
	runCycle(context.Background(), daemon)
	if daemon.pruneCalls != 0 {
		t.Errorf("prune calls = %d with no user beads, want 0", daemon.pruneCalls)
	}
}
