// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Codex ChatGPT-subscription credentials (ADR 0004 §5).
//!
//! A `provider=openai` account does not carry a bearer token: its credential is
//! the `auth.json` document `codex login` writes (`auth_mode: "chatgpt"`,
//! `tokens{id_token, access_token, refresh_token, account_id}`,
//! `last_refresh`). The controller's refresh keeper is the single writer of the
//! real refresh token and pushes the *session* form of that document, so a pod
//! never holds a usable refresh token. This module is the broker-side half of
//! that contract:
//!
//! - [`normalize`] validates a pushed document and rewrites `refresh_token` to
//!   [`PLACEHOLDER_REFRESH_TOKEN`] even if the caller sent a real one
//!   (defence in depth: coopmux fans the document out to every pod) and drops
//!   any top-level `OPENAI_API_KEY`.
//! - It reads the access token's JWT `exp` so the broker knows when the
//!   credential rotates, which is what makes Codex the first rotating
//!   credential in the fleet.

use base64::Engine;
use serde_json::Value;

/// Value every pod-facing document carries in place of the real refresh token.
pub const PLACEHOLDER_REFRESH_TOKEN: &str = "gardenio-broker-managed";

/// A validated, pod-safe Codex auth document.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CodexAuth {
    /// Canonical JSON text, refresh token replaced by the placeholder.
    pub document: String,
    /// ChatGPT account id (`tokens.account_id`), when present.
    pub account_id: Option<String>,
    /// Access-token expiry, epoch seconds, read from the JWT `exp` claim.
    pub access_expires_at: Option<u64>,
}

/// Validate `raw` as a ChatGPT-subscription `auth.json` and make it pod-safe.
pub fn normalize(raw: &str) -> Result<CodexAuth, String> {
    let mut doc: Value =
        serde_json::from_str(raw).map_err(|e| format!("codex auth document is not JSON: {e}"))?;
    let obj = doc.as_object_mut().ok_or("codex auth document must be a JSON object")?;

    // `codex login` writes auth_mode; older files omit it. Only a *different*
    // explicit mode (e.g. "apikey") is rejected: no OPENAI_API_KEY passthrough.
    match obj.get("auth_mode").and_then(Value::as_str) {
        None | Some("chatgpt") | Some("chatgptAuthTokens") => {}
        Some(other) => {
            return Err(format!("codex auth_mode {other:?} is not supported (expected chatgpt)"))
        }
    }

    // `codex login` writes `"OPENAI_API_KEY": null` in chatgpt mode; a populated
    // key must never reach a pod (subscription auth only, no API-key
    // passthrough), whether or not auth_mode is present. Drop the field.
    obj.remove("OPENAI_API_KEY");

    let tokens = obj
        .get_mut("tokens")
        .and_then(Value::as_object_mut)
        .ok_or("codex auth document has no tokens object")?;
    let access = tokens
        .get("access_token")
        .and_then(Value::as_str)
        .filter(|t| !t.is_empty())
        .ok_or("codex auth document has no tokens.access_token")?
        .to_owned();
    let account_id = tokens.get("account_id").and_then(Value::as_str).map(str::to_owned);
    tokens.insert("refresh_token".to_owned(), Value::String(PLACEHOLDER_REFRESH_TOKEN.to_owned()));

    let document = serde_json::to_string(&doc).map_err(|e| e.to_string())?;
    Ok(CodexAuth { document, account_id, access_expires_at: jwt_exp(&access) })
}

/// Extract the access token from a (normalized) auth document.
pub fn access_token(document: &str) -> Option<String> {
    let doc: Value = serde_json::from_str(document).ok()?;
    doc.pointer("/tokens/access_token").and_then(Value::as_str).map(str::to_owned)
}

/// Read the `exp` claim of a JWT without verifying it (we only schedule on it).
fn jwt_exp(jwt: &str) -> Option<u64> {
    let payload = jwt.split('.').nth(1)?;
    let bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD.decode(payload).ok()?;
    serde_json::from_slice::<Value>(&bytes).ok()?.get("exp")?.as_u64()
}

#[cfg(test)]
#[path = "codex_auth_tests.rs"]
mod tests;
