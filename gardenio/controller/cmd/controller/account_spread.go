package main

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// account_spread.go — spawn-time load spreading across a project's healthy
// account pool (ka-0000004861, plan-of-record ka-0000001771, root cause
// ka-0000003255).
//
// Why this exists: applyAccountConfig used to read a SINGLE account from
// cfg.ProjectCache[project].Account (set by buildProjectAccountOverride =
// explicit primary marker or alphabetically-first active). Every pod for a
// project therefore baked the SAME OAuth token + LiteLLM key, so under load all
// pods piled onto one Anthropic account's per-account RPM limit while the other
// healthy accounts sat idle. coopmux's pool spreader is disabled
// (COOPMUX_DISABLE_AUTO_ASSIGN=1, see ka-0000007230), so nothing else spread the
// load. selectAccount distributes new pods across all healthy project
// accounts at spawn — the controller-side analogue of coopmux's
// pick_least_loaded — without re-enabling auto-assign.

// selectAccount returns the account name a new pod for `project` should be
// credentialed against, spreading load across the project's healthy account pool
// by effective load. Aside from the wall clock (see selectAccountAt), it
// is a pure function of its inputs (cfg snapshots, project, spec, liveCounts)
// so it is fully unit-testable.
//
// Precedence — the existing guards are preserved EXACTLY and are NOT spread:
//   - A role-pinned account (resolveRoleAccount) always wins. Roles like
//     fsm-triage are pinned to a specific account regardless of project.
//   - A role that owns an exclusive account (exclusive_to_role) is never spread;
//     it uses its reserved account exclusively (data-driven partition replacing
//     the old Bedrock hardcode — ka-0000005223, policy ka-0000002660).
//
// Spread (the new behavior) applies only to ordinary project agents:
//   - The pool is first filtered to the spec's harness (ADR 0004 §4): a codex
//     spec sees only provider=openai accounts, any other spec only non-openai
//     ones, so a codex role is never handed a Claude token or the reverse.
//     spec.Harness is resolved by applyAccountConfig ahead of selection; empty
//     means claude.
//   - Candidate set = accounts labeled project:<name> with status active/empty
//     AND throttle_status != "rejected". An "allowed_warning" account is
//     low-priority (last resort), used only when no fully-healthy account
//     exists.
//   - Accounts reserved to another role (exclusive_to_role) are excluded from a
//     non-owner's pool.
//   - Among candidates, pick the lowest EFFECTIVE load; tie-break
//     alphabetically for determinism. Effective load is
//     live_pods + warning_penalty − capacity_reset_bonus − spawn_weight
//     (− long_lived_headroom for long-lived roles): the capped capacity+reset
//     bonus biases toward accounts with unused capacity in a window that resets
//     soon (use-it-or-lose-it, ka-0000001522), and spawn_weight is the
//     operator's manual lever.
//   - Long-lived roles (gardener / plan-gardener — the hours-long, Workflow-
//     hosting spawners) additionally prefer the account with the most ABSOLUTE
//     binding-window headroom and are penalized away from accounts near their
//     limit, so a long-running agent is not routed onto an account that hits the
//     wall mid-run (ka-0000001331). Short-lived roles are unaffected.
//   - If no healthy project account exists, return "" — the caller
//     (applyAccountConfig) then either takes the opus model-group fallback or
//     blocks the spawn with an explicit no-healthy-account reason. The old
//     ProjectCache/DefaultAccount fall-through was retired in Phases 2–3
//     (ka-0000004094, ka-0000007959); this is now the ONE selection function
//     over the healthy authorized pool.
func selectAccount(cfg *config.Config, project string, spec *podmanager.AgentPodSpec, liveCounts map[string]int) string {
	return selectAccountAt(cfg, project, spec, liveCounts, time.Now().UTC())
}

// selectAccountAt is selectAccount with an injectable clock: `now`
// feeds the reset-proximity term of capacityResetBonus, keeping the ranking a
// pure function of its inputs for tests.
func selectAccountAt(cfg *config.Config, project string, spec *podmanager.AgentPodSpec, liveCounts map[string]int, now time.Time) string {
	// Role pin wins and is never spread (mirrors applyAccountConfig precedence).
	if a := roleAccountPin(cfg, project, spec.Role); a != "" {
		return a
	}
	return selectSpreadAccountAt(cfg, project, spec, liveCounts, now)
}

