package server

// Tests for the decision bead type and gate system:
//
//  1. type:decision is in builtinConfigs — kd decision create no longer returns
//     "unknown bead type decision".
//  2. UpdateBead merges fields instead of replacing them — kd decision respond
//     (which only sends response_text/chosen) no longer fails "prompt: is required".
//  3. Full decision gate flow: hook emit upserts gate, gb yield satisfies the
//     gate (via POST /v1/agents/{id}/gates/decision/satisfy), which unblocks Stop.
//  4. Gate is consumed on allow: after Stop is allowed, the next Stop blocks again.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// ── stateful gate mock ─────────────────────────────────────────────────────
//
// The base mockStore stubs all gate methods as no-ops. For decision tests we
// need a store that actually tracks gate state so we can verify satisfaction.

type gateKey struct{ agent, gate string }

type gateState struct {
	satisfied bool
}

type gatedMockStore struct {
	*mockStore
	gates map[gateKey]*gateState
}

func newGatedStore() *gatedMockStore {
	return &gatedMockStore{
		mockStore: newMockStore(),
		gates:     make(map[gateKey]*gateState),
	}
}

func (g *gatedMockStore) UpsertGate(_ context.Context, agentID, gateID string) error {
	k := gateKey{agentID, gateID}
	if _, exists := g.gates[k]; !exists {
		g.gates[k] = &gateState{satisfied: false}
	}
	return nil
}

func (g *gatedMockStore) MarkGateSatisfied(_ context.Context, agentID, gateID string) error {
	k := gateKey{agentID, gateID}
	if st, ok := g.gates[k]; ok {
		st.satisfied = true
	}
	return nil
}

func (g *gatedMockStore) ClearGate(_ context.Context, agentID, gateID string) error {
	delete(g.gates, gateKey{agentID, gateID})
	return nil
}

func (g *gatedMockStore) IsGateSatisfied(_ context.Context, agentID, gateID string) (bool, error) {
	if st, ok := g.gates[gateKey{agentID, gateID}]; ok {
		return st.satisfied, nil
	}
	return false, nil
}

func (g *gatedMockStore) ListGates(_ context.Context, _ string) ([]model.GateRow, error) {
	return nil, nil
}

// newGatedTestServer returns a server backed by a stateful gate store.
// Wired with bearerSubjectVerifier + defaultBearerMiddleware so requests
// arrive at the handler with an OIDC identity (post-R2 fail-closed write
// paths require a resolvable operator).
func newGatedTestServer() (*BeadsServer, *gatedMockStore, http.Handler) {
	gs := newGatedStore()
	s := NewBeadsServer(gs, &events.NoopPublisher{})
	s.userProvisioner = nil
	inner := s.NewHTTPHandler(&bearerSubjectVerifier{})
	return s, gs, defaultBearerMiddleware(inner)
}

// ── type:decision in builtinConfigs ───────────────────────────────────────

// TestDecisionTypeRegistered verifies that POST /v1/beads with type=decision
// succeeds (i.e. the type config is in builtinConfigs).
func TestDecisionTypeRegistered(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "approve deploy?",
		"type":  "decision",
		"kind":  "data",
		"force": true,
		"fields": map[string]any{
			"prompt": "Should we deploy to prod?",
		},
	})
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if resp["type"] != "decision" {
		t.Fatalf("expected type=decision, got %v", resp["type"])
	}
	if resp["id"] == "" {
		t.Fatal("expected non-empty id")
	}
}

// TestDecisionTypeConfig verifies that GET /v1/configs/type:decision returns
// the builtin config with kind=data.
func TestDecisionTypeConfig(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/configs/type:decision", nil)
	requireStatus(t, rec, 200)

	var cfg struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	decodeJSON(t, rec, &cfg)
	if cfg.Key != "type:decision" {
		t.Fatalf("expected key=type:decision, got %q", cfg.Key)
	}

	var tc map[string]any
	if err := json.Unmarshal(cfg.Value, &tc); err != nil {
		t.Fatalf("failed to decode type config value: %v", err)
	}
	if tc["kind"] != "data" {
		t.Fatalf("expected kind=data, got %v", tc["kind"])
	}
}

// TestDecisionCreateWithOperatedBy is a regression test for gd-5Na4vNHOCc: the
// `gb decision create --for` feature (ka-0000000024) began stamping operated_by
// and gifted_by on new decision beads, but the type:decision schema did not
// declare them, so every create 400'd with "operated_by: unknown field" — which
// red-gated the release E2E (decisions-yield + gate-system, release 2026.240.1).
// The schema now declares both, so a create carrying them must succeed.
func TestDecisionCreateWithOperatedBy(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "gift?",
		"type":  "decision",
		"kind":  "data",
		"fields": map[string]any{
			"prompt":      "Push the prepared fix?",
			"options":     []map[string]any{{"id": "y", "label": "Yes"}, {"id": "n", "label": "No"}},
			"operated_by": "slack:U0AB1CD2EF",
			"gifted_by":   "slack:U0OPERATOR1",
		},
	})
	requireStatus(t, createRec, 201)
}

