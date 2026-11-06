// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Daemon-side edge-auth HMAC verification (Stage 3 / W-B-11, ka-0000001678).
//!
//! Rust port of `kbeads/internal/edgeauth/verify.go`. Same wire shape:
//!
//! ```text
//! X-Operator        : <email>           // operator identity
//! X-Auth-Audit-Id   : <ulid>            // correlation id
//! X-Edge-Auth-Ts    : <unix seconds>    // freshness anchor, ±MaxSkew window
//! X-Edge-Auth-Sig   : <base64 v1 hmac>
//! ```
//!
//! Version 2 additionally carries `X-Edge-Auth-Version`,
//! `X-Edge-Auth-Mode`, and `X-Edge-Auth-Roles`. Its
//! `X-Edge-Auth-Sig-V2` signs:
//!
//! `v2\noperator\nauthMode\nroles\nauditID\nMETHOD\npath\nts`
//!
//! Safe-deploy stance: when `DAEMON_HMAC_KEY` is set and
//! `DAEMON_HMAC_VERIFY_ENFORCE` is unset/false, the helper still computes
//! the outcome (so we log + count) but reports `trusted = true` so the
//! pre-edge-auth attribution path is unchanged. Flipping enforce=true is
//! gated on a clean ≥7d burn-in per env (runbook ka-0000000026).
//!
//! A third key, `DAEMON_HMAC_KEY_RELAY`, verifies the beads-viewer Glass
//! relay's v2 envelope on its `GET /ws/...` watch upgrade only, and the
//! identity it yields cannot send anything above the viewer tier
//! (gd-9ZGh01W5ub).

use std::collections::HashMap;
use std::fmt;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use axum::body::Body;
use axum::extract::State;
use axum::http::{HeaderMap, HeaderValue, Request};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine as _;
use ring::hmac;

use crate::state::MuxState;

/// Header names recognised on inbound requests. Kept in sync with
/// `kbeads/internal/edgeauth/verify.go` and
/// `gardenio/controller/cmd/edge-auth/proxy.go`.
pub const HEADER_OPERATOR: &str = "X-Operator";
pub const HEADER_AUDIT_ID: &str = "X-Auth-Audit-Id";
pub const HEADER_SIGNATURE: &str = "X-Edge-Auth-Sig";
pub const HEADER_SIGNATURE_V2: &str = "X-Edge-Auth-Sig-V2";
pub const HEADER_TIMESTAMP: &str = "X-Edge-Auth-Ts";
pub const HEADER_VERSION: &str = "X-Edge-Auth-Version";
pub const HEADER_AUTH_MODE: &str = "X-Edge-Auth-Mode";
pub const HEADER_ROLES: &str = "X-Edge-Auth-Roles";

/// Outcome categorisation emitted as a Prometheus label.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Outcome {
    Ok,
    NoOperator,
    NoSig,
    Stale,
    Mismatch,
    NoKey,
    Malformed,
}

impl Outcome {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Ok => "ok",
            Self::NoOperator => "no_operator",
            Self::NoSig => "no_sig",
            Self::Stale => "stale",
            Self::Mismatch => "mismatch",
            Self::NoKey => "no_key",
            Self::Malformed => "malformed",
        }
    }

    /// Iterate over all variants for metric enumeration.
    pub fn all() -> &'static [Outcome] {
        &[
            Self::Ok,
            Self::NoOperator,
            Self::NoSig,
            Self::Stale,
            Self::Mismatch,
            Self::NoKey,
            Self::Malformed,
        ]
    }
}

