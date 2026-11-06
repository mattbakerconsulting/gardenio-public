// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! gRPC integration tests using a tonic client against an in-process server.

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use tokio_stream::StreamExt;

use coop::driver::AgentState;
use coop::event::{OutputEvent, TransitionEvent};
use coop::test_support::{spawn_grpc_server, StoreBuilder, StoreCtx, StubNudgeEncoder};
use coop::transport::grpc::proto;

async fn grpc_client(
    store: Arc<coop::transport::Store>,
) -> anyhow::Result<(
    proto::coop_client::CoopClient<tonic::transport::Channel>,
    Arc<coop::transport::Store>,
)> {
    let (addr, _handle) = spawn_grpc_server(Arc::clone(&store)).await?;
    let endpoint = tonic::transport::Channel::from_shared(format!("http://{addr}"))
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    let channel = endpoint.connect().await.map_err(|e| anyhow::anyhow!("grpc connect: {e}"))?;
    Ok((proto::coop_client::CoopClient::new(channel), store))
}

#[tokio::test]
async fn grpc_get_health() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().child_pid(42).build();
    let (mut client, _state) = grpc_client(store).await?;

    let resp = client.get_health(proto::GetHealthRequest {}).await?.into_inner();
    assert_eq!(resp.status, "running");
    assert_eq!(resp.pid, Some(42));
    assert_eq!(resp.agent, "unknown");
    assert_eq!(resp.ws_clients, 0);

    Ok(())
}

#[tokio::test]
async fn grpc_get_screen() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let (mut client, _state) = grpc_client(store).await?;

    // With cursor
    let resp = client.get_screen(proto::GetScreenRequest { cursor: true }).await?.into_inner();
    assert_eq!(resp.cols, 80);
    assert_eq!(resp.rows, 24);
    assert!(!resp.alt_screen);
    assert!(resp.cursor.is_some());

    // Without cursor
    let resp2 = client.get_screen(proto::GetScreenRequest { cursor: false }).await?.into_inner();
    assert!(resp2.cursor.is_none());

    Ok(())
}

#[tokio::test]
async fn grpc_get_status() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let (mut client, _state) = grpc_client(store).await?;

    let resp = client.get_status(proto::GetStatusRequest {}).await?.into_inner();
    assert_eq!(resp.state, "starting");
    assert_eq!(resp.ws_clients, 0);
    assert_eq!(resp.bytes_written, 0);

    Ok(())
}

#[tokio::test]
async fn grpc_send_input() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().build();
    let (mut client, _state) = grpc_client(store).await?;

    let resp = client
        .send_input(proto::SendInputRequest { text: "hello".to_owned(), enter: true })
        .await?
        .into_inner();
    assert_eq!(resp.bytes_written, 6); // "hello" + "\r"

    // Verify input event received
    let event = tokio::time::timeout(Duration::from_secs(2), input_rx.recv()).await?;
    match event {
        Some(coop::event::InputEvent::Write(data)) => {
            assert_eq!(&data[..], b"hello\r");
        }
        other => anyhow::bail!("expected Write event, got {other:?}"),
    }

    Ok(())
}

#[tokio::test]
async fn grpc_send_keys() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().build();
    let (mut client, _state) = grpc_client(store).await?;

    let resp = client
        .send_keys(proto::SendKeysRequest { keys: vec!["Enter".to_owned(), "Tab".to_owned()] })
        .await?
        .into_inner();
    assert_eq!(resp.bytes_written, 2); // \r + \t

    let event = tokio::time::timeout(Duration::from_secs(2), input_rx.recv()).await?;
    match event {
        Some(coop::event::InputEvent::Write(data)) => {
            assert_eq!(&data[..], b"\r\t");
        }
        other => anyhow::bail!("expected Write event with keys, got {other:?}"),
    }

    Ok(())
}

#[tokio::test]
async fn grpc_resize() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().build();
    let (mut client, _state) = grpc_client(store).await?;

    let resp = client.resize(proto::ResizeRequest { cols: 120, rows: 40 }).await?.into_inner();
    assert_eq!(resp.cols, 120);
    assert_eq!(resp.rows, 40);

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

#[tokio::test]
async fn grpc_send_signal() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().build();
    let (mut client, _state) = grpc_client(store).await?;

    let resp = client
        .send_signal(proto::SendSignalRequest { signal: "SIGINT".to_owned() })
        .await?
        .into_inner();
    assert!(resp.delivered);

    let event = tokio::time::timeout(Duration::from_secs(2), input_rx.recv()).await?;
    assert!(
        matches!(event, Some(coop::event::InputEvent::Signal(coop::event::PtySignal::Int))),
        "expected Signal(Int), got {event:?}"
    );

    Ok(())
}

#[tokio::test]
async fn grpc_stream_output() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let (mut client, state) = grpc_client(store).await?;

    let mut stream =
        client.stream_output(proto::StreamOutputRequest { from_offset: 0 }).await?.into_inner();

    // Write data to ring + broadcast
    let data = bytes::Bytes::from("stream-test-data");
    let offset;
    {
        let mut ring = state.terminal.ring.write().await;
        ring.write(&data);
        offset = ring.total_written() - data.len() as u64;
    }
    let _ = state.channels.output_tx.send(OutputEvent::Raw { data: data.clone(), offset });

    // Read from stream
    let chunk = tokio::time::timeout(Duration::from_secs(5), stream.next())
        .await?
        .ok_or_else(|| anyhow::anyhow!("stream ended"))?
        .map_err(|e| anyhow::anyhow!("stream error: {e}"))?;

    assert_eq!(&chunk.data, b"stream-test-data");

    Ok(())
}

