// Engine-side gardener-liveness heartbeat invariant (B2, ka-0000001541).
//
// A DARK gardener — one that has silently stopped emitting cycles (dropped by the
// scheduler, wedged, or otherwise gone quiet while still OPEN and enabled) —
// emits zero cycles and zero failures, so the old FAILED-cycle watchdog never
// noticed. The 5/24–5/26 incident disabled the entire janitorial fleet for a
// week, invisibly.
//
// SCOPE BOUNDARY (read before "fixing" the open-only roster query below). This
// sweep watches OPEN + enabled gardeners only. Two other dark classes are
// DELIBERATELY out of scope, each owned by throttled, human-gated machinery that
// staleness-alerting here would only duplicate or flood:
//
//   - OPEN + enabled=false (operator- or audit-disabled): the deliberate-off
//     case. Alerting would page on a gardener someone turned off on purpose. The
//     B3 disable-transition signal (ka-0000004185) covers the OFF edge, and
//     trigger-reviver (ka-0000006342) surveys the open+disabled backlog.
//   - CLOSED + enabled=true (self-closed on a stop condition — max_runs /
//     no_progress_for / wall_clock / consecutive_failures — while the operator
//     never disabled it): the intake-groomer blind spot from bug ka-0000004633.
//     This sweep MUST NOT own it: a closed gardener's last_checked is frozen at
//     its final cycle, so EVERY closed+enabled trigger reads permanently stale —
//     ~130+ of them fleet-wide, most legitimately-retired gardener lineages.
//     Staleness cannot tell an accidentally-dark lane from a retired one, so a
//     naive widen here floods. This class is instead drained by two coordinated
//     mechanisms (bug ka-0000002082 — the reviver's 1/cycle human-gated decision
//     alone could not keep up with a 137-deep pool, so the operator queue grew
//     unboundedly): (a) the engine-side SweepClosedEnabledDrain
//     (closed_enabled_drain.go) deterministically auto-archives the retired
//     lineages (gardener-epic CLOSED, capped + audited + reversible) and BATCHES
//     the ambiguous unanchored cohort into ONE grouped archive decision per
//     outstanding batch; (b) trigger-reviver (ka-0000006342) then spends its
//     scarce per-cycle revival-review budget on the genuinely-uncovered OPEN
//     lanes the engine deliberately never touches, leaving the reopen call to a
//     human — together draining the backlog without a flood.
//
// `last_checked` is the canonical liveness signal (B1, ka-0000002275): the
// engine stamps it on every `evalOne` exit (triggers.go), so the field freezes
// the instant the engine stops seeing a trigger. It is a usable dead-man's
// switch — but until now *nothing watched the watcher*.
//
// SweepGardenerLiveness is that watcher. It runs once per Evaluate cycle, inside
// the engine that already iterates the full roster every cycle (so it cannot
// itself go dark the way a single watched gardener can — an engine-down is infra
// liveness, a separate concern). For each ENABLED gardener trigger it asserts
// `last_checked` is within N× the gardener's expected cadence. On violation it
// emits a liveness alert (notify and/or create_bead) — with NO agent spawn,
// zero tokens. Per-gardener dedup means one alert per dark episode, re-armed
// when the gardener recovers.
//
// This is the in-engine heartbeat the R0 audit (ka-0000000980 §3.4) names as
// the real B2: the freshness operator (`older_than`) and notify-on-violation
// action already shipped; what was missing is the component that sweeps the
// roster and asserts the invariant. Per design doc ka-0000007560, the
// fleet-specific declarative watch list (B4 CEL) layers on top later; this
// engine pass is the always-on floor for an OPEN gardener going dark by any path
// that bypasses the declarative triggers — the closed/disabled dark classes are
// owned by trigger-reviver, per the SCOPE BOUNDARY above.

