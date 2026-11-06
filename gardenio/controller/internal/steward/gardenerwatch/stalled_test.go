package gardenerwatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// A building plan of the home garden with no live gardener is shown to the
// steward every wake, with how its gardener stopped and the ask that follows
// (gd-hVTkwcDwxK): 18 of 26 such plans went unnoticed for days on 10-31.

func planGardener(id, plan, status string, fields map[string]string, closed time.Time) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID: id, Type: "trigger", Title: "gardener " + plan, Status: status,
		Labels: []string{"trigger:schedule", "role:gardener", plangardener.GardenerEpicLabel(plan), plangardener.MarkerLabel},
		Fields: fields,
	}
	if !closed.IsZero() {
		b.ClosedAt = closed.UTC().Format(time.RFC3339)
	}
	return b
}

// stalledFarm is the home garden with a gardener-less plan of each kind, one
// plan with a live gardener, a draft plan with none and gd-plan, with neither
// a gardener nor a step.
func stalledFarm() *farm {
	d := &farm{parent: map[string]string{}, waiting: map[string]bool{}}
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	draft := homePlan("gd-p9", "Still a draft")
	draft.Fields["plan_state"] = "draft"
	d.beads = append(d.beads, homePlan("gd-plan", "Ship the steward"),
		homePlan("gd-p1", "Budget stop"),
		planGardener("gd-g1a", "gd-p1", "closed", map[string]string{"gardener_terminated_kind": "max_runs"}, at.Add(-time.Hour)),
		planGardener("gd-g1b", "gd-p1", "closed", map[string]string{"gardener_terminated_kind": "no_progress_for"}, at),
		homePlan("gd-p2", "Refused spawns"),
		planGardener("gd-g2", "gd-p2", "closed", map[string]string{
			"gardener_terminated_kind": "consecutive_failures",
			"last_fire_error":          "spawn_agent: HTTP 403: spawn denied: role plan-gardener is privileged; a non-human caller must escalate\nretry 5 of 5",
		}, at),
		homePlan("gd-p3", "Nothing left"),
		planGardener("gd-g3", "gd-p3", "closed", map[string]string{"gardener_terminated_kind": "wall_clock"}, at),
		homePlan("gd-p4", "Waits on the owner"),
		planGardener("gd-g4", "gd-p4", "closed", map[string]string{"gardener_terminated_kind": "bead_query"}, at),
		homePlan("gd-p5", "Stopped by hand"),
		planGardener("gd-g5", "gd-p5", "closed", map[string]string{"gardener_terminated_kind": "manual"}, at),
		homePlan("gd-p6", "Paused"),
		planGardener("gd-g6", "gd-p6", "open", map[string]string{"enabled": "false"}, time.Time{}),
		homePlan("gd-p7", "Never bound"),
		homePlan("gd-p8", "Running"),
		planGardener("gd-g8", "gd-p8", "open", map[string]string{"enabled": "true"}, time.Time{}),
		draft,
	)
	homeStep(d, "gd-s1a", "gd-p1", "open", false)
	homeStep(d, "gd-s1b", "gd-p1", "open", true)
	homeStep(d, "gd-s2", "gd-p2", "in_progress", false)
	homeStep(d, "gd-s3", "gd-p3", "closed", false)
	homeStep(d, "gd-s4a", "gd-p4", "open", true)
	homeStep(d, "gd-s4b", "gd-p4", "blocked", false)
	homeStep(d, "gd-s5", "gd-p5", "open", false)
	homeStep(d, "gd-s6", "gd-p6", "open", false)
	homeStep(d, "gd-s7", "gd-p7", "open", false)
	homeStep(d, "gd-s8", "gd-p8", "open", false)
	homeStep(d, "gd-s9", "gd-p9", "open", false)
	// A step under a phase of the plan counts too.
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: "gd-p7-ph", Type: "epic", Title: "phase", Status: "open", Labels: []string{"project:" + home}})
	d.parent["gd-p7-ph"] = "gd-p7"
	homeStep(d, "gd-s7b", "gd-p7-ph", "open", false)
	return d
}

