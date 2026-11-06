// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Fallback chain reader and project-scoped rebalance logic.
//!
//! Caches account beads and auth configs from the beads daemon, walks
//! per-account fallback chains, and reassigns sessions when an account
//! is throttled — using the project-specific fallback rather than the
//! generic least-loaded assignment that `rebalance_from_account()` uses.
//!
//! Wired into the throttle watcher once the beads client is available.

use std::collections::HashMap;
use std::sync::Arc;

use crate::state::MuxState;

// ── Beads client types (defined locally until types are unified) ─────────

/// Account bead as returned by the beads daemon.
///
/// Mirrors the expected shape from `BeadsClient::list_account_beads()`.
#[derive(Debug, Clone)]
pub struct AccountBead {
    pub name: String,
    /// Bead ID (for updating the bead via the daemon API).
    pub id: String,
    /// Next account in the fallback chain, if any.
    pub fallback: Option<String>,
    /// Provider identifier: "claude", "openai", "gemini", etc.
    pub provider: String,
    /// Current operational status: "active", "throttled", etc.
    pub status: String,
    /// K8s secret name holding the credential for this account.
    pub credential_secret: String,
    // Rate-limit probe fields (populated by controller ratelimit_probe; empty if not yet probed)
    pub throttle_status: String,
    pub utilization_5h: String,
    pub utilization_7d: String,
    pub throttle_reset_at: String,
    pub representative_claim: String,
    /// Per-account billing window length in hours (e.g. "5"). Empty if unset.
    pub billing_period_hours: String,
    /// Raw bead labels, including `project:<name>` pool membership.
    pub labels: Vec<String>,
}

/// Auth config bead linking a project to its active account.
///
/// Mirrors the expected shape from `BeadsClient::list_auth_configs()`.
#[derive(Debug, Clone)]
pub struct AuthConfig {
    pub bead_id: String,
    /// The account currently active for this project.
    pub account: String,
    /// The primary (preferred) account for this project, if set.
    pub primary_account: Option<String>,
    /// Project name this config applies to.
    pub project: String,
}

/// Role-account pin from a `type=role` config bead.
///
/// Mirrors the expected shape from `BeadsClient::list_role_accounts()`.
/// Used by coopmux to give role-pinned sessions the right credentials
/// at register time. See ka-0000002136.
#[derive(Debug, Clone)]
pub struct RoleAccount {
    /// Role name (the role bead's title, e.g. `fsm-triage`).
    pub role: String,
    /// Account name to use for sessions running this role.
    pub account: String,
}

/// Trait abstracting the beads daemon client.
///
/// Implemented by the real `BeadsClient` (in beads.rs) and by test doubles.
pub trait BeadsSource {
    fn list_account_beads(
        &self,
    ) -> impl std::future::Future<Output = anyhow::Result<Vec<AccountBead>>> + Send;

    fn list_auth_configs(
        &self,
    ) -> impl std::future::Future<Output = anyhow::Result<Vec<AuthConfig>>> + Send;

    fn list_role_accounts(
        &self,
    ) -> impl std::future::Future<Output = anyhow::Result<Vec<RoleAccount>>> + Send;
}

// ── Account info ────────────────────────────────────────────────────────

/// Cached info for a single account.
#[derive(Debug, Clone, Default)]
pub struct AccountInfo {
    pub name: String,
    pub bead_id: String,
    pub fallback: Option<String>,
    pub provider: String,
    pub status: String,
    pub credential_secret: String,
    // Probe fields (from ratelimit_probe in controller; may be empty before first probe)
    pub throttle_status: String,
    pub utilization_5h: String,
    pub utilization_7d: String,
    pub throttle_reset_at: String,
    pub representative_claim: String,
    /// Per-account billing window length in hours (e.g. "5"). Drives the
    /// cooldown_until horizon written when this account is marked exhausted.
    /// Empty if unset (defaults applied at write time).
    pub billing_period_hours: String,
    /// Raw bead labels, including `project:<name>` pool membership. The
    /// authorized-account set for a project is built from accounts carrying
    /// the matching `project:<name>` label (the spread candidate set), NOT
    /// from the per-account fallback chain.
    pub labels: Vec<String>,
}

