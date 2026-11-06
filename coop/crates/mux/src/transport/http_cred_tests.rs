// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// Tests for the credential-management HTTP handlers. These focus on the
// operator-triggered `credentials_pool_rebalance` endpoint, which pushes a
// profile-register + profile-switch POST to every session serially. The
// regression under test (ka-0000000721): those POSTs must be bounded by a
// per-request timeout, so one hung/parked session pod cannot hang the operator
// endpoint forever nor permanently skip every session after it.

use std::sync::atomic::AtomicU32;
use std::sync::Arc;
use std::time::Duration;

use axum::body::to_bytes;
use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
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

/// Build a state whose broker holds the given healthy accounts (each seeded
/// with a token so it is `Healthy` and `get_credentials` returns `Some`).
async fn state_with_accounts(names: &[&str]) -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let configs: Vec<AccountConfig> = names.iter().map(|n| account(n)).collect();
    let broker = CredentialBroker::new(CredentialConfig { accounts: configs }, event_tx, None);
    for n in names {
        broker.set_token(n, format!("tok-{n}"), None, None).await.expect("seed token");
    }
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), broker))
}

fn http_entry(id: &str, url: String, assigned: Option<&str>) -> Arc<SessionEntry> {
    http_entry_with(id, url, assigned, serde_json::Value::Null)
}

fn http_entry_with(
    id: &str,
    url: String,
    assigned: Option<&str>,
    metadata: serde_json::Value,
) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url,
        auth_token: None,
        metadata,
        registered_at: std::time::Instant::now(),
        cached_screen: RwLock::new(None),
        cached_status: RwLock::new(None),
        cached_usage: RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: RwLock::new(None),
        assigned_account: RwLock::new(assigned.map(str::to_owned)),
        claimed_bead_title: RwLock::new(None),
        transport: SessionTransport::Http,
    })
}

/// Mock upstream coop that ACCEPTS the TCP connection but never sends an HTTP
/// response — a parked/hung session pod. A profile-register / profile-switch
/// POST here blocks until its per-request timeout fires; without the timeout it
/// would hang forever, wedging the operator rebalance endpoint. The spawned
/// task holds each accepted connection open until the test ends.
async fn mock_hung_upstream() -> anyhow::Result<String> {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let mut held = Vec::new();
        loop {
            match listener.accept().await {
                Ok((stream, _)) => held.push(stream),
                Err(_) => break,
            }
        }
    });
    Ok(format!("http://{addr}"))
}

async fn body_json(resp: axum::response::Response) -> anyhow::Result<serde_json::Value> {
    let bytes = to_bytes(resp.into_body(), 64 * 1024).await?;
    Ok(serde_json::from_slice(&bytes)?)
}

/// The operator-triggered `credentials_pool_rebalance` must RETURN even when a
/// target session pod is hung (TCP accepted, never responds). The two POSTs
/// (`/api/v1/session/profiles` then `/api/v1/session/switch`) each resolve when
/// their per-request timeout (`REBALANCE_REQUEST_TIMEOUT`) fires; before the fix
/// they used a timeout-less `UpstreamClient::new` on the no-default-timeout
/// shared client, so this would never return.
///
/// Uses a paused clock so the real 10s timeout elapses in virtual time and the
/// test finishes near-instantly; the outer `tokio::time::timeout` guard proves
/// boundedness (a pre-fix infinite hang would exhaust it and fail).
#[tokio::test(start_paused = true)]
async fn pool_rebalance_is_bounded_when_a_session_pod_hangs() -> anyhow::Result<()> {
    let hung_url = mock_hung_upstream().await?;

    // Broker holds only "acct-b"; the session is currently on "acct-a", so the
    // rebalance selects a different account and proceeds to push to the pod.
    let state = state_with_accounts(&["acct-b"]).await;
    let entry = http_entry("s1", hung_url, Some("acct-a"));
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&entry));

    // With the per-request timeout the two POSTs resolve in ~20s of virtual
    // time; the 120s guard proves the endpoint is bounded.
    let resp = tokio::time::timeout(
        Duration::from_secs(120),
        credentials_pool_rebalance(
            State(Arc::clone(&state)),
            Json(RebalanceRequest { from_account: None }),
        ),
    )
    .await
    .map_err(|_| anyhow::anyhow!("rebalance hung on an unresponsive pod"))?
    .into_response();
    assert_eq!(resp.status(), 200);

    // The pod was unreachable, so the profile-register POST failed and the
    // session is counted as failed rather than rebalanced — but the endpoint
    // returned instead of hanging, which is the fix.
    let body = body_json(resp).await?;
    assert_eq!(body.get("rebalanced").and_then(|v| v.as_u64()), Some(0));
    assert_eq!(body.get("failed").and_then(|v| v.as_u64()), Some(1));
    Ok(())
}

