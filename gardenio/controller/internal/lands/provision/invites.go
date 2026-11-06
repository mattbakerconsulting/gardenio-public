package provision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/mailer"
)

// Member invites (Track B spec §3.7, step 8.9). A land's owner invites a
// GitHub user from the Commons; the viewer files a land_invite bead in the
// land carrying the GitHub numeric id it resolved (kbeads land_invite.go). The
// invite pass, which runs after provisioning on every Pass, is the door:
//
//   - join on sign-in: a pending invite whose github_id belongs to exactly one
//     Keycloak user (the GitHub IdP writes github_id on first sign-in, §2.2)
//     joins that user to the land with the invite's role and to lands-tenants,
//     wakes the land's steward to greet them (step 9.14; not for a demo pass),
//     then becomes joined. No user, an empty lookup, more than one user, or a
//     user whose github_id is not exactly the invite's: nobody joins, and the
//     invite waits.
//   - expiry: a pending invite past expires_at becomes expired and closes; it
//     never joins anyone after that, whoever signs in.
//   - demo pass: in the demo land (DemoLand, the viewer's /lands/demo door,
//     gd-c6LzZkk0qy) a joined guest is taken out DemoGuestTTL after
//     joined_at, as a revoke would; they may ask the door for another pass.
//   - revoke: an owner's revoke (revoke_requested_at, written by the viewer)
//     ends a pending invite, or takes a joined user out of every group of the
//     land, and out of lands-tenants when it was their last land. Group
//     membership is read when a token is minted, so the user's next token
//     (within the realm's 5-minute access token lifespan) no longer names the
//     land.
//
// The door never touches an owner: a person who already holds a role in the
// land is not re-added (the invite records that and their role is unchanged),
// and a revoke never removes someone who holds the owner role. Roles are
// Commons step 4.2's; this is only the door.
//
// Every write is idempotent and Keycloak goes first, so a controller that
// dies part-way finishes on the next pass.

// InviteType is the bead type of a member invite.
const InviteType = "land_invite"

// Invite states (kbeads land_invite.go).
const (
	InvitePending = "pending"
	InviteJoined  = "joined"
	InviteRevoked = "revoked"
	InviteExpired = "expired"
)

// InviteTTL is how long an invite waits for its person to sign in (§3.7).
const InviteTTL = 14 * 24 * time.Hour

// Invite is a land_invite as the provisioner reads it.
type Invite struct {
	ID                string
	Land              string
	GitHubID          int64
	GitHubLogin       string
	Role              keycloakadmin.LandRole
	State             string
	ExpiresAt         time.Time // zero when unreadable, which reads as expired
	JoinedUserID      string
	JoinedAt          time.Time // zero until joined, or when unreadable
	RevokeRequestedAt string
}

// InviteFromBead reads b, or reports false when b is not a land_invite whose
// one project label is its land.
func InviteFromBead(b *beadsapi.BeadDetail) (*Invite, bool) {
	if b == nil || b.Type != InviteType {
		return nil, false
	}
	f := b.Fields
	land := f["land"]
	var projects []string
	for _, l := range b.Labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok {
			projects = append(projects, p)
		}
	}
	if land == "" || len(projects) != 1 || projects[0] != land {
		return nil, false
	}
	id, _ := strconv.ParseInt(strings.TrimSpace(f["github_id"]), 10, 64)
	exp, _ := time.Parse(time.RFC3339, f["expires_at"])
	joined, _ := time.Parse(time.RFC3339, f["joined_at"])
	return &Invite{
		ID: b.ID, Land: land, GitHubID: id, GitHubLogin: f["github_login"],
		Role: keycloakadmin.LandRole(f["role"]), State: f["state"], ExpiresAt: exp,
		JoinedUserID: f["joined_user_id"], JoinedAt: joined, RevokeRequestedAt: f["revoke_requested_at"],
	}, true
}

