package main

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// refreshProjectCache queries the daemon for project and account beads and updates
// cfg.ProjectCache and cfg.AccountCache.
func refreshProjectCache(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, cfg *config.Config) {
	rigs, err := daemon.ListProjectBeads(ctx)
	if err != nil {
		logger.Warn("failed to refresh project cache", "error", err)
		return
	}

	// Fetch account beads for LLM provider routing (type:account config beads).
	accounts, err := daemon.ListAccountBeads(ctx)
	if err != nil {
		logger.Warn("failed to refresh account cache", "error", err)
		// Non-fatal: continue with project cache refresh.
	}

	// Build the per-project active-account override from account bead labels.
	// An account labeled project:<name> with status "active" (or empty) is in
	// that project's pool; we pick the alphabetically-first active match as
	// the project cache's effective Account so failover is driven entirely
	// by account bead status instead of a separate config:auth pointer.
	projectAccountOverride := buildProjectAccountOverride(accounts)

	cfg.ProjectCacheMu.Lock()
	for name, info := range rigs {
		entry := config.ProjectCacheEntry{
			Prefix:            info.Prefix,
			GitURL:            info.GitURL,
			DefaultBranch:     info.DefaultBranch,
			Image:             info.Image,
			StorageClass:      info.StorageClass,
			ServiceAccount:    info.ServiceAccount,
			RuntimeClassName:  info.RuntimeClassName,
			DockerEnabled:     info.DockerEnabled,
			PostgresEnabled:   info.PostgresEnabled,
			NeedsK8sAPI:       info.NeedsK8sAPI,
			ZitiAccessEnabled: info.ZitiAccessEnabled,
			ZitiServiceID:     info.ZitiServiceID,
			ZitiBind:          info.ZitiBind,
			ZitiTTL:           info.ZitiTTL,
			ZitiOptional:      info.ZitiOptional,
			ExecutionTarget:   info.ExecutionTarget,
			SessionResume:     info.SessionResume,
			Ultracode:         info.Ultracode,
			CPURequest:        info.CPURequest,
			CPULimit:          info.CPULimit,
			MemoryRequest:     info.MemoryRequest,
			MemoryLimit:       info.MemoryLimit,
			Account:           info.Account,
			Model:             info.Model,
			Harness:           info.Harness,
			EnvOverrides:      info.EnvOverrides,
			Secrets:           info.Secrets,
			EnvVars:           info.EnvVars,
			Repos:             info.Repos,
			PrewarmedPool:     info.PrewarmedPool,
			LandKind:          info.LandKind,
			Namespace:         info.Namespace,
			NatsEpoch:         info.NatsEpoch,

			GithubApp:            info.GithubApp,
			GithubInstallationID: info.GithubInstallationID,
		}
		// Apply label-aware account override if one exists for this project.
		if override, ok := projectAccountOverride[name]; ok {
			entry.Account = override
		} else if entry.Account != "" && accounts != nil {
			// ka-0000000313: detect "project bead points at exhausted account,
			// no labeled alternative" — loudly warn so operators get a heads-up
			// before pods spawn against an exhausted key. applyAccountConfig's
			// exhausted-account guard is the runtime safety net; this is the
			// observability hook so the underlying misconfiguration (no
			// project:<name> labels on healthy accounts) gets fixed.
			if acct, ok := accounts[entry.Account]; ok {
				switch {
				case !accountLabeledForProject(acct.Labels, "project:"+name):
					// ka-0000006314: the pinned account is not label-visible to this
					// project at all, so NOTHING resolves — role-pin, spread, and
					// opus-fallback all decline and applyAccountConfig bakes no
					// credential. Distinct from (and checked before) the exhausted
					// case: an unlabeled pin is broken regardless of the account's
					// health, and it is the actionable root cause. Silent until now
					// because this hook only looked at Status, which for a healthy
					// unlabeled account is "" (the devsecops outage: pods spawned
					// credential-less from 2026-08-12 to 2026-08-27 and every agent
					// 401'd on request 1 with zero tokens).
					logger.Warn("project pins an account that carries no project:<name> label — the account: field is not read on the spawn path (layer 3 retired in Phase 2, ka-0000004094), so pods for this project spawn with NO ANTHROPIC_* env and 401 on their first request; add the label to the pinned account bead",
						"project", name, "account", entry.Account,
						"want_label", "project:"+name, "account_status", acct.Status)
				case acct.Status == "exhausted":
					logger.Warn("project points at exhausted account and no labeled alternative is available; with no last-resort DEFAULT_ACCOUNT net (removed in Phase 3, ka-0000007959) an all-empty pool blocks the spawn — add project:<name> labels to healthy accounts",
						"project", name, "account", entry.Account, "account_status", acct.Status)
				}
			}
		}
		cfg.ProjectCache[name] = entry
	}
	if accounts != nil {
		cfg.AccountCache = make(map[string]config.AccountCacheEntry, len(accounts))
		for name, acct := range accounts {
			cfg.AccountCache[name] = config.AccountCacheEntry{
				Provider:              acct.Provider,
				BaseURL:               acct.BaseURL,
				ProxyURL:              acct.ProxyURL,
				Credential:            acct.Credential,
				Model:                 acct.Model,
				MaxBudget:             acct.MaxBudget,
				Fallback:              acct.Fallback,
				LiteLLMKey:            acct.LiteLLMKey,
				Status:                acct.Status,
				ThrottleStatus:        acct.Fields["throttle_status"],
				ThrottleResetAt:       acct.Fields["throttle_reset_at"],
				Utilization5h:         acct.Fields["utilization_5h"],
				Utilization7d:         acct.Fields["utilization_7d"],
				ThrottleReset5hAt:     acct.Fields["throttle_reset_5h_at"],
				ThrottleReset7dAt:     acct.Fields["throttle_reset_7d_at"],
				RepresentativeClaim:   acct.Fields["representative_claim"],
				SpawnWeight:           acct.Fields["spawn_weight"],
				BillingType:           acct.Fields["billing_type"],
				OverageDisabledReason: acct.Fields["overage_disabled_reason"],
				ProbeAuthStatus:       acct.Fields["probe_auth_status"],
				TotalCostUSD:          acct.Fields["total_cost_usd"],
				SpendAtLastReset:      acct.Fields["spend_at_last_reset"],
				ExclusiveToRole:       acct.Fields["exclusive_to_role"],
				ModelGroupLimits:      acct.Fields["model_group_limits"],
				Labels:                acct.Labels,
			}
		}
		// Account-cache health validators (ka-0000002560): emit loud WARNs for a
		// circular fallback chain (no terminal escape hatch) and any project pool
		// stranded with no healthy-or-recoverable account. Observability only —
		// resolution is unchanged. Runs under the write lock over the freshly
		// rebuilt cache.
		validateAccountPools(logger, cfg.AccountCache)
	}
	cfg.ProjectCacheMu.Unlock()
	logger.Info("refreshed project cache", "projects", len(rigs), "accounts", len(accounts),
		"auth_overrides", len(projectAccountOverride))
}

