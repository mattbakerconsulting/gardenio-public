package main

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakePlanDecisionReader is an in-memory planDecisionMemberReader: a bead store
// plus a forward parent-child edge map (decision id → parent ids). It exercises
// requirePlanDecisionMember with NO HTTP round-trip.
type fakePlanDecisionReader struct {
	beads map[string]*beadsapi.BeadDetail
	deps  map[string][]beadsapi.Dependency // bead id → its forward dependencies
	err   error                            // non-nil to force GetBead/GetDependencies to fail
}

func (f *fakePlanDecisionReader) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.beads[id], nil
}

func (f *fakePlanDecisionReader) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.deps[id], nil
}

// member builds a reader where decisionID is an open decision parented onto
// planID (the happy path the resolve/dismiss verbs require).
func memberReader(planID, decisionID string) *fakePlanDecisionReader {
	return &fakePlanDecisionReader{
		beads: map[string]*beadsapi.BeadDetail{
			decisionID: {ID: decisionID, Type: "decision", Status: "open"},
			planID:     {ID: planID, Type: "epic", Status: "open"},
		},
		deps: map[string][]beadsapi.Dependency{
			decisionID: {{BeadID: decisionID, DependsOnID: planID, Type: "parent-child"}},
		},
	}
}

func TestRequirePlanDecisionMember_HappyPath(t *testing.T) {
	r := memberReader("gd-PLAN", "gd-DEC")
	if err := requirePlanDecisionMember(context.Background(), r, "gd-PLAN", "gd-DEC"); err != nil {
		t.Fatalf("requirePlanDecisionMember = %v, want nil", err)
	}
}

func TestRequirePlanDecisionMember_NotFound(t *testing.T) {
	r := &fakePlanDecisionReader{beads: map[string]*beadsapi.BeadDetail{}}
	err := requirePlanDecisionMember(context.Background(), r, "gd-PLAN", "gd-DEC")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want 'not found'", err)
	}
}

func TestRequirePlanDecisionMember_NotADecision(t *testing.T) {
	r := &fakePlanDecisionReader{beads: map[string]*beadsapi.BeadDetail{
		"gd-DEC": {ID: "gd-DEC", Type: "task", Status: "open"},
	}}
	err := requirePlanDecisionMember(context.Background(), r, "gd-PLAN", "gd-DEC")
	if err == nil || !strings.Contains(err.Error(), "not a decision") {
		t.Fatalf("err = %v, want 'not a decision'", err)
	}
}

func TestRequirePlanDecisionMember_AlreadyResolved(t *testing.T) {
	r := memberReader("gd-PLAN", "gd-DEC")
	r.beads["gd-DEC"].Status = "closed"
	err := requirePlanDecisionMember(context.Background(), r, "gd-PLAN", "gd-DEC")
	if err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("err = %v, want 'already resolved'", err)
	}
}

// A decision under a DIFFERENT plan must be rejected — the exact scoping guard
// that keeps `gb plan answer gd-planA gd-decUnderPlanB` from acting cross-plan
// (mirrors the viewer's loadPlanDecision membership check).
func TestRequirePlanDecisionMember_WrongPlan(t *testing.T) {
	r := memberReader("gd-OTHER", "gd-DEC") // parented onto gd-OTHER, not gd-PLAN
	r.beads["gd-PLAN"] = &beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open"}
	err := requirePlanDecisionMember(context.Background(), r, "gd-PLAN", "gd-DEC")
	if err == nil || !strings.Contains(err.Error(), "not a clarifying-question of plan") {
		t.Fatalf("err = %v, want cross-plan rejection", err)
	}
}

// A parent-child edge is required — a non-parent-child edge into the plan (e.g.
// a stray mention/related) must NOT count as membership.
func TestRequirePlanDecisionMember_NonParentChildEdgeRejected(t *testing.T) {
	r := memberReader("gd-PLAN", "gd-DEC")
	r.deps["gd-DEC"] = []beadsapi.Dependency{{BeadID: "gd-DEC", DependsOnID: "gd-PLAN", Type: "related"}}
	err := requirePlanDecisionMember(context.Background(), r, "gd-PLAN", "gd-DEC")
	if err == nil || !strings.Contains(err.Error(), "not a clarifying-question of plan") {
		t.Fatalf("err = %v, want membership rejection for non-parent-child edge", err)
	}
}
