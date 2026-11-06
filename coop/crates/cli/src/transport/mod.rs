// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! API contract types and server implementation for HTTP and WebSocket transports.

pub mod auth;
pub mod compat;
pub mod grpc;
pub mod handler;
pub mod http;
pub mod inbox;
pub mod nats;
pub mod nats_forward;
pub mod nats_relay;
pub mod nats_supervisor;
mod nats_url;
pub mod state;
pub mod ws;

pub use state::Store;

use std::sync::Arc;

use axum::http::StatusCode;
use axum::middleware;
use axum::response::Html;
#[cfg(debug_assertions)]
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use base64::Engine;
use serde::{Deserialize, Serialize};
use tower_http::cors::CorsLayer;

use tokio::sync::broadcast;
use tokio_util::sync::CancellationToken;

use crate::driver::{AgentState, NudgeStep, PromptKind, QuestionAnswer, RespondEncoder};
use crate::error::ErrorCode;
use crate::event::{InputEvent, TransitionEvent};

/// Translate a named key to its terminal escape sequence (case-insensitive).
pub fn encode_key(name: &str) -> Option<Vec<u8>> {
    let lower = name.to_lowercase();
    let bytes: &[u8] = match lower.as_str() {
        "enter" | "return" => b"\r",
        "tab" => b"\t",
        "escape" | "esc" => b"\x1b",
        "backspace" => b"\x7f",
        "delete" | "del" => b"\x1b[3~",
        "up" => b"\x1b[A",
        "down" => b"\x1b[B",
        "right" => b"\x1b[C",
        "left" => b"\x1b[D",
        "home" => b"\x1b[H",
        "end" => b"\x1b[F",
        "pageup" | "page_up" => b"\x1b[5~",
        "pagedown" | "page_down" => b"\x1b[6~",
        "insert" => b"\x1b[2~",
        "f1" => b"\x1bOP",
        "f2" => b"\x1bOQ",
        "f3" => b"\x1bOR",
        "f4" => b"\x1bOS",
        "f5" => b"\x1b[15~",
        "f6" => b"\x1b[17~",
        "f7" => b"\x1b[18~",
        "f8" => b"\x1b[19~",
        "f9" => b"\x1b[20~",
        "f10" => b"\x1b[21~",
        "f11" => b"\x1b[23~",
        "f12" => b"\x1b[24~",
        "space" => b" ",
        _ => {
            // Generic Ctrl-<letter> handler
            if let Some(ch_str) = lower.strip_prefix("ctrl-") {
                let ch = ch_str.chars().next()?;
                if ch.is_ascii_lowercase() {
                    let ctrl = (ch.to_ascii_uppercase() as u8).wrapping_sub(b'@');
                    return Some(vec![ctrl]);
                }
            }
            return None;
        }
    };
    Some(bytes.to_vec())
}

/// Send encoder steps to the PTY, respecting inter-step delays.
///
/// For steps with a `delay_after`, we first send a `WaitForDrain` marker
/// and wait for the backend to confirm the write completed before starting
/// the delay timer.  This prevents the channel-timing bug where large
/// writes block in the backend while the delay runs concurrently.
pub async fn deliver_steps(
    input_tx: &tokio::sync::mpsc::Sender<InputEvent>,
    steps: Vec<NudgeStep>,
) -> Result<(), ErrorCode> {
    for step in steps {
        input_tx
            .send(InputEvent::Write(bytes::Bytes::from(step.bytes)))
            .await
            .map_err(|_| ErrorCode::Internal)?;
        if let Some(delay) = step.delay_after {
            // Wait for all prior writes to be flushed to the PTY before
            // starting the delay timer.
            let (tx, rx) = tokio::sync::oneshot::channel();
            input_tx.send(InputEvent::WaitForDrain(tx)).await.map_err(|_| ErrorCode::Internal)?;
            let _ = rx.await;
            tokio::time::sleep(delay).await;
        }
    }
    Ok(())
}

