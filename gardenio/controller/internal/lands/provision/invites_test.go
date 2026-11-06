package provision

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/mailer"
)

// Step 8.9: member invites.

const (
	inviteeSub = "0f6b1a8e-dddd-4eee-8fff-000011112222"
	inviteeGH  = 4242
)

// openLand returns a world holding the provisioned land acme, owned by
// applicantSub (github_id 42), with no invites yet.
func openLand(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	appBead := w.application(StateApproved)
	pass(t, w.provisioner())
	if appBead.Fields["state"] != StateProvisioned {
		t.Fatalf("setup: %v", appBead.Fields)
	}
	w.users = append(w.users, keycloakadmin.User{ID: applicantSub, Username: "octo", Email: "octo@example.com", GitHubID: 42})
	w.mail.Reset()
	w.ops = nil
	return w
}

// inviteBead files a pending invite as the viewer would.
func (w *world) inviteBead(land string, githubID string, role string, expires time.Time) *beadsapi.BeadDetail {
	return w.add(InviteType, "Invite to "+land, "open", []string{"project:" + land}, map[string]string{
		"land": land, "github_id": githubID, "github_login": "hubot", "role": role,
		"invited_by": "oidc:octo@example.com", "expires_at": expires.UTC().Format(time.RFC3339), "state": InvitePending,
	})
}

// signIn is the GitHub IdP's first broker login: the realm now has a user
// carrying github_id.
func (w *world) signIn(sub string, githubID int64) {
	w.users = append(w.users, keycloakadmin.User{ID: sub, Username: "hubot", Email: "hubot@example.com", GitHubID: githubID})
}

// tokenGroups is what the next access token minted for sub names: Keycloak
// reads group membership at mint time.
func (w *world) tokenGroups(sub string) []string {
	var out []string
	for name, members := range w.groups {
		if members[sub] {
			out = append(out, "/"+name)
		}
	}
	sort.Strings(out)
	return out
}

func inLand(w *world, sub string) bool {
	for _, g := range w.tokenGroups(sub) {
		if strings.HasPrefix(g, "/project-acme") {
			return true
		}
	}
	return false
}

func TestInvites_JoinOnSignIn(t *testing.T) {
	w := openLand(t)
	inv := w.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
	p := w.provisioner()

	// Nobody with that github_id has signed in: the invite waits.
	pass(t, p)
	if inv.Fields["state"] != InvitePending || inLand(w, inviteeSub) || len(w.ops) != 0 {
		t.Fatalf("before sign-in: state %q ops %v", inv.Fields["state"], w.ops)
	}

	w.signIn(inviteeSub, inviteeGH)
	pass(t, p)
	if got := w.tokenGroups(inviteeSub); !slices.Equal(got, []string{"/lands-tenants", "/project-acme"}) {
		t.Fatalf("after sign-in the token names %v", got)
	}
	if inv.Fields["state"] != InviteJoined || inv.Fields["joined_user_id"] != inviteeSub || inv.Fields["joined_at"] == "" || inv.Status != "open" {
		t.Fatalf("invite = %s %v", inv.Status, inv.Fields)
	}
	if sent := w.mail.SentKind(mailer.KindLandJoined); len(sent) != 1 || !slices.Equal(sent[0].To, []string{"hubot@example.com"}) {
		t.Fatalf("joined mail = %+v", sent)
	}

	// Joined is settled: the next pass writes nothing.
	w.ops = nil
	pass(t, p)
	if len(w.ops) != 0 {
		t.Fatalf("a settled invite wrote %v", w.ops)
	}
}

func TestInvites_GuestJoinsTheGuestsGroup(t *testing.T) {
	w := openLand(t)
	inv := w.inviteBead("acme", "4242", "guest", t0.Add(InviteTTL))
	w.signIn(inviteeSub, inviteeGH)
	pass(t, w.provisioner())
	if got := w.tokenGroups(inviteeSub); !slices.Equal(got, []string{"/lands-tenants", "/project-acme/guests"}) {
		t.Fatalf("guest's token names %v", got)
	}
	if inv.Fields["state"] != InviteJoined {
		t.Fatalf("invite = %v", inv.Fields)
	}
}

