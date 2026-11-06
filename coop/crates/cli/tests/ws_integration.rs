// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! WebSocket integration tests using real connections against an in-process
//! axum server.

use std::sync::Arc;
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use tokio_tungstenite::tungstenite::Message as WsMessage;

use coop::driver::AgentState;
use coop::event::{OutputEvent, TransitionEvent};
use coop::test_support::{spawn_http_server, StoreBuilder, StoreCtx};

type WsStream =
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>;
type WsTx = futures_util::stream::SplitSink<WsStream, WsMessage>;
type WsRx = futures_util::stream::SplitStream<WsStream>;

/// Send a JSON message over the WebSocket.
async fn ws_send(stream: &mut WsTx, value: &serde_json::Value) -> anyhow::Result<()> {
    let text = serde_json::to_string(value)?;
    stream.send(WsMessage::Text(text.into())).await.map_err(|e| anyhow::anyhow!("ws send: {e}"))?;
    Ok(())
}

/// Receive a JSON message from the WebSocket with timeout.
async fn ws_recv(stream: &mut WsRx, timeout: Duration) -> anyhow::Result<serde_json::Value> {
    let msg = tokio::time::timeout(timeout, stream.next())
        .await
        .map_err(|_| anyhow::anyhow!("ws recv timeout"))?
        .ok_or_else(|| anyhow::anyhow!("ws stream closed"))?
        .map_err(|e| anyhow::anyhow!("ws recv: {e}"))?;

    match msg {
        WsMessage::Text(text) => {
            let parsed: serde_json::Value = serde_json::from_str(&text)?;
            Ok(parsed)
        }
        other => anyhow::bail!("expected Text message, got {other:?}"),
    }
}

/// Connect a WebSocket to the given server address with optional query params.
async fn ws_connect(addr: &std::net::SocketAddr, query: &str) -> anyhow::Result<(WsTx, WsRx)> {
    let url = if query.is_empty() {
        format!("ws://{addr}/ws")
    } else {
        format!("ws://{addr}/ws?{query}")
    };
    let (stream, _) = tokio_tungstenite::connect_async(&url)
        .await
        .map_err(|e| anyhow::anyhow!("ws connect: {e}"))?;
    Ok(stream.split())
}

const RECV_TIMEOUT: Duration = Duration::from_secs(5);

#[tokio::test]
async fn ws_connect_and_receive_pong() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let (addr, _handle) = spawn_http_server(store).await?;

    let (mut tx, mut rx) = ws_connect(&addr, "").await?;

    // Send Ping
    ws_send(&mut tx, &serde_json::json!({"event": "ping"})).await?;

    // Should receive Pong
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(resp.get("event").and_then(|t| t.as_str()), Some("pong"), "response: {resp}");

    Ok(())
}

#[tokio::test]
async fn ws_auth_query_param() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().auth_token("test-secret").build();
    let (addr, _handle) = spawn_http_server(store).await?;

    // Connect with correct token
    let (mut tx, mut rx) = ws_connect(&addr, "token=test-secret").await?;
    ws_send(&mut tx, &serde_json::json!({"event": "ping"})).await?;
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(resp.get("event").and_then(|t| t.as_str()), Some("pong"));

    // Connect with wrong token — should get 401 HTTP response (connection refused)
    let url = format!("ws://{addr}/ws?token=wrong");
    let result = tokio_tungstenite::connect_async(&url).await;
    assert!(result.is_err(), "should reject connection with wrong token");

    Ok(())
}

#[tokio::test]
async fn ws_auth_message() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().auth_token("auth-secret").build();
    let (addr, _handle) = spawn_http_server(store).await?;

    // Connect without token (WS upgrade succeeds; needs auth via message)
    let (mut tx, mut rx) = ws_connect(&addr, "").await?;

    // Send wrong auth — should get error
    ws_send(&mut tx, &serde_json::json!({"event": "auth", "token": "wrong"})).await?;
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(resp.get("event").and_then(|t| t.as_str()), Some("error"), "wrong auth: {resp}");

    // Send correct auth — should succeed (no error response)
    ws_send(&mut tx, &serde_json::json!({"event": "auth", "token": "auth-secret"})).await?;

    // Verify subsequent operations work (ping/pong)
    ws_send(&mut tx, &serde_json::json!({"event": "ping"})).await?;
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(resp.get("event").and_then(|t| t.as_str()), Some("pong"));

    Ok(())
}

