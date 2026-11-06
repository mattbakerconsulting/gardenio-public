package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Slack account linking (gd-OZJ6H2S3xx, decision gd-uPHB7YcHuI).
//
// slack-bridge acts for a Slack user only after that person linked their
// gardenio account: a one-time browser sign-in through the gardenio-slack-link
// Keycloak client. The bind is recorded HERE, on the person's own type:user
// bead, under the person's own token: the bridge calls POST
// /v1/identity-links/slack with the access token Keycloak just issued for the
// person, so the audit row for the link itself is oidc:<email> and the bridge's
// credential never writes a user bead. The Slack user id in the body is the
// one the bridge bound to the sign-in state it created for exactly that Slack
// user (ephemeral button); it never comes from the browser.
//
// type:user keeps write_policy.min_role gardenio-admin for ordinary writes.
// This endpoint is a server-side write of two fields (identity_link,
// slack_id) on the CALLER'S OWN bead, like /v1/operators/me/aliases: the
// person proves ownership of the email by holding the token, and ownership of
// the Slack id by having pressed the button the bridge showed only to them.

// SlackLinkClientID is the Keycloak client (a token's azp) whose sign-in may
// write the Slack bind. It is the same client BEADS_CLIENT_ROLE_CEILING caps.
const SlackLinkClientID = "gardenio-slack-link"

// identityLinkSlackField is the key under fields.identity_link that records
// the Slack bind.
const identityLinkSlackField = "slack"

// SlackIdentityLinkRequest is the body for POST and DELETE
// /v1/identity-links/slack.
type SlackIdentityLinkRequest struct {
	SlackUserID string `json:"slack_user_id"`
	SlackTeamID string `json:"slack_team_id,omitempty"`
	LinkedVia   string `json:"linked_via,omitempty"`
}

// SlackIdentityLinkResponse reports the bead the bind landed on.
type SlackIdentityLinkResponse struct {
	UserBeadID    string `json:"user_bead_id"`
	Email         string `json:"email"`
	SlackUserID   string `json:"slack_user_id"`
	AlreadyLinked bool   `json:"already_linked,omitempty"`
}

// slackIdentityLink is the JSON recorded at fields.identity_link.slack.
type slackIdentityLink struct {
	SlackID   string `json:"slack_id"`
	TeamID    string `json:"team_id,omitempty"`
	Subject   string `json:"subject"`
	LinkedAt  string `json:"linked_at"`
	Client    string `json:"client,omitempty"`
	LinkedVia string `json:"linked_via,omitempty"`
}

// userBeadForOIDCCaller returns the open type:user bead that belongs to the
// OIDC caller: the one the UserProvisioner stamped on the context when it
// runs, else the open user bead whose email is the caller's. nil, nil when there is none.
func (s *BeadsServer) userBeadForOIDCCaller(ctx context.Context, email string) (*model.Bead, error) {
	if id := UserBeadIDFromContext(ctx); id != "" {
		b, err := s.store.GetBead(ctx, id)
		if err != nil {
			return nil, err
		}
		if b != nil && b.Type == model.BeadType("user") && b.Status != model.StatusClosed {
			return b, nil
		}
	}
	beads, err := s.listUserBeads(ctx)
	if err != nil {
		return nil, err
	}
	want := canonicalAliasSubject("oidc", email)
	for _, b := range beads {
		for _, ref := range userBeadAliasRefs(b.Fields) {
			if ref.Kind == "oidc" && ref.Subject == want {
				return b, nil
			}
		}
	}
	return nil, nil
}

// userBeadFields decodes a bead's fields into a map (empty when unset).
func userBeadFields(b *model.Bead) map[string]any {
	fields := map[string]any{}
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &fields)
	}
	return fields
}

// otherUserBeadCarriesSlackID reports whether an open user bead other than
// beadID has fields.slack_id == slackID (the bridge resolves slack_id across
// all open user beads, so a second carrier would make the person ambiguous).
func (s *BeadsServer) otherUserBeadCarriesSlackID(ctx context.Context, beadID, slackID string) (bool, error) {
	beads, err := s.listUserBeads(ctx)
	if err != nil {
		return false, err
	}
	for _, b := range beads {
		if b.ID == beadID {
			continue
		}
		if sid, _ := userBeadFields(b)["slack_id"].(string); strings.TrimSpace(sid) == slackID {
			return true, nil
		}
	}
	return false, nil
}

