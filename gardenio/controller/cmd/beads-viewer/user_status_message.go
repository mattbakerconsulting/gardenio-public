package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// maxStatusMessageLen caps the operator-supplied status_message. The field
// is a Slack-style one-liner shown on the profile card; 256 chars keeps room
// for an emoji + sentence without inviting accidental paragraph dumps.
const maxStatusMessageLen = 256

// lookupUserBeadByOperator resolves an operator id to their type=user
// kind=config bead. Returns (nil, nil) when no match — operators without a
// provisioned user bead simply have no profile fields yet.
//
// User beads are matched on two keys, in order: the `operator_id` field, then
// the operator's `email`. The email fallback is load-bearing: in practice no
// user bead carries an `operator_id` field (they are provisioned with `email`,
// `slack_id`, etc.), while the write paths that persist profile prefs and the
// status message resolve the user bead by the viewer's OIDC email
// (findUserBeadByEmail). Matching on operator_id alone left the read path
// returning nil for every real operator, so pinned beads and status messages
// were written but never surfaced on the home page (ka-0000001751). The email
// fallback bridges the operator-id-keyed read to the email-keyed write.
func (s *Server) lookupUserBeadByOperator(ctx context.Context, operatorID int64) (*beadsapi.BeadDetail, error) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"user"},
		Kinds: []string{"config"},
		Limit: 500,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	target := itoa64(operatorID)
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if strings.TrimSpace(b.Fields["operator_id"]) == target {
			return b, nil
		}
	}
	// Fallback: match by the operator's email. Resolve operatorID → email via
	// the operator directory, then scan the same user beads for a matching
	// `email` field. This is the common path, since user beads carry email but
	// not operator_id.
	email := s.emailForOperator(ctx, operatorID)
	if email == "" {
		return nil, nil
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(b.Fields["email"]), email) {
			return b, nil
		}
	}
	return nil, nil
}

// emailForOperator resolves an operator id to its directory email. Returns ""
// on lookup failure or when the operator has no email — callers treat that as
// "no user bead match" rather than an error, keeping the home page rendering.
func (s *Server) emailForOperator(ctx context.Context, operatorID int64) string {
	resp, err := s.daemon.ListOperators(ctx, 0)
	if err != nil {
		s.logger.Warn("lookup user bead: list operators for email fallback",
			"operator_id", operatorID, "error", err)
		return ""
	}
	if resp == nil {
		return ""
	}
	for _, o := range resp.Operators {
		if o.ID == operatorID {
			return strings.TrimSpace(o.Email)
		}
	}
	return ""
}

// statusMessageFromBead returns the trimmed status_message field, or "" when
// the bead is nil or carries no value.
func statusMessageFromBead(bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	return strings.TrimSpace(bead.Fields["status_message"])
}

// displayNameFromUserBead resolves the operator's human-facing name from their
// user bead, mirroring the gardener_owner.go / settings resolution order:
// display_name → name → email. Returns "" when the bead is nil or carries no
// name field, leaving the caller to fall back to the slug.
func displayNameFromUserBead(bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	return firstNonEmptyField(bead.Fields, "display_name", "name", "email")
}

// resolveUserHomeViewer returns the operator id resolved from {slug} plus
// whether the OIDC viewer is the same operator (the self-only gate). The
// handler is responsible for turning a missing slug or daemon failure into the
// right HTTP status; this helper just routes the inputs.
func (s *Server) resolveUserHomeViewer(r *http.Request, slug string) (operatorID int64, isSelf bool, status int, err error) {
	ctx := r.Context()
	resp, lerr := s.daemon.LookupOperatorBySlug(ctx, slug)
	if lerr != nil {
		return 0, false, http.StatusInternalServerError, lerr
	}
	if !resp.Found {
		return 0, false, http.StatusNotFound, errors.New("not found")
	}
	if s.resolver == nil {
		return resp.OperatorID, false, http.StatusOK, nil
	}
	vid, found, lerr := s.resolver.OperatorID(ctx)
	if lerr == nil && found && vid == resp.OperatorID {
		return resp.OperatorID, true, http.StatusOK, nil
	}
	return resp.OperatorID, false, http.StatusOK, nil
}

