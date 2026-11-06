// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Named credential profiles and automatic rotation on rate limit.
//!
//! Profiles are registered via the API and stored in memory. When the agent
//! hits a rate-limit error, the session loop calls [`ProfileState::try_auto_rotate`]
//! to pick the next available profile and produce a [`SwitchRequest`].

use std::collections::{HashMap, VecDeque};
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicU8, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use tokio::sync::{broadcast, RwLock};
use tracing::debug;

use crate::driver::{AgentState, AgentType};
use crate::event::ProfileEvent;
use crate::switch::SwitchRequest;

/// A registered credential profile.
#[derive(Debug)]
pub struct Profile {
    pub name: String,
    pub credentials: HashMap<String, String>,
    pub status: ProfileStatus,
    /// Bead-authoritative health flag, pushed by coopmux (ka-0000006430).
    ///
    /// `false` means the account this profile is staged for has gone
    /// `status=exhausted`/`throttle_status=rejected` (or `disabled`) on its
    /// account bead SINCE it was staged, so [`try_auto_rotate`](ProfileState::try_auto_rotate)
    /// must not rotate onto it. **Fail-open:** defaults to `true`, so a profile
    /// with no health signal (old coopmux, or an account whose bead we can't
    /// read) is always eligible — a missing signal must never strand a session.
    pub usable: bool,
}

/// Current status of a profile.
#[derive(Debug)]
pub enum ProfileStatus {
    /// This profile is currently in use.
    Active,
    /// This profile is available for rotation.
    Available,
    /// This profile hit a rate limit and is cooling down.
    RateLimited { cooldown_until: Instant },
}

/// Process-wide profile rotation mode.
///
/// - `Auto`: automatically rotate on rate limit errors.
/// - `Manual`: detection works but no automatic rotation.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ProfileMode {
    Auto,
    Manual,
}

impl ProfileMode {
    fn as_u8(self) -> u8 {
        match self {
            Self::Auto => 0,
            Self::Manual => 1,
        }
    }

    fn from_u8(v: u8) -> Self {
        match v {
            1 => Self::Manual,
            _ => Self::Auto,
        }
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Manual => "manual",
        }
    }
}

impl std::fmt::Display for ProfileMode {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

impl std::str::FromStr for ProfileMode {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.to_lowercase().as_str() {
            "auto" => Ok(Self::Auto),
            "manual" => Ok(Self::Manual),
            other => anyhow::bail!("invalid profile mode: {other} (expected auto or manual)"),
        }
    }
}

/// Serializable snapshot of a profile's state.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProfileInfo {
    pub name: String,
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cooldown_remaining_secs: Option<u64>,
}

/// Tunables governing anti-flap and cooldown behaviour during auto-rotation.
///
/// Resolved **once** at session setup (see [`RotationConfig::from_env`]) and
/// held on [`ProfileState`]. Rotation logic must read these values from the
/// struct — never from process-global env inside [`ProfileState::try_auto_rotate`]
/// — so unit tests stay hermetic regardless of the ambient environment
/// (ka-0000003994: the agent runtime sets `COOP_ROTATE_MAX_PER_HOUR=0`, which
/// used to make every rotation test fail with "expected Switch, got Skipped").
#[derive(Debug, Clone, Copy)]
pub struct RotationConfig {
    /// Maximum profile switches allowed per rolling hour (anti-flap guard).
    pub max_switches_per_hour: u32,
    /// Cooldown applied to a profile when it is marked rate-limited.
    pub cooldown: Duration,
    /// Whether to run a read-only auth probe against a rotation candidate
    /// BEFORE committing the switch (ka-0000007302). When true, auto-rotate
    /// skips any candidate whose credential fails the probe (revoked token,
    /// org-disabled Claude Code access) instead of switching onto it and
    /// cold-restarting into the same dead account.
    ///
    /// Defaults to `false` in [`RotationConfig::default`] so unit tests stay
    /// hermetic (no network), and to `true` in [`RotationConfig::from_env`] so
    /// production sessions get the guard; disable at runtime with
    /// `COOP_ROTATE_PRESWITCH_PROBE=0` for rollback.
    pub preswitch_probe: bool,
    /// Whether to skip rotation candidates whose bead-authoritative health flag
    /// (`Profile::usable`) is `false` (ka-0000006430). coopmux stamps that flag
    /// from the account bead's `status`/`throttle_status`, so this stops
    /// `try_auto_rotate` from landing a long-lived session onto a profile that
    /// was healthy when staged but has since gone exhausted (the fo85 incident).
    ///
    /// Fail-open regardless: a profile with no health signal (`usable == true`,
    /// the default) is always eligible. This flag only governs whether an
    /// explicitly-condemned (`usable == false`) profile is filtered out.
    ///
    /// Defaults to `true` (both [`RotationConfig::default`] and
    /// [`RotationConfig::from_env`]) — the gate is a pure superset filter over
    /// data that defaults usable, so it changes no behavior until coopmux
    /// pushes a condemnation, and needs no network. Disable at runtime with
    /// `COOP_ROTATE_HEALTH_GATE=0` for rollback to the exact prior behavior.
    pub health_gate: bool,
}

