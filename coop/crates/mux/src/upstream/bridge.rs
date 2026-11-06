// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Bidirectional WebSocket bridge: single upstream connection multiplexed to N downstream clients.
//!
//! Features:
//! - **Per-client channels**: each downstream client gets a dedicated bounded mpsc channel.
//! - **Correlation routing**: outgoing requests are stamped with a `request_id`; responses are
//!   routed back to the originating client only.
//! - **Subscription filtering**: streaming events are forwarded only to clients whose flags match.
//! - **Upstream write**: downstream messages are forwarded through the single upstream connection.
//! - **Backpressure**: bounded channels prevent unbounded memory growth from slow clients.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use futures_util::{SinkExt, StreamExt};
use serde::Deserialize;
use tokio::sync::{mpsc, RwLock};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;

use crate::state::SessionEntry;

/// Maximum buffered messages per downstream client before backpressure kicks in.
/// Sized for PTY output bursts — a connected WebSocket client drains quickly.
const CLIENT_CHANNEL_CAPACITY: usize = 256;

/// Bound the upstream WebSocket handshake. `tokio_tungstenite::connect_async`
/// does its own TCP connect + TLS + HTTP-Upgrade with no timeout, so a parked /
/// half-open upstream pod would otherwise block this bridge task forever — never
/// reaching backoff/reconnect and, because the bare await never polls the cancel
/// token, never abortable by `Drop`. Mirrors the 10s UpstreamClient::with_timeout
/// used on every sibling HTTP path.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// Maximum buffered messages from downstream clients awaiting upstream send.
/// Lower than client capacity since commands flow slower than output.
const UPSTREAM_CHANNEL_CAPACITY: usize = 128;

/// Number of consecutive send failures before a slow client is disconnected.
const CLIENT_DROP_THRESHOLD: u32 = 16;

/// Maximum number of response-bearing requests retained by one bridge.
const MAX_PENDING_REQUESTS: usize = 1024;

/// Maximum number of response-bearing requests retained for one client.
const MAX_PENDING_PER_CLIENT: usize = 128;

/// Maximum time a request may wait for its upstream response.
const PENDING_REQUEST_TIMEOUT: Duration = Duration::from_secs(30);

/// Frequency for expiring requests and removing entries for disconnected clients.
const PENDING_SWEEP_INTERVAL: Duration = Duration::from_secs(1);

/// Identifies a downstream client within the bridge.
pub type ClientId = u64;

/// Subscription flags controlling which upstream events reach a downstream client.
#[derive(Debug, Clone, Copy, Default)]
pub struct SubscriptionFlags {
    pub pty: bool,
    pub screen: bool,
    pub state: bool,
}

impl SubscriptionFlags {
    /// Parse a comma-separated flags string (e.g. `"pty,state"`).
    pub fn parse(s: &str) -> Self {
        let mut flags = Self::default();
        for token in s.split(',') {
            match token.trim() {
                "pty" | "output" => flags.pty = true,
                "screen" => flags.screen = true,
                "state" => flags.state = true,
                _ => {}
            }
        }
        flags
    }

    /// Return true if an upstream event with the given `"event"` tag should be forwarded.
    fn matches_event(&self, event: Option<&str>) -> bool {
        match event {
            Some("pty" | "output") => self.pty,
            Some("replay") => self.pty,
            Some("screen") => self.screen,
            Some("transition" | "exit" | "prompt:outcome" | "stop:outcome" | "start:outcome") => {
                self.state
            }
            // Responses (have request_id) and unknown events always pass through.
            _ => true,
        }
    }
}

/// Per-client bookkeeping within the bridge.
struct ClientSlot {
    tx: mpsc::Sender<Arc<str>>,
    flags: SubscriptionFlags,
    /// Consecutive send failures (channel full). Reset on successful send.
    overflow_count: u32,
}