func TestInvites_WrongGitHubIDNeverJoins(t *testing.T) {
	cases := map[string]func(w *world){
		// Someone else signed in.
		"another id": func(w *world) { w.signIn(inviteeSub, inviteeGH+1) },
		// A server that matches attributes loosely answers with the wrong
		// user; the provisioner checks the id itself.
		"loose lookup": func(w *world) {
			w.lookup = func(int64) (keycloakadmin.User, error) {
				return keycloakadmin.User{ID: inviteeSub, GitHubID: inviteeGH + 1}, nil
			}
		},
		// FGAP v2 without view-users answers 200 with nothing in it.
		"empty lookup": func(w *world) {
			w.lookup = func(int64) (keycloakadmin.User, error) { return keycloakadmin.User{}, nil }
		},
		"lookup with no id": func(w *world) {
			w.lookup = func(id int64) (keycloakadmin.User, error) { return keycloakadmin.User{GitHubID: id}, nil }
		},
		// Two users carry the id: joining either would be a guess.
		"ambiguous": func(w *world) {
			w.signIn(inviteeSub, inviteeGH)
			w.signIn(otherSub, inviteeGH)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := openLand(t)
			inv := w.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
			setup(w)
			pass(t, w.provisioner())
			if inLand(w, inviteeSub) || inLand(w, otherSub) || inv.Fields["state"] != InvitePending || len(w.ops) != 0 {
				t.Fatalf("joined someone: state %q ops %v groups %v", inv.Fields["state"], w.ops, w.tokenGroups(inviteeSub))
			}
		})
	}
}

func TestInvites_Expiry(t *testing.T) {
	w := openLand(t)
	expired := w.inviteBead("acme", "4242", "member", t0) // expires now
	unreadable := w.inviteBead("acme", "4242", "member", t0.Add(time.Hour))
	unreadable.Fields["expires_at"] = "next week"
	w.signIn(inviteeSub, inviteeGH)
	pass(t, w.provisioner())
	for _, inv := range []*beadsapi.BeadDetail{expired, unreadable} {
		if inv.Fields["state"] != InviteExpired || inv.Status != "closed" {
			t.Errorf("%s = %s %v", inv.ID, inv.Status, inv.Fields)
		}
	}
	if inLand(w, inviteeSub) {
		t.Fatal("an expired invite joined someone")
	}

	// Still valid a minute before it runs out; not a minute after.
	w2 := openLand(t)
	inv := w2.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
	p := w2.provisioner()
	p.Now = func() time.Time { return t0.Add(InviteTTL + time.Minute) }
	w2.signIn(inviteeSub, inviteeGH)
	pass(t, p)
	if inv.Fields["state"] != InviteExpired || inLand(w2, inviteeSub) {
		t.Fatalf("after 14 days: %v, in land %v", inv.Fields, inLand(w2, inviteeSub))
	}
}

func TestInvites_RevokeTakesEffectOnNextToken(t *testing.T) {
	w := openLand(t)
	inv := w.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
	w.signIn(inviteeSub, inviteeGH)
	p := w.provisioner()
	pass(t, p)
	if !inLand(w, inviteeSub) {
		t.Fatal("setup: not joined")
	}

	// The owner revokes from the Commons.
	inv.Fields["revoke_requested_at"] = t0.Format(time.RFC3339)
	inv.Fields["revoke_requested_by"] = "oidc:octo@example.com"
	pass(t, p)
	if got := w.tokenGroups(inviteeSub); len(got) != 0 {
		t.Fatalf("after revoke the next token still names %v", got)
	}
	if inv.Fields["state"] != InviteRevoked || inv.Fields["revoked_at"] == "" || inv.Status != "closed" {
		t.Fatalf("invite = %s %v", inv.Status, inv.Fields)
	}
	// The owner is untouched.
	if got := w.tokenGroups(applicantSub); !slices.Equal(got, []string{"/lands-tenants", "/project-acme/owners"}) {
		t.Fatalf("owner's token names %v", got)
	}
}

