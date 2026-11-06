package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
)

// --- Unit tests for read_authz helpers ---

func TestOidcAllowedProjects_StaticTokenReturnsNil(t *testing.T) {
	ms := newMockStore()
	ctx := withAuthMethod(context.Background(), authMethodStatic)

	got := oidcAllowedProjects(ctx, ms)
	if got != nil {
		t.Fatalf("static token should return nil, got %v", got)
	}
}

func TestOidcAllowedProjects_AdminReturnsNil(t *testing.T) {
	ms := newMockStore()
	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})

	got := oidcAllowedProjects(ctx, ms)
	if got != nil {
		t.Fatalf("admin should return nil (unrestricted), got %v", got)
	}
}

func TestOidcAllowedProjects_NoUserBeadReturnsNil(t *testing.T) {
	ms := newMockStore()
	ctx := oidcCtx("unknown@example.com", []string{RoleOperator})

	got := oidcAllowedProjects(ctx, ms)
	if got != nil {
		t.Fatalf("no user bead should return nil (backward compat), got %v", got)
	}
}

func TestOidcAllowedProjects_EmptyAllowedProjectsReturnsNil(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-u1", "empty@example.com", "", "crew")
	ctx := oidcCtx("empty@example.com", []string{RoleOperator})

	got := oidcAllowedProjects(ctx, ms)
	if got != nil {
		t.Fatalf("empty allowed_projects should return nil, got %v", got)
	}
}

func TestOidcAllowedProjects_WildcardReturnsNil(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-u2", "wildcard@example.com", "*", "crew")
	ctx := oidcCtx("wildcard@example.com", []string{RoleOperator})

	got := oidcAllowedProjects(ctx, ms)
	if got != nil {
		t.Fatalf("wildcard should return nil (unrestricted), got %v", got)
	}
}

func TestOidcAllowedProjects_ReturnsConfiguredProjects(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-u3", "alice@example.com", "projA,projB", "crew")
	ctx := oidcCtx("alice@example.com", []string{RoleOperator})

	got := oidcAllowedProjects(ctx, ms)
	if got == nil {
		t.Fatal("expected non-nil project list")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 projects, got %d: %v", len(got), got)
	}
	wantSet := map[string]bool{"projA": true, "projB": true}
	for _, p := range got {
		if !wantSet[p] {
			t.Errorf("unexpected project %q", p)
		}
	}
}

func TestOidcAllowedProjects_NoIdentityReturnsNil(t *testing.T) {
	ms := newMockStore()
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: ""})

	got := oidcAllowedProjects(ctx, ms)
	if got != nil {
		t.Fatalf("no identity should return nil, got %v", got)
	}
}

func TestOidcProjectLabels(t *testing.T) {
	labels := oidcProjectLabels([]string{"projA", "projB"})
	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(labels))
	}
	if labels[0] != "project:projA" {
		t.Errorf("labels[0]=%q, want project:projA", labels[0])
	}
	if labels[1] != "project:projB" {
		t.Errorf("labels[1]=%q, want project:projB", labels[1])
	}
}

func TestBeadMatchesOIDCProjects_NilAllowedReturnsTrue(t *testing.T) {
	if !beadMatchesOIDCProjects([]string{"project:x"}, nil) {
		t.Fatal("nil allowedProjects should return true (unrestricted)")
	}
}

func TestBeadMatchesOIDCProjects_MatchingProject(t *testing.T) {
	if !beadMatchesOIDCProjects([]string{"project:alpha", "team:eng"}, []string{"alpha", "beta"}) {
		t.Fatal("bead with project:alpha should match allowedProjects [alpha, beta]")
	}
}

func TestBeadMatchesOIDCProjects_NoMatch(t *testing.T) {
	if beadMatchesOIDCProjects([]string{"project:gamma", "team:eng"}, []string{"alpha", "beta"}) {
		t.Fatal("bead with project:gamma should NOT match allowedProjects [alpha, beta]")
	}
}

func TestBeadMatchesOIDCProjects_NoLabels(t *testing.T) {
	if beadMatchesOIDCProjects(nil, []string{"alpha"}) {
		t.Fatal("bead with no labels should NOT match when allowedProjects is set")
	}
}

// --- Integration tests: OIDC read filtering through HTTP handlers ---

func TestOIDCReadIsolation_ListBeads_FilteredByAllowedProjects(t *testing.T) {
	srv, ms, _ := testCtx(t)

	// Create user bead for alice with access to projA and projB.
	makeUserBead(ms, "gd-user-read1", "alice@example.com", "projA,projB", "crew")

	// Create beads in different projects.
	for _, tc := range []struct {
		id      string
		project string
	}{
		{"gd-bead-a", "projA"},
		{"gd-bead-b", "projB"},
		{"gd-bead-c", "projC"},
		{"gd-bead-d", ""}, // no project label
	} {
		ms.beads[tc.id] = &model.Bead{
			ID:     tc.id,
			Title:  "Test bead " + tc.id,
			Type:   "task",
			Kind:   "issue",
			Status: model.StatusOpen,
			Fleet:  "default",
		}
		if tc.project != "" {
			ms.labels[tc.id] = []string{"project:" + tc.project}
		}
	}

	// Call ListBeads as alice (OIDC with projA,projB access).
	ctx := oidcCtx("alice@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")

	beads, _, err := srv.store.ListBeads(ctx, srv.scopeListFilter(ctx, model.BeadFilter{Fleet: "default"}))
	if err != nil {
		t.Fatalf("ListBeads failed: %v", err)
	}

	// Should only see beads from projA and projB.
	for _, b := range beads {
		labels := ms.labels[b.ID]
		if !containsLabel(labels, "project:projA") && !containsLabel(labels, "project:projB") {
			t.Errorf("bead %s (labels=%v) should not be visible to alice", b.ID, labels)
		}
	}
	if len(beads) != 2 {
		ids := make([]string, len(beads))
		for i, b := range beads {
			ids[i] = b.ID
		}
		t.Fatalf("expected 2 beads (projA, projB), got %d: %v", len(beads), ids)
	}
}

