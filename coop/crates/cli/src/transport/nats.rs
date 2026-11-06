// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! NATS event publisher — broadcasts coop events to NATS subjects.

use std::path::Path;
use std::time::Duration;

use serde_json::Value;
use tokio::sync::broadcast;
use tokio_util::sync::CancellationToken;

use crate::driver::{classify_error_detail, AgentState, ErrorCategory};
use crate::event::TransitionEvent;
use crate::transport::ws::{
    profile_event_to_msg, start_event_to_msg, stop_event_to_msg, transition_to_msg,
    usage_event_to_msg, ServerMessage,
};
use crate::transport::Store;

/// Authentication options for connecting to NATS.
#[derive(Debug, Default)]
pub struct NatsAuth {
    pub token: Option<String>,
    pub user: Option<String>,
    pub password: Option<String>,
    pub creds_path: Option<Box<Path>>,
}

/// Publishes coop events to NATS subjects as JSON.
///
/// Each published message is a JSON object with the `ServerMessage` fields
/// plus injected identity fields:
/// - `session_id` — current agent session ID (tracks switches)
/// - `agent` — agent type (e.g. "claude", "gemini")
/// - `k8s` — optional Kubernetes pod metadata (when running in K8s)
/// - Any `--label` CLI flags as metadata keys.
pub struct NatsPublisher {
    client: async_nats::Client,
    prefix: String,
    /// Static session metadata detected at construction time.
    metadata: Value,
}

impl NatsPublisher {
    /// Connect to the NATS server at `url` with optional authentication.
    ///
    /// `connect_timeout` caps a single connection attempt's full handshake
    /// (TCP/TLS + server INFO + CONNECT/PONG). On the thaw reconnect path the
    /// supervisor passes a short cap so a SYN black-holed on a not-yet-routable
    /// post-restore CNI netns returns quickly and the bounded retry loop can
    /// re-probe often (bug ka-0000001492); `None` keeps async-nats's 5 s default
    /// for the warm boot path.
    ///
    /// `retry_on_initial_connect` (boot path only) returns a live client
    /// immediately and connects in the background, so an outbound-only runtime
    /// whose overlay NATS circuit warms up lazily self-heals instead of losing
    /// its one-shot boot connect (ka-0000008143). See `build_connect_options`.
    pub async fn connect(
        url: &str,
        prefix: &str,
        agent: &str,
        labels: &[String],
        auth: NatsAuth,
        connect_timeout: Option<Duration>,
        retry_on_initial_connect: bool,
    ) -> anyhow::Result<Self> {
        let opts = build_connect_options(auth, connect_timeout, retry_on_initial_connect).await?;
        let client = opts.connect(url).await?;
        let metadata = crate::mux_client::detect_metadata(agent, labels);
        Ok(Self { client, prefix: prefix.to_owned(), metadata })
    }

