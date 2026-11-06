package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/eventbus"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"github.com/nats-io/nats.go"
)

// capturePublisher records every Publish call so a test can assert exactly which
// topics + payloads the daemon emitted.
type capturePublisher struct {
	mu       sync.Mutex
	topics   []string
	payloads []any
}

func (c *capturePublisher) Publish(_ context.Context, topic string, event any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.topics = append(c.topics, topic)
	c.payloads = append(c.payloads, event)
	return nil
}

func (c *capturePublisher) Close() error { return nil }

// planStepEvents returns every PlanStepAdded payload the publisher captured.
func (c *capturePublisher) planStepEvents() []events.PlanStepAdded {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []events.PlanStepAdded
	for i, topic := range c.topics {
		if topic != events.TopicPlanStepAdded {
			continue
		}
		if p, ok := c.payloads[i].(events.PlanStepAdded); ok {
			out = append(out, p)
		}
	}
	return out
}

// planStepFixtureServer wires a BeadsServer over a fresh mock store + capture
// publisher for the membership-resolution tests.
func planStepFixtureServer() (*BeadsServer, *mockStore, *capturePublisher) {
	ms := newMockStore()
	cap := &capturePublisher{}
	s := NewBeadsServer(ms, cap)
	return s, ms, cap
}

// stepEdge builds the parent-child dependency the emit path receives.
func stepEdge(stepID, parentID, createdBy string) *model.Dependency {
	return &model.Dependency{
		BeadID:      stepID,
		DependsOnID: parentID,
		Type:        model.DepParentChild,
		CreatedBy:   createdBy,
	}
}

func TestMaybeEmitPlanStepAdded_FlatStepUnderPlan(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	// A plan epic (carries the `plan` label) and a task step directly under it.
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.labels["gd-plan"] = []string{planMarkerLabel, "project:gardenio"}
	ms.beads["gd-step"] = &model.Bead{ID: "gd-step", Type: model.TypeTask, Title: "Do the thing", Status: model.StatusOpen}

	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-step", "gd-plan", "agent-x"), "oidc:matt@acme.example")

	evs := cap.planStepEvents()
	if len(evs) != 1 {
		t.Fatalf("emitted %d plan-step events, want 1", len(evs))
	}
	got := evs[0]
	if got.PlanID != "gd-plan" || got.StepID != "gd-step" {
		t.Errorf("event = %+v, want plan=gd-plan step=gd-step", got)
	}
	if got.PhaseID != "" {
		t.Errorf("flat step PhaseID = %q, want empty", got.PhaseID)
	}
	if got.StepTitle != "Do the thing" || got.StepType != "task" {
		t.Errorf("event title/type = %q/%q, want 'Do the thing'/'task'", got.StepTitle, got.StepType)
	}
	if got.Operator != "oidc:matt@acme.example" {
		t.Errorf("event Operator = %q, want the stamped operator", got.Operator)
	}
}

func TestMaybeEmitPlanStepAdded_PhasedStep(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	// A plan that LISTS the phase in phase_order, a phase sub-epic parented to
	// the plan, and a step parented to the phase.
	ms.beads["gd-plan"] = &model.Bead{
		ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"phase_order":["gd-phase"]}`),
	}
	ms.labels["gd-plan"] = []string{planMarkerLabel}
	ms.beads["gd-phase"] = &model.Bead{
		ID: "gd-phase", Type: model.TypeEpic, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{{BeadID: "gd-phase", DependsOnID: "gd-plan", Type: model.DepParentChild}},
	}
	ms.beads["gd-step"] = &model.Bead{ID: "gd-step", Type: model.TypeTask, Title: "Phased work", Status: model.StatusOpen}

	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-step", "gd-phase", "agent-x"), "oidc:matt@acme.example")

	evs := cap.planStepEvents()
	if len(evs) != 1 {
		t.Fatalf("emitted %d plan-step events, want 1", len(evs))
	}
	if evs[0].PlanID != "gd-plan" || evs[0].PhaseID != "gd-phase" || evs[0].StepID != "gd-step" {
		t.Errorf("event = %+v, want plan=gd-plan phase=gd-phase step=gd-step", evs[0])
	}
}

func TestMaybeEmitPlanStepAdded_PhaseNotInPhaseOrderRejected(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	// The sub-epic is parented to the plan but is NOT listed in phase_order —
	// it is a nested sub-epic, not a phase, so its children are not plan steps.
	ms.beads["gd-plan"] = &model.Bead{
		ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"phase_order":["gd-qvnt1onj9s"]}`),
	}
	ms.labels["gd-plan"] = []string{planMarkerLabel}
	ms.beads["gd-subepic"] = &model.Bead{
		ID: "gd-subepic", Type: model.TypeEpic, Status: model.StatusOpen,
		Dependencies: []*model.Dependency{{BeadID: "gd-subepic", DependsOnID: "gd-plan", Type: model.DepParentChild}},
	}
	ms.beads["gd-step"] = &model.Bead{ID: "gd-step", Type: model.TypeTask, Status: model.StatusOpen}

	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-step", "gd-subepic", "agent-x"), "op")

	if evs := cap.planStepEvents(); len(evs) != 0 {
		t.Fatalf("emitted %d plan-step events for a non-phase sub-epic, want 0 (fail-closed)", len(evs))
	}
}

