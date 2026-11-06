package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// answerFarm is the home garden (project gardenio, the operator's land on the
// Commons) with one ready plan of two steps, its owner's decision and its
// gardener bound paused behind the approve gate, as gb lands plan write and
// the plan-label reactor leave them.
type answerFarm struct {
	beads    map[string]*beadsapi.BeadDetail
	deps     []beadsapi.Dependency
	comments map[string][]string
	operator []string // the operator each write carried
}

var answerNow = time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)

func newAnswerFarm() *answerFarm {
	body := strings.Repeat("Change the thing and test it. ", 10)
	f := &answerFarm{beads: map[string]*beadsapi.BeadDetail{}, comments: map[string][]string{}}
	f.add(&beadsapi.BeadDetail{ID: "gd-proj", Type: "project", Title: "gardenio", Status: "open", OperatedBy: "oidc:op@example.com",
		Fields: map[string]string{"land_kind": "operator", "land_default_channel": "gd-general"}})
	f.add(&beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Title: "Wake on CI", Status: "open",
		Labels: []string{"project:gardenio", "plan"}, Fields: map[string]string{"plan_state": "ready", "step_order": `["gd-s1","gd-s2"]`}})
	for _, s := range []string{"gd-s1", "gd-s2"} {
		f.add(&beadsapi.BeadDetail{ID: s, Type: "task", Title: "step " + s, Status: "open", Description: body,
			Labels: []string{"project:gardenio"}, Fields: map[string]string{}})
		f.deps = append(f.deps, beadsapi.Dependency{BeadID: s, DependsOnID: "gd-plan", Type: "parent-child"})
	}
	f.add(&beadsapi.BeadDetail{ID: "gd-dec", Type: "decision", Title: "Plan gd-plan: Wake on CI - run it?", Status: "closed",
		ClosedAt: answerNow.Add(-2 * time.Minute).Format(time.RFC3339), Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"requesting_task": "gd-plan", "requesting_plan": "gd-plan", "operated_by": "oidc:op@example.com",
			"chosen": "approve", "responded_by": "op@example.com",
			"resolver_subject": "op@example.com", "resolver_admin": "true",
		}})
	f.add(&beadsapi.BeadDetail{ID: "gd-trig", Type: "trigger", Title: "plan gardener", Status: "open",
		Labels: []string{"project:gardenio", "role:gardener", "plan-gardener", "gardener-epic:gd-plan"},
		Fields: map[string]string{"role": "plan-gardener", "gardener": "plan", "enabled": "false"}})
	return f
}

func (f *answerFarm) add(b *beadsapi.BeadDetail) { f.beads[b.ID] = b }

func (f *answerFarm) record(ctx context.Context) {
	f.operator = append(f.operator, beadsapi.OperatorFromContext(ctx))
}

func (f *answerFarm) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var out []*beadsapi.BeadDetail
	if q.Offset > 0 {
		return &beadsapi.ListBeadsResult{}, nil
	}
	for _, id := range slices.Sorted(maps.Keys(f.beads)) {
		b := f.beads[id]
		if len(q.Types) > 0 && !slices.Contains(q.Types, b.Type) ||
			len(q.Statuses) > 0 && !slices.Contains(q.Statuses, b.Status) ||
			slices.ContainsFunc(q.Labels, func(l string) bool { return !slices.Contains(b.Labels, l) }) {
			continue
		}
		match := true
		for k, v := range q.Fields {
			match = match && b.Fields[k] == v
		}
		if match {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *answerFarm) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("bead %s not found", id)
}

func (f *answerFarm) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.record(ctx)
	id := fmt.Sprintf("gd-new%d", len(f.beads))
	f.add(&beadsapi.BeadDetail{ID: id, Type: req.Type, Title: req.Title, Status: "open", Labels: req.Labels, Fields: map[string]string{}})
	return id, nil
}

func (f *answerFarm) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	f.deps = append(f.deps, beadsapi.Dependency{BeadID: beadID, DependsOnID: dependsOnID, Type: depType})
	return nil
}

func (f *answerFarm) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	var out []beadsapi.Dependency
	for _, d := range f.deps {
		if d.DependsOnID == id {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *answerFarm) UpdateBeadFieldsTyped(ctx context.Context, id string, fields map[string]any) error {
	f.record(ctx)
	b, ok := f.beads[id]
	if !ok {
		return fmt.Errorf("bead %s not found", id)
	}
	for k, v := range fields {
		if s, ok := v.(string); ok {
			b.Fields[k] = s
			continue
		}
		raw, _ := json.Marshal(v)
		b.Fields[k] = string(raw)
	}
	return nil
}

func (f *answerFarm) CloseBeadAs(ctx context.Context, id, _ string, _ map[string]string) error {
	f.record(ctx)
	f.beads[id].Status = "closed"
	return nil
}

func (f *answerFarm) AddComment(_ context.Context, id, _, text string) error {
	f.comments[id] = append(f.comments[id], text)
	return nil
}

func (f *answerFarm) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	var out []beadsapi.Comment
	for _, text := range f.comments[id] {
		out = append(out, beadsapi.Comment{Author: "machine:gardenio-beads-viewer", Text: text})
	}
	return out, nil
}

