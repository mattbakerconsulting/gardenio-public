// Package main — time_window overlay half of `gb trigger reconcile`.
//
// scheduler.ReconcileFires deliberately computes the UNCONDITIONAL cron diff
// (docs/wake-intent-reconciler.md, "Scope boundary (load-bearing)"): it ignores
// conditions / cooldown / blocked_by / enabled so intent and mechanism stay
// separable. That purity is correct — but it means a trigger carrying a
// `time_window` quiet-hours condition (op="outside") reports every cron slot
// that landed INSIDE the quiet window as a "missed" fire, when the engine
// suppressed each one BY DESIGN.
//
// THE FALSE POSITIVE THIS FIXES (ka-0000005666, `habitual` drift on the gardenio
// gardener-release-owner v4 ka-0000003719). The release gardener fires `13,43 * * *
// *` (America/Los_Angeles) gated by two conditions: a bead_query lane gate AND a
// `time_window` outside 07:00-18:00 Mon-Fri PT — the release "quiet hours" policy
// (ka-0000004693). Over a 168h window it fired 40/54 expected slots; all 14-15
// "missed" slots fell strictly inside a weekday 07:00-18:00 PT quiet window, i.e.
// exactly the fires the time_window condition exists to suppress. Not drift — the
// quiet-hours gate working as designed.
//
// WHY THE POINT-IN-TIME GATE WAS NOT ENOUGH. The gate probe
// (trigger_reconcile_gate.go) already evaluates the trigger's conditions,
// INCLUDING the time_window leaf, against the current wall clock — but only as a
// point-in-time `would_fire_now` snapshot. That makes the drift-candidate count
// NON-DETERMINISTIC for a time_window-gated trigger: run the reconciler DURING
// quiet hours and would_fire_now=false zeros the residual; run it OUTSIDE quiet
// hours (a nights-and-weekends reconciler pass, or any of the 13h/day the window
// is open) and would_fire_now=true, so the historical quiet-hours misses stand as
// phantom drift and re-file the habitual bug. A pause/lease-style HISTORICAL
// overlay attributes each missed slot to the quiet window it actually fell in,
// independent of WHEN the reconciler runs — the same fix pattern that killed the
// operator-pause (ka-0000001687) and max_concurrent-lease (ka-0000007902) false
// positives.
//
// This file adds a LIVE time_window overlay to the report WITHOUT folding the
// window into ReconcileFires. It parses the trigger's `conditions`, keeps the
// time_window leaves, and attributes each missed cron slot NOT already explained
// by an operator pause or the max_concurrent lease to a time_window leaf that
// suppressed firing at that instant (evaluated at the slot's own wall clock via
// conditions.EvalTimeWindowLeafAt). Fires stays byte-for-byte intact; the overlay
// adds the deterministic `residual_missed` count — the pause→lease residual minus
// the time_window-suppressed misses — that the drift-candidate composition keys
// on. residual_missed == 0 while suppressed_missed > 0 is "the trigger's own
// quiet-hours window explains the whole deficit → IN SYNC", never drift.
//
// Chained onto the pause AND lease overlays: a missed slot already attributed to
// an operator pause or the concurrency lease is skipped here, so a slot is
// explained by exactly one overlay and timeWindow.ResidualMissed is the single
// count of misses explained by NONE of pause / lease / time_window. This mirrors
// the additive, fail-soft contract of the sibling overlays: any parse error
// leaves the overlay Evaluated=false rather than manufacturing — or silently
// erasing — a signal.
package main

import (
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"

	"github.com/spf13/cobra"
)

// timeWindowLeaf is one time_window condition on the trigger that suppressed at
// least one missed fire, surfaced for auditability (which window explained the
// deficit). Timezone/Window/Days/Op mirror the leaf's config.
type timeWindowLeaf struct {
	Timezone string   `json:"timezone,omitempty"`
	Window   string   `json:"window"`
	Days     []string `json:"days,omitempty"`
	Op       string   `json:"op"`
	// SuppressedMissed is how many of the pause/lease-residual misses this leaf
	// alone explained (a slot at whose wall clock the leaf declined to fire).
	SuppressedMissed int `json:"suppressed_missed"`
}

