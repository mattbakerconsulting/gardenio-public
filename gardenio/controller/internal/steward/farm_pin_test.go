package steward

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/gardenhub"
)

// TestFarmStewardMatchesEngine pins gardenhub's copies (farm_steward.go,
// gd-srGLTTTxPb): the labels it finds a steward by, and the cap rule the land
// header shows, which must be the engine's own (bridge.GardenerDailyRunCapHit).
func TestFarmStewardMatchesEngine(t *testing.T) {
	if gardenhub.StewardMarkerLabel != MarkerLabel || gardenhub.StewardLandLabelPrefix != LandLabelPrefix {
		t.Errorf("gardenhub finds stewards by %q and %q; steward binds them with %q and %q",
			gardenhub.StewardMarkerLabel, gardenhub.StewardLandLabelPrefix, MarkerLabel, LandLabelPrefix)
	}
	now := time.Date(2026, 10, 23, 23, 59, 0, 0, time.UTC)
	for _, g := range []bridge.GardenerSpec{
		{MaxRunsPerDay: DefaultMaxRunsPerDay, RunsDay: "2026-10-23", RunsToday: 23},
		{MaxRunsPerDay: DefaultMaxRunsPerDay, RunsDay: "2026-10-23", RunsToday: 24},
		{MaxRunsPerDay: DefaultMaxRunsPerDay, RunsDay: "2026-10-22", RunsToday: 24},
		{MaxRunsPerDay: 1, RunsDay: "2026-10-23", RunsToday: 3},
		{MaxRunsPerDay: DefaultMaxRunsPerDay},
		{RunsDay: "2026-10-23", RunsToday: 50},
		{MaxRunsPerDay: -1, RunsDay: "2026-10-23", RunsToday: 50},
	} {
		raw, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		s, ok := gardenhub.StewardRunCap(string(raw), now)
		if !ok {
			t.Fatalf("gardenhub cannot read the engine's gardener blob %s", raw)
		}
		if want := bridge.GardenerDailyRunCapHit(&g, now); s.Capped != want {
			t.Errorf("blob %s: gardenhub says capped=%v, the engine %v", raw, s.Capped, want)
		}
	}
}

// TestFarmStewardReadsMemory pins the rest of gardenhub's copies for the
// steward on the map (step 9.8): its role and name, the notes title, and the
// sections it reads its reason from, on notes as this package writes them.
func TestFarmStewardReadsMemory(t *testing.T) {
	if gardenhub.StewardRole != Role || gardenhub.StewardNotesPrefix+"tomato" != NotesTitle("tomato") {
		t.Errorf("gardenhub names the steward %q and its notes %q; steward has %q and %q",
			gardenhub.StewardRole, gardenhub.StewardNotesPrefix+"tomato", Role, NotesTitle("tomato"))
	}
	if gardenhub.StewardSectionInFlight != SectionInFlight || gardenhub.StewardSectionPromises != SectionPromises {
		t.Errorf("gardenhub reads sections %q and %q; steward writes %q and %q",
			gardenhub.StewardSectionInFlight, gardenhub.StewardSectionPromises, SectionInFlight, SectionPromises)
	}
	if !strings.HasPrefix(Author("tomato"), "agent:tomato-"+gardenhub.StewardRole) {
		t.Errorf("the steward posts as %q; gardenhub names it tomato-%s", Author("tomato"), gardenhub.StewardRole)
	}
	empty := EmptyNotes("tomato")
	if r := gardenhub.StewardReason(nil, empty, func(string) bool { return false }); r != nil {
		t.Errorf("a new land's empty notes read as reason %+v", r)
	}
	for _, c := range []struct{ section, line, kind string }{
		{SectionInFlight, "Plan X: crew on step 2", gardenhub.ReasonInFlight},
		{SectionPromises, "digest at 18:00", gardenhub.ReasonPromise},
	} {
		notes := strings.Replace(empty, "## "+c.section+"\n", "## "+c.section+"\n- "+c.line+"\n", 1)
		if notes == empty {
			t.Fatalf("no %q heading in EmptyNotes:\n%s", c.section, empty)
		}
		r := gardenhub.StewardReason(nil, notes, func(string) bool { return false })
		if r == nil || r.Kind != c.kind || r.Text != c.line {
			t.Errorf("notes with %q under %s read as %+v; want %s %q", c.line, c.section, r, c.kind, c.line)
		}
	}
}
