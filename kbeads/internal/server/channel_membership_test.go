package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Channel membership (Lands Commons spec §7.3 step 4.8, gd-Yxtnphljt4): a user
// channel or a DM admits only its members as posters, and a person joins or
// leaves a user channel with a members-only patch.

// Principals of the fixture's claimlayer tokens (scoped_test_server_test.go).
const (
	principalWriter = "oidc:clw@example.com" // tokScopedWriter, member
	principalOwner  = "oidc:clo@example.com" // tokScopedOwner
	principalGuest  = "oidc:clg@example.com" // tokScopedGuest
)

// seedChannel seeds an open channel bead in project with the given fields.
func (f *scopedFixture) seedChannel(t *testing.T, id, project string, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	f.seedTyped(id, typeChannel, "project:"+project)
	f.ms.beads[id].Kind = "data"
	f.ms.beads[id].Fields = raw
	f.ms.beads[id].CreatedBy = "oidc:op@example.com"
}

func (f *scopedFixture) channelMembers(t *testing.T, id string) []string {
	t.Helper()
	return readChannelFields(f.ms.beads[id].Fields).Members
}

// say posts a closed message into channel chID of claimlayer as token.
func (f *scopedFixture) say(t *testing.T, token, chID string) (int, string) {
	t.Helper()
	return f.do(t, token, http.MethodPost, "/v1/beads", map[string]any{
		"title": "hi", "type": "message", "status": "closed",
		"labels": []string{"project:claimlayer"},
		"fields": map[string]any{"text": "hi", "channel_id": chID},
	})
}

func (f *scopedFixture) setMembers(t *testing.T, token, chID string, members []string) (int, string) {
	t.Helper()
	return f.do(t, token, http.MethodPatch, "/v1/beads/"+chID, map[string]any{"fields": map[string]any{"members": members}})
}

func TestChannelMembership_PostIsMembersOnly(t *testing.T) {
	f := newScopedFixture(t)
	f.seedChannel(t, "ch-general", "claimlayer", map[string]any{"name": "general", "channel_kind": "general"})
	f.seedChannel(t, "ch-design", "claimlayer", map[string]any{"name": "design", "channel_kind": "user", "members": []string{principalOwner}})
	f.seedChannel(t, "ch-empty", "claimlayer", map[string]any{"name": "empty", "channel_kind": "user"})
	f.seedChannel(t, "ch-dm", "claimlayer", map[string]any{"name": "dm-clw-x", "channel_kind": "dm", "members": []string{principalWriter, "agent:x"}})

	for _, c := range []struct {
		name, token, ch string
		want            int
		says            string
	}{
		{"member in a default channel", tokScopedWriter, "ch-general", http.StatusCreated, ""},
		{"guest in a default channel", tokScopedGuest, "ch-general", http.StatusCreated, ""},
		{"owner who is a member", tokScopedOwner, "ch-design", http.StatusCreated, ""},
		{"member who is not a member", tokScopedWriter, "ch-design", http.StatusForbidden, "join the channel"},
		{"guest who is not a member", tokScopedGuest, "ch-design", http.StatusForbidden, "not a member of #design"},
		// Owning the land is not membership of the channel: an owner joins too.
		{"owner of a channel nobody joined", tokScopedOwner, "ch-empty", http.StatusForbidden, "not a member of #empty"},
		{"dm member", tokScopedWriter, "ch-dm", http.StatusCreated, ""},
		{"dm outsider", tokScopedOwner, "ch-dm", http.StatusForbidden, "only its members"},
		// The operator acts in every land; it is never a scoped principal.
		{"admin", tokOwnerAdmin, "ch-design", http.StatusCreated, ""},
	} {
		code, body := f.say(t, c.token, c.ch)
		if code != c.want || (c.says != "" && !strings.Contains(body, c.says)) {
			t.Errorf("%s: %d %s, want %d naming %q", c.name, code, body, c.want, c.says)
		}
	}

	// A message without channel_id is not land-bound and is not checked.
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads", map[string]any{
		"title": "hi", "type": "message", "status": "closed", "labels": []string{"project:claimlayer"},
	}); code != http.StatusCreated {
		t.Errorf("message with no channel: %d %s, want 201", code, body)
	}
}

