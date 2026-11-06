// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::json;

use crate::driver::AgentState;

use super::{
    extract_gemini_message, format_gemini_cause, parse_gemini_state, render_for_screen,
    tag_message, GeminiUsageTracker,
};

#[yare::parameterized(
    init_event = {
        json!({"type": "init", "session_id": "abc123", "model": "gemini-2.0-flash-exp", "timestamp": "2025-10-10T12:00:00.000Z"}),
        Some(AgentState::Working)
    },
    user_message = {
        json!({"type": "message", "role": "user", "content": "List files", "timestamp": "2025-10-10T12:00:01.000Z"}),
        Some(AgentState::Working)
    },
    assistant_message = {
        json!({"type": "message", "role": "assistant", "content": "Here are the files", "delta": true, "timestamp": "2025-10-10T12:00:02.000Z"}),
        Some(AgentState::Working)
    },
    tool_use = {
        json!({"type": "tool_use", "tool_name": "Bash", "tool_id": "bash-123", "parameters": {"command": "ls"}, "timestamp": "2025-10-10T12:00:03.000Z"}),
        Some(AgentState::Working)
    },
    tool_result = {
        json!({"type": "tool_result", "tool_id": "bash-123", "status": "success", "output": "file1.txt", "timestamp": "2025-10-10T12:00:04.000Z"}),
        Some(AgentState::Working)
    },
    result_success = {
        json!({"type": "result", "status": "success", "stats": {"total_tokens": 250}, "timestamp": "2025-10-10T12:00:05.000Z"}),
        Some(AgentState::Idle)
    },
    // The pinned CLI ends a quota-exhausted run with result.status=error (spike R8).
    result_error = {
        json!({"type": "result", "status": "error", "error": {"type": "unknown", "message": "[API Error: Resource has been exhausted (e.g. check quota).]"}}),
        Some(AgentState::Error { detail: "[gemini:quotaExhausted] [API Error: Resource has been exhausted (e.g. check quota).]".to_string() })
    },
    // A blocked stop surfaces as a warning and the turn continues.
    warning_event = {
        json!({"type": "error", "severity": "warning", "message": "Agent execution blocked: STOPGATE-BLOCK: say banana"}),
        None
    },
    error_event = {
        json!({"type": "error", "message": "rate limit exceeded", "timestamp": "2025-10-10T12:00:05.000Z"}),
        Some(AgentState::Error { detail: "rate limit exceeded".to_string() })
    },
    error_without_message = {
        json!({"type": "error", "timestamp": "2025-10-10T12:00:05.000Z"}),
        Some(AgentState::Error { detail: "unknown".to_string() })
    },
    unknown_type = {
        json!({"type": "custom_event"}),
        None
    },
    missing_type = {
        json!({"data": "something"}),
        None
    },
)]
fn state_from_stream_json(entry: serde_json::Value, expected: Option<AgentState>) {
    assert_eq!(parse_gemini_state(&entry), expected);
}