// handleUserStatusMessageView renders the read-only status-message block.
// Used as the Cancel target of the inline editor (htmx swap restores the
// original block without a full page reload).
func (s *Server) handleUserStatusMessageView(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	operatorID, isSelf, status, err := s.resolveUserHomeViewer(r, slug)
	if err != nil {
		if status == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("user status message view: resolve viewer", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", status)
		return
	}
	bead, err := s.lookupUserBeadByOperator(r.Context(), operatorID)
	if err != nil {
		s.logger.Warn("user status message view: lookup user bead", "operator_id", operatorID, "error", err)
	}
	s.renderUserStatusMessageBlock(w, r, slug, statusMessageFromBead(bead), isSelf, http.StatusOK)
}

// handleUserStatusMessageEdit returns the inline editor partial. Self-only —
// non-self viewers get 403 even with a valid OIDC session, so the editor
// surface is unavailable to peers (R5 §5.2).
func (s *Server) handleUserStatusMessageEdit(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	operatorID, isSelf, status, err := s.resolveUserHomeViewer(r, slug)
	if err != nil {
		if status == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("user status message edit: resolve viewer", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", status)
		return
	}
	if !isSelf {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	bead, err := s.lookupUserBeadByOperator(r.Context(), operatorID)
	if err != nil {
		s.logger.Warn("user status message edit: lookup user bead", "operator_id", operatorID, "error", err)
	}
	s.renderUserStatusMessageEdit(w, r, slug, statusMessageFromBead(bead), "", http.StatusOK)
}

// handleUpdateUserStatusMessage persists a new status_message on the viewer's
// user bead. Self-only — the slug must resolve to the same operator id the
// OIDC resolver returns. Empty submission clears the field. On success the
// read-only block is re-rendered so htmx can swap the editor back out.
func (s *Server) handleUpdateUserStatusMessage(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	operatorID, isSelf, status, err := s.resolveUserHomeViewer(r, slug)
	if err != nil {
		if status == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		s.logger.Error("user status message update: resolve viewer", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", status)
		return
	}
	if !isSelf {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	raw := r.PostForm.Get("status_message")
	if len(raw) > maxStatusMessageLen {
		s.renderUserStatusMessageEdit(w, r, slug, raw, "Status message is too long.", http.StatusRequestEntityTooLarge)
		return
	}
	value := strings.TrimSpace(raw)

	bead, err := s.lookupUserBeadByOperator(r.Context(), operatorID)
	if err != nil {
		s.logger.Error("user status message update: lookup user bead", "operator_id", operatorID, "error", err)
		s.renderUserStatusMessageEdit(w, r, slug, raw, "Failed to load user profile.", http.StatusBadGateway)
		return
	}
	if bead == nil {
		s.renderUserStatusMessageEdit(w, r, slug, raw, "User profile not found.", http.StatusNotFound)
		return
	}

	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), bead.ID, map[string]any{"status_message": value}); err != nil {
		s.logger.Error("user status message update: writing field", "bead_id", bead.ID, "operator_id", operatorID, "error", err)
		s.renderUserStatusMessageEdit(w, r, slug, raw, "Failed to save status message.", http.StatusBadGateway)
		return
	}

	s.renderUserStatusMessageBlock(w, r, slug, value, true, http.StatusOK)
}

// renderUserStatusMessageBlock writes the read-only status-message partial.
func (s *Server) renderUserStatusMessageBlock(w http.ResponseWriter, r *http.Request, slug, message string, isSelf bool, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/user_status_message_block.html", "user_status_message_block", map[string]any{
		"Slug":          slug,
		"StatusMessage": message,
		"IsViewerSelf":  isSelf,
	})
}

// renderUserStatusMessageEdit writes the editor partial, pre-filling the
// input with the in-flight draft. errMsg surfaces inline above the form.
func (s *Server) renderUserStatusMessageEdit(w http.ResponseWriter, r *http.Request, slug, message, errMsg string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/user_status_message_edit.html", "user_status_message_edit", map[string]any{
		"Slug":          slug,
		"StatusMessage": message,
		"Error":         errMsg,
	})
}
