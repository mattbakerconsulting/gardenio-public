// Package main — `gb trigger reconcile` subcommand.
//
// This is the deterministic, model-free half of the wake-intent audit (Slack
// thread ka-0000007909). Given a scheduled trigger, it reconstructs the
// trigger's OBSERVED fire history from durable bead state (children's
// schedule_fire_time + last_run) and diffs it against the UNCONDITIONAL cron
// schedule via scheduler.ReconcileFires, emitting a JSON report of
// expected/matched/missed/extra fires.
//
// The periodic wake-intent reconciler (a haiku agent on its OWN cron —
// watcher != watched) calls this so it never guesses timestamps by hand:
// timestamp arithmetic is deterministic and belongs in code. The model's job is
// the NL-intent -> config judgment (does wake_intent match cron/conditions?);
// THIS command is the ground-truth fire diff it reads to make that call and to
// classify a drift as fixable / impossible / habitual / over-firing.
package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"

	"github.com/spf13/cobra"
)

var (
	reconcileWindow    time.Duration
	reconcileTolerance time.Duration
	reconcileChildLim  int
	reconcileAll       bool
	reconcileRosterLim int
)

var triggerReconcileCmd = &cobra.Command{
	Use:   "reconcile [trigger-id]",
	Short: "Diff a trigger's cron schedule against its observed fire history (wake-intent audit)",
	Long: `Reconstruct a scheduled trigger's OBSERVED fire history from durable bead
state (children's schedule_fire_time + last_run) and diff it against the
UNCONDITIONAL cron schedule. Emits a JSON report of expected/matched/missed/
extra fires plus the trigger's wake_intent and mechanical config.

This is the deterministic input to the wake-intent reconciler: it does the
timestamp arithmetic so the auditing agent only has to judge whether the
natural-language wake_intent matches the mechanism, and classify any drift.

The report deliberately computes the UNCONDITIONAL cron schedule — it does not
subtract preflight conditions, cooldown, blocked_by, or enabled=false. A
"missed" fire is therefore a CANDIDATE miss (it may have been legitimately
gated), which the reconciler cross-references against wake_intent + the gate
config to classify.

With --all, reconcile the whole fleet in ONE call: every open type=trigger bead
that declares a wake_intent is surveyed and its per-trigger report collected
into a single JSON envelope. This is the cheap survey primitive the periodic
wake-intent reconciler (a haiku agent) runs each pass — one server round-trip
instead of a reconcile-per-trigger fan-out (advice ka-0000002706). Triggers
with no wake_intent are skipped: they have no declared intent to audit and fall
back to the mechanical liveness sweep alone.

Examples:
  gb trigger reconcile ka-trigABCDE --window 168h --json
  gb trigger reconcile --all --window 168h --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTriggerReconcile,
}

// reconcileReport is the JSON contract emitted to the wake-intent reconciler.
// Field names are stable and self-describing so the agent prompt can reference
// them directly.
type reconcileReport struct {
	TriggerID  string `json:"trigger_id"`
	Title      string `json:"title"`
	Enabled    bool   `json:"enabled"`
	WakeIntent string `json:"wake_intent,omitempty"`

	// Mechanism — the config the reconciler compares wake_intent against.
	Cron            string   `json:"cron,omitempty"`
	Timezone        string   `json:"timezone,omitempty"`
	Conditions      string   `json:"conditions,omitempty"`
	CooldownMinutes string   `json:"cooldown_minutes,omitempty"`
	BlockedBy       []string `json:"blocked_by,omitempty"`

	// WindowStartEffective is the actual lower bound used for expected-fire
	// enumeration, after clamping the plain lookback start (now-window) up to
	// the trigger's creation time. It equals now-window for a trigger older than
	// the window, and the creation time for one created inside it. Surfaced so
	// the reconciler can see WHY an expected count is lower than a naive
	// window/period estimate — the pre-creation slots were correctly excluded,
	// not lost.
	WindowStartEffective time.Time `json:"window_start_effective,omitzero"`
	// WindowClampedToCreation is true when WindowStartEffective was raised to the
	// trigger's creation time (i.e. the trigger is younger than the lookback
	// window). A clamped window means any "missed" count reflects only fires the
	// trigger could actually have made; pre-creation cron slots are excluded.
	WindowClampedToCreation bool `json:"window_clamped_to_creation,omitempty"`

	// WindowEndEffective is the actual upper bound used for expected-fire
	// enumeration, after clamping the plain window end (now) DOWN to a DISABLED
	// trigger's last live evaluation (last_checked). It equals now for an enabled
	// trigger, and the disable instant for one that is currently disabled.
	// Surfaced as the trailing-edge twin of WindowStartEffective so the
	// reconciler can see WHY a post-disable cron slot was excluded, not lost.
	WindowEndEffective time.Time `json:"window_end_effective,omitzero"`
	// WindowClampedToDisable is true when WindowEndEffective was lowered to a
	// disabled trigger's last live check. A clamped end means any "missed" count
	// reflects only fires the trigger could have made WHILE STILL LIVE;
	// post-disable cron slots are excluded. This is the exact mirror of
	// WindowClampedToCreation: a trigger cannot miss a fire scheduled after it
	// stopped being live any more than one scheduled before it existed.
	WindowClampedToDisable bool `json:"window_clamped_to_disable,omitempty"`

	// ObservedFireCount is how many distinct observed fires were reconstructed
	// in the window; ObservedSource notes where they came from for auditability.
	ObservedFireCount int `json:"observed_fire_count"`

	// Fires is the deterministic expected-vs-observed diff. Nil when the trigger
	// has no cron (event-only trigger) — Note explains why.
	Fires *scheduler.FireReconciliation `json:"fires,omitempty"`
	Note  string                        `json:"note,omitempty"`

	// Gate is a live snapshot of the trigger's pre-flight conditions evaluated
	// against the CURRENT bead store (would_fire_now + per-leaf raw-vs-effective
	// counts). Nil when the trigger has no conditions field. It is the
	// deterministic answer to "is a reported miss a legitimate gated no-fire?" —
	// the exact judgment the reconciler agent previously had to make by eyeballing
	// the raw conditions JSON, and got wrong on ka-0000000876 / ka-0000005279.
	// Explicitly NOT folded into Fires: ReconcileFires stays unconditional by
	// design (docs/wake-intent-reconciler.md), and this is a point-in-time gate
	// snapshot, not a historical fire count.
	Gate *gateEval `json:"gate,omitempty"`

	// Pause is the operator-pause overlay: the fleet-wide triggers-pause /
	// scheduler-pause / gardeners-pause gate windows that overlapped the audit
	// window and suppressed THIS trigger's fire path by design, plus the split of
	// Fires.Missed into pause-suppressed vs residual candidate misses. Nil when
	// the trigger has no fires diff. Like Gate it is explicitly NOT folded into
	// Fires (which stays unconditional); unlike Gate it attributes HISTORICAL
	// wall-clock intervals, so residual_missed==0 with suppressed_missed>0 is the
	// deterministic "an operator pause explains the whole deficit → IN SYNC"
	// signal that killed the false-positive ka-0000004707 drove (ka-0000001687).
	Pause *pauseExclusion `json:"pause,omitempty"`

	// Lease is the max_concurrent lease overlay: the spawned-agent lease-held
	// intervals that deferred a missed fire by design, plus the split of the
	// pause-RESIDUAL misses into lease-suppressed vs genuine drift. Nil when the
	// trigger has no fires diff. Chained onto Pause (a miss already inside a pause
	// window is attributed there, never double-counted), so lease.residual_missed
	// is the SINGLE count of misses explained by NEITHER an operator pause NOR the
	// concurrency lease — the final genuine-drift candidate. Like Gate and Pause
	// it is explicitly NOT folded into Fires (which stays unconditional). This is
	// the deterministic answer to "was this run of misses the max_concurrent lease
	// deferring a slot while a prior long fire was still live?" — the judgment the
	// reconciler got wrong on ka-0000007902 (3rd recurrence on the wedge-watch).
	Lease *leaseExclusion `json:"lease,omitempty"`

	// TimeWindow is the time_window overlay: the trigger's own quiet-hours
	// condition(s) (op="outside" / "inside"), attributing each pause/lease-residual
	// missed slot to the window that suppressed firing at that instant. Nil when the
	// trigger has no fires diff or carries no conditions. Chained onto Pause AND
	// Lease (a miss already attributed to either is skipped, never double-counted),
	// so timeWindow.residual_missed is the SINGLE count of misses explained by NONE
	// of pause / lease / time_window. Like the sibling overlays it is explicitly NOT
	// folded into Fires (which stays unconditional). Unlike the point-in-time Gate
	// (a would_fire_now snapshot whose verdict flips depending on WHEN the
	// reconciler runs relative to the quiet window), this attributes each HISTORICAL
	// slot to the window it actually fell in — the deterministic answer to "was this
	// run of misses the trigger's own quiet-hours gate doing its job?" that killed
	// the habitual re-file loop on the release gardener ka-0000005666.
	TimeWindow *timeWindowExclusion `json:"time_window,omitempty"`

	// DriftCandidateMissed is the SINGLE deterministic drift-candidate miss count
	// the reconciler classifies against — the end of the full overlay chain,
	// composed in code so the model reads ONE number instead of reconciling four
	// overlays by hand. It is the historical pause→lease residual
	// (lease.residual_missed → pause.residual_missed → fires.missed_count, first
	// evaluated wins) with the point-in-time GATE applied as the final link: when
	// gate.would_fire_now == false the pre-flight is deterministically declining
	// now (an empty lane, or one its excludes floor to 0), so the residual is a run
	// of gated no-fires and this drops to 0. When gate.would_fire_now == true (or
	// the gate is absent / un-evaluated) the residual stands — a positive value
	// here with would_fire_now == true is the genuine dark-lane signal. 0 with any
	// overlay explaining the deficit is "IN SYNC, never drift". Folding the gate
	// into this count is what closed the habitual re-file loop the raw gate signal
	// left to model judgment (ka-0000004262, siblings ka-0000005279/ka-0000001961).
	// Explicitly NOT folded into Fires, which stays unconditional (auditable).
	DriftCandidateMissed int `json:"drift_candidate_missed"`
	// GateExplainsResidual is true when the point-in-time gate (would_fire_now ==
	// false) is what dropped a POSITIVE historical residual to a 0 drift candidate
	// — i.e. the pre-flight condition legitimately explains the whole remaining
	// deficit. The deterministic "condition-gated lane doing its job → IN SYNC"
	// signal, so the reconciler never has to eyeball raw-vs-effective leaf counts
	// against a big residual by hand (the judgment it got wrong re-filing this bug).
	GateExplainsResidual bool `json:"gate_explains_residual,omitempty"`

	// Dormancy is the live gardener-dormancy verdict: is this gardener PARKED right
	// now behind an open operator escalation/decision (its whole frontier
	// single-threaded behind a human) or a declared block-until-MR-merged? Nil
	// when the trigger is not a gardener. Computed from the canonical engine
	// frontier predicates (bridge.ReconcileDormancyProbe), NOT the stamped
	// dormant_* fields — which lie: the drift math never subtracted dormancy, and
	// dormant_blocker is write-once so it goes stale when the wedge is superseded
	// (advice ka-0000002229). Like the sibling overlays it is explicitly NOT
	// folded into Fires (which stays unconditional). It is the HIGHEST-precedence
	// link in the drift chain — a parked gardener fires nothing BY DESIGN, so its
	// every candidate miss is a park, not drift (bug ka-0000007872: a correctly
	// parked gardener reported 336 phantom missed fires / 100% drift). Fail-safe:
	// Parked=true only on a positively-proven live wedge, so a genuine lease-wedge
	// outage or a genuinely drifting trigger still reports real drift.
	Dormancy *bridge.ReconcileDormancy `json:"dormancy,omitempty"`
	// DormancyExplainsResidual is true when the live dormancy verdict
	// (Dormancy.Parked) is what dropped a POSITIVE residual to a 0 drift
	// candidate — the deterministic "correctly parked, not drifting → IN SYNC"
	// signal, so the reconciler never re-files a phantom drift bug against a
	// gardener the operator is legitimately blocking.
	DormancyExplainsResidual bool `json:"dormancy_explains_residual,omitempty"`
}

// reconcileFleetReport is the JSON envelope emitted by `gb trigger reconcile
// --all`: one per-trigger reconcileReport for every open type=trigger bead that
// declares a wake_intent, plus survey-scope counts. The reconciler agent reads
// Reports directly and never re-queries per trigger — this is the fetch-once
// survey the pass runs (advice ka-0000002706).
type reconcileFleetReport struct {
	// Surveyed is how many open type=trigger beads were scanned; Reconciled is
	// how many carried a wake_intent and produced a Reports entry; Skipped is
	// Surveyed-Reconciled (no declared intent to audit).
	Surveyed   int `json:"surveyed"`
	Reconciled int `json:"reconciled"`
	Skipped    int `json:"skipped"`
	// Truncated is true when the roster hit the scan limit, so a wider fleet may
	// have wake_intent triggers this survey did not see.
	Truncated bool               `json:"truncated,omitempty"`
	Reports   []*reconcileReport `json:"reports"`
}

func runTriggerReconcile(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if reconcileAll {
		if len(args) > 0 {
			return fmt.Errorf("--all reconciles the whole wake_intent fleet; do not also pass a trigger id")
		}
		return runTriggerReconcileAll(cmd)
	}
	if len(args) != 1 {
		return fmt.Errorf("accepts a single trigger id, or --all to reconcile the whole wake_intent fleet")
	}
	id := args[0]

	bead, err := daemon.GetBead(ctx, id)
	if err != nil {
		return fmt.Errorf("fetching trigger %s: %w", id, err)
	}
	if bead == nil {
		return fmt.Errorf("trigger %s not found", id)
	}
	if bead.Type != "trigger" {
		return fmt.Errorf("bead %s is type=%q, expected type=trigger", id, bead.Type)
	}

	report := buildReconcileReport(ctx, bead, time.Now().UTC())
	return emitReconcileReport(cmd, report)
}

// buildReconcileReport computes a single trigger's reconcileReport as of `now`.
// It never returns an error: a malformed cron, an event-only trigger, or a
// fire-reconstruction failure all fold into the report's Note so the survey can
// classify them rather than aborting. (A daemon read failure while listing
// children is the one exception — it is surfaced in Note as well, since a whole
// --all sweep must not die on one trigger's transient read error.)
func buildReconcileReport(ctx context.Context, bead *beadsapi.BeadDetail, now time.Time) *reconcileReport {
	start := now.Add(-reconcileWindow)

	// Clamp the window start to the trigger's creation time: a trigger cannot
	// have "missed" a cron fire that was scheduled before it existed. Without
	// this, a trigger created partway through the lookback window has every
	// pre-creation cron slot counted as a candidate miss — a phantom drift
	// signal that spawns false "habitual"/"impossible" drift bugs (this is the
	// exact cause of bug ka-0000004392: the weekly Monday gardener was created
	// 2026-07-29 but the 168h window's only Monday slot was 2026-07-27, three
	// days before it existed, so the mechanically-correct engine reported a
	// missed fire the trigger never could have made). CreatedAt is UTC from the
	// daemon; a zero value (unset) leaves the plain lookback start untouched.
	windowClamped := false
	if created := bead.CreatedAt; !created.IsZero() && created.After(start) {
		start = created
		windowClamped = true
	}

	report := &reconcileReport{
		TriggerID:               bead.ID,
		Title:                   bead.Title,
		Enabled:                 bead.Fields["enabled"] != "false",
		WakeIntent:              bead.Fields["wake_intent"],
		Cron:                    bead.Fields["cron"],
		Timezone:                bead.Fields["timezone"],
		Conditions:              bead.Fields["conditions"],
		CooldownMinutes:         bead.Fields["cooldown_minutes"],
		BlockedBy:               parseBlockedByField(bead.Fields["blocked_by"]),
		WindowStartEffective:    start,
		WindowClampedToCreation: windowClamped,
	}

	// Live gate snapshot: evaluate the trigger's pre-flight conditions against
	// the CURRENT store so the reconciler can tell a legitimate gated no-fire
	// (would_fire_now=false + a leaf whose excludes floor a non-empty lane to 0)
	// apart from a genuine dark trigger. Computed for every trigger with a
	// conditions field — independent of the cron/fires branches below, since a
	// gated no-fire is exactly what inflates the candidate-miss count that the
	// fires diff (deliberately unconditional) reports. Fail-soft inside
	// buildGateEval: a probe error attaches Evaluated=false, never aborts.
	report.Gate = buildGateEval(ctx, daemon, report.Conditions, bead.Fields["project"])

	// Clamp the window END down to a DISABLED trigger's last live evaluation: a
	// trigger cannot have "missed" a cron fire scheduled AFTER it stopped being
	// live, exactly as it cannot have missed one scheduled before it existed (the
	// creation clamp above). A disabled trigger is skipped at load
	// (trigger_source_*.go: `if bead.Fields["enabled"] == "false" { continue }`),
	// so its last_checked freezes at the last cycle the engine evaluated it while
	// live — a hard upper bound on when it could possibly have fired.
	//
	// This is the exact cause of bug ka-0000007223: a one-shot deferred-enable
	// trigger (cron `33 2 2 8 *`) whose active hour-window opens at 02:00 (the
	// schedule engine's InWindow ignores the cron MINUTE) fired early at
	// 02:00:48Z, served its mission, and its spawned agent self-disabled it before
	// the nominal 02:33Z minute ever arrived. The reconciler then enumerated the
	// post-disable 02:33Z slot and — unable to pair it with the 33-min-early
	// observed fire inside the 10m tolerance — reported a phantom missed=1 that
	// spawned a false "impossible" drift bug. last_checked is UTC from the daemon;
	// an unparseable/zero value leaves the plain window end (now) untouched.
	end := now
	if !report.Enabled {
		if lc := parseReconcileTime(bead.Fields["last_checked"]); !lc.IsZero() && lc.Before(end) {
			end = lc
			report.WindowClampedToDisable = true
		}
	}
	report.WindowEndEffective = end

	if report.Cron == "" {
		report.Note = "trigger has no cron field — event-only trigger; deterministic fire " +
			"reconciliation does not apply. Audit its wake_intent against triggers[]/source config instead."
		return report
	}

	// If the disable clamp collapsed the window (the trigger went dark before the
	// lookback window even opened), it had no live interval in which any cron slot
	// could fire — report zero expected explicitly rather than letting
	// ReconcileFires reject the inverted window. This is the steady state a
	// self-disabled one-shot reaches once its fire ages past the lookback window.
	if report.WindowClampedToDisable && !end.After(start) {
		report.Note = fmt.Sprintf("trigger disabled at %s, before the lookback window opened (%s) — "+
			"no live cron slots in window; nothing could have fired, so nothing was missed.",
			end.UTC().Format(time.RFC3339), start.UTC().Format(time.RFC3339))
		return report
	}

	observed, err := reconstructObservedFires(ctx, bead, start, reconcileTolerance)
	if err != nil {
		report.Note = fmt.Sprintf("could not reconstruct fire history (daemon read error): %v", err)
		return report
	}
	report.ObservedFireCount = len(observed)

	fires, err := scheduler.ReconcileFires(report.Cron, report.Timezone, observed, start, end, reconcileTolerance)
	if err != nil {
		// A malformed cron is a first-class "impossible mechanism" signal — the
		// reconciler should file an impossible-config bug, so surface it in the
		// report rather than failing the command.
		report.Note = fmt.Sprintf("cron reconciliation failed (impossible/invalid mechanism): %v", err)
		return report
	}
	report.Fires = fires

	// Symmetric completion of the disable clamp above. That clamp excludes cron
	// slots scheduled AFTER a disabled trigger went dark from the EXPECTED set.
	// Its mirror on the OBSERVED side: a schedule trigger fires via InWindow,
	// which matches month/day/hour and IGNORES the cron minute (scheduler.cron
	// InWindow), so it fires at the TOP of its active hour-window — up to a full
	// hour before the nominal cron minute. A one-shot that fired early, served
	// its mission, and self-disabled therefore leaves an observed fire whose
	// nominal cron minute falls AFTER the disable instant — exactly the slot the
	// clamp correctly excluded from expected. Without this mirror, that
	// legitimate early fire has no expected counterpart and is misreported as an
	// "extra" over-fire. This is the extra=1 residue of bug ka-0000007223, whose
	// MR !6483 clamp removed the phantom missed=1 but left the orphaned observed
	// fire behind, which then re-filed as impossible drift bug ka-0000001257.
	if report.WindowClampedToDisable {
		dropPostDisableInWindowExtras(report, end)
	}

	// InWindow early-fire re-pairing. The schedule engine fires a cron entry via
	// CronExpr.InWindow (month+day+hour), which IGNORES the cron MINUTE — so a
	// weekly `13 9 * * 1` fires as soon as its ~30s poll first observes the Monday
	// 09:00 hour, i.e. at the TOP of the hour, up to ~59m BEFORE the nominal cron
	// minute. ReconcileFires, by contrast, matches an observed fire to its nominal
	// cron minute with a flat ToleranceS window (default 10m). Any cron whose
	// minute sits more than tolerance into its active hour therefore has its one
	// legitimate top-of-hour fire land OUTSIDE tolerance of the nominal slot — the
	// SAME fire is then double-counted as both a MISS (nominal slot unmatched) AND
	// an EXTRA (observed fire with no expected counterpart), manufacturing a
	// phantom over-fire (expected=1 matched=0 missed=1 extra=1) against a trigger
	// that fired exactly once, on schedule (bug ka-0000003140: OSS-Sanitization
	// Audit Gardener, cron `13 9 * * 1` PT — a 09:00-PDT top-of-hour catch-up fire
	// 13m before the 09:13 nominal slot). This re-pairs each such extra with the
	// unmatched nominal slot in its OWN active cron window, collapsing the phantom
	// miss+extra back into one matched fire. It is the general form of the
	// disable-clamp mirror above (dropPostDisableInWindowExtras), not gated on a
	// disable — and it can NEVER hide a real over-fire: a genuine second fire in a
	// window whose nominal slot already matched finds no UNMATCHED expected slot to
	// pair against and stays in Extra.
	matchInWindowEarlyFires(report)

	// Operator-pause overlay: attribute reported misses to any fleet-wide
	// triggers-pause / scheduler-pause / gardeners-pause gate window that
	// suppressed this trigger's fire path by design, and split MissedCount into
	// pause-suppressed vs residual candidate misses. Computed AFTER Fires is
	// finalized (incl. the disable-clamp extra drop above) so residual_missed is
	// measured against the same diff the report emits. Fail-soft inside
	// buildPauseExclusion: a gate-load error attaches Evaluated=false, never
	// aborts and never mutates Fires. This is the deterministic answer to "is this
	// run of missed fires an operator maintenance pause, not gardener drift?" —
	// the judgment the reconciler got wrong on ka-0000004707 (ka-0000001687).
	report.Pause = buildPauseExclusion(ctx, daemon, bead, report.Fires, start, now)

	// max_concurrent lease overlay: attribute the pause-RESIDUAL misses to the
	// trigger's live-lease throttle — a missed slot at an instant when a prior
	// fire's spawned agent still held the max_concurrent lease was deferred by
	// design, not dropped. Computed AFTER the pause overlay and CHAINED onto it
	// (a miss already inside a pause window is skipped so it is attributed to the
	// pause, never double-counted), so lease.residual_missed is the single count
	// of misses explained by NEITHER a pause NOR the lease. Fail-soft inside
	// buildLeaseExclusion: a holder-scan error attaches Evaluated=false, never
	// aborts and never mutates Fires. This is the deterministic answer to "was this
	// run of misses the max_concurrent lease deferring a slot while a prior long
	// fire was still live?" — the judgment the reconciler got wrong on
	// ka-0000007902 (3rd recurrence on the wedge-watch ka-0000002184).
	report.Lease = buildLeaseExclusion(ctx, daemon, bead, report.Fires, report.Pause, start, now)

	// time_window overlay: attribute the pause/lease-RESIDUAL misses to the
	// trigger's own quiet-hours condition(s) — a missed slot that fell inside a
	// time_window the trigger deliberately does not fire in (op="outside" business
	// hours, the release quiet-hours policy) was suppressed by design, not dropped.
	// Computed AFTER the pause + lease overlays and CHAINED onto them (a miss
	// already attributed to a pause or a full lease is skipped, never
	// double-counted), so time_window.residual_missed is the single count of misses
	// explained by NONE of pause / lease / time_window. Deterministic where the
	// point-in-time gate is not: it evaluates each historical slot at its OWN wall
	// clock rather than reading a would_fire_now snapshot whose verdict depends on
	// WHEN the reconciler runs relative to the window. This is the deterministic
	// answer to "was this run of misses the trigger's quiet-hours gate doing its
	// job?" — the judgment the reconciler got wrong (re-filing a phantom habitual
	// bug) on the release gardener ka-0000005666.
	//
	// Both label regimes are covered (ka-0000006385): the flat `conditions` field
	// (the trigger:schedule preflight) AND the `triggers[]` array (where a
	// trigger:multi gardener's per-entry preflight/match gate actually fires). The
	// overlay unions the time_window leaves from both, so a multi gardener that keeps
	// its quiet-hours gate only in triggers[].preflight no longer reports its
	// by-design suppressions as residual drift.
	report.TimeWindow = buildTimeWindowExclusion(report.Conditions, bead.Fields["triggers"], report.Fires, report.Pause, report.Lease, now)

	// Live gardener-dormancy probe: is this gardener PARKED right now behind an
	// open operator blocker (its whole frontier single-threaded behind a human)
	// or a declared block-until-MR-merged? Recomputed from the canonical engine
	// frontier predicates, NEVER the stamped dormant_* fields (they lie — see the
	// Dormancy doc + advice ka-0000002229). Nil for a non-gardener trigger.
	// Fail-safe inside the probe: only a positively-proven live wedge sets
	// Parked, so a genuine lease-wedge outage or a real drift still reports drift.
	report.Dormancy = bridge.ReconcileDormancyProbe(ctx, daemon, nil, bead)

	// Final drift-candidate composition: fold the live dormancy verdict and the
	// point-in-time gate probe onto the historical pause→lease→time_window
	// residual into the single deterministic DriftCandidateMissed field the
	// reconciler classifies against. Runs LAST so it sees every overlay finalized.
	// Dormancy takes precedence over the gate (a parked gardener fires nothing by
	// design — its misses are parks, not gated no-fires); the gate was the one
	// overlay previously left out of the residual chain (ka-0000004262), dormancy
	// the other (ka-0000007872). Idempotent + fail-soft: an absent/not-parked
	// dormancy and an absent/un-evaluated gate both pass the historical residual
	// through unchanged, never erasing a real candidate.
	applyDormancyAndGateToResidual(report)
	return report
}

// dropPostDisableInWindowExtras removes phantom "extra" over-fires that are in
// fact the legitimate early InWindow fire of a self-disabled trigger. It is only
// called when the window end was clamped to a disabled trigger's last live
// check. An extra fire is dropped when it (a) is InWindow — a fire the engine
// could legitimately have made inside an active cron window — AND (b) its
// nominal in-window cron slot (the first cron match at/after the fire) falls
// after the clamped disable instant, i.e. the exact slot the disable clamp
// already excluded from expected. A genuinely spurious observed fire OUTSIDE any
// active cron window (InWindow=false) is left untouched and still flagged as
// over-firing, so this does not weaken real over-fire detection.
func dropPostDisableInWindowExtras(report *reconcileReport, end time.Time) {
	if report.Fires == nil || len(report.Fires.Extra) == 0 || report.Cron == "" {
		return
	}
	expr, err := scheduler.ParseCron(report.Cron)
	if err != nil {
		return
	}
	loc := time.UTC
	if report.Timezone != "" {
		if parsed, lerr := time.LoadLocation(report.Timezone); lerr == nil {
			loc = parsed
		}
	}

	kept := make([]time.Time, 0, len(report.Fires.Extra))
	dropped := 0
	for _, e := range report.Fires.Extra {
		eLocal := e.In(loc)
		// Nominal cron slot in e's active window = the first cron match at or
		// after e (cron fields are local-time; normalize back to UTC to compare
		// against the UTC disable instant).
		slot := expr.Next(eLocal.Add(-time.Minute))
		if expr.InWindow(eLocal) && !slot.IsZero() && slot.UTC().After(end) {
			dropped++
			continue
		}
		kept = append(kept, e)
	}
	if dropped == 0 {
		return
	}
	report.Fires.Extra = kept
	report.Fires.ExtraCount = len(kept)
	report.Note = strings.TrimSpace(report.Note + fmt.Sprintf(
		" %d observed fire(s) landed early within an active cron window whose nominal cron "+
			"minute is after the disable instant (%s) — counted as the self-disabling InWindow "+
			"fire, not an over-fire.", dropped, end.UTC().Format(time.RFC3339)))
}

// matchInWindowEarlyFires re-pairs an "extra" observed fire that is in fact the
// legitimate top-of-hour InWindow fire of an unmatched nominal cron slot, and
// collapses the resulting phantom miss+extra back into one matched fire.
//
// WHY (bug ka-0000003140). The schedule engine fires via CronExpr.InWindow —
// month+day+hour, MINUTE IGNORED — so a cron with a non-zero minute fires at the
// TOP of its active hour (whenever the ~30s poll first sees the hour open), up to
// ~59m before the nominal cron minute. ReconcileFires matches observed→nominal
// with a flat ToleranceS window; any cron whose minute sits more than tolerance
// into its hour therefore has its one real early fire fall OUTSIDE tolerance of
// the nominal slot, double-counted as both a MISS and an EXTRA. The observed
// OSS-Sanitization Audit Gardener case: cron `13 9 * * 1` (America/Los_Angeles)
// fired at 16:00Z (09:00 PDT top-of-hour) 13m before the 16:13Z nominal slot,
// beyond the 10m tolerance → phantom expected=1 matched=0 missed=1 extra=1.
//
// An extra fire is re-paired only when (a) it is InWindow for the trigger's
// current cron (a fire the engine's InWindow gate would have allowed at that
// instant), (b) the nominal cron slot for that same active window (the first cron
// match at/after the extra's hour top) is itself an UNMATCHED expected fire in
// the diff, and (c) the extra fires at/before that nominal slot (a top-of-hour
// EARLY fire, never a late one — a late unmatched fire past tolerance is a real
// anomaly, left in Extra). First-come pairing consumes each nominal slot once, so
// a genuine second fire in an already-matched window finds no unmatched slot and
// stays in Extra. Fail-soft: unparseable cron leaves the diff untouched.
func matchInWindowEarlyFires(report *reconcileReport) {
	if report.Fires == nil || len(report.Fires.Extra) == 0 || report.Cron == "" {
		return
	}
	expr, err := scheduler.ParseCron(report.Cron)
	if err != nil {
		return
	}
	loc := time.UTC
	if report.Timezone != "" {
		if parsed, lerr := time.LoadLocation(report.Timezone); lerr == nil {
			loc = parsed
		}
	}

	kept := make([]time.Time, 0, len(report.Fires.Extra))
	paired := 0
	for _, e := range report.Fires.Extra {
		eLocal := e.In(loc)
		// The nominal cron slot in e's active window is the first cron match at or
		// after e (InWindow fires at the hour top; the nominal minute is later in
		// the same window). Only re-pair a genuine EARLY fire — one at/before its
		// own nominal slot.
		slot := expr.Next(eLocal.Add(-time.Minute))
		if !expr.InWindow(eLocal) || slot.IsZero() || slot.Before(eLocal) {
			kept = append(kept, e)
			continue
		}
		// TIGHT top-of-hour guard. InWindow ignores the cron minute, so an early
		// fire lands at the TOP of the active hour, BEFORE the hour's first cron
		// minute. Require exactly that: no cron minute in e's own clock hour at or
		// before e (the most recent cron match precedes the start of e's hour).
		// This is what separates a genuine InWindow top-of-hour fire (OSS gardener
		// `13 9 * * 1` firing at :00, nominal :13) from an off-cadence fire on an
		// hourly-active cron (groomer `3,33 * * * *` firing at :18 — its hour's
		// :03 slot already fired, so :18 is genuinely spurious and stays Extra).
		hourStart := time.Date(eLocal.Year(), eLocal.Month(), eLocal.Day(), eLocal.Hour(), 0, 0, 0, loc)
		if prev := expr.Prev(eLocal); prev.IsZero() || !prev.Before(hourStart) {
			kept = append(kept, e)
			continue
		}
		slotUTC := slot.UTC().Truncate(time.Minute)
		matchedHere := false
		for i := range report.Fires.Expected {
			ef := &report.Fires.Expected[i]
			if ef.Matched || !ef.At.UTC().Truncate(time.Minute).Equal(slotUTC) {
				continue
			}
			wasPending := ef.Pending
			ef.Matched = true
			ef.Pending = false
			ef.Observed = e.UTC()
			ef.LatenessS = int(e.UTC().Sub(ef.At.UTC()) / time.Second) // negative: fired early
			report.Fires.MatchedCount++
			// A re-paired slot was either MISSED or PENDING; move the count out of
			// whichever bucket it was in so ExpectedCount == matched+missed+pending
			// stays invariant (ka-0000001791).
			if wasPending {
				report.Fires.PendingCount--
			} else {
				report.Fires.MissedCount--
			}
			paired++
			matchedHere = true
			break
		}
		if !matchedHere {
			kept = append(kept, e)
		}
	}
	if paired == 0 {
		return
	}
	report.Fires.Extra = kept
	report.Fires.ExtraCount = len(kept)
	report.Note = strings.TrimSpace(report.Note + fmt.Sprintf(
		" %d observed fire(s) landed at the top of an active cron hour-window (the engine fires "+
			"via InWindow, ignoring the cron minute) and were re-paired with their unmatched "+
			"nominal cron slot — a scheduled early fire, not an over-fire.", paired))
}

// parseReconcileTime parses an RFC3339 timestamp from a trigger field (e.g.
// last_checked / last_run), returning the zero time for an empty or unparseable
// value so callers can fall back to their unclamped default. UTC-normalized to
// match the rest of the reconcile arithmetic.
func parseReconcileTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC()
	}
	return time.Time{}
}

