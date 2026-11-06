package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// ── pure-helper tests (no daemon) ────────────────────────────────────────────

func TestPlanTitleFromGoal_CollapsesAndTruncates(t *testing.T) {
	if got := planTitleFromGoal("  ship\n the   thing  "); got != "ship the thing" {
		t.Errorf("planTitleFromGoal = %q, want %q", got, "ship the thing")
	}
	long := strings.Repeat("x", maxPlanTitleLen+50)
	got := planTitleFromGoal(long)
	if len(got) != maxPlanTitleLen {
		t.Errorf("planTitleFromGoal length = %d, want %d", len(got), maxPlanTitleLen)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("planTitleFromGoal(long) = %q, want an ellipsis suffix", got)
	}
}

// TestPlanCreateIdempotencyKey mirrors the viewer's key: stable per
// (goal,project,actor), and any of the three changing changes the key.
func TestPlanCreateIdempotencyKey(t *testing.T) {
	base := planCreateIdempotencyKey("goal", "gardenio", "matt@x.io")
	if !strings.HasPrefix(base, "plan-create:") {
		t.Errorf("key = %q, want a plan-create: prefix", base)
	}
	if again := planCreateIdempotencyKey("goal", "gardenio", "matt@x.io"); again != base {
		t.Errorf("key not stable: %q != %q", again, base)
	}
	for name, k := range map[string]string{
		"goal":    planCreateIdempotencyKey("other", "gardenio", "matt@x.io"),
		"project": planCreateIdempotencyKey("goal", "coop", "matt@x.io"),
		"actor":   planCreateIdempotencyKey("goal", "gardenio", "other@x.io"),
	} {
		if k == base {
			t.Errorf("key must change when %s changes, but it did not", name)
		}
	}
}

