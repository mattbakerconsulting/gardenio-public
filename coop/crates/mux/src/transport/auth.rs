// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderMap, Request, StatusCode};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};

use crate::error::MuxError;
use crate::state::MuxState;
use crate::transport::land_principal::LandPrincipal;
use crate::transport::oidc::OidcClaims;
use crate::transport::project_scope::{scoped_route, ScopedRoute, SessionScope};

/// Constant-time string comparison to prevent timing side-channel attacks.
fn constant_time_eq(a: &str, b: &str) -> bool {
    let a = a.as_bytes();
    let b = b.as_bytes();
    if a.len() != b.len() {
        return false;
    }
    let mut acc = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        acc |= x ^ y;
    }
    acc == 0
}

/// Validate a Bearer token from HTTP headers.
pub fn validate_bearer(headers: &HeaderMap, expected: Option<&str>) -> Result<(), MuxError> {
    let expected = match expected {
        Some(tok) => tok,
        None => return Ok(()),
    };

    let header =
        headers.get("authorization").and_then(|v| v.to_str().ok()).ok_or(MuxError::Unauthorized)?;

    let token = header.strip_prefix("Bearer ").ok_or(MuxError::Unauthorized)?;
    if constant_time_eq(token, expected) {
        Ok(())
    } else {
        Err(MuxError::Unauthorized)
    }
}

/// Validate a token from a WebSocket query string (`?token=...`).
pub fn validate_ws_query(query: &str, expected: Option<&str>) -> Result<(), MuxError> {
    let expected = match expected {
        Some(tok) => tok,
        None => return Ok(()),
    };

    for pair in query.split('&') {
        if let Some(value) = pair.strip_prefix("token=") {
            if constant_time_eq(value, expected) {
                return Ok(());
            }
        }
    }

    Err(MuxError::Unauthorized)
}

/// Extract the bearer token from request headers.
///
/// Checks `Authorization: Bearer <token>` first, then falls back to the
/// `X-Auth-Request-Access-Token` header (set by oauth2-proxy ForwardAuth).
pub fn extract_bearer(headers: &HeaderMap) -> Option<String> {
    if let Some(auth) = headers.get("authorization").and_then(|v| v.to_str().ok()) {
        if let Some(token) = auth.strip_prefix("Bearer ") {
            return Some(token.to_owned());
        }
    }
    // ForwardAuth: oauth2-proxy sets this header with the raw access token.
    headers.get("x-auth-request-access-token").and_then(|v| v.to_str().ok()).map(|s| s.to_owned())
}

/// Authorize a WebSocket upgrade request.
///
/// WebSocket upgrades bypass [`auth_layer`] (which short-circuits `/ws/`
/// paths because the browser cannot set an `Authorization` header on a
/// `new WebSocket()` call), so each WS handler must re-run the auth checks
/// itself. This helper is the single source of truth for that decision and
/// MUST stay in lockstep with the precedence in [`auth_layer`]:
///
/// 1. **Edge-auth envelope** — when the upstream `gardenio-edge-auth` shim
///    verified an HMAC envelope it stamps [`TrustedOperator`] on the request
///    extensions. This is the path browsers reaching coopmux through the
///    unified host (`unifiedHost.viaShim: [mux]`) take: the shim strips the
///    caller's `Authorization` header and forwards no `?token=`, so the only
///    surviving proof of identity is the envelope. Honour it first — without
///    this branch every `/ws/*` upgrade through the shim 401s even though the
///    matching HTTP requests are trusted, which surfaces in the browser as a
///    failed chat/feed socket (ka-0000003786).
/// 2. **Static token** via `?token=` query param or `Authorization: Bearer`.
/// 3. **OIDC JWT** when an OIDC verifier is configured.
///
/// Returns the principal source that actually authenticated the request. Token
/// credentials are reduced to a SHA-256 fingerprint so paired SSE GET/POST
/// requests can be bound without retaining credential material. An OIDC
/// token also carries its land [`SessionScope`], which the handler must
/// enforce on the session it attaches to (gd-DcKLFjZuyl). When no auth
/// mechanism is configured at all (`auth_token` unset and no OIDC verifier)
/// the WS handlers historically allow the upgrade; callers preserve that by
/// gating on `state.config.auth_token.is_some() ||
/// state.oidc_verifier.is_some()`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) enum WsAuthenticated {
    Edge,
    Token(Vec<u8>),
    Oidc(Vec<u8>, SessionScope),
}

