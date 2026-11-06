// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::middleware;
use axum::routing::{get, post};
use axum::Extension;
use axum::Router;
use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine as _;
use ring::hmac;
use tokio::sync::broadcast;
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

use axum::http::HeaderMap;

use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{AccountConfig, CredentialConfig, CredentialEvent};
use crate::state::MuxState;
use crate::transport::auth::{
    auth_layer, edge_required_role, edge_session_frame_required_role, ws_authorized,
};
use crate::transport::edge_auth::{
    edge_auth_layer, Config as EdgeAuthConfig, TrustedOperator, HEADER_AUDIT_ID, HEADER_AUTH_MODE,
    HEADER_OPERATOR, HEADER_ROLES, HEADER_SIGNATURE_V2, HEADER_TIMESTAMP, HEADER_VERSION,
};

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
        state_dir: None,
        api_key_file: None,
        dev_mode: false,
        base_path: None,
        #[cfg(debug_assertions)]
        hot: false,
    }
}

fn test_state(config: MuxConfig) -> Arc<MuxState> {
    // MuxState builds a reqwest client, which needs a rustls CryptoProvider
    // installed. main.rs installs it at startup; mirror that here so the lib
    // unit tests don't panic with "No provider set". Idempotent — returns Err
    // (ignored) if another test already installed it.
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None);
    Arc::new(MuxState::new(config, CancellationToken::new(), broker))
}

fn app(state: Arc<MuxState>) -> Router {
    Router::new()
        .route("/api/v1/credentials/status", get(|| async { "ok" }))
        .route("/api/v1/health", get(|| async { "healthy" }))
        .route("/metrics", get(|| async { "# metrics" }))
        .layer(middleware::from_fn_with_state(state.clone(), auth_layer))
        .with_state(state)
}

// role=Some => a verified v2 envelope carrying that role. role=None => a
// cryptographically verified v1 envelope (PAT/AVA/cookie/bridge), which under
// additive enforcement (ka-0000001641) retains full pre-MR authority.
fn edge_identity(role: Option<&str>) -> TrustedOperator {
    TrustedOperator {
        operator: "operator@example.com".to_owned(),
        audit_id: "audit-1".to_owned(),
        version: if role.is_some() { 2 } else { 1 },
        auth_mode: role.map(|_| "entra".to_owned()).unwrap_or_default(),
        roles: role.map(|r| vec![r.to_owned()]).unwrap_or_default(),
        verified_v2: role.is_some(),
        verified_v1: role.is_none(),
        relay: false,
    }
}

fn edge_app(state: Arc<MuxState>, identity: TrustedOperator) -> Router {
    Router::new()
        .route("/api/v1/sessions", get(|| async { "read" }).post(|| async { "register" }))
        .route("/api/v1/sessions/s1/input", post(|| async { "input" }))
        .route("/api/v1/sessions/s1/upload", post(|| async { "upload" }))
        .route("/ws/mux", get(|| async { "mux" }))
        .route("/ws/s1", get(|| async { "session" }))
        .layer(middleware::from_fn_with_state(state.clone(), auth_layer))
        .layer(Extension(identity))
        .with_state(state)
}

fn edge_wire_app(state: Arc<MuxState>) -> Router {
    Router::new()
        .route("/api/v1/sessions/s1/input", post(|| async { "input" }))
        .layer(middleware::from_fn_with_state(state.clone(), auth_layer))
        .layer(middleware::from_fn_with_state(state.clone(), edge_auth_layer))
        .with_state(state)
}

fn signed_v2_request(role: &str, method: &str, path: &str) -> anyhow::Result<Request<Body>> {
    let ts = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs().to_string();
    let operator = "operator@example.com";
    let audit_id = "audit-response-1";
    let auth_mode = "entra";
    let message =
        format!("v2\n{operator}\n{auth_mode}\n{role}\n{audit_id}\n{method}\n{path}\n{ts}");
    let key = hmac::Key::new(hmac::HMAC_SHA256, b"edge-test-key");
    let signature = B64.encode(hmac::sign(&key, message.as_bytes()).as_ref());
    Ok(Request::builder()
        .method(method)
        .uri(path)
        .header(HEADER_OPERATOR, operator)
        .header(HEADER_AUDIT_ID, audit_id)
        .header(HEADER_TIMESTAMP, ts)
        .header(HEADER_VERSION, "2")
        .header(HEADER_AUTH_MODE, auth_mode)
        .header(HEADER_ROLES, role)
        .header(HEADER_SIGNATURE_V2, signature)
        .body(Body::empty())?)
}

