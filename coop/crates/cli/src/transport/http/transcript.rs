// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Transcript snapshot HTTP handlers.

use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::{header, HeaderMap, HeaderValue, StatusCode};
use axum::response::IntoResponse;
use axum::Json;
use serde::Deserialize;

use crate::error::ErrorCode;
use crate::transport::state::Store;

// -- Types --------------------------------------------------------------------

/// Query parameters for the transcript catchup endpoint.
#[derive(Debug, Clone, Deserialize)]
pub struct CatchupQuery {
    #[serde(default)]
    pub since_transcript: u32,
    #[serde(default)]
    pub since_line: u64,
}

// -- Handlers -----------------------------------------------------------------

/// `GET /api/v1/transcripts` — list all transcript snapshots.
pub async fn list_transcripts(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let list = s.transcript.list().await;
    Json(serde_json::json!({ "transcripts": list }))
}

/// `GET /api/v1/transcripts/catchup` — catch up from a cursor.
///
/// If the `Accept` header is `text/plain`, returns plain text with download headers.
/// Otherwise, returns JSON.
pub async fn catchup_transcripts(
    State(s): State<Arc<Store>>,
    Query(q): Query<CatchupQuery>,
    headers: HeaderMap,
) -> impl IntoResponse {
    let accept = headers.get(header::ACCEPT).and_then(|v| v.to_str().ok()).unwrap_or("");

    match s.transcript.catchup(q.since_transcript, q.since_line).await {
        Ok(resp) => {
            if accept.contains("text/plain") {
                // Return plain text with download headers: all transcript lines + live lines
                let mut all_lines = Vec::new();
                for transcript in &resp.transcripts {
                    all_lines.extend(transcript.lines.iter().cloned());
                }
                all_lines.extend(resp.live_lines.iter().cloned());
                let content = all_lines.join("\n");
                let mut response_headers = HeaderMap::new();
                response_headers.insert(
                    header::CONTENT_TYPE,
                    HeaderValue::from_static("text/plain; charset=utf-8"),
                );
                response_headers.insert(
                    header::CONTENT_DISPOSITION,
                    HeaderValue::from_static("attachment; filename=\"transcript.txt\""),
                );
                (StatusCode::OK, response_headers, content).into_response()
            } else {
                Json(serde_json::to_value(resp).unwrap_or_default()).into_response()
            }
        }
        Err(e) => {
            ErrorCode::Internal.to_http_response(format!("catchup failed: {e}")).into_response()
        }
    }
}

/// `GET /api/v1/transcripts/{number}` — get a single transcript's content.
///
/// If the `Accept` header is `text/plain`, returns plain text with download headers.
/// Otherwise, returns JSON.
pub async fn get_transcript(
    State(s): State<Arc<Store>>,
    axum::extract::Path(number): axum::extract::Path<u32>,
    headers: HeaderMap,
) -> impl IntoResponse {
    let accept = headers.get(header::ACCEPT).and_then(|v| v.to_str().ok()).unwrap_or("");

    match s.transcript.get_content(number).await {
        Ok(content) => {
            if accept.contains("text/plain") {
                // Return plain text with download headers
                let mut response_headers = HeaderMap::new();
                response_headers.insert(
                    header::CONTENT_TYPE,
                    HeaderValue::from_static("text/plain; charset=utf-8"),
                );
                let filename = format!("attachment; filename=\"transcript-{}.txt\"", number);
                if let Ok(header_value) = HeaderValue::from_str(&filename) {
                    response_headers.insert(header::CONTENT_DISPOSITION, header_value);
                }
                (StatusCode::OK, response_headers, content).into_response()
            } else {
                Json(serde_json::json!({ "number": number, "content": content })).into_response()
            }
        }
        Err(_) => ErrorCode::BadRequest
            .to_http_response(format!("transcript {number} not found"))
            .into_response(),
    }
}