// buildProjectAccountOverride derives a per-project override map from account
// bead labels: for each project:<name>-labeled account whose status is empty
// or "active", it records the project's deterministic last-resort account.
//
// Role after abolish-primary Phase 4 (ka-0000005978): this is NOT a privileged
// "primary home" and NOT a switchback target — it is the empty-pool TIEBREAKER
// only. On the normal path applyAccountConfig calls selectAccount FIRST
// and spreads new pods across the whole healthy pool; the value computed here is
// consulted as ProjectCache[project].Account only for spend attribution
// (resolveSpendAccount) — Phase 2 (ka-0000004094) removed the project-cache tail
// from the spawn path and Phase 3 (ka-0000007959) removed the terminal
// DEFAULT_ACCOUNT fall-through, so an empty spread pool now blocks the spawn
// rather than baking a last-resort credential. The stranded-pool no-credential
// hazard is audited at cache-refresh by findStrandedProjectPools.
//
// The explicit primary marker (field primary=="true") is DEMOTED, not honored as
// a privileged home: it survives here only as the empty-pool tiebreaker (a
// marked account still wins the last-resort pick over an alphabetical sibling)
// and it must keep skipping non-active marked accounts (the ka-0000002933 guard)
// so a cooked marked account is never picked. Phase 6 strips the marker from
// beads entirely; until then this ordering is a no-op on the happy path because
// spread already decided the account before this value is ever read. With no
// marker present, the alphabetically-first active match wins. This replaces the
// old config:auth bead lookup — authorized accounts now declare their projects
// via labels, and exhausted/cooldown accounts are skipped automatically.
func buildProjectAccountOverride(accounts map[string]beadsapi.AccountInfo) map[string]string {
	override := make(map[string]string)
	if len(accounts) == 0 {
		return override
	}
	names := make([]string, 0, len(accounts))
	for name := range accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	// First pass: honor explicit primary markers.
	for _, name := range names {
		acct := accounts[name]
		if acct.Status != "" && acct.Status != "active" {
			continue
		}
		if accountInfoUnusable(acct) {
			continue
		}
		if acct.Fields["primary"] != "true" {
			continue
		}
		for _, label := range acct.Labels {
			project, ok := strings.CutPrefix(label, "project:")
			if !ok || project == "" {
				continue
			}
			if _, seen := override[project]; !seen {
				override[project] = name
			}
		}
	}
	// Second pass: fall back to alphabetical-first for projects without
	// a marked primary. Skips projects already resolved above.
	for _, name := range names {
		acct := accounts[name]
		if acct.Status != "" && acct.Status != "active" {
			continue
		}
		if accountInfoUnusable(acct) {
			continue
		}
		for _, label := range acct.Labels {
			project, ok := strings.CutPrefix(label, "project:")
			if !ok || project == "" {
				continue
			}
			if _, seen := override[project]; !seen {
				override[project] = name
			}
		}
	}
	return override
}

