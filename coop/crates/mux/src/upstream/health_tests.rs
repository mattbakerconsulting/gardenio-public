// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use std::time::Duration;

use axum::routing::get;
use axum::{Json, Router};
use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;

use super::*;
use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{AccountConfig, CredentialConfig, CredentialEvent};
use crate::state::{MuxState, SessionEntry, SessionTransport};

fn health_config(max_failures: u32) -> MuxConfig {
    MuxConfig {
        host: "127.0.0.1".into(),
        port: 0,
        auth_token: None,
        oidc_issuer_url: None,
        oidc_client_id: None,
        screen_poll_ms: 1000,
        status_poll_ms: 2000,
        // Fast health checks so tests converge quickly.
        health_check_ms: 20,
        max_health_failures: max_failures,
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

fn test_state(config: MuxConfig) -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None);
    Arc::new(MuxState::new(config, CancellationToken::new(), broker))
}

/// Like `test_state`, but seeds the credential broker with named accounts so
/// `pool_status()` reports per-account `session_count`s (used by the
/// reconcile-count-move regression test).
fn test_state_with_accounts(config: MuxConfig, accounts: &[&str]) -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let configs: Vec<AccountConfig> = accounts
        .iter()
        .map(|name| AccountConfig {
            name: (*name).to_owned(),
            provider: "claude".to_owned(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        })
        .collect();
    let broker = CredentialBroker::new(CredentialConfig { accounts: configs }, event_tx, None);
    Arc::new(MuxState::new(config, CancellationToken::new(), broker))
}

fn entry_with_url(id: &str, url: String, transport: SessionTransport) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url,
        auth_token: None,
        metadata: serde_json::Value::Null,
        registered_at: std::time::Instant::now(),
        cached_screen: RwLock::new(None),
        cached_status: RwLock::new(None),
        cached_usage: RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: RwLock::new(None),
        assigned_account: RwLock::new(None),
        claimed_bead_title: RwLock::new(None),
        transport,
    })
}

