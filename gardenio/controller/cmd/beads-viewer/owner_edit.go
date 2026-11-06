package main

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxOwnerLen caps the raw owner form input the inline editor will accept.
// kbeads stores arbitrary strings, but the inline popover is intended for
// typical agent-name / email-shaped values — anything past this is almost
// certainly a paste mistake. Enforced server-side; oversized payloads
// re-render the editor with the draft preserved so the operator does not
// lose work.
const maxOwnerLen = 1024

// unownedSentinel is the explicit "no owner" value stored on the bead when
// an operator submits the Edit-owner form empty. Storing a non-empty
// sentinel prevents the server-side X-Operated-By default (ka-0000000269)
// from re-defaulting Owner on subsequent writes. Rendered as "(unowned)"
// by owner_block.html, distinct from the em-dash empty-state.
const unownedSentinel = "system:unowned"

// handleOwnerEdit returns the inline owner editor partial pre-filled with
// the bead's current owner. Anonymous viewers get 401 — the toolbar button
// is also gated, but the handler enforces the same rule defense-in-depth.
func (s *Server) handleOwnerEdit(w http.ResponseWriter, r *http.Request) {
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
		s.ownerLoadError(w, id, err)
		return
	}
	s.renderOwnerEdit(w, r, bead.ID, bead.Owner, "", http.StatusOK)
}

// handleOwnerView returns the read-only owner block — used by the editor's
// Cancel button to restore the original render without reloading the page.
func (s *Server) handleOwnerView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.ownerLoadError(w, id, err)
		return
	}
	s.renderOwnerBlock(w, r, bead, http.StatusOK)
}

// handleUpdateOwner persists a new owner via the kbeads update RPC. The form
// value `owner` is trimmed; an empty submission writes the explicit
// `system:unowned` sentinel so the server-side X-Operated-By default does
// not re-populate Owner on the next write. On success, the read-only owner
// block is re-rendered so htmx can swap the editor back out. Daemon failures
// re-render the editor with the draft preserved + an inline error so the
// operator can retry without losing their input.
func (s *Server) handleUpdateOwner(w http.ResponseWriter, r *http.Request) {
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
		s.ownerLoadError(w, id, err)
		return
	}

	raw := r.PostForm.Get("owner")
	if len(raw) > maxOwnerLen {
		s.renderOwnerEdit(w, r, id, raw, "Owner input is too long.", http.StatusRequestEntityTooLarge)
		return
	}
	owner := strings.TrimSpace(raw)
	if owner == "" {
		owner = unownedSentinel
	}

	if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Owner: &owner}); err != nil {
		s.logger.Error("updating owner", "id", id, "actor", email, "error", err)
		msg, status := daemonWriteFailure(err, "Failed to update owner.")
		s.renderOwnerEdit(w, r, id, raw, msg, status)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after owner update", "id", id, "error", err)
		fallback := *bead
		fallback.Owner = owner
		updated = &fallback
	}
	s.renderOwnerBlock(w, r, updated, http.StatusOK)
}

// ownerLoadError maps a daemon GetBead failure onto an HTTP response. A missing or
// out-of-scope bead (beadMissing) is a 404; everything else is a 500.
func (s *Server) ownerLoadError(w http.ResponseWriter, id string, err error) {
	if beadMissing(err) {
		http.Error(w, "Bead not found", http.StatusNotFound)
		return
	}
	s.logger.Error("getting bead for owner edit", "id", id, "error", err)
	http.Error(w, "Failed to load bead", http.StatusInternalServerError)
}

// renderOwnerBlock writes the read-only owner partial.
func (s *Server) renderOwnerBlock(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/owner_block.html", "owner_block", map[string]any{
		"Bead": bead,
	})
}

// renderOwnerEdit writes the editor partial, pre-filling the input with the
// bead's current owner (or the operator's in-flight draft on error paths).
// errMsg surfaces inline above the form.
func (s *Server) renderOwnerEdit(w http.ResponseWriter, r *http.Request, beadID, owner, errMsg string, status int) {
	if respondAction(w, r, beadID, actionFailure(errMsg, status)) {
		return
	}
	if status != http.StatusOK {
		writeInlineStatus(w, status)
	}
	s.renderPartial(w, r, "templates/partials/owner_edit.html", "owner_edit", map[string]any{
		"BeadID": beadID,
		"Owner":  owner,
		"Error":  errMsg,
	})
}