impl WsAuthenticated {
    /// The land scope of this principal; only an OIDC token can be scoped.
    pub(super) fn scope(&self) -> SessionScope {
        match self {
            Self::Oidc(_, scope) => scope.clone(),
            Self::Edge | Self::Token(_) => SessionScope::Unrestricted,
        }
    }
}

fn token_fingerprint(token: &str) -> Vec<u8> {
    ring::digest::digest(&ring::digest::SHA256, token.as_bytes()).as_ref().to_vec()
}

pub(super) async fn ws_authorized(
    state: &MuxState,
    headers: &HeaderMap,
    query_token: Option<&str>,
    edge_trusted: bool,
) -> Option<WsAuthenticated> {
    if edge_trusted {
        return Some(WsAuthenticated::Edge);
    }

    if state.config.auth_token.is_some() {
        let query_str = query_token.map(|t| format!("token={t}")).unwrap_or_default();
        if validate_ws_query(&query_str, state.config.auth_token.as_deref()).is_ok() {
            if let Some(token) = query_token {
                return Some(WsAuthenticated::Token(token_fingerprint(token)));
            }
        }
        if validate_bearer(headers, state.config.auth_token.as_deref()).is_ok() {
            if let Some(token) = extract_bearer(headers) {
                return Some(WsAuthenticated::Token(token_fingerprint(&token)));
            }
        }
    }

    if state.oidc_verifier.is_some() {
        if let Ok(claims) = try_oidc(state, headers).await {
            if let Some(token) = extract_bearer(headers) {
                return Some(WsAuthenticated::Oidc(
                    token_fingerprint(&token),
                    SessionScope::from_claims(&claims),
                ));
            }
        }
    }
    None
}

/// Try OIDC JWT verification if a verifier is configured, returning the
/// token's role and group claims.
pub async fn try_oidc(state: &MuxState, headers: &HeaderMap) -> Result<OidcClaims, MuxError> {
    let verifier = match state.oidc_verifier {
        Some(ref v) => v,
        None => return Err(MuxError::Unauthorized),
    };
    let token = extract_bearer(headers).ok_or(MuxError::Unauthorized)?;
    verifier.verify(&token).await
}

