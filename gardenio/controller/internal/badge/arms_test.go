package badge

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// mapLine backs a LineLookup with a fixed map and counts the reads.
func mapLine(nodes map[string]LineNode, reads *int) LineLookup {
	return func(_ context.Context, id string) (LineNode, error) {
		*reads++
		n, ok := nodes[id]
		if !ok {
			return LineNode{}, fmt.Errorf("no bead %s", id)
		}
		n.ID = id
		return n, nil
	}
}

func TestResolveArms_OwnArms(t *testing.T) {
	var reads int
	lookup := mapLine(map[string]LineNode{
		"gd-task": {Heraldry: "blue 🌱 . 🔥 . 📦", Parents: []string{"gd-plan"}},
		"gd-plan": {Heraldry: "brown 🏅 . 🖌️ . 🔎"},
	}, &reads)
	arms, err := ResolveArms(context.Background(), "gd-task", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if arms.Heraldry != "blue 🌱 . 🔥 . 📦" || arms.From != "gd-task" {
		t.Fatalf("got %+v, want the task's own arms", arms)
	}
	if arms.Inherited("gd-task") || arms.None() {
		t.Errorf("own arms reported inherited=%v none=%v", arms.Inherited("gd-task"), arms.None())
	}
	if reads != 1 {
		t.Errorf("own arms should stop after one read, got %d", reads)
	}
}

func TestResolveArms_TaskInheritsFromPlan(t *testing.T) {
	var reads int
	lookup := mapLine(map[string]LineNode{
		"gd-task":  {Parents: []string{"gd-phase"}},
		"gd-phase": {Parents: []string{"gd-plan"}},
		"gd-plan":  {Heraldry: "brown 🏅 . 🖌️ . 🔎", Parents: []string{"gd-proj"}},
		"gd-proj":  {Heraldry: "green 🌳 . 🏡"},
	}, &reads)
	arms, err := ResolveArms(context.Background(), "gd-task", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if arms.Heraldry != "brown 🏅 . 🖌️ . 🔎" || arms.From != "gd-plan" {
		t.Fatalf("got %+v, want the plan's arms (nearest armed ancestor)", arms)
	}
	if !arms.Inherited("gd-task") {
		t.Error("arms from the plan must be reported inherited")
	}
	if reads != 3 {
		t.Errorf("walk should stop at the plan (3 reads), got %d", reads)
	}
}

func TestResolveArms_NoArmedAncestor(t *testing.T) {
	var reads int
	lookup := mapLine(map[string]LineNode{
		"gd-task":  {Parents: []string{"gd-phase"}},
		"gd-phase": {Parents: []string{"gd-plan"}},
		"gd-plan":  {Heraldry: "   "},
	}, &reads)
	arms, err := ResolveArms(context.Background(), "gd-task", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !arms.None() || arms.From != "" {
		t.Fatalf("got %+v, want no arms", arms)
	}
	if arms.Inherited("gd-task") {
		t.Error("no arms cannot be inherited")
	}
	if reads != 3 {
		t.Errorf("walk should read the whole line (3), got %d", reads)
	}
}

func TestResolveArms_CycleEnds(t *testing.T) {
	var reads int
	lookup := mapLine(map[string]LineNode{
		"gd-a": {Parents: []string{"gd-b"}},
		"gd-b": {Parents: []string{"gd-a"}},
	}, &reads)
	arms, err := ResolveArms(context.Background(), "gd-a", lookup)
	if err != nil || !arms.None() {
		t.Fatalf("cycle: got %+v, %v; want no arms, no error", arms, err)
	}
	if reads != 2 {
		t.Errorf("cycle should read each bead once, got %d", reads)
	}
}

func TestResolveArms_DepthBounded(t *testing.T) {
	nodes := map[string]LineNode{}
	for i := range 3 * maxLineReads {
		nodes[fmt.Sprintf("gd-%d", i)] = LineNode{Parents: []string{fmt.Sprintf("gd-%d", i+1)}}
	}
	var reads int
	arms, err := ResolveArms(context.Background(), "gd-0", mapLine(nodes, &reads))
	if err != nil || !arms.None() {
		t.Fatalf("deep line: got %+v, %v; want no arms, no error", arms, err)
	}
	if reads != maxLineReads {
		t.Errorf("walk should stop at %d reads, got %d", maxLineReads, reads)
	}
}

func TestResolveArms_LookupErrorIsNoArms(t *testing.T) {
	boom := errors.New("daemon down")
	lookup := func(_ context.Context, id string) (LineNode, error) {
		if id == "gd-task" {
			return LineNode{ID: id, Parents: []string{"gd-plan"}}, nil
		}
		return LineNode{}, boom
	}
	arms, err := ResolveArms(context.Background(), "gd-task", lookup)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the lookup error wrapped", err)
	}
	if !arms.None() {
		t.Errorf("a failed walk must report no arms, got %+v", arms)
	}
}

// A thread-filed task lists its thread bead (no arms) before its phase
// (armed epic); it must still inherit the phase's arms (gd-NgFx1XzPAL).
func TestResolveArms_ThreadParentBeforePhase(t *testing.T) {
	var reads int
	lookup := mapLine(map[string]LineNode{
		"gd-task":   {Type: "task", Parents: []string{"gd-thread", "gd-phase"}},
		"gd-thread": {Type: "task"},
		"gd-phase":  {Type: "epic", Heraldry: "brown 📊 . 💵 . 🚢", Parents: []string{"gd-plan"}},
		"gd-plan":   {Type: "epic", Heraldry: "green 🌳 . 🏡"},
	}, &reads)
	arms, err := ResolveArms(context.Background(), "gd-task", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if arms.Heraldry != "brown 📊 . 💵 . 🚢" || arms.From != "gd-phase" {
		t.Fatalf("got %+v, want the phase's arms past the armless thread parent", arms)
	}
	if reads != 3 {
		t.Errorf("walk should stop at the phase (3 reads), got %d", reads)
	}
}

// A nearer armed ancestor beats a farther one, whichever edge it hangs off:
// the thread's own parent is two levels up, the phase one.
func TestResolveArms_NearestWinsAcrossEdges(t *testing.T) {
	var reads int
	lookup := mapLine(map[string]LineNode{
		"gd-task":   {Parents: []string{"gd-thread", "gd-phase"}},
		"gd-thread": {Parents: []string{"gd-far"}},
		"gd-far":    {Type: "epic", Heraldry: "green 🌳 . 🏡"},
		"gd-phase":  {Type: "epic", Heraldry: "brown 📊 . 💵 . 🚢"},
	}, &reads)
	arms, err := ResolveArms(context.Background(), "gd-task", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if arms.From != "gd-phase" {
		t.Fatalf("got %+v, want the nearer phase's arms", arms)
	}
}

// Among armed parents at the same distance an epic wins over a non-epic
// listed first; with no epic armed, the first armed edge wins.
func TestResolveArms_EpicFirstAtSameDistance(t *testing.T) {
	var reads int
	lookup := mapLine(map[string]LineNode{
		"gd-task":   {Parents: []string{"gd-thread", "gd-phase"}},
		"gd-thread": {Type: "task", Heraldry: "blue 🧵"},
		"gd-phase":  {Type: "epic", Heraldry: "brown 📊 . 💵 . 🚢"},
	}, &reads)
	arms, err := ResolveArms(context.Background(), "gd-task", lookup)
	if err != nil || arms.From != "gd-phase" {
		t.Fatalf("got %+v, %v; want the epic phase's arms", arms, err)
	}

	reads = 0
	lookup = mapLine(map[string]LineNode{
		"gd-task": {Parents: []string{"gd-a", "gd-b"}},
		"gd-a":    {Type: "task", Heraldry: "blue 🧵"},
		"gd-b":    {Type: "task", Heraldry: "red 🔥"},
	}, &reads)
	arms, err = ResolveArms(context.Background(), "gd-task", lookup)
	if err != nil || arms.From != "gd-a" {
		t.Fatalf("got %+v, %v; want the first armed edge", arms, err)
	}
}

// A diamond (two parents sharing a grandparent) reads the shared bead once.
func TestResolveArms_DiamondReadsOnce(t *testing.T) {
	var reads int
	lookup := mapLine(map[string]LineNode{
		"gd-task": {Parents: []string{"gd-a", "gd-b"}},
		"gd-a":    {Parents: []string{"gd-top"}},
		"gd-b":    {Parents: []string{"gd-top"}},
		"gd-top":  {},
	}, &reads)
	arms, err := ResolveArms(context.Background(), "gd-task", lookup)
	if err != nil || !arms.None() {
		t.Fatalf("got %+v, %v; want no arms", arms, err)
	}
	if reads != 4 {
		t.Errorf("diamond should read each bead once (4), got %d", reads)
	}
}