/// Spawn a background monitor that retries Enter with exponential backoff
/// if the agent doesn't transition away from `Idle`.
///
/// The first retry fires after `timeout`; subsequent retries double the
/// interval (4s → 8s → 16s → …) up to `max_retries` total attempts.
///
/// Cancellation conditions (any of these cancels the retry loop):
/// - State transitions to Working, Prompt, or Exited
/// - The returned `CancellationToken` is cancelled (by next `InputGate::acquire`)
///
/// `input_activity` is accepted for API compatibility but ignored — the
/// retry's own `\r` writes trigger `input_activity`, which caused
/// self-cancellation after just one attempt.
///
/// **Scope:** nudge only.  Respond is excluded because double-Enter on a
/// prompt could select the wrong option.
pub fn spawn_enter_retry(
    input_tx: tokio::sync::mpsc::Sender<InputEvent>,
    state_rx: broadcast::Receiver<TransitionEvent>,
    input_activity: Arc<tokio::sync::Notify>,
    timeout: std::time::Duration,
) -> CancellationToken {
    spawn_enter_retry_with_max(
        input_tx,
        state_rx,
        input_activity,
        timeout,
        DEFAULT_NUDGE_ENTER_MAX_RETRIES,
    )
}

/// Default maximum number of Enter retries after a nudge delivery.
const DEFAULT_NUDGE_ENTER_MAX_RETRIES: u32 = 10;

/// Maximum backoff ceiling so retries stay frequent enough to catch TUI quiet
/// windows.  Without a cap the exponential series reaches 64 s+ which is too
/// long — the operator has to step in manually.
const MAX_RETRY_DELAY: std::time::Duration = std::time::Duration::from_secs(8);

/// Like [`spawn_enter_retry`] but with an explicit max-retries parameter.
///
/// Note: `input_activity` was previously used as a cancellation signal, but
/// the retry's own `\r` writes flow through `handle_input` which calls
/// `input_activity.notify_waiters()`, causing self-cancellation after just
/// one attempt.  We now rely solely on state transitions (which confirm
/// Enter was actually processed) and the cancellation token (which fires
/// when a new delivery acquires the `InputGate`).
pub fn spawn_enter_retry_with_max(
    input_tx: tokio::sync::mpsc::Sender<InputEvent>,
    mut state_rx: broadcast::Receiver<TransitionEvent>,
    _input_activity: Arc<tokio::sync::Notify>,
    timeout: std::time::Duration,
    max_retries: u32,
) -> CancellationToken {
    let cancel = CancellationToken::new();
    let cancel_clone = cancel.clone();
    tokio::spawn(async move {
        tracing::debug!(
            timeout_ms = timeout.as_millis() as u64,
            max_retries,
            "nudge enter-retry: monitor started"
        );
        let mut delay = timeout;
        let mut attempt = 0u32;
        loop {
            tokio::select! {
                _ = cancel_clone.cancelled() => {
                    tracing::debug!(attempt, "nudge enter-retry: cancelled by new delivery");
                    break;
                }
                _ = async {
                    // Wait for a state transition that confirms Enter was processed.
                    while let Ok(event) = state_rx.recv().await {
                        match &event.next {
                            AgentState::Working
                            | AgentState::Prompt { .. }
                            | AgentState::Exited { .. } => break,
                            _ => continue,
                        }
                    }
                } => {
                    tracing::debug!(attempt, "nudge enter-retry: state transitioned, done");
                    break;
                }
                _ = tokio::time::sleep(delay) => {
                    attempt += 1;
                    tracing::info!(
                        attempt,
                        max_retries,
                        delay_ms = delay.as_millis() as u64,
                        "nudge enter-retry: timeout reached, resending \\r"
                    );
                    let _ = input_tx.send(InputEvent::Write(bytes::Bytes::from_static(b"\r"))).await;
                    if attempt >= max_retries {
                        tracing::warn!(
                            attempt,
                            "nudge enter-retry: max retries exhausted, giving up"
                        );
                        break;
                    }
                    // Exponential backoff capped at MAX_RETRY_DELAY so retries
                    // stay frequent enough to catch Ink TUI quiet windows.
                    delay = delay.saturating_mul(2).min(MAX_RETRY_DELAY);
                }
            }
        }
    });
    cancel
}