// runTriggerReconcileAll surveys every open type=trigger bead that declares a
// wake_intent and emits a single reconcileFleetReport. This is the per-pass
// survey the wake-intent reconciler agent runs: one roster list plus a
// per-trigger child query, versus a reconcile-per-trigger fan-out.
func runTriggerReconcileAll(cmd *cobra.Command) error {
	ctx := cmd.Context()
	now := time.Now().UTC()

	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Limit:    reconcileRosterLim,
	})
	if err != nil {
		return fmt.Errorf("listing trigger roster: %w", err)
	}

	fleet := reconcileFleetReport{
		Truncated: len(res.Beads) >= reconcileRosterLim,
		Reports:   []*reconcileReport{},
	}
	for _, bead := range res.Beads {
		if bead == nil {
			continue
		}
		fleet.Surveyed++
		// Only triggers with a declared intent are auditable — skip the rest
		// (they rely on the mechanical liveness sweep alone).
		if strings.TrimSpace(bead.Fields["wake_intent"]) == "" {
			continue
		}
		fleet.Reconciled++
		fleet.Reports = append(fleet.Reports, buildReconcileReport(ctx, bead, now))
	}
	fleet.Skipped = fleet.Surveyed - fleet.Reconciled

	if jsonOutput {
		printJSON(fleet)
		return nil
	}
	return printReconcileFleetHuman(cmd, fleet)
}

