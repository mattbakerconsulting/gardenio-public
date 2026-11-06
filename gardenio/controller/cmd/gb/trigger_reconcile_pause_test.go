// Tests for the operator-pause overlay of `gb trigger reconcile`
// (trigger_reconcile_pause.go).
//
// The centerpiece is TestTriggerReconcile_SchedulerPauseExplainsMisses — the
// regression for the false positive ka-0000004707 drove (ka-0000001687): a
// healthy `*/30` schedule gardener whose entire missed-fire deficit was a
// fleet-wide scheduler-pause maintenance window. Before the fix the reconciler
// read that as habitual gardener-logic drift; after it, residual_missed==0 with
// suppressed_missed>0 deterministically says "an operator pause explains the
// whole deficit → IN SYNC".
package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// TestTriggerReconcile_SchedulerPauseExplainsMisses reconstructs the shape of
// ka-0000004707: a `*/30` monorepo schedule trigger fires normally, then a
// fleet-wide scheduler-pause gate suppresses every schedule-source cron for a
// multi-hour window. Every missed slot lands inside that pause, so the overlay
// must attribute all of them to the pause and leave residual_missed==0.
func TestTriggerReconcile_SchedulerPauseExplainsMisses(t *testing.T) {
	// Fixed clock so the window/pause arithmetic is deterministic.
	now := mustTime(t, "2026-08-11T00:00:00Z")

	// A `*/30` trigger created well before the window; it fired every 30m up to
	// the pause, then went dark for the whole pause, then resumed. We provide
	// observed fires ONLY before the pause and after it — the pause interior is
	// entirely missing, exactly like the real trigger.
	pauseStart := mustTime(t, "2026-08-10T12:00:00Z")
	pauseEnd := mustTime(t, "2026-08-10T18:00:00Z")

	var children []reconcileBead
	// Pre-pause fires: 06:00 → 12:00 every 30m.
	for tt := mustTime(t, "2026-08-09T23:00:00Z"); !tt.After(pauseStart); tt = tt.Add(30 * time.Minute) {
		children = append(children, reconcileBead{
			ID: "gd-pre-" + tt.Format("1504"), Type: "task",
			Fields: map[string]string{"schedule_fire_time": tt.Format(time.RFC3339)},
		})
	}
	// Post-pause fires: 18:00 → 24:00 every 30m.
	for tt := pauseEnd; !tt.After(now); tt = tt.Add(30 * time.Minute) {
		children = append(children, reconcileBead{
			ID: "gd-post-" + tt.Format("1504"), Type: "task",
			Fields: map[string]string{"schedule_fire_time": tt.Format(time.RFC3339)},
		})
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigWEDGE", Type: "trigger", Status: "open", Title: "monorepo wedge watch",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "*/30 * * * *",
				"enabled":     "true",
				"wake_intent": "every 30 minutes check the merge train",
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
	bead := setupReconcileDaemonBead(t, d, "gd-trigWEDGE")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 24*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Fires == nil {
		t.Fatal("expected a Fires reconciliation")
	}
	// The unconditional diff must STILL report the raw misses (Fires stays
	// deliberately unconditional — the overlay never mutates it).
	if rep.Fires.MissedCount == 0 {
		t.Fatalf("expected the unconditional diff to still report the pause misses, got missed=%d", rep.Fires.MissedCount)
	}
	if rep.Pause == nil {
		t.Fatal("expected a pause overlay")
	}
	if !rep.Pause.Evaluated {
		t.Fatalf("pause overlay should have evaluated, got error %q", rep.Pause.Error)
	}
	if len(rep.Pause.Windows) != 1 || rep.Pause.Windows[0].Title != "scheduler-pause" {
		t.Fatalf("expected one scheduler-pause window, got %+v", rep.Pause.Windows)
	}
	// The load-bearing assertion: every miss is inside the pause → residual 0.
	if rep.Pause.SuppressedMissed != rep.Fires.MissedCount {
		t.Fatalf("all %d misses should be pause-suppressed, got suppressed=%d",
			rep.Fires.MissedCount, rep.Pause.SuppressedMissed)
	}
	if rep.Pause.ResidualMissed != 0 {
		t.Fatalf("residual_missed must be 0 when a pause explains the whole deficit, got %d", rep.Pause.ResidualMissed)
	}
}

// TestTriggerReconcile_ResidualMissOutsidePause proves the overlay does NOT
// over-subtract: a real miss OUTSIDE the pause stays a residual candidate, so a
// genuinely dark trigger is not laundered clean by an unrelated maintenance
// window.
func TestTriggerReconcile_ResidualMissOutsidePause(t *testing.T) {
	now := mustTime(t, "2026-08-11T00:00:00Z")
	pauseStart := mustTime(t, "2026-08-10T12:00:00Z")
	pauseEnd := mustTime(t, "2026-08-10T18:00:00Z")

	// Hourly trigger. Observed fires cover only up to the pause; AFTER the pause
	// closes the trigger is dark (no fires 18:00 → 24:00) — those are genuine
	// residual misses the pause cannot explain.
	var children []reconcileBead
	for tt := mustTime(t, "2026-08-09T23:00:00Z"); !tt.After(pauseStart); tt = tt.Add(time.Hour) {
		children = append(children, reconcileBead{
			ID: "gd-h-" + tt.Format("1504"), Type: "task",
			Fields: map[string]string{"schedule_fire_time": tt.Format(time.RFC3339)},
		})
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigHALF", Type: "trigger", Status: "open", Title: "half-dark hourly",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "0 * * * *",
				"enabled":     "true",
				"wake_intent": "hourly",
			},
		},
		children: children,
		gates: []reconcileBead{{
			ID: "gd-2AWyngPmp4", Type: "gate", Status: "closed", Title: "scheduler-pause",
			CreatedAt: pauseStart.Format(time.RFC3339),
			ClosedAt:  pauseEnd.Format(time.RFC3339),
		}},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigHALF")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 24*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Pause == nil || !rep.Pause.Evaluated {
		t.Fatalf("expected an evaluated pause overlay, got %+v", rep.Pause)
	}
	// The post-pause dark hours (roughly 19:00..24:00) are residual misses.
	if rep.Pause.ResidualMissed == 0 {
		t.Fatalf("post-pause dark hours must remain residual candidate misses, got residual=0 (suppressed=%d, missed=%d)",
			rep.Pause.SuppressedMissed, rep.Fires.MissedCount)
	}
	if rep.Pause.SuppressedMissed == 0 {
		t.Fatalf("the pause-interior misses must still be attributed to the pause, got suppressed=0")
	}
	if rep.Pause.SuppressedMissed+rep.Pause.ResidualMissed != rep.Fires.MissedCount {
		t.Fatalf("suppressed+residual must equal missed: %d+%d != %d",
			rep.Pause.SuppressedMissed, rep.Pause.ResidualMissed, rep.Fires.MissedCount)
	}
}

