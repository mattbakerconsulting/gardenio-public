// Engine-side "dark lane by concurrency suppression" detector (bug ka-0000002023).
//
// This is the sibling of SweepGardenerLiveness (gardener_liveness.go) and
// SweepPouringButNeverWorked (gardener_pouring_stall.go) for a THIRD, orthogonal
// silent-failure mode — the one that dark-ed the gardener-bugfix bug-drain lane
// for 15h with 23 open bugs while every dashboard read healthy:
//
//   - The liveness sweep catches a trigger the engine STOPPED evaluating
//     (last_checked frozen). It cannot see this failure: here last_checked is
//     FRESH — the engine polls the trigger every tick and correctly declines to
//     fire it because its max_concurrent lease is full.
//   - The pouring-stall sweep catches an apply_formula trigger whose poured
//     molecules are never worked. It is about work product, not about firing.
//
// This detector catches a spawn-producing trigger that is ENABLED, actively
// evaluated (last_checked fresh), yet has not FIRED in many cron cadences
// because a live-but-wedged agent pins its concurrency lease. The signal is the
// GAP between the two timestamps: last_run frozen many cadences in the past
// while last_checked keeps advancing. That gap is exactly "enabled but
// suppressed by concurrency for > N consecutive ticks" and nothing surfaced it —
// enabled stayed true, no error was recorded, last_checked read current.
//
// The lease-side age ceiling (ka-0000002023, triggers.go leaseAgeCeilingFactor)
// now auto-releases a livelocked holder after the ceiling, so a wedge self-heals
// within hours. This detector is the always-on VISIBILITY floor for the window
// before that (and for any future wedge the lease bounds cannot release — e.g. a
// genuinely-live long run legitimately holding the slot while the lane's cadence
// makes the operator want to widen max_concurrent): it makes a dark lane LOUD
// rather than silent, so the failure is discoverable in one alert instead of by
// an operator eventually noticing 15h and 23 bugs later.
//
// NEVER spawns. Alerts (notify and/or create_bead) with per-trigger dedup, one
// alert per dark episode, re-armed on recovery — the same discipline as the
// liveness sweep.

package bridge

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler/conditions"
)

// Dark-lane detector tunables.
const (
	// darkLaneStaleFactor is N in "last_run must be at least N× the trigger's
	// cron cadence behind last_checked" before the lane is flagged suppressed.
	// A trigger merely between fires is one cadence behind at most; a lane that
	// has missed this many consecutive scheduled fires while the engine keeps
	// evaluating it is genuinely dark. 4 (≈ four missed :13/:43 fires = ~2h on
	// the gardener-bugfix 30m cadence) gives generous slack for a legitimately
	// long single run holding the slot before it reads as a wedge.
	darkLaneStaleFactor = 4

	// darkLaneMinGap floors the computed gap threshold so a very tight-cadence
	// trigger (e.g. every minute) is not flagged after only a few missed ticks.
	// Aligned with the liveness sweep's min so the two agree on "long enough to
	// be real".
	darkLaneMinGap = time.Hour

	// darkLaneRosterLimit bounds the per-project account list the spend
	// classifier pulls; the trigger roster itself is the shared sweep roster
	// (sweepRosterLimit). This cap only guards a runaway query.
	darkLaneRosterLimit = 500
)

// darkLaneSweepState tracks, per trigger bead ID, whether the sweep has already
// alerted for the current suppression episode. Reset when the lane recovers (it
// fires again, so last_run catches up), so a lane that goes dark, recovers, then
// goes dark again alerts once per episode. The dedup discipline mirrors
// livenessSweepState.
type darkLaneSweepState struct {
	mu   sync.Mutex
	dark map[string]bool // trigger ID → already alerted this suppression episode
}

func newDarkLaneSweepState() *darkLaneSweepState {
	return &darkLaneSweepState{dark: make(map[string]bool)}
}

// markDark records that triggerID is dark and reports whether this is the FIRST
// observation of the current suppression episode (i.e. an alert should fire now).
func (s *darkLaneSweepState) markDark(triggerID string) (firstThisEpisode bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dark[triggerID] {
		return false
	}
	s.dark[triggerID] = true
	return true
}

// markFresh clears any dark-episode flag for triggerID (it recovered).
func (s *darkLaneSweepState) markFresh(triggerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dark, triggerID)
}

// prune drops dark-episode flags for triggers no longer in the roster so the map
// cannot grow unbounded as triggers are closed/deleted.
func (s *darkLaneSweepState) prune(live map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.dark {
		if !live[id] {
			delete(s.dark, id)
		}
	}
}