// handleLinkSlackIdentity handles POST /v1/identity-links/slack.
//
//   - OIDC only: the person's own token, and only one whose azp is
//     SlackLinkClientID (the token the gardenio-slack-link sign-in returned).
//     slack_user_id comes from the body, so a token from any other client
//     could bind someone else's unclaimed Slack id. Static, fleet, edge and
//     machine callers are refused.
//   - 404 when the caller has no user bead (an admin creates it first).
//   - 409 when the bead's slack_id is already a DIFFERENT Slack id, or when
//     another open user bead already carries this Slack id. Never auto-merge;
//     an admin resolves it.
//   - Idempotent: re-linking the same id refreshes identity_link.slack and
//     answers AlreadyLinked.
//   - Also attaches the slack:<id> operator alias to the caller's operator
//     row (409 if it belongs to someone else), so Slack-side attribution and
//     browser attribution land on one operator.
func (s *BeadsServer) handleLinkSlackIdentity(w http.ResponseWriter, r *http.Request) {
	id := AuthIdentityFromContext(r.Context())
	if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {
		writeError(w, http.StatusForbidden, "slack identity link requires the person's own OIDC token")
		return
	}
	if id.ClientID != SlackLinkClientID {
		// slack_user_id comes from the request body, so any signed-in person's
		// token could otherwise bind someone else's unclaimed Slack id. Only a
		// token minted through the link sign-in (state-bound to the Slack user
		// the bridge saw) may write the bind.
		slog.Warn("slack identity link refused: token not issued to the slack-link client",
			"auth_subject", id.Subject, "azp", id.ClientID)
		writeError(w, http.StatusForbidden, "slack identity link requires a token from the "+SlackLinkClientID+" sign-in")
		return
	}
	var req SlackIdentityLinkRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	slackID := strings.TrimSpace(req.SlackUserID)
	if slackID == "" || strings.ContainsAny(slackID, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "slack_user_id is required")
		return
	}
	email := canonicalAliasSubject("oidc", id.Subject)

	bead, err := s.userBeadForOIDCCaller(r.Context(), email)
	if err != nil {
		writeServerError(r.Context(), w, "handleLinkSlackIdentity", err, "failed to find user bead")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "no user bead for "+email+"; ask a "+RoleAdmin+" to create one")
		return
	}
	fields := userBeadFields(bead)
	if cur, _ := fields["slack_id"].(string); strings.TrimSpace(cur) != "" && strings.TrimSpace(cur) != slackID {
		slog.Warn("slack identity link refused: user bead carries a different slack_id",
			"bead_id", bead.ID, "auth_subject", email, "requested", slackID)
		writeErrorCode(w, http.StatusConflict, "your user bead is already bound to a different Slack id; ask a "+RoleAdmin+" to change it", "slack_id_conflict")
		return
	}
	taken, err := s.otherUserBeadCarriesSlackID(r.Context(), bead.ID, slackID)
	if err != nil {
		writeServerError(r.Context(), w, "handleLinkSlackIdentity", err, "failed to check slack_id")
		return
	}
	if taken {
		slog.Warn("slack identity link refused: slack_id belongs to another user bead",
			"bead_id", bead.ID, "auth_subject", email, "requested", slackID)
		writeErrorCode(w, http.StatusConflict, "this Slack id is already bound to another person; ask a "+RoleAdmin, "slack_id_conflict")
		return
	}

	already := false
	if prev, ok := fields["identity_link"].(map[string]any); ok {
		if cur, ok := prev[identityLinkSlackField].(map[string]any); ok {
			if sid, _ := cur["slack_id"].(string); sid == slackID {
				already = true
			}
		}
	}
	link := slackIdentityLink{
		SlackID:   slackID,
		TeamID:    strings.TrimSpace(req.SlackTeamID),
		Subject:   email,
		LinkedAt:  time.Now().UTC().Format(time.RFC3339),
		Client:    id.ClientID,
		LinkedVia: strings.TrimSpace(req.LinkedVia),
	}
	identityLink, _ := fields["identity_link"].(map[string]any)
	if identityLink == nil {
		identityLink = map[string]any{}
	}
	var linkMap map[string]any
	raw, _ := json.Marshal(link)
	_ = json.Unmarshal(raw, &linkMap)
	identityLink[identityLinkSlackField] = linkMap
	fields["identity_link"] = identityLink
	if cur, _ := fields["slack_id"].(string); strings.TrimSpace(cur) == "" {
		fields["slack_id"] = slackID
	}
	updated, err := json.Marshal(fields)
	if err != nil {
		writeServerError(r.Context(), w, "handleLinkSlackIdentity", err, "failed to encode fields")
		return
	}
	bead.Fields = updated
	if err := s.store.UpdateBead(r.Context(), bead); err != nil {
		writeServerError(r.Context(), w, "handleLinkSlackIdentity", err, "failed to update user bead")
		return
	}

	// Operator alias: same rule as /v1/operators/me/aliases, never merge.
	err = s.store.RunInTransaction(r.Context(), func(tx store.Store) error {
		opID, err := tx.UpsertOperator(r.Context(), "oidc", email)
		if err != nil {
			return err
		}
		if err := tx.UpsertOperatorAlias(r.Context(), opID, "oidc", email, "slack-identity-link"); err != nil {
			return err
		}
		oldID, lookupErr := tx.LookupOperatorAlias(r.Context(), "slack", slackID)
		switch {
		case lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows):
			return lookupErr
		case lookupErr == nil && oldID == opID:
			return nil
		case lookupErr == nil && oldID != 0:
			return &errSelfLinkCollision{owner: oldID}
		}
		return tx.UpsertOperatorAlias(r.Context(), opID, "slack", slackID, "slack-identity-link")
	})
	var collision *errSelfLinkCollision
	if errors.As(err, &collision) {
		slog.Warn("slack identity link: operator alias belongs to a different operator (bead bind kept)",
			"bead_id", bead.ID, "owner_operator_id", collision.owner, "auth_subject", email, "slack_id", slackID)
	} else if err != nil {
		writeServerError(r.Context(), w, "handleLinkSlackIdentity", err, "failed to link operator alias")
		return
	}

	slog.Info("slack identity linked", "bead_id", bead.ID, "auth_subject", email, "slack_id", slackID, "client", id.ClientID)
	writeJSON(w, http.StatusOK, &SlackIdentityLinkResponse{
		UserBeadID:    bead.ID,
		Email:         email,
		SlackUserID:   slackID,
		AlreadyLinked: already,
	})
}

