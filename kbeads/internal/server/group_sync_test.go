package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// --- Unit tests for groupsToProjects ---

func TestGroupsToProjects_StandardPaths(t *testing.T) {
	groups := []string{"/project-alpha", "/project-beta"}
	got := groupsToProjects(groups)
	if len(got) != 2 {
		t.Fatalf("expected 2 projects, got %d: %v", len(got), got)
	}
	if got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("got %v, want [alpha, beta]", got)
	}
}

func TestGroupsToProjects_WithoutLeadingSlash(t *testing.T) {
	groups := []string{"project-gamma"}
	got := groupsToProjects(groups)
	if len(got) != 1 || got[0] != "gamma" {
		t.Errorf("expected [gamma], got %v", got)
	}
}

func TestGroupsToProjects_NonMatchingGroupsIgnored(t *testing.T) {
	groups := []string{"/admin-group", "/users", "random", "/project-alpha"}
	got := groupsToProjects(groups)
	if len(got) != 1 || got[0] != "alpha" {
		t.Errorf("expected [alpha], got %v", got)
	}
}

func TestGroupsToProjects_EmptyGroups(t *testing.T) {
	got := groupsToProjects(nil)
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestGroupsToProjects_PrefixOnlyIgnored(t *testing.T) {
	// "project-" with no name after it should be ignored.
	groups := []string{"/project-"}
	got := groupsToProjects(groups)
	if len(got) != 0 {
		t.Errorf("expected empty for bare prefix, got %v", got)
	}
}

func TestGroupsToProjects_MixedFormats(t *testing.T) {
	groups := []string{"/project-alpha", "project-beta", "/other-group", "/project-gamma"}
	got := groupsToProjects(groups)
	if len(got) != 3 {
		t.Fatalf("expected 3 projects, got %d: %v", len(got), got)
	}
}

// --- Integration tests for group sync through UserProvisioner middleware ---

func oidcCtxWithGroups(email string, roles []string, groups []string) *http.Request {
	req := httptest.NewRequest("GET", "/v1/beads", nil)
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email})
	ctx = WithRoles(ctx, roles)
	ctx = WithGroups(ctx, groups)
	return req.WithContext(ctx)
}

func TestGroupSync_SyncsProjectsToUserBead(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	// Pre-create a user bead with no allowed_projects.
	fieldsJSON, _ := json.Marshal(map[string]string{"email": "alice@example.com"})
	ms.beads["gd-user-gs1"] = &model.Bead{
		ID:     "gd-user-gs1",
		Title:  "alice",
		Type:   "user",
		Kind:   "config",
		Status: model.StatusOpen,
		Fields: fieldsJSON,
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	req := oidcCtxWithGroups("alice@example.com", []string{RoleOperator}, []string{"/project-alpha", "/project-beta"})
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Verify user bead now has allowed_projects.
	bead := ms.beads["gd-user-gs1"]
	if bead == nil {
		t.Fatal("user bead not found")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}

	var projects []string
	if err := json.Unmarshal(fields["allowed_projects"], &projects); err != nil {
		t.Fatalf("unmarshal allowed_projects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d: %v", len(projects), projects)
	}

	projectSet := map[string]bool{}
	for _, p := range projects {
		projectSet[p] = true
	}
	if !projectSet["alpha"] || !projectSet["beta"] {
		t.Errorf("expected [alpha, beta], got %v", projects)
	}
}

func TestGroupSync_NoGroupsDoesNotModifyBead(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	fieldsJSON, _ := json.Marshal(map[string]string{"email": "bob@example.com"})
	ms.beads["gd-user-gs2"] = &model.Bead{
		ID:     "gd-user-gs2",
		Title:  "bob",
		Type:   "user",
		Kind:   "config",
		Status: model.StatusOpen,
		Fields: fieldsJSON,
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	// No groups in context — should not modify the bead.
	req := httptest.NewRequest("GET", "/v1/beads", nil)
	ctx := withAuthMethod(req.Context(), authMethodOIDC)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: "bob@example.com"})
	ctx = WithRoles(ctx, []string{RoleOperator})
	// Intentionally NOT calling WithGroups — no groups in JWT.
	req = req.WithContext(ctx)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Verify fields are unchanged.
	bead := ms.beads["gd-user-gs2"]
	var fields map[string]string
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if _, ok := fields["allowed_projects"]; ok {
		t.Error("allowed_projects should not be set when no groups are present")
	}
}

