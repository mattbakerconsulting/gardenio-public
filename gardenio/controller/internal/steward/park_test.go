package steward

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/watcher"
)

func withSteward(d *fakeDaemon) *fakeDaemon {
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: "tp-steward", Type: "trigger", Title: "Steward (" + land + ")", Status: "open",
		Labels: []string{"trigger:multi", "role:gardener", MarkerLabel, LandLabel(land), "project:" + land}})
	return d
}

func TestHook_PurseEmptySaysTheStewardsLine(t *testing.T) {
	d := withSteward(newFarm())
	if err := (Hook{Daemon: d}).PurseEmpty(context.Background(), land, purse.Purse{GrantedCredits: 100}); err != nil {
		t.Fatalf("PurseEmpty: %v", err)
	}
	if len(d.created) != 1 {
		t.Fatalf("want one post, got %d", len(d.created))
	}
	req := d.created[0]
	var f map[string]string
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatal(err)
	}
	if req.Type != "message" || f["channel_id"] != general || f["text"] != ParkText || f["author"] != "agent:"+land+"-steward" {
		t.Fatalf("post %s %v", req.Type, f)
	}
	if req.IdempotencyKey != "steward-park:"+land+":100.00" || req.Labels[0] != "project:"+land {
		t.Fatalf("key %q labels %v", req.IdempotencyKey, req.Labels)
	}
	// The line is the steward's own voice: it wakes nothing, its own trigger
	// least of all.
	ev := message("mp", general, f["author"], ParkText, nil)
	if got := fired(t, "beads.bead.created", inLand(ev, land)); len(got) != 0 {
		t.Fatalf("the park line woke %v", got)
	}
}

func TestHook_PurseEmptyWithoutAStewardSaysNothing(t *testing.T) {
	d := newFarm()
	if err := (Hook{Daemon: d}).PurseEmpty(context.Background(), land, purse.Purse{}); err != nil || len(d.created) != 0 {
		t.Fatalf("a land with no steward: err %v, %d posts", err, len(d.created))
	}
	d = withSteward(newFarm())
	delete(d.beads[0].Fields, "land_default_channel")
	if err := (Hook{Daemon: d}).PurseEmpty(context.Background(), land, purse.Purse{}); err != nil || len(d.created) != 0 {
		t.Fatalf("a land with no #general: err %v, %d posts", err, len(d.created))
	}
}

func gardenerRun(id, task, state, status, park string, at time.Time) *beadsapi.BeadDetail {
	f := map[string]string{"agent": land + "-" + id, "task_id": task, "agent_state": state}
	if park != "" {
		f[watcher.FieldParkReason] = park
	}
	return &beadsapi.BeadDetail{ID: id, Type: "agent", Status: status, CreatedAt: at,
		Labels: []string{"project:" + land, "role:" + GardenerRole}, Fields: f}
}

func step(id, title, status string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "task", Title: title, Status: status, Labels: []string{"project:" + land}}
}

func TestParkedSteps_TheStewardChoosesWhatResumes(t *testing.T) {
	d := newFarm()
	t1 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	d.beads = append(d.beads,
		step("tp-s1", "Water the beds", "in_progress"),
		gardenerRun("a1", "tp-s1", "done", "closed", watcher.ParkReason, t1),
		step("tp-s2", "Prune the vines", "open"),
		gardenerRun("a2", "tp-s2", "done", "closed", watcher.ParkReason, t1),
		gardenerRun("a2b", "tp-s2", "working", "open", "", t2), // resumed
		step("tp-s3", "Pick the tomatoes", "closed"),
		gardenerRun("a3", "tp-s3", "done", "closed", watcher.ParkReason, t1), // merged since
		step("tp-s4", "Stake the beans", "open"),
		gardenerRun("a4", "tp-s4", "done", "closed", "", t1), // not parked
		step("tp-s5", "Mulch the paths", "open"),
		gardenerRun("a5", "tp-s5", "stopping", "open", watcher.ParkReason, t1), // suspended, open
		step("tp-s6", "Fix the fence", "open"),
		gardenerRun("a6", "tp-s6", "working", "open", watcher.ParkReason, t1), // thawed
		step("tp-s7", "Frozen work", "open"),
		gardenerRun("a7", "tp-s7", "done", "closed", watcher.ParkFrozen, t1), // the freeze is not the purse
	)
	got, err := parkedSteps(context.Background(), d, land)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "tp-s1,tp-s5" {
		t.Fatalf("parked steps = %v, want tp-s1 and tp-s5", ids)
	}
	if got[0].Title != "Water the beds" || got[0].Agent != land+"-a1" {
		t.Fatalf("step %+v", got[0])
	}

	prompt, err := TriggerPromptRenderer(d)(context.Background(),
		bridge.TriggerDef{ID: "tp-trig-1", Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel(land)}}, bridge.FireEvent{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Parked when the purse ran out (open steps whose gardener was suspended):\n- tp-s1: Water the beds (gardener tomato-patch-a1)\n- tp-s5: Mulch the paths",
		ParkText,
		"gb spawn --role lands-gardener --task <step-id>",
		`"restart_when":"a credit grant for tomato-patch"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParkedSteps_NoneRendersNothing(t *testing.T) {
	prompt, err := TriggerPromptRenderer(newFarm())(context.Background(),
		bridge.TriggerDef{ID: "tp-trig-1", Role: Role, Project: land, Labels: []string{MarkerLabel, LandLabel(land)}}, bridge.FireEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "Parked when the purse ran out (") {
		t.Fatalf("a land with nothing parked lists nothing:\n%s", prompt)
	}
}