// ── No auth, no credentials → allow ──────────────────────────────────────

#[tokio::test]
async fn no_auth_no_credentials_allows_access() -> anyhow::Result<()> {
    let state = test_state(test_config());
    let resp = app(state)
        .oneshot(Request::builder().uri("/api/v1/credentials/status").body(Body::empty())?)
        .await?;
    assert_eq!(resp.status(), StatusCode::OK);
    Ok(())
}

// ── No auth, credentials present → deny (fail-closed) ───────────────────

#[tokio::test]
async fn no_auth_with_credentials_denies_access() -> anyhow::Result<()> {
    let state = test_state(test_config());

    // Add a credential account.
    let config = AccountConfig {
        name: "test-acct".into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: false,
    };
    state.credential_broker.add_account(config, Some("sk-test-key".into()), None, None).await?;

    let resp = app(state)
        .oneshot(Request::builder().uri("/api/v1/credentials/status").body(Body::empty())?)
        .await?;
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
    Ok(())
}

// ── Dev mode bypasses fail-closed ────────────────────────────────────────

#[tokio::test]
async fn dev_mode_bypasses_fail_closed() -> anyhow::Result<()> {
    let mut config = test_config();
    config.dev_mode = true;
    let state = test_state(config);

    // Add a credential account.
    let acct = AccountConfig {
        name: "test-acct".into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: false,
    };
    state.credential_broker.add_account(acct, Some("sk-test".into()), None, None).await?;

    let resp = app(state)
        .oneshot(Request::builder().uri("/api/v1/credentials/status").body(Body::empty())?)
        .await?;
    assert_eq!(resp.status(), StatusCode::OK);
    Ok(())
}

// ── Health endpoint always exempt ────────────────────────────────────────

#[tokio::test]
async fn health_endpoint_exempt_with_credentials() -> anyhow::Result<()> {
    let state = test_state(test_config());

    let acct = AccountConfig {
        name: "test-acct".into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: false,
    };
    state.credential_broker.add_account(acct, Some("sk-test".into()), None, None).await?;

    let resp =
        app(state).oneshot(Request::builder().uri("/api/v1/health").body(Body::empty())?).await?;
    assert_eq!(resp.status(), StatusCode::OK);
    Ok(())
}

// ── Metrics endpoint always exempt (ka-0000000370) ───────────────────────
//
// vmagent scrapes `/metrics` directly at the pod with no Authorization
// header. coopmux always runs with an auth token + credentials in prod, so
// without the exemption the scrape 401s and `up{component="coopmux"}` reads 0
// forever. Pin the exemption for BOTH the static-token and no-auth-with-creds
// (fail-closed) paths — both are 401 for any non-exempt route.

#[tokio::test]
async fn metrics_endpoint_exempt_with_credentials_and_token() -> anyhow::Result<()> {
    let mut config = test_config();
    config.auth_token = Some("secret-token".into());
    let state = test_state(config);

    let acct = AccountConfig {
        name: "test-acct".into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: false,
    };
    state.credential_broker.add_account(acct, Some("sk-test".into()), None, None).await?;

    // A non-exempt route with no token → 401 (proves auth is active).
    let resp = app(state.clone())
        .oneshot(Request::builder().uri("/api/v1/credentials/status").body(Body::empty())?)
        .await?;
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);

    // /metrics with no Authorization header → still 200 (exempt).
    let resp = app(state).oneshot(Request::builder().uri("/metrics").body(Body::empty())?).await?;
    assert_eq!(resp.status(), StatusCode::OK);
    Ok(())
}

