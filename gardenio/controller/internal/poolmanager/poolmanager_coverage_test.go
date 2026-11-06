package poolmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// --- closePrewarmedAgent ---

func TestClosePrewarmedAgent(t *testing.T) {
	tests := []struct {
		name       string
		serverCode int
		wantErr    bool
	}{
		{"success", http.StatusNoContent, false},
		{"server error", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch {
					t.Errorf("expected PATCH, got %s", r.Method)
				}
				if !strings.HasPrefix(r.URL.Path, "/v1/beads/") {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(tt.serverCode)
			}))
			defer srv.Close()

			daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			pm := New(daemon, &config.Config{}, slog.Default())

			err = pm.closePrewarmedAgent(context.Background(), "gd-x")
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantErr && gotBody["status"] != "closed" {
				t.Errorf("expected status=closed in body, got %v", gotBody)
			}
		})
	}
}

// --- nudgeAssignedAgent ---

// The work description goes to the assigned agent as interrupt mail; nothing
// dials the pod.
func TestNudgeAssignedAgent_CreatesInterruptMail(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/beads" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-m1"})
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, &config.Config{}, slog.Default())
	pm.nudgeAssignedAgent("gd-a1", "prewarmed-alpha", "fix the build")

	if got["type"] != "mail" || got["assignee"] != "prewarmed-alpha" {
		t.Fatalf("mail = %v, want type=mail assignee=prewarmed-alpha", got)
	}
	if desc, _ := got["description"].(string); !strings.Contains(desc, "fix the build") {
		t.Errorf("description %q missing the work", desc)
	}
	labels, _ := got["labels"].([]any)
	want := map[string]bool{"from:controller": false, "delivery:interrupt": false}
	for _, l := range labels {
		if s, ok := l.(string); ok {
			if _, ok := want[s]; ok {
				want[s] = true
			}
		}
	}
	for l, seen := range want {
		if !seen {
			t.Errorf("labels %v missing %q", labels, l)
		}
	}
}

// --- RunLoop ---

func TestRunLoop_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return empty list for any query.
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	// Empty project cache — Reconcile is a near-noop.
	cfg := &config.Config{
		PrewarmedPoolInterval: 5 * time.Millisecond,
		ProjectCache:          map[string]config.ProjectCacheEntry{},
	}
	pm := New(daemon, cfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pm.RunLoop(ctx)
		close(done)
	}()

	// Let it tick at least once.
	time.Sleep(15 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return after context cancel")
	}
}

func TestRunLoop_DefaultInterval(t *testing.T) {
	// interval <= 0 should fall through to 30s default. We just need to
	// enter the function and exit via ctx cancellation before the first tick.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		// Zero interval triggers the default branch.
		PrewarmedPoolInterval: 0,
		ProjectCache:          map[string]config.ProjectCacheEntry{},
	}
	pm := New(daemon, cfg, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pm.RunLoop(ctx)
		close(done)
	}()

	// Give the initial reconcile time to run, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return after context cancel")
	}
}

func TestRunLoop_InitialReconcileError(t *testing.T) {
	// Return 500 on list so initial Reconcile fails — RunLoop should still
	// enter its tick loop and return on ctx cancel.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		PrewarmedPoolInterval: 5 * time.Millisecond,
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
				Enabled: true, MinSize: 1, MaxSize: 2, Role: "thread", Mode: "crew",
			}},
		},
	}
	pm := New(daemon, cfg, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	pm.RunLoop(ctx) // returns when ctx expires
}

// --- Reconcile: additional branches ---

// reconcileMux builds a daemon mock with controllable agent list + tracking.
type reconcileMock struct {
	t        *testing.T
	agents   []map[string]any
	created  atomic.Int32
	patched  atomic.Int32
	closed   atomic.Int32
}

