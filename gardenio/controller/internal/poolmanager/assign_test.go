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

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"

	"log/slog"
)

// mockDaemon creates a test HTTP server that serves bead list/get/update
// requests for pool assign testing.
func mockDaemon(t *testing.T, agents []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// Return the mock agents list.
			beads := make([]map[string]any, len(agents))
			for i, a := range agents {
				beads[i] = map[string]any{
					"id":     a["id"],
					"title":  a["title"],
					"type":   "agent",
					"status": "open",
					"fields": a["fields"],
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": beads,
				"total": len(beads),
			})

		case r.Method == http.MethodGet:
			// GetBead: return the first matching agent.
			for _, a := range agents {
				if "/v1/beads/"+a["id"].(string) == r.URL.Path {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id":     a["id"],
						"title":  a["title"],
						"type":   "agent",
						"status": "open",
						"fields": a["fields"],
					})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})

		case r.Method == http.MethodPatch:
			// UpdateBeadFields: just acknowledge.
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// recordingMockDaemon is mockDaemon plus a recorder of every (method, path) the
// pool manager issues, so a test can prove AssignPrewarmed reuses the SAME pool
// bead (only reads + a field PATCH) and never CREATES a bead — a bead creation is
// what would emit a fresh spawn event and re-mint the per-run identity. The
// returned slice is appended under lock; read it only after the call returns.
func recordingMockDaemon(t *testing.T, agents []map[string]any, calls *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			beads := make([]map[string]any, len(agents))
			for i, a := range agents {
				beads[i] = map[string]any{
					"id": a["id"], "title": a["title"], "type": "agent",
					"status": "open", "fields": a["fields"],
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
		case r.Method == http.MethodGet:
			for _, a := range agents {
				if "/v1/beads/"+a["id"].(string) == r.URL.Path {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": a["id"], "title": a["title"], "type": "agent",
						"status": "open", "fields": a["fields"],
					})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		case r.Method == http.MethodPatch, r.Method == http.MethodPut:
			// UpdateBeadFields / UpdateBead: acknowledge.
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestAssignPrewarmed_ReusesSameBeadNoReMint proves the warm-pool identity
// invariant at the assignment layer (ka-0000006503): AssignPrewarmed flips the
// SAME already-launched pool bead and issues ZERO bead creations, so no fresh
// spawn event fires and the per-run Ziti lease + Secrets Manager secrets minted
// for that bead ID stay bound — nothing re-Launches or re-mints. It asserts the
// only mutating call is a field PATCH targeting the picked bead, and that no
// POST /v1/beads (create → new spawn → new identity) is ever issued.
func TestAssignPrewarmed_ReusesSameBeadNoReMint(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-warm-1",
			"title": "prewarmed-1",
			"fields": map[string]string{
				"agent":            "prewarmed-1",
				"agent_state":      "prewarmed",
				"mode":             "crew",
				"role":             "thread",
				"project":          "gardenio",
				"execution_target": "lambda_microvm",
				// Already launched: its identity is minted+bound to this bead ID.
				"runtime_id": "mvm-warm-1",
			},
		},
	}
	var calls []string
	var mu sync.Mutex
	srv := recordingMockDaemon(t, agents, &calls, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:     "C-test",
		ThreadTS:    "1234.5678",
		Description: "assigned work",
		Project:     "gardenio",
		Role:        "thread",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed: %v", err)
	}
	// The SAME pool bead is assigned — not a freshly created one.
	if result.BeadID != "gd-warm-1" {
		t.Fatalf("assigned bead = %q, want the existing pool bead gd-warm-1", result.BeadID)
	}

	mu.Lock()
	defer mu.Unlock()
	// ZERO bead creations: a POST /v1/beads is what would spawn a new agent and
	// re-mint an identity. Assignment must never create a bead.
	for _, c := range calls {
		if c == "POST /v1/beads" {
			t.Errorf("AssignPrewarmed issued a bead creation (%q) — assignment must reuse the same bead, never spawn/re-mint; calls=%v", c, calls)
		}
	}
	// Every mutating write targets the picked bead (the field flip + description),
	// never any other bead id.
	for _, c := range calls {
		if strings.HasPrefix(c, "PATCH ") || strings.HasPrefix(c, "PUT ") {
			if !strings.HasSuffix(c, "/v1/beads/gd-warm-1") {
				t.Errorf("mutating call %q does not target the picked bead gd-warm-1; calls=%v", c, calls)
			}
		}
	}
	// And a field flip did happen on that bead (the prewarmed→assigning PATCH).
	sawFlip := false
	for _, c := range calls {
		if c == "PATCH /v1/beads/gd-warm-1" {
			sawFlip = true
		}
	}
	if !sawFlip {
		t.Errorf("expected a field PATCH on gd-warm-1 (agent_state flip); calls=%v", calls)
	}
}

// testConfig returns a Config with the project cache pre-populated for testing.
func testConfig() *config.Config {
	cfg := &config.Config{
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
	return cfg
}

func TestAssignPrewarmed_Success(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-1",
			"title": "prewarmed-1",
			"fields": map[string]string{
				"agent":       "prewarmed-1",
				"agent_state": "prewarmed",
				"mode":        "crew",
				"role":        "thread",
				"project":     "gardenio",
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:     "C-test",
		ThreadTS:    "1234.5678",
		Description: "test thread",
		Project:     "gardenio",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-prewarm-1" {
		t.Errorf("expected bead ID gd-prewarm-1, got %s", result.BeadID)
	}
	if result.AgentName != "prewarmed-1" {
		t.Errorf("expected agent name prewarmed-1, got %s", result.AgentName)
	}
}

func TestAssignPrewarmed_EmptyPool(t *testing.T) {
	srv := mockDaemon(t, nil)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	pm := New(daemon, testConfig(), slog.Default())

	_, err = pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
	})
	if err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty, got: %v", err)
	}
}

