package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/runnerreg"
)

func TestFilterForRunner(t *testing.T) {
	me := runnerreg.Runner{RunnerID: "gd-r1", Owner: "oidc:alice@example.com", Projects: []string{"pet"}, Tags: []string{"browser"}}
	beads := []*beadsapi.BeadDetail{
		{ID: "placed-for-me", Labels: []string{"project:pet"}, Fields: map[string]string{"placement": "local", "runner_id": "gd-r1"}},
		{ID: "placed-by-tag", Labels: []string{"project:other"}, Fields: map[string]string{"placement": "local", "runner_tag": "browser"}},
		{ID: "placed-for-other", Labels: []string{"project:pet"}, Fields: map[string]string{"placement": "local", "runner_id": "gd-r2"}},
		{ID: "unplaced-mine", Labels: []string{"project:pet"}, Fields: map[string]string{"opened_by_operator": "oidc:alice@example.com"}},
		{ID: "unplaced-not-allowlisted", Labels: []string{"project:other"}, Fields: map[string]string{"opened_by_operator": "oidc:alice@example.com"}},
		{ID: "unplaced-someone-elses", Labels: []string{"project:pet"}, Fields: map[string]string{"opened_by_operator": "oidc:bob@example.com"}},
		{ID: "unplaced-no-opener", Labels: []string{"project:pet"}, Fields: map[string]string{}},
	}
	got := filterForRunner(beads, me, me.Owner)
	var ids []string
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	want := []string{"placed-for-me", "placed-by-tag", "unplaced-mine"}
	if len(ids) != len(want) {
		t.Fatalf("kept %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("kept %v, want %v", ids, want)
		}
	}
}

func TestDropLocallyPlaced(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "placed", Fields: map[string]string{"placement": "local", "runner_id": "gd-r1"}},
		{ID: "unplaced", Fields: map[string]string{}},
	}
	got := dropLocallyPlaced(beads)
	if len(got) != 1 || got[0].ID != "unplaced" {
		t.Fatalf("got %v, want only unplaced", got)
	}
	if len(beads) != 2 || beads[0].ID != "placed" {
		t.Fatal("dropLocallyPlaced must not reorder its input")
	}
}

func TestReadyRunner_HumanIsNeverNarrowed(t *testing.T) {
	old := loadReadyRunner
	t.Cleanup(func() { loadReadyRunner = old })
	loadReadyRunner = func() (runnerreg.Runner, error) { return runnerreg.Runner{RunnerID: "gd-r1"}, nil }
	if _, on := readyRunner(readyOptions{AgentContext: false}); on {
		t.Fatal("a human's gb ready on a registered laptop must not be narrowed")
	}
	if me, on := readyRunner(readyOptions{AgentContext: true}); !on || me.RunnerID != "gd-r1" {
		t.Fatalf("agent on a runner: %+v %v", me, on)
	}
	loadReadyRunner = func() (runnerreg.Runner, error) { return runnerreg.Runner{}, runnerreg.ErrNotRegistered }
	if _, on := readyRunner(readyOptions{AgentContext: true}); on {
		t.Fatal("no registration: not on a runner")
	}
}

func TestRefuseLocallyPlaced(t *testing.T) {
	err := refuseLocallyPlaced("gd-t1", &beadsapi.BeadDetail{Fields: map[string]string{"placement": "local", "runner_id": "gd-r1"}})
	if err == nil || err.Error() != "task gd-t1 is placed on runner gd-r1; it will be picked up by gb runner join there" {
		t.Fatalf("err = %v", err)
	}
	if err := refuseLocallyPlaced("gd-t2", &beadsapi.BeadDetail{Fields: map[string]string{}}); err != nil {
		t.Fatalf("unplaced task refused: %v", err)
	}
}

func TestDropAwaitingRelease(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "merged", Fields: map[string]string{beadsapi.AwaitsReleaseField: "true"}},
		{ID: "plain", Fields: map[string]string{}},
		{ID: "unset", Fields: map[string]string{beadsapi.AwaitsReleaseField: "false"}},
	}
	got := dropAwaitingRelease(beads)
	if len(got) != 2 || got[0].ID != "plain" || got[1].ID != "unset" {
		t.Fatalf("got %v, want plain and unset", got)
	}
	if len(beads) != 3 || beads[0].ID != "merged" {
		t.Fatal("dropAwaitingRelease must not reorder its input")
	}
}
