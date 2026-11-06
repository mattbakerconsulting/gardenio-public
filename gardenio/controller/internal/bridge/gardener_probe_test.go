package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// gardenerProbeBead builds a mock daemon holding one gardener trigger bead whose
// `gardener` field carries the given spec, plus a TriggerEngine over it. It
// mirrors the shape the schedule source's LoadTriggers + ParseTriggerDef parse
// in production, so ProbeGardenerResumeOrEscalate exercises the same decode path
// the engine uses — the point of the probe seam is that it is NOT a hand-built
// TriggerDef but a real GetBead → ParseTriggerDef round-trip.
func gardenerProbeBead(t *testing.T, id string, spec GardenerSpec) (*mockTriggerDaemon, *TriggerEngine) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal gardener spec: %v", err)
	}
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{{
			ID:     id,
			Type:   "trigger",
			Status: "open",
			Title:  "gardener probe trig",
			Labels: []string{"trigger:schedule", "role:gardener"},
			Fields: map[string]string{"gardener": string(raw), "enabled": "true"},
		}},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	return daemon, engine
}

// TestProbeGardenerResumeOrEscalate_Rearm proves the probe seam re-arms a gardener
// with resume budget remaining: it re-writes the gardener blob (bumping
// resumes_without_progress + refilling run_budget) and comments, but does NOT
// close or escalate. This is the RE-ARM leg the e2e's premature-stop scenario
// asserts against a live daemon.
func TestProbeGardenerResumeOrEscalate_Rearm(t *testing.T) {
	const id = "gd-cap-probe-rearm"
	daemon, engine := gardenerProbeBead(t, id, GardenerSpec{
		TerminalGoal:           "drain the epic",
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           5,
		ResumesWithoutProgress: 0, // budget remaining
		Stop:                   json.RawMessage(`{"loop_end":{"any":[{"kind":"max_runs"}]}}`),
	})

	if err := engine.ProbeGardenerResumeOrEscalate(context.Background(), id, scheduler.StopKindMaxRuns); err != nil {
		t.Fatalf("ProbeGardenerResumeOrEscalate: %v", err)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if slices.Contains(daemon.closedIDs, id) {
		t.Errorf("re-arm must NOT close the gardener, but closedIDs=%v", daemon.closedIDs)
	}
	if !slices.Contains(daemon.updatedIDs, id) {
		t.Errorf("re-arm must UpdateBeadFields the gardener blob, updatedIDs=%v", daemon.updatedIDs)
	}
	// The re-armed blob carries resumes_without_progress=1 and a refilled budget.
	var found bool
	for i, uid := range daemon.updatedIDs {
		if uid != id {
			continue
		}
		blob := daemon.updatedFields[i]["gardener"]
		if blob == "" {
			continue
		}
		var got GardenerSpec
		if err := json.Unmarshal([]byte(blob), &got); err != nil {
			t.Fatalf("unmarshal re-armed blob: %v", err)
		}
		if got.ResumesWithoutProgress != 1 {
			t.Errorf("resumes_without_progress = %d, want 1", got.ResumesWithoutProgress)
		}
		if got.RunBudget <= 1 {
			t.Errorf("run_budget = %d, want > 1 (refilled)", got.RunBudget)
		}
		found = true
	}
	if !found {
		t.Errorf("no gardener-blob UpdateBeadFields observed for %s", id)
	}
}

// TestProbeGardenerResumeOrEscalate_EscalateAtBudget proves the probe seam
// escalates a gardener whose resume budget is already spent: it stamps the
// gardener-stuck-escalate label (the resumer watchdog's enumeration handle) AND
// closes the bead. This is the ESCALATE-AT-BUDGET leg the e2e asserts.
func TestProbeGardenerResumeOrEscalate_EscalateAtBudget(t *testing.T) {
	const id = "gd-cap-probe-escalate"
	daemon, engine := gardenerProbeBead(t, id, GardenerSpec{
		TerminalGoal:           "drain the epic",
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           2,
		ResumesWithoutProgress: 2, // budget spent
		Stop:                   json.RawMessage(`{"loop_end":{"any":[{"kind":"max_runs"}]}}`),
	})

	if err := engine.ProbeGardenerResumeOrEscalate(context.Background(), id, scheduler.StopKindMaxRuns); err != nil {
		t.Fatalf("ProbeGardenerResumeOrEscalate: %v", err)
	}

	// labelsAddedFor takes its own lock, so read BEFORE we lock below.
	escLabels := daemon.labelsAddedFor(id)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if !slices.Contains(escLabels, gardenerStuckEscalateLabel) {
		t.Errorf("escalate must stamp %q label, addedLabels=%v", gardenerStuckEscalateLabel, daemon.addedLabels)
	}
	if !slices.Contains(daemon.closedIDs, id) {
		t.Errorf("escalate must close the gardener, closedIDs=%v", daemon.closedIDs)
	}
}

// TestProbeGardenerResumeOrEscalate_NotAGardener proves the seam refuses a bead
// with no gardener block rather than driving the gate against a non-gardener — so
// a mistargeted probe fails loudly instead of silently no-op'ing.
func TestProbeGardenerResumeOrEscalate_NotAGardener(t *testing.T) {
	const id = "gd-not-a-gardener"
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{{ID: id, Type: "trigger", Status: "open", Title: "plain trig"}},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	err := engine.ProbeGardenerResumeOrEscalate(context.Background(), id, scheduler.StopKindMaxRuns)
	if err == nil {
		t.Fatal("expected error for a non-gardener bead, got nil")
	}
}
