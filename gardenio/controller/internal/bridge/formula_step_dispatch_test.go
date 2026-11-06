package bridge

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/spawnguard"
)

// fakeStepDaemon is a recording stand-in for the daemon slice the dispatcher
// uses. Bead graph is expressed as two maps so a test can wire an arbitrary
// blocks chain without a server.
type fakeStepDaemon struct {
	mu sync.Mutex

	beads   map[string]*beadsapi.BeadDetail
	deps    map[string][]beadsapi.Dependency // beadID -> forward deps ("beadID depends on X")
	revDeps map[string][]beadsapi.Dependency // beadID -> reverse deps ("X depends on beadID")

	getBeadErr    error
	revDepErr     error
	depErr        error
	updateErr     error
	spawnErr      error
	listErr       error
	updateErrOnce bool // updateErr applies only to the first call

	// listResult is what ListBeadsFiltered returns; nil means "derive from the
	// bead map" (all open beads carrying suggest_new_agent=true), so backfill
	// tests can lean on the same graph the event-path tests wire up.
	listResult  *beadsapi.ListBeadsResult
	listQueries []beadsapi.ListBeadsQuery

	// Recorded calls.
	spawnedNames    []string
	spawnedRoles    []string
	spawnedProject  []string
	spawnedTaskIDs  []string
	spawnedPrompts  []string
	spawnedFields   []map[string]string
	spawnedOperator []string // beadsapi.OperatorFromContext at spawn time — the X-Operated-By header the daemon resolver reads
	updates         []stepFieldUpdate
	getBeadCalls    int
}

type stepFieldUpdate struct {
	beadID string
	fields map[string]string
}

func newFakeStepDaemon() *fakeStepDaemon {
	return &fakeStepDaemon{
		beads:   make(map[string]*beadsapi.BeadDetail),
		deps:    make(map[string][]beadsapi.Dependency),
		revDeps: make(map[string][]beadsapi.Dependency),
	}
}

// addBlocks wires "stepID is blocked by blockerID" in both directions, the way
// instantiateFormula's AddDependency(step, dep, "blocks") does.
func (f *fakeStepDaemon) addBlocks(stepID, blockerID string) {
	f.deps[stepID] = append(f.deps[stepID], beadsapi.Dependency{
		BeadID: stepID, DependsOnID: blockerID, Type: "blocks",
	})
	f.revDeps[blockerID] = append(f.revDeps[blockerID], beadsapi.Dependency{
		BeadID: stepID, DependsOnID: blockerID, Type: "blocks",
	})
}

func (f *fakeStepDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getBeadCalls++
	if f.getBeadErr != nil {
		return nil, f.getBeadErr
	}
	b, ok := f.beads[beadID]
	if !ok {
		return nil, errors.New("no such bead: " + beadID)
	}
	return b, nil
}

func (f *fakeStepDaemon) GetReverseDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revDepErr != nil {
		return nil, f.revDepErr
	}
	return f.revDeps[beadID], nil
}

func (f *fakeStepDaemon) GetDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.depErr != nil {
		return nil, f.depErr
	}
	return f.deps[beadID], nil
}

func (f *fakeStepDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, stepFieldUpdate{beadID: beadID, fields: fields})
	if f.updateErr != nil {
		err := f.updateErr
		if f.updateErrOnce {
			f.updateErr = nil
		}
		return err
	}
	// Mirror onto the bead so a re-read sees the stamp (durable-idempotency
	// tests depend on this).
	if b, ok := f.beads[beadID]; ok {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		maps.Copy(b.Fields, fields)
	}
	return nil
}

func (f *fakeStepDaemon) SpawnAgent(ctx context.Context, agentName, project, taskID, role, prompt string, extraFields ...map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Capture the ctx operator BEFORE the spawnErr short-circuit: the daemon's
	// createBead resolves attribution from this header (beadsapi.WithOperator →
	// X-Operated-By), so a bug that fails to set it must be observable even when
	// the real daemon would then 400 (which the fake models via spawnErr).
	f.spawnedOperator = append(f.spawnedOperator, beadsapi.OperatorFromContext(ctx))
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	f.spawnedNames = append(f.spawnedNames, agentName)
	f.spawnedRoles = append(f.spawnedRoles, role)
	f.spawnedProject = append(f.spawnedProject, project)
	f.spawnedTaskIDs = append(f.spawnedTaskIDs, taskID)
	f.spawnedPrompts = append(f.spawnedPrompts, prompt)
	merged := map[string]string{}
	for _, ef := range extraFields {
		maps.Copy(merged, ef)
	}
	f.spawnedFields = append(f.spawnedFields, merged)
	return "gd-agent-" + agentName, nil
}

func (f *fakeStepDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listQueries = append(f.listQueries, q)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResult != nil {
		return f.listResult, nil
	}
	// Default: every open bead that opted into its own agent, mirroring the
	// server-side filter BackfillReadySteps asks for.
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if b.Status != "open" {
			continue
		}
		if b.Fields == nil || !stepDispatchFieldTrue(b.Fields["suggest_new_agent"]) {
			continue
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *fakeStepDaemon) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawnedNames)
}