// timeWindowExclusion is the time_window overlay attached to a reconcileReport.
// It answers the ONE question ReconcileFires cannot deterministically and the
// point-in-time gate cannot answer for a PAST slot: of the missed fires NOT
// already explained by an operator pause or the max_concurrent lease, how many
// fell at an instant the trigger's own time_window condition suppressed firing
// (legitimate quiet-hours no-fires), and how many are RESIDUAL — misses explained
// by none of pause / lease / time_window, the only genuine-drift candidates.
type timeWindowExclusion struct {
	// Leaves are the time_window conditions that actually explained a suppressed
	// miss, so the report stays tight (a leaf that suppressed nothing is omitted).
	Leaves []timeWindowLeaf `json:"leaves,omitempty"`
	// SuppressedMissed is how many pause/lease-residual misses fell at an instant
	// a time_window leaf declined to fire — legitimate quiet-hours no-fires.
	SuppressedMissed int `json:"suppressed_missed"`
	// ResidualMissed is the pause/lease residual miss count minus SuppressedMissed:
	// misses explained by NEITHER a pause NOR the lease NOR a time_window. THIS is
	// the count the reconciler classifies as candidate drift. residual_missed == 0
	// while suppressed_missed > 0 is the deterministic "the trigger's own
	// quiet-hours window explains the whole deficit → IN SYNC" signal — the exact
	// judgment the point-in-time gate could only make when sampled INSIDE the
	// window (ka-0000005666).
	ResidualMissed int `json:"residual_missed"`
	// SuppressedNow reports whether a time_window leaf is declining to fire AT
	// THE RECONCILE INSTANT — i.e. the trigger is inside a quiet-hours window
	// right now. It is a FORWARD-looking signal, orthogonal to the historical
	// SuppressedMissed/ResidualMissed pair: a lane can be perfectly healthy
	// (residual_missed==0) AND gated shut for the next ten hours, both at once.
	// A liveness probe that keys only on residual_missed cannot tell those apart
	// (bug ka-0000008105: gardener-bugfix scored a quiet-hours-suppressed release
	// owner as live and skipped dispatch on a release-blocker P0 for ~11h).
	SuppressedNow bool `json:"suppressed_now"`
	// SuppressedUntil is the RFC3339 (UTC) instant at which the CURRENT
	// suppression lifts — the earliest minute from now at which every
	// time_window leaf allows firing again. Set only when SuppressedNow is true,
	// and only when that exit falls inside suppressionProbeHorizon. An EMPTY
	// SuppressedUntil with SuppressedNow==true therefore means "suppressed past
	// the probe horizon" — the DARK direction, which is the fail-safe a coverage
	// probe wants (cover the lane rather than assume it is about to fire).
	SuppressedUntil string `json:"suppressed_until,omitempty"`
	// SuppressedMinutesRemaining is SuppressedUntil expressed as whole minutes
	// from the reconcile instant, so a consumer can apply a horizon
	// ("will this lane fire within the next hour?") without re-parsing a
	// timestamp in jq. Zero when the trigger is not currently suppressed or the
	// exit is beyond the probe horizon.
	SuppressedMinutesRemaining int `json:"suppressed_minutes_remaining,omitempty"`
	// Evaluated is false when the conditions could not be parsed: the overlay is
	// then advisory-absent and the drift composition falls back to the lease
	// residual. Explicit so a genuine zero-window result is never confused with a
	// failed parse.
	Evaluated bool `json:"evaluated"`
	// Error carries the fail-soft reason when Evaluated is false.
	Error string `json:"error,omitempty"`
}

