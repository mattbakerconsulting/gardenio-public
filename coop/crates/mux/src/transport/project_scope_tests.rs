// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::AtomicU32;
use std::sync::{Arc, OnceLock};
use std::time::{SystemTime, UNIX_EPOCH};

use aws_lc_rs::rand::SystemRandom;
use aws_lc_rs::rsa::KeySize;
use aws_lc_rs::signature::{KeyPair as _, RsaKeyPair, RSA_PKCS1_SHA256};
use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::middleware;
use axum::routing::{get, post};
use axum::Router;
use base64::engine::general_purpose::URL_SAFE_NO_PAD as B64URL;
use base64::Engine as _;
use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

use super::*;
use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{CredentialConfig, CredentialEvent};
use crate::state::{CachedScreen, SessionEntry, SessionTransport};
use crate::transport::auth::{auth_layer, ws_authorized};
use crate::transport::oidc::{OidcClaims, OidcVerifier};
use crate::transport::{http, sse};

const ISSUER: &str = "https://keycloak.test/realms/gardenio";
const AUDIENCE: &str = "coopmux";
const KID: &str = "test-kid";
const STATIC_TOKEN: &str = "static-token";

fn claims(roles: &[&str], groups: &[&str]) -> OidcClaims {
    OidcClaims {
        roles: roles.iter().map(|s| (*s).to_owned()).collect(),
        groups: groups.iter().map(|s| (*s).to_owned()).collect(),
    }
}

fn strings(v: &[&str]) -> Vec<String> {
    v.iter().map(|s| (*s).to_owned()).collect()
}

// ---- scope resolution (mirrors kbeads isScopedPrincipal/scopeFromContext) ----

#[test]
fn project_member_is_scoped_to_its_groups() {
    let scope = SessionScope::from_claims(&claims(
        &["gardenio-project-member"],
        &["/project-beta", "/project-alpha", "/project-alpha", "/other", "/project-"],
    ));
    assert_eq!(scope, SessionScope::Projects(strings(&["alpha", "beta"])));
}

#[test]
fn project_member_without_groups_admits_nothing() {
    let scope = SessionScope::from_claims(&claims(&["gardenio-project-member"], &[]));
    assert_eq!(scope, SessionScope::Projects(vec![]));
    assert!(!scope.allows_metadata(&serde_json::json!({"project": "alpha"})));
    assert!(!scope.allows_metadata(&serde_json::json!({"project": ""})));
}

#[test]
fn admin_is_never_scoped() {
    let scope = SessionScope::from_claims(&claims(
        &["gardenio-project-member", "gardenio-admin"],
        &["/project-alpha"],
    ));
    assert_eq!(scope, SessionScope::Unrestricted);
}

#[test]
fn operator_member_is_still_scoped() {
    // kbeads scopes every non-admin project member, whatever else it holds.
    let scope = SessionScope::from_claims(&claims(
        &["gardenio-operator", "gardenio-project-member"],
        &["/project-alpha"],
    ));
    assert!(scope.is_scoped());
}

#[test]
fn non_member_tokens_keep_prior_authority() {
    assert_eq!(
        SessionScope::from_claims(&claims(&["gardenio-operator"], &["/project-alpha"])),
        SessionScope::Unrestricted
    );
    assert_eq!(SessionScope::from_claims(&claims(&[], &[])), SessionScope::Unrestricted);
}

#[test]
fn groups_accept_paths_and_bare_names() {
    assert_eq!(
        groups_to_projects(&strings(&["/project-a", "project-b", "/team-c", "project-"])),
        strings(&["a", "b"])
    );
}

#[test]
fn metadata_match_is_exact() {
    let scope = SessionScope::Projects(strings(&["alpha"]));
    assert!(scope.allows_metadata(&serde_json::json!({"project": "alpha"})));
    assert!(!scope.allows_metadata(&serde_json::json!({"project": "alphabet"})));
    assert!(!scope.allows_metadata(&serde_json::json!({"project": ["alpha"]})));
    assert!(!scope.allows_metadata(&serde_json::json!({})));
    assert!(!scope.allows_metadata(&serde_json::Value::Null));
    assert!(SessionScope::Unrestricted.allows_metadata(&serde_json::Value::Null));
}

