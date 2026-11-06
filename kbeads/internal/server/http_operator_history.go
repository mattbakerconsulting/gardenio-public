package server

import (
	"net/http"

	"gardenio/kbeads/internal/model"
)

// handleGetOperatorHistory handles GET /v1/beads/{id}/operator-history.
// Returns the rows from the bead_operator_history VIEW for one bead,
// newest first. Powers the beads-viewer ownership tab (R5 §2 row S4).
func (s *BeadsServer) handleGetOperatorHistory(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"operator_history": []*model.OperatorHistoryRow{}}) {
		return
	}

	rows, err := s.store.GetOperatorHistory(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetOperatorHistory", err, "failed to get operator history")
		return
	}

	if rows == nil {
		rows = []*model.OperatorHistoryRow{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"operator_history": rows})
}
