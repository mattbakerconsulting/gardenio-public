// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Event log catchup HTTP handler.

use std::sync::Arc;

use axum::extract::{Query, State};
use axum::response::IntoResponse;
use axum::Json;
use serde::Deserialize;

use crate::event_log::CatchupResponse;
use crate::transport::state::Store;

/// Query parameters for the event log catchup endpoint.
#[derive(Debug, Clone, Deserialize)]
pub struct EventCatchupQuery {
    #[serde(default)]
    pub since_seq: u64,
    #[serde(default)]
    pub since_hook_seq: u64,
}

/// `GET /api/v1/events/catchup` — catch up on missed state and hook events.
pub async fn catchup_events(
    State(s): State<Arc<Store>>,
    Query(q): Query<EventCatchupQuery>,
) -> impl IntoResponse {
    let resp = CatchupResponse {
        state_events: s.event_log.catchup_state(q.since_seq).await,
        hook_events: s.event_log.catchup_hooks(q.since_hook_seq).await,
    };
    Json(resp)
}

#[cfg(test)]
#[path = "events_tests.rs"]
mod tests;
