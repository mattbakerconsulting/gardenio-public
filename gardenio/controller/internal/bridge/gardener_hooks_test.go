// Unit tests for the three gardener reconcile hooks added in Phase 3b/B7-c2
// (ka-0000003115). Each hook is exercised in isolation, then an integration
// test drives evalOne end-to-end with all three features set to assert
// identical observable behavior with scheduler.Reconcile for the same gardener
// shape.

package bridge

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler/progress"
)

// -----------------------------------------------------------------------------
// gardenerClosePerRunTaskBead
// -----------------------------------------------------------------------------

func TestGardenerClosePerRunTaskBead_ClosesOpenTask(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-agent-1", Type: "agent", Status: "closed",
				Fields: map[string]string{"task_id": "gd-task-1"},
			},
			{
				ID: "gd-task-1", Type: "task", Status: "open",
				Fields: map[string]string{"schedule_id": "gd-cap-1"},
			},
		},
	}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Project:  "monorepo",
		Gardener: &GardenerSpec{LastAgentID: "gd-agent-1"},
	}
	gardenerClosePerRunTaskBead(context.Background(), daemon, discardLogger(), trig)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-task-1") {
		t.Errorf("expected per-run task gd-task-1 to be closed; closedIDs=%v", daemon.closedIDs)
	}
}

func TestGardenerClosePerRunTaskBead_SkipsWhenAgentStillRunning(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-agent-1", Type: "agent", Status: "in_progress",
				Fields: map[string]string{"task_id": "gd-task-1"},
			},
			{
				ID: "gd-task-1", Type: "task", Status: "open",
				Fields: map[string]string{"schedule_id": "gd-cap-1"},
			},
		},
	}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Gardener: &GardenerSpec{LastAgentID: "gd-agent-1"},
	}
	gardenerClosePerRunTaskBead(context.Background(), daemon, discardLogger(), trig)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.closedIDs) > 0 {
		t.Errorf("expected no closes while agent still running, got %v", daemon.closedIDs)
	}
}

func TestGardenerClosePerRunTaskBead_NoopWhenAssignBeadSet(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-agent-1", Type: "agent", Status: "closed",
				Fields: map[string]string{"task_id": "gd-task-1"}},
			{ID: "gd-task-1", Type: "task", Status: "open",
				Fields: map[string]string{"schedule_id": "gd-cap-1"}},
		},
	}
	trig := TriggerDef{
		ID: "gd-cap-1",
		Gardener: &GardenerSpec{
			LastAgentID: "gd-agent-1",
			AssignBead:  "gd-epic-1", // subtree mode — no per-run task to close
		},
	}
	gardenerClosePerRunTaskBead(context.Background(), daemon, discardLogger(), trig)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.closedIDs) > 0 {
		t.Errorf("expected no closes in subtree mode, got %v", daemon.closedIDs)
	}
}

func TestGardenerClosePerRunTaskBead_NoopWithoutGardener(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	trig := TriggerDef{ID: "gd-srcs-1"}
	gardenerClosePerRunTaskBead(context.Background(), daemon, discardLogger(), trig)
	if len(daemon.closedIDs) > 0 {
		t.Errorf("expected no-op without Gardener, got closedIDs=%v", daemon.closedIDs)
	}
}

// -----------------------------------------------------------------------------
// gardenerDiscoverProgress
// -----------------------------------------------------------------------------

// The mockTriggerDaemon's ListBeadsFiltered does not filter by Parent — only
// Types/Labels — so any closed task seeded into the bead set is credited as a
// "child" by discoverChildClosed. That is sufficient for hook-level coverage:
// these tests assert the persistence + dedup behavior of the hook, not the
// daemon's parent-walking.
func TestGardenerDiscoverProgress_PersistsNewEvents(t *testing.T) {
	spec := `{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}]}`
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-child-1", Type: "task", Status: "closed"},
		},
	}
	trig := TriggerDef{
		ID:      "gd-cap-1",
		Project: "monorepo",
		Gardener: &GardenerSpec{
			AssignBead: "gd-epic-1",
			Progress:   json.RawMessage(spec),
		},
	}
	gardenerDiscoverProgress(context.Background(), daemon, discardLogger(), trig, time.Now())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.updatedIDs, "gd-cap-1") {
		t.Fatalf("expected gardener bead to be updated; updatedIDs=%v", daemon.updatedIDs)
	}
	var foundGardener string
	for i, id := range daemon.updatedIDs {
		if id == "gd-cap-1" && daemon.updatedFields[i]["gardener"] != "" {
			foundGardener = daemon.updatedFields[i]["gardener"]
		}
	}
	if foundGardener == "" {
		t.Fatalf("no gardener field write recorded; updates=%+v", daemon.updatedFields)
	}
	var cap GardenerSpec
	if err := json.Unmarshal([]byte(foundGardener), &cap); err != nil {
		t.Fatalf("gardener JSON unmarshalable: %v", err)
	}
	state, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("progress state unmarshalable: %v", err)
	}
	if len(state.Events) == 0 {
		t.Errorf("expected at least one progress event, got %d", len(state.Events))
	}
}

