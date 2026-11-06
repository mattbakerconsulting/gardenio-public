package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// newFakeDaemon returns an empty fakeDaemon for the gardener helper tests.
// Iterations in these tests carry no OperatedByID, so slug resolution is a
// no-op and no slug fixtures are needed.
func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{}
}

// gardenerTestServer builds a Server backed by the fakeDaemon with a fixed clock.
func gardenerTestServer(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	s := NewServer(daemon, slog.Default(), "", "", "", nil)
	s.SetNow(func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) })
	return s
}

// mkGardenerBead builds a gardener trigger bead with the given GardenerSpec JSON
// encoded into the `gardener` field.
func mkGardenerBead(id, title string, spec map[string]any) *beadsapi.BeadDetail {
	raw, _ := json.Marshal(spec)
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields: map[string]string{
			"role":     "gardener",
			"gardener": string(raw),
		},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
}

// mkIteration builds an agent bead stamped as an iteration of gardenerID.
func mkIteration(id, gardenerID, priorID string, runsSpawned int, status string, created time.Time) *beadsapi.BeadDetail {
	f := map[string]string{
		"agent":                  id,
		"role":                   "crew",
		"project":                "gardenio",
		"schedule_id":            gardenerID,
		"schedule_runs_spawned":  itoa(runsSpawned),
		"schedule_max_runs":      "10",
		"schedule_terminal_goal": "drive the epic",
	}
	if priorID != "" {
		f["schedule_prior_agent_id"] = priorID
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Title:     id,
		Type:      "agent",
		Status:    status,
		Fields:    f,
		CreatedAt: created,
		UpdatedAt: created,
	}
}

// mkTriggerIteration builds an agent bead stamped as a bridge-trigger iteration
// of gardenerID. Unlike mkIteration (legacy scheduler path), it carries the
// gardener's id under `trigger_id` and deliberately omits `schedule_id` — this
// is the modern spawn shape (triggers.go actionSpawnAgent) that the gardener
// page must also count (ka-0000007051). No schedule_prior_agent_id is stamped,
// mirroring prod where trigger-path spawns lack the chain back-pointer.
func mkTriggerIteration(id, gardenerID string, runsSpawned int, status string, created time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  id,
		Type:   "agent",
		Status: status,
		Fields: map[string]string{
			"agent":                  id,
			"role":                   "crew",
			"project":                "gardenio",
			"trigger_id":             gardenerID,
			"trigger_title":          "Soup-to-nuts gardener",
			"schedule_key":           created.Format("2006-01-02T15:04"),
			"schedule_runs_spawned":  itoa(runsSpawned),
			"schedule_max_runs":      "10",
			"schedule_terminal_goal": "drive the epic",
		},
		CreatedAt: created,
		UpdatedAt: created,
	}
}

func TestGardenerViewFromBead_DecodesSpec(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	b := mkGardenerBead("gd-cap1", "Delivery gardener", map[string]any{
		"name":                 "delivery",
		"terminal_goal":        "drain the ready queue",
		"exit_condition":       "queue empty",
		"on_exit":              "notify",
		"run_budget":           100,
		"max_runs":             10,
		"retry_on_failure":     true,
		"max_retries":          3,
		"one_spawn_at_a_time":  true,
		"runs_spawned":         7,
		"runs_completed":       6,
		"consecutive_failures": 1,
		"last_agent_id":        "gd-iter7",
		"slack_user_id":        "U123",
		"user_bead_id":         "gd-user1",
		"stop":                 map[string]any{"max_runs": 10},
		"progress_state":       map[string]any{"done": 6},
	})

	v, err := s.gardenerViewFromBead(context.Background(), b)
	if err != nil {
		t.Fatalf("gardenerViewFromBead: %v", err)
	}
	if v.ID != "gd-cap1" || v.Title != "Delivery gardener" {
		t.Errorf("identity = %q/%q", v.ID, v.Title)
	}
	if v.Name != "delivery" || v.TerminalGoal != "drain the ready queue" {
		t.Errorf("goal fields = %q/%q", v.Name, v.TerminalGoal)
	}
	if v.RunBudget != 100 || v.MaxRuns != 10 || !v.RetryOnFailure || v.MaxRetries != 3 {
		t.Errorf("budget/retry = %+v", v)
	}
	if !v.OneSpawnAtATime {
		t.Errorf("OneSpawnAtATime not decoded")
	}
	if v.RunsSpawned != 7 || v.RunsCompleted != 6 || v.ConsecutiveFailures != 1 || v.LastAgentID != "gd-iter7" {
		t.Errorf("counters = %+v", v)
	}
	if v.SlackUserID != "U123" || v.UserBeadID != "gd-user1" {
		t.Errorf("ownership raw = %q/%q", v.SlackUserID, v.UserBeadID)
	}
	if !v.Enabled || v.IsClosed {
		t.Errorf("enabled/closed = %v/%v", v.Enabled, v.IsClosed)
	}
	// Stop/Progress render pretty-printed JSON (non-empty, indented).
	if v.StopJSON == "" || v.ProgressJSON == "" {
		t.Errorf("stop/progress JSON empty: %q / %q", v.StopJSON, v.ProgressJSON)
	}
}

