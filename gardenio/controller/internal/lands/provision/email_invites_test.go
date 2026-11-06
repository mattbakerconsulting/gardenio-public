package provision

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/mailer"
)

// Sign-in invites by email (gd-ycE484kLyk).

const (
	guestEmail = "someone@example.com"
	guestSub   = "0f6b1a8e-eeee-4eee-8fff-000033334444"
)

// signinInvite files a pending sign-in invite as gb lands invite would; land
// and role are "" for an invite to sign in only.
func (w *world) signinInvite(email, land, role string, expires time.Time) *beadsapi.BeadDetail {
	f := map[string]string{
		"email": email, "invited_by": "oidc:operator@example.com",
		"expires_at": expires.UTC().Format(time.RFC3339), "state": InvitePending,
	}
	if land != "" {
		f["land"], f["role"] = land, role
	}
	return w.add(SigninInviteType, "Invite "+email, "open", nil, f)
}

// gitHubUser is the user the GitHub first broker login makes: the email
// GitHub gave, unverified until the person confirms it, and the github link.
func (w *world) gitHubUser(sub, email string, verified bool) *keycloakadmin.User {
	w.users = append(w.users, keycloakadmin.User{
		ID: sub, Username: email, Email: email, EmailVerified: verified, GitHubID: 777,
		IdentityProviders: []string{keycloakadmin.GitHubProvider},
	})
	return &w.users[len(w.users)-1]
}

func TestSigninInvites_MailThenClaimOnceVerified(t *testing.T) {
	w := newWorld(t)
	inv := w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	p := w.provisioner()

	// Nobody has signed in: the invite is mailed, and waits.
	pass(t, p)
	sent := w.mail.SentKind(mailer.KindInvite)
	if len(sent) != 1 || !slices.Equal(sent[0].To, []string{guestEmail}) || !strings.Contains(sent[0].Text, "operator@example.com invited you") {
		t.Fatalf("invite mail = %+v", sent)
	}
	if inv.Fields["mailed_at"] == "" || inv.Fields["state"] != InvitePending || inv.Status != "open" {
		t.Fatalf("after mailing: %s %v", inv.Status, inv.Fields)
	}

	// They sign in with GitHub; the realm has not seen them confirm the address.
	u := w.gitHubUser(guestSub, guestEmail, false)
	w.ops = nil
	pass(t, p)
	if inv.Fields["state"] != InvitePending || len(w.tokenGroups(guestSub)) != 0 || len(w.ops) != 0 {
		t.Fatalf("an unverified email claimed: state %q ops %v", inv.Fields["state"], w.ops)
	}

	// They confirm it: the next pass lets them in.
	u.EmailVerified = true
	pass(t, p)
	if got := w.tokenGroups(guestSub); !slices.Equal(got, []string{"/invited"}) {
		t.Fatalf("after claiming the token names %v", got)
	}
	if inv.Fields["state"] != InviteClaimed || inv.Fields["claimed_user_id"] != guestSub || inv.Fields["claimed_at"] == "" || inv.Status != "closed" {
		t.Fatalf("invite = %s %v", inv.Status, inv.Fields)
	}
	if n := len(w.mail.SentKind(mailer.KindInvite)); n != 1 {
		t.Fatalf("the invite was mailed %d times", n)
	}

	// Claimed is settled: the next pass reads it no more.
	w.ops = nil
	pass(t, p)
	if len(w.ops) != 0 {
		t.Fatalf("a claimed invite wrote %v", w.ops)
	}
}

