// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::Value;

use crate::driver::AgentState;

use super::prompt::extract_ask_user_context;

/// Extract a short human-readable label from a tool's input parameters.
///
/// Mirrors the Go-side `extractToolLabel()` in `hookstream.go` so that both
/// the bridge (Slack display) and coop (cause strings, status bar) show
/// consistent summaries.
pub fn extract_tool_label(tool: &str, input: Option<&Value>) -> Option<String> {
    let obj = input?.as_object()?;

    let get_str = |key: &str| obj.get(key).and_then(|v| v.as_str()).filter(|s| !s.is_empty());

    let label = match tool {
        "Bash" => {
            if let Some(desc) = get_str("description") {
                truncate_label(desc, 50)
            } else if let Some(cmd) = get_str("command") {
                let line = cmd.split('\n').next().unwrap_or(cmd);
                // Strip leading `cd ... && ` for readability.
                let line = match (line.starts_with("cd "), line.find(" && ")) {
                    (true, Some(idx)) => &line[idx + 4..],
                    _ => line,
                };
                truncate_label(line, 50)
            } else {
                return None;
            }
        }
        "Read" | "Edit" | "Write" => {
            let fp = get_str("file_path")?;
            file_basename(fp).to_owned()
        }
        "Grep" | "Glob" => truncate_label(get_str("pattern")?, 30),
        "Agent" => truncate_label(get_str("description")?, 40),
        "WebFetch" => truncate_label(get_str("url")?, 50),
        "WebSearch" => truncate_label(get_str("query")?, 40),
        "LSP" => {
            let op = get_str("operation")?;
            if let Some(fp) = get_str("filePath") {
                truncate_label(&format!("{op} {}", file_basename(fp)), 40)
            } else {
                truncate_label(op, 30)
            }
        }
        "NotebookEdit" => {
            let np = get_str("notebook_path")?;
            file_basename(np).to_owned()
        }
        "Skill" => truncate_label(get_str("skill")?, 30),
        "SendMessage" => {
            let to = get_str("to")?;
            truncate_label(&format!("to: {to}"), 30)
        }
        _ => {
            // Generic fallback: try common field names (MCP tools, etc.).
            for key in &[
                "description",
                "query",
                "url",
                "pattern",
                "prompt",
                "operation",
                "skill",
                "command",
                "file_path",
                "path",
                "name",
            ] {
                if let Some(v) = get_str(key) {
                    return Some(truncate_label(v, 40));
                }
            }
            return None;
        }
    };
    Some(label)
}

/// Format a tool cause string, optionally including a label from tool input.
fn format_tool_cause(prefix: &str, tool: &str, input: Option<&Value>) -> String {
    match extract_tool_label(tool, input) {
        Some(label) => format!("{prefix}:tool({tool}: {label})"),
        None => format!("{prefix}:tool({tool})"),
    }
}

/// Truncate a string to `max` characters, appending "..." if truncated.
fn truncate_label(s: &str, max: usize) -> String {
    if s.len() <= max {
        s.to_owned()
    } else {
        let mut result = s[..max].to_owned();
        result.push_str("...");
        result
    }
}

/// Extract the file basename from a path.
fn file_basename(path: &str) -> &str {
    path.rsplit('/').next().unwrap_or(path)
}

/// Extract a semantic cause string from a Claude session log JSONL entry.
///
/// Uses the given `prefix` ("log" or "stdout") to build the cause.
pub fn format_claude_cause(json: &Value, prefix: &str) -> String {
    if json.get("error").is_some() {
        return format!("{prefix}:error");
    }

    // API-error assistant messages (isApiErrorMessage: true) carry the error in
    // a text block rather than a top-level `error` field; label them as errors
    // so the detector treats them at error confidence (see parse_claude_state).
    if json.get("isApiErrorMessage").and_then(|v| v.as_bool()).unwrap_or(false) {
        return format!("{prefix}:error");
    }

    let entry_type = json.get("type").and_then(|v| v.as_str());
    if entry_type != Some("assistant") {
        if entry_type == Some("user") {
            return format!("{prefix}:user");
        }
        return format!("{prefix}:working");
    }

    let Some(content) =
        json.get("message").and_then(|m| m.get("content")).and_then(|c| c.as_array())
    else {
        return format!("{prefix}:idle");
    };

    for block in content {
        let block_type = block.get("type").and_then(|v| v.as_str());
        match block_type {
            Some("tool_use") => {
                let tool = block.get("name").and_then(|v| v.as_str()).unwrap_or("unknown");
                let input = block.get("input");
                return format_tool_cause(prefix, tool, input);
            }
            Some("thinking") => return format!("{prefix}:thinking"),
            _ => {}
        }
    }

    format!("{prefix}:idle")
}

