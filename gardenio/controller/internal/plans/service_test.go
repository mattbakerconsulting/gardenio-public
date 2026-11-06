package plans

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planagent"
)

// fakeDaemon is an in-memory Daemon: beads by id, parent-child edges by
// parent, and a log of every write in call order.
type fakeDaemon struct {
	beads    map[string]*beadsapi.BeadDetail
	children map[string][]string // parent id -> child ids
	writes   []string
	nextID   int

	getErr        error // returned by GetBead for any id
	createErr     error
	linkErr       error
	closeErr      error
	reorderErr    error // returned by every ReorderBeadArrayField
	reorderCalls  int
	reorderNext   []string
	operatorStamp []string // operator on ctx at each create
}

func newFakeDaemon(beads ...*beadsapi.BeadDetail) *fakeDaemon {
	f := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}, children: map[string][]string{}}
	for _, b := range beads {
		f.beads[b.ID] = b
	}
	return f
}

func (f *fakeDaemon) link(child, parent string) {
	f.children[parent] = append(f.children[parent], child)
}

func (f *fakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.beads[id]
	if !ok {
		return nil, &beadsapi.APIError{StatusCode: 404, Message: "not found"}
	}
	return b, nil
}

func (f *fakeDaemon) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	out := map[string]*beadsapi.BeadDetail{}
	for _, id := range ids {
		if b, ok := f.beads[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

func (f *fakeDaemon) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	var deps []beadsapi.Dependency
	for _, c := range f.children[id] {
		deps = append(deps, beadsapi.Dependency{BeadID: c, DependsOnID: id, Type: "parent-child"})
	}
	return deps, nil
}

func (f *fakeDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.operatorStamp = append(f.operatorStamp, beadsapi.OperatorFromContext(ctx))
	if f.createErr != nil {
		return "", f.createErr
	}
	f.nextID++
	id := "gd-new" + string(rune('0'+f.nextID))
	f.beads[id] = &beadsapi.BeadDetail{ID: id, Title: req.Title, Type: req.Type, Labels: req.Labels, Status: "open"}
	f.writes = append(f.writes, "create "+req.Type+" "+req.Title+" "+strings.Join(req.Labels, ","))
	return id, nil
}

func (f *fakeDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	if f.linkErr != nil {
		return f.linkErr
	}
	f.link(beadID, dependsOnID)
	f.writes = append(f.writes, "link "+beadID+" "+depType+" "+dependsOnID)
	return nil
}

func (f *fakeDaemon) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	switch {
	case req.Status != nil:
		f.writes = append(f.writes, "status "+beadID+" "+*req.Status)
	case req.Title != nil:
		f.writes = append(f.writes, "title "+beadID+" "+*req.Title)
	case req.Description != nil:
		f.writes = append(f.writes, "description "+beadID)
	}
	return nil
}

func (f *fakeDaemon) UpdateBeadFieldsTyped(_ context.Context, beadID string, fields map[string]any) error {
	for k, v := range fields {
		f.writes = append(f.writes, "field "+beadID+" "+k+"="+v.(string))
	}
	return nil
}

func (f *fakeDaemon) AppendBeadArrayField(_ context.Context, beadID, field, element string) error {
	f.writes = append(f.writes, "append "+beadID+" "+field+" "+element)
	return nil
}

func (f *fakeDaemon) ReorderBeadArrayField(_ context.Context, beadID, field string, _, next []string) error {
	f.reorderCalls++
	if f.reorderErr != nil {
		return f.reorderErr
	}
	f.reorderNext = next
	f.writes = append(f.writes, "reorder "+beadID+" "+field+" "+strings.Join(next, ","))
	return nil
}

func (f *fakeDaemon) CloseBeadAs(_ context.Context, beadID, actor string, _ map[string]string) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	f.writes = append(f.writes, "close "+beadID+" by "+actor)
	return nil
}

func (f *fakeDaemon) AddComment(_ context.Context, beadID, _, _ string) error {
	f.writes = append(f.writes, "comment "+beadID)
	return nil
}

func (f *fakeDaemon) AddLabel(_ context.Context, beadID, label string) error {
	f.writes = append(f.writes, "label+ "+beadID+" "+label)
	return nil
}

func (f *fakeDaemon) RemoveLabel(_ context.Context, beadID, label string) error {
	f.writes = append(f.writes, "label- "+beadID+" "+label)
	return nil
}