func (m *reconcileMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": m.agents, "total": len(m.agents)})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// Used by UpdateBeadFields read-side. Return the matching agent.
			for _, a := range m.agents {
				if "/v1/beads/"+a["id"].(string) == r.URL.Path {
					_ = json.NewEncoder(w).Encode(a)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			c := m.created.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("gd-new-%d", c)})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// closePrewarmedAgent sends status; count those separately.
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["status"] == "closed" {
				m.closed.Add(1)
			}
			m.patched.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			// AddLabel, etc.
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestReconcile_ClosesAgentsForDisabledProject(t *testing.T) {
	// Agent belongs to project "oldproj" — which is NOT in the project cache
	// (or is disabled). Reconcile should close it.
	mock := &reconcileMock{t: t, agents: []map[string]any{
		{
			"id":     "gd-orphan",
			"title":  "orphan",
			"type":   "agent",
			"status": "open",
			"fields": map[string]string{
				"agent":       "orphan",
				"agent_state": "prewarmed",
				"project":     "oldproj",
			},
		},
	}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	// No pools enabled in cache → all prewarmed agents are orphaned.
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.closed.Load(); got != 1 {
		t.Errorf("expected 1 close for disabled-project orphan, got %d", got)
	}
	if got := mock.created.Load(); got != 0 {
		t.Errorf("expected no new agents, got %d", got)
	}
}

func TestReconcile_ClosesExcessAgents(t *testing.T) {
	// Pool config: min=max=2 → target=2. With 4 existing → close 2.
	now := time.Now()
	var agents []map[string]any
	for i := range 4 {
		agents = append(agents, map[string]any{
			"id":     fmt.Sprintf("gd-a%d", i),
			"title":  fmt.Sprintf("a%d", i),
			"type":   "agent",
			"status": "open",
			"fields": map[string]string{
				"agent":       fmt.Sprintf("a%d", i),
				"agent_state": "prewarmed",
				"project":     "gardenio",
				"role":        "thread",
				"created_at":  now.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			},
		})
	}
	mock := &reconcileMock{t: t, agents: agents}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true, MinSize: 2, MaxSize: 2, Role: "thread", Mode: "crew",
		}},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.closed.Load(); got != 2 {
		t.Errorf("expected 2 excess closes, got %d", got)
	}
	if got := mock.created.Load(); got != 0 {
		t.Errorf("expected no new creations, got %d", got)
	}
}

func TestReconcile_AtTargetNoOp(t *testing.T) {
	// min=1, max=2, 1 existing agent. No creations, no closes.
	mock := &reconcileMock{t: t, agents: []map[string]any{
		{
			"id":     "gd-a",
			"title":  "a",
			"type":   "agent",
			"status": "open",
			"fields": map[string]string{
				"agent":       "a",
				"agent_state": "prewarmed",
				"project":     "gardenio",
				"role":        "thread",
			},
		},
	}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true, MinSize: 1, MaxSize: 2, Role: "thread", Mode: "crew",
		}},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.created.Load(); got != 0 {
		t.Errorf("at-target should create nothing, got %d", got)
	}
	if got := mock.closed.Load(); got != 0 {
		t.Errorf("at-target should close nothing, got %d", got)
	}
}

func TestReconcile_ListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true, MinSize: 1, MaxSize: 2, Role: "thread", Mode: "crew",
		}},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err == nil {
		t.Fatal("expected error from list failure")
	}
}

func TestReconcile_CreateFailureAbortsInnerLoop(t *testing.T) {
	// Pool needs 3 agents, but every CreateBead returns 500 — the inner loop
	// should break after the first failure rather than calling 3 times.
	var createCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			createCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true, MinSize: 3, MaxSize: 5, Role: "thread", Mode: "crew",
		}},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile should not bubble create errors: %v", err)
	}
	if got := createCalls.Load(); got != 1 {
		t.Errorf("expected single create attempt before break, got %d", got)
	}
}

// TestReconcile_MultiRolePools verifies a single project can prewarm several
// role-keyed pools concurrently (gap G1). The project declares two roles via
// the Roles list; each (project,role) pool is reconciled to its own target.
func TestReconcile_MultiRolePools(t *testing.T) {
	// "thread" pool already has 1 of 1 (at target → no-op). "test-role" pool
	// has 0 of 2 (below target → create 2).
	mock := &reconcileMock{t: t, agents: []map[string]any{
		{
			"id":     "gd-thread-0",
			"title":  "thread-0",
			"type":   "agent",
			"status": "open",
			"fields": map[string]string{
				"agent":       "thread-0",
				"agent_state": "prewarmed",
				"project":     "gardenio",
				"role":        "thread",
			},
		},
	}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true, MinSize: 1, MaxSize: 1, Mode: "crew",
			Roles: []beadsapi.PrewarmedRolePool{
				{Role: "thread", MinSize: 1, MaxSize: 1},
				{Role: "test-role", MinSize: 2, MaxSize: 2},
			},
		}},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	// thread pool at target (no create), test-role pool needs 2.
	if got := mock.created.Load(); got != 2 {
		t.Errorf("expected 2 creates for the test-role pool, got %d", got)
	}
	// Nothing should be closed — both pools are at or below target.
	if got := mock.closed.Load(); got != 0 {
		t.Errorf("expected no closes, got %d", got)
	}
}

// TestReconcile_ClosesAgentsForRemovedRole verifies that a prewarmed agent
// whose role is no longer in the project's pool config gets closed (the
// (project,role) pool was removed), even though the project itself still has
// an enabled pool for other roles.
func TestReconcile_ClosesAgentsForRemovedRole(t *testing.T) {
	mock := &reconcileMock{t: t, agents: []map[string]any{
		{
			"id":     "gd-orphan-role",
			"title":  "orphan",
			"type":   "agent",
			"status": "open",
			"fields": map[string]string{
				"agent":       "orphan",
				"agent_state": "prewarmed",
				"project":     "gardenio",
				"role":        "retired-role",
			},
		},
	}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true, MinSize: 0, MaxSize: 0, Role: "thread", Mode: "crew",
		}},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.closed.Load(); got != 1 {
		t.Errorf("expected 1 close for the retired-role orphan, got %d", got)
	}
}

