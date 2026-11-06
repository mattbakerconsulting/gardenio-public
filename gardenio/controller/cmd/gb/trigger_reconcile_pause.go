// Package main — operator-pause overlay half of `gb trigger reconcile`.
//
// scheduler.ReconcileFires deliberately computes the UNCONDITIONAL cron diff
// (docs/wake-intent-reconciler.md, "Scope boundary (load-bearing)"): it ignores
// conditions / cooldown / blocked_by / enabled so intent and mechanism stay
// separable. That purity is correct — but it means an operator-initiated
// fleet-wide pause, during which the engine suppresses this trigger's fire path
// BY DESIGN, reads as a run of "missed" fires the reconciler cannot tell apart
// from real gardener-logic drift.
//
// THE FALSE POSITIVE THIS FIXES (ka-0000001687, from ka-0000004707). A
// `scheduler-pause` gate was open fleet-wide 08-09T17:31 → 08-10T23:43 (~30h),
// suppressing EVERY schedule-source cron by design (bridge/triggers.go
// evaluateSource → gate.skipForSchedule → loadGateState reads the open gate).
// The healthy monorepo wedge-watch (`*/30`, ka-0000002184) therefore showed 60
// missed fire-slots strictly inside the pause + 2 pre-pause blips ≈ the 61
// "missed" the reconciler read as "habitual gardener-logic/data-source drift" —
// and a crew Opus was dispatched to fix a phantom. The wedge-watch code was
// never broken; the pause was operator-initiated maintenance.
//
// This file adds a LIVE pause overlay to the report WITHOUT folding the pause
// into ReconcileFires. It loads the operator pause gate beads that overlap the
// audit window (type=gate, title in {triggers-pause, scheduler-pause,
// gardeners-pause}, status open/in_progress/closed), keeps only the ones whose
// gate actually suppresses THIS trigger's fire path, and attributes each missed
// fire that lands inside such a window to the pause that explains it. Fires
// stays byte-for-byte intact (auditable); the overlay adds the deterministic
// `residual_missed` count — MissedCount minus the pause-suppressed misses — that
// the reconciler agent keys on: residual_missed == 0 with suppressed_missed > 0
// is "a maintenance pause explains the whole deficit → IN SYNC", never drift.
//
// This mirrors the additive, fail-soft contract of the condition-gate probe
// (trigger_reconcile_gate.go, MR !6488): any query/parse error leaves the
// overlay Evaluated=false rather than manufacturing — or silently erasing — a
// signal, so a daemon hiccup can never turn a real miss into a phantom "gated"
// no-fire. Unlike that probe (a point-in-time snapshot), a pause is a historical
// wall-clock interval, so it CAN be attributed to a specific past fire — exactly
// as the disable-window clamp in trigger_reconcile.go excludes slots the trigger
// structurally could not have fired.
package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/pausegate"
	"gardenio/controller/internal/scheduler"

	"github.com/spf13/cobra"
)

// triggersPauseGateTitle is the well-known title of the global triggers-pause
// gate bead — the fleet-wide kill-switch that skips EVERY trigger (schedule- or
// event-sourced) via triggerGateState.skip (bridge/triggers.go). Defined here to
// match bridge's unexported `triggersPauseTitle` const; the scheduler-pause and
// gardeners-pause titles come from the shared pausegate package so this read and
// the fire-time read share one source of truth (ka-0000004048).
const triggersPauseGateTitle = "triggers-pause"

// pauseWindow is one operator-initiated pause interval during which this
// trigger's fire path was suppressed by design. [Start, End] is the gate's
// lifetime: Start = created_at, End = closed_at (or the reconcile `now` snapshot
// when the gate is still open). A cron slot landing inside the interval is a
// legitimate gated no-fire, not a miss.
type pauseWindow struct {
	GateID string    `json:"gate_id"`
	Title  string    `json:"title"` // triggers-pause / scheduler-pause / gardeners-pause
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	// Open reports the gate was still open at the reconcile snapshot (End == now),
	// so the window is a lower bound — it may extend past `now`.
	Open bool `json:"open,omitempty"`
}

