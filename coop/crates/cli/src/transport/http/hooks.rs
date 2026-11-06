// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Stop and start hook HTTP handlers.

use std::sync::Arc;

use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
use serde::{Deserialize, Serialize};

use crate::compact_resume::CompactResumeConfig;
use crate::driver::ErrorCategory;
use crate::start::{compose_start_script, StartConfig};
use crate::stop::{generate_block_reason, StopConfig, StopMode, StopType};
use crate::transport::state::Store;
use axum::http::StatusCode;

// -- Stop hook types ----------------------------------------------------------

/// Event-wrapped input from the stop hook (piped from stdin via curl).
///
/// Matches the same `{"event":"stop","data":{...}}` envelope that hooks
/// write to the FIFO pipe, so the endpoint receives the same format.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StopHookInput {
    // NOTE(compat): Maintain consistent structure for all hook payloads
    #[allow(dead_code)]
    pub event: String,
    #[serde(default)]
    pub data: Option<StopHookData>,
}

/// Inner data carried inside the stop-event envelope.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct StopHookData {
    /// When `true`, this is a safety-valve invocation that must be allowed.
    #[serde(default)]
    pub stop_hook_active: bool,
}

/// Verdict returned to the hook script.
///
/// Empty object `{}` means "allow" (no `decision` field).
/// `{"decision":"block","reason":"..."}` means "block".
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StopHookVerdict {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub decision: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_message: Option<String>,
}

// -- Start hook types ---------------------------------------------------------

/// Event-wrapped input from the start hook (piped from stdin via curl).
///
/// Matches the `{"event":"start","data":{...}}` envelope that hooks
/// write to the FIFO pipe.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StartHookInput {
    // NOTE(compat): Maintain consistent structure for all hook payloads
    #[allow(dead_code)]
    pub event: String,
    #[serde(default)]
    pub data: Option<serde_json::Value>,
}

// -- Stop hook handlers -------------------------------------------------------

/// `POST /api/v1/hooks/stop` — called by the hook script, returns verdict.
pub async fn hooks_stop(
    State(s): State<Arc<Store>>,
    Json(input): Json<StopHookInput>,
) -> impl IntoResponse {
    let stop = &s.stop;
    let config = stop.config.read().await;
    let last_message = s.driver.last_message.read().await.clone();

    // 1. Mode = Allow → always allow.
    if config.mode == StopMode::Allow {
        drop(config);
        stop.emit(StopType::Allowed, None, None);
        return Json(StopHookVerdict { decision: None, reason: None, last_message })
            .into_response();
    }

    // 2. Safety valve: stop_hook_active = true → must allow.
    let stop_hook_active = input.data.as_ref().is_some_and(|d| d.stop_hook_active);
    if stop_hook_active {
        drop(config);
        stop.emit(StopType::SafetyValve, None, None);
        return Json(StopHookVerdict { decision: None, reason: None, last_message })
            .into_response();
    }

    // 3. Unrecoverable error → allow.
    {
        let error = s.driver.error.read().await;
        if let Some(ref info) = *error {
            let is_unrecoverable =
                matches!(info.category, ErrorCategory::Unauthorized | ErrorCategory::OutOfCredits);
            if is_unrecoverable {
                let detail = Some(info.detail.clone());
                drop(error);
                drop(config);
                stop.emit(StopType::Error, None, detail);
                return Json(StopHookVerdict { decision: None, reason: None, last_message })
                    .into_response();
            }
        }
    }

    // 4. Signal received → allow and reset.
    if stop.signaled.swap(false, std::sync::atomic::Ordering::AcqRel) {
        let body = stop.signal_body.write().await.take();
        drop(config);
        stop.emit(StopType::Signaled, body, None);
        return Json(StopHookVerdict { decision: None, reason: None, last_message })
            .into_response();
    }

    // 5. Block: generate reason and return block verdict.
    let reason = generate_block_reason(&config);
    drop(config);
    stop.emit(StopType::Blocked, None, None);
    Json(StopHookVerdict { decision: Some("block".to_owned()), reason: Some(reason), last_message })
        .into_response()
}