// Regression for the pool-leak (ka-0000007786): parked pool members — ones the
// pool stranded in agent_state=queued (capacity-deferred) or idle
// (started-but-unassigned) — MUST count toward the pool target. Before the fix,
// listPrewarmedAgents only saw state==prewarmed, so a member that flipped to
// queued/idle vanished from the count and Reconcile minted a replacement, which
// also stranded, piling up 180+ zombie shells. Here: target=3, with 1 prewarmed
// + 1 queued + 1 idle parked member already present → the pool is AT target and
// must mint NOTHING.
func TestReconcile_CountsParkedMembersTowardTarget(t *testing.T) {
	mkAgent := func(id, state string) map[string]any {
		return map[string]any{
			"id":     id,
			"title":  "prewarmed-" + id,
			"type":   "agent",
			"status": "open",
			"fields": map[string]string{
				"agent":       "prewarmed-" + id, // pool minting convention
				"agent_state": state,
				"project":     "gardenio",
				"role":        "thread",
			},
		}
	}
	mock := &reconcileMock{t: t, agents: []map[string]any{
		mkAgent("p0", "prewarmed"),
		mkAgent("q1", "queued"),
		mkAgent("i2", "idle"),
	}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true, MinSize: 3, MaxSize: 3, Role: "thread", Mode: "crew",
		}},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.created.Load(); got != 0 {
		t.Errorf("parked members must count toward target: expected 0 new creations, got %d", got)
	}
	if got := mock.closed.Load(); got != 0 {
		t.Errorf("at target: expected 0 closes, got %d", got)
	}
}

// An ASSIGNED member that was then capacity-deferred (carries a thread/task)
// must NOT be treated as an idle parked member — it is real work. With min=1
// and one prewarmed member present plus an assigned-queued member, the pool is
// already at target on the prewarmed alone and must not close the assigned one.
func TestReconcile_AssignedQueuedNotCountedAsParked(t *testing.T) {
	mock := &reconcileMock{t: t, agents: []map[string]any{
		{
			"id": "p0", "title": "prewarmed-p0", "type": "agent", "status": "open",
			"fields": map[string]string{
				"agent": "prewarmed-p0", "agent_state": "prewarmed",
				"project": "gardenio", "role": "thread",
			},
		},
		{
			"id": "assigned", "title": "prewarmed-assigned", "type": "agent", "status": "open",
			"fields": map[string]string{
				"agent": "prewarmed-assigned", "agent_state": "queued",
				"project": "gardenio", "role": "thread",
				"slack_thread_ts": "1781550000.1", // assigned off the pool
			},
		},
	}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true, MinSize: 1, MaxSize: 1, Role: "thread", Mode: "crew",
		}},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	// The assigned member is invisible to the pool (not a parked surplus), so it
	// is neither counted nor closed; the single prewarmed satisfies target=1.
	if got := mock.closed.Load(); got != 0 {
		t.Errorf("assigned member must not be closed as surplus, got %d closes", got)
	}
	if got := mock.created.Load(); got != 0 {
		t.Errorf("expected 0 creations (prewarmed satisfies target), got %d", got)
	}
}

func TestIsParkedPoolMember(t *testing.T) {
	cases := []struct {
		name  string
		bead  beadsapi.AgentBead
		want  bool
	}{
		{"queued pool shell", beadsapi.AgentBead{AgentName: "prewarmed-1-2", AgentState: "queued"}, true},
		{"idle pool shell", beadsapi.AgentBead{AgentName: "prewarmed-1-2", AgentState: "idle"}, true},
		{"prewarmed (not parked)", beadsapi.AgentBead{AgentName: "prewarmed-1-2", AgentState: "prewarmed"}, false},
		{"working (not parked)", beadsapi.AgentBead{AgentName: "prewarmed-1-2", AgentState: "working"}, false},
		{"non-pool name", beadsapi.AgentBead{AgentName: "crew-k8s", AgentState: "queued"}, false},
		{"assigned thread", beadsapi.AgentBead{AgentName: "prewarmed-1-2", AgentState: "queued", Metadata: map[string]string{"slack_thread_ts": "1.1"}}, false},
		{"assigned task", beadsapi.AgentBead{AgentName: "prewarmed-1-2", AgentState: "idle", Metadata: map[string]string{"task_id": "gd-x"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isParkedPoolMember(tc.bead); got != tc.want {
				t.Errorf("isParkedPoolMember(%+v) = %v, want %v", tc.bead, got, tc.want)
			}
		})
	}
}
