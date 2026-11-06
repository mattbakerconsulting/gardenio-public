package server

import (
	"net/http"

	"gardenio/kbeads/internal/store"
)

// BackfillOperatorsRequest is the JSON body for
// POST /v1/admin/backfill-operators. The CLI driver fires one POST per
// run; the server does the full backfill (Pass 1 + Pass 2 + verifier)
// inline.
type BackfillOperatorsRequest struct {
	DryRun    bool `json:"dry_run"`
	BatchSize int  `json:"batch_size,omitempty"`
}

// BackfillOperatorsResponse mirrors store.BackfillOperatorsStats over the
// wire. Per-table fields are row counts: rows_updated for real runs, the
// would-be-updated row count for dry runs. Verifier fields report rows
// where operated_by TEXT is populated but operated_by_id FK is still NULL
// (must be zero on success modulo Quarantined).
type BackfillOperatorsResponse struct {
	DistinctOperatedBy    int      `json:"distinct_operated_by"`
	OperatorsUpserted     int      `json:"operators_upserted"`
	AliasesUpserted       int      `json:"aliases_upserted"`
	BeadsUpdated          int64    `json:"beads_updated"`
	CommentsUpdated       int64    `json:"comments_updated"`
	DepsUpdated           int64    `json:"deps_updated"`
	EventsUpdated         int64    `json:"events_updated"`
	MutationsUpdated      int64    `json:"mutations_updated"`
	BeadsEmptySentinel    int64    `json:"beads_empty_sentinel"`
	CommentsEmptySentinel int64    `json:"comments_empty_sentinel"`
	DepsEmptySentinel     int64    `json:"deps_empty_sentinel"`
	QuarantinedOperatedBy []string `json:"quarantined_operated_by,omitempty"`
	VerifierBeads         int64    `json:"verifier_beads"`
	VerifierComments      int64    `json:"verifier_comments"`
	VerifierDeps          int64    `json:"verifier_deps"`
	VerifierEvents        int64    `json:"verifier_events"`
	VerifierMutations     int64    `json:"verifier_mutations"`
	DryRun                bool     `json:"dry_run"`
	DurationMs            int64    `json:"duration_ms"`
}

// handleBackfillOperators handles POST /v1/admin/backfill-operators.
// Admin-only. Drives one run of the operators-FK backfill (P3); see
// docs/adr/human-attribution-identity-v2.md §4.3 / §4.5 / §4.6.
func (s *BeadsServer) handleBackfillOperators(w http.ResponseWriter, r *http.Request) {
	var req BackfillOperatorsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillOperators(r.Context(), store.BackfillOperatorsOptions{
		DryRun:    req.DryRun,
		BatchSize: req.BatchSize,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillOperators", err, "failed to backfill operators")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillOperatorsResponse{
		DistinctOperatedBy:    stats.DistinctOperatedBy,
		OperatorsUpserted:     stats.OperatorsUpserted,
		AliasesUpserted:       stats.AliasesUpserted,
		BeadsUpdated:          stats.BeadsUpdated,
		CommentsUpdated:       stats.CommentsUpdated,
		DepsUpdated:           stats.DepsUpdated,
		EventsUpdated:         stats.EventsUpdated,
		MutationsUpdated:      stats.MutationsUpdated,
		BeadsEmptySentinel:    stats.BeadsEmptySentinel,
		CommentsEmptySentinel: stats.CommentsEmptySentinel,
		DepsEmptySentinel:     stats.DepsEmptySentinel,
		QuarantinedOperatedBy: stats.QuarantinedOperatedBy,
		VerifierBeads:         stats.VerifierBeads,
		VerifierComments:      stats.VerifierComments,
		VerifierDeps:          stats.VerifierDeps,
		VerifierEvents:        stats.VerifierEvents,
		VerifierMutations:     stats.VerifierMutations,
		DryRun:                stats.DryRun,
		DurationMs:            stats.DurationMs,
	})
}
