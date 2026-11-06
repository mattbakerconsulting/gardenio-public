// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Downstream WebSocket handler for mux clients.

use std::sync::Arc;

use axum::extract::ws::{Message, WebSocket};
use axum::extract::{Path, Query, State, WebSocketUpgrade};
use axum::response::IntoResponse;
use axum::Extension;
use futures_util::{SinkExt, StreamExt};
use serde::Deserialize;

use crate::state::{MuxState, SessionEntry};
use crate::transport::auth;
use crate::transport::project_scope::SessionScope;
use crate::upstream::bridge::{SubscriptionFlags, WsBridge};

/// Query parameters for downstream WS upgrade.
#[derive(Debug, Clone, Deserialize)]
pub struct MuxWsQuery {
    pub token: Option<String>,
    /// Comma-separated upstream subscription flags (e.g. `pty,screen,state`).
    #[serde(default = "default_subscribe")]
    pub subscribe: String,
}

fn default_subscribe() -> String {
    "pty,screen,state".to_owned()
}

/// `GET /ws/{session_id}` — WebSocket upgrade for a mux session.
pub async fn ws_handler(
    State(state): State<Arc<MuxState>>,
    Path(session_id): Path<String>,
    Query(query): Query<MuxWsQuery>,
    edge_trusted: Option<Extension<crate::transport::edge_auth::TrustedOperator>>,
    headers: axum::http::HeaderMap,
    ws: WebSocketUpgrade,
) -> impl IntoResponse {
    let edge_identity = edge_trusted.map(|Extension(identity)| identity);
    // Validate auth: edge-auth envelope, then token (query/header) or OIDC.
    // See auth::ws_authorized for why the envelope must be honoured here —
    // browsers reaching coopmux through the unified-host shim carry no token.
    let no_auth = state.config.auth_token.is_none() && state.oidc_verifier.is_none();
    let scope = if no_auth {
        SessionScope::Unrestricted
    } else {
        match auth::ws_authorized(&state, &headers, query.token.as_deref(), edge_identity.is_some())
            .await
        {
            Some(authenticated) => authenticated.scope(),
            None => {
                return axum::http::Response::builder()
                    .status(401)
                    .body(axum::body::Body::from("unauthorized"))
                    .unwrap_or_default()
                    .into_response();
            }
        }
    };

    // Look up session. A land-scoped OIDC caller (gd-DcKLFjZuyl) gets the
    // same 404 for another land's session as for an unknown one.
    let sessions = state.sessions.read().await;
    let entry = match sessions.get(&session_id) {
        Some(e) if scope.allows_metadata(&e.metadata) => Arc::clone(e),
        _ => {
            return axum::http::Response::builder()
                .status(404)
                .body(axum::body::Body::from("session not found"))
                .unwrap_or_default()
                .into_response();
        }
    };
    drop(sessions);

    let subscribe = query.subscribe.clone();

    ws.on_upgrade(move |socket| {
        handle_ws(socket, entry, subscribe, session_id, edge_identity, scope)
    })
    .into_response()
}

