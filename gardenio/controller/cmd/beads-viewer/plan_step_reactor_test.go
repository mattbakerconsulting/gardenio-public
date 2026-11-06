package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// reactorTestServer builds a full Server (so planNudgeLast + promptPlanAssistant
// are real) over a fake daemon + fake coopmux, with the clock pinned to
// planAssistantTestNow for deterministic liveness/debounce.
func reactorTestServer(f *fakeDaemon, cm screenClient) *Server {
	s := NewServer(f, slog.Default(), "", "", "", cm)
	s.SetNow(func() time.Time { return planAssistantTestNow })
	return s
}

// depAddedEvent builds a beads.dependency.added SSE event carrying a single
// edge, matching the daemon's events.DependencyAdded wire shape.
func depAddedEvent(t *testing.T, beadID, dependsOn, depType, operator string) beadsapi.SSEEvent {
	t.Helper()
	payload := map[string]any{
		"dependency": map[string]any{
			"bead_id":       beadID,
			"depends_on_id": dependsOn,
			"type":          depType,
			"operated_by":   operator,
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal dep event: %v", err)
	}
	return beadsapi.SSEEvent{Event: planStepReactorTopic, Data: data}
}

// --- reactToStepEdge unit tests ---------------------------------------------

// A flat step (task parented directly to a plan epic) with a LIVE assistant is
// nudged sub-second, and no extra bead is spawned. This is the core CLI-add
// case the seam restores.
func TestReactToStepEdge_FlatStepNudgesLiveAssistant(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	// The new step bead exists in the store (the daemon created it before the edge).
	f.beads["gd-newstep"] = &beadsapi.BeadDetail{
		ID: "gd-newstep", Type: "task", Status: "open", Title: "Ship the docs",
		Labels: []string{"project:gardenio"},
	}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "oidc:op@acme.example")

	if cm.nudgeCalls != 1 {
		t.Fatalf("expected exactly one nudge, got %d", cm.nudgeCalls)
	}
	if cm.nudgeLastID != "sess-gd-asstlive" {
		t.Errorf("nudge sessionID = %q, want sess-gd-asstlive", cm.nudgeLastID)
	}
	if got := cm.nudgeLastText; !contains(got, "Ship the docs") || !contains(got, "gd-newstep") {
		t.Errorf("nudge text = %q, want it to name the step title and bead id", got)
	}
	// A live nudge must not mint a fresh assistant bead.
	if f.createCalls != 0 {
		t.Errorf("live nudge must not spawn; got %d CreateBead calls", f.createCalls)
	}
}

// A step parented under a plan PHASE sub-epic is resolved up to the plan and
// nudges the same way as a flat step (phase membership rule).
func TestReactToStepEdge_PhasedStepResolvesToPlan(t *testing.T) {
	f := newFake()
	// Plan with one phase in phase_order.
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{
		ID: "gd-plan1", Type: "epic", Status: "in_progress",
		Labels: []string{"project:gardenio", "plan"},
		Fields: map[string]string{planPhaseFieldKey: `["gd-phaseI"]`},
	}
	// Phase sub-epic: a NON-plan epic whose parent-child parent is the plan.
	f.beads["gd-phaseI"] = &beadsapi.BeadDetail{
		ID: "gd-phaseI", Type: "epic", Status: "open",
		Labels: []string{"project:gardenio"},
	}
	f.deps["gd-phaseI"] = []beadsapi.Dependency{
		{BeadID: "gd-phaseI", DependsOnID: "gd-plan1", Type: "parent-child"},
	}
	f.beads["gd-pstep"] = &beadsapi.BeadDetail{
		ID: "gd-pstep", Type: "task", Status: "open", Title: "Bench it",
		Labels: []string{"project:gardenio"},
	}
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-pstep", "gd-phaseI", "oidc:op@acme.example")

	if cm.nudgeCalls != 1 {
		t.Fatalf("expected one nudge for the phased step, got %d", cm.nudgeCalls)
	}
	if !contains(cm.nudgeLastText, "Bench it") {
		t.Errorf("nudge text = %q, want the phased step title", cm.nudgeLastText)
	}
}