#[test]
fn scoped_role_cap_is_operator() {
    let scope = SessionScope::Projects(vec![]);
    assert!(scope.allows_role("gardenio-viewer"));
    assert!(scope.allows_role("gardenio-operator"));
    assert!(!scope.allows_role("gardenio-admin"));
    assert!(!scope.allows_role("something-new"));
    assert!(SessionScope::Unrestricted.allows_role("gardenio-admin"));
}

#[test]
fn scoped_routes() {
    assert!(matches!(scoped_route("/api/v1/sessions"), Some(ScopedRoute::List)));
    assert!(matches!(scoped_route("/api/v1/sessions/s1/screen"), Some(ScopedRoute::Session("s1"))));
    assert!(matches!(
        scoped_route("/api/v1/sessions/s1/transcripts/3"),
        Some(ScopedRoute::Session("s1"))
    ));
    for path in [
        "/api/v1/sessions/s1",
        "/api/v1/sessions/",
        "/api/v1/sessions//screen",
        "/api/v1/sessions/launch",
        "/api/v1/sessions/launch/x",
        "/api/v1/credentials/status",
        "/api/v1/config/launch",
        "/api/v1/version",
        "/mux",
        "/",
    ] {
        assert!(scoped_route(path).is_none(), "{path} should not be a scoped route");
    }
}

#[test]
fn claims_merge_both_issuers() {
    let parsed = OidcClaims::from_payload(&serde_json::json!({
        "realm_access": {"roles": ["gardenio-project-member", "offline_access"]},
        "realm_roles": ["gardenio-project-member", "gardenio-viewer"],
        "groups": ["/project-alpha"],
        "groups_direct": ["/project-beta", "/project-alpha"],
    }));
    assert_eq!(
        parsed.roles,
        strings(&["gardenio-project-member", "offline_access", "gardenio-viewer"])
    );
    assert_eq!(parsed.groups, strings(&["/project-alpha", "/project-beta"]));
}

#[test]
fn malformed_group_claim_never_widens_scope() {
    // A non-array groups claim reads as no groups: scoped to nothing.
    let parsed = OidcClaims::from_payload(&serde_json::json!({
        "realm_roles": ["gardenio-project-member"],
        "groups": "/project-alpha",
    }));
    assert!(parsed.groups.is_empty());
    assert_eq!(SessionScope::from_claims(&parsed), SessionScope::Projects(vec![]));

    // A stray non-string entry is skipped and must not discard the roles.
    let parsed = OidcClaims::from_payload(&serde_json::json!({
        "realm_roles": ["gardenio-project-member"],
        "groups": ["/project-alpha", 7],
    }));
    assert_eq!(SessionScope::from_claims(&parsed), SessionScope::Projects(strings(&["alpha"])));
}

// ---- end to end through auth_layer with a real RS256 token ----

struct TestIssuer {
    key: RsaKeyPair,
    jwks: jsonwebtoken::jwk::JwkSet,
}

/// One RSA key for the whole suite: generating it is the slow part.
fn issuer() -> anyhow::Result<&'static TestIssuer> {
    static ISSUER_KEY: OnceLock<Option<TestIssuer>> = OnceLock::new();
    ISSUER_KEY
        .get_or_init(|| {
            let key = RsaKeyPair::generate(KeySize::Rsa2048).ok()?;
            let public = key.public_key();
            let jwks = serde_json::from_value(serde_json::json!({
                "keys": [{
                    "kty": "RSA",
                    "kid": KID,
                    "alg": "RS256",
                    "use": "sig",
                    "n": B64URL.encode(public.modulus().big_endian_without_leading_zero()),
                    "e": B64URL.encode(public.exponent().big_endian_without_leading_zero()),
                }]
            }))
            .ok()?;
            Some(TestIssuer { key, jwks })
        })
        .as_ref()
        .ok_or_else(|| anyhow::anyhow!("test issuer key setup failed"))
}

