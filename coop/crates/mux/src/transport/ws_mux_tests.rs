// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::AtomicU32;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, bail, ensure};
use tokio::sync::{broadcast, mpsc, RwLock};
use tokio_util::sync::CancellationToken;

use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{CredentialConfig, CredentialEvent};
use crate::state::MuxEvent;
use crate::state::{CachedScreen, CachedUsage, MuxState, SessionEntry, SessionTransport};

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

fn test_state() -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None);
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), broker))
}

fn test_entry(id: &str) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url: "http://127.0.0.1:9090".into(),
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
        transport: SessionTransport::Nats { prefix: "coop.mux".to_owned() },
    })
}

async fn set_cached_screen(entry: &SessionEntry, seq: u64, line: &str) {
    *entry.cached_screen.write().await = Some(CachedScreen {
        lines: vec![line.to_owned()],
        ansi: vec![line.to_owned()],
        cols: 80,
        rows: 24,
        alt_screen: false,
        seq,
        fetched_at: 0,
    });
}

async fn receive_event(
    out_rx: &mut mpsc::Receiver<String>,
    event_name: &str,
    timeout: Duration,
) -> anyhow::Result<serde_json::Value> {
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
        if remaining.is_zero() {
            bail!("timed out waiting for {event_name}");
        }

        let text = tokio::time::timeout(remaining, out_rx.recv())
            .await
            .map_err(|_| anyhow!("timed out waiting for {event_name}"))?
            .ok_or_else(|| anyhow!("feed closed while waiting for {event_name}"))?;
        let value: serde_json::Value = serde_json::from_str(&text)?;
        if value.get("event").and_then(serde_json::Value::as_str) == Some(event_name) {
            return Ok(value);
        }
    }
}

async fn ensure_no_screen_batch(
    out_rx: &mut mpsc::Receiver<String>,
    duration: Duration,
) -> anyhow::Result<()> {
    let deadline = tokio::time::Instant::now() + duration;
    loop {
        let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
        if remaining.is_zero() {
            return Ok(());
        }

        let maybe_text = match tokio::time::timeout(remaining, out_rx.recv()).await {
            Ok(value) => value,
            Err(_) => return Ok(()),
        };
        let Some(text) = maybe_text else {
            return Ok(());
        };
        let value: serde_json::Value = serde_json::from_str(&text)?;
        ensure!(
            value.get("event").and_then(serde_json::Value::as_str) != Some("screen_batch"),
            "unexpected unchanged screen batch: {value}"
        );
    }
}

fn screen_batch_seq(value: &serde_json::Value, session_id: &str) -> anyhow::Result<u64> {
    let screens = value
        .get("screens")
        .and_then(serde_json::Value::as_array)
        .ok_or_else(|| anyhow!("screen_batch missing screens array"))?;
    for screen in screens {
        if screen.get("session").and_then(serde_json::Value::as_str) == Some(session_id) {
            return screen
                .get("seq")
                .and_then(serde_json::Value::as_u64)
                .ok_or_else(|| anyhow!("screen for {session_id} missing seq"));
        }
    }
    bail!("screen_batch missing screen for {session_id}")
}

#[tokio::test]
async fn run_mux_feed_filters_unchanged_screen_seq_per_client() -> anyhow::Result<()> {
    let state = test_state();
    let entry = test_entry("sess-1");
    set_cached_screen(&entry, 1, "first").await;
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&entry));

    let (out_tx, mut out_rx) = mpsc::channel(16);
    let (in_tx, in_rx) = mpsc::channel(16);
    let feed = tokio::spawn(super::run_mux_feed(Arc::clone(&state), out_tx, in_rx));

    receive_event(&mut out_rx, "sessions", Duration::from_millis(200)).await?;
    in_tx
        .send(super::MuxFeedMessage {
            message: super::MuxClientMessage::Subscribe { sessions: vec!["sess-1".to_owned()] },
            edge_identity: None,
        })
        .await?;

    let first = receive_event(&mut out_rx, "screen_batch", Duration::from_millis(500)).await?;
    ensure!(screen_batch_seq(&first, "sess-1")? == 1, "first screen batch used wrong seq");

    ensure_no_screen_batch(&mut out_rx, Duration::from_millis(1100)).await?;

    *entry.cached_usage.write().await =
        Some(CachedUsage { total_cost_usd: 1.25, ..CachedUsage::default() });
    let metadata_update =
        receive_event(&mut out_rx, "screen_batch", Duration::from_millis(1500)).await?;
    ensure!(
        screen_batch_seq(&metadata_update, "sess-1")? == 1,
        "metadata-only batch changed screen seq"
    );
    let updated_cost = metadata_update["screens"][0]["total_cost_usd"]
        .as_f64()
        .ok_or_else(|| anyhow!("metadata-only batch missing updated cost"))?;
    ensure!((updated_cost - 1.25).abs() < f64::EPSILON, "metadata-only batch used stale cost");

    set_cached_screen(&entry, 2, "second").await;
    let second = receive_event(&mut out_rx, "screen_batch", Duration::from_millis(1500)).await?;
    ensure!(screen_batch_seq(&second, "sess-1")? == 2, "second screen batch used wrong seq");

    drop(in_tx);
    tokio::time::timeout(Duration::from_millis(200), feed)
        .await
        .map_err(|_| anyhow!("feed did not stop after input channel closed"))?
        .map_err(|e| anyhow!("feed task join failed: {e}"))?;

    Ok(())
}