// selectSpreadAccountAt is selectAccountAt WITHOUT the role-pin short-circuit:
// it ranks the project's healthy spread pool for spec's project/role, honoring
// every other guard (exclusive-account ownership, account_whitelist, served
// model, model-group exhaustion, long-lived headroom). It is the account a pod
// would land on if its role pin were absent.
//
// The role-pin exhaustion spill (applyAccountConfig, ka-0000001307) calls this
// directly: when a role's PINNED account is capacity-exhausted at spawn and the
// project has other healthy accounts, the spill needs the pool ranking with the
// pin removed but the role's other constraints (whitelist / long-lived) intact —
// exactly this function. A role that OWNS an exclusive account still resolves to
// "" here (roleOwnsExclusiveAccount), so an exclusive lane never spills.
func selectSpreadAccountAt(cfg *config.Config, project string, spec *podmanager.AgentPodSpec, liveCounts map[string]int, now time.Time) string {
	// A role that owns an exclusive account (exclusive_to_role) is pinned to that
	// account and is never spread across the general project pool. The role-pin
	// check above normally resolves it; this is the data-driven safety net the
	// hardcoded fsm-triage carve-out used to provide.
	if roleOwnsExclusiveAccount(cfg, spec.Role) {
		return ""
	}
	if project == "" {
		return ""
	}

	// Long-lived roles (gardener / plan-gardener) get an absolute-headroom
	// preference so a hours-long, Workflow-hosting agent is steered onto the
	// account with the most runway rather than one near its limit (ka-0000001331).
	longLived := roleIsLongLived(spec.Role)

	// Served-model gate snapshot (ka-0000002790): the set of models LiteLLM
	// actually serves. An account whose baked model is NOT in this set fails
	// every first inference turn (model-not-found) and crash-loops the pod, so
	// it must never enter the cognition spread pool — the glm-bedrock incident,
	// where a target model (valid key, bedrock-glm-5 not yet served) rode into
	// the glm-lab pool as lowest-load and was baked as agents' own brain.
	// Fail-open: gateServed is nil (no gating) unless the operator kill switch is
	// on AND the sync has published a non-empty set. See accountModelUnserved.
	var gateServed map[string]bool
	if cfg.SpreadServedModelGateEnabled {
		if s := cfg.SnapshotServedModels(); len(s) > 0 {
			gateServed = s
		}
	}

	wantLabel := "project:" + project
	harness := spec.Harness

	type candidate struct {
		name string
		// load is the EFFECTIVE load used to pick the least-loaded account:
		// live-pod count, plus a penalty for de-prioritized (allowed_warning)
		// accounts (see warningSpreadPenalty), minus the capped
		// capacity+reset bonus (see capacityResetBonus), minus the operator's
		// manual spawn_weight bias. Float because the bonus terms are
		// fractional.
		load float64
	}
	var pool []candidate
	for name, acct := range cfg.AccountCache {
		if !accountLabeledForProject(acct.Labels, wantLabel) {
			continue
		}
		// Provider gate (ADR 0004 §4): filter to the harness's providers BEFORE
		// any health or load scoring, so a codex pod is only ever offered
		// provider=openai accounts and a claude pod never is. The harness comes
		// from spec.Harness, which applyAccountConfig resolves ahead of account
		// selection; an unresolved harness is claude.
		if !accountMatchesHarness(acct, harness) {
			continue
		}
		// Role account_whitelist (fail-closed): when the role declares a
		// whitelist, restrict the spread pool to its accounts. An account
		// outside the whitelist is never a spread candidate, so a whitelisted
		// role can never be credentialed against a forbidden account. Empty
		// whitelist imposes no restriction. Mirrors gb-init's intersectAccounts
		// filter, which is now a runtime no-op (ka-0000003061).
		if !accountAllowedByRoleWhitelist(cfg, spec.Role, name) {
			continue
		}
		// status active or empty (empty means active by default). Skip
		// exhausted/cooldown — those are not part of a spreadable pool.
		if acct.Status != "" && acct.Status != "active" {
			continue
		}
		// Skip accounts that would fail every agent baked against them: keyless
		// LiteLLM-proxied accounts (401) and overage-blocked accounts (billing
		// rejected). These are throttle-allowed with Status="", so without this
		// guard the spread pool would round-robin agents onto them and park them
		// idle — the ka-0000003769 idle-agent buildup. isHealthyAuthorized is
		// THE health predicate the pool gates on.
		if !isHealthyAuthorizedFor(acct, harness) {
			continue
		}
		// Served-model gate (ka-0000002790): skip an account whose baked model is
		// not in LiteLLM's served-model set. Such an account authenticates fine
		// (valid key, throttle-allowed) but has no deployed model behind it, so
		// every agent baked against it fails its first inference turn
		// (model-not-found via LiteLLM) → 0 tokens → SIGTERM crash-loop. This is
		// the glm-bedrock class: a TARGET model to smoke-test, labeled into a
		// project pool before its model is served, picked as lowest-load and baked
		// as the agent's own cognition. gateServed is nil unless the gate is
		// enabled AND the served set is known-non-empty, so this is a strict no-op
		// on the common path (see accountModelUnserved for the fail-open rules).
		// A codex account is subscription-authed and not routed through LiteLLM, so
		// the served-model set says nothing about it.
		if harness != beadsapi.HarnessCodex && accountModelUnserved(acct, gateServed, cfg.LiteLLMURL) {
			continue
		}
		// Skip accounts reserved to another role (exclusive_to_role): an exclusive
		// account is never part of a non-owner's spread pool. Data-driven
		// replacement for the Bedrock hardcode — a new exclusive account needs no
		// controller change (ka-0000005223). Owner roles never reach here: they
		// resolve via their role pin or short-circuit at roleOwnsExclusiveAccount.
		if ex := strings.TrimSpace(acct.ExclusiveToRole); ex != "" && !roleMatchesExclusive(spec.Role, ex) {
			continue
		}
		if acct.ThrottleStatus == "rejected" {
			// Currently rate-limited — exclude entirely.
			continue
		}
		// Fable-aware HARD GATE (ka-0000003806, decision ka-0000003299): exclude an
		// account whose rate-limit window for the SPEC'S model group is exhausted,
		// even when its aggregate 5h/7d reads throttle-allowed. Anthropic splits
		// subscription limits per model family, so a Fable-tapped account looks
		// healthy on the aggregate axis and would otherwise be picked as lowest-load,
		// 429-ing every Fable agent on turn 1. The group is resolved per-candidate
		// (specModelGroupFor) so a fable ROLE excludes every fable-tapped account
		// while a fable-DEFAULT account is judged on its own default. Fail-open: no
		// group data / unknown group → no exclusion (accountModelGroupExhausted).
		if group := specModelGroupFor(cfg, project, spec.Role, spec.RequestedModel, acct); accountModelGroupExhausted(acct, group) {
			continue
		}
		load := float64(liveCounts[name])
		if acct.ThrottleStatus == "allowed_warning" {
			// Near-limit but still serving 200s. Keep it in the SAME pool as the
			// healthy accounts rather than as a separate last-resort tier, but add
			// a load penalty so the least-loaded pick prefers a fully-healthy
			// account at low load and only spreads onto the warning account once
			// the healthy accounts have taken warningSpreadPenalty pods each.
			// Previously allowed_warning was a strictly-last-resort pool, so a
			// fresh warning account (e.g. 5h window reset, 0 live pods) sat idle
			// while every pod piled onto the one fully-healthy account and
			// re-saturated its per-account RPM/5h limit (operator report
			// 2026-06-29: matthewbaker1110 idle at util_5h=0 while gardenio climbed
			// past 0.89). The penalty puts it back in rotation as real,
			// de-prioritized capacity.
			load += warningSpreadPenalty
		}
		// Capacity+reset bonus (ka-0000001522): prefer accounts with unused
		// capacity in a binding window that resets soon — use-it-or-lose-it.
		// Capped at maxCapacityResetBonus so it biases the spread rather than
		// concentrating every pod on one account.
		load -= capacityResetBonus(acct, now)
		// Long-lived-role headroom bias (ka-0000001331): for hours-long roles
		// (gardener / plan-gardener, the Workflow-hosting spawners) prefer the
		// account with the most ABSOLUTE binding-window headroom and steer away
		// from accounts already near their limit, so a long-running agent does
		// not get routed onto an account that hits the wall mid-run. Short-lived
		// roles are unaffected — they churn fast enough that live-pod balance is
		// the right signal. No probe data → no adjustment (u ok == false).
		if longLived {
			if u, ok := bindingUtilization(acct); ok {
				load -= longLivedHeadroomWeight * (1 - u)
				if u > longLivedUtilCeiling {
					load += longLivedNearLimitPenalty
				}
			}
		}
		// Manual operator bias: spawn_weight ranks the account as if it carried
		// that many fewer (positive) or more (negative) live pods. Deliberately
		// uncapped — dogpiling on request is the operator's call. Non-finite
		// values are rejected: ParseFloat accepts "NaN"/"Inf" without error,
		// and a NaN load makes the sort comparator inconsistent while an Inf
		// load is bump-proof (live-count increments can never rebalance it).
		if w, err := strconv.ParseFloat(strings.TrimSpace(acct.SpawnWeight), 64); err == nil && !math.IsNaN(w) && !math.IsInf(w, 0) {
			load -= w
		}
		pool = append(pool, candidate{name: name, load: load})
	}

	if len(pool) == 0 {
		return ""
	}

	// Least-loaded by penalized live-pod count, tie-break alphabetically
	// (deterministic).
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].load != pool[j].load {
			return pool[i].load < pool[j].load
		}
		return pool[i].name < pool[j].name
	})
	return pool[0].name
}