// A sub-epic that hangs under a plan but is NOT listed in the plan's
// phase_order is a nested sub-epic, not a phase — its task children are not
// plan steps, so no nudge fires. This locks the fail-closed membership guard so
// an unrelated hierarchy under a plan can never trigger a collaboration nudge.
func TestReactToStepEdge_NonPhaseSubEpicSkipped(t *testing.T) {
	f := newFake()
	// Plan whose phase_order does NOT list gd-sub.
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{
		ID: "gd-plan1", Type: "epic", Status: "in_progress",
		Labels: []string{"project:gardenio", "plan"},
		Fields: map[string]string{planPhaseFieldKey: `["gd-realphase"]`},
	}
	// gd-sub is an epic under the plan, but not a phase (absent from phase_order).
	f.beads["gd-sub"] = &beadsapi.BeadDetail{
		ID: "gd-sub", Type: "epic", Status: "open", Labels: []string{"project:gardenio"},
	}
	f.deps["gd-sub"] = []beadsapi.Dependency{
		{BeadID: "gd-sub", DependsOnID: "gd-plan1", Type: "parent-child"},
	}
	f.beads["gd-substep"] = &beadsapi.BeadDetail{ID: "gd-substep", Type: "task", Status: "open", Title: "x"}
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-substep", "gd-sub", "oidc:op@acme.example")

	if cm.nudgeCalls != 0 {
		t.Errorf("a step under a non-phase sub-epic must not nudge; got %d", cm.nudgeCalls)
	}
}

// An edge whose parent is NOT a plan (a plain epic with no plan label and no
// plan grandparent) fires no nudge — the fail-closed non-plan case.
func TestReactToStepEdge_NonPlanParentSkipped(t *testing.T) {
	f := newFake()
	f.beads["gd-epic"] = &beadsapi.BeadDetail{
		ID: "gd-epic", Type: "epic", Status: "open", Labels: []string{"project:gardenio"},
	}
	f.beads["gd-task"] = &beadsapi.BeadDetail{
		ID: "gd-task", Type: "task", Status: "open", Title: "unrelated",
	}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-task", "gd-epic", "oidc:op@acme.example")

	if cm.nudgeCalls != 0 {
		t.Errorf("a non-plan parent must not nudge; got %d", cm.nudgeCalls)
	}
	if f.createCalls != 0 {
		t.Errorf("a non-plan parent must not spawn; got %d CreateBead", f.createCalls)
	}
}

// An epic child parented to a plan is a PHASE being added, not a step — it must
// not nudge (the browser add-phase path never nudges either).
func TestReactToStepEdge_EpicChildIsPhaseNotStep(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	f.beads["gd-newphase"] = &beadsapi.BeadDetail{
		ID: "gd-newphase", Type: "epic", Status: "open", Title: "Phase II",
	}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-newphase", "gd-plan1", "oidc:op@acme.example")

	if cm.nudgeCalls != 0 {
		t.Errorf("an epic child (a phase add) must not nudge; got %d", cm.nudgeCalls)
	}
}

