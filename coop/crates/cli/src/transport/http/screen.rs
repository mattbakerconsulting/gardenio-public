// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Terminal screen, I/O, and health HTTP handlers.

use std::sync::atomic::Ordering;
use std::sync::Arc;

use axum::extract::{Query, State};
use axum::response::IntoResponse;
use axum::Json;
use base64::Engine;
use serde::{Deserialize, Serialize};

use crate::error::ErrorCode;
use crate::screen::CursorPosition;
use crate::transport::handler::{
    compute_health, compute_status, handle_input, handle_input_raw, handle_keys, handle_mouse,
    handle_resize, handle_signal,
};
use crate::transport::read_ring_replay;
use crate::transport::state::Store;

// -- Types --------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HealthResponse {
    pub status: String,
    pub session_id: String,
    pub pid: Option<i32>,
    pub uptime_secs: i64,
    pub agent: String,
    /// Nested terminal dimensions (deprecated — use `terminal_cols`/`terminal_rows`).
    pub terminal: TerminalSize,
    /// Terminal width (matches gRPC flat layout).
    pub terminal_cols: u16,
    /// Terminal height (matches gRPC flat layout).
    pub terminal_rows: u16,
    pub ws_clients: i32,
    pub ready: bool,
    /// Current account name (tracks active profile after credential switches).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub account_name: Option<String>,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct TerminalSize {
    pub cols: u16,
    pub rows: u16,
}

/// Response for the readiness probe.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ReadyResponse {
    pub ready: bool,
}