#[tokio::test]
async fn metrics_endpoint_exempt_fail_closed() -> anyhow::Result<()> {
    // No auth token configured but credentials present → fail-closed on every
    // non-exempt route. /metrics must still be reachable.
    let state = test_state(test_config());

    let acct = AccountConfig {
        name: "test-acct".into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: false,
    };
    state.credential_broker.add_account(acct, Some("sk-test".into()), None, None).await?;

    // Non-exempt route → denied (fail-closed).
    let resp = app(state.clone())
        .oneshot(Request::builder().uri("/api/v1/credentials/status").body(Body::empty())?)
        .await?;
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);

    // /metrics → allowed.
    let resp = app(state).oneshot(Request::builder().uri("/metrics").body(Body::empty())?).await?;
    assert_eq!(resp.status(), StatusCode::OK);
    Ok(())
}

// ── Static token auth works when configured ──────────────────────────────

#[tokio::test]
async fn static_token_auth_allows_access() -> anyhow::Result<()> {
    let mut config = test_config();
    config.auth_token = Some("secret-token".into());
    let state = test_state(config);

    let acct = AccountConfig {
        name: "test-acct".into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: false,
    };
    state.credential_broker.add_account(acct, Some("sk-test".into()), None, None).await?;

    // Without token → denied
    let resp = app(state.clone())
        .oneshot(Request::builder().uri("/api/v1/credentials/status").body(Body::empty())?)
        .await?;
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);

    // With valid token → allowed
    let resp = app(state)
        .oneshot(
            Request::builder()
                .uri("/api/v1/credentials/status")
                .header("authorization", "Bearer secret-token")
                .body(Body::empty())?,
        )
        .await?;
    assert_eq!(resp.status(), StatusCode::OK);
    Ok(())
}

#[tokio::test]
async fn edge_role_matrix_denies_viewer_mutations() -> anyhow::Result<()> {
    let state = test_state(test_config());

    // A verified v2 viewer envelope reads but cannot mutate — v2 role
    // enforcement is intact.
    let identity = edge_identity(Some("gardenio-viewer"));
    let response = edge_app(Arc::clone(&state), identity.clone())
        .oneshot(Request::builder().method("GET").uri("/api/v1/sessions").body(Body::empty())?)
        .await?;
    assert_eq!(response.status(), StatusCode::OK);

    let response = edge_app(Arc::clone(&state), identity)
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/api/v1/sessions/s1/input")
                .body(Body::empty())?,
        )
        .await?;
    assert_eq!(response.status(), StatusCode::FORBIDDEN);
    Ok(())
}

// Additive enforcement (ka-0000001641): a cryptographically verified v1
// envelope keeps EXACTLY its pre-MR authority — it can drive an operator-only
// mutation (input) that a v2 viewer above is denied. This is the coopmux mirror
// of the regression the MR introduced: v1 callers (PAT/AVA/cookie/bridge) must
// not be silently downgraded to viewer-only.
#[tokio::test]
async fn edge_verified_v1_keeps_pre_mr_mutation_authority() -> anyhow::Result<()> {
    let state = test_state(test_config());
    let identity = edge_identity(None);

    let response = edge_app(Arc::clone(&state), identity.clone())
        .oneshot(Request::builder().method("GET").uri("/api/v1/sessions").body(Body::empty())?)
        .await?;
    assert_eq!(response.status(), StatusCode::OK);

    // Operator-level mutation: allowed for verified v1 (pre-MR authority).
    let response = edge_app(Arc::clone(&state), identity.clone())
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/api/v1/sessions/s1/input")
                .body(Body::empty())?,
        )
        .await?;
    assert_eq!(response.status(), StatusCode::OK);

    // Admin-level mutation (upload): also allowed — before this MR any verified
    // envelope skipped the role gate entirely.
    let response = edge_app(Arc::clone(&state), identity)
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/api/v1/sessions/s1/upload")
                .body(Body::empty())?,
        )
        .await?;
    assert_eq!(response.status(), StatusCode::OK);
    Ok(())
}

