package provision

import (
	"errors"
	"slices"
	"testing"

	"gardenio/controller/internal/keycloakadmin"
)

// Step 9.14: the steward greets a person when they join its land.

// greeted is who land's greetings name and as what, oldest first.
func greeted(w *world, land string) []string {
	var out []string
	for _, b := range w.greetings(land) {
		out = append(out, b.Fields["name"]+"/"+b.Fields["role"])
	}
	return out
}

// A claim that makes its person a gatehouse guest greets them once, by name,
// and a later pass greets nobody again.
func TestGreet_GatehouseClaimGreetsOnce(t *testing.T) {
	w := openLand(t)
	p := w.gatehouse()
	pass(t, p)
	w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	u := w.gitHubUser(guestSub, guestEmail, true)
	u.FirstName, u.LastName = "Ada", "Lovelace"
	pass(t, p)
	pass(t, p)
	if got := greeted(w, "acme"); !slices.Equal(got, []string{"Ada Lovelace/guest"}) {
		t.Fatalf("greetings = %v, want one for Ada as a guest", got)
	}
}

// The backfill lets people in without a greeting each: they never claimed
// anything just now.
func TestGreet_BackfillGreetsNobody(t *testing.T) {
	w := newWorld(t)
	p := w.gatehouse()
	w.groups[keycloakadmin.InvitedGroup][otherSub] = true
	w.application(StateApproved)
	pass(t, p)
	if got := w.tokenGroups(otherSub); !slices.Equal(got, gatehouseGuest) {
		t.Fatalf("setup: token names %v", got)
	}
	if got := greeted(w, "acme"); len(got) != 0 {
		t.Fatalf("the backfill greeted %v", got)
	}
}

// A sign-in invite to a land greets the person there with the invite's role;
// one who already held a role there is not greeted.
func TestGreet_SigninInviteToALand(t *testing.T) {
	w := openLand(t)
	w.signinInvite(guestEmail, "acme", "member", t0.Add(InviteTTL))
	w.gitHubUser(guestSub, guestEmail, true)
	pass(t, w.provisioner())
	// The username is the email address, so the greeting names nobody.
	if got := greeted(w, "acme"); !slices.Equal(got, []string{"/member"}) {
		t.Fatalf("greetings = %v, want one, nameless, for a member", got)
	}

	w = openLand(t)
	w.users[0].EmailVerified, w.users[0].IdentityProviders = true, []string{keycloakadmin.GitHubProvider}
	w.signinInvite("octo@example.com", "acme", "guest", t0.Add(InviteTTL))
	pass(t, w.provisioner())
	if got := greeted(w, "acme"); len(got) != 0 {
		t.Fatalf("the owner was greeted as a newcomer: %v", got)
	}
}

// A joined member invite greets the person; a demo pass does not.
func TestGreet_MemberInviteButNotADemoPass(t *testing.T) {
	w := openLand(t)
	w.inviteBead("acme", "4242", "member", t0.Add(InviteTTL))
	w.signIn(inviteeSub, inviteeGH)
	pass(t, w.provisioner())
	pass(t, w.provisioner())
	if got := greeted(w, "acme"); len(got) != 1 || got[0] != PersonName(w.users[len(w.users)-1])+"/member" {
		t.Fatalf("greetings = %v, want one for the member", got)
	}

	w = openLand(t)
	w.inviteBead("acme", "4242", "guest", t0.Add(InviteTTL))
	w.signIn(inviteeSub, inviteeGH)
	p := w.provisioner()
	p.DemoLand = "acme"
	pass(t, p)
	if !inLand(w, inviteeSub) {
		t.Fatal("setup: the demo guest was not let in")
	}
	if got := greeted(w, "acme"); len(got) != 0 {
		t.Fatalf("a demo pass was greeted: %v", got)
	}
}

// A greeting that cannot be posted never keeps the person out.
func TestGreet_FailureStillLetsThePersonIn(t *testing.T) {
	w := openLand(t)
	p := w.gatehouse()
	pass(t, p)
	w.fail = map[string]error{"steward:greet": errors.New("daemon down")}
	inv := w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	w.gitHubUser(guestSub, guestEmail, true)
	pass(t, p)
	if got := w.tokenGroups(guestSub); !slices.Equal(got, gatehouseGuest) || inv.Fields["state"] != InviteClaimed {
		t.Fatalf("token names %v, invite %v", got, inv.Fields)
	}
}

// Each pass hands the land's open steward to Upgrade, so one bound before
// the person-joined wake hears it.
func TestGreet_PassUpgradesTheOpenSteward(t *testing.T) {
	w := newWorld(t)
	p, trig := stewardedLand(t, w)
	w.upgraded = nil
	pass(t, p)
	if !slices.Equal(w.upgraded, []string{"acme:" + trig.ID}) {
		t.Fatalf("upgraded = %v", w.upgraded)
	}
}

func TestPersonName(t *testing.T) {
	for _, c := range []struct {
		u    keycloakadmin.User
		want string
	}{
		{keycloakadmin.User{FirstName: " Ada ", LastName: "Lovelace", Username: "ada"}, "Ada Lovelace"},
		{keycloakadmin.User{FirstName: "Ada", Username: "ada"}, "Ada"},
		{keycloakadmin.User{Username: "octocat"}, "octocat"},
		{keycloakadmin.User{Username: "ada@example.com", Email: "ada@example.com"}, ""},
		{keycloakadmin.User{FirstName: "ada@example.com", Username: "adal"}, "adal"},
		{keycloakadmin.User{}, ""},
	} {
		if got := PersonName(c.u); got != c.want {
			t.Errorf("PersonName(%+v) = %q, want %q", c.u, got, c.want)
		}
	}
}
