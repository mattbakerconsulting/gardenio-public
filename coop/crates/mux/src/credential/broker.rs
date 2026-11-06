// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Credential broker: API key store with pool load balancing.
//!
//! Starts empty and is populated dynamically via `add_account()` and
//! `set_token()` from the controller's credentials/set API. The deprecated
//! `--credential-config` file can still pre-seed accounts at startup.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, AtomicUsize, Ordering};
use std::sync::Arc;

use tokio::sync::{broadcast, Mutex, RwLock, RwLockWriteGuard};
use zeroize::Zeroize;

use crate::credential::persist::{PersistedAccount, PersistedCredentials};
use crate::credential::{
    codex_auth, is_codex_provider, provider_compatible, provider_default_env_key, provider_family,
    AccountConfig, AccountStatus, CredentialConfig, CredentialEvent, ProviderFamily,
    CODEX_MODEL_ENV_KEY,
};

/// Set of account names that were defined in the original static config file
/// (used to distinguish dynamic accounts for persistence).
type StaticNames = std::collections::HashSet<String>;

/// How the broker picks among eligible accounts for new sessions.
///
/// Read once from `COOPMUX_ASSIGNMENT_STRATEGY` at broker construction.
/// See `coop/docs/account-barrel-rotator.md` for the rollout plan.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AssignmentStrategy {
    /// Pick the account with the lowest live session count (legacy).
    LeastLoaded,
    /// Round-robin across eligible accounts via a shared cursor.
    RoundRobin,
}

impl AssignmentStrategy {
    /// Parse an env-var value; unrecognised input falls back to `LeastLoaded`
    /// with a warning so a typo can't silently change behavior.
    fn from_env_value(raw: &str) -> Self {
        match raw.trim().to_ascii_lowercase().as_str() {
            "round_robin" | "roundrobin" => Self::RoundRobin,
            "least_loaded" | "leastloaded" | "" => Self::LeastLoaded,
            other => {
                tracing::warn!(
                    value = other,
                    "unknown COOPMUX_ASSIGNMENT_STRATEGY; falling back to least_loaded"
                );
                Self::LeastLoaded
            }
        }
    }

    fn from_env() -> Self {
        match std::env::var("COOPMUX_ASSIGNMENT_STRATEGY") {
            Ok(val) => Self::from_env_value(&val),
            Err(_) => Self::LeastLoaded,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Self::LeastLoaded => "least_loaded",
            Self::RoundRobin => "round_robin",
        }
    }

    fn to_u8(self) -> u8 {
        match self {
            Self::LeastLoaded => 0,
            Self::RoundRobin => 1,
        }
    }

    fn from_u8(v: u8) -> Self {
        match v {
            1 => Self::RoundRobin,
            _ => Self::LeastLoaded,
        }
    }
}

/// Runtime state for a single account.
struct AccountState {
    config: AccountConfig,
    status: AccountStatus,
    /// The API key or token for this account.
    api_key: Option<String>,
    /// LiteLLM virtual key for per-account budget tracking. Distributed
    /// to sessions as `ANTHROPIC_CUSTOM_HEADERS` during credential rotation.
    litellm_key: Option<String>,
    /// API endpoint for this account (controller-resolved: proxy_url >
    /// base_url > LiteLLM default). Distributed to sessions as
    /// `ANTHROPIC_BASE_URL` so a rotation onto an account fronted by a
    /// different proxy (e.g. Bedrock via a different LiteLLM) carries the
    /// endpoint alongside the token/model — without it the session keeps
    /// its spawn-time base URL and 401s (ka-0000002623).
    base_url: Option<String>,
    /// Set true by `set_litellm_key` / `set_base_url` when they actually
    /// change the resolved profile (endpoint/header) an already-issued token
    /// authenticates with, and cleared by the next `set_token`. Without it,
    /// `set_token` would gate the `Refreshed` redistribution on `token_changed`
    /// alone and silently drop a base_url/litellm_key change that arrives with
    /// an unchanged token — stranding live sessions on the stale endpoint/header
    /// (401/403), the same-token reopening of ka-0000002623.
    profile_dirty: bool,
    /// Epoch seconds at which the current credential stops working, for
    /// rotating credentials (Codex access token). `None` for static keys.
    expires_at: Option<u64>,
}

impl AccountState {
    fn new(config: AccountConfig, status: AccountStatus, api_key: Option<String>) -> Self {
        Self {
            config,
            status,
            api_key,
            litellm_key: None,
            base_url: None,
            profile_dirty: false,
            expires_at: None,
        }
    }

    /// The credential map distributed to sessions for `token`, per provider.
    ///
    /// Claude/Gemini accounts emit the token under the provider env key plus the
    /// Anthropic model / LiteLLM / base-URL routing. A Codex account emits only
    /// the auth document and `CODEX_MODEL`: subscription auth has no LiteLLM or
    /// Anthropic routing (ADR 0004 §5), and `ANTHROPIC_*` / `LITELLM_*` env must
    /// never reach a Codex pod.
    fn credential_map(&self, token: &str) -> HashMap<String, String> {
        let env_key = self
            .config
            .env_key
            .as_deref()
            .unwrap_or_else(|| provider_default_env_key(&self.config.provider));
        let mut creds = HashMap::from([(env_key.to_owned(), token.to_owned())]);
        if is_codex_provider(&self.config.provider) {
            if let Some(ref model) = self.config.model {
                creds.insert(CODEX_MODEL_ENV_KEY.to_owned(), model.clone());
            }
            return creds;
        }
        // The Anthropic model selectors only mean something to a Claude session;
        // a Gemini (or other) account must not push them.
        if provider_family(&self.config.provider).is_none_or(|f| f == ProviderFamily::Claude) {
            if let Some(ref model) = self.config.model {
                creds.insert("CLAUDE_MODEL".to_owned(), model.clone());
                creds.insert("ANTHROPIC_MODEL".to_owned(), model.clone());
            }
        }
        if let Some(ref key) = self.litellm_key {
            creds.insert(
                "ANTHROPIC_CUSTOM_HEADERS".to_owned(),
                format!("x-litellm-api-key: Bearer {key}"),
            );
        }
        if let Some(ref url) = self.base_url {
            creds.insert("ANTHROPIC_BASE_URL".to_owned(), url.clone());
        }
        creds
    }
}