// Invites runs the invite pass: every open invite is joined, expired or
// revoked as it should be. One invite's failure never stops the others; it
// is logged and retried on the next pass.
func (p *Provisioner) Invites(ctx context.Context) error {
	beads, err := listAll(ctx, p.Daemon, beadsapi.ListBeadsQuery{Types: []string{InviteType}, Statuses: liveStatuses})
	if err != nil {
		return fmt.Errorf("reading invites: %w", err)
	}
	lands := map[string]bool{} // land → open for joining, read once per pass
	for _, b := range beads {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		inv, ok := InviteFromBead(b)
		if !ok {
			p.logger().Warn("lands invites: skipping a land_invite that does not name its land in exactly one project label", "invite", b.ID)
			continue
		}
		if err := p.invite(ctx, inv, lands); err != nil {
			p.logger().Warn("lands invites: invite not settled; retrying next pass", "invite", inv.ID, "land", inv.Land, "error", err)
		}
	}
	return nil
}

// invite settles one invite.
func (p *Provisioner) invite(ctx context.Context, inv *Invite, lands map[string]bool) error {
	switch {
	case inv.State == InviteJoined && inv.RevokeRequestedAt != "":
		return p.revokeJoined(ctx, inv, "")
	case inv.State == InviteJoined && p.demoPassOver(inv):
		return p.endDemoPass(ctx, inv)
	case inv.State == InviteJoined:
		return nil
	case inv.State != InvitePending:
		// Revoked and expired invites are closed when they end; one left
		// open by a crash is closed now.
		return p.Daemon.CloseBead(ctx, inv.ID, nil)
	case inv.RevokeRequestedAt != "":
		return p.endInvite(ctx, inv, InviteRevoked, map[string]any{"revoked_at": p.stamp()})
	case inv.ExpiresAt.IsZero() || !p.now().Before(inv.ExpiresAt):
		return p.endInvite(ctx, inv, InviteExpired, nil)
	}
	if inv.GitHubID <= 0 || (inv.Role != keycloakadmin.RoleMember && inv.Role != keycloakadmin.RoleGuest) {
		return fmt.Errorf("invite carries github_id %d and role %q; it can never join anyone", inv.GitHubID, inv.Role)
	}
	open, ok := lands[inv.Land]
	if !ok {
		var err error
		if open, err = p.landOpen(ctx, inv.Land); err != nil {
			return err
		}
		lands[inv.Land] = open
	}
	if !open {
		return nil // the land is not (or no longer) a provisioned tenant land; the invite waits or expires
	}
	return p.join(ctx, inv)
}

// landOpen reports whether land is a provisioned tenant land: its live
// project bead says land_kind=tenant and has land_opened_at.
func (p *Provisioner) landOpen(ctx context.Context, land string) (bool, error) {
	if _, err := keycloakadmin.GroupName(land); err != nil || slices.Contains(p.ReservedLands, land) {
		return false, nil
	}
	b, err := p.project(ctx, land)
	if err != nil || b == nil {
		return false, err
	}
	return b.Fields["land_kind"] == "tenant" && b.Fields["land_opened_at"] != "" && b.Fields["land_closed_at"] == "", nil
}

