// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::HashMap;
use std::sync::Arc;

use tokio_util::sync::CancellationToken;

use tokio::sync::broadcast;

use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{CredentialConfig, CredentialEvent};
use crate::state::{MuxEvent, MuxState, SessionTransport};

fn test_config() -> MuxConfig {
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
        prewarm_capacity: 64,
        prewarm_poll_ms: 15000,
        state_dir: None,
        api_key_file: None,
        dev_mode: false,
        base_path: None,
        #[cfg(debug_assertions)]
        hot: false,
    }
}

fn test_state() -> Arc<MuxState> {
    state_with_accounts(&[])
}

/// Build a MuxState whose credential broker knows the named accounts, so
/// `pool_status()` reports a `session_count` for each. Empty slice = no
/// accounts (the common case).
fn state_with_accounts(names: &[&str]) -> Arc<MuxState> {
    use crate::credential::AccountConfig;
    // MuxState::new builds a reqwest client; install the provider so these
    // tests also pass when run filtered (without beads_tests in the process).
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let accounts = names
        .iter()
        .map(|n| AccountConfig {
            name: (*n).to_owned(),
            provider: "claude".to_owned(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: false,
        })
        .collect();
    let broker = CredentialBroker::new(CredentialConfig { accounts }, event_tx, None);
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), broker))
}

fn session_count_for(pool: &[crate::credential::broker::PoolAccountInfo], name: &str) -> u32 {
    pool.iter().find(|p| p.name == name).map(|p| p.session_count).unwrap_or(0)
}

// ── handle_announce ───────────────────────────────────────────────────────

#[tokio::test]
async fn announce_online_registers_session() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    let payload = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "agent": "claude",
        "ts": 1234567890
    }))?;

    super::handle_announce(&state, "coop.mux", "sess-1", &payload, &mut last_announce).await;

    let sessions = state.sessions.read().await;
    assert!(sessions.contains_key("sess-1"), "session should be registered");
    let entry = &sessions["sess-1"];
    assert_eq!(entry.url, "http://127.0.0.1:9090");
    assert!(matches!(entry.transport, SessionTransport::Nats { .. }));
    if let SessionTransport::Nats { ref prefix } = entry.transport {
        assert_eq!(prefix, "coop.mux");
    }
    assert!(last_announce.contains_key("sess-1"));
    Ok(())
}

#[tokio::test]
async fn announce_heartbeat_updates_timestamp() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    // First online.
    let payload = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &payload, &mut last_announce).await;
    let t1 = last_announce["sess-1"];

    // Small delay so timestamps differ.
    tokio::time::sleep(std::time::Duration::from_millis(10)).await;

    // Heartbeat — should NOT create a duplicate entry.
    let payload = serde_json::to_vec(&serde_json::json!({
        "event": "heartbeat",
        "session_id": "sess-1",
        "ts": 2000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &payload, &mut last_announce).await;
    let t2 = last_announce["sess-1"];

    assert!(t2 >= t1, "heartbeat should update timestamp");
    let sessions = state.sessions.read().await;
    assert_eq!(sessions.len(), 1, "should still be one session");
    Ok(())
}

#[tokio::test]
async fn announce_online_seeds_account_name() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    let payload = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "account_name": "acct-a",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &payload, &mut last_announce).await;

    let sessions = state.sessions.read().await;
    let account = sessions["sess-1"].assigned_account.read().await.clone();
    assert_eq!(account.as_deref(), Some("acct-a"));
    Ok(())
}

