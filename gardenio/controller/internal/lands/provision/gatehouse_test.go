package provision

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/keycloakadmin"
)

// Step 9.12: the gatehouse.

var gatehouseGuest = []string{"/invited", "/lands-tenants", "/project-acme/guests"}

// landProject is the live project bead of land.
func (w *world) landProject(land string) *beadsapi.BeadDetail {
	w.t.Helper()
	for _, b := range w.beads {
		if b.Type == "project" && projectName(b) == land && b.Status != "closed" {
			return b
		}
	}
	w.t.Fatalf("no project %s", land)
	return nil
}

// gatehouse is a provisioner whose gatehouse is acme.
func (w *world) gatehouse() *Provisioner {
	p := w.provisioner()
	p.Gatehouse = "acme"
	return p
}

// An invite to sign in only makes its person a gatehouse guest, and says so
// on the invite.
func TestGatehouse_ClaimJoinsTheGatehouse(t *testing.T) {
	w := openLand(t)
	p := w.gatehouse()
	pass(t, p) // the backfill: nobody is in invited yet
	inv := w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	w.gitHubUser(guestSub, guestEmail, true)
	pass(t, p)
	if got := w.tokenGroups(guestSub); !slices.Equal(got, gatehouseGuest) {
		t.Fatalf("token names %v, want %v", got, gatehouseGuest)
	}
	if inv.Fields["state"] != InviteClaimed || inv.Fields["claimed_gatehouse"] != "acme" || inv.Status != "closed" {
		t.Fatalf("invite = %s %v", inv.Status, inv.Fields)
	}
}

// Someone who already holds a land is let in and left where they are.
func TestGatehouse_ClaimLeavesALandHolderOut(t *testing.T) {
	w := openLand(t)
	w.groups["project-other"] = map[string]bool{guestSub: true}
	p := w.gatehouse()
	pass(t, p)
	inv := w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	w.gitHubUser(guestSub, guestEmail, true)
	pass(t, p)
	if got := w.tokenGroups(guestSub); !slices.Equal(got, []string{"/invited", "/project-other"}) {
		t.Fatalf("token names %v", got)
	}
	if inv.Fields["state"] != InviteClaimed || inv.Fields["claimed_gatehouse"] != "" {
		t.Fatalf("invite = %v", inv.Fields)
	}
}

// An invite to a land joins that land, never the gatehouse.
func TestGatehouse_LandInviteSkipsTheGatehouse(t *testing.T) {
	w := openLand(t)
	p := w.gatehouse()
	p.Gatehouse = "lobby"
	inv := w.signinInvite(guestEmail, "acme", "member", t0.Add(InviteTTL))
	w.gitHubUser(guestSub, guestEmail, true)
	pass(t, p)
	if got := w.tokenGroups(guestSub); !slices.Equal(got, []string{"/invited", "/lands-tenants", "/project-acme"}) {
		t.Fatalf("token names %v", got)
	}
	if inv.Fields["claimed_gatehouse"] != "" {
		t.Fatalf("invite = %v", inv.Fields)
	}
}