#[tokio::test]
async fn run_mux_feed_viewer_can_subscribe_but_cannot_send_input() -> anyhow::Result<()> {
    let state = test_state();
    let entry = test_entry("sess-1");
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&entry));

    let viewer = crate::transport::edge_auth::TrustedOperator {
        operator: "viewer@example.com".to_owned(),
        audit_id: "audit-viewer".to_owned(),
        version: 2,
        auth_mode: "entra".to_owned(),
        roles: vec!["gardenio-viewer".to_owned()],
        verified_v2: true,
        verified_v1: false,
        relay: false,
    };
    let (out_tx, mut out_rx) = mpsc::channel(16);
    let (in_tx, in_rx) = mpsc::channel(16);
    let feed = tokio::spawn(super::run_mux_feed(Arc::clone(&state), out_tx, in_rx));

    receive_event(&mut out_rx, "sessions", Duration::from_millis(200)).await?;
    in_tx
        .send(super::MuxFeedMessage {
            message: super::MuxClientMessage::Subscribe { sessions: vec!["sess-1".to_owned()] },
            edge_identity: Some(viewer.clone()),
        })
        .await?;
    in_tx
        .send(super::MuxFeedMessage {
            message: super::MuxClientMessage::InputSend {
                session: "sess-1".to_owned(),
                text: "whoami".to_owned(),
                enter: true,
            },
            edge_identity: Some(viewer),
        })
        .await?;
    let denied = receive_event(&mut out_rx, "error", Duration::from_millis(200)).await?;
    ensure!(
        denied["message"].as_str().is_some_and(|message| message.contains("gardenio-operator")),
        "unexpected denial: {denied}"
    );

    drop(in_tx);
    tokio::time::timeout(Duration::from_millis(200), feed)
        .await
        .map_err(|_| anyhow!("feed did not stop after input channel closed"))?
        .map_err(|e| anyhow!("feed task join failed: {e}"))?;
    Ok(())
}

#[tokio::test]
async fn start_watching_does_not_hold_watchers_while_waiting_for_sessions() -> anyhow::Result<()> {
    let state = test_state();
    let sessions = state.sessions.write().await;

    let task_state = Arc::clone(&state);
    let start = tokio::spawn(async move {
        let wid = task_state.feed.next_watcher_id();
        super::start_watching(&task_state, wid, "missing-session").await;
    });

    tokio::time::sleep(Duration::from_millis(10)).await;

    let watchers = tokio::time::timeout(Duration::from_millis(100), state.feed.watchers.write())
        .await
        .map_err(|_| anyhow!("start_watching held watchers while waiting for sessions"))?;

    drop(watchers);
    drop(sessions);

    tokio::time::timeout(Duration::from_millis(100), start)
        .await
        .map_err(|_| anyhow!("start_watching did not finish after sessions lock was released"))??;

    Ok(())
}

#[tokio::test]
async fn start_watching_rechecks_and_counts_distinct_watchers() -> anyhow::Result<()> {
    let state = test_state();
    state.sessions.write().await.insert("sess-1".to_owned(), test_entry("sess-1"));

    // Two distinct feeds watching the same session -> population 2.
    let wid1 = state.feed.next_watcher_id();
    let wid2 = state.feed.next_watcher_id();
    let first = super::start_watching(&state, wid1, "sess-1");
    let second = super::start_watching(&state, wid2, "sess-1");
    tokio::join!(first, second);

    // The same feed re-watching its session is idempotent (set dedup), so the
    // population stays 2 rather than inflating.
    super::start_watching(&state, wid1, "sess-1").await;

    let watchers = state.feed.watchers.read().await;
    let watcher = watchers.get("sess-1").ok_or_else(|| anyhow!("watcher should exist"))?;
    assert_eq!(watcher.count(), 2);

    Ok(())
}

