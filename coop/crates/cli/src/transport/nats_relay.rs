// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! NATS relay publisher — broadcasts session-scoped events for coopmux auto-discovery.
//!
//! When `COOP_NATS_RELAY=1`, this module publishes:
//! - `{prefix}.session.{sid}.announce` — online/offline lifecycle + heartbeat (30s)
//! - `{prefix}.session.{sid}.status` — process status (every 2s)
//! - `{prefix}.session.{sid}.state` — agent state transitions
//!
//! Coopmux subscribes to `{prefix}.session.>` for auto-discovery.
//! Screen data is NOT published over NATS — coopmux polls it via HTTP from sessions
//! that include a `url` in their announce payload.

use std::sync::Arc;

use bytes::Bytes;
use futures_util::StreamExt;
use serde_json::Value;
use tokio::sync::broadcast;
use tokio_util::sync::CancellationToken;

use crate::event::InputEvent;
use crate::transport::handler::TransportQuestionAnswer;
use crate::transport::nats::{build_connect_options, NatsAuth};
use crate::transport::state::Store;
use crate::transport::ws::transition_to_msg;

/// NATS relay publisher for session-scoped coopmux discovery.
pub struct NatsRelay {
    client: async_nats::Client,
    prefix: String,
    metadata: Value,
}

impl NatsRelay {
    /// Connect to the NATS server at `url` with optional authentication.
    ///
    /// `retry_on_initial_connect` (boot path only) returns a live client
    /// immediately and establishes the connection in the background. The relay
    /// is the ONLY presence path for outbound-only runtimes (MicroVM
    /// satellites), and their `COOP_NATS_URL` rides a Ziti proxy circuit that
    /// warms up lazily — without background retry a boot connect that loses the
    /// race to the not-yet-open circuit leaves the session silently off the mux
    /// for its whole life (ka-0000008143). Left false on thaw (fail-closed).
    pub async fn connect(
        url: &str,
        prefix: &str,
        agent: &str,
        labels: &[String],
        auth: NatsAuth,
        connect_timeout: Option<std::time::Duration>,
        retry_on_initial_connect: bool,
    ) -> anyhow::Result<Self> {
        let opts = build_connect_options(auth, connect_timeout, retry_on_initial_connect).await?;
        let client = opts.connect(url).await?;
        let metadata = crate::mux_client::detect_metadata(agent, labels);
        Ok(Self { client, prefix: prefix.to_owned(), metadata })
    }

    /// Return a clone of the underlying NATS client for the subscriber.
    pub fn client(&self) -> async_nats::Client {
        self.client.clone()
    }

    /// Return the prefix used for session-scoped subjects.
    pub fn prefix(&self) -> &str {
        &self.prefix
    }

    /// Run the relay publisher until shutdown.
    ///
    /// Spawns concurrent tasks for announce heartbeat, status updates, and state transitions.
    pub async fn run(self, store: Arc<Store>, shutdown: CancellationToken) {
        let relay = Arc::new(self);

        // Announce: online at startup, heartbeat every 30s, offline at shutdown.
        let r = Arc::clone(&relay);
        let s = Arc::clone(&store);
        let sd = shutdown.clone();
        let announce_handle = tokio::spawn(async move {
            r.announce_loop(&s, sd).await;
        });

        // Status: publish every 2s.
        let r = Arc::clone(&relay);
        let s = Arc::clone(&store);
        let sd = shutdown.clone();
        let status_handle = tokio::spawn(async move {
            r.status_loop(&s, sd).await;
        });

        // State: forward transitions from broadcast channel.
        let r = Arc::clone(&relay);
        let s = Arc::clone(&store);
        let sd = shutdown.clone();
        let state_handle = tokio::spawn(async move {
            r.state_loop(&s, sd).await;
        });

        // Usage: publish cost + token data every 5s.
        let r = Arc::clone(&relay);
        let s = Arc::clone(&store);
        let sd = shutdown.clone();
        let usage_handle = tokio::spawn(async move {
            r.usage_loop(&s, sd).await;
        });

        // Wait for shutdown, then send offline announce.
        shutdown.cancelled().await;
        relay.publish_announce(&store, "offline").await;

        // Wait for all tasks to finish.
        let _ = announce_handle.await;
        let _ = status_handle.await;
        let _ = state_handle.await;
        let _ = usage_handle.await;
    }