// SweepDarkLaneByConcurrency performs the concurrency-suppression dark-lane pass
// (bug ka-0000002023). For every ENABLED, OPEN, spawn-producing trigger with a
// max_concurrent cap, it checks whether last_run has fallen darkLaneStaleFactor×
// the trigger's cron cadence behind last_checked WHILE the trigger's concurrency
// lease is full — i.e. the engine is actively evaluating it and declining to
// fire only because the slot is pinned. A match fires a notify and/or
// create_bead alert (never a spawn), deduped per suppression episode.
//
// No-op unless darkLaneEnabled. Safe to call every Evaluate cycle: it is a
// single ListBeadsFiltered plus, for the few triggers that clear the cheap
// timestamp gate, one liveSpawnCount each (itself a single filtered list).
// Errors are logged and downgraded so a daemon hiccup cannot wedge the sweep.
func (e *TriggerEngine) SweepDarkLaneByConcurrency(ctx context.Context) {
	if !e.darkLaneEnabled {
		return
	}

	// Attribute the notice-bead / bug-bead writes both alert paths publish to
	// the machine operator, or the daemon's fail-closed audit resolver 400s
	// them "operator could not be resolved" (ka-0000007257) and the alert never
	// reaches Slack. Mirrors closed-enabled-drain / gardener-epic-reaper. Set
	// once so the fire-alert helpers (which receive this ctx) inherit it.
	ctx = beadsapi.WithSystemOperator(ctx, "dark-lane")

	res, err := e.openTriggerRoster(ctx)
	if err != nil {
		e.logger.Warn("dark-lane sweep: failed to load trigger roster", "error", err)
		return
	}

	now := e.now()
	live := make(map[string]bool, len(res.Beads))
	swept, dark := 0, 0

	for _, bead := range res.Beads {
		if bead == nil {
			continue
		}
		// Only spawn-producing triggers have a max_concurrent lease to be
		// suppressed by. A notify/create_bead-only trigger cannot go dark this way.
		action := TriggerAction(bead.Fields["action"])
		if action != TriggerActionSpawnAgent && action != TriggerActionApplyFormula {
			continue
		}
		// Disabled triggers are expected to be silent — not dark.
		if bead.Fields["enabled"] == "false" {
			continue
		}
		// E2E fixtures are seeded enabled but never fire by design; their
		// last_run is intentionally frozen, so sweeping them would alert forever.
		if isE2EFixture(bead) {
			continue
		}
		// A trigger parked behind an open operator escalation/decision is dormant
		// by design — skip, same as the liveness sweep.
		if bead.Fields["dormant_on_escalation"] == "true" {
			continue
		}

		trig := ParseTriggerDef(bead, "dark-lane")
		// No cap means no concurrency lease to be suppressed by.
		if trig.MaxConcurrent <= 0 {
			continue
		}

		live[bead.ID] = true
		swept++

		lastChecked := parseLivenessTimestamp(bead.Fields["last_checked"])
		lastRun := parseLivenessTimestamp(bead.Fields["last_run"])
		// A trigger the engine has never evaluated (no last_checked) or never
		// fired (no last_run) is not yet dark — it may be brand new, or a
		// bead-query pre-flight may be legitimately keeping it dormant (0 matches).
		// Either way there is no fire-gap to measure. Give it a pass.
		if lastChecked.IsZero() || lastRun.IsZero() {
			continue
		}

		cadence := gardenerCadence(bead, now)
		threshold := darkLaneGapThreshold(cadence)
		gap := lastChecked.Sub(lastRun)

		// Fresh enough: last_run is within the allowed number of cadences of
		// last_checked. Clear any prior dark flag so a future episode re-alerts.
		if gap <= threshold {
			e.darkLaneState.markFresh(bead.ID)
			continue
		}

		// The timestamp gate is cheap; the lease check is one more filtered list,
		// so only run it for the handful of triggers that look suppressed. The
		// lane is only DARK-by-concurrency if the lease is actually full — a lane
		// that simply has nothing to fire (empty match set) also shows a stale
		// last_run but is NOT suppressed, and must not alert. We fetch the HOLDER
		// beads (not just the count) so the spend-exhaustion classifier below can
		// inspect their agent_state — one query, shared with liveSpawnCount.
		holders := e.liveSpawnHolders(ctx, trig)
		liveHolders := len(holders)
		if liveHolders < trig.MaxConcurrent {
			// Not lease-bound: the lease is not full, so this is NOT concurrency
			// suppression. The stale last_run is either a genuinely-empty match
			// set (legitimately idle — not a signal) OR a bead_query pre-flight
			// whose exclude subtractions floor the effective count to 0 while
			// matching beads really exist (condition-suppressed dark, bug
			// ka-0000001127 — exactly how the ka-0000006656 self-subtraction bug
			// stayed silent for 2 days). Only probe the EMPTY-lease case (nothing
			// spawned): a condition-suppressed lane fires nobody, so it holds no
			// lease. The cheap raw-vs-effective probe below distinguishes the two.
			if liveHolders == 0 {
				if cond := e.classifyConditionSuppressed(ctx, bead, trig); cond != nil {
					dark++
					if !e.darkLaneState.markDark(bead.ID) {
						continue // already alerted this episode; stay quiet.
					}
					e.logger.Warn("dark-lane sweep: trigger SUPPRESSED by pre-flight condition",
						"trigger", bead.ID, "title", bead.Title,
						"last_run", lastRun.UTC().Format(time.RFC3339),
						"last_checked", lastChecked.UTC().Format(time.RFC3339),
						"gap", gap.String(), "cadence", cadence.String(),
						"gap_threshold", threshold.String(),
						"raw_lane_count", cond.rawCount, "effective_preflight_count", cond.effectiveCount)
					e.fireConditionDarkLaneAlert(ctx, bead, lastRun, lastChecked, gap, cadence, cond)
					continue
				}
			}
			e.darkLaneState.markFresh(bead.ID)
			continue
		}

		dark++
		if !e.darkLaneState.markDark(bead.ID) {
			continue // already alerted this episode; stay quiet.
		}

		// Distinguish the SPEND-EXHAUSTION sub-case (bug ka-0000001246): the lane
		// is not merely "a busy holder pinning the slot" but a holder parked in a
		// credential/spend error state (rate_limited / budget_exceeded) because its
		// account hit its monthly spend limit. That failure is invisible to every
		// updated_at check (SyncAll churns updated_at fresh) AND misdiagnosed by the
		// plain concurrency alert, whose remediation (`gb agent kill` the holder) is
		// actively WRONG here — a replacement re-wedges on the same exhausted
		// account. When a pinning holder is in that state, resolve the account's
		// spend numbers and fire the spend-aware alert instead.
		spend := e.classifySpendExhaustedHolder(ctx, bead, holders, now)

		e.logger.Warn("dark-lane sweep: trigger SUPPRESSED by concurrency",
			"trigger", bead.ID, "title", bead.Title,
			"last_run", lastRun.UTC().Format(time.RFC3339),
			"last_checked", lastChecked.UTC().Format(time.RFC3339),
			"gap", gap.String(), "cadence", cadence.String(),
			"gap_threshold", threshold.String(),
			"live_holders", liveHolders, "max_concurrent", trig.MaxConcurrent,
			"spend_exhausted", spend != nil)

		e.fireDarkLaneAlert(ctx, bead, lastRun, lastChecked, gap, cadence, threshold, liveHolders, trig.MaxConcurrent, spend)
	}

	e.darkLaneState.prune(live)

	e.logger.Info("dark-lane sweep: complete", "triggers_swept", swept, "dark", dark)
}