#[tokio::test]
async fn edge_role_matrix_operator_and_admin_boundaries() -> anyhow::Result<()> {
    let state = test_state(test_config());
    let operator = edge_identity(Some("gardenio-operator"));

    let response = edge_app(Arc::clone(&state), operator.clone())
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/api/v1/sessions/s1/input")
                .body(Body::empty())?,
        )
        .await?;
    assert_eq!(response.status(), StatusCode::OK);

    let response = edge_app(Arc::clone(&state), operator.clone())
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/api/v1/sessions/s1/upload")
                .body(Body::empty())?,
        )
        .await?;
    assert_eq!(response.status(), StatusCode::FORBIDDEN);

    let response = edge_app(Arc::clone(&state), operator)
        .oneshot(Request::builder().method("GET").uri("/ws/s1").body(Body::empty())?)
        .await?;
    assert_eq!(response.status(), StatusCode::OK);

    let admin = edge_identity(Some("gardenio-admin"));
    for (method, path) in [("POST", "/api/v1/sessions"), ("POST", "/api/v1/sessions/s1/upload")] {
        let response = edge_app(Arc::clone(&state), admin.clone())
            .oneshot(Request::builder().method(method).uri(path).body(Body::empty())?)
            .await?;
        assert_eq!(response.status(), StatusCode::OK, "{method} {path}");
    }
    Ok(())
}

#[tokio::test]
async fn edge_wire_envelope_authorizes_and_echoes_audit_id() -> anyhow::Result<()> {
    let mut state = test_state(test_config());
    let state_mut = Arc::get_mut(&mut state)
        .ok_or_else(|| anyhow::anyhow!("test state unexpectedly shared"))?;
    state_mut.edge_auth_cfg = EdgeAuthConfig {
        hmac_key: b"edge-test-key".to_vec(),
        enforce: true,
        ..EdgeAuthConfig::default()
    };

    let response = edge_wire_app(state)
        .oneshot(signed_v2_request("gardenio-operator", "POST", "/api/v1/sessions/s1/input")?)
        .await?;
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        response.headers().get(HEADER_AUDIT_ID).and_then(|value| value.to_str().ok()),
        Some("audit-response-1")
    );
    Ok(())
}

// gd-yDXrcBd0Uq: a mux with NO DAEMON_HMAC_KEY (the k3s posture) used to trust a
// bare, unsigned `X-Operator` header at viewer level, so any pod could read every
// session without the bearer. It must now be refused outright, while the bearer
// path is untouched.
#[tokio::test]
async fn unsigned_x_operator_without_hmac_key_is_rejected() -> anyhow::Result<()> {
    let mut config = test_config();
    config.auth_token = Some("secret-token".into());
    let state = test_state(config);
    assert!(!state.edge_auth_cfg.enabled(), "premise: no HMAC key configured");

    let bare = Request::builder()
        .method("POST")
        .uri("/api/v1/sessions/s1/input")
        .header(HEADER_OPERATOR, "x")
        .body(Body::empty())?;
    let response = edge_wire_app(Arc::clone(&state)).oneshot(bare).await?;
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);

    // Forging the other envelope headers does not help without a key to verify them.
    let forged = Request::builder()
        .method("POST")
        .uri("/api/v1/sessions/s1/input")
        .header(HEADER_OPERATOR, "x")
        .header(HEADER_VERSION, "2")
        .header(HEADER_AUTH_MODE, "entra")
        .header(HEADER_ROLES, "gardenio-admin")
        .header(HEADER_TIMESTAMP, "0")
        .header(HEADER_SIGNATURE_V2, "AAAA")
        .body(Body::empty())?;
    let response = edge_wire_app(Arc::clone(&state)).oneshot(forged).await?;
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);

    // No header: the bearer still gates, and a valid bearer still works.
    let no_token =
        Request::builder().method("POST").uri("/api/v1/sessions/s1/input").body(Body::empty())?;
    let response = edge_wire_app(Arc::clone(&state)).oneshot(no_token).await?;
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);

    let with_token = Request::builder()
        .method("POST")
        .uri("/api/v1/sessions/s1/input")
        .header("authorization", "Bearer secret-token")
        .body(Body::empty())?;
    let response = edge_wire_app(state).oneshot(with_token).await?;
    assert_eq!(response.status(), StatusCode::OK);
    Ok(())
}