// TestGardenerViewFromBead_ResumeSurfacing checks that the resume bookkeeping
// (bead ka-0000005353) renders onto the gardener view: the fruitless-resume
// count, the effective resume budget (0 → default 5), the last re-arm reason,
// and the stuck-escalated flag from the bead label.
func TestGardenerViewFromBead_ResumeSurfacing(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	b := mkGardenerBead("gd-cap-resume", "Resuming gardener", map[string]any{
		"terminal_goal":            "ship the thing",
		"resumes_without_progress": 2,
		"last_resume_reason":       "max_runs",
		// resume_budget omitted → effective default 5
	})
	b.Labels = append(b.Labels, "gardener-stuck-escalate")

	v, err := s.gardenerViewFromBead(context.Background(), b)
	if err != nil {
		t.Fatalf("gardenerViewFromBead: %v", err)
	}
	if v.ResumesWithoutProgress != 2 {
		t.Errorf("ResumesWithoutProgress = %d, want 2", v.ResumesWithoutProgress)
	}
	if v.ResumeBudget != 5 {
		t.Errorf("ResumeBudget = %d, want 5 (default)", v.ResumeBudget)
	}
	if v.LastResumeReason != "max_runs" {
		t.Errorf("LastResumeReason = %q, want max_runs", v.LastResumeReason)
	}
	if !v.StuckEscalated {
		t.Errorf("StuckEscalated = false, want true (label present)")
	}
}

func TestGardenerViewFromBead_NoSpec(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	// A trigger bead with no `gardener` field: header still renders identity +
	// status, no error.
	b := &beadsapi.BeadDetail{
		ID:     "gd-bare",
		Title:  "Bare trigger",
		Type:   "trigger",
		Status: "closed",
		Fields: map[string]string{"enabled": "false"},
	}
	v, err := s.gardenerViewFromBead(context.Background(), b)
	if err != nil {
		t.Fatalf("gardenerViewFromBead: %v", err)
	}
	if v.ID != "gd-bare" {
		t.Errorf("ID = %q", v.ID)
	}
	if v.Enabled {
		t.Errorf("expected disabled (enabled:false)")
	}
	if !v.IsClosed {
		t.Errorf("expected closed")
	}
	if v.TerminalGoal != "" || v.StopJSON != "" {
		t.Errorf("expected empty spec fields, got goal=%q stop=%q", v.TerminalGoal, v.StopJSON)
	}
}

func TestReconcileCounters_EngineSecondary(t *testing.T) {
	// Iterations (IterationCount) is the PRIMARY counter. The engine lifetime
	// tally is demoted to an always-shown SECONDARY line that is framed as
	// "older runs pruned" only once RunsSpawned exceeds the live count by more
	// than gardenerPrunedGap. A lead of exactly 1 is benign mid-loop and is NOT
	// framed as pruned. A gardener with no activity gets no secondary line.
	cases := []struct {
		name         string
		runsSpawned  int
		runsComplete int
		iterCount    int
		wantEngine   string
		wantPruned   bool
	}{
		{"exact match", 5, 5, 5, "5 spawned / 5 completed", false},
		{"spec one ahead (benign)", 6, 5, 5, "6 spawned / 5 completed", false},
		{"live one ahead (benign)", 5, 5, 6, "5 spawned / 5 completed", false},
		{"older runs pruned", 439, 413, 13, "439 spawned / 413 completed (older runs pruned)", true},
		{"small prune gap", 7, 6, 3, "7 spawned / 6 completed (older runs pruned)", true},
		{"both zero (no activity)", 0, 0, 0, "", false},
		{"completed-only legacy", 0, 4, 0, "0 spawned / 4 completed", false},
		// Legacy spawn_agent / bare-trigger gardeners carry no engine spec, so
		// both counters are zero even with a real thread (bug ka-0000005661). The
		// secondary line is DERIVED from IterationCount instead of printing the
		// contradictory "0 spawned / 0 completed".
		{"derived from thread", 0, 0, 12, "12 spawned / 12 completed", false},
		{"derived single run", 0, 0, 1, "1 spawned / 1 completed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := gardenerView{RunsSpawned: tc.runsSpawned, RunsCompleted: tc.runsComplete, IterationCount: tc.iterCount}
			v.reconcileCounters()
			if v.EngineCount != tc.wantEngine {
				t.Errorf("EngineCount = %q, want %q", v.EngineCount, tc.wantEngine)
			}
			if v.EnginePruned != tc.wantPruned {
				t.Errorf("EnginePruned = %v, want %v", v.EnginePruned, tc.wantPruned)
			}
		})
	}
}

