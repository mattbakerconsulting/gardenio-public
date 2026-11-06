// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Integration tests for the mux HTTP API.
//!
//! Uses `axum_test::TestServer` — no real TCP needed.

use std::sync::atomic::AtomicU32;
use std::sync::{Arc, Once};
use std::time::Instant;

static CRYPTO_INIT: Once = Once::new();
fn ensure_crypto() {
    CRYPTO_INIT.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

use axum_test::TestServer;
use tokio_util::sync::CancellationToken;

use coopmux::config::MuxConfig;
use coopmux::credential::broker::CredentialBroker;
use coopmux::credential::{AccountConfig, CredentialConfig};

use coopmux::state::{MuxState, SessionEntry};
use coopmux::transport::build_router;

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
        prewarm_capacity: 64,
        prewarm_poll_ms: 15000,
        state_dir: Some(std::env::temp_dir().join(format!("coopmux-test-{}", std::process::id()))),
        api_key_file: None,
        dev_mode: true, // integration tests run without auth
        base_path: None,
        #[cfg(debug_assertions)]
        hot: false,
    }
}

fn empty_broker() -> Arc<CredentialBroker> {
    let (event_tx, _) = tokio::sync::broadcast::channel(64);
    CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None)
}

fn test_state() -> Arc<MuxState> {
    ensure_crypto();
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), empty_broker()))
}

fn test_state_with_broker(accounts: Vec<AccountConfig>) -> Arc<MuxState> {
    ensure_crypto();
    let cred_config = CredentialConfig { accounts };
    let (event_tx, _rx) = tokio::sync::broadcast::channel(64);
    let mux_config = test_config();
    let broker = CredentialBroker::new(cred_config, event_tx, None);
    Arc::new(MuxState::new(mux_config, CancellationToken::new(), broker))
}

fn test_server(state: Arc<MuxState>) -> anyhow::Result<TestServer> {
    let router = build_router(state);
    let server = TestServer::new(router)?;
    Ok(server)
}

/// Insert a fake session entry directly (bypasses upstream health check).
async fn insert_session(state: &MuxState, id: &str, url: &str) {
    let entry = Arc::new(SessionEntry {
        id: id.to_owned(),
        url: url.to_owned(),
        auth_token: None,
        metadata: serde_json::Value::Null,
        registered_at: Instant::now(),
        cached_screen: tokio::sync::RwLock::new(None),
        cached_status: tokio::sync::RwLock::new(None),
        cached_usage: tokio::sync::RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: tokio::sync::RwLock::new(None),
        assigned_account: tokio::sync::RwLock::new(None),
        claimed_bead_title: tokio::sync::RwLock::new(None),
        transport: coopmux::state::SessionTransport::default(),
    });
    state.sessions.write().await.insert(id.to_owned(), entry);
}

#[tokio::test]
async fn health_returns_session_count() -> anyhow::Result<()> {
    let state = test_state();
    insert_session(&state, "s1", "http://fake:1001").await;
    insert_session(&state, "s2", "http://fake:1002").await;

    let server = test_server(state)?;
    let resp = server.get("/api/v1/health").await;
    resp.assert_status_ok();

    let body: serde_json::Value = resp.json();
    assert_eq!(body["status"], "running");
    assert_eq!(body["session_count"], 2);
    assert_eq!(body["credentials_total"], 0);
    assert_eq!(body["credentials_healthy"], 0);
    assert_eq!(body["credentials_missing"], 0);
    Ok(())
}

#[tokio::test]
async fn list_sessions_returns_registered() -> anyhow::Result<()> {
    let state = test_state();
    insert_session(&state, "abc", "http://fake:2001").await;
    insert_session(&state, "def", "http://fake:2002").await;

    let server = test_server(state)?;
    let resp = server.get("/api/v1/sessions").await;
    resp.assert_status_ok();

    let list: Vec<serde_json::Value> = resp.json();
    assert_eq!(list.len(), 2);

    let ids: Vec<&str> = list.iter().filter_map(|s| s["id"].as_str()).collect();
    assert!(ids.contains(&"abc"));
    assert!(ids.contains(&"def"));
    Ok(())
}

/// The session's account must appear under `assigned_account` (the unified
/// wire name shared with the WebSocket feed). The deprecated legacy duplicate
/// key `account` was removed in ka-0000001779 (follow-up to ka-0000007897)
/// after an in-repo sweep confirmed no readers of it remain — this test guards
/// that the response carries the canonical key ONLY.
#[tokio::test]
async fn list_sessions_emits_assigned_account_only() -> anyhow::Result<()> {
    let state = test_state();
    insert_session(&state, "with-acct", "http://fake:2003").await;
    if let Some(entry) = state.sessions.read().await.get("with-acct") {
        *entry.assigned_account.write().await = Some("acct-1".to_owned());
    }

    let server = test_server(state)?;
    let resp = server.get("/api/v1/sessions").await;
    resp.assert_status_ok();

    let list: Vec<serde_json::Value> = resp.json();
    let s = list
        .iter()
        .find(|s| s["id"] == "with-acct")
        .ok_or_else(|| anyhow::anyhow!("session missing from list"))?;
    assert_eq!(s["assigned_account"], "acct-1");
    // The deprecated `account` duplicate must no longer be emitted.
    assert!(s.get("account").is_none(), "legacy `account` key must be gone, got: {s}");
    Ok(())
}

#[tokio::test]
async fn deregister_session_removes_it() -> anyhow::Result<()> {
    let state = test_state();
    insert_session(&state, "to-remove", "http://fake:3001").await;

    let server = test_server(Arc::clone(&state))?;
    let resp = server.delete("/api/v1/sessions/to-remove").await;
    resp.assert_status_ok();

    let body: serde_json::Value = resp.json();
    assert_eq!(body["removed"], true);

    // Verify it's gone from the list.
    let sessions = state.sessions.read().await;
    assert!(!sessions.contains_key("to-remove"));
    Ok(())
}

