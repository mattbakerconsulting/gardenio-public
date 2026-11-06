package server

import (
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
)

// Terms acceptance (Lands spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-track-b-design.md §2.3 POST /lands/accept, step 7.4).
//
// Before a stranger may apply for a plot they accept the current terms,
// privacy policy and AUP. The acceptance is recorded on their own type:user
// bead as terms_version and terms_accepted_at. Those two fields are not
// identity fields (gardenio/docs/security/user-identity-fields.md), but they
// are evidence of what a person agreed to, so they get the same protection:
// a generic write needs an OIDC gardenio-admin (userTermsFields joins the gate
// in user_identity_authz.go), and the only other way to set them is this
// endpoint, which writes the CALLER'S OWN bead and nothing else.
//
// The route carries no role requirement. An applicant is a signed-in realm
// user with no gardenio role (B-D1), refused on every bead route, and must
// still be able to accept. The bead is the one the UserProvisioner resolved
// for the token's own subject; the request names no bead, so there is nothing
// to point at someone else's. The timestamp is the server's.

// userTermsFields are the type:user fields only an admin or POST
// /v1/users/me/terms may write.
var userTermsFields = map[string]bool{
	"terms_version":     true,
	"terms_accepted_at": true,
}

// termsVersionPattern is a document version id as the legal drafts carry it
// in front matter (e.g. "2026-10-19", "v1.2", "terms-3").
var termsVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// UserTermsRequest is the body for POST /v1/users/me/terms.
type UserTermsRequest struct {
	TermsVersion string `json:"terms_version"`
}

// UserTermsResponse reports what was recorded and where.
type UserTermsResponse struct {
	UserBeadID      string `json:"user_bead_id"`
	TermsVersion    string `json:"terms_version"`
	TermsAcceptedAt string `json:"terms_accepted_at"`
}

// handleAcceptTerms handles POST /v1/users/me/terms.
//
//   - OIDC only, any role or none: the person's own token. Static, fleet,
//     edge and machine callers have no "me" and are refused.
//   - 404 when the caller has no user bead (provisioning failed or is off).
//   - Writes exactly terms_version and terms_accepted_at; every other field
//     on the bead is left as it was.
func (s *BeadsServer) handleAcceptTerms(w http.ResponseWriter, r *http.Request) {
	id := AuthIdentityFromContext(r.Context())
	if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {
		writeError(w, http.StatusForbidden, "accepting the terms requires the person's own OIDC token")
		return
	}
	var req UserTermsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	version := strings.TrimSpace(req.TermsVersion)
	if !termsVersionPattern.MatchString(version) {
		writeError(w, http.StatusBadRequest, "terms_version is required: letters, digits, '.', '_' or '-', at most 64 characters")
		return
	}
	email := canonicalAliasSubject("oidc", id.Subject)
	bead, err := s.userBeadForOIDCCaller(r.Context(), email)
	if err != nil {
		writeServerError(r.Context(), w, "handleAcceptTerms", err, "failed to find user bead")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "no user bead for "+email)
		return
	}

	// One atomic merge of the two keys, so a concurrent write to the bead
	// (the provisioner's group sync) is never clobbered.
	acceptedAt := time.Now().UTC().Format(time.RFC3339)
	extra := map[string]any{"terms_version": version, "terms_accepted_at": acceptedAt}
	updated, err := s.store.MergeBeadFields(r.Context(), bead.ID, extra)
	if err != nil {
		writeServerError(r.Context(), w, "handleAcceptTerms", err, "failed to update user bead")
		return
	}
	if updated != nil {
		s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, email, events.BeadUpdated{
			Bead:    updated,
			Changes: map[string]any{"fields": extra},
			Actor:   email,
		})
	}

	slog.Info("terms accepted", "bead_id", bead.ID, "auth_subject", email, "terms_version", version)
	writeJSON(w, http.StatusOK, &UserTermsResponse{
		UserBeadID:      bead.ID,
		TermsVersion:    version,
		TermsAcceptedAt: acceptedAt,
	})
}

// handleMyTerms handles GET /v1/users/me/terms: what the caller last accepted,
// read from their own user bead, so the front of house can say whether the
// current version is accepted. Same caller rule as handleAcceptTerms (OIDC
// only, any role or none). A caller with no user bead yet has accepted
// nothing: 200 with empty fields, not 404, because the bead is created on
// first request and its absence says nothing about the person.
func (s *BeadsServer) handleMyTerms(w http.ResponseWriter, r *http.Request) {
	id := AuthIdentityFromContext(r.Context())
	if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {
		writeError(w, http.StatusForbidden, "reading accepted terms requires the person's own OIDC token")
		return
	}
	email := canonicalAliasSubject("oidc", id.Subject)
	bead, err := s.userBeadForOIDCCaller(r.Context(), email)
	if err != nil {
		writeServerError(r.Context(), w, "handleMyTerms", err, "failed to find user bead")
		return
	}
	resp := &UserTermsResponse{}
	if bead != nil {
		fields := userBeadFields(bead)
		resp.UserBeadID = bead.ID
		resp.TermsVersion, _ = fields["terms_version"].(string)
		resp.TermsAcceptedAt, _ = fields["terms_accepted_at"].(string)
	}
	writeJSON(w, http.StatusOK, resp)
}