func TestInvites_RevokeKeepsTenantWithAnotherLand(t *testing.T) {
	w := openLand(t)
	inv := w.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
	w.signIn(inviteeSub, inviteeGH)
	w.groups["project-beta"] = map[string]bool{inviteeSub: true}
	p := w.provisioner()
	pass(t, p)
	inv.Fields["revoke_requested_at"] = t0.Format(time.RFC3339)
	pass(t, p)
	if got := w.tokenGroups(inviteeSub); !slices.Equal(got, []string{"/lands-tenants", "/project-beta"}) {
		t.Fatalf("token names %v", got)
	}
}

func TestInvites_RevokePendingNeverJoins(t *testing.T) {
	w := openLand(t)
	inv := w.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
	inv.Fields["revoke_requested_at"] = t0.Format(time.RFC3339)
	p := w.provisioner()
	pass(t, p)
	if inv.Fields["state"] != InviteRevoked || inv.Status != "closed" {
		t.Fatalf("invite = %s %v", inv.Status, inv.Fields)
	}
	w.signIn(inviteeSub, inviteeGH)
	pass(t, p)
	if inLand(w, inviteeSub) {
		t.Fatal("a revoked invite joined someone")
	}
}

func TestInvites_NeverTouchesAnOwner(t *testing.T) {
	w := openLand(t)
	// The owner invites their own GitHub account as a guest, then revokes.
	inv := w.inviteBead("acme", "42", "guest", t0.Add(InviteTTL))
	p := w.provisioner()
	pass(t, p)
	if inv.Fields["state"] != InviteJoined || !strings.Contains(inv.Fields["outcome_note"], "already held the owner role") {
		t.Fatalf("invite = %v", inv.Fields)
	}
	if _, ok := w.groups["project-acme/guests"]; ok {
		t.Fatal("an owner's invite made the guests group")
	}
	if len(w.mail.Sent()) != 0 {
		t.Fatal("mailed an owner that they joined their own land")
	}
	inv.Fields["revoke_requested_at"] = t0.Format(time.RFC3339)
	pass(t, p)
	if got := w.tokenGroups(applicantSub); !slices.Equal(got, []string{"/lands-tenants", "/project-acme/owners"}) {
		t.Fatalf("revoking removed the owner: %v", got)
	}
	if inv.Fields["state"] != InviteRevoked || !strings.Contains(inv.Fields["outcome_note"], "never removes an owner") {
		t.Fatalf("invite = %v", inv.Fields)
	}
}

func TestInvites_WaitForAnOpenTenantLand(t *testing.T) {
	w := newWorld(t)
	// An operator project, a tenant land still provisioning, and a closed land.
	w.add("project", "Project: gardenio", "open", nil, map[string]string{})
	w.add("project", "Project: sprout", "open", nil, map[string]string{"land_kind": "tenant"})
	w.add("project", "Project: gone", "closed", nil, map[string]string{"land_kind": "tenant", "land_opened_at": "2026-10-01T00:00:00Z"})
	w.signIn(inviteeSub, inviteeGH)
	var invs []*beadsapi.BeadDetail
	for _, land := range []string{"gardenio", "sprout", "gone", "nowhere"} {
		invs = append(invs, w.inviteBead(land, "4242", "member", t0.Add(InviteTTL)))
	}
	pass(t, w.provisioner())
	for _, inv := range invs {
		if inv.Fields["state"] != InvitePending {
			t.Errorf("%s: %v", inv.Fields["land"], inv.Fields)
		}
	}
	if len(w.tokenGroups(inviteeSub)) != 0 || len(w.ops) != 0 {
		t.Fatalf("joined %v, ops %v", w.tokenGroups(inviteeSub), w.ops)
	}
}

func TestInvites_SkipsAnInviteOutsideItsLand(t *testing.T) {
	w := openLand(t)
	inv := w.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
	inv.Labels = []string{"project:acme", "project:beta"}
	w.signIn(inviteeSub, inviteeGH)
	pass(t, w.provisioner())
	if inLand(w, inviteeSub) || inv.Fields["state"] != InvitePending {
		t.Fatal("an invite with two project labels joined someone")
	}
}

