package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// A privileged ownership_grants row on a role bead (bypassRows: update and
// close on every type but decision) lets every agent holding the role mutate
// beads it did not create, so reaching such a role through spawn or re-role is
// the same authority grant as a privileged role: it must escalate for a
// non-human caller (gd-kLRIbndGNp flip prerequisite D).

func makeBypassRoleBead(ms *mockStore, id, name string) {
	ms.beads[id] = &model.Bead{
		ID: id, Title: name, Type: "role", Kind: "config", Status: model.StatusOpen,
		Fields: json.RawMessage(bypassRoleFields),
	}
}

func TestRoleIsPrivileged_AllTypesRowCounts(t *testing.T) {
	ms := newMockStore()
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	makeBypassRoleBead(ms, "gd-role-reviewer", "reviewer")
	ms.beads["gd-role-noflag"] = &model.Bead{ID: "gd-role-noflag", Title: "qa", Type: "role", Kind: "config", Status: model.StatusOpen,
		Fields: json.RawMessage(`{"ownership_grants":[]}`)}

	for _, tc := range []struct {
		role string
		want bool
	}{
		{"reviewer", true},
		{"crew, reviewer", true}, // any element of a comma list
		{"crew", false},
		{"qa", false}, // explicit empty list
		{"reviewers", false},
		{"", false},
	} {
		got, err := roleIsPrivileged(context.Background(), ms, tc.role)
		if err != nil || got != tc.want {
			t.Errorf("roleIsPrivileged(%q) = %v, %v; want %v", tc.role, got, err, tc.want)
		}
	}
}

