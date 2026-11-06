// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use axum::http::{HeaderMap, StatusCode};

use crate::error::ErrorCode;
use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::auth::{validate_bearer, validate_ws_auth, validate_ws_query};
use crate::transport::build_router;

#[yare::parameterized(
    no_token_allows_all = { None, None, true },
    valid_bearer        = { Some("secret123"), Some("Bearer secret123"), true },
    invalid_bearer      = { Some("secret123"), Some("Bearer wrong"), false },
    missing_header      = { Some("secret123"), None, false },
    wrong_scheme        = { Some("secret123"), Some("Basic dXNlcjpwYXNz"), false },
)]
fn bearer_validation(
    expected_token: Option<&str>,
    header_value: Option<&str>,
    should_pass: bool,
) -> anyhow::Result<()> {
    let mut headers = HeaderMap::new();
    if let Some(val) = header_value {
        headers.insert("authorization", val.parse().anyhow()?);
    }
    let result = validate_bearer(&headers, expected_token);
    if should_pass {
        assert!(result.is_ok(), "expected Ok, got {result:?}");
    } else {
        assert_eq!(result.err(), Some(ErrorCode::Unauthorized));
    }
    Ok(())
}

#[yare::parameterized(
    valid_token    = { "token=secret123&subscribe=output,state", Some("secret123"), true },
    invalid_token  = { "token=wrong", Some("secret123"), false },
    no_token_param = { "subscribe=output,state", Some("secret123"), false },
    no_expected    = { "subscribe=output,state", None, true },
)]
fn ws_query_validation(
    query: &str,
    expected: Option<&str>,
    should_pass: bool,
) -> anyhow::Result<()> {
    let result = validate_ws_query(query, expected);
    if should_pass {
        assert!(result.is_ok(), "expected Ok, got {result:?}");
    } else {
        assert_eq!(result.err(), Some(ErrorCode::Unauthorized));
    }
    Ok(())
}

#[yare::parameterized(
    valid       = { "secret123", Some("secret123"), true },
    invalid     = { "wrong", Some("secret123"), false },
    no_expected = { "anything", None, true },
)]
fn ws_auth_validation(
    token: &str,
    expected: Option<&str>,
    should_pass: bool,
) -> anyhow::Result<()> {
    let result = validate_ws_auth(token, expected);
    if should_pass {
        assert!(result.is_ok(), "expected Ok, got {result:?}");
    } else {
        assert_eq!(result.err(), Some(ErrorCode::Unauthorized));
    }
    Ok(())
}

// -- Middleware integration tests for the per-session hook token -------------

/// Hook callback paths must reject requests when a hook_token is set and the
/// caller presents no Authorization header.
#[tokio::test]
async fn hook_path_rejects_missing_token() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().hook_token("hook-secret").build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::UNAUTHORIZED);
    Ok(())
}

/// Hook callback paths must reject a wrong bearer token.
#[tokio::test]
async fn hook_path_rejects_wrong_token() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().hook_token("hook-secret").build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/stop")
        .add_header("authorization", "Bearer wrong")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::UNAUTHORIZED);
    Ok(())
}

/// Hook callback paths must accept the correct hook_token.
#[tokio::test]
async fn hook_path_accepts_correct_token() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new()
        .hook_token("hook-secret")
        .agent_state(crate::driver::AgentState::Working)
        .build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/stop")
        .add_header("authorization", "Bearer hook-secret")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::OK);
    Ok(())
}

/// `/api/v1/stop/resolve` is a hook-class path — must use hook_token, not auth_token.
#[tokio::test]
async fn stop_resolve_uses_hook_token() -> anyhow::Result<()> {
    let StoreCtx { store, .. } =
        StoreBuilder::new().hook_token("hook-secret").auth_token("operator-secret").build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // operator token must NOT work on the hook-class path.
    let resp = server
        .post("/api/v1/stop/resolve")
        .add_header("authorization", "Bearer operator-secret")
        .json(&serde_json::json!({"status": "done"}))
        .await;
    resp.assert_status(StatusCode::UNAUTHORIZED);

    // hook token does.
    let resp = server
        .post("/api/v1/stop/resolve")
        .add_header("authorization", "Bearer hook-secret")
        .json(&serde_json::json!({"status": "done"}))
        .await;
    resp.assert_status(StatusCode::OK);
    Ok(())
}

/// Operator endpoints continue to authenticate against the bearer (`auth_token`)
/// even when a hook_token is also set — the two slots are independent.
#[tokio::test]
async fn operator_path_uses_auth_token_not_hook_token() -> anyhow::Result<()> {
    let StoreCtx { store, .. } =
        StoreBuilder::new().auth_token("operator-secret").hook_token("hook-secret").build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // hook token must not authenticate operator-facing endpoints.
    let resp = server.get("/api/v1/status").add_header("authorization", "Bearer hook-secret").await;
    resp.assert_status(StatusCode::UNAUTHORIZED);

    // operator token does (auth passes; handler returns successfully).
    let resp =
        server.get("/api/v1/status").add_header("authorization", "Bearer operator-secret").await;
    resp.assert_status(StatusCode::OK);
    Ok(())
}

/// Public paths (health/livez/ready/metrics) bypass auth even when both tokens
/// are configured.
#[tokio::test]
async fn public_paths_bypass_auth() -> anyhow::Result<()> {
    let StoreCtx { store, .. } =
        StoreBuilder::new().auth_token("operator-secret").hook_token("hook-secret").build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Auth is bypassed → handler runs (whether it returns 200 or some other
    // non-auth status depends on session readiness; the point is "not 401").
    for path in ["/api/v1/health", "/api/v1/livez", "/api/v1/ready"] {
        let resp = server.get(path).await;
        assert_ne!(resp.status_code(), StatusCode::UNAUTHORIZED, "{path} should bypass auth");
    }
    Ok(())
}