impl fmt::Display for Outcome {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Default clock-skew window (±5 minutes) matching kbeads.
const DEFAULT_MAX_SKEW: Duration = Duration::from_secs(5 * 60);

/// Function-pointer clock seam for tests. `None` → `SystemTime::now()`.
pub type NowFn = fn() -> SystemTime;

/// Daemon-side knobs.
///
/// Built from env via [`Config::from_env`] in production; test fixtures
/// construct it directly.
#[derive(Clone)]
pub struct Config {
    /// Shared HMAC secret. Empty key short-circuits verification with
    /// [`Outcome::NoKey`].
    pub hmac_key: Vec<u8>,
    /// Key v2 (role-bearing) envelopes are signed with (`DAEMON_HMAC_KEY_V2`,
    /// `EDGE_AUTH_HMAC_KEY_V2` on the edge). Split from `hmac_key` so a holder
    /// of the v1 key alone cannot mint a v2 admin envelope (gd-dMof1PW1oJ).
    /// Empty falls back to `hmac_key`, the pre-split behaviour.
    pub hmac_key_v2: Vec<u8>,
    /// Key the beads-viewer Glass relay signs its v2 envelope with
    /// (`DAEMON_HMAC_KEY_RELAY`, `COOPMUX_RELAY_HMAC_KEY` on the viewer). Only
    /// coopmux holds it, never the beads daemon, so the internet-facing viewer
    /// never holds a key the daemon trusts (ruling gd-9ZGh01W5ub). It verifies a
    /// v2 envelope only on the relay's watch upgrade ([`relay_route`]), and the
    /// identity it yields watches only ([`TrustedOperator::may_send`]). No
    /// fallback: empty accepts no relay envelope.
    pub hmac_key_relay: Vec<u8>,
    /// When `false` (default safe-deploy stance), verify computes the
    /// outcome so the daemon can log + count, but reports `trusted = true`
    /// for the burn-in. When `true`, only [`Outcome::Ok`] is trusted.
    pub enforce: bool,
    /// Largest accepted clock drift between edge-auth and the daemon.
    /// Defaults to 5 minutes when zero.
    pub max_skew: Duration,
    /// Clock injection seam for tests.
    pub now: Option<NowFn>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            hmac_key: Vec::new(),
            hmac_key_v2: Vec::new(),
            hmac_key_relay: Vec::new(),
            enforce: false,
            max_skew: Duration::ZERO,
            now: None,
        }
    }
}

impl Config {
    /// Build a config from the standard env vars the chart sets on a
    /// daemon Deployment.
    pub fn from_env() -> Self {
        Self {
            hmac_key: std::env::var("DAEMON_HMAC_KEY").unwrap_or_default().into_bytes(),
            hmac_key_v2: std::env::var("DAEMON_HMAC_KEY_V2").unwrap_or_default().into_bytes(),
            hmac_key_relay: std::env::var("DAEMON_HMAC_KEY_RELAY").unwrap_or_default().into_bytes(),
            enforce: env_flag("DAEMON_HMAC_VERIFY_ENFORCE"),
            max_skew: Duration::ZERO,
            now: None,
        }
    }

    /// True once we have at least an HMAC key. Callers use this to decide
    /// whether to install the verify middleware at all.
    pub fn enabled(&self) -> bool {
        !self.hmac_key.is_empty() || !self.hmac_key_v2.is_empty() || !self.hmac_key_relay.is_empty()
    }

    /// The key an envelope of `version` must be signed with: `hmac_key_v2`
    /// for v2 when set, else the shared `hmac_key`.
    fn key_for(&self, version: u8) -> &[u8] {
        if version == 2 && !self.hmac_key_v2.is_empty() {
            &self.hmac_key_v2
        } else {
            &self.hmac_key
        }
    }

    fn skew(&self) -> Duration {
        if self.max_skew.is_zero() {
            DEFAULT_MAX_SKEW
        } else {
            self.max_skew
        }
    }

    fn now(&self) -> SystemTime {
        self.now.map_or_else(SystemTime::now, |f| f())
    }

