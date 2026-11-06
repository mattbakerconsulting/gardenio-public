package main

import (
	"fmt"
	"net/http"
	"strconv"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/steward"
)

// maxBulkTriggers caps how many trigger beads one bulk-toggle request will
// touch. Same defense-in-depth motivation as the project bulk cap — keeps a
// runaway form post from fanning out into hundreds of daemon RPCs.
const maxBulkTriggers = 200

// handleTriggersBulkEnable flips `enabled: true` across every bead listed
// in the `bead_id` form values. Mirrors the single-bead toggle from (c):
// per-bead UpdateBeadFieldsTyped + AddComment, then re-render every affected
// row via OOB swap so the UI reflects the new state without a refresh.
func (s *Server) handleTriggersBulkEnable(w http.ResponseWriter, r *http.Request) {
	s.handleBulkToggleTriggers(w, r, true)
}

// handleTriggersBulkDisable is the symmetric counterpart for `enabled: false`.
// Disable is NOT close — closed beads are filtered out at the per-bead guard
// and the bar action does not touch bead status.
func (s *Server) handleTriggersBulkDisable(w http.ResponseWriter, r *http.Request) {
	s.handleBulkToggleTriggers(w, r, false)
}

// handleBulkToggleTriggers is the shared implementation for the two bulk
// endpoints. We deliberately write an explicit JSON bool in both directions
// to match the asymmetric loader defaults documented in trigger_toggle.go
// (legacy schedule vs trigger:schedule); a string "true"/"false" would be
// 400-rejected by the daemon's boolean field validation (ka-0000003627).
//
// Partial-failure policy: no rollback. Successful beads are kept committed,
// failures are logged and surface as an inline error on the re-rendered
// toolbar. Each affected bead receives one audit comment via AddComment
// (best-effort — the field write is the canonical record).
func (s *Server) handleBulkToggleTriggers(w http.ResponseWriter, r *http.Request, enable bool) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	ids := dedupeNonEmpty(r.PostForm["bead_id"])
	if len(ids) == 0 {
		s.renderTriggersBulkBar(w, r, "Select at least one trigger.", nil, http.StatusBadRequest)
		return
	}
	if len(ids) > maxBulkTriggers {
		s.renderTriggersBulkBar(w, r, fmt.Sprintf("Too many triggers (max %d).", maxBulkTriggers), nil, http.StatusRequestEntityTooLarge)
		return
	}

	next := strconv.FormatBool(enable)
	verb := "disable"
	if enable {
		verb = "enable"
	}

	updated := make([]string, 0, len(ids))
	var firstErr string
	for _, id := range ids {
		bead, err := s.daemon.GetBead(r.Context(), id)
		if err != nil {
			s.logger.Error("bulk trigger toggle: load", "id", id, "actor", email, "verb", verb, "error", err)
			if firstErr == "" {
				firstErr = fmt.Sprintf("Failed to load %s.", id)
			}
			continue
		}
		// Guard: the same targets as the single-row toggle
		// (triggerToggleTarget: schedule, trigger:schedule, event-mode
		// gardeners). A stray non-trigger id in the form should not silently
		// mutate.
		if !triggerToggleTarget(bead) {
			s.logger.Warn("bulk trigger toggle: non-trigger bead skipped", "id", id, "type", bead.Type, "actor", email)
			if firstErr == "" {
				firstErr = fmt.Sprintf("%s is not a trigger.", id)
			}
			continue
		}
		// A land steward (trigger:multi + the steward marker) is a gardener, so
		// its own row toggle works, but no bulk path has ever reached it and a
		// "disable everything" sweep should not silently park a land's steward.
		if isStewardTrigger(bead) {
			s.logger.Warn("bulk trigger toggle: steward skipped", "id", id, "actor", email)
			if firstErr == "" {
				firstErr = fmt.Sprintf("%s is a land steward; toggle it on its own row.", id)
			}
			continue
		}
		// Closed beads don't render the toggle; reject them rather than
		// silently flipping a parked bead.
		if statusOpenSortKey(bead.Status) != 0 {
			s.logger.Warn("bulk trigger toggle: closed bead skipped", "id", id, "actor", email)
			if firstErr == "" {
				firstErr = fmt.Sprintf("%s is closed.", id)
			}
			continue
		}
		if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), id, map[string]any{"enabled": enable}); err != nil {
			s.logger.Error("bulk trigger toggle: update", "id", id, "actor", email, "next", next, "error", err)
			if firstErr == "" {
				firstErr = fmt.Sprintf("Failed to update %s.", id)
			}
			continue
		}
		auditText := fmt.Sprintf("enabled → %s by %s", next, email)
		if err := s.daemon.AddComment(r.Context(), id, email, auditText); err != nil {
			s.logger.Warn("bulk trigger toggle: audit comment", "id", id, "actor", email, "next", next, "error", err)
		}
		updated = append(updated, id)
	}

	httpStatus := http.StatusOK
	if firstErr != "" && len(updated) == 0 {
		httpStatus = http.StatusBadGateway
	}
	s.renderTriggersBulkBar(w, r, firstErr, updated, httpStatus)
}

