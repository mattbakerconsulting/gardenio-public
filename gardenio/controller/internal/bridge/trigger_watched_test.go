package bridge

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeHydrator stubs beadHydrator: it returns a BeadDetail for known IDs and
// counts GetBead calls so tests can assert the TTL cache suppresses re-fetch.
type fakeHydrator struct {
	beads map[string]*beadsapi.BeadDetail
	calls atomic.Int64
	err   error
}

func (f *fakeHydrator) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.beads[id], nil
}

func detail(id, status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "task", Status: status}
}

func TestWatchedResolver_ResolvesAndExcludesRoot(t *testing.T) {
	// Graph: root -> a, root -> b (forward deps).
	sg := NewSubgraphResolver(&fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"root": {dep("root", "a", "blocks"), dep("root", "b", "blocks")},
		},
	}, SubgraphResolverConfig{})

	hyd := &fakeHydrator{beads: map[string]*beadsapi.BeadDetail{
		"root": detail("root", "open"),
		"a":    detail("a", "closed"),
		"b":    detail("b", "in_progress"),
	}}

	w := NewWatchedResolver(sg, hyd, WatchedResolverConfig{})
	got, err := w.Resolve(context.Background(), "root")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	ids := map[string]bool{}
	for _, b := range got {
		ids[b.ID] = true
	}
	if ids["root"] {
		t.Errorf("root must be excluded from watched (it is exposed as `bead`)")
	}
	if !ids["a"] || !ids["b"] {
		t.Errorf("watched should contain a and b, got %v", ids)
	}
}

func TestWatchedResolver_TTLCacheSuppressesRefetch(t *testing.T) {
	clock := fixedExprNow
	sg := NewSubgraphResolver(&fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"root": {dep("root", "a", "blocks")},
		},
	}, SubgraphResolverConfig{Now: func() time.Time { return clock }})

	hyd := &fakeHydrator{beads: map[string]*beadsapi.BeadDetail{
		"root": detail("root", "open"),
		"a":    detail("a", "closed"),
	}}

	w := NewWatchedResolver(sg, hyd, WatchedResolverConfig{
		TTL: 30 * time.Second,
		Now: func() time.Time { return clock },
	})

	if _, err := w.Resolve(context.Background(), "root"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	first := hyd.calls.Load()
	if first == 0 {
		t.Fatalf("expected hydration calls on first resolve")
	}
	// Second resolve within TTL: served from cache, no new GetBead calls.
	if _, err := w.Resolve(context.Background(), "root"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got := hyd.calls.Load(); got != first {
		t.Errorf("TTL cache should suppress re-hydration: calls went %d -> %d", first, got)
	}
	// Advance past TTL: cache expires, hydration runs again.
	clock = clock.Add(31 * time.Second)
	if _, err := w.Resolve(context.Background(), "root"); err != nil {
		t.Fatalf("third resolve: %v", err)
	}
	if got := hyd.calls.Load(); got <= first {
		t.Errorf("after TTL expiry hydration should run again: calls %d, want > %d", got, first)
	}
}

func TestWatchedResolver_NodeCapTruncates(t *testing.T) {
	// root fans out to 10 children; cap hydration at 3.
	fwd := []beadsapi.Dependency{}
	beads := map[string]*beadsapi.BeadDetail{"root": detail("root", "open")}
	for i := range 10 {
		id := fmt.Sprintf("c%d", i)
		fwd = append(fwd, dep("root", id, "blocks"))
		beads[id] = detail(id, "open")
	}
	sg := NewSubgraphResolver(&fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{"root": fwd},
	}, SubgraphResolverConfig{})
	hyd := &fakeHydrator{beads: beads}

	w := NewWatchedResolver(sg, hyd, WatchedResolverConfig{MaxNodes: 3})
	got, err := w.Resolve(context.Background(), "root")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("node cap should truncate watched to 3, got %d", len(got))
	}
}

func TestWatchedResolver_NilResolverSafe(t *testing.T) {
	var w *WatchedResolver
	got, err := w.Resolve(context.Background(), "root")
	if err != nil || got != nil {
		t.Errorf("nil WatchedResolver.Resolve = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestWatchedResolver_ErrorFailsClosed(t *testing.T) {
	sg := NewSubgraphResolver(&fakeSubgraphClient{
		fwd:    map[string][]beadsapi.Dependency{"root": {dep("root", "a", "blocks")}},
		fwdErr: fmt.Errorf("boom"),
	}, SubgraphResolverConfig{})
	hyd := &fakeHydrator{beads: map[string]*beadsapi.BeadDetail{}}
	w := NewWatchedResolver(sg, hyd, WatchedResolverConfig{})
	if _, err := w.Resolve(context.Background(), "root"); err == nil {
		t.Errorf("a closure error must propagate so the expr fails closed")
	}
}

func TestWatchedResolver_PartialHydrationBestEffort(t *testing.T) {
	// b is missing from the hydrator's map (GetBead returns nil) — it is
	// dropped, but a still resolves.
	sg := NewSubgraphResolver(&fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"root": {dep("root", "a", "blocks"), dep("root", "b", "blocks")},
		},
	}, SubgraphResolverConfig{})
	hyd := &fakeHydrator{beads: map[string]*beadsapi.BeadDetail{
		"root": detail("root", "open"),
		"a":    detail("a", "closed"),
		// "b" absent → GetBead returns (nil, nil)
	}}
	w := NewWatchedResolver(sg, hyd, WatchedResolverConfig{})
	got, err := w.Resolve(context.Background(), "root")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("partial hydration should drop missing bead b and keep a, got %+v", got)
	}
}
