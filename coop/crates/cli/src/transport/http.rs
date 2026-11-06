// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! HTTP request/response types and axum handler implementations.

mod agent;
mod events;
mod hooks;
mod metrics;
mod record;
mod screen;
mod switch;
mod transcript;
mod upload;
mod usage;

pub use agent::*;
pub use events::*;
pub use hooks::*;
pub use metrics::*;
pub use record::*;
pub use screen::*;
pub use switch::*;
pub use transcript::*;
pub use upload::*;
pub use usage::*;

use std::sync::Arc;

use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
use serde::Serialize;

use crate::transport::state::Store;

// -- Lifecycle ----------------------------------------------------------------

/// `POST /api/v1/session/retire` — rename the active session log (and its
/// companion state) to `.stale` so the next coop start does not resume from it.
///
/// Idempotent. Returns 200 with a JSON body listing what was renamed.
/// `retired:false` means there was nothing to retire (no path tracked, or all
/// paths already missing) — this is not an error.
#[derive(Debug, Serialize)]
pub struct RetireResponse {
    pub retired: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub log_path: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub companion_path: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub session_env_path: Option<String>,
}

pub async fn retire_session(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let path = s.switch.session_log_path.read().await.clone();
    let outcome = match path {
        // A Codex rollout has no companion dirs: rename just the file.
        Some(p) if s.config.agent == crate::driver::AgentType::Codex => {
            crate::driver::claude::retire::RetireOutcome {
                log_retired: crate::driver::codex::rollout::retire_rollout(&p),
                ..Default::default()
            }
        }
        // A Gemini session record is likewise a single file.
        Some(p) if s.config.agent == crate::driver::AgentType::Gemini => {
            crate::driver::claude::retire::RetireOutcome {
                log_retired: crate::driver::gemini::session::retire_session(&p),
                ..Default::default()
            }
        }
        Some(p) => crate::driver::claude::retire::retire_session_log(&p),
        None => crate::driver::claude::retire::RetireOutcome::default(),
    };
    Json(RetireResponse {
        retired: outcome.any(),
        log_path: outcome.log_retired.map(|p| p.display().to_string()),
        companion_path: outcome.companion_retired.map(|p| p.display().to_string()),
        session_env_path: outcome.session_env_retired.map(|p| p.display().to_string()),
    })
}

/// `POST /api/v1/session/restart` — kill and respawn the agent process (202 Accepted).
pub async fn restart_session(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let req = crate::switch::SwitchRequest { credentials: None, force: true, profile: None };
    match s.switch.switch_tx.try_send(req) {
        Ok(()) => axum::http::StatusCode::ACCEPTED.into_response(),
        Err(tokio::sync::mpsc::error::TrySendError::Full(_)) => {
            crate::error::ErrorCode::SwitchInProgress
                .to_http_response("a switch is already in progress")
                .into_response()
        }
        Err(tokio::sync::mpsc::error::TrySendError::Closed(_)) => crate::error::ErrorCode::Internal
            .to_http_response("switch channel closed")
            .into_response(),
    }
}

/// `POST /api/v1/shutdown` — initiate graceful coop shutdown.
pub async fn shutdown(State(s): State<Arc<Store>>) -> impl IntoResponse {
    s.lifecycle.shutdown.cancel();
    Json(serde_json::json!({ "accepted": true }))
}

#[cfg(test)]
mod screen_tests;

#[cfg(test)]
mod agent_tests;

#[cfg(test)]
mod hooks_tests;

#[cfg(test)]
mod transcript_tests;

#[cfg(test)]
mod profile_tests;

#[cfg(test)]
mod switch_tests;

#[cfg(test)]
mod usage_tests;
