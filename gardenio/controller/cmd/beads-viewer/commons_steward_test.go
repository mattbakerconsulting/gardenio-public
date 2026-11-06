package main

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/commons/commonstest"
)

// TestCommonsStewards: the steward node's read (gd-FsDEc7ZXGl) finds the home garden for land "", reads its steward's
// trigger and land, the day's runs from the trigger's gardener blob, its live run (working), its plans and its notes;
// a land with no steward is nil.
func TestCommonsStewards(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	d := &commonstest.Daemon{}
	home := []string{"project:gardenio"}
	d.Put(&beadsapi.BeadDetail{ID: "gd-proj", Type: "project", Status: "open", Title: "Project: gardenio", OperatedBy: "oidc:op@example.com",
		Fields: map[string]string{"land_default_channel": "gd-chgen", "git_url": "https://github.com/acme/gardenio.git"}})
	d.Put(&beadsapi.BeadDetail{ID: "gd-stew", Type: "trigger", Status: "open", Title: "gardenio steward",
		Labels: append([]string{"steward", "gardener-land:gardenio"}, home...),
		Fields: map[string]string{"gardener": `{"max_runs_per_day":24,"runs_day":"2026-11-02","runs_today":3}`}})
	d.Put(&beadsapi.BeadDetail{ID: "gd-run", Type: "agent", Status: "open", Labels: home, Fields: map[string]string{"role": "steward", "agent_state": "working"}})
	d.Put(&beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Status: "open", Title: "Commons v2", Labels: append([]string{"plan"}, home...),
		Fields: map[string]string{"plan_state": "building"}})
	d.Put(&beadsapi.BeadDetail{ID: "gd-notes", Type: "doc", Status: "open", Title: "steward-notes:gardenio", Labels: home,
		Fields: map[string]string{"content": "## Holds\n- none\n"}})
	s := commonsStewards{src: d, now: func() time.Time { return now }}
	got, err := s.Steward(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "gd-stew" || got.Land != "gardenio" || !got.Home || got.General != "gd-chgen" {
		t.Fatalf("home steward = %+v", got)
	}
	if got.State != commons.StewardWorking || got.Run != "gd-run" || got.RunsToday != 3 || got.MaxRunsPerDay != 24 {
		t.Errorf("head = state %q run %q runs %d/%d", got.State, got.Run, got.RunsToday, got.MaxRunsPerDay)
	}
	if len(got.Plans) != 1 || got.Plans[0].ID != "gd-plan" || got.Plans[0].State != "building" || got.NotesID != "gd-notes" {
		t.Errorf("plans %+v, notes %q", got.Plans, got.NotesID)
	}
	if none, err := s.Steward(context.Background(), "elsewhere"); err != nil || none != nil {
		t.Errorf("a land with no steward = %+v, %v", none, err)
	}
}