// buildTimeWindowExclusion parses the trigger's time_window conditions and
// attributes the pause/lease-residual missed fires to the window(s) that
// suppressed firing at each slot. Returns nil when the trigger has no fires diff
// (nothing to attribute) or carries no conditions at all. Fail-soft: a parse
// error yields an Evaluated=false overlay, never an error and never a mutation of
// the raw fires diff.
//
// BOTH label regimes are covered (ka-0000006385). A trigger's time_window gate can
// live in TWO places, and which one the engine actually honours depends on the
// trigger SHAPE — exactly the split ScoreGardenerCronGating dispatches on:
//   - trigger:schedule — the flat `conditions` array is the preflight
//     (trigger_source_schedule.go is its one non-test call site).
//   - trigger:multi — the flat `conditions` field is INERT for firing; the engine
//     gates each cron entry on `triggers[i].preflight` and each event entry on
//     `triggers[i].match` (scheduler/triggers.go cronPreflightHolds / MultiTriggerSource).
//
// The audit must see the window wherever it fires, so this collects time_window
// leaves from the flat `conditions` array AND from every `triggers[i].preflight`
// / `triggers[i].match` expression, walking all/any/not compositions (the gate is
// almost always nested under the bead_query `all`, not a top-level leaf), and
// UNIONs them — a leaf that appears in both regimes (as on ka-0000003719, where
// the flat conditions were deliberately kept as a duplicate of the cron preflight)
// is counted once. Before this, the overlay parsed only the flat field, so a multi
// gardener that keeps its quiet-hours gate in `triggers[].preflight` reported every
// by-design quiet-hours suppression as residual drift — false-flagging the one
// number (residual_missed) gardeners read to detect a genuinely dark lane.
//
// The `pause` and `lease` arguments chain the overlays: a missed slot already
// inside an operator pause window or covered by a full max_concurrent lease is
// skipped so it is attributed to that overlay, not double-counted here — leaving
// timeWindow.ResidualMissed as the single "explained by none" count.
func buildTimeWindowExclusion(conditionsRaw, triggersRaw string, fires *scheduler.FireReconciliation, pause *pauseExclusion, lease *leaseExclusion, now time.Time) *timeWindowExclusion {
	if fires == nil {
		return nil // no expected/missed set to attribute (event-only / collapsed window)
	}
	condsRaw := strings.TrimSpace(conditionsRaw)
	trigRaw := strings.TrimSpace(triggersRaw)
	if condsRaw == "" && trigRaw == "" {
		return nil // unconditional trigger (neither regime) — no time_window to explain a miss
	}

	// Collect the time_window leaves from BOTH label regimes, deduped by their
	// (op, window, days, timezone) identity so a leaf duplicated across the flat
	// conditions and a triggers[] preflight is attributed once. A trigger with no
	// time_window anywhere carries no quiet-hours gate — the overlay adds no signal
	// (pause/lease residual passes through unchanged).
	var leaves []conditions.Leaf
	seen := make(map[string]bool)
	addLeaf := func(l conditions.Leaf) {
		if l.Type != "time_window" {
			return
		}
		key := timeWindowLeafKey(l)
		if seen[key] {
			return
		}
		seen[key] = true
		leaves = append(leaves, l)
	}

	// 1. Flat conditions[] — the trigger:schedule preflight (and, on a multi bead,
	// any deliberately-kept duplicate of the per-entry gate).
	if condsRaw != "" {
		conds, err := conditions.ParseConditions(condsRaw)
		if err != nil {
			return &timeWindowExclusion{Evaluated: false, Error: "parse conditions: " + err.Error()}
		}
		for _, c := range conds {
			addLeaf(c.ToExpression().Leaf)
		}
	}

	// 2. triggers[].preflight / triggers[].match — where a trigger:multi gardener's
	// gate actually fires. Walk the full all/any/not tree of each expression, not
	// just its top-level leaf: the quiet-hours leaf is virtually always AND'd under
	// a bead_query lane gate ({"all":[{bead_query...},{time_window...}]}).
	if trigRaw != "" {
		entries, err := scheduler.ParseTriggers(trigRaw)
		if err != nil {
			return &timeWindowExclusion{Evaluated: false, Error: "parse triggers: " + err.Error()}
		}
		for i := range entries {
			for _, expr := range []*conditions.Expression{entries[i].Preflight, entries[i].Match} {
				if expr == nil {
					continue
				}
				collectTimeWindowLeaves(*expr, addLeaf)
			}
		}
	}

	// Denominator: misses NOT already explained by an operator pause or the lease,
	// so a slot is attributed to exactly one overlay.
	var pauseWindows []pauseWindow
	if pause != nil && pause.Evaluated {
		pauseWindows = pause.Windows
	}
	residualBefore := residualMissesBefore(fires, pauseWindows, lease)

	tw := &timeWindowExclusion{Evaluated: true, ResidualMissed: residualBefore}
	// Forward-looking suppression state, computed from the FULL leaf set (not
	// just the leaves that happened to explain a historical miss) so it is
	// correct even on a window that suppressed nothing inside the reconcile
	// window. See applyLiveSuppression for why this is a separate signal.
	applyLiveSuppression(tw, leaves, now)
	if len(leaves) == 0 {
		return tw // no time_window condition — nothing to attribute
	}

	// Attribute each still-residual missed slot to the FIRST time_window leaf that
	// declined to fire at that instant. First-match (not all-match) keeps
	// suppressed_missed a true slot count even when two overlapping windows both
	// cover a slot.
	perLeaf := make([]int, len(leaves))
	for _, ef := range fires.Expected {
		if ef.Matched || ef.Pending {
			continue // a matched or not-yet-overdue slot is not a miss to attribute
		}
		if missWithinAnyPause(ef.At, pauseWindows) {
			continue // attributed to the operator pause
		}
		if missWithinLease(ef.At, lease) {
			continue // attributed to the max_concurrent lease
		}
		for i, leaf := range leaves {
			fireAllowed, evErr := conditions.EvalTimeWindowLeafAt(leaf, ef.At)
			if evErr != nil {
				continue // a malformed leaf explains nothing — fail-soft
			}
			if !fireAllowed {
				tw.SuppressedMissed++
				perLeaf[i]++
				break
			}
		}
	}
	tw.ResidualMissed = residualBefore - tw.SuppressedMissed

	for i, leaf := range leaves {
		if perLeaf[i] == 0 {
			continue // this window explained no miss — omit to keep the report tight
		}
		tw.Leaves = append(tw.Leaves, timeWindowLeaf{
			Timezone:         leaf.Timezone,
			Window:           leaf.Window,
			Days:             leaf.Days,
			Op:               leaf.Op,
			SuppressedMissed: perLeaf[i],
		})
	}
	sort.Slice(tw.Leaves, func(i, j int) bool { return tw.Leaves[i].Window < tw.Leaves[j].Window })
	return tw
}

