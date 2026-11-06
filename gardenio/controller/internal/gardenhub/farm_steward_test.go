package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// stewardTrigger is a land's steward trigger as steward.Bind labels it.
func stewardTrigger(id, land, gardener string) *beadsapi.BeadDetail {
	b := fb(id, "trigger", "open", "", 1)
	b.Labels = []string{"trigger:multi", "role:gardener", StewardMarkerLabel, StewardLandLabelPrefix + land, "project:" + land, "perpetual:true"}
	b.Fields = map[string]string{"gardener": gardener}
	return b
}

func TestStewardRunCap(t *testing.T) {
	now := time.Date(2026, 10, 23, 17, 30, 0, 0, time.UTC)
	midnight := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).Unix()
	for _, c := range []struct {
		name, blob string
		want       FarmSteward
	}{
		{"under the cap", `{"max_runs_per_day":24,"runs_day":"2026-10-23","runs_today":3}`, FarmSteward{MaxRunsPerDay: 24, RunsToday: 3}},
		{"at the cap", `{"max_runs_per_day":24,"runs_day":"2026-10-23","runs_today":24}`, FarmSteward{MaxRunsPerDay: 24, RunsToday: 24, Capped: true, ResumesAt: midnight}},
		{"over the cap", `{"max_runs_per_day":2,"runs_day":"2026-10-23","runs_today":5}`, FarmSteward{MaxRunsPerDay: 2, RunsToday: 5, Capped: true, ResumesAt: midnight}},
		{"yesterday's runs", `{"max_runs_per_day":24,"runs_day":"2026-10-22","runs_today":24}`, FarmSteward{MaxRunsPerDay: 24}},
		{"no runs yet", `{"max_runs_per_day":24}`, FarmSteward{MaxRunsPerDay: 24}},
		{"no cap", `{"runs_day":"2026-10-23","runs_today":99}`, FarmSteward{RunsToday: 99}},
	} {
		got, ok := StewardRunCap(c.blob, now)
		if !ok || got != c.want {
			t.Errorf("%s: StewardRunCap = %+v, %v; want %+v", c.name, got, ok, c.want)
		}
	}
	if _, ok := StewardRunCap("not json", now); ok {
		t.Error("a blob that is not JSON read as a steward")
	}
}

// TestStewardReason: the map's steward says the purse wall first, else its
// memory's first In flight line, else its first Promises line, as written
// (step 9.8; amendment §8: "the reason text comes from memory"); a line naming
// a handle still waiting on a person is parked on it; empty memory says nothing.
func TestStewardReason(t *testing.T) {
	notes := func(inFlight, promises string) string {
		return "Steward notes for tomato.\n\n## Holds\n- never merge #12\n\n## Answers (newest first)\n- passkeys only (gd-dOld001)\n\n## Promises\n" + promises + "\n## In flight\n" + inFlight
	}
	waiting := func(id string) bool { return id == "gd-dPlanX1" }
	wall, ok := &purse.Purse{State: purse.StateWall}, &purse.Purse{State: purse.StateOK}
	for _, c := range []struct {
		name  string
		p     *purse.Purse
		notes string
		want  *FarmReason
	}{
		{"the wall wins over memory", wall, notes("- Plan X: waiting on you (gd-dPlanX1)\n", ""), &FarmReason{Kind: ReasonSleeping, Text: StewardParkedText}},
		{"in flight, its first line", ok, notes("- **Plan X**: crew on step 2, PR `#14` open\n  more under it\n- second\n", "- re-run the check at 14:00\n"),
			&FarmReason{Kind: ReasonInFlight, Text: "Plan X: crew on step 2, PR #14 open"}},
		{"in flight naming a waiting handle", nil, notes("* Waiting on you: approve plan X (gd-dPlanX1)\n", ""),
			&FarmReason{Kind: ReasonParked, Text: "Waiting on you: approve plan X (gd-dPlanX1)", Href: "/garden?bead=gd-dPlanX1"}},
		{"in flight naming an answered handle", ok, notes("- plan X approved (gd-dOld001), crew next\n", ""), &FarmReason{Kind: ReasonInFlight, Text: "plan X approved (gd-dOld001), crew next"}},
		{"nothing in flight: the first promise", ok, notes("nothing yet\n", "- re-run the VAP check at 14:00\n"), &FarmReason{Kind: ReasonPromise, Text: "re-run the VAP check at 14:00"}},
		{"a heading with more words", ok, "## In flight (plans, PRs, crews)\n- one PR\n", &FarmReason{Kind: ReasonInFlight, Text: "one PR"}},
		{"Holds and Answers are not reasons", ok, notes("", ""), nil},
		{"no notes", nil, "", nil},
	} {
		got := StewardReason(c.p, c.notes, waiting)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: StewardReason = %+v, want %+v", c.name, got, c.want)
		}
	}
	long := notes("- "+strings.Repeat("word ", 60)+"\n", "")
	if r := StewardReason(nil, long, waiting); r == nil || len([]rune(r.Text)) > maxReasonText {
		t.Errorf("a long memory line = %+v; want it clipped to %d runes", r, maxReasonText)
	}
}