// TestCanonicalPlanOperator matches the viewer's canonicalOIDC: lowercase, add
// the oidc: prefix if absent, leave slack: untouched, blank stays blank.
func TestCanonicalPlanOperator(t *testing.T) {
	cases := map[string]string{
		"Matt@X.io":      "oidc:matt@x.io",
		"oidc:Matt@X.io": "oidc:matt@x.io",
		"slack:U07K1RLQ": "slack:U07K1RLQ",
		"  matt@x.io  ":  "oidc:matt@x.io",
		"":               "",
	}
	for in, want := range cases {
		if got := canonicalPlanOperator(in); got != want {
			t.Errorf("canonicalPlanOperator(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanProjectLabel(t *testing.T) {
	if got := planProjectLabel([]string{"plan", "project:gardenio"}); got != "gardenio" {
		t.Errorf("planProjectLabel = %q, want gardenio", got)
	}
	if got := planProjectLabel([]string{"plan"}); got != "" {
		t.Errorf("planProjectLabel(no project) = %q, want empty", got)
	}
	if got := planProjectLabel(nil); got != "" {
		t.Errorf("planProjectLabel(nil) = %q, want empty", got)
	}
}

// TestReorderPhaseResolved covers the raw-neighbor case, the resolved-subset
// splice that pins a stale interleaved id (ka-0000002075), and the boundary/
// absent no-ops.
func TestReorderPhaseResolved(t *testing.T) {
	// Raw case (resolved == order): swap neighbors.
	order := []string{"gd-a", "gd-b", "gd-c"}
	if got, ok := reorderPhaseResolved(order, order, "gd-b", "up"); !ok || !reflect.DeepEqual(got, []string{"gd-b", "gd-a", "gd-c"}) {
		t.Errorf("up over raw = %v (ok=%v), want [gd-b gd-a gd-c]", got, ok)
	}
	if got, ok := reorderPhaseResolved(order, order, "gd-b", "down"); !ok || !reflect.DeepEqual(got, []string{"gd-a", "gd-c", "gd-b"}) {
		t.Errorf("down over raw = %v (ok=%v), want [gd-a gd-c gd-b]", got, ok)
	}
	// Boundary + absent no-ops.
	if _, ok := reorderPhaseResolved(order, order, "gd-a", "up"); ok {
		t.Error("up at front boundary should be a no-op")
	}
	if _, ok := reorderPhaseResolved(order, order, "gd-c", "down"); ok {
		t.Error("down at end boundary should be a no-op")
	}
	if _, ok := reorderPhaseResolved(order, order, "gd-z", "up"); ok {
		t.Error("absent id should be a no-op")
	}
	// Stale-id splice: raw = [a, STALE, b]; resolved = [a, b]. Moving b up must
	// swap a and b in the RAW array (pinning STALE at index 1), not swap b with
	// its raw neighbor STALE.
	raw := []string{"gd-a", "gd-STALE", "gd-b"}
	resolved := []string{"gd-a", "gd-b"}
	got, ok := reorderPhaseResolved(raw, resolved, "gd-b", "up")
	if !ok || !reflect.DeepEqual(got, []string{"gd-b", "gd-STALE", "gd-a"}) {
		t.Errorf("stale-id splice = %v (ok=%v), want [gd-b gd-STALE gd-a]", got, ok)
	}
	// Input slice must not be mutated.
	if !reflect.DeepEqual(raw, []string{"gd-a", "gd-STALE", "gd-b"}) {
		t.Errorf("reorderPhaseResolved mutated its input: %v", raw)
	}
}

// ── fake planDaemonWriter ────────────────────────────────────────────────────

// opRecord captures a single mutating call the fake writer received, so a test
// can assert the sequence + shape of writes a verb makes (the CLI counterpart of
// the viewer tests' capturedRequest).
type opRecord struct {
	kind     string // "create" | "dep" | "append" | "reorder" | "update" | "fields" | "close" | "comment"
	bead     string
	operator string
	// create
	req beadsapi.CreateBeadRequest
	// dep
	depTo, depType string
	// append
	field, element string
	// reorder
	expected, next []string
	// update
	status string
	// fields
	fields map[string]any
	// close
	closeActor string
}

// fakePlanWriter is an in-memory planDaemonWriter: a bead store, a parent→children
// edge map, an op log, an auto-increment id minter, and knobs to inject failures
// (a CAS conflict on the Nth reorder, or a hard error on a named call). It drives
// every write verb with NO HTTP round-trip — the exact env the daemon-alone verbs
// target with only BEADS_AUTH_TOKEN.
type fakePlanWriter struct {
	beads    map[string]*beadsapi.BeadDetail
	children map[string][]string
	ops      []opRecord
	nextID   int

	// casConflicts: number of leading ReorderBeadArrayField calls that return a
	// 409 before one succeeds (exercises reorderArrayFieldWithRetry).
	casConflicts int
	reorderCalls int
	// failOn: the op kind ("create"/"dep"/"append"/…) whose FIRST occurrence
	// returns a hard error, to prove a verb surfaces (not swallows) it.
	failOn string
	// lastOperator records the operator on the most recent write's ctx, so a test
	// can assert withPlanOperator stamped it.
	lastOperator string
}

func newFakePlanWriter() *fakePlanWriter {
	return &fakePlanWriter{beads: map[string]*beadsapi.BeadDetail{}, children: map[string][]string{}}
}

func (f *fakePlanWriter) add(b *beadsapi.BeadDetail) { f.beads[b.ID] = b }

func (f *fakePlanWriter) record(op opRecord, ctx context.Context) {
	op.operator = beadsapi.OperatorFromContext(ctx)
	f.lastOperator = op.operator
	f.ops = append(f.ops, op)
}

func (f *fakePlanWriter) opsOfKind(kind string) []opRecord {
	var out []opRecord
	for _, o := range f.ops {
		if o.kind == kind {
			out = append(out, o)
		}
	}
	return out
}

func (f *fakePlanWriter) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	return f.beads[id], nil
}

func (f *fakePlanWriter) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	var deps []beadsapi.Dependency
	for _, cid := range f.children[id] {
		deps = append(deps, beadsapi.Dependency{BeadID: cid, DependsOnID: id, Type: "parent-child"})
	}
	return deps, nil
}

func (f *fakePlanWriter) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if b := f.beads[id]; b != nil {
			out[id] = b
		}
	}
	return out, nil
}

