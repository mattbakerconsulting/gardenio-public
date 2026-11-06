// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! HTTP handlers for credential management endpoints.

use std::sync::Arc;
use std::time::Duration;

use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
use serde::Deserialize;

use crate::credential::{land, AccountConfig};
use crate::error::MuxError;
use crate::state::MuxState;

/// Validate that an account name contains only safe characters: `[a-zA-Z0-9_-]+`.
/// Rejects empty strings, path separators, dots, and any other special characters
/// that could enable path traversal or filename attacks.
fn validate_account_name(name: &str) -> Result<(), String> {
    if name.is_empty() {
        return Err("account name must not be empty".into());
    }
    if !name.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-') {
        return Err(format!(
            "account name contains invalid characters (allowed: a-zA-Z0-9_-): {name}"
        ));
    }
    Ok(())
}

/// `GET /api/v1/credentials/status` — list all accounts with status.
pub async fn credentials_status(State(s): State<Arc<MuxState>>) -> impl IntoResponse {
    let broker = &s.credential_broker;
    let list = broker.status_list().await;
    Json(list).into_response()
}

/// Request body for `POST /api/v1/credentials/set`.
#[derive(Debug, Deserialize)]
pub struct SetRequest {
    pub account: String,
    pub token: String,
    #[serde(default)]
    pub refresh_token: Option<String>,
    #[serde(default)]
    pub expires_in: Option<u64>,
    /// LiteLLM virtual key for per-account budget tracking. When present,
    /// distributed to sessions as `ANTHROPIC_CUSTOM_HEADERS` so that
    /// credential rotation updates cost attribution without pod restarts.
    #[serde(default)]
    pub litellm_key: Option<String>,
    /// API endpoint for this account (controller-resolved). When present,
    /// distributed to sessions as `ANTHROPIC_BASE_URL` so that credential
    /// rotation onto this account also switches the endpoint the session
    /// authenticates against (ka-0000002623).
    #[serde(default)]
    pub base_url: Option<String>,
}

/// `POST /api/v1/credentials/set` — set API key for an existing account.
pub async fn credentials_set(
    State(s): State<Arc<MuxState>>,
    Json(req): Json<SetRequest>,
) -> impl IntoResponse {
    let broker = &s.credential_broker;
    // Store litellm_key on the account before setting the token, so that
    // the Refreshed event (emitted by set_token) includes it in credentials.
    if let Some(ref key) = req.litellm_key {
        broker.set_litellm_key(&req.account, key.clone()).await;
    }
    // Same for base_url: stored before set_token so the Refreshed event and
    // staged fallback profiles carry ANTHROPIC_BASE_URL.
    if let Some(ref url) = req.base_url {
        broker.set_base_url(&req.account, url.clone()).await;
    }
    match broker.set_token(&req.account, req.token, req.refresh_token, req.expires_in).await {
        Ok(()) => Json(serde_json::json!({ "ok": true })).into_response(),
        Err(e) => MuxError::BadRequest.to_http_response(e.to_string()).into_response(),
    }
}

/// Request body for `POST /api/v1/credentials/reauth`.
#[derive(Debug, Deserialize)]
pub struct ReauthRequest {
    #[serde(default)]
    pub account: Option<String>,
}

/// `POST /api/v1/credentials/reauth` — OAuth reauth is no longer supported.
pub async fn credentials_reauth(
    State(_s): State<Arc<MuxState>>,
    Json(_req): Json<ReauthRequest>,
) -> impl IntoResponse {
    MuxError::BadRequest
        .to_http_response(
            "OAuth reauth is no longer supported; use static API keys via /api/v1/credentials/set",
        )
        .into_response()
}

/// Request body for `POST /api/v1/credentials/new`.
#[derive(Debug, Deserialize)]
pub struct NewAccountRequest {
    pub name: String,
    pub provider: String,
    #[serde(default)]
    pub env_key: Option<String>,
    /// Model name for this account (e.g. "claude-opus-4-6"). Propagated to
    /// sessions as CLAUDE_MODEL during credential distribution / rotation.
    #[serde(default)]
    pub model: Option<String>,
    #[serde(default)]
    pub token_url: Option<String>,
    #[serde(default)]
    pub client_id: Option<String>,
    #[serde(default)]
    pub auth_url: Option<String>,
    /// OAuth device authorization endpoint (legacy).
    #[serde(default)]
    pub device_auth_url: Option<String>,
    /// Optional API key to set immediately.
    #[serde(default)]
    pub token: Option<String>,
    #[serde(default)]
    pub refresh_token: Option<String>,
    #[serde(default)]
    pub expires_in: Option<u64>,
    /// Legacy field, ignored.
    #[serde(default = "crate::credential::default_true")]
    pub reauth: bool,
}

