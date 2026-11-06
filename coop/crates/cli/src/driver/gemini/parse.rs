// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Classification of Gemini JSON streams into coop [`AgentState`]s (ADR 0005 §4).
//!
//! Two wire shapes reach the Tier 3 detector, told apart by their keys:
//!
//! - **ACP** (`gemini --acp`, primary): JSON-RPC objects with a `method`. Gemini's
//!   own `session/update` notifications only confirm activity; the turn
//!   lifecycle comes from the synthetic notifications [`super::acp`] injects
//!   (`coop/ready`, `coop/turnStarted`, `coop/agentMessage`, `coop/turnCompleted`,
//!   `coop/error`), because ACP has no "turn started"/"turn ended" notification
//!   of its own: a turn ends when the `session/prompt` *response* arrives.
//! - **`-o stream-json`** (per-turn headless): objects with a `type`
//!   (`init`, `message`, `tool_use`, `tool_result`, `error`, `result`).
//!
//! Shapes are pinned by the fixtures in `coop/tests/fixtures/gemini/` (phase 0
//! spike, `docs/research/gemini-harness/spike.md`).

use serde_json::Value;

use crate::driver::AgentState;
use crate::usage::UsageDelta;

/// Prefix of the error-class tag embedded in Gemini error details, mirroring the
/// Codex `[codex:<class>]` convention so `classify_error_detail` does not depend
/// on prose that may mention unrelated words.
pub const GEMINI_TAG_PREFIX: &str = "[gemini:";

/// Extract a semantic cause string from a Gemini JSON stream object.
pub fn format_gemini_cause(json: &Value) -> String {
    let prefix = if json.get("method").is_some() { "acp" } else { "stdout" };
    match parse_gemini_state(json) {
        Some(AgentState::Idle) => format!("{prefix}:idle"),
        Some(AgentState::Error { .. }) => format!("{prefix}:error"),
        _ => format!("{prefix}:working"),
    }
}

/// Parse one Gemini JSON object into an [`AgentState`].
///
/// Returns `None` for traffic that carries no state information (RPC responses,
/// `available_commands_update`, `usage_update`, ...).
pub fn parse_gemini_state(json: &Value) -> Option<AgentState> {
    if let Some(method) = json.get("method").and_then(Value::as_str) {
        return parse_acp_state(method, json.get("params").unwrap_or(&Value::Null));
    }
    parse_stream_state(json)
}

fn parse_acp_state(method: &str, params: &Value) -> Option<AgentState> {
    match method {
        "coop/ready" => Some(AgentState::Idle),
        "coop/turnStarted" => Some(AgentState::Working),
        "coop/turnCompleted" => {
            // A queued prompt follows immediately; do not flicker through Idle.
            if params.get("continuing").and_then(Value::as_bool).unwrap_or(false) {
                return None;
            }
            Some(AgentState::Idle)
        }
        "coop/error" => {
            let msg = params.get("message").and_then(Value::as_str).unwrap_or("unknown");
            Some(AgentState::Error { detail: tag_message(msg) })
        }
        "session/update" => {
            let kind = params.get("update")?.get("sessionUpdate")?.as_str()?;
            // Chunks, thoughts, plans and tool traffic mean the model is busy;
            // bookkeeping updates and a replayed user message do not.
            matches!(
                kind,
                "agent_message_chunk" | "agent_thought_chunk" | "tool_call" | "tool_call_update"
            )
            .then_some(AgentState::Working)
        }
        _ => None,
    }
}

fn parse_stream_state(json: &Value) -> Option<AgentState> {
    match json.get("type").and_then(Value::as_str)? {
        "result" => match json.get("status").and_then(Value::as_str) {
            Some("error") => {
                let msg = json
                    .get("error")
                    .and_then(|e| e.get("message"))
                    .and_then(Value::as_str)
                    .unwrap_or("unknown");
                Some(AgentState::Error { detail: tag_message(msg) })
            }
            _ => Some(AgentState::Idle),
        },
        // `severity: warning` is retry noise (a blocked stop, a routing hiccup);
        // only a terminal error fails the agent.
        "error" => {
            if json.get("severity").and_then(Value::as_str) == Some("warning") {
                return None;
            }
            let msg = json.get("message").and_then(Value::as_str).unwrap_or("unknown");
            Some(AgentState::Error { detail: tag_message(msg) })
        }
        "init" | "message" | "tool_use" | "tool_result" => Some(AgentState::Working),
        _ => None,
    }
}

/// Tag an error message with its class when the text names one.
///
/// Gemini reports no typed class: the pinned CLI prints
/// `[API Error: Resource has been exhausted (e.g. check quota).]` after ten
/// retries of a 429 (spike R8; exit 173), with `error.type: "unknown"`. The
/// real quota body is carried to the live pass, so this keys on the prose seen
/// so far and on the HTTP status words every Google API error envelope uses.
pub fn tag_message(message: &str) -> String {
    let lower = message.to_lowercase();
    let class = if lower.contains("resource has been exhausted")
        || lower.contains("resource_exhausted")
        || lower.contains("check quota")
        || lower.contains("quota exceeded")
    {
        Some("quotaExhausted")
    } else if lower.contains("api key not valid")
        || lower.contains("api_key_invalid")
        || lower.contains("permission_denied")
        || lower.contains("unauthenticated")
        || lower.contains("invalid auth method")
    {
        Some("unauthorized")
    } else if lower.contains("context window")
        || lower.contains("exceeds the maximum number of tokens")
    {
        Some("contextWindowExceeded")
    } else {
        None
    };
    match class {
        Some(c) => format!("{GEMINI_TAG_PREFIX}{c}] {message}"),
        None => message.to_owned(),
    }
}