func (f *fakePlanWriter) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if f.failOn == "create" {
		f.failOn = ""
		return "", errors.New("boom-create")
	}
	f.nextID++
	id := "gd-NEW" + string(rune('0'+f.nextID))
	b := &beadsapi.BeadDetail{
		ID:     id,
		Title:  req.Title,
		Type:   req.Type,
		Status: "open",
		Labels: append([]string(nil), req.Labels...),
		Fields: map[string]string{},
	}
	if len(req.Fields) > 0 {
		// createPlanEpicDaemon marshals {"plan_state":"draft"} into req.Fields;
		// decode it so the verify read-back sees plan_state=draft.
		var fm map[string]string
		if err := json.Unmarshal(req.Fields, &fm); err == nil {
			b.Fields = fm
		}
	}
	f.add(b)
	if req.ParentBeadID != "" {
		// The daemon wires parent_bead_id in the create transaction.
		f.children[req.ParentBeadID] = append(f.children[req.ParentBeadID], id)
	}
	f.record(opRecord{kind: "create", bead: id, req: req}, ctx)
	return id, nil
}

func (f *fakePlanWriter) AddDependency(ctx context.Context, beadID, dependsOnID, depType, _ string) error {
	if f.failOn == "dep" {
		f.failOn = ""
		return errors.New("boom-dep")
	}
	if depType == "parent-child" {
		f.children[dependsOnID] = append(f.children[dependsOnID], beadID)
	}
	f.record(opRecord{kind: "dep", bead: beadID, depTo: dependsOnID, depType: depType}, ctx)
	return nil
}

func (f *fakePlanWriter) AppendBeadArrayField(ctx context.Context, beadID, field, element string) error {
	if f.failOn == "append" {
		f.failOn = ""
		return errors.New("boom-append")
	}
	f.record(opRecord{kind: "append", bead: beadID, field: field, element: element}, ctx)
	return nil
}

func (f *fakePlanWriter) ReorderBeadArrayField(ctx context.Context, beadID, field string, expected, next []string) error {
	f.reorderCalls++
	if f.reorderCalls <= f.casConflicts {
		return &beadsapi.APIError{StatusCode: http.StatusConflict}
	}
	f.record(opRecord{kind: "reorder", bead: beadID, field: field, expected: expected, next: next}, ctx)
	return nil
}

func (f *fakePlanWriter) UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	rec := opRecord{kind: "update", bead: beadID}
	if req.Status != nil {
		rec.status = *req.Status
		if b := f.beads[beadID]; b != nil {
			b.Status = *req.Status
		}
	}
	f.record(rec, ctx)
	return nil
}

func (f *fakePlanWriter) UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error {
	if b := f.beads[beadID]; b != nil {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		for k, v := range fields {
			if s, ok := v.(string); ok {
				b.Fields[k] = s
			}
		}
	}
	f.record(opRecord{kind: "fields", bead: beadID, fields: fields}, ctx)
	return nil
}

func (f *fakePlanWriter) CloseBeadAs(ctx context.Context, beadID, actor string, _ map[string]string) error {
	if b := f.beads[beadID]; b != nil {
		b.Status = "closed"
	}
	f.record(opRecord{kind: "close", bead: beadID, closeActor: actor}, ctx)
	return nil
}

func (f *fakePlanWriter) AddComment(ctx context.Context, beadID, _, text string) error {
	f.record(opRecord{kind: "comment", bead: beadID, element: text}, ctx)
	return nil
}

func (f *fakePlanWriter) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	// Return every stored trigger/schedule bead (the gardener lookup filters
	// client-side via plangardener.IsGardenerForEpic).
	res := &beadsapi.ListBeadsResult{}
	for _, b := range f.beads {
		if b.Type == "trigger" || b.Type == "schedule" {
			res.Beads = append(res.Beads, b)
		}
	}
	return res, nil
}

var _ planDaemonWriter = (*fakePlanWriter)(nil)

// ── create ────────────────────────────────────────────────────────────────

