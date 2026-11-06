package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/oidcauth"
)

// Lands step 8.9: the viewer's half of member invites (lands_invites.go).

// fakeGitHubUsers answers logins from a map and records what it was asked
// through which installation.
type fakeGitHubUsers struct {
	users map[string]githubapp.GitHubUser
	asked []string
}

func (f *fakeGitHubUsers) UserByLogin(_ context.Context, inst int64, repo githubapp.RepoRef, login string) (githubapp.GitHubUser, error) {
	f.asked = append(f.asked, strings.Join([]string{repo.String(), login}, " ")+" via "+time.Duration(inst).String())
	if u, ok := f.users[strings.ToLower(login)]; ok {
		return u, nil
	}
	return githubapp.GitHubUser{}, githubapp.ErrNoGitHubUser
}

// machineCheckingIntake fails any call that does not carry the machine
// principal or that still carries the request's bearer.
type machineCheckingIntake struct{ *fakeIntake }

func checkMachine(ctx context.Context) error {
	if beadsapi.MachinePrincipalFromContext(ctx) == nil || beadsapi.UserBearerFromContext(ctx) != "" {
		return errors.New("not the machine principal")
	}
	return nil
}

func (m machineCheckingIntake) CreateBeadDetail(ctx context.Context, req beadsapi.CreateBeadRequest) (*beadsapi.BeadDetail, error) {
	if err := checkMachine(ctx); err != nil {
		return nil, err
	}
	return m.fakeIntake.CreateBeadDetail(ctx, req)
}

func (m machineCheckingIntake) UpdateBeadFieldsTyped(ctx context.Context, id string, fields map[string]any) error {
	if err := checkMachine(ctx); err != nil {
		return err
	}
	return m.fakeIntake.UpdateBeadFieldsTyped(ctx, id, fields)
}

func newInvitesFixture(t *testing.T) (*landInvites, *fakeIntake, *fakeGitHubUsers) {
	t.Helper()
	d := newFakeIntake()
	put := func(b *beadsapi.BeadDetail) {
		d.n++
		b.CreatedAt = d.now.Add(time.Duration(d.n) * time.Second)
		d.beads[b.ID] = b
		d.order = append(d.order, b.ID)
	}
	put(&beadsapi.BeadDetail{ID: "gd-acme", Type: "project", Title: "Project: acme", Status: "open", Fields: map[string]string{
		"land_kind": "tenant", "land_opened_at": "2026-10-20T18:00:00Z", "github_installation_id": "9001",
		"git_url": "https://github.com/octo/acme-site.git",
	}})
	put(&beadsapi.BeadDetail{ID: "gd-sprout", Type: "project", Title: "Project: sprout", Status: "open", Fields: map[string]string{"land_kind": "tenant"}})
	gh := &fakeGitHubUsers{users: map[string]githubapp.GitHubUser{
		"hubot":    {ID: 4242, Login: "Hubot", Type: "User"},
		"octo-org": {ID: 77, Login: "octo-org", Type: "Organization"},
	}}
	l := &landInvites{
		daemon: machineCheckingIntake{d}, machine: staticTokenSource("machine-token"), projects: d, github: gh,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return ghNow },
	}
	return l, d, gh
}

// ownerRequest is the owner's request as the Commons hands it over: their
// bearer and email on the context.
func ownerRequest() *http.Request {
	r := httptest.NewRequest("POST", "/garden/c/acme/general/invite", nil)
	ctx := beadsapi.WithUserBearer(r.Context(), "owner-access-token")
	return r.WithContext(oidcauth.ContextWithEmail(ctx, "octo@example.com"))
}

func TestLandInvites_InviteFilesTheResolvedID(t *testing.T) {
	l, d, gh := newInvitesFixture(t)
	inv, err := l.invite(ownerRequest(), "acme", "hubot", "guest")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Login != "Hubot" || inv.Role != "guest" || inv.State != "pending" || !inv.ExpiresAt.Equal(ghNow.Add(14*24*time.Hour)) {
		t.Fatalf("invite = %+v", inv)
	}
	if len(gh.asked) != 1 || !strings.HasPrefix(gh.asked[0], "octo/acme-site hubot via ") {
		t.Fatalf("GitHub was asked %v", gh.asked)
	}
	b := d.beads[inv.ID]
	if b == nil || b.Type != "land_invite" || len(b.Labels) != 1 || b.Labels[0] != "project:acme" {
		t.Fatalf("bead = %+v", b)
	}
	want := map[string]string{
		"land": "acme", "github_id": "4242", "github_login": "Hubot", "role": "guest",
		"invited_by": "oidc:octo@example.com", "state": "pending", "expires_at": "2026-11-02T18:00:00Z",
	}
	for k, v := range want {
		if b.Fields[k] != v {
			t.Errorf("%s = %q, want %q", k, b.Fields[k], v)
		}
	}

	// The same person again is refused, in words for the owner.
	_, err = l.invite(ownerRequest(), "acme", "HUBOT", "member")
	var ref *commons.InviteRefusal
	if !errors.As(err, &ref) || ref.Message != "@Hubot is already invited." {
		t.Fatalf("second invite: %v", err)
	}
	list, err := l.list(context.Background(), "acme")
	if err != nil || len(list) != 1 || list[0].ID != inv.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
}