/// `POST /api/v1/stop/resolve` — validate, store signal body, set flag.
pub async fn resolve_stop(
    State(s): State<Arc<Store>>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    match s.stop.resolve(body).await {
        Ok(()) => Json(serde_json::json!({ "accepted": true })).into_response(),
        Err(msg) => {
            s.stop.emit(StopType::Rejected, None, Some(msg.clone()));
            (StatusCode::UNPROCESSABLE_ENTITY, Json(serde_json::json!({ "error": msg })))
                .into_response()
        }
    }
}

/// `GET /api/v1/config/stop` — read current stop config.
pub async fn get_stop_config(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let config = s.stop.config.read().await;
    Json(config.clone())
}

/// `PUT /api/v1/config/stop` — update stop config.
pub async fn put_stop_config(
    State(s): State<Arc<Store>>,
    Json(new_config): Json<StopConfig>,
) -> impl IntoResponse {
    *s.stop.config.write().await = new_config;
    Json(serde_json::json!({ "updated": true }))
}

// -- Start hook handlers ------------------------------------------------------

/// `POST /api/v1/hooks/start` — called by the hook script, returns shell script.
pub async fn hooks_start(
    State(s): State<Arc<Store>>,
    Json(input): Json<StartHookInput>,
) -> impl IntoResponse {
    let start = &s.start;
    let config = start.config.read().await;

    // Extract source from data.source or data.session_type, default "unknown".
    let source = input
        .data
        .as_ref()
        .and_then(|d| {
            d.get("source")
                .or_else(|| d.get("session_type"))
                .and_then(|v| v.as_str())
                .map(|s| s.to_owned())
        })
        .unwrap_or_else(|| "unknown".to_owned());

    // Extract session_id from data.session_id.
    let session_id = input
        .data
        .as_ref()
        .and_then(|d| d.get("session_id").and_then(|v| v.as_str()).map(|s| s.to_owned()));

    let script = compose_start_script(&config, &source);
    drop(config);

    let injected = !script.is_empty();
    start.emit(source.clone(), session_id, injected);

    // Clear stale last_message when the conversation is cleared.
    // After `/clear`, Claude truncates the session log; the log watcher will
    // eventually pick up new entries, but until then the old value is misleading.
    if source == "clear" {
        *s.driver.last_message.write().await = None;
    }

    // (Harness-neutral: the snapshot copies whatever file the session-log slot
    // points at, which for Codex is the rollout, and Codex's SessionStart hook
    // carries the same `source` field.)
    //
    // Save a transcript snapshot before compaction wipes the session log, and
    // arm the post-compaction resume nudge: Claude Code returns from a
    // compaction to an EMPTY prompt, so an agent that was mid-work lands in
    // Idle and stays there until somebody nudges it (ka-0000007525).
    if source == "compact" {
        let transcript = Arc::clone(&s.transcript);
        tokio::spawn(async move {
            if let Err(e) = transcript.save_snapshot().await {
                tracing::warn!("transcript snapshot failed: {e}");
            }
        });
        crate::compact_resume::arm(Arc::clone(&s), CompactResumeConfig::from_env());
    }

    ([(axum::http::header::CONTENT_TYPE, "text/plain; charset=utf-8")], script)
}

/// `GET /api/v1/config/start` — read current start config.
pub async fn get_start_config(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let config = s.start.config.read().await;
    Json(config.clone())
}

/// `PUT /api/v1/config/start` — update start config.
pub async fn put_start_config(
    State(s): State<Arc<Store>>,
    Json(new_config): Json<StartConfig>,
) -> impl IntoResponse {
    *s.start.config.write().await = new_config;
    Json(serde_json::json!({ "updated": true }))
}