#[tokio::test]
async fn grpc_stream_agent() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let (mut client, state) = grpc_client(store).await?;

    let mut stream = client.stream_agent(proto::StreamAgentRequest {}).await?.into_inner();

    // Send state change
    let _ = state.channels.state_tx.send(TransitionEvent {
        prev: AgentState::Starting,
        next: AgentState::Working,
        seq: 1,
        cause: String::new(),
        last_message: None,
    });

    let event = tokio::time::timeout(Duration::from_secs(5), stream.next())
        .await?
        .ok_or_else(|| anyhow::anyhow!("stream ended"))?
        .map_err(|e| anyhow::anyhow!("stream error: {e}"))?;

    assert_eq!(event.prev, "starting");
    assert_eq!(event.next, "working");
    assert_eq!(event.seq, 1);

    Ok(())
}

#[tokio::test]
async fn grpc_stream_screen() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let (mut client, state) = grpc_client(store).await?;

    let mut stream = client.stream_screen(proto::StreamScreenRequest {}).await?.into_inner();

    // Push screen update
    let snapshot = state.terminal.screen.read().await.snapshot_text();
    let _ = state.channels.screen_tx.send(snapshot);

    let snap = tokio::time::timeout(Duration::from_secs(5), stream.next())
        .await?
        .ok_or_else(|| anyhow::anyhow!("stream ended"))?
        .map_err(|e| anyhow::anyhow!("stream error: {e}"))?;

    assert_eq!(snap.cols, 80);
    assert_eq!(snap.rows, 24);

    Ok(())
}

/// gRPC screen-stream broadcast-lag recovery (`service.rs` `stream_screen`).
///
/// Floods the screen broadcast channel (capacity 16) far faster than the
/// unread client drains it, forcing the server task's `recv()` to return
/// `RecvError::Lagged`. The recovery arm must read the latest live snapshot,
/// resubscribe, and keep the stream alive — so a sentinel update pushed after
/// the flood must still be delivered. If the `Lagged` arm broke the loop (the
/// regression this guards), the stream would die and the sentinel would never
/// arrive.
#[tokio::test]
async fn grpc_stream_screen_recovers_from_broadcast_lag() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let (mut client, state) = grpc_client(store).await?;

    let mut stream = client.stream_screen(proto::StreamScreenRequest {}).await?.into_inner();

    // Flood: many more updates than the broadcast can buffer (cap 16), while
    // the client is not yet reading, so the server task falls behind and its
    // next recv observes Lagged.
    const FLOOD: usize = 1000;
    for i in 0..FLOOD {
        {
            let mut screen = state.terminal.screen.write().await;
            screen.feed(format!("flood frame {i}\r\n").as_bytes());
        }
        let snapshot = state.terminal.screen.read().await.snapshot_text();
        let _ = state.channels.screen_tx.send(snapshot);
    }

    // Sentinel update pushed AFTER the flood; its seq is the highest.
    {
        let mut screen = state.terminal.screen.write().await;
        screen.feed(b"sentinel after flood\r\n");
    }
    let sentinel_seq = state.terminal.screen.read().await.seq();
    let sentinel = state.terminal.screen.read().await.snapshot_text();
    let _ = state.channels.screen_tx.send(sentinel);

    // Drain the stream until we observe the sentinel seq. Reaching it proves
    // the stream survived the flood (recovered from lag) and converged to the
    // latest snapshot. Bounded by an overall deadline so a broken (dead)
    // stream fails fast instead of hanging.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
    let mut max_seq = 0u64;
    while max_seq < sentinel_seq {
        let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
        anyhow::ensure!(!remaining.is_zero(), "stream did not converge to sentinel (max_seq={max_seq}, want {sentinel_seq}) — lag recovery likely broke the stream");
        let snap = tokio::time::timeout(remaining, stream.next())
            .await
            .map_err(|_| anyhow::anyhow!("timed out draining screen stream at max_seq={max_seq}"))?
            .ok_or_else(|| {
                anyhow::anyhow!("screen stream ended before sentinel (max_seq={max_seq})")
            })?
            .map_err(|e| anyhow::anyhow!("stream error: {e}"))?;
        max_seq = max_seq.max(snap.seq);
    }
    assert_eq!(max_seq, sentinel_seq, "converged to the post-flood sentinel snapshot");
    Ok(())
}

#[tokio::test]
async fn grpc_nudge_not_ready() -> anyhow::Result<()> {
    let StoreCtx { store, .. } =
        StoreBuilder::new().nudge_encoder(Arc::new(StubNudgeEncoder)).build();
    // NOT marking ready
    let (mut client, _state) = grpc_client(store).await?;

    let result = client.nudge(proto::NudgeRequest { message: "hello".to_owned() }).await;
    let err = result.err().ok_or_else(|| anyhow::anyhow!("expected error"))?;
    assert_eq!(err.code(), tonic::Code::Unavailable);

    Ok(())
}

#[tokio::test]
async fn grpc_nudge_no_driver() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    store.ready.store(true, Ordering::Release);
    let (mut client, _state) = grpc_client(store).await?;

    let result = client.nudge(proto::NudgeRequest { message: "hello".to_owned() }).await;
    let err = result.err().ok_or_else(|| anyhow::anyhow!("expected error"))?;
    assert_eq!(err.code(), tonic::Code::Unimplemented);

    Ok(())
}

#[tokio::test]
async fn grpc_nudge_agent_working() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new()
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .agent_state(AgentState::Working)
        .build();
    store.ready.store(true, Ordering::Release);
    let (mut client, _state) = grpc_client(store).await?;

    // Nudges are accepted in Working state — Claude picks up the message
    // when the current generation finishes.
    let resp =
        client.nudge(proto::NudgeRequest { message: "hello".to_owned() }).await?.into_inner();
    assert!(resp.delivered);
    assert!(resp.reason.is_none());

    Ok(())
}