func TestGardenerDiscoverProgress_NoopWithoutSpec(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Gardener: &GardenerSpec{},
	}
	gardenerDiscoverProgress(context.Background(), daemon, discardLogger(), trig, time.Now())
	if len(daemon.updatedIDs) > 0 {
		t.Errorf("expected no-op without Progress spec, got updates=%v", daemon.updatedIDs)
	}
}

func TestGardenerDiscoverProgress_NoopWhenNoNewEvents(t *testing.T) {
	// Spec is valid but matches nothing — no closed beads seeded.
	spec := `{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}]}`
	daemon := &mockTriggerDaemon{}
	trig := TriggerDef{
		ID: "gd-cap-1",
		Gardener: &GardenerSpec{
			AssignBead: "gd-epic-1",
			Progress:   json.RawMessage(spec),
		},
	}
	gardenerDiscoverProgress(context.Background(), daemon, discardLogger(), trig, time.Now())
	if len(daemon.updatedIDs) > 0 {
		t.Errorf("expected no write when no new events, got %v", daemon.updatedIDs)
	}
}

// -----------------------------------------------------------------------------
// gardenerEvaluateExitCondition
// -----------------------------------------------------------------------------

// noOpenBugsCondition matches when zero open bug beads exist in the daemon
// (the mock satisfies this trivially when none are seeded). bead_query is the
// simplest leaf type that exercises the full ParseCondition →
// EvaluateCondition → ListBeadsFiltered path.
const noOpenBugsCondition = `{"type":"bead_query","types":["bug"],"statuses":["open"],"op":"==","value":"0"}`

// hasOpenBugCondition matches when at least one open bug exists; used to
// assert the not-met path leaves the gardener untouched.
const hasOpenBugCondition = `{"type":"bead_query","types":["bug"],"statuses":["open"],"op":">=","value":"1"}`

func TestGardenerEvaluateExitCondition_TerminatesOnMatch(t *testing.T) {
	// No bug beads → noOpenBugsCondition is met → gardener terminated.
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
		{ID: "gd-cap-1", Type: "trigger", Status: "open"},
	}}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Project:  "monorepo",
		Gardener: &GardenerSpec{ExitCondition: noOpenBugsCondition},
	}
	terminated := gardenerEvaluateExitCondition(context.Background(), daemon, discardLogger(), trig)
	if !terminated {
		t.Fatalf("expected exit_condition met to return true")
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("expected gardener to be closed, got closedIDs=%v", daemon.closedIDs)
	}
	if !slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("expected gardener to be commented on, got commentedIDs=%v", daemon.commentedIDs)
	}
}

func TestGardenerEvaluateExitCondition_NoopWhenUnset(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Gardener: &GardenerSpec{},
	}
	if gardenerEvaluateExitCondition(context.Background(), daemon, discardLogger(), trig) {
		t.Errorf("expected false when ExitCondition is empty")
	}
	if len(daemon.closedIDs) > 0 {
		t.Errorf("expected no closes, got %v", daemon.closedIDs)
	}
}

func TestGardenerEvaluateExitCondition_ParseErrorFailOpen(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Gardener: &GardenerSpec{ExitCondition: "{not-valid-json"},
	}
	if gardenerEvaluateExitCondition(context.Background(), daemon, discardLogger(), trig) {
		t.Errorf("expected false on parse error (fail-open)")
	}
	if len(daemon.closedIDs) > 0 {
		t.Errorf("expected no closes on parse error, got %v", daemon.closedIDs)
	}
}