func TestMaybeEmitPlanStepAdded_NonPlanEpicSkipped(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	// An ordinary epic with no `plan` label — a task under it is not a plan step.
	ms.beads["gd-epic"] = &model.Bead{ID: "gd-epic", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-step"] = &model.Bead{ID: "gd-step", Type: model.TypeTask, Status: model.StatusOpen}

	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-step", "gd-epic", "agent-x"), "op")

	if evs := cap.planStepEvents(); len(evs) != 0 {
		t.Fatalf("emitted %d events for a non-plan epic, want 0", len(evs))
	}
}

func TestMaybeEmitPlanStepAdded_EpicChildIsPhaseAddNotStep(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	// The child is itself an EPIC (a phase being ADDED), not a step — excluded.
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.labels["gd-plan"] = []string{planMarkerLabel}
	ms.beads["gd-newphase"] = &model.Bead{ID: "gd-newphase", Type: model.TypeEpic, Status: model.StatusOpen}

	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-newphase", "gd-plan", "agent-x"), "op")

	if evs := cap.planStepEvents(); len(evs) != 0 {
		t.Fatalf("emitted %d events for an epic child (add-phase), want 0", len(evs))
	}
}

func TestMaybeEmitPlanStepAdded_MoleculeChildIsPhaseAddNotStep(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	// A poured molecule child of the plan is a PHASE being emplaced (ka-0000002805),
	// not a step — excluded exactly like an epic child, so it never nudges.
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.labels["gd-plan"] = []string{planMarkerLabel}
	ms.beads["gd-mol"] = &model.Bead{ID: "gd-mol", Type: model.TypeMolecule, Status: model.StatusOpen}

	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-mol", "gd-plan", "agent-x"), "op")

	if evs := cap.planStepEvents(); len(evs) != 0 {
		t.Fatalf("emitted %d events for a molecule child (emplaced phase), want 0", len(evs))
	}
}

// FREEZE/THAW ECHO GUARD (bug ka-0000001684). A plan-gardener's OWN beads
// auto-parent under its plan epic — maybeAddAutoParent links the gardener's
// type=session registration bead to its task_id (the plan epic), and its minted
// type=message plan-transcript beads land the same edge. Those are NOT plan
// steps: the daemon must NOT emit the first-class beads.plan.step.added signal
// for them, or the consumer reactor wakes the just-parked gardener and re-arms
// the echo. A real task child under the SAME plan still emits (asserted by
// TestMaybeEmitPlanStepAdded_FlatStepUnderPlan). Kept in lockstep with the
// viewer reactor twin's TestReactToStepEdge_SessionAndMessageChildrenDoNotNudge.
func TestMaybeEmitPlanStepAdded_SessionAndMessageChildrenSkipped(t *testing.T) {
	for _, stepType := range []model.BeadType{"session", "message"} {
		t.Run(string(stepType), func(t *testing.T) {
			s, ms, cap := planStepFixtureServer()
			ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}
			ms.labels["gd-plan"] = []string{planMarkerLabel}
			// A gardener's own bead (session registration / minted transcript) the
			// daemon auto-parented onto the plan epic — a parent-child edge, but not
			// a work-item step.
			ms.beads["gd-own"] = &model.Bead{ID: "gd-own", Type: stepType, Status: model.StatusOpen}

			s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-own", "gd-plan", "agent-x"), "op")

			if evs := cap.planStepEvents(); len(evs) != 0 {
				t.Fatalf("emitted %d events for a type=%s child (gardener's own auto-parented bead), want 0", len(evs), stepType)
			}
		})
	}
}

func TestMaybeEmitPlanStepAdded_ClosedPlanSkipped(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusClosed}
	ms.labels["gd-plan"] = []string{planMarkerLabel}
	ms.beads["gd-step"] = &model.Bead{ID: "gd-step", Type: model.TypeTask, Status: model.StatusOpen}

	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-step", "gd-plan", "agent-x"), "op")

	if evs := cap.planStepEvents(); len(evs) != 0 {
		t.Fatalf("emitted %d events for a closed plan, want 0", len(evs))
	}
}

