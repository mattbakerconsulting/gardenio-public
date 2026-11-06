package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// authFailover handles automatic credential failover when an agent exhausts
// its budget or hits a rate limit. It consults a per-project in-memory
// runtime map (failoverRuntime) as the source of truth for active/current
// account tracking. Cooldown metadata is persisted on the account bead
// (status=exhausted, cooldown_until, exhausted_at) so the runtime map can be
// rebuilt on controller restart.
//
// Switchback is ABOLISHED (ka-0000008240). In the primary-free regime accounts
// are equal: a throttled account is parked (status=exhausted + cooldown_until)
// and rejoins the healthy spread pool when SweepStaleExhausted clears its
// status — there is no privileged "primary" home to steer a project back to.
// The old probe/timer switchback machinery (CheckSwitchbacks,
// SwitchbackForAccount, resetToPrimary, clearRecoveredIntermediates) was gated
// off in Phase 2 and deleted in Phase 5; its timer-based recovery role is now
// carried by SweepStaleExhausted's cooldown_until-elapsed clause.
//
// NOTE: setting status=exhausted on an account bead is advisory only — it
// prevents the account from being assigned to new sessions but does NOT
// proactively rebalance existing sessions off it. Failover only fires when
// one of these events arrives:
//   - NATS gardenio.rotation.account_throttled (probe-driven, via probeEventListener)
//   - NATS coopmux.events.auth.budget_exceeded (agent-driven, via account_exhaustion.go)
//   - SSE throttle_status=rejected on an account bead (via main.go)
//
// This is intentional: existing sessions run until they hit the actual API
// wall, squeezing out remaining budget.
type authFailover struct {
	daemon *beadsapi.Client
	// accounts is the client every account-bead WRITE goes through. In
	// production it authenticates as the controller's own machine principal
	// (gd-s7aPIf0Bgp) so kbeads admits update/account through the
	// gardenio-machine-accounts grant; when the machine principal is disabled
	// it is the same client as daemon. Reads and non-account writes stay on
	// daemon: a machine principal is ownership-gated for non-config beads.
	accounts       *beadsapi.Client
	logger         *slog.Logger
	cooldown       time.Duration
	maxFailoverDur time.Duration     // force-reset after this duration
	rotation       *rotationNotifier // optional NATS publisher for rotation events

	runtime *failoverRuntime
}

// newAuthFailover creates a failover handler with configurable cooldown and
// max failover duration. Pass 0 for either to use defaults (5m / 24h).
//
// accounts is the client for account-bead writes (the machine principal when
// enabled); pass daemon itself to keep today's single-client behavior.
func newAuthFailover(daemon, accounts *beadsapi.Client, logger *slog.Logger, cooldown, maxFailoverDur time.Duration) *authFailover {
	if accounts == nil {
		accounts = daemon
	}
	if cooldown == 0 {
		cooldown = 5 * time.Minute
	}
	if maxFailoverDur == 0 {
		maxFailoverDur = 24 * time.Hour
	}
	return &authFailover{
		daemon:         daemon,
		accounts:       accounts,
		logger:         logger,
		cooldown:       cooldown,
		maxFailoverDur: maxFailoverDur,
		runtime:        newFailoverRuntime(),
	}
}

// throttleAllows returns true when the throttle_status indicates the account
// can serve requests. Anthropic's API returns "allowed", "allowed_warning",
// or "rejected" — anything other than "rejected" (or empty) is usable.
// Use this for fallback selection (can the account serve traffic?).
// For full-recovery decisions, use throttleRecovered instead.
func throttleAllows(status string) bool {
	return status != "" && status != "rejected"
}

// throttleRecovered returns true only when the account has fully recovered
// (throttle_status == "allowed"). "allowed_warning" means high utilization —
// the account could be rejected again immediately, so it should NOT be treated
// as fully recovered. Once rejected, an account must reach clean "allowed"
// status (usage reset) before being considered recovered.
func throttleRecovered(status string) bool {
	return status == "allowed"
}