/// Cached info for a project's auth config.
#[derive(Debug, Clone)]
pub struct AuthConfigInfo {
    pub bead_id: String,
    pub account: String,
    pub primary_account: Option<String>,
    pub project: String,
}

// ── AccountCache ────────────────────────────────────────────────────────

/// Cached view of account beads and auth configs from the beads daemon.
///
/// Periodically refreshed to stay in sync with the control plane. Provides
/// fallback chain resolution and project→account lookups needed for
/// project-scoped rebalancing.
///
/// `Clone` (cheap: small owned HashMaps) lets a caller snapshot the cache and
/// drop the read guard before blocking I/O under it (ka-0000004551).
#[derive(Clone)]
pub struct AccountCache {
    /// Account name → account info.
    accounts: HashMap<String, AccountInfo>,
    /// Project name → auth config info.
    auth_configs: HashMap<String, AuthConfigInfo>,
    /// Role name → account name (from `type=role` beads with `fields.account`).
    /// Sessions whose `metadata.role` matches one of these pins get the
    /// listed account as their preferred assignment, overriding the project
    /// default. See [`AccountCache::account_for_role`].
    role_accounts: HashMap<String, String>,
}

impl AccountCache {
    /// Create an empty cache.
    pub fn new() -> Self {
        Self {
            accounts: HashMap::new(),
            auth_configs: HashMap::new(),
            role_accounts: HashMap::new(),
        }
    }

    /// Refresh the cache in place from the beads daemon.
    ///
    /// Thin wrapper over [`AccountCache::fetch`]: builds a fresh cache from the
    /// daemon and swaps it into `self`. Returns an error if a required RPC
    /// fails (caller decides retry strategy).
    ///
    /// **Do not call this while holding the shared account-cache write lock** —
    /// it awaits three sequential HTTP RPCs (each up to the beads client's 30s
    /// timeout), so holding the write guard across it starves every
    /// credential-plane reader for the whole window (ka-0000000143). The refresh
    /// loop uses `fetch` off-lock and swaps under a brief write lock instead;
    /// this in-place form is retained for tests and simple single-threaded
    /// callers.
    pub async fn refresh(&mut self, client: &impl BeadsSource) -> anyhow::Result<()> {
        *self = Self::fetch(client).await?;
        Ok(())
    }

    /// Build a fresh cache from the beads daemon **without touching any shared
    /// lock**.
    ///
    /// Fetches account beads, auth configs, and role-account pins and constructs
    /// a fully-populated `AccountCache`. The caller performs the blocking RPCs
    /// here with no lock held, then swaps the result into the shared cache under
    /// a brief write lock — never holding the write guard across the RPCs
    /// (ka-0000000143, same class as the ka-0000004551 snapshot-then-drop fix).
    /// Returns an error if a required RPC fails (caller decides retry strategy).
    pub async fn fetch(client: &impl BeadsSource) -> anyhow::Result<Self> {
        let account_beads = client.list_account_beads().await?;
        let auth_configs = client.list_auth_configs().await?;
        // Role-account pins are best-effort: failure to load them shouldn't
        // collapse the cache refresh — sessions just fall through to the
        // project-default account, which is the pre-ka-0000002136 behavior.
        let role_accounts = match client.list_role_accounts().await {
            Ok(v) => v,
            Err(e) => {
                tracing::warn!(
                    error = %e,
                    "failover: role-account refresh failed, falling back to project-only assignment"
                );
                Vec::new()
            }
        };

        let mut cache = Self::new();
        for bead in account_beads {
            let fallback = match bead.fallback {
                Some(ref fb) if fb == &bead.name => {
                    tracing::error!(
                        account = %bead.name,
                        bead_id = %bead.id,
                        "failover: rejecting self-loop fallback (account points at itself); \
                         clearing fallback in cache. Fix the account bead's fallback field."
                    );
                    None
                }
                other => other,
            };
            cache.accounts.insert(
                bead.name.clone(),
                AccountInfo {
                    name: bead.name,
                    bead_id: bead.id,
                    fallback,
                    provider: bead.provider,
                    status: bead.status,
                    credential_secret: bead.credential_secret,
                    throttle_status: bead.throttle_status,
                    utilization_5h: bead.utilization_5h,
                    utilization_7d: bead.utilization_7d,
                    throttle_reset_at: bead.throttle_reset_at,
                    representative_claim: bead.representative_claim,
                    billing_period_hours: bead.billing_period_hours,
                    labels: bead.labels,
                },
            );
        }

        for cfg in auth_configs {
            cache.auth_configs.insert(
                cfg.project.clone(),
                AuthConfigInfo {
                    bead_id: cfg.bead_id,
                    account: cfg.account,
                    primary_account: cfg.primary_account,
                    project: cfg.project,
                },
            );
        }

        for r in role_accounts {
            cache.role_accounts.insert(r.role, r.account);
        }

        tracing::debug!(
            accounts = cache.accounts.len(),
            auth_configs = cache.auth_configs.len(),
            role_accounts = cache.role_accounts.len(),
            "failover: account cache refreshed"
        );
        Ok(cache)
    }