/// Reduce a pushed token to what the broker stores and distributes.
///
/// Static providers keep the token verbatim. A Codex account's token is its
/// `auth.json` document: it is validated and its refresh token replaced with the
/// placeholder, and the credential's expiry is derived from `expires_in` (when
/// the caller supplied one) or the access token's JWT `exp`. The separately
/// supplied `refresh_token` is dropped on the floor: the controller keeper is
/// the single writer of the real refresh token, and coopmux persistence is
/// tmpfs, so holding a single-use token here could only lose or leak it.
fn prepare_token(
    provider: &str,
    token: String,
    refresh_token: Option<String>,
    expires_in: Option<u64>,
) -> anyhow::Result<(String, Option<u64>)> {
    if !is_codex_provider(provider) {
        return Ok((token, None));
    }
    if let Some(mut rt) = refresh_token {
        zeroize::Zeroize::zeroize(&mut rt);
    }
    let auth = codex_auth::normalize(&token).map_err(|e| anyhow::anyhow!(e))?;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    let expires_at = expires_in.map(|s| now.saturating_add(s)).or(auth.access_expires_at);
    Ok((auth.document, expires_at))
}

impl Drop for AccountState {
    fn drop(&mut self) {
        if let Some(ref mut key) = self.api_key {
            key.zeroize();
        }
        if let Some(ref mut key) = self.litellm_key {
            key.zeroize();
        }
    }
}

/// The credential broker manages API keys for configured accounts.
pub struct CredentialBroker {
    accounts: RwLock<HashMap<String, AccountState>>,
    static_names: StaticNames,
    event_tx: broadcast::Sender<CredentialEvent>,
    persist_dir: Option<PathBuf>,
    /// Per-account session counts for pool load balancing.
    session_counts: RwLock<HashMap<String, AtomicU32>>,
    /// Per-account lifetime assignment counters. Monotonically increasing
    /// (never decremented on unassign) so operators can see fairness over
    /// the lifetime of the process, not just point-in-time load.
    cumulative_assignments: RwLock<HashMap<String, AtomicU64>>,
    /// Selection strategy for `assign_account[_for_project]`.
    ///
    /// Resolved once at construction from `COOPMUX_ASSIGNMENT_STRATEGY`; the
    /// interior `AtomicUsize` wrapper only exists so tests can flip it
    /// deterministically without racing other tests on a shared env var.
    strategy: std::sync::atomic::AtomicU8,
    /// Shared round-robin cursor; only consulted when `strategy = RoundRobin`.
    next_cursor: AtomicUsize,
    /// Whether we have already emitted a warning about failing to chmod the
    /// credential state dir. Some mount setups (e.g. K8s projected volumes)
    /// return EPERM for every chmod attempt; we still want to flag it once,
    /// but persist() runs on every credential sync — the repeated WARN drowns
    /// out real signals.
    ///
    /// `Arc` so the flag survives being moved into the `spawn_blocking` closure
    /// that performs the off-lock disk write (ka-0000007732).
    chmod_warned: Arc<AtomicBool>,
    /// Serializes credential persistence so that the disk write happens
    /// entirely OUTSIDE the accounts write lock (ka-0000007732). A mutation
    /// snapshots the accounts under the accounts lock, drops that lock, then
    /// acquires this persist lock and writes on a blocking thread. Holding a
    /// dedicated lock (never the accounts lock) across the fsync means hot-path
    /// credential ops — assign_account, mark_throttled, status_of — never
    /// serialize behind disk I/O.
    ///
    /// The `u64` inside is a monotonically increasing sequence: each mutation
    /// stamps its snapshot with the next value under the accounts lock, and a
    /// writer skips its (already-superseded) write if a newer snapshot has
    /// already reached disk. This keeps last-writer-wins semantics without
    /// re-taking the accounts lock.
    persist_state: Arc<PersistState>,
    /// Test-only: milliseconds the blocking disk write sleeps for, simulating a
    /// slow persist_dir (throttled node disk / fsync stall). Lets tests prove
    /// deterministically that hot-path account ops do NOT serialize behind the
    /// disk write (ka-0000007732). Zero in production paths.
    #[cfg(test)]
    persist_delay_ms: Arc<AtomicU64>,
}

/// Off-lock persistence coordination (ka-0000007732). See
/// `CredentialBroker::persist_state`.
struct PersistState {
    /// Sequence assigned to the most recent snapshot, bumped under the
    /// accounts lock so ordering matches mutation order.
    next_seq: AtomicU64,
    /// Guards the actual disk write and records the highest sequence already
    /// written, so a superseded snapshot is dropped instead of clobbering a
    /// newer one.
    writer: Mutex<u64>,
}

