// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! `/ws/mux` multiplexed WebSocket handler.
//!
//! Tracks which sessions each client subscribes to, manages poller
//! lifecycle, sends cached screen snapshots on connect, pushes state
//! transitions immediately + screen thumbnails periodically.

use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::time::Duration;

use axum::extract::ws::{Message, WebSocket};
use axum::extract::{Query, State, WebSocketUpgrade};
use axum::response::IntoResponse;
use axum::Extension;
use futures_util::{SinkExt, StreamExt};
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::state::{MuxEvent, MuxState, WatcherId, WatcherState};
use crate::transport::auth;
use crate::upstream::poller::spawn_screen_poller;

/// Query parameters for `/ws/mux`.
#[derive(Debug, Deserialize)]
pub struct MuxWsQuery {
    /// Auth token (query-param auth for WebSocket).
    pub token: Option<String>,
}

/// Client → server messages on `/ws/mux`.
#[derive(Debug, Deserialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub(crate) enum MuxClientMessage {
    /// Subscribe to events from specific sessions.
    Subscribe { sessions: Vec<String> },
    /// Unsubscribe from sessions.
    Unsubscribe { sessions: Vec<String> },
    /// Forward keyboard input to a session.
    #[serde(rename = "input:send")]
    InputSend {
        session: String,
        text: String,
        #[serde(default)]
        enter: bool,
    },
    /// Resize a session's terminal.
    Resize { session: String, cols: u16, rows: u16 },
}

impl MuxClientMessage {
    pub(crate) fn required_edge_role(&self) -> &'static str {
        match self {
            Self::Subscribe { .. } | Self::Unsubscribe { .. } => "gardenio-viewer",
            Self::InputSend { .. } | Self::Resize { .. } => "gardenio-operator",
        }
    }
}

/// A mux message paired with the identity authenticated on the request that
/// submitted it. WebSockets reuse the upgrade identity; SSE supplies the
/// identity from each individual POST.
#[derive(Debug)]
pub(crate) struct MuxFeedMessage {
    pub(crate) message: MuxClientMessage,
    pub(crate) edge_identity: Option<crate::transport::edge_auth::TrustedOperator>,
}

/// Server → client messages on `/ws/mux`.
///
/// Uses manual serialization because `MuxEvent` uses its own `#[serde(tag)]`
/// and cannot be nested as `#[serde(untagged)]` inside a tagged enum.
#[derive(Debug)]
enum MuxServerMessage {
    /// Session list on connect.
    Sessions { sessions: Vec<SessionSnapshot> },
    /// An event from a watched session (serialized directly as its own tagged JSON).
    Event(Box<MuxEvent>),
    /// Periodic screen thumbnail batch.
    ScreenBatch { screens: Vec<ScreenThumbnail> },
    /// Liveness heartbeat. Lets an otherwise-idle feed (no watched sessions)
    /// notice a dropped receiver — critical for the SSE fallback, whose stream
    /// task would otherwise leak until the next real event. Clients ignore it.
    Ping,
    /// Error.
    Error { message: String },
}

impl serde::Serialize for MuxServerMessage {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        match self {
            Self::Sessions { sessions } => {
                #[derive(Serialize)]
                struct Msg<'a> {
                    event: &'static str,
                    sessions: &'a [SessionSnapshot],
                }
                Msg { event: "sessions", sessions }.serialize(serializer)
            }
            Self::Event(mux_event) => mux_event.serialize(serializer),
            Self::Ping => {
                #[derive(Serialize)]
                struct Msg {
                    event: &'static str,
                }
                Msg { event: "ping" }.serialize(serializer)
            }
            Self::ScreenBatch { screens } => {
                #[derive(Serialize)]
                struct Msg<'a> {
                    event: &'static str,
                    screens: &'a [ScreenThumbnail],
                }
                Msg { event: "screen_batch", screens }.serialize(serializer)
            }
            Self::Error { message } => {
                #[derive(Serialize)]
                struct Msg<'a> {
                    event: &'static str,
                    message: &'a str,
                }
                Msg { event: "error", message }.serialize(serializer)
            }
        }
    }
}