    /// Return all cached accounts. Used by the pool API to join probe fields.
    pub fn all_accounts(&self) -> &HashMap<String, AccountInfo> {
        &self.accounts
    }

    /// Whether an account is safe to rotate *onto*, judged from the
    /// controller-authored bead fields (`status` + `throttle_status`).
    ///
    /// This mirrors the controller's `currentAccountUsable`
    /// (gardenio/controller/cmd/controller/auth_failover.go) so coopmux and the
    /// controller agree on "is this account usable":
    ///   - `status == "disabled"` → unusable (operator-disabled, e.g. an org
    ///     that turned off Claude Code subscription access).
    ///   - `status == "exhausted"` AND throttle no longer allows requests
    ///     (`throttle_status` empty or `"rejected"`) → unusable.
    ///   - everything else → usable. A non-exhausted account (e.g. a
    ///     freshly-added "open"/"active" account) is usable regardless of
    ///     `throttle_status`, so we never over-filter just because no probe
    ///     has run yet.
    ///
    /// Unknown accounts (not in the cache) are treated as NOT usable: if beads
    /// has no record we have no health signal and must not stage them as a
    /// rotation target. The credential broker's own `has_api_key` check is a
    /// separate, additional gate applied at staging time.
    pub fn account_usable(&self, account: &str) -> bool {
        match self.accounts.get(account) {
            Some(info) => {
                if info.status == "disabled" {
                    return false;
                }
                if info.status == "exhausted" && !throttle_allows(&info.throttle_status) {
                    return false;
                }
                true
            }
            None => false,
        }
    }

    /// The set of cached account names that pass the bead-authoritative
    /// [`account_usable`](Self::account_usable) rubric.
    ///
    /// Used to constrain the generic (project-less) switch-target selectors
    /// — `rebalance_from_account` on a credential-refresh failure and the
    /// operator `credentials_pool_rebalance` — to accounts the controller has
    /// NOT condemned. Without this, those paths call
    /// `broker.assign_account(None)`, which only consults the broker's
    /// in-process `AccountStatus` and will happily hand back an account the
    /// bead marks `status=exhausted` / `throttle_status=rejected` in the window
    /// before this coopmux has locally observed a 429 on it (ka-0000006526).
    pub fn usable_accounts(&self) -> std::collections::HashSet<String> {
        self.accounts.keys().filter(|name| self.account_usable(name)).cloned().collect()
    }

    /// Mutable access to the accounts map. Used by tests and cache setup.
    pub fn accounts_mut(&mut self) -> &mut HashMap<String, AccountInfo> {
        &mut self.accounts
    }

