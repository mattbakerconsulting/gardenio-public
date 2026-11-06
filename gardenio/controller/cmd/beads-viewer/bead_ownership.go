package main

import (
	"html/template"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// ownershipRow is the per-row view-model rendered by bead_ownership.html.
// One row per `bead_operator_history` VIEW row. The view classifies each
// transition with a `source_kind` ("create" / "refresh" / "sentinel-attempt"
// / "unknown-attempt" / "dibs-flip") that the template uses to decide the
// row tone — sentinel-styled rows fade the chip per R3 §3.
type ownershipRow struct {
	ID               int64
	OperatorChipHTML template.HTML // produced by renderOperatorChip
	OperatorRaw      string
	Actor            string
	PrevOperator     string
	SourceKind       string
	IsSentinel       bool
	CreatedAt        time.Time
}

// handleBeadOwnership renders the per-bead ownership-timeline page (R5 §2
// row S4). Lists rows from `bead_operator_history` newest-first and links
// each operator id back to the operator-chip's filter view.
//
// Calls GetBead first so a 403/404 from read-authz short-circuits before we
// hit /v1/beads/{id}/operator-history (read-parity, mirroring the activity
// handler).
func (s *Server) handleBeadOwnership(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for ownership", "id", id, "error", err)
		http.Error(w, "Failed to get bead", http.StatusInternalServerError)
		return
	}
	rows, err := s.daemon.GetOperatorHistory(r.Context(), id)
	if err != nil {
		s.logger.Warn("getting operator history", "id", id, "error", err)
		rows = nil
	}
	email := oidcauth.EmailFromContext(r.Context())
	views := buildOwnershipRows(rows, bead, s.basePath, email)
	data := map[string]any{
		"Bead":          bead,
		"Ownership":     views,
		"CurrentBeadID": bead.ID,
		"AuthorEmail":   email,
		"ViewerEmail":   email,
		"Project":       projectFromLabels(bead.Labels),
	}
	s.render(w, r, "bead_ownership.html", data)
}

// buildOwnershipRows materializes one ownershipRow per VIEW row. The
// operator chip is resolved using the same helper as the bead-detail header
// so renderings stay consistent — but a sentinel/unknown-attempt source_kind
// always renders the row with the sentinel tone regardless of operator id.
func buildOwnershipRows(rows []beadsapi.OperatorHistoryRow, bead *beadsapi.BeadDetail, basePath, viewerEmail string) []ownershipRow {
	if len(rows) == 0 {
		return nil
	}
	createdBy := ""
	if bead != nil {
		createdBy = bead.CreatedBy
	}
	out := make([]ownershipRow, 0, len(rows))
	for _, r := range rows {
		kind := strings.TrimSpace(r.SourceKind)
		isSentinel := kind == "sentinel-attempt" || kind == "unknown-attempt"
		chip := resolveOperatorChip(r.OperatorID, "", "", "", createdBy, basePath, viewerEmail)
		out = append(out, ownershipRow{
			ID:               r.ID,
			OperatorChipHTML: renderOperatorChip(chip),
			OperatorRaw:      r.OperatorID,
			Actor:            r.Actor,
			PrevOperator:     r.PrevOperator,
			SourceKind:       kind,
			IsSentinel:       isSentinel,
			CreatedAt:        r.BecameOwnerAt,
		})
	}
	return out
}
