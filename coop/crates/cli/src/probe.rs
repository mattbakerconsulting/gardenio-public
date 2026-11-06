// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Credential probing for profile validation.
//!
//! Provides lightweight, non-destructive validation of OAuth tokens and API
//! keys without triggering rate limits or incurring charges. Used by the
//! profile rotation system to verify credentials before or after switching.

use std::collections::HashMap;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tracing::{debug, warn};

/// Timeout for probe HTTP requests.
const PROBE_TIMEOUT: Duration = Duration::from_secs(10);

/// The real Anthropic API base URL. OAuth tokens are always validated directly
/// against Anthropic, not through proxy endpoints (LiteLLM, etc.) which don't
/// understand Bearer OAuth authentication.
const ANTHROPIC_API_BASE: &str = "https://api.anthropic.com";

/// ChatGPT backend usage route Codex itself reads for `/status` [BIN]. It is an
/// internal route that may change shape, so every failure here fails open.
const CODEX_USAGE_URL: &str = "https://chatgpt.com/backend-api/wham/usage";

use crate::driver::codex::auth::AUTH_ENV;

/// Result of probing a single credential set.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum ProbeStatus {
    /// Credential is valid and ready for use.
    Valid,
    /// Credential is invalid or expired (401/403).
    Invalid,
    /// Probe encountered a transient error (timeout, DNS, 5xx).
    Error,
    /// No probeable credential found in the profile.
    Skipped,
}

/// Subscription usage read alongside a probe, in the controller's account-bead
/// vocabulary (`utilization_5h|7d`, `throttle_reset_5h|7d_at`,
/// `representative_claim`, `throttle_status`) so a Codex pool maps onto the
/// fields `selectAccount` already consumes (ADR 0004 §5).
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct ProbeUsage {
    /// Primary (5h) window usage as a fraction, `used_percent / 100`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub utilization_5h: Option<f64>,
    /// Secondary (weekly) window usage as a fraction.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub utilization_7d: Option<f64>,
    /// Epoch seconds at which the primary window resets.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub throttle_reset_5h_at: Option<u64>,
    /// Epoch seconds at which the secondary window resets.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub throttle_reset_7d_at: Option<u64>,
    /// The binding window: `five_hour` or `seven_day`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub representative_claim: Option<String>,
    /// `rejected` when the plan reports its limit reached, else `allowed`.
    pub throttle_status: String,
}

/// Full probe result for a profile.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ProbeResult {
    pub profile: String,
    pub status: ProbeStatus,
    /// Credential type that was probed (e.g. "oauth", "api_key").
    #[serde(skip_serializing_if = "Option::is_none")]
    pub credential_type: Option<String>,
    /// Human-readable detail about the probe outcome.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
    /// Subscription usage, when the provider exposes it (Codex `wham/usage`).
    /// Absent whenever the read failed: a missing read must leave the account
    /// eligible, never condemn it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<ProbeUsage>,
}

/// Detect the credential type from a profile's env-var map and probe it.
///
/// Probing strategy:
/// - `CLAUDE_CODE_OAUTH_TOKEN`: Hits the Anthropic `/v1/models` endpoint
///   with a Bearer token to verify the OAuth credential is valid.
/// - `ANTHROPIC_API_KEY`: Hits the Anthropic `/v1/models` endpoint with
///   the `x-api-key` header to verify the API key.
///
/// - `CODEX_AUTH_JSON`: reads the ChatGPT backend `wham/usage` endpoint with the
///   document's access token, which both validates the login and returns the
///   plan's 5h / weekly windows (see [`probe_codex_auth`]).
///
/// All endpoints are read-only and do not incur charges or trigger
/// inference rate limits.
pub async fn probe_credentials(
    profile_name: &str,
    credentials: &HashMap<String, String>,
) -> ProbeResult {
    // A Codex profile carries its login as an auth document, not a bearer env.
    if let Some(doc) = credentials.get(AUTH_ENV).filter(|d| !d.is_empty()) {
        return probe_codex_auth(profile_name, doc).await;
    }

    // Try OAuth token first (preferred).
    if let Some(token) = credentials.get("CLAUDE_CODE_OAUTH_TOKEN").filter(|t| !t.is_empty()) {
        return probe_oauth_token(profile_name, token).await;
    }

    // Fall back to API key.
    if let Some(key) = credentials.get("ANTHROPIC_API_KEY").filter(|k| !k.is_empty()) {
        return probe_api_key(profile_name, key).await;
    }

    ProbeResult {
        profile: profile_name.to_owned(),
        status: ProbeStatus::Skipped,
        credential_type: None,
        detail: Some("no CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY in profile".to_owned()),
        usage: None,
    }
}

