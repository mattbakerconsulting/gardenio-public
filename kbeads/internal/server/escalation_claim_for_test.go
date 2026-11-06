package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Claim on a spawned agent's behalf and the machine-role equivalence
// (gd-1yeWlbJUJi, decision gd-P7dxTFCWmt). The fixture is the executor grant's:
// coordinator:cluster's one principal is machine:gardenio-cluster-coordinator,
// and gd-coord1 is an engine-spawned agent whose role mounts that client.

// engineCtx (spawn_lineage_authz_test.go) is the trigger engine's principal.
const engineClient = "gardenio-trigger-engine"

// newClaimForServer is newExecutorGrantServer plus the escalation-coordinator
// role (machine_client gardenio-cluster-coordinator), the laptop role, and
// gd-coord1, a trigger-spawned agent of the first.
func newClaimForServer(t *testing.T) (*BeadsServer, *mockStore) {
	t.Helper()
	s, ms := newExecutorGrantServer(t)
	seedMachineRole(ms, "gd-rcoord", "escalation-coordinator", clusterClient)
	seedMachineRole(ms, "gd-rlap", "laptop-operator", "gardenio-laptop-operator")
	seedSpawnedAgent(ms, "gd-coord1", "escalation-coordinator", "trigger:gd-trig")
	return s, ms
}

func seedMachineRole(ms *mockStore, id, name, client string) {
	f, _ := json.Marshal(map[string]any{"machine_client": client})
	ms.beads[id] = &model.Bead{ID: id, Kind: model.KindConfig, Type: "role", Title: name, Status: model.StatusOpen, Fields: f}
}

func seedSpawnedAgent(ms *mockStore, id, role, spawnedBy string) {
	seedAgent(ms, id, role, "gardenio")
	f, _ := json.Marshal(map[string]any{"role": role, "project": "gardenio", "spawned_by": spawnedBy})
	ms.beads[id].Fields = f
}

func claimFor(t *testing.T, s *BeadsServer, ctx context.Context, esc, agent string) int {
	t.Helper()
	return callEscalationVerb(t, ctx, s.handleClaimEscalationFor, esc, "claim-for", map[string]any{"agent": agent}).Code
}