func TestStalled_Plans(t *testing.T) {
	d := stalledFarm()
	stalled, _, err := Stalled(context.Background(), d, d.refs())
	if err != nil {
		t.Fatalf("Stalled: %v", err)
	}
	got := map[string]StalledPlan{}
	for _, s := range stalled {
		got[s.ID] = s
	}
	for _, w := range []StalledPlan{
		{ID: "gd-p1", Gardener: "gd-g1b", Kind: "no_progress_for", OpenSteps: 2, ReadySteps: 1, Ask: AskRebind},
		{ID: "gd-p2", Gardener: "gd-g2", Kind: "consecutive_failures", OpenSteps: 1, ReadySteps: 1, Ask: AskRebind},
		{ID: "gd-p3", Gardener: "gd-g3", Kind: "wall_clock", Ask: AskComplete},
		{ID: "gd-p4", Gardener: "gd-g4", Kind: "bead_query", OpenSteps: 2, Ask: AskWaits},
		{ID: "gd-p5", Gardener: "gd-g5", Kind: "manual", OpenSteps: 1, ReadySteps: 1, Ask: AskTell},
		{ID: "gd-p6", Gardener: "gd-g6", Kind: KindPaused, OpenSteps: 1, ReadySteps: 1, Ask: AskRebind},
		{ID: "gd-p7", OpenSteps: 2, ReadySteps: 2, Ask: AskRebind},
		{ID: "gd-plan", Ask: AskComplete},
	} {
		g, ok := got[w.ID]
		if !ok {
			t.Errorf("%s is not listed as gardener-less", w.ID)
			continue
		}
		if g.Gardener != w.Gardener || g.Kind != w.Kind || g.OpenSteps != w.OpenSteps || g.ReadySteps != w.ReadySteps || g.Ask != w.Ask {
			t.Errorf("%s = %+v, want %+v", w.ID, g, w)
		}
		delete(got, w.ID)
	}
	for id := range got {
		t.Errorf("%s is listed, but it has a live gardener or is not building", id)
	}
	if e := stalled[1].Error; stalled[1].ID != "gd-p2" || strings.Contains(e, "retry") || !strings.Contains(e, "403") {
		t.Errorf("gd-p2's error line = %q, want the first line only", e)
	}
}

// Each plan's line in the steward's prompt, under "Plans with no live
// gardener".
func TestDescribeStalled_Golden(t *testing.T) {
	d := stalledFarm()
	stalled, _, err := Stalled(context.Background(), d, d.refs())
	if err != nil {
		t.Fatalf("Stalled: %v", err)
	}
	var b strings.Builder
	for _, s := range stalled {
		b.WriteString("- " + DescribeStalled(s) + "\n")
	}
	checkGolden(t, "stalled_plans.golden", b.String())
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s drifted (go test -update rewrites it):\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// The quiet_gardeners leaf classifies a gardener-less plan with conditions'
// mirror of scheduler's stop kinds (scheduler imports conditions), so the
// leaf wakes the steward for exactly the plans this list asks it to rebind.
func TestBudgetStopKindsMirrorTheScheduler(t *testing.T) {
	want := []string{scheduler.StopKindMaxRuns, scheduler.StopKindWallClock, scheduler.StopKindNoProgressFor, scheduler.StopKindBeadQuery}
	if strings.Join(BudgetStopKinds, ",") != strings.Join(want, ",") {
		t.Fatalf("BudgetStopKinds = %v, want %v", BudgetStopKinds, want)
	}
	if !conditions.GardenerlessRebinds("gd-g", scheduler.StopKindConsecutiveFailures, "403") ||
		conditions.GardenerlessRebinds("gd-g", scheduler.StopKindConsecutiveFailures, "boom") {
		t.Fatal("conditions' consecutive_failures mirror changed")
	}
}
