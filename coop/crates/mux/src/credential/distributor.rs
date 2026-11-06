// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Credential distributor: pushes credentials to sessions as profiles.
//!
//! Listens for `CredentialEvent::Refreshed` events (emitted when API keys are
//! set via `set_token()` or `add_account()`) and distributes them to registered
//! sessions as profiles.
//!
//! Features:
//! - **Per-pod filtering**: sessions can declare `profiles_needed` in metadata
//!   to receive only matching credentials (wildcard `*` or omission = all).
//! - **Idle-check**: queries upstream status before triggering a profile switch;
//!   busy sessions receive credentials but defer the switch.
//! - **Concurrency control**: limits concurrent per-session pushes via semaphore.
//! - **Retries**: per-session distribution retries with exponential backoff.

use std::sync::Arc;
use std::time::Duration;

use tokio::sync::{broadcast, Semaphore};

use crate::credential::CredentialEvent;
use crate::state::MuxState;
use crate::upstream::client::UpstreamClient;

/// Maximum concurrent per-session credential pushes.
const MAX_CONCURRENT_PUSHES: usize = 8;

/// Maximum retries per session for a single distribution round.
const MAX_RETRIES: u32 = 3;

/// Initial backoff duration for retries.
const INITIAL_BACKOFF: Duration = Duration::from_millis(500);

/// Maximum backoff duration for retries.
const MAX_BACKOFF: Duration = Duration::from_secs(5);

/// Bounded per-request timeout for the profile-push / status / profile-switch
/// POSTs to a session's own coop pod. The shared `reqwest::Client`
/// (`state.http_client`) carries no default timeout, so without this a push to
/// a hung/parked pod (TCP accepted but never responds — parked/half-open/node
/// under memory pressure) blocks forever. Because `distribute_to_sessions`
/// awaits every per-session push handle before returning to `event_rx.recv()`,
/// a single such pod would freeze the entire serial distributor and globally
/// stall credential rotation. Matches the reassign path (ka-0000004551,
/// `REASSIGN_REQUEST_TIMEOUT`). See ka-0000002323.
const PUSH_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

/// Agent states considered "busy" (switch is deferred, credentials still pushed).
const BUSY_STATES: &[&str] = &["running", "streaming", "tool_use"];

/// Spawn a distributor task that listens for credential refresh events
/// and pushes fresh credentials to registered sessions.
pub fn spawn_distributor(state: Arc<MuxState>, mut event_rx: broadcast::Receiver<CredentialEvent>) {
    tokio::spawn(async move {
        loop {
            let event = match event_rx.recv().await {
                Ok(e) => e,
                Err(broadcast::error::RecvError::Lagged(n)) => {
                    tracing::debug!(skipped = n, "distributor lagged");
                    continue;
                }
                Err(broadcast::error::RecvError::Closed) => break,
            };

            match event {
                CredentialEvent::Refreshed { account, credentials } => {
                    distribute_to_sessions(&state, &account, &credentials, true).await;
                }
                CredentialEvent::RefreshFailed { ref account, .. } => {
                    // Auto-rebalance: reassign sessions from the failed account
                    // to a healthy one so agents don't lose API access.
                    rebalance_from_account(&state, account).await;
                }
            }
        }
    });
}