/// Axum middleware that enforces Bearer token authentication.
///
/// Exempt: `/api/v1/health`, `/metrics`, and WebSocket upgrades (`/ws/`).
/// Tries a land token, then the static token, then OIDC JWT if configured.
pub async fn auth_layer(
    state: State<Arc<MuxState>>,
    req: Request<axum::body::Body>,
    next: Next,
) -> Response {
    let path = req.uri().path();
    let method = req.method().as_str();

    // Health and metrics skip all auth. WS and SSE skip legacy bearer auth,
    // but an edge envelope is still role-checked below before the handler can
    // accept a mutation-capable connection.
    // WS/SSE auth is handled via query param in their handlers (browsers can
    // set neither an `Authorization` header on `new WebSocket()` nor on the
    // `EventSource` GET stream, so both authenticate via `?token=`/edge-auth).
    //
    // `/metrics` is scraped by vmagent directly at the pod (job
    // `kubernetes-pods`, no edge-auth in that path) with no Authorization
    // header. coopmux always runs with COOP_MUX_AUTH_TOKEN set and
    // credentials present, so without this exemption every scrape 401s and
    // `up{component="coopmux"}` reads 0 forever (ka-0000000370). The handler
    // exposes only aggregate edge-auth verify counters — no secrets — and the
    // port is bound network-internal, so unauthenticated scrape is safe.
    if path == "/api/v1/health" || path == "/metrics" {
        return next.run(req).await;
    }

    // Edge-auth: if the upstream edge-auth shim verified an HMAC envelope
    // (or preserved read compatibility during burn-in), it stamped a trusted
    // identity on the request. Apply the route matrix before skipping legacy
    // Bearer/OIDC validation.
    if let Some(identity) = req.extensions().get::<crate::transport::edge_auth::TrustedOperator>() {
        let required = edge_required_role(method, path);
        if !identity.has_minimum_role(required) {
            tracing::warn!(
                operator = %identity.operator,
                audit_id = %identity.audit_id,
                auth_mode = %identity.auth_mode,
                roles = ?identity.roles,
                method = %method,
                path = %path,
                required_role = %required,
                "edge-auth authorization denied",
            );
            return (
                StatusCode::FORBIDDEN,
                axum::Json(serde_json::json!({
                    "error": "insufficient_permissions",
                    "required_role": required,
                })),
            )
                .into_response();
        }
        return next.run(req).await;
    }

    // Browser WS/SSE callers authenticate inside their handlers because they
    // cannot set Authorization on WebSocket/EventSource constructors.
    if path.starts_with("/ws/") || path.starts_with("/sse/") {
        return next.run(req).await;
    }

    // No auth configured — fail-closed when credentials are present.
    if state.config.auth_token.is_none()
        && state.oidc_verifier.is_none()
        && state.land_token_key.is_none()
    {
        // In dev mode or when no credentials exist, allow unauthenticated access.
        if state.config.dev_mode || !state.credential_broker.has_accounts().await {
            return next.run(req).await;
        }
        // Credentials present but no auth mechanism configured — deny access.
        tracing::warn!(
            path = %path,
            "fail-closed: credentials present but no auth configured; \
             set COOP_MUX_AUTH_TOKEN, configure OIDC, or enable COOP_DEV_MODE=1"
        );
        let code = MuxError::Unauthorized;
        let body = crate::error::ErrorResponse {
            error: code.to_error_body(
                "authentication required: credentials present but no auth mechanism configured",
            ),
        };
        return (
            StatusCode::from_u16(code.http_status()).unwrap_or(StatusCode::UNAUTHORIZED),
            axum::Json(body),
        )
            .into_response();
    }

    // A land token (gd-zVTBvaSIdG) is bound to its land: it has a land
    // member's scope, plus registering and deregistering its own land's
    // sessions. The principal rides along so `register_session` binds the
    // session to the land. Checked before the static token, which admits
    // everything when it is unset.
    if let Some(principal) = land_principal(&state, req.headers()) {
        let scope = principal.scope();
        if let Some(denied) = land_denial(&state, &scope, method, path).await {
            return denied;
        }
        let mut req = req;
        req.extensions_mut().insert(scope);
        req.extensions_mut().insert(principal);
        return next.run(req).await;
    }

    // Try static token first (fast, constant-time).
    if validate_bearer(req.headers(), state.config.auth_token.as_deref()).is_ok() {
        return next.run(req).await;
    }

    // Fall back to OIDC JWT verification. A land-scoped token is confined to
    // its lands' sessions before the handler runs; the scope rides along as a
    // request extension so `list_sessions` can filter.
    if let Ok(claims) = try_oidc(&state, req.headers()).await {
        let scope = SessionScope::from_claims(&claims);
        if scope.is_scoped() {
            if let Some(denied) = scoped_denial(&state, &scope, method, path).await {
                return denied;
            }
        }
        let mut req = req;
        req.extensions_mut().insert(scope);
        return next.run(req).await;
    }

    let code = MuxError::Unauthorized;
    let body = crate::error::ErrorResponse { error: code.to_error_body("unauthorized") };
    (StatusCode::from_u16(code.http_status()).unwrap_or(StatusCode::UNAUTHORIZED), axum::Json(body))
        .into_response()
}

/// The land principal of a request whose bearer is a valid land token.
fn land_principal(state: &MuxState, headers: &HeaderMap) -> Option<LandPrincipal> {
    let key = state.land_token_key.as_ref()?;
    key.verify(&extract_bearer(headers)?)
}