#[tokio::test]
async fn watcher_lifecycle_does_not_cancel_session_lifetime() {
    let state = test_state();
    let entry = test_entry("sess-1");
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&entry));

    let wid = state.feed.next_watcher_id();
    super::start_watching(&state, wid, "sess-1").await;
    super::stop_watching(&state, wid, "sess-1").await;

    assert!(!entry.cancel.is_cancelled());
    assert!(state.feed.watchers.read().await.get("sess-1").is_none());
}

#[tokio::test]
async fn same_id_restart_rebinds_watcher_and_resends_equal_sequence() -> anyhow::Result<()> {
    let state = test_state();
    let old_entry = test_entry("sess-1");
    set_cached_screen(&old_entry, 1, "old generation").await;
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&old_entry));

    let (out_tx, mut out_rx) = mpsc::channel(16);
    let (in_tx, in_rx) = mpsc::channel(16);
    let feed = tokio::spawn(super::run_mux_feed(Arc::clone(&state), out_tx, in_rx));
    receive_event(&mut out_rx, "sessions", Duration::from_millis(200)).await?;
    in_tx
        .send(super::MuxFeedMessage {
            message: super::MuxClientMessage::Subscribe { sessions: vec!["sess-1".to_owned()] },
            edge_identity: None,
        })
        .await?;
    let first = receive_event(&mut out_rx, "screen_batch", Duration::from_millis(500)).await?;
    assert_eq!(first["screens"][0]["lines"][0], "old generation");

    state.remove_session("sess-1").await;
    let new_entry = test_entry("sess-1");
    set_cached_screen(&new_entry, 1, "new generation").await;
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&new_entry));
    let _ = state.feed.event_tx.send(MuxEvent::SessionOnline {
        session: "sess-1".to_owned(),
        url: new_entry.url.clone(),
        metadata: serde_json::Value::Null,
    });

    let restarted = receive_event(&mut out_rx, "screen_batch", Duration::from_millis(1500)).await?;
    assert_eq!(restarted["screens"][0]["seq"], 1);
    assert_eq!(restarted["screens"][0]["lines"][0], "new generation");

    let watchers = state.feed.watchers.read().await;
    let watcher = watchers.get("sess-1").ok_or_else(|| anyhow!("new watcher missing"))?;
    assert_eq!(watcher.count(), 1);
    assert!(watcher
        .entry
        .upgrade()
        .is_some_and(|watched_entry| Arc::ptr_eq(&watched_entry, &new_entry)));
    drop(watchers);

    drop(in_tx);
    tokio::time::timeout(Duration::from_millis(200), feed)
        .await
        .map_err(|_| anyhow!("feed did not stop after input channel closed"))?
        .map_err(|error| anyhow!("feed task join failed: {error}"))?;
    Ok(())
}

