// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Unit tests for the SSE fallback registry routing.

use std::sync::Arc;

use anyhow::anyhow;
use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::middleware;
use axum::routing::{get, post};
use axum::{Extension, Router};
use tokio::sync::broadcast;
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

use super::{RegistryLookupError, SseInboundMessage, SsePrincipal, SseRegistry};
use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{CredentialConfig, CredentialEvent};
use crate::state::MuxState;
use crate::transport::auth::auth_layer;

fn edge_identity(
    operator: &str,
    audit_id: &str,
    role: &str,
) -> crate::transport::edge_auth::TrustedOperator {
    crate::transport::edge_auth::TrustedOperator {
        operator: operator.to_owned(),
        audit_id: audit_id.to_owned(),
        version: 2,
        auth_mode: "entra".to_owned(),
        roles: vec![role.to_owned()],
        verified_v2: true,
        verified_v1: false,
        relay: false,
    }
}

fn edge_principal(identity: &crate::transport::edge_auth::TrustedOperator) -> SsePrincipal {
    SsePrincipal::from_authenticated(
        Some(identity),
        Some(&crate::transport::auth::WsAuthenticated::Edge),
    )
}

fn test_state_with_auth_token(auth_token: Option<&str>) -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let config = MuxConfig {
        host: "127.0.0.1".into(),
        port: 0,
        auth_token: auth_token.map(str::to_owned),
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
    };
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None);
    Arc::new(MuxState::new(config, CancellationToken::new(), broker))
}

fn test_state() -> Arc<MuxState> {
    test_state_with_auth_token(None)
}

fn sse_mux_app(
    state: Arc<MuxState>,
    identity: crate::transport::edge_auth::TrustedOperator,
) -> Router {
    Router::new()
        .route("/sse/mux/send", post(super::sse_mux_send_handler))
        .layer(middleware::from_fn_with_state(Arc::clone(&state), auth_layer))
        .layer(Extension(identity))
        .with_state(state)
}

fn sse_mux_token_app(state: Arc<MuxState>) -> Router {
    Router::new()
        .route("/sse/mux", get(super::sse_mux_handler))
        .route("/sse/mux/send", post(super::sse_mux_send_handler))
        .layer(middleware::from_fn_with_state(Arc::clone(&state), auth_layer))
        .with_state(state)
}