func TestClaimEscalationFor_CoordinatorRunHoldsItsStamp(t *testing.T) {
	s, ms := newClaimForServer(t)
	clusterEscalation(ms, "gd-xesc", map[string]any{"claimed_by": nil})

	if code := claimFor(t, s, engineCtx(), "gd-xesc", "gd-coord1"); code != http.StatusOK {
		t.Fatalf("engine claim-for: got %d, want 200", code)
	}
	if got := escField(t, ms, "gd-xesc", "claimed_by"); got != "gd-coord1" {
		t.Fatalf("claimed_by = %v, want the spawned agent gd-coord1", got)
	}
	if code := claimFor(t, s, engineCtx(), "gd-xesc", "gd-coord1"); code != http.StatusOK {
		t.Fatalf("repeat claim-for for the same agent: got %d, want 200", code)
	}
	// The run claims as its role's machine client: it already holds the claim,
	// and the row keeps naming the agent.
	if rec := callEscalationVerb(t, clusterCtx(), s.handleClaimEscalation, "gd-xesc", "claim", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("cluster machine claim over its agent's stamp: got %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-xesc", "claimed_by"); got != "gd-coord1" {
		t.Fatalf("claimed_by = %v after the machine claim, want gd-coord1 kept", got)
	}
	if !ownershipAdmits(t, s, clusterCtx(), "close", "gd-xt1") {
		t.Fatal("executor grant: the agent's claim must admit its machine client")
	}
	if ownershipAdmits(t, s, clusterCtx(), "close", "gd-xt2") {
		t.Fatal("executor grant: gd-xt2 is not an approved target")
	}
	// Identity, not liveness: a finished run's stamp is still the client's.
	ms.beads["gd-coord1"].Status = model.StatusClosed
	if !ownershipAdmits(t, s, clusterCtx(), "close", "gd-xt1") {
		t.Fatal("executor grant: a closed agent's claim must still admit its machine client")
	}
}

func TestClaimEscalationFor_NoEquivalenceForAnotherClient(t *testing.T) {
	s, ms := newClaimForServer(t)
	seedSpawnedAgent(ms, "gd-lap1", "laptop-operator", "trigger:gd-trig")
	clusterEscalation(ms, "gd-xesc", map[string]any{"claimed_by": "gd-lap1"})

	if rec := callEscalationVerb(t, clusterCtx(), s.handleClaimEscalation, "gd-xesc", "claim", map[string]any{}); rec.Code != http.StatusConflict {
		t.Fatalf("claim over an agent of another client: got %d, want 409", rec.Code)
	}
	if ownershipAdmits(t, s, clusterCtx(), "close", "gd-xt1") {
		t.Fatal("executor grant: an agent mounting another client must not admit the cluster coordinator")
	}
	ms.beads["gd-rcoord"].Status = model.StatusClosed
	clusterEscalation(ms, "gd-xesc", map[string]any{"claimed_by": "gd-coord1"})
	if ownershipAdmits(t, s, clusterCtx(), "close", "gd-xt1") {
		t.Fatal("executor grant: a closed role bead mounts nothing")
	}
}

func TestClaimEscalationFor_Refusals(t *testing.T) {
	s, ms := newClaimForServer(t)
	clusterEscalation(ms, "gd-xesc", map[string]any{"claimed_by": nil})
	seedSpawnedAgent(ms, "gd-hand1", "escalation-coordinator", "gd-crew1")
	seedSpawnedAgent(ms, "gd-lap1", "laptop-operator", "trigger:gd-trig")
	seedSpawnedAgent(ms, "gd-coord2", "escalation-coordinator", "trigger:gd-trig")

	for _, tc := range []struct {
		name  string
		ctx   context.Context
		agent string
		want  int
	}{
		{"the cluster coordinator is not the engine", clusterCtx(), "gd-coord1", http.StatusForbidden},
		{"the engine client without trigger lineage", machineCtx(engineClient), "gd-coord1", http.StatusForbidden},
		{"another client with trigger lineage", machineCtx("gardenio-other", "gardenio-machine-trigger-ops"), "gd-coord1", http.StatusForbidden},
		{"an agent no trigger spawned", engineCtx(), "gd-hand1", http.StatusForbidden},
		{"an agent outside the lane", engineCtx(), "gd-lap1", http.StatusForbidden},
		{"no such agent", engineCtx(), "gd-none", http.StatusForbidden},
		{"not an agent bead", engineCtx(), "gd-xt1", http.StatusForbidden},
	} {
		if code := claimFor(t, s, tc.ctx, "gd-xesc", tc.agent); code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, code, tc.want)
		}
	}
	if escField(t, ms, "gd-xesc", "claimed_by") != nil {
		t.Fatal("a refused claim-for stamped claimed_by")
	}

	// Positive control on the same fixture, then a second agent cannot take it.
	if code := claimFor(t, s, engineCtx(), "gd-xesc", "gd-coord1"); code != http.StatusOK {
		t.Fatalf("engine claim-for gd-coord1: got %d, want 200", code)
	}
	if code := claimFor(t, s, engineCtx(), "gd-xesc", "gd-coord2"); code != http.StatusConflict {
		t.Fatalf("claim-for a second agent: got %d, want 409", code)
	}

	// A lane that is not a coordinator's is refused.
	clusterEscalation(ms, "gd-xop", map[string]any{"claimed_by": nil, "held_by": "operator"})
	if code := claimFor(t, s, engineCtx(), "gd-xop", "gd-coord1"); code != http.StatusForbidden {
		t.Fatalf("held_by operator: got %d, want 403", code)
	}
	ms.beads["gd-coord1"].Status = model.StatusClosed
	clusterEscalation(ms, "gd-xesc", map[string]any{"claimed_by": nil})
	if code := claimFor(t, s, engineCtx(), "gd-xesc", "gd-coord1"); code != http.StatusForbidden {
		t.Fatalf("closed agent bead: got %d, want 403", code)
	}
}