// readyStep builds the canonical eligible step bead: open, poured by a formula,
// opted in, with a role. Tests mutate one field at a time to prove each gate.
func readyStep(id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Prove the fix with before/after evidence",
		Type:   "task",
		Status: "open",
		Labels: []string{"project:monorepo", "role:monorepo-fix"},
		Fields: map[string]string{
			"source_formula":    "gd-formula1",
			"source_step_id":    "capture-before",
			"molecule_id":       "gd-mol1",
			"step_role":         "monorepo-fix",
			"step_project":      "monorepo",
			"suggest_new_agent": "true",
		},
	}
}

func closedBead(id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "task", Status: "closed"}
}

// --- The core case: FSM-472. -------------------------------------------------

// TestStepDispatch_SpawnsRoleMatchedAgentWhenBlockerCloses is the regression
// test for bug ka-0000001755: the §7 repro step closes, the §8 fix step is
// ready and declares role monorepo-fix, and NOTHING was alive to hand off to.
// Before this dispatcher, zero role:monorepo-fix agents existed fleet-wide.
func TestStepDispatch_SpawnsRoleMatchedAgentWhenBlockerCloses(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want exactly 1 spawn, got %d", got)
	}
	if f.spawnedRoles[0] != "monorepo-fix" {
		t.Errorf("want role monorepo-fix (the step's declared role), got %q", f.spawnedRoles[0])
	}
	if f.spawnedProject[0] != "monorepo" {
		t.Errorf("want project monorepo, got %q", f.spawnedProject[0])
	}
	if f.spawnedTaskIDs[0] != "gd-fix" {
		t.Errorf("want the step bead as the assigned task (that is what writes the `assigned` edge), got %q", f.spawnedTaskIDs[0])
	}
	if !strings.Contains(f.spawnedNames[0], "monorepo-") {
		t.Errorf("want a project-prefixed agent name, got %q", f.spawnedNames[0])
	}
}

// --- Role resolution. --------------------------------------------------------

// TestStepDispatch_FallsBackToRoleLabel covers a step poured BEFORE this change
// landed: it carries the `role:` label pours have always written, but no
// step_role field. A live molecule mid-flight across a deploy must still
// dispatch rather than silently declining.
func TestStepDispatch_FallsBackToRoleLabel(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	step := readyStep("gd-fix")
	delete(step.Fields, "step_role")
	delete(step.Fields, "step_project")
	f.beads["gd-fix"] = step
	f.addBlocks("gd-fix", "gd-emit")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want 1 spawn from the role: label fallback, got %d", got)
	}
	if f.spawnedRoles[0] != "monorepo-fix" {
		t.Errorf("want role recovered from the role: label, got %q", f.spawnedRoles[0])
	}
	if f.spawnedProject[0] != "monorepo" {
		t.Errorf("want project recovered from the project: label, got %q", f.spawnedProject[0])
	}
}

func TestStepDispatch_StepRoleFieldWinsOverLabel(t *testing.T) {
	step := readyStep("gd-fix")
	step.Fields["step_role"] = "monorepo-verify"
	step.Labels = []string{"role:stale-role"}

	if got := stepDispatchRole(step); got != "monorepo-verify" {
		t.Errorf("want the explicit step_role field to win, got %q", got)
	}
}

// --- The opt-in gate. --------------------------------------------------------

// TestStepDispatch_SkipsWithoutSuggestNewAgent is the blast-radius guard. Most
// formulas intend ONE driver to walk the whole chain holding context; blanket
// dispatch would mint a pod per step fleet-wide and break them.
func TestStepDispatch_SkipsWithoutSuggestNewAgent(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	step := readyStep("gd-fix")
	delete(step.Fields, "suggest_new_agent")
	f.beads["gd-fix"] = step
	f.addBlocks("gd-fix", "gd-emit")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no spawn without the opt-in, got %d", got)
	}
}

func TestStepDispatch_AcceptsAlternateTruthSpellings(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "1", "yes", " true "} {
		if !stepDispatchFieldTrue(v) {
			t.Errorf("want %q to read as true (the daemon has rendered booleans several ways)", v)
		}
	}
	for _, v := range []string{"", "false", "0", "no", "maybe"} {
		if stepDispatchFieldTrue(v) {
			t.Errorf("want %q to read as false", v)
		}
	}
}

// --- Eligibility gates, as a table. ------------------------------------------