    /// Mutable access to the auth configs map. Used by tests and cache setup.
    pub fn auth_configs_mut(&mut self) -> &mut HashMap<String, AuthConfigInfo> {
        &mut self.auth_configs
    }

    /// Walk the fallback chain starting from `current_account`.
    ///
    /// Returns the ordered list of fallback accounts (not including the
    /// starting account). Detects and breaks cycles.
    ///
    /// Example: if acme-team → gardenio → (none), calling with
    /// "acme-team" returns `["gardenio"]`.
    ///
    /// Land wall: a land's purse account (`lands-<land>`) has no fallback and
    /// is never one. A chain starting at a land account is empty, and a chain
    /// stops before a land account, whatever `fallback` its bead carries.
    pub fn walk_fallback_chain(&self, current_account: &str) -> Vec<String> {
        let mut chain = Vec::new();
        if super::land::land_of_account(current_account).is_some() {
            return chain;
        }
        let mut visited = std::collections::HashSet::new();
        visited.insert(current_account.to_owned());

        let mut cursor = current_account;
        while let Some(fb) = self.accounts.get(cursor).and_then(|info| info.fallback.as_deref()) {
            if super::land::land_of_account(fb).is_some() {
                tracing::warn!(
                    account = current_account,
                    land_account = fb,
                    "failover: fallback chain points at a land account, stopping"
                );
                break;
            }
            // Cycle detection: stop if we've already visited this account.
            if !visited.insert(fb.to_owned()) {
                tracing::warn!(
                    account = current_account,
                    cycle_at = fb,
                    "failover: cycle detected in fallback chain"
                );
                break;
            }

            chain.push(fb.to_owned());
            cursor = fb;
        }

        chain
    }

    /// Find the first healthy fallback for a project.
    ///
    /// Looks up the project's current account from its auth config, walks
    /// the fallback chain, and returns the first candidate that is usable as a
    /// switch target. A candidate must clear BOTH health gates:
    ///
    ///   1. The credential broker's in-process status is `Healthy` (skips
    ///      `Throttled` / `Warming` accounts this coopmux has locally observed
    ///      hitting a rate limit).
    ///   2. The bead-authoritative [`account_usable`](Self::account_usable)
    ///      rubric (`status` + `throttle_status` from the controller's
    ///      rate-limit probe) accepts it.
    ///
    /// Gate 2 is the fix for ka-0000006526: the broker's `AccountStatus` is
    /// per-coopmux-process and starts `Healthy` after every restart, updated
    /// only by 429s THIS process has seen. An account the controller has
    /// already marked `status=exhausted` / `throttle_status=rejected` (out of
    /// credits, org spend cap, 7d window saturated) still reads broker-`Healthy`
    /// in the post-restart window — so a selector keyed only on the broker would
    /// rebalance a live session straight onto a dead account and 429 it on turn
    /// one. Consulting `account_usable` makes this selector agree with the
    /// new-session staging path (transport/http.rs), which already gates on it.
    ///
    /// Returns `None` if the project has no auth config, no fallbacks, or every
    /// fallback fails either gate.
    pub async fn find_fallback_for_project(
        &self,
        project: &str,
        broker: &crate::credential::broker::CredentialBroker,
    ) -> Option<String> {
        self.find_fallback_for_project_family(project, broker, None).await
    }