fn sign(payload: &serde_json::Value) -> anyhow::Result<String> {
    let issuer = issuer()?;
    let header = serde_json::json!({"alg": "RS256", "typ": "JWT", "kid": KID});
    let signing_input =
        format!("{}.{}", B64URL.encode(header.to_string()), B64URL.encode(payload.to_string()));
    let mut sig = vec![0u8; issuer.key.public_modulus_len()];
    issuer
        .key
        .sign(&RSA_PKCS1_SHA256, &SystemRandom::new(), signing_input.as_bytes(), &mut sig)
        .map_err(|_| anyhow::anyhow!("sign failed"))?;
    Ok(format!("{signing_input}.{}", B64URL.encode(sig)))
}

fn token(roles: &[&str], groups: &[&str]) -> anyhow::Result<String> {
    let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs();
    sign(&serde_json::json!({
        "iss": ISSUER,
        "aud": AUDIENCE,
        "iat": now,
        "exp": now + 300,
        "realm_roles": roles,
        "groups": groups,
    }))
}

fn member_token(lands: &[&str]) -> anyhow::Result<String> {
    let groups: Vec<String> = lands.iter().map(|l| format!("/project-{l}")).collect();
    let groups: Vec<&str> = groups.iter().map(String::as_str).collect();
    token(&["gardenio-project-member"], &groups)
}

fn test_config() -> MuxConfig {
    MuxConfig {
        host: "127.0.0.1".into(),
        port: 0,
        auth_token: Some(STATIC_TOKEN.into()),
        oidc_issuer_url: Some(ISSUER.into()),
        oidc_client_id: Some(AUDIENCE.into()),
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

fn entry(id: &str, metadata: serde_json::Value) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url: "http://127.0.0.1:9".into(),
        auth_token: None,
        metadata,
        registered_at: std::time::Instant::now(),
        cached_screen: RwLock::new(Some(CachedScreen {
            lines: vec![format!("screen of {id}")],
            ansi: vec![],
            cols: 80,
            rows: 24,
            alt_screen: false,
            seq: 1,
            fetched_at: 0,
        })),
        cached_status: RwLock::new(None),
        cached_usage: RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: RwLock::new(None),
        assigned_account: RwLock::new(Some("acct-1".into())),
        claimed_bead_title: RwLock::new(None),
        transport: SessionTransport::Http,
    })
}

/// State with three sessions: one in land `alpha`, one in `beta`, and one
/// registered without a project.
async fn test_state() -> anyhow::Result<Arc<MuxState>> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None);
    let mut state = MuxState::new(test_config(), CancellationToken::new(), broker);
    state.oidc_verifier =
        Some(Arc::new(OidcVerifier::from_jwks(ISSUER, AUDIENCE, issuer()?.jwks.clone())));
    let state = Arc::new(state);
    {
        let mut sessions = state.sessions.write().await;
        sessions.insert("a1".into(), entry("a1", serde_json::json!({"project": "alpha"})));
        sessions.insert("b1".into(), entry("b1", serde_json::json!({"project": "beta"})));
        sessions.insert("n1".into(), entry("n1", serde_json::json!({"role": "crew"})));
    }
    Ok(state)
}