impl Default for RotationConfig {
    fn default() -> Self {
        Self {
            max_switches_per_hour: 20,
            cooldown: Duration::from_secs(300),
            preswitch_probe: false,
            // Default ON: it is a no-network superset filter that no-ops until a
            // profile is explicitly condemned, so it is safe even for the
            // hermetic default config used by unit tests.
            health_gate: true,
        }
    }
}

impl RotationConfig {
    /// Build a config from the process environment (`COOP_ROTATE_MAX_PER_HOUR`,
    /// `COOP_ROTATE_COOLDOWN_SECS`, `COOP_ROTATE_PRESWITCH_PROBE`), falling back
    /// to defaults. Call this exactly once, at the composition root (session
    /// setup) — never inside hot rotation logic — so unit tests never depend on
    /// ambient env.
    pub fn from_env() -> Self {
        let default = Self::default();
        Self {
            max_switches_per_hour: env_u32(
                "COOP_ROTATE_MAX_PER_HOUR",
                default.max_switches_per_hour,
            ),
            cooldown: Duration::from_secs(env_u64(
                "COOP_ROTATE_COOLDOWN_SECS",
                default.cooldown.as_secs(),
            )),
            // Pre-switch probe defaults ON in production; opt out with =0.
            preswitch_probe: env_bool("COOP_ROTATE_PRESWITCH_PROBE", true),
            // Health-flag gate defaults ON; opt out with =0 for rollback.
            health_gate: env_bool("COOP_ROTATE_HEALTH_GATE", true),
        }
    }
}

/// Shared profile state. Lives on `Store`.
pub struct ProfileState {
    profiles: RwLock<Vec<Profile>>,
    /// Process-wide rotation mode (0=auto, 1=manual).
    mode: AtomicU8,
    switch_history: RwLock<VecDeque<Instant>>,
    /// Dedup flag: ensures only one retry timer is pending at a time.
    retry_pending: AtomicBool,
    /// Anti-flap / cooldown tunables, resolved once at construction.
    rotation: RotationConfig,
    /// Read-only prober used to validate a rotation candidate before switching
    /// onto it (gated by `rotation.preswitch_probe`). Injectable for hermetic
    /// tests; defaults to [`RealProber`] in the standard constructors.
    prober: Arc<dyn CredentialProber>,
    /// Whether this session's harness is Codex (`Some(true)`), Claude/Gemini
    /// (`Some(false)`), or unknown (`None`; the harness is then inferred from
    /// the Active profile). A session only ever holds, and rotates onto,
    /// profiles of its own harness: see [`ProfileState::fits`].
    harness_codex: Option<bool>,
    /// Broadcast channel for profile lifecycle events.
    pub profile_tx: broadcast::Sender<ProfileEvent>,
}

/// Whether a credential map is a Codex (ChatGPT subscription) credential.
/// Codex profiles are the only ones that carry the `auth.json` document.
fn is_codex_credentials(credentials: &HashMap<String, String>) -> bool {
    credentials.contains_key(crate::driver::codex::auth::AUTH_ENV)
}