/// Mock upstream that always answers /api/v1/health with the given account name.
async fn healthy_upstream(account_name: &'static str) -> anyhow::Result<String> {
    let app = Router::new().route(
        "/api/v1/health",
        get(move || async move { Json(serde_json::json!({ "account_name": account_name })) }),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(format!("http://{addr}"))
}

/// Mock upstream that always fails health (500).
async fn failing_upstream() -> anyhow::Result<String> {
    let app = Router::new()
        .route("/api/v1/health", get(|| async { axum::http::StatusCode::INTERNAL_SERVER_ERROR }));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(format!("http://{addr}"))
}

#[tokio::test]
async fn health_check_reconciles_account_name() -> anyhow::Result<()> {
    let state = test_state(health_config(3));
    let url = healthy_upstream("acct-reconciled").await?;
    let entry = entry_with_url("s1", url, SessionTransport::Http);
    // Start with a stale/absent account name.
    *entry.assigned_account.write().await = Some("acct-stale".to_owned());
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&entry));

    spawn_health_checker(Arc::clone(&state));

    let mut reconciled = false;
    for _ in 0..200 {
        if entry.assigned_account.read().await.as_deref() == Some("acct-reconciled") {
            reconciled = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(reconciled, "health check should reconcile account name from health response");
    // Failures counter stays at 0 for a healthy upstream.
    assert_eq!(entry.health_failures.load(Ordering::Relaxed), 0);

    state.shutdown.cancel();
    Ok(())
}

/// Regression (ka-0000007760): when the health reconcile rewrites an EXISTING
/// non-null `assigned_account` (coop self-rotated onto a new profile so its
/// reported `account_name` diverged), it must move the broker `session_count`
/// from the old account to the new one — otherwise the old account's `+1` is
/// orphaned (deregister only unassigns the *current* account), permanently
/// inflating the old account's count and skewing least-loaded assignment.
#[tokio::test]
async fn health_reconcile_moves_session_count_between_accounts() -> anyhow::Result<()> {
    let state = test_state_with_accounts(health_config(3), &["acct-old", "acct-new"]);
    // Simulate the state just after a failover reassigned S onto acct-old:
    // assigned_account=acct-old and the broker counts one live session there.
    let url = healthy_upstream("acct-new").await?;
    let entry = entry_with_url("s1", url, SessionTransport::Http);
    *entry.assigned_account.write().await = Some("acct-old".to_owned());
    state.credential_broker.session_assigned("acct-old").await;
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&entry));

    let count_for = |pool: &[crate::credential::broker::PoolAccountInfo], name: &str| -> u32 {
        pool.iter().find(|p| p.name == name).map(|p| p.session_count).unwrap_or(0)
    };
    let pool = state.credential_broker.pool_status().await;
    assert_eq!(count_for(&pool, "acct-old"), 1, "precondition: acct-old holds the session");
    assert_eq!(count_for(&pool, "acct-new"), 0);

    spawn_health_checker(Arc::clone(&state));

    // Poll until the reconcile has rewritten assigned_account to acct-new.
    let mut reconciled = false;
    for _ in 0..200 {
        if entry.assigned_account.read().await.as_deref() == Some("acct-new") {
            reconciled = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(reconciled, "reconcile should rewrite assigned_account to the reported name");

    let pool = state.credential_broker.pool_status().await;
    assert_eq!(
        count_for(&pool, "acct-old"),
        0,
        "old account's session_count must be decremented when the reconcile moves the session"
    );
    assert_eq!(
        count_for(&pool, "acct-new"),
        1,
        "new account's session_count must be incremented by the reconcile"
    );

    state.shutdown.cancel();
    Ok(())
}

/// Companion (ka-0000007760): a reconcile that FILLS a previously-null
/// `assigned_account` (adopt of an as-yet-uncounted fresh session) must NOT
/// increment the count — registration never calls `session_assigned`, so
/// counting an adopt would over-count. Only replacements of a known old
/// account move the count.
#[tokio::test]
async fn health_reconcile_does_not_count_fill_from_none() -> anyhow::Result<()> {
    let state = test_state_with_accounts(health_config(3), &["acct-new"]);
    let url = healthy_upstream("acct-new").await?;
    let entry = entry_with_url("s1", url, SessionTransport::Http);
    // assigned_account starts as None (fresh session, never counted).
    assert!(entry.assigned_account.read().await.is_none());
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&entry));

    spawn_health_checker(Arc::clone(&state));

    let mut reconciled = false;
    for _ in 0..200 {
        if entry.assigned_account.read().await.as_deref() == Some("acct-new") {
            reconciled = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(reconciled, "reconcile should adopt the reported account into a null slot");

    let pool = state.credential_broker.pool_status().await;
    let acct_new = pool.iter().find(|p| p.name == "acct-new").map(|p| p.session_count).unwrap_or(0);
    assert_eq!(acct_new, 0, "adopting an uncounted fresh session must not increment session_count");

    state.shutdown.cancel();
    Ok(())
}

#[tokio::test]
async fn health_check_evicts_after_max_failures() -> anyhow::Result<()> {
    let state = test_state(health_config(2));
    let url = failing_upstream().await?;
    let entry = entry_with_url("s-dead", url, SessionTransport::Http);
    *entry.assigned_account.write().await = Some("acct-x".to_owned());
    state.sessions.write().await.insert("s-dead".to_owned(), Arc::clone(&entry));

    spawn_health_checker(Arc::clone(&state));

    let mut evicted = false;
    for _ in 0..200 {
        if !state.sessions.read().await.contains_key("s-dead") {
            evicted = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(evicted, "session should be evicted after consecutive health failures");
    // The eviction path cancels the entry.
    assert!(entry.cancel.is_cancelled(), "evicted session entry should be cancelled");

    state.shutdown.cancel();
    Ok(())
}

#[tokio::test]
async fn health_check_skips_nats_sessions() -> anyhow::Result<()> {
    let state = test_state(health_config(1));
    // A NATS-transport session with a dead URL: the checker must NOT probe it,
    // so it should never be evicted despite max_failures=1.
    let entry = entry_with_url(
        "s-nats",
        "http://127.0.0.1:1".to_owned(),
        SessionTransport::Nats { prefix: "coop.mux".to_owned() },
    );
    state.sessions.write().await.insert("s-nats".to_owned(), Arc::clone(&entry));

    spawn_health_checker(Arc::clone(&state));

    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(
        state.sessions.read().await.contains_key("s-nats"),
        "NATS session must not be health-probed or evicted"
    );
    assert_eq!(entry.health_failures.load(Ordering::Relaxed), 0);

    state.shutdown.cancel();
    Ok(())
}

#[tokio::test]
async fn health_check_stops_on_shutdown() -> anyhow::Result<()> {
    let state = test_state(health_config(3));
    let url = healthy_upstream("acct-a").await?;
    let entry = entry_with_url("s1", url, SessionTransport::Http);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&entry));

    spawn_health_checker(Arc::clone(&state));
    // Immediately signal shutdown; the checker loop should exit without evicting.
    state.shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(120)).await;
    assert!(state.sessions.read().await.contains_key("s1"), "healthy session retained");
    Ok(())
}

#[tokio::test]
async fn health_failure_counter_resets_on_recovery() -> anyhow::Result<()> {
    // A healthy upstream keeps failures pinned at 0 even if we pre-seed a count:
    // a successful check resets the counter to 0.
    let state = test_state(health_config(5));
    let url = healthy_upstream("acct-a").await?;
    let entry = entry_with_url("s1", url, SessionTransport::Http);
    entry.health_failures.store(3, Ordering::Relaxed);
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&entry));

    spawn_health_checker(Arc::clone(&state));

    let mut reset = false;
    for _ in 0..200 {
        if entry.health_failures.load(Ordering::Relaxed) == 0 {
            reset = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(reset, "successful health check should reset the failure counter");

    state.shutdown.cancel();
    Ok(())
}
