// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Cross-land matrix for coopmux registration (gd-zVTBvaSIdG): a land token
//! registers, sees and deregisters only its own land's sessions, and cannot
//! claim a fleet project, a fleet account, or a fleet pod's url.

use std::sync::atomic::AtomicU32;
use std::sync::Arc;
use std::time::Duration;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::routing::{get, post};
use axum::{Json, Router};
use tokio::sync::{broadcast, mpsc, RwLock};
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

use super::*;
use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::failover::AccountInfo;
use crate::credential::{AccountConfig, CredentialConfig, CredentialEvent};
use crate::state::{MuxState, SessionEntry, SessionTransport};

const STATIC_TOKEN: &str = "fleet-static-token";
const KEY: &[u8] = b"land-token-test-key";

fn key() -> anyhow::Result<LandTokenKey> {
    LandTokenKey::new(KEY).ok_or_else(|| anyhow::anyhow!("empty key"))
}

fn land_token(land: &str) -> anyhow::Result<String> {
    key()?.mint(land).ok_or_else(|| anyhow::anyhow!("not a land: {land}"))
}

fn principal(land: &str) -> LandPrincipal {
    LandPrincipal { land: land.to_owned() }
}

// ---- token ----

#[test]
fn token_round_trips_its_land() -> anyhow::Result<()> {
    let key = key()?;
    let token = land_token("alpha")?;
    assert!(token.starts_with("coopmux-land.v1.alpha."), "{token}");
    assert_eq!(key.verify(&token), Some(principal("alpha")));
    Ok(())
}

#[test]
fn token_cannot_be_moved_to_another_land() -> anyhow::Result<()> {
    let key = key()?;
    let alpha = land_token("alpha")?;
    let mac = alpha.rsplit('.').next().unwrap_or_default();
    for forged in [
        format!("coopmux-land.v1.beta.{mac}"),
        format!("coopmux-land.v1.gardenio.{mac}"),
        "coopmux-land.v1.alpha.".to_owned(),
        "coopmux-land.v1.alpha".to_owned(),
        format!("coopmux-land.v2.alpha.{mac}"),
        format!("{alpha}x"),
        STATIC_TOKEN.to_owned(),
    ] {
        assert_eq!(key.verify(&forged), None, "{forged}");
    }
    // Another key's token for the same land does not verify.
    let other = LandTokenKey::new(b"other-key").ok_or_else(|| anyhow::anyhow!("empty"))?;
    assert_eq!(key.verify(&other.mint("alpha").unwrap_or_default()), None);
    Ok(())
}

#[test]
fn only_land_names_get_tokens() -> anyhow::Result<()> {
    let key = key()?;
    for bad in ["", "Alpha", "a.b", "a/b", "a b", &"x".repeat(59)] {
        assert_eq!(key.mint(bad), None, "{bad:?}");
    }
    assert!(LandTokenKey::new(b"").is_none());
    Ok(())
}

// ---- body binding ----

#[test]
fn binding_stamps_the_land_and_keeps_other_metadata() -> anyhow::Result<()> {
    let p = principal("alpha");
    let bound = bind_registration(
        &p,
        Some("s1"),
        Some(serde_json::json!({"role": "crew", "project": "alpha"})),
        Some("lands-alpha"),
    )
    .map_err(|r| anyhow::anyhow!("{r:?}"))?;
    assert_eq!(bound, serde_json::json!({"role": "crew", "project": "alpha", "land": "alpha"}));
    let bound =
        bind_registration(&p, Some("s1"), None, None).map_err(|r| anyhow::anyhow!("{r:?}"))?;
    assert_eq!(bound, serde_json::json!({"project": "alpha", "land": "alpha"}));
    Ok(())
}