// warningSpreadPenalty is the synthetic live-pod count added to an
// "allowed_warning" account when ranking spread candidates by least-loaded.
// A fully-healthy account is preferred until it carries this many pods, after
// which new pods spread onto the de-prioritized warning account. It is a small
// positive bias — large enough that a warning account is not the first pick
// when a healthy account is idle, small enough that a warning account with
// fresh capacity (0 live pods) is reached well before the healthy account
// re-saturates. Tunable; 4 means "prefer healthy until it holds 4 more pods
// than the warning account."
const warningSpreadPenalty = 4

// Capacity+reset bonus tuning (ka-0000001522).
const (
	// maxCapacityResetBonus caps the synthetic live-pod credit an account can
	// earn from unused capacity + reset proximity. The cap is what keeps this
	// a BIAS rather than a dogpile: even a fully-idle account about to reset
	// ranks at most this many pods "lighter" than its true live count, so the
	// least-loaded spread still takes over once the favored account
	// accumulates that much more load. Kept below warningSpreadPenalty so the
	// bonus can never promote an allowed_warning account over an equally
	// loaded fully-healthy one.
	maxCapacityResetBonus = 3.0

	// capacityResetHorizon is the reset-proximity window: the urgency
	// multiplier ramps 0→1 linearly as the binding window's reset approaches
	// from this far out. Beyond the horizon (or with no parsable reset time)
	// unused capacity earns a small baseline bonus only; inside it, idle
	// capacity becomes increasingly use-it-or-lose-it.
	capacityResetHorizon = 2 * time.Hour

	// capacityBaselineWeight scales the always-on unused-capacity term
	// (applies regardless of reset proximity), in synthetic pods per unit of
	// unused fraction. With all gardenio accounts sharing one reset time,
	// this is the term that actually separates them day-to-day.
	capacityBaselineWeight = 1.0

	// highUtilGuardrail is the binding-window utilization above which the
	// reset-proximity urgency term of capacityResetBonus is suppressed
	// (ka-0000001331). It stops the bonus from pulling MORE load onto an
	// account that is already climbing toward its limit as its reset nears —
	// the "nearly-exhausted account trips up a long-running agent" failure.
	// Above the guardrail an account still earns the small always-on baseline
	// for its remaining headroom, but not the ramped use-it-or-lose-it bonus.
	highUtilGuardrail = 0.7

	// Long-lived-role headroom bias (ka-0000001331). Roles like gardener /
	// plan-gardener run for hours and host Workflow fan-outs, so being routed
	// onto an account that hits its rate-limit wall mid-run is far more costly
	// than for a short-lived thread/crew agent. For these roles the spread adds
	// an ABSOLUTE-headroom preference (favor the account with the most unused
	// binding-window capacity, independent of live-pod count) and penalizes
	// accounts already near their limit.

	// longLivedHeadroomWeight scales the absolute-headroom preference for
	// long-lived roles, in synthetic pods per unit of unused binding-window
	// fraction. A fully-idle account thus ranks up to this many pods "lighter"
	// for a long-lived role than a fully-used one, on top of the normal
	// least-loaded term — so a long-running agent gravitates to the account
	// with the most runway before the wall. Kept modest so live-pod balance
	// still matters; it biases, it does not pin.
	longLivedHeadroomWeight = 2.0

	// longLivedUtilCeiling is the binding-window utilization above which an
	// account is a last resort for a long-lived role: crossing it applies
	// longLivedNearLimitPenalty. This is a soft guard (a penalty, not a hard
	// exclusion) so that when EVERY candidate is near its limit a long-lived
	// role still gets the least-bad account rather than a stranded spawn.
	longLivedUtilCeiling = 0.8

	// longLivedNearLimitPenalty is the synthetic live-pod penalty added, for a
	// long-lived role only, to an account whose binding util exceeds
	// longLivedUtilCeiling. Sized larger than longLivedHeadroomWeight so a
	// near-limit account cannot win a long-lived placement over any account
	// with real headroom, but finite so an all-near-limit pool still resolves.
	longLivedNearLimitPenalty = 6.0
)