func TestCreatePlanEpicDaemon_BornOnLabelsAndDraft(t *testing.T) {
	f := newFakePlanWriter()
	epic, err := createPlanEpicDaemon(context.Background(), f, "  ship the thing  ", "gardenio", "matt@acme.example")
	if err != nil {
		t.Fatalf("createPlanEpicDaemon: %v", err)
	}
	// The CreateBead carried BOTH labels + plan_state=draft born-on (one write).
	creates := f.opsOfKind("create")
	if len(creates) != 1 {
		t.Fatalf("want exactly 1 create, got %d", len(creates))
	}
	req := creates[0].req
	if !reflect.DeepEqual(req.Labels, []string{planLabel, "project:gardenio"}) {
		t.Errorf("labels = %v, want [plan project:gardenio]", req.Labels)
	}
	if req.IdempotencyKey == "" {
		t.Error("create must carry an idempotency key")
	}
	if req.Description != "ship the thing" {
		t.Errorf("description = %q, want the verbatim (trimmed) goal", req.Description)
	}
	// The verified epic reads back gated.
	if got := epic.Fields[planStateFieldKey]; got != "draft" {
		t.Errorf("plan_state = %q, want draft", got)
	}
	if creates[0].operator != "oidc:matt@acme.example" {
		t.Errorf("create operator = %q, want oidc:matt@acme.example", creates[0].operator)
	}
}

func TestCreatePlanEpicDaemon_Rejections(t *testing.T) {
	f := newFakePlanWriter()
	if _, err := createPlanEpicDaemon(context.Background(), f, "", "gardenio", "m@x.io"); err == nil {
		t.Error("blank goal must error")
	}
	if _, err := createPlanEpicDaemon(context.Background(), f, "goal", "", "m@x.io"); err == nil {
		t.Error("blank project must error")
	}
	if _, err := createPlanEpicDaemon(context.Background(), f, strings.Repeat("x", maxPlanGoalLen+1), "gardenio", "m@x.io"); err == nil {
		t.Error("over-long goal must error")
	}
}

func TestCreatePlanEpicDaemon_CreateFailureSurfaces(t *testing.T) {
	f := newFakePlanWriter()
	f.failOn = "create"
	if _, err := createPlanEpicDaemon(context.Background(), f, "goal", "gardenio", "m@x.io"); err == nil {
		t.Fatal("a CreateBead failure must surface, not be swallowed")
	}
}

// ── add-step ──────────────────────────────────────────────────────────────

func TestAddPlanStep_Flat(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})

	res, err := addPlanStep(context.Background(), f, "gd-PLAN", "do a thing", "", "matt@acme.example")
	if err != nil {
		t.Fatalf("addPlanStep: %v", err)
	}
	if res.Parent != "gd-PLAN" || res.Phase != "" {
		t.Errorf("flat step parent=%q phase=%q, want gd-PLAN / empty", res.Parent, res.Phase)
	}
	// Step created with the inherited project label, then linked parent-child.
	create := f.opsOfKind("create")
	if len(create) != 1 || !reflect.DeepEqual(create[0].req.Labels, []string{"project:gardenio"}) {
		t.Errorf("step create labels = %v, want [project:gardenio]", create[0].req.Labels)
	}
	dep := f.opsOfKind("dep")
	if len(dep) != 1 || dep[0].depTo != "gd-PLAN" || dep[0].depType != "parent-child" {
		t.Errorf("dep = %+v, want a parent-child edge to gd-PLAN", dep)
	}
	if f.lastOperator != "oidc:matt@acme.example" {
		t.Errorf("operator = %q, want oidc:matt@acme.example", f.lastOperator)
	}
}