// join adds the invite's person to the land once they have signed in.
func (p *Provisioner) join(ctx context.Context, inv *Invite) error {
	log := p.logger().With("invite", inv.ID, "land", inv.Land)
	u, err := p.Groups.FindUserByGitHubID(ctx, inv.GitHubID)
	switch {
	case errors.Is(err, keycloakadmin.ErrUserNotFound):
		return nil // not signed in yet
	case errors.Is(err, keycloakadmin.ErrAmbiguousUser):
		log.Warn("lands invites: more than one Keycloak user has this github_id; joining nobody", "github_id", inv.GitHubID)
		return nil
	case err != nil:
		return err
	}
	if u.ID == "" || u.GitHubID != inv.GitHubID {
		// Never trust the lookup alone: the user must carry exactly this id.
		log.Warn("lands invites: the github_id lookup answered with another user; joining nobody", "github_id", inv.GitHubID)
		return nil
	}
	note, err := p.joinLand(ctx, inv.Land, inv.Role, u.ID)
	if err != nil {
		return err
	}
	if note == "" {
		// The mail goes before the state change: a crash between the two
		// sends it twice, which beats never sending it.
		p.sendJoinedMail(ctx, inv, u)
		// A demo pass is a day's look around, not a person joining the land:
		// its steward is not woken for every pass the door hands out.
		if inv.Land != p.DemoLand {
			p.greet(ctx, inv.Land, u, inv.Role, "lands-invite:"+inv.ID+":greet")
		}
	}
	fields := map[string]any{"state": InviteJoined, "joined_user_id": u.ID, "joined_at": p.stamp()}
	if note != "" {
		fields["outcome_note"] = note
	}
	if err := p.Daemon.UpdateBeadFieldsTyped(ctx, inv.ID, fields); err != nil {
		return fmt.Errorf("marking the invite joined: %w", err)
	}
	log.Info("lands invites: joined", "user", u.ID, "role", inv.Role, "note", note)
	return nil
}

// joinLand gives userID role in land, and makes them a tenant. Someone who
// already holds a role in the land keeps it: nothing is written, and the
// note says so (it is "" when they joined).
func (p *Provisioner) joinLand(ctx context.Context, land string, role keycloakadmin.LandRole, userID string) (string, error) {
	members, err := p.Groups.LandMembers(ctx, land)
	if err != nil {
		return "", err
	}
	if i := slices.IndexFunc(members, func(m keycloakadmin.LandMember) bool { return m.ID == userID }); i >= 0 {
		return fmt.Sprintf("already held the %s role in the land when they signed in; their role is unchanged", members[i].Role), nil
	}
	if role != keycloakadmin.RoleMember {
		if _, err := p.Groups.EnsureLandRoleGroup(ctx, land, role); err != nil {
			return "", err
		}
	}
	if err := p.Groups.AddLandMember(ctx, land, role, userID); err != nil {
		return "", err
	}
	return "", p.Groups.AddTenant(ctx, userID)
}

// DefaultDemoGuestTTL is how long a demo guest pass lasts after the join
// when DemoGuestTTL is unset.
const DefaultDemoGuestTTL = 24 * time.Hour

func (p *Provisioner) demoGuestTTL() time.Duration {
	if p.DemoGuestTTL > 0 {
		return p.DemoGuestTTL
	}
	return DefaultDemoGuestTTL
}

// demoPassOver reports whether inv is a joined guest pass in the demo land
// whose time is up. An unreadable joined_at counts as up: the door hands out
// another pass, so ending one early costs a click, never access.
func (p *Provisioner) demoPassOver(inv *Invite) bool {
	if p.DemoLand == "" || inv.Land != p.DemoLand || inv.Role != keycloakadmin.RoleGuest {
		return false
	}
	return inv.JoinedAt.IsZero() || !p.now().Before(inv.JoinedAt.Add(p.demoGuestTTL()))
}

// endDemoPass ends a demo guest pass whose time is up. The pass only ever
// made its person a guest: one who holds a higher role in the demo land
// (LandMembers names the highest) got it another way, and keeps it.
func (p *Provisioner) endDemoPass(ctx context.Context, inv *Invite) error {
	note := fmt.Sprintf("demo guest pass ended %s after joining", p.demoGuestTTL())
	if inv.JoinedUserID != "" {
		members, err := p.Groups.LandMembers(ctx, inv.Land)
		if err != nil && !errors.Is(err, keycloakadmin.ErrGroupNotFound) {
			return err
		}
		i := slices.IndexFunc(members, func(m keycloakadmin.LandMember) bool { return m.ID == inv.JoinedUserID })
		if i >= 0 && members[i].Role != keycloakadmin.RoleGuest {
			return p.endInvite(ctx, inv, InviteRevoked, map[string]any{
				"revoked_at": p.stamp(), "outcome_note": fmt.Sprintf("%s; they hold the %s role, which the pass did not give, and keep it", note, members[i].Role),
			})
		}
	}
	return p.revokeJoined(ctx, inv, note)
}