// longLivedRoles are the agent roles whose pods run long enough (hours,
// often hosting Workflow fan-outs) that landing on a nearly-exhausted account
// is materially more harmful than for a short-lived agent — they are the
// "long-running agents (e.g. workflow runners)" the operator called out
// (ka-0000001331). selectAccountAt gives these roles an absolute-headroom
// preference and a near-limit penalty. A spec's Role may be a comma-separated
// list (e.g. "crew,thread"); roleIsLongLived treats the primary (first) role as
// authoritative, matching modeForRole's convention.
var longLivedRoles = map[string]bool{
	"gardener":      true,
	"plan-gardener": true,
}

// roleIsLongLived reports whether the primary role in a (possibly
// comma-separated) role string is one of the long-lived roles that should be
// biased toward absolute account headroom.
func roleIsLongLived(role string) bool {
	primary := role
	if i := strings.Index(role, ","); i >= 0 {
		primary = role[:i]
	}
	return longLivedRoles[strings.TrimSpace(primary)]
}

// bindingUtilization returns the probe-written utilization fraction (clamped to
// [0,1]) of the account's BINDING rate-limit window (RepresentativeClaim:
// five_hour | seven_day, defaulting to five_hour), and whether it was parseable.
// Both capacityResetBonus and the long-lived-role headroom bias
// (selectAccountAt) read the binding window through this one helper so
// they never disagree on which window binds or on how a free-form/non-finite
// value is handled. ok=false means the probe has not written usable data for the
// binding window (no data → callers give no bonus and no headroom preference).
func bindingUtilization(acct config.AccountCacheEntry) (float64, bool) {
	util := acct.Utilization5h
	if acct.RepresentativeClaim == "seven_day" {
		util = acct.Utilization7d
	}
	u, err := strconv.ParseFloat(strings.TrimSpace(util), 64)
	if err != nil || math.IsNaN(u) || math.IsInf(u, 0) {
		// Unparsable or non-finite (ParseFloat accepts "NaN"/"Inf" without
		// error, and NaN would poison the candidate's load) — no data.
		return 0, false
	}
	// Clamp defensively — the probe writes [0,1] but the field is free-form.
	return min(max(u, 0), 1), true
}

