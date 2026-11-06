package main

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// labelReactorServer builds a full Server over a fake daemon so reactToPlanLabel
// drives the REAL shared plangardener.Bind (not a stub) — the whole point of the
// reactor is that a raw-labeled plan gets the byte-identical gardener the viewer
// seams mint. No coopmux is needed (the bind never nudges), so a disabled screen
// client is fine.
func labelReactorServer(f *fakeDaemon) *Server {
	s := reactorTestServer(f, &fakeScreenClient{disabled: true})
	return s
}

// labelAddedEvent builds a beads.label.added SSE event (kbeads events.LabelAdded
// wire shape) naming the single label added to a bead.
func labelAddedEvent(t *testing.T, beadID, label string) beadsapi.SSEEvent {
	t.Helper()
	data, err := json.Marshal(map[string]any{"bead_id": beadID, "label": label})
	if err != nil {
		t.Fatalf("marshal label-added event: %v", err)
	}
	return beadsapi.SSEEvent{Event: plangardenerLabelAddedTopic, Data: data}
}

// beadUpdatedLabelsEvent builds a beads.bead.updated SSE event carrying a
// Changes map whose "labels" entry is the FULL new label slice (mirroring the
// daemon's UpdateBead publish), for a bead of the given type.
func beadUpdatedLabelsEvent(t *testing.T, beadID, beadType string, labels []string) beadsapi.SSEEvent {
	t.Helper()
	anyLabels := make([]any, len(labels))
	for i, l := range labels {
		anyLabels[i] = l
	}
	data, err := json.Marshal(map[string]any{
		"bead":    map[string]any{"id": beadID, "type": beadType},
		"changes": map[string]any{"labels": anyLabels},
	})
	if err != nil {
		t.Fatalf("marshal bead-updated event: %v", err)
	}
	return beadsapi.SSEEvent{Event: plangardenerBeadUpdatedTopic, Data: data}
}

// --- reactToPlanLabel unit tests --------------------------------------------

// AC(1): `kd label add <fresh-epic> plan` results in a bound gardener. A fresh
// plan epic (no plan_state → ungated → born ENABLED, parity with a legacy
// hand-mark) gets exactly one gardener minted through the shared core.
func TestReactToPlanLabel_BindsFreshEpic(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f) // persist the minted gardener so a re-churn adopts it
	f.createResp = "gd-newcap"
	// A fresh epic that just gained the `plan` label — no plan_state field.
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{
		ID: "gd-plan1", Type: "epic", Status: "in_progress",
		Title: "Ship the widget", Labels: []string{"project:gardenio", "plan"},
	}
	s := labelReactorServer(f)

	s.reactToPlanLabel(context.Background(), "gd-plan1")

	caps := gardenerCreates(f)
	if len(caps) != 1 {
		t.Fatalf("expected exactly one gardener trigger minted, got %d", len(caps))
	}
	// It carries the marker + epic-pin labels the shared core stamps.
	if !slices.Contains(caps[0].Labels, gardenerEpicLabelFor("gd-plan1")) ||
		!slices.Contains(caps[0].Labels, planGardenerMarkerLabel) {
		t.Errorf("bound gardener missing plan-gardener marker/epic labels; got %v", caps[0].Labels)
	}
	// Ungated (no plan_state) → born ENABLED, matching the legacy hand-mark path.
	fields := gardenerFieldsFromReq(t, caps[0])
	if enabled, _ := fields["enabled"].(bool); !enabled {
		t.Errorf("an ungated plan's gardener must be born enabled; enabled=%v", fields["enabled"])
	}
	// The epic here carries no operated_by, so attribution falls back to the
	// system-machine channel (see TestReactToPlanLabel_InheritsEpicHumanOperator
	// for the preferred path).
	if f.createLastSysOp != planLabelReactorSystemOperator {
		t.Errorf("gardener create system-operator = %q, want %q", f.createLastSysOp, planLabelReactorSystemOperator)
	}
	if f.createLastOpBy != "" {
		t.Errorf("an epic with no human must not stamp a human X-Operated-By; got %q", f.createLastOpBy)
	}
}