/// In-flight request awaiting an upstream response.
struct PendingRequest {
    client_id: ClientId,
    /// Original downstream correlation ID, restored before returning a response.
    client_request_id: Option<String>,
    deadline: Instant,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum PendingInsertError {
    BridgeFull,
    ClientFull,
}

#[derive(Default)]
struct PendingRequests {
    requests: HashMap<String, PendingRequest>,
    per_client: HashMap<ClientId, usize>,
}

impl PendingRequests {
    fn try_insert(
        &mut self,
        request_id: String,
        request: PendingRequest,
    ) -> Result<(), PendingInsertError> {
        if self.requests.len() >= MAX_PENDING_REQUESTS {
            return Err(PendingInsertError::BridgeFull);
        }
        if self.count_for_client(request.client_id) >= MAX_PENDING_PER_CLIENT {
            return Err(PendingInsertError::ClientFull);
        }

        *self.per_client.entry(request.client_id).or_default() += 1;
        self.requests.insert(request_id, request);
        Ok(())
    }

    fn remove(&mut self, request_id: &str) -> Option<PendingRequest> {
        let request = self.requests.remove(request_id)?;
        self.decrement_client(request.client_id);
        Some(request)
    }

    fn expire(&mut self, now: Instant) -> Vec<PendingRequest> {
        let expired_ids: Vec<String> = self
            .requests
            .iter()
            .filter(|(_, request)| request.deadline <= now)
            .map(|(request_id, _)| request_id.clone())
            .collect();
        expired_ids.iter().filter_map(|request_id| self.remove(request_id)).collect()
    }

    fn remove_disconnected(&mut self, active_clients: &HashSet<ClientId>) -> Vec<PendingRequest> {
        let disconnected_ids: Vec<String> = self
            .requests
            .iter()
            .filter(|(_, request)| !active_clients.contains(&request.client_id))
            .map(|(request_id, _)| request_id.clone())
            .collect();
        disconnected_ids.iter().filter_map(|request_id| self.remove(request_id)).collect()
    }

    fn drain_all(&mut self) -> Vec<PendingRequest> {
        self.per_client.clear();
        self.requests.drain().map(|(_, request)| request).collect()
    }

    fn count_for_client(&self, client_id: ClientId) -> usize {
        self.per_client.get(&client_id).copied().unwrap_or_default()
    }

    fn decrement_client(&mut self, client_id: ClientId) {
        let Some(count) = self.per_client.get_mut(&client_id) else {
            return;
        };
        *count = count.saturating_sub(1);
        if *count == 0 {
            self.per_client.remove(&client_id);
        }
    }
}

/// Bidirectional WebSocket bridge for a single upstream session.
///
/// One upstream WS connection is shared across all downstream clients.  The bridge handles:
/// - Fan-out of streaming events filtered by per-client [`SubscriptionFlags`].
/// - Stamping outgoing requests with correlation IDs and routing responses to originators.
/// - Backpressure: bounded channels drop messages for slow clients; persistently slow
///   clients are disconnected after [`CLIENT_DROP_THRESHOLD`] consecutive failures.
pub struct WsBridge {
    /// Send downstream-originated messages upstream (client_id, raw JSON text).
    upstream_tx: mpsc::Sender<(ClientId, String)>,
    /// Per-client state. Guarded for add/remove from outside the run loop.
    clients: Arc<RwLock<HashMap<ClientId, ClientSlot>>>,
    next_id: AtomicU64,
    cancel: CancellationToken,
}

impl WsBridge {
    /// Create and start a new bidirectional WS bridge for the given session.
    ///
    /// The bridge connects to upstream with `pty,state` subscriptions and filters
    /// per-client on the downstream side.
    pub fn connect(entry: &Arc<SessionEntry>) -> Arc<Self> {
        let cancel = entry.cancel.child_token();
        let clients: Arc<RwLock<HashMap<ClientId, ClientSlot>>> =
            Arc::new(RwLock::new(HashMap::new()));
        let (upstream_tx, upstream_rx) = mpsc::channel(UPSTREAM_CHANNEL_CAPACITY);

        let bridge = Arc::new(Self {
            upstream_tx,
            clients: Arc::clone(&clients),
            next_id: AtomicU64::new(1),
            cancel: cancel.clone(),
        });

        // The upstream subscription must cover the union of all possible client flags.
        let subscribe = "pty,state";
        let url = build_ws_url(&entry.url, entry.auth_token.as_deref(), subscribe);
        let entry_id = entry.id.clone();

        tokio::spawn(run_loop(url, entry_id, cancel, clients, upstream_rx));

        bridge
    }

