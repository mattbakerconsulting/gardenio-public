package server

import (
	"net/http"
)

// BackfillSearchTSVRequest is the JSON body for POST /v1/admin/backfill-search-tsv.
// Callers paginate the backfill by advancing AfterID with the LastID returned
// in each response, until Done=true. (ka-0000003832 / ka-search relevance
// Option 2.) When Reindex is true, the server REFRESHES the vector on rows
// carrying a context/options field (migration 040's fields extension,
// ka-0000005839) instead of filling NULL legacy rows.
type BackfillSearchTSVRequest struct {
	AfterID   string `json:"after_id"`
	BatchSize int    `json:"batch_size"`
	Reindex   bool   `json:"reindex,omitempty"`
}

// BackfillSearchTSVResponse is returned by POST /v1/admin/backfill-search-tsv.
// LastID is the cursor for the next call (empty when the page was empty). On
// the final page (Done=true) Remaining reports the count of beads whose
// search_tsv is still NULL (0 on a clean finish; always 0 in reindex mode,
// where the candidate set is drained by the keyset cursor).
type BackfillSearchTSVResponse struct {
	BeadsScanned int    `json:"beads_scanned"`
	LastID       string `json:"last_id"`
	Done         bool   `json:"done"`
	Remaining    int64  `json:"remaining,omitempty"`
}

// handleBackfillSearchTSV handles POST /v1/admin/backfill-search-tsv. In the
// default mode it fills the search_tsv FTS column (migration 039) on one
// keyset-paginated page of legacy beads whose search_tsv IS NULL. With
// Reindex=true it refreshes the vector on context/options-bearing rows so
// migration 040's fields extension (ka-0000005839) reaches already-filled rows.
// Admin-only. The migration's trigger keeps new/updated rows current; this
// sweeps the rows that pre-date the (respective) migration.
func (s *BeadsServer) handleBackfillSearchTSV(w http.ResponseWriter, r *http.Request) {
	var req BackfillSearchTSVRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillSearchTSVPage(r.Context(), req.AfterID, req.BatchSize, req.Reindex)
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillSearchTSV", err, "failed to backfill search_tsv")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillSearchTSVResponse{
		BeadsScanned: stats.BeadsScanned,
		LastID:       stats.LastID,
		Done:         stats.Done,
		Remaining:    stats.Remaining,
	})
}