// auditTrailLabel marks the append-only type=event ledger beads
// recordTriggerAudit writes on every fire (trigger_permissions.go). They share
// the trigger:<id> label with real spawned children — see reconstructObservedFires
// for how they are used (a FALLBACK per-fire record) and the two regressions the
// handling guards against.
const auditTrailLabel = "auto:trigger-audit"

// scheduleFireKeyLayout is the minute-granularity fire-key format the schedule
// source stamps as match_key (trigger_source_schedule.go: fireKey =
// lastFire.UTC().Format(...)). It is UTC (no zone in the layout → time.Parse
// yields UTC), the same grain the cron diff matches on.
const scheduleFireKeyLayout = "2006-01-02T15:04"

// cronFiresAt reports whether cron (evaluated in loc) has a scheduled fire at
// exactly the minute of t. Prev returns the most recent match at or before its
// (minute-truncated) argument, so an argument that is itself a fire slot comes
// back unchanged. Used to validate an audit-ledger fire record — which carries
// no schedule_cron — against the trigger's CURRENT cron, so a fire stamped under
// a since-superseded schedule is not resurrected as a phantom over-fire.
func cronFiresAt(cron *scheduler.CronExpr, loc *time.Location, t time.Time) bool {
	local := t.In(loc).Truncate(time.Minute)
	return cron.Prev(local).Equal(local)
}