func TestAddPlanStep_UnderPhase(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open",
		Labels: []string{"plan", "project:gardenio"},
		Fields: map[string]string{"phase_order": `["gd-PH1"]`}})
	f.add(&beadsapi.BeadDetail{ID: "gd-PH1", Type: "epic", Status: "open", Labels: []string{"project:gardenio"}})

	res, err := addPlanStep(context.Background(), f, "gd-PLAN", "phased step", "gd-PH1", "m@x.io")
	if err != nil {
		t.Fatalf("addPlanStep(phase): %v", err)
	}
	if res.Parent != "gd-PH1" || res.Phase != "gd-PH1" {
		t.Errorf("phased step parent=%q phase=%q, want gd-PH1 both", res.Parent, res.Phase)
	}
	dep := f.opsOfKind("dep")
	if len(dep) != 1 || dep[0].depTo != "gd-PH1" {
		t.Errorf("dep target = %+v, want gd-PH1", dep)
	}
}

func TestAddPlanStep_PhaseNotInPlanRejected(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan"}})
	f.add(&beadsapi.BeadDetail{ID: "gd-STRAY", Type: "epic", Status: "open"})
	// gd-STRAY is a real epic but NOT in gd-PLAN's phase_order → fail closed.
	if _, err := addPlanStep(context.Background(), f, "gd-PLAN", "t", "gd-STRAY", "m@x.io"); err == nil {
		t.Fatal("a phase not in phase_order must be rejected")
	}
	if len(f.opsOfKind("create")) != 0 {
		t.Error("no step should be created when the phase target is invalid")
	}
}

func TestAddPlanStep_Validation(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open"})
	if _, err := addPlanStep(context.Background(), f, "gd-PLAN", "   ", "", "m@x.io"); err == nil {
		t.Error("blank title must error")
	}
	if _, err := addPlanStep(context.Background(), f, "gd-PLAN", strings.Repeat("x", maxPlanStepTitleLen+1), "", "m@x.io"); err == nil {
		t.Error("over-long title must error")
	}
	// A non-epic plan id is rejected.
	f.add(&beadsapi.BeadDetail{ID: "gd-TASK", Type: "task", Status: "open"})
	if _, err := addPlanStep(context.Background(), f, "gd-TASK", "t", "", "m@x.io"); err == nil {
		t.Error("a non-epic plan id must error")
	}
}

// ── add-phase ─────────────────────────────────────────────────────────────

func TestAddPlanPhase_AppendsToPhaseOrder(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})

	res, err := addPlanPhase(context.Background(), f, "gd-PLAN", "Research & design", "m@x.io")
	if err != nil {
		t.Fatalf("addPlanPhase: %v", err)
	}
	// A phase epic minted already parented to the plan (parent_bead_id on the
	// create, so the daemon's create-time gates see it — gd-kcCrpdrlm2), and its
	// id appended to phase_order via the ATOMIC primitive (not a read-modify-write
	// update).
	create := f.opsOfKind("create")
	if len(create) != 1 {
		t.Fatalf("want 1 phase epic create, got %d", len(create))
	}
	if create[0].req.ParentBeadID != "gd-PLAN" {
		t.Errorf("create ParentBeadID = %q, want gd-PLAN", create[0].req.ParentBeadID)
	}
	if dep := f.opsOfKind("dep"); len(dep) != 0 {
		t.Errorf("dep = %+v, want none (parent-child wired by CreateBead.ParentBeadID)", dep)
	}
	if kids := f.children["gd-PLAN"]; len(kids) != 1 || kids[0] != res.ID {
		t.Errorf("children of gd-PLAN = %v, want [%s]", kids, res.ID)
	}
	app := f.opsOfKind("append")
	if len(app) != 1 || app[0].bead != "gd-PLAN" || app[0].field != planPhaseFieldKey || app[0].element != res.ID {
		t.Errorf("append = %+v, want phase_order += %s on gd-PLAN", app, res.ID)
	}
	if len(f.opsOfKind("fields")) != 0 {
		t.Error("add-phase must not use a read-modify-write fields update for the grow")
	}
}

func TestAddPlanPhase_AppendFailureSurfaces(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan"}})
	f.failOn = "append"
	if _, err := addPlanPhase(context.Background(), f, "gd-PLAN", "P", "m@x.io"); err == nil {
		t.Fatal("a phase_order append failure must surface (linked-but-unlisted phase)")
	}
}

// ── reorder-phase ───────────────────────────────────────────────────────────