#[tokio::test]
async fn heartbeat_adopts_account_when_unset() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    // Online without an account (e.g. auto-assign disabled, no seed yet).
    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;
    {
        let sessions = state.sessions.read().await;
        assert!(sessions["sess-1"].assigned_account.read().await.is_none());
    }

    // Heartbeat now carries the account — must fill the null.
    let heartbeat = serde_json::to_vec(&serde_json::json!({
        "event": "heartbeat",
        "session_id": "sess-1",
        "account_name": "acct-a",
        "ts": 2000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &heartbeat, &mut last_announce).await;

    let sessions = state.sessions.read().await;
    let account = sessions["sess-1"].assigned_account.read().await.clone();
    assert_eq!(account.as_deref(), Some("acct-a"));
    Ok(())
}

#[tokio::test]
async fn heartbeat_reconciles_changed_account() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "account_name": "acct-a",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;

    // Session self-rotated onto a different profile; announce reports the new
    // active account. NATS sessions have no health poller, so the heartbeat
    // must reconcile the change.
    let heartbeat = serde_json::to_vec(&serde_json::json!({
        "event": "heartbeat",
        "session_id": "sess-1",
        "account_name": "acct-b",
        "ts": 2000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &heartbeat, &mut last_announce).await;

    let sessions = state.sessions.read().await;
    let account = sessions["sess-1"].assigned_account.read().await.clone();
    assert_eq!(account.as_deref(), Some("acct-b"));
    Ok(())
}

/// Regression (ka-0000007760 NATS twin): when the announce reconcile rewrites a
/// NATS session's `assigned_account` from a *known* old account onto a new one,
/// it must move the broker `session_count` in lockstep — otherwise the old
/// account's `+1` (set when a rebalance/failover ran `reassign_session` on the
/// NATS session) is orphaned forever (the offline path unassigns only the
/// *current* account), permanently inflating it and skewing least-loaded.
#[tokio::test]
async fn heartbeat_reconcile_moves_session_count_between_accounts() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-old", "acct-new"]);
    let mut last_announce = HashMap::new();

    // Register the NATS session already on acct-old.
    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "",
        "account_name": "acct-old",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;

    // Simulate the state just after a failover/rebalance reassigned the session
    // onto acct-old: reassign_session bumps the broker count for the target.
    state.credential_broker.session_assigned("acct-old").await;
    let pool = state.credential_broker.pool_status().await;
    assert_eq!(session_count_for(&pool, "acct-old"), 1, "precondition: acct-old holds the session");
    assert_eq!(session_count_for(&pool, "acct-new"), 0);

    // The session self-rotates onto acct-new and announces the change.
    let heartbeat = serde_json::to_vec(&serde_json::json!({
        "event": "heartbeat",
        "session_id": "sess-1",
        "account_name": "acct-new",
        "ts": 2000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &heartbeat, &mut last_announce).await;

    let account = state.sessions.read().await["sess-1"].assigned_account.read().await.clone();
    assert_eq!(account.as_deref(), Some("acct-new"), "reconcile should rewrite assigned_account");

    let pool = state.credential_broker.pool_status().await;
    assert_eq!(
        session_count_for(&pool, "acct-old"),
        0,
        "old account's session_count must be decremented when the reconcile moves the session"
    );
    assert_eq!(
        session_count_for(&pool, "acct-new"),
        1,
        "new account's session_count must be incremented by the reconcile"
    );
    Ok(())
}

/// Companion (ka-0000007760 NATS twin): a reconcile that FILLS a previously
/// -null `assigned_account` (an adopt of an as-yet-uncounted session — NATS
/// registration never calls `session_assigned`) must NOT increment the count,
/// or the adopt would over-count. Only replacements of a *known* old account
/// move the count.
#[tokio::test]
async fn heartbeat_reconcile_does_not_count_fill_from_none() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-new"]);
    let mut last_announce = HashMap::new();

    // Register with no account_name — assigned_account starts as None (fresh,
    // uncounted session).
    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;
    assert!(
        state.sessions.read().await["sess-1"].assigned_account.read().await.is_none(),
        "precondition: session registered with a null assigned_account"
    );

    // A later heartbeat adopts acct-new into the null slot.
    let heartbeat = serde_json::to_vec(&serde_json::json!({
        "event": "heartbeat",
        "session_id": "sess-1",
        "account_name": "acct-new",
        "ts": 2000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &heartbeat, &mut last_announce).await;

    let account = state.sessions.read().await["sess-1"].assigned_account.read().await.clone();
    assert_eq!(account.as_deref(), Some("acct-new"), "reconcile should adopt into the null slot");

    let pool = state.credential_broker.pool_status().await;
    assert_eq!(
        session_count_for(&pool, "acct-new"),
        0,
        "adopting an uncounted fresh session must not increment session_count"
    );
    Ok(())
}

#[tokio::test]
async fn heartbeat_without_account_preserves_existing() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "account_name": "acct-a",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;

    // Heartbeat with no account_name (and one with empty string) must never
    // clear an existing assignment.
    for payload in [
        serde_json::json!({ "event": "heartbeat", "session_id": "sess-1", "ts": 2000 }),
        serde_json::json!({
            "event": "heartbeat",
            "session_id": "sess-1",
            "account_name": "",
            "ts": 3000
        }),
    ] {
        let bytes = serde_json::to_vec(&payload)?;
        super::handle_announce(&state, "coop.mux", "sess-1", &bytes, &mut last_announce).await;
    }

    let sessions = state.sessions.read().await;
    let account = sessions["sess-1"].assigned_account.read().await.clone();
    assert_eq!(account.as_deref(), Some("acct-a"));
    Ok(())
}

#[tokio::test]
async fn announce_offline_removes_nats_session() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    // Register.
    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;

    // Offline.
    let offline = serde_json::to_vec(&serde_json::json!({
        "event": "offline",
        "session_id": "sess-1",
        "ts": 2000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &offline, &mut last_announce).await;

    let sessions = state.sessions.read().await;
    assert!(!sessions.contains_key("sess-1"), "session should be removed");
    assert!(!last_announce.contains_key("sess-1"));
    Ok(())
}

/// Regression (ka-0000001992): when a NATS session that a rebalance/failover
/// assigned onto an account goes offline, its removal must unassign the broker
/// `session_count` for the *current* account — otherwise the `+1` set by
/// `reassign_session` is orphaned forever (remove_session itself never
/// unassigns), permanently inflating the account and skewing least-loaded away
/// from an actually-idle account for the process lifetime.
#[tokio::test]
async fn announce_offline_unassigns_session_count() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]);
    let mut last_announce = HashMap::new();

    // Register the NATS session already on acct-a.
    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "",
        "account_name": "acct-a",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;

    // A failover/rebalance ran reassign_session onto acct-a, bumping the count.
    state.credential_broker.session_assigned("acct-a").await;
    let pool = state.credential_broker.pool_status().await;
    assert_eq!(session_count_for(&pool, "acct-a"), 1, "precondition: acct-a holds the session");

    // The session goes offline.
    let offline = serde_json::to_vec(&serde_json::json!({
        "event": "offline",
        "session_id": "sess-1",
        "ts": 2000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &offline, &mut last_announce).await;

    assert!(!state.sessions.read().await.contains_key("sess-1"), "session should be removed");
    let pool = state.credential_broker.pool_status().await;
    assert_eq!(
        session_count_for(&pool, "acct-a"),
        0,
        "offline removal must unassign the session_count for the assigned account"
    );
    Ok(())
}

/// Regression (ka-0000001992): `remove_nats_session` is the shared removal path
/// for BOTH the offline announce and the 90s announce-timeout eviction sweep. It
/// must unassign the broker `session_count` for a reassigned NATS session, and
/// must be a no-op (returning false) for a non-NATS / absent session so it can
/// never evict an HTTP session or double-decrement.
#[tokio::test]
async fn remove_nats_session_unassigns_and_guards_transport() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]);
    let mut last_announce = HashMap::new();

    // Absent session -> false, no panic.
    assert!(!super::remove_nats_session(&state, "nope").await, "absent session must return false");

    // Register a NATS session on acct-a and simulate a rebalance assign.
    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "",
        "account_name": "acct-a",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;
    state.credential_broker.session_assigned("acct-a").await;
    assert_eq!(
        session_count_for(&state.credential_broker.pool_status().await, "acct-a"),
        1,
        "precondition: acct-a holds the session"
    );

    // The eviction sweep body calls remove_nats_session directly.
    assert!(
        super::remove_nats_session(&state, "sess-1").await,
        "removing a live NATS session must return true"
    );
    assert!(
        !state.sessions.read().await.contains_key("sess-1"),
        "evicted session should be removed"
    );
    assert_eq!(
        session_count_for(&state.credential_broker.pool_status().await, "acct-a"),
        0,
        "eviction must unassign the session_count for the evicted account"
    );
    Ok(())
}

