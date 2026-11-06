package bridge

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestEvaluate_SweepsShareOneLeanRoster pins gd-zXdZq4Traf: with the
// liveness, check-liveness, pouring-stall and dark-lane sweeps all on, one
// Evaluate cycle reads the open-trigger roster ONCE, with the lean projection;
// a sweep run outside Evaluate still reads its own.
func TestEvaluate_SweepsShareOneLeanRoster(t *testing.T) {
	bead := gardenerBead("gd-g1", "janitor", rfc3339Ago(10*time.Hour), nil)
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{bead}}
	var rosterReads int
	daemon.listBeadsHook = func(_ int, q beadsapi.ListBeadsQuery) error {
		if slices.Equal(q.Types, []string{"trigger"}) && slices.Equal(q.Statuses, []string{"open"}) && q.Parent == "" && len(q.Labels) == 0 {
			rosterReads++
			if !slices.Contains(q.OmitFields, "prompt") || !slices.Contains(q.Omit, "description") {
				t.Errorf("sweep roster read without the lean projection: %+v", q)
			}
		}
		return nil
	}
	rec := &recordingNotifier{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:               daemon,
		Notifier:             rec.notify,
		Logger:               slog.Default(),
		LivenessEnabled:      true,
		CheckLivenessEnabled: true,
		PouringStallEnabled:  true,
		DarkLaneEnabled:      true,
	})

	engine.Evaluate(context.Background())
	if rosterReads != 1 {
		t.Fatalf("one Evaluate cycle: want 1 shared roster read, got %d", rosterReads)
	}
	if engine.sweepRoster != nil {
		t.Error("the roster memo must not outlive the Evaluate cycle")
	}

	rosterReads = 0
	engine.SweepGardenerLiveness(context.Background())
	engine.SweepTriggerCheckLiveness(context.Background())
	if rosterReads != 2 {
		t.Fatalf("sweeps outside Evaluate: want a fresh read each, got %d", rosterReads)
	}
}
