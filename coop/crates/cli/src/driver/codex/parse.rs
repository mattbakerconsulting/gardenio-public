// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Classification of Codex JSON streams into coop [`AgentState`]s.
//!
//! Two wire shapes reach the Tier 3 detector, told apart by their keys:
//!
//! - **`codex app-server` JSON-RPC** (primary, ADR 0004 §3): objects with a
//!   `method` (`turn/started`, `turn/completed`, `error`, ...). Turn lifecycle
//!   notifications are authoritative; `item/*` traffic only confirms activity.
//! - **`codex exec --json`** (fallback): objects with a `type`
//!   (`thread.started`, `turn.started`, `item.*`, `turn.completed`,
//!   `turn.failed`). Per ADR 0004 only `turn.failed` fails a turn; `error`
//!   events and `error` items inside a turn are retry noise.
//!
//! coop itself injects three synthetic JSON-RPC notifications into the stream
//! (see [`super::rpc`]): `coop/ready`, `coop/error` and `coop/approvalDenied`.

use serde_json::Value;

use crate::driver::AgentState;
use crate::usage::UsageDelta;

/// Extracts [`UsageDelta`]s from the Codex JSON streams (ADR 0004 §9).
///
/// - `codex exec --json`: `turn.completed.usage` is the usage of that turn
///   (cumulative across a stop-gate continuation within it), one delta per turn.
/// - `codex app-server`: `thread/tokenUsage/updated` carries a running thread
///   `total` and the `last` response's usage. A delta is the growth of `total`
///   since the previous notification; the first notification uses `last`, so a
///   resumed thread's restored history is not billed to this session.
///
/// A given process emits only one of the two shapes, so nothing double counts.
/// Cost stays 0: Codex runs on the subscription, not LiteLLM-metered API keys.
#[derive(Debug, Default)]
pub struct CodexUsageTracker {
    prev_total: Option<TokenCounts>,
}

#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
struct TokenCounts {
    input: u64,
    cached: u64,
    cache_write: u64,
    output: u64,
}

impl TokenCounts {
    /// Read either spelling: exec's `input_tokens`, app-server's `inputTokens`.
    fn from_json(v: &Value) -> Self {
        let get = |snake: &str, camel: &str| {
            v.get(snake).or_else(|| v.get(camel)).and_then(Value::as_u64).unwrap_or(0)
        };
        Self {
            input: get("input_tokens", "inputTokens"),
            cached: get("cached_input_tokens", "cachedInputTokens"),
            cache_write: get("cache_write_input_tokens", "cacheWriteInputTokens"),
            output: get("output_tokens", "outputTokens"),
        }
    }

    fn since(self, prev: Self) -> Self {
        Self {
            input: self.input.saturating_sub(prev.input),
            cached: self.cached.saturating_sub(prev.cached),
            cache_write: self.cache_write.saturating_sub(prev.cache_write),
            output: self.output.saturating_sub(prev.output),
        }
    }

    /// OpenAI's `input_tokens` already includes the cached and cache-write
    /// tokens; coop's counters keep the three disjoint (as Claude reports them),
    /// so `context_tokens` (their sum) is the real window occupancy.
    fn into_delta(self) -> Option<UsageDelta> {
        if self.input == 0 && self.output == 0 {
            return None;
        }
        Some(UsageDelta {
            input_tokens: self.input.saturating_sub(self.cached + self.cache_write),
            output_tokens: self.output,
            cache_creation_input_tokens: self.cache_write,
            cache_read_input_tokens: self.cached,
            ..UsageDelta::default()
        })
    }
}

impl CodexUsageTracker {
    /// Observe one stream object; returns the usage it adds, if any.
    pub fn observe(&mut self, json: &Value) -> Option<UsageDelta> {
        if json.get("type").and_then(Value::as_str) == Some("turn.completed") {
            return TokenCounts::from_json(json.get("usage")?).into_delta();
        }
        if json.get("method").and_then(Value::as_str) == Some("thread/tokenUsage/updated") {
            let usage = json.get("params")?.get("tokenUsage")?;
            let total = TokenCounts::from_json(usage.get("total")?);
            let delta = match self.prev_total.replace(total) {
                Some(prev) => total.since(prev),
                None => TokenCounts::from_json(usage.get("last")?),
            };
            return delta.into_delta();
        }
        None
    }
}

/// Prefix of the error-class tag embedded in error details.
///
/// `classify_error_detail` keys off `[codex:<CodexErrorInfo>]` so Codex's typed
/// error classes land on the existing [`crate::driver::ErrorCategory`] values
/// without relying on prose that may mention unrelated words ("credits").
pub const CODEX_TAG_PREFIX: &str = "[codex:";

/// Parse one Codex JSON object into an [`AgentState`].
///
/// Returns `None` for traffic that carries no state information (deltas,
/// warnings, token usage, hook notifications, server-request echoes).
pub fn parse_codex_state(json: &Value) -> Option<AgentState> {
    if let Some(method) = json.get("method").and_then(Value::as_str) {
        return parse_rpc_state(method, json.get("params").unwrap_or(&Value::Null));
    }
    parse_exec_state(json)
}

