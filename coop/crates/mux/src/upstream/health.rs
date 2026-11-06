// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Background health checker for all registered sessions.

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use crate::state::MuxState;
use crate::upstream::client::UpstreamClient;

/// Timeout for individual upstream health check requests.
/// Kept short so dead pods don't block the checker and starve the mux's
/// own liveness probe.
const HEALTH_CHECK_TIMEOUT: Duration = Duration::from_secs(3);

/// Spawn a single background task that periodically checks health of all sessions.
pub fn spawn_health_checker(state: Arc<MuxState>) {
    let interval = state.config.health_check_interval();
    let max_failures = state.config.max_health_failures;

    tokio::spawn(async move {
        let mut timer = tokio::time::interval(interval);
        timer.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

        loop {
            tokio::select! {
                _ = state.shutdown.cancelled() => break,
                _ = timer.tick() => {}
            }

            // Snapshot current sessions.
            let entries: Vec<_> = {
                let sessions = state.sessions.read().await;
                sessions.values().map(Arc::clone).collect()
            };

            // Run all health checks concurrently so a single dead pod
            // doesn't block the entire tick and starve the mux health endpoint.
            let state_ref = &state;
            let checks: Vec<_> = entries
                .iter()
                .filter(|entry| {
                    !entry.cancel.is_cancelled()
                        && !matches!(entry.transport, crate::state::SessionTransport::Nats { .. })
                })
                .map(|entry| {
                    let entry = Arc::clone(entry);
                    let http = state.http_client.clone();
                    async move {
                        let client = UpstreamClient::with_timeout(
                            http,
                            entry.url.clone(),
                            entry.auth_token.clone(),
                            HEALTH_CHECK_TIMEOUT,
                        );
                        (entry, client.health().await)
                    }
                })
                .collect();

            let results = futures_util::future::join_all(checks).await;

            for (entry, result) in results {
                if entry.cancel.is_cancelled() {
                    continue;
                }

                match result {
                    Ok(health_json) => {
                        entry.health_failures.store(0, Ordering::Relaxed);

                        // Reconcile account name from the session's health
                        // response. The health endpoint includes account_name
                        // which tracks the active credential profile. This
                        // corrects stale assigned_account values that can diverge
                        // after profile switches or credential rotations.
                        if let Some(name) = health_json
                            .get("account_name")
                            .and_then(|v| v.as_str())
                            .filter(|s| !s.is_empty())
                        {
                            let current = entry.assigned_account.read().await.clone();
                            if current.as_deref() != Some(name) {
                                tracing::debug!(
                                    session_id = %entry.id,
                                    current = ?current,
                                    reported = %name,
                                    "reconciling account name from session health"
                                );
                                // Move the broker session_count in lockstep with
                                // the account rewrite. Every other assigned_account
                                // mutation that replaces an existing account
                                // (reassign, operator rebalance) pairs the write
                                // with session_unassigned(old)+session_assigned(new)
                                // so the least-loaded counter stays balanced. This
                                // reconcile is the one site that didn't: when coop
                                // self-rotates onto a new profile (its reported
                                // account_name diverges), the old account's +1 was
                                // orphaned — never decremented on deregister (which
                                // unassigns the *current* account) — permanently
                                // inflating the old account's count and steering
                                // least-loaded away from an actually-idle account.
                                // Only move the count when replacing a *known* old
                                // account; a fill-from-None here is an adopt of an
                                // uncounted fresh session (registration never calls
                                // session_assigned), so counting it would inflate.
                                if let Some(ref old) = current {
                                    state_ref.credential_broker.session_unassigned(old).await;
                                    state_ref.credential_broker.session_assigned(name).await;
                                }
                                *entry.assigned_account.write().await = Some(name.to_owned());
                            }
                        }
                    }
                    Err(e) => {
                        let prev = entry.health_failures.fetch_add(1, Ordering::Relaxed);
                        let count = prev + 1;
                        tracing::warn!(
                            session_id = %entry.id,
                            failures = count,
                            err = %e,
                            "health check failed"
                        );

                        if count >= max_failures {
                            tracing::warn!(
                                session_id = %entry.id,
                                "evicting session after {count} consecutive health failures"
                            );
                            // Unassign from credential pool before removal.
                            if let Some(account) = entry.assigned_account.read().await.as_ref() {
                                state_ref.credential_broker.session_unassigned(account).await;
                            }
                            state_ref.remove_session(&entry.id).await;
                        }
                    }
                }
            }
        }
    });
}

#[cfg(test)]
#[path = "health_tests.rs"]
mod tests;
