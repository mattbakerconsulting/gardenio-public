package planagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
)

// testNow is a fixed clock so liveness (agent_state + recency) is deterministic.
// Agents "updated" before testNow-staleThreshold are parked.
var testNow = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

// fakeDaemon is a minimal in-memory DaemonClient recording the writes the engine
// makes and serving Resolve's reads from a seeded bead map.
type fakeDaemon struct {
	beads   map[string]*beadsapi.BeadDetail
	revDeps map[string][]beadsapi.Dependency

	createCalls   int
	createLastReq beadsapi.CreateBeadRequest
	createLastCtx context.Context
	createErr     error

	addLabelLastVal string
	addDepLastType  string
	addDepLastDep   string
	addDepLastCtx   context.Context

	updateFieldsCalls  int
	updateFieldsLastID string
	updateFieldsVal    map[string]any
	updateFieldsCtx    context.Context

	updateCalls   int
	updateLastID  string
	updateLastReq beadsapi.UpdateBeadRequest
	updateLastCtx context.Context

	// listErr / revErr / batchErr force the respective read to fail.
	listErr  error
	revErr   error
	batchErr error
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		beads:   map[string]*beadsapi.BeadDetail{},
		revDeps: map[string][]beadsapi.Dependency{},
	}
}

func (f *fakeDaemon) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	if f.revErr != nil {
		return nil, f.revErr
	}
	return f.revDeps[id], nil
}

func (f *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Match on the plan_id field filter the engine uses in Resolve.
	wantPlan := q.Fields[PlanIDField]
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if b.Type != "agent" {
			continue
		}
		if wantPlan != "" && b.Fields[PlanIDField] != wantPlan {
			continue
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeDaemon) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if b := f.beads[id]; b != nil {
			out[id] = b
		}
	}
	return out, nil
}

func (f *fakeDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.createCalls++
	f.createLastReq = req
	f.createLastCtx = ctx
	if f.createErr != nil {
		return "", f.createErr
	}
	return "gd-new", nil
}

func (f *fakeDaemon) AddLabel(_ context.Context, _, label string) error {
	f.addLabelLastVal = label
	return nil
}

func (f *fakeDaemon) AddDependency(ctx context.Context, _, dependsOnID, depType, _ string) error {
	f.addDepLastType = depType
	f.addDepLastDep = dependsOnID
	f.addDepLastCtx = ctx
	return nil
}

func (f *fakeDaemon) UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error {
	f.updateFieldsCalls++
	f.updateFieldsLastID = beadID
	f.updateFieldsVal = fields
	f.updateFieldsCtx = ctx
	return nil
}

func (f *fakeDaemon) UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	f.updateCalls++
	f.updateLastID = beadID
	f.updateLastReq = req
	f.updateLastCtx = ctx
	return nil
}

// fakeNudge records Nudge calls and can be forced disabled or to return errors.
type fakeNudge struct {
	disabled    bool
	nudgeCalls  int
	nudgeErr    error
	nudgeLastID string
}

func (f *fakeNudge) Disabled() bool { return f.disabled }
func (f *fakeNudge) Nudge(_ context.Context, sessionID, _ string) error {
	f.nudgeCalls++
	f.nudgeLastID = sessionID
	return f.nudgeErr
}

// testNameGen is a deterministic NameGen for tests.
func testNameGen(project, _ string) (string, error) {
	return project + "-plan-agent-abcd", nil
}

func testEngine(f *fakeDaemon, n NudgeClient) *Engine {
	return NewEngine(Config{
		Daemon:  f,
		Nudge:   n,
		NameGen: testNameGen,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return testNow },
	})
}

func seedPlanEpic(f *fakeDaemon, id string) *beadsapi.BeadDetail {
	e := &beadsapi.BeadDetail{
		ID: id, Title: "Plan: " + id, Type: "epic", Kind: "issue",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
	}
	f.beads[id] = e
	return e
}