// FREEZE/THAW ECHO GUARD (bug ka-0000001684). A plan-gardener's OWN beads
// auto-parent under its plan epic — kbeads maybeAddAutoParent links the
// gardener's type=session registration bead to its task_id (the plan epic), and
// its minted type=message plan-transcript beads land the same edge. Those are
// NOT plan steps: the reactor must NOT nudge on them, or it wakes the just-parked
// gardener and re-arms the echo. A real task child under the SAME plan still
// nudges (asserted by TestReactToStepEdge_FlatStepNudgesLiveAssistant).
func TestReactToStepEdge_SessionAndMessageChildrenDoNotNudge(t *testing.T) {
	for _, stepType := range []string{"session", "message"} {
		t.Run(stepType, func(t *testing.T) {
			f := seedPlansFake()
			seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
			// A gardener's own bead (session registration / minted transcript) that
			// the daemon auto-parented onto the plan epic — a parent-child edge, but
			// not a work-item step.
			f.beads["gd-own"] = &beadsapi.BeadDetail{
				ID: "gd-own", Type: stepType, Status: "open", Title: "gardener's own bead",
			}
			cm := &fakeScreenClient{}
			s := reactorTestServer(f, cm)

			s.reactToStepEdge(context.Background(), "gd-own", "gd-plan1", "oidc:op@acme.example")

			if cm.nudgeCalls != 0 {
				t.Errorf("a type=%s child (gardener's own auto-parented bead) must not nudge; got %d", stepType, cm.nudgeCalls)
			}
			if f.createCalls != 0 {
				t.Errorf("a type=%s child must not spawn; got %d CreateBead", stepType, f.createCalls)
			}
		})
	}
}

// A step the bound gardener filed itself carries the operator it acts for, so
// only created_by tells it apart. It must not nudge or wake the gardener that
// just wrote it (gd-XCwA8MXX8m); another agent's step still does.
func TestReactToStepEdge_GardenersOwnStepDoesNotNudge(t *testing.T) {
	for _, tc := range []struct {
		createdBy string
		want      int
	}{
		{"gardenio-plan-asst-live", 0},
		{"gardenio-some-crew-abc", 1},
	} {
		t.Run(tc.createdBy, func(t *testing.T) {
			f := seedPlansFake()
			seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
			f.beads["gd-newstep"] = &beadsapi.BeadDetail{
				ID: "gd-newstep", Type: "task", Status: "open", Title: "Rotate the tokens",
				CreatedBy: tc.createdBy,
			}
			cm := &fakeScreenClient{}
			s := reactorTestServer(f, cm)

			s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "oidc:op@acme.example")

			if cm.nudgeCalls != tc.want || f.createCalls != 0 {
				t.Errorf("nudgeCalls = %d (want %d), createCalls = %d (want 0)", cm.nudgeCalls, tc.want, f.createCalls)
			}
		})
	}
}

// Adding a step to a CLOSED plan mints nothing and nudges nothing — the
// ka-0000000753 no-op-loop guard, mirrored from handlePlanAddStep.
func TestReactToStepEdge_ClosedPlanSkipped(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed"
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	f.beads["gd-newstep"] = &beadsapi.BeadDetail{
		ID: "gd-newstep", Type: "task", Status: "open", Title: "late step",
	}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "oidc:op@acme.example")

	if cm.nudgeCalls != 0 {
		t.Errorf("a closed plan must not nudge; got %d", cm.nudgeCalls)
	}
	if f.createCalls != 0 {
		t.Errorf("a closed plan must not spawn; got %d CreateBead", f.createCalls)
	}
}

// With NO assistant bound, a bare edge observation must NOT cold-spawn a
// gardener — the crucial guard that stops the reactor minting a spurious pod on
// every seed / `kd dep add` / formula-pour edge that lands under a plan.
// Cold-spawn parity for an agent-less plan remains the epic's tracked exit
// (ka-0000002170; needs the first-class add-step intent signal); this reactor
// only keeps the nudge firing for a plan an operator is already steering.
func TestReactToStepEdge_NoAgentDoesNotSpawn(t *testing.T) {
	f := seedPlansFake() // seeded plan, but NO plan-gardener agent bound
	f.beads["gd-newstep"] = &beadsapi.BeadDetail{
		ID: "gd-newstep", Type: "task", Status: "open", Title: "kick it off",
	}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "oidc:op@acme.example")

	if f.createCalls != 0 {
		t.Fatalf("a bare edge with no bound agent must NOT spawn; got %d CreateBead", f.createCalls)
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("no bound agent → no nudge; got %d", cm.nudgeCalls)
	}
}

