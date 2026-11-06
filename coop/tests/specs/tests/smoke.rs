// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! End-to-end smoke tests that spawn the real `coop` binary and exercise
//! HTTP, WebSocket, gRPC, Unix socket, and health port transports.

use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use tokio_tungstenite::tungstenite::Message;

use coop::transport::grpc::proto;
use coop_specs::{free_port, CoopProcess};

const TIMEOUT: Duration = Duration::from_secs(10);

// -- HTTP (TCP) ---------------------------------------------------------------

#[tokio::test]
async fn http_health() -> anyhow::Result<()> {
    let coop = CoopProcess::start(&["sleep", "10"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let resp: serde_json::Value =
        reqwest::get(format!("{}/api/v1/health", coop.base_url())).await?.json().await?;

    assert_eq!(resp["status"], "running");
    assert_eq!(resp["agent"], "unknown");
    assert!(resp["terminal"]["cols"].is_number());
    assert!(resp["terminal"]["rows"].is_number());
    assert!(resp["pid"].is_number());

    Ok(())
}

#[tokio::test]
async fn http_screen_captures_output() -> anyhow::Result<()> {
    let coop = CoopProcess::start(&["echo", "smoke-marker"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let client = reqwest::Client::new();
    let url = format!("{}/api/v1/screen/text", coop.base_url());
    let deadline = tokio::time::Instant::now() + TIMEOUT;

    loop {
        if tokio::time::Instant::now() > deadline {
            anyhow::bail!("screen never showed expected output");
        }
        let text = client.get(&url).send().await?.text().await?;
        if text.contains("smoke-marker") {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

#[tokio::test]
async fn http_input_roundtrip() -> anyhow::Result<()> {
    let coop = CoopProcess::start(&["cat"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let client = reqwest::Client::new();
    client
        .post(format!("{}/api/v1/input", coop.base_url()))
        .json(&serde_json::json!({ "text": "hello-roundtrip", "enter": true }))
        .send()
        .await?;

    let url = format!("{}/api/v1/screen/text", coop.base_url());
    let deadline = tokio::time::Instant::now() + TIMEOUT;

    loop {
        if tokio::time::Instant::now() > deadline {
            anyhow::bail!("screen never showed input echo");
        }
        let text = client.get(&url).send().await?.text().await?;
        if text.contains("hello-roundtrip") {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

#[tokio::test]
async fn http_shutdown() -> anyhow::Result<()> {
    let mut coop = CoopProcess::start(&["sleep", "60"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let client = reqwest::Client::new();
    let resp: serde_json::Value =
        client.post(format!("{}/api/v1/shutdown", coop.base_url())).send().await?.json().await?;
    assert_eq!(resp["accepted"], true);

    let _status = coop.wait_exit(TIMEOUT).await?;

    Ok(())
}

// -- WebSocket ----------------------------------------------------------------

#[tokio::test]
async fn ws_ping_pong() -> anyhow::Result<()> {
    let coop = CoopProcess::start(&["sleep", "10"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let (mut ws, _) = tokio_tungstenite::connect_async(coop.ws_url()).await?;
    ws.send(Message::Text(r#"{"event":"ping"}"#.into())).await?;

    let msg = tokio::time::timeout(TIMEOUT, ws.next())
        .await?
        .ok_or_else(|| anyhow::anyhow!("ws stream ended"))??;

    let text = match msg {
        Message::Text(t) => t.to_string(),
        other => anyhow::bail!("expected text ws message, got: {other:?}"),
    };
    let parsed: serde_json::Value = serde_json::from_str(&text)?;
    assert_eq!(parsed["event"], "pong");

    Ok(())
}

#[tokio::test]
async fn ws_get_health() -> anyhow::Result<()> {
    let coop = CoopProcess::start(&["sleep", "10"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let (mut ws, _) = tokio_tungstenite::connect_async(coop.ws_url()).await?;
    ws.send(Message::Text(r#"{"event":"health:get"}"#.into())).await?;

    let msg = tokio::time::timeout(TIMEOUT, ws.next())
        .await?
        .ok_or_else(|| anyhow::anyhow!("ws stream ended"))??;

    let text = match msg {
        Message::Text(t) => t.to_string(),
        other => anyhow::bail!("expected text ws message, got: {other:?}"),
    };
    let parsed: serde_json::Value = serde_json::from_str(&text)?;
    assert_eq!(parsed["event"], "health");
    assert_eq!(parsed["status"], "running");
    assert_eq!(parsed["agent"], "unknown");

    Ok(())
}

// -- gRPC ---------------------------------------------------------------------

#[tokio::test]
async fn grpc_health() -> anyhow::Result<()> {
    let coop = CoopProcess::build().grpc().spawn(&["sleep", "10"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let endpoint = tonic::transport::Channel::from_shared(coop.grpc_url())
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    let channel = endpoint.connect().await.map_err(|e| anyhow::anyhow!("grpc connect: {e}"))?;
    let mut client = proto::coop_client::CoopClient::new(channel);

    let resp = client.get_health(proto::GetHealthRequest {}).await?.into_inner();
    assert_eq!(resp.status, "running");
    assert_eq!(resp.agent, "unknown");
    assert!(resp.terminal_cols > 0);
    assert!(resp.terminal_rows > 0);

    Ok(())
}

#[tokio::test]
async fn grpc_screen() -> anyhow::Result<()> {
    let coop = CoopProcess::build().grpc().spawn(&["echo", "grpc-screen-test"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let endpoint = tonic::transport::Channel::from_shared(coop.grpc_url())
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    let channel = endpoint.connect().await.map_err(|e| anyhow::anyhow!("grpc connect: {e}"))?;
    let mut client = proto::coop_client::CoopClient::new(channel);

    let deadline = tokio::time::Instant::now() + TIMEOUT;
    loop {
        if tokio::time::Instant::now() > deadline {
            anyhow::bail!("grpc screen never showed expected output");
        }
        let resp = client.get_screen(proto::GetScreenRequest { cursor: false }).await?.into_inner();
        let text = resp.lines.join("\n");
        if text.contains("grpc-screen-test") {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

// -- Unix socket --------------------------------------------------------------

#[tokio::test]
async fn socket_health() -> anyhow::Result<()> {
    let coop = CoopProcess::build().no_tcp().socket().spawn(&["sleep", "10"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let socket_path = coop.socket_path().ok_or_else(|| anyhow::anyhow!("no socket path"))?;
    let body = coop_specs::unix_http_get(socket_path, "/api/v1/health").await?;
    let resp: serde_json::Value = serde_json::from_str(&body)?;

    assert_eq!(resp["status"], "running");
    assert_eq!(resp["agent"], "unknown");

    Ok(())
}

#[tokio::test]
async fn socket_screen() -> anyhow::Result<()> {
    let coop = CoopProcess::build().no_tcp().socket().spawn(&["echo", "socket-marker"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let socket_path = coop.socket_path().ok_or_else(|| anyhow::anyhow!("no socket path"))?;
    let deadline = tokio::time::Instant::now() + TIMEOUT;

    loop {
        if tokio::time::Instant::now() > deadline {
            anyhow::bail!("socket screen never showed expected output");
        }
        let body = coop_specs::unix_http_get(socket_path, "/api/v1/screen/text").await?;
        if body.contains("socket-marker") {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

// -- Health port --------------------------------------------------------------

#[tokio::test]
async fn health_port_serves_health() -> anyhow::Result<()> {
    let coop = CoopProcess::build().health().spawn(&["sleep", "10"])?;
    coop.wait_healthy(TIMEOUT).await?;

    // Health endpoint works on the health port
    let resp: serde_json::Value =
        reqwest::get(format!("{}/api/v1/health", coop.health_url())).await?.json().await?;
    assert_eq!(resp["status"], "running");

    Ok(())
}

#[tokio::test]
async fn health_port_rejects_other_routes() -> anyhow::Result<()> {
    let coop = CoopProcess::build().health().spawn(&["sleep", "10"])?;
    coop.wait_healthy(TIMEOUT).await?;

    // Screen endpoint should 404 on the health-only port
    let resp = reqwest::get(format!("{}/api/v1/screen", coop.health_url())).await?;
    assert_eq!(resp.status().as_u16(), 404);

    // Input endpoint should 404 on the health-only port
    let resp = reqwest::Client::new()
        .post(format!("{}/api/v1/input", coop.health_url()))
        .json(&serde_json::json!({ "text": "x" }))
        .send()
        .await?;
    assert_eq!(resp.status().as_u16(), 404);

    Ok(())
}

// -- NATS -----------------------------------------------------------------

use std::process::{Child, Command, Stdio};

/// Start nats-server on a free port, returning the child and the port.
/// Returns None if nats-server is not installed.
fn try_start_nats() -> Option<(Child, u16)> {
    let port = free_port().ok()?;
    let child = Command::new("nats-server")
        .args(["-p", &port.to_string()])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;
    Some((child, port))
}

#[tokio::test]
async fn nats_receives_state_transitions() -> anyhow::Result<()> {
    use futures_util::StreamExt;

    // Skip if nats-server not available.
    let Some((mut nats_proc, nats_port)) = try_start_nats() else { return Ok(()) };
    let nats_url = format!("nats://127.0.0.1:{nats_port}");

    // Wait for nats-server to accept connections.
    let client = {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        loop {
            if tokio::time::Instant::now() > deadline {
                let _ = nats_proc.kill();
                anyhow::bail!("nats-server did not become ready");
            }
            match async_nats::connect(&nats_url).await {
                Ok(c) => break c,
                Err(_) => tokio::time::sleep(Duration::from_millis(50)).await,
            }
        }
    };
    let mut sub = client.subscribe("coop.events.state").await?;

    // Spawn coop wrapping a short-lived command.
    let coop = CoopProcess::build().nats(&nats_url).spawn(&["echo", "nats-test"])?;
    coop.wait_healthy(TIMEOUT).await?;

    // Expect at least one state event (transition or exit).
    let msg = tokio::time::timeout(TIMEOUT, sub.next())
        .await?
        .ok_or_else(|| anyhow::anyhow!("no NATS message"))?;
    let event: serde_json::Value = serde_json::from_slice(&msg.payload)?;
    let is_transition =
        event["seq"].is_u64() && event["prev"].is_string() && event["next"].is_string();
    let is_exit = event["event"].as_str() == Some("exit");
    assert!(is_transition || is_exit, "expected transition or exit event, got: {event}");
    // Identity fields are injected into every NATS payload.
    assert!(event["session_id"].is_string(), "expected session_id in NATS payload");

    let _ = nats_proc.kill();
    Ok(())
}
