package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
)

// SlackOperatorAlias is one row of the slack-alias resync feed.
type SlackOperatorAlias struct {
	OperatorID int64  `json:"operator_id"`
	Subject    string `json:"subject"`
	Display    string `json:"display"`
	Email      string `json:"email"`
}

// ListSlackOperatorAliasesResponse is the response body for
// GET /v1/admin/operators/slack-aliases. Driven by the Slack users.info
// resync job (ADR docs/adr/human-attribution-identity-v2.md §7 follow-up #1).
type ListSlackOperatorAliasesResponse struct {
	Aliases []SlackOperatorAlias `json:"aliases"`
}

// UpdateOperatorIdentityRequest is the body for
// POST /v1/admin/operators/{id}/identity.
//
// A machine caller (the shared static token every agent pod holds, or a fleet
// token) must NOT choose the values: any display/email it sent landed on any
// operator row, so an agent could rename operator #N in the viewer's "done by"
// chips and audit lists, or put a real human's email on another operator row
// (gd-iRFGU6xSK7). A machine caller may only send FromUserBead — "re-derive
// operator X from its person's user bead" — and the daemon reads the email
// (an identity field only an OIDC gardenio-admin may write,
// authorizeUserIdentityFields) and the display name from that type:user bead
// itself. An explicit Display/Email stays an action by an OIDC
// gardenio-admin.
type UpdateOperatorIdentityRequest struct {
	// Display and Email are the explicit values. OIDC gardenio-admin only.
	Display string `json:"display,omitempty"`
	Email   string `json:"email,omitempty"`
	// FromUserBead derives display and email from the one type:user bead
	// whose identity aliases resolve to this operator. Mutually exclusive
	// with Display/Email.
	FromUserBead bool `json:"from_user_bead,omitempty"`
}

// UpdateOperatorIdentityResponse signals whether the operator row was
// matched. Updated=false means the id was not found (operator merged or
// deleted) — caller should drop the alias from its retry queue.
type UpdateOperatorIdentityResponse struct {
	Updated bool `json:"updated"`
	// NoUserBead is set on a from_user_bead request when no user bead with an
	// email resolves to the operator: there is nothing to derive, which is
	// not an error, and nothing is written.
	NoUserBead bool `json:"no_user_bead,omitempty"`
	// Display and Email echo the values written on a from_user_bead request.
	Display string `json:"display,omitempty"`
	Email   string `json:"email,omitempty"`
}

// errAmbiguousOperatorUserBead is returned by userBeadForOperator when more
// than one user bead resolves to the operator.
var errAmbiguousOperatorUserBead = errors.New("more than one user bead resolves to this operator")

// handleListSlackOperatorAliases handles GET /v1/admin/operators/slack-aliases.
// Admin-only. Returns every (kind=slack) alias joined with its parent
// operator's current display/email so the resync job can short-circuit
// no-op refreshes.
func (s *BeadsServer) handleListSlackOperatorAliases(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListOperatorAliasesByKind(r.Context(), "slack")
	if err != nil {
		writeServerError(r.Context(), w, "handleListSlackOperatorAliases", err, "failed to list slack operator aliases")
		return
	}
	resp := ListSlackOperatorAliasesResponse{Aliases: make([]SlackOperatorAlias, 0, len(rows))}
	for _, r := range rows {
		resp.Aliases = append(resp.Aliases, SlackOperatorAlias{
			OperatorID: r.OperatorID,
			Subject:    r.Subject,
			Display:    r.Display,
			Email:      r.Email,
		})
	}
	writeJSON(w, http.StatusOK, &resp)
}

// handleUpdateOperatorIdentity handles POST /v1/admin/operators/{id}/identity.
// Admin-only for explicit values; a machine caller may only send
// from_user_bead (see UpdateOperatorIdentityRequest). Idempotent — the same
// payload twice is safe and leaves updated_at alone.
func (s *BeadsServer) handleUpdateOperatorIdentity(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid operator id", http.StatusBadRequest)
		return
	}
	var req UpdateOperatorIdentityRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	explicit := strings.TrimSpace(req.Display) != "" || strings.TrimSpace(req.Email) != ""
	display, email := req.Display, req.Email
	switch {
	case req.FromUserBead:
		if explicit {
			writeError(w, http.StatusBadRequest, "display/email and from_user_bead are mutually exclusive")
			return
		}
		bead, err := s.userBeadForOperator(r.Context(), id)
		if errors.Is(err, errAmbiguousOperatorUserBead) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeServerError(r.Context(), w, "handleUpdateOperatorIdentity", err, "failed to find user bead for operator")
			return
		}
		if bead == nil {
			writeJSON(w, http.StatusOK, &UpdateOperatorIdentityResponse{NoUserBead: true})
			return
		}
		display, email = userBeadOperatorIdentity(bead)
	case !isAdminCaller(r.Context()):
		writeError(w, http.StatusForbidden, "insufficient permissions: setting operator display/email requires "+RoleAdmin+" (OIDC); machine callers may only send from_user_bead")
		return
	}
	updated, err := s.store.UpdateOperatorIdentity(r.Context(), id, display, email)
	if err != nil {
		writeServerError(r.Context(), w, "handleUpdateOperatorIdentity", err, "failed to update operator identity")
		return
	}
	resp := UpdateOperatorIdentityResponse{Updated: updated}
	if req.FromUserBead {
		resp.Display, resp.Email = display, email
	}
	writeJSON(w, http.StatusOK, &resp)
}

// userBeadForOperator returns the one type:user bead with an email whose
// identity aliases (userBeadAliasRefs) resolve to operatorID, nil if none
// does, and errAmbiguousOperatorUserBead if more than one does. The aliases
// come from the bead's identity fields, which only an OIDC gardenio-admin may
// write, so a match is an admin's statement that this bead is that person.
func (s *BeadsServer) userBeadForOperator(ctx context.Context, operatorID int64) (*model.Bead, error) {
	beads, err := s.listUserBeads(ctx)
	if err != nil {
		return nil, err
	}
	var match *model.Bead
	for _, b := range beads {
		if _, email := userBeadOperatorIdentity(b); email == "" {
			continue
		}
		hit := false
		for _, a := range userBeadAliasRefs(b.Fields) {
			got, err := s.store.LookupOperatorAlias(ctx, a.Kind, a.Subject)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if got == operatorID {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if match != nil && match.ID != b.ID {
			return nil, errAmbiguousOperatorUserBead
		}
		match = b
	}
	return match, nil
}

// userBeadOperatorIdentity is the (display, email) a user bead gives its
// operator row: display_name, else the bead title; and the email field,
// lower-cased the way the oidc alias is. email is "" when the bead has none
// that could be an address.
func userBeadOperatorIdentity(b *model.Bead) (display, email string) {
	f := parseFieldsToStringMap(b.Fields)
	display = strings.TrimSpace(f["display_name"])
	if display == "" {
		display = strings.TrimSpace(b.Title)
	}
	email = canonicalAliasSubject("oidc", f["email"])
	if at := strings.IndexByte(email, '@'); at <= 0 || at >= len(email)-1 {
		email = ""
	}
	return display, email
}