    /// Inspect the edge-auth headers and return the trust outcome.
    pub fn verify(&self, headers: &HeaderMap, method: &str, path: &str) -> VerifyResult {
        let op = header_str(headers, HEADER_OPERATOR);
        let audit_id = header_str(headers, HEADER_AUDIT_ID);
        let mut sig = header_str(headers, HEADER_SIGNATURE);
        let ts = header_str(headers, HEADER_TIMESTAMP);
        let version_header = header_str(headers, HEADER_VERSION);
        let mut auth_mode = header_str(headers, HEADER_AUTH_MODE);
        let roles_header = header_str(headers, HEADER_ROLES);

        let mut version = 1;
        let mut roles = Vec::new();
        if !version_header.is_empty() {
            if version_header != "2" {
                return self.result(
                    Outcome::Malformed,
                    op,
                    audit_id,
                    version,
                    String::new(),
                    roles,
                );
            }
            version = 2;
            sig = header_str(headers, HEADER_SIGNATURE_V2);
            let Some(parsed_roles) = parse_canonical_roles(&roles_header) else {
                return self.result(Outcome::Malformed, op, audit_id, version, auth_mode, roles);
            };
            if !valid_auth_mode(&auth_mode) {
                return self.result(Outcome::Malformed, op, audit_id, version, auth_mode, roles);
            }
            roles = parsed_roles;
        }

        let mut outcome = self.classify(
            self.key_for(version),
            &op,
            &audit_id,
            &sig,
            &ts,
            method,
            path,
            version,
            &auth_mode,
            &roles_header,
        );
        // The Glass relay's envelope: v2 only, its own key, its watch upgrade only.
        let mut relay = false;
        if version == 2
            && matches!(outcome, Outcome::Mismatch | Outcome::NoKey)
            && !self.hmac_key_relay.is_empty()
            && relay_route(method, path)
            && self.classify(
                &self.hmac_key_relay,
                &op,
                &audit_id,
                &sig,
                &ts,
                method,
                path,
                version,
                &auth_mode,
                &roles_header,
            ) == Outcome::Ok
        {
            outcome = Outcome::Ok;
            relay = true;
        }
        let verified_v2 = outcome == Outcome::Ok && version == 2;
        if !verified_v2 {
            // Burn-in can preserve read compatibility, but unverified request
            // headers must never become an authorization grant.
            auth_mode.clear();
            roles.clear();
        }
        let mut result = self.result(outcome, op, audit_id, version, auth_mode, roles);
        result.verified_v2 = verified_v2;
        result.relay = relay;
        // Additive enforcement (operator decision ka-0000001641): a
        // cryptographically verified v1 envelope keeps its pre-MR authority. A
        // request carrying X-Edge-Auth-Version:2 has version==2 here, so a
        // failed v2 verification is NOT verified_v1 and can never downgrade to
        // v1 authority.
        result.verified_v1 = outcome == Outcome::Ok && version == 1;
        result
    }

    fn result(
        &self,
        outcome: Outcome,
        operator: String,
        audit_id: String,
        version: u8,
        auth_mode: String,
        roles: Vec<String>,
    ) -> VerifyResult {
        let trusted = match outcome {
            Outcome::Ok => true,
            Outcome::NoOperator => false,
            // No key means no way to verify ANY envelope, so an X-Operator header
            // is just a client-supplied string. Never trust it, burn-in or not:
            // burn-in exists to soak real signed edge-auth traffic, and with no
            // key there is none. Trusting it here let any pod that can reach the
            // mux read every session at viewer level with a bare header (gd-yDXrcBd0Uq).
            Outcome::NoKey => false,
            _ => !self.enforce,
        };
        VerifyResult {
            outcome,
            trusted,
            operator,
            audit_id,
            version,
            auth_mode,
            roles,
            verified_v2: false,
            verified_v1: false,
            relay: false,
        }
    }

    // This arg list mirrors `canonical_msg_v2`'s load-bearing signing-string
    // fields (see below); reshaping it while three lanes are live on
    // codex/gardenio-edge-envelope-v2 risks rebase churn for no behavioral gain.
    // TODO(refactor): fold the envelope fields into a borrowed request struct in a dedicated MR.
    #[allow(clippy::too_many_arguments)]
    fn classify(
        &self,
        key_bytes: &[u8],
        op: &str,
        audit_id: &str,
        sig: &str,
        ts: &str,
        method: &str,
        path: &str,
        version: u8,
        auth_mode: &str,
        roles: &str,
    ) -> Outcome {
        if op.is_empty() {
            return Outcome::NoOperator;
        }
        if key_bytes.is_empty() {
            return Outcome::NoKey;
        }
        if sig.is_empty() || ts.is_empty() {
            return Outcome::NoSig;
        }

        let ts_unix: i64 = match ts.trim().parse() {
            Ok(v) => v,
            Err(_) => return Outcome::Stale,
        };
        let now_secs = match self.now().duration_since(UNIX_EPOCH) {
            Ok(d) => d.as_secs() as i64,
            Err(_) => return Outcome::Stale,
        };
        let delta = (now_secs - ts_unix).unsigned_abs();
        if delta > self.skew().as_secs() {
            return Outcome::Stale;
        }

        let got = match B64.decode(sig) {
            Ok(b) => b,
            Err(_) => return Outcome::Mismatch,
        };
        let key = hmac::Key::new(hmac::HMAC_SHA256, key_bytes);
        let msg = if version == 2 {
            canonical_msg_v2(op, auth_mode, roles, audit_id, method, path, ts)
        } else {
            canonical_msg(op, audit_id, path, ts)
        };
        // ring's verify is constant-time and length-checked.
        if hmac::verify(&key, msg.as_bytes(), &got).is_ok() {
            Outcome::Ok
        } else {
            Outcome::Mismatch
        }
    }
}

