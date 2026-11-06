package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// decodeFakeFields unmarshals the JSON fields blob of a captured CreateBead
// request into a string map for assertions.
func decodeFakeFields(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode create fields: %v", err)
	}
	return out
}

// planAssistantTestNow is a fixed clock so liveness (agent_state + recency) is
// deterministic. Assistants "updated" before now-agentStaleThreshold are parked.
var planAssistantTestNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

func planAssistantTestServer(f *fakeDaemon) *Server {
	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return planAssistantTestNow })
	return s
}

// seedPlanEpic seeds a plan epic into the fake.
func seedPlanEpic(f *fakeDaemon, id string) *beadsapi.BeadDetail {
	epic := &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Plan: " + id,
		Type:   "epic",
		Kind:   "issue",
		Status: "in_progress",
		Labels: []string{"project:gardenio", "plan"},
	}
	f.beads[id] = epic
	return epic
}

// seedAssistantBead seeds a plan-gardener agent bead bound to epicID both ways
// (assigned reverse-dep edge + fields.plan_id), with the given agent_state and
// updatedAt so liveness is controllable.
func seedAssistantBead(f *fakeDaemon, id, name, epicID, state string, updatedAt time.Time) *beadsapi.BeadDetail {
	return seedAssistantBeadWithRole(f, id, name, epicID, state, planGardenerRoleLabel, updatedAt)
}

// seedAssistantBeadWithRole is seedAssistantBead parameterized on the role
// label carried by the agent bead, so a fixture can seed a plan-gardener agent
// OR a legacy plan-assistant bead (to prove the removed alias no longer
// resolves — decision ka-0000007411). Everything else — the plan_id/task_id
// binding and the load-bearing assigned reverse-dep edge — is identical,
// matching how a plan-gardener iteration is bound in production.
func seedAssistantBeadWithRole(f *fakeDaemon, id, name, epicID, state, roleLabel string, updatedAt time.Time) *beadsapi.BeadDetail {
	a := &beadsapi.BeadDetail{
		ID:        id,
		Title:     name,
		Type:      "agent",
		Kind:      "config",
		Status:    "in_progress",
		Labels:    []string{"project:gardenio", roleLabel},
		Fields:    map[string]string{"agent_state": state, planIDField: epicID, "task_id": epicID, "last_session_id": "sess-" + id},
		UpdatedAt: updatedAt,
	}
	f.beads[id] = a
	if f.revDeps == nil {
		f.revDeps = map[string][]beadsapi.Dependency{}
	}
	f.revDeps[epicID] = append(f.revDeps[epicID], beadsapi.Dependency{BeadID: id, DependsOnID: epicID, Type: "assigned"})
	return a
}

// TestPlanAssistant_LookupByAssignedEdge finds the assistant via the assigned
// reverse-dep edge (path a).
func TestPlanAssistant_LookupByAssignedEdge(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	s := planAssistantTestServer(f)

	got := s.planAssistant(context.Background(), "gd-plan1")
	if got == nil {
		t.Fatal("expected to find the bound assistant via the assigned edge")
	}
	if got.ID != "gd-asst1" {
		t.Errorf("found %q, want gd-asst1", got.ID)
	}
}

// TestPlanAssistant_LookupByPlanIDField finds the assistant via the fields.plan_id
// binding (path b) even when the assigned edge is absent.
func TestPlanAssistant_LookupByPlanIDField(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	// Bead carries the role label + plan_id but NO assigned reverse-dep edge.
	f.beads["gd-asst2"] = &beadsapi.BeadDetail{
		ID:        "gd-asst2",
		Title:     "gardenio-plan-asst-2",
		Type:      "agent",
		Kind:      "config",
		Status:    "in_progress",
		Labels:    []string{"project:gardenio", planGardenerRoleLabel},
		Fields:    map[string]string{"agent_state": "working", planIDField: "gd-plan1", "task_id": "gd-plan1"},
		UpdatedAt: planAssistantTestNow,
	}
	s := planAssistantTestServer(f)

	got := s.planAssistant(context.Background(), "gd-plan1")
	if got == nil {
		t.Fatal("expected to find the bound assistant via the plan_id field")
	}
	if got.ID != "gd-asst2" {
		t.Errorf("found %q, want gd-asst2", got.ID)
	}
}