// darkLaneGapThreshold computes how far last_run may fall behind last_checked
// before the lane is flagged suppressed: darkLaneStaleFactor× the cadence,
// floored at darkLaneMinGap.
func darkLaneGapThreshold(cadence time.Duration) time.Duration {
	threshold := cadence * darkLaneStaleFactor
	if threshold < darkLaneMinGap {
		return darkLaneMinGap
	}
	return threshold
}

// fireDarkLaneAlert emits the alert for a concurrency-suppressed dark lane. It
// mirrors fireLivenessAlert's notify/create_bead fan-out and NEVER spawns.
//
// When spend is non-nil the lane is dark specifically because a pinning holder's
// account exhausted its monthly spend limit (bug ka-0000001246): the alert names
// the spend_limit / current_spend / days_remaining and gives the CORRECT
// remediation (the account/model config must change; killing the holder just
// respawns a pod that re-wedges on the same exhausted account). Otherwise it is
// the plain concurrency alert (bug ka-0000002023).
func (e *TriggerEngine) fireDarkLaneAlert(ctx context.Context, bead *beadsapi.BeadDetail, lastRun, lastChecked time.Time, gap, cadence, threshold time.Duration, liveHolders, maxConcurrent int, spend *spendExhaustion) {
	var msg string
	if spend != nil {
		msg = fmt.Sprintf(
			"🚨 dark-lane (SPEND-EXHAUSTED): %s (%s) is ENABLED but has gone DARK — last_run %s ago while last_checked is current (gap %s ≥ %d× cadence %s). "+
				"Its max_concurrent=%d lease is pinned by holder %s parked in agent_state=%s: account %q is over its monthly spend limit (%s), so every turn 429s and the pod backoff-loops forever (bug ka-0000001246). "+
				"This is INVISIBLE to updated_at checks (SyncAll churns updated_at fresh). Do NOT `gb agent kill` the holder — a replacement re-wedges on the same exhausted account; the account/model budget or fallback config must change (raise the limit, wait for reset, or add a healthy fallback model group).",
			bead.Title, bead.ID,
			gap.Round(time.Minute), gap.Round(time.Minute), darkLaneStaleFactor, cadence.Round(time.Minute),
			maxConcurrent, spend.holderID, spend.agentState, spend.accountName, spend.summary(),
		)
	} else {
		msg = fmt.Sprintf(
			"🚨 dark-lane: %s (%s) is ENABLED but SUPPRESSED BY CONCURRENCY — last_run %s ago while last_checked is current (gap %s ≥ %d× cadence %s). "+
				"Its max_concurrent=%d lease is full (%d live holder(s)) so the engine polls it every tick and declines to fire. "+
				"A live-but-wedged holder pinning the slot is the usual cause (bug ka-0000002023). Check the holder(s); the lease age-ceiling auto-releases a livelocked one, or `gb agent kill` it to free the slot now.",
			bead.Title, bead.ID,
			gap.Round(time.Minute), gap.Round(time.Minute), darkLaneStaleFactor, cadence.Round(time.Minute),
			maxConcurrent, liveHolders,
		)
	}

	if e.darkLaneNotify && e.notifier != nil {
		e.notifier(WithNoticeProject(ctx, noticeBeadProject(bead)), "dark-lane", msg, "")
	}

	if e.darkLaneCreateBead {
		desc := fmt.Sprintf("%s\n\nlast_run: %s\nlast_checked: %s",
			msg, lastRun.UTC().Format(time.RFC3339), lastChecked.UTC().Format(time.RFC3339))
		if spend != nil {
			desc += "\n\n" + spend.detail()
		}
		e.createDarkLaneBead(ctx, bead, desc)
	}
}

