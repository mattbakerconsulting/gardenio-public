package bridge

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// D7 e2e coverage for formula trigger lineage (task gd-C31t4gSKNc).
//
// Consolidates the three D7 lineage contracts (epic ka-0000000933):
//
//  1. Slack `/formula pour` from a thread → molecule has triggered-by
//     dep to the canonical thread bead. Covered by
//     TestE2E_FormulaPour_SlackSlash_MoleculeHasDepToThreadBead in
//     bot_formula_pour_e2e_test.go — referenced here for discoverability.
//  2. apply_formula trigger fired by an incoming bead → seed bead has
//     triggered-by deps to BOTH the trigger bead AND the originating
//     source bead. Covered by
//     TestE2E_ApplyFormulaTrigger_SeedBeadHasDepsToTriggerAndSourceBead
//     below — drives the full SSE → engine → action path.
//  3. `kd formula apply --trigger-bead gd-X` → molecule has triggered-by
//     dep to gd-X. The kbeads CLI side is covered by
//     TestTriggerAndSourceBeadFlags_WriteTriggeredByDeps in
//     kbeads/cmd/kd/formula_apply_test.go; the bridge-side equivalent is
//     covered by TestE2E_InstantiateFormulaSteps_TriggerBeadProducesMoleculeDep
//     below — drives the bridge's analog instantiateFormulaSteps with a
//     non-empty triggerBeadID and asserts the resulting dep edge.

// TestE2E_ApplyFormulaTrigger_SeedBeadHasDepsToTriggerAndSourceBead drives the
// full SSE-event → BeadStateTriggerSource → engine → actionApplyFormula path
// and asserts the D7 bullet-2 contract: the molecule (created directly by the
// trigger after D6/ka-0000006284 removed the pending_formula runner) carries
// `triggered-by` deps to BOTH the trigger-definition bead AND the originating
// source bead, with CreatedBy = "trigger:<trigger_id>".
//
// This is the e2e variant of
// TestBeadStateTriggerSource_ApplyFormulaWritesTriggeredByDeps in
// trigger_source_bead_test.go — same wire path, but lives in the *_e2e_test.go
// suite so the auto-link lineage contract is discoverable next to the other
// per-ingestion-path e2e tests (slack/webhook/jira/slack-mention).
func TestE2E_ApplyFormulaTrigger_SeedBeadHasDepsToTriggerAndSourceBead(t *testing.T) {
	const (
		triggerID = "gd-trig-applyformula-1"
		sourceID  = "gd-incoming-source-1"
		project   = "gardenio"
	)

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-applied", "BEAD")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	// Inject the SSE event for the incoming source bead.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID:     sourceID,
				Type:   "task",
				Title:  "Incoming source task",
				Status: "open",
				Labels: []string{"project:" + project},
			},
		},
	}
	src.mu.Unlock()

	// Define the apply_formula trigger bead.
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, triggerBeads(
		triggerBead(triggerID, "Apply formula on incoming", "trigger:bead", map[string]string{
			"action":       "apply_formula",
			"event_types":  "created",
			"conditions":   `[{"field":"type","op":"==","value":"task"}]`,
			"formula_id":   "gd-formula-applied",
			"formula_vars": `{"BEAD":"{{bead_id}}"}`,
			"project":      project,
		}),
	)...)
	daemon.mu.Unlock()

	// Drive the engine to drain the buffered event through
	// actionApplyFormula → writeTriggerLineage.
	engine.EvaluateSource(context.Background(), "bead")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// Molecule + at least one step bead.
	if len(daemon.createdBeads) < 2 {
		t.Fatalf("expected molecule + step beads, got %d", len(daemon.createdBeads))
	}
	if daemon.createdBeads[0].Type != "molecule" {
		t.Fatalf("first created bead type = %q, want molecule", daemon.createdBeads[0].Type)
	}

	// Both the trigger bead and the source bead must appear as triggered-by
	// targets, with CreatedBy = "trigger:<trigger_id>". Ignore parent-child
	// edges from steps → molecule.
	wantTargets := map[string]bool{
		triggerID: false,
		sourceID:  false,
	}
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			continue
		}
		if d.CreatedBy != "trigger:"+triggerID {
			t.Errorf("triggered-by dep CreatedBy = %q, want %q (dep=%+v)",
				d.CreatedBy, "trigger:"+triggerID, d)
		}
		if _, ok := wantTargets[d.DependsOnID]; !ok {
			t.Errorf("unexpected dep target %q (dep=%+v)", d.DependsOnID, d)
			continue
		}
		wantTargets[d.DependsOnID] = true
	}
	for target, seen := range wantTargets {
		if !seen {
			t.Errorf("missing triggered-by dep to %q (deps=%+v)", target, daemon.addedDeps)
		}
	}
}

