package bridge

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// End-to-end coverage for the pour-time driver-role fix (ka-0000005928, bug
// ka-0000001755). These drive the full SSE-event → BeadStateTriggerSource →
// engine → actionApplyFormula path and assert the ROLE the molecule driver is
// spawned with. Before the fix, an apply_formula trigger with no explicit
// trigger.Role spawned the driver as "crew", dropping the role the formula
// declared — the pour-time half of the §7→§8 role handoff.

// testFormulaBeadWithRoles builds a one-step formula bead carrying an explicit
// default_roles field, mirroring how the daemon renders a formula's role list
// into the map[string]string Fields projection (a JSON-array string).
func testFormulaBeadWithRoles(id, varName, defaultRolesJSON string) *beadsapi.BeadDetail {
	b := testFormulaBead(id, varName)
	if defaultRolesJSON != "" {
		b.Fields["default_roles"] = defaultRolesJSON
	}
	return b
}

// driveApplyFormulaTrigger fires a single created-bead event through an
// apply_formula trigger and returns the daemon that captured the spawn.
// triggerRole, when non-empty, is set as the trigger's own Role field.
func driveApplyFormulaTrigger(t *testing.T, formula *beadsapi.BeadDetail, triggerRole string) *mockTriggerDaemon {
	t.Helper()
	const (
		triggerID = "gd-trig-driverrole-1"
		sourceID  = "gd-incoming-driverrole-1"
		project   = "gardenio"
	)

	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{formula}}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	src.mu.Lock()
	src.pending = []beadStateEvent{{
		Topic: "beads.bead.created",
		Bead: &BeadEvent{
			ID:     sourceID,
			Type:   "task",
			Title:  "Incoming source task",
			Status: "open",
			Labels: []string{"project:" + project},
		},
	}}
	src.mu.Unlock()

	fields := map[string]string{
		"action":       "apply_formula",
		"event_types":  "created",
		"conditions":   `[{"field":"type","op":"==","value":"task"}]`,
		"formula_id":   formula.ID,
		"formula_vars": `{"BEAD":"{{bead_id}}"}`,
		"project":      project,
	}
	if triggerRole != "" {
		fields["role"] = triggerRole
	}

	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, triggerBeads(
		triggerBead(triggerID, "Apply formula on incoming", "trigger:bead", fields),
	)...)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")
	return daemon
}

func assertSingleSpawnRole(t *testing.T, daemon *mockTriggerDaemon, wantRole string) {
	t.Helper()
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedRoles) != 1 {
		t.Fatalf("expected exactly one driver spawn, got %d (roles=%v)",
			len(daemon.spawnedRoles), daemon.spawnedRoles)
	}
	if daemon.spawnedRoles[0] != wantRole {
		t.Errorf("driver spawned with role %q, want %q", daemon.spawnedRoles[0], wantRole)
	}
}

// TestE2E_ApplyFormula_DriverRole_FromDefaultRoles is the core regression: a
// formula declaring default_roles=[monorepo-fix] and a trigger with NO explicit
// role must spawn the driver as monorepo-fix, not the historical "crew".
func TestE2E_ApplyFormula_DriverRole_FromDefaultRoles(t *testing.T) {
	formula := testFormulaBeadWithRoles("gd-formula-fixproof", "BEAD", `["monorepo-fix"]`)
	daemon := driveApplyFormulaTrigger(t, formula, "")
	assertSingleSpawnRole(t, daemon, "monorepo-fix")
}

// TestE2E_ApplyFormula_DriverRole_TriggerRoleWins asserts an explicit
// trigger.Role still overrides the formula's declared role — the operator set
// it deliberately for this trigger.
func TestE2E_ApplyFormula_DriverRole_TriggerRoleWins(t *testing.T) {
	formula := testFormulaBeadWithRoles("gd-formula-fixproof", "BEAD", `["monorepo-fix"]`)
	daemon := driveApplyFormulaTrigger(t, formula, "monorepo-publish")
	assertSingleSpawnRole(t, daemon, "monorepo-publish")
}

// TestE2E_ApplyFormula_DriverRole_DefaultsToCrew asserts the historical default
// is preserved: a formula with no declared role and a trigger with no role
// still spawns a "crew" generalist driver — the seed-recipe / audit formulas
// that WANT a generalist are unaffected.
func TestE2E_ApplyFormula_DriverRole_DefaultsToCrew(t *testing.T) {
	formula := testFormulaBead("gd-formula-plain", "BEAD") // no default_roles, step 1 has no role
	daemon := driveApplyFormulaTrigger(t, formula, "")
	assertSingleSpawnRole(t, daemon, "crew")
}

// TestE2E_ApplyFormula_DriverRole_FromStep1Role asserts the step-1 fallback:
// a formula with no default_roles but whose step 1 declares a role drives the
// molecule with that role (the driver walks into step 1 first).
func TestE2E_ApplyFormula_DriverRole_FromStep1Role(t *testing.T) {
	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-step1role",
		Type:  "formula",
		Title: "Step-1 role formula",
		Fields: map[string]string{
			"vars":  `[{"name":"BEAD"}]`,
			"steps": `[{"id":"s1","title":"Do thing for {{BEAD}}","roles":["monorepo-repro"]}]`,
		},
	}
	daemon := driveApplyFormulaTrigger(t, formula, "")
	assertSingleSpawnRole(t, daemon, "monorepo-repro")
}
