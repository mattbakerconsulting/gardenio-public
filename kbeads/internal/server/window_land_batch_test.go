package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// countingStore counts the reads a land view makes.
type countingStore struct {
	store.Store
	reads atomic.Int64
}

func (c *countingStore) ListBeads(ctx context.Context, f model.BeadFilter) ([]*model.Bead, int, error) {
	c.reads.Add(1)
	return c.Store.ListBeads(ctx, f)
}

func (c *countingStore) GetBead(ctx context.Context, id string) (*model.Bead, error) {
	c.reads.Add(1)
	return c.Store.GetBead(ctx, id)
}

func (c *countingStore) GetBeadsByIDs(ctx context.Context, ids []string) ([]*model.Bead, error) {
	c.reads.Add(1)
	return c.Store.GetBeadsByIDs(ctx, ids)
}

func (c *countingStore) GetDependenciesForBeads(ctx context.Context, ids []string) (map[string][]*model.Dependency, error) {
	c.reads.Add(1)
	return c.Store.GetDependenciesForBeads(ctx, ids)
}

func (c *countingStore) GetReverseDependenciesForBeads(ctx context.Context, ids []string) (map[string][]*model.Dependency, error) {
	c.reads.Add(1)
	return c.Store.GetReverseDependenciesForBeads(ctx, ids)
}

// seedBigLand adds plans plans of phases phases of steps steps to claimlayer,
// each phase with a decision on its first step and an agent on its last, the
// size of the gardenio land that timed out (gd-rXMNcFrzDp: 31 plans,
// hundreds of steps).
func seedBigLand(f *scopedFixture, plans, phases, steps int) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	n := 0
	add := func(id string, typ model.BeadType, status model.Status, parent string, labels []string, fields string) {
		n++
		at := base.Add(time.Duration(n) * time.Minute)
		b := &model.Bead{ID: id, Type: typ, Kind: model.KindIssue, Title: "T " + id, Status: status, Fleet: "default",
			Labels: labels, CreatedAt: at, UpdatedAt: at}
		if fields != "" {
			b.Fields = json.RawMessage(fields)
		}
		if status == model.StatusClosed {
			b.ClosedAt = &at
		}
		f.ms.beads[id], f.ms.labels[id] = b, labels
		if parent != "" {
			f.ms.deps[id] = append(f.ms.deps[id], &model.Dependency{BeadID: id, DependsOnID: parent, Type: model.DepParentChild})
		}
	}
	cl := []string{"project:claimlayer"}
	for p := range plans {
		plan := fmt.Sprintf("big-%02d", p)
		var order []string
		for ph := range phases {
			order = append(order, fmt.Sprintf(`"%s-ph%d"`, plan, ph))
		}
		add(plan, model.TypeEpic, model.StatusInProgress, "", []string{"plan", "project:claimlayer"},
			fmt.Sprintf(`{"plan_state":"building","phase_order":[%s]}`, joinComma(order)))
		for ph := range phases {
			phase := fmt.Sprintf("%s-ph%d", plan, ph)
			add(phase, model.TypeEpic, model.StatusOpen, plan, cl, "")
			for st := range steps {
				step := fmt.Sprintf("%s-s%d", phase, st)
				status := model.StatusOpen
				if st%3 == 0 {
					status = model.StatusClosed
				}
				add(step, model.TypeTask, status, phase, cl, "")
				if st == 0 {
					add(step+"-dec", model.TypeDecision, model.StatusOpen, step, cl, `{"options":[{"id":"y","label":"Yes"}]}`)
				}
				if st == steps-1 {
					agent := step + "-agent"
					add(agent, "agent", model.StatusOpen, "", nil, fmt.Sprintf(`{"agent":"crew-%s","agent_state":"working"}`, step))
					f.ms.deps[agent] = append(f.ms.deps[agent], &model.Dependency{BeadID: agent, DependsOnID: step, Type: windowAssignedDep})
				}
			}
		}
	}
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// TestWindow_LandViewBatched: a land view on a land of gardenio's size reads
// the store a fixed number of times, not a few times a plan, and shows what
// the plan-by-plan walk shows.
func TestWindow_LandViewBatched(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	seedBigLand(f, 30, 3, 4)
	cs := &countingStore{Store: f.ms}
	f.srv.store = cs
	ctx := context.Background()
	wf := windowFieldSet{Target: winLandProject, TargetKind: windowTargetLand, Land: "claimlayer"}

	v, err := f.srv.buildLandWindowView(ctx, wf)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("batched land view: %d reads", cs.reads.Load())
	// GetBead, the open and closed plan lists, the subtree, and
	// readWindowClosure's three.
	if got := cs.reads.Load(); got > 7 {
		t.Errorf("land view of %d plans read the store %d times, want at most 7", len(v.Plans), got)
	}
	steps, crew := 0, 0
	for _, p := range v.Plans {
		steps += p.StepsTotal
		crew += len(p.Crew)
	}
	if len(v.Plans) < 30 || steps < 360 || crew < 90 {
		t.Errorf("land view: %d plans, %d steps, %d crew; want the 30 big plans whole", len(v.Plans), steps, crew)
	}

	open, _, err := f.srv.landWindowPlans(ctx, "claimlayer", []model.Status{
		model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred, model.StatusPartial,
	}, windowLandPlansMax)
	if err != nil {
		t.Fatal(err)
	}
	batched, err := f.srv.projectLandPlans(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	cs.reads.Store(0)
	walked, err := f.srv.projectLandPlansOneByOne(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan-by-plan walk of %d plans: %d reads", len(open), cs.reads.Load())
	if cs.reads.Load() < int64(len(open)) {
		t.Errorf("plan-by-plan walk read %d times for %d plans; the comparison is not exercising it", cs.reads.Load(), len(open))
	}
	if !reflect.DeepEqual(batched, walked) {
		a, _ := json.Marshal(batched)
		b, _ := json.Marshal(walked)
		t.Errorf("batched land view differs from the plan-by-plan walk:\nbatched %s\nwalked  %s", a, b)
	}
}

// TestWindow_LandViewBatchedBound: plans past the land's walk bound are
// withheld in the batched view as in the plan-by-plan one.
func TestWindow_LandViewBatchedBound(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowLand(t, f)
	seedBigLand(f, 4, 2, 3)
	ctx := context.Background()
	open, _, err := f.srv.landWindowPlans(ctx, "claimlayer", []model.Status{model.StatusInProgress, model.StatusOpen}, windowLandPlansMax)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a plan past windowClosureMax alone.
	huge := open[0]
	for i := range windowClosureMax + 1 {
		id := fmt.Sprintf("huge-%04d", i)
		f.ms.beads[id] = &model.Bead{ID: id, Type: model.TypeTask, Kind: model.KindIssue, Title: id, Status: model.StatusOpen, Fleet: "default"}
		f.ms.deps[id] = []*model.Dependency{{BeadID: id, DependsOnID: huge.ID, Type: model.DepParentChild}}
	}
	batched, err := f.srv.projectLandPlans(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	walked, err := f.srv.projectLandPlansOneByOne(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(batched, walked) {
		t.Errorf("batched and plan-by-plan views differ past the bound")
	}
	if !batched[0].Withheld || batched[1].Withheld {
		t.Errorf("withheld: first %v, second %v; want only the oversized plan withheld", batched[0].Withheld, batched[1].Withheld)
	}
}