// suppressionProbeHorizon bounds the forward scan applyLiveSuppression runs to
// find when the CURRENT quiet-hours suppression lifts. 48h covers every daily or
// weekday window (the release lane's 07:00-18:00 Mon-Fri PT gate exits within
// ~11h at worst) with room to spare, and caps the scan at 2880 minute-steps so a
// pathological window — one that suppresses for a whole weekend, or forever —
// cannot turn a CLI reconcile into a multi-second spin. Past the horizon the
// overlay reports SuppressedNow=true with an EMPTY SuppressedUntil, which reads
// as "suppressed indefinitely" — the safe direction for a coverage probe.
const suppressionProbeHorizon = 48 * time.Hour

// applyLiveSuppression fills the forward-looking SuppressedNow / SuppressedUntil
// / SuppressedMinutesRemaining fields: is a time_window leaf gating this trigger
// shut RIGHT NOW, and if so, when does that gate open again?
//
// WHY THIS EXISTS SEPARATELY FROM residual_missed (bug ka-0000008105). The
// historical overlay answers "is the trigger firing as designed?" — and answers
// it correctly: a quiet-hours-gated trigger reports residual_missed==0 because
// every miss is explained. But a consumer asking "will this lane act on my
// problem soon?" needs a DIFFERENT fact, and residual_missed cannot supply it at
// any threshold. The gardenio gardener-bugfix lane conflated the two: its Step-3
// release-owner liveness probe scored `enabled==true and residual_missed==0` as
// LIVE, so a release owner sitting ~10.5h into a PT-business-hours suppression
// read as healthy-and-covering and the lane skipped dispatch on a prod-bricked
// P0 for the whole window. Healthy and about-to-fire are not the same claim; this
// pair of fields is the second one, so no consumer has to re-derive cron+window
// arithmetic client-side from data the reconcile response already holds.
//
// Semantics match the engine's: conditions are AND-combined, so firing is
// suppressed when ANY leaf declines, and the suppression lifts at the first
// minute NO leaf declines. Leaves unioned across both label regimes (flat
// conditions and triggers[].preflight/match) are treated as one AND-set here,
// exactly as the historical attribution treats them; for a multi trigger whose
// entries carry different windows that is conservative — it can only report
// suppression the engine might not apply to every entry, which biases a coverage
// probe toward covering. A malformed leaf is skipped (fail-soft), matching the
// attribution loop: a leaf that cannot be evaluated explains nothing and
// suppresses nothing.
func applyLiveSuppression(tw *timeWindowExclusion, leaves []conditions.Leaf, now time.Time) {
	if !timeWindowSuppressesAt(leaves, now) {
		return // firing is allowed right now — no suppression to report
	}
	tw.SuppressedNow = true
	for step := time.Minute; step <= suppressionProbeHorizon; step += time.Minute {
		at := now.Add(step)
		if timeWindowSuppressesAt(leaves, at) {
			continue
		}
		tw.SuppressedUntil = at.UTC().Format(time.RFC3339)
		tw.SuppressedMinutesRemaining = int(step / time.Minute)
		return
	}
	// No exit inside the horizon: SuppressedUntil stays empty on purpose.
}