impl CredentialBroker {
    /// Create a new broker from config (static API key mode).
    ///
    /// `persist_dir` controls where dynamic accounts and API keys are saved
    /// to disk. Pass `None` to disable persistence entirely.
    pub fn new(
        config: CredentialConfig,
        event_tx: broadcast::Sender<CredentialEvent>,
        persist_dir: Option<PathBuf>,
    ) -> Arc<Self> {
        let mut accounts = HashMap::new();
        let mut static_names = StaticNames::new();
        for acct in &config.accounts {
            static_names.insert(acct.name.clone());
            accounts.insert(
                acct.name.clone(),
                AccountState::new(acct.clone(), AccountStatus::Missing, None),
            );
        }
        let session_counts: HashMap<String, AtomicU32> =
            accounts.keys().map(|name| (name.clone(), AtomicU32::new(0))).collect();
        let cumulative_assignments: HashMap<String, AtomicU64> =
            accounts.keys().map(|name| (name.clone(), AtomicU64::new(0))).collect();

        Arc::new(Self {
            accounts: RwLock::new(accounts),
            static_names,
            event_tx,
            persist_dir,
            session_counts: RwLock::new(session_counts),
            cumulative_assignments: RwLock::new(cumulative_assignments),
            strategy: std::sync::atomic::AtomicU8::new(AssignmentStrategy::from_env().to_u8()),
            next_cursor: AtomicUsize::new(0),
            chmod_warned: Arc::new(AtomicBool::new(false)),
            persist_state: Arc::new(PersistState {
                next_seq: AtomicU64::new(0),
                writer: Mutex::new(0),
            }),
            #[cfg(test)]
            persist_delay_ms: Arc::new(AtomicU64::new(0)),
        })
    }

    /// The active assignment strategy; exposed for `/pool` and tests.
    pub fn strategy(&self) -> AssignmentStrategy {
        AssignmentStrategy::from_u8(self.strategy.load(Ordering::Relaxed))
    }

    /// Returns the current cursor value when round-robin is active.
    pub fn rotator_cursor(&self) -> Option<usize> {
        match self.strategy() {
            AssignmentStrategy::RoundRobin => Some(self.next_cursor.load(Ordering::Relaxed)),
            AssignmentStrategy::LeastLoaded => None,
        }
    }

    /// Test-only: override the strategy without going through the env var,
    /// so individual tests can exercise the rotator without racing each
    /// other on a shared process-global env var.
    #[cfg(test)]
    fn set_strategy_for_tests(&self, strategy: AssignmentStrategy) {
        self.strategy.store(strategy.to_u8(), Ordering::Relaxed);
    }

    /// Test-only: make the blocking disk write sleep `ms` milliseconds,
    /// simulating a slow persist_dir so tests can prove hot-path account ops
    /// do not serialize behind the write (ka-0000007732).
    #[cfg(test)]
    fn set_persist_delay_for_tests(&self, ms: u64) {
        self.persist_delay_ms.store(ms, Ordering::Relaxed);
    }

    /// Test-only: block until every in-flight persist has drained to disk, so a
    /// test that asserts on file contents doesn't race the background write.
    #[cfg(test)]
    async fn wait_for_persist_idle(&self) {
        // Acquiring the writer lock guarantees no write is in progress; the
        // sequence check inside persist_and_release means the latest snapshot
        // has already been committed once the highest seq is recorded.
        let target = self.persist_state.next_seq.load(Ordering::SeqCst);
        loop {
            let written = *self.persist_state.writer.lock().await;
            if written >= target {
                return;
            }
            tokio::task::yield_now().await;
        }
    }

    /// Get the first account name.
    pub async fn first_account_name(&self) -> Option<String> {
        self.accounts.read().await.keys().next().cloned()
    }

    /// Return the last 4 characters of the API key for an account, or `None`
    /// if the account has no key.
    pub async fn key_suffix(&self, account: &str) -> Option<String> {
        let accounts = self.accounts.read().await;
        let state = accounts.get(account)?;
        let key = state.api_key.as_ref()?;
        // Take the last 4 *characters* (not bytes): slicing by a byte offset
        // panics ("byte index N is not a char boundary") when the token ends
        // in a multi-byte UTF-8 sequence, and the token is operator-controlled
        // verbatim via POST /api/v1/credentials/set (ka-0000001776).
        let mut chars: Vec<char> = key.chars().rev().take(4).collect();
        chars.reverse();
        Some(chars.into_iter().collect())
    }

    /// Get the current credentials map for an account (env_key -> token).
    ///
    /// Includes CLAUDE_MODEL / ANTHROPIC_MODEL when the account has a model
    /// configured, so that credential rotation switches the model alongside
    /// the API key.
    ///
    /// Includes ANTHROPIC_CUSTOM_HEADERS when the account has a litellm_key,
    /// so that credential rotation updates LiteLLM cost attribution alongside
    /// the API key — no pod restart needed.
    ///
    /// Includes ANTHROPIC_BASE_URL when the account has a base_url, so that a
    /// rotation onto an account served by a different endpoint (e.g. a Bedrock
    /// account behind a different LiteLLM proxy) switches the base URL along
    /// with the token and model. Without it a global/no-project session that
    /// rotates cross-endpoint keeps its spawn-time base URL and gets 401/403
    /// (ka-0000002623).
    pub async fn get_credentials(&self, account: &str) -> Option<HashMap<String, String>> {
        let accounts = self.accounts.read().await;
        let state = accounts.get(account)?;
        let token = state.api_key.as_ref()?;
        let creds = state.credential_map(token);
        Some(creds)
    }