#[tokio::test]
async fn deregister_nonexistent_returns_404() -> anyhow::Result<()> {
    let state = test_state();
    let server = test_server(state)?;
    let resp = server.delete("/api/v1/sessions/nope").await;
    resp.assert_status(axum::http::StatusCode::NOT_FOUND);
    Ok(())
}

#[tokio::test]
async fn dashboard_serves_html() -> anyhow::Result<()> {
    let state = test_state();
    let server = test_server(state)?;
    let resp = server.get("/mux").await;
    resp.assert_status_ok();

    let body = resp.text();
    assert!(body.contains("<html") || body.contains("<!DOCTYPE"));
    Ok(())
}

#[tokio::test]
async fn sse_mux_send_unknown_conn_returns_404() -> anyhow::Result<()> {
    // The SSE upstream POST endpoint must be reachable (not blocked by the
    // bearer auth_layer — browsers can't set Authorization on the GET stream),
    // and routing a control message to an unopened connection is a 404, not a
    // 401 or 500. This guards the AVA fallback path end-to-end.
    let state = test_state();
    let server = test_server(state)?;
    let resp = server
        .post("/sse/mux/send?conn=never-opened")
        .json(&serde_json::json!({ "event": "subscribe", "sessions": [] }))
        .await;
    resp.assert_status(axum::http::StatusCode::NOT_FOUND);
    Ok(())
}

#[tokio::test]
async fn sse_session_send_unknown_conn_returns_404() -> anyhow::Result<()> {
    let state = test_state();
    insert_session(&state, "sess-1", "http://127.0.0.1:1/").await;
    let server = test_server(state)?;
    let resp = server
        .post("/sse/session/sess-1/send?conn=never-opened")
        .json(&serde_json::json!({ "event": "resize", "cols": 80, "rows": 24 }))
        .await;
    resp.assert_status(axum::http::StatusCode::NOT_FOUND);
    Ok(())
}

#[tokio::test]
async fn credentials_status_empty_broker_returns_200() -> anyhow::Result<()> {
    let state = test_state();
    let server = test_server(state)?;
    let resp = server.get("/api/v1/credentials/status").await;
    resp.assert_status_ok();
    let body: serde_json::Value = resp.json();
    assert!(body.as_array().is_some_and(|a| a.is_empty()));
    Ok(())
}

#[tokio::test]
async fn credentials_set_and_status() -> anyhow::Result<()> {
    let accounts = vec![AccountConfig {
        name: "test-acct".into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }];
    let state = test_state_with_broker(accounts);
    let server = test_server(Arc::clone(&state))?;

    // Set tokens.
    let set_resp = server
        .post("/api/v1/credentials/set")
        .json(&serde_json::json!({
            "account": "test-acct",
            "token": "sk-test-token",
            "expires_in": 3600
        }))
        .await;
    set_resp.assert_status_ok();

    let body: serde_json::Value = set_resp.json();
    assert_eq!(body["ok"], true);

    // Check status.
    let status_resp = server.get("/api/v1/credentials/status").await;
    status_resp.assert_status_ok();

    let list: Vec<serde_json::Value> = status_resp.json();
    assert_eq!(list.len(), 1);
    assert_eq!(list[0]["name"], "test-acct");
    assert_eq!(list[0]["status"], "healthy");
    Ok(())
}

#[tokio::test]
async fn credentials_set_base_url_flows_into_profile_credentials() -> anyhow::Result<()> {
    // ka-0000002623: base_url pushed via credentials/set must land in the
    // credential map staged as rotation profiles, so a cross-endpoint switch
    // (e.g. onto a Bedrock account behind a different LiteLLM) carries
    // ANTHROPIC_BASE_URL alongside the token/model.
    let accounts = vec![AccountConfig {
        name: "bedrock-acct".into(),
        provider: "claude".into(),
        env_key: None,
        model: Some("bedrock-glm-5".into()),
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }];
    let state = test_state_with_broker(accounts);
    let server = test_server(Arc::clone(&state))?;

    let set_resp = server
        .post("/api/v1/credentials/set")
        .json(&serde_json::json!({
            "account": "bedrock-acct",
            "token": "sk-bedrock-token",
            "base_url": "https://litellm.app.devops.acme.example"
        }))
        .await;
    set_resp.assert_status_ok();

    let creds = state
        .credential_broker
        .get_credentials("bedrock-acct")
        .await
        .ok_or_else(|| anyhow::anyhow!("expected credentials for bedrock-acct"))?;
    assert_eq!(
        creds.get("ANTHROPIC_BASE_URL").map(String::as_str),
        Some("https://litellm.app.devops.acme.example")
    );
    assert_eq!(creds.get("CLAUDE_MODEL").map(String::as_str), Some("bedrock-glm-5"));
    Ok(())
}

#[tokio::test]
async fn credentials_add_account_then_status() -> anyhow::Result<()> {
    // Start with an EMPTY broker (no pre-configured accounts).
    let state = test_state_with_broker(vec![]);
    let server = test_server(Arc::clone(&state))?;

    // Verify status is initially empty.
    let status_resp = server.get("/api/v1/credentials/status").await;
    status_resp.assert_status_ok();
    let list: Vec<serde_json::Value> = status_resp.json();
    assert_eq!(list.len(), 0);

    // Add account via API (same as web UI form).
    let add_resp = server
        .post("/api/v1/credentials/new")
        .json(&serde_json::json!({
            "name": "my-account",
            "provider": "claude"
        }))
        .await;
    add_resp.assert_status_ok();
    let body: serde_json::Value = add_resp.json();
    assert_eq!(body["added"], true);

    // Status should now contain the new account.
    let status_resp = server.get("/api/v1/credentials/status").await;
    status_resp.assert_status_ok();
    let list: Vec<serde_json::Value> = status_resp.json();
    assert_eq!(list.len(), 1, "expected 1 account, got: {list:?}");
    assert_eq!(list[0]["name"], "my-account");
    assert_eq!(list[0]["provider"], "claude");
    assert_eq!(list[0]["status"], "missing");
    Ok(())
}