// accountInfoUnusable reports whether an account bead is unfit to be selected as
// a project's effective account, mirroring !isHealthyAuthorized (which operates
// on the cache entry) at the AccountInfo level used during cache refresh. An
// account is unusable when it routes through a LiteLLM proxy with no virtual key
// (every agent 401s) or when its billing is disabled by a genuinely-blocking
// overage reason (overageReasonBlocks). NOTE: the subscription-overage reasons
// org_spend_cap_reached AND out_of_credits are NOT blocking — they describe
// pay-as-you-go overage availability, not the subscription quota that our Max
// OAuth tokens bill against (ka-0000003769, ka-0000004929). The keyless guard
// catches throttle-allowed Status="" accounts that would otherwise be picked as
// a project's primary and park agents idle (ka-0000003769).
func accountInfoUnusable(acct beadsapi.AccountInfo) bool {
	if acct.BaseURL != "" && acct.LiteLLMKey == "" {
		return true
	}
	if overageReasonBlocks(billingType(acct.Fields), acct.Fields["overage_disabled_reason"]) {
		return true
	}
	return false
}

// refreshRoleCache queries the daemon for role beads and updates cfg.RoleCache.
func refreshRoleCache(ctx context.Context, logger *slog.Logger, daemon *beadsapi.Client, cfg *config.Config) {
	roles, err := daemon.ListRoleBeads(ctx)
	if err != nil {
		logger.Warn("failed to refresh role cache", "error", err)
		return
	}

	cfg.RoleCacheMu.Lock()
	cfg.RoleCache = make(map[string]config.RoleCacheEntry, len(roles))
	for name, info := range roles {
		cfg.RoleCache[name] = config.RoleCacheEntry{
			Image:             info.Image,
			StorageClass:      info.StorageClass,
			ServiceAccount:    info.ServiceAccount,
			RuntimeClassName:  info.RuntimeClassName,
			DockerEnabled:     info.DockerEnabled,
			PostgresEnabled:   info.PostgresEnabled,
			NeedsK8sAPI:       info.NeedsK8sAPI,
			Verifier:          info.Verifier,
			MachineClient:     info.MachineClient,
			ZitiAccessEnabled: info.ZitiAccessEnabled,
			ZitiServiceID:     info.ZitiServiceID,
			ZitiBind:          info.ZitiBind,
			ZitiTTL:           info.ZitiTTL,
			ZitiOptional:      info.ZitiOptional,
			ExecutionTarget:   info.ExecutionTarget,
			SessionResume:     info.SessionResume,
			Ultracode:         info.Ultracode,
			CPURequest:        info.CPURequest,
			CPULimit:          info.CPULimit,
			MemoryRequest:     info.MemoryRequest,
			MemoryLimit:       info.MemoryLimit,
			Secrets:           info.Secrets,
			EnvVars:           info.EnvVars,
			EnvOverrides:      info.EnvOverrides,
			ClaudeConfig:      info.ClaudeConfig,
			Account:           info.Account,
			Model:             info.Model,
			Harness:           info.Harness,
			AccountWhitelist:  info.AccountWhitelist,
			NoAccountSpill:    info.NoAccountSpill,
		}
	}
	cfg.RoleCacheMu.Unlock()
	logger.Info("refreshed role cache", "roles", len(roles))

	// Referential check: warn (don't fail) when an active agent names a role
	// with no type:role bead. Roles stay free strings with a soft warning so
	// dynamically-created roles aren't blocked before their config bead lands
	// (ka-0000001425, design doc §8.5).
	agents, err := daemon.ListAgentBeadsLean(ctx)
	if err != nil {
		logger.Warn("role validation: failed to list agent beads", "error", err)
		return
	}
	for _, w := range beadsapi.ValidateAgentRoles(agents, roles, builtinRoles) {
		logger.Warn("role validation", "warning", w)
	}
}

// builtinRoles are roles that are first-class in controller/bridge code (seeded
// as role:<name> labels by init.go and used on the pod-spawn path) but may not
// have a type:role config bead. They never trigger an unknown-role warning.
var builtinRoles = map[string]bool{
	"thread":   true,
	"crew":     true,
	"gardener": true,
	"reviewer": true,
	"job":      true,
	// The unified plan-agent role (epic ka-0000000081): gb plan gardener and the
	// viewer's plan lane both spawn into it. A type:role bead (ka-0000003712)
	// normally pins its model/runtime_class; listing it here keeps a fresh env
	// without that bead from warning on every plan-gardener iteration.
	"plan-gardener": true,
}