// createDarkLaneBead files a bug bead recording the suppressed lane, parented to
// the trigger so the alert is navigable from it in beads-viewer. A failure is
// logged but not fatal — the notify leg may already have alerted. Mirrors
// createLivenessBead.
func (e *TriggerEngine) createDarkLaneBead(ctx context.Context, trigger *beadsapi.BeadDetail, desc string) {
	title := fmt.Sprintf("Lane DARK (concurrency-suppressed): %s", trigger.Title)

	labels := []string{
		"liveness",
		"lane-dark",
		"area:triggers",
		"source:dark-lane-sweep",
	}
	if proj := trigger.Fields["project"]; proj != "" {
		labels = append(labels, "project:"+proj)
	}

	req := beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "bug",
		Description: desc,
		Labels:      labels,
		Priority:    1,
		CreatedBy:   "dark-lane-sweep",
	}

	beadID, err := e.daemon.CreateBead(ctx, req)
	if err != nil {
		e.logger.Warn("dark-lane sweep: failed to create dark-lane bead",
			"trigger", trigger.ID, "error", err)
		return
	}

	if err := e.daemon.AddDependency(ctx, beadID, trigger.ID, "parent-child", "dark-lane-sweep"); err != nil {
		e.logger.Warn("dark-lane sweep: failed to parent dark-lane bead to trigger",
			"trigger", trigger.ID, "bead", beadID, "error", err)
	}

	e.logger.Info("dark-lane sweep: filed dark-lane bead", "trigger", trigger.ID, "bead", beadID)
}

// --- condition-suppressed dark lane (bug ka-0000001127) ---
//
// The concurrency path above only fires when the max_concurrent lease is FULL.
// A lane that goes dark because its bead_query PRE-FLIGHT CONDITION is stuck at
// 0 while matching beads really exist has fresh last_checked, stale last_run,
// and an EMPTY lease — it trips the `liveHolders < MaxConcurrent` early-out and
// would alert nobody. That is exactly how the ka-0000006656
// exclude_blocked_by_open_types self-subtraction bug stayed silent for 2 days.
//
// The detector deliberately treats an empty match set as not-a-signal (most
// idle lanes legitimately have 0 matches; alerting on every idle lane would be
// pure noise). So the signal is NOT "any idle lane" — it is the DIVERGENCE
// between the effective pre-flight count (with the exclude subtractions) and the
// raw lane (without them): effective is 0/not-met while the raw lane WOULD fire.
// Only an exclude_labels / exclude_blocked_by_open_types subtraction can create
// that gap, so a pre-flight with neither can never be condition-suppressed and
// is skipped. A legitimately-closed root epic short-circuits BOTH counts to 0
// (the raw leaf keeps the Parent filter), so it reads as idle, not suppressed.

