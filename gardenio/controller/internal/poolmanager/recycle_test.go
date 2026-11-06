package poolmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// recycleHarness is a mock daemon that serves an agent list and records which
// beads get PATCHed to status=closed (the recycle/close path) plus how many new
// prewarmed beads are POSTed (the refill path).
type recycleHarness struct {
	mu       sync.Mutex
	closed   []string
	created  int
	agents   []map[string]any
	createdF func() string // optional id generator for POSTed beads
}

func (h *recycleHarness) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			h.mu.Lock()
			beads := make([]map[string]any, len(h.agents))
			for i, a := range h.agents {
				beads[i] = map[string]any{
					"id":     a["id"],
					"title":  a["title"],
					"type":   "agent",
					"status": "open",
					"fields": a["fields"],
				}
			}
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})

		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			h.mu.Lock()
			h.created++
			id := "gd-new"
			if h.createdF != nil {
				id = h.createdF()
			}
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if s, _ := body["status"].(string); s == "closed" {
				id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
				h.mu.Lock()
				h.closed = append(h.closed, id)
				h.mu.Unlock()
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost:
			// AddLabel etc.
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

// warmAgent builds a prewarmed agent list entry for the gardenio/thread pool.
func warmAgent(id, image, state string, age time.Duration, extra map[string]string) map[string]any {
	fields := map[string]string{
		"agent":        id,
		"agent_state":  state,
		"mode":         "crew",
		"role":         "thread",
		"project":      "gardenio",
		"source_image": image,
		"created_at":   time.Now().Add(-age).Format(time.RFC3339),
	}
	for k, v := range extra {
		fields[k] = v
	}
	return map[string]any{"id": id, "title": id, "fields": fields}
}

func recycleTestConfig(image string) *config.Config {
	return &config.Config{
		CoopImage:             image,
		PrewarmedPoolInterval: 30 * time.Second,
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {
				PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
					Enabled: true,
					MinSize: 2,
					MaxSize: 5,
					Role:    "thread",
					Mode:    "crew",
				},
			},
		},
	}
}

// fakeRecorder counts RecordPrewarmedAgentCycled calls.
type fakeRecorder struct{ total int }

func (f *fakeRecorder) RecordPrewarmedAgentCycled(_, _ string, count int) { f.total += count }

func TestRecycleStaleAgents_ClosesStaleOnly(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, recycleTestConfig(cur), slog.Default())
	rec := &fakeRecorder{}
	pm.SetCycleRecorder(rec)

	agents := []prewarmedAgent{
		{ID: "stale-old", State: "prewarmed", SourceImage: "agent:2026.159.19", CreatedAt: time.Now().Add(-30 * time.Minute)},
		{ID: "stale-new", State: "idle", SourceImage: "agent:2026.160.1", CreatedAt: time.Now().Add(-20 * time.Minute)},
		{ID: "fresh", State: "prewarmed", SourceImage: cur, CreatedAt: time.Now().Add(-1 * time.Minute)},
	}
	survivors := pm.recycleStaleAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)

	// drainRate defaults to 2 → both stale closed in one pass; fresh survives.
	if got := len(h.closed); got != 2 {
		t.Fatalf("expected 2 closed, got %d (%v)", got, h.closed)
	}
	if !contains(h.closed, "stale-old") || !contains(h.closed, "stale-new") {
		t.Errorf("expected stale-old+stale-new closed, got %v", h.closed)
	}
	if len(survivors) != 1 || survivors[0].ID != "fresh" {
		t.Errorf("expected only fresh to survive, got %v", ids(survivors))
	}
	if rec.total != 2 {
		t.Errorf("expected recorder total 2, got %d", rec.total)
	}
}

