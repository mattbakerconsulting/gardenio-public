// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Inbox consumer — subscribes to INBOX_EVENTS JetStream stream and delivers
//! messages to the local agent session via JSONL inject queue + nudge.
//!
//! Part of bd-xtahx.3 (Phase 2: Coop JetStream inbox subscription).

use std::fs::OpenOptions;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use nix::fcntl::Flock;

use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::driver::AgentState;
use crate::transport::handler::handle_nudge;
use crate::transport::nats::NatsAuth;
use crate::transport::Store;

/// An inbox item as published by the beads daemon to JetStream.
/// Mirrors `types.InboxItem` from the Go codebase.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InboxItem {
    pub id: String,
    pub agent_name: String,
    #[serde(default)]
    pub rig: Option<String>,
    #[serde(default)]
    pub session_id: Option<String>,
    #[serde(rename = "type")]
    pub item_type: String,
    #[serde(default)]
    pub source: String,
    pub content: String,
    #[serde(default)]
    pub priority: i32,
    #[serde(default)]
    pub created_at: String,
    #[serde(default)]
    pub delivered_at: Option<String>,
    #[serde(default)]
    pub expires_at: Option<String>,
    #[serde(default)]
    pub dedup_key: String,
}

/// JSONL entry written to the inject queue. Matches the schema in the design doc.
#[derive(Debug, Serialize)]
struct InjectEntry {
    id: String,
    #[serde(rename = "type")]
    item_type: String,
    source: String,
    content: String,
    priority: i32,
    timestamp: u64,
    dedup_key: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    ttl_seconds: Option<u64>,
}

impl From<&InboxItem> for InjectEntry {
    fn from(item: &InboxItem) -> Self {
        // Parse created_at to epoch ms, fallback to current time
        let timestamp = chrono_parse_or_now(&item.created_at);

        // Calculate TTL from expires_at if present
        let ttl_seconds = item.expires_at.as_deref().and_then(|ea| {
            let expires_ms = chrono_parse_or_now(ea);
            if expires_ms > timestamp {
                Some((expires_ms - timestamp) / 1000)
            } else {
                None
            }
        });

        Self {
            id: item.id.clone(),
            item_type: item.item_type.clone(),
            source: item.source.clone(),
            content: item.content.clone(),
            priority: item.priority,
            timestamp,
            dedup_key: item.dedup_key.clone(),
            ttl_seconds,
        }
    }
}

