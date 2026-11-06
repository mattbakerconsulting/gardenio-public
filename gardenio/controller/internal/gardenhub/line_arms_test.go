package gardenhub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const (
	planArms  = "🧭🔥📦"
	phaseArms = "📦🔥🧭"
)

// lineFake is a daemon with a fixed bead graph for the line walk: beads by id and their parent-child parents in order.
type lineFake struct {
	mu      sync.Mutex
	beads   map[string]*beadsapi.BeadDetail
	parents map[string][]string
	reads   int // ids asked for, over every batch read
	fail    bool
}

func (f *lineFake) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("daemon down")
	}
	f.reads += len(ids)
	out := map[string]*beadsapi.BeadDetail{}
	for _, id := range ids {
		if b := f.beads[id]; b != nil {
			out[id] = b
		}
	}
	return out, nil
}

func (f *lineFake) BatchGetDependencies(_ context.Context, ids []string, _ string) (map[string][]beadsapi.Dependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]beadsapi.Dependency{}
	for _, id := range ids {
		for _, p := range f.parents[id] {
			out[id] = append(out[id], beadsapi.Dependency{BeadID: id, DependsOnID: p, Type: "parent-child"})
		}
	}
	return out, nil
}

func (f *lineFake) deps(id string) []beadsapi.Dependency {
	d, _ := f.BatchGetDependencies(context.Background(), []string{id}, "forward")
	return d[id]
}

// GetBead, GetDependencies, GetComments and GetReverseDependencies make it the bead view's BeadReader too.
func (f *lineFake) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b := f.beads[id]; b != nil {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: 404, Message: "no such bead"}
}
func (f *lineFake) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	return f.deps(id), nil
}
func (f *lineFake) GetComments(context.Context, string) ([]beadsapi.Comment, error) { return nil, nil }
func (f *lineFake) GetReverseDependencies(context.Context, string) ([]beadsapi.Dependency, error) {
	return nil, nil
}

// lineFarm: a plan with arms, a phase without, a task under the phase (and under a thread bead first, as a thread-filed
// task is), a phase with arms of its own and its task, a task that stored arms before R6, and a loose task.
func lineFarm() *lineFake {
	b := func(id, typ, arms string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: id, Title: "t " + id, Type: typ, Status: "open", Heraldry: arms}
	}
	agent := func(id, task string) *beadsapi.BeadDetail {
		a := b(id, "agent", "")
		a.Fields = map[string]string{"task_id": task}
		return a
	}
	return &lineFake{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-Plan000001": b("gd-Plan000001", "epic", planArms),
			"gd-Phase00001": b("gd-Phase00001", "epic", ""),
			"gd-Phase00002": b("gd-Phase00002", "epic", phaseArms),
			"gd-Thread0001": b("gd-Thread0001", "task", ""),
			"gd-Task000001": b("gd-Task000001", "task", ""),
			"gd-Task000002": b("gd-Task000002", "bug", ""),
			"gd-Task000003": b("gd-Task000003", "task", planArms), // arms an older daemon took on a task: never shown
			"gd-Loose00001": b("gd-Loose00001", "task", ""),
			"gd-Agent00001": agent("gd-Agent00001", "gd-Task000002"), // works a task under the armed phase
			"gd-Agent00002": agent("gd-Agent00002", "gd-Loose00001"), // works a task with no armed line
		},
		parents: map[string][]string{
			"gd-Phase00001": {"gd-Plan000001"},
			"gd-Phase00002": {"gd-Plan000001"},
			"gd-Task000001": {"gd-Thread0001", "gd-Phase00001"},
			"gd-Task000002": {"gd-Phase00002"},
			"gd-Task000003": {"gd-Loose00001"},
		},
	}
}

func TestLineArmsResolve(t *testing.T) {
	f := lineFarm()
	c := newLineArms()
	got := c.resolve(context.Background(), f, []string{"gd-Task000001", "gd-Task000002", "gd-Task000003", "gd-Loose00001", "gd-Plan000001", "gd-Gone000001", "gd-Agent00001", "gd-Agent00002"})
	want := map[string]string{
		"gd-Task000001": planArms,  // through the thread bead and the unarmed phase to the plan
		"gd-Task000002": phaseArms, // the nearest armed ancestor, not the plan above it
		"gd-Plan000001": planArms,  // an armed epic shows its own
		"gd-Agent00001": phaseArms, // an agent shows its task's line (gd-ASLjQPkHJz)
	}
	for id, w := range want {
		if got[id] == nil || got[id].Canonical != w {
			t.Errorf("%s: line arms %+v, want %q", id, got[id], w)
		}
	}
	for _, id := range []string{"gd-Task000003", "gd-Loose00001", "gd-Gone000001", "gd-Agent00002"} {
		if got[id] != nil {
			t.Errorf("%s: line arms %+v, want none", id, got[id])
		}
	}
	if a := got["gd-Task000001"]; a == nil || a.N != 3 || !strings.HasPrefix(a.Name, "arms: ") {
		t.Errorf("task arms carry count and name: %+v", a)
	}

	// The second refresh reads nothing: every node is cached.
	before := f.reads
	c.resolve(context.Background(), f, []string{"gd-Task000001", "gd-Task000002"})
	if f.reads != before {
		t.Errorf("cached resolve read %d more beads", f.reads-before)
	}
}

