package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// The executor of an approved bead-close escalation may close exactly the
// beads the escalation names (gd-Ti5JGqdhpd): the 403s of gd-Z7Pz2reaBG and
// gd-7zjEgAp7vf, where the executor owned none of the beads it was approved to
// close.

// closeGrantEsc is mixed-case, as real bead ids are; the bridge spawns its
// executor as closeGrantExecutor (executorAgentName lowercases the id).
const (
	closeGrantEsc      = "gd-Esc1"
	closeGrantExecutor = "escalation-gd-esc1"
)

// closeGrantAgent is an executor agent bead that cites escID.
func closeGrantAgent(id, escID string) *model.Bead {
	return &model.Bead{ID: id, Type: "agent", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"project":"gardenio","role":"crew","escalation_id":"` + escID + `"}`)}
}

// closeGrantEscalation is an approved, in-progress bead-close escalation whose
// executor is closeGrantExecutor; fields override or add to the defaults.
func closeGrantEscalation(fields map[string]any) *model.Bead {
	f := map[string]any{
		"operation_type": "kbeads_bead_close",
		"operation_spec": map[string]any{"bead_ids": []string{"gd-t1", "gd-t2"}},
		"executor":       closeGrantExecutor,
		"approved_at":    "2026-10-27T06:03:07Z",
		"approved_by":    "owner@example.com",
	}
	for k, v := range fields {
		if v == nil {
			delete(f, k)
		} else {
			f[k] = v
		}
	}
	raw, _ := json.Marshal(f)
	return &model.Bead{ID: closeGrantEsc, Type: model.TypeEscalation, Kind: "data", Status: model.StatusInProgress,
		Labels: []string{"project:gardenio"}, Fields: raw}
}

// orphanBead is a bead whose creator is a gone agent: nobody else owns it.
func orphanBead(id string, typ model.BeadType) *model.Bead {
	return &model.Bead{ID: id, Type: typ, Status: model.StatusOpen, CreatedBy: "gone-crew-qa2",
		Labels: []string{"project:gardenio"}}
}

func TestEscalationCloseGrant_ExecutorClosesNamedBeads(t *testing.T) {
	srv := newChainWalkServer(closeGrantAgent("gd-a1", closeGrantEsc), closeGrantEscalation(nil),
		orphanBead("gd-t1", "task"), orphanBead("gd-t2", "bug"), orphanBead("gd-t3", "task"))
	ctx := agentFleetCtx("gd-a1", closeGrantExecutor)

	for _, id := range []string{"gd-t1", "gd-t2"} {
		b, _ := srv.store.GetBead(ctx, id)
		if d := srv.authorizeOwnership(ctx, "close", b); d != nil {
			t.Errorf("close %s: the executor of the approved escalation must pass, got %v", id, d)
		}
	}
	// Nothing else: another bead, or any verb but close on a named one.
	t3, _ := srv.store.GetBead(ctx, "gd-t3")
	if srv.authorizeOwnership(ctx, "close", t3) == nil {
		t.Error("close of a bead the escalation does not name must stay denied")
	}
	t1, _ := srv.store.GetBead(ctx, "gd-t1")
	for _, action := range []string{"update", "delete", "comment"} {
		if srv.authorizeOwnership(ctx, action, t1) == nil {
			t.Errorf("%s of a named bead must stay denied: the grant is close only", action)
		}
	}
}

func TestEscalationCloseGrant_TargetBeadIDAndBeadCloseSpelling(t *testing.T) {
	esc := closeGrantEscalation(map[string]any{
		"operation_type": "bead_close",
		"operation_spec": map[string]any{},
		"target_bead_id": "gd-t1",
	})
	srv := newChainWalkServer(closeGrantAgent("gd-a1", closeGrantEsc), esc, orphanBead("gd-t1", "task"))
	ctx := agentFleetCtx("gd-a1", closeGrantExecutor)
	t1, _ := srv.store.GetBead(ctx, "gd-t1")
	if d := srv.authorizeOwnership(ctx, "close", t1); d != nil {
		t.Fatalf("bead_close naming the bead in target_bead_id must pass, got %v", d)
	}
}

// The escalation's executor field is writable, so it binds nothing: the
// deterministic executor passes whatever it says, and nobody else passes
// because of it (the refusals below).
func TestEscalationCloseGrant_ExecutorFieldIsNotTheBinding(t *testing.T) {
	for _, executor := range []any{nil, "", "crew-a"} {
		srv := newChainWalkServer(closeGrantAgent("gd-a1", closeGrantEsc),
			closeGrantEscalation(map[string]any{"executor": executor}), orphanBead("gd-t1", "task"))
		ctx := agentFleetCtx("gd-a1", closeGrantExecutor)
		t1, _ := srv.store.GetBead(ctx, "gd-t1")
		if d := srv.authorizeOwnership(ctx, "close", t1); d != nil {
			t.Errorf("executor field %v: the deterministic executor must pass, got %v", executor, d)
		}
	}
}

func TestEscalationCloseGrant_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		esc     *model.Bead
		agent   *model.Bead
		ctxName string
		target  *model.Bead
	}{
		{name: "not approved", esc: closeGrantEscalation(map[string]any{"approved_at": nil})},
		{name: "approved_by alone is no approval", esc: closeGrantEscalation(map[string]any{"approved_at": ""})},
		{name: "escalation closed", esc: func() *model.Bead {
			b := closeGrantEscalation(nil)
			b.Status = model.StatusClosed
			return b
		}()},
		{name: "other operation type", esc: closeGrantEscalation(map[string]any{"operation_type": "gitlab_mr_merge"})},
		// The send-back case on PR #1266: an agent points its own bead at the
		// approved escalation and writes its own name into the escalation's
		// executor field. Its token name is not the deterministic one.
		{name: "executor field names the caller, token name is not the executor's",
			esc: closeGrantEscalation(map[string]any{"executor": "crew-a"}), ctxName: "crew-a"},
		{name: "another agent cites the escalation", agent: closeGrantAgent("gd-a2", closeGrantEsc), ctxName: "crew-b"},
		{name: "token name keeps the id's case", ctxName: "escalation-" + closeGrantEsc},
		{name: "service-account executor name", ctxName: "escalation-sa-gd-esc1"},
		{name: "another escalation's executor", ctxName: "escalation-gd-esc2"},
		{name: "agent bead cites no escalation", agent: &model.Bead{ID: "gd-a1", Type: "agent",
			Fields: json.RawMessage(`{"project":"gardenio","role":"crew"}`)}},
		{name: "ids only in a case-variant key", esc: closeGrantEscalation(map[string]any{
			"operation_spec": map[string]any{"Bead_IDs": []string{"gd-t1"}}})},
		{name: "bead_ids not a list", esc: closeGrantEscalation(map[string]any{
			"operation_spec": map[string]any{"bead_ids": "gd-t1"}})},
		{name: "a decision is never closed this way", esc: closeGrantEscalation(map[string]any{
			"operation_spec": map[string]any{"bead_ids": []string{"gd-d1"}}}),
			target: orphanBead("gd-d1", model.TypeDecision)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			esc, agent, name, target := tc.esc, tc.agent, tc.ctxName, tc.target
			if esc == nil {
				esc = closeGrantEscalation(nil)
			}
			if agent == nil {
				agent = closeGrantAgent("gd-a1", closeGrantEsc)
			}
			if name == "" {
				name = closeGrantExecutor
			}
			if target == nil {
				target = orphanBead("gd-t1", "task")
			}
			srv := newChainWalkServer(agent, esc, target)
			if srv.authorizeOwnership(agentFleetCtx(agent.ID, name), "close", target) == nil {
				t.Fatal("close must stay denied")
			}
		})
	}
}

// End to end over HTTP on real per-agent tokens: the deterministic executor's
// close goes through and leaves a mutation_history row naming the escalation;
// a bead it does not name is still a 403, and so is the same close from an
// agent whose own bead cites the escalation and whose name the escalation's
// executor field carries, when its token name is not the executor's.
func TestEscalationCloseGrant_HTTPCloseRecordsAuthority(t *testing.T) {
	srv, ms, h := newClassedTestServer(t)
	const execToken = "agent-tok-esc-exec"
	srv.fleetCfg.RegisterTokenFull(execToken, "agent-gd-ae", "gardenio", nil, nil, time.Time{}, closeGrantExecutor, false)
	ms.beads["gd-ae"] = closeGrantAgent("gd-ae", closeGrantEsc)
	ms.beads["gd-a1"] = closeGrantAgent("gd-a1", closeGrantEsc) // crew-a on testAgentToken
	ms.beads[closeGrantEsc] = closeGrantEscalation(map[string]any{"executor": "crew-a"})
	ms.beads["gd-t1"] = orphanBead("gd-t1", "task")
	ms.beads["gd-t3"] = orphanBead("gd-t3", "task")
	for _, id := range []string{closeGrantEsc, "gd-t1", "gd-t3"} {
		ms.labels[id] = []string{"project:gardenio"}
	}

	rec := doBearerJSON(t, h, "POST", "/v1/beads/gd-t1/close", testAgentToken, map[string]any{}, nil)
	requireStatus(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "ownership denied") {
		t.Fatalf("close by a self-named executor must be an ownership denial, got %s", rec.Body.String())
	}

	rec = doBearerJSON(t, h, "POST", "/v1/beads/gd-t3/close", execToken, map[string]any{}, nil)
	requireStatus(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "ownership denied") {
		t.Fatalf("close of an unnamed bead must be an ownership denial, got %s", rec.Body.String())
	}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads/gd-t1/close", execToken, map[string]any{}, nil), http.StatusOK)
	if ms.beads["gd-t1"].Status != model.StatusClosed {
		t.Fatalf("gd-t1 status = %s, want closed", ms.beads["gd-t1"].Status)
	}

	var rows []*model.MutationRow
	for _, m := range ms.mutations {
		if m.Field == escalationCloseAuthorityField {
			rows = append(rows, m)
		}
	}
	if len(rows) != 1 || rows[0].BeadID != "gd-t1" || rows[0].NewValue != "escalation:"+closeGrantEsc || rows[0].Actor != closeGrantExecutor {
		t.Fatalf("want one close_authority row on gd-t1 naming the escalation, got %+v", rows)
	}

	// A repeat close is the idempotent no-op and records nothing more.
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads/gd-t1/close", execToken, map[string]any{}, nil), http.StatusOK)
	n := 0
	for _, m := range ms.mutations {
		if m.Field == escalationCloseAuthorityField {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("idempotent re-close wrote another close_authority row (%d total)", n)
	}
}