func TestAssignPrewarmed_PicksOldest(t *testing.T) {
	now := time.Now()
	agents := []map[string]any{
		{
			"id":    "gd-new",
			"title": "prewarmed-new",
			"fields": map[string]string{
				"agent":       "prewarmed-new",
				"agent_state": "prewarmed",
				"mode":        "crew",
				"role":        "thread",
				"project":     "gardenio",
				"created_at":  now.Add(-5 * time.Minute).Format(time.RFC3339),
			},
		},
		{
			"id":    "gd-old",
			"title": "prewarmed-old",
			"fields": map[string]string{
				"agent":       "prewarmed-old",
				"agent_state": "prewarmed",
				"mode":        "crew",
				"role":        "thread",
				"project":     "gardenio",
				"created_at":  now.Add(-20 * time.Minute).Format(time.RFC3339),
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-old" {
		t.Errorf("expected oldest agent gd-old, got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_MicrovmPicksFreshest asserts the Phase-4 lifetime/cost
// ordering (ka-0000003944): a lambda_microvm pool hands out the FRESHEST member
// (newest created_at) so the caller gets the most remaining
// MaximumDurationInSeconds run budget and credential headroom — the opposite of
// the K8s FIFO-oldest pick. Same two-agent fixture as TestAssignPrewarmed_PicksOldest,
// but with execution_target=lambda_microvm stamped, so the pick flips to gd-new.
func TestAssignPrewarmed_MicrovmPicksFreshest(t *testing.T) {
	now := time.Now()
	agents := []map[string]any{
		{
			"id":    "gd-new",
			"title": "prewarmed-new",
			"fields": map[string]string{
				"agent":            "prewarmed-new",
				"agent_state":      "prewarmed",
				"mode":             "crew",
				"role":             "thread",
				"project":          "gardenio",
				"execution_target": "lambda_microvm",
				"created_at":       now.Add(-5 * time.Minute).Format(time.RFC3339),
			},
		},
		{
			"id":    "gd-old",
			"title": "prewarmed-old",
			"fields": map[string]string{
				"agent":            "prewarmed-old",
				"agent_state":      "prewarmed",
				"mode":             "crew",
				"role":             "thread",
				"project":          "gardenio",
				"execution_target": "lambda_microvm",
				"created_at":       now.Add(-20 * time.Minute).Format(time.RFC3339),
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-new" {
		t.Errorf("MicroVM pool expected freshest agent gd-new, got %s", result.BeadID)
	}
}

func TestReconcile_CreatesAgentsForProject(t *testing.T) {
	var created int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// Return empty list (no prewarmed agents yet).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []any{},
				"total": 0,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			created++
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": "gd-new-" + string(rune('0'+created)),
			})
		case r.Method == http.MethodPost:
			// AddLabel: just acknowledge.
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	// Should create 2 agents (min_size=2, pool was empty).
	if created != 2 {
		t.Errorf("expected 2 agents created, got %d", created)
	}
}

func TestReconcile_NoPoolConfig(t *testing.T) {
	srv := mockDaemon(t, nil)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	// Empty project cache = no pools configured.
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	// Should be a no-op — no API calls expected.
}

// TestAssignPrewarmed_RoleFilter verifies the fast path honors the channel
// role override: with two prewarmed roles in the same project, a request for a
// specific role picks an agent of that role (gap G2).
func TestAssignPrewarmed_RoleFilter(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-thread",
			"title": "prewarmed-thread",
			"fields": map[string]string{
				"agent":       "prewarmed-thread",
				"agent_state": "prewarmed",
				"mode":        "crew",
				"role":        "thread",
				"project":     "gardenio",
			},
		},
		{
			"id":    "gd-testrole",
			"title": "prewarmed-testrole",
			"fields": map[string]string{
				"agent":       "prewarmed-testrole",
				"agent_state": "prewarmed",
				"mode":        "crew",
				"role":        "test-role",
				"project":     "gardenio",
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "gardenio",
		Role:    "test-role",
		Channel: "C-test",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-testrole" {
		t.Errorf("expected role-matched agent gd-testrole, got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_RoleNoMatch verifies that requesting a role with no
// prewarmed agents returns ErrPoolEmpty even when the project has agents of a
// different role (so the caller falls back to cold start).
func TestAssignPrewarmed_RoleNoMatch(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-thread",
			"title": "prewarmed-thread",
			"fields": map[string]string{
				"agent":       "prewarmed-thread",
				"agent_state": "prewarmed",
				"mode":        "crew",
				"role":        "thread",
				"project":     "gardenio",
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	_, err = pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "gardenio",
		Role:    "missing-role",
	})
	if err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty for unmatched role, got: %v", err)
	}
}

// TestAssignPrewarmed_EmptyRoleMatchesAny verifies legacy behavior: an empty
// role in the request matches any role in the project.
func TestAssignPrewarmed_EmptyRoleMatchesAny(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-testrole",
			"title": "prewarmed-testrole",
			"fields": map[string]string{
				"agent":       "prewarmed-testrole",
				"agent_state": "prewarmed",
				"mode":        "crew",
				"role":        "test-role",
				"project":     "gardenio",
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "gardenio",
		// Role intentionally empty.
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-testrole" {
		t.Errorf("expected any-role match gd-testrole, got %s", result.BeadID)
	}
}

// runtimeGateConfig returns a Config whose role cache pins the "gvisor-poc" role
// to runsc and leaves "thread" on the cluster default (runc), so the runtime
// gate in AssignPrewarmed can be exercised (ka-0000004563).
func runtimeGateConfig() *config.Config {
	cfg := testConfig()
	cfg.RoleCache = map[string]config.RoleCacheEntry{
		"gvisor-poc": {RuntimeClassName: "runsc"},
		"thread":     {}, // no runtime_class → cluster default (runc)
	}
	return cfg
}

func runtimeGateAgents() []map[string]any {
	return []map[string]any{
		{
			"id":    "gd-runc",
			"title": "prewarmed-runc",
			"fields": map[string]string{
				"agent": "prewarmed-runc", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
		{
			"id":    "gd-runsc",
			"title": "prewarmed-runsc",
			"fields": map[string]string{
				"agent": "prewarmed-runsc", "agent_state": "prewarmed",
				"mode": "crew", "role": "gvisor-poc", "project": "gardenio",
			},
		},
	}
}

// TestAssignPrewarmed_RequireRuntimeClassMatches verifies the runsc runtime gate
// (ka-0000004563): a request that requires runsc claims ONLY the agent whose role
// resolves to runsc — never the runc one — so a runsc-pinned channel warm-assigns
// a freeze-eligible pod instead of cold-minting.
func TestAssignPrewarmed_RequireRuntimeClassMatches(t *testing.T) {
	srv := mockDaemon(t, runtimeGateAgents())
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, runtimeGateConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project:             "gardenio",
		RequireRuntimeClass: "runsc",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-runsc" {
		t.Errorf("expected runsc-role agent gd-runsc, got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_RequireRuntimeClassNoMatch verifies that requiring runsc
// when the pool holds only runc agents returns ErrPoolEmpty, so the caller falls
// through to a cold mint rather than grabbing a runtime-incompatible pod.
func TestAssignPrewarmed_RequireRuntimeClassNoMatch(t *testing.T) {
	agents := []map[string]any{runtimeGateAgents()[0]} // only the runc agent
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, runtimeGateConfig(), slog.Default())

	_, err = pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project:             "gardenio",
		RequireRuntimeClass: "runsc",
	})
	if err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty when no runsc agent exists, got: %v", err)
	}
}

// TestAssignPrewarmed_EmptyRuntimeClassMatchesAny verifies legacy behavior: a
// request with no RequireRuntimeClass (a default runc channel) matches any pool
// agent regardless of its resolved runtime.
func TestAssignPrewarmed_EmptyRuntimeClassMatchesAny(t *testing.T) {
	// Pool holds only the runsc agent; an unconstrained request still claims it.
	agents := []map[string]any{runtimeGateAgents()[1]}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, runtimeGateConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "gardenio",
		// RequireRuntimeClass intentionally empty.
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-runsc" {
		t.Errorf("expected unconstrained match gd-runsc, got %s", result.BeadID)
	}
}

// capturingDaemon is a mockDaemon variant that records the merged fields blob
// sent on the PATCH from UpdateBeadFields, so tests can assert exactly which
// fields AssignPrewarmed stamps on the picked agent bead. The captured map is
// written to *captured under mu.
func capturingDaemon(t *testing.T, agents []map[string]any, captured *map[string]any, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			beads := make([]map[string]any, len(agents))
			for i, a := range agents {
				beads[i] = map[string]any{
					"id": a["id"], "title": a["title"], "type": "agent",
					"status": "open", "fields": a["fields"],
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
		case r.Method == http.MethodGet:
			for _, a := range agents {
				if "/v1/beads/"+a["id"].(string) == r.URL.Path {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": a["id"], "title": a["title"], "type": "agent",
						"status": "open", "fields": a["fields"],
					})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		case r.Method == http.MethodPatch:
			// AssignPrewarmed issues two writes: the fields PATCH we care about,
			// then a description-only PATCH (no "fields" key). Only record the
			// former so the description write doesn't clobber the capture.
			var body struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Fields) > 0 {
				var f map[string]any
				if json.Unmarshal(body.Fields, &f) == nil && len(f) > 0 {
					mu.Lock()
					*captured = f
					mu.Unlock()
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestAssignPrewarmed_StampsOperatedBy verifies the defense-in-depth fix
// (ka-0000000528): when AssignRequest carries a human operator, AssignPrewarmed
// stamps fields.operated_by in the SAME atomic update that flips agent_state,
// so a warm pod's first attributed kd/gb write resolves the human via the
// reader-fallback instead of 400ing "operator could not be resolved".
func TestAssignPrewarmed_StampsOperatedBy(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-op",
			"title": "prewarmed-op",
			"fields": map[string]string{
				"agent": "prewarmed-op", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	var captured map[string]any
	var mu sync.Mutex
	srv := capturingDaemon(t, agents, &captured, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	const operator = "oidc:matthew.baker@acme.example"
	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:     "C-test",
		ThreadTS:    "1234.5678",
		Description: "test thread",
		Project:     "gardenio",
		OperatedBy:  operator,
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("no PATCH captured — AssignPrewarmed did not update the bead")
	}
	if got, _ := captured["operated_by"].(string); got != operator {
		t.Errorf("operated_by = %q, want %q", got, operator)
	}
	// The atomic update must also still flip agent_state to assigning.
	if got, _ := captured["agent_state"].(string); got != "assigning" {
		t.Errorf("agent_state = %q, want assigning", got)
	}
}

// TestAssignPrewarmed_EmptyOperatedByNotStamped verifies that an empty operator
// is NOT written: the bridge's separate post-assign UpdateBeadFields still sets
// it, and the reader-fallback only accepts well-formed human shapes, so stamping
// "" would only add a no-op/empty field. The state flip must still happen.
func TestAssignPrewarmed_EmptyOperatedByNotStamped(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-noop",
			"title": "prewarmed-noop",
			"fields": map[string]string{
				"agent": "prewarmed-noop", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	var captured map[string]any
	var mu sync.Mutex
	srv := capturingDaemon(t, agents, &captured, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
		Project:  "gardenio",
		// OperatedBy intentionally empty.
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("no PATCH captured — AssignPrewarmed did not update the bead")
	}
	if _, ok := captured["operated_by"]; ok {
		t.Errorf("operated_by should not be stamped when empty, got %v", captured["operated_by"])
	}
	if got, _ := captured["agent_state"].(string); got != "assigning" {
		t.Errorf("agent_state = %q, want assigning", got)
	}
}

// TestAssignPrewarmed_StampsThreadID verifies the race-close fix (ka-0000001160):
// when AssignRequest carries a thread (wish) head id, AssignPrewarmed stamps
// fields.thread_id AND its legacy alias slack_thread_bead_id in the SAME atomic
// update that flips agent_state — so a warm pod carries the thread pointer the
// instant it leaves the pool, and the daemon's transitive thread_id copy stamps
// the pod's first `kd create` instead of no-oping on an empty pointer.
func TestAssignPrewarmed_StampsThreadID(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-thread",
			"title": "prewarmed-thread",
			"fields": map[string]string{
				"agent": "prewarmed-thread", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	var captured map[string]any
	var mu sync.Mutex
	srv := capturingDaemon(t, agents, &captured, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	const head = "gd-61pi6kb3sx"
	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
		Project:  "gardenio",
		ThreadID: head,
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("no PATCH captured — AssignPrewarmed did not update the bead")
	}
	if got, _ := captured["thread_id"].(string); got != head {
		t.Errorf("thread_id = %q, want %q", got, head)
	}
	if got, _ := captured["slack_thread_bead_id"].(string); got != head {
		t.Errorf("slack_thread_bead_id = %q, want %q", got, head)
	}
	// The atomic update must also still flip agent_state to assigning.
	if got, _ := captured["agent_state"].(string); got != "assigning" {
		t.Errorf("agent_state = %q, want assigning", got)
	}
}

// TestAssignPrewarmed_EmptyThreadIDNotStamped verifies that an empty thread id
// is NOT written: the bridge's separate post-assign UpdateBeadFields still sets
// it, so stamping "" would only add a no-op empty field (and could mask the
// head-safe copy-if-empty semantics downstream). The state flip must still fire.
func TestAssignPrewarmed_EmptyThreadIDNotStamped(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-nothread",
			"title": "prewarmed-nothread",
			"fields": map[string]string{
				"agent": "prewarmed-nothread", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	var captured map[string]any
	var mu sync.Mutex
	srv := capturingDaemon(t, agents, &captured, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
		Project:  "gardenio",
		// ThreadID intentionally empty.
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("no PATCH captured — AssignPrewarmed did not update the bead")
	}
	if _, ok := captured["thread_id"]; ok {
		t.Errorf("thread_id should not be stamped when empty, got %v", captured["thread_id"])
	}
	if _, ok := captured["slack_thread_bead_id"]; ok {
		t.Errorf("slack_thread_bead_id should not be stamped when empty, got %v", captured["slack_thread_bead_id"])
	}
	if got, _ := captured["agent_state"].(string); got != "assigning" {
		t.Errorf("agent_state = %q, want assigning", got)
	}
}

// TestAssignPrewarmed_StampsModel verifies the pool-assign model-awareness
// protocol (design D1, ka-0000000406 / ka-0000006250): when AssignRequest carries
// a model, AssignPrewarmed stamps fields.model in the SAME atomic update that
// flips agent_state, so podspec.go's applyAgentModelOverride bakes
// ANTHROPIC_MODEL/CLAUDE_MODEL from the assigned pod's own bead. Carried by the
// `gb pool assign --model` CLI and future model-partitioned pools; the Slack
// @mention path bypasses the pool when a model is pinned and never exercises the
// HIT case (a prewarmed pod cannot be re-modeled).
func TestAssignPrewarmed_StampsModel(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-model",
			"title": "prewarmed-model",
			"fields": map[string]string{
				"agent": "prewarmed-model", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	var captured map[string]any
	var mu sync.Mutex
	srv := capturingDaemon(t, agents, &captured, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	const model = "claude-opus-4-8"
	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:     "C-test",
		ThreadTS:    "1234.5678",
		Description: "test thread",
		Project:     "gardenio",
		Model:       model,
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("no PATCH captured — AssignPrewarmed did not update the bead")
	}
	if got, _ := captured["model"].(string); got != model {
		t.Errorf("model = %q, want %q", got, model)
	}
	// The atomic update must also still flip agent_state to assigning.
	if got, _ := captured["agent_state"].(string); got != "assigning" {
		t.Errorf("agent_state = %q, want assigning", got)
	}
}

// TestAssignPrewarmed_EmptyModelNotStamped verifies that an empty model is NOT
// written: the pod keeps the account/role default baked at prewarm time, so
// stamping "" would only add a no-op/empty field. The state flip must still
// happen.
func TestAssignPrewarmed_EmptyModelNotStamped(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-nomodel",
			"title": "prewarmed-nomodel",
			"fields": map[string]string{
				"agent": "prewarmed-nomodel", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	var captured map[string]any
	var mu sync.Mutex
	srv := capturingDaemon(t, agents, &captured, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
		Project:  "gardenio",
		// Model intentionally empty.
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("no PATCH captured — AssignPrewarmed did not update the bead")
	}
	if _, ok := captured["model"]; ok {
		t.Errorf("model should not be stamped when empty, got %v", captured["model"])
	}
	if got, _ := captured["agent_state"].(string); got != "assigning" {
		t.Errorf("agent_state = %q, want assigning", got)
	}
}

// fakeAssignRecorder captures RecordPrewarmedAssign +
// RecordPrewarmedAssignDuration calls (ka-0000000550, ka-0000001143).
type fakeAssignRecorder struct {
	mu            sync.Mutex
	calls         []string // "outcome/project/role"
	durationCalls []string // "outcome/project/role/executionTarget"
}

func (f *fakeAssignRecorder) RecordPrewarmedAssign(outcome, project, role string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, outcome+"/"+project+"/"+role)
	return true
}

func (f *fakeAssignRecorder) RecordPrewarmedAssignDuration(_ float64, outcome, project, role, executionTarget string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.durationCalls = append(f.durationCalls, outcome+"/"+project+"/"+role+"/"+executionTarget)
	return true
}

func (f *fakeAssignRecorder) durationSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.durationCalls...)
}

func (f *fakeAssignRecorder) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// TestAssignPrewarmed_RecordsHit: a successful assign records exactly one hit,
// keyed by the PICKED agent's (project,role) — not the request's, whose role
// may be empty on the legacy any-role path.
func TestAssignPrewarmed_RecordsHit(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-1",
			"title": "prewarmed-1",
			"fields": map[string]string{
				"agent":       "prewarmed-1",
				"agent_state": "prewarmed",
				"mode":        "crew",
				"role":        "thread",
				"project":     "gardenio",
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())
	rec := &fakeAssignRecorder{}
	pm.SetAssignRecorder(rec)

	// Role left empty (legacy any-role request) — the hit must still be keyed
	// by the picked agent's role.
	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel: "C-test", ThreadTS: "1.2", Project: "gardenio",
	}); err != nil {
		t.Fatalf("AssignPrewarmed: %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0] != "hit/gardenio/thread" {
		t.Errorf("calls = %v, want [hit/gardenio/thread]", calls)
	}
}

// TestAssignPrewarmed_RecordsEmpty: ErrPoolEmpty records one "empty" outcome
// keyed by the requested pool.
func TestAssignPrewarmed_RecordsEmpty(t *testing.T) {
	srv := mockDaemon(t, nil)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())
	rec := &fakeAssignRecorder{}
	pm.SetAssignRecorder(rec)

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel: "C-test", ThreadTS: "1.2", Project: "gardenio", Role: "thread",
	}); err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty, got %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0] != "empty/gardenio/thread" {
		t.Errorf("calls = %v, want [empty/gardenio/thread]", calls)
	}
}

// TestAssignPrewarmed_NoRecorderIsSafe: assigns work unchanged with no
// recorder wired (the default for tests and older deployments).
func TestAssignPrewarmed_NoRecorderIsSafe(t *testing.T) {
	srv := mockDaemon(t, nil)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())
	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "gardenio",
	}); err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty, got %v", err)
	}
}

// TestAssignPrewarmed_RecordsDuration: a hit records one warm-assign latency
// sample alongside the counter, keyed the same way plus execution_target, and
// an empty pool records an empty-outcome sample. A default (K8s) project
// resolves execution_target=kubernetes.
func TestAssignPrewarmed_RecordsDuration(t *testing.T) {
	// Hit path: a member is present and gets assigned.
	t.Run("hit", func(t *testing.T) {
		agents := []map[string]any{
			{
				"id":    "gd-prewarm-1",
				"title": "prewarmed-1",
				"fields": map[string]string{
					"agent":       "prewarmed-1",
					"agent_state": "prewarmed",
					"mode":        "crew",
					"role":        "thread",
					"project":     "gardenio",
				},
			},
		}
		srv := mockDaemon(t, agents)
		defer srv.Close()
		daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		pm := New(daemon, testConfig(), slog.Default())
		rec := &fakeAssignRecorder{}
		pm.SetAssignRecorder(rec)

		if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
			Channel: "C-test", ThreadTS: "1.2", Project: "gardenio",
		}); err != nil {
			t.Fatalf("AssignPrewarmed hit: %v", err)
		}
		got := rec.durationSnapshot()
		if len(got) != 1 || got[0] != "hit/gardenio/thread/kubernetes" {
			t.Errorf("durationCalls = %v, want [hit/gardenio/thread/kubernetes]", got)
		}
	})

	// Empty path: the pool has no members; the empty outcome is still timed.
	t.Run("empty", func(t *testing.T) {
		srv := mockDaemon(t, nil)
		defer srv.Close()
		daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		pm := New(daemon, testConfig(), slog.Default())
		rec := &fakeAssignRecorder{}
		pm.SetAssignRecorder(rec)

		if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
			Channel: "C-test", ThreadTS: "1.3", Project: "gardenio", Role: "thread",
		}); err != ErrPoolEmpty {
			t.Fatalf("AssignPrewarmed empty: expected ErrPoolEmpty, got %v", err)
		}
		got := rec.durationSnapshot()
		if len(got) != 1 || got[0] != "empty/gardenio/thread/kubernetes" {
			t.Errorf("durationCalls = %v, want [empty/gardenio/thread/kubernetes]", got)
		}
	})
}