// snapLastRunToNominalMinute normalizes the trigger's last_run — its ONLY
// execution-wall-clock fire source — onto the nominal cron minute of the fire it
// records, so it dedups against that fire's canonical minute-stamped record
// instead of becoming a phantom over-fire.
//
// THE BUG (ka-0000002813, third recurrence on the intake groomer ka-0000006210).
// Every OTHER observed-fire source records the NOMINAL cron minute: a spawned
// child stamps schedule_fire_time = expr.Prev(now) and the audit ledger stamps
// match_key = the same minute (trigger_source_schedule.go: lastFire =
// expr.Prev(nowLocal)). last_run is the lone exception — gardener_evaluator.go
// stamps it time.Now().UTC(), the wall-clock instant the fire actually EXECUTED,
// which the engine's ~30s poll + cooldown + preflight can push minutes past the
// nominal cron minute. reconstructObservedFires dedups on the truncated MINUTE,
// so a fire that executed late (e.g. cron slot 18:03, executed 18:10:16) is
// counted twice: 18:03 from its child/audit record matches its cron slot, while
// last_run at 18:10 lands in a different minute bucket and falls through to Extra
// as a phantom over-fire. The gate probe from MR !6488 explains gated MISSES
// only (never extras), so that one phantom extra was the sole unexplained signal
// and re-filed the drift bug for a third time — matched=0 missed=0 extra=1.
//
// THE FIX. When last_run lands within `tolerance` of a real cron slot — i.e. it
// is a plausible late execution of that slot, exactly the slack ReconcileFires
// already grants a matched fire — snap it back to that nominal minute so it
// collapses into the same-fire record. A last_run that is NOT near any cron slot
// (a genuinely spurious out-of-window fire) is left untouched and still surfaces
// as a real over-fire, so this never weakens over-fire detection. Fail-soft: a
// nil cron or a Prev/Next with no match in horizon returns lr unchanged.
//
// Callers reconstructing the observed-fire set pass through
// snapLastRunToObservedFire, which layers an already-observed collapse on top of
// this bounded snap for the very-late condition-gated case (ka-0000003959).
func snapLastRunToNominalMinute(lr time.Time, cron *scheduler.CronExpr, loc *time.Location, tolerance time.Duration) time.Time {
	if cron == nil {
		return lr
	}
	if tolerance <= 0 {
		tolerance = scheduler.DefaultReconcileTolerance
	}
	local := lr.In(loc)
	// The two candidate cron slots bracketing lr: the most recent at/before it
	// (a late execution of that slot) and the next one after it (an execution
	// that raced slightly ahead of the poll). Snap to whichever is within
	// tolerance and nearest — mirroring ReconcileFires' greedy-nearest match.
	best := lr
	bestDelta := tolerance + time.Nanosecond // strictly beyond tolerance ⇒ no snap
	for _, slot := range []time.Time{cron.Prev(local), cron.Next(local.Add(-time.Minute))} {
		if slot.IsZero() {
			continue
		}
		delta := local.Sub(slot)
		if delta < 0 {
			delta = -delta
		}
		if delta <= tolerance && delta < bestDelta {
			best, bestDelta = slot.UTC(), delta
		}
	}
	return best
}

