// Package main — max_concurrent lease overlay half of `gb trigger reconcile`.
//
// scheduler.ReconcileFires deliberately computes the UNCONDITIONAL cron diff
// (docs/wake-intent-reconciler.md, "Scope boundary (load-bearing)"): it ignores
// conditions / cooldown / blocked_by / enabled so intent and mechanism stay
// separable. That purity is correct — but it also means it ignores the
// max_concurrent LIVE LEASE, and a spawn-producing trigger whose run outlives
// its cron period defers the next slot BY DESIGN. That deferred slot reads as a
// "missed" fire the reconciler cannot tell apart from real gardener-logic drift.
//
// THE FALSE POSITIVE THIS FIXES (ka-0000007902, 3rd recurrence on the monorepo
// wedge-watch ka-0000002184). The wedge-watch fires `*/30` with max_concurrent=1.
// After the operator-pause overlay (MR !7097) already explains the bulk of the
// deficit, TWO residual misses (2026-08-09 02:00 and 16:00) remained unexplained
// and re-filed the bug. Both were the concurrency guard doing its job: the prior
// fire's spawned agent was still live across the missed slot, so the engine's
// max_concurrent=1 lease deferred the intervening */30 slot, which then fired
// late the moment that agent terminated:
//   - agent [01:30:23 → closed 02:37:31] held the lease across 02:00 → 02:30 slot fired 02:37:32
//   - agent [15:30:34 → closed 16:48:11] held the lease across 16:00 → 16:30 slot fired 16:48:48
//
// Both were legitimately long wedge-watch runs (67m, 78m) exceeding the 30-min
// cron period. Not drift — the single-lane single-threading (triggers.go
// liveSpawnCount / max_concurrent) working exactly as designed.
//
// This file adds a LIVE lease overlay to the report WITHOUT folding the lease
// into ReconcileFires. It loads the agent beads this trigger spawned
// (spawned_by:trigger:<id>), reconstructs each holder's lease-held interval from
// its [created_at, closed_at] lifetime — capped by the SAME staleness bounds the
// engine releases on (leaseHolderStale updated_at + threshold, and
// leaseHolderOverAgeCeiling created_at + 2×threshold) — and attributes a residual
// missed slot to the lease when the engine would have had ≥ max_concurrent live
// holders at that instant. Fires stays byte-for-byte intact.
//
// The staleness cap is LOAD-BEARING, not decoration: it is exactly what keeps
// this overlay from hiding the dark-lane bug the lease-stale mechanism itself was
// built to surface (ka-0000001095 / ka-0000002023). A holder wedged for 15h
// stops holding the lease at the 3h/6h release bound, so misses AFTER that bound
// are genuine dark-lane drift and stay RESIDUAL — the overlay only credits the
// window in which the engine truly deferred.
//
// Chained onto the pause overlay: a missed slot already explained by an operator
// pause is skipped here, so lease.residual_missed is the SINGLE deterministic
// count of misses explained by NEITHER an operator pause NOR the concurrency
// lease — the final genuine-drift candidate the reconciler classifies. This
// mirrors the additive, fail-soft contract of the gate probe
// (trigger_reconcile_gate.go, MR !6488) and the pause overlay
// (trigger_reconcile_pause.go, MR !7097): any query error leaves the overlay
// Evaluated=false rather than manufacturing — or silently erasing — a signal.
package main

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"

	"github.com/spf13/cobra"
)

// defaultMaxConcurrent mirrors bridge.triggerParseInt's default for the
// max_concurrent field (triggers.go ParseTriggerDef): a trigger with no explicit
// max_concurrent caps concurrent fires at 3. Duplicated here (cmd/gb cannot
// import the bridge's unexported parser) so the overlay's lease arithmetic reads
// the same cap the engine enforces.
const defaultMaxConcurrent = 3

// leaseThresholdDefault mirrors bridge.defaultLeaseStaleAfter (3h): the age past
// which a quiet agent stops holding a trigger's max_concurrent lease. Duplicated
// for the same reason as defaultMaxConcurrent.
const leaseThresholdDefault = 3 * time.Hour

// leaseAgeCeilingFactor mirrors bridge.leaseAgeCeilingFactor (2): a holder past
// created_at + factor×threshold has livelocked and stops counting regardless of
// updated_at churn (bug ka-0000002023).
const leaseAgeCeilingFactor = 2