/// Extract the concatenated text content from an assistant JSONL entry.
///
/// Returns `None` for non-assistant entries (caller must NOT clear existing value)
/// or assistant messages with no `type: "text"` blocks.
pub fn extract_assistant_text(json: &Value) -> Option<String> {
    if json.get("type").and_then(|v| v.as_str()) != Some("assistant") {
        return None;
    }
    let content = json.get("message")?.get("content")?.as_array()?;
    let texts: Vec<&str> = content
        .iter()
        .filter(|b| b.get("type").and_then(|v| v.as_str()) == Some("text"))
        .filter_map(|b| b.get("text").and_then(|v| v.as_str()))
        .collect();
    if texts.is_empty() {
        return None;
    }
    let joined = texts.join("\n");
    let trimmed = joined.trim();
    if trimmed.is_empty() {
        return None;
    }
    Some(trimmed.to_owned())
}

/// Render a top-level `error` field value into a detail string.
///
/// Handles both string errors (`"authentication_error"`) and structured error
/// objects (`{"type": "budget_exceeded", "message": "..."}`), preferring `type`
/// as the primary identifier and falling back to `message`. Any shape that
/// carries no usable identifier (an empty object, a number, etc.) renders as the
/// `"unknown"` sentinel.
fn error_detail(error: &Value) -> String {
    if let Some(s) = error.as_str() {
        s.to_string()
    } else if let Some(obj) = error.as_object() {
        // Prefer "type" as primary identifier, fall back to "message".
        let err_type = obj.get("type").and_then(|v| v.as_str()).unwrap_or("");
        let err_msg = obj.get("message").and_then(|v| v.as_str()).unwrap_or("");
        if !err_type.is_empty() && !err_msg.is_empty() {
            format!("{err_type}: {err_msg}")
        } else if !err_type.is_empty() {
            err_type.to_string()
        } else if !err_msg.is_empty() {
            err_msg.to_string()
        } else {
            "unknown".to_string()
        }
    } else {
        "unknown".to_string()
    }
}

/// Render a top-level `error` field into a detail string only if it is
/// *meaningful* — i.e. carries a real classification signal.
///
/// Returns `None` for the non-informative `"unknown"` sentinel that Claude Code
/// (>= 2.1.x) emits alongside `isApiErrorMessage: true` when the real, classifiable
/// error lives in the assistant text block (e.g. the socket-closed banner). This
/// lets [`parse_claude_state`] prefer the text block in that case while keeping a
/// genuine `error` field (auth / budget / structured errors) authoritative.
fn meaningful_error_detail(error: &Value) -> Option<String> {
    let detail = error_detail(error);
    if detail == "unknown" {
        None
    } else {
        Some(detail)
    }
}