// pauseExclusion is the operator-pause overlay attached to a reconcileReport. It
// answers the ONE question ReconcileFires cannot: of the reported missed fires,
// how many fell inside an operator pause that suppressed this trigger's fire
// path by design (legitimate gated no-fires), and how many are RESIDUAL — misses
// outside every pause, the only ones that are candidate drift.
type pauseExclusion struct {
	// Windows are the operator pause intervals overlapping the audit window that
	// apply to THIS trigger's fire path, oldest-first. Empty when no applicable
	// pause was open in the window (the common healthy case).
	Windows []pauseWindow `json:"windows,omitempty"`
	// SuppressedMissed is how many of Fires.Missed fell inside an applicable pause
	// window — gated no-fires, not drift.
	SuppressedMissed int `json:"suppressed_missed"`
	// ResidualMissed is Fires.MissedCount - SuppressedMissed: misses NOT covered
	// by any applicable pause. THIS is the count the reconciler classifies as
	// candidate drift. residual_missed == 0 while suppressed_missed > 0 is the
	// deterministic "an operator pause explains the whole deficit → IN SYNC"
	// signal — the exact judgment the reconciler previously had to make by
	// eyeballing gate beads by hand and got wrong on ka-0000004707.
	ResidualMissed int `json:"residual_missed"`
	// Evaluated is false when the pause gates could not be loaded (daemon error):
	// the overlay is then advisory-absent, SuppressedMissed/ResidualMissed are
	// meaningless, and the agent falls back to the raw candidate misses. Explicit
	// so a genuine zero-pause result is never confused with a failed load.
	Evaluated bool `json:"evaluated"`
	// Error carries the fail-soft reason when Evaluated is false.
	Error string `json:"error,omitempty"`
}

// pauseGateClient is the daemon capability the pause overlay needs. *beadsapi.Client
// satisfies it; the reconcile command's package-level `daemon` is one.
type pauseGateClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// pauseGateScanLimit caps the per-title gate scan. Fleet-wide pauses are rare
// operator events (a handful per week at most), so a small cap paired with
// -created_at sort covers every window that could overlap a sane audit window
// without pulling the whole gate history.
const pauseGateScanLimit = 100

// applicablePauseTitles returns the pause gate titles whose open interval would
// have suppressed THIS trigger's fire path, derived from the trigger bead the
// same way the engine routes the gate at fire time:
//
//   - triggers-pause  — ALWAYS: triggerGateState.skip halts every trigger source
//     (skipForSchedule calls skip first), so the global kill-switch suppresses
//     any trigger regardless of source or shape.
//   - scheduler-pause — only schedule-flavored sources (label trigger:schedule or
//     schedule-mirror): skipForSchedule adds the scheduler-pause check on top of
//     skip, exactly the sources scheduleSourceNames gates (bridge/triggers.go).
//   - gardeners-pause  — only gardener-shaped triggers (a non-empty `gardener`
//     field): gardenerPreFireCheck halts gardener firing fleet-wide under an open
//     gardeners-pause gate (bridge/gardener_evaluator.go), orthogonal to source.
//
// Returning only the applicable titles keeps the overlay from over-subtracting —
// e.g. a gardeners-pause that never touched a plain schedule trigger must not
// erase that trigger's genuine misses.
func applicablePauseTitles(bead *beadsapi.BeadDetail) []string {
	titles := []string{triggersPauseGateTitle}

	scheduleFlavored := false
	for _, l := range bead.Labels {
		if l == "trigger:schedule" || l == "schedule-mirror" {
			scheduleFlavored = true
			break
		}
	}
	if scheduleFlavored {
		titles = append(titles, pausegate.SchedulerPauseTitle)
	}
	if strings.TrimSpace(bead.Fields["gardener"]) != "" {
		titles = append(titles, pausegate.GardenersPauseTitle)
	}
	return titles
}

// buildPauseExclusion loads the operator pause windows that overlap [start, now]
// and apply to this trigger, then attributes the report's missed fires to them.
// Returns nil when the trigger has no fires diff (nothing to attribute) so the
// report omits the overlay entirely for event-only / disabled-collapsed
// triggers. Fail-soft: a daemon error yields an Evaluated=false overlay, never
// an error and never a mutation of the raw fires diff.
func buildPauseExclusion(ctx context.Context, client pauseGateClient, bead *beadsapi.BeadDetail, fires *scheduler.FireReconciliation, start, now time.Time) *pauseExclusion {
	if fires == nil {
		return nil // no expected/missed set to attribute (event-only / collapsed window)
	}

	windows, err := loadPauseWindows(ctx, client, applicablePauseTitles(bead), start, now)
	if err != nil {
		return &pauseExclusion{Evaluated: false, Error: "load pause gates: " + err.Error()}
	}

	pe := &pauseExclusion{Evaluated: true, Windows: windows, ResidualMissed: fires.MissedCount}
	if len(windows) == 0 {
		return pe // healthy: no operator pause overlapped the window
	}

	for _, ef := range fires.Expected {
		if ef.Matched || ef.Pending {
			continue // a matched or not-yet-overdue slot is not a miss to attribute
		}
		if missWithinAnyPause(ef.At, windows) {
			pe.SuppressedMissed++
		}
	}
	pe.ResidualMissed = fires.MissedCount - pe.SuppressedMissed
	return pe
}