/// Probe an OAuth token by calling the Anthropic models endpoint directly.
///
/// Uses `Authorization: Bearer <token>` which is the standard OAuth
/// authentication method. The `/v1/models` endpoint is lightweight,
/// returns quickly, and does not consume credits.
///
/// **Important**: OAuth tokens are always probed against the real Anthropic
/// API, not `ANTHROPIC_BASE_URL`. When `ANTHROPIC_BASE_URL` points to a
/// LiteLLM proxy, the proxy doesn't understand OAuth Bearer tokens — it
/// uses its own API keys. Probing an OAuth token through LiteLLM would
/// always fail, giving a false Invalid result. Use `ANTHROPIC_OAUTH_BASE_URL`
/// to override the direct endpoint if needed.
async fn probe_oauth_token(profile_name: &str, token: &str) -> ProbeResult {
    let base_url = oauth_base_url();
    let url = format!("{base_url}/v1/models?limit=1");

    debug!(profile = profile_name, "probing OAuth token via {url}");

    let client = match reqwest::Client::builder()
        .user_agent(concat!("coop/", env!("CARGO_PKG_VERSION")))
        .timeout(PROBE_TIMEOUT)
        .build()
    {
        Ok(c) => c,
        Err(e) => {
            return ProbeResult {
                profile: profile_name.to_owned(),
                status: ProbeStatus::Error,
                credential_type: Some("oauth".to_owned()),
                detail: Some(format!("failed to build HTTP client: {e}")),
                usage: None,
            };
        }
    };

    let resp = client
        .get(&url)
        .header("authorization", format!("Bearer {token}"))
        .header("anthropic-version", "2023-06-01")
        .send()
        .await;

    classify_response(profile_name, "oauth", resp)
}

/// Probe an API key by calling the Anthropic models endpoint.
///
/// Uses `x-api-key: <key>` header for API key authentication.
async fn probe_api_key(profile_name: &str, key: &str) -> ProbeResult {
    let base_url = api_base_url();
    let url = format!("{base_url}/v1/models?limit=1");

    debug!(profile = profile_name, "probing API key via {url}");

    let client = match reqwest::Client::builder()
        .user_agent(concat!("coop/", env!("CARGO_PKG_VERSION")))
        .timeout(PROBE_TIMEOUT)
        .build()
    {
        Ok(c) => c,
        Err(e) => {
            return ProbeResult {
                profile: profile_name.to_owned(),
                status: ProbeStatus::Error,
                credential_type: Some("api_key".to_owned()),
                detail: Some(format!("failed to build HTTP client: {e}")),
                usage: None,
            };
        }
    };

    let resp = client
        .get(&url)
        .header("x-api-key", key)
        .header("anthropic-version", "2023-06-01")
        .send()
        .await;

    classify_response(profile_name, "api_key", resp)
}

