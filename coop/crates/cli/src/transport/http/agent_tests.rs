// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use axum::http::StatusCode;

use crate::driver::AgentState;
use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx, StubNudgeEncoder};
use crate::transport::build_router;

fn test_state() -> StoreCtx {
    StoreBuilder::new().child_pid(1234).build()
}

#[tokio::test]
async fn agent_state_without_driver_returns_state() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/agent").await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"state\":\"starting\""), "body: {body}");
    Ok(())
}

#[tokio::test]
async fn agent_nudge_not_ready_503() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    // ready defaults to false — nudge should be gated
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp =
        server.post("/api/v1/agent/nudge").json(&serde_json::json!({"message": "hello"})).await;
    resp.assert_status(StatusCode::SERVICE_UNAVAILABLE);
    Ok(())
}

#[tokio::test]
async fn agent_nudge_no_driver_404() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    state.ready.store(true, std::sync::atomic::Ordering::Release);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp =
        server.post("/api/v1/agent/nudge").json(&serde_json::json!({"message": "hello"})).await;
    resp.assert_status(StatusCode::NOT_FOUND);
    Ok(())
}

#[tokio::test]
async fn agent_respond_no_driver_404() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    state.ready.store(true, std::sync::atomic::Ordering::Release);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp =
        server.post("/api/v1/agent/respond").json(&serde_json::json!({"accept": true})).await;
    resp.assert_status(StatusCode::NOT_FOUND);
    Ok(())
}

#[tokio::test]
async fn agent_state_includes_error_fields() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new()
        .child_pid(1234)
        .agent_state(AgentState::Error { detail: "rate_limit_error".to_owned() })
        .build();
    // Populate error fields as session loop would
    *state.driver.error.write().await = Some(crate::transport::state::ErrorInfo {
        detail: "rate_limit_error".to_owned(),
        category: crate::driver::ErrorCategory::RateLimited,
    });

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/agent").await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"error_detail\":\"rate_limit_error\""), "body: {body}");
    assert!(body.contains("\"error_category\":\"rate_limited\""), "body: {body}");
    Ok(())
}

#[tokio::test]
async fn agent_state_omits_error_fields_when_not_error() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } =
        StoreBuilder::new().child_pid(1234).agent_state(AgentState::Working).build();

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/agent").await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(!body.contains("error_detail"), "error_detail should be absent: {body}");
    assert!(!body.contains("error_category"), "error_category should be absent: {body}");
    Ok(())
}

#[tokio::test]
async fn agent_nudge_delivered_when_working() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new()
        .child_pid(1234)
        .agent_state(AgentState::Working)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp =
        server.post("/api/v1/agent/nudge").json(&serde_json::json!({"message": "hello"})).await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"delivered\":true"), "body: {body}");
    Ok(())
}

#[tokio::test]
async fn agent_nudge_delivered_when_waiting() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new()
        .child_pid(1234)
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp =
        server.post("/api/v1/agent/nudge").json(&serde_json::json!({"message": "hello"})).await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"delivered\":true"));
    Ok(())
}

#[tokio::test]
async fn auth_rejects_without_token() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } =
        StoreBuilder::new().child_pid(1234).auth_token("secret").build();

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Health should be accessible without auth
    let resp = server.get("/api/v1/health").await;
    resp.assert_status(StatusCode::OK);

    // Screen should require auth
    let resp = server.get("/api/v1/screen").await;
    resp.assert_status(StatusCode::UNAUTHORIZED);

    // With correct bearer token, should pass
    let resp = server
        .get("/api/v1/screen")
        .add_header(
            axum::http::header::AUTHORIZATION,
            axum::http::HeaderValue::from_static("Bearer secret"),
        )
        .await;
    resp.assert_status(StatusCode::OK);

    Ok(())
}

#[tokio::test]
async fn shutdown_cancels_token() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = test_state();
    assert!(!state.lifecycle.shutdown.is_cancelled());
    let app = build_router(state.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.post("/api/v1/shutdown").json(&serde_json::json!({})).await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"accepted\":true"));
    assert!(state.lifecycle.shutdown.is_cancelled());
    Ok(())
}

#[tokio::test]
async fn shutdown_requires_auth() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } =
        StoreBuilder::new().child_pid(1234).auth_token("secret").build();
    let app = build_router(state.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Without auth token — should be rejected
    let resp = server.post("/api/v1/shutdown").json(&serde_json::json!({})).await;
    resp.assert_status(StatusCode::UNAUTHORIZED);
    assert!(!state.lifecycle.shutdown.is_cancelled());

    // With auth token — should succeed
    let resp = server
        .post("/api/v1/shutdown")
        .add_header(
            axum::http::header::AUTHORIZATION,
            axum::http::HeaderValue::from_static("Bearer secret"),
        )
        .json(&serde_json::json!({}))
        .await;
    resp.assert_status(StatusCode::OK);
    assert!(state.lifecycle.shutdown.is_cancelled());
    Ok(())
}
