package server

import (
	"context"
	"errors"
	"testing"

	"gardenio/kbeads/internal/events"
)

// countingTaintStore wraps mockStore to count GetAllTaintsForBead calls
// (taintContextOf resolves the caller's own taint set unfiltered by effect,
// per docs/adr/taints.md §15.2: propagation is effect-agnostic), so
// cache-hit/cache-miss behavior can be asserted without a real database.
type countingTaintStore struct {
	*mockStore
	calls int
	err   error
}

func (c *countingTaintStore) GetAllTaintsForBead(ctx context.Context, beadID string) ([]int64, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.mockStore.GetAllTaintsForBead(ctx, beadID)
}

func newCountingTaintServer() (*BeadsServer, *countingTaintStore) {
	ms := newMockStore()
	cs := &countingTaintStore{mockStore: ms}
	s := NewBeadsServer(cs, &events.NoopPublisher{})
	s.userProvisioner = nil
	return s, cs
}

func TestTaintContextOf_AgentStaticToken(t *testing.T) {
	s, cs := newCountingTaintServer()
	cs.taints = map[string][]int64{"gd-agent1": {1, 2}}

	ctx := withTestAgent(context.Background(), "gd-agent1")
	got, err := s.taintContextOf(ctx)
	if err != nil {
		t.Fatalf("taintContextOf: %v", err)
	}
	if len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("expected taint set {1,2}, got %v", got)
	}
	if cs.calls != 1 {
		t.Fatalf("expected 1 store call, got %d", cs.calls)
	}
}

func TestTaintContextOf_OIDCHumanIsEmpty(t *testing.T) {
	s, cs := newCountingTaintServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	got, err := s.taintContextOf(ctx)
	if err != nil {
		t.Fatalf("taintContextOf: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty taint set for OIDC human, got %v", got)
	}
	if cs.calls != 0 {
		t.Fatalf("expected no store call for OIDC human, got %d", cs.calls)
	}
}

func TestTaintContextOf_PrimaryStaticTokenIsEmpty(t *testing.T) {
	s, cs := newCountingTaintServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "test-bridge"})
	got, err := s.taintContextOf(ctx)
	if err != nil {
		t.Fatalf("taintContextOf: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty taint set for the primary static token, got %v", got)
	}
	if cs.calls != 0 {
		t.Fatalf("expected no store call for the primary static token, got %d", cs.calls)
	}
}

func TestTaintContextOf_FleetTokenIsEmpty(t *testing.T) {
	s, cs := newCountingTaintServer()

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "gardenio-fleet"})
	got, err := s.taintContextOf(ctx)
	if err != nil {
		t.Fatalf("taintContextOf: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty taint set for a fleet token, got %v", got)
	}
	if cs.calls != 0 {
		t.Fatalf("expected no store call for a fleet token, got %d", cs.calls)
	}
}

func TestTaintContextOf_CacheHitSkipsQuery(t *testing.T) {
	s, cs := newCountingTaintServer()
	cs.taints = map[string][]int64{"gd-agent1": {7}}

	ctx := withTestAgent(context.Background(), "gd-agent1")

	if _, err := s.taintContextOf(ctx); err != nil {
		t.Fatalf("first taintContextOf: %v", err)
	}
	if cs.calls != 1 {
		t.Fatalf("expected 1 store call after first resolution, got %d", cs.calls)
	}

	// Mutate the backing store directly: a cache hit must not observe this,
	// proving the second call is served from cache rather than re-querying.
	cs.taints["gd-agent1"] = []int64{7, 8, 9}

	got, err := s.taintContextOf(ctx)
	if err != nil {
		t.Fatalf("second taintContextOf: %v", err)
	}
	if cs.calls != 1 {
		t.Fatalf("expected cache hit to perform no query, but store was called %d times", cs.calls)
	}
	if len(got) != 1 || !got[7] {
		t.Fatalf("expected cached taint set {7}, got %v", got)
	}
}

func TestTaintContextOf_CacheMissAfterTTLExpiry(t *testing.T) {
	s, cs := newCountingTaintServer()
	cs.taints = map[string][]int64{"gd-agent1": {7}}

	ctx := withTestAgent(context.Background(), "gd-agent1")

	if _, err := s.taintContextOf(ctx); err != nil {
		t.Fatalf("first taintContextOf: %v", err)
	}
	s.initTaintContextCache()
	// Force the cached entry to look stale without waiting out the real TTL.
	fakeNow := s.taintContextCache.now
	s.taintContextCache.entries["gd-agent1"].cachedAt = fakeNow().Add(-2 * identityCacheTTL)

	cs.taints["gd-agent1"] = []int64{7, 8}
	got, err := s.taintContextOf(ctx)
	if err != nil {
		t.Fatalf("second taintContextOf: %v", err)
	}
	if cs.calls != 2 {
		t.Fatalf("expected a fresh query after TTL expiry, got %d calls", cs.calls)
	}
	if len(got) != 2 || !got[7] || !got[8] {
		t.Fatalf("expected refreshed taint set {7,8}, got %v", got)
	}
}

func TestTaintContextOf_StoreErrorPropagates(t *testing.T) {
	s, cs := newCountingTaintServer()
	wantErr := errors.New("boom")
	cs.err = wantErr

	ctx := withTestAgent(context.Background(), "gd-agent1")
	_, err := s.taintContextOf(ctx)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected store error to propagate, got %v", err)
	}
	if cs.calls != 1 {
		t.Fatalf("expected 1 store call, got %d", cs.calls)
	}
}
