package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// planStepAgentsServer builds a *Server over the fake with the wall clock pinned
// so agent liveness (agent_state alive AND within agentStaleThreshold of now) is
// deterministic. Returned alongside the fake so a test can seed beads/revDeps.
func planStepAgentsServer(t *testing.T, f *fakeDaemon, now time.Time) *Server {
	t.Helper()
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return now })
	return srv
}

// TestEnrichStepAgents_PrefersLiveOverStaleScalar is the core regression for
// ka-0000000647: when a second (differently-named) agent picks up a step, `kd
// claim` refuses to overwrite the assignee scalar still pointing at the FIRST
// agent, so the /plans chip showed the stale (oldest) agent. enrichStepAgents
// must override the scalar with the agent resolved from the step's `assigned`
// reverse-dep edges — the LIVE one wins over a parked one regardless of the
// scalar or of UpdatedAt order.
func TestEnrichStepAgents_PrefersLiveOverStaleScalar(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()

	// The first agent — parked (done) and freshly updated. Despite being the
	// freshest, it must lose to the live one.
	f.beads["gd-agentOld"] = &beadsapi.BeadDetail{
		ID: "gd-agentOld", Type: "agent", Title: "agent-old",
		Fields:    map[string]string{"agent": "agent-old", "agent_state": "done"},
		UpdatedAt: now.Add(-1 * time.Minute),
	}
	// The current worker — live (working), updated a little earlier than the
	// parked one to prove liveness beats freshness.
	f.beads["gd-agentNew"] = &beadsapi.BeadDetail{
		ID: "gd-agentNew", Type: "agent", Title: "agent-new",
		Fields:    map[string]string{"agent": "agent-new", "agent_state": "working"},
		UpdatedAt: now.Add(-10 * time.Minute),
	}
	// Both agents carry an `assigned` edge to the step (agent -> step), the
	// edge SpawnAgent stamps. The step's scalar assignee is still the first.
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-step1": {
			{BeadID: "gd-agentOld", DependsOnID: "gd-step1", Type: "assigned"},
			{BeadID: "gd-agentNew", DependsOnID: "gd-step1", Type: "assigned"},
		},
	}

	srv := planStepAgentsServer(t, f, now)
	steps := []planStepView{{ID: "gd-step1", Assignee: "agent-old"}}
	srv.enrichStepAgents(context.Background(), steps)

	if steps[0].Assignee != "agent-new" {
		t.Errorf("step assignee = %q, want the live current worker %q (stale scalar was agent-old)",
			steps[0].Assignee, "agent-new")
	}
}

// TestEnrichStepAgents_FreshestWinsAmongParked proves the tie-break among
// same-liveness agents: when neither `assigned` agent is live, the one with the
// freshest UpdatedAt (the most recent worker) is displayed.
func TestEnrichStepAgents_FreshestWinsAmongParked(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.beads["gd-a1"] = &beadsapi.BeadDetail{
		ID: "gd-a1", Type: "agent", Title: "agent-first",
		Fields:    map[string]string{"agent": "agent-first", "agent_state": "done"},
		UpdatedAt: now.Add(-3 * time.Hour),
	}
	f.beads["gd-a2"] = &beadsapi.BeadDetail{
		ID: "gd-a2", Type: "agent", Title: "agent-second",
		Fields:    map[string]string{"agent": "agent-second", "agent_state": "done"},
		UpdatedAt: now.Add(-30 * time.Minute),
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-step1": {
			{BeadID: "gd-a1", DependsOnID: "gd-step1", Type: "assigned"},
			{BeadID: "gd-a2", DependsOnID: "gd-step1", Type: "assigned"},
		},
	}

	srv := planStepAgentsServer(t, f, now)
	steps := []planStepView{{ID: "gd-step1", Assignee: "agent-first"}}
	srv.enrichStepAgents(context.Background(), steps)

	if steps[0].Assignee != "agent-second" {
		t.Errorf("step assignee = %q, want freshest parked agent %q", steps[0].Assignee, "agent-second")
	}
}

// TestEnrichStepAgents_NoAgentEdgeKeepsScalar proves a step with no `assigned`
// agent edge — e.g. a human-assigned step — keeps its scalar assignee untouched.
func TestEnrichStepAgents_NoAgentEdgeKeepsScalar(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	// A parent-child edge exists but no `assigned` edge — must not override.
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-step1": {
			{BeadID: "gd-child", DependsOnID: "gd-step1", Type: "parent-child"},
		},
	}

	srv := planStepAgentsServer(t, f, now)
	steps := []planStepView{{ID: "gd-step1", Assignee: "matthew.baker"}}
	srv.enrichStepAgents(context.Background(), steps)

	if steps[0].Assignee != "matthew.baker" {
		t.Errorf("step assignee = %q, want the untouched scalar %q", steps[0].Assignee, "matthew.baker")
	}
}