#[tokio::test]
async fn register_then_get_routes_to_same_channel() -> anyhow::Result<()> {
    let reg = SseRegistry::new();
    let (tx, mut rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    reg.register("conn-a".to_owned(), tx, SsePrincipal::Anonymous);

    let routed = reg
        .get("conn-a", &SsePrincipal::Anonymous)
        .map_err(|_| anyhow!("conn-a should be registered"))?;
    routed.send(SseInboundMessage { body: "hello".to_owned(), edge_identity: None }).await?;

    assert_eq!(rx.recv().await.map(|message| message.body).as_deref(), Some("hello"));
    Ok(())
}

#[tokio::test]
async fn get_unknown_conn_returns_none() {
    let reg = SseRegistry::new();
    assert!(matches!(
        reg.get("does-not-exist", &SsePrincipal::Anonymous),
        Err(RegistryLookupError::Unknown)
    ));
}

#[tokio::test]
async fn remove_deregisters_the_channel() {
    let reg = SseRegistry::new();
    let (tx, _rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    let epoch = reg.register("conn-b".to_owned(), tx, SsePrincipal::Anonymous);
    assert!(reg.get("conn-b", &SsePrincipal::Anonymous).is_ok());

    reg.remove_if_epoch("conn-b", epoch);
    assert!(matches!(
        reg.get("conn-b", &SsePrincipal::Anonymous),
        Err(RegistryLookupError::Unknown)
    ));
}

#[tokio::test]
async fn distinct_conns_route_independently() -> anyhow::Result<()> {
    let reg = Arc::new(SseRegistry::new());
    let (tx1, mut rx1) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    let (tx2, mut rx2) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    reg.register("c1".to_owned(), tx1, SsePrincipal::Anonymous);
    reg.register("c2".to_owned(), tx2, SsePrincipal::Anonymous);

    reg.get("c1", &SsePrincipal::Anonymous)
        .map_err(|_| anyhow!("c1 missing"))?
        .send(SseInboundMessage { body: "to-1".to_owned(), edge_identity: None })
        .await?;
    reg.get("c2", &SsePrincipal::Anonymous)
        .map_err(|_| anyhow!("c2 missing"))?
        .send(SseInboundMessage { body: "to-2".to_owned(), edge_identity: None })
        .await?;

    assert_eq!(rx1.recv().await.map(|message| message.body).as_deref(), Some("to-1"));
    assert_eq!(rx2.recv().await.map(|message| message.body).as_deref(), Some("to-2"));
    Ok(())
}

#[tokio::test]
async fn registry_rejects_a_different_edge_principal() {
    let reg = SseRegistry::new();
    let opener = edge_identity("operator-a@example.com", "audit-open", "gardenio-viewer");
    let other = edge_identity("operator-b@example.com", "audit-post", "gardenio-operator");
    let (tx, _rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    reg.register("bound".to_owned(), tx, edge_principal(&opener));

    assert!(matches!(
        reg.get("bound", &edge_principal(&other)),
        Err(RegistryLookupError::PrincipalMismatch)
    ));
}

#[tokio::test]
async fn registry_routes_the_post_identity_and_audit_id() -> anyhow::Result<()> {
    let reg = SseRegistry::new();
    let opener = edge_identity("operator-a@example.com", "audit-open", "gardenio-viewer");
    let caller = edge_identity("operator-a@example.com", "audit-post", "gardenio-operator");
    let (tx, mut rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    reg.register("bound".to_owned(), tx, edge_principal(&opener));

    reg.get("bound", &edge_principal(&caller))
        .map_err(|_| anyhow!("same principal should be accepted"))?
        .send(SseInboundMessage {
            body: r#"{"event":"subscribe","sessions":["s1"]}"#.to_owned(),
            edge_identity: Some(caller),
        })
        .await?;

    let routed = rx.recv().await.ok_or_else(|| anyhow!("message should be routed"))?;
    let identity =
        routed.edge_identity.ok_or_else(|| anyhow!("POST identity should be carried"))?;
    assert_eq!(identity.operator, "operator-a@example.com");
    assert_eq!(identity.audit_id, "audit-post");
    assert_eq!(identity.roles, vec!["gardenio-operator"]);
    Ok(())
}

#[tokio::test]
async fn mux_send_route_allows_viewer_subscribe_but_denies_viewer_input() -> anyhow::Result<()> {
    let state = test_state();
    let opener = edge_identity("viewer@example.com", "audit-open", "gardenio-viewer");
    let caller = edge_identity("viewer@example.com", "audit-post", "gardenio-viewer");
    let (tx, mut rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    state.sse.register("route-bound".to_owned(), tx, edge_principal(&opener));

    let subscribe = sse_mux_app(Arc::clone(&state), caller.clone())
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/mux/send?conn=route-bound")
                .body(Body::from(r#"{"event":"subscribe","sessions":["s1"]}"#))?,
        )
        .await?;
    assert_eq!(subscribe.status(), StatusCode::NO_CONTENT);
    let routed = rx.recv().await.ok_or_else(|| anyhow!("subscribe should be routed"))?;
    assert_eq!(
        routed.edge_identity.as_ref().map(|identity| identity.audit_id.as_str()),
        Some("audit-post")
    );

    let input = sse_mux_app(Arc::clone(&state), caller)
        .oneshot(Request::builder().method("POST").uri("/sse/mux/send?conn=route-bound").body(
            Body::from(r#"{"event":"input:send","session":"s1","text":"pwd","enter":true}"#),
        )?)
        .await?;
    assert_eq!(input.status(), StatusCode::FORBIDDEN);
    assert!(matches!(rx.try_recv(), Err(tokio::sync::mpsc::error::TryRecvError::Empty)));
    Ok(())
}

#[tokio::test]
async fn mux_send_route_rejects_connection_principal_mismatch() -> anyhow::Result<()> {
    let state = test_state();
    let opener = edge_identity("operator-a@example.com", "audit-open", "gardenio-viewer");
    let other = edge_identity("operator-b@example.com", "audit-post", "gardenio-operator");
    let (tx, mut rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    state.sse.register("route-bound".to_owned(), tx, edge_principal(&opener));

    let response = sse_mux_app(Arc::clone(&state), other)
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/mux/send?conn=route-bound")
                .body(Body::from(r#"{"event":"subscribe","sessions":["s1"]}"#))?,
        )
        .await?;
    assert_eq!(response.status(), StatusCode::FORBIDDEN);
    assert!(matches!(rx.try_recv(), Err(tokio::sync::mpsc::error::TryRecvError::Empty)));
    Ok(())
}

fn sse_session_send_app(
    state: Arc<MuxState>,
    identity: crate::transport::edge_auth::TrustedOperator,
) -> Router {
    Router::new()
        .route("/sse/session/{session_id}/send", post(super::sse_session_send_handler))
        .layer(middleware::from_fn_with_state(Arc::clone(&state), auth_layer))
        .layer(Extension(identity))
        .with_state(state)
}

// The per-session send twin of `/ws/{id}` forwards frames verbatim to the
// upstream coop /ws, which executes admin-reserved raw/signal/shutdown/restart/
// switch actions. `sse_session_send_handler` must decode each frame and apply
// the same tier boundary as the multiplexed feed — plain input stays OPERATOR;
// raw/signal/shutdown are held for ADMIN (ka-0000002552, lane-C findings 2+4).

#[tokio::test]
async fn session_send_operator_allows_input_but_denies_raw_and_signal() -> anyhow::Result<()> {
    let state = test_state();
    let opener = edge_identity("op@example.com", "audit-open", "gardenio-operator");
    let caller = edge_identity("op@example.com", "audit-post", "gardenio-operator");
    let (tx, mut rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    state.sse.register("sess-bound".to_owned(), tx, edge_principal(&opener));

    // Plain terminal input is an operator action → forwarded.
    let input = sse_session_send_app(Arc::clone(&state), caller.clone())
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/session/s1/send?conn=sess-bound")
                .body(Body::from(r#"{"event":"input:send","text":"pwd","enter":true}"#))?,
        )
        .await?;
    assert_eq!(input.status(), StatusCode::NO_CONTENT);
    let routed = rx.recv().await.ok_or_else(|| anyhow!("input should be routed"))?;
    assert_eq!(
        routed.edge_identity.as_ref().map(|identity| identity.audit_id.as_str()),
        Some("audit-post")
    );

    // Raw byte injection is admin-reserved → refused before it reaches the sink.
    let raw = sse_session_send_app(Arc::clone(&state), caller.clone())
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/session/s1/send?conn=sess-bound")
                .body(Body::from(r#"{"event":"input:send:raw","data":"cm0gLXJmIC8="}"#))?,
        )
        .await?;
    assert_eq!(raw.status(), StatusCode::FORBIDDEN);
    assert!(matches!(rx.try_recv(), Err(tokio::sync::mpsc::error::TryRecvError::Empty)));

    // SIGKILL is admin-reserved → refused.
    let signal = sse_session_send_app(Arc::clone(&state), caller)
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/session/s1/send?conn=sess-bound")
                .body(Body::from(r#"{"event":"signal:send","signal":"KILL"}"#))?,
        )
        .await?;
    assert_eq!(signal.status(), StatusCode::FORBIDDEN);
    assert!(matches!(rx.try_recv(), Err(tokio::sync::mpsc::error::TryRecvError::Empty)));
    Ok(())
}

#[tokio::test]
async fn session_send_admin_allows_raw_and_signal() -> anyhow::Result<()> {
    let state = test_state();
    let opener = edge_identity("admin@example.com", "audit-open", "gardenio-admin");
    let caller = edge_identity("admin@example.com", "audit-post", "gardenio-admin");
    let (tx, mut rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    state.sse.register("sess-bound".to_owned(), tx, edge_principal(&opener));

    let raw = sse_session_send_app(Arc::clone(&state), caller)
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/session/s1/send?conn=sess-bound")
                .body(Body::from(r#"{"event":"input:send:raw","data":"cHdkCg=="}"#))?,
        )
        .await?;
    assert_eq!(raw.status(), StatusCode::NO_CONTENT);
    rx.recv().await.ok_or_else(|| anyhow!("admin raw send should be routed"))?;
    Ok(())
}

#[tokio::test]
async fn session_send_viewer_denies_input() -> anyhow::Result<()> {
    let state = test_state();
    let opener = edge_identity("viewer@example.com", "audit-open", "gardenio-viewer");
    let caller = edge_identity("viewer@example.com", "audit-post", "gardenio-viewer");
    let (tx, mut rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    state.sse.register("sess-bound".to_owned(), tx, edge_principal(&opener));

    let input = sse_session_send_app(Arc::clone(&state), caller)
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/session/s1/send?conn=sess-bound")
                .body(Body::from(r#"{"event":"input:send","text":"pwd","enter":true}"#))?,
        )
        .await?;
    assert_eq!(input.status(), StatusCode::FORBIDDEN);
    assert!(matches!(rx.try_recv(), Err(tokio::sync::mpsc::error::TryRecvError::Empty)));
    Ok(())
}

#[tokio::test]
async fn session_send_unparseable_frame_fails_closed() -> anyhow::Result<()> {
    let state = test_state();
    let opener = edge_identity("op@example.com", "audit-open", "gardenio-operator");
    let caller = edge_identity("op@example.com", "audit-post", "gardenio-operator");
    let (tx, mut rx) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    state.sse.register("sess-bound".to_owned(), tx, edge_principal(&opener));

    // A body with no `event` tag classifies to ADMIN, so an operator is refused
    // rather than the raw bytes slipping through to the upstream.
    let garbage = sse_session_send_app(Arc::clone(&state), caller)
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/session/s1/send?conn=sess-bound")
                .body(Body::from(r#"not json"#))?,
        )
        .await?;
    assert_eq!(garbage.status(), StatusCode::FORBIDDEN);
    assert!(matches!(rx.try_recv(), Err(tokio::sync::mpsc::error::TryRecvError::Empty)));
    Ok(())
}

#[tokio::test]
async fn invalid_query_token_with_valid_bearer_binds_to_bearer() -> anyhow::Result<()> {
    let state = test_state_with_auth_token(Some("valid-bearer"));
    let app = sse_mux_token_app(state);

    // The stale query token is examined first but rejected. The stream must
    // bind to the bearer token that actually authenticated the GET.
    let stream_response = app
        .clone()
        .oneshot(
            Request::builder()
                .method("GET")
                .uri("/sse/mux?conn=bearer-fallback&token=stale-query-token")
                .header("authorization", "Bearer valid-bearer")
                .body(Body::empty())?,
        )
        .await?;
    assert_eq!(stream_response.status(), StatusCode::OK);

    // A paired POST using the same valid bearer and no stale query token must
    // match the stream principal.
    let send_response = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sse/mux/send?conn=bearer-fallback")
                .header("authorization", "Bearer valid-bearer")
                .body(Body::from(r#"{"event":"subscribe","sessions":["s1"]}"#))?,
        )
        .await?;
    assert_eq!(send_response.status(), StatusCode::NO_CONTENT);
    drop(stream_response);
    Ok(())
}

/// An `EventSource` reuses its `conn` id when it natively auto-reconnects after
/// a transient edge blip/idle-timeout. The reconnecting (live) stream must
/// REPLACE the dead one it supersedes so its POSTs route to the new sink —
/// rejecting the duplicate wedged the reconnect into a hard "Disconnected"
/// (ka-0000002989).
#[tokio::test]
async fn reconnecting_conn_id_replaces_the_stale_stream() -> anyhow::Result<()> {
    let reg = SseRegistry::new();
    let (tx1, mut rx1) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    let (tx2, mut rx2) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    let first = reg.register("reconnect".to_owned(), tx1, SsePrincipal::Anonymous);
    let second = reg.register("reconnect".to_owned(), tx2, SsePrincipal::Anonymous);
    assert_ne!(first, second, "each registration gets a distinct epoch");

    // POSTs now route to the reconnected (second) stream, not the dead first.
    reg.get("reconnect", &SsePrincipal::Anonymous)
        .map_err(|_| anyhow!("reconnected conn should be registered"))?
        .send(SseInboundMessage { body: "after-reconnect".to_owned(), edge_identity: None })
        .await?;
    assert_eq!(rx2.recv().await.map(|message| message.body).as_deref(), Some("after-reconnect"));
    // The stale first sink was dropped when the reconnect replaced it, so it
    // receives nothing (its channel is closed, never carrying the message).
    assert!(rx1.try_recv().is_err(), "stale first stream must receive nothing");
    Ok(())
}

/// The teardown of a superseded stream must NOT evict the successor that
/// replaced it under the same `conn` id: the late-firing [`ConnGuard`] carries
/// the old epoch and `remove_if_epoch` makes it a no-op.
#[tokio::test]
async fn stale_stream_teardown_does_not_evict_its_replacement() {
    let reg = SseRegistry::new();
    let (tx1, _rx1) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    let (tx2, _rx2) = tokio::sync::mpsc::channel::<SseInboundMessage>(4);
    let stale = reg.register("shared".to_owned(), tx1, SsePrincipal::Anonymous);
    reg.register("shared".to_owned(), tx2, SsePrincipal::Anonymous);

    // The old stream's guard fires late with its (now superseded) epoch.
    reg.remove_if_epoch("shared", stale);
    assert!(
        reg.get("shared", &SsePrincipal::Anonymous).is_ok(),
        "replacement stream must survive the stale guard's teardown"
    );
}

/// Regression: closing an *idle* per-session SSE stream (no PTY output, no
/// state change) must release the bridge client. The driver loop's only view
/// of a dropped `out_rx` used to be a failed `out_tx.send`, which never fires
/// while idle — so the task wedged forever on `client_rx`, leaking the client,
/// its upstream WS connection, and itself. `drive_session_stream` now watches
/// `out_tx.closed()`. Pre-fix this test hangs (times out) on the join; post-fix
/// the task exits and `client_count()` returns to 0.
#[tokio::test]
async fn idle_session_stream_releases_bridge_client_on_disconnect() -> anyhow::Result<()> {
    use crate::state::{SessionEntry, SessionTransport};
    use crate::upstream::bridge::{SubscriptionFlags, WsBridge};
    use std::sync::atomic::AtomicU32;
    use tokio::sync::{mpsc, RwLock};

    let entry = Arc::new(SessionEntry {
        id: "leak-test".to_owned(),
        // Unroutable upstream: the bridge's run_loop can never connect, so no
        // bridge message ever reaches the driver — exactly the idle case.
        url: "http://127.0.0.1:1".into(),
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
    });

    let bridge = WsBridge::connect(&entry);
    *entry.ws_bridge.write().await = Some(Arc::clone(&bridge));
    let (client_id, client_rx) = bridge.add_client(SubscriptionFlags::parse("pty,state")).await;
    assert_eq!(bridge.client_count().await, 1, "client registered");

    let (out_tx, out_rx) = mpsc::channel::<String>(4);
    let (_in_tx, in_rx) = mpsc::channel::<SseInboundMessage>(4);

    let driver = tokio::spawn(super::drive_session_stream(
        Arc::clone(&entry),
        Arc::clone(&bridge),
        client_id,
        client_rx,
        in_rx,
        out_tx,
    ));

    // Simulate the browser closing the idle stream: drop the SSE response's
    // receiver. No message was ever sent downstream, so only `out_tx.closed()`
    // can wake the driver.
    drop(out_rx);

    // Post-fix the driver exits promptly; pre-fix it wedges and this times out.
    tokio::time::timeout(std::time::Duration::from_secs(5), driver)
        .await
        .map_err(|_| anyhow!("driver task did not exit after client disconnect (leak)"))??;

    assert_eq!(bridge.client_count().await, 0, "bridge client released on disconnect");
    assert!(entry.ws_bridge.read().await.is_none(), "stale bridge cleared when last client left");
    Ok(())
}

/// Regression (ka-0000005810): a per-session bridge teardown must not null
/// `entry.ws_bridge` when another client has (re)registered against the *same*
/// live bridge. The old code observed `client_count()==0`, then — in a separate,
/// unlocked step — unconditionally wrote `None`; a client registering in that
/// window was orphaned and the next connect spawned a second upstream WS to the
/// same coop pod, breaking single-upstream multiplexing. The fix registers
/// under the `ws_bridge` lock (`get_or_create_bridge_and_register`) and tears
/// down under the same lock with a `ptr_eq` guard, so the sequence below can no
/// longer interleave destructively.
#[tokio::test]
async fn last_client_teardown_preserves_bridge_reused_by_concurrent_client() -> anyhow::Result<()> {
    use crate::state::{SessionEntry, SessionTransport};
    use crate::upstream::bridge::SubscriptionFlags;
    use std::sync::atomic::AtomicU32;
    use tokio::sync::RwLock;

    let entry = Arc::new(SessionEntry {
        id: "toctou-test".to_owned(),
        url: "http://127.0.0.1:1".into(),
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
    });

    let flags = || SubscriptionFlags::parse("pty,state");

    // Client A opens: creates the bridge and registers.
    let (bridge_a, client_a, _rx_a) =
        crate::transport::ws::get_or_create_bridge_and_register(&entry, flags()).await;

    // Client A closes: it is the only client, so the slot would clear —
    // *except* client C reuses the same stored bridge first.
    let (bridge_c, client_c, _rx_c) =
        crate::transport::ws::get_or_create_bridge_and_register(&entry, flags()).await;
    assert!(Arc::ptr_eq(&bridge_a, &bridge_c), "concurrent client must reuse the one live bridge");

    // Now A tears down. client_count() is 1 (C is present), so the slot must
    // survive. Pre-fix, an unconditional null here would orphan the live bridge.
    crate::transport::ws::release_client_and_maybe_clear(&entry, &bridge_a, client_a).await;

    {
        let stored = entry.ws_bridge.read().await;
        let stored = stored.as_ref().expect("bridge slot must survive: client C still connected");
        assert!(
            Arc::ptr_eq(stored, &bridge_c),
            "the surviving slot must still be C's live bridge, not a fresh one"
        );
    }
    assert_eq!(bridge_c.client_count().await, 1, "client C remains registered");

    // ptr_eq guard: a stale teardown of a *replaced* bridge must not clobber the
    // bridge currently stored. Drain C so bridge_c.client_count()==0, swap in a
    // fresh bridge, then run C's teardown — it must leave the fresh bridge alone.
    bridge_c.remove_client(client_c).await;
    let fresh = crate::upstream::bridge::WsBridge::connect(&entry);
    *entry.ws_bridge.write().await = Some(Arc::clone(&fresh));
    crate::transport::ws::release_client_and_maybe_clear(&entry, &bridge_c, client_c).await;
    {
        let stored = entry.ws_bridge.read().await;
        let stored = stored.as_ref().expect("fresh bridge must survive a stale teardown");
        assert!(Arc::ptr_eq(stored, &fresh), "ptr_eq guard must prevent nulling a replaced bridge");
    }
    Ok(())
}

#[test]
fn mux_message_roles_distinguish_subscriptions_from_controls() {
    let subscribe =
        crate::transport::ws_mux::MuxClientMessage::Subscribe { sessions: vec!["s1".to_owned()] };
    let unsubscribe =
        crate::transport::ws_mux::MuxClientMessage::Unsubscribe { sessions: vec!["s1".to_owned()] };
    let input = crate::transport::ws_mux::MuxClientMessage::InputSend {
        session: "s1".to_owned(),
        text: "pwd".to_owned(),
        enter: true,
    };
    let resize = crate::transport::ws_mux::MuxClientMessage::Resize {
        session: "s1".to_owned(),
        cols: 80,
        rows: 24,
    };

    assert_eq!(subscribe.required_edge_role(), "gardenio-viewer");
    assert_eq!(unsubscribe.required_edge_role(), "gardenio-viewer");
    assert_eq!(input.required_edge_role(), "gardenio-operator");
    assert_eq!(resize.required_edge_role(), "gardenio-operator");
}