package bridge

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// Liveness sweep tunables.
const (
	// livenessDefaultStaleFactor is N in "last_checked must be within N×
	// cadence". A gardener that has missed this many expected cycles in a row is
	// considered dark. 3 gives a gardener two missed cycles of slack (transient
	// engine hiccup, a slow daemon round-trip) before alerting, so the sweep
	// does not page on a single skipped tick.
	livenessDefaultStaleFactor = 3

	// livenessDefaultCadence is the assumed cadence for a gardener trigger that
	// carries no cron expression (e.g. an event-driven trigger:multi gardener).
	// Such a gardener is still stamped last_checked every engine poll, so the
	// engine poll interval — not a cron period — bounds its freshness. 2h
	// matches the janitorial fleet's typical cron and keeps the floor cheap;
	// a gardener with a faster cron gets a tighter bound from its own cadence.
	livenessDefaultCadence = 2 * time.Hour

	// livenessMinStaleThreshold floors the computed stale threshold so a
	// gardener with a very tight cron (e.g. every minute) is not flagged on a
	// single slow poll. The engine poll interval is ~30s; 15m of slack absorbs
	// a handful of skipped polls without masking a genuinely dark gardener.
	livenessMinStaleThreshold = 15 * time.Minute

	// livenessSystemOperator is the X-System-Operator source label both liveness
	// sweeps stamp on the trigger-notice message bead the notifier publishes.
	// These sweeps are TRUE machine-origin cron writes with no human behind
	// them, so without a resolvable operator the daemon's fail-closed audit
	// resolver 400s the notice CreateBead with "operator could not be resolved"
	// (ka-0000007257) — the notice bead is never created, slack-bridge never
	// sees it, and the DARK alert is detected but silently never delivered.
	// WithSystemOperator sends X-System-Operator, which the daemon honors on the
	// engine's static-primary token and collapses into the single system:machine
	// sink under the authoring-human gate — the sanctioned attribution for cron
	// sweeps, mirroring closed-enabled-drain / gardener-epic-reaper. It is
	// last-resort on the daemon, so any per-call human operator still wins. Must
	// satisfy sanitizeSystemOperatorHeader: ASCII lowercase / digits / '-'.
	livenessSystemOperator = "gardener-liveness"
)

// livenessSweepState tracks, per gardener bead ID, whether the sweep has already
// alerted for the current dark episode. It is reset when the gardener recovers
// (last_checked goes fresh again), so a gardener that goes dark, recovers, then
// goes dark again alerts once per episode rather than once ever or every cycle.
type livenessSweepState struct {
	mu        sync.Mutex
	darkSince map[string]bool // gardener ID → already alerted this dark episode
}

func newLivenessSweepState() *livenessSweepState {
	return &livenessSweepState{darkSince: make(map[string]bool)}
}

// markDark records that gardenerID is dark and reports whether this is the FIRST
// observation of the current dark episode (i.e. an alert should fire now).
func (s *livenessSweepState) markDark(gardenerID string) (firstThisEpisode bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.darkSince[gardenerID] {
		return false
	}
	s.darkSince[gardenerID] = true
	return true
}

// markFresh clears any dark-episode flag for gardenerID (it recovered). A
// subsequent dark episode will alert again.
func (s *livenessSweepState) markFresh(gardenerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.darkSince, gardenerID)
}

// prune drops dark-episode flags for gardeners no longer in the roster so the
// map cannot grow unbounded as gardeners are closed/deleted over time.
func (s *livenessSweepState) prune(live map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.darkSince {
		if !live[id] {
			delete(s.darkSince, id)
		}
	}
}

// gardenerCadence returns the expected time between consecutive cycles for a
// gardener trigger. A cron expression yields the gap between its two most recent
// fire times; a gardener without a cron falls back to livenessDefaultCadence.
// Returns a non-zero duration always (fail-safe: a malformed cron uses the
// default rather than a zero cadence that would make every gardener look dark).
func gardenerCadence(bead *beadsapi.BeadDetail, now time.Time) time.Duration {
	cronExpr := bead.Fields["cron"]
	if cronExpr == "" {
		return livenessDefaultCadence
	}
	expr, err := scheduler.ParseCron(cronExpr)
	if err != nil {
		return livenessDefaultCadence
	}
	// Gap between the two most recent scheduled fire times = the cron period at
	// `now`. Robust to irregular crons (e.g. business-hours-only) because it
	// measures the local cadence rather than assuming a fixed interval.
	prev := expr.Prev(now)
	if prev.IsZero() {
		return livenessDefaultCadence
	}
	prevPrev := expr.Prev(prev.Add(-time.Minute))
	if prevPrev.IsZero() {
		return livenessDefaultCadence
	}
	cadence := prev.Sub(prevPrev)
	if cadence <= 0 {
		return livenessDefaultCadence
	}
	return cadence
}

// livenessStaleThreshold computes how stale `last_checked` may be before a
// gardener is considered dark: N× its cadence, floored at
// livenessMinStaleThreshold.
func livenessStaleThreshold(cadence time.Duration) time.Duration {
	threshold := cadence * livenessDefaultStaleFactor
	if threshold < livenessMinStaleThreshold {
		return livenessMinStaleThreshold
	}
	return threshold
}