#[tokio::test]
async fn announce_invalid_json_is_ignored() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    super::handle_announce(&state, "coop.mux", "sess-1", b"bad json", &mut last_announce).await;

    let sessions = state.sessions.read().await;
    assert!(sessions.is_empty());
    Ok(())
}

#[tokio::test]
async fn announce_online_emits_session_online_event() -> anyhow::Result<()> {
    let state = test_state();
    let mut event_rx = state.feed.event_tx.subscribe();
    let mut last_announce = HashMap::new();

    let payload = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "ts": 1234
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &payload, &mut last_announce).await;

    let event = event_rx.try_recv()?;
    match event {
        MuxEvent::SessionOnline { session, url, .. } => {
            assert_eq!(session, "sess-1");
            assert_eq!(url, "http://127.0.0.1:9090");
        }
        other => anyhow::bail!("expected SessionOnline, got {other:?}"),
    }
    Ok(())
}

// ── handle_status ─────────────────────────────────────────────────────────

#[tokio::test]
async fn status_updates_cached_status() -> anyhow::Result<()> {
    let state = test_state();
    let mut last_announce = HashMap::new();

    // Register a session first.
    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "ts": 1000
    }))?;
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;

    // Send status update.
    let status_payload = serde_json::to_vec(&serde_json::json!({
        "session_id": "sess-1",
        "state": "working",
        "pid": 1234,
        "uptime_secs": 42,
        "exit_code": null,
        "screen_seq": 10,
        "bytes_read": 1024,
        "bytes_written": 512,
        "ws_clients": 0,
        "fetched_at": 9999
    }))?;
    super::handle_status(&state, "sess-1", &status_payload).await;

    let sessions = state.sessions.read().await;
    let entry = &sessions["sess-1"];
    let cached = entry.cached_status.read().await;
    let Some(status) = cached.as_ref() else {
        anyhow::bail!("status should be cached");
    };
    assert_eq!(status.state, "working");
    assert_eq!(status.pid, Some(1234));
    assert_eq!(status.uptime_secs, 42);
    assert_eq!(status.screen_seq, 10);
    Ok(())
}

