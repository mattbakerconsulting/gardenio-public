package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Spawn gate and escalation authz read capability_tiers (gd-Pcyz0ftcTg):
// positive controls for an allowed tier and a denied one, and the Lands Track B
// §5.1 lands-tenant refusals.

func makeTierRoleBead(ms *mockStore, id, name, fields string) {
	ms.beads[id] = &model.Bead{
		ID: id, Title: name, Type: "role", Kind: "config", Status: model.StatusOpen,
		Fields: json.RawMessage(fields),
	}
}

func TestRoleIsPrivileged_ReadsCapabilityTiers(t *testing.T) {
	ms := newMockStore()
	makeTierRoleBead(ms, "gd-r1", "tier-privileged", `{"capability_tiers":["privileged"]}`)
	makeTierRoleBead(ms, "gd-r2", "tier-tenant", `{"capability_tiers":["lands-tenant"],"privileged":false}`)
	makeTierRoleBead(ms, "gd-r3", "tier-none", `{"capability_tiers":[],"privileged":false}`)
	makeTierRoleBead(ms, "gd-r4", "legacy-flag", `{"privileged":true}`)
	makeTierRoleBead(ms, "gd-r5", "legacy-string", `{"privileged":"true"}`)
	makeTierRoleBead(ms, "gd-r6", "diverged", `{"capability_tiers":[],"privileged":true}`)
	makeTierRoleBead(ms, "gd-r7", "malformed-set", `{"capability_tiers":42}`)
	makeTierRoleBead(ms, "gd-r8", "malformed-fields", `not json`)
	makeTierRoleBead(ms, "gd-r9", "no-fields", ``)
	makeTierRoleBead(ms, "gd-r10", "null-fields", `null`)

	for _, tc := range []struct {
		role string
		want bool
	}{
		{"tier-privileged", true}, // the set alone, no flag
		{"tier-tenant", false},    // a restricted tier is not privileged
		{"tier-none", false},
		{"legacy-flag", true}, // a role the backfill has not reached reads as before
		{"legacy-string", true},
		{"diverged", true},         // the flag still counts while it is a derived view
		{"malformed-set", true},    // fails closed
		{"malformed-fields", true}, // fails closed
		{"no-fields", false},
		{"null-fields", false},
		{"crew, tier-privileged", true}, // any element of a comma list
		{"tier-tenant, tier-none", false},
	} {
		got, err := roleIsPrivileged(context.Background(), ms, tc.role)
		if err != nil || got != tc.want {
			t.Errorf("roleIsPrivileged(%q) = %v, %v; want %v", tc.role, got, err, tc.want)
		}
	}
}