/// Resolve the option number for a permission prompt from `accept` and `option`.
///
/// If `option` is provided, it takes precedence. Otherwise, `accept` is mapped:
/// `true` → 1 (Yes), `false` → 3 (No).
pub fn resolve_permission_option(accept: Option<bool>, option: Option<u32>) -> u32 {
    if let Some(n) = option {
        return n;
    }
    if accept.unwrap_or(false) {
        1
    } else {
        3
    }
}

/// Resolve the option number for a plan prompt from `accept` and `option`.
///
/// If `option` is provided, it takes precedence. Otherwise, `accept` is mapped:
/// `true` → 2 (Yes, auto-accept edits), `false` → 4 (freeform feedback).
pub fn resolve_plan_option(accept: Option<bool>, option: Option<u32>) -> u32 {
    if let Some(n) = option {
        return n;
    }
    if accept.unwrap_or(false) {
        2
    } else {
        4
    }
}

/// Match the current agent state to the appropriate encoder call.
///
/// Returns `(steps, answers_delivered)` where `answers_delivered` is the
/// number of question answers that were encoded (for question_current tracking).
pub fn encode_response(
    agent: &AgentState,
    encoder: &dyn RespondEncoder,
    accept: Option<bool>,
    option: Option<u32>,
    text: Option<&str>,
    answers: &[QuestionAnswer],
) -> Result<(Vec<NudgeStep>, usize), ErrorCode> {
    match agent {
        AgentState::Prompt { prompt } => match prompt.kind {
            PromptKind::Permission => {
                if prompt.options_fallback {
                    let accepted = accept.or(option.map(|n| n == 1)).unwrap_or(false);
                    let bytes = if accepted { b"\r".to_vec() } else { b"\x1b".to_vec() };
                    Ok((vec![NudgeStep { bytes, delay_after: None }], 0))
                } else {
                    let opt = resolve_permission_option(accept, option);
                    Ok((encoder.encode_permission(opt), 0))
                }
            }
            PromptKind::Plan => {
                if prompt.options_fallback {
                    let accepted = accept.or(option.map(|n| n == 1)).unwrap_or(false);
                    let bytes = if accepted { b"\r".to_vec() } else { b"\x1b".to_vec() };
                    Ok((vec![NudgeStep { bytes, delay_after: None }], 0))
                } else {
                    let opt = resolve_plan_option(accept, option);
                    Ok((encoder.encode_plan(opt, text), 0))
                }
            }
            PromptKind::Question => {
                if answers.is_empty() {
                    return Ok((vec![], 0));
                }
                let total_questions = prompt.questions.len();
                let count = answers.len();
                Ok((encoder.encode_question(answers, total_questions), count))
            }
            PromptKind::Setup => {
                let opt = option.unwrap_or(1);
                Ok((encoder.encode_setup(opt), 0))
            }
        },
        _ => Err(ErrorCode::NoPrompt),
    }
}

/// Advance `question_current` on the current `Question` state after answers
/// have been delivered to the PTY.
pub async fn update_question_current(state: &Store, answers_delivered: usize) {
    let mut agent = state.driver.agent_state.write().await;
    if let AgentState::Prompt { ref mut prompt } = *agent {
        if prompt.kind != PromptKind::Question {
            return;
        }
        let prev_aq = prompt.question_current;
        prompt.question_current =
            prev_aq.saturating_add(answers_delivered).min(prompt.questions.len());
        if prompt.question_current != prev_aq {
            let next = agent.clone();
            drop(agent);
            let seq = state.driver.state_seq.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            // Broadcast updated state so clients see question_current progress.
            let last_message = state.driver.last_message.read().await.clone();
            let _ = state.channels.state_tx.send(crate::event::TransitionEvent {
                prev: next.clone(),
                next,
                seq,
                cause: String::new(),
                last_message,
            });
        }
    }
}