func TestSigninInvites_OnlyAVerifiedGitHubUserWithThatEmailClaims(t *testing.T) {
	cases := map[string]func(w *world){
		"unverified": func(w *world) { w.gitHubUser(guestSub, guestEmail, false) },
		// A realm user with the address who does not sign in with GitHub.
		"no github link": func(w *world) {
			u := w.gitHubUser(guestSub, guestEmail, true)
			u.IdentityProviders = []string{}
		},
		"another provider": func(w *world) {
			u := w.gitHubUser(guestSub, guestEmail, true)
			u.IdentityProviders = []string{"gitlab"}
		},
		"different email": func(w *world) { w.gitHubUser(guestSub, "someone@example.org", true) },
		"different case":  func(w *world) { w.gitHubUser(guestSub, "Someone@example.com", true) },
		// A server that matches loosely answers with someone else.
		"loose lookup": func(w *world) {
			w.emailLookup = func(string) (keycloakadmin.User, error) {
				return keycloakadmin.User{ID: guestSub, Email: "nosomeone@example.com", EmailVerified: true,
					IdentityProviders: []string{keycloakadmin.GitHubProvider}}, nil
			}
		},
		// FGAP v2 without view-users answers 200 with nothing in it.
		"empty lookup": func(w *world) {
			w.emailLookup = func(string) (keycloakadmin.User, error) { return keycloakadmin.User{}, nil }
		},
		"ambiguous": func(w *world) {
			w.gitHubUser(guestSub, guestEmail, true)
			w.gitHubUser(otherSub, guestEmail, true)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			inv := w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
			setup(w)
			pass(t, w.provisioner())
			if len(w.tokenGroups(guestSub)) != 0 || len(w.tokenGroups(otherSub)) != 0 || inv.Fields["state"] != InvitePending || inv.Status != "open" {
				t.Fatalf("let someone in: state %q groups %v", inv.Fields["state"], w.tokenGroups(guestSub))
			}
			if slices.ContainsFunc(w.ops, func(op string) bool { return strings.HasPrefix(op, "kc:") }) {
				t.Fatalf("wrote to Keycloak: %v", w.ops)
			}
		})
	}
}

func TestSigninInvites_LandInviteJoinsTheLand(t *testing.T) {
	for role, want := range map[string][]string{
		"member": {"/invited", "/lands-tenants", "/project-acme"},
		"guest":  {"/invited", "/lands-tenants", "/project-acme/guests"},
	} {
		t.Run(role, func(t *testing.T) {
			w := openLand(t)
			inv := w.signinInvite(guestEmail, "acme", role, t0.Add(InviteTTL))
			w.gitHubUser(guestSub, guestEmail, true)
			pass(t, w.provisioner())
			if got := w.tokenGroups(guestSub); !slices.Equal(got, want) {
				t.Fatalf("token names %v, want %v", got, want)
			}
			if inv.Fields["state"] != InviteClaimed || inv.Status != "closed" {
				t.Fatalf("invite = %s %v", inv.Status, inv.Fields)
			}
			sent := w.mail.SentKind(mailer.KindInvite)
			if len(sent) != 1 || !strings.Contains(sent[0].Text, "to the land acme") {
				t.Fatalf("invite mail = %+v", sent)
			}
		})
	}
}

// The land's owner, invited by email, keeps their role and is let in.
func TestSigninInvites_LandInviteKeepsAHeldRole(t *testing.T) {
	w := openLand(t)
	w.users[0].EmailVerified, w.users[0].IdentityProviders = true, []string{keycloakadmin.GitHubProvider}
	inv := w.signinInvite("octo@example.com", "acme", "guest", t0.Add(InviteTTL))
	pass(t, w.provisioner())
	if got := w.tokenGroups(applicantSub); !slices.Equal(got, []string{"/invited", "/lands-tenants", "/project-acme/owners"}) {
		t.Fatalf("owner's token names %v", got)
	}
	if _, ok := w.groups["project-acme/guests"]; ok {
		t.Fatal("an owner's invite made the guests group")
	}
	if inv.Fields["state"] != InviteClaimed || !strings.Contains(inv.Fields["outcome_note"], "already held the owner role") {
		t.Fatalf("invite = %v", inv.Fields)
	}
}

// An invite to a land that is not an open tenant land waits, and lets nobody
// in until the land opens.
func TestSigninInvites_LandInviteWaitsForTheLand(t *testing.T) {
	w := newWorld(t)
	w.add("project", "Project: sprout", "open", nil, map[string]string{"land_kind": "tenant"})
	inv := w.signinInvite(guestEmail, "sprout", "member", t0.Add(InviteTTL))
	w.gitHubUser(guestSub, guestEmail, true)
	pass(t, w.provisioner())
	if inv.Fields["state"] != InvitePending || len(w.tokenGroups(guestSub)) != 0 {
		t.Fatalf("state %q groups %v", inv.Fields["state"], w.tokenGroups(guestSub))
	}
}