func newTestAnswerReactor(f *answerFarm) *PlanAnswerReactor {
	r := NewPlanAnswerReactor(f, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.now = func() time.Time { return answerNow }
	return r
}

// The owner's approve reaches plan_state=building and turns the gardener on,
// with no escalation and nobody running gb lands plan answer. A second sweep
// changes nothing.
func TestPlanAnswerReactor_OwnerApproveBuilds(t *testing.T) {
	f := newAnswerFarm()
	r := newTestAnswerReactor(f)
	if n := r.sweep(context.Background()); n != 1 {
		t.Fatalf("sweep applied %d answers, want 1", n)
	}
	if st := f.beads["gd-plan"].Fields["plan_state"]; st != "building" {
		t.Fatalf("plan_state %q, want building", st)
	}
	if en := f.beads["gd-trig"].Fields["enabled"]; en != "true" {
		t.Fatalf("gardener enabled=%q, want true", en)
	}
	for _, op := range f.operator {
		if op != "oidc:op@example.com" {
			t.Fatalf("a write carried operator %q, want the owner who answered", op)
		}
	}
	for _, b := range f.beads {
		if b.Type == "escalation" {
			t.Fatalf("filed escalation %s", b.ID)
		}
	}
	writes := len(f.operator)
	if n := r.sweep(context.Background()); n != 0 || len(f.operator) != writes {
		t.Fatalf("second sweep applied %d and wrote %d times, want nothing", n, len(f.operator)-writes)
	}
}

func TestPlanAnswerReactor_OwnerDropCloses(t *testing.T) {
	f := newAnswerFarm()
	f.beads["gd-dec"].Fields["chosen"] = "drop"
	if n := newTestAnswerReactor(f).sweep(context.Background()); n != 1 {
		t.Fatalf("sweep applied %d answers, want 1", n)
	}
	if f.beads["gd-plan"].Status != "closed" || f.beads["gd-s1"].Status != "closed" || f.beads["gd-plan"].Fields["plan_state"] != "abandoned" {
		t.Fatal("drop must abandon and close the plan and its open steps")
	}
	if f.beads["gd-trig"].Fields["enabled"] != "false" {
		t.Error("drop must not turn the gardener on")
	}
}

// An answer the server must not apply leaves the plan ready, the gardener off
// and nothing written.
func TestPlanAnswerReactor_AppliesNothingFor(t *testing.T) {
	for name, mutate := range map[string]func(f *answerFarm){
		"an answer by a non-operator": func(f *answerFarm) { f.beads["gd-dec"].Fields["resolver_admin"] = "false" },
		"a decision for another plan": func(f *answerFarm) { f.beads["gd-dec"].Fields["requesting_plan"] = "gd-other" },
		"an open decision":            func(f *answerFarm) { f.beads["gd-dec"].Status = "open" },
		"a dismissed card":            func(f *answerFarm) { f.beads["gd-dec"].Fields["chosen"] = "dismissed" },
		"words with no option":        func(f *answerFarm) { f.beads["gd-dec"].Fields["chosen"] = "" },
		"an answer two days old": func(f *answerFarm) {
			f.beads["gd-dec"].ClosedAt = answerNow.Add(-48 * time.Hour).Format(time.RFC3339)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAnswerFarm()
			mutate(f)
			if n := newTestAnswerReactor(f).sweep(context.Background()); n != 0 {
				t.Fatalf("sweep applied %d answers, want 0", n)
			}
			if f.beads["gd-plan"].Fields["plan_state"] != "ready" || f.beads["gd-trig"].Fields["enabled"] != "false" || len(f.operator) != 0 {
				t.Fatalf("plan_state %q, gardener enabled %q, %d writes; want ready, false, none",
					f.beads["gd-plan"].Fields["plan_state"], f.beads["gd-trig"].Fields["enabled"], len(f.operator))
			}
		})
	}
}

// An approve that cannot apply yet (a step with no body) says so on the plan
// once, and applies on the sweep after the step is filled in.
func TestPlanAnswerReactor_ThinStepWaits(t *testing.T) {
	f := newAnswerFarm()
	f.beads["gd-s2"].Description = ""
	r := newTestAnswerReactor(f)
	r.sweep(context.Background())
	r.sweep(context.Background())
	if got := f.comments["gd-plan"]; len(got) != 1 || !strings.Contains(got[0], "gd-s2") {
		t.Fatalf("comments on the plan %q, want one naming gd-s2", got)
	}
	if f.beads["gd-plan"].Fields["plan_state"] != "ready" {
		t.Fatal("a refused approve must leave the plan ready")
	}
	f.beads["gd-s2"].Description = strings.Repeat("Change the thing and test it. ", 10)
	if n := r.sweep(context.Background()); n != 1 || f.beads["gd-plan"].Fields["plan_state"] != "building" {
		t.Fatalf("after the fill: applied %d, plan_state %q", n, f.beads["gd-plan"].Fields["plan_state"])
	}
}

// A restarted viewer starts with nothing noticed. It must not post the
// refusal again on a plan that already carries it (gd-OxEUOYP5TU), but it
// still posts a refusal that changed.
func TestPlanAnswerReactor_ThinStepNoteSurvivesRestart(t *testing.T) {
	f := newAnswerFarm()
	f.beads["gd-s2"].Description = ""
	newTestAnswerReactor(f).sweep(context.Background())
	for range 2 {
		newTestAnswerReactor(f).sweep(context.Background())
	}
	if got := f.comments["gd-plan"]; len(got) != 1 {
		t.Fatalf("after two restarts the plan has %d comments, want 1: %q", len(got), got)
	}
	f.beads["gd-s2"].Description = strings.Repeat("Change the thing and test it. ", 10)
	f.beads["gd-s1"].Description = ""
	newTestAnswerReactor(f).sweep(context.Background())
	if got := f.comments["gd-plan"]; len(got) != 2 || !strings.Contains(got[1], "gd-s1") {
		t.Fatalf("comments on the plan %q, want a second one naming gd-s1", got)
	}
}
