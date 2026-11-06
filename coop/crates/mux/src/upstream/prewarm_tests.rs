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
use crate::credential::{CredentialConfig, CredentialEvent};
use crate::state::{MuxState, SessionEntry, SessionTransport, WatcherState};

// ── PrewarmCache pure-logic tests ─────────────────────────────────────────

#[test]
fn touch_inserts_and_lists_in_lru_order() {
    let mut cache = PrewarmCache::new(8);
    cache.touch("a");
    cache.touch("b");
    cache.touch("c");
    assert_eq!(cache.session_ids(), vec!["a", "b", "c"]);
}

#[test]
fn touch_existing_moves_to_back() {
    let mut cache = PrewarmCache::new(8);
    cache.touch("a");
    cache.touch("b");
    cache.touch("c");
    // Re-touching "a" moves it to most-recently-used (back).
    cache.touch("a");
    assert_eq!(cache.session_ids(), vec!["b", "c", "a"]);
}

#[test]
fn touch_evicts_oldest_over_capacity() {
    let mut cache = PrewarmCache::new(2);
    cache.touch("a");
    cache.touch("b");
    cache.touch("c"); // "a" is oldest → evicted.
    assert_eq!(cache.session_ids(), vec!["b", "c"]);
    cache.touch("d"); // "b" evicted.
    assert_eq!(cache.session_ids(), vec!["c", "d"]);
}

#[test]
fn remove_deletes_entry() {
    let mut cache = PrewarmCache::new(8);
    cache.touch("a");
    cache.touch("b");
    cache.remove("a");
    assert_eq!(cache.session_ids(), vec!["b"]);
    // Removing a non-existent id is a no-op.
    cache.remove("does-not-exist");
    assert_eq!(cache.session_ids(), vec!["b"]);
}

#[test]
fn empty_cache_lists_nothing() {
    let cache = PrewarmCache::new(4);
    assert!(cache.session_ids().is_empty());
}

// ── spawn_prewarm_task end-to-end ─────────────────────────────────────────

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

fn entry_with_url(id: &str, url: String) -> Arc<SessionEntry> {
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
        transport: SessionTransport::Http,
    })
}

/// Spin up a mock upstream coop that serves screen + status, counting hits.
/// Returns (base_url, screen_hits, status_hits).
async fn mock_upstream() -> anyhow::Result<(String, Arc<AtomicU32>, Arc<AtomicU32>)> {
    let screen_hits = Arc::new(AtomicU32::new(0));
    let status_hits = Arc::new(AtomicU32::new(0));

    let sh = Arc::clone(&screen_hits);
    let sth = Arc::clone(&status_hits);
    let app = Router::new()
        .route(
            "/api/v1/screen",
            get(move || {
                let sh = Arc::clone(&sh);
                async move {
                    sh.fetch_add(1, Ordering::SeqCst);
                    Json(serde_json::json!({
                        "lines": ["hello", "world"],
                        "ansi": ["\u{1b}[0mhello"],
                        "cols": 100,
                        "rows": 40,
                        "alt_screen": true,
                        "seq": 7
                    }))
                }
            }),
        )
        .route(
            "/api/v1/status",
            get(move || {
                let sth = Arc::clone(&sth);
                async move {
                    sth.fetch_add(1, Ordering::SeqCst);
                    Json(serde_json::json!({
                        "session_id": "sess-x",
                        "state": "idle",
                        "pid": 4242,
                        "uptime_secs": 12,
                        "exit_code": serde_json::Value::Null,
                        "screen_seq": 9,
                        "bytes_read": 100,
                        "bytes_written": 200,
                        "ws_clients": 1
                    }))
                }
            }),
        );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok((format!("http://{addr}"), screen_hits, status_hits))
}

