// Tests for the time_window overlay of `gb trigger reconcile`
// (trigger_reconcile_timewindow.go).
//
// The centerpiece is TestTriggerReconcile_TimeWindowExplainsQuietHourMisses —
// the regression for the false positive ka-0000005666 drove: the gardenio
// gardener-release-owner v4 (cron 13,43 * * * * PT, gated by a `time_window`
// outside 07:00-18:00 Mon-Fri quiet-hours condition) fired 40/54 slots over a
// week; every "missed" slot fell strictly inside a weekday quiet-hours window it
// deliberately does not fire in. The point-in-time would_fire_now gate zeros that
// residual ONLY when the reconciler happens to run during quiet hours; run
// outside the window (as this test does) and the gate reads would_fire_now=true,
// so the pre-fix code reported the quiet-hours misses as phantom `habitual`
// drift. The historical overlay attributes each missed slot to the quiet window
// it actually fell in, deterministically, so residual_missed==0 and
// drift_candidate_missed==0 regardless of when the reconciler runs.
package main

import (
	"testing"
	"time"

	_ "time/tzdata"

	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// quietHoursConditions is the release gardener's exact time_window quiet-hours
// condition (ka-0000004693): fire only OUTSIDE 07:00-18:00 Mon-Fri Pacific.
const quietHoursConditions = `[{"type":"time_window","timezone":"America/Los_Angeles","window":"07:00-18:00","days":["Mon","Tue","Wed","Thu","Fri"],"op":"outside"}]`

// inReleaseQuietHours reports whether a UTC instant falls inside the release
// quiet-hours window (weekday 07:00-18:00 Pacific) — the test's independent
// oracle for which cron slots the trigger legitimately suppresses.
func inReleaseQuietHours(t *testing.T, at time.Time) bool {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	l := at.In(loc)
	if l.Weekday() == time.Saturday || l.Weekday() == time.Sunday {
		return false
	}
	minOfDay := l.Hour()*60 + l.Minute()
	return minOfDay >= 7*60 && minOfDay < 18*60
}

// TestTriggerReconcile_TimeWindowExplainsQuietHourMisses reconstructs ka-0000005666:
// a `13,43 * * * *` PT release gardener gated by the quiet-hours time_window,
// firing every slot EXCEPT those inside a weekday 07:00-18:00 PT quiet window.
// The reconciler runs OUTSIDE the window (Friday evening PT) so the point-in-time
// gate would say would_fire_now=true — the exact state that made the pre-fix
// drift-candidate count nonzero. The time_window overlay must attribute every
// missed slot to the quiet window and leave residual/drift-candidate at 0.
func TestTriggerReconcile_TimeWindowExplainsQuietHourMisses(t *testing.T) {
	// Friday 2026-08-14 22:00 PT = Saturday 05:00 UTC — OUTSIDE quiet hours, so a
	// point-in-time gate reads would_fire_now=true (the bug-triggering sample time).
	now := mustTime(t, "2026-08-15T05:00:00Z")
	windowStart := now.Add(-24 * time.Hour) // Thu 22:00 PT → Fri 22:00 PT

	// Enumerate every cron slot (:13/:43 each hour) in the window; spawn a child
	// for each slot OUTSIDE quiet hours (a real fire) and leave the quiet-hours
	// slots as misses.
	var children []reconcileBead
	quietSlots := 0
	nonQuietSlots := 0
	for tt := windowStart.Truncate(time.Hour); !tt.After(now); tt = tt.Add(time.Hour) {
		for _, min := range []int{13, 43} {
			slot := tt.Add(time.Duration(min) * time.Minute)
			if slot.Before(windowStart) || slot.After(now) {
				continue
			}
			if inReleaseQuietHours(t, slot) {
				quietSlots++
				continue // suppressed by design — no child spawned
			}
			nonQuietSlots++
			children = append(children, reconcileBead{
				ID: "gd-fire-" + slot.Format("021504"), Type: "task",
				Fields: map[string]string{"schedule_fire_time": slot.Format(time.RFC3339)},
			})
		}
	}
	if quietSlots == 0 {
		t.Fatal("test setup: expected at least one quiet-hours slot in the window")
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigREL", Type: "trigger", Status: "open", Title: "release gardener v4",
			Labels:    []string{"project:gardenio"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "13,43 * * * *",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"conditions":  quietHoursConditions,
				"wake_intent": "every :13/:43 cut a release, unless quiet hours",
			},
		},
		children: children,
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigREL")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 24*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	// Pin the point-in-time gate probe's clock to `now` (Fri 22:00 PT, OUTSIDE
	// quiet hours) so would_fire_now=true deterministically — reproducing the
	// bug-triggering sample time regardless of when the suite actually runs.
	restore := conditions.SetTimeWindowNowForTest(func() time.Time { return now })
	t.Cleanup(restore)

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The unconditional diff must STILL report the quiet-hours slots as misses —
	// the overlay never mutates Fires (kept auditable, per the scope boundary).
	if rep.Fires.MissedCount != quietSlots {
		t.Fatalf("unconditional diff: expected missed=%d (the quiet-hours slots), got %d",
			quietSlots, rep.Fires.MissedCount)
	}
	if rep.Fires.MatchedCount != nonQuietSlots {
		t.Fatalf("unconditional diff: expected matched=%d, got %d", nonQuietSlots, rep.Fires.MatchedCount)
	}

	// The gate is a point-in-time snapshot at `now` (outside quiet hours) — it must
	// read would_fire_now=true, the exact state that made the pre-fix code report
	// the quiet-hours misses as drift. This asserts we are reproducing the bug, not
	// accidentally sampling inside the window where the gate would have masked it.
	if rep.Gate == nil || !rep.Gate.Evaluated {
		t.Fatalf("expected an evaluated gate, got %+v", rep.Gate)
	}
	if !rep.Gate.WouldFireNow {
		t.Fatal("test must sample OUTSIDE quiet hours (would_fire_now=true) to reproduce the bug")
	}

	// The load-bearing assertions: the time_window overlay attributes every miss
	// to the quiet window, so residual and the final drift candidate are 0.
	if rep.TimeWindow == nil {
		t.Fatal("expected a time_window overlay")
	}
	if !rep.TimeWindow.Evaluated {
		t.Fatalf("time_window overlay should have evaluated, got error %q", rep.TimeWindow.Error)
	}
	if rep.TimeWindow.SuppressedMissed != quietSlots {
		t.Fatalf("all %d quiet-hours misses should be time_window-suppressed, got suppressed=%d",
			quietSlots, rep.TimeWindow.SuppressedMissed)
	}
	if rep.TimeWindow.ResidualMissed != 0 {
		t.Fatalf("residual_missed must be 0 when the quiet-hours window explains the whole deficit, got %d",
			rep.TimeWindow.ResidualMissed)
	}
	if len(rep.TimeWindow.Leaves) != 1 || rep.TimeWindow.Leaves[0].Op != "outside" {
		t.Fatalf("expected one explaining time_window leaf (op=outside), got %+v", rep.TimeWindow.Leaves)
	}
	// The end-to-end contract: the deterministic drift-candidate count is 0, so the
	// reconciler never re-files a habitual bug for a trigger whose only deficit is
	// its own quiet-hours window doing its job.
	if rep.DriftCandidateMissed != 0 {
		t.Fatalf("drift_candidate_missed must be 0 (quiet-hours misses are not drift), got %d",
			rep.DriftCandidateMissed)
	}
}

// TestTriggerReconcile_TimeWindowDoesNotOverSubtract proves the overlay does NOT
// launder a genuine dark-lane miss: a slot OUTSIDE the quiet window that never
// fired stays a residual drift candidate even though the trigger also has quiet
// hours. A time_window gate must only explain misses that actually fell inside it.
func TestTriggerReconcile_TimeWindowDoesNotOverSubtract(t *testing.T) {
	now := mustTime(t, "2026-08-15T05:00:00Z")
	windowStart := now.Add(-24 * time.Hour)

	// Spawn a child for every OUTSIDE-quiet slot EXCEPT one specific evening slot —
	// a genuine miss with no quiet-hours excuse.
	darkSlot := mustTime(t, "2026-08-15T04:43:00Z") // Fri 21:43 PT — outside quiet hours
	var children []reconcileBead
	quietSlots := 0
	for tt := windowStart.Truncate(time.Hour); !tt.After(now); tt = tt.Add(time.Hour) {
		for _, min := range []int{13, 43} {
			slot := tt.Add(time.Duration(min) * time.Minute)
			if slot.Before(windowStart) || slot.After(now) {
				continue
			}
			if inReleaseQuietHours(t, slot) {
				quietSlots++
				continue
			}
			if slot.Equal(darkSlot) {
				continue // genuine dark-lane miss — no child
			}
			children = append(children, reconcileBead{
				ID: "gd-fire-" + slot.Format("021504"), Type: "task",
				Fields: map[string]string{"schedule_fire_time": slot.Format(time.RFC3339)},
			})
		}
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigREL2", Type: "trigger", Status: "open", Title: "release gardener v4",
			Labels:    []string{"project:gardenio"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "13,43 * * * *",
				"timezone":    "America/Los_Angeles",
				"enabled":     "true",
				"conditions":  quietHoursConditions,
				"wake_intent": "every :13/:43 cut a release, unless quiet hours",
			},
		},
		children: children,
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigREL2")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 24*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	restore := conditions.SetTimeWindowNowForTest(func() time.Time { return now })
	t.Cleanup(restore)

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil || rep.TimeWindow == nil || !rep.TimeWindow.Evaluated {
		t.Fatalf("expected an evaluated fires diff + time_window overlay, got fires=%v tw=%+v", rep.Fires, rep.TimeWindow)
	}
	// Quiet-hours slots are attributed to the window; the one dark slot is NOT.
	if rep.TimeWindow.SuppressedMissed != quietSlots {
		t.Fatalf("expected suppressed=%d (only the quiet-hours slots), got %d", quietSlots, rep.TimeWindow.SuppressedMissed)
	}
	if rep.TimeWindow.ResidualMissed != 1 {
		t.Fatalf("the genuine outside-window dark slot must remain residual, got residual=%d", rep.TimeWindow.ResidualMissed)
	}
	// The gate reads would_fire_now=true (outside quiet hours), so it cannot zero
	// the residual — the genuine miss survives as a drift candidate.
	if rep.DriftCandidateMissed != 1 {
		t.Fatalf("expected drift_candidate_missed=1 (the real dark slot), got %d", rep.DriftCandidateMissed)
	}
}

// TestBuildTimeWindowExclusion_NilFiresOmitsOverlay proves the overlay is omitted
// for an event-only / collapsed-window trigger (no fires diff), mirroring the
// pause/lease overlays' nil-fires contract.
func TestBuildTimeWindowExclusion_NilFiresOmitsOverlay(t *testing.T) {
	if tw := buildTimeWindowExclusion(quietHoursConditions, "", nil, nil, nil, outsideQuietHours(t)); tw != nil {
		t.Fatalf("expected nil overlay for nil fires, got %+v", tw)
	}
}

// TestBuildTimeWindowExclusion_NoConditionsOmitsOverlay proves an unconditional
// trigger (neither a flat conditions field NOR any triggers[] gate) gets no
// overlay — there is no time_window to explain any miss.
func TestBuildTimeWindowExclusion_NoConditionsOmitsOverlay(t *testing.T) {
	fires := &scheduler.FireReconciliation{MissedCount: 3}
	if tw := buildTimeWindowExclusion("", "", fires, nil, nil, outsideQuietHours(t)); tw != nil {
		t.Fatalf("expected nil overlay for empty conditions and triggers, got %+v", tw)
	}
}

// multiTriggersPreflightQuietHours is a trigger:multi bead's `triggers[]` where
// the quiet-hours time_window lives ONLY inside a cron entry's `preflight`, AND'd
// beneath the bead_query lane gate — the exact shape of the release gardener
// ka-0000003719. On such a bead the flat `conditions` field is empty, so an overlay
// that parses only `conditions` finds no quiet-hours gate and reports every
// by-design suppression as drift (the bug ka-0000006385). Note the window is nested
// under `all`, not a top-level leaf, so surfacing it requires walking the
// composition tree. A bead_event entry (a non-cron sibling the reconciler ignores)
// is included so the walk must skip it correctly.
const multiTriggersPreflightQuietHours = `[
  {"kind":"bead_event","events":["updated"],"match":{"all":[{"field":"type","op":"==","value":"merge-request"}]}},
  {"kind":"cron","cron":"13,43 * * * *","timezone":"America/Los_Angeles","preflight":{"all":[
    {"type":"bead_query","project":"gardenio","labels":["project:gardenio"],"statuses":["open"],"op":">","value":"0"},
    {"type":"time_window","timezone":"America/Los_Angeles","window":"07:00-18:00","days":["Mon","Tue","Wed","Thu","Fri"],"op":"outside"}
  ]}}
]`

// twoMissFires builds a FireReconciliation with exactly two missed slots: one
// INSIDE the release quiet-hours window (a by-design suppression) and one OUTSIDE
// it (a genuine dark-lane miss). The overlay must attribute the first to a
// time_window leaf and leave the second residual.
func twoMissFires(t *testing.T) (*scheduler.FireReconciliation, time.Time, time.Time) {
	t.Helper()
	quiet := mustTime(t, "2026-08-14T17:13:00Z") // Fri 10:13 PDT — inside quiet hours
	dark := mustTime(t, "2026-08-14T04:43:00Z")  // Thu 21:43 PDT — outside quiet hours
	if !inReleaseQuietHours(t, quiet) {
		t.Fatal("test setup: the quiet slot must fall inside quiet hours")
	}
	if inReleaseQuietHours(t, dark) {
		t.Fatal("test setup: the dark slot must fall outside quiet hours")
	}
	return &scheduler.FireReconciliation{
		MissedCount: 2,
		Expected: []scheduler.ExpectedFire{
			{At: quiet, Matched: false},
			{At: dark, Matched: false},
		},
	}, quiet, dark
}

// TestBuildTimeWindowExclusion_MultiPreflightNestedTimeWindow is the core
// regression for ka-0000006385: the quiet-hours window lives ONLY in
// triggers[].preflight (nested under `all`), the flat `conditions` field is empty,
// and the overlay must still find it — attributing the inside-window miss and
// leaving the outside-window miss residual. The paired assertion below (with an
// empty triggersRaw) proves the pre-fix code path could not see the window at all.
func TestBuildTimeWindowExclusion_MultiPreflightNestedTimeWindow(t *testing.T) {
	fires, _, _ := twoMissFires(t)

	// Pre-fix behavior: reading only the (empty) flat conditions, the overlay is
	// omitted entirely — so the caller falls back to the raw lease residual and BOTH
	// misses (including the by-design quiet-hours one) read as drift candidates.
	if tw := buildTimeWindowExclusion("", "", fires, nil, nil, outsideQuietHours(t)); tw != nil {
		t.Fatalf("without triggersRaw the overlay must be omitted (the bug), got %+v", tw)
	}

	// Fixed behavior: walking triggers[].preflight surfaces the nested window.
	tw := buildTimeWindowExclusion("", multiTriggersPreflightQuietHours, fires, nil, nil, outsideQuietHours(t))
	if tw == nil || !tw.Evaluated {
		t.Fatalf("expected an evaluated overlay from the preflight window, got %+v", tw)
	}
	if tw.SuppressedMissed != 1 {
		t.Fatalf("the inside-window miss must be attributed to the nested preflight time_window, got suppressed=%d", tw.SuppressedMissed)
	}
	if tw.ResidualMissed != 1 {
		t.Fatalf("the outside-window miss must remain residual (not laundered), got residual=%d", tw.ResidualMissed)
	}
	if len(tw.Leaves) != 1 || tw.Leaves[0].Op != "outside" || tw.Leaves[0].Window != "07:00-18:00" {
		t.Fatalf("expected the nested time_window leaf surfaced, got %+v", tw.Leaves)
	}
}

// TestBuildTimeWindowExclusion_DedupesLeafAcrossBothRegimes proves a window that
// appears in BOTH the flat conditions AND a triggers[].preflight — the deliberate
// duplicate state ka-0000003719 carries today — is unioned to a SINGLE leaf and its
// slot attributed once, never double-counted.
func TestBuildTimeWindowExclusion_DedupesLeafAcrossBothRegimes(t *testing.T) {
	fires, _, _ := twoMissFires(t)
	tw := buildTimeWindowExclusion(quietHoursConditions, multiTriggersPreflightQuietHours, fires, nil, nil, outsideQuietHours(t))
	if tw == nil || !tw.Evaluated {
		t.Fatalf("expected an evaluated overlay, got %+v", tw)
	}
	if tw.SuppressedMissed != 1 {
		t.Fatalf("the duplicated window must attribute the inside-window slot once, got suppressed=%d", tw.SuppressedMissed)
	}
	if tw.ResidualMissed != 1 {
		t.Fatalf("the outside-window miss must remain residual, got residual=%d", tw.ResidualMissed)
	}
	if len(tw.Leaves) != 1 {
		t.Fatalf("a window duplicated across both regimes must surface as ONE leaf, got %d: %+v", len(tw.Leaves), tw.Leaves)
	}
	if tw.Leaves[0].SuppressedMissed != 1 {
		t.Fatalf("the single deduped leaf must own the single suppressed slot, got %d", tw.Leaves[0].SuppressedMissed)
	}
}

// TestBuildTimeWindowExclusion_MatchTimeWindowCollected proves a time_window that
// lives in a triggers[].match expression (not preflight) is also collected — the
// fix walks both per-entry expressions, as the fix sketch specifies.
func TestBuildTimeWindowExclusion_MatchTimeWindowCollected(t *testing.T) {
	fires, _, _ := twoMissFires(t)
	// One bead_event entry whose MATCH nests the quiet-hours window; no preflight,
	// no flat conditions.
	triggers := `[{"kind":"bead_event","events":["closed"],"match":{"all":[
	  {"field":"type","op":"==","value":"merge-request"},
	  {"type":"time_window","timezone":"America/Los_Angeles","window":"07:00-18:00","days":["Mon","Tue","Wed","Thu","Fri"],"op":"outside"}
	]}}]`
	tw := buildTimeWindowExclusion("", triggers, fires, nil, nil, outsideQuietHours(t))
	if tw == nil || !tw.Evaluated {
		t.Fatalf("expected an evaluated overlay from the match window, got %+v", tw)
	}
	if tw.SuppressedMissed != 1 || tw.ResidualMissed != 1 {
		t.Fatalf("a time_window in triggers[].match must be collected, got suppressed=%d residual=%d",
			tw.SuppressedMissed, tw.ResidualMissed)
	}
}

// TestBuildTimeWindowExclusion_FailSoftOnTriggersParseError proves a malformed
// triggers field yields an Evaluated=false overlay, never a panic — the same
// fail-soft contract the flat conditions path has.
func TestBuildTimeWindowExclusion_FailSoftOnTriggersParseError(t *testing.T) {
	fires := &scheduler.FireReconciliation{MissedCount: 1}
	tw := buildTimeWindowExclusion("", "[{not valid json", fires, nil, nil, outsideQuietHours(t))
	if tw == nil || tw.Evaluated {
		t.Fatalf("expected an un-evaluated overlay on triggers parse error, got %+v", tw)
	}
	if tw.Error == "" {
		t.Fatal("expected a fail-soft error message")
	}
}

// TestCollectTimeWindowLeaves_WalksCompositions unit-tests the composition walk in
// isolation: a time_window nested under all / any / not must all be found, while
// non-time_window leaves (bead_query, field) are skipped. This is the primitive the
// bug turned on — before the fix only top-level leaves were consulted.
func TestCollectTimeWindowLeaves_WalksCompositions(t *testing.T) {
	// time_window leaves at three nesting depths, each with a distinct window so we
	// can assert exactly which were collected; plus a bead_query and a field leaf
	// that must be ignored.
	raw := `{"all":[
	  {"type":"bead_query","op":">","value":"0"},
	  {"any":[
	    {"field":"type","op":"==","value":"merge-request"},
	    {"type":"time_window","window":"01:00-02:00","op":"outside"}
	  ]},
	  {"not":{"type":"time_window","window":"03:00-04:00","op":"inside"}},
	  {"type":"time_window","window":"05:00-06:00","op":"outside"}
	]}`
	expr, err := conditions.ParseExpression(raw)
	if err != nil {
		t.Fatalf("parse expression: %v", err)
	}
	var got []string
	collectTimeWindowLeaves(expr, func(l conditions.Leaf) {
		if l.Type != "time_window" {
			t.Fatalf("collector emitted a non-time_window leaf: %+v", l)
		}
		got = append(got, l.Window)
	})
	want := map[string]bool{"01:00-02:00": true, "03:00-04:00": true, "05:00-06:00": true}
	if len(got) != len(want) {
		t.Fatalf("expected %d time_window leaves across the composition, got %d: %v", len(want), len(got), got)
	}
	for _, w := range got {
		if !want[w] {
			t.Fatalf("unexpected window collected: %q (all=%v)", w, got)
		}
	}
}

// TestTimeWindowLeafKey_DedupeIdentity pins the dedupe identity: two leaves that
// gate the same range on the same days in the same zone with the same op share a
// key (counted once when they appear in two regimes), and a change to ANY of those
// four fields yields a distinct key (never silently merged into one).
func TestTimeWindowLeafKey_DedupeIdentity(t *testing.T) {
	base := conditions.Leaf{
		Type: "time_window", Op: "outside", Window: "07:00-18:00",
		Timezone: "America/Los_Angeles", Days: []string{"Mon", "Tue"},
	}
	same := base // an identical leaf reached via the other regime
	if timeWindowLeafKey(base) != timeWindowLeafKey(same) {
		t.Fatal("identical time_window leaves must share a dedupe key")
	}
	for name, mut := range map[string]func(l conditions.Leaf) conditions.Leaf{
		"op":       func(l conditions.Leaf) conditions.Leaf { l.Op = "inside"; return l },
		"window":   func(l conditions.Leaf) conditions.Leaf { l.Window = "07:00-19:00"; return l },
		"timezone": func(l conditions.Leaf) conditions.Leaf { l.Timezone = "UTC"; return l },
		"days":     func(l conditions.Leaf) conditions.Leaf { l.Days = []string{"Mon"}; return l },
	} {
		if timeWindowLeafKey(base) == timeWindowLeafKey(mut(base)) {
			t.Fatalf("a leaf differing in %s must NOT dedupe against the base", name)
		}
	}
}

// TestBuildTimeWindowExclusion_FailSoftOnParseError proves a malformed conditions
// string yields an Evaluated=false overlay (advisory-absent), never a panic or a
// silently-erased signal — the fail-soft contract shared with pause/lease.
func TestBuildTimeWindowExclusion_FailSoftOnParseError(t *testing.T) {
	fires := &scheduler.FireReconciliation{MissedCount: 3}
	tw := buildTimeWindowExclusion("{not valid json", "", fires, nil, nil, outsideQuietHours(t))
	if tw == nil || tw.Evaluated {
		t.Fatalf("expected an un-evaluated overlay on parse error, got %+v", tw)
	}
	if tw.Error == "" {
		t.Fatal("expected a fail-soft error message")
	}
}

// TestBuildTimeWindowExclusion_NoTimeWindowLeafPassesResidualThrough proves a
// trigger whose conditions carry a bead_query but NO time_window leaf gets an
// evaluated overlay that suppresses nothing (residual == the incoming miss count).
func TestBuildTimeWindowExclusion_NoTimeWindowLeafPassesResidualThrough(t *testing.T) {
	fires := &scheduler.FireReconciliation{
		MissedCount: 2,
		Expected: []scheduler.ExpectedFire{
			{At: mustTime(t, "2026-08-15T00:13:00Z"), Matched: false},
			{At: mustTime(t, "2026-08-15T00:43:00Z"), Matched: false},
		},
	}
	conds := `[{"type":"bead_query","project":"gardenio","labels":["project:gardenio"],"statuses":["open"],"op":">","value":"0"}]`
	tw := buildTimeWindowExclusion(conds, "", fires, nil, nil, outsideQuietHours(t))
	if tw == nil || !tw.Evaluated {
		t.Fatalf("expected an evaluated overlay, got %+v", tw)
	}
	if tw.SuppressedMissed != 0 || tw.ResidualMissed != 2 {
		t.Fatalf("a bead_query-only gate must suppress nothing, got suppressed=%d residual=%d",
			tw.SuppressedMissed, tw.ResidualMissed)
	}
}

// TestBuildTimeWindowExclusion_PendingFireExcludedFromResidual is the overlay-
// chain half of the ka-0000001791 fix. A Pending expected fire (not yet overdue —
// inside its grace period against the window end) is unmatched but must NOT count
// toward the residual the reconciler classifies as drift. This is exactly the
// gardener-bugfix release_owner_live false-DARK: the owner lane matched every
// overdue slot and the only unmatched slot was the fresh boundary fire, so the
// residual — and therefore drift_candidate_missed — must be 0.
func TestBuildTimeWindowExclusion_PendingFireExcludedFromResidual(t *testing.T) {
	fires := &scheduler.FireReconciliation{
		MissedCount:  0, // ReconcileFires excludes the pending slot from MissedCount
		PendingCount: 1,
		Expected: []scheduler.ExpectedFire{
			{At: mustTime(t, "2026-09-01T02:13:00Z"), Matched: true},
			{At: mustTime(t, "2026-09-01T02:43:00Z"), Matched: true},
			{At: mustTime(t, "2026-09-01T03:13:00Z"), Pending: true}, // fresh boundary fire
		},
	}
	conds := `[{"type":"bead_query","project":"gardenio","labels":["project:gardenio"],"statuses":["open"],"op":">","value":"0"}]`
	tw := buildTimeWindowExclusion(conds, "", fires, nil, nil, outsideQuietHours(t))
	if tw == nil || !tw.Evaluated {
		t.Fatalf("expected an evaluated overlay, got %+v", tw)
	}
	if tw.SuppressedMissed != 0 || tw.ResidualMissed != 0 {
		t.Fatalf("a pending boundary fire must not be a residual miss, got suppressed=%d residual=%d",
			tw.SuppressedMissed, tw.ResidualMissed)
	}

	// End-to-end: the composed drift candidate must also be 0 (the false-DARK the
	// probe read as a residual miss is now correctly no verdict yet).
	r := &reconcileReport{Fires: fires, TimeWindow: tw}
	applyDormancyAndGateToResidual(r)
	if r.DriftCandidateMissed != 0 {
		t.Fatalf("drift_candidate_missed: want 0 (only unmatched slot is pending), got %d", r.DriftCandidateMissed)
	}
}

// TestPauseAndLeaseResidual_ExcludePendingFire pins the same exclusion in the
// pause and lease overlays' residual denominators — a Pending slot must not
// inflate residual_missed in any link of the chain (ka-0000001791).
func TestPauseAndLeaseResidual_ExcludePendingFire(t *testing.T) {
	fires := &scheduler.FireReconciliation{
		MissedCount:  0,
		PendingCount: 1,
		Expected: []scheduler.ExpectedFire{
			{At: mustTime(t, "2026-09-01T02:43:00Z"), Matched: true},
			{At: mustTime(t, "2026-09-01T03:13:00Z"), Pending: true},
		},
	}
	if n := pauseResidualMissCount(fires, nil); n != 0 {
		t.Fatalf("pauseResidualMissCount must skip the pending slot, got %d", n)
	}
	if n := residualMissesBefore(fires, nil, nil); n != 0 {
		t.Fatalf("residualMissesBefore must skip the pending slot, got %d", n)
	}
}

// outsideQuietHours is a fixed instant OUTSIDE the release quiet-hours window
// (Friday 2026-08-14 22:00 PT), used as the reconcile clock by the overlay tests
// that are about historical attribution rather than live suppression. Pinning it
// keeps those tests hermetic: with a wall-clock `now` the forward-looking
// suppression probe would report SuppressedNow=true or false depending on what
// time of day CI happens to run.
func outsideQuietHours(t *testing.T) time.Time {
	t.Helper()
	return mustTime(t, "2026-08-15T05:00:00Z")
}

// TestTimeWindowOverlay_QuietHoursSuppressedOwnerIsNotLive is the regression for
// bug ka-0000008105: the gardener-bugfix Step-3 release-owner liveness probe
// scored a quiet-hours-SUPPRESSED owner as live (`enabled==true and
// residual_missed==0`) and therefore skipped dispatching a P0 crew onto a
// release-blocker bug for the whole ~11h PT-business-hours suppression window.
//
// The measured cycle (2026-09-02T14:29Z = Wednesday 07:29 PT, ~29 minutes into
// the hold) is reproduced exactly. The assertions pin BOTH halves of the fix:
//
//   - residual_missed STAYS 0 — the quiet-hours misses are still correctly
//     explained, so the ka-0000005666 correction is not reverted; and
//   - suppressed_now is TRUE with a ~631-minute remaining hold, so a consumer can
//     distinguish "healthy AND about to fire" from "healthy AND dark for another
//     ten hours" — the dimension no threshold on residual_missed can supply.
func TestTimeWindowOverlay_QuietHoursSuppressedOwnerIsNotLive(t *testing.T) {
	// Wednesday 2026-09-02 07:29 PT — 29 minutes into the weekday 07:00-18:00 PT
	// quiet-hours hold, exactly the instant the bug was measured at.
	now := mustTime(t, "2026-09-02T14:29:00Z")

	// One missed cron slot inside the hold, so the historical overlay has
	// something to attribute (matching the capture's suppressed_missed=1).
	fires := &scheduler.FireReconciliation{
		Expected: []scheduler.ExpectedFire{
			{At: mustTime(t, "2026-09-02T14:13:00Z")}, // 07:13 PT — inside quiet hours, missed
		},
	}

	tw := buildTimeWindowExclusion(quietHoursConditions, "", fires, nil, nil, now)
	if tw == nil || !tw.Evaluated {
		t.Fatalf("expected an evaluated time_window overlay, got %+v", tw)
	}

	// Preserve the ka-0000005666 behavior: the hold explains the whole deficit.
	if tw.SuppressedMissed != 1 {
		t.Errorf("suppressed_missed: want 1 (the 07:13 PT slot fell inside the hold), got %d", tw.SuppressedMissed)
	}
	if tw.ResidualMissed != 0 {
		t.Errorf("residual_missed: want 0 (quiet hours explains every miss — do NOT revert ka-0000005666), got %d", tw.ResidualMissed)
	}

	// The new dimension: the owner is healthy AND gated shut right now.
	if !tw.SuppressedNow {
		t.Fatal("suppressed_now: want true — 07:29 PT Wednesday is inside the 07:00-18:00 Mon-Fri PT hold; " +
			"scoring this owner as live is bug ka-0000008105 (a release-blocker P0 went un-crewed for ~11h)")
	}
	// 07:29 PT → 18:00 PT is 10h31m; the gate reopens at the first minute the
	// window no longer covers, i.e. 18:00 PT = 2026-09-03T01:00Z.
	if got, want := tw.SuppressedMinutesRemaining, 631; got != want {
		t.Errorf("suppressed_minutes_remaining: want %d (07:29 PT → 18:00 PT), got %d", want, got)
	}
	if got, want := tw.SuppressedUntil, "2026-09-03T01:00:00Z"; got != want {
		t.Errorf("suppressed_until: want %s (18:00 PT, when the hold lifts), got %s", want, got)
	}

	// A ~60m dispatch horizon must classify this owner as NOT covering, which is
	// the whole point of the fix — the predicate the gardener prompt now applies.
	const dispatchHorizonMinutes = 60
	if tw.SuppressedNow && tw.SuppressedMinutesRemaining <= dispatchHorizonMinutes {
		t.Errorf("a %dm horizon must NOT score this owner as about-to-fire (remaining=%dm)",
			dispatchHorizonMinutes, tw.SuppressedMinutesRemaining)
	}
}

// TestTimeWindowOverlay_OwnerOutsideQuietHoursIsNotSuppressed is acceptance (2)
// of ka-0000008105: the behavior to PRESERVE. An owner reconciled while its
// quiet-hours window is open is firing normally, so suppressed_now must be false
// and the fix must not push the lane toward a duplicate dispatch.
func TestTimeWindowOverlay_OwnerOutsideQuietHoursIsNotSuppressed(t *testing.T) {
	// Wednesday 2026-09-02 20:00 PT — two hours past the 18:00 PT reopen.
	now := mustTime(t, "2026-09-03T03:00:00Z")

	fires := &scheduler.FireReconciliation{
		Expected: []scheduler.ExpectedFire{
			{At: mustTime(t, "2026-09-02T14:13:00Z")}, // 07:13 PT — inside the earlier hold
		},
	}

	tw := buildTimeWindowExclusion(quietHoursConditions, "", fires, nil, nil, now)
	if tw == nil || !tw.Evaluated {
		t.Fatalf("expected an evaluated time_window overlay, got %+v", tw)
	}
	if tw.SuppressedNow {
		t.Errorf("suppressed_now: want false at 20:00 PT (outside the 07:00-18:00 hold), got true — "+
			"this would make the safety-net lane dispatch a duplicate crew (until=%q)", tw.SuppressedUntil)
	}
	if tw.SuppressedUntil != "" || tw.SuppressedMinutesRemaining != 0 {
		t.Errorf("an unsuppressed trigger must carry no hold: until=%q remaining=%d",
			tw.SuppressedUntil, tw.SuppressedMinutesRemaining)
	}
	// The historical attribution is unchanged by the live probe.
	if tw.ResidualMissed != 0 {
		t.Errorf("residual_missed: want 0, got %d", tw.ResidualMissed)
	}
}