#[tokio::test]
async fn ws_subscription_mode_raw() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;

    let (mut _tx, mut rx) = ws_connect(&addr, "subscribe=pty").await?;

    // Push raw output via broadcast
    let data = bytes::Bytes::from("hello raw");
    let offset;
    {
        let mut ring = store.terminal.ring.write().await;
        ring.write(&data);
        offset = ring.total_written() - data.len() as u64;
    }
    let _ = store.channels.output_tx.send(OutputEvent::Raw { data, offset });

    // Should receive Output message
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(
        resp.get("event").and_then(|t| t.as_str()),
        Some("pty"),
        "pty mode should receive pty event: {resp}"
    );

    // Push a ScreenUpdate — should NOT be forwarded in raw mode
    let snapshot = store.terminal.screen.read().await.snapshot_text();
    let _ = store.channels.screen_tx.send(snapshot);

    // Try to read — should timeout (no message)
    let result =
        tokio::time::timeout(Duration::from_millis(200), ws_recv(&mut rx, RECV_TIMEOUT)).await;
    assert!(result.is_err(), "raw mode should not receive screen updates");

    Ok(())
}

#[tokio::test]
async fn ws_subscription_mode_state() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;

    let (mut _tx, mut rx) = ws_connect(&addr, "subscribe=state").await?;

    // First frame: initial state snapshot sent on connect.
    let initial = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(
        initial.get("event").and_then(|t| t.as_str()),
        Some("transition"),
        "initial state should be a transition frame: {initial}"
    );
    assert_eq!(initial.get("next").and_then(|n| n.as_str()), Some("starting"));

    // Push state change
    let _ = store.channels.state_tx.send(TransitionEvent {
        prev: AgentState::Starting,
        next: AgentState::Working,
        seq: 1,
        cause: String::new(),
        last_message: None,
    });

    // Should receive Transition
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(
        resp.get("event").and_then(|t| t.as_str()),
        Some("transition"),
        "state mode should receive state changes: {resp}"
    );
    assert_eq!(resp.get("next").and_then(|n| n.as_str()), Some("working"));

    // Push raw output — should NOT be forwarded in state mode
    let _ = store
        .channels
        .output_tx
        .send(OutputEvent::Raw { data: bytes::Bytes::from("ignored"), offset: 0 });

    let result =
        tokio::time::timeout(Duration::from_millis(200), ws_recv(&mut rx, RECV_TIMEOUT)).await;
    assert!(result.is_err(), "state mode should not receive raw output");

    Ok(())
}

#[tokio::test]
async fn ws_subscription_mode_screen() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;

    let (mut _tx, mut rx) = ws_connect(&addr, "subscribe=screen").await?;

    // Push screen update
    store.terminal.screen.write().await.feed(b"screen update");
    let snapshot = store.terminal.screen.read().await.snapshot_text();
    let _ = store.channels.screen_tx.send(snapshot);

    // Should receive Screen message
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(
        resp.get("event").and_then(|t| t.as_str()),
        Some("screen"),
        "screen mode should receive screen updates: {resp}"
    );

    Ok(())
}

