package main

import (
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

const dedupURL = "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/807"

func mrDedupBead(id string, status model.Status, updated time.Time, fields map[string]any) *model.Bead {
	raw, _ := json.Marshal(fields)
	return &model.Bead{
		ID:        id,
		Type:      "merge-request",
		Kind:      model.KindIssue,
		Status:    status,
		UpdatedAt: updated,
		Fields:    raw,
	}
}

func resetMRDedupFlags(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"dry-run": "true", "apply": "false", "limit": "0", "page-size": "200",
	} {
		if err := adminMigrateMRBeadDedupCmd.Flags().Set(k, v); err != nil {
			t.Fatalf("reset %s: %v", k, err)
		}
	}
}

func runMRDedup(t *testing.T, mc *mockBeadsClient, apply bool) string {
	t.Helper()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMRDedupFlags(t)
	if apply {
		_ = adminMigrateMRBeadDedupCmd.Flags().Set("apply", "true")
		_ = adminMigrateMRBeadDedupCmd.Flags().Set("dry-run", "false")
	}
	out := captureStdout(t, func() {
		if err := adminMigrateMRBeadDedupCmd.RunE(adminMigrateMRBeadDedupCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	return out
}

// --- pure survivor-selection precedence ---------------------------------

func TestPickSurvivor_TerminalOutcomeWins(t *testing.T) {
	older := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	// The freshly-updated bead is still `opened`; an older bead carries the
	// terminal outcome. Terminal outcome must win over recency.
	fresh := mrDedupBead("gd-fresh", model.StatusOpen, newer, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	done := mrDedupBead("gd-done", model.StatusClosed, older, map[string]any{"mr_url": dedupURL, "mr_state": "merged", "mr_outcome": "merged"})
	got := pickSurvivorMRBead([]*model.Bead{fresh, done})
	if got.ID != "gd-done" {
		t.Fatalf("survivor = %s, want gd-done (terminal outcome beats recency)", got.ID)
	}
}

func TestPickSurvivor_TerminalStateBeatsOpened(t *testing.T) {
	older := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	// Neither carries mr_outcome; the terminal mr_state wins over a fresher opened.
	fresh := mrDedupBead("gd-fresh", model.StatusOpen, newer, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	closed := mrDedupBead("gd-closed", model.StatusClosed, older, map[string]any{"mr_url": dedupURL, "mr_state": "closed"})
	got := pickSurvivorMRBead([]*model.Bead{fresh, closed})
	if got.ID != "gd-closed" {
		t.Fatalf("survivor = %s, want gd-closed (terminal state beats recency)", got.ID)
	}
}

func TestPickSurvivor_RecencyThenID(t *testing.T) {
	older := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	// Both non-terminal → fresher wins.
	a := mrDedupBead("gd-aaa", model.StatusOpen, older, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	b := mrDedupBead("gd-bbb", model.StatusOpen, newer, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	if got := pickSurvivorMRBead([]*model.Bead{a, b}); got.ID != "gd-bbb" {
		t.Fatalf("survivor = %s, want gd-bbb (fresher)", got.ID)
	}
	// Same timestamp → larger id wins, deterministically.
	c := mrDedupBead("gd-ccc", model.StatusOpen, newer, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	d := mrDedupBead("gd-ddd", model.StatusOpen, newer, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	if got := pickSurvivorMRBead([]*model.Bead{c, d}); got.ID != "gd-ddd" {
		t.Fatalf("survivor = %s, want gd-ddd (larger id tie-break)", got.ID)
	}
}

// --- dry-run performs no writes -----------------------------------------

func TestMRDedup_DryRunNoWrites(t *testing.T) {
	mc := newMockClient()
	older := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	win := mrDedupBead("gd-win", model.StatusClosed, newer, map[string]any{"mr_url": dedupURL, "mr_state": "merged", "mr_outcome": "merged"})
	lose := mrDedupBead("gd-lose", model.StatusOpen, older, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	mc.ListBeadsPages = [][]*model.Bead{{win, lose}, {}}

	runMRDedup(t, mc, false)

	if len(mc.CloseBeadCalls) != 0 {
		t.Fatalf("dry-run closed beads: %+v", mc.CloseBeadCalls)
	}
	if len(mc.AddDepCalls) != 0 || len(mc.RemoveDepCalls) != 0 {
		t.Fatalf("dry-run mutated edges: add=%+v remove=%+v", mc.AddDepCalls, mc.RemoveDepCalls)
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("dry-run updated fields: %+v", mc.UpdateBeadCalls)
	}
}

// --- apply: re-point edges, stamp marker, close loser -------------------

func TestMRDedup_ApplyRepointsAndCloses(t *testing.T) {
	mc := newMockClient()
	older := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	win := mrDedupBead("gd-win", model.StatusClosed, newer, map[string]any{"mr_url": dedupURL, "mr_state": "merged", "mr_outcome": "merged"})
	lose := mrDedupBead("gd-lose", model.StatusOpen, older, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	mc.ListBeadsPages = [][]*model.Bead{{win, lose}, {}}

	// The loser owns a forward triggered-by(loser→task) edge, and a producing
	// task points at it via produces(task→loser).
	mc.Deps = map[string][]*model.Dependency{
		"gd-lose": {{BeadID: "gd-lose", DependsOnID: "gd-task", Type: "triggered-by"}},
	}
	mc.RevDeps = map[string][]*model.Dependency{
		"gd-lose": {{BeadID: "gd-task", DependsOnID: "gd-lose", Type: "produces"}},
	}

	runMRDedup(t, mc, true)

	// Loser closed exactly once.
	if len(mc.CloseBeadCalls) != 1 || mc.CloseBeadCalls[0].ID != "gd-lose" {
		t.Fatalf("close calls = %+v, want one close of gd-lose", mc.CloseBeadCalls)
	}

	// Forward triggered-by re-pointed onto survivor: add(survivor→task), remove(lose→task).
	var addTrig, addProd bool
	for _, d := range mc.AddDepCalls {
		if d.BeadID == "gd-win" && d.DependsOnID == "gd-task" && d.Type == "triggered-by" {
			addTrig = true
		}
		if d.BeadID == "gd-task" && d.DependsOnID == "gd-win" && d.Type == "produces" {
			addProd = true
		}
	}
	if !addTrig {
		t.Errorf("missing add triggered-by(win→task); adds=%+v", mc.AddDepCalls)
	}
	if !addProd {
		t.Errorf("missing add produces(task→win); adds=%+v", mc.AddDepCalls)
	}

	var rmTrig, rmProd bool
	for _, r := range mc.RemoveDepCalls {
		if r.BeadID == "gd-lose" && r.DependsOnID == "gd-task" && r.Type == "triggered-by" {
			rmTrig = true
		}
		if r.BeadID == "gd-task" && r.DependsOnID == "gd-lose" && r.Type == "produces" {
			rmProd = true
		}
	}
	if !rmTrig {
		t.Errorf("missing remove triggered-by(lose→task); removes=%+v", mc.RemoveDepCalls)
	}
	if !rmProd {
		t.Errorf("missing remove produces(task→lose); removes=%+v", mc.RemoveDepCalls)
	}

	// Marker fields stamped on the loser AND its mr_url cleared. Clearing
	// mr_url (delete-on-null) is what removes the loser from the status-agnostic
	// UNIQUE-index population migration 042 builds — without it a "successful"
	// reconcile still 23505s (ka-0000000451).
	var stamped bool
	for _, u := range mc.UpdateBeadCalls {
		if u.ID != "gd-lose" || u.Req.Fields == nil {
			continue
		}
		var f map[string]any
		if err := json.Unmarshal(u.Req.Fields, &f); err != nil {
			t.Fatalf("marker fields unmarshal: %v", err)
		}
		mrURLKey, mrURLSet := f["mr_url"]
		if f["merged_into"] == "gd-win" && f["dedup_reason"] != "" &&
			f["dedup_former_mr_url"] == dedupURL && mrURLSet && mrURLKey == nil {
			stamped = true
		}
	}
	if !stamped {
		t.Errorf("marker fields not stamped / mr_url not cleared on gd-lose; updates=%+v", mc.UpdateBeadCalls)
	}
}

// --- already-closed loser: marker stamped, no re-close -------------------

func TestMRDedup_AlreadyClosedLoserNotReclosed(t *testing.T) {
	mc := newMockClient()
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	win := mrDedupBead("gd-win", model.StatusClosed, t0.Add(time.Hour), map[string]any{"mr_url": dedupURL, "mr_state": "merged", "mr_outcome": "merged"})
	// Loser also already closed (a duplicate close record) but non-terminal state.
	lose := mrDedupBead("gd-lose", model.StatusClosed, t0, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	mc.ListBeadsPages = [][]*model.Bead{{win, lose}, {}}

	runMRDedup(t, mc, true)

	if len(mc.CloseBeadCalls) != 0 {
		t.Fatalf("already-closed loser was re-closed: %+v", mc.CloseBeadCalls)
	}
	// Marker fields still stamped.
	if len(mc.UpdateBeadCalls) != 1 || mc.UpdateBeadCalls[0].ID != "gd-lose" {
		t.Fatalf("marker not stamped on already-closed loser: %+v", mc.UpdateBeadCalls)
	}
}

// --- single-bead url is a no-op (idempotency after reconcile) -----------

func TestMRDedup_SingleBeadNoOp(t *testing.T) {
	mc := newMockClient()
	t0 := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	only := mrDedupBead("gd-only", model.StatusClosed, t0, map[string]any{"mr_url": dedupURL, "mr_state": "merged", "mr_outcome": "merged"})
	mc.ListBeadsPages = [][]*model.Bead{{only}, {}}

	runMRDedup(t, mc, true)

	if len(mc.CloseBeadCalls) != 0 || len(mc.AddDepCalls) != 0 || len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("single-bead url produced writes: close=%+v add=%+v update=%+v",
			mc.CloseBeadCalls, mc.AddDepCalls, mc.UpdateBeadCalls)
	}
}

// --- edges onto the survivor itself are skipped, not re-added -----------

func TestMRDedup_SkipsEdgeOntoSurvivor(t *testing.T) {
	mc := newMockClient()
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	win := mrDedupBead("gd-win", model.StatusClosed, t0.Add(time.Hour), map[string]any{"mr_url": dedupURL, "mr_state": "merged", "mr_outcome": "merged"})
	lose := mrDedupBead("gd-lose", model.StatusOpen, t0, map[string]any{"mr_url": dedupURL, "mr_state": "opened"})
	mc.ListBeadsPages = [][]*model.Bead{{win, lose}, {}}
	// The loser already has a triggered-by pointing at the survivor — skip it.
	mc.Deps = map[string][]*model.Dependency{
		"gd-lose": {{BeadID: "gd-lose", DependsOnID: "gd-win", Type: "triggered-by"}},
	}

	runMRDedup(t, mc, true)

	for _, d := range mc.AddDepCalls {
		if d.DependsOnID == "gd-win" && d.BeadID == "gd-win" {
			t.Fatalf("self-edge added onto survivor: %+v", d)
		}
	}
	// No triggered-by re-point add for the survivor-target edge.
	for _, d := range mc.AddDepCalls {
		if d.Type == "triggered-by" && d.BeadID == "gd-win" && d.DependsOnID == "gd-win" {
			t.Fatalf("edge onto survivor should have been skipped: %+v", d)
		}
	}
}