// TestPlanAssistant_NoneReturnsNil returns nil when the plan has no assistant.
func TestPlanAssistant_NoneReturnsNil(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	s := planAssistantTestServer(f)
	if got := s.planAssistant(context.Background(), "gd-plan1"); got != nil {
		t.Errorf("expected nil for a plan with no assistant, got %q", got.ID)
	}
}

// TestPlanAssistant_PrefersLiveOverParked returns the live assistant when a stale
// terminal one lingers beside it.
func TestPlanAssistant_PrefersLiveOverParked(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	// Parked (stale) assistant, updated 2h ago.
	seedAssistantBead(f, "gd-parked", "gardenio-plan-asst-old", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
	// Live assistant, fresh.
	seedAssistantBead(f, "gd-live", "gardenio-plan-asst-new", "gd-plan1", "working", planAssistantTestNow)
	s := planAssistantTestServer(f)

	got := s.planAssistant(context.Background(), "gd-plan1")
	if got == nil {
		t.Fatal("expected an assistant")
	}
	if got.ID != "gd-live" {
		t.Errorf("expected the LIVE assistant gd-live, got %q", got.ID)
	}
}

// TestAssistantIsLive covers the liveness predicate: alive state + recency.
func TestAssistantIsLive(t *testing.T) {
	s := planAssistantTestServer(newFake())
	cases := []struct {
		name    string
		state   string
		updated time.Time
		want    bool
	}{
		{"working-fresh", "working", planAssistantTestNow, true},
		{"spawning-fresh", "spawning", planAssistantTestNow, true},
		{"working-stale", "working", planAssistantTestNow.Add(-2 * time.Hour), false},
		{"stopping", "stopping", planAssistantTestNow, false},
		{"done", "done", planAssistantTestNow, false},
		{"no-timestamp", "working", time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": tc.state}, UpdatedAt: tc.updated}
			if got := s.assistantIsLive(b); got != tc.want {
				t.Errorf("assistantIsLive(%s,%v) = %v, want %v", tc.state, tc.updated, got, tc.want)
			}
		})
	}
}

// TestSpawnPlanAssistant_FreshStampsBindingAndEdge proves a first spawn stamps
// role=plan-gardener (the unified role of the ka-0000000081 merge),
// fields.plan_id, task_id, and the assigned edge.
func TestSpawnPlanAssistant_FreshStampsBindingAndEdge(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	s := planAssistantTestServer(f)

	id, name, err := s.spawnPlanAssistant(context.Background(), "op@acme.example", epic, "help me plan")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if id == "" || name == "" {
		t.Fatalf("spawn returned empty id/name: %q %q", id, name)
	}
	if f.createCalls != 1 {
		t.Fatalf("expected 1 CreateBead, got %d", f.createCalls)
	}
	// Fields on the created bead — fresh spawns mint the unified role name.
	fields := decodeFakeFields(t, f.createLastReq.Fields)
	if fields["role"] != planGardenerRole {
		t.Errorf("role = %q, want %q", fields["role"], planGardenerRole)
	}
	if fields[planIDField] != "gd-plan1" {
		t.Errorf("plan_id = %q, want gd-plan1", fields[planIDField])
	}
	if fields["task_id"] != "gd-plan1" {
		t.Errorf("task_id = %q, want gd-plan1", fields["task_id"])
	}
	if fields["prompt"] != "help me plan" {
		t.Errorf("prompt = %q, want 'help me plan'", fields["prompt"])
	}
	// A fresh spawn must NOT set session_resume.
	if fields["session_resume"] == "true" {
		t.Error("fresh spawn should not set session_resume=true")
	}
	// The load-bearing assigned edge.
	if f.addDepLastType != "assigned" || f.addDepLastDep != "gd-plan1" {
		t.Errorf("assigned edge = (%s -> %s), want assigned -> gd-plan1", f.addDepLastType, f.addDepLastDep)
	}
	// The role label — new spawns are stamped with the unified name.
	if f.addLabelLastVal != planGardenerRoleLabel {
		t.Errorf("last label = %q, want %q", f.addLabelLastVal, planGardenerRoleLabel)
	}
}