/// WebSocket screen-stream broadcast-lag recovery (`ws.rs` `handle_connection`
/// screen arm). Floods the screen broadcast channel (capacity 16) with far more
/// updates than it can buffer while the client is not draining, forcing the
/// connection loop's `screen_rx.recv()` to return `RecvError::Lagged`. The
/// recovery arm must snapshot the latest screen, resubscribe, and keep the
/// stream live — so a sentinel update pushed after the flood must still be
/// delivered and carry the latest seq. Frames are made large (full 24 lines)
/// so the socket + broadcast buffers overflow deterministically.
#[tokio::test]
async fn ws_screen_stream_recovers_from_broadcast_lag() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;

    let (mut _tx, mut rx) = ws_connect(&addr, "subscribe=screen").await?;

    // A wide payload per frame so each serialized screen message is large,
    // overflowing the socket send buffer and stalling the server task — which
    // is what makes the broadcast channel (cap 16) lag deterministically.
    let wide = "x".repeat(78);

    const FLOOD: usize = 400;
    for i in 0..FLOOD {
        {
            let mut screen = store.terminal.screen.write().await;
            for _ in 0..24 {
                screen.feed(format!("{wide} {i}\r\n").as_bytes());
            }
        }
        let snapshot = store.terminal.screen.read().await.snapshot_text();
        let _ = store.channels.screen_tx.send(snapshot);
    }

    // Sentinel pushed after the flood; carries the highest seq.
    {
        let mut screen = store.terminal.screen.write().await;
        screen.feed(b"sentinel after flood\r\n");
    }
    let sentinel_seq = store.terminal.screen.read().await.seq();
    let sentinel = store.terminal.screen.read().await.snapshot_text();
    let _ = store.channels.screen_tx.send(sentinel);

    // Drain until the sentinel seq is observed. Reaching it proves the loop
    // survived the flood (recovered from lag) and converged to the latest
    // snapshot; a broken Lagged arm would kill the connection and this would
    // time out instead.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
    let mut max_seq = 0u64;
    while max_seq < sentinel_seq {
        let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
        anyhow::ensure!(
            !remaining.is_zero(),
            "screen stream did not converge to sentinel (max_seq={max_seq}, want {sentinel_seq})"
        );
        let resp = ws_recv(&mut rx, remaining).await?;
        assert_eq!(
            resp.get("event").and_then(|t| t.as_str()),
            Some("screen"),
            "every frame in screen mode must be a screen event: {resp}"
        );
        if let Some(seq) = resp.get("seq").and_then(|s| s.as_u64()) {
            max_seq = max_seq.max(seq);
        }
    }
    assert_eq!(max_seq, sentinel_seq, "converged to the post-flood sentinel snapshot");
    Ok(())
}

#[tokio::test]
async fn ws_replay_from_offset() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();

    // Write known data to ring buffer
    {
        let mut ring = store.terminal.ring.write().await;
        ring.write(b"replay-data-here");
    }

    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;
    let (mut tx, mut rx) = ws_connect(&addr, "").await?;

    // Send replay from offset 0
    ws_send(&mut tx, &serde_json::json!({"event": "replay:get", "offset": 0})).await?;

    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(resp.get("event").and_then(|t| t.as_str()), Some("replay"));
    assert_eq!(resp.get("offset").and_then(|o| o.as_u64()), Some(0));

    // Decode data
    let b64 =
        resp.get("data").and_then(|d| d.as_str()).ok_or_else(|| anyhow::anyhow!("missing data"))?;
    let decoded = base64::Engine::decode(&base64::engine::general_purpose::STANDARD, b64)?;
    assert_eq!(decoded, b"replay-data-here");

    Ok(())
}

#[tokio::test]
async fn ws_concurrent_readers() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;

    // Connect 5 clients with state mode
    let mut clients = Vec::new();
    for _ in 0..5 {
        let (tx, rx) = ws_connect(&addr, "subscribe=state").await?;
        clients.push((tx, rx));
    }

    // Drain the initial state frame sent on connect for each client.
    for (_tx, ref mut rx) in &mut clients {
        let initial = ws_recv(rx, RECV_TIMEOUT).await?;
        assert_eq!(initial.get("event").and_then(|t| t.as_str()), Some("transition"));
    }

    // Push one state change
    let _ = store.channels.state_tx.send(TransitionEvent {
        prev: AgentState::Starting,
        next: AgentState::Working,
        seq: 1,
        cause: String::new(),
        last_message: None,
    });

    // All 5 should receive the state change
    for (_tx, ref mut rx) in &mut clients {
        let resp = ws_recv(rx, RECV_TIMEOUT).await?;
        assert_eq!(
            resp.get("event").and_then(|t| t.as_str()),
            Some("transition"),
            "all clients should receive state change"
        );
    }

    Ok(())
}