// timeWindowSuppressesAt reports whether ANY time_window leaf declines to fire at
// instant `at` — the AND-combine the engine applies to a trigger's conditions. A
// leaf that fails to evaluate is skipped rather than treated as suppressing, so a
// mis-configured window never manufactures a phantom quiet-hours hold.
func timeWindowSuppressesAt(leaves []conditions.Leaf, at time.Time) bool {
	for _, leaf := range leaves {
		fireAllowed, err := conditions.EvalTimeWindowLeafAt(leaf, at)
		if err != nil {
			continue // fail-soft: a malformed leaf explains nothing
		}
		if !fireAllowed {
			return true
		}
	}
	return false
}

// timeWindowLeafKey is the dedupe identity of a time_window leaf: two leaves that
// gate the same clock range, on the same weekdays, in the same zone, with the same
// op suppress firing at exactly the same instants, so they are one gate no matter
// how many regimes (flat conditions, triggers[].preflight, triggers[].match) carry
// them. Days are joined verbatim (not sorted): a leaf's Days order is author-fixed
// and EvalTimeWindowLeafAt treats it as a set, so identical windows always share a
// day list byte-for-byte; the only cost of an incidental reorder is attributing the
// same real window twice, which over-omits nothing and never manufactures a miss.
func timeWindowLeafKey(l conditions.Leaf) string {
	return strings.Join([]string{l.Op, l.Window, l.Timezone, strings.Join(l.Days, ",")}, "\x00")
}

// collectTimeWindowLeaves walks an expression's full all/any/not composition tree
// and invokes emit for every time_window leaf it finds. The recursion is what lets
// the overlay see a quiet-hours gate nested under a boolean composition — the common
// shape, since the window is virtually always AND'd beneath a bead_query lane gate
// rather than sitting as a bare top-level leaf. A well-formed expression is either a
// composition or a leaf (Expression.IsComposition is exclusive with IsLeaf), so a
// composition node's own embedded Leaf fields are never a real leaf and are correctly
// not emitted. emit still receives only time_window leaves; the addLeaf dedupe caller
// keeps its own type guard because the flat-conditions path feeds it every condition.
func collectTimeWindowLeaves(expr conditions.Expression, emit func(conditions.Leaf)) {
	if expr.IsComposition() {
		for i := range expr.All {
			collectTimeWindowLeaves(expr.All[i], emit)
		}
		for i := range expr.Any {
			collectTimeWindowLeaves(expr.Any[i], emit)
		}
		if expr.Not != nil {
			collectTimeWindowLeaves(*expr.Not, emit)
		}
		return
	}
	if expr.Leaf.Type == "time_window" {
		emit(expr.Leaf)
	}
}

