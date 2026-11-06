package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeSubgraphClient is a stub graph: each entry lists the bead's forward
// deps, reverse deps, forward mentions, and reverse mentions. Edge types are
// not interpreted by the resolver — every neighbour goes into the closure
// regardless of edge type.
type fakeSubgraphClient struct {
	mu          sync.Mutex
	fwd         map[string][]beadsapi.Dependency
	rev         map[string][]beadsapi.Dependency
	mentions    map[string][]beadsapi.BeadReferenceSummary
	mentionedBy map[string][]beadsapi.BeadReferenceSummary

	getDepsCalls   atomic.Int64
	getRevCalls    atomic.Int64
	getMentions    atomic.Int64
	getMentionedBy atomic.Int64

	fwdErr error
}

func (f *fakeSubgraphClient) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	f.getDepsCalls.Add(1)
	if f.fwdErr != nil {
		return nil, f.fwdErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beadsapi.Dependency(nil), f.fwd[id]...), nil
}

func (f *fakeSubgraphClient) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	f.getRevCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beadsapi.Dependency(nil), f.rev[id]...), nil
}

func (f *fakeSubgraphClient) GetMentions(_ context.Context, id string, _ int) ([]beadsapi.BeadReferenceSummary, error) {
	f.getMentions.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beadsapi.BeadReferenceSummary(nil), f.mentions[id]...), nil
}

func (f *fakeSubgraphClient) GetMentionedBy(_ context.Context, id string, _ int) ([]beadsapi.BeadReferenceSummary, error) {
	f.getMentionedBy.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beadsapi.BeadReferenceSummary(nil), f.mentionedBy[id]...), nil
}

// dep is a tiny constructor for Dependency rows.
func dep(beadID, dependsOn, t string) beadsapi.Dependency {
	return beadsapi.Dependency{BeadID: beadID, DependsOnID: dependsOn, Type: t}
}

// ref is a tiny constructor for BeadReferenceSummary rows.
func ref(id string) beadsapi.BeadReferenceSummary {
	return beadsapi.BeadReferenceSummary{ID: id}
}

// TestSubgraphResolver_RootInOwnSubgraph confirms the trivial case: a bead is
// in its own subgraph and the resolver does not even hit the daemon for it.
func TestSubgraphResolver_RootInOwnSubgraph(t *testing.T) {
	client := &fakeSubgraphClient{}
	r := NewSubgraphResolver(client, SubgraphResolverConfig{})

	got, err := r.IsInSubgraph(context.Background(), "gd-root", "gd-root")
	if err != nil {
		t.Fatalf("IsInSubgraph: %v", err)
	}
	if !got {
		t.Errorf("root must be in its own subgraph")
	}
	if client.getDepsCalls.Load() != 0 {
		t.Errorf("root==candidate must short-circuit before any RPC; got %d GetDependencies calls", client.getDepsCalls.Load())
	}
}

// TestSubgraphResolver_AllEdgeKinds confirms the BFS traverses parent-child,
// blocks, related, forward mentions, and reverse mentions — the four edge
// directions listed in ka-0000007345.
func TestSubgraphResolver_AllEdgeKinds(t *testing.T) {
	// Root has one neighbour reachable via each edge direction:
	//   gd-fwd     ← forward dep (any type)
	//   gd-rev     ← reverse dep (any type)
	//   gd-mention ← forward mention (root mentions it)
	//   gd-back    ← reverse mention (it mentions root)
	// Also one deeper hop via the forward-dep target so we exercise BFS.
	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-root": {dep("gd-root", "gd-fwd", "parent-child")},
			"gd-fwd":  {dep("gd-fwd", "gd-deep", "blocks")},
		},
		rev: map[string][]beadsapi.Dependency{
			"gd-root": {dep("gd-rev", "gd-root", "related")},
		},
		mentions: map[string][]beadsapi.BeadReferenceSummary{
			"gd-root": {ref("gd-mention")},
		},
		mentionedBy: map[string][]beadsapi.BeadReferenceSummary{
			"gd-root": {ref("gd-back")},
		},
	}
	r := NewSubgraphResolver(client, SubgraphResolverConfig{MaxDepth: 4})

	for _, candidate := range []string{"gd-fwd", "gd-rev", "gd-mention", "gd-back", "gd-deep"} {
		got, err := r.IsInSubgraph(context.Background(), "gd-root", candidate)
		if err != nil {
			t.Fatalf("IsInSubgraph(%s): %v", candidate, err)
		}
		if !got {
			t.Errorf("expected %s in subgraph of gd-root via BFS over all edges", candidate)
		}
	}

	got, err := r.IsInSubgraph(context.Background(), "gd-root", "gd-unrelated")
	if err != nil {
		t.Fatalf("IsInSubgraph(gd-unrelated): %v", err)
	}
	if got {
		t.Errorf("gd-unrelated must not be in subgraph")
	}
}