// Handle processes a budget_exceeded or rate_limited event for an agent. It
// resolves the project's primary → current → fallback chain, validates the
// fallback can serve requests, and switches to it if healthy. Runtime state
// is updated on success; the exhausted account bead is marked with a cooldown
// window so a restart can reconstruct the runtime map.
func (f *authFailover) Handle(ctx context.Context, project, agentBeadID string) error {
	if project == "" {
		return nil
	}

	// Per-project cooldown — prevent flapping.
	if !f.tryAcquireCooldown(project) {
		f.logger.Info("auth failover cooldown active, skipping",
			"project", project, "agent_bead", agentBeadID)
		return nil
	}

	// Load all account beads labeled for this project so we can determine
	// both the configured primary and the currently-serving account. When
	// no account beads carry a "project:" label (legacy deployments and E2E
	// fixtures), resolve the chain via the config:auth bead instead.
	accounts, err := f.listProjectAccounts(ctx, project)
	if err != nil {
		return fmt.Errorf("listing account beads for project %s: %w", project, err)
	}
	var legacyPrimary string
	if len(accounts) == 0 {
		accounts, legacyPrimary, err = resolveLegacyProjectChain(ctx, f.daemon, project)
		if err != nil {
			return fmt.Errorf("resolving legacy chain for project %s: %w", project, err)
		}
		if len(accounts) == 0 {
			f.logger.Info("no account beads for project, cannot failover", "project", project)
			return nil
		}
	}

	primary := projectPrimary(accounts)
	if primary == "" && legacyPrimary != "" {
		primary = legacyPrimary
	}
	if primary == "" {
		f.logger.Warn("unable to determine primary account for project", "project", project)
		return nil
	}

	// Current = runtime state if present; otherwise the project is still on
	// its primary.
	state, hasState := f.runtime.Get(project)
	currentName := primary
	if hasState && state.Current != "" {
		currentName = state.Current
	}

	current := findByTitle(accounts, currentName)
	if current == nil {
		f.logger.Warn("current account bead not found",
			"account", currentName, "project", project)
		return nil
	}

	// Walk the fallback chain starting from the current account to find the
	// best healthy candidate.
	fallback, _ := f.findHealthyFallback(accounts, current, currentName, project)
	if fallback == "" {
		// No healthy successor in the chain. This is only worth alerting on
		// when the CURRENT account itself can no longer serve traffic. If the
		// current account is still healthy — e.g. we are sitting on the
		// TERMINAL account of the chain and it is active — then reaching the
		// end of the chain simply means "we are good, no rotation needed".
		// Emitting no_fallback here produced a spurious ~5-minute alert loop
		// for projects pinned to a healthy terminal account (ka-0000005652).
		if currentAccountUsable(current) {
			f.logger.Debug("no successor in chain but current account is healthy; no rotation needed",
				"current", currentName, "project", project,
				"status", current.Fields["status"],
				"throttle_status", current.Fields["throttle_status"])
			return nil
		}
		f.logger.Warn("no healthy fallback found in chain and current account is unhealthy",
			"current", currentName, "project", project,
			"status", current.Fields["status"],
			"throttle_status", current.Fields["throttle_status"])
		if f.rotation != nil {
			resetAt := earliestResetAt(accounts, current)
			f.rotation.NotifyNoFallback(project, currentName, resetAt)
		}
		return nil
	}

	if f.rotation != nil {
		f.rotation.NotifyRotating(project, currentName, fallback, agentBeadID)
	}

	// Update runtime state. Preserve FailedOverAt if already in an active
	// failover episode so the episode's original start time is retained.
	now := time.Now().UTC()
	newState := failoverState{
		Primary:        primary,
		Current:        fallback,
		FailedOverAt:   now,
		FailoverReason: "rate_limited",
		LastFailoverAt: now,
	}
	if hasState && !state.FailedOverAt.IsZero() && state.Primary == primary {
		newState.FailedOverAt = state.FailedOverAt
	}
	f.runtime.Set(project, newState)

	// Persist the current account's post-rotation state. There are two distinct
	// treatments (ka-0000003305):
	//
	//   HARD exhaustion — sideline the account for the full cooldown window
	//   (status=exhausted + cooldown_until). Reserved for accounts that are
	//   genuinely at their rolling budget: throttle_status=rejected AND 5h/7d
	//   utilization at/above nearLimitUtilization.
	//
	//   DEPRIORITIZE — a 429 while utilization still has headroom is a monthly
	//   spend-cap rejection or a transient/burst throttle, NOT budget exhaustion.
	//   Hard-pulling such an account wastes a subscription that can still serve
	//   traffic (the 2026-06-17 incident: matthewbaker-acme u7d 0.62 and
	//   gardenio u7d 0.85 were sidelined for ~24h despite ~97% success). Instead we
	//   rotate the current session away but leave the account status active and
	//   assignable, stamping deprioritized_until so account selection ranks it
	//   LAST for a short window rather than removing it from the pool.
	//
	// This is the only persisted state — the runtime map is rebuilt from it on
	// restart.
	rotCount, _ := strconv.Atoi(current.Fields["rotation_count"])
	var fields map[string]any
	if shouldHardExhaust(current) {
		cooldownUntil := f.computeCooldownUntil(current, now)
		fields = map[string]any{
			"status":          "exhausted",
			"exhausted_at":    now.Format(time.RFC3339),
			"cooldown_until":  cooldownUntil.Format(time.RFC3339),
			"rotation_count":  rotCount + 1,
			"last_rotated_at": now.Format(time.RFC3339),
		}
	} else {
		f.logger.Info("429 with utilization headroom — deprioritizing instead of exhausting",
			"account", currentName, "project", project,
			"utilization_5h", current.Fields["utilization_5h"],
			"utilization_7d", current.Fields["utilization_7d"],
			"throttle_status", current.Fields["throttle_status"])
		fields = map[string]any{
			"deprioritized_until": now.Add(deprioritizeWindow).Format(time.RFC3339),
			"rotation_count":      rotCount + 1,
			"last_rotated_at":     now.Format(time.RFC3339),
		}
	}
	if err := f.accounts.UpdateBeadFieldsTyped(ctx, current.ID, fields); err != nil {
		f.logger.Warn("failed to update account status",
			"account", currentName, "bead", current.ID, "error", err)
	}

	// NOTE: credential distribution to coopmux sessions is no longer done here.
	// Coopmux handles failover locally via project_scoped_rebalance in throttle.rs.

	f.logger.Info("auth failover complete",
		"project", project,
		"from_account", currentName,
		"to_account", fallback,
		"primary_account", primary,
		"agent_bead", agentBeadID)

	if f.rotation != nil {
		f.rotation.NotifyRotated(project, currentName, fallback)
	}

	return nil
}