// missWithinAnyPause reports whether a missed cron slot at `at` falls inside any
// pause window. The interval is inclusive on both ends: a gate open at exactly
// its created_at instant already suppresses that tick, and a slot landing on the
// closed_at instant raced the close and is treated as still-suppressed (the
// engine's ~30s poll cannot resume mid-tick). Precision over tolerance: the
// disable-clamp precedent excludes only slots the trigger structurally could not
// fire, and a real miss just outside a pause must stay a residual candidate.
func missWithinAnyPause(at time.Time, windows []pauseWindow) bool {
	for _, w := range windows {
		if !at.Before(w.Start) && !at.After(w.End) {
			return true
		}
	}
	return false
}

// loadPauseWindows queries the operator pause gate beads for each applicable
// title (open, in_progress, or closed) and returns the intervals that overlap
// [windowStart, now], oldest-first. A closed gate's interval is
// [created_at, closed_at]; an open gate's is [created_at, now] (a lower bound,
// flagged Open). The Search filter is a fuzzy prefilter, so an exact-title match
// makes the match precise — mirrors pausegate.Find / triggers.loadGateState.
func loadPauseWindows(ctx context.Context, client pauseGateClient, titles []string, windowStart, now time.Time) ([]pauseWindow, error) {
	var windows []pauseWindow
	seen := make(map[string]bool)
	for _, title := range titles {
		res, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"gate"},
			Statuses: []string{"open", "in_progress", "closed"},
			Search:   title,
			Sort:     "-created_at",
			Limit:    pauseGateScanLimit,
		})
		if err != nil {
			return nil, err
		}
		if res == nil {
			continue
		}
		for _, b := range res.Beads {
			if b == nil || b.Title != title || seen[b.ID] {
				continue
			}
			w, ok := pauseWindowFromGate(b, now)
			if !ok {
				continue
			}
			// Keep only windows that overlap the audit window: end >= windowStart
			// and start <= now. A gate that opened and closed entirely before the
			// window cannot explain any miss inside it.
			if w.End.Before(windowStart) || w.Start.After(now) {
				continue
			}
			seen[b.ID] = true
			windows = append(windows, w)
		}
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Start.Before(windows[j].Start) })
	return windows, nil
}

// pauseWindowFromGate derives the [Start, End] interval from a pause gate bead.
// Start is created_at; End is closed_at for a closed gate, or `now` for one still
// open. Returns ok=false when created_at is unset (an unusable record) so a
// malformed gate is skipped rather than manufacturing a zero-time window that
// would swallow every miss.
func pauseWindowFromGate(b *beadsapi.BeadDetail, now time.Time) (pauseWindow, bool) {
	start := b.CreatedAt
	if start.IsZero() {
		return pauseWindow{}, false
	}
	w := pauseWindow{GateID: b.ID, Title: b.Title, Start: start.UTC()}

	closed := strings.EqualFold(b.Status, "closed")
	if closed {
		// closed_at is the authoritative end; fall back to updated_at (which for a
		// gate is stamped at close, ~ms after closed_at) if the list projection
		// omitted closed_at, then to `now` as a last resort so a closed gate never
		// reads as open-ended.
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
	if w.End.Before(w.Start) {
		w.End = w.Start
	}
	return w, true
}

// printPause renders the operator-pause overlay for a human spot-check. No-op
// when the overlay is absent (nil). The JSON pause block is the machine surface
// the reconciler agent reads; this is for operators eyeballing a report.
func printPause(cmd *cobra.Command, pe *pauseExclusion) {
	if pe == nil {
		return
	}
	if !pe.Evaluated {
		cmd.Printf("  pause: (could not evaluate — %s)\n", pe.Error)
		return
	}
	if len(pe.Windows) == 0 {
		return // no operator pause overlapped the window — nothing to note
	}
	cmd.Printf("  pause: %d operator pause window(s) overlapped this trigger — suppressed_missed=%d residual_missed=%d",
		len(pe.Windows), pe.SuppressedMissed, pe.ResidualMissed)
	if pe.ResidualMissed == 0 && pe.SuppressedMissed > 0 {
		cmd.Printf("  → the operator pause explains the whole deficit; not drift")
	}
	cmd.Println()
	for _, w := range pe.Windows {
		openNote := ""
		if w.Open {
			openNote = " (still open)"
		}
		cmd.Printf("    %s %s → %s%s [%s]\n",
			w.Title, w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339), openNote, w.GateID)
	}
}

// compile-time proof the production daemon client satisfies the overlay interface.
var _ pauseGateClient = (*beadsapi.Client)(nil)
