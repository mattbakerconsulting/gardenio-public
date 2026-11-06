// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use std::time::Duration;

use axum::routing::get;
use axum::{Json, Router};
use tokio::sync::RwLock;
use tokio_util::sync::CancellationToken;

use super::*;
use crate::config::MuxConfig;
use crate::state::{SessionEntry, SessionTransport};

fn fast_config() -> MuxConfig {
    MuxConfig {
        host: "127.0.0.1".into(),
        port: 0,
        auth_token: None,
        oidc_issuer_url: None,
        oidc_client_id: None,
        // Very short intervals so the pollers fire quickly under test.
        screen_poll_ms: 20,
        status_poll_ms: 20,
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

struct Hits {
    screen: Arc<AtomicU32>,
    status: Arc<AtomicU32>,
    usage: Arc<AtomicU32>,
}

/// Mock upstream coop serving screen, status and usage endpoints, counting hits.
async fn mock_upstream() -> anyhow::Result<(String, Hits)> {
    let screen = Arc::new(AtomicU32::new(0));
    let status = Arc::new(AtomicU32::new(0));
    let usage = Arc::new(AtomicU32::new(0));

    let sc = Arc::clone(&screen);
    let st = Arc::clone(&status);
    let us = Arc::clone(&usage);
    let app = Router::new()
        .route(
            "/api/v1/screen",
            get(move || {
                let sc = Arc::clone(&sc);
                async move {
                    sc.fetch_add(1, Ordering::SeqCst);
                    Json(serde_json::json!({
                        "lines": ["a", "b"],
                        "ansi": ["a"],
                        "cols": 120,
                        "rows": 50,
                        "alt_screen": false,
                        "seq": 3
                    }))
                }
            }),
        )
        .route(
            "/api/v1/status",
            get(move || {
                let st = Arc::clone(&st);
                async move {
                    st.fetch_add(1, Ordering::SeqCst);
                    Json(serde_json::json!({
                        "session_id": "s1",
                        "state": "working",
                        "pid": 99,
                        "uptime_secs": 5,
                        "screen_seq": 4,
                        "bytes_read": 10,
                        "bytes_written": 20,
                        "ws_clients": 2
                    }))
                }
            }),
        )
        .route(
            "/api/v1/session/usage",
            get(move || {
                let us = Arc::clone(&us);
                async move {
                    us.fetch_add(1, Ordering::SeqCst);
                    Json(serde_json::json!({
                        "input_tokens": 111,
                        "output_tokens": 222,
                        "total_cost_usd": 3.5,
                        "request_count": 9
                    }))
                }
            }),
        );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok((format!("http://{addr}"), Hits { screen, status, usage }))
}

async fn wait_hits(counter: &AtomicU32) -> bool {
    for _ in 0..200 {
        if counter.load(Ordering::SeqCst) > 0 {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    counter.load(Ordering::SeqCst) > 0
}

/// Poll a session's cached screen until populated, returning the snapshot.
async fn wait_screen(entry: &SessionEntry) -> Option<crate::state::CachedScreen> {
    for _ in 0..200 {
        if let Some(s) = entry.cached_screen.read().await.clone() {
            return Some(s);
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    None
}

async fn wait_status(entry: &SessionEntry) -> Option<crate::state::CachedStatus> {
    for _ in 0..200 {
        if let Some(s) = entry.cached_status.read().await.clone() {
            return Some(s);
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    None
}

async fn wait_usage(entry: &SessionEntry) -> Option<crate::state::CachedUsage> {
    for _ in 0..200 {
        if let Some(s) = entry.cached_usage.read().await.clone() {
            return Some(s);
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    None
}

#[tokio::test]
async fn screen_poller_caches_snapshot() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream().await?;
    let entry = entry_with_url("s1", url);
    let http = reqwest::Client::new();
    let cancel = CancellationToken::new();

    spawn_screen_poller(Arc::clone(&entry), &fast_config(), http, cancel.clone());

    assert!(wait_hits(&hits.screen).await, "screen polled");

    let screen = wait_screen(&entry).await.expect("screen cached");
    assert_eq!(screen.lines, vec!["a", "b"]);
    assert_eq!(screen.cols, 120);
    assert_eq!(screen.rows, 50);
    assert!(!screen.alt_screen);
    assert_eq!(screen.seq, 3);

    cancel.cancel();
    Ok(())
}

#[tokio::test]
async fn status_poller_caches_snapshot() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream().await?;
    let entry = entry_with_url("s1", url);
    let http = reqwest::Client::new();
    let cancel = CancellationToken::new();

    spawn_screen_poller(Arc::clone(&entry), &fast_config(), http, cancel.clone());

    assert!(wait_hits(&hits.status).await, "status polled");
    let status = wait_status(&entry).await.expect("status cached");
    assert_eq!(status.session_id, "s1");
    assert_eq!(status.state, "working");
    assert_eq!(status.pid, Some(99));
    assert_eq!(status.screen_seq, 4);
    assert_eq!(status.bytes_read, 10);
    assert_eq!(status.bytes_written, 20);
    assert_eq!(status.ws_clients, 2);

    cancel.cancel();
    Ok(())
}

#[tokio::test]
async fn usage_poller_caches_snapshot() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream().await?;
    let entry = entry_with_url("s1", url);
    let http = reqwest::Client::new();
    let cancel = CancellationToken::new();

    spawn_screen_poller(Arc::clone(&entry), &fast_config(), http, cancel.clone());

    assert!(wait_hits(&hits.usage).await, "usage polled");
    let usage = wait_usage(&entry).await.expect("usage cached");
    assert_eq!(usage.input_tokens, 111);
    assert_eq!(usage.output_tokens, 222);
    assert!((usage.total_cost_usd - 3.5).abs() < f64::EPSILON);
    assert_eq!(usage.request_count, 9);

    cancel.cancel();
    Ok(())
}

#[tokio::test]
async fn pollers_skip_when_health_failing() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream().await?;
    let entry = entry_with_url("s1", url);
    // Flag the session as failing before starting the pollers: every tick body
    // early-continues, so no upstream request should ever be issued.
    entry.health_failures.store(1, Ordering::Relaxed);

    let http = reqwest::Client::new();
    let cancel = CancellationToken::new();
    spawn_screen_poller(Arc::clone(&entry), &fast_config(), http, cancel.clone());

    // Let many ticks elapse.
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert_eq!(hits.screen.load(Ordering::SeqCst), 0, "screen skipped while failing");
    assert_eq!(hits.status.load(Ordering::SeqCst), 0, "status skipped while failing");
    assert_eq!(hits.usage.load(Ordering::SeqCst), 0, "usage skipped while failing");
    assert!(entry.cached_screen.read().await.is_none());

    cancel.cancel();
    Ok(())
}

#[tokio::test]
async fn pollers_stop_on_cancel() -> anyhow::Result<()> {
    let (url, hits) = mock_upstream().await?;
    let entry = entry_with_url("s1", url);
    let http = reqwest::Client::new();
    let cancel = CancellationToken::new();
    spawn_screen_poller(Arc::clone(&entry), &fast_config(), http, cancel.clone());

    assert!(wait_hits(&hits.screen).await, "polled before cancel");
    cancel.cancel();
    tokio::time::sleep(Duration::from_millis(60)).await;
    let snapshot = hits.screen.load(Ordering::SeqCst);
    tokio::time::sleep(Duration::from_millis(150)).await;
    assert_eq!(hits.screen.load(Ordering::SeqCst), snapshot, "no polls after cancel");
    Ok(())
}

#[tokio::test]
async fn poller_tolerates_dead_upstream() -> anyhow::Result<()> {
    // Point at a port with nothing listening — poll requests fail but the loop
    // must keep running (error is logged, cache stays empty) without panicking.
    let entry = entry_with_url("s1", "http://127.0.0.1:1".to_owned());
    let http = reqwest::Client::new();
    let cancel = CancellationToken::new();
    spawn_screen_poller(Arc::clone(&entry), &fast_config(), http, cancel.clone());

    tokio::time::sleep(Duration::from_millis(120)).await;
    assert!(entry.cached_screen.read().await.is_none(), "nothing cached from dead upstream");
    cancel.cancel();
    Ok(())
}
