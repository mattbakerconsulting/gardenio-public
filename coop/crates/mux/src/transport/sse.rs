// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Server-Sent Events (SSE) fallback transport.
//!
//! AWS Verified Access (and any HTTP/2-terminating edge without RFC 8441
//! extended-CONNECT) drops classic WebSocket `Upgrade` requests, so the mux
//! dashboard behind such an edge can never open `/ws/mux` and shows
//! "Disconnected". SSE rides a plain `GET` with `text/event-stream`, which
//! those edges carry fine, so it is the browser-friendly fallback.
//!
//! Because SSE is unidirectional (server → client only), each logical
//! connection is a *pair*:
//!
//! * `GET  /sse/mux?conn=<id>`          — downstream event stream
//! * `POST /sse/mux/send?conn=<id>`     — upstream control messages
//!
//! and likewise for a single session:
//!
//! * `GET  /sse/session/{id}?conn=<id>` — downstream PTY/state stream
//! * `POST /sse/session/{id}/send?conn=<id>` — upstream input/resize/rpc
//!
//! The client generates a random `conn` id, opens the `GET` stream, then sends
//! control messages via `POST` carrying the same `conn`. The [`SseRegistry`]
//! routes each `POST` body to the matching open stream's inbound channel.

use std::collections::HashMap;
use std::convert::Infallible;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use axum::extract::{Path, Query, State};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::IntoResponse;
use axum::Extension;
use futures_util::StreamExt;
use serde::Deserialize;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

use crate::state::{MuxState, SessionEntry};
use crate::transport::auth;
use crate::transport::edge_auth::TrustedOperator;
use crate::transport::project_scope::SessionScope;
use crate::transport::ws_mux::{run_mux_feed, MuxClientMessage, MuxFeedMessage};
use crate::upstream::bridge::{SubscriptionFlags, WsBridge};

/// Capacity of the per-connection inbound (client → server) channel.
const INBOUND_CAPACITY: usize = 64;
/// Capacity of the per-connection outbound (server → client) channel.
const OUTBOUND_CAPACITY: usize = 256;

/// Routes `POST /sse/*/send` bodies to the inbound channel of the matching
/// open `GET` stream, keyed by the client-generated `conn` id.
#[derive(Default)]
pub struct SseRegistry {
    conns: Mutex<HashMap<String, SseConnection>>,
    /// Monotonic counter stamping each registration with a unique epoch, so a
    /// superseded stream's teardown can be distinguished from the live one that
    /// replaced it under the same `conn` id (see [`SseRegistry::register`]).
    next_epoch: AtomicU64,
}

struct SseConnection {
    tx: mpsc::Sender<SseInboundMessage>,
    principal: SsePrincipal,
    /// Unique per-registration marker. [`ConnGuard`] carries the epoch of the
    /// registration it owns and deregisters only if it still holds the slot,
    /// so a late-firing guard from a replaced stream cannot evict its successor.
    epoch: u64,
}

#[derive(Debug)]
struct SseInboundMessage {
    body: String,
    edge_identity: Option<TrustedOperator>,
}

/// Stable principal fields from the stream-opening request. Audit IDs and
/// roles deliberately are not part of the binding: audit IDs are per request,
/// and a user's current roles must be evaluated on every POST.
#[derive(Debug, Clone, PartialEq, Eq)]
enum SsePrincipal {
    Edge {
        operator: String,
        auth_mode: String,
        verified_v2: bool,
        // Part of the binding so a full-authority verified-v1 stream and a
        // viewer-only burn-in stream (both verified_v2=false) are distinct
        // principals — a lower-trust POST can never match a higher-trust
        // stream (ka-0000006597, additive enforcement).
        verified_v1: bool,
    },
    /// Non-edge callers do not expose verified claims to the SSE handler, so
    /// bind them to a SHA-256 fingerprint of the already-validated bearer or
    /// query token without retaining credential material in memory.
    Token(Vec<u8>),
    /// Development mode with no configured authentication.
    Anonymous,
}