// Regression for ka-0000004529: a reactively-bound gardener MUST inherit the plan
// epic's human operator. The trigger engine reads the gardener trigger bead's
// operated_by to attribute its autonomous fires; a non-human value there
// (the old unconditional system:plan-label-autobind) makes every spawn_agent
// CreateBead fail closed with HTTP 400 "operator could not be resolved", so the
// gardener matches every cycle and spawns nothing — silently, since a fireAction
// error never lands on the trigger bead. Three live gardeners were dark this way.
func TestReactToPlanLabel_InheritsEpicHumanOperator(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-newcap"
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{
		ID: "gd-plan1", Type: "epic", Status: "in_progress",
		Title: "Human-authored plan", Labels: []string{"project:gardenio", "plan"},
		OperatedBy: "slack:U0OPERATOR1",
	}
	s := labelReactorServer(f)

	s.reactToPlanLabel(context.Background(), "gd-plan1")

	if caps := gardenerCreates(f); len(caps) != 1 {
		t.Fatalf("expected one gardener minted, got %d", len(caps))
	}
	if f.createLastOpBy != "slack:U0OPERATOR1" {
		t.Errorf("gardener create X-Operated-By = %q, want the epic's human slack:U0OPERATOR1 "+
			"(a non-human operated_by makes every spawn_agent fire 400)", f.createLastOpBy)
	}
	if f.createLastSysOp != "" {
		t.Errorf("an epic WITH a human must not also ride the system-machine channel; got %q", f.createLastSysOp)
	}
}

// The inherit is gated on IsHumanOperator, not on non-emptiness: an epic whose
// operated_by is itself a sentinel (system:*, a bot alias) must NOT propagate
// that value onto the gardener — doing so would re-create the exact 400 this
// change fixes. It falls back to the system-machine channel instead.
func TestReactToPlanLabel_SentinelEpicOperatorNotInherited(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-newcap"
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{
		ID: "gd-plan1", Type: "epic", Status: "in_progress",
		Title: "Machine-authored plan", Labels: []string{"project:gardenio", "plan"},
		OperatedBy: "system:plan-label-autobind",
	}
	s := labelReactorServer(f)

	s.reactToPlanLabel(context.Background(), "gd-plan1")

	if caps := gardenerCreates(f); len(caps) != 1 {
		t.Fatalf("expected one gardener minted, got %d", len(caps))
	}
	if f.createLastOpBy != "" {
		t.Errorf("a sentinel epic operator must never be stamped as a human; got %q", f.createLastOpBy)
	}
	if f.createLastSysOp != planLabelReactorSystemOperator {
		t.Errorf("gardener create system-operator = %q, want %q", f.createLastSysOp, planLabelReactorSystemOperator)
	}
}

// AC(3): a draft/ready plan's gardener is born PAUSED behind the approve→build
// gate (parity with EnabledOnBind / the POST /plans seam), so the scheduler
// never fires an unapproved plan bound through the reactor.
func TestReactToPlanLabel_DraftBornPaused(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-newcap"
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{
		ID: "gd-plan1", Type: "epic", Status: "in_progress",
		Title: "Draft plan", Labels: []string{"project:gardenio", "plan"},
		Fields: map[string]string{planStateFieldKey: "draft"},
	}
	s := labelReactorServer(f)

	s.reactToPlanLabel(context.Background(), "gd-plan1")

	caps := gardenerCreates(f)
	if len(caps) != 1 {
		t.Fatalf("expected one gardener minted, got %d", len(caps))
	}
	fields := gardenerFieldsFromReq(t, caps[0])
	if enabled, _ := fields["enabled"].(bool); enabled {
		t.Errorf("a draft plan's gardener must be born PAUSED (enabled=false); enabled=%v", fields["enabled"])
	}
}

// AC(2): idempotent — a second label churn on a plan that ALREADY has a gardener
// adopts it, minting no duplicate.
func TestReactToPlanLabel_IdempotentAdopt(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
	s := labelReactorServer(f)

	s.reactToPlanLabel(context.Background(), "gd-plan1")

	if caps := gardenerCreates(f); len(caps) != 0 {
		t.Errorf("a plan with a bound gardener must adopt, not mint a duplicate; minted %d", len(caps))
	}
}

// A bead that is not a type=epic is never bound — the plan-shape guard
// fail-closes even if some event surfaced its id (a bead-updated race on a
// non-epic, a mislabeled task).
func TestReactToPlanLabel_NonEpicSkipped(t *testing.T) {
	f := newFake()
	f.beads["gd-task1"] = &beadsapi.BeadDetail{
		ID: "gd-task1", Type: "task", Status: "open",
		Labels: []string{"project:gardenio", "plan"},
	}
	s := labelReactorServer(f)

	s.reactToPlanLabel(context.Background(), "gd-task1")

	if caps := gardenerCreates(f); len(caps) != 0 {
		t.Errorf("a non-epic must never bind a gardener; minted %d", len(caps))
	}
}