func TestStepDispatchEligible(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*beadsapi.BeadDetail)
		wantOK  bool
		wantHas string // substring the reason must contain
	}{
		{name: "canonical ready step", mutate: func(*beadsapi.BeadDetail) {}, wantOK: true},
		{
			name:    "closed step",
			mutate:  func(b *beadsapi.BeadDetail) { b.Status = "closed" },
			wantHas: "not open",
		},
		{
			name:    "in-progress step",
			mutate:  func(b *beadsapi.BeadDetail) { b.Status = "in_progress" },
			wantHas: "not open",
		},
		{
			name:    "not a formula step",
			mutate:  func(b *beadsapi.BeadDetail) { delete(b.Fields, "source_step_id") },
			wantHas: "source_step_id",
		},
		{
			name:    "no fields at all",
			mutate:  func(b *beadsapi.BeadDetail) { b.Fields = nil },
			wantHas: "no fields",
		},
		{
			name:    "not opted in",
			mutate:  func(b *beadsapi.BeadDetail) { b.Fields["suggest_new_agent"] = "false" },
			wantHas: "suggest_new_agent",
		},
		{
			name: "no role anywhere",
			mutate: func(b *beadsapi.BeadDetail) {
				delete(b.Fields, "step_role")
				b.Labels = []string{"project:monorepo"}
			},
			wantHas: "no step role",
		},
		{
			name:    "already dispatched (durable stamp)",
			mutate:  func(b *beadsapi.BeadDetail) { b.Fields["dispatched_agent"] = "monorepo-fix-abc" },
			wantHas: "already dispatched",
		},
		{
			name:    "already assigned to a live driver",
			mutate:  func(b *beadsapi.BeadDetail) { b.Assignee = "some-live-crew" },
			wantHas: "already assigned",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			step := readyStep("gd-fix")
			tc.mutate(step)
			reason, code, ok := stepDispatchEligible(step)
			if ok != tc.wantOK {
				t.Fatalf("eligible=%v want %v (reason %q)", ok, tc.wantOK, reason)
			}
			if tc.wantHas != "" && !strings.Contains(reason, tc.wantHas) {
				t.Errorf("reason %q should mention %q", reason, tc.wantHas)
			}
			// When ineligible, the bounded code must be a real enum member so the
			// decline counter never sees an unknown label from this path.
			if !tc.wantOK && !stepDispatchDeclineReasonValid(code) {
				t.Errorf("ineligible step returned code %q, not a member of the decline-reason enum", code)
			}
			if tc.wantOK && code != "" {
				t.Errorf("eligible step should return an empty code, got %q", code)
			}
		})
	}
}

// --- Partial-unblock: the sibling-fanout case. -------------------------------

// TestStepDispatch_WaitsForAllBlockers is the one that would ship a real bug if
// wrong: a step blocked on three siblings must not fire when the FIRST closes.
func TestStepDispatch_WaitsForAllBlockers(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-a"] = closedBead("gd-a")
	f.beads["gd-b"] = &beadsapi.BeadDetail{ID: "gd-b", Status: "open"}
	f.beads["gd-c"] = &beadsapi.BeadDetail{ID: "gd-c", Status: "open"}
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-a")
	f.addBlocks("gd-fix", "gd-b")
	f.addBlocks("gd-fix", "gd-c")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())

	d.OnStepClosed(context.Background(), "gd-a")
	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no spawn while gd-b and gd-c are still open, got %d", got)
	}

	// Close b — still one open blocker.
	f.beads["gd-b"] = closedBead("gd-b")
	d.OnStepClosed(context.Background(), "gd-b")
	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no spawn while gd-c is still open, got %d", got)
	}

	// Close the last one.
	f.beads["gd-c"] = closedBead("gd-c")
	d.OnStepClosed(context.Background(), "gd-c")
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want exactly 1 spawn once every blocker closed, got %d", got)
	}
}

// TestStepDispatch_ParentChildEdgeIsNotBlocking guards the distinction the
// ready-queue makes: a step's parent-child edge to its (open) molecule is a
// hierarchy edge, not a wait edge, and must not hold dispatch.
func TestStepDispatch_ParentChildEdgeIsNotBlocking(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-mol1"] = &beadsapi.BeadDetail{ID: "gd-mol1", Type: "molecule", Status: "open"}
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")
	// The parent-child edge every poured step carries.
	f.deps["gd-fix"] = append(f.deps["gd-fix"], beadsapi.Dependency{
		BeadID: "gd-fix", DependsOnID: "gd-mol1", Type: "parent-child",
	})

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want 1 spawn — the open molecule parent must not block, got %d", got)
	}
}

// TestStepDispatch_IgnoresNonBlockingReverseEdges: closing a bead whose
// dependents hang off parent-child must not dispatch them.
func TestStepDispatch_IgnoresNonBlockingReverseEdges(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-parent"] = closedBead("gd-parent")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.revDeps["gd-parent"] = []beadsapi.Dependency{
		{BeadID: "gd-fix", DependsOnID: "gd-parent", Type: "parent-child"},
	}

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-parent")

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no spawn off a parent-child reverse edge, got %d", got)
	}
}

// --- Idempotency. ------------------------------------------------------------

// TestStepDispatch_DedupsRepeatedCloseEvents: an SSE replay after a reconnect
// re-delivers the same close. Two agents racing the same fix branch is the
// expensive failure here.
func TestStepDispatch_DedupsRepeatedCloseEvents(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	for range 5 {
		d.OnStepClosed(context.Background(), "gd-emit")
	}

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want exactly 1 spawn across 5 replayed close events, got %d", got)
	}
}

// TestStepDispatch_DurableStampSurvivesRestart: the in-process dedup map is
// gone after a restart, so the `dispatched_agent` field must carry the guard.
func TestStepDispatch_DurableStampSurvivesRestart(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("first dispatcher: want 1 spawn, got %d", got)
	}

	// A brand-new dispatcher (fresh process) sees the same close again.
	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want the durable dispatched_agent stamp to suppress the second spawn, got %d total", got)
	}
}

// TestStepDispatch_StampWrittenBeforeSpawn: ordering matters. An unstamped
// spawn is one a restart would repeat.
func TestStepDispatch_StampWrittenBeforeSpawn(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if len(f.updates) == 0 {
		t.Fatal("want a dispatched_agent stamp before the spawn, got no field update")
	}
	stamped := f.updates[0].fields["dispatched_agent"]
	if stamped == "" {
		t.Fatalf("want a non-empty dispatched_agent, got %#v", f.updates[0].fields)
	}
	if stamped != f.spawnedNames[0] {
		t.Errorf("stamp %q should name the agent actually spawned (%q) so a human can trace it",
			stamped, f.spawnedNames[0])
	}
}