// snapLastRunToObservedFire extends snapLastRunToNominalMinute with the set of
// fire minutes ALREADY reconstructed from durable per-fire records (work beads /
// audit-ledger beads), keyed exactly as reconstructObservedFires' `seen` map
// (UTC minute-truncated unix). It closes the blind spot the fixed `tolerance`
// leaves open for a legitimately-late CONDITION-GATED fire.
//
// THE RECURRENCE (ka-0000003959, gardener-rescope drain ka-0000007515). A
// condition-gated schedule trigger fires slot expr.Prev(now) only once its
// pre-flight bead-query gate passes, so execution can trail the nominal minute
// by far more than the tolerance-snap's fixed 10m window: the 2026-08-14T18:08Z
// slot did not execute until 18:26:13Z — 18m13s late, because the needs-rescope
// lane was empty until then — and its last_run wall-clock landed 8m past both
// neighbouring cron slots (18:08 is 18m away, 18:38 is 12m away). The bounded
// snap left it untouched, so it fell through to Extra as a phantom over-fire
// (extra=1) even though the SAME fire was already observed at its nominal 18:08
// minute via the spawned work bead — re-filing an over-firing drift bug against
// a trigger that fired exactly on cadence.
//
// THE COLLAPSE. The engine ALWAYS fires slot expr.Prev(nowLocal) and stamps
// last_run = now (gardener_evaluator.go / stampLastRun), so expr.Prev(last_run)
// IS that fire's nominal minute by construction. When that minute is already in
// the observed set — i.e. a durable per-fire record captured the same fire —
// last_run is redundant and collapses onto it regardless of how late execution
// ran. This can never hide a real over-fire: an extra fire needs its OWN cron
// minute not already recorded by a work/audit bead, and last_run holds only the
// single LATEST timestamp anyway (the audit-ledger/work-bead paths are the real
// per-fire over-fire signal). When expr.Prev(last_run) is NOT already observed
// (last_run is the sole record of a fire whose child was reaped, or a genuinely
// off-cadence spurious fire), fall through to the bounded tolerance snap so that
// path behaves exactly as before. A nil `seen` disables the collapse entirely,
// preserving the plain snapLastRunToNominalMinute contract.
func snapLastRunToObservedFire(lr time.Time, cron *scheduler.CronExpr, loc *time.Location, tolerance time.Duration, seen map[int64]bool) time.Time {
	if cron != nil && seen != nil {
		if prev := cron.Prev(lr.In(loc)); !prev.IsZero() {
			prevUTC := prev.UTC()
			if seen[prevUTC.Truncate(time.Minute).Unix()] {
				return prevUTC
			}
		}
	}
	return snapLastRunToNominalMinute(lr, cron, loc, tolerance)
}