// computeCooldownUntil returns the timestamp at which the given account
// should be retried. Per-account billing_period_hours wins when set;
// otherwise the controller-wide maxFailoverDur applies.
func (f *authFailover) computeCooldownUntil(account *beadsapi.BeadDetail, now time.Time) time.Time {
	dur := f.maxFailoverDur
	if h := account.Fields["billing_period_hours"]; h != "" {
		if parsed, err := time.ParseDuration(h + "h"); err == nil && parsed > 0 {
			dur = parsed
		}
	}
	return now.Add(dur)
}

// probeStaleThreshold is the maximum age of a probe_checked_at timestamp
// before it's considered too old to trust for switchback decisions.
const probeStaleThreshold = 20 * time.Minute

// nearLimitUtilization is the 5h/7d utilization at/above which a rejected
// account is treated as genuinely at its rolling budget and eligible for HARD
// exhaustion. Below this an account still has subscription headroom, so a 429
// is attributed to a monthly spend-cap or a transient/burst throttle rather
// than budget exhaustion (ka-0000003305). Set just under the probe's
// exhaustedUtilization (1.0) so an account within 1% of its budget still hard-
// exhausts, matching the "utilization_5h or utilization_7d >= ~0.99" design.
const nearLimitUtilization = 0.99

// deprioritizeWindow is how long a deprioritized (429-with-headroom) account is
// ranked last in account selection. Short by design: the account is still fully
// usable — this only steers NEW placement away from it while a spend-cap or
// transient throttle clears, without pulling it from rotation.
const deprioritizeWindow = 30 * time.Minute

// shouldHardExhaust reports whether an account that just triggered a failover
// should be HARD-sidelined (status=exhausted + cooldown) rather than merely
// deprioritized. Hard exhaustion requires BOTH that Anthropic is actively
// rejecting the account (throttle_status=rejected) AND that its rolling 5h or
// 7d utilization is at/above nearLimitUtilization. A 429 with utilization below
// that threshold is a spend-cap or transient throttle: the subscription still
// has capacity, so the account stays active and assignable and is only
// deprioritized (ka-0000003305).
func shouldHardExhaust(bead *beadsapi.BeadDetail) bool {
	if bead.Fields["throttle_status"] != "rejected" {
		return false
	}
	util5h := parseFloat(bead.Fields["utilization_5h"])
	util7d := parseFloat(bead.Fields["utilization_7d"])
	return util5h >= nearLimitUtilization || util7d >= nearLimitUtilization
}