// TestReconcileCounters_DerivedFromLifeThread asserts the legacy-gardener
// derivation contract (bug ka-0000005661): when the GardenerSpec engine tally is
// null (both counters zero) but the life-thread is non-empty, the secondary line
// is DERIVED from the rendered thread — spawned = IterationCount, completed =
// IterationCount - LiveCount — and flagged EngineDerived so the template can
// source-label it "from life-thread". A real engine tally is never re-sourced.
func TestReconcileCounters_DerivedFromLifeThread(t *testing.T) {
	t.Run("legacy: live runs excluded from completed", func(t *testing.T) {
		v := gardenerView{RunsSpawned: 0, RunsCompleted: 0, IterationCount: 94, LiveCount: 1}
		v.reconcileCounters()
		if !v.EngineDerived {
			t.Fatalf("EngineDerived = false, want true for a spec-less gardener with a thread")
		}
		if v.EngineCount != "94 spawned / 93 completed" {
			t.Errorf("EngineCount = %q, want %q", v.EngineCount, "94 spawned / 93 completed")
		}
		if v.EnginePruned {
			t.Errorf("EnginePruned = true, want false (derived line is never pruned-framed)")
		}
	})

	t.Run("real engine tally is not re-sourced as derived", func(t *testing.T) {
		v := gardenerView{RunsSpawned: 439, RunsCompleted: 413, IterationCount: 13}
		v.reconcileCounters()
		if v.EngineDerived {
			t.Fatalf("EngineDerived = true, want false when the spec carries a real tally")
		}
		if v.EngineCount != "439 spawned / 413 completed (older runs pruned)" {
			t.Errorf("EngineCount = %q, want pruned lifetime tally", v.EngineCount)
		}
	})

	t.Run("idle gardener stays blank and underived", func(t *testing.T) {
		v := gardenerView{RunsSpawned: 0, RunsCompleted: 0, IterationCount: 0}
		v.reconcileCounters()
		if v.EngineDerived || v.EngineCount != "" {
			t.Errorf("idle gardener: EngineDerived=%v EngineCount=%q, want false/empty", v.EngineDerived, v.EngineCount)
		}
	})
}

func TestGardenerIterations_ChainOrder(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// Build a clean chain: r1 (root) <- r2 <- r3 (head, in-flight). Seed the
	// daemon in a deliberately shuffled order to prove ordering comes from the
	// chain, not list order or created_at coincidence.
	r1 := mkIteration("gd-r1", "gd-cap", "", 1, "closed", t0)
	r2 := mkIteration("gd-r2", "gd-cap", "gd-r1", 2, "closed", t0.Add(time.Hour))
	r3 := mkIteration("gd-r3", "gd-cap", "gd-r2", 3, "in_progress", t0.Add(2*time.Hour))
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{r2, r3, r1}, Total: 3}

	iters, _, err := s.gardenerIterations(context.Background(), "gd-cap", s.now())
	if err != nil {
		t.Fatalf("gardenerIterations: %v", err)
	}
	if len(iters) != 3 {
		t.Fatalf("got %d iterations, want 3", len(iters))
	}
	// Thread is newest-first: r3 (run 3) then r2 (run 2) then r1 (run 1).
	if iters[0].ID != "gd-r3" || iters[0].RunNumber != 3 {
		t.Errorf("head = %q run %d, want gd-r3 run 3", iters[0].ID, iters[0].RunNumber)
	}
	if iters[2].ID != "gd-r1" || iters[2].RunNumber != 1 {
		t.Errorf("root = %q run %d, want gd-r1 run 1", iters[2].ID, iters[2].RunNumber)
	}
	// In-flight head: IsLive + "(in progress)" label, never ByTime.
	if !iters[0].IsLive || iters[0].ByTime {
		t.Errorf("head live/byTime = %v/%v", iters[0].IsLive, iters[0].ByTime)
	}
	if iters[0].RunLabel != "Iteration 3 (in progress)" {
		t.Errorf("head label = %q", iters[0].RunLabel)
	}
	// Closed run with known MaxRuns reads "Iteration k of M".
	if iters[2].RunLabel != "Iteration 1 of 10" {
		t.Errorf("root label = %q", iters[2].RunLabel)
	}
	// Embedded agentRowView is populated.
	if iters[1].agentRowView.ID != "gd-r2" {
		t.Errorf("embedded agentRowView.ID = %q", iters[1].agentRowView.ID)
	}
}