// reconstructObservedFires rebuilds the trigger's observed fire times from
// durable bead state, without the gardener self-reporting:
//
//   - children carrying schedule_fire_time (the canonical per-fire stamp the
//     schedule source writes onto every spawned bead — trigger_source_schedule.go),
//   - audit-ledger beads' match_key as a FALLBACK per-fire record for fires that
//     spawn no schedule_fire_time-stamped work bead (see below),
//   - the trigger's own last_run (best-effort last fire) as a floor, so a trigger
//     that fired once but whose child was reaped still shows one observed fire.
//
// Only fires at or after `start` are kept. Duplicate fire times (same minute)
// are collapsed so a fan-out that spawned several beads for one cron tick counts
// as one fire, matching the cron grain.
//
// WHY AUDIT-LEDGER BEADS ARE A FALLBACK FIRE SOURCE (bug ka-0000007130). Not
// every fire produces a work bead carrying schedule_fire_time + the trigger:<id>
// label. An apply_formula fire pours a MOLECULE (labeled project:<p> only, no
// trigger label, no schedule_fire_time) and an AssignBead-pinned gardener reuses
// one long-lived task — so for those triggers the ONLY durable trigger:<id>-
// labeled per-fire record is the append-only audit-ledger bead recordTriggerAudit
// writes on every fire. Skipping it entirely (the pre-fix behavior) collapsed the
// observed set to last_run alone (a single timestamp = the LATEST fire), so every
// earlier real fire read as a phantom "unexplained-miss" — exactly the false miss
// this trigger's daily-standup gardener hit (it fired on 2026-08-04, molecule +
// audit bead both prove it, yet the diff reported missed=1). The fix reads the
// audit bead's match_key — the exact cron-minute fire key the schedule source
// stamped — as a fire time of last resort.
//
// Two regressions this handling must NOT reintroduce — both manufacture phantom
// "extra" over-fires against the CURRENT cron, the recurrence bug ka-0000004209
// (habitual sibling of ka-0000004392, whose MR !6478 clamp fixed a different,
// creation-boundary symptom):
//
//  1. Audit receipt double-counting the SAME fire. When a fire DOES spawn a
//     schedule_fire_time work bead, its audit receipt records the same cron
//     minute in match_key; because both bucket to the same minute the audit
//     record dedups against the real fire rather than becoming a phantom second
//     fire ~seconds later. An audit bead with no match_key, a non-timestamp
//     match_key, or a denied/errored outcome contributes nothing (skipped) — so
//     it can never become the spurious over-fire the pre-ka-0000004209
//     CreatedAt fallback produced.
//  2. Fires stamped with a superseded cron. Every spawned child records the
//     schedule_cron in force AT FIRE TIME (trigger_source_schedule.go); when an
//     operator later edits the cron, a legitimate fire under the OLD schedule has
//     no counterpart in the NEW cron and is flagged "extra". Work beads carry
//     schedule_cron so the direct comparison drops them; audit beads do NOT, so
//     their fallback fire is validated against the current cron via cronFiresAt
//     instead (a fire minute the current cron no longer schedules is dropped).
//     Parse failures / missing stamps fall through (kept) so nothing regresses to
//     dropping real fires on an unparseable field.
func reconstructObservedFires(ctx context.Context, trigger *beadsapi.BeadDetail, start time.Time, tolerance time.Duration) ([]time.Time, error) {
	seen := make(map[int64]bool)
	var fires []time.Time
	add := func(t time.Time) {
		if t.IsZero() || t.Before(start) {
			return
		}
		key := t.UTC().Truncate(time.Minute).Unix()
		if seen[key] {
			return
		}
		seen[key] = true
		fires = append(fires, t.UTC())
	}

	// Parse the trigger's CURRENT cron once so we can drop fires stamped with a
	// superseded schedule. A trigger with no/invalid cron leaves currentCron nil,
	// and the stale-cron filter is skipped entirely (nothing to compare against).
	var currentCron *scheduler.CronExpr
	if raw := trigger.Fields["cron"]; raw != "" {
		if parsed, perr := scheduler.ParseCron(raw); perr == nil {
			currentCron = parsed
		}
	}

	// Timezone the current cron is evaluated in, for the audit-fallback stale-cron
	// guard (cronFiresAt). Matches the zone ReconcileFires enumerates expected
	// fires in; an empty/invalid zone falls back to UTC exactly as it does.
	loc := time.UTC
	if tz := trigger.Fields["timezone"]; tz != "" {
		if parsed, lerr := time.LoadLocation(tz); lerr == nil {
			loc = parsed
		}
	}

	// Children spawned by this trigger carry schedule_fire_time. The schedule
	// source labels each spawned bead trigger:<id>, so query by that label. Audit-
	// ledger beads carry the same label but are handled as a fallback fire source
	// (see the doc comment); collect them separately and add them AFTER the work-
	// bead fires + last_run so a fire and its same-minute audit receipt dedup.
	//
	// PAGE the whole labeled set — never a single capped page (ka-0000007883). A
	// fixed Limit sorted -created_at silently drops the OLDEST children once the
	// set outgrows the cap, and every dropped child is a fire whose slot then
	// reads as a phantom in-window miss. A busy */30 trigger spawns a work bead
	// AND an audit bead per fire — ~2 beads/fire, ~672 over a 168h window — so a
	// 500-cap dropped ~28 of the earliest, manufacturing 14 residual "misses" at
	// the window's leading edge (drift_candidate_missed=14) against a trigger
	// that in fact fired every one of those slots. ListAllBeadsFiltered exhausts
	// the match set by offset-walking and warns loudly (never truncates
	// silently) if it ever hits its own page cap — the exact hazard-class fix in
	// advice ka-0000003555. It pins created_at-ascending internally, so the
	// audit-vs-work-bead dedup below is unaffected by iteration order.
	res, err := daemon.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"trigger:" + trigger.ID},
	})
	if err != nil {
		return nil, err
	}
	var auditFires []time.Time
	for _, child := range res.Beads {
		if child == nil {
			continue
		}
		// Audit-ledger bead: the fallback per-fire record. Only an ALLOWED fire
		// whose match_key parses as a schedule fire key and still lands on the
		// CURRENT cron counts (see doc comment for both guards).
		if slices.Contains(child.Labels, auditTrailLabel) {
			if child.Fields["outcome"] != "allow" {
				continue
			}
			mk := child.Fields["match_key"]
			if mk == "" {
				continue
			}
			ft, perr := time.Parse(scheduleFireKeyLayout, mk)
			if perr != nil {
				continue
			}
			ft = ft.UTC()
			if currentCron != nil && !cronFiresAt(currentCron, loc, ft) {
				continue
			}
			auditFires = append(auditFires, ft)
			continue
		}
		// Skip fires stamped with a cron that is a structurally different schedule
		// from the trigger's current one (operator edited the cron since). Only a
		// present, parseable, genuinely-different stamp drops the fire; an absent or
		// unparseable stamp, or a cron the trigger no longer has, is kept.
		if currentCron != nil {
			if fc := child.Fields["schedule_cron"]; fc != "" {
				if firedCron, perr := scheduler.ParseCron(fc); perr == nil && !firedCron.Equal(currentCron) {
					continue
				}
			}
		}
		if ft := child.Fields["schedule_fire_time"]; ft != "" {
			if parsed, perr := time.Parse(time.RFC3339, ft); perr == nil {
				add(parsed)
				continue
			}
		}
		// Fallback: a child with no explicit fire stamp counts at its creation
		// time (older schedule beads predate the schedule_fire_time stamp).
		add(child.CreatedAt)
	}

	// last_run is the trigger's own best-effort last-fire marker. Include it so a
	// single-fire trigger whose child was GC'd still reconstructs one fire. Unlike
	// every other fire source, last_run records the fire's EXECUTION wall-clock,
	// not its nominal cron minute — so snap a within-tolerance late execution back
	// to its cron slot before dedup, or it double-counts as a phantom over-fire
	// (ka-0000002813; see snapLastRunToNominalMinute).
	if lr := trigger.Fields["last_run"]; lr != "" {
		if parsed, perr := time.Parse(time.RFC3339, lr); perr == nil {
			// Pass `seen` so a legitimately-late condition-gated fire whose nominal
			// slot is already recorded by a work/audit bead collapses onto it, even
			// when execution ran past the bounded tolerance window (ka-0000003959).
			add(snapLastRunToObservedFire(parsed, currentCron, loc, tolerance, seen))
		}
	}

	// Apply audit-ledger fallback fires LAST so a fire already reconstructed from
	// a work bead / last_run collapses its audit receipt in the minute-dedup
	// (regression 1), never a phantom second fire.
	for _, ft := range auditFires {
		add(ft)
	}

	sort.Slice(fires, func(i, j int) bool { return fires[i].Before(fires[j]) })
	return fires, nil
}