func TestChannelMembership_JoinAndLeave(t *testing.T) {
	f := newScopedFixture(t)
	f.seedChannel(t, "ch-design", "claimlayer", map[string]any{"name": "design", "channel_kind": "user", "members": []string{principalOwner}})

	// A member joins, may then post, and leaves.
	if code, body := f.setMembers(t, tokScopedWriter, "ch-design", []string{principalOwner, principalWriter}); code != http.StatusOK {
		t.Fatalf("member joins: %d %s, want 200", code, body)
	}
	if got := string(f.ms.beads["ch-design"].Fields); !strings.Contains(got, `"name":"design"`) {
		t.Errorf("join did not merge into fields: %s", got)
	}
	if got := f.ms.beads["ch-design"].CreatedBy; got != "oidc:op@example.com" {
		t.Errorf("joining re-attributed the channel to %q", got)
	}
	if code, body := f.say(t, tokScopedWriter, "ch-design"); code != http.StatusCreated {
		t.Errorf("member posts after joining: %d %s, want 201", code, body)
	}
	if code, body := f.setMembers(t, tokScopedWriter, "ch-design", []string{principalOwner}); code != http.StatusOK {
		t.Errorf("member leaves: %d %s, want 200", code, body)
	}
	if code, _ := f.say(t, tokScopedWriter, "ch-design"); code != http.StatusForbidden {
		t.Errorf("member posts after leaving: %d, want 403", code)
	}

	// A member changes no one else's entry; a guest may not join at all.
	for _, c := range []struct {
		name, token string
		members     []string
		says        string
	}{
		{"member adds someone else", tokScopedWriter, []string{principalOwner, principalGuest}, "only an owner of land claimlayer may add someone else"},
		{"member removes someone else", tokScopedWriter, []string{}, "only an owner of land claimlayer may remove someone else"},
		{"member joins from a stale read", tokScopedWriter, []string{principalWriter}, "may remove someone else"},
		{"guest joins", tokScopedGuest, []string{principalOwner, principalGuest}, "a guest in land claimlayer may not join a channel"},
		{"bad principal", tokScopedWriter, []string{principalOwner, "bob"}, "may add someone else"},
	} {
		if code, body := f.setMembers(t, c.token, "ch-design", c.members); code != http.StatusForbidden || !strings.Contains(body, c.says) {
			t.Errorf("%s: %d %s, want 403 naming %q", c.name, code, body, c.says)
		}
	}
	if got := f.channelMembers(t, "ch-design"); !slices.Equal(got, []string{principalOwner}) {
		t.Fatalf("denied patches changed members: %v", got)
	}

	// The owner adds and removes anyone, the guest included; the guest leaves.
	if code, body := f.setMembers(t, tokScopedOwner, "ch-design", []string{principalOwner, principalGuest, principalWriter}); code != http.StatusOK {
		t.Fatalf("owner adds two: %d %s, want 200", code, body)
	}
	if code, body := f.say(t, tokScopedGuest, "ch-design"); code != http.StatusCreated {
		t.Errorf("added guest posts: %d %s, want 201", code, body)
	}
	if code, body := f.setMembers(t, tokScopedGuest, "ch-design", []string{principalOwner, principalWriter}); code != http.StatusOK {
		t.Errorf("guest leaves: %d %s, want 200", code, body)
	}
	if code, body := f.setMembers(t, tokScopedOwner, "ch-design", []string{principalOwner}); code != http.StatusOK {
		t.Errorf("owner removes a member: %d %s, want 200", code, body)
	}
	// The members list is still validated after the gate.
	if code, body := f.setMembers(t, tokScopedOwner, "ch-design", []string{principalOwner, "bob"}); code != http.StatusBadRequest || !strings.Contains(body, "must be oidc:") {
		t.Errorf("owner adds a non-principal: %d %s, want 400", code, body)
	}
	if code, body := f.setMembers(t, tokScopedOwner, "ch-design", []string{principalOwner, principalOwner}); code != http.StatusBadRequest || !strings.Contains(body, "twice") {
		t.Errorf("owner lists a principal twice: %d %s, want 400", code, body)
	}
}