// isGardenerTrigger reports whether a trigger bead is a gardener-shape (carries a
// non-empty `gardener` block). Only gardeners are swept: a plain source-only
// trigger has no liveness contract (it fires reactively and may legitimately
// sit idle for days), whereas a gardener is expected to drive its loop on a
// cadence and going dark is the failure the 5/24–5/26 incident is about.
func isGardenerTrigger(bead *beadsapi.BeadDetail) bool {
	return bead.Fields["gardener"] != ""
}

// SweepGardenerLiveness performs the engine-side heartbeat invariant pass (B2).
// For every ENABLED, OPEN gardener trigger it asserts `last_checked` is within
// N× the gardener's cadence; a violation fires a liveness alert (notify and/or
// create_bead per the engine's liveness config) with NO agent spawn. Dedup is
// per dark episode.
//
// Safe to call every Evaluate cycle: it is a single ListBeadsFiltered plus
// in-memory arithmetic, and it never spawns. Errors are logged and downgraded
// so a daemon hiccup cannot wedge the sweep.
func (e *TriggerEngine) SweepGardenerLiveness(ctx context.Context) {
	if !e.livenessEnabled {
		return
	}

	// Attribute the notice-bead write this sweep's alert publishes to the
	// machine operator, or the daemon 400s it "operator could not be resolved"
	// and the DARK alert never reaches Slack (see livenessSystemOperator).
	ctx = beadsapi.WithSystemOperator(ctx, livenessSystemOperator)

	// Statuses:["open"] is INTENTIONAL, not an oversight — see the SCOPE BOUNDARY
	// in this file's header. Closed+enabled gardeners have a frozen last_checked so
	// they would ALL read stale (~130+ fleet-wide, most retired lineages); that
	// class is drained instead by SweepClosedEnabledDrain (closed_enabled_drain.go,
	// bug ka-0000002082) + trigger-reviver (ka-0000006342) — the engine sweep
	// auto-archives the retired lineages and batches the ambiguous cohort, freeing
	// the reviver's per-cycle budget for the genuinely-uncovered OPEN lanes.
	// Widening this query to closed would flood. Locked by
	// TestLivenessSweep_SkipsClosedGardener.
	res, err := e.openTriggerRoster(ctx)
	if err != nil {
		e.logger.Warn("liveness sweep: failed to load gardener roster", "error", err)
		return
	}

	now := time.Now()
	live := make(map[string]bool, len(res.Beads))
	swept, dark := 0, 0

	for _, bead := range res.Beads {
		if bead == nil {
			continue
		}
		// Only gardeners carry a liveness contract.
		if !isGardenerTrigger(bead) {
			continue
		}
		// Disabled gardeners are expected to be silent — not dark. (B3
		// ka-0000004185 owns the separate "alert on the disable transition"
		// signal; here we must not false-positive on a deliberately-off
		// gardener.)
		if bead.Fields["enabled"] == "false" {
			continue
		}
		// E2E fixtures are seeded enabled into the prod daemon but never fire
		// (ka-0000005435); their last_checked is intentionally frozen, so
		// sweeping them would alert forever.
		if isE2EFixture(bead) {
			continue
		}
		// A gardener parked behind an open operator escalation/decision is
		// dormant by design (bug ka-0000003082): the engine suppresses its fire
		// but it is not dark. Skip — alerting here would page on a gardener that
		// is correctly waiting on a human.
		if bead.Fields["dormant_on_escalation"] == "true" {
			continue
		}

		live[bead.ID] = true
		swept++

		lastChecked := parseLivenessTimestamp(bead.Fields["last_checked"])
		// A gardener the engine has never evaluated (last_checked unset) is not
		// yet dark — it may have just been created this cycle. The bootstrap
		// path stamps last_checked on first sight; until then, give it a pass.
		if lastChecked.IsZero() {
			continue
		}

		// CRITICAL: the sweep must only flag gardeners the engine has STOPPED
		// evaluating — not gardeners the engine is evaluating but legitimately
		// choosing not to fire. Two evalOne gates pause a gardener WITHOUT
		// stamping last_checked (by design, so the gardener resumes instantly on
		// unblock), and would otherwise freeze last_checked while the gardener is
		// perfectly alive:
		//
		//   1. blocked_by an OPEN decision/escalation (triggers.go evalOne).
		//   2. one_spawn_at_a_time with a still-running agent.
		//   3. max_concurrent_runs: the single-live-instance default (a gardener
		//      deferring while a prior run is still spawning/working — bug
		//      ka-0000003097). This is the general form of #2 and also freezes
		//      last_checked without the gardener being dark.
		//
		// Skip all here by reusing the engine's own gate helpers, so the
		// sweep's skip set tracks the no-stamp set exactly. (dormant_on_escalation
		// above is the fourth such gate, short-circuited cheaply on the field.)
		trig := ParseTriggerDef(bead, "liveness")
		if len(e.triggerBlockedBy(ctx, trig)) > 0 {
			continue
		}
		if trig.Gardener != nil && trig.Gardener.OneSpawnAtATime && trig.Gardener.LastAgentID != "" &&
			gardenerAgentStillRunning(ctx, e.daemon, e.logger, trig.Gardener.LastAgentID, trig.LeaseStaleAfter, now) {
			continue
		}
		if gardenerAtConcurrentRunsCap(ctx, e.daemon, e.logger, trig, now) {
			continue
		}

		cadence := gardenerCadence(bead, now)
		threshold := livenessStaleThreshold(cadence)
		age := now.Sub(lastChecked)

		if age <= threshold {
			// Fresh — clear any prior dark flag so a future episode re-alerts.
			e.livenessState.markFresh(bead.ID)
			continue
		}

		dark++
		if !e.livenessState.markDark(bead.ID) {
			// Already alerted for this dark episode; stay quiet.
			continue
		}

		e.logger.Warn("liveness sweep: gardener is DARK",
			"gardener", bead.ID, "title", bead.Title,
			"last_checked", lastChecked.UTC().Format(time.RFC3339),
			"age", age.String(), "cadence", cadence.String(),
			"stale_threshold", threshold.String())

		e.fireLivenessAlert(ctx, bead, lastChecked, age, cadence, threshold)
	}

	e.livenessState.prune(live)

	e.logger.Info("liveness sweep: complete",
		"gardeners_swept", swept, "dark", dark)
}