    /// Subscribe to all broadcast channels and publish events until shutdown.
    pub async fn run(self, store: &Store, shutdown: CancellationToken) {
        let mut state_rx = store.channels.state_tx.subscribe();
        let mut prompt_rx = store.channels.prompt_tx.subscribe();
        let mut hook_rx = store.channels.hook_tx.subscribe();
        let mut stop_rx = store.stop.stop_tx.subscribe();
        let mut start_rx = store.start.start_tx.subscribe();
        let mut usage_rx = store.usage.usage_tx.subscribe();
        let mut profile_rx = store.profile.profile_tx.subscribe();

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                event = state_rx.recv() => {
                    if let Ok(ref te) = event {
                        self.maybe_publish_auth_exhausted(store, te).await;
                        self.maybe_publish_terminal_auth_failure(store, te).await;
                        self.maybe_publish_account_exhausted(store, te).await;
                    }
                    self.handle_with(store, event, &format!("{}.state", self.prefix), |e| {
                        transition_to_msg(&e)
                    }).await;
                }
                event = prompt_rx.recv() => {
                    self.handle_with(store, event, &format!("{}.prompt", self.prefix), |e| {
                        ServerMessage::PromptOutcome {
                            source: e.source,
                            r#type: e.r#type,
                            subtype: e.subtype,
                            option: e.option,
                        }
                    }).await;
                }
                event = hook_rx.recv() => {
                    self.handle_with(store, event, &format!("{}.hook", self.prefix), |e| {
                        ServerMessage::HookRaw { data: e.json }
                    }).await;
                }
                event = stop_rx.recv() => {
                    self.handle_with(store, event, &format!("{}.stop", self.prefix), |e| {
                        stop_event_to_msg(&e)
                    }).await;
                }
                event = start_rx.recv() => {
                    self.handle_with(store, event, &format!("{}.start", self.prefix), |e| {
                        start_event_to_msg(&e)
                    }).await;
                }
                event = usage_rx.recv() => {
                    self.handle_with(store, event, &format!("{}.usage", self.prefix), |e| {
                        usage_event_to_msg(&e)
                    }).await;
                }
                event = profile_rx.recv() => {
                    self.handle_with(store, event, &format!("{}.profile", self.prefix), |e| {
                        profile_event_to_msg(&e)
                    }).await;
                }
            }
        }
    }

    /// Publish a dedicated `{prefix}.auth.{event}` event when a state
    /// transition carries a `BudgetExceeded` or `RateLimited` error category.
    ///
    /// Both categories indicate that the current account's credentials are
    /// exhausted and the controller should trigger auth failover.
    ///
    /// The event payload includes env-sourced metadata (agent, project, account)
    /// so the controller can route failover without additional lookups.
    async fn maybe_publish_auth_exhausted(&self, store: &Store, event: &TransitionEvent) {
        // Check explicit Error state first.
        let detail = match &event.next {
            AgentState::Error { detail } => detail.clone(),
            // Claude Code sometimes reports rate limits inline: the agent goes
            // working → idle with the error in `last_message` instead of
            // transitioning to Error state. Catch that case here.
            AgentState::Idle if matches!(event.prev, AgentState::Working) => {
                match event.last_message {
                    Some(ref msg) if classify_error_detail(msg).is_auth_exhaustion() => msg.clone(),
                    _ => return,
                }
            }
            _ => return,
        };
        let category = classify_error_detail(&detail);
        let event_name = match category {
            ErrorCategory::BudgetExceeded => "budget_exceeded",
            ErrorCategory::RateLimited => "rate_limited",
            _ => return,
        };

        let session_id = store.session_id.read().await.clone();
        let account = store.profile.active_name().await.unwrap_or_default();
        let payload = serde_json::json!({
            "event": event_name,
            "session_id": session_id,
            "detail": &detail,
            "agent": std::env::var("BOAT_AGENT").unwrap_or_default(),
            "project": std::env::var("BOAT_PROJECT").unwrap_or_default(),
            "account": account,
        });

        // Inject session metadata (agent type, labels, k8s).
        let mut obj = match serde_json::to_value(&payload) {
            Ok(Value::Object(map)) => map,
            _ => return,
        };
        if let Value::Object(ref meta) = self.metadata {
            for (k, v) in meta {
                obj.entry(k.clone()).or_insert_with(|| v.clone());
            }
        }

        // Publish to the canonical auth subject. Use budget_exceeded for both
        // categories so existing NATS subscribers (controller, monitoring) pick
        // it up without changes.
        let subject = format!("{}.auth.budget_exceeded", self.prefix);
        let bytes = match serde_json::to_vec(&obj) {
            Ok(b) => b,
            Err(e) => {
                tracing::warn!("nats: auth_exhausted serialize failed: {e}");
                return;
            }
        };
        if let Err(e) = self.client.publish(subject.clone(), bytes.into()).await {
            tracing::warn!("nats: publish to {subject} failed: {e}");
        }
    }

    /// Publish a DIAGNOSTIC-only `{prefix}.auth.terminal_failure` event when a
    /// state transition carries a **terminal, non-failover** credential error
    /// (`Unauthorized` or `OutOfCredits`; see
    /// [`ErrorCategory::is_terminal_auth_failure`]).
    ///
    /// Motivation (ka-0000003962): [`maybe_publish_auth_exhausted`] maps only
    /// `BudgetExceeded` / `RateLimited` to an event and `return`s for every
    /// other category, so when an agent's FIRST inference turn fails with a
    /// terminal auth error — e.g. a `401 key not allowed to access model` — NO
    /// NATS event is ever published and nothing is written to the agent bead.
    /// The real error string then lives only in a live poll of
    /// `/api/v1/agent`, which is gone the moment the pod restarts; the
    /// 2026-07-25 opus-5 outage ran ~4.5h fleet-wide with `total_errors=0` on
    /// every affected bead as a result.
    ///
    /// This is a SEPARATE subject from `{prefix}.auth.budget_exceeded` on
    /// purpose: the controller's account-exhaustion listener treats
    /// `budget_exceeded` as a failover trigger, and a terminal auth failure
    /// must NEVER trigger failover (rotating credentials cannot fix a key ACL
    /// or an out-of-credits account — it would cold-restart the pod forever;
    /// see the `is_auth_exhaustion` note in error_category.rs and
    /// ka-0000001718). The controller subscribes to this subject purely to
    /// persist `error_category` + `error_detail` onto the agent bead — a
    /// durable diagnostic record, no behavior change.
    async fn maybe_publish_terminal_auth_failure(&self, store: &Store, event: &TransitionEvent) {
        // Same detection shape as maybe_publish_auth_exhausted: an explicit
        // Error state, or the inline Working→Idle case where Claude Code
        // surfaces the error in `last_message` without a distinct Error state.
        let detail = match &event.next {
            AgentState::Error { detail } => detail.clone(),
            AgentState::Idle if matches!(event.prev, AgentState::Working) => {
                match event.last_message {
                    Some(ref msg) if classify_error_detail(msg).is_terminal_auth_failure() => {
                        msg.clone()
                    }
                    _ => return,
                }
            }
            _ => return,
        };
        let category = classify_error_detail(&detail);
        if !category.is_terminal_auth_failure() {
            return;
        }

        let session_id = store.session_id.read().await.clone();
        let account = store.profile.active_name().await.unwrap_or_default();
        let payload = serde_json::json!({
            "event": "terminal_auth_failure",
            "error_category": category.as_str(),
            "session_id": session_id,
            "detail": &detail,
            "agent": std::env::var("BOAT_AGENT").unwrap_or_default(),
            "project": std::env::var("BOAT_PROJECT").unwrap_or_default(),
            "account": account,
        });

        // Inject session metadata (agent type, labels, k8s, agent_bead_id).
        let mut obj = match serde_json::to_value(&payload) {
            Ok(Value::Object(map)) => map,
            _ => return,
        };
        if let Value::Object(ref meta) = self.metadata {
            for (k, v) in meta {
                obj.entry(k.clone()).or_insert_with(|| v.clone());
            }
        }

        let subject = format!("{}.auth.terminal_failure", self.prefix);
        let bytes = match serde_json::to_vec(&obj) {
            Ok(b) => b,
            Err(e) => {
                tracing::warn!("nats: terminal_auth_failure serialize failed: {e}");
                return;
            }
        };
        if let Err(e) = self.client.publish(subject.clone(), bytes.into()).await {
            tracing::warn!("nats: publish to {subject} failed: {e}");
        }
    }

    /// Publish a dedicated `{prefix}.account.exhausted` event when the agent
    /// transitions to `Parked` state due to all profiles being rate-limited.
    ///
    /// This fires when profile rotation has been attempted and all options are
    /// depleted — the controller can use this to identify which account/project
    /// is affected and take remedial action (e.g., credential rotation, alerts).
    async fn maybe_publish_account_exhausted(&self, store: &Store, event: &TransitionEvent) {
        let (reason, resume_at_epoch_ms) = match &event.next {
            AgentState::Parked { reason, resume_at_epoch_ms } => (reason, resume_at_epoch_ms),
            _ => return,
        };

        let session_id = store.session_id.read().await.clone();
        let account = store.profile.active_name().await.unwrap_or_default();
        let payload = serde_json::json!({
            "event": "account_exhausted",
            "session_id": session_id,
            "parked_reason": reason,
            "resume_at_epoch_ms": resume_at_epoch_ms,
            "agent": std::env::var("BOAT_AGENT").unwrap_or_default(),
            "project": std::env::var("BOAT_PROJECT").unwrap_or_default(),
            "account": account,
        });

        let mut obj = match serde_json::to_value(&payload) {
            Ok(Value::Object(map)) => map,
            _ => return,
        };
        if let Value::Object(ref meta) = self.metadata {
            for (k, v) in meta {
                obj.entry(k.clone()).or_insert_with(|| v.clone());
            }
        }

        let subject = format!("{}.account.exhausted", self.prefix);
        let bytes = match serde_json::to_vec(&obj) {
            Ok(b) => b,
            Err(e) => {
                tracing::warn!("nats: account_exhausted serialize failed: {e}");
                return;
            }
        };
        if let Err(e) = self.client.publish(subject.clone(), bytes.into()).await {
            tracing::warn!("nats: publish to {subject} failed: {e}");
        }
    }

    /// Convert a domain event to a [`ServerMessage`], inject identity fields, and publish.
    async fn handle_with<T, F>(
        &self,
        store: &Store,
        result: Result<T, broadcast::error::RecvError>,
        subject: &str,
        convert: F,
    ) where
        F: FnOnce(T) -> ServerMessage,
    {
        match result {
            Ok(event) => {
                let msg = convert(event);
                let mut obj = match serde_json::to_value(&msg) {
                    Ok(Value::Object(map)) => map,
                    Ok(_) => return, // ServerMessage always serializes to an object
                    Err(e) => {
                        tracing::warn!("nats: failed to serialize event for {subject}: {e}");
                        return;
                    }
                };

                // Inject session identity.
                let session_id = store.session_id.read().await.clone();
                obj.insert("session_id".to_owned(), Value::String(session_id));

                // Inject session metadata (agent type, labels, k8s).
                if let Value::Object(ref meta) = self.metadata {
                    for (k, v) in meta {
                        obj.insert(k.clone(), v.clone());
                    }
                }

                let payload = match serde_json::to_vec(&obj) {
                    Ok(p) => p,
                    Err(e) => {
                        tracing::warn!("nats: failed to serialize event for {subject}: {e}");
                        return;
                    }
                };
                if let Err(e) = self.client.publish(subject.to_owned(), payload.into()).await {
                    tracing::warn!("nats: publish to {subject} failed: {e}");
                }
            }
            Err(broadcast::error::RecvError::Lagged(n)) => {
                tracing::debug!("nats: {subject} subscriber lagged by {n}");
            }
            Err(broadcast::error::RecvError::Closed) => {
                tracing::debug!("nats: {subject} channel closed");
            }
        }
    }
}