/// Regression test: the refresh loop previously held an `RwLock` write guard
/// across a 60-second `tokio::time::sleep`, blocking all `status_list()` reads.
#[tokio::test]
async fn credentials_status_not_blocked_by_refresh_loop() -> anyhow::Result<()> {
    let state = test_state_with_broker(vec![]);
    let server = test_server(Arc::clone(&state))?;

    // Add account with no token — spawns a refresh loop that sleeps in the
    // "no refresh token" branch.
    let add_resp = server
        .post("/api/v1/credentials/new")
        .json(&serde_json::json!({
            "name": "no-token-acct",
            "provider": "claude"
        }))
        .await;
    add_resp.assert_status_ok();

    // Give the refresh loop a chance to start and enter its sleep.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;

    // Status must respond promptly — not blocked for 60s by the refresh loop.
    let result = tokio::time::timeout(
        std::time::Duration::from_secs(2),
        server.get("/api/v1/credentials/status"),
    )
    .await;
    assert!(result.is_ok(), "status endpoint blocked by refresh loop lock");

    let resp = result?;
    resp.assert_status_ok();
    let list: Vec<serde_json::Value> = resp.json();
    assert_eq!(list.len(), 1);
    assert_eq!(list[0]["name"], "no-token-acct");
    Ok(())
}

#[tokio::test]
async fn credentials_reauth_without_broker_returns_400() -> anyhow::Result<()> {
    let state = test_state();
    let server = test_server(state)?;
    let resp = server.post("/api/v1/credentials/reauth").json(&serde_json::json!({})).await;
    resp.assert_status(axum::http::StatusCode::BAD_REQUEST);
    Ok(())
}

#[tokio::test]
async fn launch_without_command_returns_400() -> anyhow::Result<()> {
    // No launch command configured.
    let state = test_state();
    let server = test_server(state)?;
    let resp = server.post("/api/v1/sessions/launch").await;
    resp.assert_status(axum::http::StatusCode::BAD_REQUEST);
    Ok(())
}

#[tokio::test]
async fn launch_with_empty_body() -> anyhow::Result<()> {
    // Backward compatibility — empty body should work.
    let mut cfg = test_config();
    cfg.launch = Some("echo 'launched'".into());
    let state = Arc::new(MuxState::new(cfg, CancellationToken::new(), empty_broker()));
    let server = test_server(state)?;

    let resp = server.post("/api/v1/sessions/launch").await;
    resp.assert_status_ok();

    let body: serde_json::Value = resp.json();
    assert_eq!(body["launched"], true);
    Ok(())
}

#[tokio::test]
async fn launch_with_env_vars() -> anyhow::Result<()> {
    // Launch with user-supplied env vars in request body.
    let mut cfg = test_config();
    cfg.launch = Some("echo 'launched with env'".into());
    let state = Arc::new(MuxState::new(cfg, CancellationToken::new(), empty_broker()));
    let server = test_server(state)?;

    let resp = server
        .post("/api/v1/sessions/launch")
        .json(&serde_json::json!({
            "env": {
                "GIT_REPO": "https://github.com/user/repo",
                "WORKING_DIR": "/workspace"
            }
        }))
        .await;
    resp.assert_status_ok();

    let body: serde_json::Value = resp.json();
    assert_eq!(body["launched"], true);
    Ok(())
}

#[tokio::test]
async fn launch_with_empty_env_object() -> anyhow::Result<()> {
    // Empty env object should behave the same as no body.
    let mut cfg = test_config();
    cfg.launch = Some("echo 'launched'".into());
    let state = Arc::new(MuxState::new(cfg, CancellationToken::new(), empty_broker()));
    let server = test_server(state)?;

    let resp = server.post("/api/v1/sessions/launch").json(&serde_json::json!({ "env": {} })).await;
    resp.assert_status_ok();

    let body: serde_json::Value = resp.json();
    assert_eq!(body["launched"], true);
    Ok(())
}

/// Mock coop session server that records incoming request paths in order.
///
/// Used to verify that the rebalance flow registers profiles before switching.
fn mock_coop_session(log: Arc<tokio::sync::Mutex<Vec<String>>>) -> axum::Router {
    use axum::routing::post;

    let profiles_log = Arc::clone(&log);
    let switch_log = Arc::clone(&log);

    axum::Router::new()
        .route(
            "/api/v1/session/profiles",
            post(move |axum::Json(_body): axum::Json<serde_json::Value>| {
                let log = Arc::clone(&profiles_log);
                async move {
                    log.lock().await.push("profiles".to_owned());
                    axum::Json(serde_json::json!({ "registered": 1 }))
                }
            }),
        )
        .route(
            "/api/v1/session/switch",
            post(move |axum::Json(_body): axum::Json<serde_json::Value>| {
                let log = Arc::clone(&switch_log);
                async move {
                    log.lock().await.push("switch".to_owned());
                    axum::http::StatusCode::ACCEPTED
                }
            }),
        )
}

