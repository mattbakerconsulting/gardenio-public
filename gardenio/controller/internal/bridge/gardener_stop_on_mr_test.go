package bridge

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Integration coverage for the --stop-on-mr-merged flag (task ka-0000005598):
// a gardener whose stop.loop_end declares {"kind":"specific_mr_merged","mr_url":...}
// is driven through the REAL engine fire path (engine.Evaluate → gardenerPreFireCheck
// → scheduler.EvaluateGardenerStopSpec via gardenerDaemonStopAdapter, which forwards
// MRBeadByURL to the daemon). It proves the terminal close fires when the exact MR
// is merged and BLOCKS (gardener keeps running) while it is unmerged.

const stopOnMRURL = "https://gitlab.com/acme/app/-/merge_requests/42"

// specificMRStop is a bounded stop blob: a specific_mr_merged loop_end targeting
// stopOnMRURL plus the mandatory hard_stop wall_clock so the bounded-gardener
// guard (if it runs) is satisfied.
func specificMRStop(url string) json.RawMessage {
	return json.RawMessage(`{"loop_end":{"any":[{"kind":"specific_mr_merged","mr_url":"` + url + `"}]},` +
		`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`)
}

// TestGardener_StopOnMRMerged_MergedMR_ClosesAsSuccess: the target MR's mirror
// bead reports merged, so the terminal specific_mr_merged loop_end fires and the
// gardener is CLOSED (not re-armed). The close stamps the loop_end / specific_mr_merged
// slot+kind so the outcome reads as a clean goal-achieved success.
func TestGardener_StopOnMRMerged_MergedMR_ClosesAsSuccess(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 3, // >0 so the never-started guard is irrelevant here
		Stop:        specificMRStop(stopOnMRURL),
	})
	daemon.mrBeadsByURL = map[string]*beadsapi.BeadDetail{
		stopOnMRURL: {
			ID:     "gd-mr-1",
			Type:   "merge-request",
			Status: "closed",
			Fields: map[string]string{"mr_url": stopOnMRURL, "mr_outcome": "merged"},
		},
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawns := len(daemon.spawnedNames)
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	// Find the close field-map for gd-cap-1 to assert the terminal slot/kind.
	var closeFields map[string]string
	for i, id := range daemon.closedIDs {
		if id == "gd-cap-1" && i < len(daemon.closedFields) {
			closeFields = daemon.closedFields[i]
		}
	}
	daemon.mu.Unlock()

	if !closed {
		t.Fatalf("gardener must CLOSE when its target MR is merged; closedIDs=%v", daemon.closedIDs)
	}
	if spawns != 0 {
		t.Errorf("a gardener whose stop condition is met must not spawn, got %d", spawns)
	}
	if closeFields != nil {
		if closeFields["gardener_terminated_slot"] != "loop_end" {
			t.Errorf("gardener_terminated_slot = %q, want loop_end", closeFields["gardener_terminated_slot"])
		}
		if closeFields["gardener_terminated_kind"] != "specific_mr_merged" {
			t.Errorf("gardener_terminated_kind = %q, want specific_mr_merged", closeFields["gardener_terminated_kind"])
		}
	}
}

// TestGardener_StopOnMRMerged_UnmergedMR_KeepsRunning: while the target MR is
// still open, the specific_mr_merged loop_end does NOT fire — the gardener is not
// closed and proceeds to spawn its iteration normally (the stop BLOCKS the close
// but never blocks forward work).
func TestGardener_StopOnMRMerged_UnmergedMR_KeepsRunning(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 3,
		Stop:        specificMRStop(stopOnMRURL),
	})
	daemon.mrBeadsByURL = map[string]*beadsapi.BeadDetail{
		stopOnMRURL: {
			ID:     "gd-mr-1",
			Type:   "merge-request",
			Status: "open",
			Fields: map[string]string{"mr_url": stopOnMRURL, "mr_state": "opened", "mr_pipeline_status": "running"},
		},
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawns := len(daemon.spawnedNames)
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()

	if closed {
		t.Fatalf("gardener must NOT close while its target MR is unmerged; closedIDs=%v", daemon.closedIDs)
	}
	if spawns != 1 {
		t.Errorf("gardener with an unmet MR stop should spawn its iteration, got %d spawns", spawns)
	}
}

// TestGardener_StopOnMRMerged_MirrorAbsent_KeepsRunning: the target MR has no
// mirror bead yet (not synced). specific_mr_merged fails-open to not-met, so the
// gardener keeps running rather than closing on a transient absence.
func TestGardener_StopOnMRMerged_MirrorAbsent_KeepsRunning(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 3,
		Stop:        specificMRStop(stopOnMRURL),
	})
	// mrBeadsByURL intentionally nil — MRBeadByURL returns (nil, nil).

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()

	if closed {
		t.Fatalf("gardener must NOT close when the MR mirror bead is absent; closedIDs=%v", daemon.closedIDs)
	}
}