func TestReorderPlanPhase_CASWrite(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open",
		Fields: map[string]string{"phase_order": `["gd-PH1","gd-PH2"]`}})
	f.add(&beadsapi.BeadDetail{ID: "gd-PH1", Type: "epic", Status: "open"})
	f.add(&beadsapi.BeadDetail{ID: "gd-PH2", Type: "epic", Status: "open"})
	f.children["gd-PLAN"] = []string{"gd-PH1", "gd-PH2"}

	res, err := reorderPlanPhase(context.Background(), f, "gd-PLAN", "gd-PH2", "up", "m@x.io")
	if err != nil {
		t.Fatalf("reorderPlanPhase: %v", err)
	}
	if res.Status != "ok" || !reflect.DeepEqual(res.Order, []string{"gd-PH2", "gd-PH1"}) {
		t.Errorf("result = %+v, want ok / [gd-PH2 gd-PH1]", res)
	}
	reorders := f.opsOfKind("reorder")
	if len(reorders) != 1 {
		t.Fatalf("want 1 reorder, got %d", len(reorders))
	}
	if !reflect.DeepEqual(reorders[0].expected, []string{"gd-PH1", "gd-PH2"}) {
		t.Errorf("CAS expected = %v, want the stored order [gd-PH1 gd-PH2]", reorders[0].expected)
	}
	if !reflect.DeepEqual(reorders[0].next, []string{"gd-PH2", "gd-PH1"}) {
		t.Errorf("CAS next = %v, want [gd-PH2 gd-PH1]", reorders[0].next)
	}
}

// TestReorderPlanPhase_RetriesOnConflict: the first reorder 409s, the retry
// re-reads + re-derives and lands.
func TestReorderPlanPhase_RetriesOnConflict(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open",
		Fields: map[string]string{"phase_order": `["gd-PH1","gd-PH2"]`}})
	f.add(&beadsapi.BeadDetail{ID: "gd-PH1", Type: "epic", Status: "open"})
	f.add(&beadsapi.BeadDetail{ID: "gd-PH2", Type: "epic", Status: "open"})
	f.children["gd-PLAN"] = []string{"gd-PH1", "gd-PH2"}
	f.casConflicts = 2 // two 409s, then success

	res, err := reorderPlanPhase(context.Background(), f, "gd-PLAN", "gd-PH2", "up", "m@x.io")
	if err != nil {
		t.Fatalf("reorderPlanPhase (with retries): %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("status = %q, want ok after retries", res.Status)
	}
	if f.reorderCalls != 3 {
		t.Errorf("reorder attempts = %d, want 3 (2 conflicts + 1 success)", f.reorderCalls)
	}
}

func TestReorderPlanPhase_Noop(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open",
		Fields: map[string]string{"phase_order": `["gd-PH1","gd-PH2"]`}})
	f.add(&beadsapi.BeadDetail{ID: "gd-PH1", Type: "epic", Status: "open"})
	f.add(&beadsapi.BeadDetail{ID: "gd-PH2", Type: "epic", Status: "open"})
	f.children["gd-PLAN"] = []string{"gd-PH1", "gd-PH2"}

	// Moving the first phase up is a boundary no-op — no write.
	res, err := reorderPlanPhase(context.Background(), f, "gd-PLAN", "gd-PH1", "up", "m@x.io")
	if err != nil {
		t.Fatalf("reorderPlanPhase(noop): %v", err)
	}
	if res.Status != "noop" {
		t.Errorf("status = %q, want noop", res.Status)
	}
	if len(f.opsOfKind("reorder")) != 0 {
		t.Error("a boundary no-op must not write")
	}
}

func TestReorderPlanPhase_BadDir(t *testing.T) {
	f := newFakePlanWriter()
	if _, err := reorderPlanPhase(context.Background(), f, "gd-PLAN", "gd-PH1", "sideways", "m@x.io"); err == nil {
		t.Error("an invalid dir must error")
	}
}

// ── complete / reopen ─────────────────────────────────────────────────────