func TestMaybeEmitPlanStepAdded_NonParentChildEdgeSkipped(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.labels["gd-plan"] = []string{planMarkerLabel}
	ms.beads["gd-step"] = &model.Bead{ID: "gd-step", Type: model.TypeTask, Status: model.StatusOpen}

	// A blocks edge is not a step edge, even under a real plan.
	blocks := &model.Dependency{BeadID: "gd-step", DependsOnID: "gd-plan", Type: model.DepBlocks, CreatedBy: "agent-x"}
	s.maybeEmitPlanStepAdded(context.Background(), blocks, "op")

	if evs := cap.planStepEvents(); len(evs) != 0 {
		t.Fatalf("emitted %d events for a blocks edge, want 0", len(evs))
	}
}

func TestMaybeEmitPlanStepAdded_OperatorFallsBackToCreatedBy(t *testing.T) {
	s, ms, cap := planStepFixtureServer()
	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.labels["gd-plan"] = []string{planMarkerLabel}
	ms.beads["gd-step"] = &model.Bead{ID: "gd-step", Type: model.TypeTask, Status: model.StatusOpen}

	// Empty operator (auth disabled) — the emit site passes created_by instead.
	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-step", "gd-plan", "agent-x"), "agent-x")

	evs := cap.planStepEvents()
	if len(evs) != 1 || evs[0].Operator != "agent-x" {
		t.Fatalf("event operator fallback = %+v, want operator=agent-x", evs)
	}
}

// TestMaybeEmitPlanStepAdded_DurableJetStreamCopy drives the full emit path with
// a REAL JetStream bus wired into the server, asserting the durable
// beads.plan.step.added copy lands on PLAN_EVENTS AND that a re-emit of the same
// (plan, step) edge is deduped away — the end-to-end proof of the at-least-once
// + dedupe contract, not just the PublishDeduped unit.
func TestMaybeEmitPlanStepAdded_DurableJetStreamCopy(t *testing.T) {
	ns, js := startTestNATS(t)
	_ = ns
	if err := eventbus.EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	bus := eventbus.New()
	bus.SetJetStream(js)
	s.SetBus(bus)

	ms.beads["gd-plan"] = &model.Bead{ID: "gd-plan", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.labels["gd-plan"] = []string{planMarkerLabel}
	ms.beads["gd-step"] = &model.Bead{ID: "gd-step", Type: model.TypeTask, Title: "Durable step", Status: model.StatusOpen}

	// Emit twice for the SAME edge — a double-write / reconnect replay.
	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-step", "gd-plan", "agent-x"), "oidc:op@x")
	s.maybeEmitPlanStepAdded(context.Background(), stepEdge("gd-step", "gd-plan", "agent-x"), "oidc:op@x")
	time.Sleep(150 * time.Millisecond)

	info, err := js.StreamInfo(eventbus.StreamPlanEvents)
	if err != nil {
		t.Fatalf("StreamInfo PLAN_EVENTS: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("PLAN_EVENTS has %d messages after two same-edge emits, want 1 (dedupe)", info.State.Msgs)
	}

	// The persisted message is the well-formed, self-describing payload the
	// consumer reactor reads.
	sub, err := js.SubscribeSync(planStepAddedSubject, nats.DeliverAll())
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck // test cleanup
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected the durable step-added message, got: %v", err)
	}
	var got events.PlanStepAdded
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("durable payload does not decode as PlanStepAdded: %v", err)
	}
	if got.PlanID != "gd-plan" || got.StepID != "gd-step" || got.StepTitle != "Durable step" {
		t.Errorf("durable payload = %+v, want plan=gd-plan step=gd-step title='Durable step'", got)
	}
	if !strings.Contains(string(msg.Data), "oidc:op@x") {
		t.Errorf("durable payload should carry the operator, got %q", string(msg.Data))
	}
}

func TestPlanStepDedupeID_StableAndDistinct(t *testing.T) {
	a := planStepDedupeID("gd-plan", "gd-step")
	if a != planStepDedupeID("gd-plan", "gd-step") {
		t.Error("dedupe id is not stable for the same (plan, step)")
	}
	if a == planStepDedupeID("gd-plan", "gd-step2") {
		t.Error("dedupe id collides across distinct steps")
	}
	if a == planStepDedupeID("gd-plan2", "gd-step") {
		t.Error("dedupe id collides across distinct plans")
	}
}