/// Build `ConnectOptions` from the auth configuration.
///
/// Priority (first match wins):
/// 1. Credentials file (JWT/NKey — standard NATS 2.0 auth)
/// 2. Username + secret (explicit password, else the token): multi-user
///    server config (nats.auth.subjectIsolation) only supports user/password
///    entries, so when a username is configured the delivered token is sent
///    as that user's PASSWORD — a bare token can never match a users[] entry
///    (ka-0000000002).
/// 3. Token (legacy single-token auth)
/// 4. No auth
///
/// `connect_timeout`, when set, caps a single attempt's full connection
/// establishment + handshake (async-nats's `connection_timeout`; default 5 s).
/// The thaw reconnect path passes a short cap so each attempt fails fast on a
/// not-yet-routable netns and the bounded retry loop re-probes frequently
/// (bug ka-0000001492).
///
/// `retry_on_initial_connect`, when true, makes `connect()` return a live
/// client IMMEDIATELY and establish the TCP/handshake in the background,
/// retrying until the server is reachable rather than erroring on the first
/// miss. The boot path sets this for the *required* transports (publisher,
/// relay) so a MicroVM whose `COOP_NATS_URL` rides a Ziti proxy circuit that
/// warms up LAZILY does not lose its one-shot boot connect and stay silently
/// off the relay for the whole session (ka-0000008143). It is DELIBERATELY left
/// false on the thaw reconnect path: thaw drives its own bounded external retry
/// loop under the fail-closed deadline gate and re-resolves a fresh token per
/// attempt, so `connect()` there MUST be able to fail (async-nats's own
/// background reconnect would otherwise retry with the *stale* token the
/// supervisor is explicitly reconnecting to avoid — see nats_supervisor.rs).
pub(crate) async fn build_connect_options(
    auth: NatsAuth,
    connect_timeout: Option<Duration>,
    retry_on_initial_connect: bool,
) -> anyhow::Result<async_nats::ConnectOptions> {
    let base = if let Some(ref path) = auth.creds_path {
        async_nats::ConnectOptions::with_credentials_file(path).await?
    } else if let Some(user) = auth.user {
        let pass = auth.password.or(auth.token).unwrap_or_default();
        async_nats::ConnectOptions::with_user_and_password(user, pass)
    } else if let Some(token) = auth.token {
        async_nats::ConnectOptions::with_token(token)
    } else {
        async_nats::ConnectOptions::new()
    };
    let base = match connect_timeout {
        Some(t) => base.connection_timeout(t),
        None => base,
    };
    Ok(if retry_on_initial_connect { base.retry_on_initial_connect() } else { base })
}
