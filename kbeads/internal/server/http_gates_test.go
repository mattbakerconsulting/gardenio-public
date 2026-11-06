package server

import (
	"context"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// ── stateful gate mock with ListGates support ───────────────────────────
//
// The gatedMockStore from decision_test.go has a ListGates that returns nil.
// For gate endpoint tests we need ListGates to return actual data.

type gateListMockStore struct {
	*mockStore
	gates map[gateKey]*gateState
}

func newGateListStore() *gateListMockStore {
	return &gateListMockStore{
		mockStore: newMockStore(),
		gates:     make(map[gateKey]*gateState),
	}
}

func (g *gateListMockStore) UpsertGate(_ context.Context, agentID, gateID string) error {
	k := gateKey{agentID, gateID}
	if _, exists := g.gates[k]; !exists {
		g.gates[k] = &gateState{satisfied: false}
	}
	return nil
}

func (g *gateListMockStore) MarkGateSatisfied(_ context.Context, agentID, gateID string) error {
	k := gateKey{agentID, gateID}
	if st, ok := g.gates[k]; ok {
		st.satisfied = true
	}
	return nil
}

func (g *gateListMockStore) ClearGate(_ context.Context, agentID, gateID string) error {
	delete(g.gates, gateKey{agentID, gateID})
	return nil
}

func (g *gateListMockStore) IsGateSatisfied(_ context.Context, agentID, gateID string) (bool, error) {
	if st, ok := g.gates[gateKey{agentID, gateID}]; ok {
		return st.satisfied, nil
	}
	return false, nil
}

func (g *gateListMockStore) ListGates(_ context.Context, agentID string) ([]model.GateRow, error) {
	var rows []model.GateRow
	for k, st := range g.gates {
		if k.agent != agentID {
			continue
		}
		status := "pending"
		var satisfiedAt *time.Time
		if st.satisfied {
			status = "satisfied"
			now := time.Now().UTC()
			satisfiedAt = &now
		}
		rows = append(rows, model.GateRow{
			AgentBeadID: k.agent,
			GateID:      k.gate,
			Status:      status,
			SatisfiedAt: satisfiedAt,
		})
	}
	return rows, nil
}

// ── tests ───────────────────────────────────────────────────────────────

func TestHandleListGates_Empty(t *testing.T) {
	_, _, h := newTestServer()

	rec := doJSON(t, h, "GET", "/v1/agents/gd-agent-1/gates", nil)
	requireStatus(t, rec, 200)

	var gates []model.GateRow
	decodeJSON(t, rec, &gates)
	if len(gates) != 0 {
		t.Fatalf("expected 0 gates, got %d", len(gates))
	}
}

func TestHandleListGates_WithData(t *testing.T) {
	gs := newGateListStore()
	s := NewBeadsServer(gs, &events.NoopPublisher{})
	h := s.NewHTTPHandler(nil)

	// Seed some gates.
	gs.gates[gateKey{"gd-agent-1", "decision"}] = &gateState{satisfied: false}
	gs.gates[gateKey{"gd-agent-1", "review"}] = &gateState{satisfied: true}
	gs.gates[gateKey{"gd-other", "decision"}] = &gateState{satisfied: false}

	rec := doJSON(t, h, "GET", "/v1/agents/gd-agent-1/gates", nil)
	requireStatus(t, rec, 200)

	var gates []model.GateRow
	decodeJSON(t, rec, &gates)
	if len(gates) != 2 {
		t.Fatalf("expected 2 gates for gd-agent-1, got %d", len(gates))
	}

	// Verify we got the right statuses.
	statusMap := make(map[string]string)
	for _, g := range gates {
		statusMap[g.GateID] = g.Status
	}
	if statusMap["decision"] != "pending" {
		t.Fatalf("expected decision gate to be pending, got %q", statusMap["decision"])
	}
	if statusMap["review"] != "satisfied" {
		t.Fatalf("expected review gate to be satisfied, got %q", statusMap["review"])
	}
}

func TestHandleSatisfyGate(t *testing.T) {
	_, gs, h := newGatedTestServer()

	// Create a pending gate.
	gs.gates[gateKey{"gd-agent-1", "decision"}] = &gateState{satisfied: false}

	rec := doJSON(t, h, "POST", "/v1/agents/gd-agent-1/gates/decision/satisfy", nil)
	requireStatus(t, rec, 200)

	var resp map[string]string
	decodeJSON(t, rec, &resp)
	if resp["status"] != "satisfied" {
		t.Fatalf("expected status=satisfied, got %q", resp["status"])
	}

	// Verify the gate is actually satisfied.
	if st, ok := gs.gates[gateKey{"gd-agent-1", "decision"}]; !ok || !st.satisfied {
		t.Fatal("expected gate to be satisfied in store")
	}
}

func TestHandleSatisfyGate_CreatesIfNotExists(t *testing.T) {
	_, gs, h := newGatedTestServer()

	// No gate exists yet — satisfy should upsert then satisfy.
	rec := doJSON(t, h, "POST", "/v1/agents/gd-agent-1/gates/newgate/satisfy", nil)
	requireStatus(t, rec, 200)

	if st, ok := gs.gates[gateKey{"gd-agent-1", "newgate"}]; !ok || !st.satisfied {
		t.Fatal("expected gate to be created and satisfied")
	}
}

func TestHandleClearGate(t *testing.T) {
	_, gs, h := newGatedTestServer()

	// Create a satisfied gate.
	gs.gates[gateKey{"gd-agent-1", "decision"}] = &gateState{satisfied: true}

	rec := doJSON(t, h, "DELETE", "/v1/agents/gd-agent-1/gates/decision", nil)
	requireStatus(t, rec, 200)

	var resp map[string]string
	decodeJSON(t, rec, &resp)
	if resp["status"] != "pending" {
		t.Fatalf("expected status=pending, got %q", resp["status"])
	}

	// Verify gate is cleared.
	if _, ok := gs.gates[gateKey{"gd-agent-1", "decision"}]; ok {
		t.Fatal("expected gate to be removed from store after clear")
	}
}

func TestHandleClearGate_Nonexistent(t *testing.T) {
	_, _, h := newGatedTestServer()

	// Clearing a non-existent gate should still return 200 (idempotent).
	rec := doJSON(t, h, "DELETE", "/v1/agents/gd-agent-1/gates/nonexistent", nil)
	requireStatus(t, rec, 200)
}
