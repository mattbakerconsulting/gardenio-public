// Tests for the max_concurrent lease overlay of `gb trigger reconcile`
// (trigger_reconcile_lease.go).
//
// The centerpiece is TestTriggerReconcile_LeaseExplainsResidualMiss — the
// regression for the false positive ka-0000007902 drove (3rd recurrence on the
// monorepo wedge-watch ka-0000002184): a healthy `*/30` max_concurrent=1 trigger
// whose residual missed slot was the concurrency guard deferring a fire while a
// prior long-running fire's agent still held the single lease. Before the fix
// the reconciler read that residual as unexplained habitual drift; after it,
// lease.suppressed_missed>0 with lease.residual_missed==0 deterministically says
// "the max_concurrent lease deferred it by design → IN SYNC".
package main

import (
	"testing"
	"time"

	"gardenio/controller/internal/scheduler"
)

// TestTriggerReconcile_LeaseExplainsResidualMiss reconstructs the exact shape of
// ka-0000007902. A `*/30` trigger with max_concurrent=1 fires every 30m, EXCEPT
// the 02:00 slot: the 01:30 fire's spawned agent ran long (created 01:30, closed
// 02:37), holding the single lease across 02:00, so the engine deferred that
// slot and it fired late at 02:30 (observed at the 02:30 mark). The lease overlay
// must attribute that residual miss to the max_concurrent lease and leave
// residual_missed==0.
func TestTriggerReconcile_LeaseExplainsResidualMiss(t *testing.T) {
	// Window spans exactly 01:00 → 03:30 (start clamps to now-window), so the
	// ONLY missed slot is 02:00 — the lease-deferred one.
	now := mustTime(t, "2026-08-09T03:31:00Z")

	// Observed fires: every 30m from 01:00 to 03:30, but NOTHING at 02:00 — the
	// 02:00 slot was lease-deferred and its work landed at 02:30.
	fireTimes := []string{
		"2026-08-09T01:00:00Z",
		"2026-08-09T01:30:00Z",
		// 02:00 missing — lease held by the 01:30 agent.
		"2026-08-09T02:30:00Z",
		"2026-08-09T03:00:00Z",
		"2026-08-09T03:30:00Z",
	}
	var children []reconcileBead
	for _, ft := range fireTimes {
		children = append(children, reconcileBead{
			ID: "gd-task-" + ft, Type: "task",
			Fields: map[string]string{"schedule_fire_time": ft},
		})
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigWEDGE", Type: "trigger", Status: "open", Title: "monorepo wedge watch",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":           "*/30 * * * *",
				"enabled":        "true",
				"max_concurrent": "1",
				"wake_intent":    "every 30 minutes check the merge train",
			},
		},
		children: children,
		// The lease holder: the 01:30 fire's agent, alive 01:30 → 02:37, spanning
		// the 02:00 slot.
		agents: []reconcileBead{{
			ID: "gd-agent-0130", Type: "agent", Status: "closed",
			CreatedAt: "2026-08-09T01:30:23Z",
			ClosedAt:  "2026-08-09T02:37:31Z",
			UpdatedAt: "2026-08-09T02:37:34Z",
			Fields:    map[string]string{"spawned_by": "trigger:gd-trigWEDGE"},
		}},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigWEDGE")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 151*time.Minute, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The unconditional diff must STILL report the raw miss (Fires stays
	// deliberately unconditional — the overlay never mutates it).
	if rep.Fires.MissedCount != 1 {
		t.Fatalf("expected the unconditional diff to report the 1 lease-deferred miss, got missed=%d", rep.Fires.MissedCount)
	}
	if rep.Lease == nil {
		t.Fatal("expected a lease overlay")
	}
	if !rep.Lease.Evaluated {
		t.Fatalf("lease overlay should have evaluated, got error %q", rep.Lease.Error)
	}
	if rep.Lease.MaxConcurrent != 1 {
		t.Fatalf("expected max_concurrent=1, got %d", rep.Lease.MaxConcurrent)
	}
	// The load-bearing assertion: the miss is lease-suppressed → residual 0.
	if rep.Lease.SuppressedMissed != 1 {
		t.Fatalf("expected the 02:00 miss to be lease-suppressed, got suppressed=%d", rep.Lease.SuppressedMissed)
	}
	if rep.Lease.ResidualMissed != 0 {
		t.Fatalf("residual_missed must be 0 when the lease explains the deficit, got %d", rep.Lease.ResidualMissed)
	}
	if len(rep.Lease.Windows) != 1 || rep.Lease.Windows[0].AgentID != "gd-agent-0130" {
		t.Fatalf("expected one explaining lease window (gd-agent-0130), got %+v", rep.Lease.Windows)
	}
}