/// Regression for ka-0000005358: a SessionOnline that a feed never observes —
/// because the broadcast channel overflowed and the feed's receiver Lagged past
/// it — must NOT leave the watcher permanently bound to the dead generation.
///
/// A same-id restart normally re-runs `ensure_watching` on the SessionOnline
/// event, rebinding the watcher onto the new generation. If that event is
/// dropped by lag, the feed used to `continue` and skip the rebind, leaving the
/// watcher stranded on the old (removed) generation and its thumbnails stale
/// until the client happened to re-subscribe. The fix re-arms every watched
/// session on lag recovery.
///
/// The test forces a real Lagged: the broadcast channel holds 512 events, and
/// `#[tokio::test]` runs on a current-thread runtime, so sending >512 events
/// synchronously (without yielding) guarantees the spawned feed's next `recv`
/// returns `Lagged` before it can drain any of them.
#[tokio::test]
async fn lagged_broadcast_re_arms_watchers_onto_new_generation() -> anyhow::Result<()> {
    let state = test_state();
    let old_entry = test_entry("sess-1");
    set_cached_screen(&old_entry, 1, "old generation").await;
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&old_entry));

    let (out_tx, mut out_rx) = mpsc::channel(16);
    let (in_tx, in_rx) = mpsc::channel(16);
    let _feed = tokio::spawn(super::run_mux_feed(Arc::clone(&state), out_tx, in_rx));
    receive_event(&mut out_rx, "sessions", Duration::from_millis(200)).await?;
    in_tx
        .send(super::MuxFeedMessage {
            message: super::MuxClientMessage::Subscribe { sessions: vec!["sess-1".to_owned()] },
            edge_identity: None,
        })
        .await?;
    receive_event(&mut out_rx, "screen_batch", Duration::from_millis(500)).await?;

    // Same-id restart: the old generation is removed (dropping the watcher
    // entry) and a new generation registers under the same id.
    state.remove_session("sess-1").await;
    let new_entry = test_entry("sess-1");
    set_cached_screen(&new_entry, 1, "new generation").await;
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&new_entry));

    // Overflow the 512-deep broadcast channel so the SessionOnline the feed
    // would normally answer is dropped by lag. `broadcast::Sender::send` is
    // synchronous, so on this current-thread runtime the feed cannot drain any
    // of these until the test next awaits — guaranteeing a Lagged recv.
    let _ = state.feed.event_tx.send(MuxEvent::SessionOnline {
        session: "sess-1".to_owned(),
        url: new_entry.url.clone(),
        metadata: serde_json::Value::Null,
    });
    for _ in 0..600 {
        let _ = state.feed.event_tx.send(MuxEvent::SessionOffline { session: "flood".to_owned() });
    }

    // Drain the feed's outbound channel so it stays unblocked while it processes
    // the lag recovery, and poll until the watcher rebinds onto the new
    // generation (or fail after a deadline).
    let deadline = tokio::time::Instant::now() + Duration::from_millis(1500);
    let rebound = loop {
        {
            let watchers = state.feed.watchers.read().await;
            if let Some(ws) = watchers.get("sess-1") {
                if ws.entry.upgrade().is_some_and(|e| Arc::ptr_eq(&e, &new_entry)) {
                    break true;
                }
            }
        }
        if tokio::time::Instant::now() >= deadline {
            break false;
        }
        // Unblock the feed (its 16-deep out channel fills as it forwards the
        // flood) and let it run.
        let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
        let _ = tokio::time::timeout(remaining.min(Duration::from_millis(50)), out_rx.recv()).await;
    };
    ensure!(rebound, "watcher was not re-armed onto the new generation after lag recovery");

    {
        let watchers = state.feed.watchers.read().await;
        let ws = watchers.get("sess-1").ok_or_else(|| anyhow!("watcher missing after re-arm"))?;
        assert_eq!(ws.count(), 1, "re-arm must not inflate the watcher population");
    }

    drop(in_tx);
    // The feed may be blocked forwarding the flood; dropping in_tx alone does
    // not necessarily wake it promptly, so drain any remaining output.
    while tokio::time::timeout(Duration::from_millis(50), out_rx.recv())
        .await
        .ok()
        .flatten()
        .is_some()
    {}
    Ok(())
}

/// Regression for ka-0000004753: when N dashboard feeds watch one session and
/// that session cycles offline -> online (pod restart / NATS heartbeat gap),
/// `remove_session` drops the whole watcher entry and each surviving feed
/// re-adds itself on `SessionOnline`. The watcher population must be
/// reconstructed to N — not collapsed to 1 — so the FIRST feed to unsubscribe
/// or disconnect does not tear the shared screen poller out from under the
/// others.
///
/// Pre-fix (integer `count` reset to 1 on re-watch) this test fails: after the
/// two feeds re-watch the new generation the count is 1, so the first
/// `stop_watching` drops it to 0, removes the entry, and cancels the poller
/// while the second feed is still watching.
#[tokio::test]
async fn multi_watcher_survives_generation_swap_until_last_leaves() -> anyhow::Result<()> {
    let state = test_state();

    // Two distinct feeds watch generation 1 of sess-1.
    let old_entry = test_entry("sess-1");
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&old_entry));
    let wid1 = state.feed.next_watcher_id();
    let wid2 = state.feed.next_watcher_id();
    super::start_watching(&state, wid1, "sess-1").await;
    super::start_watching(&state, wid2, "sess-1").await;
    assert_eq!(
        state.feed.watchers.read().await.get("sess-1").map(crate::state::WatcherState::count),
        Some(2)
    );

    // Session goes offline: remove_session drops the watcher entry entirely.
    state.remove_session("sess-1").await;
    assert!(state.feed.watchers.read().await.get("sess-1").is_none());

    // New generation registers; both surviving feeds re-watch on SessionOnline.
    let new_entry = test_entry("sess-1");
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&new_entry));
    super::ensure_watching(&state, wid1, "sess-1").await;
    super::ensure_watching(&state, wid2, "sess-1").await;

    // Population must be reconstructed to 2, bound to the NEW generation.
    {
        let watchers = state.feed.watchers.read().await;
        let ws =
            watchers.get("sess-1").ok_or_else(|| anyhow!("watcher missing after re-online"))?;
        assert_eq!(ws.count(), 2, "generation swap collapsed the watcher population");
        assert!(ws.entry.upgrade().is_some_and(|watched| Arc::ptr_eq(&watched, &new_entry)));
    }

    // First feed leaves (unsubscribe or disconnect): the entry + poller MUST
    // survive because the second feed is still watching.
    super::stop_watching(&state, wid1, "sess-1").await;
    {
        let watchers = state.feed.watchers.read().await;
        let ws = watchers
            .get("sess-1")
            .ok_or_else(|| anyhow!("watcher torn down while a feed was still watching"))?;
        assert_eq!(ws.count(), 1);
        assert!(!ws.poller_cancel.is_cancelled(), "poller cancelled with a live watcher");
    }

    // Last feed leaves: now the entry is removed and the poller stops.
    super::stop_watching(&state, wid2, "sess-1").await;
    assert!(state.feed.watchers.read().await.get("sess-1").is_none());

    Ok(())
}

