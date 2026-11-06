package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Lands step 8.9: member invites (land_invite.go).

func landInviteBody(fields map[string]any, labels ...string) map[string]any {
	if labels == nil {
		labels = []string{"project:tomato-patch"}
	}
	return map[string]any{
		"title":  "Invite octocat to tomato-patch",
		"type":   string(typeLandInvite),
		"labels": labels,
		"fields": fields,
	}
}

func validLandInviteFields() map[string]any {
	return map[string]any{
		"land":         "tomato-patch",
		"github_id":    "583231",
		"github_login": "octocat",
		"role":         "member",
		"invited_by":   "oidc:owner@example.com",
		"expires_at":   "2026-11-04T12:00:00Z",
		"state":        "pending",
	}
}

func inviteWith(k string, v any) map[string]any {
	f := validLandInviteFields()
	if v == nil {
		delete(f, k)
	} else {
		f[k] = v
	}
	return f
}

func TestLandInvite_TypeRegistered(t *testing.T) {
	s, _, _ := newTestServer()
	if !strings.Contains(strings.Join(BuiltinBeadTypeNames(), ","), string(typeLandInvite)) {
		t.Fatal("land_invite is not a builtin type")
	}
	tc, err := s.resolveTypeConfig(context.Background(), typeLandInvite)
	if err != nil || tc == nil {
		t.Fatalf("resolveTypeConfig: %v %v", tc, err)
	}
	if tc.Kind != model.KindData || tc.WritePolicy == nil || tc.WritePolicy.MinRole != RoleAdmin {
		t.Errorf("land_invite must be data-kind and admin-gated: %+v", tc)
	}
	// No land tier writes one: an owner's invite goes through the viewer.
	for _, act := range []string{"create", "update", "close", "comment"} {
		if anyTierAllows(typeLandInvite, act) {
			t.Errorf("actionTable allows %s on land_invite", act)
		}
	}
}

func TestLandInvite_CreateValid(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	for _, role := range []string{"member", "guest"} {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", landInviteBody(inviteWith("role", role)))
		requireStatus(t, rec, http.StatusCreated)
		var b model.Bead
		decodeJSON(t, rec, &b)
		if got := ms.labels[b.ID]; len(got) != 1 || got[0] != "project:tomato-patch" {
			t.Fatalf("labels = %v", got)
		}
	}
}

func TestLandInvite_CreateRejected(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	createLand(t, h, ms, "gardenio-own", "")
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no project label", landInviteBody(validLandInviteFields(), "lands"), "needs the label project:tomato-patch"},
		{"wrong project label", landInviteBody(validLandInviteFields(), "project:other"), "exactly one project label"},
		{"two project labels", landInviteBody(validLandInviteFields(), "project:tomato-patch", "project:gardenio-own"), "exactly one project label"},
		{"owner role", landInviteBody(inviteWith("role", "owner")), "role"},
		{"no role", landInviteBody(inviteWith("role", nil)), "role"},
		{"login for id", landInviteBody(inviteWith("github_id", "octocat")), "github_id"},
		{"zero id", landInviteBody(inviteWith("github_id", "0")), "github_id"},
		{"padded id", landInviteBody(inviteWith("github_id", "0583231")), "github_id"},
		{"bad login", landInviteBody(inviteWith("github_login", "-octo")), "github_login"},
		{"no expiry", landInviteBody(inviteWith("expires_at", nil)), "expires_at"},
		{"born joined", landInviteBody(inviteWith("state", "joined")), "created in state"},
		{"born with a user", landInviteBody(inviteWith("joined_user_id", "u-1")), "cannot carry joined_user_id"},
		{"unknown land", landInviteBody(inviteWith("land", "nowhere"), "project:nowhere"), "not a project"},
		{"operator land", landInviteBody(inviteWith("land", "gardenio-own"), "project:gardenio-own"), "not a tenant land"},
	}
	for _, tc := range cases {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: code %d body %s, want 400 containing %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestLandInvite_UpdateRules(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	newInvite := func() string {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", landInviteBody(validLandInviteFields()))
		requireStatus(t, rec, http.StatusCreated)
		var b model.Bead
		decodeJSON(t, rec, &b)
		return b.ID
	}
	patch := func(id string, fields map[string]any) int {
		return doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": fields}).Code
	}

	id := newInvite()
	// Who is invited, to what and until when never change.
	for k, v := range map[string]any{
		"github_id": "1", "github_login": "someone", "role": "guest",
		"invited_by": "oidc:x@example.com", "expires_at": "2027-01-01T00:00:00Z", "land": "other",
	} {
		if code := patch(id, map[string]any{k: v}); code != http.StatusBadRequest {
			t.Errorf("changing %s: code %d, want 400", k, code)
		}
	}
	// Re-sending a fixed value is not a change.
	if code := patch(id, map[string]any{"role": "member"}); code != http.StatusOK {
		t.Errorf("same role: code %d", code)
	}
	// A revoke request is not a state change.
	if code := patch(id, map[string]any{"revoke_requested_at": "2026-10-22T00:00:00Z", "revoke_requested_by": "oidc:owner@example.com"}); code != http.StatusOK {
		t.Errorf("revoke request: code %d", code)
	}
	// pending → joined → revoked; revoked is terminal.
	if code := patch(id, map[string]any{"state": "joined", "joined_user_id": "u-1"}); code != http.StatusOK {
		t.Fatalf("join: code %d", code)
	}
	if code := patch(id, map[string]any{"state": "expired"}); code != http.StatusBadRequest {
		t.Errorf("joined → expired: code %d, want 400", code)
	}
	if code := patch(id, map[string]any{"state": "revoked"}); code != http.StatusOK {
		t.Fatalf("revoke: code %d", code)
	}
	if code := patch(id, map[string]any{"state": "pending"}); code != http.StatusBadRequest {
		t.Errorf("revoked → pending: code %d, want 400", code)
	}

	// pending → expired is terminal too.
	id = newInvite()
	if code := patch(id, map[string]any{"state": "expired"}); code != http.StatusOK {
		t.Fatalf("expire: code %d", code)
	}
	if code := patch(id, map[string]any{"state": "joined"}); code != http.StatusBadRequest {
		t.Errorf("expired → joined: code %d, want 400", code)
	}

	// The land label is fixed.
	id = newInvite()
	rec := doJSON(t, h, http.MethodDelete, "/v1/beads/"+id+"/labels/project:tomato-patch", nil)
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads/"+id+"/labels", map[string]any{"label": "project:gardenio-own"})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"labels": []string{"project:other"}})
	requireStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPost, "/v1/beads/"+id+"/labels", map[string]any{"label": "seen"})
	requireStatus(t, rec, http.StatusCreated)
}