    /// Store a LiteLLM virtual key for an account. Called before `set_token()`
    /// so the subsequent `Refreshed` event includes the key in credentials.
    pub async fn set_litellm_key(&self, account: &str, key: String) {
        let mut accounts = self.accounts.write().await;
        if let Some(state) = accounts.get_mut(account) {
            // Mark the profile dirty when the key actually changes so a
            // subsequent same-token set_token still redistributes the new
            // ANTHROPIC_CUSTOM_HEADERS to live sessions (ka-0000002623,
            // same-token path).
            if state.litellm_key.as_deref() != Some(key.as_str()) {
                state.profile_dirty = true;
            }
            state.litellm_key = Some(key);
        }
    }

    /// Store the API base URL for an account. Called before `set_token()`
    /// so the subsequent `Refreshed` event includes `ANTHROPIC_BASE_URL` in
    /// credentials, and so staged fallback profiles carry the endpoint a
    /// rotation target actually authenticates against (ka-0000002623).
    pub async fn set_base_url(&self, account: &str, url: String) {
        let mut accounts = self.accounts.write().await;
        if let Some(state) = accounts.get_mut(account) {
            // Mark the profile dirty when the endpoint actually changes so a
            // subsequent same-token set_token still redistributes the new
            // ANTHROPIC_BASE_URL to live sessions (ka-0000002623, same-token
            // path).
            if state.base_url.as_deref() != Some(url.as_str()) {
                state.profile_dirty = true;
            }
            state.base_url = Some(url);
        }
    }

    // ── Pool load balancing ─────────────────────────────────────────────

    /// Pick a healthy account for a new session.
    ///
    /// Warming accounts are skipped — they are approaching the rate limit and
    /// should not receive new sessions. Existing sessions on warming accounts
    /// continue undisturbed.
    ///
    /// Selection uses `self.strategy`: least-loaded by default, or round-robin
    /// via the shared `next_cursor` when `COOPMUX_ASSIGNMENT_STRATEGY=round_robin`.
    /// `preferred`, if Healthy, short-circuits either strategy to preserve
    /// project→account binding.
    pub async fn assign_account(&self, preferred: Option<&str>) -> Option<String> {
        self.assign_account_for_family(preferred, None).await
    }

    /// [`assign_account`](Self::assign_account) for a session of a known
    /// provider family. Only accounts whose provider is compatible with
    /// `family` are eligible (see [`provider_compatible`]); with `None` the
    /// session is treated as non-Codex, so a Codex account is never handed to a
    /// session that did not ask for one.
    pub async fn assign_account_for_family(
        &self,
        preferred: Option<&str>,
        family: Option<ProviderFamily>,
    ) -> Option<String> {
        let accounts = self.accounts.read().await;
        let counts = self.session_counts.read().await;

        // Check preferred account first (only if healthy, not warming, and of
        // the session's provider family).
        if let Some(pref) = preferred {
            if let Some(state) = accounts.get(pref) {
                if state.status == AccountStatus::Healthy
                    && provider_compatible(family, &state.config.provider)
                {
                    return Some(pref.to_owned());
                }
            }
        }

        match self.strategy() {
            AssignmentStrategy::LeastLoaded => {
                self.pick_least_loaded(&accounts, &counts, None, family)
            }
            AssignmentStrategy::RoundRobin => self.pick_round_robin(&accounts, None, family),
        }
    }

    /// Pick the least-loaded healthy account for a new session, restricted
    /// to accounts authorized for a specific project.
    ///
    /// Unlike `assign_account()` which falls back to the global pool,
    /// this method only considers accounts in the `authorized` set (derived
    /// from the project's auth config and its fallback chain). Returns
    /// `None` if no authorized account is healthy — the session will remain
    /// unassigned until an authorized account recovers.
    pub async fn assign_account_for_project(
        &self,
        preferred: Option<&str>,
        authorized: &std::collections::HashSet<String>,
    ) -> Option<String> {
        self.assign_account_for_project_family(preferred, authorized, None).await
    }

    /// [`assign_account_for_project`](Self::assign_account_for_project) for a
    /// session of a known provider family: the `authorized` set is further
    /// restricted to accounts compatible with `family`.
    pub async fn assign_account_for_project_family(
        &self,
        preferred: Option<&str>,
        authorized: &std::collections::HashSet<String>,
        family: Option<ProviderFamily>,
    ) -> Option<String> {
        let accounts = self.accounts.read().await;
        let counts = self.session_counts.read().await;

        // Check preferred account first (only if healthy, authorized, and of
        // the session's provider family).
        if let Some(pref) = preferred {
            if authorized.contains(pref) {
                if let Some(state) = accounts.get(pref) {
                    if state.status == AccountStatus::Healthy
                        && provider_compatible(family, &state.config.provider)
                    {
                        return Some(pref.to_owned());
                    }
                }
            }
        }

        match self.strategy() {
            AssignmentStrategy::LeastLoaded => {
                self.pick_least_loaded(&accounts, &counts, Some(authorized), family)
            }
            AssignmentStrategy::RoundRobin => {
                self.pick_round_robin(&accounts, Some(authorized), family)
            }
        }
    }

    /// Provider string of a configured account.
    pub async fn account_provider(&self, account: &str) -> Option<String> {
        self.accounts.read().await.get(account).map(|s| s.config.provider.clone())
    }

    /// Provider of `account` as seen by a credential distribution: the
    /// configured provider, or — for an account the broker no longer knows —
    /// inferred from `credentials`, since only a Codex account carries the
    /// `CODEX_AUTH_JSON` document. Empty when unknown (treated as non-Codex).
    pub async fn provider_for_credentials(
        &self,
        account: &str,
        credentials: &HashMap<String, String>,
    ) -> String {
        if let Some(provider) = self.account_provider(account).await {
            return provider;
        }
        if credentials.contains_key(super::CODEX_AUTH_ENV_KEY) {
            "codex".to_owned()
        } else {
            String::new()
        }
    }