// sanitizeWatchIDs trims, de-blanks, and de-duplicates an operator-supplied
// gardener watch-id allow-list (order-preserving). Returns nil for an
// all-empty input so the all-status pass is cheaply a no-op.
func sanitizeWatchIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SweepGardenerLivenessAllowList is the steady-state poll that closes the
// transition-only blind spot (bug ka-0000004451). For each operator-configured
// watched gardener id it asserts — across ALL statuses, every cycle — the same
// liveness invariant the declarative assert ka-0000002431 checks on transitions:
// the gardener must be OPEN, not disabled, and freshly evaluated. Unlike the main
// SweepGardenerLiveness (open-only roster) and unlike the event-driven assert
// (which sees a gardener exactly once, on its close/disable transition, and never
// again — so a single missed SSE/reconciler event leaves it dark forever), this
// pass re-reads the explicit id list every cycle regardless of any event. That
// is the poll-every-cycle parity the residual task ka-0000000484 named as the
// only path that truly matches the retired guard ka-0000006629.
//
// It is safe to widen to closed/disabled HERE — where the main sweep must not —
// precisely because the id list is a small, explicit, operator-curated allow-
// list, not the ~130-deep fleet-wide closed+enabled pool the main sweep's SCOPE
// BOUNDARY excludes to avoid a flood. A batch fetch plus in-memory checks; never
// spawns. Dedup is per dark episode in a DEDICATED state map (livenessAllowListState,
// NOT the main sweep's) so the two passes' prune cycles never clobber each other.
// No-op unless livenessEnabled and the allow-list is non-empty.
func (e *TriggerEngine) SweepGardenerLivenessAllowList(ctx context.Context) {
	if !e.livenessEnabled || len(e.livenessWatchIDs) == 0 {
		return
	}

	// Attribute the notice-bead write this sweep's alert publishes to the
	// machine operator, or the daemon 400s it "operator could not be resolved"
	// and the DARK alert never reaches Slack (see livenessSystemOperator).
	ctx = beadsapi.WithSystemOperator(ctx, livenessSystemOperator)

	beads, err := e.daemon.BatchGetBeads(ctx, e.livenessWatchIDs)
	if err != nil {
		e.logger.Warn("liveness allow-list sweep: batch fetch failed",
			"ids", e.livenessWatchIDs, "error", err)
		return
	}

	now := time.Now()
	checked, dark := 0, 0
	// The set of ids this pass tracks, so prune drops dark flags for ids the
	// operator removed from the watch list between cycles (bounded-map hygiene,
	// mirrors the main sweep's prune(live)).
	watched := make(map[string]bool, len(e.livenessWatchIDs))

	for _, id := range e.livenessWatchIDs {
		checked++
		watched[id] = true

		bead := beads[id]
		if bead == nil {
			// A watched id that no longer resolves (deleted, or an authoring
			// typo that silently rots) is itself a violation: the watch list is
			// meant to alert on a dark gardener, and a vanished id is the extreme
			// of dark. Surface it once per episode rather than decaying quietly.
			if e.livenessAllowListState.markDark(id) {
				dark++
				e.logger.Warn("liveness allow-list sweep: watched gardener id not found",
					"gardener", id)
				e.fireLivenessAllowListAlert(ctx, id, nil, "not found", now)
			}
			continue
		}

		reason := livenessAllowListViolation(bead, now)
		if reason == "" {
			// Healthy (open, enabled, fresh) — clear any prior dark flag so a
			// future episode re-alerts.
			e.livenessAllowListState.markFresh(id)
			continue
		}

		if !e.livenessAllowListState.markDark(id) {
			// Already alerted for this dark episode; stay quiet.
			continue
		}
		dark++
		e.logger.Warn("liveness allow-list sweep: watched gardener is DARK",
			"gardener", bead.ID, "title", bead.Title,
			"status", bead.Status, "enabled", bead.Fields["enabled"],
			"last_checked", bead.Fields["last_checked"], "reason", reason)
		e.fireLivenessAllowListAlert(ctx, bead.ID, bead, reason, now)
	}

	e.livenessAllowListState.prune(watched)

	e.logger.Info("liveness allow-list sweep: complete",
		"checked", checked, "dark", dark)
}

