package conditions

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The quiet_gardeners leaf counts a building plan whose gardener the engine
// closed (gd-DivW9ZOyL2): on 11-04 seven gardeners closed on max_runs at
// 09:24Z, the leaf read 0 at 10:57Z and 12:57Z, and no one rebound them for
// six hours.

// closedGardener is a plan gardener the engine closed on kind, an hour ago.
func (f *quietFarm) closedGardener(id, plan, kind, fireError string) *beadsapi.BeadDetail {
	b := f.gardener(id, plan, "event", 3*time.Hour, map[string]string{
		"gardener_terminated_kind": kind, "last_fire_error": fireError,
	})
	b.Status, b.ClosedAt = "closed", quietAt.Add(-time.Hour).Format(time.RFC3339)
	return b
}

// gardenerlessFarm has one building plan, gd-p1, with a ready step and its
// gardener closed on kind.
func gardenerlessFarm(kind, fireError string) *quietFarm {
	f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
	f.plan("gd-p1", "building")
	f.closedGardener("gd-g1", "gd-p1", kind, fireError)
	f.step("gd-s1", "gd-p1", "task", "open", nil)
	return f
}

func countQuiet(t *testing.T, f *quietFarm) int {
	t.Helper()
	n, err := CountQuietGardeners(context.Background(), f, "gardenio", quietAt)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestQuietGardenersLeaf_EngineClosedGardenerWakes(t *testing.T) {
	now := quietAt
	resetQuietMemo(t, &now)
	f := gardenerlessFarm("max_runs", "")
	if ok, err := Evaluate(context.Background(), quietLeaf(), Env{Bead: f}); err != nil || !ok {
		t.Fatalf("engine-closed gardener with a ready step: %v, %v; want true", ok, err)
	}
	// The prompt's quiet list does not have it: the gardener-less list does.
	if q := findQuiet(t, f); len(q) != 0 {
		t.Fatalf("FindQuietGardeners lists the gardener-less plan: %+v", q)
	}
}

// Only the stops the steward rebinds itself count; a plan whose steps all
// wait, or whose gardener failed on a fault, would wake it every 2h for
// nothing it can do.
func TestCountGardenerless_RebindOnly(t *testing.T) {
	for _, c := range []struct {
		name      string
		kind, err string
		want      int
	}{
		{"max_runs", "max_runs", "", 1},
		{"wall_clock", "wall_clock", "", 1},
		{"no_progress_for", "no_progress_for", "", 1},
		{"bead_query", "bead_query", "", 1},
		{"spawns refused", "consecutive_failures", "spawn: 403 Forbidden", 1},
		{"a fault (tell)", "consecutive_failures", "spawn: exit status 1", 0},
		{"stopped by hand (tell)", "manual", "", 0},
		{"stop not recorded (tell)", "", "", 0},
	} {
		if got := countQuiet(t, gardenerlessFarm(c.kind, c.err)); got != c.want {
			t.Errorf("%s: count %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCountGardenerless_NeverBoundAndPaused(t *testing.T) {
	f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
	f.plan("gd-p1", "building")
	f.step("gd-s1", "gd-p1", "task", "open", nil)
	f.plan("gd-p2", "building")
	f.gardener("gd-g2", "gd-p2", "event", 9*time.Hour, map[string]string{"enabled": "false"})
	f.step("gd-s2", "gd-p2", "task", "open", nil)
	if got := countQuiet(t, f); got != 2 {
		t.Fatalf("never bound and paused: count %d, want 2", got)
	}
}

func TestCountGardenerless_WaitsAndCompleteDoNotCount(t *testing.T) {
	f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
	// Waits: every open step waits on a bead or a decision.
	f.plan("gd-p1", "building")
	f.closedGardener("gd-g1", "gd-p1", "max_runs", "")
	f.step("gd-s1a", "gd-p1", "task", "open", nil)
	f.waiting["gd-s1a"] = true
	f.step("gd-s1b", "gd-p1", "task", "open", func(b *beadsapi.BeadDetail) { b.BlockedOnDecision = true })
	// Complete: no open step is left.
	f.plan("gd-p2", "building")
	f.closedGardener("gd-g2", "gd-p2", "max_runs", "")
	f.step("gd-s2", "gd-p2", "task", "closed", nil)
	if got := countQuiet(t, f); got != 0 {
		t.Fatalf("waits and complete: count %d, want 0", got)
	}
}

// A plan the steward has seen to in the last day does not count again, so
// one it cannot fix wakes it at most once a day.
func TestCountGardenerless_SeenToInTheWindow(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*quietFarm)
		want int
	}{
		{"rebound 1h ago", func(f *quietFarm) {
			f.comment("gd-p1", "gardenio-steward-a1b", "steward: re-armed the gardener (gd-g0 stopped on max_runs); 1 steps ready"+GardenerlessSeenMarker, time.Hour)
		}, 0},
		{"told the owner 23h ago", func(f *quietFarm) {
			f.comment("gd-p1", "gardenio-steward-a1b", "steward: rearm refused: 403"+GardenerlessSeenMarker, 23*time.Hour)
		}, 0},
		{"asked the owner 1h ago", func(f *quietFarm) { f.decision("gd-d1", "gd-g1", "gardenio-steward-a1b", time.Hour) }, 0},
		{"rebound 25h ago", func(f *quietFarm) {
			f.comment("gd-p1", "gardenio-steward-a1b", "steward: rebound"+GardenerlessSeenMarker, 25*time.Hour)
		}, 1},
		{"marker by someone else", func(f *quietFarm) {
			f.comment("gd-p1", "gardenio-crew-x1y", "steward: rebound"+GardenerlessSeenMarker, time.Hour)
		}, 1},
		{"a nudge is not a rebind", func(f *quietFarm) {
			f.comment("gd-p1", "gardenio-steward-a1b", QuietNudgeText("gd-g1", 3*time.Hour, 1), time.Hour)
		}, 1},
		{"another land's steward decision", func(f *quietFarm) { f.decision("gd-d1", "gd-g1", "acme-steward-a1b", time.Hour) }, 1},
	} {
		f := gardenerlessFarm("max_runs", "")
		c.edit(f)
		if got := countQuiet(t, f); got != c.want {
			t.Errorf("%s: count %d, want %d", c.name, got, c.want)
		}
	}
}

// A plan with any open, enabled trigger bound to it is live, as the
// gardener-less list reads it, even without the plan-gardener label; the
// newest closed gardener is the one classified.
func TestCountGardenerless_LiveAndNewestClosed(t *testing.T) {
	f := gardenerlessFarm("max_runs", "")
	f.beads = append(f.beads, &beadsapi.BeadDetail{ID: "gd-g1b", Type: "trigger", Status: "open",
		Labels: []string{"role:gardener"}, Fields: map[string]string{"assign_bead": "gd-p1", "enabled": "true"}})
	if got := countQuiet(t, f); got != 0 {
		t.Fatalf("live by assign_bead: count %d, want 0", got)
	}
	f = gardenerlessFarm("max_runs", "")
	f.closedGardener("gd-g0", "gd-p1", "manual", "").ClosedAt = quietAt.Add(-48 * time.Hour).Format(time.RFC3339)
	if got := countQuiet(t, f); got != 1 {
		t.Fatalf("an older hand-closed gardener: count %d, want 1", got)
	}
	f.closedGardener("gd-g2", "gd-p1", "manual", "")
	f.beads[len(f.beads)-1].ClosedAt = quietAt.Add(-time.Minute).Format(time.RFC3339)
	if got := countQuiet(t, f); got != 0 {
		t.Fatalf("the newest gardener was closed by hand: count %d, want 0", got)
	}
}

// A live quiet gardener still wakes the steward beside a gardener-less plan,
// and each counts once.
func TestCountGardenerless_BesideQuietGardeners(t *testing.T) {
	f := gardenerlessFarm("max_runs", "")
	f.plan("gd-p2", "building")
	f.gardener("gd-g2", "gd-p2", "event", 3*time.Hour, nil)
	f.step("gd-s2", "gd-p2", "task", "open", nil)
	if got := countQuiet(t, f); got != 2 {
		t.Fatalf("a quiet and a gardener-less plan: count %d, want 2", got)
	}
}

// A read error fails the count: a watch that cannot see wakes nobody.
func TestCountGardenerless_ReadErrorFails(t *testing.T) {
	f := gardenerlessFarm("max_runs", "")
	f.failComments = true
	if _, err := CountQuietGardeners(context.Background(), f, "gardenio", quietAt); err == nil {
		t.Fatal("comment read error: want an error")
	}
	f = gardenerlessFarm("max_runs", "")
	f.fail = func(q beadsapi.ListBeadsQuery) bool { return len(q.Statuses) == 1 && q.Statuses[0] == "closed" }
	if _, err := CountQuietGardeners(context.Background(), f, "gardenio", quietAt); err == nil {
		t.Fatal("last gardener read error: want an error")
	}
}
