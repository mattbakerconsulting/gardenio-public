// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Throttle watcher: monitors session transitions for rate-limit and
//! budget-exhaustion signals, marks accounts as throttled in the broker,
//! and publishes NATS events so the controller can trigger failover.
//!
//! Subscribes to `MuxEvent::Transition` from the feed broadcast channel.
//! When a transition carries `error_category == "rate_limited"` or
//! `"budget_exceeded"`, the session's assigned account is marked throttled.
//! Recovery is detected when a throttled account's session transitions to
//! a working state.
//!
//! ## Single detection mechanism
//!
//! Throttle detection relies exclusively on `error_category` emitted by
//! coop-cli. The coop-cli `transition_to_msg` function classifies
//! `last_message` content on working→idle transitions and sets
//! `error_category` when a rate-limit pattern is found — so this watcher
//! does NOT need to duplicate that scan.
//!
//! ## Post-rebalance grace period
//!
//! After a successful project rebalance, sessions may still carry stale
//! `last_message` text from the previous account. A 15-second grace window
//! suppresses new throttle signals to avoid immediately marking the newly
//! assigned account as throttled on the next working→idle transition.

use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::sync::broadcast;

use crate::state::{MuxEvent, MuxState};

/// Error categories that indicate an account is throttled.
const THROTTLE_CATEGORIES: &[&str] = &["rate_limited", "budget_exceeded"];

/// Agent states that indicate the account is working again (recovery).
const WORKING_STATES: &[&str] = &["working", "running", "streaming", "tool_use"];

/// After a rebalance, suppress new throttle signals for this duration.
const POST_REBALANCE_GRACE: Duration = Duration::from_secs(15);

/// Spawn a throttle watcher task that monitors session transitions for
/// rate-limit signals and marks accounts as throttled in the broker.
///
/// Also spawns a NATS warming watcher that subscribes to controller-emitted
/// `account_warming` / `account_cooled` events for soft failover.
pub fn spawn_throttle_watcher(state: Arc<MuxState>) {
    let mut event_rx = state.feed.event_tx.subscribe();

    // Spawn NATS warming watcher as a sibling task.
    spawn_warming_watcher(Arc::clone(&state));

    tokio::spawn(async move {
        // Track the last time a rebalance was performed to implement the
        // post-rebalance grace period.
        let mut last_rebalanced_at: Option<Instant> = None;

        loop {
            let event = match event_rx.recv().await {
                Ok(e) => e,
                Err(broadcast::error::RecvError::Lagged(n)) => {
                    tracing::debug!(skipped = n, "throttle watcher lagged");
                    continue;
                }
                Err(broadcast::error::RecvError::Closed) => break,
            };

            if let MuxEvent::Transition {
                ref session,
                ref next,
                ref error_category,
                ref error_detail,
                ..
            } = event
            {
                // Check for throttle signal via error_category set by coop-cli.
                // coop-cli classifies working→idle last_message patterns and
                // sets error_category, so we don't need a separate scan here.
                if let Some(ref cat) = error_category {
                    if THROTTLE_CATEGORIES.contains(&cat.as_str()) {
                        let in_grace = last_rebalanced_at
                            .map(|t| t.elapsed() < POST_REBALANCE_GRACE)
                            .unwrap_or(false);

                        if in_grace {
                            tracing::debug!(
                                session = %session,
                                error_category = %cat,
                                "throttle watcher: suppressing throttle signal (post-rebalance grace)"
                            );
                            continue;
                        }

                        let rebalanced =
                            handle_throttle(&state, session, cat, error_detail.as_deref()).await;
                        if rebalanced {
                            last_rebalanced_at = Some(Instant::now());
                        }
                        continue;
                    }
                }

                // Check for recovery: a session on a throttled account starts working.
                if WORKING_STATES.contains(&next.as_str()) {
                    handle_recovery(&state, session).await;
                }
            }
        }
    });
}

/// NATS subjects for controller-emitted soft failover events.
const WARMING_SUBJECT: &str = "coopmux.events.auth.account_warming";
const COOLED_SUBJECT: &str = "coopmux.events.auth.account_cooled";

/// Spawn a background task that subscribes to NATS warming/cooled events
/// from the controller's ratelimit probe. These drive soft failover: warming
/// accounts stop receiving new sessions, cooled accounts are restored.
fn spawn_warming_watcher(state: Arc<MuxState>) {
    let shutdown = state.shutdown.clone();
    tokio::spawn(async move {
        if let Err(e) = run_warming_watcher(state, shutdown).await {
            tracing::warn!(err = %e, "warming watcher exited");
        }
    });
}