// revokeJoined takes a joined person out of the land, then ends the invite.
// An owner is never removed this way. note, when set, says why on the invite.
func (p *Provisioner) revokeJoined(ctx context.Context, inv *Invite, note string) error {
	extra := map[string]any{"revoked_at": p.stamp()}
	if note != "" {
		extra["outcome_note"] = note
	}
	if inv.JoinedUserID == "" {
		return p.endInvite(ctx, inv, InviteRevoked, extra)
	}
	members, err := p.Groups.LandMembers(ctx, inv.Land)
	if err != nil && !errors.Is(err, keycloakadmin.ErrGroupNotFound) {
		return err
	}
	i := slices.IndexFunc(members, func(m keycloakadmin.LandMember) bool { return m.ID == inv.JoinedUserID })
	switch {
	case i >= 0 && members[i].Role == keycloakadmin.RoleOwner:
		extra["outcome_note"] = "holds the owner role; revoking an invite never removes an owner"
	case i >= 0:
		if err := p.Groups.RemoveFromLand(ctx, inv.Land, inv.JoinedUserID); err != nil {
			return err
		}
		fallthrough
	default:
		// Out of the land (now, or already): out of lands-tenants too when
		// it was their last land. Re-checked every time, so a crash after
		// the removal still finishes.
		left, err := p.Groups.UserLands(ctx, inv.JoinedUserID)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			if err := p.Groups.RemoveTenant(ctx, inv.JoinedUserID); err != nil && !errors.Is(err, keycloakadmin.ErrGroupNotFound) {
				return err
			}
		}
	}
	return p.endInvite(ctx, inv, InviteRevoked, extra)
}

// endInvite moves the invite to a terminal state and closes it.
func (p *Provisioner) endInvite(ctx context.Context, inv *Invite, state string, extra map[string]any) error {
	return p.endBead(ctx, inv.ID, inv.Land, state, extra)
}

// endBead moves the member or sign-in invite id to a terminal state and
// closes it. land is for the log.
func (p *Provisioner) endBead(ctx context.Context, id, land, state string, extra map[string]any) error {
	fields := map[string]any{"state": state}
	for k, v := range extra {
		fields[k] = v
	}
	if err := p.Daemon.UpdateBeadFieldsTyped(ctx, id, fields); err != nil {
		return fmt.Errorf("marking the invite %s: %w", state, err)
	}
	if err := p.Daemon.CloseBead(ctx, id, nil); err != nil {
		return fmt.Errorf("closing the %s invite: %w", state, err)
	}
	p.logger().Info("lands invites: ended", "invite", id, "land", land, "state", state)
	return nil
}

// sendJoinedMail tells the person they are in, when mail is on and Keycloak
// knows their address.
func (p *Provisioner) sendJoinedMail(ctx context.Context, inv *Invite, u keycloakadmin.User) {
	if p.Templates == nil || p.Mailer == nil || !mailer.Enabled(p.Mailer) || u.Email == "" {
		return
	}
	m, err := p.Templates.Render(mailer.KindLandJoined, u.Email, mailer.Data{Name: inv.GitHubLogin, Land: inv.Land})
	if err != nil {
		p.logger().Warn("lands invites: mail render failed", "invite", inv.ID, "error", err)
		return
	}
	if err := p.Mailer.Send(ctx, m); err != nil {
		p.logger().Warn("lands invites: mail send failed", "invite", inv.ID, "error", err)
	}
}

func (p *Provisioner) stamp() string { return p.now().UTC().Format(time.RFC3339) }
