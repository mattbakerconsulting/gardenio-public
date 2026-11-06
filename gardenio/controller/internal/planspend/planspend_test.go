package planspend

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

var now = time.Date(2026, 10, 27, 19, 30, 0, 0, time.UTC)

// fakeSource serves reverse deps and beads from maps, counting calls.
type fakeSource struct {
	rev      map[string][]beadsapi.Dependency
	beads    map[string]*beadsapi.BeadDetail
	depsErr  error
	beadsErr error
	depCalls int
	getCalls int
	got      []string // every id hydrated
}

func (f *fakeSource) BatchGetDependencies(_ context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	f.depCalls++
	if f.depsErr != nil {
		return nil, f.depsErr
	}
	if direction != "reverse" {
		return nil, errors.New("only reverse")
	}
	out := map[string][]beadsapi.Dependency{}
	for _, id := range ids {
		out[id] = f.rev[id]
	}
	return out, nil
}

func (f *fakeSource) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	f.getCalls++
	if f.beadsErr != nil {
		return nil, f.beadsErr
	}
	f.got = append(f.got, ids...)
	out := map[string]*beadsapi.BeadDetail{}
	for _, id := range ids {
		if b := f.beads[id]; b != nil {
			out[id] = b
		}
	}
	return out, nil
}

func newFake() *fakeSource {
	return &fakeSource{rev: map[string][]beadsapi.Dependency{}, beads: map[string]*beadsapi.BeadDetail{}}
}

// child adds a parent-child edge; typ is the edge's BeadType hint ("" = none).
func (f *fakeSource) child(parent, id, beadType, hint string) {
	f.beads[id] = &beadsapi.BeadDetail{ID: id, Type: beadType, Status: "open"}
	f.rev[parent] = append(f.rev[parent], beadsapi.Dependency{BeadID: id, DependsOnID: parent, Type: "parent-child", BeadType: hint})
}

// crew adds an agent assigned to target.
func (f *fakeSource) crew(target, id, state, status, total, live string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{ID: id, Type: "agent", Status: status, UpdatedAt: now.Add(-time.Minute),
		Fields: map[string]string{"agent_state": state, "role": "crew", "total_cost_usd": total, "live_cost_usd": live}}
	f.beads[id] = b
	f.rev[target] = append(f.rev[target], beadsapi.Dependency{BeadID: id, DependsOnID: target, Type: "assigned"})
	return b
}

func plan(id, phaseOrder string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{ID: id, Type: "epic", Status: "open", Labels: []string{"plan"}, Fields: map[string]string{}}
	if phaseOrder != "" {
		b.Fields["phase_order"] = phaseOrder
	}
	return b
}

func cents(usd float64) int64 { return int64(math.Round(usd * 100)) }

