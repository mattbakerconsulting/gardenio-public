// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Slow-poll pre-warmer for recently registered sessions.
//!
//! Maintains an LRU of up to `capacity` session IDs. Every `interval`,
//! iterates the LRU and polls screen+status for sessions that don't
//! have active watchers (i.e., no dashboard subscriber running a fast poller).

use std::sync::Arc;
use std::time::Duration;

use indexmap::IndexMap;
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;

use crate::state::{epoch_ms, CachedScreen, CachedStatus, MuxState};
use crate::upstream::client::UpstreamClient;

/// Bounded per-request timeout for the screen/status GETs the pre-warm loop
/// issues against each prewarmed session's own coop pod. The shared
/// `reqwest::Client` (`state.http_client`) carries no default timeout, so
/// without this a poll of a hung/parked pod (TCP accepted but never responds —
/// parked/half-open/node under memory pressure) would block forever. Because
/// `spawn_prewarm_task` polls sessions SERIALLY in a single long-lived loop
/// (`for entry in to_poll { client.get_screen().await; client.get_status().await }`),
/// one such pod would freeze the entire loop — the task never returns to
/// `tick.tick()` and every OTHER prewarmed session's cached screen/status goes
/// permanently stale until coopmux restarts. A bounded timeout turns a hung pod
/// into a skipped poll (the `Ok(..)` arm just doesn't run) while the loop
/// advances. Mirrors the health poller (`HEALTH_CHECK_TIMEOUT`), the reassign
/// path (`REASSIGN_REQUEST_TIMEOUT`), and the distributor push
/// (`PUSH_REQUEST_TIMEOUT`, ka-0000002323). See ka-0000003032.
const PREWARM_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

/// Bounded LRU of session IDs for pre-warming.
///
/// Uses `IndexMap` for O(1) insert/remove with insertion-order iteration.
/// Most-recently-touched entries are at the back; eviction pops from the front.
pub struct PrewarmCache {
    map: IndexMap<String, ()>,
    capacity: usize,
}

impl PrewarmCache {
    pub fn new(capacity: usize) -> Self {
        Self { map: IndexMap::with_capacity(capacity), capacity }
    }

    /// Bump a session to the back (most-recently-used).
    pub fn touch(&mut self, session_id: &str) {
        // Remove + reinsert to move to back.
        self.map.shift_remove(session_id);
        self.map.insert(session_id.to_owned(), ());
        // Evict oldest if over capacity.
        while self.map.len() > self.capacity {
            self.map.shift_remove_index(0);
        }
    }

    /// Remove a session from the cache.
    pub fn remove(&mut self, session_id: &str) {
        self.map.shift_remove(session_id);
    }

    /// Return all session IDs in LRU order (oldest first).
    pub fn session_ids(&self) -> Vec<String> {
        self.map.keys().cloned().collect()
    }
}

/// Spawn the pre-warm polling task.
///
/// Every `interval`, iterates the LRU and polls screen+status for sessions
/// that don't have an active watcher (fast poller).
pub fn spawn_prewarm_task(
    state: Arc<MuxState>,
    prewarm: Arc<Mutex<PrewarmCache>>,
    interval: std::time::Duration,
    shutdown: CancellationToken,
) {
    spawn_prewarm_task_with_timeout(state, prewarm, interval, PREWARM_REQUEST_TIMEOUT, shutdown);
}

