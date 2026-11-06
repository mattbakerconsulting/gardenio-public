// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Integration tests for the session loop + HTTP transport, exercising
//! the full stack in-process via `axum_test::TestServer`.

use std::sync::Arc;

use axum::http::StatusCode;
use bytes::Bytes;
use tokio_util::sync::CancellationToken;

use coop::backend::spawn::NativePty;
use coop::config::Config;
use coop::driver::AgentState;
use coop::event::InputEvent;
use coop::session::{Session, SessionConfig};
use coop::test_support::{StoreBuilder, StoreCtx};
use coop::transport::build_router;
use coop::transport::handler::SessionStatus;
use coop::transport::http::{HealthResponse, InputRequest, ScreenResponse};

#[tokio::test]
async fn session_echo_captures_output_and_exits_zero() -> anyhow::Result<()> {
    let config = Config::test();
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();

    let backend = NativePty::spawn(&["echo".into(), "integration".into()], 80, 24, &[])?;
    let session = Session::new(&config, SessionConfig::new(Arc::clone(&store), backend));

    let status = session.run_to_exit(&config, &mut input_rx).await?;
    assert_eq!(status.code, Some(0));

    // Ring should contain output
    let ring = store.terminal.ring.read().await;
    assert!(ring.total_written() > 0);
    let (a, b) = ring.read_from(0).ok_or(anyhow::anyhow!("no ring data"))?;
    let mut data = a.to_vec();
    data.extend_from_slice(b);
    let text = String::from_utf8_lossy(&data);
    assert!(text.contains("integration"), "ring: {text:?}");

    // Screen should contain output
    let screen = store.terminal.screen.read().await;
    let snap = screen.snapshot();
    let lines = snap.lines.join("\n");
    assert!(lines.contains("integration"), "screen: {lines:?}");

    Ok(())
}

#[tokio::test]
async fn session_input_roundtrip() -> anyhow::Result<()> {
    let config = Config::test();
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();
    let input_tx = store.channels.input_tx.clone();

    let backend = NativePty::spawn(&["/bin/cat".into()], 80, 24, &[])?;
    let session = Session::new(&config, SessionConfig::new(Arc::clone(&store), backend));

    let session_handle = tokio::spawn(async move {
        let config = Config::test();
        session.run_to_exit(&config, &mut input_rx).await
    });

    // Send input via the channel (simulating transport layer)
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    input_tx.send(InputEvent::Write(Bytes::from_static(b"roundtrip\n"))).await?;
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;

    // Send Ctrl-D to close cat
    input_tx.send(InputEvent::Write(Bytes::from_static(b"\x04"))).await?;
    drop(input_tx);

    let status = session_handle.await??;
    assert_eq!(status.code, Some(0));

    // Verify output captured in ring
    let ring = store.terminal.ring.read().await;
    let (a, b) = ring.read_from(0).ok_or(anyhow::anyhow!("no ring data"))?;
    let mut data = a.to_vec();
    data.extend_from_slice(b);
    let text = String::from_utf8_lossy(&data);
    assert!(text.contains("roundtrip"), "ring: {text:?}");

    Ok(())
}

#[tokio::test]
async fn session_shutdown_terminates_child() -> anyhow::Result<()> {
    let config = Config::test();
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();
    let shutdown = CancellationToken::new();

    let backend =
        NativePty::spawn(&["/bin/sh".into(), "-c".into(), "sleep 60".into()], 80, 24, &[])?;
    let session =
        Session::new(&config, SessionConfig::new(store, backend).with_shutdown(shutdown.clone()));

    // Cancel after a short delay
    tokio::spawn(async move {
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        shutdown.cancel();
    });

    let status = session.run_to_exit(&config, &mut input_rx).await?;
    assert!(status.code.is_some() || status.signal.is_some(), "expected exit: {status:?}");
    Ok(())
}

#[tokio::test]
async fn session_exited_state_broadcast() -> anyhow::Result<()> {
    let config = Config::test();
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();

    let backend = NativePty::spawn(&["true".into()], 80, 24, &[])?;
    let session = Session::new(&config, SessionConfig::new(Arc::clone(&store), backend));

    let _ = session.run_to_exit(&config, &mut input_rx).await?;

    // After run(), agent_state should be Exited
    let agent = store.driver.agent_state.read().await;
    match &*agent {
        AgentState::Exited { status } => {
            assert_eq!(status.code, Some(0));
        }
        other => {
            anyhow::bail!("expected Exited state, got {:?}", other.as_str());
        }
    }
    Ok(())
}

#[tokio::test]
async fn http_health_endpoint() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    let resp = server.get("/api/v1/health").await;
    resp.assert_status(StatusCode::OK);
    let health: HealthResponse = resp.json();
    assert_eq!(health.status, "running");
    assert_eq!(health.agent, "unknown");
    assert_eq!(health.terminal.cols, 80);
    assert_eq!(health.terminal.rows, 24);
    Ok(())
}

