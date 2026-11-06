// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Agent state and interaction HTTP handlers.

use std::sync::atomic::Ordering;
use std::sync::Arc;

use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
use serde::{Deserialize, Serialize};

use crate::driver::PromptContext;
use crate::transport::handler::{
    error_message, extract_parked_fields, handle_nudge, handle_respond, TransportQuestionAnswer,
};
use crate::transport::state::Store;

// -- Types --------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AgentResponse {
    pub agent: String,
    pub session_id: String,
    pub state: String,
    pub since_seq: u64,
    pub screen_seq: u64,
    pub detection_tier: String,
    pub detection_cause: String,
    pub prompt: Option<PromptContext>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error_detail: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error_category: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub parked_reason: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub resume_at_epoch_ms: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub last_message: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub account_name: Option<String>,
    /// Live context-window occupancy in tokens at the most recent request
    /// (`input + cache_read + cache_creation`). The controller's status
    /// reporter mirrors this onto the agent bead so it surfaces on the
    /// beads-viewer and Slack agent cards. Zero before the first request.
    pub context_tokens: u64,
    /// In-pod running cost estimate (USD) for this session so far. Summed from
    /// per-turn LiteLLM response_cost — non-authoritative (the controller
    /// reconciles the real total at session close) but lets the card show a
    /// live "cost so far" instead of $0 until the session ends.
    pub cost_usd: f64,
    /// Cumulative count of LLM round-trips (requests) this session has made so
    /// far — the "turns" counter. The controller's status reporter mirrors this
    /// onto the agent bead as the live `live_turns` field so it advances while
    /// the agent runs; at session close the authoritative `total_request_count`
    /// takes over. Zero before the first request.
    pub request_count: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NudgeRequest {
    pub message: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RespondRequest {
    pub accept: Option<bool>,
    pub text: Option<String>,
    #[serde(default)]
    pub answers: Vec<TransportQuestionAnswer>,
    pub option: Option<i32>,
}

// -- Handlers -----------------------------------------------------------------

/// `GET /api/v1/agent`
pub async fn agent(State(s): State<Arc<Store>>) -> impl IntoResponse {
    let state = s.driver.agent_state.read().await;
    let screen = s.terminal.screen.read().await;
    let detection = s.driver.detection.read().await;
    let session_id = s.session_id.read().await.clone();
    let usage = s.usage.snapshot().await;

    let (parked_reason, resume_at_epoch_ms) = extract_parked_fields(&state);

    Json(AgentResponse {
        agent: s.config.agent.to_string(),
        session_id,
        state: state.as_str().to_owned(),
        since_seq: s.driver.state_seq.load(Ordering::Acquire),
        screen_seq: screen.seq(),
        detection_tier: detection.tier_str(),
        detection_cause: detection.cause.clone(),
        prompt: state.prompt().cloned(),
        error_detail: s.driver.error.read().await.as_ref().map(|e| e.detail.clone()),
        error_category: s
            .driver
            .error
            .read()
            .await
            .as_ref()
            .map(|e| e.category.as_str().to_owned()),
        parked_reason,
        resume_at_epoch_ms,
        last_message: s.driver.last_message.read().await.clone(),
        account_name: s.profile.active_name().await,
        context_tokens: usage.context_tokens,
        cost_usd: usage.total_cost_usd,
        request_count: usage.request_count,
    })
    .into_response()
}

/// `POST /api/v1/agent/nudge`
pub async fn agent_nudge(
    State(s): State<Arc<Store>>,
    Json(req): Json<NudgeRequest>,
) -> impl IntoResponse {
    match handle_nudge(&s, &req.message).await {
        Ok(outcome) => Json(outcome).into_response(),
        Err(code) => code.to_http_response(error_message(code)).into_response(),
    }
}

/// `POST /api/v1/agent/respond`
pub async fn agent_respond(
    State(s): State<Arc<Store>>,
    Json(req): Json<RespondRequest>,
) -> impl IntoResponse {
    match handle_respond(&s, req.accept, req.option, req.text.as_deref(), &req.answers).await {
        Ok(outcome) => Json(outcome).into_response(),
        Err(code) => code.to_http_response(error_message(code)).into_response(),
    }
}
