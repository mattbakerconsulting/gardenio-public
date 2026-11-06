// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Shared session reassignment: pool bookkeeping + profile push + switch.
//!
//! Consolidates the duplicated 7-step pattern used when moving a session from
//! one credential account to another (rebalance, failover, switchback).

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use crate::credential::broker::CredentialBroker;
use crate::credential::{provider_compatible, session_family};
use crate::state::SessionEntry;
use crate::upstream::client::UpstreamClient;

/// Bounded per-request timeout for the profile-push / profile-switch POSTs to a
/// session's own coop pod. The shared `reqwest::Client` carries no default
/// timeout, so without this a reassign to a hung/parked pod would block forever
/// — and the throttle-storm rebalance that drives these reassigns is exactly
/// when the target pods are most likely slow or unresponsive. See ka-0000004551.
const REASSIGN_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

/// Reassign a single session from one account to another.
///
/// Performs:
/// 1. Pool bookkeeping (unassign old, assign new)
/// 2. Update `assigned_account` on the session entry
/// 3. Push credentials as a profile to the session
/// 4. Trigger a profile switch
///
/// Refuses (returns `false`, touching nothing) when `to_account`'s provider does
/// not match the session's agent: a Claude session is never moved onto a Codex
/// account, nor the reverse. Every selector already filters by provider; this is
/// the backstop at the one place that actually moves a session.
///
/// Returns `true` if the switch API call succeeded, `false` otherwise.
/// Pool bookkeeping and profile push are best-effort — the switch result
/// determines success.
pub async fn reassign_session(
    http_client: reqwest::Client,
    broker: &CredentialBroker,
    entry: &Arc<SessionEntry>,
    from_account: &str,
    to_account: &str,
    credentials: &HashMap<String, String>,
) -> bool {
    // 0. Provider gate.
    let provider = broker.provider_for_credentials(to_account, credentials).await;
    if !provider_compatible(session_family(&entry.metadata), &provider) {
        tracing::warn!(
            session = %entry.id,
            to = to_account,
            provider = %provider,
            "reassign refused: account provider does not match the session's agent"
        );
        return false;
    }

    // 1. Pool bookkeeping.
    broker.session_unassigned(from_account).await;
    broker.session_assigned(to_account).await;

    // 2. Update assigned account.
    *entry.assigned_account.write().await = Some(to_account.to_owned());

    // 3. Push credentials as profile.
    let client = UpstreamClient::with_timeout(
        http_client,
        entry.url.clone(),
        entry.auth_token.clone(),
        REASSIGN_REQUEST_TIMEOUT,
    );
    let profile_body = serde_json::json!({
        "profiles": [{
            "name": to_account,
            "credentials": credentials,
        }]
    });
    let _ = client.post_json("/api/v1/session/profiles", &profile_body).await;

    // 4. Trigger profile switch.
    let switch_body = serde_json::json!({ "profile": to_account });
    client.post_json("/api/v1/session/switch", &switch_body).await.is_ok()
}

#[cfg(test)]
#[path = "reassign_tests.rs"]
mod tests;