func TestPlans_SumsCrewAcrossPlansInOneBatch(t *testing.T) {
	f := newFake()
	// Plan A: phased. Plan B: flat. Hints on some edges spare their hydration.
	f.child("gd-A", "gd-phA", "epic", "epic")
	f.child("gd-phA", "gd-a1", "task", "task")
	f.child("gd-A", "gd-a2", "bug", "")
	f.child("gd-A", "gd-adoc", "bundle", "bundle")
	f.child("gd-B", "gd-b1", "feature", "")
	f.crew("gd-a1", "gd-x1", "working", "in_progress", "1.00", "0.25") // live: 1.25
	f.crew("gd-a2", "gd-x2", "done", "closed", "2.00", "5.00")         // 2.00
	f.crew("gd-adoc", "gd-x3", "working", "open", "9.00", "")          // not a step
	f.crew("gd-phA", "gd-x4", "working", "open", "9.00", "")           // a phase is not a step
	f.crew("gd-b1", "gd-y1", "idle", "closed", "0.333", "")            // 0.333
	f.crew("gd-B", "gd-y2", "spawning", "open", "", "0.10")            // live on the epic: 0.10
	f.rev["gd-b1"] = append(f.rev["gd-b1"], beadsapi.Dependency{BeadID: "gd-y1", DependsOnID: "gd-b1", Type: "blocks"})

	got, err := Plans(context.Background(), f, []*beadsapi.BeadDetail{plan("gd-A", `["gd-phA"]`), plan("gd-B", "")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if a := got["gd-A"]; cents(a.USD) != 325 || a.Crew != 2 || a.Live != 1 {
		t.Errorf("plan A = %+v, want $3.25, 2 crew, 1 live", a)
	}
	if b := got["gd-B"]; math.Abs(b.USD-0.433) > 1e-9 || b.Crew != 2 || b.Live != 1 {
		t.Errorf("plan B = %+v, want $0.433, 2 crew, 1 live", b)
	}
	// epics, phases, steps: three reverse-deps batches for the whole set.
	if f.depCalls != 3 {
		t.Errorf("BatchGetDependencies calls = %d, want 3", f.depCalls)
	}
	for _, id := range f.got {
		if id == "gd-phA" || id == "gd-a1" || id == "gd-adoc" {
			t.Errorf("hydrated %s although its edge carried its type", id)
		}
	}
}

func TestPlans_PhaseNotInOrderIsASubPlan(t *testing.T) {
	f := newFake()
	f.child("gd-P", "gd-sub", "epic", "")
	f.child("gd-sub", "gd-s1", "task", "")
	f.crew("gd-s1", "gd-x", "done", "closed", "4.00", "")
	got, err := Plans(context.Background(), f, []*beadsapi.BeadDetail{plan("gd-P", "")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["gd-P"]; r.Crew != 0 || r.USD != 0 {
		t.Errorf("sub-plan crew leaked into the plan: %+v", r)
	}
}

func TestPlans_ExcludesPlanGardenerAndNonAgents(t *testing.T) {
	f := newFake()
	g := f.crew("gd-P", "gd-g", "working", "open", "30.00", "")
	g.Fields["role"] = "plan-gardener"
	g2 := f.crew("gd-P", "gd-g2", "working", "open", "30.00", "")
	g2.Fields["role"] = ""
	g2.Labels = []string{"role:plan-gardener"}
	f.crew("gd-P", "gd-t", "", "open", "30.00", "").Type = "task"
	f.crew("gd-P", "gd-c", "working", "open", "1.00", "")
	got, err := Plans(context.Background(), f, []*beadsapi.BeadDetail{plan("gd-P", "")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["gd-P"]; r.Crew != 1 || cents(r.USD) != 100 {
		t.Errorf("got %+v, want only gd-c ($1.00)", r)
	}
}

func TestPlans_ErrorsFailTheWholeCall(t *testing.T) {
	for name, f := range map[string]*fakeSource{
		"deps":  {depsErr: errors.New("down")},
		"beads": {beadsErr: errors.New("down")},
	} {
		f.rev = map[string][]beadsapi.Dependency{"gd-P": {{BeadID: "gd-c", DependsOnID: "gd-P", Type: "assigned"}}}
		if got, err := Plans(context.Background(), f, []*beadsapi.BeadDetail{plan("gd-P", "")}, now); err == nil {
			t.Errorf("%s error: got %+v, want an error", name, got)
		}
	}
}

func TestPlans_NoCrewIsAZeroRollup(t *testing.T) {
	f := newFake()
	got, err := Plans(context.Background(), f, []*beadsapi.BeadDetail{plan("gd-P", "")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := got["gd-P"]; !ok || r != (Rollup{}) {
		t.Errorf("got %+v (present %v), want a zero rollup", r, ok)
	}
}

func TestCrewLive(t *testing.T) {
	for _, c := range []struct {
		state, status string
		age           time.Duration
		want          bool
	}{
		{"working", "in_progress", time.Minute, true},
		{"yielding", "open", 59 * time.Minute, true},
		{"spawning", "open", 0, true}, // no heartbeat yet reads fresh
		{"working", "open", time.Hour, false},
		{"working", "closed", time.Minute, false},
		{"idle", "open", time.Minute, false},
	} {
		b := &beadsapi.BeadDetail{Status: c.status, Fields: map[string]string{"agent_state": c.state}}
		if c.age > 0 {
			b.UpdatedAt = now.Add(-c.age)
		}
		if got := CrewLive(b, now); got != c.want {
			t.Errorf("%s/%s age %v: CrewLive = %v, want %v", c.state, c.status, c.age, got, c.want)
		}
	}
}

func TestRead(t *testing.T) {
	synced := now.Add(-20 * time.Minute).Format(time.RFC3339)
	old := now.Add(-31 * time.Minute).Format(time.RFC3339)
	for name, c := range map[string]struct {
		fields map[string]string
		want   Reading
	}{
		"unset":   {map[string]string{}, Reading{}},
		"garbage": {map[string]string{FieldUSD: "abc"}, Reading{}},
		"fresh": {map[string]string{FieldUSD: "1.2345", FieldSyncedAt: synced, FieldCrew: "3", FieldLive: "1"},
			Reading{Has: true, USD: 1.2345, SyncedAt: now.Add(-20 * time.Minute), Crew: 3, Live: 1}},
		"settled and old": {map[string]string{FieldUSD: "2", FieldSyncedAt: old, FieldCrew: "2", FieldLive: "0"},
			Reading{Has: true, USD: 2, SyncedAt: now.Add(-31 * time.Minute), Crew: 2}},
		"moving and old": {map[string]string{FieldUSD: "2", FieldSyncedAt: old, FieldCrew: "2", FieldLive: "1"},
			Reading{Has: true, USD: 2, SyncedAt: now.Add(-31 * time.Minute), Crew: 2, Live: 1, Stale: true}},
		"moving, no stamp": {map[string]string{FieldUSD: "2", FieldLive: "1"},
			Reading{Has: true, USD: 2, Live: 1, Stale: true}},
	} {
		if got := Read(c.fields, now); got != c.want {
			t.Errorf("%s: Read = %+v, want %+v", name, got, c.want)
		}
	}
}

// TestReadWithPass: an unchanged moving figure is not re-stamped
// (gd-vwMhcJgts2), so it stays fresh while the controller has written any
// plan within StaleAfter, and greys once none has.
func TestReadWithPass(t *testing.T) {
	moving := map[string]string{FieldUSD: "2", FieldSyncedAt: now.Add(-5 * time.Hour).Format(time.RFC3339), FieldCrew: "2", FieldLive: "1"}
	for name, c := range map[string]struct {
		fields map[string]string
		passAt time.Time
		stale  bool
	}{
		"own stamp old, pass fresh":   {moving, now.Add(-10 * time.Minute), false},
		"own stamp old, pass old":     {moving, now.Add(-StaleAfter - time.Minute), true},
		"own stamp old, no pass":      {moving, time.Time{}, true},
		"own stamp fresh, pass older": {map[string]string{FieldUSD: "2", FieldSyncedAt: now.Add(-time.Minute).Format(time.RFC3339), FieldLive: "1"}, now.Add(-time.Hour), false},
		"settled, pass old":           {map[string]string{FieldUSD: "2", FieldSyncedAt: moving[FieldSyncedAt]}, now.Add(-time.Hour), false},
	} {
		if got := ReadWithPass(c.fields, now, c.passAt); got.Stale != c.stale {
			t.Errorf("%s: ReadWithPass stale = %v, want %v", name, got.Stale, c.stale)
		}
	}
}
