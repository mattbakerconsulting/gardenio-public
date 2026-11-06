package server

import (
	"context"
	"net/http"

	"gardenio/kbeads/internal/model"
)

// handleListGates handles GET /v1/agents/{id}/gates.
// Returns all gate rows for an agent bead.
func (s *BeadsServer) handleListGates(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.gateTargetAllowed(r.Context(), agentID) {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	gates, err := s.store.ListGates(r.Context(), agentID)
	if err != nil {
		writeServerError(r.Context(), w, "handleListGates", err, "internal server error")
		return
	}
	if gates == nil {
		gates = []model.GateRow{}
	}
	writeJSON(w, http.StatusOK, gates)
}

// handleSatisfyGate handles POST /v1/agents/{id}/gates/{gate_id}/satisfy.
// Manually satisfies a gate for an agent bead.
func (s *BeadsServer) handleSatisfyGate(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	gateID := r.PathValue("gate_id")
	if agentID == "" || gateID == "" {
		writeError(w, http.StatusBadRequest, "id and gate_id are required")
		return
	}
	if !s.gateTargetAllowed(r.Context(), agentID) {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	// Upsert first to ensure row exists.
	if err := s.store.UpsertGate(r.Context(), agentID, gateID); err != nil {
		writeServerError(r.Context(), w, "handleSatisfyGate.upsert", err, "internal server error")
		return
	}
	if err := s.store.MarkGateSatisfied(r.Context(), agentID, gateID); err != nil {
		writeServerError(r.Context(), w, "handleSatisfyGate", err, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "satisfied"})
}

// handleClearGate handles DELETE /v1/agents/{id}/gates/{gate_id}.
// Resets a gate back to pending status.
func (s *BeadsServer) handleClearGate(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	gateID := r.PathValue("gate_id")
	if agentID == "" || gateID == "" {
		writeError(w, http.StatusBadRequest, "id and gate_id are required")
		return
	}
	if !s.gateTargetAllowed(r.Context(), agentID) {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if err := s.store.ClearGate(r.Context(), agentID, gateID); err != nil {
		writeServerError(r.Context(), w, "handleClearGate", err, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
}

// gateTargetAllowed reports whether the caller may read or change agentID's
// gates. scopedRouteGate lets a tenant land's agent reach these routes
// (tenantAgentRoutes) for its own Stop gate, so without this it could satisfy
// or reset the stop gate of an agent on another land (the cross-land matrix,
// gd-USqiG2sdPU). An agent on a tenant land reaches only its own gates; any
// other id answers 404, the same whether the agent exists or not. Fleet
// agents, bridges and operators are unchanged. A runner reaches only the
// gates of its own agents (runnerAgentOwned).
func (s *BeadsServer) gateTargetAllowed(ctx context.Context, agentID string) bool {
	if !s.runnerAgentOwned(ctx, agentID) {
		return false
	}
	own, ok := agentBeadIDFromContext(ctx)
	if !ok || own == agentID {
		return true
	}
	return !s.callerIsTenant(ctx)
}
