package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// ── Plan movement is resume progress (gd-WE8g4FFEHm) ──────────────────────

// planProgressGardener builds a plan gardener on gd-epic-1 (gd-step-1 open,
// gd-step-2 in_progress) that has just spent its run budget after one counted
// resume, with every run self-reporting progress_score 0 and the plan
// fingerprint stamped at that resume.
func planProgressGardener(t *testing.T, resumes, budget int) (*mockTriggerDaemon, *TriggerEngine, TriggerDef) {
	t.Helper()
	daemon, engine, _, trig := gardenerTrigger(t, GardenerSpec{
		RunBudget:              4,
		RunsSpawned:            4,
		ResumeBudget:           budget,
		ResumesWithoutProgress: resumes,
		LastResumeReason:       "max_runs",
		LastResumeRunsSpawned:  2,
		AssignBead:             "gd-epic-1",
		Stop:                   json.RawMessage(`{"loop_end":{"any":[{"kind":"max_runs"}]}}`),
		ProgressState:          json.RawMessage(`{"runs":[{"agent_id":"a-1","progress_score":0},{"agent_id":"a-2","progress_score":0}]}`),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "plan"},
		&beadsapi.BeadDetail{ID: "gd-step-1", Type: "task", Status: "open", Title: "step 1"},
		&beadsapi.BeadDetail{ID: "gd-step-2", Type: "task", Status: "in_progress", Title: "step 2"},
	)
	daemon.parentOf = map[string]string{"gd-step-1": "gd-epic-1", "gd-step-2": "gd-epic-1"}
	digest, err := gardenerPlanProgressDigest(context.Background(), daemon, "gd-epic-1")
	if err != nil || digest == "" {
		t.Fatalf("setup: plan digest: %q, %v", digest, err)
	}
	trig.Gardener.PlanProgressDigest = digest
	return daemon, engine, trig
}

func planStep(d *mockTriggerDaemon, id string) *beadsapi.BeadDetail {
	for _, b := range d.beads {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// TestGardener_PlanMoved_ResetsResumesWithoutProgress: when steps close, start
// or a step's PR merges while every run self-reports progress 0, the next
// counted resume resets ResumesWithoutProgress to 0 instead of spending it.
// An unmoved plan still counts the resume.
func TestGardener_PlanMoved_ResetsResumesWithoutProgress(t *testing.T) {
	cases := map[string]struct {
		change func(d *mockTriggerDaemon)
		want   int
	}{
		"plan unchanged": {change: func(*mockTriggerDaemon) {}, want: 2},
		"step closed":    {change: func(d *mockTriggerDaemon) { planStep(d, "gd-step-2").Status = "closed" }, want: 0},
		"step started":   {change: func(d *mockTriggerDaemon) { planStep(d, "gd-step-1").Status = "in_progress" }, want: 0},
		"open step's PR merged": {change: func(d *mockTriggerDaemon) {
			d.mrBeadsByTask = map[string]*beadsapi.BeadDetail{
				"gd-step-2": {ID: "gd-mr-1", Type: "merge-request", Fields: map[string]string{"mr_state": "merged"}},
			}
		}, want: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			daemon, engine, trig := planProgressGardener(t, 1, 3)
			before := trig.Gardener.PlanProgressDigest
			daemon.mu.Lock()
			tc.change(daemon)
			daemon.mu.Unlock()

			engine.Evaluate(context.Background())

			cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
			if cap.ResumesWithoutProgress != tc.want {
				t.Errorf("ResumesWithoutProgress: want %d, got %d", tc.want, cap.ResumesWithoutProgress)
			}
			if cap.RunBudget <= cap.RunsSpawned {
				t.Errorf("want a refilled run budget, got run_budget=%d runs_spawned=%d", cap.RunBudget, cap.RunsSpawned)
			}
			if moved := tc.want == 0; moved == (cap.PlanProgressDigest == before) {
				t.Errorf("want the stamped digest to follow the plan (moved=%v), before=%q after=%q", moved, before, cap.PlanProgressDigest)
			}
		})
	}
}

// TestGardener_StopGateReopen_RearmsSpentGardener: the 11-04 gd-U4BtfWKjvc
// case. A crew dies and the stop gate returns its step to ready the same
// minute the gardener spends its resume budget. The reopened step is new ready
// work, so the gardener re-arms (counter 0, fresh run budget) instead of
// parking or closing, and fires on the next cycle.
func TestGardener_StopGateReopen_RearmsSpentGardener(t *testing.T) {
	daemon, engine, trig := planProgressGardener(t, 3, 3)
	daemon.mu.Lock()
	planStep(daemon, "gd-step-2").Status = "open"
	daemon.mu.Unlock()

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	spawns := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if closed || trig.Gardener.DormantOnEscalation {
		t.Fatalf("reopened step: want re-armed, got closed=%v dormant=%v", closed, trig.Gardener.DormantOnEscalation)
	}
	if spawns != 0 {
		t.Errorf("the re-arm cycle skips the fire, got %d spawns", spawns)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.ResumesWithoutProgress != 0 || cap.RunBudget <= cap.RunsSpawned {
		t.Errorf("want fresh budgets, got resumes=%d run_budget=%d runs_spawned=%d", cap.ResumesWithoutProgress, cap.RunBudget, cap.RunsSpawned)
	}

	if skip, reason := gardenerPreFireCheck(context.Background(), daemon, slog.Default(), trig); skip {
		t.Errorf("re-armed gardener must fire next cycle, got skip %q", reason)
	}
}

// TestGardener_SpentBudget_PlanUnmoved_StillParks: with the plan unchanged
// since the last resume, a spent budget still parks the plan gardener dormant.
func TestGardener_SpentBudget_PlanUnmoved_StillParks(t *testing.T) {
	daemon, engine, trig := planProgressGardener(t, 3, 3)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()
	if closed || !trig.Gardener.DormantOnEscalation || trig.Gardener.DormantBlockerKind != gardenerDormantKindResumeBudgetSpent {
		t.Errorf("unmoved plan: want dormant %q, got closed=%v dormant=%v kind=%q",
			gardenerDormantKindResumeBudgetSpent, closed, trig.Gardener.DormantOnEscalation, trig.Gardener.DormantBlockerKind)
	}
}

// TestGardener_FirstResume_StampsBaselineAndCounts: a gardener with no stamped
// fingerprint counts its resume as usual and stamps the baseline.
func TestGardener_FirstResume_StampsBaselineAndCounts(t *testing.T) {
	daemon, engine, trig := planProgressGardener(t, 1, 3)
	trig.Gardener.PlanProgressDigest = ""
	daemon.mu.Lock()
	planStep(daemon, "gd-step-2").Status = "closed"
	daemon.mu.Unlock()

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.ResumesWithoutProgress != 2 || cap.PlanProgressDigest == "" {
		t.Errorf("first resume: want counted (2) and a stamped digest, got resumes=%d digest=%q", cap.ResumesWithoutProgress, cap.PlanProgressDigest)
	}
}