// TestStepDispatch_StampFailureAbortsSpawn: if we cannot make the dispatch
// durable, we must not spawn — an unstamped spawn is duplicated on restart.
func TestStepDispatch_StampFailureAbortsSpawn(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")
	f.updateErr = errors.New("daemon down")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no spawn when the durability stamp failed, got %d", got)
	}
}

// TestStepDispatch_SpawnFailureRollsBackStamp: a stamp with no agent behind it
// silently suppresses every future attempt, which is worse than no stamp.
func TestStepDispatch_SpawnFailureRollsBackStamp(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")
	f.spawnErr = errors.New("no capacity")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.beads["gd-fix"].Fields["dispatched_agent"]; got != "" {
		t.Errorf("want dispatched_agent cleared after a failed spawn so the step is retryable, got %q", got)
	}

	// And it must actually retry.
	f.spawnErr = nil
	d.OnStepClosed(context.Background(), "gd-emit")
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want the rolled-back step to dispatch on the next close event, got %d spawns", got)
	}
}

// TestStepDispatch_SingleClaimFlagSet: two racing spawn paths (this dispatcher
// and a still-live molecule driver) must converge on one agent. That guard
// lives in SpawnAgent and is opt-in.
func TestStepDispatch_SingleClaimFlagSet(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if len(f.spawnedFields) == 0 {
		t.Fatal("no spawn recorded")
	}
	if f.spawnedFields[0]["single_claim_task"] != "1" {
		t.Error("want single_claim_task=1 so SpawnAgent refuses a second agent on this step")
	}
	if f.spawnedFields[0]["spawned_by"] != "formula-step-dispatcher" {
		t.Errorf("want spawned_by attribution, got %q", f.spawnedFields[0]["spawned_by"])
	}
	if f.spawnedFields[0]["molecule_id"] != "gd-mol1" {
		t.Errorf("want the molecule carried onto the agent bead, got %q", f.spawnedFields[0]["molecule_id"])
	}
	if f.spawnedFields[0]["source_step_id"] != "capture-before" {
		t.Errorf("want the formula step id carried through, got %q", f.spawnedFields[0]["source_step_id"])
	}
}

func TestStepDispatch_DedupExpiresAfterTTL(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	now := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)
	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.now = func() time.Time { return now }

	if !d.claim("gd-fix") {
		t.Fatal("first claim should succeed")
	}
	if d.claim("gd-fix") {
		t.Error("second claim inside the TTL should be refused")
	}
	now = now.Add(stepDispatchDedupTTL + time.Minute)
	if !d.claim("gd-fix") {
		t.Error("claim should be re-grantable once the TTL expired")
	}
}

// --- Failure handling: never wedge. ------------------------------------------

func TestStepDispatch_FailsClosedOnUnreadableBlocker(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")
	// A second blocker whose bead is missing from the graph.
	f.addBlocks("gd-fix", "gd-ghost")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no spawn when a blocker cannot be read — spawning on a half-read graph puts an agent on work whose inputs may not exist, got %d", got)
	}
}

func TestStepDispatch_SurvivesDaemonErrors(t *testing.T) {
	tests := []struct {
		name    string
		breakIt func(*fakeStepDaemon)
	}{
		{name: "reverse deps fail", breakIt: func(f *fakeStepDaemon) { f.revDepErr = errors.New("boom") }},
		{name: "get bead fails", breakIt: func(f *fakeStepDaemon) { f.getBeadErr = errors.New("boom") }},
		{name: "forward deps fail", breakIt: func(f *fakeStepDaemon) { f.depErr = errors.New("boom") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeStepDaemon()
			f.beads["gd-emit"] = closedBead("gd-emit")
			f.beads["gd-fix"] = readyStep("gd-fix")
			f.addBlocks("gd-fix", "gd-emit")
			tc.breakIt(f)

			d := NewFormulaStepDispatcher(f, nil, quietLogger())
			// The contract is "logs and returns" — a panic here would wedge the
			// SSE pump and silently stop dispatching every OTHER step.
			d.OnStepClosed(context.Background(), "gd-emit")

			if got := f.spawnCount(); got != 0 {
				t.Errorf("want no spawn on a daemon error, got %d", got)
			}
		})
	}
}

func TestStepDispatch_NilSafe(t *testing.T) {
	var d *FormulaStepDispatcher
	d.OnStepClosed(context.Background(), "gd-x")
	d.Register(nil)

	if got := NewFormulaStepDispatcher(nil, nil, quietLogger()); got != nil {
		t.Error("want nil dispatcher for a nil daemon, so a deployment without one just runs without dispatch")
	}
}

func TestStepDispatch_EmptyClosedIDIsNoOp(t *testing.T) {
	f := newFakeStepDaemon()
	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "")
	if f.getBeadCalls != 0 {
		t.Errorf("want no daemon traffic for an empty bead id, got %d GetBead calls", f.getBeadCalls)
	}
}

// --- Spawn guard. ------------------------------------------------------------

