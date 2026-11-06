package main

import (
	"bytes"
	"context"
	"html/template"
	"net/http"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxDescriptionLen caps the description body the inline editor will accept.
// kbeads itself stores arbitrarily large descriptions, but the inline popover
// is intended for typical issue bodies — anything past this is almost certainly
// a paste mistake and should round-trip via `kd update` instead. The limit is
// enforced server-side; an oversized payload re-renders the editor with the
// draft preserved so the operator does not lose work.
const maxDescriptionLen = 64 * 1024

// handleDescriptionEdit returns the inline editor partial pre-filled with the
// bead's current description. Anonymous viewers get 401 — the toolbar button
// is also gated, but the handler enforces the same rule defense-in-depth.
func (s *Server) handleDescriptionEdit(w http.ResponseWriter, r *http.Request) {
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
		s.descriptionLoadError(w, id, err)
		return
	}
	s.renderDescriptionEdit(w, r, bead, "", http.StatusOK)
}

// handleDescriptionView returns the read-only description block — used by the
// editor's Cancel button to restore the original render without reloading the
// page.
func (s *Server) handleDescriptionView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.descriptionLoadError(w, id, err)
		return
	}
	s.renderDescriptionBlock(w, r, bead, http.StatusOK)
}

// handleDescriptionPreview renders a markdown preview for the operator's
// in-flight draft, reusing the same goldmark + LinkifyHTML pipeline that
// produces the saved view (renderDescriptionBlock). Used by the description
// editor's debounced htmx POST so the preview pane matches the eventual
// saved render byte-for-byte. Anonymous viewers get 401 — the editor is
// itself gated behind the same role.
func (s *Server) handleDescriptionPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	if oidcauth.EmailFromContext(r.Context()) == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	desc := r.PostForm.Get("description")
	if len(desc) > maxDescriptionLen {
		// Mirror the save-path cap rather than rendering an oversized preview;
		// the editor's save button will surface the same error.
		http.Error(w, "Description is too long.", http.StatusRequestEntityTooLarge)
		return
	}
	// Best-effort bead load: the preview must resolve `!N` MR refs against the
	// same project as the saved view, and the project comes off the bead's
	// labels. A load failure degrades to default linkification rather than
	// failing the preview.
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Debug("getting bead for description preview", "id", id, "error", err)
		bead = nil
	}
	s.renderDescriptionPreview(w, r, bead, desc, http.StatusOK)
}

// handleUpdateDescription persists a new description via the kbeads update
// RPC. On success, the read-only description block is re-rendered so htmx can
// swap the editor back out. Daemon failures re-render the editor with the
// draft preserved + an inline error so the operator can retry without losing
// their text.
func (s *Server) handleUpdateDescription(w http.ResponseWriter, r *http.Request) {
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
		s.descriptionLoadError(w, id, err)
		return
	}

	desc := r.PostForm.Get("description")
	if len(desc) > maxDescriptionLen {
		// Preserve the user's draft on the editor so they can trim and retry.
		copyForDraft := *bead
		copyForDraft.Description = desc
		s.renderDescriptionEdit(w, r, &copyForDraft, "Description is too long.", http.StatusRequestEntityTooLarge)
		return
	}

	if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Description: &desc}); err != nil {
		s.logger.Error("updating description", "id", id, "actor", email, "error", err)
		// Preserve the draft so the operator can retry without losing text.
		draft := *bead
		draft.Description = desc
		msg, status := daemonWriteFailure(err, "Failed to update description.")
		s.renderDescriptionEdit(w, r, &draft, msg, status)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after description update", "id", id, "error", err)
		fallback := *bead
		fallback.Description = desc
		updated = &fallback
	}
	s.renderDescriptionBlock(w, r, updated, http.StatusOK)
}

// descriptionLoadError maps a daemon GetBead failure onto an HTTP response. A missing or
// out-of-scope bead (beadMissing) is a 404; everything else is a 500.
func (s *Server) descriptionLoadError(w http.ResponseWriter, id string, err error) {
	if beadMissing(err) {
		http.Error(w, "Bead not found", http.StatusNotFound)
		return
	}
	s.logger.Error("getting bead for description edit", "id", id, "error", err)
	http.Error(w, "Failed to load bead", http.StatusInternalServerError)
}

// renderDescriptionBlock writes the read-only description partial. Mirrors the
// markdown rendering in handleBeadShow so the swapped fragment matches the
// initial server render byte-for-byte.
func (s *Server) renderDescriptionBlock(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/description_block.html", "description_block", map[string]any{
		"Bead":            bead,
		"DescriptionHTML": s.renderMarkdownDescriptionForBead(r.Context(), bead, bead.Description, nil),
	})
}

// renderDescriptionEdit writes the editor partial, pre-filling the textarea
// with the bead's current description (or the operator's in-flight draft on
// error paths). errMsg surfaces inline above the form. The initial preview
// pane is rendered server-side from the same description so the editor opens
// with a representative render before the operator types.
func (s *Server) renderDescriptionEdit(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, errMsg string, status int) {
	if respondAction(w, r, bead.ID, actionFailure(errMsg, status)) {
		return
	}
	if status != http.StatusOK {
		writeInlineStatus(w, status)
	}
	s.renderPartial(w, r, "templates/partials/description_edit.html", "description_edit", map[string]any{
		"BeadID":          bead.ID,
		"Description":     bead.Description,
		"DescriptionHTML": s.renderMarkdownDescriptionForBead(r.Context(), bead, bead.Description, nil),
		"Error":           errMsg,
	})
}

// renderDescriptionPreview writes the preview partial standalone — used by the
// debounced htmx POST during edit. Same pipeline as renderDescriptionBlock so
// the preview matches the saved view byte-for-byte.
func (s *Server) renderDescriptionPreview(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, draft string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/description_preview.html", "description_preview", map[string]any{
		"DescriptionHTML": s.renderMarkdownDescriptionForBead(r.Context(), bead, draft, nil),
	})
}

// renderMarkdownDescriptionForBead is the shared markdown render path used by
// the read-only description block, the editor's preview pane, and the plan
// narrative. Keeping it in one place guarantees the preview matches the saved
// view (ka-XXX linkify, code highlight, GFM features). On render failure the
// rendered HTML is empty so the template falls back to the empty placeholder.
//
// The bead's project-resolved linkifier options are applied so a bare `!N` MR
// ref in a swapped htmx fragment points at the same repo the full-page render
// chose; without it every partial fell back to the process-wide default GitLab
// project and could contradict the initial render (ka-0000007929). A nil bead
// degrades to that default.
//
// typedByID maps target bead ID → typed-mention verb for typed mentions
// found in the description field; matching anchors get
// data-relationship-type=<verb> so the highlight script (E2 — ka-0000000608)
// can prefer the typed match when ?rel=<verb> is in the URL. Pass nil for
// edit/preview paths where the daemon hasn't re-extracted yet — the
// highlight script falls back to "any match" gracefully.
func (s *Server) renderMarkdownDescriptionForBead(ctx context.Context, bead *beadsapi.BeadDetail, text string, typedByID map[string]string) template.HTML {
	if text == "" {
		return ""
	}
	beadID := ""
	if bead != nil {
		beadID = bead.ID
	}
	var buf bytes.Buffer
	if err := s.md.Convert([]byte(text), &buf); err != nil {
		s.logger.Warn("rendering markdown", "id", beadID, "error", err)
		return ""
	}
	return LinkifyDescriptionHTMLWithOpts(buf.String(), s.basePath, typedByID, s.linkifyOptsForBead(ctx, bead))
}