// A stale pool member whose pod was capacity-deferred (agent_state=queued) with
// no thread/task is reclaimable surplus and MUST be recycled — otherwise it
// lingers on the OLD image after a release and, under a saturated COOP_MAX_PODS
// cap, the pool never converges to the new image (ka-0000006901). The e2e
// pool-recycle module hit exactly this: POOL_N=5 > COOP_MAX_PODS=4 flips ≥1
// stale member to queued, and the old {prewarmed,idle}-only recycler skipped it.
func TestRecycleStaleAgents_RecyclesQueuedSurplus(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, recycleTestConfig(cur), slog.Default())
	rec := &fakeRecorder{}
	pm.SetCycleRecorder(rec)

	stale := "agent:2026.160.1"
	agents := []prewarmedAgent{
		// Capacity-deferred stale surplus (no thread/task) — must be recycled.
		{ID: "queued-stale", State: "queued", SourceImage: stale, CreatedAt: time.Now().Add(-10 * time.Minute)},
		// A queued member carrying real work (thread) must never be recycled.
		{ID: "queued-assigned", State: "queued", SourceImage: stale, HasThread: true, CreatedAt: time.Now().Add(-9 * time.Minute)},
		// Fresh member survives.
		{ID: "fresh", State: "prewarmed", SourceImage: cur, CreatedAt: time.Now().Add(-1 * time.Minute)},
	}
	survivors := pm.recycleStaleAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)

	if len(h.closed) != 1 || h.closed[0] != "queued-stale" {
		t.Fatalf("expected only queued-stale recycled, got %v", h.closed)
	}
	if !containsAgent(survivors, "queued-assigned") || !containsAgent(survivors, "fresh") {
		t.Errorf("expected queued-assigned + fresh to survive, got %v", ids(survivors))
	}
	if rec.total != 1 {
		t.Errorf("expected recorder total 1, got %d", rec.total)
	}
}

func TestRecycleStaleAgents_NeverTouchesAssigned(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	pm := New(daemon, recycleTestConfig(cur), slog.Default())

	stale := "agent:2026.160.1"
	agents := []prewarmedAgent{
		{ID: "working", State: "working", SourceImage: stale},
		{ID: "assigning", State: "assigning", SourceImage: stale},
		{ID: "has-thread", State: "prewarmed", SourceImage: stale, HasThread: true},
		{ID: "has-task", State: "idle", SourceImage: stale, HasTask: true},
	}
	survivors := pm.recycleStaleAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)

	if len(h.closed) != 0 {
		t.Fatalf("expected 0 closed (none eligible), got %v", h.closed)
	}
	if len(survivors) != 4 {
		t.Errorf("expected all 4 to survive, got %v", ids(survivors))
	}
}

func TestRecycleStaleAgents_RateCapConvergence(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	pm := New(daemon, recycleTestConfig(cur), slog.Default())

	stale := "agent:2026.160.1"
	mk := func() []prewarmedAgent {
		var a []prewarmedAgent
		for i := 0; i < 5; i++ {
			a = append(a, prewarmedAgent{
				ID:          string(rune('a' + i)),
				State:       "prewarmed",
				SourceImage: stale,
				CreatedAt:   time.Now().Add(-time.Duration(50-i) * time.Minute),
			})
		}
		return a
	}

	// Pass 1: drainRate=2 → 2 closed, 3 survive.
	s1 := pm.recycleStaleAgents(context.Background(), poolKey{"gardenio", "thread"}, mk())
	if len(h.closed) != 2 || len(s1) != 3 {
		t.Fatalf("pass1: expected 2 closed / 3 survive, got %d closed %d survive", len(h.closed), len(s1))
	}
	// Oldest-first: a,b are the two oldest.
	if !contains(h.closed, "a") || !contains(h.closed, "b") {
		t.Errorf("expected oldest a,b closed first, got %v", h.closed)
	}
}

func TestRecycleStaleAgents_UnstampedTreatedStale(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	pm := New(daemon, recycleTestConfig(cur), slog.Default())

	agents := []prewarmedAgent{
		{ID: "unstamped", State: "prewarmed", SourceImage: "", CreatedAt: time.Now().Add(-time.Hour)},
	}
	survivors := pm.recycleStaleAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)
	if len(h.closed) != 1 || h.closed[0] != "unstamped" {
		t.Fatalf("expected unstamped recycled once, got %v", h.closed)
	}
	if len(survivors) != 0 {
		t.Errorf("expected no survivors, got %v", ids(survivors))
	}
}