#[yare::parameterized(
    ready = { json!({"method": "coop/ready", "params": {"sessionId": "s"}}), Some(AgentState::Idle) },
    turn_started = { json!({"method": "coop/turnStarted", "params": {}}), Some(AgentState::Working) },
    turn_completed = {
        json!({"method": "coop/turnCompleted", "params": {"stopReason": "end_turn", "continuing": false}}),
        Some(AgentState::Idle)
    },
    // Another queued prompt starts right away: no Idle in between.
    turn_completed_continuing = {
        json!({"method": "coop/turnCompleted", "params": {"stopReason": "end_turn", "continuing": true}}),
        None
    },
    coop_error = {
        json!({"method": "coop/error", "params": {"message": "gemini acp initialize failed: boom"}}),
        Some(AgentState::Error { detail: "gemini acp initialize failed: boom".to_string() })
    },
    message_chunk = {
        json!({"method": "session/update", "params": {"update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "x"}}}}),
        Some(AgentState::Working)
    },
    tool_call = {
        json!({"method": "session/update", "params": {"update": {"sessionUpdate": "tool_call", "title": "ls"}}}),
        Some(AgentState::Working)
    },
    tool_call_update = {
        json!({"method": "session/update", "params": {"update": {"sessionUpdate": "tool_call_update", "status": "completed"}}}),
        Some(AgentState::Working)
    },
    usage_update_is_bookkeeping = {
        json!({"method": "session/update", "params": {"update": {"sessionUpdate": "usage_update", "used": 103, "size": 1048576}}}),
        None
    },
    commands_update_is_bookkeeping = {
        json!({"method": "session/update", "params": {"update": {"sessionUpdate": "available_commands_update"}}}),
        None
    },
    rpc_response = { json!({"jsonrpc": "2.0", "id": 4, "result": {"stopReason": "end_turn"}}), None },
)]
fn state_from_acp(entry: serde_json::Value, expected: Option<AgentState>) {
    assert_eq!(parse_gemini_state(&entry), expected);
}

#[test]
fn causes_name_the_wire_shape() {
    let ready = json!({"method": "coop/ready", "params": {}});
    let started = json!({"method": "coop/turnStarted", "params": {}});
    let boom = json!({"method": "coop/error", "params": {"message": "x"}});
    assert_eq!(format_gemini_cause(&ready), "acp:idle");
    assert_eq!(format_gemini_cause(&started), "acp:working");
    assert_eq!(format_gemini_cause(&boom), "acp:error");
    assert_eq!(format_gemini_cause(&json!({"type": "result", "status": "success"})), "stdout:idle");
    assert_eq!(format_gemini_cause(&json!({"type": "message"})), "stdout:working");
}

#[test]
fn error_messages_are_tagged_with_their_class() {
    use crate::driver::{classify_error_detail, ErrorCategory};
    for (message, category) in [
        (
            "[API Error: Resource has been exhausted (e.g. check quota).]",
            ErrorCategory::RateLimited,
        ),
        ("API key not valid. Please pass a valid API key.", ErrorCategory::Unauthorized),
        ("Please set an Auth method in your settings", ErrorCategory::Other),
    ] {
        let tagged = tag_message(message);
        assert_eq!(classify_error_detail(&tagged), category, "{tagged}");
    }
    assert_eq!(tag_message("plain failure"), "plain failure");
}

#[test]
fn last_message_comes_from_the_assembled_turn_only() {
    let msg = json!({"method": "coop/agentMessage", "params": {"text": "pong"}});
    assert_eq!(extract_gemini_message(&msg).as_deref(), Some("pong"));
    let blank = json!({"method": "coop/agentMessage", "params": {"text": "  "}});
    assert_eq!(extract_gemini_message(&blank), None);
    // A raw chunk is a fragment, not the reply.
    let chunk = json!({"method": "session/update", "params": {"update": {
        "sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "po"}}}});
    assert_eq!(extract_gemini_message(&chunk), None);
}

fn usage_of(entry: serde_json::Value) -> Option<(u64, u64, u64)> {
    GeminiUsageTracker
        .observe(&entry)
        .map(|d| (d.input_tokens, d.output_tokens, d.cache_read_input_tokens))
}

#[test]
fn acp_turn_usage_is_read_from_the_prompt_response() {
    // acp/basic.jsonl: inputTokens 100, outputTokens 3, totalTokens 103.
    let done = json!({"method": "coop/turnCompleted", "params": {
        "stopReason": "end_turn",
        "usage": {"inputTokens": 100, "outputTokens": 3, "totalTokens": 103}}});
    assert_eq!(usage_of(done), Some((100, 3, 0)));
    // The running window figure is not a billable count.
    let window = json!({"method": "session/update", "params": {"update": {
        "sessionUpdate": "usage_update", "used": 103, "size": 1048576}}});
    assert_eq!(usage_of(window), None);
    // A cancelled turn reports no usage.
    let cancelled = json!({"method": "coop/turnCompleted", "params": {
        "stopReason": "cancelled", "usage": null}});
    assert_eq!(usage_of(cancelled), None);
}

#[test]
fn cached_tokens_are_split_out_of_input() {
    let done = json!({"method": "coop/turnCompleted", "params": {"usage": {
        "inputTokens": 1000, "outputTokens": 10, "totalTokens": 1010, "cachedReadTokens": 400}}});
    assert_eq!(usage_of(done), Some((600, 10, 400)));
}

#[test]
fn thinking_tokens_count_as_output_whether_or_not_they_are_in_the_output_figure() {
    let excluded = json!({"method": "coop/turnCompleted", "params": {"usage": {
        "inputTokens": 100, "outputTokens": 10, "totalTokens": 150, "thoughtTokens": 40}}});
    assert_eq!(usage_of(excluded), Some((100, 50, 0)));
    let included = json!({"method": "coop/turnCompleted", "params": {"usage": {
        "inputTokens": 100, "outputTokens": 50, "totalTokens": 150, "thoughtTokens": 40}}});
    assert_eq!(usage_of(included), Some((100, 50, 0)));
}

#[test]
fn stream_json_result_stats_are_the_turn_usage() {
    // headless/stream-json-tool-yolo.jsonl
    let result = json!({"type": "result", "status": "success", "stats": {
        "total_tokens": 258, "input_tokens": 230, "output_tokens": 28, "cached": 0, "input": 230}});
    assert_eq!(usage_of(result), Some((230, 28, 0)));
    assert_eq!(usage_of(json!({"type": "result", "status": "success"})), None);
}

#[test]
fn the_screen_gets_a_readable_transcript() {
    let line = |v: serde_json::Value| render_for_screen(&v);
    assert_eq!(
        line(json!({"method": "coop/prompt", "params": {"text": "hi"}})).as_deref(),
        Some("> hi")
    );
    assert_eq!(
        line(json!({"method": "coop/agentMessage", "params": {"text": "pong"}})).as_deref(),
        Some("● pong")
    );
    assert_eq!(
        line(json!({"method": "session/update", "params": {"update": {
            "sessionUpdate": "tool_call", "title": "touch x"}}}))
        .as_deref(),
        Some("$ touch x")
    );
    assert_eq!(
        line(json!({"method": "coop/turnCompleted", "params": {"stopReason": "end_turn"}}))
            .as_deref(),
        Some("-- turn end_turn --")
    );
    assert_eq!(line(json!({"jsonrpc": "2.0", "id": 4, "result": {}})), None);
}