// SweepStaleExhausted clears `status="exhausted"` on any account bead whose
// rate-limit probe has reported `throttle_status="allowed"` recently —
// independent of the in-memory failover runtime map.
//
// Why a runtime-map-independent sweep is required:
//
// The old switchback stale-exhausted clear path only ran when a switchback was
// triggered against a project whose runtime map entry was still inFailover.
// After a controller restart that map is empty, so nothing ever fired, and any
// accounts that were marked `status="exhausted"` by a prior controller process
// stayed stuck forever. That left the account-rotation logic
// (buildProjectAccountOverride) considering them dead, even though the
// rate-limit probe said they're healthy — which is exactly what pinned every
// gardenio agent to litellm-bedrock on 2026-04-25 (see ka-0000002663 for the
// incident writeup).
//
// This sweep is the runtime-map-independent recovery path: probe
// state on the account bead alone is enough to clear stale exhausted
// status, regardless of whether any project is currently failed over.
//
// Sole recovery path (ka-0000008240): with switchback abolished, this sweep is
// now the ONLY thing that returns an exhausted account to the healthy spread
// pool. To preserve the old switchback maxFailoverDur safety net in a
// primary-free form, an exhausted account recovers when EITHER:
//
//   - its rate-limit probe recovered (throttle_status allows) AND the probe is
//     fresh (within probeStaleThreshold) — the pre-existing probe path; OR
//   - its cooldown_until has elapsed — the timer-based backstop. This is what
//     lets a Rust-coopmux-marked account (status=exhausted + cooldown_until set
//     by project_scoped_rebalance, but possibly never probed) recover, and what
//     recovers any account whose probe has gone stale/down. Without it, killing
//     switchback would strand such accounts exhausted forever — the high-severity
//     probe-liveness risk called out in the epic.
//
// An account still in its cooldown window with no fresh recovering probe stays
// parked. When an account remains exhausted PAST its cooldown_until but is NOT
// cleared here — because a real blocker (out-of-credits, or throttle still
// rejected) holds it — we emit a WARN so a genuinely stuck account is visible
// rather than silently leaking capacity.
//
// The out-of-credits hold still wins over the cooldown-elapsed clause: a metered
// account out of billing credits must stay parked no matter how long its cooldown
// has run, because real inference still 429s on billing.
//
// Caller is the periodic tick in main.go.
func (f *authFailover) SweepStaleExhausted(ctx context.Context) {
	accounts, err := f.listAccountBeads(ctx)
	if err != nil {
		f.logger.Warn("sweep-exhausted: failed to list account beads", "error", err)
		return
	}

	now := time.Now().UTC()

	// Per-tick counters. Emitted as a single Info heartbeat at the end of
	// every sweep so we can tell from logs whether the function ran, what
	// it saw, and why it did or did not act. Without this we cannot
	// distinguish "function never called" from "function called, nothing
	// matched". See ka-0000007920.
	var (
		exhausted          int // bead.Fields["status"] == "exhausted"
		recovered          int // exhausted AND throttle_status == "allowed"
		freshProbe         int // recovered AND probe within probeStaleThreshold
		cooldownElapsed    int // cleared via the cooldown_until-elapsed backstop
		cleared            int // cleared (either path) AND UpdateBeadFields succeeded
		updateErrors       int
		probeStale         int
		throttleNotAllowed int
		outOfCredits       int      // rate-limit-OK but overage rejected=out_of_credits
		stuckPastCooldown  int      // exhausted past cooldown_until but a real blocker holds it
		exhaustedSamples   []string // up to 3 titles, for at-a-glance log diagnosis
	)

	for _, bead := range accounts {
		if bead.Fields["status"] != "exhausted" {
			continue
		}
		exhausted++
		if len(exhaustedSamples) < 3 {
			exhaustedSamples = append(exhaustedSamples, bead.Title)
		}

		// Out-of-credits hold (ka-0000008164 / ka-0000003350): a METERED account
		// whose rate-limit windows recovered (throttle_status != rejected) can
		// still be OUT OF BILLING CREDITS (overage_status=rejected,
		// reason=out_of_credits) and 429 on every real request. Clearing exhausted
		// here would flip it back to active seconds after a billing 429 re-exhausted
		// it — the active↔exhausted oscillation. Keep it parked until credits return.
		// This hold intentionally OUTRANKS the cooldown-elapsed backstop below: a
		// metered out-of-credits account must never be cleared on a timer, since its
		// blocker is billing, not a rate-limit window. Scoped to out_of_credits AND
		// metered: org_spend_cap_reached is benign on healthy subscription accounts,
		// and out_of_credits itself is benign on subscription accounts
		// (ka-0000004929 / ka-0000003350) — those clear normally so the healthy fleet
		// account is never stranded.
		if overageOutOfCredits(billingType(bead.Fields), bead.Fields["overage_status"], bead.Fields["overage_disabled_reason"]) {
			outOfCredits++
			if cooldownUntilElapsed(bead, now) {
				f.logger.Warn("sweep-exhausted: account exhausted past cooldown but held out-of-billing-credits",
					"account", bead.Title, "bead", bead.ID,
					"overage_disabled_reason", bead.Fields["overage_disabled_reason"],
					"cooldown_until", bead.Fields["cooldown_until"])
				stuckPastCooldown++
			}
			continue
		}

		// Recovery clause 1 — fresh recovering probe. Clear the exhausted flag as
		// soon as the account can serve traffic again (throttleAllows: anything but
		// "rejected"/empty), NOT only on a clean "allowed". An account that recovered
		// to "allowed_warning" is serving 200s — holding it exhausted for the full
		// cooldown wastes real capacity. Require a recent probe so we don't clear
		// based on stale data from before the account actually recovered.
		probeRecovered := false
		if throttleAllows(bead.Fields["throttle_status"]) {
			recovered++
			if checkedAt, perr := time.Parse(time.RFC3339, bead.Fields["probe_checked_at"]); perr == nil {
				if time.Since(checkedAt) < probeStaleThreshold {
					probeRecovered = true
					freshProbe++
				} else {
					probeStale++
				}
			} else {
				probeStale++
			}
		} else {
			throttleNotAllowed++
		}

		// Recovery clause 2 — cooldown_until-elapsed backstop (the primary-free
		// replacement for the switchback maxFailoverDur auto-reset). An account
		// whose cooldown window has passed rejoins the pool even without a fresh
		// recovering probe, so a Rust-marked/unprobed account or one behind a
		// down/stale probe cannot leak capacity forever.
		coolElapsed := cooldownUntilElapsed(bead, now)

		if !probeRecovered && !coolElapsed {
			// Still parked: neither the probe nor the timer says recover yet.
			continue
		}

		if err := f.accounts.UpdateBeadFields(ctx, bead.ID, map[string]string{
			"status":         "active",
			"cooldown_until": "",
		}); err != nil {
			updateErrors++
			f.logger.Warn("sweep-exhausted: failed to clear stale exhausted",
				"account", bead.Title, "bead", bead.ID, "error", err)
			continue
		}
		cleared++
		if coolElapsed && !probeRecovered {
			cooldownElapsed++
		}
		f.logger.Info("sweep-exhausted: cleared stale exhausted status on recovered account",
			"account", bead.Title, "bead", bead.ID,
			"throttle_status", bead.Fields["throttle_status"],
			"probe_checked_at", bead.Fields["probe_checked_at"],
			"recovered_via", recoveryReason(probeRecovered, coolElapsed))
	}

	f.logger.Info("sweep-exhausted: tick complete",
		"total_accounts", len(accounts),
		"exhausted", exhausted,
		"throttle_not_allowed", throttleNotAllowed,
		"out_of_credits", outOfCredits,
		"recovered", recovered,
		"probe_stale", probeStale,
		"fresh_probe", freshProbe,
		"cooldown_elapsed", cooldownElapsed,
		"stuck_past_cooldown", stuckPastCooldown,
		"cleared", cleared,
		"update_errors", updateErrors,
		"exhausted_samples", strings.Join(exhaustedSamples, ","))
}