/// Classify an HTTP response into a ProbeResult.
fn classify_response(
    profile_name: &str,
    cred_type: &str,
    resp: Result<reqwest::Response, reqwest::Error>,
) -> ProbeResult {
    match resp {
        Ok(r) => {
            let status_code = r.status().as_u16();
            debug!(profile = profile_name, status = status_code, "probe response");

            if r.status().is_success() {
                ProbeResult {
                    profile: profile_name.to_owned(),
                    status: ProbeStatus::Valid,
                    credential_type: Some(cred_type.to_owned()),
                    detail: None,
                    usage: None,
                }
            } else if status_code == 401 || status_code == 403 {
                ProbeResult {
                    profile: profile_name.to_owned(),
                    status: ProbeStatus::Invalid,
                    credential_type: Some(cred_type.to_owned()),
                    detail: Some(format!("authentication failed (HTTP {status_code})")),
                    usage: None,
                }
            } else if status_code == 429 {
                // Rate limited during probe — treat as valid (credential works,
                // just rate-limited). This avoids marking a working credential
                // as invalid just because the account is temporarily throttled.
                ProbeResult {
                    profile: profile_name.to_owned(),
                    status: ProbeStatus::Valid,
                    credential_type: Some(cred_type.to_owned()),
                    detail: Some("rate limited during probe (credential is valid)".to_owned()),
                    usage: None,
                }
            } else {
                warn!(
                    profile = profile_name,
                    status = status_code,
                    "unexpected status during probe"
                );
                ProbeResult {
                    profile: profile_name.to_owned(),
                    status: ProbeStatus::Error,
                    credential_type: Some(cred_type.to_owned()),
                    detail: Some(format!("unexpected HTTP {status_code}")),
                    usage: None,
                }
            }
        }
        Err(e) => {
            let detail = if e.is_timeout() {
                "request timed out".to_owned()
            } else if e.is_connect() {
                "connection failed".to_owned()
            } else {
                format!("request failed: {e}")
            };
            warn!(profile = profile_name, error = %e, "probe request failed");
            ProbeResult {
                profile: profile_name.to_owned(),
                status: ProbeStatus::Error,
                credential_type: Some(cred_type.to_owned()),
                detail: Some(detail),
                usage: None,
            }
        }
    }
}

/// Resolve the API base URL for API key probing.
///
/// Respects `ANTHROPIC_BASE_URL` because API keys may belong to a LiteLLM
/// proxy or other intermediary that sits at that URL.
fn api_base_url() -> String {
    std::env::var("ANTHROPIC_BASE_URL").unwrap_or_else(|_| ANTHROPIC_API_BASE.to_owned())
}

/// Resolve the base URL for OAuth token probing.
///
/// Defaults to the real Anthropic API (`https://api.anthropic.com`),
/// ignoring `ANTHROPIC_BASE_URL` which typically points to a LiteLLM
/// proxy that doesn't understand OAuth Bearer tokens. Override with
/// `ANTHROPIC_OAUTH_BASE_URL` for testing or custom deployments.
fn oauth_base_url() -> String {
    std::env::var("ANTHROPIC_OAUTH_BASE_URL").unwrap_or_else(|_| ANTHROPIC_API_BASE.to_owned())
}

// -- Codex (ChatGPT subscription) ---------------------------------------------

/// Override for the usage endpoint, for tests and non-default deployments.
fn codex_usage_url() -> String {
    std::env::var("CODEX_USAGE_URL").unwrap_or_else(|_| CODEX_USAGE_URL.to_owned())
}

/// Probe a Codex `auth.json` document by reading `wham/usage`.
///
/// - 2xx → `Valid`, with [`ProbeUsage`] when the body parses.
/// - 401/403 → `Invalid` (the access token or the login is dead).
/// - 429 → `Valid` (the login works, the plan is just limited).
/// - Anything else, including an unparseable 2xx body, is inconclusive:
///   `Error` carries no usage and callers treat it as passing, so a route
///   change or outage never strands a session.
async fn probe_codex_auth(profile_name: &str, document: &str) -> ProbeResult {
    let result = |status, detail: Option<String>, usage| ProbeResult {
        profile: profile_name.to_owned(),
        status,
        credential_type: Some("codex_chatgpt".to_owned()),
        detail,
        usage,
    };

    let doc: serde_json::Value = match serde_json::from_str(document) {
        Ok(d) => d,
        Err(_) => {
            return result(
                ProbeStatus::Skipped,
                Some("CODEX_AUTH_JSON is not a JSON document".to_owned()),
                None,
            )
        }
    };
    let Some(access) = doc.pointer("/tokens/access_token").and_then(|t| t.as_str()) else {
        return result(
            ProbeStatus::Skipped,
            Some("auth document has no access_token".into()),
            None,
        );
    };
    let account_id = doc.pointer("/tokens/account_id").and_then(|t| t.as_str());

    let url = codex_usage_url();
    debug!(profile = profile_name, "probing codex login via {url}");

    let client = match reqwest::Client::builder()
        .user_agent(concat!("coop/", env!("CARGO_PKG_VERSION")))
        .timeout(PROBE_TIMEOUT)
        .build()
    {
        Ok(c) => c,
        Err(e) => {
            return result(
                ProbeStatus::Error,
                Some(format!("failed to build HTTP client: {e}")),
                None,
            )
        }
    };
    let mut req = client.get(&url).header("authorization", format!("Bearer {access}"));
    if let Some(id) = account_id {
        req = req.header("chatgpt-account-id", id);
    }

    let resp = match req.send().await {
        Ok(r) => r,
        Err(e) => {
            warn!(profile = profile_name, error = %e, "codex usage probe failed");
            let detail = if e.is_timeout() { "request timed out" } else { "request failed" };
            return result(ProbeStatus::Error, Some(detail.to_owned()), None);
        }
    };
    let code = resp.status().as_u16();
    match code {
        401 | 403 => {
            result(ProbeStatus::Invalid, Some(format!("authentication failed (HTTP {code})")), None)
        }
        429 => result(
            ProbeStatus::Valid,
            Some("rate limited during probe (credential is valid)".to_owned()),
            None,
        ),
        200..=299 => match resp.json::<serde_json::Value>().await {
            Ok(body) => result(ProbeStatus::Valid, None, parse_wham_usage(&body, now_secs())),
            Err(e) => {
                warn!(profile = profile_name, error = %e, "codex usage body unreadable");
                result(ProbeStatus::Valid, Some("usage body unreadable".to_owned()), None)
            }
        },
        _ => {
            warn!(profile = profile_name, status = code, "unexpected status during codex probe");
            result(ProbeStatus::Error, Some(format!("unexpected HTTP {code}")), None)
        }
    }
}