func emitReconcileReport(cmd *cobra.Command, report *reconcileReport) error {
	if jsonOutput {
		printJSON(report)
		return nil
	}
	return printReconcileHuman(cmd, report)
}

// printReconcileFleetHuman renders the --all survey envelope for a terminal:
// a scope header, then each per-trigger report. The JSON envelope is the
// machine surface the reconciler agent reads; this is for a human spot-check.
func printReconcileFleetHuman(cmd *cobra.Command, fleet reconcileFleetReport) error {
	cmd.Printf("Wake-intent fleet survey: %d triggers surveyed, %d with wake_intent reconciled, %d skipped%s\n\n",
		fleet.Surveyed, fleet.Reconciled, fleet.Skipped,
		map[bool]string{true: "   [TRUNCATED — roster limit hit]", false: ""}[fleet.Truncated])
	if fleet.Reconciled == 0 {
		cmd.Println("No open triggers declare a wake_intent — nothing to reconcile.")
		return nil
	}
	for _, r := range fleet.Reports {
		if err := printReconcileHuman(cmd, r); err != nil {
			return err
		}
		cmd.Println()
	}
	return nil
}

func printReconcileHuman(cmd *cobra.Command, r *reconcileReport) error {
	cmd.Printf("Trigger %s — %s\n", r.TriggerID, r.Title)
	cmd.Printf("  enabled:     %v\n", r.Enabled)
	if r.WakeIntent != "" {
		cmd.Printf("  wake_intent: %s\n", r.WakeIntent)
	} else {
		cmd.Println("  wake_intent: (unset — no declared intent to audit)")
	}
	cmd.Printf("  cron:        %s  tz=%s\n", defaultIfEmpty(r.Cron, "(none)"), defaultIfEmpty(r.Timezone, "UTC"))
	if r.Conditions != "" {
		cmd.Printf("  conditions:  %s\n", r.Conditions)
	}
	if r.Note != "" {
		cmd.Printf("  note: %s\n", r.Note)
	}
	printGate(cmd, r.Gate)
	printDormancy(cmd, r.Dormancy)
	printPause(cmd, r.Pause)
	printLease(cmd, r.Lease)
	printTimeWindow(cmd, r.TimeWindow)
	if r.Fires == nil {
		return nil
	}
	f := r.Fires
	cmd.Printf("\n  window: %s → %s (tolerance %ds)%s%s\n",
		f.WindowStart.Format(time.RFC3339), f.WindowEnd.Format(time.RFC3339), f.ToleranceS,
		map[bool]string{true: "   [start clamped to trigger creation]", false: ""}[r.WindowClampedToCreation],
		map[bool]string{true: "   [end clamped to disable]", false: ""}[r.WindowClampedToDisable])
	cmd.Printf("  observed fires: %d\n", r.ObservedFireCount)
	pendingNote := ""
	if f.PendingCount > 0 {
		pendingNote = fmt.Sprintf("   PENDING(not yet overdue): %d", f.PendingCount)
	}
	cmd.Printf("  expected: %d   matched: %d   MISSED: %d   EXTRA(over-fire): %d%s%s\n",
		f.ExpectedCount, f.MatchedCount, f.MissedCount, f.ExtraCount, pendingNote,
		map[bool]string{true: "   [TRUNCATED — window too wide]", false: ""}[f.Truncated])
	if f.MissedCount > 0 {
		cmd.Println("  missed fires (candidate — may have been gated):")
		for _, ef := range f.Expected {
			if !ef.Matched && !ef.Pending {
				cmd.Printf("    - %s\n", ef.At.Format(time.RFC3339))
			}
		}
	}
	if f.PendingCount > 0 {
		cmd.Println("  pending fires (inside tolerance of window end — no verdict yet):")
		for _, ef := range f.Expected {
			if ef.Pending {
				cmd.Printf("    - %s\n", ef.At.Format(time.RFC3339))
			}
		}
	}
	if f.ExtraCount > 0 {
		cmd.Println("  extra fires (no cron counterpart — over-firing):")
		for _, e := range f.Extra {
			cmd.Printf("    - %s\n", e.Format(time.RFC3339))
		}
	}
	cmd.Printf("  DRIFT CANDIDATE (missed, after pause+lease+time_window+dormancy+gate): %d", r.DriftCandidateMissed)
	switch {
	case r.DormancyExplainsResidual:
		cmd.Printf("   → the gardener is parked (dormant) behind an open operator blocker; the residual is by-design parks, not drift")
	case r.TimeWindow != nil && r.TimeWindow.Evaluated && r.TimeWindow.SuppressedMissed > 0 && r.TimeWindow.ResidualMissed == 0:
		cmd.Printf("   → every missed slot fell inside the trigger's own quiet-hours window; suppressed by design, not drift")
	case r.GateExplainsResidual:
		cmd.Printf("   → the pre-flight gate declines to fire now; the residual is gated no-fires, not drift")
	}
	cmd.Println()
	return nil
}