#[derive(Debug, Serialize)]
struct SessionSnapshot {
    id: String,
    url: String,
    state: Option<String>,
    metadata: serde_json::Value,
    #[serde(skip_serializing_if = "Option::is_none")]
    assigned_account: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    litellm_key_suffix: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    claimed_bead_title: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    total_cost_usd: Option<f64>,
}

#[derive(Debug, Serialize)]
struct ScreenThumbnail {
    session: String,
    lines: Vec<String>,
    ansi: Vec<String>,
    cols: u16,
    rows: u16,
    seq: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    state: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    assigned_account: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    litellm_key_suffix: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    total_cost_usd: Option<f64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct ScreenVersion {
    screen_seq: u64,
    cols: u16,
    rows: u16,
    state: Option<String>,
    assigned_account: Option<String>,
    litellm_key_suffix: Option<String>,
    total_cost_bits: Option<u64>,
}

impl ScreenThumbnail {
    fn version(&self) -> ScreenVersion {
        ScreenVersion {
            screen_seq: self.seq,
            cols: self.cols,
            rows: self.rows,
            state: self.state.clone(),
            assigned_account: self.assigned_account.clone(),
            litellm_key_suffix: self.litellm_key_suffix.clone(),
            total_cost_bits: self.total_cost_usd.map(f64::to_bits),
        }
    }
}

/// WebSocket upgrade handler for `/ws/mux`.
pub async fn ws_mux_handler(
    State(state): State<Arc<MuxState>>,
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
    if !no_auth {
        match auth::ws_authorized(&state, &headers, query.token.as_deref(), edge_identity.is_some())
            .await
        {
            None => {
                return axum::http::Response::builder()
                    .status(401)
                    .body(axum::body::Body::from("unauthorized"))
                    .unwrap_or_default()
                    .into_response();
            }
            // The feed subscribes across sessions by id, so a land-scoped OIDC
            // caller (gd-DcKLFjZuyl) is refused it; it attaches per session.
            Some(authenticated) if authenticated.scope().is_scoped() => {
                return axum::http::Response::builder()
                    .status(403)
                    .body(axum::body::Body::from("forbidden"))
                    .unwrap_or_default()
                    .into_response();
            }
            Some(_) => {}
        }
    }

    ws.on_upgrade(move |socket| handle_mux_ws(state, socket, edge_identity)).into_response()
}

/// Per-connection handler for `/ws/mux`.
///
/// Bridges the WebSocket to the transport-agnostic [`run_mux_feed`] core via a
/// pair of mpsc channels: server→client JSON strings on `out_rx`, client→server
/// parsed messages on `in_tx`. The SSE fallback ([`crate::transport::sse`])
/// drives the same core with HTTP-backed channels instead.
async fn handle_mux_ws(
    state: Arc<MuxState>,
    socket: WebSocket,
    edge_identity: Option<crate::transport::edge_auth::TrustedOperator>,
) {
    let (mut ws_tx, mut ws_rx) = socket.split();

    // server → client (serialized JSON) and client → server (parsed messages).
    let (out_tx, mut out_rx) = mpsc::channel::<String>(256);
    let (in_tx, in_rx) = mpsc::channel::<MuxFeedMessage>(64);

    // Drive the feed core in a task; it owns watch state + cleanup.
    let feed_state = Arc::clone(&state);
    let feed = tokio::spawn(async move { run_mux_feed(feed_state, out_tx, in_rx).await });

    loop {
        tokio::select! {
            // Feed core → downstream WebSocket.
            outgoing = out_rx.recv() => {
                match outgoing {
                    Some(text) => {
                        if ws_tx.send(Message::Text(text.into())).await.is_err() {
                            break;
                        }
                    }
                    None => break,
                }
            }

            // Downstream WebSocket → feed core.
            incoming = ws_rx.next() => {
                let msg = match incoming {
                    Some(Ok(m)) => m,
                    Some(Err(_)) | None => break,
                };
                match msg {
                    Message::Text(text) => {
                        match serde_json::from_str::<MuxClientMessage>(&text) {
                            Ok(client_msg) => {
                                let feed_msg = MuxFeedMessage {
                                    message: client_msg,
                                    edge_identity: edge_identity.clone(),
                                };
                                if in_tx.send(feed_msg).await.is_err() {
                                    break;
                                }
                            }
                            Err(_) => {
                                let err = MuxServerMessage::Error {
                                    message: "invalid message".to_owned(),
                                };
                                if let Ok(text) = serde_json::to_string(&err) {
                                    let _ = ws_tx.send(Message::Text(text.into())).await;
                                }
                            }
                        }
                    }
                    Message::Close(_) => break,
                    _ => {}
                }
            }
        }
    }

    let _ = ws_tx.send(Message::Close(None)).await;
    // Dropping in_tx/out_rx ends the feed core, which runs its own cleanup.
    drop(in_tx);
    drop(out_rx);
    let _ = feed.await;
}

/// Transport-agnostic mux aggregation feed.
///
/// Sends serialized server→client JSON on `out_tx`, consumes parsed
/// client→server messages from `in_rx`, and tracks per-connection watch state.
/// Returns when either channel closes (client disconnect), running watcher
/// cleanup on the way out. Shared by the WebSocket handler and the SSE
/// fallback so both transports carry identical mux semantics.
pub(crate) async fn run_mux_feed(
    state: Arc<MuxState>,
    out_tx: mpsc::Sender<String>,
    mut in_rx: mpsc::Receiver<MuxFeedMessage>,
) {
    let mut event_rx = state.feed.event_tx.subscribe();

    // Unique id for this feed loop; identifies this client in every session's
    // watcher set so a generation swap can reconstruct the true population
    // instead of collapsing all watchers to a single refcount.
    let watcher_id = state.feed.next_watcher_id();

    // Track which sessions this client is watching.
    let mut watched: HashSet<String> = HashSet::new();
    let mut last_sent_screen_version: HashMap<String, ScreenVersion> = HashMap::new();

    // Send initial session list.
    {
        let sessions = state.sessions.read().await;
        let mut snapshots = Vec::with_capacity(sessions.len());
        for entry in sessions.values() {
            let cached_state = entry.cached_status.read().await.as_ref().map(|s| s.state.clone());
            let assigned_account = entry.assigned_account.read().await.clone();
            let litellm_key_suffix = match &assigned_account {
                Some(acct) => state.credential_broker.key_suffix(acct).await,
                None => None,
            };
            let claimed_bead_title = entry.claimed_bead_title.read().await.clone();
            let total_cost_usd = entry.cached_usage.read().await.as_ref().map(|u| u.total_cost_usd);
            snapshots.push(SessionSnapshot {
                id: entry.id.clone(),
                url: entry.url.clone(),
                state: cached_state,
                metadata: entry.metadata.clone(),
                assigned_account,
                litellm_key_suffix,
                claimed_bead_title,
                total_cost_usd,
            });
        }
        drop(sessions);
        let msg = MuxServerMessage::Sessions { sessions: snapshots };
        if send_feed_json(&out_tx, &msg).await.is_err() {
            return;
        }
    }

    // Screen thumbnail push interval (1 Hz).
    let mut screen_interval = tokio::time::interval(std::time::Duration::from_secs(1));
    screen_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

    // Liveness ping (15s). Keeps the outbound channel exercised so a feed with
    // no watched sessions still notices a dropped receiver (SSE fallback).
    let mut ping_interval = tokio::time::interval(std::time::Duration::from_secs(15));
    ping_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

    loop {
        tokio::select! {
            // Liveness heartbeat.
            _ = ping_interval.tick() => {
                if send_feed_json(&out_tx, &MuxServerMessage::Ping).await.is_err() {
                    break;
                }
            }

            // Mux events (state transitions, online/offline).
            event = event_rx.recv() => {
                let event = match event {
                    Ok(e) => e,
                    Err(tokio::sync::broadcast::error::RecvError::Lagged(dropped)) => {
                        // The broadcast channel overflowed and silently dropped
                        // `dropped` events for this feed. Any of them may have
                        // been a SessionOnline for a watched session that
                        // restarted under the same id — the event we normally
                        // answer with ensure_watching to rebind the watcher onto
                        // the new generation and force a fresh thumbnail. Because
                        // we cannot tell which events were lost, re-arm every
                        // currently watched session defensively so a stale
                        // generation binding self-heals here instead of
                        // persisting (stale thumbnails) until the client happens
                        // to re-subscribe.
                        tracing::warn!(
                            dropped,
                            watched = watched.len(),
                            "mux feed lagged; re-arming watchers"
                        );
                        for session in &watched {
                            last_sent_screen_version.remove(session);
                            ensure_watching(&state, watcher_id, session).await;
                        }
                        continue;
                    }
                    Err(_) => break,
                };
                match &event {
                    MuxEvent::SessionOffline { session } => {
                        last_sent_screen_version.remove(session);
                    }
                    MuxEvent::SessionOnline { session, .. } if watched.contains(session) => {
                        last_sent_screen_version.remove(session);
                        ensure_watching(&state, watcher_id, session).await;
                    }
                    _ => {}
                }
                // Forward session lifecycle and credential events to all clients;
                // state transitions only for watched sessions.
                let should_forward = match &event {
                    MuxEvent::CredentialRefreshed { .. }
                    | MuxEvent::CredentialRefreshFailed { .. }
                    | MuxEvent::SessionOnline { .. }
                    | MuxEvent::SessionOffline { .. } => true,
                    MuxEvent::Transition { session, .. } => watched.contains(session),
                    // Forward unknown future variants by default
                    #[allow(unreachable_patterns)]
                    _ => true,
                };
                if should_forward {
                    let msg = MuxServerMessage::Event(Box::new(event));
                    if send_feed_json(&out_tx, &msg).await.is_err() {
                        break;
                    }
                }
            }

            // Screen thumbnail batch.
            _ = screen_interval.tick() => {
                if watched.is_empty() {
                    continue;
                }
                let screens = collect_screens(
                    &state,
                    watched.iter().map(String::as_str),
                    &last_sent_screen_version,
                )
                .await;
                if !screens.is_empty()
                    && send_screen_batch(&out_tx, screens, &mut last_sent_screen_version)
                        .await
                        .is_err()
                {
                    break;
                }
            }

            // Client messages.
            msg = in_rx.recv() => {
                let feed_msg = match msg {
                    Some(m) => m,
                    None => break,
                };
                let MuxFeedMessage { message: client_msg, edge_identity } = feed_msg;
                match client_msg {
                    MuxClientMessage::Subscribe { sessions } => {
                        let mut new_sids = Vec::new();
                        for sid in sessions {
                            if watched.insert(sid.clone()) {
                                start_watching(&state, watcher_id, &sid).await;
                                new_sids.push(sid);
                            }
                        }
                        // Send immediate screen_batch for newly subscribed sessions
                        // so clients don't wait for the next periodic tick.
                        if !new_sids.is_empty() {
                            let screens = collect_screens(
                                &state,
                                new_sids.iter().map(String::as_str),
                                &last_sent_screen_version,
                            )
                            .await;
                            if !screens.is_empty()
                                && send_screen_batch(
                                    &out_tx,
                                    screens,
                                    &mut last_sent_screen_version,
                                )
                                .await
                                .is_err()
                            {
                                break;
                            }
                        }
                    }
                    MuxClientMessage::Unsubscribe { sessions } => {
                        for sid in sessions {
                            if watched.remove(&sid) {
                                last_sent_screen_version.remove(&sid);
                                stop_watching(&state, watcher_id, &sid).await;
                            }
                        }
                    }
                    MuxClientMessage::InputSend { session, text, enter } => {
                        if !edge_can_operate(edge_identity.as_ref()) {
                            audit_mux_action(
                                edge_identity.as_ref(),
                                "input_send",
                                &session,
                                "denied",
                            );
                            let _ = send_feed_json(
                                &out_tx,
                                &MuxServerMessage::Error {
                                    message: "insufficient permissions: requires gardenio-operator"
                                        .to_owned(),
                                },
                            )
                            .await;
                            continue;
                        }
                        proxy_input(&state, &session, &text, enter).await;
                        audit_mux_action(
                            edge_identity.as_ref(),
                            "input_send",
                            &session,
                            "forwarded",
                        );
                    }
                    MuxClientMessage::Resize { session, cols, rows } => {
                        if !edge_can_operate(edge_identity.as_ref()) {
                            audit_mux_action(
                                edge_identity.as_ref(),
                                "resize",
                                &session,
                                "denied",
                            );
                            let _ = send_feed_json(
                                &out_tx,
                                &MuxServerMessage::Error {
                                    message: "insufficient permissions: requires gardenio-operator"
                                        .to_owned(),
                                },
                            )
                            .await;
                            continue;
                        }
                        proxy_resize(&state, &session, cols, rows).await;
                        audit_mux_action(
                            edge_identity.as_ref(),
                            "resize",
                            &session,
                            "forwarded",
                        );
                    }
                }
            }
        }
    }

    // Cleanup: stop watching all sessions.
    for sid in &watched {
        stop_watching(&state, watcher_id, sid).await;
    }
}

fn edge_can_operate(identity: Option<&crate::transport::edge_auth::TrustedOperator>) -> bool {
    identity.is_none_or(|id| id.may_send("gardenio-operator"))
}

fn audit_mux_action(
    identity: Option<&crate::transport::edge_auth::TrustedOperator>,
    action: &str,
    session: &str,
    outcome: &str,
) {
    let Some(identity) = identity else {
        return;
    };
    tracing::info!(
        operator = %identity.operator,
        audit_id = %identity.audit_id,
        auth_mode = %identity.auth_mode,
        roles = ?identity.roles,
        action = %action,
        target_session = %session,
        outcome = %outcome,
        "edge-auth mux action",
    );
}

/// Collect screen thumbnails for the given watched session ids.
async fn collect_screens<'a>(
    state: &MuxState,
    session_ids: impl Iterator<Item = &'a str>,
    last_sent_screen_version: &HashMap<String, ScreenVersion>,
) -> Vec<ScreenThumbnail> {
    let entries: Vec<(String, Arc<crate::state::SessionEntry>)> = {
        let sessions = state.sessions.read().await;
        session_ids
            .filter_map(|session_id| {
                sessions.get(session_id).map(|entry| (session_id.to_owned(), Arc::clone(entry)))
            })
            .collect()
    };

    let mut screens = Vec::with_capacity(entries.len());
    for (session_id, entry) in entries {
        let cached_state = entry.cached_status.read().await.as_ref().map(|s| s.state.clone());
        let assigned_account = entry.assigned_account.read().await.clone();
        let litellm_key_suffix = match &assigned_account {
            Some(acct) => state.credential_broker.key_suffix(acct).await,
            None => None,
        };
        let total_cost_usd = entry.cached_usage.read().await.as_ref().map(|u| u.total_cost_usd);
        let cached_screen = entry.cached_screen.read().await;
        let Some(screen) = cached_screen.as_ref() else {
            continue;
        };
        let version = ScreenVersion {
            screen_seq: screen.seq,
            cols: screen.cols,
            rows: screen.rows,
            state: cached_state.clone(),
            assigned_account: assigned_account.clone(),
            litellm_key_suffix: litellm_key_suffix.clone(),
            total_cost_bits: total_cost_usd.map(f64::to_bits),
        };
        if last_sent_screen_version.get(&session_id) == Some(&version) {
            continue;
        }
        let thumbnail = ScreenThumbnail {
            session: session_id,
            lines: screen.lines.clone(),
            ansi: screen.ansi.clone(),
            cols: screen.cols,
            rows: screen.rows,
            seq: screen.seq,
            state: cached_state,
            assigned_account,
            litellm_key_suffix,
            total_cost_usd,
        };
        screens.push(thumbnail);
    }
    screens
}