func TestGardenerIterations_BrokenChainFallsBackToTime(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// r2 references a prior id that is NOT in the set (dangling) → chain broken
	// → fall back to created_at ascending.
	r1 := mkIteration("gd-r1", "gd-cap", "", 1, "closed", t0.Add(2*time.Hour))
	r2 := mkIteration("gd-r2", "gd-cap", "gd-GONE", 2, "closed", t0)
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{r1, r2}, Total: 2}

	iters, _, err := s.gardenerIterations(context.Background(), "gd-cap", s.now())
	if err != nil {
		t.Fatalf("gardenerIterations: %v", err)
	}
	if len(iters) != 2 {
		t.Fatalf("got %d, want 2", len(iters))
	}
	// created_at ascending: r2 (earlier) = run 1, r1 (later) = run 2. Newest
	// first in the thread → r1 (run 2) at top.
	if iters[0].RunNumber != 2 || iters[0].ID != "gd-r1" {
		t.Errorf("top = %q run %d, want gd-r1 run 2", iters[0].ID, iters[0].RunNumber)
	}
	// Canonical runLabel (gardener_timeline.go) keeps the "of N" budget on
	// closed runs even under the by-time fallback.
	if !iters[0].ByTime || iters[0].RunLabel != "Iteration 2 of 10 (by time)" {
		t.Errorf("byTime/label = %v/%q", iters[0].ByTime, iters[0].RunLabel)
	}
}

func TestGardenerIterations_FiltersByScheduleID(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	mine := mkIteration("gd-mine", "gd-cap", "", 1, "closed", t0)
	other := mkIteration("gd-other", "gd-DIFFERENT", "", 1, "closed", t0)
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{mine, other}, Total: 2}

	iters, _, err := s.gardenerIterations(context.Background(), "gd-cap", s.now())
	if err != nil {
		t.Fatalf("gardenerIterations: %v", err)
	}
	if len(iters) != 1 || iters[0].ID != "gd-mine" {
		t.Fatalf("expected only gd-mine, got %+v", iters)
	}
}

// TestGardenerIterations_CountsTriggerID is the regression guard for
// ka-0000007051: modern gardeners spawn via the bridge-trigger path, which
// stamps trigger_id (NOT schedule_id) on each iteration. The gardener page
// previously queried only schedule_id, so /gardener/{id} rendered 0 Iterations /
// 0 Runs for every active gardener. gardenerIterations must match on trigger_id
// too.
func TestGardenerIterations_CountsTriggerID(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// Three trigger-path iterations: schedule_id ABSENT, trigger_id == gardener.
	r1 := mkTriggerIteration("gd-r1", "gd-cap", 1, "closed", t0)
	r2 := mkTriggerIteration("gd-r2", "gd-cap", 2, "closed", t0.Add(time.Hour))
	r3 := mkTriggerIteration("gd-r3", "gd-cap", 3, "in_progress", t0.Add(2*time.Hour))
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{r2, r3, r1}, Total: 3}

	iters, _, err := s.gardenerIterations(context.Background(), "gd-cap", s.now())
	if err != nil {
		t.Fatalf("gardenerIterations: %v", err)
	}
	if len(iters) != 3 {
		t.Fatalf("got %d iterations, want 3 (trigger_id path must be counted)", len(iters))
	}
	// No schedule_prior_agent_id chain on trigger-path spawns → created_at
	// fallback ordering (newest first), every card ByTime.
	if iters[0].ID != "gd-r3" {
		t.Errorf("head = %q, want gd-r3 (newest by created_at)", iters[0].ID)
	}
}