// TestSubgraphResolver_DepthBound confirms the MaxDepth cap stops BFS at the
// configured horizon. depth=1 reaches the root's direct neighbours but not
// their neighbours.
func TestSubgraphResolver_DepthBound(t *testing.T) {
	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-root": {dep("gd-root", "gd-1", "parent-child")},
			"gd-1":    {dep("gd-1", "gd-2", "parent-child")},
			"gd-2":    {dep("gd-2", "gd-3", "parent-child")},
		},
	}
	r := NewSubgraphResolver(client, SubgraphResolverConfig{MaxDepth: 1})

	in, err := r.IsInSubgraph(context.Background(), "gd-root", "gd-1")
	if err != nil || !in {
		t.Fatalf("gd-1 expected in subgraph at depth 1; got %v err=%v", in, err)
	}
	// MaxDepth=1 means we expand root once. gd-1 is added but its neighbours
	// (gd-2) are NOT visited.
	in, err = r.IsInSubgraph(context.Background(), "gd-root", "gd-2")
	if err != nil {
		t.Fatalf("IsInSubgraph(gd-2): %v", err)
	}
	if in {
		t.Errorf("gd-2 must not be in subgraph at depth 1")
	}
}

// TestSubgraphResolver_Cycles confirms the BFS terminates on a cycle without
// re-visiting nodes.
func TestSubgraphResolver_Cycles(t *testing.T) {
	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-a": {dep("gd-a", "gd-b", "parent-child")},
			"gd-b": {dep("gd-b", "gd-a", "parent-child")},
		},
	}
	r := NewSubgraphResolver(client, SubgraphResolverConfig{MaxDepth: 4})

	in, err := r.IsInSubgraph(context.Background(), "gd-a", "gd-b")
	if err != nil {
		t.Fatalf("IsInSubgraph: %v", err)
	}
	if !in {
		t.Errorf("gd-b must be in subgraph of gd-a despite cycle")
	}

	// Two beads, four edge-direction RPCs each = exactly 8 RPCs for one
	// closure expansion (no cycling).
	totalRPCs := client.getDepsCalls.Load() + client.getRevCalls.Load() +
		client.getMentions.Load() + client.getMentionedBy.Load()
	if totalRPCs != 8 {
		t.Errorf("expected 8 RPCs (2 beads × 4 edge directions), got %d", totalRPCs)
	}
}