// conditionSuppression is the resolved raw-vs-effective picture for a lane that
// is dark because its bead_query pre-flight subtracts its way to 0 while the
// raw lane is non-empty. rawCount is the match count ignoring the excludes;
// effectiveCount is the same query AFTER the excludes/closed-root — the number
// the engine actually gates the fire on.
type conditionSuppression struct {
	rawCount       int    // matching beads ignoring exclude subtractions
	effectiveCount int    // the pre-flight count the engine fires on (with excludes)
	op             string // the gating comparison op (e.g. ">")
	value          string // the gating comparison value (e.g. "0")

	excludeLabels             []string // exclude_labels on the gating condition, if any
	excludeBlockedByOpenTypes []string // exclude_blocked_by_open_types, if any
}

// classifyConditionSuppressed runs the cheap raw-vs-effective probe for an
// empty-lease trigger whose last_run has fallen behind. It returns a non-nil
// conditionSuppression only when the trigger's bead_query pre-flight is
// suppressing a NON-EMPTY lane: the effective (subtracted) count does not fire
// while the raw (excludes-stripped) count would. Returns nil — no alert — in
// every other case, including on any query error (fail-soft), so a daemon
// hiccup can never manufacture a false dark-lane signal.
func (e *TriggerEngine) classifyConditionSuppressed(ctx context.Context, bead *beadsapi.BeadDetail, trig TriggerDef) *conditionSuppression {
	raw := strings.TrimSpace(bead.Fields["conditions"])
	if raw == "" {
		// No pre-flight gate — the stale last_run is a genuinely empty match set,
		// not condition suppression.
		return nil
	}
	conds, err := conditions.ParseConditions(raw)
	if err != nil || len(conds) == 0 {
		return nil
	}

	for _, c := range conds {
		// Only a bead_query with an exclude subtraction can make the raw lane
		// diverge from the effective count. A count-only or exclude-free
		// condition has raw == effective and can never be suppressed this way.
		if c.Type != "bead_query" {
			continue
		}
		if len(c.ExcludeLabels) == 0 && len(c.ExcludeBlockedByOpenTypes) == 0 {
			continue
		}

		leaf := c.ToExpression().Leaf
		effCount, err := conditions.EvaluateBeadQueryCount(ctx, e.daemon, leaf, trig.Project)
		if err != nil {
			e.logger.Warn("dark-lane sweep: effective pre-flight count failed; skipping condition probe (fail-soft)",
				"trigger", bead.ID, "error", err)
			return nil
		}
		// If the effective pre-flight WOULD fire, the lane is not suppressed —
		// its stale last_run is a transient (work just appeared, fires next
		// cron tick), not a dark lane.
		if conditions.CompareCount(effCount, c.Op, c.Value) {
			return nil
		}

		// Re-count the RAW lane: the same query with the exclude subtractions
		// stripped (Parent and every other filter preserved, so a closed root
		// still short-circuits both counts to 0 and reads as idle, not dark).
		rawLeaf := leaf
		rawLeaf.ExcludeLabels = nil
		rawLeaf.ExcludeBlockedByOpenTypes = nil
		rawCount, err := conditions.EvaluateBeadQueryCount(ctx, e.daemon, rawLeaf, trig.Project)
		if err != nil {
			e.logger.Warn("dark-lane sweep: raw pre-flight count failed; skipping condition probe (fail-soft)",
				"trigger", bead.ID, "error", err)
			return nil
		}
		// Raw lane also would not fire → genuinely idle/empty → not our signal.
		if !conditions.CompareCount(rawCount, c.Op, c.Value) {
			return nil
		}

		// Effective pre-flight is 0/not-met while the raw lane WOULD fire: the
		// exclude subtractions are hiding real work — condition-suppressed dark.
		return &conditionSuppression{
			rawCount:                  rawCount,
			effectiveCount:            effCount,
			op:                        c.Op,
			value:                     c.Value,
			excludeLabels:             c.ExcludeLabels,
			excludeBlockedByOpenTypes: c.ExcludeBlockedByOpenTypes,
		}
	}
	return nil
}

