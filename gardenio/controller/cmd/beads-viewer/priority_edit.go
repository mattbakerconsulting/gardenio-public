package main

import (
	"net/http"
	"strconv"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// handlePriorityEdit returns the inline priority editor partial pre-filled
// with the bead's current priority. Anonymous viewers get 401 — the toolbar
// button is also gated, but the handler enforces the same rule
// defense-in-depth.
func (s *Server) handlePriorityEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	if oidcauth.EmailFromContext(r.Context()) == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.priorityLoadError(w, id, err)
		return
	}
	s.renderPriorityEdit(w, r, bead.ID, bead.Priority, "", http.StatusOK)
}

// handlePriorityView returns the read-only priority block — used by the
// editor's Cancel button to restore the original render without reloading
// the page.
func (s *Server) handlePriorityView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.priorityLoadError(w, id, err)
		return
	}
	s.renderPriorityBlock(w, r, bead, http.StatusOK)
}

// handleUpdatePriority persists a new priority via the kbeads update RPC. The
// form value `priority` must parse to an integer in [priorityMin, priorityMax];
// anything else re-renders the editor with the draft preserved + an inline
// error so the operator can correct without losing their input. On success,
// the read-only priority block is re-rendered so htmx can swap the editor
// back out.
func (s *Server) handleUpdatePriority(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.priorityLoadError(w, id, err)
		return
	}

	raw := r.PostForm.Get("priority")
	p, err := strconv.Atoi(raw)
	if err != nil || p < priorityMin || p > priorityMax {
		s.renderPriorityEdit(w, r, id, bead.Priority, "Priority must be an integer between 0 and 5.", http.StatusBadRequest)
		return
	}

	if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Priority: &p}); err != nil {
		s.logger.Error("updating priority", "id", id, "actor", email, "priority", p, "error", err)
		msg, status := daemonWriteFailure(err, "Failed to update priority.")
		s.renderPriorityEdit(w, r, id, p, msg, status)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after priority update", "id", id, "error", err)
		fallback := *bead
		fallback.Priority = p
		updated = &fallback
	}
	s.renderPriorityBlock(w, r, updated, http.StatusOK)
}

// priorityLoadError maps a daemon GetBead failure onto an HTTP response. A missing or
// out-of-scope bead (beadMissing) is a 404; everything else is a 500.
func (s *Server) priorityLoadError(w http.ResponseWriter, id string, err error) {
	if beadMissing(err) {
		http.Error(w, "Bead not found", http.StatusNotFound)
		return
	}
	s.logger.Error("getting bead for priority edit", "id", id, "error", err)
	http.Error(w, "Failed to load bead", http.StatusInternalServerError)
}

// renderPriorityBlock writes the read-only priority partial.
func (s *Server) renderPriorityBlock(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/priority_block.html", "priority_block", map[string]any{
		"Bead": bead,
	})
}

// renderPriorityEdit writes the editor partial, pre-filling the select with
// the bead's current priority (or the operator's in-flight draft on error
// paths). errMsg surfaces inline above the form.
func (s *Server) renderPriorityEdit(w http.ResponseWriter, r *http.Request, beadID string, priority int, errMsg string, status int) {
	if respondAction(w, r, beadID, actionFailure(errMsg, status)) {
		return
	}
	if status != http.StatusOK {
		writeInlineStatus(w, status)
	}
	s.renderPartial(w, r, "templates/partials/priority_edit.html", "priority_edit", map[string]any{
		"BeadID":          beadID,
		"Priority":        priority,
		"PriorityOptions": priorityOptions,
		"Error":           errMsg,
	})
}