fn app(state: Arc<MuxState>) -> Router {
    Router::new()
        .route("/api/v1/sessions", get(http::list_sessions))
        .route("/api/v1/sessions/{id}", axum::routing::delete(|| async { "deleted" }))
        .route("/api/v1/sessions/{id}/screen", get(http::session_screen))
        .route("/api/v1/sessions/{id}/input", post(|| async { "typed" }))
        .route("/api/v1/sessions/{id}/input/raw", post(|| async { "raw" }))
        .route("/api/v1/sessions/launch", post(|| async { "launched" }))
        .route("/api/v1/credentials/status", get(|| async { "creds" }))
        .route("/sse/mux", get(sse::sse_mux_handler))
        .route("/sse/session/{session_id}", get(sse::sse_session_handler))
        .layer(middleware::from_fn_with_state(state.clone(), auth_layer))
        .with_state(state)
}

async fn send(
    state: &Arc<MuxState>,
    method: &str,
    uri: &str,
    bearer: &str,
) -> anyhow::Result<(StatusCode, String)> {
    let req = Request::builder()
        .method(method)
        .uri(uri)
        .header("authorization", format!("Bearer {bearer}"))
        .body(Body::empty())?;
    let resp = app(state.clone()).oneshot(req).await?;
    let status = resp.status();
    let body = axum::body::to_bytes(resp.into_body(), 1 << 20).await?;
    Ok((status, String::from_utf8_lossy(&body).into_owned()))
}

fn listed(body: &str) -> anyhow::Result<(Vec<String>, Vec<serde_json::Value>)> {
    let list: Vec<serde_json::Value> = serde_json::from_str(body)?;
    let mut ids: Vec<String> =
        list.iter().filter_map(|s| s["id"].as_str().map(str::to_owned)).collect();
    ids.sort();
    Ok((ids, list))
}

#[tokio::test]
async fn scoped_list_shows_only_own_land_without_accounts() -> anyhow::Result<()> {
    let state = test_state().await?;
    let (status, body) =
        send(&state, "GET", "/api/v1/sessions", &member_token(&["alpha"])?).await?;
    assert_eq!(status, StatusCode::OK);
    let (ids, list) = listed(&body)?;
    assert_eq!(ids, strings(&["a1"]));
    assert!(list[0]["assigned_account"].is_null(), "scoped list leaked account: {body}");
    Ok(())
}

#[tokio::test]
async fn member_of_two_lands_sees_both() -> anyhow::Result<()> {
    let state = test_state().await?;
    let bearer = member_token(&["alpha", "beta"])?;
    let (_, body) = send(&state, "GET", "/api/v1/sessions", &bearer).await?;
    assert_eq!(listed(&body)?.0, strings(&["a1", "b1"]));
    Ok(())
}

#[tokio::test]
async fn member_with_no_groups_sees_nothing() -> anyhow::Result<()> {
    let state = test_state().await?;
    let (status, body) = send(&state, "GET", "/api/v1/sessions", &member_token(&[])?).await?;
    assert_eq!(status, StatusCode::OK);
    assert!(listed(&body)?.0.is_empty());
    Ok(())
}

#[tokio::test]
async fn unrestricted_callers_still_see_everything() -> anyhow::Result<()> {
    let state = test_state().await?;
    for bearer in [
        STATIC_TOKEN.to_owned(),
        token(&["gardenio-admin", "gardenio-project-member"], &["/project-alpha"])?,
        token(&["gardenio-operator"], &[])?,
    ] {
        let (status, body) = send(&state, "GET", "/api/v1/sessions", &bearer).await?;
        assert_eq!(status, StatusCode::OK);
        let (ids, list) = listed(&body)?;
        assert_eq!(ids, strings(&["a1", "b1", "n1"]));
        assert_eq!(list[0]["assigned_account"], "acct-1");
    }
    Ok(())
}