/// Push credentials to matching registered sessions as a profile.
///
/// When `switch` is true, also triggers a profile switch on each session —
/// unless the session is currently busy, in which case credentials are
/// registered but the switch is deferred.
pub async fn distribute_to_sessions(
    state: &MuxState,
    account: &str,
    credentials: &std::collections::HashMap<String, String>,
    switch: bool,
) {
    let sessions = state.sessions.read().await;
    let count = sessions.len();
    if count == 0 {
        tracing::info!(account, "distributor: no sessions registered, nothing to distribute");
        return;
    }

    // Collect eligible sessions: the account's provider must match the
    // session's harness (a Claude session never receives a Codex profile, nor
    // the reverse), the land wall must admit it (a land's purse account reaches
    // only that land's sessions, and a land session receives no fleet account),
    // and the session's profiles_needed metadata must admit it.
    let snapshot: Vec<_> = sessions.values().cloned().collect();
    drop(sessions);
    let provider = state.credential_broker.provider_for_credentials(account, credentials).await;
    let known_lands = super::land::known_land_accounts(state).await;
    let mut eligible = Vec::with_capacity(snapshot.len());
    for entry in snapshot {
        let assigned = entry.assigned_account.read().await.clone();
        let land = super::land::session_land(&entry.metadata, assigned.as_deref(), &known_lands);
        if session_accepts_account(&entry.metadata, account, &provider, land.as_deref()) {
            eligible.push(entry);
        }
    }

    let eligible_count = eligible.len();
    let skipped = count - eligible_count;
    if eligible_count == 0 {
        tracing::info!(
            account,
            count,
            skipped,
            "distributor: no eligible sessions for this account"
        );
        return;
    }

    // Bead-authoritative health for this account, stamped onto every pushed
    // profile so coop's in-session rotation skips a condemned one (ka-0000006430).
    // This is the "completeness fix" from the design: a `Refreshed` event on a
    // billing-dead account would otherwise re-push it with no health signal.
    //
    // Fail-open on an UNKNOWN account: `account_usable` returns `false` for an
    // account absent from the cache (no health signal), but that must NOT
    // condemn a legitimately-refreshed profile before the cache is populated.
    // Only stamp `false` when the account is KNOWN and condemned; an unknown
    // account (empty/not-yet-loaded cache) pushes `true`, matching the pre-fix
    // behavior and coop's own default.
    let usable = {
        let cache = state.account_cache.read().await;
        if cache.all_accounts().contains_key(account) {
            cache.account_usable(account)
        } else {
            true
        }
    };
    tracing::info!(
        account,
        eligible = eligible_count,
        skipped,
        "distributor: pushing credentials to eligible sessions"
    );

    let semaphore = Arc::new(Semaphore::new(MAX_CONCURRENT_PUSHES));
    let ok = Arc::new(std::sync::atomic::AtomicU32::new(0));
    let failed = Arc::new(std::sync::atomic::AtomicU32::new(0));
    let deferred = Arc::new(std::sync::atomic::AtomicU32::new(0));

    let mut handles = Vec::with_capacity(eligible_count);

    for entry in eligible {
        let sem = Arc::clone(&semaphore);
        let ok = Arc::clone(&ok);
        let failed = Arc::clone(&failed);
        let deferred = Arc::clone(&deferred);
        let account = account.to_owned();
        let credentials = credentials.clone();
        let http = state.http_client.clone();

        handles.push(tokio::spawn(async move {
            let _permit = sem.acquire().await;

            // Only switch if this is the session's assigned account. Other
            // accounts get their profiles registered (for failover readiness)
            // but don't trigger a switch away from the current assignment.
            let should_switch =
                switch && entry.assigned_account.read().await.as_deref() == Some(account.as_str());

            match push_to_session(http, &entry, &account, &credentials, should_switch, usable).await
            {
                PushResult::Ok => {
                    ok.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                }
                PushResult::Deferred => {
                    deferred.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                }
                PushResult::Failed => {
                    failed.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                }
            }
        }));
    }

    // Await all concurrent pushes.
    for h in handles {
        let _ = h.await;
    }

    let ok = ok.load(std::sync::atomic::Ordering::Relaxed);
    let failed = failed.load(std::sync::atomic::Ordering::Relaxed);
    let deferred = deferred.load(std::sync::atomic::Ordering::Relaxed);
    tracing::info!(account, ok, failed, deferred, "distributor: distribution complete");
}