/// Entry in a registration request.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProfileEntry {
    pub name: String,
    pub credentials: HashMap<String, String>,
    /// Optional bead-authoritative health flag pushed by coopmux
    /// (ka-0000006430). `Some(false)` condemns this profile as a rotation
    /// target; `Some(true)` clears it; `None` (an old coopmux, or a body that
    /// omits the field) leaves the profile's current usability untouched and
    /// defaults new profiles to usable. Serialized only when set so pushes stay
    /// wire-compatible with pre-flag coop-cli.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usable: Option<bool>,
}

/// Result of attempting automatic profile rotation.
#[derive(Debug)]
pub enum RotateOutcome {
    /// Switch to this profile now.
    Switch(SwitchRequest),
    /// All profiles on cooldown; retry after this duration.
    Exhausted { retry_after: Duration },
    /// Rotation not applicable (disabled, < 2 profiles, anti-flap).
    Skipped,
}

/// Read a `u64` from an env var, falling back to a default.
fn env_u64(var: &str, default: u64) -> u64 {
    std::env::var(var).ok().and_then(|v| v.parse().ok()).unwrap_or(default)
}

/// Read a `u32` from an env var, falling back to a default.
fn env_u32(var: &str, default: u32) -> u32 {
    std::env::var(var).ok().and_then(|v| v.parse().ok()).unwrap_or(default)
}

/// Read a boolean-ish env var, falling back to a default. Accepts the usual
/// truthy/falsey spellings (`1/0`, `true/false`, `yes/no`, `on/off`);
/// unrecognised values fall back to the default.
fn env_bool(var: &str, default: bool) -> bool {
    match std::env::var(var) {
        Ok(v) => match v.trim().to_ascii_lowercase().as_str() {
            "1" | "true" | "yes" | "on" => true,
            "0" | "false" | "no" | "off" => false,
            _ => default,
        },
        Err(_) => default,
    }
}

/// A read-only credential prober used to validate a rotation candidate BEFORE
/// switching onto it. Abstracted behind a trait so unit tests inject a fake and
/// never touch the network (ka-0000007302). The production implementation
/// ([`RealProber`]) delegates to [`crate::probe::probe_credentials`].
pub trait CredentialProber: Send + Sync {
    /// Probe a candidate profile's credentials, returning only the resulting
    /// [`crate::probe::ProbeStatus`].
    fn probe(
        &self,
        name: String,
        credentials: HashMap<String, String>,
    ) -> Pin<Box<dyn Future<Output = crate::probe::ProbeStatus> + Send>>;
}

/// Production prober: hits the real Anthropic (or LiteLLM) read-only endpoint.
pub struct RealProber;

impl CredentialProber for RealProber {
    fn probe(
        &self,
        name: String,
        credentials: HashMap<String, String>,
    ) -> Pin<Box<dyn Future<Output = crate::probe::ProbeStatus> + Send>> {
        Box::pin(async move { crate::probe::probe_credentials(&name, &credentials).await.status })
    }
}

impl Default for ProfileState {
    fn default() -> Self {
        Self::new()
    }
}

impl ProfileState {
    /// Create an empty profile state with **default** rotation config.
    ///
    /// This constructor is env-independent by design so unit tests are
    /// hermetic. Production wiring that wants to honour the
    /// `COOP_ROTATE_*` env knobs must use [`ProfileState::with_rotation`]
    /// (typically `ProfileState::with_rotation(RotationConfig::from_env())`).
    pub fn new() -> Self {
        Self::with_rotation(RotationConfig::default())
    }

    /// Create an empty profile state with an explicit rotation config and the
    /// production [`RealProber`].
    pub fn with_rotation(rotation: RotationConfig) -> Self {
        Self::with_rotation_and_prober(rotation, Arc::new(RealProber))
    }

    /// Create an empty profile state with an explicit rotation config and an
    /// explicit prober. Tests inject a fake prober here so pre-switch probing
    /// stays hermetic; production wiring uses [`with_rotation`](Self::with_rotation).
    pub fn with_rotation_and_prober(
        rotation: RotationConfig,
        prober: Arc<dyn CredentialProber>,
    ) -> Self {
        let (profile_tx, _) = broadcast::channel(64);
        Self {
            profiles: RwLock::new(Vec::new()),
            mode: AtomicU8::new(ProfileMode::Auto.as_u8()),
            switch_history: RwLock::new(VecDeque::new()),
            retry_pending: AtomicBool::new(false),
            rotation,
            prober,
            harness_codex: None,
            profile_tx,
        }
    }