// TestEnrichStepAgents_NonAgentBeadIgnored proves an `assigned` edge whose
// target bead is not type=agent is skipped (defensive: the edge type is reused
// elsewhere) — the scalar is kept when no agent-typed candidate exists.
func TestEnrichStepAgents_NonAgentBeadIgnored(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.beads["gd-notagent"] = &beadsapi.BeadDetail{
		ID: "gd-notagent", Type: "task", Title: "not an agent",
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-step1": {
			{BeadID: "gd-notagent", DependsOnID: "gd-step1", Type: "assigned"},
		},
	}

	srv := planStepAgentsServer(t, f, now)
	steps := []planStepView{{ID: "gd-step1", Assignee: "orig"}}
	srv.enrichStepAgents(context.Background(), steps)

	if steps[0].Assignee != "orig" {
		t.Errorf("step assignee = %q, want the untouched scalar %q (non-agent edge target)", steps[0].Assignee, "orig")
	}
}

// TestHandlePlanDetail_StepChipShowsCurrentAgent is the end-to-end regression
// for ka-0000000647 through the real /plans/{id} handler: a step whose scalar
// assignee is stuck on the FIRST (now parked) agent, but which a live second
// agent is currently working via an `assigned` edge, must render the LIVE
// agent's name in the build-step assignee chip — not the stale scalar.
func TestHandlePlanDetail_StepChipShowsCurrentAgent(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	// The seeded step's scalar assignee is stuck on the first agent (kd claim
	// won't overwrite it for the second, differently-named worker).
	f.beads["gd-step1"].Assignee = "gardenio-old-agent-aaa"

	// Two agents worked the step; the second is live.
	f.beads["gd-agentOld"] = &beadsapi.BeadDetail{
		ID: "gd-agentOld", Type: "agent", Title: "gardenio-old-agent-aaa",
		Fields:    map[string]string{"agent": "gardenio-old-agent-aaa", "agent_state": "done"},
		UpdatedAt: now.Add(-2 * time.Hour),
	}
	f.beads["gd-agentNew"] = &beadsapi.BeadDetail{
		ID: "gd-agentNew", Type: "agent", Title: "gardenio-new-agent-bbb",
		Fields:    map[string]string{"agent": "gardenio-new-agent-bbb", "agent_state": "working"},
		UpdatedAt: now.Add(-2 * time.Minute),
	}
	// The step's reverse deps carry both `assigned` edges (agent -> step).
	f.revDeps["gd-step1"] = []beadsapi.Dependency{
		{BeadID: "gd-agentOld", DependsOnID: "gd-step1", Type: "assigned"},
		{BeadID: "gd-agentNew", DependsOnID: "gd-step1", Type: "assigned"},
	}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetNow(func() time.Time { return now })
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Scope the assertion to the step TIMELINE section (#plan-steps … #plan-crew):
	// the contract here is the build-step assignee CHIP, not the whole page. The
	// crew roster (#plan-crew, ka-0000000150) legitimately lists every agent that
	// worked a step — including the finished gd-agentOld — so a whole-body
	// strings.Contains would spuriously trip on that roster row.
	stepSection := planStepsSection(w.Body.String())
	if !strings.Contains(stepSection, "gardenio-new-agent-bbb") {
		t.Error("build-step chip should show the live current agent gardenio-new-agent-bbb")
	}
	if strings.Contains(stepSection, "gardenio-old-agent-aaa") {
		t.Error("build-step chip must NOT show the stale scalar agent gardenio-old-agent-aaa")
	}
}

// planStepsSection slices the rendered plan-detail body down to the step
// timeline section — from the #plan-steps anchor up to the #plan-crew roster
// that follows it — so a step-chip assertion is not confused by the crew roster
// (which lists finished agents by name too, ka-0000000150). Falls back to the
// whole body if either anchor is absent so the assertion still runs.
func planStepsSection(body string) string {
	start := strings.Index(body, `id="plan-steps"`)
	if start < 0 {
		return body
	}
	if end := strings.Index(body[start:], `id="plan-crew"`); end >= 0 {
		return body[start : start+end]
	}
	return body[start:]
}

// TestPickLiveOrFreshestAgent covers the choice primitive directly: live beats
// parked regardless of UpdatedAt; among same-liveness the freshest wins; nils
// degrade to the other side.
func TestPickLiveOrFreshestAgent(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	live := &beadsapi.BeadDetail{
		Type: "agent", Fields: map[string]string{"agent_state": "working"},
		UpdatedAt: now.Add(-10 * time.Minute), // live but older
	}
	parkedFresh := &beadsapi.BeadDetail{
		Type: "agent", Fields: map[string]string{"agent_state": "done"},
		UpdatedAt: now.Add(-1 * time.Minute), // parked but newer
	}
	if got := pickLiveOrFreshestAgent(parkedFresh, live, now); got != live {
		t.Error("live agent should beat a fresher parked one")
	}
	if got := pickLiveOrFreshestAgent(live, parkedFresh, now); got != live {
		t.Error("live agent should win regardless of argument order")
	}

	parkedOld := &beadsapi.BeadDetail{
		Type: "agent", Fields: map[string]string{"agent_state": "failed"},
		UpdatedAt: now.Add(-2 * time.Hour),
	}
	if got := pickLiveOrFreshestAgent(parkedOld, parkedFresh, now); got != parkedFresh {
		t.Error("among parked agents the freshest UpdatedAt should win")
	}

	if got := pickLiveOrFreshestAgent(nil, live, now); got != live {
		t.Error("nil current should yield the candidate")
	}
	if got := pickLiveOrFreshestAgent(live, nil, now); got != live {
		t.Error("nil candidate should keep the current")
	}
}