/// Rebalance must register the profile on the session before switching.
///
/// Regression test: previously, `credentials_pool_rebalance` called
/// `POST /api/v1/session/switch` without first registering the target
/// account as a profile, causing the switch to silently fail with 400.
#[tokio::test]
async fn rebalance_registers_profile_before_switch() -> anyhow::Result<()> {
    // 1. Spin up a mock coop session server that records request order.
    let request_log: Arc<tokio::sync::Mutex<Vec<String>>> =
        Arc::new(tokio::sync::Mutex::new(Vec::new()));
    let mock_router = mock_coop_session(Arc::clone(&request_log));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let mock_addr = listener.local_addr()?;
    tokio::spawn(async move {
        axum::serve(listener, mock_router).await.ok();
    });

    // 2. Set up mux state with two accounts: acct-a (assigned) and acct-b (target).
    let accounts = vec![
        AccountConfig {
            name: "acct-a".into(),
            provider: "claude".into(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        },
        AccountConfig {
            name: "acct-b".into(),
            provider: "claude".into(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        },
    ];
    let state = test_state_with_broker(accounts);

    // Set API keys so accounts are healthy and have credentials.
    let broker = &state.credential_broker;
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;

    // Insert a session pointing at the mock coop server, assigned to acct-a.
    let mock_url = format!("http://{mock_addr}");
    let entry = Arc::new(SessionEntry {
        id: "s1".to_owned(),
        url: mock_url,
        auth_token: None,
        metadata: serde_json::Value::Null,
        registered_at: Instant::now(),
        cached_screen: tokio::sync::RwLock::new(None),
        cached_status: tokio::sync::RwLock::new(None),
        cached_usage: tokio::sync::RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: tokio::sync::RwLock::new(None),
        assigned_account: tokio::sync::RwLock::new(Some("acct-a".into())),
        claimed_bead_title: tokio::sync::RwLock::new(None),
        transport: coopmux::state::SessionTransport::default(),
    });
    state.sessions.write().await.insert("s1".to_owned(), entry);

    // Mark acct-a as having one session so rebalance picks acct-b.
    broker.session_assigned("acct-a").await;

    // 3. Call rebalance via the mux API.
    let server = test_server(state)?;
    let resp = server.post("/api/v1/credentials/pool/rebalance").json(&serde_json::json!({})).await;
    resp.assert_status_ok();

    let body: serde_json::Value = resp.json();
    assert_eq!(body["rebalanced"], 1, "expected 1 session rebalanced, got: {body:?}");
    assert_eq!(body["failed"], 0, "expected no failures, got: {body:?}");

    // 4. Verify the mock received profiles registration BEFORE switch.
    let log = request_log.lock().await;
    assert_eq!(log.len(), 2, "expected 2 requests (profiles + switch), got: {log:?}");
    assert_eq!(log[0], "profiles", "first request must be profile registration, got: {log:?}");
    assert_eq!(log[1], "switch", "second request must be switch, got: {log:?}");

    Ok(())
}

/// The operator rebalance endpoint must not move a Claude session onto a Codex
/// account (the only other healthy account), nor push it a Codex profile.
#[tokio::test]
async fn rebalance_never_moves_claude_session_to_codex_account() -> anyhow::Result<()> {
    let request_log: Arc<tokio::sync::Mutex<Vec<String>>> =
        Arc::new(tokio::sync::Mutex::new(Vec::new()));
    let mock_router = mock_coop_session(Arc::clone(&request_log));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let mock_addr = listener.local_addr()?;
    tokio::spawn(async move {
        axum::serve(listener, mock_router).await.ok();
    });

    let account = |name: &str, provider: &str| AccountConfig {
        name: name.into(),
        provider: provider.into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    };
    let state =
        test_state_with_broker(vec![account("claude-a", "claude"), account("codex-a", "openai")]);
    let broker = &state.credential_broker;
    broker.set_token("claude-a", "key-a".into(), None, None).await?;
    let doc = serde_json::json!({
        "auth_mode": "chatgpt",
        "tokens": { "access_token": "at", "refresh_token": "rt", "id_token": "id" },
    })
    .to_string();
    broker.set_token("codex-a", doc, None, None).await?;

    let entry = Arc::new(SessionEntry {
        id: "s1".to_owned(),
        url: format!("http://{mock_addr}"),
        auth_token: None,
        metadata: serde_json::json!({ "agent": "claude" }),
        registered_at: Instant::now(),
        cached_screen: tokio::sync::RwLock::new(None),
        cached_status: tokio::sync::RwLock::new(None),
        cached_usage: tokio::sync::RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: tokio::sync::RwLock::new(None),
        assigned_account: tokio::sync::RwLock::new(Some("claude-a".into())),
        claimed_bead_title: tokio::sync::RwLock::new(None),
        transport: coopmux::state::SessionTransport::default(),
    });
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&entry));
    broker.session_assigned("claude-a").await;

    let server = test_server(state)?;
    let resp = server.post("/api/v1/credentials/pool/rebalance").json(&serde_json::json!({})).await;
    resp.assert_status_ok();
    let body: serde_json::Value = resp.json();
    assert_eq!(body["rebalanced"], 0, "claude session rebalanced onto codex: {body:?}");
    assert!(request_log.lock().await.is_empty(), "no profile may be pushed to the session");
    assert_eq!(entry.assigned_account.read().await.as_deref(), Some("claude-a"));
    Ok(())
}

// ── Project-scoped credential isolation integration tests ──────────────