// A plan with a PARKED (stopped) agent is WOKEN in place — the wake reuses the
// prior bead id (stable S3 session), never a fresh cold-mint. This proves the
// seam still delivers to a plan an operator was steering that has since parked.
func TestReactToStepEdge_WakesParkedAssistant(t *testing.T) {
	f := seedPlansFake()
	// A parked assistant: stale updatedAt + a terminal-ish state so assistantIsLive
	// is false but planAssistant still resolves it (the wake target).
	seedAssistantBead(f, "gd-asstpark", "gardenio-plan-asst-park", "gd-plan1", "stopping",
		planAssistantTestNow.Add(-2*time.Hour))
	f.beads["gd-newstep"] = &beadsapi.BeadDetail{
		ID: "gd-newstep", Type: "task", Status: "open", Title: "resume this",
	}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "oidc:op@acme.example")

	// Wake reuses the prior bead id, so it does NOT mint a new agent bead.
	if f.createCalls != 0 {
		t.Errorf("waking a parked agent must not mint a new bead; got %d CreateBead", f.createCalls)
	}
	// The wake flips the parked bead back to in_progress (UpdateBead), re-driving
	// its pod with the collaboration seed as its next prompt.
	if f.updateCalls == 0 {
		t.Errorf("expected the parked agent to be woken (UpdateBead), got %d update calls", f.updateCalls)
	}
}

// The per-plan debounce collapses a rapid burst of adds to one plan (from any
// writer — browser or daemon-direct CLI, all observed as edges here) into ONE
// nudge.
func TestReactToStepEdge_DebouncesBurst(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	f.beads["gd-s1"] = &beadsapi.BeadDetail{ID: "gd-s1", Type: "task", Status: "open", Title: "one"}
	f.beads["gd-s2"] = &beadsapi.BeadDetail{ID: "gd-s2", Type: "task", Status: "open", Title: "two"}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	// Clock is pinned, so both adds fall inside the same debounce window.
	s.reactToStepEdge(context.Background(), "gd-s1", "gd-plan1", "oidc:op@acme.example")
	s.reactToStepEdge(context.Background(), "gd-s2", "gd-plan1", "oidc:op@acme.example")

	if cm.nudgeCalls != 1 {
		t.Errorf("a burst within the debounce window must nudge once, got %d", cm.nudgeCalls)
	}
}

// The debounce is DURABLE (ka-0000002855): the first nudge stamps the window on
// the plan epic (fields.last_nudge_at), so a SECOND server with ZERO in-memory
// state — the post-restart / cross-process condition the volatile map could not
// survive — still respects the window and does not re-fire. This is the core
// property the task adds: a viewer restart (or a concurrent add from the CLI /
// controller reactor, each its own process) between two adds no longer
// double-nudges.
func TestReactToStepEdge_DebounceSurvivesRestart(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	f.beads["gd-s1"] = &beadsapi.BeadDetail{ID: "gd-s1", Type: "task", Status: "open", Title: "one"}
	f.beads["gd-s2"] = &beadsapi.BeadDetail{ID: "gd-s2", Type: "task", Status: "open", Title: "two"}

	// First add on the original server nudges and durably stamps last_nudge_at.
	cm1 := &fakeScreenClient{}
	s1 := reactorTestServer(f, cm1)
	s1.reactToStepEdge(context.Background(), "gd-s1", "gd-plan1", "oidc:op@acme.example")
	if cm1.nudgeCalls != 1 {
		t.Fatalf("first add must nudge once, got %d", cm1.nudgeCalls)
	}
	if got := f.beads["gd-plan1"].Fields[planLastNudgeField]; got == "" {
		t.Fatalf("first nudge must persist %s on the plan epic; got empty", planLastNudgeField)
	}

	// A FRESH server over the SAME daemon — no shared in-memory debounce state,
	// exactly like a restarted (or a second) viewer/reactor process. The pinned
	// clock keeps this add inside the window of the persisted stamp.
	cm2 := &fakeScreenClient{}
	s2 := reactorTestServer(f, cm2)
	s2.reactToStepEdge(context.Background(), "gd-s2", "gd-plan1", "oidc:op@acme.example")
	if cm2.nudgeCalls != 0 {
		t.Errorf("a fresh server within the persisted window must NOT re-nudge (durable debounce); got %d", cm2.nudgeCalls)
	}
}

