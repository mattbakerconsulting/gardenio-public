package server

import (
	"testing"

	"gardenio/kbeads/internal/model"
)

// These tests cover the ADR docs/adr/taints.md §9.1 viewer team-page
// acceptance criterion: "Membership edit controls are visible and functional
// only for the team's own owners and platform admins, matching p1-team's
// write policy." type:team's WritePolicy.MinRole is gardenio-admin (B6), so
// without authorizeTeamMembershipBypass a non-admin owner's `kd team add` /
// `kd team remove` would 403 despite being the team's own owner — these pin
// the narrow carve-out that lets that through.

func TestUpdateBead_TeamOwnerCanEditMembership(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:bob@example.com"],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := oidcCtx("alice@example.com", []string{RoleOperator})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"members":["oidc:bob@example.com","oidc:carol@example.com"],"owners":["oidc:alice@example.com"]}`),
	})
	if err != nil {
		t.Fatalf("expected team owner to edit membership, got error: %v", err)
	}
}

func TestUpdateBead_TeamNonOwnerNonAdminDenied(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00mallory@example.com": 99}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:bob@example.com"],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := oidcCtx("mallory@example.com", []string{RoleOperator})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"members":["oidc:bob@example.com","oidc:mallory@example.com"],"owners":["oidc:alice@example.com"]}`),
	})
	if err == nil {
		t.Fatal("expected non-owner non-admin caller to be denied")
	}
	var denial *AuthzDenial
	if !isAuthzDenial(err, &denial) {
		t.Fatalf("expected an *AuthzDenial, got: %v", err)
	}
}

// TestUpdateBead_TeamMemberNonOwnerDenied covers the operator review's third
// PR #145 required change: a caller who IS a member but is NOT an owner must
// still be denied. callerIsTeamOwner mirrors teamsForOperator's
// member-matching logic exactly, just against `owners` instead of `members`
// (team_membership.go) — a regression that accidentally matched against
// `members` (letting any member, not just an owner, rewrite the team) would
// still pass TestUpdateBead_TeamNonOwnerNonAdminDenied above, since that
// test's caller (mallory) is neither a member nor an owner. This test's
// caller (bob) is a member but not an owner, so it fails specifically on
// that regression.
func TestUpdateBead_TeamMemberNonOwnerDenied(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{
		"oidc\x00alice@example.com": 42,
		"oidc\x00bob@example.com":   43,
	}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":["oidc:bob@example.com"],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := oidcCtx("bob@example.com", []string{RoleOperator})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"members":["oidc:bob@example.com"],"owners":["oidc:alice@example.com","oidc:bob@example.com"]}`),
	})
	if err == nil {
		t.Fatal("expected a member who is not an owner to be denied")
	}
	var denial *AuthzDenial
	if !isAuthzDenial(err, &denial) {
		t.Fatalf("expected an *AuthzDenial, got: %v", err)
	}
}

func TestUpdateBead_TeamOwnerOfDifferentTeamDenied(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":[],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen
	seedTeamBead(ms, "gd-team2", `{"team":"acme-staging","members":[],"owners":["oidc:someone-else@example.com"]}`)
	ms.beads["gd-team2"].Status = model.StatusOpen

	ctx := oidcCtx("alice@example.com", []string{RoleOperator})
	_, err := s.updateBead(ctx, "gd-team2", updateBeadInput{
		Fields: []byte(`{"members":["oidc:alice@example.com"],"owners":["oidc:someone-else@example.com"]}`),
	})
	if err == nil {
		t.Fatal("expected owner of a different team to be denied")
	}
	var denial *AuthzDenial
	if !isAuthzDenial(err, &denial) {
		t.Fatalf("expected an *AuthzDenial, got: %v", err)
	}
}

func TestUpdateBead_TeamOwnerCannotEditNonMembershipFields(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","display":"Acme Prod","members":[],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := oidcCtx("alice@example.com", []string{RoleOperator})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"display":"Renamed","owners":["oidc:alice@example.com"]}`),
	})
	if err == nil {
		t.Fatal("expected owner patching a non-membership field (display) to be denied")
	}
	var denial *AuthzDenial
	if !isAuthzDenial(err, &denial) {
		t.Fatalf("expected an *AuthzDenial, got: %v", err)
	}
}