func TestCompletePlan_ClosesStampsAndDisablesGardener(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open",
		Labels: []string{"plan"}, Fields: map[string]string{"plan_state": "building"}})
	// A bound gardener (trigger with the gardener-epic label + role field).
	f.add(&beadsapi.BeadDetail{ID: "gd-CAP", Type: "trigger", Status: "open",
		Labels: []string{"role:gardener", "gardener-epic:gd-PLAN"},
		Fields: map[string]string{"role": "gardener", "enabled": "true"}})

	res, err := completePlan(context.Background(), f, "gd-PLAN", "matt@acme.example")
	if err != nil {
		t.Fatalf("completePlan: %v", err)
	}
	if res.PlanState != "complete" || res.Status != "closed" {
		t.Errorf("result = %+v, want complete / closed", res)
	}
	// Ordering: gardener disabled BEFORE the epic close.
	var disabledIdx, closeIdx = -1, -1
	for i, o := range f.ops {
		if o.kind == "fields" && o.bead == "gd-CAP" {
			if v, ok := o.fields["enabled"].(bool); ok && !v {
				disabledIdx = i
			}
		}
		if o.kind == "close" && o.bead == "gd-PLAN" {
			closeIdx = i
		}
	}
	if disabledIdx < 0 {
		t.Fatal("gardener was not disabled")
	}
	if closeIdx < 0 {
		t.Fatal("plan was not closed")
	}
	if disabledIdx > closeIdx {
		t.Errorf("gardener disable (idx %d) must precede the epic close (idx %d)", disabledIdx, closeIdx)
	}
	// The plan_state=complete stamp landed on the epic.
	if f.beads["gd-PLAN"].Fields["plan_state"] != "complete" {
		t.Errorf("plan_state = %q, want complete", f.beads["gd-PLAN"].Fields["plan_state"])
	}
}

func TestCompletePlan_NoGardenerStillCompletes(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan"},
		Fields: map[string]string{"plan_state": "building"}})
	res, err := completePlan(context.Background(), f, "gd-PLAN", "m@x.io")
	if err != nil {
		t.Fatalf("completePlan (no gardener): %v", err)
	}
	if res.Status != "closed" {
		t.Errorf("status = %q, want closed even with no gardener", res.Status)
	}
}

func TestCompletePlan_IdempotentWhenAlreadyComplete(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "closed", Labels: []string{"plan"},
		Fields: map[string]string{"plan_state": "complete"}})
	res, err := completePlan(context.Background(), f, "gd-PLAN", "m@x.io")
	if err != nil {
		t.Fatalf("completePlan (idempotent): %v", err)
	}
	if res.Status != "closed" {
		t.Errorf("status = %q, want closed", res.Status)
	}
	// No second close/stamp on an already-terminal plan.
	if len(f.opsOfKind("close")) != 0 {
		t.Error("an already-complete+closed plan must not be closed again")
	}
}

func TestAbandonPlan_ClosesStampsAndDisablesGardener(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open",
		Labels: []string{"plan"}, Fields: map[string]string{"plan_state": "building"}})
	f.add(&beadsapi.BeadDetail{ID: "gd-CAP", Type: "trigger", Status: "open",
		Labels: []string{"role:gardener", "gardener-epic:gd-PLAN"},
		Fields: map[string]string{"role": "gardener", "enabled": "true"}})

	res, err := abandonPlan(context.Background(), f, "gd-PLAN", "matt@acme.example")
	if err != nil {
		t.Fatalf("abandonPlan: %v", err)
	}
	if res.PlanState != "abandoned" || res.Status != "closed" {
		t.Errorf("result = %+v, want abandoned / closed", res)
	}
	// Ordering: gardener disabled BEFORE the epic close (same safe-failure order
	// as completePlan).
	var disabledIdx, closeIdx = -1, -1
	for i, o := range f.ops {
		if o.kind == "fields" && o.bead == "gd-CAP" {
			if v, ok := o.fields["enabled"].(bool); ok && !v {
				disabledIdx = i
			}
		}
		if o.kind == "close" && o.bead == "gd-PLAN" {
			closeIdx = i
		}
	}
	if disabledIdx < 0 {
		t.Fatal("gardener was not disabled")
	}
	if closeIdx < 0 {
		t.Fatal("plan was not closed")
	}
	if disabledIdx > closeIdx {
		t.Errorf("gardener disable (idx %d) must precede the epic close (idx %d)", disabledIdx, closeIdx)
	}
	if f.beads["gd-PLAN"].Fields["plan_state"] != "abandoned" {
		t.Errorf("plan_state = %q, want abandoned", f.beads["gd-PLAN"].Fields["plan_state"])
	}
}

