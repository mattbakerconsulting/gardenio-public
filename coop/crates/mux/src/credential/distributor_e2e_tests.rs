// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// End-to-end distributor tests that drive `distribute_to_sessions` and
// `rebalance_from_account` against a mock upstream coop server, exercising the
// profile-register + status-check + profile-switch codepaths.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;

use axum::routing::{get, post};
use axum::{Json, Router};
use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;

use super::*;
use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{AccountConfig, CredentialConfig, CredentialEvent};
use crate::state::{MuxState, SessionEntry, SessionTransport};

pub(super) fn test_config() -> MuxConfig {
    MuxConfig {
        host: "127.0.0.1".into(),
        port: 0,
        auth_token: None,
        oidc_issuer_url: None,
        oidc_client_id: None,
        screen_poll_ms: 500,
        status_poll_ms: 2000,
        health_check_ms: 10000,
        max_health_failures: 3,
        launch: None,
        credential_config: None,
        api_key_file: None,
        prewarm_capacity: 64,
        prewarm_poll_ms: 15000,
        state_dir: None,
        dev_mode: false,
        base_path: None,
        #[cfg(debug_assertions)]
        hot: false,
    }
}

pub(super) fn account(name: &str) -> AccountConfig {
    AccountConfig {
        name: name.to_owned(),
        provider: "claude".to_owned(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }
}

pub(super) async fn state_with_accounts(names: &[&str]) -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let configs: Vec<AccountConfig> = names.iter().map(|n| account(n)).collect();
    let broker = CredentialBroker::new(CredentialConfig { accounts: configs }, event_tx, None);
    for n in names {
        broker.set_token(n, format!("tok-{n}"), None, None).await.expect("seed token");
    }
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), broker))
}

pub(super) fn entry(
    id: &str,
    url: String,
    assigned: Option<&str>,
    metadata: serde_json::Value,
) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url,
        auth_token: None,
        metadata,
        registered_at: std::time::Instant::now(),
        cached_screen: RwLock::new(None),
        cached_status: RwLock::new(None),
        cached_usage: RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: RwLock::new(None),
        assigned_account: RwLock::new(assigned.map(str::to_owned)),
        claimed_bead_title: RwLock::new(None),
        transport: SessionTransport::default(),
    })
}

pub(super) struct Hits {
    pub(super) profiles: Arc<AtomicU32>,
    pub(super) switch: Arc<AtomicU32>,
    /// Body of the most recent `/api/v1/session/profiles` push, for asserting
    /// the stamped `usable` flag (ka-0000006430).
    last_profile_body: Arc<RwLock<Option<serde_json::Value>>>,
}

/// Mock upstream coop that accepts profile registration + switch, and reports
/// the given agent state on /api/v1/status.
pub(super) async fn mock_upstream(state_str: &'static str) -> anyhow::Result<(String, Hits)> {
    let profiles = Arc::new(AtomicU32::new(0));
    let switch = Arc::new(AtomicU32::new(0));
    let last_profile_body = Arc::new(RwLock::new(None));

    let p = Arc::clone(&profiles);
    let s = Arc::clone(&switch);
    let body_sink = Arc::clone(&last_profile_body);
    let app = Router::new()
        .route(
            "/api/v1/session/profiles",
            post(move |Json(body): Json<serde_json::Value>| {
                let p = Arc::clone(&p);
                let body_sink = Arc::clone(&body_sink);
                async move {
                    p.fetch_add(1, Ordering::SeqCst);
                    *body_sink.write().await = Some(body);
                    Json(serde_json::json!({"ok": true}))
                }
            }),
        )
        .route(
            "/api/v1/session/switch",
            post(move || {
                let s = Arc::clone(&s);
                async move {
                    s.fetch_add(1, Ordering::SeqCst);
                    Json(serde_json::json!({"ok": true}))
                }
            }),
        )
        .route(
            "/api/v1/status",
            get(move || async move { Json(serde_json::json!({ "state": state_str })) }),
        );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok((format!("http://{addr}"), Hits { profiles, switch, last_profile_body }))
}