// TestDecisionCreateWithEvidence: `gb decision create --evidence` stamps the
// evidence field (comma-separated bead ids whose screenshots the decision
// shows, gd-4hDcGbtVGc). type:decision is strict, so until the builtin
// declared it every such create 400'd "evidence: unknown field" and no
// decision was filed.
func TestDecisionCreateWithEvidence(t *testing.T) {
	_, ms, h := newTestServer()

	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "ship it?",
		"type":  "decision",
		"kind":  "data",
		"fields": map[string]any{
			"prompt":          "Ship the new card?",
			"options":         []map[string]any{{"id": "y", "label": "Yes"}, {"id": "n", "label": "No"}},
			"requesting_task": "kd-task1",
			"evidence":        "kd-shots1,kd-shots2",
		},
	})
	requireStatus(t, rec, 201)

	var created map[string]any
	decodeJSON(t, rec, &created)
	var fields map[string]any
	if err := json.Unmarshal(ms.beads[created["id"].(string)].Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["evidence"] != "kd-shots1,kd-shots2" {
		t.Errorf("evidence = %v, want it stored as stamped", fields["evidence"])
	}
}

// ── field merge on update ──────────────────────────────────────────────────

// TestUpdateDecisionFieldsMerged verifies that PATCH /v1/beads/{id} merges
// the incoming fields into existing ones rather than replacing them.
// This is the fix for kd decision respond failing "prompt: is required".
func TestUpdateDecisionFieldsMerged(t *testing.T) {
	_, ms, h := newTestServer()

	// Create a decision bead with prompt and options already set.
	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "deploy?",
		"type":  "decision",
		"kind":  "data",
		"fields": map[string]any{
			"prompt":  "Deploy to production?",
			"options": []map[string]any{{"id": "y", "label": "Yes"}, {"id": "n", "label": "No"}},
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// Now update with only the response fields (simulating kd decision respond).
	updateRec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
		"fields": map[string]any{
			"chosen":       "y",
			"responded_by": "alice",
		},
	})
	requireStatus(t, updateRec, 200)

	// Verify the updated bead still has the original prompt AND the new response.
	bead := ms.beads[id]
	if bead == nil {
		t.Fatalf("bead %s not found in store", id)
	}
	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to decode bead fields: %v", err)
	}
	if fields["prompt"] != "Deploy to production?" {
		t.Errorf("prompt field overwritten; got %v", fields["prompt"])
	}
	if fields["chosen"] != "y" {
		t.Errorf("chosen field not set; got %v", fields["chosen"])
	}
	if fields["responded_by"] != "alice" {
		t.Errorf("responded_by field not set; got %v", fields["responded_by"])
	}
}

// TestUpdateDecisionReflagFields verifies that PATCH /v1/beads/{id} accepts
// the reflag_count/reflag_reason/reflagged_at/reflagged_by fields the
// `gb decision reflag` CLI writes. Regression test for gd-Q37TKUAgFO.
func TestUpdateDecisionReflagFields(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "deploy?",
		"type":  "decision",
		"kind":  "data",
		"fields": map[string]any{
			"prompt":  "Deploy to production?",
			"options": []map[string]any{{"id": "y", "label": "Yes"}, {"id": "n", "label": "No"}},
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	updateRec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
		"fields": map[string]any{
			"prompt":        "Deploy to production?",
			"reflag_count":  "1",
			"reflag_reason": "still blocking release",
			"reflagged_at":  "2026-05-23T01:00:00Z",
			"reflagged_by":  "gardener-x",
		},
	})
	requireStatus(t, updateRec, 200)

	bead := ms.beads[id]
	if bead == nil {
		t.Fatalf("bead %s not found in store", id)
	}
	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to decode bead fields: %v", err)
	}
	if fields["reflag_count"] != "1" {
		t.Errorf("reflag_count not set; got %v", fields["reflag_count"])
	}
	if fields["reflag_reason"] != "still blocking release" {
		t.Errorf("reflag_reason not set; got %v", fields["reflag_reason"])
	}
	if fields["reflagged_at"] != "2026-05-23T01:00:00Z" {
		t.Errorf("reflagged_at not set; got %v", fields["reflagged_at"])
	}
	if fields["reflagged_by"] != "gardener-x" {
		t.Errorf("reflagged_by not set; got %v", fields["reflagged_by"])
	}
}