    /// Register a new downstream client with the given subscription flags.
    ///
    /// Returns a `(ClientId, Receiver)` pair.  Messages matching the flags (and
    /// correlation-routed responses) arrive on the receiver.
    pub async fn add_client(
        &self,
        flags: SubscriptionFlags,
    ) -> (ClientId, mpsc::Receiver<Arc<str>>) {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        let (tx, rx) = mpsc::channel(CLIENT_CHANNEL_CAPACITY);
        self.clients.write().await.insert(id, ClientSlot { tx, flags, overflow_count: 0 });
        (id, rx)
    }

    /// Remove a downstream client.
    pub async fn remove_client(&self, id: ClientId) {
        self.clients.write().await.remove(&id);
    }

    /// Return the number of connected downstream clients.
    pub async fn client_count(&self) -> usize {
        self.clients.read().await.len()
    }

    /// Send a message from a downstream client through the upstream WS connection.
    ///
    /// The bridge stamps a `request_id` so the response can be correlation-routed
    /// back to this client. If the upstream channel is full, the message is dropped.
    pub fn send_upstream(&self, client_id: ClientId, text: String) {
        if let Err(mpsc::error::TrySendError::Full(_)) =
            self.upstream_tx.try_send((client_id, text))
        {
            tracing::warn!("upstream channel full, dropping downstream message");
        }
    }
}

impl Drop for WsBridge {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}

async fn run_loop(
    url: String,
    entry_id: String,
    cancel: CancellationToken,
    clients: Arc<RwLock<HashMap<ClientId, ClientSlot>>>,
    mut downstream_rx: mpsc::Receiver<(ClientId, String)>,
) {
    let mut backoff_ms = 100u64;
    let max_backoff_ms = 5000u64;
    let mut rid_counter: u64 = 0;
    let mut pending = PendingRequests::default();

    loop {
        if cancel.is_cancelled() {
            break;
        }

        // Race the handshake against the cancel token and a hard timeout so a
        // parked upstream can neither hang this bridge nor make the task
        // uncancellable. `Ok(res)` = handshake settled, `Err(_)` = timed out.
        let connect_result = tokio::select! {
            _ = cancel.cancelled() => break,
            res = tokio::time::timeout(
                CONNECT_TIMEOUT,
                tokio_tungstenite::connect_async(&url),
            ) => res,
        };

        match connect_result {
            Ok(Ok((ws_stream, _))) => {
                backoff_ms = 100;
                tracing::debug!(session_id = %entry_id, "upstream WS connected");

                let (mut write, mut read) = ws_stream.split();
                let mut pending_sweep = tokio::time::interval(PENDING_SWEEP_INTERVAL);
                pending_sweep.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

                loop {
                    tokio::select! {
                        _ = cancel.cancelled() => return,

                        _ = pending_sweep.tick() => {
                            let active_clients: HashSet<ClientId> =
                                clients.read().await.keys().copied().collect();
                            let expired = pending.expire(Instant::now());
                            notify_pending_failures(
                                &clients,
                                expired,
                                "upstream_timeout",
                                "upstream request timed out",
                            )
                            .await;
                            // Disconnected clients do not need an error, but their retained
                            // request state must be released promptly.
                            let _ = pending.remove_disconnected(&active_clients);
                        }

                        // Upstream -> route to clients
                        msg = read.next() => {
                            match msg {
                                Some(Ok(Message::Text(text))) => {
                                    let text = text.to_string();
                                    let info = extract_route_info(&text);
                                    let shared: Arc<str> = Arc::from(text.as_str());

                                    if let Some(rid) = info.request_id {
                                        // Response: route to originator only.
                                        if let Some(req) = pending.remove(rid) {
                                            let guard = clients.read().await;
                                            if let Some(slot) = guard.get(&req.client_id) {
                                                let msg = restore_request_id(
                                                    &text,
                                                    req.client_request_id.as_deref(),
                                                );
                                                let _ = slot.tx.try_send(msg);
                                            }
                                        }
                                    } else {
                                        // Streaming event: fan out by subscription flags.
                                        // Collect IDs of slow clients to disconnect.
                                        let mut to_remove = Vec::new();
                                        let mut guard = clients.write().await;
                                        for (id, slot) in guard.iter_mut() {
                                            if slot.flags.matches_event(info.event) {
                                                match slot.tx.try_send(Arc::clone(&shared)) {
                                                    Ok(()) => {
                                                        slot.overflow_count = 0;
                                                    }
                                                    Err(mpsc::error::TrySendError::Full(_)) => {
                                                        slot.overflow_count += 1;
                                                        if slot.overflow_count >= CLIENT_DROP_THRESHOLD {
                                                            tracing::warn!(
                                                                client_id = id,
                                                                session_id = %entry_id,
                                                                "disconnecting slow client after {} consecutive drops",
                                                                slot.overflow_count
                                                            );
                                                            to_remove.push(*id);
                                                        } else {
                                                            tracing::debug!(
                                                                client_id = id,
                                                                overflow = slot.overflow_count,
                                                                "client channel full, dropping message"
                                                            );
                                                        }
                                                    }
                                                    Err(mpsc::error::TrySendError::Closed(_)) => {
                                                        to_remove.push(*id);
                                                    }
                                                }
                                            }
                                        }
                                        for id in to_remove {
                                            guard.remove(&id);
                                        }
                                    }
                                }
                                Some(Ok(Message::Close(_))) | None => {
                                    tracing::debug!(session_id = %entry_id, "upstream WS closed");
                                    break;
                                }
                                Some(Err(e)) => {
                                    tracing::debug!(session_id = %entry_id, err = %e, "upstream WS error");
                                    break;
                                }
                                _ => {} // ping/pong/binary ignored
                            }
                        }

                        // Downstream -> stamp request_id, forward upstream
                        msg = downstream_rx.recv() => {
                            match msg {
                                Some((client_id, text)) => {
                                    let route = extract_route_info(&text);
                                    if !expects_response(route.event) {
                                        if write.send(Message::Text(text.into())).await.is_err() {
                                            tracing::debug!(session_id = %entry_id, "upstream WS write failed");
                                            break;
                                        }
                                        continue;
                                    }

                                    let client_request_id = route.request_id.map(str::to_owned);
                                    let stamped = stamp_request_id(&mut rid_counter, &text);
                                    let request = PendingRequest {
                                        client_id,
                                        client_request_id,
                                        deadline: Instant::now() + PENDING_REQUEST_TIMEOUT,
                                    };
                                    if let Err(reason) =
                                        pending.try_insert(stamped.request_id.clone(), request)
                                    {
                                        let message = match reason {
                                            PendingInsertError::BridgeFull => {
                                                "bridge pending-request limit reached"
                                            }
                                            PendingInsertError::ClientFull => {
                                                "client pending-request limit reached"
                                            }
                                        };
                                        notify_client_failure(
                                            &clients,
                                            client_id,
                                            route.request_id,
                                            "upstream_overloaded",
                                            message,
                                        )
                                        .await;
                                        continue;
                                    }
                                    if write.send(Message::Text(stamped.text.into())).await.is_err() {
                                        tracing::debug!(session_id = %entry_id, "upstream WS write failed");
                                        break;
                                    }
                                }
                                None => return, // bridge dropped
                            }
                        }
                    }
                }
            }
            Ok(Err(e)) => {
                tracing::debug!(
                    session_id = %entry_id,
                    err = %e,
                    backoff_ms,
                    "upstream WS connect failed, retrying"
                );
            }
            Err(_elapsed) => {
                tracing::debug!(
                    session_id = %entry_id,
                    timeout_s = CONNECT_TIMEOUT.as_secs(),
                    backoff_ms,
                    "upstream WS connect timed out, retrying"
                );
            }
        }

        let disconnected = pending.drain_all();
        notify_pending_failures(
            &clients,
            disconnected,
            "upstream_disconnected",
            "upstream connection was lost",
        )
        .await;

        // Exponential backoff before reconnect.
        tokio::select! {
            _ = cancel.cancelled() => break,
            _ = tokio::time::sleep(std::time::Duration::from_millis(backoff_ms)) => {}
        }
        backoff_ms = (backoff_ms * 2).min(max_backoff_ms);
    }
}

/// Lightweight routing info extracted from a JSON message without full deserialization.
#[derive(Deserialize, Default)]
struct RouteInfo<'a> {
    #[serde(default)]
    event: Option<&'a str>,
    #[serde(default)]
    request_id: Option<&'a str>,
}