/// Per-connection WebSocket handler.
async fn handle_ws(
    socket: WebSocket,
    entry: Arc<SessionEntry>,
    subscribe: String,
    session_id: String,
    edge_identity: Option<crate::transport::edge_auth::TrustedOperator>,
    scope: SessionScope,
) {
    let flags = SubscriptionFlags::parse(&subscribe);
    let (bridge, client_id, mut client_rx) = get_or_create_bridge_and_register(&entry, flags).await;
    let (mut ws_tx, mut ws_rx) = socket.split();

    loop {
        tokio::select! {
            _ = entry.cancel.cancelled() => break,

            // Bridge -> downstream client
            msg = client_rx.recv() => {
                match msg {
                    Some(text) => {
                        if ws_tx.send(Message::Text(text.to_string().into())).await.is_err() {
                            break;
                        }
                    }
                    None => break,
                }
            }

            // Downstream client -> bridge (upstream)
            msg = ws_rx.next() => {
                match msg {
                    Some(Ok(Message::Text(text))) => {
                        // The upgrade gate only proves OPERATOR, but the upstream
                        // coop /ws executes admin-reserved actions (raw byte
                        // injection, signals incl. SIGKILL, shutdown/restart/
                        // switch). Decode the frame tag and enforce its tier
                        // before forwarding — mirroring the multiplexed feed's
                        // per-message check. An edge-authed caller below the
                        // required role is refused; a land-scoped OIDC caller
                        // is capped at operator below; static-token and
                        // unscoped OIDC callers retain full access as before.
                        if let Some(identity) = edge_identity.as_ref() {
                            let required = auth::edge_session_frame_required_role(&text);
                            if !identity.may_send(required) {
                                tracing::warn!(
                                    operator = %identity.operator,
                                    audit_id = %identity.audit_id,
                                    auth_mode = %identity.auth_mode,
                                    roles = ?identity.roles,
                                    action = "ws_raw_send",
                                    target_session = %session_id,
                                    required_role = %required,
                                    outcome = "denied",
                                    "edge-auth mux action",
                                );
                                let err = serde_json::json!({
                                    "event": "error",
                                    "code": "forbidden",
                                    "message": format!(
                                        "insufficient permissions: requires {required}"
                                    ),
                                });
                                if ws_tx
                                    .send(Message::Text(err.to_string().into()))
                                    .await
                                    .is_err()
                                {
                                    break;
                                }
                                continue;
                            }
                            tracing::info!(
                                operator = %identity.operator,
                                audit_id = %identity.audit_id,
                                auth_mode = %identity.auth_mode,
                                roles = ?identity.roles,
                                action = "ws_raw_send",
                                target_session = %session_id,
                                required_role = %required,
                                outcome = "forwarded",
                                "edge-auth mux action",
                            );
                        }
                        // A land-scoped OIDC caller is capped at the operator
                        // tier: it may type into its own land's terminal, never
                        // send admin-reserved raw/signal/lifecycle frames.
                        let required = auth::edge_session_frame_required_role(&text);
                        if !scope.allows_role(required) {
                            tracing::warn!(
                                action = "ws_raw_send",
                                target_session = %session_id,
                                required_role = %required,
                                outcome = "denied",
                                "oidc land scope: frame denied",
                            );
                            let err = serde_json::json!({
                                "event": "error",
                                "code": "forbidden",
                                "message": format!("insufficient permissions: requires {required}"),
                            });
                            if ws_tx.send(Message::Text(err.to_string().into())).await.is_err() {
                                break;
                            }
                            continue;
                        }
                        bridge.send_upstream(client_id, text.to_string());
                    }
                    Some(Ok(Message::Close(_))) | None => break,
                    Some(Err(_)) => break,
                    _ => {}
                }
            }
        }
    }

    // Complete the WebSocket close handshake so the browser doesn't
    // report "The network connection was lost."
    let _ = ws_tx.send(Message::Close(None)).await;

    release_client_and_maybe_clear(&entry, &bridge, client_id).await;
}

/// Get the existing WS bridge (or create one) and register a downstream client
/// against it *atomically* under the `ws_bridge` write lock.
///
/// Folding `add_client` into the locked section serializes registration against
/// [`release_client_and_maybe_clear`], which holds the same lock across its
/// remove-count-null critical section. That closes a check-then-act TOCTOU
/// (ka-0000005810): without it, a client could register on a bridge between a
/// departing client observing `client_count()==0` and its `*ws_bridge = None`,
/// orphaning a live bridge and forcing the next connect to spawn a *second*
/// upstream WS to the same coop pod (single-upstream multiplexing broken).
pub(crate) async fn get_or_create_bridge_and_register(
    entry: &Arc<SessionEntry>,
    flags: SubscriptionFlags,
) -> (Arc<WsBridge>, crate::upstream::bridge::ClientId, tokio::sync::mpsc::Receiver<Arc<str>>) {
    let mut guard = entry.ws_bridge.write().await;
    let bridge = match *guard {
        Some(ref bridge) => Arc::clone(bridge),
        None => {
            let bridge = WsBridge::connect(entry);
            *guard = Some(Arc::clone(&bridge));
            bridge
        }
    };
    let (client_id, client_rx) = bridge.add_client(flags).await;
    (bridge, client_id, client_rx)
}

/// Remove a downstream client and, if it was the last one, clear the shared
/// bridge slot so the next connect creates a fresh upstream connection.
///
/// The whole remove-count-null sequence runs under the `ws_bridge` write lock
/// and only nulls when the stored bridge is still *this* bridge
/// (`Arc::ptr_eq`). Both guards are required: the lock excludes a concurrent
/// [`get_or_create_bridge_and_register`] from registering a client mid-teardown,
/// and the `ptr_eq` check avoids clobbering a bridge that was already replaced.
pub(crate) async fn release_client_and_maybe_clear(
    entry: &Arc<SessionEntry>,
    bridge: &Arc<WsBridge>,
    client_id: crate::upstream::bridge::ClientId,
) {
    let mut guard = entry.ws_bridge.write().await;
    bridge.remove_client(client_id).await;
    if bridge.client_count().await == 0
        && guard.as_ref().is_some_and(|stored| Arc::ptr_eq(stored, bridge))
    {
        *guard = None;
    }
}