// TestSubgraphResolver_Cache confirms the TTL cache short-circuits repeated
// queries against the same root.
func TestSubgraphResolver_Cache(t *testing.T) {
	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-root": {dep("gd-root", "gd-1", "parent-child")},
		},
	}
	now := time.Unix(1_700_000_000, 0)
	clock := &now
	r := NewSubgraphResolver(client, SubgraphResolverConfig{
		MaxDepth: 4,
		TTL:      10 * time.Second,
		Now:      func() time.Time { return *clock },
	})

	for range 5 {
		if _, err := r.IsInSubgraph(context.Background(), "gd-root", "gd-1"); err != nil {
			t.Fatalf("IsInSubgraph: %v", err)
		}
	}
	// One expansion → 2 beads × 4 RPCs = 8 RPCs total across all 5 calls.
	totalRPCs := client.getDepsCalls.Load() + client.getRevCalls.Load() +
		client.getMentions.Load() + client.getMentionedBy.Load()
	if totalRPCs != 8 {
		t.Errorf("expected 8 RPCs (1 expansion, cached for the rest); got %d", totalRPCs)
	}

	// Advance the clock past TTL and verify the next call re-expands.
	*clock = now.Add(11 * time.Second)
	if _, err := r.IsInSubgraph(context.Background(), "gd-root", "gd-1"); err != nil {
		t.Fatalf("IsInSubgraph: %v", err)
	}
	totalRPCs = client.getDepsCalls.Load() + client.getRevCalls.Load() +
		client.getMentions.Load() + client.getMentionedBy.Load()
	if totalRPCs != 16 {
		t.Errorf("expected 16 RPCs after TTL expiry (2 expansions); got %d", totalRPCs)
	}
}

// TestSubgraphResolver_NodeBound confirms MaxNodes saturates the BFS and
// returns false for candidates beyond the cap.
func TestSubgraphResolver_NodeBound(t *testing.T) {
	// Linear chain longer than the cap.
	fwd := map[string][]beadsapi.Dependency{}
	for i := range 50 {
		fwd[fmtID(i)] = []beadsapi.Dependency{dep(fmtID(i), fmtID(i+1), "parent-child")}
	}
	client := &fakeSubgraphClient{fwd: fwd}
	r := NewSubgraphResolver(client, SubgraphResolverConfig{
		MaxDepth: 100,
		MaxNodes: 5,
	})

	in, err := r.IsInSubgraph(context.Background(), "gd-0", "gd-3")
	if err != nil || !in {
		t.Fatalf("gd-3 expected in saturated closure; got %v err=%v", in, err)
	}
	in, err = r.IsInSubgraph(context.Background(), "gd-0", "gd-42")
	if err != nil {
		t.Fatalf("IsInSubgraph(gd-42): %v", err)
	}
	if in {
		t.Errorf("gd-42 must not appear in MaxNodes-saturated closure")
	}
}

// TestSubgraphResolver_PropagatesError confirms a transport error from the
// underlying client bubbles up to the caller (fail-closed semantics).
func TestSubgraphResolver_PropagatesError(t *testing.T) {
	client := &fakeSubgraphClient{
		fwd:    map[string][]beadsapi.Dependency{},
		fwdErr: errors.New("boom"),
	}
	r := NewSubgraphResolver(client, SubgraphResolverConfig{})

	_, err := r.IsInSubgraph(context.Background(), "gd-root", "gd-target")
	if err == nil {
		t.Fatal("expected error from underlying client to propagate")
	}
}

// TestSubgraphResolver_NilClientErrors confirms the resolver returns a clear
// error when no client is wired — protects against misconfiguration.
func TestSubgraphResolver_NilClientErrors(t *testing.T) {
	r := NewSubgraphResolver(nil, SubgraphResolverConfig{})
	if _, err := r.IsInSubgraph(context.Background(), "gd-a", "gd-b"); err == nil {
		t.Fatal("expected error from nil client")
	}
}

// TestSubgraphResolver_EmptyArgs confirms missing root or candidate yields
// (false, nil) rather than an error.
func TestSubgraphResolver_EmptyArgs(t *testing.T) {
	r := NewSubgraphResolver(&fakeSubgraphClient{}, SubgraphResolverConfig{})

	for _, tc := range []struct{ root, candidate string }{
		{"", "gd-x"},
		{"gd-x", ""},
		{"", ""},
	} {
		got, err := r.IsInSubgraph(context.Background(), tc.root, tc.candidate)
		if err != nil {
			t.Errorf("IsInSubgraph(%q,%q): unexpected error %v", tc.root, tc.candidate, err)
		}
		if got {
			t.Errorf("IsInSubgraph(%q,%q) = true; want false", tc.root, tc.candidate)
		}
	}
}

func fmtID(i int) string {
	return "gd-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