// TestDecisionArtifactFields verifies that artifact-lifecycle fields
// (required_artifact, artifact_status) are accepted by PATCH on a decision bead.
func TestDecisionArtifactFields(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "deploy?",
		"type":  "decision",
		"kind":  "data",
		"force": true,
		"fields": map[string]any{
			"prompt": "Deploy now?",
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// Simulate gb decision respond setting artifact lifecycle fields.
	updateRec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
		"fields": map[string]any{
			"chosen":            "go",
			"required_artifact": "report",
			"artifact_status":   "pending",
		},
	})
	requireStatus(t, updateRec, 200)

	bead := ms.beads[id]
	if bead == nil {
		t.Fatalf("bead %s not found", id)
	}
	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to decode fields: %v", err)
	}
	if fields["required_artifact"] != "report" {
		t.Errorf("required_artifact = %v, want report", fields["required_artifact"])
	}
	if fields["artifact_status"] != "pending" {
		t.Errorf("artifact_status = %v, want pending", fields["artifact_status"])
	}
}

// ── decision gate: full flow ───────────────────────────────────────────────

// TestDecisionGateFlow exercises the complete decision gate lifecycle:
//  1. POST /v1/hooks/emit with hook_type=Stop → gate is pending → blocked
//  2. POST /v1/beads (decision) sets requesting_agent_bead_id
//  3. POST /v1/decisions/{id}/resolve → closes decision (gate NOT yet satisfied)
//  4. POST /v1/agents/{id}/gates/decision/satisfy → gate satisfied (simulates gb yield)
//  5. POST /v1/hooks/emit with hook_type=Stop → now unblocked (gate consumed)
func TestDecisionGateFlow(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-test"

	// Step 1: Emit Stop hook → gate is pending → response should be blocked.
	stopRec := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id":     agentID,
		"hook_type":         "Stop",
		"claude_session_id": "sess-abc",
		"cwd":               "/workspace",
		"actor":             "test-agent",
	})
	requireStatus(t, stopRec, 200)
	var stopResp1 map[string]any
	decodeJSON(t, stopRec, &stopResp1)
	if stopResp1["block"] != true {
		t.Fatalf("expected block=true on first Stop, got %v", stopResp1)
	}

	// Step 2: Create a decision bead referencing the agent.
	decisionRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "approve shutdown?",
		"type":  "decision",
		"kind":  "data",
		"force": true,
		"fields": map[string]any{
			"prompt":                   "Can the agent shut down?",
			"requesting_agent_bead_id": agentID,
		},
	})
	requireStatus(t, decisionRec, 201)
	var decisionBead map[string]any
	decodeJSON(t, decisionRec, &decisionBead)
	decisionID := decisionBead["id"].(string)

	// Gate should still be pending (decision not yet resolved).
	if st := gs.gates[gateKey{agentID, "decision"}]; st != nil && st.satisfied {
		t.Fatal("gate should not be satisfied yet")
	}

	// Step 3: Resolve the decision. Gate is NOT satisfied by resolve — that is
	// now gb yield's responsibility (via POST .../satisfy).
	resolveRec := doJSON(t, h, "POST", "/v1/decisions/"+decisionID+"/resolve", map[string]any{
		"selected_option": "y",
		"responded_by":    "test-agent",
	})
	requireStatus(t, resolveRec, 200)

	// Gate should still be pending after resolve.
	if st := gs.gates[gateKey{agentID, "decision"}]; st != nil && st.satisfied {
		t.Fatal("gate should not be satisfied by resolve alone")
	}

	// Step 4: gb yield calls the satisfy endpoint after detecting the decision
	// was resolved, and also sets gate_satisfied_by=yield on the agent bead.
	satisfyRec := doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	requireStatus(t, satisfyRec, 200)

	if st := gs.gates[gateKey{agentID, "decision"}]; st == nil || !st.satisfied {
		t.Fatal("gate should be satisfied after satisfy endpoint called")
	}

	// Simulate gb yield setting gate_satisfied_by=yield on the agent bead.
	// The stop hook requires this field to verify the gate was properly satisfied.
	gs.beads[agentID] = &model.Bead{
		ID:     agentID,
		Fields: json.RawMessage(`{"gate_satisfied_by":"yield"}`),
	}

	// Step 5: Emit Stop hook again → gate is now satisfied AND gate_satisfied_by=yield
	// → unblocked (and gate consumed).
	stopRec2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id":     agentID,
		"hook_type":         "Stop",
		"claude_session_id": "sess-abc",
		"cwd":               "/workspace",
		"actor":             "test-agent",
	})
	requireStatus(t, stopRec2, 200)
	var stopResp2 map[string]any
	decodeJSON(t, stopRec2, &stopResp2)
	if stopResp2["block"] == true {
		t.Fatalf("expected unblocked on Stop after gate satisfied, got %v", stopResp2)
	}
}