// An epic whose `plan` label was removed again before the reactor's re-fetch
// resolves to no bind — the guard reads the FRESH bead, not the (stale) event.
func TestReactToPlanLabel_LabelGoneBeforeFetch(t *testing.T) {
	f := newFake()
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{
		ID: "gd-plan1", Type: "epic", Status: "in_progress",
		Labels: []string{"project:gardenio"}, // `plan` already removed
	}
	s := labelReactorServer(f)

	s.reactToPlanLabel(context.Background(), "gd-plan1")

	if caps := gardenerCreates(f); len(caps) != 0 {
		t.Errorf("an epic lacking the plan label must not bind; minted %d", len(caps))
	}
}

// A fetch miss (bead deleted between event and read) is a silent no-op, never a
// panic or a spurious bind.
func TestReactToPlanLabel_FetchMiss(t *testing.T) {
	f := newFake()
	s := labelReactorServer(f)

	s.reactToPlanLabel(context.Background(), "gd-nonexistent")

	if caps := gardenerCreates(f); len(caps) != 0 {
		t.Errorf("a fetch miss must not bind; minted %d", len(caps))
	}
}

// --- dispatch filter unit tests ---------------------------------------------

// recordingBinder captures the bead ids reactToPlanLabel is called with, so the
// dispatch filter can be asserted without the full bind machinery.
type recordingBinder struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingBinder) reactToPlanLabel(_ context.Context, beadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, beadID)
}