/// Rebalance sessions from a failed/degraded account to a healthy one.
///
/// Called automatically when a credential refresh fails. Finds all sessions
/// assigned to the degraded account and reassigns them to the least-loaded
/// healthy account, switching their active profile.
pub async fn rebalance_from_account(state: &MuxState, failed_account: &str) {
    let broker = &state.credential_broker;

    // A land's purse account has no fallback: its sessions stay on it, and the
    // refresh failure surfaces through the usual refresh-failure notification.
    // Moving them onto a fleet account would bill the land's work to the fleet.
    if super::land::land_of_account(failed_account).is_some() {
        tracing::warn!(
            account = failed_account,
            "pool rebalance: refresh failed on a land account, leaving its sessions in place"
        );
        return;
    }

    // Find a healthy target account, constrained to accounts the controller's
    // rate-limit probe has NOT condemned on their account bead (ka-0000006526).
    // The broker's in-process AccountStatus starts Healthy after every coopmux
    // restart and only reflects 429s THIS process has seen, so `assign_account`
    // alone would happily return a bead-exhausted/rejected account in that
    // window and drop the session straight onto a dead key. Gating on the
    // bead-authoritative usable set keeps this path in agreement with the
    // new-session staging path (transport/http.rs) and find_fallback_for_project.
    //
    // Provider gate: every session on `failed_account` runs that account's
    // harness, so the target must be of the same provider family — a Claude
    // session is never rebalanced onto a Codex account, nor the reverse.
    //
    // Land wall: the candidates never include a land's purse account
    // (`fleet_rebalance_candidates`), with or without an account cache.
    let family = broker.account_family(failed_account).await;
    let candidates = super::land::fleet_rebalance_candidates(state).await;
    let target = broker.assign_account_for_project_family(None, &candidates, family).await;
    let Some(target_name) = target else {
        tracing::warn!(
            account = failed_account,
            "pool rebalance: no healthy accounts available for reassignment"
        );
        return;
    };

    // Don't rebalance to the same failed account.
    if target_name == failed_account {
        tracing::debug!(
            account = failed_account,
            "pool rebalance: only account is the failed one, skipping"
        );
        return;
    }

    // Get credentials for the target account.
    let Some(target_creds) = broker.get_credentials(&target_name).await else {
        tracing::warn!(
            account = %target_name,
            "pool rebalance: target account has no credentials"
        );
        return;
    };

    let target_provider = broker.account_provider(&target_name).await.unwrap_or_default();

    // Collect affected sessions under the read lock, then drop before I/O. A
    // land session that is (wrongly) on a fleet account is left alone rather
    // than moved to another fleet account.
    let known_lands = super::land::known_land_accounts(state).await;
    let affected: Vec<_> = {
        let sessions = state.sessions.read().await;
        let mut result = Vec::new();
        for entry in sessions.values() {
            let current = entry.assigned_account.read().await.clone();
            let land = super::land::session_land(&entry.metadata, current.as_deref(), &known_lands);
            if current.as_deref() == Some(failed_account)
                && super::land::land_wall_allows(land.as_deref(), &target_name)
                && super::provider_compatible(
                    super::session_family(&entry.metadata),
                    &target_provider,
                )
            {
                result.push(entry.clone());
            }
        }
        result
    };

    let mut reassigned = 0u32;

    for entry in &affected {
        if super::reassign::reassign_session(
            state.http_client.clone(),
            broker,
            entry,
            failed_account,
            &target_name,
            &target_creds,
        )
        .await
        {
            tracing::info!(
                session = %entry.id,
                from = failed_account,
                to = %target_name,
                "pool: auto-rebalanced session from failed account"
            );
            reassigned += 1;
        } else {
            tracing::warn!(
                session = %entry.id,
                "pool: auto-rebalance switch failed"
            );
        }
    }

    if reassigned > 0 {
        tracing::info!(
            from = failed_account,
            to = %target_name,
            count = reassigned,
            "pool: auto-rebalanced sessions away from failed account"
        );
    }
}

/// Result of pushing credentials to a single session.
enum PushResult {
    /// Credentials pushed and switch triggered.
    Ok,
    /// Credentials pushed but switch deferred (session busy).
    Deferred,
    /// Failed after retries.
    Failed,
}

