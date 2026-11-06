package badge

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// lineReader is a daemon of fixed beads and forward edges for ResolveLine.
type lineReader struct {
	beads map[string]*beadsapi.BeadDetail
	deps  map[string][]beadsapi.Dependency
}

func (r lineReader) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b := r.beads[id]; b != nil {
		return b, nil
	}
	return nil, errors.New("not found")
}

func (r lineReader) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	return r.deps[id], nil
}

func pc(child, parent string) beadsapi.Dependency {
	return beadsapi.Dependency{BeadID: child, DependsOnID: parent, Type: "parent-child"}
}

// An agent bead walks to the task it is assigned, by task_id or its assigned edge, and takes that task's line arms;
// a task's stored arms (pre-R6) and arms that do not parse are never read (gd-ASLjQPkHJz).
func TestResolveLine_AgentShowsItsTasksLine(t *testing.T) {
	const arms = "🔥🧭📦"
	r := lineReader{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-Epic": {ID: "gd-Epic", Type: "epic", Heraldry: arms, Title: "the epic"},
			"gd-Task": {ID: "gd-Task", Type: "task", Heraldry: "🐝🐝🐝"},
			"gd-Bad":  {ID: "gd-Bad", Type: "epic", Heraldry: "not arms"},
			"gd-Lone": {ID: "gd-Lone", Type: "task"},
		},
		deps: map[string][]beadsapi.Dependency{
			"gd-Task": {pc("gd-Task", "gd-Bad"), pc("gd-Task", "gd-Epic")},
		},
	}
	byField := &beadsapi.BeadDetail{ID: "gd-Agent1", Type: "agent", Fields: map[string]string{"task_id": "gd-Task"}}
	byEdge := &beadsapi.BeadDetail{ID: "gd-Agent2", Type: "agent"}
	edge := []beadsapi.Dependency{{BeadID: "gd-Agent2", DependsOnID: "gd-Task", Type: "assigned"}}
	for _, c := range []struct {
		b   *beadsapi.BeadDetail
		fwd []beadsapi.Dependency
	}{{byField, nil}, {byEdge, edge}} {
		a, read := ResolveLine(context.Background(), r, c.b, c.fwd)
		if a.Heraldry != arms || a.From != "gd-Epic" {
			t.Errorf("%s: arms %+v, want the epic's", c.b.ID, a)
		}
		if read["gd-Epic"] == nil || read["gd-Epic"].Title != "the epic" {
			t.Errorf("%s: the bearer is among the beads read: %v", c.b.ID, read)
		}
	}
	lone := &beadsapi.BeadDetail{ID: "gd-Agent3", Type: "agent", Fields: map[string]string{"task_id": "gd-Lone"}}
	if a, _ := ResolveLine(context.Background(), r, lone, nil); !a.None() {
		t.Errorf("an agent whose task has no armed line shows none: %+v", a)
	}
	idle := &beadsapi.BeadDetail{ID: "gd-Agent4", Type: "agent"}
	if a, _ := ResolveLine(context.Background(), r, idle, nil); !a.None() {
		t.Errorf("an agent with no task shows none: %+v", a)
	}
}
