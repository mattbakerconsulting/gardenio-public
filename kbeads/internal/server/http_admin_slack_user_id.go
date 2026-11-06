package server

import (
	"net/http"

	"gardenio/kbeads/internal/store"
)

// BackfillSlackUserIDRequest is the JSON body for
// POST /v1/admin/backfill-slack-user-id. Drives one run of ka-0000001582's
// thread-derived slack_user_id backfill.
type BackfillSlackUserIDRequest struct {
	Statuses  []string `json:"statuses,omitempty"`
	BatchSize int      `json:"batch_size,omitempty"`
	DryRun    bool     `json:"dry_run,omitempty"`
}

// BackfillSlackUserIDResponse mirrors store.BackfillSlackUserIDStats over
// the wire.
type BackfillSlackUserIDResponse struct {
	DryRun               bool  `json:"dry_run"`
	BeforeMissing        int64 `json:"before_missing"`
	AfterMissing         int64 `json:"after_missing"`
	Updated              int64 `json:"updated"`
	ResolvedViaThreadID  int64 `json:"resolved_via_thread_id"`
	ResolvedViaCreatedBy int64 `json:"resolved_via_created_by"`
	Unresolved           int64 `json:"unresolved"`
	DurationMs           int64 `json:"duration_ms"`
}

// handleBackfillSlackUserID handles
// POST /v1/admin/backfill-slack-user-id. Admin-only.
func (s *BeadsServer) handleBackfillSlackUserID(w http.ResponseWriter, r *http.Request) {
	var req BackfillSlackUserIDRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillSlackUserID(r.Context(), store.BackfillSlackUserIDOptions{
		Statuses:  req.Statuses,
		BatchSize: req.BatchSize,
		DryRun:    req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillSlackUserID", err, "failed to backfill slack_user_id")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillSlackUserIDResponse{
		DryRun:               stats.DryRun,
		BeforeMissing:        stats.BeforeMissing,
		AfterMissing:         stats.AfterMissing,
		Updated:              stats.Updated,
		ResolvedViaThreadID:  stats.ResolvedViaThreadID,
		ResolvedViaCreatedBy: stats.ResolvedViaCreatedBy,
		Unresolved:           stats.Unresolved,
		DurationMs:           stats.DurationMs,
	})
}