// TestTriggerReconcile_NoPauseHealthy is the healthy steady state: no operator
// pause overlapped the window, so the overlay evaluates with zero windows and
// residual_missed equals the raw missed count (the overlay changes nothing).
func TestTriggerReconcile_NoPauseHealthy(t *testing.T) {
	now := mustTime(t, "2026-08-11T00:00:00Z")
	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigCLEAN", Type: "trigger", Status: "open", Title: "clean hourly",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "0 * * * *",
				"enabled":     "true",
				"wake_intent": "hourly",
			},
		},
		// no children → all missed, but NO pause gate seeded
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigCLEAN")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 6*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Pause == nil || !rep.Pause.Evaluated {
		t.Fatalf("expected an evaluated pause overlay, got %+v", rep.Pause)
	}
	if len(rep.Pause.Windows) != 0 {
		t.Fatalf("no pause was seeded — expected zero windows, got %+v", rep.Pause.Windows)
	}
	if rep.Pause.SuppressedMissed != 0 {
		t.Fatalf("no pause → suppressed_missed must be 0, got %d", rep.Pause.SuppressedMissed)
	}
	if rep.Pause.ResidualMissed != rep.Fires.MissedCount {
		t.Fatalf("no pause → residual_missed must equal missed (%d), got %d", rep.Fires.MissedCount, rep.Pause.ResidualMissed)
	}
}