    /// Announce loop: online event at startup, heartbeat every 30s.
    async fn announce_loop(&self, store: &Store, shutdown: CancellationToken) {
        self.publish_announce(store, "online").await;

        let mut interval = tokio::time::interval(std::time::Duration::from_secs(30));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        // Skip the first immediate tick (we already published online).
        interval.tick().await;

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                _ = interval.tick() => {
                    self.publish_announce(store, "heartbeat").await;
                }
            }
        }
    }

    /// Publish an announce event (online, heartbeat, offline).
    async fn publish_announce(&self, store: &Store, event: &str) {
        let session_id = store.session_id.read().await.clone();
        let subject = format!("{}.session.{session_id}.announce", self.prefix);

        let mut obj = serde_json::Map::new();
        obj.insert("event".to_owned(), Value::String(event.to_owned()));
        obj.insert("session_id".to_owned(), Value::String(session_id));
        obj.insert("ts".to_owned(), Value::Number(epoch_ms().into()));

        // Inject URL from COOP_URL env var so coopmux can HTTP-poll this session.
        // Outbound-only runtimes (COOP_OUTBOUND_ONLY) have no address coopmux can
        // dial back, so they advertise no callback URL — coopmux drives them
        // purely over NATS (status/state/input) and never HTTP-polls them.
        if !crate::mux_client::outbound_only() {
            if let Ok(url) = std::env::var("COOP_URL") {
                obj.insert("url".to_owned(), Value::String(url));
            }
        }

        // Include active profile name so coopmux can display it without a
        // credential broker.
        if let Some(account) = store.profile.active_name().await {
            if !account.is_empty() {
                obj.insert("account_name".to_owned(), Value::String(account));
            }
        }

        // Inject metadata (agent type, labels, k8s).
        if let Value::Object(ref meta) = self.metadata {
            for (k, v) in meta {
                obj.insert(k.clone(), v.clone());
            }
        }

        self.publish(&subject, &obj).await;
    }

    /// Status loop: publish status every 2s.
    async fn status_loop(&self, store: &Store, shutdown: CancellationToken) {
        let mut interval = tokio::time::interval(std::time::Duration::from_secs(2));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                _ = interval.tick() => {
                    let status = crate::transport::handler::compute_status(store).await;
                    let session_id = store.session_id.read().await.clone();
                    let subject = format!("{}.session.{session_id}.status", self.prefix);

                    if let Ok(Value::Object(map)) = serde_json::to_value(&status) {
                        self.publish(&subject, &map).await;
                    }
                }
            }
        }
    }

    /// Usage loop: publish cost + token data every 5s.
    async fn usage_loop(&self, store: &Store, shutdown: CancellationToken) {
        let mut interval = tokio::time::interval(std::time::Duration::from_secs(5));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                _ = interval.tick() => {
                    let snap = store.usage.snapshot().await;
                    let session_id = store.session_id.read().await.clone();
                    let subject = format!("{}.session.{session_id}.usage", self.prefix);

                    let mut obj = serde_json::Map::new();
                    obj.insert("input_tokens".to_owned(), Value::Number(snap.input_tokens.into()));
                    obj.insert("output_tokens".to_owned(), Value::Number(snap.output_tokens.into()));
                    obj.insert("request_count".to_owned(), Value::Number(snap.request_count.into()));
                    self.publish(&subject, &obj).await;
                }
            }
        }
    }

    /// State loop: forward state transitions from broadcast channel.
    async fn state_loop(&self, store: &Store, shutdown: CancellationToken) {
        let mut state_rx = store.channels.state_tx.subscribe();

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                event = state_rx.recv() => {
                    match event {
                        Ok(e) => {
                            let session_id = store.session_id.read().await.clone();
                            let subject = format!("{}.session.{session_id}.state", self.prefix);
                            let msg = transition_to_msg(&e);
                            if let Ok(Value::Object(mut map)) = serde_json::to_value(&msg) {
                                map.insert("session_id".to_owned(), Value::String(session_id));
                                if let Value::Object(ref meta) = self.metadata {
                                    for (k, v) in meta {
                                        map.insert(k.clone(), v.clone());
                                    }
                                }
                                self.publish(&subject, &map).await;
                            }
                        }
                        Err(broadcast::error::RecvError::Lagged(n)) => {
                            tracing::debug!("nats-relay: state subscriber lagged by {n}");
                        }
                        Err(broadcast::error::RecvError::Closed) => break,
                    }
                }
            }
        }
    }

    /// Publish a JSON object to a NATS subject.
    async fn publish(&self, subject: &str, obj: &serde_json::Map<String, Value>) {
        let payload = match serde_json::to_vec(obj) {
            Ok(p) => p,
            Err(e) => {
                tracing::warn!("nats-relay: failed to serialize for {subject}: {e}");
                return;
            }
        };
        if let Err(e) = self.client.publish(subject.to_owned(), payload.into()).await {
            tracing::warn!("nats-relay: publish to {subject} failed: {e}");
        }
    }
}