// Concurrent adds on ONE server collapse to exactly one nudge decision: the
// per-server mutex serialises the read-modify-write and the GetBead is issued
// INSIDE the lock, so the second-through-Nth callers observe the first's
// just-persisted stamp (read-after-write) rather than the stale pre-stamp state.
// Run under -race, this also asserts the durable debounce has no data race.
func TestShouldNudgePlanAssistant_ConcurrentAddsFireOnce(t *testing.T) {
	f := seedPlansFake()
	s := reactorTestServer(f, &fakeScreenClient{})

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	fired := 0
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if s.shouldNudgePlanAssistant(context.Background(), "gd-plan1") {
				mu.Lock()
				fired++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if fired != 1 {
		t.Errorf("concurrent adds within the window must yield exactly one nudge decision, got %d", fired)
	}
	if f.beads["gd-plan1"].Fields[planLastNudgeField] == "" {
		t.Errorf("the winning add must persist %s on the plan epic", planLastNudgeField)
	}
}

// Once the persisted window has elapsed, a fresh server re-fires and re-stamps —
// the durable stamp does not wedge the plan's collaboration forever, it just
// bounds a burst.
func TestReactToStepEdge_DebounceExpiresAfterWindow(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	f.beads["gd-s1"] = &beadsapi.BeadDetail{ID: "gd-s1", Type: "task", Status: "open", Title: "one"}
	f.beads["gd-s2"] = &beadsapi.BeadDetail{ID: "gd-s2", Type: "task", Status: "open", Title: "two"}

	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)
	s.reactToStepEdge(context.Background(), "gd-s1", "gd-plan1", "oidc:op@acme.example")
	if cm.nudgeCalls != 1 {
		t.Fatalf("first add must nudge once, got %d", cm.nudgeCalls)
	}

	// Advance the clock past the debounce window; the next add is a fresh burst.
	later := planAssistantTestNow.Add(planAddStepNudgeWindow + time.Second)
	s.SetNow(func() time.Time { return later })
	s.reactToStepEdge(context.Background(), "gd-s2", "gd-plan1", "oidc:op@acme.example")
	if cm.nudgeCalls != 2 {
		t.Errorf("an add past the window must nudge again, got %d", cm.nudgeCalls)
	}
	if got := f.beads["gd-plan1"].Fields[planLastNudgeField]; got != later.UTC().Format(time.RFC3339) {
		t.Errorf("second nudge must re-stamp %s to %q; got %q",
			planLastNudgeField, later.UTC().Format(time.RFC3339), got)
	}
}

// An edge carrying no operator identity is skipped rather than firing a write
// that the daemon audit resolver would 400.
func TestReactToStepEdge_NoOperatorSkipped(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	f.beads["gd-newstep"] = &beadsapi.BeadDetail{ID: "gd-newstep", Type: "task", Status: "open", Title: "x"}
	cm := &fakeScreenClient{}
	s := reactorTestServer(f, cm)

	s.reactToStepEdge(context.Background(), "gd-newstep", "gd-plan1", "")

	if cm.nudgeCalls != 0 {
		t.Errorf("an edge with no operator must not nudge; got %d", cm.nudgeCalls)
	}
}

// --- reactor stream / dispatch tests ----------------------------------------

// recordingReactor is a stub planReactor that records the (bead,parent,op)
// tuples handed to it, so the stream/dispatch layer is testable without a
// server.
type recordingReactor struct {
	mu    sync.Mutex
	calls [][3]string
}

func (r *recordingReactor) reactToStepEdge(_ context.Context, beadID, parentID, operator string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [3]string{beadID, parentID, operator})
}