#[test]
fn binding_refuses_anything_outside_the_land() {
    let p = principal("alpha");
    let meta = |v: serde_json::Value| Some(v);
    let cases = [
        (None, None, None, Refusal::NoId),
        (Some(""), None, None, Refusal::NoId),
        (Some("s"), meta(serde_json::json!(["x"])), None, Refusal::Metadata),
        (
            Some("s"),
            meta(serde_json::json!({"project": "gardenio"})),
            None,
            Refusal::Project("\"gardenio\"".into()),
        ),
        (
            Some("s"),
            meta(serde_json::json!({"project": "beta"})),
            None,
            Refusal::Project("\"beta\"".into()),
        ),
        (
            Some("s"),
            meta(serde_json::json!({"project": ["alpha"]})),
            None,
            Refusal::Project("[\"alpha\"]".into()),
        ),
        (
            Some("s"),
            meta(serde_json::json!({"land": "beta"})),
            None,
            Refusal::Land("\"beta\"".into()),
        ),
        (Some("s"), None, Some("fleet-a"), Refusal::Account("fleet-a".into())),
        (Some("s"), None, Some("lands-beta"), Refusal::Account("lands-beta".into())),
        (Some("s"), None, Some("lands-"), Refusal::Account("lands-".into())),
    ];
    for (id, metadata, account, want) in cases {
        assert_eq!(
            bind_registration(&p, id, metadata.clone(), account),
            Err(want),
            "{id:?} {metadata:?} {account:?}"
        );
    }
}

#[test]
fn upstream_must_answer_as_the_session() {
    let p = principal("alpha");
    let health = |v: serde_json::Value| v;
    assert_eq!(
        check_upstream_identity(&p, "s1", &health(serde_json::json!({"session_id": "s1"}))),
        Ok(())
    );
    assert_eq!(
        check_upstream_identity(
            &p,
            "s1",
            &health(serde_json::json!({"session_id": "s1", "account_name": "lands-alpha"}))
        ),
        Ok(())
    );
    assert_eq!(
        check_upstream_identity(&p, "s1", &health(serde_json::json!({"session_id": "fleet-1"}))),
        Err(Refusal::UpstreamId(Some("fleet-1".into())))
    );
    assert_eq!(
        check_upstream_identity(&p, "s1", &health(serde_json::json!({"status": "running"}))),
        Err(Refusal::UpstreamId(None))
    );
    assert_eq!(
        check_upstream_identity(
            &p,
            "s1",
            &health(serde_json::json!({"session_id": "s1", "account_name": "fleet-a"}))
        ),
        Err(Refusal::UpstreamAccount("fleet-a".into()))
    );
}

// ---- cross-land matrix through the router ----