/// `POST /api/v1/credentials/new` — create a new credential account.
pub async fn credentials_new(
    State(s): State<Arc<MuxState>>,
    Json(req): Json<NewAccountRequest>,
) -> impl IntoResponse {
    if let Err(msg) = validate_account_name(&req.name) {
        return MuxError::BadRequest.to_http_response(msg).into_response();
    }

    let broker = &s.credential_broker;

    let config = AccountConfig {
        name: req.name.clone(),
        provider: req.provider,
        env_key: req.env_key,
        model: req.model,
        token_url: req.token_url,
        client_id: req.client_id,
        auth_url: req.auth_url,
        device_auth_url: req.device_auth_url,
        reauth: req.reauth,
    };

    match broker.add_account(config, req.token, req.refresh_token, req.expires_in).await {
        Ok(()) => Json(serde_json::json!({ "added": true, "account": req.name })).into_response(),
        Err(e) => MuxError::BadRequest.to_http_response(e.to_string()).into_response(),
    }
}

/// `DELETE /api/v1/credentials/{account}` — remove a credential account.
pub async fn credentials_delete(
    State(s): State<Arc<MuxState>>,
    axum::extract::Path(account): axum::extract::Path<String>,
) -> impl IntoResponse {
    if let Err(msg) = validate_account_name(&account) {
        return MuxError::BadRequest.to_http_response(msg).into_response();
    }

    let broker = &s.credential_broker;

    match broker.remove_account(&account).await {
        Ok(()) => Json(serde_json::json!({ "removed": true, "account": account })).into_response(),
        Err(e) => MuxError::BadRequest.to_http_response(e.to_string()).into_response(),
    }
}

/// Request body for `POST /api/v1/credentials/distribute`.
#[derive(Debug, Deserialize)]
pub struct DistributeRequest {
    pub account: String,
    #[serde(default)]
    pub switch: bool,
}

/// `POST /api/v1/credentials/distribute` — manually push an account's credentials to all sessions.
pub async fn credentials_distribute(
    State(s): State<Arc<MuxState>>,
    Json(req): Json<DistributeRequest>,
) -> impl IntoResponse {
    let broker = &s.credential_broker;

    let credentials = match broker.get_credentials(&req.account).await {
        Some(creds) => creds,
        None => {
            return MuxError::BadRequest
                .to_http_response(format!("no credentials available for account: {}", req.account))
                .into_response()
        }
    };

    crate::credential::distributor::distribute_to_sessions(
        &s,
        &req.account,
        &credentials,
        req.switch,
    )
    .await;
    Json(serde_json::json!({ "distributed": true, "account": req.account })).into_response()
}

/// `GET /api/v1/credentials/pool` — pool utilization status enriched with probe data.
pub async fn credentials_pool(State(s): State<Arc<MuxState>>) -> impl IntoResponse {
    let broker = &s.credential_broker;

    let pool = broker.pool_status().await;
    let total_sessions: u32 = pool.iter().map(|a| a.session_count).sum();
    let healthy_accounts =
        pool.iter().filter(|a| a.status == crate::credential::AccountStatus::Healthy).count();

    // Enrich each pool entry with probe data from the account cache.
    let cache = s.account_cache.read().await;
    let probe_map = cache.all_accounts();

    let accounts: Vec<serde_json::Value> = pool
        .iter()
        .map(|acct| {
            let mut v = serde_json::to_value(acct).unwrap_or_default();
            if let Some(info) = probe_map.get(&acct.name) {
                if let serde_json::Value::Object(ref mut map) = v {
                    map.insert(
                        "throttle_status".into(),
                        serde_json::Value::String(info.throttle_status.clone()),
                    );
                    map.insert(
                        "utilization_5h".into(),
                        serde_json::Value::String(info.utilization_5h.clone()),
                    );
                    map.insert(
                        "utilization_7d".into(),
                        serde_json::Value::String(info.utilization_7d.clone()),
                    );
                    map.insert(
                        "throttle_reset_at".into(),
                        serde_json::Value::String(info.throttle_reset_at.clone()),
                    );
                    map.insert(
                        "representative_claim".into(),
                        serde_json::Value::String(info.representative_claim.clone()),
                    );
                }
            }
            v
        })
        .collect();

    let strategy = broker.strategy().as_str();
    let (rotator_cursor, rotator_next_index) = match broker.rotator_cursor() {
        Some(cursor) if healthy_accounts > 0 => {
            (Some(cursor), Some(cursor.wrapping_rem(healthy_accounts)))
        }
        Some(cursor) => (Some(cursor), None),
        None => (None, None),
    };

    // Kill switch (full rollback): when COOPMUX_DISABLE_AUTO_ASSIGN is set the
    // auto-assign / profile-staging path is short-circuited (see http.rs), so
    // broker.session_assigned() is never called for new sessions. That makes
    // per-account session_count / cumulative_assignments stuck at 0 and the
    // Rebalance / Push controls effective no-ops. Surface the flag so the UI
    // can hide those misleading counts + controls. See ka-0000005557.
    let auto_assign_disabled = std::env::var("COOPMUX_DISABLE_AUTO_ASSIGN").is_ok();

    Json(serde_json::json!({
        "accounts": accounts,
        "total_sessions": total_sessions,
        "healthy_accounts": healthy_accounts,
        "total_accounts": pool.len(),
        "strategy": strategy,
        "rotator_cursor": rotator_cursor,
        "rotator_next_index": rotator_next_index,
        "auto_assign_disabled": auto_assign_disabled,
    }))
    .into_response()
}