// TestStepDispatch_RespectsSpawnGuard guards ka-0000001730: a step dispatch the
// shared spawn guard refuses spawns nothing and stamps nothing.
func TestStepDispatch_RespectsSpawnGuard(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	// A one-slot budget, already spent. (RateMax: 0 means "no rate cap", not
	// "refuse everything" — see spawnguard.Allow.)
	guard := spawnguard.New(spawnguard.Config{
		RateMax: 1, RateWindow: time.Hour,
		TripThreshold: 100, TripWindow: time.Minute, TripCooldown: time.Minute,
	})
	if ok, _ := guard.Allow(); !ok {
		t.Fatal("precondition: the first Allow should consume the only slot")
	}

	d := NewFormulaStepDispatcher(f, guard, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no spawn when the guard refuses, got %d", got)
	}
	if got := f.beads["gd-fix"].Fields["dispatched_agent"]; got != "" {
		t.Errorf("a guard refusal must leave no durable stamp to clean up, got %q", got)
	}
}

// --- Prompt. -----------------------------------------------------------------

func TestStepDispatchPrompt_PointsAtTheBeadNotTheDescription(t *testing.T) {
	step := readyStep("gd-fix")
	step.Description = strings.Repeat("a very long phase contract. ", 500)

	prompt := stepDispatchPrompt(step, "gd-emit")

	for _, want := range []string{"gd-fix", "gd-emit", "capture-before", "gd-mol1", "gd-formula1"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt should reference %q so the agent can find its context; got:\n%s", want, prompt)
		}
	}
	// The step description is the contract and lives on the bead — inlining it
	// into BOAT_PROMPT just makes the agent read it twice.
	if strings.Contains(prompt, step.Description) {
		t.Error("prompt should point at the bead, not inline its whole description")
	}
}

// --- SSE registration end-to-end. --------------------------------------------

func TestStepDispatch_RegisterHandlesCloseEvent(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: "http://unused",
		Topics:        []string{stepDispatchTopic},
		Logger:        quietLogger(),
	})
	NewFormulaStepDispatcher(f, nil, quietLogger()).Register(stream)

	// Drive the registered handler with the daemon's real close-event shape.
	stream.mu.RLock()
	handlers := stream.handlers[stepDispatchTopic]
	stream.mu.RUnlock()
	if len(handlers) != 1 {
		t.Fatalf("want exactly 1 handler registered on %s, got %d", stepDispatchTopic, len(handlers))
	}
	handlers[0](context.Background(), []byte(`{"bead":{"id":"gd-emit","type":"task","status":"closed"}}`))

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want the SSE close event to drive a dispatch, got %d spawns", got)
	}
}

// --- Ready-queue parity. -----------------------------------------------------
//
// This dispatcher's premise is "the bead just became ready", so its notion of
// blocked must match the daemon ready-queue's exactly (blockingDepTypes and
// IsTerminalForProgression in kbeads). Divergence either spawns for beads the
// fleet does not consider workable, or strands ones it does.

func TestStepDispatchIsBlockingDep_MatchesReadyQueue(t *testing.T) {
	// The daemon's blockingDepTypes, verbatim.
	for _, blocking := range []string{"blocks", "blocked-by", "depends-on"} {
		if !stepDispatchIsBlockingDep(blocking) {
			t.Errorf("%q is a wait edge in the ready-queue and must block dispatch too", blocking)
		}
	}
	// parent-child is a HIERARCHY edge. Every poured step has one onto its
	// still-open molecule, so treating it as blocking means nothing ever fires.
	for _, notBlocking := range []string{"parent-child", "related", "triggered-by", "assigned", ""} {
		if stepDispatchIsBlockingDep(notBlocking) {
			t.Errorf("%q is not a wait edge and must not block dispatch", notBlocking)
		}
	}
}

func TestStepDispatchBlockerCleared_MatchesReadyQueue(t *testing.T) {
	// IsTerminalForProgression: closed OR partial.
	for _, cleared := range []string{"closed", "partial"} {
		if !stepDispatchBlockerCleared(cleared) {
			t.Errorf("%q is terminal-for-progression in the ready-queue and must unblock", cleared)
		}
	}
	for _, holds := range []string{"open", "in_progress", "deferred", "blocked", ""} {
		if stepDispatchBlockerCleared(holds) {
			t.Errorf("%q must still block", holds)
		}
	}
}

// TestStepDispatch_PartialBlockerUnblocks: `partial` means "done enough to
// proceed". Requiring `closed` would strand exactly the pipelines that use it.
func TestStepDispatch_PartialBlockerUnblocks(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = &beadsapi.BeadDetail{ID: "gd-emit", Status: "partial"}
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want 1 spawn — partial is terminal-for-progression in the ready-queue, got %d", got)
	}
}

// TestStepDispatch_DependsOnEdgeAlsoDispatches: formulas poured by other
// writers use "depends-on" rather than "blocks"; both are wait edges.
func TestStepDispatch_DependsOnEdgeAlsoDispatches(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.deps["gd-fix"] = []beadsapi.Dependency{
		{BeadID: "gd-fix", DependsOnID: "gd-emit", Type: "depends-on"},
	}
	f.revDeps["gd-emit"] = []beadsapi.Dependency{
		{BeadID: "gd-fix", DependsOnID: "gd-emit", Type: "depends-on"},
	}

	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want 1 spawn off a depends-on wait edge, got %d", got)
	}
}