// TestGardenerIterations_UnionAndDedupe proves the dual-key match unions a mix
// of legacy (schedule_id) and trigger-path (trigger_id) iterations, and that a
// bead carrying BOTH keys for this gardener is counted exactly once.
func TestGardenerIterations_UnionAndDedupe(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	legacy := mkIteration("gd-legacy", "gd-cap", "", 1, "closed", t0)
	trigger := mkTriggerIteration("gd-trigger", "gd-cap", 2, "closed", t0.Add(time.Hour))
	// A bead stamped with both keys pointing at this gardener must not double-count.
	both := mkTriggerIteration("gd-both", "gd-cap", 3, "closed", t0.Add(2*time.Hour))
	both.Fields["schedule_id"] = "gd-cap"
	other := mkTriggerIteration("gd-other", "gd-DIFFERENT", 1, "closed", t0)
	d.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{legacy, trigger, both, other},
		Total: 4,
	}

	iters, _, err := s.gardenerIterations(context.Background(), "gd-cap", s.now())
	if err != nil {
		t.Fatalf("gardenerIterations: %v", err)
	}
	if len(iters) != 3 {
		t.Fatalf("got %d iterations, want 3 (legacy+trigger+both, deduped, other excluded)", len(iters))
	}
	for _, it := range iters {
		if it.ID == "gd-other" {
			t.Errorf("gd-other (different gardener) must be excluded")
		}
	}
}

func TestGardenerIterations_Empty(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)
	d.liveAgentsResp = &beadsapi.ListBeadsResult{}

	iters, _, err := s.gardenerIterations(context.Background(), "gd-cap", s.now())
	if err != nil {
		t.Fatalf("gardenerIterations: %v", err)
	}
	if len(iters) != 0 {
		t.Errorf("expected no iterations, got %d", len(iters))
	}

	// Empty schedule id short-circuits without a daemon call.
	if iters, trunc, err := s.gardenerIterations(context.Background(), "", s.now()); err != nil || iters != nil || trunc {
		t.Errorf("empty scheduleID: got %v / %v / %v", iters, trunc, err)
	}
}

// TestGardenerIterations_HistoryTruncated is the guard for ka-0000002835: a
// per-key query that fills the gardenerIterationPageLimit page means the daemon
// holds more iterations for that key than the page materialized, so the rendered
// thread is missing its oldest runs. gardenerIterations must report truncated so
// the page can surface the per-key ceiling before a gardener silently crosses it.
func TestGardenerIterations_HistoryTruncated(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// A full page of legacy (schedule_id) iterations: len == the cap, so the
	// daemon almost certainly has more it could not return.
	beads := make([]*beadsapi.BeadDetail, 0, gardenerIterationPageLimit)
	for i := range gardenerIterationPageLimit {
		beads = append(beads, mkIteration("gd-r"+itoa(i), "gd-cap", "", i+1, "closed", t0.Add(time.Duration(i)*time.Minute)))
	}
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: beads, Total: gardenerIterationPageLimit}

	iters, truncated, err := s.gardenerIterations(context.Background(), "gd-cap", s.now())
	if err != nil {
		t.Fatalf("gardenerIterations: %v", err)
	}
	if !truncated {
		t.Errorf("expected truncated=true when a per-key page fills the cap (got %d iterations)", len(iters))
	}
}

// TestGardenerIterations_NotTruncatedBelowCap proves the guard does NOT false-fire
// on a thread comfortably under the per-key ceiling — the common case today
// (largest single-key gardener is ~184/500).
func TestGardenerIterations_NotTruncatedBelowCap(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	r1 := mkIteration("gd-r1", "gd-cap", "", 1, "closed", t0)
	r2 := mkIteration("gd-r2", "gd-cap", "gd-r1", 2, "closed", t0.Add(time.Hour))
	d.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{r1, r2}, Total: 2}

	_, truncated, err := s.gardenerIterations(context.Background(), "gd-cap", s.now())
	if err != nil {
		t.Fatalf("gardenerIterations: %v", err)
	}
	if truncated {
		t.Errorf("expected truncated=false for a 2-iteration thread well under the cap")
	}
}

// NB: run-label formatting (runLabel) and the pure ordering function
// (orderGardenerIterations) are exercised canonically in gardener_timeline_test.go
// (Q4, ka-0000004816), which owns those symbols. The tests above exercise the
// Server-level integration: gardenerViewFromBead (header decode) and
// gardenerIterations (daemon lookup → field filter → ordered life-thread).