// isStewardTrigger reports whether b is a land steward's trigger
// (steward.Bind: type=trigger, trigger:multi, the steward marker label,
// perpetual:true). The bulk toggles skip it; see handleBulkToggleTriggers.
func isStewardTrigger(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Type == "trigger" && hasLabel(b, "trigger:multi") && hasLabel(b, steward.MarkerLabel)
}

// renderTriggersBulkBar writes the cleared bulk toolbar plus OOB trigger_row
// partials for every successfully-updated bead. The toolbar is the htmx
// target (#triggers-bulk-bar); each row partial OOB-swaps into its own
// #trigger-row-{id}.
func (s *Server) renderTriggersBulkBar(w http.ResponseWriter, r *http.Request, errMsg string, updatedIDs []string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Render the cleared toolbar (count=0, no errors unless set). Lives at
	// id="triggers-bulk-bar" so htmx outerHTML swap drops it back into place.
	// Clone (via bindAgentLink) before executing: the parsed partials are
	// shared across Server instances (loadTemplateAssets), and html/template
	// forbids Clone after a direct execute.
	toolbarTmpl, ok := s.partials["templates/partials/trigger_bulk_toolbar.html"]
	if !ok {
		s.logger.Error("trigger_bulk_toolbar partial not registered")
		return
	}
	boundToolbar, err := s.bindAgentLink(toolbarTmpl, r)
	if err != nil {
		s.logger.Error("trigger_bulk_toolbar bind", "error", err)
		return
	}
	toolbarData := map[string]any{
		"BasePath": s.basePath,
		"ErrMsg":   errMsg,
	}
	if err := boundToolbar.ExecuteTemplate(w, "trigger_bulk_toolbar", toolbarData); err != nil {
		s.logger.Error("trigger_bulk_toolbar render", "error", err)
		return
	}

	// OOB row partials for every successfully-updated bead. Re-fetch each
	// bead so the row reflects the new enabled state. Per-bead failure is
	// logged and silently dropped — the operator can refresh to recover.
	rowTmpl, ok := s.partials["templates/partials/trigger_row.html"]
	if !ok {
		s.logger.Error("trigger_row partial not registered")
		return
	}
	boundRow, err := s.bindAgentLink(rowTmpl, r)
	if err != nil {
		s.logger.Error("trigger_row bind", "error", err)
		return
	}
	for _, id := range updatedIDs {
		bead, err := s.daemon.GetBead(r.Context(), id)
		if err != nil {
			s.logger.Warn("re-fetching bead for OOB trigger row", "id", id, "error", err)
			continue
		}
		row := buildTriggerRow(bead, s.now())
		data := map[string]any{
			"Row":      row,
			"BasePath": s.basePath,
			"OOB":      true,
		}
		if err := boundRow.ExecuteTemplate(w, "trigger_row", data); err != nil {
			s.logger.Error("trigger_row OOB render", "id", id, "error", err)
		}
	}
}