/// Extract the `usable` flag of the single pushed profile from a captured body.
/// Returns `None` if the field is absent (old-coopmux-compatible push).
fn pushed_usable(body: &serde_json::Value) -> Option<bool> {
    body.get("profiles")?.as_array()?.first()?.get("usable")?.as_bool()
}

fn creds() -> HashMap<String, String> {
    HashMap::from([("CLAUDE_CODE_OAUTH_TOKEN".to_owned(), "tok-acct-a".to_owned())])
}

#[tokio::test]
async fn distribute_registers_profile_and_switches_idle_session() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a"]).await;
    // The session is assigned to acct-a → should_switch is true → switch fires.
    let e = entry("s1", url, Some("acct-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    distribute_to_sessions(&state, "acct-a", &creds(), true).await;

    assert_eq!(hits.profiles.load(Ordering::SeqCst), 1, "profile registered");
    assert_eq!(hits.switch.load(Ordering::SeqCst), 1, "idle session switched");
    Ok(())
}

#[tokio::test]
async fn distribute_defers_switch_for_busy_session() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream("running").await?;
    let state = state_with_accounts(&["acct-a"]).await;
    let e = entry("s1", url, Some("acct-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    distribute_to_sessions(&state, "acct-a", &creds(), true).await;

    // Busy session: profile still registered, but the switch is deferred.
    assert_eq!(hits.profiles.load(Ordering::SeqCst), 1, "profile registered even when busy");
    assert_eq!(hits.switch.load(Ordering::SeqCst), 0, "switch deferred for busy session");
    Ok(())
}

#[tokio::test]
async fn distribute_registers_without_switch_when_not_assigned() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a", "acct-b"]).await;
    // Session assigned to acct-b, but we distribute acct-a creds: profile is
    // registered for failover readiness, but no switch (not the assigned acct).
    let e = entry("s1", url, Some("acct-b"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    distribute_to_sessions(&state, "acct-a", &creds(), true).await;

    assert_eq!(hits.profiles.load(Ordering::SeqCst), 1, "profile registered");
    assert_eq!(hits.switch.load(Ordering::SeqCst), 0, "no switch — not the assigned account");
    Ok(())
}

#[tokio::test]
async fn distribute_skips_sessions_filtered_by_profiles_needed() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a"]).await;
    // profiles_needed excludes acct-a → session ineligible → no calls at all.
    let meta = serde_json::json!({ "profiles_needed": ["some-other-acct"] });
    let e = entry("s1", url, Some("acct-a"), meta);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    distribute_to_sessions(&state, "acct-a", &creds(), true).await;

    assert_eq!(hits.profiles.load(Ordering::SeqCst), 0, "ineligible session not pushed");
    assert_eq!(hits.switch.load(Ordering::SeqCst), 0);
    Ok(())
}

#[tokio::test]
async fn distribute_no_sessions_is_noop() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    // No sessions registered — must return cleanly.
    distribute_to_sessions(&state, "acct-a", &creds(), true).await;
    Ok(())
}

/// Insert an account into the state's cache with the given bead health, so
/// `distribute_to_sessions` can read its usability (ka-0000006430).
async fn seed_account_health(state: &MuxState, name: &str, status: &str, throttle_status: &str) {
    let mut cache = state.account_cache.write().await;
    cache.accounts_mut().insert(
        name.to_owned(),
        crate::credential::failover::AccountInfo {
            name: name.to_owned(),
            status: status.to_owned(),
            throttle_status: throttle_status.to_owned(),
            ..Default::default()
        },
    );
}

#[tokio::test]
async fn distribute_stamps_usable_true_for_healthy_account() -> anyhow::Result<()> {
    // The completeness fix: a Refreshed event on a HEALTHY account stamps
    // usable=true on the pushed profile so coop keeps it as a rotation target.
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a"]).await;
    seed_account_health(&state, "acct-a", "active", "allowed").await;
    let e = entry("s1", url, Some("acct-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    distribute_to_sessions(&state, "acct-a", &creds(), true).await;

    assert_eq!(hits.profiles.load(Ordering::SeqCst), 1, "profile registered");
    let body = hits.last_profile_body.read().await.clone().expect("body captured");
    assert_eq!(pushed_usable(&body), Some(true), "healthy account stamped usable=true");
    Ok(())
}

#[tokio::test]
async fn distribute_stamps_usable_false_for_exhausted_account() -> anyhow::Result<()> {
    // The incident vector: a Refreshed event on a billing-dead account
    // (exhausted + throttle rejected) must stamp usable=false so coop's
    // try_auto_rotate will NOT land on it.
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a"]).await;
    seed_account_health(&state, "acct-a", "exhausted", "rejected").await;
    let e = entry("s1", url, Some("acct-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    distribute_to_sessions(&state, "acct-a", &creds(), true).await;

    let body = hits.last_profile_body.read().await.clone().expect("body captured");
    assert_eq!(pushed_usable(&body), Some(false), "exhausted account stamped usable=false");
    Ok(())
}

#[tokio::test]
async fn distribute_fails_open_usable_true_for_unknown_account() -> anyhow::Result<()> {
    // Fail-open: an account not yet in the cache (beads client not up) must push
    // usable=true, not condemn a legitimately-refreshed profile before the cache
    // is populated.
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a"]).await;
    // Note: NO seed_account_health — the cache is empty.
    let e = entry("s1", url, Some("acct-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    distribute_to_sessions(&state, "acct-a", &creds(), true).await;

    let body = hits.last_profile_body.read().await.clone().expect("body captured");
    assert_eq!(pushed_usable(&body), Some(true), "unknown account fails open to usable=true");
    Ok(())
}

#[tokio::test]
async fn distribute_without_switch_flag_only_registers() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a"]).await;
    let e = entry("s1", url, Some("acct-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    // switch=false → profile registered, no status check, no switch.
    distribute_to_sessions(&state, "acct-a", &creds(), false).await;

    assert_eq!(hits.profiles.load(Ordering::SeqCst), 1);
    assert_eq!(hits.switch.load(Ordering::SeqCst), 0, "switch not requested");
    Ok(())
}

#[tokio::test]
async fn rebalance_moves_sessions_off_failed_account() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a", "acct-b"]).await;
    // Session on acct-a; acct-a "fails". Mark it throttled so the pool's
    // assign_account deterministically picks the healthy acct-b as the target
    // (otherwise both accounts are Healthy and selection is HashMap-order).
    state.credential_broker.mark_throttled("acct-a").await;
    let e = entry("s1", url, Some("acct-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    rebalance_from_account(&state, "acct-a").await;

    // reassign_session pushes a profile and triggers a switch on the target.
    assert!(hits.profiles.load(Ordering::SeqCst) >= 1, "profile pushed to session");
    assert!(hits.switch.load(Ordering::SeqCst) >= 1, "switch triggered");
    // The session's assigned account should now be the healthy target.
    let assigned = e.assigned_account.read().await.clone();
    assert_eq!(assigned.as_deref(), Some("acct-b"), "session reassigned to healthy account");
    Ok(())
}

#[tokio::test]
async fn rebalance_noop_when_only_failed_account_available() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream("idle").await?;
    let state = state_with_accounts(&["acct-a"]).await;
    let e = entry("s1", url, Some("acct-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    // Only acct-a exists — target resolves to the same failed account, so
    // rebalance short-circuits without touching the session.
    rebalance_from_account(&state, "acct-a").await;

    assert_eq!(hits.profiles.load(Ordering::SeqCst), 0, "no reassignment when no other account");
    assert_eq!(hits.switch.load(Ordering::SeqCst), 0);
    assert_eq!(e.assigned_account.read().await.as_deref(), Some("acct-a"));
    Ok(())
}