func TestLandInvite_MachineGrants(t *testing.T) {
	gate := machineCapabilityGrants["gardenio-machine-lands-gate"]
	prov := machineCapabilityGrants["gardenio-machine-lands-provisioner"]
	has := func(c machineCapability, action string) bool {
		for _, w := range c.Writes {
			if w.Action == action && w.BeadType == typeLandInvite {
				return true
			}
		}
		return false
	}
	if !has(gate, "create") || !has(gate, "update") || has(gate, "close") {
		t.Errorf("the Gate files and revokes invites and closes none: %+v", gate.Writes)
	}
	if has(prov, "create") || !has(prov, "update") || !has(prov, "close") {
		t.Errorf("the provisioner moves and closes invites and files none: %+v", prov.Writes)
	}
	owns := false
	for _, typ := range prov.Ownership {
		owns = owns || typ == typeLandInvite
	}
	if !owns {
		t.Error("the provisioner updates invites the Gate created, so it needs typed ownership of land_invite")
	}
}

// One open invite per account, and open_invite_cap, are the daemon's
// (gd-GPvddfQY6y): two viewer replicas filing at once must not give one
// account two passes or the demo land more than its cap.
func TestLandInvite_OneOpenPerAccountAndCap(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	create := func(fields map[string]any) (int, string) {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", landInviteBody(fields))
		var b model.Bead
		if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
			decodeJSON(t, rec, &b)
		}
		return rec.Code, b.ID
	}
	invite := func(id, login string, limit any) map[string]any {
		f := inviteWith("github_id", id)
		f["github_login"] = login
		if limit != nil {
			f[landInviteCapField] = limit
		}
		return f
	}
	invites := func() int {
		n := 0
		for _, b := range ms.beads {
			if b.Type == typeLandInvite {
				n++
			}
		}
		return n
	}

	code, first := create(invite("583231", "octocat", nil))
	if code != http.StatusCreated {
		t.Fatalf("first invite: code %d", code)
	}
	// A second create for the account, as another replica would, is the first.
	if code, again := create(invite("583231", "octocat", 1)); again != first || invites() != 1 {
		t.Fatalf("same account: code %d id %q, want %q and one invite (have %d)", code, again, first, invites())
	}
	// At the cap a create for another account is refused.
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", landInviteBody(invite("1001", "hubot", 1)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), landInviteCapField) {
		t.Fatalf("at the cap: code %d body %s, want 400 naming %s", rec.Code, rec.Body.String(), landInviteCapField)
	}
	if code, _ := create(invite("1001", "hubot", 2)); code != http.StatusCreated || invites() != 2 {
		t.Fatalf("under the cap: code %d, %d invites", code, invites())
	}
	for _, bad := range []any{0, "2", 1.5} {
		if code, _ := create(invite("1002", "monalisa", bad)); code != http.StatusBadRequest {
			t.Errorf("cap %v: code %d, want 400", bad, code)
		}
	}
	// An invite being revoked neither holds the account nor counts.
	patch := doJSON(t, h, http.MethodPatch, "/v1/beads/"+first, map[string]any{"fields": map[string]any{
		"revoke_requested_at": "2026-10-22T00:00:00Z", "revoke_requested_by": "oidc:owner@example.com",
	}})
	requireStatus(t, patch, http.StatusOK)
	code, second := create(invite("583231", "octocat", 2))
	if code != http.StatusCreated || second == first {
		t.Fatalf("after a revoke request: code %d id %q, want a new invite", code, second)
	}
	// The cap is fixed once filed.
	patch = doJSON(t, h, http.MethodPatch, "/v1/beads/"+second, map[string]any{"fields": map[string]any{landInviteCapField: 9}})
	requireStatus(t, patch, http.StatusBadRequest)
}