#[tokio::test]
async fn http_status_endpoint() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    let resp = server.get("/api/v1/status").await;
    resp.assert_status(StatusCode::OK);
    let status: SessionStatus = resp.json();
    assert_eq!(status.state, "starting");
    assert_eq!(status.ws_clients, 0);
    Ok(())
}

#[tokio::test]
async fn http_screen_endpoint() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    let resp = server.get("/api/v1/screen").await;
    resp.assert_status(StatusCode::OK);
    let screen: ScreenResponse = resp.json();
    assert_eq!(screen.cols, 80);
    assert_eq!(screen.rows, 24);
    assert!(!screen.alt_screen);
    Ok(())
}

#[tokio::test]
async fn http_screen_text_endpoint() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    let resp = server.get("/api/v1/screen/text").await;
    resp.assert_status(StatusCode::OK);
    let ct_header = resp.header("content-type");
    let ct = ct_header.to_str().unwrap_or("");
    assert_eq!(ct, "text/plain; charset=utf-8");
    Ok(())
}

#[tokio::test]
async fn http_input_endpoint() -> anyhow::Result<()> {
    let StoreCtx { store, input_rx: mut consumer_input_rx, .. } =
        StoreBuilder::new().ring_size(65536).build();
    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    let resp = server
        .post("/api/v1/input")
        .json(&InputRequest { text: "hello".to_owned(), enter: true })
        .await;

    resp.assert_status(StatusCode::OK);

    // Verify the input was received on the channel
    let event = consumer_input_rx.recv().await;
    match event {
        Some(InputEvent::Write(data)) => {
            assert_eq!(&data[..], b"hello\r");
        }
        other => {
            anyhow::bail!("expected Write event, got: {other:?}");
        }
    }
    Ok(())
}

#[tokio::test]
async fn http_nudge_returns_not_ready_before_startup() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    let resp = server
        .post("/api/v1/agent/nudge")
        .json(&serde_json::json!({"message": "do something"}))
        .await;

    // Agent not ready yet -> NOT_READY error (503)
    resp.assert_status(StatusCode::SERVICE_UNAVAILABLE);
    Ok(())
}

#[tokio::test]
async fn http_nudge_returns_no_driver_for_unknown() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    // Mark ready so the not-ready gate is passed
    store.ready.store(true, std::sync::atomic::Ordering::Release);
    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    let resp = server
        .post("/api/v1/agent/nudge")
        .json(&serde_json::json!({"message": "do something"}))
        .await;

    // No nudge encoder configured -> NO_DRIVER error
    resp.assert_status(StatusCode::NOT_FOUND);
    Ok(())
}

#[tokio::test]
async fn http_auth_rejects_bad_token() -> anyhow::Result<()> {
    let StoreCtx { store, .. } =
        StoreBuilder::new().ring_size(65536).auth_token("secret-token").build();

    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    // Health endpoint skips auth
    let resp = server.get("/api/v1/health").await;
    resp.assert_status(StatusCode::OK);

    // No token on protected route -> 401
    let resp = server.get("/api/v1/status").await;
    resp.assert_status(StatusCode::UNAUTHORIZED);

    // Wrong token -> 401
    let resp = server
        .get("/api/v1/status")
        .add_header(
            axum::http::header::AUTHORIZATION,
            axum::http::HeaderValue::from_static("Bearer wrong-token"),
        )
        .await;
    resp.assert_status(StatusCode::UNAUTHORIZED);

    // Correct token -> 200
    let resp = server
        .get("/api/v1/status")
        .add_header(
            axum::http::header::AUTHORIZATION,
            axum::http::HeaderValue::from_static("Bearer secret-token"),
        )
        .await;
    resp.assert_status(StatusCode::OK);

    Ok(())
}

#[tokio::test]
async fn http_agent_state_endpoint() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();
    let router = build_router(store);
    let server = axum_test::TestServer::new(router)?;

    let resp = server.get("/api/v1/agent").await;
    // Without a driver, state is returned normally (Starting/Unknown).
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"state\":"), "body: {body}");
    Ok(())
}

#[tokio::test]
async fn full_stack_echo_screen_via_http() -> anyhow::Result<()> {
    let config = Config::test();
    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new().ring_size(65536).build();

    let backend = NativePty::spawn(&["echo".into(), "fullstack".into()], 80, 24, &[])?;
    let session = Session::new(&config, SessionConfig::new(Arc::clone(&store), backend));

    // Run session to completion
    let _ = session.run_to_exit(&config, &mut input_rx).await?;

    // Now query the HTTP layer
    let router = build_router(Arc::clone(&store));
    let server = axum_test::TestServer::new(router)?;

    let resp = server.get("/api/v1/screen").await;
    resp.assert_status(StatusCode::OK);
    let screen: ScreenResponse = resp.json();
    let lines = screen.lines.join("\n");
    assert!(lines.contains("fullstack"), "screen: {lines:?}");

    // Verify status shows exited
    let resp2 = server.get("/api/v1/status").await;
    resp2.assert_status(StatusCode::OK);
    let status: SessionStatus = resp2.json();
    assert_eq!(status.state, "exited");
    assert_eq!(status.exit_code, Some(0));

    Ok(())
}