/// Response for the liveness probe — fully lock-free.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LivezResponse {
    pub status: String,
    pub uptime_secs: i64,
    pub pid: Option<i32>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct ScreenQuery {
    #[serde(default, alias = "cursor")]
    pub cursor: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ScreenResponse {
    pub lines: Vec<String>,
    pub ansi: Vec<String>,
    pub cols: u16,
    pub rows: u16,
    pub alt_screen: bool,
    /// The mouse tracking mode the app has set (9, 1000, 1002 or 1003; 0 for none) and
    /// whether it asked for SGR reports (1006), as `POST input/mouse` encodes them.
    #[serde(default)]
    pub mouse_tracking: u16,
    #[serde(default)]
    pub mouse_sgr: bool,
    pub cursor: Option<CursorPosition>,
    pub seq: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct OutputQuery {
    #[serde(default)]
    pub offset: u64,
    pub limit: Option<usize>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OutputResponse {
    pub data: String,
    pub offset: u64,
    pub next_offset: u64,
    pub total_written: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InputRequest {
    pub text: String,
    #[serde(default)]
    pub enter: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InputRawRequest {
    pub data: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InputResponse {
    pub bytes_written: i32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct KeysRequest {
    pub keys: Vec<String>,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct ResizeRequest {
    pub cols: u16,
    pub rows: u16,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
pub struct ResizeResponse {
    pub cols: u16,
    pub rows: u16,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SignalRequest {
    pub signal: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SignalResponse {
    pub delivered: bool,
}

// -- Handlers -----------------------------------------------------------------

/// `GET /api/v1/health`
pub async fn health(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let h = compute_health(&s).await;
    let account_name = s.profile.active_name().await;
    Json(HealthResponse {
        status: h.status,
        session_id: h.session_id,
        pid: h.pid,
        uptime_secs: h.uptime_secs,
        agent: h.agent,
        terminal: TerminalSize { cols: h.terminal_cols, rows: h.terminal_rows },
        terminal_cols: h.terminal_cols,
        terminal_rows: h.terminal_rows,
        ws_clients: h.ws_clients,
        ready: h.ready,
        account_name,
    })
}

/// `GET /api/v1/ready` — readiness probe (200 when ready, 503 otherwise).
pub async fn ready(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let is_ready = s.ready.load(Ordering::Acquire);
    let status = if is_ready {
        axum::http::StatusCode::OK
    } else {
        axum::http::StatusCode::SERVICE_UNAVAILABLE
    };
    (status, Json(ReadyResponse { ready: is_ready }))
}

/// `GET /api/v1/livez` — liveness probe. Reads only atomics (lock-free).
///
/// Returns 503 when the agent is in an unrecoverable zombie state (process
/// exited, fatal auth errors) so K8s restarts the pod instead of letting it
/// run indefinitely as a zombie.
pub async fn livez(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let pid = s.terminal.child_pid.load(Ordering::Relaxed);
    let uptime = s.config.started_at.elapsed().as_secs() as i64;
    let is_zombie = s.lifecycle.zombie.load(Ordering::Acquire);

    let status_code = if is_zombie {
        axum::http::StatusCode::SERVICE_UNAVAILABLE
    } else {
        axum::http::StatusCode::OK
    };

    let status_str = if is_zombie { "zombie" } else { "alive" };

    (
        status_code,
        Json(LivezResponse {
            status: status_str.to_owned(),
            uptime_secs: uptime,
            pid: if pid == 0 { None } else { Some(pid as i32) },
        }),
    )
}

/// `GET /api/v1/screen`
pub async fn screen(
    State(s): State<Arc<Store>>,
    Query(q): Query<ScreenQuery>,
) -> impl IntoResponse {
    let (snap, mouse) = {
        let screen = s.terminal.screen.read().await;
        (screen.snapshot_with_ansi(), screen.mouse_modes())
    };

    Json(ScreenResponse {
        lines: snap.lines,
        ansi: snap.ansi,
        cols: snap.cols,
        rows: snap.rows,
        alt_screen: snap.alt_screen,
        mouse_tracking: mouse.tracking,
        mouse_sgr: mouse.sgr,
        cursor: if q.cursor { Some(snap.cursor) } else { None },
        seq: snap.sequence,
    })
}

/// `GET /api/v1/screen/text`
pub async fn screen_text(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let snap = s.terminal.screen.read().await.snapshot_text();
    let text = snap.lines.join("\n");
    ([(axum::http::header::CONTENT_TYPE, "text/plain; charset=utf-8")], text)
}

/// `GET /api/v1/output`
pub async fn output(
    State(s): State<Arc<Store>>,
    Query(q): Query<OutputQuery>,
) -> impl IntoResponse {
    let ring = s.terminal.ring.read().await;
    let r = read_ring_replay(&ring, q.offset, q.limit);

    Json(OutputResponse {
        data: r.data,
        offset: r.offset,
        next_offset: r.next_offset,
        total_written: r.total_written,
    })
}

/// `GET /api/v1/status`
pub async fn status(State(s): State<Arc<Store>>) -> impl IntoResponse {
    Json(compute_status(&s).await)
}

/// `POST /api/v1/input`
pub async fn input(
    State(s): State<Arc<Store>>,
    Json(req): Json<InputRequest>,
) -> impl IntoResponse {
    let len = handle_input(&s, req.text, req.enter).await;
    Json(InputResponse { bytes_written: len }).into_response()
}

/// `POST /api/v1/input/raw`
pub async fn input_raw(
    State(s): State<Arc<Store>>,
    Json(req): Json<InputRawRequest>,
) -> impl IntoResponse {
    let decoded = match base64::engine::general_purpose::STANDARD.decode(&req.data) {
        Ok(d) => d,
        Err(_) => {
            return ErrorCode::BadRequest.to_http_response("invalid base64 data").into_response()
        }
    };
    let len = handle_input_raw(&s, decoded).await;
    Json(InputResponse { bytes_written: len }).into_response()
}

/// `POST /api/v1/input/keys`
pub async fn input_keys(
    State(s): State<Arc<Store>>,
    Json(req): Json<KeysRequest>,
) -> impl IntoResponse {
    match handle_keys(&s, &req.keys).await {
        Ok(len) => Json(InputResponse { bytes_written: len }).into_response(),
        Err(bad_key) => ErrorCode::BadRequest
            .to_http_response(format!("unknown key: {bad_key}"))
            .into_response(),
    }
}

/// `POST /api/v1/input/mouse`: one press or release, encoded by coop as the
/// app asked (never raw bytes from the caller); `bytes_written` is 0 when the
/// app is not taking the mouse.
pub async fn input_mouse(
    State(s): State<Arc<Store>>,
    Json(req): Json<crate::mouse::MouseEvent>,
) -> impl IntoResponse {
    match handle_mouse(&s, &req).await {
        Ok(len) => Json(InputResponse { bytes_written: len }).into_response(),
        Err(()) => ErrorCode::BadRequest.to_http_response("cell is off the screen").into_response(),
    }
}

/// `POST /api/v1/resize`
pub async fn resize(
    State(s): State<Arc<Store>>,
    Json(req): Json<ResizeRequest>,
) -> impl IntoResponse {
    match handle_resize(&s, req.cols, req.rows).await {
        Ok(()) => Json(ResizeResponse { cols: req.cols, rows: req.rows }).into_response(),
        Err(_) => {
            ErrorCode::BadRequest.to_http_response("cols and rows must be positive").into_response()
        }
    }
}

/// `POST /api/v1/signal`
pub async fn signal(
    State(s): State<Arc<Store>>,
    Json(req): Json<SignalRequest>,
) -> impl IntoResponse {
    match handle_signal(&s, &req.signal).await {
        Ok(()) => Json(SignalResponse { delivered: true }).into_response(),
        Err(bad_signal) => ErrorCode::BadRequest
            .to_http_response(format!("unknown signal: {bad_signal}"))
            .into_response(),
    }
}