/// Per-request verification result. `trusted` indicates whether the daemon
/// should honour `X-Operator` on this request.
#[derive(Debug, Clone)]
pub struct VerifyResult {
    pub outcome: Outcome,
    pub trusted: bool,
    pub operator: String,
    pub audit_id: String,
    pub version: u8,
    pub auth_mode: String,
    pub roles: Vec<String>,
    pub verified_v2: bool,
    pub verified_v1: bool,
    /// Verified with the Glass relay key (`hmac_key_relay`): watch only.
    pub relay: bool,
}

/// Marker extension stamped on the request when an envelope verifies (or
/// when we trust it during burn-in). Downstream middleware (`auth_layer`)
/// checks for this and skips Bearer/OIDC validation.
#[derive(Debug, Clone)]
pub struct TrustedOperator {
    pub operator: String,
    pub audit_id: String,
    pub version: u8,
    pub auth_mode: String,
    pub roles: Vec<String>,
    pub verified_v2: bool,
    pub verified_v1: bool,
    /// Verified with the Glass relay key (`hmac_key_relay`): watch only.
    pub relay: bool,
}

impl TrustedOperator {
    /// Additive enforcement (operator decision ka-0000001641):
    ///
    /// * A cryptographically verified v1 envelope keeps EXACTLY its pre-MR
    ///   authority — before this change any verified `TrustedOperator` skipped
    ///   the role gate entirely — so it satisfies every route's role
    ///   requirement. A v1 envelope is HMAC-signed, so anyone who can mint one
    ///   already holds the shared key and could always mutate; this is status
    ///   quo, not a new grant.
    /// * A verified v2 envelope is role-enforced against its canonical roles.
    /// * A burn-in-trusted but unverified envelope (neither v1 nor v2) is
    ///   pinned to the read-only viewer floor, so it can never mutate — the
    ///   operator invariant "burn-in-trusted invalid envelopes must not receive
    ///   legacy mutation authority".
    pub fn has_minimum_role(&self, required: &str) -> bool {
        let required_rank = role_rank(required);
        if required_rank == 0 {
            return false;
        }
        if self.verified_v1 {
            return true;
        }
        if !self.verified_v2 {
            return required_rank <= role_rank("gardenio-viewer");
        }
        self.roles.iter().any(|role| role_rank(role) >= required_rank)
    }

    /// [`Self::has_minimum_role`] for a frame the caller sends upstream on a
    /// socket it already opened. A relay-key identity watches only: it may send
    /// viewer-tier frames (ping, screen and replay queries, mux subscribe) and
    /// nothing that types, resizes, signals or restarts, whatever its roles.
    pub fn may_send(&self, required: &str) -> bool {
        if self.relay && role_rank(required) > role_rank("gardenio-viewer") {
            return false;
        }
        self.has_minimum_role(required)
    }
}

/// The only requests a relay-key envelope can verify on: the Glass relay's
/// `GET /ws/{session}` and `GET /ws/mux` upgrades. Every other route, and any
/// other method, needs the v2 key.
fn relay_route(method: &str, path: &str) -> bool {
    method.eq_ignore_ascii_case("GET")
        && path
            .strip_prefix("/ws/")
            .is_some_and(|session| !session.is_empty() && !session.contains('/'))
}