/// Extract the assistant's reply for the shared "last message" slot.
///
/// ACP streams the reply as many `agent_message_chunk`s, so the client
/// assembles the turn and injects it as `coop/agentMessage` just before
/// `coop/turnCompleted`.
pub fn extract_gemini_message(json: &Value) -> Option<String> {
    if json.get("method").and_then(Value::as_str) != Some("coop/agentMessage") {
        return None;
    }
    let text = json.get("params")?.get("text")?.as_str()?;
    (!text.trim().is_empty()).then(|| text.to_owned())
}

/// Extracts [`UsageDelta`]s from the Gemini JSON streams.
///
/// - ACP: the `session/prompt` response carries `usage` for the turn; the
///   client forwards it on `coop/turnCompleted`. `usage_update` is a running
///   context-window figure, not a billable count, and is ignored here.
/// - `stream-json`: the final `result` event's `stats` is the turn's usage.
///
/// A given process emits only one of the two, so nothing double counts. Cost
/// stays 0: Gemini is not routed through LiteLLM and its spend tracking is a
/// separate bead (ADR 0005 §5 C).
#[derive(Debug, Default)]
pub struct GeminiUsageTracker;

impl GeminiUsageTracker {
    /// Observe one stream object; returns the usage it adds, if any.
    pub fn observe(&mut self, json: &Value) -> Option<UsageDelta> {
        if json.get("method").and_then(Value::as_str) == Some("coop/turnCompleted") {
            return acp_usage(json.get("params")?.get("usage")?);
        }
        if json.get("type").and_then(Value::as_str) == Some("result") {
            return stream_usage(json.get("stats")?);
        }
        None
    }
}

fn u64_of(v: &Value, key: &str) -> u64 {
    v.get(key).and_then(Value::as_u64).unwrap_or(0)
}

/// Build a delta from Gemini's counters.
///
/// Google's `promptTokenCount` includes the cached tokens, while coop keeps the
/// counters disjoint (as Claude reports them), so cached is subtracted from
/// input. `total` also covers thinking and tool-prompt tokens, which are billed
/// as output but may not be in the reported output count, so output is whatever
/// is left of the total after the prompt. Whether the live API reports output
/// with or without thoughts is carried to the live pass (spike.md); this rule
/// is right either way.
fn delta(input: u64, output: u64, cached: u64, total: u64) -> Option<UsageDelta> {
    let output = output.max(total.saturating_sub(input));
    if input == 0 && output == 0 {
        return None;
    }
    Some(UsageDelta {
        input_tokens: input.saturating_sub(cached),
        output_tokens: output,
        cache_read_input_tokens: cached.min(input),
        ..UsageDelta::default()
    })
}

fn acp_usage(usage: &Value) -> Option<UsageDelta> {
    delta(
        u64_of(usage, "inputTokens"),
        u64_of(usage, "outputTokens"),
        u64_of(usage, "cachedReadTokens"),
        u64_of(usage, "totalTokens"),
    )
}

fn stream_usage(stats: &Value) -> Option<UsageDelta> {
    delta(
        u64_of(stats, "input_tokens"),
        u64_of(stats, "output_tokens"),
        u64_of(stats, "cached"),
        u64_of(stats, "total_tokens"),
    )
}

/// A human-readable one-liner for the terminal screen.
///
/// An ACP pod has no TUI: the backend renders this compact transcript to the
/// PTY side so the screen, ring buffer and recordings show something readable.
/// Returns `None` for traffic not worth a line.
pub fn render_for_screen(json: &Value) -> Option<String> {
    let method = json.get("method").and_then(Value::as_str)?;
    let params = json.get("params").unwrap_or(&Value::Null);
    match method {
        "coop/agentMessage" => Some(format!("● {}", params.get("text")?.as_str()?)),
        "coop/prompt" => Some(format!("> {}", params.get("text")?.as_str()?)),
        "coop/turnCompleted" => {
            Some(format!("-- turn {} --", params.get("stopReason")?.as_str().unwrap_or("done")))
        }
        "coop/ready" => Some("-- gemini ready --".to_owned()),
        "coop/error" => Some(format!("! {}", params.get("message")?.as_str()?)),
        "coop/permission" => Some(format!(
            "! permission {}: {}",
            params.get("outcome")?.as_str()?,
            params.get("title").and_then(Value::as_str).unwrap_or("tool")
        )),
        "session/update" => {
            let update = params.get("update")?;
            match update.get("sessionUpdate")?.as_str()? {
                "tool_call" => Some(format!("$ {}", update.get("title")?.as_str()?)),
                _ => None,
            }
        }
        _ => None,
    }
}

#[cfg(test)]
#[path = "parse_tests.rs"]
mod tests;