func TestRecycleStaleAgents_UnknownCurrentImageFailsSafe(t *testing.T) {
	h := &recycleHarness{}
	srv := h.server(t)
	defer srv.Close()
	daemon, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	// CoopImage empty → effectiveImage == "" → recycle is a no-op.
	pm := New(daemon, recycleTestConfig(""), slog.Default())

	agents := []prewarmedAgent{
		{ID: "x", State: "prewarmed", SourceImage: "agent:old"},
	}
	survivors := pm.recycleStaleAgents(context.Background(), poolKey{"gardenio", "thread"}, agents)
	if len(h.closed) != 0 {
		t.Fatalf("expected 0 closed when current image unknown, got %v", h.closed)
	}
	if len(survivors) != 1 {
		t.Errorf("expected agent retained, got %v", ids(survivors))
	}
}

// TestReconcile_RecyclesStaleAndRefills exercises the full Reconcile pass: a
// pool of stale-image warm pods is rate-recycled and refilled on the current
// image in the same pass.
func TestReconcile_RecyclesStaleAndRefills(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{
		agents: []map[string]any{
			warmAgent("stale-1", "agent:2026.160.1", "prewarmed", 30*time.Minute, nil),
			warmAgent("stale-2", "agent:2026.160.1", "prewarmed", 25*time.Minute, nil),
		},
	}
	srv := h.server(t)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, recycleTestConfig(cur), slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	// Both stale pods recycled (drainRate=2, MinSize=2).
	if len(h.closed) != 2 {
		t.Errorf("expected 2 stale recycled, got %d (%v)", len(h.closed), h.closed)
	}
	// After recycling, 0 survivors → deficit = target(>=MinSize=2) → at least 2
	// fresh agents created on the current image.
	if h.created < 2 {
		t.Errorf("expected >=2 fresh agents created to refill, got %d", h.created)
	}
}

// TestReconcile_RecyclesQueuedStaleAndRefills is the full-pass analogue of the
// e2e pool-recycle convergence case (ka-0000006901): a stale pool member that
// was capacity-deferred to agent_state=queued must be picked up as a parked pool
// member, recycled, and refilled on the current image — not stranded on the old
// image because it left the "prewarmed" sub-state. The agent name carries the
// "prewarmed-" mint prefix so isParkedPoolMember counts it as reclaimable surplus.
func TestReconcile_RecyclesQueuedStaleAndRefills(t *testing.T) {
	cur := "agent:2026.160.7"
	h := &recycleHarness{
		agents: []map[string]any{
			warmAgent("prewarmed-stale-q1", "agent:2026.160.1", "queued", 30*time.Minute, nil),
			warmAgent("prewarmed-stale-p2", "agent:2026.160.1", "prewarmed", 25*time.Minute, nil),
		},
	}
	srv := h.server(t)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, recycleTestConfig(cur), slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	// Both stale members recycled — the queued one included (drainRate=2).
	if len(h.closed) != 2 {
		t.Errorf("expected 2 stale recycled (incl. queued), got %d (%v)", len(h.closed), h.closed)
	}
	if !contains(h.closed, "prewarmed-stale-q1") {
		t.Errorf("expected the queued stale member to be recycled, got %v", h.closed)
	}
	// After recycling, deficit → at least MinSize(2) fresh agents minted.
	if h.created < 2 {
		t.Errorf("expected >=2 fresh agents created to refill, got %d", h.created)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsAgent(a []prewarmedAgent, id string) bool {
	return contains(ids(a), id)
}

func ids(a []prewarmedAgent) []string {
	out := make([]string, len(a))
	for i, x := range a {
		out[i] = x.ID
	}
	return out
}