// TestDecisionGateNotSatisfiedByClose verifies that closing a decision bead
// alone does NOT satisfy the gate. Gate satisfaction is now gb yield's
// responsibility (via POST /v1/agents/{id}/gates/{gate}/satisfy).
func TestDecisionGateNotSatisfiedByClose(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-close-test"

	// Emit Stop to register the gate.
	stopRec := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stopRec, 200)
	var stopResp map[string]any
	decodeJSON(t, stopRec, &stopResp)
	if stopResp["block"] != true {
		t.Fatalf("expected block=true, got %v", stopResp)
	}

	// Create and close a decision bead for this agent.
	decisionRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "ok to close?",
		"type":  "decision",
		"kind":  "data",
		"force": true,
		"fields": map[string]any{
			"prompt":                   "Proceed?",
			"requesting_agent_bead_id": agentID,
		},
	})
	requireStatus(t, decisionRec, 201)
	var decisionBead map[string]any
	decodeJSON(t, decisionRec, &decisionBead)
	decisionID := decisionBead["id"].(string)

	// Close via POST /v1/beads/{id}/close.
	closeRec := doJSON(t, h, "POST", "/v1/beads/"+decisionID+"/close", map[string]any{
		"closed_by": "test-agent",
	})
	requireStatus(t, closeRec, 200)

	// Gate must still be pending after close — satisfaction is gb yield's job.
	if st := gs.gates[gateKey{agentID, "decision"}]; st != nil && st.satisfied {
		t.Fatal("gate must not be satisfied by close alone; gb yield must call the satisfy endpoint")
	}
}

// TestDecisionResolve_ParksGatedBead_EndToEnd is the end-to-end regression test
// for the intake-thrash fix (ka-0000005620): resolving a decision through the
// real POST /v1/decisions/{id}/resolve handler parks the OPEN bead blocked-by it
// at `blocked`, so a closed decision's `blocks` edge can no longer silently free
// the gated work back into the ready lane. This also asserts the resolve handler
// actually reaches afterBeadClosed (it previously bypassed the choke point).
func TestDecisionResolve_ParksGatedBead_EndToEnd(t *testing.T) {
	_, gs, h := newGatedTestServer()

	// The operator-gated bead thrashing in the intake lane.
	gs.beads["gd-gated"] = &model.Bead{
		ID:     "gd-gated",
		Kind:   model.KindIssue,
		Type:   model.TypeTask,
		Title:  "operator-gated work",
		Status: model.StatusOpen,
	}

	// Create the decision that gates it.
	decisionRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":  "hold, flip, or re-home?",
		"type":   "decision",
		"kind":   "data",
		"force":  true,
		"fields": map[string]any{"prompt": "which?"},
	})
	requireStatus(t, decisionRec, 201)
	var decisionBead map[string]any
	decodeJSON(t, decisionRec, &decisionBead)
	decisionID := decisionBead["id"].(string)

	// Link the gate exactly as `gb decision create --requesting-task` does:
	// the gated bead blocks→ the decision.
	if err := gs.AddDependency(context.Background(), &model.Dependency{
		BeadID:      "gd-gated",
		DependsOnID: decisionID,
		Type:        model.DepBlocks,
	}); err != nil {
		t.Fatalf("link blocks edge: %v", err)
	}

	// Operator resolves the decision.
	resolveRec := doJSON(t, h, "POST", "/v1/decisions/"+decisionID+"/resolve", map[string]any{
		"selected_option": "hold",
		"responded_by":    "operator@example.com",
	})
	requireStatus(t, resolveRec, 200)

	// The gated bead must now be parked at `blocked` — not left open to thrash.
	if got := gs.beads["gd-gated"].Status; got != model.StatusBlocked {
		t.Fatalf("expected gated bead parked to blocked after decision resolve, got status=%q", got)
	}
}

// TestDecisionCancel_ParksGatedBead_EndToEnd verifies the dismiss path parks its
// gated work too. Dismissal is the worst thrash case (the question was never even
// answered, yet the gate evaporated: ka-0000004466) — POST
// /v1/decisions/{id}/cancel must run the same close cascade as resolve.
func TestDecisionCancel_ParksGatedBead_EndToEnd(t *testing.T) {
	_, gs, h := newGatedTestServer()

	gs.beads["gd-gated"] = &model.Bead{
		ID:     "gd-gated",
		Kind:   model.KindIssue,
		Type:   model.TypeTask,
		Title:  "operator-gated work",
		Status: model.StatusOpen,
	}

	decisionRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":  "which auth path?",
		"type":   "decision",
		"kind":   "data",
		"force":  true,
		"fields": map[string]any{"prompt": "which?"},
	})
	requireStatus(t, decisionRec, 201)
	var decisionBead map[string]any
	decodeJSON(t, decisionRec, &decisionBead)
	decisionID := decisionBead["id"].(string)

	if err := gs.AddDependency(context.Background(), &model.Dependency{
		BeadID:      "gd-gated",
		DependsOnID: decisionID,
		Type:        model.DepBlocks,
	}); err != nil {
		t.Fatalf("link blocks edge: %v", err)
	}

	cancelRec := doJSON(t, h, "POST", "/v1/decisions/"+decisionID+"/cancel", map[string]any{
		"reason": "premise contradicted; re-approaching",
	})
	requireStatus(t, cancelRec, 200)

	if got := gs.beads["gd-gated"].Status; got != model.StatusBlocked {
		t.Fatalf("expected gated bead parked to blocked after decision cancel, got status=%q", got)
	}
}

