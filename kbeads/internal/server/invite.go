package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Sign-in invites (owner ruling gd-hCMlZ5jnvB = b, task gd-ycE484kLyk).
//
// GitHub sign-in is invite-only, and invites go out by email. An admin files
// one type=invite bead naming the person's email address (gb lands invite).
// The Lands provisioner mails the invitee, and when a Keycloak user whose
// verified email is exactly that address, and who has a GitHub link, exists,
// it adds them to the static group invited (and to the land's group when the
// invite names one) and marks the invite claimed. The gardenio.clonk.tv edge
// admits /lands/* only to /invited and the groups it already admitted.
//
// The type is admin-gated. An invite carries no project label rule: it holds
// a stranger's email address, so with none it is read by admins and the
// machine principals only. The rules here are about shape:
//
//   - email is one lowercase address (Keycloak stores emails lowercased, so
//     the provisioner's match is an exact compare);
//   - land and role come together or not at all, and land names a tenant
//     land;
//   - born pending, with nothing the provisioner writes already set;
//   - who is invited, to what, by whom and until when never change;
//   - state moves pending → claimed / revoked / expired, and each of those
//     is terminal.

const typeInvite model.BeadType = "invite"

// Invite states.
const (
	invitePending = "pending"
	inviteClaimed = "claimed"
	inviteRevoked = "revoked"
	inviteExpired = "expired"
)

var inviteStateEdges = map[string][]string{
	invitePending: {inviteClaimed, inviteRevoked, inviteExpired},
	inviteClaimed: nil,
	inviteRevoked: nil,
	inviteExpired: nil,
}

// inviteFixedFields are written when the invite is filed and never after.
var inviteFixedFields = []string{"email", "land", "role", "invited_by", "expires_at"}

// inviteOutcomeFields are the provisioner's and the revoke's; a new invite
// carries none of them.
var inviteOutcomeFields = []string{
	"mailed_at", "claimed_user_id", "claimed_at", "claimed_gatehouse", "revoke_requested_at", "revoke_requested_by", "revoked_at", "outcome_note",
}

type inviteFields struct {
	Email string `json:"email"`
	Land  string `json:"land"`
	Role  string `json:"role"`
	State string `json:"state"`
}

func decodeInviteFields(raw json.RawMessage) (inviteFields, map[string]any) {
	var f inviteFields
	all := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &f)
		_ = json.Unmarshal(raw, &all)
	}
	return f, all
}

// inviteEmailError reports an email that is not one bare lowercase address.
func inviteEmailError(email string) error {
	a, err := mail.ParseAddress(email)
	if err != nil || a.Name != "" || a.Address != email || len(email) > 254 || strings.Count(email, "@") != 1 {
		return fmt.Errorf("email %q must be one bare email address", email)
	}
	if email != strings.ToLower(email) {
		return fmt.Errorf("email %q must be lowercase (Keycloak stores emails lowercased)", email)
	}
	return nil
}

// validateInviteShape checks the field rules that hold in every state.
func validateInviteShape(f inviteFields) error {
	if err := inviteEmailError(f.Email); err != nil {
		return err
	}
	switch {
	case f.Land == "" && f.Role != "":
		return fmt.Errorf("an invite with no land has no role; got role %q", f.Role)
	case f.Land != "" && f.Role != "member" && f.Role != "guest":
		return fmt.Errorf("an invite to a land gives the role member or guest, not %q", f.Role)
	}
	return nil
}

// validateInviteCreate enforces the create-time rules: the field shape, state
// pending with no outcome set, and a land, when named, that is a tenant land.
func validateInviteCreate(ctx context.Context, st store.Store, bead *model.Bead) error {
	f, all := decodeInviteFields(bead.Fields)
	if f.State != invitePending {
		return fmt.Errorf("an invite is created in state %q, not %q", invitePending, f.State)
	}
	var set []string
	for _, k := range inviteOutcomeFields {
		if v, ok := all[k]; ok && !isEmptyFieldValue(v) {
			set = append(set, k)
		}
	}
	if len(set) > 0 {
		return fmt.Errorf("a new invite cannot carry %s", strings.Join(set, ", "))
	}
	if err := validateInviteShape(f); err != nil {
		return err
	}
	if f.Land == "" {
		return nil
	}
	if strings.TrimSpace(f.Land) != f.Land {
		return fmt.Errorf("an invite's land %q has spaces around it", f.Land)
	}
	return tenantLandError(ctx, st, f.Land)
}

// validateInviteUpdate checks a fields update against the post-merge bead:
// the fixed fields unchanged, the shape, and a legal state move.
func validateInviteUpdate(prior, next json.RawMessage) error {
	from, before := decodeInviteFields(prior)
	to, after := decodeInviteFields(next)
	for _, k := range inviteFixedFields {
		if !jsonValueEqual(before[k], after[k]) {
			return fmt.Errorf("an invite's %s is fixed when it is written; revoke it and invite again", k)
		}
	}
	if err := validateInviteShape(to); err != nil {
		return err
	}
	if from.State == to.State {
		return nil
	}
	if !slices.Contains(inviteStateEdges[from.State], to.State) {
		legal := "none"
		if edges := inviteStateEdges[from.State]; len(edges) > 0 {
			legal = strings.Join(edges, ", ")
		}
		return fmt.Errorf("invite state cannot move %s → %s; from %s it may move to %s", from.State, to.State, from.State, legal)
	}
	return nil
}