// fakeGardeners records gardener calls into the same write log as the daemon,
// so a test can assert the order across both.
type fakeGardeners struct {
	d         *fakeDaemon
	gardener  *beadsapi.BeadDetail
	enableErr error
}

func (g *fakeGardeners) PlanGardener(context.Context, string) *beadsapi.BeadDetail { return g.gardener }

func (g *fakeGardeners) SetGardenerEnabled(_ context.Context, gardener *beadsapi.BeadDetail, enable bool, _ string) error {
	if g.enableErr != nil {
		return g.enableErr
	}
	if enable {
		g.d.writes = append(g.d.writes, "gardener on "+gardener.ID)
	} else {
		g.d.writes = append(g.d.writes, "gardener off "+gardener.ID)
	}
	return nil
}

func (g *fakeGardeners) BindGardener(_ context.Context, epic *beadsapi.BeadDetail, _ string) {
	g.d.writes = append(g.d.writes, "bind "+epic.ID)
}

type fakeAssistant struct{ seeds []string }

func (a *fakeAssistant) Prompt(_ context.Context, _ *beadsapi.BeadDetail, _, seed string) (planagent.DeliveryOutcome, string, error) {
	a.seeds = append(a.seeds, seed)
	return planagent.DeliverySent, "gardener-1", nil
}

var testActor = Actor{Email: "Op@acme.example", Operator: "oidc:op@acme.example"}

func newTestService(d *fakeDaemon) (*Service, *fakeGardeners, *fakeAssistant) {
	g := &fakeGardeners{d: d}
	a := &fakeAssistant{}
	return &Service{Daemon: d, Gardeners: g, Assistant: a, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, g, a
}

func planBead(fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}, Fields: fields}
}

func wantKind(t *testing.T, err error, want Kind) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want kind %d", want)
	}
	if got := KindOf(err); got != want {
		t.Fatalf("KindOf(%v) = %d, want %d", err, got, want)
	}
}

func TestAddStepParentsToPhaseAndStampsOperator(t *testing.T) {
	d := newFakeDaemon(planBead(map[string]string{PhaseOrderField: `["gd-ph1"]`}),
		&beadsapi.BeadDetail{ID: "gd-ph1", Type: "epic", Labels: []string{"project:lands"}})
	svc, _, _ := newTestService(d)

	id, err := svc.AddStep(context.Background(), "gd-plan", "  write it  ", "gd-ph1", testActor)
	if err != nil {
		t.Fatalf("AddStep: %v", err)
	}
	// The project label comes from the phase, not the plan, and the edge goes to
	// the phase.
	want := []string{"create task write it project:lands", "link " + id + " parent-child gd-ph1"}
	if !slices.Equal(d.writes, want) {
		t.Fatalf("writes = %q, want %q", d.writes, want)
	}
	if d.operatorStamp[0] != testActor.Operator {
		t.Fatalf("CreateBead ctx operator = %q, want %q", d.operatorStamp[0], testActor.Operator)
	}
}

func TestAddStepRejections(t *testing.T) {
	d := newFakeDaemon(planBead(nil))
	svc, _, _ := newTestService(d)
	ctx := context.Background()

	_, err := svc.AddStep(ctx, "gd-missing", "x", "", testActor)
	wantKind(t, err, KindNotFound)
	_, err = svc.AddStep(ctx, "gd-plan", "   ", "", testActor)
	wantKind(t, err, KindInvalid)
	_, err = svc.AddStep(ctx, "gd-plan", strings.Repeat("x", MaxStepTitleLen+1), "", testActor)
	wantKind(t, err, KindTooLarge)
	_, err = svc.AddStep(ctx, "gd-plan", "x", "gd-stray", testActor)
	wantKind(t, err, KindInvalid)
	if !strings.Contains(Message(err), "not in phase_order") {
		t.Fatalf("phase rejection message = %q", Message(err))
	}
	if len(d.writes) != 0 {
		t.Fatalf("rejected adds wrote %q", d.writes)
	}

	d.linkErr = errors.New("boom")
	id, err := svc.AddStep(ctx, "gd-plan", "x", "", testActor)
	wantKind(t, err, KindUpstream)
	if id == "" || Message(err) != "Step created but could not be linked to the plan." {
		t.Fatalf("link failure: id=%q msg=%q", id, Message(err))
	}
}