// recoveryReason renders which SweepStaleExhausted clause cleared an account,
// for the per-account Info log. "probe" = fresh recovering probe, "cooldown" =
// cooldown_until-elapsed backstop, "probe+cooldown" = both were satisfied.
func recoveryReason(probeRecovered, cooldownElapsed bool) string {
	switch {
	case probeRecovered && cooldownElapsed:
		return "probe+cooldown"
	case probeRecovered:
		return "probe"
	default:
		return "cooldown"
	}
}

// findHealthyFallback walks the fallback chain starting from the given
// account's fallback pointer. Returns the healthiest candidate by tier, or
// ("", nil) if the chain has no usable accounts.
//
// Preference order:
//  1. Non-exhausted + throttle allows (tier 0, ideal)
//  2. Exhausted but fully recovered (throttle_status=allowed) (tier 1)
//  3. Exhausted with allowed_warning — last resort (tier 2)
func (f *authFailover) findHealthyFallback(accounts []*beadsapi.BeadDetail, current *beadsapi.BeadDetail, currentName, project string) (string, *beadsapi.BeadDetail) {
	visited := map[string]bool{currentName: true}
	candidate := current.Fields["fallback"]

	var bestName string
	var bestBead *beadsapi.BeadDetail
	bestTier := 99

	for candidate != "" {
		if visited[candidate] {
			f.logger.Warn("circular fallback chain detected",
				"current", currentName, "at", candidate, "project", project)
			break
		}
		visited[candidate] = true

		bead := findByTitle(accounts, candidate)
		if bead == nil {
			f.logger.Warn("fallback account bead not found",
				"fallback", candidate, "current", currentName, "project", project)
			break
		}

		tier := fallbackTier(bead)
		if tier < 0 {
			f.logger.Info("fallback account ineligible, trying next in chain",
				"fallback", candidate, "project", project,
				"status", bead.Fields["status"],
				"throttle_status", bead.Fields["throttle_status"])
			candidate = bead.Fields["fallback"]
			continue
		}

		if tier < bestTier {
			bestName = candidate
			bestBead = bead
			bestTier = tier
			if tier == 0 {
				break
			}
		}

		candidate = bead.Fields["fallback"]
	}

	if bestBead == nil {
		f.logger.Warn("no healthy fallback found in chain",
			"current", currentName, "project", project)
		return "", nil
	}

	if bestBead.Fields["litellm_key"] == "" && bestBead.Fields["base_url"] != "" {
		f.logger.Warn("fallback account has base_url but no litellm_key — agents will fail LiteLLM auth",
			"fallback", bestName, "project", project,
			"base_url", bestBead.Fields["base_url"])
	}

	f.logger.Info("selected fallback account",
		"fallback", bestName, "tier", bestTier, "project", project,
		"status", bestBead.Fields["status"],
		"throttle_status", bestBead.Fields["throttle_status"])

	return bestName, bestBead
}