// The positive control the task names: a non-human spawn of a role holding an
// allowed tier passes, and of one holding the privileged tier (set through
// capability_tiers only) escalates.
func TestAuthorizeAgentSpawn_TierPositiveControls(t *testing.T) {
	ms := newMockStore()
	makeTierRoleBead(ms, "gd-r1", "k8s-live", `{"capability_tiers":["privileged"]}`)
	makeTierRoleBead(ms, "gd-r2", "lands-gardener", `{"capability_tiers":["lands-tenant"]}`)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	ms.beads["gd-crew1"] = &model.Bead{ID: "gd-crew1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	crewCtx := withAuthMethod(agentFleetCtx("gd-crew1", "crew-a"), authMethodStatic)

	if err := authorizeAgentSpawn(crewCtx, ms, agentFields("gardenio", "lands-gardener")); err != nil {
		t.Fatalf("allowed tier: spawn of a lands-tenant role must pass the privileged gate, got %v", err)
	}
	err := authorizeAgentSpawn(crewCtx, ms, agentFields("gardenio", "k8s-live"))
	var d *AuthzDenial
	if !errors.As(err, &d) || !d.Escalatable || d.SpawnRole != "k8s-live" {
		t.Fatalf("denied tier: spawn of a privileged-tier role must escalate, got %v", err)
	}
	// The same split for the escalation inbox's view of a spawn.
	srv := &BeadsServer{store: ms}
	for role, want := range map[string]bool{"k8s-live": true, "lands-gardener": false} {
		esc := &model.Bead{ID: "gd-esc-" + role, Type: model.TypeEscalation,
			Fields: json.RawMessage(`{"operation_type":"create","spawn_role":"` + role + `","operation_spec":{"type":"agent","fields":{"role":"` + role + `","project":"gardenio"}}}`)}
		if got := srv.escalationAudienceForced(context.Background(), esc); got != want {
			t.Errorf("escalationAudienceForced(spawn of %s) = %v, want %v", role, got, want)
		}
	}
}

// landFixture is a land "plot-a" with its steward and one lands-gardener.
type landFixture struct {
	ms         *mockStore
	steward    context.Context
	gardener   context.Context
	crew       context.Context
	gardenerAB *model.Bead
}

func newLandFixture(t *testing.T) *landFixture {
	t.Helper()
	ms := newMockStore()
	makeTierRoleBead(ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	makeTierRoleBead(ms, "gd-role-lg", landsGardenerRole, `{"capability_tiers":["lands-tenant"]}`)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	makeRoleBead(ms, "gd-role-infra", "infra", true)
	ms.beads["gd-stew"] = &model.Bead{ID: "gd-stew", Type: "agent", Fields: agentFields("plot-a", landsStewardRole)}
	lg := &model.Bead{ID: "gd-lg", Type: "agent", Fields: agentFields("plot-a", landsGardenerRole)}
	ms.beads[lg.ID] = lg
	ms.beads["gd-crew1"] = &model.Bead{ID: "gd-crew1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	return &landFixture{
		ms:         ms,
		steward:    withAuthMethod(agentFleetCtx("gd-stew", "steward-a"), authMethodStatic),
		gardener:   withAuthMethod(agentFleetCtx("gd-lg", "lands-gardener-a"), authMethodStatic),
		crew:       withAuthMethod(agentFleetCtx("gd-crew1", "crew-a"), authMethodStatic),
		gardenerAB: lg,
	}
}

func requireTenantDenial(t *testing.T, err error) {
	t.Helper()
	var d *AuthzDenial
	if !errors.As(err, &d) {
		t.Fatalf("want a lands-tenant *AuthzDenial, got %T: %v", err, err)
	}
	if d.Escalatable || d.DeniedReason != landsTenantDeniedReason || !strings.Contains(d.Message, "#support") {
		t.Fatalf("want a non-escalatable lands_tenant denial pointing at #support, got %+v", d)
	}
}

func TestLandsTenantSpawn_StewardSpawnsOnlyLandsGardenerInItsLand(t *testing.T) {
	f := newLandFixture(t)

	// Allowed: the steward puts a lands-gardener in its own land.
	if err := authorizeAgentSpawn(f.steward, f.ms, agentFields("plot-a", landsGardenerRole)); err != nil {
		t.Fatalf("steward spawning lands-gardener in its own land: %v", err)
	}
	for name, fields := range map[string]json.RawMessage{
		"another role":      agentFields("plot-a", "crew"),
		"the steward role":  agentFields("plot-a", landsStewardRole),
		"a comma list":      agentFields("plot-a", landsGardenerRole+",crew"),
		"a privileged role": agentFields("plot-a", "infra"),
		"another project":   agentFields("plot-b", landsGardenerRole),
		"no project":        agentFields("", landsGardenerRole),
	} {
		t.Run(name, func(t *testing.T) {
			requireTenantDenial(t, authorizeAgentSpawn(f.steward, f.ms, fields))
		})
	}
	// A lands-gardener spawns nothing, not even another lands-gardener.
	requireTenantDenial(t, authorizeAgentSpawn(f.gardener, f.ms, agentFields("plot-a", landsGardenerRole)))
	// Control: an agent outside the tier is unaffected.
	if err := authorizeAgentSpawn(f.crew, f.ms, agentFields("gardenio", "crew")); err != nil {
		t.Fatalf("crew spawn must be unaffected: %v", err)
	}
}

// Re-roling an agent bead is the same gate (authorizeAgentRole action
// "update"): a tenant cannot move an agent out of lands-gardener.
func TestLandsTenantSpawn_ReRole(t *testing.T) {
	f := newLandFixture(t)
	requireTenantDenial(t, authorizeAgentRoleUpdate(f.steward, f.ms, f.gardenerAB, json.RawMessage(`{"role":"crew"}`)))
	requireTenantDenial(t, authorizeAgentRoleUpdate(f.gardener, f.ms, f.gardenerAB, json.RawMessage(`{"role":"steward"}`)))
	// Control: an update that leaves the role alone is not a spawn.
	if err := authorizeAgentRoleUpdate(f.gardener, f.ms, f.gardenerAB, json.RawMessage(`{"agent_state":"working"}`)); err != nil {
		t.Fatalf("unrelated update denied: %v", err)
	}
}

// The target role bead must itself be exactly lands-tenant: a lands-gardener
// role bead that is missing, untiered, privileged or ownership-bypassing
// refuses the steward's spawn.
func TestLandsTenantSpawn_TargetRoleBeadMustBeTenant(t *testing.T) {
	for name, fields := range map[string]string{
		"no tier":          `{"capability_tiers":[]}`,
		"privileged too":   `{"capability_tiers":["lands-tenant","privileged"]}`,
		"legacy flag":      `{"capability_tiers":["lands-tenant"],"privileged":true}`,
		"all-types grant":  `{"capability_tiers":["lands-tenant"],"ownership_grants":` + bypassRows + `}`,
		"narrow grant row": `{"capability_tiers":["lands-tenant"],"ownership_grants":[{"types":["task"],"label":"thread","actions":["update"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newLandFixture(t)
			makeTierRoleBead(f.ms, "gd-role-lg", landsGardenerRole, fields)
			requireTenantDenial(t, authorizeAgentSpawn(f.steward, f.ms, agentFields("plot-a", landsGardenerRole)))
		})
	}
	t.Run("missing", func(t *testing.T) {
		f := newLandFixture(t)
		delete(f.ms.beads, "gd-role-lg")
		requireTenantDenial(t, authorizeAgentSpawn(f.steward, f.ms, agentFields("plot-a", landsGardenerRole)))
	})
}

// The caller's tier is resolved from the agent bead the per-agent token names;
// when it cannot be, the spawn is refused.
func TestLandsTenantSpawn_CallerLookupFailsClosed(t *testing.T) {
	f := newLandFixture(t)
	if err := authorizeAgentSpawn(f.steward, roleListErrStore{f.ms}, agentFields("plot-a", landsGardenerRole)); err == nil {
		t.Fatal("a role lookup error must refuse the spawn")
	}
	ghost := withAuthMethod(agentFleetCtx("gd-gone", "ghost"), authMethodStatic)
	var d *AuthzDenial
	if err := authorizeAgentSpawn(ghost, f.ms, agentFields("gardenio", "crew")); !errors.As(err, &d) || d.Escalatable {
		t.Fatalf("a per-agent token with no agent bead must be refused, got %v", err)
	}
	// Control: the primary static token names no agent bead and is unaffected.
	if err := authorizeAgentSpawn(withAuthMethod(context.Background(), authMethodStatic), f.ms, agentFields("gardenio", "crew")); err != nil {
		t.Fatalf("static token spawn of crew: %v", err)
	}
}

// A tenant agent files no escalation: neither directly nor through an
// X-Escalate retry of an escalatable denial. A crew agent still may.
func TestLandsTenantEscalation_Create(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	makeTierRoleBead(ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	body := map[string]any{
		"title": "esc", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}},
	}

	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", landsStewardRole)}
	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil)
	requireStatus(t, rec, http.StatusForbidden)
	var d AuthzDenial
	decodeJSON(t, rec, &d)
	if d.DeniedReason != landsTenantDeniedReason || d.Escalatable {
		t.Fatalf("tenant escalation create: want a lands_tenant denial, got %+v", d)
	}

	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil), http.StatusCreated)
}

func TestLandsTenantEscalation_XEscalateRetryFilesNothing(t *testing.T) {
	ms := newMockStore()
	makeTierRoleBead(ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	ms.beads["gd-stew"] = &model.Bead{ID: "gd-stew", Type: "agent", Fields: agentFields("plot-a", landsStewardRole)}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})

	req := httptest.NewRequest("POST", "/v1/beads", nil).WithContext(agentFleetCtx("gd-stew", "steward-a"))
	req.Header.Set("X-Escalate", "true")
	rec := httptest.NewRecorder()
	denied := &AuthzDenial{Action: "create", BeadType: "task", Escalatable: true, Message: "denied"}
	if !srv.handleEscalation(rec, req, denied, json.RawMessage(`{}`), "") {
		t.Fatal("handleEscalation must answer a tenant's retry itself")
	}
	requireStatus(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), landsTenantDeniedReason) {
		t.Fatalf("want the lands_tenant denial, got %s", rec.Body.String())
	}
	for _, b := range ms.beads {
		if b.Type == model.TypeEscalation {
			t.Fatalf("no escalation may be filed for a tenant, found %s", b.ID)
		}
	}
	// The taint grant-request branch is behind the same check.
	rec = httptest.NewRecorder()
	denied.TaintNames = []string{"untrusted-input"}
	if !srv.handleEscalation(rec, req, denied, json.RawMessage(`{}`), "gd-x") || rec.Code != http.StatusForbidden {
		t.Fatalf("tenant grant-request retry: status %d, want 403", rec.Code)
	}
}

// A tenant agent rejects (and so approves) nothing: authorizeEscalationVerb
// refuses its approve before the handler acts. Since P7 Task 9 every agent's
// reject is refused earlier, by caller class, so a crew agent's is too; an
// operator's on the page session goes through.
func TestLandsTenantEscalation_RejectRefused(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	makeTierRoleBead(ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	ms.beads["gd-esc1"] = &model.Bead{ID: "gd-esc1", Type: model.TypeEscalation, Kind: "data", Title: "esc",
		Status: model.StatusOpen, Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"operation_type":"manual_operator_action","operation_spec":{"x":1}}`)}
	ms.labels["gd-esc1"] = []string{"project:gardenio"}

	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", landsStewardRole)}
	for _, verb := range []string{"reject", "approve"} {
		rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-esc1/"+verb, testAgentToken, map[string]any{"reason": "no"}, nil)
		requireStatus(t, rec, http.StatusForbidden)
		want := landsTenantDeniedReason
		if verb == "reject" {
			want = "caller class: agent"
		}
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("tenant %s: want %q in the denial, got %s", verb, want, rec.Body.String())
		}
	}
	if ms.beads["gd-esc1"].Status != model.StatusOpen {
		t.Fatal("a refused reject must leave the escalation open")
	}

	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-esc1/reject", testAgentToken, map[string]any{"reason": "no"}, nil), http.StatusForbidden)
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-esc1/reject", "owner@example.com", map[string]any{"reason": "no"}, nil), http.StatusOK)
}