// TestDecisionGateConsumed verifies the one-shot consumption model: after a
// satisfied gate allows a Stop, the gate is reset to pending so the next Stop
// blocks again. A new decision+yield cycle is required for every Stop.
func TestDecisionGateConsumed(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-consumed"

	// Step 1: First Stop → gate pending → blocked.
	stop1 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop1, 200)
	var r1 map[string]any
	decodeJSON(t, stop1, &r1)
	if r1["block"] != true {
		t.Fatalf("step 1: expected block=true, got %v", r1)
	}

	// Step 2: gb yield calls satisfy endpoint AND sets gate_satisfied_by=yield.
	satisfyRec := doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	requireStatus(t, satisfyRec, 200)
	// Simulate gb yield setting gate_satisfied_by=yield on the agent bead.
	gs.beads[agentID] = &model.Bead{
		ID:     agentID,
		Fields: json.RawMessage(`{"gate_satisfied_by":"yield"}`),
	}

	// Step 3: Second Stop → gate satisfied AND gate_satisfied_by=yield → allowed,
	// gate consumed (reset to pending) and field cleared.
	stop2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop2, 200)
	var r2 map[string]any
	decodeJSON(t, stop2, &r2)
	if r2["block"] == true {
		t.Fatalf("step 3: expected unblocked after gate satisfied, got %v", r2)
	}

	// Step 4: Third Stop → gate was consumed/reset → blocked again.
	stop3 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop3, 200)
	var r3 map[string]any
	decodeJSON(t, stop3, &r3)
	if r3["block"] != true {
		t.Fatalf("step 4: expected block=true after gate was consumed, got %v", r3)
	}
}

// TestDecisionGateManualMarkBlocked verifies that satisfying the decision gate
// via the satisfy endpoint WITHOUT setting gate_satisfied_by=yield on the agent
// bead still results in a block. The stop hook requires the marker field to
// confirm the gate was properly satisfied via 'gb yield'.
func TestDecisionGateManualMarkBlocked(t *testing.T) {
	_, _, h := newGatedTestServer()

	const agentID = "gd-agent-manual-mark"

	// Step 1: Stop → gate pending → blocked.
	stop1 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop1, 200)
	var r1 map[string]any
	decodeJSON(t, stop1, &r1)
	if r1["block"] != true {
		t.Fatalf("expected block=true on first Stop, got %v", r1)
	}

	// Step 2: Satisfy the gate directly (simulates 'gb gate mark decision')
	// WITHOUT setting gate_satisfied_by on the agent bead.
	satisfyRec := doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	requireStatus(t, satisfyRec, 200)

	// Step 3: Stop hook → gate is satisfied BUT gate_satisfied_by is missing
	// → must still block (manual mark, not a real yield).
	stop2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop2, 200)
	var r2 map[string]any
	decodeJSON(t, stop2, &r2)
	if r2["block"] != true {
		t.Fatalf("expected block=true when gate_satisfied_by is missing, got %v", r2)
	}
}

// TestDecisionGateOperatorOverride verifies that an operator using
// 'gb gate mark decision --force' (which sets gate_satisfied_by=operator)
// can unlock the stop gate. This covers the case where no decision bead
// was created (operator wants to let the agent stop without a decision).
func TestDecisionGateOperatorOverride(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-operator-override"

	// Step 1: Stop → gate pending → blocked.
	stop1 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop1, 200)
	var r1 map[string]any
	decodeJSON(t, stop1, &r1)
	if r1["block"] != true {
		t.Fatalf("expected block=true on first Stop, got %v", r1)
	}

	// Step 2: Operator satisfies the gate via 'gb gate mark decision --force',
	// which calls the satisfy endpoint AND sets gate_satisfied_by=operator.
	satisfyRec := doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	requireStatus(t, satisfyRec, 200)
	gs.beads[agentID] = &model.Bead{
		ID:     agentID,
		Fields: json.RawMessage(`{"gate_satisfied_by":"operator"}`),
	}

	// Step 3: Stop hook → gate satisfied AND gate_satisfied_by=operator → allowed.
	stop2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop2, 200)
	var r2 map[string]any
	decodeJSON(t, stop2, &r2)
	if r2["block"] == true {
		t.Fatalf("expected unblocked when gate_satisfied_by=operator, got %v", r2)
	}
}