fn now_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// One `wham/usage` rate-limit window, normalised.
struct Window {
    used_fraction: Option<f64>,
    resets_at: Option<u64>,
}

fn parse_window(w: Option<&serde_json::Value>, now: u64) -> Option<Window> {
    let w = w?.as_object()?;
    let used_fraction =
        w.get("used_percent").and_then(|v| v.as_f64()).map(|p| (p / 100.0).max(0.0));
    let resets_at = w
        .get("reset_at")
        .or_else(|| w.get("resets_at"))
        .and_then(|v| v.as_u64())
        .or_else(|| w.get("reset_after_seconds").and_then(|v| v.as_u64()).map(|s| now + s));
    Some(Window { used_fraction, resets_at })
}

/// Map a `wham/usage` body onto [`ProbeUsage`].
///
/// The route is internal and its field names come from the Codex binary rather
/// than a published schema (ADR 0004 §10), so this is deliberately tolerant:
/// the windows may sit under `rate_limit` or at the top level, and a body with
/// no recognisable window yields `None` (fail open), never an error.
pub fn parse_wham_usage(body: &serde_json::Value, now: u64) -> Option<ProbeUsage> {
    let root = body.get("rate_limit").filter(|v| v.is_object()).unwrap_or(body);
    let primary = parse_window(root.get("primary_window"), now);
    let secondary = parse_window(root.get("secondary_window"), now);
    if primary.is_none() && secondary.is_none() {
        return None;
    }

    let limit_reached = root.get("limit_reached").and_then(|v| v.as_bool()).unwrap_or(false)
        || root.get("allowed").and_then(|v| v.as_bool()) == Some(false);

    let u5 = primary.as_ref().and_then(|w| w.used_fraction);
    let u7 = secondary.as_ref().and_then(|w| w.used_fraction);
    // The binding window is whichever is fuller; ties go to the shorter window.
    let representative_claim = match (u5, u7) {
        (Some(a), Some(b)) if b > a => Some("seven_day"),
        (Some(_), _) => Some("five_hour"),
        (None, Some(_)) => Some("seven_day"),
        (None, None) => None,
    };

    Some(ProbeUsage {
        utilization_5h: u5,
        utilization_7d: u7,
        throttle_reset_5h_at: primary.and_then(|w| w.resets_at),
        throttle_reset_7d_at: secondary.and_then(|w| w.resets_at),
        representative_claim: representative_claim.map(str::to_owned),
        throttle_status: if limit_reached { "rejected" } else { "allowed" }.to_owned(),
    })
}

#[cfg(test)]
#[path = "probe_tests.rs"]
mod tests;