async fn send_screen_batch(
    tx: &mpsc::Sender<String>,
    screens: Vec<ScreenThumbnail>,
    last_sent_screen_version: &mut HashMap<String, ScreenVersion>,
) -> Result<(), ()> {
    let sent_versions: Vec<(String, ScreenVersion)> =
        screens.iter().map(|screen| (screen.session.clone(), screen.version())).collect();
    let msg = MuxServerMessage::ScreenBatch { screens };
    send_feed_json(tx, &msg).await?;
    for (session_id, version) in sent_versions {
        last_sent_screen_version.insert(session_id, version);
    }
    Ok(())
}

/// Serialize a server message and send it on the feed sink.
async fn send_feed_json(tx: &mpsc::Sender<String>, msg: &MuxServerMessage) -> Result<(), ()> {
    let text = serde_json::to_string(msg).map_err(|_| ())?;
    tx.send(text).await.map_err(|_| ())
}

/// Register `watcher_id` on a session, starting screen/status pollers if needed.
async fn start_watching(state: &MuxState, watcher_id: WatcherId, session_id: &str) {
    watch_session(state, watcher_id, session_id).await;
}

/// Ensure an already-subscribed feed follows the current session generation
/// after a `session:online`. Re-adding the same id is idempotent (set dedup),
/// and it re-registers the id if a generation swap dropped the watcher entry.
async fn ensure_watching(state: &MuxState, watcher_id: WatcherId, session_id: &str) {
    watch_session(state, watcher_id, session_id).await;
}

