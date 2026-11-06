// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// Tests for the mux proxy handlers. These drive `session_agent`, `proxy_get`,
// and `proxy_post` against a mock upstream coop server, exercising the
// timeout-bounded UpstreamClient call sites (ka-0000002323) end to end.

use std::collections::HashMap;
use std::sync::atomic::AtomicU32;
use std::sync::Arc;

use axum::body::to_bytes;
use axum::extract::{Path, State};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;

use super::*;
use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{CredentialConfig, CredentialEvent};
use crate::state::{MuxState, SessionEntry, SessionTransport};

fn test_config() -> MuxConfig {
    MuxConfig {
        host: "127.0.0.1".into(),
        port: 0,
        auth_token: None,
        oidc_issuer_url: None,
        oidc_client_id: None,
        screen_poll_ms: 500,
        status_poll_ms: 2000,
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

fn test_state() -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None);
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), broker))
}

fn http_entry(id: &str, url: String) -> Arc<SessionEntry> {
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

/// Mock upstream coop exposing the endpoints the proxy handlers hit.
async fn mock_upstream() -> anyhow::Result<String> {
    let app = Router::new()
        .route(
            "/api/v1/agent",
            get(|| async { Json(serde_json::json!({"agent": "claude", "state": "idle"})) }),
        )
        .route("/api/v1/output", get(|| async { Json(serde_json::json!({"lines": ["hello"]})) }))
        .route(
            "/api/v1/agent/nudge",
            post(|Json(body): Json<serde_json::Value>| async move {
                Json(serde_json::json!({"echo": body}))
            }),
        );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(format!("http://{addr}"))
}

async fn body_json(resp: axum::response::Response) -> anyhow::Result<serde_json::Value> {
    let bytes = to_bytes(resp.into_body(), 64 * 1024).await?;
    Ok(serde_json::from_slice(&bytes)?)
}

#[tokio::test]
async fn session_agent_proxies_upstream_agent() -> anyhow::Result<()> {
    let url = mock_upstream().await?;
    let state = test_state();
    let entry = http_entry("s1", url);
    state.sessions.write().await.insert("s1".to_owned(), entry);

    let resp =
        session_agent(State(Arc::clone(&state)), Path("s1".to_owned())).await.into_response();
    assert_eq!(resp.status(), 200);
    let body = body_json(resp).await?;
    assert_eq!(body.get("agent").and_then(|v| v.as_str()), Some("claude"));
    Ok(())
}

#[tokio::test]
async fn session_agent_unknown_session_is_404() -> anyhow::Result<()> {
    let state = test_state();
    let resp =
        session_agent(State(Arc::clone(&state)), Path("nope".to_owned())).await.into_response();
    assert_eq!(resp.status(), 404);
    Ok(())
}

#[tokio::test]
async fn proxy_get_forwards_to_upstream() -> anyhow::Result<()> {
    let url = mock_upstream().await?;
    let state = test_state();
    state.sessions.write().await.insert("s1".to_owned(), http_entry("s1", url));

    let resp = proxy_get(&state, "s1", "/api/v1/output", &HashMap::new()).await;
    assert_eq!(resp.status(), 200);
    let body = body_json(resp).await?;
    assert_eq!(body.get("lines").and_then(|v| v.as_array()).map(|a| a.len()), Some(1));
    Ok(())
}

#[tokio::test]
async fn proxy_get_unknown_session_is_404() -> anyhow::Result<()> {
    let state = test_state();
    let resp = proxy_get(&state, "nope", "/api/v1/output", &HashMap::new()).await;
    assert_eq!(resp.status(), 404);
    Ok(())
}

#[tokio::test]
async fn proxy_post_forwards_to_upstream() -> anyhow::Result<()> {
    let url = mock_upstream().await?;
    let state = test_state();
    state.sessions.write().await.insert("s1".to_owned(), http_entry("s1", url));

    let resp =
        proxy_post(&state, "s1", "/api/v1/agent/nudge", serde_json::json!({"msg": "hi"})).await;
    assert_eq!(resp.status(), 200);
    let body = body_json(resp).await?;
    assert_eq!(body.pointer("/echo/msg").and_then(|v| v.as_str()), Some("hi"));
    Ok(())
}

#[tokio::test]
async fn proxy_post_unknown_session_is_404() -> anyhow::Result<()> {
    let state = test_state();
    let resp = proxy_post(&state, "nope", "/api/v1/agent/nudge", serde_json::json!({})).await;
    assert_eq!(resp.status(), 404);
    Ok(())
}