#[tokio::test]
async fn status_for_unknown_session_is_ignored() -> anyhow::Result<()> {
    let state = test_state();

    let status_payload = serde_json::to_vec(&serde_json::json!({
        "session_id": "nonexistent",
        "state": "idle",
        "pid": null,
        "uptime_secs": 0,
        "exit_code": null,
        "screen_seq": 0,
        "bytes_read": 0,
        "bytes_written": 0,
        "ws_clients": 0,
        "fetched_at": 0
    }))?;
    // Should not panic.
    super::handle_status(&state, "nonexistent", &status_payload).await;
    Ok(())
}

#[tokio::test]
async fn status_invalid_json_is_ignored() -> anyhow::Result<()> {
    let state = test_state();
    // Should not panic.
    super::handle_status(&state, "sess-1", b"not json").await;
    Ok(())
}

/// Regression: the real coop producer (`nats_relay::status_loop`) serializes a
/// `SessionStatus`, which has NO `fetched_at` field. A strict decode of
/// `CachedStatus` (which requires `fetched_at`) rejected every such message, so
/// NATS-transport sessions never populated `cached_status`. Feed the exact
/// producer shape (no `fetched_at`) and assert it decodes and caches.
#[tokio::test]
async fn status_without_fetched_at_is_cached() -> anyhow::Result<()> {
    let state = test_state();

    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "ts": 1000
    }))?;
    let mut last_announce = HashMap::new();
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;

    // Exact producer payload: SessionStatus fields, NO `fetched_at`.
    let status_payload = serde_json::to_vec(&serde_json::json!({
        "session_id": "sess-1",
        "state": "working",
        "pid": 1234,
        "uptime_secs": 42,
        "exit_code": null,
        "screen_seq": 10,
        "bytes_read": 1024,
        "bytes_written": 512,
        "ws_clients": 0
    }))?;
    super::handle_status(&state, "sess-1", &status_payload).await;

    let sessions = state.sessions.read().await;
    let entry = &sessions["sess-1"];
    let cached = entry.cached_status.read().await;
    let Some(status) = cached.as_ref() else {
        anyhow::bail!("status should be cached even without fetched_at");
    };
    assert_eq!(status.state, "working");
    assert_eq!(status.pid, Some(1234));
    // Receive-time stamp is applied by the handler, not the wire payload.
    assert!(status.fetched_at > 0, "fetched_at should be stamped on receive");
    Ok(())
}

// ── handle_usage ──────────────────────────────────────────────────────────