// TestE2E_InstantiateFormulaSteps_TriggerBeadProducesMoleculeDep is the
// bridge-side e2e analog of `kd formula apply --trigger-bead gd-X` (D7
// bullet 3). The kbeads CLI populates the molecule's `trigger_bead_ids`
// field and writes the same `triggered-by` dep edge from molecule to the
// supplied trigger bead; the bridge-side path goes through
// instantiateFormulaSteps with a non-empty triggerBeadID.
//
// This test pins the dep-edge shape end-to-end for a multi-step formula:
//
//   - molecule → triggered-by → trigger bead (exactly one such edge)
//   - every step → parent-child → molecule
//   - no step double-parents to the trigger bead (D5 single-parent rule)
func TestE2E_InstantiateFormulaSteps_TriggerBeadProducesMoleculeDep(t *testing.T) {
	const triggerBeadID = "gd-formula-apply-trigger"

	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{
		ID:    "gd-formula-applied-multi",
		Title: "Formula applied via --trigger-bead",
	}
	steps := []formulaStep{
		{ID: "scan", Title: "Scan", Type: "task"},
		{ID: "patch", Title: "Patch", Type: "task"},
		{ID: "verify", Title: "Verify", Type: "task"},
	}

	molID, nSteps, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "gardenio", triggerBeadID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nSteps != len(steps) {
		t.Fatalf("expected %d steps created, got %d", len(steps), nSteps)
	}

	deps := daemon.getDeps()

	// Exactly one triggered-by dep, from molecule to the supplied trigger bead.
	var triggeredByDeps []depCall
	for _, d := range deps {
		if d.DepType == "triggered-by" {
			triggeredByDeps = append(triggeredByDeps, d)
		}
	}
	if len(triggeredByDeps) != 1 {
		t.Fatalf("expected exactly 1 triggered-by dep, got %d: %+v",
			len(triggeredByDeps), triggeredByDeps)
	}
	got := triggeredByDeps[0]
	if got.BeadID != molID {
		t.Errorf("triggered-by dep should originate at molecule %q, got %q", molID, got.BeadID)
	}
	if got.DependsOnID != triggerBeadID {
		t.Errorf("triggered-by dep should point at trigger bead %q, got %q",
			triggerBeadID, got.DependsOnID)
	}
	if got.CreatedBy != "slack-bridge" {
		t.Errorf("triggered-by dep CreatedBy = %q, want %q", got.CreatedBy, "slack-bridge")
	}

	// Every step has a parent-child dep to the molecule.
	stepParentChildCount := 0
	for _, d := range deps {
		if d.DepType != "parent-child" {
			continue
		}
		if d.DependsOnID != molID {
			t.Errorf("parent-child dep should point at molecule %s, got %+v", molID, d)
			continue
		}
		if d.BeadID == molID {
			t.Errorf("parent-child dep should originate at step, not molecule: %+v", d)
			continue
		}
		stepParentChildCount++
	}
	if stepParentChildCount != len(steps) {
		t.Errorf("expected %d step→molecule parent-child deps, got %d (deps=%+v)",
			len(steps), stepParentChildCount, deps)
	}

	// D5 single-parent: no step has its own triggered-by edge to the trigger
	// bead — lineage is transitive via the molecule.
	for _, d := range deps {
		if d.DepType == "triggered-by" && d.BeadID != molID {
			t.Errorf("step %s must not double-parent to trigger bead via triggered-by: %+v",
				d.BeadID, d)
		}
	}
}