func TestStewardLand(t *testing.T) {
	for _, c := range []struct {
		labels []string
		want   string
	}{
		{[]string{"steward", "gardener-land:tomato", "project:tomato"}, "tomato"},
		{[]string{"steward", "gardener-land:tomato", "project:gardenio"}, ""}, // bound to a land it is not in
		{[]string{"steward", "gardener-land:tomato", "gardener-land:basil", "project:tomato", "project:basil"}, ""},
		{[]string{"steward", "project:tomato"}, ""},
	} {
		if got := stewardLand(c.labels); got != c.want {
			t.Errorf("stewardLand(%v) = %q, want %q", c.labels, got, c.want)
		}
	}
}

// notesBead is a land's steward-notes:<land> doc bead, its body in fields.content.
func notesBead(id, land, content string) *beadsapi.BeadDetail {
	b := fb(id, "doc", "open", "", 1)
	b.Title, b.Labels, b.Fields = StewardNotesPrefix+land, []string{"project:" + land}, map[string]string{"content": content, "format": "md"}
	return b
}

// stewardFailSrc fails every trigger read while fail is set.
type stewardFailSrc struct {
	*fakeFarmSrc
	fail *bool
}

func (s stewardFailSrc) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if *s.fail && (in(q.Types, "trigger") || strings.HasPrefix(q.Search, StewardNotesPrefix)) {
		return nil, fmt.Errorf("daemon down")
	}
	return s.fakeFarmSrc.ListBeadsFiltered(ctx, q)
}

// TestFarmCostSteward: a land's row carries its steward's run cap (spec §4.3),
// capped when today's runs reach it; a land with no steward, or a steward
// bound by a stray label, carries none; the row names the land's notes bead
// (the land header's link, gd-q4NgYNGpy8); a failed trigger read keeps the
// last triggers and notes, and the cap still lifts at midnight.
func TestFarmCostSteward(t *testing.T) {
	now := time.Date(2026, 10, 23, 17, 30, 0, 0, time.UTC)
	src := newFakeFarm()
	paused := stewardTrigger("gd-stw2", "tomato", `{"max_runs_per_day":24}`)
	paused.Status = "blocked"
	stray := stewardTrigger("gd-stw3", "basil", `{"max_runs_per_day":1,"runs_day":"2026-10-23","runs_today":1}`)
	stray.Labels = append(stray.Labels[:4], "project:gardenio") // gardener-land:basil inside gardenio
	src.add(
		projectBead("gd-proj1", "gardenio", map[string]string{}),
		projectBead("gd-proj2", "tomato", map[string]string{}),
		projectBead("gd-proj3", "basil", map[string]string{}),
		stewardTrigger("gd-stw1", "tomato", `{"max_runs_per_day":24,"runs_day":"2026-10-23","runs_today":24}`),
		paused, stray, notesBead("gd-nts1", "tomato", "## Promises\n- digest at 18:00\n\n## In flight\n- Waiting on you: approve plan X (gd-dPlanX1)\n"),
	)
	ask := fb("gd-dPlanX1", "decision", "open", "", 1)
	ask.Labels, ask.Title = []string{"project:tomato"}, "Approve plan X?"
	src.add(ask)
	fail := false
	f := newTestFarm(stewardFailSrc{src, &fail})
	f.now = func() time.Time { return now }
	f.SetSpendSource(&fakeSpend{byWin: map[time.Duration]map[string]float64{}})
	read := func() map[string]*FarmSteward {
		t.Helper()
		f.slow.at, f.costSt.at = time.Time{}, time.Time{}
		if err := f.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		var cost FarmCost
		if err := json.Unmarshal(farmDoc(t, f)["cost"], &cost); err != nil {
			t.Fatal(err)
		}
		out := map[string]*FarmSteward{}
		for _, r := range cost.Projects {
			out[r.Project] = r.Steward
		}
		return out
	}

	got := read()
	want := FarmSteward{ID: "gd-stw1", Href: "/garden?bead=gd-stw1", Agent: "tomato-steward", MaxRunsPerDay: 24, RunsToday: 24, Capped: true,
		ResumesAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).Unix(), Notes: "gd-nts1"}
	wantReason := FarmReason{Kind: ReasonParked, Text: "Waiting on you: approve plan X (gd-dPlanX1)", Href: "/garden?bead=gd-dPlanX1"}
	if s := got["tomato"]; s == nil || s.Reason == nil || *s.Reason != wantReason {
		t.Errorf("tomato's steward = %+v, want reason %+v from its memory", s, wantReason)
	} else if s.Reason = nil; *s != want {
		t.Errorf("tomato's steward = %+v, want %+v", s, want)
	}
	if got["gardenio"] != nil || got["basil"] != nil {
		t.Errorf("gardenio %+v / basil %+v carry a steward; the only one is tomato's", got["gardenio"], got["basil"])
	}

	fail, now = true, now.Add(7*time.Hour) // past midnight, the daemon down
	got = read()
	if s := got["tomato"]; s == nil || s.ID != "gd-stw1" || s.Capped || s.RunsToday != 0 || s.MaxRunsPerDay != 24 {
		t.Errorf("after midnight with the trigger read failing, tomato's steward = %+v; want gd-stw1, 0 of 24, not capped", s)
	} else if s.Reason == nil || s.Reason.Text != wantReason.Text || s.Notes != "gd-nts1" {
		t.Errorf("after a failed read, tomato's steward's reason = %+v, notes %q; want the memory last read", s.Reason, s.Notes)
	}
}
