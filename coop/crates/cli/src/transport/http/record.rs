// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Recording HTTP handlers.

use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::header;
use axum::response::IntoResponse;
use axum::Json;
use serde::Deserialize;

use crate::transport::state::Store;

/// `GET /api/v1/recording` — recording status.
pub async fn get_recording(State(s): State<Arc<Store>>) -> impl IntoResponse {
    Json(s.record.status())
}

/// Request body for PUT /api/v1/recording.
#[derive(Debug, Deserialize)]
pub struct PutRecordingBody {
    pub enabled: bool,
}

/// `PUT /api/v1/recording` — toggle recording on/off.
pub async fn put_recording(
    State(s): State<Arc<Store>>,
    Json(body): Json<PutRecordingBody>,
) -> impl IntoResponse {
    if body.enabled {
        s.record.enable().await;
    } else {
        s.record.disable();
    }
    let status = s.record.status();
    Json(serde_json::json!({
        "enabled": status.enabled,
        "path": status.path,
    }))
}

/// Query parameters for recording catchup endpoint.
#[derive(Debug, Deserialize)]
pub struct RecordingCatchupQuery {
    #[serde(default)]
    pub since_seq: u64,
}

/// `GET /api/v1/recording/catchup` — catch up on missed recording entries.
pub async fn catchup_recording(
    State(s): State<Arc<Store>>,
    Query(q): Query<RecordingCatchupQuery>,
) -> impl IntoResponse {
    let entries = s.record.catchup(q.since_seq).await;
    Json(serde_json::json!({ "entries": entries }))
}

/// `GET /api/v1/recording/download` — download full recording file.
pub async fn download_recording(State(s): State<Arc<Store>>) -> impl IntoResponse {
    match s.record.download().await {
        Some(data) => ([(header::CONTENT_TYPE, "application/jsonl")], data).into_response(),
        None => (axum::http::StatusCode::NOT_FOUND, "no recording file available").into_response(),
    }
}

#[cfg(test)]
#[path = "record_tests.rs"]
mod tests;
