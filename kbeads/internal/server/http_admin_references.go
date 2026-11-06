package server

import (
	"context"
	"errors"
	"net/http"
)

// reextractTimeoutMessage is the client-visible body returned when a page
// overruns the per-request deadline. The generic `failed to reextract
// references` message discarded the actionable `context deadline exceeded`
// cause, so an operator could not tell a timeout from corrupt data without
// kubectl on the daemon (ka-0000001700). The kd CLI also keys on this stable
// prefix to auto-halve the page and retry the same cursor.
const reextractTimeoutMessage = "reextract page exceeded the request time budget; retry with a smaller --batch-size"

// ReextractReferencesRequest is the JSON body for POST /v1/admin/reextract-references.
// Callers paginate the backfill by advancing AfterID with the LastID returned
// in each response, until Done=true. Type, when non-empty, narrows the walk to
// beads of that type — used to reindex `type=decision` FIRST so the
// parked_on_decision badge is sound before the full ~677k-row backfill reaches
// the late-sorting decision ids (ka-0000005898).
type ReextractReferencesRequest struct {
	AfterID   string `json:"after_id"`
	BatchSize int    `json:"batch_size"`
	Type      string `json:"type,omitempty"`
}

// ReextractReferencesResponse is returned by POST /v1/admin/reextract-references.
// LastID is the cursor for the next call (empty when the page was empty).
// On the final page (Done=true) TotalReferences reports the post-backfill
// cardinality of bead_references.
type ReextractReferencesResponse struct {
	BeadsScanned      int    `json:"beads_scanned"`
	CommentsScanned   int    `json:"comments_scanned"`
	ReferencesWritten int    `json:"references_written"`
	LastID            string `json:"last_id"`
	Done              bool   `json:"done"`
	TotalReferences   int64  `json:"total_references,omitempty"`
}

// handleReextractReferences handles POST /v1/admin/reextract-references.
// Walks one keyset-paginated page of beads, re-runs reference extraction
// over each bead's text fields and every one of its comments, and replaces
// the corresponding rows in bead_references. Admin-only.
func (s *BeadsServer) handleReextractReferences(w http.ResponseWriter, r *http.Request) {
	var req ReextractReferencesRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.ReextractReferencesPage(r.Context(), req.AfterID, req.BatchSize, req.Type)
	if err != nil {
		// A deadline hit is expected on an over-large page: it means the walk
		// tipped over the per-request budget, NOT that the data is corrupt.
		// Surface a distinct, actionable body (still a 500 so the CLI's
		// auto-halving retry engages) instead of the opaque generic message
		// that discarded the `context deadline exceeded` cause (ka-0000001700).
		if errors.Is(err, context.DeadlineExceeded) {
			writeServerError(r.Context(), w, "handleReextractReferences", err, reextractTimeoutMessage)
			return
		}
		writeServerError(r.Context(), w, "handleReextractReferences", err, "failed to reextract references")
		return
	}
	writeJSON(w, http.StatusOK, &ReextractReferencesResponse{
		BeadsScanned:      stats.BeadsScanned,
		CommentsScanned:   stats.CommentsScanned,
		ReferencesWritten: stats.ReferencesWritten,
		LastID:            stats.LastID,
		Done:              stats.Done,
		TotalReferences:   stats.TotalReferences,
	})
}