// ── WebSocket authorization: edge-auth envelope honoured (ka-0000003786) ──
//
// WS upgrades bypass auth_layer, so each handler re-runs the auth checks via
// ws_authorized. The regression that broke chat for browsers reaching coopmux
// through the unified-host shim was that the WS path ignored the
// TrustedOperator envelope the shim stamps. These tests pin the precedence so
// the helper stays in lockstep with auth_layer.

#[tokio::test]
async fn ws_authorized_honors_edge_auth_envelope() -> anyhow::Result<()> {
    // Static token configured, browser sends NO ?token= and NO Authorization
    // (the shim strips it) — but the edge-auth envelope verified upstream.
    let mut config = test_config();
    config.auth_token = Some("secret-token".into());
    let state = test_state(config);

    let headers = HeaderMap::new();
    // Without the envelope and without a token → denied.
    assert!(ws_authorized(&state, &headers, None, false).await.is_none());
    // With the envelope (edge_trusted=true) → allowed even with no token.
    assert!(ws_authorized(&state, &headers, None, true).await.is_some());
    Ok(())
}

#[tokio::test]
async fn ws_authorized_accepts_query_token() -> anyhow::Result<()> {
    let mut config = test_config();
    config.auth_token = Some("secret-token".into());
    let state = test_state(config);

    let headers = HeaderMap::new();
    assert!(ws_authorized(&state, &headers, Some("secret-token"), false).await.is_some());
    assert!(ws_authorized(&state, &headers, Some("wrong-token"), false).await.is_none());
    Ok(())
}

#[tokio::test]
async fn ws_authorized_accepts_bearer_header() -> anyhow::Result<()> {
    let mut config = test_config();
    config.auth_token = Some("secret-token".into());
    let state = test_state(config);

    let mut headers = HeaderMap::new();
    headers.insert("authorization", "Bearer secret-token".parse()?);
    assert!(ws_authorized(&state, &headers, None, false).await.is_some());
    Ok(())
}

// ── Per-session control-frame tier boundary (ka-0000002552) ───────────────
//
// The per-session `/ws/{id}` upgrade is only OPERATOR-gated, but each frame is
// forwarded verbatim to the upstream coop /ws, which executes admin-reserved
// actions. `edge_session_frame_required_role` restores the tier boundary the
// REST matrix defines. These pin the mapping so a future frame variant cannot
// silently inherit operator authority.

#[test]
fn session_frame_reads_are_viewer() {
    for body in [
        r#"{"event":"screen:get"}"#,
        r#"{"event":"replay:get","offset":0}"#,
        r#"{"event":"status:get"}"#,
        r#"{"event":"agent:get"}"#,
        r#"{"event":"health:get"}"#,
        r#"{"event":"transcript:list"}"#,
        r#"{"event":"usage:get"}"#,
        r#"{"event":"ping"}"#,
    ] {
        assert_eq!(edge_session_frame_required_role(body), "gardenio-viewer", "{body}");
    }
}

#[test]
fn session_frame_terminal_input_is_operator() {
    for body in [
        r#"{"event":"input:send","text":"pwd","enter":true}"#,
        r#"{"event":"keys:send","keys":["Enter"]}"#,
        r#"{"event":"mouse:send","button":"left","col":1,"row":1}"#,
        r#"{"event":"resize","cols":80,"rows":24}"#,
        r#"{"event":"nudge","message":"hi"}"#,
        r#"{"event":"respond","option":1}"#,
    ] {
        assert_eq!(edge_session_frame_required_role(body), "gardenio-operator", "{body}");
    }
}

#[test]
fn session_frame_raw_signal_lifecycle_are_admin() {
    for body in [
        r#"{"event":"input:send:raw","data":"cm0gLXJm"}"#,
        r#"{"event":"signal:send","signal":"KILL"}"#,
        r#"{"event":"shutdown"}"#,
        r#"{"event":"session:restart"}"#,
        r#"{"event":"session:switch","force":true}"#,
    ] {
        assert_eq!(edge_session_frame_required_role(body), "gardenio-admin", "{body}");
    }
}