/// Refuse a land principal's request that leaves its land or its tier.
///
/// On top of what a land OIDC member may do ([`scoped_denial`]), a land
/// principal may register a session (the handler binds it to the land) and
/// deregister one of its land's sessions.
async fn land_denial(
    state: &MuxState,
    scope: &SessionScope,
    method: &str,
    path: &str,
) -> Option<Response> {
    if path == "/api/v1/sessions" && method.eq_ignore_ascii_case("POST") {
        return None;
    }
    if method.eq_ignore_ascii_case("DELETE") {
        if let Some(id) = path.strip_prefix("/api/v1/sessions/").filter(|id| !id.contains('/')) {
            if scope.allows_session(state, id).await {
                return None;
            }
            return Some(
                MuxError::SessionNotFound.to_http_response("session not found").into_response(),
            );
        }
    }
    scoped_denial(state, scope, method, path).await
}

/// Refuse a land-scoped OIDC request that leaves its lands or its tier.
///
/// Returns `None` when the request may proceed. A route outside the scoped set
/// or above the operator tier is 403; a session outside the caller's lands is
/// 404, the same answer an unknown id gets.
async fn scoped_denial(
    state: &MuxState,
    scope: &SessionScope,
    method: &str,
    path: &str,
) -> Option<Response> {
    let required = edge_required_role(method, path);
    let id = match scoped_route(path) {
        Some(ScopedRoute::List) if scope.allows_role(required) => return None,
        Some(ScopedRoute::Session(id)) if scope.allows_role(required) => id,
        _ => {
            tracing::warn!(
                method = %method,
                path = %path,
                required_role = %required,
                "oidc land scope: route denied",
            );
            return Some(
                (
                    StatusCode::FORBIDDEN,
                    axum::Json(serde_json::json!({
                        "error": "insufficient_permissions",
                        "required_role": required,
                    })),
                )
                    .into_response(),
            );
        }
    };
    if scope.allows_session(state, id).await {
        return None;
    }
    tracing::warn!(
        method = %method,
        path = %path,
        "oidc land scope: session outside caller's lands",
    );
    Some(MuxError::SessionNotFound.to_http_response("session not found").into_response())
}

/// Minimum Gardenio role for an edge-authenticated Coopmux route.
///
/// Unknown reads default to viewer; unknown mutations default to admin so a
/// newly added control endpoint cannot silently inherit operator authority.
pub(crate) fn edge_required_role(method: &str, path: &str) -> &'static str {
    const VIEWER: &str = "gardenio-viewer";
    const OPERATOR: &str = "gardenio-operator";
    const ADMIN: &str = "gardenio-admin";

    if path.starts_with("/api/v1/credentials/") || path == "/api/v1/credentials" {
        return ADMIN;
    }

    if path == "/ws/mux" {
        // Subscribe/unsubscribe are viewer operations. Input and resize
        // messages are checked inside the multiplexed feed.
        return VIEWER;
    }
    if path.starts_with("/ws/") {
        // Per-session sockets carry raw upstream control messages, so the
        // upgrade itself is mutation-capable. The finer-grained per-frame tier
        // (admin-reserved raw/signal/shutdown vs operator input/resize) is
        // enforced inside the handler via `edge_session_frame_required_role`.
        return OPERATOR;
    }
    if path == "/sse/mux/send" {
        // The multiplexed send endpoint carries both viewer-safe
        // subscribe/unsubscribe messages and operator-only input/resize
        // messages. The handler decodes the body and applies the finer-grained
        // role check before routing it to the feed.
        return VIEWER;
    }
    if path.starts_with("/sse/") {
        // The per-session send twin (`POST /sse/session/{id}/send`) forwards the
        // same raw control frames as `/ws/{id}`, so the OPERATOR route gate is a
        // floor only — `sse_session_send_handler` applies
        // `edge_session_frame_required_role` per body before forwarding.
        return if method.eq_ignore_ascii_case("GET") { VIEWER } else { OPERATOR };
    }

    if path == "/api/v1/sessions" && method.eq_ignore_ascii_case("POST") {
        return ADMIN;
    }
    if path == "/api/v1/sessions/launch" && method.eq_ignore_ascii_case("POST") {
        return ADMIN;
    }
    if path.starts_with("/api/v1/sessions/") {
        if method.eq_ignore_ascii_case("DELETE") {
            return ADMIN;
        }
        if method.eq_ignore_ascii_case("POST") {
            if path.ends_with("/agent/nudge")
                || path.ends_with("/agent/respond")
                || path.ends_with("/input")
                || path.ends_with("/input/keys")
                || path.ends_with("/input/mouse")
                || path.ends_with("/resize")
            {
                return OPERATOR;
            }
            // Raw bytes and uploads are deliberately held for admin.
            return ADMIN;
        }
    }

    if method.eq_ignore_ascii_case("GET")
        || method.eq_ignore_ascii_case("HEAD")
        || method.eq_ignore_ascii_case("OPTIONS")
    {
        VIEWER
    } else {
        ADMIN
    }
}