// TestDecisionGateOperatorNotConsumed verifies that operator overrides persist
// across multiple Stop attempts. Unlike yield-based satisfaction (which is
// consumed after one Stop), operator overrides must survive repeated Stop hooks
// so thread agents and operator-cleared gates don't loop indefinitely.
func TestDecisionGateOperatorNotConsumed(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-operator-persist"

	// Step 1: Stop → gate pending → blocked.
	stop1 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop1, 200)
	var r1 map[string]any
	decodeJSON(t, stop1, &r1)
	if r1["block"] != true {
		t.Fatalf("step 1: expected block=true, got %v", r1)
	}

	// Step 2: Operator satisfies gate + sets gate_satisfied_by=operator.
	satisfyRec := doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	requireStatus(t, satisfyRec, 200)
	gs.beads[agentID] = &model.Bead{
		ID:     agentID,
		Fields: json.RawMessage(`{"gate_satisfied_by":"operator"}`),
	}

	// Step 3: First Stop after operator override → allowed.
	stop2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop2, 200)
	var r2 map[string]any
	decodeJSON(t, stop2, &r2)
	if r2["block"] == true {
		t.Fatalf("step 3: expected unblocked, got %v", r2)
	}

	// Step 4: Second Stop → operator override was NOT consumed → still allowed.
	// This is the key assertion: without the fix, the gate would be consumed
	// after step 3, and this step would block, causing an infinite loop for
	// thread agents.
	stop3 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop3, 200)
	var r3 map[string]any
	decodeJSON(t, stop3, &r3)
	if r3["block"] == true {
		t.Fatalf("step 4: expected operator override to persist (not consumed), got block=true")
	}

	// Step 5: Third Stop → still allowed (persistence is indefinite).
	stop4 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop4, 200)
	var r4 map[string]any
	decodeJSON(t, stop4, &r4)
	if r4["block"] == true {
		t.Fatalf("step 5: expected operator override to persist on third attempt, got block=true")
	}
}

// TestDecisionGateManualForceNotConsumed verifies the legacy "manual-force"
// value for gate_satisfied_by also persists across Stop attempts, matching
// the operator override behavior for backward compatibility.
func TestDecisionGateManualForceNotConsumed(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-manual-force-persist"

	// Register gate via Stop.
	doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})

	// Satisfy gate + set legacy manual-force marker.
	doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	gs.beads[agentID] = &model.Bead{
		ID:     agentID,
		Fields: json.RawMessage(`{"gate_satisfied_by":"manual-force"}`),
	}

	// First Stop → allowed.
	stop1 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop1, 200)
	var r1 map[string]any
	decodeJSON(t, stop1, &r1)
	if r1["block"] == true {
		t.Fatalf("expected unblocked with manual-force, got block=true")
	}

	// Second Stop → still allowed (not consumed).
	stop2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop2, 200)
	var r2 map[string]any
	decodeJSON(t, stop2, &r2)
	if r2["block"] == true {
		t.Fatalf("expected manual-force to persist (not consumed), got block=true")
	}
}

// TestDecisionGateReadySatisfied verifies that 'gb ready' can satisfy the
// decision gate when the agent has no open decisions. This tests the fix for
// the bug where gate_satisfied_by=ready was not accepted by the server,
// causing persistent agents to be blocked after checking for more work.
func TestDecisionGateReadySatisfied(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-ready-test"

	// Step 1: Stop → gate pending → blocked.
	stop1 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop1, 200)
	var r1 map[string]any
	decodeJSON(t, stop1, &r1)
	if r1["block"] != true {
		t.Fatalf("step 1: expected block=true, got %v", r1)
	}

	// Step 2: gb ready satisfies the gate and sets gate_satisfied_by=ready.
	satisfyRec := doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	requireStatus(t, satisfyRec, 200)
	gs.beads[agentID] = &model.Bead{
		ID:     agentID,
		Fields: json.RawMessage(`{"gate_satisfied_by":"ready"}`),
	}

	// Step 3: Stop → gate satisfied with ready → allowed.
	stop2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop2, 200)
	var r2 map[string]any
	decodeJSON(t, stop2, &r2)
	if r2["block"] == true {
		t.Fatalf("step 3: expected unblocked when gate_satisfied_by=ready, got block=true")
	}
}