/// Parse an RFC 3339 timestamp string to epoch milliseconds, or return current time.
fn chrono_parse_or_now(s: &str) -> u64 {
    // Try ISO 8601 / RFC 3339 parse
    if let Ok(ts) = s.parse::<chrono_lite::DateTime>() {
        return ts.timestamp_millis() as u64;
    }
    // Fallback: current time
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

/// Subscribes to inbox JetStream subjects and delivers to the local agent.
pub struct InboxConsumer {
    client: async_nats::Client,
    agent_name: String,
    rig_name: Option<String>,
    inject_dir: PathBuf,
}

impl InboxConsumer {
    /// Connect to NATS and create the inbox consumer.
    ///
    /// - `agent_name`: the GT_ROLE or resolved agent identity (e.g., "mayor")
    /// - `rig_name`: optional rig name for rig-scoped messages
    /// - `inject_dir`: path to `.runtime/inject-queue/` directory
    pub async fn connect(
        url: &str,
        agent_name: &str,
        rig_name: Option<&str>,
        inject_dir: &Path,
        auth: NatsAuth,
    ) -> anyhow::Result<Self> {
        // Best-effort transport: single-shot on thaw, so it keeps async-nats's
        // default 5 s connection timeout and no background initial-connect retry.
        let opts = super::nats::build_connect_options(auth, None, false).await?;
        let client = opts.connect(url).await?;
        Ok(Self {
            client,
            agent_name: agent_name.to_owned(),
            rig_name: rig_name.map(|s| s.to_owned()),
            inject_dir: inject_dir.to_owned(),
        })
    }

    /// Run the inbox consumer loop until shutdown.
    ///
    /// 1. Creates a durable JetStream consumer named `inbox-{agent_name}`
    /// 2. Subscribes to `inbox.agent.{agent_name}`, `inbox.rig.{rig}`, `inbox.all`
    /// 3. On receive: write to JSONL with flock, ack, nudge if idle
    /// 4. Every 10 minutes: heartbeat reconciliation via daemon RPC
    pub async fn run(self, store: Arc<Store>, shutdown: CancellationToken) {
        let js = async_nats::jetstream::new(self.client.clone());

        // Build filter subjects for this agent
        let mut subjects = vec![format!("inbox.agent.{}", self.agent_name), "inbox.all".to_owned()];
        if let Some(ref rig) = self.rig_name {
            subjects.push(format!("inbox.rig.{}", rig));
        }

        // Create or bind to durable consumer
        let consumer_name = format!("inbox-{}", self.agent_name);
        let consumer = match self.create_or_get_consumer(&js, &consumer_name, &subjects).await {
            Ok(c) => c,
            Err(e) => {
                tracing::error!("inbox: failed to create JetStream consumer: {e}");
                return;
            }
        };

        // Ensure inject directory exists
        if let Err(e) = std::fs::create_dir_all(&self.inject_dir) {
            tracing::error!("inbox: failed to create inject dir {:?}: {e}", self.inject_dir);
            return;
        }

        let mut heartbeat = tokio::time::interval(Duration::from_secs(600));
        heartbeat.tick().await; // skip first immediate tick

        tracing::info!("inbox: consumer {} listening on {:?}", consumer_name, subjects);

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => {
                    tracing::info!("inbox: shutting down");
                    break;
                }
                batch_result = consumer.fetch().max_messages(10).messages() => {
                    match batch_result {
                        Ok(mut messages) => {
                            use futures_util::StreamExt;
                            while let Some(msg_result) = messages.next().await {
                                match msg_result {
                                    Ok(msg) => {
                                        self.handle_message(&store, msg).await;
                                    }
                                    Err(e) => {
                                        tracing::debug!("inbox: message error: {e}");
                                    }
                                }
                            }
                        }
                        Err(e) => {
                            tracing::debug!("inbox: fetch error: {e}");
                            tokio::time::sleep(Duration::from_secs(1)).await;
                        }
                    }
                }
                _ = heartbeat.tick() => {
                    self.heartbeat_reconcile(&store).await;
                }
            }
        }
    }

    /// Create or get a durable JetStream consumer for inbox delivery.
    async fn create_or_get_consumer(
        &self,
        js: &async_nats::jetstream::Context,
        consumer_name: &str,
        subjects: &[String],
    ) -> anyhow::Result<
        async_nats::jetstream::consumer::Consumer<async_nats::jetstream::consumer::pull::Config>,
    > {
        use async_nats::jetstream::consumer::pull::Config as PullConfig;
        use async_nats::jetstream::consumer::DeliverPolicy;

        let stream = js.get_stream("INBOX_EVENTS").await?;

        let config = PullConfig {
            durable_name: Some(consumer_name.to_owned()),
            filter_subjects: subjects.to_vec(),
            deliver_policy: DeliverPolicy::New,
            ack_wait: Duration::from_secs(30),
            ..Default::default()
        };

        let consumer = stream.get_or_create_consumer(consumer_name, config).await?;

        Ok(consumer)
    }

    /// Handle a single inbox message from JetStream.
    async fn handle_message(&self, store: &Store, msg: async_nats::jetstream::Message) {
        // Parse the inbox item
        let item: InboxItem = match serde_json::from_slice(&msg.payload) {
            Ok(item) => item,
            Err(e) => {
                tracing::warn!("inbox: failed to parse message: {e}");
                // Ack anyway to avoid infinite redelivery of malformed messages
                let _ = msg.ack().await;
                return;
            }
        };

        tracing::info!(
            "inbox: received item {} (type={}, from={}, to={})",
            item.id,
            item.item_type,
            item.source,
            item.agent_name,
        );

        // Resolve session ID for JSONL filename
        let session_id = store.session_id.read().await.clone();
        let jsonl_path = self.inject_dir.join(format!("{session_id}.jsonl"));

        // Write to JSONL with flock
        if let Err(e) = write_inject_entry(&jsonl_path, &item) {
            tracing::error!("inbox: failed to write JSONL entry: {e}");
            // NAK so it gets redelivered
            let _ = msg.ack_with(async_nats::jetstream::AckKind::Nak(None)).await;
            return;
        }

        // Ack AFTER successful write (at-least-once guarantee)
        if let Err(e) = msg.ack().await {
            tracing::warn!("inbox: ack failed: {e}");
        }

        // Nudge agent if idle
        self.maybe_nudge(store, &item).await;
    }

    /// Nudge the agent if it's currently idle, so the next hook cycle picks up
    /// the injected message.
    async fn maybe_nudge(&self, store: &Store, item: &InboxItem) {
        let agent = store.driver.agent_state.read().await;
        if !matches!(&*agent, AgentState::Idle) {
            tracing::debug!("inbox: agent not idle, skipping nudge");
            return;
        }
        drop(agent);

        let nudge_msg = format!(
            "Inbox: {} from {} — {}",
            item.item_type,
            item.source,
            truncate(&item.content, 80),
        );

        match handle_nudge(store, &nudge_msg).await {
            Ok(outcome) => {
                if outcome.delivered {
                    tracing::info!("inbox: nudged agent for item {}", item.id);
                } else {
                    tracing::debug!("inbox: nudge not delivered (agent busy)");
                }
            }
            Err(e) => {
                tracing::debug!("inbox: nudge failed: {e:?}");
            }
        }
    }

    /// Heartbeat reconciliation: run `bd inbox drain --reconcile` to catch
    /// messages missed during NATS downtime or gaps.
    async fn heartbeat_reconcile(&self, _store: &Store) {
        tracing::debug!("inbox: heartbeat reconciliation");
        let result = tokio::process::Command::new("bd")
            .args(["inbox", "drain", "--reconcile", "--json"])
            .output()
            .await;

        match result {
            Ok(output) => {
                if output.status.success() {
                    let stdout = String::from_utf8_lossy(&output.stdout);
                    if !stdout.trim().is_empty() {
                        tracing::info!("inbox: heartbeat drain result: {}", stdout.trim());
                    }
                } else {
                    let stderr = String::from_utf8_lossy(&output.stderr);
                    tracing::warn!("inbox: heartbeat drain failed: {}", stderr.trim());
                }
            }
            Err(e) => {
                tracing::warn!("inbox: heartbeat drain command failed: {e}");
            }
        }
    }
}

