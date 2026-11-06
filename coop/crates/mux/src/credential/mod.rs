// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Credential brokering: API key store with distribution.
//!
//! Always initialized. Accounts are populated dynamically at runtime via the
//! controller's credentials/set API. The `--credential-config` file path is
//! deprecated and will be removed in a future release.
//! Stores API keys for configured accounts and pushes them to coop sessions
//! as profiles.

pub mod broker;
pub mod codex_auth;
pub mod distributor;
pub mod failover;
pub mod land;
pub mod persist;
pub mod reassign;
pub mod throttle;

use std::collections::HashMap;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

/// Top-level credential configuration.
///
/// Historically loaded from `--credential-config` (now deprecated).
/// Accounts are now populated dynamically via the credentials/set API.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CredentialConfig {
    /// Named credential accounts.
    pub accounts: Vec<AccountConfig>,
}

/// Configuration for a single credential account.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AccountConfig {
    /// Display name for this account.
    pub name: String,
    /// Provider identifier: "claude", "openai", "gemini", etc.
    pub provider: String,
    /// Explicit env var name for the credential. Falls back to provider default.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub env_key: Option<String>,
    /// Model name for this account (e.g. "claude-opus-4-6"). Included in
    /// credentials as CLAUDE_MODEL / ANTHROPIC_MODEL so that session switches
    /// after credential rotation use the correct model for the new account.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
    /// Legacy OAuth fields — accepted for config-file compatibility, ignored at runtime.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub token_url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub client_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub auth_url: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub device_auth_url: Option<String>,
    #[serde(default = "default_true")]
    pub reauth: bool,
}

pub fn default_true() -> bool {
    true
}

/// Resolve the state directory for mux data.
///
/// Checks `COOP_MUX_STATE_DIR`, then `$XDG_STATE_HOME/coop/mux`,
/// then `$HOME/.local/state/coop/mux`.
pub fn state_dir() -> PathBuf {
    if let Ok(dir) = std::env::var("COOP_MUX_STATE_DIR") {
        return PathBuf::from(dir);
    }
    if let Ok(xdg) = std::env::var("XDG_STATE_HOME") {
        return PathBuf::from(xdg).join("coop/mux");
    }
    if let Ok(home) = std::env::var("HOME") {
        return PathBuf::from(home).join(".local/state/coop/mux");
    }
    PathBuf::from(".coop/mux")
}

/// Events emitted by the credential broker.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum CredentialEvent {
    /// Fresh credentials are available for distribution.
    Refreshed { account: String, credentials: HashMap<String, String> },
    /// A credential operation failed.
    #[serde(rename = "refresh:failed")]
    RefreshFailed { account: String, error: String },
}

/// Status of an account.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AccountStatus {
    Healthy,
    /// Account exists but has no API key configured.
    Missing,
    /// Account utilization is approaching the limit. New sessions are steered
    /// away but existing sessions continue undisturbed.
    Warming,
    /// Account is rate-limited or budget-exhausted (detected by coopmux from session transitions).
    Throttled,
}

/// Credential-map key carrying the Codex `auth.json` document. coop writes its
/// value to `$CODEX_HOME/auth.json` instead of exporting it to the agent.
pub const CODEX_AUTH_ENV_KEY: &str = "CODEX_AUTH_JSON";

/// Credential-map key carrying an account's default Codex model.
pub const CODEX_MODEL_ENV_KEY: &str = "CODEX_MODEL";

/// Whether `provider` authenticates with a rotating Codex `auth.json` document
/// (ChatGPT subscription) rather than a static bearer token.
pub fn is_codex_provider(provider: &str) -> bool {
    matches!(provider.to_lowercase().as_str(), "openai" | "codex")
}

/// The harness family a provider's credentials belong to. A session's agent and
/// an account's provider must agree on it: a Claude session can never use a
/// Codex (ChatGPT subscription) credential, and a Codex session can never use a
/// Claude one.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProviderFamily {
    Claude,
    Codex,
    Gemini,
}

/// Family of an account `provider` ("claude", "openai", ...), if recognized.
pub fn provider_family(provider: &str) -> Option<ProviderFamily> {
    match provider.to_lowercase().as_str() {
        "claude" | "anthropic" => Some(ProviderFamily::Claude),
        "openai" | "codex" => Some(ProviderFamily::Codex),
        "gemini" | "google" => Some(ProviderFamily::Gemini),
        _ => None,
    }
}

/// Family of a session, from the `agent` key coop reports in its registration
/// metadata (`claude`, `codex`, `gemini`, `unknown`). `None` when absent or
/// unrecognized.
pub fn session_family(metadata: &serde_json::Value) -> Option<ProviderFamily> {
    provider_family(metadata.get("agent")?.as_str()?)
}

/// Whether a session of family `session` may hold an account of `provider`.
///
/// When both families are known they must be equal. Otherwise the Codex
/// boundary still holds: a session whose agent is unknown is treated as
/// non-Codex, and an account whose provider is unrecognized is non-Codex, so a
/// Codex credential only ever pairs with a Codex session. With no session
/// family (`None` from a caller that has no session) only this boundary is
/// enforced.
pub fn provider_compatible(session: Option<ProviderFamily>, provider: &str) -> bool {
    let account = provider_family(provider);
    match (session, account) {
        (Some(s), Some(a)) => s == a,
        (s, a) => (s == Some(ProviderFamily::Codex)) == (a == Some(ProviderFamily::Codex)),
    }
}

/// Resolve the default env var name for a provider.
pub fn provider_default_env_key(provider: &str) -> &str {
    match provider.to_lowercase().as_str() {
        // Claude provider uses OAuth tokens, not API keys. Using
        // CLAUDE_CODE_OAUTH_TOKEN avoids the "Detected a custom API key"
        // prompt in Claude Code that ANTHROPIC_API_KEY triggers.
        "claude" | "anthropic" => "CLAUDE_CODE_OAUTH_TOKEN",
        // ADR 0004 §5: Codex runs on a ChatGPT subscription, so the credential
        // is the auth.json document, never an OPENAI_API_KEY.
        "openai" | "codex" => CODEX_AUTH_ENV_KEY,
        "gemini" | "google" => "GEMINI_API_KEY",
        _ => "API_KEY",
    }
}