// A plan can be abandoned straight from draft — the "cheap exit" that never walks
// the plan forward to building (the daemon accepts draft→abandoned).
func TestAbandonPlan_FromDraft(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan"},
		Fields: map[string]string{"plan_state": "draft"}})
	res, err := abandonPlan(context.Background(), f, "gd-PLAN", "m@x.io")
	if err != nil {
		t.Fatalf("abandonPlan (from draft): %v", err)
	}
	if res.PlanState != "abandoned" || res.Status != "closed" {
		t.Errorf("result = %+v, want abandoned / closed", res)
	}
}

func TestAbandonPlan_IdempotentWhenAlreadyAbandoned(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "closed", Labels: []string{"plan"},
		Fields: map[string]string{"plan_state": "abandoned"}})
	res, err := abandonPlan(context.Background(), f, "gd-PLAN", "m@x.io")
	if err != nil {
		t.Fatalf("abandonPlan (idempotent): %v", err)
	}
	if res.Status != "closed" {
		t.Errorf("status = %q, want closed", res.Status)
	}
	if len(f.opsOfKind("close")) != 0 {
		t.Error("an already-abandoned+closed plan must not be closed again")
	}
}

func TestReopenPlan_RestoresBuilding(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "closed", Labels: []string{"plan"},
		Fields: map[string]string{"plan_state": "complete"}})

	res, err := reopenPlan(context.Background(), f, "gd-PLAN", "m@x.io")
	if err != nil {
		t.Fatalf("reopenPlan: %v", err)
	}
	if res.PlanState != "building" || res.Status != "open" {
		t.Errorf("result = %+v, want building / open", res)
	}
	// The epic was reopened (status=open) and plan_state restored to building.
	upd := f.opsOfKind("update")
	if len(upd) != 1 || upd[0].status != "open" {
		t.Errorf("update = %+v, want a single status=open", upd)
	}
	if f.beads["gd-PLAN"].Fields["plan_state"] != "building" {
		t.Errorf("plan_state = %q, want building", f.beads["gd-PLAN"].Fields["plan_state"])
	}
}

// An abandoned plan reopens to building exactly like a completed one — both
// terminal states share the single back-edge.
func TestReopenPlan_FromAbandoned(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "closed", Labels: []string{"plan"},
		Fields: map[string]string{"plan_state": "abandoned"}})

	res, err := reopenPlan(context.Background(), f, "gd-PLAN", "m@x.io")
	if err != nil {
		t.Fatalf("reopenPlan (from abandoned): %v", err)
	}
	if res.PlanState != "building" || res.Status != "open" {
		t.Errorf("result = %+v, want building / open", res)
	}
	if f.beads["gd-PLAN"].Fields["plan_state"] != "building" {
		t.Errorf("plan_state = %q, want building", f.beads["gd-PLAN"].Fields["plan_state"])
	}
}

func TestReopenPlan_OpenPlanIsNoop(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Status: "open", Labels: []string{"plan"},
		Fields: map[string]string{"plan_state": "building"}})
	res, err := reopenPlan(context.Background(), f, "gd-PLAN", "m@x.io")
	if err != nil {
		t.Fatalf("reopenPlan(open): %v", err)
	}
	if res.Status != "open" {
		t.Errorf("status = %q, want open", res.Status)
	}
	// An already-open, non-complete plan must not be re-updated.
	if len(f.opsOfKind("update")) != 0 {
		t.Error("an open non-complete plan reopen must be a no-op")
	}
}
