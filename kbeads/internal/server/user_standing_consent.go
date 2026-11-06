package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Standing consent to push CI fixes to a person's MR (gd-QV2WuUjely).
//
// `gb consent-to-push` files a decision addressed to an MR author asking
// whether a prepared CI fix may be pushed to their MR. A person may give
// standing consent instead, so such decisions are answered push-to-my-mr
// without asking them each time. Before this file the consent was a key inside
// the user bead's free-form `preferences` blob, which the shared static token
// every agent pod holds may write, and the agent CLI itself closed the decision
// with chosen=push-to-my-mr. An agent (or an injected prompt) could set the
// preference on anyone's bead and then "answer" for them.
//
// Now:
//
//   - The consent is the top-level type:user field consent_push_to_mr_auto, in
//     userStandingConsentFields, which joins isGatedUserField: a generic write
//     needs an OIDC gardenio-admin, and a static or fleet token gets 403. The
//     person sets their own with POST /v1/users/me/consent-push, which writes
//     only the bead their own OIDC token resolves to. preferences is not read.
//   - The daemon, not the CLI, answers the decision. When a consent-to-push
//     decision is created addressed to exactly one open user bead that carries
//     the consent, createBead closes it with chosen=push-to-my-mr and responded_by
//     system:standing-consent, a sentinel no human or agent credential can be
//     attributed as. answeredByHumanOrSlackBridge refuses system:* actors, so a
//     standing-consent answer never unlocks a privileged continuation respawn.
//
// The decision's MR and branch are still the filing agent's word; the consent
// says the person accepts fixes on their own MRs, and the executor that acts on
// the answer pushes only to consent_push_mr.

// userConsentPushAutoField is the type:user field recording standing consent.
const userConsentPushAutoField = "consent_push_to_mr_auto"

// userStandingConsentFields are the type:user fields that let the system act
// for the person without asking. They share the identity fields' write gate.
var userStandingConsentFields = map[string]bool{
	userConsentPushAutoField: true,
}

// standingConsentActor is the responder recorded on a decision answered by a
// person's standing consent.
const standingConsentActor = "system:standing-consent"

// The consent-to-push decision contract gb consent-to-push stamps
// (gardenio/controller/cmd/gb/consent_to_push.go).
const (
	consentPushFlagField     = "consent_push"
	consentPushToMyMROption  = "push-to-my-mr"
	standingConsentUserField = "standing_consent_user"
)

// UserConsentPushRequest is the body for POST /v1/users/me/consent-push.
type UserConsentPushRequest struct {
	Auto *bool `json:"auto"`
}

// UserConsentPushResponse reports the caller's standing consent.
type UserConsentPushResponse struct {
	UserBeadID string `json:"user_bead_id"`
	Auto       bool   `json:"auto"`
}

// consentPushCaller resolves the caller of /v1/users/me/consent-push to their
// own user bead, the same caller rule as handleAcceptTerms: the person's own
// OIDC token, never a static, fleet, edge or machine credential. An applicant
// (no gardenio role and no group, Track B B-D1) has no MRs here to consent
// about, so like read cursors the route is not open to them. It writes the
// error and returns ok=false when the caller is refused.
func (s *BeadsServer) consentPushCaller(w http.ResponseWriter, r *http.Request) (*model.Bead, string, bool) {
	id := AuthIdentityFromContext(r.Context())
	if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {
		writeError(w, http.StatusForbidden, "standing consent to push needs the person's own OIDC token")
		return nil, "", false
	}
	if len(RolesFromContext(r.Context())) == 0 && len(GroupsFromContext(r.Context())) == 0 {
		writeError(w, http.StatusForbidden, "standing consent to push needs a gardenio role or a land")
		return nil, "", false
	}
	email := canonicalAliasSubject("oidc", id.Subject)
	bead, err := s.userBeadForOIDCCaller(r.Context(), email)
	if err != nil {
		writeServerError(r.Context(), w, "consentPushCaller", err, "failed to find user bead")
		return nil, "", false
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "no user bead for "+email)
		return nil, "", false
	}
	return bead, email, true
}

// handleSetMyConsentPush handles POST /v1/users/me/consent-push: the person
// gives ({"auto": true}) or withdraws ({"auto": false}) their own standing
// consent. The request names no bead, so it cannot be aimed at someone else.
func (s *BeadsServer) handleSetMyConsentPush(w http.ResponseWriter, r *http.Request) {
	bead, email, ok := s.consentPushCaller(w, r)
	if !ok {
		return
	}
	var req UserConsentPushRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Auto == nil {
		writeError(w, http.StatusBadRequest, "auto (true or false) is required")
		return
	}

	extra := map[string]any{userConsentPushAutoField: *req.Auto}
	updated, err := s.store.MergeBeadFields(r.Context(), bead.ID, extra)
	if err != nil {
		writeServerError(r.Context(), w, "handleSetMyConsentPush", err, "failed to update user bead")
		return
	}
	if updated != nil {
		s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, email, events.BeadUpdated{
			Bead:    updated,
			Changes: map[string]any{"fields": extra},
			Actor:   email,
		})
	}

	slog.Info("standing consent to push set", "bead_id", bead.ID, "auth_subject", email, "auto", *req.Auto)
	writeJSON(w, http.StatusOK, &UserConsentPushResponse{UserBeadID: bead.ID, Auto: *req.Auto})
}