// fireConditionDarkLaneAlert emits the alert for a condition-suppressed dark
// lane. It mirrors fireDarkLaneAlert's notify/create_bead fan-out and NEVER
// spawns, but names the raw-vs-effective counts and the exclude subtractions
// so the operator can go straight to the pre-flight gate rather than hunting a
// wedged holder that does not exist.
func (e *TriggerEngine) fireConditionDarkLaneAlert(ctx context.Context, bead *beadsapi.BeadDetail, lastRun, lastChecked time.Time, gap, cadence time.Duration, cond *conditionSuppression) {
	subtractions := conditionSubtractionSummary(cond)
	msg := fmt.Sprintf(
		"🚨 dark-lane (CONDITION-SUPPRESSED): %s (%s) is ENABLED but has gone DARK — last_run %s ago while last_checked is current (gap %s ≥ %d× cadence %s), and its concurrency lease is EMPTY. "+
			"Its bead_query pre-flight fires on `%s %s` but subtracts its way to an effective count of %d while %d matching bead(s) really exist (raw lane, subtractions ignored). "+
			"The %s are hiding real work, so the engine polls every tick and correctly declines to fire — no wedged holder to kill, this is the ka-0000006656 self-subtraction class (bug ka-0000001127). "+
			"Check the pre-flight condition's exclude filters against the raw lane.",
		bead.Title, bead.ID,
		gap.Round(time.Minute), gap.Round(time.Minute), darkLaneStaleFactor, cadence.Round(time.Minute),
		cond.op, cond.value, cond.effectiveCount, cond.rawCount,
		subtractions,
	)

	if e.darkLaneNotify && e.notifier != nil {
		e.notifier(WithNoticeProject(ctx, noticeBeadProject(bead)), "dark-lane", msg, "")
	}

	if e.darkLaneCreateBead {
		desc := fmt.Sprintf("%s\n\nlast_run: %s\nlast_checked: %s\nraw_lane_count: %d\neffective_preflight_count: %d\ngating_op: %s %s\nexclude_labels: %s\nexclude_blocked_by_open_types: %s",
			msg, lastRun.UTC().Format(time.RFC3339), lastChecked.UTC().Format(time.RFC3339),
			cond.rawCount, cond.effectiveCount, cond.op, cond.value,
			strings.Join(cond.excludeLabels, ","), strings.Join(cond.excludeBlockedByOpenTypes, ","))
		e.createConditionDarkLaneBead(ctx, bead, desc)
	}
}

// conditionSubtractionSummary renders which exclude subtraction(s) are hiding
// the raw lane, for the one-line notify.
func conditionSubtractionSummary(cond *conditionSuppression) string {
	var parts []string
	if len(cond.excludeBlockedByOpenTypes) > 0 {
		parts = append(parts, "exclude_blocked_by_open_types="+strings.Join(cond.excludeBlockedByOpenTypes, ","))
	}
	if len(cond.excludeLabels) > 0 {
		parts = append(parts, "exclude_labels="+strings.Join(cond.excludeLabels, ","))
	}
	if len(parts) == 0 {
		return "pre-flight exclude subtractions"
	}
	return strings.Join(parts, " + ")
}

// createConditionDarkLaneBead files a bug bead recording the condition-suppressed
// lane, parented to the trigger. Mirrors createDarkLaneBead but carries a
// distinct title/label so a condition-suppressed episode is navigable and
// filterable apart from the concurrency-suppressed one.
func (e *TriggerEngine) createConditionDarkLaneBead(ctx context.Context, trigger *beadsapi.BeadDetail, desc string) {
	title := fmt.Sprintf("Lane DARK (condition-suppressed): %s", trigger.Title)

	labels := []string{
		"liveness",
		"lane-dark",
		"lane-dark-condition",
		"area:triggers",
		"source:dark-lane-sweep",
	}
	if proj := trigger.Fields["project"]; proj != "" {
		labels = append(labels, "project:"+proj)
	}

	req := beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "bug",
		Description: desc,
		Labels:      labels,
		Priority:    1,
		CreatedBy:   "dark-lane-sweep",
	}

	beadID, err := e.daemon.CreateBead(ctx, req)
	if err != nil {
		e.logger.Warn("dark-lane sweep: failed to create condition-dark-lane bead",
			"trigger", trigger.ID, "error", err)
		return
	}

	if err := e.daemon.AddDependency(ctx, beadID, trigger.ID, "parent-child", "dark-lane-sweep"); err != nil {
		e.logger.Warn("dark-lane sweep: failed to parent condition-dark-lane bead to trigger",
			"trigger", trigger.ID, "bead", beadID, "error", err)
	}

	e.logger.Info("dark-lane sweep: filed condition-dark-lane bead", "trigger", trigger.ID, "bead", beadID)
}

// darkLaneSpendErrorStates are the agent_state values that mean a holder is
// pinning its lease because the account's credentials/budget cannot serve it —
// the exact states the credential-wedge sweep (trigger_credential_wedge.go)
// keys on, and the states an agent lands in when its account hits the monthly
// spend limit. A holder here is the spend-exhaustion signature, not a busy run.
var darkLaneSpendErrorStates = map[string]bool{
	"rate_limited":    true,
	"budget_exceeded": true,
}

// spendExhaustion is the resolved spend picture for a dark lane pinned by a
// spend-exhausted holder (bug ka-0000001246). The three headline numbers the
// acceptance requires — spend_limit, current_spend, days_remaining — are
// pointer-typed so an honestly-UNKNOWN value (the account carries no synced
// limit/reset, which is the common case when the ceiling is Anthropic-side and
// opaque) renders as "unknown" rather than a fabricated zero.
type spendExhaustion struct {
	holderID    string // agent bead pinning the lease
	agentState  string // rate_limited | budget_exceeded
	accountName string // resolved account, or "(unknown)" when unresolved

	spendLimitUSD   *float64 // account monthly spend limit (max_budget), if synced
	currentSpendUSD *float64 // spend so far this billing period, if synced
	daysRemaining   *float64 // days until the budget/throttle window resets, if known
}