/// A hung pod must not starve the sessions AFTER it: with two sessions, the
/// first pointing at a hung pod and the second at a responsive pod, the second
/// still gets its rebalance attempt (the loop advances past the timed-out first
/// instead of blocking on it forever).
#[tokio::test(start_paused = true)]
async fn pool_rebalance_reaches_sessions_after_a_hung_one() -> anyhow::Result<()> {
    use axum::routing::post;
    use axum::Router;

    let hung_url = mock_hung_upstream().await?;

    // A responsive upstream that 200s both the profile-register and switch POSTs.
    let switches = Arc::new(AtomicU32::new(0));
    let switches_route = Arc::clone(&switches);
    let app = Router::new()
        .route("/api/v1/session/profiles", post(|| async { Json(serde_json::json!({"ok": true})) }))
        .route(
            "/api/v1/session/switch",
            post(move || {
                let switches = Arc::clone(&switches_route);
                async move {
                    switches.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                    Json(serde_json::json!({"ok": true}))
                }
            }),
        );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let live_addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    let live_url = format!("http://{live_addr}");

    let state = state_with_accounts(&["acct-b"]).await;
    // s1 is hung, s2 is responsive; both currently on "acct-a".
    state
        .sessions
        .write()
        .await
        .insert("s1".to_owned(), http_entry("s1", hung_url, Some("acct-a")));
    state
        .sessions
        .write()
        .await
        .insert("s2".to_owned(), http_entry("s2", live_url, Some("acct-a")));

    let resp = tokio::time::timeout(
        Duration::from_secs(120),
        credentials_pool_rebalance(
            State(Arc::clone(&state)),
            Json(RebalanceRequest { from_account: None }),
        ),
    )
    .await
    .map_err(|_| anyhow::anyhow!("rebalance hung on an unresponsive pod"))?
    .into_response();
    assert_eq!(resp.status(), 200);

    // The responsive session was reached and switched despite the hung one
    // being walked first (HashMap order is arbitrary, but either way s2 is
    // reached because the hung POST is bounded).
    assert_eq!(
        switches.load(std::sync::atomic::Ordering::SeqCst),
        1,
        "the responsive session must be rebalanced even when another pod hangs"
    );
    Ok(())
}

/// Land wall (gd-bD6BNm16oA): the operator pool rebalance never moves a land
/// session (on its purse account, or known by its project) and never targets a
/// land's purse account — with or without an account cache.
#[tokio::test]
async fn pool_rebalance_never_crosses_the_land_wall() -> anyhow::Result<()> {
    use axum::routing::post;
    use axum::Router;

    for with_cache in [false, true] {
        // A responsive upstream that records every switch target.
        let targets = Arc::new(tokio::sync::Mutex::new(Vec::<String>::new()));
        let sink = Arc::clone(&targets);
        let app = Router::new()
            .route(
                "/api/v1/session/profiles",
                post(|| async { Json(serde_json::json!({"ok": true})) }),
            )
            .route(
                "/api/v1/session/switch",
                post(move |Json(body): Json<serde_json::Value>| {
                    let sink = Arc::clone(&sink);
                    async move {
                        let profile = body["profile"].as_str().unwrap_or_default().to_owned();
                        sink.lock().await.push(profile);
                        Json(serde_json::json!({"ok": true}))
                    }
                }),
            );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
        let url = format!("http://{}", listener.local_addr()?);
        tokio::spawn(async move {
            let _ = axum::serve(listener, app).await;
        });

        let state = state_with_accounts(&["fleet-a", "fleet-b", "lands-x"]).await;
        if with_cache {
            let mut cache = state.account_cache.write().await;
            for name in ["fleet-a", "fleet-b", "lands-x"] {
                cache.accounts_mut().insert(
                    name.to_owned(),
                    crate::credential::failover::AccountInfo {
                        name: name.to_owned(),
                        status: "open".to_owned(),
                        ..Default::default()
                    },
                );
            }
        }
        // fleet-a is throttled so the fleet session's only target is fleet-b.
        state.credential_broker.mark_throttled("fleet-a").await;
        let land = serde_json::json!({ "project": "x" });
        {
            let mut sessions = state.sessions.write().await;
            sessions.insert("f".to_owned(), http_entry("f", url.clone(), Some("fleet-a")));
            sessions.insert(
                "x".to_owned(),
                http_entry_with("x", url.clone(), Some("lands-x"), land.clone()),
            );
            sessions
                .insert("xp".to_owned(), http_entry_with("xp", url.clone(), Some("fleet-a"), land));
        }

        let resp = credentials_pool_rebalance(
            State(Arc::clone(&state)),
            Json(RebalanceRequest { from_account: None }),
        )
        .await
        .into_response();
        let body = body_json(resp).await?;

        assert_eq!(*targets.lock().await, vec!["fleet-b".to_owned()], "cache={with_cache}");
        assert_eq!(body.get("rebalanced").and_then(|v| v.as_u64()), Some(1));
        assert_eq!(body.get("skipped_land").and_then(|v| v.as_u64()), Some(2));
        let sessions = state.sessions.read().await;
        for (id, want) in [("f", "fleet-b"), ("x", "lands-x"), ("xp", "fleet-a")] {
            let got = match sessions.get(id) {
                Some(e) => e.assigned_account.read().await.clone(),
                None => None,
            };
            assert_eq!(got.as_deref(), Some(want), "session {id}, cache={with_cache}");
        }
    }
    Ok(())
}
