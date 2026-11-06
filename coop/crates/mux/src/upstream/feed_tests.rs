// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::AtomicU32;
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;

use super::*;
use crate::state::{MuxEvent, SessionEntry, SessionTransport};

/// A TCP listener that accepts connections but never completes the WebSocket
/// handshake — the parked / half-open upstream the connect-timeout defends
/// against. Accepted sockets are held (moved into a Vec) so the peer stays in
/// the TLS/HTTP-Upgrade wait rather than seeing a RST.
async fn stalled_upstream() -> anyhow::Result<(String, CancellationToken)> {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let stop = CancellationToken::new();
    let stop_child = stop.clone();
    tokio::spawn(async move {
        let mut held = Vec::new();
        loop {
            tokio::select! {
                _ = stop_child.cancelled() => break,
                accepted = listener.accept() => {
                    if let Ok((sock, _)) = accepted {
                        held.push(sock); // never read/write → handshake stalls
                    }
                }
            }
        }
    });
    Ok((format!("http://{addr}"), stop))
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

/// Fault injection: point the feed at an upstream that accepts TCP but never
/// completes the WS handshake, then cancel. The feed must terminate promptly and
/// emit `SessionOffline`.
///
/// Pre-fix, `connect_async(&ws_url).await` was a bare await outside any
/// `select!`, so a stalled handshake blocked the task forever — it never polled
/// the cancel token, so this test hangs (the spawned task is uncancellable).
/// Post-fix the handshake is raced against `cancel.cancelled()`, so cancel wins
/// immediately.
#[tokio::test]
async fn feed_cancel_terminates_task_stuck_in_handshake() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();

    let (url, listener_stop) = stalled_upstream().await?;
    let entry = entry_with_url("s-stalled", url);
    let cancel = entry.cancel.clone();

    let (event_tx, mut event_rx) = broadcast::channel::<MuxEvent>(16);
    spawn_event_feed(event_tx, Arc::clone(&entry), cancel.clone());

    // Give the feed a moment to reach the (stalling) handshake, then cancel.
    tokio::time::sleep(Duration::from_millis(100)).await;
    cancel.cancel();

    // The task must observe the cancellation and emit SessionOffline well within
    // the 10s CONNECT_TIMEOUT — proving it did not sit blocked in the handshake.
    let offline = tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            match event_rx.recv().await {
                Ok(MuxEvent::SessionOffline { session }) => return Some(session),
                Ok(_) => continue,
                Err(_) => return None,
            }
        }
    })
    .await;

    listener_stop.cancel();

    let session = offline
        .map_err(|_| {
            anyhow::anyhow!("feed did not terminate after cancel — task stuck in handshake")
        })?
        .ok_or_else(|| anyhow::anyhow!("event channel closed before SessionOffline"))?;
    assert_eq!(session, "s-stalled");
    Ok(())
}