    /// Provider family of a configured account, if the account exists and its
    /// provider is recognized.
    pub async fn account_family(&self, account: &str) -> Option<ProviderFamily> {
        let accounts = self.accounts.read().await;
        provider_family(&accounts.get(account)?.config.provider)
    }

    /// Whether `account` may be held by a session of `family`. An unknown
    /// account is incompatible.
    pub async fn account_compatible(&self, account: &str, family: Option<ProviderFamily>) -> bool {
        let accounts = self.accounts.read().await;
        accounts.get(account).is_some_and(|s| provider_compatible(family, &s.config.provider))
    }

    /// Legacy selector: lowest live session count among eligible accounts.
    /// Optional `authorized` restricts the eligible set to that membership.
    fn pick_least_loaded(
        &self,
        accounts: &HashMap<String, AccountState>,
        counts: &HashMap<String, AtomicU32>,
        authorized: Option<&std::collections::HashSet<String>>,
        family: Option<ProviderFamily>,
    ) -> Option<String> {
        let mut best: Option<(String, u32)> = None;
        for (name, state) in accounts.iter() {
            if let Some(set) = authorized {
                if !set.contains(name) {
                    continue;
                }
            }
            if !provider_compatible(family, &state.config.provider) {
                continue;
            }
            if state.status != AccountStatus::Healthy {
                continue;
            }
            let count = counts.get(name).map(|c| c.load(Ordering::Relaxed)).unwrap_or(0);
            match &best {
                None => best = Some((name.clone(), count)),
                Some((_, best_count)) if count < *best_count => {
                    best = Some((name.clone(), count));
                }
                _ => {}
            }
        }
        best.map(|(name, _)| name)
    }

    /// Round-robin selector: index the lexicographically-sorted eligible list
    /// by `next_cursor`, then `fetch_add(1)` so subsequent calls advance.
    ///
    /// On churn the cursor is taken mod the new length (per design doc §2),
    /// producing a short one-cycle unfairness window that's accepted in
    /// exchange for not needing a persistent rank field.
    fn pick_round_robin(
        &self,
        accounts: &HashMap<String, AccountState>,
        authorized: Option<&std::collections::HashSet<String>>,
        family: Option<ProviderFamily>,
    ) -> Option<String> {
        let mut eligible: Vec<&String> = accounts
            .iter()
            .filter(|(name, state)| {
                state.status == AccountStatus::Healthy
                    && authorized.is_none_or(|set| set.contains(*name))
                    && provider_compatible(family, &state.config.provider)
            })
            .map(|(name, _)| name)
            .collect();
        if eligible.is_empty() {
            return None;
        }
        eligible.sort();
        let idx = self.next_cursor.fetch_add(1, Ordering::Relaxed) % eligible.len();
        Some(eligible[idx].clone())
    }

    /// Record that a session has been assigned to an account.
    ///
    /// Bumps both the live `session_counts` (decremented on unassign) and the
    /// `cumulative_assignments` counter (monotonic, used by `/pool` to expose
    /// lifetime fairness).
    pub async fn session_assigned(&self, account: &str) {
        let counts = self.session_counts.read().await;
        if let Some(counter) = counts.get(account) {
            counter.fetch_add(1, Ordering::Relaxed);
        } else {
            drop(counts);
            let mut counts = self.session_counts.write().await;
            counts
                .entry(account.to_owned())
                .or_insert_with(|| AtomicU32::new(0))
                .fetch_add(1, Ordering::Relaxed);
        }

        let cumulative = self.cumulative_assignments.read().await;
        if let Some(counter) = cumulative.get(account) {
            counter.fetch_add(1, Ordering::Relaxed);
        } else {
            drop(cumulative);
            let mut cumulative = self.cumulative_assignments.write().await;
            cumulative
                .entry(account.to_owned())
                .or_insert_with(|| AtomicU64::new(0))
                .fetch_add(1, Ordering::Relaxed);
        }

        tracing::debug!(account, "pool: session assigned");
    }

    /// Record that a session has been unassigned from an account.
    pub async fn session_unassigned(&self, account: &str) {
        let counts = self.session_counts.read().await;
        if let Some(counter) = counts.get(account) {
            let _ = counter.fetch_update(Ordering::Relaxed, Ordering::Relaxed, |v| {
                if v > 0 {
                    Some(v - 1)
                } else {
                    None
                }
            });
        }
        tracing::debug!(account, "pool: session unassigned");
    }

    /// List accounts that are unhealthy.
    pub async fn unhealthy_accounts(&self) -> Vec<String> {
        let accounts = self.accounts.read().await;
        accounts
            .iter()
            .filter(|(_, state)| state.status != AccountStatus::Healthy)
            .map(|(name, _)| name.clone())
            .collect()
    }

    /// Get all healthy account names.
    pub async fn healthy_accounts(&self) -> Vec<String> {
        let accounts = self.accounts.read().await;
        accounts
            .iter()
            .filter(|(_, state)| state.status == AccountStatus::Healthy)
            .map(|(name, _)| name.clone())
            .collect()
    }

    /// Mark an account as warming (approaching rate limit, soft steering).
    ///
    /// Only transitions from `Healthy` → `Warming`. Other states are left alone.
    /// Returns `true` if the account was actually marked as warming.
    pub async fn mark_warming(&self, account: &str) -> bool {
        let mut accounts = self.accounts.write().await;
        if let Some(state) = accounts.get_mut(account) {
            if state.status == AccountStatus::Healthy {
                state.status = AccountStatus::Warming;
                tracing::warn!(account, "broker: account marked as warming");
                return true;
            }
        }
        false
    }