    /// Bind this state to the session's agent. Profiles of the other harness —
    /// a Codex `auth.json` profile on a Claude session, or a Claude token
    /// profile on a Codex session — are then refused at [`register`](Self::register)
    /// and never offered by [`try_auto_rotate`](Self::try_auto_rotate). coopmux
    /// already filters by provider before pushing; this is the receiving-end
    /// backstop so a mis-push can never put a foreign credential in rotation.
    #[must_use]
    pub fn for_agent(mut self, agent: AgentType) -> Self {
        self.harness_codex = match agent {
            AgentType::Codex => Some(true),
            AgentType::Claude | AgentType::Gemini => Some(false),
            AgentType::Unknown => None,
        };
        self
    }

    /// Whether `credentials` belong to this session's harness. With no bound
    /// agent the harness is inferred from `active` (the Active profile's
    /// credentials), and anything fits while there is no Active profile yet.
    fn fits(
        &self,
        credentials: &HashMap<String, String>,
        active: Option<&HashMap<String, String>>,
    ) -> bool {
        match self.harness_codex.or_else(|| active.map(is_codex_credentials)) {
            Some(codex) => codex == is_codex_credentials(credentials),
            None => true,
        }
    }

    /// Return the current rotation mode.
    pub fn mode(&self) -> ProfileMode {
        ProfileMode::from_u8(self.mode.load(Ordering::Acquire))
    }

    /// Set the rotation mode.
    pub fn set_mode(&self, mode: ProfileMode) {
        self.mode.store(mode.as_u8(), Ordering::Release);
    }

    /// Register credential profiles additively (upsert), preserving the
    /// currently Active profile.
    ///
    /// Existing profiles are updated in place (credentials refreshed, status
    /// untouched); unseen profiles are appended as Available. The first entry
    /// becomes Active **only** when the profile set was previously empty (fresh
    /// session) — so initial registration still seeds an Active profile for
    /// `active_name()` / the health `account_name`.
    ///
    /// This is intentionally NOT a wholesale replace. The coopmux credential
    /// distributor pushes a single-profile body on every account add/refresh
    /// to all eligible sessions; a replace marked that lone profile Active and
    /// clobbered every session's reported `account_name` to the last-pushed
    /// account — even sessions on unrelated projects whose live credentials
    /// were never switched (ka-0000005975). Moving the Active profile is now
    /// owned exclusively by the switch path ([`set_active`](Self::set_active)).
    pub async fn register(&self, entries: Vec<ProfileEntry>) {
        let mut profiles = self.profiles.write().await;
        let was_empty = profiles.is_empty();
        for entry in entries {
            let active_creds = profiles
                .iter()
                .find(|p| matches!(p.status, ProfileStatus::Active))
                .map(|p| &p.credentials);
            if !self.fits(&entry.credentials, active_creds) {
                tracing::warn!(
                    profile = %entry.name,
                    "refusing credential profile for a different harness than this session"
                );
                continue;
            }
            if let Some(existing) = profiles.iter_mut().find(|p| p.name == entry.name) {
                // Known profile: refresh credentials, preserve status so an
                // Active/RateLimited profile is never demoted by a push.
                existing.credentials = entry.credentials;
                // Update the bead-authoritative health flag when the push
                // carries one (ka-0000006430); a push that omits it (`None`)
                // leaves the last-known usability intact so a creds-only refresh
                // never resurrects a condemned profile.
                if let Some(usable) = entry.usable {
                    existing.usable = usable;
                }
            } else {
                profiles.push(Profile {
                    name: entry.name,
                    credentials: entry.credentials,
                    status: ProfileStatus::Available,
                    // Fail-open: a profile with no explicit health signal is
                    // usable until coopmux condemns it.
                    usable: entry.usable.unwrap_or(true),
                });
            }
        }
        // Seed an Active profile only on the first registration into an empty
        // set. Subsequent pushes never change which profile is Active.
        if was_empty {
            if let Some(first) = profiles.first_mut() {
                first.status = ProfileStatus::Active;
            }
        }
    }

