package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/progress"
)

// iterWriteDaemon is an iterCacheDaemon (which honours sort and limit like the
// beads daemon) that also records gardener blob writes.
type iterWriteDaemon struct {
	*iterCacheDaemon
	writes []string
}

func (d *iterWriteDaemon) UpdateBeadFields(_ context.Context, _ string, fields map[string]string) error {
	d.writes = append(d.writes, fields["gardener"])
	return nil
}

func (d *iterWriteDaemon) lastWrite(t *testing.T) GardenerSpec {
	t.Helper()
	if len(d.writes) == 0 {
		t.Fatal("no gardener write")
	}
	var g GardenerSpec
	if err := json.Unmarshal([]byte(d.writes[len(d.writes)-1]), &g); err != nil {
		t.Fatalf("decode gardener write: %v", err)
	}
	return g
}

// pastLimitIterations is total closed iterations of gardener gd-g, one a
// minute; gd-run000 is the oldest.
func pastLimitIterations(total int) []*beadsapi.BeadDetail {
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	beads := make([]*beadsapi.BeadDetail, 0, total)
	for i := range total {
		at := base.Add(time.Duration(i) * time.Minute)
		b := iterBead(fmt.Sprintf("gd-run%03d", i), "closed", at, at.Add(30*time.Second))
		b.Fields["agent_state"] = "done"
		b.Fields["wrapup"] = `{"accomplishments":"x","progress_score":1}`
		beads = append(beads, b)
	}
	return beads
}

// A gardener with more closed iterations than gardenerIterationFinalizeLimit
// must reconcile its newest runs (gd-Ahi1H2jR9a). The lists used to read the
// oldest 500, so runs 501-600 never reached run_log or ProgressState.Runs.
func TestGardener_PastIterationLimit_ReconcilesNewestRuns(t *testing.T) {
	const total = gardenerIterationFinalizeLimit + 100
	ctx, logger := context.Background(), slog.Default()
	newest := fmt.Sprintf("gd-run%03d", total-1)

	t.Run("run_log", func(t *testing.T) {
		d := &iterWriteDaemon{iterCacheDaemon: &iterCacheDaemon{beads: pastLimitIterations(total)}}
		trig := TriggerDef{ID: "gd-g", Gardener: &GardenerSpec{}}
		gardenerReconcileRunLogFromIterations(ctx, d, logger, trig)

		rl, err := scheduler.ParseRunLog(string(d.lastWrite(t).RunLog))
		if err != nil {
			t.Fatal(err)
		}
		if len(rl) != scheduler.MaxRunLogEntries {
			t.Fatalf("run_log: %d entries, want %d", len(rl), scheduler.MaxRunLogEntries)
		}
		if first, want := rl[0].Agent, fmt.Sprintf("gd-run%03d", total-scheduler.MaxRunLogEntries); first != want {
			t.Errorf("oldest run_log entry %s, want %s", first, want)
		}
		if last := rl[len(rl)-1].Agent; last != newest {
			t.Errorf("newest run_log entry %s, want %s", last, newest)
		}
	})

	t.Run("progress_state", func(t *testing.T) {
		d := &iterWriteDaemon{iterCacheDaemon: &iterCacheDaemon{beads: pastLimitIterations(total)}}
		trig := TriggerDef{ID: "gd-g", Gardener: &GardenerSpec{Progress: json.RawMessage(progressSpecWithSurface)}}
		gardenerFinalizeRunsFromIterations(ctx, d, logger, trig)

		st, err := progress.ParseState(string(d.lastWrite(t).ProgressState))
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Runs) != progress.MaxRuns {
			t.Fatalf("Runs: %d, want %d", len(st.Runs), progress.MaxRuns)
		}
		for i, r := range st.Runs {
			if want := fmt.Sprintf("gd-run%03d", total-progress.MaxRuns+i); r.AgentID != want {
				t.Fatalf("Runs[%d] = %s, want %s (the newest %d, oldest first)", i, r.AgentID, want, progress.MaxRuns)
			}
		}

		// The runs that fell off MaxRuns are evicted, not unrecorded: a second
		// pass over the same iterations writes nothing.
		writes := len(d.writes)
		gardenerFinalizeRunsFromIterations(ctx, d, logger, trig)
		if len(d.writes) != writes {
			t.Fatalf("second pass rewrote the gardener blob")
		}

		// A new run is credited and evicts the oldest retained one.
		at := d.beads[len(d.beads)-1].CreatedAt.Add(time.Minute)
		b := iterBead("gd-run-next", "closed", at, at.Add(30*time.Second))
		b.Fields["agent_state"] = "done"
		d.beads = append(d.beads, b)
		gardenerFinalizeRunsFromIterations(ctx, d, logger, trig)
		st, _ = progress.ParseState(string(d.lastWrite(t).ProgressState))
		if len(st.Runs) != progress.MaxRuns || st.Runs[len(st.Runs)-1].AgentID != "gd-run-next" ||
			st.Runs[0].AgentID != fmt.Sprintf("gd-run%03d", total-progress.MaxRuns+1) {
			t.Fatalf("after a new run: %d runs from %s to %s", len(st.Runs), st.Runs[0].AgentID, st.Runs[len(st.Runs)-1].AgentID)
		}
	})
}

// A Runs list left out of order (an earlier cycle re-credited evicted runs at
// the newest end) is put back in order, and an older iteration that still
// fits the window lands in place rather than at the end.
func TestGardener_FinalizeFromIterations_OrdersRunsByStart(t *testing.T) {
	ctx, logger := context.Background(), slog.Default()
	iters := pastLimitIterations(3)
	seed := progress.State{}
	seed.AppendRun(progress.RunRecord{AgentID: "gd-run002", StartedAt: iters[2].CreatedAt})
	seed.AppendRun(progress.RunRecord{AgentID: "gd-run000", StartedAt: iters[0].CreatedAt})
	seedJSON, _ := seed.Marshal()

	d := &iterWriteDaemon{iterCacheDaemon: &iterCacheDaemon{beads: iters}}
	trig := TriggerDef{ID: "gd-g", Gardener: &GardenerSpec{
		Progress:      json.RawMessage(progressSpecWithSurface),
		ProgressState: json.RawMessage(seedJSON),
	}}
	gardenerFinalizeRunsFromIterations(ctx, d, logger, trig)

	st, _ := progress.ParseState(string(d.lastWrite(t).ProgressState))
	var got []string
	for _, r := range st.Runs {
		got = append(got, r.AgentID)
	}
	if fmt.Sprint(got) != "[gd-run000 gd-run001 gd-run002]" {
		t.Fatalf("Runs = %v, want [gd-run000 gd-run001 gd-run002]", got)
	}
}