// printDormancy renders the live gardener-dormancy verdict for a human
// spot-check. No-op for a non-gardener trigger (nil dormancy). The JSON dormancy
// block is the machine surface the reconciler agent reads; this is the operator
// eyeball line. It prints the LIVE wedge inputs (blocker id + actionable-child
// count) and flags a stale stamped_blocker so a reader never trusts the
// write-once field the bug warns about (ka-0000007872 ASK 2 / advice
// ka-0000002229).
func printDormancy(cmd *cobra.Command, d *bridge.ReconcileDormancy) {
	if d == nil {
		return
	}
	if !d.Evaluated {
		cmd.Println("  dormancy: (could not evaluate — no daemon client)")
		return
	}
	if !d.Parked {
		// Only worth a line when the gardener DECLARED dormancy yet is NOT
		// live-parked — the exact "the field says dormant but the frontier is
		// actionable, so this IS drift" case the bug's break-it test covers.
		if d.DormantDeclared {
			cmd.Printf("  dormancy: NOT parked (live frontier is actionable: %s) — "+
				"dormant_on_escalation=true is stale; missed fires are real drift\n",
				dormancyActionableSummary(d))
		}
		return
	}
	cmd.Printf("  dormancy: PARKED (%s, live blocker %s)", defaultIfEmpty(d.Kind, "operator_wedge"), d.LiveBlocker)
	if d.Kind == "operator_wedge" {
		cmd.Printf("  → entire frontier single-threaded behind an open operator escalation/decision (%s); fires nothing by design",
			dormancyActionableSummary(d))
	}
	cmd.Println()
	if d.StampedBlockerStale {
		cmd.Printf("    ⚠ stamped dormant_blocker %s is STALE (superseded) — the live blocker is %s; trust the live value\n",
			d.StampedBlocker, d.LiveBlocker)
	}
}

// dormancyActionableSummary renders the live actionable-child count for the
// human line, or a "not computed" note when the probe could not measure it.
func dormancyActionableSummary(d *bridge.ReconcileDormancy) string {
	if d.ActionableChildren < 0 {
		return "actionable children not computed"
	}
	return fmt.Sprintf("actionable children=%d", d.ActionableChildren)
}

func init() {
	triggerReconcileCmd.Flags().DurationVar(&reconcileWindow, "window", 7*24*time.Hour,
		"lookback window for observed fires (e.g. 24h, 168h)")
	triggerReconcileCmd.Flags().DurationVar(&reconcileTolerance, "tolerance", scheduler.DefaultReconcileTolerance,
		"max slack between an expected cron fire and a matching observed fire")
	// Deprecated no-op: fire reconstruction now offset-walks the whole labeled
	// child set (ListAllBeadsFiltered), so there is no cap to tune. Kept as a
	// hidden flag so any script still passing --child-limit does not error
	// (ka-0000007883).
	triggerReconcileCmd.Flags().IntVar(&reconcileChildLim, "child-limit", 0,
		"deprecated no-op: fire reconstruction pages the whole child set")
	_ = triggerReconcileCmd.Flags().MarkDeprecated("child-limit",
		"fire reconstruction now pages the whole child set; the flag is a no-op")
	_ = triggerReconcileCmd.Flags().MarkHidden("child-limit")
	triggerReconcileCmd.Flags().BoolVar(&reconcileAll, "all", false,
		"reconcile the whole fleet: survey every open trigger with a wake_intent and emit one envelope (no trigger id)")
	triggerReconcileCmd.Flags().IntVar(&reconcileRosterLim, "roster-limit", 1000,
		"max open triggers scanned for the --all fleet survey")
	triggerCmd.AddCommand(triggerReconcileCmd)
}
