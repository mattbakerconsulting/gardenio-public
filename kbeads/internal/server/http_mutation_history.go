package server

import (
	"net/http"

	"gardenio/kbeads/internal/model"
)

// handleListMutations handles GET /v1/beads/{id}/mutations. Returns the
// append-only mutation_history rows for a bead in chronological order.
func (s *BeadsServer) handleListMutations(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if !s.authorizeSubresourceRead(w, r, beadID, map[string]any{"mutations": []*model.MutationRow{}}) {
		return
	}

	muts, err := s.store.ListMutations(r.Context(), beadID)
	if err != nil {
		writeServerError(r.Context(), w, "handleListMutations", err, "failed to list mutations")
		return
	}

	if muts == nil {
		muts = []*model.MutationRow{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"mutations": muts})
}
