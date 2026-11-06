// Engine-side CHECK-liveness invariant for NON-gardener triggers (ka-0000004245).
//
// THE GAP THIS CLOSES. SweepGardenerLiveness (gardener_liveness.go) asserts
// `last_checked` freshness, but only for gardener-shape triggers — isGardenerTrigger
// gates the roster, on the reasoning that "a plain source-only trigger has no
// liveness contract (it fires reactively and may legitimately sit idle for days)".
//
// That reasoning conflates two different fields. It is true of FIRING (last_run):
// an intake trigger whose JQL matches nothing may correctly never fire for weeks.
// It is NOT true of CHECKING (last_checked): the engine stamps last_checked on
// EVERY evalOne exit for every enabled trigger it evaluates, fire or no fire
// (stampCheckedOutcome). So last_checked is a valid dead-man's switch for a plain
// trigger too — and nothing scored it.
//
// The 2026-08-14 outage that motivated this sweep: three prod jira intake
// triggers (ka-0000005979 PE, ka-0000004986 DEVOPS, ka-0000005997 FSM) had
// last_checked frozen for ELEVEN DAYS while every health surface read
// enabled=true. Commit 6a8511bce folded last_outcome/last_outcome_at into the
// last_checked patch without adding them to the daemon's
// serviceManagedFieldsByType["trigger"] allowlist, so the ownership bypass
// stopped applying and every stamp was 403'd (~180 authz-denial audit beads an
// hour, ~47k total). None of the four existing sweeps could see it:
//
//	gardener-liveness   — skipped: not gardener-shape (this file's gap)
//	pouring-stall      — scoped to apply_formula molecule progress
//	dark-lane          — keys on last_run vs a pinned max_concurrent lease
//	trigger-starved    — the INVERSE symptom: last_checked advancing too FAST
//	                     (foreign writer starving cooldown), not frozen
//
// WHY last_checked IS VALID EVIDENCE HERE (advice ka-0000002586: verify liveness
// with a field that changes on success, and prove it against a known-good
// control). last_checked is monotonic and advances on every successful
// evaluation, so it genuinely discriminates: on a healthy trigger it moves each
// cycle; on the three broken ones it was byte-identical for 11 days. That is the
// opposite of a field like `enabled`, which read `true` on healthy and broken
// alike and is precisely why this went unnoticed. The paired tests assert BOTH
// directions — fires on the frozen fixture, stays silent on the fresh control.
//
// SCOPE. Alert-only: WARN log + optional notify, no spawn, no bead mutation,
// no tokens. Dedup is per dark episode in a DEDICATED state map, so it can
// never clobber the gardener sweep's prune cycle.
package bridge

import (
	"context"
	"fmt"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// checkLivenessStaleFactor is N in "last_checked must be within N× the
	// trigger's own effective cooldown". The engine re-evaluates a trigger as
	// soon as its cooldown expires, so a trigger that has missed this many
	// consecutive evaluation windows has stopped being checked. 6 is
	// deliberately looser than the gardener sweep's 3: a plain intake trigger
	// can be legitimately deferred by a concurrency gate for a window or two,
	// and this sweep's job is catching a lane dark for HOURS, not smoothing
	// jitter.
	checkLivenessStaleFactor = 6

	// checkLivenessMinThreshold floors the computed threshold. With a 5m
	// cooldown, 6× is 30m; this floor only binds for very tight cooldowns
	// (the 30s triggerCooldown default → 3m, floored to 30m). Generous on
	// purpose: an 11-day outage is caught either way, and a high floor keeps
	// the sweep quiet through daemon hiccups and rollout restarts.
	checkLivenessMinThreshold = 30 * time.Minute

	// checkLivenessSystemOperator is the X-System-Operator label for this
	// sweep's notice write. Same rationale as livenessSystemOperator: a
	// machine-origin cron write with no human behind it needs a resolvable
	// operator or the daemon fail-closed audit resolver 400s the notice and
	// the alert is detected but silently never delivered (ka-0000007257).
	// Must satisfy sanitizeSystemOperatorHeader: ASCII lowercase / digits / '-'.
	checkLivenessSystemOperator = "trigger-check-liveness"
)

// checkLivenessThreshold computes how stale last_checked may be for a trigger
// with the given effective cooldown before the lane is considered unchecked.
func checkLivenessThreshold(cooldown time.Duration) time.Duration {
	threshold := cooldown * checkLivenessStaleFactor
	if threshold < checkLivenessMinThreshold {
		return checkLivenessMinThreshold
	}
	return threshold
}