// handleUnlinkSlackIdentity handles DELETE /v1/identity-links/slack: the OIDC
// caller removes identity_link.slack from their own user bead, and slack_id
// when it is the linked id. Idempotent. The bridge calls this, as the person,
// before it revokes the person's offline token on unlink; a Keycloak-side
// revoke that bypasses the bridge leaves the record in place, which is
// harmless (the bridge holds no usable credential and refuses with the link
// prompt) and visible to an admin.
func (s *BeadsServer) handleUnlinkSlackIdentity(w http.ResponseWriter, r *http.Request) {
	id := AuthIdentityFromContext(r.Context())
	if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {
		writeError(w, http.StatusForbidden, "slack identity unlink requires the person's own OIDC token")
		return
	}
	email := canonicalAliasSubject("oidc", id.Subject)
	bead, err := s.userBeadForOIDCCaller(r.Context(), email)
	if err != nil {
		writeServerError(r.Context(), w, "handleUnlinkSlackIdentity", err, "failed to find user bead")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "no user bead for "+email)
		return
	}
	fields := userBeadFields(bead)
	var linked string
	identityLink, _ := fields["identity_link"].(map[string]any)
	if identityLink != nil {
		if cur, ok := identityLink[identityLinkSlackField].(map[string]any); ok {
			linked, _ = cur["slack_id"].(string)
		}
		delete(identityLink, identityLinkSlackField)
		if len(identityLink) == 0 {
			delete(fields, "identity_link")
		} else {
			fields["identity_link"] = identityLink
		}
	}
	if linked == "" {
		writeJSON(w, http.StatusOK, &SlackIdentityLinkResponse{UserBeadID: bead.ID, Email: email})
		return
	}
	if cur, _ := fields["slack_id"].(string); strings.TrimSpace(cur) == linked {
		delete(fields, "slack_id")
	}
	updated, err := json.Marshal(fields)
	if err != nil {
		writeServerError(r.Context(), w, "handleUnlinkSlackIdentity", err, "failed to encode fields")
		return
	}
	bead.Fields = updated
	if err := s.store.UpdateBead(r.Context(), bead); err != nil {
		writeServerError(r.Context(), w, "handleUnlinkSlackIdentity", err, "failed to update user bead")
		return
	}
	slog.Info("slack identity unlinked", "bead_id", bead.ID, "auth_subject", email, "slack_id", linked)
	writeJSON(w, http.StatusOK, &SlackIdentityLinkResponse{UserBeadID: bead.ID, Email: email, SlackUserID: linked})
}
