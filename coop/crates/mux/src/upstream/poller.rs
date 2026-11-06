// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Background pollers for screen, status, and usage caching.

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use tokio_util::sync::CancellationToken;

use crate::config::MuxConfig;
use crate::state::{epoch_ms, CachedScreen, CachedStatus, CachedUsage, SessionEntry};
use crate::upstream::client::UpstreamClient;

/// Timeout for poller requests. Kept short so dead sessions don't accumulate
/// pending connections — pollers fire frequently (500ms–2s) and a 10s default
/// timeout allows ~20 in-flight timeouts per dead session before the health
/// checker evicts it.
const POLLER_TIMEOUT: Duration = Duration::from_secs(2);

/// Spawn background tasks that poll screen and status for a session.
///
/// The `cancel` token is used to stop the pollers independently of the
/// session entry's own cancel token (which controls registration lifetime).
pub fn spawn_screen_poller(
    entry: Arc<SessionEntry>,
    config: &MuxConfig,
    http_client: reqwest::Client,
    cancel: CancellationToken,
) {
    let screen_interval = config.screen_poll_interval();
    let status_interval = config.status_poll_interval();

    // Screen poller
    {
        let entry = Arc::clone(&entry);
        let cancel = cancel.clone();
        let http = http_client.clone();
        tokio::spawn(async move {
            let client = UpstreamClient::with_timeout(
                http,
                entry.url.clone(),
                entry.auth_token.clone(),
                POLLER_TIMEOUT,
            );
            let mut interval = tokio::time::interval(screen_interval);
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

            loop {
                tokio::select! {
                    _ = cancel.cancelled() => break,
                    _ = interval.tick() => {}
                }

                // Skip polling when the health checker has flagged this session
                // as failing — no point piling up timeout requests against a
                // dead upstream.
                if entry.health_failures.load(Ordering::Relaxed) > 0 {
                    continue;
                }

                match client.get_screen().await {
                    Ok(value) => {
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
                    Err(e) => {
                        tracing::debug!(session_id = %entry.id, err = %e, "screen poll failed");
                    }
                }
            }
        });
    }

    // Status poller
    {
        let entry = Arc::clone(&entry);
        let cancel = cancel.clone();
        let http = http_client.clone();
        tokio::spawn(async move {
            let client = UpstreamClient::with_timeout(
                http,
                entry.url.clone(),
                entry.auth_token.clone(),
                POLLER_TIMEOUT,
            );
            let mut interval = tokio::time::interval(status_interval);
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

            loop {
                tokio::select! {
                    _ = cancel.cancelled() => break,
                    _ = interval.tick() => {}
                }

                // Skip polling when health check is failing.
                if entry.health_failures.load(Ordering::Relaxed) > 0 {
                    continue;
                }

                match client.get_status().await {
                    Ok(value) => {
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
                            uptime_secs: value
                                .get("uptime_secs")
                                .and_then(|v| v.as_i64())
                                .unwrap_or(0),
                            exit_code: value
                                .get("exit_code")
                                .and_then(|v| v.as_i64())
                                .map(|v| v as i32),
                            screen_seq: value
                                .get("screen_seq")
                                .and_then(|v| v.as_u64())
                                .unwrap_or(0),
                            bytes_read: value
                                .get("bytes_read")
                                .and_then(|v| v.as_u64())
                                .unwrap_or(0),
                            bytes_written: value
                                .get("bytes_written")
                                .and_then(|v| v.as_u64())
                                .unwrap_or(0),
                            ws_clients: value
                                .get("ws_clients")
                                .and_then(|v| v.as_i64())
                                .unwrap_or(0) as i32,
                            fetched_at: epoch_ms(),
                        };
                        *entry.cached_status.write().await = Some(status);
                    }
                    Err(e) => {
                        tracing::debug!(session_id = %entry.id, err = %e, "status poll failed");
                    }
                }
            }
        });
    }

    // Usage poller — fetches cost + token data every 5s (lower frequency than
    // screen/status since cost changes less frequently).
    {
        let entry = Arc::clone(&entry);
        let http = http_client;
        tokio::spawn(async move {
            let client = UpstreamClient::with_timeout(
                http,
                entry.url.clone(),
                entry.auth_token.clone(),
                POLLER_TIMEOUT,
            );
            let mut interval = tokio::time::interval(Duration::from_secs(5));
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

            loop {
                tokio::select! {
                    _ = cancel.cancelled() => break,
                    _ = interval.tick() => {}
                }

                if entry.health_failures.load(Ordering::Relaxed) > 0 {
                    continue;
                }

                match client.get_usage().await {
                    Ok(value) => {
                        let usage = CachedUsage {
                            input_tokens: value
                                .get("input_tokens")
                                .and_then(|v| v.as_u64())
                                .unwrap_or(0),
                            output_tokens: value
                                .get("output_tokens")
                                .and_then(|v| v.as_u64())
                                .unwrap_or(0),
                            total_cost_usd: value
                                .get("total_cost_usd")
                                .and_then(|v| v.as_f64())
                                .unwrap_or(0.0),
                            request_count: value
                                .get("request_count")
                                .and_then(|v| v.as_u64())
                                .unwrap_or(0),
                            fetched_at: epoch_ms(),
                        };
                        *entry.cached_usage.write().await = Some(usage);
                    }
                    Err(e) => {
                        tracing::trace!(session_id = %entry.id, err = %e, "usage poll failed");
                    }
                }
            }
        });
    }
}

#[cfg(test)]
#[path = "poller_tests.rs"]
mod tests;