func TestUpdateBead_TeamOwnerCannotEditTitleAlongsideMembership(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":[],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	newTitle := "renamed"
	ctx := oidcCtx("alice@example.com", []string{RoleOperator})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Title:  &newTitle,
		Fields: []byte(`{"members":["oidc:alice@example.com"],"owners":["oidc:alice@example.com"]}`),
	})
	if err == nil {
		t.Fatal("expected a patch touching Title alongside membership fields to be denied")
	}
}

func TestUpdateBead_TeamAdminCanEditAnyField(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","display":"Acme Prod","members":[],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"display":"Renamed"}`),
	})
	if err != nil {
		t.Fatalf("expected admin to edit any team field, got error: %v", err)
	}
}

// TestUpdateBead_ScopedTokenWithOperatedByHeaderCannotBypassTokenScope pins
// the operator-review blocker on PR #145: authorizeTeamMembershipBypass must
// never override a non-OIDC caller's checkTokenScopes denial, and
// callerIsTeamOwner must never trust a caller-supplied X-Operated-By header.
// Before the fix, a static token scoped to disallow this exact write, paired
// with an X-Operated-By naming a real team owner, overrode its own scope
// restriction and rewrote the team's owners.
func TestUpdateBead_ScopedTokenWithOperatedByHeaderCannotBypassTokenScope(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":[],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := staticCtx()
	ctx = WithTokenTypeScope(ctx, []string{"agent"})
	ctx = WithOperatedByHeader(ctx, "oidc:alice@example.com")

	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"owners":["oidc:mallory@example.com"]}`),
	})
	if err == nil {
		t.Fatal("expected a token-scope-denied caller asserting team ownership via X-Operated-By to be denied")
	}
	var denial *AuthzDenial
	if !isAuthzDenial(err, &denial) {
		t.Fatalf("expected an *AuthzDenial, got: %v", err)
	}
}

// TestUpdateBead_TeamOwnersCannotBeEmptiedByAdmin pins the second
// operator-review "also fix" on PR #145: even an admin cannot empty a team's
// owners, since an owner-less team is unrecoverable through the normal
// non-admin edit path.
func TestUpdateBead_TeamOwnersCannotBeEmptiedByAdmin(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":[],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"owners":[]}`),
	})
	if err == nil {
		t.Fatal("expected emptying owners to be rejected even for an admin")
	}
}

// TestUpdateBead_TeamOwnerCannotEmptyOwnOwners covers the same rule from the
// team-owner bypass path.
func TestUpdateBead_TeamOwnerCannotEmptyOwnOwners(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	ms.operatorAliases = map[string]int64{"oidc\x00alice@example.com": 42}
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","members":[],"owners":["oidc:alice@example.com"]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := oidcCtx("alice@example.com", []string{RoleOperator})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"owners":[]}`),
	})
	if err == nil {
		t.Fatal("expected the team's own owner to be rejected when emptying owners")
	}
}

// TestUpdateBead_TeamUnrelatedFieldEditAllowedDespiteLegacyEmptyOwners
// confirms the empty-owners check only fires when the patch itself touches
// `owners`: a legacy team that already carries an empty/absent owners list
// can still have unrelated fields edited by an admin.
func TestUpdateBead_TeamUnrelatedFieldEditAllowedDespiteLegacyEmptyOwners(t *testing.T) {
	s, ms := newTeamMembershipTestServer()
	seedTeamBead(ms, "gd-team1", `{"team":"acme-prod","display":"Old Name","members":[],"owners":[]}`)
	ms.beads["gd-team1"].Status = model.StatusOpen

	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})
	_, err := s.updateBead(ctx, "gd-team1", updateBeadInput{
		Fields: []byte(`{"display":"New Name"}`),
	})
	if err != nil {
		t.Fatalf("expected an unrelated field edit to succeed despite legacy empty owners, got error: %v", err)
	}
}

// isAuthzDenial unwraps err into an *AuthzDenial, mirroring errors.As without
// importing the errors package solely for this helper.
func isAuthzDenial(err error, target **AuthzDenial) bool {
	d, ok := err.(*AuthzDenial)
	if !ok {
		return false
	}
	*target = d
	return true
}
