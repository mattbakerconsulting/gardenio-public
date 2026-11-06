// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::Ordering;
use std::sync::Arc;

use bytes::Bytes;

use crate::driver::AgentState;
use crate::event::InputEvent;
use crate::test_support::{StoreBuilder, StoreCtx, StubNudgeEncoder, StubRespondEncoder};

/// Helper: call `handle_input` with a JSON payload and return the received InputEvent.
async fn send_input(payload: &[u8]) -> Option<InputEvent> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().child_pid(1234).build();
    super::handle_input(&store, payload).await;
    input_rx.try_recv().ok()
}

// ── handle_input ──────────────────────────────────────────────────────────

#[tokio::test]
async fn input_text_only() -> anyhow::Result<()> {
    let event = send_input(br#"{"text":"hello"}"#).await;
    match event {
        Some(InputEvent::Write(data)) => {
            assert_eq!(data, Bytes::from("hello"));
        }
        other => anyhow::bail!("expected Write, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn input_text_with_enter() -> anyhow::Result<()> {
    let event = send_input(br#"{"text":"yes","enter":true}"#).await;
    match event {
        Some(InputEvent::Write(data)) => {
            assert_eq!(data, Bytes::from("yes\r"));
        }
        other => anyhow::bail!("expected Write, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn input_empty_text_with_enter() -> anyhow::Result<()> {
    let event = send_input(br#"{"text":"","enter":true}"#).await;
    match event {
        Some(InputEvent::Write(data)) => {
            assert_eq!(data, Bytes::from("\r"));
        }
        other => anyhow::bail!("expected Write, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn input_invalid_json_is_ignored() -> anyhow::Result<()> {
    let event = send_input(b"not json").await;
    assert!(event.is_none(), "invalid JSON should not produce an InputEvent");
    Ok(())
}

#[tokio::test]
async fn input_missing_text_field_is_ignored() -> anyhow::Result<()> {
    let event = send_input(br#"{"enter":true}"#).await;
    assert!(event.is_none(), "missing 'text' field should not produce an InputEvent");
    Ok(())
}

// ── handle_nudge ──────────────────────────────────────────────────────────

/// Helper: wait briefly for an InputEvent so detached deliveries have a
/// chance to flush.  `handle_nudge` now returns as soon as the delivery is
/// queued onto a background task (see `NudgeOutcome` doc), so try_recv
/// races the spawn.
async fn recv_with_timeout(rx: &mut tokio::sync::mpsc::Receiver<InputEvent>) -> Option<InputEvent> {
    tokio::time::timeout(std::time::Duration::from_secs(1), rx.recv()).await.ok().flatten()
}

#[tokio::test]
async fn nudge_with_message() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new()
        .child_pid(1234)
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    store.ready.store(true, Ordering::Release);

    super::handle_nudge(&store, br#"{"message":"please continue"}"#).await;

    // StubNudgeEncoder passes through message bytes unchanged.  Delivery is
    // detached onto a background task, so wait briefly for it to flush.
    let event = recv_with_timeout(&mut input_rx).await;
    assert!(event.is_some(), "nudge should have sent an InputEvent");
    Ok(())
}

#[tokio::test]
async fn nudge_empty_message_defaults_to_continue() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new()
        .child_pid(1234)
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    store.ready.store(true, Ordering::Release);

    super::handle_nudge(&store, br#"{}"#).await;

    let event = recv_with_timeout(&mut input_rx).await;
    assert!(event.is_some(), "empty nudge should default to 'continue'");
    Ok(())
}

#[tokio::test]
async fn nudge_invalid_json_uses_default() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new()
        .child_pid(1234)
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    store.ready.store(true, Ordering::Release);

    super::handle_nudge(&store, b"bad json").await;

    let event = recv_with_timeout(&mut input_rx).await;
    assert!(event.is_some(), "invalid JSON nudge should still send 'continue'");
    Ok(())
}

// ── handle_respond ────────────────────────────────────────────────────────

#[tokio::test]
async fn respond_invalid_json_is_ignored() -> anyhow::Result<()> {
    let StoreCtx { store, mut input_rx, .. } =
        StoreBuilder::new().child_pid(1234).respond_encoder(Arc::new(StubRespondEncoder)).build();
    store.ready.store(true, Ordering::Release);

    super::handle_respond(&store, b"not json").await;

    let event = input_rx.try_recv();
    assert!(event.is_err(), "invalid JSON respond should produce no InputEvent");
    Ok(())
}

#[tokio::test]
async fn respond_valid_json_parses_fields() -> anyhow::Result<()> {
    // With no prompt state, respond will fail at the encoder level (no active prompt),
    // but the deserialization path should succeed.
    let StoreCtx { store, .. } =
        StoreBuilder::new().child_pid(1234).respond_encoder(Arc::new(StubRespondEncoder)).build();
    store.ready.store(true, Ordering::Release);

    // This should not panic — it should gracefully handle the "not in prompt" case.
    super::handle_respond(&store, br#"{"accept":true,"option":1,"text":"ok","answers":[]}"#).await;
    Ok(())
}

// ── epoch_ms ──────────────────────────────────────────────────────────────

#[test]
fn epoch_ms_is_reasonable() {
    let ms = super::epoch_ms();
    // Sanity: should be after 2025-01-01 and before 2100-01-01.
    assert!(ms > 1_735_689_600_000, "epoch_ms too small: {ms}");
    assert!(ms < 4_102_444_800_000, "epoch_ms too large: {ms}");
}

// ── boot-path initial-connect retry (ka-0000008143) ────────────────────────

/// A TCP port bound to a listener that is then dropped is the simplest way to
/// get a deterministically-closed loopback port: the OS refuses connects
/// immediately with ECONNREFUSED. Returns the URL to dial.
fn dead_nats_url() -> String {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind ephemeral");
    let port = listener.local_addr().expect("local_addr").port();
    drop(listener); // close it → connects to this port now refuse instantly
    format!("nats://127.0.0.1:{port}")
}

/// Regression for ka-0000008143. The MicroVM relay's `COOP_NATS_URL` rides a
/// Ziti proxy circuit that warms up LAZILY, so coop's boot connect routinely
/// fires against a not-yet-reachable endpoint. `NatsRelay::connect` MUST tolerate
/// that on the boot path (retry_on_initial_connect=true → returns a live client
/// that establishes in the background) rather than erroring single-shot and
/// leaving the session silently off the mux for its whole life.
///
/// This asserts the exact semantic contrast against a deterministically-closed
/// port — no broker, no timing races:
///   - retry_on_initial_connect = false → `connect()` errors (the old bug)
///   - retry_on_initial_connect = true  → `connect()` succeeds (the fix)
#[tokio::test]
async fn relay_connect_retry_on_initial_connect_tolerates_dead_endpoint() -> anyhow::Result<()> {
    use crate::transport::nats::NatsAuth;
    use crate::transport::nats_relay::NatsRelay;

    let url = dead_nats_url();

    // Boot path (the fix): background retry → connect() returns Ok immediately
    // even though nothing is listening yet.
    let ok =
        NatsRelay::connect(&url, "coop.mux", "claude", &[], NatsAuth::default(), None, true).await;
    assert!(
        ok.is_ok(),
        "retry_on_initial_connect=true must return a live client against a dead endpoint, got: {:?}",
        ok.err()
    );

    // Thaw path (fail-closed, unchanged): single-shot → connect() errors so the
    // supervisor's bounded retry loop drives reconnection with a fresh token.
    let err =
        NatsRelay::connect(&url, "coop.mux", "claude", &[], NatsAuth::default(), None, false).await;
    assert!(
        err.is_err(),
        "retry_on_initial_connect=false must error single-shot against a dead endpoint (fail-closed thaw semantics)"
    );

    Ok(())
}