// capacityResetBonus returns the synthetic live-pod credit (≥ 0, capped at
// maxCapacityResetBonus) an account earns for unused capacity in its BINDING
// rate-limit window, scaled up as that window's reset approaches:
//
//	bonus = unused × (baseline + (max − baseline) × urgency)
//
// where unused = 1 − utilization of the binding window (representative_claim:
// five_hour | seven_day, defaulting to five_hour), and urgency ramps 0→1 over
// the last capacityResetHorizon before the window resets. Capacity left when
// the window rolls is simply lost, so an idle account near reset is the
// cheapest possible pod placement (ka-0000001522). Returns 0 when the account
// has no parsable utilization for its binding window (probe hasn't run) —
// no data, no bonus.
//
// Headroom guardrail (ka-0000001331): the reset-proximity urgency term is
// suppressed once the binding window is already heavily utilized
// (u > highUtilGuardrail). Without it, an account climbing toward its limit
// earns an INCREASING bonus as its reset approaches, pulling even more load onto
// it precisely as it nears exhaustion — the "nearly-exhausted account trips up a
// long-running agent" failure (operator report 2026-07-14: shannon at
// util_5h=0.76, ~2h from a 5h reset and the only allowed account, kept ramping
// its bonus). Above the guardrail an account still earns the small always-on
// baseline for whatever headroom remains — which correctly still prefers the
// more-headroom of two high-util accounts — but never the ramped
// use-it-or-lose-it bonus.
func capacityResetBonus(acct config.AccountCacheEntry, now time.Time) float64 {
	u, ok := bindingUtilization(acct)
	if !ok {
		return 0
	}
	unused := 1 - u

	reset := acct.ThrottleReset5hAt
	if acct.RepresentativeClaim == "seven_day" {
		reset = acct.ThrottleReset7dAt
	}
	urgency := 0.0
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(reset)); err == nil {
		if until := t.Sub(now); until > 0 && until < capacityResetHorizon {
			urgency = 1 - until.Seconds()/capacityResetHorizon.Seconds()
		} else if until <= 0 {
			// Reset time in the past means the probe data is stale — the window
			// has already rolled and the utilization no longer describes it.
			// Treat as no urgency rather than max: don't dogpile on stale data.
			urgency = 0
		}
	}
	if u > highUtilGuardrail {
		// Near-limit: kill reset-proximity amplification (see doc comment).
		urgency = 0
	}
	return unused * (capacityBaselineWeight + (maxCapacityResetBonus-capacityBaselineWeight)*urgency)
}

