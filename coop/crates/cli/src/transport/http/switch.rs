// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Session switch and profile HTTP handlers.

use std::sync::Arc;

use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
use serde::{Deserialize, Serialize};

use crate::error::ErrorCode;
use crate::profile::{ProfileEntry, ProfileInfo, ProfileMode};
use crate::switch::SwitchRequest;
use crate::transport::handler::resolve_switch_profile;
use crate::transport::state::Store;

// -- Switch -------------------------------------------------------------------

/// `POST /api/v1/session/switch` — schedule a credential switch (202 Accepted).
pub async fn switch_session(
    State(s): State<Arc<Store>>,
    Json(mut req): Json<SwitchRequest>,
) -> impl IntoResponse {
    if let Err(code) = resolve_switch_profile(&s, &mut req).await {
        return code.to_http_response("unknown profile").into_response();
    }
    match s.switch.switch_tx.try_send(req) {
        Ok(()) => axum::http::StatusCode::ACCEPTED.into_response(),
        Err(tokio::sync::mpsc::error::TrySendError::Full(_)) => ErrorCode::SwitchInProgress
            .to_http_response("a switch is already in progress")
            .into_response(),
        Err(tokio::sync::mpsc::error::TrySendError::Closed(_)) => {
            ErrorCode::Internal.to_http_response("switch channel closed").into_response()
        }
    }
}

// -- Profiles -----------------------------------------------------------------

/// Request body for `POST /api/v1/session/profiles`.
#[derive(Debug, Deserialize)]
pub struct RegisterProfilesRequest {
    pub profiles: Vec<ProfileEntry>,
}

/// Response for `GET /api/v1/session/profiles`.
#[derive(Debug, Serialize)]
pub struct ProfileListResponse {
    pub profiles: Vec<ProfileInfo>,
    pub mode: String,
    pub active_profile: Option<String>,
}

/// `POST /api/v1/session/profiles` — register credential profiles.
pub async fn register_profiles(
    State(s): State<Arc<Store>>,
    Json(req): Json<RegisterProfilesRequest>,
) -> impl IntoResponse {
    let count = req.profiles.len();
    s.register_profiles(req.profiles).await;
    Json(serde_json::json!({ "registered": count }))
}

/// `GET /api/v1/session/profiles` — list all profiles with status.
pub async fn list_profiles(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let profiles = s.profile.list().await;
    let mode = s.profile.mode().as_str().to_owned();
    let active_profile = s.profile.active_name().await;
    Json(ProfileListResponse { profiles, mode, active_profile })
}

// -- Profile Mode -------------------------------------------------------------

/// Request body for `PUT /api/v1/session/profiles/mode`.
#[derive(Debug, Deserialize)]
pub struct ProfileModeRequest {
    pub mode: String,
}

/// Response for `GET/PUT /api/v1/session/profiles/mode`.
#[derive(Debug, Serialize)]
pub struct ProfileModeResponse {
    pub mode: String,
}

/// `GET /api/v1/session/profiles/mode` — get the current profile rotation mode.
pub async fn get_profile_mode(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let mode = s.profile.mode().as_str().to_owned();
    Json(ProfileModeResponse { mode })
}

/// `PUT /api/v1/session/profiles/mode` — set the profile rotation mode.
pub async fn put_profile_mode(
    State(s): State<Arc<Store>>,
    Json(req): Json<ProfileModeRequest>,
) -> impl IntoResponse {
    match req.mode.parse::<ProfileMode>() {
        Ok(mode) => {
            s.profile.set_mode(mode);
            Json(ProfileModeResponse { mode: mode.as_str().to_owned() }).into_response()
        }
        Err(_) => ErrorCode::BadRequest
            .to_http_response("invalid mode: expected auto or manual")
            .into_response(),
    }
}

// -- Profile Probe ------------------------------------------------------------

/// Response for `POST /api/v1/session/profiles/probe`.
#[derive(Debug, Serialize)]
pub struct ProbeResponse {
    pub results: Vec<crate::probe::ProbeResult>,
}

/// Request body for probing profiles.
#[derive(Debug, Default, Deserialize)]
pub struct ProbeRequest {
    /// If set, probe only this profile. Otherwise probe all.
    #[serde(default)]
    pub profile: Option<String>,
}

/// `POST /api/v1/session/profiles/probe` — probe credentials for validity.
///
/// Validates OAuth tokens or API keys by hitting a lightweight, read-only
/// endpoint. Does not trigger rate limits or incur charges.
pub async fn probe_profiles(
    State(s): State<Arc<Store>>,
    Json(req): Json<ProbeRequest>,
) -> impl IntoResponse {
    let profile_name = req.profile;

    let results = if let Some(name) = profile_name {
        match s.profile.probe_one(&name).await {
            Some(result) => vec![result],
            None => {
                return ErrorCode::NotFound
                    .to_http_response(format!("profile not found: {name}"))
                    .into_response();
            }
        }
    } else {
        s.profile.probe_all().await
    };

    Json(ProbeResponse { results }).into_response()
}
