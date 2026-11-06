package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestSuspendAgent_SetsGateSatisfiedBy is a regression test for gd-p1HiE6EBFu:
// the freeze/suspend path (suspendAgent) must record gate_satisfied_by, not
// just satisfy the gate row. The server-side Stop hook (kbeads http_hooks.go)
// treats a satisfied gate with an empty gate_satisfied_by as an unauthorized
// bypass and re-BLOCKS the stop ("gate was not satisfied via 'gb yield'"),
// looping until the stop-gate.sh escape hatch fires. Before the fix, every
// freeze-eligible crew agent ending a normal turn hit that loop.
//
// We drive suspendAgent against a fake daemon and assert that the PATCH to the
// agent bead carries BOTH agent_state=stopping and gate_satisfied_by=operator.
func TestSuspendAgent_SetsGateSatisfiedBy(t *testing.T) {
	var (
		mu            sync.Mutex
		gateSatisfied bool
		patchedFields = map[string]any{}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost &&
			r.URL.Path == "/v1/agents/gd-suspend-test/gates/decision/satisfy":
			mu.Lock()
			gateSatisfied = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-suspend-test":
			// UpdateBeadFields fetches the bead first to preserve field types.
			// Return an empty fields object so every patched key is treated as
			// a fresh string write.
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"id":     "gd-suspend-test",
				"fields": json.RawMessage(`{}`),
			})

		case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/gd-suspend-test":
			var body struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var f map[string]any
			_ = json.Unmarshal(body.Fields, &f)
			mu.Lock()
			maps.Copy(patchedFields, f)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		default:
			// syncAgentSessions / any other call — succeed quietly.
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	defer func() { daemon = oldDaemon }()

	if err := suspendAgent(context.Background(), "gd-suspend-test"); err != nil {
		t.Fatalf("suspendAgent returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if !gateSatisfied {
		t.Error("suspendAgent did not satisfy the decision gate")
	}
	if got := patchedFields["agent_state"]; got != "stopping" {
		t.Errorf("agent_state = %v, want \"stopping\"", got)
	}
	if got := patchedFields["gate_satisfied_by"]; got != "operator" {
		t.Errorf("gate_satisfied_by = %v, want \"operator\" "+
			"(empty value is exactly the gd-p1HiE6EBFu bug — server re-blocks the stop)", got)
	}
}
