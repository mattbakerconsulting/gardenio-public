package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
)

// handleUndoMutation handles POST /v1/mutations/{id}/undo. Reverses one
// mutation_history row by applying the stored old_value back to the bead.
// Admin-only; non-admin OIDC callers are rejected with a 403 and an audit
// bead is written. Static-token callers (service-to-service) bypass the role
// check, mirroring the rest of the admin surface.
func (s *BeadsServer) handleUndoMutation(w http.ResponseWriter, r *http.Request) {
	if denied := s.guardUndoAdmin(r); denied {
		writeError(w, http.StatusForbidden, "insufficient permissions: requires "+RoleAdmin)
		return
	}

	idStr := r.PathValue("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a numeric mutation_history row id")
		return
	}

	bead, mut, err := s.undoMutation(r.Context(), id, DefaultUndoRetentionDays)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "mutation row not found")
		return
	case errors.Is(err, ErrUndoUnsupportedMutation), errors.Is(err, ErrUndoUnsupportedField):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ErrUndoOutOfWindow):
		writeError(w, http.StatusGone, err.Error())
		return
	case errors.Is(err, ErrUndoConflict):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		// Restored arms go back through the register and the R6 type rule
		// (heraldry.go): another bead may have taken them since, or the bead
		// may no longer be a kind that bears arms.
		if hc, ok := asHeraldryConflict(err); ok {
			writeHeraldryConflict(w, hc)
			return
		}
		var ie inputError
		if errors.As(err, &ie) {
			writeError(w, http.StatusBadRequest, ie.Error())
			return
		}
		writeServerError(r.Context(), w, "handleUndoMutation", err, "failed to undo mutation")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"undone": mut,
		"bead":   bead,
	})
}

// guardUndoAdmin returns true when the caller is an OIDC user without the
// admin role. Mirrors requireRole(RoleAdmin, ...) but writes an audit bead
// before rejecting so the attempt is recorded — the acceptance criteria for
// step 8c demands an audit row on denial.
func (s *BeadsServer) guardUndoAdmin(r *http.Request) bool {
	if !roleBearing(authMethodFromContext(r.Context())) {
		return false
	}
	roles := RolesFromContext(r.Context())
	if hasMinimumRole(roles, RoleAdmin) {
		return false
	}
	id := AuthIdentityFromContext(r.Context())
	s.recordAuditEntry(r.Context(), auditEntry{
		Actor:      id.Subject,
		Action:     "undo",
		Resource:   r.PathValue("id"),
		Outcome:    "deny",
		Reason:     "insufficient permissions: requires " + RoleAdmin,
		AuthMethod: id.Type,
		SourceIP:   clientIP(r, s.trustedProxies),
	})
	return true
}
