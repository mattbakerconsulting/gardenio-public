package commons

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/commons/commonstest"
)

// invitesFake is LandInvites over a slice. claimlayer is a tenant land (the
// switchFake's rule); bo is its owner when the request says X-Test-Owner.
type invitesFake struct {
	invites []Invite
	calls   []string
}

func (f *invitesFake) wire(h *Handler) {
	(&switchFake{frozen: map[string]bool{}}).wire(h)
	gate := testGate
	gate.MayAct = func(r *http.Request, land, action string) bool {
		if action == ActionMemberManage {
			return r.Header.Get("X-Test-As") != "claimlayer" || r.Header.Get("X-Test-Owner") != ""
		}
		return testGate.MayAct(r, land, action)
	}
	h.Gate = gate
	h.Invites = LandInvites{
		List: func(_ context.Context, land string) ([]Invite, error) {
			f.calls = append(f.calls, "list:"+land)
			return f.invites, nil
		},
		Invite: func(r *http.Request, land, login, role string) (Invite, error) {
			f.calls = append(f.calls, "invite:"+land+":"+login+":"+role)
			if login == "ghost" {
				return Invite{}, &InviteRefusal{Message: "GitHub has no account called @ghost."}
			}
			inv := Invite{ID: "cl-inv9", Login: login, Role: role, State: "pending", ExpiresAt: t0.Add(14 * 24 * time.Hour)}
			f.invites = append([]Invite{inv}, f.invites...)
			return inv, nil
		},
		Revoke: func(r *http.Request, land, id string) (Invite, error) {
			f.calls = append(f.calls, "revoke:"+land+":"+id)
			for i := range f.invites {
				if f.invites[i].ID == id {
					f.invites[i].Revoking = true
					return f.invites[i], nil
				}
			}
			return Invite{}, &InviteRefusal{Message: "That invite is no longer open."}
		},
	}
}

func invitesServer(t *testing.T) (http.Handler, *invitesFake) {
	t.Helper()
	f := &invitesFake{invites: []Invite{
		{ID: "cl-inv2", Login: "hubot", Role: "guest", State: "pending", ExpiresAt: t0.Add(10 * 24 * time.Hour)},
		{ID: "cl-inv1", Login: "monalisa", Role: "member", State: "joined", ExpiresAt: t0.Add(-24 * time.Hour)},
	}}
	d := commonstest.TwoLands(t0)
	d.Members["claimlayer"][1].Role = "owner" // bo
	return commonsServer(t, d, func(h *Handler) { f.wire(h); h.People = d }), f
}

func asOwner(method, path string, form url.Values) *http.Request {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("X-Test-As", "claimlayer")
	r.Header.Set("X-Test-Owner", "1")
	return r
}

func serve(h http.Handler, r *http.Request) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// The People lightbox answers a tenant land's owner and the operator, and
// nobody on an operator land or without invites wired.
func TestPeopleAreTheOwners(t *testing.T) {
	h, _ := invitesServer(t)
	const members = "/garden/c/claimlayer/general/members"
	if code, body := serve(h, asOwner("GET", members, nil)); code != http.StatusOK || !strings.Contains(body, "People in claimlayer") {
		t.Errorf("owner GET members = %d:\n%s", code, body)
	}
	if code, _ := get(t, h, members, ""); code != http.StatusOK {
		t.Errorf("operator GET members = %d, want 200", code)
	}
	if code, _ := get(t, h, "/garden/c/gardenio/general/members", ""); code != http.StatusForbidden {
		t.Errorf("operator land GET members = %d, want 403", code)
	}
	plain := commonsServer(t, commonstest.TwoLands(t0), (&switchFake{frozen: map[string]bool{}}).wire)
	if code, _ := get(t, plain, members, ""); code != http.StatusForbidden {
		t.Errorf("no invites GET members = %d, want 403", code)
	}
}

func TestPeopleRoutesRefuseAMember(t *testing.T) {
	h, f := invitesServer(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/garden/c/claimlayer/general/members"},
		{"POST", "/garden/c/claimlayer/general/invite"},
		{"POST", "/garden/c/claimlayer/general/revoke"},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader("login=hubot&role=member&id=cl-inv2"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Test-As", "claimlayer")
		if code, _ := serve(h, r); code != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403", c.method, c.path, code)
		}
	}
	// An operator land takes no invites, even from the operator.
	if code, _ := get(t, h, "/garden/c/gardenio/general/members", ""); code != http.StatusForbidden {
		t.Errorf("operator land members = %d, want 403", code)
	}
	// Another site's form is refused before anything is read.
	r := asOwner("POST", "/garden/c/claimlayer/general/invite", url.Values{"login": {"hubot"}, "role": {"member"}})
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if code, _ := serve(h, r); code != http.StatusForbidden {
		t.Errorf("cross-site invite = %d, want 403", code)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "list:") {
			t.Errorf("a refused request reached the invites: %v", f.calls)
		}
	}
}