// livenessAllowListViolation reports why a watched gardener violates the
// steady-state liveness invariant, or "" when it holds. The invariant mirrors
// the declarative assert ka-0000002431 clause-for-clause (status==open &&
// enabled!=false && last_checked fresh within the stale threshold), but is
// evaluated as a poll over the bead's CURRENT state rather than off a lifecycle
// event — so it catches a gardener that closed or was disabled on a transition
// the event stream never delivered.
func livenessAllowListViolation(bead *beadsapi.BeadDetail, now time.Time) string {
	if bead.Status != "open" {
		return fmt.Sprintf("status=%s (expected open)", bead.Status)
	}
	if bead.Fields["enabled"] == "false" {
		return "disabled (enabled=false)"
	}
	lastChecked := parseLivenessTimestamp(bead.Fields["last_checked"])
	if lastChecked.IsZero() {
		// Open+enabled but never evaluated by the engine: unlike the main sweep
		// (which gives a just-created gardener a bootstrap pass), an id on the
		// operator's explicit watch list is expected to be a live gardener the
		// engine polls, so a permanently-unset last_checked is a real gap.
		return "last_checked unset (never evaluated)"
	}
	cadence := gardenerCadence(bead, now)
	threshold := livenessStaleThreshold(cadence)
	if age := now.Sub(lastChecked); age > threshold {
		return fmt.Sprintf("last_checked stale: %s old (stale after %s)",
			age.Round(time.Minute), threshold.Round(time.Minute))
	}
	return ""
}

// fireLivenessAllowListAlert emits the alert for a dark watched gardener found by
// the all-status poll. bead is nil when the watched id no longer resolves. It
// mirrors fireLivenessAlert's notify/create_bead fan-out and NEVER spawns.
func (e *TriggerEngine) fireLivenessAllowListAlert(ctx context.Context, id string, bead *beadsapi.BeadDetail, reason string, now time.Time) {
	_ = now
	title := id
	if bead != nil {
		title = fmt.Sprintf("%s (%s)", bead.Title, id)
	}
	msg := fmt.Sprintf(
		"🚨 gardener-liveness (watch-list): %s is DARK — %s. "+
			"A watched gardener has gone dark on a transition the event-driven assert "+
			"ka-0000002431 did not catch. Fix: re-open and `gb trigger enable %s`, or "+
			"drop it from the watch list if it is intentionally retired.",
		title, reason, id,
	)

	if e.livenessNotify && e.notifier != nil {
		e.notifier(WithNoticeProject(ctx, noticeBeadProject(bead)), "gardener-liveness", msg, "")
	}

	if e.livenessCreateBead {
		e.createLivenessAllowListBead(ctx, id, bead, msg)
	}
}