    /// [`find_fallback_for_project`](Self::find_fallback_for_project) restricted
    /// to candidates whose provider matches `family` (see
    /// [`provider_compatible`](super::provider_compatible)). A Codex project's
    /// chain must not fail over onto a Claude account, nor the reverse.
    pub async fn find_fallback_for_project_family(
        &self,
        project: &str,
        broker: &crate::credential::broker::CredentialBroker,
        family: Option<super::ProviderFamily>,
    ) -> Option<String> {
        let config = self.auth_configs.get(project)?;
        // A land project has no fallback: its sessions stay on its own purse
        // account rather than fail over onto the fleet.
        if super::land::land_of_account(&config.account).is_some()
            || self.accounts.contains_key(&super::land::land_account_name(project))
        {
            tracing::debug!(project, "failover: land project has no fallback");
            return None;
        }
        let chain = self.walk_fallback_chain(&config.account);

        for candidate in &chain {
            if !broker.account_compatible(candidate, family).await {
                tracing::debug!(
                    account = %candidate,
                    project,
                    "failover: skipping fallback candidate of a different provider"
                );
                continue;
            }
            // Bead-authoritative gate first: it is a cheap in-memory check and
            // excludes accounts the controller probe has condemned even when
            // the broker still reads them Healthy (ka-0000006526).
            if !self.account_usable(candidate) {
                tracing::debug!(
                    account = %candidate,
                    project,
                    "failover: skipping fallback candidate marked exhausted/disabled on its account bead"
                );
                continue;
            }
            let status = broker.account_status(candidate).await;
            if status == "healthy" {
                return Some(candidate.clone());
            }
        }

        None
    }

    /// Return which projects currently use the given account.
    ///
    /// Scans auth configs for projects whose active account matches.
    pub fn projects_using_account(&self, account: &str) -> Vec<String> {
        self.auth_configs
            .values()
            .filter(|cfg| cfg.account == account)
            .map(|cfg| cfg.project.clone())
            .collect()
    }

    /// Look up the configured account for a project from config:auth beads.
    pub fn account_for_project(&self, project: &str) -> Option<String> {
        self.auth_configs.get(project).map(|cfg| cfg.account.clone())
    }

    /// Look up the role-pinned account from `type=role` beads.
    ///
    /// `role` may be a single role name (e.g. `fsm-triage`) or a
    /// comma-separated multi-role spec (e.g. `crew,thread`); the first
    /// component with a pinned account wins. Matches the precedence rule
    /// in `gardenio/controller/cmd/controller/podspec.go::resolveRoleAccount`.
    /// Returns `None` if no component has an account pin.
    pub fn account_for_role(&self, role: &str) -> Option<String> {
        if role.is_empty() || self.role_accounts.is_empty() {
            return None;
        }
        for part in role.split(',') {
            let r = part.trim();
            if r.is_empty() {
                continue;
            }
            if let Some(account) = self.role_accounts.get(r) {
                return Some(account.clone());
            }
        }
        None
    }

    /// Mutable access to the role-account map. Used by tests and cache setup.
    pub fn role_accounts_mut(&mut self) -> &mut HashMap<String, String> {
        &mut self.role_accounts
    }

    /// Return the complete set of accounts authorized for a project.
    ///
    /// The authorized set is the project's **account pool**: every cached
    /// account that carries the `project:<project>` label. This mirrors the
    /// controller-side `selectSpreadAccount` candidate set
    /// (`gardenio/controller/cmd/controller/account_spread.go`) so coopmux and
    /// the controller agree on which accounts a project may use, and so the
    /// staging path can spread a session's fallback profiles across the whole
    /// pool rather than a single chain.
    ///
    /// This is deliberately NOT derived from `walk_fallback_chain` nor from the
    /// project's currently-configured `account`:
    ///   - The fallback chain is a linear list; building the authorized set
    ///     from it collapses to one account whenever the configured account has
    ///     no `fallback` pointer, which defeats load spreading and re-creates
    ///     the single-account pileup (ka-0000003255).
    ///   - The configured `account` can be stale or point at a cooked account;
    ///     pinning the authorized set to it would forbid every healthy sibling
    ///     in the pool (item 3 of ka-0000001040).
    ///
    /// Health/usability filtering is applied downstream (the `usable` gate in
    /// the staging path), so this set is the broad candidate pool — exhausted
    /// or disabled members are dropped at staging time, not here.
    ///
    /// Returns an empty set if no cached account is labeled for the project.
    pub fn authorized_accounts_for_project(
        &self,
        project: &str,
    ) -> std::collections::HashSet<String> {
        if project.is_empty() {
            return std::collections::HashSet::new();
        }
        let want_label = format!("project:{project}");
        self.accounts
            .values()
            .filter(|acct| account_labeled_for_project(&acct.labels, &want_label))
            .map(|acct| acct.name.clone())
            .collect()
    }

