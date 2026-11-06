package provision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/mailer"
)

// Sign-in invites by email (owner ruling gd-hCMlZ5jnvB = b, gd-ycE484kLyk).
// GitHub sign-in is invite-only: the gardenio.clonk.tv edge admits /lands/
// only to /invited and the groups it admitted before. An admin files a
// type=invite bead naming an email address (gb lands invite; kbeads
// invite.go), optionally with a tenant land and a role. The sign-in invite
// pass, which runs after the member invite pass, settles each one:
//
//   - mail: a pending invite with no mailed_at is mailed to its address
//     (mailer.KindInvite), then stamped. With mail off nothing is stamped,
//     so the mail goes out once mail is on; a send that fails is retried at
//     most every InviteMailRetry.
//   - claim: once exactly one Keycloak user has exactly the invite's email,
//     with emailVerified=true and a link to the github identity provider,
//     they join InvitedGroup, and the land with the invite's role (and
//     lands-tenants) when the invite names one, or the gatehouse as a guest
//     when it names none and they hold no land (gatehouse.go), and the
//     invite is claimed and closed. A person who joined a land so (not one who
//     already held a role there) is greeted by its steward (greet). No such
//     user, an unverified email or no GitHub link: the
//     invite waits. The realm verifies every GitHub registrant's email
//     (login.verifyEmail; the github IdP never trusts GitHub's), so the
//     match is to an address the person proved they hold.
//   - expiry and revoke end a pending invite, as for member invites.
//
// The pass never links accounts and never touches the GitHub first broker
// login flow: it only reads users that flow already made. A claimed invite
// is terminal; taking access away again is a group removal in Keycloak.

// SigninInviteType is the bead type of a sign-in invite.
const SigninInviteType = "invite"

// InviteClaimed is a sign-in invite whose person got in. Pending, revoked
// and expired are the member invite's states.
const InviteClaimed = "claimed"

// InviteMailRetry is how long a failed invite mail waits before the pass
// sends it again.
const InviteMailRetry = time.Hour

// SigninInvite is an invite as the provisioner reads it.
type SigninInvite struct {
	ID                string
	Email             string
	Land              string // "" for an invite to sign in only
	Role              keycloakadmin.LandRole
	InvitedBy         string
	State             string
	ExpiresAt         time.Time // zero when unreadable, which reads as expired
	MailedAt          string
	RevokeRequestedAt string
}

// SigninInviteFromBead reads b, or reports false when b is not an invite.
func SigninInviteFromBead(b *beadsapi.BeadDetail) (*SigninInvite, bool) {
	if b == nil || b.Type != SigninInviteType {
		return nil, false
	}
	f := b.Fields
	exp, _ := time.Parse(time.RFC3339, f["expires_at"])
	return &SigninInvite{
		ID: b.ID, Email: f["email"], Land: f["land"], Role: keycloakadmin.LandRole(f["role"]),
		InvitedBy: f["invited_by"], State: f["state"], ExpiresAt: exp, MailedAt: f["mailed_at"],
		RevokeRequestedAt: f["revoke_requested_at"],
	}, true
}

// SigninInvites runs the sign-in invite pass. One invite's failure never
// stops the others; it is logged and retried on the next pass.
func (p *Provisioner) SigninInvites(ctx context.Context) error {
	beads, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{Types: []string{SigninInviteType}, Statuses: liveStatuses})
	if err != nil {
		return fmt.Errorf("reading sign-in invites: %w", err)
	}
	lands := map[string]bool{} // land → open for joining, read once per pass
	for _, b := range beads {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		inv, _ := SigninInviteFromBead(b)
		if inv == nil {
			continue
		}
		if err := p.signinInvite(ctx, inv, lands); err != nil {
			p.logger().Warn("lands sign-in invites: invite not settled; retrying next pass", "invite", inv.ID, "error", err)
		}
	}
	return nil
}

