package main

import (
	"log/slog"
	"sort"
	"strings"

	"gardenio/controller/internal/config"
)

// account_pool_validator.go — account-cache health validators run once per
// project-cache refresh (ka-0000002560). These are OBSERVABILITY hooks: they do
// not change resolution, they emit loud WARNs so the misconfigurations that
// produced the 2026-06-29 monorepo exhaustion incident (a silent CIRCULAR
// fallback chain, and a project pool with no self-healing account) surface to
// operators before — or as — pods start failing, rather than being diagnosed by
// hand after a fleet-wide stall.

// validateAccountPools runs every account-cache validator and emits WARN logs
// for the problems it finds. Called from refreshProjectCache after the account
// cache is rebuilt, under the caller's ProjectCacheMu write lock (read-only over
// the cache). A no-op for an empty cache.
func validateAccountPools(logger *slog.Logger, accounts map[string]config.AccountCacheEntry) {
	if len(accounts) == 0 {
		return
	}

	// Criterion 1: a CIRCULAR fallback chain (e.g. the monorepo
	// gardenio2 → gardenio3 → matthewbaker-acme → gardenio2 loop) is walked
	// cycle-guarded by both the reactive failover path (findHealthyFallback) and
	// the spawn path (accountFallbackChain) — but both bail SILENTLY at the first
	// repeat. A cycle is almost always an operator typo (a fallback pointer that
	// should terminate at a spillover key instead loops home), and it defeats the
	// terminal-escape semantics the fallback chain is supposed to provide. Warn
	// loudly, once per distinct cycle.
	for _, cycle := range detectFallbackCycles(accounts) {
		logger.Warn("circular account fallback chain detected — the chain loops instead of terminating at a spillover/default account, so exhaustion has no terminal escape hatch; break the cycle by pointing the last account's fallback at a healthy spillover key (or clearing it)",
			"cycle", strings.Join(cycle, " -> "), "cycle_len", len(cycle))
	}

	// Criterion 3: a project label that resolves to NO healthy-or-recoverable
	// account is a CONFIG-time error, not a transient runtime stall: every account
	// authorized for the project is permanently unfit (operator-disabled, a
	// keyless LiteLLM misconfig that 401s every agent, or a non-self-healing
	// billing block). Such a pool will NOT recover on its own when a rate-limit
	// window resets — it needs operator action — so it is worth a louder, distinct
	// signal than the transient all-exhausted case (which the spawn-time escape
	// hatch handles by deferring). Pools whose accounts are merely exhausted (and
	// will reset) are deliberately NOT flagged here.
	//
	// This is also the empty-pool no-credential-spawn audit. Since the terminal
	// cfg.DefaultAccount fall-through was removed (account-routing.md §7 Phase 3,
	// ka-0000007959), a stranded pool has no last-resort safety net at all: when
	// its whole pool is momentarily empty, spawn-time selection is spread-only and
	// resolves to nothing, so a pod spawns with no ANTHROPIC_* env → synthetic
	// turns with no loud signal (the ka-0000007343 outage class). Fixing or
	// relabeling a healthy account into the pool is the only remedy.
	for project, n := range findStrandedProjectPools(accounts) {
		logger.Warn("project pool has accounts but NONE are healthy or recoverable — every project:<name>-labeled account is operator-disabled, keyless-misconfigured, or billing-blocked, so pods cannot resolve a usable credential and waiting will not help; there is no last-resort DEFAULT_ACCOUNT net (removed in Phase 3, ka-0000007959), so an all-empty pool spawns pods with NO credential → synthetic turns (ka-0000007343); fix or re-key an account, or relabel a healthy one",
			"project", project, "labeled_accounts", n)
	}
}