func TestRetitleStepChecksMembershipBeforeTitle(t *testing.T) {
	d := newFakeDaemon(planBead(nil),
		&beadsapi.BeadDetail{ID: "gd-s1", Type: "task"},
		&beadsapi.BeadDetail{ID: "gd-other", Type: "task"})
	d.link("gd-s1", "gd-plan")
	svc, _, _ := newTestService(d)
	ctx := context.Background()

	wantKind(t, svc.RetitleStep(ctx, "gd-plan", " ", "t", testActor), KindBadRequest)
	// Not a step of this plan: a not-found even with an empty title.
	wantKind(t, svc.RetitleStep(ctx, "gd-plan", "gd-other", "", testActor), KindNotFound)
	wantKind(t, svc.RetitleStep(ctx, "gd-plan", "gd-s1", "", testActor), KindInvalid)
	if err := svc.RetitleStep(ctx, "gd-plan", "gd-s1", " new ", testActor); err != nil {
		t.Fatalf("RetitleStep: %v", err)
	}
	if !slices.Equal(d.writes, []string{"title gd-s1 new"}) {
		t.Fatalf("writes = %q", d.writes)
	}
}

func TestReorderStepSeedsFromRenderedOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	d := newFakeDaemon(planBead(nil),
		&beadsapi.BeadDetail{ID: "gd-a", Type: "task", CreatedAt: t0},
		&beadsapi.BeadDetail{ID: "gd-b", Type: "task", CreatedAt: t0.Add(time.Hour)},
		&beadsapi.BeadDetail{ID: "gd-c", Type: "task", CreatedAt: t0.Add(2 * time.Hour)})
	for _, id := range []string{"gd-c", "gd-a", "gd-b"} {
		d.link(id, "gd-plan")
	}
	svc, _, _ := newTestService(d)
	ctx := context.Background()

	wantKind(t, svc.ReorderStep(ctx, "gd-plan", "gd-b", "sideways", testActor), KindBadRequest)
	if err := svc.ReorderStep(ctx, "gd-plan", "gd-b", "up", testActor); err != nil {
		t.Fatalf("ReorderStep: %v", err)
	}
	// A never-reordered group seeds from creation order and writes every sibling.
	if want := []string{"gd-b", "gd-a", "gd-c"}; !slices.Equal(d.reorderNext, want) {
		t.Fatalf("next = %q, want %q", d.reorderNext, want)
	}

	// A boundary move is a no-op, not an error.
	d.reorderCalls = 0
	if err := svc.ReorderStep(ctx, "gd-plan", "gd-c", "down", testActor); err != nil || d.reorderCalls != 0 {
		t.Fatalf("boundary move: err=%v calls=%d", err, d.reorderCalls)
	}
}

func TestReorderConflictRetriesThenReportsConflict(t *testing.T) {
	d := newFakeDaemon(planBead(map[string]string{PhaseOrderField: `["gd-p1","gd-p2"]`}),
		&beadsapi.BeadDetail{ID: "gd-p1", Type: "epic"},
		&beadsapi.BeadDetail{ID: "gd-p2", Type: "epic"})
	d.link("gd-p1", "gd-plan")
	d.link("gd-p2", "gd-plan")
	d.reorderErr = &beadsapi.APIError{StatusCode: 409, Message: "stale"}
	svc, _, _ := newTestService(d)

	err := svc.ReorderPhase(context.Background(), "gd-plan", "gd-p2", "up", testActor)
	wantKind(t, err, KindConflict)
	if d.reorderCalls != MaxCASReorderAttempts {
		t.Fatalf("CAS attempts = %d, want %d", d.reorderCalls, MaxCASReorderAttempts)
	}

	d.reorderErr = errors.New("daemon down")
	err = svc.ReorderPhase(context.Background(), "gd-plan", "gd-p2", "up", testActor)
	wantKind(t, err, KindUpstream)
	if Message(err) != "Failed to reorder phase." {
		t.Fatalf("message = %q", Message(err))
	}
}

func TestCreatePhaseLinksThenAppends(t *testing.T) {
	d := newFakeDaemon(planBead(nil))
	svc, _, _ := newTestService(d)

	id, err := svc.CreatePhase(context.Background(), "gd-plan", "Research", testActor)
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	want := []string{
		"create epic Research project:gardenio",
		"link " + id + " parent-child gd-plan",
		"append gd-plan phase_order " + id,
	}
	if !slices.Equal(d.writes, want) {
		t.Fatalf("writes = %q, want %q", d.writes, want)
	}
}