func (r *recordingReactor) snapshot() [][3]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][3]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// dispatch forwards a parent-child edge (with its operator) to the reactor and
// drops every non-parent-child edge and malformed payload.
func TestPlanStepReactor_DispatchFiltersEdges(t *testing.T) {
	rec := &recordingReactor{}
	p := NewPlanStepReactor(newFake(), rec, slog.Default())

	ctx := context.Background()
	// A parent-child edge → forwarded.
	p.dispatch(ctx, depAddedEvent(t, "gd-step", "gd-plan", "parent-child", "oidc:op@acme.example"))
	// A child-of alias → forwarded (isParentChildAlias).
	p.dispatch(ctx, depAddedEvent(t, "gd-step2", "gd-plan", "child-of", "oidc:op@acme.example"))
	// A non-parent-child edge → dropped.
	p.dispatch(ctx, depAddedEvent(t, "gd-a", "gd-b", "blocks", "oidc:op@acme.example"))
	p.dispatch(ctx, depAddedEvent(t, "gd-agent", "gd-plan", "assigned", "oidc:op@acme.example"))
	// Malformed payload → dropped, no panic.
	p.dispatch(ctx, beadsapi.SSEEvent{Event: planStepReactorTopic, Data: []byte("{not json")})
	// Empty ids → dropped.
	p.dispatch(ctx, depAddedEvent(t, "", "gd-plan", "parent-child", "oidc:op@acme.example"))

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 forwarded parent-child edges, got %d: %+v", len(got), got)
	}
	if got[0] != [3]string{"gd-step", "gd-plan", "oidc:op@acme.example"} {
		t.Errorf("first forwarded = %+v", got[0])
	}
	if got[1][0] != "gd-step2" {
		t.Errorf("second forwarded child = %q, want gd-step2", got[1][0])
	}
}

// dispatch falls back to created_by when operated_by is absent.
func TestPlanStepReactor_DispatchOperatorFallback(t *testing.T) {
	rec := &recordingReactor{}
	p := NewPlanStepReactor(newFake(), rec, slog.Default())

	data, _ := json.Marshal(map[string]any{
		"dependency": map[string]any{
			"bead_id":       "gd-step",
			"depends_on_id": "gd-plan",
			"type":          "parent-child",
			"created_by":    "gardenio-gardener",
		},
	})
	p.dispatch(context.Background(), beadsapi.SSEEvent{Event: planStepReactorTopic, Data: data})

	got := rec.snapshot()
	if len(got) != 1 || got[0][2] != "gardenio-gardener" {
		t.Fatalf("expected created_by fallback operator, got %+v", got)
	}
}

// The reactor pumps a live stream end-to-end: events pushed on the fake
// EventStream channel reach the reactor, and Close unwinds the loop.
func TestPlanStepReactor_StreamEndToEnd(t *testing.T) {
	f := newFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	rec := &recordingReactor{}
	p := NewPlanStepReactor(f, rec, slog.Default())
	p.Start()
	defer p.Close()

	ch <- depAddedEvent(t, "gd-step", "gd-plan", "parent-child", "oidc:op@acme.example")

	// Poll until the event is dispatched (the pump runs on its own goroutine).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.snapshot()) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := rec.snapshot()
	if len(got) != 1 || got[0][0] != "gd-step" {
		t.Fatalf("expected the streamed edge to reach the reactor, got %+v", got)
	}
	if f.eventStreamLastTopics != planStepReactorTopic {
		t.Errorf("reactor subscribed to %q, want %q", f.eventStreamLastTopics, planStepReactorTopic)
	}
}

// NewPlanStepReactor is nil-safe on a nil daemon or nil reactor, and every
// method tolerates a nil receiver.
func TestPlanStepReactor_NilSafe(t *testing.T) {
	if NewPlanStepReactor(nil, &recordingReactor{}, slog.Default()) != nil {
		t.Error("nil daemon should yield a nil reactor")
	}
	if NewPlanStepReactor(newFake(), nil, slog.Default()) != nil {
		t.Error("nil planReactor should yield a nil reactor")
	}
	var p *PlanStepReactor
	p.Start() // no panic
	p.Close() // no panic
}

// contains is a tiny alias so assertions read clearly.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