/// NATS relay subscriber — receives input, nudge, and respond commands from coopmux.
pub struct NatsRelaySubscriber {
    client: async_nats::Client,
    prefix: String,
}

impl NatsRelaySubscriber {
    /// Create a subscriber reusing an existing NATS client.
    pub fn new(client: async_nats::Client, prefix: String) -> Self {
        Self { client, prefix }
    }

    /// Subscribe to input subjects and forward commands to the coop session.
    pub async fn run(self, store: Arc<Store>, shutdown: CancellationToken) {
        let session_id = store.session_id.read().await.clone();

        let input_subject = format!("{}.session.{session_id}.input", self.prefix);
        let nudge_subject = format!("{}.session.{session_id}.nudge", self.prefix);
        let respond_subject = format!("{}.session.{session_id}.respond", self.prefix);

        let mut input_sub = match self.client.subscribe(input_subject.clone()).await {
            Ok(s) => s,
            Err(e) => {
                tracing::warn!("nats-relay: failed to subscribe to {input_subject}: {e}");
                return;
            }
        };
        let mut nudge_sub = match self.client.subscribe(nudge_subject.clone()).await {
            Ok(s) => s,
            Err(e) => {
                tracing::warn!("nats-relay: failed to subscribe to {nudge_subject}: {e}");
                return;
            }
        };
        let mut respond_sub = match self.client.subscribe(respond_subject.clone()).await {
            Ok(s) => s,
            Err(e) => {
                tracing::warn!("nats-relay: failed to subscribe to {respond_subject}: {e}");
                return;
            }
        };

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                msg = input_sub.next() => {
                    let Some(msg) = msg else { break };
                    handle_input(&store, &msg.payload).await;
                }
                msg = nudge_sub.next() => {
                    let Some(msg) = msg else { break };
                    handle_nudge(&store, &msg.payload).await;
                }
                msg = respond_sub.next() => {
                    let Some(msg) = msg else { break };
                    handle_respond(&store, &msg.payload).await;
                }
            }
        }
    }
}

/// Handle keyboard input from dashboard via NATS.
async fn handle_input(store: &Store, payload: &[u8]) {
    #[derive(serde::Deserialize)]
    struct InputMsg {
        text: String,
        #[serde(default)]
        enter: bool,
    }

    let msg: InputMsg = match serde_json::from_slice(payload) {
        Ok(m) => m,
        Err(e) => {
            tracing::debug!("nats-relay: invalid input message: {e}");
            return;
        }
    };

    let mut data = msg.text.into_bytes();
    if msg.enter {
        data.push(b'\r');
    }
    let _ = store.channels.input_tx.send(InputEvent::Write(Bytes::from(data))).await;
}

/// Handle nudge command from dashboard via NATS.
async fn handle_nudge(store: &Store, payload: &[u8]) {
    #[derive(serde::Deserialize)]
    struct NudgeMsg {
        #[serde(default)]
        message: String,
    }

    let msg: NudgeMsg = match serde_json::from_slice(payload) {
        Ok(m) => m,
        Err(_) => NudgeMsg { message: String::new() },
    };

    let message = if msg.message.is_empty() { "continue" } else { &msg.message };
    let _ = crate::transport::handler::handle_nudge(store, message).await;
}

/// Handle respond command from dashboard via NATS.
async fn handle_respond(store: &Store, payload: &[u8]) {
    #[derive(serde::Deserialize)]
    struct RespondMsg {
        accept: Option<bool>,
        option: Option<i32>,
        text: Option<String>,
        #[serde(default)]
        answers: Vec<TransportQuestionAnswer>,
    }

    let msg: RespondMsg = match serde_json::from_slice(payload) {
        Ok(m) => m,
        Err(e) => {
            tracing::debug!("nats-relay: invalid respond message: {e}");
            return;
        }
    };

    let _ = crate::transport::handler::handle_respond(
        store,
        msg.accept,
        msg.option,
        msg.text.as_deref(),
        &msg.answers,
    )
    .await;
}

/// Return current epoch millis.
fn epoch_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

#[cfg(test)]
#[path = "nats_relay_tests.rs"]
mod tests;