// countCredentialedProjectAccounts returns how many accounts in the cache are
// labeled project:<project> AND carry a usable credential (a LiteLLM key or an
// OAuth/secret credential). It is used purely for observability: if this count
// is > 0 yet account resolution selected nothing, the resolver wrongly excluded
// every candidate (e.g. an overage/throttle/exhausted gate misfiring) and pods
// would spawn with no ANTHROPIC_* env — a condition applyAccountConfig now logs
// loudly instead of swallowing (ka-0000007343).
func countCredentialedProjectAccounts(cfg *config.Config, project, harness string) int {
	if project == "" {
		return 0
	}
	wantLabel := "project:" + project
	n := 0
	for _, acct := range cfg.AccountCache {
		if !accountLabeledForProject(acct.Labels, wantLabel) {
			continue
		}
		if !accountMatchesHarness(acct, harness) {
			continue
		}
		if acct.LiteLLMKey != "" || acct.Credential != "" {
			n++
		}
	}
	return n
}

// projectPinnedAccount returns the account name a project bead pins via its
// `account:` field, or "" when the project is unknown or pins nothing.
//
// The field is NO LONGER a resolution input — Phase 2 (ka-0000004094) retired the
// layer-3 project-cache fall-through that read it, and account resolution now
// keys entirely off the project:<name> label on account beads. This reader exists
// only so the no-account refusal path can tell "project legitimately uses direct
// provider auth" apart from "project pins an account nobody labeled", which is
// dead config that silently produced credential-less pods (ka-0000006314).
//
// Takes ProjectCacheMu for the read: applyAccountConfig's caller
// (applyCommonConfig) does not hold it, and the periodic sync goroutine writes
// the map via refreshProjectCache.
func projectPinnedAccount(cfg *config.Config, project string) string {
	if project == "" {
		return ""
	}
	cfg.ProjectCacheMu.RLock()
	defer cfg.ProjectCacheMu.RUnlock()
	entry, ok := cfg.ProjectCache[project]
	if !ok {
		return ""
	}
	return strings.TrimSpace(entry.Account)
}

// accountModelUnserved reports whether an account's baked model is confidently
// KNOWN to be unserved by the LiteLLM proxy it routes through — the signal that
// baking it as cognition would crash-loop the pod (ka-0000002790). It is
// deliberately fail-open: it returns false (do NOT exclude) unless every
// condition below holds, so a missing served set, a foreign proxy, or a
// model-less account never wrongly strips a healthy account from the pool.
//
//   - served == nil / empty: the served-model set is UNKNOWN (the sync has not
//     run, is disabled, or its last fetch failed). No data → no gating.
//   - acct.Model == "": no model is baked, so the agent inherits the
//     controller-global model, which is served by construction. Not gated.
//   - the account does not route through the SAME LiteLLM the served set was
//     fetched from (accountUsesLiteLLM): its served set is one we cannot see
//     (e.g. an external devops proxy), so we cannot judge its model. Not gated.
//
// Only when the served set is known, the account uses our proxy, and its model
// is absent from that set do we return true — an undeployed-model account that
// would fail every request.
func accountModelUnserved(acct config.AccountCacheEntry, served map[string]bool, litellmURL string) bool {
	if len(served) == 0 {
		return false
	}
	model := strings.TrimSpace(acct.Model)
	if model == "" {
		return false
	}
	if !accountUsesLiteLLM(acct, litellmURL) {
		return false
	}
	return !served[model]
}