// currentAccountUsable reports whether the project's current account can still
// serve traffic. It is used to decide whether reaching the end of the fallback
// chain (no healthy successor) is an actual problem or simply "we are good".
//
// An account is usable unless it is operator-disabled, or it is exhausted with
// a throttle status that no longer allows requests. A healthy/active terminal
// account satisfies this, so no no_fallback alert is warranted (ka-0000005652).
func currentAccountUsable(bead *beadsapi.BeadDetail) bool {
	if bead == nil {
		return false
	}
	status := bead.Fields["status"]
	if status == "disabled" {
		return false
	}
	if status == "exhausted" && !throttleAllows(bead.Fields["throttle_status"]) {
		return false
	}
	return true
}

// fallbackTier classifies an account's health for fallback selection. Lower
// tier is healthier. Returns -1 for unusable accounts.
//
//	Tier 0: non-exhausted, throttle allows (ideal)
//	Tier 1: exhausted but fully recovered (throttle_status=allowed)
//	Tier 2: exhausted with allowed_warning (usable but risky)
//	-1:     status=disabled (operator-disabled), or exhausted AND rejected
func fallbackTier(bead *beadsapi.BeadDetail) int {
	status := bead.Fields["status"]
	if status == "disabled" {
		return -1
	}
	// Probe-gate (ka-0000002085): an account the read-only probe found
	// auth-dead (probe_auth_status="rejected" — a 401/403: revoked token or org
	// with Claude Code access disabled) is never a usable failover target, no
	// matter how healthy its rate-limit window looks. Pool status is not
	// sufficient to pick a switch-target; rotating a live session onto it kills
	// the session (the matthewbaker1110 hard error).
	if bead.Fields["probe_auth_status"] == "rejected" {
		return -1
	}
	exhausted := status == "exhausted"
	ts := bead.Fields["throttle_status"]

	if exhausted && !throttleAllows(ts) {
		return -1
	}
	if !exhausted {
		return 0
	}
	if throttleRecovered(ts) {
		return 1
	}
	return 2
}

