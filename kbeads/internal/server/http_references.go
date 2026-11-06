package server

import (
	"net/http"
	"strconv"

	"gardenio/kbeads/internal/model"
)

// defaultReferencesLimit is the per-page row count when ?limit= is missing or
// invalid. The store enforces the same default if a non-positive value reaches
// it, but parsing here lets the response reflect what was actually used.
const defaultReferencesLimit = 50

// maxReferencesLimit caps how many rows a single page may return so an
// unbounded ?limit= can't be used to spike memory.
const maxReferencesLimit = 500

// parseRelationshipTypeFilter reads ?type= from the request and converts
// to the *string filter shape used by the store layer (T6,
// kbeads/docs/research-typed-mention-syntax.md):
//
//   - param absent      → nil (no filter; all rows).
//   - ?type=<verb>      → non-nil pointer to "<verb>" (exact match).
//   - ?type= (empty)    → non-nil pointer to "" (untyped only — IS NULL).
//   - ?type=mentions    → non-nil pointer to "" (alias for the untyped
//     family; "mentions" is the dep_type name we already
//     emit for untyped rows on /v1/graph).
func parseRelationshipTypeFilter(r *http.Request) *string {
	q := r.URL.Query()
	if !q.Has("type") {
		return nil
	}
	v := q.Get("type")
	if v == "mentions" {
		v = ""
	}
	return &v
}

// parseLimitOffset reads ?limit= and ?offset= from the request, applies
// defaults, and clamps limit to [1, maxReferencesLimit]. Returns (limit,
// offset, ok) — ok is false when a value was supplied but not a valid integer,
// in which case the caller has already received an HTTP 400.
func parseLimitOffset(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	q := r.URL.Query()
	limit := defaultReferencesLimit
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return 0, 0, false
		}
		if v > maxReferencesLimit {
			v = maxReferencesLimit
		}
		limit = v
	}
	offset := 0
	if raw := q.Get("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = v
	}
	return limit, offset, true
}

// handleListMentionedBy handles GET /v1/beads/{id}/mentioned-by.
//
// Returns the beads whose text references {id}. One row per (source bead,
// source field) — a bead that mentions {id} in both its description and a
// comment appears twice, with different source_field values. Soft-deleted
// source beads are filtered out by the store. See
// kbeads/docs/design-bead-references.md § 5a.
func (s *BeadsServer) handleListMentionedBy(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	limit, offset, ok := parseLimitOffset(w, r)
	if !ok {
		return
	}
	empty := map[string]any{"references": []*model.BeadReferenceSummary{}, "total": 0, "limit": limit, "offset": offset}
	if !s.authorizeSubresourceRead(w, r, beadID, empty) {
		return
	}

	relType := parseRelationshipTypeFilter(r)
	rows, total, err := s.store.ListBeadsMentioning(r.Context(), beadID, limit, offset, relType)
	if err != nil {
		writeServerError(r.Context(), w, "handleListMentionedBy", err, "failed to list mentioned-by")
		return
	}
	rows, dropped, err := s.gateReferenceRows(r.Context(), rows, true)
	if err != nil {
		writeServerError(r.Context(), w, "handleListMentionedBy", err, "failed to resolve taint grants")
		return
	}
	total -= dropped
	if rows == nil {
		rows = []*model.BeadReferenceSummary{}
	}
	// Each row carries the other bead's title: inherit:read for those too.
	if err := s.propagateReadTaintBeads(r.Context(), referenceSummaryIDs(rows)); err != nil {
		writeServerError(r.Context(), w, "handleListMentionedBy", err, "failed to propagate read taint")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"references": rows,
		"total":      total,
		"limit":      limit,
		"offset":     offset,
	})
}

// handleListMentions handles GET /v1/beads/{id}/mentions.
//
// Returns the references emitted by {id} — i.e. the ka-IDs that appear in
// {id}'s title, description, notes, and comments. The store LEFT-JOINs to
// beads(id), so dangling targets (typos, cross-fleet IDs, not-yet-created
// beads) still appear with empty Title/Type/Status — matching the design's
// intent that bead_references.target_bead_id is intentionally NOT a foreign
// key. See kbeads/docs/design-bead-references.md § 5a.
func (s *BeadsServer) handleListMentions(w http.ResponseWriter, r *http.Request) {
	beadID := r.PathValue("id")
	if beadID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	limit, offset, ok := parseLimitOffset(w, r)
	if !ok {
		return
	}
	empty := map[string]any{"references": []*model.BeadReferenceSummary{}, "total": 0, "limit": limit, "offset": offset}
	if !s.authorizeSubresourceRead(w, r, beadID, empty) {
		return
	}

	relType := parseRelationshipTypeFilter(r)
	rows, total, err := s.store.ListBeadReferencesFrom(r.Context(), beadID, limit, offset, relType)
	if err != nil {
		writeServerError(r.Context(), w, "handleListMentions", err, "failed to list mentions")
		return
	}
	rows, _, err = s.gateReferenceRows(r.Context(), rows, false)
	if err != nil {
		writeServerError(r.Context(), w, "handleListMentions", err, "failed to resolve taint grants")
		return
	}
	if rows == nil {
		rows = []*model.BeadReferenceSummary{}
	}
	// Each row carries the other bead's title: inherit:read for those too.
	if err := s.propagateReadTaintBeads(r.Context(), referenceSummaryIDs(rows)); err != nil {
		writeServerError(r.Context(), w, "handleListMentions", err, "failed to propagate read taint")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"references": rows,
		"total":      total,
		"limit":      limit,
		"offset":     offset,
	})
}