// TestTriggerReconcile_LeaseDoesNotOverSubtract proves the overlay does NOT
// launder a genuinely dark trigger: a miss with NO lease holder covering it stays
// a residual candidate. The 02:00 slot is missed, but the 01:30 agent closed at
// 01:40 — well before 02:00 — so nothing held the lease across the miss.
func TestTriggerReconcile_LeaseDoesNotOverSubtract(t *testing.T) {
	// Window spans exactly 01:00 → 03:30, so the ONLY missed slot is 02:00.
	now := mustTime(t, "2026-08-09T03:31:00Z")

	fireTimes := []string{
		"2026-08-09T01:00:00Z",
		"2026-08-09T01:30:00Z",
		// 02:00 missing — but no agent is live across it.
		"2026-08-09T02:30:00Z",
		"2026-08-09T03:00:00Z",
		"2026-08-09T03:30:00Z",
	}
	var children []reconcileBead
	for _, ft := range fireTimes {
		children = append(children, reconcileBead{
			ID: "gd-task-" + ft, Type: "task",
			Fields: map[string]string{"schedule_fire_time": ft},
		})
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigDARKGAP", Type: "trigger", Status: "open", Title: "wedge watch with real gap",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":           "*/30 * * * *",
				"enabled":        "true",
				"max_concurrent": "1",
				"wake_intent":    "every 30 minutes check the merge train",
			},
		},
		children: children,
		// The 01:30 agent closed at 01:40 — it did NOT span the 02:00 slot.
		agents: []reconcileBead{{
			ID: "gd-agent-0130", Type: "agent", Status: "closed",
			CreatedAt: "2026-08-09T01:30:23Z",
			ClosedAt:  "2026-08-09T01:40:00Z",
			UpdatedAt: "2026-08-09T01:40:00Z",
			Fields:    map[string]string{"spawned_by": "trigger:gd-trigDARKGAP"},
		}},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigDARKGAP")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 151*time.Minute, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Lease == nil || !rep.Lease.Evaluated {
		t.Fatalf("expected an evaluated lease overlay, got %+v", rep.Lease)
	}
	if rep.Lease.SuppressedMissed != 0 {
		t.Fatalf("no holder spans the 02:00 miss — nothing should be lease-suppressed, got %d", rep.Lease.SuppressedMissed)
	}
	if rep.Lease.ResidualMissed != 1 {
		t.Fatalf("the genuine 02:00 gap must stay residual, got residual=%d", rep.Lease.ResidualMissed)
	}
	if len(rep.Lease.Windows) != 0 {
		t.Fatalf("no explaining window expected, got %+v", rep.Lease.Windows)
	}
}

// TestTriggerReconcile_LeaseStalenessCapDoesNotHideDarkLane is the load-bearing
// guard that the overlay cannot hide the dark-lane bug the lease-stale mechanism
// itself exists to surface (ka-0000001095). A single agent bead is created and
// then WEDGES for many hours (updated_at frozen right after creation). The engine
// releases its lease at the 3h staleness bound and fires again — so misses AFTER
// that bound are genuine drift and MUST stay residual, not be credited to the
// stale holder's whole runtime.
func TestTriggerReconcile_LeaseStalenessCapDoesNotHideDarkLane(t *testing.T) {
	now := mustTime(t, "2026-08-09T12:00:00Z")

	// Only ONE observed fire at 01:00; then the agent wedges and the lane goes
	// dark. The engine's 3h lease-stale bound releases the wedged holder at ~04:00,
	// so slots from 04:30 onward are genuine dark-lane misses.
	children := []reconcileBead{{
		ID: "gd-task-0100", Type: "task",
		Fields: map[string]string{"schedule_fire_time": "2026-08-09T01:00:00Z"},
	}}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-w67xcwGETk", Type: "trigger", Status: "open", Title: "wedged wedge watch",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":           "*/30 * * * *",
				"enabled":        "true",
				"max_concurrent": "1",
				"wake_intent":    "every 30 minutes check the merge train",
			},
		},
		children: children,
		// A wedged agent: created 01:00, updated_at frozen at 01:01 (quiet), never
		// closed. Its quiet-staleness release is 01:01 + 3h = 04:01. It cannot
		// credit lease-suppression past that bound.
		agents: []reconcileBead{{
			ID: "gd-agent-wedged", Type: "agent", Status: "in_progress",
			CreatedAt: "2026-08-09T01:00:10Z",
			UpdatedAt: "2026-08-09T01:01:00Z",
			Fields:    map[string]string{"spawned_by": "trigger:gd-w67xcwGETk"},
		}},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-w67xcwGETk")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 14*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Lease == nil || !rep.Lease.Evaluated {
		t.Fatalf("expected an evaluated lease overlay, got %+v", rep.Lease)
	}
	// There are many missed slots (01:30 → 12:00). Only those inside the wedged
	// holder's [01:00, 04:01] lease window may be credited; the many misses after
	// 04:01 MUST remain residual — the overlay must not launder the dark lane.
	if rep.Lease.ResidualMissed == 0 {
		t.Fatal("the post-staleness dark-lane misses must stay residual — the overlay must not credit a wedged holder past its 3h release bound")
	}
	// Sanity: the window end was capped at the quiet-release bound (~04:01), not
	// extended to `now`.
	for _, w := range rep.Lease.Windows {
		if w.AgentID == "gd-agent-wedged" {
			cap := mustTime(t, "2026-08-09T04:01:00Z")
			if w.End.After(cap.Add(time.Minute)) {
				t.Fatalf("wedged holder's lease window must be capped at the staleness bound ~04:01, got End=%s", w.End.Format(time.RFC3339))
			}
		}
	}
}