async fn watch_session(state: &MuxState, watcher_id: WatcherId, session_id: &str) {
    // Snapshot the session before touching watchers so this path never holds
    // watchers while waiting on sessions, which would invert remove_session's order.
    let entry = {
        let sessions = state.sessions.read().await;
        match sessions.get(session_id) {
            Some(e) => Arc::clone(e),
            None => return,
        }
    };

    let poller_cancel = CancellationToken::new();

    {
        let mut watchers = state.feed.watchers.write().await;
        if entry.cancel.is_cancelled() {
            return;
        }
        if let Some(ws) = watchers.get_mut(session_id) {
            let same_generation =
                ws.entry.upgrade().is_some_and(|watched_entry| Arc::ptr_eq(&watched_entry, &entry));
            if same_generation {
                // Set dedup: re-watching the same generation never inflates.
                ws.watchers.insert(watcher_id);
                return;
            }
            ws.poller_cancel.cancel();
        }
        watchers.insert(
            session_id.to_owned(),
            WatcherState {
                watchers: HashSet::from([watcher_id]),
                entry: Arc::downgrade(&entry),
                poller_cancel: poller_cancel.clone(),
            },
        );
    }

    if poller_cancel.is_cancelled() {
        return;
    }

    // NATS-transport sessions receive screen/status updates via NATS. HTTP
    // sessions already own exactly one lifetime event feed from registration;
    // dashboard watchers only control their polling tasks.
    if !matches!(entry.transport, crate::state::SessionTransport::Nats { .. }) {
        spawn_screen_poller(entry, &state.config, state.http_client.clone(), poller_cancel);
    }
}