/// Write a single JSONL entry to the inject queue file with flock(2) safety.
///
/// Uses O_APPEND|O_CREATE|O_WRONLY and a single write() call per entry.
/// The flock ensures cross-language safety with Go's syscall.Flock.
fn write_inject_entry(path: &Path, item: &InboxItem) -> anyhow::Result<()> {
    let entry = InjectEntry::from(item);
    let mut line = serde_json::to_string(&entry)?;
    line.push('\n');

    let file = OpenOptions::new().create(true).append(true).open(path)?;

    // Acquire exclusive flock (released on drop of Flock guard)
    let mut locked = Flock::lock(file, nix::fcntl::FlockArg::LockExclusive)
        .map_err(|(_file, errno)| anyhow::anyhow!("flock failed: {}", errno))?;

    // Single write call for atomicity with O_APPEND
    locked.write_all(line.as_bytes())?;

    // Flock released on drop
    Ok(())
}

/// Truncate a string to `max` characters, appending "..." if truncated.
fn truncate(s: &str, max: usize) -> String {
    if s.len() <= max {
        s.to_owned()
    } else {
        let end = s.char_indices().nth(max.saturating_sub(3)).map(|(i, _)| i).unwrap_or(max);
        format!("{}...", &s[..end])
    }
}

/// Minimal DateTime parser for RFC 3339 timestamps.
/// Avoids pulling in the full chrono crate.
mod chrono_lite {
    pub struct DateTime {
        millis: i64,
    }

    impl DateTime {
        pub fn timestamp_millis(&self) -> i64 {
            self.millis
        }
    }

    impl std::str::FromStr for DateTime {
        type Err = ();

        fn from_str(s: &str) -> Result<Self, Self::Err> {
            // Parse "2026-02-15T10:30:00Z" or "2026-02-15T10:30:00.123Z" or with offset
            // Minimal parser: extract year, month, day, hour, min, sec, optional fractional
            let s = s.trim();
            if s.len() < 19 {
                return Err(());
            }

            let year: i64 = s[0..4].parse().map_err(|_| ())?;
            let month: i64 = s[5..7].parse().map_err(|_| ())?;
            let day: i64 = s[8..10].parse().map_err(|_| ())?;
            let hour: i64 = s[11..13].parse().map_err(|_| ())?;
            let min: i64 = s[14..16].parse().map_err(|_| ())?;
            let sec: i64 = s[17..19].parse().map_err(|_| ())?;

            // Parse fractional seconds if present
            let mut frac_ms: i64 = 0;
            let mut rest = &s[19..];
            if rest.starts_with('.') {
                rest = &rest[1..];
                let frac_end = rest.find(|c: char| !c.is_ascii_digit()).unwrap_or(rest.len());
                let frac_str = &rest[..frac_end];
                if !frac_str.is_empty() {
                    let padded = format!("{:0<3}", &frac_str[..frac_str.len().min(3)]);
                    frac_ms = padded.parse().unwrap_or(0);
                }
            }

            // Approximate epoch calculation (good enough for timestamp comparison)
            // Days from year 0 to epoch (1970-01-01)
            let days_in_month = [0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
            let mut days: i64 = 0;
            for y in 1970..year {
                days += if is_leap(y) { 366 } else { 365 };
            }
            for m in 1..month {
                days += days_in_month[m as usize];
                if m == 2 && is_leap(year) {
                    days += 1;
                }
            }
            days += day - 1;

            let secs = days * 86400 + hour * 3600 + min * 60 + sec;
            Ok(DateTime { millis: secs * 1000 + frac_ms })
        }
    }

    fn is_leap(y: i64) -> bool {
        (y % 4 == 0 && y % 100 != 0) || y % 400 == 0
    }
}
