package bridge

import (
	"encoding/json"
	"testing"

	"gardenio/controller/internal/scheduler"
)

// TestGardenerBlobRoundTrips is the bounded-stop regression (ka-0000006744): a
// gardener's `gardener` JSON blob carrying run_budget AND a hard_stop wall_clock
// must serialize and decode through ParseTriggerDef's GardenerSpec path without
// loss, so the firing-time guard sees the same run_budget + stop spec the
// author wrote. (The kbeads type:trigger config schema already declares
// run_budget(integer) + stop(json) + progress(json) + terminal_goal + max_runs
// — verified in kbeads/internal/server/config.go; the kbeads-side admit-without-400
// round-trip is covered by TestTypeTriggerAdmitsGardenerBoundsFields in that
// package. This test pins the controller-side decode that the guard depends on.)
func TestGardenerBlobRoundTrips(t *testing.T) {
	stop := scheduler.StopSpec{
		LoopEnd: scheduler.StopSlot{Any: []scheduler.StopEntry{
			{Kind: scheduler.StopKindMaxRuns},
		}},
		HardStop: scheduler.StopSlot{Any: []scheduler.StopEntry{
			{Kind: scheduler.StopKindWallClock, Duration: "168h"},
			{Kind: scheduler.StopKindConsecutiveFailures, Value: "5"},
		}},
		OnLoopEnd: "close",
	}
	stopRaw, err := json.Marshal(stop)
	if err != nil {
		t.Fatalf("marshal stop: %v", err)
	}

	orig := GardenerSpec{
		AssignBead:   "gd-epic",
		TerminalGoal: "All children closed.",
		RunBudget:    20,
		MaxRuns:      20,
		Stop:         json.RawMessage(stopRaw),
	}
	blob, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal gardener blob: %v", err)
	}

	// Decode exactly as ParseTriggerDef does.
	var got GardenerSpec
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("decode gardener blob (this would be a schema/round-trip break): %v", err)
	}

	if got.RunBudget != 20 {
		t.Errorf("run_budget did not round-trip: got %d, want 20", got.RunBudget)
	}
	// The decoded stop blob must still parse to a spec carrying the hard_stop
	// wall_clock — i.e. the guard would accept it.
	parsed, err := scheduler.ParseStopSpec(string(got.Stop))
	if err != nil {
		t.Fatalf("parse decoded stop: %v", err)
	}
	ok, reason := hasBoundedHardStopWallClock(parsed)
	if !ok {
		t.Errorf("hard_stop wall_clock lost in round-trip: %s", reason)
	}

	// End-to-end: the round-tripped spec passes the bounded-stop guard.
	if v := ValidateGardenerSpec(&got); v != nil {
		t.Errorf("round-tripped bounded gardener wrongly rejected: %s", v.Error())
	}
}