// Only a user channel is joinable, only through a members-only patch, and only
// in the caller's own land.
func TestChannelMembership_JoinLimits(t *testing.T) {
	f := newScopedFixture(t)
	f.seedChannel(t, "ch-general", "claimlayer", map[string]any{"name": "general", "channel_kind": "general"})
	f.seedChannel(t, "ch-dm", "claimlayer", map[string]any{"name": "dm-clo-x", "channel_kind": "dm", "members": []string{principalOwner, "agent:x"}})
	f.seedChannel(t, "ch-design", "claimlayer", map[string]any{"name": "design", "channel_kind": "user"})
	f.seedChannel(t, "ch-far", "gardenio", map[string]any{"name": "far", "channel_kind": "user"})

	for _, c := range []struct {
		name, token, ch string
		patch           map[string]any
		says            string
	}{
		{"join a default channel", tokScopedOwner, "ch-general", map[string]any{"fields": map[string]any{"members": []string{principalOwner}}}, "only a user channel"},
		{"join a dm", tokScopedOwner, "ch-dm", map[string]any{"fields": map[string]any{"members": []string{principalOwner, "agent:x", principalWriter}}}, "only a user channel"},
		{"members plus rename", tokScopedOwner, "ch-design", map[string]any{"fields": map[string]any{"members": []string{principalOwner}, "name": "renamed"}}, "may not update beads of type channel"},
		{"members plus topic", tokScopedOwner, "ch-design", map[string]any{"description": "t", "fields": map[string]any{"members": []string{principalOwner}}}, "may not update beads of type channel"},
		// Another land's channel answers as a missing one (gd-7APTAIhdC5).
		{"another land", tokScopedOwner, "ch-far", map[string]any{"fields": map[string]any{"members": []string{principalOwner}}}, "bead not found"},
	} {
		want := http.StatusForbidden
		if c.says == "bead not found" {
			want = http.StatusNotFound
		}
		if code, body := f.do(t, c.token, http.MethodPatch, "/v1/beads/"+c.ch, c.patch); code != want || !strings.Contains(body, c.says) {
			t.Errorf("%s: %d %s, want %d naming %q", c.name, code, body, want, c.says)
		}
	}
	// Archiving or creating a channel stays the operator's.
	if code, body := f.do(t, tokScopedOwner, http.MethodPost, "/v1/beads/ch-design/close", map[string]any{}); code != http.StatusForbidden {
		t.Errorf("owner archives a channel: %d %s, want 403", code, body)
	}
	if code, body := f.do(t, tokScopedOwner, http.MethodPost, "/v1/beads", map[string]any{
		"title": "#x", "type": "channel", "labels": []string{"project:claimlayer"},
		"fields": map[string]any{"name": "x", "channel_kind": "user"},
	}); code != http.StatusForbidden {
		t.Errorf("owner creates a channel: %d %s, want 403", code, body)
	}
	// Positive control: the admin edits any channel's members.
	if code, body := f.setMembers(t, tokOwnerAdmin, "ch-far", []string{"oidc:someone@example.com"}); code != http.StatusOK {
		t.Errorf("admin sets members: %d %s, want 200", code, body)
	}
}

// Members are allowed on user and dm channels only (create-time validation).
func TestChannelCreate_UserChannelMembers(t *testing.T) {
	_, _, h := newTestServer()
	land := []string{"project:gardenio"}
	createdID(t, createChannel(t, h, land, map[string]any{"name": "design", "channel_kind": "user"}))
	createdID(t, createChannel(t, h, land, map[string]any{"name": "ops", "channel_kind": "user", "members": []string{"oidc:a@x.io"}}))
	requireRejected(t, createChannel(t, h, land, map[string]any{"name": "ops2", "channel_kind": "user", "members": []string{"oidc:a@x.io", "oidc:a@x.io"}}), "twice")
	requireRejected(t, createChannel(t, h, land, map[string]any{"name": "ops3", "channel_kind": "user", "members": []string{"bob"}}), `"bob" must be oidc:<email> or agent:<name>`)
	requireRejected(t, createChannel(t, h, land, map[string]any{"name": "general", "channel_kind": "general", "members": []string{"oidc:a@x.io"}}), "open to everyone")
}

func TestChannelMembersOnly(t *testing.T) {
	for kind, want := range map[string]bool{"user": true, "dm": true, "general": false, "crew": false, "decisions": false, "escalations": false, "": false} {
		if got := channelMembersOnly(kind); got != want {
			t.Errorf("channelMembersOnly(%q) = %v, want %v", kind, got, want)
		}
	}
	// The guest table admits join so a guest can leave; the join rule itself
	// refuses a guest's join.
	if !tierAllows(tierGuest, typeChannel, scopedActionJoin) || tierAllows(tierMember, typeChannel, "update") {
		t.Error("channel must be join-only for scoped principals, guests included")
	}
}