// SweepTriggerCheckLiveness asserts that every ENABLED, OPEN, NON-gardener
// trigger has had last_checked stamped within N× its own cooldown. A violation
// means the engine has stopped recording evaluations of that lane — it was
// dropped from the roster, or (the ka-0000004245 case) the stamp write itself
// is being rejected. Either way the lane's liveness signal is dead and the
// operator cannot tell from any health surface.
//
// Gardeners are EXCLUDED because SweepGardenerLiveness already owns them with a
// tighter, cadence-aware threshold and a richer skip set; sweeping them here
// would double-alert.
//
// Safe to call every Evaluate cycle: one ListBeadsFiltered plus in-memory
// arithmetic, never spawns. Errors are logged and downgraded.
func (e *TriggerEngine) SweepTriggerCheckLiveness(ctx context.Context) {
	if !e.checkLivenessEnabled {
		return
	}

	ctx = beadsapi.WithSystemOperator(ctx, checkLivenessSystemOperator)

	// Statuses:["open"] for the same reason as the gardener sweep's SCOPE
	// BOUNDARY: a CLOSED trigger's last_checked is frozen at its final cycle by
	// design, so every closed+enabled trigger would read permanently stale and
	// flood. That cohort is owned by SweepClosedEnabledDrain + trigger-reviver.
	res, err := e.openTriggerRoster(ctx)
	if err != nil {
		e.logger.Warn("check-liveness sweep: failed to load trigger roster", "error", err)
		return
	}

	now := time.Now()
	live := make(map[string]bool, len(res.Beads))
	swept, dark := 0, 0

	for _, bead := range res.Beads {
		if bead == nil {
			continue
		}
		// Gardeners belong to SweepGardenerLiveness.
		if isGardenerTrigger(bead) {
			continue
		}
		// A deliberately-disabled trigger is silent by intent, not dark.
		if bead.Fields["enabled"] == "false" {
			continue
		}
		// E2E fixtures are seeded enabled but never fire, so their
		// last_checked is intentionally frozen (ka-0000005435).
		if isE2EFixture(bead) {
			continue
		}
		// Parked behind an open operator decision/escalation: dormant by
		// design, and the engine deliberately does NOT stamp last_checked so
		// the lane resumes instantly on unblock (bug ka-0000003082).
		if bead.Fields["dormant_on_escalation"] == "true" {
			continue
		}

		trig := ParseTriggerDef(bead, "check-liveness")

		// blocked_by an OPEN decision/escalation also suppresses the stamp —
		// reuse the engine's own gate so this skip set tracks the no-stamp set
		// exactly, rather than drifting from it.
		if len(e.triggerBlockedBy(ctx, trig)) > 0 {
			continue
		}

		live[bead.ID] = true
		swept++

		lastChecked := parseLivenessTimestamp(bead.Fields["last_checked"])
		// Never evaluated yet (just created): the first sight stamps it.
		if lastChecked.IsZero() {
			continue
		}

		cooldown := effectiveCooldown(trig)
		threshold := checkLivenessThreshold(cooldown)
		age := now.Sub(lastChecked)

		if age <= threshold {
			e.checkLivenessState.markFresh(bead.ID)
			continue
		}

		dark++
		if !e.checkLivenessState.markDark(bead.ID) {
			continue // already alerted this episode
		}

		e.logger.Warn("check-liveness sweep: trigger has NOT been checked",
			"trigger", bead.ID, "title", bead.Title,
			"source", bead.Fields["source"],
			"last_checked", lastChecked.UTC().Format(time.RFC3339),
			"age", age.String(), "cooldown", cooldown.String(),
			"stale_threshold", threshold.String())

		e.fireCheckLivenessAlert(ctx, bead, lastChecked, age, cooldown, threshold)
	}

	e.checkLivenessState.prune(live)

	e.logger.Info("check-liveness sweep: complete",
		"triggers_swept", swept, "unchecked", dark)
}

// fireCheckLivenessAlert emits the operator-facing alert. Notify-only by
// design: this sweep files no bead, so it cannot itself become a source of
// bead churn the way the 47k denial-audit flood did.
func (e *TriggerEngine) fireCheckLivenessAlert(ctx context.Context, bead *beadsapi.BeadDetail, lastChecked time.Time, age, cooldown, threshold time.Duration) {
	msg := fmt.Sprintf(
		"🚨 trigger-check-liveness: %s (%s) is UNCHECKED — enabled=true but last_checked has not advanced in %s "+
			"(cooldown %s, stale after %s; last_checked %s). The engine has stopped recording evaluations of this "+
			"lane: it was dropped from the roster, or the last_checked stamp write is being rejected "+
			"(the ka-0000004245 ownership-denial class — check for authz:deny:update:trigger audit beads).",
		bead.Title, bead.ID,
		age.Round(time.Minute), cooldown.Round(time.Second), threshold.Round(time.Minute),
		lastChecked.UTC().Format(time.RFC3339),
	)

	if e.checkLivenessNotify && e.notifier != nil {
		e.notifier(WithNoticeProject(ctx, noticeBeadProject(bead)), "trigger-check-liveness", msg, "")
	}
}
