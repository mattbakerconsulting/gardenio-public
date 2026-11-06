package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// seedUser seeds a type:user bead for email whose allowed_projects is projects
// (a JSON list, or "*" for a wildcard grant).
func (f *scopedFixture) seedUser(t *testing.T, id, email string, projects any, status model.Status) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"email": email, "allowed_projects": projects})
	if err != nil {
		t.Fatal(err)
	}
	name, _, _ := strings.Cut(email, "@")
	f.ms.beads[id] = &model.Bead{ID: id, Title: name, Type: "user", Kind: "config", Status: status, Fleet: "default", Fields: raw}
}

// seedPeople seeds ann and cy in claimlayer, bob in other, a wildcard user and
// a closed claimlayer user.
func (f *scopedFixture) seedPeople(t *testing.T) {
	t.Helper()
	f.seedUser(t, "u-ann", "ann@example.com", []string{"claimlayer"}, model.StatusOpen)
	f.seedUser(t, "u-cy", "cy@example.com", []string{"other", "claimlayer"}, model.StatusOpen)
	f.seedUser(t, "u-bob", "bob@example.com", []string{"other"}, model.StatusOpen)
	f.seedUser(t, "u-star", "star@example.com", "*", model.StatusOpen)
	f.seedUser(t, "u-gone", "gone@example.com", []string{"claimlayer"}, model.StatusClosed)
}

func (f *scopedFixture) members(t *testing.T, token, land string) (int, map[string]string) {
	t.Helper()
	code, body := f.do(t, token, http.MethodGet, "/v1/lands/"+land+"/members", nil)
	if code != http.StatusOK {
		return code, nil
	}
	var resp LandMembersResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("members of %s: %v (%s)", land, err, body)
	}
	out := map[string]string{}
	for _, m := range resp.Members {
		out[m.Principal] = m.Name
	}
	return code, out
}

func TestLandMembers_OnlyTheLandsPeople(t *testing.T) {
	f := newScopedFixture(t)
	f.seedPeople(t)
	for _, tok := range []string{tokScopedWriter, tokScopedGuest, tokScopedOwner, tokOwnerAdmin} {
		code, got := f.members(t, tok, "claimlayer")
		if code != http.StatusOK {
			t.Fatalf("%s: members of claimlayer: %d", tok, code)
		}
		if got["oidc:ann@example.com"] != "ann" || got["oidc:cy@example.com"] != "cy" {
			t.Errorf("%s: members of claimlayer = %v, want ann and cy", tok, got)
		}
		for _, p := range []string{"oidc:bob@example.com", "oidc:star@example.com", "oidc:gone@example.com"} {
			if _, ok := got[p]; ok {
				t.Errorf("%s: members of claimlayer lists %s", tok, p)
			}
		}
	}
	// Cross-land negative: a claimlayer member cannot list another land's
	// people, and the answer is the same as for a land that does not exist.
	for _, land := range []string{"other", "gardenio", "nosuch"} {
		if code, got := f.members(t, tokScopedWriter, land); code != http.StatusNotFound {
			t.Errorf("claimlayer member lists %s: %d %v, want 404", land, code, got)
		}
	}
	// The land principal lists only its own land.
	if code, _ := f.members(t, tokLandClaimlayer, "claimlayer"); code != http.StatusOK {
		t.Errorf("land principal, own land: %d", code)
	}
	if code, _ := f.members(t, tokLandClaimlayer, "other"); code != http.StatusNotFound {
		t.Errorf("land principal, other land: %d, want 404", code)
	}
	// The admin sees any land's.
	if code, got := f.members(t, tokOwnerAdmin, "other"); code != http.StatusOK || got["oidc:bob@example.com"] != "bob" {
		t.Errorf("admin, other land: %d %v", code, got)
	}
}

// sayAs writes a commons message into channel chID of land as the person with
// email, on token, and returns its id.
func (f *scopedFixture) sayAs(t *testing.T, token, land, chID, email string) string {
	t.Helper()
	code, body := f.do(t, token, http.MethodPost, "/v1/beads", map[string]any{
		"title": "hi @ann", "type": "message", "status": "closed",
		"labels": []string{"project:" + land},
		"fields": map[string]any{"text": "hi @ann", "channel_id": chID, "author": "oidc:" + email, "origin": "commons"},
	})
	if code != http.StatusCreated {
		t.Fatalf("say as %s: %d %s", email, code, body)
	}
	var b struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &b)
	return b.ID
}

