package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"gardenio/kbeads/internal/model"
)

// FindSimilarResponse is the response body for GET /v1/beads/{id}/similar.
type FindSimilarResponse struct {
	SourceID string                  `json:"source_id"`
	Model    string                  `json:"model,omitempty"`
	Results  []*model.SemanticResult `json:"results"`
}

// handleFindSimilar handles GET /v1/beads/{id}/similar. It looks up the
// source bead's stored embedding, runs a semantic nearest-neighbor search,
// excludes the source bead from the results, and applies the caller's
// limit and min_score filters.
//
// Query params:
//   - limit:     max number of results (default 10, clamped to [1, 100])
//   - min_score: drop results with score < min_score (default 0, i.e. no filter)
func (s *BeadsServer) handleFindSimilar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	if limit > 100 {
		limit = 100
	}

	var minScore float64
	if v := r.URL.Query().Get("min_score"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid min_score")
			return
		}
		minScore = f
	}

	// Verify the source bead exists and the caller is authorized to read it.
	// This mirrors the project/fleet checks on handleGetBead.
	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleFindSimilar", err, "failed to get bead")
		return
	}
	if !s.authorizeBeadRead(w, r, bead, "bead not found") {
		return
	}

	vec, modelName, err := s.store.GetEmbedding(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusConflict, "bead has no embedding; run `kd embed sync` or wait for async indexing")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleFindSimilar", err, "failed to look up embedding")
		return
	}

	// Ask for limit+1 so we can drop the source bead from the results (it will
	// match itself with score=1) and still return `limit` neighbors.
	//
	// The source bead read above is gated, but the NEIGHBOR search was not:
	// without scope injection a caller could read full out-of-scope neighbor
	// beads whose only tie to the source is embedding proximity
	// (ka-0000008132). Inject the fleet-token project: label (AND) and the OIDC
	// allowed-projects labels (OR), mirroring handleSemanticSearch.
	filter := s.scopeListFilter(r.Context(), model.BeadFilter{Fleet: FleetFromContext(r.Context())})
	raw, err := s.store.SemanticSearch(r.Context(), vec, filter, limit+1)
	if err != nil {
		writeServerError(r.Context(), w, "handleFindSimilar", err, "semantic search failed")
		return
	}

	out := make([]*model.SemanticResult, 0, limit)
	for _, res := range raw {
		if res == nil || res.Bead == nil {
			continue
		}
		if res.Bead.ID == id {
			continue
		}
		if minScore > 0 && res.Score < minScore {
			continue
		}
		out = append(out, res)
		if len(out) >= limit {
			break
		}
	}

	// inherit:read for every neighbour returned. The source bead's text is
	// not in the response, and it passed no taint gate here.
	if err := s.propagateReadTaintBeads(r.Context(), semanticResultIDs(out)); err != nil {
		writeServerError(r.Context(), w, "handleFindSimilar", err, "failed to propagate read taint")
		return
	}

	writeJSON(w, http.StatusOK, &FindSimilarResponse{
		SourceID: id,
		Model:    modelName,
		Results:  out,
	})
}
