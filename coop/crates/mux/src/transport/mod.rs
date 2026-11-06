// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! HTTP + WebSocket transport for the mux proxy.

pub mod auth;
pub mod base_path;
pub mod edge_auth;
pub mod http;
pub mod http_cred;
pub mod land_principal;
pub mod nats_sub;
pub mod oidc;
pub mod project_scope;
pub mod sse;
pub mod ws;
pub mod ws_mux;

use std::sync::Arc;

use axum::middleware;
use axum::response::Html;
use axum::routing::{delete, get, post};
use axum::Router;
use tower_http::cors::CorsLayer;

use crate::state::MuxState;

/// Embedded mux dashboard HTML.
const MUX_HTML: &str = include_str!("../../../web/dist/mux.html");

/// Path to on-disk mux HTML (debug builds only, for `--hot` live reload).
#[cfg(debug_assertions)]
const MUX_HTML_PATH: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../web/dist/mux.html");

/// Build the axum `Router`, optionally serving HTML from disk for live reload.
#[cfg(debug_assertions)]
pub fn build_router_hot(state: Arc<MuxState>, hot: bool) -> Router {
    use axum::http::StatusCode;
    use axum::response::IntoResponse;

    let bp = state.config.normalized_base_path();
    if hot {
        let bp_for_route = bp.clone();
        build_router_inner(
            state,
            get(move || {
                let bp = bp_for_route.clone();
                async move {
                    match tokio::fs::read_to_string(MUX_HTML_PATH).await {
                        Ok(html) => Html(base_path::inject_html(&html, &bp)).into_response(),
                        Err(e) => (
                            StatusCode::INTERNAL_SERVER_ERROR,
                            format!("failed to read mux.html: {e}"),
                        )
                            .into_response(),
                    }
                }
            }),
            &bp,
        )
    } else {
        let html: Arc<str> = Arc::from(base_path::inject_html(MUX_HTML, &bp).as_str());
        let html_for_route = Arc::clone(&html);
        build_router_inner(
            state,
            get(move || {
                let html = Arc::clone(&html_for_route);
                async move { Html(html.to_string()) }
            }),
            &bp,
        )
    }
}

/// Build the axum `Router` with all mux routes.
pub fn build_router(state: Arc<MuxState>) -> Router {
    let bp = state.config.normalized_base_path();
    let html: Arc<str> = Arc::from(base_path::inject_html(MUX_HTML, &bp).as_str());
    let html_for_route = Arc::clone(&html);
    build_router_inner(
        state,
        get(move || {
            let html = Arc::clone(&html_for_route);
            async move { Html(html.to_string()) }
        }),
        &bp,
    )
}

fn build_router_inner(
    state: Arc<MuxState>,
    mux_route: axum::routing::MethodRouter<Arc<MuxState>>,
    base_path: &str,
) -> Router {
    let router = Router::new()
        // Health + version (no auth)
        .route("/api/v1/health", get(http::health))
        .route("/api/v1/version", get(http::version))
        // Prometheus metrics (no auth, internal scrape only)
        .route("/metrics", get(http::metrics))
        // Session management
        .route("/api/v1/sessions", post(http::register_session).get(http::list_sessions))
        .route("/api/v1/sessions/{id}", delete(http::deregister_session))
        // Cached data
        .route("/api/v1/sessions/{id}/screen", get(http::session_screen))
        .route("/api/v1/sessions/{id}/status", get(http::session_status))
        // Proxy endpoints
        .route("/api/v1/sessions/{id}/agent", get(http::session_agent))
        .route("/api/v1/sessions/{id}/agent/nudge", post(http::session_agent_nudge))
        .route("/api/v1/sessions/{id}/agent/respond", post(http::session_agent_respond))
        .route("/api/v1/sessions/{id}/input", post(http::session_input))
        .route("/api/v1/sessions/{id}/input/raw", post(http::session_input_raw))
        .route("/api/v1/sessions/{id}/input/keys", post(http::session_input_keys))
        .route("/api/v1/sessions/{id}/input/mouse", post(http::session_input_mouse))
        .route("/api/v1/sessions/{id}/resize", post(http::session_resize))
        .route("/api/v1/sessions/{id}/upload", post(http::session_upload))
        // Proxy GET endpoints (output, transcripts, recording)
        .route("/api/v1/sessions/{id}/output", get(http::session_output))
        .route("/api/v1/sessions/{id}/transcripts", get(http::session_transcripts))
        .route("/api/v1/sessions/{id}/transcripts/catchup", get(http::session_transcripts_catchup))
        .route(
            "/api/v1/sessions/{id}/transcripts/{number}",
            get(http::session_transcript_by_number),
        )
        .route("/api/v1/sessions/{id}/recording", get(http::session_recording))
        .route("/api/v1/sessions/{id}/recording/catchup", get(http::session_recording_catchup))
        // Launch
        .route("/api/v1/sessions/launch", post(http::launch_session))
        .route("/api/v1/config/launch", get(http::launch_config))
        // WebSocket (per-session bridge)
        .route("/ws/{session_id}", get(ws::ws_handler))
        // Mux aggregation
        .route("/ws/mux", get(ws_mux::ws_mux_handler))
        // SSE fallback (for edges that drop WS upgrades, e.g. AWS Verified
        // Access). Downstream GET streams + upstream POST control pairs.
        .route("/sse/mux", get(sse::sse_mux_handler))
        .route("/sse/mux/send", post(sse::sse_mux_send_handler))
        .route("/sse/session/{session_id}", get(sse::sse_session_handler))
        .route("/sse/session/{session_id}/send", post(sse::sse_session_send_handler))
        .route("/mux", mux_route.clone())
        // Credential management
        .route("/api/v1/credentials/status", get(http_cred::credentials_status))
        .route("/api/v1/credentials/new", post(http_cred::credentials_new))
        .route("/api/v1/credentials/{account}", delete(http_cred::credentials_delete))
        .route("/api/v1/credentials/set", post(http_cred::credentials_set))
        .route("/api/v1/credentials/reauth", post(http_cred::credentials_reauth))
        .route("/api/v1/credentials/distribute", post(http_cred::credentials_distribute))
        // Credential pool
        .route("/api/v1/credentials/pool", get(http_cred::credentials_pool))
        .route("/api/v1/credentials/pool/rebalance", post(http_cred::credentials_pool_rebalance))
        // Middleware (applied bottom-up: inner-most listed first)
        .layer(middleware::from_fn_with_state(state.clone(), auth::auth_layer))
        // Edge-auth runs OUTSIDE auth_layer so it can stamp a trust
        // marker on the request extensions BEFORE auth_layer reads it.
        .layer(middleware::from_fn_with_state(state.clone(), edge_auth::edge_auth_layer))
        .layer(CorsLayer::permissive());

    // When BASE_PATH is set, also serve the dashboard at "/" so that a
    // strip-prefix proxy (Traefik `stripPrefix: /mux`) hitting `/mux/` →
    // `/` lands on the dashboard rather than a 404.
    let router = if base_path.is_empty() { router } else { router.route("/", mux_route) };

    router.with_state(state)
}