fn test_config() -> MuxConfig {
    MuxConfig {
        host: "127.0.0.1".into(),
        port: 0,
        auth_token: Some(STATIC_TOKEN.into()),
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

fn account(name: &str) -> AccountConfig {
    AccountConfig {
        name: name.to_owned(),
        provider: "claude".to_owned(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }
}

/// A mock coop that answers health as `session_id` (reporting `account`) and
/// forwards every profile push it receives.
struct Upstream {
    url: String,
    pushes: mpsc::UnboundedReceiver<serde_json::Value>,
}

async fn upstream(session_id: &str, account: Option<&str>) -> anyhow::Result<Upstream> {
    let health = serde_json::json!({
        "status": "running",
        "session_id": session_id,
        "account_name": account,
    });
    let (tx, pushes) = mpsc::unbounded_channel();
    let app = Router::new()
        .route("/api/v1/health", get(move || async move { Json(health) }))
        .route(
            "/api/v1/session/profiles",
            post(move |Json(body): Json<serde_json::Value>| async move {
                let _ = tx.send(body);
                Json(serde_json::json!({"ok": true}))
            }),
        )
        .fallback(|| async { Json(serde_json::json!({})) });
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(Upstream { url: format!("http://{addr}"), pushes })
}

fn entry(id: &str, url: &str, metadata: serde_json::Value) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url: url.to_owned(),
        auth_token: None,
        metadata,
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

/// coopmux holding fleet account `fleet-a` and land account `lands-alpha`,
/// with a fleet session `f1`, an alpha session `a1` and a beta session `b1`.
/// `f1`'s url is a live fleet coop.
struct Farm {
    state: Arc<MuxState>,
    fleet_url: String,
}

async fn farm() -> anyhow::Result<Farm> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let names = ["fleet-a", "lands-alpha"];
    let broker = CredentialBroker::new(
        CredentialConfig { accounts: names.iter().map(|n| account(n)).collect() },
        event_tx,
        None,
    );
    for n in names {
        broker.set_token(n, format!("tok-{n}"), None, None).await?;
    }
    let mut state = MuxState::new(test_config(), CancellationToken::new(), broker);
    state.land_token_key = Some(Arc::new(key()?));
    let state = Arc::new(state);
    {
        let mut cache = state.account_cache.write().await;
        for n in names {
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
    let fleet = upstream("f1", Some("fleet-a")).await?;
    {
        let mut sessions = state.sessions.write().await;
        sessions.insert(
            "f1".into(),
            entry("f1", &fleet.url, serde_json::json!({"project": "gardenio"})),
        );
        sessions.insert(
            "a1".into(),
            entry(
                "a1",
                "http://127.0.0.1:9/a1",
                serde_json::json!({"project": "alpha", "land": "alpha"}),
            ),
        );
        sessions.insert(
            "b1".into(),
            entry(
                "b1",
                "http://127.0.0.1:9/b1",
                serde_json::json!({"project": "beta", "land": "beta"}),
            ),
        );
    }
    Ok(Farm { state, fleet_url: fleet.url })
}

async fn call(
    state: &Arc<MuxState>,
    method: &str,
    uri: &str,
    bearer: &str,
    body: Option<serde_json::Value>,
) -> anyhow::Result<(StatusCode, String)> {
    let req = Request::builder()
        .method(method)
        .uri(uri)
        .header("authorization", format!("Bearer {bearer}"))
        .header("content-type", "application/json");
    let req = match body {
        Some(body) => req.body(Body::from(body.to_string()))?,
        None => req.body(Body::empty())?,
    };
    let resp = crate::transport::build_router(Arc::clone(state)).oneshot(req).await?;
    let status = resp.status();
    let bytes = axum::body::to_bytes(resp.into_body(), 1 << 20).await?;
    Ok((status, String::from_utf8_lossy(&bytes).into_owned()))
}

async fn register(
    state: &Arc<MuxState>,
    bearer: &str,
    body: serde_json::Value,
) -> anyhow::Result<(StatusCode, String)> {
    call(state, "POST", "/api/v1/sessions", bearer, Some(body)).await
}

async fn metadata_of(state: &MuxState, id: &str) -> Option<serde_json::Value> {
    state.sessions.read().await.get(id).map(|e| e.metadata.clone())
}

#[tokio::test]
async fn land_registration_matrix() -> anyhow::Result<()> {
    let farm = farm().await?;
    let alpha = land_token("alpha")?;

    // Each row: what the body claims, what the coop at its url answers as,
    // and whether coopmux takes it.
    struct Row {
        name: &'static str,
        id: Option<&'static str>,
        metadata: serde_json::Value,
        account: Option<&'static str>,
        upstream_id: &'static str,
        upstream_account: Option<&'static str>,
        ok: bool,
    }
    let row = |name, id: Option<&'static str>, metadata, account, upstream_account, ok| Row {
        name,
        id,
        metadata,
        account,
        upstream_id: id.unwrap_or("x"),
        upstream_account,
        ok,
    };
    let rows = [
        row(
            "own land, no project",
            Some("n1"),
            serde_json::json!({"role": "crew"}),
            None,
            None,
            true,
        ),
        row(
            "own land named",
            Some("n2"),
            serde_json::json!({"project": "alpha"}),
            None,
            None,
            true,
        ),
        row(
            "own account",
            Some("n3"),
            serde_json::Value::Null,
            Some("lands-alpha"),
            Some("lands-alpha"),
            true,
        ),
        row(
            "fleet project",
            Some("x1"),
            serde_json::json!({"project": "gardenio"}),
            None,
            None,
            false,
        ),
        row(
            "other land's project",
            Some("x2"),
            serde_json::json!({"project": "beta"}),
            None,
            None,
            false,
        ),
        row(
            "other land binding",
            Some("x3"),
            serde_json::json!({"land": "beta"}),
            None,
            None,
            false,
        ),
        row("fleet account", Some("x4"), serde_json::Value::Null, Some("fleet-a"), None, false),
        row(
            "other land's account",
            Some("x5"),
            serde_json::Value::Null,
            Some("lands-beta"),
            None,
            false,
        ),
        row(
            "coop reports fleet account",
            Some("x6"),
            serde_json::Value::Null,
            None,
            Some("fleet-a"),
            false,
        ),
        row("no id", None, serde_json::Value::Null, None, None, false),
        row("fleet session's id", Some("f1"), serde_json::Value::Null, None, None, false),
        row("other land's session id", Some("b1"), serde_json::Value::Null, None, None, false),
    ];
    for r in rows {
        let coop = upstream(r.upstream_id, r.upstream_account).await?;
        let mut body = serde_json::json!({"url": coop.url, "metadata": r.metadata});
        if let Some(id) = r.id {
            body["id"] = id.into();
        }
        if let Some(account) = r.account {
            body["account_name"] = account.into();
        }
        let (status, text) = register(&farm.state, &alpha, body).await?;
        if r.ok {
            assert_eq!(status, StatusCode::OK, "{}: {text}", r.name);
            let id = r.id.unwrap_or_default();
            let meta = metadata_of(&farm.state, id).await.unwrap_or_default();
            assert_eq!(meta["project"], "alpha", "{}: {meta}", r.name);
            assert_eq!(meta["land"], "alpha", "{}: {meta}", r.name);
        } else {
            assert_eq!(status, StatusCode::FORBIDDEN, "{}: {text}", r.name);
            assert!(text.contains("land_registration_refused"), "{}: {text}", r.name);
        }
    }
    // The fleet and beta sessions the refused rows aimed at are untouched.
    assert_eq!(
        metadata_of(&farm.state, "f1").await,
        Some(serde_json::json!({"project": "gardenio"}))
    );
    assert_eq!(
        metadata_of(&farm.state, "b1").await.map(|m| m["land"].clone()),
        Some("beta".into())
    );
    Ok(())
}

#[tokio::test]
async fn land_cannot_take_a_fleet_pods_url() -> anyhow::Result<()> {
    let farm = farm().await?;
    let alpha = land_token("alpha")?;
    // The fleet coop answers as f1, so a new id at its url is refused...
    let (status, _) =
        register(&farm.state, &alpha, serde_json::json!({"url": farm.fleet_url, "id": "steal"}))
            .await?;
    assert_eq!(status, StatusCode::FORBIDDEN);
    // ...and even a coop that answers to the new id may not evict f1 by
    // sharing its url.
    let liar = upstream("steal", None).await?;
    farm.state
        .sessions
        .write()
        .await
        .insert("f2".into(), entry("f2", &liar.url, serde_json::json!({"project": "gardenio"})));
    let (status, _) =
        register(&farm.state, &alpha, serde_json::json!({"url": liar.url, "id": "steal"})).await?;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert!(metadata_of(&farm.state, "f1").await.is_some());
    assert!(metadata_of(&farm.state, "f2").await.is_some());
    assert!(metadata_of(&farm.state, "steal").await.is_none());
    Ok(())
}

#[tokio::test]
async fn forged_land_token_is_unauthorized() -> anyhow::Result<()> {
    let farm = farm().await?;
    let alpha = land_token("alpha")?;
    let mac = alpha.rsplit('.').next().unwrap_or_default();
    let coop = upstream("n1", None).await?;
    let (status, _) = register(
        &farm.state,
        &format!("coopmux-land.v1.gardenio.{mac}"),
        serde_json::json!({"url": coop.url, "id": "n1", "metadata": {"project": "gardenio"}}),
    )
    .await?;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    Ok(())
}

#[tokio::test]
async fn land_token_routes_stay_in_land() -> anyhow::Result<()> {
    let farm = farm().await?;
    let alpha = land_token("alpha")?;

    let (status, body) = call(&farm.state, "GET", "/api/v1/sessions", &alpha, None).await?;
    assert_eq!(status, StatusCode::OK);
    let list: Vec<serde_json::Value> = serde_json::from_str(&body)?;
    let ids: Vec<&str> = list.iter().filter_map(|s| s["id"].as_str()).collect();
    assert_eq!(ids, vec!["a1"], "{body}");

    for (method, uri, want) in [
        ("GET", "/api/v1/sessions/f1/screen", StatusCode::NOT_FOUND),
        ("GET", "/api/v1/sessions/b1/screen", StatusCode::NOT_FOUND),
        ("POST", "/api/v1/sessions/f1/input", StatusCode::NOT_FOUND),
        ("DELETE", "/api/v1/sessions/f1", StatusCode::NOT_FOUND),
        ("DELETE", "/api/v1/sessions/b1", StatusCode::NOT_FOUND),
        ("POST", "/api/v1/sessions/a1/input/raw", StatusCode::FORBIDDEN),
        ("POST", "/api/v1/sessions/launch", StatusCode::FORBIDDEN),
        ("GET", "/api/v1/credentials/status", StatusCode::FORBIDDEN),
        ("POST", "/api/v1/credentials/distribute", StatusCode::FORBIDDEN),
        ("GET", "/api/v1/credentials/pool", StatusCode::FORBIDDEN),
    ] {
        let (status, body) = call(&farm.state, method, uri, &alpha, None).await?;
        assert_eq!(status, want, "{method} {uri}: {body}");
    }
    assert!(metadata_of(&farm.state, "f1").await.is_some());
    assert!(metadata_of(&farm.state, "b1").await.is_some());

    let (status, _) = call(&farm.state, "DELETE", "/api/v1/sessions/a1", &alpha, None).await?;
    assert_eq!(status, StatusCode::OK);
    assert!(metadata_of(&farm.state, "a1").await.is_none());
    Ok(())
}

#[tokio::test]
async fn static_token_registration_is_unchanged() -> anyhow::Result<()> {
    let farm = farm().await?;
    let coop = upstream("anything", None).await?;
    let (status, body) = register(
        &farm.state,
        STATIC_TOKEN,
        serde_json::json!({"url": coop.url, "id": "fleet-2", "metadata": {"project": "gardenio"}}),
    )
    .await?;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(
        metadata_of(&farm.state, "fleet-2").await,
        Some(serde_json::json!({"project": "gardenio"}))
    );
    Ok(())
}

#[tokio::test]
async fn land_session_is_staged_only_its_own_account() -> anyhow::Result<()> {
    let farm = farm().await?;
    let mut coop = upstream("n1", Some("lands-alpha")).await?;
    let (status, body) = register(
        &farm.state,
        &land_token("alpha")?,
        serde_json::json!({"url": coop.url, "id": "n1", "account_name": "lands-alpha"}),
    )
    .await?;
    assert_eq!(status, StatusCode::OK, "{body}");
    let push = tokio::time::timeout(Duration::from_secs(10), coop.pushes.recv())
        .await?
        .ok_or_else(|| anyhow::anyhow!("no profile push"))?;
    let staged: Vec<&str> = push["profiles"]
        .as_array()
        .map(|a| a.iter().filter_map(|p| p["name"].as_str()).collect())
        .unwrap_or_default();
    assert_eq!(staged, vec!["lands-alpha"], "{push}");
    Ok(())
}