    /// Clear warming status, restoring the account to Healthy.
    ///
    /// Only transitions from `Warming` → `Healthy`. Other states are left alone.
    /// Returns `true` if the account was actually warming and is now healthy.
    pub async fn clear_warming(&self, account: &str) -> bool {
        let mut accounts = self.accounts.write().await;
        if let Some(state) = accounts.get_mut(account) {
            if state.status == AccountStatus::Warming {
                state.status = AccountStatus::Healthy;
                tracing::info!(account, "broker: account warming cleared");
                return true;
            }
        }
        false
    }

    /// Mark an account as throttled (rate-limited or budget-exhausted).
    ///
    /// Transitions from `Healthy` or `Warming` → `Throttled`. Other states
    /// (Missing) are left alone since they indicate a different problem.
    pub async fn mark_throttled(&self, account: &str) {
        let mut accounts = self.accounts.write().await;
        if let Some(state) = accounts.get_mut(account) {
            if state.status == AccountStatus::Healthy || state.status == AccountStatus::Warming {
                state.status = AccountStatus::Throttled;
                tracing::warn!(account, "broker: account marked as throttled");
            }
        }
    }

    /// Clear throttled status, restoring the account to Healthy.
    ///
    /// Transitions from `Throttled` → `Healthy` only. `Warming` is deliberately
    /// NOT cleared here: it is controller-owned soft-steer state, set by the
    /// ratelimit probe's `account_warming` NATS event (`mark_warming`) and
    /// cleared only by its `account_cooled` counterpart (`clear_warming`). The
    /// local recovery path (a live session's working-state transition) drives
    /// this method, and must not clobber the controller's utilization-based
    /// assessment — otherwise a routine idle→working transition on any surviving
    /// session of a warming account resets it to Healthy (and publishes a
    /// contradicting `account_healthy` event), re-admitting new sessions onto a
    /// near-capacity account and defeating soft failover (ka-0000003184). Other
    /// states (`Missing`, `Warming`) are left alone. Returns `true` if the
    /// account was actually throttled and is now healthy.
    pub async fn clear_throttled(&self, account: &str) -> bool {
        let mut accounts = self.accounts.write().await;
        if let Some(state) = accounts.get_mut(account) {
            if state.status == AccountStatus::Throttled {
                let prev = state.status;
                state.status = AccountStatus::Healthy;
                tracing::info!(account, ?prev, "broker: account restored to healthy");
                return true;
            }
        }
        false
    }

    /// Get the status of an account as a string (serde rename_all = snake_case).
    pub async fn account_status(&self, account: &str) -> String {
        let accounts = self.accounts.read().await;
        match accounts.get(account) {
            Some(state) => match state.status {
                AccountStatus::Healthy => "healthy",
                AccountStatus::Missing => "missing",
                AccountStatus::Warming => "warming",
                AccountStatus::Throttled => "throttled",
            }
            .to_owned(),
            None => "unknown".to_owned(),
        }
    }

    /// Set the API key for an account. Emits a `Refreshed` event so the
    /// distributor pushes the credential to sessions when the token OR the
    /// resolved profile (base_url / litellm_key, staged by set_base_url /
    /// set_litellm_key just before this call) changed. Gating on the token
    /// alone would silently drop an endpoint/header change that arrives with an
    /// unchanged token, stranding live sessions on the stale profile (401/403,
    /// the same-token reopening of ka-0000002623). A repeated identical sync
    /// (same token AND same profile) still emits nothing, preserving the
    /// anti-churn behaviour for the controller's periodic 5-min resync.
    ///
    /// Static providers ignore `refresh_token` / `expires_in` (static keys don't
    /// expire). A Codex account is a rotating credential: `access_token` is its
    /// `auth.json` document, validated and stripped of any real refresh token
    /// (see [`prepare_token`]); each rotation of the document is a token change
    /// and so redistributes to live sessions, and `expires_in` (or the access
    /// token's JWT `exp`) is recorded as the account's `expires_at`.
    pub async fn set_token(
        &self,
        account: &str,
        access_token: String,
        refresh_token: Option<String>,
        expires_in: Option<u64>,
    ) -> anyhow::Result<()> {
        let mut accounts = self.accounts.write().await;
        let state = accounts
            .get_mut(account)
            .ok_or_else(|| anyhow::anyhow!("unknown account: {account}"))?;
        let (access_token, expires_at) =
            prepare_token(&state.config.provider, access_token, refresh_token, expires_in)?;

        // Emit a Refreshed event when the token OR the resolved profile changed.
        // profile_dirty is staged by set_base_url / set_litellm_key (called just
        // before this in the credentials/set handler) so an endpoint/header
        // change carried by a same-token sync still redistributes to live
        // sessions rather than being silently dropped (ka-0000002623, same-token
        // path). Consume the flag whichever branch we take.
        let token_changed = state.api_key.as_deref() != Some(access_token.as_str());
        let profile_changed = state.profile_dirty;
        state.profile_dirty = false;
        let status_changed = state.status != AccountStatus::Healthy;

        state.api_key = Some(access_token.clone());
        state.expires_at = expires_at;

        // Only transition Missing → Healthy. Preserve Throttled and Warming
        // status — those are set by local detection (429 responses, rate-limit
        // probes) and should only be cleared by explicit recovery signals
        // (clear_throttled / clear_warming). Without this guard, the
        // controller's periodic credential sync (every 5 min) would reset
        // Throttled accounts back to Healthy, causing new sessions to be
        // assigned to accounts that are still rate-limited.
        if state.status == AccountStatus::Missing {
            state.status = AccountStatus::Healthy;
        }

        if token_changed || profile_changed {
            let credentials = state.credential_map(&access_token);
            let _ = self
                .event_tx
                .send(CredentialEvent::Refreshed { account: account.to_owned(), credentials });
        } else if status_changed && state.status == AccountStatus::Healthy {
            tracing::debug!(account, "token unchanged but status restored to healthy");
        } else if status_changed {
            tracing::debug!(
                account,
                status = ?state.status,
                "token unchanged, preserving non-healthy status"
            );
        }

        self.persist_and_release(accounts).await;
        Ok(())
    }