func TestLineArmsDaemonDown(t *testing.T) {
	f := lineFarm()
	f.fail = true
	if got := newLineArms().resolve(context.Background(), f, []string{"gd-Task000001"}); len(got) != 0 {
		t.Errorf("a failed read shows no arms, got %+v", got)
	}
}

// A ledger row (the home strips' letters, plants and figures) draws the line arms before the title, for both skies, and the item carries them in words.
func TestHubRowWearsLineArms(t *testing.T) {
	lf := lineFarm()
	src := &hubLineSource{fakeSource: newFake(), lineFake: lf}
	task := bead("gd-Task000001", "task", 100)
	loose := bead("gd-Loose00001", "task", 101)
	agent := bead("gd-Agent00001", "agent", 102) // the barn's agents wear their task's line arms too (5.9, gd-QboM9sPYCg)
	src.set([]*beadsapi.BeadDetail{task, loose, agent}, nil)
	h := New(src, "/v", nil)
	if err := h.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := h.frags["gd-Task000001"]
	for _, w := range []string{
		`data-aname="arms: `,
		`<span class="ar-pic ar-line"><img class="ar-day" src="/v/garden/arms.svg?a=`,
		`&amp;theme=night"`,
		`width="66" height="20"`,
	} {
		if !strings.Contains(row, w) {
			t.Errorf("row lacks %q:\n%s", w, row)
		}
	}
	if i, j := strings.Index(row, "ar-line"), strings.Index(row, "t gd-Task000001"); i < 0 || j < i {
		t.Errorf("the arms come before the title:\n%s", row)
	}
	if a := h.items["gd-Agent00001"]; a.Section != SecAgent || a.Arms != phaseArms {
		t.Errorf("an agent row wears its task's line arms: section %q arms %q, want %q", a.Section, a.Arms, phaseArms)
	}
	if strings.Contains(h.frags["gd-Loose00001"], "ar-line") {
		t.Errorf("a bead whose line bears no arms shows none:\n%s", h.frags["gd-Loose00001"])
	}
}

type hubLineSource struct {
	*fakeSource
	*lineFake
}

// The bead view sends a task its line's arms and names the epic that bears them; an epic sends its own as arms.
func TestBeadViewLineArms(t *testing.T) {
	f := lineFarm()
	d, err := readBead(context.Background(), f, "gd-Task000001")
	if err != nil {
		t.Fatal(err)
	}
	if d.Arms != nil {
		t.Errorf("a task bears no arms of its own: %+v", d.Arms)
	}
	if d.LineArms == nil || d.LineArms.Canonical != planArms || d.LineFrom == nil || d.LineFrom.ID != "gd-Plan000001" || d.LineFrom.Title != "t gd-Plan000001" {
		t.Errorf("task line arms %+v from %+v, want the plan's", d.LineArms, d.LineFrom)
	}
	d, err = readBead(context.Background(), f, "gd-Phase00002")
	if err != nil {
		t.Fatal(err)
	}
	if d.Arms == nil || d.Arms.Canonical != phaseArms || d.LineArms != nil {
		t.Errorf("an armed epic shows its own arms only: arms %+v line %+v", d.Arms, d.LineArms)
	}
	d, err = readBead(context.Background(), f, "gd-Loose00001")
	if err != nil {
		t.Fatal(err)
	}
	if d.LineArms != nil {
		t.Errorf("a loose task has no line arms: %+v", d.LineArms)
	}
}

// An agent bead bears no arms; its bead view sends its task's line arms and names the epic that bears them, and an agent
// whose task has no armed line gets none (gd-ASLjQPkHJz).
func TestBeadViewAgentLineArms(t *testing.T) {
	f := lineFarm()
	d, err := readBead(context.Background(), f, "gd-Agent00001")
	if err != nil {
		t.Fatal(err)
	}
	if d.Arms != nil {
		t.Errorf("an agent bears no arms of its own: %+v", d.Arms)
	}
	if d.LineArms == nil || d.LineArms.Canonical != phaseArms || d.LineFrom == nil || d.LineFrom.ID != "gd-Phase00002" || d.LineFrom.Title != "t gd-Phase00002" {
		t.Errorf("agent line arms %+v from %+v, want its task's phase's", d.LineArms, d.LineFrom)
	}
	d, err = readBead(context.Background(), f, "gd-Agent00002")
	if err != nil {
		t.Fatal(err)
	}
	if d.LineArms != nil || d.LineFrom != nil {
		t.Errorf("an agent whose task has no armed line shows none: %+v from %+v", d.LineArms, d.LineFrom)
	}
}