/// Remove `watcher_id` from a session, stopping pollers when the last watcher leaves.
async fn stop_watching(state: &MuxState, watcher_id: WatcherId, session_id: &str) {
    let mut watchers = state.feed.watchers.write().await;
    if let Some(ws) = watchers.get_mut(session_id) {
        ws.watchers.remove(&watcher_id);
        if ws.watchers.is_empty() {
            ws.poller_cancel.cancel();
            watchers.remove(session_id);
        }
    }
}

/// Per-request timeout for the inline input/resize proxies.
///
/// `run_mux_feed` awaits `proxy_input`/`proxy_resize` INLINE inside its single
/// `tokio::select!` loop, so a hung upstream POST would freeze the operator's
/// entire live feed (no pings, no screen thumbnails, no further input). The
/// shared `state.http_client` carries no default timeout, so cap each call
/// here — matching the sibling reassign/prewarm/distributor/rebalance paths
/// (ka-0000002323, ka-0000003032, ka-0000004551). See ka-0000003978.
const PROXY_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

/// Proxy keyboard input to an upstream session.
async fn proxy_input(state: &MuxState, session_id: &str, text: &str, enter: bool) {
    proxy_input_with_timeout(state, session_id, text, enter, PROXY_REQUEST_TIMEOUT).await;
}

