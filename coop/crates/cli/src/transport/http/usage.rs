// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Session usage HTTP handler.

use std::sync::Arc;

use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
use serde::{Deserialize, Serialize};

use crate::transport::state::Store;

// -- Types --------------------------------------------------------------------

/// Response for the session usage endpoint.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UsageResponse {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cache_read_tokens: u64,
    pub cache_write_tokens: u64,
    pub request_count: u64,
    pub total_api_ms: u64,
    pub uptime_secs: i64,
    /// In-pod running cost estimate (USD), summed from per-turn response_cost.
    pub total_cost_usd: f64,
    /// Live context-window occupancy in tokens at the most recent request.
    pub context_tokens: u64,
}

// -- Handlers -----------------------------------------------------------------

/// `GET /api/v1/session/usage` — cumulative API usage for this session.
pub async fn session_usage(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let snap = s.usage.snapshot().await;
    let uptime = s.config.started_at.elapsed().as_secs() as i64;
    Json(UsageResponse {
        input_tokens: snap.input_tokens,
        output_tokens: snap.output_tokens,
        cache_read_tokens: snap.cache_read_tokens,
        cache_write_tokens: snap.cache_write_tokens,
        request_count: snap.request_count,
        total_api_ms: snap.total_api_ms,
        uptime_secs: uptime,
        total_cost_usd: snap.total_cost_usd,
        context_tokens: snap.context_tokens,
    })
}