/// Minimum Gardenio role for a single control frame carried on a per-session
/// channel (`/ws/{id}` or `POST /sse/session/{id}/send`).
///
/// [`edge_required_role`] only gates the per-session *upgrade* at OPERATOR, but
/// each frame is forwarded verbatim to the upstream coop `/ws`, which executes
/// admin-reserved actions: raw byte injection into the PTY (`input:send:raw`),
/// signals incl. SIGKILL (`signal:send`), and lifecycle control (`shutdown`,
/// `session:restart`, `session:switch`). The REST matrix deliberately reserves
/// the equivalents for ADMIN (`/input/raw`, and there is no REST route at all
/// for signal/shutdown/restart/switch), so we mirror that tier boundary here
/// per frame — the same pattern the multiplexed feed uses via
/// [`crate::transport::ws_mux::MuxClientMessage::required_edge_role`]. Plain
/// terminal input and resize stay OPERATOR; read-only queries are VIEWER; any
/// unrecognized or config-mutating frame fails closed to ADMIN so a newly added
/// upstream control cannot silently inherit operator authority.
///
/// The per-session protocol is an internally-tagged enum keyed on `event`
/// (`coop/crates/cli/src/transport/ws_msg.rs` `ClientMessage`); we decode only
/// the tag. A body we cannot parse (or that omits `event`) fails closed.
pub(crate) fn edge_session_frame_required_role(body: &str) -> &'static str {
    const VIEWER: &str = "gardenio-viewer";
    const OPERATOR: &str = "gardenio-operator";
    const ADMIN: &str = "gardenio-admin";

    #[derive(serde::Deserialize)]
    struct FrameTag {
        event: Option<String>,
    }

    let event = match serde_json::from_str::<FrameTag>(body) {
        Ok(FrameTag { event: Some(event) }) => event,
        _ => return ADMIN,
    };

    match event.as_str() {
        // Read-only queries and connection keep-alive.
        "health:get" | "ready:get" | "screen:get" | "replay:get" | "status:get" | "agent:get"
        | "stop:config:get" | "config:start:get" | "transcript:list" | "transcript:get"
        | "transcript:catchup" | "recording:get" | "recording:catchup" | "usage:get"
        | "profiles:list" | "profiles:mode" | "ping" => VIEWER,

        // Operator terminal actions — mirror the REST matrix, where `/input`,
        // `/input/keys`, `/input/mouse`, `/agent/nudge`, and `/agent/respond`
        // are OPERATOR. A mouse frame names a button and a cell; coop builds the
        // report bytes itself, so it is not raw input.
        "input:send" | "keys:send" | "mouse:send" | "resize" | "nudge" | "respond"
        | "stop:resolve" => OPERATOR,

        // Admin-reserved: raw byte injection, signals (incl. SIGKILL), and
        // lifecycle control. The REST matrix holds `/input/raw` for admin and
        // exposes no route at all for signal/shutdown/restart/switch.
        "input:send:raw" | "signal:send" | "shutdown" | "session:restart" | "session:switch" => {
            ADMIN
        }

        // Unknown or config-mutating frames (recording:put, profiles:register,
        // profiles:mode:set, config:put:get, stop:config:put, auth, …) fail
        // closed to ADMIN.
        _ => ADMIN,
    }
}

#[cfg(test)]
#[path = "auth_tests.rs"]
mod tests;