// accountUsesLiteLLM reports whether an account's LiteLLM base_url is the SAME
// proxy the served-model set describes (the controller-global cfg.LiteLLMURL).
// An account with an empty base_url falls back to that global URL at spawn (see
// applyAccountConfig's baseURL resolution), so it is served by our proxy too.
// An account pointed at a different proxy has a served set we did not fetch, so
// accountModelUnserved must not judge its model against ours. Comparison is
// trailing-slash- and case-insensitive. proxy_url (claudemore) is intentionally
// ignored: claudemore forwards to the SAME litellm, so the served set behind it
// is unchanged — base_url is what names the model-serving proxy.
func accountUsesLiteLLM(acct config.AccountCacheEntry, litellmURL string) bool {
	global := strings.TrimRight(strings.TrimSpace(litellmURL), "/")
	b := strings.TrimRight(strings.TrimSpace(acct.BaseURL), "/")
	if b == "" {
		// Empty base_url uses the controller-global litellm at spawn — served by
		// our proxy iff a global URL is configured.
		return global != ""
	}
	return global != "" && strings.EqualFold(b, global)
}

// accountLabeledForProject reports whether an account's raw bead labels contain
// the given "project:<name>" label.
func accountLabeledForProject(labels []string, wantLabel string) bool {
	for _, l := range labels {
		if l == wantLabel {
			return true
		}
	}
	return false
}

// liteLLMKeyFromHeader inverts litellmHeaderFor: it extracts the LiteLLM virtual
// key from a baked ANTHROPIC_CUSTOM_HEADERS value. Returns "" if the header
// does not carry an x-litellm-api-key line. Kept in lockstep with
// litellmHeaderFor / applyAccountConfig.
func liteLLMKeyFromHeader(header string) string {
	const prefix = "x-litellm-api-key: Bearer "
	// Multiple headers are newline-separated (applyAccountConfig appends an
	// x-litellm-tags line); scan each line for the api-key.
	for _, line := range strings.Split(header, "\n") {
		if k, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(k)
		}
	}
	return ""
}

// podLiteLLMKey returns the LiteLLM key baked into a pod's agent container, or
// "" if none. Mirrors podHasCustomHeader's container/env walk.
func podLiteLLMKey(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != "agent" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "ANTHROPIC_CUSTOM_HEADERS" {
				return liteLLMKeyFromHeader(e.Value)
			}
		}
	}
	return ""
}

// countLivePodsByAccount buckets running agent pods by the account whose LiteLLM
// key they carry, using the cfg.AccountCache key→name reverse mapping. Pods that
// are terminal (Failed/Succeeded) or carry an unrecognized key are skipped. The
// returned map is keyed by account NAME so selectAccount can look up
// candidates directly.
func countLivePodsByAccount(pods []corev1.Pod, accounts map[string]config.AccountCacheEntry) map[string]int {
	// Build LiteLLM-key → account-name reverse index.
	keyToAccount := make(map[string]string, len(accounts))
	for name, acct := range accounts {
		if acct.LiteLLMKey != "" {
			keyToAccount[acct.LiteLLMKey] = name
		}
	}
	counts := make(map[string]int)
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		key := podLiteLLMKey(pod)
		if key == "" {
			continue
		}
		if name, ok := keyToAccount[key]; ok {
			counts[name]++
		}
	}
	return counts
}

// refreshAccountLiveCounts replaces cfg.AccountLiveCounts with a fresh snapshot
// derived from the current live agent pod list. Called once per periodic sync
// cycle. The spawn path reads this snapshot and optimistically bumps it (see
// config.Config.BumpAccountLiveCount) so a burst of spawns within one window
// still spreads.
func refreshAccountLiveCounts(ctx context.Context, logger *slog.Logger, pods podmanager.Manager, namespace string, cfg *config.Config) {
	agentPods, err := pods.ListAgentPods(ctx, namespace, map[string]string{
		podmanager.LabelApp: podmanager.LabelAppValue,
	})
	if err != nil {
		logger.Warn("failed to list agent pods for account live-count refresh", "error", err)
		return
	}
	cfg.ProjectCacheMu.RLock()
	accounts := cfg.AccountCache
	cfg.ProjectCacheMu.RUnlock()

	counts := countLivePodsByAccount(agentPods, accounts)
	cfg.SetAccountLiveCounts(counts)
}