func TestSigninInvites_ExpiryAndRevoke(t *testing.T) {
	w := newWorld(t)
	expired := w.signinInvite(guestEmail, "", "", t0)
	unreadable := w.signinInvite(guestEmail, "", "", t0.Add(time.Hour))
	unreadable.Fields["expires_at"] = "soon"
	revoked := w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	revoked.Fields["revoke_requested_at"] = t0.Format(time.RFC3339)
	w.gitHubUser(guestSub, guestEmail, true)
	pass(t, w.provisioner())
	for inv, want := range map[*beadsapi.BeadDetail]string{expired: InviteExpired, unreadable: InviteExpired, revoked: InviteRevoked} {
		if inv.Fields["state"] != want || inv.Status != "closed" {
			t.Errorf("%s = %s %v, want %s", inv.ID, inv.Status, inv.Fields, want)
		}
	}
	if revoked.Fields["revoked_at"] == "" {
		t.Error("revoked_at not stamped")
	}
	if len(w.tokenGroups(guestSub)) != 0 || len(w.mail.Sent()) != 0 {
		t.Fatalf("an ended invite let someone in (%v) or mailed (%d)", w.tokenGroups(guestSub), len(w.mail.Sent()))
	}

	// Valid a minute before the fourteen days are up; not a minute after.
	w2 := newWorld(t)
	inv := w2.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	w2.gitHubUser(guestSub, guestEmail, true)
	p := w2.provisioner()
	p.Now = func() time.Time { return t0.Add(InviteTTL + time.Minute) }
	pass(t, p)
	if inv.Fields["state"] != InviteExpired || len(w2.tokenGroups(guestSub)) != 0 {
		t.Fatalf("after 14 days: %v, groups %v", inv.Fields, w2.tokenGroups(guestSub))
	}
}

// With mail off, nothing is stamped (the mail goes out once mail is on) and
// the invite still works. A failed send is retried an hour later, not on
// every pass.
func TestSigninInvites_MailOffAndMailFailure(t *testing.T) {
	w := newWorld(t)
	inv := w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	p := w.provisioner()
	p.Mailer = mailer.Disabled{}
	pass(t, p)
	if inv.Fields["mailed_at"] != "" {
		t.Fatalf("mail off stamped mailed_at: %v", inv.Fields)
	}

	p = w.provisioner()
	w.mail.FailWith(errors.New("ses: address is not verified"))
	pass(t, p)
	w.mail.FailWith(nil)
	p.Now = func() time.Time { return t0.Add(InviteMailRetry - time.Minute) }
	pass(t, p)
	if inv.Fields["mailed_at"] != "" || len(w.mail.Sent()) != 0 {
		t.Fatalf("retried within the hour: %v, %d sent", inv.Fields, len(w.mail.Sent()))
	}
	p.Now = func() time.Time { return t0.Add(InviteMailRetry) }
	pass(t, p)
	if inv.Fields["mailed_at"] == "" || len(w.mail.SentKind(mailer.KindInvite)) != 1 {
		t.Fatalf("not retried after the hour: %v", inv.Fields)
	}

	// Mail off never stops a claim.
	w2 := newWorld(t)
	inv = w2.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	w2.gitHubUser(guestSub, guestEmail, true)
	p = w2.provisioner()
	p.Mailer = mailer.Disabled{}
	pass(t, p)
	if inv.Fields["state"] != InviteClaimed {
		t.Fatalf("mail off: %v", inv.Fields)
	}
}

func TestSigninInvites_CrashAtEveryWriteResumes(t *testing.T) {
	run := func(crashAt int) string {
		w := openLand(t)
		w.signinInvite(guestEmail, "acme", "guest", t0.Add(InviteTTL))
		w.gitHubUser(guestSub, guestEmail, true)
		w.crashAt = crashAt
		_ = w.provisioner().Pass(context.Background())
		w.revive()
		pass(t, w.provisioner())
		for _, b := range w.byType(SigninInviteType) {
			for _, k := range []string{"mailed_at", "claimed_at"} {
				if b.Fields[k] != "" {
					b.Fields[k] = "<set>"
				}
			}
		}
		return w.snapshot()
	}
	want := run(0)
	if !strings.Contains(want, "state=claimed") {
		t.Fatalf("no crash: %s", want)
	}
	for i := 1; i <= 8; i++ {
		if got := run(i); got != want {
			t.Fatalf("crash at write %d:\n%s\nwant:\n%s", i, got, want)
		}
	}
}