/// Extract the `event` and `request_id` fields from a JSON object.
fn extract_route_info(json: &str) -> RouteInfo<'_> {
    serde_json::from_str(json).unwrap_or_default()
}

/// Successful authentication is the only upstream command that deliberately
/// returns no response. All other valid client messages have a response and
/// need correlation routing even when the caller omitted a request ID.
fn expects_response(event: Option<&str>) -> bool {
    !matches!(event, Some("auth"))
}

/// Result of stamping a `request_id` onto an outgoing JSON message.
struct StampedMessage {
    text: String,
    request_id: String,
}

/// Replace (or insert) `request_id` in a JSON message with a bridge-assigned correlation ID.
///
/// Uses `serde_json::Value` to properly handle messages that already contain a `request_id`
/// from the downstream client, avoiding duplicate keys.
fn stamp_request_id(counter: &mut u64, json: &str) -> StampedMessage {
    *counter += 1;
    let rid = counter.to_string();

    if let Ok(mut value) = serde_json::from_str::<serde_json::Value>(json) {
        if let Some(obj) = value.as_object_mut() {
            obj.insert("request_id".to_owned(), serde_json::Value::String(rid.clone()));
        }
        let text = serde_json::to_string(&value).unwrap_or_else(|_| json.to_owned());
        return StampedMessage { text, request_id: rid };
    }

    // Fallback: string splice for non-JSON (shouldn't happen with well-formed messages).
    let text = if let Some(rest) = json.strip_prefix('{') {
        format!("{{\"request_id\":\"{rid}\",{rest}")
    } else {
        format!("{{\"request_id\":\"{rid}\",\"_raw\":{json}}}")
    };
    StampedMessage { text, request_id: rid }
}