/// Semantic cause string for a parsed entry (`rpc:*` for app-server, `exec:*`
/// for the exec stream).
pub fn format_codex_cause(json: &Value) -> String {
    if let Some(method) = json.get("method").and_then(Value::as_str) {
        let params = json.get("params").unwrap_or(&Value::Null);
        return match parse_rpc_state(method, params) {
            Some(AgentState::Idle) => "rpc:idle".to_owned(),
            Some(AgentState::Error { .. }) => "rpc:error".to_owned(),
            _ => "rpc:working".to_owned(),
        };
    }
    match parse_exec_state(json) {
        Some(AgentState::Idle) => "exec:idle".to_owned(),
        Some(AgentState::Error { .. }) => "exec:error".to_owned(),
        _ => "exec:working".to_owned(),
    }
}

fn parse_rpc_state(method: &str, params: &Value) -> Option<AgentState> {
    match method {
        // The thread exists and can take input (synthetic, emitted once the
        // thread/start or thread/resume response arrives).
        "coop/ready" | "thread/started" => Some(AgentState::Idle),
        "turn/started" => Some(AgentState::Working),
        "turn/completed" => {
            let turn = params.get("turn")?;
            match turn.get("status").and_then(Value::as_str) {
                Some("completed" | "interrupted") => Some(AgentState::Idle),
                Some("failed") => {
                    let detail = turn_error_detail(turn.get("error"));
                    Some(AgentState::Error { detail })
                }
                Some("inProgress") => Some(AgentState::Working),
                _ => None,
            }
        }
        // `willRetry: true` is the client reconnecting; only a terminal error
        // fails the agent.
        "error" => {
            if params.get("willRetry").and_then(Value::as_bool).unwrap_or(false) {
                return None;
            }
            Some(AgentState::Error { detail: turn_error_detail(params.get("error")) })
        }
        "thread/status/changed" => {
            // idle/active are redundant with the turn lifecycle (and idle is
            // emitted *before* turn/completed). Only a system error matters.
            let status = params.get("status")?.get("type")?.as_str()?;
            (status == "systemError")
                .then(|| AgentState::Error { detail: "codex thread entered systemError".into() })
        }
        "item/started" | "item/completed" => Some(AgentState::Working),
        "account/rateLimits/updated" => {
            let reached = params.get("rateLimits")?.get("rateLimitReachedType")?.as_str()?;
            Some(AgentState::Error {
                detail: format!(
                    "{CODEX_TAG_PREFIX}usageLimitExceeded] codex rate limit reached ({reached})"
                ),
            })
        }
        "coop/error" => {
            let msg = params.get("message").and_then(Value::as_str).unwrap_or("unknown");
            Some(AgentState::Error { detail: msg.to_owned() })
        }
        // Approvals must never occur under approval_policy=never; one is an
        // error state, never a prompt (ADR 0004 §3).
        "coop/approvalDenied" => {
            let kind = params.get("request").and_then(Value::as_str).unwrap_or("approval");
            Some(AgentState::Error {
                detail: format!(
                    "codex requested {kind} under approval_policy=never; coop auto-declined"
                ),
            })
        }
        _ => None,
    }
}

fn parse_exec_state(json: &Value) -> Option<AgentState> {
    match json.get("type").and_then(Value::as_str)? {
        "thread.started" | "turn.started" => Some(AgentState::Working),
        "item.started" => Some(AgentState::Working),
        // `error` items are informational (model-metadata warnings etc.).
        "item.completed" | "item.updated" => {
            let item_type = json.get("item")?.get("type")?.as_str()?;
            (item_type != "error").then_some(AgentState::Working)
        }
        "turn.completed" => Some(AgentState::Idle),
        "turn.failed" => {
            let msg = json
                .get("error")
                .and_then(|e| e.get("message"))
                .and_then(Value::as_str)
                .unwrap_or("unknown");
            Some(AgentState::Error { detail: tag_exec_message(msg) })
        }
        _ => None,
    }
}

/// Render a `TurnError` (`{message, codexErrorInfo, additionalDetails}`) as an
/// error detail string, tagged with its [`CodexErrorInfo`] class when known.
fn turn_error_detail(error: Option<&Value>) -> String {
    let Some(error) = error else { return "unknown".to_owned() };
    let message = error.get("message").and_then(Value::as_str).unwrap_or("unknown");
    let info = error.get("codexErrorInfo");
    match info.and_then(error_info_name) {
        // The typed class for a rejected credential is `unauthorized`, but the
        // backend reports a 401/403 on the request path as a *connection* class
        // carrying `httpStatusCode`; retag it so it never reads as connectivity
        // (gd-fr3xjSxIws).
        Some(class) if is_connection_class(&class) && info.is_some_and(is_auth_status) => {
            format!("{CODEX_TAG_PREFIX}unauthorized] {message}")
        }
        Some(class) => format!("{CODEX_TAG_PREFIX}{class}] {message}"),
        None => tag_exec_message(message),
    }
}