// A claim before the gatehouse opens lets the person in; the backfill takes
// them into the gatehouse once it opens, once.
func TestGatehouse_BackfillWhenItOpens(t *testing.T) {
	w := newWorld(t)
	p := w.gatehouse()
	inv := w.signinInvite(guestEmail, "", "", t0.Add(InviteTTL))
	w.gitHubUser(guestSub, guestEmail, true)
	pass(t, p)
	if got := w.tokenGroups(guestSub); !slices.Equal(got, []string{"/invited"}) || inv.Fields["claimed_gatehouse"] != "" {
		t.Fatalf("before the gatehouse: token names %v, invite %v", got, inv.Fields)
	}
	// Someone in invited by hand, and the land's own applicant, who will own
	// it.
	w.groups[keycloakadmin.InvitedGroup][otherSub] = true
	w.groups[keycloakadmin.InvitedGroup][applicantSub] = true

	w.application(StateApproved)
	pass(t, p)
	proj := w.landProject("acme")
	if proj.Fields[GatehouseField] != "true" || proj.Fields[GatehouseBackfilledField] != t0.Format(time.RFC3339) {
		t.Fatalf("project = %v", proj.Fields)
	}
	for _, sub := range []string{guestSub, otherSub} {
		if got := w.tokenGroups(sub); !slices.Equal(got, gatehouseGuest) {
			t.Fatalf("%s: token names %v, want %v", sub, got, gatehouseGuest)
		}
	}
	if got := w.tokenGroups(applicantSub); !slices.Equal(got, []string{"/invited", "/lands-tenants", "/project-acme/owners"}) {
		t.Fatalf("the owner's token names %v", got)
	}

	// Done once: someone taken out by hand stays out.
	delete(w.groups["project-acme/guests"], otherSub)
	w.ops = nil
	pass(t, p)
	if got := w.tokenGroups(otherSub); slices.Contains(got, "/project-acme/guests") {
		t.Fatalf("the backfill ran again: %v", got)
	}
	if len(w.ops) != 0 {
		t.Fatalf("a settled gatehouse wrote %v", w.ops)
	}
}

// A backfill that fails stamps nothing and runs again on the next pass.
func TestGatehouse_BackfillRetries(t *testing.T) {
	w := openLand(t)
	w.groups[keycloakadmin.InvitedGroup][guestSub] = true
	p := w.gatehouse()
	w.fail["kc:lands"] = errors.New("keycloak down")
	if err := p.Pass(context.Background()); err == nil {
		t.Fatal("the pass hid the failure")
	}
	if proj := w.landProject("acme"); proj.Fields[GatehouseBackfilledField] != "" {
		t.Fatalf("stamped after a failure: %v", proj.Fields)
	}
	delete(w.fail, "kc:lands")
	pass(t, p)
	if got := w.tokenGroups(guestSub); !slices.Equal(got, gatehouseGuest) {
		t.Fatalf("token names %v", got)
	}
	if proj := w.landProject("acme"); proj.Fields[GatehouseBackfilledField] == "" {
		t.Fatalf("not stamped: %v", proj.Fields)
	}
}

// A land that opened before it was named the gatehouse is marked by the pass;
// one provisioned as the gatehouse is marked at birth. Any other land is
// never marked.
func TestGatehouse_Mark(t *testing.T) {
	w := openLand(t)
	if v := w.landProject("acme").Fields[GatehouseField]; v != "" {
		t.Fatalf("an ordinary land is marked %q", v)
	}
	pass(t, w.gatehouse())
	if v := w.landProject("acme").Fields[GatehouseField]; v != "true" {
		t.Fatalf("the gatehouse is marked %q", v)
	}

	w = newWorld(t)
	w.application(StateApproved)
	p := w.gatehouse()
	p.Gatehouse = "lobby"
	pass(t, p)
	if v := w.landProject("acme").Fields[GatehouseField]; v != "" {
		t.Fatalf("acme marked %q when the gatehouse is lobby", v)
	}
}

// With no gatehouse, or one that is not open, the pass writes nothing.
func TestGatehouse_NoneOrClosed(t *testing.T) {
	for name, p := range map[string]func(w *world) *Provisioner{
		"none":     func(w *world) *Provisioner { return w.provisioner() },
		"not open": func(w *world) *Provisioner { p := w.gatehouse(); p.Gatehouse = "lobby"; return p },
	} {
		t.Run(name, func(t *testing.T) {
			w := openLand(t)
			w.groups[keycloakadmin.InvitedGroup][guestSub] = true
			pass(t, p(w))
			if len(w.ops) != 0 || len(w.tokenGroups(guestSub)) != 1 {
				t.Fatalf("ops %v, groups %v", w.ops, w.tokenGroups(guestSub))
			}
		})
	}
}