// TestDecisionGateReadyNotConsumed verifies that ready-based gate satisfaction
// persists across multiple Stop attempts, like operator overrides. This prevents
// persistent agents from looping between gb ready and blocked stops.
func TestDecisionGateReadyNotConsumed(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-ready-persist"

	// Register gate via Stop.
	doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})

	// Satisfy gate with ready.
	doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	gs.beads[agentID] = &model.Bead{
		ID:     agentID,
		Fields: json.RawMessage(`{"gate_satisfied_by":"ready"}`),
	}

	// First Stop → allowed.
	stop1 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop1, 200)
	var r1 map[string]any
	decodeJSON(t, stop1, &r1)
	if r1["block"] == true {
		t.Fatalf("expected unblocked with ready, got block=true")
	}

	// Second Stop → still allowed (not consumed).
	stop2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stop2, 200)
	var r2 map[string]any
	decodeJSON(t, stop2, &r2)
	if r2["block"] == true {
		t.Fatalf("expected ready to persist (not consumed), got block=true")
	}
}

// ── report-gated decision flow ─────────────────────────────────────────

// TestDecisionReportGatedFlow exercises the report-gated lifecycle:
//  1. Create decision with "report:summary" label
//  2. Resolve the decision → resolve response includes report_required
//  3. Gate stays pending until gb yield calls the satisfy endpoint
func TestDecisionReportGatedFlow(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-report-test"

	// Step 1: Emit Stop hook → gate pending → blocked.
	stopRec := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stopRec, 200)
	var stopResp map[string]any
	decodeJSON(t, stopRec, &stopResp)
	if stopResp["block"] != true {
		t.Fatalf("expected block=true on Stop, got %v", stopResp)
	}

	// Step 2: Create decision with report:summary label.
	decisionRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":  "deploy approval?",
		"type":   "decision",
		"kind":   "data",
		"force":  true,
		"labels": []string{"report:summary"},
		"fields": map[string]any{
			"prompt":                   "Approve deploy to prod?",
			"requesting_agent_bead_id": agentID,
		},
	})
	requireStatus(t, decisionRec, 201)
	var decisionBead map[string]any
	decodeJSON(t, decisionRec, &decisionBead)
	decisionID := decisionBead["id"].(string)

	// Step 3: Resolve the decision.
	resolveRec := doJSON(t, h, "POST", "/v1/decisions/"+decisionID+"/resolve", map[string]any{
		"selected_option": "approved",
		"responded_by":    "human",
	})
	requireStatus(t, resolveRec, 200)

	// Verify resolve response includes report_required.
	var resolveResp map[string]any
	decodeJSON(t, resolveRec, &resolveResp)
	if resolveResp["report_required"] != true {
		t.Fatalf("expected report_required=true in resolve response, got %v", resolveResp["report_required"])
	}
	if resolveResp["report_type"] != "summary" {
		t.Fatalf("expected report_type=summary, got %v", resolveResp["report_type"])
	}

	// Gate should still be pending — resolve alone does not satisfy.
	if st := gs.gates[gateKey{agentID, "decision"}]; st != nil && st.satisfied {
		t.Fatal("gate should NOT be satisfied after resolve (gb yield must call satisfy)")
	}

	// Step 4: gb yield calls the satisfy endpoint after report workflow completes,
	// and sets gate_satisfied_by=yield on the agent bead.
	satisfyRec := doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	requireStatus(t, satisfyRec, 200)

	if st := gs.gates[gateKey{agentID, "decision"}]; st == nil || !st.satisfied {
		t.Fatal("gate should be satisfied after satisfy endpoint called")
	}

	// Simulate gb yield setting gate_satisfied_by=yield on the agent bead.
	gs.beads[agentID] = &model.Bead{
		ID:     agentID,
		Fields: json.RawMessage(`{"gate_satisfied_by":"yield"}`),
	}

	// Step 5: Stop hook should now be unblocked.
	stopRec2 := doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})
	requireStatus(t, stopRec2, 200)
	var stopResp2 map[string]any
	decodeJSON(t, stopRec2, &stopResp2)
	if stopResp2["block"] == true {
		t.Fatalf("expected unblocked after satisfy endpoint called, got %v", stopResp2)
	}
}

