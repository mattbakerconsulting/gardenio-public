package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// The beads-viewer's plan answer reactor (gd-Dl88Htipuq) applies the owner's
// answer to a plan decision as the viewer's machine principal. Its
// plan_scope capability reaches the plan's own beads and the plan's
// gardener, and nothing else.

func planReactorCtx() context.Context {
	return machineCtx("gardenio-beads-viewer", "gardenio-operator", "gardenio-machine-plan-reactor")
}

func TestPlanReactor_PlanScope(t *testing.T) {
	s, ms, _ := newTestServer()
	planScopeFixture(ms)
	ms.beads["gd-loose"] = &model.Bead{ID: "gd-loose", Kind: model.KindIssue, Type: model.TypeTask, Status: model.StatusOpen, CreatedBy: "owner@example.com"}
	ms.labels["gd-loose"] = []string{"project:gardenio"}
	ms.beads["gd-epic-trig"] = &model.Bead{
		ID: "gd-epic-trig", Kind: model.KindData, Type: "trigger", Status: model.StatusOpen,
		CreatedBy: "owner@example.com", Fields: json.RawMessage(`{"role":"gardener","enabled":false}`),
	}
	ms.labels["gd-epic-trig"] = []string{"project:gardenio", "role:gardener", gardenerEpicLabelPrefix + "gd-loose"}

	reactor := planReactorCtx()
	for _, c := range []struct {
		bead, action string
		want         bool
	}{
		{"gd-plan", "update", true},
		{"gd-phase", "close", true},
		{"gd-step", "update", true},
		{"gd-step", "close", true},
		{"gd-doc", "close", true},
		{"gd-step2", "close", true},
		{"gd-trig", "update", true},
		{"gd-trig", "close", false},       // the gardener is updated (enabled), never closed
		{"gd-step", "delete", false},      // never delete
		{"gd-loose", "close", false},      // a task in no plan
		{"gd-epic-trig", "update", false}, // a gardener pinned to an epic that is not a plan
	} {
		bead := ms.beads[c.bead]
		bead.Labels = ms.labels[c.bead]
		got := s.authorizeOwnership(reactor, c.action, bead) == nil
		if got != c.want {
			t.Errorf("plan reactor %s %s: admitted=%v, want %v", c.action, c.bead, got, c.want)
		}
	}

	// An escalation under the plan is not one of the plan's own beads (it has
	// the shared-queue carve-out of its own).
	esc := ms.beads["gd-esc"]
	esc.Labels = ms.labels["gd-esc"]
	if _, ok := s.authorizePlanReactor(reactor, "close", esc); ok {
		t.Error("the plan-scope rule must not admit an escalation under the plan")
	}

	// The same machine client without the plan-reactor role, and another
	// machine role, get the ordinary ownership denial.
	for name, ctx := range map[string]context.Context{
		"viewer without the role": machineCtx("gardenio-beads-viewer", "gardenio-operator"),
		"accounts":                machineCtx("gardenio-controller", "gardenio-machine-accounts"),
	} {
		step := ms.beads["gd-step"]
		step.Labels = ms.labels["gd-step"]
		if s.authorizeOwnership(ctx, "close", step) == nil {
			t.Errorf("%s: closing a plan step must be denied", name)
		}
	}
}

// The reactor turns on the paused gardener of a plan the owner approved, for
// whichever agent filed the decision, and the server seals it to the approver.
func TestPlanReactor_EnableSealedFromApproval(t *testing.T) {
	paused := `{"action":"spawn_agent","role":"plan-gardener","project":"gardenio","enabled":false}`
	enable := updateBeadInput{Fields: json.RawMessage(`{"enabled":true}`)}

	f := newPlanApprovalFixture(t)
	f.bind(ownerAgentCtx("gd-crew2", "gardenio-crew-other"), "gd-trig", paused, planGardenerLabels(approvalPlan))
	if _, err := f.srv.updateBead(planReactorCtx(), "gd-trig", enable); err != nil {
		t.Fatal(err)
	}
	if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != lineageAdmin {
		t.Fatalf("the reactor's enable must seal the gardener to the approver, got %q", got)
	}
	if err := f.engineFire("gd-trig"); err != nil {
		t.Fatalf("sealed gardener must spawn, got %v", err)
	}

	for name, c := range map[string]struct {
		mutate map[string]any
		ctx    context.Context
	}{
		"answered by a non-admin":          {mutate: map[string]any{decisionResolverAdminField: false}},
		"the decision asked about another": {mutate: map[string]any{"requesting_task": "gd-other"}},
		"the owner chose drop":             {mutate: map[string]any{"chosen": "drop"}},
		"another machine role enables it":  {ctx: machineCtx("gardenio-trigger-engine", "gardenio-machine-trigger-ops")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPlanApprovalFixture(t)
			if c.mutate != nil {
				delete(f.ms.deps, approvalPlan)
				f.decision("gd-dec2", f.now.Add(-time.Minute), c.mutate)
			}
			f.bind(ownerAgentCtx("gd-crew2", "gardenio-crew-other"), "gd-trig", paused, planGardenerLabels(approvalPlan))
			ctx := c.ctx
			if ctx == nil {
				ctx = planReactorCtx()
			}
			_, _ = f.srv.updateBead(ctx, "gd-trig", enable)
			if got := sealedSpawnAuthor(f.ms.beads["gd-trig"]); got != "" {
				t.Fatalf("sealed to %q, want unsealed", got)
			}
		})
	}
}
