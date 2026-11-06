package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Sign-in invites by email (invite.go, gd-ycE484kLyk).

func signinInviteBody(fields map[string]any) map[string]any {
	return map[string]any{
		"title":  "Invite someone@example.com",
		"type":   string(typeInvite),
		"fields": fields,
	}
}

func validSigninInviteFields() map[string]any {
	return map[string]any{
		"email":      "someone@example.com",
		"invited_by": "oidc:owner@example.com",
		"expires_at": "2026-11-11T12:00:00Z",
		"state":      "pending",
	}
}

func signinInviteWith(kv ...any) map[string]any {
	f := validSigninInviteFields()
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(f, kv[i].(string))
		} else {
			f[kv[i].(string)] = kv[i+1]
		}
	}
	return f
}

func TestInvite_TypeRegistered(t *testing.T) {
	s, _, _ := newTestServer()
	if !slices.Contains(BuiltinBeadTypeNames(), string(typeInvite)) {
		t.Fatal("invite is not a builtin type")
	}
	tc, err := s.resolveTypeConfig(context.Background(), typeInvite)
	if err != nil || tc == nil {
		t.Fatalf("resolveTypeConfig: %v %v", tc, err)
	}
	if tc.Kind != model.KindData || tc.WritePolicy == nil || tc.WritePolicy.MinRole != RoleAdmin {
		t.Errorf("invite must be data-kind and admin-gated: %+v", tc)
	}
	for _, act := range []string{"create", "update", "close", "comment"} {
		if anyTierAllows(typeInvite, act) {
			t.Errorf("actionTable allows %s on invite", act)
		}
	}
}