    /// Return a serializable snapshot of all profiles.
    pub async fn list(&self) -> Vec<ProfileInfo> {
        let profiles = self.profiles.read().await;
        let now = Instant::now();
        profiles
            .iter()
            .map(|p| {
                let (status, cooldown) = match &p.status {
                    ProfileStatus::Active => ("active".to_owned(), None),
                    ProfileStatus::Available => ("available".to_owned(), None),
                    ProfileStatus::RateLimited { cooldown_until } => {
                        let remaining = cooldown_until.saturating_duration_since(now).as_secs();
                        ("rate_limited".to_owned(), Some(remaining))
                    }
                };
                ProfileInfo { name: p.name.clone(), status, cooldown_remaining_secs: cooldown }
            })
            .collect()
    }

    /// Return the name of the currently active profile, if any.
    pub async fn active_name(&self) -> Option<String> {
        let profiles = self.profiles.read().await;
        profiles.iter().find(|p| matches!(p.status, ProfileStatus::Active)).map(|p| p.name.clone())
    }

    /// Resolve credentials for a named profile.
    pub async fn resolve_credentials(&self, name: &str) -> Option<HashMap<String, String>> {
        let profiles = self.profiles.read().await;
        profiles.iter().find(|p| p.name == name).map(|p| p.credentials.clone())
    }

    /// Mark a profile as Active after a successful switch.
    pub async fn set_active(&self, name: &str) -> bool {
        let mut profiles = self.profiles.write().await;
        let found = profiles.iter().any(|p| p.name == name);
        if found {
            let prev_active = profiles
                .iter()
                .find(|p| matches!(p.status, ProfileStatus::Active))
                .map(|p| p.name.clone());
            for p in profiles.iter_mut() {
                if p.name == name {
                    p.status = ProfileStatus::Active;
                } else if matches!(p.status, ProfileStatus::Active) {
                    p.status = ProfileStatus::Available;
                }
            }
            drop(profiles);
            let _ = self
                .profile_tx
                .send(ProfileEvent::ProfileSwitched { from: prev_active, to: name.to_owned() });
        }
        found
    }