// residualMissesBefore counts the missed fires NOT already explained by an
// operator pause or the max_concurrent lease — the denominator the time_window
// overlay attributes against, so the three historical overlays never double-count
// a slot. Mirrors pauseResidualMissCount, then subtracts the lease-attributed
// slots exactly as the lease overlay itself did.
func residualMissesBefore(fires *scheduler.FireReconciliation, pauseWindows []pauseWindow, lease *leaseExclusion) int {
	n := 0
	for _, ef := range fires.Expected {
		if ef.Matched || ef.Pending {
			continue // a matched or not-yet-overdue slot is not a miss to attribute
		}
		if missWithinAnyPause(ef.At, pauseWindows) {
			continue
		}
		if missWithinLease(ef.At, lease) {
			continue
		}
		n++
	}
	return n
}

// missWithinLease reports whether a missed slot at `at` was covered by a full
// max_concurrent lease — i.e. the lease overlay already attributed it. Reuses the
// lease overlay's own holder intervals so the two agree exactly on which slots
// the lease explained. Returns false when the lease overlay is absent or did not
// evaluate (nothing attributed there yet).
func missWithinLease(at time.Time, lease *leaseExclusion) bool {
	if lease == nil || !lease.Evaluated || lease.MaxConcurrent <= 0 {
		return false
	}
	if len(holdersAt(at, lease.Windows)) >= lease.MaxConcurrent {
		return true
	}
	return false
}

// printTimeWindow renders the time_window overlay for a human spot-check. No-op
// when the overlay is absent (nil) or explained nothing. The JSON timeWindow
// block is the machine surface the reconciler agent reads; this is for operators
// eyeballing a report.
func printTimeWindow(cmd *cobra.Command, tw *timeWindowExclusion) {
	if tw == nil {
		return
	}
	if !tw.Evaluated {
		cmd.Printf("  time_window: (could not evaluate — %s)\n", tw.Error)
		return
	}
	if tw.SuppressedMissed == 0 && len(tw.Leaves) == 0 && !tw.SuppressedNow {
		return // no missed slot fell inside a quiet-hours window, and none is open now
	}
	if tw.SuppressedNow {
		if tw.SuppressedUntil != "" {
			cmd.Printf("  time_window: SUPPRESSED NOW — quiet-hours gate closed for another %dm (reopens %s)\n",
				tw.SuppressedMinutesRemaining, tw.SuppressedUntil)
		} else {
			cmd.Printf("  time_window: SUPPRESSED NOW — quiet-hours gate closed beyond the %s probe horizon\n",
				suppressionProbeHorizon)
		}
	}
	if tw.SuppressedMissed == 0 && len(tw.Leaves) == 0 {
		return // live suppression reported above; no historical misses to attribute
	}
	cmd.Printf("  time_window: suppressed_missed=%d residual_missed=%d", tw.SuppressedMissed, tw.ResidualMissed)
	if tw.ResidualMissed == 0 && tw.SuppressedMissed > 0 {
		cmd.Printf("  → each missed slot fell inside the trigger's own quiet-hours window; suppressed by design, not drift")
	}
	cmd.Println()
	for _, l := range tw.Leaves {
		daysNote := ""
		if len(l.Days) > 0 {
			daysNote = " " + strings.Join(l.Days, ",")
		}
		cmd.Printf("    %s %s%s [%s]: suppressed_missed=%d\n", l.Op, l.Window, daysNote, l.Timezone, l.SuppressedMissed)
	}
}