// TestSpawnPlanAssistant_RevivesSameBead proves a wake while a prior assistant
// exists REVIVES THE SAME bead in place — same id (stable S3 session-resume key),
// same name, session_resume=true, prompt refreshed, stop_requested cleared, and
// status flipped to in_progress (the AgentSpawn re-spawn trigger). It must NOT
// mint a new bead, because a new id would defeat S3 resume and cold-start the
// woken pod (bug ka-0000002360).
func TestSpawnPlanAssistant_RevivesSameBead(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	// A prior (parked) assistant already bound to the plan.
	seedAssistantBead(f, "gd-prior", "gardenio-plan-asst-prior", "gd-plan1", "stopping", planAssistantTestNow.Add(-30*time.Minute))
	s := planAssistantTestServer(f)

	id, name, err := s.spawnPlanAssistant(context.Background(), "op@acme.example", epic, "another question")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if id != "gd-prior" {
		t.Errorf("wake returned bead id %q, want the prior id gd-prior (stable id = stable S3 resume key)", id)
	}
	if name != "gardenio-plan-asst-prior" {
		t.Errorf("wake name = %q, want the prior name gardenio-plan-asst-prior (session continuity)", name)
	}
	// No new bead: the prior is revived in place.
	if f.createCalls != 0 {
		t.Errorf("wake must NOT CreateBead a new assistant; got %d", f.createCalls)
	}
	// ONE atomic write (ka-0000001583): the wake flips status AND refreshes the
	// wake fields in a SINGLE UpdateBead, NOT a separate fields-only write. A
	// fields-only write on a CLOSED prior bead would be stripped by the daemon's
	// dropTransientClosedFields guard, leaving agent_state="done" and looping the
	// reopen→reap cycle that made messaging a stopped gardener a no-op.
	if f.updateFieldsCalls != 0 {
		t.Errorf("wake must NOT use a separate UpdateBeadFieldsTyped write; got %d (status + fields must be one atomic UpdateBead)", f.updateFieldsCalls)
	}
	if f.updateLastID != "gd-prior" {
		t.Errorf("wake updated bead %q, want gd-prior", f.updateLastID)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "in_progress" {
		t.Errorf("wake should flip gd-prior to status=in_progress; got status=%v", f.updateLastReq.Status)
	}
	if f.updateLastReq.Fields["session_resume"] != "true" {
		t.Errorf("wake should set session_resume=true on the revived bead; got %v", f.updateLastReq.Fields["session_resume"])
	}
	if f.updateLastReq.Fields["prompt"] != "another question" {
		t.Errorf("wake should refresh prompt to the operator message; got %v", f.updateLastReq.Fields["prompt"])
	}
	if got, ok := f.updateLastReq.Fields["stop_requested"]; !ok || got != "" {
		t.Errorf("wake should clear stop_requested; got %v (present=%v)", got, ok)
	}
	// The wake must also DELETE agent_state (nil, not "") so a stale terminal
	// state (rate_limited / budget_exceeded / done) cannot intercept the
	// status→in_progress re-spawn in subscriber/sse.go (bug ka-0000004492) nor get
	// re-reaped as a podless_leak (ka-0000001583). agent_state is an enum with no
	// "" member, so the clear is a delete.
	if got, ok := f.updateLastReq.Fields["agent_state"]; !ok || got != nil {
		t.Errorf("wake should delete agent_state (nil); got %v (present=%v)", got, ok)
	}
}

// TestSpawnPlanAssistant_WakesRateLimited proves the specific failure Matt hit
// (bug ka-0000004492): an assistant whose prior session died as rate_limited is
// still revivable. The wake must clear the stale agent_state so the SSE watcher
// does not misread the status→in_progress update as a BudgetExceeded event and
// swallow the re-spawn.
func TestSpawnPlanAssistant_WakesRateLimited(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	// A prior assistant that died rate_limited (status stays in_progress; the
	// pod is gone). This is the exact live state of the dead bead in the report.
	seedAssistantBead(f, "gd-rl", "gardenio-plan-asst-rl", "gd-plan1", "rate_limited", planAssistantTestNow.Add(-15*time.Minute))
	s := planAssistantTestServer(f)

	id, _, err := s.spawnPlanAssistant(context.Background(), "op@acme.example", epic, "you back?")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if id != "gd-rl" {
		t.Errorf("wake returned bead id %q, want the prior id gd-rl (revive in place)", id)
	}
	if f.createCalls != 0 {
		t.Errorf("wake of a rate_limited assistant must NOT mint a new bead; got %d", f.createCalls)
	}
	// The wake DELETES agent_state (nil) in the SAME atomic write as the status
	// flip, so the single AgentSpawn event carries no terminal state and the
	// re-spawn is not swallowed as BudgetExceeded (ka-0000004492 / ka-0000001583).
	if got, ok := f.updateLastReq.Fields["agent_state"]; !ok || got != nil {
		t.Errorf("wake of a rate_limited assistant MUST delete agent_state (nil); got %v (present=%v)", got, ok)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "in_progress" {
		t.Errorf("wake should flip to status=in_progress; got %v", f.updateLastReq.Status)
	}
}

// TestSpawnPlanAssistant_ClosedPlanRefusesFreshSpawn proves the backstop guard
// (ka-0000000753): spawnPlanAssistant refuses to mint a FRESH gardener for a
// CLOSED plan, returning errPlanClosedNoWake and writing nothing. A closed plan
// is done; spawning a gardener for it only feeds the no-op respawn loop.
func TestSpawnPlanAssistant_ClosedPlanRefusesFreshSpawn(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	epic.Status = "closed"
	s := planAssistantTestServer(f)

	id, name, err := s.spawnPlanAssistant(context.Background(), "op@acme.example", epic, "collaborate")
	if !errors.Is(err, errPlanClosedNoWake) {
		t.Fatalf("spawn on a closed plan must return errPlanClosedNoWake; got id=%q name=%q err=%v", id, name, err)
	}
	if id != "" || name != "" {
		t.Errorf("refused spawn must return empty id/name; got id=%q name=%q", id, name)
	}
	if f.createCalls != 0 {
		t.Errorf("closed plan must not CreateBead a gardener; got %d", f.createCalls)
	}
}

// TestSpawnPlanAssistant_ClosedPlanRefusesWake proves the guard also blocks the
// WAKE path: a parked gardener bound to a CLOSED plan is NOT revived. The flip to
// status=in_progress is exactly what re-emits AgentSpawn and respawns the pod,
// so refusing it here is what actually stops the observed loop (ka-0000000753).
func TestSpawnPlanAssistant_ClosedPlanRefusesWake(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	epic.Status = "closed"
	// A parked gardener bound to the (now closed) plan — the exact live shape of
	// the looping bead in the report.
	seedAssistantBead(f, "gd-prior", "gardenio-plan-asst-prior", "gd-plan1", "idle", planAssistantTestNow.Add(-30*time.Minute))
	s := planAssistantTestServer(f)

	id, name, err := s.spawnPlanAssistant(context.Background(), "op@acme.example", epic, "still there?")
	if !errors.Is(err, errPlanClosedNoWake) {
		t.Fatalf("wake on a closed plan must return errPlanClosedNoWake; got id=%q name=%q err=%v", id, name, err)
	}
	// The guard runs BEFORE the wake, so no wake writes happen.
	if f.updateFieldsCalls != 0 {
		t.Errorf("closed plan must not refresh a gardener's wake fields; got %d UpdateBeadFields calls", f.updateFieldsCalls)
	}
	if f.updateCalls != 0 {
		t.Errorf("closed plan must not flip a gardener back to in_progress; got %d UpdateBead calls", f.updateCalls)
	}
	if f.createCalls != 0 {
		t.Errorf("closed plan wake must not mint a bead; got %d", f.createCalls)
	}
}

// TestAssistantLinkURL covers the three branches of assistantLinkURL — the
// coopmux-terminal preference, the /{id} bead-page fallback, and nil — that
// back the plan "assistant live / Open ->" affordance (ka-0000007019). The old
// target /agents/{id} 404'd; this helper never emits it.
func TestAssistantLinkURL(t *testing.T) {
	f := newFake()
	s := planAssistantTestServer(f)

	// nil assistant → empty, not external.
	if url, ext := s.assistantLinkURL(nil); url != "" || ext {
		t.Errorf("nil assistant: got (%q, %v), want (\"\", false)", url, ext)
	}

	// No coopmux configured (basePath empty) → bead-page fallback /{id}.
	bead := &beadsapi.BeadDetail{ID: "gd-asst", Notes: "pod_name: pod-xyz"}
	if url, ext := s.assistantLinkURL(bead); url != "/gd-asst" || ext {
		t.Errorf("no coopmux: got (%q, %v), want (\"/gd-asst\", false)", url, ext)
	}

	// Coopmux configured + pod_name present → coopmux terminal page, external.
	s.coopmuxPublicURL = "https://coopmux.example/mux"
	if url, ext := s.assistantLinkURL(bead); url != "https://coopmux.example/mux#pod-xyz" || !ext {
		t.Errorf("with coopmux+pod: got (%q, %v), want (\"https://coopmux.example/mux#pod-xyz\", true)", url, ext)
	}

	// Coopmux configured but no pod_name (parked/frozen, no live pod) → fallback.
	noPod := &beadsapi.BeadDetail{ID: "gd-parked"}
	if url, ext := s.assistantLinkURL(noPod); url != "/gd-parked" || ext {
		t.Errorf("coopmux but no pod_name: got (%q, %v), want (\"/gd-parked\", false)", url, ext)
	}
}

// TestPlanAssistant_ResolvesPlanGardenerOnly is the acceptance test for the
// dual-read REMOVAL (operator decision ka-0000007411 = remove): a role:plan-gardener
// agent bound to the plan resolves as its bound agent, and a LEGACY
// role:plan-assistant bead (the alias that was hard-removed) no longer resolves.
// Runs the assertion against both discovery paths: the assigned reverse-dep edge
// (path a) and the fields.plan_id role-scoped query (path b — the one that
// actually depends on the server-side role filter).
func TestPlanAssistant_ResolvesPlanGardenerOnly(t *testing.T) {
	t.Run("plan-gardener role/via assigned edge", func(t *testing.T) {
		f := newFake()
		seedPlanEpic(f, "gd-plan1")
		seedAssistantBeadWithRole(f, "gd-asst", "gardenio-plan-asst", "gd-plan1", "working", planGardenerRoleLabel, planAssistantTestNow)
		s := planAssistantTestServer(f)

		got := s.planAssistant(context.Background(), "gd-plan1")
		if got == nil {
			t.Fatal("a plan-gardener agent bound to the plan should resolve as its bound agent")
		}
		if got.ID != "gd-asst" {
			t.Errorf("found %q, want gd-asst", got.ID)
		}
	})
	t.Run("plan-gardener role/via plan_id field only", func(t *testing.T) {
		f := newFake()
		seedPlanEpic(f, "gd-plan1")
		// Role label + plan_id binding but NO assigned reverse-dep edge, so
		// resolution rides ONLY the fields.plan_id role-scoped query (path b).
		f.beads["gd-asst"] = &beadsapi.BeadDetail{
			ID:        "gd-asst",
			Title:     "gardenio-plan-asst",
			Type:      "agent",
			Kind:      "config",
			Status:    "in_progress",
			Labels:    []string{"project:gardenio", planGardenerRoleLabel},
			Fields:    map[string]string{"agent_state": "working", planIDField: "gd-plan1", "task_id": "gd-plan1"},
			UpdatedAt: planAssistantTestNow,
		}
		s := planAssistantTestServer(f)

		got := s.planAssistant(context.Background(), "gd-plan1")
		if got == nil {
			t.Fatal("a plan-gardener agent bound only by fields.plan_id should resolve via the role-scoped query")
		}
		if got.ID != "gd-asst" {
			t.Errorf("found %q, want gd-asst", got.ID)
		}
	})
	t.Run("legacy plan-assistant role no longer resolves", func(t *testing.T) {
		f := newFake()
		seedPlanEpic(f, "gd-plan1")
		// A pre-rename plan-assistant bead bound both ways. The alias was
		// removed, so resolution keys only on role:plan-gardener and this bead
		// must NOT resolve. isPlanAssistantBead rejects it even on the
		// assigned-edge candidate path.
		seedAssistantBeadWithRole(f, "gd-legacy", "gardenio-plan-asst-legacy", "gd-plan1", "working", "role:plan-assistant", planAssistantTestNow)
		s := planAssistantTestServer(f)

		if got := s.planAssistant(context.Background(), "gd-plan1"); got != nil {
			t.Errorf("a legacy plan-assistant bead must not resolve after the alias removal, got %q", got.ID)
		}
	})
}

// TestIsPlanAssistantBead_KeysOnPlanGardenerOnly pins the predicate: it accepts
// the role:plan-gardener label when the plan binding matches, and rejects the
// removed legacy role:plan-assistant alias, a missing role label, and a wrong
// plan binding.
func TestIsPlanAssistantBead_KeysOnPlanGardenerOnly(t *testing.T) {
	mk := func(roleLabel, planID string) *beadsapi.BeadDetail {
		labels := []string{"project:gardenio"}
		if roleLabel != "" {
			labels = append(labels, roleLabel)
		}
		return &beadsapi.BeadDetail{
			ID:     "gd-x",
			Type:   "agent",
			Labels: labels,
			Fields: map[string]string{planIDField: planID, "task_id": planID},
		}
	}
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		epic string
		want bool
	}{
		{"plan-gardener role, matching plan", mk(planGardenerRoleLabel, "gd-plan1"), "gd-plan1", true},
		{"plan-gardener role, wrong plan", mk(planGardenerRoleLabel, "gd-other"), "gd-plan1", false},
		{"legacy plan-assistant role (removed alias)", mk("role:plan-assistant", "gd-plan1"), "gd-plan1", false},
		{"no role label", mk("", "gd-plan1"), "gd-plan1", false},
		{"unrelated role label", mk("role:crew", "gd-plan1"), "gd-plan1", false},
		{"nil bead", nil, "gd-plan1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlanAssistantBead(tc.bead, tc.epic); got != tc.want {
				t.Errorf("isPlanAssistantBead(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestPlanAssistantBulk_KeysOnPlanGardenerOnly proves the /plans-index bulk join
// (S4) resolves plan-gardener agents and skips the removed legacy plan-assistant
// alias, from the single role-scoped list query.
func TestPlanAssistantBulk_KeysOnPlanGardenerOnly(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanEpic(f, "gd-plan2")
	seedAssistantBeadWithRole(f, "gd-legacy", "gardenio-plan-asst-legacy", "gd-plan1", "working", "role:plan-assistant", planAssistantTestNow)
	seedAssistantBeadWithRole(f, "gd-gardener", "gardenio-plan-asst-gardener", "gd-plan2", "working", planGardenerRoleLabel, planAssistantTestNow)
	s := planAssistantTestServer(f)

	out := s.planAssistantBulk(context.Background(), []string{"gd-plan1", "gd-plan2"})
	if got := out["gd-plan1"]; got != nil {
		t.Errorf("plan1 (legacy plan-assistant role) should not resolve after the alias removal, got %v", got)
	}
	if got := out["gd-plan2"]; got == nil || got.ID != "gd-gardener" {
		t.Errorf("plan2 (plan-gardener role) resolved %v, want gd-gardener", out["gd-plan2"])
	}
}