// detectFallbackCycles returns the distinct circular fallback chains in the
// account cache. Each account has out-degree ≤ 1 (a single `Fallback` pointer),
// so the cache is a functional graph: every weakly-connected component is a set
// of trees feeding into at most one cycle. Each returned slice is one cycle's
// member names, sorted for deterministic output. A self-referential account
// (A.fallback == A) is a length-1 cycle.
//
// Pure (no I/O) so it is unit-testable. Cross-account fallback pointers to
// unknown accounts terminate the walk (no cycle), matching accountFallbackChain.
func detectFallbackCycles(accounts map[string]config.AccountCacheEntry) [][]string {
	// done marks accounts whose component has already been fully explored, so a
	// later start that walks into it stops without re-reporting its cycle. The
	// functional-graph structure guarantees each cycle is discovered exactly once
	// (by the first walk that enters its component).
	done := make(map[string]bool, len(accounts))
	var cycles [][]string

	names := make([]string, 0, len(accounts))
	for name := range accounts {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, start := range names {
		if done[start] {
			continue
		}
		// Walk the single-successor chain from start, recording each node's
		// position so a repeat reveals the cycle's entry point.
		pos := make(map[string]int)
		var path []string
		cur := start
		for cur != "" {
			if at, inPath := pos[cur]; inPath {
				// Found a cycle: path[at:] are its members.
				cycle := append([]string(nil), path[at:]...)
				sort.Strings(cycle)
				cycles = append(cycles, cycle)
				break
			}
			if done[cur] {
				// Walked into an already-explored component; its cycle (if any)
				// was already recorded. Stop without double-reporting.
				break
			}
			acct, ok := accounts[cur]
			if !ok {
				break // dangling fallback pointer — no cycle
			}
			pos[cur] = len(path)
			path = append(path, cur)
			cur = strings.TrimSpace(acct.Fallback)
		}
		for _, n := range path {
			done[n] = true
		}
	}
	return cycles
}

// findStrandedProjectPools returns the projects whose entire account pool is
// CONFIG-stranded: the project has ≥ 1 project:<name>-labeled account, but every
// one of them is permanently unfit (accountHealthyOrRecoverable == false). The
// value is the count of labeled accounts for that project. Projects with at
// least one healthy-or-recoverable account are omitted. Pure (no I/O).
func findStrandedProjectPools(accounts map[string]config.AccountCacheEntry) map[string]int {
	labeled := make(map[string]int)
	recoverable := make(map[string]bool)
	for _, acct := range accounts {
		for _, l := range acct.Labels {
			project, ok := strings.CutPrefix(l, "project:")
			if !ok || project == "" {
				continue
			}
			labeled[project]++
			if accountHealthyOrRecoverable(acct) {
				recoverable[project] = true
			}
		}
	}
	stranded := make(map[string]int)
	for project, n := range labeled {
		if !recoverable[project] {
			stranded[project] = n
		}
	}
	return stranded
}

// accountHealthyOrRecoverable reports whether an account is either usable now or
// will become usable on its own once a rate-limit window resets — the negation
// is "permanently unfit until an operator intervenes". An account is NOT
// recoverable when it is:
//
//   - operator-disabled (Status == "disabled") — stays down until re-enabled;
//   - a keyless LiteLLM misconfig (BaseURL set, LiteLLMKey empty) — every agent
//     401s until an operator adds the virtual key;
//   - billing-blocked by a non-self-healing overage reason (overageReasonBlocks).
//
// A merely capacity-exhausted account (Status == "exhausted", over budget, or
// throttle-rejected) IS recoverable: its window resets without operator action,
// so its project pool is a transient runtime condition (handled by the spawn
// escape hatch), NOT a config error. Used only by findStrandedProjectPools.
func accountHealthyOrRecoverable(acct config.AccountCacheEntry) bool {
	if acct.Status == "disabled" {
		return false
	}
	if acct.BaseURL != "" && acct.LiteLLMKey == "" {
		return false
	}
	if overageReasonBlocks(acctBillingType(acct), acct.OverageDisabledReason) {
		return false
	}
	return true
}