// seedAgent seeds a plan-gardener agent bead bound to epicID both ways (assigned
// reverse-dep edge + fields.plan_id) with the given agent_state and updatedAt.
func seedAgent(f *fakeDaemon, id, name, epicID, state string, updatedAt time.Time) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID: id, Title: name, Type: "agent",
		Labels:    []string{RoleLabel, "project:gardenio"},
		Fields:    map[string]string{PlanIDField: epicID, "task_id": epicID, "agent_state": state},
		UpdatedAt: updatedAt,
	}
	f.beads[id] = b
	f.revDeps[epicID] = append(f.revDeps[epicID], beadsapi.Dependency{BeadID: id, DependsOnID: epicID, Type: "assigned"})
	return b
}

func decodeFields(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	return out
}

// --- Resolve ---

func TestResolve_FindsBoundAgent(t *testing.T) {
	f := newFakeDaemon()
	seedPlanEpic(f, "gd-plan1")
	seedAgent(f, "gd-a", "gardenio-plan-agent-a", "gd-plan1", "working", testNow)
	got := testEngine(f, nil).Resolve(context.Background(), "gd-plan1")
	if got == nil || got.ID != "gd-a" {
		t.Fatalf("Resolve = %v, want gd-a", got)
	}
}

func TestResolve_NoneWhenUnbound(t *testing.T) {
	f := newFakeDaemon()
	seedPlanEpic(f, "gd-plan1")
	if got := testEngine(f, nil).Resolve(context.Background(), "gd-plan1"); got != nil {
		t.Fatalf("Resolve = %v, want nil", got)
	}
}

func TestResolve_PrefersLiveOverParked(t *testing.T) {
	f := newFakeDaemon()
	seedPlanEpic(f, "gd-plan1")
	// Parked (older, terminal state) then a live one — live must win regardless
	// of update order.
	seedAgent(f, "gd-parked", "parked", "gd-plan1", "stopping", testNow.Add(-2*time.Minute))
	seedAgent(f, "gd-live", "live", "gd-plan1", "working", testNow.Add(-30*time.Minute))
	got := testEngine(f, nil).Resolve(context.Background(), "gd-plan1")
	if got == nil || got.ID != "gd-live" {
		t.Fatalf("Resolve = %v, want gd-live (live beats parked)", got)
	}
}

// --- IsLive / IsQueued / IsPlanAssistantBead ---