// signinInvite settles one sign-in invite.
func (p *Provisioner) signinInvite(ctx context.Context, inv *SigninInvite, lands map[string]bool) error {
	switch {
	case inv.State != InvitePending:
		// Every other state is terminal and closed when it is reached; one
		// left open by a crash is closed now.
		return p.Daemon.CloseBead(ctx, inv.ID, nil)
	case inv.RevokeRequestedAt != "":
		return p.endBead(ctx, inv.ID, inv.Land, InviteRevoked, map[string]any{"revoked_at": p.stamp()})
	case inv.ExpiresAt.IsZero() || !p.now().Before(inv.ExpiresAt):
		return p.endBead(ctx, inv.ID, inv.Land, InviteExpired, nil)
	}
	if inv.Email == "" || (inv.Land == "") != (inv.Role == "") ||
		(inv.Land != "" && inv.Role != keycloakadmin.RoleMember && inv.Role != keycloakadmin.RoleGuest) {
		return fmt.Errorf("invite carries email %q, land %q and role %q; it can never be claimed", inv.Email, inv.Land, inv.Role)
	}
	if inv.MailedAt == "" {
		if err := p.mailInvite(ctx, inv); err != nil {
			return err
		}
	}
	if inv.Land != "" {
		open, err := p.landOpenOnce(ctx, inv.Land, lands)
		if err != nil {
			return err
		}
		if !open {
			return nil // the land is not (or no longer) an open tenant land; the invite waits or expires
		}
	}
	gatehouse := false
	if inv.Land == "" && p.Gatehouse != "" {
		// A gatehouse that is not open yet lets the claim go ahead: its
		// backfill takes this person in when it opens.
		var err error
		if gatehouse, err = p.landOpenOnce(ctx, p.Gatehouse, lands); err != nil {
			return err
		}
	}
	return p.claim(ctx, inv, gatehouse)
}

// landOpenOnce is landOpen, read once per pass into lands.
func (p *Provisioner) landOpenOnce(ctx context.Context, land string, lands map[string]bool) (bool, error) {
	if open, ok := lands[land]; ok {
		return open, nil
	}
	open, err := p.landOpen(ctx, land)
	if err != nil {
		return false, err
	}
	lands[land] = open
	return open, nil
}

// mailInvite sends the invite mail and stamps mailed_at. With mail off it
// does nothing; a failed send is logged and left for a later pass.
func (p *Provisioner) mailInvite(ctx context.Context, inv *SigninInvite) error {
	if p.Templates == nil || p.Mailer == nil || !mailer.Enabled(p.Mailer) {
		return nil
	}
	if at, ok := p.inviteMailFailed[inv.ID]; ok && p.now().Before(at.Add(InviteMailRetry)) {
		return nil
	}
	by := strings.TrimPrefix(inv.InvitedBy, "oidc:")
	m, err := p.Templates.Render(mailer.KindInvite, inv.Email, mailer.Data{InvitedBy: by, Land: inv.Land})
	if err == nil {
		err = p.Mailer.Send(ctx, m)
	}
	if err != nil {
		if p.inviteMailFailed == nil {
			p.inviteMailFailed = map[string]time.Time{}
		}
		p.inviteMailFailed[inv.ID] = p.now()
		p.logger().Warn("lands sign-in invites: invite mail not sent; retrying later", "invite", inv.ID, "retry_after", InviteMailRetry, "error", err)
		return nil
	}
	delete(p.inviteMailFailed, inv.ID)
	// The mail goes before the stamp: a crash between the two sends it
	// twice, which beats never sending it.
	if err := p.Daemon.UpdateBeadFieldsTyped(ctx, inv.ID, map[string]any{"mailed_at": p.stamp()}); err != nil {
		return fmt.Errorf("stamping mailed_at: %w", err)
	}
	inv.MailedAt = p.stamp()
	return nil
}