// TestAssignPrewarmed_DurationTagsMicrovmTarget: a lambda_microvm project stamps
// execution_target=lambda_microvm on the warm-assign latency series, so the
// MicroVM warm pool's assign is a distinct histogram series from the K8s pool.
func TestAssignPrewarmed_DurationTagsMicrovmTarget(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-mvm",
			"title": "prewarmed-mvm",
			"fields": map[string]string{
				"agent":            "prewarmed-mvm",
				"agent_state":      "prewarmed",
				"mode":             "crew",
				"role":             "thread",
				"project":          "gardenio",
				"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	// Mark the gardenio project as a MicroVM backend so effectiveExecutionTarget
	// resolves lambda_microvm for its pool.
	entry := cfg.ProjectCache["gardenio"]
	entry.ExecutionTarget = podmanager.ExecutionTargetLambdaMicroVM
	cfg.ProjectCache["gardenio"] = entry

	pm := New(daemon, cfg, slog.Default())
	rec := &fakeAssignRecorder{}
	pm.SetAssignRecorder(rec)

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel: "C-test", ThreadTS: "1.2", Project: "gardenio",
	}); err != nil {
		t.Fatalf("AssignPrewarmed: %v", err)
	}
	got := rec.durationSnapshot()
	if len(got) != 1 || got[0] != "hit/gardenio/thread/lambda_microvm" {
		t.Errorf("durationCalls = %v, want [hit/gardenio/thread/lambda_microvm]", got)
	}
}