/// Remove the `request_id` field from a JSON message.
///
/// Used when forwarding a response to a client that sent the original message without
/// a `request_id` (fire-and-forget). Stripping the bridge-assigned ID ensures the client
/// sees the response as a streaming event rather than an RPC response.
fn restore_request_id(json: &str, client_request_id: Option<&str>) -> Arc<str> {
    if let Ok(mut value) = serde_json::from_str::<serde_json::Value>(json) {
        if let Some(obj) = value.as_object_mut() {
            match client_request_id {
                Some(request_id) => {
                    obj.insert(
                        "request_id".to_owned(),
                        serde_json::Value::String(request_id.to_owned()),
                    );
                }
                None => {
                    obj.remove("request_id");
                }
            }
        }
        let restored = serde_json::to_string(&value).unwrap_or_else(|_| json.to_owned());
        return Arc::from(restored);
    }
    Arc::from(json)
}

async fn notify_pending_failures(
    clients: &RwLock<HashMap<ClientId, ClientSlot>>,
    requests: Vec<PendingRequest>,
    code: &str,
    message: &str,
) {
    if requests.is_empty() {
        return;
    }
    let guard = clients.read().await;
    for request in requests {
        if let Some(slot) = guard.get(&request.client_id) {
            let error = pending_error_message(request.client_request_id.as_deref(), code, message);
            let _ = slot.tx.try_send(error);
        }
    }
}

async fn notify_client_failure(
    clients: &RwLock<HashMap<ClientId, ClientSlot>>,
    client_id: ClientId,
    request_id: Option<&str>,
    code: &str,
    message: &str,
) {
    let guard = clients.read().await;
    if let Some(slot) = guard.get(&client_id) {
        let _ = slot.tx.try_send(pending_error_message(request_id, code, message));
    }
}

fn pending_error_message(request_id: Option<&str>, code: &str, message: &str) -> Arc<str> {
    let mut value = serde_json::json!({
        "event": "error",
        "code": code,
        "message": message,
    });
    if let (Some(request_id), Some(object)) = (request_id, value.as_object_mut()) {
        object.insert("request_id".to_owned(), serde_json::Value::String(request_id.to_owned()));
    }
    Arc::from(value.to_string())
}

#[cfg(test)]
#[path = "bridge_tests.rs"]
mod tests;

/// Build the upstream WebSocket URL from an HTTP base URL.
fn build_ws_url(base_url: &str, auth_token: Option<&str>, subscribe: &str) -> String {
    let ws_base = if base_url.starts_with("https://") {
        base_url.replacen("https://", "wss://", 1)
    } else {
        base_url.replacen("http://", "ws://", 1)
    };

    let mut url = format!("{ws_base}/ws?subscribe={subscribe}");
    if let Some(token) = auth_token {
        url.push_str(&format!("&token={token}"));
    }
    url
}