// TestTriggerReconcile_SchedulerPauseIgnoredForNonScheduleTrigger proves the
// overlay routes the gate the same way the engine does: a scheduler-pause does
// NOT suppress an EVENT-sourced trigger's cron misses, so it must not be
// subtracted. (Only triggers-pause is global; scheduler-pause is
// schedule-flavored-only.) Here a non-schedule-labeled trigger with a cron sees
// a scheduler-pause window but must report it as inapplicable.
func TestTriggerReconcile_SchedulerPauseIgnoredForNonScheduleTrigger(t *testing.T) {
	now := mustTime(t, "2026-08-11T00:00:00Z")
	pauseStart := mustTime(t, "2026-08-10T12:00:00Z")
	pauseEnd := mustTime(t, "2026-08-10T18:00:00Z")

	d := &reconcileDaemon{
		trigger: reconcileBead{
			// A multi-source trigger (label trigger:multi, NOT trigger:schedule):
			// scheduler-pause does not gate it, so its cron misses are not excused.
			ID: "gd-trigMULTI", Type: "trigger", Status: "open", Title: "multi gardener",
			Labels:    []string{"project:monorepo", "trigger:multi"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "0 * * * *",
				"enabled":     "true",
				"wake_intent": "hourly plus events",
			},
		},
		gates: []reconcileBead{{
			ID: "gd-dU9OUb7cKu", Type: "gate", Status: "closed", Title: "scheduler-pause",
			CreatedAt: pauseStart.Format(time.RFC3339),
			ClosedAt:  pauseEnd.Format(time.RFC3339),
		}},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigMULTI")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 24*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Pause == nil || !rep.Pause.Evaluated {
		t.Fatalf("expected an evaluated pause overlay, got %+v", rep.Pause)
	}
	// scheduler-pause is not applicable to a non-schedule trigger → no windows.
	if len(rep.Pause.Windows) != 0 {
		t.Fatalf("scheduler-pause must not apply to a non-schedule trigger, got %+v", rep.Pause.Windows)
	}
	if rep.Pause.ResidualMissed != rep.Fires.MissedCount {
		t.Fatalf("no applicable pause → residual must equal missed (%d), got %d",
			rep.Fires.MissedCount, rep.Pause.ResidualMissed)
	}
}

// TestTriggerReconcile_GardenersPauseAppliesToGardenerTrigger proves gardeners-pause
// IS subtracted for a gardener-shaped trigger (a non-empty `gardener` field),
// matching gardenerPreFireCheck's fleet-wide halt.
func TestTriggerReconcile_GardenersPauseAppliesToGardenerTrigger(t *testing.T) {
	now := mustTime(t, "2026-08-11T00:00:00Z")
	pauseStart := mustTime(t, "2026-08-10T12:00:00Z")
	pauseEnd := mustTime(t, "2026-08-10T18:00:00Z")

	// Hourly gardener, dark during the pause; observed only before it.
	var children []reconcileBead
	for tt := mustTime(t, "2026-08-09T23:00:00Z"); !tt.After(pauseStart); tt = tt.Add(time.Hour) {
		children = append(children, reconcileBead{
			ID: "gd-cap-" + tt.Format("1504"), Type: "task",
			Fields: map[string]string{"schedule_fire_time": tt.Format(time.RFC3339)},
		})
	}
	// Resume after the pause so the ONLY misses are the pause interior.
	for tt := pauseEnd; !tt.After(now); tt = tt.Add(time.Hour) {
		children = append(children, reconcileBead{
			ID: "gd-capA-" + tt.Format("1504"), Type: "task",
			Fields: map[string]string{"schedule_fire_time": tt.Format(time.RFC3339)},
		})
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigCAP", Type: "trigger", Status: "open", Title: "a gardener",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "0 * * * *",
				"enabled":     "true",
				"wake_intent": "hourly gardener",
				"gardener":    `{"run_budget":60}`,
			},
		},
		children: children,
		gates: []reconcileBead{{
			ID: "gd-gateCAP", Type: "gate", Status: "closed", Title: "gardeners-pause",
			CreatedAt: pauseStart.Format(time.RFC3339),
			ClosedAt:  pauseEnd.Format(time.RFC3339),
		}},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigCAP")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 24*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Pause == nil || !rep.Pause.Evaluated {
		t.Fatalf("expected an evaluated pause overlay, got %+v", rep.Pause)
	}
	if len(rep.Pause.Windows) != 1 || rep.Pause.Windows[0].Title != "gardeners-pause" {
		t.Fatalf("expected one gardeners-pause window applied to the gardener, got %+v", rep.Pause.Windows)
	}
	if rep.Pause.ResidualMissed != 0 {
		t.Fatalf("gardeners-pause should explain the whole gardener deficit, got residual=%d", rep.Pause.ResidualMissed)
	}
}