// TestDecisionWithoutReportLabel verifies decisions without report: labels
// do not include report_required in the resolve response, and that gate
// satisfaction still requires an explicit satisfy call (gb yield).
func TestDecisionWithoutReportLabel(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-no-report"

	// Register gate.
	doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})

	// Create decision WITHOUT report label.
	decisionRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "simple decision",
		"type":  "decision",
		"kind":  "data",
		"force": true,
		"fields": map[string]any{
			"prompt":                   "Continue?",
			"requesting_agent_bead_id": agentID,
		},
	})
	requireStatus(t, decisionRec, 201)
	var bead map[string]any
	decodeJSON(t, decisionRec, &bead)
	decisionID := bead["id"].(string)

	// Resolve.
	resolveRec := doJSON(t, h, "POST", "/v1/decisions/"+decisionID+"/resolve", map[string]any{
		"selected_option": "yes",
		"responded_by":    "human",
	})
	requireStatus(t, resolveRec, 200)

	// Verify resolve response does NOT include report_required.
	var resolveResp map[string]any
	decodeJSON(t, resolveRec, &resolveResp)
	if resolveResp["report_required"] != nil {
		t.Fatalf("expected no report_required for decision without label, got %v", resolveResp["report_required"])
	}

	// Gate should still be pending — gb yield must call satisfy endpoint.
	if st := gs.gates[gateKey{agentID, "decision"}]; st != nil && st.satisfied {
		t.Fatal("gate should NOT be satisfied by resolve alone")
	}

	// gb yield calls satisfy endpoint.
	satisfyRec := doJSON(t, h, "POST", "/v1/agents/"+agentID+"/gates/decision/satisfy", nil)
	requireStatus(t, satisfyRec, 200)

	if st := gs.gates[gateKey{agentID, "decision"}]; st == nil || !st.satisfied {
		t.Fatal("gate should be satisfied after satisfy endpoint called")
	}
}

// TestReportGateSatisfiedByGenericClose verifies that closing a decision bead
// with a report: label via POST /v1/beads/{id}/close also skips gate satisfaction.
func TestReportGateSatisfiedByGenericClose(t *testing.T) {
	_, gs, h := newGatedTestServer()

	const agentID = "gd-agent-generic-close"

	// Register gate.
	doJSON(t, h, "POST", "/v1/hooks/emit", map[string]any{
		"agent_bead_id": agentID,
		"hook_type":     "Stop",
		"actor":         "test-agent",
	})

	// Create decision with report label.
	decisionRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":  "report-gated close test",
		"type":   "decision",
		"kind":   "data",
		"force":  true,
		"labels": []string{"report:deploy-checklist"},
		"fields": map[string]any{
			"prompt":                   "Ready?",
			"requesting_agent_bead_id": agentID,
		},
	})
	requireStatus(t, decisionRec, 201)
	var bead map[string]any
	decodeJSON(t, decisionRec, &bead)
	decisionID := bead["id"].(string)

	// Close via generic close endpoint.
	closeRec := doJSON(t, h, "POST", "/v1/beads/"+decisionID+"/close", map[string]any{
		"closed_by": "human",
		"chosen":    "approved",
	})
	requireStatus(t, closeRec, 200)

	// Gate should NOT be satisfied — report required.
	if st := gs.gates[gateKey{agentID, "decision"}]; st != nil && st.satisfied {
		t.Fatal("gate should NOT be satisfied after closing report-gated decision")
	}
}

// TestReportTypeRegistered verifies POST /v1/beads with type=report succeeds.
func TestReportTypeRegistered(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "test report",
		"type":  "report",
		"kind":  "data",
		"fields": map[string]any{
			"decision_id": "gd-decision-123",
			"report_type": "summary",
			"content":     "Report content here.",
		},
	})
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if resp["type"] != "report" {
		t.Fatalf("expected type=report, got %v", resp["type"])
	}
}

// TestDecisionExtractFieldsReportRequired verifies that extractDecisionFields
// includes report_required and report_type when a report: label is present.
func TestDecisionExtractFieldsReportRequired(t *testing.T) {
	_, _, h := newTestServer()

	// Create decision with report label.
	decisionRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":  "decision with report",
		"type":   "decision",
		"kind":   "data",
		"force":  true,
		"labels": []string{"report:code-review"},
		"fields": map[string]any{
			"prompt": "Review needed?",
		},
	})
	requireStatus(t, decisionRec, 201)
	var bead map[string]any
	decodeJSON(t, decisionRec, &bead)
	decisionID := bead["id"].(string)

	// GET /v1/decisions/{id} should include report fields.
	getRec := doJSON(t, h, "GET", "/v1/decisions/"+decisionID, nil)
	requireStatus(t, getRec, 200)

	var resp struct {
		Decision map[string]any `json:"decision"`
	}
	decodeJSON(t, getRec, &resp)

	if resp.Decision["report_required"] != true {
		t.Fatalf("expected report_required=true, got %v", resp.Decision["report_required"])
	}
	if resp.Decision["report_type"] != "code-review" {
		t.Fatalf("expected report_type=code-review, got %v", resp.Decision["report_type"])
	}
}

// TestDecisionCreateUnknownTypeGone verifies the old "unknown bead type" error
// no longer occurs — regression test for the original bug.
func TestDecisionCreateUnknownTypeGone(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title":  "should this work?",
		"type":   "decision",
		"kind":   "data",
		"force":  true,
		"fields": map[string]any{"prompt": "yes?"},
	})
	// Must NOT be 400 "unknown bead type decision".
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("got 400 (unknown bead type); regression: %s", rec.Body.String())
	}
	requireStatus(t, rec, 201)
}