/// Internal variant of [`spawn_prewarm_task`] that takes the per-request
/// timeout explicitly. Kept private so production always uses the tuned
/// [`PREWARM_REQUEST_TIMEOUT`]; tests inject a short timeout to prove a hung
/// session's poll is bounded and the serial loop advances to the rest.
fn spawn_prewarm_task_with_timeout(
    state: Arc<MuxState>,
    prewarm: Arc<Mutex<PrewarmCache>>,
    interval: std::time::Duration,
    request_timeout: Duration,
    shutdown: CancellationToken,
) {
    tokio::spawn(async move {
        let mut tick = tokio::time::interval(interval);
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

        // Per-session client wrappers reuse the shared `reqwest::Client` from
        // `MuxState`, so this map holds only small `(base_url, auth_token)`
        // wrappers — no per-session connection pools.
        let mut clients: std::collections::HashMap<String, UpstreamClient> =
            std::collections::HashMap::new();

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                _ = tick.tick() => {}
            }

            let ids = prewarm.lock().await.session_ids();
            if ids.is_empty() {
                clients.clear();
                continue;
            }

            // Snapshot which sessions have active watchers so we can skip them.
            let watchers = state.feed.watchers.read().await;
            let active: std::collections::HashSet<&str> =
                watchers.keys().map(|s| s.as_str()).collect();

            // Snapshot session entries we need to poll.
            let sessions = state.sessions.read().await;
            let to_poll: Vec<Arc<crate::state::SessionEntry>> = ids
                .iter()
                .filter(|id| !active.contains(id.as_str()))
                .filter_map(|id| sessions.get(id).cloned())
                .collect();
            drop(sessions);
            drop(watchers);

            // Evict cached clients for sessions no longer in the prewarm set.
            clients.retain(|id, _| ids.iter().any(|s| s == id));

            for entry in to_poll {
                let client = clients.entry(entry.id.clone()).or_insert_with(|| {
                    UpstreamClient::with_timeout(
                        state.http_client.clone(),
                        entry.url.clone(),
                        entry.auth_token.clone(),
                        request_timeout,
                    )
                });

                // Poll screen.
                if let Ok(value) = client.get_screen().await {
                    let lines: Vec<String> = value
                        .get("lines")
                        .and_then(|v| serde_json::from_value(v.clone()).ok())
                        .unwrap_or_default();
                    let ansi: Vec<String> = value
                        .get("ansi")
                        .and_then(|v| serde_json::from_value(v.clone()).ok())
                        .unwrap_or_default();
                    let screen = CachedScreen {
                        lines,
                        ansi,
                        cols: value.get("cols").and_then(|v| v.as_u64()).unwrap_or(80) as u16,
                        rows: value.get("rows").and_then(|v| v.as_u64()).unwrap_or(24) as u16,
                        alt_screen: value
                            .get("alt_screen")
                            .and_then(|v| v.as_bool())
                            .unwrap_or(false),
                        seq: value.get("seq").and_then(|v| v.as_u64()).unwrap_or(0),
                        fetched_at: epoch_ms(),
                    };
                    *entry.cached_screen.write().await = Some(screen);
                }

                // Poll status.
                if let Ok(value) = client.get_status().await {
                    let status = CachedStatus {
                        session_id: value
                            .get("session_id")
                            .and_then(|v| v.as_str())
                            .unwrap_or_default()
                            .to_owned(),
                        state: value
                            .get("state")
                            .and_then(|v| v.as_str())
                            .unwrap_or("unknown")
                            .to_owned(),
                        pid: value.get("pid").and_then(|v| v.as_i64()).map(|v| v as i32),
                        uptime_secs: value.get("uptime_secs").and_then(|v| v.as_i64()).unwrap_or(0),
                        exit_code: value
                            .get("exit_code")
                            .and_then(|v| v.as_i64())
                            .map(|v| v as i32),
                        screen_seq: value.get("screen_seq").and_then(|v| v.as_u64()).unwrap_or(0),
                        bytes_read: value.get("bytes_read").and_then(|v| v.as_u64()).unwrap_or(0),
                        bytes_written: value
                            .get("bytes_written")
                            .and_then(|v| v.as_u64())
                            .unwrap_or(0),
                        ws_clients: value.get("ws_clients").and_then(|v| v.as_i64()).unwrap_or(0)
                            as i32,
                        fetched_at: epoch_ms(),
                    };
                    *entry.cached_status.write().await = Some(status);
                }
            }
        }
    });
}

#[cfg(test)]
#[path = "prewarm_tests.rs"]
mod tests;