/// Read from the ring buffer starting at `offset`, combine wrapping slices,
/// and return the raw bytes.
pub fn read_ring_combined(ring: &crate::ring::RingBuffer, offset: u64) -> Vec<u8> {
    let (a, b) = ring.read_from(offset).unwrap_or((&[], &[]));
    [a, b].concat()
}

/// Result of reading and encoding a ring buffer replay.
pub struct ReplayData {
    pub data: String,
    pub offset: u64,
    pub next_offset: u64,
    pub total_written: u64,
}

/// Clamp offset to the oldest available position, read from the ring buffer,
/// optionally truncate, and base64-encode.  Shared by WS and HTTP handlers.
pub fn read_ring_replay(
    ring: &crate::ring::RingBuffer,
    offset: u64,
    limit: Option<usize>,
) -> ReplayData {
    let total_written = ring.total_written();
    let offset = offset.max(ring.oldest_offset());
    let mut combined = read_ring_combined(ring, offset);
    if let Some(limit) = limit {
        combined.truncate(limit);
    }
    let read_len = combined.len() as u64;
    ReplayData {
        data: base64::engine::general_purpose::STANDARD.encode(&combined),
        offset,
        next_offset: offset + read_len,
        total_written,
    }
}

/// Top-level error response envelope shared across HTTP and WebSocket.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ErrorResponse {
    pub error: ErrorBody,
}

/// Error body containing a machine-readable code and human-readable message.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ErrorBody {
    pub code: String,
    pub message: String,
}

impl ErrorCode {
    /// Convert this error code into a transport [`ErrorBody`].
    pub fn to_error_body(&self, message: impl Into<String>) -> ErrorBody {
        ErrorBody { code: self.as_str().to_owned(), message: message.into() }
    }

    /// Convert this error code into an axum JSON error response.
    pub fn to_http_response(
        &self,
        message: impl Into<String>,
    ) -> (StatusCode, Json<ErrorResponse>) {
        let status =
            StatusCode::from_u16(self.http_status()).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
        let body = ErrorResponse { error: self.to_error_body(message) };
        (status, Json(body))
    }
}

/// Convert named key sequences to raw bytes for PTY input.
///
/// Delegates to [`encode_key`] for each key; returns an error with the
/// unrecognised key name if any key is unknown.
pub fn keys_to_bytes(keys: &[String]) -> Result<Vec<u8>, String> {
    let mut out = Vec::new();
    for key in keys {
        match encode_key(key) {
            Some(bytes) => out.extend_from_slice(&bytes),
            None => return Err(key.clone()),
        }
    }
    Ok(out)
}

/// Embedded web terminal UI (served at `/`).
const TERMINAL_HTML: &str = include_str!("../../../web/dist/terminal.html");

/// Path to on-disk terminal HTML (debug builds only, for `--hot` live reload).
#[cfg(debug_assertions)]
const TERMINAL_HTML_PATH: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../web/dist/terminal.html");

/// Build the axum `Router`, optionally serving HTML from disk for live reload.
#[cfg(debug_assertions)]
pub fn build_router_hot(state: Arc<Store>, hot: bool) -> Router {
    if hot {
        build_router_inner(
            state,
            get(|| async {
                match tokio::fs::read_to_string(TERMINAL_HTML_PATH).await {
                    Ok(html) => Html(html).into_response(),
                    Err(e) => (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        format!("failed to read terminal.html: {e}"),
                    )
                        .into_response(),
                }
            }),
        )
    } else {
        build_router_inner(state, get(|| async { Html(TERMINAL_HTML) }))
    }
}

/// Build the axum `Router` with all HTTP and WebSocket routes.
pub fn build_router(state: Arc<Store>) -> Router {
    build_router_inner(state, get(|| async { Html(TERMINAL_HTML) }))
}

