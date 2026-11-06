package server

import (
	"net/http"
)

// BackfillCategoryRequest is the JSON body for POST /v1/admin/backfill-category.
// Callers paginate the backfill by advancing AfterID with the LastID returned
// in each response, until Done=true (ka-0000007274). It re-stamps the
// beads.category column on legacy rows whose stored category disagrees with the
// current derive_bead_category rule table — chiefly the deliverable/artifact
// rows carrying a stale `intermediate` after migration 043.
type BackfillCategoryRequest struct {
	AfterID   string `json:"after_id"`
	BatchSize int    `json:"batch_size"`
}

// BackfillCategoryResponse is returned by POST /v1/admin/backfill-category.
// LastID is the cursor for the next call (empty when the page was empty).
type BackfillCategoryResponse struct {
	BeadsScanned int    `json:"beads_scanned"`
	LastID       string `json:"last_id"`
	Done         bool   `json:"done"`
}

// handleBackfillCategory handles POST /v1/admin/backfill-category. It re-stamps
// the beads.category materialized column (migration 043 / ka-0000007274) on one
// keyset-paginated page of beads whose stored category disagrees with the
// current derive_bead_category rule table. Admin-only. The migration-020 trigger
// keeps new/updated rows current; this sweeps the rows that pre-date migration
// 043 (mainly deliverable beads stamped `intermediate` before the artifact rule
// landed).
func (s *BeadsServer) handleBackfillCategory(w http.ResponseWriter, r *http.Request) {
	var req BackfillCategoryRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillCategoryPage(r.Context(), req.AfterID, req.BatchSize)
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillCategory", err, "failed to backfill category")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillCategoryResponse{
		BeadsScanned: stats.BeadsScanned,
		LastID:       stats.LastID,
		Done:         stats.Done,
	})
}