/// The class name of a `CodexErrorInfo`: either a bare string
/// (`"unauthorized"`) or a single-key object (`{"httpConnectionFailed":{..}}`).
fn error_info_name(info: &Value) -> Option<String> {
    match info {
        Value::String(s) => Some(s.clone()),
        Value::Object(map) => map.keys().next().cloned(),
        _ => None,
    }
}

fn is_connection_class(class: &str) -> bool {
    matches!(class, "httpConnectionFailed" | "responseStreamConnectionFailed")
}

/// `true` when a `CodexErrorInfo` object carries `httpStatusCode` 401 or 403.
fn is_auth_status(info: &Value) -> bool {
    let code =
        info.as_object().and_then(|m| m.values().next()).and_then(|v| v.get("httpStatusCode"));
    matches!(code.and_then(Value::as_u64), Some(401 | 403))
}

/// The exec stream carries no typed error class, so infer one from the message
/// text. Untagged messages fall through to the generic substring classifier.
fn tag_exec_message(message: &str) -> String {
    let lower = message.to_lowercase();
    let class = if lower.contains("usage limit") {
        Some("usageLimitExceeded")
    } else if lower.contains("context window") || lower.contains("context_length_exceeded") {
        Some("contextWindowExceeded")
    } else if lower.contains("401 unauthorized")
        || lower.contains("invalid_api_key")
        || lower.contains("token_expired")
        || lower.contains("refresh token")
    {
        Some("unauthorized")
    } else if lower.contains("429") || lower.contains("rate limit") {
        Some("rateLimitExceeded")
    } else {
        None
    };
    match class {
        Some(c) => format!("{CODEX_TAG_PREFIX}{c}] {message}"),
        None => message.to_owned(),
    }
}

/// Extract assistant text from a completed agent-message entry, for the
/// shared "last message" slot.
pub fn extract_codex_message(json: &Value) -> Option<String> {
    // app-server: item/completed {item:{type:"agentMessage", text}}
    if json.get("method").and_then(Value::as_str) == Some("item/completed") {
        let item = json.get("params")?.get("item")?;
        if item.get("type").and_then(Value::as_str) == Some("agentMessage") {
            return item.get("text").and_then(Value::as_str).map(str::to_owned);
        }
        return None;
    }
    // exec: item.completed {item:{type:"agent_message", text}}
    if json.get("type").and_then(Value::as_str) == Some("item.completed") {
        let item = json.get("item")?;
        if item.get("type").and_then(Value::as_str) == Some("agent_message") {
            return item.get("text").and_then(Value::as_str).map(str::to_owned);
        }
    }
    None
}

/// A human-readable one-liner for the terminal screen.
///
/// A Codex pod has no TUI (ADR 0004 §3): the backend renders this compact
/// transcript to the PTY side so the screen, ring buffer and recordings show
/// something readable. Returns `None` for traffic not worth a line.
pub fn render_for_screen(json: &Value) -> Option<String> {
    if let Some(method) = json.get("method").and_then(Value::as_str) {
        let params = json.get("params").unwrap_or(&Value::Null);
        return match method {
            "item/completed" => render_rpc_item(params.get("item")?),
            "turn/completed" => {
                let turn = params.get("turn")?;
                let status = turn.get("status").and_then(Value::as_str)?;
                Some(format!("-- turn {status} --"))
            }
            "error" if !params.get("willRetry").and_then(Value::as_bool).unwrap_or(false) => {
                let msg = params.get("error")?.get("message")?.as_str()?;
                Some(format!("! {msg}"))
            }
            "coop/ready" => Some("-- codex ready --".to_owned()),
            "coop/error" => Some(format!("! {}", params.get("message")?.as_str()?)),
            "coop/approvalDenied" => {
                Some(format!("! approval auto-declined: {}", params.get("request")?.as_str()?))
            }
            _ => None,
        };
    }
    match json.get("type").and_then(Value::as_str)? {
        "item.completed" => render_exec_item(json.get("item")?),
        "turn.completed" => Some("-- turn completed --".to_owned()),
        "turn.failed" => {
            Some(format!("! {}", json.get("error")?.get("message")?.as_str().unwrap_or("failed")))
        }
        _ => None,
    }
}

fn render_rpc_item(item: &Value) -> Option<String> {
    match item.get("type").and_then(Value::as_str)? {
        "agentMessage" => Some(format!("● {}", item.get("text")?.as_str()?)),
        "userMessage" => {
            let text = item.get("content")?.as_array()?.iter().find_map(|c| c.get("text"))?;
            Some(format!("> {}", text.as_str()?))
        }
        "commandExecution" => Some(format!("$ {}", item.get("command")?.as_str()?)),
        _ => None,
    }
}

fn render_exec_item(item: &Value) -> Option<String> {
    match item.get("type").and_then(Value::as_str)? {
        "agent_message" => Some(format!("● {}", item.get("text")?.as_str()?)),
        "command_execution" => Some(format!("$ {}", item.get("command")?.as_str()?)),
        _ => None,
    }
}

#[cfg(test)]
#[path = "parse_tests.rs"]
mod tests;