// TestStepDispatch_RegistersBothTopics: a step going `partial` emits an UPDATE,
// not a close. Subscribing to closes alone silently strands those pipelines.
func TestStepDispatch_RegistersBothTopics(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = &beadsapi.BeadDetail{ID: "gd-emit", Status: "partial"}
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: "http://unused",
		Topics:        []string{stepDispatchTopic, stepDispatchUpdateTopic},
		Logger:        quietLogger(),
	})
	NewFormulaStepDispatcher(f, nil, quietLogger()).Register(stream)

	stream.mu.RLock()
	updateHandlers := stream.handlers[stepDispatchUpdateTopic]
	stream.mu.RUnlock()
	if len(updateHandlers) != 1 {
		t.Fatalf("want a handler on %s, got %d", stepDispatchUpdateTopic, len(updateHandlers))
	}

	updateHandlers[0](context.Background(),
		[]byte(`{"bead":{"id":"gd-emit","type":"task","status":"partial"}}`))
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want a partial-status update to drive a dispatch, got %d spawns", got)
	}
}

// TestStepDispatch_UpdateHandlerIgnoresNonTerminal is the cost guard: the
// updated topic is a fleet-wide firehose, so a non-terminal update must cost
// one status compare and no daemon traffic.
func TestStepDispatch_UpdateHandlerIgnoresNonTerminal(t *testing.T) {
	f := newFakeStepDaemon()
	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: "http://unused",
		Topics:        []string{stepDispatchUpdateTopic},
		Logger:        quietLogger(),
	})
	NewFormulaStepDispatcher(f, nil, quietLogger()).Register(stream)

	stream.mu.RLock()
	h := stream.handlers[stepDispatchUpdateTopic][0]
	stream.mu.RUnlock()

	for _, status := range []string{"open", "in_progress", "deferred", ""} {
		h(context.Background(),
			[]byte(`{"bead":{"id":"gd-x","type":"task","status":"`+status+`"}}`))
	}
	if f.getBeadCalls != 0 {
		t.Errorf("a non-terminal update must not touch the daemon, got %d GetBead calls", f.getBeadCalls)
	}
}

func TestStepDispatch_IgnoresUnparseableEvent(t *testing.T) {
	f := newFakeStepDaemon()
	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: "http://unused",
		Topics:        []string{stepDispatchTopic},
		Logger:        quietLogger(),
	})
	NewFormulaStepDispatcher(f, nil, quietLogger()).Register(stream)

	stream.mu.RLock()
	h := stream.handlers[stepDispatchTopic][0]
	stream.mu.RUnlock()

	for _, payload := range []string{`not json`, `{}`, `{"bead":{}}`, `{"bead":{"id":""}}`} {
		h(context.Background(), []byte(payload))
	}
	if f.getBeadCalls != 0 {
		t.Errorf("want malformed events dropped without daemon traffic, got %d GetBead calls", f.getBeadCalls)
	}
}

// --- Backfill sweep (ka-0000006285). -----------------------------------------
//
// The SSE close-event path re-drives a step only when a blocker closes, and in
// a linear chain that happens exactly once. A spawn refused by the guard at
// that instant, or a close missed during an SSE outage / pod handoff, would
// strand the step forever. BackfillReadySteps is the durable retry.

// TestStepDispatch_BackfillRetriesGuardRefusedStep is the core regression: a
// step's dispatch is refused by an exhausted guard (no later close event will
// come to retry it), then the guard budget refills and the backfill sweep
// dispatches it. This is the FSM-472 stranding closed durably rather than
// depending on an event that never repeats.
func TestStepDispatch_BackfillRetriesGuardRefusedStep(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	// One-slot budget, already spent — the close-event dispatch will be refused.
	guard := spawnguard.New(spawnguard.Config{
		RateMax: 1, RateWindow: time.Hour,
		TripThreshold: 100, TripWindow: time.Minute, TripCooldown: time.Minute,
	})
	if ok, _ := guard.Allow(); !ok {
		t.Fatal("precondition: first Allow should consume the only slot")
	}

	d := NewFormulaStepDispatcher(f, guard, quietLogger())
	d.OnStepClosed(context.Background(), "gd-emit")
	if got := f.spawnCount(); got != 0 {
		t.Fatalf("precondition: guard should refuse the event-path dispatch, got %d spawns", got)
	}
	if got := f.beads["gd-fix"].Fields["dispatched_agent"]; got != "" {
		t.Fatalf("a refused dispatch must leave no durable stamp, got %q", got)
	}

	// Budget refills; the backfill sweep is the retry the linear chain never
	// gets from another close event.
	guard.SetNowFunc(func() time.Time { return time.Now().Add(2 * time.Hour) })
	d.BackfillReadySteps(context.Background())

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want the backfill sweep to retry the stranded step, got %d spawns", got)
	}
	if f.spawnedRoles[0] != "monorepo-fix" {
		t.Errorf("want the step's declared role on the retry, got %q", f.spawnedRoles[0])
	}
}

// TestStepDispatch_BackfillDispatchesMissedStep covers the SSE-miss case: a step
// is fully unblocked (its blocker already closed) but no close event ever
// reached this process. The sweep discovers it from the ready state alone.
func TestStepDispatch_BackfillDispatchesMissedStep(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	// Never call OnStepClosed — simulate the missed event.
	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.BackfillReadySteps(context.Background())

	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want the sweep to dispatch a step whose close event was missed, got %d spawns", got)
	}
	// The server-side filter must narrow to open + opted-in steps, not the
	// whole bead table.
	if len(f.listQueries) != 1 {
		t.Fatalf("want exactly one list query per sweep, got %d", len(f.listQueries))
	}
	q := f.listQueries[0]
	if q.Fields["suggest_new_agent"] != "true" {
		t.Errorf("want the sweep to filter suggest_new_agent=true server-side, got %v", q.Fields)
	}
	if len(q.Statuses) != 1 || q.Statuses[0] != "open" {
		t.Errorf("want the sweep to filter status=open server-side, got %v", q.Statuses)
	}
}