#[tokio::test]
async fn ws_state_subscribe_sends_initial_exited() -> anyhow::Result<()> {
    // Simulate a client connecting after the process already exited.
    let StoreCtx { store, .. } = StoreBuilder::new()
        .agent_state(AgentState::Exited {
            status: coop::driver::ExitStatus { code: Some(0), signal: None },
        })
        .build();
    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;

    let (mut _tx, mut rx) = ws_connect(&addr, "subscribe=state").await?;

    // First frame should be exit (the current state at connect time).
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(
        resp.get("event").and_then(|t| t.as_str()),
        Some("exit"),
        "late-connecting client should see exit state: {resp}"
    );
    assert_eq!(resp.get("code").and_then(|c| c.as_i64()), Some(0));

    Ok(())
}

#[tokio::test]
async fn ws_resize_sends_event() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().build();
    let (addr, _handle) = spawn_http_server(store).await?;

    let (mut tx, _ws_rx) = ws_connect(&addr, "").await?;

    ws_send(&mut tx, &serde_json::json!({"event": "resize", "cols": 120, "rows": 40})).await?;

    // Verify resize event received
    let event = tokio::time::timeout(Duration::from_secs(2), input_rx.recv()).await?;
    match event {
        Some(coop::event::InputEvent::Resize { cols, rows }) => {
            assert_eq!(cols, 120);
            assert_eq!(rows, 40);
        }
        other => anyhow::bail!("expected Resize event, got {other:?}"),
    }

    Ok(())
}

// -- Transcript WebSocket tests -----------------------------------------------

use coop::transcript::TranscriptState;

fn transcript_store() -> (std::sync::Arc<coop::transport::state::Store>, tempfile::TempDir) {
    let tmp = tempfile::tempdir().expect("create tempdir");
    let ts_dir = tmp.path().join("transcripts");
    let log = tmp.path().join("session.jsonl");
    std::fs::write(&log, "").expect("create session log");

    let ts = std::sync::Arc::new(
        TranscriptState::new(ts_dir, Some(log)).expect("create transcript state"),
    );
    let StoreCtx { store, .. } = StoreBuilder::new().transcript(ts).build();
    (store, tmp)
}

#[tokio::test]
async fn ws_transcript_list_and_get() -> anyhow::Result<()> {
    let (store, tmp) = transcript_store();
    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;

    let (mut tx, mut rx) = ws_connect(&addr, "").await?;

    // List — should be empty.
    ws_send(&mut tx, &serde_json::json!({"event": "transcript:list"})).await?;
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(resp["event"], "transcript:list");
    assert_eq!(resp["transcripts"].as_array().map(|a| a.len()), Some(0));

    // Save a snapshot by writing to the log and calling save directly.
    let log = tmp.path().join("session.jsonl");
    std::fs::write(&log, "{\"msg\":\"hello\"}\n")?;
    store.transcript.save_snapshot().await?;

    // Get transcript 1.
    ws_send(&mut tx, &serde_json::json!({"event": "transcript:get", "number": 1})).await?;
    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(resp["event"], "transcript:content");
    assert_eq!(resp["number"], 1);
    assert!(resp["content"].as_str().unwrap_or("").contains("hello"));

    Ok(())
}

#[tokio::test]
async fn ws_transcript_subscription() -> anyhow::Result<()> {
    let (store, tmp) = transcript_store();
    let (addr, _handle) = spawn_http_server(Arc::clone(&store)).await?;

    // Connect with transcript subscription.
    let (mut _tx, mut rx) = ws_connect(&addr, "subscribe=transcripts").await?;

    // Save a snapshot — should push a transcript:saved message.
    let log = tmp.path().join("session.jsonl");
    std::fs::write(&log, "{\"msg\":\"data\"}\n")?;
    store.transcript.save_snapshot().await?;

    let resp = ws_recv(&mut rx, RECV_TIMEOUT).await?;
    assert_eq!(resp["event"], "transcript:saved", "response: {resp}");
    assert_eq!(resp["number"], 1);
    assert_eq!(resp["line_count"], 1);

    Ok(())
}