// createLivenessAllowListBead files a bug bead recording the dark watched
// gardener. When the gardener bead still resolves the alert is parented to it (so
// it is navigable in beads-viewer); a vanished id gets an un-parented bead.
// Best-effort — a failure is logged, not fatal (the notify leg may already have
// alerted).
func (e *TriggerEngine) createLivenessAllowListBead(ctx context.Context, id string, gardener *beadsapi.BeadDetail, desc string) {
	title := fmt.Sprintf("Watched gardener DARK: %s", id)
	if gardener != nil {
		title = fmt.Sprintf("Watched gardener DARK: %s", gardener.Title)
	}

	labels := []string{
		"liveness",
		"gardener-dark",
		"source:liveness-sweep",
	}
	if gardener != nil {
		if proj := gardener.Fields["project"]; proj != "" {
			labels = append(labels, "project:"+proj)
		}
	}

	beadID, err := e.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "bug",
		Description: desc,
		Labels:      labels,
		Priority:    1,
		CreatedBy:   "liveness-sweep",
	})
	if err != nil {
		e.logger.Warn("liveness allow-list sweep: failed to create dark-gardener bead",
			"gardener", id, "error", err)
		return
	}

	if gardener != nil {
		if err := e.daemon.AddDependency(ctx, beadID, gardener.ID, "parent-child", "liveness-sweep"); err != nil {
			e.logger.Warn("liveness allow-list sweep: failed to parent dark-gardener bead",
				"gardener", gardener.ID, "bead", beadID, "error", err)
		}
	}

	e.logger.Info("liveness allow-list sweep: filed dark-gardener bead",
		"gardener", id, "bead", beadID)
}

// parseLivenessTimestamp parses an RFC3339 last_checked value; an empty or
// malformed value yields the zero time (treated as "never evaluated").
func parseLivenessTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// fireLivenessAlert emits the liveness alert for a dark gardener. It mirrors the
// assert action's notify/create_bead fan-out but is driven directly off the
// bead (the sweep is not a source.Evaluate match) and NEVER spawns an agent.
func (e *TriggerEngine) fireLivenessAlert(ctx context.Context, bead *beadsapi.BeadDetail, lastChecked time.Time, age, cadence, threshold time.Duration) {
	msg := fmt.Sprintf(
		"🚨 gardener-liveness: %s (%s) is DARK — last_checked %s ago (cadence %s, stale after %s). "+
			"The gardener has stopped emitting cycles; it may be source-closed, disabled, or dropped by the scheduler.",
		bead.Title, bead.ID,
		age.Round(time.Minute), cadence.Round(time.Minute), threshold.Round(time.Minute),
	)

	if e.livenessNotify && e.notifier != nil {
		e.notifier(WithNoticeProject(ctx, noticeBeadProject(bead)), "gardener-liveness", msg, "")
	}

	if e.livenessCreateBead {
		// The bead body carries the absolute last_checked timestamp on top of
		// the human-readable summary so the alert is self-contained.
		desc := fmt.Sprintf("%s\n\nlast_checked: %s", msg, lastChecked.UTC().Format(time.RFC3339))
		e.createLivenessBead(ctx, bead, desc)
	}
}

// createLivenessBead files a bug bead recording the dark gardener, parented to
// the gardener so the alert is navigable from the gardener in beads-viewer. A
// failure is logged but not fatal — the notify leg may already have alerted.
func (e *TriggerEngine) createLivenessBead(ctx context.Context, gardener *beadsapi.BeadDetail, desc string) {
	title := fmt.Sprintf("Gardener DARK: %s", gardener.Title)

	labels := []string{
		"liveness",
		"gardener-dark",
		"source:liveness-sweep",
	}
	if proj := gardener.Fields["project"]; proj != "" {
		labels = append(labels, "project:"+proj)
	}

	req := beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "bug",
		Description: desc,
		Labels:      labels,
		Priority:    1,
		CreatedBy:   "liveness-sweep",
	}

	beadID, err := e.daemon.CreateBead(ctx, req)
	if err != nil {
		e.logger.Warn("liveness sweep: failed to create dark-gardener bead",
			"gardener", gardener.ID, "error", err)
		return
	}

	// Parent the alert to the gardener so it appears under it in the graph.
	if err := e.daemon.AddDependency(ctx, beadID, gardener.ID, "parent-child", "liveness-sweep"); err != nil {
		e.logger.Warn("liveness sweep: failed to parent dark-gardener bead to gardener",
			"gardener", gardener.ID, "bead", beadID, "error", err)
	}

	e.logger.Info("liveness sweep: filed dark-gardener bead",
		"gardener", gardener.ID, "bead", beadID)
}