/// Internal variant of [`proxy_input`] that takes the per-request timeout
/// explicitly. Kept private so production always uses the tuned
/// [`PROXY_REQUEST_TIMEOUT`]; tests inject a short timeout to prove a hung
/// upstream POST is bounded and does not stall the `run_mux_feed` loop.
async fn proxy_input_with_timeout(
    state: &MuxState,
    session_id: &str,
    text: &str,
    enter: bool,
    request_timeout: Duration,
) {
    let sessions = state.sessions.read().await;
    let entry = match sessions.get(session_id) {
        Some(e) => Arc::clone(e),
        None => return,
    };
    drop(sessions);

    let body = serde_json::json!({ "text": text, "enter": enter });

    // For NATS-transport sessions, publish via NATS instead of HTTP.
    if let crate::state::SessionTransport::Nats { ref prefix } = entry.transport {
        let subject = format!("{prefix}.session.{session_id}.input");
        let client_guard = state.nats_client.read().await;
        if let Some(ref nats_client) = *client_guard {
            let payload = serde_json::to_vec(&body).unwrap_or_default();
            let _ = nats_client.publish(subject, payload.into()).await;
        }
        return;
    }

    let client = crate::upstream::client::UpstreamClient::with_timeout(
        state.http_client.clone(),
        entry.url.clone(),
        entry.auth_token.clone(),
        request_timeout,
    );
    let _ = client.post_json("/api/v1/input", &body).await;
}

/// Proxy terminal resize to an upstream session.
async fn proxy_resize(state: &MuxState, session_id: &str, cols: u16, rows: u16) {
    proxy_resize_with_timeout(state, session_id, cols, rows, PROXY_REQUEST_TIMEOUT).await;
}

/// Internal variant of [`proxy_resize`] that takes the per-request timeout
/// explicitly. See [`proxy_input_with_timeout`].
async fn proxy_resize_with_timeout(
    state: &MuxState,
    session_id: &str,
    cols: u16,
    rows: u16,
    request_timeout: Duration,
) {
    let sessions = state.sessions.read().await;
    let entry = match sessions.get(session_id) {
        Some(e) => Arc::clone(e),
        None => return,
    };
    drop(sessions);

    let client = crate::upstream::client::UpstreamClient::with_timeout(
        state.http_client.clone(),
        entry.url.clone(),
        entry.auth_token.clone(),
        request_timeout,
    );
    let body = serde_json::json!({ "cols": cols, "rows": rows });
    let _ = client.post_json("/api/v1/resize", &body).await;
}

#[cfg(test)]
#[path = "ws_mux_tests.rs"]
mod tests;