func (f *scopedFixture) notify(t *testing.T, token, land, to, msgID string) (int, string) {
	t.Helper()
	return f.do(t, token, http.MethodPost, "/v1/beads", map[string]any{
		"title": "mention", "type": "notification",
		"labels": []string{"project:" + land},
		"fields": map[string]any{"to": to, "reason": "mention", "message_id": msgID},
	})
}

func TestNotification_MentionInOwnLandOnly(t *testing.T) {
	f := newScopedFixture(t)
	f.seedPeople(t)
	f.seedChannel(t, "ch-cl", "claimlayer", map[string]any{"name": "general", "channel_kind": "general"})
	f.seedChannel(t, "ch-other", "other", map[string]any{"name": "general", "channel_kind": "general"})

	mine := f.sayAs(t, tokScopedWriter, "claimlayer", "ch-cl", "clw@example.com")

	// Same-land control: a member notifies another member of its land about
	// its own message, and so does a guest.
	if code, body := f.notify(t, tokScopedWriter, "claimlayer", "oidc:ann@example.com", mine); code != http.StatusCreated {
		t.Errorf("member notifies a member of its land: %d %s", code, body)
	}
	if code, body := f.notify(t, tokScopedWriter, "claimlayer", "oidc:cy@example.com", mine); code != http.StatusCreated {
		t.Errorf("member notifies a member of two lands: %d %s", code, body)
	}
	guests := f.sayAs(t, tokScopedGuest, "claimlayer", "ch-cl", "clg@example.com")
	if code, body := f.notify(t, tokScopedGuest, "claimlayer", "oidc:ann@example.com", guests); code != http.StatusCreated {
		t.Errorf("guest notifies a member of its land: %d %s", code, body)
	}

	// Cross-land: a person of another land, and the same answer for a
	// stranger, a wildcard user and a closed user bead.
	for _, to := range []string{"oidc:bob@example.com", "oidc:nobody@example.com", "oidc:star@example.com", "oidc:gone@example.com"} {
		code, body := f.notify(t, tokScopedWriter, "claimlayer", to, mine)
		if code != http.StatusBadRequest || !strings.Contains(body, errNotificationTarget.Error()) {
			t.Errorf("notify %s from claimlayer: %d %s, want 400 %q", to, code, body, errNotificationTarget)
		}
	}
	// Cross-land: a notification in a land that is not the caller's.
	if code, body := f.notify(t, tokScopedWriter, "other", "oidc:bob@example.com", mine); code != http.StatusForbidden {
		t.Errorf("claimlayer member notifies in other: %d %s, want 403", code, body)
	}
	// A message of another land cannot be the subject, even for the admin.
	theirs := f.sayAs(t, tokOwnerAdmin, "other", "ch-other", "owner@example.com")
	if code, body := f.notify(t, tokOwnerAdmin, "claimlayer", "oidc:ann@example.com", theirs); code != http.StatusBadRequest {
		t.Errorf("notification in claimlayer about a message of other: %d %s, want 400", code, body)
	}
	// Someone else's words: a person notifies only about its own message.
	owners := f.sayAs(t, tokScopedOwner, "claimlayer", "ch-cl", "clo@example.com")
	if code, body := f.notify(t, tokScopedWriter, "claimlayer", "oidc:ann@example.com", owners); code != http.StatusBadRequest {
		t.Errorf("member notifies about the owner's message: %d %s, want 400", code, body)
	}
	// Shape: an agent is not a person, and a message is required.
	if code, _ := f.notify(t, tokScopedWriter, "claimlayer", "agent:x", mine); code != http.StatusBadRequest {
		t.Errorf("notification to an agent: %d, want 400", code)
	}
	if code, _ := f.notify(t, tokScopedWriter, "claimlayer", "oidc:ann@example.com", ""); code != http.StatusBadRequest {
		t.Errorf("notification with no message: %d, want 400", code)
	}
	// Create-only for people: a member cannot edit one.
	for id, b := range f.ms.beads {
		if b.Type != typeNotification {
			continue
		}
		if code, _ := f.do(t, tokScopedWriter, http.MethodPatch, "/v1/beads/"+id, map[string]any{"description": "d"}); code != http.StatusForbidden {
			t.Errorf("member updates notification %s: %d, want 403", id, code)
		}
		// And it never moves to another land, even for the admin.
		code, body := f.do(t, tokOwnerAdmin, http.MethodPatch, "/v1/beads/"+id, map[string]any{"labels": []string{"project:other"}})
		if code != http.StatusBadRequest {
			t.Errorf("admin moves notification %s to other: %d %s, want 400", id, code, body)
		}
		break
	}
}