func TestIsLive(t *testing.T) {
	e := testEngine(newFakeDaemon(), nil)
	cases := []struct {
		name  string
		state string
		upd   time.Time
		want  bool
	}{
		{"working-fresh", "working", testNow.Add(-time.Minute), true},
		{"working-stale", "working", testNow.Add(-2 * time.Hour), false},
		{"stopping", "stopping", testNow, false},
		{"queued", queuedState, testNow, false},
		{"no-timestamp-working", "working", time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": tc.state}, UpdatedAt: tc.upd}
			if got := e.IsLive(b); got != tc.want {
				t.Errorf("IsLive(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
	if e.IsLive(nil) {
		t.Error("IsLive(nil) = true, want false")
	}
}

func TestIsQueued(t *testing.T) {
	if !IsQueued(&beadsapi.BeadDetail{Fields: map[string]string{"agent_state": queuedState}}) {
		t.Error("IsQueued(queued) = false, want true")
	}
	if IsQueued(&beadsapi.BeadDetail{Fields: map[string]string{"agent_state": "working"}}) {
		t.Error("IsQueued(working) = true, want false")
	}
	if IsQueued(nil) {
		t.Error("IsQueued(nil) = true, want false")
	}
}

func TestIsPlanAssistantBead(t *testing.T) {
	// Right role + right plan binding.
	ok := &beadsapi.BeadDetail{Labels: []string{RoleLabel}, Fields: map[string]string{PlanIDField: "gd-plan1"}}
	if !IsPlanAssistantBead(ok, "gd-plan1") {
		t.Error("bound plan-gardener agent not recognized")
	}
	// task_id fallback binding.
	tid := &beadsapi.BeadDetail{Labels: []string{RoleLabel}, Fields: map[string]string{"task_id": "gd-plan1"}}
	if !IsPlanAssistantBead(tid, "gd-plan1") {
		t.Error("task_id-bound plan-gardener agent not recognized")
	}
	// Missing role label → not the plan's agent even if bound.
	noRole := &beadsapi.BeadDetail{Fields: map[string]string{PlanIDField: "gd-plan1"}}
	if IsPlanAssistantBead(noRole, "gd-plan1") {
		t.Error("agent without role:plan-gardener label wrongly recognized")
	}
	// Bound to a different plan.
	if IsPlanAssistantBead(ok, "gd-other") {
		t.Error("agent bound to gd-plan1 wrongly matched gd-other")
	}
}

// --- Spawn ---

func TestSpawn_FreshStampsBindingAndEdge(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	id, name, err := testEngine(f, nil).Spawn(context.Background(), "op@acme.example", epic, "help me plan")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if id != "gd-new" || name != "gardenio-plan-agent-abcd" {
		t.Fatalf("Spawn = (%q,%q), want (gd-new, gardenio-plan-agent-abcd)", id, name)
	}
	if f.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", f.createCalls)
	}
	fields := decodeFields(t, f.createLastReq.Fields)
	if fields["role"] != SpawnRole {
		t.Errorf("role = %q, want %q", fields["role"], SpawnRole)
	}
	if fields[PlanIDField] != "gd-plan1" || fields["task_id"] != "gd-plan1" {
		t.Errorf("plan_id/task_id = %q/%q, want gd-plan1", fields[PlanIDField], fields["task_id"])
	}
	if fields["prompt"] != "help me plan" {
		t.Errorf("prompt = %q", fields["prompt"])
	}
	if fields["session_resume"] == "true" {
		t.Error("fresh spawn should not set session_resume")
	}
	if f.addDepLastType != "assigned" || f.addDepLastDep != "gd-plan1" {
		t.Errorf("assigned edge = (%s->%s), want assigned->gd-plan1", f.addDepLastType, f.addDepLastDep)
	}
	if f.addLabelLastVal != RoleLabel {
		t.Errorf("last label = %q, want %q", f.addLabelLastVal, RoleLabel)
	}
}

func TestSpawn_RevivesSameBead(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	seedAgent(f, "gd-prior", "gardenio-plan-agent-prior", "gd-plan1", "stopping", testNow.Add(-30*time.Minute))
	id, name, err := testEngine(f, nil).Spawn(context.Background(), "op@acme.example", epic, "another question")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if id != "gd-prior" || name != "gardenio-plan-agent-prior" {
		t.Fatalf("wake = (%q,%q), want prior id/name (S3 continuity)", id, name)
	}
	if f.createCalls != 0 {
		t.Errorf("wake must NOT CreateBead; got %d", f.createCalls)
	}
	// ONE atomic write (ka-0000001583): the wake sends status + fields in a single
	// UpdateBead PATCH, NOT a separate fields-only UpdateBeadFieldsTyped call. A
	// separate fields-only write on a closed bead would be stripped by the daemon's
	// dropTransientClosedFields guard, leaving agent_state="done" and looping the
	// reopen→reap cycle.
	if f.updateFieldsCalls != 0 {
		t.Errorf("wake must NOT use a separate UpdateBeadFieldsTyped write; got %d "+
			"(status + fields must land in one atomic UpdateBead, ka-0000001583)", f.updateFieldsCalls)
	}
	if f.updateCalls != 1 {
		t.Errorf("wake must be exactly ONE UpdateBead write; got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-prior" {
		t.Errorf("wake updated %q, want gd-prior", f.updateLastID)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "in_progress" {
		t.Errorf("status = %v, want in_progress", f.updateLastReq.Status)
	}
	if f.updateLastReq.Fields["session_resume"] != "true" {
		t.Errorf("session_resume = %v, want true", f.updateLastReq.Fields["session_resume"])
	}
	if f.updateLastReq.Fields["prompt"] != "another question" {
		t.Errorf("prompt = %v", f.updateLastReq.Fields["prompt"])
	}
	if got, ok := f.updateLastReq.Fields["stop_requested"]; !ok || got != "" {
		t.Errorf("stop_requested = %v (present=%v), want cleared", got, ok)
	}
	// agent_state MUST be DELETED (nil) in the SAME write as the reopen so the
	// single AgentSpawn event carries no terminal agent_state — otherwise a
	// rate_limited/done prior state intercepts the wake (ka-0000004492) or re-reaps
	// the bead (ka-0000001583). It is nil (a delete), NOT "" — agent_state is an
	// enum with no empty-string member, so "" 400s at the daemon.
	if got, ok := f.updateLastReq.Fields["agent_state"]; !ok || got != nil {
		t.Errorf("agent_state = %v (present=%v), want nil/delete (ka-0000001583)", got, ok)
	}
}

func TestSpawn_ClosedPlanRefusesFresh(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	epic.Status = "closed"
	id, name, err := testEngine(f, nil).Spawn(context.Background(), "op", epic, "collaborate")
	if !errors.Is(err, ErrPlanClosedNoWake) {
		t.Fatalf("err = %v, want ErrPlanClosedNoWake", err)
	}
	if id != "" || name != "" {
		t.Errorf("refused spawn = (%q,%q), want empty", id, name)
	}
	if f.createCalls != 0 {
		t.Errorf("closed plan must not CreateBead; got %d", f.createCalls)
	}
}

func TestSpawn_ClosedPlanRefusesWake(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	epic.Status = "closed"
	seedAgent(f, "gd-prior", "prior", "gd-plan1", "idle", testNow.Add(-30*time.Minute))
	_, _, err := testEngine(f, nil).Spawn(context.Background(), "op", epic, "still there?")
	if !errors.Is(err, ErrPlanClosedNoWake) {
		t.Fatalf("err = %v, want ErrPlanClosedNoWake", err)
	}
	if f.updateFieldsCalls != 0 || f.updateCalls != 0 || f.createCalls != 0 {
		t.Errorf("closed plan wake wrote: fields=%d update=%d create=%d, want 0/0/0",
			f.updateFieldsCalls, f.updateCalls, f.createCalls)
	}
}

// --- Prompt ---

func TestPrompt_NudgesLiveAgent(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	a := seedAgent(f, "gd-live", "live", "gd-plan1", "working", testNow)
	a.Fields["last_session_id"] = "sess-1"
	n := &fakeNudge{}
	res, err := testEngine(f, n).Prompt(context.Background(), epic, "op", "hey")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if n.nudgeCalls != 1 {
		t.Errorf("nudgeCalls = %d, want 1", n.nudgeCalls)
	}
	if res.Path != PromptPathNudge || res.Outcome != DeliverySent {
		t.Errorf("res = %+v, want nudge/sent", res)
	}
	if f.createCalls != 0 || f.updateCalls != 0 {
		t.Errorf("live nudge must not spawn/wake; create=%d update=%d", f.createCalls, f.updateCalls)
	}
}

// A pod-run agent is registered with coopmux under its pod name, and
// last_session_id is coop's own id, which coopmux never saw. Nudging the
// latter 404s and the fall-through Wake re-drives the running pod, a second
// session on it (gd-XCwA8MXX8m). The nudge must address the pod name.
func TestPrompt_NudgesLiveAgentByPodName(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	a := seedAgent(f, "gd-live", "live", "gd-plan1", "working", testNow)
	a.Fields["last_session_id"] = "6dedd2f8-coop-session"
	a.Notes = "pod_name: crew-gardenio-plan-gardener-live\npod_namespace: gardenio"
	n := &fakeNudge{}
	res, err := testEngine(f, n).Prompt(context.Background(), epic, "op", "hey")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if n.nudgeLastID != "crew-gardenio-plan-gardener-live" {
		t.Errorf("nudged session %q, want the pod name", n.nudgeLastID)
	}
	if res.Path != PromptPathNudge || f.updateCalls != 0 {
		t.Errorf("res = %+v, updateCalls = %d; a live pod must be nudged, not woken", res, f.updateCalls)
	}
}

func TestPrompt_ParkedWakes(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	seedAgent(f, "gd-parked", "parked", "gd-plan1", "stopping", testNow.Add(-30*time.Minute))
	n := &fakeNudge{}
	res, err := testEngine(f, n).Prompt(context.Background(), epic, "op", "wake up")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if n.nudgeCalls != 0 {
		t.Errorf("parked wake must not Nudge; got %d", n.nudgeCalls)
	}
	if res.Path != PromptPathSpawn || res.Outcome != DeliveryQueuedWaking {
		t.Errorf("res = %+v, want spawn/queued-waking", res)
	}
	if f.updateCalls != 1 {
		t.Errorf("parked wake should flip status; updateCalls = %d", f.updateCalls)
	}
}

func TestPrompt_FirstContactSpawns(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	res, err := testEngine(f, &fakeNudge{}).Prompt(context.Background(), epic, "op", "first message")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Path != PromptPathSpawn || res.Outcome != DeliverySpawning {
		t.Errorf("res = %+v, want spawn/spawning", res)
	}
	if f.createCalls != 1 {
		t.Errorf("first contact should CreateBead; got %d", f.createCalls)
	}
}

func TestPrompt_ClosedPlanGracefulNoop(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	epic.Status = "closed"
	res, err := testEngine(f, &fakeNudge{}).Prompt(context.Background(), epic, "op", "hi")
	if err != nil {
		t.Fatalf("closed plan Prompt should be a graceful no-op, got err=%v", err)
	}
	if res.Path != PromptPathSpawn || res.Outcome != DeliveryNone || res.AgentName != "" {
		t.Errorf("res = %+v, want spawn/none/empty (graceful no-op)", res)
	}
	if f.createCalls != 0 || f.updateCalls != 0 {
		t.Errorf("closed plan must write nothing; create=%d update=%d", f.createCalls, f.updateCalls)
	}
}

func TestPrompt_LiveButSessionGoneFallsThroughToWake(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	a := seedAgent(f, "gd-live", "live", "gd-plan1", "working", testNow)
	a.Fields["last_session_id"] = "sess-gone"
	n := &fakeNudge{nudgeErr: coopmuxapi.ErrSessionNotFound}
	res, err := testEngine(f, n).Prompt(context.Background(), epic, "op", "hey")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// Nudge attempted, failed session-gone → wake path (revive same bead).
	if n.nudgeCalls != 1 {
		t.Errorf("nudgeCalls = %d, want 1 (attempted)", n.nudgeCalls)
	}
	if res.Path != PromptPathSpawn {
		t.Errorf("path = %v, want spawn (fell through)", res.Path)
	}
	if f.updateCalls != 1 {
		t.Errorf("session-gone should wake (flip status); updateCalls = %d", f.updateCalls)
	}
}

func TestPrompt_HardNudgeErrorSurfaces(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	a := seedAgent(f, "gd-live", "live", "gd-plan1", "working", testNow)
	a.Fields["last_session_id"] = "sess-1"
	boom := errors.New("upstream boom")
	n := &fakeNudge{nudgeErr: boom}
	res, err := testEngine(f, n).Prompt(context.Background(), epic, "op", "hey")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the hard nudge error", err)
	}
	if res.Path != PromptPathNudge {
		t.Errorf("path = %v, want nudge (no spawn on hard error)", res.Path)
	}
	if f.createCalls != 0 || f.updateCalls != 0 {
		t.Errorf("hard nudge error must not spawn/wake; create=%d update=%d", f.createCalls, f.updateCalls)
	}
}

func TestPrompt_NoCoopmuxDegradesToWake(t *testing.T) {
	f := newFakeDaemon()
	epic := seedPlanEpic(f, "gd-plan1")
	a := seedAgent(f, "gd-live", "live", "gd-plan1", "working", testNow)
	a.Fields["last_session_id"] = "sess-1"
	// nil NudgeClient → no coopmux; a live agent's message still must be delivered
	// via the wake/thaw path rather than lost.
	res, err := testEngine(f, nil).Prompt(context.Background(), epic, "op", "hey")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Path != PromptPathSpawn {
		t.Errorf("path = %v, want spawn (degraded, no coopmux)", res.Path)
	}
	if f.updateCalls != 1 {
		t.Errorf("no-coopmux live agent should wake; updateCalls = %d", f.updateCalls)
	}
}
