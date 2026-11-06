// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! HTTP handlers for the mux proxy.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use axum::extract::{Path, Query, State};
use axum::response::IntoResponse;
use axum::{Extension, Json};
use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::error::MuxError;
use crate::state::{epoch_ms, MuxEvent, MuxState, SessionEntry};
use crate::transport::land_principal::{self, LandPrincipal};
use crate::transport::project_scope::SessionScope;
use crate::upstream::client::UpstreamClient;

// -- Request/Response types ---------------------------------------------------

#[derive(Debug, Serialize)]
pub struct HealthResponse {
    pub status: String,
    pub session_count: usize,
    pub credentials_total: usize,
    pub credentials_healthy: usize,
    pub credentials_missing: usize,
}

#[derive(Debug, Deserialize)]
pub struct RegisterRequest {
    pub url: String,
    #[serde(default)]
    pub auth_token: Option<String>,
    #[serde(default)]
    pub id: Option<String>,
    #[serde(default)]
    pub metadata: Option<serde_json::Value>,
    /// Account name reported by the session (e.g. from BOAT_ACCOUNT).
    /// Used as fallback when no credential broker assigns one.
    #[serde(default)]
    pub account_name: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct RegisterResponse {
    pub id: String,
    pub registered: bool,
}

#[derive(Debug, Serialize)]
pub struct SessionInfo {
    pub id: String,
    pub url: String,
    pub metadata: serde_json::Value,
    pub registered_at_ms: u64,
    pub health_failures: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cached_state: Option<String>,
    /// The session's reconciled account, under the same wire name the
    /// WebSocket feed uses (`SessionSnapshot`/`ScreenThumbnail` in
    /// `ws_mux.rs`). Unified in ka-0000007897 — this key used to be `account`
    /// on REST only. The deprecated `account` duplicate was removed in
    /// ka-0000001779 after an in-repo sweep confirmed no readers of it remain.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub assigned_account: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub account_status: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub litellm_key_suffix: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub claimed_bead_title: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct DeregisterResponse {
    pub id: String,
    pub removed: bool,
}

#[derive(Debug, Serialize)]
pub struct LaunchConfigResponse {
    pub available: bool,
    pub cwd: String,
}

#[derive(Debug, Serialize)]
pub struct LaunchResponse {
    pub launched: bool,
}

#[derive(Debug, Deserialize)]
pub struct LaunchRequest {
    /// Optional environment variables to inject into the launched session.
    #[serde(default)]
    pub env: HashMap<String, String>,
}

// -- Helpers ------------------------------------------------------------------

/// Environment variable keys that are reserved by the system and cannot be
/// overridden by user-supplied env vars in launch requests.
/// Bounded per-request timeout for proxy/health POSTs and GETs to a session's
/// own coop pod. The shared `state.http_client` carries no default timeout, so
/// without this a request to a hung/parked pod (TCP accepted, never responds)
/// would block the handling task forever. Matches the distributor push path
/// (`PUSH_REQUEST_TIMEOUT`). See ka-0000002323.
const UPSTREAM_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

const RESERVED_ENV_KEYS: &[&str] = &[
    // Coop system vars
    "COOP_MUX_URL",
    "COOP_MUX_TOKEN",
    "COOP_URL",
    "COOP_SESSION_ID",
    // K8s vars (injected by downward API)
    "POD_NAME",
    "POD_NAMESPACE",
    "POD_IP",
    "POD_UID",
    // Credential vars (managed by broker)
    "ANTHROPIC_API_KEY",
    "CLAUDE_CODE_OAUTH_TOKEN",
    "OPENAI_API_KEY",
    "CODEX_AUTH_JSON",
    "GEMINI_API_KEY",
];

/// Filter out reserved env vars from user-supplied map.
fn filter_user_env(env: HashMap<String, String>) -> HashMap<String, String> {
    env.into_iter()
        .filter(|(k, _)| !RESERVED_ENV_KEYS.iter().any(|reserved| k == *reserved))
        .collect()
}

// -- Handlers -----------------------------------------------------------------

/// `GET /api/v1/version`
pub async fn version() -> impl IntoResponse {
    Json(serde_json::json!({ "version": env!("CARGO_PKG_VERSION") }))
}

/// `GET /metrics` — Prometheus text format. Currently exposes the
/// edge-auth verify counter (ka-0000007914 / W-B-11). Internal scrape
/// only; no auth (the metrics port is bound network-internal).
pub async fn metrics(State(s): State<Arc<MuxState>>) -> impl IntoResponse {
    let mut buf = String::new();
    s.edge_auth_metrics.render_prometheus(&mut buf);
    (
        axum::http::StatusCode::OK,
        [("content-type", "text/plain; version=0.0.4; charset=utf-8")],
        buf,
    )
}

/// `GET /api/v1/health`
pub async fn health(State(s): State<Arc<MuxState>>) -> impl IntoResponse {
    let sessions = s.sessions.read().await;
    let statuses = s.credential_broker.status_list().await;
    let total = statuses.len();
    let healthy = statuses.iter().filter(|a| a.has_api_key).count();
    Json(HealthResponse {
        status: "running".to_owned(),
        session_count: sessions.len(),
        credentials_total: total,
        credentials_healthy: healthy,
        credentials_missing: total - healthy,
    })
}

/// Refuse a land principal's registration. Every refusal answers alike so a
/// tenant cannot probe fleet session ids or urls; the reason is logged.
fn land_refusal(
    principal: &LandPrincipal,
    refusal: &land_principal::Refusal,
) -> axum::response::Response {
    tracing::warn!(land = %principal.land, ?refusal, "land registration refused");
    (
        axum::http::StatusCode::FORBIDDEN,
        Json(serde_json::json!({ "error": "land_registration_refused" })),
    )
        .into_response()
}

/// `POST /api/v1/sessions` — register a coop session.
///
/// A land principal's registration is bound to its land (gd-zVTBvaSIdG): the
/// metadata is stamped with the land, the only account it may report is the
/// land's, the coop at `url` must answer as that session, and it may neither
/// take nor evict a session outside its land. Other principals are fleet and
/// are trusted as before.
pub async fn register_session(
    State(s): State<Arc<MuxState>>,
    principal: Option<Extension<LandPrincipal>>,
    Json(req): Json<RegisterRequest>,
) -> impl IntoResponse {
    let principal = principal.map(|Extension(p)| p);
    let url = req.url.trim_end_matches('/').to_owned();
    let mut metadata = req.metadata;
    if let Some(ref p) = principal {
        match land_principal::bind_registration(
            p,
            req.id.as_deref(),
            metadata.take(),
            req.account_name.as_deref(),
        ) {
            Ok(bound) => metadata = Some(bound),
            Err(refusal) => return land_refusal(p, &refusal),
        }
    }

    // Validate upstream is reachable.
    let client = UpstreamClient::with_timeout(
        s.http_client.clone(),
        url.clone(),
        req.auth_token.clone(),
        UPSTREAM_REQUEST_TIMEOUT,
    );
    let health = match client.health().await {
        Ok(health) => health,
        Err(e) => {
            tracing::warn!(url = %url, err = %e, "upstream health check failed during registration");
            return MuxError::UpstreamError
                .to_http_response(format!("upstream unreachable: {e}"))
                .into_response();
        }
    };

    let id = req.id.unwrap_or_else(|| uuid::Uuid::new_v4().to_string());
    if let Some(ref p) = principal {
        if let Err(refusal) = land_principal::check_upstream_identity(p, &id, &health) {
            return land_refusal(p, &refusal);
        }
    }
    let metadata = metadata.unwrap_or(serde_json::Value::Null);
    let cancel = CancellationToken::new();

    // Clone values needed for the SessionOnline event and credential push before moving into entry.
    let event_url = url.clone();
    let event_metadata = metadata.clone();
    let cred_url = url.clone();
    let cred_token = req.auth_token.clone();

    // Seed assigned_account from the registration request (e.g. BOAT_ACCOUNT).
    // When the session already has valid credentials (reported via account_name),
    // the pool assignment will prefer this account and skip the session switch
    // to avoid an unnecessary Claude Code restart.
    let initial_account = req.account_name.filter(|s| !s.is_empty());

    let entry = Arc::new(SessionEntry {
        id: id.clone(),
        url,
        auth_token: req.auth_token,
        metadata,
        registered_at: std::time::Instant::now(),
        cached_screen: tokio::sync::RwLock::new(None),
        cached_status: tokio::sync::RwLock::new(None),
        cached_usage: tokio::sync::RwLock::new(None),
        health_failures: std::sync::atomic::AtomicU32::new(0),
        cancel,
        ws_bridge: tokio::sync::RwLock::new(None),
        assigned_account: tokio::sync::RwLock::new(initial_account.clone()),
        claimed_bead_title: tokio::sync::RwLock::new(None),
        transport: crate::state::SessionTransport::default(),
    });

    let (is_new, stale, evicted_entries) = {
        let mut sessions = s.sessions.write().await;
        // A land principal may neither re-register a session outside its land
        // nor evict one that shares its url. Checked under the write lock so
        // no fleet registration can slip in between check and insert.
        if let Some(ref p) = principal {
            let scope = p.scope();
            let refusal =
                if sessions.get(&id).is_some_and(|e| !scope.allows_metadata(&e.metadata)) {
                    Some(land_principal::Refusal::IdHeld)
                } else if sessions.values().any(|e| {
                    e.id != id && e.url == entry.url && !scope.allows_metadata(&e.metadata)
                }) {
                    Some(land_principal::Refusal::UrlHeld)
                } else {
                    None
                };
            if let Some(refusal) = refusal {
                drop(sessions);
                return land_refusal(p, &refusal);
            }
        }
        if let Some(existing) = sessions.get(&id) {
            // Heartbeat re-registration: keep the existing entry so that
            // pollers/feeds (which hold Arc clones of the old entry) continue
            // writing to the same cached_screen/cached_status that screen_batch
            // reads.  Replacing the entry would orphan their writes.
            //
            // The new request's account_name would otherwise be dropped here.
            // Adopt it ONLY when the existing entry has no account yet, so a
            // heartbeat that carries an account (e.g. coop reporting its seeded
            // active_name, or gb's BOAT_ACCOUNT) fills a null assigned_account
            // even when auto-assign is disabled and the first health reconcile
            // has not run. We never overwrite a non-null value: the health
            // poller (reconcile from the session's reported account_name) is the
            // ongoing source of truth and may already hold the live account after
            // a switch. See ka-0000002509.
            if let Some(ref acct) = initial_account {
                let mut cur = existing.assigned_account.write().await;
                if cur.is_none() {
                    tracing::debug!(
                        session_id = %id,
                        account = %acct,
                        "adopting account from heartbeat re-registration (was unset)"
                    );
                    *cur = Some(acct.clone());
                }
            }
            (false, vec![], vec![])
        } else {
            // Evict any stale session(s) pointing to the same URL (e.g. after a
            // pod restart generated a new session UUID for the same coop instance).
            let stale: Vec<String> = sessions
                .iter()
                .filter(|(k, v)| *k != &id && v.url == entry.url)
                .map(|(k, _)| k.clone())
                .collect();
            let mut evicted = Vec::new();
            for stale_id in &stale {
                if let Some(old) = sessions.remove(stale_id) {
                    old.cancel.cancel();
                    let _ = s
                        .feed
                        .event_tx
                        .send(MuxEvent::SessionOffline { session: stale_id.clone() });
                    tracing::info!(
                        old_session = %stale_id,
                        new_session = %id,
                        url = %entry.url,
                        "evicted stale session with same URL"
                    );
                    evicted.push(old);
                }
            }
            sessions.insert(id.clone(), Arc::clone(&entry));
            (true, stale, evicted)
        }
    };
    // Unassign evicted sessions from the credential pool.
    for evicted_entry in &evicted_entries {
        if let Some(account) = evicted_entry.assigned_account.read().await.as_ref() {
            s.credential_broker.session_unassigned(account).await;
        }
    }
    // Clean up watchers for any evicted sessions (separate lock to avoid
    // holding sessions + watchers simultaneously).
    // Note: evicted sessions already had their cancel tokens triggered above,
    // so their pollers/feeds will stop. This just cleans the watchers map.
    if is_new {
        // Clean up watchers + prewarm for evicted stale sessions.
        if !stale.is_empty() {
            let mut watchers = s.feed.watchers.write().await;
            for stale_id in &stale {
                if let Some(ws) = watchers.remove(stale_id) {
                    ws.poller_cancel.cancel();
                }
            }
            drop(watchers);
            let mut prewarm = s.prewarm.lock().await;
            for stale_id in &stale {
                prewarm.remove(stale_id);
            }
        }

        // Add to pre-warm cache for slow-poll background updates.
        s.prewarm.lock().await.touch(&id);

        // Clone metadata for credential filtering before moving into the event.
        let cred_metadata = event_metadata.clone();

        // Notify connected dashboard clients about the new session.
        let _ = s.feed.event_tx.send(MuxEvent::SessionOnline {
            session: id.clone(),
            url: event_url,
            metadata: event_metadata,
        });

        // Always start the event feed for new sessions so the throttle watcher
        // receives transitions even when no dashboard is open.
        if !matches!(entry.transport, crate::state::SessionTransport::Nats { .. }) {
            let feed_cancel = entry.cancel.child_token();
            crate::upstream::feed::spawn_event_feed(
                s.feed.event_tx.clone(),
                Arc::clone(&entry),
                feed_cancel,
            );
        }

        // Stage fallback credential profiles on the new session so its
        // in-session rotation (coop's `try_auto_rotate`) has somewhere to go
        // when the booted account hits a rate limit.
        //
        // IMPORTANT (ka-0000005014 / ka-0000007230): coopmux NO LONGER issues a
        // registration-time `/api/v1/session/switch`. That switch was the cause
        // of the respawn loop: a freshly-spawned coop reports `account_name =
        // active_name()` = None (its profile list is empty until coopmux pushes),
        // so `already_has_creds` was always false and every new pod got switched
        // → coop exited with the credential-switch code → gb init restarted it →
        // it re-registered as None → loop. The agent already booted with valid
        // env-injected credentials (ANTHROPIC_BASE_URL + ANTHROPIC_CUSTOM_HEADERS
        // from resolve-auth.sh); coopmux's job here is only to *add* healthy
        // alternatives, never to swap the live one out at registration.
        //
        // Recovery from rate limits is now driven by coop itself: a 429 →
        // `handle_rate_limit` → `try_auto_rotate` picks one of these staged
        // profiles and performs a graceful in-session switch (restart only the
        // claude child with --resume, work preserved).
        //
        // Profile pushes keep the hq-uc5sup retry/backoff so a slow-to-bind new
        // pod still receives its fallbacks.
        {
            let broker = Arc::clone(&s.credential_broker);
            let session_url = cred_url;
            let session_token = cred_token;
            let session_metadata = cred_metadata;
            let entry_clone = Arc::clone(&entry);
            let reported_account = initial_account.clone();
            let state_ref = Arc::clone(&s);
            tokio::spawn(async move {
                // Kill switch (full rollback). When COOPMUX_DISABLE_AUTO_ASSIGN
                // is set, sessions register and appear in the dashboard but
                // receive NOTHING from coopmux — no profile staging, no switch.
                // Agents run on exactly the credentials the controller injected
                // via env at spawn, with no in-session rotation available. This
                // is the safe revert if staging ever misbehaves; the default
                // (unset) is now to stage fallback profiles so rotation works.
                if std::env::var("COOPMUX_DISABLE_AUTO_ASSIGN").is_ok() {
                    tracing::info!(
                        "pool: COOPMUX_DISABLE_AUTO_ASSIGN set — skipping profile staging for new session"
                    );
                    return;
                }

                // Determine the project authorization set + the account this
                // session booted on. We use these to scope which fallback
                // profiles to stage, NOT to switch the session.
                //
                // Role-pin handling (ka-0000002136): a session's `metadata.role`
                // may pin an account via a `type=role` bead; that pinned account
                // is union'd into the project's authorized set so it is eligible
                // for staging alongside the project's own fallback chain.
                let project = entry_clone
                    .metadata
                    .get("project")
                    .and_then(|p| p.as_str())
                    .map(|s| s.to_owned());
                let role =
                    entry_clone.metadata.get("role").and_then(|r| r.as_str()).map(|s| s.to_owned());

                // Land wall: a land session may stage only its own purse
                // account, and a fleet session never stages a land's. Computed
                // before the cache lock below (it takes that lock itself).
                let known_lands = crate::credential::land::known_land_accounts(&state_ref).await;
                let land = crate::credential::land::session_land(
                    &session_metadata,
                    reported_account.as_deref(),
                    &known_lands,
                );

                // Compute the authorized account set for this session (project
                // fallback chain ∪ role pin), and snapshot per-account health
                // from the controller-authored account cache so we only stage
                // accounts that are safe to rotate onto.
                let (authorized, usable) = {
                    let cache = state_ref.account_cache.read().await;
                    // A land session takes no role pin (a steward pin would
                    // hand it a fleet profile), nor does a fleet session take
                    // a pin onto a land account.
                    let role_pinned_account =
                        role.as_deref().and_then(|r| cache.account_for_role(r)).filter(|pin| {
                            land.is_none() && crate::credential::land::land_wall_allows(None, pin)
                        });
                    let authorized = match (land.as_deref(), project.as_deref()) {
                        (Some(land), _) => Some(std::collections::HashSet::from([
                            crate::credential::land::land_account_name(land),
                        ])),
                        (None, Some(proj)) => {
                            let mut set = cache.authorized_accounts_for_project(proj);
                            if let Some(ref pin) = role_pinned_account {
                                set.insert(pin.clone());
                            }
                            if set.is_empty() {
                                None
                            } else {
                                Some(set)
                            }
                        }
                        (None, None) => role_pinned_account
                            .clone()
                            .map(|pin| std::collections::HashSet::from([pin])),
                    };
                    if let Some(ref pin) = role_pinned_account {
                        tracing::info!(
                            role = role.as_deref().unwrap_or("?"),
                            project = project.as_deref().unwrap_or("?"),
                            role_account = %pin,
                            "pool: role-account pin eligible for staging on new session"
                        );
                    }
                    // Build the set of accounts that are usable as rotation
                    // targets per the bead-authoritative health rubric. Used
                    // below to filter the staged pool. The session's own booted
                    // account is exempt — it already holds those env creds, so
                    // we always keep its profile slot even if it just went
                    // unhealthy (so the slot exists for switchback).
                    let usable: std::collections::HashSet<String> = cache
                        .all_accounts()
                        .keys()
                        .filter(|name| cache.account_usable(name))
                        .cloned()
                        .collect();
                    (authorized, usable)
                };

                // Collect account profiles into a single batch, then push once.
                // Coop's register() upserts profiles additively (ka-0000005975),
                // so staging never moves the session's Active profile or clobbers
                // its reported account_name — it only adds Available fallbacks.
                //
                // Filtering rules:
                //   - land sessions: only the land's own purse account;
                //   - project-scoped sessions: only authorized accounts;
                //   - global (no project/role): all accounts;
                //   - fleet sessions never stage a land's purse account;
                //   - skip accounts with no API key (Missing);
                //   - skip accounts that are NOT usable rotation targets
                //     (disabled / exhausted+rejected) per the account cache —
                //     EXCEPT the session's own booted account, whose slot we
                //     always keep so switchback is possible once it recovers;
                //   - honor per-pod `profiles_needed`.
                let status_list = broker.status_list().await;
                let mut profile_entries = Vec::new();
                let is_project_scoped = authorized.is_some();
                let booted_account = reported_account.as_deref();
                for acct in &status_list {
                    // The session's own booted account is ALWAYS staged (when it
                    // has credentials), bypassing the project-authorization,
                    // health, and profiles_needed filters below. The session is
                    // already running on this account's env-injected credentials;
                    // staging its profile keeps coop's Active profile aligned
                    // with its live creds and gives it a slot to switch back to
                    // once it recovers. Without this short-circuit a booted
                    // account that is outside its project's authorized set, or
                    // currently exhausted, would be dropped and an arbitrary
                    // fallback would become coop's Active profile. (ka-0000005014)
                    let is_booted = booted_account == Some(acct.name.as_str());

                    if !is_booted {
                        if !crate::credential::land::land_wall_allows(land.as_deref(), &acct.name) {
                            tracing::debug!(
                                account = %acct.name,
                                land = land.as_deref().unwrap_or(""),
                                "pool: skipping account behind the land wall for new-session staging"
                            );
                            continue;
                        }
                        if is_project_scoped {
                            if let Some(ref auth_set) = authorized {
                                if !auth_set.contains(&acct.name) {
                                    continue;
                                }
                            }
                        }
                        // Health gate: never stage an account that is not a safe
                        // rotation target.
                        if !usable.contains(&acct.name) {
                            tracing::debug!(
                                account = %acct.name,
                                "pool: skipping unusable account for new-session staging (disabled/exhausted)"
                            );
                            continue;
                        }
                        // Apply per-pod filtering: skip accounts this session doesn't need.
                        if !crate::credential::distributor::session_needs_account_metadata(
                            &session_metadata,
                            &acct.name,
                        ) {
                            tracing::debug!(
                                account = %acct.name,
                                "skipping profile push to new session (not in profiles_needed)"
                            );
                            continue;
                        }
                    }
                    // Provider gate: a Claude session never stages a Codex
                    // profile, nor the reverse. Applies to every account,
                    // including the booted one and `profiles_needed: ["*"]`.
                    if !crate::credential::provider_compatible(
                        crate::credential::session_family(&session_metadata),
                        &acct.provider,
                    ) {
                        tracing::debug!(
                            account = %acct.name,
                            provider = %acct.provider,
                            "skipping profile push to new session (provider does not match its agent)"
                        );
                        continue;
                    }
                    // Skip accounts with no API key (Missing status) — including
                    // the booted account if the broker somehow has no creds for
                    // it (nothing to stage).
                    if !acct.has_api_key {
                        continue;
                    }
                    let Some(credentials) = broker.get_credentials(&acct.name).await else {
                        continue;
                    };
                    // Stamp the bead-authoritative health flag (ka-0000006430) so
                    // coop's in-session `try_auto_rotate` skips a condemned
                    // profile. Non-booted accounts staged here all passed the
                    // `usable` gate above, so they carry `true`; the booted
                    // account is exempt from that gate and carries its real
                    // health (it is the Active profile — never a rotation
                    // candidate — but stamping the true value keeps the flag
                    // honest for a later switchback).
                    let is_usable = usable.contains(&acct.name);
                    profile_entries.push(serde_json::json!({
                        "name": acct.name,
                        "credentials": credentials,
                        "usable": is_usable,
                    }));
                }

                if profile_entries.is_empty() {
                    tracing::debug!(
                        project = project.as_deref().unwrap_or("?"),
                        "pool: no fallback profiles to stage for new session"
                    );
                    return;
                }

                // Order the booted account FIRST in the batch. coop's
                // ProfileState::register seeds `profiles.first()` as Active when
                // the profile set was previously empty (a fresh session). Since
                // status_list() iterates a HashMap (arbitrary order), without
                // this an arbitrary fallback could become the session's Active
                // profile → coop would report the wrong account_name → the
                // coopmux health poller would reconcile assigned_account to it →
                // a later credential refresh would fire an unrequested
                // /session/switch (the very thing we removed at registration).
                // Putting the booted account first keeps coop's reported account
                // aligned with its live env credentials. (ka-0000005014)
                if let Some(booted) = booted_account {
                    if let Some(pos) = profile_entries
                        .iter()
                        .position(|e| e.get("name").and_then(|n| n.as_str()) == Some(booted))
                    {
                        if pos != 0 {
                            profile_entries.swap(0, pos);
                        }
                    }
                }

                // Push the batch with retries (hq-uc5sup resilience). NO switch
                // follows — the session keeps its env-injected credentials and
                // self-rotates onto one of these staged profiles only when it
                // actually hits a rate limit.
                let staged = profile_entries.len();
                let client = UpstreamClient::with_timeout(
                    state_ref.http_client.clone(),
                    session_url.clone(),
                    session_token.clone(),
                    UPSTREAM_REQUEST_TIMEOUT,
                );
                let profile_body = serde_json::json!({ "profiles": profile_entries });
                let mut backoff = std::time::Duration::from_millis(500);
                let max_retries = 3u32;
                for attempt in 0..=max_retries {
                    match client.post_json("/api/v1/session/profiles", &profile_body).await {
                        Ok(_) => {
                            tracing::info!(
                                staged,
                                project = project.as_deref().unwrap_or("?"),
                                "pool: staged fallback profiles on new session (no switch)"
                            );
                            break;
                        }
                        Err(e) => {
                            if attempt == max_retries {
                                tracing::warn!(
                                    count = staged,
                                    attempt,
                                    err = %e,
                                    "pool: failed to stage profiles on new session after retries"
                                );
                            } else {
                                tracing::debug!(
                                    attempt,
                                    err = %e,
                                    "pool: profile staging on new session failed, retrying"
                                );
                                tokio::time::sleep(backoff).await;
                                backoff = (backoff * 2).min(std::time::Duration::from_secs(5));
                            }
                        }
                    }
                }
            });
        }

        tracing::info!(session_id = %id, "session registered");
    } else {
        tracing::debug!(session_id = %id, "session re-registered (heartbeat)");
    }

    Json(RegisterResponse { id, registered: true }).into_response()
}

/// `DELETE /api/v1/sessions/{id}` — deregister a session.
pub async fn deregister_session(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
) -> impl IntoResponse {
    if let Some(entry) = s.remove_session(&id).await {
        // Unassign from the credential pool.
        if let Some(account) = entry.assigned_account.read().await.as_ref() {
            s.credential_broker.session_unassigned(account).await;
        }
        tracing::info!(session_id = %id, "session deregistered");
        Json(DeregisterResponse { id, removed: true }).into_response()
    } else {
        MuxError::SessionNotFound.to_http_response("session not found").into_response()
    }
}

/// `GET /api/v1/sessions` — list all registered sessions.
///
/// A land-scoped OIDC caller (gd-DcKLFjZuyl) sees only its lands' sessions,
/// without the credential-pool fields (account, its status, key suffix), which
/// are operator data.
pub async fn list_sessions(
    State(s): State<Arc<MuxState>>,
    scope: Option<Extension<SessionScope>>,
) -> impl IntoResponse {
    let scope = scope.map_or(SessionScope::Unrestricted, |Extension(scope)| scope);
    let sessions = s.sessions.read().await;
    let mut list = Vec::with_capacity(sessions.len());
    for entry in sessions.values() {
        if !scope.allows_metadata(&entry.metadata) {
            continue;
        }
        let cached_state = entry.cached_status.read().await.as_ref().map(|st| st.state.clone());
        let registered_at_ms =
            epoch_ms().saturating_sub(entry.registered_at.elapsed().as_millis() as u64);
        let account =
            if scope.is_scoped() { None } else { entry.assigned_account.read().await.clone() };
        let (account_status, litellm_key_suffix) = match &account {
            Some(acct) => (
                Some(s.credential_broker.account_status(acct).await),
                s.credential_broker.key_suffix(acct).await,
            ),
            None => (None, None),
        };
        let claimed_bead_title = entry.claimed_bead_title.read().await.clone();
        list.push(SessionInfo {
            id: entry.id.clone(),
            url: entry.url.clone(),
            metadata: entry.metadata.clone(),
            registered_at_ms,
            health_failures: entry.health_failures.load(std::sync::atomic::Ordering::Relaxed),
            cached_state,
            assigned_account: account,
            account_status,
            litellm_key_suffix,
            claimed_bead_title,
        });
    }
    Json(list)
}

/// `GET /api/v1/sessions/{id}/screen` — cached screen snapshot.
pub async fn session_screen(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
) -> impl IntoResponse {
    let sessions = s.sessions.read().await;
    let entry = match sessions.get(&id) {
        Some(e) => Arc::clone(e),
        None => {
            return MuxError::SessionNotFound.to_http_response("session not found").into_response()
        }
    };
    drop(sessions);

    let cached = entry.cached_screen.read().await;
    match cached.as_ref() {
        Some(screen) => Json(screen.clone()).into_response(),
        None => MuxError::UpstreamError.to_http_response("screen not yet cached").into_response(),
    }
}

/// `GET /api/v1/sessions/{id}/status` — cached status.
pub async fn session_status(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
) -> impl IntoResponse {
    let sessions = s.sessions.read().await;
    let entry = match sessions.get(&id) {
        Some(e) => Arc::clone(e),
        None => {
            return MuxError::SessionNotFound.to_http_response("session not found").into_response()
        }
    };
    drop(sessions);

    let cached = entry.cached_status.read().await;
    match cached.as_ref() {
        Some(status) => Json(status.clone()).into_response(),
        None => MuxError::UpstreamError.to_http_response("status not yet cached").into_response(),
    }
}

/// `GET /api/v1/sessions/{id}/agent` — proxy to upstream agent endpoint.
pub async fn session_agent(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
) -> impl IntoResponse {
    let sessions = s.sessions.read().await;
    let entry = match sessions.get(&id) {
        Some(e) => Arc::clone(e),
        None => {
            return MuxError::SessionNotFound.to_http_response("session not found").into_response()
        }
    };
    drop(sessions);

    let client = UpstreamClient::with_timeout(
        s.http_client.clone(),
        entry.url.clone(),
        entry.auth_token.clone(),
        UPSTREAM_REQUEST_TIMEOUT,
    );
    match client.get_agent().await {
        Ok(value) => Json(value).into_response(),
        Err(e) => {
            MuxError::UpstreamError.to_http_response(format!("upstream error: {e}")).into_response()
        }
    }
}

/// `POST /api/v1/sessions/{id}/agent/nudge` — proxy nudge to upstream agent.
pub async fn session_agent_nudge(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    proxy_post(&s, &id, "/api/v1/agent/nudge", body).await
}

/// `POST /api/v1/sessions/{id}/agent/respond` — proxy respond to upstream agent.
pub async fn session_agent_respond(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    proxy_post(&s, &id, "/api/v1/agent/respond", body).await
}

/// `POST /api/v1/sessions/{id}/input` — proxy input to upstream.
pub async fn session_input(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    proxy_post(&s, &id, "/api/v1/input", body).await
}

/// `POST /api/v1/sessions/{id}/input/raw` — proxy raw input to upstream.
pub async fn session_input_raw(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    proxy_post(&s, &id, "/api/v1/input/raw", body).await
}

/// `POST /api/v1/sessions/{id}/input/keys` — proxy keys to upstream.
pub async fn session_input_keys(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    proxy_post(&s, &id, "/api/v1/input/keys", body).await
}

/// `POST /api/v1/sessions/{id}/input/mouse` — proxy one mouse press or release
/// to upstream, which encodes it as the app asked (never raw bytes).
pub async fn session_input_mouse(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    proxy_post(&s, &id, "/api/v1/input/mouse", body).await
}

/// `POST /api/v1/sessions/{id}/resize` — proxy a terminal resize to upstream
/// (`{"cols": .., "rows": ..}`): the same resize `/ws/mux` and the per-session
/// socket carry as a frame, for a caller that holds no socket (the garden's
/// Glass, when the person attached fits the terminal to their screen).
pub async fn session_resize(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    proxy_post(&s, &id, "/api/v1/resize", body).await
}

/// `POST /api/v1/sessions/{id}/upload` — proxy file upload to upstream.
pub async fn session_upload(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    proxy_post(&s, &id, "/api/v1/upload", body).await
}

// -- Proxy GET endpoints (output, transcripts, recording) ---------------------

/// `GET /api/v1/sessions/{id}/output` — proxy raw output from upstream.
pub async fn session_output(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Query(params): Query<HashMap<String, String>>,
) -> impl IntoResponse {
    proxy_get(&s, &id, "/api/v1/output", &params).await
}

/// `GET /api/v1/sessions/{id}/transcripts` — proxy transcript list from upstream.
pub async fn session_transcripts(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
) -> impl IntoResponse {
    proxy_get(&s, &id, "/api/v1/transcripts", &HashMap::new()).await
}

/// `GET /api/v1/sessions/{id}/transcripts/catchup` — proxy transcript catchup.
pub async fn session_transcripts_catchup(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Query(params): Query<HashMap<String, String>>,
) -> impl IntoResponse {
    proxy_get(&s, &id, "/api/v1/transcripts/catchup", &params).await
}

/// `GET /api/v1/sessions/{id}/transcripts/{number}` — proxy single transcript.
pub async fn session_transcript_by_number(
    State(s): State<Arc<MuxState>>,
    Path((id, number)): Path<(String, String)>,
) -> impl IntoResponse {
    let path = format!("/api/v1/transcripts/{number}");
    proxy_get(&s, &id, &path, &HashMap::new()).await
}

/// `GET /api/v1/sessions/{id}/recording` — proxy recording status from upstream.
pub async fn session_recording(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
) -> impl IntoResponse {
    proxy_get(&s, &id, "/api/v1/recording", &HashMap::new()).await
}

/// `GET /api/v1/sessions/{id}/recording/catchup` — proxy recording catchup.
pub async fn session_recording_catchup(
    State(s): State<Arc<MuxState>>,
    Path(id): Path<String>,
    Query(params): Query<HashMap<String, String>>,
) -> impl IntoResponse {
    proxy_get(&s, &id, "/api/v1/recording/catchup", &params).await
}

/// `GET /api/v1/config/launch` — whether launch is available.
pub async fn launch_config(State(s): State<Arc<MuxState>>) -> impl IntoResponse {
    let cwd = std::env::current_dir().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();
    Json(LaunchConfigResponse { available: s.config.launch.is_some(), cwd })
}

/// `POST /api/v1/sessions/launch` — spawn a new session via the configured launch command.
///
/// Accepts optional JSON body with environment variables:
/// ```json
/// {
///   "env": {
///     "GIT_REPO": "https://github.com/user/repo",
///     "WORKING_DIR": "/workspace/project",
///     "GIT_BRANCH": "feature-branch"
///   }
/// }
/// ```
pub async fn launch_session(
    State(s): State<Arc<MuxState>>,
    body: Option<Json<LaunchRequest>>,
) -> impl IntoResponse {
    let launch = match &s.config.launch {
        Some(cmd) => cmd.clone(),
        None => {
            return MuxError::BadRequest
                .to_http_response("launch command not configured")
                .into_response()
        }
    };

    let mux_url = format!("http://{}:{}", s.config.host, s.config.port);

    let mut cmd = tokio::process::Command::new("sh");
    cmd.args(["-c", &launch]);

    // 1. First, inject user-supplied env vars (if any), filtered for safety.
    if let Some(Json(req)) = body {
        let filtered = filter_user_env(req.env);
        for (key, value) in filtered {
            cmd.env(key, value);
        }
    }

    // 2. Then inject system vars (can override user vars if needed).
    cmd.env("COOP_MUX_URL", &mux_url);
    if let Some(token) = &s.config.auth_token {
        cmd.env("COOP_MUX_TOKEN", token);
    }

    // 3. Finally inject credentials (highest priority — cannot be overridden).
    {
        let broker = &s.credential_broker;
        let status_list = broker.status_list().await;
        for acct in &status_list {
            if acct.status != crate::credential::AccountStatus::Healthy {
                continue;
            }
            if let Some(credentials) = broker.get_credentials(&acct.name).await {
                for (key, value) in &credentials {
                    cmd.env(key, value);
                }
            }
        }
    }

    cmd.stdin(std::process::Stdio::null());
    cmd.stdout(std::process::Stdio::inherit());
    cmd.stderr(std::process::Stdio::inherit());
    // Detach into a new process group so launched sessions survive mux restart.
    cmd.process_group(0);

    match cmd.spawn() {
        Ok(_child) => Json(LaunchResponse { launched: true }).into_response(),
        Err(e) => {
            tracing::error!(err = %e, "failed to spawn launch command");
            MuxError::Internal.to_http_response(format!("failed to spawn: {e}")).into_response()
        }
    }
}

/// Generic POST proxy to upstream coop.
///
/// For NATS-transport sessions, routes input/nudge/respond through NATS
/// instead of HTTP. Other paths fall through to HTTP (which may fail if
/// the session is only reachable via NATS).
async fn proxy_post(
    state: &MuxState,
    session_id: &str,
    path: &str,
    body: serde_json::Value,
) -> axum::response::Response {
    let sessions = state.sessions.read().await;
    let entry = match sessions.get(session_id) {
        Some(e) => Arc::clone(e),
        None => {
            return MuxError::SessionNotFound.to_http_response("session not found").into_response()
        }
    };
    drop(sessions);

    // For NATS-transport sessions, route input/nudge/respond via NATS.
    if let crate::state::SessionTransport::Nats { ref prefix } = entry.transport {
        let nats_subject = match path {
            "/api/v1/input" | "/api/v1/input/raw" | "/api/v1/input/keys" => {
                Some(format!("{prefix}.session.{session_id}.input"))
            }
            "/api/v1/agent/nudge" => Some(format!("{prefix}.session.{session_id}.nudge")),
            "/api/v1/agent/respond" => Some(format!("{prefix}.session.{session_id}.respond")),
            _ => None,
        };

        if let Some(subject) = nats_subject {
            let client_guard = state.nats_client.read().await;
            if let Some(ref nats_client) = *client_guard {
                let payload = serde_json::to_vec(&body).unwrap_or_default();
                if let Err(e) = nats_client.publish(subject, payload.into()).await {
                    return MuxError::UpstreamError
                        .to_http_response(format!("nats publish error: {e}"))
                        .into_response();
                }
                return Json(serde_json::json!({"ok": true})).into_response();
            }
            return MuxError::UpstreamError
                .to_http_response("nats client not available")
                .into_response();
        }
    }

    let client = UpstreamClient::with_timeout(
        state.http_client.clone(),
        entry.url.clone(),
        entry.auth_token.clone(),
        UPSTREAM_REQUEST_TIMEOUT,
    );
    match client.post_json(path, &body).await {
        Ok(value) => Json(value).into_response(),
        Err(e) => {
            MuxError::UpstreamError.to_http_response(format!("upstream error: {e}")).into_response()
        }
    }
}

/// Generic GET proxy to upstream coop.
async fn proxy_get(
    state: &MuxState,
    session_id: &str,
    path: &str,
    params: &HashMap<String, String>,
) -> axum::response::Response {
    let sessions = state.sessions.read().await;
    let entry = match sessions.get(session_id) {
        Some(e) => Arc::clone(e),
        None => {
            return MuxError::SessionNotFound.to_http_response("session not found").into_response()
        }
    };
    drop(sessions);

    // Build upstream path with query parameters.
    let upstream_path = if params.is_empty() {
        path.to_owned()
    } else {
        let qs: Vec<String> = params.iter().map(|(k, v)| format!("{k}={v}")).collect();
        format!("{path}?{}", qs.join("&"))
    };

    let client = UpstreamClient::with_timeout(
        state.http_client.clone(),
        entry.url.clone(),
        entry.auth_token.clone(),
        UPSTREAM_REQUEST_TIMEOUT,
    );
    match client.get_json(&upstream_path).await {
        Ok(value) => Json(value).into_response(),
        Err(e) => {
            MuxError::UpstreamError.to_http_response(format!("upstream error: {e}")).into_response()
        }
    }
}

#[cfg(test)]
#[path = "http_tests.rs"]
mod tests;