func TestGroupSync_CachePreventsRedundantUpdates(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	// First request — creates user bead and syncs groups.
	req1 := oidcCtxWithGroups("carol@example.com", []string{RoleOperator}, []string{"/project-alpha"})
	handler.ServeHTTP(httptest.NewRecorder(), req1)

	// Find the created bead.
	var beadID string
	for id, b := range ms.beads {
		if b.Type == "user" {
			beadID = id
			break
		}
	}
	if beadID == "" {
		t.Fatal("user bead not created")
	}

	// Record the bead state after first sync.
	bead1Fields := make([]byte, len(ms.beads[beadID].Fields))
	copy(bead1Fields, ms.beads[beadID].Fields)

	// Second request with same groups — should hit cache, no update.
	req2 := oidcCtxWithGroups("carol@example.com", []string{RoleOperator}, []string{"/project-alpha"})
	handler.ServeHTTP(httptest.NewRecorder(), req2)

	// Fields should be identical (no unnecessary write).
	bead2Fields := ms.beads[beadID].Fields
	if string(bead1Fields) != string(bead2Fields) {
		t.Error("expected no field change on second request with same groups")
	}
}

func TestGroupSync_UpdatesWhenGroupsChange(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	// First request with project-alpha.
	req1 := oidcCtxWithGroups("dave@example.com", []string{RoleOperator}, []string{"/project-alpha"})
	handler.ServeHTTP(httptest.NewRecorder(), req1)

	var beadID string
	for id, b := range ms.beads {
		if b.Type == "user" {
			beadID = id
			break
		}
	}
	if beadID == "" {
		t.Fatal("user bead not created")
	}

	// Second request with different groups — should update.
	req2 := oidcCtxWithGroups("dave@example.com", []string{RoleOperator}, []string{"/project-beta", "/project-gamma"})
	handler.ServeHTTP(httptest.NewRecorder(), req2)

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ms.beads[beadID].Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}

	var projects []string
	if err := json.Unmarshal(fields["allowed_projects"], &projects); err != nil {
		t.Fatalf("unmarshal allowed_projects: %v", err)
	}

	projectSet := map[string]bool{}
	for _, p := range projects {
		projectSet[p] = true
	}
	if !projectSet["beta"] || !projectSet["gamma"] || projectSet["alpha"] {
		t.Errorf("expected [beta, gamma], got %v", projects)
	}
}

func TestGroupSync_NonProjectGroupsIgnored(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	fieldsJSON, _ := json.Marshal(map[string]string{"email": "eve@example.com"})
	ms.beads["gd-user-gs5"] = &model.Bead{
		ID:     "gd-user-gs5",
		Title:  "eve",
		Type:   "user",
		Kind:   "config",
		Status: model.StatusOpen,
		Fields: fieldsJSON,
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	// Groups that don't match the project- convention should be ignored.
	req := oidcCtxWithGroups("eve@example.com", []string{RoleOperator}, []string{"/admin-group", "/users"})
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// No allowed_projects should be set since no groups matched the convention.
	bead := ms.beads["gd-user-gs5"]
	var fields map[string]string
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if _, ok := fields["allowed_projects"]; ok {
		t.Error("allowed_projects should not be set when no groups match the project- convention")
	}
}

func TestGroupSync_PreservesExistingFields(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})

	// Pre-create user bead with custom fields.
	existingFields := map[string]any{
		"email":         "frank@example.com",
		"auth_methods":  "gitlab-oauth",
		"allowed_roles": []string{"crew", "gardener"},
	}
	fieldsJSON, _ := json.Marshal(existingFields)
	ms.beads["gd-user-gs6"] = &model.Bead{
		ID:     "gd-user-gs6",
		Title:  "frank",
		Type:   "user",
		Kind:   "config",
		Status: model.StatusOpen,
		Fields: fieldsJSON,
	}

	// Pre-populate the provisioner cache so it finds the pre-created bead.
	up.mu.Lock()
	up.cache["frank@example.com"] = "gd-user-gs6"
	up.mu.Unlock()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := up.Middleware(inner)

	req := oidcCtxWithGroups("frank@example.com", []string{RoleOperator}, []string{"/project-alpha"})
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Verify existing fields are preserved alongside new allowed_projects.
	bead := ms.beads["gd-user-gs6"]
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}

	// email still present
	var email string
	json.Unmarshal(fields["email"], &email)
	if email != "frank@example.com" {
		t.Errorf("email = %q, want frank@example.com", email)
	}

	// allowed_roles still present
	var roles []string
	json.Unmarshal(fields["allowed_roles"], &roles)
	if len(roles) != 2 {
		t.Errorf("allowed_roles lost: got %v", roles)
	}

	// allowed_projects added
	var projects []string
	json.Unmarshal(fields["allowed_projects"], &projects)
	if len(projects) != 1 || projects[0] != "alpha" {
		t.Errorf("allowed_projects = %v, want [alpha]", projects)
	}
}

func TestGroupsContext(t *testing.T) {
	ctx := WithGroups(context.Background(), []string{"group-a", "group-b"})
	got := GroupsFromContext(ctx)
	if len(got) != 2 || got[0] != "group-a" || got[1] != "group-b" {
		t.Errorf("expected [group-a, group-b], got %v", got)
	}
}

func TestGroupsFromContext_Empty(t *testing.T) {
	got := GroupsFromContext(context.Background())
	if got != nil {
		t.Errorf("expected nil from bare context, got %v", got)
	}
}