// claim lets the invite's person in once they have signed in with GitHub and
// verified the invite's email. gatehouse is true when the invite names no land
// and the gatehouse is open: the person joins it as a guest when they hold no
// land.
func (p *Provisioner) claim(ctx context.Context, inv *SigninInvite, gatehouse bool) error {
	log := p.logger().With("invite", inv.ID, "land", inv.Land)
	u, err := p.Groups.FindUserByEmail(ctx, inv.Email)
	switch {
	case errors.Is(err, keycloakadmin.ErrUserNotFound):
		return nil // not signed in yet
	case errors.Is(err, keycloakadmin.ErrAmbiguousUser):
		log.Warn("lands sign-in invites: more than one Keycloak user has this email; letting nobody in")
		return nil
	case err != nil:
		return err
	}
	switch {
	case u.ID == "" || u.Email != inv.Email:
		// Never trust the lookup alone: the user must carry exactly this
		// address.
		log.Warn("lands sign-in invites: the email lookup answered with another user; letting nobody in")
		return nil
	case !u.EmailVerified:
		return nil // signed in, but has not confirmed the address yet
	case !slices.Contains(u.IdentityProviders, keycloakadmin.GitHubProvider):
		return nil // a realm user with this address, but not one who signs in with GitHub
	}
	if err := p.Groups.AddInvited(ctx, u.ID); err != nil {
		return err
	}
	note, joinedGatehouse := "", false
	if inv.Land != "" {
		if note, err = p.joinLand(ctx, inv.Land, inv.Role, u.ID); err != nil {
			return err
		}
	} else if gatehouse {
		if joinedGatehouse, err = p.joinGatehouse(ctx, u.ID); err != nil {
			return err
		}
	}
	fields := map[string]any{"state": InviteClaimed, "claimed_user_id": u.ID, "claimed_at": p.stamp()}
	if note != "" {
		fields["outcome_note"] = note
	}
	if joinedGatehouse {
		fields["claimed_gatehouse"] = p.Gatehouse
	}
	// The greeting goes before the state change, as a mail does: its post is
	// idempotent, so a crash between the two greets once, not never.
	switch key := "lands-invite:" + inv.ID + ":greet"; {
	case inv.Land != "" && note == "":
		p.greet(ctx, inv.Land, u, inv.Role, key)
	case joinedGatehouse:
		p.greet(ctx, p.Gatehouse, u, keycloakadmin.RoleGuest, key)
	}
	if err := p.Daemon.UpdateBeadFieldsTyped(ctx, inv.ID, fields); err != nil {
		return fmt.Errorf("marking the invite claimed: %w", err)
	}
	if err := p.Daemon.CloseBead(ctx, inv.ID, nil); err != nil {
		return fmt.Errorf("closing the claimed invite: %w", err)
	}
	log.Info("lands sign-in invites: claimed", "user", u.ID, "role", inv.Role, "note", note, "gatehouse", joinedGatehouse)
	return nil
}

// greet wakes land's steward to greet u, who just joined it with role (step
// 9.14, decision D3 gd-NLc4LzmCAE): the greeting lands in the land's #general
// (D2 gd-R0hspYDBbq, no private room). It is best effort: a person is let in
// whether or not the greeting is posted, so a failure is logged, not returned.
func (p *Provisioner) greet(ctx context.Context, land string, u keycloakadmin.User, role keycloakadmin.LandRole, key string) {
	if p.Stewards == nil {
		return
	}
	if err := p.Stewards.Greet(ctx, land, PersonName(u), string(role), key); err != nil {
		p.logger().Warn("lands: could not wake the steward to greet a person who joined", "land", land, "user", u.ID, "error", err)
	}
}

// PersonName is what a steward's greeting calls u: their profile name, else
// their username, and "" when the only name there is looks like an email
// address. Every guest of a land reads its #general, so a greeting never
// carries one.
func PersonName(u keycloakadmin.User) string {
	for _, n := range []string{strings.TrimSpace(u.FirstName + " " + u.LastName), u.Username} {
		if n = strings.Join(strings.Fields(n), " "); n != "" && !strings.Contains(n, "@") {
			return n
		}
	}
	return ""
}