// summary renders the three headline numbers inline for the one-line notify.
func (s *spendExhaustion) summary() string {
	return fmt.Sprintf("spend_limit=%s, current_spend=%s, days_remaining=%s",
		fmtUSDPtr(s.spendLimitUSD), fmtUSDPtr(s.currentSpendUSD), fmtDaysPtr(s.daysRemaining))
}

// detail renders the labelled spend block appended to the create_bead description.
func (s *spendExhaustion) detail() string {
	return fmt.Sprintf(
		"spend_exhausted: true\nholder: %s\nagent_state: %s\naccount: %s\nspend_limit_usd: %s\ncurrent_spend_usd: %s\ndays_remaining: %s",
		s.holderID, s.agentState, s.accountName,
		fmtUSDPtr(s.spendLimitUSD), fmtUSDPtr(s.currentSpendUSD), fmtDaysPtr(s.daysRemaining))
}

// classifySpendExhaustedHolder returns a resolved spendExhaustion when at least
// one holder pinning the trigger's lease is parked in a credential/spend error
// state (rate_limited / budget_exceeded), else nil (the plain concurrency case).
//
// It picks the FIRST such holder as representative (a max_concurrent lane is
// almost always 1, so there is exactly one), resolves the account it was billed
// against, and reads the spend numbers off the account bead best-effort. All
// resolution is fail-soft: an unresolved account or unsynced limit degrades to
// "unknown" in the alert rather than suppressing it — the dark lane is real
// regardless of how much spend telemetry we can attach.
func (e *TriggerEngine) classifySpendExhaustedHolder(ctx context.Context, trigger *beadsapi.BeadDetail, holders []*beadsapi.BeadDetail, now time.Time) *spendExhaustion {
	var holder *beadsapi.BeadDetail
	for _, h := range holders {
		if h != nil && darkLaneSpendErrorStates[h.Fields["agent_state"]] {
			holder = h
			break
		}
	}
	if holder == nil {
		return nil // no spend-error holder — plain concurrency suppression.
	}

	se := &spendExhaustion{
		holderID:    holder.ID,
		agentState:  holder.Fields["agent_state"],
		accountName: "(unknown)",
	}

	acct := e.resolveHolderAccount(ctx, trigger, holder)
	if acct == nil {
		return se // dark + spend-exhausted, but we could not resolve the account.
	}
	se.accountName = acct.Title
	e.fillSpendFromAccount(se, acct, now)
	return se
}

// resolveHolderAccount finds the account bead the wedged holder was billed
// against — but ONLY when it can do so with confidence. It returns nil (caller
// degrades to "unknown", no numbers) rather than GUESS, because naming the wrong
// account and surfacing its healthy spend as the cause is worse than admitting we
// don't know. The three confident cases, in order:
//
//  1. Direct link: the holder's `account` field names an account in the pool.
//  2. A single project account: unambiguous.
//  3. Exactly one project account looks spend-exhausted (status/throttle/limit).
//
// If several accounts look exhausted, or several exist and none looks exhausted
// (the incident shape — a model-group-specific wall leaves every account reading
// pool-healthy), it is ambiguous and we return nil. Any read error or empty pool
// also yields nil.
func (e *TriggerEngine) resolveHolderAccount(ctx context.Context, trigger, holder *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	project := firstNonEmpty(holder.Fields["project"], trigger.Fields["project"])

	accounts := e.listProjectAccounts(ctx, project)
	if len(accounts) == 0 {
		return nil
	}

	// (1) Direct link: the holder names its account.
	if name := strings.TrimSpace(holder.Fields["account"]); name != "" {
		for _, a := range accounts {
			if a.Title == name {
				return a
			}
		}
	}

	// (2) Single project account ⇒ unambiguous.
	if len(accounts) == 1 {
		return accounts[0]
	}

	// (3) Several accounts: only attribute if EXACTLY ONE looks spend-exhausted,
	// so the numbers we surface match the wall the holder hit. Zero or multiple
	// matches is ambiguous — do not guess.
	var exhausted *beadsapi.BeadDetail
	for _, a := range accounts {
		if accountLooksExhausted(a) {
			if exhausted != nil {
				return nil // more than one candidate — ambiguous.
			}
			exhausted = a
		}
	}
	return exhausted // the sole exhausted-looking account, or nil if none.
}