/// Build an HTTP-transport session entry pointing at `url`.
fn http_entry(id: &str, url: &str) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url: url.to_owned(),
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
        transport: SessionTransport::Http,
    })
}

/// Spin up a mock upstream whose `/api/v1/input` and `/api/v1/resize` never
/// respond (accept the connection, then park forever) — models a hung/parked
/// session pod, the exact state an operator is trying to intervene on.
async fn mock_hung_upstream() -> anyhow::Result<String> {
    use axum::routing::post;
    use axum::{Json, Router};

    let app = Router::new()
        .route(
            "/api/v1/input",
            post(|| async {
                std::future::pending::<()>().await;
                Json(serde_json::json!({}))
            }),
        )
        .route(
            "/api/v1/resize",
            post(|| async {
                std::future::pending::<()>().await;
                Json(serde_json::json!({}))
            }),
        );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(format!("http://{addr}"))
}

/// Regression for ka-0000003978: a hung upstream session pod must NOT block the
/// inline input/resize proxies forever. `run_mux_feed` awaits these calls
/// inside its single `select!` loop, so without a per-request timeout a hung
/// POST would freeze the operator's entire live feed (no pings, no screen
/// thumbnails, no further input). With a bounded timeout each proxy returns
/// promptly. Fault injection: remove the timeout (`Duration::from_secs(3600)`
/// here) and the `tokio::time::timeout` guard below fires instead.
#[tokio::test]
async fn hung_upstream_does_not_block_input_or_resize_proxy() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let hung_url = mock_hung_upstream().await?;
    let state = test_state();
    let entry = http_entry("sess-hung", &hung_url);
    state.sessions.write().await.insert("sess-hung".to_owned(), entry);

    // Short per-request timeout so a hung poll is bounded well within the test.
    let request_timeout = Duration::from_millis(150);
    // Outer guard is far longer than the request timeout: if the proxy honored
    // the timeout it returns in ~150ms; if it hung it trips this guard instead.
    let guard = Duration::from_secs(5);

    tokio::time::timeout(
        guard,
        super::proxy_input_with_timeout(&state, "sess-hung", "ls", true, request_timeout),
    )
    .await
    .map_err(|_| anyhow!("proxy_input hung on a parked upstream instead of timing out"))?;

    tokio::time::timeout(
        guard,
        super::proxy_resize_with_timeout(&state, "sess-hung", 120, 40, request_timeout),
    )
    .await
    .map_err(|_| anyhow!("proxy_resize hung on a parked upstream instead of timing out"))?;

    Ok(())
}

#[test]
fn edge_can_operate_refuses_relay_identity() {
    let relay = crate::transport::edge_auth::TrustedOperator {
        operator: "alice@example.com".to_owned(),
        audit_id: "audit-relay".to_owned(),
        version: 2,
        auth_mode: "cookie".to_owned(),
        roles: vec!["gardenio-operator".to_owned()],
        verified_v2: true,
        verified_v1: false,
        relay: true,
    };
    assert!(!super::edge_can_operate(Some(&relay)));
    let direct = crate::transport::edge_auth::TrustedOperator { relay: false, ..relay };
    assert!(super::edge_can_operate(Some(&direct)));
}