    /// Load persisted credentials and seed account states.
    pub async fn load_persisted(&self, creds: &PersistedCredentials) {
        let mut accounts = self.accounts.write().await;

        // Restore dynamic account configs first.
        for acct_config in &creds.dynamic_accounts {
            if !accounts.contains_key(&acct_config.name) {
                accounts.insert(
                    acct_config.name.clone(),
                    AccountState::new(acct_config.clone(), AccountStatus::Missing, None),
                );
            }
        }

        // Seed API keys from persisted state.
        for (name, persisted) in &creds.accounts {
            if let Some(state) = accounts.get_mut(name) {
                state.api_key = Some(persisted.access_token.clone());
                state.expires_at = (persisted.expires_at > 0).then_some(persisted.expires_at);
                state.status = AccountStatus::Healthy;
            }
        }

        // Ensure session counts exist for all accounts.
        let mut counts = self.session_counts.write().await;
        for name in accounts.keys() {
            counts.entry(name.clone()).or_insert_with(|| AtomicU32::new(0));
        }
        let mut cumulative = self.cumulative_assignments.write().await;
        for name in accounts.keys() {
            cumulative.entry(name.clone()).or_insert_with(|| AtomicU64::new(0));
        }
    }

    /// Dynamically add a new account at runtime.
    pub async fn add_account(
        self: &Arc<Self>,
        config: AccountConfig,
        access_token: Option<String>,
        refresh_token: Option<String>,
        expires_in: Option<u64>,
    ) -> anyhow::Result<()> {
        let name = config.name.clone();
        let (access_token, expires_at) = match access_token {
            Some(token) => {
                let (token, expires_at) =
                    prepare_token(&config.provider, token, refresh_token, expires_in)?;
                (Some(token), expires_at)
            }
            None => (None, None),
        };

        {
            let mut accounts = self.accounts.write().await;
            if accounts.contains_key(&name) {
                anyhow::bail!("account already exists: {name}");
            }

            let has_token = access_token.is_some();
            let status = if has_token { AccountStatus::Healthy } else { AccountStatus::Missing };

            let mut state = AccountState::new(config.clone(), status, access_token.clone());
            state.expires_at = expires_at;
            accounts.insert(name.clone(), state);

            if let Some(ref token) = access_token {
                let credentials = accounts[&name].credential_map(token);
                let _ = self
                    .event_tx
                    .send(CredentialEvent::Refreshed { account: name.clone(), credentials });
            }

            self.persist_and_release(accounts).await;
        }

        self.session_counts.write().await.entry(name.clone()).or_insert_with(|| AtomicU32::new(0));
        self.cumulative_assignments
            .write()
            .await
            .entry(name.clone())
            .or_insert_with(|| AtomicU64::new(0));

        tracing::info!(account = %name, "account added");
        Ok(())
    }

    /// Remove a dynamically-added account at runtime.
    ///
    /// Returns an error if the account does not exist.
    pub async fn remove_account(&self, account: &str) -> anyhow::Result<()> {
        let mut accounts = self.accounts.write().await;
        if accounts.remove(account).is_none() {
            anyhow::bail!("unknown account: {account}");
        }

        self.persist_and_release(accounts).await;

        self.session_counts.write().await.remove(account);
        self.cumulative_assignments.write().await.remove(account);
        tracing::info!(account = %account, "account removed");
        Ok(())
    }

    /// Snapshot the in-memory account state into the serializable
    /// `PersistedCredentials` form. Pure and cheap — no I/O — so it is safe to
    /// run while the accounts lock is held (ka-0000007732).
    fn snapshot_for_persist(
        &self,
        accounts: &HashMap<String, AccountState>,
    ) -> PersistedCredentials {
        let mut persisted = PersistedCredentials::default();
        for (name, state) in accounts {
            if let Some(ref token) = state.api_key {
                persisted.accounts.insert(
                    name.clone(),
                    PersistedAccount {
                        access_token: token.clone(),
                        refresh_token: None,
                        expires_at: state.expires_at.unwrap_or(0),
                    },
                );
            }
            if !self.static_names.contains(name) {
                persisted.dynamic_accounts.push(state.config.clone());
            }
        }
        persisted
    }