// listProjectAccounts returns the open account beads labeled for the project.
// Fail-soft: a nil/empty result on any error (the caller degrades gracefully).
func (e *TriggerEngine) listProjectAccounts(ctx context.Context, project string) []*beadsapi.BeadDetail {
	if project == "" {
		return nil
	}
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Labels:   []string{"project:" + project},
		Limit:    darkLaneRosterLimit,
	})
	if err != nil {
		e.logger.Warn("dark-lane sweep: failed to list project accounts",
			"project", project, "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	return res.Beads
}

// fillSpendFromAccount reads the spend numbers off the account bead onto se,
// leaving any field it cannot source honestly nil (rendered "unknown"):
//
//   - spend_limit  ← spend_limit_usd (LiteLLM max_budget, synced by spend_sync),
//     falling back to the operator-set max_budget hint.
//   - current_spend ← total_cost_usd minus spend_at_last_reset (the billing-period
//     spend, the same figure `gb usage` prints), floored at 0.
//   - days_remaining ← (spend_reset_at | throttle_reset_at) − now, in days.
func (e *TriggerEngine) fillSpendFromAccount(se *spendExhaustion, acct *beadsapi.BeadDetail, now time.Time) {
	// spend_limit: the synced LiteLLM budget wins; the operator max_budget hint is
	// the fallback. Only a positive value is a real limit.
	if v, ok := parsePositiveFloat(acct.Fields["spend_limit_usd"]); ok {
		se.spendLimitUSD = &v
	} else if v, ok := parsePositiveFloat(acct.Fields["max_budget"]); ok {
		se.spendLimitUSD = &v
	}

	// current_spend: billing-period spend = total − snapshot-at-last-reset.
	if raw := strings.TrimSpace(acct.Fields["total_cost_usd"]); raw != "" {
		if total, err := strconv.ParseFloat(raw, 64); err == nil {
			spent := total
			if snap, err := strconv.ParseFloat(strings.TrimSpace(acct.Fields["spend_at_last_reset"]), 64); err == nil {
				spent = total - snap
			}
			if spent < 0 {
				spent = 0
			}
			se.currentSpendUSD = &spent
		}
	}

	// days_remaining: prefer the budget reset instant; fall back to the throttle
	// reset the rate-limit probe writes. Past/zero → 0 days (reset is due).
	resetAt := firstNonEmptyField(acct.Fields, "spend_reset_at", "throttle_reset_at")
	if d, ok := daysUntil(resetAt, now); ok {
		se.daysRemaining = &d
	}
}

// accountLooksExhausted reports whether an account bead shows a capacity-spent
// signal: a lifecycle status of exhausted, a rejected throttle status, or synced
// spend at/over its limit. Used to pick the representative account when a project
// has several. Conservative — any one signal is enough.
func accountLooksExhausted(a *beadsapi.BeadDetail) bool {
	if a == nil {
		return false
	}
	if a.Fields["status"] == "exhausted" {
		return true
	}
	if ts := a.Fields["throttle_status"]; ts == "rejected" {
		return true
	}
	limit, hasLimit := parsePositiveFloat(a.Fields["spend_limit_usd"])
	if !hasLimit {
		limit, hasLimit = parsePositiveFloat(a.Fields["max_budget"])
	}
	if hasLimit {
		if total, err := strconv.ParseFloat(strings.TrimSpace(a.Fields["total_cost_usd"]), 64); err == nil {
			spent := total
			if snap, err := strconv.ParseFloat(strings.TrimSpace(a.Fields["spend_at_last_reset"]), 64); err == nil {
				spent = total - snap
			}
			if spent >= limit {
				return true
			}
		}
	}
	return false
}

// daysUntil parses an RFC3339 instant and returns the days from now until it,
// floored at 0 (a past/zero instant means the window is due to reset). ok=false
// when the string is empty or unparseable so the caller leaves days_remaining
// unknown rather than reporting a fabricated 0.
func daysUntil(resetAt string, now time.Time) (float64, bool) {
	reset, err := time.Parse(time.RFC3339, strings.TrimSpace(resetAt))
	if err != nil {
		return 0, false
	}
	d := reset.Sub(now).Hours() / 24
	if d < 0 {
		d = 0
	}
	return d, true
}

// parsePositiveFloat parses s as a float64 and reports ok only for a positive,
// finite value — an empty, malformed, zero, or negative field is "no value".
func parsePositiveFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// firstNonEmpty returns the first argument that is non-empty after trimming.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// fmtUSDPtr renders a nullable dollar amount: "$12.34" or "unknown".
func fmtUSDPtr(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("$%.2f", *v)
}

// fmtDaysPtr renders a nullable day count: "3.2d" or "unknown".
func fmtDaysPtr(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1fd", *v)
}