// TestTriggerReconcile_OpenPauseWindowUsesNow proves an open (not-yet-closed)
// pause gate is treated as spanning [created_at, now], flagged Open — so a pause
// still in effect at reconcile time excuses the misses since it opened.
func TestTriggerReconcile_OpenPauseWindowUsesNow(t *testing.T) {
	now := mustTime(t, "2026-08-11T00:00:00Z")
	pauseStart := mustTime(t, "2026-08-10T20:00:00Z")

	// Hourly trigger; fires up to the pause open, then dark to now.
	var children []reconcileBead
	for tt := mustTime(t, "2026-08-10T11:00:00Z"); !tt.After(pauseStart); tt = tt.Add(time.Hour) {
		children = append(children, reconcileBead{
			ID: "gd-o-" + tt.Format("1504"), Type: "task",
			Fields: map[string]string{"schedule_fire_time": tt.Format(time.RFC3339)},
		})
	}

	d := &reconcileDaemon{
		trigger: reconcileBead{
			ID: "gd-trigOPEN", Type: "trigger", Status: "open", Title: "hourly under open pause",
			Labels:    []string{"project:monorepo", "trigger:schedule"},
			CreatedAt: "2026-08-01T00:00:00Z",
			Fields: map[string]string{
				"cron":        "0 * * * *",
				"enabled":     "true",
				"wake_intent": "hourly",
			},
		},
		children: children,
		gates: []reconcileBead{{
			// still OPEN — no closed_at.
			ID: "gd-gateOPEN", Type: "gate", Status: "open", Title: "scheduler-pause",
			CreatedAt: pauseStart.Format(time.RFC3339),
		}},
	}
	bead := setupReconcileDaemonBead(t, d, "gd-trigOPEN")

	oldWindow, oldTol := reconcileWindow, reconcileTolerance
	reconcileWindow, reconcileTolerance = 12*time.Hour, scheduler.DefaultReconcileTolerance
	t.Cleanup(func() { reconcileWindow, reconcileTolerance = oldWindow, oldTol })

	rep := buildReconcileReport(t.Context(), bead, now)
	if rep.Pause == nil || !rep.Pause.Evaluated {
		t.Fatalf("expected an evaluated pause overlay, got %+v", rep.Pause)
	}
	if len(rep.Pause.Windows) != 1 || !rep.Pause.Windows[0].Open {
		t.Fatalf("expected one open pause window flagged Open, got %+v", rep.Pause.Windows)
	}
	if !rep.Pause.Windows[0].End.Equal(now) {
		t.Fatalf("open pause window End must be `now` (%s), got %s", now, rep.Pause.Windows[0].End)
	}
	if rep.Pause.ResidualMissed != 0 {
		t.Fatalf("an open pause covering all post-open misses should leave residual=0, got %d", rep.Pause.ResidualMissed)
	}
}

// errPauseClient is a pauseGateClient whose gate scan always fails, so the
// overlay must degrade to Evaluated=false.
type errPauseClient struct{}

func (errPauseClient) ListBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return nil, errors.New("boom")
}

// TestBuildPauseExclusion_FailSoftOnDaemonError proves a gate-load error yields
// an Evaluated=false overlay (never a nil-with-signal, never a mutation of the
// raw fires diff) so a daemon hiccup can't manufacture — or erase — a signal.
func TestBuildPauseExclusion_FailSoftOnDaemonError(t *testing.T) {
	fires := &scheduler.FireReconciliation{MissedCount: 5}
	bead := &beadsapi.BeadDetail{ID: "gd-x", Type: "trigger", Labels: []string{"trigger:schedule"}}
	pe := buildPauseExclusion(t.Context(), errPauseClient{}, bead, fires,
		mustTime(t, "2026-08-10T00:00:00Z"), mustTime(t, "2026-08-11T00:00:00Z"))
	if pe == nil {
		t.Fatal("expected a non-nil overlay carrying the fail-soft reason")
	}
	if pe.Evaluated {
		t.Fatal("a daemon error must leave the overlay Evaluated=false")
	}
	if pe.Error == "" {
		t.Fatal("expected a fail-soft error reason")
	}
	if fires.MissedCount != 5 {
		t.Fatalf("the raw fires diff must be untouched, got missed=%d", fires.MissedCount)
	}
}

// TestBuildPauseExclusion_NilFiresOmitsOverlay proves an event-only / collapsed
// trigger (no fires diff) gets no overlay at all, so the report omits it.
func TestBuildPauseExclusion_NilFiresOmitsOverlay(t *testing.T) {
	bead := &beadsapi.BeadDetail{ID: "gd-x", Type: "trigger", Labels: []string{"trigger:schedule"}}
	if pe := buildPauseExclusion(t.Context(), errPauseClient{}, bead, nil,
		mustTime(t, "2026-08-10T00:00:00Z"), mustTime(t, "2026-08-11T00:00:00Z")); pe != nil {
		t.Fatalf("nil fires must yield a nil overlay, got %+v", pe)
	}
}