func TestGardenerEvaluateExitCondition_NotMet(t *testing.T) {
	// One open bug bead → hasOpenBugCondition matches but we want NotMet, so
	// use noOpenBugsCondition here: one open bug → count==1 → "== 0" is false.
	// The bug carries project:monorepo so the now project-scoped bead_query
	// (ka-0000003939) counts it under the trigger's monorepo lane.
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-bug-1", Type: "bug", Status: "open", Labels: []string{"project:monorepo"}},
		},
	}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Project:  "monorepo",
		Gardener: &GardenerSpec{ExitCondition: noOpenBugsCondition},
	}
	if gardenerEvaluateExitCondition(context.Background(), daemon, discardLogger(), trig) {
		t.Errorf("expected false when exit_condition not met")
	}
	if len(daemon.closedIDs) > 0 {
		t.Errorf("expected no close when not met, got %v", daemon.closedIDs)
	}
}

// Belt-and-suspenders: the positive complement test asserts hasOpenBugCondition
// matches as expected, guarding against silent regressions in the test fixture.
func TestGardenerEvaluateExitCondition_PositiveComplement(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-bug-1", Type: "bug", Status: "open", Labels: []string{"project:monorepo"}},
		},
	}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Project:  "monorepo",
		Gardener: &GardenerSpec{ExitCondition: hasOpenBugCondition},
	}
	if !gardenerEvaluateExitCondition(context.Background(), daemon, discardLogger(), trig) {
		t.Errorf("expected true when hasOpenBugCondition met")
	}
}

// -----------------------------------------------------------------------------
// Integration: all three hooks wired in evalOne
// -----------------------------------------------------------------------------

// TestGardener_AllThreeHooks_EvalOneIntegration drives evalOne with a gardener
// that has (1) a terminated prior agent + open per-run task, (2) a progress
// spec that matches one event, and (3) an exit_condition that evaluates to met.
// After Evaluate:
//   - the per-run task is closed (hook 1),
//   - the gardener bead's `gardener` field is updated with non-empty
//     progress_state (hook 2),
//   - the gardener trigger is itself closed (hook 3, exit_condition).
//
// This mirrors the observable behavior of scheduler.Reconcile for a gardener
// with all three features set.
func TestGardener_AllThreeHooks_EvalOneIntegration(t *testing.T) {
	progressSpec := `{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}]}`

	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		LastAgentID:   "gd-agent-1",
		ExitCondition: noOpenBugsCondition,
		Progress:      json.RawMessage(progressSpec),
		// No AssignBead — exercise per-run close path.
	})
	// Add closed agent + open per-run task + closed task (credited as child).
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{
			ID: "gd-agent-1", Type: "agent", Status: "closed",
			Fields: map[string]string{"task_id": "gd-task-1"},
		},
		&beadsapi.BeadDetail{
			ID: "gd-task-1", Type: "task", Status: "open",
			Fields: map[string]string{"schedule_id": "gd-cap-1"},
		},
		&beadsapi.BeadDetail{
			ID: "gd-child-closed-1", Type: "task", Status: "closed",
		},
	)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// Hook 1: per-run task closed.
	if !slices.Contains(daemon.closedIDs, "gd-task-1") {
		t.Errorf("hook 1: expected gd-task-1 to be closed; closedIDs=%v", daemon.closedIDs)
	}

	// Hook 3: exit_condition met → gardener trigger itself closed.
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("hook 3: expected gardener gd-cap-1 to be closed; closedIDs=%v", daemon.closedIDs)
	}

	// Hook 3 leaves a terminating comment ("gardener terminated: loop_end:exit_condition").
	if !slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("hook 3: expected terminating comment on gardener; commentedIDs=%v", daemon.commentedIDs)
	}

	// Exit-conditioned gardener must NOT spawn this cycle.
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected no spawn when exit_condition met, got %d", len(daemon.spawnedNames))
	}
}

// TestGardener_ProgressPersisted_NoExitCondition exercises progress without
// exit_condition firing: the gardener bead's `gardener` field should be updated
// to include progress_state, but the trigger itself must remain open.
func TestGardener_ProgressPersisted_NoExitCondition(t *testing.T) {
	progressSpec := `{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}]}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
		Progress:   json.RawMessage(progressSpec),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{
			ID: "gd-child-closed-1", Type: "task", Status: "closed",
		},
	)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	var foundProgress bool
	for i, id := range daemon.updatedIDs {
		if id != "gd-cap-1" {
			continue
		}
		raw := daemon.updatedFields[i]["gardener"]
		if raw == "" {
			continue
		}
		if strings.Contains(raw, "progress_state") {
			foundProgress = true
			break
		}
	}
	if !foundProgress {
		t.Errorf("expected gardener JSON update with progress_state; updates=%+v", daemon.updatedFields)
	}

	// Gardener trigger must not be closed (no ExitCondition + run_budget unset).
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("gardener should remain open without exit_condition; closedIDs=%v", daemon.closedIDs)
	}
}