/// Bounded per-request timeout for the profile-register / profile-switch POSTs
/// this operator-triggered rebalance pushes to each session's own coop pod. The
/// shared `reqwest::Client` (`state.http_client`) carries no default timeout and
/// `UpstreamClient::new` leaves `timeout: None`, so without this a push to a
/// hung/parked pod (TCP accepted but never responds — parked/half-open/node
/// under memory pressure) blocks forever. Because `credentials_pool_rebalance`
/// walks sessions serially, one such pod would (1) hang the operator
/// `POST /api/v1/credentials/pool/rebalance` indefinitely and (2) permanently
/// skip rebalancing every session after it. Matches the reassign path
/// (`REASSIGN_REQUEST_TIMEOUT`) and the distributor push (`PUSH_REQUEST_TIMEOUT`,
/// ka-0000002323). See ka-0000000721.
const REBALANCE_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

/// Request body for `POST /api/v1/credentials/pool/rebalance`.
#[derive(Debug, Deserialize)]
pub struct RebalanceRequest {
    /// Optional: only rebalance sessions from this account.
    #[serde(default)]
    pub from_account: Option<String>,
}

/// `POST /api/v1/credentials/pool/rebalance` — manually trigger credential rebalance.
pub async fn credentials_pool_rebalance(
    State(s): State<Arc<MuxState>>,
    Json(req): Json<RebalanceRequest>,
) -> impl IntoResponse {
    let broker = &s.credential_broker;

    // Collect sessions under the read lock, then drop before I/O.
    let entries: Vec<_> = {
        let sessions = s.sessions.read().await;
        sessions.values().cloned().collect()
    };

    // Bead-authoritative usable set: constrain the target selection so this
    // operator-triggered rebalance never moves a session onto an account the
    // controller has already marked exhausted/rejected (ka-0000006526). With no
    // bead health signal (no cache yet) every broker account is a candidate so
    // the endpoint still works. Either way a land's purse account is never a
    // target (the land wall).
    let candidates = land::fleet_rebalance_candidates(&s).await;
    let known_lands = land::known_land_accounts(&s).await;

    let mut reassigned = 0u32;
    let mut failed = 0u32;
    let mut skipped_land = 0u32;

    for entry in &entries {
        let current_account = entry.assigned_account.read().await.clone();

        if let Some(ref from) = req.from_account {
            if current_account.as_deref() != Some(from.as_str()) {
                continue;
            }
        }

        // Land wall: a land session stays on its own purse account; there is
        // no other account it may hold.
        if land::session_land(&entry.metadata, current_account.as_deref(), &known_lands).is_some() {
            skipped_land += 1;
            continue;
        }

        // Provider gate: pick only accounts matching this session's agent.
        let family = crate::credential::session_family(&entry.metadata);
        let new_account = broker.assign_account_for_project_family(None, &candidates, family).await;
        let Some(ref new_name) = new_account else { continue };

        if current_account.as_deref() == Some(new_name.as_str()) {
            continue;
        }

        // Get credentials for the target account so we can register the profile.
        let Some(new_creds) = broker.get_credentials(new_name).await else {
            tracing::warn!(
                session = %entry.id,
                account = %new_name,
                "pool: rebalance skipped — no credentials for target account"
            );
            failed += 1;
            continue;
        };

        if let Some(ref old) = current_account {
            broker.session_unassigned(old).await;
        }
        broker.session_assigned(new_name).await;
        *entry.assigned_account.write().await = Some(new_name.clone());

        let client = crate::upstream::client::UpstreamClient::with_timeout(
            s.http_client.clone(),
            entry.url.clone(),
            entry.auth_token.clone(),
            REBALANCE_REQUEST_TIMEOUT,
        );

        // Register the profile before switching — the session must know about
        // this account's credentials or the switch will fail with 400.
        let profile_body = serde_json::json!({
            "profiles": [{
                "name": new_name,
                "credentials": new_creds,
            }]
        });
        if let Err(e) = client.post_json("/api/v1/session/profiles", &profile_body).await {
            tracing::warn!(
                session = %entry.id,
                account = %new_name,
                err = %e,
                "pool: rebalance profile registration failed"
            );
            failed += 1;
            continue;
        }

        let switch_body = serde_json::json!({ "profile": new_name });
        match client.post_json("/api/v1/session/switch", &switch_body).await {
            Ok(_) => {
                tracing::info!(
                    session = %entry.id,
                    from = ?current_account,
                    to = %new_name,
                    "pool: rebalanced session"
                );
                reassigned += 1;
            }
            Err(e) => {
                tracing::warn!(
                    session = %entry.id,
                    to = %new_name,
                    err = %e,
                    "pool: rebalance switch failed"
                );
                failed += 1;
            }
        }
    }

    Json(serde_json::json!({
        "rebalanced": reassigned,
        "failed": failed,
        "skipped_land": skipped_land,
    }))
    .into_response()
}

#[cfg(test)]
#[path = "http_cred_tests.rs"]
mod tests;