// TestTriggerReconcile_LeaseChainsAfterPause proves the two overlays chain: a
// miss already explained by an operator pause is NOT double-counted by the lease
// overlay. Here every miss is inside a scheduler-pause, so the lease overlay sees
// zero pause-residual misses and reports suppressed=0 / residual=0 without
// touching the pause-attributed slots.
func TestTriggerReconcile_LeaseChainsAfterPause(t *testing.T) {
	now := mustTime(t, "2026-08-11T00:00:00Z")
	pauseStart := mustTime(t, "2026-08-10T12:00:00Z")
	pauseEnd := mustTime(t, "2026-08-10T18:00:00Z")

	var children []reconcileBead
	for tt := mustTime(t, "2026-08-09T23:00:00Z"); !tt.After(pauseStart); tt = tt.Add(30 * time.Minute) {
		children = append(children, reconcileBead{
			ID: "gd-pre-" + tt.Format("1504"), Type: "task",
			Fields: map[string]string{"schedule_fire_time": tt.Format(time.RFC3339)},
		})
	}
	for tt := pauseEnd; !tt.After(now); tt = tt.Add(30 * time.Minute) {
		children = append(children, reconcileBead{
			ID: "gd-post-" + tt.Format("1504"), Type: "task",
			Fields: map[string]string{"schedule_fire_time": tt.Format(time.RFC3339)},
		})
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-FTTkBdZ4uB", Type: "trigger", Status: "open", Title: "paused wedge watch",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":           "*/30 * * * *",
				"enabled":        "true",
				"max_concurrent": "1",
				"wake_intent":    "every 30 minutes check the merge train",
			},
		},
		children: children,
		gates: []reconcileBead{{
			ID: "gd-gateSCHED", Type: "gate", Status: "closed", Title: "scheduler-pause",
			CreatedAt: pauseStart.Format(time.RFC3339),
			ClosedAt:  pauseEnd.Format(time.RFC3339),
			UpdatedAt: pauseEnd.Format(time.RFC3339),
		}},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-FTTkBdZ4uB")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 24*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Pause == nil || rep.Pause.ResidualMissed != 0 {
		t.Fatalf("expected the pause overlay to explain the whole deficit, got %+v", rep.Pause)
	}
	if rep.Lease == nil || !rep.Lease.Evaluated {
		t.Fatalf("expected an evaluated lease overlay, got %+v", rep.Lease)
	}
	// Every miss is pause-attributed, so the lease overlay has nothing to
	// double-count: suppressed=0, residual=0.
	if rep.Lease.SuppressedMissed != 0 {
		t.Fatalf("pause-attributed misses must not be lease-double-counted, got suppressed=%d", rep.Lease.SuppressedMissed)
	}
	if rep.Lease.ResidualMissed != 0 {
		t.Fatalf("no pause-residual misses remain, so lease residual must be 0, got %d", rep.Lease.ResidualMissed)
	}
}

// TestTriggerReconcile_LeaseNoHoldersReportsResidual confirms that when a trigger
// has no spawned agents at all, the overlay reports every pause-residual miss as
// residual (no lease can explain anything) rather than erroring or claiming
// suppression.
func TestTriggerReconcile_LeaseNoHoldersReportsResidual(t *testing.T) {
	now := mustTime(t, "2026-08-09T04:00:00Z")

	// A dark hourly trigger with one early fire and then nothing.
	children := []reconcileBead{{
		ID: "gd-task-0000", Type: "task",
		Fields: map[string]string{"schedule_fire_time": "2026-08-09T00:00:00Z"},
	}}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigNOAGENTS", Type: "trigger", Status: "open", Title: "no-agent hourly",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":           "0 * * * *",
				"enabled":        "true",
				"max_concurrent": "1",
				"wake_intent":    "hourly sweep",
			},
		},
		children: children,
		// no agents seeded
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigNOAGENTS")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 6*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Lease == nil || !rep.Lease.Evaluated {
		t.Fatalf("expected an evaluated lease overlay, got %+v", rep.Lease)
	}
	if rep.Lease.SuppressedMissed != 0 {
		t.Fatalf("no holders → nothing suppressed, got %d", rep.Lease.SuppressedMissed)
	}
	if rep.Fires == nil || rep.Lease.ResidualMissed != rep.Fires.MissedCount {
		t.Fatalf("with no holders every miss is residual, got residual=%d missed=%d",
			rep.Lease.ResidualMissed, rep.Fires.MissedCount)
	}
}
