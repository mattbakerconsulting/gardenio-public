package poolmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// TestPoolSystemOperatorIsValidLabel pins the X-System-Operator source label
// the pool manager stamps on its machine-origin writes to the daemon's
// sanitizeSystemOperatorHeader contract (ASCII lowercase letters, digits, or
// single internal dashes; length 1..64; no leading/trailing/double dash). If
// this drifts to an invalid shape the daemon silently drops the override and
// the write fail-closes again with HTTP 400 (ka-0000006169).
func TestPoolSystemOperatorIsValidLabel(t *testing.T) {
	s := poolSystemOperator
	if s == "" || len(s) > 64 {
		t.Fatalf("poolSystemOperator %q has invalid length %d", s, len(s))
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		t.Fatalf("poolSystemOperator %q must not start or end with '-'", s)
	}
	prevDash := false
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			prevDash = false
		case r == '-':
			if prevDash {
				t.Fatalf("poolSystemOperator %q has a double dash at index %d", s, i)
			}
			prevDash = true
		default:
			t.Fatalf("poolSystemOperator %q has invalid char %q at index %d", s, r, i)
		}
	}
}

// sysOpCapturingDaemon records the X-System-Operator header seen on the
// FIRST mutating request (POST create or PATCH update) it observes, so a test
// can assert the controller attributed its machine-origin write to the
// system:<source> sink. GET /v1/beads returns the supplied agents.
func sysOpCapturingDaemon(t *testing.T, agents []map[string]any, gotHeader *string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	record := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if *gotHeader == "" {
			// Use a sentinel so "header absent" is distinguishable from
			// "header present but empty" — a real refusal cause.
			if v := r.Header.Get("X-System-Operator"); v != "" {
				*gotHeader = v
			} else {
				*gotHeader = "<absent>"
			}
		}
	}
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
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			record(r)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-new-prewarm"})
		case r.Method == http.MethodPatch, r.Method == http.MethodPost:
			record(r)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

// TestReconcile_StampsSystemOperatorOnCreate is the regression guard for
// ka-0000006169: a pool below target mints prewarmed agents, and that
// CreateBead must carry X-System-Operator: pool-manager so the daemon resolves
// the system:pool-manager machine sink instead of fail-closing the header-less
// static-token write with HTTP 400 "operator could not be resolved".
func TestReconcile_StampsSystemOperatorOnCreate(t *testing.T) {
	var gotHeader string
	var mu sync.Mutex
	// Empty pool with min_size=2 (from testConfig) forces a mint on reconcile.
	srv := sysOpCapturingDaemon(t, nil, &gotHeader, &mu)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotHeader == "" {
		t.Fatal("no mutating request captured — Reconcile did not write to the daemon")
	}
	if gotHeader != poolSystemOperator {
		t.Errorf("X-System-Operator = %q, want %q", gotHeader, poolSystemOperator)
	}
}

// TestAssignPrewarmed_StampsSystemOperator verifies the assign PATCH carries
// the machine-operator header too, so the controller-performed assign write is
// not fail-closed in a non-fleet-scoped (ephemeral) environment.
func TestAssignPrewarmed_StampsSystemOperator(t *testing.T) {
	var gotHeader string
	var mu sync.Mutex
	agents := []map[string]any{
		{
			"id":    "gd-prewarm-sysop",
			"title": "prewarmed-sysop",
			"fields": map[string]string{
				"agent": "prewarmed-sysop", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	srv := sysOpCapturingDaemon(t, agents, &gotHeader, &mu)
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
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotHeader == "" {
		t.Fatal("no mutating request captured — AssignPrewarmed did not write to the daemon")
	}
	if gotHeader != poolSystemOperator {
		t.Errorf("X-System-Operator = %q, want %q", gotHeader, poolSystemOperator)
	}
}