func TestInvite_CreateValid(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	for name, f := range map[string]map[string]any{
		"no land": validSigninInviteFields(),
		"member":  signinInviteWith("land", "tomato-patch", "role", "member"),
		"guest":   signinInviteWith("land", "tomato-patch", "role", "guest"),
		"plus":    signinInviteWith("email", "first.last+lands@sub.example.org"),
	} {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", signinInviteBody(f))
		if rec.Code != http.StatusCreated {
			t.Errorf("%s: code %d body %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestInvite_CreateRejected(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	createLand(t, h, ms, "gardenio-own", "")
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no email", signinInviteBody(signinInviteWith("email", nil)), "email"},
		{"not an email", signinInviteBody(signinInviteWith("email", "someone")), "email"},
		{"named address", signinInviteBody(signinInviteWith("email", "Some One <someone@example.com>")), "bare email"},
		{"two addresses", signinInviteBody(signinInviteWith("email", "a@example.com, b@example.com")), "email"},
		{"padded", signinInviteBody(signinInviteWith("email", " someone@example.com")), "email"},
		{"uppercase", signinInviteBody(signinInviteWith("email", "Someone@Example.com")), "lowercase"},
		{"no expiry", signinInviteBody(signinInviteWith("expires_at", nil)), "expires_at"},
		{"no inviter", signinInviteBody(signinInviteWith("invited_by", nil)), "invited_by"},
		{"born claimed", signinInviteBody(signinInviteWith("state", "claimed")), "created in state"},
		{"born mailed", signinInviteBody(signinInviteWith("mailed_at", "2026-10-28T00:00:00Z")), "cannot carry mailed_at"},
		{"born with a user", signinInviteBody(signinInviteWith("claimed_user_id", "u-1")), "cannot carry claimed_user_id"},
		{"born in the gatehouse", signinInviteBody(signinInviteWith("claimed_gatehouse", "gatehouse")), "cannot carry claimed_gatehouse"},
		{"role with no land", signinInviteBody(signinInviteWith("role", "member")), "no role"},
		{"land with no role", signinInviteBody(signinInviteWith("land", "tomato-patch")), "member or guest"},
		{"owner role", signinInviteBody(signinInviteWith("land", "tomato-patch", "role", "owner")), "role"},
		{"unknown land", signinInviteBody(signinInviteWith("land", "nowhere", "role", "member")), "not a project"},
		{"operator land", signinInviteBody(signinInviteWith("land", "gardenio-own", "role", "member")), "not a tenant land"},
	}
	for _, tc := range cases {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: code %d body %s, want 400 containing %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestInvite_UpdateRules(t *testing.T) {
	_, ms, h := newTestServer()
	createLand(t, h, ms, "tomato-patch", "tenant")
	newInvite := func(f map[string]any) string {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", signinInviteBody(f))
		requireStatus(t, rec, http.StatusCreated)
		var b model.Bead
		decodeJSON(t, rec, &b)
		return b.ID
	}
	patch := func(id string, fields map[string]any) int {
		return doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": fields}).Code
	}

	id := newInvite(signinInviteWith("land", "tomato-patch", "role", "member"))
	// Who is invited, to what, by whom and until when never change.
	for k, v := range map[string]any{
		"email": "other@example.com", "land": "other", "role": "guest",
		"invited_by": "oidc:x@example.com", "expires_at": "2027-01-01T00:00:00Z",
	} {
		if code := patch(id, map[string]any{k: v}); code != http.StatusBadRequest {
			t.Errorf("changing %s: code %d, want 400", k, code)
		}
	}
	if code := patch(id, map[string]any{"email": "someone@example.com"}); code != http.StatusOK {
		t.Errorf("same email: code %d", code)
	}
	// The mail stamp and a revoke request are not state changes.
	if code := patch(id, map[string]any{"mailed_at": "2026-10-28T00:00:00Z"}); code != http.StatusOK {
		t.Errorf("mailed_at: code %d", code)
	}
	if code := patch(id, map[string]any{"revoke_requested_at": "2026-10-29T00:00:00Z", "revoke_requested_by": "oidc:owner@example.com"}); code != http.StatusOK {
		t.Errorf("revoke request: code %d", code)
	}
	// pending → claimed; claimed is terminal.
	if code := patch(id, map[string]any{"state": "claimed", "claimed_user_id": "u-1"}); code != http.StatusOK {
		t.Fatalf("claim: code %d", code)
	}
	for _, to := range []string{"revoked", "expired", "pending"} {
		if code := patch(id, map[string]any{"state": to}); code != http.StatusBadRequest {
			t.Errorf("claimed → %s: code %d, want 400", to, code)
		}
	}

	// A land-less invite never gains a land or a role. Its claim may name
	// the gatehouse its person joined (step 9.12).
	id = newInvite(validSigninInviteFields())
	if code := patch(id, map[string]any{"land": "tomato-patch", "role": "member"}); code != http.StatusBadRequest {
		t.Errorf("adding a land: code %d, want 400", code)
	}
	if code := patch(id, map[string]any{"state": "claimed", "claimed_user_id": "u-2", "claimed_gatehouse": "tomato-patch"}); code != http.StatusOK {
		t.Errorf("claim into the gatehouse: code %d", code)
	}
	// pending → expired and pending → revoked are terminal.
	for _, end := range []string{"expired", "revoked"} {
		id = newInvite(validSigninInviteFields())
		if code := patch(id, map[string]any{"state": end}); code != http.StatusOK {
			t.Fatalf("%s: code %d", end, code)
		}
		if code := patch(id, map[string]any{"state": "claimed"}); code != http.StatusBadRequest {
			t.Errorf("%s → claimed: code %d, want 400", end, code)
		}
	}
}

func TestInvite_MachineGrants(t *testing.T) {
	gate := machineCapabilityGrants["gardenio-machine-lands-gate"]
	prov := machineCapabilityGrants["gardenio-machine-lands-provisioner"]
	has := func(c machineCapability, action string) bool {
		for _, w := range c.Writes {
			if w.Action == action && w.BeadType == typeInvite {
				return true
			}
		}
		return false
	}
	if has(gate, "create") || has(gate, "update") || has(gate, "close") {
		t.Errorf("the Gate writes no sign-in invite; an admin files them: %+v", gate.Writes)
	}
	if has(prov, "create") || !has(prov, "update") || !has(prov, "close") {
		t.Errorf("the provisioner moves and closes invites and files none: %+v", prov.Writes)
	}
	if !slices.Contains(prov.Ownership, typeInvite) {
		t.Error("the provisioner updates invites an admin created, so it needs typed ownership of invite")
	}
}

// The provisioner marks the gatehouse's project bead and stamps its one-time
// backfill there (step 9.12).
func TestProject_GatehouseFields(t *testing.T) {
	_, ms, h := newTestServer()
	id := createLand(t, h, ms, "gatehouse", "tenant")
	rec := doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{
		"land_gatehouse": true, "land_gatehouse_backfilled_at": "2026-10-31T18:00:00Z",
	}})
	requireStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": map[string]any{"land_gatehouse": "maybe"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("land_gatehouse=maybe: code %d, want 400", rec.Code)
	}
}