/// Regression: the real coop producer (`nats_relay::usage_loop`) emits ONLY
/// `input_tokens`, `output_tokens`, and `request_count` — never
/// `total_cost_usd` or `fetched_at`. A strict decode of `CachedUsage` (which
/// required all five fields) rejected every usage message, so NATS-transport
/// sessions never populated `cached_usage` (no cost in the dashboard). Feed the
/// exact producer shape and assert it decodes and caches.
#[tokio::test]
async fn usage_with_producer_payload_is_cached() -> anyhow::Result<()> {
    let state = test_state();

    let online = serde_json::to_vec(&serde_json::json!({
        "event": "online",
        "session_id": "sess-1",
        "url": "http://127.0.0.1:9090",
        "ts": 1000
    }))?;
    let mut last_announce = HashMap::new();
    super::handle_announce(&state, "coop.mux", "sess-1", &online, &mut last_announce).await;

    // Exact producer payload: only the three counters usage_loop publishes.
    let usage_payload = serde_json::to_vec(&serde_json::json!({
        "input_tokens": 1500,
        "output_tokens": 640,
        "request_count": 7
    }))?;
    super::handle_usage(&state, "sess-1", &usage_payload).await;

    let sessions = state.sessions.read().await;
    let entry = &sessions["sess-1"];
    let cached = entry.cached_usage.read().await;
    let Some(usage) = cached.as_ref() else {
        anyhow::bail!("usage should be cached with the producer's 3-field payload");
    };
    assert_eq!(usage.input_tokens, 1500);
    assert_eq!(usage.output_tokens, 640);
    assert_eq!(usage.request_count, 7);
    // Omitted fields default; fetched_at is stamped on receive.
    assert_eq!(usage.total_cost_usd, 0.0);
    assert!(usage.fetched_at > 0, "fetched_at should be stamped on receive");
    Ok(())
}

#[tokio::test]
async fn usage_for_unknown_session_is_ignored() -> anyhow::Result<()> {
    let state = test_state();
    let usage_payload = serde_json::to_vec(&serde_json::json!({
        "input_tokens": 1,
        "output_tokens": 2,
        "request_count": 3
    }))?;
    // Should not panic.
    super::handle_usage(&state, "nonexistent", &usage_payload).await;
    Ok(())
}

#[tokio::test]
async fn usage_invalid_json_is_ignored() -> anyhow::Result<()> {
    let state = test_state();
    // Should not panic.
    super::handle_usage(&state, "sess-1", b"not json").await;
    Ok(())
}

// ── handle_state ──────────────────────────────────────────────────────────

#[tokio::test]
async fn state_emits_transition_event() -> anyhow::Result<()> {
    let state = test_state();
    let mut event_rx = state.feed.event_tx.subscribe();

    let state_payload = serde_json::to_vec(&serde_json::json!({
        "prev": "idle",
        "next": "working",
        "seq": 5,
        "cause": "hook",
        "last_message": "doing stuff"
    }))?;
    super::handle_state(&state, "sess-1", &state_payload).await;

    let event = event_rx.try_recv()?;
    match event {
        MuxEvent::Transition { session, prev, next, seq, cause, last_message, .. } => {
            assert_eq!(session, "sess-1");
            assert_eq!(prev, "idle");
            assert_eq!(next, "working");
            assert_eq!(seq, 5);
            assert_eq!(cause, "hook");
            assert_eq!(last_message.as_deref(), Some("doing stuff"));
        }
        other => anyhow::bail!("expected Transition, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn state_with_minimal_fields() -> anyhow::Result<()> {
    let state = test_state();
    let mut event_rx = state.feed.event_tx.subscribe();

    // Only required fields — all optional fields default.
    let state_payload = serde_json::to_vec(&serde_json::json!({
        "prev": "starting",
        "next": "idle"
    }))?;
    super::handle_state(&state, "sess-2", &state_payload).await;

    let event = event_rx.try_recv()?;
    match event {
        MuxEvent::Transition { session, prev, next, seq, cause, .. } => {
            assert_eq!(session, "sess-2");
            assert_eq!(prev, "starting");
            assert_eq!(next, "idle");
            assert_eq!(seq, 0);
            assert!(cause.is_empty());
        }
        other => anyhow::bail!("expected Transition, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn state_invalid_json_is_ignored() -> anyhow::Result<()> {
    let state = test_state();
    let mut event_rx = state.feed.event_tx.subscribe();

    super::handle_state(&state, "sess-1", b"bad json").await;

    // No event should be emitted.
    assert!(event_rx.try_recv().is_err());
    Ok(())
}