/// Push credentials to a single session with retries and idle checking.
///
/// `usable` is the bead-authoritative health flag stamped onto the pushed
/// profile (ka-0000006430) so coop's in-session rotation skips a condemned
/// account. Fail-open: pass `true` when health is unknown.
async fn push_to_session(
    http_client: reqwest::Client,
    entry: &crate::state::SessionEntry,
    account: &str,
    credentials: &std::collections::HashMap<String, String>,
    switch: bool,
    usable: bool,
) -> PushResult {
    let client = UpstreamClient::with_timeout(
        http_client,
        entry.url.clone(),
        entry.auth_token.clone(),
        PUSH_REQUEST_TIMEOUT,
    );
    let profile_body = serde_json::json!({
        "profiles": [{
            "name": account,
            "credentials": credentials,
            "usable": usable,
        }]
    });

    // Retry loop for profile registration.
    let mut backoff = INITIAL_BACKOFF;
    let mut registered = false;
    for attempt in 0..=MAX_RETRIES {
        match client.post_json("/api/v1/session/profiles", &profile_body).await {
            Ok(_) => {
                registered = true;
                break;
            }
            Err(e) => {
                if attempt == MAX_RETRIES {
                    tracing::warn!(
                        session = %entry.id, account, attempt, err = %e,
                        "distributor: failed to push profile after retries"
                    );
                    return PushResult::Failed;
                }
                tracing::debug!(
                    session = %entry.id, account, attempt, err = %e,
                    "distributor: profile push failed, retrying"
                );
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(MAX_BACKOFF);
            }
        }
    }

    if !registered {
        return PushResult::Failed;
    }

    if !switch {
        tracing::info!(session = %entry.id, account, "distributor: credentials pushed (no switch requested)");
        return PushResult::Ok;
    }

    // Status check: query upstream to decide whether/how to switch.
    // - Busy agents (running/streaming/tool_use) → defer switch.
    // - Errored agents → force switch (they won't recover without fresh creds).
    // - Idle/unknown → normal switch.
    let (is_busy, is_error) = match client.get_status().await {
        Ok(status) => {
            let state = status.get("state").and_then(|s| s.as_str()).unwrap_or("");
            (BUSY_STATES.contains(&state), state == "error")
        }
        Err(e) => {
            tracing::debug!(
                session = %entry.id, account, err = %e,
                "distributor: could not check session status, assuming idle"
            );
            (false, false)
        }
    };

    if is_busy {
        tracing::info!(
            session = %entry.id, account,
            "distributor: credentials pushed, switch deferred (session busy)"
        );
        return PushResult::Deferred;
    }

    // Force switch for errored agents — they're stuck and won't transition
    // to idle on their own, so a normal (non-force) switch would be stored
    // as pending but never executed.
    let force = is_error;
    if force {
        tracing::info!(
            session = %entry.id, account,
            "distributor: agent in error state, using force switch"
        );
    }

    // Trigger profile switch with retry.
    let switch_body = serde_json::json!({
        "profile": account,
        "force": force,
    });

    backoff = INITIAL_BACKOFF;
    for attempt in 0..=MAX_RETRIES {
        match client.post_json("/api/v1/session/switch", &switch_body).await {
            Ok(_) => {
                tracing::info!(
                    session = %entry.id, account,
                    "distributor: credentials pushed and switch triggered"
                );
                return PushResult::Ok;
            }
            Err(e) => {
                if attempt == MAX_RETRIES {
                    tracing::warn!(
                        session = %entry.id, account, attempt, err = %e,
                        "distributor: switch failed after retries (credentials still registered)"
                    );
                    // Credentials were registered, switch just failed.
                    return PushResult::Deferred;
                }
                tracing::debug!(
                    session = %entry.id, account, attempt, err = %e,
                    "distributor: switch failed, retrying"
                );
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(MAX_BACKOFF);
            }
        }
    }

    PushResult::Deferred
}

/// Whether a session of `session_land` (`None` = a fleet session, see
/// [`session_land`](super::land::session_land)) may receive the profile for
/// `account` (of `provider`): the provider must be compatible with the
/// session's agent, the land wall must admit the account, and the session's
/// `profiles_needed` metadata must admit it.
///
/// Provider compatibility and the land wall are checked first and cannot be
/// overridden by `profiles_needed` (not even `["*"]`): a session without a
/// filter would otherwise receive every account's profile, including another
/// harness's or a land's purse account.
pub fn session_accepts_account(
    metadata: &serde_json::Value,
    account: &str,
    provider: &str,
    session_land: Option<&str>,
) -> bool {
    super::provider_compatible(super::session_family(metadata), provider)
        && super::land::land_wall_allows(session_land, account)
        && session_needs_account_metadata(metadata, account)
}

/// Check whether metadata indicates a session needs a given account.
///
/// Public variant used by session registration to filter initial profile pushes.
pub fn session_needs_account_metadata(metadata: &serde_json::Value, account: &str) -> bool {
    let Some(needed) = metadata.get("profiles_needed") else {
        return true; // No filter → receives everything.
    };
    let Some(arr) = needed.as_array() else {
        return true; // Malformed → treat as no filter.
    };
    if arr.is_empty() {
        return true; // Empty array → receives everything.
    }
    let account_lower = account.to_lowercase();
    arr.iter().any(|v| v.as_str().is_some_and(|s| s == "*" || s.to_lowercase() == account_lower))
}

#[cfg(test)]
#[path = "distributor_tests.rs"]
mod tests;

#[cfg(test)]
#[path = "distributor_e2e_tests.rs"]
mod e2e_tests;

#[cfg(test)]
#[path = "distributor_provider_tests.rs"]
mod provider_tests;

#[cfg(test)]
#[path = "distributor_land_tests.rs"]
mod land_tests;