func TestInvites_CrashAtEveryWriteResumes(t *testing.T) {
	run := func(crashAt int) (string, int) {
		w := openLand(t)
		inv := w.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
		w.signIn(inviteeSub, inviteeGH)
		w.crashAt = crashAt
		_ = w.provisioner().Pass(context.Background())
		w.revive()
		pass(t, w.provisioner())
		inv.Fields["revoke_requested_at"] = t0.Format(time.RFC3339)
		w.writes, w.crashAt = 0, crashAt
		_ = w.provisioner().Pass(context.Background())
		w.revive()
		pass(t, w.provisioner())
		for _, k := range []string{"joined_at", "revoked_at", "revoke_requested_at"} {
			if inv.Fields[k] != "" {
				inv.Fields[k] = "<set>"
			}
		}
		return w.snapshot(), w.writes
	}
	want, _ := run(0)
	for i := 1; i <= 6; i++ {
		if got, _ := run(i); got != want {
			t.Fatalf("crash at write %d:\n%s\nwant:\n%s", i, got, want)
		}
	}
}

// The demo land's guest passes (the viewer's /lands/demo door, gd-c6LzZkk0qy)
// end DemoGuestTTL after the join.
func TestInvites_DemoPassEnds(t *testing.T) {
	w := openLand(t)
	inv := w.inviteBead("acme", "4242", "guest", t0.Add(time.Hour))
	w.signIn(inviteeSub, inviteeGH)
	p := w.provisioner()
	p.DemoLand, p.DemoGuestTTL = "acme", 2*time.Hour
	pass(t, p)
	if inv.Fields["state"] != InviteJoined || !inLand(w, inviteeSub) {
		t.Fatalf("setup: %v", inv.Fields)
	}

	// Within the pass: nothing changes.
	p.Now = func() time.Time { return t0.Add(2*time.Hour - time.Minute) }
	w.ops = nil
	pass(t, p)
	if len(w.ops) != 0 || !inLand(w, inviteeSub) {
		t.Fatalf("a live pass wrote %v", w.ops)
	}

	// Past it: out of the land and of lands-tenants, the pass closed.
	p.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	pass(t, p)
	if got := w.tokenGroups(inviteeSub); len(got) != 0 {
		t.Fatalf("after the pass the token names %v", got)
	}
	if inv.Fields["state"] != InviteRevoked || inv.Status != "closed" || !strings.Contains(inv.Fields["outcome_note"], "demo guest pass ended 2h0m0s") {
		t.Fatalf("pass = %s %v", inv.Status, inv.Fields)
	}
	if got := w.tokenGroups(applicantSub); !slices.Equal(got, []string{"/lands-tenants", "/project-acme/owners"}) {
		t.Fatalf("the owner's token names %v", got)
	}
}

// A pass only ever gave the guest role: someone who holds more keeps it.
func TestInvites_DemoPassKeepsAHigherRole(t *testing.T) {
	w := openLand(t)
	inv := w.inviteBead("acme", "42", "guest", t0.Add(time.Hour)) // the owner's own GitHub account
	p := w.provisioner()
	p.DemoLand = "acme"
	pass(t, p)
	p.Now = func() time.Time { return t0.Add(DefaultDemoGuestTTL) }
	pass(t, p)
	if got := w.tokenGroups(applicantSub); !slices.Equal(got, []string{"/lands-tenants", "/project-acme/owners"}) {
		t.Fatalf("ending the pass removed the owner: %v", got)
	}
	if inv.Fields["state"] != InviteRevoked || !strings.Contains(inv.Fields["outcome_note"], "hold the owner role") {
		t.Fatalf("pass = %v", inv.Fields)
	}
}

// Outside the demo land, and for a member invite in it, a join never expires.
func TestInvites_OnlyDemoGuestPassesExpire(t *testing.T) {
	for name, c := range map[string]struct{ demo, role string }{
		"another land": {"demo", "guest"},
		"no demo land": {"", "guest"},
		"member":       {"acme", "member"},
	} {
		t.Run(name, func(t *testing.T) {
			w := openLand(t)
			inv := w.inviteBead("acme", "4242", c.role, t0.Add(InviteTTL))
			w.signIn(inviteeSub, inviteeGH)
			p := w.provisioner()
			p.DemoLand = c.demo
			pass(t, p)
			p.Now = func() time.Time { return t0.Add(30 * 24 * time.Hour) }
			pass(t, p)
			if inv.Fields["state"] != InviteJoined || !inLand(w, inviteeSub) {
				t.Fatalf("invite = %v", inv.Fields)
			}
		})
	}
}