impl SsePrincipal {
    fn from_authenticated(
        identity: Option<&TrustedOperator>,
        authenticated: Option<&auth::WsAuthenticated>,
    ) -> Self {
        if let Some(identity) = identity {
            return Self::Edge {
                operator: identity.operator.clone(),
                auth_mode: identity.auth_mode.clone(),
                verified_v2: identity.verified_v2,
                verified_v1: identity.verified_v1,
            };
        }
        match authenticated {
            Some(
                auth::WsAuthenticated::Token(fingerprint)
                | auth::WsAuthenticated::Oidc(fingerprint, _),
            ) => Self::Token(fingerprint.clone()),
            None => Self::Anonymous,
            // An edge-auth result always arrives with a TrustedOperator
            // extension; fail closed to an unmatchable anonymous principal if
            // an internal caller violates that invariant.
            Some(auth::WsAuthenticated::Edge) => Self::Anonymous,
        }
    }
}

impl SseRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Register an open stream's inbound sink under `conn_id`, returning the
    /// unique epoch stamped on this registration.
    ///
    /// A colliding `conn_id` REPLACES the existing entry rather than being
    /// rejected: the browser's `EventSource` reuses the same `conn` id when it
    /// natively auto-reconnects after a transient edge blip or idle-timeout, so
    /// the reconnecting (live) stream must win over the dead one it supersedes.
    /// Rejecting the duplicate instead wedged the reconnect into a hard error
    /// (a 409 is not `text/event-stream`, so `EventSource` gives up), surfacing
    /// as the "Disconnected" flicker this transport exists to avoid.
    fn register(
        &self,
        conn_id: String,
        tx: mpsc::Sender<SseInboundMessage>,
        principal: SsePrincipal,
    ) -> u64 {
        let epoch = self.next_epoch.fetch_add(1, Ordering::Relaxed);
        if let Ok(mut conns) = self.conns.lock() {
            conns.insert(conn_id, SseConnection { tx, principal, epoch });
        }
        epoch
    }

    /// Remove a connection only if it still holds the given `epoch` (called
    /// when a stream ends). The epoch guard makes teardown of a stream that was
    /// already replaced under the same `conn_id` a no-op, so it cannot evict the
    /// successor that took its place.
    fn remove_if_epoch(&self, conn_id: &str, epoch: u64) {
        if let Ok(mut conns) = self.conns.lock() {
            if conns.get(conn_id).is_some_and(|conn| conn.epoch == epoch) {
                conns.remove(conn_id);
            }
        }
    }

    /// Look up an open connection's inbound sink.
    fn get(
        &self,
        conn_id: &str,
        principal: &SsePrincipal,
    ) -> Result<mpsc::Sender<SseInboundMessage>, RegistryLookupError> {
        let conns = self.conns.lock().map_err(|_| RegistryLookupError::Unavailable)?;
        let connection = conns.get(conn_id).ok_or(RegistryLookupError::Unknown)?;
        if &connection.principal != principal {
            return Err(RegistryLookupError::PrincipalMismatch);
        }
        Ok(connection.tx.clone())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RegistryLookupError {
    Unknown,
    PrincipalMismatch,
    Unavailable,
}

/// RAII guard that deregisters a connection when the stream's driver task ends.
struct ConnGuard {
    registry: Arc<SseRegistry>,
    conn_id: String,
    epoch: u64,
}

impl Drop for ConnGuard {
    fn drop(&mut self) {
        self.registry.remove_if_epoch(&self.conn_id, self.epoch);
    }
}

/// Query params shared by the SSE `GET` stream endpoints.
#[derive(Debug, Deserialize)]
pub struct SseQuery {
    /// Client-generated connection id correlating the GET stream with its POSTs.
    pub conn: String,
    /// Auth token (query-param auth, mirrors the WS handlers).
    pub token: Option<String>,
    /// Comma-separated upstream subscription flags (per-session stream only).
    pub subscribe: Option<String>,
}

/// Body of a `POST /sse/*/send` control message.
#[derive(Debug, Deserialize)]
pub struct SseSendQuery {
    pub conn: String,
    pub token: Option<String>,
}

/// Whether the caller is a land-scoped OIDC principal (gd-DcKLFjZuyl).
fn is_land_scoped(authenticated: Option<&auth::WsAuthenticated>) -> bool {
    authenticated.is_some_and(|a| a.scope().is_scoped())
}

fn unauthorized() -> axum::response::Response {
    axum::http::Response::builder()
        .status(401)
        .body(axum::body::Body::from("unauthorized"))
        .unwrap_or_default()
        .into_response()
}

fn forbidden() -> axum::response::Response {
    axum::http::Response::builder()
        .status(403)
        .body(axum::body::Body::from("insufficient permissions"))
        .unwrap_or_default()
        .into_response()
}

fn bad_request(message: &'static str) -> axum::response::Response {
    axum::http::Response::builder()
        .status(400)
        .body(axum::body::Body::from(message))
        .unwrap_or_default()
        .into_response()
}

/// Build an SSE response from an outbound channel, with keep-alive comments
/// so intermediary proxies don't reap an idle stream.
fn sse_response(out_rx: mpsc::Receiver<String>) -> axum::response::Response {
    let stream = ReceiverStream::new(out_rx)
        .map(|text| Ok::<Event, Infallible>(Event::default().data(text)));
    Sse::new(stream).keep_alive(KeepAlive::default()).into_response()
}

async fn authenticate_sse(
    state: &MuxState,
    headers: &axum::http::HeaderMap,
    query_token: Option<&str>,
    edge_trusted: bool,
) -> Result<Option<auth::WsAuthenticated>, axum::response::Response> {
    if state.config.auth_token.is_none() && state.oidc_verifier.is_none() {
        return Ok(None);
    }
    auth::ws_authorized(state, headers, query_token, edge_trusted)
        .await
        .map(Some)
        .ok_or_else(unauthorized)
}

/// `GET /sse/mux` — downstream aggregation feed over SSE.
///
/// Mirrors `/ws/mux` exactly: drives the shared [`run_mux_feed`] core, with the
/// inbound (client → server) side fed by `POST /sse/mux/send` via the registry.
pub async fn sse_mux_handler(
    State(state): State<Arc<MuxState>>,
    Query(query): Query<SseQuery>,
    edge_trusted: Option<Extension<crate::transport::edge_auth::TrustedOperator>>,
    headers: axum::http::HeaderMap,
) -> axum::response::Response {
    let edge_identity = edge_trusted.map(|Extension(identity)| identity);
    let authenticated =
        match authenticate_sse(&state, &headers, query.token.as_deref(), edge_identity.is_some())
            .await
        {
            Ok(authenticated) => authenticated,
            Err(response) => return response,
        };
    // The feed subscribes across sessions by id, so a land-scoped OIDC caller
    // (gd-DcKLFjZuyl) is refused it; it attaches per session instead.
    if is_land_scoped(authenticated.as_ref()) {
        return forbidden();
    }

    // Inbound: POST bodies (raw JSON text) → parsed MuxClientMessage → feed core.
    let (in_text_tx, mut in_text_rx) = mpsc::channel::<SseInboundMessage>(INBOUND_CAPACITY);
    let (feed_in_tx, feed_in_rx) = mpsc::channel::<MuxFeedMessage>(INBOUND_CAPACITY);
    let principal =
        SsePrincipal::from_authenticated(edge_identity.as_ref(), authenticated.as_ref());
    let epoch = state.sse.register(query.conn.clone(), in_text_tx, principal);

    // Outbound: feed core → SSE stream.
    let (out_tx, out_rx) = mpsc::channel::<String>(OUTBOUND_CAPACITY);

    let guard = ConnGuard { registry: Arc::clone(&state.sse), conn_id: query.conn.clone(), epoch };
    let feed_state = Arc::clone(&state);
    tokio::spawn(async move {
        // Hold the registry guard for the lifetime of the feed; dropping it
        // (when the core returns on client disconnect) deregisters the conn.
        let _guard = guard;
        // Parse inbound text into client messages until the POST side closes
        // or the feed core drops its receiver.
        let parser = tokio::spawn(async move {
            while let Some(inbound) = in_text_rx.recv().await {
                match serde_json::from_str::<MuxClientMessage>(&inbound.body) {
                    Ok(msg) => {
                        let feed_msg =
                            MuxFeedMessage { message: msg, edge_identity: inbound.edge_identity };
                        if feed_in_tx.send(feed_msg).await.is_err() {
                            break;
                        }
                    }
                    Err(_) => continue,
                }
            }
        });
        run_mux_feed(feed_state, out_tx, feed_in_rx).await;
        parser.abort();
    });

    sse_response(out_rx)
}

/// `POST /sse/mux/send` — upstream control messages for an open `/sse/mux`.
pub async fn sse_mux_send_handler(
    State(state): State<Arc<MuxState>>,
    Query(query): Query<SseSendQuery>,
    edge_trusted: Option<Extension<crate::transport::edge_auth::TrustedOperator>>,
    headers: axum::http::HeaderMap,
    body: String,
) -> axum::response::Response {
    let edge_identity = edge_trusted.map(|Extension(identity)| identity);
    let authenticated =
        match authenticate_sse(&state, &headers, query.token.as_deref(), edge_identity.is_some())
            .await
        {
            Ok(authenticated) => authenticated,
            Err(response) => return response,
        };
    if is_land_scoped(authenticated.as_ref()) {
        return forbidden();
    }
    let message = match serde_json::from_str::<MuxClientMessage>(&body) {
        Ok(message) => message,
        Err(_) => return bad_request("invalid mux message"),
    };
    if edge_identity
        .as_ref()
        .is_some_and(|identity| !identity.may_send(message.required_edge_role()))
    {
        return forbidden();
    }
    let principal =
        SsePrincipal::from_authenticated(edge_identity.as_ref(), authenticated.as_ref());
    route_inbound(&state, &query.conn, body, principal, edge_identity).await
}

/// `GET /sse/session/{id}` — downstream per-session PTY/state stream over SSE.
///
/// Mirrors `/ws/{id}`: bridges the upstream [`WsBridge`] to an SSE stream, with
/// inbound input/resize/rpc fed by `POST /sse/session/{id}/send`.
pub async fn sse_session_handler(
    State(state): State<Arc<MuxState>>,
    Path(session_id): Path<String>,
    Query(query): Query<SseQuery>,
    edge_trusted: Option<Extension<crate::transport::edge_auth::TrustedOperator>>,
    headers: axum::http::HeaderMap,
) -> axum::response::Response {
    let edge_identity = edge_trusted.map(|Extension(identity)| identity);
    let authenticated =
        match authenticate_sse(&state, &headers, query.token.as_deref(), edge_identity.is_some())
            .await
        {
            Ok(authenticated) => authenticated,
            Err(response) => return response,
        };

    // A land-scoped OIDC caller (gd-DcKLFjZuyl) gets the same 404 for another
    // land's session as for an unknown one.
    let scope = authenticated.as_ref().map_or(SessionScope::Unrestricted, |a| a.scope());
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

    let subscribe = query.subscribe.clone().unwrap_or_else(|| "pty,screen,state".to_owned());
    let flags = SubscriptionFlags::parse(&subscribe);

    // Register (replacing any stale entry for this conn id — an EventSource
    // auto-reconnect reuses it) before adding a bridge client.
    let (in_text_tx, in_text_rx) = mpsc::channel::<SseInboundMessage>(INBOUND_CAPACITY);
    let principal =
        SsePrincipal::from_authenticated(edge_identity.as_ref(), authenticated.as_ref());
    let epoch = state.sse.register(query.conn.clone(), in_text_tx, principal);

    let (bridge, client_id, client_rx) =
        crate::transport::ws::get_or_create_bridge_and_register(&entry, flags).await;

    // Outbound: bridge messages → SSE stream.
    let (out_tx, out_rx) = mpsc::channel::<String>(OUTBOUND_CAPACITY);

    let guard = ConnGuard { registry: Arc::clone(&state.sse), conn_id: query.conn.clone(), epoch };
    let bridge_for_task = Arc::clone(&bridge);
    let entry_for_task = Arc::clone(&entry);
    tokio::spawn(async move {
        let _guard = guard;
        drive_session_stream(
            entry_for_task,
            bridge_for_task,
            client_id,
            client_rx,
            in_text_rx,
            out_tx,
        )
        .await;
    });

    sse_response(out_rx)
}

/// Drives a single per-session SSE stream: forwards bridge → client, POST
/// bodies → bridge, and — crucially — detects downstream disconnect so the
/// bridge client is released promptly.
///
/// SSE is a readless `GET`, so unlike `ws::handle_ws` (whose WS read half
/// yields `Close`/`None` on TCP teardown) this task has no natural signal that
/// the browser has gone away. Its only view of a dropped `out_rx` is a failed
/// `out_tx.send`, which never runs while the session is *idle* (no PTY output,
/// no state change) — the common case for a Claude session parked at a prompt.
/// Without the `out_tx.closed()` arm below, closing an idle stream (tab close,
/// laptop sleep, network blip) would wedge this task forever on `client_rx`,
/// leaking the bridge client, its upstream WS connection, and this task —
/// monotonically, for the life of the process. This mirrors the 15s liveness
/// ping `run_mux_feed` added for the same reason (`ws_mux.rs`), but detects the
/// drop directly instead of probing, so no stray frame reaches the terminal.
async fn drive_session_stream(
    entry: Arc<SessionEntry>,
    bridge: Arc<WsBridge>,
    client_id: crate::upstream::bridge::ClientId,
    mut client_rx: mpsc::Receiver<Arc<str>>,
    mut in_text_rx: mpsc::Receiver<SseInboundMessage>,
    out_tx: mpsc::Sender<String>,
) {
    loop {
        tokio::select! {
            _ = entry.cancel.cancelled() => break,

            // Downstream client disconnected: the SSE response holding `out_rx`
            // was dropped, so `closed()` resolves. Run cleanup promptly.
            _ = out_tx.closed() => break,

            // Bridge → downstream SSE stream.
            msg = client_rx.recv() => {
                match msg {
                    Some(text) => {
                        if out_tx.send(text.to_string()).await.is_err() {
                            break;
                        }
                    }
                    None => break,
                }
            }

            // POST bodies → bridge upstream.
            incoming = in_text_rx.recv() => {
                match incoming {
                    Some(inbound) => bridge.send_upstream(client_id, inbound.body),
                    None => break,
                }
            }
        }
    }

    // Release this client and, if last, clear the shared bridge slot — under the
    // `ws_bridge` lock with a `ptr_eq` guard so a concurrently-registering client
    // can't be orphaned (ka-0000005810). Shared with `ws::handle_ws`.
    crate::transport::ws::release_client_and_maybe_clear(&entry, &bridge, client_id).await;
}

/// `POST /sse/session/{id}/send` — upstream messages for an open session SSE.
pub async fn sse_session_send_handler(
    State(state): State<Arc<MuxState>>,
    Path(_session_id): Path<String>,
    Query(query): Query<SseSendQuery>,
    edge_trusted: Option<Extension<crate::transport::edge_auth::TrustedOperator>>,
    headers: axum::http::HeaderMap,
    body: String,
) -> axum::response::Response {
    let edge_identity = edge_trusted.map(|Extension(identity)| identity);
    let authenticated =
        match authenticate_sse(&state, &headers, query.token.as_deref(), edge_identity.is_some())
            .await
        {
            Ok(authenticated) => authenticated,
            Err(response) => return response,
        };
    // The OPERATOR route gate is a floor only. This body is forwarded verbatim
    // to the upstream coop /ws, which executes admin-reserved raw/signal/
    // shutdown/restart/switch frames, so enforce the per-frame tier here —
    // mirroring `sse_mux_send_handler` (which decodes the multiplexed body) and
    // `ws::handle_ws`. Edge-authed callers are role-checked here, and a
    // land-scoped OIDC caller is capped at operator below; static-token and
    // unscoped OIDC callers keep their prior authority.
    let required = auth::edge_session_frame_required_role(&body);
    if edge_identity.as_ref().is_some_and(|identity| !identity.may_send(required)) {
        return forbidden();
    }
    // A land-scoped OIDC caller is capped at the operator tier, as on `/ws/{id}`.
    if authenticated.as_ref().is_some_and(|a| !a.scope().allows_role(required)) {
        return forbidden();
    }
    let principal =
        SsePrincipal::from_authenticated(edge_identity.as_ref(), authenticated.as_ref());
    route_inbound(&state, &query.conn, body, principal, edge_identity).await
}

/// Forward a control-message body to the matching open stream's inbound sink.
async fn route_inbound(
    state: &MuxState,
    conn_id: &str,
    body: String,
    principal: SsePrincipal,
    edge_identity: Option<TrustedOperator>,
) -> axum::response::Response {
    match state.sse.get(conn_id, &principal) {
        Ok(tx) => match tx.send(SseInboundMessage { body, edge_identity }).await {
            Ok(()) => (axum::http::StatusCode::NO_CONTENT, "").into_response(),
            // Stream gone between lookup and send.
            Err(_) => (axum::http::StatusCode::GONE, "stream closed").into_response(),
        },
        Err(RegistryLookupError::Unknown) => {
            (axum::http::StatusCode::NOT_FOUND, "unknown connection").into_response()
        }
        Err(RegistryLookupError::PrincipalMismatch) => {
            (axum::http::StatusCode::FORBIDDEN, "connection principal mismatch").into_response()
        }
        Err(RegistryLookupError::Unavailable) => {
            (axum::http::StatusCode::SERVICE_UNAVAILABLE, "connection registry unavailable")
                .into_response()
        }
    }
}

#[cfg(test)]
#[path = "sse_tests.rs"]
mod tests;