    /// Update the active account for a project in the in-memory cache.
    ///
    /// Called after a failover or switchback to keep the cache consistent
    /// with the beads daemon state. New sessions will be assigned the
    /// updated account immediately without waiting for a pod restart.
    pub fn update_project_account(
        &mut self,
        project: &str,
        new_account: &str,
        new_primary: Option<&str>,
    ) {
        if let Some(cfg) = self.auth_configs.get_mut(project) {
            cfg.account = new_account.to_owned();
            if let Some(primary) = new_primary {
                cfg.primary_account =
                    if primary.is_empty() { None } else { Some(primary.to_owned()) };
            }
        }
    }
}

impl Default for AccountCache {
    fn default() -> Self {
        Self::new()
    }
}

// ── Project-scoped rebalance ────────────────────────────────────────────

/// Rebalance sessions away from a throttled account using project-specific
/// fallback chains.
///
/// Unlike `rebalance_from_account()` in distributor.rs which picks the
/// globally least-loaded healthy account, this function respects per-project
/// fallback chains configured in beads. Each project may have a different
/// fallback target.
///
/// For each affected project:
/// 1. Determine the correct fallback via the account's fallback chain
/// 2. Get credentials for the fallback account
/// 3. Reassign sessions, push credentials, and trigger profile switch
///
/// Returns a map of `project → new_account` for each successfully rebalanced
/// project. The caller should apply these to the in-memory `AccountCache`
/// (under a write lock) so that new session registrations pick up the change.
pub async fn project_scoped_rebalance(
    state: &MuxState,
    cache: &AccountCache,
    throttled_account: &str,
) -> HashMap<String, (String, Option<String>)> {
    let broker = &state.credential_broker;

    // Find which projects are affected.
    let affected_projects = cache.projects_using_account(throttled_account);
    if affected_projects.is_empty() {
        tracing::debug!(
            account = throttled_account,
            "failover: no projects using throttled account"
        );
        return HashMap::new();
    }

    // Fallbacks must be of the throttled account's provider family.
    let family = broker.account_family(throttled_account).await;

    // Build a per-project fallback map: project → fallback account name.
    let mut project_fallbacks: HashMap<String, String> = HashMap::new();
    for project in &affected_projects {
        match cache.find_fallback_for_project_family(project, broker, family).await {
            Some(fallback) => {
                project_fallbacks.insert(project.clone(), fallback);
            }
            None => {
                tracing::warn!(
                    project,
                    account = throttled_account,
                    "failover: no healthy fallback for project"
                );
            }
        }
    }

    if project_fallbacks.is_empty() {
        tracing::warn!(
            account = throttled_account,
            projects = ?affected_projects,
            "failover: no healthy fallbacks available for any affected project"
        );
        return HashMap::new();
    }

    // Pre-fetch credentials for each unique fallback account.
    let unique_targets: std::collections::HashSet<&str> =
        project_fallbacks.values().map(|s| s.as_str()).collect();
    let mut target_creds: HashMap<String, HashMap<String, String>> = HashMap::new();
    for target in &unique_targets {
        match broker.get_credentials(target).await {
            Some(creds) => {
                target_creds.insert((*target).to_owned(), creds);
            }
            None => {
                tracing::warn!(
                    account = *target,
                    "failover: fallback account has no credentials, skipping"
                );
            }
        }
    }

    // Collect affected sessions: sessions assigned to the throttled account,
    // grouped by which project fallback they should use.
    //
    // Sessions carry metadata with a `project` field that maps them to a
    // project. Sessions without a project field are skipped (they'll be
    // handled by the generic rebalance in distributor.rs).
    let affected_sessions: Vec<(Arc<crate::state::SessionEntry>, String)> = {
        let sessions = state.sessions.read().await;
        let mut result = Vec::new();
        for entry in sessions.values() {
            let current = entry.assigned_account.read().await.clone();
            if current.as_deref() != Some(throttled_account) {
                continue;
            }

            // Determine the session's project from metadata.
            let project = match entry.metadata.get("project").and_then(|v| v.as_str()) {
                Some(p) => p.to_owned(),
                None => continue, // No project tag — skip for project-scoped rebalance.
            };

            // Only include if we have a fallback for this project.
            if let Some(target) = project_fallbacks.get(&project) {
                if target_creds.contains_key(target.as_str()) {
                    result.push((Arc::clone(entry), target.clone()));
                }
            }
        }
        result
    };

    if affected_sessions.is_empty() {
        tracing::debug!(account = throttled_account, "failover: no sessions to rebalance");
        return HashMap::new();
    }

    let mut reassigned = 0u32;
    let mut failed = 0u32;

    for (entry, target_name) in &affected_sessions {
        let creds = match target_creds.get(target_name.as_str()) {
            Some(c) => c,
            None => continue,
        };

        if super::reassign::reassign_session(
            state.http_client.clone(),
            broker,
            entry,
            throttled_account,
            target_name,
            creds,
        )
        .await
        {
            tracing::info!(
                session = %entry.id,
                from = throttled_account,
                to = %target_name,
                "failover: rebalanced session to project fallback"
            );
            reassigned += 1;
        } else {
            tracing::warn!(
                session = %entry.id,
                from = throttled_account,
                to = %target_name,
                "failover: profile switch failed during rebalance"
            );
            failed += 1;
        }
    }

    tracing::info!(
        account = throttled_account,
        reassigned,
        failed,
        projects = ?affected_projects,
        "failover: project-scoped rebalance complete"
    );

    // Record the failover in the in-memory cache only. coopmux no longer
    // writes the config:auth bead (update/config): rewriting which account a
    // project's credentials route to is a privilege coopmux does not hold on
    // its machine principal, and the controller owns that record
    // (auth_failover.go -> updateAuthConfigForProject; decision gd-HYilfX1Bk8).
    // The session rebalance above already happened; the account bead below is
    // the persisted signal the controller acts on. The periodic cache refresh
    // re-reads config:auth from beads, and `account_usable` keeps the exhausted
    // account out of fallback selection until the controller clears it.
    let mut cache_updates: HashMap<String, (String, Option<String>)> = HashMap::new();
    if reassigned > 0 {
        for (project, target) in &project_fallbacks {
            if cache.auth_configs.contains_key(project) {
                // primary is None so the in-memory auth config mirrors the
                // "no switchback pin" state (switchback is abolished,
                // ka-0000008240).
                cache_updates.insert(project.clone(), (target.clone(), None));
            }
        }

        let beads_client_guard = state.beads_client.read().await;
        if let Some(ref beads_client) = *beads_client_guard {
            // Mark the throttled account bead as exhausted WITH cooldown
            // metadata. The Go switchback/recovery sweep
            // (auth_failover.go::inActiveCooldown / retryStuckFailovers) only
            // recognizes a Rust-marked account as recoverable when it carries
            // status=exhausted PLUS a parseable cooldown_until; without these
            // the account would be stuck exhausted forever. cooldown_until is
            // now + the per-account billing window (billing_period_hours,
            // default 5h for a Claude Max account). (ka-0000001040 item 4.)
            //
            // This is an update/account write: admin-gated in kbeads and NOT
            // in machineAllowedWrites, so the static token is denied. It goes
            // through coopmux's own machine principal when one is configured
            // (gd-Wcu0DOkHfN; state.machine_beads_client, BEADS_OIDC_* env),
            // whose gardenio-machine-accounts role grants exactly this write.
            if let Some(acct) = cache.accounts.get(throttled_account) {
                let now = iso8601_now();
                let cooldown_until = cooldown_until_from(&acct.billing_period_hours);
                let mut fields = std::collections::HashMap::new();
                fields.insert("status".to_owned(), "exhausted".to_owned());
                fields.insert("exhausted_at".to_owned(), now);
                fields.insert("cooldown_until".to_owned(), cooldown_until);
                let account_writer: &crate::beads::BeadsClient =
                    state.machine_beads_client.as_deref().unwrap_or(beads_client);
                let principal = if account_writer.is_machine() { "machine" } else { "static" };
                if let Err(e) = account_writer.update_bead_fields(&acct.bead_id, &fields).await {
                    tracing::warn!(
                        account = throttled_account,
                        principal,
                        err = %e,
                        "failover: failed to mark account bead as exhausted"
                    );
                } else {
                    tracing::info!(
                        account = throttled_account,
                        principal,
                        "failover: marked account bead exhausted"
                    );
                }
            }
        }
    }

    cache_updates
}