// TestStepDispatch_BackfillIsIdempotent: a step already dispatched (durable
// stamp present) must not be re-dispatched by a later sweep — the same guard
// the event path relies on.
func TestStepDispatch_BackfillIsIdempotent(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.BackfillReadySteps(context.Background())
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("first sweep should dispatch once, got %d", got)
	}
	// Second sweep sees the durable dispatched_agent stamp and must skip.
	d.BackfillReadySteps(context.Background())
	if got := f.spawnCount(); got != 1 {
		t.Fatalf("want no second dispatch — the durable stamp is the idempotency guard, got %d", got)
	}
}

// TestStepDispatch_BackfillSkipsStillBlocked: a candidate surfaced by the list
// (opted-in, open) whose blockers are NOT all cleared must be skipped, exactly
// like the event path.
func TestStepDispatch_BackfillSkipsStillBlocked(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = &beadsapi.BeadDetail{ID: "gd-emit", Status: "open"} // still open
	f.beads["gd-fix"] = readyStep("gd-fix")
	f.addBlocks("gd-fix", "gd-emit")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.BackfillReadySteps(context.Background())

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no dispatch while a blocker is still open, got %d", got)
	}
}

// TestStepDispatch_BackfillListErrorIsBestEffort: a list failure logs and
// returns without panicking or spawning.
func TestStepDispatch_BackfillListErrorIsBestEffort(t *testing.T) {
	f := newFakeStepDaemon()
	f.listErr = errors.New("daemon down")

	d := NewFormulaStepDispatcher(f, nil, quietLogger())
	d.BackfillReadySteps(context.Background())

	if got := f.spawnCount(); got != 0 {
		t.Fatalf("want no spawn on a list error, got %d", got)
	}
}

// TestStepDispatch_BackfillNilSafe: a nil dispatcher (no daemon wired) is a
// no-op, matching the nil-safe contract the constructor documents.
func TestStepDispatch_BackfillNilSafe(t *testing.T) {
	var d *FormulaStepDispatcher
	d.BackfillReadySteps(context.Background()) // must not panic
}

// TestStepDispatchPrompt_BackfillVariant: a backfill dispatch has no single
// triggering close, so the prompt must not tell the agent a bead literally
// named "backfill" closed. It still points at the step bead.
func TestStepDispatchPrompt_BackfillVariant(t *testing.T) {
	step := readyStep("gd-fix")
	prompt := stepDispatchPrompt(step, stepDispatchBackfillSentinel)

	if strings.Contains(prompt, "backfill closed") || strings.Contains(prompt, "step backfill") {
		t.Errorf("backfill prompt must not narrate a bead named %q closing; got:\n%s", stepDispatchBackfillSentinel, prompt)
	}
	if !strings.Contains(prompt, "all its blocking steps have completed") {
		t.Errorf("backfill prompt should state the step is ready without a single closing bead; got:\n%s", prompt)
	}
	// It still routes the agent to its own step bead.
	if !strings.Contains(prompt, "gd-fix") {
		t.Errorf("backfill prompt should still point at the step bead; got:\n%s", prompt)
	}
}

// --- Operator attribution on the spawn context (ka-0000000302). --------------
//
// The daemon's createBead fail-closes on every audit-bearing write when no
// human operator can be resolved (resolveAuditOperatorRequired, R2
// ka-0000005772). It reads the operator from the X-Operated-By REQUEST HEADER
// — carried on ctx via beadsapi.WithOperator — NOT from the operated_by FIELD
// in the bead payload. The dispatcher used to propagate operated_by only as a
// bead field, so the spawn's create call sent no header and the daemon 400'd
// "operator could not be resolved"; the stamp then rolled back and the step
// retried forever. These tests pin that the spawn now carries the operator on
// the context.

// operatorReadyStep is readyStep plus a human operated_by field, matching what
// instantiateFormula propagates onto every poured step from the pour's operator.
func operatorReadyStep(id, operatedBy string) *beadsapi.BeadDetail {
	s := readyStep(id)
	s.Fields["operated_by"] = operatedBy
	return s
}

// TestStepDispatch_SpawnCarriesStepOperatorOnContext is the direct regression
// for the bug: a step carrying a human operated_by must spawn with that
// operator on the daemon-client context (the X-Operated-By header), not merely
// as a bead field.
func TestStepDispatch_SpawnCarriesStepOperatorOnContext(t *testing.T) {
	const op = "slack:U0OPERATOR1"
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = operatorReadyStep("gd-fix", op)
	f.addBlocks("gd-fix", "gd-emit")

	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if len(f.spawnedOperator) != 1 {
		t.Fatalf("want exactly 1 spawn, got %d", len(f.spawnedOperator))
	}
	if f.spawnedOperator[0] != op {
		t.Errorf("spawn ctx must carry X-Operated-By=%q (else createBead 400s 'operator could not be resolved'); got %q",
			op, f.spawnedOperator[0])
	}
	// And the field is still stamped so the spawned POD inherits BEADS_OPERATED_BY.
	if f.spawnedFields[0]["operated_by"] != op {
		t.Errorf("want operated_by field %q on the agent bead too, got %q", op, f.spawnedFields[0]["operated_by"])
	}
}