func TestOIDCReadIsolation_ListBeads_WildcardSeesAll(t *testing.T) {
	srv, ms, _ := testCtx(t)

	makeUserBead(ms, "gd-user-read2", "admin-user@example.com", "*", "")

	ms.beads["gd-bead-x"] = &model.Bead{
		ID: "gd-bead-x", Title: "X", Type: "task", Kind: "issue",
		Status: model.StatusOpen, Fleet: "default",
	}
	ms.labels["gd-bead-x"] = []string{"project:projX"}
	ms.beads["gd-bead-y"] = &model.Bead{
		ID: "gd-bead-y", Title: "Y", Type: "task", Kind: "issue",
		Status: model.StatusOpen, Fleet: "default",
	}
	ms.labels["gd-bead-y"] = []string{"project:projY"}

	ctx := oidcCtx("admin-user@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")

	beads, _, err := srv.store.ListBeads(ctx, srv.scopeListFilter(ctx, model.BeadFilter{Fleet: "default"}))
	if err != nil {
		t.Fatalf("ListBeads failed: %v", err)
	}
	// Wildcard user should see all beads (including the user bead itself).
	if len(beads) < 2 {
		t.Fatalf("wildcard user should see all beads, got %d", len(beads))
	}
}

func TestOIDCReadIsolation_ListBeads_NoUserBeadSeesAll(t *testing.T) {
	srv, ms, _ := testCtx(t)

	// No user bead for this email — backward compat: sees everything.
	ms.beads["gd-bead-z"] = &model.Bead{
		ID: "gd-bead-z", Title: "Z", Type: "task", Kind: "issue",
		Status: model.StatusOpen, Fleet: "default",
	}
	ms.labels["gd-bead-z"] = []string{"project:secret"}

	ctx := oidcCtx("newuser@example.com", []string{RoleViewer})
	ctx = WithFleet(ctx, "default")

	beads, _, err := srv.store.ListBeads(ctx, srv.scopeListFilter(ctx, model.BeadFilter{Fleet: "default"}))
	if err != nil {
		t.Fatalf("ListBeads failed: %v", err)
	}
	if len(beads) < 1 {
		t.Fatal("user with no user bead should see all beads (backward compat)")
	}
}

func TestOIDCReadIsolation_GetBead_DeniedForOutOfScope(t *testing.T) {
	srv, ms, _ := testCtx(t)

	makeUserBead(ms, "gd-user-read3", "bob@example.com", "projA", "crew")

	ms.beads["gd-secret"] = &model.Bead{
		ID: "gd-secret", Title: "Secret", Type: "task", Kind: "issue",
		Status: model.StatusOpen, Fleet: "default",
	}
	ms.labels["gd-secret"] = []string{"project:projB"}

	ctx := oidcCtx("bob@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")

	_, err := srv.getBeadFiltered(ctx, "gd-secret")
	if err == nil {
		t.Fatal("expected error for out-of-scope bead")
	}
	if err.Error() != "not authorized to read beads in this project" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOIDCReadIsolation_GetBead_AllowedForInScope(t *testing.T) {
	srv, ms, _ := testCtx(t)

	makeUserBead(ms, "gd-user-read4", "carol@example.com", "projA", "crew")

	ms.beads["gd-mine"] = &model.Bead{
		ID: "gd-mine", Title: "Mine", Type: "task", Kind: "issue",
		Status: model.StatusOpen, Fleet: "default",
	}
	ms.labels["gd-mine"] = []string{"project:projA"}

	ctx := oidcCtx("carol@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")

	bead, err := srv.getBeadFiltered(ctx, "gd-mine")
	if err != nil {
		t.Fatalf("expected no error for in-scope bead, got: %v", err)
	}
	if bead.ID != "gd-mine" {
		t.Fatalf("expected gd-mine, got %s", bead.ID)
	}
}

func TestOIDCReadIsolation_StaticTokenUnaffected(t *testing.T) {
	srv, ms, _ := testCtx(t)

	ms.beads["gd-any"] = &model.Bead{
		ID: "gd-any", Title: "Any", Type: "task", Kind: "issue",
		Status: model.StatusOpen, Fleet: "default",
	}
	ms.labels["gd-any"] = []string{"project:secret"}

	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithFleet(ctx, "default")

	bead, err := srv.getBeadFiltered(ctx, "gd-any")
	if err != nil {
		t.Fatalf("static token should have unrestricted access, got: %v", err)
	}
	if bead.ID != "gd-any" {
		t.Fatalf("expected gd-any, got %s", bead.ID)
	}
}