    /// Core rotation method: check mode, anti-flap, mark current as rate-limited,
    /// pick the next available profile that passes a pre-switch probe, and
    /// return a [`RotateOutcome`].
    ///
    /// Pre-switch probe (ka-0000007302): when `rotation.preswitch_probe` is set,
    /// each rotation candidate is validated with a read-only auth probe BEFORE
    /// the switch is committed. A candidate whose credential is already dead —
    /// revoked OAuth token, org-disabled Claude Code access (the observed
    /// `matthewbaker1110` hard error) — fails the probe (401/403 -> `Invalid`)
    /// and is skipped, so auto-rotate never lands a session on it only to
    /// cold-restart into the same failure. An inconclusive probe (timeout / 5xx
    /// / no probeable credential) is treated as passing: a flaky probe must
    /// never strand a session that a legitimate switch could have recovered.
    ///
    /// The probe is network I/O, so it runs WITHOUT the `profiles` write lock
    /// held (the same snapshot-then-probe shape as [`probe_all`](Self::probe_all)) --
    /// state is mutated under the lock, released across the probe, then
    /// re-acquired to finalize.
    pub async fn try_auto_rotate(&self) -> RotateOutcome {
        // Guard: rotation disabled.
        if self.mode() == ProfileMode::Manual {
            return RotateOutcome::Skipped;
        }

        // Phase 1 -- fast, pure state mutation under the write lock: run the
        // guards, mark the current active profile rate-limited, promote expired
        // cooldowns, and collect the ordered rotation candidates. No network
        // I/O happens here, so the lock is held only briefly.
        let candidates: Vec<(String, HashMap<String, String>)> = {
            let mut profiles = self.profiles.write().await;

            // Guard: need at least 2 profiles to rotate.
            if profiles.len() < 2 {
                return RotateOutcome::Skipped;
            }

            // Anti-flap: check switch rate. Tunables come from `self.rotation`
            // (resolved once at construction) -- never from process-global env
            // here, so unit tests stay hermetic (ka-0000003994).
            let max_switches_per_hour = self.rotation.max_switches_per_hour;
            {
                let mut history = self.switch_history.write().await;
                let one_hour_ago = Instant::now() - Duration::from_secs(3600);
                while history.front().is_some_and(|t| *t < one_hour_ago) {
                    history.pop_front();
                }
                if history.len() as u32 >= max_switches_per_hour {
                    return RotateOutcome::Skipped;
                }
            }

            let now = Instant::now();
            let cooldown = self.rotation.cooldown;

            // Mark current active profile as rate-limited.
            let active_idx =
                profiles.iter().position(|p| matches!(p.status, ProfileStatus::Active));
            if let Some(idx) = active_idx {
                let exhausted_name = profiles[idx].name.clone();
                profiles[idx].status =
                    ProfileStatus::RateLimited { cooldown_until: now + cooldown };
                let _ = self
                    .profile_tx
                    .send(ProfileEvent::ProfileExhausted { profile: exhausted_name });
            }

            // Promote expired cooldowns to Available.
            for p in profiles.iter_mut() {
                if let ProfileStatus::RateLimited { cooldown_until } = &p.status {
                    if *cooldown_until <= now {
                        p.status = ProfileStatus::Available;
                    }
                }
            }

            // Collect Available candidates in round-robin order (from after
            // active), cloning name+creds so the probe below needs no lock.
            //
            // Health gate (ka-0000006430): when `health_gate` is on, drop any
            // candidate coopmux has condemned via the bead-authoritative
            // `usable` flag (an account that went exhausted/rejected AFTER it was
            // staged). The profile stays staged — only skipped as a rotation
            // target — so switchback is trivial once it recovers, mirroring the
            // cooldown model. Fail-open: `usable` defaults true, so a profile
            // with no health signal is never filtered here.
            let health_gate = self.rotation.health_gate;
            let start = active_idx.map(|i| i + 1).unwrap_or(0);
            let len = profiles.len();
            (0..len)
                .map(|offset| (start + offset) % len)
                .filter(|&i| matches!(profiles[i].status, ProfileStatus::Available))
                .filter(|&i| !health_gate || profiles[i].usable)
                // Harness gate: never rotate a Claude session onto a Codex
                // profile, nor the reverse.
                .filter(|&i| {
                    self.fits(
                        &profiles[i].credentials,
                        active_idx.map(|a| &profiles[a].credentials),
                    )
                })
                .map(|i| (profiles[i].name.clone(), profiles[i].credentials.clone()))
                .collect()
        };

        // Phase 2 -- choose a candidate WITHOUT holding the lock. With the probe
        // enabled, walk candidates in priority order and take the first that
        // does not FAIL the probe (Invalid); remember the rejected ones so they
        // can be cooled down. With the probe disabled, keep the historical
        // behaviour: take the first available candidate.
        let mut chosen: Option<(String, HashMap<String, String>)> = None;
        let mut rejected: Vec<String> = Vec::new();
        if self.rotation.preswitch_probe {
            for (name, creds) in candidates {
                match self.prober.probe(name.clone(), creds.clone()).await {
                    crate::probe::ProbeStatus::Invalid => {
                        debug!(
                            profile = %name,
                            "pre-switch probe rejected rotation candidate (auth invalid); skipping"
                        );
                        rejected.push(name);
                    }
                    status => {
                        debug!(
                            profile = %name,
                            ?status,
                            "pre-switch probe accepted rotation candidate"
                        );
                        chosen = Some((name, creds));
                        break;
                    }
                }
            }
        } else {
            chosen = candidates.into_iter().next();
        }

        // Phase 3 -- finalize under the lock: cool down any probe-rejected
        // candidate so it is not retried immediately, then either record the
        // switch (anti-flap accounting) and return it, or report exhaustion.
        let now = Instant::now();
        let cooldown = self.rotation.cooldown;
        if !rejected.is_empty() {
            let mut profiles = self.profiles.write().await;
            for name in &rejected {
                if let Some(p) = profiles.iter_mut().find(|p| &p.name == name) {
                    // Only demote a still-Available profile -- never stomp one a
                    // concurrent switch may have re-activated in the meantime.
                    if matches!(p.status, ProfileStatus::Available) {
                        p.status = ProfileStatus::RateLimited { cooldown_until: now + cooldown };
                    }
                }
            }
        }

        match chosen {
            Some((next_name, next_creds)) => {
                // Record switch timestamp. Acquire switch_history WITHOUT
                // holding profiles to avoid lock-order issues (both are RwLocks
                // on the same struct).
                self.switch_history.write().await.push_back(Instant::now());

                RotateOutcome::Switch(SwitchRequest {
                    credentials: Some(next_creds),
                    force: true,
                    profile: Some(next_name),
                })
            }
            None => {
                // No usable candidate -- none Available, or every candidate
                // failed the probe. Compute retry_after from the shortest
                // remaining cooldown.
                let profiles = self.profiles.read().await;
                let retry_after = profiles
                    .iter()
                    .filter_map(|p| match &p.status {
                        ProfileStatus::RateLimited { cooldown_until } => {
                            Some(cooldown_until.saturating_duration_since(now))
                        }
                        _ => None,
                    })
                    .min()
                    .unwrap_or(cooldown);
                let _ = self.profile_tx.send(ProfileEvent::ProfileRotationExhausted {
                    retry_after_secs: retry_after.as_secs(),
                });
                RotateOutcome::Exhausted { retry_after }
            }
        }
    }