fn build_router_inner(
    state: Arc<Store>,
    index_route: axum::routing::MethodRouter<Arc<Store>>,
) -> Router {
    Router::new()
        .route("/", index_route)
        .route("/api/v1/health", get(http::health))
        .route("/api/v1/ready", get(http::ready))
        .route("/api/v1/livez", get(http::livez))
        .route("/api/v1/screen", get(http::screen))
        .route("/api/v1/screen/text", get(http::screen_text))
        .route("/api/v1/output", get(http::output))
        .route("/api/v1/status", get(http::status))
        .route("/api/v1/input", post(http::input))
        .route("/api/v1/input/raw", post(http::input_raw))
        .route("/api/v1/input/keys", post(http::input_keys))
        .route("/api/v1/input/mouse", post(http::input_mouse))
        .route("/api/v1/resize", post(http::resize))
        .route("/api/v1/signal", post(http::signal))
        .route("/api/v1/agent", get(http::agent))
        .route("/api/v1/agent/nudge", post(http::agent_nudge))
        .route("/api/v1/agent/respond", post(http::agent_respond))
        .route("/api/v1/hooks/stop", post(http::hooks_stop))
        .route("/api/v1/stop/resolve", post(http::resolve_stop))
        .route("/api/v1/session/usage", get(http::session_usage))
        .route("/api/v1/session/profiles", post(http::register_profiles).get(http::list_profiles))
        .route(
            "/api/v1/session/profiles/mode",
            get(http::get_profile_mode).put(http::put_profile_mode),
        )
        .route("/api/v1/session/profiles/probe", post(http::probe_profiles))
        .route("/api/v1/session/switch", post(http::switch_session))
        .route("/api/v1/session/restart", post(http::restart_session))
        .route("/api/v1/session/retire", post(http::retire_session))
        .route("/api/v1/shutdown", post(http::shutdown))
        .route("/api/v1/config/stop", get(http::get_stop_config).put(http::put_stop_config))
        .route("/api/v1/hooks/start", post(http::hooks_start))
        .route("/api/v1/config/start", get(http::get_start_config).put(http::put_start_config))
        .route("/api/v1/transcripts", get(http::list_transcripts))
        .route("/api/v1/transcripts/catchup", get(http::catchup_transcripts))
        .route("/api/v1/events/catchup", get(http::catchup_events))
        .route("/api/v1/recording", get(http::get_recording).put(http::put_recording))
        .route("/api/v1/recording/catchup", get(http::catchup_recording))
        .route("/api/v1/recording/download", get(http::download_recording))
        .route("/api/v1/upload", post(http::upload))
        .route("/api/v1/transcripts/{number}", get(http::get_transcript))
        .route("/metrics", get(http::metrics))
        .route("/ws", get(ws::ws_handler))
        .layer(middleware::from_fn_with_state(state.clone(), auth::auth_layer))
        .layer(middleware::from_fn(compat::http_compat_layer))
        .layer(CorsLayer::permissive())
        .with_state(state)
}

/// Build a minimal health-only router (for `--port-health`).
pub fn build_health_router(state: Arc<Store>) -> Router {
    Router::new()
        .route("/api/v1/health", get(http::health))
        .route("/api/v1/ready", get(http::ready))
        .route("/api/v1/livez", get(http::livez))
        .route("/api/v1/agent", get(http::agent))
        .with_state(state)
}

/// Build a minimal metrics-only router (for `--port-metrics`).
///
/// Serves `GET /metrics` without the bearer-token auth layer so a Prometheus
/// scraper can reach it on a dedicated port. The session usage counters and
/// latency histograms it exposes are non-sensitive operational telemetry.
pub fn build_metrics_router(state: Arc<Store>) -> Router {
    Router::new().route("/metrics", get(http::metrics)).with_state(state)
}

#[cfg(test)]
#[path = "mod_tests.rs"]
mod tests;
