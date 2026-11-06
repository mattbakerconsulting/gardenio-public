package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
)

// agentFake seeds one agent bead in the claimlayer project with a coopmux
// session and pod, on top of the two-project fixture.
func agentFake() *fakeDaemon {
	f := twoProjectFake()
	f.beads["cl-agent"] = &beadsapi.BeadDetail{
		ID: "cl-agent", Title: "cl agent pod", Type: "agent", Status: "in_progress",
		Labels: []string{"project:claimlayer"},
		Notes:  "pod_name: agent-cl-0",
		Fields: map[string]string{"agent": "cl-agent", "agent_state": "working", "last_session_id": "sess-1"},
	}
	return f
}

// The agent bead page renders a coopmux terminal snapshot and links to the
// coopmux dashboard (which lists every pod). Neither is for a scoped principal;
// both stay for an admin.
func TestScopedViewer_AgentBeadPageHasNoTerminal(t *testing.T) {
	cm := &fakeScreenClient{screen: &coopmuxapi.Screen{Lines: []string{"SECRETSCREENLINE"}}}
	srv := NewServer(newScopedDaemon(agentFake()), testLogger(), "", "", "", cm)
	srv.coopmuxPublicURL = "https://mux.example"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })

	get := func(bearer string, p principal) string {
		r := requestAs("GET", "/cl-agent", p)
		r = r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /cl-agent as %s = %d, want 200", p.email, rec.Code)
		}
		return rec.Body.String()
	}

	cm.lastID = ""
	scoped := get(scopedBearer, scopedPrincipal())
	if cm.lastID != "" {
		t.Error("scoped bead page fetched a coopmux screen snapshot")
	}
	for _, leak := range []string{"SECRETSCREENLINE", "mux.example", `id="term"`, "Watch live terminal"} {
		if strings.Contains(scoped, leak) {
			t.Errorf("scoped agent bead page contains %q", leak)
		}
	}

	admin := get(adminBearer, adminPrincipal())
	for _, want := range []string{"SECRETSCREENLINE", "https://mux.example#agent-cl-0", `id="term"`} {
		if !strings.Contains(admin, want) {
			t.Errorf("admin agent bead page is missing %q", want)
		}
	}
}

// The /agents rows carry a coopmux link; a scoped principal's rows do not.
func TestScopedViewer_AgentRowHasNoMuxLink(t *testing.T) {
	srv := NewServer(agentFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.coopmuxPublicURL = "https://mux.example"
	b := agentFake().beads["cl-agent"]
	now := time.Now()

	admin := srv.agentRowFromBeadCtx(context.Background(), b, "claimlayer", now)
	if admin.TerminalURL != "https://mux.example#agent-cl-0" {
		t.Fatalf("unrestricted row TerminalURL = %q, want the mux link", admin.TerminalURL)
	}
	scopedCtx := withViewerScope(context.Background(), viewerScope{Projects: []string{"claimlayer"}})
	if got := srv.agentRowFromBeadCtx(scopedCtx, b, "claimlayer", now).TerminalURL; got != "" {
		t.Errorf("scoped row TerminalURL = %q, want empty", got)
	}
}

// Nothing invalidates the hub's project memo, so a bead moved between projects
// is re-read once the TTL passes.
func TestFeedSSE_ProjectMemoExpires(t *testing.T) {
	fake := twoProjectFake()
	hub := NewBeadFeedHub(fake, testLogger())
	clock := time.Now()
	hub.projNow = func() time.Time { return clock }
	ctx := context.Background()

	if labels, ok := hub.beadProjectLabels(ctx, "cl-1"); !ok || len(labels) != 1 || labels[0] != "project:claimlayer" {
		t.Fatalf("first lookup = %v, %v", labels, ok)
	}
	fake.beads["cl-1"].Labels = []string{"project:gardenio"} // moved

	clock = clock.Add(beadFeedProjectTTL / 2)
	if labels, _ := hub.beadProjectLabels(ctx, "cl-1"); labels[0] != "project:claimlayer" {
		t.Errorf("within TTL the memo should still answer, got %v", labels)
	}
	clock = clock.Add(beadFeedProjectTTL)
	if labels, _ := hub.beadProjectLabels(ctx, "cl-1"); labels[0] != "project:gardenio" {
		t.Errorf("after TTL the bead should be re-read, got %v", labels)
	}
}

// A daemon 403 on the plan is the same not-found as a missing plan, not a 500.
func TestHandlePlanEvents_DaemonForbiddenIs404(t *testing.T) {
	srv := NewServer(newScopedDaemon(twoProjectFake()), testLogger(), "", "", "", &fakeScreenClient{})
	for _, id := range []string{"gd-1", "zz-404"} {
		r := requestAs("GET", "/plans/"+id+"/events", scopedPrincipal())
		r = r.WithContext(beadsapi.WithUserBearer(r.Context(), scopedBearer))
		r.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		srv.handlePlanEvents(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /plans/%s/events = %d, want 404", id, rec.Code)
		}
	}
}

// The Mezmo cache is process-wide and keyed by query, so a scoped principal
// neither reads nor fills it.
func TestMezmoFetch_ScopedBypassesCache(t *testing.T) {
	cache := newMezmoCache(8)
	var calls atomic.Int32
	fetch := func() (int, error) { return int(calls.Add(1)), nil }
	run := func(ctx context.Context) int {
		v, err := mezmoFetch(ctx, cache, "slug", "q", "1h", "f", fetch)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	scoped := withViewerScope(context.Background(), viewerScope{Projects: []string{"claimlayer"}})

	if a, b := run(scoped), run(scoped); a == b {
		t.Errorf("scoped fetches shared a result (%d, %d): cache was used", a, b)
	}
	// The scoped calls must not have filled the cache for an admin either.
	before := calls.Load()
	run(context.Background())
	run(context.Background())
	if got := calls.Load() - before; got != 1 {
		t.Errorf("unrestricted fetches hit upstream %d times, want 1 (cached after first)", got)
	}
}