    /// Probe all registered profiles' credentials for validity.
    ///
    /// Returns a [`ProbeResult`] for each profile. Probing is done concurrently
    /// but respects the HTTP timeout per probe. Profiles with no recognized
    /// credential keys are returned with [`ProbeStatus::Skipped`].
    pub async fn probe_all(&self) -> Vec<crate::probe::ProbeResult> {
        let snapshots: Vec<_> = {
            let profiles = self.profiles.read().await;
            profiles.iter().map(|p| (p.name.clone(), p.credentials.clone())).collect()
        };
        let futs: Vec<_> = snapshots
            .iter()
            .map(|(name, creds)| crate::probe::probe_credentials(name, creds))
            .collect();
        futures_util::future::join_all(futs).await
    }

    /// Probe a single named profile's credentials.
    pub async fn probe_one(&self, name: &str) -> Option<crate::probe::ProbeResult> {
        let profiles = self.profiles.read().await;
        let profile = profiles.iter().find(|p| p.name == name)?;
        let name = profile.name.clone();
        let creds = profile.credentials.clone();
        drop(profiles);
        Some(crate::probe::probe_credentials(&name, &creds).await)
    }

    /// Spawn a delayed retry task that calls `try_auto_rotate` once cooldowns expire.
    ///
    /// Uses an `AtomicBool` flag to ensure only one retry timer is pending.
    /// The timer no-ops if the agent is no longer in `Parked` state when it fires.
    pub fn schedule_retry(
        self: &Arc<Self>,
        retry_after: Duration,
        store: Arc<crate::transport::Store>,
    ) {
        // Dedup: only one retry timer at a time.
        if self.retry_pending.swap(true, Ordering::AcqRel) {
            return;
        }
        let profile = Arc::clone(self);
        tokio::spawn(async move {
            tokio::time::sleep(retry_after).await;

            // Clear the dedup flag so future retries can schedule.
            profile.retry_pending.store(false, Ordering::Release);

            // Guard: only retry if the agent is still Parked.
            let current = store.driver.agent_state.read().await;
            if !matches!(&*current, AgentState::Parked { .. }) {
                debug!("retry timer fired but agent is no longer parked, skipping");
                return;
            }
            drop(current);

            match profile.try_auto_rotate().await {
                RotateOutcome::Switch(req) => {
                    debug!("retry timer: cooldown expired, switching to profile {:?}", req.profile);
                    let _ = store.switch.switch_tx.try_send(req);
                }
                RotateOutcome::Exhausted { retry_after } => {
                    debug!("retry timer: still exhausted, re-scheduling in {retry_after:?}");
                    profile.schedule_retry(retry_after, store);
                }
                RotateOutcome::Skipped => {
                    debug!("retry timer: rotation skipped");
                }
            }
        });
    }
}

#[cfg(test)]
#[path = "profile_tests.rs"]
mod tests;