/// NATS warming/cooled event payload from the controller ratelimit probe.
#[derive(serde::Deserialize)]
struct WarmingEvent {
    #[serde(default)]
    from_account: String,
    #[serde(default)]
    utilization: f64,
    #[serde(default)]
    threshold: f64,
}

async fn run_warming_watcher(
    state: Arc<MuxState>,
    shutdown: tokio_util::sync::CancellationToken,
) -> anyhow::Result<()> {
    // Wait for the NATS client to become available (set by nats_sub).
    let client = loop {
        {
            let guard = state.nats_client.read().await;
            if let Some(ref c) = *guard {
                break c.clone();
            }
        }
        tokio::select! {
            _ = shutdown.cancelled() => return Ok(()),
            _ = tokio::time::sleep(Duration::from_secs(2)) => {}
        }
    };

    tracing::info!("warming watcher: subscribed to NATS warming/cooled events");

    let mut warming_sub = client.subscribe(WARMING_SUBJECT.to_owned()).await?;
    let mut cooled_sub = client.subscribe(COOLED_SUBJECT.to_owned()).await?;

    loop {
        tokio::select! {
            _ = shutdown.cancelled() => break,
            msg = futures_util::StreamExt::next(&mut warming_sub) => {
                let Some(msg) = msg else { break };
                let event: WarmingEvent = match serde_json::from_slice(&msg.payload) {
                    Ok(e) => e,
                    Err(e) => {
                        tracing::debug!(err = %e, "warming watcher: invalid warming payload");
                        continue;
                    }
                };
                if event.from_account.is_empty() {
                    continue;
                }
                let marked = state.credential_broker.mark_warming(&event.from_account).await;
                if marked {
                    tracing::warn!(
                        account = %event.from_account,
                        utilization = event.utilization,
                        threshold = event.threshold,
                        "warming watcher: account marked as warming, steering new sessions away"
                    );
                }
            }
            msg = futures_util::StreamExt::next(&mut cooled_sub) => {
                let Some(msg) = msg else { break };
                let event: WarmingEvent = match serde_json::from_slice(&msg.payload) {
                    Ok(e) => e,
                    Err(e) => {
                        tracing::debug!(err = %e, "warming watcher: invalid cooled payload");
                        continue;
                    }
                };
                if event.from_account.is_empty() {
                    continue;
                }
                let cleared = state.credential_broker.clear_warming(&event.from_account).await;
                if cleared {
                    tracing::info!(
                        account = %event.from_account,
                        utilization = event.utilization,
                        threshold = event.threshold,
                        "warming watcher: account cooled, restored for new sessions"
                    );
                }
            }
        }
    }

    Ok(())
}

/// Handle a throttle signal: mark the session's account as throttled and
/// publish a NATS event.
///
/// Returns `true` if a project rebalance was performed (used to start the
/// post-rebalance grace period).
async fn handle_throttle(
    state: &MuxState,
    session_id: &str,
    error_category: &str,
    error_detail: Option<&str>,
) -> bool {
    let account = match lookup_session_account(state, session_id).await {
        Some(a) => a,
        None => return false,
    };

    let broker = &state.credential_broker;

    broker.mark_throttled(&account).await;

    tracing::warn!(
        account = %account,
        session = %session_id,
        error_category,
        error_detail,
        "throttle watcher: account throttled"
    );

    // Publish NATS event for observability / controller awareness.
    publish_throttle_event(state, &account, session_id, error_category, error_detail).await;

    // Kill switch (ka-0000005463): a rebalance pushes /session/switch to every
    // affected session, restarting the agent process mid-work. When operators
    // are troubleshooting restart storms this env turns the session-moving
    // half off while keeping throttle marking + NATS events (observability)
    // intact.
    if failover_disabled() {
        tracing::warn!(
            account = %account,
            "throttle watcher: COOPMUX_DISABLE_FAILOVER set — skipping project rebalance"
        );
        return false;
    }

    // Trigger project-scoped rebalance using cached fallback chains from beads.
    //
    // Snapshot the cache and DROP the read guard before the rebalance runs: it
    // issues timeout-bounded-but-still-blocking HTTP POSTs to every affected
    // session's pod, and holding the (write-preferring) read lock across that
    // I/O would queue the periodic cache-refresh writer and, behind it, every
    // other reader (new-session staging, distributor) — stalling the whole
    // credential plane during the exact throttle storm this rebalance remediates
    // (ka-0000004551). The snapshot clones a few small HashMaps.
    let cache_snapshot = {
        let cache = state.account_cache.read().await;
        (*cache).clone()
    };
    let cache_updates =
        crate::credential::failover::project_scoped_rebalance(state, &cache_snapshot, &account)
            .await;

    let rebalanced = !cache_updates.is_empty();

    // Apply rebalance results to the in-memory cache so new session registrations
    // pick up the updated account assignment without waiting for a pod restart.
    if rebalanced {
        let mut cache = state.account_cache.write().await;
        for (project, (new_account, new_primary)) in &cache_updates {
            cache.update_project_account(project, new_account, new_primary.as_deref());
        }
    }

    rebalanced
}