fn header_str(h: &HeaderMap, key: &str) -> String {
    h.get(key).and_then(|v| v.to_str().ok()).unwrap_or_default().to_owned()
}

fn canonical_msg(operator: &str, audit_id: &str, path: &str, ts: &str) -> String {
    format!("{operator}\n{audit_id}\n{path}\n{ts}")
}

// The positional arg list is the v2 signing string field-for-field (operator,
// auth_mode, roles, audit_id, method, path, ts). That byte layout is a
// cross-language invariant shared with kbeads/internal/edgeauth and
// gardenio/controller/cmd/edge-auth; a signature change here can silently desync
// a verifier without a compile error. So the arity is load-bearing, NOT a
// grab-bag to fold into a struct.
// TODO(refactor): if ever reshaped, do it in one coordinated cross-language MR, never as a lint-only cleanup on this arg count.
#[allow(clippy::too_many_arguments)]
fn canonical_msg_v2(
    operator: &str,
    auth_mode: &str,
    roles: &str,
    audit_id: &str,
    method: &str,
    path: &str,
    ts: &str,
) -> String {
    format!(
        "v2\n{operator}\n{auth_mode}\n{roles}\n{audit_id}\n{}\n{path}\n{ts}",
        method.to_ascii_uppercase()
    )
}

fn parse_canonical_roles(raw: &str) -> Option<Vec<String>> {
    if raw.is_empty() {
        return None;
    }
    let mut roles = Vec::new();
    for role in raw.split(',') {
        if !matches!(role, "gardenio-viewer" | "gardenio-operator" | "gardenio-admin")
            || roles.iter().any(|existing| existing == role)
        {
            return None;
        }
        roles.push(role.to_owned());
    }
    let mut sorted = roles.clone();
    sorted.sort();
    if sorted.join(",") != raw {
        return None;
    }
    Some(roles)
}

fn valid_auth_mode(mode: &str) -> bool {
    matches!(mode, "entra" | "ava" | "pat" | "cookie")
}

fn role_rank(role: &str) -> u8 {
    match role {
        "gardenio-viewer" => 1,
        "gardenio-operator" => 2,
        "gardenio-admin" => 3,
        _ => 0,
    }
}

fn env_flag(key: &str) -> bool {
    matches!(
        std::env::var(key).unwrap_or_default().trim().to_ascii_lowercase().as_str(),
        "1" | "true" | "yes" | "on"
    )
}

/// Prometheus counters keyed by `(outcome, enforce)`.
///
/// Equivalent of kbeads' `edgeAuthVerifyTotal` `CounterVec`. Hand-rolled
/// because the `prometheus` crate isn't in coop's deps; rendered in the
/// Prometheus text format on `/metrics`.
pub struct EdgeAuthMetrics {
    counters: RwLock<HashMap<(Outcome, bool), Arc<AtomicU64>>>,
}

impl Default for EdgeAuthMetrics {
    fn default() -> Self {
        Self::new()
    }
}

impl EdgeAuthMetrics {
    pub fn new() -> Self {
        Self { counters: RwLock::new(HashMap::new()) }
    }

    /// Increment the counter for `(outcome, enforce)`.
    pub fn inc(&self, outcome: Outcome, enforce: bool) {
        if let Some(c) = self.get_existing(outcome, enforce) {
            c.fetch_add(1, Ordering::Relaxed);
            return;
        }
        // Slow path: create the counter.
        let mut w = match self.counters.write() {
            Ok(g) => g,
            Err(p) => p.into_inner(),
        };
        let entry = w.entry((outcome, enforce)).or_insert_with(|| Arc::new(AtomicU64::new(0)));
        entry.fetch_add(1, Ordering::Relaxed);
    }

    fn get_existing(&self, outcome: Outcome, enforce: bool) -> Option<Arc<AtomicU64>> {
        let r = self.counters.read().ok()?;
        r.get(&(outcome, enforce)).cloned()
    }

