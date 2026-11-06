package main

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// plans_stepdata_typefilter_test.go — proves the /plans index step-data walk
// hydrates only step-relevant children (issue / phase / decision) when the
// reverse-dependency rows carry the child type, and stays behavior-identical to
// the pre-filter walk (ka-0000003113).
//
// A plan epic accrues many parent-child children that are NOT steps — its bound
// plan-gardener agent, session + message beads, docs, bundles. The walk below
// (renderedStepRollup / pickPlanCurrentOrNextStep / buildPlanGrid /
// countOpenDecisionChildren) discards every one of them, but the index used to
// hydrate them all as full bead records (fields + description). The daemon now
// stamps each child's type onto the reverse-dep row's BeadType, so hydration
// skips the known non-step children.

// seedTypedPlanFake builds one plan epic with a mix of children, each carrying a
// BeadType on its reverse-dep row: one open task step, one open decision, and
// three non-step children (agent, session, message). withTypes controls whether
// the reverse-dep rows carry BeadType (new daemon) or leave it empty (old
// daemon → degrade to full hydration).
func seedTypedPlanFake(withTypes bool) (*fakeDaemon, []*beadsapi.BeadDetail) {
	f := newFake()
	f.revDeps = map[string][]beadsapi.Dependency{}
	f.beads = map[string]*beadsapi.BeadDetail{}
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	const epicID = "gd-plan-typed"
	epic := &beadsapi.BeadDetail{
		ID:        epicID,
		Title:     "Typed plan",
		Kind:      "issue",
		Type:      "epic",
		Status:    "in_progress",
		Labels:    []string{"project:gardenio", "plan"},
		UpdatedAt: base,
	}
	step := &beadsapi.BeadDetail{
		ID: "gd-step", Title: "Do the thing", Kind: "issue", Type: "task",
		Status: "in_progress", Assignee: "gardenio-worker", UpdatedAt: base.Add(time.Minute),
	}
	decision := &beadsapi.BeadDetail{
		ID: "gd-dec", Title: "Pick a path", Type: "decision", Status: "open",
		Fields: map[string]string{"prompt": "which"}, CreatedAt: base, UpdatedAt: base,
	}
	// Non-step children the walk provably discards.
	agent := &beadsapi.BeadDetail{ID: "gd-agent", Type: "agent", Status: "open", UpdatedAt: base}
	session := &beadsapi.BeadDetail{ID: "gd-session", Type: "session", Status: "open", UpdatedAt: base}
	message := &beadsapi.BeadDetail{ID: "gd-message", Type: "message", Status: "open", UpdatedAt: base}

	for _, b := range []*beadsapi.BeadDetail{epic, step, decision, agent, session, message} {
		f.beads[b.ID] = b
	}
	mk := func(id, typ string) beadsapi.Dependency {
		d := beadsapi.Dependency{BeadID: id, DependsOnID: epicID, Type: "parent-child"}
		if withTypes {
			d.BeadType = typ
		}
		return d
	}
	f.revDeps[epicID] = []beadsapi.Dependency{
		mk("gd-step", "task"),
		mk("gd-dec", "decision"),
		mk("gd-agent", "agent"),
		mk("gd-session", "session"),
		mk("gd-message", "message"),
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{epic}, Total: 1}
	return f, []*beadsapi.BeadDetail{epic}
}

// TestPlanIndexStepData_SkipsNonStepChildrenWhenTyped asserts the non-step
// children (agent/session/message) are NOT hydrated when the reverse-dep rows
// carry their type, while the step + decision still are — and the derived
// step-data (current step, rollup, open-decision count) is unchanged.
func TestPlanIndexStepData_SkipsNonStepChildrenWhenTyped(t *testing.T) {
	f, epics := seedTypedPlanFake(true)
	s := testServerWithDaemon(f)

	got := s.planIndexStepData(context.Background(), epics)
	info := got["gd-plan-typed"]
	if info == nil {
		t.Fatal("expected step-data entry for the plan")
	}

	// Step-relevant children hydrated; known non-step children skipped.
	hydrated := f.batchGetBeadsAllIDs
	for _, id := range []string{"gd-step", "gd-dec"} {
		if _, ok := hydrated[id]; !ok {
			t.Errorf("expected step-relevant child %s to be hydrated", id)
		}
	}
	for _, id := range []string{"gd-agent", "gd-session", "gd-message"} {
		if _, ok := hydrated[id]; ok {
			t.Errorf("non-step child %s should NOT have been hydrated", id)
		}
	}

	// Derived data is correct: one open step is the current/next, one total /
	// zero closed, one open decision.
	if info.NextStep == nil || info.NextStep.ID != "gd-step" {
		t.Errorf("NextStep = %+v, want gd-step", info.NextStep)
	}
	if info.StepsTotal != 1 || info.StepsClosed != 0 {
		t.Errorf("rollup = %d/%d, want 1/0", info.StepsClosed, info.StepsTotal)
	}
	if info.OpenDecisions != 1 {
		t.Errorf("OpenDecisions = %d, want 1", info.OpenDecisions)
	}
}

// TestPlanIndexStepData_TypeFilterIsBehaviorPreserving pins that the filtered
// walk (new daemon, typed rows) yields the SAME derived step-data as the
// unfiltered walk (old daemon, empty BeadType → every child hydrated). The
// filter must be a pure hydration optimization, never a semantic change.
func TestPlanIndexStepData_TypeFilterIsBehaviorPreserving(t *testing.T) {
	typedFake, typedEpics := seedTypedPlanFake(true)
	untypedFake, untypedEpics := seedTypedPlanFake(false)

	typed := testServerWithDaemon(typedFake).planIndexStepData(context.Background(), typedEpics)["gd-plan-typed"]
	untyped := testServerWithDaemon(untypedFake).planIndexStepData(context.Background(), untypedEpics)["gd-plan-typed"]

	if typed == nil || untyped == nil {
		t.Fatal("expected step-data entries from both walks")
	}
	// Old daemon (no types) hydrates ALL children incl. the non-step ones.
	for _, id := range []string{"gd-agent", "gd-session", "gd-message"} {
		if _, ok := untypedFake.batchGetBeadsAllIDs[id]; !ok {
			t.Errorf("degrade path should hydrate %s (unknown type)", id)
		}
	}
	// Same derived step-data either way.
	if (typed.NextStep == nil) != (untyped.NextStep == nil) {
		t.Fatalf("NextStep presence diverged: typed=%v untyped=%v", typed.NextStep, untyped.NextStep)
	}
	if typed.NextStep != nil && typed.NextStep.ID != untyped.NextStep.ID {
		t.Errorf("NextStep diverged: %s vs %s", typed.NextStep.ID, untyped.NextStep.ID)
	}
	if typed.StepsTotal != untyped.StepsTotal || typed.StepsClosed != untyped.StepsClosed {
		t.Errorf("rollup diverged: typed=%d/%d untyped=%d/%d",
			typed.StepsClosed, typed.StepsTotal, untyped.StepsClosed, untyped.StepsTotal)
	}
	if typed.OpenDecisions != untyped.OpenDecisions {
		t.Errorf("OpenDecisions diverged: typed=%d untyped=%d", typed.OpenDecisions, untyped.OpenDecisions)
	}
}