/// End-to-end: project-scoped assignment uses authorized accounts only.
///
/// Verifies that `AccountCache::authorized_accounts_for_project()` and
/// `CredentialBroker::assign_account_for_project()` work together correctly
/// to enforce project-scoped credential isolation.
#[tokio::test]
async fn project_scoped_assignment_uses_only_authorized_accounts() -> anyhow::Result<()> {
    use coopmux::credential::failover::{AccountInfo, AuthConfigInfo};

    let accounts = vec![
        AccountConfig {
            name: "proj-a-primary".into(),
            provider: "claude".into(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        },
        AccountConfig {
            name: "proj-a-fallback".into(),
            provider: "claude".into(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        },
        AccountConfig {
            name: "global-only".into(),
            provider: "claude".into(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        },
    ];
    let state = test_state_with_broker(accounts);
    let broker = &state.credential_broker;

    // Set up all accounts as healthy.
    broker.set_token("proj-a-primary", "key-primary".into(), None, None).await?;
    broker.set_token("proj-a-fallback", "key-fallback".into(), None, None).await?;
    broker.set_token("global-only", "key-global".into(), None, None).await?;

    // Configure AccountCache: the project-a pool = both accounts carrying the
    // `project:project-a` label. The authorized set is built from this pool
    // (mirrors the Go selectSpreadAccount candidate set), NOT the fallback
    // chain — so both pool members are authorized even though neither needs a
    // fallback pointer. global-only carries no project-a label.
    {
        let mut cache = state.account_cache.write().await;
        cache.accounts_mut().insert(
            "proj-a-primary".to_owned(),
            AccountInfo {
                name: "proj-a-primary".to_owned(),
                fallback: None,
                provider: "claude".to_owned(),
                labels: vec!["project:project-a".to_owned()],
                ..Default::default()
            },
        );
        cache.accounts_mut().insert(
            "proj-a-fallback".to_owned(),
            AccountInfo {
                name: "proj-a-fallback".to_owned(),
                fallback: None,
                provider: "claude".to_owned(),
                labels: vec!["project:project-a".to_owned()],
                ..Default::default()
            },
        );
        cache.accounts_mut().insert(
            "global-only".to_owned(),
            AccountInfo {
                name: "global-only".to_owned(),
                fallback: None,
                provider: "claude".to_owned(),
                labels: vec![],
                ..Default::default()
            },
        );
        cache.auth_configs_mut().insert(
            "project-a".to_owned(),
            AuthConfigInfo {
                bead_id: "bead-cfg-a".to_owned(),
                account: "proj-a-primary".to_owned(),
                primary_account: None,
                project: "project-a".to_owned(),
            },
        );
    }

    // 1. Get authorized set and verify it.
    let authorized = {
        let cache = state.account_cache.read().await;
        cache.authorized_accounts_for_project("project-a")
    };
    assert!(authorized.contains("proj-a-primary"));
    assert!(authorized.contains("proj-a-fallback"));
    assert!(!authorized.contains("global-only"));

    // 2. Project-scoped assignment prefers primary.
    let assigned = broker.assign_account_for_project(Some("proj-a-primary"), &authorized).await;
    assert_eq!(assigned, Some("proj-a-primary".to_owned()));

    // 3. When primary is throttled, falls back to authorized fallback.
    broker.mark_throttled("proj-a-primary").await;
    let assigned = broker.assign_account_for_project(Some("proj-a-primary"), &authorized).await;
    assert_eq!(assigned, Some("proj-a-fallback".to_owned()));

    // 4. When all authorized are throttled, returns None — does NOT use global-only.
    broker.mark_throttled("proj-a-fallback").await;
    let assigned = broker.assign_account_for_project(Some("proj-a-primary"), &authorized).await;
    assert_eq!(assigned, None);

    // 5. Global pool (no project scope) DOES return global-only.
    let global_assigned = broker.assign_account(None).await;
    assert_eq!(global_assigned, Some("global-only".to_owned()));

    Ok(())
}

/// End-to-end: session without project metadata uses global pool.
#[tokio::test]
async fn session_without_project_uses_global_pool() -> anyhow::Result<()> {
    let accounts = vec![
        AccountConfig {
            name: "acct-1".into(),
            provider: "claude".into(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        },
        AccountConfig {
            name: "acct-2".into(),
            provider: "claude".into(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        },
    ];
    let state = test_state_with_broker(accounts);
    let broker = &state.credential_broker;

    broker.set_token("acct-1", "key-1".into(), None, None).await?;
    broker.set_token("acct-2", "key-2".into(), None, None).await?;

    // No project context → global pool.
    let authorized = {
        let cache = state.account_cache.read().await;
        cache.authorized_accounts_for_project("nonexistent-project")
    };
    assert!(authorized.is_empty());

    // Global pool assignment works.
    let assigned = broker.assign_account(None).await;
    assert!(assigned.is_some());

    Ok(())
}

// ── Registration-time staging (in-session rotation enable, ka-0000005014) ──
//
// These tests pin the new contract: on a new session registration coopmux
// STAGES health-filtered fallback profiles but NEVER issues a
// /api/v1/session/switch. This is the regression guard for the respawn loop
// (the registration switch is gone) AND the health gate (unusable accounts are
// not staged).

/// Records the staged profile names (in pushed order) and whether a switch was
/// ever requested.
#[derive(Clone, Default)]
struct StagingRecord {
    /// Profile names exactly as received (order preserved).
    ordered: Arc<tokio::sync::Mutex<Vec<String>>>,
    /// Profile names sorted (order-independent membership assertions).
    profiles: Arc<tokio::sync::Mutex<Vec<String>>>,
    switch_calls: Arc<std::sync::atomic::AtomicU32>,
}

/// Mock coop session server that captures the profile-staging body and counts
/// switch calls. Returns the bound address plus the shared record.
async fn spawn_mock_coop(record: StagingRecord) -> anyhow::Result<String> {
    use axum::routing::{get, post};

    let profiles = Arc::clone(&record.profiles);
    let ordered = Arc::clone(&record.ordered);
    let switch_calls = Arc::clone(&record.switch_calls);

    let router = axum::Router::new()
        // register_session() health-checks the upstream before registering.
        .route(
            "/api/v1/health",
            get(|| async { axum::Json(serde_json::json!({ "status": "running" })) }),
        )
        .route(
            "/api/v1/session/profiles",
            post(move |axum::Json(body): axum::Json<serde_json::Value>| {
                let profiles = Arc::clone(&profiles);
                let ordered = Arc::clone(&ordered);
                async move {
                    if let Some(arr) = body.get("profiles").and_then(|p| p.as_array()) {
                        let names: Vec<String> = arr
                            .iter()
                            .filter_map(|e| e.get("name").and_then(|n| n.as_str()))
                            .map(|s| s.to_owned())
                            .collect();
                        *ordered.lock().await = names.clone();
                        let mut sorted = names;
                        sorted.sort();
                        *profiles.lock().await = sorted;
                    }
                    axum::Json(serde_json::json!({ "registered": 1 }))
                }
            }),
        )
        .route(
            "/api/v1/session/switch",
            post(move |axum::Json(_body): axum::Json<serde_json::Value>| {
                let switch_calls = Arc::clone(&switch_calls);
                async move {
                    switch_calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                    axum::http::StatusCode::ACCEPTED
                }
            }),
        );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        axum::serve(listener, router).await.ok();
    });
    Ok(format!("http://{addr}"))
}

fn account_cfg(name: &str) -> AccountConfig {
    AccountConfig {
        name: name.into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }
}

/// Registration stages healthy fallback profiles and issues NO switch.
///
/// Regression test for the respawn loop: a session reporting account_name=None
/// must NOT trigger a registration-time switch. It must, however, receive the
/// healthy accounts as staged profiles so its in-session rotation has fallbacks.
#[tokio::test]
async fn registration_stages_profiles_without_switch() -> anyhow::Result<()> {
    use coopmux::credential::failover::AccountInfo;

    let state = test_state_with_broker(vec![account_cfg("acct-a"), account_cfg("acct-b")]);
    let broker = &state.credential_broker;
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;

    // Both accounts healthy in the bead-authoritative cache.
    {
        let mut cache = state.account_cache.write().await;
        for n in ["acct-a", "acct-b"] {
            cache.accounts_mut().insert(
                n.to_owned(),
                AccountInfo {
                    name: n.to_owned(),
                    status: "open".to_owned(),
                    provider: "claude".to_owned(),
                    ..Default::default()
                },
            );
        }
    }

    let record = StagingRecord::default();
    let mock_url = spawn_mock_coop(record.clone()).await?;

    // Register a new session with NO reported account (the loop-trigger case).
    let server = test_server(Arc::clone(&state))?;
    let resp = server
        .post("/api/v1/sessions")
        .json(&serde_json::json!({
            "url": mock_url,
            "id": "s-stage",
        }))
        .await;
    resp.assert_status_ok();

    // The staging push happens in a spawned task; give it a moment.
    wait_for_profiles(&record, 2).await;

    let profiles = record.profiles.lock().await.clone();
    assert_eq!(
        profiles,
        vec!["acct-a".to_owned(), "acct-b".to_owned()],
        "both healthy accounts staged"
    );
    assert_eq!(
        record.switch_calls.load(std::sync::atomic::Ordering::SeqCst),
        0,
        "registration must NOT issue a switch (respawn-loop regression)"
    );

    Ok(())
}

/// Unusable accounts (disabled / exhausted+rejected) are NOT staged, but the
/// session's own booted account is always staged so it can switch back.
#[tokio::test]
async fn registration_health_filters_staged_profiles() -> anyhow::Result<()> {
    use coopmux::credential::failover::AccountInfo;

    let state = test_state_with_broker(vec![
        account_cfg("healthy-acct"),
        account_cfg("disabled-acct"),
        account_cfg("exhausted-acct"),
        account_cfg("booted-acct"),
    ]);
    let broker = &state.credential_broker;
    for n in ["healthy-acct", "disabled-acct", "exhausted-acct", "booted-acct"] {
        broker.set_token(n, format!("key-{n}"), None, None).await?;
    }

    {
        let mut cache = state.account_cache.write().await;
        let mut put = |name: &str, status: &str, throttle: &str| {
            cache.accounts_mut().insert(
                name.to_owned(),
                AccountInfo {
                    name: name.to_owned(),
                    status: status.to_owned(),
                    throttle_status: throttle.to_owned(),
                    provider: "claude".to_owned(),
                    ..Default::default()
                },
            );
        };
        put("healthy-acct", "open", "");
        put("disabled-acct", "disabled", "allowed");
        put("exhausted-acct", "exhausted", "rejected");
        // The booted account is itself exhausted+rejected (i.e. the account
        // that just rate-limited) — it must still be staged so the session has
        // its slot for switchback once it recovers.
        put("booted-acct", "exhausted", "rejected");
    }

    let record = StagingRecord::default();
    let mock_url = spawn_mock_coop(record.clone()).await?;

    let server = test_server(Arc::clone(&state))?;
    let resp = server
        .post("/api/v1/sessions")
        .json(&serde_json::json!({
            "url": mock_url,
            "id": "s-filter",
            "account_name": "booted-acct",
        }))
        .await;
    resp.assert_status_ok();

    wait_for_profiles(&record, 2).await;

    let profiles = record.profiles.lock().await.clone();
    assert_eq!(
        profiles,
        vec!["booted-acct".to_owned(), "healthy-acct".to_owned()],
        "only the healthy account + the booted account are staged; disabled/exhausted excluded"
    );
    // The booted account must be staged FIRST so coop's register() seeds it as
    // Active (rather than an arbitrary fallback) — see ka-0000005014.
    let ordered = record.ordered.lock().await.clone();
    assert_eq!(
        ordered.first().map(String::as_str),
        Some("booted-acct"),
        "booted account staged first"
    );
    assert_eq!(
        record.switch_calls.load(std::sync::atomic::Ordering::SeqCst),
        0,
        "no switch at registration"
    );

    Ok(())
}

/// The booted account is staged first AND is always included, even when it is
/// itself exhausted and outside the (here, global) usable set — so coop's Active
/// profile aligns with its live env credentials and never drifts onto a
/// fallback. Regression guard for the arbitrary-Active bug (ka-0000005014).
#[tokio::test]
async fn registration_stages_booted_account_first_even_when_exhausted() -> anyhow::Result<()> {
    use coopmux::credential::failover::AccountInfo;

    let state = test_state_with_broker(vec![
        account_cfg("alpha"),
        account_cfg("bravo"),
        account_cfg("booted"),
    ]);
    let broker = &state.credential_broker;
    for n in ["alpha", "bravo", "booted"] {
        broker.set_token(n, format!("key-{n}"), None, None).await?;
    }
    {
        let mut cache = state.account_cache.write().await;
        let mut put = |name: &str, status: &str, throttle: &str| {
            cache.accounts_mut().insert(
                name.to_owned(),
                AccountInfo {
                    name: name.to_owned(),
                    status: status.to_owned(),
                    throttle_status: throttle.to_owned(),
                    provider: "claude".to_owned(),
                    ..Default::default()
                },
            );
        };
        put("alpha", "open", "");
        put("bravo", "open", "");
        // The booted account just rate-limited (exhausted+rejected) — it must
        // still be staged, and first.
        put("booted", "exhausted", "rejected");
    }

    let record = StagingRecord::default();
    let mock_url = spawn_mock_coop(record.clone()).await?;
    let server = test_server(Arc::clone(&state))?;
    let resp = server
        .post("/api/v1/sessions")
        .json(&serde_json::json!({
            "url": mock_url,
            "id": "s-order",
            "account_name": "booted",
        }))
        .await;
    resp.assert_status_ok();

    wait_for_profiles(&record, 3).await;

    let ordered = record.ordered.lock().await.clone();
    assert_eq!(
        ordered.first().map(String::as_str),
        Some("booted"),
        "booted account must be staged first so it becomes coop's Active profile"
    );
    let profiles = record.profiles.lock().await.clone();
    assert_eq!(
        profiles,
        vec!["alpha".to_owned(), "booted".to_owned(), "bravo".to_owned()],
        "all healthy accounts + the exhausted booted account are staged"
    );
    assert_eq!(record.switch_calls.load(std::sync::atomic::Ordering::SeqCst), 0);

    Ok(())
}

// ── Land wall at staging (gd-bD6BNm16oA) ──
//
// A land's purse account (`lands-<land>`) is never staged on a fleet session,
// and a land session stages only its own purse account — not even a role pin.

/// Broker + cache with `fleet-a`, `fleet-b` and `lands-x`, all healthy.
async fn land_wall_state() -> anyhow::Result<Arc<MuxState>> {
    use coopmux::credential::failover::AccountInfo;

    let names = ["fleet-a", "fleet-b", "lands-x"];
    let state = test_state_with_broker(names.iter().map(|n| account_cfg(n)).collect());
    for n in names {
        state.credential_broker.set_token(n, format!("key-{n}"), None, None).await?;
    }
    let mut cache = state.account_cache.write().await;
    for n in names {
        let labels = if n == "lands-x" { vec!["project:x".to_owned()] } else { vec![] };
        cache.accounts_mut().insert(
            n.to_owned(),
            AccountInfo {
                name: n.to_owned(),
                status: "open".to_owned(),
                provider: "claude".to_owned(),
                labels,
                ..Default::default()
            },
        );
    }
    drop(cache);
    Ok(state)
}

/// Register a session with `body` against a fresh mock coop and return the
/// sorted staged profile names.
async fn stage(
    state: &Arc<MuxState>,
    mut body: serde_json::Value,
    n: usize,
) -> anyhow::Result<Vec<String>> {
    let record = StagingRecord::default();
    body["url"] = serde_json::json!(spawn_mock_coop(record.clone()).await?);
    let server = test_server(Arc::clone(state))?;
    server.post("/api/v1/sessions").json(&body).await.assert_status_ok();
    wait_for_profiles(&record, n).await;
    // Give a late extra profile the chance to show up before asserting.
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    assert_eq!(record.switch_calls.load(std::sync::atomic::Ordering::SeqCst), 0);
    let profiles = record.profiles.lock().await.clone();
    Ok(profiles)
}

#[tokio::test]
async fn staging_never_hands_a_fleet_session_a_land_purse() -> anyhow::Result<()> {
    let state = land_wall_state().await?;
    let fleet = vec!["fleet-a".to_owned(), "fleet-b".to_owned()];

    // No project: the global pool, minus the land's purse.
    let got = stage(&state, serde_json::json!({ "id": "s-global" }), 2).await?;
    assert_eq!(got, fleet, "project-less session");

    // A project with no labelled accounts (authorized=None): same.
    let got =
        stage(&state, serde_json::json!({ "id": "s-proj", "metadata": { "project": "proj" } }), 2)
            .await?;
    assert_eq!(got, fleet, "project with no labelled accounts");
    Ok(())
}

#[tokio::test]
async fn staging_hands_a_land_session_only_its_purse() -> anyhow::Result<()> {
    let state = land_wall_state().await?;
    // A steward role pin onto a fleet account must not reach a land session.
    state
        .account_cache
        .write()
        .await
        .role_accounts_mut()
        .insert("steward".into(), "fleet-a".into());

    let got = stage(
        &state,
        serde_json::json!({ "id": "s-land", "metadata": { "project": "x", "role": "steward" } }),
        1,
    )
    .await?;
    assert_eq!(got, vec!["lands-x".to_owned()], "land session known by its project");

    let got = stage(
        &state,
        serde_json::json!({ "id": "s-land-booted", "account_name": "lands-x", "metadata": { "role": "steward" } }),
        1,
    )
    .await?;
    assert_eq!(got, vec!["lands-x".to_owned()], "land session known by its booted account");
    Ok(())
}

// NOTE: the COOPMUX_DISABLE_AUTO_ASSIGN kill switch is intentionally NOT
// covered by an integration test here. It is read via a process-global env var
// inside the registration task, so a test that sets it would race the two
// staging tests above (which run in parallel and assert profiles ARE staged),
// making the whole suite flaky. The kill-switch path is a single early-return;
// its behavior (stage nothing) is verified by inspection + the helm wiring.

/// Poll the staging record until `n` profiles have been pushed, or time out.
async fn wait_for_profiles(record: &StagingRecord, n: usize) {
    for _ in 0..50 {
        if record.profiles.lock().await.len() >= n {
            return;
        }
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    }
}

// ── Per-session WebSocket control-frame tier enforcement (ka-0000002552) ──
//
// The per-session `/ws/{id}` upgrade is only OPERATOR-gated, but `handle_ws`
// forwards each frame verbatim to the upstream coop /ws, which executes
// admin-reserved raw/signal/shutdown/restart/switch actions. Lane-C finding 3
// flagged `ws.rs` at 0% patch coverage — the raw-forward path was untested,
// and the synthetic role-matrix tests never send a control frame. These drive a
// REAL WebSocket through `build_router` (HTTP transport) so the actual
// `handle_ws` per-frame gate runs.

/// Set `edge_auth_cfg` on a freshly-built state so the edge-auth layer verifies
/// a signed v2 envelope (enforce=true, real HMAC key).
fn edge_state_with_key(key: &[u8]) -> anyhow::Result<Arc<MuxState>> {
    ensure_crypto();
    let (event_tx, _rx) = tokio::sync::broadcast::channel(64);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None);
    // dev_mode=false so edge-auth actually gates; auth_token unset so browser
    // WS callers authenticate purely via the edge envelope.
    let mut config = test_config();
    config.dev_mode = false;
    let mut state = Arc::new(MuxState::new(config, CancellationToken::new(), broker));
    let state_mut =
        Arc::get_mut(&mut state).ok_or_else(|| anyhow::anyhow!("fresh state is unshared"))?;
    state_mut.edge_auth_cfg = coopmux::transport::edge_auth::Config {
        hmac_key: key.to_vec(),
        enforce: true,
        ..coopmux::transport::edge_auth::Config::default()
    };
    Ok(state)
}

/// Build the signed v2 edge-auth headers for a `GET /ws/{id}` upgrade.
fn signed_ws_headers(
    key: &[u8],
    role: &str,
    path: &str,
) -> anyhow::Result<Vec<(&'static str, String)>> {
    use base64::engine::general_purpose::STANDARD as B64;
    use base64::Engine as _;
    use coopmux::transport::edge_auth::{
        HEADER_AUDIT_ID, HEADER_AUTH_MODE, HEADER_OPERATOR, HEADER_ROLES, HEADER_SIGNATURE_V2,
        HEADER_TIMESTAMP, HEADER_VERSION,
    };
    use std::time::{SystemTime, UNIX_EPOCH};

    let ts = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs().to_string();
    let operator = "operator@example.com";
    let audit_id = "audit-ws-1";
    let auth_mode = "entra";
    // GET upgrade → method is GET in the canonical string.
    let message = format!("v2\n{operator}\n{auth_mode}\n{role}\n{audit_id}\nGET\n{path}\n{ts}");
    let hkey = ring::hmac::Key::new(ring::hmac::HMAC_SHA256, key);
    let signature = B64.encode(ring::hmac::sign(&hkey, message.as_bytes()).as_ref());
    Ok(vec![
        (HEADER_OPERATOR, operator.to_owned()),
        (HEADER_AUDIT_ID, audit_id.to_owned()),
        (HEADER_TIMESTAMP, ts),
        (HEADER_VERSION, "2".to_owned()),
        (HEADER_AUTH_MODE, auth_mode.to_owned()),
        (HEADER_ROLES, role.to_owned()),
        (HEADER_SIGNATURE_V2, signature),
    ])
}

fn ws_server(state: Arc<MuxState>) -> anyhow::Result<TestServer> {
    let router = build_router(state);
    let server = TestServer::builder().http_transport().build(router)?;
    Ok(server)
}

fn ws_request(
    server: &TestServer,
    key: &[u8],
    role: &str,
    path: &str,
) -> anyhow::Result<axum_test::TestRequest> {
    let mut req = server.get_websocket(path);
    for (name, value) in signed_ws_headers(key, role, path)? {
        req = req.add_header(name, value);
    }
    Ok(req)
}

#[tokio::test]
async fn ws_operator_raw_and_signal_frames_are_refused() -> anyhow::Result<()> {
    let key = b"ws-edge-test-key";
    let state = edge_state_with_key(key)?;
    insert_session(&state, "s1", "http://127.0.0.1:1/").await;
    let server = ws_server(state)?;

    // Operator passes the OPERATOR upgrade gate.
    let mut ws =
        ws_request(&server, key, "gardenio-operator", "/ws/s1")?.await.into_websocket().await;

    // Raw byte injection is admin-reserved → handler returns an error frame and
    // does NOT forward.
    ws.send_text(r#"{"event":"input:send:raw","data":"cm0gLXJmIC8="}"#).await;
    let reply: serde_json::Value = ws.receive_json().await;
    assert_eq!(reply["event"], "error");
    assert_eq!(reply["code"], "forbidden");

    // SIGKILL is admin-reserved → refused too.
    ws.send_text(r#"{"event":"signal:send","signal":"KILL"}"#).await;
    let reply: serde_json::Value = ws.receive_json().await;
    assert_eq!(reply["event"], "error");
    assert_eq!(reply["code"], "forbidden");

    ws.close().await;
    Ok(())
}

#[tokio::test]
async fn ws_admin_raw_frame_is_accepted() -> anyhow::Result<()> {
    let key = b"ws-edge-test-key";
    let state = edge_state_with_key(key)?;
    insert_session(&state, "s1", "http://127.0.0.1:1/").await;
    let server = ws_server(state)?;

    let mut ws = ws_request(&server, key, "gardenio-admin", "/ws/s1")?.await.into_websocket().await;

    // Admin may send raw bytes → the frame is forwarded to the (dead) upstream
    // bridge rather than refused, so no error frame comes back. Assert the
    // handler does not reject it within a short window.
    ws.send_text(r#"{"event":"input:send:raw","data":"cHdkCg=="}"#).await;
    let refused = tokio::time::timeout(std::time::Duration::from_millis(300), ws.receive_message())
        .await
        .ok()
        .map(|msg| msg.into_text().unwrap_or_default().to_string())
        .filter(|text| text.contains("\"forbidden\""));
    assert!(refused.is_none(), "admin raw frame must not be refused, got: {refused:?}");

    ws.close().await;
    Ok(())
}

#[tokio::test]
async fn ws_viewer_cannot_open_per_session_socket() -> anyhow::Result<()> {
    let key = b"ws-edge-test-key";
    let state = edge_state_with_key(key)?;
    insert_session(&state, "s1", "http://127.0.0.1:1/").await;
    let server = ws_server(state)?;

    // A viewer fails the OPERATOR upgrade gate (auth_layer) → 403, no upgrade.
    let response = ws_request(&server, key, "gardenio-viewer", "/ws/s1")?.await;
    response.assert_status(axum_test::http::StatusCode::FORBIDDEN);
    Ok(())
}