// tryAcquireCooldown returns true when enough time has passed since this
// project's last failover decision to attempt another. Consults and updates
// the runtime state's LastFailoverAt.
func (f *authFailover) tryAcquireCooldown(project string) bool {
	state, ok := f.runtime.Get(project)
	if ok && !state.LastFailoverAt.IsZero() && time.Since(state.LastFailoverAt) < f.cooldown {
		return false
	}
	// Stamp LastFailoverAt optimistically so concurrent events are rejected.
	// The full runtime state is updated inside Handle; if Handle bails early
	// without doing the failover, the stamped time simply defers the next
	// attempt by the cooldown window (same safety property as the old map).
	newState := state
	newState.LastFailoverAt = time.Now()
	f.runtime.Set(project, newState)
	return true
}

// listProjectAccounts returns all account beads labeled for the given project
// (regardless of status) so callers can reason about exhausted/cooldown state.
func (f *authFailover) listProjectAccounts(ctx context.Context, project string) ([]*beadsapi.BeadDetail, error) {
	result, err := f.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Labels:   []string{"project:" + project},
		Limit:    100,
	})
	if err != nil {
		return nil, err
	}
	return result.Beads, nil
}

// listAccountBeads fetches all open account beads in a single API call.
func (f *authFailover) listAccountBeads(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	result, err := f.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Limit:    200,
	})
	if err != nil {
		return nil, err
	}
	return result.Beads, nil
}

// projectPrimary selects a deterministic representative account for a project
// from its label-scoped pool.
//
// Role after abolish-primary (ka-0000008240): switchback is deleted, so this no
// longer designates a privileged "home" to steer a project back onto. Its only
// caller is the probe event listener's throttle-attribution path
// (probe_event_listener.go), which uses it to decide whether a throttled
// account is the one a project would nominally resolve to — a read-only
// attribution hint, not a steering decision. Kept primary-marker-aware and
// skip-cooked so its attribution stays consistent with
// buildProjectAccountOverride's empty-pool tiebreaker; the marker itself is
// stripped from beads in Phase 6.
//
// An explicit primary marker (field primary=="true") on a healthy (active)
// account wins when set; ties between multiple marked accounts break
// alphabetically. A primary marker on an exhausted/disabled account is ignored
// so that this selection stays consistent with buildProjectAccountOverride (the
// new-pod path), which likewise skips non-active accounts — otherwise the two
// diverge (ka-0000002933). Otherwise the primary is the head of the fallback
// graph: the account (sorted alphabetically for determinism) that no other
// account in the pool lists as its fallback. If no such head exists (all
// accounts point at each other), alphabetical first wins.
func projectPrimary(accounts []*beadsapi.BeadDetail) string {
	if len(accounts) == 0 {
		return ""
	}
	var marked []string
	for _, a := range accounts {
		if a.Fields["primary"] != "true" {
			continue
		}
		// Skip a primary marker stranded on a non-active account; mirrors
		// buildProjectAccountOverride's "status != active" guard.
		if status := a.Fields["status"]; status != "" && status != "active" {
			continue
		}
		marked = append(marked, a.Title)
	}
	if len(marked) > 0 {
		sort.Strings(marked)
		return marked[0]
	}
	fallbackTargets := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		if fb := a.Fields["fallback"]; fb != "" {
			fallbackTargets[fb] = true
		}
	}
	names := make([]string, 0, len(accounts))
	for _, a := range accounts {
		names = append(names, a.Title)
	}
	sort.Strings(names)
	for _, n := range names {
		if !fallbackTargets[n] {
			return n
		}
	}
	return names[0]
}