func TestLandInvites_Refusals(t *testing.T) {
	l, d, _ := newInvitesFixture(t)
	for _, c := range []struct{ land, login, want string }{
		{"acme", "nobody", "GitHub has no account called @nobody."},
		{"acme", "octo-org", "@octo-org is not a person's account on GitHub."},
		{"sprout", "hubot", "sprout is not open yet"},
		{"elsewhere", "hubot", "elsewhere is not open yet"},
	} {
		_, err := l.invite(ownerRequest(), c.land, c.login, "member")
		var ref *commons.InviteRefusal
		if !errors.As(err, &ref) || !strings.Contains(ref.Message, c.want) {
			t.Errorf("%s/%s: %v, want a refusal containing %q", c.land, c.login, err, c.want)
		}
	}
	// No identity, no invite.
	anon := httptest.NewRequest("POST", "/x", nil)
	if _, err := l.invite(anon, "acme", "hubot", "member"); err == nil {
		t.Error("an invite with no signed-in owner was filed")
	}
	if d.creates != 0 {
		t.Fatalf("a refused invite filed %d beads", d.creates)
	}
}

func TestLandInvites_Revoke(t *testing.T) {
	l, d, _ := newInvitesFixture(t)
	inv, err := l.invite(ownerRequest(), "acme", "hubot", "member")
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.revoke(ownerRequest(), "acme", inv.ID)
	if err != nil || !got.Revoking {
		t.Fatalf("revoke = %+v %v", got, err)
	}
	f := d.beads[inv.ID].Fields
	if f["revoke_requested_by"] != "oidc:octo@example.com" || f["revoke_requested_at"] == "" || f["state"] != "pending" {
		t.Fatalf("fields = %v", f)
	}
	// Revoking again changes nothing; another land's revoke, a closed invite
	// and a bead that is no invite are refused.
	if _, err := l.revoke(ownerRequest(), "acme", inv.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	d.beads["gd-msg1"] = &beadsapi.BeadDetail{ID: "gd-msg1", Type: "message", Labels: []string{"project:acme"}, Fields: map[string]string{"land": "acme"}}
	for land, id := range map[string]string{"sprout": inv.ID, "acme": "gd-msg1"} {
		_, err := l.revoke(ownerRequest(), land, id)
		var ref *commons.InviteRefusal
		if !errors.As(err, &ref) {
			t.Errorf("revoke %s in %s: %v, want a refusal", id, land, err)
		}
	}
	if _, err := l.revoke(ownerRequest(), "acme", "gd-none"); err == nil {
		t.Error("revoking a missing invite succeeded")
	}
	// A revoked invite is still listed, as revoking, until the provisioner
	// ends it.
	list, _ := l.list(context.Background(), "acme")
	if len(list) != 1 || !list[0].Revoking {
		t.Fatalf("list = %+v", list)
	}
}

func TestLandInvites_ListKeepsToTheLand(t *testing.T) {
	l, d, _ := newInvitesFixture(t)
	d.beads["gd-x1"] = &beadsapi.BeadDetail{ID: "gd-x1", Type: "land_invite", Status: "open",
		Labels: []string{"project:acme", "project:beta"}, Fields: map[string]string{"land": "acme", "state": "pending"}}
	d.beads["gd-x2"] = &beadsapi.BeadDetail{ID: "gd-x2", Type: "land_invite", Status: "open",
		Labels: []string{"project:acme"}, Fields: map[string]string{"land": "beta", "state": "pending"}}
	d.beads["gd-x3"] = &beadsapi.BeadDetail{ID: "gd-x3", Type: "land_invite", Status: "open",
		Labels: []string{"project:acme"}, Fields: map[string]string{"land": "acme", "state": "expired"}}
	d.order = append(d.order, "gd-x1", "gd-x2", "gd-x3")
	list, err := l.list(context.Background(), "acme")
	if err != nil || len(list) != 0 {
		t.Fatalf("list = %+v %v", list, err)
	}
}
