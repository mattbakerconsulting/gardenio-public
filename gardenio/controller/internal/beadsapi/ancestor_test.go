package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestIsAncestor walks the parent-child dep chain. The httptest server
// hands back canned dependency responses keyed on the bead id in the URL
// (/v1/beads/<id>/dependencies); IsAncestor walks up until it hits the
// requested parent, runs out of edges, or trips the depth/cycle guard.
func TestIsAncestor(t *testing.T) {
	// Graph (child → parent):
	//   gd-leaf → gd-mid → gd-top
	//   gd-orphan (no parent)
	//   gd-cycle1 → gd-cycle2 → gd-cycle1 (cycle)
	parents := map[string][]Dependency{
		"gd-leaf":   {{BeadID: "gd-leaf", DependsOnID: "gd-mid", Type: "parent-child"}},
		"gd-mid":    {{BeadID: "gd-mid", DependsOnID: "gd-top", Type: "parent-child"}},
		"gd-top":    nil,
		"gd-orphan": nil,
		// Mixed dep types — only "parent-child" should be followed.
		"gd-mixed": {
			{BeadID: "gd-mixed", DependsOnID: "gd-blocker", Type: "blocks"},
			{BeadID: "gd-mixed", DependsOnID: "gd-top", Type: "parent-child"},
		},
		"gd-cycle1": {{BeadID: "gd-cycle1", DependsOnID: "gd-cycle2", Type: "parent-child"}},
		"gd-cycle2": {{BeadID: "gd-cycle2", DependsOnID: "gd-cycle1", Type: "parent-child"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path is /v1/beads/<id>/dependencies.
		const prefix = "/v1/beads/"
		const suffix = "/dependencies"
		path := r.URL.Path
		if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
		deps, ok := parents[id]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	ctx := context.Background()

	cases := []struct {
		name           string
		parent, child  string
		want           bool
		wantErrContain string
	}{
		{"trivial: parent==child", "gd-leaf", "gd-leaf", true, ""},
		{"empty parent", "", "gd-leaf", false, ""},
		{"empty child", "gd-top", "", false, ""},
		{"direct parent", "gd-mid", "gd-leaf", true, ""},
		{"transitive ancestor", "gd-top", "gd-leaf", true, ""},
		{"unrelated bead", "gd-other", "gd-leaf", false, ""},
		{"orphan child has no ancestors", "gd-top", "gd-orphan", false, ""},
		{"only parent-child edges followed", "gd-blocker", "gd-mixed", false, ""},
		{"parent-child reachable via mixed", "gd-top", "gd-mixed", true, ""},
		{"cycle terminates without false positive", "gd-other", "gd-cycle1", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.IsAncestor(ctx, tc.parent, tc.child)
			if tc.wantErrContain != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContain) {
					t.Fatalf("expected err contains %q, got %v", tc.wantErrContain, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("IsAncestor(%q,%q): %v", tc.parent, tc.child, err)
			}
			if got != tc.want {
				t.Errorf("IsAncestor(%q,%q) = %v, want %v", tc.parent, tc.child, got, tc.want)
			}
		})
	}
}

// TestIsAncestor_ParentCache checks the cache EnableParentCache arms: a
// repeated walk costs no GETs, InvalidateParents and the TTL each force one
// refetch, and a fetch that races an invalidation is not cached
// (gd-ZaztjcPrDc).
func TestIsAncestor_ParentCache(t *testing.T) {
	var mu sync.Mutex
	parents := map[string]string{"gd-leaf": "gd-mid", "gd-mid": "gd-top"}
	gets := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/dependencies")
		mu.Lock()
		gets[id]++
		parent := parents[id]
		mu.Unlock()
		var deps []Dependency
		if parent != "" {
			deps = append(deps, Dependency{BeadID: id, DependsOnID: parent, Type: "parent-child"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	c.EnableParentCache(time.Minute)
	now := time.Unix(1_800_000_000, 0)
	c.parents.now = func() time.Time { return now }
	ctx := context.Background()
	isAnc := func(parent, child string) bool {
		t.Helper()
		got, err := c.IsAncestor(ctx, parent, child)
		if err != nil {
			t.Fatalf("IsAncestor(%q,%q): %v", parent, child, err)
		}
		return got
	}
	getsOf := func(id string) int {
		mu.Lock()
		defer mu.Unlock()
		return gets[id]
	}
	total := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, v := range gets {
			n += v
		}
		return n
	}

	// gd-other is not an ancestor, so the walk runs to the root: 3 GETs.
	if isAnc("gd-other", "gd-leaf") {
		t.Fatal("gd-other is not an ancestor of gd-leaf")
	}
	if got := total(); got != 3 {
		t.Fatalf("first walk: %d GETs, want 3", got)
	}
	for range 10 {
		if !isAnc("gd-top", "gd-leaf") || isAnc("gd-other", "gd-leaf") {
			t.Fatal("cached walk answered wrongly")
		}
	}
	if got := total(); got != 3 {
		t.Fatalf("cached walks: %d GETs, want still 3", got)
	}

	// Reparent gd-mid under gd-new; the dependency event invalidates it.
	mu.Lock()
	parents["gd-mid"] = "gd-new"
	mu.Unlock()
	c.InvalidateParents("gd-mid", "gd-new")
	if !isAnc("gd-new", "gd-leaf") || isAnc("gd-top", "gd-leaf") {
		t.Fatal("walk after invalidation did not see the new parent")
	}
	if got := getsOf("gd-mid"); got != 2 {
		t.Fatalf("gd-mid fetched %d times, want 2 (one refetch after invalidation)", got)
	}
	if got := getsOf("gd-leaf"); got != 1 {
		t.Fatalf("gd-leaf fetched %d times, want 1 (its entry was not invalidated)", got)
	}

	// Past the TTL every entry refetches once.
	before := total()
	now = now.Add(time.Minute)
	isAnc("gd-other", "gd-leaf")
	if got := total() - before; got != 3 {
		t.Fatalf("walk after TTL: %d GETs, want 3", got)
	}

	// A fetch that began before an invalidation must not be stored.
	pc := c.parents
	_, gen, _ := pc.get("gd-race")
	pc.invalidate([]string{"gd-race"})
	pc.put("gd-race", "gd-stale", gen)
	if _, _, ok := pc.get("gd-race"); ok {
		t.Fatal("a put under a pre-invalidation generation was cached")
	}
}

// TestIsAncestor_ParentCacheOffByDefault keeps every other caller on the
// uncached walk: without EnableParentCache each call refetches each level.
func TestIsAncestor_ParentCacheOffByDefault(t *testing.T) {
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": []Dependency{}})
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	for range 3 {
		if _, err := c.IsAncestor(context.Background(), "gd-top", "gd-leaf"); err != nil {
			t.Fatal(err)
		}
	}
	if got := gets.Load(); got != 3 {
		t.Fatalf("%d GETs, want 3", got)
	}
}