#[test]
fn session_frame_unknown_or_malformed_fails_closed_to_admin() {
    for body in [
        r#"{"event":"future:control"}"#,               // unknown variant
        r#"{"event":"recording:put","enabled":true}"#, // config mutation
        r#"{"event":"profiles:register","profiles":[]}"#,
        r#"{"event":"auth","token":"x"}"#,
        r#"{"no_event":"here"}"#, // missing event tag
        r#"not json at all"#,
        r#""#,   // empty body
        r#"{}"#, // empty object
    ] {
        assert_eq!(edge_session_frame_required_role(body), "gardenio-admin", "{body}");
    }
}

#[test]
fn per_session_upgrade_route_is_operator_floor() {
    // The upgrade gate stays OPERATOR; the ADMIN reservation is per-frame.
    assert_eq!(edge_required_role("GET", "/ws/s1"), "gardenio-operator");
    assert_eq!(edge_required_role("GET", "/ws/mux"), "gardenio-viewer");
    assert_eq!(edge_required_role("POST", "/sse/session/s1/send"), "gardenio-operator");
}

// ── Finding 3: viewer cannot even open a per-session socket ───────────────
//
// The synthetic `edge_app` router carries the real `auth_layer`, so this
// exercises the actual upgrade gate (not a stub) for the `/ws/{id}` route.

#[tokio::test]
async fn edge_role_matrix_viewer_denied_per_session_ws_upgrade() -> anyhow::Result<()> {
    let state = test_state(test_config());
    let viewer = edge_identity(Some("gardenio-viewer"));

    let response = edge_app(Arc::clone(&state), viewer)
        .oneshot(Request::builder().method("GET").uri("/ws/s1").body(Body::empty())?)
        .await?;
    assert_eq!(response.status(), StatusCode::FORBIDDEN);
    Ok(())
}

// ── Shared tier fixture: Go relay port must not drift (Lands Glass 6.1) ───
//
// The Lands Glass relay (gardenio/controller/internal/glass/tiers.go) applies
// a Go port of `edge_required_role` and `edge_session_frame_required_role`
// before forwarding to coopmux, which re-checks the originals. One fixture,
// `tests/fixtures/edge_tiers.json`, is asserted here and by the Go test
// `TestTiersMatchSharedFixture`, so an edit to either table that is not
// mirrored in the other reds one side of CI.

const EDGE_TIERS_FIXTURE: &str = include_str!("../../tests/fixtures/edge_tiers.json");

#[derive(serde::Deserialize)]
struct EdgeTierFixture {
    routes: Vec<EdgeTierRouteCase>,
    session_frames: Vec<EdgeTierFrameCase>,
}

#[derive(serde::Deserialize)]
struct EdgeTierRouteCase {
    method: String,
    path: String,
    role: String,
}

#[derive(serde::Deserialize)]
struct EdgeTierFrameCase {
    body: String,
    role: String,
}

#[test]
fn edge_tier_fixture_pins_both_tables() {
    let fixture: EdgeTierFixture =
        serde_json::from_str(EDGE_TIERS_FIXTURE).expect("edge_tiers.json parses");
    assert!(!fixture.routes.is_empty(), "fixture has no route cases");
    assert!(!fixture.session_frames.is_empty(), "fixture has no frame cases");

    for case in &fixture.routes {
        assert_eq!(
            edge_required_role(&case.method, &case.path),
            case.role,
            "edge_required_role({:?}, {:?})",
            case.method,
            case.path
        );
    }
    for case in &fixture.session_frames {
        assert_eq!(
            edge_session_frame_required_role(&case.body),
            case.role,
            "edge_session_frame_required_role({:?})",
            case.body
        );
    }

    // Every tier appears in each table, so dropping a whole tier from one
    // side cannot pass trivially.
    for role in ["gardenio-viewer", "gardenio-operator", "gardenio-admin"] {
        assert!(fixture.routes.iter().any(|c| c.role == role), "no {role} route case");
        assert!(fixture.session_frames.iter().any(|c| c.role == role), "no {role} frame case");
    }
}