func TestEditDocOnlyRewritesBundleDocs(t *testing.T) {
	d := newFakeDaemon(planBead(nil),
		&beadsapi.BeadDetail{ID: "gd-bundle", Type: "bundle"},
		&beadsapi.BeadDetail{ID: "gd-doc", Type: "doc", Fields: map[string]string{"format": "markdown"}},
		&beadsapi.BeadDetail{ID: "gd-img", Type: "doc", Fields: map[string]string{"format": "image"}},
		&beadsapi.BeadDetail{ID: "gd-stray", Type: "doc"})
	d.link("gd-bundle", "gd-plan")
	d.link("gd-doc", "gd-bundle")
	d.link("gd-img", "gd-bundle")
	svc, _, _ := newTestService(d)
	ctx := context.Background()

	wantKind(t, svc.EditDoc(ctx, "gd-plan", "gd-stray", "x", testActor), KindNotFound)
	wantKind(t, svc.EditDoc(ctx, "gd-plan", "gd-img", "x", testActor), KindInvalid)
	wantKind(t, svc.EditDoc(ctx, "gd-plan", "gd-doc", strings.Repeat("x", MaxDocLen+1), testActor), KindTooLarge)
	if err := svc.EditDoc(ctx, "gd-plan", "gd-doc", "body", testActor); err != nil {
		t.Fatalf("EditDoc: %v", err)
	}
	if !slices.Equal(d.writes, []string{"field gd-doc content=body"}) {
		t.Fatalf("writes = %q", d.writes)
	}
}

func TestDispatchStepRefusesDoneStep(t *testing.T) {
	d := newFakeDaemon(planBead(nil),
		&beadsapi.BeadDetail{ID: "gd-s1", Type: "task", Status: "open", Title: "Do it"},
		&beadsapi.BeadDetail{ID: "gd-s2", Type: "task", Status: "closed"})
	svc, _, a := newTestService(d)
	ctx := context.Background()

	_, err := svc.DispatchStep(ctx, "gd-plan", "gd-s2", testActor)
	wantKind(t, err, KindRefused)
	got, err := svc.DispatchStep(ctx, "gd-plan", "gd-s1", testActor)
	if err != nil {
		t.Fatalf("DispatchStep: %v", err)
	}
	if got.Outcome != planagent.DeliverySent || got.Gardener != "gardener-1" {
		t.Fatalf("dispatch = %+v", got)
	}
	if len(a.seeds) != 1 || !strings.Contains(a.seeds[0], `step gd-s1 ("Do it") now`) {
		t.Fatalf("seeds = %q", a.seeds)
	}
}

func TestMarkTogglesLabelAndBindsOnPromotion(t *testing.T) {
	d := newFakeDaemon(&beadsapi.BeadDetail{ID: "gd-epic", Type: "epic"}, planBead(nil))
	svc, _, _ := newTestService(d)
	ctx := context.Background()

	if err := svc.Mark(ctx, "gd-epic", testActor); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := svc.Mark(ctx, "gd-plan", testActor); err != nil {
		t.Fatalf("unmark: %v", err)
	}
	want := []string{"label+ gd-epic plan", "bind gd-epic", "label- gd-plan plan"}
	if !slices.Equal(d.writes, want) {
		t.Fatalf("writes = %q, want %q", d.writes, want)
	}
}

func TestSetState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cur       string
		requested State
		kind      Kind // 0 with wantWrite "" means no error
		wantWrite string
	}{
		{name: "toggle draft to ready", cur: "draft", wantWrite: "field gd-plan plan_state=ready"},
		{name: "toggle ready to draft", cur: "ready", wantWrite: "field gd-plan plan_state=draft"},
		{name: "unset toggles to ready", cur: "", wantWrite: "field gd-plan plan_state=ready"},
		{name: "explicit no-op", cur: "ready", requested: StateReady},
		{name: "building refused", cur: "building", requested: StateDraft, kind: KindRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon(planBead(map[string]string{StateField: tc.cur}))
			svc, _, _ := newTestService(d)
			err := svc.SetState(context.Background(), "gd-plan", tc.requested, testActor)
			if tc.kind != 0 {
				wantKind(t, err, tc.kind)
			} else if err != nil {
				t.Fatalf("SetState: %v", err)
			}
			var want []string
			if tc.wantWrite != "" {
				want = []string{tc.wantWrite, "comment gd-plan"}
			}
			if !slices.Equal(d.writes, want) {
				t.Fatalf("writes = %q, want %q", d.writes, want)
			}
		})
	}
}