/// Handle potential recovery: if a session on a throttled account transitions
/// to a working state, clear the throttle and publish a recovery signal.
async fn handle_recovery(state: &MuxState, session_id: &str) {
    let account = match lookup_session_account(state, session_id).await {
        Some(a) => a,
        None => return,
    };

    let broker = &state.credential_broker;

    // Only act if the account was actually throttled.
    if !broker.clear_throttled(&account).await {
        return;
    }

    tracing::info!(
        account = %account,
        session = %session_id,
        "throttle watcher: account recovered"
    );

    // Publish NATS recovery event so other components can react. Switchback is
    // abolished (ka-0000008240): coopmux no longer moves projects back onto a
    // recovered "primary". The recovered account simply rejoins the equal spread
    // pool once the controller's SweepStaleExhausted clears its exhausted status
    // on the account bead; new session registrations pick it up from there.
    publish_recovery_event(state, &account, session_id).await;
}

/// Operator kill switch for the session-moving half of throttle handling
/// (ka-0000005463): when COOPMUX_DISABLE_FAILOVER is set, rebalance and
/// switchback are skipped so no /session/switch is pushed to live agents.
/// Throttle bookkeeping and NATS events keep flowing for observability.
fn failover_disabled() -> bool {
    std::env::var("COOPMUX_DISABLE_FAILOVER").is_ok()
}

/// Look up the assigned account for a session.
async fn lookup_session_account(state: &MuxState, session_id: &str) -> Option<String> {
    let sessions = state.sessions.read().await;
    let entry = Arc::clone(sessions.get(session_id)?);
    drop(sessions);
    let account = entry.assigned_account.read().await.clone();
    account
}

/// Publish an account-recovery event to NATS so other components (controller,
/// coopmux switchback logic) can react.
async fn publish_recovery_event(state: &MuxState, account: &str, session_id: &str) {
    let nats_client = state.nats_client.read().await;
    let client = match nats_client.as_ref() {
        Some(c) => c,
        None => return,
    };

    let payload = serde_json::json!({
        "event": "account_healthy",
        "account": account,
        "session_id": session_id,
        "source": "coopmux",
    });

    let bytes = match serde_json::to_vec(&payload) {
        Ok(b) => b,
        Err(e) => {
            tracing::warn!(err = %e, "throttle watcher: failed to serialize recovery payload");
            return;
        }
    };

    let subject = "coopmux.events.auth.account_healthy".to_owned();
    if let Err(e) = client.publish(subject.clone(), bytes.into()).await {
        tracing::warn!(subject, err = %e, "throttle watcher: failed to publish recovery event");
    }
}

/// Publish a throttle event to NATS using the existing `auth.budget_exceeded`
/// subject pattern so the controller picks it up without changes.
async fn publish_throttle_event(
    state: &MuxState,
    account: &str,
    session_id: &str,
    error_category: &str,
    error_detail: Option<&str>,
) {
    let nats_client = state.nats_client.read().await;
    let client = match nats_client.as_ref() {
        Some(c) => c,
        None => return,
    };

    let payload = serde_json::json!({
        "event": "account_throttled",
        "account": account,
        "session_id": session_id,
        "error_category": error_category,
        "error_detail": error_detail.unwrap_or_default(),
        "source": "coopmux",
    });

    let bytes = match serde_json::to_vec(&payload) {
        Ok(b) => b,
        Err(e) => {
            tracing::warn!(err = %e, "throttle watcher: failed to serialize NATS payload");
            return;
        }
    };

    // Publish on the existing pattern the controller subscribes to.
    let subject = "coopmux.events.auth.budget_exceeded".to_owned();
    if let Err(e) = client.publish(subject.clone(), bytes.into()).await {
        tracing::warn!(subject, err = %e, "throttle watcher: failed to publish NATS event");
    }
}

#[cfg(test)]
#[path = "throttle_tests.rs"]
mod tests;