func TestInviteRoute(t *testing.T) {
	h, f := invitesServer(t)
	code, body := serve(h, asOwner("POST", "/garden/c/claimlayer/general/invite", url.Values{"login": {"@Octocat "}, "role": {"guest"}}))
	if code != http.StatusOK {
		t.Fatalf("invite = %d %s", code, body)
	}
	mustContain(t, body, `role="status">Invited @Octocat as a guest.`, `data-invite="cl-inv9"`)
	if f.calls[0] != "invite:claimlayer:Octocat:guest" {
		t.Fatalf("calls = %v", f.calls)
	}

	// What the owner must fix comes back in the lightbox, with the form kept.
	for _, c := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"login": {"ghost"}, "role": {"member"}}, "GitHub has no account called @ghost."},
		{url.Values{"login": {"not a login"}, "role": {"member"}}, "That is not a GitHub login"},
		{url.Values{"login": {"hubot"}, "role": {"owner"}}, "Pick member or guest."},
	} {
		code, body := serve(h, asOwner("POST", "/garden/c/claimlayer/general/invite", c.form))
		if code != http.StatusOK {
			t.Fatalf("%v = %d", c.form, code)
		}
		mustContain(t, body, `role="alert">`+c.want, `aria-invalid="true"`)
	}
	if n := len(f.invites); n != 3 {
		t.Fatalf("a refused invite was filed: %d invites", n)
	}
}

func TestRevokeRoute(t *testing.T) {
	h, f := invitesServer(t)
	code, body := serve(h, asOwner("POST", "/garden/c/claimlayer/general/revoke", url.Values{"id": {"cl-inv1"}}))
	if code != http.StatusOK {
		t.Fatalf("revoke = %d %s", code, body)
	}
	mustContain(t, body, "Revoking @monalisa. They lose claimlayer within five minutes.", "member · revoking")
	mustNotContain(t, body, `aria-label="Revoke @monalisa"`)
	_, body = serve(h, asOwner("POST", "/garden/c/claimlayer/general/revoke", url.Values{"id": {"cl-inv2"}}))
	mustContain(t, body, "Revoking @hubot. The invite will not let them in.")
	_, body = serve(h, asOwner("POST", "/garden/c/claimlayer/general/revoke", url.Values{"id": {"cl-gone"}}))
	mustContain(t, body, `role="alert">That invite is no longer open.`)
	if f.calls[len(f.calls)-2] != "revoke:claimlayer:cl-gone" {
		t.Fatalf("calls = %v", f.calls)
	}
}

// TestPeopleListsMembersWithBecome: the lightbox lists the land's members
// (People.LandMembers), each row with a Become button that is hidden until
// web/become.js shows it to an admin (plan gd-0SUIdKXWV3 step 3); the server
// never offers it unhidden, nor on the viewer's own row (bo, the owner here).
func TestPeopleListsMembersWithBecome(t *testing.T) {
	h, _ := invitesServer(t)
	code, body := serve(h, asOwner("GET", "/garden/c/claimlayer/general/members", nil))
	if code != http.StatusOK {
		t.Fatalf("members = %d %s", code, body)
	}
	for _, want := range []string{
		`<button class="btn small ghost cm-become" type="button" data-become="oidc:ann@claimlayer.example" data-become-name="ann" aria-label="Become ann" hidden>Become</button>`,
		`<span class="cm-invite-who">Bo Lind</span><span class="cm-invite-what">owner</span>`,
		`data-become="oidc:cy@both.example"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the lightbox lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "oidc:dee@gardenio.example") {
		t.Error("the lightbox lists another land's member")
	}
	if strings.Contains(body, `data-become="oidc:bo@claimlayer.example"`) {
		t.Error("the viewer's own row offers Become")
	}
	if n := strings.Count(body, "data-become="); n != 2 {
		t.Errorf("%d Become buttons, want one per member but the viewer (2)", n)
	}
	if strings.Count(body, " hidden>Become</button>") != 2 {
		t.Error("a Become button is not hidden")
	}
}

// TestPeopleGolden pins the People lightbox markup (an owner's view of two
// invites). A change you meant is `go test ./internal/commons -run
// TestPeopleGolden -update`.
func TestPeopleGolden(t *testing.T) {
	h, _ := invitesServer(t)
	code, body := serve(h, asOwner("GET", "/garden/c/claimlayer/general/members", nil))
	if code != http.StatusOK {
		t.Fatalf("members = %d %s", code, body)
	}
	file := filepath.Join("testdata", "people.golden.html")
	if *update {
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if body != string(want) {
		t.Errorf("people lightbox drifted from %s (run with -update if meant):\n%s", file, body)
	}
}