/// Poll a condition until it holds or the deadline elapses.
async fn wait_until<F>(mut cond: F) -> bool
where
    F: FnMut() -> bool,
{
    for _ in 0..200 {
        if cond() {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    cond()
}

#[tokio::test]
async fn prewarm_task_populates_screen_and_status_for_unwatched_session() -> anyhow::Result<()> {
    let (url, screen_hits, status_hits) = mock_upstream().await?;
    let state = test_state();
    let entry = entry_with_url("sess-1", url);
    state.sessions.write().await.insert("sess-1".to_owned(), Arc::clone(&entry));

    // Add the session to the prewarm LRU (no active watchers → eligible for prewarm).
    state.prewarm.lock().await.touch("sess-1");

    let cancel = CancellationToken::new();
    spawn_prewarm_task(
        Arc::clone(&state),
        Arc::clone(&state.prewarm),
        Duration::from_millis(20),
        cancel.clone(),
    );

    let hit = wait_until(|| {
        screen_hits.load(Ordering::SeqCst) > 0 && status_hits.load(Ordering::SeqCst) > 0
    })
    .await;
    assert!(hit, "prewarm task should poll screen + status of the unwatched session");

    // Poll until the write locks land both cached values (avoids a fixed sleep).
    let cached = {
        let mut ok = false;
        for _ in 0..200 {
            if entry.cached_screen.read().await.is_some()
                && entry.cached_status.read().await.is_some()
            {
                ok = true;
                break;
            }
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
        ok
    };
    assert!(cached, "prewarm should populate cached screen + status");

    let screen = entry.cached_screen.read().await.clone();
    let screen = screen.expect("cached screen populated");
    assert_eq!(screen.lines, vec!["hello", "world"]);
    assert_eq!(screen.cols, 100);
    assert_eq!(screen.rows, 40);
    assert!(screen.alt_screen);
    assert_eq!(screen.seq, 7);

    let status = entry.cached_status.read().await.clone();
    let status = status.expect("cached status populated");
    assert_eq!(status.state, "idle");
    assert_eq!(status.pid, Some(4242));
    assert_eq!(status.screen_seq, 9);

    cancel.cancel();
    Ok(())
}

#[tokio::test]
async fn prewarm_task_skips_watched_sessions() -> anyhow::Result<()> {
    let (url, screen_hits, _status_hits) = mock_upstream().await?;
    let state = test_state();
    let entry = entry_with_url("sess-w", url);
    state.sessions.write().await.insert("sess-w".to_owned(), Arc::clone(&entry));
    state.prewarm.lock().await.touch("sess-w");

    // Register an active watcher for this session — prewarm must skip it, since
    // a fast poller (owned by the watcher) already covers it.
    state.feed.watchers.write().await.insert(
        "sess-w".to_owned(),
        WatcherState {
            watchers: std::collections::HashSet::from([0u64]),
            entry: Arc::downgrade(&entry),
            poller_cancel: CancellationToken::new(),
        },
    );

    let cancel = CancellationToken::new();
    spawn_prewarm_task(
        Arc::clone(&state),
        Arc::clone(&state.prewarm),
        Duration::from_millis(20),
        cancel.clone(),
    );

    // Let several ticks elapse.
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert_eq!(screen_hits.load(Ordering::SeqCst), 0, "watched session must not be prewarm-polled");

    cancel.cancel();
    Ok(())
}

#[tokio::test]
async fn prewarm_task_stops_on_cancel() -> anyhow::Result<()> {
    let (url, screen_hits, _status_hits) = mock_upstream().await?;
    let state = test_state();
    let entry = entry_with_url("sess-c", url);
    state.sessions.write().await.insert("sess-c".to_owned(), Arc::clone(&entry));
    state.prewarm.lock().await.touch("sess-c");

    let cancel = CancellationToken::new();
    spawn_prewarm_task(
        Arc::clone(&state),
        Arc::clone(&state.prewarm),
        Duration::from_millis(20),
        cancel.clone(),
    );

    assert!(
        wait_until(|| screen_hits.load(Ordering::SeqCst) > 0).await,
        "should poll before cancel"
    );
    cancel.cancel();
    tokio::time::sleep(Duration::from_millis(60)).await;
    let after_cancel = screen_hits.load(Ordering::SeqCst);
    tokio::time::sleep(Duration::from_millis(120)).await;
    assert_eq!(screen_hits.load(Ordering::SeqCst), after_cancel, "no further polls after cancel");
    Ok(())
}

/// Spin up a mock upstream whose `/api/v1/screen` never responds (accepts the
/// connection, then parks forever) — models a hung/parked session pod.
/// `/api/v1/status` is served so we can tell the two endpoints apart, but the
/// serial loop hits `screen` first, so a poll of this upstream would block
/// indefinitely without a per-request timeout.
async fn mock_hung_upstream() -> anyhow::Result<String> {
    let app = Router::new()
        .route(
            "/api/v1/screen",
            get(|| async {
                // Never resolve: hold the request open until the client's own
                // timeout fires (or the test ends).
                std::future::pending::<()>().await;
                Json(serde_json::json!({}))
            }),
        )
        .route("/api/v1/status", get(|| async { Json(serde_json::json!({"state": "idle"})) }));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(format!("http://{addr}"))
}

/// Regression for ka-0000003032: a single hung session pod must NOT stall the
/// serial pre-warm loop. With a bounded per-request timeout, the hung session's
/// poll returns Err quickly and the loop advances to poll a healthy session in
/// the same LRU. Before the fix (no timeout) the loop blocked forever on the
/// hung session's `get_screen().await` and the healthy one was never polled.
#[tokio::test]
async fn prewarm_hung_session_does_not_block_healthy_sessions() -> anyhow::Result<()> {
    let (healthy_url, screen_hits, status_hits) = mock_upstream().await?;
    let hung_url = mock_hung_upstream().await?;
    let state = test_state();

    // Insert the HUNG session first so it is polled before the healthy one —
    // this is what would deadlock the serial loop without a timeout.
    let hung = entry_with_url("sess-hung", hung_url);
    let healthy = entry_with_url("sess-ok", healthy_url);
    {
        let mut sessions = state.sessions.write().await;
        sessions.insert("sess-hung".to_owned(), Arc::clone(&hung));
        sessions.insert("sess-ok".to_owned(), Arc::clone(&healthy));
    }
    {
        let mut lru = state.prewarm.lock().await;
        lru.touch("sess-hung"); // oldest → polled first each tick
        lru.touch("sess-ok");
    }

    let cancel = CancellationToken::new();
    // Short request timeout so the hung poll is bounded well within the test.
    spawn_prewarm_task_with_timeout(
        Arc::clone(&state),
        Arc::clone(&state.prewarm),
        Duration::from_millis(20),
        Duration::from_millis(150),
        cancel.clone(),
    );

    // The healthy session must still get polled despite being ordered AFTER the
    // hung one in the LRU.
    let healthy_polled = wait_until(|| {
        screen_hits.load(Ordering::SeqCst) > 0 && status_hits.load(Ordering::SeqCst) > 0
    })
    .await;
    assert!(
        healthy_polled,
        "healthy session must be polled even though a hung session precedes it in the LRU"
    );

    let cached = healthy.cached_screen.read().await.is_some();
    assert!(cached, "healthy session's screen should be cached despite the hung peer");

    cancel.cancel();
    Ok(())
}

#[tokio::test]
async fn prewarm_task_no_sessions_is_a_noop() -> anyhow::Result<()> {
    let state = test_state();
    let cancel = CancellationToken::new();
    // Empty prewarm cache — the loop should tick, find nothing, and clear clients.
    spawn_prewarm_task(
        Arc::clone(&state),
        Arc::clone(&state.prewarm),
        Duration::from_millis(20),
        cancel.clone(),
    );
    tokio::time::sleep(Duration::from_millis(80)).await;
    cancel.cancel();
    Ok(())
}