// handleMyConsentPush handles GET /v1/users/me/consent-push: whether the
// caller has given standing consent, from their own user bead.
func (s *BeadsServer) handleMyConsentPush(w http.ResponseWriter, r *http.Request) {
	bead, _, ok := s.consentPushCaller(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, &UserConsentPushResponse{UserBeadID: bead.ID, Auto: userHasStandingConsentToPush(bead)})
}

// userHasStandingConsentToPush reports whether a user bead carries standing
// consent. Only the gated top-level field counts; a consent_push_to_mr_auto key
// inside preferences (the pre-gd-QV2WuUjely location, writable by any agent) is
// ignored.
func userHasStandingConsentToPush(b *model.Bead) bool {
	return b != nil && parseFieldsToStringMap(b.Fields)[userConsentPushAutoField] == "true"
}

// isConsentToPushDecision reports whether fields describe an open question
// gb consent-to-push filed: the consent_push flag plus a push-to-my-mr option.
func isConsentToPushDecision(fields json.RawMessage) bool {
	if parseFieldsToStringMap(fields)[consentPushFlagField] != "true" {
		return false
	}
	var f struct {
		Options json.RawMessage `json:"options"`
	}
	if json.Unmarshal(fields, &f) != nil {
		return false
	}
	for _, o := range parseAgingOptions(f.Options) {
		if o.ID == consentPushToMyMROption {
			return true
		}
	}
	return false
}

// standingConsentUserFor returns the one open user bead whose identity fields
// stand for operatedBy ("oidc:<email>", "slack:<id>", ...). nil when none or
// more than one matches: an ambiguous addressee is asked, not answered for.
func (s *BeadsServer) standingConsentUserFor(ctx context.Context, operatedBy string) (*model.Bead, error) {
	kind, subject := splitOperator(strings.TrimSpace(operatedBy))
	subject = canonicalAliasSubject(kind, subject)
	if kind == "" || subject == "" {
		return nil, nil
	}
	users, err := s.listUserBeads(ctx)
	if err != nil {
		return nil, err
	}
	var match *model.Bead
	for _, b := range users {
		if b.Status == model.StatusClosed {
			continue
		}
		for _, ref := range userBeadAliasRefs(b.Fields) {
			if ref.Kind == kind && ref.Subject == subject {
				if match != nil && match.ID != b.ID {
					return nil, nil
				}
				match = b
				break
			}
		}
	}
	return match, nil
}

// maybeResolveByStandingConsent answers a just-created consent-to-push decision
// for its addressee when they have given standing consent, running the same
// merge → close → publish → cascade as handleResolveDecision and the aging
// sweep. It returns the closed decision, or nil when it left the decision open
// (not a consent-to-push decision, no single addressee, no consent, or an
// error, which is logged: the person is then asked as usual).
func (s *BeadsServer) maybeResolveByStandingConsent(ctx context.Context, bead *model.Bead) *model.Bead {
	if bead == nil || bead.Type != model.TypeDecision || bead.Status == model.StatusClosed ||
		!isConsentToPushDecision(bead.Fields) {
		return nil
	}
	operatedBy := parseFieldsToStringMap(bead.Fields)["operated_by"]
	user, err := s.standingConsentUserFor(ctx, operatedBy)
	if err != nil {
		slog.Warn("standing consent: user lookup failed; leaving decision for the person",
			"decision", bead.ID, "err", err.Error())
		return nil
	}
	if !userHasStandingConsentToPush(user) {
		return nil
	}

	respondedAt := time.Now().UTC().Format(time.RFC3339)
	extra := map[string]any{
		"chosen":                     consentPushToMyMROption,
		"responded_at":               respondedAt,
		"responded_by":               standingConsentActor,
		"rationale":                  "Standing consent: " + operatedBy + " opted in to having CI fixes pushed to their MRs (user bead " + user.ID + ")",
		standingConsentUserField:     user.ID,
		decisionResolverSubjectField: standingConsentActor,
		decisionResolverAdminField:   false,
	}
	if err := s.mergeBeadFields(ctx, bead.ID, extra); err != nil {
		slog.Warn("standing consent: could not record the answer; leaving decision for the person",
			"decision", bead.ID, "err", err.Error())
		return nil
	}
	closed, err := s.store.CloseBead(ctx, bead.ID, standingConsentActor)
	if err != nil || closed == nil {
		slog.Warn("standing consent: close failed; decision stays open", "decision", bead.ID, "err", err)
		return nil
	}

	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, standingConsentActor, events.BeadClosed{
		Bead:     closed,
		ClosedBy: standingConsentActor,
		DecisionAnswer: &events.DecisionAnswer{
			Chosen:      consentPushToMyMROption,
			RespondedBy: standingConsentActor,
			RespondedAt: respondedAt,
		},
	})
	s.afterBeadClosed(ctx, closed)

	slog.Info("standing consent: answered consent-to-push decision",
		"decision", bead.ID, "user_bead", user.ID, "chosen", consentPushToMyMROption)
	return closed
}
