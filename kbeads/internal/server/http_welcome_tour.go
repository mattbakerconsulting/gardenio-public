package server

import (
	"log/slog"
	"net/http"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// The welcome tour's seen flag (Lands Track B step 9.14b, gd-aEIUgpyxpi).
//
// The first time a person opens /garden, the page walks them through their
// land, its #general and how to ask, once. Whether they have had it is one
// field on their own type:user bead, welcome_tour_seen_at (RFC 3339), so a
// second device does not run it again: beads-viewer reads it on GET /garden
// and sets it when the tour finishes or is dismissed. The field is gated
// (isGatedUserField) so an agent's token cannot mark someone's tour seen; the
// person sets their own here, with the read-cursors caller rule: their own
// OIDC token, a gardenio role or a land, their own bead. The request names no
// bead and carries no value: the daemon stamps the time, once.

// userWelcomeTourField is the type:user field recording when the person
// finished or dismissed the welcome tour.
const userWelcomeTourField = "welcome_tour_seen_at"

// WelcomeTourResponse is the caller's welcome tour state. SeenAt is "" while
// the tour is still due.
type WelcomeTourResponse struct {
	UserBeadID string `json:"user_bead_id"`
	SeenAt     string `json:"seen_at"`
}

// welcomeTourCaller resolves the caller of /v1/users/me/welcome-tour to their
// own user bead, as consentPushCaller does. It writes the error and returns
// ok=false when the caller is refused.
func (s *BeadsServer) welcomeTourCaller(w http.ResponseWriter, r *http.Request) (*model.Bead, string, bool) {
	id := AuthIdentityFromContext(r.Context())
	if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {
		writeError(w, http.StatusForbidden, "the welcome tour needs the person's own OIDC token")
		return nil, "", false
	}
	// An applicant (no gardenio role and no group, Track B B-D1) has no
	// /garden to be shown round.
	if len(RolesFromContext(r.Context())) == 0 && len(GroupsFromContext(r.Context())) == 0 {
		writeError(w, http.StatusForbidden, "the welcome tour needs a gardenio role or a land")
		return nil, "", false
	}
	email := canonicalAliasSubject("oidc", id.Subject)
	bead, err := s.userBeadForOIDCCaller(r.Context(), email)
	if err != nil {
		writeServerError(r.Context(), w, "welcomeTourCaller", err, "failed to find user bead")
		return nil, "", false
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "no user bead for "+email)
		return nil, "", false
	}
	return bead, email, true
}

// welcomeTourSeenAt is when the bead's person had the welcome tour, "" when
// they have not.
func welcomeTourSeenAt(b *model.Bead) string {
	if b == nil {
		return ""
	}
	return parseFieldsToStringMap(b.Fields)[userWelcomeTourField]
}

// handleMyWelcomeTour handles GET /v1/users/me/welcome-tour.
func (s *BeadsServer) handleMyWelcomeTour(w http.ResponseWriter, r *http.Request) {
	bead, _, ok := s.welcomeTourCaller(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, &WelcomeTourResponse{UserBeadID: bead.ID, SeenAt: welcomeTourSeenAt(bead)})
}

// handleSeenMyWelcomeTour handles POST /v1/users/me/welcome-tour: the caller
// has had the tour. The first call stamps the time; a later one (the tour
// ended on two devices at once) answers the stored time and writes nothing.
func (s *BeadsServer) handleSeenMyWelcomeTour(w http.ResponseWriter, r *http.Request) {
	bead, email, ok := s.welcomeTourCaller(w, r)
	if !ok {
		return
	}
	if at := welcomeTourSeenAt(bead); at != "" {
		writeJSON(w, http.StatusOK, &WelcomeTourResponse{UserBeadID: bead.ID, SeenAt: at})
		return
	}
	at := time.Now().UTC().Format(time.RFC3339)
	extra := map[string]any{userWelcomeTourField: at}
	updated, err := s.store.MergeBeadFields(r.Context(), bead.ID, extra)
	if err != nil {
		writeServerError(r.Context(), w, "handleSeenMyWelcomeTour", err, "failed to update user bead")
		return
	}
	if updated != nil {
		s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, email, events.BeadUpdated{
			Bead:    updated,
			Changes: map[string]any{"fields": extra},
			Actor:   email,
		})
	}
	slog.Info("welcome tour seen", "bead_id", bead.ID, "auth_subject", email)
	writeJSON(w, http.StatusOK, &WelcomeTourResponse{UserBeadID: bead.ID, SeenAt: at})
}
