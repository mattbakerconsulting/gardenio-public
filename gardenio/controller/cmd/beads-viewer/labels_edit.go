package main

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxLabelsLen caps the raw labels form input the inline editor will accept.
// kbeads itself stores arbitrarily many labels, but the inline popover is
// intended for typical label sets — anything past this is almost certainly a
// paste mistake. The limit is enforced server-side; an oversized payload
// re-renders the editor with the draft preserved so the operator does not
// lose work.
const maxLabelsLen = 4 * 1024

// handleLabelsEdit returns the inline labels editor partial pre-filled with
// the bead's current label set joined by ", ". Anonymous viewers get 401 —
// the toolbar button is also gated, but the handler enforces the same rule
// defense-in-depth.
func (s *Server) handleLabelsEdit(w http.ResponseWriter, r *http.Request) {
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
		s.labelsLoadError(w, id, err)
		return
	}
	s.renderLabelsEdit(w, r, bead.ID, joinLabels(bead.Labels), "", http.StatusOK)
}

// handleLabelsView returns the read-only labels block — used by the editor's
// Cancel button to restore the original render without reloading the page.
func (s *Server) handleLabelsView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.labelsLoadError(w, id, err)
		return
	}
	s.renderLabelsBlock(w, r, bead, http.StatusOK)
}

// handleUpdateLabels persists a new label set via the kbeads update RPC. The
// form value `labels` is parsed as a comma-separated list; whitespace per
// token is trimmed, duplicates are dropped, and an empty submission clears
// all labels. On success, the read-only labels block is re-rendered so htmx
// can swap the editor back out. Daemon failures re-render the editor with
// the draft preserved + an inline error so the operator can retry without
// losing their input.
func (s *Server) handleUpdateLabels(w http.ResponseWriter, r *http.Request) {
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
		s.labelsLoadError(w, id, err)
		return
	}

	raw := r.PostForm.Get("labels")
	if len(raw) > maxLabelsLen {
		s.renderLabelsEdit(w, r, id, raw, "Labels input is too long.", http.StatusRequestEntityTooLarge)
		return
	}
	labels := parseLabelsInput(raw)

	if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Labels: &labels}); err != nil {
		s.logger.Error("updating labels", "id", id, "actor", email, "error", err)
		msg, status := daemonWriteFailure(err, "Failed to update labels.")
		s.renderLabelsEdit(w, r, id, raw, msg, status)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after labels update", "id", id, "error", err)
		fallback := *bead
		fallback.Labels = labels
		updated = &fallback
	}
	s.renderLabelsBlock(w, r, updated, http.StatusOK)
}

// labelsLoadError maps a daemon GetBead failure onto an HTTP response. A missing or
// out-of-scope bead (beadMissing) is a 404; everything else is a 500.
func (s *Server) labelsLoadError(w http.ResponseWriter, id string, err error) {
	if beadMissing(err) {
		http.Error(w, "Bead not found", http.StatusNotFound)
		return
	}
	s.logger.Error("getting bead for labels edit", "id", id, "error", err)
	http.Error(w, "Failed to load bead", http.StatusInternalServerError)
}

// renderLabelsBlock writes the read-only labels partial.
func (s *Server) renderLabelsBlock(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/labels_block.html", "labels_block", map[string]any{
		"Bead": bead,
	})
}

// renderLabelsEdit writes the editor partial, pre-filling the input with the
// bead's current labels (or the operator's in-flight draft on error paths).
// errMsg surfaces inline above the form.
func (s *Server) renderLabelsEdit(w http.ResponseWriter, r *http.Request, beadID, labels, errMsg string, status int) {
	if respondAction(w, r, beadID, actionFailure(errMsg, status)) {
		return
	}
	if status != http.StatusOK {
		writeInlineStatus(w, status)
	}
	s.renderPartial(w, r, "templates/partials/labels_edit.html", "labels_edit", map[string]any{
		"BeadID": beadID,
		"Labels": labels,
		"Error":  errMsg,
	})
}

// parseLabelsInput parses a comma-separated label string into a deduped slice
// in input order. Whitespace per token is trimmed; empty tokens are dropped.
// Returns a non-nil empty slice for empty/whitespace-only input so callers
// can distinguish "clear all" from "leave alone" via pointer-vs-nil.
func parseLabelsInput(raw string) []string {
	out := []string{}
	seen := make(map[string]struct{})
	for tok := range strings.SplitSeq(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	return out
}

// joinLabels renders a label slice as the comma-separated form the editor
// pre-fills. Empty slices return "" so the input is blank.
func joinLabels(labels []string) string {
	return strings.Join(labels, ", ")
}