/// Parse a Claude session log JSONL entry into an [`AgentState`].
///
/// Returns `None` if the entry cannot be meaningfully classified (e.g.
/// missing required fields on an assistant message).
pub fn parse_claude_state(json: &Value) -> Option<AgentState> {
    // Error detail resolution, in priority order:
    //   1. A *meaningful* top-level `error` field — authoritative. A string like
    //      "authentication_error" or a structured object {"type":"budget_exceeded",
    //      "message":"..."} carries the canonical classification and MUST win, so a
    //      fatal/zombie category (Unauthorized / BudgetExceeded) is never overridden
    //      by a generic human-readable banner.
    //   2. Otherwise, if the entry is an `isApiErrorMessage` assistant message, the
    //      assistant text block — see the isApiErrorMessage rationale below.
    //   3. Otherwise, the non-informative sentinel ("unknown" / "API Error").
    //
    // The ordering matters because real Claude Code (>= 2.1.x) emits BOTH a
    // top-level `error` field AND `isApiErrorMessage: true` on the same entry, with
    // the *real* error living in the text block while `error` is a coarse sentinel:
    //   {"type":"assistant","error":"unknown","isApiErrorMessage":true,
    //    "message":{"content":[{"type":"text",
    //      "text":"API Error: The socket connection was closed unexpectedly …"}]}}
    // A naive "error field first" check returns Error{detail:"unknown"}, which
    // classify_error_detail() maps to ErrorCategory::Other — a category with no
    // recovery branch in the controller's monitorExit — so the agent sits on the
    // error screen with no dismiss/backoff/re-nudge and never resumes (ka-0000003334,
    // the gap !4864 missed: its "socket connection was closed" → ServerError rule is
    // dead code when the discriminating text never reaches the classifier). Treating
    // the "unknown" sentinel as non-meaningful and preferring the text block routes
    // the socket-closed banner to ServerError → in-place dismiss + backoff +
    // re-nudge, same session.
    let error_field_detail = json.get("error").and_then(meaningful_error_detail);
    let is_api_error = json.get("isApiErrorMessage").and_then(|v| v.as_bool()).unwrap_or(false);

    if let Some(detail) = error_field_detail {
        return Some(AgentState::Error { detail });
    }

    // API-error assistant messages. When the Claude Code CLI exhausts its own
    // internal retries on a transient upstream failure (e.g. the LiteLLM proxy
    // cooldown banner "Server is temporarily limiting requests … No deployments
    // available for selected model, Try again in 5 seconds.", or a server-initiated
    // "API Error: The socket connection was closed unexpectedly … fetch()"), it
    // records a normal `assistant` entry whose text block carries the human-readable
    // banner and flags it with `isApiErrorMessage: true` (often alongside a coarse
    // top-level `error` sentinel like "unknown", handled above). Without this branch
    // such an entry falls through to the "assistant message with no tool_use → Idle"
    // case below, so the composite detector rejects it as a downgrade from the
    // in-flight Working state and the banner never reaches `classify_error_detail`
    // (no rotation, no nudge/retry — ka-0000006242). Surface it as an Error so
    // `transition::process_detected_state` classifies the banner and routes it
    // through the matching recovery path (RateLimited → handle_rate_limit(),
    // ServerError → the controller's server_error re-nudge).
    if is_api_error {
        // Prefer the classifiable banner text. Any top-level `error` field that
        // could reach here is the non-meaningful "unknown" sentinel (a meaningful
        // value is authoritative and already returned above), so there is nothing
        // useful to fall back to — use the generic "API Error" placeholder when
        // the entry carries no text block.
        let detail = extract_assistant_text(json).unwrap_or_else(|| "API Error".to_owned());
        return Some(AgentState::Error { detail });
    }

    // No assistant text and not flagged as an API error, but a bare/sentinel
    // `error` field is present — fall back to the sentinel so the entry is still
    // surfaced as an Error rather than silently dropped.
    if let Some(error) = json.get("error") {
        return Some(AgentState::Error { detail: error_detail(error) });
    }

    let entry_type = json.get("type").and_then(|v| v.as_str());

    // Detect user interrupts: Escape during response writes text containing
    // "[Request interrupted by user]"; rejecting a tool use writes an entry
    // with `toolUseResult: "User rejected tool use"`. Both are turn
    // boundaries — the agent is idle. Other user messages → working.
    //
    // Local commands (e.g. `/model`, `/help`) write meta entries and
    // entries whose content is a plain string with XML-like tags
    // (`<command-name>`, `<local-command-stdout>`, `<local-command-caveat>`).
    // These never trigger an API turn, so emitting Working would leave
    // the session stuck — ignore them.
    if entry_type == Some("user") {
        // Meta entries (e.g. local-command-caveat wrappers) are not real turns.
        if json.get("isMeta").and_then(|v| v.as_bool()).unwrap_or(false) {
            return None;
        }

        // Non-turn user messages have plain-string content (not an array) with
        // XML-like tag markers. These are injected by the CLI for:
        //   - Local commands: `<command-name>/model</command-name>...`
        //   - Local command output: `<local-command-stdout>...</local-command-stdout>`
        //   - Command mode bash: `<bash-input>ls</bash-input>`,
        //     `<bash-stdout>...</bash-stdout><bash-stderr>...</bash-stderr>`
        //
        // None of these start a real API turn, so emitting Working would
        // leave the session stuck. Skip them.
        if let Some(content_str) =
            json.get("message").and_then(|m| m.get("content")).and_then(|c| c.as_str())
        {
            let trimmed = content_str.trim_start();
            if trimmed.starts_with("<command-name>")
                || trimmed.starts_with("<local-command-")
                || trimmed.starts_with("<bash-")
            {
                return None;
            }
        }

        let is_interrupt = json
            .get("toolUseResult")
            .and_then(|v| v.as_str())
            .is_some_and(|s| s == "User rejected tool use")
            || json
                .get("message")
                .and_then(|m| m.get("content"))
                .and_then(|c| c.as_array())
                .is_some_and(|blocks| {
                    blocks.iter().any(|b| {
                        b.get("type").and_then(|v| v.as_str()) == Some("text")
                            && b.get("text")
                                .and_then(|v| v.as_str())
                                .is_some_and(|t| t.contains("[Request interrupted by user]"))
                    })
                });
        return if is_interrupt { Some(AgentState::Idle) } else { Some(AgentState::Working) };
    }

    // Only assistant messages carry meaningful state transitions.
    // Other types (progress, system, file-history-snapshot, etc.) are
    // session metadata — not agent state signals. Emitting Working for
    // these would let Tier 2 spuriously escalate over a Tier 1 Idle
    // (e.g. a stop_hook_summary arriving after the Stop hook FIFO event).
    if entry_type != Some("assistant") {
        return None;
    }

    let content = json.get("message")?.get("content")?.as_array()?;

    for block in content {
        let block_type = block.get("type").and_then(|v| v.as_str());
        match block_type {
            Some("tool_use") => {
                let tool = block.get("name").and_then(|v| v.as_str()).unwrap_or("unknown");
                return match tool {
                    "AskUserQuestion" => {
                        Some(AgentState::Prompt { prompt: extract_ask_user_context(block) })
                    }
                    _ => Some(AgentState::Working),
                };
            }
            Some("thinking") => return Some(AgentState::Working),
            _ => {}
        }
    }

    // Assistant message with no tool_use or thinking blocks — idle
    Some(AgentState::Idle)
}

#[cfg(test)]
#[path = "parse_tests.rs"]
mod tests;
