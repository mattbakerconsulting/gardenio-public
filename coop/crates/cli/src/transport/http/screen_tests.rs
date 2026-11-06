// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use axum::http::StatusCode;

use crate::event::InputEvent;
use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::build_router;

fn test_state() -> StoreCtx {
    StoreBuilder::new().child_pid(1234).build()
}

#[tokio::test]
async fn health_200() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/health").await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"status\":\"running\""));
    assert!(body.contains("\"pid\":1234"));
    // Flat terminal dimensions (matching gRPC)
    assert!(body.contains("\"terminal_cols\":"), "body: {body}");
    assert!(body.contains("\"terminal_rows\":"), "body: {body}");
    // Nested terminal (backward compat)
    assert!(body.contains("\"terminal\":{"), "body: {body}");
    Ok(())
}

#[tokio::test]
async fn health_account_name_from_active_profile() -> anyhow::Result<()> {
    use crate::profile::ProfileEntry;

    let StoreCtx { store: state, .. } = test_state();
    // Register profiles — the first entry becomes Active.
    state
        .profile
        .register(vec![ProfileEntry {
            name: "live-profile".to_owned(),
            credentials: Default::default(),
            usable: None,
        }])
        .await;

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/health").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["account_name"], "live-profile");
    Ok(())
}

#[tokio::test]
async fn health_account_name_omitted_when_no_active_profile() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/health").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body.get("account_name").is_none() || body["account_name"].is_null());
    Ok(())
}

#[tokio::test]
async fn screen_snapshot() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/screen").await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"cols\":80"));
    assert!(body.contains("\"rows\":24"));
    Ok(())
}

#[tokio::test]
async fn screen_include_cursor() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Default: cursor is null
    let resp = server.get("/api/v1/screen").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body["cursor"].is_null(), "cursor should be null by default");

    // cursor=true: cursor is an object
    let resp = server.get("/api/v1/screen?cursor=true").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body["cursor"].is_object(), "cursor should be an object when requested");

    // Backward compat: cursor=true alias
    let resp = server.get("/api/v1/screen?cursor=true").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body["cursor"].is_object(), "cursor alias should work");
    Ok(())
}

#[tokio::test]
async fn screen_text_plain() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/screen/text").await;
    resp.assert_status(StatusCode::OK);
    let content_type =
        resp.headers().get("content-type").and_then(|v| v.to_str().ok()).unwrap_or("");
    assert!(content_type.contains("text/plain"));
    Ok(())
}

#[tokio::test]
async fn output_with_offset() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    {
        let mut ring = state.terminal.ring.write().await;
        ring.write(b"hello world");
    }
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/output?offset=0").await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"total_written\":11"));
    Ok(())
}

#[tokio::test]
async fn status_running() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/status").await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"state\":\"starting\""));
    assert!(body.contains("\"uptime_secs\":"), "body: {body}");
    Ok(())
}

#[tokio::test]
async fn input_sends_event() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/input")
        .json(&serde_json::json!({"text": "hello", "enter": true}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"bytes_written\":6"));

    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(_))));
    Ok(())
}

#[tokio::test]
async fn input_raw_sends_event() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // "hello" base64-encoded
    let resp =
        server.post("/api/v1/input/raw").json(&serde_json::json!({"data": "aGVsbG8="})).await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"bytes_written\":5"), "body: {body}");

    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(_))));
    Ok(())
}

#[tokio::test]
async fn input_raw_rejects_bad_base64() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/input/raw")
        .json(&serde_json::json!({"data": "not-valid-base64!!!"}))
        .await;
    resp.assert_status(StatusCode::BAD_REQUEST);
    Ok(())
}

#[tokio::test]
async fn keys_sends_event() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/input/keys")
        .json(&serde_json::json!({"keys": ["Escape", "Enter", "Ctrl-C"]}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"bytes_written\":3"));

    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(_))));
    Ok(())
}

#[tokio::test]
async fn mouse_sends_a_report_only_while_the_app_tracks_it() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = test_state();
    let terminal = std::sync::Arc::clone(&state.terminal);
    let server = axum_test::TestServer::new(build_router(state)).anyhow()?;
    let click = serde_json::json!({"button": "left", "col": 3, "row": 2});

    let resp = server.post("/api/v1/input/mouse").json(&click).await;
    resp.assert_status(StatusCode::OK);
    assert!(resp.text().contains("\"bytes_written\":0"));

    terminal.screen.write().await.feed(b"\x1b[?1000;1006h");
    let resp = server.post("/api/v1/input/mouse").json(&click).await;
    resp.assert_status(StatusCode::OK);
    match input_rx.recv().await {
        Some(InputEvent::Write(b)) => assert_eq!(&b[..], b"\x1b[<0;3;2M"),
        other => anyhow::bail!("expected a write, got {other:?}"),
    }

    let off = serde_json::json!({"button": "left", "col": 999, "row": 2});
    server.post("/api/v1/input/mouse").json(&off).await.assert_status(StatusCode::BAD_REQUEST);
    Ok(())
}

#[tokio::test]
async fn resize_sends_event() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp =
        server.post("/api/v1/resize").json(&serde_json::json!({"cols": 120, "rows": 40})).await;
    resp.assert_status(StatusCode::OK);

    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Resize { cols: 120, rows: 40 })));
    Ok(())
}

#[tokio::test]
async fn signal_delivers() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.post("/api/v1/signal").json(&serde_json::json!({"signal": "SIGINT"})).await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"delivered\":true"));

    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Signal(crate::event::PtySignal::Int))));
    Ok(())
}

#[tokio::test]
async fn resize_rejects_zero_cols() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).map_err(|e| anyhow::anyhow!("{e}"))?;

    let resp =
        server.post("/api/v1/resize").json(&serde_json::json!({"cols": 0, "rows": 24})).await;
    resp.assert_status(StatusCode::BAD_REQUEST);
    Ok(())
}

#[tokio::test]
async fn resize_rejects_zero_rows() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).map_err(|e| anyhow::anyhow!("{e}"))?;

    let resp =
        server.post("/api/v1/resize").json(&serde_json::json!({"cols": 80, "rows": 0})).await;
    resp.assert_status(StatusCode::BAD_REQUEST);
    Ok(())
}