#[tokio::test]
async fn scoped_session_routes_stay_in_land() -> anyhow::Result<()> {
    let state = test_state().await?;
    let alpha = member_token(&["alpha"])?;
    let (status, body) = send(&state, "GET", "/api/v1/sessions/a1/screen", &alpha).await?;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert!(body.contains("screen of a1"));
    let (status, _) = send(&state, "POST", "/api/v1/sessions/a1/input", &alpha).await?;
    assert_eq!(status, StatusCode::OK);

    // Another land's session, a project-less one and an unknown id all 404 alike.
    for id in ["b1", "n1", "zz"] {
        let uri = format!("/api/v1/sessions/{id}/screen");
        assert_eq!(send(&state, "GET", &uri, &alpha).await?.0, StatusCode::NOT_FOUND, "{uri}");
        let uri = format!("/api/v1/sessions/{id}/input");
        assert_eq!(send(&state, "POST", &uri, &alpha).await?.0, StatusCode::NOT_FOUND, "{uri}");
    }
    Ok(())
}

#[tokio::test]
async fn scoped_caller_is_capped_at_operator() -> anyhow::Result<()> {
    let state = test_state().await?;
    let alpha = member_token(&["alpha"])?;
    for (method, uri) in [
        ("POST", "/api/v1/sessions/a1/input/raw"),
        ("DELETE", "/api/v1/sessions/a1"),
        ("POST", "/api/v1/sessions/launch"),
        ("POST", "/api/v1/sessions"),
        ("GET", "/api/v1/credentials/status"),
    ] {
        let (status, _) = send(&state, method, uri, &alpha).await?;
        assert_eq!(status, StatusCode::FORBIDDEN, "{method} {uri}");
    }
    // The same route is open to an admin token.
    let admin = token(&["gardenio-admin"], &[])?;
    let (status, _) = send(&state, "POST", "/api/v1/sessions/a1/input/raw", &admin).await?;
    assert_eq!(status, StatusCode::OK);
    Ok(())
}

#[tokio::test]
async fn scoped_caller_is_refused_the_mux_feed() -> anyhow::Result<()> {
    let state = test_state().await?;
    let (status, _) = send(&state, "GET", "/sse/mux?conn=c1", &member_token(&["alpha"])?).await?;
    assert_eq!(status, StatusCode::FORBIDDEN);
    Ok(())
}

#[tokio::test]
async fn scoped_sse_attach_outside_land_is_not_found() -> anyhow::Result<()> {
    let state = test_state().await?;
    let alpha = member_token(&["alpha"])?;
    for id in ["b1", "n1"] {
        let uri = format!("/sse/session/{id}?conn=c1");
        assert_eq!(send(&state, "GET", &uri, &alpha).await?.0, StatusCode::NOT_FOUND, "{uri}");
    }
    Ok(())
}

#[tokio::test]
async fn ws_authorized_carries_the_land_scope() -> anyhow::Result<()> {
    let state = test_state().await?;
    let mut headers = axum::http::HeaderMap::new();
    headers.insert("authorization", format!("Bearer {}", member_token(&["alpha"])?).parse()?);
    let authenticated = ws_authorized(&state, &headers, None, false)
        .await
        .ok_or_else(|| anyhow::anyhow!("member token not authorized"))?;
    assert_eq!(authenticated.scope(), SessionScope::Projects(strings(&["alpha"])));

    let static_auth =
        ws_authorized(&state, &axum::http::HeaderMap::new(), Some(STATIC_TOKEN), false)
            .await
            .ok_or_else(|| anyhow::anyhow!("static token not authorized"))?;
    assert_eq!(static_auth.scope(), SessionScope::Unrestricted);
    Ok(())
}

#[tokio::test]
async fn forged_token_is_rejected() -> anyhow::Result<()> {
    let state = test_state().await?;
    let good = member_token(&["alpha"])?;
    // Swap in a payload claiming admin while keeping the original signature.
    let mut parts: Vec<String> = good.split('.').map(str::to_owned).collect();
    parts[1] = B64URL.encode(
        serde_json::json!({
            "iss": ISSUER,
            "aud": AUDIENCE,
            "exp": u32::MAX,
            "realm_roles": ["gardenio-admin"],
        })
        .to_string(),
    );
    let (status, _) = send(&state, "GET", "/api/v1/sessions", &parts.join(".")).await?;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    Ok(())
}