func TestAuthorizeAgentSpawn_NonHumanIntoBypassRoleEscalates(t *testing.T) {
	ms := newMockStore()
	makeBypassRoleBead(ms, "gd-role-reviewer", "reviewer")
	makeRoleBead(ms, "gd-role-crew", "crew", false)

	// A crew on its own per-agent token spawning a reviewer child.
	ms.beads["gd-crew1"] = &model.Bead{ID: "gd-crew1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	crewCtx := withAuthMethod(agentFleetCtx("gd-crew1", "crew-a"), authMethodStatic)
	err := authorizeAgentSpawn(crewCtx, ms, agentFields("gardenio", "reviewer"))
	var d *AuthzDenial
	if !errors.As(err, &d) || !d.Escalatable || d.SpawnRole != "reviewer" {
		t.Fatalf("spawn into a privileged-row role must be an escalatable denial, got %v", err)
	}
	// The primary static token is held to the same gate.
	if err := authorizeAgentSpawn(withAuthMethod(context.Background(), authMethodStatic), ms, agentFields("gardenio", "reviewer")); err == nil {
		t.Fatal("static token spawning a privileged-row role must be denied")
	}
	// Controls: an ordinary role is unaffected; an OIDC admin passes; an
	// approved-escalation replay passes.
	if err := authorizeAgentSpawn(crewCtx, ms, agentFields("gardenio", "crew")); err != nil {
		t.Fatalf("non-bypass role must be unaffected: %v", err)
	}
	if err := authorizeAgentSpawn(oidcCtx("admin@example.com", []string{RoleAdmin}), ms, agentFields("gardenio", "reviewer")); err != nil {
		t.Fatalf("admin must pass: %v", err)
	}
}

func TestAuthorizeAgentRoleUpdate_ReRoleIntoBypassRoleEscalates(t *testing.T) {
	ms := newMockStore()
	makeBypassRoleBead(ms, "gd-role-reviewer", "reviewer")
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	agent := &model.Bead{ID: "gd-crew1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	ms.beads[agent.ID] = agent
	crewCtx := withAuthMethod(agentFleetCtx("gd-crew1", "crew-a"), authMethodStatic)

	err := authorizeAgentRoleUpdate(crewCtx, ms, agent, json.RawMessage(`{"role":"reviewer"}`))
	var d *AuthzDenial
	if !errors.As(err, &d) || !d.Escalatable || d.SpawnRole != "reviewer" || d.Action != "update" {
		t.Fatalf("re-role into a privileged-row role must be an escalatable denial, got %v", err)
	}
	if err := authorizeAgentRoleUpdate(crewCtx, ms, agent, json.RawMessage(`{"role":"crew, reviewer"}`)); err == nil {
		t.Fatal("adding a privileged-row role to a comma list must be denied")
	}
	// Controls: leaving the role alone, and an echoed unchanged role, pass.
	if err := authorizeAgentRoleUpdate(crewCtx, ms, agent, json.RawMessage(`{"agent_state":"working"}`)); err != nil {
		t.Fatalf("unrelated update denied: %v", err)
	}
	if err := authorizeAgentRoleUpdate(crewCtx, ms, agent, json.RawMessage(`{"role":"crew"}`)); err != nil {
		t.Fatalf("echoed role denied: %v", err)
	}
}

// Role beads are looked up by exact title, not by listing the first N role
// beads: the bead carrying the grant is found however many other roles exist.
func TestRoleBeadLookup_ByExactTitle_NotAListCap(t *testing.T) {
	ms := newMockStore()
	for i := 0; i < 700; i++ {
		id := "gd-role-filler-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i/676))
		ms.beads[id] = &model.Bead{ID: id, Title: "filler-" + id, Type: "role", Kind: "config", Status: model.StatusOpen, Fields: json.RawMessage(`{}`)}
	}
	makeBypassRoleBead(ms, "gd-role-reviewer", "reviewer")

	if got, err := roleIsPrivileged(context.Background(), ms, "reviewer"); err != nil || !got {
		t.Fatalf("grant among 700 other roles must be found, got %v, %v", got, err)
	}
	f := ms.lastListFilter
	if len(f.Titles) != 1 || f.Titles[0] != "reviewer" || f.Limit != 0 {
		t.Errorf("lookup must be by exact title with no cap, got Titles=%v Limit=%d", f.Titles, f.Limit)
	}

	srv := &BeadsServer{store: ms}
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", "reviewer")}
	foreign := &model.Bead{ID: "gd-x", Type: "task", Kind: model.KindIssue, CreatedBy: "someone-else", Status: model.StatusOpen}
	if adm, ok := srv.evaluateOwnershipGrants(agentFleetCtx("gd-a1", "reviewer-a"), "update", foreign); !ok || adm.grant != "role:reviewer/0" {
		t.Fatalf("the role row must be found among 700 other roles, got %q %v", adm.grant, ok)
	}
}

// The ownership bypass covers update and close, not delete: close already ends
// a bead's life and no coordinating role needs to delete another crew's work.
func TestAuthorizeOwnership_RoleBypassExcludesDelete(t *testing.T) {
	foreign := &model.Bead{ID: "gd-x", Type: "task", CreatedBy: "someone-else", Status: model.StatusOpen}
	srv := newChainWalkServer(
		&model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", "reviewer")},
		roleBeadWith("gd-r1", "reviewer", bypassRoleFields, model.StatusOpen),
	)
	ctx := agentFleetCtx("gd-a1", "reviewer-a")
	for _, action := range []string{"update", "close"} {
		if d := srv.authorizeOwnership(ctx, action, foreign); d != nil {
			t.Errorf("%s: role bypass must apply, got %v", action, d)
		}
	}
	if d := srv.authorizeOwnership(ctx, "delete", foreign); d == nil {
		t.Error("delete: role bypass must NOT apply to another crew's bead")
	}
	// The agent's own bead is still deletable (creator passes before the bypass).
	own := &model.Bead{ID: "gd-y", Type: "task", CreatedBy: "reviewer-a", Status: model.StatusOpen}
	if d := srv.authorizeOwnership(ctx, "delete", own); d != nil {
		t.Errorf("delete of own bead denied: %v", d)
	}
}

// Setting ownership_grants is admin-only. This is the AGENT-token write path
// (a pod's own per-agent fleet token, the caller the flip puts every crew on),
// distinct from the machine/static caller covered in
// TestRoleBead_OwnershipGrants_MachineDenied. A narrow row is gated as an
// all-types one is: any row widens the role.
func TestRoleBead_OwnershipGrants_AgentTokenDenied(t *testing.T) {
	agentCtx := withAuthMethod(agentFleetCtx("gd-crew1", "crew-a"), authMethodStatic)

	s, sms, _ := newTestServer()
	seedAgent(sms, "gd-crew1", "crew", "gardenio") // the write gate reads the caller's role
	for name, fields := range map[string]string{
		"all types": `{"image":"registry/agent:latest","ownership_grants":` + bypassRows + `}`,
		"narrow":    herderRowsGrant,
	} {
		requireDenied(t, func() error {
			_, err := s.createBead(agentCtx, createBeadInput{Title: "reviewer-" + name, Type: "role", Fields: json.RawMessage(fields)})
			return err
		}())
	}
	// Control: an empty list grants nothing and is not gated.
	if _, err := s.createBead(agentCtx, createBeadInput{Title: "reviewer3", Type: "role", Fields: json.RawMessage(`{"ownership_grants":[]}`)}); err != nil {
		t.Fatalf("empty grant create by agent token denied: %v", err)
	}

	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedConfigBead(t, ms, "gd-role1", "role", `{"image":"registry/agent:latest"}`)
	if d := srv.authorizeAgentIdentityFields(agentCtx, "update", "role", json.RawMessage(bypassRoleFields), json.RawMessage(`{"image":"registry/agent:latest"}`)); d == nil {
		t.Fatal("agent-token update setting ownership_grants must be denied")
	}
	if d := srv.authorizeAgentBeadFieldWrite(agentCtx, "update", ms.beads["gd-role1"], roleOwnershipGrantsField); d == nil {
		t.Fatal("agent-token single-field write of ownership_grants must be denied")
	}
	// Control: admin passes.
	if d := srv.authorizeAgentIdentityFields(adminOIDCCtx(), "update", "role", json.RawMessage(bypassRoleFields), json.RawMessage(`{}`)); d != nil {
		t.Fatalf("admin update denied: %v", d)
	}
}
