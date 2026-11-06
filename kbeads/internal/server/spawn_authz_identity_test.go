package server

import (
	"context"
	"errors"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// roleListErrStore fails every type:role list, so roleIsPrivileged cannot
// resolve a role's privilege.
type roleListErrStore struct {
	*mockStore
}

func (s roleListErrStore) ListBeads(ctx context.Context, f model.BeadFilter) ([]*model.Bead, int, error) {
	for _, t := range f.Type {
		if t == "role" {
			return nil, 0, errors.New("store unavailable")
		}
	}
	return s.mockStore.ListBeads(ctx, f)
}

// A store error resolving role privilege must not let a non-human caller skip
// the escalation gate (gd-jTIz9PSHlK): it is denied, escalatably.
func TestAuthorizeAgentSpawn_RoleLookupErrorFailsClosed(t *testing.T) {
	ms := newMockStore()
	makeRoleBead(ms, "gd-role-k8s-live", "k8s-live", true)
	ctx := withAuthMethod(context.Background(), authMethodStatic)

	for _, role := range []string{"k8s-live", "crew"} {
		err := authorizeAgentSpawn(ctx, roleListErrStore{ms}, agentFields("gardenio", role))
		var denial *AuthzDenial
		if !errors.As(err, &denial) {
			t.Fatalf("role %q: lookup error should deny with *AuthzDenial, got %T: %v", role, err, err)
		}
		if !denial.Escalatable || denial.SpawnRole != role {
			t.Errorf("role %q: denial should be escalatable with SpawnRole set, got escalatable=%v spawn_role=%q",
				role, denial.Escalatable, denial.SpawnRole)
		}
	}
}

func TestLookupSpawnPermissions_CaseInsensitiveEmail(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user-mixed", " Alice@Example.COM", "monorepo", "crew")

	perms, err := lookupSpawnPermissions(context.Background(), ms, "alice@example.com")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if perms == nil || len(perms.AllowedProjects) != 1 || perms.AllowedProjects[0] != "monorepo" {
		t.Fatalf("mixed-case bead not found or wrong perms: %+v", perms)
	}

	// Its restriction applies to the lowercase subject.
	err = authorizeAgentSpawn(oidcCtx("alice@example.com", []string{RoleOperator}), ms, agentFields("gardenio", "crew"))
	if err == nil {
		t.Fatal("allowed_projects on a mixed-case user bead should still deny an unlisted project")
	}
}

// Two user beads for one email (in any case) make the permissions depend on
// row order, so every caller of the lookup fails closed.
func TestLookupSpawnPermissions_DuplicateEmailRefused(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user-a", "dup@example.com", "monorepo", "crew")
	makeUserBead(ms, "gd-user-b", "DUP@example.com", "", "")

	if _, err := lookupSpawnPermissions(context.Background(), ms, "dup@example.com"); !errors.Is(err, errAmbiguousUserBead) {
		t.Fatalf("expected errAmbiguousUserBead, got %v", err)
	}

	ctx := oidcCtx("dup@example.com", []string{RoleOperator})
	if err := authorizeAgentSpawn(ctx, ms, agentFields("monorepo", "crew")); err == nil {
		t.Error("spawn should be refused when the email is on two user beads")
	}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	if d := srv.checkProjectScope(ctx, "create", "task", "monorepo", false); d == nil {
		t.Error("project-scoped write should be refused when the email is on two user beads")
	}

	if got := oidcAllowedProjects(ctx, ms); got == nil || len(got) != 0 {
		t.Errorf("read filter should match nothing for an ambiguous email, got %#v", got)
	}
}

// Provisioning finds a user bead whose email differs only in case instead of
// creating a duplicate, and creates nothing for an already-ambiguous email.
func TestEnsureUserBead_CaseInsensitive(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user-mixed", "Bob@Example.com", "", "")
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	id, err := up.ensureUserBead(context.Background(), "bob@example.com")
	if err != nil || id != "gd-user-mixed" {
		t.Fatalf("ensureUserBead = %q, %v; want the existing gd-user-mixed", id, err)
	}

	makeUserBead(ms, "gd-user-dup", "BOB@example.com", "", "")
	before := len(ms.beads)
	up2 := NewUserProvisioner(ms, &events.NoopPublisher{})
	if _, err := up2.ensureUserBead(context.Background(), "bob@example.com"); !errors.Is(err, errAmbiguousUserBead) {
		t.Fatalf("ambiguous email: want errAmbiguousUserBead, got %v", err)
	}
	if len(ms.beads) != before {
		t.Error("ensureUserBead created a bead for an ambiguous email")
	}
}