func (r *recordingBinder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// dispatchReactor builds a PlanLabelReactor over a recording binder (no daemon
// stream needed — dispatch is called directly).
func dispatchReactor(b planGardenerBinder) *PlanLabelReactor {
	return &PlanLabelReactor{binder: b, logger: testLogger()}
}

// A beads.label.added event for the `plan` label hands the bead id to the
// binder; any OTHER label is dropped before any binder call.
func TestDispatch_LabelAddedPlanOnly(t *testing.T) {
	b := &recordingBinder{}
	p := dispatchReactor(b)
	ctx := context.Background()

	p.dispatch(ctx, labelAddedEvent(t, "gd-plan1", "plan"))
	p.dispatch(ctx, labelAddedEvent(t, "gd-plan2", "project:gardenio"))
	p.dispatch(ctx, labelAddedEvent(t, "gd-plan3", "role:gardener"))

	if got := b.got(); !slices.Equal(got, []string{"gd-plan1"}) {
		t.Errorf("only the plan-label add should dispatch; got %v", got)
	}
}

// A beads.bead.updated event whose Changes labels newly contain `plan` on an
// EPIC dispatches; the same on a non-epic, or without `plan` in the labels, or
// with no labels change, is dropped.
func TestDispatch_BeadUpdatedLabelsGate(t *testing.T) {
	b := &recordingBinder{}
	p := dispatchReactor(b)
	ctx := context.Background()

	// epic + labels contain plan → dispatch.
	p.dispatch(ctx, beadUpdatedLabelsEvent(t, "gd-epicplan", "epic", []string{"project:gardenio", "plan"}))
	// epic + labels change but NO plan → drop.
	p.dispatch(ctx, beadUpdatedLabelsEvent(t, "gd-ea2hr5hlnm", "epic", []string{"project:gardenio", "area:ui"}))
	// non-epic + labels contain plan → drop (only an epic can be a plan).
	p.dispatch(ctx, beadUpdatedLabelsEvent(t, "gd-taskplan", "task", []string{"plan"}))
	// epic + a non-labels field change (no "labels" key) → drop.
	statusOnly, _ := json.Marshal(map[string]any{
		"bead":    map[string]any{"id": "gd-trgekdoqfc", "type": "epic"},
		"changes": map[string]any{"status": "in_progress"},
	})
	p.dispatch(ctx, beadsapi.SSEEvent{Event: plangardenerBeadUpdatedTopic, Data: statusOnly})

	if got := b.got(); !slices.Equal(got, []string{"gd-epicplan"}) {
		t.Errorf("only the epic gaining the plan label should dispatch; got %v", got)
	}
}

// Malformed payloads on either topic never panic and never dispatch.
func TestDispatch_MalformedPayloadIgnored(t *testing.T) {
	b := &recordingBinder{}
	p := dispatchReactor(b)
	ctx := context.Background()

	p.dispatch(ctx, beadsapi.SSEEvent{Event: plangardenerLabelAddedTopic, Data: json.RawMessage("not json")})
	p.dispatch(ctx, beadsapi.SSEEvent{Event: plangardenerBeadUpdatedTopic, Data: json.RawMessage("{")})
	// label.added with an empty bead id → dropped.
	p.dispatch(ctx, labelAddedEvent(t, "", "plan"))
	// bead.updated with a nil bead → dropped.
	nilBead, _ := json.Marshal(map[string]any{"changes": map[string]any{"labels": []any{"plan"}}})
	p.dispatch(ctx, beadsapi.SSEEvent{Event: plangardenerBeadUpdatedTopic, Data: nilBead})
	// An unrelated topic on the same stream → dropped by the switch default.
	p.dispatch(ctx, beadsapi.SSEEvent{Event: "beads.bead.created", Data: json.RawMessage("{}")})

	if got := b.got(); len(got) != 0 {
		t.Errorf("malformed / empty / unrelated events must not dispatch; got %v", got)
	}
}

// changesLabelsContainPlan handles every value shape without panicking.
func TestChangesLabelsContainPlan(t *testing.T) {
	cases := []struct {
		name    string
		changes map[string]any
		want    bool
	}{
		{"nil map", nil, false},
		{"no labels key", map[string]any{"status": "open"}, false},
		{"labels contains plan", map[string]any{"labels": []any{"project:x", "plan"}}, true},
		{"labels without plan", map[string]any{"labels": []any{"project:x"}}, false},
		{"labels wrong element type", map[string]any{"labels": []any{123, true}}, false},
		{"labels not a slice", map[string]any{"labels": "plan"}, false},
		{"labels null", map[string]any{"labels": nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := changesLabelsContainPlan(tc.changes); got != tc.want {
				t.Errorf("changesLabelsContainPlan(%v) = %v, want %v", tc.changes, got, tc.want)
			}
		})
	}
}

// --- end-to-end pump test ----------------------------------------------------

// The reactor, driven through its real upstream pump off a live SSE channel,
// binds a gardener for a plan epic when a beads.label.added event arrives — the
// full path from stream to bind. Also proves the reactor subscribes to BOTH
// topics.
func TestPlanLabelReactor_EndToEndBindsOnLabelAdded(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-newcap"
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{
		ID: "gd-plan1", Type: "epic", Status: "in_progress",
		Title: "Ship it", Labels: []string{"project:gardenio", "plan"},
	}
	ch := make(chan beadsapi.SSEEvent, 1)
	f.eventStreamCh = ch
	s := labelReactorServer(f)

	p := NewPlanLabelReactor(f, s, testLogger())
	if p == nil {
		t.Fatal("NewPlanLabelReactor returned nil for a wired daemon+binder")
	}
	p.Start()
	defer p.Close()

	ch <- labelAddedEvent(t, "gd-plan1", "plan")

	// Wait (bounded) for the bind to land — the pump dispatches on a background
	// goroutine, so poll the mintMu-guarded snapshot (NOT gardenerCreates, which
	// reads f.createReqs unlocked) to stay race-clean under `go test -race`.
	deadline := time.Now().Add(2 * time.Second)
	triggerCreates := func() int {
		n := 0
		for _, req := range f.createReqsSnapshot() {
			if req.Type == "trigger" {
				n++
			}
		}
		return n
	}
	for {
		if triggerCreates() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected one gardener bound via the pump within 2s, got %d", triggerCreates())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The reactor subscribed to both label-mutation topics on one stream. Safe
	// to read plainly here: eventStreamLastTopics is written in the pump
	// goroutine BEFORE any CreateBead, so the snapshot's mintMu acquire above
	// established a happens-before edge to this read.
	if f.eventStreamLastTopics != planLabelReactorTopics {
		t.Errorf("reactor subscribed to %q, want %q", f.eventStreamLastTopics, planLabelReactorTopics)
	}
}

// NewPlanLabelReactor returns nil (a safe no-op) when unwired, and its methods
// tolerate a nil receiver.
func TestNewPlanLabelReactor_NilSafe(t *testing.T) {
	if NewPlanLabelReactor(nil, &recordingBinder{}, testLogger()) != nil {
		t.Error("nil daemon should yield a nil reactor")
	}
	if NewPlanLabelReactor(newFake(), nil, testLogger()) != nil {
		t.Error("nil binder should yield a nil reactor")
	}
	var p *PlanLabelReactor
	p.Start() // must not panic
	p.Close() // must not panic
}