    /// Snapshot the current counter values.
    pub fn snapshot(&self) -> Vec<(Outcome, bool, u64)> {
        let r = match self.counters.read() {
            Ok(g) => g,
            Err(p) => p.into_inner(),
        };
        let mut out: Vec<_> =
            r.iter().map(|(&(o, e), v)| (o, e, v.load(Ordering::Relaxed))).collect();
        out.sort_by(|a, b| a.0.as_str().cmp(b.0.as_str()).then(a.1.cmp(&b.1)));
        out
    }

    /// Render the counter in Prometheus text format.
    pub fn render_prometheus(&self, buf: &mut String) {
        buf.push_str("# HELP coopmux_edge_auth_verify_total Number of edge-auth HMAC verify attempts on the coopmux daemon, labelled by outcome (ok/no_operator/no_sig/stale/mismatch/no_key/malformed) and enforce mode (true/false). Burn-in counts non-ok outcomes without granting roles; enforce mode rejects.\n");
        buf.push_str("# TYPE coopmux_edge_auth_verify_total counter\n");
        for (outcome, enforce, v) in self.snapshot() {
            buf.push_str(&format!(
                "coopmux_edge_auth_verify_total{{outcome=\"{}\",enforce=\"{}\"}} {}\n",
                outcome.as_str(),
                enforce,
                v
            ));
        }
    }
}

/// Axum middleware that verifies the gardenio-edge-auth HMAC envelope on
/// inbound HTTP requests.
///
/// Behaviour matches `kbeads/internal/server/edge_auth.go`:
///
/// - `/api/v1/health` is exempt to keep readiness probes drama-free.
/// - No `X-Operator` header → pass through to the legacy auth chain.
/// - Verified (or burn-in trusted) → stamp [`TrustedOperator`] on the
///   request extensions so [`crate::transport::auth::auth_layer`] skips
///   Bearer/OIDC validation.
/// - Untrusted in enforce mode → 401.
pub async fn edge_auth_layer(
    state: State<Arc<MuxState>>,
    mut req: Request<Body>,
    next: Next,
) -> Response {
    let path = req.uri().path().to_owned();
    if path == "/api/v1/health" {
        return next.run(req).await;
    }

    // No X-Operator → defer to legacy auth.
    if req.headers().get(HEADER_OPERATOR).is_none() {
        return next.run(req).await;
    }

    let cfg = &state.edge_auth_cfg;
    let method = req.method().as_str().to_owned();
    let result = cfg.verify(req.headers(), &method, &path);
    state.edge_auth_metrics.inc(result.outcome, cfg.enforce);

    if result.outcome == Outcome::Ok {
        tracing::debug!(
            operator = %result.operator,
            audit_id = %result.audit_id,
            enforce = cfg.enforce,
            relay = result.relay,
            "edge-auth verify ok",
        );
    } else {
        tracing::warn!(
            outcome = %result.outcome,
            operator = %result.operator,
            audit_id = %result.audit_id,
            trusted = result.trusted,
            enforce = cfg.enforce,
            "edge-auth verify failed",
        );
    }

    if !result.trusted {
        // Enforce mode + non-OK outcome → reject.
        return (axum::http::StatusCode::UNAUTHORIZED, "edge-auth verification failed")
            .into_response();
    }

    let trusted = TrustedOperator {
        operator: result.operator,
        audit_id: result.audit_id,
        version: result.version,
        auth_mode: result.auth_mode,
        roles: result.roles,
        verified_v2: result.verified_v2,
        verified_v1: result.verified_v1,
        relay: result.relay,
    };
    req.extensions_mut().insert(trusted.clone());

    let mut response = next.run(req).await;
    if result.outcome == Outcome::Ok && !trusted.audit_id.is_empty() {
        if let Ok(value) = HeaderValue::from_str(&trusted.audit_id) {
            response.headers_mut().insert(HEADER_AUDIT_ID, value);
        }
    }
    if crate::transport::auth::edge_required_role(&method, &path) != "gardenio-viewer" {
        tracing::info!(
            operator = %trusted.operator,
            audit_id = %trusted.audit_id,
            auth_mode = %trusted.auth_mode,
            roles = ?trusted.roles,
            method = %method,
            path = %path,
            status = response.status().as_u16(),
            "edge-auth mutation completed",
        );
    }
    response
}

#[cfg(test)]
#[path = "edge_auth_tests.rs"]
mod tests;