// leaseHolderScanLimit caps the spawned-agent scan. A high-frequency spawn
// trigger produces at most a few hundred agent beads over a sane audit window
// (the wedge-watch made ~190 over 168h); a generous cap paired with -created_at
// sort covers every holder whose interval could overlap the window.
const leaseHolderScanLimit = 1000

// leaseWindow is one agent bead's reconstructed lease-held interval. [Start, End]
// is the span during which this holder counted toward the trigger's
// max_concurrent cap: Start = created_at, End = the earliest of its close, its
// quiet-staleness release (updated_at + threshold), and its age-ceiling release
// (created_at + 2×threshold) — exactly the bounds liveSpawnHolders releases on.
type leaseWindow struct {
	AgentID string    `json:"agent_id"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	// Open reports the holder was still alive (never closed) at the reconcile
	// snapshot, so End is `now` (or a staleness cap), a lower bound.
	Open bool `json:"open,omitempty"`
}

// leaseExclusion is the max_concurrent lease overlay attached to a
// reconcileReport. It answers the ONE question ReconcileFires cannot: of the
// missed fires NOT already explained by an operator pause, how many fell at an
// instant when the engine already had ≥ max_concurrent live lease holders (so
// the fire was deferred by design), and how many are RESIDUAL — misses explained
// by neither pause nor lease, the only genuine-drift candidates.
type leaseExclusion struct {
	// MaxConcurrent is the cap the lease arithmetic used (the trigger's
	// max_concurrent field, default 3). Surfaced so the reconciler can see WHY a
	// given overlap was or was not treated as suppressing.
	MaxConcurrent int `json:"max_concurrent"`
	// Windows are the lease holder intervals that actually EXPLAIN a suppressed
	// miss (a holder whose interval covers at least one lease-suppressed slot),
	// oldest-first. Empty in the healthy case (no residual miss coincided with a
	// full lease) — the report stays tight, mirroring the gate probe's
	// exclude-bearing-leaves-only rule.
	Windows []leaseWindow `json:"windows,omitempty"`
	// SuppressedMissed is how many pause-residual misses fell at an instant with
	// ≥ MaxConcurrent live holders — legitimate concurrency-deferred no-fires.
	SuppressedMissed int `json:"suppressed_missed"`
	// ResidualMissed is the pause-residual miss count minus SuppressedMissed:
	// misses explained by NEITHER an operator pause NOR the concurrency lease.
	// THIS is the count the reconciler classifies as candidate drift.
	// residual_missed == 0 while suppressed_missed > 0 (or pause explains the
	// rest) is the deterministic "an operator pause and/or the max_concurrent
	// lease explain the whole deficit → IN SYNC" signal.
	ResidualMissed int `json:"residual_missed"`
	// Evaluated is false when the spawned-agent scan failed (daemon error): the
	// overlay is then advisory-absent and the agent falls back to the pause
	// residual. Explicit so a genuine zero-holder result is never confused with a
	// failed load.
	Evaluated bool `json:"evaluated"`
	// Error carries the fail-soft reason when Evaluated is false.
	Error string `json:"error,omitempty"`
}

// leaseHolderClient is the daemon capability the lease overlay needs.
// *beadsapi.Client satisfies it; the reconcile command's package-level `daemon`
// is one.
type leaseHolderClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// buildLeaseExclusion loads the agent beads this trigger spawned, reconstructs
// their lease-held intervals, and attributes the pause-residual missed fires to
// the max_concurrent lease. Returns nil when the trigger has no fires diff
// (nothing to attribute). Fail-soft: a daemon error yields an Evaluated=false
// overlay, never an error and never a mutation of the raw fires diff.
//
// The `pause` argument chains the overlays: a missed slot already inside an
// operator pause window is skipped so it is attributed to the pause, not
// double-counted here — leaving lease.ResidualMissed as the single "explained by
// neither" count.
func buildLeaseExclusion(ctx context.Context, client leaseHolderClient, bead *beadsapi.BeadDetail, fires *scheduler.FireReconciliation, pause *pauseExclusion, start, now time.Time) *leaseExclusion {
	if fires == nil {
		return nil // no expected/missed set to attribute (event-only / collapsed window)
	}

	maxConc := parseMaxConcurrent(bead.Fields["max_concurrent"])
	threshold := parseLeaseThreshold(bead.Fields["lease_stale_hours"])

	holders, err := loadLeaseHolders(ctx, client, bead.ID, threshold, start, now)
	if err != nil {
		return &leaseExclusion{Evaluated: false, MaxConcurrent: maxConc, Error: "load lease holders: " + err.Error()}
	}

	// Pause windows already attributed to the operator pause — skip those misses
	// so a slot is explained by exactly one overlay and lease.residual is the
	// final "explained by neither" count.
	var pauseWindows []pauseWindow
	if pause != nil && pause.Evaluated {
		pauseWindows = pause.Windows
	}

	le := &leaseExclusion{Evaluated: true, MaxConcurrent: maxConc}
	// A non-positive cap (max_concurrent<=0) means "no live-lease throttle" — the
	// engine's `spawnsAgent && trig.MaxConcurrent > 0` guard never seeds `fired`,
	// so no fire is ever lease-deferred. Report every pause-residual miss as
	// residual; there is no lease to explain any of them.
	if maxConc <= 0 || len(holders) == 0 {
		le.ResidualMissed = pauseResidualMissCount(fires, pauseWindows)
		return le
	}

	// Attribute each pause-residual missed slot to the lease when the engine
	// would have had >= maxConc live holders at that instant. Collect the holders
	// that actually explain a suppressed miss so Windows stays tight.
	explaining := make(map[string]leaseWindow)
	for _, ef := range fires.Expected {
		if ef.Matched || ef.Pending {
			continue // a matched or not-yet-overdue slot is not a miss to attribute
		}
		if missWithinAnyPause(ef.At, pauseWindows) {
			continue // already explained by an operator pause — attributed there
		}
		covering := holdersAt(ef.At, holders)
		if len(covering) >= maxConc {
			le.SuppressedMissed++
			for _, w := range covering {
				explaining[w.AgentID] = w
			}
		}
	}
	le.ResidualMissed = pauseResidualMissCount(fires, pauseWindows) - le.SuppressedMissed

	for _, w := range explaining {
		le.Windows = append(le.Windows, w)
	}
	sort.Slice(le.Windows, func(i, j int) bool { return le.Windows[i].Start.Before(le.Windows[j].Start) })
	return le
}

// pauseResidualMissCount is the number of missed fires NOT covered by any pause
// window — the denominator the lease overlay attributes against, so lease and
// pause never double-count a slot.
func pauseResidualMissCount(fires *scheduler.FireReconciliation, pauseWindows []pauseWindow) int {
	n := 0
	for _, ef := range fires.Expected {
		if ef.Matched || ef.Pending {
			continue // a matched or not-yet-overdue slot is not a miss to attribute
		}
		if missWithinAnyPause(ef.At, pauseWindows) {
			continue
		}
		n++
	}
	return n
}

// holdersAt returns the lease windows live at instant `at` (inclusive on both
// ends, mirroring missWithinAnyPause): a holder whose interval brackets the
// missed cron slot was occupying the lease when the engine would have evaluated
// that slot.
func holdersAt(at time.Time, holders []leaseWindow) []leaseWindow {
	var out []leaseWindow
	for _, w := range holders {
		if !at.Before(w.Start) && !at.After(w.End) {
			out = append(out, w)
		}
	}
	return out
}

// loadLeaseHolders queries the agent beads this trigger spawned (spawned_by:
// trigger:<id>, including CLOSED holders — this reconstructs history, not the
// live count) and returns their reconstructed lease-held intervals that overlap
// [windowStart, now]. Mirrors bridge.liveSpawnHolders' identity (the spawned_by
// field filter) and its release bounds (leaseHolderStale + leaseHolderOverAgeCeiling).
func loadLeaseHolders(ctx context.Context, client leaseHolderClient, triggerID string, threshold time.Duration, windowStart, now time.Time) ([]leaseWindow, error) {
	res, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred", "closed"},
		Fields:   map[string]string{"spawned_by": "trigger:" + triggerID},
		Sort:     "-created_at",
		Limit:    leaseHolderScanLimit,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}

	var windows []leaseWindow
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		w, ok := leaseWindowFromAgent(b, threshold, now)
		if !ok {
			continue
		}
		// Keep only holders whose lease interval overlaps the audit window: a
		// holder that released its lease before the window opened cannot explain
		// any miss inside it.
		if w.End.Before(windowStart) || w.Start.After(now) {
			continue
		}
		windows = append(windows, w)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Start.Before(windows[j].Start) })
	return windows, nil
}

// leaseWindowFromAgent derives the [Start, End] lease-held interval from a
// spawned agent bead. Start is created_at. End is the EARLIEST of:
//   - the holder's terminal instant: closed_at for a closed agent, else `now`
//     (flagged Open) — the engine only stops counting a holder once it reaches a
//     terminal state or the reconcile snapshot;
//   - the quiet-staleness release updated_at + threshold (leaseHolderStale);
//   - the age-ceiling release created_at + 2×threshold (leaseHolderOverAgeCeiling).
//
// The two staleness caps are what keep a wedged/livelocked holder from crediting
// its ENTIRE runtime as lease-held: past the release bound the engine fires
// again, so misses there are genuine drift, not lease-suppressed. Returns
// ok=false when created_at is unset (an unusable record) so a malformed bead is
// skipped rather than manufacturing a zero-time interval.
func leaseWindowFromAgent(b *beadsapi.BeadDetail, threshold time.Duration, now time.Time) (leaseWindow, bool) {
	if b.CreatedAt.IsZero() {
		return leaseWindow{}, false
	}
	start := b.CreatedAt.UTC()
	w := leaseWindow{AgentID: b.ID, Start: start}

	closed := strings.EqualFold(b.Status, "closed")
	if closed {
		if end := parseReconcileTime(b.ClosedAt); !end.IsZero() {
			w.End = end
		} else if !b.UpdatedAt.IsZero() {
			w.End = b.UpdatedAt.UTC()
		} else {
			w.End = now
		}
	} else {
		w.End = now
		w.Open = true
	}

	// Cap by the two staleness release bounds the engine enforces, so the
	// interval never extends past the instant the engine would have released the
	// lease. A zero updated_at skips the quiet cap (symmetric with
	// leaseHolderStale treating zero updated_at as NOT stale).
	if !b.UpdatedAt.IsZero() {
		if quietRelease := b.UpdatedAt.UTC().Add(threshold); quietRelease.Before(w.End) {
			w.End = quietRelease
		}
	}
	if ceiling := start.Add(leaseAgeCeilingFactor * threshold); ceiling.Before(w.End) {
		w.End = ceiling
	}
	if w.End.Before(w.Start) {
		w.End = w.Start
	}
	return w, true
}

// parseMaxConcurrent parses the trigger's max_concurrent field, defaulting to
// defaultMaxConcurrent (3) for an empty/unparseable value — mirroring
// bridge.triggerParseInt(bead.Fields["max_concurrent"], 3).
func parseMaxConcurrent(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultMaxConcurrent
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultMaxConcurrent
	}
	return n
}

// parseLeaseThreshold parses the optional per-trigger lease_stale_hours override
// into a Duration, defaulting to leaseThresholdDefault (3h) — mirroring
// bridge.leaseStaleHoursField + resolveLeaseStale. A non-positive/unparseable
// value falls back to the default.
func parseLeaseThreshold(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return leaseThresholdDefault
	}
	hours, err := strconv.ParseFloat(raw, 64)
	if err != nil || hours <= 0 {
		return leaseThresholdDefault
	}
	return time.Duration(hours * float64(time.Hour))
}

// printLease renders the max_concurrent lease overlay for a human spot-check.
// No-op when the overlay is absent (nil) or found nothing to attribute. The JSON
// lease block is the machine surface the reconciler agent reads; this is for
// operators eyeballing a report.
func printLease(cmd *cobra.Command, le *leaseExclusion) {
	if le == nil {
		return
	}
	if !le.Evaluated {
		cmd.Printf("  lease: (could not evaluate — %s)\n", le.Error)
		return
	}
	if le.SuppressedMissed == 0 && len(le.Windows) == 0 {
		return // no residual miss coincided with a full max_concurrent lease
	}
	cmd.Printf("  lease: max_concurrent=%d — suppressed_missed=%d residual_missed=%d",
		le.MaxConcurrent, le.SuppressedMissed, le.ResidualMissed)
	if le.ResidualMissed == 0 && le.SuppressedMissed > 0 {
		cmd.Printf("  → a prior fire's agent held the max_concurrent lease across each missed slot; deferred by design, not drift")
	}
	cmd.Println()
	for _, w := range le.Windows {
		openNote := ""
		if w.Open {
			openNote = " (still open)"
		}
		cmd.Printf("    holder %s → %s%s [%s]\n",
			w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339), openNote, w.AgentID)
	}
}

// compile-time proof the production daemon client satisfies the overlay interface.
var _ leaseHolderClient = (*beadsapi.Client)(nil)
