package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The home garden's steward is not held to the lands-tenant table (owner,
// 11-02): it fires and rebinds its plans' gardeners (role plan-gardener) and
// files escalations, though it shares the steward role bead with tenant
// stewards. Every other project, and every unreadable one, keeps the table.

func homeProjectBead(ms *mockStore, name, fields, operatedBy string) {
	ms.beads["gd-proj-"+name] = &model.Bead{
		ID: "gd-proj-" + name, Title: name, Type: "project", Status: model.StatusOpen,
		Fields: json.RawMessage(fields), OperatedBy: operatedBy,
	}
}

const homeFields = `{"land_default_channel":"#general"}`

func TestHomeSteward_SpawnsPlanGardener(t *testing.T) {
	f := newLandFixture(t)
	makeRoleBead(f.ms, "gd-role-pg", "plan-gardener", false)
	f.ms.beads["gd-stew"] = &model.Bead{ID: "gd-stew", Type: "agent", Fields: agentFields("gardenio", landsStewardRole)}

	// Control: with no home project bead the steward is a tenant and refused.
	requireTenantDenial(t, authorizeAgentSpawn(f.steward, f.ms, agentFields("gardenio", "plan-gardener")))

	homeProjectBead(f.ms, "gardenio", homeFields, "oidc:owner@example.com")
	if err := authorizeAgentSpawn(f.steward, f.ms, agentFields("gardenio", "plan-gardener")); err != nil {
		t.Fatalf("home steward spawning plan-gardener: %v", err)
	}
	// Its notes are still the steward's.
	caller, err := landsTenantCallerFrom(f.steward, f.ms)
	if err != nil || caller == nil || !caller.Home || !caller.isSteward() || caller.Project != "gardenio" {
		t.Fatalf("home steward caller: %+v, %v", caller, err)
	}
	// A lands-gardener in the same project is still a tenant.
	f.ms.beads["gd-lg"] = &model.Bead{ID: "gd-lg", Type: "agent", Fields: agentFields("gardenio", landsGardenerRole)}
	requireTenantDenial(t, authorizeAgentSpawn(f.gardener, f.ms, agentFields("gardenio", "plan-gardener")))
}

func TestHomeSteward_OnlyTheHomeGarden(t *testing.T) {
	for name, tc := range map[string]struct{ fields, op, role string }{
		"tenant land":       {`{"land_kind":"tenant","land_default_channel":"#general"}`, "oidc:owner@example.com", landsStewardRole},
		"no channel":        {`{}`, "oidc:owner@example.com", landsStewardRole},
		"machine operator":  {homeFields, "machine:provisioner", landsStewardRole},
		"no operator":       {homeFields, "", landsStewardRole},
		"steward plus crew": {homeFields, "oidc:owner@example.com", landsStewardRole + ",crew"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLandFixture(t)
			makeRoleBead(f.ms, "gd-role-pg", "plan-gardener", false)
			f.ms.beads["gd-stew"] = &model.Bead{ID: "gd-stew", Type: "agent", Fields: agentFields("plot-a", tc.role)}
			homeProjectBead(f.ms, "plot-a", tc.fields, tc.op)
			requireTenantDenial(t, authorizeAgentSpawn(f.steward, f.ms, agentFields("plot-a", "plan-gardener")))
		})
	}
}

func TestHomeSteward_FilesEscalations(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	makeTierRoleBead(ms, "gd-role-steward", landsStewardRole, `{"capability_tiers":["lands-tenant"]}`)
	body := map[string]any{
		"title": "esc", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}},
	}
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", landsStewardRole)}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil), http.StatusForbidden)

	homeProjectBead(ms, "gardenio", homeFields, "oidc:owner@example.com")
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, nil), http.StatusCreated)
}