    /// Persist current account state to disk WITHOUT holding the accounts lock
    /// (ka-0000007732).
    ///
    /// The caller passes its accounts write guard by value. We serialize a
    /// snapshot of the account state (cheap, in-memory) while the guard is
    /// still held — so the on-disk view is always consistent with a real
    /// point-in-time state — stamp it with a monotonic sequence, then **drop
    /// the accounts guard** before doing any blocking disk I/O. This means hot
    /// credential ops (assign_account, mark_throttled, status_of, …) never
    /// serialize behind an fsync on a slow persist_dir (real PVC / throttled
    /// node disk).
    ///
    /// The blocking `std::fs` work runs on a `spawn_blocking` thread so it also
    /// never stalls a tokio executor worker.
    async fn persist_and_release(
        &self,
        accounts: RwLockWriteGuard<'_, HashMap<String, AccountState>>,
    ) {
        // No persist_dir → nothing to write; just drop the guard.
        let Some(dir) = self.persist_dir.clone() else {
            drop(accounts);
            return;
        };

        // Build the snapshot and claim a sequence number while we still hold
        // the accounts lock, so snapshot order matches mutation order.
        let persisted = self.snapshot_for_persist(&accounts);
        let seq = self.persist_state.next_seq.fetch_add(1, Ordering::SeqCst) + 1;

        // Everything below is off the accounts lock.
        drop(accounts);

        let persist_state = Arc::clone(&self.persist_state);
        let chmod_warned = Arc::clone(&self.chmod_warned);
        #[cfg(test)]
        let delay_ms = self.persist_delay_ms.load(Ordering::Relaxed);

        // Serialize writers on the dedicated persist lock (never the accounts
        // lock). Last-writer-wins: if a newer snapshot already reached disk,
        // this superseded write is skipped.
        let mut written = persist_state.writer.lock().await;
        if *written >= seq {
            return;
        }
        let latest = seq;

        let result = tokio::task::spawn_blocking(move || {
            #[cfg(test)]
            if delay_ms > 0 {
                std::thread::sleep(std::time::Duration::from_millis(delay_ms));
            }
            Self::persist_blocking(&dir, &persisted, &chmod_warned)
        })
        .await;

        match result {
            Ok(Ok(())) => *written = latest,
            Ok(Err(e)) => tracing::warn!(err = %e, "failed to persist credentials"),
            Err(e) => tracing::warn!(err = %e, "credential persist task panicked"),
        }
    }

    /// The synchronous, blocking disk write. Runs on a `spawn_blocking` thread
    /// and NEVER while the accounts lock is held (ka-0000007732).
    fn persist_blocking(
        dir: &Path,
        persisted: &PersistedCredentials,
        chmod_warned: &AtomicBool,
    ) -> anyhow::Result<()> {
        let path = dir.join("credentials.json");
        if !dir.exists() {
            std::fs::create_dir_all(dir)?;
        }
        // W10 (ka-0000006638): credential state dir must be owner-only (0700)
        // so a co-tenant in the container cannot enumerate sibling tmp files
        // while we're mid-rename. emptyDir tmpfs mounts in K8s land at 0755
        // by default, so we re-check on every persist — but skip the chmod
        // (and the WARN-on-EPERM spam) when the dir already has correct perms
        // or when a previous chmod attempt failed (ka-0000002661).
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let current_mode = std::fs::metadata(dir).ok().map(|m| m.permissions().mode() & 0o777);
            if current_mode != Some(0o700) {
                if let Err(e) =
                    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))
                {
                    if !chmod_warned.swap(true, Ordering::Relaxed) {
                        tracing::warn!(
                            err = %e,
                            mode = ?current_mode.map(|m| format!("{m:o}")),
                            "failed to set 0700 on credential state dir; will not retry-warn"
                        );
                    } else {
                        tracing::debug!(err = %e, "chmod 0700 on credential state dir still failing");
                    }
                }
            }
        }
        crate::credential::persist::save(&path, persisted)
    }

    /// Whether any credential accounts are registered in the broker.
    pub async fn has_accounts(&self) -> bool {
        !self.accounts.read().await.is_empty()
    }

    /// Get status info for all accounts.
    pub async fn status_list(&self) -> Vec<AccountStatusInfo> {
        let accounts = self.accounts.read().await;
        accounts
            .iter()
            .map(|(name, state)| AccountStatusInfo {
                name: name.clone(),
                provider: state.config.provider.clone(),
                status: state.status,
                has_api_key: state.api_key.is_some(),
                expires_at: state.expires_at,
            })
            .collect()
    }

    /// Get the pool status: per-account utilization info.
    pub async fn pool_status(&self) -> Vec<PoolAccountInfo> {
        let accounts = self.accounts.read().await;
        let counts = self.session_counts.read().await;
        let cumulative = self.cumulative_assignments.read().await;

        accounts
            .iter()
            .map(|(name, state)| {
                let session_count =
                    counts.get(name).map(|c| c.load(Ordering::Relaxed)).unwrap_or(0);
                let cumulative_assignments =
                    cumulative.get(name).map(|c| c.load(Ordering::Relaxed)).unwrap_or(0);
                PoolAccountInfo {
                    name: name.clone(),
                    provider: state.config.provider.clone(),
                    status: state.status,
                    session_count,
                    cumulative_assignments,
                    has_api_key: state.api_key.is_some(),
                    expires_at: state.expires_at,
                }
            })
            .collect()
    }
}

// ── Public types ────────────────────────────────────────────────────────

/// Status info for an account (returned by the API).
#[derive(Debug, Clone, serde::Serialize)]
pub struct AccountStatusInfo {
    pub name: String,
    pub provider: String,
    pub status: AccountStatus,
    pub has_api_key: bool,
    /// Epoch seconds the current credential expires; `None` for static keys.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<u64>,
}

/// Pool utilization info for an account (returned by the pool API).
#[derive(Debug, Clone, serde::Serialize)]
pub struct PoolAccountInfo {
    pub name: String,
    pub provider: String,
    pub status: AccountStatus,
    pub session_count: u32,
    /// Lifetime count of sessions assigned to this account since coopmux
    /// started. Never decremented, so operators can see fairness over time.
    pub cumulative_assignments: u64,
    pub has_api_key: bool,
    /// Epoch seconds the current credential expires; `None` for static keys.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<u64>,
}

// ── Tests ────────────────────────────────────────────────────────────

#[cfg(test)]
#[path = "broker_tests.rs"]
mod tests;

#[cfg(test)]
#[path = "broker_codex_tests.rs"]
mod codex_tests;