/// Default cooldown window (seconds) when an account has no
/// `billing_period_hours`. 5 hours matches the Claude Max rolling window — the
/// same default horizon the controller applies for an unconfigured account.
const DEFAULT_COOLDOWN_SECS: u64 = 5 * 3600;

/// Compute the `cooldown_until` timestamp for a just-exhausted account: now
/// plus the account's billing window (`billing_period_hours`, parsed as whole
/// hours), falling back to [`DEFAULT_COOLDOWN_SECS`] when the field is empty or
/// unparseable. Returns an ISO-8601 UTC timestamp string.
fn cooldown_until_from(billing_period_hours: &str) -> String {
    let secs = billing_period_hours
        .trim()
        .parse::<u64>()
        .ok()
        .filter(|h| *h > 0)
        .map(|h| h * 3600)
        .unwrap_or(DEFAULT_COOLDOWN_SECS);
    iso8601_at(now_unix().saturating_add(secs))
}

/// Current Unix time in seconds.
fn now_unix() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default().as_secs()
}

/// Format the current time as an ISO-8601 timestamp (UTC).
///
/// Uses `std::time::SystemTime` to avoid adding a `chrono` dependency.
fn iso8601_now() -> String {
    iso8601_at(now_unix())
}

/// Format an absolute Unix timestamp (seconds) as an ISO-8601 UTC string.
fn iso8601_at(secs: u64) -> String {
    // Convert epoch seconds to a basic ISO-8601 date-time.
    let days = secs / 86400;
    let remaining = secs % 86400;
    let hours = remaining / 3600;
    let minutes = (remaining % 3600) / 60;
    let seconds = remaining % 60;

    // Days since 1970-01-01 to Y-M-D.
    let (year, month, day) = days_to_ymd(days);
    format!("{year:04}-{month:02}-{day:02}T{hours:02}:{minutes:02}:{seconds:02}Z")
}

/// Convert days since Unix epoch to (year, month, day).
fn days_to_ymd(mut days: u64) -> (u64, u64, u64) {
    // Civil calendar algorithm adapted from Howard Hinnant.
    days += 719_468;
    let era = days / 146_097;
    let doe = days - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    (y, m, d)
}

/// Whether an Anthropic rate-limit probe `throttle_status` still allows
/// requests. Mirrors the controller's `throttleAllows`
/// (gardenio/controller/cmd/controller/auth_failover.go): the API returns
/// "allowed", "allowed_warning", or "rejected" — anything other than
/// "rejected" (or empty/unprobed) is usable.
fn throttle_allows(status: &str) -> bool {
    !status.is_empty() && status != "rejected"
}

/// Whether an account's raw bead labels contain the given `project:<name>`
/// label. Mirrors `accountLabeledForProject` on the controller side
/// (`gardenio/controller/cmd/controller/account_spread.go`).
fn account_labeled_for_project(labels: &[String], want_label: &str) -> bool {
    labels.iter().any(|l| l == want_label)
}

// ── Tests ───────────────────────────────────────────────────────────────

#[cfg(test)]
#[path = "failover_tests.rs"]
mod tests;
