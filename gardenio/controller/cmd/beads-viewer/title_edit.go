package main

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxTitleLen caps the title the inline editor will accept. kbeads itself
// stores arbitrarily long titles, but the bead-list and per-row renders trim
// at well under this — anything past this length is almost certainly a paste
// mistake and should round-trip via `kd update` instead. The limit is enforced
// server-side; an oversized payload re-renders the editor with the draft
// preserved so the operator does not lose work.
const maxTitleLen = 1024

// handleTitleEdit returns the inline editor partial pre-filled with the
// bead's current title. Anonymous viewers get 401 — the click trigger on the
// H1 is also gated, but the handler enforces the same rule defense-in-depth.
func (s *Server) handleTitleEdit(w http.ResponseWriter, r *http.Request) {
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
		s.titleLoadError(w, id, err)
		return
	}
	s.renderTitleEdit(w, r, bead, bead.Title, "", http.StatusOK)
}

// handleTitleView returns the read-only title block — used by the editor's
// Cancel button to restore the original render without reloading the page.
func (s *Server) handleTitleView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.titleLoadError(w, id, err)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	s.renderTitleBlock(w, r, bead, email, http.StatusOK)
}

// handleUpdateTitle persists a new title via the kbeads update RPC. On
// success, the read-only title block is re-rendered so htmx can swap the
// editor back out. Empty/whitespace input is rejected inline with the
// original title restored so the operator can correct without a stale draft
// hanging around. Daemon failures re-render the editor with the operator's
// draft preserved + an inline error so they can retry.
func (s *Server) handleUpdateTitle(w http.ResponseWriter, r *http.Request) {
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
		s.titleLoadError(w, id, err)
		return
	}

	raw := r.PostForm.Get("title")
	title := strings.TrimSpace(raw)
	if title == "" {
		// Empty input — restore the bead's original title in the form rather
		// than leaving the operator's whitespace draft visible.
		s.renderTitleEdit(w, r, bead, bead.Title, "Title cannot be empty.", http.StatusBadRequest)
		return
	}
	if len(title) > maxTitleLen {
		// Preserve the operator's draft so they can trim and retry.
		s.renderTitleEdit(w, r, bead, title, "Title is too long.", http.StatusRequestEntityTooLarge)
		return
	}

	if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Title: &title}); err != nil {
		s.logger.Error("updating title", "id", id, "actor", email, "error", err)
		// Preserve the draft so the operator can retry without losing text.
		msg, status := daemonWriteFailure(err, "Failed to update title.")
		s.renderTitleEdit(w, r, bead, title, msg, status)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after title update", "id", id, "error", err)
		fallback := *bead
		fallback.Title = title
		updated = &fallback
	}
	s.renderTitleBlock(w, r, updated, email, http.StatusOK)
}

// titleLoadError maps a daemon GetBead failure onto an HTTP response. A missing or
// out-of-scope bead (beadMissing) is a 404; everything else is a 500.
func (s *Server) titleLoadError(w http.ResponseWriter, id string, err error) {
	if beadMissing(err) {
		http.Error(w, "Bead not found", http.StatusNotFound)
		return
	}
	s.logger.Error("getting bead for title edit", "id", id, "error", err)
	http.Error(w, "Failed to load bead", http.StatusInternalServerError)
}

// renderTitleBlock writes the read-only title partial. Mirrors the linkified
// rendering in handleBeadShow so the swapped fragment matches the initial
// server render byte-for-byte.
func (s *Server) renderTitleBlock(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, email string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/title_block.html", "title_block", map[string]any{
		"Bead":      bead,
		"TitleHTML": LinkifyTextWithOpts(bead.Title, s.basePath, s.linkifyOptsForBead(r.Context(), bead)),
		"Actions":   buildBeadActions(bead, email, s.viewerPinnedSet(r.Context(), email)),
	})
}

// renderTitleEdit writes the editor partial, pre-filling the input with the
// supplied draft (the bead's current title on the open path, the operator's
// in-flight value on error paths). errMsg surfaces inline above the form.
func (s *Server) renderTitleEdit(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, draft, errMsg string, status int) {
	if respondAction(w, r, bead.ID, actionFailure(errMsg, status)) {
		return
	}
	if status != http.StatusOK {
		writeInlineStatus(w, status)
	}
	s.renderPartial(w, r, "templates/partials/title_edit.html", "title_edit", map[string]any{
		"BeadID": bead.ID,
		"Title":  draft,
		"Error":  errMsg,
	})
}