// TestStepDispatch_SpawnUsesDefaultOperatorWhenStepHasNone: a step with no
// operated_by (e.g. an older pour) falls back to the configured default
// operator on the context, so the spawn still attributes to a human and the
// daemon accepts it.
func TestStepDispatch_SpawnUsesDefaultOperatorWhenStepHasNone(t *testing.T) {
	const def = "slack:U0DEFAULT99"
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix") // no operated_by field
	f.addBlocks("gd-fix", "gd-emit")

	NewFormulaStepDispatcher(f, nil, quietLogger()).
		WithDefaultOperator(def).
		OnStepClosed(context.Background(), "gd-emit")

	if len(f.spawnedOperator) != 1 {
		t.Fatalf("want exactly 1 spawn, got %d", len(f.spawnedOperator))
	}
	if f.spawnedOperator[0] != def {
		t.Errorf("spawn ctx must fall back to the default operator %q, got %q", def, f.spawnedOperator[0])
	}
}

// TestStepDispatch_StepOperatorWinsOverDefault: when the step carries its own
// human operator, that wins — the default is only a fallback.
func TestStepDispatch_StepOperatorWinsOverDefault(t *testing.T) {
	const stepOp = "slack:U0OPERATOR1"
	const def = "slack:U0DEFAULT99"
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = operatorReadyStep("gd-fix", stepOp)
	f.addBlocks("gd-fix", "gd-emit")

	NewFormulaStepDispatcher(f, nil, quietLogger()).
		WithDefaultOperator(def).
		OnStepClosed(context.Background(), "gd-emit")

	if len(f.spawnedOperator) != 1 {
		t.Fatalf("want exactly 1 spawn, got %d", len(f.spawnedOperator))
	}
	if f.spawnedOperator[0] != stepOp {
		t.Errorf("step's own operated_by %q must win over default %q, got %q", stepOp, def, f.spawnedOperator[0])
	}
}

// TestStepDispatch_NonHumanStepOperatorNotPlacedOnHeader: a sentinel /
// provider-only operated_by must NOT be placed on the X-Operated-By header (the
// daemon's sanitizer rejects it and drops audit records). With no default
// configured, ctx stays unattributed and the daemon applies its own
// spawn-chain / fleet fallback exactly as before — the dispatcher does not make
// things worse by stamping a poison header.
func TestStepDispatch_NonHumanStepOperatorNotPlacedOnHeader(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = operatorReadyStep("gd-fix", "system:legacy-pre-attribution")
	f.addBlocks("gd-fix", "gd-emit")

	NewFormulaStepDispatcher(f, nil, quietLogger()).OnStepClosed(context.Background(), "gd-emit")

	if len(f.spawnedOperator) != 1 {
		t.Fatalf("want exactly 1 spawn, got %d", len(f.spawnedOperator))
	}
	if f.spawnedOperator[0] != "" {
		t.Errorf("a non-human step operated_by must not become the X-Operated-By header, got %q", f.spawnedOperator[0])
	}
}

// TestStepDispatch_WithDefaultOperatorDropsNonHuman: a misconfigured non-human
// default operator is dropped to "" (mirrors TriggerDefaultOperatorFromEnv), so
// it can never poison the header.
func TestStepDispatch_WithDefaultOperatorDropsNonHuman(t *testing.T) {
	f := newFakeStepDaemon()
	f.beads["gd-emit"] = closedBead("gd-emit")
	f.beads["gd-fix"] = readyStep("gd-fix") // no operated_by
	f.addBlocks("gd-fix", "gd-emit")

	NewFormulaStepDispatcher(f, nil, quietLogger()).
		WithDefaultOperator("system:not-a-human").
		OnStepClosed(context.Background(), "gd-emit")

	if len(f.spawnedOperator) != 1 {
		t.Fatalf("want exactly 1 spawn, got %d", len(f.spawnedOperator))
	}
	if f.spawnedOperator[0] != "" {
		t.Errorf("a non-human default operator must be dropped, not placed on the header; got %q", f.spawnedOperator[0])
	}
}

// TestStepDispatch_BackfillSpawnCarriesStepOperator: the durable backfill sweep
// (the path that re-drives a step whose unblocking close was missed) must
// attribute the spawn the same way the event path does — this is the exact path
// the prod failure surfaced on.
func TestStepDispatch_BackfillSpawnCarriesStepOperator(t *testing.T) {
	const op = "slack:U0OPERATOR1"
	f := newFakeStepDaemon()
	// A fully-unblocked, opted-in step with no closing event to observe.
	f.beads["gd-fix"] = operatorReadyStep("gd-fix", op)

	NewFormulaStepDispatcher(f, nil, quietLogger()).BackfillReadySteps(context.Background())

	if len(f.spawnedOperator) != 1 {
		t.Fatalf("want exactly 1 backfill spawn, got %d", len(f.spawnedOperator))
	}
	if f.spawnedOperator[0] != op {
		t.Errorf("backfill spawn ctx must carry X-Operated-By=%q, got %q", op, f.spawnedOperator[0])
	}
}
