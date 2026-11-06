package gardenhub

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// homeWorld is a small farm with something in every cell of three home strips: gardenio's, zeta's and the Commons's.
func homeWorld() *fakeFarmSrc {
	f := newFakeFarm()
	f.add(planBead("gd-hP", "building", "in_progress", "gd-hP1"),
		fb("gd-hP1", "epic", "in_progress", "gd-hP", 10),
		fb("gd-hP1a", "task", "in_progress", "gd-hP1", 20), // on a plot: not in the Growing field
		fb("gd-hPd", "decision", "open", "gd-hP1", 30),     // a letter on a plan is still the project's letter
		fb("gd-hL1", "task", "in_progress", "", 40),        // growing
		fb("gd-hL2", "bug", "open", "", 41),                // a wildflower
		fb("gd-hE1", "escalation", "open", "", 42),
		wishBead("gd-hW1", "open", "slack", 43),
		zb("gd-hZ1", "task", "open", "", 50),
		zb("gd-hZd", "decision", "open", "", 51),
	)
	zw := wishBead("gd-hZw", "open", "commons", 52)
	zw.Labels = []string{"project:zeta", "source:commons", "thread"}
	nd := fb("gd-hNd", "decision", "open", "", 60)
	nd.Labels = nil
	nl := fb("gd-hNl", "chore", "in_progress", "", 61)
	nl.Labels = nil
	f.add(zw, nd, nl)

	field := agentBead("gd-gField", "field", "crew", "", "", 70) // no label: its project is a field
	field.Labels, field.Fields["project"] = nil, "zeta"
	f.add(
		agentBead("gd-gHome", "home", "crew", "", "", 70),                  // gardenio's barn
		agentBead("gd-gOn", "on", "crew", "working", "gd-hP1a", 70),        // out on its plot
		agentBead("gd-gStuck", "stuck", "crew", "failed", "gd-hP1a", 70),   // out on its plot, stuck
		agentBead("gd-gIdle", "idle", "crew", "", "gd-hP1a", 70),           // idle: back at home, not out on its plot
		agentBead("gd-gMeadow", "meadow", "crew", "working", "gd-hL1", 70), // a meadow bead has no plot
		agentBead("gd-gSteward", "steward", "steward", "working", "", 70),  // at its land's sign
		agentBead("gd-gThread", "thread", "thread", "working", "", 70),     // the Commons's barn
		field,
	)
	return f
}

func farmHomes(t *testing.T, raw json.RawMessage) FarmHomes {
	t.Helper()
	var h FarmHomes
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("home: %v\n%s", err, raw)
	}
	return h
}

func TestFarmHomeCounts(t *testing.T) {
	f := newTestFarm(homeWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc := farmDoc(t, f)
	want := FarmHomes{
		"gardenio": {Letters: 2, Field: 2, Growing: 1, Barn: 3, Wishes: 1},
		"zeta":     {Letters: 1, Field: 1, Barn: 1, Wishes: 1},
		"":         {Letters: 1, Field: 1, Growing: 1, Barn: 1},
	}
	if got := farmHomes(t, doc["home"]); !reflect.DeepEqual(got, want) {
		t.Errorf("home = %+v, want %+v", got, want)
	}

	// A project-scoped viewer gets its own row only: never the Commons's, never another land's.
	_, raw, _, _ := cutDoc(t, f, "zeta")
	if got := farmHomes(t, raw["home"]); !reflect.DeepEqual(got, FarmHomes{"zeta": want["zeta"]}) {
		t.Errorf("zeta's home = %+v, want zeta's row only", got)
	}
	_, raw, _, _ = cutDoc(t, f, "nobody")
	if got := farmHomes(t, raw["home"]); len(got) != 0 {
		t.Errorf("an empty set's home = %+v, want {}", got)
	}

	// A new letter moves the counts, and the whole farm's patch carries the blob.
	_, live, cancel := f.Subscribe(0)
	defer cancel()
	f.src.(*fakeFarmSrc).add(fb("gd-hE2", "escalation", "open", "", 80))
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := nextPatch(t, live)
	if got := farmHomes(t, p.Home)["gardenio"].Letters; got != 3 {
		t.Errorf("patched gardenio letters = %d, want 3 (patch %s)", got, p.Home)
	}
}

// A gardener's cut decides whether it is in a scoped viewer's barn: one the cut hides (its task in another land) is not
// counted, so the count names nothing the viewer cannot see.
func TestFarmHomeCutCountsOnlyAdmittedGardeners(t *testing.T) {
	src := homeWorld()
	src.add(agentBead("gd-gAway", "away", "crew", "", "gd-hZ1", 70)) // a gardenio gardener sent to a zeta bead
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := farmHomes(t, farmDoc(t, f)["home"])["gardenio"].Barn; got != 4 {
		t.Errorf("whole farm: gardenio barn = %d, want 4", got)
	}
	_, raw, _, _ := cutDoc(t, f, "gardenio")
	if got := farmHomes(t, raw["home"])["gardenio"].Barn; got != 3 {
		t.Errorf("gardenio's cut: barn = %d, want 3 (the one sent to zeta is hidden)", got)
	}
}

func TestBarnOf(t *testing.T) {
	plot := &FarmPlot{Plan: "gd-p"}
	for _, c := range []struct {
		g    FarmGardener
		key  string
		home bool
	}{
		{FarmGardener{Project: "gardenio", Role: "crew", Bucket: GardenerIdle}, "gardenio", true},
		{FarmGardener{Project: "gardenio", Role: "crew", Bucket: GardenerAlive, Plot: plot}, "", false},
		{FarmGardener{Project: "gardenio", Role: "crew", Bucket: GardenerStuck, Plot: plot}, "", false},
		{FarmGardener{Project: "gardenio", Role: "crew", Bucket: GardenerDormant, Plot: plot}, "gardenio", true},
		{FarmGardener{Project: "gardenio", Role: "Steward", Bucket: GardenerAlive}, "", false},
		{FarmGardener{Role: "steward", Bucket: GardenerAlive}, "", true}, // no land to stand at
		{FarmGardener{Project: "gardenio", Role: "crew, dm", Bucket: GardenerAlive}, "", true},
		{FarmGardener{Role: "crew", Bucket: GardenerIdle}, "", true},
	} {
		key, home := barnOf(c.g)
		if key != c.key || home != c.home {
			t.Errorf("barnOf(%+v) = %q, %v; want %q, %v", c.g, key, home, c.key, c.home)
		}
	}
}

// lettersOf counts what the Mailbox lists: open decisions and escalations, not a resolved one still in the rows.
func TestLettersOf(t *testing.T) {
	d := fb("gd-d", "decision", "open", "", 1)
	e := fb("gd-e", "escalation", "open", "", 1)
	ip := fb("gd-x", "escalation", "in_progress", "", 1)
	tk := fb("gd-t", "task", "open", "", 1)
	if got := lettersOf([]*beadsapi.BeadDetail{d, e, ip, tk}); !reflect.DeepEqual(got, map[string]int{"gardenio": 2}) {
		t.Errorf("lettersOf = %v", got)
	}
}
