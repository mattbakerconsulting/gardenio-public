package main

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxAssigneeLen caps the raw assignee form input the inline editor will
// accept. kbeads stores arbitrary strings, but the inline popover is intended
// for typical agent-name / email-shaped values — anything past this is
// almost certainly a paste mistake. Enforced server-side; oversized payloads
// re-render the editor with the draft preserved so the operator does not
// lose work.
const maxAssigneeLen = 1024

// handleAssigneeEdit returns the inline assignee editor partial pre-filled
// with the bead's current assignee. Anonymous viewers get 401 — the toolbar
// button is also gated, but the handler enforces the same rule
// defense-in-depth.
func (s *Server) handleAssigneeEdit(w http.ResponseWriter, r *http.Request) {
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
		s.assigneeLoadError(w, id, err)
		return
	}
	s.renderAssigneeEdit(w, r, bead.ID, bead.Assignee, "", http.StatusOK)
}

// handleAssigneeView returns the read-only assignee block — used by the
// editor's Cancel button to restore the original render without reloading
// the page.
func (s *Server) handleAssigneeView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.assigneeLoadError(w, id, err)
		return
	}
	s.renderAssigneeBlock(w, r, bead, http.StatusOK)
}

// handleUpdateAssignee persists a new assignee via the kbeads update RPC.
// The form value `assignee` is trimmed; an empty submission clears the
// assignee. Status is left as-is so the popover behaves like a pure
// assignee mutation. On success, the read-only assignee block is
// re-rendered so htmx can swap the editor back out. Daemon failures
// re-render the editor with the draft preserved + an inline error so the
// operator can retry without losing their input.
func (s *Server) handleUpdateAssignee(w http.ResponseWriter, r *http.Request) {
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
		s.assigneeLoadError(w, id, err)
		return
	}

	raw := r.PostForm.Get("assignee")
	if len(raw) > maxAssigneeLen {
		s.renderAssigneeEdit(w, r, id, raw, "Assignee input is too long.", http.StatusRequestEntityTooLarge)
		return
	}
	assignee := strings.TrimSpace(raw)

	if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Assignee: &assignee}); err != nil {
		s.logger.Error("updating assignee", "id", id, "actor", email, "assignee", assignee, "error", err)
		msg, status := daemonWriteFailure(err, "Failed to update assignee.")
		s.renderAssigneeEdit(w, r, id, raw, msg, status)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after assignee update", "id", id, "error", err)
		fallback := *bead
		fallback.Assignee = assignee
		updated = &fallback
	}
	s.renderAssigneeBlock(w, r, updated, http.StatusOK)
}

// assigneeLoadError maps a daemon GetBead failure onto an HTTP response. A missing or
// out-of-scope bead (beadMissing) is a 404; everything else is a 500.
func (s *Server) assigneeLoadError(w http.ResponseWriter, id string, err error) {
	if beadMissing(err) {
		http.Error(w, "Bead not found", http.StatusNotFound)
		return
	}
	s.logger.Error("getting bead for assignee edit", "id", id, "error", err)
	http.Error(w, "Failed to load bead", http.StatusInternalServerError)
}

// renderAssigneeBlock writes the read-only assignee partial.
func (s *Server) renderAssigneeBlock(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/assignee_block.html", "assignee_block", map[string]any{
		"Bead": bead,
	})
}

// renderAssigneeEdit writes the editor partial, pre-filling the input with
// the bead's current assignee (or the operator's in-flight draft on error
// paths). errMsg surfaces inline above the form.
func (s *Server) renderAssigneeEdit(w http.ResponseWriter, r *http.Request, beadID, assignee, errMsg string, status int) {
	if respondAction(w, r, beadID, actionFailure(errMsg, status)) {
		return
	}
	if status != http.StatusOK {
		writeInlineStatus(w, status)
	}
	s.renderPartial(w, r, "templates/partials/assignee_edit.html", "assignee_edit", map[string]any{
		"BeadID":   beadID,
		"Assignee": assignee,
		"Error":    errMsg,
	})
}