func TestBuild(t *testing.T) {
	ctx := context.Background()

	d := newFakeDaemon(planBead(map[string]string{StateField: "draft"}))
	svc, g, _ := newTestService(d)
	wantKind(t, svc.Build(ctx, "gd-plan", testActor), KindRefused)

	d.beads["gd-plan"].Fields[StateField] = "ready"
	wantKind(t, svc.Build(ctx, "gd-plan", testActor), KindConflict)

	// State first, then the enable: a failed enable leaves building + paused.
	g.gardener = &beadsapi.BeadDetail{ID: "gd-g"}
	if err := svc.Build(ctx, "gd-plan", testActor); err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{"field gd-plan plan_state=building", "comment gd-plan", "gardener on gd-g"}
	if !slices.Equal(d.writes, want) {
		t.Fatalf("writes = %q, want %q", d.writes, want)
	}

	d.beads["gd-plan"].Fields[StateField] = "ready"
	g.enableErr = errors.New("Gardener is closed")
	err := svc.Build(ctx, "gd-plan", testActor)
	wantKind(t, err, KindUpstream)
	if !strings.Contains(Message(err), "starting the gardener failed: Gardener is closed") {
		t.Fatalf("message = %q", Message(err))
	}
}

func TestRetireDisablesThenStampsThenCloses(t *testing.T) {
	d := newFakeDaemon(planBead(map[string]string{StateField: "building"}))
	svc, g, _ := newTestService(d)
	g.gardener = &beadsapi.BeadDetail{ID: "gd-g"}

	if err := svc.Abandon(context.Background(), "gd-plan", testActor); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	want := []string{"gardener off gd-g", "field gd-plan plan_state=abandoned", "comment gd-plan", "close gd-plan by " + testActor.Operator}
	if !slices.Equal(d.writes, want) {
		t.Fatalf("writes = %q, want %q", d.writes, want)
	}

	d.writes = nil
	d.closeErr = errors.New("boom")
	err := svc.Complete(context.Background(), "gd-plan", testActor)
	wantKind(t, err, KindUpstream)
	if Message(err) != "Marked complete, but closing the plan failed — try again." {
		t.Fatalf("message = %q", Message(err))
	}
}

func TestTerminalVerbsAreIdempotent(t *testing.T) {
	d := newFakeDaemon(planBead(map[string]string{StateField: "complete"}))
	d.beads["gd-plan"].Status = "closed"
	svc, _, _ := newTestService(d)
	ctx := context.Background()

	if err := svc.Complete(ctx, "gd-plan", testActor); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(d.writes) != 0 {
		t.Fatalf("re-complete wrote %q", d.writes)
	}
	if err := svc.Reopen(ctx, "gd-plan", testActor); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	want := []string{"status gd-plan open", "field gd-plan plan_state=building", "comment gd-plan"}
	if !slices.Equal(d.writes, want) {
		t.Fatalf("writes = %q, want %q", d.writes, want)
	}
}

func TestPlanLoads(t *testing.T) {
	d := newFakeDaemon(&beadsapi.BeadDetail{ID: "gd-task", Type: "task"})
	svc, _, _ := newTestService(d)
	ctx := context.Background()

	// Strict (lifecycle) loads tell a 404 or a non-epic from a broken daemon.
	wantKind(t, svc.Complete(ctx, "gd-missing", testActor), KindNotFound)
	wantKind(t, svc.Complete(ctx, "gd-task", testActor), KindNotFound)
	d.getErr = errors.New("connection refused")
	wantKind(t, svc.Complete(ctx, "gd-plan", testActor), KindInternal)
	// The step and edit verbs answer any read failure as a not-found.
	wantKind(t, svc.EditNarrative(ctx, "gd-plan", "x", testActor), KindNotFound)
}

func TestKindOfAndMessageOnForeignErrors(t *testing.T) {
	err := errors.New("plain")
	if KindOf(err) != KindInternal || Message(err) != "Internal Server Error" {
		t.Fatalf("KindOf/Message on a plain error = %d/%q", KindOf(err), Message(err))
	}
	wrapped := errors.Join(errors.New("ctx"), fail(KindConflict, "busy", nil))
	if KindOf(wrapped) != KindConflict || Message(wrapped) != "busy" {
		t.Fatalf("KindOf/Message on a wrapped error = %d/%q", KindOf(wrapped), Message(wrapped))
	}
}
